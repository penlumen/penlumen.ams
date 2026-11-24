package web

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/penlumen/penlumen.ams/internal/auth"
	"github.com/penlumen/penlumen.ams/internal/db"
	"github.com/penlumen/penlumen.ams/internal/schoolsync"
	"github.com/penlumen/penlumen.ams/internal/sms"
	"github.com/penlumen/penlumen.ams/internal/store"
)

// These tests run the whole app over HTTP against a real database and a fake
// Penlumen School: a fresh SQLite file per test, or Postgres when
// AMS_TEST_DATABASE_URL names a database they may wipe:
//
//	AMS_TEST_DATABASE_URL=postgres://ams:ams@localhost:5433/penlumen_ams?sslmode=disable go test ./...

const secret = "a-test-secret-that-is-long-enough!"

var (
	teacher = auth.User{Sub: "teacher-1", Name: "Mrs Adebayo", Role: "staff", SchoolUUID: "school-1", BranchUUIDs: []string{"branch-1"}}
	other   = auth.User{Sub: "teacher-2", Name: "Mr Other", Role: "staff", SchoolUUID: "school-2", BranchUUIDs: []string{"branch-9"}}
	amina   = auth.User{Sub: "student-1", Name: "Amina Okafor", Role: "student", SchoolUUID: "school-1",
		BranchUUIDs: []string{"branch-1"}, RegNumber: "PLS/1042", Class: &auth.ClassRef{UUID: "class-2a", Name: "JSS 2A"}}
	tobi = auth.User{Sub: "student-2", Name: "Tobi Adeyemi", Role: "student", SchoolUUID: "school-1",
		BranchUUIDs: []string{"branch-1"}, RegNumber: "PLS/1031", Class: &auth.ClassRef{UUID: "class-2a", Name: "JSS 2A"}}
	outsider = auth.User{Sub: "student-3", Name: "Zainab", Role: "student", SchoolUUID: "school-1",
		BranchUUIDs: []string{"branch-1"}, RegNumber: "PLS/1200", Class: &auth.ClassRef{UUID: "class-3a", Name: "JSS 3A"}}
)

// fakeSchool is a stand-in Penlumen School that records the scores it gets.
type fakeSchool struct {
	mu        sync.Mutex
	scores    []sms.Scores
	withdrawn []string
	// down makes every request fail, as when Penlumen School is offline.
	down bool
	// gone are students Penlumen School no longer has.
	gone map[string]bool
}

var fakeStudents = []string{
	`{"uuid":"student-1","name":"Amina Okafor","reg_number":"PLS/1042","status":"active","branch_uuid":"branch-1","class":{"uuid":"class-2a","name":"JSS 2A"}}`,
	`{"uuid":"student-2","name":"Tobi Adeyemi","reg_number":"PLS/1031","status":"active","branch_uuid":"branch-1","class":{"uuid":"class-2a","name":"JSS 2A"}}`,
	`{"uuid":"student-9","name":"Uche Nwosu","reg_number":"PLS/1099","status":"active","branch_uuid":"branch-1","class":{"uuid":"class-2a","name":"JSS 2A"}}`,
	`{"uuid":"student-3","name":"Zainab","reg_number":"PLS/1200","status":"active","branch_uuid":"branch-1","class":{"uuid":"class-3a","name":"JSS 3A"}}`,
}

func (f *fakeSchool) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/v1/oidc/token", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"access_token":"tok","token_type":"Bearer","expires_in":3600}`)
	})
	mux.HandleFunc("GET /api/v1/integrations/v1/school", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"data":{"uuid":"school-1","name":"Greenfield","theme":{"preset":"forest","primary":"#166534"},
			"branches":[{"uuid":"branch-1","name":"Main","assessment_max":40,"examination_max":60}]}}`)
	})
	mux.HandleFunc("GET /api/v1/integrations/v1/students", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("page") != "1" {
			fmt.Fprint(w, `{"data":[],"total":0}`)
			return
		}
		f.mu.Lock()
		var list []string
		for _, st := range fakeStudents {
			id := st[len(`{"uuid":"`):strings.Index(st, `",`)]
			if !f.gone[id] {
				list = append(list, st)
			}
		}
		f.mu.Unlock()
		fmt.Fprintf(w, `{"data":[%s],"total":%d}`, strings.Join(list, ","), len(list))
	})
	mux.HandleFunc("GET /api/v1/integrations/v1/staff", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("page") != "1" {
			fmt.Fprint(w, `{"data":[],"total":0}`)
			return
		}
		fmt.Fprint(w, `{"data":[
			{"uuid":"teacher-1","name":"Mrs Adebayo","email":"adebayo@greenfield.test","role":"staff","position":"ACADEMIC","branch_uuids":["branch-1"]},
			{"uuid":"office-1","name":"Ms Bursar","email":"office@greenfield.test","role":"staff","position":"ADMINISTRATIVE","branch_uuids":["branch-1"]},
			{"uuid":"admin-1","name":"Mr Head","email":"head@greenfield.test","role":"admin","position":"ADMINISTRATIVE","branch_uuids":["branch-1"]}],"total":3}`)
	})
	mux.HandleFunc("GET /api/v1/integrations/v1/classes", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"data":[
			{"uuid":"class-2a","name":"JSS 2A","branch_uuid":"branch-1","subjects":["Basic Science","Mathematics"],
			 "subject_teachers":[{"uuid":"s-1","name":"Basic Science","teacher_uuid":"teacher-1"},{"uuid":"s-2","name":"Mathematics","teacher_uuid":"teacher-1"}]},
			{"uuid":"class-2b","name":"JSS 2B","branch_uuid":"branch-1","subjects":["Mathematics"],
			 "subject_teachers":[{"uuid":"s-3","name":"Mathematics","teacher_uuid":null}]}],"total":2}`)
	})
	mux.HandleFunc("GET /api/v1/integrations/v1/terms", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"data":[{"uuid":"term-1","branch_uuid":"branch-1","session":"2026/2027","term":"First Term","status":"ACTIVE"}]}`)
	})
	mux.HandleFunc("DELETE /api/v1/integrations/v1/scores/{source}", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		source := r.PathValue("source")
		if st := r.URL.Query().Get("student_uuid"); st != "" {
			source += "|" + st
		}
		f.withdrawn = append(f.withdrawn, source)
		f.mu.Unlock()
		fmt.Fprint(w, `{"data":{"removed":1,"skipped":0,"results":[]}}`)
	})
	mux.HandleFunc("POST /api/v1/integrations/v1/scores", func(w http.ResponseWriter, r *http.Request) {
		var s sms.Scores
		json.NewDecoder(r.Body).Decode(&s)
		f.mu.Lock()
		f.scores = append(f.scores, s)
		f.mu.Unlock()
		var results []string
		for _, sc := range s.Scores {
			results = append(results, fmt.Sprintf(`{"student_uuid":%q,"status":"saved"}`, sc.StudentUUID))
		}
		fmt.Fprintf(w, `{"data":{"saved":%d,"skipped":0,"results":[%s]}}`, len(s.Scores), strings.Join(results, ","))
	})
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		down := f.down
		f.mu.Unlock()
		if down {
			http.Error(w, "down", http.StatusServiceUnavailable)
			return
		}
		mux.ServeHTTP(w, r)
	})
}

type harness struct {
	t        *testing.T
	srv      *httptest.Server
	sessions *auth.Sessions
	now      time.Time
	school   *fakeSchool
	store    *store.Store
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	dbURL := os.Getenv("AMS_TEST_DATABASE_URL")
	if dbURL == "" {
		dbURL = "sqlite:" + filepath.Join(t.TempDir(), "penlumen_ams.db")
	}
	ctx := context.Background()
	pool, err := db.Open(ctx, dbURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { pool.Close() })
	if err := pool.Migrate(); err != nil {
		t.Fatal(err)
	}
	if pool.Dialect == db.Postgres {
		if _, err := pool.ExecContext(ctx, `TRUNCATE assessments CASCADE`); err != nil {
			t.Fatal(err)
		}
	}

	// UTC, like the app's Location below, so form times mean the same thing.
	h := &harness{t: t, school: &fakeSchool{}, now: time.Now().UTC().Truncate(time.Minute), store: store.New(pool)}
	school := httptest.NewServer(h.school.handler())
	t.Cleanup(school.Close)

	h.sessions = auth.NewSessions(secret, false)
	issuer := school.URL + "/api/v1/oidc"
	// AMS's copy of the school, as the sync keeps it, plus a teacher from
	// another school (to check they can't see this one's assessments).
	client := sms.New(issuer, "pl_ems_test", "x")
	syncer := schoolsync.New(client, store.New(pool))
	if err := syncer.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	if err := h.store.SyncPerson(ctx, store.Person{UUID: other.Sub, Role: "staff", Name: other.Name, BranchUUIDs: other.BranchUUIDs}); err != nil {
		t.Fatal(err)
	}
	app := New(Options{
		DB:       pool,
		Sync:     syncer,
		Auth:     auth.New(issuer, "pl_ems_test", "x", "http://app/auth/callback", h.sessions),
		School:   client,
		BaseURL:  "http://app.test",
		Location: time.UTC,
		Now:      func() time.Time { return h.now },
	})
	h.srv = httptest.NewServer(app.Routes())
	t.Cleanup(h.srv.Close)
	return h
}

// do sends a request as user (nil: signed out) and returns the status, body
// and redirect location. Redirects aren't followed.
func (h *harness) do(user *auth.User, method, path string, form url.Values, header ...string) (int, string, string) {
	h.t.Helper()
	var body io.Reader
	if form != nil {
		body = strings.NewReader(form.Encode())
	}
	req, _ := http.NewRequest(method, h.srv.URL+path, body)
	if form != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	for i := 0; i+1 < len(header); i += 2 {
		req.Header.Set(header[i], header[i+1])
	}
	if user != nil {
		rec := httptest.NewRecorder()
		if err := h.sessions.Set(rec, auth.SessionCookie, user, time.Hour); err != nil {
			h.t.Fatal(err)
		}
		for _, c := range rec.Result().Cookies() {
			req.AddCookie(c)
		}
	}
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	res, err := client.Do(req)
	if err != nil {
		h.t.Fatal(err)
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	return res.StatusCode, string(b), res.Header.Get("Location")
}

func (h *harness) json(user *auth.User, path, body string) int {
	h.t.Helper()
	req, _ := http.NewRequest(http.MethodPost, h.srv.URL+path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	rec := httptest.NewRecorder()
	h.sessions.Set(rec, auth.SessionCookie, user, time.Hour)
	for _, c := range rec.Result().Cookies() {
		req.AddCookie(c)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		h.t.Fatal(err)
	}
	res.Body.Close()
	return res.StatusCode
}

func mustContain(t *testing.T, what, body string, wants ...string) {
	t.Helper()
	for _, want := range wants {
		if !strings.Contains(body, want) {
			t.Fatalf("%s: missing %q in:\n%s", what, want, body)
		}
	}
}

func expect(t *testing.T, what string, got, want int) {
	t.Helper()
	if got != want {
		t.Fatalf("%s: status %d, want %d", what, got, want)
	}
}

func TestTeacherBuildsStudentsSitScoresReachTheReport(t *testing.T) {
	h := newHarness(t)
	t0 := h.now
	local := func(d time.Duration) string { return t0.Add(d).Format(formTime) }

	// --- The teacher builds a class test. ---
	code, body, _ := h.do(&teacher, "GET", "/assessments/new", nil)
	expect(t, "new form", code, 200)
	mustContain(t, "new form", body, "JSS 2A", "Basic Science", "First Term",
		// The school's theme colour from Penlumen School.
		"--primary:#166534")

	form := url.Values{
		"title": {"CA 1 test"}, "kind": {"ca"}, "subject": {"Basic Science"},
		"class": {"class-2a"}, "term": {"term-1"}, "report_marks": {"10"}, "duration": {"30"},
		"opens_at": {local(time.Hour)}, "closes_at": {local(3 * time.Hour)},
	}
	bad := url.Values{}
	for k, v := range form {
		bad[k] = v
	}
	bad.Set("title", "")
	bad.Set("report_marks", "50") // more than the branch's 40 CA marks
	bad.Set("subject", "French")  // not a subject she teaches
	code, body, _ = h.do(&teacher, "POST", "/assessments", bad)
	expect(t, "invalid assessment", code, 422)
	mustContain(t, "invalid assessment", body, "Give it a name", "from 1 to 40", "You don&#39;t teach French in JSS 2A")

	code, _, loc := h.do(&teacher, "POST", "/assessments", form)
	expect(t, "create", code, 303)
	id := strings.TrimPrefix(loc, "/assessments/")

	add := func(body, marks string, answer int, options ...string) int {
		f := url.Values{"body": {body}, "marks": {marks}, "answer": {fmt.Sprint(answer)}, "option": options}
		code, _, _ := h.do(&teacher, "POST", "/assessments/"+id+"/questions", f)
		return code
	}
	expect(t, "question with one option", add("Bad?", "1", 0, "Only", "", "", ""), 422)
	expect(t, "question 1", add("Which part of the cell controls it?", "2", 0, "Nucleus", "Cytoplasm", "Vacuole", ""), 303)
	expect(t, "question 2", add("SI unit of force?", "3", 2, "Joule", "", "Newton", "Watt"), 303)

	code, body, _ = h.do(&teacher, "GET", "/assessments/"+id, nil)
	expect(t, "builder", code, 200)
	mustContain(t, "builder", body, "2 objective, 0 theory · 5 marks", "Which part of the cell",
		// The empty option B was dropped, so Newton moved up to B.
		`B</span><span>Newton<span class="sr-only"> (right answer)`,
		// Switch states are plain values (an attribute named data-on... would be
		// treated as JavaScript by html/template and come out as " false ").
		`data-enabled="false"`)

	code, _, _ = h.do(&other, "GET", "/assessments/"+id, nil)
	expect(t, "another school's teacher", code, 404)

	code, _, _ = h.do(&teacher, "POST", "/assessments/"+id+"/status", url.Values{"action": {"schedule"}})
	expect(t, "schedule", code, 303)
	expect(t, "questions locked once scheduled", add("Late?", "1", 0, "A", "B", "", ""), 409)

	// --- Before it opens, students see it coming up. ---
	code, body, _ = h.do(&amina, "GET", "/", nil)
	expect(t, "student home", code, 200)
	mustContain(t, "student home", body, "Coming up", "CA 1 test")
	code, _, _ = h.do(&outsider, "GET", "/take/"+id, nil)
	expect(t, "student of another class", code, 404)
	code, _, _ = h.do(&amina, "POST", "/take/"+id+"/start", url.Values{})
	expect(t, "start before it opens", code, 303)
	code, body, _ = h.do(&amina, "GET", "/take/"+id, nil)
	mustContain(t, "not started yet", body, "It opens")

	// --- It opens; Amina sits it. ---
	h.now = t0.Add(90 * time.Minute)
	code, body, _ = h.do(&amina, "GET", "/", nil)
	mustContain(t, "student home while open", body, "Open now", "Open until", "Start")
	_, body, _ = h.do(&amina, "GET", "/take/"+id, nil)
	mustContain(t, "start page", body, "Start exam", "I've read the rules")
	_, _, loc = h.do(&amina, "POST", "/take/"+id+"/start", url.Values{})
	if !strings.HasSuffix(loc, "?rules=1") {
		t.Fatalf("start without ticking the rules went to %q", loc)
	}
	code, _, _ = h.do(&amina, "POST", "/take/"+id+"/start", url.Values{"agree": {"on"}})
	expect(t, "start", code, 303)
	code, body, _ = h.do(&amina, "GET", "/take/"+id, nil)
	expect(t, "exam", code, 200)
	mustContain(t, "exam", body, "Which part of the cell", "SI unit of force", `data-seconds-left="1800"`)
	if strings.Contains(body, `data-right`) {
		t.Fatal("the exam page shows the right answers")
	}
	qids := questionIDs(body)
	if len(qids) != 2 {
		t.Fatalf("found question ids %v", qids)
	}
	attempt := attemptID(body)

	// Autosave: right answer to "cell" (option 0), via JSON.
	cellQ, forceQ := qids["Which part of the cell"], qids["SI unit of force"]
	expect(t, "autosave", h.json(&amina, "/attempts/"+attempt+"/answers", fmt.Sprintf(`{"question_id":%q,"answer":0}`, cellQ)), 200)
	expect(t, "someone else's attempt", h.json(&tobi, "/attempts/"+attempt+"/answers", fmt.Sprintf(`{"question_id":%q,"answer":1}`, cellQ)), 404)

	// Submit with a wrong answer to "force" in the form.
	code, _, _ = h.do(&amina, "POST", "/attempts/"+attempt+"/submit", url.Values{"q_" + forceQ: {"0"}})
	expect(t, "submit", code, 303)
	code, body, _ = h.do(&amina, "GET", "/take/"+id, nil)
	mustContain(t, "after submitting", body, "Exam submitted", "once your teacher has marked it and sent it")
	expect(t, "answer after submitting", h.json(&amina, "/attempts/"+attempt+"/answers", fmt.Sprintf(`{"question_id":%q,"answer":2}`, forceQ)), 409)

	// --- Tobi starts but doesn't finish in time. ---
	h.do(&tobi, "POST", "/take/"+id+"/start", url.Values{"agree": {"on"}})
	h.now = t0.Add(130 * time.Minute) // past his 30 minutes, but still open

	code, body, _ = h.do(&teacher, "GET", "/assessments/"+id+"/submissions", nil)
	expect(t, "results while open", code, 200)
	mustContain(t, "results while open", body, "Amina Okafor", "2 / 5", "4 / 10", "Tobi Adeyemi", "Time ran out", "0 / 5")
	code, _, loc = h.do(&teacher, "POST", "/assessments/"+id+"/publish", url.Values{})
	if !strings.Contains(loc, "End+the+exam") {
		t.Fatalf("publish while open went to %q", loc)
	}

	// --- The teacher closes it and sends the scores. ---
	code, _, _ = h.do(&teacher, "POST", "/assessments/"+id+"/status", url.Values{"action": {"close"}})
	expect(t, "close", code, 303)
	code, _, _ = h.do(&teacher, "POST", "/assessments/"+id+"/publish", url.Values{})
	expect(t, "publish", code, 303)

	if len(h.school.scores) != 1 {
		t.Fatalf("Penlumen School got %d batches", len(h.school.scores))
	}
	got := h.school.scores[0]
	if got.SourceID != "ams:"+id || got.Subject != "Basic Science" || got.Component != "assessment" || got.CalendarUUID != "term-1" {
		t.Errorf("batch = %+v", got)
	}
	byStudent := map[string]float64{}
	for _, s := range got.Scores {
		byStudent[s.StudentUUID] = s.Score
	}
	if byStudent["student-1"] != 4 || byStudent["student-2"] != 0 || len(byStudent) != 2 {
		t.Errorf("scores = %v, want Amina 4 (2/5 of 10) and Tobi 0", byStudent)
	}

	code, body, _ = h.do(&teacher, "GET", "/assessments/"+id+"/submissions", nil)
	mustContain(t, "results after publishing", body, "2 saved on reports", "Saved", "Send scores again")

	code, body, _ = h.do(&amina, "GET", "/", nil)
	mustContain(t, "student home after", body, "Completed", "CA 1 test", "Sent to your result")
}

func TestCrossSiteFormsAreRefused(t *testing.T) {
	h := newHarness(t)
	code, _, _ := h.do(&teacher, "POST", "/assessments", url.Values{"title": {"x"}}, "Origin", "https://evil.test")
	expect(t, "cross-site post", code, 403)
}

func TestSignedOutAndWrongRole(t *testing.T) {
	h := newHarness(t)
	code, _, loc := h.do(nil, "GET", "/assessments/new", nil)
	if code != 302 || !strings.HasPrefix(loc, "/login?return_to=") {
		t.Fatalf("signed out: %d %q", code, loc)
	}
	code, _, _ = h.do(&amina, "GET", "/assessments/new", nil)
	expect(t, "student on a teacher page", code, 403)
	code, _, _ = h.do(&teacher, "POST", "/take/x/start", url.Values{})
	expect(t, "teacher starting a test", code, 403)
}

// questionIDs maps the start of each question's text to its id, read from
// the exam page.
func questionIDs(page string) map[string]string {
	out := map[string]string{}
	for _, part := range strings.Split(page, `data-question="`)[1:] {
		id := part[:strings.Index(part, `"`)]
		text := part[strings.Index(part, `data-q-text>`)+len(`data-q-text>`):]
		text = text[:strings.Index(text, "<")]
		for _, key := range []string{"Which part of the cell", "SI unit of force"} {
			if strings.HasPrefix(text, key) {
				out[key] = id
			}
		}
	}
	return out
}

func attemptID(page string) string {
	part := page[strings.Index(page, `data-attempt="`)+len(`data-attempt="`):]
	return part[:strings.Index(part, `"`)]
}

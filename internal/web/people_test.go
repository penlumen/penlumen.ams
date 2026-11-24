package web

import (
	"context"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/penlumen/penlumen.ams/internal/auth"
	"github.com/penlumen/penlumen.ams/internal/db"
	"github.com/penlumen/penlumen.ams/internal/store"
)

// browser signs in like a person does, keeping its cookies.
type browser struct {
	t      *testing.T
	base   string
	client *http.Client
}

func newBrowser(t *testing.T, base string) *browser {
	jar, _ := cookiejar.New(nil)
	return &browser{t: t, base: base, client: &http.Client{Jar: jar,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
}

func (b *browser) do(method, path string, form url.Values) (int, string, string) {
	b.t.Helper()
	var body io.Reader
	if form != nil {
		body = strings.NewReader(form.Encode())
	}
	req, _ := http.NewRequest(method, b.base+path, body)
	if form != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	res, err := b.client.Do(req)
	if err != nil {
		b.t.Fatal(err)
	}
	defer res.Body.Close()
	out, _ := io.ReadAll(res.Body)
	return res.StatusCode, string(out), res.Header.Get("Location")
}

var shownSecret = regexp.MustCompile(`<code[^>]*>([^<]+)</code>`)

// A school with no Penlumen School: set up, add classes, terms, staff and
// students, sign in with passwords and PINs, sit an exam.
func TestStandaloneSchool(t *testing.T) {
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
		for _, q := range []string{`TRUNCATE assessments CASCADE`, `TRUNCATE users`, `TRUNCATE students`, `TRUNCATE classes`, `TRUNCATE terms`, `TRUNCATE settings`} {
			if _, err := pool.ExecContext(ctx, q); err != nil {
				t.Fatal(err)
			}
		}
	}
	sessions := auth.NewSessions(secret, false)
	srv := httptest.NewServer(New(Options{DB: pool, Auth: auth.New("", "", "", "", sessions),
		BaseURL: "http://app.test", Location: time.UTC}).Routes())
	t.Cleanup(srv.Close)

	// A new install asks for its first administrator.
	head := newBrowser(t, srv.URL)
	_, _, loc := head.do("GET", "/", nil)
	if loc != "/setup" {
		t.Fatalf("a new install went to %q, not /setup", loc)
	}
	code, page, _ := head.do("POST", "/setup", url.Values{"school": {"Hilltop"}, "name": {"Mrs Head"},
		"email": {"head@hilltop.test"}, "password": {"short"}, "confirm": {"short"}})
	expect(t, "short password", code, 422)
	mustContain(t, "short password", page, "Use at least 8 characters.")
	code, _, loc = head.do("POST", "/setup", url.Values{"school": {"Hilltop"}, "name": {"Mrs Head"},
		"email": {"Head@Hilltop.test"}, "password": {"a long password"}, "confirm": {"a long password"}})
	if code != 303 || !strings.HasPrefix(loc, "/people") {
		t.Fatalf("setup: %d to %q", code, loc)
	}
	_, _, loc = newBrowser(t, srv.URL).do("GET", "/setup", nil)
	expect(t, "setup only once", map[bool]int{true: 1}[loc == "/login"], 1)

	// Classes and terms, then a student and a teacher.
	head.do("POST", "/classes", url.Values{"name": {"Primary 5"}, "subjects": {"Mathematics\nEnglish, Science"}})
	head.do("POST", "/terms", url.Values{"session": {"2026/2027"}, "term": {"First Term"}, "current": {"on"}})
	_, page, _ = head.do("GET", "/people?tab=classes", nil)
	mustContain(t, "classes", page, "Primary 5", "Mathematics · ", "English · ", "Science · ", "no teacher yet", "Remove Primary 5")
	classID := regexp.MustCompile(`/people\?tab=students&amp;class=([0-9a-f-]{36})`).FindStringSubmatch(page)[1]

	code, page, _ = head.do("POST", "/people", url.Values{"role": {"student"}, "name": {"Bola Ade"}, "reg_number": {"hs/001"}, "class": {classID}})
	expect(t, "add student", code, 201)
	pin := shownSecret.FindStringSubmatch(page)[1]
	if !regexp.MustCompile(`^[0-9]{6}$`).MatchString(pin) {
		t.Fatalf("new PIN %q", pin)
	}
	code, page, _ = head.do("POST", "/people", url.Values{"role": {"student"}, "name": {"Copy"}, "reg_number": {"HS/001"}, "class": {classID}})
	expect(t, "same reg number", code, 422)
	mustContain(t, "same reg number", page, "Another student has this reg number.")
	code, page, _ = head.do("POST", "/people", url.Values{"role": {"staff"}, "name": {"Mr Teach"}, "email": {"teach@hilltop.test"}, "password": {"teach-pass-1"}})
	expect(t, "add teacher", code, 201)
	mustContain(t, "add teacher", page, "teach-pass-1")

	// The student signs in with reg number (any case) and PIN.
	bola := newBrowser(t, srv.URL)
	code, page, _ = bola.do("POST", "/login", url.Values{"as": {"student"}, "name": {"HS/001"}, "password": {"000000x"}})
	expect(t, "wrong PIN", code, 401)
	mustContain(t, "wrong PIN", page, "That reg number and PIN don")
	code, _, loc = bola.do("POST", "/login", url.Values{"as": {"student"}, "name": {"hs/001"}, "password": {pin}, "return_to": {"/"}})
	if code != 303 || loc != "/" {
		t.Fatalf("student sign-in: %d to %q", code, loc)
	}
	_, page, _ = bola.do("GET", "/", nil)
	mustContain(t, "student home", page, "Bola Ade", "Primary 5", "HS/001", "Hilltop")
	// Staff sign-in doesn't take a student's details, and vice versa.
	code, _, _ = newBrowser(t, srv.URL).do("POST", "/login", url.Values{"as": {"staff"}, "name": {"hs/001"}, "password": {pin}})
	expect(t, "student as staff", code, 401)

	// The teacher builds an assessment from AMS's own class and term.
	teach := newBrowser(t, srv.URL)
	code, _, _ = teach.do("POST", "/login", url.Values{"as": {"staff"}, "name": {"TEACH@hilltop.test"}, "password": {"teach-pass-1"}, "return_to": {"/"}})
	expect(t, "teacher sign-in", code, 303)
	// A teacher sets assessments only for subjects assigned to them.
	_, page, _ = teach.do("GET", "/assessments/new", nil)
	mustContain(t, "before teaching anything", page, "No subjects are assigned to you yet.")
	_, staffPage, _ := head.do("GET", "/people?tab=staff", nil)
	teachID := regexp.MustCompile(`href="/people/([0-9a-f-]{36})">Mr Teach`).FindStringSubmatch(staffPage)[1]
	code, _, _ = head.do("POST", "/classes/"+classID+"/teachers", url.Values{"teacher:Science": {teachID}, "teacher:English": {"not-staff"}})
	expect(t, "assign teachers", code, 303)
	_, page, _ = head.do("GET", "/people?tab=classes", nil)
	mustContain(t, "assigned", page, `Science · <span class="text-fg-2">Mr Teach</span>`, "English · <span class=\"text-warn\">no teacher yet")
	_, page, _ = teach.do("GET", "/assessments/new", nil)
	mustContain(t, "new assessment", page, "Primary 5", "2026/2027 First Term")
	code, _, _ = teach.do("GET", "/people", nil)
	expect(t, "teachers don't manage people", code, 403)
	_, terms, _ := head.do("GET", "/people?tab=terms", nil)
	termID := regexp.MustCompile(`/terms/([0-9a-f-]{36})/delete`).FindStringSubmatch(terms)[1]
	now := time.Now().UTC()
	code, _, loc = teach.do("POST", "/assessments", url.Values{"title": {"Weekly test"}, "kind": {"ca"}, "class": {classID},
		"subject": {"Science"}, "term": {termID},
		"report_marks": {"10"}, "duration": {"20"},
		"opens_at": {now.Add(-time.Hour).Format(formTime)}, "closes_at": {now.Add(time.Hour).Format(formTime)}})
	if code != 303 {
		t.Fatalf("create assessment: %d", code)
	}
	id := strings.TrimPrefix(loc, "/assessments/")
	teach.do("POST", "/assessments/"+id+"/questions", url.Values{"kind": {"objective"}, "body": {"3 x 4?"}, "option": {"12", "7", "", ""}, "answer": {"0"}, "marks": {"1"}})
	teach.do("POST", "/assessments/"+id+"/status", url.Values{"action": {"schedule"}})
	_, page, _ = bola.do("GET", "/", nil)
	mustContain(t, "exam listed", page, "Science · Weekly test")
	code, _, _ = bola.do("POST", "/take/"+id+"/start", url.Values{"agree": {"on"}})
	expect(t, "start", code, 303)
	// No report cards to send to: the scores export instead.
	_, page, _ = teach.do("GET", "/assessments/"+id+"/submissions", nil)
	mustContain(t, "submissions", page, "Bola Ade", "Export CSV")
	if strings.Contains(page, "Send scores to report cards") {
		t.Error("sending to report cards is offered without Penlumen School")
	}

	// A new PIN; the old one stops working. Removing her ends her session.
	_, page, _ = head.do("GET", "/people?tab=students", nil)
	bolaID := regexp.MustCompile(`href="/people/([0-9a-f-]{36})">Bola Ade`).FindStringSubmatch(page)[1]
	_, page, _ = head.do("POST", "/people/"+bolaID+"/password", url.Values{"password": {"4321"}})
	mustContain(t, "new PIN", page, ">4321<")
	code, _, _ = newBrowser(t, srv.URL).do("POST", "/login", url.Values{"as": {"student"}, "name": {"HS/001"}, "password": {pin}})
	expect(t, "old PIN", code, 401)
	head.do("POST", "/people/"+bolaID+"/delete", nil)
	_, _, loc = bola.do("GET", "/", nil)
	if code, _, _ := bola.do("GET", "/take/"+id, nil); code != 302 {
		t.Errorf("a removed student still opens the exam: %d", code)
	}
	_, page, _ = teach.do("GET", "/assessments/"+id+"/submissions", nil)
	mustContain(t, "kept after removal", page, "Bola Ade")

	// Guessing is stopped after 5 wrong tries.
	guess := newBrowser(t, srv.URL)
	for i := 0; i < 5; i++ {
		guess.do("POST", "/login", url.Values{"as": {"staff"}, "name": {"head@hilltop.test"}, "password": {"guess"}})
	}
	code, _, _ = guess.do("POST", "/login", url.Values{"as": {"staff"}, "name": {"head@hilltop.test"}, "password": {"a long password"}})
	expect(t, "too many tries", code, 429)
}

// With Penlumen School: everyone synced can sign in to AMS with a PIN or
// password, also while Penlumen School is down; people it removes can't.
func TestPenlumenSchoolPeopleAndOutages(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	head := auth.User{Sub: "admin-1", Name: "Mr Head", Role: "admin", SchoolUUID: "school-1", BranchUUIDs: []string{"branch-1"}}

	// Synced people show as from Penlumen School; their details can't be
	// changed here, but their AMS PIN can be set.
	_, page, _ := h.do(&head, "GET", "/people?tab=students", nil)
	mustContain(t, "students", page, "Amina Okafor", "PLS/1042", "Penlumen School", "No PIN yet")
	code, _, _ := h.do(&head, "POST", "/people/"+amina.Sub, url.Values{"name": {"Changed"}, "reg_number": {"X"}, "class": {"class-2a"}})
	expect(t, "edit a synced student", code, 409)
	_, page, _ = h.do(&head, "POST", "/people/"+amina.Sub+"/password", url.Values{"password": {"2468"}})
	mustContain(t, "PIN set", page, ">2468<")
	code, _, _ = h.do(&teacher, "GET", "/people", nil)
	expect(t, "teachers don't manage people", code, 403)

	// Penlumen School goes down; Amina still signs in, with her PIN.
	h.school.down = true
	b := newBrowser(t, h.srv.URL)
	code, _, _ = b.do("POST", "/login", url.Values{"as": {"student"}, "name": {"pls/1042"}, "password": {"2468"}, "return_to": {"/"}})
	expect(t, "sign in while it's down", code, 303)
	_, page, _ = b.do("GET", "/", nil)
	mustContain(t, "home while it's down", page, "Amina Okafor", "JSS 2A")
	_, page, _ = h.do(&head, "POST", "/sync", nil)
	_, page, _ = h.do(&head, "GET", "/people", nil)
	mustContain(t, "sync failed", page, "couldn")

	// Added in AMS while it was down, then in Penlumen School too: one person.
	_, page, _ = h.do(&head, "POST", "/people", url.Values{"role": {"student"}, "name": {"Ngozi Eze"}, "reg_number": {"PLS/1300"}, "class": {"class-2a"}, "password": {"1357"}})
	localID := regexp.MustCompile(`action="/people/([0-9a-f-]{36})/password"`).FindStringSubmatch(page)[1]
	h.store.SyncPerson(ctx, store.Person{UUID: "student-10", Role: "student", Name: "Ngozi Eze", RegNumber: "PLS/1300", ClassUUID: "class-2a"})
	if _, err := h.store.Person(ctx, localID); err == nil {
		t.Error("the AMS copy of Ngozi is still there")
	}
	ngozi, _, err := h.store.PersonFor(ctx, true, "PLS/1300")
	if err != nil || ngozi.UUID != "student-10" || ngozi.Source != store.SourceSMS || !ngozi.HasPassword {
		t.Errorf("merged: %+v %v", ngozi, err)
	}

	// Back up, Penlumen School no longer has Tobi: he can't sign in or carry on.
	h.school.down = false
	h.school.gone = map[string]bool{"student-2": true}
	_, page, _ = h.do(&head, "POST", "/sync", nil)
	_, page, _ = h.do(&head, "GET", "/people?tab=students", nil)
	mustContain(t, "removed there", page, "Removed in Penlumen School")
	h.store.SetPassword(ctx, tobi.Sub, "x")
	code, _, _ = h.do(&tobi, "GET", "/", nil)
	if code != 200 {
		t.Fatalf("home: %d", code)
	}
	_, page, _ = h.do(&tobi, "GET", "/", nil)
	if strings.Contains(page, "Tobi Adeyemi") {
		t.Error("Tobi is still signed in after Penlumen School removed him")
	}
}

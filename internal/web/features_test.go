package web

import (
	"bytes"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/penlumen/penlumen.ams/internal/auth"
)

// upload posts a CSV as the "file" field, plus any other fields.
func (h *harness) upload(user *auth.User, path, csv string, fields url.Values) (int, string, string) {
	h.t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, _ := mw.CreateFormFile("file", "questions.csv")
	io.WriteString(fw, csv)
	for k, vs := range fields {
		for _, v := range vs {
			mw.WriteField(k, v)
		}
	}
	mw.Close()
	req, _ := http.NewRequest(http.MethodPost, h.srv.URL+path, &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	rec := newCookie(h, user)
	req.AddCookie(rec)
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	res, err := client.Do(req)
	if err != nil {
		h.t.Fatal(err)
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	return res.StatusCode, string(b), res.Header.Get("Location")
}

func newCookie(h *harness, user *auth.User) *http.Cookie {
	w := &cookieRecorder{header: http.Header{}}
	if err := h.sessions.Set(w, auth.SessionCookie, user, time.Hour); err != nil {
		h.t.Fatal(err)
	}
	return (&http.Response{Header: w.header}).Cookies()[0]
}

type cookieRecorder struct{ header http.Header }

func (c *cookieRecorder) Header() http.Header         { return c.header }
func (c *cookieRecorder) Write(b []byte) (int, error) { return len(b), nil }
func (c *cookieRecorder) WriteHeader(int)             {}

var questionID = regexp.MustCompile(`data-question="([0-9a-f-]+)" data-kind="(\w+)"`)

func TestTheoryCSVMonitorMarkingReports(t *testing.T) {
	h := newHarness(t)
	t0 := h.now
	local := func(d time.Duration) string { return t0.Add(d).Format(formTime) }

	// --- An exam with objective and theory questions, from a CSV. ---
	_, _, loc := h.do(&teacher, "POST", "/assessments", url.Values{
		"title": {"First term examination"}, "kind": {"exam"}, "subject": {"Basic Science"},
		"class": {"class-2a"}, "term": {"term-1"}, "report_marks": {"60"}, "duration": {"40"},
		"opens_at": {local(time.Hour)}, "closes_at": {local(3 * time.Hour)},
	})
	id := strings.TrimPrefix(loc, "/assessments/")
	if id == "" || strings.Contains(id, "/") {
		t.Fatalf("create went to %q", loc)
	}

	csv := "type,question,option_a,option_b,option_c,option_d,option_e,option_f,answer,marks,marking_guide\n" +
		"objective,Which organ pumps blood?,Lungs,Heart,Liver,,,,B,2,\n" +
		"theory,Explain photosynthesis.,,,,,,,,8,Light; chlorophyll; glucose\n" +
		"theory,Name two parts of a flower.,,,,,,,,4,\"Petal, sepal, stamen\"\n" +
		"objective,Broken row,Only one,,,,,,A,1,\n"
	code, body, _ := h.upload(&teacher, "/assessments/"+id+"/import", csv, nil)
	expect(t, "import", code, 200)
	mustContain(t, "import", body, "Imported 3 questions", "Row 5: an objective question needs at least two options",
		"Which organ pumps blood?", "Explain photosynthesis.", "Light; chlorophyll; glucose", "1 objective, 2 theory · 14 marks")
	_, body, _ = h.do(&teacher, "GET", "/assessments/"+id+"/questions.csv", nil)
	mustContain(t, "exam export", body, "objective,Which organ pumps blood?,Lungs,Heart,Liver,,,,B,2,",
		"theory,Name two parts of a flower.,,,,,,,,4,\"Petal, sepal, stamen\"")
	// The question bank is gone.
	code, _, _ = h.do(&teacher, "GET", "/bank", nil)
	expect(t, "no bank", code, 404)

	// Details can change before anyone starts.
	code, _, _ = h.do(&teacher, "POST", "/assessments/"+id+"/details", url.Values{
		"title": {"First term exam"}, "kind": {"exam"}, "subject": {"Basic Science"},
		"class": {"class-2a"}, "term": {"term-1"}, "report_marks": {"60"}, "duration": {"40"},
		"opens_at": {local(time.Hour)}, "closes_at": {local(3 * time.Hour)}, "shuffle_questions": {"on"},
	})
	expect(t, "edit details", code, 303)
	_, body, _ = h.do(&teacher, "GET", "/assessments/"+id, nil)
	mustContain(t, "after edit", body, "Basic Science · First term exam", `data-enabled="true"`)

	h.do(&teacher, "POST", "/assessments/"+id+"/status", url.Values{"action": {"schedule"}})

	// --- Amina sits it: an objective answer and an essay. ---
	h.now = t0.Add(90 * time.Minute)
	h.do(&amina, "POST", "/take/"+id+"/start", url.Values{"agree": {"on"}})
	_, body, _ = h.do(&amina, "GET", "/take/"+id, nil)
	mustContain(t, "exam", body, "Section B · Theory", "saves as you type", "data-editor", `data-upload="/attempts/`)
	if strings.Contains(body, "Light; chlorophyll") {
		t.Fatal("the exam shows the marking guide")
	}
	var heartQ, essayQ, flowerQ string
	for _, m := range questionID.FindAllStringSubmatch(body, -1) {
		chunk := body[strings.Index(body, m[0]):]
		switch {
		case m[2] == "objective":
			heartQ = m[1]
		case strings.Index(chunk, "Explain photosynthesis") < 2000 && strings.Contains(chunk[:min(len(chunk), 2000)], "Explain photosynthesis"):
			essayQ = m[1]
		default:
			flowerQ = m[1]
		}
	}
	attempt := attemptID(body)
	expect(t, "objective answer", h.json(&amina, "/attempts/"+attempt+"/answers", fmt.Sprintf(`{"question_id":%q,"answer":1}`, heartQ)), 200)
	expect(t, "essay answer", h.json(&amina, "/attempts/"+attempt+"/answers",
		fmt.Sprintf(`{"question_id":%q,"text":"Plants use light and chlorophyll to make glucose."}`, essayQ)), 200)
	expect(t, "left the page", h.json(&amina, "/attempts/"+attempt+"/ping?event=left_page", ``), 204)
	expect(t, "came back", h.json(&amina, "/attempts/"+attempt+"/ping?event=returned", ``), 204)
	// A reload sends "left the page" as it unloads, then takes it back;
	// the real leave she came back from stays.
	expect(t, "reload unloads", h.json(&amina, "/attempts/"+attempt+"/ping?event=left_page", ``), 204)
	expect(t, "reloaded", h.json(&amina, "/attempts/"+attempt+"/ping?event=reloaded", ``), 204)
	expect(t, "reloaded twice", h.json(&amina, "/attempts/"+attempt+"/ping?event=reloaded", ``), 204)
	_ = flowerQ // left blank: scores 0 without marking

	// The live monitor sees her writing, the page leave, and Uche not started.
	code, body, _ = h.do(&teacher, "GET", "/monitor/"+id+"/live", nil)
	expect(t, "monitor", code, 200)
	mustContain(t, "monitor", body, "Amina Okafor", "Writing", "Left the page 1×", "2/3", "Uche Nwosu", "Not started")
	_, _, loc = h.do(&teacher, "POST", "/monitor/"+id+"/time", url.Values{"attempt": {attempt}, "minutes": {"10"}})
	if !strings.Contains(loc, "Gave+1+student+10+more+minutes") {
		t.Fatalf("add time went to %q", loc)
	}
	_, body, _ = h.do(&teacher, "GET", "/monitor/"+id+"/live", nil)
	mustContain(t, "after extra time", body, "+10 min")

	h.do(&amina, "POST", "/attempts/"+attempt+"/submit", url.Values{})

	// --- Close; scores wait for the essay to be marked. ---
	h.do(&teacher, "POST", "/assessments/"+id+"/status", url.Values{"action": {"close"}})
	_, body, _ = h.do(&teacher, "GET", "/assessments/"+id+"/submissions", nil)
	mustContain(t, "results before marking", body, "1 written answer is still to mark", "2 / 14")
	_, _, loc = h.do(&teacher, "POST", "/assessments/"+id+"/publish", url.Values{})
	if !strings.Contains(loc, "still+to+mark") {
		t.Fatalf("publish before marking went to %q", loc)
	}

	_, body, _ = h.do(&teacher, "GET", "/marking", nil)
	mustContain(t, "marking list", body, "First term exam", "1 to mark")
	_, body, _ = h.do(&teacher, "GET", "/marking/"+id, nil)
	mustContain(t, "marking", body, "Script 001", "Plants use light and chlorophyll", "Light; chlorophyll; glucose", "Not answered (scored 0)")
	if strings.Contains(body, "Amina") {
		t.Fatal("marking shows the student's name")
	}
	_, _, loc = h.do(&teacher, "POST", "/marking/"+id+"/"+attempt, url.Values{"score_" + essayQ: {"9"}})
	if !strings.Contains(loc, "from+0+to+8") {
		t.Fatalf("a mark above the question's marks went to %q", loc)
	}
	h.do(&teacher, "POST", "/marking/"+id+"/"+attempt, url.Values{"score_" + essayQ: {"6"}, "comment_" + essayQ: {"Good."}})

	_, body, _ = h.do(&teacher, "GET", "/assessments/"+id+"/submissions", nil)
	mustContain(t, "results after marking", body, "8 / 14", "34.29 / 60")
	code, _, _ = h.do(&teacher, "POST", "/assessments/"+id+"/publish", url.Values{})
	expect(t, "publish", code, 303)
	last := h.school.scores[len(h.school.scores)-1]
	if last.Component != "examination" || len(last.Scores) != 1 || last.Scores[0].Score != 34.29 {
		t.Errorf("sent %+v", last)
	}

	// --- Reports and the results file. ---
	_, body, _ = h.do(&teacher, "GET", "/reports", nil)
	mustContain(t, "reports", body, "First term exam", "57%", "100%")
	_, body, _ = h.do(&teacher, "GET", "/reports/"+id, nil)
	mustContain(t, "report", body, "How scores spread", "Question by question", "100% right", "average 75%", "Worth going over in class")
	_, body, _ = h.do(&teacher, "GET", "/assessments/"+id+"/submissions.csv", nil)
	mustContain(t, "results csv", body, "Amina Okafor,PLS/1042,submitted,8,14,34.29,60,saved")

	// --- Reopen it until later. ---
	_, _, loc = h.do(&teacher, "POST", "/assessments/"+id+"/status", url.Values{"action": {"closes"}, "closes_at": {local(5 * time.Hour)}})
	if !strings.Contains(loc, "Closing+time+changed") {
		t.Fatalf("reopen went to %q", loc)
	}
	_, body, _ = h.do(&teacher, "GET", "/assessments/"+id, nil)
	mustContain(t, "after reopening", body, "Live")
	// Details are locked now that someone has sat it.
	_, _, loc = h.do(&teacher, "GET", "/assessments/"+id+"/details", nil)
	if !strings.Contains(loc, "can%27t+change") && !strings.Contains(loc, "can't+change") {
		t.Fatalf("editing details after a start went to %q", loc)
	}

	// --- Delete it for good: scores come off report cards first. ---
	_, _, loc = h.do(&teacher, "POST", "/assessments/"+id+"/delete", url.Values{})
	if !strings.Contains(loc, "Tick+the+box") {
		t.Fatalf("delete without confirming went to %q", loc)
	}
	_, _, loc = h.do(&teacher, "POST", "/assessments/"+id+"/delete", url.Values{"confirm": {"on"}})
	if !strings.HasPrefix(loc, "/?notice=Deleted") {
		t.Fatalf("delete went to %q", loc)
	}
	if len(h.school.withdrawn) != 1 || h.school.withdrawn[0] != "ams:"+id {
		t.Errorf("withdrawn from report cards: %v", h.school.withdrawn)
	}
	code, _, _ = h.do(&teacher, "GET", "/assessments/"+id, nil)
	expect(t, "deleted assessment", code, 404)
	code, _, _ = h.do(&amina, "GET", "/take/"+id, nil)
	expect(t, "deleted assessment for a student", code, 404)
}

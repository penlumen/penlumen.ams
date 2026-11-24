package web

import (
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/penlumen/penlumen.ams/internal/auth"
)

func TestResetAndDeleteAStudentsExam(t *testing.T) {
	h := newHarness(t)
	t0 := h.now
	local := func(d time.Duration) string { return t0.Add(d).Format(formTime) }

	_, _, loc := h.do(&teacher, "POST", "/assessments", url.Values{
		"title": {"Mid-term test"}, "kind": {"ca"}, "subject": {"Basic Science"},
		"class": {"class-2a"}, "term": {"term-1"}, "report_marks": {"10"}, "duration": {"20"},
		"opens_at": {local(time.Hour)}, "closes_at": {local(2 * time.Hour)},
	})
	id := strings.TrimPrefix(loc, "/assessments/")
	h.do(&teacher, "POST", "/assessments/"+id+"/questions", url.Values{
		"kind": {"objective"}, "body": {"2 + 2?"}, "option": {"3", "4", "", ""}, "answer": {"1"}, "marks": {"2"}})
	h.do(&teacher, "POST", "/assessments/"+id+"/status", url.Values{"action": {"schedule"}})

	// Amina and Tobi both sit it; Amina gets it right.
	h.now = t0.Add(70 * time.Minute)
	attemptOf := func(user *auth.User) (string, string) {
		_, body, _ := h.do(user, "GET", "/take/"+id, nil)
		return attemptID(body), questionID.FindStringSubmatch(body)[1]
	}
	h.do(&amina, "POST", "/take/"+id+"/start", url.Values{"agree": {"on"}})
	h.do(&tobi, "POST", "/take/"+id+"/start", url.Values{"agree": {"on"}})
	aminaAttempt, q := attemptOf(&amina)
	tobiAttempt, _ := attemptOf(&tobi)
	// A chosen option, as the exam page writes it; check it finds one.
	chosen := regexp.MustCompile(`type="radio" name="q_[^"]+" value="[0-9]+" checked`)
	expect(t, "answer", h.json(&amina, "/attempts/"+aminaAttempt+"/answers", fmt.Sprintf(`{"question_id":%q,"answer":1}`, q)), 200)
	if _, page, _ := h.do(&amina, "GET", "/take/"+id, nil); !chosen.MatchString(page) {
		t.Fatal("the chosen-answer check doesn't find a chosen answer")
	}
	h.do(&amina, "POST", "/attempts/"+aminaAttempt+"/submit", url.Values{"q_" + q: {"1"}})
	h.do(&tobi, "POST", "/attempts/"+tobiAttempt+"/submit", url.Values{"q_" + q: {"0"}})

	// It ends and the scores go out.
	h.now = t0.Add(3 * time.Hour)
	expect(t, "publish", func() int { c, _, _ := h.do(&teacher, "POST", "/assessments/"+id+"/publish", url.Values{}); return c }(), 303)
	// Old links to Results go to Submissions.
	code, _, loc := h.do(&teacher, "GET", "/assessments/"+id+"/results", nil)
	if code != http.StatusMovedPermanently || loc != "/assessments/"+id+"/submissions" {
		t.Fatalf("old results link: %d to %q", code, loc)
	}
	_, body, _ := h.do(&teacher, "GET", "/assessments/"+id+"/submissions", nil)
	mustContain(t, "submissions", body, "Submissions", "Manage", "Reset exam", "Delete exam", "Their score is on their report card")

	// --- Reset Amina, even though the exam has ended. ---
	_, _, loc = h.do(&teacher, "POST", "/assessments/"+id+"/attempts/"+aminaAttempt+"/reset", url.Values{})
	if !strings.Contains(loc, "Tick+the+box") {
		t.Fatalf("reset without confirming went to %q", loc)
	}
	_, _, loc = h.do(&teacher, "POST", "/assessments/"+id+"/attempts/"+aminaAttempt+"/reset", url.Values{"confirm": {"on"}})
	if !strings.Contains(loc, "Reset+Amina+Okafor") || !strings.Contains(loc, "taken+off+their+report+card") {
		t.Fatalf("reset went to %q", loc)
	}
	if got := h.school.withdrawn; len(got) != 1 || got[0] != "ams:"+id+"|student-1" {
		t.Errorf("withdrawn = %v, want Amina's score only", got)
	}
	// She sits it again now, with a fresh 20 minutes and no answers.
	code, body, _ = h.do(&amina, "GET", "/take/"+id, nil)
	expect(t, "after reset", code, 200)
	mustContain(t, "after reset", body, `data-seconds-left="1200"`, "2 + 2?")
	if chosen.MatchString(body) {
		t.Error("her old answer is still chosen")
	}
	expect(t, "answer after reset", h.json(&amina, "/attempts/"+aminaAttempt+"/answers", fmt.Sprintf(`{"question_id":%q,"answer":1}`, q)), 200)
	_, body, _ = h.do(&teacher, "GET", "/assessments/"+id+"/submissions", nil)
	mustContain(t, "results after reset", body, "Amina Okafor", "Writing")

	// --- Delete Tobi's completely. ---
	_, _, loc = h.do(&teacher, "POST", "/assessments/"+id+"/attempts/"+tobiAttempt+"/delete", url.Values{"confirm": {"on"}})
	if !strings.Contains(loc, "Deleted+Tobi+Adeyemi") {
		t.Fatalf("delete went to %q", loc)
	}
	if got := h.school.withdrawn; len(got) != 2 || got[1] != "ams:"+id+"|student-2" {
		t.Errorf("withdrawn = %v, want Tobi's score too", got)
	}
	_, body, _ = h.do(&teacher, "GET", "/assessments/"+id+"/submissions", nil)
	if strings.Contains(body, "Tobi Adeyemi") {
		t.Error("Tobi still shows in the results")
	}
	// The exam has ended, so he can't start again unless it's reopened.
	_, body, _ = h.do(&tobi, "GET", "/take/"+id, nil)
	mustContain(t, "Tobi after delete", body, "This has closed")

	// Another assessment's attempt can't be changed through this one.
	code, _, _ = h.do(&teacher, "POST", "/assessments/"+id+"/attempts/00000000-0000-0000-0000-000000000000/delete", url.Values{"confirm": {"on"}})
	expect(t, "unknown attempt", code, 404)
}

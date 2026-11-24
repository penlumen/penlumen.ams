package web

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/penlumen/penlumen.ams/internal/auth"
)

// A 1×1 PNG.
var tinyPNG = []byte("\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR\x00\x00\x00\x01\x00\x00\x00\x01\x08\x06\x00\x00\x00\x1f\x15\xc4\x89\x00\x00\x00\rIDATx\x9cc\xf8\x0f\x00\x00\x01\x01\x00\x05\x18\xd8N\x00\x00\x00\x00IEND\xaeB`\x82")

var imageURL = regexp.MustCompile(`^/images/[0-9a-f-]{36}$`)

// uploadPicture sends tinyPNG and returns the picture's address.
func (h *harness) uploadPicture(user *auth.User, path string) string {
	t := h.t
	t.Helper()
	code, body := h.post(user, path, "image/png", tinyPNG)
	t.Helper()
	var out struct{ URL string }
	if code != http.StatusOK || json.Unmarshal(body, &out) != nil || !imageURL.MatchString(out.URL) {
		t.Fatalf("upload: %d %s", code, body)
	}
	return out.URL
}

func TestEditQuestionsFormattingAndPictures(t *testing.T) {
	h := newHarness(t)
	t0 := h.now
	local := func(d time.Duration) string { return t0.Add(d).Format(formTime) }
	_, _, loc := h.do(&teacher, "POST", "/assessments", url.Values{
		"title": {"Chemistry test"}, "kind": {"ca"}, "subject": {"Basic Science"},
		"class": {"class-2a"}, "term": {"term-1"}, "report_marks": {"10"}, "duration": {"20"},
		"opens_at": {local(time.Hour)}, "closes_at": {local(2 * time.Hour)},
	})
	id := strings.TrimPrefix(loc, "/assessments/")

	// A picture for a question; only real pictures are kept.
	pic := h.uploadPicture(&teacher, "/assessments/"+id+"/images")
	code, _ := h.post(&teacher, "/assessments/"+id+"/images", "image/svg+xml", []byte(`<svg xmlns="http://www.w3.org/2000/svg"><script>alert(1)</script></svg>`))
	expect(t, "svg refused", code, 400)
	code, _ = h.post(&other, "/assessments/"+id+"/images", "image/png", tinyPNG)
	expect(t, "another school's teacher", code, 404)

	// Formatting and the picture stay; a script and an outside picture don't.
	body := `What is H<sub>2</sub>O?<script>alert(1)</script><img src="` + pic + `"><img src="https://evil.test/x.png">`
	h.do(&teacher, "POST", "/assessments/"+id+"/questions", url.Values{
		"kind": {"objective"}, "body": {body}, "option": {"Water", "Salt", "", ""}, "answer": {"0"}, "marks": {"1"}})
	h.do(&teacher, "POST", "/assessments/"+id+"/questions", url.Values{
		"kind": {"theory"}, "body": {"Draw a water molecule."}, "marks": {"4"}, "guide": {"Two H, one O"}})
	_, page, _ := h.do(&teacher, "GET", "/assessments/"+id, nil)
	mustContain(t, "builder", page, `What is H<sub>2</sub>O?<img src="`+pic+`">`, "Edit question 1", "Save changes")
	if strings.Contains(page, "alert(1)") || strings.Contains(page, "evil.test/x.png\"") {
		t.Fatal("a script or outside picture got through")
	}

	// Editing: wording, options, the right answer and marks; it stays objective.
	qid := regexp.MustCompile(`id="q-([0-9a-f-]{36})"`).FindStringSubmatch(page)[1]
	code, page, _ = h.do(&teacher, "POST", "/assessments/"+id+"/questions/"+qid, url.Values{
		"kind": {"theory"}, "body": {"<br>"}, "option": {"Water", "Salt", "", ""}, "answer": {"0"}, "marks": {"1"}})
	expect(t, "blank edit", code, 422)
	mustContain(t, "blank edit", page, "Write the question.")
	code, _, loc = h.do(&teacher, "POST", "/assessments/"+id+"/questions/"+qid, url.Values{
		"kind": {"theory"}, "body": {"What is <b>H<sub>2</sub>O</b>?"}, "option": {"Salt", "Water", "Ice", ""}, "answer": {"1"}, "marks": {"2"}})
	expect(t, "edit", code, 303)
	if loc != "/assessments/"+id+"#q-"+qid {
		t.Errorf("edit went to %q", loc)
	}
	_, page, _ = h.do(&teacher, "GET", "/assessments/"+id, nil)
	mustContain(t, "after edit", page, "What is <b>H<sub>2</sub>O</b>?", "1 objective, 1 theory · 6 marks", ">Ice<")
	if !regexp.MustCompile(`data-right="true"><span[^>]*>B</span><span>Water`).MatchString(page) {
		t.Error("Water (B) isn't the right answer after the edit")
	}

	// The CSV file has the words; formatting and pictures stay in the app.
	_, csv, _ := h.do(&teacher, "GET", "/assessments/"+id+"/questions.csv", nil)
	mustContain(t, "csv", csv, "What is H2O?")

	// Scheduled: no more edits.
	h.do(&teacher, "POST", "/assessments/"+id+"/status", url.Values{"action": {"schedule"}})
	code, _, _ = h.do(&teacher, "POST", "/assessments/"+id+"/questions/"+qid, url.Values{
		"body": {"Changed"}, "option": {"A", "B"}, "answer": {"0"}, "marks": {"1"}})
	expect(t, "edit once scheduled", code, 409)

	// Students see the question's picture only once they've started.
	code, _, _ = h.get(&amina, pic)
	expect(t, "picture before starting", code, 404)
	h.now = t0.Add(70 * time.Minute)
	h.do(&amina, "POST", "/take/"+id+"/start", url.Values{"agree": {"on"}})
	h.do(&tobi, "POST", "/take/"+id+"/start", url.Values{"agree": {"on"}})
	code, img, kind := h.get(&amina, pic)
	if code != 200 || kind != "image/png" || string(img) != string(tinyPNG) {
		t.Fatalf("question picture: %d %s", code, kind)
	}

	// A written answer with formatting and a picture of her working.
	_, exam, _ := h.do(&amina, "GET", "/take/"+id, nil)
	attempt := attemptID(exam)
	mustContain(t, "exam", exam, `What is <b>H<sub>2</sub>O</b>?`, `data-upload="/attempts/`+attempt+`/images"`)
	var theoryQ string
	for _, m := range questionID.FindAllStringSubmatch(exam, -1) {
		if m[2] == "theory" {
			theoryQ = m[1]
		}
	}
	answerPic := h.uploadPicture(&amina, "/attempts/"+attempt+"/images")
	code, _ = h.post(&tobi, "/attempts/"+attempt+"/images", "image/png", tinyPNG)
	expect(t, "upload to someone else's exam", code, 404)
	answer := fmt.Sprintf(`<p>Two <i>hydrogen</i>, one oxygen:</p><img src=%q><img src=x onerror="alert(2)">`, answerPic)
	b, _ := json.Marshal(map[string]any{"question_id": theoryQ, "text": answer})
	expect(t, "answer", h.json(&amina, "/attempts/"+attempt+"/answers", string(b)), 200)
	code, _, _ = h.get(&tobi, answerPic)
	expect(t, "another student sees her picture", code, 404)
	code, _, _ = h.get(&amina, answerPic)
	expect(t, "her own picture", code, 200)
	h.do(&amina, "POST", "/attempts/"+attempt+"/submit", url.Values{"t_" + theoryQ: {answer}})
	code, _ = h.post(&amina, "/attempts/"+attempt+"/images", "image/png", tinyPNG)
	expect(t, "upload after submitting", code, 409)

	// The teacher marks it, picture and all, with the script gone.
	_, marking, _ := h.do(&teacher, "GET", "/marking/"+id+"?script="+attempt, nil)
	mustContain(t, "marking", marking, `<p>Two <i>hydrogen</i>, one oxygen:</p><img src="`+answerPic+`">`)
	if strings.Contains(marking, "alert(2)") {
		t.Fatal("a script in an answer reached the teacher")
	}
	code, _, _ = h.get(&teacher, answerPic)
	expect(t, "teacher sees the answer picture", code, 200)
	code, _, _ = h.get(&other, answerPic)
	expect(t, "another school's teacher", code, 404)
}

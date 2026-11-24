package web

import (
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/penlumen/penlumen.ams/internal/auth"
	"github.com/penlumen/penlumen.ams/internal/seb"
)

const sebKey = "4f8c2a91d03b7e65a1c9f2e48b07d3a6c5e19f80b2d4a7c63e9f1b05d8a2c47e"

// jsonAs posts JSON as user with extra headers (pairs).
func (h *harness) jsonAs(user *auth.User, path, body string, header ...string) int {
	h.t.Helper()
	req, _ := http.NewRequest(http.MethodPost, h.srv.URL+path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	for i := 0; i+1 < len(header); i += 2 {
		req.Header.Set(header[i], header[i+1])
	}
	req.AddCookie(newCookie(h, user))
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		h.t.Fatal(err)
	}
	res.Body.Close()
	return res.StatusCode
}

func TestSafeExamBrowser(t *testing.T) {
	h := newHarness(t)
	t0 := h.now
	local := func(d time.Duration) string { return t0.Add(d).Format(formTime) }
	form := url.Values{
		"title": {"Mock exam"}, "kind": {"exam"}, "subject": {"Basic Science"},
		"class": {"class-2a"}, "term": {"term-1"}, "report_marks": {"60"}, "duration": {"30"},
		"opens_at": {local(time.Hour)}, "closes_at": {local(3 * time.Hour)},
		"require_seb": {"on"}, "seb_keys": {"not-a-key"},
	}
	code, body, _ := h.do(&teacher, "POST", "/assessments", form)
	expect(t, "bad config key", code, 422)
	mustContain(t, "bad config key", body, "A Config Key is 64 letters and numbers")

	form.Set("seb_keys", strings.ToUpper(sebKey))
	_, _, loc := h.do(&teacher, "POST", "/assessments", form)
	id := strings.TrimPrefix(loc, "/assessments/")
	h.do(&teacher, "POST", "/assessments/"+id+"/questions", url.Values{
		"kind": {"objective"}, "body": {"2 + 2?"}, "option": {"3", "4", "", ""}, "answer": {"1"}, "marks": {"1"}})
	h.do(&teacher, "POST", "/assessments/"+id+"/status", url.Values{"action": {"schedule"}})
	_, body, _ = h.do(&teacher, "GET", "/assessments/"+id, nil)
	mustContain(t, "builder", body, "Required · Config Key checked", "/seb/"+id+".seb")

	// The settings file SEB downloads itself, without signing in.
	code, body, _ = h.do(nil, "GET", "/seb/"+id+".seb", nil)
	expect(t, "seb file", code, 200)
	mustContain(t, "seb file", body, "<string>http://app.test/take/"+id+"</string>", "<key>sendBrowserExamKey</key>")

	h.now = t0.Add(90 * time.Minute)
	signed := func(path string) []string {
		return []string{seb.HeaderConfigKey, seb.Hash("http://app.test"+path, sebKey)}
	}

	// An ordinary browser is turned away, with a button to open SEB.
	code, body, _ = h.do(&amina, "GET", "/take/"+id, nil)
	expect(t, "ordinary browser", code, 403)
	mustContain(t, "ordinary browser", body, "Open this exam in Safe Exam Browser", "seb://app.test/seb/"+id+".seb")
	code, _, _ = h.do(&amina, "POST", "/take/"+id+"/start", url.Values{"agree": {"on"}})
	expect(t, "start outside SEB", code, 403)
	// A fake SEB user agent doesn't pass when there's a Config Key.
	code, _, _ = h.do(&amina, "GET", "/take/"+id, nil, "User-Agent", "Mozilla/5.0 SEB/3.7")
	expect(t, "fake user agent", code, 403)
	// A hash for another page doesn't pass either.
	code, _, _ = h.do(&amina, "GET", "/take/"+id, nil, signed("/take/other")...)
	expect(t, "replayed hash", code, 403)

	// SEB, signing each request, gets in.
	code, body, _ = h.do(&amina, "GET", "/take/"+id, nil, signed("/take/"+id)...)
	expect(t, "in SEB", code, 200)
	mustContain(t, "in SEB", body, "Start exam")
	code, _, _ = h.do(&amina, "POST", "/take/"+id+"/start", url.Values{"agree": {"on"}}, signed("/take/"+id+"/start")...)
	expect(t, "start in SEB", code, 303)
	_, body, _ = h.do(&amina, "GET", "/take/"+id, nil, signed("/take/"+id)...)
	attempt := attemptID(body)
	q := questionID.FindStringSubmatch(body)[1]

	answer := fmt.Sprintf(`{"question_id":%q,"answer":1}`, q)
	expect(t, "save outside SEB", h.jsonAs(&amina, "/attempts/"+attempt+"/answers", answer), 403)
	expect(t, "save in SEB", h.jsonAs(&amina, "/attempts/"+attempt+"/answers", answer, signed("/attempts/"+attempt+"/answers")...), 200)
	code, _, _ = h.do(&amina, "POST", "/attempts/"+attempt+"/submit", url.Values{}, signed("/attempts/"+attempt+"/submit")...)
	expect(t, "submit in SEB", code, 303)
	_, body, _ = h.do(&amina, "GET", "/take/"+id, nil, signed("/take/"+id)...)
	mustContain(t, "done in SEB", body, "Exit Safe Exam Browser", `href="/seb/quit"`)

	// Exams without SEB, and drafts, have no settings file.
	code, _, _ = h.do(nil, "GET", "/seb/not-an-exam.seb", nil)
	expect(t, "no such seb file", code, 404)
}

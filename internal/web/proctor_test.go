package web

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/penlumen/penlumen.ams/internal/auth"
)

// A tiny JPEG header is enough for the server, which only checks the type.
var jpeg = append([]byte{0xFF, 0xD8, 0xFF, 0xE0}, bytes.Repeat([]byte{0x42}, 64)...)

// post sends a raw body as user and returns the status and body.
func (h *harness) post(user *auth.User, path, contentType string, body []byte) (int, []byte) {
	h.t.Helper()
	req, _ := http.NewRequest(http.MethodPost, h.srv.URL+path, bytes.NewReader(body))
	req.Header.Set("Content-Type", contentType)
	req.Header.Set("Accept", "application/json")
	if user != nil {
		req.AddCookie(newCookie(h, user))
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		h.t.Fatal(err)
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	return res.StatusCode, b
}

func (h *harness) get(user *auth.User, path string) (int, []byte, string) {
	h.t.Helper()
	req, _ := http.NewRequest(http.MethodGet, h.srv.URL+path, nil)
	if user != nil {
		req.AddCookie(newCookie(h, user))
	}
	res, err := (&http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}).Do(req)
	if err != nil {
		h.t.Fatal(err)
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	return res.StatusCode, b, res.Header.Get("Content-Type")
}

func TestCameraSoundAndScreenMonitoring(t *testing.T) {
	h := newHarness(t)
	t0 := h.now
	local := func(d time.Duration) string { return t0.Add(d).Format(formTime) }
	create := func(title string, proctoring bool) string {
		f := url.Values{
			"title": {title}, "kind": {"exam"}, "subject": {"Basic Science"},
			"class": {"class-2a"}, "term": {"term-1"}, "report_marks": {"60"}, "duration": {"30"},
			"opens_at": {local(time.Hour)}, "closes_at": {local(2 * time.Hour)},
		}
		if proctoring {
			f.Set("proctoring", "on")
		}
		_, _, loc := h.do(&teacher, "POST", "/assessments", f)
		id := strings.TrimPrefix(loc, "/assessments/")
		h.do(&teacher, "POST", "/assessments/"+id+"/questions", url.Values{
			"kind": {"objective"}, "body": {"Pick B"}, "option": {"A", "B", "", ""}, "answer": {"1"}, "marks": {"1"}})
		h.do(&teacher, "POST", "/assessments/"+id+"/status", url.Values{"action": {"schedule"}})
		return id
	}
	id := create("Monitored exam", true)
	plain := create("Plain test", false)
	_, body, _ := h.do(&teacher, "GET", "/assessments/"+id, nil)
	mustContain(t, "builder", body, "Camera, sound and screen", "Photos on alerts · kept 30 days")

	h.now = t0.Add(70 * time.Minute)

	// The start page explains it and keeps Start locked until the check passes.
	_, body, _ = h.do(&amina, "GET", "/take/"+id, nil)
	mustContain(t, "start page", body, "This exam uses your camera, microphone and screen",
		"Sound is never recorded", "deleted 30 days after", "data-proctor-check", "disabled data-proctor-gate", "/static/proctor.js")
	_, body, _ = h.do(&amina, "GET", "/take/"+plain, nil)
	if strings.Contains(body, "data-proctor") {
		t.Fatal("an exam without monitoring asks for the camera")
	}

	h.do(&amina, "POST", "/take/"+id+"/start", url.Values{"agree": {"on"}})
	_, body, _ = h.do(&amina, "GET", "/take/"+id, nil)
	mustContain(t, "exam page", body, `data-proctor data-monitored="false"`, "Share and continue")
	attempt := attemptID(body)
	ping := func(path string) int { code, _ := h.post(&amina, path, "application/json", nil); return code }

	// Monitoring alerts only on monitored exams, and only known ones.
	h.do(&amina, "POST", "/take/"+plain+"/start", url.Values{"agree": {"on"}})
	_, plainBody, _ := h.do(&amina, "GET", "/take/"+plain, nil)
	expect(t, "noise on a plain exam", ping("/attempts/"+attemptID(plainBody)+"/ping?event=noise"), 400)
	expect(t, "unknown event", ping("/attempts/"+attempt+"/ping?event=hacked"), 400)

	// Not sharing yet: the invigilator sees it.
	_, body, _ = h.do(&teacher, "GET", "/monitor/"+id+"/live", nil)
	mustContain(t, "monitor before sharing", body, "Not sharing camera or screen")

	// Sharing starts; a start photo; then noise with a photo of the student
	// and of the screen.
	expect(t, "monitoring on", ping("/attempts/"+attempt+"/ping?event=monitoring_on"), 204)
	snap := func(source, reason string, img []byte) int {
		code, _ := h.post(&amina, fmt.Sprintf("/attempts/%s/snapshot?source=%s&reason=%s", attempt, source, reason), "image/jpeg", img)
		return code
	}
	expect(t, "not a jpeg", snap("camera", "start", []byte("<svg>")), 400)
	expect(t, "bad reason", snap("camera", "because", jpeg), 400)
	expect(t, "too big", snap("camera", "start", append(jpeg, make([]byte, 500<<10)...)), 400)
	expect(t, "start photo", snap("camera", "start", jpeg), 204)
	expect(t, "noise", ping("/attempts/"+attempt+"/ping?event=noise"), 204)
	expect(t, "noise photo", snap("camera", "noise", jpeg), 204)
	expect(t, "noise screen", snap("screen", "noise", jpeg), 204)
	expect(t, "screen stopped", ping("/attempts/"+attempt+"/ping?event=screen_off"), 204)

	_, body, _ = h.do(&amina, "GET", "/take/"+id, nil)
	mustContain(t, "exam page after sharing", body, `data-monitored="true"`)

	_, body, _ = h.do(&teacher, "GET", "/monitor/"+id+"/live", nil)
	mustContain(t, "monitor", body, "Talking or noise 1×", "Camera, mic or screen off 1×", "Evidence · 3 photos")
	if strings.Contains(body, "Not sharing camera or screen") {
		t.Error("still says not sharing")
	}

	code, body, _ := h.do(&teacher, "GET", "/monitor/"+id+"/students/"+attempt, nil)
	expect(t, "evidence", code, 200)
	mustContain(t, "evidence", body, "Amina Okafor", "Start of the exam", "Talking or noise", "Screen sharing stopped",
		"Camera photo of the student", "Photo of the student's screen", "deleted automatically on")
	img := body[strings.Index(body, `src="/snapshots/`)+len(`src="`):]
	img = img[:strings.Index(img, `"`)]

	code, got, kind := h.get(&teacher, img)
	if code != 200 || kind != "image/jpeg" || !bytes.Equal(got, jpeg) {
		t.Errorf("photo for the teacher: %d %s %d bytes", code, kind, len(got))
	}
	if code, _, _ := h.get(&amina, img); code != 403 {
		t.Errorf("photo for a student: %d", code)
	}
	if code, _, _ := h.get(&other, img); code != 404 {
		t.Errorf("photo for another school: %d", code)
	}

	// Photos come only while writing.
	h.do(&amina, "POST", "/attempts/"+attempt+"/submit", url.Values{})
	expect(t, "photo after submitting", snap("camera", "noise", jpeg), 409)

	// 30 days after closing, photos and alerts go; the answers stay. (The
	// cleanup deletes what closed before "now minus KeepEvidence".)
	snaps, events, err := h.store.Purge(t.Context(), t0.Add(2*time.Hour+29*24*time.Hour).Add(-KeepEvidence))
	if err != nil || snaps != 0 || events != 0 {
		t.Fatalf("purge before 30 days: %d photos, %d alerts, %v", snaps, events, err)
	}
	snaps, events, err = h.store.Purge(t.Context(), t0.Add(2*time.Hour+31*24*time.Hour).Add(-KeepEvidence))
	if err != nil || snaps != 3 || events < 3 {
		t.Fatalf("purge after 30 days: %d photos, %d alerts, %v", snaps, events, err)
	}
	_, body, _ = h.do(&teacher, "GET", "/monitor/"+id+"/students/"+attempt, nil)
	mustContain(t, "evidence after purge", body, "No alerts or photos")
	_, body, _ = h.do(&teacher, "GET", "/assessments/"+id+"/submissions", nil)
	mustContain(t, "results after purge", body, "Amina Okafor", "Submitted")
}

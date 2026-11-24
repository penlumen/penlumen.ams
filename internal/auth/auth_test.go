package auth

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestSessionRoundTrip(t *testing.T) {
	s := NewSessions("a-test-secret-that-is-long-enough!", false)
	rec := httptest.NewRecorder()
	if err := s.Set(rec, "c", User{Sub: "u1", Name: "Amina", Role: "student"}, time.Hour); err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	for _, c := range rec.Result().Cookies() {
		req.AddCookie(c)
	}
	var u User
	if err := s.Get(req, "c", &u); err != nil {
		t.Fatal(err)
	}
	if u.Sub != "u1" || !u.IsStudent() {
		t.Errorf("got %+v", u)
	}
}

func TestSessionRejectsTampering(t *testing.T) {
	s := NewSessions("a-test-secret-that-is-long-enough!", false)
	rec := httptest.NewRecorder()
	_ = s.Set(rec, "c", User{Sub: "u1", Role: "student"}, time.Hour)
	cookie := rec.Result().Cookies()[0]

	// Same payload signed with another key.
	other := NewSessions("another-secret-that-is-long-enough", false)
	rec2 := httptest.NewRecorder()
	_ = other.Set(rec2, "c", User{Sub: "u1", Role: "admin"}, time.Hour)
	forged := rec2.Result().Cookies()[0]

	for name, c := range map[string]*http.Cookie{
		"wrong key":    forged,
		"edited":       {Name: "c", Value: "x" + cookie.Value},
		"no signature": {Name: "c", Value: "abc"},
	} {
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req.AddCookie(c)
		var u User
		if err := s.Get(req, "c", &u); err == nil {
			t.Errorf("%s: accepted %+v", name, u)
		}
	}
}

func TestSessionExpires(t *testing.T) {
	s := NewSessions("a-test-secret-that-is-long-enough!", false)
	rec := httptest.NewRecorder()
	_ = s.Set(rec, "c", User{Sub: "u1"}, -time.Minute)
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.AddCookie(&http.Cookie{Name: "c", Value: rec.Result().Cookies()[0].Value})
	var u User
	if err := s.Get(req, "c", &u); err == nil {
		t.Error("expired cookie accepted")
	}
}

// Penlumen School can send the same sign-in back twice. The second callback
// finds the one-time login cookie gone; someone already signed in carries on.
func TestDuplicateCallbackWhenSignedIn(t *testing.T) {
	s := NewSessions("a-test-secret-that-is-long-enough!", false)
	a := New("http://school.test/api/v1/oidc", "id", "secret", "http://app.test/auth/callback", s)

	rec := httptest.NewRecorder()
	_ = s.Set(rec, SessionCookie, User{Sub: "u1", Role: "student"}, time.Hour)
	req := httptest.NewRequest(http.MethodGet, "/auth/callback?code=x&state=stale", nil)
	req.AddCookie(rec.Result().Cookies()[0])

	res := httptest.NewRecorder()
	a.Callback(res, req)
	if res.Code != http.StatusFound || res.Header().Get("Location") != "/" {
		t.Fatalf("signed in: %d %q, want a redirect home", res.Code, res.Header().Get("Location"))
	}

	res = httptest.NewRecorder()
	a.Callback(res, httptest.NewRequest(http.MethodGet, "/auth/callback?code=x&state=stale", nil))
	if res.Code != http.StatusBadRequest {
		t.Fatalf("signed out: %d, want 400", res.Code)
	}
}

func TestSafeReturn(t *testing.T) {
	for in, want := range map[string]string{
		"/assessments/1":    "/assessments/1",
		"":                  "/",
		"https://evil.test": "/",
		"//evil.test":       "/",
		`/\evil.test`:       "/",
	} {
		if got := safeReturn(in); got != want {
			t.Errorf("safeReturn(%q) = %q, want %q", in, got, want)
		}
	}
}

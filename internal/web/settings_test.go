package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/penlumen/penlumen.ams/internal/auth"
	"github.com/penlumen/penlumen.ams/internal/config"
	"github.com/penlumen/penlumen.ams/internal/db"
)

func TestIssuerFromAPIAddress(t *testing.T) {
	for in, want := range map[string]string{
		"https://api.school.com":             "https://api.school.com/api/v1/oidc",
		"api.school.com/":                    "https://api.school.com/api/v1/oidc",
		"http://localhost:5445/api/v1":       "http://localhost:5445/api/v1/oidc",
		"http://localhost:5445/api/v1/oidc/": "http://localhost:5445/api/v1/oidc",
	} {
		got, err := issuerFrom(in)
		if err != nil || got != want {
			t.Errorf("issuerFrom(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	if _, err := issuerFrom("ftp://x"); err == nil {
		t.Error("ftp:// was accepted")
	}
}

func TestIsLocal(t *testing.T) {
	cases := []struct {
		remote, host, forwarded string
		want                    bool
	}{
		{"127.0.0.1:5000", "localhost:8080", "", true},
		{"[::1]:5000", "[::1]:8080", "", true},
		{"127.0.0.1:5000", "exams.school.com", "", false}, // a proxy on the same computer
		{"127.0.0.1:5000", "localhost:8080", "203.0.113.9", false},
		{"192.168.1.20:5000", "localhost:8080", "", false},
	}
	for _, c := range cases {
		r := httptest.NewRequest("GET", "/setup", nil)
		r.RemoteAddr, r.Host = c.remote, c.host
		if c.forwarded != "" {
			r.Header.Set("X-Forwarded-For", c.forwarded)
		}
		if got := isLocal(r); got != c.want {
			t.Errorf("isLocal(%s via %s, forwarded %q) = %v", c.remote, c.host, c.forwarded, got)
		}
	}
}

// A new install: Penlumen School is connected from the setup page (only on
// the computer running AMS), and an administrator changes the port later.
func TestSetupAndSettingsWriteTheSettingsFile(t *testing.T) {
	ctx := context.Background()
	pool, err := db.Open(ctx, "sqlite:"+filepath.Join(t.TempDir(), "penlumen_ams.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { pool.Close() })
	if err := pool.Migrate(); err != nil {
		t.Fatal(err)
	}
	school := httptest.NewServer((&fakeSchool{}).handler())
	t.Cleanup(school.Close)

	file := filepath.Join(t.TempDir(), ".env")
	if err := os.WriteFile(file, []byte("# keep me\nAMS_ADDR=:8080\nSESSION_SECRET=abc\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var restarts atomic.Int32
	settings := &Settings{
		Config: config.Config{File: file, Addr: ":8080", BaseURL: "http://localhost:8080", Timezone: "UTC",
			FromEnvironment: map[string]bool{}},
		Restart: func() { restarts.Add(1) },
	}
	srv := httptest.NewServer(New(Options{DB: pool, Auth: auth.New("", "", "", "", auth.NewSessions(secret, false)),
		BaseURL: "http://localhost:8080", Location: time.UTC, Settings: settings}).Routes())
	t.Cleanup(srv.Close)
	waitRestart := func(want int32) {
		t.Helper()
		for i := 0; i < 40 && restarts.Load() < want; i++ {
			time.Sleep(50 * time.Millisecond)
		}
		if got := restarts.Load(); got != want {
			t.Fatalf("restarted %d times, want %d", got, want)
		}
	}
	read := func() string {
		b, _ := os.ReadFile(file)
		return string(b)
	}

	b := newBrowser(t, srv.URL)
	_, body, _ := b.do("GET", "/setup?mode=connect", nil)
	mustContain(t, "setup", body, "With Penlumen School", "Test connection", "/auth/callback")

	connect := url.Values{"api": {school.URL}, "client_id": {"pl_ems_test"}, "client_secret": {"s3cret"},
		"portal_url": {"https://portal.school.test"}}

	// Not from another computer, or through a proxy.
	req, _ := http.NewRequest("POST", srv.URL+"/setup/connect", strings.NewReader(connect.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-Forwarded-For", "203.0.113.9")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	expect(t, "connect through a proxy", res.StatusCode, http.StatusForbidden)
	if strings.Contains(read(), "SMS_CLIENT_ID") {
		t.Fatal("the settings file changed")
	}

	_, body, _ = b.do("POST", "/setup/test", connect)
	mustContain(t, "test connection", body, `"ok":true`, "Greenfield")

	code, body, _ := b.do("POST", "/setup/connect", connect)
	expect(t, "connect", code, http.StatusOK)
	mustContain(t, "connect", body, "Settings saved")
	waitRestart(1)
	mustContain(t, "settings file", read(), "# keep me", "SESSION_SECRET=abc",
		"SMS_ISSUER="+school.URL+"/api/v1/oidc", "SMS_CLIENT_ID=pl_ems_test", "SMS_CLIENT_SECRET=s3cret",
		"SMS_PORTAL_URL=https://portal.school.test")

	// (The fake restart changed nothing, so this server still runs on its
	// own.) The first administrator, then Settings.
	code, _, _ = b.do("POST", "/setup", url.Values{"school": {"Greenfield"}, "name": {"Mrs Bello"},
		"email": {"bello@school.test"}, "password": {"long enough"}, "confirm": {"long enough"}})
	expect(t, "first administrator", code, http.StatusSeeOther)
	_, body, _ = b.do("GET", "/settings", nil)
	mustContain(t, "settings", body, "Address and time", "Not connected", file)

	code, body, _ = b.do("POST", "/settings", url.Values{"port": {"9090"}, "base_url": {"http://localhost:8080"},
		"timezone": {"Africa/Lagos"}, "action": {"save"}})
	expect(t, "save settings", code, http.StatusOK)
	mustContain(t, "save settings", body, "Settings saved")
	waitRestart(2)
	// The localhost address follows the port.
	mustContain(t, "settings file", read(), "AMS_ADDR=:9090", "AMS_BASE_URL=http://localhost:9090", "AMS_TIMEZONE=Africa/Lagos")

	code, body, _ = b.do("POST", "/settings", url.Values{"port": {"99999"}, "base_url": {"http://localhost:8080"},
		"timezone": {"Mars/Base"}})
	expect(t, "bad settings", code, http.StatusUnprocessableEntity)
	mustContain(t, "bad settings", body, "from 1 to 65535", "Pick a time zone")
}

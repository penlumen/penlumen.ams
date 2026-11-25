package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The first run makes a settings file with a random session secret, and
// Load reads it back.
func TestLoadCreatesTheSettingsFile(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir) // a test binary lives in a temporary folder, so Find uses this
	for _, k := range Keys {
		t.Setenv(k, "")
	}
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.Created || len(cfg.SessionSecret) < 32 || cfg.Connected() {
		t.Fatalf("created %v, secret %d characters, connected %v", cfg.Created, len(cfg.SessionSecret), cfg.Connected())
	}
	if want := "sqlite:" + filepath.Join(dir, "penlumen_ams.db"); cfg.DatabaseURL != want {
		t.Errorf("database %q, want %q", cfg.DatabaseURL, want)
	}
	again, err := Load()
	if err != nil || again.Created || again.SessionSecret != cfg.SessionSecret {
		t.Fatalf("second load: created %v, same secret %v, %v", again.Created, again.SessionSecret == cfg.SessionSecret, err)
	}

	// The environment wins over the file.
	t.Setenv("AMS_TIMEZONE", "Europe/London")
	env, err := Load()
	if err != nil || env.Timezone != "Europe/London" || !env.FromEnvironment["AMS_TIMEZONE"] {
		t.Fatalf("timezone %q from environment %v: %v", env.Timezone, env.FromEnvironment["AMS_TIMEZONE"], err)
	}
}

func TestSaveKeepsCommentsAndOtherLines(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".env")
	os.WriteFile(path, []byte("# about the port\r\nAMS_ADDR=:8080\r\nSMS_CLIENT_ID=old\r\nOTHER=1\r\n"), 0o600)
	if err := Save(path, map[string]string{"AMS_ADDR": ":9090", "SMS_CLIENT_ID": "", "SMS_PORTAL_URL": "https://my school"}); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(path)
	want := "# about the port\nAMS_ADDR=:9090\nSMS_CLIENT_ID=\nOTHER=1\nSMS_PORTAL_URL=\"https://my school\"\n"
	if string(b) != want {
		t.Fatalf("got\n%s\nwant\n%s", b, want)
	}
	values, _ := readFile(path)
	if values["SMS_PORTAL_URL"] != "https://my school" {
		t.Errorf("read back %q", values["SMS_PORTAL_URL"])
	}
}

func TestDatabaseBesideSettings(t *testing.T) {
	dir := filepath.Join("C:", "Exams")
	for in, want := range map[string]string{
		"sqlite:penlumen_ams.db":         "sqlite:" + filepath.Join(dir, "penlumen_ams.db"),
		"postgres://u:p@db/penlumen_ams": "postgres://u:p@db/penlumen_ams",
	} {
		if got := besideSettings(in, dir); got != want {
			t.Errorf("besideSettings(%q) = %q, want %q", in, got, want)
		}
	}
	abs, _ := filepath.Abs("x.db")
	if got := besideSettings("sqlite:"+abs, dir); !strings.HasSuffix(got, abs) {
		t.Errorf("an absolute path moved: %q", got)
	}
}

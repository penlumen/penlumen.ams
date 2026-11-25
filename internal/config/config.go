// Package config reads the app's settings: environment variables first, then
// a settings file (.env). The file is looked for in the working directory,
// then next to the program; a school that double-clicks ams.exe gets one
// created next to it on first run, and fills it in from the setup page.
package config

import (
	"bufio"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/penlumen/penlumen.ams/internal/db"
)

// FileName is the settings file.
const FileName = ".env"

type Config struct {
	// Addr is where the server listens, e.g. ":8080".
	Addr string
	// BaseURL is the address people open the app at, without a trailing slash.
	BaseURL string
	// DatabaseURL is the app's own database: a SQLite file (sqlite:penlumen_ams.db,
	// the default) or Postgres (postgres://…).
	DatabaseURL string
	// SessionSecret signs sign-in cookies. At least 32 characters.
	SessionSecret string
	// SMSIssuer is Penlumen School's OpenID Connect issuer,
	// e.g. http://localhost:5445/api/v1/oidc.
	SMSIssuer string
	// ClientID and ClientSecret come from School Settings › Integrations.
	// Both empty: AMS runs on its own, with its own people and classes.
	ClientID     string
	ClientSecret string
	// Location is the school's time zone: opening and closing times are
	// typed and shown in it.
	Location *time.Location
	// Timezone is Location's name, e.g. Africa/Lagos.
	Timezone string
	// WebhookSecret verifies Penlumen School's webhooks (optional: without
	// it, changes show up within directory.TTL instead of at once).
	WebhookSecret string
	// PortalURL is Penlumen School's web app, linked from the header.
	PortalURL string

	// File is the settings file in use (it may not exist yet).
	File string
	// Created is true when the settings file was made on this start.
	Created bool
	// FromEnvironment lists settings set by environment variables: they win
	// over the file, so the settings page can't change them.
	FromEnvironment map[string]bool
}

// SecureCookies is true when the app is served over HTTPS.
func (c Config) SecureCookies() bool {
	return strings.HasPrefix(c.BaseURL, "https://")
}

// Connected is true when Penlumen School is set up.
func (c Config) Connected() bool { return c.ClientID != "" }

// Keys are the settings AMS reads.
var Keys = []string{
	"AMS_ADDR", "AMS_BASE_URL", "AMS_TIMEZONE", "SESSION_SECRET", "DATABASE_URL",
	"SMS_ISSUER", "SMS_CLIENT_ID", "SMS_CLIENT_SECRET", "SMS_WEBHOOK_SECRET", "SMS_PORTAL_URL",
}

// Load reads the settings. With no settings file anywhere (and no
// SESSION_SECRET in the environment) it creates one next to the program,
// with a random session secret, so the first double-click just works.
func Load() (Config, error) {
	file := Find()
	values, err := readFile(file)
	if err != nil {
		return Config{}, err
	}
	created := false
	if values == nil && os.Getenv("SESSION_SECRET") == "" {
		if err := create(file); err != nil {
			return Config{}, fmt.Errorf("couldn't create the settings file %s: %w", file, err)
		}
		created = true
		if values, err = readFile(file); err != nil {
			return Config{}, err
		}
	}

	fromEnv := map[string]bool{}
	get := func(name, fallback string) string {
		if v, ok := os.LookupEnv(name); ok && v != "" {
			fromEnv[name] = true
			return v
		}
		if v := values[name]; v != "" {
			return v
		}
		return fallback
	}
	cfg := Config{
		Addr:            get("AMS_ADDR", ":8080"),
		BaseURL:         strings.TrimRight(get("AMS_BASE_URL", "http://localhost:8080"), "/"),
		DatabaseURL:     get("DATABASE_URL", db.DefaultURL),
		SessionSecret:   get("SESSION_SECRET", ""),
		SMSIssuer:       strings.TrimRight(get("SMS_ISSUER", "http://localhost:5445/api/v1/oidc"), "/"),
		ClientID:        get("SMS_CLIENT_ID", ""),
		ClientSecret:    get("SMS_CLIENT_SECRET", ""),
		WebhookSecret:   get("SMS_WEBHOOK_SECRET", ""),
		PortalURL:       strings.TrimRight(get("SMS_PORTAL_URL", "http://localhost:3000"), "/"),
		Timezone:        get("AMS_TIMEZONE", "Africa/Lagos"),
		File:            file,
		Created:         created,
		FromEnvironment: fromEnv,
	}
	cfg.DatabaseURL = besideSettings(cfg.DatabaseURL, filepath.Dir(file))

	if cfg.SessionSecret == "" {
		return cfg, fmt.Errorf("missing setting: SESSION_SECRET (in %s)", file)
	}
	if len(cfg.SessionSecret) < 32 {
		return cfg, errors.New("SESSION_SECRET must be at least 32 characters")
	}
	// Penlumen School is optional: without it AMS runs on its own.
	if (cfg.ClientID == "") != (cfg.ClientSecret == "") {
		return cfg, errors.New("set both SMS_CLIENT_ID and SMS_CLIENT_SECRET to connect Penlumen School, or neither to run AMS on its own")
	}
	loc, err := time.LoadLocation(cfg.Timezone)
	if err != nil {
		return cfg, fmt.Errorf("AMS_TIMEZONE %q isn't a time zone, e.g. Africa/Lagos", cfg.Timezone)
	}
	cfg.Location = loc
	return cfg, nil
}

// Find is the settings file to use: .env in the working directory if there
// is one, otherwise .env next to the program (made there on first run). A
// program built by `go run` or `go test` lives in a temporary go-build
// folder, so it uses the working directory instead.
func Find() string {
	if _, err := os.Stat(FileName); err == nil {
		abs, _ := filepath.Abs(FileName)
		return abs
	}
	if exe, err := os.Executable(); err == nil {
		dir := filepath.Dir(exe)
		if resolved, err := filepath.EvalSymlinks(dir); err == nil {
			dir = resolved
		}
		if !strings.Contains(dir, "go-build") {
			return filepath.Join(dir, FileName)
		}
	}
	abs, _ := filepath.Abs(FileName)
	return abs
}

// besideSettings makes a relative SQLite path relative to the settings
// file's folder, so a double-clicked ams.exe keeps its database next to it
// whatever folder Windows starts it in.
func besideSettings(url, dir string) string {
	if strings.HasPrefix(url, "postgres://") || strings.HasPrefix(url, "postgresql://") {
		return url
	}
	path := strings.TrimPrefix(strings.TrimPrefix(url, "sqlite:"), "//")
	if path == "" || filepath.IsAbs(path) {
		return url
	}
	return "sqlite:" + filepath.Join(dir, path)
}

// readFile reads KEY=VALUE lines; nil (no error) when there's no file.
func readFile(path string) (map[string]string, error) {
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	values := map[string]string{}
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		values[strings.TrimSpace(key)] = strings.Trim(strings.TrimSpace(value), `"'`)
	}
	return values, scanner.Err()
}

// NewSecret is a random value for SESSION_SECRET.
func NewSecret() string {
	b := make([]byte, 36)
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

func create(path string) error {
	content := fmt.Sprintf(`# Settings for Exams (Penlumen AMS). Change them from the app (Settings,
# or the setup page on first run) rather than here; restart AMS after
# editing this file by hand.

AMS_ADDR=:8080
AMS_BASE_URL=http://localhost:8080
AMS_TIMEZONE=Africa/Lagos

# Signs sign-in cookies. Keep it secret; changing it signs everyone out.
SESSION_SECRET=%s

# A SQLite file next to this one; or a postgres:// URL.
DATABASE_URL=sqlite:penlumen_ams.db

# Penlumen School (optional): filled in when you connect it on the setup page.
SMS_ISSUER=
SMS_CLIENT_ID=
SMS_CLIENT_SECRET=
SMS_WEBHOOK_SECRET=
SMS_PORTAL_URL=
`, NewSecret())
	return writeAtomic(path, content)
}

// Save sets keys in the settings file, keeping its other lines and
// comments, and adds keys it didn't have. An empty value clears a key.
func Save(path string, changes map[string]string) error {
	existing, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	lines := strings.Split(strings.ReplaceAll(string(existing), "\r\n", "\n"), "\n")
	done := map[string]bool{}
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		key, _, ok := strings.Cut(trimmed, "=")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		if value, change := changes[key]; change {
			lines[i] = key + "=" + quote(value)
			done[key] = true
		}
	}
	var added []string
	for key := range changes {
		if !done[key] {
			added = append(added, key)
		}
	}
	sort.Strings(added)
	for len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	for _, key := range added {
		lines = append(lines, key+"="+quote(changes[key]))
	}
	return writeAtomic(path, strings.Join(lines, "\n")+"\n")
}

// quote wraps values with spaces or # in quotes, so they read back the same.
func quote(v string) string {
	v = strings.ReplaceAll(strings.ReplaceAll(v, "\n", ""), "\r", "")
	if strings.ContainsAny(v, " #\"'") {
		return `"` + strings.ReplaceAll(v, `"`, "") + `"`
	}
	return v
}

// writeAtomic writes through a temporary file, so a crash never leaves a
// half-written settings file. It's only readable by the owner: it holds
// secrets.
func writeAtomic(path, content string) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(content), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

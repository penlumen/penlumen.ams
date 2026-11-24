// Package config reads the app's settings from environment variables, and
// from a .env file in the working directory when there is one.
package config

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/penlumen/penlumen.ams/internal/db"
)

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
	// WebhookSecret verifies Penlumen School's webhooks (optional: without
	// it, changes show up within directory.TTL instead of at once).
	WebhookSecret string
	// PortalURL is Penlumen School's web app, linked from the header.
	PortalURL string
}

// SecureCookies is true when the app is served over HTTPS.
func (c Config) SecureCookies() bool {
	return strings.HasPrefix(c.BaseURL, "https://")
}

func Load() (Config, error) {
	if err := loadDotEnv(".env"); err != nil {
		return Config{}, err
	}
	cfg := Config{
		Addr:          env("AMS_ADDR", ":8080"),
		BaseURL:       strings.TrimRight(env("AMS_BASE_URL", "http://localhost:8080"), "/"),
		DatabaseURL:   env("DATABASE_URL", db.DefaultURL),
		SessionSecret: os.Getenv("SESSION_SECRET"),
		SMSIssuer:     strings.TrimRight(env("SMS_ISSUER", "http://localhost:5445/api/v1/oidc"), "/"),
		ClientID:      os.Getenv("SMS_CLIENT_ID"),
		ClientSecret:  os.Getenv("SMS_CLIENT_SECRET"),
		WebhookSecret: os.Getenv("SMS_WEBHOOK_SECRET"),
		PortalURL:     strings.TrimRight(env("SMS_PORTAL_URL", "http://localhost:3000"), "/"),
	}

	if cfg.SessionSecret == "" {
		return cfg, errors.New("missing setting: SESSION_SECRET (copy .env.example to .env)")
	}
	// Penlumen School is optional: without it AMS runs on its own.
	if (cfg.ClientID == "") != (cfg.ClientSecret == "") {
		return cfg, errors.New("set both SMS_CLIENT_ID and SMS_CLIENT_SECRET to connect Penlumen School, or neither to run AMS on its own")
	}
	if len(cfg.SessionSecret) < 32 {
		return cfg, errors.New("SESSION_SECRET must be at least 32 characters")
	}
	zone := env("AMS_TIMEZONE", "Africa/Lagos")
	loc, err := time.LoadLocation(zone)
	if err != nil {
		return cfg, fmt.Errorf("AMS_TIMEZONE %q isn't a time zone, e.g. Africa/Lagos", zone)
	}
	cfg.Location = loc
	return cfg, nil
}

func env(name, fallback string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return fallback
}

// loadDotEnv sets KEY=VALUE lines from path. Variables already set in the
// environment win, so a real deployment never reads stale values from a file.
func loadDotEnv(path string) error {
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer f.Close()

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
		key = strings.TrimSpace(key)
		value = strings.Trim(strings.TrimSpace(value), `"'`)
		if _, set := os.LookupEnv(key); !set {
			os.Setenv(key, value)
		}
	}
	return scanner.Err()
}

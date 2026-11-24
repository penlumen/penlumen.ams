// Command ams runs the Penlumen Assessment Management System: tests, CA and
// exams for schools that use Penlumen School.
package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
	_ "time/tzdata" // time zones even on systems without them (Windows, distroless)

	"github.com/penlumen/penlumen.ams/internal/auth"
	"github.com/penlumen/penlumen.ams/internal/config"
	"github.com/penlumen/penlumen.ams/internal/db"
	"github.com/penlumen/penlumen.ams/internal/schoolsync"
	"github.com/penlumen/penlumen.ams/internal/sms"
	"github.com/penlumen/penlumen.ams/internal/store"
	"github.com/penlumen/penlumen.ams/internal/web"
)

func main() {
	if err := run(); err != nil {
		slog.Error("ams stopped", "error", err)
		os.Exit(1)
	}
}

func run() error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	cfg, err := config.Load()
	if err != nil {
		return err
	}

	database, err := db.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer database.Close()
	slog.Info("database", "kind", database.Dialect)
	if err := database.Migrate(); err != nil {
		return err
	}

	sessions := auth.NewSessions(cfg.SessionSecret, cfg.SecureCookies())
	// Penlumen School is optional. With it, AMS keeps a copy of its people,
	// classes and terms (synced now, every few minutes and on its webhooks)
	// and offers "Sign in with your school portal"; without it, AMS has its
	// own. Either way AMS works when Penlumen School is down.
	var school *sms.Client
	var syncer *schoolsync.Syncer
	issuer := ""
	if cfg.ClientID != "" {
		issuer = cfg.SMSIssuer
		school = sms.New(cfg.SMSIssuer, cfg.ClientID, cfg.ClientSecret)
		syncer = schoolsync.New(school, store.New(database))
		syncer.Start(ctx)
		slog.Info("penlumen school connected", "issuer", cfg.SMSIssuer)
	} else {
		slog.Info("running on its own: no penlumen school connected")
	}
	signIn := auth.New(issuer, cfg.ClientID, cfg.ClientSecret, cfg.BaseURL+"/auth/callback", sessions)

	app := web.New(web.Options{
		DB:            database,
		Auth:          signIn,
		School:        school,
		Sync:          syncer,
		BaseURL:       cfg.BaseURL,
		PortalURL:     cfg.PortalURL,
		WebhookSecret: cfg.WebhookSecret,
		Location:      cfg.Location,
	})
	// Photos and alerts from camera, sound and screen monitoring are kept 30
	// days after an exam closes.
	app.StartCleanup(ctx)

	srv := &http.Server{
		Addr:              cfg.Addr,
		Handler:           app.Routes(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       2 * time.Minute,
	}

	errs := make(chan error, 1)
	go func() {
		slog.Info("ams listening", "addr", cfg.Addr, "url", cfg.BaseURL)
		errs <- srv.ListenAndServe()
	}()

	select {
	case err := <-errs:
		if !errors.Is(err, http.ErrServerClosed) {
			return err
		}
	case <-ctx.Done():
		slog.Info("shutting down")
		shutdown, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		return srv.Shutdown(shutdown)
	}
	return nil
}

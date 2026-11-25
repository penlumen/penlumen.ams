// Command ams runs the Penlumen Assessment Management System: tests, CA and
// exams for schools that use Penlumen School, or on its own.
//
// A school can just double-click ams.exe: it creates its settings file next
// to itself, opens the browser at the setup page, and runs until its window
// is closed.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
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

// errRestart asks main to start again with the settings just saved.
var errRestart = errors.New("restart")

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	setConsoleTitle("Exams")

	first := true
	for {
		err := run(ctx, first)
		first = false
		if errors.Is(err, errRestart) {
			slog.Info("restarting with the new settings")
			continue
		}
		if err != nil {
			slog.Error("ams stopped", "error", err)
			if ownConsole() {
				// Double-clicked: keep the window open so the message can be read.
				fmt.Fprintf(os.Stderr, "\n  Exams couldn't start: %v\n\n  Press Enter to close this window.", err)
				fmt.Scanln()
			}
			os.Exit(1)
		}
		return
	}
}

func run(ctx context.Context, first bool) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	if cfg.Created {
		slog.Info("settings file created", "file", cfg.File)
	}
	// Stops the sync and clean-up when this run ends (on a restart too).
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

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
	if cfg.Connected() {
		issuer = cfg.SMSIssuer
		school = sms.New(cfg.SMSIssuer, cfg.ClientID, cfg.ClientSecret)
		syncer = schoolsync.New(school, store.New(database))
		syncer.Start(ctx)
		slog.Info("penlumen school connected", "issuer", cfg.SMSIssuer)
	} else {
		slog.Info("running on its own: no penlumen school connected")
	}
	signIn := auth.New(issuer, cfg.ClientID, cfg.ClientSecret, cfg.BaseURL+"/auth/callback", sessions)

	restart := make(chan struct{}, 1)
	app := web.New(web.Options{
		DB:            database,
		Auth:          signIn,
		School:        school,
		Sync:          syncer,
		BaseURL:       cfg.BaseURL,
		PortalURL:     cfg.PortalURL,
		WebhookSecret: cfg.WebhookSecret,
		Location:      cfg.Location,
		Settings: &web.Settings{
			Config: cfg,
			Restart: func() {
				select {
				case restart <- struct{}{}:
				default:
				}
			},
		},
	})
	// Photos and alerts from camera, sound and screen monitoring are kept 30
	// days after an exam closes.
	app.StartCleanup(ctx)

	srv := &http.Server{
		Handler:           app.Routes(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       2 * time.Minute,
	}
	listener, err := net.Listen("tcp", cfg.Addr)
	if err != nil {
		return fmt.Errorf("can't listen on %s (is Exams already running, or is another program using that port? Change AMS_ADDR in %s): %w", cfg.Addr, cfg.File, err)
	}

	errs := make(chan error, 1)
	go func() { errs <- srv.Serve(listener) }()
	slog.Info("ams listening", "addr", cfg.Addr, "url", cfg.BaseURL)
	local := localURL(cfg.Addr)
	banner(cfg, local)
	if first && ownConsole() && os.Getenv("AMS_OPEN_BROWSER") != "0" {
		openBrowser(local)
	}

	shutdown := func() error {
		stopCtx, stopCancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer stopCancel()
		return srv.Shutdown(stopCtx)
	}
	select {
	case err := <-errs:
		if !errors.Is(err, http.ErrServerClosed) {
			return err
		}
	case <-restart:
		if err := shutdown(); err != nil {
			return err
		}
		return errRestart
	case <-ctx.Done():
		slog.Info("shutting down")
		return shutdown()
	}
	return nil
}

// localURL is the address to open on this computer.
func localURL(addr string) string {
	_, port, err := net.SplitHostPort(addr)
	if err != nil || port == "" {
		port = "8080"
	}
	if port == "80" {
		return "http://localhost"
	}
	return "http://localhost:" + port
}

func banner(cfg config.Config, local string) {
	var b strings.Builder
	b.WriteString("\n  Exams is running.\n\n")
	fmt.Fprintf(&b, "  Open %s in your browser.\n", local)
	if cfg.BaseURL != local {
		fmt.Fprintf(&b, "  Students and staff on other computers open %s\n", cfg.BaseURL)
	}
	fmt.Fprintf(&b, "\n  Settings: %s\n", cfg.File)
	if ownConsole() {
		b.WriteString("  Close this window to stop Exams.\n")
	} else {
		b.WriteString("  Press Ctrl+C to stop.\n")
	}
	fmt.Fprintln(os.Stdout, b.String())
}

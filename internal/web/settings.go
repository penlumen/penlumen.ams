package web

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"golang.org/x/oauth2"

	"github.com/penlumen/penlumen.ams/internal/auth"
	"github.com/penlumen/penlumen.ams/internal/config"
	"github.com/penlumen/penlumen.ams/internal/sms"
)

// Settings lets the setup and Settings pages change AMS's settings file:
// its address, time zone and the connection to Penlumen School. Saving
// writes the file and restarts AMS in place with it.
type Settings struct {
	// Config is what AMS is running with now.
	Config config.Config
	// Restart applies the saved settings.
	Restart func()
}

// Common time zones for the picker; any IANA name is accepted.
var timezones = []string{
	"Africa/Lagos", "Africa/Accra", "Africa/Abidjan", "Africa/Johannesburg", "Africa/Nairobi",
	"Africa/Cairo", "Africa/Casablanca", "Africa/Kampala", "Africa/Dar_es_Salaam", "Africa/Kigali",
	"Europe/London", "America/New_York", "Asia/Dubai", "UTC",
}

// isLocal is true for a request made on the computer running AMS, not
// through a proxy. Penlumen School's secrets can only be typed in from
// there until an administrator exists.
func isLocal(r *http.Request) bool {
	ip := net.ParseIP(clientIP(r))
	if ip == nil || !ip.IsLoopback() {
		return false
	}
	for _, h := range []string{"X-Forwarded-For", "X-Forwarded-Host", "X-Real-Ip", "Forwarded"} {
		if r.Header.Get(h) != "" {
			return false
		}
	}
	host := r.Host
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	host = strings.Trim(host, "[]")
	if host == "localhost" {
		return true
	}
	hostIP := net.ParseIP(host)
	return hostIP != nil && hostIP.IsLoopback()
}

// apiAddress is the address of Penlumen School's API shown in forms:
// the issuer without its /api/v1/oidc.
func apiAddress(issuer string) string {
	return strings.TrimSuffix(strings.TrimSuffix(issuer, "/oidc"), "/api/v1")
}

// issuerFrom turns the API address typed in (with or without its path)
// into the OpenID Connect issuer.
func issuerFrom(address string) (string, error) {
	address = strings.TrimRight(strings.TrimSpace(address), "/")
	if address == "" {
		return "", errors.New("Give Penlumen School's API address.")
	}
	if !strings.Contains(address, "://") {
		address = "https://" + address
	}
	u, err := url.Parse(address)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return "", errors.New("That isn't a web address, e.g. https://api.yourschool.com")
	}
	switch {
	case strings.HasSuffix(address, "/oidc"):
		return address, nil
	case strings.HasSuffix(address, "/api/v1"):
		return address + "/oidc", nil
	default:
		return address + "/api/v1/oidc", nil
	}
}

func webAddress(v, what string) (string, error) {
	v = strings.TrimRight(strings.TrimSpace(v), "/")
	u, err := url.Parse(v)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.Path != "" {
		return "", fmt.Errorf("Give %s as http:// or https:// and the host, e.g. http://192.168.1.10:8080", what)
	}
	return v, nil
}

// testConnection checks the client ID and secret with Penlumen School and
// gives the school's name.
func testConnection(ctx context.Context, issuer, clientID, secret string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	school, err := sms.New(issuer, clientID, secret).School(ctx)
	if err == nil {
		return school.Name, nil
	}
	slog.Warn("testing penlumen school", "issuer", issuer, "error", err)
	var tokenErr *oauth2.RetrieveError
	var apiErr *sms.APIError
	switch {
	case errors.As(err, &tokenErr) && tokenErr.Response != nil && tokenErr.Response.StatusCode == http.StatusNotFound:
		return "", fmt.Errorf("%s doesn't look like Penlumen School's API. Check the address.", apiAddress(issuer))
	case errors.As(err, &tokenErr):
		return "", errors.New("Penlumen School didn't accept that client ID and secret. Copy them again from School Settings › Integrations.")
	case errors.As(err, &apiErr) && apiErr.Status == http.StatusForbidden:
		return "", errors.New("The client ID and secret work, but this integration can't read the school. Give it the school.read and results.write scopes in Integrations.")
	case errors.As(err, &apiErr) && apiErr.Status == http.StatusNotFound:
		return "", fmt.Errorf("%s doesn't look like Penlumen School's API. Check the address.", apiAddress(issuer))
	case errors.As(err, &apiErr):
		return "", fmt.Errorf("Penlumen School said: %s", apiErr.Message)
	case errors.Is(err, context.DeadlineExceeded):
		return "", fmt.Errorf("%s didn't answer in time. Check the address, and that this computer is online.", apiAddress(issuer))
	default:
		return "", fmt.Errorf("Couldn't reach %s. Check the address, and that this computer is online.", apiAddress(issuer))
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

type testResult struct {
	OK      bool   `json:"ok"`
	Message string `json:"message"`
}

func testAnswer(w http.ResponseWriter, r *http.Request, apiAddr, clientID, secret string) {
	issuer, err := issuerFrom(apiAddr)
	if err == nil && (clientID == "" || secret == "") {
		err = errors.New("Give the client ID and the client secret.")
	}
	if err != nil {
		writeJSON(w, http.StatusOK, testResult{Message: err.Error()})
		return
	}
	name, err := testConnection(r.Context(), issuer, clientID, secret)
	if err != nil {
		writeJSON(w, http.StatusOK, testResult{Message: err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, testResult{OK: true, Message: "Connected to " + name + "."})
}

// applySettings saves changes to the settings file and shows a page that
// waits for AMS to come back with them.
func (s *Server) applySettings(w http.ResponseWriter, r *http.Request, changes map[string]string, port, then string) {
	if err := config.Save(s.settings.Config.File, changes); err != nil {
		s.fail(w, r, http.StatusInternalServerError, "Couldn't save the settings file "+s.settings.Config.File+": "+err.Error())
		return
	}
	keys := make([]string, 0, len(changes))
	for k := range changes {
		keys = append(keys, k)
	}
	slog.Info("settings saved", "file", s.settings.Config.File, "changed", keys)
	restart := s.settings.Restart
	go func() {
		time.Sleep(500 * time.Millisecond) // let this page reach the browser first
		restart()
	}()
	s.render(w, r, http.StatusOK, "restarting.html", map[string]any{"Port": port, "Then": then})
}

// ---------- First run: connecting Penlumen School ----------

// setupAllowed says whether this request may set up Penlumen School from
// the setup page: only before anyone can sign in, and only on the
// computer running AMS.
func (s *Server) setupAllowed(w http.ResponseWriter, r *http.Request) bool {
	switch {
	case !s.needsSetup(r.Context()):
		http.Redirect(w, r, "/login", http.StatusSeeOther)
	case s.settings == nil:
		s.fail(w, r, http.StatusNotFound, "Settings can't be changed here.")
	case !isLocal(r):
		s.fail(w, r, http.StatusForbidden, "Connect Penlumen School from the computer running Exams: open "+localAddress(s.settings.Config)+"/setup there.")
	default:
		return true
	}
	return false
}

func localAddress(c config.Config) string {
	_, port, _ := net.SplitHostPort(c.Addr)
	if port == "" || port == "80" {
		return "http://localhost"
	}
	return "http://localhost:" + port
}

func (s *Server) setupTest(w http.ResponseWriter, r *http.Request) {
	if !s.needsSetup(r.Context()) || s.settings == nil || !isLocal(r) {
		writeJSON(w, http.StatusForbidden, testResult{Message: "Not allowed."})
		return
	}
	testAnswer(w, r, r.PostFormValue("api"), strings.TrimSpace(r.PostFormValue("client_id")), strings.TrimSpace(r.PostFormValue("client_secret")))
}

func (s *Server) setupConnect(w http.ResponseWriter, r *http.Request) {
	if !s.setupAllowed(w, r) {
		return
	}
	cfg := s.settings.Config
	f := map[string]string{}
	for _, k := range []string{"api", "portal_url", "client_id", "client_secret", "webhook_secret"} {
		f[k] = strings.TrimSpace(r.PostFormValue(k))
	}
	errs := map[string]string{}
	issuer, err := issuerFrom(f["api"])
	if err != nil {
		errs["api"] = err.Error()
	}
	portal := ""
	if f["portal_url"] != "" {
		if portal, err = webAddress(f["portal_url"], "the portal's address"); err != nil {
			errs["portal_url"] = err.Error()
		}
	}
	if f["client_id"] == "" {
		errs["client_id"] = "Give the client ID."
	}
	if f["client_secret"] == "" {
		errs["client_secret"] = "Give the client secret."
	}
	if len(errs) == 0 {
		if _, err := testConnection(r.Context(), issuer, f["client_id"], f["client_secret"]); err != nil {
			errs["connection"] = err.Error()
		}
	}
	if len(errs) > 0 {
		f["client_secret"] = "" // never sent back
		s.render(w, r, http.StatusUnprocessableEntity, "setup.html", s.setupData(r, "connect", f, errs))
		return
	}
	changes := map[string]string{
		"SMS_ISSUER": issuer, "SMS_CLIENT_ID": f["client_id"], "SMS_CLIENT_SECRET": f["client_secret"],
		"SMS_WEBHOOK_SECRET": f["webhook_secret"],
	}
	if portal != "" {
		changes["SMS_PORTAL_URL"] = portal
	}
	for k := range changes {
		if cfg.FromEnvironment[k] {
			delete(changes, k)
		}
	}
	s.applySettings(w, r, changes, "", "/login")
}

// setupData is what setup.html shows: the two ways to run AMS.
func (s *Server) setupData(r *http.Request, mode string, f, errs map[string]string) map[string]any {
	d := map[string]any{"Mode": mode, "F": f, "Errors": errs, "Local": isLocal(r), "CanConnect": s.settings != nil}
	if s.settings != nil {
		d["LocalAddress"] = localAddress(s.settings.Config)
		d["BaseURL"] = s.settings.Config.BaseURL
	}
	return d
}

// ---------- Settings page (administrators) ----------

type settingsField struct {
	Key   string // in the settings file
	Value string
	// Locked: set by an environment variable, which wins over the file.
	Locked bool
}

func (s *Server) settingsData(f, errs map[string]string) map[string]any {
	c := s.settings.Config
	_, port, _ := net.SplitHostPort(c.Addr)
	if f == nil {
		f = map[string]string{
			"port": port, "base_url": c.BaseURL, "timezone": c.Timezone,
			"client_id": c.ClientID, "portal_url": c.PortalURL,
		}
		if c.Connected() {
			f["api"] = apiAddress(c.SMSIssuer)
		}
	}
	locked := map[string]bool{
		"port": c.FromEnvironment["AMS_ADDR"], "base_url": c.FromEnvironment["AMS_BASE_URL"],
		"timezone": c.FromEnvironment["AMS_TIMEZONE"],
		"connection": c.FromEnvironment["SMS_ISSUER"] || c.FromEnvironment["SMS_CLIENT_ID"] ||
			c.FromEnvironment["SMS_CLIENT_SECRET"],
		"portal_url":     c.FromEnvironment["SMS_PORTAL_URL"],
		"webhook_secret": c.FromEnvironment["SMS_WEBHOOK_SECRET"],
	}
	return map[string]any{
		"Section": "settings", "F": f, "Errors": errs, "Locked": locked,
		"Connected": c.Connected(), "HasWebhookSecret": c.WebhookSecret != "",
		"File": c.File, "Timezones": timezones, "BaseURL": c.BaseURL,
	}
}

func (s *Server) settingsPage(w http.ResponseWriter, r *http.Request, _ *auth.User) {
	if s.settings == nil {
		s.fail(w, r, http.StatusNotFound, "Settings can't be changed here.")
		return
	}
	d := s.settingsData(nil, map[string]string{})
	d["Notice"] = r.URL.Query().Get("notice")
	s.render(w, r, http.StatusOK, "settings.html", d)
}

func (s *Server) testSettings(w http.ResponseWriter, r *http.Request, _ *auth.User) {
	if s.settings == nil {
		writeJSON(w, http.StatusNotFound, testResult{Message: "Settings can't be changed here."})
		return
	}
	id := strings.TrimSpace(r.PostFormValue("client_id"))
	secret := strings.TrimSpace(r.PostFormValue("client_secret"))
	if secret == "" && id == s.settings.Config.ClientID {
		secret = s.settings.Config.ClientSecret // kept unless a new one is typed
	}
	testAnswer(w, r, r.PostFormValue("api"), id, secret)
}

func (s *Server) saveSettings(w http.ResponseWriter, r *http.Request, _ *auth.User) {
	if s.settings == nil {
		s.fail(w, r, http.StatusNotFound, "Settings can't be changed here.")
		return
	}
	c := s.settings.Config
	f := map[string]string{}
	for _, k := range []string{"port", "base_url", "timezone", "api", "portal_url", "client_id", "client_secret", "webhook_secret"} {
		f[k] = strings.TrimSpace(r.PostFormValue(k))
	}
	disconnect := r.PostFormValue("action") == "disconnect"
	errs := map[string]string{}
	changes := map[string]string{}
	set := func(key, value, current string) {
		if value != current && !c.FromEnvironment[key] {
			changes[key] = value
		}
	}

	// Where AMS listens and is opened.
	host, oldPort, _ := net.SplitHostPort(c.Addr)
	port, err := strconv.Atoi(f["port"])
	if err != nil || port < 1 || port > 65535 {
		errs["port"] = "Give a port number from 1 to 65535, e.g. 8080."
	}
	base, err := webAddress(f["base_url"], "the address")
	if err != nil {
		errs["base_url"] = err.Error()
	}
	newPort := ""
	if len(errs) == 0 && strconv.Itoa(port) != oldPort {
		newPort = strconv.Itoa(port)
		set("AMS_ADDR", net.JoinHostPort(host, newPort), c.Addr)
		// http://localhost:8080 follows the port, unless it was changed too.
		if base == c.BaseURL {
			if u, err := url.Parse(base); err == nil && u.Port() == oldPort {
				u.Host = net.JoinHostPort(u.Hostname(), newPort)
				base = u.String()
			}
		}
	}
	if base != "" {
		set("AMS_BASE_URL", base, c.BaseURL)
	}
	if _, err := time.LoadLocation(f["timezone"]); err != nil || f["timezone"] == "" {
		errs["timezone"] = "Pick a time zone, e.g. Africa/Lagos."
	} else {
		set("AMS_TIMEZONE", f["timezone"], c.Timezone)
	}

	// Penlumen School.
	switch {
	case disconnect:
		if ok, err := s.store.HasAdminLogin(r.Context()); err != nil || !ok {
			errs["connection"] = "Give yourself an Exams password on the People page first, so you can still sign in once Penlumen School is disconnected."
			break
		}
		set("SMS_CLIENT_ID", "", c.ClientID)
		set("SMS_CLIENT_SECRET", "", c.ClientSecret)
		set("SMS_WEBHOOK_SECRET", "", c.WebhookSecret)
	case f["client_id"] == "" && c.Connected():
		errs["client_id"] = "Give the client ID, or use Disconnect to run Exams on its own."
	case f["client_id"] != "" || f["api"] != "" || f["client_secret"] != "":
		issuer, err := issuerFrom(f["api"])
		if err != nil {
			errs["api"] = err.Error()
		}
		secret := f["client_secret"]
		if secret == "" && f["client_id"] == c.ClientID {
			secret = c.ClientSecret
		}
		if f["client_id"] == "" {
			errs["client_id"] = "Give the client ID."
		} else if secret == "" {
			errs["client_secret"] = "Give the client secret."
		}
		changed := issuer != c.SMSIssuer || f["client_id"] != c.ClientID || secret != c.ClientSecret
		if len(errs) == 0 && changed {
			if _, err := testConnection(r.Context(), issuer, f["client_id"], secret); err != nil {
				errs["connection"] = err.Error()
			}
		}
		if len(errs) == 0 {
			set("SMS_ISSUER", issuer, c.SMSIssuer)
			set("SMS_CLIENT_ID", f["client_id"], c.ClientID)
			set("SMS_CLIENT_SECRET", secret, c.ClientSecret)
		}
	}
	if !disconnect {
		if f["webhook_secret"] != "" {
			set("SMS_WEBHOOK_SECRET", f["webhook_secret"], c.WebhookSecret)
		}
		if f["portal_url"] != "" {
			if portal, err := webAddress(f["portal_url"], "the portal's address"); err != nil {
				errs["portal_url"] = err.Error()
			} else {
				set("SMS_PORTAL_URL", portal, c.PortalURL)
			}
		}
	}

	if len(errs) > 0 {
		f["client_secret"], f["webhook_secret"] = "", ""
		s.render(w, r, http.StatusUnprocessableEntity, "settings.html", s.settingsData(f, errs))
		return
	}
	if len(changes) == 0 {
		back(w, r, "/settings", "Nothing changed.")
		return
	}
	s.applySettings(w, r, changes, newPort, "/settings?notice="+url.QueryEscape("Settings saved."))
}

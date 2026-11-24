// Package web holds the HTTP routes and the pages people see. Pages are
// rendered on the server with html/template and styled with Tailwind; the
// exam screen adds a little JavaScript (static/exam.js) for autosave and the
// timer, and the live monitor refreshes itself.
package web

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"html/template"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/penlumen/penlumen.ams/internal/auth"
	"github.com/penlumen/penlumen.ams/internal/db"
	"github.com/penlumen/penlumen.ams/internal/directory"
	"github.com/penlumen/penlumen.ams/internal/richtext"
	"github.com/penlumen/penlumen.ams/internal/schoolsync"
	"github.com/penlumen/penlumen.ams/internal/sms"
	"github.com/penlumen/penlumen.ams/internal/store"
	"github.com/penlumen/penlumen.ams/internal/webhook"
)

//go:embed templates/*.html
var templateFS embed.FS

//go:embed static
var staticFS embed.FS

type Options struct {
	DB   *db.DB
	Auth *auth.Auth
	// School is Penlumen School's API, and Sync keeps AMS's copy of the
	// school in step with it. Both nil when AMS runs on its own.
	School  *sms.Client
	Sync    *schoolsync.Syncer
	BaseURL string
	// PortalURL is Penlumen School's web app, linked from the header.
	PortalURL string
	// WebhookSecret verifies Penlumen School's webhooks.
	WebhookSecret string
	// Location is the school's time zone.
	Location *time.Location
	// Now is the clock; tests replace it. Defaults to time.Now.
	Now func() time.Time
}

type Server struct {
	db        *db.DB
	store     *store.Store
	auth      *auth.Auth
	school    *sms.Client
	sync      *schoolsync.Syncer
	dir       *directory.Directory
	logins    *loginLimiter
	hooks     *webhook.Handler
	origin    string
	portalURL string
	loc       *time.Location
	now       func() time.Time
	pages     map[string]*template.Template
}

func New(o Options) *Server {
	s := &Server{
		db:        o.DB,
		store:     store.New(o.DB),
		auth:      o.Auth,
		school:    o.School,
		sync:      o.Sync,
		logins:    newLoginLimiter(),
		portalURL: o.PortalURL,
		loc:       o.Location,
		now:       o.Now,
	}
	s.dir = directory.New(s.store, func() {
		if s.sync != nil {
			s.sync.Now()
		}
	})
	if s.auth != nil {
		s.auth.OnSignIn = s.recordPortalSignIn
	}
	if s.loc == nil {
		s.loc = time.UTC
	}
	if s.now == nil {
		s.now = time.Now
	}
	s.hooks = &webhook.Handler{Secret: o.WebhookSecret, Cache: s.dir}
	if u, err := url.Parse(o.BaseURL); err == nil {
		s.origin = u.Scheme + "://" + u.Host
	}
	s.pages = parsePages(s.funcs())
	return s
}

// parsePages gives each page its own template set (layout.html plus the
// page), so pages can't clash over block names.
func parsePages(funcs template.FuncMap) map[string]*template.Template {
	entries, err := templateFS.ReadDir("templates")
	if err != nil {
		panic(err)
	}
	pages := map[string]*template.Template{}
	for _, e := range entries {
		if e.Name() == "layout.html" {
			continue
		}
		pages[e.Name()] = template.Must(template.New(e.Name()).Funcs(funcs).
			ParseFS(templateFS, "templates/layout.html", "templates/"+e.Name()))
	}
	return pages
}

func (s *Server) Routes() http.Handler {
	mux := http.NewServeMux()
	mux.Handle("GET /static/", http.FileServerFS(staticFS))
	mux.HandleFunc("GET /healthz", s.health)
	mux.Handle("POST /webhooks/ams", s.hooks)

	// Signing in: an AMS password (staff) or PIN (students), or through
	// Penlumen School when it's set up. A new stand-alone install asks for
	// its first administrator.
	mux.HandleFunc("GET /login", s.loginPage)
	mux.HandleFunc("POST /login", s.login)
	mux.HandleFunc("GET /login/portal", s.auth.Login)
	mux.HandleFunc("GET /auth/callback", s.auth.Callback)
	mux.HandleFunc("POST /logout", s.auth.Logout)
	mux.HandleFunc("GET /setup", s.setupPage)
	mux.HandleFunc("POST /setup", s.setup)

	// Administrators: students, staff, classes and terms.
	mux.HandleFunc("GET /people", s.admin(s.peoplePage))
	mux.HandleFunc("GET /people/new", s.admin(s.personForm))
	mux.HandleFunc("POST /people", s.admin(s.savePerson))
	mux.HandleFunc("GET /people/{id}", s.admin(s.personForm))
	mux.HandleFunc("POST /people/{id}", s.admin(s.savePerson))
	mux.HandleFunc("POST /people/{id}/password", s.admin(s.setPassword))
	mux.HandleFunc("POST /people/{id}/delete", s.admin(s.removePerson))
	mux.HandleFunc("POST /classes", s.admin(s.saveClass))
	mux.HandleFunc("POST /classes/{id}/delete", s.admin(s.removeClass))
	mux.HandleFunc("POST /classes/{id}/teachers", s.admin(s.saveClassTeachers))
	mux.HandleFunc("POST /terms", s.admin(s.saveTerm))
	mux.HandleFunc("POST /terms/{id}/delete", s.admin(s.removeTerm))
	mux.HandleFunc("POST /sync", s.admin(s.syncNow))

	mux.HandleFunc("GET /{$}", s.home)

	// Safe Exam Browser: an exam's settings file, and where SEB closes.
	mux.HandleFunc("GET /seb/quit", s.sebQuit)
	mux.HandleFunc("GET /seb/{file}", s.sebConfig)

	// Teachers and administrators: building assessments.
	mux.HandleFunc("GET /assessments/new", s.staff(s.newAssessment))
	mux.HandleFunc("POST /assessments", s.staff(s.createAssessment))
	mux.HandleFunc("GET /assessments/{id}", s.staff(s.editAssessment))
	mux.HandleFunc("GET /assessments/{id}/details", s.staff(s.detailsForm))
	mux.HandleFunc("POST /assessments/{id}/details", s.staff(s.saveDetails))
	mux.HandleFunc("POST /assessments/{id}/questions", s.staff(s.addQuestion))
	mux.HandleFunc("POST /assessments/{id}/questions/{qid}", s.staff(s.updateQuestion))
	mux.HandleFunc("POST /assessments/{id}/questions/{qid}/delete", s.staff(s.deleteQuestion))
	mux.HandleFunc("POST /assessments/{id}/images", s.staff(s.uploadQuestionImage))
	mux.HandleFunc("POST /assessments/{id}/status", s.staff(s.changeStatus))
	mux.HandleFunc("POST /assessments/{id}/delete", s.staff(s.deleteAssessment))
	mux.HandleFunc("POST /assessments/{id}/import", s.staff(s.importQuestions))
	mux.HandleFunc("GET /assessments/{id}/questions.csv", s.staff(s.exportQuestions))
	mux.HandleFunc("GET /assessments/{id}/submissions", s.staff(s.submissions))
	mux.HandleFunc("GET /assessments/{id}/submissions.csv", s.staff(s.exportSubmissions))
	// Old links from before Results was renamed Submissions.
	mux.HandleFunc("GET /assessments/{id}/results", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/assessments/"+r.PathValue("id")+"/submissions", http.StatusMovedPermanently)
	})
	mux.HandleFunc("POST /assessments/{id}/publish", s.staff(s.publish))
	mux.HandleFunc("POST /assessments/{id}/attempts/{attempt}/{action}", s.staff(s.changeAttempt))

	// Teachers: running and marking.
	mux.HandleFunc("GET /monitor", s.staff(s.monitorList))
	mux.HandleFunc("GET /monitor/{id}", s.staff(s.monitor))
	mux.HandleFunc("GET /monitor/{id}/live", s.staff(s.monitorLive))
	mux.HandleFunc("POST /monitor/{id}/time", s.staff(s.addTime))
	mux.HandleFunc("GET /monitor/{id}/students/{attempt}", s.staff(s.evidence))
	mux.HandleFunc("GET /snapshots/{id}", s.staff(s.snapshotImage))
	mux.HandleFunc("GET /marking", s.staff(s.markingList))
	mux.HandleFunc("GET /marking/{id}", s.staff(s.marking))
	mux.HandleFunc("POST /marking/{id}/{attempt}", s.staff(s.saveMarks))
	mux.HandleFunc("GET /reports", s.staff(s.reportsList))
	mux.HandleFunc("GET /reports/{id}", s.staff(s.report))

	mux.HandleFunc("GET /questions-template.csv", s.staff(s.csvTemplate))

	// Students.
	mux.HandleFunc("GET /take/{id}", s.student(s.takeAssessment))
	mux.HandleFunc("POST /take/{id}/start", s.student(s.startAttempt))
	mux.HandleFunc("POST /attempts/{id}/answers", s.student(s.saveAnswer))
	mux.HandleFunc("POST /attempts/{id}/ping", s.student(s.ping))
	mux.HandleFunc("POST /attempts/{id}/snapshot", s.student(s.snapshot))
	mux.HandleFunc("POST /attempts/{id}/images", s.student(s.uploadAnswerImage))
	mux.HandleFunc("GET /images/{id}", s.requireUser(func(*auth.User) bool { return true }, s.image))
	mux.HandleFunc("POST /attempts/{id}/submit", s.student(s.submitAttempt))

	return s.sameOrigin(logRequests(mux))
}

func (s *Server) health(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	if err := s.db.PingContext(ctx); err != nil {
		http.Error(w, "database unavailable", http.StatusServiceUnavailable)
		return
	}
	w.Write([]byte("ok"))
}

func (s *Server) home(w http.ResponseWriter, r *http.Request) {
	user := s.currentUser(r)
	switch {
	case user == nil:
		s.loginPage(w, r)
	case user.IsStudent():
		s.studentHome(w, r, user)
	default:
		s.staffHome(w, r, user)
	}
}

// ---------- Guards ----------

type handler func(w http.ResponseWriter, r *http.Request, user *auth.User)

func (s *Server) staff(h handler) http.HandlerFunc {
	return s.requireUser(func(u *auth.User) bool { return u.IsStaff() }, h)
}

func (s *Server) admin(h handler) http.HandlerFunc {
	return s.requireUser(func(u *auth.User) bool { return u.IsAdmin() }, h)
}

func (s *Server) student(h handler) http.HandlerFunc {
	return s.requireUser(func(u *auth.User) bool { return u.IsStudent() }, h)
}

func (s *Server) requireUser(allowed func(*auth.User) bool, h handler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		user := s.currentUser(r)
		if user == nil {
			if r.Method == http.MethodGet {
				http.Redirect(w, r, "/login?return_to="+url.QueryEscape(r.URL.RequestURI()), http.StatusFound)
				return
			}
			s.fail(w, r, http.StatusUnauthorized, "You've been signed out. Sign in again and retry.")
			return
		}
		if !allowed(user) {
			s.fail(w, r, http.StatusForbidden, "This page isn't for your account.")
			return
		}
		h(w, r, user)
	}
}

// sameOrigin refuses form posts and saves sent from other sites. Session
// cookies are SameSite=Lax as well; this is a second line of defence.
// Webhooks come from a server, with no Origin header, and are signed.
func (s *Server) sameOrigin(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			if o := r.Header.Get("Origin"); o != "" && o != s.origin {
				http.Error(w, "Cross-site request refused", http.StatusForbidden)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

// ---------- Rendering ----------

// pageData adds what every page needs: the user, the school's name and its
// colours, and the teacher menu's current entry.
func (s *Server) pageData(r *http.Request, data map[string]any) map[string]any {
	if data == nil {
		data = map[string]any{}
	}
	data["User"] = s.currentUser(r)
	data["PortalURL"] = s.portalURL
	if _, ok := data["Section"]; !ok {
		data["Section"] = "exams"
	}
	{ // the school's name, signed in or not
		if school, err := s.dir.School(r.Context()); err == nil {
			data["SchoolName"] = school.Name
		}
	}
	return data
}

func (s *Server) render(w http.ResponseWriter, r *http.Request, status int, page string, data map[string]any) {
	s.renderBlock(w, r, status, page, page, data)
}

// renderBlock renders one named template from a page's set (the live
// monitor refreshes just its table this way).
func (s *Server) renderBlock(w http.ResponseWriter, r *http.Request, status int, page, block string, data map[string]any) {
	t, ok := s.pages[page]
	if !ok {
		panic("no page " + page)
	}
	data = s.pageData(r, data)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	if err := t.ExecuteTemplate(w, block, data); err != nil {
		slog.Error("render", "page", page, "block", block, "error", err)
	}
}

// fail shows a friendly error page (or JSON to the exam screen's saves).
func (s *Server) fail(w http.ResponseWriter, r *http.Request, status int, message string) {
	if strings.Contains(r.Header.Get("Accept"), "application/json") {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		fmt.Fprintf(w, `{"error":%q}`, message)
		return
	}
	s.render(w, r, status, "error.html", map[string]any{"Status": status, "Message": message})
}

// failErr turns a store or Penlumen School error into a page.
func (s *Server) failErr(w http.ResponseWriter, r *http.Request, err error) {
	var apiErr *sms.APIError
	switch {
	case errors.Is(err, store.ErrNotFound):
		s.fail(w, r, http.StatusNotFound, "That doesn't exist, or was deleted.")
	case errors.Is(err, store.ErrStatus):
		s.fail(w, r, http.StatusConflict, "That can't be changed now. Reload the page to see where things stand.")
	case errors.Is(err, store.ErrClosed):
		s.fail(w, r, http.StatusConflict, "Time is up, or this was already submitted.")
	case errors.As(err, &apiErr):
		slog.Warn("penlumen school", "status", apiErr.Status, "message", apiErr.Message)
		s.fail(w, r, http.StatusBadGateway, "Penlumen School said: "+apiErr.Message)
	default:
		slog.Error("request failed", "path", r.URL.Path, "error", err)
		s.fail(w, r, http.StatusInternalServerError, "Something went wrong. Try again; if it keeps happening, tell your administrator.")
	}
}

// back redirects to path with a message shown at the top of the page.
func back(w http.ResponseWriter, r *http.Request, path, notice string) {
	if notice != "" {
		sep := "?"
		if strings.Contains(path, "?") {
			sep = "&"
		}
		path += sep + "notice=" + url.QueryEscape(notice)
	}
	http.Redirect(w, r, path, http.StatusSeeOther)
}

func (s *Server) funcs() template.FuncMap {
	return template.FuncMap{
		// when: "Mon 24 Nov, 09:00" in the school's time zone.
		"when": func(t time.Time) string { return t.In(s.loc).Format("Mon 2 Jan, 15:04") },
		// clock: "09:00".
		"clock": func(t time.Time) string { return t.In(s.loc).Format("15:04") },
		// formTime: the value a datetime-local input wants.
		"formTime": func(t time.Time) string { return t.In(s.loc).Format(formTime) },
		// num: 3 not 3.00, 2.5 stays 2.5.
		"num":   func(f float64) string { return strconv.FormatFloat(f, 'f', -1, 64) },
		"inc":   func(i int) int { return i + 1 },
		"add":   func(a, b int) int { return a + b },
		"pct":   func(part, whole float64) int { return percent(part, whole) },
		"pctOf": func(part, whole int) int { return percent(float64(part), float64(whole)) },
		"letter": func(i int) string {
			if i < 0 || i > 25 {
				return "?"
			}
			return string(rune('A' + i))
		},
		"kindName": kindName,
		// rich shows formatted text (questions, written answers), cleaned again
		// on the way out; plain is its words only, for one-line summaries.
		"rich":  func(s string) template.HTML { return template.HTML(richtext.Sanitize(s)) },
		"plain": richtext.ToText,
		"deref": func(p *int) int {
			if p == nil {
				return -1
			}
			return *p
		},
		"deref2": func(p *float64) float64 {
			if p == nil {
				return 0
			}
			return *p
		},
		// score: how well a class did on a question, 0 to 100.
		"score": score,
		// themeCSS: the school's brand colours. Each school runs its own app,
		// so one theme serves every page; the directory caches it.
		"themeCSS": func() template.CSS {
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			if school, err := s.dir.School(ctx); err == nil && school.Theme != nil {
				return themeCSS(school.Theme.Primary)
			}
			return themeCSS(DefaultPrimary)
		},
		"isRight": func(q store.Question, option int) bool {
			return q.Answer != nil && *q.Answer == option
		},
		"initials": initials,
		"firstLetter": func(name string) string {
			for _, r := range strings.TrimSpace(name) {
				return strings.ToUpper(string(r))
			}
			return "?"
		},
		"firstName": firstName,
		"greeting": func() string {
			switch h := s.now().In(s.loc).Hour(); {
			case h < 12:
				return "Good morning"
			case h < 17:
				return "Good afternoon"
			}
			return "Good evening"
		},
		"today": func() string { return s.now().In(s.loc).Format("Monday 2 January 2006") },
		// dict: named arguments for a template, (dict "Name" "x" "On" true).
		"list":     func(v ...string) []string { return v },
		"title":    func(s string) string { return strings.ToUpper(s[:1]) + s[1:] },
		"upper":    strings.ToUpper,
		"join":     strings.Join,
		"contains": contains,
		"dict": func(pairs ...any) (map[string]any, error) {
			if len(pairs)%2 != 0 {
				return nil, fmt.Errorf("dict needs pairs, got %d values", len(pairs))
			}
			m := map[string]any{}
			for i := 0; i < len(pairs); i += 2 {
				key, ok := pairs[i].(string)
				if !ok {
					return nil, fmt.Errorf("dict key %v isn't text", pairs[i])
				}
				m[key] = pairs[i+1]
			}
			return m, nil
		},
	}
}

func percent(part, whole float64) int {
	if whole <= 0 {
		return 0
	}
	p := int(part/whole*100 + 0.5)
	return max(0, min(100, p))
}

func kindName(kind string) string {
	switch kind {
	case "ca":
		return "Assessment"
	case "exam":
		return "Examination"
	}
	return kind
}

func initials(name string) string {
	var out []rune
	for _, word := range strings.Fields(name) {
		for _, r := range word {
			out = append(out, r)
			break
		}
		if len(out) == 2 {
			break
		}
	}
	return strings.ToUpper(string(out))
}

func firstName(name string) string {
	if f := strings.Fields(name); len(f) > 0 {
		return f[0]
	}
	return ""
}

func logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		next.ServeHTTP(w, r)
		slog.Debug("request", "method", r.Method, "path", r.URL.Path, "took", time.Since(start))
	})
}

// contains reports whether list has v.
func contains(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}

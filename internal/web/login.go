package web

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"log/slog"
	"math/big"
	"net"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/bcrypt"

	"github.com/penlumen/penlumen.ams/internal/auth"
	"github.com/penlumen/penlumen.ams/internal/directory"
	"github.com/penlumen/penlumen.ams/internal/schoolsync"
	"github.com/penlumen/penlumen.ams/internal/sms"
	"github.com/penlumen/penlumen.ams/internal/store"
)

// Signing in with AMS's own accounts: staff with their email and a
// password, students with their reg number and a PIN, both set by an
// administrator on the People page. This works whether or not Penlumen
// School is up; "Sign in with your school portal" is offered as well when
// it's set up.

const (
	MinPassword = 8
	// Students' PINs are 4 to 8 digits; new ones are 6.
	pinDigits = 6
)

var pinPattern = regexp.MustCompile(`^[0-9]{4,8}$`)

// currentUser is the signed-in person, as long as they can still sign in
// (an administrator may have removed them, or Penlumen School deactivated
// them, since the session started).
func (s *Server) currentUser(r *http.Request) *auth.User {
	u := s.auth.CurrentUser(r)
	if u == nil {
		return nil
	}
	p, err := s.store.Person(r.Context(), u.Sub)
	if err != nil || !p.Active() {
		return nil
	}
	return u
}

// userFor is the session for a person.
func (s *Server) userFor(ctx context.Context, p store.Person) (auth.User, error) {
	school, err := s.dir.School(ctx)
	if err != nil {
		return auth.User{}, err
	}
	u := auth.User{Sub: p.UUID, Name: p.Name, Email: p.Email, Role: p.Role, SchoolUUID: school.UUID,
		BranchUUIDs: p.BranchUUIDs, RegNumber: strings.ToUpper(p.RegNumber)}
	// Administrators added in AMS, and staff given no branch, see all of them.
	if (p.Role == "admin" && p.Source == store.SourceLocal) || (!p.IsStudent() && len(p.BranchUUIDs) == 0) {
		u.BranchUUIDs = nil
		for _, b := range school.Branches {
			u.BranchUUIDs = append(u.BranchUUIDs, b.UUID)
		}
	}
	if p.IsStudent() && p.ClassUUID != "" {
		classes, err := s.dir.Classes(ctx)
		if err != nil {
			return u, err
		}
		for _, c := range classes {
			if c.UUID == p.ClassUUID {
				u.Class = &auth.ClassRef{UUID: c.UUID, Name: c.Name}
				u.BranchUUIDs = []string{c.BranchUUID}
			}
		}
	}
	return u, nil
}

// recordPortalSignIn runs when someone signs in through Penlumen School: it
// keeps them in AMS (so they're known when Penlumen School is down), and
// refuses anyone removed there.
func (s *Server) recordPortalSignIn(ctx context.Context, u *auth.User) error {
	p := store.Person{UUID: u.Sub, Role: u.Role, Name: u.Name, Email: u.Email, RegNumber: u.RegNumber,
		BranchUUIDs: u.BranchUUIDs, Status: "active"}
	if u.Role != "admin" && u.Role != "student" {
		p.Role = "staff"
	}
	if u.Class != nil {
		p.ClassUUID = u.Class.UUID
	}
	if err := s.store.SyncPerson(ctx, p); err != nil {
		return err
	}
	// The school too, if a sync hasn't saved it yet.
	if _, err := s.dir.School(ctx); errors.Is(err, directory.ErrNoSchool) && s.sync != nil {
		s.sync.Now()
	}
	return nil
}

type loginData struct {
	Student  bool
	Name     string
	ReturnTo string
	Error    string
	Portal   bool
	Setup    bool
}

func (s *Server) loginPage(w http.ResponseWriter, r *http.Request) {
	if s.currentUser(r) != nil {
		http.Redirect(w, r, auth.SafeReturn(r.URL.Query().Get("return_to")), http.StatusFound)
		return
	}
	if s.needsSetup(r.Context()) {
		http.Redirect(w, r, "/setup", http.StatusFound)
		return
	}
	s.renderLogin(w, r, http.StatusOK, loginData{
		Student:  r.URL.Query().Get("as") == "student",
		ReturnTo: auth.SafeReturn(r.URL.Query().Get("return_to")),
	})
}

func (s *Server) renderLogin(w http.ResponseWriter, r *http.Request, status int, d loginData) {
	d.Portal = s.auth.PortalEnabled()
	s.render(w, r, status, "login.html", map[string]any{"L": d})
}

func (s *Server) login(w http.ResponseWriter, r *http.Request) {
	d := loginData{
		Student:  r.PostFormValue("as") == "student",
		Name:     strings.TrimSpace(r.PostFormValue("name")),
		ReturnTo: auth.SafeReturn(r.PostFormValue("return_to")),
	}
	secret := r.PostFormValue("password")
	key := clientIP(r) + "|" + store.SignInName(d.Name)
	if !s.logins.allowed(key) {
		d.Error = "Too many tries. Wait 15 minutes, or ask your school's administrator."
		s.renderLogin(w, r, http.StatusTooManyRequests, d)
		return
	}
	p, hash, err := s.store.PersonFor(r.Context(), d.Student, d.Name)
	ok := err == nil && p.Active() && hash != "" && p.IsStudent() == d.Student &&
		bcrypt.CompareHashAndPassword([]byte(hash), []byte(secret)) == nil
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		s.failErr(w, r, err)
		return
	}
	if !ok {
		s.logins.failed(key)
		if d.Student {
			d.Error = "That reg number and PIN don't match. Check them, or ask your teacher for your PIN."
		} else {
			d.Error = "That email and password don't match. An administrator can set a new password."
		}
		s.renderLogin(w, r, http.StatusUnauthorized, d)
		return
	}
	user, err := s.userFor(r.Context(), p)
	if err != nil {
		s.failErr(w, r, err)
		return
	}
	s.logins.reset(key)
	if err := s.auth.SignIn(w, user); err != nil {
		s.failErr(w, r, err)
		return
	}
	slog.Info("signed in", "person", p.UUID, "role", p.Role, "with", "ams")
	http.Redirect(w, r, d.ReturnTo, http.StatusSeeOther)
}

func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// ---------- First administrator ----------

// needsSetup is true for a stand-alone install with no administrator yet.
// (With Penlumen School, administrators sign in through it and set AMS
// passwords on the People page.)
func (s *Server) needsSetup(ctx context.Context) bool {
	if s.auth.PortalEnabled() {
		return false
	}
	ok, err := s.store.HasAdminLogin(ctx)
	return err == nil && !ok
}

func (s *Server) setupPage(w http.ResponseWriter, r *http.Request) {
	if !s.needsSetup(r.Context()) {
		http.Redirect(w, r, "/login", http.StatusFound)
		return
	}
	s.render(w, r, http.StatusOK, "setup.html", map[string]any{"F": map[string]string{}, "Errors": map[string]string{}})
}

func (s *Server) setup(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if !s.needsSetup(ctx) {
		http.Redirect(w, r, "/login", http.StatusFound)
		return
	}
	f := map[string]string{}
	for _, k := range []string{"school", "name", "email"} {
		f[k] = strings.TrimSpace(r.PostFormValue(k))
	}
	errs := map[string]string{}
	if f["school"] == "" {
		errs["school"] = "Give the school's name."
	}
	if f["name"] == "" {
		errs["name"] = "Give your name."
	}
	if !strings.Contains(f["email"], "@") {
		errs["email"] = "Give your email address."
	}
	password := r.PostFormValue("password")
	if len(password) < MinPassword {
		errs["password"] = fmt.Sprintf("Use at least %d characters.", MinPassword)
	} else if password != r.PostFormValue("confirm") {
		errs["confirm"] = "The two passwords don't match."
	}
	if len(errs) > 0 {
		s.render(w, r, http.StatusUnprocessableEntity, "setup.html", map[string]any{"F": f, "Errors": errs})
		return
	}

	// The school, with one branch, unless a sync already saved one.
	if _, err := s.dir.School(ctx); errors.Is(err, directory.ErrNoSchool) {
		school := sms.School{UUID: newUUID(), Name: f["school"], Slug: schoolsync.StandaloneSlug,
			Branches: []sms.Branch{{UUID: newUUID(), Name: "Main", AssessmentMax: 40, ExaminationMax: 60}}}
		if err := s.store.SetSetting(ctx, directory.SchoolKey, school); err != nil {
			s.failErr(w, r, err)
			return
		}
	}
	hash, err := hashSecret(password)
	if err != nil {
		s.failErr(w, r, err)
		return
	}
	id, err := s.store.AddPerson(ctx, store.Person{Role: "admin", Name: f["name"], Email: f["email"]}, hash)
	if err != nil {
		s.failErr(w, r, err)
		return
	}
	p, err := s.store.Person(ctx, id)
	if err != nil {
		s.failErr(w, r, err)
		return
	}
	user, err := s.userFor(ctx, p)
	if err == nil {
		err = s.auth.SignIn(w, user)
	}
	if err != nil {
		s.failErr(w, r, err)
		return
	}
	slog.Info("first administrator set up", "person", id)
	back(w, r, "/people", "Welcome. Add your classes, terms, staff and students here.")
}

// ---------- Secrets ----------

func hashSecret(secret string) (string, error) {
	b, err := bcrypt.GenerateFromPassword([]byte(secret), bcrypt.DefaultCost)
	return string(b), err
}

// newPIN is a random 6-digit PIN.
func newPIN() string {
	n, _ := rand.Int(rand.Reader, big.NewInt(1_000_000))
	return fmt.Sprintf("%0*d", pinDigits, n.Int64())
}

func newUUID() string {
	var b [16]byte
	rand.Read(b[:])
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:])
}

// ---------- Too many tries ----------

// loginLimiter stops guessing: 5 wrong tries for one account from one
// address lock it there for 15 minutes.
type loginLimiter struct {
	mu    sync.Mutex
	tries map[string][]time.Time
}

const (
	maxTries   = 5
	triesReset = 15 * time.Minute
)

func newLoginLimiter() *loginLimiter { return &loginLimiter{tries: map[string][]time.Time{}} }

func (l *loginLimiter) recent(key string, now time.Time) []time.Time {
	var keep []time.Time
	for _, t := range l.tries[key] {
		if now.Sub(t) < triesReset {
			keep = append(keep, t)
		}
	}
	return keep
}

func (l *loginLimiter) allowed(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.recent(key, time.Now())) < maxTries
}

func (l *loginLimiter) failed(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	l.tries[key] = append(l.recent(key, now), now)
	if len(l.tries) > 10000 { // don't grow without end
		for k := range l.tries {
			if len(l.recent(k, now)) == 0 {
				delete(l.tries, k)
			}
		}
	}
}

func (l *loginLimiter) reset(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.tries, key)
}

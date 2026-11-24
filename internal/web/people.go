package web

import (
	"crypto/rand"
	"errors"
	"fmt"
	"log/slog"
	"math/big"
	"net/http"
	"strings"

	"github.com/penlumen/penlumen.ams/internal/auth"
	"github.com/penlumen/penlumen.ams/internal/sms"
	"github.com/penlumen/penlumen.ams/internal/store"
)

// The People page, for administrators: students, staff, classes and terms.
// Those from Penlumen School are shown as such and change there (AMS only
// sets their password or PIN); the rest are AMS's own, added and removed
// here. Penlumen School wins when it has the same person.

type personRow struct {
	store.Person
	Class string
}

func (s *Server) peoplePage(w http.ResponseWriter, r *http.Request, user *auth.User) {
	ctx := r.Context()
	tab := r.URL.Query().Get("tab")
	switch tab {
	case "staff", "classes", "terms":
	default:
		tab = "students"
	}
	school, err := s.dir.School(ctx)
	if err != nil {
		s.failErr(w, r, err)
		return
	}
	classes, err := s.store.Classes(ctx)
	if err != nil {
		s.failErr(w, r, err)
		return
	}
	names := map[string]string{}
	for _, c := range classes {
		names[c.UUID] = c.Name
	}
	class := r.URL.Query().Get("class")
	staff, err := s.store.People(ctx, false, "")
	if err != nil {
		s.failErr(w, r, err)
		return
	}
	staffNames := map[string]string{}
	var activeStaff []store.Person
	for _, p := range staff {
		staffNames[p.UUID] = p.Name
		if p.Active() {
			activeStaff = append(activeStaff, p)
		}
	}
	data := map[string]any{
		"Tab": tab, "ClassFilter": class, "Classes": classes, "Branches": school.Branches,
		"Staff": activeStaff, "StaffNames": staffNames,
		"BranchNames": branchNames(school.Branches), "Section": "people",
		"Notice": r.URL.Query().Get("notice"), "Portal": s.school != nil,
	}
	if s.sync != nil {
		data["Sync"] = s.sync.Status()
	}
	switch tab {
	case "students", "staff":
		people, err := s.store.People(ctx, tab == "students", class)
		if err != nil {
			s.failErr(w, r, err)
			return
		}
		rows := make([]personRow, len(people))
		for i, p := range people {
			rows[i] = personRow{Person: p, Class: names[p.ClassUUID]}
		}
		data["People"] = rows
	case "terms":
		terms, err := s.store.Terms(ctx)
		if err != nil {
			s.failErr(w, r, err)
			return
		}
		data["Terms"] = terms
	}
	s.render(w, r, http.StatusOK, "people.html", data)
}

func branchNames(bs []sms.Branch) map[string]string {
	out := map[string]string{}
	for _, b := range bs {
		out[b.UUID] = b.Name
	}
	return out
}

// ---------- One person ----------

type personForm struct {
	ID        string
	Role      string
	Source    string
	Name      string
	Email     string
	RegNumber string
	Class     string
	Branches  []string
	Position  string
	HasLogin  bool
	Status    string
}

func (s *Server) renderPerson(w http.ResponseWriter, r *http.Request, status int, f personForm, errs map[string]string, secret string) {
	school, err := s.dir.School(r.Context())
	if err != nil {
		s.failErr(w, r, err)
		return
	}
	classes, err := s.store.Classes(r.Context())
	if err != nil {
		s.failErr(w, r, err)
		return
	}
	s.render(w, r, status, "person_form.html", map[string]any{
		"F": f, "Errors": errs, "Secret": secret, "Classes": classes, "Branches": school.Branches,
		"Section": "people", "Notice": r.URL.Query().Get("notice"),
	})
}

func formForPerson(p store.Person) personForm {
	return personForm{ID: p.UUID, Role: p.Role, Source: p.Source, Name: p.Name, Email: p.Email,
		RegNumber: strings.ToUpper(p.RegNumber), Class: p.ClassUUID, Branches: p.BranchUUIDs,
		Position: p.Position, HasLogin: p.HasPassword, Status: p.Status}
}

func (s *Server) personForm(w http.ResponseWriter, r *http.Request, user *auth.User) {
	id := r.PathValue("id")
	if id == "" {
		role := "student"
		if r.URL.Query().Get("role") == "staff" {
			role = "staff"
		}
		s.renderPerson(w, r, http.StatusOK, personForm{Role: role, Source: store.SourceLocal,
			Class: r.URL.Query().Get("class"), Position: "academic"}, nil, "")
		return
	}
	p, err := s.store.Person(r.Context(), id)
	if err != nil {
		s.failErr(w, r, err)
		return
	}
	s.renderPerson(w, r, http.StatusOK, formForPerson(p), nil, "")
}

// savePerson adds someone, or saves changes to someone added in AMS. A new
// person's PIN or password is shown once.
func (s *Server) savePerson(w http.ResponseWriter, r *http.Request, user *auth.User) {
	ctx := r.Context()
	id := r.PathValue("id")
	if err := r.ParseForm(); err != nil {
		s.fail(w, r, http.StatusBadRequest, "The form couldn't be read.")
		return
	}
	f := personForm{ID: id, Source: store.SourceLocal,
		Role:      r.PostFormValue("role"),
		Name:      strings.TrimSpace(r.PostFormValue("name")),
		Email:     strings.TrimSpace(r.PostFormValue("email")),
		RegNumber: strings.ToUpper(strings.TrimSpace(r.PostFormValue("reg_number"))),
		Class:     r.PostFormValue("class"),
		Branches:  r.PostForm["branch"],
		Position:  r.PostFormValue("position"),
	}
	if f.Position != "administrative" {
		f.Position = "academic"
	}
	if id != "" {
		old, err := s.store.Person(ctx, id)
		if err != nil {
			s.failErr(w, r, err)
			return
		}
		if old.Source != store.SourceLocal {
			s.fail(w, r, http.StatusConflict, "This person comes from Penlumen School: change them there.")
			return
		}
		f.HasLogin, f.Status = old.HasPassword, old.Status
		if old.IsStudent() {
			f.Role = "student"
		} else if f.Role == "student" {
			f.Role = old.Role
		}
	}
	errs := map[string]string{}
	switch f.Role {
	case "student", "staff", "admin":
	default:
		f.Role = "student"
	}
	if f.Name == "" {
		errs["name"] = "Give their name."
	}
	secret := r.PostFormValue("password")
	if f.Role == "student" {
		if f.RegNumber == "" {
			errs["reg_number"] = "Give their reg number: they sign in with it."
		}
		classes, err := s.store.Classes(ctx)
		if err != nil {
			s.failErr(w, r, err)
			return
		}
		found := false
		for _, c := range classes {
			found = found || c.UUID == f.Class
		}
		if !found {
			errs["class"] = "Choose their class."
		}
		f.Email, f.Branches = "", nil
		if secret != "" && !pinPattern.MatchString(secret) {
			errs["password"] = "A PIN is 4 to 8 digits."
		}
	} else {
		if !strings.Contains(f.Email, "@") {
			errs["email"] = "Give their email address: they sign in with it."
		}
		f.RegNumber, f.Class = "", ""
		if secret != "" && len(secret) < MinPassword {
			errs["password"] = fmt.Sprintf("Use at least %d characters.", MinPassword)
		}
		if id == user.Sub && f.Role != "admin" {
			errs["role"] = "You can't take away your own administrator role."
		}
	}
	if len(errs) > 0 {
		s.renderPerson(w, r, http.StatusUnprocessableEntity, f, errs, "")
		return
	}

	p := store.Person{UUID: id, Role: f.Role, Name: f.Name, Email: f.Email, RegNumber: f.RegNumber,
		ClassUUID: f.Class, BranchUUIDs: f.Branches, Position: f.Position}
	if id != "" {
		err := s.store.UpdatePerson(ctx, p)
		if errors.Is(err, store.ErrTaken) {
			s.renderPerson(w, r, http.StatusUnprocessableEntity, f, takenError(f.Role), "")
			return
		}
		if err != nil {
			s.failErr(w, r, err)
			return
		}
		slog.Info("person changed", "person", id, "by", user.Sub)
		back(w, r, "/people?tab="+tabFor(f.Role), f.Name+" saved.")
		return
	}

	if secret == "" {
		secret = newSecret(f.Role)
	}
	hash, err := hashSecret(secret)
	if err != nil {
		s.failErr(w, r, err)
		return
	}
	newID, err := s.store.AddPerson(ctx, p, hash)
	if errors.Is(err, store.ErrTaken) {
		s.renderPerson(w, r, http.StatusUnprocessableEntity, f, takenError(f.Role), "")
		return
	}
	if err != nil {
		s.failErr(w, r, err)
		return
	}
	slog.Info("person added", "person", newID, "role", f.Role, "by", user.Sub)
	added, err := s.store.Person(ctx, newID)
	if err != nil {
		s.failErr(w, r, err)
		return
	}
	s.renderPerson(w, r, http.StatusCreated, formForPerson(added), nil, secret)
}

func takenError(role string) map[string]string {
	if role == "student" {
		return map[string]string{"reg_number": "Another student has this reg number."}
	}
	return map[string]string{"email": "Someone else signs in with this email."}
}

func tabFor(role string) string {
	if role == "student" {
		return "students"
	}
	return "staff"
}

// newSecret is a new student's PIN, or a staff member's first password.
func newSecret(role string) string {
	if role == "student" {
		return newPIN()
	}
	// Easy to read out and type: no 0/O, 1/l/I.
	const letters = "abcdefghjkmnpqrstuvwxyz23456789"
	b := make([]byte, 10)
	for i := range b {
		n, _ := rand.Int(rand.Reader, big.NewInt(int64(len(letters))))
		b[i] = letters[n.Int64()]
	}
	return string(b)
}

// setPassword gives anyone (from Penlumen School too) a new AMS password or
// PIN: the one typed, or a new one shown once.
func (s *Server) setPassword(w http.ResponseWriter, r *http.Request, user *auth.User) {
	p, err := s.store.Person(r.Context(), r.PathValue("id"))
	if err != nil {
		s.failErr(w, r, err)
		return
	}
	f := formForPerson(p)
	secret := r.PostFormValue("password")
	switch {
	case secret == "":
		secret = newSecret(p.Role)
	case p.IsStudent() && !pinPattern.MatchString(secret):
		s.renderPerson(w, r, http.StatusUnprocessableEntity, f, map[string]string{"new_password": "A PIN is 4 to 8 digits."}, "")
		return
	case !p.IsStudent() && len(secret) < MinPassword:
		s.renderPerson(w, r, http.StatusUnprocessableEntity, f, map[string]string{"new_password": fmt.Sprintf("Use at least %d characters.", MinPassword)}, "")
		return
	}
	hash, err := hashSecret(secret)
	if err == nil {
		err = s.store.SetPassword(r.Context(), p.UUID, hash)
	}
	if err != nil {
		s.failErr(w, r, err)
		return
	}
	slog.Info("password set", "person", p.UUID, "by", user.Sub)
	f.HasLogin = true
	s.renderPerson(w, r, http.StatusOK, f, nil, secret)
}

func (s *Server) removePerson(w http.ResponseWriter, r *http.Request, user *auth.User) {
	p, err := s.store.Person(r.Context(), r.PathValue("id"))
	if err != nil {
		s.failErr(w, r, err)
		return
	}
	if p.UUID == user.Sub {
		back(w, r, "/people/"+p.UUID, "You can't remove yourself.")
		return
	}
	if p.Source != store.SourceLocal {
		back(w, r, "/people/"+p.UUID, "This person comes from Penlumen School: remove them there.")
		return
	}
	if err := s.store.RemovePerson(r.Context(), p.UUID); err != nil {
		s.failErr(w, r, err)
		return
	}
	slog.Info("person removed", "person", p.UUID, "by", user.Sub)
	back(w, r, "/people?tab="+tabFor(p.Role), p.Name+" removed. Their past exams are kept.")
}

// ---------- Classes and terms ----------

// oneBranch is the branch a form chose, or the only one.
func (s *Server) oneBranch(r *http.Request) (string, error) {
	school, err := s.dir.School(r.Context())
	if err != nil {
		return "", err
	}
	want := r.PostFormValue("branch")
	for _, b := range school.Branches {
		if b.UUID == want || (want == "" && len(school.Branches) == 1) {
			return b.UUID, nil
		}
	}
	return "", nil
}

func (s *Server) saveClass(w http.ResponseWriter, r *http.Request, user *auth.User) {
	here := "/people?tab=classes"
	name := strings.TrimSpace(r.PostFormValue("name"))
	var subjects []string
	seen := map[string]bool{}
	for _, part := range strings.FieldsFunc(r.PostFormValue("subjects"), func(c rune) bool { return c == '\n' || c == ',' }) {
		if p := strings.TrimSpace(part); p != "" && !seen[strings.ToLower(p)] {
			seen[strings.ToLower(p)] = true
			subjects = append(subjects, p)
		}
	}
	branch, err := s.oneBranch(r)
	if err != nil {
		s.failErr(w, r, err)
		return
	}
	switch {
	case name == "":
		back(w, r, here, "Give the class a name.")
		return
	case len(subjects) == 0:
		back(w, r, here, "List the class's subjects, one per line.")
		return
	case branch == "":
		back(w, r, here, "Choose the class's branch.")
		return
	}
	// An edited class keeps its subjects' teachers.
	teachers := map[string]string{}
	if id := r.PostFormValue("id"); id != "" {
		classes, err := s.store.Classes(r.Context())
		if err != nil {
			s.failErr(w, r, err)
			return
		}
		for _, c := range classes {
			if c.UUID == id {
				for _, subject := range subjects {
					if t, ok := c.Teachers[subject]; ok {
						teachers[subject] = t
					}
				}
			}
		}
	}
	_, err = s.store.SaveClass(r.Context(), store.Class{UUID: r.PostFormValue("id"), Name: name, BranchUUID: branch,
		Subjects: subjects, Teachers: teachers, Source: store.SourceLocal})
	if errors.Is(err, store.ErrStatus) {
		back(w, r, here, "That class comes from Penlumen School: change it there.")
		return
	}
	if err != nil {
		s.failErr(w, r, err)
		return
	}
	back(w, r, here, name+" saved.")
}

// saveClassTeachers assigns a teacher to each subject of a class added in
// AMS. (Penlumen School's classes get theirs there.)
func (s *Server) saveClassTeachers(w http.ResponseWriter, r *http.Request, user *auth.User) {
	here := "/people?tab=classes"
	ctx := r.Context()
	if err := r.ParseForm(); err != nil {
		s.fail(w, r, http.StatusBadRequest, "The form couldn't be read.")
		return
	}
	classes, err := s.store.Classes(ctx)
	if err != nil {
		s.failErr(w, r, err)
		return
	}
	var class *store.Class
	for i := range classes {
		if classes[i].UUID == r.PathValue("id") {
			class = &classes[i]
		}
	}
	if class == nil {
		s.failErr(w, r, store.ErrNotFound)
		return
	}
	if class.Source != store.SourceLocal {
		back(w, r, here, "That class comes from Penlumen School: assign its teachers there.")
		return
	}
	staff, err := s.store.People(ctx, false, "")
	if err != nil {
		s.failErr(w, r, err)
		return
	}
	isStaff := map[string]bool{}
	for _, p := range staff {
		isStaff[p.UUID] = p.Active()
	}
	teachers := map[string]string{}
	for _, subject := range class.Subjects {
		if t := r.PostFormValue("teacher:" + subject); isStaff[t] {
			teachers[subject] = t
		}
	}
	class.Teachers = teachers
	if _, err := s.store.SaveClass(ctx, *class); err != nil {
		s.failErr(w, r, err)
		return
	}
	back(w, r, here, class.Name+"'s teachers saved.")
}

func (s *Server) removeClass(w http.ResponseWriter, r *http.Request, user *auth.User) {
	here := "/people?tab=classes"
	err := s.store.RemoveClass(r.Context(), r.PathValue("id"))
	switch {
	case errors.Is(err, store.ErrTaken):
		back(w, r, here, "Move or remove the class's students first.")
	case errors.Is(err, store.ErrStatus):
		back(w, r, here, "That class comes from Penlumen School: remove it there.")
	case err != nil:
		s.failErr(w, r, err)
	default:
		back(w, r, here, "Class removed.")
	}
}

func (s *Server) saveTerm(w http.ResponseWriter, r *http.Request, user *auth.User) {
	here := "/people?tab=terms"
	session, term := strings.TrimSpace(r.PostFormValue("session")), strings.TrimSpace(r.PostFormValue("term"))
	branch, err := s.oneBranch(r)
	if err != nil {
		s.failErr(w, r, err)
		return
	}
	if session == "" || term == "" || branch == "" {
		back(w, r, here, "Give the session (e.g. 2026/2027), the term (e.g. First Term) and the branch.")
		return
	}
	status := ""
	if r.PostFormValue("current") == "on" {
		status = "ACTIVE"
	}
	if _, err := s.store.SaveTerm(r.Context(), store.Term{UUID: r.PostFormValue("id"), BranchUUID: branch,
		Session: session, Term: term, Status: status, Source: store.SourceLocal}); err != nil {
		if errors.Is(err, store.ErrStatus) {
			back(w, r, here, "That term comes from Penlumen School: change it there.")
			return
		}
		s.failErr(w, r, err)
		return
	}
	back(w, r, here, session+" "+term+" saved.")
}

func (s *Server) removeTerm(w http.ResponseWriter, r *http.Request, user *auth.User) {
	here := "/people?tab=terms"
	err := s.store.RemoveTerm(r.Context(), r.PathValue("id"))
	switch {
	case errors.Is(err, store.ErrTaken):
		back(w, r, here, "Assessments use this term, so it stays.")
	case errors.Is(err, store.ErrStatus):
		back(w, r, here, "That term comes from Penlumen School: remove it there.")
	case err != nil:
		s.failErr(w, r, err)
	default:
		back(w, r, here, "Term removed.")
	}
}

func (s *Server) syncNow(w http.ResponseWriter, r *http.Request, user *auth.User) {
	if s.sync == nil {
		back(w, r, "/people", "Penlumen School isn't connected.")
		return
	}
	if err := s.sync.SyncAndRecord(r.Context()); err != nil {
		slog.Warn("sync now", "error", err)
		back(w, r, "/people", "Penlumen School couldn't be reached. AMS keeps working with what it has.")
		return
	}
	back(w, r, "/people", "Up to date with Penlumen School.")
}

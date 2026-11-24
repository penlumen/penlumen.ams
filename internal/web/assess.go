package web

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/penlumen/penlumen.ams/internal/auth"
	"github.com/penlumen/penlumen.ams/internal/marking"
	"github.com/penlumen/penlumen.ams/internal/questioncsv"
	"github.com/penlumen/penlumen.ams/internal/richtext"
	"github.com/penlumen/penlumen.ams/internal/seb"
	"github.com/penlumen/penlumen.ams/internal/sms"
	"github.com/penlumen/penlumen.ams/internal/store"
)

const formTime = "2006-01-02T15:04"

// ---------- Home ----------

type assessmentRow struct {
	store.Assessment
	Window string
	Class  string
}

func (s *Server) staffHome(w http.ResponseWriter, r *http.Request, user *auth.User) {
	list, err := s.store.AssessmentsForStaff(r.Context(), user.SchoolUUID, user.BranchUUIDs)
	if err != nil {
		s.failErr(w, r, err)
		return
	}
	names := s.classNames(r.Context())
	now := s.now()
	rows := make([]assessmentRow, len(list))
	counts := map[string]int{}
	for i, a := range list {
		rows[i] = assessmentRow{Assessment: a, Window: a.Window(now), Class: className(a.ClassUUID, names)}
		counts[rows[i].Window]++
	}
	s.render(w, r, http.StatusOK, "staff_home.html", map[string]any{"Assessments": rows, "Counts": counts})
}

// classNames maps class uuid to name. Penlumen School being down only costs
// the names, never the page.
func (s *Server) classNames(ctx context.Context) map[string]string {
	names := map[string]string{}
	classes, err := s.dir.Classes(ctx)
	if err != nil {
		slog.Warn("penlumen school: classes", "error", err)
		return names
	}
	for _, c := range classes {
		names[c.UUID] = c.Name
	}
	return names
}

// className is a class's name, or "a class" if Penlumen School doesn't say.
func className(uuid string, names map[string]string) string {
	if n, ok := names[uuid]; ok {
		return n
	}
	return "a class"
}

// termName is "2026/2027 First Term", or a dash if Penlumen School is down.
func (s *Server) termName(ctx context.Context, calendar string) string {
	terms, err := s.dir.Terms(ctx)
	if err != nil {
		return "—"
	}
	for _, t := range terms {
		if t.UUID == calendar {
			return strings.TrimSpace(t.Session + " " + t.Term)
		}
	}
	return "—"
}

// ---------- The assessment form (new and edit) ----------

type assessmentForm struct {
	Title, Kind, Class, Subject, Term string
	ReportMarks, Duration             string
	OpensAt, ClosesAt                 string
	ShuffleQuestions, ShowScore       bool
	RequireSEB                        bool
	Proctoring                        bool
	SEBKeys                           string
	Errors                            map[string]string
}

type classChoice struct {
	sms.Class
	Branch string
}

type termChoice struct {
	UUID, Label string
	Current     bool
}

// classInfo is what the form's script needs to show a class's subjects and
// its branch's terms once a class is picked.
type classInfo struct {
	Subjects []string     `json:"subjects"`
	Terms    []termChoice `json:"terms"`
}

type schoolChoices struct {
	Classes  []classChoice
	Branches map[string]sms.Branch
	ByClass  map[string]classInfo
	// MineOnly: a teacher, offered only the subjects they teach.
	MineOnly bool
}

// choices loads the classes (with subjects) and terms in the user's branches.
func (s *Server) choices(ctx context.Context, user *auth.User) (schoolChoices, error) {
	c := schoolChoices{Branches: map[string]sms.Branch{}, ByClass: map[string]classInfo{}}
	// Teachers (academic staff) set assessments only for the subjects they
	// teach; administrators and administrative staff for any.
	if !user.IsAdmin() {
		p, err := s.store.Person(ctx, user.Sub)
		if err != nil {
			return c, err
		}
		c.MineOnly = !p.AllSubjects()
	}
	school, err := s.dir.School(ctx)
	if err != nil {
		return c, err
	}
	for _, b := range school.Branches {
		if contains(user.BranchUUIDs, b.UUID) {
			c.Branches[b.UUID] = b
		}
	}
	classes, err := s.dir.Classes(ctx)
	if err != nil {
		return c, err
	}
	terms, err := s.dir.Terms(ctx)
	if err != nil {
		return c, err
	}
	termsByBranch := map[string][]termChoice{}
	for _, t := range terms {
		termsByBranch[t.BranchUUID] = append(termsByBranch[t.BranchUUID], termChoice{
			UUID: t.UUID, Label: strings.TrimSpace(t.Session + " " + t.Term), Current: t.Status == "ACTIVE",
		})
	}
	for _, cl := range classes {
		b, ok := c.Branches[cl.BranchUUID]
		if !ok {
			continue
		}
		if c.MineOnly {
			var mine []string
			for _, st := range cl.SubjectTeachers {
				if st.TeacherUUID != nil && *st.TeacherUUID == user.Sub {
					mine = append(mine, st.Name)
				}
			}
			if len(mine) == 0 {
				continue
			}
			cl.Subjects = mine
		}
		c.Classes = append(c.Classes, classChoice{Class: cl, Branch: b.Name})
		c.ByClass[cl.UUID] = classInfo{Subjects: cl.Subjects, Terms: termsByBranch[cl.BranchUUID]}
	}
	sort.Slice(c.Classes, func(i, j int) bool {
		if c.Classes[i].Branch != c.Classes[j].Branch {
			return c.Classes[i].Branch < c.Classes[j].Branch
		}
		return c.Classes[i].Name < c.Classes[j].Name
	})
	return c, nil
}

func (s *Server) renderForm(w http.ResponseWriter, r *http.Request, status int, c schoolChoices, f assessmentForm, a *store.Assessment) {
	info := c.ByClass[f.Class]
	s.render(w, r, status, "assessment_form.html", map[string]any{
		"Choices":  c,
		"Form":     f,
		"Info":     info,
		"Multi":    len(c.Branches) > 1,
		"Existing": a,
	})
}

func (s *Server) newAssessment(w http.ResponseWriter, r *http.Request, user *auth.User) {
	choices, err := s.choices(r.Context(), user)
	if err != nil {
		s.failErr(w, r, err)
		return
	}
	now := s.now().In(s.loc).Truncate(time.Hour).Add(time.Hour)
	f := assessmentForm{
		Kind: "ca", ReportMarks: "10", Duration: "30",
		OpensAt: now.Format(formTime), ClosesAt: now.Add(2 * time.Hour).Format(formTime),
	}
	s.renderForm(w, r, http.StatusOK, choices, f, nil)
}

// readAssessmentForm checks the form; errors land in f.Errors.
func (s *Server) readAssessmentForm(r *http.Request, user *auth.User, c schoolChoices) (store.Assessment, assessmentForm) {
	f := assessmentForm{
		Title:            strings.TrimSpace(r.PostFormValue("title")),
		Kind:             r.PostFormValue("kind"),
		Class:            r.PostFormValue("class"),
		Subject:          r.PostFormValue("subject"),
		Term:             r.PostFormValue("term"),
		ReportMarks:      strings.TrimSpace(r.PostFormValue("report_marks")),
		Duration:         strings.TrimSpace(r.PostFormValue("duration")),
		OpensAt:          r.PostFormValue("opens_at"),
		ClosesAt:         r.PostFormValue("closes_at"),
		ShuffleQuestions: r.PostFormValue("shuffle_questions") == "on",
		ShowScore:        r.PostFormValue("show_score") == "on",
		RequireSEB:       r.PostFormValue("require_seb") == "on",
		Proctoring:       r.PostFormValue("proctoring") == "on",
		SEBKeys:          strings.TrimSpace(r.PostFormValue("seb_keys")),
		Errors:           map[string]string{},
	}
	a := store.Assessment{
		SchoolUUID: user.SchoolUUID,
		Title:      f.Title,
		Kind:       f.Kind,
		Subject:    f.Subject,
		Settings:   store.Settings{ShuffleQuestions: f.ShuffleQuestions, ShowScore: f.ShowScore, RequireSEB: f.RequireSEB, Proctoring: f.Proctoring},
		CreatedBy:  user.Sub,
	}
	if a.Title == "" || len(a.Title) > 200 {
		f.Errors["title"] = "Give it a name, up to 200 characters."
	}
	if f.RequireSEB {
		keys, bad := seb.ParseKeys(f.SEBKeys)
		if len(bad) > 0 {
			f.Errors["seb_keys"] = "A Config Key is 64 letters and numbers (0–9, a–f), as the SEB Config Tool shows it. Check: " + strings.Join(bad, ", ")
		}
		a.Settings.SEBConfigKeys = keys
	}
	switch a.Kind {
	case "ca":
		a.CountsTowards = "assessment"
	case "exam":
		a.CountsTowards = "examination"
	default:
		f.Errors["kind"] = "Choose a type."
	}

	var class *classChoice
	for i := range c.Classes {
		if c.Classes[i].UUID == f.Class {
			class = &c.Classes[i]
		}
	}
	if class == nil {
		f.Errors["class"] = "Choose a class."
	} else {
		a.BranchUUID = class.BranchUUID
		a.ClassUUID = class.UUID
		switch {
		case a.Subject == "":
			f.Errors["subject"] = "Choose a subject."
		case !contains(class.Subjects, a.Subject) && c.MineOnly:
			f.Errors["subject"] = fmt.Sprintf("You don't teach %s in %s. Ask an administrator to assign it to you.", a.Subject, class.Name)
		case !contains(class.Subjects, a.Subject):
			f.Errors["subject"] = fmt.Sprintf("%s doesn't take %s.", class.Name, a.Subject)
		}
		for _, t := range c.ByClass[class.UUID].Terms {
			if t.UUID == f.Term {
				a.CalendarUUID = t.UUID
			}
		}
		if a.CalendarUUID == "" {
			f.Errors["term"] = "Choose a term."
		}
	}

	marks, err := strconv.ParseFloat(f.ReportMarks, 64)
	limit := 100.0
	if b, ok := c.Branches[a.BranchUUID]; ok {
		limit = b.AssessmentMax
		if a.CountsTowards == "examination" {
			limit = b.ExaminationMax
		}
	}
	if err != nil || marks <= 0 || marks > limit {
		f.Errors["report_marks"] = fmt.Sprintf("Enter a number from 1 to %s.", strconv.FormatFloat(limit, 'f', -1, 64))
	}
	a.ReportMarks = marking.Round2(marks)

	minutes, err := strconv.Atoi(f.Duration)
	if err != nil || minutes < 1 || minutes > 600 {
		f.Errors["duration"] = "Enter the minutes allowed, from 1 to 600."
	}
	a.DurationMinutes = minutes

	opens, errOpen := time.ParseInLocation(formTime, f.OpensAt, s.loc)
	closes, errClose := time.ParseInLocation(formTime, f.ClosesAt, s.loc)
	switch {
	case errOpen != nil:
		f.Errors["opens_at"] = "Choose when it opens."
	case errClose != nil:
		f.Errors["closes_at"] = "Choose when it closes."
	case !closes.After(opens):
		f.Errors["closes_at"] = "It must close after it opens."
	}
	a.OpensAt, a.ClosesAt = opens, closes
	return a, f
}

func (s *Server) createAssessment(w http.ResponseWriter, r *http.Request, user *auth.User) {
	if err := r.ParseForm(); err != nil {
		s.fail(w, r, http.StatusBadRequest, "The form couldn't be read.")
		return
	}
	choices, err := s.choices(r.Context(), user)
	if err != nil {
		s.failErr(w, r, err)
		return
	}
	a, f := s.readAssessmentForm(r, user, choices)
	if r.PostFormValue("pick") == "class" { // no JavaScript: show the class's subjects
		f.Errors = nil
		s.renderForm(w, r, http.StatusOK, choices, f, nil)
		return
	}
	if len(f.Errors) > 0 {
		s.renderForm(w, r, http.StatusUnprocessableEntity, choices, f, nil)
		return
	}
	id, err := s.store.CreateAssessment(r.Context(), a)
	if err != nil {
		s.failErr(w, r, err)
		return
	}
	http.Redirect(w, r, "/assessments/"+id, http.StatusSeeOther)
}

// canEditDetails: details change only until someone starts.
func (s *Server) canEditDetails(ctx context.Context, a store.Assessment) bool {
	if w := a.Window(s.now()); w != "draft" && w != "upcoming" {
		return false
	}
	started, err := s.store.HasAttempts(ctx, a.ID)
	return err == nil && !started
}

func (s *Server) detailsForm(w http.ResponseWriter, r *http.Request, user *auth.User) {
	a, ok := s.loadForStaff(w, r, user)
	if !ok {
		return
	}
	if !s.canEditDetails(r.Context(), a) {
		back(w, r, "/assessments/"+a.ID, "Details can't change once students can start. Change the closing time instead.")
		return
	}
	choices, err := s.choices(r.Context(), user)
	if err != nil {
		s.failErr(w, r, err)
		return
	}
	f := assessmentForm{
		Title: a.Title, Kind: a.Kind, Subject: a.Subject, Term: a.CalendarUUID,
		ReportMarks: strconv.FormatFloat(a.ReportMarks, 'f', -1, 64), Duration: strconv.Itoa(a.DurationMinutes),
		OpensAt: a.OpensAt.In(s.loc).Format(formTime), ClosesAt: a.ClosesAt.In(s.loc).Format(formTime),
		ShuffleQuestions: a.Settings.ShuffleQuestions, ShowScore: a.Settings.ShowScore,
		Proctoring: a.Settings.Proctoring, RequireSEB: a.Settings.RequireSEB, SEBKeys: strings.Join(a.Settings.SEBConfigKeys, "\n"),
	}
	f.Class = a.ClassUUID
	s.renderForm(w, r, http.StatusOK, choices, f, &a)
}

func (s *Server) saveDetails(w http.ResponseWriter, r *http.Request, user *auth.User) {
	existing, ok := s.loadForStaff(w, r, user)
	if !ok {
		return
	}
	if !s.canEditDetails(r.Context(), existing) {
		back(w, r, "/assessments/"+existing.ID, "Details can't change once students can start.")
		return
	}
	if err := r.ParseForm(); err != nil {
		s.fail(w, r, http.StatusBadRequest, "The form couldn't be read.")
		return
	}
	choices, err := s.choices(r.Context(), user)
	if err != nil {
		s.failErr(w, r, err)
		return
	}
	a, f := s.readAssessmentForm(r, user, choices)
	if r.PostFormValue("pick") == "class" {
		f.Errors = nil
		s.renderForm(w, r, http.StatusOK, choices, f, &existing)
		return
	}
	if len(f.Errors) > 0 {
		s.renderForm(w, r, http.StatusUnprocessableEntity, choices, f, &existing)
		return
	}
	a.ID = existing.ID
	if err := s.store.UpdateAssessment(r.Context(), a); err != nil {
		s.failErr(w, r, err)
		return
	}
	back(w, r, "/assessments/"+a.ID, "Details saved.")
}

// ---------- The builder ----------

// loadForStaff loads the assessment in the path if the user may manage it.
func (s *Server) loadForStaff(w http.ResponseWriter, r *http.Request, user *auth.User) (store.Assessment, bool) {
	a, err := s.store.Assessment(r.Context(), r.PathValue("id"))
	if err == nil && (a.SchoolUUID != user.SchoolUUID || !contains(user.BranchUUIDs, a.BranchUUID)) {
		err = store.ErrNotFound
	}
	if err != nil {
		s.failErr(w, r, err)
		return a, false
	}
	return a, true
}

type questionForm struct {
	ID      string // set when editing a question
	Kind    string
	Body    string
	Options []string
	Answer  string
	Marks   string
	Guide   string
	Error   string
}

func blankForms() (questionForm, questionForm) {
	return questionForm{Kind: "objective", Options: make([]string, 4), Marks: "1"},
		questionForm{Kind: "theory", Marks: "5"}
}

func (s *Server) editAssessment(w http.ResponseWriter, r *http.Request, user *auth.User) {
	a, ok := s.loadForStaff(w, r, user)
	if !ok {
		return
	}
	obj, theory := blankForms()
	s.renderBuilder(w, r, http.StatusOK, a, obj, theory, r.URL.Query().Get("notice"), nil)
}

// formFor fills a question form from a saved question, to edit it.
func formFor(q store.Question) questionForm {
	qf := questionForm{ID: q.ID, Kind: q.Kind, Body: q.Body, Marks: strconv.FormatFloat(q.Marks, 'f', -1, 64), Guide: q.Guide}
	if q.IsTheory() {
		return qf
	}
	qf.Options = append([]string{}, q.Options...)
	for len(qf.Options) < 4 {
		qf.Options = append(qf.Options, "")
	}
	if q.Answer != nil {
		qf.Answer = strconv.Itoa(*q.Answer)
	}
	return qf
}

func (s *Server) renderBuilder(w http.ResponseWriter, r *http.Request, status int, a store.Assessment, obj, theory questionForm, notice string, importErrors []questioncsv.RowError) {
	s.renderBuilderEditing(w, r, status, a, obj, theory, notice, importErrors, questionForm{})
}

// renderBuilderEditing is renderBuilder with one question's edit form
// showing a problem (edit.ID is that question).
func (s *Server) renderBuilderEditing(w http.ResponseWriter, r *http.Request, status int, a store.Assessment, obj, theory questionForm, notice string, importErrors []questioncsv.RowError, edit questionForm) {
	questions, err := s.store.Questions(r.Context(), a.ID)
	if err != nil {
		s.failErr(w, r, err)
		return
	}
	// Each question's edit form, filled in.
	forms := map[string]questionForm{}
	for _, q := range questions {
		forms[q.ID] = formFor(q)
	}
	if edit.ID != "" {
		forms[edit.ID] = edit
	}
	var objective, written []store.Question
	var objMarks, theoryMarks float64
	for _, q := range questions {
		if q.IsTheory() {
			written = append(written, q)
			theoryMarks += q.Marks
		} else {
			objective = append(objective, q)
			objMarks += q.Marks
		}
	}
	s.render(w, r, status, "assessment_edit.html", map[string]any{
		"A":            a,
		"Window":       a.Window(s.now()),
		"Class":        className(a.ClassUUID, s.classNames(r.Context())),
		"Term":         s.termName(r.Context(), a.CalendarUUID),
		"Objective":    objective,
		"Theory":       written,
		"ObjMarks":     objMarks,
		"TheoryMarks":  theoryMarks,
		"Q":            obj,
		"T":            theory,
		"Forms":        forms,
		"Notice":       notice,
		"ImportErrors": importErrors,
		"CanEdit":      s.canEditDetails(r.Context(), a),
	})
}

// readQuestion checks a question form. Empty objective options are dropped,
// keeping track of where the right one went.
func readQuestion(r *http.Request) (store.Question, questionForm) {
	qf := questionForm{
		Kind:    r.PostFormValue("kind"),
		Body:    richtext.Clean(r.PostFormValue("body")),
		Options: r.PostForm["option"],
		Answer:  r.PostFormValue("answer"),
		Marks:   strings.TrimSpace(r.PostFormValue("marks")),
		Guide:   strings.TrimSpace(r.PostFormValue("guide")),
	}
	if qf.Kind != "theory" {
		qf.Kind = "objective"
	}
	for len(qf.Options) < 4 {
		qf.Options = append(qf.Options, "")
	}
	q := store.Question{Kind: qf.Kind, Body: qf.Body, Guide: qf.Guide}
	marks, err := strconv.ParseFloat(qf.Marks, 64)
	if err != nil || marks <= 0 || marks > 100 {
		qf.Error = "Marks must be a number from 0.5 to 100."
	}
	q.Marks = marking.Round2(marks)
	if qf.Body == "" {
		qf.Error = "Write the question."
	}
	if qf.Kind == "theory" || qf.Error != "" {
		return q, qf
	}

	right, _ := strconv.Atoi(qf.Answer)
	answer := -1
	for i, o := range qf.Options {
		o = strings.TrimSpace(o)
		if o == "" {
			continue
		}
		if i == right && qf.Answer != "" {
			answer = len(q.Options)
		}
		q.Options = append(q.Options, o)
	}
	switch {
	case len(q.Options) < 2:
		qf.Error = "Give at least two options."
	case answer < 0:
		qf.Error = "Choose which option is right."
	}
	q.Answer = &answer
	return q, qf
}

func (s *Server) addQuestion(w http.ResponseWriter, r *http.Request, user *auth.User) {
	a, ok := s.loadForStaff(w, r, user)
	if !ok {
		return
	}
	if err := r.ParseForm(); err != nil {
		s.fail(w, r, http.StatusBadRequest, "The form couldn't be read.")
		return
	}
	q, qf := readQuestion(r)
	if qf.Error != "" {
		obj, theory := blankForms()
		if qf.Kind == "theory" {
			theory = qf
		} else {
			obj = qf
		}
		s.renderBuilder(w, r, http.StatusUnprocessableEntity, a, obj, theory, "", nil)
		return
	}
	if err := s.store.AddQuestion(r.Context(), a.ID, q); err != nil {
		s.failErr(w, r, err)
		return
	}
	anchor := "#objective"
	if q.IsTheory() {
		anchor = "#theory"
	}
	http.Redirect(w, r, "/assessments/"+a.ID+anchor, http.StatusSeeOther)
}

// updateQuestion saves an edited question. Its section (objective or
// theory) stays; only drafts change.
func (s *Server) updateQuestion(w http.ResponseWriter, r *http.Request, user *auth.User) {
	a, ok := s.loadForStaff(w, r, user)
	if !ok {
		return
	}
	old, err := s.store.Question(r.Context(), a.ID, r.PathValue("qid"))
	if err != nil {
		s.failErr(w, r, err)
		return
	}
	if err := r.ParseForm(); err != nil {
		s.fail(w, r, http.StatusBadRequest, "The form couldn't be read.")
		return
	}
	r.PostForm.Set("kind", old.Kind)
	q, qf := readQuestion(r)
	q.ID, qf.ID = old.ID, old.ID
	if qf.Error != "" {
		obj, theory := blankForms()
		s.renderBuilderEditing(w, r, http.StatusUnprocessableEntity, a, obj, theory, "", nil, qf)
		return
	}
	if err := s.store.UpdateQuestion(r.Context(), a.ID, q); err != nil {
		s.failErr(w, r, err)
		return
	}
	http.Redirect(w, r, "/assessments/"+a.ID+"#q-"+q.ID, http.StatusSeeOther)
}

func (s *Server) deleteQuestion(w http.ResponseWriter, r *http.Request, user *auth.User) {
	a, ok := s.loadForStaff(w, r, user)
	if !ok {
		return
	}
	if err := s.store.DeleteQuestion(r.Context(), a.ID, r.PathValue("qid")); err != nil {
		s.failErr(w, r, err)
		return
	}
	http.Redirect(w, r, "/assessments/"+a.ID, http.StatusSeeOther)
}

// changeStatus moves an assessment through its life: schedule it, take it
// back to draft, end it now, or change when it closes (which also reopens
// a closed one).
func (s *Server) changeStatus(w http.ResponseWriter, r *http.Request, user *auth.User) {
	a, ok := s.loadForStaff(w, r, user)
	if !ok {
		return
	}
	here := "/assessments/" + a.ID
	now := s.now()
	var err error
	switch r.PostFormValue("action") {
	case "schedule":
		if a.QuestionCount == 0 {
			back(w, r, here, "Add at least one question first.")
			return
		}
		if !a.ClosesAt.After(now) {
			back(w, r, here, "The closing time has passed. Edit the details first.")
			return
		}
		err = s.store.SetStatus(r.Context(), a.ID, []string{"draft"}, "scheduled")
	case "unschedule":
		if !s.canEditDetails(r.Context(), a) || a.Status != "scheduled" {
			back(w, r, here, "Students can already open it.")
			return
		}
		err = s.store.SetStatus(r.Context(), a.ID, []string{"scheduled"}, "draft")
	case "close":
		err = s.store.CloseNow(r.Context(), a.ID)
	case "closes":
		closes, perr := time.ParseInLocation(formTime, r.PostFormValue("closes_at"), s.loc)
		if perr != nil || !closes.After(now) {
			back(w, r, here, "Choose a closing time in the future.")
			return
		}
		err = s.store.SetClosing(r.Context(), a.ID, closes)
		if err == nil {
			back(w, r, here, "Closing time changed to "+closes.In(s.loc).Format("Mon 2 Jan, 15:04")+".")
			return
		}
	default:
		s.fail(w, r, http.StatusBadRequest, "Unknown action.")
		return
	}
	if err != nil {
		s.failErr(w, r, err)
		return
	}
	http.Redirect(w, r, here, http.StatusSeeOther)
}

// deleteAssessment removes an assessment for good. Scores already sent are
// taken off report cards first (approved report cards keep them); if
// Penlumen School can't be reached, nothing is deleted.
func (s *Server) deleteAssessment(w http.ResponseWriter, r *http.Request, user *auth.User) {
	a, ok := s.loadForStaff(w, r, user)
	if !ok {
		return
	}
	if r.PostFormValue("confirm") != "on" {
		back(w, r, "/assessments/"+a.ID, "Tick the box to confirm, then delete.")
		return
	}
	if a.PublishedAt != nil && s.school != nil {
		if err := s.school.WithdrawScores(r.Context(), "ams:"+a.ID); err != nil {
			s.failErr(w, r, err)
			return
		}
	}
	if err := s.store.DeleteAssessment(r.Context(), a.ID); err != nil {
		s.failErr(w, r, err)
		return
	}
	slog.Info("assessment deleted", "id", a.ID, "by", user.Sub, "title", a.Title)
	back(w, r, "/", fmt.Sprintf("Deleted %s · %s.", a.Subject, a.Title))
}

// ---------- Questions from CSV ----------

// maxUpload is the largest CSV accepted.
const maxUpload = 1 << 20

func (s *Server) importQuestions(w http.ResponseWriter, r *http.Request, user *auth.User) {
	a, ok := s.loadForStaff(w, r, user)
	if !ok {
		return
	}
	qs, rowErrs, err := readUpload(w, r)
	if err != nil {
		obj, theory := blankForms()
		s.renderBuilder(w, r, http.StatusUnprocessableEntity, a, obj, theory, err.Error(), nil)
		return
	}
	for _, q := range qs {
		if err := s.store.AddQuestion(r.Context(), a.ID, q); err != nil {
			s.failErr(w, r, err)
			return
		}
	}
	notice := fmt.Sprintf("Imported %d %s.", len(qs), plural(len(qs), "question", "questions"))
	if len(rowErrs) > 0 {
		notice += fmt.Sprintf(" %d %s skipped; fix them in the file and import just those.", len(rowErrs), plural(len(rowErrs), "row was", "rows were"))
	}
	a, _ = s.store.Assessment(r.Context(), a.ID)
	obj, theory := blankForms()
	s.renderBuilder(w, r, http.StatusOK, a, obj, theory, notice, rowErrs)
}

// readUpload reads the uploaded "file" as question CSV.
func readUpload(w http.ResponseWriter, r *http.Request) ([]store.Question, []questioncsv.RowError, error) {
	r.Body = http.MaxBytesReader(w, r.Body, maxUpload+4096)
	if err := r.ParseMultipartForm(maxUpload); err != nil {
		return nil, nil, fmt.Errorf("choose a CSV file of up to 1 MB")
	}
	file, _, err := r.FormFile("file")
	if err != nil {
		return nil, nil, fmt.Errorf("choose a CSV file to import")
	}
	defer file.Close()
	qs, rowErrs, err := questioncsv.Read(file)
	// A spreadsheet holds plain text: keep it exactly as typed.
	for i := range qs {
		qs[i].Body = richtext.FromText(qs[i].Body)
	}
	return qs, rowErrs, err
}

func (s *Server) exportQuestions(w http.ResponseWriter, r *http.Request, user *auth.User) {
	a, ok := s.loadForStaff(w, r, user)
	if !ok {
		return
	}
	qs, err := s.store.Questions(r.Context(), a.ID)
	if err != nil {
		s.failErr(w, r, err)
		return
	}
	// The file holds the words; formatting and pictures stay in the app.
	for i := range qs {
		qs[i].Body = richtext.ToText(qs[i].Body)
	}
	sendCSV(w, slug(a.Subject+" "+a.Title)+"-questions.csv")
	if err := questioncsv.Write(w, qs); err != nil {
		slog.Error("export questions", "error", err)
	}
}

func sendCSV(w http.ResponseWriter, filename string) {
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="`+filename+`"`)
}

var nonWord = regexp.MustCompile(`[^a-z0-9]+`)

func slug(s string) string {
	out := strings.Trim(nonWord.ReplaceAllString(strings.ToLower(s), "-"), "-")
	if out == "" {
		return "questions"
	}
	return out
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

// csvTemplate is an example file teachers can fill in.
func (s *Server) csvTemplate(w http.ResponseWriter, r *http.Request, user *auth.User) {
	zero := 0
	sendCSV(w, "questions-template.csv")
	questioncsv.Write(w, []store.Question{
		{Kind: "objective", Body: "Which part of the cell controls all its activities?",
			Options: []string{"Nucleus", "Cytoplasm", "Cell membrane", "Vacuole"}, Answer: &zero, Marks: 2},
		{Kind: "theory", Body: "State three differences between a plant cell and an animal cell.", Marks: 6,
			Guide: "Cell wall, large vacuole, chloroplasts: 2 marks each"},
	})
}

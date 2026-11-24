package web

import (
	"encoding/json"
	"errors"
	"html/template"
	"math/rand/v2"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/penlumen/penlumen.ams/internal/auth"
	"github.com/penlumen/penlumen.ams/internal/richtext"
	"github.com/penlumen/penlumen.ams/internal/seb"
	"github.com/penlumen/penlumen.ams/internal/store"
)

// MaxAnswer is the longest written answer kept, in characters (not
// counting formatting), and MaxAnswerHTML its longest formatted text.
const (
	MaxAnswer     = 20000
	MaxAnswerHTML = 100000
)

type studentRow struct {
	store.Assessment
	Window  string
	Attempt *store.Attempt
}

func (s *Server) studentHome(w http.ResponseWriter, r *http.Request, user *auth.User) {
	if user.Class == nil {
		s.render(w, r, http.StatusOK, "student_home.html", map[string]any{"NoClass": true})
		return
	}
	list, err := s.store.AssessmentsForClass(r.Context(), user.SchoolUUID, user.Class.UUID)
	if err != nil {
		s.failErr(w, r, err)
		return
	}
	attempts, err := s.store.AttemptsForStudent(r.Context(), user.Sub)
	if err != nil {
		s.failErr(w, r, err)
		return
	}
	now := s.now()
	var open, upcoming, done []studentRow
	for _, a := range list {
		row := studentRow{Assessment: a, Window: a.Window(now)}
		if at, ok := attempts[a.ID]; ok {
			row.Attempt = &at
		}
		switch {
		case row.Attempt != nil && !row.Attempt.Open(now):
			done = append(done, row)
		case row.Window == "open":
			open = append(open, row)
		case row.Window == "upcoming":
			upcoming = append(upcoming, row)
		default:
			done = append(done, row)
		}
	}
	s.render(w, r, http.StatusOK, "student_home.html", map[string]any{
		"Open": open, "Upcoming": upcoming, "Done": done,
	})
}

// loadForStudent loads the assessment in the path if it's set for the
// student's class and not a draft.
func (s *Server) loadForStudent(w http.ResponseWriter, r *http.Request, user *auth.User, id string) (store.Assessment, bool) {
	a, err := s.store.Assessment(r.Context(), id)
	if err == nil && (a.SchoolUUID != user.SchoolUUID || a.Status == "draft" ||
		user.Class == nil || a.ClassUUID != user.Class.UUID) {
		err = store.ErrNotFound
	}
	if err != nil {
		s.failErr(w, r, err)
		return a, false
	}
	return a, true
}

// sebOK lets the request through if the exam doesn't need Safe Exam
// Browser, or it comes from SEB. Otherwise it answers: a page telling the
// student to open the exam in SEB, or an error for the exam screen's saves.
func (s *Server) sebOK(w http.ResponseWriter, r *http.Request, a store.Assessment) bool {
	if !a.Settings.RequireSEB || seb.Check(r, s.origin+r.URL.RequestURI(), a.Settings.SEBConfigKeys) {
		return true
	}
	if strings.Contains(r.Header.Get("Accept"), "application/json") {
		s.fail(w, r, http.StatusForbidden, "This exam must be taken in Safe Exam Browser.")
		return false
	}
	s.render(w, r, http.StatusForbidden, "seb_required.html", map[string]any{
		"A": a,
		// html/template blocks link schemes it doesn't know (seb://); this one
		// is built here from the app's own address, so it's safe.
		"LaunchURL":   template.URL(seb.LaunchURL(s.origin + "/seb/" + a.ID + ".seb")),
		"DownloadURL": seb.DownloadURL,
	})
	return false
}

// sebConfig is the exam's Safe Exam Browser settings file. SEB downloads it
// itself (without the student's sign-in), so it holds nothing private: just
// where the exam starts and how to leave.
func (s *Server) sebConfig(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimSuffix(r.PathValue("file"), ".seb")
	a, err := s.store.Assessment(r.Context(), id)
	if err != nil || !a.Settings.RequireSEB || a.Status == "draft" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "application/seb")
	w.Header().Set("Content-Disposition", `attachment; filename="`+slug(a.Subject+" "+a.Title)+`.seb"`)
	w.Write(seb.Config(s.origin+"/take/"+a.ID, s.origin+"/seb/quit", a.Settings.Proctoring))
}

// sebQuit is where SEB is sent to close once the student has finished.
func (s *Server) sebQuit(w http.ResponseWriter, r *http.Request) {
	s.render(w, r, http.StatusOK, "seb_quit.html", nil)
}

// takeAssessment shows whatever fits: the start page, the exam, or the
// finished page.
func (s *Server) takeAssessment(w http.ResponseWriter, r *http.Request, user *auth.User) {
	a, ok := s.loadForStudent(w, r, user, r.PathValue("id"))
	if !ok || !s.sebOK(w, r, a) {
		return
	}
	now := s.now()
	at, err := s.store.AttemptFor(r.Context(), a.ID, user.Sub)
	switch {
	case errors.Is(err, store.ErrNotFound):
		data := map[string]any{"A": a, "Window": a.Window(now)}
		if r.URL.Query().Get("rules") == "1" {
			data["Notice"] = "Tick that you've read the rules, then start."
		}
		s.render(w, r, http.StatusOK, "take_start.html", data)
		return
	case err != nil:
		s.failErr(w, r, err)
		return
	}

	if !at.Open(now) {
		if at.Status == "in_progress" {
			if err := s.finishAttempt(r.Context(), at, "expired"); err != nil {
				s.failErr(w, r, err)
				return
			}
			if at, err = s.store.Attempt(r.Context(), at.ID); err != nil {
				s.failErr(w, r, err)
				return
			}
		}
		data := map[string]any{"A": a, "Attempt": at}
		if a.Settings.ShowScore && at.Score != nil {
			unmarked, _ := s.store.Unmarked(r.Context(), a.ID)
			if unmarked == 0 {
				data["Score"] = *at.Score
			}
		}
		s.render(w, r, http.StatusOK, "take_done.html", data)
		return
	}
	s.renderExam(w, r, a, at)
}

type examQuestion struct {
	store.Question
	Number  int
	Section string // A or B
	Chosen  int    // -1 when not answered
	Text    string
	Flagged bool
}

func (s *Server) renderExam(w http.ResponseWriter, r *http.Request, a store.Assessment, at store.Attempt) {
	questions, err := s.store.Questions(r.Context(), a.ID)
	if err != nil {
		s.failErr(w, r, err)
		return
	}
	responses, err := s.store.Responses(r.Context(), at.ID)
	if err != nil {
		s.failErr(w, r, err)
		return
	}
	byID := map[string]store.Question{}
	for _, q := range questions {
		q.Answer, q.Guide = nil, "" // never send the answer key to the browser
		byID[q.ID] = q
	}
	// Section A (objective) first, then B (theory), each in the student's order.
	var list []examQuestion
	for _, kind := range []string{"objective", "theory"} {
		for _, id := range at.QuestionOrder {
			q, ok := byID[id]
			if !ok || q.Kind != kind {
				continue // deleted after the student started, or the other section
			}
			eq := examQuestion{Question: q, Number: len(list) + 1, Section: "A", Chosen: -1}
			if q.IsTheory() {
				eq.Section = "B"
			}
			if resp, ok := responses[id]; ok {
				eq.Flagged = resp.Flagged
				if resp.Answer != nil {
					eq.Chosen = *resp.Answer
				}
				if resp.Text != nil {
					eq.Text = *resp.Text
				}
			}
			list = append(list, eq)
		}
	}
	var objective, theory int
	for _, q := range list {
		if q.IsTheory() {
			theory++
		} else {
			objective++
		}
	}
	secondsLeft := max(int(at.Deadline.Sub(s.now()).Seconds()), 0)
	monitored := false
	if a.Settings.Proctoring {
		if monitored, err = s.store.HasEvent(r.Context(), at.ID, "monitoring_on"); err != nil {
			s.failErr(w, r, err)
			return
		}
	}
	s.render(w, r, http.StatusOK, "take_exam.html", map[string]any{
		"A":           a,
		"Attempt":     at,
		"Questions":   list,
		"Objective":   objective,
		"Theory":      theory,
		"SecondsLeft": secondsLeft,
		"Monitored":   monitored,
	})
}

func (s *Server) startAttempt(w http.ResponseWriter, r *http.Request, user *auth.User) {
	a, ok := s.loadForStudent(w, r, user, r.PathValue("id"))
	if !ok || !s.sebOK(w, r, a) {
		return
	}
	here := "/take/" + a.ID
	now := s.now()
	if a.Window(now) != "open" {
		http.Redirect(w, r, here, http.StatusSeeOther)
		return
	}
	if r.PostFormValue("agree") != "on" {
		if _, err := s.store.AttemptFor(r.Context(), a.ID, user.Sub); errors.Is(err, store.ErrNotFound) {
			http.Redirect(w, r, here+"?rules=1", http.StatusSeeOther)
			return
		}
	}
	questions, err := s.store.Questions(r.Context(), a.ID)
	if err != nil {
		s.failErr(w, r, err)
		return
	}
	order := make([]string, len(questions))
	for i, q := range questions {
		order[i] = q.ID
	}
	if a.Settings.ShuffleQuestions {
		rand.Shuffle(len(order), func(i, j int) { order[i], order[j] = order[j], order[i] })
	}
	deadline := now.Add(time.Duration(a.DurationMinutes) * time.Minute)
	if a.ClosesAt.Before(deadline) {
		deadline = a.ClosesAt
	}
	_, err = s.store.StartAttempt(r.Context(), store.Attempt{
		AssessmentID:  a.ID,
		StudentUUID:   user.Sub,
		StudentName:   user.Name,
		RegNumber:     user.RegNumber,
		ClassUUID:     user.Class.UUID,
		Deadline:      deadline,
		QuestionOrder: order,
	})
	if err != nil {
		s.failErr(w, r, err)
		return
	}
	http.Redirect(w, r, here, http.StatusSeeOther)
}

// loadAttempt loads the student's own attempt from the path.
func (s *Server) loadAttempt(w http.ResponseWriter, r *http.Request, user *auth.User) (store.Attempt, bool) {
	at, err := s.store.Attempt(r.Context(), r.PathValue("id"))
	if err == nil && at.StudentUUID != user.Sub {
		err = store.ErrNotFound
	}
	if err != nil {
		s.failErr(w, r, err)
		return at, false
	}
	// Saves and submits need Safe Exam Browser too, if the exam does.
	a, err := s.store.Assessment(r.Context(), at.AssessmentID)
	if err != nil {
		s.failErr(w, r, err)
		return at, false
	}
	return at, s.sebOK(w, r, a)
}

// saveAnswer is the exam screen's autosave: one answer as JSON, either a
// chosen option or written text.
func (s *Server) saveAnswer(w http.ResponseWriter, r *http.Request, user *auth.User) {
	r.Header.Set("Accept", "application/json")
	at, ok := s.loadAttempt(w, r, user)
	if !ok {
		return
	}
	var body struct {
		QuestionID string  `json:"question_id"`
		Answer     *int    `json:"answer"`
		Text       *string `json:"text"`
		Flagged    bool    `json:"flagged"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, MaxAnswerHTML*2+4096)).Decode(&body); err != nil || body.QuestionID == "" {
		s.fail(w, r, http.StatusBadRequest, "Bad answer.")
		return
	}
	if body.Answer != nil && (*body.Answer < 0 || *body.Answer > 25) {
		s.fail(w, r, http.StatusBadRequest, "Bad answer.")
		return
	}
	if body.Text != nil {
		t := richtext.Clean(*body.Text)
		if richtext.Length(t) > MaxAnswer || len(t) > MaxAnswerHTML {
			s.fail(w, r, http.StatusBadRequest, "That answer is too long.")
			return
		}
		body.Text = &t
	}
	if !at.Open(s.now()) {
		s.failErr(w, r, store.ErrClosed)
		return
	}
	resp := store.Response{Answer: body.Answer, Text: body.Text, Flagged: body.Flagged}
	if err := s.store.SaveResponse(r.Context(), at.ID, body.QuestionID, resp); err != nil {
		s.failErr(w, r, err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Write([]byte(`{"saved":true}`))
}

// ping is the exam screen checking in (every 30 seconds), or telling the
// invigilator the student left or came back to the page.
func (s *Server) ping(w http.ResponseWriter, r *http.Request, user *auth.User) {
	r.Header.Set("Accept", "application/json")
	at, ok := s.loadAttempt(w, r, user)
	if !ok {
		return
	}
	var err error
	switch kind := r.URL.Query().Get("event"); {
	case kind == "":
		err = s.store.Seen(r.Context(), at.ID)
	case kind == "left_page" || kind == "returned":
		err = s.store.Event(r.Context(), at.ID, kind)
	case kind == "reloaded":
		// The page was only reloaded, not left: take back its "left the page".
		err = s.store.Reloaded(r.Context(), at.ID)
	case store.EventKinds[kind] != "":
		// Camera, sound and screen alerts: only for exams that monitor them.
		a, aerr := s.store.Assessment(r.Context(), at.AssessmentID)
		if aerr != nil || !a.Settings.Proctoring {
			s.fail(w, r, http.StatusBadRequest, "This exam doesn't use camera, sound and screen monitoring.")
			return
		}
		err = s.store.Event(r.Context(), at.ID, kind)
	default:
		s.fail(w, r, http.StatusBadRequest, "Unknown event.")
		return
	}
	if err != nil {
		s.failErr(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// submitAttempt saves the answers in the form (so the exam also works
// without JavaScript), then marks and closes the attempt.
func (s *Server) submitAttempt(w http.ResponseWriter, r *http.Request, user *auth.User) {
	at, ok := s.loadAttempt(w, r, user)
	if !ok {
		return
	}
	here := "/take/" + at.AssessmentID
	r.Body = http.MaxBytesReader(w, r.Body, 4<<20)
	if err := r.ParseForm(); err != nil {
		s.fail(w, r, http.StatusBadRequest, "The answers couldn't be read.")
		return
	}
	if at.Open(s.now()) {
		for _, id := range at.QuestionOrder {
			resp := store.Response{Flagged: r.PostFormValue("flag_"+id) == "1"}
			raw := strings.TrimSpace(r.PostFormValue("q_" + id))
			text, hasText := r.PostForm["t_"+id]
			switch {
			case raw != "":
				n, err := strconv.Atoi(raw)
				if err != nil || n < 0 || n > 25 {
					continue
				}
				resp.Answer = &n
			case hasText:
				t := text[0]
				if len(t) > MaxAnswerHTML {
					t = t[:MaxAnswerHTML] // cleaning closes anything cut off
				}
				t = richtext.Clean(t)
				resp.Text = &t
			case !resp.Flagged:
				continue // nothing to save
			}
			if err := s.store.SaveResponse(r.Context(), at.ID, id, resp); err != nil && !errors.Is(err, store.ErrClosed) {
				s.failErr(w, r, err)
				return
			}
		}
	}
	status := "submitted"
	if !at.Open(s.now()) {
		status = "expired"
	}
	if at.Status == "in_progress" {
		if err := s.finishAttempt(r.Context(), at, status); err != nil {
			s.failErr(w, r, err)
			return
		}
	}
	http.Redirect(w, r, here, http.StatusSeeOther)
}

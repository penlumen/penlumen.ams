package web

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/penlumen/penlumen.ams/internal/auth"
	"github.com/penlumen/penlumen.ams/internal/marking"
	"github.com/penlumen/penlumen.ams/internal/sms"
	"github.com/penlumen/penlumen.ams/internal/store"
)

type resultRow struct {
	store.Attempt
	Report  float64
	Outcome *sms.ScoreOutcome
}

// resultRows finishes timed-out attempts, then lists everyone with their
// score on the report and what Penlumen School said last time.
func (s *Server) resultRows(ctx context.Context, a store.Assessment) ([]resultRow, sms.ScoresResult, error) {
	var report sms.ScoresResult
	if err := s.finishExpired(ctx, a); err != nil {
		return nil, report, err
	}
	attempts, err := s.store.Attempts(ctx, a.ID)
	if err != nil {
		return nil, report, err
	}
	if len(a.PublishReport) > 0 {
		_ = json.Unmarshal(a.PublishReport, &report)
	}
	outcomes := map[string]*sms.ScoreOutcome{}
	for i := range report.Results {
		outcomes[report.Results[i].StudentUUID] = &report.Results[i]
	}
	rows := make([]resultRow, len(attempts))
	for i, at := range attempts {
		rows[i] = resultRow{Attempt: at, Outcome: outcomes[at.StudentUUID]}
		if at.Score != nil {
			rows[i].Report = marking.ToReport(*at.Score, a.TotalMarks, a.ReportMarks)
		}
	}
	return rows, report, nil
}

func (s *Server) submissions(w http.ResponseWriter, r *http.Request, user *auth.User) {
	a, ok := s.loadForStaff(w, r, user)
	if !ok {
		return
	}
	rows, report, err := s.resultRows(r.Context(), a)
	if err != nil {
		s.failErr(w, r, err)
		return
	}
	unmarked, err := s.store.Unmarked(r.Context(), a.ID)
	if err != nil {
		s.failErr(w, r, err)
		return
	}
	writing := 0
	for _, row := range rows {
		if row.Status == "in_progress" {
			writing++
		}
	}
	window := a.Window(s.now())
	finished := len(rows) - writing
	s.render(w, r, http.StatusOK, "submissions.html", map[string]any{
		"A":         a,
		"Window":    window,
		"Rows":      rows,
		"Writing":   writing,
		"Finished":  finished,
		"Unmarked":  unmarked,
		"Report":    report,
		"Connected": s.school != nil,
		"CanPublish": s.school != nil && (window == "closed" || window == "published") && writing == 0 &&
			unmarked == 0 && finished > 0,
		"Notice": r.URL.Query().Get("notice"),
	})
}

// finishExpired marks attempts whose time ran out without a submit.
func (s *Server) finishExpired(ctx context.Context, a store.Assessment) error {
	attempts, err := s.store.Attempts(ctx, a.ID)
	if err != nil {
		return err
	}
	now := s.now()
	for _, at := range attempts {
		if at.Status == "in_progress" && !at.Open(now) {
			if err := s.finishAttempt(ctx, at, "expired"); err != nil {
				return err
			}
		}
	}
	return nil
}

// finishAttempt marks the objective answers and closes the attempt; theory
// answers wait for a teacher.
func (s *Server) finishAttempt(ctx context.Context, at store.Attempt, status string) error {
	questions, err := s.store.Questions(ctx, at.AssessmentID)
	if err != nil {
		return err
	}
	responses, err := s.store.Responses(ctx, at.ID)
	if err != nil {
		return err
	}
	mq := make([]marking.Question, len(questions))
	for i, q := range questions {
		mq[i] = marking.Question{ID: q.ID, Marks: q.Marks, Answer: q.Answer}
	}
	answers := map[string]*int{}
	for id, resp := range responses {
		answers[id] = resp.Answer
	}
	return s.store.FinishAttempt(ctx, at.ID, status, marking.Objective(mq, answers).PerQuestion)
}

func (s *Server) publish(w http.ResponseWriter, r *http.Request, user *auth.User) {
	a, ok := s.loadForStaff(w, r, user)
	if !ok {
		return
	}
	here := "/assessments/" + a.ID + "/submissions"
	if s.school == nil {
		back(w, r, here, "Report cards are in Penlumen School, which isn't connected. Export the scores as CSV instead.")
		return
	}
	if window := a.Window(s.now()); window != "closed" && window != "published" {
		back(w, r, here, "End the exam before sending scores.")
		return
	}
	rows, _, err := s.resultRows(r.Context(), a)
	if err != nil {
		s.failErr(w, r, err)
		return
	}
	if n, err := s.store.Unmarked(r.Context(), a.ID); err != nil || n > 0 {
		back(w, r, here, fmt.Sprintf("%d written %s still to mark.", n, plural(n, "answer is", "answers are")))
		return
	}
	batch := sms.Scores{
		SourceID:     "ams:" + a.ID,
		Title:        a.Title,
		Subject:      a.Subject,
		Component:    a.CountsTowards,
		CalendarUUID: a.CalendarUUID,
	}
	for _, row := range rows {
		if row.Score != nil {
			batch.Scores = append(batch.Scores, sms.StudentMark{StudentUUID: row.StudentUUID, Score: row.Report})
		}
	}
	if len(batch.Scores) == 0 {
		back(w, r, here, "Nobody has sat it yet.")
		return
	}
	result, err := s.school.RecordScores(r.Context(), batch)
	if err != nil {
		s.failErr(w, r, err)
		return
	}
	if err := s.store.MarkPublished(r.Context(), a.ID, result); err != nil {
		s.failErr(w, r, err)
		return
	}
	http.Redirect(w, r, here, http.StatusSeeOther)
}

func (s *Server) exportSubmissions(w http.ResponseWriter, r *http.Request, user *auth.User) {
	a, ok := s.loadForStaff(w, r, user)
	if !ok {
		return
	}
	rows, _, err := s.resultRows(r.Context(), a)
	if err != nil {
		s.failErr(w, r, err)
		return
	}
	sendCSV(w, slug(a.Subject+" "+a.Title)+"-submissions.csv")
	cw := csv.NewWriter(w)
	cw.Write([]string{"student", "reg_number", "status", "score", "out_of", "report_score", "report_out_of", "penlumen_school"})
	f := func(v float64) string { return strconv.FormatFloat(v, 'f', -1, 64) }
	for _, row := range rows {
		score, report := "", ""
		if row.Score != nil {
			score, report = f(*row.Score), f(row.Report)
		}
		sent := "not sent"
		if row.Outcome != nil {
			sent = row.Outcome.Status
			if row.Outcome.Message != "" {
				sent += ": " + row.Outcome.Message
			}
		}
		cw.Write([]string{row.StudentName, row.RegNumber, statusName(row.Status), score, f(a.TotalMarks), report, f(a.ReportMarks), sent})
	}
	cw.Flush()
	if err := cw.Error(); err != nil {
		slog.Error("export submissions", "error", err)
	}
}

func statusName(status string) string {
	switch status {
	case "in_progress":
		return "writing"
	case "expired":
		return "time ran out"
	}
	return "submitted"
}

// changeAttempt resets or deletes one student's attempt, whatever its state.
// If their score was already sent, it's taken off their report card first;
// if Penlumen School can't be reached, nothing changes.
func (s *Server) changeAttempt(w http.ResponseWriter, r *http.Request, user *auth.User) {
	a, ok := s.loadForStaff(w, r, user)
	if !ok {
		return
	}
	here := "/assessments/" + a.ID + "/submissions"
	at, err := s.store.Attempt(r.Context(), r.PathValue("attempt"))
	if err != nil || at.AssessmentID != a.ID {
		s.failErr(w, r, store.ErrNotFound)
		return
	}
	action := r.PathValue("action")
	if action != "reset" && action != "delete" {
		s.fail(w, r, http.StatusNotFound, "Unknown action.")
		return
	}
	if r.PostFormValue("confirm") != "on" {
		back(w, r, here, "Tick the box to confirm, then try again.")
		return
	}
	if a.PublishedAt != nil && s.school != nil {
		if err := s.school.WithdrawStudentScore(r.Context(), "ams:"+a.ID, at.StudentUUID); err != nil {
			s.failErr(w, r, err)
			return
		}
	}
	var notice string
	if action == "reset" {
		// A full, fresh timer, even if the exam has ended: the student sits it again now.
		deadline := s.now().Add(time.Duration(a.DurationMinutes) * time.Minute)
		if err := s.store.ResetAttempt(r.Context(), at.ID, deadline); err != nil {
			s.failErr(w, r, err)
			return
		}
		notice = fmt.Sprintf("Reset %s's exam: their answers are cleared and they have %d minutes from now to sit it again.", at.StudentName, a.DurationMinutes)
	} else {
		if err := s.store.DeleteAttempt(r.Context(), at.ID); err != nil {
			s.failErr(w, r, err)
			return
		}
		notice = fmt.Sprintf("Deleted %s's exam.", at.StudentName)
		if a.Window(s.now()) == "open" {
			notice += " They can start again while it's open."
		}
	}
	if a.PublishedAt != nil && s.school != nil {
		notice += " Their score was taken off their report card; send scores again once they're done."
	}
	slog.Info("attempt "+action, "assessment", a.ID, "attempt", at.ID, "student", at.StudentUUID, "by", user.Sub)
	back(w, r, here, notice)
}

// Package store reads and writes the app's own database, SQLite or Postgres.
// Every query lives here, written as plain SQL that both understand (see
// sql.go), so the rest of the code never builds SQL.
package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"github.com/penlumen/penlumen.ams/internal/db"
)

var (
	ErrNotFound = errors.New("not found")
	// ErrClosed means an attempt can no longer be changed: it was submitted,
	// or its time is up.
	ErrClosed = errors.New("this attempt is closed")
	// ErrStatus means the assessment isn't in a state that allows the change.
	ErrStatus = errors.New("not allowed in the assessment's current state")
)

// Grace is how long after the deadline answers are still accepted, to allow
// for a slow connection at the last second.
const Grace = 30 * time.Second

type Store struct {
	db      *sql.DB
	dialect db.Dialect
}

func New(d *db.DB) *Store {
	if d == nil {
		return &Store{} // pages-only tests
	}
	return &Store{db: d.DB, dialect: d.Dialect}
}

// ---------- Assessments ----------

type Settings struct {
	ShuffleQuestions bool `json:"shuffle_questions"`
	// ShowScore shows students their mark as soon as they submit.
	ShowScore bool `json:"show_score"`
	// RequireSEB means students must use Safe Exam Browser (see package seb).
	RequireSEB bool `json:"require_seb,omitempty"`
	// SEBConfigKeys are the Config Keys SEB's signed requests are checked
	// against; without any, only SEB's user agent is checked.
	SEBConfigKeys []string `json:"seb_config_keys,omitempty"`
	// Proctoring turns on camera, sound and screen monitoring: students must
	// share all three to start, noise and lost devices raise alerts, and
	// photos are taken when something suspicious happens.
	Proctoring bool `json:"proctoring,omitempty"`
}

type Assessment struct {
	ID              string
	SchoolUUID      string
	BranchUUID      string
	CalendarUUID    string
	Subject         string
	Title           string
	Kind            string // ca (assessment) or exam (examination)
	CountsTowards   string // assessment or examination
	ReportMarks     float64
	DurationMinutes int
	OpensAt         time.Time
	ClosesAt        time.Time
	Settings        Settings
	Status          string // draft, scheduled, closed or published
	CreatedBy       string
	CreatedAt       time.Time
	PublishedAt     *time.Time
	PublishReport   json.RawMessage
	// ClassUUID is the class (in Penlumen School) that sits it.
	ClassUUID string

	// Worked out from its questions.
	QuestionCount int
	TotalMarks    float64
}

// Window says where the assessment is for students right now: draft,
// upcoming, open, closed or published.
func (a Assessment) Window(now time.Time) string {
	switch a.Status {
	case "draft", "closed", "published":
		return a.Status
	}
	switch {
	case now.Before(a.OpensAt):
		return "upcoming"
	case now.After(a.ClosesAt):
		return "closed"
	}
	return "open"
}

const assessmentColumns = `
	a.id, a.school_uuid, a.branch_uuid, a.calendar_uuid, a.subject, a.title,
	a.kind, a.counts_towards, a.report_marks, a.duration_minutes,
	a.opens_at, a.closes_at, a.settings, a.status, a.created_by, a.created_at,
	a.published_at, a.publish_report, a.class_uuid,
	(SELECT count(*) FROM questions q JOIN sections s ON s.id = q.section_id
	 WHERE s.assessment_id = a.id),
	coalesce((SELECT sum(q.marks) FROM questions q JOIN sections s ON s.id = q.section_id
	          WHERE s.assessment_id = a.id), 0)`

type scanner interface{ Scan(dest ...any) error }

func scanAssessment(row scanner) (Assessment, error) {
	var a Assessment
	var publishReport []byte
	err := row.Scan(
		&a.ID, &a.SchoolUUID, &a.BranchUUID, &a.CalendarUUID, &a.Subject, &a.Title,
		&a.Kind, &a.CountsTowards, &a.ReportMarks, &a.DurationMinutes,
		&a.OpensAt, &a.ClosesAt, &a.Settings, &a.Status, &a.CreatedBy, &a.CreatedAt,
		&a.PublishedAt, &publishReport, &a.ClassUUID, &a.QuestionCount, &a.TotalMarks,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return a, ErrNotFound
	}
	a.PublishReport = publishReport
	return a, err
}

func collectAssessments(rows *sql.Rows, err error) ([]Assessment, error) {
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Assessment
	for rows.Next() {
		a, err := scanAssessment(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// CreateAssessment saves a new draft with its two sections.
func (s *Store) CreateAssessment(ctx context.Context, a Assessment) (string, error) {
	tx, c, err := s.begin(ctx)
	if err != nil {
		return "", err
	}
	defer tx.Rollback()

	id := newID()
	if _, err := c.exec(ctx, `
		INSERT INTO assessments (id, school_uuid, branch_uuid, calendar_uuid, subject, title,
			kind, counts_towards, report_marks, duration_minutes, opens_at, closes_at,
			settings, created_by, class_uuid)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15)`,
		id, a.SchoolUUID, a.BranchUUID, a.CalendarUUID, a.Subject, a.Title,
		a.Kind, a.CountsTowards, a.ReportMarks, a.DurationMinutes, a.OpensAt, a.ClosesAt,
		a.Settings, a.CreatedBy, a.ClassUUID); err != nil {
		return "", err
	}
	if _, err := c.exec(ctx, `
		INSERT INTO sections (id, assessment_id, position, title, kind)
		VALUES ($2, $1, 1, 'Section A · Objective', 'objective'),
		       ($3, $1, 2, 'Section B · Theory', 'theory')`, id, newID(), newID()); err != nil {
		return "", err
	}
	return id, tx.Commit()
}

// UpdateAssessment saves edited details. Only drafts and scheduled
// assessments nobody has started can change this much; the caller checks.
func (s *Store) UpdateAssessment(ctx context.Context, a Assessment) error {
	n, err := s.c().exec(ctx, `
		UPDATE assessments SET branch_uuid = $2, calendar_uuid = $3, subject = $4, title = $5,
			kind = $6, counts_towards = $7, report_marks = $8, duration_minutes = $9,
			opens_at = $10, closes_at = $11, settings = $12, class_uuid = $13, updated_at = now()
		WHERE id = $1 AND status IN ('draft', 'scheduled')
		  AND NOT EXISTS (SELECT 1 FROM attempts t WHERE t.assessment_id = $1)`,
		a.ID, a.BranchUUID, a.CalendarUUID, a.Subject, a.Title, a.Kind, a.CountsTowards,
		a.ReportMarks, a.DurationMinutes, a.OpensAt, a.ClosesAt, a.Settings, a.ClassUUID)
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrStatus
	}
	return nil
}

// SetClosing moves the closing time (reopening a closed assessment, or
// extending an open one) and schedules it again. Scores already sent stay
// on reports until sent again.
func (s *Store) SetClosing(ctx context.Context, id string, closes time.Time) error {
	n, err := s.c().exec(ctx, `
		UPDATE assessments SET closes_at = $2, status = 'scheduled', updated_at = now()
		WHERE id = $1 AND status IN ('scheduled', 'closed', 'published') AND opens_at < $2`, id, closes)
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrStatus
	}
	return nil
}

// DeleteAssessment removes an assessment for good, with its questions and
// every attempt, answer and mark (the tables cascade).
func (s *Store) DeleteAssessment(ctx context.Context, id string) error {
	n, err := s.c().exec(ctx, `DELETE FROM assessments WHERE id = $1`, id)
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// HasAttempts reports whether anyone has started the assessment.
func (s *Store) HasAttempts(ctx context.Context, id string) (bool, error) {
	var n int
	err := s.c().row(ctx, `SELECT count(*) FROM attempts WHERE assessment_id = $1`, id).Scan(&n)
	return n > 0, err
}

func (s *Store) Assessment(ctx context.Context, id string) (Assessment, error) {
	return scanAssessment(s.c().row(ctx,
		`SELECT `+assessmentColumns+` FROM assessments a WHERE a.id = $1`, id))
}

// AssessmentsForStaff lists a school's assessments in the given branches,
// newest first.
func (s *Store) AssessmentsForStaff(ctx context.Context, school string, branches []string) ([]Assessment, error) {
	list, args := in(2, branches)
	return collectAssessments(s.c().query(ctx, `
		SELECT `+assessmentColumns+` FROM assessments a
		WHERE a.school_uuid = $1 AND a.branch_uuid IN (`+list+`)
		ORDER BY a.opens_at DESC, a.created_at DESC`, append([]any{school}, args...)...))
}

// AssessmentsForClass lists what a class's students can see (not drafts).
func (s *Store) AssessmentsForClass(ctx context.Context, school, class string) ([]Assessment, error) {
	return collectAssessments(s.c().query(ctx, `
		SELECT `+assessmentColumns+` FROM assessments a
		WHERE a.school_uuid = $1 AND a.status <> 'draft' AND a.class_uuid = $2
		ORDER BY a.opens_at`, school, class))
}

// SetStatus moves an assessment to `to`, but only from one of `from`.
func (s *Store) SetStatus(ctx context.Context, id string, from []string, to string) error {
	list, args := in(3, from)
	n, err := s.c().exec(ctx, `
		UPDATE assessments SET status = $2, updated_at = now()
		WHERE id = $1 AND status IN (`+list+`)`, append([]any{id, to}, args...)...)
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrStatus
	}
	return nil
}

// CloseNow closes a scheduled assessment straight away; students still
// writing have their time cut to now.
func (s *Store) CloseNow(ctx context.Context, id string) error {
	tx, c, err := s.begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	n, err := c.exec(ctx, `
		UPDATE assessments SET status = 'closed', updated_at = now(),
			closes_at = CASE WHEN closes_at < now() THEN closes_at ELSE now() END
		WHERE id = $1 AND status = 'scheduled'`, id)
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrStatus
	}
	if _, err := c.exec(ctx, `
		UPDATE attempts SET deadline = CASE WHEN deadline < now() THEN deadline ELSE now() END
		WHERE assessment_id = $1 AND status = 'in_progress'`, id); err != nil {
		return err
	}
	return tx.Commit()
}

// MarkPublished records Penlumen School's answer to the scores.
func (s *Store) MarkPublished(ctx context.Context, id string, report any) error {
	b, err := json.Marshal(report)
	if err != nil {
		return err
	}
	_, err = s.c().exec(ctx, `
		UPDATE assessments SET status = 'published', published_at = now(),
			publish_report = $2, updated_at = now()
		WHERE id = $1`, id, string(b))
	return err
}

// ---------- Questions ----------

type Question struct {
	ID       string
	Kind     string // objective or theory (its section's kind)
	Position int
	Body     string
	Options  []string
	Answer   *int // index into Options; nil for theory
	Marks    float64
	// Guide tells the marker what earns marks (theory).
	Guide string
}

func (q Question) IsTheory() bool { return q.Kind == "theory" }

func (s *Store) Questions(ctx context.Context, assessmentID string) ([]Question, error) {
	rows, err := s.c().query(ctx, `
		SELECT q.id, s.kind, q.position, q.body, q.options, q.answer, q.marks,
		       coalesce(q.marking_guide, '')
		FROM questions q JOIN sections s ON s.id = q.section_id
		WHERE s.assessment_id = $1
		ORDER BY s.position, q.position`, assessmentID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Question
	for rows.Next() {
		var q Question
		if err := rows.Scan(&q.ID, &q.Kind, &q.Position, &q.Body, (*jsonList)(&q.Options), &q.Answer, &q.Marks, &q.Guide); err != nil {
			return nil, err
		}
		if q.Options == nil {
			q.Options = []string{}
		}
		out = append(out, q)
	}
	return out, rows.Err()
}

// AddQuestion adds a question at the end of its section (objective or
// theory, by q.Kind). Only drafts change.
func (s *Store) AddQuestion(ctx context.Context, assessmentID string, q Question) error {
	var options any
	if q.Kind != "theory" {
		options = jsonList(q.Options)
	}
	n, err := s.c().exec(ctx, `
		INSERT INTO questions (id, section_id, position, body, options, answer, marks, marking_guide)
		SELECT $8, s.id,
		       coalesce((SELECT max(position) FROM questions WHERE section_id = s.id), 0) + 1,
		       $2, $3, $4, $5, nullif($7, '')
		FROM sections s JOIN assessments a ON a.id = s.assessment_id
		WHERE s.assessment_id = $1 AND s.kind = $6 AND a.status = 'draft'
		ORDER BY s.position LIMIT 1`,
		assessmentID, q.Body, options, q.Answer, q.Marks, kindOr(q.Kind), q.Guide, newID())
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrStatus
	}
	return nil
}

// UpdateQuestion saves an edited question (its text, options, right answer,
// marks and marking guide; not its section). Only drafts change.
func (s *Store) UpdateQuestion(ctx context.Context, assessmentID string, q Question) error {
	var options any
	if !q.IsTheory() {
		options = jsonList(q.Options)
	}
	n, err := s.c().exec(ctx, `
		UPDATE questions SET body = $3, options = $4, answer = $5, marks = $6, marking_guide = nullif($7, '')
		WHERE id = $2 AND section_id IN (
			SELECT s.id FROM sections s JOIN assessments a ON a.id = s.assessment_id
			WHERE a.id = $1 AND a.status = 'draft')`,
		assessmentID, q.ID, q.Body, options, q.Answer, q.Marks, q.Guide)
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrStatus
	}
	return nil
}

// Question finds one of an assessment's questions.
func (s *Store) Question(ctx context.Context, assessmentID, questionID string) (Question, error) {
	qs, err := s.Questions(ctx, assessmentID)
	if err != nil {
		return Question{}, err
	}
	for _, q := range qs {
		if q.ID == questionID {
			return q, nil
		}
	}
	return Question{}, ErrNotFound
}

func kindOr(kind string) string {
	if kind == "theory" {
		return "theory"
	}
	return "objective"
}

func (s *Store) DeleteQuestion(ctx context.Context, assessmentID, questionID string) error {
	n, err := s.c().exec(ctx, `
		DELETE FROM questions
		WHERE id = $2 AND section_id IN (
			SELECT s.id FROM sections s JOIN assessments a ON a.id = s.assessment_id
			WHERE a.id = $1 AND a.status = 'draft')`, assessmentID, questionID)
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrStatus
	}
	return nil
}

// ---------- Attempts ----------

type Attempt struct {
	ID            string
	AssessmentID  string
	StudentUUID   string
	StudentName   string
	RegNumber     string
	ClassUUID     string
	StartedAt     time.Time
	Deadline      time.Time
	SubmittedAt   *time.Time
	Status        string // in_progress, submitted or expired
	QuestionOrder []string
	// Score is the marks so far: objective marks plus any theory a teacher
	// has marked.
	Score        *float64
	LastSeenAt   *time.Time
	ExtraMinutes int
}

// Open is true while answers are still accepted.
func (a Attempt) Open(now time.Time) bool {
	return a.Status == "in_progress" && now.Before(a.Deadline.Add(Grace))
}

// Finished is true once submitted or timed out.
func (a Attempt) Finished() bool { return a.Status != "in_progress" }

const attemptColumns = `id, assessment_id, student_uuid, student_name, reg_number,
	class_uuid, started_at, deadline, submitted_at, status, question_order, score,
	last_seen_at, extra_minutes`

// attemptFields are where attemptColumns scan into.
func attemptFields(a *Attempt) []any {
	return []any{&a.ID, &a.AssessmentID, &a.StudentUUID, &a.StudentName, &a.RegNumber,
		&a.ClassUUID, &a.StartedAt, &a.Deadline, &a.SubmittedAt, &a.Status,
		(*jsonList)(&a.QuestionOrder), &a.Score, &a.LastSeenAt, &a.ExtraMinutes}
}

func scanAttempt(row scanner) (Attempt, error) {
	var a Attempt
	err := row.Scan(attemptFields(&a)...)
	if errors.Is(err, sql.ErrNoRows) {
		return a, ErrNotFound
	}
	return a, err
}

func collectAttempts(rows *sql.Rows, err error) ([]Attempt, error) {
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Attempt
	for rows.Next() {
		a, err := scanAttempt(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// StartAttempt creates the student's attempt, or returns the one they
// already have: starting twice never resets the timer.
func (s *Store) StartAttempt(ctx context.Context, a Attempt) (Attempt, error) {
	_, err := s.c().exec(ctx, `
		INSERT INTO attempts (id, assessment_id, student_uuid, student_name, reg_number,
			class_uuid, deadline, question_order, last_seen_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, now())
		ON CONFLICT (assessment_id, student_uuid) DO NOTHING`,
		newID(), a.AssessmentID, a.StudentUUID, a.StudentName, a.RegNumber, a.ClassUUID,
		a.Deadline, jsonList(a.QuestionOrder))
	if err != nil {
		return Attempt{}, err
	}
	return s.AttemptFor(ctx, a.AssessmentID, a.StudentUUID)
}

func (s *Store) AttemptFor(ctx context.Context, assessmentID, studentUUID string) (Attempt, error) {
	return scanAttempt(s.c().row(ctx, `SELECT `+attemptColumns+` FROM attempts
		WHERE assessment_id = $1 AND student_uuid = $2`, assessmentID, studentUUID))
}

func (s *Store) Attempt(ctx context.Context, id string) (Attempt, error) {
	return scanAttempt(s.c().row(ctx, `SELECT `+attemptColumns+` FROM attempts
		WHERE id = $1`, id))
}

func (s *Store) Attempts(ctx context.Context, assessmentID string) ([]Attempt, error) {
	return collectAttempts(s.c().query(ctx, `SELECT `+attemptColumns+` FROM attempts
		WHERE assessment_id = $1 ORDER BY student_name`, assessmentID))
}

// AttemptsForStudent maps assessment id to the student's attempt.
func (s *Store) AttemptsForStudent(ctx context.Context, studentUUID string) (map[string]Attempt, error) {
	list, err := collectAttempts(s.c().query(ctx, `SELECT `+attemptColumns+` FROM attempts
		WHERE student_uuid = $1`, studentUUID))
	if err != nil {
		return nil, err
	}
	out := map[string]Attempt{}
	for _, a := range list {
		out[a.AssessmentID] = a
	}
	return out, nil
}

type Response struct {
	Answer  *int    // objective: the chosen option
	Text    *string // theory: the written answer
	Flagged bool
	// Set once marked (automatically, or by a teacher for theory).
	Score   *float64
	Comment string
}

// SaveResponse keeps the latest answer to one question. It fails with
// ErrClosed once the attempt is submitted or past its deadline (plus Grace),
// or if the question isn't on the student's paper.
func (s *Store) SaveResponse(ctx context.Context, attemptID, questionID string, r Response) error {
	var order jsonList
	err := s.c().row(ctx, `SELECT question_order FROM attempts WHERE id = $1`, attemptID).Scan(&order)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrClosed
	}
	if err != nil {
		return err
	}
	onPaper := false
	for _, id := range order {
		onPaper = onPaper || id == questionID
	}
	if !onPaper {
		return ErrClosed
	}
	n, err := s.c().exec(ctx, `
		INSERT INTO responses (attempt_id, question_id, answer, text_answer, flagged, saved_at)
		SELECT t.id, $2, $3, $4, $5, now()
		FROM attempts t
		WHERE t.id = $1 AND t.status = 'in_progress' AND t.deadline >= $6
		ON CONFLICT (attempt_id, question_id)
		DO UPDATE SET answer = excluded.answer, text_answer = excluded.text_answer,
		              flagged = excluded.flagged, saved_at = now()`,
		attemptID, questionID, r.Answer, r.Text, r.Flagged, time.Now().Add(-Grace))
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrClosed
	}
	_, err = s.c().exec(ctx, `UPDATE attempts SET last_seen_at = now() WHERE id = $1`, attemptID)
	return err
}

func (s *Store) Responses(ctx context.Context, attemptID string) (map[string]Response, error) {
	rows, err := s.c().query(ctx, `SELECT question_id, answer, text_answer, flagged, score, coalesce(comment, '')
		FROM responses WHERE attempt_id = $1`, attemptID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]Response{}
	for rows.Next() {
		var id string
		var r Response
		if err := rows.Scan(&id, &r.Answer, &r.Text, &r.Flagged, &r.Score, &r.Comment); err != nil {
			return nil, err
		}
		out[id] = r
	}
	return out, rows.Err()
}

// FinishAttempt records the automatic marks and closes the attempt. status
// is "submitted" (the student pressed Submit) or "expired" (time ran out).
// Theory answers left blank get 0; written ones wait for a teacher. It does
// nothing if the attempt is already finished.
func (s *Store) FinishAttempt(ctx context.Context, attemptID, status string, marks map[string]float64) error {
	tx, c, err := s.begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	// Finished now, or at the deadline if time had already run out.
	n, err := c.exec(ctx, `
		UPDATE attempts SET status = $2,
			submitted_at = CASE WHEN deadline >= $3 THEN now() ELSE deadline END
		WHERE id = $1 AND status = 'in_progress'`, attemptID, status, time.Now().Add(-Grace))
	if err != nil {
		return err
	}
	if n == 0 {
		return nil
	}
	for questionID, mark := range marks {
		if _, err := c.exec(ctx, `UPDATE responses SET score = $3
			WHERE attempt_id = $1 AND question_id = $2`, attemptID, questionID, mark); err != nil {
			return err
		}
	}
	if _, err := c.exec(ctx, `
		UPDATE responses SET score = 0
		WHERE attempt_id = $1 AND trim(coalesce(text_answer, '')) = ''
		  AND question_id IN (SELECT q.id FROM questions q JOIN sections s ON s.id = q.section_id
		                      WHERE s.kind = 'theory')`, attemptID); err != nil {
		return err
	}
	if err := recomputeScore(ctx, c, attemptID); err != nil {
		return err
	}
	return tx.Commit()
}

// recomputeScore sets an attempt's score to the sum of its marked answers.
func recomputeScore(ctx context.Context, c conn, attemptID string) error {
	_, err := c.exec(ctx, `UPDATE attempts SET score =
		(SELECT coalesce(sum(score), 0) FROM responses WHERE attempt_id = $1)
		WHERE id = $1`, attemptID)
	return err
}

package store

import (
	"context"
	"errors"

	"database/sql"
)

// Script is one finished attempt as a marker sees it: numbered, not named,
// so marking is fair.
type Script struct {
	AttemptID string
	Number    int
	Pending   int // written theory answers not marked yet
	Theory    int // theory answers written
}

// Scripts lists finished attempts that have theory answers, numbered in the
// order students started.
func (s *Store) Scripts(ctx context.Context, assessmentID string) ([]Script, error) {
	rows, err := s.c().query(ctx, `
		SELECT t.id,
		       row_number() OVER (ORDER BY t.started_at, t.id),
		       count(*) FILTER (WHERE r.score IS NULL),
		       count(r.question_id)
		FROM attempts t
		LEFT JOIN responses r ON r.attempt_id = t.id
		  AND trim(coalesce(r.text_answer, '')) <> ''
		  AND r.question_id IN (SELECT q.id FROM questions q JOIN sections s ON s.id = q.section_id
		                        WHERE s.assessment_id = t.assessment_id AND s.kind = 'theory')
		WHERE t.assessment_id = $1 AND t.status <> 'in_progress'
		GROUP BY t.id, t.started_at
		ORDER BY t.started_at, t.id`, assessmentID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Script
	for rows.Next() {
		var sc Script
		if err := rows.Scan(&sc.AttemptID, &sc.Number, &sc.Pending, &sc.Theory); err != nil {
			return nil, err
		}
		if sc.Theory == 0 {
			sc.Pending = 0
		}
		out = append(out, sc)
	}
	return out, rows.Err()
}

// Unmarked counts written theory answers waiting for a teacher.
func (s *Store) Unmarked(ctx context.Context, assessmentID string) (int, error) {
	var n int
	err := s.c().row(ctx, `
		SELECT count(*) FROM responses r
		JOIN attempts t ON t.id = r.attempt_id
		JOIN questions q ON q.id = r.question_id
		JOIN sections s ON s.id = q.section_id
		WHERE t.assessment_id = $1 AND t.status <> 'in_progress'
		  AND s.kind = 'theory' AND r.score IS NULL
		  AND trim(coalesce(r.text_answer, '')) <> ''`, assessmentID).Scan(&n)
	return n, err
}

// SaveMark records a teacher's mark for one theory answer and updates the
// attempt's total. The attempt must be finished.
func (s *Store) SaveMark(ctx context.Context, attemptID, questionID string, score float64, comment, by string) error {
	tx, c, err := s.begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var status string
	err = c.row(ctx, `SELECT status FROM attempts WHERE id = $1 {{forUpdate}}`, attemptID).Scan(&status)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	if status == "in_progress" {
		return ErrStatus
	}
	n, err := c.exec(ctx, `
		UPDATE responses SET score = $3, comment = nullif($4, ''), marked_by = $5
		WHERE attempt_id = $1 AND question_id = $2`, attemptID, questionID, score, comment, by)
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrNotFound
	}
	if err := recomputeScore(ctx, c, attemptID); err != nil {
		return err
	}
	return tx.Commit()
}

// QuestionStat is how a class did on one question, for reports.
type QuestionStat struct {
	QuestionID string
	Answered   int     // attempts that answered it
	Right      int     // objective: got it right
	MarksSum   float64 // marks given, for the average
	Marked     int     // answers with a mark
}

func (s *Store) QuestionStats(ctx context.Context, assessmentID string) (map[string]QuestionStat, error) {
	rows, err := s.c().query(ctx, `
		SELECT q.id,
		       count(r.question_id) FILTER (WHERE r.answer IS NOT NULL OR trim(coalesce(r.text_answer, '')) <> ''),
		       count(*) FILTER (WHERE r.score IS NOT NULL AND r.score >= q.marks),
		       coalesce(sum(r.score), 0),
		       count(r.score)
		FROM questions q
		JOIN sections s ON s.id = q.section_id
		LEFT JOIN responses r ON r.question_id = q.id
		  AND r.attempt_id IN (SELECT id FROM attempts WHERE assessment_id = $1 AND status <> 'in_progress')
		WHERE s.assessment_id = $1
		GROUP BY q.id`, assessmentID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]QuestionStat{}
	for rows.Next() {
		var st QuestionStat
		if err := rows.Scan(&st.QuestionID, &st.Answered, &st.Right, &st.MarksSum, &st.Marked); err != nil {
			return nil, err
		}
		out[st.QuestionID] = st
	}
	return out, rows.Err()
}

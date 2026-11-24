package store

import (
	"context"
	"time"

	"github.com/penlumen/penlumen.ams/internal/db"
)

// Progress is one attempt as the live monitor shows it.
type Progress struct {
	Attempt
	Answered  int        // questions with an answer
	LastSaved *time.Time // the last answer saved
	LeftPage  int        // times the student left the exam page
	// Camera, sound and screen monitoring:
	Noise      int  // times talking or noise was heard
	DevicesOff int  // times the camera, microphone or screen sharing stopped
	Snapshots  int  // photos taken
	Monitored  bool // camera, microphone and screen came on at least once
}

func (s *Store) Progress(ctx context.Context, assessmentID string) ([]Progress, error) {
	rows, err := s.c().query(ctx, `
		SELECT `+attemptColumns+`,
		  (SELECT count(*) FROM responses r WHERE r.attempt_id = t.id
		     AND (r.answer IS NOT NULL OR trim(coalesce(r.text_answer, '')) <> '')),
		  (SELECT max(saved_at) FROM responses r WHERE r.attempt_id = t.id),
		  (SELECT count(*) FROM monitoring e WHERE e.attempt_id = t.id AND e.kind = 'left_page'),
		  (SELECT count(*) FROM monitoring e WHERE e.attempt_id = t.id AND e.kind = 'noise'),
		  (SELECT count(*) FROM monitoring e WHERE e.attempt_id = t.id
		     AND e.kind IN ('camera_off', 'mic_off', 'screen_off')),
		  (SELECT count(*) FROM snapshots n WHERE n.attempt_id = t.id),
		  EXISTS (SELECT 1 FROM monitoring e WHERE e.attempt_id = t.id AND e.kind = 'monitoring_on')
		FROM attempts t WHERE t.assessment_id = $1`, assessmentID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Progress
	for rows.Next() {
		var p Progress
		fields := append(attemptFields(&p.Attempt), &p.Answered, sqlTime{&p.LastSaved}, &p.LeftPage,
			&p.Noise, &p.DevicesOff, &p.Snapshots, &p.Monitored)
		if err := rows.Scan(fields...); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// Seen records that the exam screen checked in.
func (s *Store) Seen(ctx context.Context, attemptID string) error {
	_, err := s.c().exec(ctx, `UPDATE attempts SET last_seen_at = now()
		WHERE id = $1 AND status = 'in_progress'`, attemptID)
	return err
}

// Event records something the invigilator should know about: kind is
// left_page or returned.
func (s *Store) Event(ctx context.Context, attemptID, kind string) error {
	_, err := s.c().exec(ctx, `
		INSERT INTO monitoring (attempt_id, kind)
		SELECT id, $2 FROM attempts WHERE id = $1 AND status = 'in_progress'`, attemptID, kind)
	if err != nil {
		return err
	}
	return s.Seen(ctx, attemptID)
}

// ReloadWindow is how soon after "left the page" a reload of the same tab
// takes it back.
const ReloadWindow = time.Minute

// Reloaded takes back the "left the page" a page reload sent as it
// unloaded: the newest one, if it's recent and the student hasn't come
// back since (a real leave they returned from stays).
func (s *Store) Reloaded(ctx context.Context, attemptID string) error {
	_, err := s.c().exec(ctx, `
		DELETE FROM monitoring WHERE id = (
			SELECT e.id FROM monitoring e
			JOIN attempts t ON t.id = e.attempt_id AND t.status = 'in_progress'
			WHERE e.attempt_id = $1 AND e.kind = 'left_page'
			  AND e.at > $2
			  AND NOT EXISTS (SELECT 1 FROM monitoring r
			                  WHERE r.attempt_id = e.attempt_id AND r.kind = 'returned' AND r.id > e.id)
			ORDER BY e.id DESC LIMIT 1)`, attemptID, time.Now().Add(-ReloadWindow))
	if err != nil {
		return err
	}
	return s.Seen(ctx, attemptID)
}

// ResetAttempt clears an attempt's answers, marks and alerts and starts it
// again with a fresh deadline, whatever state it was in (even after the
// exam ended), so the student can sit it again straight away.
func (s *Store) ResetAttempt(ctx context.Context, attemptID string, deadline time.Time) error {
	tx, c, err := s.begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	n, err := c.exec(ctx, `
		UPDATE attempts SET status = 'in_progress', started_at = now(), deadline = $2,
			submitted_at = NULL, score = NULL, last_seen_at = NULL, extra_minutes = 0
		WHERE id = $1`, attemptID, deadline)
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrNotFound
	}
	for _, q := range []string{
		`DELETE FROM responses WHERE attempt_id = $1`,
		`DELETE FROM monitoring WHERE attempt_id = $1`,
		`DELETE FROM snapshots WHERE attempt_id = $1`,
	} {
		if _, err := c.exec(ctx, q, attemptID); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// DeleteAttempt removes an attempt with its answers, marks and alerts, as
// if the student never started.
func (s *Store) DeleteAttempt(ctx context.Context, attemptID string) error {
	n, err := s.c().exec(ctx, `DELETE FROM attempts WHERE id = $1`, attemptID)
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// AddTime gives students still writing more minutes: one attempt, or all of
// the assessment's when attemptID is empty. Returns how many got it.
func (s *Store) AddTime(ctx context.Context, assessmentID, attemptID string, minutes int) (int, error) {
	later := "deadline + make_interval(mins => $2)"
	if s.dialect == db.SQLite {
		later = "strftime('%Y-%m-%d %H:%M:%f', deadline, '+' || $2 || ' minutes')"
	}
	query := `UPDATE attempts SET deadline = ` + later + `, extra_minutes = extra_minutes + $2
		WHERE assessment_id = $1 AND status = 'in_progress'`
	args := []any{assessmentID, minutes}
	if attemptID != "" {
		query += ` AND id = $3`
		args = append(args, attemptID)
	}
	n, err := s.c().exec(ctx, query, args...)
	if err != nil {
		return 0, err
	}
	return int(n), nil
}

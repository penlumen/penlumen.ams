package store

import (
	"context"
	"errors"
	"time"

	"database/sql"
)

// Events the exam screen reports, and what the invigilator sees. Anything
// else is refused.
var EventKinds = map[string]string{
	"left_page":     "Left the exam page",
	"returned":      "Came back to the exam page",
	"noise":         "Talking or noise",
	"camera_off":    "Camera turned off",
	"mic_off":       "Microphone turned off",
	"screen_off":    "Screen sharing stopped",
	"monitoring_on": "Camera, microphone and screen on",
}

// SnapshotReasons are why a photo may be taken.
var SnapshotReasons = map[string]bool{
	"start": true, "left_page": true, "noise": true,
	"camera_off": true, "mic_off": true, "screen_off": true,
}

type Snapshot struct {
	ID      int64
	Source  string // camera or screen
	Reason  string
	TakenAt time.Time
}

// AddSnapshot keeps a photo for an attempt still being written.
func (s *Store) AddSnapshot(ctx context.Context, attemptID, source, reason string, image []byte) error {
	n, err := s.c().exec(ctx, `
		INSERT INTO snapshots (attempt_id, source, reason, image)
		SELECT id, $2, $3, $4 FROM attempts
		WHERE id = $1 AND status = 'in_progress'
		  AND deadline >= $5`,
		attemptID, source, reason, image, time.Now().Add(-Grace))
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrClosed
	}
	return nil
}

// Snapshots lists an attempt's photos, oldest first (without the images).
func (s *Store) Snapshots(ctx context.Context, attemptID string) ([]Snapshot, error) {
	rows, err := s.c().query(ctx, `SELECT id, source, reason, taken_at FROM snapshots
		WHERE attempt_id = $1 ORDER BY taken_at, id`, attemptID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Snapshot
	for rows.Next() {
		var sn Snapshot
		if err := rows.Scan(&sn.ID, &sn.Source, &sn.Reason, &sn.TakenAt); err != nil {
			return nil, err
		}
		out = append(out, sn)
	}
	return out, rows.Err()
}

// SnapshotImage is one photo and the attempt it belongs to.
func (s *Store) SnapshotImage(ctx context.Context, id int64) ([]byte, string, error) {
	var img []byte
	var attemptID string
	err := s.c().row(ctx, `SELECT image, attempt_id FROM snapshots WHERE id = $1`, id).Scan(&img, &attemptID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, "", ErrNotFound
	}
	return img, attemptID, err
}

// HasEvent reports whether an attempt has had an event of this kind.
func (s *Store) HasEvent(ctx context.Context, attemptID, kind string) (bool, error) {
	var ok bool
	err := s.c().row(ctx, `SELECT EXISTS (SELECT 1 FROM monitoring
		WHERE attempt_id = $1 AND kind = $2)`, attemptID, kind).Scan(&ok)
	return ok, err
}

type Event struct {
	Kind string
	At   time.Time
}

// Events lists what the exam screen reported for an attempt, oldest first.
func (s *Store) Events(ctx context.Context, attemptID string) ([]Event, error) {
	rows, err := s.c().query(ctx, `SELECT kind, at FROM monitoring
		WHERE attempt_id = $1 ORDER BY at, id`, attemptID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Event
	for rows.Next() {
		var e Event
		if err := rows.Scan(&e.Kind, &e.At); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// Purge deletes photos and alerts of assessments that closed before
// `before`, keeping attempts, answers and marks. It returns how many photos
// and alerts went.
func (s *Store) Purge(ctx context.Context, before time.Time) (int64, int64, error) {
	old := `attempt_id IN (SELECT t.id FROM attempts t JOIN assessments a ON a.id = t.assessment_id
	                        WHERE a.closes_at < $1)`
	snaps, err := s.c().exec(ctx, `DELETE FROM snapshots WHERE `+old, before)
	if err != nil {
		return 0, 0, err
	}
	events, err := s.c().exec(ctx, `DELETE FROM monitoring WHERE `+old, before)
	if err != nil {
		return 0, 0, err
	}
	return snaps, events, nil
}

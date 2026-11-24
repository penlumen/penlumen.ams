package store

import (
	"context"
	"database/sql"
	"errors"
)

// Pictures in questions (uploaded by staff) and in written answers
// (uploaded by students), linked from the text as /images/{id}. They go
// with their assessment or attempt when that is deleted.

// MaxImage is the biggest picture kept, in bytes.
const MaxImage = 2 << 20

// Image says what a picture belongs to, to check who may see it.
type Image struct {
	ID           string
	AssessmentID string
	AttemptID    string // set for pictures in an answer
	ContentType  string
}

// AddQuestionImage keeps a picture for an assessment's questions.
func (s *Store) AddQuestionImage(ctx context.Context, assessmentID, by, contentType string, data []byte) (string, error) {
	id := newID()
	_, err := s.c().exec(ctx, `
		INSERT INTO images (id, assessment_id, uploaded_by, content_type, data)
		VALUES ($1, $2, $3, $4, $5)`, id, assessmentID, by, contentType, data)
	return id, err
}

// AddAnswerImage keeps a picture for a written answer, while the attempt can
// still be changed.
func (s *Store) AddAnswerImage(ctx context.Context, attemptID, by, contentType string, data []byte) (string, error) {
	id := newID()
	n, err := s.c().exec(ctx, `
		INSERT INTO images (id, assessment_id, attempt_id, uploaded_by, content_type, data)
		SELECT $1, assessment_id, id, $3, $4, $5 FROM attempts
		WHERE id = $2 AND status = 'in_progress' AND deadline >= $6`,
		id, attemptID, by, contentType, data, nowMinusGrace())
	if err != nil {
		return "", err
	}
	if n == 0 {
		return "", ErrClosed
	}
	return id, nil
}

// ImageInfo finds a picture without loading it.
func (s *Store) ImageInfo(ctx context.Context, id string) (Image, error) {
	var im Image
	err := s.c().row(ctx, `SELECT id, assessment_id, coalesce(CAST(attempt_id AS TEXT), ''), content_type
		FROM images WHERE id = $1`, id).Scan(&im.ID, &im.AssessmentID, &im.AttemptID, &im.ContentType)
	if errors.Is(err, sql.ErrNoRows) {
		return im, ErrNotFound
	}
	return im, err
}

func (s *Store) ImageData(ctx context.Context, id string) ([]byte, error) {
	var data []byte
	err := s.c().row(ctx, `SELECT data FROM images WHERE id = $1`, id).Scan(&data)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return data, err
}

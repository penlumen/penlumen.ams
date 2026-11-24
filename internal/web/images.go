package web

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"

	"github.com/penlumen/penlumen.ams/internal/auth"
	"github.com/penlumen/penlumen.ams/internal/store"
)

// Pictures in questions and written answers. The editor (static/editor.js)
// uploads one and puts its /images/{id} address in the text.

// imageTypes are the pictures kept, by what their bytes say they are (never
// SVG, which can carry scripts).
var imageTypes = map[string]bool{
	"image/png": true, "image/jpeg": true, "image/gif": true, "image/webp": true,
}

// readImage reads an upload's bytes and checks they're a picture.
func readImage(w http.ResponseWriter, r *http.Request) ([]byte, string, string) {
	data, err := io.ReadAll(http.MaxBytesReader(w, r.Body, store.MaxImage))
	if err != nil {
		return nil, "", "Pictures can be up to 2 MB."
	}
	kind := http.DetectContentType(data)
	if !imageTypes[kind] {
		return nil, "", "Use a PNG, JPEG, GIF or WebP picture."
	}
	return data, kind, ""
}

func sendImageURL(w http.ResponseWriter, id string) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"url": "/images/" + id})
}

// uploadQuestionImage keeps a picture for one of an assessment's questions.
func (s *Server) uploadQuestionImage(w http.ResponseWriter, r *http.Request, user *auth.User) {
	r.Header.Set("Accept", "application/json")
	a, ok := s.loadForStaff(w, r, user)
	if !ok {
		return
	}
	data, kind, problem := readImage(w, r)
	if problem != "" {
		s.fail(w, r, http.StatusBadRequest, problem)
		return
	}
	id, err := s.store.AddQuestionImage(r.Context(), a.ID, user.Sub, kind, data)
	if err != nil {
		s.failErr(w, r, err)
		return
	}
	sendImageURL(w, id)
}

// uploadAnswerImage keeps a picture in a student's written answer, while
// they can still change it.
func (s *Server) uploadAnswerImage(w http.ResponseWriter, r *http.Request, user *auth.User) {
	r.Header.Set("Accept", "application/json")
	at, ok := s.loadAttempt(w, r, user)
	if !ok {
		return
	}
	data, kind, problem := readImage(w, r)
	if problem != "" {
		s.fail(w, r, http.StatusBadRequest, problem)
		return
	}
	id, err := s.store.AddAnswerImage(r.Context(), at.ID, user.Sub, kind, data)
	if errors.Is(err, store.ErrClosed) {
		s.fail(w, r, http.StatusConflict, "Time is up: no more changes.")
		return
	}
	if err != nil {
		s.failErr(w, r, err)
		return
	}
	sendImageURL(w, id)
}

// image shows a picture to staff who can open its assessment, and to
// students: their own answers' pictures, and a question's once they've
// started that exam.
func (s *Server) image(w http.ResponseWriter, r *http.Request, user *auth.User) {
	im, err := s.store.ImageInfo(r.Context(), r.PathValue("id"))
	if err == nil {
		err = s.mayView(r, user, im)
	}
	if err != nil {
		http.NotFound(w, r)
		return
	}
	data, err := s.store.ImageData(r.Context(), im.ID)
	if err != nil {
		s.failErr(w, r, err)
		return
	}
	h := w.Header()
	h.Set("Content-Type", im.ContentType)
	h.Set("Content-Length", strconv.Itoa(len(data)))
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Content-Security-Policy", "default-src 'none'")
	// A picture never changes; its address is new for every upload.
	h.Set("Cache-Control", "private, max-age=31536000, immutable")
	w.Write(data)
}

func (s *Server) mayView(r *http.Request, user *auth.User, im store.Image) error {
	if user.IsStaff() {
		a, err := s.store.Assessment(r.Context(), im.AssessmentID)
		if err != nil {
			return err
		}
		if a.SchoolUUID != user.SchoolUUID || !contains(user.BranchUUIDs, a.BranchUUID) {
			return store.ErrNotFound
		}
		return nil
	}
	if im.AttemptID != "" {
		at, err := s.store.Attempt(r.Context(), im.AttemptID)
		if err != nil || at.StudentUUID != user.Sub {
			return store.ErrNotFound
		}
		return nil
	}
	if _, err := s.store.AttemptFor(r.Context(), im.AssessmentID, user.Sub); err != nil {
		return store.ErrNotFound
	}
	return nil
}

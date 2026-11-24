package web

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/penlumen/penlumen.ams/internal/auth"
	"github.com/penlumen/penlumen.ams/internal/store"
)

// KeepEvidence is how long photos and alerts are kept after an exam closes.
const KeepEvidence = 30 * 24 * time.Hour

// maxPhoto is the largest photo accepted (the exam screen sends ~30–80 KB).
const maxPhoto = 400 << 10

// snapshot receives a photo from the exam screen of a monitored exam.
func (s *Server) snapshot(w http.ResponseWriter, r *http.Request, user *auth.User) {
	r.Header.Set("Accept", "application/json")
	at, ok := s.loadAttempt(w, r, user)
	if !ok {
		return
	}
	a, err := s.store.Assessment(r.Context(), at.AssessmentID)
	if err != nil {
		s.failErr(w, r, err)
		return
	}
	source, reason := r.URL.Query().Get("source"), r.URL.Query().Get("reason")
	if !a.Settings.Proctoring || (source != "camera" && source != "screen") || !store.SnapshotReasons[reason] {
		s.fail(w, r, http.StatusBadRequest, "Bad photo.")
		return
	}
	img, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxPhoto))
	// JPEG files start FF D8 FF; anything else isn't kept.
	if err != nil || len(img) < 3 || !bytes.HasPrefix(img, []byte{0xFF, 0xD8, 0xFF}) {
		s.fail(w, r, http.StatusBadRequest, "Bad photo.")
		return
	}
	if err := s.store.AddSnapshot(r.Context(), at.ID, source, reason, img); err != nil {
		s.failErr(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

type evidenceItem struct {
	At     time.Time
	Label  string
	Photos []store.Snapshot
}

// evidence shows one student's alerts and the photos taken with them.
func (s *Server) evidence(w http.ResponseWriter, r *http.Request, user *auth.User) {
	a, ok := s.loadForStaff(w, r, user)
	if !ok {
		return
	}
	at, err := s.store.Attempt(r.Context(), r.PathValue("attempt"))
	if err != nil || at.AssessmentID != a.ID {
		s.failErr(w, r, store.ErrNotFound)
		return
	}
	events, err := s.store.Events(r.Context(), at.ID)
	if err != nil {
		s.failErr(w, r, err)
		return
	}
	snaps, err := s.store.Snapshots(r.Context(), at.ID)
	if err != nil {
		s.failErr(w, r, err)
		return
	}
	// The start photo first, then each alert with the photos taken within a
	// few seconds of it.
	var items []evidenceItem
	used := map[int64]bool{}
	for _, sn := range snaps {
		if sn.Reason == "start" {
			items = append(items, evidenceItem{At: sn.TakenAt, Label: "Start of the exam", Photos: []store.Snapshot{sn}})
			used[sn.ID] = true
		}
	}
	for _, e := range events {
		if e.Kind == "returned" {
			continue
		}
		item := evidenceItem{At: e.At, Label: store.EventKinds[e.Kind]}
		for _, sn := range snaps {
			if !used[sn.ID] && sn.Reason == e.Kind && sn.TakenAt.Sub(e.At) > -10*time.Second && sn.TakenAt.Sub(e.At) < 15*time.Second {
				item.Photos = append(item.Photos, sn)
				used[sn.ID] = true
			}
		}
		items = append(items, item)
	}
	for _, sn := range snaps {
		if !used[sn.ID] {
			items = append(items, evidenceItem{At: sn.TakenAt, Label: store.EventKinds[sn.Reason], Photos: []store.Snapshot{sn}})
		}
	}
	s.render(w, r, http.StatusOK, "evidence.html", map[string]any{
		"A": a, "Attempt": at, "Items": items, "Section": "monitor",
		"DeleteOn": a.ClosesAt.Add(KeepEvidence),
	})
}

// snapshotImage serves one photo to staff of the assessment's branch.
func (s *Server) snapshotImage(w http.ResponseWriter, r *http.Request, user *auth.User) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	img, attemptID, err := s.store.SnapshotImage(r.Context(), id)
	if err != nil {
		s.failErr(w, r, err)
		return
	}
	at, err := s.store.Attempt(r.Context(), attemptID)
	if err != nil {
		s.failErr(w, r, err)
		return
	}
	a, err := s.store.Assessment(r.Context(), at.AssessmentID)
	if err != nil || a.SchoolUUID != user.SchoolUUID || !contains(user.BranchUUIDs, a.BranchUUID) {
		s.failErr(w, r, store.ErrNotFound)
		return
	}
	w.Header().Set("Content-Type", "image/jpeg")
	w.Header().Set("Cache-Control", "private, max-age=3600")
	w.Write(img)
}

// StartCleanup deletes photos and alerts of exams that closed more than
// KeepEvidence ago, now and then every hour, until ctx ends.
func (s *Server) StartCleanup(ctx context.Context) {
	clean := func() {
		snaps, events, err := s.store.Purge(ctx, s.now().Add(-KeepEvidence))
		if err != nil {
			slog.Error("deleting old monitoring photos", "error", err)
			return
		}
		if snaps+events > 0 {
			slog.Info("deleted old monitoring photos and alerts", "photos", snaps, "alerts", events)
		}
	}
	go func() {
		clean()
		tick := time.NewTicker(time.Hour)
		defer tick.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
				clean()
			}
		}
	}()
}

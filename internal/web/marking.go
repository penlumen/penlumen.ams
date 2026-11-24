package web

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/penlumen/penlumen.ams/internal/auth"
	"github.com/penlumen/penlumen.ams/internal/marking"
	"github.com/penlumen/penlumen.ams/internal/store"
)

type markingRow struct {
	assessmentRow
	Scripts, Pending int
}

// markingList shows every assessment with written answers to mark.
func (s *Server) markingList(w http.ResponseWriter, r *http.Request, user *auth.User) {
	list, err := s.store.AssessmentsForStaff(r.Context(), user.SchoolUUID, user.BranchUUIDs)
	if err != nil {
		s.failErr(w, r, err)
		return
	}
	names := s.classNames(r.Context())
	now := s.now()
	var rows []markingRow
	for _, a := range list {
		if a.Status == "draft" {
			continue
		}
		if err := s.finishExpired(r.Context(), a); err != nil {
			s.failErr(w, r, err)
			return
		}
		scripts, err := s.store.Scripts(r.Context(), a.ID)
		if err != nil {
			s.failErr(w, r, err)
			return
		}
		row := markingRow{assessmentRow: assessmentRow{Assessment: a, Window: a.Window(now), Class: className(a.ClassUUID, names)}}
		for _, sc := range scripts {
			if sc.Theory > 0 {
				row.Scripts++
				row.Pending += sc.Pending
			}
		}
		if row.Scripts > 0 {
			rows = append(rows, row)
		}
	}
	s.render(w, r, http.StatusOK, "marking_list.html", map[string]any{"Rows": rows, "Section": "marking"})
}

type markItem struct {
	store.Question
	Text    string
	Score   *float64
	Comment string
}

func (s *Server) marking(w http.ResponseWriter, r *http.Request, user *auth.User) {
	a, ok := s.loadForStaff(w, r, user)
	if !ok {
		return
	}
	if err := s.finishExpired(r.Context(), a); err != nil {
		s.failErr(w, r, err)
		return
	}
	all, err := s.store.Scripts(r.Context(), a.ID)
	if err != nil {
		s.failErr(w, r, err)
		return
	}
	var scripts []store.Script
	pending := 0
	for _, sc := range all {
		if sc.Theory > 0 {
			scripts = append(scripts, sc)
			pending += sc.Pending
		}
	}

	// The script asked for, else the first with work left, else the first.
	var current *store.Script
	want := r.URL.Query().Get("script")
	for i := range scripts {
		if scripts[i].AttemptID == want {
			current = &scripts[i]
		}
	}
	for i := range scripts {
		if current == nil && scripts[i].Pending > 0 {
			current = &scripts[i]
		}
	}
	if current == nil && len(scripts) > 0 {
		current = &scripts[0]
	}

	data := map[string]any{
		"A": a, "Scripts": scripts, "Current": current, "Pending": pending,
		"Section": "marking", "Notice": r.URL.Query().Get("notice"),
	}
	if current != nil {
		at, err := s.store.Attempt(r.Context(), current.AttemptID)
		if err != nil {
			s.failErr(w, r, err)
			return
		}
		items, objScore, objMax, err := s.markItems(r, a, current.AttemptID)
		if err != nil {
			s.failErr(w, r, err)
			return
		}
		data["Items"], data["ObjScore"], data["ObjMax"], data["Attempt"] = items, objScore, objMax, at
	}
	s.render(w, r, http.StatusOK, "marking.html", data)
}

// markItems are the written answers in one script, plus its objective score.
func (s *Server) markItems(r *http.Request, a store.Assessment, attemptID string) ([]markItem, float64, float64, error) {
	questions, err := s.store.Questions(r.Context(), a.ID)
	if err != nil {
		return nil, 0, 0, err
	}
	responses, err := s.store.Responses(r.Context(), attemptID)
	if err != nil {
		return nil, 0, 0, err
	}
	var items []markItem
	var objScore, objMax float64
	for _, q := range questions {
		resp := responses[q.ID]
		if !q.IsTheory() {
			objMax += q.Marks
			if resp.Score != nil {
				objScore += *resp.Score
			}
			continue
		}
		item := markItem{Question: q, Score: resp.Score, Comment: resp.Comment}
		if resp.Text != nil {
			item.Text = *resp.Text
		}
		items = append(items, item)
	}
	return items, objScore, objMax, nil
}

func (s *Server) saveMarks(w http.ResponseWriter, r *http.Request, user *auth.User) {
	a, ok := s.loadForStaff(w, r, user)
	if !ok {
		return
	}
	attemptID := r.PathValue("attempt")
	at, err := s.store.Attempt(r.Context(), attemptID)
	if err != nil || at.AssessmentID != a.ID {
		s.failErr(w, r, store.ErrNotFound)
		return
	}
	if err := r.ParseForm(); err != nil {
		s.fail(w, r, http.StatusBadRequest, "The form couldn't be read.")
		return
	}
	items, _, _, err := s.markItems(r, a, attemptID)
	if err != nil {
		s.failErr(w, r, err)
		return
	}
	here := "/marking/" + a.ID + "?script=" + attemptID
	for _, item := range items {
		if strings.TrimSpace(item.Text) == "" {
			continue // blank answers already got 0
		}
		raw := strings.TrimSpace(r.PostFormValue("score_" + item.ID))
		if raw == "" {
			continue // not marked yet
		}
		score, err := strconv.ParseFloat(raw, 64)
		if err != nil || score < 0 || score > item.Marks {
			back(w, r, here, fmt.Sprintf("A mark must be from 0 to %s.", strconv.FormatFloat(item.Marks, 'f', -1, 64)))
			return
		}
		comment := strings.TrimSpace(r.PostFormValue("comment_" + item.ID))
		if len(comment) > 1000 {
			comment = comment[:1000]
		}
		if err := s.store.SaveMark(r.Context(), attemptID, item.ID, marking.Round2(score), comment, user.Sub); err != nil {
			s.failErr(w, r, err)
			return
		}
	}

	// On to the next script with work left.
	scripts, err := s.store.Scripts(r.Context(), a.ID)
	if err != nil {
		s.failErr(w, r, err)
		return
	}
	for _, sc := range scripts {
		if sc.Pending > 0 && sc.AttemptID != attemptID {
			back(w, r, "/marking/"+a.ID+"?script="+sc.AttemptID, "Marks saved.")
			return
		}
	}
	back(w, r, here, "Marks saved. Every script is marked.")
}

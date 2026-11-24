package web

import (
	"net/http"
	"sort"

	"github.com/penlumen/penlumen.ams/internal/auth"
	"github.com/penlumen/penlumen.ams/internal/store"
)

// PassMark is the percentage counted as a pass in reports.
const PassMark = 50

type summary struct {
	Sat                int
	Average, High, Low float64 // percentages
	Passed             int
	PassRate           int
	Bands              []band
}

// band is a grade band, as on Nigerian report cards.
type band struct {
	Label      string
	From, To   int
	Count, Pct int
}

func summarise(a store.Assessment, attempts []store.Attempt) summary {
	bands := []band{{Label: "70 – 100", From: 70, To: 100}, {Label: "60 – 69", From: 60, To: 69},
		{Label: "50 – 59", From: 50, To: 59}, {Label: "40 – 49", From: 40, To: 49}, {Label: "0 – 39", From: 0, To: 39}}
	var sm summary
	var total float64
	for _, at := range attempts {
		if at.Score == nil || !at.Finished() {
			continue
		}
		p := float64(percent(*at.Score, a.TotalMarks))
		if sm.Sat == 0 || p > sm.High {
			sm.High = p
		}
		if sm.Sat == 0 || p < sm.Low {
			sm.Low = p
		}
		sm.Sat++
		total += p
		if p >= PassMark {
			sm.Passed++
		}
		for i := range bands {
			if int(p) >= bands[i].From && int(p) <= bands[i].To {
				bands[i].Count++
			}
		}
	}
	if sm.Sat > 0 {
		sm.Average = float64(int(total/float64(sm.Sat)*10+0.5)) / 10
		sm.PassRate = percent(float64(sm.Passed), float64(sm.Sat))
		for i := range bands {
			bands[i].Pct = percent(float64(bands[i].Count), float64(sm.Sat))
		}
	}
	sm.Bands = bands
	return sm
}

type reportRow struct {
	assessmentRow
	summary
}

func (s *Server) reportsList(w http.ResponseWriter, r *http.Request, user *auth.User) {
	list, err := s.store.AssessmentsForStaff(r.Context(), user.SchoolUUID, user.BranchUUIDs)
	if err != nil {
		s.failErr(w, r, err)
		return
	}
	names := s.classNames(r.Context())
	now := s.now()
	var rows []reportRow
	for _, a := range list {
		if a.Status == "draft" {
			continue
		}
		attempts, err := s.store.Attempts(r.Context(), a.ID)
		if err != nil {
			s.failErr(w, r, err)
			return
		}
		sm := summarise(a, attempts)
		if sm.Sat == 0 {
			continue
		}
		rows = append(rows, reportRow{
			assessmentRow: assessmentRow{Assessment: a, Window: a.Window(now), Class: className(a.ClassUUID, names)},
			summary:       sm,
		})
	}
	s.render(w, r, http.StatusOK, "reports_list.html", map[string]any{"Rows": rows, "Section": "reports", "PassMark": PassMark})
}

type questionReport struct {
	store.Question
	Number int
	Stat   store.QuestionStat
	// Right is the share who got an objective question right; Average the
	// average mark as a share of the question's marks (theory).
	Right, Average int
}

func (s *Server) report(w http.ResponseWriter, r *http.Request, user *auth.User) {
	a, ok := s.loadForStaff(w, r, user)
	if !ok {
		return
	}
	if err := s.finishExpired(r.Context(), a); err != nil {
		s.failErr(w, r, err)
		return
	}
	attempts, err := s.store.Attempts(r.Context(), a.ID)
	if err != nil {
		s.failErr(w, r, err)
		return
	}
	questions, err := s.store.Questions(r.Context(), a.ID)
	if err != nil {
		s.failErr(w, r, err)
		return
	}
	stats, err := s.store.QuestionStats(r.Context(), a.ID)
	if err != nil {
		s.failErr(w, r, err)
		return
	}
	sm := summarise(a, attempts)
	var qs []questionReport
	for i, q := range questions {
		st := stats[q.ID]
		qr := questionReport{Question: q, Number: i + 1, Stat: st}
		qr.Right = percent(float64(st.Right), float64(sm.Sat))
		if st.Marked > 0 {
			qr.Average = percent(st.MarksSum/float64(st.Marked), q.Marks)
		}
		qs = append(qs, qr)
	}
	// The hardest questions are the useful ones to discuss with a class.
	hardest := append([]questionReport(nil), qs...)
	sort.SliceStable(hardest, func(i, j int) bool { return score(hardest[i]) < score(hardest[j]) })
	if len(hardest) > 3 {
		hardest = hardest[:3]
	}

	finished := attempts[:0:0]
	for _, at := range attempts {
		if at.Finished() && at.Score != nil {
			finished = append(finished, at)
		}
	}
	sort.SliceStable(finished, func(i, j int) bool { return *finished[i].Score > *finished[j].Score })
	s.render(w, r, http.StatusOK, "report.html", map[string]any{
		"A": a, "Window": a.Window(s.now()), "Class": className(a.ClassUUID, s.classNames(r.Context())),
		"Summary": sm, "Questions": qs, "Hardest": hardest, "Ranked": finished,
		"Section": "reports", "PassMark": PassMark,
	})
}

func score(q questionReport) int {
	if q.IsTheory() {
		return q.Average
	}
	return q.Right
}

package web

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"sort"
	"strconv"
	"time"

	"github.com/penlumen/penlumen.ams/internal/auth"
	"github.com/penlumen/penlumen.ams/internal/store"
)

// OnlineWithin is how recently the exam screen must have checked in for the
// monitor to call a student online (it checks in every 30 seconds).
const OnlineWithin = 75 * time.Second

type monitorRow struct {
	Name, RegNumber string
	State           string // not_started, writing, offline, submitted, expired
	Progress        *store.Progress
	Attention       bool
}

func (s *Server) monitorList(w http.ResponseWriter, r *http.Request, user *auth.User) {
	list, err := s.store.AssessmentsForStaff(r.Context(), user.SchoolUUID, user.BranchUUIDs)
	if err != nil {
		s.failErr(w, r, err)
		return
	}
	names := s.classNames(r.Context())
	now := s.now()
	var live, soon []assessmentRow
	for _, a := range list {
		row := assessmentRow{Assessment: a, Window: a.Window(now), Class: className(a.ClassUUID, names)}
		switch row.Window {
		case "open":
			live = append(live, row)
		case "upcoming":
			soon = append(soon, row)
		}
	}
	sort.Slice(soon, func(i, j int) bool { return soon[i].OpensAt.Before(soon[j].OpensAt) })
	s.render(w, r, http.StatusOK, "monitor_list.html", map[string]any{"Live": live, "Soon": soon, "Section": "monitor"})
}

// monitorData is everything the monitor page and its refresh show.
func (s *Server) monitorData(ctx context.Context, a store.Assessment) (map[string]any, error) {
	if err := s.finishExpired(ctx, a); err != nil {
		return nil, err
	}
	progress, err := s.store.Progress(ctx, a.ID)
	if err != nil {
		return nil, err
	}
	now := s.now()
	byStudent := map[string]*store.Progress{}
	for i := range progress {
		byStudent[progress[i].StudentUUID] = &progress[i]
	}

	var rows []monitorRow
	seen := map[string]bool{}
	students, err := s.dir.Students(ctx, a.ClassUUID)
	if err != nil {
		slog.Warn("penlumen school: students", "error", err)
	}
	for _, st := range students {
		seen[st.UUID] = true
		rows = append(rows, s.monitorRow(st.Name, st.RegNumber, byStudent[st.UUID], now))
	}
	// Anyone who sat it but has since moved class still shows.
	for i := range progress {
		if !seen[progress[i].StudentUUID] {
			rows = append(rows, s.monitorRow(progress[i].StudentName, progress[i].RegNumber, &progress[i], now))
		}
	}

	order := map[string]int{"offline": 0, "writing": 1, "not_started": 2, "expired": 3, "submitted": 4}
	sort.SliceStable(rows, func(i, j int) bool {
		if rows[i].Attention != rows[j].Attention {
			return rows[i].Attention
		}
		if order[rows[i].State] != order[rows[j].State] {
			return order[rows[i].State] < order[rows[j].State]
		}
		return rows[i].Name < rows[j].Name
	})
	counts := map[string]int{}
	for _, row := range rows {
		counts[row.State]++
	}
	left := a.ClosesAt.Sub(now)
	return map[string]any{
		"A":        a,
		"Window":   a.Window(now),
		"Rows":     rows,
		"Counts":   counts,
		"TimeLeft": formatLeft(left),
		"Total":    a.QuestionCount,
		"Updated":  now,
		"Section":  "monitor",
	}, nil
}

func (s *Server) monitorRow(name, reg string, p *store.Progress, now time.Time) monitorRow {
	row := monitorRow{Name: name, RegNumber: reg, State: "not_started", Progress: p}
	if p == nil {
		return row
	}
	switch {
	case p.Status == "submitted":
		row.State = "submitted"
	case p.Status == "expired":
		row.State = "expired"
	case p.LastSeenAt != nil && now.Sub(*p.LastSeenAt) <= OnlineWithin:
		row.State = "writing"
	default:
		row.State = "offline"
	}
	row.Attention = row.State == "offline" || (p.Status == "in_progress" && (p.LeftPage > 0 || p.Noise > 0 || p.DevicesOff > 0))
	return row
}

func formatLeft(d time.Duration) string {
	if d <= 0 {
		return "00:00:00"
	}
	d = d.Round(time.Second)
	if days := int(d.Hours()) / 24; days >= 2 {
		return fmt.Sprintf("%d days", days)
	}
	return fmt.Sprintf("%02d:%02d:%02d", int(d.Hours()), int(d.Minutes())%60, int(d.Seconds())%60)
}

func (s *Server) monitor(w http.ResponseWriter, r *http.Request, user *auth.User) {
	a, ok := s.loadForStaff(w, r, user)
	if !ok {
		return
	}
	data, err := s.monitorData(r.Context(), a)
	if err != nil {
		s.failErr(w, r, err)
		return
	}
	data["Notice"] = r.URL.Query().Get("notice")
	s.render(w, r, http.StatusOK, "monitor.html", data)
}

// monitorLive is the part of the monitor page that refreshes itself.
func (s *Server) monitorLive(w http.ResponseWriter, r *http.Request, user *auth.User) {
	a, ok := s.loadForStaff(w, r, user)
	if !ok {
		return
	}
	data, err := s.monitorData(r.Context(), a)
	if err != nil {
		s.failErr(w, r, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	s.renderBlock(w, r, http.StatusOK, "monitor.html", "live", data)
}

func (s *Server) addTime(w http.ResponseWriter, r *http.Request, user *auth.User) {
	a, ok := s.loadForStaff(w, r, user)
	if !ok {
		return
	}
	minutes, err := strconv.Atoi(r.PostFormValue("minutes"))
	if err != nil || minutes < 1 || minutes > 120 {
		back(w, r, "/monitor/"+a.ID, "Choose from 1 to 120 minutes.")
		return
	}
	n, err := s.store.AddTime(r.Context(), a.ID, r.PostFormValue("attempt"), minutes)
	if err != nil {
		s.failErr(w, r, err)
		return
	}
	back(w, r, "/monitor/"+a.ID, fmt.Sprintf("Gave %d %s %d more %s.", n, plural(n, "student", "students"), minutes, plural(minutes, "minute", "minutes")))
}

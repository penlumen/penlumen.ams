// Package schoolsync copies the school from Penlumen School into AMS's own
// database: the school (name, theme, branches), classes, terms, students and
// staff. It runs on start, every Interval, and straight away when Penlumen
// School's webhooks say something changed. When Penlumen School can't be
// reached, AMS keeps working from the last copy.
//
// Penlumen School wins for its own people: their names, classes and roles
// are overwritten, and people removed there can no longer sign in. Their
// AMS password or PIN is never touched, and people added in AMS are left
// alone (or merged, when Penlumen School later has the same person).
package schoolsync

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/penlumen/penlumen.ams/internal/directory"
	"github.com/penlumen/penlumen.ams/internal/sms"
	"github.com/penlumen/penlumen.ams/internal/store"
)

// Interval is how often to sync without a webhook.
const Interval = 10 * time.Minute

// Source is the part of the Penlumen School client the sync reads.
type Source interface {
	School(ctx context.Context) (sms.School, error)
	Classes(ctx context.Context) ([]sms.Class, error)
	Terms(ctx context.Context, branchUUID string) ([]sms.Term, error)
	AllStudents(ctx context.Context) ([]sms.Student, error)
	Staff(ctx context.Context) ([]sms.StaffMember, error)
}

type Syncer struct {
	src   Source
	store *store.Store
	kick  chan struct{}

	mu   sync.Mutex
	last Status
}

// Status is how the last sync went, for the People page.
type Status struct {
	At    time.Time
	Error string
}

func New(src Source, st *store.Store) *Syncer {
	return &Syncer{src: src, store: st, kick: make(chan struct{}, 1)}
}

// Now asks for a sync soon (without waiting for it).
func (s *Syncer) Now() {
	select {
	case s.kick <- struct{}{}:
	default: // one is already waiting
	}
}

func (s *Syncer) Status() Status {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.last
}

// Start syncs now and then every Interval, or when asked, until ctx ends.
func (s *Syncer) Start(ctx context.Context) {
	go func() {
		t := time.NewTicker(Interval)
		defer t.Stop()
		for {
			s.run(ctx)
			select {
			case <-ctx.Done():
				return
			case <-t.C:
			case <-s.kick:
			}
		}
	}()
}

func (s *Syncer) run(ctx context.Context) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	s.SyncAndRecord(ctx)
}

// SyncAndRecord syncs once and keeps how it went for Status.
func (s *Syncer) SyncAndRecord(ctx context.Context) error {
	err := s.Sync(ctx)
	st := Status{At: time.Now()}
	if err != nil {
		st.Error = err.Error()
		slog.Warn("sync with penlumen school failed; using the last copy", "error", err)
	}
	s.mu.Lock()
	s.last = st
	s.mu.Unlock()
	return err
}

// Sync copies everything once. Nothing is removed unless the whole read
// worked, so a failure halfway never empties AMS.
func (s *Syncer) Sync(ctx context.Context) error {
	school, err := s.src.School(ctx)
	if err != nil {
		return err
	}
	classes, err := s.src.Classes(ctx)
	if err != nil {
		return err
	}
	terms, err := s.src.Terms(ctx, "")
	if err != nil {
		return err
	}
	students, err := s.src.AllStudents(ctx)
	if err != nil {
		return err
	}
	staff, err := s.src.Staff(ctx)
	if err != nil {
		return err
	}

	if err := s.saveSchool(ctx, school); err != nil {
		return err
	}
	var seen []string
	for _, c := range classes {
		teachers := map[string]string{}
		for _, st := range c.SubjectTeachers {
			if st.TeacherUUID != nil && *st.TeacherUUID != "" {
				teachers[st.Name] = *st.TeacherUUID
			}
		}
		if _, err := s.store.SaveClass(ctx, store.Class{UUID: c.UUID, Name: c.Name, BranchUUID: c.BranchUUID,
			Subjects: c.Subjects, Teachers: teachers, Source: store.SourceSMS}); err != nil {
			return err
		}
		seen = append(seen, c.UUID)
	}
	if err := s.store.PruneSynced(ctx, "classes", seen); err != nil {
		return err
	}
	seen = nil
	for _, t := range terms {
		if _, err := s.store.SaveTerm(ctx, store.Term{UUID: t.UUID, BranchUUID: t.BranchUUID, Session: t.Session,
			Term: t.Term, Status: t.Status, Source: store.SourceSMS}); err != nil {
			return err
		}
		seen = append(seen, t.UUID)
	}
	if err := s.store.PruneSynced(ctx, "terms", seen); err != nil {
		return err
	}

	seen = nil
	for _, st := range students {
		p := store.Person{UUID: st.UUID, Role: "student", Name: st.Name, RegNumber: st.RegNumber,
			ClassUUID: st.Class.UUID, BranchUUIDs: []string{st.BranchUUID}, Status: st.Status}
		if err := s.store.SyncPerson(ctx, p); err != nil {
			return err
		}
		seen = append(seen, st.UUID)
	}
	if _, err := s.store.DeactivateMissing(ctx, "student", seen); err != nil {
		return err
	}
	seen = nil
	for _, m := range staff {
		role := "staff"
		if m.Role == "admin" {
			role = "admin"
		}
		p := store.Person{UUID: m.UUID, Role: role, Name: m.Name, Email: m.Email, BranchUUIDs: m.BranchUUIDs,
			Position: strings.ToLower(m.Position)}
		if err := s.store.SyncPerson(ctx, p); err != nil {
			return err
		}
		seen = append(seen, m.UUID)
	}
	_, err = s.store.DeactivateMissing(ctx, "staff", seen)
	return err
}

// saveSchool keeps Penlumen School's school. A school that ran on its own
// first keeps its assessments, classes and terms: they move to Penlumen
// School's school and (first) branch.
func (s *Syncer) saveSchool(ctx context.Context, school sms.School) error {
	var old sms.School
	err := s.store.Setting(ctx, directory.SchoolKey, &old)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return err
	}
	if err == nil && old.UUID != school.UUID && old.Slug == StandaloneSlug {
		if err := s.store.MoveSchool(ctx, old.UUID, school.UUID); err != nil {
			return err
		}
		if len(old.Branches) > 0 && len(school.Branches) > 0 {
			if err := s.store.MoveBranch(ctx, old.Branches[0].UUID, school.Branches[0].UUID); err != nil {
				return err
			}
		}
	}
	return s.store.SetSetting(ctx, directory.SchoolKey, school)
}

// StandaloneSlug marks a school set up in AMS without Penlumen School.
const StandaloneSlug = "ams-standalone"

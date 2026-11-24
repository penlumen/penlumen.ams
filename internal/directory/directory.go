// Package directory is what pages read about the school: the school and its
// theme and branches, classes with their subjects, terms, and a class's
// students. It reads AMS's own copy (package store), which package sync keeps
// in step with Penlumen School, so pages work when Penlumen School is down
// and in a school that runs AMS on its own.
package directory

import (
	"context"
	"errors"

	"github.com/penlumen/penlumen.ams/internal/sms"
	"github.com/penlumen/penlumen.ams/internal/store"
)

// SchoolKey is the setting holding the school (sms.School as JSON).
const SchoolKey = "school"

// ErrNoSchool means the school hasn't been set up: no sync with Penlumen
// School yet, and no stand-alone setup.
var ErrNoSchool = errors.New("the school isn't set up yet")

type Directory struct {
	store *store.Store
	// changed is told when Penlumen School says something changed (its
	// webhooks), to sync now.
	changed func()
}

func New(st *store.Store, changed func()) *Directory {
	if changed == nil {
		changed = func() {}
	}
	return &Directory{store: st, changed: changed}
}

// School is the school, its theme and branches.
func (d *Directory) School(ctx context.Context) (sms.School, error) {
	var s sms.School
	err := d.store.Setting(ctx, SchoolKey, &s)
	if errors.Is(err, store.ErrNotFound) {
		return s, ErrNoSchool
	}
	return s, err
}

// Classes are every class, with their subjects.
func (d *Directory) Classes(ctx context.Context) ([]sms.Class, error) {
	list, err := d.store.Classes(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]sms.Class, len(list))
	for i, c := range list {
		out[i] = sms.Class{UUID: c.UUID, Name: c.Name, BranchUUID: c.BranchUUID, Subjects: c.Subjects}
		for _, name := range c.Subjects {
			st := sms.SubjectTeacher{Name: name}
			if t, ok := c.Teachers[name]; ok {
				st.TeacherUUID = &t
			}
			out[i].SubjectTeachers = append(out[i].SubjectTeachers, st)
		}
	}
	return out, nil
}

// Terms of every branch.
func (d *Directory) Terms(ctx context.Context) ([]sms.Term, error) {
	list, err := d.store.Terms(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]sms.Term, len(list))
	for i, t := range list {
		out[i] = sms.Term{UUID: t.UUID, BranchUUID: t.BranchUUID, Session: t.Session, Term: t.Term, Status: t.Status}
	}
	return out, nil
}

// Students in one class (active ones).
func (d *Directory) Students(ctx context.Context, classUUID string) ([]sms.Student, error) {
	list, err := d.store.Students(ctx, classUUID)
	if err != nil {
		return nil, err
	}
	out := make([]sms.Student, len(list))
	for i, p := range list {
		out[i] = sms.Student{UUID: p.UUID, Name: p.Name, RegNumber: p.RegNumber, Status: "active"}
		out[i].Class.UUID = p.ClassUUID
	}
	return out, nil
}

// Forget is called by Penlumen School's webhooks when something changed:
// it syncs now instead of waiting for the next regular sync.
func (d *Directory) Forget(string) { d.changed() }

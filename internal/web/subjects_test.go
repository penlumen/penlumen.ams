package web

import (
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/penlumen/penlumen.ams/internal/auth"
)

// Teachers (academic staff) set assessments only for the subjects assigned
// to them in Penlumen School; administrative staff and administrators for
// any.
func TestTeachersSetOnlyTheirSubjects(t *testing.T) {
	h := newHarness(t)
	office := auth.User{Sub: "office-1", Name: "Ms Bursar", Role: "staff", SchoolUUID: "school-1", BranchUUIDs: []string{"branch-1"}}
	head := auth.User{Sub: "admin-1", Name: "Mr Head", Role: "admin", SchoolUUID: "school-1", BranchUUIDs: []string{"branch-1"}}
	form := func(class, subject string) url.Values {
		return url.Values{"title": {"Test"}, "kind": {"ca"}, "class": {class}, "subject": {subject}, "term": {"term-1"},
			"report_marks": {"10"}, "duration": {"20"},
			"opens_at": {h.now.Add(time.Hour).Format(formTime)}, "closes_at": {h.now.Add(2 * time.Hour).Format(formTime)}}
	}

	// Mrs Adebayo teaches JSS 2A's subjects only: JSS 2B isn't offered.
	_, page, _ := h.do(&teacher, "GET", "/assessments/new", nil)
	mustContain(t, "her form", page, "JSS 2A", "The classes and subjects you teach.")
	if strings.Contains(page, "JSS 2B") {
		t.Error("JSS 2B is offered to a teacher who doesn't teach it")
	}
	code, page, _ := h.do(&teacher, "POST", "/assessments", form("class-2b", "Mathematics"))
	expect(t, "a class she doesn't teach", code, 422)
	code, _, _ = h.do(&teacher, "POST", "/assessments", form("class-2a", "Mathematics"))
	expect(t, "her subject", code, 303)

	// Administrative staff and administrators: any subject.
	for _, u := range []auth.User{office, head} {
		_, page, _ = h.do(&u, "GET", "/assessments/new", nil)
		mustContain(t, u.Name+"'s form", page, "JSS 2A", "JSS 2B")
		code, _, _ = h.do(&u, "POST", "/assessments", form("class-2b", "Mathematics"))
		expect(t, u.Name+" sets JSS 2B Mathematics", code, 303)
	}

	// The People page shows who teaches what.
	_, page, _ = h.do(&head, "GET", "/people?tab=classes", nil)
	mustContain(t, "classes", page, "Basic Science · <span class=\"text-fg-2\">Mrs Adebayo</span>", "no teacher yet")
	_, page, _ = h.do(&head, "GET", "/people?tab=staff", nil)
	mustContain(t, "staff", page, "Administrative staff", "Teacher", "Administrator")

	// A teacher with no subjects is told why there's nothing to choose.
	_, page, _ = h.do(&other, "GET", "/assessments/new", nil)
	mustContain(t, "no subjects", page, "No subjects are assigned to you yet.")
}

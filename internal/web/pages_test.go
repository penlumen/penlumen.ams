package web

import "testing"

// Every page template parses (a typo would otherwise only show when the
// page is first opened).
func TestPagesParse(t *testing.T) {
	s := New(Options{BaseURL: "http://app.test"})
	for _, page := range []string{
		"login.html", "setup.html", "people.html", "person_form.html", "error.html", "student_home.html", "take_start.html", "take_exam.html", "take_done.html",
		"staff_home.html", "assessment_form.html", "assessment_edit.html", "submissions.html",
		"monitor_list.html", "monitor.html", "marking_list.html", "marking.html",
		"reports_list.html", "report.html", "seb_required.html", "seb_quit.html", "evidence.html",
	} {
		if s.pages[page] == nil {
			t.Errorf("no page %s", page)
		}
	}
}

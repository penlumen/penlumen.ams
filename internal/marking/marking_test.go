package marking

import "testing"

func ptr(n int) *int { return &n }

func TestObjective(t *testing.T) {
	questions := []Question{
		{ID: "q1", Marks: 3, Answer: ptr(0)},
		{ID: "q2", Marks: 3, Answer: ptr(2)},
		{ID: "q3", Marks: 2, Answer: ptr(1)},
		{ID: "essay", Marks: 8}, // marked by a teacher, not here
	}
	r := Objective(questions, map[string]*int{
		"q1": ptr(0), // right
		"q2": ptr(1), // wrong
		// q3 not answered
		"essay": ptr(0),
	})

	if r.Total != 3 {
		t.Errorf("Total = %v, want 3", r.Total)
	}
	if r.Max != 16 {
		t.Errorf("Max = %v, want 16 (theory counts towards the paper's total)", r.Max)
	}
	want := map[string]float64{"q1": 3, "q2": 0, "q3": 0}
	for id, mark := range want {
		if got, ok := r.PerQuestion[id]; !ok || got != mark {
			t.Errorf("PerQuestion[%s] = %v, %v; want %v", id, got, ok, mark)
		}
	}
	if _, ok := r.PerQuestion["essay"]; ok {
		t.Error("theory question was marked automatically")
	}
}

func TestToReport(t *testing.T) {
	cases := []struct {
		name                    string
		score, max, reportMarks float64
		want                    float64
	}{
		{"CA test worth 10", 24, 30, 10, 8},
		{"exam worth 60", 27, 36, 60, 45},
		{"rounds to 2 places", 1, 3, 10, 3.33},
		{"full marks", 36, 36, 60, 60},
		{"zero", 0, 36, 60, 0},
		{"empty paper", 5, 0, 10, 0},
		{"never above report marks", 40, 36, 60, 60},
	}
	for _, c := range cases {
		if got := ToReport(c.score, c.max, c.reportMarks); got != c.want {
			t.Errorf("%s: ToReport(%v, %v, %v) = %v, want %v", c.name, c.score, c.max, c.reportMarks, got, c.want)
		}
	}
}

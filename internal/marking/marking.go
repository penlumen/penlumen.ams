// Package marking scores objective answers and scales marks to the report.
// It has no database or HTTP code, so it's the easiest place to start.
package marking

import "math"

// Question is what marking needs to know about one question.
type Question struct {
	ID     string
	Marks  float64
	Answer *int // the right option; nil for questions marked by a teacher
}

// Result of marking one attempt.
type Result struct {
	// PerQuestion is the mark for each automatically marked question.
	PerQuestion map[string]float64
	// Total is the marks the student got; Max is what the paper is out of.
	Total float64
	Max   float64
}

// Objective marks the questions that have a right answer: full marks for the
// right option, nothing otherwise. answers maps question id to the chosen
// option (nil or missing means not answered).
func Objective(questions []Question, answers map[string]*int) Result {
	r := Result{PerQuestion: map[string]float64{}}
	for _, q := range questions {
		r.Max += q.Marks
		if q.Answer == nil {
			continue
		}
		mark := 0.0
		if got := answers[q.ID]; got != nil && *got == *q.Answer {
			mark = q.Marks
		}
		r.PerQuestion[q.ID] = mark
		r.Total += mark
	}
	r.Total = Round2(r.Total)
	r.Max = Round2(r.Max)
	return r
}

// ToReport scales a mark out of max to the report's scale. A 30-mark paper
// worth 10 report marks turns 24/30 into 8.
func ToReport(score, max, reportMarks float64) float64 {
	if max <= 0 || score <= 0 {
		return 0
	}
	if score > max {
		score = max
	}
	return Round2(score / max * reportMarks)
}

// Round2 rounds to two decimal places, the precision Penlumen School keeps.
func Round2(n float64) float64 {
	return math.Round(n*100) / 100
}

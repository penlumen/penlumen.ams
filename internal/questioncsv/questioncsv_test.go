package questioncsv

import (
	"bytes"
	"strings"
	"testing"

	"github.com/penlumen/penlumen.ams/internal/store"
)

func TestRead(t *testing.T) {
	file := "\xef\xbb\xbftype,question,option_a,option_b,option_c,option_d,option_e,option_f,answer,marks,marking_guide\n" +
		"objective,Which part of the cell controls it?,Nucleus,Cytoplasm,Vacuole,,,,A,2,\n" +
		"objective,SI unit of force?,Joule,,Newton,Watt,,,c,3,\n" + // gap at B: Newton is the 2nd option
		"theory,State three differences between plant and animal cells.,,,,,,,,8,Cell wall; vacuole; chloroplasts\n" +
		",,,,,,,,,,\n" + // blank row: skipped
		"objective,Only one option,Yes,,,,,,A,1,\n" +
		"essay,Wrong type,,,,,,,,5,\n" +
		"objective,Bad answer,One,Two,,,,,D,1,\n" +
		"theory,No marks,,,,,,,,,\n"

	qs, bad, err := Read(strings.NewReader(file))
	if err != nil {
		t.Fatal(err)
	}
	if len(qs) != 3 {
		t.Fatalf("got %d questions: %+v", len(qs), qs)
	}
	if *qs[0].Answer != 0 || len(qs[0].Options) != 3 || qs[0].Marks != 2 {
		t.Errorf("q1 = %+v", qs[0])
	}
	if *qs[1].Answer != 1 || qs[1].Options[1] != "Newton" {
		t.Errorf("q2: answer %d of %v, want Newton (index 1)", *qs[1].Answer, qs[1].Options)
	}
	if !qs[2].IsTheory() || qs[2].Answer != nil || qs[2].Guide != "Cell wall; vacuole; chloroplasts" || qs[2].Marks != 8 {
		t.Errorf("q3 = %+v", qs[2])
	}

	wantRows := map[int]string{6: "two options", 7: "objective", 8: "letter", 9: "marks"}
	if len(bad) != len(wantRows) {
		t.Fatalf("errors = %v", bad)
	}
	for _, e := range bad {
		if !strings.Contains(e.Message, wantRows[e.Row]) {
			t.Errorf("row %d: %q, want it to mention %q", e.Row, e.Message, wantRows[e.Row])
		}
	}
}

func TestReadNeedsHeader(t *testing.T) {
	_, _, err := Read(strings.NewReader("objective,Q?,A,B,,,,,A,1,\n"))
	if err == nil || !strings.Contains(err.Error(), "name the columns") {
		t.Fatalf("err = %v", err)
	}
}

func TestWriteThenReadGivesTheSameQuestions(t *testing.T) {
	two := 2
	in := []store.Question{
		{Kind: "objective", Body: "Pick, \"carefully\"", Options: []string{"a", "b", "c"}, Answer: &two, Marks: 1.5},
		{Kind: "theory", Body: "Explain.", Marks: 10, Guide: "Two points, 5 each"},
	}
	var buf bytes.Buffer
	if err := Write(&buf, in); err != nil {
		t.Fatal(err)
	}
	out, bad, err := Read(&buf)
	if err != nil || len(bad) > 0 {
		t.Fatalf("err %v, bad %v", err, bad)
	}
	if len(out) != 2 || out[0].Body != in[0].Body || *out[0].Answer != 2 || out[0].Marks != 1.5 ||
		out[1].Guide != in[1].Guide || out[1].Marks != 10 {
		t.Errorf("round trip: %+v", out)
	}
}

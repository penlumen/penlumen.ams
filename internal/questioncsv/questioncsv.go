// Package questioncsv reads and writes questions as CSV, for importing a
// paper a teacher typed in a spreadsheet and exporting one to share.
//
// The columns, with a header row:
//
//	type,question,option_a,option_b,option_c,option_d,option_e,option_f,answer,marks,marking_guide
//
// type is "objective" or "theory". Objective questions need two or more
// options and answer, the right option's letter (A to F). Theory questions
// leave the options and answer empty and may have a marking guide.
package questioncsv

import (
	"encoding/csv"
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/penlumen/penlumen.ams/internal/store"
)

// Header is the first row of every file.
var Header = []string{"type", "question", "option_a", "option_b", "option_c", "option_d",
	"option_e", "option_f", "answer", "marks", "marking_guide"}

// MaxRows stops an upload from flooding a paper.
const MaxRows = 500

// RowError says which row is wrong and why, in words a teacher can act on.
type RowError struct {
	Row     int // as the spreadsheet numbers it (the header is row 1)
	Message string
}

func (e RowError) Error() string { return fmt.Sprintf("row %d: %s", e.Row, e.Message) }

// Read parses a file. It returns every good question, and an error for each
// bad row, so a teacher can fix them all at once.
func Read(r io.Reader) ([]store.Question, []RowError, error) {
	cr := csv.NewReader(r)
	cr.FieldsPerRecord = -1
	cr.TrimLeadingSpace = true
	records, err := cr.ReadAll()
	if err != nil {
		return nil, nil, fmt.Errorf("that isn't a CSV file this app can read: %w", err)
	}
	if len(records) == 0 {
		return nil, nil, fmt.Errorf("the file is empty")
	}

	// Columns by name, so their order doesn't matter.
	col := map[string]int{}
	for i, name := range records[0] {
		col[strings.ToLower(strings.TrimSpace(strings.TrimPrefix(name, "\xef\xbb\xbf")))] = i
	}
	for _, need := range []string{"type", "question", "answer", "marks"} {
		if _, ok := col[need]; !ok {
			return nil, nil, fmt.Errorf("the first row must name the columns; %q is missing (download the template to see them)", need)
		}
	}
	if len(records)-1 > MaxRows {
		return nil, nil, fmt.Errorf("import at most %d questions at a time", MaxRows)
	}

	get := func(rec []string, name string) string {
		if i, ok := col[name]; ok && i < len(rec) {
			return strings.TrimSpace(rec[i])
		}
		return ""
	}

	var out []store.Question
	var bad []RowError
	for n, rec := range records[1:] {
		row := n + 2
		if strings.TrimSpace(strings.Join(rec, "")) == "" {
			continue // blank line
		}
		q := store.Question{
			Kind:  strings.ToLower(get(rec, "type")),
			Body:  get(rec, "question"),
			Guide: get(rec, "marking_guide"),
		}
		fail := func(msg string) { bad = append(bad, RowError{row, msg}) }

		if q.Kind != "objective" && q.Kind != "theory" {
			fail(`type must be "objective" or "theory"`)
			continue
		}
		if q.Body == "" {
			fail("the question is empty")
			continue
		}
		marks, err := strconv.ParseFloat(get(rec, "marks"), 64)
		if err != nil || marks <= 0 || marks > 100 {
			fail("marks must be a number from 0.5 to 100")
			continue
		}
		q.Marks = marks

		if q.Kind == "objective" {
			for _, c := range "abcdef" {
				if o := get(rec, "option_"+string(c)); o != "" {
					q.Options = append(q.Options, o)
				}
			}
			if len(q.Options) < 2 {
				fail("an objective question needs at least two options")
				continue
			}
			letter := strings.ToUpper(get(rec, "answer"))
			idx := -1
			if len(letter) == 1 {
				idx = int(letter[0] - 'A')
			}
			// The letter refers to the option columns, which may have gaps.
			pos, seen := -1, 0
			for i, c := range "abcdef" {
				if get(rec, "option_"+string(c)) == "" {
					continue
				}
				if i == idx {
					pos = seen
				}
				seen++
			}
			if pos < 0 {
				fail("answer must be the letter of a filled-in option, e.g. B")
				continue
			}
			q.Answer = &pos
		}
		out = append(out, q)
	}
	return out, bad, nil
}

// Write writes questions with the header row.
func Write(w io.Writer, qs []store.Question) error {
	cw := csv.NewWriter(w)
	if err := cw.Write(Header); err != nil {
		return err
	}
	for _, q := range qs {
		rec := make([]string, len(Header))
		rec[0] = q.Kind
		if rec[0] == "" {
			rec[0] = "objective"
		}
		rec[1] = q.Body
		if !q.IsTheory() {
			for i, o := range q.Options {
				if i < 6 {
					rec[2+i] = o
				}
			}
			if q.Answer != nil && *q.Answer >= 0 && *q.Answer < 6 {
				rec[8] = string(rune('A' + *q.Answer))
			}
		}
		rec[9] = strconv.FormatFloat(q.Marks, 'f', -1, 64)
		rec[10] = q.Guide
		if err := cw.Write(rec); err != nil {
			return err
		}
	}
	cw.Flush()
	return cw.Error()
}

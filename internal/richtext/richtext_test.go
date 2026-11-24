package richtext

import "testing"

const img = `/images/3f1c2a9e-1111-4222-8333-444455556666`

func TestSanitizeKeepsFormattingOnly(t *testing.T) {
	for in, want := range map[string]string{
		`<b>H</b><sub>2</sub>O and x<sup>2</sup>`:              `<b>H</b><sub>2</sub>O and x<sup>2</sup>`,
		`<ul><li>one</li></ul><ol><li>two</li></ol>`:           `<ul><li>one</li></ul><ol><li>two</li></ol>`,
		`<img src="` + img + `" alt="A cell">`:                 `<img src="` + img + `" alt="A cell">`,
		`<script>alert(1)</script>hi`:                          `hi`,
		`<img src=x onerror="alert(1)">`:                       ``,
		`<img src="https://evil.test/a.png">`:                  ``,
		`<img src="` + img + `?x=1">`:                          ``,
		`<a href="javascript:alert(1)">click</a>`:              `click`,
		`<p style="color:red" onclick="x()">styled</p>`:        `<p>styled</p>`,
		`<iframe src="` + img + `"></iframe><svg><g/></svg>ok`: `ok`,
		`x &lt; 5 &amp; y`:                                     `x &lt; 5 &amp; y`,
	} {
		if got := Sanitize(in); got != want {
			t.Errorf("Sanitize(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestCleanEmptiesBlankAnswers(t *testing.T) {
	for _, blank := range []string{"", "   ", "<br>", "<div><br></div>", "<p> </p>", "<script>x</script>"} {
		if got := Clean(blank); got != "" {
			t.Errorf("Clean(%q) = %q, want blank", blank, got)
		}
	}
	if got := Clean(`<img src="` + img + `">`); got == "" {
		t.Error("an answer that is only a picture was dropped")
	}
}

func TestTextAndBack(t *testing.T) {
	if got := FromText("x < 5 & y\nnext line"); got != "x &lt; 5 &amp; y<br>next line" {
		t.Errorf("FromText = %q", got)
	}
	if got := ToText(`<p>Name <b>two</b> parts:</p><ul><li>a &lt; b</li><li>c</li></ul><img src="` + img + `">`); got != "Name two parts:\na < b\nc\n[picture]" {
		t.Errorf("ToText = %q", got)
	}
	if got := Length(`<b>abc</b><br><img src="` + img + `">`); got != 4 {
		t.Errorf("Length = %d, want 4 (abc and the line break)", got)
	}
}

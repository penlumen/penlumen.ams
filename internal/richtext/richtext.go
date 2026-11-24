// Package richtext keeps formatted text safe. Questions and written answers
// are a little HTML from the editor (static/editor.js): bold, italic,
// underline, superscript and subscript, lists and pictures uploaded to this
// app. Anything else, scripts and outside links included, is removed before
// it's saved and again before it's shown, because students write answers
// that teachers open.
package richtext

import (
	"html"
	"regexp"
	"strings"

	"github.com/microcosm-cc/bluemonday"
	nethtml "golang.org/x/net/html"
	"golang.org/x/net/html/atom"
)

// ImageSrc is what a picture's address must look like: one uploaded here.
var ImageSrc = regexp.MustCompile(`^/images/[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

var policy = func() *bluemonday.Policy {
	p := bluemonday.NewPolicy()
	p.AllowElements("p", "br", "div", "b", "strong", "i", "em", "u", "sub", "sup", "ul", "ol", "li")
	p.AllowRelativeURLs(true)
	p.AllowAttrs("src").Matching(ImageSrc).OnElements("img")
	p.AllowAttrs("alt").Matching(regexp.MustCompile(`^[\p{L}\p{N}\s.,'()-]{0,200}$`)).OnElements("img")
	return p
}()

// Sanitize keeps only the formatting the editor makes.
func Sanitize(s string) string {
	return policy.Sanitize(s)
}

// Clean is Sanitize for saving: it trims, and an answer with no words and
// no pictures (an empty editor still holds a <br>) becomes "".
func Clean(s string) string {
	s = strings.TrimSpace(Sanitize(s))
	if strings.TrimSpace(text(s, false)) == "" && !strings.Contains(s, "<img") {
		return ""
	}
	return s
}

// FromText turns plain text (from a CSV file) into formatted text that
// shows the same, line breaks included.
func FromText(s string) string {
	s = strings.ReplaceAll(strings.TrimSpace(s), "\r\n", "\n")
	return strings.ReplaceAll(html.EscapeString(s), "\n", "<br>")
}

// ToText is the words without formatting, for CSV files and one-line
// summaries; pictures show as [picture].
func ToText(s string) string {
	return strings.TrimSpace(text(s, true))
}

// Length counts the characters a student wrote, not the formatting.
func Length(s string) int {
	return len([]rune(text(s, false)))
}

func text(s string, pictures bool) string {
	nodes, err := nethtml.ParseFragment(strings.NewReader(s), &nethtml.Node{Type: nethtml.ElementNode, Data: "div", DataAtom: atom.Div})
	if err != nil {
		return s
	}
	var b strings.Builder
	var walk func(n *nethtml.Node)
	walk = func(n *nethtml.Node) {
		switch {
		case n.Type == nethtml.TextNode:
			b.WriteString(n.Data)
		case n.Type == nethtml.ElementNode && n.Data == "br":
			b.WriteString("\n")
		case n.Type == nethtml.ElementNode && n.Data == "img":
			if pictures {
				b.WriteString("[picture]")
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
		if n.Type == nethtml.ElementNode {
			switch n.Data {
			case "p", "div", "li":
				if !strings.HasSuffix(b.String(), "\n") {
					b.WriteString("\n")
				}
			}
		}
	}
	for _, n := range nodes {
		walk(n)
	}
	return b.String()
}

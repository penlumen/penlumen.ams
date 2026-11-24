package web

import (
	"fmt"
	"html/template"
	"math"
	"regexp"
	"strconv"
	"strings"
)

// DefaultPrimary is Penlumen blue, used when a school hasn't picked a colour.
const DefaultPrimary = "#003884"

var hexColour = regexp.MustCompile(`^#[0-9a-fA-F]{6}$`)

// themeCSS turns a school's brand colour into the CSS variables the layout
// uses, the same way Penlumen School does (apps/web/src/lib/theme.ts), so
// both apps look like one product.
func themeCSS(primary string) template.CSS {
	if !hexColour.MatchString(primary) {
		primary = DefaultPrimary
	}
	primary = strings.ToLower(primary)
	vars := []struct{ name, value string }{
		{"--primary", primary},
		{"--primary-hover", mix(primary, "#000000", 0.15)},
		{"--on-primary", foregroundFor(primary)},
		{"--accent", mix(primary, "#ffffff", 0.9)},
		{"--accent-fg", mix(primary, "#000000", 0.1)},
		{"--side-accent", mix(primary, "#ffffff", 0.95)},
		{"--focus", mix(primary, "#ffffff", 0.4)},
		{"--hover-line", mix(primary, "#ffffff", 0.55)},
	}
	var b strings.Builder
	// html:root outranks the layout's :root defaults, which the Tailwind
	// script injects later in the page.
	b.WriteString("html:root{")
	for _, v := range vars {
		fmt.Fprintf(&b, "%s:%s;", v.name, v.value)
	}
	b.WriteString("}")
	return template.CSS(b.String())
}

type rgb [3]float64

func toRGB(hex string) rgb {
	var c rgb
	for i := range 3 {
		n, _ := strconv.ParseUint(hex[1+2*i:3+2*i], 16, 8)
		c[i] = float64(n)
	}
	return c
}

func (c rgb) hex() string {
	return fmt.Sprintf("#%02x%02x%02x", int(math.Round(c[0])), int(math.Round(c[1])), int(math.Round(c[2])))
}

// mix blends hex towards target by amount (0 keeps hex, 1 gives target).
func mix(hex, target string, amount float64) string {
	a, b := toRGB(hex), toRGB(target)
	var out rgb
	for i := range 3 {
		out[i] = a[i] + (b[i]-a[i])*amount
	}
	return out.hex()
}

func luminance(hex string) float64 {
	c := toRGB(hex)
	var l [3]float64
	for i, v := range c {
		s := v / 255
		if s <= 0.03928 {
			l[i] = s / 12.92
		} else {
			l[i] = math.Pow((s+0.055)/1.055, 2.4)
		}
	}
	return 0.2126*l[0] + 0.7152*l[1] + 0.0722*l[2]
}

func contrast(a, b string) float64 {
	hi, lo := luminance(a), luminance(b)
	if lo > hi {
		hi, lo = lo, hi
	}
	return (hi + 0.05) / (lo + 0.05)
}

// foregroundFor is white or near-black, whichever reads better on hex.
func foregroundFor(hex string) string {
	if contrast(hex, "#ffffff") >= contrast(hex, "#212121") {
		return "#ffffff"
	}
	return "#212121"
}

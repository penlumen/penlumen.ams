package web

import (
	"strings"
	"testing"
)

// The numbers here come from Penlumen School's theme.ts, so the two apps
// show the same shades.
func TestThemeMatchesPenlumenSchool(t *testing.T) {
	css := string(themeCSS("#166534")) // Forest Green
	for _, want := range []string{
		"--primary:#166534;",
		"--on-primary:#ffffff;",
		"--accent:#e8f0eb;",    // mix(primary, white, 0.9)
		"--accent-fg:#145b2f;", // mix(primary, black, 0.1)
	} {
		if !strings.Contains(css, want) {
			t.Errorf("missing %s in %s", want, css)
		}
	}
}

func TestThemeFallsBackToPenlumenBlue(t *testing.T) {
	for _, bad := range []string{"", "green", "#12345", "red;}body{display:none"} {
		if css := string(themeCSS(bad)); !strings.Contains(css, "--primary:#003884;") {
			t.Errorf("themeCSS(%q) = %s", bad, css)
		}
	}
}

func TestLightBrandColoursGetDarkText(t *testing.T) {
	if got := foregroundFor("#f2b23b"); got != "#212121" {
		t.Errorf("foregroundFor(amber) = %s", got)
	}
	if got := foregroundFor("#003884"); got != "#ffffff" {
		t.Errorf("foregroundFor(navy) = %s", got)
	}
}

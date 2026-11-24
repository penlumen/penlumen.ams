package seb

import (
	"net/http/httptest"
	"strings"
	"testing"
)

const key = "a3f5c0d2b1e4f6a7b8c9d0e1f2a3b4c5d6e7f8091a2b3c4d5e6f708192a3b4c5"

func TestCheckWithConfigKey(t *testing.T) {
	url := "https://exams.school.test/take/abc"
	req := httptest.NewRequest("GET", "/take/abc", nil)
	if Check(req, url, []string{key}) {
		t.Error("a request without the header passed")
	}

	req.Header.Set(HeaderConfigKey, Hash(url, key))
	if !Check(req, url, []string{"0000000000000000000000000000000000000000000000000000000000000000", key}) {
		t.Error("SEB's signed request was refused")
	}
	// The hash covers the address, so it can't be replayed on another page.
	if Check(req, "https://exams.school.test/take/other", []string{key}) {
		t.Error("a hash for one page passed on another")
	}
	// A user agent alone doesn't pass when a key is set.
	req2 := httptest.NewRequest("GET", "/take/abc", nil)
	req2.Header.Set("User-Agent", "Mozilla/5.0 SEB/3.7")
	if Check(req2, url, []string{key}) {
		t.Error("a fake user agent passed a keyed check")
	}
}

func TestCheckWithoutKey(t *testing.T) {
	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0) SEB/3.7.1")
	if !Check(req, "http://x/", nil) {
		t.Error("SEB's user agent was refused")
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 Chrome/130")
	if Check(req, "http://x/", nil) {
		t.Error("Chrome passed")
	}
}

func TestParseKeys(t *testing.T) {
	keys, bad := ParseKeys(strings.ToUpper(key) + "\n\n  " + key + ", not-a-key")
	if len(keys) != 1 || keys[0] != key {
		t.Errorf("keys = %v (want one, lower case, no duplicates)", keys)
	}
	if len(bad) != 1 || bad[0] != "not-a-key" {
		t.Errorf("bad = %v", bad)
	}
}

func TestLaunchAndConfig(t *testing.T) {
	if got := LaunchURL("https://exams.school.test/seb/abc.seb"); got != "sebs://exams.school.test/seb/abc.seb" {
		t.Errorf("https: %s", got)
	}
	if got := LaunchURL("http://localhost:8080/seb/abc.seb"); got != "seb://localhost:8080/seb/abc.seb" {
		t.Errorf("http: %s", got)
	}
	cfg := string(Config("https://exams.school.test/take/abc?x=1&y=2", "https://exams.school.test/seb/quit", false))
	if strings.Contains(cfg, "allowVideoCapture") {
		t.Error("camera allowed without monitoring")
	}
	if !strings.Contains(string(Config("a", "b", true)), "<key>allowVideoCapture</key>\n\t<true/>") {
		t.Error("camera not allowed with monitoring")
	}
	for _, want := range []string{
		"<key>startURL</key>\n\t<string>https://exams.school.test/take/abc?x=1&amp;y=2</string>",
		"<key>sendBrowserExamKey</key>\n\t<true/>",
		"<key>quitURL</key>\n\t<string>https://exams.school.test/seb/quit</string>",
	} {
		if !strings.Contains(cfg, want) {
			t.Errorf("config missing %q", want)
		}
	}
}

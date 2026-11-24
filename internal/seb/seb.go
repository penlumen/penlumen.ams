// Package seb lets an exam require Safe Exam Browser (SEB), the free
// locked-down exam browser (https://safeexambrowser.org): full screen, no
// other apps, no copying, no switching away.
//
// SEB adds two headers to every request it makes. AMS checks the Config Key
// one: X-SafeExamBrowser-ConfigKeyHash is the SHA-256 (hex) of the full page
// address followed by the exam's Config Key, which a teacher copies from the
// SEB Config Tool. An ordinary browser can't produce it. Without a Config
// Key, AMS can only check that the browser says it is SEB, which a
// determined student could fake.
package seb

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"fmt"
	"html"
	"net/http"
	"regexp"
	"strings"
)

// HeaderConfigKey is the header SEB signs each request with.
const HeaderConfigKey = "X-SafeExamBrowser-ConfigKeyHash"

// DownloadURL is where students get SEB.
const DownloadURL = "https://safeexambrowser.org/download_en.html"

var keyPattern = regexp.MustCompile(`^[0-9a-fA-F]{64}$`)

// ParseKeys reads Config Keys pasted one per line (or separated by commas
// or spaces). It returns the keys and any that aren't 64 hex characters.
func ParseKeys(text string) (keys, bad []string) {
	fields := strings.FieldsFunc(text, func(r rune) bool {
		return r == '\n' || r == '\r' || r == ',' || r == ' ' || r == '\t'
	})
	seen := map[string]bool{}
	for _, f := range fields {
		k := strings.ToLower(strings.TrimSpace(f))
		switch {
		case k == "":
		case !keyPattern.MatchString(k):
			bad = append(bad, f)
		case !seen[k]:
			seen[k] = true
			keys = append(keys, k)
		}
	}
	return keys, bad
}

// Hash is what SEB sends for a page address and Config Key.
func Hash(absoluteURL, configKey string) string {
	sum := sha256.Sum256([]byte(absoluteURL + configKey))
	return hex.EncodeToString(sum[:])
}

// Check reports whether r comes from Safe Exam Browser. absoluteURL is the
// address SEB requested (the app's public address plus the path and query).
// With Config Keys it checks the signed header against each; without, only
// the browser's own claim.
func Check(r *http.Request, absoluteURL string, keys []string) bool {
	if len(keys) == 0 {
		ua := r.UserAgent()
		return strings.Contains(ua, "SEB/") || strings.Contains(ua, "SafeExamBrowser")
	}
	got := strings.ToLower(r.Header.Get(HeaderConfigKey))
	if got == "" {
		return false
	}
	for _, k := range keys {
		if subtle.ConstantTimeCompare([]byte(got), []byte(Hash(absoluteURL, k))) == 1 {
			return true
		}
	}
	return false
}

// LaunchURL turns the address of a .seb file into the link that opens it in
// SEB: http becomes seb, https becomes sebs.
func LaunchURL(configURL string) string {
	switch {
	case strings.HasPrefix(configURL, "https://"):
		return "sebs://" + strings.TrimPrefix(configURL, "https://")
	case strings.HasPrefix(configURL, "http://"):
		return "seb://" + strings.TrimPrefix(configURL, "http://")
	}
	return configURL
}

// Config is an exam's SEB settings file (an XML property list, which SEB
// opens directly). It starts at startURL, signs requests so AMS can check
// them, and lets the student leave from quitURL once they've submitted.
// Schools can open it in the SEB Config Tool to change more, save it, and
// paste the Config Key the tool shows into the exam's settings.
//
// media lets the exam use the camera and microphone inside SEB (for exams
// with camera, sound and screen monitoring).
func Config(startURL, quitURL string, media bool) []byte {
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
`)
	str := func(k, v string) {
		fmt.Fprintf(&b, "\t<key>%s</key>\n\t<string>%s</string>\n", k, html.EscapeString(v))
	}
	boolean := func(k string, v bool) {
		fmt.Fprintf(&b, "\t<key>%s</key>\n\t<%t/>\n", k, v)
	}
	str("startURL", startURL)
	str("quitURL", quitURL)
	boolean("quitURLConfirm", false)
	boolean("sendBrowserExamKey", true)
	boolean("examSessionClearCookiesOnStart", true)
	boolean("allowQuit", true)
	boolean("showReloadButton", true)
	boolean("browserWindowAllowReload", true)
	boolean("allowSpellCheck", false)
	boolean("allowDictionaryLookup", false)
	boolean("enablePrivateClipboard", true)
	boolean("allowPreferencesWindow", false)
	boolean("URLFilterEnable", false)
	if media {
		boolean("browserMediaAutoplay", true)
		boolean("allowVideoCapture", true)
		boolean("allowAudioCapture", true)
	}
	b.WriteString("</dict>\n</plist>\n")
	return []byte(b.String())
}

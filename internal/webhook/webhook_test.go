package webhook

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

type forgot struct{ what []string }

func (f *forgot) Forget(what string) { f.what = append(f.what, what) }

func sign(secret string, t time.Time, body string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	fmt.Fprintf(mac, "%d.%s", t.Unix(), body)
	return fmt.Sprintf("t=%d,v1=%s", t.Unix(), hex.EncodeToString(mac.Sum(nil)))
}

func TestClassChangeClearsClasses(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	cache := &forgot{}
	h := &Handler{Secret: "whsec", Cache: cache, Now: func() time.Time { return now }}
	body := `{"event":"class.updated","data":{"uuid":"c1","subjects":["Mathematics"]}}`

	req := httptest.NewRequest(http.MethodPost, "/webhooks/ams", strings.NewReader(body))
	req.Header.Set("X-Penlumen-Signature", sign("whsec", now, body))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusNoContent {
		t.Fatalf("status %d", rec.Code)
	}
	if strings.Join(cache.what, ",") != "classes,students" {
		t.Errorf("forgot %v", cache.what)
	}
}

func TestBadSignaturesAreRefused(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	body := `{"event":"class.updated"}`
	for name, header := range map[string]string{
		"wrong secret": sign("other", now, body),
		"too old":      sign("whsec", now.Add(-10*time.Minute), body),
		"missing":      "",
		"edited body":  sign("whsec", now, `{"event":"ping"}`),
	} {
		cache := &forgot{}
		h := &Handler{Secret: "whsec", Cache: cache, Now: func() time.Time { return now }}
		req := httptest.NewRequest(http.MethodPost, "/webhooks/ams", strings.NewReader(body))
		req.Header.Set("X-Penlumen-Signature", header)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized || len(cache.what) != 0 {
			t.Errorf("%s: status %d, forgot %v", name, rec.Code, cache.what)
		}
	}
}

// Package webhook receives Penlumen School's signed webhooks and keeps the
// app's copy of school data current: a class or its subjects change, a
// student moves class, and the next page shows it.
package webhook

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Tolerance is how old a delivery may be; older ones are refused as replays.
const Tolerance = 5 * time.Minute

// Forgetter drops cached copies (directory.Directory does).
type Forgetter interface{ Forget(what string) }

type Handler struct {
	Secret string
	Cache  Forgetter
	Now    func() time.Time
}

// Verify checks the X-Penlumen-Signature header ("t=<unix>,v1=<hex>"): an
// HMAC-SHA256 of "<t>.<raw body>" with the signing secret.
func Verify(secret, header string, body []byte, now time.Time) error {
	var t, sig string
	for _, part := range strings.Split(header, ",") {
		k, v, _ := strings.Cut(strings.TrimSpace(part), "=")
		switch k {
		case "t":
			t = v
		case "v1":
			sig = v
		}
	}
	unix, err := strconv.ParseInt(t, 10, 64)
	if err != nil || sig == "" {
		return errors.New("missing or malformed signature")
	}
	if d := now.Sub(time.Unix(unix, 0)); d > Tolerance || d < -Tolerance {
		return errors.New("signature too old")
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(t + "."))
	mac.Write(body)
	want := hex.EncodeToString(mac.Sum(nil))
	if !hmac.Equal([]byte(want), []byte(sig)) {
		return errors.New("signature doesn't match")
	}
	return nil
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if h.Secret == "" {
		http.Error(w, "webhooks aren't set up (SMS_WEBHOOK_SECRET)", http.StatusServiceUnavailable)
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
	if err != nil {
		http.Error(w, "body too large", http.StatusRequestEntityTooLarge)
		return
	}
	now := time.Now
	if h.Now != nil {
		now = h.Now
	}
	if err := Verify(h.Secret, r.Header.Get("X-Penlumen-Signature"), body, now()); err != nil {
		slog.Warn("webhook refused", "error", err)
		http.Error(w, "bad signature", http.StatusUnauthorized)
		return
	}
	var event struct {
		Event string `json:"event"`
	}
	if err := json.Unmarshal(body, &event); err != nil {
		http.Error(w, "bad body", http.StatusBadRequest)
		return
	}
	switch {
	case strings.HasPrefix(event.Event, "class."):
		h.Cache.Forget("classes")
		h.Cache.Forget("students")
	case strings.HasPrefix(event.Event, "student."):
		h.Cache.Forget("students")
	case event.Event == "ping":
		h.Cache.Forget("")
	}
	slog.Info("webhook", "event", event.Event)
	w.WriteHeader(http.StatusNoContent)
}

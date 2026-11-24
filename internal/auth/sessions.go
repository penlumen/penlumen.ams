package auth

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"
)

// Sessions keeps small values in signed cookies, so the server holds no
// session state. Values can be read by the browser, so never store secrets.
type Sessions struct {
	key    []byte
	secure bool
}

func NewSessions(secret string, secure bool) *Sessions {
	return &Sessions{key: []byte(secret), secure: secure}
}

var errBadCookie = errors.New("missing, expired or tampered cookie")

type envelope struct {
	Expires int64           `json:"exp"`
	Value   json.RawMessage `json:"v"`
}

func (s *Sessions) Set(w http.ResponseWriter, name string, value any, ttl time.Duration) error {
	v, err := json.Marshal(value)
	if err != nil {
		return err
	}
	payload, err := json.Marshal(envelope{Expires: time.Now().Add(ttl).Unix(), Value: v})
	if err != nil {
		return err
	}
	encoded := base64.RawURLEncoding.EncodeToString(payload)
	http.SetCookie(w, &http.Cookie{
		Name:     name,
		Value:    encoded + "." + s.sign(encoded),
		Path:     "/",
		MaxAge:   int(ttl.Seconds()),
		HttpOnly: true,
		Secure:   s.secure,
		SameSite: http.SameSiteLaxMode,
	})
	return nil
}

func (s *Sessions) Get(r *http.Request, name string, out any) error {
	c, err := r.Cookie(name)
	if err != nil {
		return errBadCookie
	}
	encoded, sig, ok := strings.Cut(c.Value, ".")
	if !ok || !hmac.Equal([]byte(sig), []byte(s.sign(encoded))) {
		return errBadCookie
	}
	payload, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil {
		return errBadCookie
	}
	var env envelope
	if err := json.Unmarshal(payload, &env); err != nil || time.Now().Unix() > env.Expires {
		return errBadCookie
	}
	return json.Unmarshal(env.Value, out)
}

func (s *Sessions) Clear(w http.ResponseWriter, name string) {
	http.SetCookie(w, &http.Cookie{
		Name: name, Value: "", Path: "/", MaxAge: -1,
		HttpOnly: true, Secure: s.secure, SameSite: http.SameSiteLaxMode,
	})
}

func (s *Sessions) sign(data string) string {
	mac := hmac.New(sha256.New, s.key)
	mac.Write([]byte(data))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

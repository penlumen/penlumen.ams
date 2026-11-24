// Package auth signs students and staff in through Penlumen School (OpenID
// Connect, authorization code flow with PKCE) and keeps them signed in.
package auth

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"
)

const (
	// SessionCookie holds the signed-in User (tests set it directly).
	SessionCookie = "ams_session"
	loginCookie   = "ams_login"
	sessionTTL    = 12 * time.Hour
)

// User is the signed-in person, from Penlumen School's ID token.
type User struct {
	Sub         string   `json:"sub"`
	Name        string   `json:"name"`
	Email       string   `json:"email,omitempty"`
	Picture     string   `json:"picture,omitempty"`
	Role        string   `json:"penlumen_role"` // student, staff, admin or parent
	SchoolUUID  string   `json:"school_uuid"`
	BranchUUIDs []string `json:"branch_uuids"`
	RegNumber   string   `json:"reg_number,omitempty"`
	// Class is a student's class; nil for staff.
	Class *ClassRef `json:"class,omitempty"`
}

type ClassRef struct {
	UUID string `json:"uuid"`
	Name string `json:"name"`
}

func (u User) IsStudent() bool { return u.Role == "student" }
func (u User) IsStaff() bool   { return u.Role == "staff" || u.Role == "admin" }
func (u User) IsAdmin() bool   { return u.Role == "admin" }

type Auth struct {
	issuer, clientID, clientSecret, redirectURL string
	sessions                                    *Sessions

	// OnSignIn runs after a sign-in through Penlumen School, before the
	// session starts: AMS records the person, and can refuse them.
	OnSignIn func(ctx context.Context, user *User) error

	mu       sync.Mutex
	provider *oidc.Provider
}

// PortalEnabled is true when Penlumen School sign-in is set up.
func (a *Auth) PortalEnabled() bool { return a.issuer != "" && a.clientID != "" }

// SignIn starts a session for someone who signed in with an AMS password
// or PIN.
func (a *Auth) SignIn(w http.ResponseWriter, user User) error {
	return a.sessions.Set(w, SessionCookie, user, sessionTTL)
}

// SafeReturn is path if it's on this site, otherwise "/".
func SafeReturn(path string) string { return safeReturn(path) }

// New doesn't contact Penlumen School: discovery happens on the first sign
// in, so the app still starts when the school system is down.
func New(issuer, clientID, clientSecret, redirectURL string, sessions *Sessions) *Auth {
	return &Auth{
		issuer: issuer, clientID: clientID, clientSecret: clientSecret,
		redirectURL: redirectURL, sessions: sessions,
	}
}

func (a *Auth) discover(ctx context.Context) (*oidc.Provider, oauth2.Config, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.provider == nil {
		p, err := oidc.NewProvider(ctx, a.issuer)
		if err != nil {
			return nil, oauth2.Config{}, err
		}
		a.provider = p
	}
	return a.provider, oauth2.Config{
		ClientID:     a.clientID,
		ClientSecret: a.clientSecret,
		RedirectURL:  a.redirectURL,
		Endpoint:     a.provider.Endpoint(),
		Scopes:       []string{oidc.ScopeOpenID, "profile", "email"},
	}, nil
}

type pendingLogin struct {
	State    string `json:"state"`
	Nonce    string `json:"nonce"`
	Verifier string `json:"verifier"`
	ReturnTo string `json:"return_to"`
}

// Login sends the person to Penlumen School to sign in.
func (a *Auth) Login(w http.ResponseWriter, r *http.Request) {
	_, conf, err := a.discover(r.Context())
	if err != nil {
		slog.Error("sign-in: can't reach Penlumen School", "error", err)
		http.Error(w, "Penlumen School can't be reached right now. Try again in a minute.", http.StatusBadGateway)
		return
	}
	p := pendingLogin{
		State:    randomString(),
		Nonce:    randomString(),
		Verifier: oauth2.GenerateVerifier(),
		ReturnTo: safeReturn(r.URL.Query().Get("return_to")),
	}
	if err := a.sessions.Set(w, loginCookie, p, 10*time.Minute); err != nil {
		http.Error(w, "Couldn't start signing in", http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, conf.AuthCodeURL(p.State, oidc.Nonce(p.Nonce), oauth2.S256ChallengeOption(p.Verifier)), http.StatusFound)
}

// Callback is where Penlumen School sends the person back after signing in.
func (a *Auth) Callback(w http.ResponseWriter, r *http.Request) {
	var p pendingLogin
	if err := a.sessions.Get(r, loginCookie, &p); err != nil || r.URL.Query().Get("state") != p.State {
		// A second callback for a sign-in that already worked (a double
		// redirect, the Back button): the person is signed in, so carry on.
		if a.CurrentUser(r) != nil {
			http.Redirect(w, r, "/", http.StatusFound)
			return
		}
		http.Error(w, "This sign-in link has expired. Please sign in again.", http.StatusBadRequest)
		return
	}
	a.sessions.Clear(w, loginCookie)
	if e := r.URL.Query().Get("error"); e != "" {
		http.Error(w, "Sign-in was cancelled or refused: "+e, http.StatusForbidden)
		return
	}

	provider, conf, err := a.discover(r.Context())
	if err != nil {
		http.Error(w, "Penlumen School can't be reached right now.", http.StatusBadGateway)
		return
	}
	token, err := conf.Exchange(r.Context(), r.URL.Query().Get("code"), oauth2.VerifierOption(p.Verifier))
	if err != nil {
		slog.Warn("sign-in: code exchange failed", "error", err)
		http.Error(w, "Sign-in failed. Please try again.", http.StatusBadRequest)
		return
	}
	rawID, _ := token.Extra("id_token").(string)
	idToken, err := provider.Verifier(&oidc.Config{ClientID: a.clientID}).Verify(r.Context(), rawID)
	if err != nil || idToken.Nonce != p.Nonce {
		slog.Warn("sign-in: bad id token", "error", err)
		http.Error(w, "Sign-in failed. Please try again.", http.StatusBadRequest)
		return
	}
	var user User
	if err := idToken.Claims(&user); err != nil {
		http.Error(w, "Sign-in failed. Please try again.", http.StatusBadRequest)
		return
	}
	if user.Role == "parent" {
		http.Error(w, "This app is for students and staff.", http.StatusForbidden)
		return
	}
	if a.OnSignIn != nil {
		if err := a.OnSignIn(r.Context(), &user); err != nil {
			slog.Warn("sign-in refused", "sub", user.Sub, "error", err)
			http.Error(w, "Your account can't sign in to this app. Ask your school's administrator.", http.StatusForbidden)
			return
		}
	}
	if err := a.sessions.Set(w, SessionCookie, user, sessionTTL); err != nil {
		http.Error(w, "Couldn't keep you signed in", http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, p.ReturnTo, http.StatusFound)
}

func (a *Auth) Logout(w http.ResponseWriter, r *http.Request) {
	a.sessions.Clear(w, SessionCookie)
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// CurrentUser is the signed-in person, or nil.
func (a *Auth) CurrentUser(r *http.Request) *User {
	var u User
	if err := a.sessions.Get(r, SessionCookie, &u); err != nil {
		return nil
	}
	return &u
}

// RequireUser sends people who aren't signed in to the sign-in page.
func (a *Auth) RequireUser(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if a.CurrentUser(r) == nil {
			http.Redirect(w, r, "/login?return_to="+r.URL.RequestURI(), http.StatusFound)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func randomString() string {
	b := make([]byte, 24)
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

// safeReturn only allows paths on this site, so a link can't bounce people
// to another site after signing in.
func safeReturn(path string) string {
	if !strings.HasPrefix(path, "/") || strings.HasPrefix(path, "//") || strings.Contains(path, `\`) {
		return "/"
	}
	return path
}

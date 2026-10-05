package app

// Development-only first-party sign-in. Sessions live in process memory and
// are deliberately unusable by staging/production configuration.

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"sync"
	"time"

	"energylens.local/cloud/internal/account"
	"github.com/go-chi/chi/v5"
)

const devAuthIssuer = "urn:energylens:dev-local:v1"

const (
	devAuthMaxLoginFailures = 5
	devAuthFailureWindow    = time.Minute
)

type devSession struct {
	access         string
	refresh        string
	authTime       time.Time
	accessExpires  time.Time
	refreshExpires time.Time
}

type devAuth struct {
	email              string
	password           string
	mu                 sync.Mutex
	byAccess           map[string]devSession
	byRefresh          map[string]string
	failures           int
	failureWindowStart time.Time
}

func newDevAuth(c Config) *devAuth {
	return &devAuth{email: c.DevAuthEmail, password: c.DevAuthPassword,
		byAccess: make(map[string]devSession), byRefresh: make(map[string]string)}
}

func devToken() (string, error) {
	var raw [32]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(raw[:]), nil
}

func (d *devAuth) Verify(_ context.Context, token string) (account.Identity, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	session, ok := d.byAccess[token]
	if !ok || !time.Now().Before(session.accessExpires) {
		return account.Identity{}, account.ErrInvalidCredential
	}
	return account.Identity{Issuer: devAuthIssuer, Subject: d.email, Email: d.email,
		EmailVerified: true, AuthTime: session.authTime}, nil
}

func (d *devAuth) Routes(r chi.Router) {
	r.Route("/auth/local", func(r chi.Router) {
		r.Post("/login", d.login)
		r.Post("/refresh", d.refresh)
		r.Post("/logout", d.logout)
	})
}

func devDecode(w http.ResponseWriter, r *http.Request, out any) bool {
	w.Header().Set("Cache-Control", "no-store")
	if r.Header.Get("Content-Type") != "application/json" {
		http.Error(w, "invalid request", 415)
		return false
	}
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096))
	decoder.DisallowUnknownFields()
	if decoder.Decode(out) != nil || !errors.Is(decoder.Decode(new(any)), io.EOF) {
		http.Error(w, "invalid request", 400)
		return false
	}
	return true
}

func (d *devAuth) issueLocked(w http.ResponseWriter, old *devSession) {
	access, err := devToken()
	if err != nil {
		http.Error(w, "auth unavailable", 503)
		return
	}
	refresh, err := devToken()
	if err != nil {
		http.Error(w, "auth unavailable", 503)
		return
	}
	now := time.Now().UTC()
	authTime := now
	if old != nil {
		authTime = old.authTime
	}
	session := devSession{access: access, refresh: refresh, authTime: authTime,
		accessExpires: now.Add(time.Hour), refreshExpires: now.Add(30 * 24 * time.Hour)}
	if old != nil {
		delete(d.byAccess, old.access)
		delete(d.byRefresh, old.refresh)
	}
	d.byAccess[access] = session
	d.byRefresh[refresh] = access
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"token_type": "Bearer", "access_token": access, "refresh_token": refresh,
		"access_expires_at":  session.accessExpires.Format(time.RFC3339),
		"refresh_expires_at": session.refreshExpires.Format(time.RFC3339),
	})
}

func (d *devAuth) login(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Email    string `json:"email"`
		Password string `json:"password"`
	}
	if !devDecode(w, r, &in) {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	now := time.Now()
	if d.failureWindowStart.IsZero() || now.Sub(d.failureWindowStart) >= devAuthFailureWindow {
		d.failures = 0
		d.failureWindowStart = now
	}
	if d.failures >= devAuthMaxLoginFailures {
		http.Error(w, "too many login attempts", http.StatusTooManyRequests)
		return
	}
	if subtle.ConstantTimeCompare([]byte(in.Email), []byte(d.email)) != 1 ||
		subtle.ConstantTimeCompare([]byte(in.Password), []byte(d.password)) != 1 {
		d.failures++
		http.Error(w, "invalid credential", 401)
		return
	}
	d.failures = 0
	d.failureWindowStart = time.Time{}
	d.issueLocked(w, nil)
}

func (d *devAuth) refresh(w http.ResponseWriter, r *http.Request) {
	var in struct {
		RefreshToken string `json:"refresh_token"`
	}
	if !devDecode(w, r, &in) {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	access, ok := d.byRefresh[in.RefreshToken]
	session := d.byAccess[access]
	if !ok || !time.Now().Before(session.refreshExpires) {
		http.Error(w, "invalid credential", 401)
		return
	}
	d.issueLocked(w, &session)
}

func (d *devAuth) logout(w http.ResponseWriter, r *http.Request) {
	var in struct {
		RefreshToken string `json:"refresh_token"`
	}
	if !devDecode(w, r, &in) {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if access, ok := d.byRefresh[in.RefreshToken]; ok {
		delete(d.byAccess, access)
		delete(d.byRefresh, in.RefreshToken)
	}
	w.WriteHeader(http.StatusNoContent)
}

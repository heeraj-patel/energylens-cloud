package account

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
)

// LocalAuthHTTP is not mounted by app.Build. Activation must install the full
// service set, mail sender, trusted edge source, and client flows together.
type LocalAuthHTTP struct {
	Enabled      bool
	Registration LocalRegistrationService
	Login        LocalLoginService
	Refresh      LocalRefreshService
	Recovery     LocalRecoveryService
	SourcePolicy LocalAuthSourcePolicy
}

func (h LocalAuthHTTP) ready() bool {
	return h.Enabled && h.Registration.configured() && h.Login.configured() && h.Refresh.configured() && h.Recovery.configured() &&
		h.Registration.DB == h.Login.DB && h.Login.DB == h.Refresh.DB && h.Refresh.DB == h.Recovery.DB &&
		h.Login.Codec.ActiveKeyID == h.Refresh.Codec.ActiveKeyID && h.Login.Codec.Audience == h.Refresh.Codec.Audience &&
		bytes.Equal(h.Login.Codec.Private, h.Refresh.Codec.Private)
}

func (h LocalAuthHTTP) Routes(r chi.Router) {
	if !h.ready() {
		return
	}
	r.Route("/auth/local", func(r chi.Router) {
		r.Use(localAuthNoStore)
		r.Post("/registration", h.registration)
		r.Post("/verify-email", h.verify)
		r.Post("/login", h.login)
		r.Post("/refresh", h.refresh)
		r.Post("/logout", h.logout)
		r.Post("/recovery", h.recovery)
		r.Post("/reset-password", h.reset)
	})
}

func localAuthNoStore(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Pragma", "no-cache")
		w.Header().Set("Referrer-Policy", "no-referrer")
		next.ServeHTTP(w, r)
	})
}

func localAuthDecode(w http.ResponseWriter, r *http.Request, out any) bool {
	if r.Header.Get("Content-Type") != "application/json" {
		fail(w, 415, "invalid_request", "application/json required")
		return false
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 4096))
	if err != nil || len(body) == 0 {
		fail(w, 400, "invalid_request", "invalid request body")
		return false
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if dec.Decode(out) != nil {
		fail(w, 400, "invalid_request", "invalid request body")
		return false
	}
	var extra any
	if !errors.Is(dec.Decode(&extra), io.EOF) {
		fail(w, 400, "invalid_request", "invalid request body")
		return false
	}
	return true
}

func localAuthError(w http.ResponseWriter, err error) {
	if errors.Is(err, ErrInvalidCredential) {
		fail(w, 401, "invalid_credential", "invalid credential")
		return
	}
	// Database and provider errors must not disclose private identifiers.
	fail(w, 503, "auth_unavailable", "authentication temporarily unavailable")
}

func (h LocalAuthHTTP) registration(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Email    string `json:"email"`
		Password string `json:"password"`
	}
	if !localAuthDecode(w, r, &in) {
		return
	}
	source, err := h.SourcePolicy.Source(r)
	if err != nil {
		localAuthError(w, err)
		return
	}
	out, err := h.Registration.RequestRegistration(r.Context(), in.Email, in.Password, source, time.Now().UTC())
	if err != nil {
		localAuthError(w, err)
		return
	}
	jsonOut(w, 202, out)
}
func (h LocalAuthHTTP) verify(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Token string `json:"token"`
	}
	if !localAuthDecode(w, r, &in) {
		return
	}
	if err := h.Registration.VerifyEmail(r.Context(), in.Token, time.Now().UTC()); err != nil {
		localAuthError(w, err)
		return
	}
	w.WriteHeader(204)
}
func (h LocalAuthHTTP) login(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Email    string `json:"email"`
		Password string `json:"password"`
	}
	if !localAuthDecode(w, r, &in) {
		return
	}
	source, err := h.SourcePolicy.Source(r)
	if err != nil {
		localAuthError(w, err)
		return
	}
	tokens, err := h.Login.Login(r.Context(), in.Email, in.Password, source, time.Now().UTC())
	if err != nil {
		localAuthError(w, err)
		return
	}
	localAuthTokensOut(w, tokens)
}
func (h LocalAuthHTTP) refresh(w http.ResponseWriter, r *http.Request) {
	var in struct {
		RefreshToken string `json:"refresh_token"`
	}
	if !localAuthDecode(w, r, &in) {
		return
	}
	tokens, err := h.Refresh.Rotate(r.Context(), in.RefreshToken, time.Now().UTC())
	if err != nil {
		localAuthError(w, err)
		return
	}
	localAuthTokensOut(w, tokens)
}
func (h LocalAuthHTTP) logout(w http.ResponseWriter, r *http.Request) {
	var in struct {
		RefreshToken string `json:"refresh_token"`
	}
	if !localAuthDecode(w, r, &in) {
		return
	}
	if err := h.Refresh.Logout(r.Context(), in.RefreshToken, time.Now().UTC()); err != nil {
		localAuthError(w, err)
		return
	}
	w.WriteHeader(204)
}
func (h LocalAuthHTTP) recovery(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Email string `json:"email"`
	}
	if !localAuthDecode(w, r, &in) {
		return
	}
	source, err := h.SourcePolicy.Source(r)
	if err != nil {
		localAuthError(w, err)
		return
	}
	out, err := h.Recovery.RequestReset(r.Context(), in.Email, source, time.Now().UTC())
	if err != nil {
		localAuthError(w, err)
		return
	}
	jsonOut(w, 202, out)
}
func (h LocalAuthHTTP) reset(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Token    string `json:"token"`
		Password string `json:"password"`
	}
	if !localAuthDecode(w, r, &in) {
		return
	}
	if err := h.Recovery.ResetPassword(r.Context(), in.Token, in.Password, time.Now().UTC()); err != nil {
		localAuthError(w, err)
		return
	}
	w.WriteHeader(204)
}

func localAuthTokensOut(w http.ResponseWriter, t LocalLoginTokens) {
	jsonOut(w, 200, struct {
		AccessToken      string    `json:"access_token"`
		TokenType        string    `json:"token_type"`
		AccessExpiresAt  time.Time `json:"access_expires_at"`
		RefreshToken     string    `json:"refresh_token"`
		RefreshExpiresAt time.Time `json:"refresh_expires_at"`
	}{t.AccessToken, "Bearer", t.AccessExpiresAt, t.RefreshToken, t.RefreshExpiresAt})
}

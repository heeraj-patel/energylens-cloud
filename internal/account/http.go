package account

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

type Service struct {
	Store       Store
	Verifier    Verifier
	Preferences PreferenceStore
	Profile     ProfileStore
	Deletion    DeletionStore
}

type DeletionStore interface {
	DeleteAccount(context.Context, string) error
}
type apiError struct {
	Code      string         `json:"code"`
	Message   string         `json:"message"`
	RequestID string         `json:"request_id"`
	Details   map[string]any `json:"details"`
}

func fail(w http.ResponseWriter, status int, code, msg string) {
	jsonOut(w, status, apiError{Code: code, Message: msg, RequestID: uuid.NewString(), Details: map[string]any{}})
}
func jsonOut(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
func (s Service) Routes(r chi.Router) {
	r.With(s.auth).Get("/me", s.me)
	if s.Profile != nil {
		r.With(s.auth).Patch("/me/profile", s.patchProfile)
	}
	if s.Deletion != nil {
		r.With(s.auth).Delete("/me", s.deleteMe)
	}
	if s.Preferences != nil {
		r.With(s.auth).Get("/me/preferences", s.accountPreferencesGet)
		r.With(s.auth).Patch("/me/preferences", s.accountPreferencesPatch)
	}
	r.Route("/homes", func(r chi.Router) {
		r.Use(s.auth)
		r.Get("/", s.list)
		r.Post("/", s.create)
		r.Get("/{home}", s.get)
		r.Patch("/{home}", s.patch)
		if s.Preferences != nil {
			r.Get("/{home}/preferences", s.homePreferencesGet)
			r.Patch("/{home}/preferences", s.homePreferencesPatch)
		}
	})
}

type userKey struct{}
type identityKey struct{}

func (s Service) auth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		a := r.Header.Get("Authorization")
		if !strings.HasPrefix(a, "Bearer ") || strings.TrimSpace(a[7:]) == "" {
			fail(w, 401, "unauthorized", "bearer token required")
			return
		}
		id, err := s.Verifier.Verify(r.Context(), strings.TrimSpace(a[7:]))
		if err != nil {
			fail(w, 401, "unauthorized", "invalid bearer token")
			return
		}
		user, err := s.Store.Resolve(r.Context(), id)
		if err != nil {
			if errors.Is(err, ErrDeleted) {
				fail(w, 403, "account_deleted", "account is closed")
				return
			}
			slog.Error("account identity resolution failed", "err", err)
			fail(w, 500, "internal_error", "could not resolve account")
			return
		}
		ctx := contextWithUser(r, user)
		ctx = context.WithValue(ctx, identityKey{}, id)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}
func contextWithUser(r *http.Request, u User) context.Context {
	return context.WithValue(r.Context(), userKey{}, u)
}
func user(r *http.Request) User { return r.Context().Value(userKey{}).(User) }
func (s Service) me(w http.ResponseWriter, r *http.Request) {
	u := user(r)
	if u.ProfileVersion > 0 {
		etag(w, u.ProfileVersion)
	}
	jsonOut(w, 200, u)
}
func (s Service) deleteMe(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	identity := r.Context().Value(identityKey{}).(Identity)
	now := time.Now().UTC()
	if identity.AuthTime.IsZero() || identity.AuthTime.After(now.Add(time.Minute)) || now.Sub(identity.AuthTime) > 5*time.Minute {
		fail(w, http.StatusForbidden, "recent_sign_in_required", "sign in again to delete the account")
		return
	}
	if _, ok := idempotencyKey(w, r); !ok {
		return
	}
	if r.ContentLength > 0 {
		fail(w, http.StatusBadRequest, "invalid_request", "request body must be empty")
		return
	}
	if err := s.Deletion.DeleteAccount(r.Context(), user(r).ID); err != nil {
		if errors.Is(err, ErrDeleted) {
			fail(w, http.StatusForbidden, "account_deleted", "account is closed")
			return
		}
		internal(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
func (s Service) list(w http.ResponseWriter, r *http.Request) {
	homes, err := s.Store.List(r.Context(), user(r).ID)
	if err != nil {
		internal(w, err)
		return
	}
	jsonOut(w, 200, map[string]any{"homes": homes})
}
func (s Service) get(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "home")
	if !validUUID(id) {
		fail(w, 404, "not_found", "home not found")
		return
	}
	h, err := s.Store.Get(r.Context(), user(r).ID, id)
	if err != nil {
		storeError(w, err)
		return
	}
	etag(w, h.Version)
	jsonOut(w, 200, h)
}
func (s Service) create(w http.ResponseWriter, r *http.Request) {
	key, ok := idempotencyKey(w, r)
	if !ok {
		return
	}
	var in HomeCreate
	body, ok := decode(w, r, &in)
	if !ok {
		return
	}
	if !validCreate(in) {
		fail(w, 400, "invalid_request", "invalid home fields")
		return
	}
	hash := sha256.Sum256(body)
	h, err := s.Store.Create(r.Context(), user(r).ID, in, key, hash[:])
	if err != nil {
		storeError(w, err)
		return
	}
	etag(w, h.Version)
	jsonOut(w, 201, h)
}
func (s Service) patch(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "home")
	if !validUUID(id) {
		fail(w, 404, "not_found", "home not found")
		return
	}
	key, ok := idempotencyKey(w, r)
	if !ok {
		return
	}
	version, ok := matchVersion(w, r)
	if !ok {
		return
	}
	var in HomePatch
	body, ok := decode(w, r, &in)
	if !ok {
		return
	}
	if !validPatch(in) {
		fail(w, 400, "invalid_request", "invalid home fields")
		return
	}
	hash := sha256.Sum256(append([]byte(strconv.FormatInt(version, 10)+"\n"), body...))
	h, err := s.Store.Patch(r.Context(), user(r).ID, id, in, version, key, hash[:])
	if err != nil {
		storeError(w, err)
		return
	}
	etag(w, h.Version)
	jsonOut(w, 200, h)
}
func validUUID(s string) bool {
	id, err := uuid.Parse(s)
	return err == nil && id.String() == strings.ToLower(s)
}
func idempotencyKey(w http.ResponseWriter, r *http.Request) (string, bool) {
	key := r.Header.Get("Idempotency-Key")
	if !validUUID(key) {
		fail(w, 400, "invalid_request", "UUID Idempotency-Key required")
		return "", false
	}
	return key, true
}
func matchVersion(w http.ResponseWriter, r *http.Request) (int64, bool) {
	v := r.Header.Get("If-Match")
	if v == "" {
		fail(w, 428, "invalid_request", "If-Match required")
		return 0, false
	}
	if len(v) < 3 || v[0] != '"' || v[len(v)-1] != '"' {
		fail(w, 400, "invalid_request", "strong version ETag required")
		return 0, false
	}
	n, e := strconv.ParseInt(v[1:len(v)-1], 10, 64)
	if e != nil || n < 1 {
		fail(w, 400, "invalid_request", "invalid If-Match version")
		return 0, false
	}
	return n, true
}
func etag(w http.ResponseWriter, n int64) {
	w.Header().Set("ETag", strconv.Quote(strconv.FormatInt(n, 10)))
}
func decode(w http.ResponseWriter, r *http.Request, v any) ([]byte, bool) {
	if !strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
		fail(w, 415, "invalid_request", "application/json required")
		return nil, false
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 64<<10))
	if err != nil {
		fail(w, 400, "invalid_request", "invalid request body")
		return nil, false
	}
	d := json.NewDecoder(strings.NewReader(string(body)))
	d.DisallowUnknownFields()
	if err := d.Decode(v); err != nil {
		fail(w, 400, "invalid_request", "invalid JSON")
		return nil, false
	}
	var extra any
	if err := d.Decode(&extra); err != io.EOF {
		fail(w, 400, "invalid_request", "one JSON object required")
		return nil, false
	}
	return body, true
}
func validCreate(h HomeCreate) bool {
	if !validName(h.Name) || !validLocation(h.Location) {
		return false
	}
	if h.Location.CountryCode == "US" {
		if !validUSZIPPostal(h.Location.PostalCode) {
			return false
		}
		if _, resolved := resolveUSZIPTimeZone(h.Location.PostalCode); resolved {
			return true
		}
	}
	return validCreateExplicitTimezone(h.Timezone)
}

func validCreateExplicitTimezone(z string) bool {
	// LoadLocation treats Local as a special process setting, not an IANA zone.
	return z != "Local" && validTimezone(z)
}

func validPatch(h HomePatch) bool {
	if h.Name == nil && h.Timezone == nil && h.Location == nil {
		return false
	}
	if h.Name != nil && !validName(*h.Name) {
		return false
	}
	if h.Timezone != nil && !validTimezone(*h.Timezone) {
		return false
	}
	return h.Location == nil || validLocation(*h.Location)
}
func validName(n string) bool { return n == strings.TrimSpace(n) && len(n) > 0 && len(n) <= 120 }
func validTimezone(z string) bool {
	if len(z) < 3 {
		return false
	}
	_, err := time.LoadLocation(z)
	return err == nil
}
func validLocation(l Location) bool {
	return len(l.CountryCode) == 2 && l.CountryCode[0] >= 'A' && l.CountryCode[0] <= 'Z' && l.CountryCode[1] >= 'A' && l.CountryCode[1] <= 'Z' && strings.TrimSpace(l.PostalCode) != "" && (l.Precision == "postal_code" || l.Precision == "city" || l.Precision == "address") && (l.Source == "user_entered" || l.Source == "user_confirmed" || l.Source == "geocoded")
}
func storeError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, ErrNotFound):
		fail(w, 404, "not_found", "home not found")
	case errors.Is(err, ErrVersion):
		fail(w, 409, "version_conflict", "home version changed")
	case errors.Is(err, ErrKeyReuse):
		fail(w, 409, "idempotency_key_reuse", "key was used with another request")
	default:
		internal(w, err)
	}
}
func internal(w http.ResponseWriter, err error) {
	slog.Error("account home store failed", "err", err)
	fail(w, 500, "internal_error", "account request failed")
}

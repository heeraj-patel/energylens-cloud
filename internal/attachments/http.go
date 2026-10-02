package attachments

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

type Service struct {
	Store  Store
	Auth   func(http.Handler) http.Handler
	UserID func(*http.Request) string
}

func (s Service) Routes(r chi.Router) {
	r.Route("/homes/{home}/attachments", func(r chi.Router) {
		r.Use(s.Auth)
		r.Get("/", s.list)
		r.Post("/", s.create)
		r.Patch("/{attachment}", s.close)
	})
}
func validUUID(s string) bool { v, e := uuid.Parse(s); return e == nil && v.String() == s }
func fail(w http.ResponseWriter, status int, code string) {
	write(w, status, map[string]any{"code": code, "message": http.StatusText(status), "request_id": uuid.NewString(), "details": map[string]any{}})
}
func write(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
func storeError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, ErrNotFound):
		fail(w, 404, "not_found")
	case errors.Is(err, ErrDisclosure):
		fail(w, 403, "history_disclosure_required")
	case errors.Is(err, ErrVersion):
		fail(w, 409, "version_conflict")
	case errors.Is(err, ErrConflict):
		fail(w, 409, "attachment_conflict")
	case errors.Is(err, ErrKeyReuse):
		fail(w, 409, "idempotency_key_reuse")
	default:
		slog.Error("attachment request failed", "error", err)
		fail(w, 500, "internal_error")
	}
}
func read(w http.ResponseWriter, r *http.Request, v any) ([]byte, bool) {
	if !strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
		fail(w, 415, "invalid_request")
		return nil, false
	}
	raw, e := io.ReadAll(http.MaxBytesReader(w, r.Body, 64<<10))
	if e != nil {
		fail(w, 400, "invalid_request")
		return nil, false
	}
	d := json.NewDecoder(strings.NewReader(string(raw)))
	d.DisallowUnknownFields()
	if d.Decode(v) != nil || d.Decode(new(any)) != io.EOF {
		fail(w, 400, "invalid_request")
		return nil, false
	}
	return raw, true
}
func parseVersion(w http.ResponseWriter, r *http.Request) (int64, bool) {
	raw := r.Header.Get("If-Match")
	if raw == "" {
		fail(w, 428, "version_required")
		return 0, false
	}
	if len(raw) < 3 || raw[0] != '"' || raw[len(raw)-1] != '"' {
		fail(w, 400, "invalid_request")
		return 0, false
	}
	v, e := strconv.ParseInt(raw[1:len(raw)-1], 10, 64)
	if e != nil || v < 1 {
		fail(w, 400, "invalid_request")
		return 0, false
	}
	return v, true
}
func parseKey(w http.ResponseWriter, r *http.Request) (string, bool) {
	v := r.Header.Get("Idempotency-Key")
	if !validUUID(v) {
		fail(w, 400, "invalid_request")
		return "", false
	}
	return v, true
}
func scopedHome(w http.ResponseWriter, r *http.Request) (string, bool) {
	v := chi.URLParam(r, "home")
	if !validUUID(v) {
		fail(w, 404, "not_found")
		return "", false
	}
	return v, true
}
func (s Service) list(w http.ResponseWriter, r *http.Request) {
	home, ok := scopedHome(w, r)
	if !ok {
		return
	}
	a, e := s.Store.List(r.Context(), s.UserID(r), home)
	if e != nil {
		storeError(w, e)
		return
	}
	write(w, 200, map[string]any{"attachments": a})
}
func (s Service) create(w http.ResponseWriter, r *http.Request) {
	home, ok := scopedHome(w, r)
	if !ok {
		return
	}
	key, ok := parseKey(w, r)
	if !ok {
		return
	}
	version, ok := parseVersion(w, r)
	if !ok {
		return
	}
	var in CreateInput
	raw, ok := read(w, r, &in)
	if !ok {
		return
	}
	if !validUUID(in.SensorID) || !validUUID(in.ApplianceID) || in.IdentityRevision < 1 || in.StartedAt.IsZero() {
		fail(w, 400, "invalid_request")
		return
	}
	hash := sha256.Sum256(append([]byte(strconv.FormatInt(version, 10)+"\n"), raw...))
	a, e := s.Store.Create(r.Context(), s.UserID(r), home, in, version, key, hash[:])
	if e != nil {
		storeError(w, e)
		return
	}
	w.Header().Set("ETag", strconv.Quote(strconv.FormatInt(version+1, 10)))
	write(w, 201, a)
}
func (s Service) close(w http.ResponseWriter, r *http.Request) {
	home, ok := scopedHome(w, r)
	if !ok {
		return
	}
	id := chi.URLParam(r, "attachment")
	if !validUUID(id) {
		fail(w, 404, "not_found")
		return
	}
	key, ok := parseKey(w, r)
	if !ok {
		return
	}
	version, ok := parseVersion(w, r)
	if !ok {
		return
	}
	var in CloseInput
	raw, ok := read(w, r, &in)
	if !ok {
		return
	}
	if in.EndedAt.IsZero() {
		fail(w, 400, "invalid_request")
		return
	}
	hash := sha256.Sum256(append([]byte(id+"\n"+strconv.FormatInt(version, 10)+"\n"), raw...))
	a, e := s.Store.Close(r.Context(), s.UserID(r), home, id, in.EndedAt, version, key, hash[:])
	if e != nil {
		storeError(w, e)
		return
	}
	w.Header().Set("ETag", strconv.Quote(strconv.FormatInt(a.Version, 10)))
	write(w, 200, a)
}

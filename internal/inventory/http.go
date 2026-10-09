package inventory

import (
	"bytes"
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
	r.Route("/homes/{home}/appliances", func(r chi.Router) {
		r.Use(s.Auth)
		r.Get("/", s.list)
		r.Post("/", s.create)
		r.Get("/{appliance}", s.get)
		r.Patch("/{appliance}", s.patch)
		r.Delete("/{appliance}", s.archive)
	})
}
func fail(w http.ResponseWriter, status int, code, msg string) {
	write(w, status, map[string]any{"code": code, "message": msg, "request_id": uuid.NewString(), "details": map[string]any{}})
}
func write(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
func storeError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, ErrNotFound):
		fail(w, 404, "not_found", "appliance or home not found")
	case errors.Is(err, ErrVersion):
		fail(w, 409, "version_conflict", "appliance version changed")
	case errors.Is(err, ErrKeyReuse):
		fail(w, 409, "idempotency_key_reuse", "key used with another request")
	case errors.Is(err, ErrCatalogMismatch):
		fail(w, 409, "catalog_mismatch", "confirmed catalog identity does not match")
	case errors.Is(err, ErrDisclosureRequired):
		fail(w, 403, "history_disclosure_required", "accept history disclosure before changing appliances")
	case errors.Is(err, ErrAttachmentConflict):
		fail(w, 409, "attachment_conflict", "")
	default:
		slog.Error("inventory store failed", "error", err)
		fail(w, 500, "internal_error", "inventory request failed")
	}
}
func parseHome(w http.ResponseWriter, r *http.Request) (string, bool) {
	home := chi.URLParam(r, "home")
	if !validUUID(home) {
		fail(w, 404, "not_found", "home not found")
		return "", false
	}
	return home, true
}
func parseIDs(w http.ResponseWriter, r *http.Request) (string, string, bool) {
	home, ok := parseHome(w, r)
	if !ok {
		return "", "", false
	}
	id := chi.URLParam(r, "appliance")
	if !validUUID(id) {
		fail(w, 404, "not_found", "appliance not found")
		return "", "", false
	}
	return home, id, true
}
func key(w http.ResponseWriter, r *http.Request) (string, bool) {
	k := r.Header.Get("Idempotency-Key")
	if !validUUID(k) {
		fail(w, 400, "invalid_request", "UUID Idempotency-Key required")
		return "", false
	}
	return k, true
}
func match(w http.ResponseWriter, r *http.Request) (int64, bool) {
	raw := r.Header.Get("If-Match")
	if raw == "" {
		fail(w, 428, "invalid_request", "If-Match required")
		return 0, false
	}
	if len(raw) < 3 || raw[0] != '"' || raw[len(raw)-1] != '"' {
		fail(w, 400, "invalid_request", "strong version ETag required")
		return 0, false
	}
	n, err := strconv.ParseInt(raw[1:len(raw)-1], 10, 64)
	if err != nil || n < 1 {
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
		fail(w, 400, "invalid_request", "invalid body")
		return nil, false
	}
	d := json.NewDecoder(bytes.NewReader(body))
	d.DisallowUnknownFields()
	if err = d.Decode(v); err != nil {
		fail(w, 400, "invalid_request", "invalid JSON")
		return nil, false
	}
	var extra any
	if err = d.Decode(&extra); !errors.Is(err, io.EOF) {
		fail(w, 400, "invalid_request", "one JSON object required")
		return nil, false
	}
	return body, true
}
func patchFields(body []byte) bool {
	var fields map[string]json.RawMessage
	if json.Unmarshal(body, &fields) != nil || len(fields) == 0 {
		return false
	}
	for k, v := range fields {
		switch k {
		case "name", "category", "identity":
			if bytes.Equal(bytes.TrimSpace(v), []byte("null")) {
				return false
			}
		case "room":
		default:
			return false
		}
	}
	return true
}
func (s Service) list(w http.ResponseWriter, r *http.Request) {
	home, ok := parseHome(w, r)
	if !ok {
		return
	}
	user := s.UserID(r)
	a, revision, err := s.Store.ListWithRevision(r.Context(), user, home)
	if err != nil {
		storeError(w, err)
		return
	}
	w.Header().Set(InventoryRevisionHeader, revision)
	for _, appliance := range a {
		if appliance.DemoDeviceID != "" {
			w.Header().Set("Cache-Control", "no-store")
			w.Header().Set("X-EnergyLens-Data-Mode", "sample")
			w.Header().Set("X-EnergyLens-Fixture-Revision", appliance.DemoFixtureRevision)
			break
		}
	}
	write(w, 200, map[string]any{"appliances": a})
}
func (s Service) get(w http.ResponseWriter, r *http.Request) {
	home, id, ok := parseIDs(w, r)
	if !ok {
		return
	}
	user := s.UserID(r)
	a, err := s.Store.Get(r.Context(), user, home, id)
	if errors.Is(err, ErrNotFound) {
		items, _, listErr := s.Store.ListWithRevision(r.Context(), user, home)
		if listErr != nil && !errors.Is(listErr, ErrNotFound) {
			storeError(w, listErr)
			return
		}
		if listErr == nil {
			for _, item := range items {
				if item.ID == id && item.DemoDeviceID != "" {
					a, err = item, nil
					w.Header().Set("Cache-Control", "no-store")
					w.Header().Set("X-EnergyLens-Data-Mode", "sample")
					w.Header().Set("X-EnergyLens-Fixture-Revision", item.DemoFixtureRevision)
					break
				}
			}
		}
	}
	if err != nil {
		storeError(w, err)
		return
	}
	etag(w, a.Version)
	write(w, 200, a)
}
func (s Service) create(w http.ResponseWriter, r *http.Request) {
	home, ok := parseHome(w, r)
	if !ok {
		return
	}
	k, ok := key(w, r)
	if !ok {
		return
	}
	var in CreateInput
	body, ok := decode(w, r, &in)
	if !ok {
		return
	}
	if !ValidCreate(in) {
		fail(w, 400, "invalid_request", "invalid appliance fields")
		return
	}
	hash := sha256.Sum256(body)
	a, err := s.Store.Create(r.Context(), s.UserID(r), home, in, k, hash[:])
	if err != nil {
		storeError(w, err)
		return
	}
	etag(w, a.Version)
	write(w, 201, a)
}
func (s Service) patch(w http.ResponseWriter, r *http.Request) {
	home, id, ok := parseIDs(w, r)
	if !ok {
		return
	}
	k, ok := key(w, r)
	if !ok {
		return
	}
	version, ok := match(w, r)
	if !ok {
		return
	}
	var in PatchInput
	body, ok := decode(w, r, &in)
	if !ok {
		return
	}
	if !patchFields(body) || !ValidPatch(in) {
		fail(w, 400, "invalid_request", "invalid appliance fields")
		return
	}
	hash := sha256.Sum256(append([]byte(strconv.FormatInt(version, 10)+"\n"), body...))
	a, err := s.Store.Patch(r.Context(), s.UserID(r), home, id, in, version, k, hash[:])
	if err != nil {
		storeError(w, err)
		return
	}
	etag(w, a.Version)
	write(w, 200, a)
}

func (s Service) archive(w http.ResponseWriter, r *http.Request) {
	home, id, ok := parseIDs(w, r)
	if !ok {
		return
	}
	k, ok := key(w, r)
	if !ok {
		return
	}
	version, ok := match(w, r)
	if !ok {
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1))
	if err != nil || len(body) != 0 {
		fail(w, 400, "invalid_request", "")
		return
	}
	hash := sha256.Sum256([]byte(strconv.FormatInt(version, 10)))
	_, err = s.Store.Archive(r.Context(), s.UserID(r), home, id, version, k, hash[:])
	if err != nil {
		storeError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

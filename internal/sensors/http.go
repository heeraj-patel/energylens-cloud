package sensors

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"

	"energylens.local/cloud/internal/hubtrust"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

type Service struct {
	Store  Store
	Auth   func(http.Handler) http.Handler
	UserID func(*http.Request) string
}

func (s Service) AccountRoutes(r chi.Router) {
	r.Route("/homes/{home}/sensors", func(r chi.Router) { r.Use(s.Auth); r.Get("/", s.listOwner); r.Post("/", s.create) })
}
func (s Service) HubRoutes(r chi.Router) { r.Get("/hubs/{hub}/sensors", s.listHub) }
func validUUID(s string) bool            { v, e := uuid.Parse(s); return e == nil && v.String() == s }
func fail(w http.ResponseWriter, status int, code string) {
	write(w, status, map[string]any{"code": code, "message": http.StatusText(status), "request_id": uuid.NewString(), "details": map[string]any{}})
}
func write(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
func internal(w http.ResponseWriter, e error) {
	slog.Error("sensor request failed", "error", e)
	fail(w, 500, "internal_error")
}
func validInput(v CreateInput) bool {
	if !validUUID(v.HubID) || v.OwnershipEpoch < 1 || len(v.LocalSensorID) < 1 || len(v.LocalSensorID) > 256 || strings.TrimSpace(v.LocalSensorID) != v.LocalSensorID || strings.ContainsAny(v.LocalSensorID, "\x00\r\n") {
		return false
	}
	switch v.Kind {
	case "plug", "strip_outlet", "circuit", "utility_meter":
	default:
		return false
	}
	if v.ParentSensorID != nil && !validUUID(*v.ParentSensorID) {
		return false
	}
	if v.Capabilities == nil {
		return false
	}
	seen := map[string]bool{}
	for _, c := range v.Capabilities {
		switch c {
		case "power", "energy", "voltage", "current", "relay_state":
		default:
			return false
		}
		if seen[c] {
			return false
		}
		seen[c] = true
	}
	return true
}
func (s Service) create(w http.ResponseWriter, r *http.Request) {
	home := chi.URLParam(r, "home")
	if !validUUID(home) {
		fail(w, 404, "not_found")
		return
	}
	key := r.Header.Get("Idempotency-Key")
	if !validUUID(key) {
		fail(w, 400, "invalid_request")
		return
	}
	if !strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
		fail(w, 415, "invalid_request")
		return
	}
	raw, e := io.ReadAll(http.MaxBytesReader(w, r.Body, 64<<10))
	if e != nil {
		fail(w, 400, "invalid_request")
		return
	}
	var in CreateInput
	d := json.NewDecoder(strings.NewReader(string(raw)))
	d.DisallowUnknownFields()
	if d.Decode(&in) != nil || d.Decode(new(any)) != io.EOF || !validInput(in) {
		fail(w, 400, "invalid_request")
		return
	}
	hash := sha256.Sum256(raw)
	v, e := s.Store.Create(r.Context(), s.UserID(r), home, in, key, hash[:])
	switch {
	case errors.Is(e, ErrNotFound):
		fail(w, 404, "not_found")
	case errors.Is(e, ErrConflict):
		fail(w, 409, "sensor_conflict")
	case errors.Is(e, ErrKeyReuse):
		fail(w, 409, "idempotency_key_reuse")
	case e != nil:
		internal(w, e)
	default:
		write(w, 201, v)
	}
}
func (s Service) listOwner(w http.ResponseWriter, r *http.Request) {
	home := chi.URLParam(r, "home")
	if !validUUID(home) {
		fail(w, 404, "not_found")
		return
	}
	v, e := s.Store.ListOwner(r.Context(), s.UserID(r), home)
	if errors.Is(e, ErrNotFound) {
		fail(w, 404, "not_found")
	} else if e != nil {
		internal(w, e)
	} else {
		write(w, 200, map[string]any{"sensors": v})
	}
}
func (s Service) listHub(w http.ResponseWriter, r *http.Request) {
	hub := chi.URLParam(r, "hub")
	if !validUUID(hub) {
		fail(w, 404, "not_found")
		return
	}
	if r.TLS == nil || len(r.TLS.VerifiedChains) == 0 || len(r.TLS.PeerCertificates) == 0 {
		fail(w, 403, "forbidden")
		return
	}
	peer := r.TLS.PeerCertificates[0]
	peerHub, epoch, ok := hubtrust.PeerIdentity(peer)
	if !ok || peerHub != hub {
		fail(w, 403, "forbidden")
		return
	}
	v, e := s.Store.ListHub(r.Context(), hub, peer.SerialNumber.Text(16), epoch)
	if errors.Is(e, ErrDenied) {
		fail(w, 403, "forbidden")
	} else if e != nil {
		internal(w, e)
	} else {
		write(w, 200, v)
	}
}

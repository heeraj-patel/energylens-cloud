package hubtrust

import (
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"
)

func (s Service) publishPairingCode(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if s.Pairing == nil {
		fail(w, 503, "pairing_unavailable")
		return
	}
	hub := chi.URLParam(r, "hub")
	if !uuidOK(hub) || r.TLS == nil || len(r.TLS.VerifiedChains) == 0 || len(r.TLS.PeerCertificates) == 0 {
		fail(w, 403, "forbidden")
		return
	}
	peer := r.TLS.PeerCertificates[0]
	identity, epoch, ok := peerIdentity(peer)
	if !ok || identity != hub {
		fail(w, 403, "forbidden")
		return
	}
	var in PairingPublication
	if !read(w, r, &in) {
		return
	}
	expires, err := s.Pairing.Publish(r.Context(), hub, peer.SerialNumber.Text(16), epoch, in)
	switch {
	case errors.Is(err, ErrDenied):
		fail(w, 403, "forbidden")
	case errors.Is(err, ErrConflict):
		fail(w, 409, "pairing_conflict")
	case errors.Is(err, ErrPairingRateLimited):
		fail(w, 429, "rate_limited")
	case err != nil:
		fail(w, 503, "pairing_unavailable")
	default:
		out(w, 200, map[string]any{"expires_at": expires})
	}
}

func (s Service) resolvePairingCode(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if s.Pairing == nil || s.PairingSource == nil {
		fail(w, 503, "pairing_unavailable")
		return
	}
	source, err := s.PairingSource(r)
	if err != nil {
		fail(w, 400, "invalid_request")
		return
	}
	err = s.Pairing.AllowResolve(r.Context(), s.UserID(r), source)
	if errors.Is(err, ErrPairingRateLimited) {
		fail(w, 429, "rate_limited")
		return
	}
	if err != nil {
		fail(w, 503, "pairing_unavailable")
		return
	}
	var in struct {
		Code     string `json:"code"`
		ClientID string `json:"client_id"`
	}
	if !read(w, r, &in) {
		return
	}
	operation, ok := requestKey(w, r)
	if !ok {
		return
	}
	session := ""
	if s.SessionID != nil {
		session = s.SessionID(r)
	}
	if session == "" {
		fail(w, 401, "unauthorized")
		return
	}
	resolved, err := s.Pairing.Resolve(r.Context(), s.UserID(r), chi.URLParam(r, "home"), session, in.ClientID, operation, in.Code)
	switch {
	case errors.Is(err, ErrNotFound):
		fail(w, 404, "pairing_unavailable")
	case err != nil:
		fail(w, 503, "pairing_unavailable")
	default:
		out(w, 200, resolved)
	}
}

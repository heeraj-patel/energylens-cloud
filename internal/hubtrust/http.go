package hubtrust

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"log/slog"
	"math/big"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

type Service struct {
	Store  Store
	Signer tls.Certificate
	Auth   func(http.Handler) http.Handler
	UserID func(*http.Request) string
}
type apiError struct {
	Code      string         `json:"code"`
	Message   string         `json:"message"`
	RequestID string         `json:"request_id"`
	Details   map[string]any `json:"details"`
}

func out(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
func fail(w http.ResponseWriter, status int, code string) {
	out(w, status, apiError{code, http.StatusText(status), uuid.NewString(), map[string]any{}})
}
func read(w http.ResponseWriter, r *http.Request, v any) bool {
	if !strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
		fail(w, 415, "invalid_request")
		return false
	}
	b, e := io.ReadAll(http.MaxBytesReader(w, r.Body, 64<<10))
	if e != nil {
		fail(w, 400, "invalid_request")
		return false
	}
	d := json.NewDecoder(strings.NewReader(string(b)))
	d.DisallowUnknownFields()
	if d.Decode(v) != nil {
		fail(w, 400, "invalid_request")
		return false
	}
	var extra any
	if d.Decode(&extra) != io.EOF {
		fail(w, 400, "invalid_request")
		return false
	}
	return true
}
func uuidOK(s string) bool {
	id, e := uuid.Parse(s)
	return e == nil && id.String() == strings.ToLower(s)
}
func (s Service) AccountRoutes(r chi.Router) {
	r.With(s.Auth).Post("/homes/{home}/hub-claims", s.begin)
	r.With(s.Auth).Post("/hubs/claim", s.finish)
	r.With(s.Auth).Delete("/homes/{home}/hubs/{hub}", s.unclaim)
}
func (s Service) EnrollmentRoutes(r chi.Router) { r.Post("/hubs/{hub}/enroll", s.enroll) }
func (s Service) HubRoutes(r chi.Router) {
	r.Post("/hubs/{hub}/certificate", s.renew)
	r.Get("/hubs/{hub}/ownership", s.ownership)
}

// ownership provides a fresh, cloud-authenticated home binding to the hub.
// A prior-epoch certificate may be used only to renew, never to learn or bind
// the next owner's home. This route exists only on the hub mTLS listener.
func (s Service) ownership(w http.ResponseWriter, r *http.Request) {
	hubID := chi.URLParam(r, "hub")
	if !uuidOK(hubID) {
		fail(w, 404, "not_found")
		return
	}
	if r.TLS == nil || len(r.TLS.VerifiedChains) == 0 || len(r.TLS.PeerCertificates) == 0 {
		fail(w, 403, "forbidden")
		return
	}
	peer := r.TLS.PeerCertificates[0]
	peerHub, peerEpoch, ok := peerIdentity(peer)
	if !ok || peerHub != hubID {
		fail(w, 403, "forbidden")
		return
	}
	hub, err := s.Store.VerifyCertificate(r.Context(), hubID, peer.SerialNumber.Text(16), peerEpoch)
	if errors.Is(err, ErrDenied) {
		fail(w, 403, "ownership_revoked")
		return
	}
	if err != nil {
		internal(w, err)
		return
	}
	if hub.Epoch != peerEpoch {
		fail(w, 403, "ownership_revoked")
		return
	}
	if hub.HomeID == nil {
		fail(w, 404, "ownership_unbound")
		return
	}
	out(w, 200, map[string]any{
		"hub_id": hub.ID, "local_hub_id": hub.LocalID,
		"home_id": *hub.HomeID, "ownership_epoch": hub.Epoch,
	})
}
func requestKey(w http.ResponseWriter, r *http.Request) (string, bool) {
	key := r.Header.Get("Idempotency-Key")
	if !uuidOK(key) {
		fail(w, 400, "invalid_request")
		return "", false
	}
	return key, true
}
func (s Service) begin(w http.ResponseWriter, r *http.Request) {
	home := chi.URLParam(r, "home")
	if !uuidOK(home) {
		fail(w, 404, "not_found")
		return
	}
	var in struct {
		LocalHubID string `json:"local_hub_id"`
	}
	if !read(w, r, &in) {
		return
	}
	key, ok := requestKey(w, r)
	if !ok {
		return
	}
	c, e := s.Store.Begin(r.Context(), s.UserID(r), home, in.LocalHubID, key)
	if errors.Is(e, ErrNotFound) {
		fail(w, 404, "not_found")
		return
	}
	if errors.Is(e, ErrKeyReuse) {
		fail(w, 409, "idempotency_key_reuse")
		return
	}
	if e != nil {
		internal(w, e)
		return
	}
	out(w, 201, c)
}
func (s Service) finish(w http.ResponseWriter, r *http.Request) {
	var in struct {
		ClaimID         string `json:"claim_id"`
		LocalHubID      string `json:"local_hub_id"`
		PossessionProof string `json:"possession_proof"`
		DeviceSignature string `json:"device_signature"`
	}
	if !read(w, r, &in) {
		return
	}
	if !uuidOK(in.ClaimID) {
		fail(w, 400, "invalid_request")
		return
	}
	key, ok := requestKey(w, r)
	if !ok {
		return
	}
	o, e := s.Store.Finish(r.Context(), s.UserID(r), in.ClaimID, in.LocalHubID, in.PossessionProof, in.DeviceSignature, key)
	switch {
	case errors.Is(e, ErrNotFound):
		fail(w, 404, "not_found")
	case errors.Is(e, ErrDenied):
		fail(w, 403, "forbidden")
	case errors.Is(e, ErrKeyReuse):
		fail(w, 409, "idempotency_key_reuse")
	case errors.Is(e, ErrConflict), errors.Is(e, ErrExpired):
		fail(w, 409, "claim_conflict")
	case e != nil:
		internal(w, e)
	default:
		out(w, 200, o)
	}
}
func (s Service) unclaim(w http.ResponseWriter, r *http.Request) {
	home, hub := chi.URLParam(r, "home"), chi.URLParam(r, "hub")
	if !uuidOK(home) || !uuidOK(hub) {
		fail(w, 404, "not_found")
		return
	}
	et := r.Header.Get("If-Match")
	if len(et) < 3 || et[0] != '"' || et[len(et)-1] != '"' {
		fail(w, 428, "version_required")
		return
	}
	v, e := strconv.ParseInt(et[1:len(et)-1], 10, 64)
	if e != nil || v < 1 {
		fail(w, 400, "invalid_request")
		return
	}
	key, ok := requestKey(w, r)
	if !ok {
		return
	}
	e = s.Store.Unclaim(r.Context(), s.UserID(r), home, hub, v, key)
	switch {
	case errors.Is(e, ErrNotFound):
		fail(w, 404, "not_found")
	case errors.Is(e, ErrKeyReuse):
		fail(w, 409, "idempotency_key_reuse")
	case errors.Is(e, ErrConflict):
		fail(w, 409, "version_conflict")
	case e != nil:
		internal(w, e)
	default:
		w.WriteHeader(204)
	}
}
func parseCSR(raw string) (*x509.CertificateRequest, []byte, error) {
	block, rest := pem.Decode([]byte(raw))
	if block == nil || block.Type != "CERTIFICATE REQUEST" || len(strings.TrimSpace(string(rest))) > 0 {
		return nil, nil, ErrDenied
	}
	csr, e := x509.ParseCertificateRequest(block.Bytes)
	if e != nil || csr.CheckSignature() != nil {
		return nil, nil, ErrDenied
	}
	return csr, block.Bytes, nil
}
func (s Service) issue(w http.ResponseWriter, r *http.Request, hub Hub, csr *x509.CertificateRequest, nonce, oldSerial string) {
	if len(s.Signer.Certificate) == 0 || s.Signer.PrivateKey == nil {
		fail(w, 503, "enrollment_unavailable")
		return
	}
	ca, e := x509.ParseCertificate(s.Signer.Certificate[0])
	if e != nil || !ca.IsCA {
		fail(w, 503, "enrollment_unavailable")
		return
	}
	serialBytes := make([]byte, 16)
	if _, e = rand.Read(serialBytes); e != nil {
		internal(w, e)
		return
	}
	serial := new(big.Int).SetBytes(serialBytes)
	now := time.Now().UTC()
	expiry := now.Add(24 * time.Hour)
	cert := &x509.Certificate{SerialNumber: serial, NotBefore: now.Add(-time.Minute), NotAfter: expiry, KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}, BasicConstraintsValid: true, Subject: csr.Subject}
	cert.Subject.CommonName = "energylens-hub:" + hub.ID + ":" + strconv.FormatInt(hub.Epoch, 10)
	identity, _ := url.Parse("spiffe://energylens.local/hubs/" + hub.ID + "/epochs/" + strconv.FormatInt(hub.Epoch, 10))
	cert.URIs = []*url.URL{identity}
	der, e := x509.CreateCertificate(rand.Reader, cert, ca, csr.PublicKey, s.Signer.PrivateKey)
	if e != nil {
		internal(w, e)
		return
	}
	certPEM := string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
	if nonce != "" {
		hash := sha256.Sum256(csr.Raw)
		e = s.Store.SaveEnrollmentCertificate(r.Context(), nonce, serial.Text(16), hub.ID, hub.Epoch, expiry, certPEM, hash[:])
	} else {
		hash := sha256.Sum256(csr.Raw)
		e = s.Store.SaveRenewedCertificate(r.Context(), oldSerial, serial.Text(16), hub.ID, hub.Epoch, expiry, certPEM, hash[:])
	}
	if errors.Is(e, ErrDenied) {
		fail(w, 403, "forbidden")
		return
	}
	if e != nil {
		internal(w, e)
		return
	}
	out(w, 200, map[string]any{"certificate_pem": certPEM, "expires_at": expiry, "ownership_epoch": hub.Epoch})
}
func (s Service) enroll(w http.ResponseWriter, r *http.Request) {
	hubID := chi.URLParam(r, "hub")
	if !uuidOK(hubID) {
		fail(w, 404, "not_found")
		return
	}
	var in struct {
		LocalHubID      string `json:"local_hub_id"`
		CSRPem          string `json:"csr_pem"`
		Nonce           string `json:"nonce"`
		SignedAt        string `json:"signed_at"`
		DeviceSignature string `json:"device_signature"`
	}
	if !read(w, r, &in) {
		return
	}
	csr, der, e := parseCSR(in.CSRPem)
	if e != nil {
		fail(w, 400, "invalid_request")
		return
	}
	sig, e := decode64(in.DeviceSignature, 64)
	if e != nil {
		fail(w, 403, "forbidden")
		return
	}
	hub, e := s.Store.Enrollment(r.Context(), hubID, in.LocalHubID, in.Nonce, in.SignedAt, der, sig)
	if errors.Is(e, ErrDenied) {
		fail(w, 403, "forbidden")
		return
	}
	if e != nil {
		internal(w, e)
		return
	}
	hash := sha256.Sum256(der)
	replay, found, e := s.Store.EnrollmentReplay(r.Context(), hubID, in.Nonce, hash[:])
	if found {
		switch {
		case errors.Is(e, ErrKeyReuse):
			fail(w, 409, "idempotency_key_reuse")
		case e != nil:
			fail(w, 403, "forbidden")
		default:
			out(w, 200, map[string]any{"certificate_pem": replay.PEM, "expires_at": replay.Expiry, "ownership_epoch": replay.Epoch})
		}
		return
	}
	if e != nil {
		internal(w, e)
		return
	}
	s.issue(w, r, hub, csr, in.Nonce, "")
}
func (s Service) renew(w http.ResponseWriter, r *http.Request) {
	hubID := chi.URLParam(r, "hub")
	if !uuidOK(hubID) {
		fail(w, 404, "not_found")
		return
	}
	if r.TLS == nil || len(r.TLS.VerifiedChains) == 0 || len(r.TLS.PeerCertificates) == 0 {
		fail(w, 403, "forbidden")
		return
	}
	peer := r.TLS.PeerCertificates[0]
	var in struct {
		CSRPem         string `json:"csr_pem"`
		OwnershipEpoch int64  `json:"ownership_epoch"`
	}
	if !read(w, r, &in) {
		return
	}
	if in.OwnershipEpoch < 0 {
		fail(w, 400, "invalid_request")
		return
	}
	peerHub, peerEpoch, ok := peerIdentity(peer)
	if !ok || peerHub != hubID {
		fail(w, 403, "forbidden")
		return
	}
	csr, _, e := parseCSR(in.CSRPem)
	if e != nil {
		fail(w, 400, "invalid_request")
		return
	}
	hash := sha256.Sum256(csr.Raw)
	replay, found, e := s.Store.RenewalReplay(r.Context(), hubID, peer.SerialNumber.Text(16), peerEpoch, in.OwnershipEpoch, hash[:])
	if found {
		switch {
		case errors.Is(e, ErrKeyReuse):
			fail(w, 409, "idempotency_key_reuse")
		case e != nil:
			fail(w, 403, "forbidden")
		default:
			out(w, 200, map[string]any{"certificate_pem": replay.PEM, "expires_at": replay.Expiry, "ownership_epoch": replay.Epoch})
		}
		return
	}
	if e != nil {
		internal(w, e)
		return
	}
	hub, e := s.Store.VerifyCertificate(r.Context(), hubID, peer.SerialNumber.Text(16), peerEpoch)
	if e != nil || hub.Epoch != in.OwnershipEpoch {
		fail(w, 403, "forbidden")
		return
	}
	s.issue(w, r, hub, csr, "", peer.SerialNumber.Text(16))
}
func peerIdentity(cert *x509.Certificate) (string, int64, bool) {
	if len(cert.URIs) != 1 {
		return "", 0, false
	}
	u := cert.URIs[0]
	if u.Scheme != "spiffe" || u.Host != "energylens.local" || u.RawQuery != "" || u.Fragment != "" {
		return "", 0, false
	}
	parts := strings.Split(u.Path, "/")
	if len(parts) != 5 || parts[1] != "hubs" || parts[3] != "epochs" || !uuidOK(parts[2]) {
		return "", 0, false
	}
	epoch, e := strconv.ParseInt(parts[4], 10, 64)
	return parts[2], epoch, e == nil && epoch > 0
}
func internal(w http.ResponseWriter, e error) {
	slog.Error("hub trust failure", "error", e)
	fail(w, 500, "internal_error")
}

// PeerIdentity parses the signed hub identity already used by hub trust routes.
func PeerIdentity(cert *x509.Certificate) (string, int64, bool) { return peerIdentity(cert) }

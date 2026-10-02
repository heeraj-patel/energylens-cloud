package telemetry

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"energylens.local/cloud/internal/hubtrust"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

const maxBody = 256 << 10
const maxEvents = 128

var ErrDenied = errors.New("hub certificate or ownership epoch denied")
var ErrConflict = errors.New("batch or event conflict")
var ErrReference = errors.New("sensor or attachment not current")

type Event struct {
	Kind                 string     `json:"kind"`
	EventID              string     `json:"event_id,omitempty"`
	GapID                string     `json:"gap_id,omitempty"`
	Revision             int64      `json:"revision"`
	SensorID             string     `json:"sensor_id"`
	Timestamp            *time.Time `json:"timestamp,omitempty"`
	Watts                *float64   `json:"watts,omitempty"`
	KWhCumulative        *float64   `json:"kwh_cumulative,omitempty"`
	SourceMethod         *string    `json:"source_method,omitempty"`
	SourceTransport      *string    `json:"source_transport,omitempty"`
	Start                *time.Time `json:"start,omitempty"`
	End                  *time.Time `json:"end,omitempty"`
	EnergyWh             *float64   `json:"energy_wh,omitempty"`
	CoveredSeconds       *int64     `json:"covered_seconds,omitempty"`
	AttachmentRevisionID *string    `json:"attachment_revision_id,omitempty"`
	Fuel                 *string    `json:"fuel,omitempty"`
	Quality              *string    `json:"quality,omitempty"`
	Source               *string    `json:"source,omitempty"`
	FirstTimestamp       *time.Time `json:"first_timestamp,omitempty"`
	LastTimestamp        *time.Time `json:"last_timestamp,omitempty"`
	Reason               *string    `json:"reason,omitempty"`
	DroppedCount         *int64     `json:"dropped_count,omitempty"`
}

func (e Event) id() string {
	if e.Kind == "coverage_gap" {
		return e.GapID
	}
	return e.EventID
}

type Batch struct {
	BatchID        string  `json:"batch_id"`
	OwnershipEpoch int64   `json:"ownership_epoch"`
	SchemaVersion  int     `json:"schema_version"`
	Events         []Event `json:"events"`
}
type Acknowledgement struct {
	BatchID          string    `json:"batch_id"`
	OwnershipEpoch   int64     `json:"ownership_epoch"`
	AcceptedEventIDs []string  `json:"accepted_event_ids"`
	Cursor           int64     `json:"cursor"`
	AcknowledgedAt   time.Time `json:"acknowledged_at"`
}
type Service struct{ Store Store }

func (s Service) HubRoutes(r chi.Router) { r.Post("/hubs/{hub}/sync/batches", s.upload) }
func validUUID(s string) bool            { v, e := uuid.Parse(s); return e == nil && v.String() == s }
func finiteNonnegative(v *float64) bool {
	return v != nil && !math.IsNaN(*v) && !math.IsInf(*v, 0) && *v >= 0
}
func within(t time.Time, now time.Time) bool {
	return !t.Before(now.Add(-30*24*time.Hour)) && !t.After(now.Add(10*time.Minute))
}
func (e Event) valid(now time.Time) bool {
	if !validUUID(e.id()) || !validUUID(e.SensorID) || e.Revision < 1 || e.Revision > 1000 {
		return false
	}
	switch e.Kind {
	case "power_point":
		return e.GapID == "" && e.Timestamp != nil && within(*e.Timestamp, now) && finiteNonnegative(e.Watts) &&
			(e.KWhCumulative == nil || finiteNonnegative(e.KWhCumulative)) && e.Start == nil && e.End == nil && e.EnergyWh == nil && e.CoveredSeconds == nil && e.AttachmentRevisionID == nil && e.FirstTimestamp == nil && e.LastTimestamp == nil && e.Reason == nil && e.DroppedCount == nil && e.Fuel == nil && e.Quality == nil && e.Source == nil && validSource(e.SourceMethod) && validSource(e.SourceTransport)
	case "energy_interval":
		if e.GapID != "" || e.Timestamp != nil || e.Watts != nil || e.KWhCumulative != nil || e.Start == nil || e.End == nil || !e.End.After(*e.Start) || !within(*e.Start, now) || !within(*e.End, now) || !finiteNonnegative(e.EnergyWh) || e.CoveredSeconds == nil || *e.CoveredSeconds < 1 || *e.CoveredSeconds > int64(e.End.Sub(*e.Start).Seconds()) || e.AttachmentRevisionID == nil || !validUUID(*e.AttachmentRevisionID) || e.Fuel == nil || *e.Fuel != "electricity" || e.Quality == nil || *e.Quality != "metered" || e.Source == nil || *e.Source != "verified_driver" || e.SourceTransport != nil || e.FirstTimestamp != nil || e.LastTimestamp != nil || e.Reason != nil || e.DroppedCount != nil {
			return false
		}
		return validSource(e.SourceMethod)
	case "coverage_gap":
		return e.EventID == "" && e.Revision == 1 && e.FirstTimestamp != nil && e.LastTimestamp != nil && !e.LastTimestamp.Before(*e.FirstTimestamp) && within(*e.FirstTimestamp, now) && within(*e.LastTimestamp, now) && e.DroppedCount != nil && *e.DroppedCount > 0 && *e.DroppedCount <= 100000000 && validReason(e.Reason) && e.Timestamp == nil && e.Watts == nil && e.KWhCumulative == nil && e.Start == nil && e.End == nil && e.EnergyWh == nil && e.CoveredSeconds == nil && e.AttachmentRevisionID == nil && e.Fuel == nil && e.Quality == nil && e.Source == nil && e.SourceMethod == nil && e.SourceTransport == nil
	}
	return false
}
func validReason(v *string) bool {
	if v == nil {
		return false
	}
	switch *v {
	case "age_limit", "byte_limit", "counter_reset", "offline", "unknown":
		return true
	}
	return false
}
func validSource(v *string) bool { return v == nil || (len(*v) <= 128 && strings.TrimSpace(*v) == *v) }
func (b Batch) valid(now time.Time) bool {
	if !validUUID(b.BatchID) || b.OwnershipEpoch < 1 || b.SchemaVersion != 1 || len(b.Events) < 1 || len(b.Events) > maxEvents {
		return false
	}
	seen := map[string]bool{}
	for _, e := range b.Events {
		if !e.valid(now) {
			return false
		}
		key := e.id() + "/" + strconv.FormatInt(e.Revision, 10)
		if seen[key] {
			return false
		}
		seen[key] = true
	}
	return true
}
func fail(w http.ResponseWriter, status int, code string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"code": code, "message": http.StatusText(status), "request_id": uuid.NewString(), "details": map[string]any{}})
}
func (s Service) upload(w http.ResponseWriter, r *http.Request) {
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
	if err := s.Store.VerifyCurrent(r.Context(), hub, peer.SerialNumber.Text(16), epoch); err != nil {
		if errors.Is(err, ErrDenied) {
			fail(w, 403, "forbidden")
		} else {
			slog.Error("telemetry hub verification failed", "error", err)
			fail(w, 500, "internal_error")
		}
		return
	}
	if !strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
		fail(w, 415, "invalid_request")
		return
	}
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBody))
	if err != nil {
		fail(w, 413, "payload_too_large")
		return
	}
	var b Batch
	d := json.NewDecoder(strings.NewReader(string(raw)))
	d.DisallowUnknownFields()
	if d.Decode(&b) != nil || d.Decode(new(any)) != io.EOF || !b.valid(time.Now().UTC()) {
		fail(w, 400, "invalid_request")
		return
	}
	if b.OwnershipEpoch != epoch {
		fail(w, 409, "ownership_epoch_mismatch")
		return
	}
	hash := sha256.Sum256(raw)
	ack, err := s.Store.Accept(r.Context(), hub, peer.SerialNumber.Text(16), epoch, b, hash[:])
	switch {
	case errors.Is(err, ErrDenied):
		fail(w, 403, "forbidden")
	case errors.Is(err, ErrReference):
		fail(w, 409, "invalid_telemetry_reference")
	case errors.Is(err, ErrConflict):
		fail(w, 409, "telemetry_conflict")
	case err != nil:
		slog.Error("telemetry ingest failed", "error", err)
		fail(w, 500, "internal_error")
	default:
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(ack)
	}
}

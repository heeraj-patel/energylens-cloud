package kasaruntime

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"strconv"
	"strings"
	"time"

	"energylens.local/cloud/internal/hubtrust"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

const maxBody = 128 << 10
const maxSnapshots = 64

var ErrDenied = errors.New("current hub certificate denied")
var ErrReference = errors.New("sensor or attachment reference denied")
var ErrConflict = errors.New("snapshot id reused with different payload")

type Snapshot struct {
	SnapshotID       string     `json:"snapshot_id"`
	SensorID         string     `json:"sensor_id"`
	AttachmentID     *string    `json:"attachment_id,omitempty"`
	Scope            string     `json:"scope"`
	WindowLabel      string     `json:"window_label"`
	NativeUnit       string     `json:"native_unit"`
	NativeMinutes    int64      `json:"native_minutes"`
	DurationSeconds  int64      `json:"duration_seconds"`
	ReportedStart    *time.Time `json:"reported_start,omitempty"`
	ReportedEnd      *time.Time `json:"reported_end,omitempty"`
	DeviceRegion     *string    `json:"device_region,omitempty"`
	DeviceClockValid bool       `json:"device_clock_valid"`
	ObservedAt       time.Time  `json:"observed_at"`
	Method           string     `json:"method"`
	Transport        string     `json:"transport"`
	Firmware         *string    `json:"firmware,omitempty"`
	Quality          string     `json:"quality"`
}

type Upload struct {
	OwnershipEpoch int64      `json:"ownership_epoch"`
	Snapshots      []Snapshot `json:"snapshots"`
}

type Service struct {
	Store  Store
	Auth   func(http.Handler) http.Handler
	UserID func(*http.Request) string
}

func (s Service) AccountRoutes(r chi.Router) {
	r.With(s.Auth).Get("/homes/{home}/kasa-runtime", s.list)
}
func (s Service) HubRoutes(r chi.Router) {
	r.Post("/hubs/{hub}/kasa-runtime/snapshots", s.upload)
}
func validUUID(v string) bool { id, err := uuid.Parse(v); return err == nil && id.String() == v }
func validText(v *string, max int) bool {
	return v == nil || (len(*v) > 0 && len(*v) <= max && strings.TrimSpace(*v) == *v)
}
func (v Snapshot) valid(now time.Time) bool {
	if !validUUID(v.SnapshotID) || !validUUID(v.SensorID) || v.Scope != "single_plug" ||
		(v.WindowLabel != "today" && v.WindowLabel != "past7" && v.WindowLabel != "past30") ||
		v.NativeUnit != "minutes" || v.NativeMinutes < 0 || v.NativeMinutes > 43200 ||
		v.DurationSeconds != v.NativeMinutes*60 || v.ObservedAt.IsZero() ||
		v.ObservedAt.Before(now.Add(-30*24*time.Hour)) || v.ObservedAt.After(now.Add(10*time.Minute)) ||
		!validText(v.DeviceRegion, 64) || !validText(v.Firmware, 128) ||
		(v.DeviceClockValid && v.Quality != "protocol_reported_clock_verified" && v.Quality != "protocol_reported_clock_unverified") ||
		(!v.DeviceClockValid && v.Quality != "protocol_reported_clock_unverified") {
		return false
	}
	if (v.ReportedStart == nil) != (v.ReportedEnd == nil) {
		return false
	}
	if v.ReportedStart != nil && (!v.ReportedEnd.After(*v.ReportedStart) || v.ReportedEnd.After(v.ObservedAt) || v.ReportedStart.Before(now.Add(-31*24*time.Hour))) {
		return false
	}
	if v.AttachmentID != nil && (!validUUID(*v.AttachmentID) || v.ReportedStart == nil || !v.DeviceClockValid || v.Quality != "protocol_reported_clock_verified") {
		return false
	}
	return (v.Method == "get_device_usage" && v.Transport == "kasa_http_klap") ||
		(v.Method == "get_daystat" && v.Transport == "kasa_legacy_tcp")
}
func (u Upload) valid(now time.Time) bool {
	if u.OwnershipEpoch < 1 || len(u.Snapshots) < 1 || len(u.Snapshots) > maxSnapshots {
		return false
	}
	seen := map[string]bool{}
	for _, v := range u.Snapshots {
		if !v.valid(now) || seen[v.SnapshotID] {
			return false
		}
		seen[v.SnapshotID] = true
	}
	return true
}
func fail(w http.ResponseWriter, status int, code string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"code": code, "message": http.StatusText(status), "request_id": uuid.NewString(), "details": map[string]any{}})
}
func verifyPeer(r *http.Request, hub string) (string, int64, bool) {
	if r.TLS == nil || len(r.TLS.VerifiedChains) == 0 || len(r.TLS.PeerCertificates) == 0 {
		return "", 0, false
	}
	peer := r.TLS.PeerCertificates[0]
	peerHub, epoch, ok := hubtrust.PeerIdentity(peer)
	return peer.SerialNumber.Text(16), epoch, ok && peerHub == hub
}
func (s Service) upload(w http.ResponseWriter, r *http.Request) {
	hub := chi.URLParam(r, "hub")
	if !validUUID(hub) {
		fail(w, 404, "not_found")
		return
	}
	serial, epoch, ok := verifyPeer(r, hub)
	if !ok {
		fail(w, 403, "forbidden")
		return
	}
	mediaType, _, mediaErr := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if mediaErr != nil || mediaType != "application/json" {
		fail(w, 415, "invalid_request")
		return
	}
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBody))
	if err != nil {
		fail(w, 413, "payload_too_large")
		return
	}
	var upload Upload
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&upload) != nil || decoder.Decode(new(any)) != io.EOF || !upload.valid(time.Now().UTC()) {
		fail(w, 400, "invalid_request")
		return
	}
	if upload.OwnershipEpoch != epoch {
		fail(w, 409, "ownership_epoch_mismatch")
		return
	}
	accepted, err := s.Store.Accept(r.Context(), hub, serial, epoch, upload)
	switch {
	case errors.Is(err, ErrDenied):
		fail(w, 403, "forbidden")
	case errors.Is(err, ErrReference):
		fail(w, 409, "invalid_runtime_reference")
	case errors.Is(err, ErrConflict):
		fail(w, 409, "runtime_conflict")
	case err != nil:
		slog.Error("Kasa runtime ingest failed", "error", err)
		fail(w, 500, "internal_error")
	default:
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"accepted_snapshot_ids": accepted})
	}
}

type pageCursor struct {
	Home string    `json:"home"`
	From time.Time `json:"from"`
	To   time.Time `json:"to"`
	At   time.Time `json:"at"`
	ID   string    `json:"id"`
}

func decodeCursor(raw string) (pageCursor, error) {
	var c pageCursor
	if len(raw) > 2048 {
		return c, errors.New("cursor too long")
	}
	body, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil {
		return c, err
	}
	err = json.Unmarshal(body, &c)
	return c, err
}
func encodeCursor(c pageCursor) string {
	body, _ := json.Marshal(c)
	return base64.RawURLEncoding.EncodeToString(body)
}
func (s Service) list(w http.ResponseWriter, r *http.Request) {
	home := chi.URLParam(r, "home")
	if !validUUID(home) {
		fail(w, 404, "not_found")
		return
	}
	limit := 50
	if raw := r.URL.Query().Get("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 || n > 100 {
			fail(w, 400, "invalid_request")
			return
		}
		limit = n
	}
	now := time.Now().UTC()
	c := pageCursor{Home: home, From: now.Add(-30 * 24 * time.Hour), To: now}
	if raw := r.URL.Query().Get("cursor"); raw != "" {
		var err error
		c, err = decodeCursor(raw)
		if err != nil || c.Home != home || !validUUID(c.ID) || c.To.After(now.Add(10*time.Minute)) || c.From.Before(now.Add(-31*24*time.Hour)) || !c.To.After(c.From) || c.At.Before(c.From) || c.At.After(c.To) {
			fail(w, 400, "invalid_request")
			return
		}
	}
	rows, err := s.Store.Page(r.Context(), home, s.UserID(r), c, limit+1)
	if errors.Is(err, ErrReference) {
		fail(w, 404, "not_found")
		return
	}
	if err != nil {
		slog.Error("Kasa runtime read failed", "error", err)
		fail(w, 500, "internal_error")
		return
	}
	more := len(rows) > limit
	if more {
		rows = rows[:limit]
	}
	next := any(nil)
	if more {
		last := rows[len(rows)-1]
		c.At, c.ID = last.ObservedAt, last.SnapshotID
		next = encodeCursor(c)
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"snapshots": rows, "next_cursor": next, "from": c.From, "to": c.To})
}

package reports

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

type PreviewStore interface {
	Create(rctx context.Context, owner, home string, selected []uuid.UUID, key uuid.UUID, now time.Time) (Preview, error)
	Get(rctx context.Context, owner, home, previewID string, now time.Time) (Preview, error)
	QueueDelivery(ctx context.Context, owner, home string, in DeliveryInput, now time.Time) (DeliveryRequest, error)
}

type Service struct {
	Store  PreviewStore
	Auth   func(http.Handler) http.Handler
	UserID func(*http.Request) string
	// DirectSend invokes the configured provider for an approved direct request.
	DirectSend func(context.Context, string) (bool, error)
}

type directDeliveryStore interface {
	Delivery(context.Context, string, string, string) (DeliveryRequest, error)
}

func (s Service) Routes(r chi.Router) {
	r.With(s.Auth).Post("/homes/{home}/reports/preview", s.create)
	r.With(s.Auth).Get("/homes/{home}/reports/previews/{preview}", s.get)
	r.With(s.Auth).Post("/homes/{home}/reports/previews/{preview}/deliveries", s.enqueue)
}

type deliveryRequestBody struct {
	ApprovedSnapshotSHA256 string   `json:"approved_snapshot_sha256"`
	Recipients             []string `json:"recipients"`
	Approved               bool     `json:"approved"`
	SendNow                bool     `json:"send_now,omitempty"`
}

func (s Service) enqueue(w http.ResponseWriter, r *http.Request) {
	home, previewID := chi.URLParam(r, "home"), chi.URLParam(r, "preview")
	if !canonicalUUID(home) || !canonicalUUID(previewID) {
		respondError(w, http.StatusNotFound, "not_found")
		return
	}
	if strings.TrimSpace(strings.Split(r.Header.Get("Content-Type"), ";")[0]) != "application/json" {
		respondError(w, http.StatusUnsupportedMediaType, "unsupported_media_type")
		return
	}
	keyRaw := r.Header.Get("Idempotency-Key")
	key, err := uuid.Parse(keyRaw)
	if err != nil || key == uuid.Nil || keyRaw != key.String() {
		respondError(w, http.StatusBadRequest, "invalid_request")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 4096)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	var input deliveryRequestBody
	if err := dec.Decode(&input); err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			respondError(w, http.StatusRequestEntityTooLarge, "request_too_large")
		} else {
			respondError(w, http.StatusBadRequest, "invalid_request")
		}
		return
	}
	var extra any
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		respondError(w, http.StatusBadRequest, "invalid_request")
		return
	}
	if !input.Approved {
		respondError(w, http.StatusBadRequest, "invalid_request")
		return
	}
	if len(input.ApprovedSnapshotSHA256) != 64 || strings.ToLower(input.ApprovedSnapshotSHA256) != input.ApprovedSnapshotSHA256 {
		respondError(w, http.StatusBadRequest, "invalid_request")
		return
	}
	if _, err := hex.DecodeString(input.ApprovedSnapshotSHA256); err != nil {
		respondError(w, http.StatusBadRequest, "invalid_request")
		return
	}
	if _, err := normalizeRecipients(input.Recipients); err != nil {
		respondError(w, http.StatusBadRequest, "invalid_request")
		return
	}
	if input.SendNow && (s.DirectSend == nil || s.Store == nil) {
		respondError(w, http.StatusServiceUnavailable, "delivery_unavailable")
		return
	}
	if input.SendNow {
		if _, ok := s.Store.(directDeliveryStore); !ok {
			respondError(w, http.StatusServiceUnavailable, "delivery_unavailable")
			return
		}
	}
	request, err := s.Store.QueueDelivery(r.Context(), s.UserID(r), home, DeliveryInput{
		PreviewID: previewID, ApprovedSnapshotSHA256: input.ApprovedSnapshotSHA256,
		Recipients: input.Recipients, Approved: input.Approved, IdempotencyKey: key,
		SendNow: input.SendNow,
	}, time.Now().UTC())
	switch {
	case errors.Is(err, ErrNotFound):
		respondError(w, http.StatusNotFound, "not_found")
	case errors.Is(err, ErrInvalidRecipients), errors.Is(err, ErrApprovalRequired):
		respondError(w, http.StatusBadRequest, "invalid_request")
	case errors.Is(err, ErrKeyReuse):
		respondError(w, http.StatusConflict, "idempotency_key_reuse")
	case errors.Is(err, ErrDeliveryModeConflict):
		respondError(w, http.StatusConflict, "delivery_mode_conflict")
	case errors.Is(err, ErrSnapshotMismatch):
		respondError(w, http.StatusConflict, "snapshot_mismatch")
	case errors.Is(err, ErrStalePreview):
		respondError(w, http.StatusConflict, "preview_stale")
	case errors.Is(err, ErrDeliveryRate):
		respondError(w, http.StatusTooManyRequests, "delivery_rate_limited")
	case err != nil:
		slog.Error("report delivery enqueue failed", "error", err)
		respondError(w, http.StatusInternalServerError, "internal_error")
	default:
		if input.SendNow {
			if request.State != "sent" {
				_, sendErr := s.DirectSend(r.Context(), request.ID)
				if sendErr != nil {
					slog.Warn("direct report provider acceptance unconfirmed", "error", sendErr)
				}
			}
			current, readErr := s.Store.(directDeliveryStore).Delivery(r.Context(), s.UserID(r), home, request.ID)
			if readErr != nil {
				respondError(w, http.StatusServiceUnavailable, "delivery_unconfirmed")
				return
			}
			if current.State != "sent" {
				if current.State == "canceled" {
					respondError(w, http.StatusBadGateway, "delivery_failed")
				} else {
					respondError(w, http.StatusServiceUnavailable, "delivery_unconfirmed")
				}
				return
			}
			respondJSON(w, http.StatusCreated, current)
			return
		}
		respondJSON(w, http.StatusCreated, request)
	}
}

type previewRequest struct {
	ApplianceIDs []string `json:"appliance_ids"`
}

func (s Service) create(w http.ResponseWriter, r *http.Request) {
	home := chi.URLParam(r, "home")
	if !canonicalUUID(home) {
		respondError(w, http.StatusNotFound, "not_found")
		return
	}
	if strings.TrimSpace(strings.Split(r.Header.Get("Content-Type"), ";")[0]) != "application/json" {
		respondError(w, http.StatusUnsupportedMediaType, "unsupported_media_type")
		return
	}
	keyRaw := r.Header.Get("Idempotency-Key")
	key, err := uuid.Parse(keyRaw)
	if err != nil || key == uuid.Nil || keyRaw != key.String() {
		respondError(w, http.StatusBadRequest, "invalid_request")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 4096)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	var input previewRequest
	if err := dec.Decode(&input); err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			respondError(w, http.StatusRequestEntityTooLarge, "request_too_large")
		} else {
			respondError(w, http.StatusBadRequest, "invalid_request")
		}
		return
	}
	var extra any
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		respondError(w, http.StatusBadRequest, "invalid_request")
		return
	}
	if len(input.ApplianceIDs) == 0 || len(input.ApplianceIDs) > 25 {
		respondError(w, http.StatusBadRequest, "invalid_request")
		return
	}
	selected := make([]uuid.UUID, 0, len(input.ApplianceIDs))
	seen := map[uuid.UUID]bool{}
	for _, raw := range input.ApplianceIDs {
		id, err := uuid.Parse(raw)
		if err != nil || raw != id.String() || id == uuid.Nil || seen[id] {
			respondError(w, http.StatusBadRequest, "invalid_request")
			return
		}
		selected = append(selected, id)
		seen[id] = true
	}
	preview, err := s.Store.Create(r.Context(), s.UserID(r), home, selected, key, time.Now().UTC())
	if errors.Is(err, ErrNotFound) {
		respondError(w, http.StatusNotFound, "not_found")
		return
	}
	if errors.Is(err, ErrInvalidSelection) {
		respondError(w, http.StatusBadRequest, "invalid_request")
		return
	}
	if errors.Is(err, ErrKeyReuse) {
		respondError(w, http.StatusConflict, "idempotency_key_reuse")
		return
	}
	if err != nil {
		slog.Error("report preview create failed", "error", err)
		respondError(w, http.StatusInternalServerError, "internal_error")
		return
	}
	respondJSON(w, http.StatusCreated, preview)
}

func (s Service) get(w http.ResponseWriter, r *http.Request) {
	home, id := chi.URLParam(r, "home"), chi.URLParam(r, "preview")
	if !canonicalUUID(home) || !canonicalUUID(id) {
		respondError(w, http.StatusNotFound, "not_found")
		return
	}
	preview, err := s.Store.Get(r.Context(), s.UserID(r), home, id, time.Now().UTC())
	if errors.Is(err, ErrNotFound) {
		respondError(w, http.StatusNotFound, "not_found")
		return
	}
	if err != nil {
		slog.Error("report preview read failed", "error", err)
		respondError(w, http.StatusInternalServerError, "internal_error")
		return
	}
	respondJSON(w, http.StatusOK, preview)
}

func canonicalUUID(raw string) bool {
	id, err := uuid.Parse(raw)
	return err == nil && id != uuid.Nil && raw == id.String()
}

func respondJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "private, no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func respondError(w http.ResponseWriter, status int, code string) {
	respondJSON(w, status, map[string]any{"code": code, "message": http.StatusText(status), "request_id": uuid.NewString(), "details": map[string]any{}})
}

package meteredhistory

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

type Service struct {
	Store  Store
	Auth   func(http.Handler) http.Handler
	UserID func(*http.Request) string
}

func (s Service) Routes(r chi.Router) { r.With(s.Auth).Get("/homes/{home}/metered-history", s.list) }

type pageCursor struct {
	HomeID         string    `json:"home_id"`
	From           time.Time `json:"from"`
	To             time.Time `json:"to"`
	SensorID       string    `json:"sensor_id,omitempty"`
	AttachmentID   string    `json:"attachment_id,omitempty"`
	SnapshotCursor int64     `json:"snapshot_cursor"`
	AsOf           time.Time `json:"as_of"`
	AfterTime      time.Time `json:"after_time"`
	AfterHub       string    `json:"after_hub"`
	AfterEpoch     int64     `json:"after_epoch"`
	AfterEvent     string    `json:"after_event"`
}

func uuidOK(s string) bool { v, e := uuid.Parse(s); return e == nil && v.String() == s }
func fail(w http.ResponseWriter, status int, code string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"code": code, "message": http.StatusText(status), "request_id": uuid.NewString(), "details": map[string]any{}})
}
func validRange(from, to, now time.Time) bool {
	return !from.IsZero() && !to.IsZero() && to.After(from) && to.Sub(from) <= 7*24*time.Hour && !from.Before(now.Add(-30*24*time.Hour)) && !to.After(now.Add(10*time.Minute))
}
func decodeCursor(raw string) (pageCursor, error) {
	var c pageCursor
	if len(raw) > 2048 {
		return c, errors.New("cursor too long")
	}
	b, e := base64.RawURLEncoding.DecodeString(raw)
	if e != nil {
		return c, e
	}
	e = json.Unmarshal(b, &c)
	if e != nil {
		return c, e
	}
	if !uuidOK(c.HomeID) || c.SnapshotCursor < 0 || c.AfterTime.IsZero() || !uuidOK(c.AfterHub) || c.AfterEpoch < 1 || !uuidOK(c.AfterEvent) {
		return c, errors.New("invalid cursor")
	}
	return c, nil
}
func encodeCursor(c pageCursor) string {
	b, _ := json.Marshal(c)
	return base64.RawURLEncoding.EncodeToString(b)
}
func (s Service) list(w http.ResponseWriter, r *http.Request) {
	home := chi.URLParam(r, "home")
	if !uuidOK(home) {
		fail(w, 404, "not_found")
		return
	}
	params := r.URL.Query()
	limit := 50
	if raw := params.Get("limit"); raw != "" {
		v, e := strconv.Atoi(raw)
		if e != nil || v < 1 || v > 100 {
			fail(w, 400, "invalid_request")
			return
		}
		limit = v
	}
	now := time.Now().UTC()
	q := Query{HomeID: home, OwnerID: s.UserID(r), Limit: limit}
	var asOf time.Time
	if raw := params.Get("cursor"); raw != "" {
		if params.Has("from") || params.Has("to") || params.Has("sensor_id") || params.Has("attachment_id") {
			fail(w, 400, "invalid_request")
			return
		}
		c, e := decodeCursor(raw)
		if e != nil || c.HomeID != home || !validRange(c.From, c.To, now) {
			fail(w, 400, "invalid_request")
			return
		}
		q.From, q.To, q.SensorID, q.AttachmentID, q.SnapshotCursor, q.AfterTime, q.AfterHub, q.AfterEpoch, q.AfterEvent = c.From, c.To, c.SensorID, c.AttachmentID, c.SnapshotCursor, &c.AfterTime, c.AfterHub, c.AfterEpoch, c.AfterEvent
		asOf = c.AsOf
	} else {
		rawFrom, rawTo := params.Get("from"), params.Get("to")
		if rawFrom == "" && rawTo == "" {
			q.To = now
			q.From = now.Add(-24 * time.Hour)
		} else if rawFrom == "" || rawTo == "" {
			fail(w, 400, "invalid_request")
			return
		} else {
			var e error
			q.From, e = time.Parse(time.RFC3339, rawFrom)
			if e != nil {
				fail(w, 400, "invalid_request")
				return
			}
			q.To, e = time.Parse(time.RFC3339, rawTo)
			if e != nil {
				fail(w, 400, "invalid_request")
				return
			}
		}
		if !validRange(q.From, q.To, now) {
			fail(w, 400, "invalid_request")
			return
		}
		q.SensorID = params.Get("sensor_id")
		q.AttachmentID = params.Get("attachment_id")
		if q.SensorID != "" && !uuidOK(q.SensorID) || q.AttachmentID != "" && !uuidOK(q.AttachmentID) {
			fail(w, 400, "invalid_request")
			return
		}
	}
	if e := s.Store.Scope(r.Context(), q); e != nil {
		if errors.Is(e, ErrNotFound) {
			fail(w, 404, "not_found")
		} else {
			slog.Error("metered history scope failed", "error", e)
			fail(w, 500, "internal_error")
		}
		return
	}
	if q.AfterTime == nil {
		var e error
		q.SnapshotCursor, asOf, e = s.Store.Snapshot(r.Context())
		if e != nil {
			slog.Error("metered history snapshot failed", "error", e)
			fail(w, 500, "internal_error")
			return
		}
	}
	rows, e := s.Store.Page(r.Context(), q)
	if e != nil {
		slog.Error("metered history page failed", "error", e)
		fail(w, 500, "internal_error")
		return
	}
	more := len(rows) > q.Limit
	if more {
		rows = rows[:q.Limit]
	}
	events := make([]json.RawMessage, 0, len(rows))
	for _, row := range rows {
		events = append(events, row.Payload)
	}
	var next any
	if more {
		last := rows[len(rows)-1]
		next = encodeCursor(pageCursor{HomeID: home, From: q.From, To: q.To, SensorID: q.SensorID, AttachmentID: q.AttachmentID, SnapshotCursor: q.SnapshotCursor, AsOf: asOf, AfterTime: last.SortTime, AfterHub: last.HubID, AfterEpoch: last.Epoch, AfterEvent: last.EventID})
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"events": events, "as_of": asOf, "as_of_cursor": q.SnapshotCursor, "next_cursor": next})
}

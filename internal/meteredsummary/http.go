package meteredsummary

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

func (s Service) Routes(r chi.Router) { r.With(s.Auth).Get("/homes/{home}/metered-summaries", s.list) }

type cursor struct {
	Home, Granularity, Sensor, Attachment string
	From, To                              time.Time
	After                                 Key
}

func uuidOK(s string) bool { v, e := uuid.Parse(s); return e == nil && v.String() == s }
func bad(w http.ResponseWriter, status int, code string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"code": code, "message": http.StatusText(status), "request_id": uuid.NewString(), "details": map[string]any{}})
}
func validRange(from, to, now time.Time) bool {
	return !from.IsZero() && !to.IsZero() && to.After(from) && !from.Before(now.AddDate(-2, 0, 0)) && !to.After(now.Add(10*time.Minute)) && !to.After(from.AddDate(2, 0, 0))
}
func decodeCursor(raw string) (cursor, error) {
	var c cursor
	if len(raw) > 2048 {
		return c, errors.New("cursor too long")
	}
	b, e := base64.RawURLEncoding.DecodeString(raw)
	if e != nil {
		return c, e
	}
	if e = json.Unmarshal(b, &c); e != nil {
		return c, e
	}
	if !uuidOK(c.Home) || !uuidOK(c.After.Hub) || !uuidOK(c.After.Sensor) || c.After.Epoch < 1 || c.After.Bucket.IsZero() || c.After.Kind != "energy" && c.After.Kind != "power_sample_stats" && c.After.Kind != "coverage_gap" || c.After.Attachment != "" && !uuidOK(c.After.Attachment) {
		return c, errors.New("invalid cursor")
	}
	return c, nil
}
func encodeCursor(c cursor) string {
	b, _ := json.Marshal(c)
	return base64.RawURLEncoding.EncodeToString(b)
}
func (s Service) list(w http.ResponseWriter, r *http.Request) {
	home := chi.URLParam(r, "home")
	if !uuidOK(home) {
		bad(w, 404, "not_found")
		return
	}
	p := r.URL.Query()
	limit := 50
	if v := p.Get("limit"); v != "" {
		n, e := strconv.Atoi(v)
		if e != nil || n < 1 || n > 100 {
			bad(w, 400, "invalid_request")
			return
		}
		limit = n
	}
	now := time.Now().UTC()
	q := Query{HomeID: home, OwnerID: s.UserID(r), Limit: limit}
	if raw := p.Get("cursor"); raw != "" {
		if p.Has("from") || p.Has("to") || p.Has("granularity") || p.Has("sensor_id") || p.Has("attachment_id") {
			bad(w, 400, "invalid_request")
			return
		}
		c, e := decodeCursor(raw)
		if e != nil || c.Home != home || !validRange(c.From, c.To, now) || c.Granularity != "hour" && c.Granularity != "day" || c.Sensor != "" && !uuidOK(c.Sensor) || c.Attachment != "" && !uuidOK(c.Attachment) || c.After.Bucket.Before(c.From) || !c.After.Bucket.Before(c.To) {
			bad(w, 400, "invalid_request")
			return
		}
		q.From, q.To, q.Granularity, q.SensorID, q.AttachmentID, q.After = c.From, c.To, c.Granularity, c.Sensor, c.Attachment, &c.After
	} else {
		q.Granularity = p.Get("granularity")
		if q.Granularity != "hour" && q.Granularity != "day" {
			bad(w, 400, "invalid_request")
			return
		}
		from, to := p.Get("from"), p.Get("to")
		if from == "" && to == "" {
			q.To = now
			q.From = now.AddDate(0, 0, -30)
		} else if from == "" || to == "" {
			bad(w, 400, "invalid_request")
			return
		} else {
			var e error
			q.From, e = time.Parse(time.RFC3339, from)
			if e != nil {
				bad(w, 400, "invalid_request")
				return
			}
			q.To, e = time.Parse(time.RFC3339, to)
			if e != nil {
				bad(w, 400, "invalid_request")
				return
			}
		}
		if !validRange(q.From, q.To, now) {
			bad(w, 400, "invalid_request")
			return
		}
		q.SensorID, q.AttachmentID = p.Get("sensor_id"), p.Get("attachment_id")
		if q.SensorID != "" && !uuidOK(q.SensorID) || q.AttachmentID != "" && !uuidOK(q.AttachmentID) {
			bad(w, 400, "invalid_request")
			return
		}
	}
	if e := s.Store.Scope(r.Context(), q); e != nil {
		if errors.Is(e, ErrNotFound) {
			bad(w, 404, "not_found")
		} else {
			slog.Error("metered summary scope failed", "error", e)
			bad(w, 500, "internal_error")
		}
		return
	}
	rows, e := s.Store.Page(r.Context(), q)
	if e != nil {
		slog.Error("metered summary page failed", "error", e)
		bad(w, 500, "internal_error")
		return
	}
	more := len(rows) > limit
	if more {
		rows = rows[:limit]
	}
	items := make([]json.RawMessage, 0, len(rows))
	for _, row := range rows {
		items = append(items, row.Payload)
	}
	var next any
	if more {
		next = encodeCursor(cursor{Home: home, Granularity: q.Granularity, Sensor: q.SensorID, Attachment: q.AttachmentID, From: q.From, To: q.To, After: rows[len(rows)-1].Key})
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"granularity": q.Granularity, "summaries": items, "next_cursor": next})
}

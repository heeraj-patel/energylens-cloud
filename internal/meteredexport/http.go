// Package meteredexport serves bounded, owner-scoped exports of retained
// metering records. It keeps detailed events and each summary resolution as
// separate datasets; clients must not sum hour and day rows together.
package meteredexport

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"energylens.local/cloud/internal/meteredhistory"
	"energylens.local/cloud/internal/meteredsummary"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

type Service struct {
	History meteredhistory.Store
	Summary meteredsummary.Store
	Auth    func(http.Handler) http.Handler
	UserID  func(*http.Request) string
	// CursorKey is a dedicated, deployment-stable secret shared by API replicas.
	// PreviousCursorKey accepts cursors issued before a key rotation.
	CursorKey, PreviousCursorKey []byte
}

func (s Service) Routes(r chi.Router) {
	r.With(s.Auth).Get("/homes/{home}/metered-export", s.list)
}

type cursor struct {
	Home, Owner, Dataset, Sensor, Attachment string
	From, To                                 time.Time
	SnapshotCursor                           int64
	AsOf                                     time.Time
	HistoryAfter                             *historyKey
	SummaryAfter                             *meteredsummary.Key
}

type historyKey struct {
	Time    time.Time
	Hub     string
	Epoch   int64
	EventID string
}

var errCursorScope = errors.New("cursor scope mismatch")

func uuidOK(s string) bool { u, err := uuid.Parse(s); return err == nil && u.String() == s }

func fail(w http.ResponseWriter, status int, code string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"code": code, "message": http.StatusText(status), "request_id": uuid.NewString(), "details": map[string]any{}})
}

func validRange(c cursor, now time.Time) bool {
	if c.From.IsZero() || c.To.IsZero() || !c.To.After(c.From) || c.To.After(now.Add(10*time.Minute)) {
		return false
	}
	if c.Dataset == "history" {
		return !c.From.Before(now.Add(-30*24*time.Hour)) && c.To.Sub(c.From) <= 7*24*time.Hour
	}
	return (c.Dataset == "hour" || c.Dataset == "day") && !c.From.Before(now.AddDate(-2, 0, 0)) && !c.To.After(c.From.AddDate(2, 0, 0))
}

func validCursor(c cursor, now time.Time) bool {
	if !validRange(c, now) || c.Sensor != "" && !uuidOK(c.Sensor) || c.Attachment != "" && !uuidOK(c.Attachment) {
		return false
	}
	if c.Dataset == "history" {
		k := c.HistoryAfter
		return c.SummaryAfter == nil && c.SnapshotCursor >= 0 && !c.AsOf.IsZero() && k != nil &&
			!k.Time.Before(c.From) && k.Time.Before(c.To) && uuidOK(k.Hub) && k.Epoch > 0 && uuidOK(k.EventID)
	}
	k := c.SummaryAfter
	return c.HistoryAfter == nil && c.SnapshotCursor == 0 && c.AsOf.IsZero() && k != nil &&
		!k.Bucket.Before(c.From) && k.Bucket.Before(c.To) && uuidOK(k.Hub) && uuidOK(k.Sensor) && k.Epoch > 0 &&
		(k.Kind == "energy" || k.Kind == "power_sample_stats" || k.Kind == "coverage_gap") &&
		(k.Attachment == "" || uuidOK(k.Attachment))
}

func (s Service) decodeCursor(raw, home, owner string, now time.Time) (cursor, error) {
	var c cursor
	if len(raw) > 4096 {
		return c, errors.New("cursor too long")
	}
	parts := strings.Split(raw, ".")
	if len(parts) != 3 || parts[0] != "v1" {
		return c, errors.New("invalid cursor version")
	}
	b, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil || base64.RawURLEncoding.EncodeToString(b) != parts[1] {
		return c, errors.New("invalid cursor payload")
	}
	mac, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil || len(mac) != sha256.Size || base64.RawURLEncoding.EncodeToString(mac) != parts[2] {
		return c, errors.New("invalid cursor signature")
	}
	message := []byte(parts[0] + "." + parts[1])
	if !validMAC(s.CursorKey, message, mac) && !validMAC(s.PreviousCursorKey, message, mac) {
		return c, errors.New("invalid cursor signature")
	}
	if err = json.Unmarshal(b, &c); err != nil {
		return c, err
	}
	if c.Home != home || c.Owner != owner {
		return c, errCursorScope
	}
	if !validCursor(c, now) {
		return c, errors.New("invalid cursor")
	}
	return c, nil
}

func validMAC(key, message, signature []byte) bool {
	if len(key) < 32 {
		return false
	}
	h := hmac.New(sha256.New, key)
	_, _ = h.Write(message)
	return hmac.Equal(h.Sum(nil), signature)
}

func (s Service) encodeCursor(c cursor) string {
	b, _ := json.Marshal(c)
	message := "v1." + base64.RawURLEncoding.EncodeToString(b)
	h := hmac.New(sha256.New, s.CursorKey)
	_, _ = h.Write([]byte(message))
	return message + "." + base64.RawURLEncoding.EncodeToString(h.Sum(nil))
}

func (s Service) list(w http.ResponseWriter, r *http.Request) {
	if len(s.CursorKey) < 32 {
		internalError(w, "metered export cursor key unavailable", errors.New("missing cursor key"))
		return
	}
	home := chi.URLParam(r, "home")
	if !uuidOK(home) {
		fail(w, 404, "not_found")
		return
	}
	p := r.URL.Query()
	limit := 50
	if raw := p.Get("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 || n > 100 {
			fail(w, 400, "invalid_request")
			return
		}
		limit = n
	}
	now := time.Now().UTC()
	owner := s.UserID(r)
	c := cursor{Home: home, Owner: owner}
	if raw := p.Get("cursor"); raw != "" {
		if p.Has("dataset") || p.Has("from") || p.Has("to") || p.Has("sensor_id") || p.Has("attachment_id") {
			fail(w, 400, "invalid_request")
			return
		}
		var err error
		c, err = s.decodeCursor(raw, home, owner, now)
		if err != nil {
			if errors.Is(err, errCursorScope) {
				fail(w, 404, "not_found")
			} else {
				fail(w, 400, "invalid_request")
			}
			return
		}
	} else {
		c.Dataset, c.Sensor, c.Attachment = p.Get("dataset"), p.Get("sensor_id"), p.Get("attachment_id")
		if c.Dataset != "history" && c.Dataset != "hour" && c.Dataset != "day" || c.Sensor != "" && !uuidOK(c.Sensor) || c.Attachment != "" && !uuidOK(c.Attachment) {
			fail(w, 400, "invalid_request")
			return
		}
		from, to := p.Get("from"), p.Get("to")
		if from == "" && to == "" {
			c.To = now
			if c.Dataset == "history" {
				c.From = now.Add(-24 * time.Hour)
			} else {
				c.From = now.AddDate(0, 0, -30)
			}
		} else if from == "" || to == "" {
			fail(w, 400, "invalid_request")
			return
		} else {
			var err error
			c.From, err = time.Parse(time.RFC3339, from)
			if err != nil {
				fail(w, 400, "invalid_request")
				return
			}
			c.To, err = time.Parse(time.RFC3339, to)
			if err != nil {
				fail(w, 400, "invalid_request")
				return
			}
		}
		if !validRange(c, now) {
			fail(w, 400, "invalid_request")
			return
		}
	}
	if c.Dataset == "history" {
		s.history(w, r, c, limit)
	} else {
		s.summary(w, r, c, limit)
	}
}

func (s Service) history(w http.ResponseWriter, r *http.Request, c cursor, limit int) {
	q := meteredhistory.Query{HomeID: c.Home, OwnerID: s.UserID(r), From: c.From, To: c.To, SensorID: c.Sensor, AttachmentID: c.Attachment, Limit: limit, SnapshotCursor: c.SnapshotCursor}
	if err := s.History.Scope(r.Context(), q); err != nil {
		scopeError(w, err)
		return
	}
	if c.HistoryAfter != nil {
		q.AfterTime, q.AfterHub, q.AfterEpoch, q.AfterEvent = &c.HistoryAfter.Time, c.HistoryAfter.Hub, c.HistoryAfter.Epoch, c.HistoryAfter.EventID
	} else {
		var err error
		q.SnapshotCursor, c.AsOf, err = s.History.Snapshot(r.Context())
		if err != nil {
			internalError(w, "metered export snapshot failed", err)
			return
		}
	}
	rows, err := s.History.Page(r.Context(), q)
	if err != nil {
		internalError(w, "metered export history page failed", err)
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
		last := rows[len(rows)-1]
		c.SnapshotCursor, c.HistoryAfter = q.SnapshotCursor, &historyKey{Time: last.SortTime, Hub: last.HubID, Epoch: last.Epoch, EventID: last.EventID}
		next = s.encodeCursor(c)
	}
	write(w, c, c.AsOf, items, next, "ingest_snapshot")
}

func (s Service) summary(w http.ResponseWriter, r *http.Request, c cursor, limit int) {
	q := meteredsummary.Query{HomeID: c.Home, OwnerID: s.UserID(r), Granularity: c.Dataset, From: c.From, To: c.To, SensorID: c.Sensor, AttachmentID: c.Attachment, Limit: limit, After: c.SummaryAfter}
	if err := s.Summary.Scope(r.Context(), q); err != nil {
		scopeError(w, err)
		return
	}
	rows, err := s.Summary.Page(r.Context(), q)
	if err != nil {
		internalError(w, "metered export summary page failed", err)
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
		c.SummaryAfter = &rows[len(rows)-1].Key
		next = s.encodeCursor(c)
	}
	write(w, c, time.Now().UTC(), items, next, "live_rollup")
}

func scopeError(w http.ResponseWriter, err error) {
	if errors.Is(err, meteredhistory.ErrNotFound) || errors.Is(err, meteredsummary.ErrNotFound) {
		fail(w, 404, "not_found")
	} else {
		internalError(w, "metered export scope failed", err)
	}
}
func internalError(w http.ResponseWriter, message string, err error) {
	slog.Error(message, "error", err)
	fail(w, 500, "internal_error")
}
func write(w http.ResponseWriter, c cursor, asOf time.Time, records []json.RawMessage, next any, consistency string) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"dataset": c.Dataset, "from": c.From, "to": c.To, "retention": map[bool]string{true: "detail_30_days", false: "summary_24_months"}[c.Dataset == "history"], "continuation_consistency": consistency, "as_of": asOf, "records": records, "next_cursor": next})
}

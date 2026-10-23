package appliancefacts

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"energylens.local/cloud/internal/inventory"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

type Service struct {
	Composer
	Auth   func(http.Handler) http.Handler
	UserID func(*http.Request) string
}

func (s Service) Routes(r chi.Router) {
	r.With(s.Auth).Get("/homes/{home}/appliances/{appliance}/facts", s.get)
}

func (s Service) get(w http.ResponseWriter, r *http.Request) {
	home, id := chi.URLParam(r, "home"), chi.URLParam(r, "appliance")
	if _, err := uuid.Parse(home); err != nil {
		fail(w, http.StatusNotFound, "not_found", "home not found")
		return
	}
	if _, err := uuid.Parse(id); err != nil {
		fail(w, http.StatusNotFound, "not_found", "appliance not found")
		return
	}
	capability := r.URL.Query().Get("capability")
	if capability != "" && capability != "gas_dryer_recommendations_v1" && capability != "gas_appliance_recommendations_v1" {
		fail(w, http.StatusBadRequest, "invalid_request", "unknown capability")
		return
	}
	owner := s.UserID(r)
	a, err := s.owned(r.Context(), owner, home, id)
	if errors.Is(err, inventory.ErrNotFound) {
		fail(w, http.StatusNotFound, "not_found", "appliance or home not found")
		return
	}
	if err != nil {
		slog.Error("appliance facts owner check failed", "error", err)
		fail(w, http.StatusInternalServerError, "internal_error", "appliance facts request failed")
		return
	}
	now := time.Now().UTC()
	tag, err := s.etag(r.Context(), home, a, capability, now)
	if err != nil {
		slog.Error("appliance facts revision failed", "error", err)
		fail(w, http.StatusInternalServerError, "internal_error", "appliance facts request failed")
		return
	}
	w.Header().Set("ETag", tag)
	w.Header().Set("Cache-Control", "private, no-cache")
	if matches(r.Header.Get("If-None-Match"), tag) {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(s.compose(r.Context(), owner, home, a, capability, now))
}

// owned is the single owner check; sample appliances are served as inventory does.
func (c Composer) owned(ctx context.Context, owner, home, id string) (inventory.Appliance, error) {
	a, err := c.Appliances.Get(ctx, owner, home, id)
	if !errors.Is(err, inventory.ErrNotFound) {
		return a, err
	}
	items, _, err := c.Appliances.ListWithRevision(ctx, owner, home)
	if err != nil {
		return inventory.Appliance{}, err
	}
	for _, item := range items {
		if item.ID == id && item.DemoDeviceID != "" {
			return item, nil
		}
	}
	return inventory.Appliance{}, inventory.ErrNotFound
}

func (c Composer) etag(ctx context.Context, home string, a inventory.Appliance, capability string, now time.Time) (string, error) {
	fp, err := c.Revisions.Fingerprint(ctx, home, a.ID, now)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256([]byte(strings.Join([]string{a.ID, strconv.FormatInt(a.Version, 10), capability, fp}, "|")))
	return `"` + hex.EncodeToString(sum[:]) + `"`, nil
}

func matches(header, tag string) bool {
	for _, candidate := range strings.Split(header, ",") {
		candidate = strings.TrimPrefix(strings.TrimSpace(candidate), "W/")
		if candidate == "*" || candidate == tag {
			return true
		}
	}
	return false
}

func fail(w http.ResponseWriter, status int, code, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"code": code, "message": msg, "request_id": uuid.NewString(), "details": map[string]any{}})
}

// PGRevisions reads the stored revisions behind the estimate, catalog, programs,
// answers and tips sections in one statement.
type PGRevisions struct{ Pool *pgxpool.Pool }

func (p PGRevisions) Fingerprint(ctx context.Context, home, appliance string, now time.Time) (string, error) {
	var fp string
	err := p.Pool.QueryRow(ctx, `SELECT concat_ws('|',
 (SELECT COALESCE(max(revision),0)||'/'||count(*) FROM tariff_revisions WHERE home_id=$1),
 (SELECT COALESCE(encode(request_sha256,'hex')||resolved_at::text,'') FROM datawiz_energy_resolutions WHERE appliance_ref='cloud:'||$2::text),
 (SELECT count(*)||'/'||COALESCE(max(created_at)::text,'')||'/'||COALESCE(max(reviewed_at)::text,'') FROM public_program_revisions),
 (SELECT count(*) FROM reviewed_usage_facts),
 (SELECT count(*) FROM reviewed_usage_fact_revisions),
 (SELECT COALESCE(max(revision),0)||'/'||count(*) FROM question_answers WHERE home_id=$1 AND appliance_id=$2::uuid),
 (SELECT COALESCE(revision,0) FROM metered_summary_revision WHERE home_id=$1),
 (SELECT count(*)||'/'||COALESCE(max(created_at)::text,'') FROM recommendation_events WHERE home_id=$1 AND appliance_id=$2::uuid),
 $3::text)`, home, appliance, now.Format("2006-01-02")).Scan(&fp)
	return fp, err
}

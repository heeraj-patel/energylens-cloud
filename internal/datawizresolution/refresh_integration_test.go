package datawizresolution

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"energylens.local/cloud/internal/migrate"
	"energylens.local/cloud/migrations"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

func refreshPool(t *testing.T) (context.Context, *pgxpool.Pool) {
	t.Helper()
	dsn := os.Getenv("ENERGYLENS_CLOUD_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("ENERGYLENS_CLOUD_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	schema := "datawizrefresh_test_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	admin, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = admin.Exec(ctx, "CREATE SCHEMA "+schema); err != nil {
		t.Fatal(err)
	}
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	cfg.ConnConfig.RuntimeParams["search_path"] = schema
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	conn, err := pgx.ConnectConfig(ctx, cfg.ConnConfig)
	if err != nil {
		t.Fatal(err)
	}
	if err = migrate.Apply(ctx, conn, migrations.FS); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		pool.Close()
		conn.Close(ctx)
		admin.Exec(ctx, "DROP SCHEMA "+schema+" CASCADE")
		admin.Close()
	})
	return ctx, pool
}

type refreshResponse struct {
	Code     string `json:"code"`
	Reason   string `json:"reason"`
	QueuedAt string `json:"queued_at"`
	Details  struct {
		RetryAfter int `json:"retry_after_seconds"`
	} `json:"details"`
}

func TestRefreshEndpoint(t *testing.T) {
	ctx, pool := refreshPool(t)
	owner, other, home, otherHome := uuid.NewString(), uuid.NewString(), uuid.NewString(), uuid.NewString()
	appliance, second, archived, foreign := uuid.NewString(), uuid.NewString(), uuid.NewString(), uuid.NewString()
	for _, q := range []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO users(id) VALUES($1),($2)`, []any{owner, other}},
		{`INSERT INTO homes(id,owner_user_id,name,timezone) VALUES($1,$2,'Home','UTC'),($3,$4,'Other','UTC')`, []any{home, owner, otherHome, other}},
		{`INSERT INTO appliances(id,home_id,name,category,identity) VALUES($1,$2,'A','light','{}'),($3,$2,'B','light','{}'),($5,$2,'C','light','{}'),($4,$6,'D','light','{}')`, []any{appliance, home, second, foreign, archived, otherHome}},
		{`UPDATE appliances SET archived_at=now() WHERE id=$1`, []any{archived}},
	} {
		if _, err := pool.Exec(ctx, q.sql, q.args...); err != nil {
			t.Fatal(err)
		}
	}
	user := owner
	r := chi.NewRouter()
	Service{Store: PGStore{Pool: pool}, Auth: func(h http.Handler) http.Handler { return h }, UserID: func(*http.Request) string { return user }}.Routes(r)
	post := func(home, appliance, key string) (int, refreshResponse) {
		req := httptest.NewRequest(http.MethodPost, "/homes/"+home+"/appliances/"+appliance+"/energy-resolution/refresh", nil)
		if key != "" {
			req.Header.Set("Idempotency-Key", key)
		}
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)
		var body refreshResponse
		_ = json.Unmarshal(rec.Body.Bytes(), &body)
		return rec.Code, body
	}
	attempts := func() (n int) {
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM datawiz_resolve_attempts WHERE reason='owner_refresh'`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return
	}

	if code, _ := post(home, appliance, ""); code != 400 {
		t.Fatalf("missing key: %d", code)
	}
	for name, ids := range map[string][2]string{"archived": {home, archived}, "unknown": {home, uuid.NewString()}, "cross-home": {home, foreign}, "other owner's home": {otherHome, foreign}} {
		if code, _ := post(ids[0], ids[1], uuid.NewString()); code != 404 {
			t.Fatalf("%s: %d", name, code)
		}
	}
	key := uuid.NewString()
	code, first := post(home, appliance, key)
	if code != 202 || first.Reason != "owner_refresh" || first.QueuedAt == "" || attempts() != 1 {
		t.Fatalf("first refresh: %d %+v", code, first)
	}
	if code, replay := post(home, appliance, key); code != 202 || replay.QueuedAt != first.QueuedAt || attempts() != 1 {
		t.Fatalf("replay: %d %+v", code, replay)
	}
	code, dup := post(home, appliance, uuid.NewString())
	var storedAt time.Time
	if err := pool.QueryRow(ctx, `SELECT queued_at FROM datawiz_resolve_queue WHERE appliance_id=$1`, appliance).Scan(&storedAt); err != nil {
		t.Fatal(err)
	}
	parsed, _ := time.Parse(time.RFC3339Nano, first.QueuedAt)
	if code != 202 || dup.QueuedAt != first.QueuedAt || attempts() != 1 || !storedAt.Equal(parsed) {
		t.Fatalf("queued dedup: %d %+v stored %v", code, dup, storedAt)
	}

	if _, err := pool.Exec(ctx, `DELETE FROM datawiz_resolve_queue`); err != nil {
		t.Fatal(err)
	}
	code, limited := post(home, appliance, uuid.NewString())
	if code != 429 || limited.Code != "refresh_rate_limited" || limited.Details.RetryAfter < 590 || limited.Details.RetryAfter > 600 {
		t.Fatalf("appliance limit: %d %+v", code, limited)
	}
	if _, err := pool.Exec(ctx, `UPDATE datawiz_resolve_attempts SET queued_at=queued_at-interval '11 minutes'`); err != nil {
		t.Fatal(err)
	}
	if code, _ = post(home, appliance, uuid.NewString()); code != 202 {
		t.Fatalf("after the gap: %d", code)
	}

	if _, err := pool.Exec(ctx, `DELETE FROM datawiz_resolve_queue`); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `DELETE FROM datawiz_resolve_attempts`); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO datawiz_resolve_attempts(appliance_id,home_id,reason,queued_at) SELECT $1,$2,'owner_refresh',now()-interval '1 hour'-i*interval '1 minute' FROM generate_series(1,29) i`, uuid.NewString(), home); err != nil {
		t.Fatal(err)
	}
	if code, _ = post(home, second, uuid.NewString()); code != 202 {
		t.Fatalf("29 refreshes in the day: %d", code)
	}
	if _, err := pool.Exec(ctx, `DELETE FROM datawiz_resolve_queue`); err != nil {
		t.Fatal(err)
	}
	code, limited = post(home, second, uuid.NewString())
	if code != 429 || limited.Details.RetryAfter < 1 || limited.Details.RetryAfter > 86400 {
		t.Fatalf("home limit: %d %+v", code, limited)
	}
	if code, _ = post(otherHome, foreign, uuid.NewString()); code != 404 {
		t.Fatalf("other owner: %d", code)
	}
	user = other
	if code, _ = post(otherHome, foreign, uuid.NewString()); code != 202 {
		t.Fatalf("other home keeps its own budget: %d", code)
	}
}

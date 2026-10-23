package programs

import (
	"context"
	"errors"
	"github.com/go-chi/chi/v5"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"energylens.local/cloud/internal/migrate"
	"energylens.local/cloud/migrations"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestRefreshOwnerScopeCoalescingAndFailure(t *testing.T) {
	dsn := os.Getenv("ENERGYLENS_CLOUD_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("ENERGYLENS_CLOUD_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	schema := "program_refresh_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	admin, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	if _, err = admin.Exec(ctx, "CREATE SCHEMA "+schema); err != nil {
		t.Fatal(err)
	}
	defer admin.Exec(ctx, "DROP SCHEMA "+schema+" CASCADE")
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	cfg.ConnConfig.RuntimeParams["search_path"] = schema
	conn, err := pgx.ConnectConfig(ctx, cfg.ConnConfig)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	if err = migrate.Apply(ctx, conn, migrations.FS); err != nil {
		t.Fatal(err)
	}
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	owner := make([]string, 2)
	home := make([]string, 2)
	appliance := make([]string, 2)
	for i := range owner {
		if err = pool.QueryRow(ctx, `INSERT INTO users DEFAULT VALUES RETURNING id::text`).Scan(&owner[i]); err != nil {
			t.Fatal(err)
		}
		if err = pool.QueryRow(ctx, `INSERT INTO homes(owner_user_id,name,timezone) VALUES($1,'Home','America/Chicago') RETURNING id::text`, owner[i]).Scan(&home[i]); err != nil {
			t.Fatal(err)
		}
		if _, err = pool.Exec(ctx, `INSERT INTO home_location_revisions(home_id,revision,country_code,postal_code,precision,source) VALUES($1,1,'US','78732','postal_code','user_entered')`, home[i]); err != nil {
			t.Fatal(err)
		}
		if err = pool.QueryRow(ctx, `INSERT INTO appliances(home_id,name,category,identity) VALUES($1,'Washer','washer','{}') RETURNING id::text`, home[i]).Scan(&appliance[i]); err != nil {
			t.Fatal(err)
		}
	}
	store := RefreshStore{Pool: pool}
	router := chi.NewRouter()
	service := OwnerService{Store: PGOwnerStore{Pool: pool}, Auth: func(h http.Handler) http.Handler { return h }, UserID: func(*http.Request) string { return owner[0] }}
	service.Routes(router)
	apiKey := uuid.NewString()
	apiRequest := httptest.NewRequest(http.MethodPost, "/homes/"+home[0]+"/programs/discovery-refresh", strings.NewReader(`{"appliance_id":"`+appliance[0]+`"}`))
	apiRequest.Header.Set("Idempotency-Key", apiKey)
	apiResponse := httptest.NewRecorder()
	router.ServeHTTP(apiResponse, apiRequest)
	if apiResponse.Code != http.StatusAccepted {
		t.Fatalf("POST refresh: %d %s", apiResponse.Code, apiResponse.Body.String())
	}
	key := uuid.NewString()
	apiStatus, err := store.Enqueue(ctx, owner[0], home[0], appliance[0], apiKey)
	if err != nil || !strings.Contains(apiResponse.Body.String(), apiStatus.RequestID) {
		t.Fatalf("API status: %+v %v %s", apiStatus, err, apiResponse.Body.String())
	}
	getResponse := httptest.NewRecorder()
	router.ServeHTTP(getResponse, httptest.NewRequest(http.MethodGet, "/homes/"+home[0]+"/programs/discovery-refresh/"+apiStatus.RequestID, nil))
	if getResponse.Code != http.StatusOK || !strings.Contains(getResponse.Body.String(), `"state":"pending"`) {
		t.Fatalf("GET refresh: %d %s", getResponse.Code, getResponse.Body.String())
	}
	first, err := store.Enqueue(ctx, owner[0], home[0], appliance[0], key)
	if err != nil || first.State != "pending" || first.FinishedAt != nil || first.Coverage != nil {
		t.Fatalf("enqueue: %+v %v", first, err)
	}
	replay, err := store.Enqueue(ctx, owner[0], home[0], appliance[0], key)
	if err != nil || replay.RequestID != first.RequestID {
		t.Fatalf("replay: %+v %v", replay, err)
	}
	second, err := store.Enqueue(ctx, owner[1], home[1], appliance[1], uuid.NewString())
	if err != nil || second.RequestID == first.RequestID {
		t.Fatalf("second owner: %+v %v", second, err)
	}
	var jobs int
	if err = pool.QueryRow(ctx, `SELECT count(*) FROM program_discovery_refresh_jobs`).Scan(&jobs); err != nil || jobs != 1 {
		t.Fatalf("coalescing: %d %v", jobs, err)
	}
	if _, err = store.Status(ctx, owner[1], home[1], first.RequestID); !errors.Is(err, ErrOwnerHomeNotFound) {
		t.Fatalf("foreign status: %v", err)
	}
	if _, err = store.Enqueue(ctx, owner[0], home[0], appliance[1], key); !errors.Is(err, ErrOwnerHomeNotFound) {
		t.Fatalf("foreign appliance: %v", err)
	}
	if _, err = pool.Exec(ctx, `INSERT INTO home_location_revisions(home_id,revision,country_code,postal_code,precision,source) VALUES($1,2,'US','78733','postal_code','user_entered')`, home[0]); err != nil {
		t.Fatal(err)
	}
	if _, err = store.Status(ctx, owner[0], home[0], first.RequestID); !errors.Is(err, ErrOwnerHomeNotFound) {
		t.Fatalf("stale location status: %v", err)
	}
}

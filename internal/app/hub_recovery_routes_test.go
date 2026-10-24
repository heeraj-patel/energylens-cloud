package app

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"energylens.local/cloud/internal/hubtrust"
	"energylens.local/cloud/internal/migrate"
	"energylens.local/cloud/migrations"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// A certificate issued by unclaimed-hub recovery carries the unclaimed epoch
// and no home, so every home-scoped Hub route must deny it.
func TestRecoveryCertificateDeniedOnHomeRoutes(t *testing.T) {
	dsn := os.Getenv("ENERGYLENS_CLOUD_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("ENERGYLENS_CLOUD_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	schema := "recovery_routes_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	admin, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close(ctx)
	if _, err = admin.Exec(ctx, "CREATE SCHEMA "+schema); err != nil {
		t.Fatal(err)
	}
	defer admin.Exec(ctx, "DROP SCHEMA "+schema+" CASCADE")
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	cfg.ConnConfig.RuntimeParams["search_path"] = schema
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	conn, err := pgx.ConnectConfig(ctx, cfg.ConnConfig)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	if err = migrate.Apply(ctx, conn, migrations.FS); err != nil {
		t.Fatal(err)
	}
	var owner, home string
	if err = pool.QueryRow(ctx, `INSERT INTO users DEFAULT VALUES RETURNING id`).Scan(&owner); err != nil {
		t.Fatal(err)
	}
	if err = pool.QueryRow(ctx, `INSERT INTO homes(owner_user_id,name,timezone) VALUES($1,'H','UTC') RETURNING id`, owner).Scan(&home); err != nil {
		t.Fatal(err)
	}
	pub, _, _ := ed25519.GenerateKey(rand.Reader)
	store := hubtrust.Store{Pool: pool}
	hub, _, err := store.Provision(ctx, "elhub_recovery", pub, "test")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = pool.Exec(ctx, `UPDATE hubs SET home_id=NULL,ownership_epoch=3 WHERE id=$1`, hub); err != nil {
		t.Fatal(err)
	}
	if _, err = pool.Exec(ctx, `INSERT INTO hub_certificates(serial,hub_id,ownership_epoch,expires_at,certificate_pem) VALUES('beef',$1,3,now()+interval '1 hour','TEST')`, hub); err != nil {
		t.Fatal(err)
	}
	service := &hubtrust.Service{Store: store, Auth: func(next http.Handler) http.Handler { return next }, UserID: func(*http.Request) string { return owner }, SessionID: func(*http.Request) string { return uuid.NewString() }}
	router := cloudRouter(nil, func(context.Context) error { return nil }, service, pool)
	identity, _ := url.Parse("spiffe://energylens.local/hubs/" + hub + "/epochs/" + strconv.Itoa(3))
	peer := &x509.Certificate{SerialNumber: big.NewInt(0xbeef), URIs: []*url.URL{identity}}
	base := "/v1/hubs/" + hub
	for _, route := range []struct{ method, path, body string }{
		{"POST", "/sync/batches", `{}`},
		{"GET", "/sensors", ``},
		{"GET", "/attachments", ``},
		{"POST", "/catalog/model-search", `{}`},
		{"GET", "/catalog/products/" + uuid.NewString(), ``},
		{"GET", "/usage-facts", ``},
		{"PUT", "/connection", `{"https_origin":"https://192.168.1.2:1","certificate_sha256":"` + strings.Repeat("a", 64) + `"}`},
		{"POST", "/owner-authorizations", `{"home_id":"` + home + `"}`},
		{"POST", "/kasa-runtime/snapshots", `{"ownership_epoch":3,"snapshots":[{"snapshot_id":"` + uuid.NewString() + `","sensor_id":"` + uuid.NewString() + `","scope":"single_plug","window_label":"today","native_unit":"minutes","native_minutes":1,"duration_seconds":60,"device_clock_valid":false,"observed_at":"` + time.Now().UTC().Format(time.RFC3339) + `","method":"get_daystat","transport":"kasa_legacy_tcp","quality":"protocol_reported_clock_unverified"}]}`},
	} {
		req := httptest.NewRequest(route.method, base+route.path, strings.NewReader(route.body))
		req.Header.Set("Content-Type", "application/json")
		req.TLS = &tls.ConnectionState{PeerCertificates: []*x509.Certificate{peer}, VerifiedChains: [][]*x509.Certificate{{peer}}}
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		if rec.Code != 403 && rec.Code != 404 {
			t.Errorf("%s %s: %d %s", route.method, route.path, rec.Code, rec.Body.String())
		}
		t.Logf("%s %s -> %d", route.method, route.path, rec.Code)
	}
}

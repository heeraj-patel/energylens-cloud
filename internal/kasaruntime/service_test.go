package kasaruntime

import (
	"context"
	"errors"
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

func validSnapshot(at time.Time) Snapshot {
	return Snapshot{SnapshotID: uuid.NewString(), SensorID: uuid.NewString(), Scope: "single_plug", WindowLabel: "today", NativeUnit: "minutes", NativeMinutes: 0, DurationSeconds: 0, DeviceClockValid: false, ObservedAt: at, Method: "get_daystat", Transport: "kasa_legacy_tcp", Quality: "protocol_reported_clock_unverified"}
}
func TestReportedOnlyValidation(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	v := validSnapshot(now)
	if !v.valid(now) {
		t.Fatal("reported zero must be valid")
	}
	v.Quality = "unavailable"
	if v.valid(now) {
		t.Fatal("unavailable window must not upload")
	}
	v.Quality = "protocol_reported_clock_unverified"
	v.DurationSeconds = 60
	if v.valid(now) {
		t.Fatal("duration must equal native minutes")
	}
	v.DurationSeconds = 0
	v.AttachmentID = ptr(uuid.NewString())
	if v.valid(now) {
		t.Fatal("unknown window cannot attribute attachment")
	}
	start, end := now.Add(-time.Hour), now
	v.ReportedStart, v.ReportedEnd = &start, &end
	if v.valid(now) {
		t.Fatal("unverified clock cannot attribute attachment")
	}
}
func ptr(s string) *string { return &s }

func TestUploadRequiresVerifiedHubTLS(t *testing.T) {
	r := chi.NewRouter()
	(Service{}).HubRoutes(r)
	hub := uuid.NewString()
	req := httptest.NewRequest(http.MethodPost, "/hubs/"+hub+"/kasa-runtime/snapshots", strings.NewReader(`{"ownership_epoch":1,"snapshots":[]}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Amzn-Mtls-Clientcert", "forged")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusForbidden {
		t.Fatalf("unverified TLS returned %d", w.Code)
	}
}

func TestPersistentRuntimeScopeReplayAndTransfer(t *testing.T) {
	dsn := os.Getenv("ENERGYLENS_CLOUD_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("ENERGYLENS_CLOUD_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	schema := "runtime_test_" + strings.ReplaceAll(uuid.NewString(), "-", "")
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
	ids := make([]string, 7)
	for i := range ids {
		ids[i] = uuid.NewString()
	}
	owner, other, home, hub, sensor, appliance, attachment := ids[0], ids[1], ids[2], ids[3], ids[4], ids[5], ids[6]
	statements := []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO users(id) VALUES($1),($2)`, []any{owner, other}},
		{`INSERT INTO homes(id,owner_user_id,name,timezone) VALUES($1,$2,'House','UTC')`, []any{home, owner}},
		{`INSERT INTO hubs(id,local_hub_id,device_public_key,registered_by,home_id,ownership_epoch) VALUES($1,'elhub_runtime_test',$2,'test',$3,2)`, []any{hub, make([]byte, 32), home}},
		{`INSERT INTO hub_certificates(serial,hub_id,ownership_epoch,expires_at,certificate_pem) VALUES('abc',$1,2,now()+interval '1 hour','test')`, []any{hub}},
		{`INSERT INTO sensor_endpoints(id,home_id,hub_id,ownership_epoch,local_sensor_id,kind) VALUES($1,$2,$3,2,'plug','plug')`, []any{sensor, home, hub}},
		{`INSERT INTO appliances(id,home_id,name,category,identity) VALUES($1,$2,'Device','refrigerator','{}')`, []any{appliance, home}},
		{`INSERT INTO appliance_revisions(appliance_id,home_id,version,snapshot) VALUES($1,$2,1,'{}')`, []any{appliance, home}},
		{`INSERT INTO attachments(id,home_id,sensor_id,appliance_id,identity_revision,started_at) VALUES($1,$2,$3,$4,1,now()-interval '1 hour')`, []any{attachment, home, sensor, appliance}},
	}
	for _, s := range statements {
		if _, err = pool.Exec(ctx, s.sql, s.args...); err != nil {
			t.Fatal(err)
		}
	}
	store := Store{Pool: pool}
	now := time.Now().UTC().Truncate(time.Second)
	v := validSnapshot(now)
	v.SensorID = sensor
	upload := Upload{OwnershipEpoch: 2, Snapshots: []Snapshot{v}}
	if _, err = store.Accept(ctx, hub, "abc", 2, upload); err != nil {
		t.Fatal(err)
	}
	if _, err = store.Accept(ctx, hub, "abc", 2, upload); err != nil {
		t.Fatalf("replay: %v", err)
	}
	q := pageCursor{Home: home, From: now.Add(-time.Hour), To: now.Add(time.Minute)}
	rows, err := store.Page(ctx, home, owner, q, 2)
	if err != nil || len(rows) != 1 || rows[0].NativeMinutes != 0 || rows[0].AttachmentID != nil {
		t.Fatalf("owner read: %+v %v", rows, err)
	}
	if _, err = store.Page(ctx, home, other, q, 2); !errors.Is(err, ErrReference) {
		t.Fatalf("other owner: %v", err)
	}
	if _, err = pool.Exec(ctx, `UPDATE kasa_runtime_snapshots SET native_minutes=1 WHERE snapshot_id=$1`, v.SnapshotID); err == nil {
		t.Fatal("snapshot update must be rejected")
	}
	changed := v
	changed.NativeMinutes = 1
	changed.DurationSeconds = 60
	if _, err = store.Accept(ctx, hub, "abc", 2, Upload{OwnershipEpoch: 2, Snapshots: []Snapshot{changed}}); !errors.Is(err, ErrConflict) {
		t.Fatalf("conflicting replay: %v", err)
	}
	before := now.Add(-2 * time.Hour)
	end := now.Add(-time.Minute)
	attributed := v
	attributed.SnapshotID = uuid.NewString()
	attributed.AttachmentID = &attachment
	attributed.ReportedStart = &before
	attributed.ReportedEnd = &end
	attributed.DeviceClockValid = true
	attributed.Quality = "protocol_reported_clock_verified"
	if _, err = store.Accept(ctx, hub, "abc", 2, Upload{OwnershipEpoch: 2, Snapshots: []Snapshot{attributed}}); !errors.Is(err, ErrReference) {
		t.Fatalf("partial attachment: %v", err)
	}
	before = now.Add(-30 * time.Minute)
	attributed.ReportedStart = &before
	if _, err = store.Accept(ctx, hub, "abc", 2, Upload{OwnershipEpoch: 2, Snapshots: []Snapshot{attributed}}); err != nil {
		t.Fatalf("full attachment: %v", err)
	}
	good, bad := v, v
	good.SnapshotID, bad.SnapshotID = uuid.NewString(), uuid.NewString()
	bad.SensorID = uuid.NewString()
	if _, err = store.Accept(ctx, hub, "abc", 2, Upload{OwnershipEpoch: 2, Snapshots: []Snapshot{good, bad}}); !errors.Is(err, ErrReference) {
		t.Fatalf("mixed batch: %v", err)
	}
	var partialCount int
	if err = pool.QueryRow(ctx, `SELECT count(*) FROM kasa_runtime_snapshots WHERE snapshot_id=$1`, good.SnapshotID).Scan(&partialCount); err != nil || partialCount != 0 {
		t.Fatalf("partial batch persisted: %d %v", partialCount, err)
	}
	if _, err = pool.Exec(ctx, `UPDATE hub_certificates SET revoked_at=now() WHERE serial='abc'`); err != nil {
		t.Fatal(err)
	}
	if _, err = store.Accept(ctx, hub, "abc", 2, Upload{OwnershipEpoch: 2, Snapshots: []Snapshot{good}}); !errors.Is(err, ErrDenied) {
		t.Fatalf("revoked certificate: %v", err)
	}
	if _, err = pool.Exec(ctx, `UPDATE hub_certificates SET revoked_at=NULL WHERE serial='abc'`); err != nil {
		t.Fatal(err)
	}
	if _, err = pool.Exec(ctx, `UPDATE hubs SET ownership_epoch=3 WHERE id=$1`, hub); err != nil {
		t.Fatal(err)
	}
	newer := v
	newer.SnapshotID = uuid.NewString()
	if _, err = store.Accept(ctx, hub, "abc", 2, Upload{OwnershipEpoch: 2, Snapshots: []Snapshot{newer}}); !errors.Is(err, ErrDenied) {
		t.Fatalf("old epoch: %v", err)
	}
	if _, err = pool.Exec(ctx, `UPDATE homes SET owner_user_id=$1 WHERE id=$2`, other, home); err != nil {
		t.Fatal(err)
	}
	if _, err = store.Page(ctx, home, owner, q, 2); !errors.Is(err, ErrReference) {
		t.Fatalf("former owner read: %v", err)
	}
	if rows, err = store.Page(ctx, home, other, q, 2); err != nil || len(rows) != 2 {
		t.Fatalf("new owner read: %+v %v", rows, err)
	}
}

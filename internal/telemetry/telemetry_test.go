package telemetry

import (
	"context"
	"crypto/sha256"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"energylens.local/cloud/internal/account"
	"energylens.local/cloud/internal/migrate"
	"energylens.local/cloud/migrations"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestEventKindsStayDistinct(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	watts := 120.0
	point := Event{Kind: "power_point", EventID: uuid.NewString(), Revision: 1, SensorID: uuid.NewString(), Timestamp: &now, Watts: &watts}
	if !point.valid(now) {
		t.Fatal("point rejected")
	}
	point.EnergyWh = &watts
	if point.valid(now) {
		t.Fatal("point disguised as interval")
	}
	point.EnergyWh = nil
	gapReason := "age_limit"
	count := int64(2)
	gap := Event{Kind: "coverage_gap", GapID: uuid.NewString(), Revision: 1, SensorID: point.SensorID, FirstTimestamp: &now, LastTimestamp: &now, Reason: &gapReason, DroppedCount: &count}
	if !gap.valid(now) {
		t.Fatal("gap rejected")
	}
}

func TestDurableReplayAndEpochSafety(t *testing.T) {
	dsn := os.Getenv("ENERGYLENS_CLOUD_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("ENERGYLENS_CLOUD_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	schema := "telemetry_test_" + strings.ReplaceAll(uuid.NewString(), "-", "")
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
	accounts := account.PGStore{Pool: pool}
	owner, err := accounts.Resolve(ctx, account.Identity{Issuer: "https://id.test", Subject: "telemetry"})
	if err != nil {
		t.Fatal(err)
	}
	accepted := time.Now().UTC().Format(time.RFC3339)
	if _, err = (account.PGPreferenceStore{Pool: pool}).PatchAccountPreferences(ctx, owner.ID, account.AccountPreferencesPatch{HistoryDisclosureAcceptedAt: &accepted}, 1, uuid.NewString(), make([]byte, 32)); err != nil {
		t.Fatal(err)
	}
	home, err := accounts.Create(ctx, owner.ID, account.HomeCreate{Name: "House", Timezone: "America/Chicago", Location: account.Location{CountryCode: "US", PostalCode: "78732", Precision: "postal_code", Source: "user_entered"}}, uuid.NewString(), make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	var hub, sensor string
	err = pool.QueryRow(ctx, `INSERT INTO hubs(local_hub_id,device_public_key,registered_by,home_id,ownership_epoch) VALUES('elhub_telemetry_test',$1,'test',$2,2) RETURNING id`, make([]byte, 32), home.ID).Scan(&hub)
	if err != nil {
		t.Fatal(err)
	}
	_, err = pool.Exec(ctx, `INSERT INTO hub_certificates(serial,hub_id,ownership_epoch,expires_at,certificate_pem) VALUES('abc',$1,2,now()+interval '1 hour','test')`, hub)
	if err != nil {
		t.Fatal(err)
	}
	err = pool.QueryRow(ctx, `INSERT INTO sensor_endpoints(home_id,hub_id,ownership_epoch,local_sensor_id,kind,capabilities) VALUES($1,$2,2,'meter','plug',ARRAY['power','energy']) RETURNING id`, home.ID, hub).Scan(&sensor)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	watts := 120.0
	reason := "byte_limit"
	count := int64(3)
	p := Event{Kind: "power_point", EventID: uuid.NewString(), Revision: 1, SensorID: sensor, Timestamp: &now, Watts: &watts}
	g := Event{Kind: "coverage_gap", GapID: uuid.NewString(), Revision: 1, SensorID: sensor, FirstTimestamp: &now, LastTimestamp: &now, Reason: &reason, DroppedCount: &count}
	b := Batch{BatchID: uuid.NewString(), OwnershipEpoch: 2, SchemaVersion: 1, Events: []Event{p, g}}
	if !b.valid(now) {
		t.Fatal("batch invalid")
	}
	hash := sha256.Sum256([]byte("same bytes"))
	store := Store{Pool: pool}
	if err := store.VerifyCurrent(ctx, hub, "abc", 2); err != nil {
		t.Fatalf("current certificate: %v", err)
	}
	ack, err := store.Accept(ctx, hub, "abc", 2, b, hash[:])
	if err != nil || len(ack.AcceptedEventIDs) != 2 || ack.Cursor < 1 {
		t.Fatalf("accept: %+v %v", ack, err)
	}
	replay, err := store.Accept(ctx, hub, "abc", 2, b, hash[:])
	if err != nil || replay.Cursor != ack.Cursor || !replay.AcknowledgedAt.Equal(ack.AcknowledgedAt) {
		t.Fatalf("replay: %+v %v", replay, err)
	}
	otherHash := sha256.Sum256([]byte("changed"))
	if _, err = store.Accept(ctx, hub, "abc", 2, b, otherHash[:]); !errors.Is(err, ErrConflict) {
		t.Fatalf("batch conflict: %v", err)
	}
	var n int
	err = pool.QueryRow(ctx, `SELECT count(*) FROM ingest_events`).Scan(&n)
	if err != nil || n != 2 {
		t.Fatalf("event count %d %v", n, err)
	}
	err = pool.QueryRow(ctx, `SELECT count(*) FROM energy_observations WHERE kind='power_point' AND energy_wh IS NULL AND start_at IS NULL`).Scan(&n)
	if err != nil || n != 1 {
		t.Fatalf("point semantics %d %v", n, err)
	}
	err = pool.QueryRow(ctx, `SELECT count(*) FROM coverage_gaps WHERE dropped_count=3`).Scan(&n)
	if err != nil || n != 1 {
		t.Fatalf("gap semantics %d %v", n, err)
	}
	duplicate := b
	duplicate.BatchID = uuid.NewString()
	duplicate.Events = []Event{p}
	if _, err = store.Accept(ctx, hub, "abc", 2, duplicate, hash[:]); err != nil {
		t.Fatalf("event replay in new batch: %v", err)
	}
	changed := p
	differentWatts := 130.0
	changed.Watts = &differentWatts
	duplicate.BatchID = uuid.NewString()
	duplicate.Events = []Event{changed}
	if _, err = store.Accept(ctx, hub, "abc", 2, duplicate, hash[:]); !errors.Is(err, ErrConflict) {
		t.Fatalf("event conflict: %v", err)
	}
	corrected := changed
	corrected.Revision = 2
	duplicate.BatchID = uuid.NewString()
	duplicate.Events = []Event{corrected}
	if _, err = store.Accept(ctx, hub, "abc", 2, duplicate, hash[:]); err != nil {
		t.Fatalf("correction: %v", err)
	}
	bad := p
	bad.SensorID = uuid.NewString()
	duplicate.BatchID = uuid.NewString()
	duplicate.Events = []Event{bad}
	if _, err = store.Accept(ctx, hub, "abc", 2, duplicate, hash[:]); !errors.Is(err, ErrReference) {
		t.Fatalf("unmapped sensor: %v", err)
	}
	newPoint := p
	newPoint.EventID = uuid.NewString()
	mixed := Batch{BatchID: uuid.NewString(), OwnershipEpoch: 2, SchemaVersion: 1, Events: []Event{newPoint, bad}}
	if _, err = store.Accept(ctx, hub, "abc", 2, mixed, hash[:]); !errors.Is(err, ErrReference) {
		t.Fatalf("mixed batch: %v", err)
	}
	err = pool.QueryRow(ctx, `SELECT count(*) FROM ingest_events WHERE event_id=$1`, newPoint.EventID).Scan(&n)
	if err != nil || n != 0 {
		t.Fatalf("partial batch persisted %d %v", n, err)
	}
	start := now.Add(-time.Minute)
	energy := 2.3
	covered := int64(60)
	fuel, quality, source := "electricity", "metered", "verified_driver"
	missingAttachment := uuid.NewString()
	interval := Event{Kind: "energy_interval", EventID: uuid.NewString(), Revision: 1, SensorID: sensor, Start: &start, End: &now, EnergyWh: &energy, CoveredSeconds: &covered, AttachmentRevisionID: &missingAttachment, Fuel: &fuel, Quality: &quality, Source: &source}
	if !interval.valid(now) {
		t.Fatal("valid interval rejected")
	}
	intervalBatch := Batch{BatchID: uuid.NewString(), OwnershipEpoch: 2, SchemaVersion: 1, Events: []Event{interval}}
	if _, err = store.Accept(ctx, hub, "abc", 2, intervalBatch, hash[:]); !errors.Is(err, ErrReference) {
		t.Fatalf("missing attachment: %v", err)
	}
	var appliance, attachment string
	err = pool.QueryRow(ctx, `INSERT INTO appliances(home_id,name,category,identity) VALUES($1,'Metered appliance','refrigerator','{}') RETURNING id`, home.ID).Scan(&appliance)
	if err != nil {
		t.Fatal(err)
	}
	_, err = pool.Exec(ctx, `INSERT INTO appliance_revisions(appliance_id,home_id,version,snapshot) VALUES($1,$2,1,'{}')`, appliance, home.ID)
	if err != nil {
		t.Fatal(err)
	}
	err = pool.QueryRow(ctx, `INSERT INTO attachments(home_id,sensor_id,appliance_id,identity_revision,started_at) VALUES($1,$2,$3,1,$4) RETURNING id`, home.ID, sensor, appliance, start.Add(-time.Hour)).Scan(&attachment)
	if err != nil {
		t.Fatal(err)
	}
	interval.AttachmentRevisionID = &attachment
	intervalBatch.Events = []Event{interval}
	if _, err = store.Accept(ctx, hub, "abc", 2, intervalBatch, hash[:]); err != nil {
		t.Fatalf("interval upload: %v", err)
	}
	err = pool.QueryRow(ctx, `SELECT count(*) FROM energy_observations WHERE kind='energy_interval' AND energy_wh=2.3 AND covered_seconds=60 AND observed_at IS NULL`).Scan(&n)
	if err != nil || n != 1 {
		t.Fatalf("interval semantics %d %v", n, err)
	}
	_, err = pool.Exec(ctx, `UPDATE hubs SET home_id=NULL,ownership_epoch=3 WHERE id=$1`, hub)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.VerifyCurrent(ctx, hub, "abc", 2); !errors.Is(err, ErrDenied) {
		t.Fatalf("prior certificate after transfer: %v", err)
	}
	if _, err = store.Accept(ctx, hub, "abc", 2, b, hash[:]); !errors.Is(err, ErrDenied) {
		t.Fatalf("old epoch replay after transfer: %v", err)
	}
}

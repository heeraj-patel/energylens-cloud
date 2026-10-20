package attachments

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
	"energylens.local/cloud/internal/telemetry"
	"energylens.local/cloud/migrations"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestOwnerAttachmentMoveCloseAndHistory(t *testing.T) {
	dsn := os.Getenv("ENERGYLENS_CLOUD_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("ENERGYLENS_CLOUD_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	schema := "attachments_test_" + strings.ReplaceAll(uuid.NewString(), "-", "")
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
	owner, err := accounts.Resolve(ctx, account.Identity{Issuer: "https://id.test", Subject: "attachment-owner"})
	if err != nil {
		t.Fatal(err)
	}
	other, err := accounts.Resolve(ctx, account.Identity{Issuer: "https://id.test", Subject: "attachment-other"})
	if err != nil {
		t.Fatal(err)
	}
	home, err := accounts.Create(ctx, owner.ID, account.HomeCreate{Name: "House", Timezone: "America/Chicago", Location: account.Location{CountryCode: "US", PostalCode: "78732", Precision: "postal_code", Source: "user_entered"}}, uuid.NewString(), make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	var hub, sensor, appliance1, appliance2 string
	err = pool.QueryRow(ctx, `INSERT INTO hubs(local_hub_id,device_public_key,registered_by,home_id,ownership_epoch) VALUES('elhub_attachment_test',$1,'test',$2,2) RETURNING id`, make([]byte, 32), home.ID).Scan(&hub)
	if err != nil {
		t.Fatal(err)
	}
	_, err = pool.Exec(ctx, `INSERT INTO hub_certificates(serial,hub_id,ownership_epoch,expires_at,certificate_pem) VALUES('attachment-cert',$1,2,now()+interval '1 hour','test')`, hub)
	if err != nil {
		t.Fatal(err)
	}
	err = pool.QueryRow(ctx, `INSERT INTO sensor_endpoints(home_id,hub_id,ownership_epoch,local_sensor_id,kind,capabilities) VALUES($1,$2,2,'meter','plug',ARRAY['power']) RETURNING id`, home.ID, hub).Scan(&sensor)
	if err != nil {
		t.Fatal(err)
	}
	for _, target := range []*string{&appliance1, &appliance2} {
		err = pool.QueryRow(ctx, `INSERT INTO appliances(home_id,name,category,identity) VALUES($1,'Device','refrigerator','{}') RETURNING id`, home.ID).Scan(target)
		if err != nil {
			t.Fatal(err)
		}
		_, err = pool.Exec(ctx, `INSERT INTO appliance_revisions(appliance_id,home_id,version,snapshot) VALUES($1,$2,1,'{}')`, *target, home.ID)
		if err != nil {
			t.Fatal(err)
		}
	}
	store := Store{Pool: pool}
	start := time.Now().UTC().Add(time.Second).Truncate(time.Microsecond)
	hash := sha256.Sum256([]byte("create"))
	key := uuid.NewString()
	in := CreateInput{SensorID: sensor, ApplianceID: appliance1, IdentityRevision: 1, StartedAt: start}
	if _, err = store.Create(ctx, other.ID, home.ID, in, 1, key, hash[:]); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-owner: %v", err)
	}
	first, err := store.Create(ctx, owner.ID, home.ID, in, 1, key, hash[:])
	if err != nil || first.ID == "" || first.Version != 1 {
		t.Fatalf("attach: %+v %v", first, err)
	}
	replay, err := store.Create(ctx, owner.ID, home.ID, in, 1, key, hash[:])
	if err != nil || replay.ID != first.ID {
		t.Fatalf("replay: %+v %v", replay, err)
	}
	changedHash := sha256.Sum256([]byte("changed"))
	if _, err = store.Create(ctx, owner.ID, home.ID, in, 1, key, changedHash[:]); !errors.Is(err, ErrKeyReuse) {
		t.Fatalf("key reuse: %v", err)
	}
	if _, err = store.Create(ctx, owner.ID, home.ID, in, 1, uuid.NewString(), hash[:]); !errors.Is(err, ErrVersion) {
		t.Fatalf("stale sensor version: %v", err)
	}
	intervalStart := start.Add(30 * time.Second)
	intervalEnd := start.Add(90 * time.Second)
	energy := 2.0
	covered := int64(60)
	fuel, quality, source := "electricity", "metered", "verified_driver"
	interval := telemetry.Event{Kind: "energy_interval", EventID: uuid.NewString(), Revision: 1, SensorID: sensor, Start: &intervalStart, End: &intervalEnd, EnergyWh: &energy, CoveredSeconds: &covered, AttachmentRevisionID: &first.ID, Fuel: &fuel, Quality: &quality, Source: &source}
	batch := telemetry.Batch{BatchID: uuid.NewString(), OwnershipEpoch: 2, SchemaVersion: 1, Events: []telemetry.Event{interval}}
	if _, err = (telemetry.Store{Pool: pool}).Accept(ctx, hub, "attachment-cert", 2, batch, hash[:]); err != nil {
		t.Fatalf("accepted interval: %v", err)
	}
	backdated := CreateInput{SensorID: sensor, ApplianceID: appliance2, IdentityRevision: 1, StartedAt: start.Add(time.Minute)}
	if _, err = store.Create(ctx, owner.ID, home.ID, backdated, 2, uuid.NewString(), hash[:]); !errors.Is(err, ErrConflict) {
		t.Fatalf("backdated move over telemetry: %v", err)
	}
	moveAt := start.Add(2 * time.Minute)
	movedInput := CreateInput{SensorID: sensor, ApplianceID: appliance2, IdentityRevision: 1, StartedAt: moveAt}
	second, err := store.Create(ctx, owner.ID, home.ID, movedInput, 2, uuid.NewString(), hash[:])
	if err != nil || second.ID == first.ID {
		t.Fatalf("move: %+v %v", second, err)
	}
	listing, err := store.List(ctx, owner.ID, home.ID)
	if err != nil || len(listing) != 2 || listing[0].EndedAt == nil || !listing[0].EndedAt.Equal(moveAt) {
		t.Fatalf("history: %+v %v", listing, err)
	}
	if _, err = store.List(ctx, other.ID, home.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-owner list: %v", err)
	}
	end := moveAt.Add(time.Minute)
	closed, err := store.Close(ctx, owner.ID, home.ID, second.ID, end, 1, uuid.NewString(), hash[:])
	if err != nil || closed.Version != 2 || closed.EndedAt == nil {
		t.Fatalf("close: %+v %v", closed, err)
	}
	if _, err = store.Close(ctx, owner.ID, home.ID, second.ID, end, 1, uuid.NewString(), hash[:]); !errors.Is(err, ErrVersion) {
		t.Fatalf("stale close: %v", err)
	}
	var count int
	err = pool.QueryRow(ctx, `SELECT count(*) FROM attachment_revisions WHERE attachment_id=$1`, first.ID).Scan(&count)
	if err != nil || count != 2 {
		t.Fatalf("first revisions %d %v", count, err)
	}
	err = pool.QueryRow(ctx, `SELECT count(*) FROM attachment_revisions WHERE attachment_id=$1`, second.ID).Scan(&count)
	if err != nil || count != 2 {
		t.Fatalf("second revisions %d %v", count, err)
	}
	if _, err = pool.Exec(ctx, `UPDATE attachment_revisions SET snapshot='{}' WHERE attachment_id=$1`, first.ID); err == nil {
		t.Fatal("revision mutation accepted")
	}
	if _, err = pool.Exec(ctx, `UPDATE hubs SET home_id=NULL,ownership_epoch=3 WHERE id=$1`, hub); err != nil {
		t.Fatal(err)
	}
	if _, err = store.Create(ctx, owner.ID, home.ID, movedInput, 4, uuid.NewString(), hash[:]); !errors.Is(err, ErrNotFound) {
		t.Fatalf("prior epoch sensor: %v", err)
	}
}

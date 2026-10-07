package inventory

import (
	"context"
	"crypto/sha256"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"energylens.local/cloud/internal/account"
	"energylens.local/cloud/internal/attachments"
	"energylens.local/cloud/internal/migrate"
	"energylens.local/cloud/internal/telemetry"
	"energylens.local/cloud/migrations"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestInventoryOwnerVersionReplayAndCatalogLink(t *testing.T) {
	dsn := os.Getenv("ENERGYLENS_CLOUD_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("ENERGYLENS_CLOUD_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	schema := "inventory_test_" + strings.ReplaceAll(uuid.NewString(), "-", "")
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
	owner, err := accounts.Resolve(ctx, account.Identity{Issuer: "https://id.test", Subject: "owner"})
	if err != nil {
		t.Fatal(err)
	}
	other, err := accounts.Resolve(ctx, account.Identity{Issuer: "https://id.test", Subject: "other"})
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
	store := PGStore{Pool: pool}
	revision := func() string {
		t.Helper()
		_, token, err := store.ListWithRevision(ctx, owner.ID, home.ID)
		if err != nil || token == "" {
			t.Fatalf("inventory revision: %q %v", token, err)
		}
		return token
	}
	emptyRevision := revision()
	if _, token, err := store.ListWithRevision(ctx, other.ID, home.ID); !errors.Is(err, ErrNotFound) || token != "" {
		t.Fatalf("cross-owner revision = %q %v", token, err)
	}
	transientHome, err := accounts.Create(ctx, owner.ID, account.HomeCreate{Name: "Temporary", Timezone: "America/Chicago", Location: account.Location{CountryCode: "US", PostalCode: "78732", Precision: "postal_code", Source: "user_entered"}}, uuid.NewString(), make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	_, neverUsedRevision, err := store.ListWithRevision(ctx, owner.ID, transientHome.ID)
	if err != nil {
		t.Fatal(err)
	}
	room := "Kitchen"
	input := CreateInput{Name: "Fridge", Category: "refrigerator", Room: &room, Identity: Identity{Brand: "Acme", FullModel: "A123", Fuel: "electricity", MatchQuality: "unresolved"}}
	hash := sha256.Sum256([]byte("create"))
	transient, err := store.Create(ctx, owner.ID, transientHome.ID, input, uuid.NewString(), hash[:])
	if err != nil {
		t.Fatal(err)
	}
	if _, err = store.Archive(ctx, owner.ID, transientHome.ID, transient.ID, transient.Version, uuid.NewString(), hash[:]); err != nil {
		t.Fatal(err)
	}
	active, archivedOnlyRevision, err := store.ListWithRevision(ctx, owner.ID, transientHome.ID)
	if err != nil || len(active) != 0 || archivedOnlyRevision == neverUsedRevision {
		t.Fatalf("last-row archive revision: %d %q %q %v", len(active), archivedOnlyRevision, neverUsedRevision, err)
	}
	key := uuid.NewString()
	if _, err = store.Create(ctx, other.ID, home.ID, input, key, hash[:]); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-owner create = %v", err)
	}
	a, err := store.Create(ctx, owner.ID, home.ID, input, key, hash[:])
	if err != nil || a.Version != 1 {
		t.Fatalf("create %+v %v", a, err)
	}
	createdRevision := revision()
	if createdRevision == emptyRevision || revision() != createdRevision {
		t.Fatal("create did not change to a stable revision")
	}
	var scanCount int
	if err = pool.QueryRow(ctx, `SELECT count(*) FROM identity_scan_requests WHERE home_id=$1`, home.ID).Scan(&scanCount); err != nil || scanCount != 0 {
		t.Fatalf("unresolved identity enqueued: %d %v", scanCount, err)
	}
	confirmed := input
	confirmed.Identity.FullModel = "A124"
	confirmed.Identity.MatchQuality = "confirmed_exact"
	firstConfirmed, err := store.Create(ctx, owner.ID, home.ID, confirmed, uuid.NewString(), hash[:])
	if err != nil {
		t.Fatal(err)
	}
	if _, err = store.Create(ctx, owner.ID, home.ID, confirmed, uuid.NewString(), hash[:]); err != nil {
		t.Fatal(err)
	}
	if err = pool.QueryRow(ctx, `SELECT count(*) FROM identity_scan_requests WHERE home_id=$1`, home.ID).Scan(&scanCount); err != nil || scanCount != 1 {
		t.Fatalf("confirmed identity dedup: %d %v", scanCount, err)
	}
	newModel := confirmed.Identity
	newModel.FullModel = "A125"
	if _, err = store.Patch(ctx, owner.ID, home.ID, firstConfirmed.ID, PatchInput{Identity: &newModel}, 1, uuid.NewString(), hash[:]); err != nil {
		t.Fatal(err)
	}
	if err = pool.QueryRow(ctx, `SELECT count(*) FROM identity_scan_requests WHERE home_id=$1`, home.ID).Scan(&scanCount); err != nil || scanCount != 2 {
		t.Fatalf("changed identity trigger: %d %v", scanCount, err)
	}
	again, err := store.Create(ctx, owner.ID, home.ID, input, key, hash[:])
	if err != nil || again.ID != a.ID {
		t.Fatalf("create replay %+v %v", again, err)
	}
	beforePatchRevision := revision()
	otherHash := sha256.Sum256([]byte("different"))
	if _, err = store.Create(ctx, owner.ID, home.ID, input, key, otherHash[:]); !errors.Is(err, ErrKeyReuse) {
		t.Fatalf("key reuse = %v", err)
	}
	if _, err = store.Get(ctx, other.ID, home.ID, a.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-owner get = %v", err)
	}
	if _, err = store.List(ctx, other.ID, home.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-owner list = %v", err)
	}
	if _, err = store.Patch(ctx, other.ID, home.ID, a.ID, PatchInput{Name: &input.Name}, 1, uuid.NewString(), hash[:]); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-owner patch = %v", err)
	}
	name := "Kitchen fridge"
	patchKey := uuid.NewString()
	patchHash := sha256.Sum256([]byte("patch"))
	updated, err := store.Patch(ctx, owner.ID, home.ID, a.ID, PatchInput{Name: &name}, 1, patchKey, patchHash[:])
	if err != nil || updated.Version != 2 || updated.Name != name {
		t.Fatalf("patch %+v %v", updated, err)
	}
	patchedRevision := revision()
	if patchedRevision == beforePatchRevision || revision() != patchedRevision {
		t.Fatal("patch did not change to a stable revision")
	}
	replay, err := store.Patch(ctx, owner.ID, home.ID, a.ID, PatchInput{Name: &name}, 1, patchKey, patchHash[:])
	if err != nil || replay.Version != 2 {
		t.Fatalf("patch replay %+v %v", replay, err)
	}
	if _, err = pool.Exec(ctx, `UPDATE account_preferences SET history_disclosure_accepted_at=NULL WHERE user_id=$1`, owner.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = store.Create(ctx, owner.ID, home.ID, input, key, hash[:]); !errors.Is(err, ErrDisclosureRequired) {
		t.Fatalf("create replay without disclosure = %v", err)
	}
	if _, err = store.Patch(ctx, other.ID, home.ID, a.ID, PatchInput{Name: &name}, 1, patchKey, patchHash[:]); !errors.Is(err, ErrNotFound) {
		t.Fatalf("other owner before disclosure = %v", err)
	}
	if _, err = store.Patch(ctx, owner.ID, home.ID, a.ID, PatchInput{Name: &name}, 1, patchKey, patchHash[:]); !errors.Is(err, ErrDisclosureRequired) {
		t.Fatalf("patch replay without disclosure = %v", err)
	}
	if _, err = pool.Exec(ctx, `UPDATE account_preferences SET history_disclosure_accepted_at=now() WHERE user_id=$1`, owner.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = store.Patch(ctx, owner.ID, home.ID, a.ID, PatchInput{Name: &name}, 1, uuid.NewString(), patchHash[:]); !errors.Is(err, ErrVersion) {
		t.Fatalf("stale patch = %v", err)
	}
	var versions int
	if err = pool.QueryRow(ctx, `SELECT count(*) FROM appliance_revisions WHERE appliance_id=$1`, a.ID).Scan(&versions); err != nil || versions != 2 {
		t.Fatalf("revisions = %d %v", versions, err)
	}
	if _, err = pool.Exec(ctx, `UPDATE appliance_revisions SET snapshot='{}' WHERE appliance_id=$1`, a.ID); err == nil {
		t.Fatal("revision mutation accepted")
	}
	missing := uuid.NewString()
	exact := Identity{Brand: "Acme", FullModel: "A123", Fuel: "electricity", MatchQuality: "confirmed_exact", CatalogProductID: &missing}
	if _, err = store.Patch(ctx, owner.ID, home.ID, a.ID, PatchInput{Identity: &exact}, 2, uuid.NewString(), patchHash[:]); !errors.Is(err, ErrCatalogMismatch) {
		t.Fatalf("missing catalog link = %v", err)
	}
	_, err = pool.Exec(ctx, `INSERT INTO catalog_products(id,category,brand,full_model,fuel,match_quality,product_json) VALUES($1,'refrigerator','Acme','A123','electricity','confirmed_exact','{}')`, missing)
	if err != nil {
		t.Fatal(err)
	}
	linked, err := store.Patch(ctx, owner.ID, home.ID, a.ID, PatchInput{Identity: &exact}, 2, uuid.NewString(), patchHash[:])
	if err != nil || linked.Identity.CatalogProductID == nil || *linked.Identity.CatalogProductID != missing {
		t.Fatalf("exact catalog link %+v %v", linked, err)
	}
	cleared, err := store.Patch(ctx, owner.ID, home.ID, a.ID, PatchInput{Room: []byte("null")}, 3, uuid.NewString(), patchHash[:])
	if err != nil || cleared.Room != nil || cleared.Version != 4 {
		t.Fatalf("clear optional room %+v %v", cleared, err)
	}
	family := exact
	family.MatchQuality = "documented_variant_family"
	if ValidIdentity(family) {
		t.Fatal("family identity accepted exact catalog link")
	}
	var hub, sensor string
	if err = pool.QueryRow(ctx, `INSERT INTO hubs(local_hub_id,device_public_key,registered_by,home_id,ownership_epoch) VALUES('elhub_inventory_archive_test',$1,'test',$2,1) RETURNING id`, make([]byte, 32), home.ID).Scan(&hub); err != nil {
		t.Fatal(err)
	}
	if _, err = pool.Exec(ctx, `INSERT INTO hub_certificates(serial,hub_id,ownership_epoch,expires_at,certificate_pem) VALUES('inventory-archive-cert',$1,1,now()+interval '1 hour','test')`, hub); err != nil {
		t.Fatal(err)
	}
	if err = pool.QueryRow(ctx, `INSERT INTO sensor_endpoints(home_id,hub_id,ownership_epoch,local_sensor_id,kind,capabilities) VALUES($1,$2,1,'archive-meter','plug',ARRAY['power']) RETURNING id`, home.ID, hub).Scan(&sensor); err != nil {
		t.Fatal(err)
	}
	if _, err = pool.Exec(ctx, `UPDATE sensor_endpoints SET created_at=now()-interval '2 hours' WHERE id=$1`, sensor); err != nil {
		t.Fatal(err)
	}
	start := time.Now().UTC().Add(-time.Hour)
	attached, err := (attachments.Store{Pool: pool}).Create(ctx, owner.ID, home.ID, attachments.CreateInput{SensorID: sensor, ApplianceID: a.ID, IdentityRevision: cleared.Version, StartedAt: start}, 1, uuid.NewString(), hash[:])
	if err != nil {
		t.Fatalf("attach before archive: %v", err)
	}
	intervalStart, intervalEnd := start.Add(20*time.Minute), start.Add(21*time.Minute)
	energy := 2.0
	covered := int64(60)
	fuel, quality, source := "electricity", "metered", "verified_driver"
	interval := telemetry.Event{Kind: "energy_interval", EventID: uuid.NewString(), Revision: 1, SensorID: sensor, Start: &intervalStart, End: &intervalEnd, EnergyWh: &energy, CoveredSeconds: &covered, AttachmentRevisionID: &attached.ID, Fuel: &fuel, Quality: &quality, Source: &source}
	batch := telemetry.Batch{BatchID: uuid.NewString(), OwnershipEpoch: 1, SchemaVersion: 1, Events: []telemetry.Event{interval}}
	if _, err = (telemetry.Store{Pool: pool}).Accept(ctx, hub, "inventory-archive-cert", 1, batch, hash[:]); err != nil {
		t.Fatalf("historical observation: %v", err)
	}
	archiveHash := sha256.Sum256([]byte("archive version 4"))
	archiveKey := uuid.NewString()
	if _, err = store.Archive(ctx, other.ID, home.ID, a.ID, cleared.Version, archiveKey, archiveHash[:]); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-owner archive: %v", err)
	}
	if _, err = store.Archive(ctx, owner.ID, home.ID, a.ID, 1, uuid.NewString(), hash[:]); !errors.Is(err, ErrVersion) {
		t.Fatalf("stale archive: %v", err)
	}
	beforeArchiveRevision := revision()
	archived, err := store.Archive(ctx, owner.ID, home.ID, a.ID, cleared.Version, archiveKey, archiveHash[:])
	if err != nil || archived.Version != cleared.Version+1 || archived.ArchivedAt == nil {
		t.Fatalf("archive: %+v %v", archived, err)
	}
	archivedRevision := revision()
	if archivedRevision == beforeArchiveRevision || revision() != archivedRevision {
		t.Fatal("archive did not change to a stable revision")
	}
	if again, err := store.Archive(ctx, owner.ID, home.ID, a.ID, cleared.Version, archiveKey, archiveHash[:]); err != nil || again.Version != archived.Version {
		t.Fatalf("archive retry: %+v %v", again, err)
	}
	if _, err = store.Archive(ctx, owner.ID, home.ID, a.ID, cleared.Version, archiveKey, hash[:]); !errors.Is(err, ErrKeyReuse) {
		t.Fatalf("archive key reuse: %v", err)
	}
	if _, err = store.Get(ctx, owner.ID, home.ID, a.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("archived get: %v", err)
	}
	var end time.Time
	var attachmentVersion, sensorVersion, attachmentRevisions, applianceRevisions int64
	if err = pool.QueryRow(ctx, `SELECT ended_at,version FROM attachments WHERE id=$1`, attached.ID).Scan(&end, &attachmentVersion); err != nil {
		t.Fatal(err)
	}
	if !end.After(start) || attachmentVersion != 2 {
		t.Fatalf("attachment not closed: %v %d", end, attachmentVersion)
	}
	if err = pool.QueryRow(ctx, `SELECT version FROM sensor_endpoints WHERE id=$1`, sensor).Scan(&sensorVersion); err != nil || sensorVersion != 3 {
		t.Fatalf("sensor revision: %d %v", sensorVersion, err)
	}
	if err = pool.QueryRow(ctx, `SELECT count(*) FROM attachment_revisions WHERE attachment_id=$1`, attached.ID).Scan(&attachmentRevisions); err != nil || attachmentRevisions != 2 {
		t.Fatalf("attachment lineage: %d %v", attachmentRevisions, err)
	}
	if err = pool.QueryRow(ctx, `SELECT count(*) FROM appliance_revisions WHERE appliance_id=$1`, a.ID).Scan(&applianceRevisions); err != nil || applianceRevisions != 5 {
		t.Fatalf("appliance lineage: %d %v", applianceRevisions, err)
	}
	var observedCount int
	if err = pool.QueryRow(ctx, `SELECT count(*) FROM energy_observations WHERE attachment_revision_id=$1`, attached.ID).Scan(&observedCount); err != nil || observedCount != 1 {
		t.Fatalf("historical observation lost: %d %v", observedCount, err)
	}
	var originalEnd *time.Time
	if err = pool.QueryRow(ctx, `SELECT snapshot->>'ended_at' FROM attachment_revisions WHERE attachment_id=$1 AND version=1`, attached.ID).Scan(&originalEnd); err != nil || originalEnd != nil {
		t.Fatalf("original attachment snapshot changed: %v %v", originalEnd, err)
	}
	var futureSensor string
	if err = pool.QueryRow(ctx, `INSERT INTO sensor_endpoints(home_id,hub_id,ownership_epoch,local_sensor_id,kind,capabilities) VALUES($1,$2,1,'future-meter','plug',ARRAY['power']) RETURNING id`, home.ID, hub).Scan(&futureSensor); err != nil {
		t.Fatal(err)
	}
	future, err := (attachments.Store{Pool: pool}).Create(ctx, owner.ID, home.ID, attachments.CreateInput{SensorID: futureSensor, ApplianceID: firstConfirmed.ID, IdentityRevision: 2, StartedAt: time.Now().UTC().Add(time.Hour)}, 1, uuid.NewString(), hash[:])
	if err != nil {
		t.Fatalf("future attachment: %v", err)
	}
	if _, err = store.Archive(ctx, owner.ID, home.ID, firstConfirmed.ID, 2, uuid.NewString(), hash[:]); !errors.Is(err, ErrAttachmentConflict) {
		t.Fatalf("unsafe future closure: %v", err)
	}
	if current, err := store.Get(ctx, owner.ID, home.ID, firstConfirmed.ID); err != nil || current.ArchivedAt != nil {
		t.Fatalf("conflicted archive changed appliance: %+v %v", current, err)
	}
	var futureEnd *time.Time
	if err = pool.QueryRow(ctx, `SELECT ended_at FROM attachments WHERE id=$1`, future.ID).Scan(&futureEnd); err != nil || futureEnd != nil {
		t.Fatalf("conflicted archive closed attachment: %v %v", futureEnd, err)
	}
}

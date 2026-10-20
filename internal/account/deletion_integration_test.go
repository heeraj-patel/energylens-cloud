package account

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"energylens.local/cloud/internal/migrate"
	"energylens.local/cloud/internal/telemetry"
	"energylens.local/cloud/migrations"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestDeletionRevokesHubAndReplaysAfterRestore(t *testing.T) {
	dsn := os.Getenv("ENERGYLENS_CLOUD_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("ENERGYLENS_CLOUD_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	schema := "deletion_test_" + strings.ReplaceAll(uuid.NewString(), "-", "")
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

	store := PGStore{Pool: pool}
	emptyArchive := testDirectoryJournal(t)
	ownerIdentity := Identity{Issuer: "https://identity.example", Subject: "owner", Email: "owner@example.test", EmailVerified: true}
	owner, err := store.Resolve(ctx, ownerIdentity)
	if err != nil {
		t.Fatal(err)
	}
	other, err := store.Resolve(ctx, Identity{Issuer: "https://identity.example", Subject: "other"})
	if err != nil {
		t.Fatal(err)
	}
	home, secondHome, hub, cert := uuid.NewString(), uuid.NewString(), uuid.NewString(), "deletion-cert"
	previewID, applianceID := uuid.NewString(), uuid.NewString()
	sensorID, attachmentID, interventionID := uuid.NewString(), uuid.NewString(), uuid.NewString()
	for _, row := range []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO homes(id,owner_user_id,name,timezone) VALUES($1,$2,'House','UTC'),($3,$2,'Second','UTC')`, []any{home, owner.ID, secondHome}},
		{`INSERT INTO hubs(id,local_hub_id,device_public_key,registered_by,home_id,ownership_epoch) VALUES($1,'elhub_deletion_test',$2,'test',$3,2)`, []any{hub, make([]byte, 32), home}},
		{`INSERT INTO hub_certificates(serial,hub_id,ownership_epoch,expires_at,certificate_pem) VALUES($1,$2,2,now()+interval '1 hour','test')`, []any{cert, hub}},
		{`INSERT INTO hub_ownership_history(hub_id,ownership_epoch,home_id,owner_user_id) VALUES($1,2,$2,$3)`, []any{hub, home, owner.ID}},
		{`INSERT INTO energy_rating_catalog(brand,model,category,source_kind,annual_kwh,source_ref,contract_version) VALUES('Public','Model','refrigerator','epa_exact',100,'source','v1')`, nil},
		{`INSERT INTO home_location_revisions(home_id,revision,country_code,postal_code,precision,source) VALUES($1,1,'US','60601','postal_code','user_entered')`, []any{home}},
		{`INSERT INTO appliances(id,home_id,name,category,identity) VALUES($1,$2,'Private dryer','dryer','{"brand":"Private","full_model":"Sensitive123","fuel":"natural_gas","match_quality":"confirmed_exact"}')`, []any{applianceID, home}},
		{`INSERT INTO appliance_revisions(appliance_id,home_id,version,snapshot) VALUES($1,$2,1,'{}')`, []any{applianceID, home}},
		{`INSERT INTO sensor_endpoints(id,home_id,hub_id,ownership_epoch,local_sensor_id,kind) VALUES($1,$2,$3,2,'private-plug','plug')`, []any{sensorID, home, hub}},
		{`INSERT INTO attachments(id,home_id,sensor_id,appliance_id,identity_revision,started_at) VALUES($1,$2,$3,$4,1,now()-interval '20 days')`, []any{attachmentID, home, sensorID, applianceID}},
		{`INSERT INTO interventions(id,home_id,appliance_id,attachment_id,rule_id,occurred_at,comparable_use,comparable_weather) VALUES($1,$2,$3,$4,'private-action',date_trunc('hour',now()-interval '8 days'),false,false)`, []any{interventionID, home, applianceID, attachmentID}},
		{`INSERT INTO intervention_idempotency(owner_user_id,home_id,key,request_hash,intervention_id) VALUES($1,$2,$3,$4,$5)`, []any{owner.ID, home, uuid.NewString(), make([]byte, 32), interventionID}},
		{`INSERT INTO report_previews(id,owner_user_id,home_id,home_version,idempotency_key,request_hash,schema_version,payload,expires_at) VALUES($1,$2,$3,1,$4,$5,'manual-preview/v1','{"private":"report"}',now()+interval '1 hour')`, []any{previewID, owner.ID, home, uuid.NewString(), make([]byte, 32)}},
		{`INSERT INTO report_delivery_requests(owner_user_id,home_id,preview_id,idempotency_key,request_hash,recipient_hash,recipients,approved_snapshot_sha256) VALUES($1,$2,$3,$4,$5,$5,ARRAY['private@example.test'],repeat('a',64))`, []any{owner.ID, home, previewID, uuid.NewString(), make([]byte, 32)}},
		{`INSERT INTO account_home_idempotency(owner_user_id,operation,key,request_hash,response_json,response_status) VALUES($1,'create_home',$2,$3,$4,201)`, []any{owner.ID, uuid.NewString(), make([]byte, 32), `{"id":"` + home + `","postal_code":"60601"}`}},
		{`INSERT INTO tariff_revisions(home_id,revision,fuel,rate,rate_unit,currency,rate_kind,source_ref,source_as_of,is_fallback,effective_from) VALUES($1,1,'electricity',0.1,'kWh','USD','user_tariff','private tariff',now(),false,now())`, []any{home}},
	} {
		if _, err = pool.Exec(ctx, row.sql, row.args...); err != nil {
			t.Fatal(err)
		}
	}
	if err = store.PurgeHome(ctx, home, emptyArchive); !errors.Is(err, ErrPurgeNotReady) {
		t.Fatalf("untombstoned purge: %v", err)
	}
	if _, err = pool.Exec(ctx, `UPDATE homes SET deleted_at=now() WHERE id=$1`, secondHome); err != nil {
		t.Fatal(err)
	}
	if err = store.PurgeHome(ctx, secondHome, emptyArchive); !errors.Is(err, ErrPurgeNotReady) {
		t.Fatalf("tombstone without journal purge: %v", err)
	}
	if _, err = pool.Exec(ctx, `UPDATE homes SET deleted_at=NULL WHERE id=$1`, secondHome); err != nil {
		t.Fatal(err)
	}
	uploader := telemetry.Store{Pool: pool}
	if err = uploader.VerifyCurrent(ctx, hub, cert, 2); err != nil {
		t.Fatalf("before deletion: %v", err)
	}
	if err = store.DeleteHome(ctx, other.ID, home); !errors.Is(err, ErrNotFound) {
		t.Fatalf("foreign deletion: %v", err)
	}
	if err = uploader.VerifyCurrent(ctx, hub, cert, 2); err != nil {
		t.Fatalf("foreign deletion changed hub: %v", err)
	}
	if err = store.DeleteHome(ctx, owner.ID, home); err != nil {
		t.Fatal(err)
	}
	if err = uploader.VerifyCurrent(ctx, hub, cert, 2); !errors.Is(err, telemetry.ErrDenied) {
		t.Fatalf("stale upload: %v", err)
	}
	if _, err = store.Get(ctx, owner.ID, home); !errors.Is(err, ErrNotFound) {
		t.Fatalf("deleted home read: %v", err)
	}
	var epoch int64
	var currentHome *string
	var revoked, ended, deleted bool
	if err = pool.QueryRow(ctx, `SELECT ownership_epoch,home_id FROM hubs WHERE id=$1`, hub).Scan(&epoch, &currentHome); err != nil {
		t.Fatal(err)
	}
	if epoch != 3 || currentHome != nil {
		t.Fatalf("hub still associated: epoch=%d home=%v", epoch, currentHome)
	}
	if err = pool.QueryRow(ctx, `SELECT revoked_at IS NOT NULL FROM hub_certificates WHERE serial=$1`, cert).Scan(&revoked); err != nil || !revoked {
		t.Fatalf("certificate: %v %v", revoked, err)
	}
	if err = pool.QueryRow(ctx, `SELECT ended_at IS NOT NULL FROM hub_ownership_history WHERE hub_id=$1 AND ownership_epoch=2`, hub).Scan(&ended); err != nil || !ended {
		t.Fatalf("ownership history: %v %v", ended, err)
	}
	if err = pool.QueryRow(ctx, `SELECT deleted_at IS NOT NULL FROM homes WHERE id=$1`, home).Scan(&deleted); err != nil || !deleted {
		t.Fatalf("home tombstone: %v %v", deleted, err)
	}
	if err = store.DeleteHome(ctx, owner.ID, home); !errors.Is(err, ErrNotFound) {
		t.Fatalf("duplicate deletion: %v", err)
	}
	if err = store.DeleteAccount(ctx, owner.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = store.Resolve(ctx, ownerIdentity); !errors.Is(err, ErrDeleted) {
		t.Fatalf("deleted identity resolved: %v", err)
	}
	if err = store.DeleteAccount(ctx, owner.ID); !errors.Is(err, ErrDeleted) {
		t.Fatalf("duplicate account deletion: %v", err)
	}
	if _, err = store.Get(ctx, owner.ID, secondHome); !errors.Is(err, ErrNotFound) {
		t.Fatalf("second home still visible: %v", err)
	}
	var publicCount int
	if err = pool.QueryRow(ctx, `SELECT count(*) FROM energy_rating_catalog WHERE brand='Public'`).Scan(&publicCount); err != nil || publicCount != 1 {
		t.Fatalf("public catalog changed: %d %v", publicCount, err)
	}
	entries, err := store.JournalSince(ctx, 0, 10)
	if err != nil || len(entries) != 3 {
		t.Fatalf("journal %v %v", entries, err)
	}
	var accountEntry DeletionEntry
	for _, e := range entries {
		if e.Kind == "account" {
			accountEntry = e
		}
	}
	if accountEntry.ObjectID != owner.ID {
		t.Fatalf("missing account entry: %v", entries)
	}
	for _, test := range []struct {
		name    string
		archive JournalArchive
	}{
		{"nil archive", nil},
		{"unexported archive", emptyArchive},
	} {
		if err = store.PurgeHome(ctx, home, test.archive); !errors.Is(err, ErrPurgeArchiveNotReady) {
			t.Fatalf("%s home purge: %v", test.name, err)
		}
		if err = store.PurgeAccount(ctx, owner.ID, test.archive); !errors.Is(err, ErrPurgeArchiveNotReady) {
			t.Fatalf("%s account purge: %v", test.name, err)
		}
	}
	var privateCount int
	if err = pool.QueryRow(ctx, `SELECT count(*) FROM appliances WHERE home_id=$1`, home).Scan(&privateCount); err != nil || privateCount != 1 {
		t.Fatalf("private rows changed before export: %d %v", privateCount, err)
	}
	staleArchive := testDirectoryJournal(t)
	for _, entry := range entries[:len(entries)-1] {
		if err = staleArchive.Append(entry); err != nil {
			t.Fatal(err)
		}
	}
	if err = store.PurgeHome(ctx, home, staleArchive); !errors.Is(err, ErrPurgeArchiveNotReady) {
		t.Fatalf("stale archive home purge: %v", err)
	}
	if err = store.PurgeAccount(ctx, owner.ID, staleArchive); !errors.Is(err, ErrPurgeArchiveNotReady) {
		t.Fatalf("stale archive account purge: %v", err)
	}
	missingArchive := testDirectoryJournal(t)
	missingArchive.Dir = filepath.Join(missingArchive.Dir, "missing")
	if err = store.PurgeHome(ctx, home, missingArchive); err == nil {
		t.Fatal("missing archive allowed home purge")
	}
	if err = store.PurgeAccount(ctx, owner.ID, missingArchive); err == nil {
		t.Fatal("missing archive allowed account purge")
	}
	corruptArchive := testDirectoryJournal(t)
	for _, entry := range entries {
		if err = corruptArchive.Append(entry); err != nil {
			t.Fatal(err)
		}
	}
	files, err := os.ReadDir(corruptArchive.Dir)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(corruptArchive.Dir, files[0].Name()), []byte("{}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err = store.PurgeHome(ctx, home, corruptArchive); err == nil {
		t.Fatal("corrupt archive allowed home purge")
	}
	if err = store.PurgeAccount(ctx, owner.ID, corruptArchive); err == nil {
		t.Fatal("corrupt archive allowed account purge")
	}
	if err = pool.QueryRow(ctx, `SELECT count(*) FROM appliances WHERE home_id=$1`, home).Scan(&privateCount); err != nil || privateCount != 1 {
		t.Fatalf("private rows changed after rejected purges: %d %v", privateCount, err)
	}
	archive := testDirectoryJournal(t)
	if count, err := store.ExportDeletionJournal(ctx, archive); err != nil || count != 3 {
		t.Fatalf("external export: %d %v", count, err)
	}
	// Simulate an older backup restored without the disable/revocation changes,
	// then replay an independently retained journal entry before serving traffic.
	if _, err = pool.Exec(ctx, `UPDATE users SET deleted_at=NULL,email='restored@example.test',email_verified=true WHERE id=$1`, owner.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = pool.Exec(ctx, `UPDATE homes SET deleted_at=NULL WHERE owner_user_id=$1`, owner.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = pool.Exec(ctx, `UPDATE hubs SET home_id=$2,ownership_epoch=2 WHERE id=$1`, hub, home); err != nil {
		t.Fatal(err)
	}
	if _, err = pool.Exec(ctx, `UPDATE hub_certificates SET revoked_at=NULL WHERE serial=$1`, cert); err != nil {
		t.Fatal(err)
	}
	if count, replayErr := store.ReplayDeletionJournal(ctx, archive); replayErr != nil || count != 3 {
		t.Fatalf("external replay: %d %v", count, replayErr)
	}
	if err = store.ReapplyDeletion(ctx, accountEntry); err != nil {
		t.Fatalf("replay idempotency: %v", err)
	}
	if err = uploader.VerifyCurrent(ctx, hub, cert, 2); !errors.Is(err, telemetry.ErrDenied) {
		t.Fatalf("restored upload: %v", err)
	}
	if _, err = store.Resolve(ctx, ownerIdentity); !errors.Is(err, ErrDeleted) {
		t.Fatalf("restored identity resolved: %v", err)
	}
	if err = pool.QueryRow(ctx, `SELECT deleted_at IS NOT NULL FROM users WHERE id=$1`, owner.ID).Scan(&deleted); err != nil || !deleted {
		t.Fatalf("restored account tombstone: %v %v", deleted, err)
	}
	if err = pool.QueryRow(ctx, `SELECT count(*) FROM energy_rating_catalog WHERE brand='Public'`).Scan(&publicCount); err != nil || publicCount != 1 {
		t.Fatalf("public catalog after replay: %d %v", publicCount, err)
	}
	if accountEntry.DeletedAt.After(time.Now()) {
		t.Fatal("future deletion time")
	}
	if err = store.PurgeHome(ctx, home, archive); err != nil {
		t.Fatalf("purge home: %v", err)
	}
	if err = store.PurgeHome(ctx, home, archive); err != nil {
		t.Fatalf("repeat home purge: %v", err)
	}
	for _, table := range []string{"home_location_revisions", "report_previews", "report_delivery_requests", "account_home_idempotency", "tariff_revisions", "interventions", "intervention_idempotency"} {
		var count int
		if err = pool.QueryRow(ctx, `SELECT count(*) FROM `+table).Scan(&count); err != nil || count != 0 {
			t.Fatalf("private table %s: count=%d err=%v", table, count, err)
		}
	}
	if err = store.PurgeAccount(ctx, owner.ID, archive); err != nil {
		t.Fatalf("purge account: %v", err)
	}
	if err = store.PurgeAccount(ctx, owner.ID, archive); err != nil {
		t.Fatalf("repeat account purge: %v", err)
	}
	if _, err = store.Resolve(ctx, ownerIdentity); !errors.Is(err, ErrDeleted) {
		t.Fatalf("purged account resolved: %v", err)
	}
	if err = pool.QueryRow(ctx, `SELECT count(*) FROM account_preferences WHERE user_id=$1`, owner.ID).Scan(&privateCount); err != nil || privateCount != 0 {
		t.Fatalf("account preferences after purge: %d %v", privateCount, err)
	}
	if err = pool.QueryRow(ctx, `SELECT count(*) FROM energy_rating_catalog WHERE brand='Public'`).Scan(&publicCount); err != nil || publicCount != 1 {
		t.Fatalf("public catalog after purge: %d %v", publicCount, err)
	}
}

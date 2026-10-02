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
	"energylens.local/cloud/internal/migrate"
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
	room := "Kitchen"
	input := CreateInput{Name: "Fridge", Category: "refrigerator", Room: &room, Identity: Identity{Brand: "Acme", FullModel: "A123", Fuel: "electricity", MatchQuality: "unresolved"}}
	hash := sha256.Sum256([]byte("create"))
	key := uuid.NewString()
	if _, err = store.Create(ctx, other.ID, home.ID, input, key, hash[:]); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-owner create = %v", err)
	}
	a, err := store.Create(ctx, owner.ID, home.ID, input, key, hash[:])
	if err != nil || a.Version != 1 {
		t.Fatalf("create %+v %v", a, err)
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
}

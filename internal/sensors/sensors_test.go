package sensors

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

func TestCreateInputValidation(t *testing.T) {
	v := CreateInput{HubID: uuid.NewString(), OwnershipEpoch: 1, LocalSensorID: "plug-local", Kind: "plug", Capabilities: []string{"power", "energy"}}
	if !validInput(v) {
		t.Fatal("valid sensor rejected")
	}
	v.Capabilities = []string{"power", "power"}
	if validInput(v) {
		t.Fatal("duplicate capability accepted")
	}
	v.Capabilities = []string{"power"}
	v.LocalSensorID = " plug-local"
	if validInput(v) {
		t.Fatal("noncanonical local ID accepted")
	}
}

func TestRegistrationOwnerEpochAndHubMapping(t *testing.T) {
	dsn := os.Getenv("ENERGYLENS_CLOUD_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("ENERGYLENS_CLOUD_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	schema := "sensors_test_" + strings.ReplaceAll(uuid.NewString(), "-", "")
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
	var hub string
	err = pool.QueryRow(ctx, `INSERT INTO hubs(local_hub_id,device_public_key,registered_by,home_id,ownership_epoch) VALUES('elhub_sensor_test',$1,'test',$2,2) RETURNING id`, make([]byte, 32), home.ID).Scan(&hub)
	if err != nil {
		t.Fatal(err)
	}
	_, err = pool.Exec(ctx, `INSERT INTO hub_certificates(serial,hub_id,ownership_epoch,expires_at,certificate_pem) VALUES('abc',$1,2,$2,'test')`, hub, time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	store := Store{Pool: pool}
	input := CreateInput{HubID: hub, OwnershipEpoch: 2, LocalSensorID: "local-meter", Kind: "plug", Capabilities: []string{"power", "energy"}}
	hash := sha256.Sum256([]byte("create"))
	key := uuid.NewString()
	if _, err = store.Create(ctx, other.ID, home.ID, input, key, hash[:]); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-owner create: %v", err)
	}
	sensor, err := store.Create(ctx, owner.ID, home.ID, input, key, hash[:])
	if err != nil || sensor.ID == "" || sensor.ID == input.LocalSensorID {
		t.Fatalf("create: %+v %v", sensor, err)
	}
	replay, err := store.Create(ctx, owner.ID, home.ID, input, key, hash[:])
	if err != nil || replay.ID != sensor.ID {
		t.Fatalf("replay: %+v %v", replay, err)
	}
	different := sha256.Sum256([]byte("different"))
	if _, err = store.Create(ctx, owner.ID, home.ID, input, key, different[:]); !errors.Is(err, ErrKeyReuse) {
		t.Fatalf("key reuse: %v", err)
	}
	if _, err = store.Create(ctx, owner.ID, home.ID, input, uuid.NewString(), hash[:]); !errors.Is(err, ErrConflict) {
		t.Fatalf("duplicate local ID: %v", err)
	}
	if _, err = store.ListOwner(ctx, other.ID, home.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-owner list: %v", err)
	}
	listing, err := store.ListOwner(ctx, owner.ID, home.ID)
	if err != nil || len(listing) != 1 || listing[0].ID != sensor.ID {
		t.Fatalf("owner listing: %+v %v", listing, err)
	}
	mapping, err := store.ListHub(ctx, hub, "abc", 2)
	if err != nil || len(mapping.Sensors) != 1 || mapping.Sensors[0].ID != sensor.ID {
		t.Fatalf("hub listing: %+v %v", mapping, err)
	}
	if _, err = store.ListHub(ctx, hub, "abc", 1); !errors.Is(err, ErrDenied) {
		t.Fatalf("stale cert epoch: %v", err)
	}
	if _, err = pool.Exec(ctx, `UPDATE hubs SET home_id=NULL,ownership_epoch=3 WHERE id=$1`, hub); err != nil {
		t.Fatal(err)
	}
	if _, err = store.ListHub(ctx, hub, "abc", 2); !errors.Is(err, ErrDenied) {
		t.Fatalf("transfer mapping: %v", err)
	}
	listing, err = store.ListOwner(ctx, owner.ID, home.ID)
	if err != nil || len(listing) != 0 {
		t.Fatalf("old mapping visible: %+v %v", listing, err)
	}
	if _, err = store.Create(ctx, owner.ID, home.ID, input, uuid.NewString(), hash[:]); !errors.Is(err, ErrNotFound) {
		t.Fatalf("old owner create: %v", err)
	}
}

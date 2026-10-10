package account

import (
	"context"
	"crypto/sha256"
	"errors"
	"os"
	"strings"
	"testing"

	"energylens.local/cloud/internal/migrate"
	"energylens.local/cloud/migrations"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestProfileVersionReplayAndIdentity(t *testing.T) {
	dsn := os.Getenv("ENERGYLENS_CLOUD_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("ENERGYLENS_CLOUD_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	schema := "profile_test_" + strings.ReplaceAll(uuid.NewString(), "-", "")
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
	accounts := PGStore{Pool: pool}
	identity := Identity{Issuer: "https://issuer.example", Subject: "profile-owner", Email: "first@example.test", EmailVerified: true}
	initial, err := accounts.Resolve(ctx, identity)
	if err != nil || initial.ProfileVersion != 1 || initial.DisplayName != nil {
		t.Fatalf("initial profile = %+v %v", initial, err)
	}
	other, err := accounts.Resolve(ctx, Identity{Issuer: "https://issuer.example", Subject: "other"})
	if err != nil {
		t.Fatal(err)
	}
	profile := PGProfileStore{Pool: pool}
	name := "Ada Lovelace"
	key := uuid.NewString()
	hash := sha256.Sum256([]byte("1\n" + name))
	updated, err := profile.PatchProfile(ctx, initial.ID, &name, 1, key, hash[:])
	if err != nil || updated.ProfileVersion != 2 || updated.DisplayName == nil || *updated.DisplayName != name {
		t.Fatalf("updated profile = %+v %v", updated, err)
	}
	replay, err := profile.PatchProfile(ctx, initial.ID, &name, 1, key, hash[:])
	if err != nil || replay.ProfileVersion != 2 {
		t.Fatalf("replay = %+v %v", replay, err)
	}
	if _, err = profile.PatchProfile(ctx, initial.ID, nil, 1, key, sha256.New().Sum(nil)); !errors.Is(err, ErrKeyReuse) {
		t.Fatalf("key reuse = %v", err)
	}
	if _, err = profile.PatchProfile(ctx, initial.ID, nil, 1, uuid.NewString(), hash[:]); !errors.Is(err, ErrVersion) {
		t.Fatalf("stale version = %v", err)
	}
	identity.Email = "second@example.test"
	resolved, err := accounts.Resolve(ctx, identity)
	if err != nil || resolved.ID != initial.ID || resolved.ProfileVersion != 2 || resolved.DisplayName == nil || *resolved.DisplayName != name {
		t.Fatalf("stable identity = %+v %v", resolved, err)
	}
	unmodified, err := accounts.Resolve(ctx, Identity{Issuer: "https://issuer.example", Subject: "other"})
	if err != nil || unmodified.ID != other.ID || unmodified.DisplayName != nil || unmodified.ProfileVersion != 1 {
		t.Fatalf("other profile = %+v %v", unmodified, err)
	}
}

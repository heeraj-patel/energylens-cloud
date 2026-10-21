package questions

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"energylens.local/cloud/internal/account"
	"energylens.local/cloud/internal/inventory"
	"energylens.local/cloud/internal/migrate"
	"energylens.local/cloud/migrations"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestPatchCarriesOnlyApplicableAnswers(t *testing.T) {
	dsn := os.Getenv("ENERGYLENS_CLOUD_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("ENERGYLENS_CLOUD_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	schema := "questions_carry_" + strings.ReplaceAll(uuid.NewString(), "-", "")
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
	owner, err := accounts.Resolve(ctx, account.Identity{Issuer: "https://id.test", Subject: "carry-owner"})
	if err != nil {
		t.Fatal(err)
	}
	home, err := accounts.Create(ctx, owner.ID, account.HomeCreate{Name: "House", Timezone: "America/Chicago", Location: account.Location{CountryCode: "US", PostalCode: "78732", Precision: "postal_code", Source: "user_entered"}}, uuid.NewString(), make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	inv := inventory.PGStore{Pool: pool, AnswerApplies: Applies}
	store := PGStore{Pool: pool}
	a := addAppliance(t, ctx, inv, owner.ID, home.ID, "washer")
	now := time.Now().UTC().Format(time.RFC3339)
	in := AnswerCreate{QuestionID: "washer.cycles_per_week", QuestionVersion: onboardingVersion, ApplianceID: a.ID, IdentityRevision: a.Version, Status: "confirmed", Value: json.RawMessage("4"), ConfirmedAt: &now}
	h := sha256.Sum256([]byte("carry"))
	if _, err = store.Submit(ctx, owner.ID, home.ID, []AnswerCreate{in}, uuid.NewString(), h[:]); err != nil {
		t.Fatal(err)
	}
	current := func(id string) map[string]string {
		t.Helper()
		answers, err := store.CurrentAnswers(ctx, owner.ID, home.ID, id)
		if err != nil {
			t.Fatal(err)
		}
		out := map[string]string{}
		for _, answer := range answers {
			out[answer.QuestionID] = string(answer.Value)
		}
		return out
	}
	patch := func(a inventory.Appliance, in inventory.PatchInput) inventory.Appliance {
		t.Helper()
		sum := sha256.Sum256([]byte(a.ID + string(rune(a.Version))))
		next, err := inv.Patch(ctx, owner.ID, home.ID, a.ID, in, a.Version, uuid.NewString(), sum[:])
		if err != nil {
			t.Fatal(err)
		}
		return next
	}
	name := "Basement washer"
	renamed := patch(a, inventory.PatchInput{Name: &name})
	if renamed.Version != 2 || current(a.ID)["washer.cycles_per_week"] != "4" {
		t.Fatalf("answer lost across a rename: version %d %v", renamed.Version, current(a.ID))
	}
	category := "dishwasher"
	changed := patch(renamed, inventory.PatchInput{Category: &category})
	if changed.Version != 3 || len(current(a.ID)) != 0 {
		t.Fatalf("answer to a question that no longer applies was carried: version %d %v", changed.Version, current(a.ID))
	}
}

package usagefactimport

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"energylens.local/cloud/internal/migrate"
	"energylens.local/cloud/internal/recommendation"
	"energylens.local/cloud/migrations"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestPostgresUsageImportReplayConflictAndImmutability(t *testing.T) {
	dsn := os.Getenv("ENERGYLENS_CLOUD_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("ENERGYLENS_CLOUD_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	schema := "usage_test_" + strings.ReplaceAll(uuid.NewString(), "-", "")
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
	p, der, priv, key := fixture(t)
	verified, err := ParseAndVerify(signFixture(t, p, der, priv), key)
	if err != nil {
		t.Fatal(err)
	}
	result, err := Import(ctx, pool, verified, "operator")
	if err != nil || result.Replayed || result.Facts != 1 {
		t.Fatalf("first import: %+v %v", result, err)
	}
	replay, err := Import(ctx, pool, verified, "operator")
	if err != nil || !replay.Replayed {
		t.Fatalf("replay: %+v %v", replay, err)
	}
	var annual, usage int
	if err = pool.QueryRow(ctx, `SELECT count(*) FROM catalog_fact_revisions`).Scan(&annual); err != nil || annual != 0 {
		t.Fatalf("annual facts changed: %d %v", annual, err)
	}
	if err = pool.QueryRow(ctx, `SELECT count(*) FROM reviewed_usage_facts`).Scan(&usage); err != nil || usage != 1 {
		t.Fatalf("usage rows: %d %v", usage, err)
	}
	if _, err = pool.Exec(ctx, `UPDATE reviewed_usage_facts SET value=9`); err == nil {
		t.Fatal("immutable fact updated")
	}
	if _, err = pool.Exec(ctx, `DELETE FROM reviewed_usage_publications`); err == nil {
		t.Fatal("immutable publication deleted")
	}
	conflict := p
	conflict.Facts = append([]Fact(nil), p.Facts...)
	conflict.Facts[0].Value = "0.43"
	changed, err := ParseAndVerify(signFixture(t, conflict, der, priv), key)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = Import(ctx, pool, changed, "operator"); err == nil {
		t.Fatal("conflicting immutable ID accepted")
	}
	if err = pool.QueryRow(ctx, `SELECT count(*) FROM reviewed_usage_publications`).Scan(&usage); err != nil || usage != 1 {
		t.Fatalf("conflict did not roll back publication: %d %v", usage, err)
	}
	alias := p
	alias.Facts = append([]Fact(nil), p.Facts...)
	alias.Facts[0].ID = "22222222-2222-5222-8222-222222222222"
	alias.Facts[0].Model = "WM-3070-HWA"
	aliased, err := ParseAndVerify(signFixture(t, alias, der, priv), key)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = Import(ctx, pool, aliased, "operator"); err == nil {
		t.Fatal("ambiguous normalized model identity accepted")
	}
	if err = pool.QueryRow(ctx, `SELECT count(*) FROM reviewed_usage_publications`).Scan(&usage); err != nil || usage != 1 {
		t.Fatalf("ambiguous identity did not roll back publication: %d %v", usage, err)
	}
	extended := p
	extended.Facts = append([]Fact(nil), p.Facts...)
	second := p.Facts[0]
	second.ID = "33333333-3333-5333-8333-333333333333"
	second.Model = "WM3070HXA"
	extended.Facts = append(extended.Facts, second)
	extendedVerified, err := ParseAndVerify(signFixture(t, extended, der, priv), key)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = Import(ctx, pool, extendedVerified, "operator"); err == nil {
		t.Fatal("same immutable fact joined a second publication")
	}
	if err = pool.QueryRow(ctx, `SELECT count(*) FROM reviewed_usage_publications`).Scan(&usage); err != nil || usage != 1 {
		t.Fatalf("repeated fact did not roll back publication: %d %v", usage, err)
	}
	altered := verified
	altered.EnvelopeHash = strings.Repeat("c", 64)
	if _, err = Import(ctx, pool, altered, "operator"); err == nil {
		t.Fatal("mutated verified envelope accepted")
	}
}

func TestSignedElectricCycleFactsReachRecommendationsOnlyForExactCompatibleEvidence(t *testing.T) {
	dsn := os.Getenv("ENERGYLENS_CLOUD_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("ENERGYLENS_CLOUD_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	schema := "usage_recommendation_" + strings.ReplaceAll(uuid.NewString(), "-", "")
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

	p, der, priv, key := fixture(t)
	type caseRow struct {
		name, category, applianceCategory, factModel, applianceModel, factVariant, applianceVariant, fuel string
		family, mixed, active                                                                             bool
	}
	cases := []caseRow{
		{name: "washer exact", category: "washer", factModel: "W100", applianceModel: "W100", factVariant: "US-120V", applianceVariant: "US-120V", fuel: "electricity", active: true},
		{name: "dryer exact", category: "dryer", factModel: "D100", applianceModel: "D100", factVariant: "US-120V", applianceVariant: "US-120V", fuel: "electricity", active: true},
		{name: "dishwasher exact", category: "dishwasher", factModel: "DW100", applianceModel: "DW100", factVariant: "US-120V", applianceVariant: "US-120V", fuel: "electricity", active: true},
		{name: "family scope", category: "washer", factModel: "WM3070HWA", applianceModel: "WM3070HWA", factVariant: "US-120V", applianceVariant: "US-120V", fuel: "electricity", family: true},
		{name: "mixed energy", category: "washer", factModel: "W400", applianceModel: "W400", factVariant: "US-120V", applianceVariant: "US-120V", fuel: "electricity", mixed: true},
		{name: "model mismatch", category: "washer", factModel: "W500", applianceModel: "W501", factVariant: "US-120V", applianceVariant: "US-120V", fuel: "electricity"},
		{name: "variant mismatch", category: "washer", factModel: "W600", applianceModel: "W600", factVariant: "US-120V", applianceVariant: "US-240V", fuel: "electricity"},
		{name: "category mismatch", category: "washer", applianceCategory: "dishwasher", factModel: "W700", applianceModel: "W700", factVariant: "US-120V", applianceVariant: "US-120V", fuel: "electricity"},
		{name: "fuel mismatch", category: "dryer", factModel: "D800", applianceModel: "D800", factVariant: "US-120V", applianceVariant: "US-120V", fuel: "natural_gas"},
	}
	base := p.Facts[0]
	p.Facts = nil
	factIDs := make([]string, len(cases))
	for i, tc := range cases {
		f := fixtureFactForRecommendation(base, tc.category, tc.factModel, tc.factVariant)
		f.ID = uuid.NewString()
		if tc.family {
			f.MatchQuality = "documented_variant_family"
			f.DocumentedScope, f.MatchedModel = "WM3070H*A", "WM3070H*A"
		}
		if tc.mixed {
			f.IncludedEnergy.Components = []string{"appliance_electricity", "water_heating_electricity"}
		}
		factIDs[i] = f.ID
		p.Facts = append(p.Facts, f)
	}
	verified, err := ParseAndVerify(signFixture(t, p, der, priv), key)
	if err != nil {
		t.Fatal(err)
	}
	if result, err := Import(ctx, pool, verified, "operator"); err != nil || result.Facts != len(cases) {
		t.Fatalf("signed import: %+v %v", result, err)
	}
	owner, home, tariffID := uuid.NewString(), uuid.NewString(), uuid.NewString()
	now := time.Now().UTC()
	for _, stmt := range []struct {
		q    string
		args []any
	}{
		{`INSERT INTO users(id) VALUES($1)`, []any{owner}},
		{`INSERT INTO homes(id,owner_user_id,name,timezone) VALUES($1,$2,'House','UTC')`, []any{home, owner}},
		{`INSERT INTO tariff_revisions(id,home_id,revision,fuel,rate,rate_unit,currency,rate_kind,source_ref,source_as_of,is_fallback,effective_from) VALUES($1,$2,1,'electricity',0.15,'kWh','USD','user_tariff','customer',$3,false,$4)`, []any{tariffID, home, now, now.Add(-time.Hour)}},
	} {
		if _, err := pool.Exec(ctx, stmt.q, stmt.args...); err != nil {
			t.Fatal(err)
		}
	}
	devices := make([]string, len(cases))
	answers := make([]string, len(cases))
	for i, tc := range cases {
		devices[i], answers[i] = uuid.NewString(), uuid.NewString()
		applianceCategory := tc.category
		if tc.applianceCategory != "" {
			applianceCategory = tc.applianceCategory
		}
		identity := `{"brand":"LG","full_model":"` + tc.applianceModel + `","variant":"` + tc.applianceVariant + `","fuel":"` + tc.fuel + `","match_quality":"confirmed_exact"}`
		if _, err := pool.Exec(ctx, `INSERT INTO appliances(id,home_id,name,category,identity) VALUES($1,$2,$3,$4,$5)`, devices[i], home, tc.name, applianceCategory, identity); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(ctx, `INSERT INTO question_answers(id,home_id,appliance_id,appliance_version,question_id,question_version,revision,status,answer_value,confirmed_at) VALUES($1,$2,$3,1,$4,1,1,'confirmed','3',$5)`, answers[i], home, devices[i], applianceCategory+".cycles_per_week", now); err != nil {
			t.Fatal(err)
		}
	}
	items, err := (recommendation.PGStore{Pool: pool}).Current(ctx, owner, home, now)
	if err != nil || len(items) != len(cases) {
		t.Fatalf("recommendation read: %d appliances, %v", len(items), err)
	}
	byID := make(map[string]recommendation.Appliance, len(items))
	for _, item := range items {
		byID[item.ID] = item
	}
	for i, tc := range cases {
		item := byID[devices[i]]
		if tc.fuel == "natural_gas" && (len(item.Facts) != 0 || item.GasUsage != nil) {
			t.Fatalf("%s: electric fact attached to gas dryer: %+v", tc.name, item)
		}
		actions := recommendation.Evaluate(item)
		if tc.fuel == "natural_gas" {
			actions = recommendation.EvaluateGasEnabled(item)
		}
		if len(actions) != 1 {
			t.Fatalf("%s: actions %+v", tc.name, actions)
		}
		action := actions[0]
		if action.SavingsAmount != nil {
			t.Fatalf("%s: savings asserted: %+v", tc.name, action)
		}
		if tc.active {
			if action.State != "active" || action.NextQuestion != "" || len(action.EvidenceRefs) != 3 || action.EvidenceRefs[0].ID != factIDs[i] || action.EvidenceRefs[0].Kind != "cycle_energy" || action.EvidenceRefs[1].ID != answers[i] || action.EvidenceRefs[2].ID != tariffID {
				t.Fatalf("%s: compatible fact did not activate advice: %+v", tc.name, action)
			}
		} else if action.State != "candidate" || action.NextQuestion != "find_reviewed_energy_source" || len(action.EvidenceRefs) != 0 {
			t.Fatalf("%s: incompatible fact activated advice: %+v", tc.name, action)
		}
	}
}

func fixtureFactForRecommendation(base Fact, category, model, variant string) Fact {
	f := base
	f.Category, f.Model, f.Variant = category, model, variant
	f.MatchQuality = "confirmed_exact"
	f.DocumentedScope, f.MatchedModel = model, model
	return f
}

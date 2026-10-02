package estimation

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"energylens.local/cloud/internal/account"
	"energylens.local/cloud/internal/inventory"
	"energylens.local/cloud/internal/migrate"
	"energylens.local/cloud/internal/tariff"
	"energylens.local/cloud/migrations"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type estimateBody struct {
	ID                string `json:"id"`
	Kind              string `json:"kind"`
	UnavailableReason string `json:"unavailable_reason"`
	Cost              *struct {
		Value string `json:"value"`
	} `json:"cost"`
	EvidenceRefs []struct {
		Kind     string `json:"kind"`
		Revision int64  `json:"revision"`
	} `json:"evidence_refs"`
}

func decodeEstimate(t *testing.T, raw json.RawMessage) estimateBody {
	t.Helper()
	var out estimateBody
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestPersistedEstimateDependenciesAndOwnerIsolation(t *testing.T) {
	dsn := os.Getenv("ENERGYLENS_CLOUD_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("ENERGYLENS_CLOUD_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	schema := "estimate_test_" + strings.ReplaceAll(uuid.NewString(), "-", "")
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
	owner, err := accounts.Resolve(ctx, account.Identity{Issuer: "https://id.test", Subject: "estimate-owner"})
	if err != nil {
		t.Fatal(err)
	}
	other, err := accounts.Resolve(ctx, account.Identity{Issuer: "https://id.test", Subject: "estimate-other"})
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
	store := PGRunStore{Pool: pool}
	if _, err = store.ListRuns(ctx, other.ID, home.ID, nil, time.Now().UTC()); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-owner run = %v", err)
	}
	products, err := store.ListRuns(ctx, owner.ID, home.ID, nil, time.Now().UTC())
	if err != nil || len(products) != 0 {
		t.Fatalf("empty home: %v %v", products, err)
	}
	publication := strings.Repeat("a", 64)
	if _, err = pool.Exec(ctx, `INSERT INTO catalog_publications(payload_sha256,key_id,envelope_sha256,imported_by,product_count,fact_count) VALUES($1,$1,$1,'test',1,2)`, publication); err != nil {
		t.Fatal(err)
	}
	productID := uuid.NewString()
	productJSON, _ := json.Marshal(map[string]any{"id": productID, "category": "computer", "brand": "Acme", "full_model": "A123", "variant": nil, "region": nil, "fuel": "electricity", "match_quality": "confirmed_exact"})
	if _, err = pool.Exec(ctx, `INSERT INTO catalog_products(id,category,brand,full_model,fuel,match_quality,product_json) VALUES($1,'computer','Acme','A123','electricity','confirmed_exact',$2)`, productID, productJSON); err != nil {
		t.Fatal(err)
	}
	factID := uuid.NewString()
	if _, err = pool.Exec(ctx, `INSERT INTO catalog_fact_revisions(id,product_id,publication_sha256,kind,value,unit,test_basis,included_energy,source_kind,source_url,source_hash,source_hash_basis,source_manifest,reviewed_at,revision,fact_json) VALUES($1,$2,$3,'annual_energy',365,'kWh','{}','{}','epa_exact','https://example.org/fact',$3,'reviewed_row_manifest','sources/fact.json',now(),1,'{}')`, factID, productID, publication); err != nil {
		t.Fatal(err)
	}
	inv := inventory.PGStore{Pool: pool}
	identity := inventory.Identity{Brand: "Acme", FullModel: "A123", Fuel: "electricity", MatchQuality: "confirmed_exact", CatalogProductID: &productID}
	hash := sha256.Sum256([]byte("appliance"))
	a, err := inv.Create(ctx, owner.ID, home.ID, inventory.CreateInput{Name: "Computer", Category: "computer", Identity: identity}, uuid.NewString(), hash[:])
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	runs, err := store.ListRuns(ctx, owner.ID, home.ID, &a.ID, now)
	if err != nil || len(runs) != 1 {
		t.Fatalf("without rate: %v %v", runs, err)
	}
	first := decodeEstimate(t, runs[0])
	if first.Kind != "unavailable" || first.UnavailableReason != "rate_unavailable" {
		t.Fatalf("missing rate: %+v", first)
	}
	if _, err = pool.Exec(ctx, `INSERT INTO tariff_revisions(home_id,revision,fuel,rate,rate_unit,currency,rate_kind,source_ref,source_as_of,is_fallback,effective_from) VALUES($1,1,'electricity',0.18,'kWh','USD','user_tariff','old bill',now()-interval '2 years',false,now()-interval '2 years')`, home.ID); err != nil {
		t.Fatal(err)
	}
	runs, err = store.ListRuns(ctx, owner.ID, home.ID, &a.ID, now.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	stale := decodeEstimate(t, runs[0])
	if stale.Kind != "unavailable" || stale.UnavailableReason != "rate_unavailable" {
		t.Fatalf("stale user rate accepted: %+v", stale)
	}
	fallback := false
	// The monthly estimate requires a tariff covering the entire home month.
	// A one-day-old rate stops covering it after the first day of the month.
	rateDate := now.Add(-35 * 24 * time.Hour).Format(time.RFC3339)
	rate, err := (tariff.PGStore{Pool: pool}).Create(ctx, owner.ID, home.ID, tariff.Create{Fuel: "electricity", Rate: "0.20", RateUnit: "kWh", Currency: "USD", RateKind: "user_tariff", SourceRef: "bill", SourceAsOf: rateDate, IsFallback: &fallback, EffectiveFrom: rateDate}, uuid.NewString(), make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	runs, err = store.ListRuns(ctx, owner.ID, home.ID, &a.ID, now)
	if err != nil {
		t.Fatal(err)
	}
	second := decodeEstimate(t, runs[0])
	if second.Kind != "estimated_operating_cost" || second.Cost == nil || second.ID == first.ID || len(second.EvidenceRefs) != 2 {
		t.Fatalf("reviewed estimate: %+v", second)
	}
	if second.EvidenceRefs[0].Kind != "catalog_fact" || second.EvidenceRefs[1].Kind != "tariff" || second.EvidenceRefs[1].Revision != rate.Revision {
		t.Fatalf("dependency refs: %+v", second.EvidenceRefs)
	}
	runs, err = store.ListRuns(ctx, owner.ID, home.ID, &a.ID, now.Add(time.Second))
	if err != nil || decodeEstimate(t, runs[0]).ID != second.ID {
		t.Fatalf("repeat created new run: %v %v", runs, err)
	}
	name := "New computer"
	patchHash := sha256.Sum256([]byte("patch"))
	if _, err = inv.Patch(ctx, owner.ID, home.ID, a.ID, inventory.PatchInput{Name: &name}, 1, uuid.NewString(), patchHash[:]); err != nil {
		t.Fatal(err)
	}
	var invalidations int
	if err = pool.QueryRow(ctx, `SELECT count(*) FROM estimate_run_invalidations WHERE run_id=$1`, second.ID).Scan(&invalidations); err != nil || invalidations != 1 {
		t.Fatalf("appliance invalidation: %d %v", invalidations, err)
	}
	runs, err = store.ListRuns(ctx, owner.ID, home.ID, &a.ID, now.Add(2*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	third := decodeEstimate(t, runs[0])
	if third.ID == second.ID {
		t.Fatal("appliance change reused run")
	}
	if _, err = pool.Exec(ctx, `INSERT INTO catalog_fact_revisions(id,product_id,publication_sha256,kind,value,unit,test_basis,included_energy,source_kind,source_url,source_hash,source_hash_basis,source_manifest,reviewed_at,revision,fact_json) VALUES($1,$2,$3,'annual_energy',365,'kWh','{}','{}','oem_family','https://example.org/family',$4,'reviewed_row_manifest','sources/family.json',now(),2,'{}')`, uuid.NewString(), productID, publication, strings.Repeat("b", 64)); err != nil {
		t.Fatal(err)
	}
	runs, err = store.ListRuns(ctx, owner.ID, home.ID, &a.ID, now.Add(3*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	family := decodeEstimate(t, runs[0])
	if family.Kind != "unavailable" || family.UnavailableReason != "basis_incomparable" {
		t.Fatalf("family source promoted to exact: %+v", family)
	}
	if _, err = pool.Exec(ctx, `INSERT INTO catalog_fact_revisions(id,product_id,publication_sha256,kind,value,unit,test_basis,included_energy,source_kind,source_url,source_hash,source_hash_basis,source_manifest,reviewed_at,revision,fact_json) VALUES($1,$2,$3,'annual_energy',400,'kWh','{}','{}','epa_exact','https://example.org/conflict',$4,'reviewed_row_manifest','sources/conflict.json',now(),2,'{}')`, uuid.NewString(), productID, publication, strings.Repeat("c", 64)); err != nil {
		t.Fatal(err)
	}
	runs, err = store.ListRuns(ctx, owner.ID, home.ID, &a.ID, now.Add(4*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	conflict := decodeEstimate(t, runs[0])
	if conflict.Kind != "unavailable" || conflict.UnavailableReason != "basis_incomparable" {
		t.Fatalf("conflicting facts accepted: %+v", conflict)
	}
	if _, err = pool.Exec(ctx, `UPDATE estimate_runs SET formula_version='forged' WHERE id=$1`, second.ID); err == nil {
		t.Fatal("estimate run changed")
	}
}

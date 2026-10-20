package inventory

import (
	"context"
	"crypto/sha256"
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

type linkFixture struct {
	t     *testing.T
	ctx   context.Context
	pool  *pgxpool.Pool
	store PGStore
	owner string
	home  string
	pub   string
}

func newLinkFixture(t *testing.T) *linkFixture {
	dsn := os.Getenv("ENERGYLENS_CLOUD_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("ENERGYLENS_CLOUD_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	schema := "inventory_link_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	admin, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(admin.Close)
	if _, err = admin.Exec(ctx, "CREATE SCHEMA "+schema); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { admin.Exec(ctx, "DROP SCHEMA "+schema+" CASCADE") })
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	cfg.ConnConfig.RuntimeParams["search_path"] = schema
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	conn, err := pgx.ConnectConfig(ctx, cfg.ConnConfig)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	if err = migrate.Apply(ctx, conn, migrations.FS); err != nil {
		t.Fatal(err)
	}
	accounts := account.PGStore{Pool: pool}
	owner, err := accounts.Resolve(ctx, account.Identity{Issuer: "https://id.test", Subject: "link-owner"})
	if err != nil {
		t.Fatal(err)
	}
	home, err := accounts.Create(ctx, owner.ID, account.HomeCreate{Name: "House", Timezone: "America/Chicago", Location: account.Location{CountryCode: "US", PostalCode: "78732", Precision: "postal_code", Source: "user_entered"}}, uuid.NewString(), make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	pub := strings.Repeat("b", 64)
	if _, err = pool.Exec(ctx, `INSERT INTO catalog_publications(payload_sha256,key_id,envelope_sha256,imported_by,product_count,fact_count) VALUES($1,$1,$1,'test',0,0)`, pub); err != nil {
		t.Fatal(err)
	}
	return &linkFixture{t, ctx, pool, PGStore{Pool: pool}, owner.ID, home.ID, pub}
}

func (f *linkFixture) product(brand, brandKey, model, quality string, variant, region *string) string {
	f.t.Helper()
	id := uuid.NewString()
	json := `{"id":"` + id + `"` + brandKeyJSON(brandKey) + `}`
	if _, err := f.pool.Exec(f.ctx, `INSERT INTO catalog_products(id,category,brand,full_model,variant,region,fuel,match_quality,product_json) VALUES($1,'refrigerator',$2,$3,$4,$5,'electricity',$6,$7::jsonb)`, id, brand, model, variant, region, quality, json); err != nil {
		f.t.Fatal(err)
	}
	return id
}

func brandKeyJSON(key string) string {
	if key == "" {
		return ""
	}
	return `,"brand_key":"` + key + `"`
}

func (f *linkFixture) fact(product, sourceKind, matched string, reviewed time.Time) {
	f.t.Helper()
	id := uuid.NewString()
	_, err := f.pool.Exec(f.ctx, `INSERT INTO catalog_fact_revisions(id,product_id,publication_sha256,kind,value,unit,test_basis,included_energy,source_kind,source_url,source_hash,source_hash_basis,source_manifest,reviewed_at,revision,fact_json)
	VALUES($1,$2,$3,'annual_energy',300,'kWh',jsonb_build_object('matched_model',$4::text),'{}',$5,'https://example.org/f',$3,'source_row','m',$6,1,'{}')`, id, product, f.pub, matched, sourceKind, reviewed)
	if err != nil {
		f.t.Fatal(err)
	}
}

func (f *linkFixture) create(brand, model, fuel string) Appliance {
	f.t.Helper()
	hash := sha256.Sum256([]byte(brand + model))
	a, err := f.store.Create(f.ctx, f.owner, f.home, CreateInput{Name: "Fridge", Category: "refrigerator", Identity: Identity{Brand: brand, FullModel: model, Fuel: fuel, MatchQuality: "unresolved"}}, uuid.NewString(), hash[:])
	if err != nil {
		f.t.Fatal(err)
	}
	return a
}

func (f *linkFixture) wantLink(a Appliance, product, quality string) {
	f.t.Helper()
	got := a.Identity
	if product == "" && got.CatalogProductID == nil && got.MatchQuality == quality {
		return
	}
	if product == "" || got.CatalogProductID == nil || *got.CatalogProductID != product || got.MatchQuality != quality {
		f.t.Fatalf("identity = %+v, want product %q quality %q", got, product, quality)
	}
	stored, err := f.store.Get(f.ctx, f.owner, f.home, a.ID)
	if err != nil || stored.Identity.CatalogProductID == nil || *stored.Identity.CatalogProductID != product {
		f.t.Fatalf("stored identity %+v %v", stored.Identity, err)
	}
	var column *string
	if err = f.pool.QueryRow(f.ctx, `SELECT catalog_product_id::text FROM appliances WHERE id=$1`, a.ID).Scan(&column); err != nil || column == nil || *column != product {
		f.t.Fatalf("catalog_product_id column %v %v", column, err)
	}
}

func TestCreateLinksExactAndFamilyCatalogProducts(t *testing.T) {
	f := newLinkFixture(t)
	now := time.Now().UTC()

	exact := f.product("Acme", "acme", "A123", "confirmed_exact", nil, nil)
	f.fact(exact, "epa_exact", "", now)
	f.wantLink(f.create("acme", " A123", "electricity"), exact, "confirmed_exact")

	f.product("Acme", "acme", "NOFACT1", "confirmed_exact", nil, nil)
	f.wantLink(f.create("Acme", "NOFACT1", "electricity"), "", "unresolved")

	for _, region := range []string{"US", "CA"} {
		f.fact(f.product("Beta", "beta", "B200", "confirmed_exact", nil, &region), "epa_exact", "", now)
	}
	f.wantLink(f.create("Beta", "B200", "electricity"), "", "unresolved")

	f.wantLink(f.create("Acme", "A123", "natural_gas"), "", "unresolved")

	older := f.product("LG", "lg", "L500", "confirmed_exact", nil, nil)
	newer := f.product("LG Electronics", "lgelectronics", "L500", "confirmed_exact", nil, nil)
	f.fact(older, "epa_exact", "", now.Add(-48*time.Hour))
	f.fact(newer, "epa_exact", "", now.Add(-time.Hour))
	f.wantLink(f.create("LG", "L500", "electricity"), newer, "confirmed_exact")
	f.wantLink(f.create("LG Electronics", "L500", "electricity"), newer, "confirmed_exact")

	oem := f.product("Gamma", "gamma", "G900", "confirmed_exact", nil, nil)
	es := f.product("Gamma", "gamma", "G900", "confirmed_exact", nil, nil)
	f.fact(oem, "oem_exact", "", now)
	f.fact(es, "epa_exact", "", now.Add(-72*time.Hour))
	f.wantLink(f.create("Gamma", "G900", "electricity"), es, "confirmed_exact")

	family := f.product("Delta", "delta", "DX1234A", "documented_variant_family", nil, nil)
	f.fact(family, "oem_family", "DX1234*", now)
	f.wantLink(f.create("Delta", "DX1234B", "electricity"), family, "documented_variant_family")

	for _, id := range []string{"EX5678A", "EX5678B"} {
		p := f.product("Eps", "eps", id, "documented_variant_family", nil, nil)
		f.fact(p, "epa_family", "EX5678*", now)
	}
	f.wantLink(f.create("Eps", "EX5678C", "electricity"), "", "unresolved")

	exactWins := f.product("Delta", "delta", "DX1234C", "confirmed_exact", nil, nil)
	f.fact(exactWins, "epa_exact", "", now)
	f.wantLink(f.create("Delta", "DX1234C", "electricity"), exactWins, "confirmed_exact")
}

func TestPatchModelEditMatchesCreate(t *testing.T) {
	f := newLinkFixture(t)
	now := time.Now().UTC()
	first := f.product("Acme", "acme", "A1", "confirmed_exact", nil, nil)
	second := f.product("Acme", "acme", "A2", "confirmed_exact", nil, nil)
	picked := f.product("Acme", "acme", "A3", "confirmed_exact", nil, nil)
	for _, p := range []string{first, second, picked} {
		f.fact(p, "epa_exact", "", now)
	}
	cases := []struct {
		name  string
		model string
		id    func(old string) *string
		MQ    string
		stale bool
	}{
		{"old id echoed, no catalog match", "NOPE9", func(old string) *string { return &old }, "confirmed_exact", true},
		{"old id echoed, unresolved", "NOPE9", func(old string) *string { return &old }, "unresolved", true},
		{"old id echoed, exact match exists", "A2", func(old string) *string { return &old }, "confirmed_exact", true},
		{"new picked id", "ZZZ1", func(string) *string { return &picked }, "confirmed_exact", false},
		{"no id, confirmed_exact, no match", "NOPE9", func(string) *string { return nil }, "confirmed_exact", false},
		{"no id, unresolved, no match", "NOPE9", func(string) *string { return nil }, "unresolved", false},
		{"no id, exact match exists", "A2", func(string) *string { return nil }, "unresolved", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			base := f.create("Acme", "A1", "electricity")
			f.wantLink(base, first, "confirmed_exact")
			identity := Identity{Brand: "Acme", FullModel: c.model, Fuel: "electricity", MatchQuality: c.MQ, CatalogProductID: c.id(first)}
			hash := sha256.Sum256([]byte("parity" + c.name))
			createIdentity := identity
			if c.stale {
				createIdentity.CatalogProductID = nil
			}
			created, err := f.store.Create(f.ctx, f.owner, f.home, CreateInput{Name: "Fridge", Category: "refrigerator", Identity: createIdentity}, uuid.NewString(), hash[:])
			if err != nil {
				t.Fatal(err)
			}
			patched, err := f.store.Patch(f.ctx, f.owner, f.home, base.ID, PatchInput{Identity: &identity}, base.Version, uuid.NewString(), hash[:])
			if err != nil {
				t.Fatal(err)
			}
			want, got := created.Identity, patched.Identity
			if !sameOptional(want.CatalogProductID, got.CatalogProductID) || want.MatchQuality != got.MatchQuality {
				t.Fatalf("patch identity (%v, %s), create identity (%v, %s)", got.CatalogProductID, got.MatchQuality, want.CatalogProductID, want.MatchQuality)
			}
		})
	}
}

func TestRelinkCatalogLinksUnlinkedAppliancesOnce(t *testing.T) {
	f := newLinkFixture(t)
	now := time.Now().UTC()
	exact := f.create("Acme", "A123", "electricity")
	family := f.create("Delta", "DX1234B", "electricity")
	unknown := f.create("Zed", "Z1", "electricity")
	gas := f.create("Acme", "A123", "natural_gas")
	pExact := f.product("Acme", "acme", "A123", "confirmed_exact", nil, nil)
	f.fact(pExact, "epa_exact", "", now)
	pFamily := f.product("Delta", "delta", "DX1234A", "documented_variant_family", nil, nil)
	f.fact(pFamily, "oem_family", "DX1234*", now)

	got, err := f.store.RelinkCatalog(f.ctx)
	if err != nil || got != (RelinkResult{LinkedExact: 1, LinkedFamily: 1, Unresolved: 1}) {
		t.Fatalf("first pass %+v %v", got, err)
	}
	for id, want := range map[string]string{exact.ID: pExact, family.ID: pFamily} {
		var column *string
		var version int64
		var revisions int
		if err = f.pool.QueryRow(f.ctx, `SELECT catalog_product_id::text,version,(SELECT count(*) FROM appliance_revisions WHERE appliance_id=$1) FROM appliances WHERE id=$1`, id).Scan(&column, &version, &revisions); err != nil || column == nil || *column != want || version != 2 || revisions != 2 {
			t.Fatalf("appliance %s: %v %d %d %v", id, column, version, revisions, err)
		}
	}
	for _, id := range []string{unknown.ID, gas.ID} {
		var version int64
		if err = f.pool.QueryRow(f.ctx, `SELECT version FROM appliances WHERE id=$1`, id).Scan(&version); err != nil || version != 1 {
			t.Fatalf("untouched %s: %d %v", id, version, err)
		}
	}
	got, err = f.store.RelinkCatalog(f.ctx)
	if err != nil || got != (RelinkResult{Unresolved: 1}) {
		t.Fatalf("second pass %+v %v", got, err)
	}
}

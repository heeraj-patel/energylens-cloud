package householdenergy

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"energylens.local/cloud/internal/account"
	"energylens.local/cloud/internal/energyrate"
	"energylens.local/cloud/internal/gasestimate"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

type gasHarness struct {
	t      *testing.T
	ctx    context.Context
	pool   *pgxpool.Pool
	owner  string
	home   string
	router http.Handler
	svc    Service
	facts  int
	pub    bool
}

func fixedGasRate(basis string, usd float64) GasRateResolver {
	return func(context.Context, string) (energyrate.GasResolved, error) {
		return energyrate.GasResolved{USDPerTherm: usd, Basis: basis}, nil
	}
}

func newGasHarness(t *testing.T, gas GasRateResolver) *gasHarness {
	t.Helper()
	ctx, pool := resolutionTestPool(t)
	accounts := account.PGStore{Pool: pool}
	owner, err := accounts.Resolve(ctx, account.Identity{Issuer: "https://id.test", Subject: "gas-owner"})
	if err != nil {
		t.Fatal(err)
	}
	home, err := accounts.Create(ctx, owner.ID, account.HomeCreate{Name: "House", Timezone: "America/Chicago", Location: account.Location{CountryCode: "US", PostalCode: "78732", Precision: "postal_code", Source: "user_entered"}}, uuid.NewString(), make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 10, 21, 18, 0, 0, 0, time.UTC)
	h := &gasHarness{t: t, ctx: ctx, pool: pool, owner: owner.ID, home: home.ID}
	h.svc = Service{Pool: pool, Auth: func(next http.Handler) http.Handler { return next }, UserID: func(*http.Request) string { return owner.ID },
		Now: func() time.Time { return now }, GasRate: gas,
		NationalRate: func(context.Context) (float64, error) { return 0.1831, nil }}
	router := chi.NewRouter()
	router.Route("/v1", h.svc.Routes)
	h.router = router
	return h
}

func (h *gasHarness) exec(sql string, args ...any) {
	h.t.Helper()
	if _, err := h.pool.Exec(h.ctx, sql, args...); err != nil {
		h.t.Fatal(err)
	}
}

// product seeds a catalog product with active facts keyed by {kind, unit}.
func (h *gasHarness) product(category, fuel, model string, facts map[[2]string]float64, basis string) string {
	h.t.Helper()
	hash := strings.Repeat("d", 64)
	if !h.pub {
		h.exec(`INSERT INTO catalog_publications(payload_sha256,key_id,envelope_sha256,imported_by,product_count,fact_count) VALUES($1,$1,$1,'test',1,1)`, hash)
		h.pub = true
	}
	id := uuid.NewString()
	h.exec(`INSERT INTO catalog_products(id,category,brand,full_model,fuel,match_quality,product_json) VALUES($1,$2,'Acme',$3,$4,'confirmed_exact','{}')`, id, category, model, fuel)
	for ku, v := range facts {
		h.facts++
		fact, _ := json.Marshal(map[string]any{"id": uuid.NewString(), "kind": ku[0], "value": v, "unit": ku[1]})
		h.exec(`INSERT INTO catalog_fact_revisions(id,product_id,publication_sha256,kind,value,unit,test_basis,included_energy,source_kind,source_url,source_hash,source_hash_basis,source_manifest,reviewed_at,revision,fact_json)
			VALUES($1,$2,$3,$4,$5,$6,$7::jsonb,'{}','other_official','https://example.org/f',$8,'reviewed_row_manifest','m','2026-10-01T00:00:00Z',1,$9)`,
			uuid.NewString(), id, hash, ku[0], v, ku[1], basis, fmt.Sprintf("%064x", h.facts), fact)
	}
	return id
}

func (h *gasHarness) appliance(category, fuel, model string, product *string) string {
	h.t.Helper()
	id := uuid.NewString()
	identity := fmt.Sprintf(`{"brand":"Acme","full_model":%q,"fuel":%q,"match_quality":"confirmed_exact"}`, model, fuel)
	if product == nil {
		identity = fmt.Sprintf(`{"brand":"Acme","full_model":%q,"fuel":%q,"match_quality":"unresolved"}`, model, fuel)
	}
	h.exec(`INSERT INTO appliances(id,home_id,name,category,catalog_product_id,identity) VALUES($1,$2,$3,$4,$5,$6)`, id, h.home, model, category, product, identity)
	return id
}

func (h *gasHarness) get(path string) map[string]any {
	h.t.Helper()
	req := httptest.NewRequest("GET", "/v1/homes/"+h.home+path, nil)
	w := httptest.NewRecorder()
	h.router.ServeHTTP(w, req)
	if w.Code != 200 {
		h.t.Fatalf("%s: %d %s", path, w.Code, w.Body.String())
	}
	var data map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &data); err != nil {
		h.t.Fatal(err)
	}
	return data
}

func deviceRow(t *testing.T, view map[string]any, id string) map[string]any {
	t.Helper()
	rows, _ := view["device_allocations"].([]any)
	for _, r := range rows {
		row := r.(map[string]any)
		if row["appliance_id"] == id {
			return row["allocation"].(map[string]any)
		}
	}
	return nil
}

func amountOf(t *testing.T, m map[string]any, key string) float64 {
	t.Helper()
	v, ok := m[key].(map[string]any)
	if !ok {
		t.Fatalf("%s missing in %+v", key, m)
	}
	s, _ := v["amount"].(string)
	if s == "" {
		s, _ = v["value"].(string)
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		t.Fatalf("%s: %v", key, err)
	}
	return f
}

func near(t *testing.T, what string, got, want, tol float64) {
	t.Helper()
	if math.Abs(got-want) > tol {
		t.Fatalf("%s = %v, want %v +- %v", what, got, want, tol)
	}
}

func absent(t *testing.T, m map[string]any, keys ...string) {
	t.Helper()
	for _, k := range keys {
		if _, ok := m[k]; ok {
			t.Fatalf("%s present: %+v", k, m[k])
		}
	}
}

const dryerCEF = `{"appendix":"D1"}`

func TestGasDryerYearWithNationalRate(t *testing.T) {
	h := newGasHarness(t, fixedGasRate(energyrate.GasBasisStatic, 1.481))
	p := h.product("dryer", "natural_gas", "DVG45T6000V", map[[2]string]float64{{"combined_energy_factor", "lb/kWh"}: 3.3}, dryerCEF)
	id := h.appliance("dryer", "natural_gas", "DVG45T6000V", &p)
	// Month granularity covers calendar 2026: one full year of the yearly figures.
	year := h.get("/energy-trends?granularity=month")
	totals := year["totals"].(map[string]any)
	near(t, "totals.gas_cost", amountOf(t, totals, "gas_cost"), 33.62, 0.01)
	absent(t, totals, "total_cost", "gas_energy", "fuel")
	if totals["energy"] != nil || totals["cost"] != nil {
		t.Fatalf("T1: totals stay metered electric and empty here: %+v", totals)
	}
	sum := 0.0
	for _, b := range year["buckets"].([]any) {
		if row := deviceRow(t, b.(map[string]any), id); row != nil {
			sum += amountOf(t, row, "gas_cost")
		}
	}
	near(t, "bucket gas_cost sum", sum, 33.62, 0.01)

	day := h.get("/dashboard")
	row := deviceRow(t, day, id)
	if row == nil {
		t.Fatal("gas dryer row missing")
	}
	if row["fuel"] != "natural_gas" || row["gas_basis"] != "derived_from_cef" || row["gas_rate_basis"] != "static" || row["rate_basis"] != "us_average" || row["evidence_basis"] != "estimated" {
		t.Fatalf("markers: %+v", row)
	}
	near(t, "electric/day", amountOf(t, row, "energy"), 59.43/365, 1e-4)
	near(t, "therms/day", amountOf(t, row, "gas_energy"), 22.698/365, 1e-4)
	near(t, "kwh_eq/day", amountOf(t, row, "total_energy_kwh_eq"), 724.65/365, 1e-3)
	near(t, "gas_cost/day", amountOf(t, row, "gas_cost"), 22.698/365*1.481, 1e-4)
	near(t, "electric cost/day", amountOf(t, row, "cost"), 59.43/365*0.1831, 1e-4)
	near(t, "total_cost/day", amountOf(t, row, "total_cost"), amountOf(t, row, "cost")+amountOf(t, row, "gas_cost"), 1e-6)
	if row["gas_energy"].(map[string]any)["unit"] != "therm" || row["total_energy_kwh_eq"].(map[string]any)["unit"] != "kWh" {
		t.Fatalf("units: %+v", row)
	}
}

func TestGasRateBasisIsAlwaysFlagged(t *testing.T) {
	for _, tc := range []struct {
		name  string
		gas   GasRateResolver
		basis string
		cost  float64
	}{
		{"national fallback", fixedGasRate(energyrate.GasBasisNational, 1.2), "national_average", 1.2},
		{"state fallback", fixedGasRate(energyrate.GasBasisState, 1.6), "state_average", 1.6},
		{"user tariff wins", fixedGasRate(energyrate.GasBasisNational, 1.2), "user_tariff", 2.0},
		{"rate without basis is no rate", fixedGasRate("", 1.2), "", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newGasHarness(t, tc.gas)
			p := h.product("dryer", "natural_gas", "DVG45T6000V", map[[2]string]float64{{"combined_energy_factor", "lb/kWh"}: 3.3}, dryerCEF)
			id := h.appliance("dryer", "natural_gas", "DVG45T6000V", &p)
			if tc.basis == "user_tariff" {
				now := h.svc.Now()
				h.exec(`INSERT INTO tariff_revisions(home_id,revision,fuel,rate,rate_unit,currency,rate_kind,source_ref,source_as_of,is_fallback,effective_from) VALUES($1,1,'natural_gas',2.0,'therm','USD','user_tariff','bill',$2,false,$3)`, h.home, now.Add(-time.Hour), now.AddDate(0, -1, 0))
			}
			row := deviceRow(t, h.get("/dashboard"), id)
			if tc.basis == "" {
				absent(t, row, "gas_cost", "total_cost", "gas_rate_basis")
				return
			}
			if row["gas_rate_basis"] != tc.basis {
				t.Fatalf("gas_rate_basis %v, want %s", row["gas_rate_basis"], tc.basis)
			}
			near(t, "gas_cost/day", amountOf(t, row, "gas_cost"), 22.698/365*tc.cost, 1e-4)
		})
	}
}

func TestNoGasRateKeepsElectricCostAndOmitsTotals(t *testing.T) {
	h := newGasHarness(t, nil)
	p := h.product("dryer", "natural_gas", "DVG45T6000V", map[[2]string]float64{{"combined_energy_factor", "lb/kWh"}: 3.3}, dryerCEF)
	id := h.appliance("dryer", "natural_gas", "DVG45T6000V", &p)
	day := h.get("/dashboard")
	row := deviceRow(t, day, id)
	absent(t, row, "gas_cost", "total_cost", "gas_rate_basis")
	if row["gas_energy"] == nil || row["cost"] == nil {
		t.Fatalf("therms and electric cost stay: %+v", row)
	}
	absent(t, day["totals"].(map[string]any), "gas_cost", "total_cost")
}

func TestGasWasteOnlyFromPerModelStandbyFact(t *testing.T) {
	h := newGasHarness(t, fixedGasRate(energyrate.GasBasisNational, 1.2))
	dryer := h.appliance("dryer", "natural_gas", "D1", ptrTo(h.product("dryer", "natural_gas", "D1", map[[2]string]float64{{"combined_energy_factor", "lb/kWh"}: 3.3}, dryerCEF)))
	tank := h.appliance("water_heater", "natural_gas", "W1", ptrTo(h.product("water_heater", "natural_gas", "W1", map[[2]string]float64{{"annual_fuel", "therm"}: 178}, "{}")))
	typicalOnly := h.appliance("dryer", "natural_gas", "D2", nil)
	boiler := h.appliance("boiler", "natural_gas", "B1", ptrTo(h.product("boiler", "natural_gas", "B1", map[[2]string]float64{{"standby_power", "W"}: 9, {"off_mode_power", "W"}: 5}, `{"product_group_code":"boiler"}`)))
	offOnly := h.appliance("boiler", "natural_gas", "B2", ptrTo(h.product("boiler", "natural_gas", "B2", map[[2]string]float64{{"off_mode_power", "W"}: 5}, `{"product_group_code":"boiler"}`)))
	day := h.get("/dashboard")
	for _, id := range []string{dryer, tank, typicalOnly, offOnly} {
		row := deviceRow(t, day, id)
		if row == nil {
			t.Fatalf("row %s missing", id)
		}
		absent(t, row, "waste_basis", "standby_cost")
		if row["waste_energy"] != nil || row["waste_cost"] != nil {
			t.Fatalf("gas class got waste: %+v", row)
		}
	}
	b := deviceRow(t, day, boiler)
	if b["waste_basis"] != "rated_standby" || b["fuel"] != "natural_gas" {
		t.Fatalf("boiler: %+v", b)
	}
	// Upper bound: max(standby, off) W x 8.76 over the year, here one day.
	near(t, "boiler standby kWh", amountOf(t, b, "waste_energy"), 9*8.76/365, 1e-5)
	near(t, "boiler standby cost", amountOf(t, b, "standby_cost"), 9*8.76/365*0.1831, 1e-5)
	near(t, "waste_cost = standby_cost", amountOf(t, b, "waste_cost"), amountOf(t, b, "standby_cost"), 1e-9)
	absent(t, b, "gas_energy", "gas_cost", "total_cost")

	// Gas energy is never waste: a priced dryer leaves useful/waste unset.
	d := deviceRow(t, day, dryer)
	if d["useful_energy"] != nil || d["unclassified_energy"] == nil {
		t.Fatalf("dryer split: %+v", d)
	}
}

func TestGasAppliancesStayOutOfStandbyTips(t *testing.T) {
	h := newGasHarness(t, fixedGasRate(energyrate.GasBasisNational, 1.2))
	boiler := h.appliance("boiler", "natural_gas", "B1", ptrTo(h.product("boiler", "natural_gas", "B1", map[[2]string]float64{{"standby_power", "W"}: 9}, `{"product_group_code":"boiler"}`)))
	if row := deviceRow(t, h.get("/dashboard"), boiler); row == nil || row["waste_cost"] == nil {
		t.Fatalf("the boiler has a standby figure: %+v", row)
	}
	waste, err := h.svc.DeviceMonthlyWaste(h.ctx, h.owner, h.home)
	if err != nil {
		t.Fatal(err)
	}
	for _, w := range waste {
		if w.ApplianceID == boiler {
			t.Fatalf("gas standby fed the top-savings waste: %+v", w)
		}
	}
}

func TestTotalsGasCostSumsGasAppliancesAndElectricRowsAreUnchanged(t *testing.T) {
	h := newGasHarness(t, fixedGasRate(energyrate.GasBasisNational, 1.5))
	dryer := h.appliance("dryer", "natural_gas", "D1", ptrTo(h.product("dryer", "natural_gas", "D1", map[[2]string]float64{{"combined_energy_factor", "lb/kWh"}: 3.3}, dryerCEF)))
	tank := h.appliance("water_heater", "natural_gas", "W1", ptrTo(h.product("water_heater", "natural_gas", "W1", map[[2]string]float64{{"annual_fuel", "therm"}: 178, {"annual_energy", "kWh"}: 40}, "{}")))
	propane := h.appliance("water_heater", "propane", "P1", ptrTo(h.product("water_heater", "propane", "P1", map[[2]string]float64{{"annual_fuel", "gallon"}: 200}, "{}")))
	lamp := uuid.NewString()
	h.exec(`INSERT INTO appliances(id,home_id,name,category,identity) VALUES($1,$2,'Lamp','light','{"fuel":"electricity","match_quality":"unresolved"}')`, lamp, h.home)
	h.exec(`INSERT INTO datawiz_energy_resolutions(appliance_ref,request_sha256,record,resolved_at) VALUES($1,$2,$3,now())`, "cloud:"+lamp, make([]byte, 32), `{"appliance_version":1,"status":"resolved","device_kind":"real_non_metering","values":{"kwh_per_day":0.008,"waste_kwh_per_day":0.008}}`)

	day := h.get("/dashboard")
	totals := day["totals"].(map[string]any)
	d, w, p := deviceRow(t, day, dryer), deviceRow(t, day, tank), deviceRow(t, day, propane)
	near(t, "totals.gas_cost", amountOf(t, totals, "gas_cost"), amountOf(t, d, "gas_cost")+amountOf(t, w, "gas_cost"), 1e-6)
	near(t, "water heater gas", amountOf(t, w, "gas_cost"), 178.0/365*1.5, 1e-4)
	near(t, "water heater electric", amountOf(t, w, "energy"), 40.0/365, 1e-6)
	absent(t, totals, "total_cost", "fuel")

	// Propane: gallons only, so no therms and no gas cost (Q5); electric typical stays.
	if p["fuel"] != "propane" || p["energy"] == nil {
		t.Fatalf("propane: %+v", p)
	}
	absent(t, p, "gas_energy", "gas_energy_kwh_eq", "total_energy_kwh_eq", "gas_cost", "total_cost", "gas_rate_basis", "gas_basis")

	// AC06: the electric appliance keeps exactly the old fields, no new keys.
	l := deviceRow(t, day, lamp)
	want := []string{"energy", "cost", "useful_energy", "useful_cost", "waste_energy", "waste_cost", "unclassified_energy", "unclassified_cost", "availability", "evidence_basis", "rate_basis"}
	if len(l) != len(want) {
		t.Fatalf("electric row keys changed: %+v", l)
	}
	for _, k := range want {
		if _, ok := l[k]; !ok {
			t.Fatalf("electric row lost %s: %+v", k, l)
		}
	}
	absent(t, l, "fuel", "gas_energy", "total_cost", "gas_cost", "standby_cost")
}

func TestElectricHomeOutputHasNoGasFieldsAndNeverResolvesGasRate(t *testing.T) {
	calls := 0
	h := newGasHarness(t, func(context.Context, string) (energyrate.GasResolved, error) {
		calls++
		return energyrate.GasResolved{USDPerTherm: 1, Basis: energyrate.GasBasisNational}, nil
	})
	lamp := uuid.NewString()
	h.exec(`INSERT INTO appliances(id,home_id,name,category,identity) VALUES($1,$2,'Lamp','light','{"fuel":"electricity","match_quality":"unresolved"}')`, lamp, h.home)
	for _, path := range []string{"/dashboard", "/energy-trends?granularity=day"} {
		view := h.get(path)
		raw, _ := json.Marshal(view)
		for _, k := range []string{"gas_", "total_cost", "standby_cost", `"fuel"`, "total_energy_kwh_eq"} {
			if strings.Contains(string(raw), k) {
				t.Fatalf("%s: electric home output contains %s", path, k)
			}
		}
	}
	if calls != 0 {
		t.Fatalf("gas rate resolved %d times for an electric home", calls)
	}
}

func TestApplyGasOnMeasuredRowKeepsElectricAndAddsGas(t *testing.T) {
	cost := money{Amount: "0.100000", Currency: "USD"}
	a := allocation{Energy: &quantity{Value: "0.500000000000", Unit: "kWh"}, Cost: cost, Availability: "partial", EvidenceBasis: "mixed"}
	g := gasAppliance{id: "a", fuel: "natural_gas", estimate: gasestimate.Estimate{GasBasis: gasestimate.BasisDerivedFromCEF}}
	f := gasFigures{therms: big.NewRat(2, 1), kwhEq: big.NewRat(5862, 100)}
	applyGas(&a, g, f, &GasRate{USDPerTherm: big.NewRat(3, 2), Basis: GasRateBasisUserTariff})
	if a.Energy.Value != "0.500000000000" || a.Cost != cost {
		t.Fatalf("measured electric changed: %+v", a)
	}
	if a.TotalEnergyKWhEq.Value != "59.120000000000" || a.GasCost.(money).Amount != "3.000000" || a.TotalCost.(money).Amount != "3.100000" || a.GasRateBasis != "user_tariff" || a.Fuel != "natural_gas" {
		t.Fatalf("gas fields: %+v", a)
	}
	// No gas rate: total_cost stays unset so the UI falls back to cost.
	b := allocation{Energy: a.Energy, Cost: cost}
	applyGas(&b, g, f, nil)
	if b.GasCost != nil || b.TotalCost != nil || b.GasEnergy == nil {
		t.Fatalf("no rate: %+v", b)
	}
	// Known electric energy with no electric price: the full cost is unknown.
	c := allocation{Energy: a.Energy}
	applyGas(&c, g, f, &GasRate{USDPerTherm: big.NewRat(3, 2), Basis: "static"})
	if c.GasCost == nil || c.TotalCost != nil {
		t.Fatalf("unpriced electric part: %+v", c)
	}
}

func ptrTo(s string) *string { return &s }

func TestDecorateProratesMeteredGasOverFullPeriod(t *testing.T) {
	start := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	buckets := make([]bucket, 30)
	rows := make([][]deviceAllocation, 30)
	for i := range buckets {
		buckets[i] = bucket{Start: start.AddDate(0, 0, i), End: start.AddDate(0, 0, i+1)}
		if i < 10 {
			rows[i] = []deviceAllocation{{ApplianceID: "a"}}
		}
	}
	therms, kwh := 365.0, 365.0*29.3
	g := gasAppliance{id: "a", fuel: "natural_gas", metered: true, estimate: gasestimate.Estimate{GasTherms: &therms, GasKWhEq: &kwh}}
	gc := &gasContext{appliances: []gasAppliance{g}, rate: &GasRate{USDPerTherm: big.NewRat(2, 1), Basis: "static"}}
	period := []deviceAllocation{{ApplianceID: "a"}}
	gc.decorate(buckets, rows, period)
	if got := period[0].Allocation.GasCost.(money).Amount; got != "60.000000" {
		t.Fatalf("period gas_cost = %s, want the full 30-day value 60.000000", got)
	}
	if got := rows[0][0].Allocation.GasCost.(money).Amount; got != "2.000000" {
		t.Fatalf("bucket gas_cost = %s", got)
	}
}

// meteredGasFallback runs decorate and gasDeviceRows over 30 day buckets where
// only the first 10 carry a measured row, as the dashboard does.
func meteredGasFallback(t *testing.T, est gasestimate.Estimate, measured func(i int) allocation) ([]bucket, []deviceAllocation) {
	t.Helper()
	h := newGasHarness(t, nil)
	start := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	buckets := make([]bucket, 30)
	rows := make([][]deviceAllocation, 30)
	var period allocation
	for i := range buckets {
		buckets[i] = bucket{Start: start.AddDate(0, 0, i), End: start.AddDate(0, 0, i+1)}
		if i < 10 {
			a := measured(i)
			rows[i] = []deviceAllocation{{ApplianceID: "a", Allocation: a}}
			period = sumMeasuredRows(period, a)
		}
	}
	g := gasAppliance{id: "a", fuel: "natural_gas", metered: true, estimate: est}
	gc := &gasContext{appliances: []gasAppliance{g}, rate: &GasRate{USDPerTherm: big.NewRat(2, 1), Basis: "static"}}
	periodRows := []deviceAllocation{{ApplianceID: "a", Allocation: period}, {ApplianceID: "e", Allocation: allocation{Energy: &quantity{Value: "1.000000000000", Unit: "kWh"}}}}
	gc.decorate(buckets, rows, periodRows)
	for i := range buckets {
		buckets[i].DeviceAllocations = rows[i]
	}
	tx, err := h.pool.Begin(h.ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(h.ctx)
	byBucket, extra, err := gasDeviceRows(h.ctx, tx, gc, h.home, buckets, time.Date(2026, 10, 21, 0, 0, 0, 0, time.UTC), h.svc.NationalRate, periodRows)
	if err != nil || len(extra) != 0 {
		t.Fatalf("rows: %+v %v", extra, err)
	}
	for i := range buckets {
		buckets[i].DeviceAllocations = append(buckets[i].DeviceAllocations, byBucket[i]...)
	}
	if e := periodRows[1].Allocation; e.Energy.Value != "1.000000000000" || e.EvidenceBasis != "" || e.Fuel != "" {
		t.Fatalf("electric row changed: %+v", e)
	}
	return buckets, periodRows[:1]
}

// sumMeasuredRows adds measured energy, cost and waste of one bucket to the period row.
func sumMeasuredRows(p, a allocation) allocation {
	var out allocation
	out.Availability, out.EvidenceBasis, out.WasteBasis, out.RateBasis = a.Availability, a.EvidenceBasis, a.WasteBasis, a.RateBasis
	add := func(x, y any) *big.Rat {
		var s *big.Rat
		addRat(&s, ratOf(x))
		addRat(&s, ratOf(y))
		return s
	}
	q := func(r *big.Rat) *quantity {
		if r == nil {
			return nil
		}
		return &quantity{Value: r.FloatString(12), Unit: "kWh"}
	}
	m := func(r *big.Rat) any {
		if r == nil {
			return nil
		}
		return money{Amount: r.FloatString(6), Currency: "USD"}
	}
	out.Energy = q(add(p.Energy, a.Energy))
	out.Cost = m(add(p.Cost, a.Cost))
	if w := add(p.WasteEnergy, a.WasteEnergy); w != nil {
		out.WasteEnergy, out.UsefulEnergy = q(w), q(add(p.UsefulEnergy, a.UsefulEnergy))
		out.WasteCost, out.UsefulCost = m(add(p.WasteCost, a.WasteCost)), m(add(p.UsefulCost, a.UsefulCost))
	} else {
		out.UnclassifiedEnergy = out.Energy
		out.UnclassifiedCost = out.Cost
	}
	return out
}

func rowSum(buckets []bucket, id string, field func(allocation) any) float64 {
	var sum float64
	for _, b := range buckets {
		for _, d := range b.DeviceAllocations {
			if d.ApplianceID == id {
				if r := ratOf(field(d.Allocation)); r != nil {
					f, _ := r.Float64()
					sum += f
				}
			}
		}
	}
	return sum
}

func countRows(buckets []bucket, id string) (n int) {
	for _, b := range buckets {
		if hasDeviceRow(b.DeviceAllocations, id) {
			n++
		}
	}
	return n
}

func TestMeteredGasBoilerUnmeasuredBucketsFallBackToRatedStandby(t *testing.T) {
	therms, kwh, elec, standby := 365.0, 365.0*29.3, 87.6, 87.6
	est := gasestimate.Estimate{GasTherms: &therms, GasKWhEq: &kwh, ElectricKWh: &elec, StandbyKWh: &standby}
	buckets, period := meteredGasFallback(t, est, func(int) allocation {
		return allocation{Energy: &quantity{Value: "0.500000000000", Unit: "kWh"}, Cost: money{Amount: "0.091550", Currency: "USD"},
			UsefulEnergy: &quantity{Value: "0.400000000000", Unit: "kWh"}, WasteEnergy: &quantity{Value: "0.100000000000", Unit: "kWh"},
			UsefulCost: money{Amount: "0.073240", Currency: "USD"}, WasteCost: money{Amount: "0.018310", Currency: "USD"},
			WasteBasis: "standby", RateBasis: "us_average", Availability: "partial", EvidenceBasis: "measured"}
	})
	if got := countRows(buckets, "a"); got != 30 {
		t.Fatalf("bucket rows = %d, want 30", got)
	}
	for i, b := range buckets {
		a := b.DeviceAllocations[0].Allocation
		if i < 10 && (a.WasteBasis != "standby" || a.Energy.Value != "0.500000000000" || a.GasCost == nil) {
			t.Fatalf("measured bucket %d changed: %+v", i, a)
		}
		if i >= 10 && (a.WasteBasis != "rated_standby" || a.EvidenceBasis != "estimated" || a.StandbyCost == nil || a.GasCost == nil) {
			t.Fatalf("unmeasured bucket %d: %+v", i, a)
		}
	}
	p := period[0].Allocation
	near(t, "period waste", ratFloat(p.WasteEnergy), 10*0.1+20*0.24, 1e-9)
	near(t, "period waste = bucket sum", ratFloat(p.WasteEnergy), rowSum(buckets, "a", func(a allocation) any { return a.WasteEnergy }), 1e-9)
	near(t, "period energy", ratFloat(p.Energy), 10*0.5+20*0.24, 1e-9)
	near(t, "period useful+waste", ratFloat(p.UsefulEnergy)+ratFloat(p.WasteEnergy), ratFloat(p.Energy), 1e-9)
	near(t, "period cost", ratFloat(p.Cost), rowSum(buckets, "a", func(a allocation) any { return a.Cost }), 1e-5)
	near(t, "period waste cost", ratFloat(p.WasteCost), rowSum(buckets, "a", func(a allocation) any { return a.WasteCost }), 1e-5)
	near(t, "period standby cost", ratFloat(p.StandbyCost), 20*0.24*0.1831, 1e-5)
	near(t, "period gas_cost", ratFloat(p.GasCost), 60, 1e-9)
	near(t, "gas_cost sums", ratFloat(p.GasCost), rowSum(buckets, "a", func(a allocation) any { return a.GasCost }), 1e-5)
	near(t, "total cost", ratFloat(p.TotalCost), ratFloat(p.Cost)+60, 1e-5)
	if p.WasteBasis != "standby" || p.EvidenceBasis != "mixed" {
		t.Fatalf("period marks: %+v", p)
	}
}

func TestMeteredGasWithoutStandbyFigureGetsEstimateButNoWaste(t *testing.T) {
	therms, kwh, elec := 365.0, 365.0*29.3, 36.5
	est := gasestimate.Estimate{GasTherms: &therms, GasKWhEq: &kwh, ElectricKWh: &elec}
	buckets, period := meteredGasFallback(t, est, func(int) allocation {
		return allocation{Energy: &quantity{Value: "0.500000000000", Unit: "kWh"}, Cost: money{Amount: "0.091550", Currency: "USD"}, Availability: "partial", EvidenceBasis: "measured"}
	})
	if got := countRows(buckets, "a"); got != 30 {
		t.Fatalf("bucket rows = %d, want 30", got)
	}
	for i := 10; i < 30; i++ {
		a := buckets[i].DeviceAllocations[0].Allocation
		if a.Energy == nil || a.GasCost == nil || a.WasteEnergy != nil || a.WasteBasis != "" || a.StandbyCost != nil {
			t.Fatalf("unmeasured bucket %d: %+v", i, a)
		}
	}
	p := period[0].Allocation
	if p.WasteEnergy != nil || p.WasteBasis != "" {
		t.Fatalf("period waste without a standby figure: %+v", p)
	}
	near(t, "period energy", ratFloat(p.Energy), 10*0.5+20*0.1, 1e-9)
	near(t, "period unclassified", ratFloat(p.UnclassifiedEnergy), ratFloat(p.Energy), 1e-9)
	near(t, "gas_cost sums", ratFloat(p.GasCost), rowSum(buckets, "a", func(a allocation) any { return a.GasCost }), 1e-5)
}

func ratFloat(v any) float64 {
	f, _ := ratOf(v).Float64()
	return f
}

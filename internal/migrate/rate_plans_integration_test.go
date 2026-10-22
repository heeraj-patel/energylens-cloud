package migrate

import (
	"context"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
)

const planInsert = `INSERT INTO utility_rate_plans(eiaid,utility_name,sector,plan_key,version,plan_name,is_standard,base_tiers,base_label,
fixed_charge,fixed_charge_per,currency,effective_from,effective_to,source_kind,source_url,source_file_url,source_sha256,retrieved_at,checked_at)
VALUES($1,'Example Power','residential',$2,$3,'E-TOU-C',false,$4,'winter off-peak',$5,$6,'USD','2026-06-01',$7,$8,
'https://example.test/tariff.pdf','https://example.test/tariff.pdf',$9,now(),now()) RETURNING id::text`

type planArgs struct {
	eiaid, key string
	version    int
	tiers      any
	fixed      any
	per        any
	to         any
	kind, sha  string
}

func validPlan() planArgs {
	return planArgs{"14328", "official:pge:e-tou-c:p", 1, `[{"through_kwh":11.0,"per":"day","rate":"0.28617"},{"through_kwh":null,"per":"day","rate":"0.36757"}]`,
		"0.79343", "day", nil, "official_tariff", strings.Repeat("b", 64)}
}

func insertPlan(ctx context.Context, conn *pgx.Conn, a planArgs) (string, error) {
	var id string
	err := conn.QueryRow(ctx, planInsert, a.eiaid, a.key, a.version, a.tiers, a.fixed, a.per, a.to, a.kind, a.sha).Scan(&id)
	return id, err
}

const periodInsert = `INSERT INTO utility_rate_plan_periods(plan_id,ordinal,name,adder,days,start_local,end_local,months) VALUES($1,$2,$3,$4,$5,$6,$7,$8)`

var allDays = []string{"mon", "tue", "wed", "thu", "fri", "sat", "sun"}

func TestUtilityRatePlanConstraints(t *testing.T) {
	ctx, conn, _ := migratedSchemaConn(t)
	id, err := insertPlan(ctx, conn, validPlan())
	if err != nil {
		t.Fatalf("valid plan rejected: %v", err)
	}
	urdb := validPlan()
	urdb.key, urdb.kind, urdb.tiers, urdb.fixed, urdb.per, urdb.to = "urdb:69de6fa62aa9e9994f0ed2bd", "urdb", nil, nil, nil, "2027-01-01"
	if _, err = insertPlan(ctx, conn, urdb); err != nil {
		t.Fatalf("valid urdb plan without base tiers rejected: %v", err)
	}
	bad := map[string]func(*planArgs){
		"non-numeric eiaid":      func(a *planArgs) { a.eiaid = "PGE" },
		"bad plan key":           func(a *planArgs) { a.key = "official:PGE E-TOU-C" },
		"unknown key source":     func(a *planArgs) { a.key = "eia:14328" },
		"zero version":           func(a *planArgs) { a.version = 0 },
		"duplicate version":      func(a *planArgs) {},
		"second current version": func(a *planArgs) { a.version = 2 },
		"tiers not open":         func(a *planArgs) { a.tiers = `[{"through_kwh":11.0,"per":"day","rate":"0.28617"}]` },
		"tiers not array":        func(a *planArgs) { a.tiers = `{"rate":"0.3"}` },
		"negative fixed charge":  func(a *planArgs) { a.fixed = "-1" },
		"fixed charge per year":  func(a *planArgs) { a.per = "year" },
		"fixed charge no unit":   func(a *planArgs) { a.per = nil },
		"end before start":       func(a *planArgs) { a.to = "2026-05-01" },
		"bad source kind":        func(a *planArgs) { a.kind = "user_tariff" },
		"urdb kind official key": func(a *planArgs) { a.kind = "urdb" },
		"bad sha":                func(a *planArgs) { a.sha = "XYZ" },
	}
	for name, mutate := range bad {
		a := validPlan()
		mutate(&a)
		if _, err := insertPlan(ctx, conn, a); err == nil {
			t.Errorf("%s: invalid plan accepted", name)
		}
	}
	if _, err := conn.Exec(ctx, `INSERT INTO utility_rate_plans(eiaid,utility_name,sector,plan_key,version,plan_name,is_standard,base_label,currency,effective_from,source_kind,source_url,source_file_url,source_sha256,retrieved_at,checked_at)
VALUES('1','x','commercial','urdb:x',1,'x',false,'x','USD','2026-01-01','urdb','u','u',$1,now(),now())`, strings.Repeat("a", 64)); err == nil {
		t.Error("non-residential sector accepted")
	}

	if _, err = conn.Exec(ctx, `UPDATE utility_rate_plans SET checked_at=now()+interval '1 day' WHERE id=$1`, id); err != nil {
		t.Fatalf("checked_at update rejected: %v", err)
	}
	if _, err = conn.Exec(ctx, `UPDATE utility_rate_plans SET base_tiers='[{"through_kwh":null,"per":"month","rate":"0.1"}]' WHERE id=$1`, id); err == nil {
		t.Error("base_tiers update accepted")
	}
	if _, err = conn.Exec(ctx, `UPDATE utility_rate_plans SET source_notes='["x"]' WHERE id=$1`, id); err == nil {
		t.Error("source_notes update accepted")
	}
	if _, err = conn.Exec(ctx, `DELETE FROM utility_rate_plans WHERE id=$1`, id); err == nil {
		t.Error("plan delete accepted")
	}
	if _, err = conn.Exec(ctx, `UPDATE utility_rate_plans SET superseded_at=now() WHERE id=$1`, id); err != nil {
		t.Fatalf("superseded_at update rejected: %v", err)
	}
	next := validPlan()
	next.version = 2
	if _, err = insertPlan(ctx, conn, next); err != nil {
		t.Fatalf("next version after supersede rejected: %v", err)
	}
}

func TestUtilityRatePlanPeriodConstraints(t *testing.T) {
	ctx, conn, _ := migratedSchemaConn(t)
	plan, err := insertPlan(ctx, conn, validPlan())
	if err != nil {
		t.Fatal(err)
	}
	summer := []int16{6, 7, 8, 9}
	good := []struct {
		name, start, end string
		adder            string
		days             []string
	}{
		{"summer peak", "16:00", "21:00", "0.15483", allDays},
		{"summer off-peak", "21:00", "16:00", "0.03183", allDays},
		{"super off-peak", "00:00", "06:00", "-0.08988", []string{"mon", "tue", "wed", "thu", "fri"}},
		{"peak", "08:00", "24:00", "0.2264", []string{"sat", "sun"}},
	}
	for i, p := range good {
		if _, err := conn.Exec(ctx, periodInsert, plan, i+1, p.name, p.adder, p.days, p.start, p.end, summer); err != nil {
			t.Fatalf("%s rejected: %v", p.name, err)
		}
	}
	var wraps int
	if err := conn.QueryRow(ctx, `SELECT count(*) FROM utility_rate_plan_periods WHERE plan_id=$1 AND end_local < start_local`, plan).Scan(&wraps); err != nil || wraps != 1 {
		t.Fatalf("wrapping row count = %d, %v", wraps, err)
	}
	var end string
	if err := conn.QueryRow(ctx, `SELECT end_local::text FROM utility_rate_plan_periods WHERE plan_id=$1 AND ordinal=4`, plan).Scan(&end); err != nil || end != "24:00:00" {
		t.Fatalf("24:00 end = %q, %v", end, err)
	}
	bad := []struct {
		name string
		args []any
	}{
		{"zero adder", []any{plan, 10, "peak", "0", allDays, "16:00", "21:00", summer}},
		{"zero ordinal", []any{plan, 0, "peak", "0.1", allDays, "16:00", "21:00", summer}},
		{"duplicate ordinal", []any{plan, 1, "peak", "0.1", allDays, "16:00", "21:00", summer}},
		{"blank name", []any{plan, 11, "  ", "0.1", allDays, "16:00", "21:00", summer}},
		{"long name", []any{plan, 12, strings.Repeat("p", 61), "0.1", allDays, "16:00", "21:00", summer}},
		{"bad day", []any{plan, 13, "peak", "0.1", []string{"monday"}, "16:00", "21:00", summer}},
		{"no days", []any{plan, 14, "peak", "0.1", []string{}, "16:00", "21:00", summer}},
		{"same start and end", []any{plan, 15, "peak", "0.1", allDays, "16:00", "16:00", summer}},
		{"start at 24:00", []any{plan, 16, "peak", "0.1", allDays, "24:00", "06:00", summer}},
		{"month 13", []any{plan, 17, "peak", "0.1", allDays, "16:00", "21:00", []int16{13}}},
		{"no months", []any{plan, 18, "peak", "0.1", allDays, "16:00", "21:00", []int16{}}},
		{"unknown plan", []any{"00000000-0000-0000-0000-000000000000", 1, "peak", "0.1", allDays, "16:00", "21:00", summer}},
	}
	for _, c := range bad {
		if _, err := conn.Exec(ctx, periodInsert, c.args...); err == nil {
			t.Errorf("%s: invalid period accepted", c.name)
		}
	}
	if _, err := conn.Exec(ctx, `UPDATE utility_rate_plan_periods SET adder=0.2 WHERE plan_id=$1 AND ordinal=1`, plan); err == nil {
		t.Error("period update accepted")
	}
	if _, err := conn.Exec(ctx, `DELETE FROM utility_rate_plan_periods WHERE plan_id=$1`, plan); err == nil {
		t.Error("period delete accepted")
	}
}

func TestTariffRevisionPlanColumnsAndHomeUtility(t *testing.T) {
	ctx, conn, _ := migratedSchemaConn(t)
	plan, err := insertPlan(ctx, conn, validPlan())
	if err != nil {
		t.Fatal(err)
	}
	var user, home string
	if err = conn.QueryRow(ctx, `INSERT INTO users DEFAULT VALUES RETURNING id::text`).Scan(&user); err != nil {
		t.Fatal(err)
	}
	if err = conn.QueryRow(ctx, `INSERT INTO homes(owner_user_id,name,timezone) VALUES($1,'House','America/Los_Angeles') RETURNING id::text`, user).Scan(&home); err != nil {
		t.Fatal(err)
	}
	revision := `INSERT INTO tariff_revisions(home_id,revision,fuel,rate,rate_unit,currency,rate_kind,source_ref,source_as_of,is_fallback,effective_from,utility_rate_plan_id,plan_chosen_by)
VALUES($1,$2,'electricity',0.36757,'kWh','USD','official_tariff','https://example.test/tariff.pdf',now(),false,now(),$3,$4) RETURNING id::text`
	var rev string
	if err = conn.QueryRow(ctx, revision, home, 1, plan, "default").Scan(&rev); err != nil {
		t.Fatalf("plan revision rejected: %v", err)
	}
	if _, err = conn.Exec(ctx, `INSERT INTO tariff_revisions(home_id,revision,fuel,rate,rate_unit,currency,rate_kind,source_ref,source_as_of,is_fallback,effective_from)
VALUES($1,2,'electricity',0.2,'kWh','USD','user_tariff','user',now(),false,now())`, home); err != nil {
		t.Fatalf("revision without plan rejected: %v", err)
	}
	for name, args := range map[string][]any{
		"plan without chooser": {home, 3, plan, nil},
		"chooser without plan": {home, 4, nil, "user"},
		"unknown chooser":      {home, 5, plan, "sweep"},
	} {
		if _, err := conn.Exec(ctx, revision, args...); err == nil {
			t.Errorf("%s: invalid revision accepted", name)
		}
	}
	periods := `INSERT INTO tariff_revision_periods(tariff_revision_id,ordinal,name,adder,days,start_local,end_local,months) VALUES($1,$2,$3,$4,$5,$6,$7,$8)`
	if _, err = conn.Exec(ctx, periods, rev, 1, "summer off-peak", "0.03183", allDays, "21:00", "16:00", []int16{6, 7, 8, 9}); err != nil {
		t.Fatalf("wrapping revision period rejected: %v", err)
	}
	if _, err = conn.Exec(ctx, periods, rev, 2, "super off-peak", "-0.09341", allDays, "00:00", "06:00", []int16{1, 2, 3, 4, 5, 11, 12}); err != nil {
		t.Fatalf("negative adder rejected: %v", err)
	}
	if _, err = conn.Exec(ctx, periods, rev, 3, "peak", "0", allDays, "16:00", "21:00", []int16{1}); err == nil {
		t.Error("zero revision adder accepted")
	}
	if _, err = conn.Exec(ctx, `UPDATE tariff_revision_periods SET name='x' WHERE tariff_revision_id=$1`, rev); err == nil {
		t.Error("revision period update accepted")
	}
	if _, err = conn.Exec(ctx, `DELETE FROM tariff_revision_periods WHERE tariff_revision_id=$1`, rev); err == nil {
		t.Error("revision period delete accepted")
	}

	utility := `INSERT INTO home_electric_utility(home_id,revision,eiaid,chosen_by) VALUES($1,$2,$3,$4)`
	if _, err = conn.Exec(ctx, utility, home, 1, "14328", "zip_single"); err != nil {
		t.Fatalf("zip utility rejected: %v", err)
	}
	if _, err = conn.Exec(ctx, utility, home, 2, "16612", "user"); err != nil {
		t.Fatalf("user utility rejected: %v", err)
	}
	for name, args := range map[string][]any{
		"duplicate revision": {home, 2, "14328", "user"},
		"bad eiaid":          {home, 3, "PGE", "user"},
		"bad chooser":        {home, 4, "14328", "default"},
		"zero revision":      {home, 0, "14328", "user"},
	} {
		if _, err := conn.Exec(ctx, utility, args...); err == nil {
			t.Errorf("%s: invalid home utility accepted", name)
		}
	}
	if _, err = conn.Exec(ctx, `UPDATE home_electric_utility SET eiaid='1' WHERE home_id=$1`, home); err == nil {
		t.Error("home utility update accepted")
	}
	if _, err = conn.Exec(ctx, `DELETE FROM home_electric_utility WHERE home_id=$1`, home); err == nil {
		t.Error("home utility delete accepted")
	}
}

func TestDatawizRunsAcceptRatePlansRoute(t *testing.T) {
	ctx, conn, _ := migratedSchemaConn(t)
	insert := `INSERT INTO datawiz_runs(id,route,request,mode,status) VALUES(gen_random_uuid(),$1,'{"source":"urdb"}','realtime','running')`
	if _, err := conn.Exec(ctx, insert, "rate_plans"); err != nil {
		t.Fatalf("rate_plans route rejected: %v", err)
	}
	if _, err := conn.Exec(ctx, insert, "catalog"); err != nil {
		t.Fatalf("catalog route rejected: %v", err)
	}
	if _, err := conn.Exec(ctx, insert, "rates"); err == nil {
		t.Error("unknown route accepted")
	}
}

package tariff

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

var (
	ErrUnknownUtility = errors.New("unknown electric utility")
	ErrPlanNotOffered = errors.New("rate plan not offered for the home's utility")
	ErrNoBaseRate     = errors.New("plan has no base rate and the home has no current rate")
	eiaidPattern      = regexp.MustCompile(`^[0-9]{1,12}$`)
	zip5              = regexp.MustCompile(`^([0-9]{5})(?:-?[0-9]{4})?$`)
)

// RankSQL is the reader rank of an electricity revision: a user's own tariff or plan pick, then other official, then regional.
const RankSQL = `CASE WHEN rate_kind='user_tariff' OR plan_chosen_by='user' THEN 0 WHEN rate_kind='official_tariff' THEN 1 ELSE 2 END`

// RefreshAge is how old a plan revision's source_as_of must be before a newer plan check gives it a new revision.
const RefreshAge = 30 * 24 * time.Hour

type UtilityCandidate struct {
	EIAID       string `json:"eiaid"`
	UtilityName string `json:"utility_name"`
}

type ElectricUtility struct {
	EIAID       string `json:"eiaid"`
	UtilityName string `json:"utility_name"`
	ChosenBy    string `json:"chosen_by"`
}

type RatePeriod struct {
	Name       string   `json:"name"`
	Adder      string   `json:"adder"`
	Days       []string `json:"days"`
	StartLocal string   `json:"start_local"`
	EndLocal   string   `json:"end_local"`
	Months     []int16  `json:"months"`
}

type RatePlanSource struct {
	Kind        string   `json:"kind"`
	URL         string   `json:"url"`
	RetrievedAt string   `json:"retrieved_at"`
	CheckedAt   string   `json:"checked_at"`
	Notes       []string `json:"notes"`
}

type UtilityRatePlan struct {
	ID            string         `json:"id"`
	PlanName      string         `json:"plan_name"`
	Variant       *string        `json:"variant"`
	IsStandard    bool           `json:"is_standard"`
	IsTOU         bool           `json:"is_tou"`
	EffectiveFrom string         `json:"effective_from"`
	Periods       []RatePeriod   `json:"periods"`
	BaseLabel     string         `json:"base_label"`
	Source        RatePlanSource `json:"source"`
}

type UtilityRatePlans struct {
	UtilityCandidates []UtilityCandidate `json:"utility_candidates"`
	Utility           *ElectricUtility   `json:"utility"`
	Plans             []UtilityRatePlan  `json:"plans"`
	CurrentPlanID     *string            `json:"current_plan_id"`
}

type RatePlanStore interface {
	RatePlans(ctx context.Context, owner, home string, now time.Time) (UtilityRatePlans, error)
	ChooseUtility(ctx context.Context, owner, home, eiaid, key string, hash []byte, now time.Time) (UtilityRatePlans, error)
	ChoosePlan(ctx context.Context, owner, home, planID, key string, hash []byte, now time.Time) (UtilityRatePlans, error)
}

type dbtx interface {
	Exec(context.Context, string, ...any) (pgconn.CommandTag, error)
	Query(context.Context, string, ...any) (pgx.Rows, error)
	QueryRow(context.Context, string, ...any) pgx.Row
}

type baseTier struct {
	ThroughKWh *json.Number `json:"through_kwh"`
	Per        string       `json:"per"`
	Rate       string       `json:"rate"`
}

type planRecord struct {
	UtilityRatePlan
	eiaid, key, kind, sourceURL, sha string
	version                          int
	tiers                            []baseTier
	fixedCharge, fixedPer            *string
	checkedAt                        time.Time
	current                          bool
}

const planSelect = `SELECT p.id::text,p.eiaid,p.plan_key,p.version,p.plan_name,p.variant,p.is_standard,p.base_tiers,p.base_label,
 p.fixed_charge::text,p.fixed_charge_per,to_char(p.effective_from,'YYYY-MM-DD'),p.source_kind,p.source_url,p.source_sha256,
 p.source_notes,p.retrieved_at,p.checked_at,p.superseded_at IS NULL FROM utility_rate_plans p`

func stamp(t time.Time) string { return t.UTC().Format(time.RFC3339) }

func clock(raw string) string {
	if strings.HasSuffix(raw, ":00") && len(raw) == 8 {
		return raw[:5]
	}
	return raw
}

func readPlans(ctx context.Context, q dbtx, where string, args ...any) ([]planRecord, error) {
	rows, err := q.Query(ctx, planSelect+" "+where, args...)
	if err != nil {
		return nil, err
	}
	plans := []planRecord{}
	for rows.Next() {
		var p planRecord
		var tiers, notes []byte
		var retrieved, checked time.Time
		if err = rows.Scan(&p.ID, &p.eiaid, &p.key, &p.version, &p.PlanName, &p.Variant, &p.IsStandard, &tiers, &p.BaseLabel,
			&p.fixedCharge, &p.fixedPer, &p.EffectiveFrom, &p.kind, &p.sourceURL, &p.sha, &notes, &retrieved, &checked, &p.current); err != nil {
			rows.Close()
			return nil, err
		}
		if tiers != nil {
			d := json.NewDecoder(bytes.NewReader(tiers))
			d.UseNumber()
			if err = d.Decode(&p.tiers); err != nil || len(p.tiers) == 0 {
				rows.Close()
				return nil, fmt.Errorf("invalid base tiers for plan %s", p.ID)
			}
		}
		p.Source = RatePlanSource{Kind: p.kind, URL: p.sourceURL, RetrievedAt: stamp(retrieved), CheckedAt: stamp(checked), Notes: []string{}}
		if err = json.Unmarshal(notes, &p.Source.Notes); err != nil {
			rows.Close()
			return nil, err
		}
		p.checkedAt = checked
		p.Periods = []RatePeriod{}
		plans = append(plans, p)
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		return nil, err
	}
	if len(plans) == 0 {
		return plans, nil
	}
	index := map[string]int{}
	ids := make([]string, len(plans))
	for i, p := range plans {
		index[p.ID], ids[i] = i, p.ID
	}
	periods, err := q.Query(ctx, `SELECT plan_id::text,name,adder::text,days,start_local::text,end_local::text,months
 FROM utility_rate_plan_periods WHERE plan_id=ANY($1::uuid[]) ORDER BY plan_id,ordinal`, ids)
	if err != nil {
		return nil, err
	}
	defer periods.Close()
	for periods.Next() {
		var id string
		var r RatePeriod
		if err = periods.Scan(&id, &r.Name, &r.Adder, &r.Days, &r.StartLocal, &r.EndLocal, &r.Months); err != nil {
			return nil, err
		}
		r.Adder = canonicalDecimal(r.Adder)
		r.StartLocal, r.EndLocal = clock(r.StartLocal), clock(r.EndLocal)
		p := &plans[index[id]]
		p.Periods = append(p.Periods, r)
		p.IsTOU = true
	}
	return plans, periods.Err()
}

func canonicalDecimal(raw string) string {
	v, ok := new(big.Rat).SetString(raw)
	if !ok {
		return raw
	}
	s := v.FloatString(8)
	s = strings.TrimRight(s, "0")
	return strings.TrimSuffix(s, ".")
}

func ownedHome(ctx context.Context, q dbtx, owner, home string, lock bool) error {
	sql := `SELECT id FROM homes WHERE id=$1 AND owner_user_id=$2 AND deleted_at IS NULL`
	if lock {
		sql += " FOR UPDATE"
	}
	var id string
	err := q.QueryRow(ctx, sql, home, owner).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	return err
}

func utilityName(ctx context.Context, q dbtx, eiaid string) (string, error) {
	var name *string
	err := q.QueryRow(ctx, `SELECT COALESCE((SELECT min(utility_name) FROM oedi_utility_zips WHERE eiaid=$1),
 (SELECT min(utility_name) FROM utility_rate_plans WHERE eiaid=$1))`, eiaid).Scan(&name)
	if err != nil || name == nil {
		return "", err
	}
	return *name, nil
}

// resolveUtility is the latest utility row while the home's ZIP is unchanged since it, else the ZIP's only OEDI utility.
func resolveUtility(ctx context.Context, q dbtx, home string, record bool) (*ElectricUtility, []UtilityCandidate, error) {
	candidates := []UtilityCandidate{}
	var country, postal string
	hasLocation := true
	err := q.QueryRow(ctx, `SELECT country_code,postal_code FROM home_location_revisions WHERE home_id=$1 ORDER BY revision DESC LIMIT 1`, home).Scan(&country, &postal)
	if errors.Is(err, pgx.ErrNoRows) {
		hasLocation = false
	} else if err != nil {
		return nil, nil, err
	}
	if m := zip5.FindStringSubmatch(strings.TrimSpace(postal)); hasLocation && country == "US" && m != nil {
		rows, err := q.Query(ctx, `SELECT eiaid,min(utility_name) FROM oedi_utility_zips WHERE zip=$1 GROUP BY eiaid ORDER BY eiaid`, m[1])
		if err != nil {
			return nil, nil, err
		}
		for rows.Next() {
			var c UtilityCandidate
			if err = rows.Scan(&c.EIAID, &c.UtilityName); err != nil {
				rows.Close()
				return nil, nil, err
			}
			candidates = append(candidates, c)
		}
		rows.Close()
		if err = rows.Err(); err != nil {
			return nil, nil, err
		}
	}
	var revision int64
	var eiaid, chosenBy string
	var sameLocation bool
	err = q.QueryRow(ctx, `SELECT u.revision,u.eiaid,u.chosen_by,
 COALESCE((SELECT l.country_code=$2 AND l.postal_code=$3 FROM home_location_revisions l
   WHERE l.home_id=u.home_id AND l.recorded_at<=u.recorded_at ORDER BY l.revision DESC LIMIT 1),$4)
 FROM home_electric_utility u WHERE u.home_id=$1 ORDER BY u.revision DESC LIMIT 1`, home, country, postal, !hasLocation).Scan(&revision, &eiaid, &chosenBy, &sameLocation)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return nil, nil, err
	}
	if err == nil && sameLocation {
		name, err := utilityName(ctx, q, eiaid)
		return &ElectricUtility{EIAID: eiaid, UtilityName: name, ChosenBy: chosenBy}, candidates, err
	}
	if len(candidates) != 1 {
		return nil, candidates, nil
	}
	if record {
		if _, err = q.Exec(ctx, `INSERT INTO home_electric_utility(home_id,revision,eiaid,chosen_by) VALUES($1,$2,$3,'zip_single')`, home, revision+1, candidates[0].EIAID); err != nil {
			return nil, nil, err
		}
	}
	return &ElectricUtility{EIAID: candidates[0].EIAID, UtilityName: candidates[0].UtilityName, ChosenBy: "zip_single"}, candidates, nil
}

type homeRevision struct {
	id, rate         string
	planID, chosenBy *string
	sourceAsOf       time.Time
	planKey          string
	planVersion      int
	planCurrent      bool
	planCheckedAt    time.Time
}

const revisionSelect = `SELECT r.id::text,r.rate::text,r.utility_rate_plan_id::text,r.plan_chosen_by,r.source_as_of,
 COALESCE(p.plan_key,''),COALESCE(p.version,0),COALESCE(p.superseded_at IS NULL,false),COALESCE(p.checked_at,r.source_as_of),r.rate_kind,r.plan_chosen_by
 FROM tariff_revisions r LEFT JOIN utility_rate_plans p ON p.id=r.utility_rate_plan_id`

func scanRevision(row pgx.Row) (*homeRevision, int, error) {
	var r homeRevision
	var kind string
	var chosen *string
	err := row.Scan(&r.id, &r.rate, &r.planID, &r.chosenBy, &r.sourceAsOf, &r.planKey, &r.planVersion, &r.planCurrent, &r.planCheckedAt, &kind, &chosen)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, 0, nil
	}
	rank := 2
	if kind == "user_tariff" || chosen != nil && *chosen == "user" {
		rank = 0
	} else if kind == "official_tariff" {
		rank = 1
	}
	return &r, rank, err
}

// currentRevision is the electricity revision the readers select at now, with its rank.
func currentRevision(ctx context.Context, q dbtx, home string, now time.Time) (*homeRevision, int, error) {
	return scanRevision(q.QueryRow(ctx, revisionSelect+` WHERE r.home_id=$1 AND r.fuel='electricity'
 AND r.effective_from<=$2 AND (r.effective_to IS NULL OR r.effective_to>$2)
 AND r.source_as_of<=$2 AND r.source_as_of>$2::timestamptz-`+ElectricityMaxAgeInterval+`
 ORDER BY CASE WHEN r.rate_kind='user_tariff' OR r.plan_chosen_by='user' THEN 0 WHEN r.rate_kind='official_tariff' THEN 1 ELSE 2 END,
 r.effective_from DESC,r.revision DESC,r.id DESC LIMIT 1`, home, now))
}

func latestRevision(ctx context.Context, q dbtx, home string) (*homeRevision, error) {
	r, _, err := scanRevision(q.QueryRow(ctx, revisionSelect+` WHERE r.home_id=$1 AND r.fuel='electricity' ORDER BY r.revision DESC LIMIT 1`, home))
	return r, err
}

func samePrices(a, b planRecord) bool {
	sig := func(p planRecord) string {
		var parts []string
		for _, t := range p.tiers {
			parts = append(parts, canonicalDecimal(t.Rate))
		}
		fixed := ""
		if p.fixedCharge != nil && p.fixedPer != nil {
			fixed = canonicalDecimal(*p.fixedCharge) + "/" + *p.fixedPer
		}
		parts = append(parts, fmt.Sprintf("tiers=%t fixed=%s", p.tiers != nil, fixed))
		for _, r := range p.Periods {
			parts = append(parts, fmt.Sprintf("%s %v %s-%s %v", r.Adder, r.Days, r.StartLocal, r.EndLocal, r.Months))
		}
		return strings.Join(parts, "|")
	}
	return sig(a) == sig(b)
}

// defaultPlan is the utility's standard plan when the data decides it: official plans win over URDB, and
// standard variants must differ at most in tier allowances; the first by variant and plan_key is used.
func defaultPlan(ctx context.Context, q dbtx, eiaid string, now time.Time) (*planRecord, error) {
	plans, err := readPlans(ctx, q, `WHERE p.eiaid=$1 AND p.superseded_at IS NULL AND p.replaced_by_plan_key IS NULL
 AND p.is_standard AND p.effective_from<=$2::date AND (p.effective_to IS NULL OR p.effective_to>$2::date)
 ORDER BY p.variant NULLS FIRST,p.plan_key`, eiaid, now)
	if err != nil || len(plans) == 0 {
		return nil, err
	}
	official := []planRecord{}
	for _, p := range plans {
		if p.kind == "official_tariff" {
			official = append(official, p)
		}
	}
	if len(official) > 0 {
		plans = official
	}
	for _, p := range plans[1:] {
		if !samePrices(plans[0], p) {
			return nil, nil
		}
	}
	return &plans[0], nil
}

func allowance(t baseTier) (*big.Rat, bool) {
	if t.ThroughKWh == nil {
		return nil, true
	}
	v, ok := new(big.Rat).SetString(t.ThroughKWh.String())
	if !ok {
		return nil, false
	}
	if t.Per == "day" {
		v.Mul(v, big.NewRat(30, 1))
	}
	return v, true
}

// measuredMonthKWh is the home's single root utility meter energy over the 30 days before now, when its history spans them.
func measuredMonthKWh(ctx context.Context, q dbtx, home string, now time.Time) (*big.Rat, error) {
	const meter = `FROM energy_hourly h JOIN sensor_endpoints s ON s.id=h.sensor_id
 WHERE h.home_id=$1 AND s.kind='utility_meter' AND s.parent_sensor_id IS NULL AND h.status='complete'`
	var meters int
	var wh *string
	var spans bool
	err := q.QueryRow(ctx, `SELECT count(DISTINCT h.sensor_id),sum(h.energy_wh)::text,
 EXISTS (SELECT 1 `+meter+` AND h.bucket_start<$2::timestamptz-interval '30 days')
 `+meter+` AND h.bucket_start>=$2::timestamptz-interval '30 days' AND h.bucket_start+interval '1 hour'<=$2`, home, now).Scan(&meters, &wh, &spans)
	if err != nil || meters != 1 || wh == nil || !spans {
		return nil, err
	}
	v, ok := new(big.Rat).SetString(*wh)
	if !ok {
		return nil, fmt.Errorf("invalid metered energy")
	}
	return v.Quo(v, big.NewRat(1000, 1)), nil
}

// baseRate picks the base per Q14 and names the rule used, for source_ref.
func baseRate(ctx context.Context, q dbtx, home string, plan planRecord, current *homeRevision, now time.Time) (string, string, error) {
	if plan.tiers == nil {
		if current == nil {
			return "", "", ErrNoBaseRate
		}
		return current.rate, "base:home_rate:" + current.id, nil
	}
	if len(plan.tiers) == 1 {
		return plan.tiers[0].Rate, "base:single_tier", nil
	}
	kwh, err := measuredMonthKWh(ctx, q, home, now)
	if err != nil {
		return "", "", err
	}
	if kwh != nil {
		for i, t := range plan.tiers {
			limit, ok := allowance(t)
			if !ok {
				return "", "", fmt.Errorf("invalid tier allowance")
			}
			if limit == nil || kwh.Cmp(limit) <= 0 {
				return t.Rate, fmt.Sprintf("base:measured_30d_kwh:%s:tier%d", kwh.FloatString(1), i+1), nil
			}
		}
	}
	var res *string
	if err = q.QueryRow(ctx, `SELECT COALESCE(
 (SELECT min(z.res_rate) FROM oedi_utility_zips z JOIN LATERAL (SELECT postal_code FROM home_location_revisions WHERE home_id=$1 ORDER BY revision DESC LIMIT 1) l ON left(l.postal_code,5)=z.zip WHERE z.eiaid=$2),
 (SELECT min(res_rate) FROM oedi_utility_zips WHERE eiaid=$2))::text`, home, plan.eiaid).Scan(&res); err != nil {
		return "", "", err
	}
	if res != nil {
		rate, ok := new(big.Rat).SetString(*res)
		if !ok {
			return "", "", fmt.Errorf("invalid OEDI res_rate")
		}
		chosen := 0
		for i, t := range plan.tiers {
			if v, ok := new(big.Rat).SetString(t.Rate); ok && v.Cmp(rate) <= 0 {
				chosen = i
			}
		}
		return plan.tiers[chosen].Rate, fmt.Sprintf("base:oedi_res_rate:%s:tier%d", canonicalDecimal(*res), chosen+1), nil
	}
	return plan.tiers[0].Rate, "base:no_usage_data:tier1", nil
}

func writePlanRevision(ctx context.Context, q dbtx, home string, plan planRecord, chosenBy string, current *homeRevision, now time.Time) error {
	rate, rule, err := baseRate(ctx, q, home, plan, current, now)
	if err != nil {
		return err
	}
	var fixed *string
	if plan.fixedPer != nil && *plan.fixedPer == "month" {
		fixed = plan.fixedCharge
	}
	ref := fmt.Sprintf("%s sha256:%s plan:%s@v%d %s", plan.sourceURL, plan.sha, plan.key, plan.version, rule)
	if len(ref) > 2048 {
		return errors.New("plan source reference too long")
	}
	var id string
	err = q.QueryRow(ctx, `INSERT INTO tariff_revisions(home_id,revision,fuel,rate,rate_unit,currency,rate_kind,source_ref,source_as_of,is_fallback,fixed_charge,effective_from,utility_rate_plan_id,plan_chosen_by)
 SELECT $1,COALESCE(MAX(revision),0)+1,'electricity',$2::numeric,'kWh','USD','official_tariff',$3,$4,false,$5::numeric,$6,$7,$8
 FROM tariff_revisions WHERE home_id=$1 AND fuel='electricity' RETURNING id::text`, home, rate, ref, plan.checkedAt, fixed, now, plan.ID, chosenBy).Scan(&id)
	if err != nil {
		return err
	}
	_, err = q.Exec(ctx, `INSERT INTO tariff_revision_periods(tariff_revision_id,ordinal,name,adder,days,start_local,end_local,months)
 SELECT $1,ordinal,name,adder,days,start_local,end_local,months FROM utility_rate_plan_periods WHERE plan_id=$2`, id, plan.ID)
	return err
}

// refreshPlanRevision gives a plan revision a new revision on a newer plan version, or on a newer check of a 30-day-old one.
func refreshPlanRevision(ctx context.Context, q dbtx, home string, r homeRevision, current *homeRevision, now time.Time) (bool, error) {
	if r.planID == nil || r.chosenBy == nil {
		return false, nil
	}
	newer, err := readPlans(ctx, q, `WHERE p.plan_key=$1 AND p.superseded_at IS NULL AND p.version>$2`, r.planKey, r.planVersion)
	if err != nil {
		return false, err
	}
	if current == nil {
		current = &r
	}
	if len(newer) == 1 {
		return true, writePlanRevision(ctx, q, home, newer[0], *r.chosenBy, current, now)
	}
	if !r.planCurrent || !r.planCheckedAt.After(r.sourceAsOf) || !r.sourceAsOf.Before(now.Add(-RefreshAge)) {
		return false, nil
	}
	same, err := readPlans(ctx, q, `WHERE p.id=$1`, *r.planID)
	if err != nil || len(same) != 1 {
		return false, err
	}
	return true, writePlanRevision(ctx, q, home, same[0], *r.chosenBy, current, now)
}

// applyRatePlans records a single-candidate utility, then applies the default plan or refreshes the plan revision.
func applyRatePlans(ctx context.Context, q dbtx, home string, now time.Time) (bool, error) {
	wrote, err := applyRatePlansOnce(ctx, q, home, now)
	if errors.Is(err, ErrNoBaseRate) {
		return false, nil
	}
	return wrote, err
}

func applyRatePlansOnce(ctx context.Context, q dbtx, home string, now time.Time) (bool, error) {
	utility, _, err := resolveUtility(ctx, q, home, true)
	if err != nil {
		return false, err
	}
	current, rank, err := currentRevision(ctx, q, home, now)
	if err != nil {
		return false, err
	}
	if current != nil && rank == 0 {
		return refreshPlanRevision(ctx, q, home, *current, current, now)
	}
	latest, err := latestRevision(ctx, q, home)
	if err != nil {
		return false, err
	}
	if latest != nil && latest.chosenBy != nil && *latest.chosenBy == "user" {
		if wrote, err := refreshPlanRevision(ctx, q, home, *latest, current, now); wrote || err != nil {
			return wrote, err
		}
	}
	var plan *planRecord
	if utility != nil {
		if plan, err = defaultPlan(ctx, q, utility.EIAID, now); err != nil {
			return false, err
		}
	}
	isDefault := latest != nil && latest.chosenBy != nil && *latest.chosenBy == "default"
	if plan != nil && !(isDefault && latest.planKey == plan.key) {
		err = writePlanRevision(ctx, q, home, *plan, "default", current, now)
		return err == nil, err
	}
	if isDefault {
		return refreshPlanRevision(ctx, q, home, *latest, current, now)
	}
	return false, nil
}

func ratePlansView(ctx context.Context, q dbtx, home string, now time.Time) (UtilityRatePlans, error) {
	out := UtilityRatePlans{Plans: []UtilityRatePlan{}}
	utility, candidates, err := resolveUtility(ctx, q, home, false)
	if err != nil {
		return out, err
	}
	out.UtilityCandidates, out.Utility = candidates, utility
	if utility != nil {
		plans, err := readPlans(ctx, q, `WHERE p.eiaid=$1 AND p.superseded_at IS NULL AND p.replaced_by_plan_key IS NULL
 ORDER BY (p.source_kind='official_tariff') DESC,p.is_standard DESC,p.plan_name,p.variant NULLS FIRST,p.plan_key`, utility.EIAID)
		if err != nil {
			return out, err
		}
		for _, p := range plans {
			out.Plans = append(out.Plans, p.UtilityRatePlan)
		}
	}
	current, _, err := currentRevision(ctx, q, home, now)
	if err != nil {
		return out, err
	}
	if current != nil {
		out.CurrentPlanID = current.planID
	}
	return out, nil
}

type PGRatePlanStore struct{ Pool *pgxpool.Pool }

func (s PGRatePlanStore) RatePlans(ctx context.Context, owner, home string, now time.Time) (UtilityRatePlans, error) {
	tx, err := s.Pool.BeginTx(ctx, pgx.TxOptions{AccessMode: pgx.ReadOnly})
	if err != nil {
		return UtilityRatePlans{}, err
	}
	defer tx.Rollback(ctx)
	if err = ownedHome(ctx, tx, owner, home, false); err != nil {
		return UtilityRatePlans{}, err
	}
	return ratePlansView(ctx, tx, home, now)
}

func (s PGRatePlanStore) choose(ctx context.Context, owner, home, key string, hash []byte, now time.Time, apply func(pgx.Tx) error) (UtilityRatePlans, error) {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return UtilityRatePlans{}, err
	}
	defer tx.Rollback(ctx)
	if err = ownedHome(ctx, tx, owner, home, true); err != nil {
		return UtilityRatePlans{}, err
	}
	var priorHash, priorBody []byte
	err = tx.QueryRow(ctx, `SELECT request_hash,response_json FROM tariff_idempotency WHERE owner_user_id=$1 AND home_id=$2 AND key=$3`, owner, home, key).Scan(&priorHash, &priorBody)
	if err == nil {
		if !bytes.Equal(priorHash, hash) {
			return UtilityRatePlans{}, ErrKeyReuse
		}
		var prior UtilityRatePlans
		return prior, json.Unmarshal(priorBody, &prior)
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return UtilityRatePlans{}, err
	}
	if err = apply(tx); err != nil {
		return UtilityRatePlans{}, err
	}
	view, err := ratePlansView(ctx, tx, home, now)
	if err != nil {
		return UtilityRatePlans{}, err
	}
	body, err := json.Marshal(view)
	if err != nil {
		return UtilityRatePlans{}, err
	}
	if _, err = tx.Exec(ctx, `INSERT INTO tariff_idempotency(owner_user_id,home_id,key,request_hash,response_json) VALUES($1,$2,$3,$4,$5)`, owner, home, key, hash, body); err != nil {
		return UtilityRatePlans{}, err
	}
	return view, tx.Commit(ctx)
}

func (s PGRatePlanStore) ChooseUtility(ctx context.Context, owner, home, eiaid, key string, hash []byte, now time.Time) (UtilityRatePlans, error) {
	return s.choose(ctx, owner, home, key, hash, now, func(tx pgx.Tx) error {
		var known bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM oedi_utility_zips WHERE eiaid=$1) OR EXISTS (SELECT 1 FROM utility_rate_plans WHERE eiaid=$1)`, eiaid).Scan(&known); err != nil {
			return err
		}
		if !known {
			return ErrUnknownUtility
		}
		if _, err := tx.Exec(ctx, `INSERT INTO home_electric_utility(home_id,revision,eiaid,chosen_by)
 SELECT $1,COALESCE(MAX(revision),0)+1,$2,'user' FROM home_electric_utility WHERE home_id=$1`, home, eiaid); err != nil {
			return err
		}
		_, err := applyRatePlans(ctx, tx, home, now)
		return err
	})
}

func (s PGRatePlanStore) ChoosePlan(ctx context.Context, owner, home, planID, key string, hash []byte, now time.Time) (UtilityRatePlans, error) {
	return s.choose(ctx, owner, home, key, hash, now, func(tx pgx.Tx) error {
		utility, _, err := resolveUtility(ctx, tx, home, true)
		if err != nil {
			return err
		}
		if utility == nil {
			return ErrPlanNotOffered
		}
		plans, err := readPlans(ctx, tx, `WHERE p.id=$1 AND p.eiaid=$2 AND p.superseded_at IS NULL AND p.replaced_by_plan_key IS NULL`, planID, utility.EIAID)
		if err != nil {
			return err
		}
		if len(plans) != 1 {
			return ErrPlanNotOffered
		}
		current, _, err := currentRevision(ctx, tx, home, now)
		if err != nil {
			return err
		}
		return writePlanRevision(ctx, tx, home, plans[0], "user", current, now)
	})
}

// RatePlanSweep applies default plans and plan refreshes; since limits a pass to homes whose location changed.
type RatePlanSweep struct{ Pool *pgxpool.Pool }

func (s RatePlanSweep) RunOnce(ctx context.Context, now time.Time, since *time.Time) (int, error) {
	sql := `SELECT id::text FROM homes WHERE deleted_at IS NULL ORDER BY id`
	args := []any{}
	if since != nil {
		sql = `SELECT DISTINCT h.id::text FROM homes h JOIN home_location_revisions l ON l.home_id=h.id
 WHERE h.deleted_at IS NULL AND l.recorded_at>=$1 ORDER BY 1`
		args = append(args, *since)
	}
	rows, err := s.Pool.Query(ctx, sql, args...)
	if err != nil {
		return 0, err
	}
	homes, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return 0, err
	}
	sort.Strings(homes)
	written := 0
	var failures error
	for _, home := range homes {
		wrote, err := s.sweepHome(ctx, home, now)
		if err != nil {
			failures = errors.Join(failures, fmt.Errorf("home %s: %w", home, err))
		} else if wrote {
			written++
		}
	}
	return written, failures
}

func (s RatePlanSweep) sweepHome(ctx context.Context, home string, now time.Time) (bool, error) {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback(ctx)
	var id string
	if err = tx.QueryRow(ctx, `SELECT id FROM homes WHERE id=$1 AND deleted_at IS NULL FOR UPDATE`, home).Scan(&id); errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	} else if err != nil {
		return false, err
	}
	wrote, err := applyRatePlans(ctx, tx, home, now)
	if err != nil {
		return false, err
	}
	return wrote, tx.Commit(ctx)
}

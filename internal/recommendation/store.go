package recommendation

import (
	"context"
	"encoding/json"
	"energylens.local/cloud/internal/datawizfacts"
	"energylens.local/cloud/internal/catalog"
	"energylens.local/cloud/internal/gasestimate"
	tariffpkg "energylens.local/cloud/internal/tariff"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

var ErrNotFound = errors.New("home not found")

type EvidenceStore interface {
	Current(context.Context, string, string, time.Time) ([]Appliance, error)
}

type PGStore struct{ Pool *pgxpool.Pool }

// Research reads current source guidance independently of appliance rule
// eligibility. The HTTP layer calls it only after Current authorizes the home.
func (s PGStore) Research(ctx context.Context) ([]datawizfacts.Fact, error) {
	return currentEfficiencyTips(ctx, s.Pool)
}

func currentEfficiencyTips(ctx context.Context, db datawizfacts.Querier) ([]datawizfacts.Fact, error) {
	tips := []datawizfacts.Fact{}
	for _, contextName := range []string{"night", "away", "peak_usage", "habits", "appliance_type", "upgrades", "maintenance"} {
		facts, err := datawizfacts.Current(ctx, db, "efficiency", "efficiency:"+contextName)
		if err != nil {
			return nil, err
		}
		for _, fact := range facts {
			if fact.Field == "efficiency_tip" {
				tips = append(tips, fact)
			}
		}
	}
	return tips, nil
}

type storedIdentity struct {
	Brand        string  `json:"brand"`
	FullModel    string  `json:"full_model"`
	Variant      *string `json:"variant"`
	Fuel         string  `json:"fuel"`
	MatchQuality string  `json:"match_quality"`
}

// Current reads every dependency from one database snapshot. A newer answer,
// appliance revision, catalog link, or tariff cannot be mixed with older rows.
func (s PGStore) Current(ctx context.Context, owner, home string, now time.Time) ([]Appliance, error) {
	tx, err := s.Pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	var id string
	err = tx.QueryRow(ctx, `SELECT id FROM homes WHERE id=$1 AND owner_user_id=$2 AND deleted_at IS NULL`, home, owner).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	ruleCategories := make([]string, 0, len(initial)+len(gasRules))
	seenCategory := map[string]bool{}
	for _, c := range ruleCategoryNames() {
		if !seenCategory[c] {
			seenCategory[c] = true
			ruleCategories = append(ruleCategories, c)
		}
	}
	ruleKeys := map[string]string{}
	ruleCounts := map[string]int{}
	ruleRows, err := tx.Query(ctx, `SELECT v.raw,COALESCE(c.key,a.category_key,v.raw) FROM unnest($1::text[]) v(raw) LEFT JOIN energy_categories c ON c.key=v.raw LEFT JOIN energy_category_aliases a ON a.alias=v.raw`, ruleCategories)
	if err != nil {
		return nil, err
	}
	for ruleRows.Next() {
		var raw, key string
		if err = ruleRows.Scan(&raw, &key); err != nil {
			break
		}
		ruleKeys[raw] = key
		ruleCounts[key]++
	}
	if err == nil {
		err = ruleRows.Err()
	}
	ruleRows.Close()
	if err != nil {
		return nil, err
	}
	var tariff *Evidence
	var tariffID string
	var tariffRevision int64
	var tariffRate string
	var electricRate *float64
	err = tx.QueryRow(ctx, `SELECT id,revision,rate::text FROM tariff_revisions WHERE home_id=$1 AND fuel='electricity'
	 AND rate_unit='kWh' AND currency='USD'
	 AND effective_from <= $2 AND (effective_to IS NULL OR effective_to > $2)
	 AND source_as_of <= $2 AND source_as_of > $2::timestamptz - `+tariffpkg.ElectricityMaxAgeInterval+`
	 ORDER BY is_fallback ASC,revision DESC LIMIT 1`, home, now).Scan(&tariffID, &tariffRevision, &tariffRate)
	if err == nil {
		tariff = &Evidence{ID: tariffID, Kind: "tariff", Revision: tariffRevision}
		if v, parseErr := strconv.ParseFloat(tariffRate, 64); parseErr == nil {
			electricRate = &v
		}
	}
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return nil, err
	}
	rows, err := tx.Query(ctx, `SELECT a.id,a.category,COALESCE(c.key,alias.category_key,a.category),a.identity,a.version,a.catalog_product_id,
	 cp.category,cp.fuel,cp.match_quality FROM appliances a
	 LEFT JOIN catalog_products cp ON cp.id=a.catalog_product_id
	 LEFT JOIN energy_categories c ON c.key=a.category
	 LEFT JOIN energy_category_aliases alias ON alias.alias=a.category
	 WHERE a.home_id=$1 AND a.archived_at IS NULL ORDER BY a.created_at,a.id LIMIT 101`, home)
	if err != nil {
		return nil, err
	}
	type current struct {
		Appliance
		hotWaterGPD     *float64
		householdUnsure bool
		Version   int64
		ProductID *string
		Details   storedIdentity
	}
	items := []current{}
	for rows.Next() {
		var item current
		var raw []byte
		var catalogCategory, catalogFuel, catalogQuality *string
		if err = rows.Scan(&item.ID, &item.Category, &item.categoryKey, &raw, &item.Version, &item.ProductID,
			&catalogCategory, &catalogFuel, &catalogQuality); err != nil {
			break
		}
		var identity storedIdentity
		if err = json.Unmarshal(raw, &identity); err != nil {
			break
		}
		item.Fuel, item.Identity = identity.Fuel, identity.MatchQuality
		item.Details = identity
		item.ruleCategoryKeys, item.ruleKeyCounts = ruleKeys, ruleCounts
		item.Tariff = tariff
		item.ElectricRate = electricRate
		item.Answers = map[string]Evidence{}
		// An inconsistent product link cannot satisfy an exact-source gate.
		if item.ProductID == nil || item.Identity != "confirmed_exact" || catalogCategory == nil ||
			catalogFuel == nil || catalogQuality == nil || *catalogCategory != item.Category ||
			*catalogFuel != item.Fuel || *catalogQuality != "confirmed_exact" {
			item.ProductID = nil
		}
		items = append(items, item)
	}
	if err == nil {
		err = rows.Err()
	}
	rows.Close()
	if err != nil {
		return nil, err
	}
	if len(items) > 100 {
		return nil, fmt.Errorf("too many active appliances")
	}
	researchTips, err := currentEfficiencyTips(ctx, tx)
	if err != nil {
		return nil, err
	}
	for i := range items {
		item := &items[i]
		item.ResearchTips = researchTips
		if item.Fuel == "electricity" && item.Identity == "confirmed_exact" &&
			(item.Category == "washer" || item.Category == "dryer" || item.Category == "dishwasher") &&
			item.Details.Brand != "" && item.Details.FullModel != "" && item.Details.Variant != nil && *item.Details.Variant != "" {
			// A usage publication is independent of the annual catalog link. Count
			// every row for this identity first so a second or incompatible cycle
			// fact cannot silently select a favorable source.
			var count int
			err = tx.QueryRow(ctx, `SELECT count(*) FROM active_reviewed_usage_facts
			 WHERE category=$1 AND manufacturer=$2 AND model=$3 AND fuel='electricity'
			 AND variant=$4 AND measurement_kind='cycle_energy'`, item.Category,
				item.Details.Brand, item.Details.FullModel, *item.Details.Variant).Scan(&count)
			if err != nil {
				return nil, err
			}
			if count == 1 {
				var factID string
				err = tx.QueryRow(ctx, `SELECT id FROM active_reviewed_usage_facts
				 WHERE category=$1 AND manufacturer=$2 AND model=$3 AND fuel='electricity'
				 AND variant=$4 AND measurement_kind='cycle_energy' AND unit='kWh/cycle'
				 AND value>0 AND match_quality='confirmed_exact'
				 AND documented_scope=$3 AND matched_model=$3
				 AND source_kind IN ('oem_pdf','epa_row')
				 AND nullif(btrim(test_basis),'') IS NOT NULL
				 AND nullif(btrim(cycle_definition),'') IS NOT NULL AND operation_mode IS NULL
				 AND included_energy->>'status'='documented'
				 AND included_energy->'components'='["appliance_electricity"]'::jsonb`,
					item.Category, item.Details.Brand, item.Details.FullModel, *item.Details.Variant).Scan(&factID)
				if err == nil {
					item.Facts = append(item.Facts, Evidence{ID: factID, Kind: "cycle_energy", Revision: 1})
				}
				if err != nil && !errors.Is(err, pgx.ErrNoRows) {
					return nil, err
				}
			}
		}
		if item.Category == "dryer" && item.Fuel == "natural_gas" && item.Identity == "confirmed_exact" &&
			item.Details.Brand != "" && item.Details.FullModel != "" && item.Details.Variant != nil && *item.Details.Variant != "" {
			var count int
			err = tx.QueryRow(ctx, `SELECT count(*) FROM active_reviewed_usage_facts
			 WHERE category='dryer' AND manufacturer=$1 AND model=$2 AND fuel='natural_gas'
			 AND variant=$3 AND measurement_kind='cycle_energy'`, item.Details.Brand,
				item.Details.FullModel, *item.Details.Variant).Scan(&count)
			if err != nil {
				return nil, err
			}
			if count == 1 {
				var factID string
				err = tx.QueryRow(ctx, `SELECT id FROM active_reviewed_usage_facts
				 WHERE category='dryer' AND manufacturer=$1 AND model=$2 AND fuel='natural_gas'
				 AND variant=$3 AND measurement_kind='cycle_energy' AND unit='therm/cycle'
				 AND match_quality='confirmed_exact' AND documented_scope=$2 AND matched_model=$2
				 AND source_kind='oem_pdf' AND included_energy->>'status'='documented'
				 AND included_energy->'components'='["appliance_gas"]'::jsonb`, item.Details.Brand,
					item.Details.FullModel, *item.Details.Variant).Scan(&factID)
				if err == nil {
					item.GasUsage = &Evidence{ID: factID, Kind: "gas_cycle_energy", Revision: 1}
				}
				if err != nil && !errors.Is(err, pgx.ErrNoRows) {
					return nil, err
				}
			}
			var rateID string
			var rateRevision int64
			err = tx.QueryRow(ctx, `SELECT id,revision FROM tariff_revisions
			 WHERE home_id=$1 AND fuel='natural_gas' AND rate_unit='therm' AND currency='USD'
			 AND effective_from<=$2 AND (effective_to IS NULL OR effective_to>$2)
			 AND source_as_of<=$2 AND source_as_of>=$2::timestamptz - interval '18 months'
			 ORDER BY `+tariffpkg.RankSQL+`,
			 effective_from DESC,revision DESC,id DESC LIMIT 1`, home, now).Scan(&rateID, &rateRevision)
			if err == nil {
				item.GasTariff = &Evidence{ID: rateID, Kind: "gas_tariff", Revision: rateRevision}
			}
			if err != nil && !errors.Is(err, pgx.ErrNoRows) {
				return nil, err
			}
		}
		if item.ProductID != nil {
			factRows, queryErr := tx.Query(ctx, `SELECT DISTINCT ON (kind) id,kind,revision FROM active_catalog_facts
			 WHERE product_id=$1 ORDER BY kind,reviewed_at DESC,revision DESC,id DESC`, *item.ProductID)
			if queryErr != nil {
				return nil, queryErr
			}
			for factRows.Next() {
				var fact Evidence
				if queryErr = factRows.Scan(&fact.ID, &fact.Kind, &fact.Revision); queryErr != nil {
					break
				}
				item.Facts = append(item.Facts, fact)
			}
			if queryErr == nil {
				queryErr = factRows.Err()
			}
			factRows.Close()
			if queryErr != nil {
				return nil, queryErr
			}
		}
		answerRows, queryErr := tx.Query(ctx, `SELECT DISTINCT ON (question_id) id,question_id,question_version,revision,status,answer_value
		 FROM question_answers WHERE home_id=$1 AND appliance_id=$2 AND appliance_version=$3
		 ORDER BY question_id,revision DESC`, home, item.ID, item.Version)
		if queryErr != nil {
			return nil, queryErr
		}
		for answerRows.Next() {
			var answerID, questionID, status string
			var answerValue []byte
			var questionVersion, revision int64
			if queryErr = answerRows.Scan(&answerID, &questionID, &questionVersion, &revision, &status, &answerValue); queryErr != nil {
				break
			}
			if status == "unknown" && questionID == item.Category+".household_size" {
				item.householdUnsure = true
			}
			if status != "confirmed" || (questionVersion != 1 && questionVersion != 2) {
				continue
			}
			if questionID == item.Category+".household_size" {
				var gallons float64
				if json.Unmarshal(answerValue, &gallons) == nil && gallons > 0 && gallons <= 500 {
					item.Answers["household_size"] = Evidence{ID: answerID, Kind: "confirmed_answer", Revision: revision}
					item.hotWaterGPD = &gallons
				}
			}
			for _, kind := range []string{"hours_per_week", "cycles_per_week"} {
				if questionID == item.Category+"."+kind {
					var frequency float64
					if json.Unmarshal(answerValue, &frequency) != nil || frequency < 0 ||
						(kind == "hours_per_week" && frequency > 168) || (kind == "cycles_per_week" && frequency > 100) {
						continue
					}
					item.Answers[kind] = Evidence{ID: answerID, Kind: "confirmed_answer", Revision: revision}
					if kind == "hours_per_week" {
						item.HoursPerWeek = &frequency
					} else {
						item.CyclesPerWeek = &frequency
					}
				}
			}
		}
		if queryErr == nil {
			queryErr = answerRows.Err()
		}
		answerRows.Close()
		if queryErr != nil {
			return nil, queryErr
		}
		if err = loadGasValues(ctx, tx, &item.Appliance, item.ProductID, item.hotWaterGPD, item.householdUnsure, home, now); err != nil {
			return nil, err
		}
		if item.Identity == "confirmed_exact" && item.Fuel == "electricity" && item.Tariff != nil &&
			item.Details.Brand != "" && item.Details.FullModel != "" && item.Details.Variant != nil && *item.Details.Variant != "" {
			// A scenario must match the current reviewed baseline, exact device,
			// confirmed usage and tariff in this transaction. Ambiguous revisions
			// or overlapping alternatives suppress the amount.
			scenarioRows, scenarioErr := tx.Query(ctx, `SELECT s.id::text,s.revision,s.rule_id,
			 f.id::text,f.value::text,s.alternative_kwh::text,s.usage_unit,s.exclusion_group,s.source_url
			 FROM active_reviewed_action_benefit_scenarios s
			 JOIN active_reviewed_usage_facts f ON f.id=s.baseline_fact_id
			 WHERE s.category=$1 AND s.manufacturer=$2 AND s.model=$3 AND s.variant=$4
			 AND s.fuel='electricity' AND f.category=s.category AND f.manufacturer=s.manufacturer
			 AND f.model=s.model AND f.variant=s.variant AND f.fuel=s.fuel
			 AND f.match_quality='confirmed_exact' AND f.documented_scope=f.model AND f.matched_model=f.model
			 AND f.value>=s.alternative_kwh AND f.source_kind IN ('oem_pdf','epa_row')
			 AND f.included_energy->>'status'='documented'
			 AND f.included_energy->'components'='["appliance_electricity"]'::jsonb
			 AND ((s.usage_unit='kWh/use' AND f.measurement_kind='cycle_energy' AND f.unit='kWh/cycle'
			       AND f.cycle_definition=s.service_definition AND f.operation_mode IS NULL)
			   OR (s.usage_unit='kWh/hour' AND f.measurement_kind='hourly_energy' AND f.unit='kWh/hour'
			       AND f.operation_mode=s.service_definition AND f.cycle_definition IS NULL))`,
				item.Category, item.Details.Brand, item.Details.FullModel, *item.Details.Variant)
			if scenarioErr != nil {
				return nil, scenarioErr
			}
			type candidate struct {
				scenario BenefitScenario
				group    string
			}
			candidates := []candidate{}
			for scenarioRows.Next() {
				var c candidate
				var scenarioID, baselineID, unit string
				var revision int64
				if scenarioErr = scenarioRows.Scan(&scenarioID, &revision, &c.scenario.RuleID,
					&baselineID, &c.scenario.BaselineEnergy, &c.scenario.AlternativeEnergy, &unit, &c.group, &c.scenario.SourceURL); scenarioErr != nil {
					break
				}
				c.scenario.ApplianceID = item.ID
				c.scenario.EnergyUnit = unit
				c.scenario.RateUSDPerKWh = tariffRate
				c.scenario.BaselineRef = Evidence{ID: baselineID, Kind: "reviewed_usage_fact", Revision: 1}
				c.scenario.AlternativeRef = Evidence{ID: scenarioID, Kind: "reviewed_action_scenario", Revision: revision}
				c.scenario.TariffRef = *item.Tariff
				usageKind := "cycles_per_week"
				if unit == "kWh/hour" {
					usageKind = "hours_per_week"
				}
				if ref, ok := item.Answers[usageKind]; ok {
					c.scenario.UsageRef = ref
					c.scenario.UsagePerWeek = item.frequencyFor(usageKind)
					candidates = append(candidates, c)
				}
			}
			if scenarioErr == nil {
				scenarioErr = scenarioRows.Err()
			}
			scenarioRows.Close()
			if scenarioErr != nil {
				return nil, scenarioErr
			}
			ruleCount, groupCount := map[string]int{}, map[string]int{}
			for _, c := range candidates {
				ruleCount[c.scenario.RuleID]++
				groupCount[c.group]++
			}
			for _, c := range candidates {
				if ruleCount[c.scenario.RuleID] == 1 && groupCount[c.group] == 1 {
					item.Scenarios = append(item.Scenarios, c.scenario)
				}
			}
		}
	}
	if err = tx.Commit(ctx); err != nil {
		return nil, err
	}
	out := make([]Appliance, 0, len(items))
	for _, item := range items {
		out = append(out, item.Appliance)
	}
	return out, nil
}

func ruleCategoryNames() []string {
	out := make([]string, 0, len(initial)+len(gasRules))
	for _, r := range initial {
		out = append(out, r.category)
	}
	for _, r := range gasRules {
		out = append(out, r.category)
	}
	return out
}

// loadGasValues reads catalog gas facts, estimates and the gas rate for an
// exact gas appliance covered by gasRules. Propane gets no gas rate (D-2e).
func loadGasValues(ctx context.Context, tx pgx.Tx, a *Appliance, productID *string, gallons *float64, unsure bool, home string, now time.Time) error {
	covered := false
	for _, r := range gasRules {
		covered = covered || a.matchesRuleCategory(r.category)
	}
	if !covered || a.Identity != "confirmed_exact" || (a.Fuel != "natural_gas" && a.Fuel != "propane") {
		return nil
	}
	var facts catalog.GasFacts
	if productID != nil {
		var err error
		if facts, err = catalog.ReadGasFacts(ctx, tx, *productID); err != nil && !errors.Is(err, catalog.ErrNotFound) {
			return err
		}
	}
	class, tankless := gasestimate.ClassOf(a.Category)
	est, ok := gasestimate.Compute(gasestimate.Input{Class: class, Tankless: tankless, Fuel: a.Fuel, Facts: facts,
		Usage: gasestimate.Usage{HoursPerWeek: a.HoursPerWeek, HotWaterGalPerDay: gallons}})
	g := &GasValues{AFUE: facts.AFUE, HouseholdNotSure: unsure,
		Tankless: tankless || strings.Contains(strings.ToLower(facts.ProductGroup), "instantaneous")}
	if facts.UEF != nil {
		uef := facts.UEF.Value
		g.UEF = &uef
		g.DrawPattern, _ = facts.UEF.TestBasis["draw_pattern"].(string)
	}
	if ok {
		g.StandbyKWh, g.StandbyRated, g.Therms = est.StandbyKWh, est.StandbyBasis == gasestimate.BasisRatedFact, est.GasTherms
	}
	a.Gas = g
	if a.Fuel != "natural_gas" {
		return nil
	}
	var rateID string
	var rateRevision int64
	var rate float64
	err := tx.QueryRow(ctx, `SELECT id,revision,rate::float8 FROM tariff_revisions
	 WHERE home_id=$1 AND fuel='natural_gas' AND rate_unit='therm' AND currency='USD'
	 AND effective_from<=$2 AND (effective_to IS NULL OR effective_to>$2)
	 AND source_as_of<=$2 AND source_as_of>=$2::timestamptz - interval '18 months'
	 ORDER BY `+tariffpkg.RankSQL+`,
	 effective_from DESC,revision DESC,id DESC LIMIT 1`, home, now).Scan(&rateID, &rateRevision, &rate)
	if err == nil {
		a.GasTariff, a.GasRate = &Evidence{ID: rateID, Kind: "gas_tariff", Revision: rateRevision}, &rate
	}
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	return nil
}

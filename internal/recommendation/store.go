package recommendation

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

var ErrNotFound = errors.New("home not found")

type EvidenceStore interface {
	Current(context.Context, string, string, time.Time) ([]Appliance, error)
}

type PGStore struct{ Pool *pgxpool.Pool }

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
	var tariff *Evidence
	var tariffID string
	var tariffRevision int64
	err = tx.QueryRow(ctx, `SELECT id,revision FROM tariff_revisions WHERE home_id=$1 AND fuel='electricity'
	 AND effective_from <= $2 AND (effective_to IS NULL OR effective_to > $2)
	 ORDER BY is_fallback ASC,revision DESC LIMIT 1`, home, now).Scan(&tariffID, &tariffRevision)
	if err == nil {
		tariff = &Evidence{ID: tariffID, Kind: "tariff", Revision: tariffRevision}
	}
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return nil, err
	}
	rows, err := tx.Query(ctx, `SELECT a.id,a.category,a.identity,a.version,a.catalog_product_id,
	 cp.category,cp.fuel,cp.match_quality FROM appliances a
	 LEFT JOIN catalog_products cp ON cp.id=a.catalog_product_id
	 WHERE a.home_id=$1 AND a.archived_at IS NULL ORDER BY a.created_at,a.id LIMIT 101`, home)
	if err != nil {
		return nil, err
	}
	type current struct {
		Appliance
		Version   int64
		ProductID *string
		Details   storedIdentity
	}
	items := []current{}
	for rows.Next() {
		var item current
		var raw []byte
		var catalogCategory, catalogFuel, catalogQuality *string
		if err = rows.Scan(&item.ID, &item.Category, &raw, &item.Version, &item.ProductID,
			&catalogCategory, &catalogFuel, &catalogQuality); err != nil {
			break
		}
		var identity storedIdentity
		if err = json.Unmarshal(raw, &identity); err != nil {
			break
		}
		item.Fuel, item.Identity = identity.Fuel, identity.MatchQuality
		item.Details = identity
		item.Tariff = tariff
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
	for i := range items {
		item := &items[i]
		if item.Category == "dryer" && item.Fuel == "natural_gas" && item.Identity == "confirmed_exact" &&
			item.Details.Brand != "" && item.Details.FullModel != "" && item.Details.Variant != nil && *item.Details.Variant != "" {
			var count int
			err = tx.QueryRow(ctx, `SELECT count(*) FROM reviewed_usage_facts
			 WHERE category='dryer' AND manufacturer=$1 AND model=$2 AND fuel='natural_gas'
			 AND variant=$3 AND measurement_kind='cycle_energy'`, item.Details.Brand,
				item.Details.FullModel, *item.Details.Variant).Scan(&count)
			if err != nil {
				return nil, err
			}
			if count == 1 {
				var factID string
				err = tx.QueryRow(ctx, `SELECT id FROM reviewed_usage_facts
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
			 ORDER BY CASE rate_kind WHEN 'user_tariff' THEN 0 WHEN 'official_tariff' THEN 1 ELSE 2 END,
			 effective_from DESC,revision DESC,id DESC LIMIT 1`, home, now).Scan(&rateID, &rateRevision)
			if err == nil {
				item.GasTariff = &Evidence{ID: rateID, Kind: "gas_tariff", Revision: rateRevision}
			}
			if err != nil && !errors.Is(err, pgx.ErrNoRows) {
				return nil, err
			}
		}
		if item.ProductID != nil {
			factRows, queryErr := tx.Query(ctx, `SELECT DISTINCT ON (kind) id,kind,revision FROM catalog_fact_revisions
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
			if status != "confirmed" || questionVersion != 1 {
				continue
			}
			if item.Category == "dryer" && item.Fuel == "natural_gas" && questionID == "dryer.cycles_per_week" {
				var cycles int64
				if json.Unmarshal(answerValue, &cycles) != nil || cycles < 0 || cycles > 100 {
					continue
				}
			}
			for _, kind := range []string{"hours_per_week", "cycles_per_week"} {
				if questionID == item.Category+"."+kind {
					item.Answers[kind] = Evidence{ID: answerID, Kind: "confirmed_answer", Revision: revision}
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

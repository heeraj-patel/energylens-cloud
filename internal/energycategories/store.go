package energycategories

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type Type struct {
	Key   string `json:"key"`
	Label string `json:"label"`
}
type Group struct {
	Key   string `json:"key"`
	Label string `json:"label"`
	Types []Type `json:"types"`
}
type List struct {
	Version int64   `json:"version"`
	Groups  []Group `json:"groups"`
}
type Store interface {
	List(context.Context) (List, error)
	Export(context.Context) (json.RawMessage, int64, error)
}
type PGStore struct{ Pool *pgxpool.Pool }

func Ensure(ctx context.Context, tx pgx.Tx, raw string) (string, error) {
	var key string
	err := tx.QueryRow(ctx, `SELECT energy_category_ensure($1)`, raw).Scan(&key)
	return key, err
}

func (s PGStore) List(ctx context.Context) (List, error) {
	var out List
	out.Groups = []Group{}
	tx, err := s.Pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return out, err
	}
	defer tx.Rollback(ctx)
	if err = tx.QueryRow(ctx, `SELECT version FROM energy_category_catalog WHERE id`).Scan(&out.Version); err != nil {
		return out, err
	}
	rows, err := tx.Query(ctx, `SELECT g.key,g.label,c.key,c.label FROM energy_category_groups g LEFT JOIN energy_categories c ON c.group_key=g.key ORDER BY g.sort_order,g.key,c.sort_order,c.key`)
	if err != nil {
		return out, err
	}
	for rows.Next() {
		var groupKey, groupLabel string
		var typeKey, typeLabel *string
		if err = rows.Scan(&groupKey, &groupLabel, &typeKey, &typeLabel); err != nil {
			break
		}
		if len(out.Groups) == 0 || out.Groups[len(out.Groups)-1].Key != groupKey {
			out.Groups = append(out.Groups, Group{groupKey, groupLabel, []Type{}})
		}
		if typeKey != nil && typeLabel != nil {
			g := &out.Groups[len(out.Groups)-1]
			g.Types = append(g.Types, Type{*typeKey, *typeLabel})
		}
	}
	if err == nil {
		err = rows.Err()
	}
	rows.Close()
	if err != nil {
		return out, err
	}
	return out, tx.Commit(ctx)
}

func (s PGStore) Export(ctx context.Context) (json.RawMessage, int64, error) {
	tx, err := s.Pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return nil, 0, err
	}
	defer tx.Rollback(ctx)
	var version int64
	var meta []byte
	if err = tx.QueryRow(ctx, `SELECT version,meta FROM energy_category_catalog WHERE id`).Scan(&version, &meta); err != nil {
		return nil, 0, err
	}
	var doc map[string]any
	if err = json.Unmarshal(meta, &doc); err != nil {
		return nil, 0, err
	}
	if seed, ok := doc["seed_version"]; ok {
		doc["version"] = seed
		delete(doc, "seed_version")
	}
	doc["catalog_version"] = version
	groups := []map[string]any{}
	rows, err := tx.Query(ctx, `SELECT key,label,sort_order FROM energy_category_groups ORDER BY sort_order,key`)
	if err != nil {
		return nil, 0, err
	}
	for rows.Next() {
		var k, l string
		var order int
		if err = rows.Scan(&k, &l, &order); err != nil {
			break
		}
		groups = append(groups, map[string]any{"key": k, "label": l, "sort_order": order})
	}
	if err == nil {
		err = rows.Err()
	}
	rows.Close()
	if err != nil {
		return nil, 0, err
	}
	doc["groups"] = groups
	categories := map[string]any{}
	rows, err = tx.Query(ctx, `SELECT key,label,group_key,sort_order,usage_kind,default_usage::float8,standby_applies,typical_active_watts::float8,typical_standby_watts::float8,typical_kwh_per_cycle::float8,typical_cycle_hours::float8,demo_active_watts::float8,demo_hours_per_day::float8,profile FROM energy_categories ORDER BY key`)
	if err != nil {
		return nil, 0, err
	}
	for rows.Next() {
		var key, label, group, usage string
		var order int
		var standby bool
		var def, active, standbyW, cycle, cycleH *float64
		var demoW, demoH float64
		var profile []byte
		if err = rows.Scan(&key, &label, &group, &order, &usage, &def, &standby, &active, &standbyW, &cycle, &cycleH, &demoW, &demoH, &profile); err != nil {
			break
		}
		var item map[string]any
		if err = json.Unmarshal(profile, &item); err != nil {
			break
		}
		item["label"] = label
		item["group"] = group
		item["sort_order"] = order
		item["usage_kind"] = usage
		item["standby_applies"] = standby
		item["typical_active_watts"] = active
		item["typical_standby_watts"] = standbyW
		item["typical_kwh_per_cycle"] = cycle
		item["typical_cycle_hours"] = cycleH
		item["demo_active_watts"] = demoW
		item["demo_hours_per_day"] = demoH
		if slider, ok := item["slider"].(map[string]any); ok && def != nil {
			slider["default"] = *def
		}
		categories[key] = item
	}
	if err == nil {
		err = rows.Err()
	}
	rows.Close()
	if err != nil {
		return nil, 0, err
	}
	doc["categories"] = categories
	aliases := map[string]string{}
	rows, err = tx.Query(ctx, `SELECT alias,category_key FROM energy_category_aliases ORDER BY alias`)
	if err != nil {
		return nil, 0, err
	}
	for rows.Next() {
		var alias, key string
		if err = rows.Scan(&alias, &key); err != nil {
			break
		}
		aliases[alias] = key
	}
	if err == nil {
		err = rows.Err()
	}
	rows.Close()
	if err != nil {
		return nil, 0, err
	}
	doc["aliases"] = aliases
	if err = tx.Commit(ctx); err != nil {
		return nil, 0, err
	}
	data, err := json.Marshal(doc)
	if err != nil {
		return nil, 0, fmt.Errorf("encode category export: %w", err)
	}
	return data, version, nil
}

package datawizresolution

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"

	"energylens.local/cloud/internal/datawizfacts"
	"energylens.local/cloud/internal/householdenergy"
	"energylens.local/cloud/internal/inventory"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

var ErrNotFound = errors.New("energy resolution not found")

type Store interface {
	ForOwner(context.Context, string, string, string) (json.RawMessage, error)
	ForHub(context.Context, string, string) (json.RawMessage, error)
	Refresh(ctx context.Context, owner, home, appliance, key string) (RefreshResult, error)
}
type PGStore struct{ Pool *pgxpool.Pool }

func (s PGStore) ForOwner(ctx context.Context, owner, home, appliance string) (json.RawMessage, error) {
	tx, err := s.Pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	var raw []byte
	err = tx.QueryRow(ctx, `SELECT demo_cost_snapshot FROM homes WHERE id=$1 AND owner_user_id=$2 AND deleted_at IS NULL`, home, owner).Scan(&raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	version, found, err := liveOrDemo(ctx, tx, home, appliance, raw)
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, ErrNotFound
	}
	body, err := resolution(ctx, tx, appliance, version)
	if err != nil {
		return nil, err
	}
	return body, tx.Commit(ctx)
}
func (s PGStore) ForHub(ctx context.Context, hub, appliance string) (json.RawMessage, error) {
	tx, err := s.Pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	var home string
	var raw []byte
	err = tx.QueryRow(ctx, `SELECT h.id::text,h.demo_cost_snapshot FROM hubs b JOIN homes h ON h.id=b.home_id WHERE b.id=$1 AND h.deleted_at IS NULL`, hub).Scan(&home, &raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	version, found, err := liveOrDemo(ctx, tx, home, appliance, raw)
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, ErrNotFound
	}
	body, err := resolution(ctx, tx, appliance, version)
	if err != nil {
		return nil, err
	}
	return body, tx.Commit(ctx)
}
func liveOrDemo(ctx context.Context, db pgx.Tx, home, appliance string, raw []byte) (int64, bool, error) {
	var version int64
	if err := db.QueryRow(ctx, `SELECT version FROM appliances WHERE id=$1 AND home_id=$2 AND archived_at IS NULL`, appliance, home).Scan(&version); err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return 0, false, err
	}
	if version > 0 {
		return version, true, nil
	}
	if len(raw) == 0 {
		return 0, false, nil
	}
	snapshot, err := householdenergy.DecodeDemoCostSnapshot(bytes.NewReader(raw))
	if err != nil {
		return 0, false, err
	}
	rows, err := inventory.DemoAppliances(snapshot, home)
	if err != nil {
		return 0, false, err
	}
	for _, a := range rows {
		if a.ID == appliance {
			return a.Version, true, nil
		}
	}
	return 0, false, nil
}
func resolution(ctx context.Context, db pgx.Tx, appliance string, version int64) (json.RawMessage, error) {
	data, _, err := datawizfacts.Resolution(ctx, db, "cloud:"+appliance, version)
	if errors.Is(err, datawizfacts.ErrNoResolution) {
		return nil, ErrNotFound
	}
	return data, err
}

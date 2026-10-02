package sensors

import (
	"bytes"
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

var ErrNotFound = errors.New("home, hub, or sensor not found")
var ErrConflict = errors.New("sensor registration conflict")
var ErrKeyReuse = errors.New("idempotency key reused")
var ErrDenied = errors.New("hub certificate denied")

type Sensor struct {
	ID             string   `json:"id"`
	HomeID         string   `json:"home_id"`
	HubID          string   `json:"hub_id"`
	OwnershipEpoch int64    `json:"ownership_epoch"`
	LocalSensorID  string   `json:"local_sensor_id"`
	ParentSensorID *string  `json:"parent_sensor_id,omitempty"`
	Kind           string   `json:"kind"`
	Capabilities   []string `json:"capabilities"`
	Version        int64    `json:"version"`
}
type CreateInput struct {
	HubID          string   `json:"hub_id"`
	OwnershipEpoch int64    `json:"ownership_epoch"`
	LocalSensorID  string   `json:"local_sensor_id"`
	ParentSensorID *string  `json:"parent_sensor_id,omitempty"`
	Kind           string   `json:"kind"`
	Capabilities   []string `json:"capabilities"`
}
type HubMapping struct {
	HubID          string   `json:"hub_id"`
	HomeID         string   `json:"home_id"`
	OwnershipEpoch int64    `json:"ownership_epoch"`
	Sensors        []Sensor `json:"sensors"`
}
type Store struct{ Pool *pgxpool.Pool }

const selectSensor = `SELECT id,home_id,hub_id,ownership_epoch,local_sensor_id,parent_sensor_id,kind,capabilities,version FROM sensor_endpoints`

func scanSensor(row pgx.Row) (Sensor, error) {
	var v Sensor
	err := row.Scan(&v.ID, &v.HomeID, &v.HubID, &v.OwnershipEpoch, &v.LocalSensorID, &v.ParentSensorID, &v.Kind, &v.Capabilities, &v.Version)
	return v, err
}
func (s Store) Create(ctx context.Context, owner, home string, in CreateInput, key string, hash []byte) (Sensor, error) {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return Sensor{}, err
	}
	defer tx.Rollback(ctx)
	// Lock the hub against claim/unclaim while checking the current home and epoch.
	var current int64
	err = tx.QueryRow(ctx, `SELECT b.ownership_epoch FROM hubs b JOIN homes h ON h.id=b.home_id AND h.id=$2 AND h.owner_user_id=$3 AND h.deleted_at IS NULL WHERE b.id=$1 AND b.revoked_at IS NULL FOR UPDATE OF b`, in.HubID, home, owner).Scan(&current)
	if errors.Is(err, pgx.ErrNoRows) {
		return Sensor{}, ErrNotFound
	}
	if err != nil {
		return Sensor{}, err
	}
	if current != in.OwnershipEpoch {
		return Sensor{}, ErrConflict
	}
	var oldHash []byte
	var oldID string
	err = tx.QueryRow(ctx, `SELECT request_hash,sensor_id FROM sensor_request_idempotency WHERE owner_user_id=$1 AND home_id=$2 AND key=$3`, owner, home, key).Scan(&oldHash, &oldID)
	if err == nil {
		if !bytes.Equal(oldHash, hash) {
			return Sensor{}, ErrKeyReuse
		}
		v, e := scanSensor(tx.QueryRow(ctx, selectSensor+` WHERE id=$1`, oldID))
		if e != nil {
			return Sensor{}, e
		}
		return v, tx.Commit(ctx)
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return Sensor{}, err
	}
	// A parent must belong to this same active hub and epoch.
	if in.ParentSensorID != nil {
		var exists bool
		err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM sensor_endpoints WHERE id=$1 AND home_id=$2 AND hub_id=$3 AND ownership_epoch=$4 AND archived_at IS NULL)`, *in.ParentSensorID, home, in.HubID, current).Scan(&exists)
		if err != nil {
			return Sensor{}, err
		}
		if !exists {
			return Sensor{}, ErrNotFound
		}
	}
	var exists bool
	err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM sensor_endpoints WHERE hub_id=$1 AND ownership_epoch=$2 AND local_sensor_id=$3)`, in.HubID, current, in.LocalSensorID).Scan(&exists)
	if err != nil {
		return Sensor{}, err
	}
	if exists {
		return Sensor{}, ErrConflict
	}
	v, err := scanSensor(tx.QueryRow(ctx, `INSERT INTO sensor_endpoints(home_id,hub_id,ownership_epoch,local_sensor_id,parent_sensor_id,kind,capabilities) VALUES($1,$2,$3,$4,$5,$6,$7) RETURNING id,home_id,hub_id,ownership_epoch,local_sensor_id,parent_sensor_id,kind,capabilities,version`, home, in.HubID, current, in.LocalSensorID, in.ParentSensorID, in.Kind, in.Capabilities))
	if err != nil {
		return Sensor{}, err
	}
	_, err = tx.Exec(ctx, `INSERT INTO sensor_request_idempotency(owner_user_id,home_id,key,request_hash,sensor_id) VALUES($1,$2,$3,$4,$5)`, owner, home, key, hash, v.ID)
	if err != nil {
		return Sensor{}, err
	}
	return v, tx.Commit(ctx)
}
func (s Store) ListOwner(ctx context.Context, owner, home string) ([]Sensor, error) {
	rows, err := s.Pool.Query(ctx, `SELECT b.id,b.home_id,b.hub_id,b.ownership_epoch,b.local_sensor_id,b.parent_sensor_id,b.kind,b.capabilities,b.version FROM sensor_endpoints b JOIN hubs h ON h.id=b.hub_id AND h.home_id=b.home_id AND h.ownership_epoch=b.ownership_epoch AND h.revoked_at IS NULL JOIN homes o ON o.id=b.home_id AND o.owner_user_id=$1 AND o.deleted_at IS NULL WHERE b.home_id=$2 AND b.archived_at IS NULL ORDER BY b.created_at,b.id`, owner, home)
	if err != nil {
		return nil, err
	}
	out := []Sensor{}
	for rows.Next() {
		v, e := scanSensor(rows)
		if e != nil {
			rows.Close()
			return nil, e
		}
		out = append(out, v)
	}
	if err = rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()
	var owned bool
	err = s.Pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM homes WHERE id=$1 AND owner_user_id=$2 AND deleted_at IS NULL)`, home, owner).Scan(&owned)
	if err != nil {
		return nil, err
	}
	if !owned {
		return nil, ErrNotFound
	}
	return out, nil
}
func (s Store) ListHub(ctx context.Context, hub, serial string, peerEpoch int64) (HubMapping, error) {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return HubMapping{}, err
	}
	defer tx.Rollback(ctx)
	var m HubMapping
	err = tx.QueryRow(ctx, `SELECT b.id,b.home_id,b.ownership_epoch FROM hubs b JOIN hub_certificates c ON c.hub_id=b.id AND c.serial=$2 AND c.ownership_epoch=$3 AND c.revoked_at IS NULL AND c.expires_at>now() WHERE b.id=$1 AND b.home_id IS NOT NULL AND b.ownership_epoch=$3 AND b.revoked_at IS NULL FOR SHARE OF b,c`, hub, serial, peerEpoch).Scan(&m.HubID, &m.HomeID, &m.OwnershipEpoch)
	if errors.Is(err, pgx.ErrNoRows) {
		return HubMapping{}, ErrDenied
	}
	if err != nil {
		return HubMapping{}, err
	}
	rows, err := tx.Query(ctx, selectSensor+` WHERE hub_id=$1 AND home_id=$2 AND ownership_epoch=$3 AND archived_at IS NULL ORDER BY created_at,id`, hub, m.HomeID, m.OwnershipEpoch)
	if err != nil {
		return HubMapping{}, err
	}
	m.Sensors = []Sensor{}
	for rows.Next() {
		v, e := scanSensor(rows)
		if e != nil {
			rows.Close()
			return HubMapping{}, e
		}
		m.Sensors = append(m.Sensors, v)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return HubMapping{}, err
	}
	return m, tx.Commit(ctx)
}

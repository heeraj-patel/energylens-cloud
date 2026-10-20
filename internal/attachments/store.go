package attachments

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

var ErrNotFound = errors.New("home, sensor, appliance, or attachment not found")
var ErrVersion = errors.New("attachment or sensor version changed")
var ErrConflict = errors.New("attachment effective interval conflicts")
var ErrKeyReuse = errors.New("idempotency key reused")

type Attachment struct {
	ID               string     `json:"id"`
	HomeID           string     `json:"home_id"`
	SensorID         string     `json:"sensor_id"`
	ApplianceID      string     `json:"appliance_id"`
	IdentityRevision int64      `json:"identity_revision"`
	StartedAt        time.Time  `json:"started_at"`
	EndedAt          *time.Time `json:"ended_at"`
	Version          int64      `json:"version"`
	CreatedAt        time.Time  `json:"created_at"`
	UpdatedAt        time.Time  `json:"updated_at"`
}
type CreateInput struct {
	SensorID         string    `json:"sensor_id"`
	ApplianceID      string    `json:"appliance_id"`
	IdentityRevision int64     `json:"identity_revision"`
	StartedAt        time.Time `json:"started_at"`
}
type CloseInput struct {
	EndedAt time.Time `json:"ended_at"`
}
type Store struct{ Pool *pgxpool.Pool }

const selectAttachment = `SELECT id,home_id,sensor_id,appliance_id,identity_revision,started_at,ended_at,version,created_at,updated_at FROM attachments`

func scan(row pgx.Row) (Attachment, error) {
	var a Attachment
	err := row.Scan(&a.ID, &a.HomeID, &a.SensorID, &a.ApplianceID, &a.IdentityRevision, &a.StartedAt, &a.EndedAt, &a.Version, &a.CreatedAt, &a.UpdatedAt)
	return a, err
}
func pgConflict(err error) bool {
	var e *pgconn.PgError
	return errors.As(err, &e) && (e.Code == "23P01" || e.Code == "23505")
}
func owned(ctx context.Context, tx pgx.Tx, owner, home string) error {
	var id string
	err := tx.QueryRow(ctx, `SELECT id FROM homes WHERE id=$1 AND owner_user_id=$2 AND deleted_at IS NULL FOR UPDATE`, home, owner).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	return nil
}
func replay(ctx context.Context, tx pgx.Tx, owner, home, op, key string, hash []byte) (Attachment, bool, error) {
	var old, body []byte
	err := tx.QueryRow(ctx, `SELECT request_hash,response_json FROM attachment_request_idempotency WHERE owner_user_id=$1 AND home_id=$2 AND operation=$3 AND key=$4`, owner, home, op, key).Scan(&old, &body)
	if errors.Is(err, pgx.ErrNoRows) {
		return Attachment{}, false, nil
	}
	if err != nil {
		return Attachment{}, false, err
	}
	if !bytes.Equal(old, hash) {
		return Attachment{}, true, ErrKeyReuse
	}
	var a Attachment
	err = json.Unmarshal(body, &a)
	return a, true, err
}
func save(ctx context.Context, tx pgx.Tx, owner, home, op, key string, hash []byte, a Attachment) error {
	body, err := json.Marshal(a)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `INSERT INTO attachment_request_idempotency(owner_user_id,home_id,operation,key,request_hash,response_json) VALUES($1,$2,$3,$4,$5,$6)`, owner, home, op, key, hash, body)
	return err
}
func snapshot(ctx context.Context, tx pgx.Tx, a Attachment) error {
	body, err := json.Marshal(a)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `INSERT INTO attachment_revisions(attachment_id,version,home_id,snapshot) VALUES($1,$2,$3,$4)`, a.ID, a.Version, a.HomeID, body)
	return err
}
func (s Store) List(ctx context.Context, owner, home string) ([]Attachment, error) {
	var exists bool
	err := s.Pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM homes WHERE id=$1 AND owner_user_id=$2 AND deleted_at IS NULL)`, home, owner).Scan(&exists)
	if err != nil {
		return nil, err
	}
	if !exists {
		return nil, ErrNotFound
	}
	rows, err := s.Pool.Query(ctx, `SELECT a.id,a.home_id,a.sensor_id,a.appliance_id,a.identity_revision,a.started_at,a.ended_at,a.version,a.created_at,a.updated_at FROM attachments a JOIN homes h ON h.id=a.home_id AND h.owner_user_id=$2 AND h.deleted_at IS NULL WHERE a.home_id=$1 ORDER BY a.started_at,a.id`, home, owner)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Attachment{}
	for rows.Next() {
		a, e := scan(rows)
		if e != nil {
			return nil, e
		}
		out = append(out, a)
	}
	return out, rows.Err()
}
func (s Store) Create(ctx context.Context, owner, home string, in CreateInput, sensorVersion int64, key string, hash []byte) (Attachment, error) {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return Attachment{}, err
	}
	defer tx.Rollback(ctx)
	if err = owned(ctx, tx, owner, home); err != nil {
		return Attachment{}, err
	}
	if old, found, e := replay(ctx, tx, owner, home, "create", key, hash); found || e != nil {
		return old, e
	}
	var current, epoch int64
	var created time.Time
	err = tx.QueryRow(ctx, `SELECT s.version,s.ownership_epoch,s.created_at FROM sensor_endpoints s JOIN hubs b ON b.id=s.hub_id AND b.home_id=s.home_id AND b.ownership_epoch=s.ownership_epoch AND b.revoked_at IS NULL WHERE s.id=$1 AND s.home_id=$2 AND s.archived_at IS NULL FOR UPDATE OF s,b`, in.SensorID, home).Scan(&current, &epoch, &created)
	if errors.Is(err, pgx.ErrNoRows) {
		return Attachment{}, ErrNotFound
	}
	if err != nil {
		return Attachment{}, err
	}
	if current != sensorVersion {
		return Attachment{}, ErrVersion
	}
	if in.StartedAt.Before(created) || in.StartedAt.After(time.Now().Add(30*24*time.Hour)) {
		return Attachment{}, ErrConflict
	}
	var applianceVersion int64
	err = tx.QueryRow(ctx, `SELECT a.version FROM appliances a JOIN appliance_revisions r ON r.appliance_id=a.id AND r.version=$3 AND r.home_id=a.home_id WHERE a.id=$1 AND a.home_id=$2 AND a.archived_at IS NULL FOR SHARE OF a,r`, in.ApplianceID, home, in.IdentityRevision).Scan(&applianceVersion)
	if errors.Is(err, pgx.ErrNoRows) {
		return Attachment{}, ErrNotFound
	}
	if err != nil {
		return Attachment{}, err
	}
	if applianceVersion != in.IdentityRevision {
		return Attachment{}, ErrVersion
	}
	old, err := scan(tx.QueryRow(ctx, selectAttachment+` WHERE sensor_id=$1 AND ended_at IS NULL FOR UPDATE`, in.SensorID))
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return Attachment{}, err
	}
	if err == nil {
		if !in.StartedAt.After(old.StartedAt) {
			return Attachment{}, ErrConflict
		}
		var used bool
		err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM energy_observations WHERE attachment_revision_id=$1 AND end_at>$2)`, old.ID, in.StartedAt).Scan(&used)
		if err != nil {
			return Attachment{}, err
		}
		if used {
			return Attachment{}, ErrConflict
		}
		old, err = scan(tx.QueryRow(ctx, `UPDATE attachments SET ended_at=$2,version=version+1,updated_at=now() WHERE id=$1 AND ended_at IS NULL RETURNING id,home_id,sensor_id,appliance_id,identity_revision,started_at,ended_at,version,created_at,updated_at`, old.ID, in.StartedAt))
		if err != nil {
			return Attachment{}, err
		}
		if err = snapshot(ctx, tx, old); err != nil {
			return Attachment{}, err
		}
	}
	var a Attachment
	a, err = scan(tx.QueryRow(ctx, `INSERT INTO attachments(home_id,sensor_id,appliance_id,identity_revision,started_at) VALUES($1,$2,$3,$4,$5) RETURNING id,home_id,sensor_id,appliance_id,identity_revision,started_at,ended_at,version,created_at,updated_at`, home, in.SensorID, in.ApplianceID, in.IdentityRevision, in.StartedAt))
	if pgConflict(err) {
		return Attachment{}, ErrConflict
	}
	if err != nil {
		return Attachment{}, err
	}
	if err = snapshot(ctx, tx, a); err != nil {
		return Attachment{}, err
	}
	_, err = tx.Exec(ctx, `UPDATE sensor_endpoints SET version=version+1 WHERE id=$1`, in.SensorID)
	if err != nil {
		return Attachment{}, err
	}
	if err = save(ctx, tx, owner, home, "create", key, hash, a); err != nil {
		return Attachment{}, err
	}
	return a, tx.Commit(ctx)
}
func (s Store) Close(ctx context.Context, owner, home, id string, end time.Time, version int64, key string, hash []byte) (Attachment, error) {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return Attachment{}, err
	}
	defer tx.Rollback(ctx)
	if err = owned(ctx, tx, owner, home); err != nil {
		return Attachment{}, err
	}
	if old, found, e := replay(ctx, tx, owner, home, "close", key, hash); found || e != nil {
		return old, e
	}
	a, err := scan(tx.QueryRow(ctx, selectAttachment+` WHERE id=$1 AND home_id=$2 FOR UPDATE`, id, home))
	if errors.Is(err, pgx.ErrNoRows) {
		return Attachment{}, ErrNotFound
	}
	if err != nil {
		return Attachment{}, err
	}
	var current int64
	err = tx.QueryRow(ctx, `SELECT s.version FROM sensor_endpoints s JOIN hubs b ON b.id=s.hub_id AND b.home_id=s.home_id AND b.ownership_epoch=s.ownership_epoch AND b.revoked_at IS NULL WHERE s.id=$1 AND s.home_id=$2 AND s.archived_at IS NULL FOR UPDATE OF s,b`, a.SensorID, home).Scan(&current)
	if errors.Is(err, pgx.ErrNoRows) {
		return Attachment{}, ErrNotFound
	}
	if err != nil {
		return Attachment{}, err
	}
	if a.Version != version {
		return Attachment{}, ErrVersion
	}
	if a.EndedAt != nil || !end.After(a.StartedAt) || end.After(time.Now().Add(10*time.Minute)) {
		return Attachment{}, ErrConflict
	}
	var used bool
	err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM energy_observations WHERE attachment_revision_id=$1 AND end_at>$2)`, a.ID, end).Scan(&used)
	if err != nil {
		return Attachment{}, err
	}
	if used {
		return Attachment{}, ErrConflict
	}
	a, err = scan(tx.QueryRow(ctx, `UPDATE attachments SET ended_at=$2,version=version+1,updated_at=now() WHERE id=$1 AND ended_at IS NULL RETURNING id,home_id,sensor_id,appliance_id,identity_revision,started_at,ended_at,version,created_at,updated_at`, id, end))
	if err != nil {
		return Attachment{}, err
	}
	if err = snapshot(ctx, tx, a); err != nil {
		return Attachment{}, err
	}
	_, err = tx.Exec(ctx, `UPDATE sensor_endpoints SET version=version+1 WHERE id=$1`, a.SensorID)
	if err != nil {
		return Attachment{}, err
	}
	if err = save(ctx, tx, owner, home, "close", key, hash, a); err != nil {
		return Attachment{}, err
	}
	return a, tx.Commit(ctx)
}

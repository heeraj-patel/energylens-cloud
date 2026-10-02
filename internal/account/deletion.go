package account

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

// DeletionEntry is the minimal operator journal record to copy outside the
// database and replay after restoring an older backup. The journal contains no
// names, locations, addresses, meter values, or report content.
type DeletionEntry struct {
	Sequence  int64     `json:"sequence"`
	Kind      string    `json:"kind"`
	ObjectID  string    `json:"object_id"`
	DeletedAt time.Time `json:"deleted_at"`
}

// DeleteHome disables one currently owned home, ends its hub association,
// increments each affected hub epoch, and revokes every issued certificate.
// Dependent private rows remain until the separate physical purge exists.
func (s PGStore) DeleteHome(ctx context.Context, owner, home string) error {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if err = lockActiveUser(ctx, tx, owner); err != nil {
		return err
	}
	if err = deleteHomeTx(ctx, tx, owner, home, time.Now().UTC(), false); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// DeleteAccount disables the account's OIDC identity and all its homes in one
// transaction. Auth middleware rejects that issuer/subject after commit.
func (s PGStore) DeleteAccount(ctx context.Context, owner string) error {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if err = lockActiveUser(ctx, tx, owner); err != nil {
		return err
	}
	when := time.Now().UTC()
	rows, err := tx.Query(ctx, `SELECT id FROM homes WHERE owner_user_id=$1 ORDER BY id`, owner)
	if err != nil {
		return err
	}
	homes := []string{}
	for rows.Next() {
		var home string
		if err = rows.Scan(&home); err != nil {
			rows.Close()
			return err
		}
		homes = append(homes, home)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, home := range homes {
		if err = deleteHomeTx(ctx, tx, owner, home, when, true); err != nil {
			return err
		}
	}
	if _, err = tx.Exec(ctx, `UPDATE users SET deleted_at=$2,updated_at=$2,email=NULL,email_verified=false WHERE id=$1`, owner, when); err != nil {
		return err
	}
	if err = recordDeletion(ctx, tx, "account", owner, when); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func lockActiveUser(ctx context.Context, tx pgx.Tx, owner string) error {
	var deleted *time.Time
	err := tx.QueryRow(ctx, `SELECT deleted_at FROM users WHERE id=$1 FOR UPDATE`, owner).Scan(&deleted)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	if deleted != nil {
		return ErrDeleted
	}
	return nil
}

func deleteHomeTx(ctx context.Context, tx pgx.Tx, owner, home string, when time.Time, replay bool) error {
	var rowOwner string
	var deleted *time.Time
	err := tx.QueryRow(ctx, `SELECT owner_user_id,deleted_at FROM homes WHERE id=$1 FOR UPDATE`, home).Scan(&rowOwner, &deleted)
	if errors.Is(err, pgx.ErrNoRows) {
		if replay {
			return recordDeletion(ctx, tx, "home", home, when)
		}
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	if rowOwner != owner {
		return ErrNotFound
	}
	if deleted != nil && !replay {
		return ErrNotFound
	}
	if _, err = tx.Exec(ctx, `UPDATE homes SET deleted_at=LEAST(COALESCE(deleted_at,$2),$2),version=version+1,updated_at=$2 WHERE id=$1 AND deleted_at IS NULL`, home, when); err != nil {
		return err
	}
	// Lock current hub rows before certificate revocation; uploads also lock hub
	// and certificate rows and must either commit before this transaction or
	// fail the current-home/current-epoch checks after it.
	rows, err := tx.Query(ctx, `SELECT id FROM hubs WHERE home_id=$1 ORDER BY id FOR UPDATE`, home)
	if err != nil {
		return err
	}
	hubs := []string{}
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		hubs = append(hubs, id)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, hub := range hubs {
		if _, err = tx.Exec(ctx, `UPDATE hub_certificates SET revoked_at=COALESCE(revoked_at,$2) WHERE hub_id=$1 AND revoked_at IS NULL`, hub, when); err != nil {
			return err
		}
		if _, err = tx.Exec(ctx, `UPDATE hub_ownership_history SET ended_at=COALESCE(ended_at,$2) WHERE hub_id=$1 AND ended_at IS NULL`, hub, when); err != nil {
			return err
		}
		if _, err = tx.Exec(ctx, `UPDATE hubs SET home_id=NULL,ownership_epoch=ownership_epoch+1,version=version+1 WHERE id=$1 AND home_id=$2`, hub, home); err != nil {
			return err
		}
	}
	if _, err = tx.Exec(ctx, `UPDATE hub_claims SET expires_at=LEAST(expires_at,$2) WHERE home_id=$1 AND consumed_at IS NULL`, home, when); err != nil {
		return err
	}
	return recordDeletion(ctx, tx, "home", home, when)
}

func recordDeletion(ctx context.Context, tx pgx.Tx, kind, id string, when time.Time) error {
	_, err := tx.Exec(ctx, `INSERT INTO privacy_deletion_journal(kind,object_id,deleted_at) VALUES($1,$2,$3) ON CONFLICT(kind,object_id) DO UPDATE SET deleted_at=LEAST(privacy_deletion_journal.deleted_at,EXCLUDED.deleted_at)`, kind, id, when)
	return err
}

// JournalSince returns a bounded ordered copy for an operator-controlled
// independent deletion ledger. This is deliberately not a customer route.
func (s PGStore) JournalSince(ctx context.Context, after int64, limit int) ([]DeletionEntry, error) {
	if after < 0 || limit < 1 || limit > 1000 {
		return nil, errors.New("invalid journal page")
	}
	rows, err := s.Pool.Query(ctx, `SELECT sequence,kind,object_id,deleted_at FROM privacy_deletion_journal WHERE sequence>$1 ORDER BY sequence LIMIT $2`, after, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []DeletionEntry{}
	for rows.Next() {
		var e DeletionEntry
		if err = rows.Scan(&e.Sequence, &e.Kind, &e.ObjectID, &e.DeletedAt); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// ReapplyDeletion is for a trusted restore operator using entries from the
// independent ledger. It is idempotent. A restored identity may still exist;
// the account entry disables it and all restored homes before API traffic.
func (s PGStore) ReapplyDeletion(ctx context.Context, e DeletionEntry) error {
	if e.DeletedAt.IsZero() || (e.Kind != "account" && e.Kind != "home") {
		return errors.New("invalid deletion entry")
	}
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if e.Kind == "account" {
		var existing string
		err = tx.QueryRow(ctx, `SELECT id FROM users WHERE id=$1 FOR UPDATE`, e.ObjectID).Scan(&existing)
		if errors.Is(err, pgx.ErrNoRows) {
			err = recordDeletion(ctx, tx, e.Kind, e.ObjectID, e.DeletedAt)
			if err != nil {
				return err
			}
			return tx.Commit(ctx)
		}
		if err != nil {
			return err
		}
		rows, queryErr := tx.Query(ctx, `SELECT id FROM homes WHERE owner_user_id=$1 ORDER BY id`, e.ObjectID)
		if queryErr != nil {
			return queryErr
		}
		homes := []string{}
		for rows.Next() {
			var id string
			if queryErr = rows.Scan(&id); queryErr != nil {
				rows.Close()
				return queryErr
			}
			homes = append(homes, id)
		}
		queryErr = rows.Err()
		rows.Close()
		if queryErr != nil {
			return queryErr
		}
		for _, home := range homes {
			if err = deleteHomeTx(ctx, tx, e.ObjectID, home, e.DeletedAt, true); err != nil {
				return err
			}
		}
		if _, err = tx.Exec(ctx, `UPDATE users SET deleted_at=LEAST(COALESCE(deleted_at,$2),$2),updated_at=$2,email=NULL,email_verified=false WHERE id=$1`, e.ObjectID, e.DeletedAt); err != nil {
			return err
		}
	} else {
		var owner string
		err = tx.QueryRow(ctx, `SELECT owner_user_id FROM homes WHERE id=$1`, e.ObjectID).Scan(&owner)
		if errors.Is(err, pgx.ErrNoRows) {
			err = recordDeletion(ctx, tx, e.Kind, e.ObjectID, e.DeletedAt)
			if err != nil {
				return err
			}
			return tx.Commit(ctx)
		}
		if err != nil {
			return err
		}
		if err = deleteHomeTx(ctx, tx, owner, e.ObjectID, e.DeletedAt, true); err != nil {
			return err
		}
	}
	if err = recordDeletion(ctx, tx, e.Kind, e.ObjectID, e.DeletedAt); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

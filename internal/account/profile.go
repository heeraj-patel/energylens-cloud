package account

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type ProfileStore interface {
	PatchProfile(context.Context, string, *string, int64, string, []byte) (User, error)
}

type PGProfileStore struct{ Pool *pgxpool.Pool }

func (s PGProfileStore) PatchProfile(ctx context.Context, owner string, name *string, version int64, key string, hash []byte) (User, error) {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return User{}, err
	}
	defer tx.Rollback(ctx)
	var current int64
	err = tx.QueryRow(ctx, `SELECT profile_version FROM users WHERE id=$1 AND deleted_at IS NULL FOR UPDATE`, owner).Scan(&current)
	if errors.Is(err, pgx.ErrNoRows) {
		return User{}, ErrNotFound
	}
	if err != nil {
		return User{}, err
	}
	var replay User
	if found, e := prefReplay(ctx, tx, owner, "account_profile", key, hash, &replay); found || e != nil {
		return replay, e
	}
	if current != version {
		return User{}, ErrVersion
	}
	var updated User
	err = tx.QueryRow(ctx, `UPDATE users SET display_name=$2,profile_version=profile_version+1,updated_at=now() WHERE id=$1 AND deleted_at IS NULL RETURNING id,email,email_verified,display_name,profile_version,created_at`, owner, name).Scan(&updated.ID, &updated.Email, &updated.EmailVerified, &updated.DisplayName, &updated.ProfileVersion, &updated.CreatedAt)
	if err != nil {
		return User{}, err
	}
	if err = savePrefReplay(ctx, tx, owner, "account_profile", key, hash, updated); err != nil {
		return User{}, err
	}
	return updated, tx.Commit(ctx)
}

func validDisplayName(name *string) bool {
	if name == nil {
		return true
	}
	if *name == "" || *name != strings.TrimSpace(*name) || !utf8.ValidString(*name) || utf8.RuneCountInString(*name) > 120 {
		return false
	}
	for _, r := range *name {
		if unicode.IsControl(r) {
			return false
		}
	}
	return true
}

func (s Service) patchProfile(w http.ResponseWriter, r *http.Request) {
	key, ok := idempotencyKey(w, r)
	if !ok {
		return
	}
	version, ok := matchVersion(w, r)
	if !ok {
		return
	}
	var fields map[string]json.RawMessage
	body, ok := decode(w, r, &fields)
	if !ok {
		return
	}
	raw, present := fields["display_name"]
	if !present || len(fields) != 1 {
		fail(w, http.StatusBadRequest, "invalid_request", "display_name required")
		return
	}
	var name *string
	if string(raw) != "null" {
		var value string
		if err := json.Unmarshal(raw, &value); err != nil {
			fail(w, http.StatusBadRequest, "invalid_request", "invalid display_name")
			return
		}
		name = &value
	}
	if !validDisplayName(name) {
		fail(w, http.StatusBadRequest, "invalid_request", "invalid display_name")
		return
	}
	hash := sha256.Sum256(append([]byte(strconv.FormatInt(version, 10)+"\n"), body...))
	updated, err := s.Profile.PatchProfile(r.Context(), user(r).ID, name, version, key, hash[:])
	if err != nil {
		switch {
		case errors.Is(err, ErrVersion):
			fail(w, http.StatusConflict, "version_conflict", "profile version changed")
		case errors.Is(err, ErrKeyReuse):
			fail(w, http.StatusConflict, "idempotency_key_reuse", "key was used with another request")
		case errors.Is(err, ErrNotFound), errors.Is(err, ErrDeleted):
			fail(w, http.StatusForbidden, "account_deleted", "account is closed")
		default:
			internal(w, err)
		}
		return
	}
	etag(w, updated.ProfileVersion)
	jsonOut(w, http.StatusOK, updated)
}

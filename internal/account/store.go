package account

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"energylens.local/cloud/internal/democost"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

var ErrNotFound = errors.New("not found")
var ErrVersion = errors.New("version conflict")
var ErrKeyReuse = errors.New("idempotency key reused with different request")
var ErrDeleted = errors.New("account deleted")

type User struct {
	ID             string    `json:"id"`
	Email          *string   `json:"email"`
	EmailVerified  bool      `json:"email_verified"`
	Username       *string   `json:"username"`
	AuthMethod     string    `json:"auth_method"`
	DisplayName    *string   `json:"display_name"`
	ProfileVersion int64     `json:"profile_version"`
	CreatedAt      time.Time `json:"created_at"`
}
type Location struct {
	CountryCode string  `json:"country_code"`
	PostalCode  string  `json:"postal_code"`
	City        *string `json:"city,omitempty"`
	County      *string `json:"county,omitempty"`
	Precision   string  `json:"precision"`
	Source      string  `json:"source"`
}
type Home struct {
	ID                 string    `json:"id"`
	Name               string    `json:"name"`
	Timezone           string    `json:"timezone"`
	Location           Location  `json:"location"`
	TimezoneResolution string    `json:"timezone_resolution,omitempty"`
	Version            int64     `json:"version"`
	CreatedAt          time.Time `json:"created_at"`
	UpdatedAt          time.Time `json:"updated_at"`
}
type HomeCreate struct {
	Name     string   `json:"name"`
	Timezone string   `json:"timezone"`
	Location Location `json:"location"`
}
type HomePatch struct {
	Name     *string   `json:"name,omitempty"`
	Timezone *string   `json:"timezone,omitempty"`
	Location *Location `json:"location,omitempty"`
}

type Store interface {
	Resolve(context.Context, Identity) (User, error)
	List(context.Context, string) ([]Home, error)
	Get(context.Context, string, string) (Home, error)
	Create(context.Context, string, HomeCreate, string, []byte) (Home, error)
	Patch(context.Context, string, string, HomePatch, int64, string, []byte) (Home, error)
}

type PGStore struct{ Pool *pgxpool.Pool }

func (s PGStore) Resolve(ctx context.Context, id Identity) (User, error) {
	if id.Issuer == "" || id.Subject == "" {
		return User{}, errors.New("missing stable identity")
	}
	var u User
	var deletedAt *time.Time
	q := `SELECT u.id,u.email,u.email_verified,u.display_name,u.profile_version,u.created_at,u.deleted_at FROM users u JOIN auth_identities i ON i.user_id=u.id WHERE i.issuer=$1 AND i.subject=$2`
	err := s.Pool.QueryRow(ctx, q, id.Issuer, id.Subject).Scan(&u.ID, &u.Email, &u.EmailVerified, &u.DisplayName, &u.ProfileVersion, &u.CreatedAt, &deletedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		// First-party identities are created only by the credential transaction.
		if id.Issuer == LocalIssuer || id.Issuer == LocalUsernameIssuer {
			return User{}, ErrNotFound
		}
		tx, e := s.Pool.Begin(ctx)
		if e != nil {
			return User{}, e
		}
		defer tx.Rollback(ctx)
		var newID string
		if e = tx.QueryRow(ctx, `INSERT INTO users(email,email_verified) VALUES($1,$2) RETURNING id`, nullableEmail(id), id.EmailVerified).Scan(&newID); e != nil {
			return User{}, e
		}
		var inserted string
		e = tx.QueryRow(ctx, `INSERT INTO auth_identities(user_id,issuer,subject) VALUES($1,$2,$3) ON CONFLICT (issuer,subject) DO NOTHING RETURNING user_id`, newID, id.Issuer, id.Subject).Scan(&inserted)
		if errors.Is(e, pgx.ErrNoRows) {
			_ = tx.Rollback(ctx)
		} else if e != nil {
			return User{}, e
		} else if e = tx.Commit(ctx); e != nil {
			return User{}, e
		}
		err = s.Pool.QueryRow(ctx, q, id.Issuer, id.Subject).Scan(&u.ID, &u.Email, &u.EmailVerified, &u.DisplayName, &u.ProfileVersion, &u.CreatedAt, &deletedAt)
	}
	if err != nil {
		return User{}, fmt.Errorf("resolve identity: %w", err)
	}
	if deletedAt != nil {
		return User{}, ErrDeleted
	}
	switch id.Issuer {
	case LocalUsernameIssuer:
		u.AuthMethod = "username_password"
		if err = s.Pool.QueryRow(ctx, `SELECT username_normalized FROM auth_password_credentials
			WHERE user_id=$1 AND credential_kind='username_local' AND activated_at IS NOT NULL`, u.ID).Scan(&u.Username); err != nil {
			return User{}, fmt.Errorf("resolve username identity: %w", err)
		}
	case LocalIssuer:
		u.AuthMethod = "email_password"
	default:
		u.AuthMethod = "oidc"
	}
	// A verified email may change without changing the user's primary identity.
	if id.EmailVerified && id.Email != "" {
		err = s.Pool.QueryRow(ctx, `UPDATE users SET email=$2,email_verified=true,updated_at=now() WHERE id=$1 AND deleted_at IS NULL RETURNING id,email,email_verified,display_name,profile_version,created_at`, u.ID, id.Email).Scan(&u.ID, &u.Email, &u.EmailVerified, &u.DisplayName, &u.ProfileVersion, &u.CreatedAt)
		if errors.Is(err, pgx.ErrNoRows) {
			return User{}, ErrDeleted
		}
		if err != nil {
			return User{}, err
		}
	}
	return u, nil
}
func nullableEmail(id Identity) any {
	if id.EmailVerified && id.Email != "" {
		return id.Email
	}
	return nil
}

const homeSelect = `SELECT h.id,h.name,h.timezone,h.version,h.created_at,h.updated_at,l.country_code,l.postal_code,l.city,l.county,l.precision,l.source FROM homes h JOIN LATERAL (SELECT * FROM home_location_revisions WHERE home_id=h.id ORDER BY revision DESC LIMIT 1) l ON true`

type row interface{ Scan(...any) error }

func scanHome(r row) (Home, error) {
	var h Home
	err := r.Scan(&h.ID, &h.Name, &h.Timezone, &h.Version, &h.CreatedAt, &h.UpdatedAt, &h.Location.CountryCode, &h.Location.PostalCode, &h.Location.City, &h.Location.County, &h.Location.Precision, &h.Location.Source)
	return h, err
}
func (s PGStore) List(ctx context.Context, owner string) ([]Home, error) {
	rows, err := s.Pool.Query(ctx, homeSelect+` WHERE h.owner_user_id=$1 AND h.deleted_at IS NULL ORDER BY h.created_at,h.id`, owner)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Home{}
	for rows.Next() {
		h, e := scanHome(rows)
		if e != nil {
			return nil, e
		}
		out = append(out, h)
	}
	return out, rows.Err()
}
func (s PGStore) Get(ctx context.Context, owner, home string) (Home, error) {
	h, err := scanHome(s.Pool.QueryRow(ctx, homeSelect+` WHERE h.owner_user_id=$1 AND h.id=$2 AND h.deleted_at IS NULL`, owner, home))
	if errors.Is(err, pgx.ErrNoRows) {
		return Home{}, ErrNotFound
	}
	return h, err
}
func readReplay(ctx context.Context, tx pgx.Tx, owner, op, key string, hash []byte) (Home, bool, error) {
	var oldHash, body []byte
	var status int
	err := tx.QueryRow(ctx, `SELECT request_hash,response_json,response_status FROM account_home_idempotency WHERE owner_user_id=$1 AND operation=$2 AND key=$3`, owner, op, key).Scan(&oldHash, &body, &status)
	if errors.Is(err, pgx.ErrNoRows) {
		return Home{}, false, nil
	}
	if err != nil {
		return Home{}, false, err
	}
	if string(oldHash) != string(hash) {
		return Home{}, true, ErrKeyReuse
	}
	var h Home
	if err = json.Unmarshal(body, &h); err != nil {
		return Home{}, true, err
	}
	return h, true, nil
}
func saveReplay(ctx context.Context, tx pgx.Tx, owner, op, key string, hash []byte, h Home, status int) error {
	b, e := json.Marshal(h)
	if e != nil {
		return e
	}
	_, e = tx.Exec(ctx, `INSERT INTO account_home_idempotency(owner_user_id,operation,key,request_hash,response_json,response_status) VALUES($1,$2,$3,$4,$5,$6)`, owner, op, key, hash, b, status)
	return e
}
func (s PGStore) Create(ctx context.Context, owner string, in HomeCreate, key string, hash []byte) (Home, error) {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return Home{}, err
	}
	defer tx.Rollback(ctx)
	// Serialize requests for this account so identical concurrent keys replay.
	var lockedOwner string
	if err = tx.QueryRow(ctx, `SELECT id FROM users WHERE id=$1 AND deleted_at IS NULL FOR UPDATE`, owner).Scan(&lockedOwner); errors.Is(err, pgx.ErrNoRows) {
		return Home{}, ErrNotFound
	} else if err != nil {
		return Home{}, err
	}
	if h, found, e := readReplay(ctx, tx, owner, "create_home", key, hash); found || e != nil {
		return h, e
	}
	var h Home
	err = tx.QueryRow(ctx, `INSERT INTO homes(owner_user_id,name,timezone) VALUES($1,$2,$3) RETURNING id,name,timezone,version,created_at,updated_at`, owner, in.Name, in.Timezone).Scan(&h.ID, &h.Name, &h.Timezone, &h.Version, &h.CreatedAt, &h.UpdatedAt)
	if err != nil {
		return Home{}, err
	}
	h.Location = in.Location
	_, err = tx.Exec(ctx, `INSERT INTO home_location_revisions(home_id,revision,country_code,postal_code,city,county,precision,source) VALUES($1,1,$2,$3,$4,$5,$6,$7)`, h.ID, in.Location.CountryCode, in.Location.PostalCode, in.Location.City, in.Location.County, in.Location.Precision, in.Location.Source)
	if err != nil {
		return Home{}, err
	}
	if err = saveReplay(ctx, tx, owner, "create_home", key, hash, h, 201); err != nil {
		return Home{}, err
	}
	return h, tx.Commit(ctx)
}
func (s PGStore) Patch(ctx context.Context, owner, home string, in HomePatch, version int64, key string, hash []byte) (Home, error) {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return Home{}, err
	}
	defer tx.Rollback(ctx)
	var lockedOwner string
	if err = tx.QueryRow(ctx, `SELECT id FROM users WHERE id=$1 AND deleted_at IS NULL FOR UPDATE`, owner).Scan(&lockedOwner); errors.Is(err, pgx.ErrNoRows) {
		return Home{}, ErrNotFound
	} else if err != nil {
		return Home{}, err
	}
	var current int64
	var currentTimezone string
	var currentDemo []byte
	err = tx.QueryRow(ctx, `SELECT version,timezone,demo_cost_snapshot FROM homes WHERE id=$1 AND owner_user_id=$2 AND deleted_at IS NULL FOR UPDATE`, home, owner).Scan(&current, &currentTimezone, &currentDemo)
	if errors.Is(err, pgx.ErrNoRows) {
		return Home{}, ErrNotFound
	}
	if err != nil {
		return Home{}, err
	}
	op := "patch_home:" + home
	if h, found, e := readReplay(ctx, tx, owner, op, key, hash); found || e != nil {
		return h, e
	}
	if current != version {
		return Home{}, ErrVersion
	}
	effectiveTimezone := in.Timezone
	timezoneResolution := ""
	if in.Location != nil {
		var oldCountry, oldPostal string
		if err = tx.QueryRow(ctx, `SELECT country_code,postal_code FROM home_location_revisions WHERE home_id=$1 ORDER BY revision DESC LIMIT 1`, home).Scan(&oldCountry, &oldPostal); err != nil {
			return Home{}, err
		}
		if in.Location.CountryCode != oldCountry || in.Location.PostalCode != oldPostal {
			timezoneResolution = "unresolved"
			if in.Location.CountryCode == "US" {
				if zone, found := resolveUSZIPTimeZone(in.Location.PostalCode); found {
					effectiveTimezone = &zone
					timezoneResolution = "resolved"
				}
			}
			if timezoneResolution == "unresolved" && in.Timezone != nil {
				timezoneResolution = ""
			}
		}
	}
	updatedDemo := currentDemo
	if effectiveTimezone != nil && *effectiveTimezone != currentTimezone && len(currentDemo) > 0 {
		updatedDemo = democost.Retarget(currentDemo, currentTimezone, *effectiveTimezone)
	}
	var demoParam any
	if len(updatedDemo) > 0 {
		demoParam = string(updatedDemo)
	}
	var h Home
	err = tx.QueryRow(ctx, `UPDATE homes SET name=COALESCE($3,name),timezone=COALESCE($4,timezone),
		demo_cost_snapshot=$5::jsonb,
		version=version+1,updated_at=now() WHERE id=$1 AND owner_user_id=$2 RETURNING id,name,timezone,version,created_at,updated_at`, home, owner, in.Name, effectiveTimezone, demoParam).Scan(&h.ID, &h.Name, &h.Timezone, &h.Version, &h.CreatedAt, &h.UpdatedAt)
	if err != nil {
		return Home{}, err
	}
	h.TimezoneResolution = timezoneResolution
	if in.Location != nil {
		h.Location = *in.Location
		_, err = tx.Exec(ctx, `INSERT INTO home_location_revisions(home_id,revision,country_code,postal_code,city,county,precision,source) VALUES($1,$2,$3,$4,$5,$6,$7,$8)`, home, h.Version, h.Location.CountryCode, h.Location.PostalCode, h.Location.City, h.Location.County, h.Location.Precision, h.Location.Source)
	} else {
		err = tx.QueryRow(ctx, `SELECT country_code,postal_code,city,county,precision,source FROM home_location_revisions WHERE home_id=$1 ORDER BY revision DESC LIMIT 1`, home).Scan(&h.Location.CountryCode, &h.Location.PostalCode, &h.Location.City, &h.Location.County, &h.Location.Precision, &h.Location.Source)
	}
	if err != nil {
		return Home{}, err
	}
	if err = saveReplay(ctx, tx, owner, op, key, hash, h, 200); err != nil {
		return Home{}, err
	}
	return h, tx.Commit(ctx)
}

// Package migrate applies the EnergyLens Cloud SQL files with a dedicated
// migration database role. API and worker startup never run DDL.
package migrate

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"strings"

	"energylens.local/cloud/migrations"
	"github.com/jackc/pgx/v5"
)

const lockKey int64 = 0x656c636d69677274 // elcmigrt

func Run(ctx context.Context, dsn, environment string) error {
	cfg, err := ValidateConfig(dsn, environment)
	if err != nil {
		return err
	}
	conn, err := pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		return fmt.Errorf("connect migration database: %w", err)
	}
	defer conn.Close(context.WithoutCancel(ctx))
	return Apply(ctx, conn, migrations.FS)
}

// ValidateConfig checks the migration role without connecting to the database.
func ValidateConfig(dsn, environment string) (*pgx.ConnConfig, error) {
	if strings.TrimSpace(dsn) == "" {
		return nil, errors.New("ENERGYLENS_CLOUD_MIGRATION_DATABASE_URL is required")
	}
	cfg, err := pgx.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("invalid migration database configuration: %w", err)
	}
	if environment != "dev" && environment != "test" && environment != "staging" && environment != "production" {
		return nil, errors.New("ENERGYLENS_CLOUD_ENV must be dev, test, staging, or production")
	}
	if environment != "dev" && environment != "test" {
		if cfg.TLSConfig == nil || cfg.TLSConfig.ServerName == "" || cfg.TLSConfig.InsecureSkipVerify {
			return nil, errors.New("migration database must use sslmode=verify-full")
		}
		for _, fallback := range cfg.Fallbacks {
			if fallback.TLSConfig == nil || fallback.TLSConfig.InsecureSkipVerify {
				return nil, errors.New("migration database plaintext or unverified fallback is forbidden")
			}
		}
	}
	return cfg, nil
}

func Files(fsys fs.FS) ([]string, error) {
	names, err := fs.Glob(fsys, "0*.sql")
	if err != nil {
		return nil, err
	}
	if len(names) == 0 {
		return nil, errors.New("no migration files")
	}
	return names, nil
}

// Apply checks every previously recorded SQL checksum. Existing 0016/0017
// installations without a ledger are safely baselined by running their
// idempotent SQL once, then recording the exact bytes applied.
func Apply(ctx context.Context, conn *pgx.Conn, fsys fs.FS) error {
	names, err := Files(fsys)
	if err != nil {
		return err
	}
	if _, err := conn.Exec(ctx, "SELECT pg_advisory_lock($1)", lockKey); err != nil {
		return fmt.Errorf("lock migrations: %w", err)
	}
	defer func() { _, _ = conn.Exec(context.WithoutCancel(ctx), "SELECT pg_advisory_unlock($1)", lockKey) }()
	if _, err := conn.Exec(ctx, `CREATE TABLE IF NOT EXISTS energylens_schema_migrations (
        name TEXT PRIMARY KEY, sha256 TEXT NOT NULL, applied_at TIMESTAMPTZ NOT NULL DEFAULT now()
    )`); err != nil {
		return fmt.Errorf("create migration ledger: %w", err)
	}
	for _, name := range names {
		body, err := fs.ReadFile(fsys, name)
		if err != nil {
			return fmt.Errorf("read %s: %w", name, err)
		}
		sum := sha256.Sum256(body)
		digest := hex.EncodeToString(sum[:])
		var old string
		err = conn.QueryRow(ctx, "SELECT sha256 FROM energylens_schema_migrations WHERE name=$1", name).Scan(&old)
		if err == nil {
			if old != digest {
				return fmt.Errorf("migration %s checksum changed after application", name)
			}
			continue
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("read migration ledger: %w", err)
		}
		tx, err := conn.Begin(ctx)
		if err != nil {
			return fmt.Errorf("begin %s: %w", name, err)
		}
		if _, err = tx.Exec(ctx, string(body)); err == nil {
			_, err = tx.Exec(ctx, "INSERT INTO energylens_schema_migrations(name,sha256) VALUES($1,$2)", name, digest)
		}
		if err != nil {
			_ = tx.Rollback(context.WithoutCancel(ctx))
			return fmt.Errorf("apply %s: %w", name, err)
		}
		if err = tx.Commit(ctx); err != nil {
			return fmt.Errorf("commit %s: %w", name, err)
		}
		slog.Info("migration applied", "name", name)
	}
	return nil
}

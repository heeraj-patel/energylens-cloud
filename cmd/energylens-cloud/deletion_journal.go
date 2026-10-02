package main

import (
	"context"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"

	"energylens.local/cloud/internal/account"
	"github.com/jackc/pgx/v5/pgxpool"
)

func deletionJournalConfig() (account.DirectoryJournal, bool, error) {
	dir, keyPath := os.Getenv("ENERGYLENS_CLOUD_DELETION_JOURNAL_DIR"), os.Getenv("ENERGYLENS_CLOUD_DELETION_JOURNAL_KEY_FILE")
	if dir == "" && keyPath == "" {
		return account.DirectoryJournal{}, false, nil
	}
	if dir == "" || keyPath == "" {
		return account.DirectoryJournal{}, false, errors.New("both deletion journal directory and key file are required")
	}
	info, err := os.Lstat(keyPath)
	if err != nil {
		return account.DirectoryJournal{}, false, err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 || info.Size() > 130 {
		return account.DirectoryJournal{}, false, errors.New("deletion journal key file must be a private regular file")
	}
	raw, err := os.ReadFile(keyPath)
	if err != nil {
		return account.DirectoryJournal{}, false, err
	}
	key, err := hex.DecodeString(strings.TrimSpace(string(raw)))
	if err != nil || len(key) < 32 {
		return account.DirectoryJournal{}, false, errors.New("deletion journal key file must contain at least 32 hex-encoded bytes")
	}
	return account.DirectoryJournal{Dir: dir, Key: key}, true, nil
}

func deletionJournalOperator(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("deletion-journal", flag.ContinueOnError)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return errors.New("usage: deletion-journal export|replay|verify")
	}
	action := fs.Arg(0)
	if action != "export" && action != "replay" && action != "verify" {
		return errors.New("usage: deletion-journal export|replay|verify")
	}
	journal, enabled, err := deletionJournalConfig()
	if err != nil {
		return err
	}
	if !enabled {
		return errors.New("deletion journal configuration required")
	}
	if action == "verify" {
		entries, err := journal.ReadAll()
		if err != nil {
			return err
		}
		fmt.Printf("verified %d deletion records\n", len(entries))
		return nil
	}
	pool, err := operatorPool(ctx)
	if err != nil {
		return err
	}
	defer pool.Close()
	store := account.PGStore{Pool: pool}
	var count int
	if action == "export" {
		count, err = store.ExportDeletionJournal(ctx, journal)
	} else {
		count, err = store.ReplayDeletionJournal(ctx, journal)
	}
	if err != nil {
		return err
	}
	fmt.Printf("%s %d deletion records\n", action, count)
	return nil
}

func operatorPool(ctx context.Context) (*pgxpool.Pool, error) {
	dsn := os.Getenv("ENERGYLENS_CLOUD_MIGRATION_DATABASE_URL")
	if dsn == "" {
		return nil, errors.New("migration database URL required for deletion journal")
	}
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, err
	}
	env := os.Getenv("ENERGYLENS_CLOUD_ENV")
	if env != "dev" && env != "test" {
		if cfg.ConnConfig.TLSConfig == nil || cfg.ConnConfig.TLSConfig.ServerName == "" || cfg.ConnConfig.TLSConfig.InsecureSkipVerify {
			return nil, errors.New("deletion journal database requires sslmode=verify-full")
		}
		for _, fallback := range cfg.ConnConfig.Fallbacks {
			if fallback.TLSConfig == nil || fallback.TLSConfig.InsecureSkipVerify {
				return nil, errors.New("unverified deletion journal database fallback forbidden")
			}
		}
	}
	return pgxpool.NewWithConfig(ctx, cfg)
}

// A configured archive is replayed before any API or worker listener starts.
func replayDeletionJournalBeforeServe(ctx context.Context) error {
	journal, enabled, err := deletionJournalConfig()
	if err != nil || !enabled {
		return err
	}
	pool, err := operatorPool(ctx)
	if err != nil {
		return err
	}
	defer pool.Close()
	_, err = (account.PGStore{Pool: pool}).ReplayDeletionJournal(ctx, journal)
	return err
}

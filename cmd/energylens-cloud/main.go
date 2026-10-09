package main

import (
	"context"
	"encoding/json"
	"energylens.local/cloud/internal/catalogimport"
	"energylens.local/cloud/internal/householdenergy"
	"energylens.local/cloud/internal/hubtrust"
	"energylens.local/cloud/internal/usagefactimport"
	"errors"
	"flag"
	"fmt"
	"github.com/jackc/pgx/v5/pgxpool"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"energylens.local/cloud/internal/app"
	"energylens.local/cloud/internal/migrate"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, os.Args[1:]); err != nil {
		slog.Error("energylens-cloud stopped", "error", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string) error {
	if len(args) > 0 && args[0] == "export-unresolved-device-help" {
		return identityHelpExport(ctx, args[1:])
	}
	if len(args) > 0 && args[0] == "check-local-auth-keys" {
		fs := flag.NewFlagSet("check-local-auth-keys", flag.ContinueOnError)
		previousID := fs.String("retain-key-id", "", "previous key ID that must remain trusted")
		previousPath := fs.String("retain-public-key-file", "", "pinned previous public key PEM")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		if fs.NArg() != 0 {
			return errors.New("unexpected check-local-auth-keys argument")
		}
		cfg, err := app.LoadConfig("api")
		if err != nil {
			return err
		}
		return app.CheckLocalAuthKeys(cfg, *previousID, *previousPath)
	}
	if len(args) > 0 && args[0] == "deletion-journal" {
		return deletionJournalOperator(ctx, args[1:])
	}
	if len(args) > 0 && (args[0] == "provision-hub" || args[0] == "issue-hub-enroll-nonce" || args[0] == "import-reviewed-catalog" || args[0] == "import-reviewed-usage-facts" || args[0] == "import-official-tariff" || args[0] == "import-reviewed-program" || args[0] == "import-demo-cost-snapshot") {
		return operator(ctx, args)
	}
	if len(args) > 0 && args[0] == "migrate" {
		if len(args) != 1 {
			return errors.New("usage: energylens-cloud migrate")
		}
		return migrate.Run(ctx, os.Getenv("ENERGYLENS_CLOUD_MIGRATION_DATABASE_URL"), os.Getenv("ENERGYLENS_CLOUD_ENV"))
	}
	fs := flag.NewFlagSet("energylens-cloud", flag.ContinueOnError)
	roleFlag := fs.String("role", "", "api, worker, or all")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return fmt.Errorf("unexpected argument %q", fs.Arg(0))
	}
	role := *roleFlag
	if role == "" {
		role = os.Getenv("ENERGYLENS_CLOUD_ROLE")
	}
	if role == "" {
		role = "all"
	}
	cfg, err := app.LoadConfig(role)
	if err != nil {
		return err
	}
	if err := replayDeletionJournalBeforeServe(ctx); err != nil {
		return fmt.Errorf("deletion journal pre-serve replay: %w", err)
	}
	return app.Serve(ctx, cfg)
}

// Operational provisioning is intentionally outside the public HTTP surface.
// Access requires the dedicated migration/provisioning database credential.
func operator(ctx context.Context, args []string) error {
	dsn := os.Getenv("ENERGYLENS_CLOUD_MIGRATION_DATABASE_URL")
	if dsn == "" {
		return errors.New("migration/provisioning database URL required")
	}
	cfg, e := pgxpool.ParseConfig(dsn)
	if e != nil {
		return e
	}
	env := os.Getenv("ENERGYLENS_CLOUD_ENV")
	if env != "dev" && env != "test" {
		if cfg.ConnConfig.TLSConfig == nil || cfg.ConnConfig.TLSConfig.ServerName == "" || cfg.ConnConfig.TLSConfig.InsecureSkipVerify {
			return errors.New("provisioning database requires sslmode=verify-full")
		}
		for _, f := range cfg.ConnConfig.Fallbacks {
			if f.TLSConfig == nil || f.TLSConfig.InsecureSkipVerify {
				return errors.New("unverified provisioning database fallback forbidden")
			}
		}
	}
	if apiDSN := os.Getenv("ENERGYLENS_CLOUD_DATABASE_URL"); apiDSN != "" {
		apiCfg, err := pgxpool.ParseConfig(apiDSN)
		if err == nil && apiCfg.ConnConfig.Config.User == cfg.ConnConfig.Config.User {
			return errors.New("provisioning requires a distinct database role from API")
		}
	}
	pool, e := pgxpool.NewWithConfig(ctx, cfg)
	if e != nil {
		return e
	}
	defer pool.Close()
	s := hubtrust.Store{Pool: pool}
	fs := flag.NewFlagSet(args[0], flag.ContinueOnError)
	var result any
	if args[0] == "import-reviewed-catalog" {
		artifact := fs.String("artifact", "", "signed reviewed fact publication JSON")
		actor := fs.String("imported-by", "", "administrator identifier")
		if e = fs.Parse(args[1:]); e != nil {
			return e
		}
		if fs.NArg() != 0 || *artifact == "" || strings.TrimSpace(*actor) == "" {
			return errors.New("artifact and imported-by required")
		}
		keyPath := os.Getenv("ENERGYLENS_CLOUD_KB_PUBLIC_KEY_FILE")
		if keyPath == "" {
			return errors.New("pinned ENERGYLENS_CLOUD_KB_PUBLIC_KEY_FILE required")
		}
		verified, err := catalogimport.ReadAndVerify(*artifact, keyPath)
		if err != nil {
			return err
		}
		result, err = catalogimport.Import(ctx, pool, verified, *actor)
		if err != nil {
			return err
		}
	}
	if args[0] == "import-reviewed-usage-facts" {
		artifact := fs.String("artifact", "", "signed reviewed usage-fact publication JSON")
		actor := fs.String("imported-by", "", "administrator identifier")
		if e = fs.Parse(args[1:]); e != nil {
			return e
		}
		if fs.NArg() != 0 || *artifact == "" || strings.TrimSpace(*actor) == "" {
			return errors.New("artifact and imported-by required")
		}
		keyPath := os.Getenv("ENERGYLENS_CLOUD_KB_PUBLIC_KEY_FILE")
		if keyPath == "" {
			return errors.New("pinned ENERGYLENS_CLOUD_KB_PUBLIC_KEY_FILE required")
		}
		verified, err := usagefactimport.ReadAndVerify(*artifact, keyPath)
		if err != nil {
			return err
		}
		result, err = usagefactimport.Import(ctx, pool, verified, *actor)
		if err != nil {
			return err
		}
	}
	if args[0] == "import-official-tariff" {
		result, e = importOfficialTariff(ctx, pool, fs, args[1:])
		if e != nil {
			return e
		}
	}
	if args[0] == "import-reviewed-program" {
		result, e = importReviewedProgram(ctx, pool, fs, args[1:])
		if e != nil {
			return e
		}
	}
	if args[0] == "import-demo-cost-snapshot" {
		owner := fs.String("owner-id", "", "owning user UUID")
		home := fs.String("home-id", "", "home UUID")
		seed := fs.Bool("seed-mobile-demo-v1", false, "import the approved 18-device mobile demo schedule")
		if e = fs.Parse(args[1:]); e != nil {
			return e
		}
		if fs.NArg() != 0 || *owner == "" || *home == "" {
			return errors.New("owner-id and home-id required; use --seed-mobile-demo-v1 or provide snapshot JSON on stdin")
		}
		var snapshot householdenergy.DemoCostSnapshot
		if *seed {
			snapshot = householdenergy.MobileDemoCostSnapshot()
		} else {
			snapshot, e = householdenergy.DecodeDemoCostSnapshot(io.LimitReader(os.Stdin, 1<<20))
			if e != nil {
				return e
			}
		}
		if e = householdenergy.ImportDemoCostSnapshot(ctx, pool, *owner, *home, snapshot); e != nil {
			return e
		}
		result = map[string]string{"home_id": *home, "fixture_revision": snapshot.FixtureRevision, "status": "imported"}
	}
	if args[0] == "provision-hub" {
		local := fs.String("local-hub-id", "", "hub local ID")
		key := fs.String("public-key", "", "Ed25519 public key, unpadded base64url")
		actor := fs.String("approved-by", "", "administrator identifier")
		if e = fs.Parse(args[1:]); e != nil {
			return e
		}
		if fs.NArg() != 0 || strings.TrimSpace(*actor) == "" {
			return errors.New("local-hub-id, public-key and approved-by required")
		}
		pub, err := hubtrust.PublicKey(*key)
		if err != nil {
			return err
		}
		id, nonce, err := s.Provision(ctx, *local, pub, *actor)
		if err != nil {
			return err
		}
		result = map[string]string{"hub_id": id, "enrollment_nonce": nonce}
	}
	if args[0] == "issue-hub-enroll-nonce" {
		hub := fs.String("hub-id", "", "registered cloud hub UUID")
		if e = fs.Parse(args[1:]); e != nil {
			return e
		}
		if fs.NArg() != 0 || *hub == "" {
			return errors.New("hub-id required")
		}
		nonce, err := s.IssueEnrollmentNonce(ctx, *hub)
		if err != nil {
			return err
		}
		result = map[string]string{"hub_id": *hub, "enrollment_nonce": nonce}
	}
	return json.NewEncoder(os.Stdout).Encode(result)
}

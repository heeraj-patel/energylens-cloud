package main

import (
	"context"
	"encoding/json"
	"energylens.local/cloud/internal/account"
	"energylens.local/cloud/internal/catalogimport"
	"energylens.local/cloud/internal/datawizfacts"
	"energylens.local/cloud/internal/datawizrequests"
	"energylens.local/cloud/internal/energycategories"
	"energylens.local/cloud/internal/householdenergy"
	"energylens.local/cloud/internal/hubtrust"
	"energylens.local/cloud/internal/inventory"
	"energylens.local/cloud/internal/ratedinputimport"
	"energylens.local/cloud/internal/scenarioimport"
	"energylens.local/cloud/internal/usagefactimport"
	"errors"
	"flag"
	"fmt"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

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
	if len(args) > 0 && args[0] == "check-config" {
		if len(args) != 2 {
			return errors.New("usage: energylens-cloud check-config api|worker|migrate")
		}
		if args[1] == "migrate" {
			environment, err := migrationEnvironment()
			if err != nil {
				return errors.New("migration configuration rejected")
			}
			_, err = migrate.ValidateConfig(os.Getenv("ENERGYLENS_CLOUD_MIGRATION_DATABASE_URL"), environment)
			if err != nil {
				return errors.New("migration configuration rejected")
			}
			return nil
		}
		if args[1] != "api" && args[1] != "worker" {
			return errors.New("check-config role must be api, worker, or migrate")
		}
		cfg, err := app.LoadConfig(args[1])
		if err != nil {
			return errors.New("Cloud configuration rejected")
		}
		if cfg.LocalAuthEnabled && cfg.Role != app.RoleWorker {
			if err := app.CheckLocalAuthKeys(cfg, "", ""); err != nil {
				return errors.New("Cloud local auth key configuration rejected")
			}
		}
		return nil
	}
	if len(args) > 0 && args[0] == "export-unresolved-device-help" {
		return identityHelpExport(ctx, args[1:])
	}
	if len(args) > 0 && args[0] == "export-datawiz-requests" {
		return exportDatawizRequests(ctx, args[1:])
	}
	if len(args) > 0 && args[0] == "export-energy-categories" {
		return exportEnergyCategories(ctx, args[1:])
	}
	if len(args) > 0 && args[0] == "queue-datawiz-resolve" {
		return queueDatawizResolve(ctx, args[1:])
	}
	if len(args) > 0 && args[0] == "relink-catalog" {
		return relinkCatalog(ctx, args[1:])
	}
	if len(args) > 0 && args[0] == "read-datawiz-current" {
		fs := flag.NewFlagSet("read-datawiz-current", flag.ContinueOnError)
		route := fs.String("route", "", "device, zip, savings, or efficiency")
		subject := fs.String("subject", "", "normalized Datawiz subject key")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		if fs.NArg() != 0 || *subject == "" || (*route != "device" && *route != "zip" && *route != "savings" && *route != "efficiency") {
			return errors.New("usage: energylens-cloud read-datawiz-current -route device|zip|savings|efficiency -subject <key>")
		}
		dsn := os.Getenv("ENERGYLENS_CLOUD_DATABASE_URL")
		if dsn == "" {
			return errors.New("ENERGYLENS_CLOUD_DATABASE_URL required")
		}
		pool, err := pgxpool.New(ctx, dsn)
		if err != nil {
			return err
		}
		defer pool.Close()
		facts, err := (datawizfacts.Store{DB: pool}).Current(ctx, *route, *subject)
		if err != nil {
			return err
		}
		return json.NewEncoder(os.Stdout).Encode(facts)
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
	if len(args) > 0 && (args[0] == "adopt-dev-account-username" || args[0] == "unlock-dev-username-login" || args[0] == "provision-hub" || args[0] == "issue-hub-enroll-nonce" || args[0] == "import-reviewed-catalog" || args[0] == "import-reviewed-usage-facts" || args[0] == "import-reviewed-rated-input" || args[0] == "import-reviewed-action-scenarios" || args[0] == "import-official-tariff" || args[0] == "import-reviewed-program" || args[0] == "import-demo-cost-snapshot") {
		return operator(ctx, args)
	}
	if len(args) > 0 && args[0] == "migrate" {
		if len(args) != 1 {
			return errors.New("usage: energylens-cloud migrate")
		}
		environment, err := migrationEnvironment()
		if err != nil {
			return err
		}
		return migrate.Run(ctx, os.Getenv("ENERGYLENS_CLOUD_MIGRATION_DATABASE_URL"), environment)
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

func migrationEnvironment() (string, error) {
	environment, err := app.MigrationEnvironment()
	if err != nil {
		return "", err
	}
	if os.Getenv("RUNTIME_ENV") == "local" {
		if err := app.ValidateLocalDatabaseURL(os.Getenv("ENERGYLENS_CLOUD_MIGRATION_DATABASE_URL")); err != nil {
			return "", fmt.Errorf("ENERGYLENS_CLOUD_MIGRATION_DATABASE_URL: %w", err)
		}
	}
	return environment, nil
}

func exportDatawizRequests(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("export-datawiz-requests", flag.ContinueOnError)
	output := fs.String("output", "", "absolute private requests directory")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 || *output == "" || !filepath.IsAbs(*output) {
		return errors.New("usage: energylens-cloud export-datawiz-requests -output <absolute-directory>")
	}
	dsn := os.Getenv("ENERGYLENS_CLOUD_DATABASE_URL")
	if dsn == "" {
		return errors.New("ENERGYLENS_CLOUD_DATABASE_URL required")
	}
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return err
	}
	defer pool.Close()
	count, err := (datawizrequests.Producer{DB: pool, Dir: *output}).RunOnce(ctx)
	if err != nil {
		return err
	}
	return json.NewEncoder(os.Stdout).Encode(map[string]int{"requests_written": count})
}

func exportEnergyCategories(ctx context.Context, args []string) error {
	if len(args) != 0 {
		return errors.New("usage: energylens-cloud export-energy-categories")
	}
	dsn := os.Getenv("ENERGYLENS_CLOUD_DATABASE_URL")
	if dsn == "" {
		return errors.New("ENERGYLENS_CLOUD_DATABASE_URL required")
	}
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return err
	}
	defer pool.Close()
	body, _, err := (energycategories.PGStore{Pool: pool}).Export(ctx)
	if err != nil {
		return err
	}
	_, err = os.Stdout.Write(append(body, '\n'))
	return err
}

func queueDatawizResolve(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("queue-datawiz-resolve", flag.ContinueOnError)
	appliance := fs.String("appliance", "", "real or simulated appliance UUID")
	output := fs.String("output", "", "absolute private requests directory")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 || *appliance == "" || (*output != "" && !filepath.IsAbs(*output)) {
		return errors.New("usage: energylens-cloud queue-datawiz-resolve -appliance <uuid> [-output <absolute-directory>]")
	}
	if _, err := uuid.Parse(*appliance); err != nil {
		return errors.New("invalid appliance UUID")
	}
	dsn := os.Getenv("ENERGYLENS_CLOUD_DATABASE_URL")
	if dsn == "" {
		return errors.New("ENERGYLENS_CLOUD_DATABASE_URL required")
	}
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return err
	}
	defer pool.Close()
	p := datawizrequests.Producer{DB: pool, Dir: *output}
	found, err := p.Exists(ctx, *appliance)
	if err != nil {
		return err
	}
	if !found {
		return errors.New("appliance not found")
	}
	if err = datawizrequests.Queue(ctx, pool, *appliance, "admin"); err != nil {
		return err
	}
	count := 0
	if *output != "" {
		count, err = p.DrainQueuedAppliance(ctx, *appliance)
		if err != nil {
			return err
		}
	}
	return json.NewEncoder(os.Stdout).Encode(map[string]any{"queued": *appliance, "reason": "admin", "requests_written": count})
}

// relinkCatalog is the one-time pass that links existing appliances to catalog products.
func relinkCatalog(ctx context.Context, args []string) error {
	if len(args) != 0 {
		return errors.New("usage: energylens-cloud relink-catalog")
	}
	dsn := os.Getenv("ENERGYLENS_CLOUD_DATABASE_URL")
	if dsn == "" {
		return errors.New("ENERGYLENS_CLOUD_DATABASE_URL required")
	}
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return err
	}
	defer pool.Close()
	result, err := (inventory.PGStore{Pool: pool}).RelinkCatalog(ctx)
	if err != nil {
		return err
	}
	return json.NewEncoder(os.Stdout).Encode(map[string]int{"linked_exact": result.LinkedExact, "linked_family": result.LinkedFamily, "unresolved": result.Unresolved})
}

// Operational provisioning is intentionally outside the public HTTP surface.
// Access requires the dedicated migration/provisioning database credential.
func operator(ctx context.Context, args []string) error {
	if (args[0] == "adopt-dev-account-username" || args[0] == "unlock-dev-username-login") && os.Getenv("RUNTIME_ENV") != "local" {
		return errors.New("development account operator requires RUNTIME_ENV=local")
	}
	dsn := os.Getenv("ENERGYLENS_CLOUD_MIGRATION_DATABASE_URL")
	if dsn == "" {
		return errors.New("migration/provisioning database URL required")
	}
	env, e := migrationEnvironment()
	if e != nil {
		return e
	}
	cfg, e := pgxpool.ParseConfig(dsn)
	if e != nil {
		return e
	}
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
	if args[0] == "adopt-dev-account-username" {
		userID := fs.String("user-id", "", "existing verified development user UUID")
		username := fs.String("username", "", "new runtime-local username")
		passwordStdin := fs.Bool("password-stdin", false, "read the private password from standard input")
		if e = fs.Parse(args[1:]); e != nil {
			return e
		}
		if fs.NArg() != 0 || *userID == "" || *username == "" || !*passwordStdin {
			return errors.New("usage: adopt-dev-account-username --user-id UUID --username NAME --password-stdin")
		}
		raw, err := io.ReadAll(io.LimitReader(os.Stdin, 1026))
		if err != nil || len(raw) > 1025 {
			return errors.New("invalid private password input")
		}
		password := strings.TrimSuffix(string(raw), "\n")
		if err := account.AdoptDevAccountUsername(ctx, pool, *userID, *username, password, time.Now().UTC()); err != nil {
			return err
		}
		result = map[string]string{"user_id": *userID, "status": "adopted"}
	}
	if args[0] == "unlock-dev-username-login" {
		userID := fs.String("user-id", "", "approved development user UUID")
		username := fs.String("username", "", "approved development username")
		if e = fs.Parse(args[1:]); e != nil {
			return e
		}
		if fs.NArg() != 0 || *userID != account.DevLocalUserID || *username != "dev" {
			return errors.New("usage: unlock-dev-username-login --user-id APPROVED_DEV_UUID --username dev")
		}
		key, err := account.LoadLocalRateKey(os.Getenv("ENERGYLENS_CLOUD_LOCAL_AUTH_LOGIN_RATE_KEY_FILE"))
		if err != nil {
			return fmt.Errorf("load local login rate key: %w", err)
		}
		if err := account.UnlockDevUsernameLogin(ctx, pool, key, *userID, *username); err != nil {
			return err
		}
		result = map[string]string{"user_id": *userID, "status": "unlocked"}
	}
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
	if args[0] == "import-reviewed-rated-input" {
		artifact := fs.String("artifact", "", "signed reviewed rated-input publication JSON")
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
		verified, err := ratedinputimport.ReadAndVerify(*artifact, keyPath)
		if err != nil {
			return err
		}
		result, err = ratedinputimport.Import(ctx, pool, verified, *actor)
		if err != nil {
			return err
		}
	}
	if args[0] == "import-reviewed-action-scenarios" {
		artifact := fs.String("artifact", "", "signed reviewed action-scenario publication JSON")
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
		verified, err := scenarioimport.ReadAndVerify(*artifact, keyPath)
		if err != nil {
			return err
		}
		result, err = scenarioimport.Import(ctx, pool, verified, *actor)
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
		if e = fs.Parse(args[1:]); e != nil {
			return e
		}
		if fs.NArg() != 0 || *owner == "" || *home == "" {
			return errors.New("owner-id and home-id required; provide snapshot JSON on stdin")
		}
		snapshot, e := householdenergy.DecodeDemoCostSnapshot(io.LimitReader(os.Stdin, 1<<20))
		if e != nil {
			return e
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

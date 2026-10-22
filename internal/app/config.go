package app

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"energylens.local/cloud/internal/account"
	"energylens.local/cloud/internal/programs"
	"energylens.local/cloud/internal/reports"
	"energylens.local/cloud/internal/weather"
	"github.com/jackc/pgx/v5/pgxpool"
)

type Role string

const (
	RoleAPI    Role = "api"
	RoleWorker Role = "worker"
	RoleAll    Role = "all"
)

type Config struct {
	Role                         Role
	RuntimeEnvironment           string
	Environment                  string
	APIListen                    string
	HubListen                    string
	HubCertFile                  string
	HubKeyFile                   string
	HubCAFile                    string
	HubSignCertFile              string
	HubSignKeyFile               string
	DatabaseURL                  string
	WeatherOpenMeteoAPIKey       string
	WeatherCoarseCells           string
	OIDCIssuer                   string
	OIDCAudience                 string
	OIDCJWKSURL                  string
	MeteredExportCursorKeyFile   string
	MeteredExportPreviousKeyFile string
	MeteredRollupEnabled         bool
	KasaRuntimeRetentionEnabled  bool
	ReportRetentionEnabled       bool
	ReportDeliveryEnabled        bool
	ReportDirectSendEnabled      bool
	ResendAPIKey                 string
	ResendFrom                   string
	LocalAuthMailEnabled         bool
	LocalAuthResendAPIKey        string
	LocalAuthResendFrom          string
	LocalAuthLinkOrigin          string
	LocalAuthProxyCIDRs          string
	LocalAuthEnabled             bool
	DevAuthEnabled               bool
	DevAuthEmail                 string
	DevAuthPassword              string
	AccountDeletionEnabled       bool
	SampleFixturesEnabled        bool
	SampleFixtureUserIDs         string
	SampleFixtureHomeIDs         string
	LocalAuthActiveKeyID         string
	LocalAuthPrivateKey          string
	LocalAuthPublicKeys          string
	LocalAuthAudience            string
	LocalAuthEmailAudience       string
	LocalAuthRegisterKey         string
	LocalAuthLoginKey            string
	LocalAuthRecoveryKey         string
	DatawizRequestsDir           string
	ProgramRefreshEnabled        bool
	ProgramSourceProbeEnabled    bool
	OEDIIOUPath                  string
	OEDINonIOUPath               string
	OEDIIOUSHA256                string
	OEDINonIOUSHA256             string
	OEDIRetrievedAt              time.Time
}

func LoadConfig(role string) (Config, error) {
	c := Config{
		Role: Role(strings.TrimSpace(role)), Environment: strings.TrimSpace(os.Getenv("ENERGYLENS_CLOUD_ENV")),
		APIListen: os.Getenv("ENERGYLENS_CLOUD_BIND"), HubListen: os.Getenv("ENERGYLENS_CLOUD_HUB_BIND"),
		HubCertFile: os.Getenv("ENERGYLENS_CLOUD_TLS_CERT"), HubKeyFile: os.Getenv("ENERGYLENS_CLOUD_TLS_KEY"), HubCAFile: os.Getenv("ENERGYLENS_CLOUD_HUB_CLIENT_CA"), HubSignCertFile: os.Getenv("ENERGYLENS_CLOUD_HUB_SIGN_CERT"), HubSignKeyFile: os.Getenv("ENERGYLENS_CLOUD_HUB_SIGN_KEY"),
		DatabaseURL:            os.Getenv("ENERGYLENS_CLOUD_DATABASE_URL"),
		WeatherOpenMeteoAPIKey: os.Getenv("ENERGYLENS_CLOUD_OPEN_METEO_API_KEY"), WeatherCoarseCells: os.Getenv("ENERGYLENS_CLOUD_WEATHER_COARSE_CELLS"),
		OIDCIssuer: os.Getenv("ENERGYLENS_CLOUD_OIDC_ISSUER"), OIDCAudience: os.Getenv("ENERGYLENS_CLOUD_OIDC_AUDIENCE"), OIDCJWKSURL: os.Getenv("ENERGYLENS_CLOUD_OIDC_JWKS_URL"),
		MeteredExportCursorKeyFile: os.Getenv("ENERGYLENS_CLOUD_METERED_EXPORT_CURSOR_KEY_FILE"), MeteredExportPreviousKeyFile: os.Getenv("ENERGYLENS_CLOUD_METERED_EXPORT_PREVIOUS_CURSOR_KEY_FILE"),
		ResendAPIKey: os.Getenv("ENERGYLENS_CLOUD_RESEND_API_KEY"), ResendFrom: os.Getenv("ENERGYLENS_CLOUD_RESEND_FROM"),
		LocalAuthResendAPIKey: os.Getenv("ENERGYLENS_CLOUD_LOCAL_AUTH_RESEND_API_KEY"), LocalAuthResendFrom: os.Getenv("ENERGYLENS_CLOUD_LOCAL_AUTH_RESEND_FROM"), LocalAuthLinkOrigin: os.Getenv("ENERGYLENS_CLOUD_LOCAL_AUTH_LINK_ORIGIN"), LocalAuthProxyCIDRs: os.Getenv("ENERGYLENS_CLOUD_LOCAL_AUTH_TRUSTED_PROXY_CIDRS"),
		LocalAuthActiveKeyID: os.Getenv("ENERGYLENS_CLOUD_LOCAL_AUTH_ACTIVE_KEY_ID"), LocalAuthPrivateKey: os.Getenv("ENERGYLENS_CLOUD_LOCAL_AUTH_PRIVATE_KEY_FILE"), LocalAuthPublicKeys: os.Getenv("ENERGYLENS_CLOUD_LOCAL_AUTH_PUBLIC_KEY_FILES"), LocalAuthAudience: os.Getenv("ENERGYLENS_CLOUD_LOCAL_AUTH_AUDIENCE"),
		LocalAuthRegisterKey: os.Getenv("ENERGYLENS_CLOUD_LOCAL_AUTH_REGISTER_RATE_KEY_FILE"), LocalAuthLoginKey: os.Getenv("ENERGYLENS_CLOUD_LOCAL_AUTH_LOGIN_RATE_KEY_FILE"), LocalAuthRecoveryKey: os.Getenv("ENERGYLENS_CLOUD_LOCAL_AUTH_RECOVERY_RATE_KEY_FILE"),
		DevAuthEmail: os.Getenv("ENERGYLENS_CLOUD_DEV_AUTH_EMAIL"), DevAuthPassword: os.Getenv("ENERGYLENS_CLOUD_DEV_AUTH_PASSWORD"),
		SampleFixtureUserIDs: os.Getenv("ENERGYLENS_CLOUD_SAMPLE_FIXTURE_USER_IDS"),
		SampleFixtureHomeIDs: os.Getenv("ENERGYLENS_CLOUD_SAMPLE_FIXTURE_HOME_IDS"),
		OEDIIOUPath:          os.Getenv("ENERGYLENS_CLOUD_OEDI_IOU_CSV"), OEDINonIOUPath: os.Getenv("ENERGYLENS_CLOUD_OEDI_NON_IOU_CSV"), OEDIIOUSHA256: os.Getenv("ENERGYLENS_CLOUD_OEDI_IOU_SHA256"), OEDINonIOUSHA256: os.Getenv("ENERGYLENS_CLOUD_OEDI_NON_IOU_SHA256"),
		DatawizRequestsDir: os.Getenv("ENERGYLENS_CLOUD_DATAWIZ_REQUESTS_DIR"),
	}
	if raw := os.Getenv("ENERGYLENS_CLOUD_PROGRAM_REFRESH_ENABLED"); raw != "" {
		enabled, err := strconv.ParseBool(raw)
		if err != nil {
			return Config{}, fmt.Errorf("invalid ENERGYLENS_CLOUD_PROGRAM_REFRESH_ENABLED: %w", err)
		}
		c.ProgramRefreshEnabled = enabled
	}
	if raw := os.Getenv("ENERGYLENS_CLOUD_PROGRAM_SOURCE_PROBE_ENABLED"); raw != "" {
		enabled, err := strconv.ParseBool(raw)
		if err != nil {
			return Config{}, fmt.Errorf("invalid ENERGYLENS_CLOUD_PROGRAM_SOURCE_PROBE_ENABLED: %w", err)
		}
		c.ProgramSourceProbeEnabled = enabled
	}
	if raw := os.Getenv("ENERGYLENS_CLOUD_OEDI_RETRIEVED_AT"); raw != "" {
		parsed, err := time.Parse(time.RFC3339, raw)
		if err != nil {
			return Config{}, fmt.Errorf("invalid OEDI retrieval timestamp: %w", err)
		}
		c.OEDIRetrievedAt = parsed.UTC()
	}
	if raw := os.Getenv("ENERGYLENS_CLOUD_METERED_ROLLUP_ENABLED"); raw != "" {
		enabled, err := strconv.ParseBool(raw)
		if err != nil {
			return Config{}, fmt.Errorf("invalid ENERGYLENS_CLOUD_METERED_ROLLUP_ENABLED %q: %w", raw, err)
		}
		c.MeteredRollupEnabled = enabled
	}
	if raw := os.Getenv("ENERGYLENS_CLOUD_REPORT_RETENTION_ENABLED"); raw != "" {
		enabled, err := strconv.ParseBool(raw)
		if err != nil {
			return Config{}, fmt.Errorf("invalid ENERGYLENS_CLOUD_REPORT_RETENTION_ENABLED %q: %w", raw, err)
		}
		c.ReportRetentionEnabled = enabled
	}
	if raw := os.Getenv("ENERGYLENS_CLOUD_KASA_RUNTIME_RETENTION_ENABLED"); raw != "" {
		enabled, err := strconv.ParseBool(raw)
		if err != nil {
			return Config{}, fmt.Errorf("invalid ENERGYLENS_CLOUD_KASA_RUNTIME_RETENTION_ENABLED %q: %w", raw, err)
		}
		c.KasaRuntimeRetentionEnabled = enabled
	}
	if raw := os.Getenv("ENERGYLENS_CLOUD_REPORT_DELIVERY_ENABLED"); raw != "" {
		enabled, err := strconv.ParseBool(raw)
		if err != nil {
			return Config{}, fmt.Errorf("invalid ENERGYLENS_CLOUD_REPORT_DELIVERY_ENABLED %q: %w", raw, err)
		}
		c.ReportDeliveryEnabled = enabled
	}
	if raw := os.Getenv("ENERGYLENS_CLOUD_REPORT_DIRECT_SEND_ENABLED"); raw != "" {
		enabled, err := strconv.ParseBool(raw)
		if err != nil {
			return Config{}, fmt.Errorf("invalid ENERGYLENS_CLOUD_REPORT_DIRECT_SEND_ENABLED %q: %w", raw, err)
		}
		c.ReportDirectSendEnabled = enabled
	}
	if raw := os.Getenv("ENERGYLENS_CLOUD_LOCAL_AUTH_MAIL_ENABLED"); raw != "" {
		enabled, err := strconv.ParseBool(raw)
		if err != nil {
			return Config{}, fmt.Errorf("invalid ENERGYLENS_CLOUD_LOCAL_AUTH_MAIL_ENABLED: %w", err)
		}
		c.LocalAuthMailEnabled = enabled
	}
	if raw := os.Getenv("ENERGYLENS_CLOUD_LOCAL_AUTH_ENABLED"); raw != "" {
		enabled, err := strconv.ParseBool(raw)
		if err != nil {
			return Config{}, fmt.Errorf("invalid ENERGYLENS_CLOUD_LOCAL_AUTH_ENABLED: %w", err)
		}
		c.LocalAuthEnabled = enabled
	}
	if raw := os.Getenv("ENERGYLENS_CLOUD_DEV_AUTH_ENABLED"); raw != "" {
		enabled, err := strconv.ParseBool(raw)
		if err != nil {
			return Config{}, fmt.Errorf("invalid ENERGYLENS_CLOUD_DEV_AUTH_ENABLED: %w", err)
		}
		c.DevAuthEnabled = enabled
	}
	if raw := os.Getenv("ENERGYLENS_CLOUD_ACCOUNT_DELETION_ENABLED"); raw != "" {
		enabled, err := strconv.ParseBool(raw)
		if err != nil {
			return Config{}, fmt.Errorf("invalid ENERGYLENS_CLOUD_ACCOUNT_DELETION_ENABLED: %w", err)
		}
		c.AccountDeletionEnabled = enabled
	}
	if raw := os.Getenv("ENERGYLENS_CLOUD_SAMPLE_FIXTURES_ENABLED"); raw != "" {
		enabled, err := strconv.ParseBool(raw)
		if err != nil {
			return Config{}, fmt.Errorf("invalid ENERGYLENS_CLOUD_SAMPLE_FIXTURES_ENABLED: %w", err)
		}
		c.SampleFixturesEnabled = enabled
	}
	if raw, set := os.LookupEnv("RUNTIME_ENV"); set {
		if raw == "" || raw != strings.TrimSpace(raw) {
			return Config{}, errors.New("invalid RUNTIME_ENV")
		}
		c.RuntimeEnvironment = raw
		switch raw {
		case "local":
			if c.Environment != "" && c.Environment != "dev" {
				return Config{}, errors.New("RUNTIME_ENV conflicts with ENERGYLENS_CLOUD_ENV")
			}
			if _, set := os.LookupEnv("ENERGYLENS_CLOUD_LOCAL_AUTH_ENABLED"); set && !c.LocalAuthEnabled {
				return Config{}, errors.New("RUNTIME_ENV conflicts with ENERGYLENS_CLOUD_LOCAL_AUTH_ENABLED")
			}
			c.LocalAuthEnabled = true
			c.LocalAuthEmailAudience = c.LocalAuthAudience
			c.LocalAuthAudience = os.Getenv("ENERGYLENS_CLOUD_RUNTIME_LOCAL_AUTH_AUDIENCE")
		case "dev", "test", "staging", "production":
			if c.Environment != "" && c.Environment != raw {
				return Config{}, errors.New("RUNTIME_ENV conflicts with ENERGYLENS_CLOUD_ENV")
			}
			c.Environment = raw
		default:
			return Config{}, errors.New("invalid RUNTIME_ENV")
		}
	}
	if c.DevAuthEnabled && c.Environment == "" && c.RuntimeEnvironment != "local" {
		return Config{}, errors.New("development auth requires explicit ENERGYLENS_CLOUD_ENV=dev")
	}
	if c.Environment == "" {
		c.Environment = "dev"
	}
	if c.APIListen == "" {
		c.APIListen = "127.0.0.1:18092"
	}
	// The old names remain accepted for deployments during the single-port
	// migration. They configure the same server certificate, never a listener.
	if c.HubCertFile == "" {
		c.HubCertFile = os.Getenv("ENERGYLENS_CLOUD_HUB_TLS_CERT")
	}
	if c.HubKeyFile == "" {
		c.HubKeyFile = os.Getenv("ENERGYLENS_CLOUD_HUB_TLS_KEY")
	}
	return c, c.Validate()
}

func (c Config) Validate() error {
	switch c.RuntimeEnvironment {
	case "", "local", "dev", "test", "staging", "production":
	default:
		return errors.New("invalid RUNTIME_ENV")
	}
	if c.RuntimeEnvironment == "local" {
		if c.Role != RoleWorker && (c.LocalAuthAudience == "" || c.LocalAuthAudience == c.LocalAuthEmailAudience) {
			return errors.New("RUNTIME_ENV=local requires distinct ENERGYLENS_CLOUD_RUNTIME_LOCAL_AUTH_AUDIENCE")
		}
		if c.Environment != "dev" || !c.LocalAuthEnabled || c.DevAuthEnabled || c.DevAuthEmail != "" || c.DevAuthPassword != "" || c.OIDCIssuer != "" || c.OIDCAudience != "" || c.OIDCJWKSURL != "" || c.LocalAuthMailEnabled || c.LocalAuthResendAPIKey != "" || c.LocalAuthResendFrom != "" || c.LocalAuthLinkOrigin != "" {
			return errors.New("RUNTIME_ENV=local conflicts with Cloud environment or auth provider settings")
		}
		if c.DatabaseURL == "" {
			return errors.New("RUNTIME_ENV=local requires ENERGYLENS_CLOUD_DATABASE_URL")
		}
		if c.Role != RoleWorker && (c.HubCertFile == "" || c.HubKeyFile == "" || c.HubCAFile == "") {
			return errors.New("RUNTIME_ENV=local requires ENERGYLENS_CLOUD_TLS_CERT, ENERGYLENS_CLOUD_TLS_KEY and ENERGYLENS_CLOUD_HUB_CLIENT_CA")
		}
	} else if c.RuntimeEnvironment != "" && c.Environment != c.RuntimeEnvironment {
		return errors.New("RUNTIME_ENV conflicts with ENERGYLENS_CLOUD_ENV")
	}
	if c.DatawizRequestsDir != "" {
		if c.DatabaseURL == "" || c.Role == RoleAPI || !filepath.IsAbs(c.DatawizRequestsDir) {
			return errors.New("Datawiz requests require worker/all role, database, and absolute requests directory")
		}
	}
	if c.SampleFixturesEnabled {
		if c.Environment != "dev" && c.Environment != "test" {
			return errors.New("sample fixtures require dev or test environment")
		}
		if c.Role != RoleAPI && c.Role != RoleAll || c.DatabaseURL == "" || strings.TrimSpace(c.SampleFixtureUserIDs) == "" || strings.TrimSpace(c.SampleFixtureHomeIDs) == "" {
			return errors.New("sample fixtures require API, database, and explicit user and home allowlists")
		}
	}
	if (c.SampleFixtureUserIDs != "" || c.SampleFixtureHomeIDs != "") && !c.SampleFixturesEnabled {
		return errors.New("sample fixture allowlists require fixture gate")
	}
	if c.DevAuthEnabled || c.DevAuthEmail != "" || c.DevAuthPassword != "" {
		if !c.DevAuthEnabled || c.Environment != "dev" || c.Role != RoleAPI && c.Role != RoleAll || c.DatabaseURL == "" || c.OIDCIssuer != "" || c.LocalAuthEnabled || c.LocalAuthMailEnabled {
			return errors.New("development auth requires dev API with database and no other owner authentication")
		}
		if c.DevAuthEmail == "" || c.DevAuthEmail != strings.TrimSpace(c.DevAuthEmail) || !strings.Contains(c.DevAuthEmail, "@") || len(c.DevAuthEmail) > 254 || len(c.DevAuthPassword) < 12 || len(c.DevAuthPassword) > 256 {
			return errors.New("development auth requires fixed email and 12-256 character password")
		}
	}
	if (c.WeatherOpenMeteoAPIKey == "") != (c.WeatherCoarseCells == "") {
		return errors.New("commercial weather API key and coarse cell map must be configured together")
	}
	if c.WeatherCoarseCells != "" {
		if _, err := weather.ParseCoarseCells(c.WeatherCoarseCells); err != nil {
			return fmt.Errorf("invalid weather coarse cell map: %w", err)
		}
	}
	switch c.Role {
	case RoleAPI, RoleWorker, RoleAll:
	default:
		return fmt.Errorf("invalid role %q (want api, worker, or all)", c.Role)
	}
	switch c.Environment {
	case "dev", "test", "staging", "production":
	default:
		return fmt.Errorf("invalid ENERGYLENS_CLOUD_ENV %q", c.Environment)
	}
	if c.APIListen == "" {
		return errors.New("API listen address required")
	}
	if c.MeteredExportPreviousKeyFile != "" && c.MeteredExportCursorKeyFile == "" {
		return errors.New("metered export previous cursor key requires active key")
	}
	for _, path := range []string{c.MeteredExportCursorKeyFile, c.MeteredExportPreviousKeyFile} {
		if path != "" && !filepath.IsAbs(path) {
			return errors.New("metered export cursor key paths must be absolute")
		}
	}
	for _, path := range []string{c.MeteredExportCursorKeyFile, c.MeteredExportPreviousKeyFile} {
		if path != "" && (path == c.LocalAuthPrivateKey || path == c.HubSignKeyFile || path == c.LocalAuthRegisterKey || path == c.LocalAuthLoginKey || path == c.LocalAuthRecoveryKey) {
			return errors.New("metered export cursor keys must use dedicated files")
		}
	}
	if c.MeteredExportCursorKeyFile != "" && c.MeteredExportCursorKeyFile == c.MeteredExportPreviousKeyFile {
		return errors.New("metered export active and previous cursor keys must use separate files")
	}
	if c.DatabaseURL != "" && (c.Role == RoleAPI || c.Role == RoleAll) && (c.OIDCIssuer != "" || c.LocalAuthEnabled) && c.MeteredExportCursorKeyFile == "" && (c.Environment == "production" || c.Environment == "staging") {
		return errors.New("owner API requires ENERGYLENS_CLOUD_METERED_EXPORT_CURSOR_KEY_FILE")
	}
	if c.AccountDeletionEnabled {
		if c.Environment == "production" {
			return errors.New("production account deletion requires an independently backed-up deletion journal gate")
		}
		if c.Role != RoleAPI && c.Role != RoleAll || c.DatabaseURL == "" || c.OIDCIssuer == "" && !c.LocalAuthEnabled {
			return errors.New("account deletion requires API role, database and owner authentication")
		}
	}
	if c.LocalAuthProxyCIDRs != "" {
		if c.Role != RoleAPI && c.Role != RoleAll {
			return errors.New("local auth trusted proxies require API or all role")
		}
		if _, err := account.ParseLocalAuthSourcePolicy(c.LocalAuthProxyCIDRs); err != nil {
			return err
		}
	}
	if c.LocalAuthEnabled && (c.Role == RoleAPI || c.Role == RoleAll) {
		if c.DatabaseURL == "" || c.RuntimeEnvironment != "local" && !c.LocalAuthMailEnabled {
			return errors.New("local auth requires database and enabled local mail outside RUNTIME_ENV=local")
		}
		if c.LocalAuthAudience == "" || c.LocalAuthAudience != strings.TrimSpace(c.LocalAuthAudience) || len(c.LocalAuthAudience) > 256 || c.LocalAuthActiveKeyID == "" || c.LocalAuthPrivateKey == "" || c.LocalAuthRegisterKey == "" || c.LocalAuthLoginKey == "" || c.RuntimeEnvironment != "local" && c.LocalAuthRecoveryKey == "" {
			return errors.New("local auth signing and rate key configuration incomplete")
		}
		paths, err := c.localAuthKeyFiles()
		if err != nil || len(paths.PublicPaths) == 0 {
			return errors.New("invalid local auth public key configuration")
		}
		for _, path := range []string{c.LocalAuthPrivateKey, c.LocalAuthRegisterKey, c.LocalAuthLoginKey, c.LocalAuthRecoveryKey} {
			if path == "" {
				continue
			}
			if !filepath.IsAbs(path) {
				return errors.New("local auth key paths must be absolute")
			}
		}
		if c.LocalAuthRegisterKey == c.LocalAuthLoginKey || c.LocalAuthRecoveryKey != "" && (c.LocalAuthRegisterKey == c.LocalAuthRecoveryKey || c.LocalAuthLoginKey == c.LocalAuthRecoveryKey) {
			return errors.New("local auth rate keys must use separate files")
		}
	} else if c.LocalAuthEnabled && c.RuntimeEnvironment != "local" {
		return errors.New("local auth requires API role")
	}
	if c.ProgramSourceProbeEnabled && !c.ProgramRefreshEnabled {
		return errors.New("program source probe requires program refresh")
	}
	if c.ProgramRefreshEnabled {
		if c.Role != RoleWorker && c.Role != RoleAll {
			return errors.New("program refresh requires worker or all role")
		}
		if c.DatabaseURL == "" || c.OEDIIOUPath == "" || c.OEDINonIOUPath == "" || c.OEDIIOUSHA256 == "" || c.OEDINonIOUSHA256 == "" || c.OEDIRetrievedAt.IsZero() {
			return errors.New("program refresh requires database and both full OEDI CSVs with SHA-256 and retrieval time")
		}
		if !filepath.IsAbs(c.OEDIIOUPath) || !filepath.IsAbs(c.OEDINonIOUPath) || filepath.Base(c.OEDIIOUPath) != "iou_zipcodes_2024.csv" || filepath.Base(c.OEDINonIOUPath) != "non_iou_zipcodes_2024.csv" {
			return errors.New("program refresh requires absolute full OEDI CSV paths")
		}
		// Validate both complete operator files before starting a worker that
		// could otherwise claim private requests and fail every lookup.
		resolver := programs.OEDIZIPResolver{IOUPath: c.OEDIIOUPath, NonIOUPath: c.OEDINonIOUPath, IOUSHA256: c.OEDIIOUSHA256, NonIOUSHA256: c.OEDINonIOUSHA256, RetrievedAt: c.OEDIRetrievedAt}
		if _, err := resolver.ResolveZIPDetailed(context.Background(), "US", "00000"); err != nil {
			return fmt.Errorf("invalid OEDI program refresh files: %w", err)
		}
	}
	if c.MeteredRollupEnabled {
		if c.Role != RoleWorker && c.Role != RoleAll {
			return errors.New("metered rollup requires worker or all role")
		}
		if c.DatabaseURL == "" {
			return errors.New("metered rollup requires ENERGYLENS_CLOUD_DATABASE_URL")
		}
	}
	if c.ReportRetentionEnabled {
		if c.Role != RoleWorker && c.Role != RoleAll {
			return errors.New("report retention requires worker or all role")
		}
		if c.DatabaseURL == "" {
			return errors.New("report retention requires ENERGYLENS_CLOUD_DATABASE_URL")
		}
	}
	if c.KasaRuntimeRetentionEnabled {
		if c.Role != RoleWorker && c.Role != RoleAll {
			return errors.New("Kasa runtime retention requires worker or all role")
		}
		if c.DatabaseURL == "" {
			return errors.New("Kasa runtime retention requires ENERGYLENS_CLOUD_DATABASE_URL")
		}
	}
	if c.ResendAPIKey != "" || c.ResendFrom != "" {
		if c.Role == RoleAPI && !c.ReportDirectSendEnabled {
			return errors.New("API report provider configuration requires direct send enabled")
		}
		if c.ResendAPIKey == "" || c.ResendFrom == "" || c.DatabaseURL == "" {
			return errors.New("report provider requires API key, sender and database")
		}
		if _, err := reports.NewResendProvider(c.ResendAPIKey, c.ResendFrom); err != nil {
			return err
		}
	}
	if c.ReportDeliveryEnabled {
		if c.Role != RoleWorker && c.Role != RoleAll {
			return errors.New("report delivery requires worker or all role")
		}
		if c.DatabaseURL == "" || c.ResendAPIKey == "" || c.ResendFrom == "" {
			return errors.New("report delivery requires database, Resend API key and sender")
		}
	}
	if c.ReportDirectSendEnabled {
		if c.Role != RoleAPI && c.Role != RoleAll {
			return errors.New("direct report send requires API or all role")
		}
		if c.DatabaseURL == "" || c.ResendAPIKey == "" || c.ResendFrom == "" {
			return errors.New("direct report send requires database, Resend API key and sender")
		}
	}
	if c.LocalAuthMailEnabled || c.LocalAuthResendAPIKey != "" || c.LocalAuthResendFrom != "" || c.LocalAuthLinkOrigin != "" {
		if c.Role != RoleAPI && c.Role != RoleAll {
			return errors.New("local auth mail requires API or all role")
		}
		if c.DatabaseURL == "" || c.LocalAuthResendAPIKey == "" || c.LocalAuthResendFrom == "" || c.LocalAuthLinkOrigin == "" {
			return errors.New("local auth mail requires database, dedicated API key, sender and HTTPS link origin")
		}
		if _, err := account.NewResendLocalSender(c.LocalAuthResendAPIKey, c.LocalAuthResendFrom, c.LocalAuthLinkOrigin); err != nil {
			return err
		}
	}
	if c.Environment == "production" && c.Role == RoleAll {
		return errors.New("all role is for development only")
	}
	if c.Environment != "dev" && c.Environment != "test" && c.DatabaseURL == "" {
		return errors.New("ENERGYLENS_CLOUD_DATABASE_URL required outside development")
	}
	configured := c.OIDCIssuer != "" || c.OIDCAudience != "" || c.OIDCJWKSURL != ""
	if configured && (c.OIDCIssuer == "" || c.OIDCAudience == "" || c.OIDCJWKSURL == "") {
		return errors.New("OIDC issuer, audience, and JWKS URL must be configured together")
	}
	if configured && (c.OIDCIssuer == account.LocalIssuer || c.OIDCIssuer == account.LocalUsernameIssuer || c.OIDCIssuer != strings.TrimSpace(c.OIDCIssuer) || c.OIDCAudience != strings.TrimSpace(c.OIDCAudience) || c.OIDCAudience == "") {
		return errors.New("OIDC issuer and audience must be distinct and canonical")
	}
	if configured && c.LocalAuthEnabled && c.OIDCAudience == c.LocalAuthAudience {
		return errors.New("OIDC and local auth audiences must differ")
	}
	if c.Environment == "production" && c.Role != RoleWorker && !configured && !c.LocalAuthEnabled {
		return errors.New("production API requires OIDC or enabled local auth")
	}
	if configured && c.Environment != "dev" && c.Environment != "test" {
		if !strings.HasPrefix(c.OIDCIssuer, "https://") || !strings.HasPrefix(c.OIDCJWKSURL, "https://") {
			return errors.New("OIDC issuer and JWKS URL must use HTTPS")
		}
	}
	if c.DatabaseURL != "" {
		cfg, err := pgxpool.ParseConfig(c.DatabaseURL)
		if err != nil {
			if c.RuntimeEnvironment == "local" {
				return errors.New("invalid ENERGYLENS_CLOUD_DATABASE_URL")
			}
			return fmt.Errorf("invalid database configuration: %w", err)
		}
		if c.RuntimeEnvironment == "local" {
			if err := validateLocalDatabaseEndpoint(c.DatabaseURL, cfg); err != nil {
				return err
			}
		}
		if c.Environment != "dev" && c.Environment != "test" {
			if cfg.ConnConfig.TLSConfig == nil || cfg.ConnConfig.TLSConfig.ServerName == "" || cfg.ConnConfig.TLSConfig.InsecureSkipVerify {
				return errors.New("database must use certificate-verified TLS (sslmode=verify-full)")
			}
			for _, fallback := range cfg.ConnConfig.Fallbacks {
				if fallback.TLSConfig == nil || fallback.TLSConfig.InsecureSkipVerify {
					return errors.New("database plaintext or unverified fallback is forbidden")
				}
			}
		}
	}
	if c.HubListen != "" {
		return errors.New("ENERGYLENS_CLOUD_HUB_BIND is obsolete; all routes use ENERGYLENS_CLOUD_BIND")
	}
	if c.Role != RoleWorker && c.Environment != "dev" && c.Environment != "test" && (c.HubCertFile == "" || c.HubKeyFile == "" || c.HubCAFile == "") {
		return errors.New("Cloud TLS requires server certificate, key, and hub client CA")
	}
	if c.Environment == "production" && c.Role != RoleWorker && c.HubSignCertFile == "" {
		return errors.New("production API requires hub signing certificate and key")
	}
	if c.HubSignCertFile != "" && c.HubCAFile == "" {
		return errors.New("hub signing requires trusted client CA")
	}
	if (c.HubSignCertFile == "") != (c.HubSignKeyFile == "") {
		return errors.New("hub signing certificate and key must be configured together")
	}
	if c.HubCertFile != "" || c.HubKeyFile != "" || c.HubCAFile != "" {
		if c.Role == RoleWorker || c.HubCertFile == "" || c.HubKeyFile == "" || c.HubCAFile == "" {
			return errors.New("Cloud TLS requires API role, server certificate, key, and hub client CA")
		}
	}
	return nil
}

// BuildLocalAuthSender is the runtime construction gate for local-auth mail.
// Disabled configuration has no provider or network client.
func (c Config) BuildLocalAuthSender() (*account.ResendLocalSender, error) {
	if !c.LocalAuthMailEnabled {
		return nil, nil
	}
	if c.Role != RoleAPI && c.Role != RoleAll || c.DatabaseURL == "" {
		return nil, errors.New("local auth mail is unavailable for this role")
	}
	return account.NewResendLocalSender(c.LocalAuthResendAPIKey, c.LocalAuthResendFrom, c.LocalAuthLinkOrigin)
}

func (c Config) LocalAuthSourcePolicy() (account.LocalAuthSourcePolicy, error) {
	return account.ParseLocalAuthSourcePolicy(c.LocalAuthProxyCIDRs)
}

func (c Config) localAuthKeyFiles() (account.LocalKeyFiles, error) {
	if len(c.LocalAuthPublicKeys) > 4096 {
		return account.LocalKeyFiles{}, errors.New("local auth public key configuration too long")
	}
	var paths map[string]string
	if err := json.Unmarshal([]byte(c.LocalAuthPublicKeys), &paths); err != nil || len(paths) == 0 || len(paths) > 8 {
		return account.LocalKeyFiles{}, errors.New("invalid local auth public key configuration")
	}
	for _, path := range paths {
		if !filepath.IsAbs(path) {
			return account.LocalKeyFiles{}, errors.New("local auth public key paths must be absolute")
		}
	}
	profile := account.LocalEmailProfile
	if c.RuntimeEnvironment == "local" {
		profile = account.LocalUsernameProfile
	}
	return account.LocalKeyFiles{ActiveKeyID: c.LocalAuthActiveKeyID, PrivatePath: c.LocalAuthPrivateKey, PublicPaths: paths, Audience: c.LocalAuthAudience, Profile: profile}, nil
}

// ValidateLocalDatabaseURL applies the same endpoint boundary to a separately
// configured migration credential without loading API keys or connecting.
func ValidateLocalDatabaseURL(raw string) error {
	cfg, err := pgxpool.ParseConfig(raw)
	if err != nil {
		return errors.New("invalid local PostgreSQL configuration")
	}
	return validateLocalDatabaseEndpoint(raw, cfg)
}

func validateLocalDatabaseEndpoint(raw string, cfg *pgxpool.Config) error {
	if !explicitLocalDatabaseHost(raw) || !privateLocalDatabaseHost(cfg.ConnConfig.Host) {
		return errors.New("RUNTIME_ENV=local requires an explicitly configured local/private PostgreSQL host or Unix socket")
	}
	for _, fallback := range cfg.ConnConfig.Fallbacks {
		if !privateLocalDatabaseHost(fallback.Host) {
			return errors.New("RUNTIME_ENV=local requires all PostgreSQL fallback hosts to be local/private")
		}
	}
	return nil
}

func privateLocalDatabaseHost(host string) bool {
	if strings.HasPrefix(host, "/") || host == "localhost" || strings.HasSuffix(host, ".local") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && (ip.IsLoopback() || ip.IsPrivate())
}

func explicitLocalDatabaseHost(raw string) bool {
	if strings.HasPrefix(raw, "postgres://") || strings.HasPrefix(raw, "postgresql://") {
		parsed, err := url.Parse(raw)
		return err == nil && (parsed.Hostname() != "" || parsed.Query().Get("host") != "")
	}
	for _, field := range strings.Fields(raw) {
		if strings.HasPrefix(field, "host=") && len(field) > len("host=") {
			return true
		}
	}
	return false
}

// MigrationEnvironment maps explicit runtime selection onto the existing
// migration role policy without requiring API-only settings or keys.
func MigrationEnvironment() (string, error) {
	legacy := strings.TrimSpace(os.Getenv("ENERGYLENS_CLOUD_ENV"))
	runtime, set := os.LookupEnv("RUNTIME_ENV")
	if !set {
		return legacy, nil
	}
	if runtime == "" || runtime != strings.TrimSpace(runtime) {
		return "", errors.New("invalid RUNTIME_ENV")
	}
	if runtime == "local" {
		if legacy != "" && legacy != "dev" {
			return "", errors.New("RUNTIME_ENV conflicts with ENERGYLENS_CLOUD_ENV")
		}
		return "dev", nil
	}
	switch runtime {
	case "dev", "test", "staging", "production":
		if legacy != "" && legacy != runtime {
			return "", errors.New("RUNTIME_ENV conflicts with ENERGYLENS_CLOUD_ENV")
		}
		return runtime, nil
	default:
		return "", errors.New("invalid RUNTIME_ENV")
	}
}

func (c Config) hubTLSConfig() (*tls.Config, error) {
	pair, err := tls.LoadX509KeyPair(c.HubCertFile, c.HubKeyFile)
	if err != nil {
		return nil, fmt.Errorf("load hub server certificate: %w", err)
	}
	data, err := os.ReadFile(c.HubCAFile)
	if err != nil {
		return nil, fmt.Errorf("read hub client CA: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(data) {
		return nil, errors.New("hub client CA contains no certificates")
	}
	return &tls.Config{MinVersion: tls.VersionTLS12, Certificates: []tls.Certificate{pair}, ClientAuth: tls.VerifyClientCertIfGiven, ClientCAs: pool}, nil
}

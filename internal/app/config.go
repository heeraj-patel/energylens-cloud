package app

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"energylens.local/cloud/internal/account"
	"energylens.local/cloud/internal/programs"
	"energylens.local/cloud/internal/reports"
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
	Environment                  string
	APIListen                    string
	HubListen                    string
	HubCertFile                  string
	HubKeyFile                   string
	HubCAFile                    string
	HubSignCertFile              string
	HubSignKeyFile               string
	DatabaseURL                  string
	EIAAPIKey                    string
	OIDCIssuer                   string
	OIDCAudience                 string
	OIDCJWKSURL                  string
	MeteredExportCursorKeyFile   string
	MeteredExportPreviousKeyFile string
	MeteredRollupEnabled         bool
	KasaRuntimeRetentionEnabled  bool
	ReportRetentionEnabled       bool
	ReportDeliveryEnabled        bool
	ResendAPIKey                 string
	ResendFrom                   string
	LocalAuthMailEnabled         bool
	LocalAuthResendAPIKey        string
	LocalAuthResendFrom          string
	LocalAuthLinkOrigin          string
	LocalAuthProxyCIDRs          string
	LocalAuthEnabled             bool
	AccountDeletionEnabled       bool
	LocalAuthActiveKeyID         string
	LocalAuthPrivateKey          string
	LocalAuthPublicKeys          string
	LocalAuthAudience            string
	LocalAuthRegisterKey         string
	LocalAuthLoginKey            string
	LocalAuthRecoveryKey         string
	KBScanPython                 string
	KBScanSources                string
	KBScanCandidateIndex         string
	KBScanAllowEPA               bool
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
		HubCertFile: os.Getenv("ENERGYLENS_CLOUD_HUB_TLS_CERT"), HubKeyFile: os.Getenv("ENERGYLENS_CLOUD_HUB_TLS_KEY"), HubCAFile: os.Getenv("ENERGYLENS_CLOUD_HUB_CLIENT_CA"), HubSignCertFile: os.Getenv("ENERGYLENS_CLOUD_HUB_SIGN_CERT"), HubSignKeyFile: os.Getenv("ENERGYLENS_CLOUD_HUB_SIGN_KEY"),
		DatabaseURL: os.Getenv("ENERGYLENS_CLOUD_DATABASE_URL"), EIAAPIKey: os.Getenv("EIA_API_KEY"),
		OIDCIssuer: os.Getenv("ENERGYLENS_CLOUD_OIDC_ISSUER"), OIDCAudience: os.Getenv("ENERGYLENS_CLOUD_OIDC_AUDIENCE"), OIDCJWKSURL: os.Getenv("ENERGYLENS_CLOUD_OIDC_JWKS_URL"),
		MeteredExportCursorKeyFile: os.Getenv("ENERGYLENS_CLOUD_METERED_EXPORT_CURSOR_KEY_FILE"), MeteredExportPreviousKeyFile: os.Getenv("ENERGYLENS_CLOUD_METERED_EXPORT_PREVIOUS_CURSOR_KEY_FILE"),
		ResendAPIKey: os.Getenv("ENERGYLENS_CLOUD_RESEND_API_KEY"), ResendFrom: os.Getenv("ENERGYLENS_CLOUD_RESEND_FROM"),
		LocalAuthResendAPIKey: os.Getenv("ENERGYLENS_CLOUD_LOCAL_AUTH_RESEND_API_KEY"), LocalAuthResendFrom: os.Getenv("ENERGYLENS_CLOUD_LOCAL_AUTH_RESEND_FROM"), LocalAuthLinkOrigin: os.Getenv("ENERGYLENS_CLOUD_LOCAL_AUTH_LINK_ORIGIN"), LocalAuthProxyCIDRs: os.Getenv("ENERGYLENS_CLOUD_LOCAL_AUTH_TRUSTED_PROXY_CIDRS"),
		LocalAuthActiveKeyID: os.Getenv("ENERGYLENS_CLOUD_LOCAL_AUTH_ACTIVE_KEY_ID"), LocalAuthPrivateKey: os.Getenv("ENERGYLENS_CLOUD_LOCAL_AUTH_PRIVATE_KEY_FILE"), LocalAuthPublicKeys: os.Getenv("ENERGYLENS_CLOUD_LOCAL_AUTH_PUBLIC_KEY_FILES"), LocalAuthAudience: os.Getenv("ENERGYLENS_CLOUD_LOCAL_AUTH_AUDIENCE"),
		LocalAuthRegisterKey: os.Getenv("ENERGYLENS_CLOUD_LOCAL_AUTH_REGISTER_RATE_KEY_FILE"), LocalAuthLoginKey: os.Getenv("ENERGYLENS_CLOUD_LOCAL_AUTH_LOGIN_RATE_KEY_FILE"), LocalAuthRecoveryKey: os.Getenv("ENERGYLENS_CLOUD_LOCAL_AUTH_RECOVERY_RATE_KEY_FILE"),
		OEDIIOUPath: os.Getenv("ENERGYLENS_CLOUD_OEDI_IOU_CSV"), OEDINonIOUPath: os.Getenv("ENERGYLENS_CLOUD_OEDI_NON_IOU_CSV"), OEDIIOUSHA256: os.Getenv("ENERGYLENS_CLOUD_OEDI_IOU_SHA256"), OEDINonIOUSHA256: os.Getenv("ENERGYLENS_CLOUD_OEDI_NON_IOU_SHA256"),
		KBScanPython: os.Getenv("ENERGYLENS_CLOUD_KB_PYTHON"), KBScanSources: os.Getenv("ENERGYLENS_CLOUD_KB_SOURCES"), KBScanCandidateIndex: os.Getenv("ENERGYLENS_CLOUD_KB_CANDIDATE_INDEX"),
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
	if raw := os.Getenv("ENERGYLENS_CLOUD_KB_ALLOW_EPA"); raw != "" {
		enabled, err := strconv.ParseBool(raw)
		if err != nil {
			return Config{}, fmt.Errorf("invalid ENERGYLENS_CLOUD_KB_ALLOW_EPA: %w", err)
		}
		c.KBScanAllowEPA = enabled
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
	if raw := os.Getenv("ENERGYLENS_CLOUD_ACCOUNT_DELETION_ENABLED"); raw != "" {
		enabled, err := strconv.ParseBool(raw)
		if err != nil {
			return Config{}, fmt.Errorf("invalid ENERGYLENS_CLOUD_ACCOUNT_DELETION_ENABLED: %w", err)
		}
		c.AccountDeletionEnabled = enabled
	}
	if c.Environment == "" {
		c.Environment = "dev"
	}
	if c.APIListen == "" {
		c.APIListen = "127.0.0.1:8091"
	}
	return c, c.Validate()
}

func (c Config) Validate() error {
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
	if c.LocalAuthEnabled {
		if c.Role != RoleAPI && c.Role != RoleAll || c.DatabaseURL == "" || !c.LocalAuthMailEnabled {
			return errors.New("local auth requires API role, database and enabled local mail")
		}
		if c.LocalAuthAudience == "" || c.LocalAuthAudience != strings.TrimSpace(c.LocalAuthAudience) || len(c.LocalAuthAudience) > 256 || c.LocalAuthActiveKeyID == "" || c.LocalAuthPrivateKey == "" || c.LocalAuthRegisterKey == "" || c.LocalAuthLoginKey == "" || c.LocalAuthRecoveryKey == "" {
			return errors.New("local auth signing and rate key configuration incomplete")
		}
		paths, err := c.localAuthKeyFiles()
		if err != nil || len(paths.PublicPaths) == 0 {
			return errors.New("invalid local auth public key configuration")
		}
		for _, path := range []string{c.LocalAuthPrivateKey, c.LocalAuthRegisterKey, c.LocalAuthLoginKey, c.LocalAuthRecoveryKey} {
			if !filepath.IsAbs(path) {
				return errors.New("local auth key paths must be absolute")
			}
		}
		if c.LocalAuthRegisterKey == c.LocalAuthLoginKey || c.LocalAuthRegisterKey == c.LocalAuthRecoveryKey || c.LocalAuthLoginKey == c.LocalAuthRecoveryKey {
			return errors.New("local auth rate keys must use separate files")
		}
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
		if c.Role != RoleWorker && c.Role != RoleAll {
			return errors.New("report provider configuration requires worker or all role")
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
	if c.KBScanPython != "" || c.KBScanSources != "" || c.KBScanCandidateIndex != "" || c.KBScanAllowEPA {
		if c.KBScanPython == "" || c.KBScanSources == "" || c.DatabaseURL == "" || c.Role == RoleAPI {
			return errors.New("KB scan requires worker role, database, Python and sources")
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
	if configured && (c.OIDCIssuer == account.LocalIssuer || c.OIDCIssuer != strings.TrimSpace(c.OIDCIssuer) || c.OIDCAudience != strings.TrimSpace(c.OIDCAudience) || c.OIDCAudience == "") {
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
			return fmt.Errorf("invalid database configuration: %w", err)
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
	if c.Environment == "production" && c.Role != RoleWorker && c.HubListen == "" {
		return errors.New("production API requires separate hub mTLS listener")
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
	if c.HubListen != "" {
		if c.Role == RoleWorker {
			return errors.New("worker cannot serve hub listener")
		}
		if c.HubListen == c.APIListen {
			return errors.New("hub listener must use a distinct address")
		}
		if c.HubCertFile == "" || c.HubKeyFile == "" || c.HubCAFile == "" {
			return errors.New("hub listener requires TLS certificate, key, and client CA")
		}
	} else if c.HubCertFile != "" || c.HubKeyFile != "" || c.HubCAFile != "" {
		return errors.New("hub TLS settings require ENERGYLENS_CLOUD_HUB_BIND")
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
	return account.LocalKeyFiles{ActiveKeyID: c.LocalAuthActiveKeyID, PrivatePath: c.LocalAuthPrivateKey, PublicPaths: paths, Audience: c.LocalAuthAudience}, nil
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
	return &tls.Config{MinVersion: tls.VersionTLS12, Certificates: []tls.Certificate{pair}, ClientAuth: tls.RequireAndVerifyClientCert, ClientCAs: pool}, nil
}

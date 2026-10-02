package app

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"

	"energylens.local/cloud/internal/account"
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
	Role                   Role
	Environment            string
	APIListen              string
	HubListen              string
	HubCertFile            string
	HubKeyFile             string
	HubCAFile              string
	HubSignCertFile        string
	HubSignKeyFile         string
	DatabaseURL            string
	EIAAPIKey              string
	OIDCIssuer             string
	OIDCAudience           string
	OIDCJWKSURL            string
	MeteredRollupEnabled   bool
	ReportRetentionEnabled bool
	ReportDeliveryEnabled  bool
	ResendAPIKey           string
	ResendFrom             string
	LocalAuthMailEnabled   bool
	LocalAuthResendAPIKey  string
	LocalAuthResendFrom    string
	LocalAuthLinkOrigin    string
	LocalAuthProxyCIDRs    string
	KBScanPython           string
	KBScanSources          string
	KBScanCandidateIndex   string
	KBScanAllowEPA         bool
}

func LoadConfig(role string) (Config, error) {
	c := Config{
		Role: Role(strings.TrimSpace(role)), Environment: strings.TrimSpace(os.Getenv("ENERGYLENS_CLOUD_ENV")),
		APIListen: os.Getenv("ENERGYLENS_CLOUD_BIND"), HubListen: os.Getenv("ENERGYLENS_CLOUD_HUB_BIND"),
		HubCertFile: os.Getenv("ENERGYLENS_CLOUD_HUB_TLS_CERT"), HubKeyFile: os.Getenv("ENERGYLENS_CLOUD_HUB_TLS_KEY"), HubCAFile: os.Getenv("ENERGYLENS_CLOUD_HUB_CLIENT_CA"), HubSignCertFile: os.Getenv("ENERGYLENS_CLOUD_HUB_SIGN_CERT"), HubSignKeyFile: os.Getenv("ENERGYLENS_CLOUD_HUB_SIGN_KEY"),
		DatabaseURL: os.Getenv("ENERGYLENS_CLOUD_DATABASE_URL"), EIAAPIKey: os.Getenv("EIA_API_KEY"),
		OIDCIssuer: os.Getenv("ENERGYLENS_CLOUD_OIDC_ISSUER"), OIDCAudience: os.Getenv("ENERGYLENS_CLOUD_OIDC_AUDIENCE"), OIDCJWKSURL: os.Getenv("ENERGYLENS_CLOUD_OIDC_JWKS_URL"),
		ResendAPIKey: os.Getenv("ENERGYLENS_CLOUD_RESEND_API_KEY"), ResendFrom: os.Getenv("ENERGYLENS_CLOUD_RESEND_FROM"),
		LocalAuthResendAPIKey: os.Getenv("ENERGYLENS_CLOUD_LOCAL_AUTH_RESEND_API_KEY"), LocalAuthResendFrom: os.Getenv("ENERGYLENS_CLOUD_LOCAL_AUTH_RESEND_FROM"), LocalAuthLinkOrigin: os.Getenv("ENERGYLENS_CLOUD_LOCAL_AUTH_LINK_ORIGIN"), LocalAuthProxyCIDRs: os.Getenv("ENERGYLENS_CLOUD_LOCAL_AUTH_TRUSTED_PROXY_CIDRS"),
		KBScanPython: os.Getenv("ENERGYLENS_CLOUD_KB_PYTHON"), KBScanSources: os.Getenv("ENERGYLENS_CLOUD_KB_SOURCES"), KBScanCandidateIndex: os.Getenv("ENERGYLENS_CLOUD_KB_CANDIDATE_INDEX"),
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
	if c.LocalAuthProxyCIDRs != "" {
		if c.Role != RoleAPI && c.Role != RoleAll {
			return errors.New("local auth trusted proxies require API or all role")
		}
		if _, err := account.ParseLocalAuthSourcePolicy(c.LocalAuthProxyCIDRs); err != nil {
			return err
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
	if c.Environment == "production" && c.Role != RoleWorker && !configured {
		return errors.New("production API requires OIDC issuer, audience, and JWKS URL")
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

// BuildLocalAuthSender is the sole runtime construction gate for the future
// local-auth routes. Disabled configuration has no provider or network client.
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

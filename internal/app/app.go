// Package app runs the path-routed EnergyLens Cloud process. No account or hub
// operation is registered until its authentication and ownership checks exist.
package app

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	"energylens.local/cloud/internal/account"
	"energylens.local/cloud/internal/attachments"
	"energylens.local/cloud/internal/catalog"
	"energylens.local/cloud/internal/energyrate"
	"energylens.local/cloud/internal/estimation"
	"energylens.local/cloud/internal/faq"
	"energylens.local/cloud/internal/hubtrust"
	"energylens.local/cloud/internal/identityscan"
	"energylens.local/cloud/internal/inventory"
	"energylens.local/cloud/internal/kasaruntime"
	"energylens.local/cloud/internal/meteredexport"
	"energylens.local/cloud/internal/meteredhistory"
	"energylens.local/cloud/internal/meteredrollup"
	"energylens.local/cloud/internal/meteredsummary"
	"energylens.local/cloud/internal/outcomes"
	"energylens.local/cloud/internal/programs"
	"energylens.local/cloud/internal/questions"
	"energylens.local/cloud/internal/recommendation"
	"energylens.local/cloud/internal/reports"
	"energylens.local/cloud/internal/sensors"
	"energylens.local/cloud/internal/tariff"
	"energylens.local/cloud/internal/telemetry"
	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type Module struct {
	Name  string
	Mount func(chi.Router)
}

type Runtime struct {
	Pool *pgxpool.Pool
	API  http.Handler
	Hub  http.Handler
}

func (r *Runtime) Close() {
	if r.Pool != nil {
		r.Pool.Close()
	}
}

// Build opens one process-wide pool. API modules and enabled worker jobs share it.
func Build(ctx context.Context, c Config) (*Runtime, error) {
	if err := c.Validate(); err != nil {
		return nil, err
	}
	rt := &Runtime{}
	if c.DatabaseURL != "" {
		cfg, err := pgxpool.ParseConfig(c.DatabaseURL)
		if err != nil {
			return nil, fmt.Errorf("parse database configuration: %w", err)
		}
		cfg.MaxConns = 10
		rt.Pool, err = pgxpool.NewWithConfig(ctx, cfg)
		if err != nil {
			return nil, fmt.Errorf("open database pool: %w", err)
		}
		pingCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		if err := rt.Pool.Ping(pingCtx); err != nil {
			rt.Close()
			return nil, fmt.Errorf("database unavailable: %w", err)
		}
	}
	modules := make([]Module, 0, 4)
	var hubModule *hubtrust.Service
	if rt.Pool != nil && (c.Role == RoleAPI || c.Role == RoleAll) {
		hubModule = &hubtrust.Service{Store: hubtrust.Store{Pool: rt.Pool}}
		if c.HubSignCertFile != "" {
			pair, e := tls.LoadX509KeyPair(c.HubSignCertFile, c.HubSignKeyFile)
			if e != nil {
				rt.Close()
				return nil, fmt.Errorf("load hub signing key: %w", e)
			}
			if err := validateHubSigner(pair, c.HubCAFile); err != nil {
				rt.Close()
				return nil, err
			}
			hubModule.Signer = pair
		}
		modules = append(modules, Module{Name: "hub-enrollment", Mount: hubModule.EnrollmentRoutes})
	}
	if c.Role == RoleAPI || c.Role == RoleAll {
		var cache energyrate.Cache
		if rt.Pool != nil {
			var err error
			cache, err = energyrate.NewPgCache(ctx, rt.Pool)
			if err != nil {
				rt.Close()
				return nil, err
			}
		}
		var fetcher energyrate.RateFetcher
		if c.EIAAPIKey != "" {
			fetcher = energyrate.NewEIAClient("", c.EIAAPIKey)
		}
		rates := energyrate.NewService(energyrate.Options{
			Fetcher: fetcher, HasKey: c.EIAAPIKey != "", Cache: cache,
			StaticStates: map[string]energyrate.StaticRate{"TX": {
				USDPerKWh: 0.1588,
				AsOf:      time.Date(2026, time.July, 1, 0, 0, 0, 0, time.UTC),
				SourceURL: "https://www.eia.gov/electricity/monthly/epm_table_grapher.php?t=epmt_5_6_a",
			}},
		})
		modules = append(modules, Module{Name: "energy-rates", Mount: rates.Routes})
		if rt.Pool != nil && (c.OIDCIssuer != "" || c.LocalAuthEnabled) {
			var verifier account.Verifier
			if c.OIDCIssuer != "" {
				verifier = &account.OIDCVerifier{Issuer: c.OIDCIssuer, Audience: c.OIDCAudience, JWKSURL: c.OIDCJWKSURL}
			}
			if c.LocalAuthEnabled {
				localHTTP, localVerifier, err := buildLocalAuth(c, rt.Pool)
				if err != nil {
					rt.Close()
					return nil, err
				}
				if verifier == nil {
					verifier = localVerifier
				} else {
					verifier = account.DualVerifier{OIDCIssuer: c.OIDCIssuer, OIDC: verifier, Local: localVerifier}
				}
				modules = append(modules, Module{Name: "local-auth", Mount: localHTTP.Routes})
			}
			accounts := account.Service{Store: account.PGStore{Pool: rt.Pool}, Preferences: account.PGPreferenceStore{Pool: rt.Pool}, Verifier: verifier}
			if c.AccountDeletionEnabled {
				accounts.Deletion = account.PGStore{Pool: rt.Pool}
			}
			modules = append(modules, Module{Name: "account-homes", Mount: accounts.Routes})
			appliances := inventory.Service{Store: inventory.PGStore{Pool: rt.Pool}, Auth: accounts.Auth, UserID: account.UserID}
			modules = append(modules, Module{Name: "appliance-inventory", Mount: appliances.Routes})
			products := catalog.Service{Store: catalog.PGStore{Pool: rt.Pool}, Auth: accounts.Auth}
			modules = append(modules, Module{Name: "reviewed-catalog", Mount: products.Routes})
			tariffs := tariff.Service{Store: tariff.PGStore{Pool: rt.Pool}, Auth: accounts.Auth, UserID: account.UserID}
			modules = append(modules, Module{Name: "home-tariffs", Mount: tariffs.Routes})
			estimates := estimation.Service{Store: estimation.PGRunStore{Pool: rt.Pool}, Auth: accounts.Auth, UserID: account.UserID}
			modules = append(modules, Module{Name: "home-estimates", Mount: estimates.Routes})
			answersFAQ := faq.Service{Store: faq.PGStore{Pool: rt.Pool}, Auth: accounts.Auth, UserID: account.UserID}
			modules = append(modules, Module{Name: "home-faq-answers", Mount: answersFAQ.Routes})
			recommendations := recommendation.Service{Store: recommendation.PGStore{Pool: rt.Pool}, Auth: accounts.Auth, UserID: account.UserID}
			modules = append(modules, Module{Name: "home-recommendations", Mount: recommendations.Routes})
			ownerPrograms := programs.OwnerService{Store: programs.PGOwnerStore{Pool: rt.Pool}, Auth: accounts.Auth, UserID: account.UserID}
			modules = append(modules, Module{Name: "home-programs", Mount: ownerPrograms.Routes})
			reportPreviews := reports.Service{Store: reports.Store{DB: rt.Pool}, Auth: accounts.Auth, UserID: account.UserID}
			modules = append(modules, Module{Name: "home-report-previews", Mount: reportPreviews.Routes})
			meteredExport := meteredexport.Service{History: meteredhistory.Store{Pool: rt.Pool}, Summary: meteredsummary.Store{Pool: rt.Pool}, Auth: accounts.Auth, UserID: account.UserID}
			modules = append(modules, Module{Name: "home-metered-export", Mount: meteredExport.Routes})
			answers := questions.Service{Store: questions.PGStore{Pool: rt.Pool}, Auth: accounts.Auth, UserID: account.UserID}
			modules = append(modules, Module{Name: "home-questions", Mount: answers.Routes})
			hubModule.Auth = accounts.Auth
			hubModule.UserID = account.UserID
			modules = append(modules, Module{Name: "hub-claims", Mount: hubModule.AccountRoutes})
			sensorModule := sensors.Service{Store: sensors.Store{Pool: rt.Pool}, Auth: accounts.Auth, UserID: account.UserID}
			modules = append(modules, Module{Name: "sensors", Mount: sensorModule.AccountRoutes})
			attachmentModule := attachments.Service{Store: attachments.Store{Pool: rt.Pool}, Auth: accounts.Auth, UserID: account.UserID}
			modules = append(modules, Module{Name: "attachments", Mount: attachmentModule.Routes})
			historyModule := meteredhistory.Service{Store: meteredhistory.Store{Pool: rt.Pool}, Auth: accounts.Auth, UserID: account.UserID}
			modules = append(modules, Module{Name: "metered-history", Mount: historyModule.Routes})
			summaryModule := meteredsummary.Service{Store: meteredsummary.Store{Pool: rt.Pool}, Auth: accounts.Auth, UserID: account.UserID}
			modules = append(modules, Module{Name: "metered-summaries", Mount: summaryModule.Routes})
			outcomeModule := outcomes.Service{Store: outcomes.Store{Pool: rt.Pool}, Auth: accounts.Auth, UserID: account.UserID}
			modules = append(modules, Module{Name: "measured-outcomes", Mount: outcomeModule.Routes})
			runtimeModule := kasaruntime.Service{Store: kasaruntime.Store{Pool: rt.Pool}, Auth: accounts.Auth, UserID: account.UserID}
			modules = append(modules, Module{Name: "kasa-runtime", Mount: runtimeModule.AccountRoutes})
		}
	}
	ready := func(ctx context.Context) error {
		if rt.Pool == nil {
			return nil
		}
		return rt.Pool.Ping(ctx)
	}
	rt.API = apiRouter(modules, ready)
	rt.Hub = hubRouter(ready, hubModule, rt.Pool)
	return rt, nil
}

// The edge may append identity headers, but no caller-supplied identity header
// is allowed into an API module. Actual user identity will come from verified
// bearer tokens when account routes are activated.
func stripIdentity(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		for key := range r.Header {
			k := strings.ToLower(key)
			if strings.HasPrefix(k, "x-amzn-mtls-") || strings.HasPrefix(k, "x-energylens-identity-") || k == "x-forwarded-user" || k == "x-authenticated-user" {
				r.Header.Del(key)
			}
		}
		next.ServeHTTP(w, r)
	})
}

func probes(r chi.Router, ready func(context.Context) error) {
	r.Get("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	r.Get("/readyz", func(w http.ResponseWriter, req *http.Request) {
		ctx, cancel := context.WithTimeout(req.Context(), 2*time.Second)
		defer cancel()
		if err := ready(ctx); err != nil {
			http.Error(w, "not ready", http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ready"))
	})
}

func apiRouter(modules []Module, ready func(context.Context) error) http.Handler {
	r := chi.NewRouter()
	probes(r, ready)
	r.Route("/v1", func(v chi.Router) {
		for _, m := range modules {
			if m.Mount != nil {
				m.Mount(v)
			}
		}
	})
	return stripIdentity(r)
}

// Hub operations are deliberately absent until certificate revocation and
// current ownership epoch can be checked against the durable hub store.
func hubRouter(ready func(context.Context) error, hub *hubtrust.Service, pools ...*pgxpool.Pool) http.Handler {
	r := chi.NewRouter()
	probes(r, ready)
	if hub != nil {
		r.Route("/v1", func(v chi.Router) {
			hub.HubRoutes(v)
			if len(pools) > 0 && pools[0] != nil {
				sensorModule := sensors.Service{Store: sensors.Store{Pool: pools[0]}}
				sensorModule.HubRoutes(v)
				ingest := telemetry.Service{Store: telemetry.Store{Pool: pools[0]}}
				ingest.HubRoutes(v)
				runtimeModule := kasaruntime.Service{Store: kasaruntime.Store{Pool: pools[0]}}
				runtimeModule.HubRoutes(v)
			}
		})
	}
	return r
}

func Serve(ctx context.Context, c Config) error {
	if c.KBScanPython != "" {
		if err := (identityscan.KBScanner{Python: c.KBScanPython, Sources: c.KBScanSources, CandidateIndex: c.KBScanCandidateIndex, AllowEPA: c.KBScanAllowEPA}).Validate(); err != nil {
			return err
		}
	}
	rt, err := Build(ctx, c)
	if err != nil {
		return err
	}
	defer rt.Close()
	var reportProvider reports.DeliveryProvider
	if c.ReportDeliveryEnabled {
		reportProvider, err = reports.NewResendProvider(c.ResendAPIKey, c.ResendFrom)
		if err != nil {
			return err
		}
	}
	apiListener, err := net.Listen("tcp", c.APIListen)
	if err != nil {
		return fmt.Errorf("listen API: %w", err)
	}
	defer apiListener.Close()
	apiServer := &http.Server{Handler: rt.API, ReadHeaderTimeout: 5 * time.Second, IdleTimeout: 60 * time.Second}
	servers := []*http.Server{apiServer}
	listeners := []net.Listener{apiListener}
	if c.HubListen != "" {
		tlsCfg, err := c.hubTLSConfig()
		if err != nil {
			return err
		}
		plain, err := net.Listen("tcp", c.HubListen)
		if err != nil {
			return fmt.Errorf("listen hub mTLS: %w", err)
		}
		defer plain.Close()
		listeners = append(listeners, tls.NewListener(plain, tlsCfg))
		servers = append(servers, &http.Server{Handler: rt.Hub, ReadHeaderTimeout: 5 * time.Second, IdleTimeout: 60 * time.Second})
	}
	result := make(chan error, len(servers))
	for i, server := range servers {
		listener := listeners[i]
		go func(s *http.Server) { result <- s.Serve(listener) }(server)
	}
	var stopWorker context.CancelFunc
	var workerDone <-chan struct{}
	if c.MeteredRollupEnabled || c.ReportRetentionEnabled || c.ReportDeliveryEnabled || c.KBScanPython != "" {
		workerCtx, cancel := context.WithCancel(ctx)
		stopWorker = cancel
		done := make(chan struct{})
		workerDone = done
		go func() {
			defer close(done)
			var jobs []<-chan struct{}
			if c.MeteredRollupEnabled {
				finished := make(chan struct{})
				jobs = append(jobs, finished)
				go func() {
					defer close(finished)
					runMeteredRollups(workerCtx, meteredrollup.Job{Pool: rt.Pool}, rollupInterval)
				}()
			}
			if c.ReportRetentionEnabled {
				finished := make(chan struct{})
				jobs = append(jobs, finished)
				go func() {
					defer close(finished)
					runReportRetention(workerCtx, reports.RetentionJob{DB: rt.Pool}, reportRetentionInterval)
				}()
			}
			if c.ReportDeliveryEnabled {
				finished := make(chan struct{})
				jobs = append(jobs, finished)
				go func() {
					defer close(finished)
					runReportDeliveries(workerCtx, reports.DeliveryWorker{DB: rt.Pool, Provider: reportProvider, Enabled: true}, reportDeliveryInterval)
				}()
			}
			if c.KBScanPython != "" {
				finished := make(chan struct{})
				jobs = append(jobs, finished)
				go func() {
					defer close(finished)
					runIdentityScans(workerCtx, identityscan.Store{Pool: rt.Pool}, identityscan.KBScanner{Python: c.KBScanPython, Sources: c.KBScanSources, CandidateIndex: c.KBScanCandidateIndex, AllowEPA: c.KBScanAllowEPA}, 10*time.Second)
				}()
			}
			for _, finished := range jobs {
				<-finished
			}
		}()
	}
	slog.Info("energylens-cloud listening", "role", c.Role, "api", c.APIListen, "hub_mtls", c.HubListen)
	var serveErr error
	select {
	case <-ctx.Done():
	case err := <-result:
		if !errors.Is(err, http.ErrServerClosed) {
			serveErr = fmt.Errorf("listener stopped: %w", err)
		}
	}
	if stopWorker != nil {
		stopWorker()
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for _, server := range servers {
		if err := server.Shutdown(shutdownCtx); err != nil {
			serveErr = errors.Join(serveErr, err)
		}
	}
	if workerDone != nil {
		<-workerDone
	}
	return serveErr
}

func validateHubSigner(pair tls.Certificate, caPath string) error {
	if len(pair.Certificate) == 0 || caPath == "" {
		return errors.New("hub signer and trusted client CA required")
	}
	cert, err := x509.ParseCertificate(pair.Certificate[0])
	if err != nil {
		return err
	}
	if !cert.IsCA || cert.KeyUsage&x509.KeyUsageCertSign == 0 {
		return errors.New("hub signing certificate must be a CA")
	}
	body, err := os.ReadFile(caPath)
	if err != nil {
		return err
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(body) {
		return errors.New("hub client CA contains no certificates")
	}
	if _, err = cert.Verify(x509.VerifyOptions{Roots: roots, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageAny}}); err != nil {
		return fmt.Errorf("hub signer not trusted by client CA: %w", err)
	}
	return nil
}

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
	"energylens.local/cloud/internal/appliancefacts"
	"energylens.local/cloud/internal/attachments"
	"energylens.local/cloud/internal/catalog"
	"energylens.local/cloud/internal/cyclecomparison"
	"energylens.local/cloud/internal/datawizfacts"
	"energylens.local/cloud/internal/datawizrequests"
	"energylens.local/cloud/internal/datawizresolution"
	"energylens.local/cloud/internal/designfixture"
	"energylens.local/cloud/internal/devicestate"
	"energylens.local/cloud/internal/energycategories"
	"energylens.local/cloud/internal/energyrate"
	"energylens.local/cloud/internal/estimation"
	"energylens.local/cloud/internal/faq"
	"energylens.local/cloud/internal/householdenergy"
	"energylens.local/cloud/internal/hubtrust"
	"energylens.local/cloud/internal/inventory"
	"energylens.local/cloud/internal/kasaruntime"
	"energylens.local/cloud/internal/meteredexport"
	"energylens.local/cloud/internal/meteredhistory"
	"energylens.local/cloud/internal/meteredrollup"
	"energylens.local/cloud/internal/meteredsummary"
	"energylens.local/cloud/internal/outcomes"
	"energylens.local/cloud/internal/programs"
	"energylens.local/cloud/internal/questions"
	"energylens.local/cloud/internal/ratedinputapi"
	"energylens.local/cloud/internal/recommendation"
	"energylens.local/cloud/internal/reports"
	"energylens.local/cloud/internal/sensors"
	"energylens.local/cloud/internal/tariff"
	"energylens.local/cloud/internal/telemetry"
	"energylens.local/cloud/internal/usagefactapi"
	"energylens.local/cloud/internal/weather"
	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type Module struct {
	Name  string
	Mount func(chi.Router)
}

type Runtime struct {
	Pool    *pgxpool.Pool
	API     http.Handler
	Weather *weather.Live
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
		modules = append(modules, Module{Name: "auth-capabilities", Mount: c.authCapabilitiesRoutes})
		var cache energyrate.Cache
		var gasCache energyrate.GasCache
		if rt.Pool != nil {
			var err error
			cache, err = energyrate.NewPgCache(ctx, rt.Pool)
			if err != nil {
				rt.Close()
				return nil, err
			}
			gasCache, err = energyrate.NewPgGasCache(ctx, rt.Pool)
			if err != nil {
				rt.Close()
				return nil, err
			}
		}
		var rateResearch *datawizfacts.Store
		if rt.Pool != nil {
			rateResearch = &datawizfacts.Store{DB: rt.Pool}
		}
		rates := energyrate.NewService(energyrate.Options{
			Research: rateResearch,
			Cache:    cache, GasCache: gasCache,
			GasStatic: energyrate.GasStaticRate{
				USDPerTherm: 1.481, // EIA 2025 annual residential $15.34/Mcf / 10.36 Mcf-to-therm divisor
				AsOf:        time.Date(2025, time.January, 1, 0, 0, 0, 0, time.UTC),
				SourceURL:   "https://www.eia.gov/dnav/ng/NG_PRI_SUM_DCU_NUS_A.htm",
			},
			Static: energyrate.StaticRate{
				USDPerKWh: 0.1831,
				AsOf:      time.Date(2026, time.July, 1, 0, 0, 0, 0, time.UTC),
				SourceURL: "https://www.eia.gov/electricity/monthly/epm_table_grapher.php?t=epmt_5_6_a", // EPM Table 5.6.A, U.S. Total residential, released 2026-09-24
			},
			StaticStates: map[string]energyrate.StaticRate{"TX": {
				USDPerKWh: 0.1588,
				AsOf:      time.Date(2026, time.July, 1, 0, 0, 0, 0, time.UTC),
				SourceURL: "https://www.eia.gov/electricity/monthly/epm_table_grapher.php?t=epmt_5_6_a",
			}},
		})
		modules = append(modules, Module{Name: "energy-rates", Mount: rates.Routes})
		if rt.Pool != nil && (c.OIDCIssuer != "" || c.LocalAuthEnabled || c.DevAuthEnabled) {
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
			if c.DevAuthEnabled {
				dev := newDevAuth(c)
				verifier = dev
				modules = append(modules, Module{Name: "dev-auth", Mount: dev.Routes})
			}
			accounts := account.Service{Store: account.PGStore{Pool: rt.Pool}, Preferences: account.PGPreferenceStore{Pool: rt.Pool}, Profile: account.PGProfileStore{Pool: rt.Pool}, Verifier: verifier}
			if c.AccountDeletionEnabled {
				accounts.Deletion = account.PGStore{Pool: rt.Pool}
			}
			modules = append(modules, Module{Name: "account-homes", Mount: accounts.Routes})
			liveEnergy := householdenergy.Service{Pool: rt.Pool, Auth: accounts.Auth, UserID: account.UserID, NationalRate: rates.NationalUSDPerKWh, GasRate: rates.ResolveGas}
			modules = append(modules, Module{Name: "live-household-energy", Mount: liveEnergy.Routes})
			deviceStates := devicestate.Service{Store: devicestate.PGStore{Pool: rt.Pool}, Auth: accounts.Auth, UserID: account.UserID}
			modules = append(modules, Module{Name: "home-device-states", Mount: deviceStates.Routes})
			if c.SampleFixturesEnabled {
				allowed, err := designfixture.ParseUserIDs(c.SampleFixtureUserIDs)
				if err != nil {
					rt.Close()
					return nil, fmt.Errorf("sample fixture allowlist: %w", err)
				}
				allowedHomes, err := designfixture.ParseUserIDs(c.SampleFixtureHomeIDs)
				if err != nil {
					rt.Close()
					return nil, fmt.Errorf("sample fixture home allowlist: %w", err)
				}
				fixtureService := designfixture.Service{Homes: account.PGStore{Pool: rt.Pool}, Auth: accounts.Auth, UserID: account.UserID, AllowedUsers: allowed, AllowedHomes: allowedHomes}
				modules = append(modules, Module{Name: "isolated-mobile-design-fixtures", Mount: fixtureService.Routes})
			}
			appliances := inventory.Service{Store: inventory.PGStore{Pool: rt.Pool, AnswerApplies: questions.Applies}, Auth: accounts.Auth, UserID: account.UserID}
			modules = append(modules, Module{Name: "appliance-inventory", Mount: appliances.Routes})
			categories := energycategories.Service{Store: energycategories.PGStore{Pool: rt.Pool}, Auth: accounts.Auth}
			modules = append(modules, Module{Name: "energy-categories", Mount: categories.Routes})
			resolutions := datawizresolution.Service{Store: datawizresolution.PGStore{Pool: rt.Pool}, Auth: accounts.Auth, UserID: account.UserID}
			modules = append(modules, Module{Name: "energy-resolutions", Mount: resolutions.Routes})
			products := catalog.Service{Store: catalog.PGStore{Pool: rt.Pool}, Auth: accounts.Auth}
			modules = append(modules, Module{Name: "reviewed-catalog", Mount: products.Routes})
			usageFacts := usagefactapi.Service{Store: usagefactapi.PGStore{Pool: rt.Pool}, Auth: accounts.Auth}
			modules = append(modules, Module{Name: "reviewed-usage-facts", Mount: usageFacts.Routes})
			tariffs := tariff.Service{Store: tariff.PGStore{Pool: rt.Pool}, Plans: tariff.PGRatePlanStore{Pool: rt.Pool}, Auth: accounts.Auth, UserID: account.UserID}
			modules = append(modules, Module{Name: "home-tariffs", Mount: tariffs.Routes})
			estimates := estimation.Service{Store: estimation.PGRunStore{Pool: rt.Pool, NationalRate: rates.NationalUSDPerKWh}, Auth: accounts.Auth, UserID: account.UserID}
			modules = append(modules, Module{Name: "home-estimates", Mount: estimates.Routes})
			answersFAQ := faq.Service{Store: faq.PGStore{Pool: rt.Pool}, Recommendations: recommendation.PGStore{Pool: rt.Pool}, Programs: programs.PGOwnerStore{Pool: rt.Pool}, Auth: accounts.Auth, UserID: account.UserID}
			if c.WeatherOpenMeteoAPIKey != "" {
				cells, err := weather.ParseCoarseCells(c.WeatherCoarseCells)
				if err != nil {
					rt.Close()
					return nil, err
				}
				provider := weather.OpenMeteo{APIKey: c.WeatherOpenMeteoAPIKey, Cells: cells}
				answersFAQ.Weather = &weather.Service{Store: weather.PGStore{Pool: rt.Pool}, Provider: provider, TTL: 24 * time.Hour}
			}
			modules = append(modules, Module{Name: "home-faq-answers", Mount: answersFAQ.Routes})
			rt.Weather = weather.NewLive(rt.Pool, c.WeatherUserAgent)
			recommendations := recommendation.Service{Store: recommendation.PGStore{Pool: rt.Pool}, Auth: accounts.Auth, UserID: account.UserID, Waste: liveEnergy, Weather: rt.Weather, TOU: liveEnergy}
			modules = append(modules, Module{Name: "home-recommendations", Mount: recommendations.Routes})
			ownerPrograms := programs.OwnerService{Store: programs.PGOwnerStore{Pool: rt.Pool}, Auth: accounts.Auth, UserID: account.UserID}
			modules = append(modules, Module{Name: "home-programs", Mount: ownerPrograms.Routes})
			reportPreviews := reports.Service{Store: reports.Store{DB: rt.Pool}, Auth: accounts.Auth, UserID: account.UserID}
			if c.ReportDirectSendEnabled {
				provider, providerErr := reports.NewResendProvider(c.ResendAPIKey, c.ResendFrom)
				if providerErr != nil {
					rt.Close()
					return nil, providerErr
				}
				direct := reports.DeliveryWorker{DB: rt.Pool, Provider: provider, Enabled: true}
				reportPreviews.DirectSend = func(ctx context.Context, requestID string) (bool, error) {
					return direct.DeliverRequest(ctx, requestID, time.Now().UTC())
				}
			}
			modules = append(modules, Module{Name: "home-report-previews", Mount: reportPreviews.Routes})
			cursorKey, previousCursorKey, err := c.meteredExportKeys()
			if err != nil {
				rt.Close()
				return nil, fmt.Errorf("configure metered export cursors: %w", err)
			}
			meteredExport := meteredexport.Service{History: meteredhistory.Store{Pool: rt.Pool}, Summary: meteredsummary.Store{Pool: rt.Pool}, Auth: accounts.Auth, UserID: account.UserID, CursorKey: cursorKey, PreviousCursorKey: previousCursorKey}
			modules = append(modules, Module{Name: "home-metered-export", Mount: meteredExport.Routes})
			answers := questions.Service{Store: questions.PGStore{Pool: rt.Pool}, Auth: accounts.Auth, UserID: account.UserID}
			modules = append(modules, Module{Name: "home-questions", Mount: answers.Routes})
			applianceFacts := appliancefacts.Service{Composer: appliancefacts.Composer{Appliances: appliances.Store, Resolutions: resolutions.Store, Estimates: estimates.Store, Programs: ownerPrograms.Store, Recommendations: recommendations.Store, Answers: answers.Store, Efficiency: liveEnergy, Revisions: appliancefacts.PGRevisions{Pool: rt.Pool}}, Auth: accounts.Auth, UserID: account.UserID}
			modules = append(modules, Module{Name: "appliance-facts", Mount: applianceFacts.Routes})
			hubModule.Auth = accounts.Auth
			hubModule.UserID = account.UserID
			hubModule.SessionID = account.SessionID
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
			cycleModule := cyclecomparison.Service{Store: cyclecomparison.Store{Pool: rt.Pool}, Auth: accounts.Auth, UserID: account.UserID}
			modules = append(modules, Module{Name: "dishwasher-cycle-comparison", Mount: cycleModule.Routes})
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
	rt.API = cloudRouter(modules, ready, hubModule, rt.Pool)
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
	return cloudRouter(modules, ready, nil, nil)
}

// cloudRouter places bearer, enrollment, and client-certificate routes on the
// same path-routed listener. Hub handlers retain their durable revocation and
// ownership checks after this transport-level certificate gate.
func cloudRouter(modules []Module, ready func(context.Context) error, hub *hubtrust.Service, pool *pgxpool.Pool) http.Handler {
	r := chi.NewRouter()
	probes(r, ready)
	r.Route("/v1", func(v chi.Router) {
		for _, m := range modules {
			if m.Mount != nil {
				m.Mount(v)
			}
		}
		if hub != nil {
			v.Group(func(h chi.Router) {
				h.Use(requireVerifiedHubCertificate)
				hub.HubRoutes(h)
				if pool != nil {
					(catalog.Service{Store: catalog.PGStore{Pool: pool}, HubAuth: catalog.HubAuthentication(hubtrust.Store{Pool: pool})}).HubRoutes(h)
					(usagefactapi.Service{Store: usagefactapi.PGStore{Pool: pool}, HubCertificates: hubtrust.Store{Pool: pool}}).HubRoutes(h)
					(energycategories.Service{Store: energycategories.PGStore{Pool: pool}, HubCertificates: hubtrust.Store{Pool: pool}}).HubRoutes(h)
					(datawizresolution.Service{Store: datawizresolution.PGStore{Pool: pool}, HubCertificates: hubtrust.Store{Pool: pool}}).HubRoutes(h)
					(ratedinputapi.Service{Store: ratedinputapi.PGStore{Pool: pool}, HubCertificates: hubtrust.Store{Pool: pool}}).HubRoutes(h)
					(sensors.Service{Store: sensors.Store{Pool: pool}}).HubRoutes(h)
					(attachments.Service{Store: attachments.Store{Pool: pool}}).HubRoutes(h)
					(telemetry.Service{Store: telemetry.Store{Pool: pool}}).HubRoutes(h)
					(kasaruntime.Service{Store: kasaruntime.Store{Pool: pool}}).HubRoutes(h)
				}
			})
		}
	})
	return stripIdentity(r)
}

func requireVerifiedHubCertificate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.TLS == nil || len(r.TLS.VerifiedChains) == 0 || len(r.TLS.PeerCertificates) == 0 {
			http.Error(w, "hub certificate required", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func Serve(ctx context.Context, c Config) error {
	if c.Role != RoleWorker && (c.HubCertFile == "" || c.HubKeyFile == "" || c.HubCAFile == "") {
		return errors.New("Cloud TLS listener requires server certificate, key, and hub client CA")
	}
	var tlsCfg *tls.Config
	if c.Role != RoleWorker {
		var err error
		tlsCfg, err = c.hubTLSConfig()
		if err != nil {
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
	var server *http.Server
	var result <-chan error
	if c.Role != RoleWorker {
		plain, err := net.Listen("tcp", c.APIListen)
		if err != nil {
			return fmt.Errorf("listen Cloud TLS: %w", err)
		}
		defer plain.Close()
		server = &http.Server{Handler: rt.API, ReadHeaderTimeout: 5 * time.Second, IdleTimeout: 60 * time.Second}
		serveResult := make(chan error, 1)
		result = serveResult
		go func() { serveResult <- server.Serve(tls.NewListener(plain, tlsCfg)) }()
	}
	var stopWorker context.CancelFunc
	var workerDone <-chan struct{}
	ratePlanSweep := rt.Pool != nil && c.Role != RoleAPI
	if c.MeteredRollupEnabled || c.KasaRuntimeRetentionEnabled || c.ReportRetentionEnabled || c.ReportDeliveryEnabled || c.DatawizRequestsDir != "" || ratePlanSweep {
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
			if c.KasaRuntimeRetentionEnabled {
				finished := make(chan struct{})
				jobs = append(jobs, finished)
				go func() {
					defer close(finished)
					runKasaRuntimeRetention(workerCtx, kasaruntime.RetentionJob{DB: rt.Pool}, kasaRuntimeRetentionInterval)
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
			if c.DatawizRequestsDir != "" {
				finished := make(chan struct{})
				jobs = append(jobs, finished)
				go func() {
					defer close(finished)
					runDatawizRequests(workerCtx, datawizrequests.Producer{DB: rt.Pool, Dir: c.DatawizRequestsDir}, 5*time.Second)
				}()
			}
			if c.DatawizRequestsDir != "" {
				finished := make(chan struct{})
				jobs = append(jobs, finished)
				go func() {
					defer close(finished)
					runDatawizRequests(workerCtx, datawizrequests.Requeuer{DB: rt.Pool}, time.Hour)
				}()
			}
			if ratePlanSweep {
				finished := make(chan struct{})
				jobs = append(jobs, finished)
				go func() {
					defer close(finished)
					runRatePlanSweeps(workerCtx, tariff.RatePlanSweep{Pool: rt.Pool}, ratePlanChangeInterval, ratePlanSweepInterval)
				}()
			}
			for _, finished := range jobs {
				<-finished
			}
		}()
	}
	slog.Info("energylens-cloud running", "role", c.Role, "tls_listen", c.APIListen)
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
	if server != nil {
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

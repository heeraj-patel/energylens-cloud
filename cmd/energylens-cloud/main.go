package main

import (
	"log"
	"net/http"
	"os"
	"time"

	"energylens.local/cloud/internal/energyrate"
	"github.com/go-chi/chi/v5"
)

func main() {
	key := os.Getenv("EIA_API_KEY")
	var fetcher energyrate.RateFetcher
	if key != "" {
		fetcher = energyrate.NewEIAClient("", key)
	}
	rateService := energyrate.NewService(energyrate.Options{
		Fetcher: fetcher,
		HasKey:  key != "",
		// EIA Electric Power Monthly Table 5.6.A, July 2026 residential TX:
		// 15.88 cents/kWh. This dated state average is a pilot fallback, not a tariff.
		StaticStates: map[string]energyrate.StaticRate{
			"TX": {
				USDPerKWh: 0.1588,
				AsOf:      time.Date(2026, time.July, 1, 0, 0, 0, 0, time.UTC),
				SourceURL: "https://www.eia.gov/electricity/monthly/epm_table_grapher.php?t=epmt_5_6_a",
			},
		},
	})
	r := chi.NewRouter()
	r.Get("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	r.Route("/v1", rateService.Routes)
	server := &http.Server{
		Addr:              bindAddress(),
		Handler:           r,
		ReadHeaderTimeout: 5 * time.Second,
	}
	log.Printf("EnergyLens cloud development API listening on %s", server.Addr)
	log.Fatal(server.ListenAndServe())
}

func bindAddress() string {
	if value := os.Getenv("ENERGYLENS_CLOUD_BIND"); value != "" {
		return value
	}
	return "127.0.0.1:8091"
}

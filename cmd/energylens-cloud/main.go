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
		// No guessed household rate: without EIA or a cached value, the
		// endpoint reports a temporary failure.
	})
	r := chi.NewRouter()
	r.Get("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	r.Route("/v1", rateService.Routes)
	server := &http.Server{
		Addr:              "127.0.0.1:8091",
		Handler:           r,
		ReadHeaderTimeout: 5 * time.Second,
	}
	log.Printf("EnergyLens cloud development API listening on %s", server.Addr)
	log.Fatal(server.ListenAndServe())
}

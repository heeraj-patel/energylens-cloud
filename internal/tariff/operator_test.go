package tariff

import (
	"crypto/sha256"
	"encoding/hex"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestOfficialImportValidation(t *testing.T) {
	now := time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)
	snapshot := []byte("reviewed official source")
	digest := sha256.Sum256(snapshot)
	fallback := false
	in := OfficialImport{OwnerUserID: uuid.NewString(), HomeID: uuid.NewString(), SourceURL: "https://utility.example.test/rates", SourceSHA256: hex.EncodeToString(digest[:]), Tariff: Create{Fuel: "natural_gas", Rate: "1.20", RateUnit: "therm", Currency: "USD", RateKind: "official_tariff", SourceAsOf: now.Format(time.RFC3339), IsFallback: &fallback, EffectiveFrom: now.Add(-time.Hour).Format(time.RFC3339)}}
	if _, err := in.validate(snapshot, "operator", now); err != nil {
		t.Fatalf("valid official rate rejected: %v", err)
	}
	for name, change := range map[string]func(*OfficialImport){
		"customer rate kind": func(v *OfficialImport) { v.Tariff.RateKind = "user_tariff" },
		"forged fallback":    func(v *OfficialImport) { value := true; v.Tariff.IsFallback = &value },
		"wrong unit":         func(v *OfficialImport) { v.Tariff.RateUnit = "kWh" },
		"non-USD":            func(v *OfficialImport) { v.Tariff.Currency = "CAD" },
		"private URL":        func(v *OfficialImport) { v.SourceURL = "http://utility.example.test/rates" },
		"stale source":       func(v *OfficialImport) { v.Tariff.SourceAsOf = now.AddDate(-2, 0, 0).Format(time.RFC3339) },
		"future source":      func(v *OfficialImport) { v.Tariff.SourceAsOf = now.Add(time.Hour).Format(time.RFC3339) },
		"source mismatch":    func(v *OfficialImport) { v.SourceSHA256 = "bad" },
	} {
		t.Run(name, func(t *testing.T) {
			changed := in
			change(&changed)
			if _, err := changed.validate(snapshot, "operator", now); err == nil {
				t.Fatal("invalid official rate accepted")
			}
		})
	}
	if sameDecimal("10", "1") || !sameDecimal("1.2000", "1.2") {
		t.Fatal("decimal replay comparison incorrect")
	}
}

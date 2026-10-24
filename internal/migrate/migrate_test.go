package migrate

import (
	"testing"

	"energylens.local/cloud/migrations"
)

func TestEmbeddedMigrationIDsPreserveExistingFiles(t *testing.T) {
	names, err := Files(migrations.FS)
	if err != nil {
		t.Fatal(err)
	}
	if len(names) < 2 || names[0] != "0016_energy_rate_cache.sql" || names[1] != "0017_energy_rating_catalog.sql" {
		t.Fatalf("existing migrations changed: %v", names)
	}
	if len(names) < 3 || names[2] != "0018_account_homes.sql" {
		t.Fatalf("account/home migration missing or out of order: %v", names)
	}
	seen := map[string]bool{}
	ids := map[string]int{}
	for _, name := range names {
		if seen[name] {
			t.Fatalf("duplicate migration ID: %s", name)
		}
		seen[name] = true
		ids[name[:4]]++
		if ids[name[:4]] > 1 && name[:4] != "0098" {
			t.Fatalf("unexpected duplicate migration ID: %s", name)
		}
	}
	if ids["0098"] != 2 || !seen["0098_hub_pairing_codes.sql"] || !seen["0098_weather_days.sql"] {
		t.Fatalf("the two existing 0098 migrations changed: %v", names)
	}
}

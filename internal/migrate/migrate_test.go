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
	for _, name := range names {
		if seen[name[:4]] {
			t.Fatalf("duplicate migration ID: %s", name)
		}
		seen[name[:4]] = true
	}
}

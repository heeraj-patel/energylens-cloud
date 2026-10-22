package inventory

import (
	"testing"

	"github.com/google/uuid"
)

func TestCreateAndPatchQueueProgramDiscovery(t *testing.T) {
	f := newLinkFixture(t)
	count := func(category string) int {
		t.Helper()
		var n int
		if err := f.pool.QueryRow(f.ctx, `SELECT count(*) FROM program_discovery_refresh_requests WHERE home_id=$1 AND category=$2`, f.home, category).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	a := f.createDryer("Acme", "GD1", "natural_gas")
	if count("dryer") != 1 {
		t.Fatalf("dryer requests after create = %d", count("dryer"))
	}
	name := "Renamed"
	b, err := f.store.Patch(f.ctx, f.owner, f.home, a.ID, PatchInput{Name: &name}, a.Version, uuid.NewString(), make([]byte, 32))
	if err != nil || count("dryer") != 1 {
		t.Fatalf("rename queued a refresh: %v %d", err, count("dryer"))
	}
	identity := b.Identity
	identity.Fuel = "electricity"
	c, err := f.store.Patch(f.ctx, f.owner, f.home, a.ID, PatchInput{Identity: &identity}, b.Version, uuid.NewString(), make([]byte, 32))
	if err != nil || count("dryer") != 2 {
		t.Fatalf("fuel change refresh: %v %d", err, count("dryer"))
	}
	category := "washer"
	if _, err = f.store.Patch(f.ctx, f.owner, f.home, a.ID, PatchInput{Category: &category}, c.Version, uuid.NewString(), make([]byte, 32)); err != nil || count("washer") != 1 {
		t.Fatalf("category change refresh: %v %d", err, count("washer"))
	}
}

func TestCreateWithoutLocationStillSucceeds(t *testing.T) {
	f := newLinkFixture(t)
	if _, err := f.pool.Exec(f.ctx, `DELETE FROM home_location_revisions WHERE home_id=$1`, f.home); err != nil {
		t.Skipf("cannot clear location: %v", err)
	}
	a := f.createDryer("Acme", "GD2", "natural_gas")
	var n int
	if err := f.pool.QueryRow(f.ctx, `SELECT count(*) FROM program_discovery_refresh_requests WHERE home_id=$1`, f.home).Scan(&n); err != nil || n != 0 || a.ID == "" {
		t.Fatalf("requests %d err %v", n, err)
	}
}

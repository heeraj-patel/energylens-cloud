package programs

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func oediFixtureResolver(t *testing.T) OEDIZIPResolver {
	t.Helper()
	dir := t.TempDir()
	copyFile := func(name string) (string, string) {
		body, err := os.ReadFile(filepath.Join("testdata", name))
		if err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, body, 0600); err != nil {
			t.Fatal(err)
		}
		digest := sha256.Sum256(body)
		return path, hex.EncodeToString(digest[:])
	}
	iou, iouHash := copyFile("iou_zipcodes_2024.csv")
	non, nonHash := copyFile("non_iou_zipcodes_2024.csv")
	return OEDIZIPResolver{IOUPath: iou, NonIOUPath: non, IOUSHA256: iouHash,
		NonIOUSHA256: nonHash, RetrievedAt: time.Date(2026, 10, 2, 17, 0, 0, 0, time.UTC)}
}

func TestOEDIZIPResolverKeepsNeutralCandidatesAndProvenance(t *testing.T) {
	r := oediFixtureResolver(t)
	result, err := r.ResolveZIPDetailed(context.Background(), "US", "92503")
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Scopes) != 3 {
		t.Fatalf("scopes = %d", len(result.Scopes))
	}
	for _, scope := range result.Scopes {
		if scope.Kind != "utility" || scope.ProviderRole != "electric_service_candidate" || scope.SourceKey != "" || !strings.HasPrefix(scope.Key, "eia:") {
			t.Fatalf("unsafe scope: %+v", scope)
		}
	}
	if result.Provenance["source_data_year"] != 2024 || len(result.Limitations) != 5 {
		t.Fatalf("evidence: %+v / %+v", result.Provenance, result.Limitations)
	}
	missing, err := r.ResolveZIPDetailed(context.Background(), "US", "99999")
	if err != nil || len(missing.Scopes) != 0 || len(missing.Limitations) == 0 {
		t.Fatalf("source-limited miss: %+v, %v", missing, err)
	}
}

func TestOEDIZIPResolverRejectsMutatedAndUnboundedSources(t *testing.T) {
	r := oediFixtureResolver(t)
	r.IOUSHA256 = strings.Repeat("0", 64)
	if _, err := r.ResolveZIPDetailed(context.Background(), "US", "92503"); err == nil || !strings.Contains(err.Error(), "SHA-256 mismatch") {
		t.Fatalf("changed digest: %v", err)
	}
	r = oediFixtureResolver(t)
	if err := os.WriteFile(r.IOUPath, []byte(strings.Repeat("x", 7_000_001)), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := r.ResolveZIPDetailed(context.Background(), "US", "92503"); err == nil || !strings.Contains(err.Error(), "oversized") {
		t.Fatalf("oversized input: %v", err)
	}
	r = oediFixtureResolver(t)
	if _, err := r.ResolveZIPDetailed(context.Background(), "CA", "92503"); err == nil {
		t.Fatal("accepted non-US ZIP")
	}
	r = oediFixtureResolver(t)
	malformed := []byte("zip,eiaid,utility_name,state,service_type,ownership,comm_rate,ind_rate,res_rate\n92503,bad,Wrong,CA,Bundled,Investor Owned,0,0,0\n")
	if err := os.WriteFile(r.IOUPath, malformed, 0600); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(malformed)
	r.IOUSHA256 = hex.EncodeToString(digest[:])
	if _, err := r.ResolveZIPDetailed(context.Background(), "US", "92503"); err == nil || !strings.Contains(err.Error(), "matched row is invalid") {
		t.Fatalf("malformed matched row: %v", err)
	}
}

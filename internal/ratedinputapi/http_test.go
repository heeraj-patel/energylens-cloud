package ratedinputapi

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http/httptest"
	"testing"
)

type testStore struct{ facts []Fact }

func (s testStore) Find(_ context.Context, scope Scope) ([]Fact, error) { return s.facts, nil }

func TestLookupCarriesExactIdentityAndBodyDigest(t *testing.T) {
	fact := Fact{ID: "fact-id", Category: "vacuum", Manufacturer: "Shark", Model: "AZ1002", SourceKind: "oem_rated_input", MatchedModel: "AZ1000 Series", RatedInputWatts: "1350", Unit: "W"}
	r := httptest.NewRequest("GET", "/v1/hubs/hub/rated-input?category=vacuum&manufacturer=Shark&model=AZ1002", nil)
	w := httptest.NewRecorder()
	(Service{Store: testStore{facts: []Fact{fact}}}).get(w, r)
	if w.Code != 200 {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
	if w.Header().Get("Cache-Control") != "private, no-store" {
		t.Fatal("missing private no-store header")
	}
	sum := sha256.Sum256(w.Body.Bytes())
	if w.Header().Get("X-EnergyLens-Content-SHA256") != hex.EncodeToString(sum[:]) {
		t.Fatal("response digest mismatch")
	}
	var response struct {
		Facts []Fact `json:"facts"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if len(response.Facts) != 1 || response.Facts[0].Category != "vacuum" || response.Facts[0].Manufacturer != "Shark" || response.Facts[0].Model != "AZ1002" {
		t.Fatalf("identity omitted from response: %+v", response.Facts)
	}
}

package inventory

import "testing"

func TestFallbackIdentityFieldsAndBulbLabel(t *testing.T) {
	str := func(s string) *string { return &s }
	i := func(n int) *int { return &n }
	f := func(n float64) *float64 { return &n }
	base := Identity{Brand: "Acme", FullModel: "A100", Fuel: "electricity", MatchQuality: "unresolved", FCCID: str("2ABCD-X"), ICID: str("1234A-X"), SerialNumber: str("SN123"), UPC: str("012345678901"), BuildYear: i(2026)}
	if !ValidIdentity(base) {
		t.Fatal("optional scan fields rejected")
	}
	base.BulbLabel = &BulbLabel{LoadKind: "replaceable_bulb", BulbCount: i(2), CombinedInputWatts: f(19)}
	if !ValidIdentity(base) {
		t.Fatal("replaceable bulb label rejected")
	}
	base.BulbLabel.BulbCount = i(9)
	if ValidIdentity(base) {
		t.Fatal("bulb count above eight accepted")
	}
	base.BulbLabel = &BulbLabel{LoadKind: "integrated_lamp"}
	if !ValidIdentity(base) {
		t.Fatal("integrated lamp rejected")
	}
	base.BulbLabel.CombinedInputWatts = f(19)
	if ValidIdentity(base) {
		t.Fatal("integrated lamp wattage accepted")
	}
	base.BulbLabel = nil
	base.BuildYear = i(2101)
	if ValidIdentity(base) {
		t.Fatal("future build year accepted")
	}
}

func TestUnresolvedIdentityAcceptsCategoryOnlyAndModelOnly(t *testing.T) {
	base := Identity{Fuel: "electricity", MatchQuality: "unresolved"}
	if !ValidIdentity(base) || !ValidIdentity(Identity{FullModel: "A100", Fuel: "electricity", MatchQuality: "unresolved"}) {
		t.Fatal("category-only or model-only unresolved identity rejected")
	}
	if ValidIdentity(Identity{Fuel: "electricity", MatchQuality: "confirmed_exact"}) || ValidIdentity(Identity{Brand: "Acme", Fuel: "electricity", MatchQuality: "documented_variant_family"}) {
		t.Fatal("resolved identity accepted without make and model")
	}
	if ValidIdentity(Identity{Brand: " Acme ", Fuel: "electricity", MatchQuality: "unresolved"}) {
		t.Fatal("untrimmed partial identity accepted")
	}
}

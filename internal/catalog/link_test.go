package catalog

import "testing"

func TestSendableLinkTrimIdentityQuality(t *testing.T) {
	exact := LinkedProduct{Brand: "KitchenAid", FullModel: "KRMF706EBS", MatchQuality: "confirmed_exact"}
	if q, ok := SendableLink("KitchenAid", "KRMF706XYZ", "documented_variant_family", exact); !ok || q != "documented_variant_family" {
		t.Fatalf("trim link: %q %t", q, ok)
	}
	if _, ok := SendableLink("KitchenAid", "KRMF706XYZ", "confirmed_exact", exact); ok {
		t.Fatal("mismatched exact link must not be sent")
	}
	if q, ok := SendableLink("KitchenAid", "krmf706ebs", "confirmed_exact", exact); !ok || q != "confirmed_exact" {
		t.Fatalf("exact link: %q %t", q, ok)
	}
}

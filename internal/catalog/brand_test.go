package catalog

import (
	"reflect"
	"testing"
)

func TestBrandKeys(t *testing.T) {
	for name, want := range map[string][]string{
		"LG Electronics": {"lgelectronics", "lg", "lgsignature"},
		"lg":             {"lg", "lgelectronics", "lgsignature"},
		"TP Link":        {"tplink"},
		"Black & Decker": {"blackdecker"},
		"Acme-Co.":       {"acmeco"},
		"  ":             {},
	} {
		if got := BrandKeys(name); !reflect.DeepEqual(got, want) {
			t.Errorf("BrandKeys(%q) = %v, want %v", name, got, want)
		}
	}
	if CanonicalBrandKey("LG Electronics") != CanonicalBrandKey("lg signature") || CanonicalBrandKey("Acme") != "acme" {
		t.Fatal("canonical brand key")
	}
}

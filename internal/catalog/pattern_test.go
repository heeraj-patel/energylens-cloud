package catalog

import "testing"

func TestDocumentedPatternMatches(t *testing.T) {
	for _, model := range []string{"WM3070HWA", "wm3070hxa", "WM-3070HXA"} {
		if !documentedPatternMatches("WM3070H*A", model) {
			t.Errorf("expected %q to match", model)
		}
	}
	for _, model := range []string{"WM3070HXB", "WM3070HXXA", "XWM3070HWA", "WM3070H", "WM3070H[W]A"} {
		if documentedPatternMatches("WM3070H*A", model) {
			t.Errorf("unexpected match for %q", model)
		}
	}
	for _, pattern := range []string{"WM3070H.*A", "WM3070H[WX]A", "WM3070H+A", "W***A", "WM3070H(?=W)A"} {
		if documentedPatternMatches(pattern, "WM3070HWA") {
			t.Errorf("unreviewable pattern %q matched", pattern)
		}
	}
}

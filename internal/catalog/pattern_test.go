package catalog

import "testing"

func TestDocumentedPatternMatches(t *testing.T) {
	for _, model := range []string{"WM3070HWA", "wm3070hxa", "WM-3070HXA"} {
		if !documentedPatternMatches("WM3070H?A", model) {
			t.Errorf("expected %q to match", model)
		}
	}
	for _, model := range []string{"WM3070HXB", "WM3070HXXA", "WM3070HA", "XWM3070HWA", "WM3070H", "WM3070H[W]A"} {
		if documentedPatternMatches("WM3070H?A", model) {
			t.Errorf("unexpected match for %q", model)
		}
	}
	for _, model := range []string{"KSSC48QTS00", "KSSC48QT500"} {
		if !documentedPatternMatches("KSSC48QT?0?", model) {
			t.Errorf("reviewed EnerGuide pattern should match %q", model)
		}
	}
	for _, model := range []string{"KSSC48QTS11", "KSSC48QTSS00", "KSSC48FTS00", "KSSC48QT00"} {
		if documentedPatternMatches("KSSC48QT?0?", model) {
			t.Errorf("reviewed EnerGuide pattern should reject %q", model)
		}
	}
	for _, pattern := range []string{"WM3070H.*A", "WM3070H[WX]A", "WM3070H+A", "W***A", "W???A", "ABCDE*?*", "ABCD*", "WM3070H(?=W)A"} {
		if documentedPatternMatches(pattern, "WM3070HWA") {
			t.Errorf("unreviewable pattern %q matched", pattern)
		}
	}
}

func TestAsteriskIsOneOrMoreCharacters(t *testing.T) {
	for _, c := range []struct {
		pattern, model string
		want           bool
	}{
		{"ABCDE*", "ABCDE1", true},
		{"ABCDE*", "ABCDE123", true},
		{"ABCDE*", "ABCDE", false},
		{"WM3070H*A", "WM3070HWA", true},
		{"WM3070H*A", "WM3070HXXA", true},
		{"WM3070H*A", "WM3070HA", false},
		{"ABC*DEF*G", "ABC1DEF22G", true},
		{"ABC*DEF*G", "ABCDEF2G", false},
		{"ABCDE?", "ABCDE1", true},
		{"ABCDE?", "ABCDE123", false},
		{"ABCDE?", "ABCDE", false},
	} {
		if got := documentedPatternMatches(c.pattern, c.model); got != c.want {
			t.Errorf("%s vs %s: got %v", c.pattern, c.model, got)
		}
	}
}

func TestWildcardPrefixMatches(t *testing.T) {
	for _, c := range []struct {
		pattern, prefix string
		want            bool
	}{
		{"KSSC48QVS??", "KSSC48QVS", true},
		{"KSSC48QVS??", "KSSC48QVSX", true},
		{"KSSC48QVS??", "KSSC48QVSXYZ", false},
		{"KSSC48QVS*", "KSSC48QVSXYZ", true},
		{"AB*DEF", "ABXXDEF", true},
		{"AB*DEF", "AXXDEF", false},
		{"AB*DEF", "AB", true},
		{"AB?", "ABC", true},
		{"ABCDEF", "ABCDEFG", false},
	} {
		if got := wildcardPrefixMatches(c.pattern, c.prefix); got != c.want {
			t.Errorf("%s starts with %s: got %v", c.pattern, c.prefix, got)
		}
	}
}

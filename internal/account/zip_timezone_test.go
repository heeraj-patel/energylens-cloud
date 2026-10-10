package account

import "testing"

func TestResolveUSZIPTimeZone(t *testing.T) {
	for postal, want := range map[string]string{
		"78732":      "America/Chicago",
		"60601":      "America/Chicago",
		"94107":      "America/Los_Angeles",
		"10001":      "America/New_York",
		"00601":      "America/Puerto_Rico",
		"78732-1234": "America/Chicago",
	} {
		got, ok := resolveUSZIPTimeZone(postal)
		if !ok || got != want {
			t.Errorf("ZIP %s: got %q, %t; want %q", postal, got, ok, want)
		}
	}
	for _, postal := range []string{"85001", "00000", "abcde", "7873", "78732-xyz", ""} {
		if got, ok := resolveUSZIPTimeZone(postal); ok || got != "" {
			t.Errorf("unsupported ZIP %s resolved to %q", postal, got)
		}
	}
}

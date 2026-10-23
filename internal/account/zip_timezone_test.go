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
	for _, postal := range []string{"85001", "00000", "abcde", "7873", "78732-xyz", "78732-abcd", "78732-12x4", ""} {
		if got, ok := resolveUSZIPTimeZone(postal); ok || got != "" {
			t.Errorf("unsupported ZIP %s resolved to %q", postal, got)
		}
	}
}

func TestCreateTimezone(t *testing.T) {
	for _, tc := range []struct {
		country, postal, submitted, want string
	}{
		{"US", "78732", "", "America/Chicago"},
		{"US", "78732", "invalid", "America/Chicago"},
		{"US", "94107-1234", "America/New_York", "America/Los_Angeles"},
		{"US", "85001", "America/Phoenix", "America/Phoenix"},
		{"CA", "M5V 2T6", "America/Toronto", "America/Toronto"},
	} {
		in := HomeCreate{Timezone: tc.submitted, Location: Location{CountryCode: tc.country, PostalCode: tc.postal}}
		if got := createTimezone(in); got != tc.want {
			t.Errorf("createTimezone(%q, %q, %q) = %q, want %q", tc.country, tc.postal, tc.submitted, got, tc.want)
		}
	}
}

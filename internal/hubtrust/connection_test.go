package hubtrust

import (
	"strings"
	"testing"
)

func TestConnectionHintRequiresPrivateBareHTTPSOriginAndLeafPin(t *testing.T) {
	pin := strings.Repeat("a", 64)
	for _, origin := range []string{
		"https://192.168.68.136:18787",
	} {
		if !validConnection(origin, pin) {
			t.Fatalf("rejected valid LAN origin %q", origin)
		}
	}
	for _, origin := range []string{
		"http://192.168.68.136:18787",
		"https://127.0.0.1:18787",
		"https://169.254.1.2:18787",
		"https://8.8.8.8:18787",
		"https://100.66.70.66:18787",
		"https://[fd00::1]:18787",
		"https://example.com:18787",
		"https://user@192.168.68.136:18787",
		"https://192.168.68.136",
		"https://192.168.68.136:0",
		"https://192.168.68.136:18787/path",
		"https://192.168.68.136:18787?x=1",
		"https://192.168.68.136:18787?",
		"https://192.168.68.136:18787#pin",
	} {
		if validConnection(origin, pin) {
			t.Fatalf("accepted invalid origin %q", origin)
		}
	}
	if validConnection("https://192.168.68.136:18787", strings.Repeat("A", 64)) || validConnection("https://192.168.68.136:18787", "short") {
		t.Fatal("accepted invalid certificate pin")
	}
}

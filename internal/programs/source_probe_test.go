package programs

import (
	"context"
	"net/netip"
	"testing"
)

func TestPageProbeRejectsUnsafeSourceRegistrations(t *testing.T) {
	base := RegisteredSource{CanonicalURL: "https://utility.example/rebates", AllowedHosts: []string{"utility.example"}, AccessMethod: "html", MaxResponseBytes: 100_000, MaxRequests: 1}
	if _, err := validatedSourceURL(base); err != nil {
		t.Fatal(err)
	}
	for name, change := range map[string]func(*RegisteredSource){
		"non-HTTPS":        func(s *RegisteredSource) { s.CanonicalURL = "http://utility.example/rebates" },
		"credentials":      func(s *RegisteredSource) { s.CanonicalURL = "https://user:pass@utility.example/rebates" },
		"nonstandard port": func(s *RegisteredSource) { s.CanonicalURL = "https://utility.example:8443/rebates" },
		"IP literal":       func(s *RegisteredSource) { s.CanonicalURL = "https://8.8.8.8/rebates" },
		"lookalike host":   func(s *RegisteredSource) { s.CanonicalURL = "https://utility.example.evil.test/rebates" },
		"fragment":         func(s *RegisteredSource) { s.CanonicalURL = "https://utility.example/rebates#token" },
		"unsupported PDF":  func(s *RegisteredSource) { s.AccessMethod = "pdf" },
		"excess body":      func(s *RegisteredSource) { s.MaxResponseBytes = 5 << 20 },
	} {
		t.Run(name, func(t *testing.T) {
			source := base
			change(&source)
			if _, err := validatedSourceURL(source); err == nil {
				t.Fatal("unsafe source accepted")
			}
		})
	}
}

func TestPageProbeRejectsNonPublicAddresses(t *testing.T) {
	for _, value := range []string{"127.0.0.1", "10.0.0.1", "169.254.169.254", "100.64.1.1", "192.0.0.8", "198.19.0.1", "::1", "fc00::1", "fe80::1", "2001:db8::1", "::ffff:127.0.0.1"} {
		if publicSourceIP(netip.MustParseAddr(value)) {
			t.Errorf("accepted non-public address %s", value)
		}
	}
	for _, value := range []string{"8.8.8.8", "1.1.1.1", "2606:4700:4700::1111"} {
		if !publicSourceIP(netip.MustParseAddr(value)) {
			t.Errorf("rejected public address %s", value)
		}
	}
}

func TestPageProbeNeverConnectsToLoopback(t *testing.T) {
	err := (HTTPSPageProbe{}).Check(context.Background(), RegisteredSource{
		CanonicalURL: "https://localhost/rebates", AllowedHosts: []string{"localhost"},
		AccessMethod: "html", MaxResponseBytes: 100_000, MaxRequests: 1,
	})
	if err == nil {
		t.Fatal("loopback source accepted")
	}
}

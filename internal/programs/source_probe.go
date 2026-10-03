package programs

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"time"
)

// HTTPSPageProbe checks one terms-approved public page. Checking reachability
// does not extract, review, or publish any program terms. It is deliberately
// separate from the operator-reviewed snapshot import.
type HTTPSPageProbe struct{}

var excludedSourceNetworks = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"), netip.MustParsePrefix("100.64.0.0/10"),
	netip.MustParsePrefix("192.0.0.0/24"), netip.MustParsePrefix("198.18.0.0/15"),
	netip.MustParsePrefix("192.0.2.0/24"), netip.MustParsePrefix("198.51.100.0/24"),
	netip.MustParsePrefix("203.0.113.0/24"),
	netip.MustParsePrefix("224.0.0.0/4"), netip.MustParsePrefix("240.0.0.0/4"),
	netip.MustParsePrefix("::/128"), netip.MustParsePrefix("fc00::/7"),
	netip.MustParsePrefix("fe80::/10"), netip.MustParsePrefix("2001:db8::/32"),
}

func publicSourceIP(ip netip.Addr) bool {
	ip = ip.Unmap()
	if !ip.IsValid() || !ip.IsGlobalUnicast() || ip.IsPrivate() || ip.IsLoopback() || ip.IsLinkLocalUnicast() {
		return false
	}
	for _, block := range excludedSourceNetworks {
		if block.Contains(ip) {
			return false
		}
	}
	return true
}

func validatedSourceURL(source RegisteredSource) (*url.URL, error) {
	if source.AccessMethod != "html" && source.AccessMethod != "feed" && source.AccessMethod != "api" {
		return nil, errors.New("source access method is not a page")
	}
	if source.MaxRequests < 1 || source.MaxResponseBytes < 1 || source.MaxResponseBytes > 4<<20 {
		return nil, errors.New("invalid source fetch budget")
	}
	u, err := url.Parse(source.CanonicalURL)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.Fragment != "" || u.Opaque != "" || u.Port() != "" {
		return nil, errors.New("invalid source URL")
	}
	if net.ParseIP(u.Hostname()) != nil || strings.HasSuffix(u.Hostname(), ".") || strings.ToLower(u.Hostname()) != u.Hostname() {
		return nil, errors.New("source requires a canonical DNS host")
	}
	allowed := false
	for _, host := range source.AllowedHosts {
		if host == u.Hostname() {
			allowed = true
		}
	}
	if !allowed {
		return nil, errors.New("source host is not allowlisted")
	}
	return u, nil
}

func (HTTPSPageProbe) Check(ctx context.Context, source RegisteredSource) error {
	u, err := validatedSourceURL(source)
	if err != nil {
		return err
	}
	addresses, err := net.DefaultResolver.LookupNetIP(ctx, "ip", u.Hostname())
	if err != nil || len(addresses) == 0 {
		return errors.New("source DNS lookup failed")
	}
	// Reject the complete answer if any address is non-public. The transport
	// connects to one vetted IP, so a second DNS lookup cannot rebind the host.
	for _, ip := range addresses {
		if !publicSourceIP(ip) {
			return errors.New("source DNS contains a non-public address")
		}
	}
	address := net.JoinHostPort(addresses[0].String(), "443")
	transport := &http.Transport{
		Proxy: nil, TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12},
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{Timeout: 5 * time.Second}).DialContext(ctx, "tcp", address)
		},
		DisableKeepAlives: true,
	}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: 10 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "text/html, application/json, application/xml, text/xml")
	req.Header.Set("User-Agent", "EnergyLens-program-coverage/1")
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("source check failed: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("source returned HTTP %d", resp.StatusCode)
	}
	mediaType := strings.ToLower(strings.TrimSpace(strings.Split(resp.Header.Get("Content-Type"), ";")[0]))
	if mediaType != "text/html" && mediaType != "application/json" && mediaType != "application/xml" && mediaType != "text/xml" {
		return errors.New("source returned unsupported content type")
	}
	if resp.ContentLength > source.MaxResponseBytes {
		return errors.New("source response exceeds byte budget")
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, source.MaxResponseBytes+1))
	if err != nil || len(body) == 0 || int64(len(body)) > source.MaxResponseBytes {
		return errors.New("source response is empty or exceeds byte budget")
	}
	return nil
}

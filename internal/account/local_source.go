package account

import (
	"errors"
	"net"
	"net/http"
	"net/netip"
	"strings"
)

// LocalAuthSourcePolicy trusts at most one explicitly configured proxy hop.
// A trusted proxy must append the actual socket client's IP as the sole XFF
// value. Multi-hop and prepopulated XFF chains fail closed.
type LocalAuthSourcePolicy struct{ trusted []netip.Prefix }

func ParseLocalAuthSourcePolicy(raw string) (LocalAuthSourcePolicy, error) {
	if raw == "" {
		return LocalAuthSourcePolicy{}, nil
	}
	if len(raw) > 1024 {
		return LocalAuthSourcePolicy{}, errors.New("trusted proxy CIDR list too long")
	}
	parts := strings.Split(raw, ",")
	if len(parts) > 16 {
		return LocalAuthSourcePolicy{}, errors.New("too many trusted proxy CIDRs")
	}
	policy := LocalAuthSourcePolicy{trusted: make([]netip.Prefix, 0, len(parts))}
	seen := map[string]bool{}
	for _, part := range parts {
		if part == "" || part != strings.TrimSpace(part) {
			return LocalAuthSourcePolicy{}, errors.New("invalid trusted proxy CIDR")
		}
		prefix, err := netip.ParsePrefix(part)
		if err != nil || !prefix.IsValid() || prefix.Addr().Zone() != "" || prefix.Addr().Is4In6() || prefix.Masked() != prefix || prefix.String() != part || seen[part] ||
			prefix.Addr().Is4() && prefix.Bits() < 16 || prefix.Addr().Is6() && prefix.Bits() < 48 {
			return LocalAuthSourcePolicy{}, errors.New("invalid trusted proxy CIDR")
		}
		seen[part] = true
		policy.trusted = append(policy.trusted, prefix)
	}
	return policy, nil
}

func (p LocalAuthSourcePolicy) Source(r *http.Request) (string, error) {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return "", ErrInvalidCredential
	}
	peer, err := netip.ParseAddr(host)
	if err != nil || peer.Zone() != "" || peer.IsUnspecified() {
		return "", ErrInvalidCredential
	}
	peer = peer.Unmap()
	trusted := false
	for _, prefix := range p.trusted {
		if prefix.Contains(peer) {
			trusted = true
			break
		}
	}
	if !trusted {
		return peer.String(), nil
	}
	values := r.Header.Values("X-Forwarded-For")
	if len(values) != 1 || strings.Contains(values[0], ",") {
		return "", ErrInvalidCredential
	}
	client, err := netip.ParseAddr(strings.TrimSpace(values[0]))
	if err != nil || client.Zone() != "" || client.IsUnspecified() {
		return "", ErrInvalidCredential
	}
	return client.Unmap().String(), nil
}

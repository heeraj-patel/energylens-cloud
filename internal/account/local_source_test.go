package account

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestLocalAuthSourcePolicyTrustsOnlyOneConfiguredHop(t *testing.T) {
	p, err := ParseLocalAuthSourcePolicy("10.20.0.0/16,2001:db8:abcd::/48")
	if err != nil {
		t.Fatal(err)
	}
	request := func(peer string, xff ...string) (string, error) {
		r := httptest.NewRequest("POST", "/v1/auth/local/login", nil)
		r.RemoteAddr = peer
		for _, value := range xff {
			r.Header.Add("X-Forwarded-For", value)
		}
		return p.Source(r)
	}
	if got, err := request("198.51.100.5:1234", "203.0.113.77"); err != nil || got != "198.51.100.5" {
		t.Fatalf("untrusted spoofed header used: %q %v", got, err)
	}
	if got, err := request("10.20.3.4:1234", "203.0.113.77"); err != nil || got != "203.0.113.77" {
		t.Fatalf("trusted single hop rejected: %q %v", got, err)
	}
	if got, err := request("[2001:db8:abcd::1]:1234", "2001:db8::9"); err != nil || got != "2001:db8::9" {
		t.Fatalf("trusted IPv6 hop rejected: %q %v", got, err)
	}
	for _, xff := range [][]string{nil, {"203.0.113.77,198.51.100.8"}, {"203.0.113.77", "198.51.100.8"}, {"unknown"}, {"0.0.0.0"}, {"fe80::1%en0"}} {
		if got, err := request("10.20.3.4:1234", xff...); err == nil {
			t.Fatalf("unsafe forwarded chain accepted: %q", got)
		}
	}
	if got, err := request("not-a-socket", "203.0.113.77"); err == nil {
		t.Fatalf("invalid peer accepted: %q", got)
	}
}

func TestLocalAuthSourcePolicyRejectsBroadOrNoncanonicalTrust(t *testing.T) {
	for _, raw := range []string{"0.0.0.0/0", "10.0.0.0/8", "::/0", "2001:db8::/32", "10.20.0.1/16", "10.20.0.0/16,", "10.20.0.0/16,10.20.0.0/16", " 10.20.0.0/16", "::ffff:10.20.0.0/112"} {
		if _, err := ParseLocalAuthSourcePolicy(raw); err == nil {
			t.Fatalf("unsafe trusted range accepted: %s", raw)
		}
	}
	if p, err := ParseLocalAuthSourcePolicy(""); err != nil || len(p.trusted) != 0 {
		t.Fatalf("default direct-peer policy: %+v %v", p, err)
	}
}

func TestLocalRegistrationUnusableSourceIsBadRequest(t *testing.T) {
	p, err := ParseLocalAuthSourcePolicy("10.20.0.0/16")
	if err != nil {
		t.Fatal(err)
	}
	for name, tc := range map[string]struct {
		profile LocalAuthProfile
		body    string
	}{
		"username": {LocalUsernameProfile, `{"username":"alice","password":"correct long password"}`},
		"email":    {LocalEmailProfile, `{"email":"alice@example.test","password":"correct long password"}`},
	} {
		h := LocalAuthHTTP{SourcePolicy: p}
		h.Login.Codec.Profile = tc.profile
		r := httptest.NewRequest("POST", "/v1/auth/local/registration", strings.NewReader(tc.body))
		r.Header.Set("Content-Type", "application/json")
		r.RemoteAddr = "10.20.3.4:1234"
		w := httptest.NewRecorder()
		h.registration(w, r)
		var body apiError
		if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		if w.Code != 400 || body.Code != "invalid_request" || body.Message != "client address could not be determined" {
			t.Fatalf("%s: %d %s", name, w.Code, w.Body.String())
		}
	}
}

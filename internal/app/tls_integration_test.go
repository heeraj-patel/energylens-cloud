package app

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"energylens.local/cloud/internal/hubtrust"
	"github.com/go-chi/chi/v5"
)

func TestSingleListenerTLSClientCertificateAccess(t *testing.T) {
	trustedCA, trustedKey := testCertificateAuthority(t, "trusted")
	serverCert, serverKey := testSignedCertificate(t, trustedCA, trustedKey, "localhost", []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth})
	clientCert, clientKey := testSignedCertificate(t, trustedCA, trustedKey, "hub", []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth})
	untrustedCA, untrustedKey := testCertificateAuthority(t, "untrusted")
	untrustedCert, untrustedKeyPEM := testSignedCertificate(t, untrustedCA, untrustedKey, "attacker", []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth})

	dir := t.TempDir()
	certPath := writeTestPEM(t, dir, "server.crt", serverCert)
	keyPath := writeTestPEM(t, dir, "server.key", serverKey)
	caPath := writeTestPEM(t, dir, "client-ca.crt", trustedCA)
	tlsConfig, err := (Config{HubCertFile: certPath, HubKeyFile: keyPath, HubCAFile: caPath}).hubTLSConfig()
	if err != nil {
		t.Fatalf("build production listener TLS config: %v", err)
	}

	handler := cloudRouter([]Module{{Name: "public-test", Mount: func(router chi.Router) {
		router.Get("/test-public", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })
	}}}, func(context.Context) error { return nil }, &hubtrust.Service{}, nil)
	server := httptest.NewUnstartedServer(handler)
	server.TLS = tlsConfig
	server.StartTLS()
	defer server.Close()

	roots := x509.NewCertPool()
	rootBlock, _ := pem.Decode(trustedCA)
	trustedRoot, err := x509.ParseCertificate(rootBlock.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	roots.AddCert(trustedRoot)
	clientPair, err := tls.X509KeyPair(clientCert, clientKey)
	if err != nil {
		t.Fatal(err)
	}
	untrustedPair, err := tls.X509KeyPair(untrustedCert, untrustedKeyPEM)
	if err != nil {
		t.Fatal(err)
	}
	makeClient := func(pair *tls.Certificate) *http.Client {
		config := &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12}
		if pair != nil {
			config.Certificates = []tls.Certificate{*pair}
		}
		return &http.Client{Transport: &http.Transport{TLSClientConfig: config}}
	}

	for _, tc := range []struct {
		name   string
		client *http.Client
		path   string
		want   int
	}{
		{"trusted client reaches Hub handler", makeClient(&clientPair), "/v1/hubs/not-a-uuid/ownership", http.StatusNotFound},
		{"no certificate reaches public route", makeClient(nil), "/v1/test-public", http.StatusNoContent},
		{"no certificate is denied Hub route", makeClient(nil), "/v1/hubs/not-a-uuid/ownership", http.StatusForbidden},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resp, err := tc.client.Get(server.URL + tc.path)
			if err != nil {
				t.Fatalf("request failed: %v", err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != tc.want {
				t.Fatalf("status = %d, want %d", resp.StatusCode, tc.want)
			}
		})
	}

	spoof := makeClient(nil)
	request, err := http.NewRequest(http.MethodGet, server.URL+"/v1/hubs/not-a-uuid/ownership", nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"X-Amzn-Mtls-Clientcert", "X-Energylens-Identity-User", "X-Forwarded-User", "X-Authenticated-User"} {
		request.Header.Set(name, "forged")
	}
	resp, err := spoof.Do(request)
	if err != nil {
		t.Fatalf("spoofed-header request failed: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("spoofed identity headers granted Hub access: status %d", resp.StatusCode)
	}

	attacker := makeClient(&untrustedPair)
	attacker.Transport.(*http.Transport).TLSClientConfig.GetClientCertificate = func(*tls.CertificateRequestInfo) (*tls.Certificate, error) {
		return &untrustedPair, nil
	}
	if resp, err := attacker.Get(server.URL + "/v1/test-public"); err == nil {
		resp.Body.Close()
		t.Fatal("untrusted client certificate completed TLS handshake")
	}
}

func testCertificateAuthority(t *testing.T, commonName string) ([]byte, *ecdsa.PrivateKey) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 120))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	template := &x509.Certificate{SerialNumber: serial, Subject: pkix.Name{CommonName: commonName}, NotBefore: now.Add(-time.Minute), NotAfter: now.Add(time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), key
}

func testSignedCertificate(t *testing.T, caPEM []byte, caKey *ecdsa.PrivateKey, commonName string, usages []x509.ExtKeyUsage) ([]byte, []byte) {
	t.Helper()
	ca, _ := pem.Decode(caPEM)
	parent, err := x509.ParseCertificate(ca.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 120))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	template := &x509.Certificate{SerialNumber: serial, Subject: pkix.Name{CommonName: commonName}, DNSNames: []string{"localhost"}, IPAddresses: []net.IP{net.ParseIP("127.0.0.1")}, NotBefore: now.Add(-time.Minute), NotAfter: now.Add(time.Hour), KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: usages}
	der, err := x509.CreateCertificate(rand.Reader, template, parent, &key.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})
}

func writeTestPEM(t *testing.T, dir, name string, data []byte) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

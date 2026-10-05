package catalog

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"energylens.local/cloud/internal/hubtrust"
	"github.com/go-chi/chi/v5"
)

type certificateStore struct {
	hub   hubtrust.Hub
	err   error
	reads int
}

func (s *certificateStore) VerifyCertificate(_ context.Context, hub, serial string, epoch int64) (hubtrust.Hub, error) {
	s.reads++
	return s.hub, s.err
}

func TestHubCatalogTrustBoundary(t *testing.T) {
	const hubID = "11111111-1111-4111-8111-111111111111"
	const productID = "22222222-2222-4222-8222-222222222222"
	home := "33333333-3333-4333-8333-333333333333"
	identity, _ := url.Parse("spiffe://energylens.local/hubs/" + hubID + "/epochs/2")
	peer := &x509.Certificate{URIs: []*url.URL{identity}, SerialNumber: big.NewInt(42)}
	verified := &tls.ConnectionState{PeerCertificates: []*x509.Certificate{peer}, VerifiedChains: [][]*x509.Certificate{{peer}}}
	for _, tc := range []struct {
		name    string
		tls     *tls.ConnectionState
		epoch   int64
		claimed bool
		err     error
		pathHub string
		want    int
	}{
		{"no certificate", nil, 2, true, nil, hubID, 403},
		{"unverified certificate", &tls.ConnectionState{PeerCertificates: []*x509.Certificate{peer}}, 2, true, nil, hubID, 403},
		{"wrong hub", verified, 2, true, nil, productID, 403},
		{"revoked certificate", verified, 2, true, hubtrust.ErrDenied, hubID, 403},
		{"old ownership epoch", verified, 3, true, nil, hubID, 403},
		{"unclaimed hub", verified, 2, false, nil, hubID, 403},
		{"database unavailable", verified, 2, true, errors.New("offline"), hubID, 503},
		{"current owned hub", verified, 2, true, nil, hubID, 200},
	} {
		t.Run(tc.name, func(t *testing.T) {
			certificates := &certificateStore{hub: hubtrust.Hub{ID: hubID, Epoch: tc.epoch}, err: tc.err}
			if tc.claimed {
				certificates.hub.HomeID = &home
			}
			for _, method := range []string{http.MethodGet, http.MethodPost} {
				store := &fakeStore{}
				s := Service{Store: store, HubAuth: HubAuthentication(certificates)}
				router := chi.NewRouter()
				router.Route("/v1", s.HubRoutes)
				path := "/v1/hubs/" + tc.pathHub + "/catalog/products/" + productID
				if method == http.MethodPost {
					path = "/v1/hubs/" + tc.pathHub + "/catalog/model-search"
				}
				req := httptest.NewRequest(method, path, strings.NewReader(`{"brand":"Acme","model":"A123"}`))
				req.TLS = tc.tls
				req.Header.Set("Authorization", "Bearer cannot-replace-certificate")
				req.Header.Set("X-Amzn-Mtls-Clientcert", "cannot-replace-certificate")
				req.Header.Set("Content-Type", "application/json")
				req.Header.Set("Idempotency-Key", productID)
				w := httptest.NewRecorder()
				router.ServeHTTP(w, req)
				if w.Code != tc.want {
					t.Fatalf("%s status %d: %s", method, w.Code, w.Body.String())
				}
				if tc.want != 200 && store.reads != 0 {
					t.Fatal("denied request read catalog")
				}
				if tc.want == 200 && store.reads != 1 {
					t.Fatal("authorized request did not use shared catalog store")
				}
				if w.Header().Get("Cache-Control") != "private, no-store" {
					t.Fatal("missing authenticated cache policy")
				}
			}
		})
	}
}

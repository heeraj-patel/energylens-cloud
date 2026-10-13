package usagefactapi

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"energylens.local/cloud/internal/hubtrust"
	"github.com/go-chi/chi/v5"
)

type fakeFacts struct{ calls int }

func (f *fakeFacts) Find(_ context.Context, _ Scope) ([]Fact, error) { f.calls++; return nil, nil }

type fakeCertificate struct {
	hub hubtrust.Hub
	err error
}

func (f fakeCertificate) VerifyCertificate(_ context.Context, _, _ string, _ int64) (hubtrust.Hub, error) {
	return f.hub, f.err
}

const query = "?category=washer&manufacturer=Acme&model=W123&fuel=electricity&variant=US-120V"

func TestOwnerRequiresBearerBeforeLookup(t *testing.T) {
	store := &fakeFacts{}
	auth := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("Authorization") != "Bearer valid" {
				w.WriteHeader(401)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
	router := chi.NewRouter()
	router.Route("/v1", (Service{Store: store, Auth: auth}).Routes)
	for _, tc := range []struct {
		token, path string
		status      int
	}{{"", "/v1/usage-facts" + query, 401}, {"Bearer invalid", "/v1/usage-facts" + query, 401}, {"Bearer valid", "/v1/usage-facts?category=washer", 400}, {"Bearer valid", "/v1/usage-facts" + query, 200}} {
		r := httptest.NewRequest("GET", tc.path, nil)
		r.Header.Set("Authorization", tc.token)
		w := httptest.NewRecorder()
		router.ServeHTTP(w, r)
		if w.Code != tc.status {
			t.Fatalf("status %d want %d: %s", w.Code, tc.status, w.Body.String())
		}
	}
	if store.calls != 1 {
		t.Fatalf("lookup calls = %d", store.calls)
	}
}
func TestHubRequiresCurrentClaimedCertificate(t *testing.T) {
	const hubID = "11111111-1111-4111-8111-111111111111"
	home := "22222222-2222-4222-8222-222222222222"
	identity, _ := url.Parse("spiffe://energylens.local/hubs/" + hubID + "/epochs/2")
	peer := &x509.Certificate{URIs: []*url.URL{identity}, SerialNumber: big.NewInt(42)}
	verified := &tls.ConnectionState{PeerCertificates: []*x509.Certificate{peer}, VerifiedChains: [][]*x509.Certificate{{peer}}}
	for _, tc := range []struct {
		name string
		tls  *tls.ConnectionState
		hub  hubtrust.Hub
		err  error
		want int
	}{
		{"missing", nil, hubtrust.Hub{}, nil, 403},
		{"unverified", &tls.ConnectionState{PeerCertificates: []*x509.Certificate{peer}}, hubtrust.Hub{}, nil, 403},
		{"revoked", verified, hubtrust.Hub{}, hubtrust.ErrDenied, 403},
		{"old epoch", verified, hubtrust.Hub{ID: hubID, Epoch: 3, HomeID: &home}, nil, 403},
		{"unclaimed", verified, hubtrust.Hub{ID: hubID, Epoch: 2}, nil, 403},
		{"database unavailable", verified, hubtrust.Hub{}, errors.New("offline"), 503},
		{"current", verified, hubtrust.Hub{ID: hubID, Epoch: 2, HomeID: &home}, nil, 200},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := &fakeFacts{}
			svc := Service{Store: store, HubCertificates: fakeCertificate{tc.hub, tc.err}}
			router := chi.NewRouter()
			router.Route("/v1", svc.HubRoutes)
			r := httptest.NewRequest("GET", "/v1/hubs/"+hubID+"/usage-facts"+query, nil)
			r.TLS = tc.tls
			r.Header.Set("Authorization", "Bearer ignored")
			w := httptest.NewRecorder()
			router.ServeHTTP(w, r)
			if w.Code != tc.want {
				t.Fatalf("status %d want %d: %s", w.Code, tc.want, w.Body.String())
			}
			if store.calls != 0 && tc.want != 200 || store.calls != 1 && tc.want == 200 {
				t.Fatalf("lookup calls = %d", store.calls)
			}
		})
	}
}

type returnedFacts []Fact

func (f returnedFacts) Find(context.Context, Scope) ([]Fact, error) { return f, nil }
func TestThreeKindsIncludingStandbyAreServed(t *testing.T) {
	mode := "network standby"
	facts := returnedFacts{{MeasurementKind: "cycle_energy"}, {MeasurementKind: "hourly_energy"}, {MeasurementKind: "standby_power", Value: "0.5", Unit: "W", OperationMode: &mode, IncludedEnergy: json.RawMessage(`{"status":"documented","components":["standby_electricity"]}`)}}
	for _, size := range []int{3, 4} {
		rows := append(returnedFacts(nil), facts...)
		if size == 4 {
			rows = append(rows, Fact{})
		}
		svc := Service{Store: rows}
		w := httptest.NewRecorder()
		svc.get(w, httptest.NewRequest("GET", "/usage-facts"+query, nil))
		if size == 4 {
			if w.Code != 500 {
				t.Fatal("overfull fact scope accepted")
			}
			continue
		}
		var response struct {
			Facts []Fact `json:"facts"`
		}
		digest := sha256.Sum256(w.Body.Bytes())
		if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &response) != nil || len(response.Facts) != 3 || response.Facts[2].Unit != "W" || *response.Facts[2].OperationMode != mode || w.Header().Get("X-EnergyLens-Content-SHA256") != hex.EncodeToString(digest[:]) {
			t.Fatalf("standby API response: %s", w.Body.String())
		}
	}
}

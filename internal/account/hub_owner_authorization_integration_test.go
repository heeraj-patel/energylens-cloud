package account

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"energylens.local/cloud/internal/hubtrust"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

// The route uses the real account middleware, a live local session family,
// and an mTLS peer whose signed identity and serial match the Hub database.
func TestHubOwnerAuthorizationUsesLiveSessionAndCurrentCertificate(t *testing.T) {
	pool, _ := sessionTestPool(t)
	ctx := context.Background()
	owner, codec, login, refresh := usernameSessionFixture(t, pool)
	other := uuid.NewString()
	hash, err := HashLocalPassword("another long password")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = pool.Exec(ctx, `INSERT INTO users(id,email_verified) VALUES($1,false)`, other); err != nil {
		t.Fatal(err)
	}
	if _, err = pool.Exec(ctx, `INSERT INTO auth_identities(user_id,issuer,subject) VALUES($1,$2,$1::uuid::text)`, other, LocalUsernameIssuer); err != nil {
		t.Fatal(err)
	}
	if _, err = pool.Exec(ctx, `INSERT INTO auth_password_credentials(user_id,credential_kind,username_normalized,activated_at,password_hash) VALUES($1,'username_local','other-owner',now(),$2)`, other, hash); err != nil {
		t.Fatal(err)
	}
	ownerSession, err := login.Login(ctx, "session-owner", "original long password", "192.0.2.1", time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	otherSession, err := login.Login(ctx, "other-owner", "another long password", "192.0.2.2", time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	var home, hub string
	if err = pool.QueryRow(ctx, `INSERT INTO homes(owner_user_id,name,timezone) VALUES($1,'House','America/Chicago') RETURNING id`, owner).Scan(&home); err != nil {
		t.Fatal(err)
	}
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	if err = pool.QueryRow(ctx, `INSERT INTO hubs(local_hub_id,device_public_key,home_id,ownership_epoch,registered_by) VALUES('elhub_route', $1, $2, 2, 'test') RETURNING id`, pub, home).Scan(&hub); err != nil {
		t.Fatal(err)
	}

	caPub, caKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	caTemplate := &x509.Certificate{SerialNumber: big.NewInt(100), Subject: pkix.Name{CommonName: "test hub CA"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	caDER, err := x509.CreateCertificate(rand.Reader, caTemplate, caTemplate, caPub, caKey)
	if err != nil {
		t.Fatal(err)
	}
	ca, err := x509.ParseCertificate(caDER)
	if err != nil {
		t.Fatal(err)
	}
	issue := func(serial int64, epoch int64) tls.Certificate {
		t.Helper()
		clientPub, clientKey, e := ed25519.GenerateKey(rand.Reader)
		if e != nil {
			t.Fatal(e)
		}
		identity, e := url.Parse("spiffe://energylens.local/hubs/" + hub + "/epochs/" + big.NewInt(epoch).String())
		if e != nil {
			t.Fatal(e)
		}
		template := &x509.Certificate{SerialNumber: big.NewInt(serial), Subject: pkix.Name{CommonName: "EnergyLens Hub"}, NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(time.Hour), URIs: []*url.URL{identity}, KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}}
		der, e := x509.CreateCertificate(rand.Reader, template, ca, clientPub, caKey)
		if e != nil {
			t.Fatal(e)
		}
		if _, e = pool.Exec(ctx, `INSERT INTO hub_certificates(serial,hub_id,ownership_epoch,expires_at,certificate_pem) VALUES($1,$2,$3,now()+interval '1 hour',$4)`, big.NewInt(serial).Text(16), hub, epoch, string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))); e != nil {
			t.Fatal(e)
		}
		return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: clientKey}
	}
	current := issue(0xa11, 2)
	stale := issue(0xa12, 1)
	accounts := Service{Store: PGStore{Pool: pool}, Verifier: LocalAccessVerifier{DB: pool, Codec: codec}}
	hubService := hubtrust.Service{Store: hubtrust.Store{Pool: pool}, Auth: accounts.Auth, UserID: UserID, SessionID: SessionID}
	router := chi.NewRouter()
	router.Route("/v1", hubService.HubRoutes)
	server := httptest.NewUnstartedServer(router)
	caPool := x509.NewCertPool()
	caPool.AddCert(ca)
	server.TLS = &tls.Config{ClientAuth: tls.RequireAndVerifyClientCert, ClientCAs: caPool}
	server.StartTLS()
	defer server.Close()
	request := func(cert tls.Certificate, access, requestedHome string) (int, map[string]any, http.Header) {
		t.Helper()
		transport := server.Client().Transport.(*http.Transport).Clone()
		transport.TLSClientConfig.Certificates = []tls.Certificate{cert}
		client := &http.Client{Transport: transport, Timeout: 5 * time.Second}
		defer client.CloseIdleConnections()
		body, _ := json.Marshal(map[string]string{"home_id": requestedHome})
		req, e := http.NewRequest(http.MethodPost, server.URL+"/v1/hubs/"+hub+"/owner-authorizations", bytes.NewReader(body))
		if e != nil {
			t.Fatal(e)
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+access)
		response, e := client.Do(req)
		if e != nil {
			t.Fatal(e)
		}
		defer response.Body.Close()
		var result map[string]any
		if e = json.NewDecoder(response.Body).Decode(&result); e != nil {
			t.Fatal(e)
		}
		return response.StatusCode, result, response.Header
	}
	status, result, headers := request(current, ownerSession.AccessToken, home)
	if status != 200 || result["user_id"] != owner || result["home_id"] != home || result["hub_id"] != hub || result["ownership_epoch"] != float64(2) || result["session_id"] == "" || headers.Get("Cache-Control") != "no-store" {
		t.Fatalf("valid owner route: %d %+v", status, result)
	}
	if status, _, _ = request(current, otherSession.AccessToken, home); status != 404 {
		t.Fatalf("foreign account: %d", status)
	}
	if status, _, _ = request(stale, ownerSession.AccessToken, home); status != 404 {
		t.Fatalf("stale Hub epoch: %d", status)
	}
	if err = refresh.Logout(ctx, ownerSession.RefreshToken, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if status, _, _ = request(current, ownerSession.AccessToken, home); status != 401 {
		t.Fatalf("revoked account session: %d", status)
	}
}

package hubtrust

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"energylens.local/cloud/internal/migrate"
	"energylens.local/cloud/migrations"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestProofTranscripts(t *testing.T) {
	pub, priv, e := ed25519.GenerateKey(rand.Reader)
	if e != nil {
		t.Fatal(e)
	}
	proof := b64(bytes.Repeat([]byte{7}, 32))
	nonce := b64(bytes.Repeat([]byte{8}, 32))
	id := uuid.NewString()
	local := "elhub_test"
	msg := claimTranscript(id, local, nonce, proof)
	expected := "energylens.hub.claim.v1\n" + id + "\n" + local + "\n" + nonce + "\n" + shaHex(proof) + "\n"
	if string(msg) != expected {
		t.Fatal("claim transcript differs from contract")
	}
	sig := ed25519.Sign(priv, msg)
	if !ed25519.Verify(pub, msg, sig) || ed25519.Verify(pub, claimTranscript(id, local, nonce, b64(bytes.Repeat([]byte{9}, 32))), sig) {
		t.Fatal("claim proof not bound")
	}
	csrHash := sha256.Sum256([]byte("csr"))
	emsg := enrollTranscript(id, local, nonce, "2026-09-30T00:00:00Z", hex.EncodeToString(csrHash[:]))
	if !strings.HasPrefix(string(emsg), "energylens.hub.enroll.v1\n"+id+"\n") {
		t.Fatal("enrollment transcript differs from contract")
	}
	if !ed25519.Verify(pub, emsg, ed25519.Sign(priv, emsg)) {
		t.Fatal("enrollment signature")
	}
}
func TestHubTrustTransaction(t *testing.T) {
	dsn := os.Getenv("ENERGYLENS_CLOUD_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("ENERGYLENS_CLOUD_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	admin, e := pgxpool.New(ctx, dsn)
	if e != nil {
		t.Fatal(e)
	}
	defer admin.Close()
	schema := "trust_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	if _, e = admin.Exec(ctx, "CREATE SCHEMA "+schema); e != nil {
		t.Fatal(e)
	}
	defer admin.Exec(ctx, "DROP SCHEMA "+schema+" CASCADE")
	cfg, e := pgxpool.ParseConfig(dsn)
	if e != nil {
		t.Fatal(e)
	}
	cfg.ConnConfig.RuntimeParams["search_path"] = schema
	pool, e := pgxpool.NewWithConfig(ctx, cfg)
	if e != nil {
		t.Fatal(e)
	}
	defer pool.Close()
	conn, e := pgx.ConnectConfig(ctx, cfg.ConnConfig)
	if e != nil {
		t.Fatal(e)
	}
	defer conn.Close(ctx)
	if e = migrate.Apply(ctx, conn, migrations.FS); e != nil {
		t.Fatal(e)
	}
	var owner, other, home string
	e = pool.QueryRow(ctx, `INSERT INTO users DEFAULT VALUES RETURNING id`).Scan(&owner)
	if e != nil {
		t.Fatal(e)
	}
	e = pool.QueryRow(ctx, `INSERT INTO users DEFAULT VALUES RETURNING id`).Scan(&other)
	if e != nil {
		t.Fatal(e)
	}
	e = pool.QueryRow(ctx, `INSERT INTO homes(owner_user_id,name,timezone) VALUES($1,'House','America/Chicago') RETURNING id`, owner).Scan(&home)
	if e != nil {
		t.Fatal(e)
	}
	pub, priv, e := ed25519.GenerateKey(rand.Reader)
	if e != nil {
		t.Fatal(e)
	}
	s := Store{Pool: pool}
	id, nonce, e := s.Provision(ctx, "elhub_unit", pub, "test-admin")
	if e != nil {
		t.Fatal(e)
	}
	if _, e = s.Begin(ctx, other, home, "elhub_unit", uuid.NewString()); !errorsIs(e, ErrNotFound) {
		t.Fatalf("cross-owner challenge: %v", e)
	}
	beginKey := uuid.NewString()
	c, e := s.Begin(ctx, owner, home, "elhub_unit", beginKey)
	if e != nil {
		t.Fatal(e)
	}
	same, e := s.Begin(ctx, owner, home, "elhub_unit", beginKey)
	if e != nil || same.ClaimID != c.ClaimID {
		t.Fatalf("begin replay: %v %+v", e, same)
	}
	proof := b64(bytes.Repeat([]byte{4}, 32))
	sig := b64(ed25519.Sign(priv, claimTranscript(c.ClaimID, c.LocalHubID, c.Nonce, proof)))
	if _, e = s.Finish(ctx, other, c.ClaimID, c.LocalHubID, proof, sig, uuid.NewString()); !errorsIs(e, ErrDenied) {
		t.Fatalf("cross-owner claim: %v", e)
	}
	if _, e = s.Finish(ctx, owner, c.ClaimID, c.LocalHubID, b64(bytes.Repeat([]byte{5}, 32)), sig, uuid.NewString()); !errorsIs(e, ErrDenied) {
		t.Fatalf("wrong proof: %v", e)
	}
	finishKey := uuid.NewString()
	got, e := s.Finish(ctx, owner, c.ClaimID, c.LocalHubID, proof, sig, finishKey)
	if e != nil {
		t.Fatal(e)
	}
	if got.HubID != id || got.HomeID != home || got.Epoch != 2 {
		t.Fatalf("bad ownership: %+v", got)
	}
	ownerHubs, e := s.ListOwner(ctx, owner, home)
	if e != nil || len(ownerHubs) != 1 || ownerHubs[0] != got {
		t.Fatalf("owner hub readback: %+v %v", ownerHubs, e)
	}
	if _, e = s.ListOwner(ctx, other, home); !errorsIs(e, ErrDenied) {
		t.Fatalf("foreign hub readback: %v", e)
	}
	ownerRoute := func(user string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		request := httptest.NewRequest(http.MethodGet, "/v1/homes/"+home+"/hubs", nil)
		router := chi.NewRouter()
		router.Route("/v1", (Service{Store: s, Auth: func(next http.Handler) http.Handler { return next }, UserID: func(*http.Request) string { return user }}).AccountRoutes)
		router.ServeHTTP(rec, request)
		return rec
	}
	if rec := ownerRoute(owner); rec.Code != 200 || !strings.Contains(rec.Body.String(), `"local_hub_id":"elhub_unit"`) {
		t.Fatalf("owner route readback: %d %s", rec.Code, rec.Body.String())
	}
	if rec := ownerRoute(other); rec.Code != 404 {
		t.Fatalf("foreign route readback: %d", rec.Code)
	}
	for _, cert := range []struct {
		serial string
		epoch  int64
	}{{"a11", 2}, {"a12", 1}} {
		if _, e = pool.Exec(ctx, `INSERT INTO hub_certificates(serial,hub_id,ownership_epoch,expires_at,certificate_pem) VALUES($1,$2,$3,now()+interval '1 hour','TEST')`, cert.serial, id, cert.epoch); e != nil {
			t.Fatal(e)
		}
	}
	pin := strings.Repeat("a", 64)
	origin := "https://192.168.68.136:18787"
	if e = s.PublishConnection(ctx, id, "a12", 1, origin, pin); !errorsIs(e, ErrDenied) {
		t.Fatalf("prior epoch published connection: %v", e)
	}
	if e = s.PublishConnection(ctx, id, "other", 2, origin, pin); !errorsIs(e, ErrDenied) {
		t.Fatalf("unknown certificate published connection: %v", e)
	}
	if e = s.PublishConnection(ctx, id, "a11", 2, origin, pin); e != nil {
		t.Fatal(e)
	}
	ownerHubs, e = s.ListOwner(ctx, owner, home)
	if e != nil || len(ownerHubs) != 1 || ownerHubs[0].Connection == nil || ownerHubs[0].Connection.HTTPSOrigin != origin || ownerHubs[0].Connection.CertificateSHA256 != pin || !ownerHubs[0].Connection.ExpiresAt.After(time.Now()) {
		t.Fatalf("published connection unavailable: %+v %v", ownerHubs, e)
	}
	if rec := ownerRoute(owner); rec.Code != 200 || rec.Header().Get("Cache-Control") != "no-store" || !strings.Contains(rec.Body.String(), `"connection":`) {
		t.Fatalf("owner connection route: %d %s", rec.Code, rec.Body.String())
	}
	if rec := ownerRoute(other); rec.Code != 404 || strings.Contains(rec.Body.String(), origin) {
		t.Fatalf("foreign owner learned connection: %d", rec.Code)
	}
	if _, e = pool.Exec(ctx, `UPDATE hub_connections SET published_at=now()-interval '20 minutes',expires_at=now()-interval '1 second' WHERE hub_id=$1`, id); e != nil {
		t.Fatal(e)
	}
	ownerHubs, e = s.ListOwner(ctx, owner, home)
	if e != nil || ownerHubs[0].Connection != nil {
		t.Fatalf("expired connection exposed: %+v %v", ownerHubs, e)
	}
	if e = s.PublishConnection(ctx, id, "a11", 2, origin, pin); e != nil {
		t.Fatal(e)
	}
	if _, e = pool.Exec(ctx, `UPDATE hub_certificates SET revoked_at=now() WHERE serial='a11'`); e != nil {
		t.Fatal(e)
	}
	ownerHubs, e = s.ListOwner(ctx, owner, home)
	if e != nil || ownerHubs[0].Connection != nil {
		t.Fatalf("revoked publisher exposed: %+v %v", ownerHubs, e)
	}
	if _, e = pool.Exec(ctx, `UPDATE hub_certificates SET revoked_at=NULL WHERE serial='a11'`); e != nil {
		t.Fatal(e)
	}
	connectionRequest := func(hub string, serial int64, epoch int64, body string, withTLS bool) *httptest.ResponseRecorder {
		request := httptest.NewRequest(http.MethodPut, "/v1/hubs/"+hub+"/connection", strings.NewReader(body))
		request.Header.Set("Content-Type", "application/json")
		if withTLS {
			u, _ := url.Parse("spiffe://energylens.local/hubs/" + id + "/epochs/" + strconv.FormatInt(epoch, 10))
			cert := &x509.Certificate{SerialNumber: big.NewInt(serial), URIs: []*url.URL{u}}
			request.TLS = &tls.ConnectionState{PeerCertificates: []*x509.Certificate{cert}, VerifiedChains: [][]*x509.Certificate{{cert}}}
		}
		rec := httptest.NewRecorder()
		router := chi.NewRouter()
		router.Route("/v1", (Service{Store: s}).HubRoutes)
		router.ServeHTTP(rec, request)
		return rec
	}
	validBody := `{"https_origin":"https://192.168.68.136:18787","certificate_sha256":"` + pin + `"}`
	if rec := connectionRequest(id, 0xa11, 2, validBody, false); rec.Code != 403 {
		t.Fatalf("no TLS published: %d", rec.Code)
	}
	if rec := connectionRequest(id, 0xa12, 1, validBody, true); rec.Code != 403 {
		t.Fatalf("old cert published: %d", rec.Code)
	}
	if rec := connectionRequest(uuid.NewString(), 0xa11, 2, validBody, true); rec.Code != 403 {
		t.Fatalf("wrong Hub published: %d", rec.Code)
	}
	if rec := connectionRequest(id, 0xa11, 2, `{"https_origin":"http://192.168.68.136:18787","certificate_sha256":"`+pin+`"}`, true); rec.Code != 400 {
		t.Fatalf("invalid origin published: %d", rec.Code)
	}
	if rec := connectionRequest(id, 0xa11, 2, validBody, true); rec.Code != 204 || rec.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("current cert publish failed: %d", rec.Code)
	}
	ownerAuthorization := func(user, sid string, serial int64, certEpoch int64, requestedHome string) *httptest.ResponseRecorder {
		u, _ := url.Parse("spiffe://energylens.local/hubs/" + id + "/epochs/" + strconv.FormatInt(certEpoch, 10))
		cert := &x509.Certificate{SerialNumber: big.NewInt(serial), URIs: []*url.URL{u}}
		req := httptest.NewRequest(http.MethodPost, "/v1/hubs/"+id+"/owner-authorizations", strings.NewReader(`{"home_id":"`+requestedHome+`"}`))
		req.Header.Set("Content-Type", "application/json")
		req.TLS = &tls.ConnectionState{PeerCertificates: []*x509.Certificate{cert}, VerifiedChains: [][]*x509.Certificate{{cert}}}
		rec := httptest.NewRecorder()
		router := chi.NewRouter()
		router.Route("/v1", (Service{Store: s, Auth: func(next http.Handler) http.Handler { return next }, UserID: func(*http.Request) string { return user }, SessionID: func(*http.Request) string { return sid }}).HubRoutes)
		router.ServeHTTP(rec, req)
		return rec
	}
	sid := uuid.NewString()
	if rec := ownerAuthorization(owner, sid, 0xa11, 2, home); rec.Code != 200 || !strings.Contains(rec.Body.String(), `"session_id":"`+sid+`"`) || rec.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("owner authorization: %d %s", rec.Code, rec.Body.String())
	}
	if rec := ownerAuthorization(other, sid, 0xa11, 2, home); rec.Code != 404 {
		t.Fatalf("cross-account owner authorization: %d", rec.Code)
	}
	if rec := ownerAuthorization(owner, sid, 0xa12, 1, home); rec.Code != 404 {
		t.Fatalf("stale certificate owner authorization: %d", rec.Code)
	}
	if rec := ownerAuthorization(owner, "", 0xa11, 2, home); rec.Code != 401 {
		t.Fatalf("sessionless profile owner authorization: %d", rec.Code)
	}
	ownershipRequest := func(serial int64, certEpoch int64) *httptest.ResponseRecorder {
		u, _ := url.Parse("spiffe://energylens.local/hubs/" + id + "/epochs/" + strconv.FormatInt(certEpoch, 10))
		cert := &x509.Certificate{SerialNumber: big.NewInt(serial), URIs: []*url.URL{u}}
		req := httptest.NewRequest(http.MethodGet, "/v1/hubs/"+id+"/ownership", nil)
		req.TLS = &tls.ConnectionState{PeerCertificates: []*x509.Certificate{cert}, VerifiedChains: [][]*x509.Certificate{{cert}}}
		rec := httptest.NewRecorder()
		router := chi.NewRouter()
		router.Route("/v1", (Service{Store: s}).HubRoutes)
		router.ServeHTTP(rec, req)
		return rec
	}
	if rec := ownershipRequest(0xa11, 2); rec.Code != 200 || !strings.Contains(rec.Body.String(), home) {
		t.Fatalf("current ownership lookup: %d %s", rec.Code, rec.Body.String())
	}
	peerURI, _ := url.Parse("spiffe://energylens.local/hubs/" + id + "/epochs/2")
	peer := &x509.Certificate{SerialNumber: big.NewInt(0xa11), URIs: []*url.URL{peerURI}}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	request := httptest.NewRequest(http.MethodGet, "/v1/hubs/"+id+"/ownership", nil).WithContext(canceled)
	request.TLS = &tls.ConnectionState{PeerCertificates: []*x509.Certificate{peer}, VerifiedChains: [][]*x509.Certificate{{peer}}}
	response := httptest.NewRecorder()
	router := chi.NewRouter()
	router.Route("/v1", (Service{Store: s}).HubRoutes)
	router.ServeHTTP(response, request)
	if response.Code != 500 || !strings.Contains(response.Body.String(), `"code":"internal_error"`) {
		t.Fatalf("database failure masqueraded as revocation: %d %s", response.Code, response.Body.String())
	}
	if rec := ownershipRequest(0xa12, 1); rec.Code != 403 || !strings.Contains(rec.Body.String(), `"code":"ownership_revoked"`) {
		t.Fatalf("prior epoch learned owner: %d", rec.Code)
	}
	if _, e = pool.Exec(ctx, `UPDATE homes SET deleted_at=now() WHERE id=$1`, home); e != nil {
		t.Fatal(e)
	}
	if rec := ownershipRequest(0xa11, 2); rec.Code != 404 || !strings.Contains(rec.Body.String(), `"code":"ownership_unbound"`) {
		t.Fatalf("tombstoned home disclosed ownership: %d %s", rec.Code, rec.Body.String())
	}
	if _, e = pool.Exec(ctx, `UPDATE homes SET deleted_at=NULL WHERE id=$1`, home); e != nil {
		t.Fatal(e)
	}
	if _, e = pool.Exec(ctx, `UPDATE users SET deleted_at=now() WHERE id=$1`, owner); e != nil {
		t.Fatal(e)
	}
	if rec := ownershipRequest(0xa11, 2); rec.Code != 404 || !strings.Contains(rec.Body.String(), `"code":"ownership_unbound"`) {
		t.Fatalf("tombstoned account disclosed ownership: %d %s", rec.Code, rec.Body.String())
	}
	if _, e = pool.Exec(ctx, `UPDATE users SET deleted_at=NULL WHERE id=$1`, owner); e != nil {
		t.Fatal(e)
	}
	replayed, e := s.Finish(ctx, owner, c.ClaimID, c.LocalHubID, proof, sig, finishKey)
	if e != nil || replayed.Epoch != got.Epoch {
		t.Fatalf("finish replay: %v %+v", e, replayed)
	}
	if _, e = s.Finish(ctx, owner, c.ClaimID, c.LocalHubID, proof, sig, uuid.NewString()); !errorsIs(e, ErrExpired) {
		t.Fatalf("replay: %v", e)
	}
	if e = s.Unclaim(ctx, other, home, id, got.Version, uuid.NewString()); !errorsIs(e, ErrNotFound) {
		t.Fatalf("cross-owner unclaim: %v", e)
	}
	unclaimKey := uuid.NewString()
	if e = s.Unclaim(ctx, owner, home, id, got.Version, unclaimKey); e != nil {
		t.Fatal(e)
	}
	if e = s.PublishConnection(ctx, id, "a11", 2, origin, pin); !errorsIs(e, ErrDenied) {
		t.Fatalf("unclaimed Hub published connection: %v", e)
	}
	ownerHubs, e = s.ListOwner(ctx, owner, home)
	if e != nil || len(ownerHubs) != 0 {
		t.Fatalf("unclaimed hub remained in owner list: %+v %v", ownerHubs, e)
	}
	if rec := ownershipRequest(0xa11, 2); rec.Code != 403 || !strings.Contains(rec.Body.String(), `"code":"ownership_revoked"`) {
		t.Fatalf("unclaimed hub disclosed former home: %d", rec.Code)
	}
	if e = s.Unclaim(ctx, owner, home, id, got.Version, unclaimKey); e != nil {
		t.Fatalf("unclaim replay: %v", e)
	}
	var epoch int64
	var currentHome *string
	e = pool.QueryRow(ctx, `SELECT ownership_epoch,home_id FROM hubs WHERE id=$1`, id).Scan(&epoch, &currentHome)
	if e != nil || epoch != 3 || currentHome != nil {
		t.Fatalf("unclaim epoch=%d home=%v err=%v", epoch, currentHome, e)
	}
	if _, e = pool.Exec(ctx, `INSERT INTO hub_certificates(serial,hub_id,ownership_epoch,expires_at,certificate_pem) VALUES('a13',$1,3,now()+interval '1 hour','TEST')`, id); e != nil {
		t.Fatal(e)
	}
	if rec := ownershipRequest(0xa13, 3); rec.Code != 404 || !strings.Contains(rec.Body.String(), `"code":"ownership_unbound"`) {
		t.Fatalf("unclaimed current cert disclosed a home: %d", rec.Code)
	}
	// Provisioning nonce is exactly one-use, including a signed timestamp and CSR hash.
	der := []byte("csr-der")
	hash := sha256.Sum256(der)
	signedAt := time.Now().UTC().Format(time.RFC3339)
	enrollSig := ed25519.Sign(priv, enrollTranscript(id, "elhub_unit", nonce, signedAt, hex.EncodeToString(hash[:])))
	if _, e = s.Enrollment(ctx, id, "elhub_unit", nonce, signedAt, der, enrollSig); e != nil {
		t.Fatal(e)
	}
	if e = s.SaveEnrollmentCertificate(ctx, nonce, "testserial", id, 3, time.Now().Add(time.Hour), "PEM", hash[:]); e != nil {
		t.Fatal(e)
	}
	if _, e = s.Enrollment(ctx, id, "elhub_unit", nonce, signedAt, der, enrollSig); e != nil {
		t.Fatalf("signed enrollment retry: %v", e)
	}
	replayCert, found, e := s.EnrollmentReplay(ctx, id, nonce, hash[:])
	if e != nil || !found || replayCert.PEM != "PEM" || replayCert.Epoch != 3 {
		t.Fatalf("enrollment replay: %+v %v %v", replayCert, found, e)
	}
	otherHash := sha256.Sum256([]byte("other"))
	if _, found, e = s.EnrollmentReplay(ctx, id, nonce, otherHash[:]); !found || !errorsIs(e, ErrKeyReuse) {
		t.Fatalf("changed CSR replay: found=%v err=%v", found, e)
	}
	if e = s.SaveEnrollmentCertificate(ctx, nonce, "secondserial", id, 3, time.Now().Add(time.Hour), "PEM2", hash[:]); !errorsIs(e, ErrDenied) {
		t.Fatalf("nonce reused: %v", e)
	}
	if e = s.SaveRenewedCertificate(ctx, "testserial", "renewedserial", id, 3, time.Now().Add(time.Hour), "RENEWED", hash[:]); e != nil {
		t.Fatal(e)
	}
	renewal, found, e := s.RenewalReplay(ctx, id, "testserial", 3, 3, hash[:])
	if e != nil || !found || renewal.PEM != "RENEWED" {
		t.Fatalf("renewal replay: %+v %v %v", renewal, found, e)
	}
	if _, found, e = s.RenewalReplay(ctx, id, "testserial", 3, 3, otherHash[:]); !found || !errorsIs(e, ErrKeyReuse) {
		t.Fatalf("changed renewal CSR: found=%v err=%v", found, e)
	}
	// Transfer of ownership invalidates even a matching renewal replay.
	newOwnerHome := home
	if _, e = pool.Exec(ctx, `UPDATE hubs SET ownership_epoch=ownership_epoch+1,home_id=$2 WHERE id=$1`, id, newOwnerHome); e != nil {
		t.Fatal(e)
	}
	ownerHubs, e = s.ListOwner(ctx, owner, home)
	if e != nil || len(ownerHubs) != 1 || ownerHubs[0].Connection != nil {
		t.Fatalf("prior epoch connection exposed after transfer: %+v %v", ownerHubs, e)
	}
	if _, found, e = s.RenewalReplay(ctx, id, "testserial", 3, 4, hash[:]); !found || !errorsIs(e, ErrDenied) {
		t.Fatalf("stale epoch replay: found=%v err=%v", found, e)
	}
}
func errorsIs(err, target error) bool { return err == target }

func TestRenewRequiresVerifiedTLS(t *testing.T) {
	svc := Service{}
	r := chi.NewRouter()
	r.Route("/v1", svc.HubRoutes)
	hubID := uuid.NewString()
	req := httptest.NewRequest(http.MethodPost, "/v1/hubs/"+hubID+"/certificate", strings.NewReader(`{"csr_pem":"x","ownership_epoch":1}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Amzn-Mtls-Clientcert", "forged")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("spoofed header accepted: %d", rec.Code)
	}
	statusReq := httptest.NewRequest(http.MethodGet, "/v1/hubs/"+hubID+"/ownership", nil)
	statusReq.Header.Set("X-Amzn-Mtls-Clientcert", "forged")
	statusRec := httptest.NewRecorder()
	r.ServeHTTP(statusRec, statusReq)
	if statusRec.Code != http.StatusForbidden {
		t.Fatalf("spoofed ownership header accepted: %d", statusRec.Code)
	}
}
func TestPeerIdentitySAN(t *testing.T) {
	id := uuid.NewString()
	good, _ := url.Parse("spiffe://energylens.local/hubs/" + id + "/epochs/4")
	hub, epoch, ok := peerIdentity(&x509.Certificate{URIs: []*url.URL{good}})
	if !ok || hub != id || epoch != 4 {
		t.Fatal("valid SAN rejected")
	}
	bad, _ := url.Parse("spiffe://attacker.example/hubs/" + id + "/epochs/4")
	if _, _, ok = peerIdentity(&x509.Certificate{URIs: []*url.URL{bad}}); ok {
		t.Fatal("untrusted SAN accepted")
	}
}

// Canonical vectors: energylens-common-contracts/api/testdata/hub-trust-transcripts.json.
func TestCommonContractTranscriptVectors(t *testing.T) {
	claim := claimTranscript("38d634c0-54c6-4c60-9523-0923dce8c040", "elhub_contractvector", "AgICAgICAgICAgICAgICAgICAgICAgICAgICAgICAgI", "AQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQE")
	sum := sha256.Sum256(claim)
	if got := hex.EncodeToString(sum[:]); got != "0775c356287cee9de72ef6537329dd4b988614296604e0cb0ad9a195e7fe656e" {
		t.Fatalf("claim transcript hash %s", got)
	}
	der := bytes.Repeat([]byte{3}, 40)
	csrHash := sha256.Sum256(der)
	enroll := enrollTranscript("da17c425-d948-4d6b-8cc0-acf84da38c39", "elhub_contractvector", "AgICAgICAgICAgICAgICAgICAgICAgICAgICAgICAgI", "2026-09-30T12:00:00Z", hex.EncodeToString(csrHash[:]))
	sum = sha256.Sum256(enroll)
	if got := hex.EncodeToString(sum[:]); got != "1443bf29fef87383c37f588db8beea95d47191accea91153eda9926653600508" {
		t.Fatalf("enrollment transcript hash %s", got)
	}
}

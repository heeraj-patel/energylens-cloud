package hubtrust

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"energylens.local/cloud/internal/account"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

type recoveryEnv struct {
	t    *testing.T
	pool *pgxpool.Pool
	svc  Service
	f    pairingFixture
}

func newRecoveryEnv(t *testing.T, pool *pgxpool.Pool, code string) recoveryEnv {
	t.Helper()
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "test ca"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	f := newPairingFixture(t, pool, code)
	return recoveryEnv{t: t, pool: pool, f: f, svc: Service{Store: f.d.Store, Pairing: &f.d, Signer: tls.Certificate{Certificate: [][]byte{der}, PrivateKey: caKey}}}
}

func (e recoveryEnv) post(path string, body any) *httptest.ResponseRecorder {
	raw, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPost, "/v1/hubs/"+e.f.hub+path, bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	router := chi.NewRouter()
	router.Route("/v1", e.svc.EnrollmentRoutes)
	router.ServeHTTP(rec, req)
	return rec
}

func (e recoveryEnv) challengeAs(local string, key ed25519.PrivateKey, signedAt string) *httptest.ResponseRecorder {
	sig := b64(ed25519.Sign(key, enrollChallengeTranscript(e.f.hub, local, signedAt)))
	return e.post("/enroll-challenges", map[string]string{"local_hub_id": local, "signed_at": signedAt, "device_signature": sig})
}

func (e recoveryEnv) challenge() *httptest.ResponseRecorder {
	return e.challengeAs(e.f.localID(), e.f.privateKey, time.Now().UTC().Format(time.RFC3339))
}

func (e recoveryEnv) nonce() (string, int64) {
	e.t.Helper()
	rec := e.challenge()
	if rec.Code != 200 || rec.Header().Get("Cache-Control") != "no-store" {
		e.t.Fatalf("challenge: %d %s", rec.Code, rec.Body.String())
	}
	var out struct {
		Nonce     string    `json:"enrollment_nonce"`
		Epoch     int64     `json:"ownership_epoch"`
		ExpiresAt time.Time `json:"expires_at"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil || len(out.Nonce) != 43 || out.ExpiresAt.Before(time.Now().Add(14*time.Minute)) {
		e.t.Fatalf("challenge body: %v %s", err, rec.Body.String())
	}
	return out.Nonce, out.Epoch
}

func (e recoveryEnv) enroll(nonce string) *httptest.ResponseRecorder {
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	csrDER, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{Subject: pkix.Name{CommonName: "hub"}}, key)
	if err != nil {
		e.t.Fatal(err)
	}
	signedAt := time.Now().UTC().Format(time.RFC3339)
	hash := sha256.Sum256(csrDER)
	sig := b64(ed25519.Sign(e.f.privateKey, enrollTranscript(e.f.hub, e.f.localID(), nonce, signedAt, hex.EncodeToString(hash[:]))))
	return e.post("/enroll", map[string]string{"local_hub_id": e.f.localID(), "csr_pem": string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: csrDER})), "nonce": nonce, "signed_at": signedAt, "device_signature": sig})
}

func (f pairingFixture) localID() string {
	var local string
	if err := f.d.Store.Pool.QueryRow(context.Background(), `SELECT local_hub_id FROM hubs WHERE id=$1`, f.hub).Scan(&local); err != nil {
		panic(err)
	}
	return local
}

func (e recoveryEnv) newUser(name string) (string, string) {
	e.t.Helper()
	var user, home string
	if err := e.pool.QueryRow(context.Background(), `INSERT INTO users DEFAULT VALUES RETURNING id`).Scan(&user); err != nil {
		e.t.Fatal(err)
	}
	if err := e.pool.QueryRow(context.Background(), `INSERT INTO homes(owner_user_id,name,timezone) VALUES($1,$2,'UTC') RETURNING id`, user, name).Scan(&home); err != nil {
		e.t.Fatal(err)
	}
	return user, home
}

func (e recoveryEnv) claim(owner, home string) Ownership {
	e.t.Helper()
	ctx := context.Background()
	c, err := e.f.d.Store.Begin(ctx, owner, home, e.f.localID(), uuid.NewString())
	if err != nil {
		e.t.Fatal(err)
	}
	proof := b64(mustRandom())
	sig := b64(ed25519.Sign(e.f.privateKey, claimTranscript(c.ClaimID, c.LocalHubID, c.Nonce, proof)))
	o, err := e.f.d.Store.Finish(ctx, owner, c.ClaimID, c.LocalHubID, proof, sig, uuid.NewString())
	if err != nil {
		e.t.Fatal(err)
	}
	return o
}

func (e recoveryEnv) unclaim(owner, home string) {
	e.t.Helper()
	var version int64
	if err := e.pool.QueryRow(context.Background(), `SELECT version FROM hubs WHERE id=$1`, e.f.hub).Scan(&version); err != nil {
		e.t.Fatal(err)
	}
	if err := e.f.d.Store.Unclaim(context.Background(), owner, home, e.f.hub, version, uuid.NewString()); err != nil {
		e.t.Fatal(err)
	}
}

func certSerial(t *testing.T, body []byte) string {
	t.Helper()
	var out struct {
		PEM   string `json:"certificate_pem"`
		Epoch int64  `json:"ownership_epoch"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatal(err)
	}
	block, _ := pem.Decode([]byte(out.PEM))
	c, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	return c.SerialNumber.Text(16)
}

func TestUnclaimedRecovery(t *testing.T) {
	pool := pairingTestPool(t)
	ctx := context.Background()
	t.Run("removed hub re-enrolls and is claimable by another account", func(t *testing.T) {
		e := newRecoveryEnv(t, pool, "RCV123")
		a, homeA := e.newUser("A")
		b, homeB := e.newUser("B")
		claimed := e.claim(a, homeA)
		e.unclaim(a, homeA)
		nonce, epoch := e.nonce()
		if epoch != claimed.Epoch+1 {
			t.Fatalf("epoch %d after claim at %d", epoch, claimed.Epoch)
		}
		rec := e.enroll(nonce)
		if rec.Code != 200 {
			t.Fatalf("enroll: %d %s", rec.Code, rec.Body.String())
		}
		serial := certSerial(t, rec.Body.Bytes())
		if _, err := e.f.d.Publish(ctx, e.f.hub, serial, epoch, e.f.in); err != nil {
			t.Fatal(err)
		}
		res, err := e.f.d.Resolve(ctx, b, homeB, uuid.NewString(), uuid.NewString(), uuid.NewString(), e.f.in.Code)
		if err != nil || res.HubID != e.f.hub {
			t.Fatalf("resolve: %v", err)
		}
		if o := e.claim(b, homeB); o.Epoch != epoch+1 || o.HomeID != homeB {
			t.Fatalf("claim by B: %+v", o)
		}
		if _, err = e.f.d.Store.AuthorizeOwner(ctx, e.f.hub, serial, epoch, a, homeA); err == nil {
			t.Fatal("A authorized")
		}
		if hubs, err := e.f.d.Store.ListOwner(ctx, a, homeA); err != nil || len(hubs) != 0 {
			t.Fatalf("A owner list: %+v %v", hubs, err)
		}
	})
	t.Run("challenge rejections", func(t *testing.T) {
		e := newRecoveryEnv(t, pool, "RCV234")
		now := time.Now().UTC()
		_, other, _ := ed25519.GenerateKey(rand.Reader)
		for name, rec := range map[string]*httptest.ResponseRecorder{
			"bad signature": e.challengeAs(e.f.localID(), other, now.Format(time.RFC3339)),
			"wrong local":   e.challengeAs("elhub_other", e.f.privateKey, now.Format(time.RFC3339)),
			"stale":         e.challengeAs(e.f.localID(), e.f.privateKey, now.Add(-6*time.Minute).Format(time.RFC3339)),
			"future":        e.challengeAs(e.f.localID(), e.f.privateKey, now.Add(2*time.Minute).Format(time.RFC3339)),
		} {
			if rec.Code != 403 || !strings.Contains(rec.Body.String(), `"code":"forbidden"`) {
				t.Fatalf("%s: %d %s", name, rec.Code, rec.Body.String())
			}
		}
		for i := 0; i < 3; i++ {
			e.nonce()
		}
		if rec := e.challenge(); rec.Code != 429 || !strings.Contains(rec.Body.String(), `"code":"rate_limited"`) {
			t.Fatalf("fourth challenge: %d", rec.Code)
		}
		if _, err := pool.Exec(ctx, `UPDATE hub_enrollment_nonces SET expires_at=now()-interval '1 second' WHERE hub_id=$1`, e.f.hub); err != nil {
			t.Fatal(err)
		}
		e.nonce()
		var rows int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM hub_enrollment_nonces WHERE hub_id=$1 AND purpose='unclaimed_recovery'`, e.f.hub).Scan(&rows); err != nil || rows != 1 {
			t.Fatalf("expired recovery rows kept: %d %v", rows, err)
		}
		owner, home := e.newUser("C")
		e.claim(owner, home)
		if rec := e.challenge(); rec.Code != 409 || !strings.Contains(rec.Body.String(), `"code":"hub_claimed"`) {
			t.Fatalf("claimed: %d %s", rec.Code, rec.Body.String())
		}
		if rec := e.challengeAs(e.f.localID(), other, now.Format(time.RFC3339)); rec.Code != 403 {
			t.Fatalf("claimed hub with bad signature disclosed state: %d", rec.Code)
		}
		if _, err := pool.Exec(ctx, `UPDATE hubs SET revoked_at=now() WHERE id=$1`, e.f.hub); err != nil {
			t.Fatal(err)
		}
		if rec := e.challenge(); rec.Code != 403 {
			t.Fatalf("revoked: %d", rec.Code)
		}
		if rec := e.post("/enroll-challenges", map[string]string{"local_hub_id": "x"}); rec.Code != 403 {
			t.Fatalf("empty signature: %d", rec.Code)
		}
	})
	t.Run("claim after challenge denies enroll", func(t *testing.T) {
		e := newRecoveryEnv(t, pool, "RCV345")
		nonce, _ := e.nonce()
		owner, home := e.newUser("D")
		e.claim(owner, home)
		if rec := e.enroll(nonce); rec.Code != 403 {
			t.Fatalf("enroll after claim: %d %s", rec.Code, rec.Body.String())
		}
	})
	t.Run("nonce from an earlier unclaimed epoch is denied", func(t *testing.T) {
		e := newRecoveryEnv(t, pool, "RCV456")
		owner, home := e.newUser("E")
		e.claim(owner, home)
		e.unclaim(owner, home)
		nonce, _ := e.nonce()
		e.claim(owner, home)
		e.unclaim(owner, home)
		if rec := e.enroll(nonce); rec.Code != 403 {
			t.Fatalf("stale epoch enroll: %d %s", rec.Code, rec.Body.String())
		}
		fresh, epoch := e.nonce()
		if rec := e.enroll(fresh); rec.Code != 200 || epoch == 0 {
			t.Fatalf("current epoch enroll: %d", rec.Code)
		}
	})
	t.Run("recovery after home deletion", func(t *testing.T) {
		e := newRecoveryEnv(t, pool, "RCV567")
		owner, home := e.newUser("F")
		e.claim(owner, home)
		if err := (account.PGStore{Pool: pool}).DeleteHome(ctx, owner, home); err != nil {
			t.Fatal(err)
		}
		nonce, _ := e.nonce()
		if rec := e.enroll(nonce); rec.Code != 200 {
			t.Fatalf("enroll after home deletion: %d %s", rec.Code, rec.Body.String())
		}
	})
	t.Run("operator nonce ignores home and epoch recovery rules", func(t *testing.T) {
		e := newRecoveryEnv(t, pool, "RCV678")
		nonce, err := e.f.d.Store.IssueEnrollmentNonce(ctx, e.f.hub)
		if err != nil {
			t.Fatal(err)
		}
		var purpose string
		var epoch *int64
		h := sha256.Sum256(mustDecode(t, nonce))
		if err = pool.QueryRow(ctx, `SELECT purpose,ownership_epoch FROM hub_enrollment_nonces WHERE nonce_hash=$1`, h[:]).Scan(&purpose, &epoch); err != nil || purpose != "operator" || epoch != nil {
			t.Fatalf("operator nonce row: %s %v %v", purpose, epoch, err)
		}
		if rec := e.enroll(nonce); rec.Code != 200 {
			t.Fatalf("operator enroll: %d", rec.Code)
		}
	})
}

func mustDecode(t *testing.T, s string) []byte {
	t.Helper()
	b, err := decode64(s, 32)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func mustRandom() []byte {
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	return b
}

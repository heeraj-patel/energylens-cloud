package hubtrust

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"energylens.local/cloud/internal/migrate"
	"energylens.local/cloud/migrations"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestPairingCodeCryptography(t *testing.T) {
	d := PairingDirectory{Key: bytes.Repeat([]byte{7}, 32)}
	if !pairingCodeFormat.MatchString(normalizePairingCode(" ab34xy ")) {
		t.Fatal("normalization")
	}
	for _, bad := range []string{"ABC12", "ABC1234", "AB12OI", "AB12LU", "ABC\n12"} {
		if pairingCodeFormat.MatchString(normalizePairingCode(bad)) {
			t.Fatalf("accepted %q", bad)
		}
	}
	if bytes.Equal(d.hash("code", "ABC123"), d.hash("rate-owner", "ABC123")) {
		t.Fatal("HMAC domains overlap")
	}
	expiry := time.Now().UTC().Truncate(time.Microsecond)
	aad := pairingAAD("hub", "publication", expiry)
	secret := strings.Repeat("c", 64)
	sealed, err := d.seal(secret, aad)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(sealed, []byte(secret)) {
		t.Fatal("plaintext retained")
	}
	if plain, err := d.open(sealed, aad); err != nil || plain != secret {
		t.Fatal("roundtrip", err)
	}
	for _, wrong := range [][]byte{pairingAAD("other", "publication", expiry), pairingAAD("hub", "other", expiry), pairingAAD("hub", "publication", expiry.Add(time.Second))} {
		if _, err := d.open(sealed, wrong); err == nil {
			t.Fatal("ciphertext not bound")
		}
	}
	sealed[len(sealed)-1] ^= 1
	if _, err := d.open(sealed, aad); err == nil {
		t.Fatal("tampered ciphertext opened")
	}
}

func pairingTestPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("ENERGYLENS_CLOUD_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("ENERGYLENS_CLOUD_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	admin, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	schema := "pairing_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	if _, err = admin.Exec(ctx, "CREATE SCHEMA "+schema); err != nil {
		t.Fatal(err)
	}
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	cfg.ConnConfig.RuntimeParams["search_path"] = schema
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { pool.Close(); admin.Exec(ctx, "DROP SCHEMA "+schema+" CASCADE"); admin.Close() })
	conn, err := pgx.ConnectConfig(ctx, cfg.ConnConfig)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	if err = migrate.Apply(ctx, conn, migrations.FS); err != nil {
		t.Fatal(err)
	}
	return pool
}

type pairingFixture struct {
	d                                                    PairingDirectory
	hub, serial, owner, home, session, client, operation string
	in                                                   PairingPublication
	privateKey                                           ed25519.PrivateKey
}

func newPairingFixture(t *testing.T, pool *pgxpool.Pool, code string) pairingFixture {
	t.Helper()
	ctx := context.Background()
	f := pairingFixture{d: PairingDirectory{Store: Store{Pool: pool}, Key: bytes.Repeat([]byte{8}, 32)}, session: uuid.NewString(), client: uuid.NewString(), operation: uuid.NewString()}
	pub, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	f.privateKey = privateKey
	f.hub, _, err = f.d.Store.Provision(ctx, "elhub_"+strings.ReplaceAll(uuid.NewString(), "-", ""), pub, "test")
	if err != nil {
		t.Fatal(err)
	}
	f.serial = strings.ReplaceAll(uuid.NewString(), "-", "")
	if _, err = pool.Exec(ctx, `INSERT INTO hub_certificates(serial,hub_id,ownership_epoch,expires_at,certificate_pem) VALUES($1,$2,1,now()+interval '1 hour','TEST')`, f.serial, f.hub); err != nil {
		t.Fatal(err)
	}
	if err = pool.QueryRow(ctx, `INSERT INTO users DEFAULT VALUES RETURNING id`).Scan(&f.owner); err != nil {
		t.Fatal(err)
	}
	if err = pool.QueryRow(ctx, `INSERT INTO homes(owner_user_id,name,timezone) VALUES($1,'Test','UTC') RETURNING id`, f.owner).Scan(&f.home); err != nil {
		t.Fatal(err)
	}
	f.in = PairingPublication{PublicationID: uuid.NewString(), Code: code, EnrollmentSecret: strings.Repeat("a", 64), HTTPSOrigin: "https://192.168.1.20:18788", CertificateSHA256: strings.Repeat("b", 64), ExpiresAt: time.Now().UTC().Add(14 * time.Minute).Truncate(time.Second)}
	return f
}
func (f pairingFixture) publish(t *testing.T) time.Time {
	t.Helper()
	expiry, err := f.d.Publish(context.Background(), f.hub, f.serial, 1, f.in)
	if err != nil {
		t.Fatal(err)
	}
	return expiry
}
func (f pairingFixture) resolve() (PairingResolution, error) {
	return f.d.Resolve(context.Background(), f.owner, f.home, f.session, f.client, f.operation, f.in.Code)
}

func TestPairingDirectory(t *testing.T) {
	pool := pairingTestPool(t)
	ctx := context.Background()
	t.Run("claim invalidates rendezvous", func(t *testing.T) {
		f := newPairingFixture(t, pool, "QRS345")
		f.publish(t)
		resolved, err := f.resolve()
		if err != nil {
			t.Fatal(err)
		}
		challenge, err := f.d.Store.Begin(ctx, f.owner, f.home, resolved.LocalHubID, uuid.NewString())
		if err != nil {
			t.Fatal(err)
		}
		proof := b64(bytes.Repeat([]byte{5}, 32))
		signature := b64(ed25519.Sign(f.privateKey, claimTranscript(challenge.ClaimID, challenge.LocalHubID, challenge.Nonce, proof)))
		if _, err = f.d.Store.Finish(ctx, f.owner, challenge.ClaimID, challenge.LocalHubID, proof, signature, uuid.NewString()); err != nil {
			t.Fatal(err)
		}
		var count int
		if err = pool.QueryRow(ctx, `SELECT count(*) FROM hub_pairing_codes WHERE hub_id=$1`, f.hub).Scan(&count); err != nil || count != 0 {
			t.Fatal("claim retained pairing capability", err)
		}
		if _, err = f.resolve(); !errors.Is(err, ErrNotFound) {
			t.Fatal("claimed code replay", err)
		}
	})
	t.Run("encrypted reserve and exact bounded replay", func(t *testing.T) {
		f := newPairingFixture(t, pool, "ABC123")
		expiry := f.publish(t)
		var sealed, codeHash, requestHash []byte
		if err := pool.QueryRow(ctx, `SELECT enrollment_ciphertext,code_hash,request_hash FROM hub_pairing_codes WHERE hub_id=$1`, f.hub).Scan(&sealed, &codeHash, &requestHash); err != nil {
			t.Fatal(err)
		}
		for _, blob := range [][]byte{sealed, codeHash, requestHash} {
			if bytes.Contains(blob, []byte(f.in.Code)) || bytes.Contains(blob, []byte(f.in.EnrollmentSecret)) {
				t.Fatal("stored raw capability")
			}
		}
		got, err := f.resolve()
		if err != nil || got.HubID != f.hub || got.EnrollmentSecret != f.in.EnrollmentSecret || !got.ExpiresAt.Equal(expiry) {
			t.Fatal("resolve", err)
		}
		replay, err := f.resolve()
		if err != nil || replay != got {
			t.Fatal("resolve replay", err)
		}
		for _, change := range []func(*pairingFixture){func(x *pairingFixture) { x.owner = uuid.NewString() }, func(x *pairingFixture) { x.home = uuid.NewString() }, func(x *pairingFixture) { x.session = uuid.NewString() }, func(x *pairingFixture) { x.client = uuid.NewString() }, func(x *pairingFixture) { x.operation = uuid.NewString() }} {
			other := f
			change(&other)
			if _, err := other.resolve(); !errors.Is(err, ErrNotFound) {
				t.Fatal("foreign reservation returned capability", err)
			}
		}
		if repeated := f.publish(t); !repeated.Equal(expiry) {
			t.Fatal("publication extended")
		}
		replacement := f.in
		replacement.PublicationID = uuid.NewString()
		replacement.Code = "BCD234"
		if _, err := f.d.Publish(ctx, f.hub, f.serial, 1, replacement); !errors.Is(err, ErrConflict) {
			t.Fatal("resolved code was replaced", err)
		}
		if _, err := pool.Exec(ctx, `UPDATE hub_pairing_codes SET expires_at=now()-interval '1 second',published_at=now()-interval '1 hour' WHERE hub_id=$1`, f.hub); err != nil {
			t.Fatal(err)
		}
		if _, err := f.resolve(); !errors.Is(err, ErrNotFound) {
			t.Fatal("expired replay", err)
		}
	})
	t.Run("old publication cannot replace newer code", func(t *testing.T) {
		f := newPairingFixture(t, pool, "CDE234")
		f.publish(t)
		old := f
		f.in.PublicationID = uuid.NewString()
		f.in.Code = "DEF345"
		f.publish(t)
		if _, err := old.d.Publish(ctx, old.hub, old.serial, 1, old.in); !errors.Is(err, ErrConflict) {
			t.Fatal("resurrected old code", err)
		}
		if _, err := old.resolve(); !errors.Is(err, ErrNotFound) {
			t.Fatal("old code resolved", err)
		}
		if _, err := f.resolve(); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("collision cannot replace another Hub", func(t *testing.T) {
		f := newPairingFixture(t, pool, "EFG456")
		f.publish(t)
		other := newPairingFixture(t, pool, f.in.Code)
		if _, err := other.d.Publish(ctx, other.hub, other.serial, 1, other.in); !errors.Is(err, ErrConflict) {
			t.Fatal("collision", err)
		}
		if got, err := f.resolve(); err != nil || got.HubID != f.hub {
			t.Fatal("original publication changed", err)
		}
	})
	t.Run("certificate expiry bounds lifetime", func(t *testing.T) {
		f := newPairingFixture(t, pool, "FGH567")
		certExpiry := time.Now().UTC().Add(time.Minute).Truncate(time.Microsecond)
		pool.Exec(ctx, `UPDATE hub_certificates SET expires_at=$2 WHERE serial=$1`, f.serial, certExpiry)
		if expiry := f.publish(t); !expiry.Equal(certExpiry) {
			t.Fatal("certificate lifetime exceeded")
		}
	})
	for _, kind := range []string{"revoked_certificate", "stale_epoch", "owned", "revoked_hub"} {
		t.Run(kind, func(t *testing.T) {
			codes := map[string]string{"revoked_certificate": "GHJ678", "stale_epoch": "HJK789", "owned": "JKM890", "revoked_hub": "KMN901"}
			f := newPairingFixture(t, pool, codes[kind])
			f.publish(t)
			switch kind {
			case "revoked_certificate":
				pool.Exec(ctx, `UPDATE hub_certificates SET revoked_at=now() WHERE serial=$1`, f.serial)
			case "stale_epoch":
				pool.Exec(ctx, `UPDATE hubs SET ownership_epoch=2 WHERE id=$1`, f.hub)
			case "owned":
				pool.Exec(ctx, `UPDATE hubs SET home_id=$2 WHERE id=$1`, f.hub, f.home)
			case "revoked_hub":
				pool.Exec(ctx, `UPDATE hubs SET revoked_at=now() WHERE id=$1`, f.hub)
			}
			if _, err := f.resolve(); !errors.Is(err, ErrNotFound) {
				t.Fatal("invalid trust resolved", err)
			}
			if _, err := f.d.Publish(ctx, f.hub, f.serial, 1, f.in); !errors.Is(err, ErrDenied) {
				t.Fatal("invalid trust published", err)
			}
		})
	}
	t.Run("only one concurrent resolver", func(t *testing.T) {
		f := newPairingFixture(t, pool, "MNP012")
		f.publish(t)
		other := newPairingFixture(t, pool, "NPQ123")
		other.in.Code = f.in.Code
		var wg sync.WaitGroup
		results := make(chan error, 2)
		for _, attempt := range []pairingFixture{f, other} {
			wg.Add(1)
			go func(x pairingFixture) { defer wg.Done(); _, err := x.resolve(); results <- err }(attempt)
		}
		wg.Wait()
		close(results)
		success, miss := 0, 0
		for err := range results {
			if err == nil {
				success++
			} else if errors.Is(err, ErrNotFound) {
				miss++
			} else {
				t.Fatal(err)
			}
		}
		if success != 1 || miss != 1 {
			t.Fatalf("success=%d miss=%d", success, miss)
		}
	})
}

func TestPairingDirectoryLiveCodeBudget(t *testing.T) {
	pool := pairingTestPool(t)
	ctx := context.Background()
	for i := 0; i < maxActivePairingCodes; i++ {
		f := newPairingFixture(t, pool, fmt.Sprintf("Z%05d", i))
		f.publish(t)
	}
	f := newPairingFixture(t, pool, "ZZZZZZ")
	if _, err := f.d.Publish(ctx, f.hub, f.serial, 1, f.in); !errors.Is(err, ErrPairingRateLimited) {
		t.Fatal("live-code budget exceeded", err)
	}
}

func TestPairingDirectoryHTTPAndBudgets(t *testing.T) {
	pool := pairingTestPool(t)
	ctx := context.Background()
	f := newPairingFixture(t, pool, "PQR234")
	service := Service{Store: f.d.Store, Pairing: &f.d, Auth: func(next http.Handler) http.Handler { return next }, UserID: func(*http.Request) string { return f.owner }, SessionID: func(*http.Request) string { return f.session }, PairingSource: func(*http.Request) (string, error) { return "192.0.2.1", nil }}
	router := chi.NewRouter()
	router.Route("/v1", func(r chi.Router) { service.AccountRoutes(r); service.HubRoutes(r) })
	body, _ := json.Marshal(f.in)
	publish := func(withTLS bool) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodPut, "/v1/hubs/"+f.hub+"/pairing-code", bytes.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
		if withTLS {
			u, _ := url.Parse("spiffe://energylens.local/hubs/" + f.hub + "/epochs/1")
			serial, _ := new(big.Int).SetString(f.serial, 16)
			cert := &x509.Certificate{SerialNumber: serial, URIs: []*url.URL{u}}
			r.TLS = &tls.ConnectionState{PeerCertificates: []*x509.Certificate{cert}, VerifiedChains: [][]*x509.Certificate{{cert}}}
		}
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, r)
		return rec
	}
	if rec := publish(false); rec.Code != 403 {
		t.Fatal("publication without mTLS", rec.Code)
	}
	if rec := publish(true); rec.Code != 200 || rec.Header().Get("Cache-Control") != "no-store" || strings.Contains(rec.Body.String(), f.in.EnrollmentSecret) {
		t.Fatal("publish", rec.Code)
	}
	resolve := func(body string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodPost, "/v1/homes/"+f.home+"/hub-pairings/resolve", strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("Idempotency-Key", f.operation)
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, r)
		return rec
	}
	if rec := resolve(`{"code":"pqr234","client_id":"` + f.client + `"}`); rec.Code != 200 || rec.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("resolve", rec.Code, rec.Body.String())
	}
	for i := 0; i < 4; i++ {
		if rec := resolve(`{broken`); rec.Code != 400 {
			t.Fatal("malformed", rec.Code)
		}
	}
	if rec := resolve(`{broken`); rec.Code != 429 {
		t.Fatal("malformed guesses not counted", rec.Code)
	}
	restarted := PairingDirectory{Store: f.d.Store, Key: append([]byte(nil), f.d.Key...)}
	if err := restarted.AllowResolve(ctx, f.owner, "192.0.2.99"); !errors.Is(err, ErrPairingRateLimited) {
		t.Fatal("restart reset owner budget", err)
	}
	pool.Exec(ctx, `DELETE FROM hub_pairing_rate_limits`)
	for i := 0; i < 20; i++ {
		if err := f.d.AllowResolve(ctx, uuid.NewString(), "192.0.2.1"); err != nil {
			t.Fatal(err)
		}
	}
	if err := f.d.AllowResolve(ctx, uuid.NewString(), "192.0.2.1"); !errors.Is(err, ErrPairingRateLimited) {
		t.Fatal("source budget", err)
	}
	pool.Exec(ctx, `DELETE FROM hub_pairing_rate_limits`)
	for i := 0; i < 100; i++ {
		if err := f.d.AllowResolve(ctx, uuid.NewString(), uuid.NewString()); err != nil {
			t.Fatal(err)
		}
	}
	if err := f.d.AllowResolve(ctx, uuid.NewString(), uuid.NewString()); !errors.Is(err, ErrPairingRateLimited) {
		t.Fatal("distributed guesses bypassed global budget", err)
	}
}

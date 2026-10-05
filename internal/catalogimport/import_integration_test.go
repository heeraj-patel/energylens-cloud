package catalogimport

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"io/fs"
	"os"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"time"

	"energylens.local/cloud/internal/catalog"
	"energylens.local/cloud/internal/migrate"
	"energylens.local/cloud/migrations"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestCatalogGenerationBackfillsExistingPublication(t *testing.T) {
	dsn := os.Getenv("ENERGYLENS_CLOUD_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("ENERGYLENS_CLOUD_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	schema := "catalog_backfill_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	admin, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	if _, err = admin.Exec(ctx, "CREATE SCHEMA "+schema); err != nil {
		t.Fatal(err)
	}
	defer admin.Exec(ctx, "DROP SCHEMA "+schema+" CASCADE")
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	cfg.ConnConfig.RuntimeParams["search_path"] = schema
	conn, err := pgx.ConnectConfig(ctx, cfg.ConnConfig)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	prior := fstest.MapFS{}
	names, err := migrate.Files(migrations.FS)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range names {
		if name == "0058_catalog_generation.sql" {
			continue
		}
		body, readErr := fs.ReadFile(migrations.FS, name)
		if readErr != nil {
			t.Fatal(readErr)
		}
		prior[name] = &fstest.MapFile{Data: body}
	}
	if err = migrate.Apply(ctx, conn, prior); err != nil {
		t.Fatal(err)
	}
	hash := strings.Repeat("a", 64)
	if _, err = conn.Exec(ctx, `INSERT INTO catalog_publications(payload_sha256,key_id,envelope_sha256,imported_by,product_count,fact_count) VALUES($1,$1,$1,'test',1,1)`, hash); err != nil {
		t.Fatal(err)
	}
	if err = migrate.Apply(ctx, conn, migrations.FS); err != nil {
		t.Fatal(err)
	}
	var generation, current int64
	if err = conn.QueryRow(ctx, `SELECT generation FROM catalog_publications WHERE payload_sha256=$1`, hash).Scan(&generation); err != nil || generation != 1 {
		t.Fatalf("backfilled generation = %d, %v", generation, err)
	}
	if err = conn.QueryRow(ctx, `SELECT generation FROM catalog_generation`).Scan(&current); err != nil || current != 1 {
		t.Fatalf("current generation = %d, %v", current, err)
	}
	if _, err = conn.Exec(ctx, `UPDATE catalog_publications SET generation=2 WHERE payload_sha256=$1`, hash); err == nil {
		t.Fatal("backfilled publication became mutable")
	}
	secondHash := strings.Repeat("b", 64)
	if _, err = conn.Exec(ctx, `INSERT INTO catalog_publications(payload_sha256,key_id,envelope_sha256,imported_by,product_count,fact_count) VALUES($1,$1,$1,'test',0,0)`, secondHash); err != nil {
		t.Fatal(err)
	}
	if err = conn.QueryRow(ctx, `SELECT generation FROM catalog_publications WHERE payload_sha256=$1`, secondHash).Scan(&generation); err != nil || generation != 2 {
		t.Fatalf("post-backfill generation = %d, %v", generation, err)
	}
}

func TestImportPublicationTransactionAndReplay(t *testing.T) {
	dsn := os.Getenv("ENERGYLENS_CLOUD_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("ENERGYLENS_CLOUD_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	schema := "catalog_test_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	admin, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	if _, err = admin.Exec(ctx, "CREATE SCHEMA "+schema); err != nil {
		t.Fatal(err)
	}
	defer admin.Exec(ctx, "DROP SCHEMA "+schema+" CASCADE")
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	cfg.ConnConfig.RuntimeParams["search_path"] = schema
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	conn, err := pgx.ConnectConfig(ctx, cfg.ConnConfig)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	if err = migrate.Apply(ctx, conn, migrations.FS); err != nil {
		t.Fatal(err)
	}
	raw, key, payload := fixture(t)
	verified, err := ParseAndVerify(raw, key)
	if err != nil {
		t.Fatal(err)
	}
	result, err := Import(ctx, pool, verified, "test-operator")
	if err != nil || result.Replayed || result.Products != 1 || result.Facts != 1 || result.Generation != 1 {
		t.Fatalf("first import %+v %v", result, err)
	}
	store := catalog.PGStore{Pool: pool}
	if _, snapshotGeneration, snapshotErr := store.SnapshotGet(ctx, payload.Products[0].ID); snapshotErr != nil || snapshotGeneration != 1 {
		t.Fatalf("initial product snapshot generation %d %v", snapshotGeneration, snapshotErr)
	}
	replay, err := Import(ctx, pool, verified, "test-operator")
	if err != nil || !replay.Replayed || replay.Generation != result.Generation {
		t.Fatalf("replay %+v %v", replay, err)
	}
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			again, importErr := Import(ctx, pool, verified, "test-operator")
			if importErr != nil || !again.Replayed || again.Generation != 1 {
				t.Errorf("concurrent replay %+v %v", again, importErr)
			}
		}()
	}
	wg.Wait()
	var fetched *string
	if err = pool.QueryRow(ctx, `SELECT source_fetched_at::text FROM catalog_fact_revisions LIMIT 1`).Scan(&fetched); err != nil || fetched != nil {
		t.Fatalf("source fetch time invented: %v %v", fetched, err)
	}
	if _, err = pool.Exec(ctx, `UPDATE catalog_fact_revisions SET value=999`); err == nil {
		t.Fatal("immutable fact updated")
	}
	conflicting := verified
	conflicting.PayloadHash = strings.Repeat("b", 64)
	conflicting.Envelope.Payload.Products[0].Brand = "Different"
	if _, err = Import(ctx, pool, conflicting, "test-operator"); err == nil {
		t.Fatal("conflicting product accepted")
	}
	var count int
	if err = pool.QueryRow(ctx, `SELECT count(*) FROM catalog_publications`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("conflict did not roll back publication: %d %v", count, err)
	}
	var generation int64
	if err = pool.QueryRow(ctx, `SELECT generation FROM catalog_generation`).Scan(&generation); err != nil || generation != 1 {
		t.Fatalf("rolled-back publication advanced generation: %d %v", generation, err)
	}
	differentEnvelope := verified
	differentEnvelope.EnvelopeHash = strings.Repeat("c", 64)
	if _, err = Import(ctx, pool, differentEnvelope, "test-operator"); err == nil {
		t.Fatal("different replay accepted")
	}
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKIXPublicKey(pub)
	if err != nil {
		t.Fatal(err)
	}
	revisionKey := pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der})
	priorID := payload.Products[0].Facts[0].ID
	replacementID := uuid.NewString()
	payload.SchemaVersion = 2
	payload.Products[0].Facts[0].ID = replacementID
	payload.Products[0].Facts[0].Value = "321.5"
	payload.Products[0].Facts[0].Revision = 2
	payload.Products[0].Facts[0].ReviewedAt = time.Now().UTC().Truncate(time.Second).Format(time.RFC3339)
	payload.Revision = &Revision{ID: uuid.NewString(), Action: "supersede", PriorFactID: priorID, ReplacementFactID: &replacementID, Reason: "corrected official value", Reviewer: "reviewer", ReviewedAt: time.Now().UTC().Truncate(time.Second).Format(time.RFC3339), ReviewSHA256: strings.Repeat("d", 64)}
	signed := signFixture(t, payload, der, priv)
	corrected, err := ParseAndVerify(signed, revisionKey)
	if err != nil {
		t.Fatal(err)
	}
	if correctedResult, importErr := Import(ctx, pool, corrected, "test-operator"); importErr != nil || correctedResult.Generation != 2 {
		t.Fatalf("correction generation %+v %v", correctedResult, importErr)
	}
	if replay, err = Import(ctx, pool, corrected, "test-operator"); err != nil || !replay.Replayed || replay.Generation != 2 {
		t.Fatalf("revision replay %+v %v", replay, err)
	}
	if _, snapshotGeneration, snapshotErr := store.SnapshotGet(ctx, payload.Products[0].ID); snapshotErr != nil || snapshotGeneration != 2 {
		t.Fatalf("corrected product snapshot generation %d %v", snapshotGeneration, snapshotErr)
	}
	product, err := store.Get(ctx, payload.Products[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	var current struct {
		Facts []struct {
			ID    string `json:"id"`
			Value string `json:"value"`
		} `json:"facts"`
	}
	if err = json.Unmarshal(product, &current); err != nil || len(current.Facts) != 1 || current.Facts[0].ID != replacementID || current.Facts[0].Value != "321.5" {
		t.Fatalf("corrected facts: %s %v", product, err)
	}
	// The previous source row and its publication stay available for audit.
	if err = pool.QueryRow(ctx, `SELECT count(*) FROM catalog_fact_revisions WHERE id IN ($1,$2)`, priorID, replacementID).Scan(&count); err != nil || count != 2 {
		t.Fatalf("historical facts: %d %v", count, err)
	}
	wrongIdentity := payload
	wrongIdentity.Products = []Product{payload.Products[0]}
	wrongIdentity.Products[0].ID = uuid.NewString()
	wrongIdentity.Products[0].Facts = []Fact{payload.Products[0].Facts[0]}
	wrongIdentity.Products[0].Facts[0].ID = uuid.NewString()
	wrongIdentity.Products[0].Facts[0].Revision = 3
	wrongIdentity.Revision = &Revision{ID: uuid.NewString(), Action: "supersede", PriorFactID: replacementID, ReplacementFactID: &wrongIdentity.Products[0].Facts[0].ID, Reason: "incorrect product", Reviewer: "reviewer", ReviewedAt: time.Now().UTC().Truncate(time.Second).Format(time.RFC3339), ReviewSHA256: strings.Repeat("c", 64)}
	invalidIdentity, err := ParseAndVerify(signFixture(t, wrongIdentity, der, priv), revisionKey)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = Import(ctx, pool, invalidIdentity, "test-operator"); err == nil {
		t.Fatal("replacement changed product identity")
	}
	conflictingSource := Payload{Schema: payload.Schema, SchemaVersion: 1, Products: []Product{payload.Products[0]}}
	conflictingSource.Products[0].Facts = []Fact{payload.Products[0].Facts[0]}
	conflictingSource.Products[0].Facts[0].ID = uuid.NewString()
	conflictingSource.Products[0].Facts[0].Revision = 1
	conflictingSource.Products[0].Facts[0].SourceHash = strings.Repeat("a", 64)
	conflictingSource.Products[0].Facts[0].ReviewedAt = time.Now().UTC().Add(time.Second).Truncate(time.Second).Format(time.RFC3339)
	conflictVerified, err := ParseAndVerify(signFixture(t, conflictingSource, der, priv), revisionKey)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = Import(ctx, pool, conflictVerified, "test-operator"); err == nil {
		t.Fatal("v1 reused an immutable source identity")
	}
	retraction := Payload{Schema: payload.Schema, SchemaVersion: 2, Products: []Product{}, Revision: &Revision{ID: uuid.NewString(), Action: "retract", PriorFactID: replacementID, Reason: "source withdrawn", Reviewer: "reviewer", ReviewedAt: time.Now().UTC().Add(time.Second).Truncate(time.Second).Format(time.RFC3339), ReviewSHA256: strings.Repeat("e", 64)}}
	retracted, err := ParseAndVerify(signFixture(t, retraction, der, priv), revisionKey)
	if err != nil {
		t.Fatal(err)
	}
	if retractedResult, importErr := Import(ctx, pool, retracted, "test-operator"); importErr != nil || retractedResult.Generation != 3 || retractedResult.Products != 0 || retractedResult.Facts != 0 {
		t.Fatalf("pure retraction generation %+v %v", retractedResult, importErr)
	}
	product, err = store.Get(ctx, payload.Products[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if err = json.Unmarshal(product, &current); err != nil || len(current.Facts) != 0 {
		t.Fatalf("retracted facts: %s %v", product, err)
	}
	if _, snapshotGeneration, snapshotErr := store.SnapshotGet(ctx, payload.Products[0].ID); snapshotErr != nil || snapshotGeneration != 3 {
		t.Fatalf("retracted product snapshot generation %d %v", snapshotGeneration, snapshotErr)
	}
	if replay, err = Import(ctx, pool, retracted, "test-operator"); err != nil || !replay.Replayed || replay.Generation != 3 {
		t.Fatalf("retraction replay %+v %v", replay, err)
	}
	stale := Payload{Schema: payload.Schema, SchemaVersion: 1, Products: []Product{payload.Products[0]}}
	stale.Products[0].Facts = []Fact{payload.Products[0].Facts[0]}
	stale.Products[0].Facts[0].ID = uuid.NewString()
	stale.Products[0].Facts[0].Revision = 1
	stale.Products[0].Facts[0].SourceHash = strings.Repeat("9", 64)
	stale.Products[0].Facts[0].ReviewedAt = time.Now().UTC().Add(-time.Minute).Truncate(time.Second).Format(time.RFC3339)
	staleVerified, err := ParseAndVerify(signFixture(t, stale, der, priv), revisionKey)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = Import(ctx, pool, staleVerified, "test-operator"); err == nil {
		t.Fatal("stale v1 publication resurrected withdrawn product evidence")
	}
	duplicate := retraction
	duplicate.Revision = &Revision{ID: uuid.NewString(), Action: "retract", PriorFactID: priorID, Reason: "second retraction", Reviewer: "reviewer", ReviewedAt: time.Now().UTC().Truncate(time.Second).Format(time.RFC3339), ReviewSHA256: strings.Repeat("f", 64)}
	verifiedDuplicate, err := ParseAndVerify(signFixture(t, duplicate, der, priv), revisionKey)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = Import(ctx, pool, verifiedDuplicate, "test-operator"); err == nil {
		t.Fatal("already closed fact retracted twice")
	}
	_, _, independent := fixture(t)
	var concurrent [2]Verified
	for i := range concurrent {
		copyPayload := independent
		copyPayload.Products = []Product{independent.Products[0]}
		copyPayload.Products[0].ID = uuid.NewString()
		copyPayload.Products[0].Facts = []Fact{independent.Products[0].Facts[0]}
		copyPayload.Products[0].Facts[0].ID = uuid.NewString()
		concurrent[i], err = ParseAndVerify(signFixture(t, copyPayload, der, priv), revisionKey)
		if err != nil {
			t.Fatal(err)
		}
	}
	var concurrentResults [2]Result
	var concurrentErrors [2]error
	for i := range concurrent {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			concurrentResults[i], concurrentErrors[i] = Import(ctx, pool, concurrent[i], "test-operator")
		}(i)
	}
	wg.Wait()
	if concurrentErrors[0] != nil || concurrentErrors[1] != nil ||
		concurrentResults[0].Replayed || concurrentResults[1].Replayed ||
		concurrentResults[0].Generation+concurrentResults[1].Generation != 9 ||
		concurrentResults[0].Generation == concurrentResults[1].Generation {
		t.Fatalf("concurrent distinct imports: %+v %+v, %v %v", concurrentResults[0], concurrentResults[1], concurrentErrors[0], concurrentErrors[1])
	}
}

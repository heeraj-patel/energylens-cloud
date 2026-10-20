package questions

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"energylens.local/cloud/internal/account"
	"energylens.local/cloud/internal/datawizrequests"
	"energylens.local/cloud/internal/inventory"
	"energylens.local/cloud/internal/migrate"
	"energylens.local/cloud/migrations"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestQuestionsOwnerUnknownZeroReplayAndInvalidation(t *testing.T) {
	dsn := os.Getenv("ENERGYLENS_CLOUD_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("ENERGYLENS_CLOUD_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	schema := "questions_test_" + strings.ReplaceAll(uuid.NewString(), "-", "")
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
	accounts := account.PGStore{Pool: pool}
	owner, err := accounts.Resolve(ctx, account.Identity{Issuer: "https://id.test", Subject: "answer-owner"})
	if err != nil {
		t.Fatal(err)
	}
	other, err := accounts.Resolve(ctx, account.Identity{Issuer: "https://id.test", Subject: "answer-other"})
	if err != nil {
		t.Fatal(err)
	}
	home, err := accounts.Create(ctx, owner.ID, account.HomeCreate{Name: "House", Timezone: "America/Chicago", Location: account.Location{CountryCode: "US", PostalCode: "78732", Precision: "postal_code", Source: "user_entered"}}, uuid.NewString(), make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	inv := inventory.PGStore{Pool: pool}
	hash := sha256.Sum256([]byte("appliance"))
	a, err := inv.Create(ctx, owner.ID, home.ID, inventory.CreateInput{Name: "Washer", Category: "washer", Identity: inventory.Identity{Brand: "Acme", FullModel: "W100", Fuel: "electricity", MatchQuality: "unresolved"}}, uuid.NewString(), hash[:])
	if err != nil {
		t.Fatal(err)
	}
	store := PGStore{Pool: pool}
	if _, err = store.List(ctx, other.ID, home.ID, "onboarding", nil); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-owner questions: %v", err)
	}
	q, err := store.List(ctx, owner.ID, home.ID, "onboarding", &a.ID)
	if err != nil || len(q) != 1 || q[0].ID != "washer.cycles_per_week" || q[0].IdentityRevision != 1 {
		t.Fatalf("washer question: %+v %v", q, err)
	}
	avoid, err := store.List(ctx, owner.ID, home.ID, "avoid", &a.ID)
	if err != nil || len(avoid) != 0 {
		t.Fatalf("washer has no avoid prompt: %+v %v", avoid, err)
	}
	faq, err := store.List(ctx, owner.ID, home.ID, "faq", &a.ID)
	if err != nil || len(faq) != 1 || faq[0].ID != "faq-contributors/v1.washer.recent_cycles" {
		t.Fatalf("washer FAQ prompt: %+v %v", faq, err)
	}
	if _, err = store.List(ctx, other.ID, home.ID, "faq", &a.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-owner FAQ prompts: %v", err)
	}
	if _, err = store.List(ctx, owner.ID, home.ID, "faq", ptr(uuid.NewString())); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown appliance: %v", err)
	}
	// A persisted run is invalidated by the answer trigger even before a
	// useful estimate rule is introduced.
	runID := uuid.NewString()
	if _, err = pool.Exec(ctx, `INSERT INTO estimate_runs(id,home_id,appliance_id,appliance_version,period_start,period_end,timezone,formula_version,dependency_hash,dependency_snapshot,result_json) VALUES($1,$2,$3,1,'2026-10-01','2026-11-01','America/Chicago','reviewed-energy/v1',$4,'{}','{}')`, runID, home.ID, a.ID, bytes.Repeat([]byte{1}, 32)); err != nil {
		t.Fatal(err)
	}
	unknown := AnswerCreate{QuestionID: q[0].ID, QuestionVersion: q[0].Version, ApplianceID: a.ID, IdentityRevision: 1, Status: "unknown", Value: json.RawMessage("null")}
	key := uuid.NewString()
	reqHash := sha256.Sum256([]byte("unknown"))
	if _, err = store.Submit(ctx, other.ID, home.ID, []AnswerCreate{unknown}, key, reqHash[:]); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-owner submit: %v", err)
	}
	answers, err := store.Submit(ctx, owner.ID, home.ID, []AnswerCreate{unknown}, key, reqHash[:])
	if err != nil || len(answers) != 1 || answers[0].Revision != 1 || answers[0].ConfirmedAt != nil {
		t.Fatalf("unknown: %+v %v", answers, err)
	}
	if answers[0].Status != "unknown" {
		t.Fatalf("answer without disclosure: %+v", answers[0])
	}
	if _, err = store.CurrentAnswers(ctx, other.ID, home.ID, a.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-owner answer read: %v", err)
	}
	current, err := store.CurrentAnswers(ctx, owner.ID, home.ID, a.ID)
	if err != nil || len(current) != 1 || current[0].Status != "unknown" || string(current[0].Value) != "null" {
		t.Fatalf("current unknown: %+v %v", current, err)
	}
	var invalidations int
	if err = pool.QueryRow(ctx, `SELECT count(*) FROM estimate_run_invalidations WHERE run_id=$1`, runID).Scan(&invalidations); err != nil || invalidations != 1 {
		t.Fatalf("answer invalidation: %d %v", invalidations, err)
	}
	replay, err := store.Submit(ctx, owner.ID, home.ID, []AnswerCreate{unknown}, key, reqHash[:])
	if err != nil || replay[0].ID != answers[0].ID {
		t.Fatalf("replay: %+v %v", replay, err)
	}
	if _, err = store.Submit(ctx, owner.ID, home.ID, []AnswerCreate{unknown}, key, bytes.Repeat([]byte{2}, 32)); !errors.Is(err, ErrKeyReuse) {
		t.Fatalf("key conflict: %v", err)
	}
	confirmed := unknown
	confirmed.Status = "confirmed"
	confirmed.Value = json.RawMessage("4")
	now := time.Now().UTC().Format(time.RFC3339)
	confirmed.ConfirmedAt = &now
	confirmedKey := uuid.NewString()
	answers, err = store.Submit(ctx, owner.ID, home.ID, []AnswerCreate{confirmed}, confirmedKey, reqHash[:])
	if err != nil || answers[0].Revision != 2 || string(answers[0].Value) != "4" {
		t.Fatalf("confirmed zero: %+v %v", answers, err)
	}
	var queued string
	if err = pool.QueryRow(ctx, `SELECT reason FROM datawiz_resolve_queue WHERE appliance_id=$1`, a.ID).Scan(&queued); err != nil || queued != "usage_answer" {
		t.Fatalf("usage answer not queued: %q %v", queued, err)
	}
	dir := t.TempDir()
	producer := datawizrequests.Producer{DB: pool, Dir: dir}
	if _, err = producer.RunOnce(ctx); err != nil {
		t.Fatalf("drain answer queue: %v", err)
	}
	paths, err := filepath.Glob(filepath.Join(dir, "resolve", "*.json"))
	if err != nil || len(paths) != 1 {
		t.Fatalf("resolve requests after queue: %d %v", len(paths), err)
	}
	body, err := os.ReadFile(paths[0])
	if err != nil {
		t.Fatal(err)
	}
	var request struct {
		Usage map[string]float64 `json:"usage"`
	}
	if err = json.Unmarshal(body, &request); err != nil || request.Usage["cycles_per_week"] != 4 {
		t.Fatalf("queued usage: %s %v", body, err)
	}
	var pending int
	if err = pool.QueryRow(ctx, `SELECT count(*) FROM datawiz_resolve_queue WHERE appliance_id=$1`, a.ID).Scan(&pending); err != nil || pending != 0 {
		t.Fatalf("queue not drained: %d %v", pending, err)
	}
	if _, err = store.Submit(ctx, owner.ID, home.ID, []AnswerCreate{confirmed}, confirmedKey, reqHash[:]); err != nil {
		t.Fatalf("confirmed replay: %v", err)
	}
	if err = pool.QueryRow(ctx, `SELECT count(*) FROM datawiz_resolve_queue WHERE appliance_id=$1`, a.ID).Scan(&pending); err != nil || pending != 0 {
		t.Fatalf("replay queued resolve: %d %v", pending, err)
	}
	current, err = store.CurrentAnswers(ctx, owner.ID, home.ID, a.ID)
	if err != nil || len(current) != 1 || current[0].Revision != 2 || current[0].Status != "confirmed" || string(current[0].Value) != "4" || current[0].ConfirmedAt == nil {
		t.Fatalf("current confirmed zero: %+v %v", current, err)
	}
	bad := confirmed
	bad.Value = json.RawMessage("-1")
	if _, err = store.Submit(ctx, owner.ID, home.ID, []AnswerCreate{bad}, uuid.NewString(), reqHash[:]); !errors.Is(err, ErrInvalid) {
		t.Fatalf("negative cycles: %v", err)
	}
	if _, err = store.Submit(ctx, owner.ID, home.ID, []AnswerCreate{confirmed, confirmed}, uuid.NewString(), reqHash[:]); !errors.Is(err, ErrInvalid) {
		t.Fatalf("duplicate batch: %v", err)
	}
	faqUnknown := AnswerCreate{QuestionID: faq[0].ID, QuestionVersion: faq[0].Version, ApplianceID: a.ID, IdentityRevision: 1, Status: "unknown", Value: json.RawMessage("null")}
	if _, err = store.Submit(ctx, owner.ID, home.ID, []AnswerCreate{faqUnknown}, uuid.NewString(), reqHash[:]); err != nil {
		t.Fatalf("save FAQ unknown: %v", err)
	}
	if err = pool.QueryRow(ctx, `SELECT count(*) FROM datawiz_resolve_queue WHERE appliance_id=$1`, a.ID).Scan(&pending); err != nil || pending != 0 {
		t.Fatalf("FAQ queued resolve: %d %v", pending, err)
	}
	faq, err = store.List(ctx, owner.ID, home.ID, "faq", &a.ID)
	if err != nil || len(faq) != 0 {
		t.Fatalf("answered FAQ should advance flow: %+v %v", faq, err)
	}
	current, err = store.CurrentAnswers(ctx, owner.ID, home.ID, a.ID)
	if err != nil || len(current) != 2 {
		t.Fatalf("current answers include FAQ: %+v %v", current, err)
	}
	computerHash := sha256.Sum256([]byte("computer"))
	computer, err := inv.Create(ctx, owner.ID, home.ID, inventory.CreateInput{Name: "Computer", Category: "computer", Identity: inventory.Identity{Brand: "Acme", FullModel: "C100", Fuel: "electricity", MatchQuality: "unresolved"}}, uuid.NewString(), computerHash[:])
	if err != nil {
		t.Fatal(err)
	}
	avoid, err = store.List(ctx, owner.ID, home.ID, "avoid", &computer.ID)
	if err != nil || len(avoid) != 1 || avoid[0].ID != "avoid.computer.unused_on_hours_per_week" {
		t.Fatalf("computer avoid prompt: %+v %v", avoid, err)
	}
	avoidConfirmed := AnswerCreate{QuestionID: avoid[0].ID, QuestionVersion: avoid[0].Version, ApplianceID: computer.ID, IdentityRevision: 1, Status: "confirmed", Value: json.RawMessage("0"), ConfirmedAt: &now}
	if _, err = store.Submit(ctx, owner.ID, home.ID, []AnswerCreate{avoidConfirmed}, uuid.NewString(), reqHash[:]); err != nil {
		t.Fatalf("save avoid zero: %v", err)
	}
	if err = pool.QueryRow(ctx, `SELECT count(*) FROM datawiz_resolve_queue WHERE appliance_id=$1`, computer.ID).Scan(&pending); err != nil || pending != 0 {
		t.Fatalf("avoid queued resolve: %d %v", pending, err)
	}
	avoid, err = store.List(ctx, owner.ID, home.ID, "avoid", &computer.ID)
	if err != nil || len(avoid) != 0 {
		t.Fatalf("answered avoid should advance flow: %+v %v", avoid, err)
	}
	name := "Updated washer"
	patchHash := sha256.Sum256([]byte("patch"))
	if _, err = inv.Patch(ctx, owner.ID, home.ID, a.ID, inventory.PatchInput{Name: &name}, 1, uuid.NewString(), patchHash[:]); err != nil {
		t.Fatal(err)
	}
	current, err = store.CurrentAnswers(ctx, owner.ID, home.ID, a.ID)
	if err != nil || len(current) != 0 {
		t.Fatalf("changed appliance revision must not prefill: %+v %v", current, err)
	}
	if _, err = store.Submit(ctx, owner.ID, home.ID, []AnswerCreate{confirmed}, uuid.NewString(), reqHash[:]); !errors.Is(err, ErrVersion) {
		t.Fatalf("stale identity answer: %v", err)
	}
	if _, err = pool.Exec(ctx, `UPDATE question_answers SET status='unknown' WHERE id=$1`, answers[0].ID); err == nil {
		t.Fatal("immutable answer changed")
	}
	if _, err = pool.Exec(ctx, `UPDATE appliances SET identity='{"brand":"Acme","full_model":"W100","variant":"US-120V","fuel":"electricity","match_quality":"confirmed_exact"}',version=3 WHERE id=$1`, a.ID); err != nil {
		t.Fatal(err)
	}
	factID := uuid.NewString()
	factHash := strings.Repeat("a", 64)
	if _, err = pool.Exec(ctx, `INSERT INTO reviewed_usage_publications(payload_sha256,key_id,envelope_sha256,imported_by,fact_count) VALUES($1,$1,$1,'operator',1)`, factHash); err != nil {
		t.Fatal(err)
	}
	if _, err = pool.Exec(ctx, `INSERT INTO reviewed_usage_facts
		(id,publication_sha256,category,manufacturer,model,fuel,variant,match_quality,documented_scope,matched_model,measurement_kind,value,unit,test_basis,cycle_definition,included_energy,source_kind,source_url,source_sha256,source_page_index,review_sha256,reviewer,reviewed_at,source_manifest,fact_json)
		VALUES($1,$2,'washer','Acme','W100','electricity','US-120V','confirmed_exact','W100','W100','cycle_energy',0.5,'kWh/cycle','normal cycle','normal cycle','{"status":"documented","components":["appliance_electricity"]}','oem_pdf','https://example.org/washer.pdf',$2,0,$2,'reviewer',now(),'washer.json','{}')`, factID, factHash); err != nil {
		t.Fatal(err)
	}
	cycleQuestions, err := store.List(ctx, owner.ID, home.ID, "onboarding", &a.ID)
	if err != nil || len(cycleQuestions) != 2 || cycleQuestions[1].ID != ReviewedCycleQuestionID("washer", factID) || cycleQuestions[1].CycleDefinition != "normal cycle" || cycleQuestions[1].SourceFactID != factID || cycleQuestions[1].IdentityRevision != 3 {
		t.Fatalf("reviewed cycle prompt: %+v %v", cycleQuestions, err)
	}
	cycleAnswer := AnswerCreate{QuestionID: cycleQuestions[1].ID, QuestionVersion: 1, ApplianceID: a.ID, IdentityRevision: 3, Status: "confirmed", Value: json.RawMessage("3"), ConfirmedAt: &now}
	if _, err = store.Submit(ctx, owner.ID, home.ID, []AnswerCreate{cycleAnswer}, uuid.NewString(), reqHash[:]); err != nil {
		t.Fatalf("matched cycle answer: %v", err)
	}
	if _, err = pool.Exec(ctx, `INSERT INTO reviewed_usage_fact_revisions(id,publication_sha256,prior_fact_id,action,reason,reviewer,reviewed_at,review_sha256) VALUES($1,$2,$3,'retract','source corrected','reviewer',now(),$2)`, uuid.NewString(), factHash, factID); err != nil {
		t.Fatal(err)
	}
	cycleQuestions, err = store.List(ctx, owner.ID, home.ID, "onboarding", &a.ID)
	if err != nil || len(cycleQuestions) != 1 {
		t.Fatalf("retracted source still prompted: %+v %v", cycleQuestions, err)
	}
	if _, err = store.Submit(ctx, owner.ID, home.ID, []AnswerCreate{cycleAnswer}, uuid.NewString(), reqHash[:]); !errors.Is(err, ErrInvalid) {
		t.Fatalf("retracted source still accepts answers: %v", err)
	}
	checkChoiceQuestions(t, ctx, pool, store, inv, owner.ID, home.ID, now)
}
func ptr(v string) *string { return &v }

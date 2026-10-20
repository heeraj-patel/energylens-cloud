package questions

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"energylens.local/cloud/internal/datawizrequests"
	"energylens.local/cloud/internal/inventory"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

func addAppliance(t *testing.T, ctx context.Context, inv inventory.PGStore, owner, home, category string) inventory.Appliance {
	t.Helper()
	h := sha256.Sum256([]byte(category))
	a, err := inv.Create(ctx, owner, home, inventory.CreateInput{Name: category, Category: category, Identity: inventory.Identity{Brand: "Acme", FullModel: "X1", Fuel: "electricity", MatchQuality: "unresolved"}}, uuid.NewString(), h[:])
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func resolveUsage(t *testing.T, ctx context.Context, pool *pgxpool.Pool, applianceID string) map[string]float64 {
	t.Helper()
	dir := t.TempDir()
	if _, err := (datawizrequests.Producer{DB: pool, Dir: dir}).RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	paths, err := filepath.Glob(filepath.Join(dir, "resolve", "*.json"))
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range paths {
		body, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		var request struct {
			ApplianceRef string             `json:"appliance_ref"`
			Usage        map[string]float64 `json:"usage"`
		}
		if err = json.Unmarshal(body, &request); err != nil {
			t.Fatal(err)
		}
		if request.ApplianceRef == "cloud:"+applianceID {
			return request.Usage
		}
	}
	t.Fatalf("no resolve request for %s in %d files", applianceID, len(paths))
	return nil
}

func checkChoiceQuestions(t *testing.T, ctx context.Context, pool *pgxpool.Pool, store PGStore, inv inventory.PGStore, owner, home, now string) {
	t.Helper()
	answer := func(a inventory.Appliance, q Question, value string) error {
		in := AnswerCreate{QuestionID: q.ID, QuestionVersion: q.Version, ApplianceID: a.ID, IdentityRevision: 1, Status: "confirmed", Value: json.RawMessage(value), ConfirmedAt: &now}
		h := sha256.Sum256([]byte(value))
		_, err := store.Submit(ctx, owner, home, []AnswerCreate{in}, uuid.NewString(), h[:])
		return err
	}
	byID := func(qs []Question) map[string]Question {
		m := map[string]Question{}
		for _, q := range qs {
			m[q.ID] = q
		}
		return m
	}

	fridge := addAppliance(t, ctx, inv, owner, home, "refrigerator")
	qs, err := store.List(ctx, owner, home, "onboarding", &fridge.ID)
	m := byID(qs)
	if err != nil || len(qs) != 2 || m["refrigerator.age_band"].AnswerKind != "choice" || len(m["refrigerator.age_band"].Options) != 3 || m["refrigerator.unconditioned_space"].Version != 2 {
		t.Fatalf("refrigerator questions: %+v %v", qs, err)
	}
	if err = answer(fridge, m["refrigerator.age_band"], "1.1"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("value outside options accepted: %v", err)
	}
	if err = answer(fridge, m["refrigerator.unconditioned_space"], "2"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("value outside options accepted: %v", err)
	}
	if err = answer(fridge, m["refrigerator.age_band"], "1.25"); err != nil {
		t.Fatalf("age band: %v", err)
	}
	if err = answer(fridge, m["refrigerator.unconditioned_space"], "1"); err != nil {
		t.Fatalf("unconditioned space: %v", err)
	}
	usage := resolveUsage(t, ctx, pool, fridge.ID)
	if len(usage) != 2 || usage["age_factor"] != 1.25 || usage["unconditioned_space"] != 1 {
		t.Fatalf("fridge usage: %v", usage)
	}

	heater := addAppliance(t, ctx, inv, owner, home, "space_heater")
	qs, err = store.List(ctx, owner, home, "onboarding", &heater.ID)
	m = byID(qs)
	if err != nil || len(qs) != 2 || len(m["space_heater.hours_per_week"].Options) != 5 || len(m["space_heater.season_share"].Options) != 4 {
		t.Fatalf("heater questions: %+v %v", qs, err)
	}
	if err = answer(heater, m["space_heater.hours_per_week"], "20"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("free-form hours accepted: %v", err)
	}
	if err = answer(heater, m["space_heater.hours_per_week"], "14"); err != nil {
		t.Fatal(err)
	}
	if err = answer(heater, m["space_heater.season_share"], "0.5"); err != nil {
		t.Fatal(err)
	}
	usage = resolveUsage(t, ctx, pool, heater.ID)
	if usage["hours_per_week"] != 14 || usage["season_share"] != 0.5 {
		t.Fatalf("heater usage: %v", usage)
	}

	fan := addAppliance(t, ctx, inv, owner, home, "fan")
	qs, err = store.List(ctx, owner, home, "onboarding", &fan.ID)
	m = byID(qs)
	if err != nil || len(qs) != 2 || m["fan.hours_per_week"].ID == "" || m["fan.season_share"].ID == "" {
		t.Fatalf("fan questions: %+v %v", qs, err)
	}
	if err = answer(fan, m["fan.season_share"], "0.25"); err != nil {
		t.Fatal(err)
	}
	if usage = resolveUsage(t, ctx, pool, fan.ID); usage["season_share"] != 0.25 {
		t.Fatalf("fan usage: %v", usage)
	}

	tv := addAppliance(t, ctx, inv, owner, home, "tv")
	if _, err = pool.Exec(ctx, `INSERT INTO question_answers(home_id,appliance_id,appliance_version,question_id,question_version,revision,status,answer_value,confirmed_at) VALUES($1,$2,1,'television.hours_per_week',1,1,'confirmed','35',now())`, home, tv.ID); err != nil {
		t.Fatal(err)
	}
	if err = datawizrequests.Queue(ctx, pool, tv.ID, "usage_answer"); err != nil {
		t.Fatal(err)
	}
	if usage = resolveUsage(t, ctx, pool, tv.ID); usage["hours_per_week"] != 35 {
		t.Fatalf("v1 answer not mapped: %v", usage)
	}
}

package questions

import "testing"

func TestWaterHeaterHouseholdSizeQuestion(t *testing.T) {
	qs := questionsForMapped("gas_water_heater", "water_heater", "a1", 1, "onboarding")
	if len(qs) != 1 {
		t.Fatalf("questions: %+v", qs)
	}
	q := qs[0]
	if q.ID != "water_heater.household_size" || q.AnswerKind != "choice" || !q.Optional || q.Version != onboardingVersion || q.Prompt != "How many people live in your home?" {
		t.Fatalf("question: %+v", q)
	}
	want := []Option{{"people_0_2", 38}, {"people_3_5", 64}, {"people_6_plus", 91}}
	if len(q.Options) != len(want) {
		t.Fatalf("options: %+v", q.Options)
	}
	for i, o := range want {
		if q.Options[i] != o {
			t.Fatalf("option %d: %+v", i, q.Options[i])
		}
	}
	if !isUsageQuestion(q.ID) || !Applies("water_heater", "water_heater", q.ID) {
		t.Fatal("household size is not a usage question")
	}
}

func TestBoilerGetsHvacUsageQuestions(t *testing.T) {
	qs := questionsForMapped("boiler", "boiler", "a1", 1, "onboarding")
	if len(qs) != 2 || qs[0].ID != "hvac.hours_per_week" || qs[1].ID != "hvac.season_share" {
		t.Fatalf("questions: %+v", qs)
	}
	hvac := questionsFor("hvac", "a1", 1, "onboarding")
	for i, q := range qs {
		if q.Prompt != hvac[i].Prompt || q.Unit != hvac[i].Unit || len(q.Options) != len(hvac[i].Options) {
			t.Fatalf("question %d differs from hvac: %+v", i, q)
		}
		if !isUsageQuestion(q.ID) || !Applies("boiler", "boiler", q.ID) {
			t.Fatalf("%s is not an applicable usage question", q.ID)
		}
	}
}

func TestHouseholdSizeAskedOnlyForGasFuels(t *testing.T) {
	for fuel, want := range map[string]bool{"natural_gas": true, "propane": true, "electricity": false, "": false, "fuel_oil": false} {
		if got := askedForFuel("water_heater.household_size", fuel); got != want {
			t.Fatalf("fuel %q: got %v", fuel, got)
		}
	}
	if !askedForFuel("hvac.hours_per_week", "electricity") {
		t.Fatal("other questions must not be gated")
	}
}

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

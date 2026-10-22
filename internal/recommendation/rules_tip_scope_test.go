package recommendation

import (
	"encoding/json"
	"testing"

	"energylens.local/cloud/internal/datawizfacts"
)

func TestHabitsTipWithoutCategoriesAttachesToAnyCategory(t *testing.T) {
	habits := researchTip("habits", "habits")
	habits.Value = json.RawMessage(`{"context":"habits","applicable_categories":[]}`)
	night := researchTip("night", "night")
	night.Value = json.RawMessage(`{"context":"night","applicable_categories":[]}`)
	a := Appliance{ID: "tv-1", Category: "television", Fuel: "electricity", ResearchTips: []datawizfacts.Fact{habits, night}}
	got := Evaluate(a)
	assertTipAttachments(t, got[0], []datawizfacts.Fact{habits})
}

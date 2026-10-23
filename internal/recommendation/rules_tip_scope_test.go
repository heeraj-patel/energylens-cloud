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

func TestResearchTipsSkipNonElectricFuelUnlessTagged(t *testing.T) {
	plain := researchTip("plain", "habits")
	tagged := researchTip("tagged", "habits")
	tagged.Value = json.RawMessage(`{"context":"habits","applicable_categories":["hvac"],"applicable_fuels":["natural_gas"]}`)
	tips := []datawizfacts.Fact{plain, tagged}
	cases := []struct {
		fuel string
		want []datawizfacts.Fact
	}{
		{"", []datawizfacts.Fact{plain}},
		{"electricity", []datawizfacts.Fact{plain}},
		{"natural_gas", []datawizfacts.Fact{tagged}},
		{"propane", nil},
	}
	for _, c := range cases {
		got := Evaluate(Appliance{ID: "h", Category: "hvac", Fuel: c.fuel, ResearchTips: tips})
		assertTipAttachments(t, got[0], c.want)
	}
}

func TestCategoryTipsPrecedeSingleGeneralTip(t *testing.T) {
	general := func(id string) datawizfacts.Fact {
		f := researchTip(id, "habits")
		f.Value = json.RawMessage(`{"context":"habits","applicable_categories":[]}`)
		return f
	}
	cat := []datawizfacts.Fact{researchTip("c1", "appliance_type"), researchTip("c2", "upgrades")}
	tips := []datawizfacts.Fact{general("g1"), general("g2"), cat[0], cat[1]}
	a := Appliance{ID: "hvac-1", Category: "hvac", Fuel: "electricity", ResearchTips: tips}
	assertTipAttachments(t, Evaluate(a)[0], []datawizfacts.Fact{cat[0], cat[1], tips[0]})
	a.ResearchTips = tips[:2]
	assertTipAttachments(t, Evaluate(a)[0], tips[:1])
}

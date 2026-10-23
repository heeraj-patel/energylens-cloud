package recommendation

import (
	"encoding/json"
	"math/big"
	"net/http"
	"strings"
	"testing"
	"time"

	"energylens.local/cloud/internal/datawizfacts"
	"energylens.local/cloud/internal/householdenergy"
)

const wxApplianceID = "11111111-1111-4111-8111-111111111111"

func keyedTip(id, key, context, savingsType string, categories ...string) datawizfacts.Fact {
	tip := weatherTip(id, context, savingsType, categories...)
	tip.RecordKey = key
	return tip
}

func ratOf(v string) *big.Rat {
	r, _ := new(big.Rat).SetString(v)
	return r
}

func savingsOf(electric, gas string) householdenergy.WeatherSavings {
	sv := householdenergy.WeatherSavings{Devices: map[string]householdenergy.WeatherDevice{}, ShiftCurrency: "USD"}
	if electric != "" {
		sv.ElectricMonthly, sv.ElectricCurrency = ratOf(electric), "USD"
	}
	if gas != "" {
		sv.GasMonthly, sv.GasCurrency = ratOf(gas), "USD"
	}
	return sv
}

func qualifiedIDs(a Appliance, tips []datawizfacts.Fact, w *WeatherState, sv householdenergy.WeatherSavings) string {
	return strings.Join(tipIDs(qualifyingWeatherTips(a, tips, w, sv)), ",")
}

func TestWeatherTipNeedsTenDollarsAMonth(t *testing.T) {
	tips := []datawizfacts.Fact{keyedTip("hot", "official:weather_hot:W1:aa", "weather_hot", "energy_reduction", "hvac")}
	w := &WeatherState{HotRun: true}
	for cost, want := range map[string]string{"100.00": "hot", "99.90": "", "100.04": "hot", "99.94": ""} {
		if got := qualifiedIDs(hvacWith(w), tips, w, savingsOf(cost, "")); got != want {
			t.Fatalf("electric %s: got %q, want %q", cost, got, want)
		}
	}
}

func TestWeatherTipEstimateFollowsTheTrigger(t *testing.T) {
	tips := []datawizfacts.Fact{
		keyedTip("hot", "official:weather_hot:W1:aa", "weather_hot", "energy_reduction", "hvac"),
		keyedTip("cold", "official:weather_cold:W2:bb", "weather_cold", "energy_reduction", "hvac"),
	}
	both := &WeatherState{HotRun: true, ColdRun: true}
	for name, c := range map[string]struct {
		sv   householdenergy.WeatherSavings
		want string
	}{
		"electric only":              {savingsOf("100", ""), "hot"},
		"gas only":                   {savingsOf("", "100"), "cold"},
		"no gas: no cold device tip": {savingsOf("500", ""), "hot"},
		"both":                       {savingsOf("100", "100"), "hot,cold"},
		"no cost data":               {savingsOf("", ""), ""},
	} {
		if got := qualifiedIDs(hvacWith(both), tips, both, c.sv); got != c.want {
			t.Fatalf("%s: got %q, want %q", name, got, c.want)
		}
	}
	cad := savingsOf("500", "500")
	cad.ElectricCurrency, cad.GasCurrency = "CAD", "CAD"
	if got := qualifiedIDs(hvacWith(both), tips, both, cad); got != "" {
		t.Fatalf("non-USD kept %q", got)
	}
}

func TestShiftTipUsesTheMeasuredSavingElseTheCostShare(t *testing.T) {
	shift := keyedTip("shift", "official:weather_hot:W8:aa", "weather_hot", "load_shift", "hvac")
	w := &WeatherState{HotRun: true}
	a := hvacWith(w)
	withDevice := func(d householdenergy.WeatherDevice, electric string) householdenergy.WeatherSavings {
		sv := savingsOf(electric, "")
		sv.Devices[a.ID] = d
		return sv
	}
	for name, c := range map[string]struct {
		sv   householdenergy.WeatherSavings
		want string
	}{
		"measured 10.00 kept":                   {withDevice(householdenergy.WeatherDevice{Metered: true, MonthlyShiftSaving: ratOf("10")}, "500"), "shift"},
		"measured 9.99 dropped":                 {withDevice(householdenergy.WeatherDevice{Metered: true, MonthlyShiftSaving: ratOf("9.99")}, "500"), ""},
		"measured replaces a larger cost share": {withDevice(householdenergy.WeatherDevice{Metered: true, MonthlyShiftSaving: ratOf("3")}, "500"), ""},
		"unmetered uses the cost share":         {withDevice(householdenergy.WeatherDevice{}, "500"), "shift"},
		"flat tariff uses the cost share":       {withDevice(householdenergy.WeatherDevice{Metered: true}, "500"), "shift"},
	} {
		if got := qualifiedIDs(a, []datawizfacts.Fact{shift}, w, c.sv); got != c.want {
			t.Fatalf("%s: got %q, want %q", name, got, c.want)
		}
	}
	cad := withDevice(householdenergy.WeatherDevice{Metered: true, MonthlyShiftSaving: ratOf("50")}, "")
	cad.ShiftCurrency = "CAD"
	if got := qualifiedIDs(a, []datawizfacts.Fact{shift}, w, cad); got != "" {
		t.Fatalf("CAD shift kept %q", got)
	}
}

func TestWeatherTipsDedupeByNumberKeepingStoreOrder(t *testing.T) {
	tips := []datawizfacts.Fact{
		keyedTip("w8-a", "official:weather_hot:W8:aaa", "weather_hot", "energy_reduction", "hvac"),
		keyedTip("w8-b", "official:weather_hot:W8:bbb", "weather_hot", "energy_reduction", "hvac"),
		keyedTip("w8-c", "official:weather_hot:W8:ccc", "weather_hot", "energy_reduction", "hvac"),
	}
	w := &WeatherState{HotRun: true}
	if got := qualifiedIDs(hvacWith(w), tips, w, savingsOf("500", "")); got != "w8-a" {
		t.Fatalf("got %q", got)
	}
}

func TestWeatherTipOrderIsEstimateThenTriggerThenKey(t *testing.T) {
	sv := savingsOf("200", "200")
	sv.Devices[wxApplianceID] = householdenergy.WeatherDevice{Metered: true, MonthlyShiftSaving: ratOf("30")}
	tips := []datawizfacts.Fact{
		keyedTip("z-run", "official:weather_hot:W3:zz", "weather_hot", "energy_reduction", "hvac"),
		keyedTip("cold-alert", "official:weather_cold:W2:bb", "weather_cold", "energy_reduction", "hvac"),
		keyedTip("a-run", "official:weather_hot:W1:aa", "weather_hot", "energy_reduction", "hvac"),
		keyedTip("shift", "official:weather_hot:W8:cc", "weather_hot", "load_shift", "hvac"),
	}
	a := Appliance{ID: wxApplianceID, Category: "hvac", Fuel: "electricity"}
	w := &WeatherState{HotRun: true, ColdAlert: true}
	// $30 shift first; the $20 ties go to the alert-backed cold tip, then by record key.
	if got := qualifiedIDs(a, tips, w, sv); got != "shift,cold-alert,a-run,z-run" {
		t.Fatalf("order %q", got)
	}
	tips = append(tips, keyedTip("m-run", "official:weather_hot:W4:mm", "weather_hot", "energy_reduction", "hvac"))
	action := recommendationFor(t, tips, nil, sv)
	if got := strings.Join(tipIDs(action.ResearchEvidence), ","); got != "shift,a-run,z-run" {
		t.Fatalf("at most three weather tips: %q", got)
	}
}

func recommendationFor(t *testing.T, weatherTips, research []datawizfacts.Fact, sv householdenergy.WeatherSavings) Action {
	t.Helper()
	base := &researchStubStore{stubStore: &stubStore{appliances: []Appliance{{ID: wxApplianceID, Category: "hvac", Fuel: "electricity", ResearchTips: research}}}, tips: research}
	snap := availableSnapshot()
	snap.Alerts = snap.Alerts[:1]
	svc := Service{Store: weatherStubStore{base, weatherTips}, Weather: &fakeWeather{snap: snap}, WeatherSavings: fakeSavings{sv}}
	code, raw := getRecommendations(t, svc, "?capability=weather_tips_v1")
	var body weatherBody
	if code != http.StatusOK || json.Unmarshal(raw, &body) != nil || len(body.Actions) != 1 {
		t.Fatalf("%d %s", code, raw)
	}
	return body.Actions[0]
}

func TestWeatherTipsFillTheCapBeforeCategoryAndGeneralTips(t *testing.T) {
	sv := savingsOf("200", "")
	sv.Devices[wxApplianceID] = householdenergy.WeatherDevice{Metered: true}
	weatherTips := []datawizfacts.Fact{
		keyedTip("w1", "official:weather_hot:W1:aa", "weather_hot", "energy_reduction", "hvac"),
		keyedTip("w2", "official:weather_hot:W2:aa", "weather_hot", "energy_reduction", "hvac"),
	}
	research := []datawizfacts.Fact{researchTip("night", "night"), researchTip("maint", "maintenance"), habitsTip("h1"), habitsTip("h2")}
	action := recommendationFor(t, weatherTips, research, sv)
	if got := strings.Join(tipIDs(action.ResearchEvidence), ","); got != "w1,w2,night" || action.WeatherContext == nil || !action.WeatherContext.MeasuredUse {
		t.Fatalf("attached %q context %+v", got, action.WeatherContext)
	}
	action = recommendationFor(t, weatherTips[:1], []datawizfacts.Fact{habitsTip("h1"), habitsTip("h2")}, sv)
	if got := strings.Join(tipIDs(action.ResearchEvidence), ","); got != "w1,h1" {
		t.Fatalf("one general tip: %q", got)
	}
	action = recommendationFor(t, weatherTips, research, savingsOf("50", ""))
	if got := strings.Join(tipIDs(action.ResearchEvidence), ","); got != "night,maint,h1" || action.WeatherContext != nil {
		t.Fatalf("dropped weather tips: %q context %+v", got, action.WeatherContext)
	}
}

func TestMeasuredUseFollowsMetering(t *testing.T) {
	weatherTips := []datawizfacts.Fact{keyedTip("w1", "official:weather_hot:W1:aa", "weather_hot", "energy_reduction", "hvac")}
	for _, metered := range []bool{true, false} {
		sv := savingsOf("200", "")
		sv.Devices[wxApplianceID] = householdenergy.WeatherDevice{Metered: metered}
		action := recommendationFor(t, weatherTips, nil, sv)
		if action.WeatherContext == nil || action.WeatherContext.MeasuredUse != metered {
			t.Fatalf("metered %v: %+v", metered, action.WeatherContext)
		}
	}
}

func TestDeviceWeatherTipsDropWithoutAnyCostData(t *testing.T) {
	weatherTips := []datawizfacts.Fact{keyedTip("w1", "official:weather_hot:W1:aa", "weather_hot", "energy_reduction", "hvac")}
	action := recommendationFor(t, weatherTips, nil, householdenergy.WeatherSavings{})
	if len(action.ResearchEvidence) != 0 || action.WeatherContext != nil {
		t.Fatalf("%+v", action)
	}
}

func TestActiveWeatherKeepsOnlyQualifyingDeviceTips(t *testing.T) {
	home := keyedTip("home", "official:weather_hot:W5:aa", "weather_hot", "energy_reduction", "home")
	pump := keyedTip("pump-1", "official:weather_hot:W8:aa", "weather_hot", "load_shift", "hvac")
	pumpAgain := keyedTip("pump-2", "official:weather_hot:W8:bb", "weather_hot", "load_shift", "hvac")
	other := keyedTip("other", "official:weather_hot:W9:aa", "weather_hot", "energy_reduction", "pool_pump")
	base := &researchStubStore{stubStore: &stubStore{appliances: []Appliance{{ID: wxApplianceID, Category: "hvac", Fuel: "electricity"}}}}
	for name, c := range map[string]struct {
		sv   householdenergy.WeatherSavings
		want string
	}{
		"qualifies": {savingsOf("500", ""), "home,pump-1"},
		"too small": {savingsOf("50", ""), "home"},
		"no cost":   {savingsOf("", ""), "home"},
	} {
		svc := Service{Store: weatherStubStore{base, []datawizfacts.Fact{home, pump, pumpAgain, other}}, Weather: &fakeWeather{snap: availableSnapshot()}, WeatherSavings: fakeSavings{c.sv}}
		_, tips, err := svc.ActiveWeather(t.Context(), "owner", "home", time.Now())
		if err != nil {
			t.Fatal(err)
		}
		var ids []string
		for _, tip := range tips {
			ids = append(ids, tip.Fact.ID)
		}
		if strings.Join(ids, ",") != c.want {
			t.Fatalf("%s: got %v, want %s", name, ids, c.want)
		}
	}
}

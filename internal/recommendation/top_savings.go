package recommendation

import (
	"context"
	"encoding/json"
	"energylens.local/cloud/internal/householdenergy"
	"errors"
	"log/slog"
	"math/big"
	"net/http"
	"sort"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

type moneyValue struct {
	Amount   string `json:"amount"`
	Currency string `json:"currency"`
}

type topSavingsMove struct {
	ApplianceID    *string     `json:"appliance_id"`
	DeviceID       *string     `json:"device_id"`
	Name           string      `json:"name"`
	MonthlySavings moneyValue  `json:"monthly_savings"`
	Basis          string      `json:"basis"`
	EvidenceBasis  string      `json:"evidence_basis"`
	DailyWaste     *moneyValue `json:"daily_waste"`
	ActionID       *string     `json:"action_id"`
	ActionTitle    *string     `json:"action_title"`
}

type savingsCandidate struct {
	applianceID, deviceID, name string
	amount                      *big.Rat
	currency                    string
	basis, evidenceBasis        string
	waste                       *big.Rat
	action                      *Action
}

func (s Service) topSavingsMove(w http.ResponseWriter, r *http.Request) {
	home := chi.URLParam(r, "home")
	if _, err := uuid.Parse(home); err != nil {
		fail(w, http.StatusNotFound, "not_found")
		return
	}
	now := time.Now().UTC()
	owner := s.UserID(r)
	appliances, err := s.Store.Current(r.Context(), owner, home, now)
	if errors.Is(err, ErrNotFound) {
		fail(w, http.StatusNotFound, "not_found")
		return
	}
	if err != nil {
		slog.Error("recommendation evidence read failed", "error", err)
		fail(w, http.StatusInternalServerError, "internal_error")
		return
	}
	var waste []householdenergy.DeviceWaste
	if s.Waste != nil {
		waste, err = s.Waste.DeviceMonthlyWaste(r.Context(), owner, home)
		if errors.Is(err, householdenergy.ErrNotFound) {
			fail(w, http.StatusNotFound, "not_found")
			return
		}
		if err != nil {
			slog.Error("top savings waste read failed", "error", err)
			fail(w, http.StatusInternalServerError, "internal_error")
			return
		}
	}
	var actions []Action
	for _, appliance := range appliances {
		actions = append(actions, Evaluate(appliance)...)
	}
	actions = rankActions(actions)
	window := struct {
		Start        string `json:"start"`
		EndExclusive string `json:"end_exclusive"`
	}{now.Format(time.RFC3339), now.Format(time.RFC3339)}
	if len(waste) > 0 {
		window.Start, window.EndExclusive = waste[0].WindowStart.UTC().Format(time.RFC3339), waste[0].WindowEnd.UTC().Format(time.RFC3339)
	}
	var move *topSavingsMove
	if best := bestSavingsCandidate(appliances, waste, actions); best != nil {
		move = best.response()
	}
	w.Header().Set("Cache-Control", "private, no-store")
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(struct {
		HomeID      string          `json:"home_id"`
		GeneratedAt string          `json:"generated_at"`
		Window      any             `json:"window"`
		Move        *topSavingsMove `json:"move"`
	}{home, now.Format(time.RFC3339), window, move})
}

func bestSavingsCandidate(appliances []Appliance, waste []householdenergy.DeviceWaste, actions []Action) *savingsCandidate {
	byID := map[string]*savingsCandidate{}
	var order []string
	names := map[string]string{}
	for _, appliance := range appliances {
		names[appliance.ID] = appliance.Category
	}
	currency := ""
	for _, item := range waste {
		if item.MonthlyWaste == nil || item.MonthlyWaste.Sign() <= 0 {
			continue
		}
		if currency == "" {
			currency = item.Currency
		}
		if item.Currency != currency {
			continue
		}
		key := item.ApplianceID
		if key == "" {
			key = "device:" + item.DeviceID
		}
		byID[key] = &savingsCandidate{applianceID: item.ApplianceID, deviceID: item.DeviceID, name: item.Name, amount: item.MonthlyWaste, currency: item.Currency, basis: "idle_waste", evidenceBasis: item.EvidenceBasis, waste: item.MonthlyWaste}
		order = append(order, key)
	}
	for i := range actions {
		action := actions[i]
		saving, ok := comparableSaving(action)
		if !ok || action.ApplianceID == "" {
			continue
		}
		monthly, ok := monthlySaving(saving, action.SavingsPeriod)
		if !ok || monthly.Sign() <= 0 {
			continue
		}
		if currency == "" {
			currency = action.SavingsCurrency
		}
		if action.SavingsCurrency != currency {
			continue
		}
		cur := byID[action.ApplianceID]
		if cur == nil {
			cur = &savingsCandidate{applianceID: action.ApplianceID, name: names[action.ApplianceID], currency: currency, evidenceBasis: "estimated"}
			byID[action.ApplianceID] = cur
			order = append(order, action.ApplianceID)
		}
		if cur.action == nil || monthlyOf(cur.action).Cmp(monthly) < 0 {
			cur.action = &actions[i]
			if cur.basis == "" || monthly.Cmp(cur.amount) > 0 {
				cur.amount, cur.basis = monthly, "recommendation"
			}
		}
	}
	if len(order) == 0 {
		return nil
	}
	for _, key := range order {
		cur := byID[key]
		if cur.name == "" {
			cur.name = names[cur.applianceID]
		}
		if cur.name == "" {
			cur.name = cur.applianceID + cur.deviceID
		}
	}
	sort.Slice(order, func(i, j int) bool {
		a, b := byID[order[i]], byID[order[j]]
		if c := a.amount.Cmp(b.amount); c != 0 {
			return c > 0
		}
		if a.name != b.name {
			return a.name < b.name
		}
		return order[i] < order[j]
	})
	return byID[order[0]]
}

func monthlyOf(action *Action) *big.Rat {
	saving, _ := comparableSaving(*action)
	monthly, _ := monthlySaving(saving, action.SavingsPeriod)
	return monthly
}

func monthlySaving(value *big.Rat, period string) (*big.Rat, bool) {
	switch period {
	case "year":
		return new(big.Rat).Quo(value, big.NewRat(12, 1)), true
	case "month":
		return value, true
	case "day":
		return new(big.Rat).Mul(value, big.NewRat(30, 1)), true
	default:
		return nil, false
	}
}

func (c *savingsCandidate) response() *topSavingsMove {
	move := &topSavingsMove{Name: c.name, MonthlySavings: moneyValue{c.amount.FloatString(2), c.currency}, Basis: c.basis, EvidenceBasis: c.evidenceBasis}
	if c.applianceID != "" {
		move.ApplianceID = &c.applianceID
	}
	if c.deviceID != "" {
		move.DeviceID = &c.deviceID
	}
	if c.waste != nil {
		daily := new(big.Rat).Quo(c.waste, big.NewRat(30, 1))
		move.DailyWaste = &moneyValue{daily.FloatString(2), c.currency}
	}
	if c.basis == "recommendation" && c.action != nil {
		id, title := c.action.RuleID, c.action.Title
		move.ActionID, move.ActionTitle = &id, &title
	}
	return move
}

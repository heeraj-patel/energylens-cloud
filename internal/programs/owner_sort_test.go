package programs

import (
	"encoding/json"
	"testing"
)

func TestFixedOfferSavingUsesOnlyComparableFixedDollars(t *testing.T) {
	cases := []struct {
		name, kind, terms string
		want              bool
	}{
		{"fixed rebate", "rebate", `{"amount":"125","currency":"USD"}`, true},
		{"fixed discount", "discount", `{"amount":75,"period":"one_time"}`, true},
		{"up to", "rebate", `{"amount":"200","up_to":true}`, false},
		{"annual", "rebate", `{"amount":"50","period":"annual"}`, false},
		{"non dollar", "rebate", `{"amount":"100","currency":"CAD"}`, false},
		{"energy rate", "energy_savings", `{"amount":"100"}`, false},
		{"ambiguous text", "rebate", `{"amount":"up to 500"}`, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, got := fixedOfferSaving(OwnerProgram{BenefitKind: tc.kind, BenefitTerms: json.RawMessage(tc.terms)})
			if got != tc.want {
				t.Fatalf("comparable=%v, want %v", got, tc.want)
			}
		})
	}
}

package estimation

import "testing"

func TestAnnualFigureIsOperatingCostAcrossLeapYear(t *testing.T) {
	in := Input{
		Fact:     Fact{Kind: "annual_energy", Unit: "kWh", Value: "659", Reviewed: true, Source: "reviewed_exact"},
		Identity: "confirmed_exact", RatePerKWh: "0.20", RateKind: "regional_average",
		StartDate: "2024-01-01", EndDate: "2025-01-01", Timezone: "America/Chicago",
	}
	got, err := Calculate(in)
	if err != nil {
		t.Fatal(err)
	}
	if got.Kind != "estimated_operating_cost" || got.EnergyKWh != "659.000000" || got.Cost != "131.800000" || got.Waste != "not_yet_assessed" {
		t.Fatalf("unexpected annual estimate: %+v", got)
	}
	in.StartDate, in.EndDate = "2024-03-10", "2024-03-11" // DST shift is still one calendar day.
	got, err = Calculate(in)
	if err != nil {
		t.Fatal(err)
	}
	if got.EnergyKWh != "1.800546" {
		t.Fatalf("unexpected DST day estimate: %+v", got)
	}
}

func TestConfirmedCyclesAreUsefulAndUnknownIsUnavailable(t *testing.T) {
	in := Input{
		Fact:     Fact{Kind: "cycle_energy", Unit: "kWh/cycle", Value: "1.25", Reviewed: true, Source: "reviewed_family"},
		Identity: "documented_variant_family", RatePerKWh: "0.16", RateKind: "official_tariff",
		StartDate: "2026-09-01", EndDate: "2026-10-01", Timezone: "America/Chicago",
	}
	got, err := Calculate(in)
	if err != nil || got.Kind != "unavailable" || got.UnavailableReason != "usage_unknown" {
		t.Fatalf("unknown cycles must remain unavailable: %+v, %v", got, err)
	}
	zero := int64(0)
	in.Cycles = &zero
	got, err = Calculate(in)
	if err != nil || got.Kind != "estimated_useful_cost" || got.EnergyKWh != "0.000000" || got.Waste != "not_yet_assessed" {
		t.Fatalf("confirmed zero cycles must not become unknown or waste: %+v, %v", got, err)
	}
	cycles := int64(8)
	in.Cycles = &cycles
	got, err = Calculate(in)
	if err != nil || got.EnergyKWh != "10.000000" || got.Cost != "1.600000" {
		t.Fatalf("unexpected cycle estimate: %+v, %v", got, err)
	}
}

func TestConfirmedHoursRequirePerHourFact(t *testing.T) {
	in := Input{
		Fact:     Fact{Kind: "hour_energy", Unit: "kWh/hour", Value: "0.125", Reviewed: true, Source: "reviewed_exact"},
		Identity: "confirmed_exact", RatePerKWh: "0.20", RateKind: "user_tariff",
		StartDate: "2026-10-01", EndDate: "2026-11-01", Timezone: "America/Chicago",
	}
	got, err := Calculate(in)
	if err != nil || got.UnavailableReason != "usage_unknown" {
		t.Fatalf("unknown hours: %+v, %v", got, err)
	}
	zero := "0"
	in.Hours = &zero
	got, err = Calculate(in)
	if err != nil || got.Kind != "estimated_useful_cost" || got.EnergyKWh != "0.000000" {
		t.Fatalf("confirmed zero hours: %+v, %v", got, err)
	}
	hours := "12.5"
	in.Hours = &hours
	got, err = Calculate(in)
	if err != nil || got.EnergyKWh != "1.562500" || got.Cost != "0.312500" || got.Waste != "not_yet_assessed" {
		t.Fatalf("hour estimate: %+v, %v", got, err)
	}
	in.Fact.Unit = "kWh"
	if _, err = Calculate(in); err == nil {
		t.Fatal("accepted annual unit as a per-hour fact")
	}
}

func TestUnreviewedMissingRateAndInvalidInputs(t *testing.T) {
	in := Input{
		Fact:     Fact{Kind: "annual_energy", Unit: "kWh", Value: "100", Source: "reviewed_exact"},
		Identity: "confirmed_exact", RatePerKWh: "0.20", RateKind: "regional_average",
		StartDate: "2026-01-01", EndDate: "2027-01-01", Timezone: "America/Chicago",
	}
	got, err := Calculate(in)
	if err != nil || got.UnavailableReason != "source_unreviewed" {
		t.Fatalf("unreviewed: %+v, %v", got, err)
	}
	in.Fact.Reviewed = true
	in.RatePerKWh = ""
	got, err = Calculate(in)
	if err != nil || got.UnavailableReason != "rate_unavailable" {
		t.Fatalf("missing rate: %+v, %v", got, err)
	}
	in.RatePerKWh = "0.20"
	in.RatePerKWh = "0"
	got, err = Calculate(in)
	if err != nil || got.Kind != "estimated_operating_cost" || got.Cost != "0.000000" {
		t.Fatalf("confirmed zero rate must be distinct from unknown: %+v, %v", got, err)
	}
	in.RatePerKWh = "0.20"
	in.Fact.Value = "NaN"
	if _, err = Calculate(in); err == nil {
		t.Fatal("accepted nondecimal energy")
	}
	in.Fact.Value = "100"
	in.Fact.Unit = "kWh/cycle"
	if _, err = Calculate(in); err == nil {
		t.Fatal("accepted fact kind/unit mismatch")
	}
	in.Fact.Unit = "kWh"
	in.EndDate = in.StartDate
	if _, err = Calculate(in); err == nil {
		t.Fatal("accepted empty period")
	}
}

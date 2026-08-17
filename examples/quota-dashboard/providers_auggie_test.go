package main

import (
	"testing"
	"time"
)

func TestParseAuggieWindow(t *testing.T) {
	tests := []struct {
		name      string
		remaining string
		included  string
		wantUsed  float64
		wantTotal float64
		wantPct   float64
		wantErr   bool
	}{
		{name: "normal decimals", remaining: "25.5", included: "100.0", wantUsed: 74.5, wantTotal: 100, wantPct: 74.5},
		{name: "zero remaining", remaining: "0", included: "10", wantUsed: 10, wantTotal: 10, wantPct: 100},
		{name: "negative remaining", remaining: "-5", included: "10", wantUsed: 15, wantTotal: 10, wantPct: 100},
		{name: "remaining over included", remaining: "15", included: "10", wantUsed: -5, wantTotal: 10, wantPct: 0},
		{name: "invalid remaining", remaining: "not-a-number", included: "10", wantErr: true},
		{name: "non-finite remaining", remaining: "NaN", included: "10", wantErr: true},
		{name: "invalid included", remaining: "1", included: "not-a-number", wantErr: true},
		{name: "zero included", remaining: "0", included: "0", wantErr: true},
		{name: "negative included", remaining: "0", included: "-1", wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			window, err := parseAuggieWindow(auggieStatus{
				AmountRemaining:        tc.remaining,
				AmountIncludedPerCycle: tc.included,
			})
			if (err != nil) != tc.wantErr {
				t.Fatalf("parseAuggieWindow() error = %v, wantErr %v", err, tc.wantErr)
			}
			if tc.wantErr {
				return
			}
			if window.Used != tc.wantUsed || window.Total != tc.wantTotal || window.UsedPercent != tc.wantPct {
				t.Fatalf("parseAuggieWindow() = used %v, total %v, pct %v; want %v, %v, %v", window.Used, window.Total, window.UsedPercent, tc.wantUsed, tc.wantTotal, tc.wantPct)
			}
		})
	}
}

func TestParseAuggieWindowBillingDate(t *testing.T) {
	date := "2026-08-31T12:34:56Z"
	window, err := parseAuggieWindow(auggieStatus{
		AmountRemaining:        "1",
		AmountIncludedPerCycle: "2",
		BillingCycleEndDate:    date,
	})
	if err != nil {
		t.Fatalf("parseAuggieWindow() error = %v", err)
	}
	want, _ := time.Parse(time.RFC3339, date)
	if !window.ResetAt.Equal(want) {
		t.Fatalf("ResetAt = %v, want %v", window.ResetAt, want)
	}
}

func TestAuggiePlan(t *testing.T) {
	tests := []struct {
		plan, unit, want string
	}{
		{plan: "Pro", unit: "credits", want: "Pro · credits"},
		{plan: "Pro credits", unit: "credits", want: "Pro credits"},
		{plan: "Pro", unit: "", want: "Pro"},
		{plan: "", unit: "credits", want: "credits"},
	}
	for _, tc := range tests {
		if got := auggiePlan(tc.plan, tc.unit); got != tc.want {
			t.Errorf("auggiePlan(%q, %q) = %q, want %q", tc.plan, tc.unit, got, tc.want)
		}
	}
}

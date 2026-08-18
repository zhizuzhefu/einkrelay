package main

import (
	"encoding/json"
	"testing"
)

func TestGrokUsagePercent(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want float64
	}{
		{"omitted", "", 0},
		{"null", "null", 0},
		{"zero", "0", 0},
		{"number", "12.5", 12.5},
		{"wrapped", `{"val":7}`, 7},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var raw json.RawMessage
			if tc.raw != "" {
				raw = json.RawMessage(tc.raw)
			}
			if got := grokUsagePercent(raw); got != tc.want {
				t.Fatalf("grokUsagePercent(%s)=%v want %v", tc.raw, got, tc.want)
			}
		})
	}
}

func TestPrettyGrokTier(t *testing.T) {
	cases := map[string]string{
		"":                                 "",
		"SUBSCRIPTION_TIER_X_PREMIUM_PLUS": "X Premium+",
		"X_PREMIUM_PLUS":                   "X Premium+",
		"SUPERGROK":                        "SuperGrok",
		"SUPERGROK_HEAVY":                  "SuperGrok Heavy",
		"custom":                           "custom",
	}
	for in, want := range cases {
		if got := prettyGrokTier(in); got != want {
			t.Fatalf("prettyGrokTier(%q)=%q want %q", in, got, want)
		}
	}
}

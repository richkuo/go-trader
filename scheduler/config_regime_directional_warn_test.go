package main

import (
	"strings"
	"testing"
)

// #1076: regimeDirectionalPolicyWarnings must emit exactly one advisory per strategy that
// configures the regime→direction selection surface, and stay silent otherwise. The warning
// is the non-breaking guard chosen after the premise was empirically refuted; hard-rejecting
// the keys was rejected as less safe (a forced disable can strand a shared-coin live short).
func TestRegimeDirectionalPolicyWarnings(t *testing.T) {
	cfg := &Config{Strategies: []StrategyConfig{
		{ID: "plain-long"}, // no policy → no warning
		{ID: "dir-policy", RegimeDirectionalPolicy: &RegimeDirectionalPolicy{
			TrendRegime: map[string]RegimeDirectionalEntry{
				"trending_up":   {Direction: "long"},
				"trending_down": {Direction: "short"},
				"ranging":       {Direction: "long"},
			},
		}},
		{ID: "allowed-only", AllowedRegimes: []string{"trending_up"}}, // entry-gating, not direction
	}}

	warns := regimeDirectionalPolicyWarnings(cfg)
	if len(warns) != 1 {
		t.Fatalf("want exactly 1 warning (only the regime_directional_policy strategy), got %d: %v",
			len(warns), warns)
	}
	w := warns[0]
	if !strings.Contains(w, "dir-policy") {
		t.Errorf("warning must name the offending strategy id; got %q", w)
	}
	if !strings.Contains(w, "#1076") {
		t.Errorf("warning must cite the #1076 evidence; got %q", w)
	}
	if !strings.HasPrefix(w, "[WARN]") {
		t.Errorf("warning must use the [WARN] operator prefix; got %q", w)
	}
}

// IsConfigured keys off TrendRegime/raw, so a strategy with the policy present but no other
// trigger still warns — and a nil config must not panic.
func TestRegimeDirectionalPolicyWarningsEdgeCases(t *testing.T) {
	if got := regimeDirectionalPolicyWarnings(nil); got != nil {
		t.Errorf("nil config must yield no warnings, got %v", got)
	}
	empty := &Config{Strategies: []StrategyConfig{{ID: "a"}, {ID: "b"}}}
	if got := regimeDirectionalPolicyWarnings(empty); len(got) != 0 {
		t.Errorf("strategies without the policy must yield no warnings, got %v", got)
	}
	// Policy present via TrendRegime → IsConfigured true → one warning.
	one := &Config{Strategies: []StrategyConfig{{ID: "x", RegimeDirectionalPolicy: &RegimeDirectionalPolicy{
		TrendRegime: map[string]RegimeDirectionalEntry{"trending_up": {Direction: "long"}},
	}}}}
	if got := regimeDirectionalPolicyWarnings(one); len(got) != 1 {
		t.Fatalf("configured policy must yield 1 warning, got %d", len(got))
	}
}

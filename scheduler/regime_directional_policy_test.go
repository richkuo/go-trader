package main

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestRegimeDirectionalPolicyResolveRaw covers parsing + validation:
// canonical labels required, valid direction enum, wrapper key enforced,
// unknown keys rejected.
func TestRegimeDirectionalPolicyResolveRaw(t *testing.T) {
	t.Run("accepts canonical shape", func(t *testing.T) {
		raw := `{"trend_regime": {
			"trending_up":   {"direction": "long",  "invert_signal": false},
			"trending_down": {"direction": "short", "invert_signal": true},
			"ranging":       {"direction": "long",  "invert_signal": false}
		}}`
		var p RegimeDirectionalPolicy
		if err := json.Unmarshal([]byte(raw), &p); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		errs := p.ResolveRaw("strategy[test].regime_directional_policy")
		if len(errs) != 0 {
			t.Fatalf("expected no errors, got: %v", errs)
		}
		if len(p.TrendRegime) != 3 {
			t.Fatalf("expected 3 entries, got %d", len(p.TrendRegime))
		}
		entry := p.TrendRegime["trending_down"]
		if entry.Direction != "short" || !entry.InvertSignal {
			t.Fatalf("trending_down: got %+v", entry)
		}
	})

	t.Run("rejects missing wrapper key", func(t *testing.T) {
		raw := `{"trending_up": {"direction": "long"}}`
		var p RegimeDirectionalPolicy
		_ = json.Unmarshal([]byte(raw), &p)
		errs := p.ResolveRaw("x")
		if len(errs) == 0 || !strings.Contains(errs[0], "trend_regime") {
			t.Fatalf("expected wrapper-key error, got: %v", errs)
		}
	})

	t.Run("rejects missing canonical label", func(t *testing.T) {
		raw := `{"trend_regime": {
			"trending_up":   {"direction": "long"},
			"trending_down": {"direction": "short", "invert_signal": true}
		}}`
		var p RegimeDirectionalPolicy
		_ = json.Unmarshal([]byte(raw), &p)
		errs := p.ResolveRaw("x")
		found := false
		for _, e := range errs {
			if strings.Contains(e, "missing required regime labels") && strings.Contains(e, "ranging") {
				found = true
			}
		}
		if !found {
			t.Fatalf("expected missing-labels error mentioning ranging, got: %v", errs)
		}
	})

	t.Run("rejects unknown regime label", func(t *testing.T) {
		raw := `{"trend_regime": {
			"trending_up":   {"direction": "long"},
			"trending_down": {"direction": "short"},
			"ranging":       {"direction": "long"},
			"trending_sideways": {"direction": "long"}
		}}`
		var p RegimeDirectionalPolicy
		_ = json.Unmarshal([]byte(raw), &p)
		errs := p.ResolveRaw("x")
		found := false
		for _, e := range errs {
			if strings.Contains(e, "unknown regime label") && strings.Contains(e, "trending_sideways") {
				found = true
			}
		}
		if !found {
			t.Fatalf("expected unknown-label error, got: %v", errs)
		}
	})

	t.Run("rejects invalid direction value", func(t *testing.T) {
		raw := `{"trend_regime": {
			"trending_up":   {"direction": "sideways"},
			"trending_down": {"direction": "short"},
			"ranging":       {"direction": "long"}
		}}`
		var p RegimeDirectionalPolicy
		_ = json.Unmarshal([]byte(raw), &p)
		errs := p.ResolveRaw("x")
		found := false
		for _, e := range errs {
			if strings.Contains(e, "direction") && strings.Contains(e, "sideways") {
				found = true
			}
		}
		if !found {
			t.Fatalf("expected invalid-direction error, got: %v", errs)
		}
		// A present-but-invalid label must NOT also surface as
		// "missing required regime labels: trending_up" — the operator
		// should see one error per typo, not two.
		for _, e := range errs {
			if strings.Contains(e, "missing required regime labels") {
				t.Fatalf("invalid-direction must not double-report as missing: %v", errs)
			}
		}
	})

	t.Run("rejects unknown entry key", func(t *testing.T) {
		raw := `{"trend_regime": {
			"trending_up":   {"direction": "long", "size_mult": 2.0},
			"trending_down": {"direction": "short"},
			"ranging":       {"direction": "long"}
		}}`
		var p RegimeDirectionalPolicy
		_ = json.Unmarshal([]byte(raw), &p)
		errs := p.ResolveRaw("x")
		found := false
		for _, e := range errs {
			if strings.Contains(e, "unknown key") && strings.Contains(e, "size_mult") {
				found = true
			}
		}
		if !found {
			t.Fatalf("expected unknown-key error, got: %v", errs)
		}
	})

	t.Run("invert_signal defaults to false when omitted", func(t *testing.T) {
		raw := `{"trend_regime": {
			"trending_up":   {"direction": "long"},
			"trending_down": {"direction": "short"},
			"ranging":       {"direction": "long"}
		}}`
		var p RegimeDirectionalPolicy
		_ = json.Unmarshal([]byte(raw), &p)
		errs := p.ResolveRaw("x")
		if len(errs) != 0 {
			t.Fatalf("unexpected errs: %v", errs)
		}
		if p.TrendRegime["trending_down"].InvertSignal {
			t.Fatalf("expected invert default false")
		}
	})
}

func TestRegimeDirectionalPolicyEqualForReload(t *testing.T) {
	makePolicy := func(downDir string, downInvert bool) *RegimeDirectionalPolicy {
		return &RegimeDirectionalPolicy{TrendRegime: map[string]RegimeDirectionalEntry{
			"trending_up":   {Direction: "long", InvertSignal: false},
			"trending_down": {Direction: downDir, InvertSignal: downInvert},
			"ranging":       {Direction: "long", InvertSignal: false},
		}}
	}
	a := makePolicy("short", true)
	b := makePolicy("short", true)
	if !a.EqualForReload(b) {
		t.Fatalf("identical policies should be equal")
	}
	c := makePolicy("short", false)
	if a.EqualForReload(c) {
		t.Fatalf("invert change should not be equal")
	}
	d := makePolicy("long", true)
	if a.EqualForReload(d) {
		t.Fatalf("direction change should not be equal")
	}
	var nilP *RegimeDirectionalPolicy
	if !nilP.EqualForReload(nil) {
		t.Fatalf("nil/nil should be equal")
	}
	if a.EqualForReload(nil) {
		t.Fatalf("nil and configured should not be equal")
	}
}

// TestApplyRegimeDirectionalPolicy covers the resolver semantics:
// flat -> current regime, open -> pos.Regime (hold semantics),
// no policy -> no-op.
func TestApplyRegimeDirectionalPolicy(t *testing.T) {
	makePolicy := func() *RegimeDirectionalPolicy {
		return &RegimeDirectionalPolicy{TrendRegime: map[string]RegimeDirectionalEntry{
			"trending_up":   {Direction: "long", InvertSignal: false},
			"trending_down": {Direction: "short", InvertSignal: true},
			"ranging":       {Direction: "long", InvertSignal: false},
		}}
	}

	t.Run("flat uses current regime", func(t *testing.T) {
		sc := StrategyConfig{Direction: "long", InvertSignal: false, RegimeDirectionalPolicy: makePolicy()}
		entry, applied, legacy := applyRegimeDirectionalPolicy(&sc, "trending_down", "", 0)
		if !applied {
			t.Fatalf("expected applied")
		}
		if legacy {
			t.Fatalf("flat state should not flag legacy fallback")
		}
		if entry.Direction != "short" || !entry.InvertSignal {
			t.Fatalf("bad entry: %+v", entry)
		}
		if sc.Direction != "short" || !sc.InvertSignal {
			t.Fatalf("sc not mutated: dir=%q invert=%t", sc.Direction, sc.InvertSignal)
		}
	})

	t.Run("open position uses pos.Regime (hold)", func(t *testing.T) {
		sc := StrategyConfig{Direction: "long", InvertSignal: false, RegimeDirectionalPolicy: makePolicy()}
		// pos opened under trending_down, current regime flipped to trending_up
		entry, applied, legacy := applyRegimeDirectionalPolicy(&sc, "trending_up", "trending_down", 0.001)
		if !applied {
			t.Fatalf("expected applied")
		}
		if legacy {
			t.Fatalf("posRegime set; should not flag legacy fallback")
		}
		// Should resolve from pos.Regime -> short policy continues
		if entry.Direction != "short" || !entry.InvertSignal {
			t.Fatalf("expected hold under prior policy, got: %+v", entry)
		}
		if sc.Direction != "short" {
			t.Fatalf("sc.Direction not held: %q", sc.Direction)
		}
	})

	t.Run("flat after pos closed picks new regime", func(t *testing.T) {
		sc := StrategyConfig{Direction: "long", InvertSignal: false, RegimeDirectionalPolicy: makePolicy()}
		// Position closed (qty=0); current regime is trending_up
		entry, applied, legacy := applyRegimeDirectionalPolicy(&sc, "trending_up", "trending_down", 0)
		if !applied {
			t.Fatalf("expected applied")
		}
		if legacy {
			t.Fatalf("flat state should not flag legacy fallback")
		}
		if entry.Direction != "long" || entry.InvertSignal {
			t.Fatalf("expected long+no-invert after flat under trending_up, got: %+v", entry)
		}
	})

	t.Run("no policy is no-op", func(t *testing.T) {
		sc := StrategyConfig{Direction: "long", InvertSignal: false}
		_, applied, _ := applyRegimeDirectionalPolicy(&sc, "trending_down", "", 0)
		if applied {
			t.Fatalf("expected no-op when policy nil")
		}
		if sc.Direction != "long" || sc.InvertSignal {
			t.Fatalf("sc mutated unexpectedly: dir=%q invert=%t", sc.Direction, sc.InvertSignal)
		}
	})

	t.Run("unknown regime is no-op", func(t *testing.T) {
		sc := StrategyConfig{Direction: "long", InvertSignal: false, RegimeDirectionalPolicy: makePolicy()}
		_, applied, _ := applyRegimeDirectionalPolicy(&sc, "", "", 0)
		if applied {
			t.Fatalf("empty regime should not resolve")
		}
		if sc.Direction != "long" {
			t.Fatalf("sc mutated under empty regime: %q", sc.Direction)
		}
	})

	t.Run("legacy position without pos.Regime falls back to current", func(t *testing.T) {
		sc := StrategyConfig{Direction: "long", InvertSignal: false, RegimeDirectionalPolicy: makePolicy()}
		entry, applied, legacy := applyRegimeDirectionalPolicy(&sc, "trending_up", "", 0.5)
		if !applied {
			t.Fatalf("expected applied")
		}
		if !legacy {
			t.Fatalf("open position with empty posRegime should flag legacy fallback")
		}
		if entry.Direction != "long" {
			t.Fatalf("expected fallback to current trending_up -> long, got: %+v", entry)
		}
	})
}

// TestValidateConfigRegimeDirectionalPolicy covers config-level validation:
// HL perps only, requires regime.enabled, valid shape.
func TestValidateConfigRegimeDirectionalPolicy(t *testing.T) {
	makePolicyJSON := `{"trend_regime": {
		"trending_up":   {"direction": "long",  "invert_signal": false},
		"trending_down": {"direction": "short", "invert_signal": true},
		"ranging":       {"direction": "long",  "invert_signal": false}
	}}`
	unmarshal := func() *RegimeDirectionalPolicy {
		var p RegimeDirectionalPolicy
		if err := json.Unmarshal([]byte(makePolicyJSON), &p); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		return &p
	}

	t.Run("accepts HL perps with regime enabled", func(t *testing.T) {
		cfg := Config{
			Strategies: []StrategyConfig{{
				ID:                      "hl-test-btc",
				Type:                    "perps",
				Platform:                "hyperliquid",
				Script:                  "shared_scripts/check_hyperliquid.py",
				Args:                    []string{"sma_crossover", "BTC", "1h", "--mode=paper"},
				Capital:                 1000,
				Leverage:                5,
				MaxDrawdownPct:          60,
				Direction:               "long",
				RegimeDirectionalPolicy: unmarshal(),
			}},
			Regime:        &RegimeConfig{Enabled: true, Period: 14, ADXThreshold: 20},
			PortfolioRisk: &PortfolioRiskConfig{MaxDrawdownPct: 25, WarnThresholdPct: 80},
		}
		if err := ValidateConfig(&cfg); err != nil {
			t.Fatalf("expected no error, got: %v", err)
		}
	})

	t.Run("rejects when regime detection disabled", func(t *testing.T) {
		cfg := Config{
			Strategies: []StrategyConfig{{
				ID:                      "hl-test-btc",
				Type:                    "perps",
				Platform:                "hyperliquid",
				Script:                  "shared_scripts/check_hyperliquid.py",
				Args:                    []string{"sma_crossover", "BTC", "1h", "--mode=paper"},
				Capital:                 1000,
				Leverage:                5,
				MaxDrawdownPct:          60,
				Direction:               "long",
				RegimeDirectionalPolicy: unmarshal(),
			}},
			Regime:        &RegimeConfig{Enabled: false},
			PortfolioRisk: &PortfolioRiskConfig{MaxDrawdownPct: 25, WarnThresholdPct: 80},
		}
		err := ValidateConfig(&cfg)
		if err == nil || !strings.Contains(err.Error(), "regime_directional_policy requires top-level regime.enabled=true") {
			t.Fatalf("expected regime-enabled error, got: %v", err)
		}
	})

	t.Run("rejects on non-HL platform", func(t *testing.T) {
		cfg := Config{
			Strategies: []StrategyConfig{{
				ID:                      "okx-test-btc",
				Type:                    "perps",
				Platform:                "okx",
				Script:                  "shared_scripts/check_okx.py",
				Args:                    []string{"sma_crossover", "BTC-USDT-SWAP", "1h"},
				Capital:                 1000,
				Leverage:                5,
				MaxDrawdownPct:          60,
				Direction:               "long",
				RegimeDirectionalPolicy: unmarshal(),
			}},
			Regime:        &RegimeConfig{Enabled: true, Period: 14, ADXThreshold: 20},
			PortfolioRisk: &PortfolioRiskConfig{MaxDrawdownPct: 25, WarnThresholdPct: 80},
		}
		err := ValidateConfig(&cfg)
		if err == nil || !strings.Contains(err.Error(), "regime_directional_policy is only supported for HL perps") {
			t.Fatalf("expected HL-perps-only error, got: %v", err)
		}
	})

	t.Run("hot reload blocks shape change while open", func(t *testing.T) {
		old := StrategyConfig{
			ID:       "hl-test-btc",
			Type:     "perps",
			Platform: "hyperliquid",
			RegimeDirectionalPolicy: &RegimeDirectionalPolicy{TrendRegime: map[string]RegimeDirectionalEntry{
				"trending_up":   {Direction: "long", InvertSignal: false},
				"trending_down": {Direction: "short", InvertSignal: true},
				"ranging":       {Direction: "long", InvertSignal: false},
			}},
		}
		ns := old
		ns.RegimeDirectionalPolicy = &RegimeDirectionalPolicy{TrendRegime: map[string]RegimeDirectionalEntry{
			"trending_up":   {Direction: "long", InvertSignal: false},
			"trending_down": {Direction: "both", InvertSignal: false}, // shape change
			"ranging":       {Direction: "long", InvertSignal: false},
		}}
		openState := &AppState{
			Strategies: map[string]*StrategyState{
				"hl-test-btc": {ID: "hl-test-btc", Positions: map[string]*Position{
					"BTC": {Symbol: "BTC", Quantity: 0.001, AvgCost: 60000, Side: "short", Regime: "trending_down"},
				}},
			},
		}
		flatState := &AppState{
			Strategies: map[string]*StrategyState{
				"hl-test-btc": {ID: "hl-test-btc", Positions: map[string]*Position{}},
			},
		}
		err := validateHotReloadStateCompatible(&Config{Strategies: []StrategyConfig{old}}, &Config{Strategies: []StrategyConfig{ns}}, openState)
		if err == nil || !strings.Contains(err.Error(), "regime_directional_policy shape changed with open positions") {
			t.Fatalf("expected shape-change rejection while open, got: %v", err)
		}
		if err := validateHotReloadStateCompatible(&Config{Strategies: []StrategyConfig{old}}, &Config{Strategies: []StrategyConfig{ns}}, flatState); err != nil {
			t.Fatalf("flat hot reload should be accepted, got: %v", err)
		}
	})

	t.Run("hot reload blocks add/remove while open", func(t *testing.T) {
		old := StrategyConfig{
			ID:       "hl-test-btc",
			Type:     "perps",
			Platform: "hyperliquid",
		}
		ns := old
		ns.RegimeDirectionalPolicy = &RegimeDirectionalPolicy{TrendRegime: map[string]RegimeDirectionalEntry{
			"trending_up":   {Direction: "long", InvertSignal: false},
			"trending_down": {Direction: "short", InvertSignal: true},
			"ranging":       {Direction: "long", InvertSignal: false},
		}}
		openState := &AppState{
			Strategies: map[string]*StrategyState{
				"hl-test-btc": {ID: "hl-test-btc", Positions: map[string]*Position{
					"BTC": {Symbol: "BTC", Quantity: 0.001, AvgCost: 60000, Side: "short", Regime: "trending_down"},
				}},
			},
		}
		err := validateHotReloadStateCompatible(&Config{Strategies: []StrategyConfig{old}}, &Config{Strategies: []StrategyConfig{ns}}, openState)
		if err == nil || !strings.Contains(err.Error(), "regime_directional_policy mode changed with open positions") {
			t.Fatalf("expected mode-change rejection while open, got: %v", err)
		}
	})

	t.Run("rejects on HL manual type", func(t *testing.T) {
		cfg := Config{
			Strategies: []StrategyConfig{{
				ID:                      "hl-manual-btc",
				Type:                    "manual",
				Platform:                "hyperliquid",
				Symbol:                  "BTC",
				Timeframe:               "1h",
				Leverage:                5,
				Capital:                 1000,
				MaxDrawdownPct:          60,
				Direction:               "long",
				RegimeDirectionalPolicy: unmarshal(),
			}},
			Regime:        &RegimeConfig{Enabled: true, Period: 14, ADXThreshold: 20},
			PortfolioRisk: &PortfolioRiskConfig{MaxDrawdownPct: 25, WarnThresholdPct: 80},
		}
		err := ValidateConfig(&cfg)
		if err == nil || !strings.Contains(err.Error(), "regime_directional_policy is only supported for HL perps") {
			t.Fatalf("expected HL-perps-only error, got: %v", err)
		}
	})
}

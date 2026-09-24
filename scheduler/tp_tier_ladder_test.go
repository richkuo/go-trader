package main

import (
	"strings"
	"testing"
)

func tpLadder(pairs ...[2]float64) []interface{} {
	out := make([]interface{}, len(pairs))
	for i, p := range pairs {
		out[i] = map[string]interface{}{"atr_multiple": p[0], "close_fraction": p[1]}
	}
	return out
}

func TestValidateTPTierLadders(t *testing.T) {
	increasing := tpLadder([2]float64{1, 0.3}, [2]float64{2, 0.6}, [2]float64{3, 0.8})
	equal := tpLadder([2]float64{1, 0.5}, [2]float64{2, 0.5}, [2]float64{3, 1})
	decreasing := tpLadder([2]float64{1, 0.6}, [2]float64{2, 0.4}, [2]float64{3, 1})
	tierKeyed := func(rangingFrac float64) []interface{} {
		return []interface{}{
			map[string]interface{}{"trend_regime": map[string]interface{}{
				"trending_up":   map[string]interface{}{"atr_multiple": 1.0, "close_fraction": 0.4},
				"trending_down": map[string]interface{}{"atr_multiple": 1.0, "close_fraction": 0.4},
				"ranging":       map[string]interface{}{"atr_multiple": 0.5, "close_fraction": 0.6},
			}},
			map[string]interface{}{"trend_regime": map[string]interface{}{
				"trending_up":   map[string]interface{}{"atr_multiple": 2.0, "close_fraction": 1.0},
				"trending_down": map[string]interface{}{"atr_multiple": 2.0, "close_fraction": 1.0},
				"ranging":       map[string]interface{}{"atr_multiple": 1.0, "close_fraction": rangingFrac},
			}},
		}
	}
	unified := func(rangingTiers []interface{}) map[string]interface{} {
		block := unifiedBlock()
		block[regimeClassifierKey].(map[string]interface{})["ranging"].(map[string]interface{})["tp_tiers"] = rangingTiers
		return block
	}
	cases := []struct {
		name     string
		platform string
		typ      string
		close    *StrategyRef
		wantErr  string
	}{
		{"strategy tiered_tp_atr increasing", "hyperliquid", "perps", &StrategyRef{Name: "tiered_tp_atr", Params: map[string]interface{}{"tp_tiers": increasing}}, ""},
		{"strategy tiered_tp_atr_live equal fractions", "hyperliquid", "perps", &StrategyRef{Name: "tiered_tp_atr_live", Params: map[string]interface{}{"tp_tiers": equal}}, "strategy[s1].close_strategy(tiered_tp_atr_live).tp_tiers: tier 1 close_fraction 0.5 must be greater than tier 0"},
		{"strategy tiered_tp_atr decreasing", "hyperliquid", "perps", &StrategyRef{Name: "tiered_tp_atr", Params: map[string]interface{}{"tp_tiers": decreasing}}, "tp_tiers: tier 1 close_fraction 0.4"},
		{"single tier stays legal", "hyperliquid", "perps", &StrategyRef{Name: "tiered_tp_atr_live", Params: map[string]interface{}{"tp_tiers": tpLadder([2]float64{2, 0.5})}}, ""},
		{"non-positive multiple", "hyperliquid", "perps", &StrategyRef{Name: "tiered_tp_atr_live", Params: map[string]interface{}{"tp_tiers": tpLadder([2]float64{0, 0.2}, [2]float64{1, 1})}}, "tp_tiers[0].atr_multiple: must be > 0"},
		{"tier-keyed regime label decreasing", "hyperliquid", "perps", &StrategyRef{Name: "tiered_tp_atr_regime", Params: map[string]interface{}{"tp_tiers": tierKeyed(0.5)}}, "tp_tiers[regime ranging]: tier 1 close_fraction 0.5"},
		{"tier-keyed regime increasing", "hyperliquid", "perps", &StrategyRef{Name: "tiered_tp_atr_regime", Params: map[string]interface{}{"tp_tiers": tierKeyed(1.0)}}, ""},
		{"unified dynamic label decreasing", "hyperliquid", "perps", &StrategyRef{Name: dynamicCloseStrategyName, Params: unified(decreasing)}, "trend_regime.ranging.tp_tiers: tier 1 close_fraction 0.4"},
		{"manual default ladder equal fractions", "hyperliquid", "manual", &StrategyRef{Name: "tiered_tp_atr_live", Params: map[string]interface{}{"tp_tiers": equal}}, "strategy[s1].close_strategy(tiered_tp_atr_live).tp_tiers: tier 1"},
		{"other platform keeps its ladder", "okx", "perps", &StrategyRef{Name: "tiered_tp_atr_live", Params: map[string]interface{}{"tp_tiers": equal}}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &Config{
				Regime:     &RegimeConfig{Enabled: true},
				Strategies: []StrategyConfig{{ID: "s1", Platform: tc.platform, Type: tc.typ, CloseStrategy: tc.close}},
			}
			errs := validateTPTierLadders(cfg)
			if tc.wantErr == "" {
				if len(errs) != 0 {
					t.Fatalf("errors = %v, want none", errs)
				}
				return
			}
			if len(errs) == 0 || !strings.Contains(strings.Join(errs, "\n"), tc.wantErr) {
				t.Fatalf("errors = %v, want one containing %q", errs, tc.wantErr)
			}
		})
	}
}

func TestLoadConfigRejectsANonIncreasingHyperliquidTPLadder(t *testing.T) {
	strategy := func(closeRef string) string {
		return `{
			"id": "hl-tp-eth",
			"type": "perps",
			"platform": "hyperliquid",
			"script": "shared_scripts/check_hyperliquid.py",
			"args": ["sma_crossover", "ETH", "1h", "--mode=paper"],
			"capital": 1000,
			"leverage": 5,
			"close_strategy": ` + closeRef + `
		}`
	}
	cases := []struct {
		name    string
		cfg     string
		wantErr string
	}{
		{"strategy ladder with equal fractions", `{"strategies": [` + strategy(`{"name": "tiered_tp_atr_live", "params": {"tp_tiers": [{"atr_multiple": 1, "close_fraction": 0.5}, {"atr_multiple": 2, "close_fraction": 0.5}]}}`) + `]}`,
			"strategy[hl-tp-eth].close_strategy(tiered_tp_atr_live).tp_tiers: tier 1 close_fraction 0.5 must be greater than tier 0"},
		{"strategy ladder fixed", `{"strategies": [` + strategy(`{"name": "tiered_tp_atr_live", "params": {"tp_tiers": [{"atr_multiple": 1, "close_fraction": 0.5}, {"atr_multiple": 2, "close_fraction": 0.8}]}}`) + `]}`, ""},
		{"user default ladder injected into the strategy", `{"user_defaults": {"close": {"tiered_tp_atr_live": {"tp_tiers": [{"atr_multiple": 1, "close_fraction": 0.7}, {"atr_multiple": 2, "close_fraction": 0.3}]}}}, "strategies": [` + strategy(`{"name": "tiered_tp_atr_live"}`) + `]}`,
			"strategy[hl-tp-eth].close_strategy(tiered_tp_atr_live).tp_tiers: tier 1 close_fraction 0.3"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := writeTestConfig(t, t.TempDir(), tc.cfg)
			_, err := LoadConfigReadOnly(path)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("LoadConfigReadOnly: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("LoadConfigReadOnly error = %v, want one containing %q", err, tc.wantErr)
			}
		})
	}
}

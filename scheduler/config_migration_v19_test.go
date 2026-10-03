package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func v19ParseRawConfig(t *testing.T, raw string) map[string]interface{} {
	t.Helper()
	var out map[string]interface{}
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		t.Fatalf("parse raw config: %v", err)
	}
	return out
}

func v19StrategyBlock(t *testing.T, raw map[string]interface{}) map[string]interface{} {
	t.Helper()
	strats, ok := raw["strategies"].([]interface{})
	if !ok || len(strats) == 0 {
		t.Fatalf("expected strategies[] in migrated config")
	}
	sc, ok := strats[0].(map[string]interface{})
	if !ok {
		t.Fatalf("strategies[0] is not an object")
	}
	return sc
}

func TestLoadConfigV19MigratesUserDefaultKeys(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	raw := `{
		"config_version": 18,
		"regime": {"enabled": true, "period": 14, "adx_threshold": 20},
		"strategies": [{
			"id": "hl-eth-v19-user-defaults",
			"type": "perps",
			"platform": "hyperliquid",
			"script": "shared_scripts/check_hyperliquid.py",
			"args": ["sma_crossover", "ETH", "1h", "--mode=paper"],
			"capital": 1000,
			"leverage": 3
		}],
		"user_defaults": {
			"regime_atr": {
				"stop_loss_atr_regime": {
					"trend_regime": {
						"trending_up": {"atr_multiple": 1.5},
						"ranging": {"atr_multiple": 0.5}
					}
				},
				"trail_stop_atr_regime": {
					"trend_regime": {
						"trending_up": {"atr_multiple": 2.25},
						"ranging": {"atr_multiple": 1.25}
					}
				}
			},
			"manual": {
				"trail_stop_atr_regime": {
					"trend_regime": {
						"trending_up": {"atr_multiple": 1.0},
						"ranging": {"atr_multiple": 0.5}
					}
				}
			},
			"close": {
				"trailing_tp_ratchet_regime": {
					"tp_tiers": {
						"ranging": [
							{"atr_multiple": 1.0, "close_fraction": 0.0, "trailing_mult_after": 1.0},
							{"atr_multiple": 2.0, "close_fraction": 0.0, "trailing_mult_after": 0.75}
						]
					},
					"trail_stop_atr_regime": {
						"trend_regime": {
							"trending_up": {"atr_multiple": 2.25},
							"trending_down": {"atr_multiple": 2.25},
							"ranging": {"atr_multiple": 1.25}
						}
					}
				}
			}
		}
	}`
	if err := os.WriteFile(path, []byte(raw), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	cfg, err := LoadConfigForProbe(path)
	if err != nil {
		t.Fatalf("LoadConfigForProbe should migrate user_defaults to v19 canonical, got error: %v", err)
	}
	if cfg.UserDefaults == nil {
		t.Fatalf("expected UserDefaults to be populated")
	}
	if cfg.UserDefaults.RegimeATR == nil {
		t.Fatalf("expected UserDefaults.RegimeATR")
	}
	if _, present := cfg.UserDefaults.RegimeATR["stop_loss_atr_mult_regime"]; !present {
		t.Fatalf("expected UserDefaults.RegimeATR.stop_loss_atr_mult_regime after v19 migration; got keys %v",
			cfg.UserDefaults.RegimeATR)
	}
	if _, present := cfg.UserDefaults.RegimeATR["trailing_stop_atr_mult_regime"]; !present {
		t.Fatalf("expected UserDefaults.RegimeATR.trailing_stop_atr_mult_regime after v19 migration")
	}
	if cfg.UserDefaults.Manual == nil || cfg.UserDefaults.Manual.TrailingStopATRMultRegime == nil {
		t.Fatalf("expected UserDefaults.Manual.TrailingStopATRMultRegime after v19 migration")
	}
	if cfg.UserDefaults.Close == nil {
		t.Fatalf("expected UserDefaults.Close")
	}
	ratchet := cfg.UserDefaults.Close["trailing_tp_ratchet_regime"]
	if ratchet == nil {
		t.Fatalf("expected trailing_tp_ratchet_regime close default")
	}
	ratchetRaw, ok := ratchet["trailing_stop_atr_mult_regime"].(map[string]interface{})
	if !ok {
		t.Fatalf("expected ratchet trailing_stop_atr_mult_regime to be a map; got %T", ratchet["trailing_stop_atr_mult_regime"])
	}
	ratchetBlock, ratchetErrs := parseRegimeATRBlock(ratchetRaw,
		"user_defaults.close.trailing_tp_ratchet_regime.trailing_stop_atr_mult_regime",
		regimeSurfaceTrailing, nil)
	if len(ratchetErrs) > 0 {
		t.Fatalf("parse ratchet trailing block: %v", ratchetErrs)
	}
	if ratchetBlock.TrendRegime["ranging"].ATR != 1.25 {
		t.Fatalf("ratchet ranging ATR = %v, want 1.25",
			ratchetBlock.TrendRegime["ranging"].ATR)
	}
}

func TestMigrateV19AtrMultRegimeKey(t *testing.T) {
	cases := []struct {
		name    string
		raw     string
		wantErr []string
		check   func(t *testing.T, sc map[string]interface{})
	}{
		{
			name: "conflicting_blocks_rejected_naming_legacy_key",
			raw: `{
				"strategies": [{
					"id": "hl-eth-conflict",
					"type": "perps",
					"platform": "hyperliquid",
					"trail_stop_atr_regime": {
						"trend_regime": {"ranging": {"atr_multiple": 1.0}}
					},
					"trailing_stop_atr_mult_regime": {
						"trend_regime": {"ranging": {"atr_multiple": 2.0}}
					}
				}]
			}`,
			wantErr: []string{"conflicts", `"trail_stop_atr_regime"`},
		},
		{
			name: "identical_blocks_drop_redundant_legacy_key",
			raw: `{
				"strategies": [{
					"id": "hl-eth-redundant",
					"type": "perps",
					"platform": "hyperliquid",
					"trail_stop_atr_regime": {
						"trend_regime": {"ranging": {"atr_multiple": 2.0}}
					},
					"trailing_stop_atr_mult_regime": {
						"trend_regime": {"ranging": {"atr_multiple": 2.0}}
					}
				}]
			}`,
			check: func(t *testing.T, sc map[string]interface{}) {
				if _, present := sc["trail_stop_atr_regime"]; present {
					t.Fatalf("legacy key should be dropped after identical-block merge")
				}
				canon, present := sc["trailing_stop_atr_mult_regime"]
				if !present {
					t.Fatalf("canonical key should remain")
				}
				canonMap, ok := canon.(map[string]interface{})
				if !ok {
					t.Fatalf("canonical block should be an object, got %T", canon)
				}
				trend, ok := canonMap["trend_regime"].(map[string]interface{})["ranging"].(map[string]interface{})["atr_multiple"]
				if !ok || trend != 2.0 {
					t.Fatalf("canonical atr_multiple = %v, want 2.0", trend)
				}
			},
		},
		{
			name: "rename_leaves_trailing_stop_pct_untouched",
			raw: `{
				"strategies": [{
					"id": "hl-eth-pct",
					"type": "perps",
					"platform": "hyperliquid",
					"trailing_stop_pct": 1.5,
					"trail_stop_atr_regime": {
						"trend_regime": {"ranging": {"atr_multiple": 1.0}}
					}
				}]
			}`,
			check: func(t *testing.T, sc map[string]interface{}) {
				if pct, ok := sc["trailing_stop_pct"].(float64); !ok || pct != 1.5 {
					t.Fatalf("trailing_stop_pct should be untouched, got %v", sc["trailing_stop_pct"])
				}
				if _, present := sc["trail_stop_atr_regime"]; present {
					t.Fatalf("legacy key should be renamed away")
				}
				if _, present := sc["trailing_stop_atr_mult_regime"]; !present {
					t.Fatalf("canonical key missing after rename")
				}
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			data := v19ParseRawConfig(t, tc.raw)
			err := migrateV19AtrMultRegimeKey(data)
			if len(tc.wantErr) > 0 {
				if err == nil {
					t.Fatalf("expected conflict error, got nil")
				}
				for _, want := range tc.wantErr {
					if !strings.Contains(err.Error(), want) {
						t.Fatalf("error %q missing %q", err, want)
					}
				}
				return
			}
			if err != nil {
				t.Fatalf("v19 migration failed: %v", err)
			}
			tc.check(t, v19StrategyBlock(t, data))
		})
	}
}

func TestV19RenamePreservesPreExistingUnrelatedKeys(t *testing.T) {
	raw := `{
		"config_version": 18,
		"regime": {"enabled": true, "period": 14, "adx_threshold": 20},
		"strategies": [{
			"id": "hl-eth-extra",
			"type": "perps",
			"platform": "hyperliquid",
			"script": "shared_scripts/check_hyperliquid.py",
			"args": ["sma_crossover", "ETH", "1h", "--mode=paper"],
			"capital": 1500,
			"max_drawdown_pct": 12,
			"leverage": 5,
			"margin_mode": "isolated",
			"trail_stop_atr_regime": {
				"trend_regime": {
					"trending_up": {"atr_multiple": 2.5},
					"trending_down": {"atr_multiple": 2.5},
					"ranging": {"atr_multiple": 1.5}
				}
			}
		}]
	}`
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, []byte(raw), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	cfg, err := LoadConfigForProbe(path)
	if err != nil {
		t.Fatalf("LoadConfigForProbe failed: %v", err)
	}
	sc := cfg.Strategies[0]
	if sc.Capital != 1500 {
		t.Fatalf("capital should be preserved through v19 migration, got %v", sc.Capital)
	}
	if sc.MaxDrawdownPct != 12 {
		t.Fatalf("max_drawdown_pct should be preserved, got %v", sc.MaxDrawdownPct)
	}
	if sc.Leverage != 5 {
		t.Fatalf("leverage should be preserved, got %v", sc.Leverage)
	}
	if sc.MarginMode != "isolated" {
		t.Fatalf("margin_mode should be preserved, got %q", sc.MarginMode)
	}
}

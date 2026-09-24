package main

import (
	"testing"
)

func TestMigrateV15CloseKeys(t *testing.T) {
	strategyOf := func(t *testing.T, raw map[string]interface{}) map[string]interface{} {
		t.Helper()
		return raw["strategies"].([]interface{})[0].(map[string]interface{})
	}
	closeParamsOf := func(t *testing.T, raw map[string]interface{}) map[string]interface{} {
		t.Helper()
		return strategyOf(t, raw)["close_strategy"].(map[string]interface{})["params"].(map[string]interface{})
	}
	regimeTier := func(frac, up, ranging, down float64) map[string]interface{} {
		return map[string]interface{}{
			"close_fraction": frac,
			"trend_regime": map[string]interface{}{
				"trending_up":   map[string]interface{}{"atr": up},
				"ranging":       map[string]interface{}{"atr": ranging},
				"trending_down": map[string]interface{}{"atr": down},
			},
		}
	}
	cases := []struct {
		name  string
		raw   map[string]interface{}
		check func(t *testing.T, raw map[string]interface{})
	}{
		{
			name: "tp_at_pct_becomes_single_tier_tiered_tp_pct",
			raw: map[string]interface{}{
				"config_version": 14,
				"strategies": []interface{}{
					map[string]interface{}{
						"id": "s1",
						"close_strategy": map[string]interface{}{
							"name":   "tp_at_pct",
							"params": map[string]interface{}{"pct": 0.05},
						},
					},
				},
			},
			check: func(t *testing.T, raw map[string]interface{}) {
				ref := strategyOf(t, raw)["close_strategy"].(map[string]interface{})
				if ref["name"] != "tiered_tp_pct" {
					t.Fatalf("name = %v, want tiered_tp_pct", ref["name"])
				}
				tier0 := closeParamsOf(t, raw)["tp_tiers"].([]interface{})[0].(map[string]interface{})
				if tier0["profit_pct"].(float64) != 0.05 {
					t.Errorf("profit_pct = %v, want 0.05", tier0["profit_pct"])
				}
				if tier0["close_fraction"].(float64) != 1.0 {
					t.Errorf("close_fraction = %v, want 1.0", tier0["close_fraction"])
				}
			},
		},
		{
			name: "canonicalizes_tier_keys",
			raw: map[string]interface{}{
				"strategies": []interface{}{
					map[string]interface{}{
						"id": "s1",
						"close_strategy": map[string]interface{}{
							"name": "tiered_tp_atr",
							"params": map[string]interface{}{
								"tiers": []interface{}{
									map[string]interface{}{"atr": 1.0, "fraction": 0.5},
									map[string]interface{}{"multiple": 2.0, "close_fraction": 1.0},
								},
							},
						},
					},
				},
			},
			check: func(t *testing.T, raw map[string]interface{}) {
				params := closeParamsOf(t, raw)
				tiers, ok := params["tp_tiers"].([]interface{})
				if !ok {
					t.Fatalf("params = %#v, want tp_tiers list", params)
				}
				t0 := tiers[0].(map[string]interface{})
				if t0["atr_multiple"].(float64) != 1.0 || t0["close_fraction"].(float64) != 0.5 {
					t.Errorf("tier[0] = %#v", t0)
				}
			},
		},
		{
			name: "lifts_legacy_regime_into_unified_close",
			raw: map[string]interface{}{
				"default_stop_loss_atr_mult": 1.5,
				"strategies": []interface{}{
					map[string]interface{}{
						"id": "hl-test",
						"stop_loss_atr_regime": map[string]interface{}{
							"trend_regime": map[string]interface{}{
								"trending_up":   map[string]interface{}{"atr": 1.5},
								"ranging":       map[string]interface{}{"atr": 0.8},
								"trending_down": map[string]interface{}{"atr": 1.5},
							},
						},
						"close_strategy": map[string]interface{}{
							"name": "tiered_tp_atr_regime",
							"params": map[string]interface{}{
								"tiers": []interface{}{regimeTier(0.5, 2.0, 1.0, 2.0), regimeTier(1.0, 4.0, 2.0, 4.0)},
							},
						},
					},
				},
			},
			check: func(t *testing.T, raw map[string]interface{}) {
				sc := strategyOf(t, raw)
				if _, ok := sc["stop_loss_atr_regime"]; ok {
					t.Error("stop_loss_atr_regime should be removed after fold")
				}
				if _, ok := sc["stop_loss_atr_mult"]; ok {
					t.Error("stop_loss_atr_mult should be removed after fold")
				}
				params := closeParamsOf(t, raw)
				if !closeParamsAreUnifiedRegime(params) {
					t.Fatalf("params not unified: %#v", params)
				}
				up := params["trend_regime"].(map[string]interface{})["trending_up"].(map[string]interface{})
				if up["stop_loss_atr"].(float64) != 1.5 {
					t.Errorf("trending_up stop_loss_atr = %v, want 1.5", up["stop_loss_atr"])
				}
				tiers := up["tp_tiers"].([]interface{})
				if len(tiers) != 2 {
					t.Fatalf("trending_up tp_tiers len = %d, want 2", len(tiers))
				}
				t0 := tiers[0].(map[string]interface{})
				if t0["atr_multiple"].(float64) != 2.0 || t0["close_fraction"].(float64) != 0.5 {
					t.Errorf("tier[0] = %#v", t0)
				}
			},
		},
		{
			name: "lift_legacy_regime_strips_scalar_stop_loss_into_fold",
			raw: map[string]interface{}{
				"strategies": []interface{}{
					map[string]interface{}{
						"id":                 "hl-test",
						"stop_loss_atr_mult": 1.5,
						"close_strategy": map[string]interface{}{
							"name": "tiered_tp_atr_regime",
							"params": map[string]interface{}{
								"tiers": []interface{}{regimeTier(0.5, 2.0, 1.0, 2.0)},
							},
						},
					},
				},
			},
			check: func(t *testing.T, raw map[string]interface{}) {
				sc := strategyOf(t, raw)
				if _, ok := sc["stop_loss_atr_mult"]; ok {
					t.Fatal("stop_loss_atr_mult should be stripped after unified fold")
				}
				up := closeParamsOf(t, raw)["trend_regime"].(map[string]interface{})["trending_up"].(map[string]interface{})
				if up["stop_loss_atr"].(float64) != 1.5 {
					t.Errorf("trending_up stop_loss_atr = %v, want 1.5 from scalar fallback", up["stop_loss_atr"])
				}
			},
		},
		{
			name: "trailing_tp_ratchet_regime_table_canonicalized_per_regime",
			raw: map[string]interface{}{
				"config_version": 14,
				"strategies": []interface{}{
					map[string]interface{}{
						"id":       "s1",
						"type":     "perps",
						"platform": "hyperliquid",
						"close_strategy": map[string]interface{}{
							"name": "trailing_tp_ratchet_regime",
							"params": map[string]interface{}{
								"tp_tiers": map[string]interface{}{
									"trending_up": []interface{}{
										map[string]interface{}{"atr": 1.5, "fraction": 0.0, "trailing_mult_after": 2.0},
									},
									"trending_down": []interface{}{
										map[string]interface{}{"multiple": 1.0, "close_fraction": 0.25, "trailing_mult_after": 1.5},
									},
									"ranging": []interface{}{
										map[string]interface{}{"atr_multiple": 1.0, "close_fraction": 0.0, "trailing_mult_after": 2.5},
									},
								},
							},
						},
					},
				},
			},
			check: func(t *testing.T, raw map[string]interface{}) {
				params := closeParamsOf(t, raw)
				table, ok := params["tp_tiers"].(map[string]interface{})
				if !ok {
					t.Fatalf("tp_tiers = %#v, want regime-keyed map", params["tp_tiers"])
				}
				up := table["trending_up"].([]interface{})[0].(map[string]interface{})
				if up["atr_multiple"].(float64) != 1.5 || up["close_fraction"].(float64) != 0 {
					t.Errorf("trending_up tier = %#v", up)
				}
				if up["trailing_mult_after"].(float64) != 2.0 {
					t.Errorf("trailing_mult_after = %v", up["trailing_mult_after"])
				}
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			migrateV15CloseKeys(tc.raw)
			tc.check(t, tc.raw)
		})
	}
}

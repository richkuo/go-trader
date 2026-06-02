package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestMigrateV15CloseKeys_TPAtPct(t *testing.T) {
	raw := map[string]interface{}{
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
	}
	migrateV15CloseKeys(raw)
	ref := raw["strategies"].([]interface{})[0].(map[string]interface{})["close_strategy"].(map[string]interface{})
	if ref["name"] != "tiered_tp_pct" {
		t.Fatalf("name = %v, want tiered_tp_pct", ref["name"])
	}
	params := ref["params"].(map[string]interface{})
	tiers := params["tp_tiers"].([]interface{})
	tier0 := tiers[0].(map[string]interface{})
	if tier0["profit_pct"].(float64) != 0.05 {
		t.Errorf("profit_pct = %v, want 0.05", tier0["profit_pct"])
	}
	if tier0["close_fraction"].(float64) != 1.0 {
		t.Errorf("close_fraction = %v, want 1.0", tier0["close_fraction"])
	}
}

func TestMigrateV15CloseKeys_CanonicalizeTierKeys(t *testing.T) {
	raw := map[string]interface{}{
		"strategies": []interface{}{
			map[string]interface{}{
				"id": "s1",
				"close_strategy": map[string]interface{}{
					"name": "tiered_tp_atr",
					"params": map[string]interface{}{
						"tiers": []interface{}{
							map[string]interface{}{
								"atr":      1.0,
								"fraction": 0.5,
							},
							map[string]interface{}{
								"multiple":       2.0,
								"close_fraction": 1.0,
							},
						},
					},
				},
			},
		},
	}
	migrateV15CloseKeys(raw)
	ref := raw["strategies"].([]interface{})[0].(map[string]interface{})["close_strategy"].(map[string]interface{})
	params := ref["params"].(map[string]interface{})
	tiers, ok := params["tp_tiers"].([]interface{})
	if !ok {
		t.Fatalf("params = %#v, want tp_tiers list", params)
	}
	t0 := tiers[0].(map[string]interface{})
	if t0["atr_multiple"].(float64) != 1.0 || t0["close_fraction"].(float64) != 0.5 {
		t.Errorf("tier[0] = %#v", t0)
	}
}

func TestMigrateV15CloseKeys_LiftLegacyRegime(t *testing.T) {
	tier0 := map[string]interface{}{
		"close_fraction": 0.5,
		"trend_regime": map[string]interface{}{
			"trending_up":   map[string]interface{}{"atr": 2.0},
			"ranging":       map[string]interface{}{"atr": 1.0},
			"trending_down": map[string]interface{}{"atr": 2.0},
		},
	}
	tier1 := map[string]interface{}{
		"close_fraction": 1.0,
		"trend_regime": map[string]interface{}{
			"trending_up":   map[string]interface{}{"atr": 4.0},
			"ranging":       map[string]interface{}{"atr": 2.0},
			"trending_down": map[string]interface{}{"atr": 4.0},
		},
	}
	raw := map[string]interface{}{
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
						"tiers": []interface{}{tier0, tier1},
					},
				},
			},
		},
	}
	migrateV15CloseKeys(raw)
	sc := raw["strategies"].([]interface{})[0].(map[string]interface{})
	if _, ok := sc["stop_loss_atr_regime"]; ok {
		t.Error("stop_loss_atr_regime should be removed after fold")
	}
	if _, ok := sc["stop_loss_atr_mult"]; ok {
		t.Error("stop_loss_atr_mult should be removed after fold")
	}
	ref := sc["close_strategy"].(map[string]interface{})
	params := ref["params"].(map[string]interface{})
	if !closeParamsAreUnifiedRegime(params) {
		t.Fatalf("params not unified: %#v", params)
	}
	tr := params["trend_regime"].(map[string]interface{})
	up := tr["trending_up"].(map[string]interface{})
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
}

func TestMigrateV15CloseKeys_LiftLegacyRegime_StripsScalarStopLoss(t *testing.T) {
	tier0 := map[string]interface{}{
		"close_fraction": 0.5,
		"trend_regime": map[string]interface{}{
			"trending_up":   map[string]interface{}{"atr": 2.0},
			"ranging":       map[string]interface{}{"atr": 1.0},
			"trending_down": map[string]interface{}{"atr": 2.0},
		},
	}
	raw := map[string]interface{}{
		"strategies": []interface{}{
			map[string]interface{}{
				"id":                 "hl-test",
				"stop_loss_atr_mult": 1.5,
				"close_strategy": map[string]interface{}{
					"name": "tiered_tp_atr_regime",
					"params": map[string]interface{}{
						"tiers": []interface{}{tier0},
					},
				},
			},
		},
	}
	migrateV15CloseKeys(raw)
	sc := raw["strategies"].([]interface{})[0].(map[string]interface{})
	if _, ok := sc["stop_loss_atr_mult"]; ok {
		t.Fatal("stop_loss_atr_mult should be stripped after unified fold")
	}
	params := sc["close_strategy"].(map[string]interface{})["params"].(map[string]interface{})
	up := params["trend_regime"].(map[string]interface{})["trending_up"].(map[string]interface{})
	if up["stop_loss_atr"].(float64) != 1.5 {
		t.Errorf("trending_up stop_loss_atr = %v, want 1.5 from scalar fallback", up["stop_loss_atr"])
	}
}

func TestLoadConfig_V15_RegimeFoldStripsScalarStop(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "v14.json")
	dbPath := filepath.Join(dir, "state.db")
	tier0 := map[string]interface{}{
		"close_fraction": 0.5,
		"trend_regime": map[string]interface{}{
			"trending_up":   map[string]interface{}{"atr": 2.0},
			"ranging":       map[string]interface{}{"atr": 1.0},
			"trending_down": map[string]interface{}{"atr": 2.0},
		},
	}
	tier1 := map[string]interface{}{
		"close_fraction": 1.0,
		"trend_regime": map[string]interface{}{
			"trending_up":   map[string]interface{}{"atr": 4.0},
			"ranging":       map[string]interface{}{"atr": 2.0},
			"trending_down": map[string]interface{}{"atr": 4.0},
		},
	}
	v14 := map[string]interface{}{
		"config_version":             14,
		"db_file":                    dbPath,
		"default_stop_loss_atr_mult": 1.0,
		"portfolio_risk":             map[string]interface{}{"max_drawdown_pct": 25, "warn_threshold_pct": 60},
		"regime": map[string]interface{}{
			"enabled":       true,
			"period":        14,
			"adx_threshold": 20,
		},
		"strategies": []interface{}{
			map[string]interface{}{
				"id":                 "hl-test",
				"type":               "perps",
				"platform":           "hyperliquid",
				"script":             "shared_scripts/check_hyperliquid.py",
				"args":               []interface{}{"tema_cross_bd", "BTC", "1h", "--mode=paper"},
				"capital":            1000.0,
				"max_drawdown_pct":   25.0,
				"leverage":           1.0,
				"stop_loss_atr_mult": 1.5,
				"close_strategy": map[string]interface{}{
					"name": "tiered_tp_atr_regime",
					"params": map[string]interface{}{
						"tiers": []interface{}{tier0, tier1},
					},
				},
			},
		},
	}
	data, _ := json.MarshalIndent(v14, "", "  ")
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig after v15 migration: %v", err)
	}
	if err := ValidateConfig(cfg); err != nil {
		t.Fatalf("ValidateConfig: %v", err)
	}
	sc := cfg.Strategies[0]
	if sc.StopLossATRMult != nil {
		t.Fatalf("StopLossATRMult = %v, want nil after unified fold", *sc.StopLossATRMult)
	}
	if !strategyUsesUnifiedRegimeClose(sc) {
		t.Fatal("expected unified regime close after migration")
	}
}

func TestLoadConfig_V15_MigratesCloseKeysOnDisk(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "v14.json")
	v14 := map[string]interface{}{
		"config_version": 14,
		"strategies": []interface{}{
			map[string]interface{}{
				"id":      "spot-test",
				"type":    "spot",
				"script":  "shared_scripts/check_strategy.py",
				"args":    []interface{}{"momentum", "BTC/USDT", "1h"},
				"capital": 1000.0,
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
	}
	data, _ := json.MarshalIndent(v14, "", "  ")
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.ConfigVersion != CurrentConfigVersion {
		t.Errorf("ConfigVersion = %d, want %d", cfg.ConfigVersion, CurrentConfigVersion)
	}
	updated, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var disk map[string]interface{}
	if err := json.Unmarshal(updated, &disk); err != nil {
		t.Fatal(err)
	}
	if int(disk["config_version"].(float64)) != CurrentConfigVersion {
		t.Errorf("on-disk config_version = %v", disk["config_version"])
	}
	sc := cfg.Strategies[0]
	tiersRaw, ok := closeTierListParam(sc.CloseStrategy.Params)
	if !ok {
		t.Fatal("CloseStrategy missing tp_tiers after migration")
	}
	want := []interface{}{
		map[string]interface{}{"atr_multiple": 1.0, "close_fraction": 0.5},
		map[string]interface{}{"atr_multiple": 2.0, "close_fraction": 1.0},
	}
	if !reflect.DeepEqual(tiersRaw, want) {
		t.Errorf("tp_tiers = %#v, want %#v", tiersRaw, want)
	}
}

func TestMigrateV15CloseKeys_TrailingTPRatchetRegimeTable(t *testing.T) {
	raw := map[string]interface{}{
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
								map[string]interface{}{
									"atr":                 1.5,
									"fraction":            0.0,
									"trailing_mult_after": 2.0,
								},
							},
							"trending_down": []interface{}{
								map[string]interface{}{
									"multiple":            1.0,
									"close_fraction":      0.25,
									"trailing_mult_after": 1.5,
								},
							},
							"ranging": []interface{}{
								map[string]interface{}{
									"atr_multiple":        1.0,
									"close_fraction":      0.0,
									"trailing_mult_after": 2.5,
								},
							},
						},
					},
				},
			},
		},
	}
	migrateV15CloseKeys(raw)
	ref := raw["strategies"].([]interface{})[0].(map[string]interface{})["close_strategy"].(map[string]interface{})
	params := ref["params"].(map[string]interface{})
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
}

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestKnownStrategyConfigKeysCoversCoreFields(t *testing.T) {
	known := knownStrategyConfigKeys()
	mustHave := []string{
		"id", "type", "platform", "symbol", "timeframe",
		"script", "args",
		"open_strategy", "close_strategy", "close_strategies", "allowed_regimes",
		"capital", "capital_pct", "initial_capital",
		"max_drawdown_pct", "interval_seconds",
		"htf_filter", "allow_shorts", "direction",
		"leverage", "sizing_leverage", "margin_per_trade_usd",
		"stop_loss_pct", "stop_loss_margin_pct",
		"trailing_stop_pct", "trailing_stop_atr_mult", "stop_loss_atr_mult",
		"trailing_stop_min_move_pct", "margin_mode",
		"theta_harvest", "futures", "circuit_breaker",
	}
	for _, k := range mustHave {
		if !known[k] {
			t.Errorf("knownStrategyConfigKeys missing %q — did a StrategyConfig json tag get renamed?", k)
		}
	}
}

func TestValidateStrategyJSONKeysAcceptsBothCloseSpellings(t *testing.T) {
	raw := []byte(`{
		"strategies": [
			{"id": "s1", "type": "spot", "script": "x.py", "args": [], "close_strategy": {"name": "tiered_tp_atr"}},
			{"id": "s2", "type": "spot", "script": "x.py", "args": [], "close_strategies": [{"name": "tiered_tp_atr"}]}
		]
	}`)
	if errs := validateStrategyJSONKeys(raw); len(errs) != 0 {
		t.Fatalf("want no unknown-field errors for either close spelling, got %v", errs)
	}
}

func TestValidateStrategyJSONKeysAcceptsAllKnownFields(t *testing.T) {
	raw := []byte(`{
		"strategies": [
			{
				"id": "hl-rmc-eth-live",
				"type": "perps",
				"platform": "hyperliquid",
				"script": "shared_scripts/check_hyperliquid.py",
				"args": ["range_mean_revert", "ETH", "1h", "--mode=live"],
				"open_strategy": {"name": "range_mean_revert"},
				"close_strategies": [{"name": "tiered_tp_atr"}],
				"capital": 1000,
				"leverage": 5,
				"sizing_leverage": 5,
				"margin_mode": "isolated",
				"stop_loss_atr_mult": 1.5,
				"trailing_stop_min_move_pct": 0.5,
				"direction": "long",
				"max_drawdown_pct": 50,
				"circuit_breaker": false
			}
		]
	}`)
	if errs := validateStrategyJSONKeys(raw); len(errs) != 0 {
		t.Fatalf("expected no errors for known fields, got: %v", errs)
	}
}

func TestValidateStrategyJSONKeysIgnoresTopLevelKeys(t *testing.T) {
	raw := []byte(`{
		"some_top_level_unknown": true,
		"strategies": [{"id": "s1", "type": "spot", "script": "x.py", "args": []}]
	}`)
	if errs := validateStrategyJSONKeys(raw); len(errs) != 0 {
		t.Fatalf("unknown top-level keys should not be flagged here (only strategy fields), got: %v", errs)
	}
}

func TestValidateUserDefaultsJSONKeysAcceptsCanonicalShape(t *testing.T) {
	raw := []byte(`{
		"user_defaults": {
			"close": {
				"trailing_tp_ratchet": {
					"tp_tiers": [
						{"atr_multiple": 2.0, "trailing_mult_after": 1.5, "close_fraction": 0.0}
					]
				}
			},
			"regime_atr": {
				"stop_loss_atr_mult_regime": {"use_defaults": true}
			},
			"manual": {
				"margin_usd": 125,
				"stop_loss_atr_mult": 2.25,
				"side": "short",
				"tp_tiers": [{"atr_multiple": 2.0, "close_fraction": 1.0}],
				"trailing_stop_atr_mult_regime": {"use_defaults": true}
			}
		},
		"strategies": [{"id": "s1", "type": "spot", "script": "x.py", "args": []}]
	}`)
	if errs := validateUserDefaultsJSONKeys(raw); len(errs) != 0 {
		t.Fatalf("expected no user_defaults unknown-key errors, got: %v", errs)
	}
}

func TestValidateStrategyJSONKeysReportsByIDDeterministically(t *testing.T) {
	raw := []byte(`{
		"strategies": [
			{"id": "b-strat", "type": "spot", "script": "x.py", "args": [], "zzz": 1, "aaa": 2},
			{"id": "a-strat", "type": "spot", "script": "x.py", "args": [], "bogus": 3}
		]
	}`)
	errs := validateStrategyJSONKeys(raw)
	want := []string{
		`strategy[b-strat]: unknown field "aaa"`,
		`strategy[b-strat]: unknown field "zzz"`,
		`strategy[a-strat]: unknown field "bogus"`,
	}
	if len(errs) != len(want) {
		t.Fatalf("want %d errs, got %d: %v", len(want), len(errs), errs)
	}
	for i, w := range want {
		if !strings.HasPrefix(errs[i], w) {
			t.Errorf("errs[%d] = %q, want prefix %q", i, errs[i], w)
		}
	}
}

func TestLoadConfigRejectsUnknownStrategyKey(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, "config.json")
	body := `{
		"config_version": 14,
		"db_file": "` + filepath.Join(tmp, "state.db") + `",
		"strategies": [
			{
				"id": "hl-rmc-eth-live",
				"type": "perps",
				"platform": "hyperliquid",
				"script": "shared_scripts/check_hyperliquid.py",
				"args": ["range_mean_revert", "ETH", "1h", "--mode=paper"],
				"open_strategy": {"name": "range_mean_revert"},
				"close_strategies": [{"name": "tiered_tp_atr"}],
				"capital": 1000,
				"max_drawdown_pct": 50,
				"take_profit_atr_mult": 2.0
			}
		]
	}`
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := LoadConfig(path)
	if err == nil {
		t.Fatal("LoadConfig accepted unknown strategy field")
	}
	if !strings.Contains(err.Error(), `unknown field "take_profit_atr_mult"`) {
		t.Errorf("error %q does not name the unknown field", err)
	}
}

func TestLoadConfigMigratesLegacyManualDefaultsBeforeUnknownKeyValidation(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, "config.json")
	body := `{
		"config_version": 16,
		"db_file": "` + filepath.Join(tmp, "state.db") + `",
		"manual_defaults": {"stop_loss_atr_mult": 2.25},
		"strategies": [
			{
				"id": "hl-manual-eth-live",
				"type": "manual",
				"platform": "hyperliquid",
				"symbol": "ETH",
				"timeframe": "1h",
				"capital": 1000,
				"max_drawdown_pct": 20,
				"leverage": 20
			}
		]
	}`
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig rejected legacy manual_defaults alias: %v", err)
	}
	sc := cfg.Strategies[0]
	if sc.StopLossATRMult == nil || *sc.StopLossATRMult != 2.25 {
		t.Fatalf("StopLossATRMult = %v, want migrated legacy alias value 2.25", sc.StopLossATRMult)
	}
}

func TestValidateStrategyJSONKeysUnknownFieldHints(t *testing.T) {
	cases := []struct {
		name     string
		strategy string
		wantSubs []string
	}{
		{
			name:     "invented TP field names strategy id, field, and close_strategy hint",
			strategy: `{"id": "hl-momentum-btc", "type": "perps", "platform": "hyperliquid", "script": "shared_scripts/check_hyperliquid.py", "args": ["momentum", "BTC", "1h"], "take_profit_atr_mult": 2.0}`,
			wantSubs: []string{`strategy[hl-momentum-btc]: unknown field "take_profit_atr_mult"`, "close_strategy"},
		},
		{
			name:     "legacy params hints open_strategy shape",
			strategy: `{"id": "s1", "type": "spot", "script": "x.py", "args": [], "params": {"foo": 1}}`,
			wantSubs: []string{"open_strategy: {name, params}"},
		},
		{
			name:     "misspelled stop_loss_atr_multiple hints valid SL fields",
			strategy: `{"id": "s1", "type": "perps", "script": "x.py", "args": [], "stop_loss_atr_multiple": 1.5}`,
			wantSubs: []string{"valid SL fields"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			errs := validateStrategyJSONKeys([]byte(`{"strategies": [` + tc.strategy + `]}`))
			if len(errs) != 1 {
				t.Fatalf("want 1 error, got %d: %v", len(errs), errs)
			}
			for _, w := range tc.wantSubs {
				if !strings.Contains(errs[0], w) {
					t.Errorf("error %q missing %q", errs[0], w)
				}
			}
		})
	}
}

func TestValidateUserDefaultsJSONKeysFlagsUnknownKeys(t *testing.T) {
	cases := []struct {
		name         string
		userDefaults string
		wantErr      string
	}{
		{"typo'd sibling", `{"manaul": {"stop_loss_atr_mult": 2.25}}`, `user_defaults: unknown field "manaul"`},
		{"typo'd manual leaf", `{"manual": {"margin": 125}}`, `user_defaults.manual: unknown field "margin"`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			raw := []byte(`{"user_defaults": ` + tc.userDefaults + `, "strategies": [{"id": "s1", "type": "spot", "script": "x.py", "args": []}]}`)
			errs := validateUserDefaultsJSONKeys(raw)
			if len(errs) != 1 {
				t.Fatalf("want 1 error, got %d: %v", len(errs), errs)
			}
			if !strings.Contains(errs[0], tc.wantErr) {
				t.Fatalf("unexpected error: %v", errs)
			}
		})
	}
}

func TestLoadConfigRejectsUnknownUserDefaultsKeys(t *testing.T) {
	cases := []struct {
		name         string
		userDefaults string
		wantErr      string
	}{
		{"manual leaf", `{"manual": {"stop_loss_atr_mlt": 2.25}}`, `user_defaults.manual: unknown field "stop_loss_atr_mlt"`},
		{"sibling", `{"manaul": {"stop_loss_atr_mult": 2.25}}`, `user_defaults: unknown field "manaul"`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tmp := t.TempDir()
			path := filepath.Join(tmp, "config.json")
			body := `{
				"config_version": 16,
				"db_file": "` + filepath.Join(tmp, "state.db") + `",
				"user_defaults": ` + tc.userDefaults + `,
				"strategies": [{"id": "hl-manual-eth-live", "type": "manual", "platform": "hyperliquid", "symbol": "ETH", "timeframe": "1h", "capital": 1000, "max_drawdown_pct": 20, "leverage": 20}]
			}`
			if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
				t.Fatal(err)
			}
			_, err := LoadConfig(path)
			if err == nil {
				t.Fatalf("LoadConfig accepted unknown user_defaults %s", tc.name)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("error %q does not name the unknown user_defaults %s", err, tc.name)
			}
		})
	}
}

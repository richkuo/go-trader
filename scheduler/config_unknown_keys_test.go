package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Sanity-check that the reflection-derived known-keys set actually contains
// the fields operators rely on day-to-day. A future rename of a json tag that
// also missed a deprecation here would otherwise silently turn live configs
// into "unknown field" errors on the next deploy.
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
		"theta_harvest", "futures",
	}
	for _, k := range mustHave {
		if !known[k] {
			t.Errorf("knownStrategyConfigKeys missing %q — did a StrategyConfig json tag get renamed?", k)
		}
	}
}

func TestValidateStrategyJSONKeysFlagsInventedTPField(t *testing.T) {
	raw := []byte(`{
		"strategies": [
			{
				"id": "hl-momentum-btc",
				"type": "perps",
				"platform": "hyperliquid",
				"script": "shared_scripts/check_hyperliquid.py",
				"args": ["momentum", "BTC", "1h"],
				"take_profit_atr_mult": 2.0
			}
		]
	}`)
	errs := validateStrategyJSONKeys(raw)
	if len(errs) != 1 {
		t.Fatalf("want 1 error for invented field, got %d: %v", len(errs), errs)
	}
	if !strings.Contains(errs[0], `strategy[hl-momentum-btc]: unknown field "take_profit_atr_mult"`) {
		t.Errorf("error missing strategy id + field name: %q", errs[0])
	}
	if !strings.Contains(errs[0], "close_strategy") {
		t.Errorf("error missing TP-field hint pointing operator to close_strategy: %q", errs[0])
	}
}

// #842: close_strategy is the canonical key and close_strategies is the
// accepted legacy spelling (UnmarshalJSON still reads the array) — neither must
// be flagged as an unknown field.
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

func TestValidateStrategyJSONKeysHintsLegacyParams(t *testing.T) {
	raw := []byte(`{
		"strategies": [
			{"id": "s1", "type": "spot", "script": "x.py", "args": [], "params": {"foo": 1}}
		]
	}`)
	errs := validateStrategyJSONKeys(raw)
	if len(errs) != 1 || !strings.Contains(errs[0], "open_strategy: {name, params}") {
		t.Fatalf("want legacy params hint, got %v", errs)
	}
}

func TestValidateStrategyJSONKeysHintsStopLossTypos(t *testing.T) {
	raw := []byte(`{
		"strategies": [
			{"id": "s1", "type": "perps", "script": "x.py", "args": [], "stop_loss_atr_multiple": 1.5}
		]
	}`)
	errs := validateStrategyJSONKeys(raw)
	if len(errs) != 1 || !strings.Contains(errs[0], "valid SL fields") {
		t.Fatalf("want SL hint for misspelled stop_loss_atr_multiple, got %v", errs)
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

// #1048: the reflection-derived known-key set must include the new
// circuit_breaker field so configs carrying it validate.
func TestKnownStrategyConfigKeysIncludesCircuitBreaker(t *testing.T) {
	if !knownStrategyConfigKeys()["circuit_breaker"] {
		t.Fatal("circuit_breaker should be a known strategy config key")
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

func TestValidateStrategyJSONKeysReportsByIDDeterministically(t *testing.T) {
	raw := []byte(`{
		"strategies": [
			{"id": "b-strat", "type": "spot", "script": "x.py", "args": [], "zzz": 1, "aaa": 2},
			{"id": "a-strat", "type": "spot", "script": "x.py", "args": [], "bogus": 3}
		]
	}`)
	errs := validateStrategyJSONKeys(raw)
	// Strategy-major order, key-sorted within each strategy.
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

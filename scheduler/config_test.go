package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"testing"
)

func writeTestConfig(t *testing.T, dir, content string) string {
	t.Helper()
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestEffectiveMarginPerTradeUSDOmittedReturnsZero(t *testing.T) {
	sc := StrategyConfig{Type: "perps", Leverage: 20, SizingLeverage: 1}
	if got := EffectiveMarginPerTradeUSD(sc); got != 0 {
		t.Errorf("EffectiveMarginPerTradeUSD(omitted) = %g, want 0", got)
	}
	if got := ComputePerpsOpenNotional(sc, 1000); got != 1000 {
		t.Errorf("ComputePerpsOpenNotional with omitted margin_per_trade_usd = %g, want 1000 (cash × sizing_leverage)", got)
	}
}

func TestLoadConfigLiveCredentialChecksFollowIsLiveArgs(t *testing.T) {
	for _, v := range []string{"HYPERLIQUID_SECRET_KEY", "OKX_API_KEY", "OKX_API_SECRET", "OKX_PASSPHRASE", "TOPSTEP_API_KEY", "TOPSTEP_API_SECRET", "TOPSTEP_ACCOUNT_ID", "ROBINHOOD_USERNAME", "ROBINHOOD_PASSWORD", "ROBINHOOD_TOTP_SECRET"} {
		t.Setenv(v, "")
	}
	hl := func(args ...string) map[string]interface{} {
		return map[string]interface{}{"id": "hl-eth", "type": "perps", "platform": "", "script": "shared_scripts/check_hyperliquid.py",
			"args": args, "capital": 100.0, "max_drawdown_pct": 10.0, "leverage": 1.0, "direction": "long", "margin_mode": "isolated"}
	}
	okx := func(typ string, args ...string) map[string]interface{} {
		sc := map[string]interface{}{"id": "okx-eth", "type": typ, "platform": "okx", "script": "shared_scripts/check_okx.py",
			"args": args, "capital": 100.0, "max_drawdown_pct": 10.0}
		if typ == "perps" {
			sc["leverage"] = 1.0
		}
		return sc
	}
	rh := func(args ...string) map[string]interface{} {
		return map[string]interface{}{"id": "rh-btc", "type": "spot", "platform": "robinhood", "script": "shared_scripts/check_robinhood.py",
			"args": args, "capital": 1000.0, "max_drawdown_pct": 10.0}
	}
	ts := func(args ...string) map[string]interface{} {
		return map[string]interface{}{"id": "ts-es", "type": "futures", "platform": "topstep", "script": "shared_scripts/check_topstep.py",
			"args": args, "capital": 1000.0, "max_drawdown_pct": 10.0}
	}
	rhWant := []string{"strategy[rh-btc]: --mode=live requires ROBINHOOD_USERNAME env var", "strategy[rh-btc]: --mode=live requires ROBINHOOD_PASSWORD env var", "strategy[rh-btc]: --mode=live requires ROBINHOOD_TOTP_SECRET env var"}
	requires := regexp.MustCompile(`strategy\[[^\]]+\]: --mode=live requires [A-Z_]+ env var`)
	cases := []struct {
		name  string
		sc    map[string]interface{}
		probe bool
		want  []string
	}{
		{"hyperliquid empty platform split live", hl("momentum", "ETH", "1h", "--mode", "live"), false,
			[]string{"strategy[hl-eth]: --mode=live requires HYPERLIQUID_SECRET_KEY env var"}},
		{"okx perps split live", okx("perps", "momentum", "ETH", "1h", "--mode", "live"), false,
			[]string{"strategy[okx-eth]: --mode=live requires OKX_API_KEY env var", "strategy[okx-eth]: --mode=live requires OKX_API_SECRET env var", "strategy[okx-eth]: --mode=live requires OKX_PASSPHRASE env var"}},
		{"okx spot split paper", okx("spot", "momentum", "ETH", "1h", "--mode", "paper"), false, nil},
		{"robinhood split live", rh("momentum_pro", "BTC", "1h", "--mode", "live"), false, rhWant},
		{"robinhood equals live", rh("momentum_pro", "BTC", "1h", "--mode=live"), false, rhWant},
		{"topstep split live", ts("momentum", "ES", "1h", "--mode", "live"), false,
			[]string{"strategy[ts-es]: --mode=live requires TOPSTEP_API_KEY env var", "strategy[ts-es]: --mode=live requires TOPSTEP_API_SECRET env var", "strategy[ts-es]: --mode=live requires TOPSTEP_ACCOUNT_ID env var"}},
		{"topstep split live probe skips credentials", ts("momentum", "ES", "1h", "--mode", "live"), true, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			data, err := json.Marshal(map[string]interface{}{"interval_seconds": 300, "strategies": []interface{}{tc.sc}})
			if err != nil {
				t.Fatal(err)
			}
			path := writeTestConfig(t, t.TempDir(), string(data))
			load := LoadConfig
			if tc.probe {
				load = LoadConfigForProbe
			}
			_, loadErr := load(path)
			var got []string
			if loadErr != nil {
				got = requires.FindAllString(loadErr.Error(), -1)
			}
			if !slices.Equal(got, tc.want) {
				t.Fatalf("credential errors = %q, want %q (load error: %v)", got, tc.want, loadErr)
			}
			if tc.want == nil && loadErr != nil {
				t.Fatalf("load must pass when no credential is required, got %v", loadErr)
			}
		})
	}
}

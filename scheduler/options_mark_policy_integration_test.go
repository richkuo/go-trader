package main

import (
	"strings"
	"testing"
)

func optionPolicyStrategy(id, platform, underlying string) map[string]interface{} {
	sc := map[string]interface{}{
		"id":      id,
		"type":    "options",
		"script":  "shared_scripts/check_options.py",
		"args":    []string{"vol_mean_reversion", underlying, "--mode=paper"},
		"capital": 1000,
	}
	if platform != "" {
		sc["platform"] = platform
		sc["args"] = []string{"vol_mean_reversion", underlying, "--platform=" + platform, "--mode=paper"}
	}
	return sc
}

func TestOptionPricingPolicyThroughConfigLoaders(t *testing.T) {
	refused := []struct {
		name, id, platform, underlying, want string
	}{
		{"unsupported_platform", "opt-binanceus", "binanceus", "BTC", `options platform "binanceus" has no option pricer (supported: deribit, ibkr, okx, robinhood)`},
		{"deribit_sol", "deribit-sol", "deribit", "SOL", `deribit options price only the inverse BTC and ETH option families`},
		{"okx_hype", "okx-hype", "okx", "HYPE", `okx options price only the inverse BTC and ETH option families`},
		{"robinhood_crypto", "rh-btc", "robinhood", "BTC", `robinhood options price only listed equity and ETF underlyings; "BTC" resolves to the Yahoo crypto pair BTC-USD`},
		{"prefixless_options_default_deribit", "opt-noprefix", "", "SOL", `deribit options price only the inverse BTC and ETH option families`},
	}
	for _, tc := range refused {
		t.Run("refuses_"+tc.name, func(t *testing.T) {
			path := writeNoEdgeConfig(t, CurrentConfigVersion, optionPolicyStrategy(tc.id, tc.platform, tc.underlying))
			_, readErr := LoadConfigReadOnly(path)
			_, loadErr := LoadConfig(path)
			for name, err := range map[string]error{"LoadConfigReadOnly": readErr, "LoadConfig": loadErr} {
				if err == nil {
					t.Fatalf("%s accepted %s %s options", name, tc.platform, tc.underlying)
				}
				if !strings.Contains(err.Error(), "strategy["+tc.id+"]") || !strings.Contains(err.Error(), tc.want) {
					t.Fatalf("%s error = %v, want strategy id and %q", name, err, tc.want)
				}
			}
		})
	}
	accepted := []struct {
		id, platform, underlying, wantPlatform string
	}{
		{"deribit-btc", "deribit", "BTC", "deribit"},
		{"deribit-eth", "deribit", "ETH", "deribit"},
		{"okx-eth", "okx", "ETH", "okx"},
		{"rh-spy", "robinhood", "SPY", "robinhood"},
		{"ibkr-btc", "ibkr", "BTC", "ibkr"},
		{"opt-prefixless", "", "BTC", "deribit"},
	}
	for _, tc := range accepted {
		t.Run("loads_"+tc.id, func(t *testing.T) {
			path := writeNoEdgeConfig(t, CurrentConfigVersion, optionPolicyStrategy(tc.id, tc.platform, tc.underlying))
			readOnly, err := LoadConfigReadOnly(path)
			if err != nil {
				t.Fatalf("LoadConfigReadOnly: %v", err)
			}
			cfg, err := LoadConfig(path)
			if err != nil {
				t.Fatalf("LoadConfig: %v", err)
			}
			for _, c := range []*Config{readOnly, cfg} {
				if got := c.Strategies[0].Platform; got != tc.wantPlatform {
					t.Fatalf("resolved platform = %q, want %q", got, tc.wantPlatform)
				}
			}
		})
	}
}

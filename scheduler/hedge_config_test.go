package main

import (
	"strings"
	"testing"
)

// hlPerpsWithHedge builds a minimal HL perps strategy config carrying a hedge
// block, for validation tests.
func hlPerpsWithHedge(id, coin string, h *HedgeConfig) StrategyConfig {
	return StrategyConfig{
		ID:       id,
		Type:     "perps",
		Platform: "hyperliquid",
		Script:   "check_hyperliquid.py",
		Args:     []string{"open_strat", coin},
		Hedge:    h,
	}
}

func TestNormalizeHedgeCoin(t *testing.T) {
	cases := map[string]string{
		"BTC":            "BTC",
		"btc":            "BTC",
		"  eth ":         "ETH",
		"BTC/USDC:USDC":  "BTC",
		"BTC/USDT":       "BTC",
		"SOL/USDC:USDC ": "SOL",
		"":               "",
	}
	for in, want := range cases {
		if got := normalizeHedgeCoin(in); got != want {
			t.Errorf("normalizeHedgeCoin(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestHedgeAccessorsDefaults(t *testing.T) {
	sc := hlPerpsWithHedge("s1", "ETH", &HedgeConfig{Enabled: true, Symbol: "BTC"})
	if !HedgeEnabled(sc) {
		t.Fatal("HedgeEnabled should be true")
	}
	if got := hedgeCoin(sc); got != "BTC" {
		t.Errorf("hedgeCoin = %q, want BTC", got)
	}
	if got := HedgeRatio(sc); got != 1.0 {
		t.Errorf("HedgeRatio default = %g, want 1.0", got)
	}
	if got := hedgeLeverage(sc); got != 1.0 {
		t.Errorf("hedgeLeverage default = %g, want 1.0", got)
	}
	if got := hedgeMarginMode(sc); got != "isolated" {
		t.Errorf("hedgeMarginMode default = %q, want isolated", got)
	}

	// nil hedge → all safe.
	bare := hlPerpsWithHedge("s2", "ETH", nil)
	if HedgeEnabled(bare) || hedgeCoin(bare) != "" {
		t.Error("nil hedge should be disabled with empty coin")
	}

	// explicit values.
	sc2 := hlPerpsWithHedge("s3", "ETH", &HedgeConfig{Enabled: true, Symbol: "BTC/USDC:USDC", Ratio: 0.5, Leverage: 3, MarginMode: "Cross"})
	if got := HedgeRatio(sc2); got != 0.5 {
		t.Errorf("HedgeRatio = %g, want 0.5", got)
	}
	if got := hedgeLeverage(sc2); got != 3 {
		t.Errorf("hedgeLeverage = %g, want 3", got)
	}
	if got := hedgeMarginMode(sc2); got != "cross" {
		t.Errorf("hedgeMarginMode = %q, want cross", got)
	}
	if got := hedgeCoin(sc2); got != "BTC" {
		t.Errorf("hedgeCoin = %q, want BTC", got)
	}

	// disabled block → HedgeEnabled false but hedgeCoin still resolves.
	sc3 := hlPerpsWithHedge("s4", "ETH", &HedgeConfig{Enabled: false, Symbol: "BTC"})
	if HedgeEnabled(sc3) {
		t.Error("disabled hedge should not be enabled")
	}
}

func hasErrContaining(errs []string, substr string) bool {
	for _, e := range errs {
		if strings.Contains(e, substr) {
			return true
		}
	}
	return false
}

func TestValidateHedgeConfigs_Valid(t *testing.T) {
	cfg := &Config{Strategies: []StrategyConfig{
		hlPerpsWithHedge("eth-long", "ETH", &HedgeConfig{Enabled: true, Symbol: "BTC", Side: "inverse", Ratio: 1.0, Leverage: 3, MarginMode: "cross"}),
	}}
	if errs := validateHedgeConfigs(cfg); len(errs) != 0 {
		t.Errorf("expected no errors, got %v", errs)
	}
}

func TestValidateHedgeConfigs_OwnCoinCollision(t *testing.T) {
	cfg := &Config{Strategies: []StrategyConfig{
		hlPerpsWithHedge("eth-long", "ETH", &HedgeConfig{Enabled: true, Symbol: "ETH"}),
	}}
	errs := validateHedgeConfigs(cfg)
	if !hasErrContaining(errs, "own primary coin") {
		t.Errorf("expected own-coin collision, got %v", errs)
	}
}

func TestValidateHedgeConfigs_PeerCoinCollision(t *testing.T) {
	cfg := &Config{Strategies: []StrategyConfig{
		hlPerpsWithHedge("eth-long", "ETH", &HedgeConfig{Enabled: true, Symbol: "BTC"}),
		hlPerpsWithHedge("btc-strat", "BTC", nil), // BTC is another strategy's primary coin
	}}
	errs := validateHedgeConfigs(cfg)
	if !hasErrContaining(errs, "collides with configured strategy coin") {
		t.Errorf("expected peer-coin collision, got %v", errs)
	}
}

func TestValidateHedgeConfigs_HedgeVsHedgeCollision(t *testing.T) {
	cfg := &Config{Strategies: []StrategyConfig{
		hlPerpsWithHedge("eth-long", "ETH", &HedgeConfig{Enabled: true, Symbol: "BTC"}),
		hlPerpsWithHedge("sol-long", "SOL", &HedgeConfig{Enabled: true, Symbol: "BTC"}),
	}}
	errs := validateHedgeConfigs(cfg)
	if !hasErrContaining(errs, "shared by hedge-enabled strategies") {
		t.Errorf("expected hedge-vs-hedge collision, got %v", errs)
	}
}

func TestValidateHedgeConfigs_NonHLPerpsRejected(t *testing.T) {
	spot := StrategyConfig{ID: "spot1", Type: "spot", Platform: "binanceus", Hedge: &HedgeConfig{Enabled: true, Symbol: "BTC"}}
	cfg := &Config{Strategies: []StrategyConfig{spot}}
	errs := validateHedgeConfigs(cfg)
	if !hasErrContaining(errs, "Hyperliquid-perps-only") {
		t.Errorf("expected HL-perps-only rejection, got %v", errs)
	}
}

func TestValidateHedgeConfigs_DirectionBothRejected(t *testing.T) {
	sc := hlPerpsWithHedge("eth", "ETH", &HedgeConfig{Enabled: true, Symbol: "BTC"})
	sc.Direction = "both"
	cfg := &Config{Strategies: []StrategyConfig{sc}}
	errs := validateHedgeConfigs(cfg)
	if !hasErrContaining(errs, "direction=\"both\"") {
		t.Errorf("expected direction=both rejection, got %v", errs)
	}
}

func TestValidateHedgeConfigs_BadFields(t *testing.T) {
	cases := []struct {
		name   string
		hedge  *HedgeConfig
		substr string
	}{
		{"bad side", &HedgeConfig{Enabled: true, Symbol: "BTC", Side: "same"}, "hedge.side"},
		{"bad ratio", &HedgeConfig{Enabled: true, Symbol: "BTC", Ratio: 11}, "hedge.ratio"},
		{"neg ratio", &HedgeConfig{Enabled: true, Symbol: "BTC", Ratio: -1}, "hedge.ratio"},
		{"bad leverage", &HedgeConfig{Enabled: true, Symbol: "BTC", Leverage: 101}, "hedge.leverage"},
		{"bad margin", &HedgeConfig{Enabled: true, Symbol: "BTC", MarginMode: "weird"}, "hedge.margin_mode"},
		{"bad platform", &HedgeConfig{Enabled: true, Symbol: "BTC", Platform: "okx"}, "hedge.platform"},
		{"bad type", &HedgeConfig{Enabled: true, Symbol: "BTC", Type: "spot"}, "hedge.type"},
		{"empty symbol", &HedgeConfig{Enabled: true, Symbol: ""}, "hedge.symbol is required"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &Config{Strategies: []StrategyConfig{hlPerpsWithHedge("eth", "ETH", tc.hedge)}}
			errs := validateHedgeConfigs(cfg)
			if !hasErrContaining(errs, tc.substr) {
				t.Errorf("expected error containing %q, got %v", tc.substr, errs)
			}
		})
	}
}

func TestValidateHedgeConfigs_DisabledBlockSkipsCollision(t *testing.T) {
	// A disabled hedge block that would otherwise collide must not error.
	cfg := &Config{Strategies: []StrategyConfig{
		hlPerpsWithHedge("eth", "ETH", &HedgeConfig{Enabled: false, Symbol: "ETH"}),
	}}
	if errs := validateHedgeConfigs(cfg); len(errs) != 0 {
		t.Errorf("disabled hedge should skip collision matrix, got %v", errs)
	}
}

func TestValidateHedgeJSONKeys_UnknownNestedField(t *testing.T) {
	raw := []byte(`{"strategies":[{"id":"eth","type":"perps","platform":"hyperliquid","hedge":{"enabled":true,"symbol":"BTC","ration":1.0}}]}`)
	errs := validateStrategyJSONKeys(raw)
	if !hasErrContaining(errs, "hedge: unknown field \"ration\"") {
		t.Errorf("expected nested unknown-key error, got %v", errs)
	}
}

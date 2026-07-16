package main

import (
	"strings"
	"testing"
)

// hlPerps builds a minimal HL perps StrategyConfig with the given id, coin, and
// optional hedge block for validateHedgeConfigs tests.
func hlPerps(id, coin string, hedge *HedgeConfig) StrategyConfig {
	return StrategyConfig{
		ID:       id,
		Type:     "perps",
		Platform: "hyperliquid",
		Script:   "check_hyperliquid.py",
		Args:     []string{"check_hyperliquid.py", coin},
		Hedge:    hedge,
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
	strategies := []StrategyConfig{
		hlPerps("eth", "ETH", &HedgeConfig{Enabled: true, Symbol: "BTC", Side: "inverse", Ratio: 1.0, Leverage: 3, MarginMode: "cross"}),
	}
	if errs := validateHedgeConfigs(strategies); len(errs) != 0 {
		t.Fatalf("expected no errors, got %v", errs)
	}
}

func TestValidateHedgeConfigs_CcxtSymbolNormalizes(t *testing.T) {
	// "BTC/USDC:USDC" must resolve to coin "BTC" and validate cleanly.
	strategies := []StrategyConfig{
		hlPerps("eth", "ETH", &HedgeConfig{Enabled: true, Symbol: "BTC/USDC:USDC"}),
	}
	if errs := validateHedgeConfigs(strategies); len(errs) != 0 {
		t.Fatalf("expected ccxt symbol to normalize cleanly, got %v", errs)
	}
	if got := hedgeCoin(strategies[0]); got != "BTC" {
		t.Fatalf("hedgeCoin = %q, want BTC", got)
	}
}

func TestValidateHedgeConfigs_DisabledBlockInert(t *testing.T) {
	// A disabled hedge block with otherwise-illegal fields must not error.
	strategies := []StrategyConfig{
		hlPerps("eth", "ETH", &HedgeConfig{Enabled: false, Symbol: "ETH", Side: "garbage", Ratio: 999}),
	}
	if errs := validateHedgeConfigs(strategies); len(errs) != 0 {
		t.Fatalf("disabled block should be inert, got %v", errs)
	}
}

func TestValidateHedgeConfigs_OwnCoinCollision(t *testing.T) {
	strategies := []StrategyConfig{
		hlPerps("eth", "ETH", &HedgeConfig{Enabled: true, Symbol: "ETH"}),
	}
	errs := validateHedgeConfigs(strategies)
	if !hasErrContaining(errs, "own primary coin") {
		t.Fatalf("expected own-coin collision error, got %v", errs)
	}
}

func TestValidateHedgeConfigs_PeerCoinCollision(t *testing.T) {
	// eth's hedge coin BTC collides with a peer strategy's primary coin BTC.
	strategies := []StrategyConfig{
		hlPerps("eth", "ETH", &HedgeConfig{Enabled: true, Symbol: "BTC"}),
		hlPerps("btc", "BTC", nil),
	}
	errs := validateHedgeConfigs(strategies)
	if !hasErrContaining(errs, "configured primary coin") {
		t.Fatalf("expected peer-coin collision error, got %v", errs)
	}
}

func TestValidateHedgeConfigs_PeerCoinCollisionManualPaper(t *testing.T) {
	// Collision must also catch a MANUAL peer's coin (paper or live).
	manual := StrategyConfig{ID: "man", Type: "manual", Platform: "hyperliquid", Symbol: "btc"}
	strategies := []StrategyConfig{
		hlPerps("eth", "ETH", &HedgeConfig{Enabled: true, Symbol: "BTC"}),
		manual,
	}
	errs := validateHedgeConfigs(strategies)
	if !hasErrContaining(errs, "configured primary coin") {
		t.Fatalf("expected manual-peer collision error (case-normalized), got %v", errs)
	}
}

func TestValidateHedgeConfigs_HedgeVsHedgeCollision(t *testing.T) {
	strategies := []StrategyConfig{
		hlPerps("eth", "ETH", &HedgeConfig{Enabled: true, Symbol: "SOL"}),
		hlPerps("avax", "AVAX", &HedgeConfig{Enabled: true, Symbol: "SOL"}),
	}
	errs := validateHedgeConfigs(strategies)
	if !hasErrContaining(errs, "shared with other hedge-enabled") {
		t.Fatalf("expected hedge-vs-hedge collision error, got %v", errs)
	}
}

func TestValidateHedgeConfigs_NonHLPerpsRejected(t *testing.T) {
	spot := StrategyConfig{ID: "s", Type: "spot", Platform: "binanceus", Hedge: &HedgeConfig{Enabled: true, Symbol: "BTC"}}
	errs := validateHedgeConfigs([]StrategyConfig{spot})
	if !hasErrContaining(errs, "only supported on hyperliquid perps") {
		t.Fatalf("expected non-HL-perps rejection, got %v", errs)
	}
}

func TestValidateHedgeConfigs_Vocabulary(t *testing.T) {
	cases := []struct {
		name  string
		hedge *HedgeConfig
		want  string
	}{
		{"side", &HedgeConfig{Enabled: true, Symbol: "BTC", Side: "direct"}, "side must be"},
		{"platform", &HedgeConfig{Enabled: true, Symbol: "BTC", Platform: "okx"}, "platform must be"},
		{"type", &HedgeConfig{Enabled: true, Symbol: "BTC", Type: "spot"}, "type must be"},
		{"ratio_hi", &HedgeConfig{Enabled: true, Symbol: "BTC", Ratio: 11}, "ratio must be"},
		{"ratio_neg", &HedgeConfig{Enabled: true, Symbol: "BTC", Ratio: -1}, "ratio must be"},
		{"margin", &HedgeConfig{Enabled: true, Symbol: "BTC", MarginMode: "portfolio"}, "margin_mode must be"},
		{"lev_hi", &HedgeConfig{Enabled: true, Symbol: "BTC", Leverage: 101}, "leverage must be"},
		{"lev_lo", &HedgeConfig{Enabled: true, Symbol: "BTC", Leverage: 0.5}, "leverage must be"},
		{"empty_symbol", &HedgeConfig{Enabled: true, Symbol: ""}, "symbol is empty"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			errs := validateHedgeConfigs([]StrategyConfig{hlPerps("eth", "ETH", tc.hedge)})
			if !hasErrContaining(errs, tc.want) {
				t.Fatalf("expected error containing %q, got %v", tc.want, errs)
			}
		})
	}
}

func TestValidateHedgeConfigs_DirectionBothRejected(t *testing.T) {
	sc := hlPerps("eth", "ETH", &HedgeConfig{Enabled: true, Symbol: "BTC"})
	sc.Direction = DirectionBoth
	errs := validateHedgeConfigs([]StrategyConfig{sc})
	if !hasErrContaining(errs, "direction=\"both\"") {
		t.Fatalf("expected direction=both rejection, got %v", errs)
	}
}

func TestValidateHedgeConfigs_LongShortAllowed(t *testing.T) {
	for _, dir := range []string{DirectionLong, DirectionShort} {
		sc := hlPerps("eth", "ETH", &HedgeConfig{Enabled: true, Symbol: "BTC"})
		sc.Direction = dir
		if errs := validateHedgeConfigs([]StrategyConfig{sc}); len(errs) != 0 {
			t.Fatalf("direction %q should be allowed with a hedge, got %v", dir, errs)
		}
	}
}

func TestValidateHedgeJSONKeys_Typo(t *testing.T) {
	raw := []byte(`{"strategies":[{"id":"eth","hedge":{"enabled":true,"symbol":"BTC","ration":2}}]}`)
	errs := validateStrategyJSONKeys(raw)
	if !hasErrContaining(errs, "hedge: unknown field \"ration\"") {
		t.Fatalf("expected nested hedge unknown-key error, got %v", errs)
	}
}

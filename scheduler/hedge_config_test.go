package main

import (
	"strings"
	"testing"
)

func perpsHedgeStrategy(id, coin string, hedge *HedgeConfig) StrategyConfig {
	return StrategyConfig{
		ID:        id,
		Type:      "perps",
		Platform:  "hyperliquid",
		Direction: "long",
		Args:      []string{"check_hyperliquid.py", coin, "live"},
		Hedge:     hedge,
	}
}

func errsContain(errs []string, substr string) bool {
	for _, e := range errs {
		if strings.Contains(e, substr) {
			return true
		}
	}
	return false
}

func TestHedgeConfigValidCase(t *testing.T) {
	strats := []StrategyConfig{
		perpsHedgeStrategy("hl-eth", "ETH", &HedgeConfig{Enabled: true, Symbol: "BTC", Side: "inverse", Ratio: 1.0, MarginMode: "cross", Leverage: 3}),
	}
	if errs := hyperliquidHedgeConfigErrors(strats); len(errs) != 0 {
		t.Errorf("valid hedge config produced errors: %v", errs)
	}
}

func TestHedgeConfigCcxtSymbolNormalizes(t *testing.T) {
	strats := []StrategyConfig{
		perpsHedgeStrategy("hl-eth", "ETH", &HedgeConfig{Enabled: true, Symbol: "BTC/USDC:USDC"}),
	}
	if errs := hyperliquidHedgeConfigErrors(strats); len(errs) != 0 {
		t.Errorf("ccxt hedge symbol should normalize to BTC and validate: %v", errs)
	}
}

func TestHedgeConfigRejectsHedgeEqualsOwnCoin(t *testing.T) {
	strats := []StrategyConfig{
		perpsHedgeStrategy("hl-eth", "ETH", &HedgeConfig{Enabled: true, Symbol: "ETH"}),
	}
	errs := hyperliquidHedgeConfigErrors(strats)
	if !errsContain(errs, "own primary coin") {
		t.Errorf("expected own-coin collision, got %v", errs)
	}
}

func TestHedgeConfigRejectsHedgeEqualsOtherStrategyCoin(t *testing.T) {
	strats := []StrategyConfig{
		perpsHedgeStrategy("hl-eth", "ETH", &HedgeConfig{Enabled: true, Symbol: "BTC"}),
		perpsHedgeStrategy("hl-btc", "BTC", nil),
	}
	errs := hyperliquidHedgeConfigErrors(strats)
	if !errsContain(errs, "configured trading coin") {
		t.Errorf("expected configured-coin collision with hl-btc, got %v", errs)
	}
}

func TestHedgeConfigRejectsHedgeVsHedgeCollision(t *testing.T) {
	strats := []StrategyConfig{
		perpsHedgeStrategy("hl-eth", "ETH", &HedgeConfig{Enabled: true, Symbol: "BTC"}),
		perpsHedgeStrategy("hl-sol", "SOL", &HedgeConfig{Enabled: true, Symbol: "BTC"}),
	}
	errs := hyperliquidHedgeConfigErrors(strats)
	if !errsContain(errs, "shared by hedge-enabled strategies") {
		t.Errorf("expected hedge-vs-hedge collision, got %v", errs)
	}
}

func TestHedgeConfigRejectsManualHost(t *testing.T) {
	strats := []StrategyConfig{
		{ID: "man", Type: "manual", Platform: "hyperliquid", Symbol: "ETH", Args: []string{"hold", "ETH"}, Hedge: &HedgeConfig{Enabled: true, Symbol: "BTC"}},
	}
	errs := hyperliquidHedgeConfigErrors(strats)
	if !errsContain(errs, "type=perps") {
		t.Errorf("expected manual-host rejection, got %v", errs)
	}
}

func TestHedgeConfigRejectsNonHyperliquid(t *testing.T) {
	strats := []StrategyConfig{
		{ID: "okx", Type: "perps", Platform: "okx", Args: []string{"check_okx.py", "ETH", "live"}, Hedge: &HedgeConfig{Enabled: true, Symbol: "BTC"}},
	}
	errs := hyperliquidHedgeConfigErrors(strats)
	if !errsContain(errs, "platform=hyperliquid") {
		t.Errorf("expected non-HL platform rejection, got %v", errs)
	}
}

func TestHedgeConfigRejectsDirectionBoth(t *testing.T) {
	sc := perpsHedgeStrategy("hl-eth", "ETH", &HedgeConfig{Enabled: true, Symbol: "BTC"})
	sc.Direction = "both"
	errs := hyperliquidHedgeConfigErrors([]StrategyConfig{sc})
	if !errsContain(errs, `direction="both"`) {
		t.Errorf("expected direction=both rejection, got %v", errs)
	}
}

func TestHedgeConfigRejectsBadVocabulary(t *testing.T) {
	sc := perpsHedgeStrategy("hl-eth", "ETH", &HedgeConfig{Enabled: true, Symbol: "BTC", Side: "opposite", Ratio: 20, MarginMode: "weird", Leverage: 200, Platform: "binance", Type: "spot"})
	errs := hyperliquidHedgeConfigErrors([]StrategyConfig{sc})
	for _, want := range []string{"side must be", "ratio must be", "margin_mode must be", "leverage must be", "platform must be", "type must be"} {
		if !errsContain(errs, want) {
			t.Errorf("expected error containing %q, got %v", want, errs)
		}
	}
}

func TestHedgeConfigRejectsEmptySymbolWhenEnabled(t *testing.T) {
	sc := perpsHedgeStrategy("hl-eth", "ETH", &HedgeConfig{Enabled: true, Symbol: ""})
	errs := hyperliquidHedgeConfigErrors([]StrategyConfig{sc})
	if !errsContain(errs, "symbol is required") {
		t.Errorf("expected empty-symbol rejection, got %v", errs)
	}
}

func TestHedgeConfigDisabledBlockStillValidatesShape(t *testing.T) {
	// A disabled block with a bad ratio still surfaces the shape error, but no
	// host/collision checks (Enabled=false).
	sc := perpsHedgeStrategy("hl-eth", "ETH", &HedgeConfig{Enabled: false, Symbol: "ETH", Ratio: -5})
	errs := hyperliquidHedgeConfigErrors([]StrategyConfig{sc})
	if !errsContain(errs, "ratio must be") {
		t.Errorf("disabled block should still validate ratio, got %v", errs)
	}
	if errsContain(errs, "own primary coin") {
		t.Errorf("disabled block should NOT run collision checks, got %v", errs)
	}
}

func TestHedgeNestedUnknownKeyRejected(t *testing.T) {
	raw := []byte(`{"strategies":[{"id":"hl-eth","type":"perps","platform":"hyperliquid","hedge":{"enabled":true,"symbol":"BTC","ration":1.0}}]}`)
	errs := validateStrategyJSONKeys(raw)
	if !errsContain(errs, `hedge: unknown field "ration"`) {
		t.Errorf("expected nested hedge typo rejection, got %v", errs)
	}
}

func TestHedgeKnownKeysAccepted(t *testing.T) {
	raw := []byte(`{"strategies":[{"id":"hl-eth","type":"perps","platform":"hyperliquid","hedge":{"enabled":true,"symbol":"BTC","side":"inverse","ratio":1.0,"platform":"hyperliquid","type":"perps","margin_mode":"cross","leverage":3}}]}`)
	errs := validateStrategyJSONKeys(raw)
	for _, e := range errs {
		if strings.Contains(e, "hedge:") {
			t.Errorf("valid hedge keys should not be flagged, got %v", errs)
		}
	}
}

func TestHedgeAccessorDefaults(t *testing.T) {
	sc := StrategyConfig{Hedge: &HedgeConfig{Enabled: true, Symbol: "btc/usdc:usdc"}}
	if !sc.HedgeEnabled() {
		t.Error("HedgeEnabled should be true")
	}
	if hedgeCoin(sc) != "BTC" {
		t.Errorf("hedgeCoin normalize: want BTC, got %s", hedgeCoin(sc))
	}
	if hedgeRatio(sc) != 1.0 {
		t.Errorf("default ratio: want 1.0, got %g", hedgeRatio(sc))
	}
	if hedgeSide(sc) != "inverse" {
		t.Errorf("default side: want inverse, got %s", hedgeSide(sc))
	}
	if hedgeMarginMode(sc) != "isolated" {
		t.Errorf("default margin mode: want isolated, got %s", hedgeMarginMode(sc))
	}
	if hedgeLeverage(sc) != 1.0 {
		t.Errorf("default leverage: want 1.0, got %g", hedgeLeverage(sc))
	}
}

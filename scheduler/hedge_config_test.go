package main

import (
	"strings"
	"testing"
)

// Config-surface and collision-matrix tests for #1159 correlated hedge legs.
//
// The collision rules are the load-bearing phase-1 safety constraint: they are
// what makes it safe for every shared-coin mechanism (peer margin checks,
// circuit-breaker drain, kill-switch fill share, reconcile ownership) to stay
// blind to hedge coins. Each rule gets an explicit test.

func hedgePerpsStrategy(id, coin string) StrategyConfig {
	return StrategyConfig{
		ID:       id,
		Type:     "perps",
		Platform: "hyperliquid",
		Script:   "shared_scripts/check_hyperliquid.py",
		Args:     []string{"--symbol", coin, "--mode", "live"},
	}
}

func withHedge(sc StrategyConfig, h *HedgeConfig) StrategyConfig {
	sc.Hedge = h
	return sc
}

func hedgeErrsContaining(errs []string, needle string) bool {
	for _, e := range errs {
		if strings.Contains(e, needle) {
			return true
		}
	}
	return false
}

func TestValidateHedgeConfigsAcceptsCleanConfig(t *testing.T) {
	cfg := &Config{Strategies: []StrategyConfig{
		withHedge(hedgePerpsStrategy("eth-long", "ETH"), &HedgeConfig{Enabled: true, Symbol: "BTC", Ratio: 1.0, Leverage: 3, MarginMode: "cross"}),
	}}
	if errs := validateHedgeConfigs(cfg); len(errs) != 0 {
		t.Fatalf("expected no errors, got %v", errs)
	}
}

func TestValidateHedgeConfigsRejectsOwnPrimaryCoin(t *testing.T) {
	cfg := &Config{Strategies: []StrategyConfig{
		withHedge(hedgePerpsStrategy("eth-long", "ETH"), &HedgeConfig{Enabled: true, Symbol: "ETH"}),
	}}
	errs := validateHedgeConfigs(cfg)
	if !hedgeErrsContaining(errs, "is the strategy's own coin") {
		t.Fatalf("expected own-coin rejection, got %v", errs)
	}
}

// A ccxt-style symbol must normalize to the same coin as the bare ticker, or
// the collision check could be bypassed by writing "ETH/USDC:USDC".
func TestValidateHedgeConfigsNormalizesCcxtSymbolForCollisions(t *testing.T) {
	cfg := &Config{Strategies: []StrategyConfig{
		withHedge(hedgePerpsStrategy("eth-long", "ETH"), &HedgeConfig{Enabled: true, Symbol: "eth/USDC:USDC"}),
	}}
	errs := validateHedgeConfigs(cfg)
	if !hedgeErrsContaining(errs, "is the strategy's own coin") {
		t.Fatalf("expected ccxt-normalized own-coin rejection, got %v", errs)
	}
}

func TestValidateHedgeConfigsRejectsPeerPrimaryCoin(t *testing.T) {
	cfg := &Config{Strategies: []StrategyConfig{
		withHedge(hedgePerpsStrategy("eth-long", "ETH"), &HedgeConfig{Enabled: true, Symbol: "BTC"}),
		hedgePerpsStrategy("btc-long", "BTC"),
	}}
	errs := validateHedgeConfigs(cfg)
	if !hedgeErrsContaining(errs, "is the primary coin of strategy/strategies btc-long") {
		t.Fatalf("expected peer-primary rejection, got %v", errs)
	}
}

// Paper peers count. A paper strategy on the hedge coin today becomes a live
// peer the moment its mode flips, and nothing would re-validate at that point.
func TestValidateHedgeConfigsRejectsPaperPeerPrimaryCoin(t *testing.T) {
	paper := hedgePerpsStrategy("btc-paper", "BTC")
	paper.Args = []string{"--symbol", "BTC", "--mode", "paper"}
	cfg := &Config{Strategies: []StrategyConfig{
		withHedge(hedgePerpsStrategy("eth-long", "ETH"), &HedgeConfig{Enabled: true, Symbol: "BTC"}),
		paper,
	}}
	errs := validateHedgeConfigs(cfg)
	if !hedgeErrsContaining(errs, "is the primary coin of strategy/strategies btc-paper") {
		t.Fatalf("expected paper-peer rejection, got %v", errs)
	}
}

// Manual strategies share the same on-chain coin aggregation, so they count too.
func TestValidateHedgeConfigsRejectsManualPeerPrimaryCoin(t *testing.T) {
	manual := StrategyConfig{ID: "btc-manual", Type: "manual", Platform: "hyperliquid", Symbol: "BTC"}
	cfg := &Config{Strategies: []StrategyConfig{
		withHedge(hedgePerpsStrategy("eth-long", "ETH"), &HedgeConfig{Enabled: true, Symbol: "BTC"}),
		manual,
	}}
	errs := validateHedgeConfigs(cfg)
	if !hedgeErrsContaining(errs, "is the primary coin of strategy/strategies btc-manual") {
		t.Fatalf("expected manual-peer rejection, got %v", errs)
	}
}

func TestValidateHedgeConfigsRejectsHedgeVsHedgeCollision(t *testing.T) {
	cfg := &Config{Strategies: []StrategyConfig{
		withHedge(hedgePerpsStrategy("eth-long", "ETH"), &HedgeConfig{Enabled: true, Symbol: "BTC"}),
		withHedge(hedgePerpsStrategy("sol-long", "SOL"), &HedgeConfig{Enabled: true, Symbol: "btc"}),
	}}
	errs := validateHedgeConfigs(cfg)
	if !hedgeErrsContaining(errs, "claimed by multiple hedge-enabled strategies (eth-long, sol-long)") {
		t.Fatalf("expected hedge-vs-hedge rejection, got %v", errs)
	}
}

func TestValidateHedgeConfigsRejectsDirectionBoth(t *testing.T) {
	sc := hedgePerpsStrategy("eth-both", "ETH")
	sc.Direction = DirectionBoth
	cfg := &Config{Strategies: []StrategyConfig{
		withHedge(sc, &HedgeConfig{Enabled: true, Symbol: "BTC"}),
	}}
	errs := validateHedgeConfigs(cfg)
	if !hedgeErrsContaining(errs, "hedge is not supported with direction") {
		t.Fatalf("expected direction=both rejection, got %v", errs)
	}
}

// Legacy allow_shorts=true resolves to direction "both" via EffectiveDirection,
// so it must be caught by the same gate — otherwise the reject is bypassable
// with a pre-v14 field.
func TestValidateHedgeConfigsRejectsLegacyAllowShortsBoth(t *testing.T) {
	sc := hedgePerpsStrategy("eth-legacy", "ETH")
	sc.AllowShorts = true
	if EffectiveDirection(sc) != DirectionBoth {
		t.Skipf("allow_shorts no longer resolves to %q; gate is covered by the direction test", DirectionBoth)
	}
	cfg := &Config{Strategies: []StrategyConfig{
		withHedge(sc, &HedgeConfig{Enabled: true, Symbol: "BTC"}),
	}}
	errs := validateHedgeConfigs(cfg)
	if !hedgeErrsContaining(errs, "hedge is not supported with direction") {
		t.Fatalf("expected legacy allow_shorts rejection, got %v", errs)
	}
}

func TestValidateHedgeConfigsRejectsNonPerpsAndNonHyperliquid(t *testing.T) {
	spot := StrategyConfig{ID: "spot-x", Type: "spot", Platform: "binanceus"}
	cfg := &Config{Strategies: []StrategyConfig{
		withHedge(spot, &HedgeConfig{Enabled: true, Symbol: "BTC"}),
	}}
	errs := validateHedgeConfigs(cfg)
	if !hedgeErrsContaining(errs, "only supported for perps strategies") {
		t.Fatalf("expected type rejection, got %v", errs)
	}
	if !hedgeErrsContaining(errs, "only supported on hyperliquid") {
		t.Fatalf("expected platform rejection, got %v", errs)
	}
}

func TestValidateHedgeConfigsRejectsBadVocabularyAndBounds(t *testing.T) {
	cases := []struct {
		name   string
		hedge  *HedgeConfig
		needle string
	}{
		{"side", &HedgeConfig{Enabled: true, Symbol: "BTC", Side: "same"}, "hedge.side must be empty or"},
		{"platform", &HedgeConfig{Enabled: true, Symbol: "BTC", Platform: "okx"}, "hedge.platform must be empty or"},
		{"type", &HedgeConfig{Enabled: true, Symbol: "BTC", Type: "spot"}, "hedge.type must be empty or"},
		{"ratio-high", &HedgeConfig{Enabled: true, Symbol: "BTC", Ratio: 100}, "hedge.ratio must be in"},
		{"ratio-negative", &HedgeConfig{Enabled: true, Symbol: "BTC", Ratio: -1}, "hedge.ratio must be in"},
		{"leverage", &HedgeConfig{Enabled: true, Symbol: "BTC", Leverage: 1000}, "hedge.leverage must be in"},
		{"margin", &HedgeConfig{Enabled: true, Symbol: "BTC", MarginMode: "portfolio"}, "hedge.margin_mode must be"},
		{"symbol", &HedgeConfig{Enabled: true, Symbol: "  "}, "hedge.symbol is required"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &Config{Strategies: []StrategyConfig{
				withHedge(hedgePerpsStrategy("eth-long", "ETH"), tc.hedge),
			}}
			errs := validateHedgeConfigs(cfg)
			if !hedgeErrsContaining(errs, tc.needle) {
				t.Fatalf("expected %q, got %v", tc.needle, errs)
			}
		})
	}
}

// A disabled block is inert: it must pass the shape checks but must NOT be
// subject to the ownership/collision rules, so an operator can park a hedge
// block on a strategy while its coin is temporarily claimed elsewhere.
func TestValidateHedgeConfigsDisabledBlockSkipsCollisionsButKeepsShapeChecks(t *testing.T) {
	cfg := &Config{Strategies: []StrategyConfig{
		withHedge(hedgePerpsStrategy("eth-long", "ETH"), &HedgeConfig{Enabled: false, Symbol: "ETH", Side: "same"}),
		hedgePerpsStrategy("btc-long", "BTC"),
	}}
	errs := validateHedgeConfigs(cfg)
	if hedgeErrsContaining(errs, "is the strategy's own coin") {
		t.Fatalf("disabled block must not trip collision rules, got %v", errs)
	}
	if !hedgeErrsContaining(errs, "hedge.side must be empty or") {
		t.Fatalf("disabled block must still be shape-checked, got %v", errs)
	}
}

func TestHedgeAccessorDefaults(t *testing.T) {
	sc := withHedge(hedgePerpsStrategy("eth", "ETH"), &HedgeConfig{Enabled: true, Symbol: "btc/USDC:USDC"})
	if got := hedgeCoin(sc); got != "BTC" {
		t.Fatalf("hedgeCoin = %q, want BTC", got)
	}
	if got := hedgeRatio(sc); got != 1 {
		t.Fatalf("hedgeRatio default = %v, want 1", got)
	}
	if got := hedgeLeverage(sc); got != 1 {
		t.Fatalf("hedgeLeverage default = %v, want 1", got)
	}
	if got := hedgeMarginMode(sc); got != "isolated" {
		t.Fatalf("hedgeMarginMode default = %q, want isolated", got)
	}
	// A disabled block must resolve to no coin so every gated call site
	// (marks, kill switch, reconcile) collapses to "no hedge" on one check.
	sc.Hedge.Enabled = false
	if got := hedgeCoin(sc); got != "" {
		t.Fatalf("hedgeCoin on disabled block = %q, want empty", got)
	}
	if HedgeEnabled(sc) {
		t.Fatal("HedgeEnabled must be false for a disabled block")
	}
	sc.Hedge = nil
	if HedgeEnabled(sc) || hedgeCoin(sc) != "" {
		t.Fatal("nil hedge block must resolve to disabled/no-coin")
	}
}

func TestHedgeSideForPrimary(t *testing.T) {
	if got := HedgeSideForPrimary("long"); got != "short" {
		t.Fatalf("long → %q, want short", got)
	}
	if got := HedgeSideForPrimary("short"); got != "long" {
		t.Fatalf("short → %q, want long", got)
	}
	// An unknown side must NOT be guessed — a wrong guess doubles exposure.
	if got := HedgeSideForPrimary(""); got != "" {
		t.Fatalf("unknown side → %q, want empty (fail closed)", got)
	}
}

// A typo inside the hedge block must fail loudly. Silently defaulting
// `"ration": 0.5` to ratio 1.0 would open a hedge at twice the intended size.
func TestValidateStrategyJSONKeysFlagsUnknownHedgeKeys(t *testing.T) {
	raw := []byte(`{"strategies":[{"id":"eth-long","hedge":{"enabled":true,"symbol":"BTC","ration":0.5}}]}`)
	errs := validateStrategyJSONKeys(raw)
	if !hedgeErrsContaining(errs, `strategy[eth-long]: unknown field "ration" in hedge block`) {
		t.Fatalf("expected unknown hedge key error, got %v", errs)
	}
}

func TestValidateStrategyJSONKeysAcceptsKnownHedgeKeys(t *testing.T) {
	raw := []byte(`{"strategies":[{"id":"eth-long","hedge":{"enabled":true,"symbol":"BTC","side":"inverse","ratio":1,"platform":"hyperliquid","type":"perps","margin_mode":"cross","leverage":3}}]}`)
	if errs := validateStrategyJSONKeys(raw); len(errs) != 0 {
		t.Fatalf("expected no unknown-key errors, got %v", errs)
	}
}

// The top-level "hedge" key must be recognized by the reflective strategy-key
// guard, or every hedge config would be rejected as an unknown field.
func TestKnownStrategyConfigKeysIncludesHedge(t *testing.T) {
	if !knownStrategyConfigKeys()["hedge"] {
		t.Fatal("knownStrategyConfigKeys must include \"hedge\"")
	}
}

func TestHedgeConfigEqual(t *testing.T) {
	a := &HedgeConfig{Enabled: true, Symbol: "BTC", Ratio: 1}
	b := &HedgeConfig{Enabled: true, Symbol: "BTC", Ratio: 1}
	if !hedgeConfigEqual(a, b) {
		t.Fatal("identical blocks must compare equal")
	}
	if hedgeConfigEqual(a, nil) || hedgeConfigEqual(nil, a) {
		t.Fatal("adding/removing the block must compare unequal")
	}
	if !hedgeConfigEqual(nil, nil) {
		t.Fatal("nil/nil must compare equal")
	}
	c := &HedgeConfig{Enabled: true, Symbol: "BTC", Ratio: 0.5}
	if hedgeConfigEqual(a, c) {
		t.Fatal("ratio change must compare unequal")
	}
}

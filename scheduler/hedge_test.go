package main

import (
	"strings"
	"sync"
	"testing"
)

// --- pure helpers (hedge.go) ---

func TestHedgeOpenQtyNotionalSizing(t *testing.T) {
	// $10,000 primary fill (1 BTC @ $10k) hedged 1:1 into a $2000 hedge coin
	// mid should size to 5 units.
	qty, ok := hedgeOpenQty(1, 10000, 1.0, 2000)
	if !ok {
		t.Fatalf("expected ok=true")
	}
	if qty != 5 {
		t.Errorf("qty = %g, want 5", qty)
	}

	// Ratio 0.5 halves the hedge notional.
	qty, ok = hedgeOpenQty(1, 10000, 0.5, 2000)
	if !ok || qty != 2.5 {
		t.Errorf("qty = %g ok=%v, want 2.5/true", qty, ok)
	}
}

func TestHedgeOpenQtyRejectsNonPositiveInputs(t *testing.T) {
	cases := []struct {
		name                string
		qty, px, ratio, mid float64
	}{
		{"zero qty", 0, 100, 1, 100},
		{"negative qty", -1, 100, 1, 100},
		{"zero price", 1, 0, 1, 100},
		{"zero ratio", 1, 100, 0, 100},
		{"zero mid", 1, 100, 1, 0},
		{"negative mid", 1, 100, 1, -5},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, ok := hedgeOpenQty(tc.qty, tc.px, tc.ratio, tc.mid); ok {
				t.Errorf("expected ok=false")
			}
		})
	}
}

func TestHedgeReduceQtyProportional(t *testing.T) {
	// Primary closes 25% (2.5 of 10) -> hedge (5) reduces by 25% (1.25).
	got := hedgeReduceQty(5, 10, 2.5)
	if got != 1.25 {
		t.Errorf("got %g, want 1.25", got)
	}
}

func TestHedgeReduceQtyFullCloseNoDust(t *testing.T) {
	// A full close (closedQty == primaryQtyBefore) must return the FULL
	// hedge qty, not a value that leaves dust from float rounding.
	got := hedgeReduceQty(3.333333, 10, 10)
	if got != 3.333333 {
		t.Errorf("got %g, want the full hedge qty 3.333333 (no dust residue)", got)
	}
}

func TestHedgeReduceQtyNeverExceedsHedgeQty(t *testing.T) {
	// closedQty slightly exceeding primaryQtyBefore (rounding on the caller
	// side) must never compute a reduce amount larger than the hedge holds.
	got := hedgeReduceQty(5, 10, 10.0001)
	if got != 5 {
		t.Errorf("got %g, want capped at hedge qty 5", got)
	}
}

func TestHedgeReduceQtyRejectsNonPositiveInputs(t *testing.T) {
	if got := hedgeReduceQty(0, 10, 5); got != 0 {
		t.Errorf("zero hedge qty: got %g, want 0", got)
	}
	if got := hedgeReduceQty(5, 0, 5); got != 0 {
		t.Errorf("zero primaryQtyBefore: got %g, want 0", got)
	}
	if got := hedgeReduceQty(5, 10, 0); got != 0 {
		t.Errorf("zero primaryQtyClosed: got %g, want 0", got)
	}
}

func TestHedgeSideForPrimaryInverseMapping(t *testing.T) {
	if got := hedgeSideForPrimary("buy"); got != "sell" {
		t.Errorf("buy -> %q, want sell", got)
	}
	if got := hedgeSideForPrimary("sell"); got != "buy" {
		t.Errorf("sell -> %q, want buy", got)
	}
}

func TestHedgeOrderSkipReason(t *testing.T) {
	if r := hedgeOrderSkipReason(1, 100); r != "" {
		t.Errorf("valid order: want no skip reason, got %q", r)
	}
	if r := hedgeOrderSkipReason(0, 100); r == "" {
		t.Errorf("zero qty: want a skip reason")
	}
	if r := hedgeOrderSkipReason(1, 0); r == "" {
		t.Errorf("zero mid: want a skip reason")
	}
}

// --- accessors (config.go) ---

func TestHedgeCoinNormalization(t *testing.T) {
	sc := StrategyConfig{Hedge: &HedgeConfig{Enabled: true, Symbol: "BTC/USDC:USDC"}}
	if got := hedgeCoin(sc); got != "BTC" {
		t.Errorf("hedgeCoin(%q) = %q, want BTC", sc.Hedge.Symbol, got)
	}
	sc2 := StrategyConfig{Hedge: &HedgeConfig{Enabled: true, Symbol: " btc "}}
	if got := hedgeCoin(sc2); got != "BTC" {
		t.Errorf("hedgeCoin(%q) = %q, want BTC", sc2.Hedge.Symbol, got)
	}
}

func TestHedgeRatioDefault(t *testing.T) {
	sc := StrategyConfig{Hedge: &HedgeConfig{Enabled: true, Symbol: "BTC"}}
	if got := hedgeRatio(sc); got != 1.0 {
		t.Errorf("default ratio = %g, want 1.0", got)
	}
	sc.Hedge.Ratio = 0.5
	if got := hedgeRatio(sc); got != 0.5 {
		t.Errorf("explicit ratio = %g, want 0.5", got)
	}
}

func TestHedgeExchangeLeverageDefault(t *testing.T) {
	sc := StrategyConfig{Hedge: &HedgeConfig{Enabled: true, Symbol: "BTC"}}
	if got := hedgeExchangeLeverage(sc); got != 1 {
		t.Errorf("default leverage = %g, want 1", got)
	}
}

func TestHedgeMarginModeDefault(t *testing.T) {
	sc := StrategyConfig{Hedge: &HedgeConfig{Enabled: true, Symbol: "BTC"}}
	if got := hedgeMarginMode(sc); got != "isolated" {
		t.Errorf("default margin mode = %q, want isolated", got)
	}
}

// --- config validation (config.go validateConfig + hyperliquidHedgeCollisionErrors) ---

func hedgeTestStrategy(id, coin string, hedge *HedgeConfig) StrategyConfig {
	return StrategyConfig{
		ID: id, Type: "perps", Platform: "hyperliquid", Script: "shared_scripts/check_hyperliquid.py",
		Args: []string{"sma_crossover", coin, "1h", "--mode=paper"}, Capital: 1000, MaxDrawdownPct: 10,
		Hedge: hedge,
	}
}

func TestValidateConfig_HedgeSymbolEqualsOwnCoin(t *testing.T) {
	cfg := &Config{IntervalSeconds: 600, Strategies: []StrategyConfig{
		hedgeTestStrategy("eth-long", "ETH", &HedgeConfig{Enabled: true, Symbol: "ETH"}),
	}}
	err := validateConfig(cfg, true)
	if err == nil || !strings.Contains(err.Error(), "must not equal the strategy's own primary coin") {
		t.Fatalf("want same-coin rejection, got: %v", err)
	}
}

func TestValidateConfig_HedgeSymbolEqualsPeerCoin(t *testing.T) {
	cfg := &Config{IntervalSeconds: 600, Strategies: []StrategyConfig{
		hedgeTestStrategy("eth-long", "ETH", &HedgeConfig{Enabled: true, Symbol: "BTC"}),
		hedgeTestStrategy("btc-long", "BTC", nil),
	}}
	err := validateConfig(cfg, true)
	if err == nil || !strings.Contains(err.Error(), "collides with the primary coin") {
		t.Fatalf("want peer-coin collision rejection, got: %v", err)
	}
}

func TestValidateConfig_HedgeVsHedgeCollision(t *testing.T) {
	cfg := &Config{IntervalSeconds: 600, Strategies: []StrategyConfig{
		hedgeTestStrategy("eth-long", "ETH", &HedgeConfig{Enabled: true, Symbol: "SOL"}),
		hedgeTestStrategy("btc-long", "BTC", &HedgeConfig{Enabled: true, Symbol: "SOL"}),
	}}
	err := validateConfig(cfg, true)
	if err == nil || !strings.Contains(err.Error(), "configured as a hedge coin by multiple strategies") {
		t.Fatalf("want hedge-vs-hedge collision rejection, got: %v", err)
	}
}

func TestValidateConfig_HedgeValidConfigLoadsClean(t *testing.T) {
	cfg := &Config{IntervalSeconds: 600, Strategies: []StrategyConfig{
		hedgeTestStrategy("eth-long", "ETH", &HedgeConfig{Enabled: true, Symbol: "BTC", Ratio: 1.0, MarginMode: "cross", Leverage: 3}),
	}}
	if err := validateConfig(cfg, true); err != nil {
		t.Fatalf("want clean load, got: %v", err)
	}
}

func TestValidateConfig_HedgeOnManualTypeRejected(t *testing.T) {
	sc := StrategyConfig{
		ID: "manual-eth", Type: "manual", Platform: "hyperliquid", Symbol: "ETH", Timeframe: "1h",
		Leverage: 5, Hedge: &HedgeConfig{Enabled: true, Symbol: "BTC"},
	}
	cfg := &Config{IntervalSeconds: 600, Strategies: []StrategyConfig{sc}}
	err := validateConfig(cfg, true)
	if err == nil || !strings.Contains(err.Error(), "hedge is only supported for HL perps strategies") {
		t.Fatalf("want manual-type rejection, got: %v", err)
	}
}

func TestValidateConfig_HedgeRatioBounds(t *testing.T) {
	cfg := &Config{IntervalSeconds: 600, Strategies: []StrategyConfig{
		hedgeTestStrategy("eth-long", "ETH", &HedgeConfig{Enabled: true, Symbol: "BTC", Ratio: 11}),
	}}
	err := validateConfig(cfg, true)
	if err == nil || !strings.Contains(err.Error(), "hedge.ratio must be in") {
		t.Fatalf("want ratio-bound rejection, got: %v", err)
	}
}

func TestValidateConfig_HedgeSymbolNormalizedForm(t *testing.T) {
	cfg := &Config{IntervalSeconds: 600, Strategies: []StrategyConfig{
		hedgeTestStrategy("eth-long", "ETH", &HedgeConfig{Enabled: true, Symbol: "BTC/USDC:USDC"}),
	}}
	if err := validateConfig(cfg, true); err != nil {
		t.Fatalf("want clean load for perps-market hedge symbol form, got: %v", err)
	}
}

// --- hot reload (config_reload.go) ---

func TestValidateHotReloadStateCompatible_HedgeBlockedWhileOpen(t *testing.T) {
	mkCfg := func(hedge *HedgeConfig) *Config {
		sc := StrategyConfig{
			ID: "hl-eth", Type: "perps", Platform: "hyperliquid", Script: "x.py",
			Args: []string{"a", "ETH", "1h"}, Capital: 1000, MaxDrawdownPct: 10,
			Leverage: 5, MarginMode: "isolated", Hedge: hedge,
		}
		return minimalReloadConfig([]StrategyConfig{sc})
	}
	openState := &AppState{Strategies: map[string]*StrategyState{
		"hl-eth": {ID: "hl-eth", Positions: map[string]*Position{
			"ETH": {Symbol: "ETH", Quantity: 0.5, AvgCost: 3000, Side: "long"},
		}},
	}}
	flatState := &AppState{Strategies: map[string]*StrategyState{
		"hl-eth": {ID: "hl-eth", Positions: map[string]*Position{}},
	}}

	err := validateHotReloadStateCompatible(mkCfg(nil), mkCfg(&HedgeConfig{Enabled: true, Symbol: "BTC"}), openState)
	if err == nil || !strings.Contains(err.Error(), "hedge block changed with open positions") {
		t.Fatalf("open primary: want hedge-block-changed rejection, got: %v", err)
	}

	if err := validateHotReloadStateCompatible(mkCfg(nil), mkCfg(&HedgeConfig{Enabled: true, Symbol: "BTC"}), flatState); err != nil {
		t.Fatalf("flat: want no error, got: %v", err)
	}
}

func TestValidateHotReloadStateCompatible_HedgeBlockedWhileOnlyHedgeLegOpen(t *testing.T) {
	// #1159: strategyHasOpenPositions must see the hedge Position too — it
	// lives in the same Positions map as the primary — so a hedge edit is
	// blocked even when the PRIMARY coin key shows nothing (e.g. a stale
	// partial state during repair).
	mkCfg := func(hedge *HedgeConfig) *Config {
		sc := StrategyConfig{
			ID: "hl-eth", Type: "perps", Platform: "hyperliquid", Script: "x.py",
			Args: []string{"a", "ETH", "1h"}, Capital: 1000, MaxDrawdownPct: 10,
			Leverage: 5, MarginMode: "isolated", Hedge: hedge,
		}
		return minimalReloadConfig([]StrategyConfig{sc})
	}
	hedgeOnlyOpenState := &AppState{Strategies: map[string]*StrategyState{
		"hl-eth": {ID: "hl-eth", Positions: map[string]*Position{
			"BTC": {Symbol: "BTC", Quantity: 0.1, AvgCost: 50000, Side: "short", IsHedge: true, HedgeForSymbol: "ETH"},
		}},
	}}
	err := validateHotReloadStateCompatible(mkCfg(&HedgeConfig{Enabled: true, Symbol: "BTC"}), mkCfg(&HedgeConfig{Enabled: true, Symbol: "SOL"}), hedgeOnlyOpenState)
	if err == nil || !strings.Contains(err.Error(), "hedge block changed with open positions") {
		t.Fatalf("hedge-only open: want hedge-block-changed rejection, got: %v", err)
	}
}

func TestHedgeConfigEqual(t *testing.T) {
	a := &HedgeConfig{Enabled: true, Symbol: "BTC", Ratio: 1}
	b := &HedgeConfig{Enabled: true, Symbol: "BTC", Ratio: 1}
	c := &HedgeConfig{Enabled: true, Symbol: "SOL", Ratio: 1}
	if !hedgeConfigEqual(a, b) {
		t.Errorf("want equal")
	}
	if hedgeConfigEqual(a, c) {
		t.Errorf("want not equal")
	}
	if !hedgeConfigEqual(nil, nil) {
		t.Errorf("nil==nil want equal")
	}
	if hedgeConfigEqual(a, nil) {
		t.Errorf("a!=nil want not equal")
	}
}

// --- startup drift (hedge.go checkHedgeStateDriftAtStartup) ---

func TestCheckHedgeStateDriftAtStartup_EnabledButPrimaryUnpaired(t *testing.T) {
	cfg := &Config{Strategies: []StrategyConfig{
		hedgeTestStrategy("eth-long", "ETH", &HedgeConfig{Enabled: true, Symbol: "BTC"}),
	}}
	state := &AppState{Strategies: map[string]*StrategyState{
		"eth-long": {ID: "eth-long", Positions: map[string]*Position{
			"ETH": {Symbol: "ETH", Quantity: 1, AvgCost: 3000, Side: "long"}, // no HedgeSymbol stamp
		}},
	}}
	warnings := checkHedgeStateDriftAtStartup(cfg, state)
	if len(warnings) != 1 || !strings.Contains(warnings[0], "carries no hedge pairing") {
		t.Fatalf("want one unpaired-primary warning, got: %v", warnings)
	}
}

func TestCheckHedgeStateDriftAtStartup_DisabledButHedgeLegPersisted(t *testing.T) {
	cfg := &Config{Strategies: []StrategyConfig{
		hedgeTestStrategy("eth-long", "ETH", nil),
	}}
	state := &AppState{Strategies: map[string]*StrategyState{
		"eth-long": {ID: "eth-long", Positions: map[string]*Position{
			"BTC": {Symbol: "BTC", Quantity: 0.1, AvgCost: 50000, Side: "short", IsHedge: true, HedgeForSymbol: "ETH"},
		}},
	}}
	warnings := checkHedgeStateDriftAtStartup(cfg, state)
	if len(warnings) != 1 || !strings.Contains(warnings[0], "is disabled/absent but an open hedge leg") {
		t.Fatalf("want one orphaned-hedge warning, got: %v", warnings)
	}
}

func TestCheckHedgeStateDriftAtStartup_SymbolEditedWhileOpen(t *testing.T) {
	cfg := &Config{Strategies: []StrategyConfig{
		hedgeTestStrategy("eth-long", "ETH", &HedgeConfig{Enabled: true, Symbol: "SOL"}),
	}}
	state := &AppState{Strategies: map[string]*StrategyState{
		"eth-long": {ID: "eth-long", Positions: map[string]*Position{
			"ETH": {Symbol: "ETH", Quantity: 1, AvgCost: 3000, Side: "long", HedgeSymbol: "BTC"},
			"BTC": {Symbol: "BTC", Quantity: 0.1, AvgCost: 50000, Side: "short", IsHedge: true, HedgeForSymbol: "ETH"},
		}},
	}}
	warnings := checkHedgeStateDriftAtStartup(cfg, state)
	if len(warnings) != 1 || !strings.Contains(warnings[0], "is now \"SOL\" but an open hedge leg on \"BTC\"") {
		t.Fatalf("want symbol-edited warning, got: %v", warnings)
	}
}

func TestCheckHedgeStateDriftAtStartup_ConsistentPairNoWarning(t *testing.T) {
	cfg := &Config{Strategies: []StrategyConfig{
		hedgeTestStrategy("eth-long", "ETH", &HedgeConfig{Enabled: true, Symbol: "BTC"}),
	}}
	state := &AppState{Strategies: map[string]*StrategyState{
		"eth-long": {ID: "eth-long", Positions: map[string]*Position{
			"ETH": {Symbol: "ETH", Quantity: 1, AvgCost: 3000, Side: "long", HedgeSymbol: "BTC"},
			"BTC": {Symbol: "BTC", Quantity: 0.1, AvgCost: 50000, Side: "short", IsHedge: true, HedgeForSymbol: "ETH"},
		}},
	}}
	if warnings := checkHedgeStateDriftAtStartup(cfg, state); len(warnings) != 0 {
		t.Fatalf("want no warnings, got: %v", warnings)
	}
}

// --- coherence sweep detection (hedge_sweep.go) ---

func TestSnapshotHedgeCoherenceJobs_PrimaryWithoutHedge(t *testing.T) {
	sc := hedgeTestStrategy("eth-long", "ETH", &HedgeConfig{Enabled: true, Symbol: "BTC"})
	sc.Args = []string{"sma_crossover", "ETH", "1h", "--mode=live"}
	state := &AppState{Strategies: map[string]*StrategyState{
		"eth-long": {ID: "eth-long", Positions: map[string]*Position{
			"ETH": {Symbol: "ETH", Quantity: 1, AvgCost: 3000, Side: "long"},
		}},
	}}
	jobs := snapshotHedgeCoherenceJobs(state, []StrategyConfig{sc}, []StrategyConfig{sc}, nil)
	if len(jobs) != 1 || jobs[0].Action != hedgeSweepClosePrimary {
		t.Fatalf("want one close_primary job, got: %+v", jobs)
	}
	if jobs[0].Qty != 1 {
		t.Errorf("Qty = %g, want 1", jobs[0].Qty)
	}
	if jobs[0].PrimaryShared {
		t.Errorf("PrimaryShared = true, want false (sole owner)")
	}
}

func TestSnapshotHedgeCoherenceJobs_HedgeWithoutPrimary(t *testing.T) {
	sc := hedgeTestStrategy("eth-long", "ETH", &HedgeConfig{Enabled: true, Symbol: "BTC"})
	sc.Args = []string{"sma_crossover", "ETH", "1h", "--mode=live"}
	state := &AppState{Strategies: map[string]*StrategyState{
		"eth-long": {ID: "eth-long", Positions: map[string]*Position{
			"BTC": {Symbol: "BTC", Quantity: 0.1, AvgCost: 50000, Side: "short", IsHedge: true, HedgeForSymbol: "ETH"},
		}},
	}}
	jobs := snapshotHedgeCoherenceJobs(state, []StrategyConfig{sc}, []StrategyConfig{sc}, nil)
	if len(jobs) != 1 || jobs[0].Action != hedgeSweepCloseHedge {
		t.Fatalf("want one close_hedge job, got: %+v", jobs)
	}
}

func TestSnapshotHedgeCoherenceJobs_BothAbsentNoJob(t *testing.T) {
	sc := hedgeTestStrategy("eth-long", "ETH", &HedgeConfig{Enabled: true, Symbol: "BTC"})
	sc.Args = []string{"sma_crossover", "ETH", "1h", "--mode=live"}
	state := &AppState{Strategies: map[string]*StrategyState{
		"eth-long": {ID: "eth-long", Positions: map[string]*Position{}},
	}}
	if jobs := snapshotHedgeCoherenceJobs(state, []StrategyConfig{sc}, []StrategyConfig{sc}, nil); len(jobs) != 0 {
		t.Fatalf("want no jobs, got: %+v", jobs)
	}
}

func TestSnapshotHedgeCoherenceJobs_OversizedHedgeReduces(t *testing.T) {
	sc := hedgeTestStrategy("eth-long", "ETH", &HedgeConfig{Enabled: true, Symbol: "BTC", Ratio: 1.0})
	sc.Args = []string{"sma_crossover", "ETH", "1h", "--mode=live"}
	state := &AppState{Strategies: map[string]*StrategyState{
		"eth-long": {ID: "eth-long", Positions: map[string]*Position{
			"ETH": {Symbol: "ETH", Quantity: 1, AvgCost: 2000, Side: "long"},
			// Target hedge qty at these marks: (1*2000*1.0)/50000 = 0.04. Booked
			// hedge holds 0.1 — well beyond the dust tolerance.
			"BTC": {Symbol: "BTC", Quantity: 0.1, AvgCost: 50000, Side: "short", IsHedge: true, HedgeForSymbol: "ETH"},
		}},
	}}
	prices := map[string]float64{"ETH": 2000, "BTC": 50000}
	jobs := snapshotHedgeCoherenceJobs(state, []StrategyConfig{sc}, []StrategyConfig{sc}, prices)
	if len(jobs) != 1 || jobs[0].Action != hedgeSweepReduceHedge {
		t.Fatalf("want one reduce_hedge job, got: %+v", jobs)
	}
	wantReduce := 0.1 - 0.04
	if diff := jobs[0].Qty - wantReduce; diff > 1e-9 || diff < -1e-9 {
		t.Errorf("Qty = %g, want %g", jobs[0].Qty, wantReduce)
	}
}

func TestSnapshotHedgeCoherenceJobs_ConsistentPairNoJob(t *testing.T) {
	sc := hedgeTestStrategy("eth-long", "ETH", &HedgeConfig{Enabled: true, Symbol: "BTC", Ratio: 1.0})
	sc.Args = []string{"sma_crossover", "ETH", "1h", "--mode=live"}
	state := &AppState{Strategies: map[string]*StrategyState{
		"eth-long": {ID: "eth-long", Positions: map[string]*Position{
			"ETH": {Symbol: "ETH", Quantity: 1, AvgCost: 2000, Side: "long"},
			"BTC": {Symbol: "BTC", Quantity: 0.04, AvgCost: 50000, Side: "short", IsHedge: true, HedgeForSymbol: "ETH"},
		}},
	}}
	prices := map[string]float64{"ETH": 2000, "BTC": 50000}
	if jobs := snapshotHedgeCoherenceJobs(state, []StrategyConfig{sc}, []StrategyConfig{sc}, prices); len(jobs) != 0 {
		t.Fatalf("want no jobs for a consistent pair, got: %+v", jobs)
	}
}

// --- PR #1337 review fixes ---

// applyHedgeOpenFill must never blend a new-side fill into an existing
// opposite-side hedge Position (a flip whose close leg failed must not
// corrupt the surviving hedge into a wrong-side/wrong-qty Position).
func TestApplyHedgeOpenFill_RefusesOppositeSideBlend(t *testing.T) {
	sc := hedgeTestStrategy("eth-long", "ETH", &HedgeConfig{Enabled: true, Symbol: "BTC"})
	s := &StrategyState{
		ID: "eth-long", Platform: "hyperliquid", Cash: 1000,
		Positions: map[string]*Position{
			"BTC": {Symbol: "BTC", Quantity: 0.1, AvgCost: 50000, Side: "short", IsHedge: true, HedgeForSymbol: "ETH"},
		},
	}
	// New fill is "buy" (long) against an existing SHORT hedge Position.
	got := applyHedgeOpenFill(s, sc, "ETH", "posid-1", "buy", 0.05, 51000, 1.0, 0, "", false)
	if got != nil {
		t.Fatalf("want nil (refused), got %+v", got)
	}
	pos := s.Positions["BTC"]
	if pos.Quantity != 0.1 || pos.AvgCost != 50000 || pos.Side != "short" {
		t.Fatalf("existing opposite-side position must be untouched, got %+v", pos)
	}
	if len(s.TradeHistory) != 0 {
		t.Fatalf("want no trade recorded on refusal, got %d", len(s.TradeHistory))
	}
}

// Same-side blend (the ordinary scale-in-shaped add) must still work.
func TestApplyHedgeOpenFill_BlendsSameSide(t *testing.T) {
	sc := hedgeTestStrategy("eth-long", "ETH", &HedgeConfig{Enabled: true, Symbol: "BTC"})
	s := &StrategyState{
		ID: "eth-long", Platform: "hyperliquid", Cash: 1000,
		Positions: map[string]*Position{
			"BTC": {Symbol: "BTC", Quantity: 0.1, AvgCost: 50000, Side: "short", IsHedge: true, HedgeForSymbol: "ETH"},
		},
	}
	got := applyHedgeOpenFill(s, sc, "ETH", "posid-1", "sell", 0.1, 52000, 1.0, 0, "", false)
	if got == nil {
		t.Fatalf("want a blended position, got nil")
	}
	if got.Quantity != 0.2 {
		t.Errorf("Quantity = %g, want 0.2", got.Quantity)
	}
	wantAvg := (50000*0.1 + 52000*0.1) / 0.2
	if got.AvgCost != wantAvg {
		t.Errorf("AvgCost = %g, want %g", got.AvgCost, wantAvg)
	}
	if got.Side != "short" {
		t.Errorf("Side = %q, want short", got.Side)
	}
}

// Fresh open (no existing hedge position) must still work.
func TestApplyHedgeOpenFill_CreatesFreshWhenNoneExists(t *testing.T) {
	sc := hedgeTestStrategy("eth-long", "ETH", &HedgeConfig{Enabled: true, Symbol: "BTC"})
	s := &StrategyState{
		ID: "eth-long", Platform: "hyperliquid", Cash: 1000,
		Positions: map[string]*Position{
			"ETH": {Symbol: "ETH", Quantity: 1, AvgCost: 3000, Side: "long"},
		},
	}
	got := applyHedgeOpenFill(s, sc, "ETH", "posid-1", "sell", 0.06, 50000, 1.0, 0, "", false)
	if got == nil {
		t.Fatalf("want a fresh hedge position, got nil")
	}
	if got.Quantity != 0.06 || got.Side != "short" || !got.IsHedge || got.HedgeForSymbol != "ETH" {
		t.Errorf("got %+v", got)
	}
	if primary := s.Positions["ETH"]; primary.HedgeSymbol != "BTC" {
		t.Errorf("primary.HedgeSymbol = %q, want BTC", primary.HedgeSymbol)
	}
}

// mirrorHedgeReduce must report success (true) on the two no-op paths that
// never place a live order, so a flip caller doesn't wrongly skip the
// reopen when there was nothing to close in the first place.
func TestMirrorHedgeReduce_NoHedgePositionReturnsTrue(t *testing.T) {
	sc := hedgeTestStrategy("eth-long", "ETH", &HedgeConfig{Enabled: true, Symbol: "BTC"})
	sc.Args = []string{"sma_crossover", "ETH", "1h", "--mode=live"}
	s := &StrategyState{ID: "eth-long", Platform: "hyperliquid", Positions: map[string]*Position{}}
	var mu sync.RWMutex
	if ok := mirrorHedgeReduce(sc, s, &mu, "ETH", 1, 1, "", 0, false, true, nil, nil); !ok {
		t.Fatalf("want true when there is no hedge position to reduce")
	}
}

// Paper-mode full reduce (closedQty == qtyBefore) must fully close the
// hedge position, report success, and never attempt a live order.
func TestMirrorHedgeReduce_PaperFullClose(t *testing.T) {
	sc := hedgeTestStrategy("eth-long", "ETH", &HedgeConfig{Enabled: true, Symbol: "BTC"})
	s := &StrategyState{
		ID: "eth-long", Platform: "hyperliquid", Cash: 1000,
		Positions: map[string]*Position{
			"ETH": {Symbol: "ETH", Quantity: 1, AvgCost: 3000, Side: "long"},
			"BTC": {Symbol: "BTC", Quantity: 0.05, AvgCost: 50000, Side: "short", IsHedge: true, HedgeForSymbol: "ETH"},
		},
	}
	var mu sync.RWMutex
	ok := mirrorHedgeReduce(sc, s, &mu, "ETH", 1, 1, "", 0, false, false, nil, nil)
	if !ok {
		t.Fatalf("want true (paper path never fails)")
	}
	if _, exists := s.Positions["BTC"]; exists {
		t.Errorf("want hedge position fully closed and removed")
	}
	if primary := s.Positions["ETH"]; primary.HedgeSymbol != "" {
		t.Errorf("want HedgeSymbol cleared on the primary after a full hedge close, got %q", primary.HedgeSymbol)
	}
}

// Paper-mode partial reduce must shrink (not remove) the hedge position.
func TestMirrorHedgeReduce_PaperPartialReduce(t *testing.T) {
	sc := hedgeTestStrategy("eth-long", "ETH", &HedgeConfig{Enabled: true, Symbol: "BTC"})
	s := &StrategyState{
		ID: "eth-long", Platform: "hyperliquid", Cash: 1000,
		Positions: map[string]*Position{
			"ETH": {Symbol: "ETH", Quantity: 1, AvgCost: 3000, Side: "long"},
			"BTC": {Symbol: "BTC", Quantity: 0.1, AvgCost: 50000, Side: "short", IsHedge: true, HedgeForSymbol: "ETH"},
		},
	}
	var mu sync.RWMutex
	// Primary closes 25% (0.25 of 1.0) -> hedge (0.1) reduces by 25% (0.025).
	ok := mirrorHedgeReduce(sc, s, &mu, "ETH", 1, 0.25, "", 0, false, false, nil, nil)
	if !ok {
		t.Fatalf("want true (paper path never fails)")
	}
	pos, exists := s.Positions["BTC"]
	if !exists {
		t.Fatalf("want hedge position to survive a partial reduce")
	}
	wantQty := 0.075
	if diff := pos.Quantity - wantQty; diff > 1e-9 || diff < -1e-9 {
		t.Errorf("Quantity = %g, want %g", pos.Quantity, wantQty)
	}
}

// onChainCoinQty (hedge_sweep.go) is the pure detector the crash-orphan
// recovery path uses to decide whether a real on-chain hedge position
// exists before fail-closing the primary.
func TestOnChainCoinQty(t *testing.T) {
	positions := []HLPosition{
		{Coin: "BTC", Size: -0.1},
		{Coin: "ETH", Size: 0},
		{Coin: "SOL", Size: 5},
	}
	if qty, found := onChainCoinQty(positions, "BTC"); !found || qty != 0.1 {
		t.Errorf("BTC: qty=%g found=%v, want 0.1/true (unsigned)", qty, found)
	}
	if _, found := onChainCoinQty(positions, "ETH"); found {
		t.Errorf("ETH: want not found (zero size)")
	}
	if _, found := onChainCoinQty(positions, "DOGE"); found {
		t.Errorf("DOGE: want not found (no entry)")
	}
	if qty, found := onChainCoinQty(positions, "SOL"); !found || qty != 5 {
		t.Errorf("SOL: qty=%g found=%v, want 5/true", qty, found)
	}
}

// --- collision helper (config.go) ---

func TestHyperliquidHedgeCollisionErrors_NoCollision(t *testing.T) {
	strategies := []StrategyConfig{
		hedgeTestStrategy("eth-long", "ETH", &HedgeConfig{Enabled: true, Symbol: "SOL"}),
		hedgeTestStrategy("btc-long", "BTC", nil),
	}
	if errs := hyperliquidHedgeCollisionErrors(strategies); len(errs) != 0 {
		t.Fatalf("want no collisions, got: %v", errs)
	}
}

package main

import (
	"reflect"
	"strings"
	"sync"
	"testing"
)

// ---------------------------------------------------------------------------
// Config validation (#1159 phase-1 constraint matrix)

func hedgeTestStrategy(id, coin string, hedge *HedgeConfig) StrategyConfig {
	return StrategyConfig{
		ID:       id,
		Type:     "perps",
		Platform: "hyperliquid",
		Script:   "shared_scripts/check_hyperliquid.py",
		Args:     []string{"ema_crossover", coin, "1h", "--mode=live"},
		Hedge:    hedge,
	}
}

func hedgeCfg(strategies ...StrategyConfig) *Config {
	return &Config{Strategies: strategies}
}

func TestValidateHedgeConfigs_RejectMatrix(t *testing.T) {
	cases := []struct {
		name string
		cfg  *Config
		want string // substring of the expected error; "" = no errors
	}{
		{
			name: "valid minimal block",
			cfg:  hedgeCfg(hedgeTestStrategy("a", "ETH", &HedgeConfig{Enabled: true, Symbol: "BTC"})),
			want: "",
		},
		{
			name: "ccxt symbol normalizes",
			cfg:  hedgeCfg(hedgeTestStrategy("a", "ETH", &HedgeConfig{Enabled: true, Symbol: "BTC/USDC:USDC", Side: "inverse", Ratio: 1.5, MarginMode: "cross", Leverage: 3})),
			want: "",
		},
		{
			name: "own coin collision",
			cfg:  hedgeCfg(hedgeTestStrategy("a", "ETH", &HedgeConfig{Enabled: true, Symbol: "ETH"})),
			want: "own coin",
		},
		{
			name: "peer configured-coin collision",
			cfg: hedgeCfg(
				hedgeTestStrategy("a", "ETH", &HedgeConfig{Enabled: true, Symbol: "BTC"}),
				hedgeTestStrategy("b", "BTC", nil),
			),
			want: "collides with strategy",
		},
		{
			name: "hedge-vs-hedge collision",
			cfg: hedgeCfg(
				hedgeTestStrategy("a", "ETH", &HedgeConfig{Enabled: true, Symbol: "BTC"}),
				hedgeTestStrategy("b", "SOL", &HedgeConfig{Enabled: true, Symbol: "BTC"}),
			),
			want: "already strategy",
		},
		{
			name: "disabled block does not claim a hedge coin",
			cfg: hedgeCfg(
				hedgeTestStrategy("a", "ETH", &HedgeConfig{Enabled: false, Symbol: "BTC"}),
				hedgeTestStrategy("b", "SOL", &HedgeConfig{Enabled: true, Symbol: "BTC"}),
			),
			want: "",
		},
		{
			name: "bad side",
			cfg:  hedgeCfg(hedgeTestStrategy("a", "ETH", &HedgeConfig{Enabled: true, Symbol: "BTC", Side: "same"})),
			want: "only \"inverse\"",
		},
		{
			name: "ratio out of bounds",
			cfg:  hedgeCfg(hedgeTestStrategy("a", "ETH", &HedgeConfig{Enabled: true, Symbol: "BTC", Ratio: 11})),
			want: "ratio",
		},
		{
			name: "bad margin mode",
			cfg:  hedgeCfg(hedgeTestStrategy("a", "ETH", &HedgeConfig{Enabled: true, Symbol: "BTC", MarginMode: "portfolio"})),
			want: "margin_mode",
		},
		{
			name: "bad platform",
			cfg:  hedgeCfg(hedgeTestStrategy("a", "ETH", &HedgeConfig{Enabled: true, Symbol: "BTC", Platform: "okx"})),
			want: "platform",
		},
		{
			name: "missing symbol",
			cfg:  hedgeCfg(hedgeTestStrategy("a", "ETH", &HedgeConfig{Enabled: true})),
			want: "symbol: required",
		},
		{
			name: "direction both rejected",
			cfg: func() *Config {
				sc := hedgeTestStrategy("a", "ETH", &HedgeConfig{Enabled: true, Symbol: "BTC"})
				sc.Direction = "both"
				return hedgeCfg(sc)
			}(),
			want: "direction \"both\"",
		},
		{
			name: "non-HL-perps strategy rejected",
			cfg: func() *Config {
				sc := hedgeTestStrategy("a", "ETH", &HedgeConfig{Enabled: true, Symbol: "BTC"})
				sc.Type = "spot"
				sc.Platform = "binanceus"
				return hedgeCfg(sc)
			}(),
			want: "Hyperliquid perps only",
		},
		{
			name: "manual peer coin collides too",
			cfg: func() *Config {
				manual := StrategyConfig{ID: "m", Type: "manual", Platform: "hyperliquid", Symbol: "BTC"}
				return hedgeCfg(hedgeTestStrategy("a", "ETH", &HedgeConfig{Enabled: true, Symbol: "BTC"}), manual)
			}(),
			want: "collides with strategy",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			errs := validateHedgeConfigs(tc.cfg)
			if tc.want == "" {
				if len(errs) != 0 {
					t.Fatalf("expected no errors, got %v", errs)
				}
				return
			}
			found := false
			for _, e := range errs {
				if strings.Contains(e, tc.want) {
					found = true
				}
			}
			if !found {
				t.Fatalf("expected an error containing %q, got %v", tc.want, errs)
			}
		})
	}
}

func TestHedgeCoinNormalization(t *testing.T) {
	sc := hedgeTestStrategy("a", "ETH", &HedgeConfig{Enabled: true, Symbol: " btc/usdc:usdc "})
	if got := hedgeCoin(sc); got != "BTC" {
		t.Fatalf("hedgeCoin = %q, want BTC", got)
	}
	if got := hedgeCoin(StrategyConfig{}); got != "" {
		t.Fatalf("hedgeCoin(no block) = %q, want empty", got)
	}
}

// ---------------------------------------------------------------------------
// Pure decision core

func TestHedgeTargetDecision(t *testing.T) {
	sc := hedgeTestStrategy("a", "ETH", &HedgeConfig{Enabled: true, Symbol: "BTC", Ratio: 1.0})

	t.Run("open inverse with notional sizing", func(t *testing.T) {
		snap := hedgeSnapshot{PrimaryQty: 2, PrimarySide: "long"}
		a := hedgeTargetDecision(sc, snap, 3000, 60000)
		if a.Kind != hedgeActionOpen || a.Side != "short" {
			t.Fatalf("got %+v, want open short", a)
		}
		// 2 * 3000 * 1.0 / 60000 = 0.1 BTC
		if a.Qty < 0.0999 || a.Qty > 0.1001 {
			t.Fatalf("qty = %v, want 0.1", a.Qty)
		}
	})

	t.Run("short primary opens long hedge", func(t *testing.T) {
		snap := hedgeSnapshot{PrimaryQty: 2, PrimarySide: "short"}
		a := hedgeTargetDecision(sc, snap, 3000, 60000)
		if a.Kind != hedgeActionOpen || a.Side != "long" {
			t.Fatalf("got %+v, want open long", a)
		}
	})

	t.Run("ratio scales notional", func(t *testing.T) {
		sc2 := hedgeTestStrategy("a", "ETH", &HedgeConfig{Enabled: true, Symbol: "BTC", Ratio: 0.5})
		a := hedgeTargetDecision(sc2, hedgeSnapshot{PrimaryQty: 2, PrimarySide: "long"}, 3000, 60000)
		if a.Qty < 0.0499 || a.Qty > 0.0501 {
			t.Fatalf("qty = %v, want 0.05", a.Qty)
		}
	})

	t.Run("primary flat closes hedge", func(t *testing.T) {
		snap := hedgeSnapshot{HedgeQty: 0.1, HedgeSide: "short", HedgeBasis: 2}
		a := hedgeTargetDecision(sc, snap, 3000, 60000)
		if a.Kind != hedgeActionCloseFull || a.Qty != 0.1 {
			t.Fatalf("got %+v, want closeFull 0.1", a)
		}
	})

	t.Run("primary flat closes hedge even with unusable prices", func(t *testing.T) {
		snap := hedgeSnapshot{HedgeQty: 0.1, HedgeSide: "short", HedgeBasis: 2}
		a := hedgeTargetDecision(sc, snap, 0, 0)
		if a.Kind != hedgeActionCloseFull {
			t.Fatalf("got %+v, want closeFull", a)
		}
	})

	t.Run("scale-in add mirrors delta notional", func(t *testing.T) {
		snap := hedgeSnapshot{PrimaryQty: 3, PrimarySide: "long", HedgeQty: 0.1, HedgeSide: "short", HedgeBasis: 2}
		a := hedgeTargetDecision(sc, snap, 3000, 60000)
		if a.Kind != hedgeActionAdd || a.Side != "short" {
			t.Fatalf("got %+v, want add short", a)
		}
		// delta 1 ETH * 3000 / 60000 = 0.05 BTC
		if a.Qty < 0.0499 || a.Qty > 0.0501 {
			t.Fatalf("qty = %v, want 0.05", a.Qty)
		}
	})

	t.Run("partial close reduces proportionally", func(t *testing.T) {
		snap := hedgeSnapshot{PrimaryQty: 1, PrimarySide: "long", HedgeQty: 0.1, HedgeSide: "short", HedgeBasis: 2}
		a := hedgeTargetDecision(sc, snap, 3000, 60000)
		if a.Kind != hedgeActionReduce {
			t.Fatalf("got %+v, want reduce", a)
		}
		// (2-1)/2 of 0.1 = 0.05
		if a.Qty < 0.0499 || a.Qty > 0.0501 {
			t.Fatalf("qty = %v, want 0.05", a.Qty)
		}
	})

	t.Run("mark drift alone never re-trades", func(t *testing.T) {
		snap := hedgeSnapshot{PrimaryQty: 2, PrimarySide: "long", HedgeQty: 0.1, HedgeSide: "short", HedgeBasis: 2}
		a := hedgeTargetDecision(sc, snap, 4500, 90000) // prices moved, qty didn't
		if a.Kind != hedgeActionNone {
			t.Fatalf("got %+v, want none", a)
		}
	})

	t.Run("wrong hedge side closes (defense in depth)", func(t *testing.T) {
		snap := hedgeSnapshot{PrimaryQty: 2, PrimarySide: "long", HedgeQty: 0.1, HedgeSide: "long", HedgeBasis: 2}
		a := hedgeTargetDecision(sc, snap, 3000, 60000)
		if a.Kind != hedgeActionCloseFull {
			t.Fatalf("got %+v, want closeFull", a)
		}
	})

	t.Run("unusable price fails closed on open", func(t *testing.T) {
		snap := hedgeSnapshot{PrimaryQty: 2, PrimarySide: "long"}
		a := hedgeTargetDecision(sc, snap, 3000, 0)
		if a.Kind != hedgeActionNone || a.Reason == "" {
			t.Fatalf("got %+v, want none with reason", a)
		}
	})

	t.Run("foreign position on hedge coin holds", func(t *testing.T) {
		snap := hedgeSnapshot{PrimaryQty: 2, PrimarySide: "long", HedgeForeign: true}
		a := hedgeTargetDecision(sc, snap, 3000, 60000)
		if a.Kind != hedgeActionNone || a.Reason == "" {
			t.Fatalf("got %+v, want none with reason", a)
		}
	})

	t.Run("dust add defers without advancing basis", func(t *testing.T) {
		snap := hedgeSnapshot{PrimaryQty: 2.0005, PrimarySide: "long", HedgeQty: 0.1, HedgeSide: "short", HedgeBasis: 2}
		a := hedgeTargetDecision(sc, snap, 3000, 60000) // delta notional $1.50 < $10
		if a.Kind != hedgeActionNone {
			t.Fatalf("got %+v, want none (deferred)", a)
		}
	})

	t.Run("disabled hedge does nothing", func(t *testing.T) {
		a := hedgeTargetDecision(StrategyConfig{}, hedgeSnapshot{PrimaryQty: 2, PrimarySide: "long"}, 3000, 60000)
		if a.Kind != hedgeActionNone {
			t.Fatalf("got %+v, want none", a)
		}
	})
}

func TestHedgeOrderSkipReason(t *testing.T) {
	sc := hedgeTestStrategy("a", "ETH", &HedgeConfig{Enabled: true, Symbol: "BTC"})
	if got := hedgeOrderSkipReason(sc, hedgeAction{Kind: hedgeActionOpen, Qty: 0.1, Side: "short"}); got != "" {
		t.Fatalf("expected proceed, got %q", got)
	}
	if got := hedgeOrderSkipReason(sc, hedgeAction{Kind: hedgeActionOpen, Qty: 0, Side: "short"}); got == "" {
		t.Fatalf("expected skip for zero qty")
	}
	if got := hedgeOrderSkipReason(sc, hedgeAction{Kind: hedgeActionOpen, Qty: 0.1, Side: "sideways"}); got == "" {
		t.Fatalf("expected skip for invalid side")
	}
	if got := hedgeOrderSkipReason(StrategyConfig{}, hedgeAction{Kind: hedgeActionOpen, Qty: 0.1, Side: "short"}); got == "" {
		t.Fatalf("expected skip for hedge-less strategy")
	}
}

// ---------------------------------------------------------------------------
// Booking / risk attribution

func hedgeTestState() *StrategyState {
	return &StrategyState{
		ID:        "a",
		Type:      "perps",
		Platform:  "hyperliquid",
		Cash:      10000,
		Positions: map[string]*Position{},
	}
}

func TestApplyHedgeFill_OpenReduceCloseLifecycle(t *testing.T) {
	sc := hedgeTestStrategy("a", "ETH", &HedgeConfig{Enabled: true, Symbol: "BTC", Leverage: 3})
	s := hedgeTestState()
	var logger *StrategyLogger

	// Open: primary 2 ETH long → 0.1 BTC short hedge.
	openAction := hedgeAction{Kind: hedgeActionOpen, Qty: 0.1, Side: "short"}
	applyHedgeFill(sc, s, "ETH", "BTC", openAction, hedgeSnapshot{PrimaryQty: 2, PrimarySide: "long"}, 0.1, 60000, 2.5, true, "111", logger)

	pos := s.Positions["BTC"]
	if pos == nil {
		t.Fatalf("hedge position not created")
	}
	if pos.HedgeFor != "ETH" || pos.HedgePrimaryQtyBasis != 2 || pos.Side != "short" || pos.Multiplier != 1 || pos.Leverage != 3 {
		t.Fatalf("hedge position metadata wrong: %+v", pos)
	}
	if len(s.TradeHistory) != 1 || s.TradeHistory[0].TradeType != "hedge" || s.TradeHistory[0].IsClose {
		t.Fatalf("open trade wrong: %+v", s.TradeHistory)
	}
	if s.Cash != 10000-2.5 {
		t.Fatalf("open fee not deducted: cash=%v", s.Cash)
	}

	// Reduce: primary shrank 2 → 1, reduce 0.05 BTC.
	reduceAction := hedgeAction{Kind: hedgeActionReduce, Qty: 0.05}
	applyHedgeFill(sc, s, "ETH", "BTC", reduceAction, hedgeSnapshot{PrimaryQty: 1, PrimarySide: "long", HedgeQty: 0.1, HedgeSide: "short", HedgeBasis: 2}, 0.05, 59000, 1.0, true, "112", logger)
	pos = s.Positions["BTC"]
	if pos == nil || pos.Quantity < 0.0499 || pos.Quantity > 0.0501 {
		t.Fatalf("reduce did not shrink hedge: %+v", pos)
	}
	if pos.HedgePrimaryQtyBasis != 1 {
		t.Fatalf("basis not advanced on reduce: %v", pos.HedgePrimaryQtyBasis)
	}
	last := s.TradeHistory[len(s.TradeHistory)-1]
	if last.TradeType != "hedge" || !last.IsClose {
		t.Fatalf("reduce leg wrong: %+v", last)
	}
	// Short reduced at a profit (60000 → 59000): gross = 0.05*1000 = 50.
	if last.RealizedPnL < 49.9 || last.RealizedPnL > 50.1 {
		t.Fatalf("reduce PnL = %v, want ~50 gross", last.RealizedPnL)
	}

	// CloseFull: primary flat.
	lossesBefore := s.RiskState.ConsecutiveLosses
	closeAction := hedgeAction{Kind: hedgeActionCloseFull, Qty: pos.Quantity}
	applyHedgeFill(sc, s, "ETH", "BTC", closeAction, hedgeSnapshot{HedgeQty: pos.Quantity, HedgeSide: "short", HedgeBasis: 1}, pos.Quantity, 61000, 1.0, true, "113", logger)
	if s.Positions["BTC"] != nil {
		t.Fatalf("hedge position not removed on full close")
	}
	last = s.TradeHistory[len(s.TradeHistory)-1]
	if last.TradeType != "hedge" || !last.IsClose {
		t.Fatalf("close leg wrong: %+v", last)
	}
	// Losing hedge close (short, 60000→61000) must NOT bump the CB loss streak.
	if s.RiskState.ConsecutiveLosses != lossesBefore {
		t.Fatalf("hedge loss bumped ConsecutiveLosses %d → %d", lossesBefore, s.RiskState.ConsecutiveLosses)
	}
	if s.RiskState.DailyPnL == 0 {
		t.Fatalf("hedge close PnL missing from DailyPnL accounting")
	}
}

func TestRecordHedgeTradeResult_NoLossStreak(t *testing.T) {
	var r RiskState
	RecordHedgeTradeResult(&r, -100)
	if r.ConsecutiveLosses != 0 {
		t.Fatalf("hedge loss counted in streak")
	}
	if r.DailyPnL != -100 {
		t.Fatalf("DailyPnL = %v, want -100", r.DailyPnL)
	}
	RecordTradeResult(&r, -50)
	if r.ConsecutiveLosses != 1 {
		t.Fatalf("primary loss must still count")
	}
}

func TestClassifyPositionTradeType_Hedge(t *testing.T) {
	s := hedgeTestState()
	pos := &Position{Symbol: "BTC", Multiplier: 1, HedgeFor: "ETH"}
	if got := classifyPositionTradeType(s, pos); got != "hedge" {
		t.Fatalf("got %q, want hedge", got)
	}
	if got := positionCloseTradeType(pos); got != "hedge" {
		t.Fatalf("positionCloseTradeType = %q, want hedge", got)
	}
	if got := positionCloseTradeType(&Position{Symbol: "ETH"}); got != "perps" {
		t.Fatalf("non-hedge positionCloseTradeType = %q, want perps", got)
	}
}

func TestBookPerpsClose_HedgeLegLabelsAndStreak(t *testing.T) {
	s := hedgeTestState()
	s.Positions["BTC"] = &Position{
		Symbol: "BTC", Quantity: 0.1, AvgCost: 60000, Side: "short",
		Multiplier: 1, OwnerStrategyID: "a", HedgeFor: "ETH",
	}
	// Close at a loss for the short.
	if !bookPerpsCloseWithFillFee(s, "BTC", 61000, 1.0, true, "9", "hedge_close", "hedge(ETH) close", "hedge(ETH) close", nil) {
		t.Fatalf("close not booked")
	}
	last := s.TradeHistory[len(s.TradeHistory)-1]
	if last.TradeType != "hedge" {
		t.Fatalf("close TradeType = %q, want hedge", last.TradeType)
	}
	if s.RiskState.ConsecutiveLosses != 0 {
		t.Fatalf("hedge close loss bumped the streak")
	}
}

// ---------------------------------------------------------------------------
// Kill-switch / snapshot / reconcile extensions

func TestSnapshotVirtualQuantities_IncludesHedgeLeg(t *testing.T) {
	sc := hedgeTestStrategy("a", "ETH", &HedgeConfig{Enabled: true, Symbol: "BTC"})
	s := hedgeTestState()
	s.Positions["ETH"] = &Position{Symbol: "ETH", Quantity: 2, Side: "long", Multiplier: 1}
	s.Positions["BTC"] = &Position{Symbol: "BTC", Quantity: 0.1, Side: "short", Multiplier: 1, HedgeFor: "ETH"}
	snap := snapshotHyperliquidVirtualQuantities(map[string]*StrategyState{"a": s}, []StrategyConfig{sc})
	if snap["ETH"]["a"] != 2 || snap["BTC"]["a"] != 0.1 {
		t.Fatalf("snapshot missing legs: %+v", snap)
	}
}

func TestHedgeCoinsWithHeldLegs(t *testing.T) {
	sc := hedgeTestStrategy("a", "ETH", &HedgeConfig{Enabled: true, Symbol: "BTC"})
	s := hedgeTestState()
	// Declared but flat: not in the roster (protects foreign positions).
	if got := hedgeCoinsWithHeldLegs(map[string]*StrategyState{"a": s}, []StrategyConfig{sc}); got != nil {
		t.Fatalf("flat hedge coin must not join the roster: %v", got)
	}
	s.Positions["BTC"] = &Position{Symbol: "BTC", Quantity: 0.1, Side: "short", Multiplier: 1, HedgeFor: "ETH"}
	got := hedgeCoinsWithHeldLegs(map[string]*StrategyState{"a": s}, []StrategyConfig{sc})
	if !got["BTC"] {
		t.Fatalf("held hedge leg missing from roster: %v", got)
	}
}

func TestForceCloseHyperliquidLive_HeldHedgeCoinCloses(t *testing.T) {
	sc := hedgeTestStrategy("a", "ETH", &HedgeConfig{Enabled: true, Symbol: "BTC"})
	positions := []HLPosition{
		{Coin: "ETH", Size: 2},
		{Coin: "BTC", Size: -0.1},
	}
	var closed []string
	closer := func(sym string, partialSz *float64, oids []int64) (*HyperliquidCloseResult, error) {
		closed = append(closed, sym)
		return &HyperliquidCloseResult{Close: &HyperliquidClose{Symbol: sym, Fill: &HyperliquidCloseFill{AvgPx: 1, TotalSz: 1}}}, nil
	}
	// Without the hedge roster the BTC position is skipped as unowned.
	report := forceCloseHyperliquidLive(t.Context(), positions, []StrategyConfig{sc}, closer, nil, nil)
	if len(report.ClosedCoins) != 1 || report.ClosedCoins[0] != "ETH" {
		t.Fatalf("baseline: got %v, want [ETH]", report.ClosedCoins)
	}
	// With the held-leg roster it closes.
	closed = nil
	report = forceCloseHyperliquidLive(t.Context(), positions, []StrategyConfig{sc}, closer, nil, map[string]bool{"BTC": true})
	if len(report.ClosedCoins) != 2 {
		t.Fatalf("got %v, want ETH+BTC", report.ClosedCoins)
	}
}

func TestValidatePerpsDirectionConfig_SkipsHedgeLegs(t *testing.T) {
	sc := hedgeTestStrategy("a", "ETH", &HedgeConfig{Enabled: true, Symbol: "BTC"})
	sc.Direction = "long"
	s := hedgeTestState()
	// Inverse hedge: a SHORT under direction=long would warn without the skip.
	s.Positions["BTC"] = &Position{Symbol: "BTC", Quantity: 0.1, Side: "short", Multiplier: 1, HedgeFor: "ETH"}
	state := &AppState{Strategies: map[string]*StrategyState{"a": s}}
	warnings := ValidatePerpsDirectionConfig(state, &Config{Strategies: []StrategyConfig{sc}})
	if len(warnings) != 0 {
		t.Fatalf("hedge leg tripped the direction gap warning: %v", warnings)
	}
}

func TestValidateHedgeStateConsistency(t *testing.T) {
	sc := hedgeTestStrategy("a", "ETH", nil) // hedge block removed
	s := hedgeTestState()
	s.Positions["BTC"] = &Position{Symbol: "BTC", Quantity: 0.1, Side: "short", Multiplier: 1, HedgeFor: "ETH"}
	state := &AppState{Strategies: map[string]*StrategyState{"a": s}}
	warnings := validateHedgeStateConsistency(state, &Config{Strategies: []StrategyConfig{sc}})
	if len(warnings) != 1 || !strings.Contains(warnings[0], "no longer declares") {
		t.Fatalf("expected config-gap warning, got %v", warnings)
	}
	// Symbol mismatch.
	sc2 := hedgeTestStrategy("a", "ETH", &HedgeConfig{Enabled: true, Symbol: "SOL"})
	warnings = validateHedgeStateConsistency(state, &Config{Strategies: []StrategyConfig{sc2}})
	if len(warnings) != 1 || !strings.Contains(warnings[0], "hedge.symbol") {
		t.Fatalf("expected symbol-gap warning, got %v", warnings)
	}
	// Matching config: no warning.
	sc3 := hedgeTestStrategy("a", "ETH", &HedgeConfig{Enabled: true, Symbol: "BTC"})
	if warnings = validateHedgeStateConsistency(state, &Config{Strategies: []StrategyConfig{sc3}}); len(warnings) != 0 {
		t.Fatalf("expected no warnings, got %v", warnings)
	}
}

// ---------------------------------------------------------------------------
// Hot reload (#1159 constraint 7)

func TestHedgeHotReload_BlockedWhileOpenFreeWhenFlat(t *testing.T) {
	mk := func(h *HedgeConfig) *Config {
		sc := hedgeTestStrategy("a", "ETH", h)
		return &Config{Strategies: []StrategyConfig{sc}}
	}
	openState := &AppState{Strategies: map[string]*StrategyState{"a": {
		ID:        "a",
		Positions: map[string]*Position{"ETH": {Symbol: "ETH", Quantity: 1, Side: "long"}},
	}}}
	flatState := &AppState{Strategies: map[string]*StrategyState{"a": {ID: "a", Positions: map[string]*Position{}}}}

	old := mk(nil)
	next := mk(&HedgeConfig{Enabled: true, Symbol: "BTC"})
	if err := validateHotReloadStateCompatible(old, next, openState); err == nil {
		t.Fatalf("hedge add while open must be blocked")
	}
	if err := validateHotReloadStateCompatible(old, next, flatState); err != nil {
		t.Fatalf("hedge add when flat must reload: %v", err)
	}
	// A residual hedge leg with the primary flat also blocks.
	hedgeOnlyState := &AppState{Strategies: map[string]*StrategyState{"a": {
		ID:        "a",
		Positions: map[string]*Position{"BTC": {Symbol: "BTC", Quantity: 0.1, Side: "short", HedgeFor: "ETH"}},
	}}}
	if err := validateHotReloadStateCompatible(next, mk(nil), hedgeOnlyState); err == nil {
		t.Fatalf("hedge removal with residual hedge leg must be blocked")
	}
}

func TestHedgeRestartShapeMasked(t *testing.T) {
	a := hedgeTestStrategy("a", "ETH", nil)
	b := hedgeTestStrategy("a", "ETH", &HedgeConfig{Enabled: true, Symbol: "BTC"})
	if !reflect.DeepEqual(strategyRestartShape(a), strategyRestartShape(b)) {
		t.Fatalf("hedge block must be masked from the restart shape (hot-reloadable when flat)")
	}
}

// ---------------------------------------------------------------------------
// Unknown-key guard

func TestHedgeUnknownKeyGuard(t *testing.T) {
	raw := []byte(`{"strategies":[{"id":"a","type":"perps","platform":"hyperliquid",
		"hedge":{"enabled":true,"symbol":"BTC","ration":2.0}}]}`)
	errs := validateStrategyJSONKeys(raw)
	found := false
	for _, e := range errs {
		if strings.Contains(e, `"ration"`) && strings.Contains(e, "hedge") {
			found = true
		}
	}
	if !found {
		t.Fatalf("typo'd hedge key not flagged: %v", errs)
	}
	if !knownStrategyConfigKeys()["hedge"] {
		t.Fatalf("hedge missing from knownStrategyConfigKeys")
	}
}

// ---------------------------------------------------------------------------
// Manual drain attribution (review finding: applyManualAction close branch)

func TestApplyManualAction_HedgeCloseAttribution(t *testing.T) {
	sc := hedgeTestStrategy("a", "ETH", &HedgeConfig{Enabled: true, Symbol: "BTC"})
	s := hedgeTestState()
	s.Positions["BTC"] = &Position{
		Symbol: "BTC", Quantity: 0.1, InitialQuantity: 0.1, AvgCost: 60000,
		Side: "short", Multiplier: 1, OwnerStrategyID: "a", HedgeFor: "ETH",
	}
	state := &AppState{Strategies: map[string]*StrategyState{"a": s}}
	scByID := map[string]StrategyConfig{"a": sc}

	// Losing hedge close (short closed higher) drained via the manual queue
	// — e.g. forceCloseHedgeFollowUp after force-closing a WINNING primary.
	a := PendingManualAction{
		StrategyID: "a", Action: "close", Symbol: "BTC", Side: "buy",
		Quantity: 0.1, FillPrice: 61000, FillFee: 1.0,
		RealizedPnL: -101, IsFullClose: true,
	}
	if err := applyManualAction(state, nil, scByID, a); err != nil {
		t.Fatalf("applyManualAction: %v", err)
	}
	last := s.TradeHistory[len(s.TradeHistory)-1]
	if last.TradeType != "hedge" {
		t.Fatalf("drained hedge close TradeType = %q, want hedge (would count in lifetime W/L)", last.TradeType)
	}
	if s.RiskState.ConsecutiveLosses != 0 {
		t.Fatalf("drained hedge close bumped ConsecutiveLosses to %d", s.RiskState.ConsecutiveLosses)
	}
	if s.RiskState.DailyPnL != -101 {
		t.Fatalf("DailyPnL = %v, want -101 (hedge PnL still books to daily accounting)", s.RiskState.DailyPnL)
	}

	// Inverse guard: a genuine PRIMARY close on the same strategy still
	// bumps the streak and keeps the perps label.
	s.Positions["ETH"] = &Position{
		Symbol: "ETH", Quantity: 1, InitialQuantity: 1, AvgCost: 3000,
		Side: "long", Multiplier: 1, OwnerStrategyID: "a",
	}
	p := PendingManualAction{
		StrategyID: "a", Action: "close", Symbol: "ETH", Side: "sell",
		Quantity: 1, FillPrice: 2900, FillFee: 1.0,
		RealizedPnL: -101, IsFullClose: true,
	}
	if err := applyManualAction(state, nil, scByID, p); err != nil {
		t.Fatalf("applyManualAction primary: %v", err)
	}
	last = s.TradeHistory[len(s.TradeHistory)-1]
	if last.TradeType != "perps" {
		t.Fatalf("primary close TradeType = %q, want perps", last.TradeType)
	}
	if s.RiskState.ConsecutiveLosses != 1 {
		t.Fatalf("primary losing close must bump the streak, got %d", s.RiskState.ConsecutiveLosses)
	}
}

func TestApplyManualAction_HedgePartialCloseAttribution(t *testing.T) {
	sc := hedgeTestStrategy("a", "ETH", &HedgeConfig{Enabled: true, Symbol: "BTC"})
	s := hedgeTestState()
	s.Positions["BTC"] = &Position{
		Symbol: "BTC", Quantity: 0.1, InitialQuantity: 0.1, AvgCost: 60000,
		Side: "short", Multiplier: 1, OwnerStrategyID: "a", HedgeFor: "ETH",
	}
	state := &AppState{Strategies: map[string]*StrategyState{"a": s}}
	a := PendingManualAction{
		StrategyID: "a", Action: "close", Symbol: "BTC", Side: "buy",
		Quantity: 0.05, FillPrice: 61000, FillFee: 0.5,
		RealizedPnL: -50.5, IsFullClose: false,
	}
	if err := applyManualAction(state, nil, map[string]StrategyConfig{"a": sc}, a); err != nil {
		t.Fatalf("applyManualAction: %v", err)
	}
	last := s.TradeHistory[len(s.TradeHistory)-1]
	if last.TradeType != "hedge" || s.RiskState.ConsecutiveLosses != 0 {
		t.Fatalf("partial hedge close attribution wrong: type=%q streak=%d", last.TradeType, s.RiskState.ConsecutiveLosses)
	}
	if s.Positions["BTC"] == nil || s.Positions["BTC"].Quantity != 0.05 {
		t.Fatalf("partial close remainder wrong: %+v", s.Positions["BTC"])
	}
}

// ---------------------------------------------------------------------------
// On-demand hedge mid refetch (review optional finding)

func TestRunHedgeSync_RefetchesMissingHedgeMid(t *testing.T) {
	sc := hedgeTestStrategy("a", "ETH", &HedgeConfig{Enabled: true, Symbol: "BTC"})
	sc.Args = []string{"ema_crossover", "ETH", "1h", "--mode=paper"} // paper: no subprocess
	s := hedgeTestState()
	s.Positions["ETH"] = &Position{Symbol: "ETH", Quantity: 2, Side: "long", Multiplier: 1, OwnerStrategyID: "a"}
	orig := hedgeMidRefetch
	defer func() { hedgeMidRefetch = orig }()
	refetched := false
	hedgeMidRefetch = func(coins []string) (map[string]float64, error) {
		refetched = true
		if len(coins) != 1 || coins[0] != "BTC" {
			t.Fatalf("refetch coins = %v, want [BTC]", coins)
		}
		return map[string]float64{"BTC": 60000}, nil
	}

	var mu sync.RWMutex
	prices := map[string]float64{"ETH": 3000} // hedge mid missing from the batch
	runHedgeSync(sc, s, "ETH", 3000, prices, true, &mu, nil, newTestLogger(t))

	if !refetched {
		t.Fatalf("missing hedge mid did not trigger the on-demand refetch")
	}
	// Paper hedge opened at the refetched mark instead of unwinding the primary.
	if pos := s.Positions["BTC"]; pos == nil || pos.Side != "short" || pos.HedgeFor != "ETH" {
		t.Fatalf("hedge not opened after refetch: %+v", s.Positions["BTC"])
	}
	if s.Positions["ETH"] == nil {
		t.Fatalf("primary was unwound despite a recoverable price gap")
	}
	if prices["BTC"] != 60000 {
		t.Fatalf("refetched mid not published to the cycle price map")
	}
}

func TestRunHedgeSync_UnwindsWhenRefetchAlsoEmpty(t *testing.T) {
	sc := hedgeTestStrategy("a", "ETH", &HedgeConfig{Enabled: true, Symbol: "BTC"})
	sc.Args = []string{"ema_crossover", "ETH", "1h", "--mode=paper"}
	s := hedgeTestState()
	s.Positions["ETH"] = &Position{Symbol: "ETH", Quantity: 2, AvgCost: 2900, Side: "long", Multiplier: 1, OwnerStrategyID: "a"}

	orig := hedgeMidRefetch
	defer func() { hedgeMidRefetch = orig }()
	hedgeMidRefetch = func(coins []string) (map[string]float64, error) {
		return map[string]float64{}, nil // still no mark
	}

	var mu sync.RWMutex
	prices := map[string]float64{"ETH": 3000}
	runHedgeSync(sc, s, "ETH", 3000, prices, true, &mu, nil, newTestLogger(t))

	// Fail-closed: fresh open with an unhedgeable target unwinds the primary.
	if s.Positions["ETH"] != nil {
		t.Fatalf("primary not unwound after refetch also came back empty")
	}
}

// ---------------------------------------------------------------------------
// Snapshot helper

func TestHedgeSyncSnapshot(t *testing.T) {
	s := hedgeTestState()
	s.Positions["ETH"] = &Position{Symbol: "ETH", Quantity: 2, Side: "long"}
	s.Positions["BTC"] = &Position{Symbol: "BTC", Quantity: 0.1, Side: "short", HedgeFor: "ETH", HedgePrimaryQtyBasis: 2}
	snap := hedgeSyncSnapshot(s, "ETH", "BTC")
	if snap.PrimaryQty != 2 || snap.HedgeQty != 0.1 || snap.HedgeBasis != 2 || snap.HedgeForeign || snap.HedgeStranded {
		t.Fatalf("snapshot wrong: %+v", snap)
	}
	// Foreign position (no HedgeFor stamp).
	s.Positions["BTC"].HedgeFor = ""
	snap = hedgeSyncSnapshot(s, "ETH", "BTC")
	if !snap.HedgeForeign {
		t.Fatalf("foreign hedge-coin position not flagged: %+v", snap)
	}
}

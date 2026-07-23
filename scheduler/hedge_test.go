package main

import (
	"context"
	"io"
	"math"
	"strings"
	"testing"
	"time"
)

func hedgeTestConfig(id, symbol, hedgeSymbol string) StrategyConfig {
	return StrategyConfig{
		ID:             id,
		Type:           "perps",
		Platform:       "hyperliquid",
		Script:         "shared_scripts/check_hyperliquid.py",
		Args:           []string{"rsi", symbol, "--mode=live"},
		Capital:        1000,
		MaxDrawdownPct: 20,
		Leverage:       3,
		MarginMode:     "isolated",
		Hedge: &HedgeConfig{
			Enabled:    true,
			Symbol:     hedgeSymbol,
			Side:       HedgeSideInverse,
			Ratio:      0.5,
			Platform:   "hyperliquid",
			Type:       "perps",
			MarginMode: "cross",
			Leverage:   2,
		},
	}
}

func hedgeTestState(id string) *StrategyState {
	return &StrategyState{
		ID:              id,
		Type:            "perps",
		Platform:        "hyperliquid",
		Cash:            1000,
		InitialCapital:  1000,
		Positions:       map[string]*Position{},
		OptionPositions: map[string]*OptionPosition{},
	}
}

func hedgeSilentLogger(id string) *StrategyLogger {
	return &StrategyLogger{stratID: id, writer: io.Discard}
}

// --- sizing -----------------------------------------------------------------

func TestHedgeOpenQtyInverseNotionalSizing(t *testing.T) {
	// 4 ETH @ $2500 = $10k notional × ratio 0.5 = $5k ÷ BTC $50k = 0.1 BTC.
	qty, err := hedgeOpenQty(0.5, 4, 2500, 50000)
	if err != nil {
		t.Fatal(err)
	}
	if math.Abs(qty-0.1) > 1e-12 {
		t.Fatalf("qty = %g, want 0.1", qty)
	}
	if _, err := hedgeOpenQty(0.5, 4, 0, 50000); err == nil {
		t.Fatal("expected error for zero primary mark")
	}
	if _, err := hedgeOpenQty(0.5, 4, 2500, 0); err == nil {
		t.Fatal("expected error for zero hedge mark")
	}
}

// --- convergence planning ----------------------------------------------------

func TestPlanHedgeConvergenceLifecycle(t *testing.T) {
	long4 := &hedgePrimarySnapshot{Symbol: "ETH", Side: "long", Quantity: 4}
	short4 := &hedgePrimarySnapshot{Symbol: "ETH", Side: "short", Quantity: 4}
	tests := []struct {
		name    string
		primary *hedgePrimarySnapshot
		hedge   *hedgeLegSnapshot
		want    []hedgeOrder
	}{
		{
			name:    "fresh open long primary → sell hedge",
			primary: long4,
			want:    []hedgeOrder{{Side: "sell", Quantity: 0.1, CoveredAfter: 4}},
		},
		{
			name:    "fresh open short primary → buy hedge",
			primary: short4,
			want:    []hedgeOrder{{Side: "buy", Quantity: 0.1, CoveredAfter: 4}},
		},
		{
			name:    "scale-in adds uncovered delta",
			primary: &hedgePrimarySnapshot{Symbol: "ETH", Side: "long", Quantity: 6},
			hedge:   &hedgeLegSnapshot{Symbol: "BTC", Side: "short", Quantity: 0.1, Covered: 4},
			// delta 2 ETH × $2500 × 0.5 ÷ $50000 = 0.05 BTC
			want: []hedgeOrder{{Side: "sell", Quantity: 0.05, CoveredAfter: 6}},
		},
		{
			name:    "partial close reduces proportionally",
			primary: &hedgePrimarySnapshot{Symbol: "ETH", Side: "long", Quantity: 2},
			hedge:   &hedgeLegSnapshot{Symbol: "BTC", Side: "short", Quantity: 0.1, Covered: 4},
			want:    []hedgeOrder{{Close: true, Quantity: 0.05, CoveredAfter: 2}},
		},
		{
			name:  "primary flat → full close",
			hedge: &hedgeLegSnapshot{Symbol: "BTC", Side: "short", Quantity: 0.1, Covered: 4},
			want:  []hedgeOrder{{Close: true, FullClose: true, Quantity: 0.1}},
		},
		{
			name:    "primary flip → close then reopen inverse",
			primary: short4,
			hedge:   &hedgeLegSnapshot{Symbol: "BTC", Side: "short", Quantity: 0.1, Covered: 4},
			want: []hedgeOrder{
				{Close: true, FullClose: true, Quantity: 0.1},
				{Side: "buy", Quantity: 0.1, CoveredAfter: 4},
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			plan, err := planHedgeConvergence(0.5, tc.primary, tc.hedge, 2500, 50000)
			if err != nil {
				t.Fatal(err)
			}
			if len(plan.Orders) != len(tc.want) {
				t.Fatalf("orders = %+v, want %+v", plan.Orders, tc.want)
			}
			for i := range plan.Orders {
				got, want := plan.Orders[i], tc.want[i]
				if got.Side != want.Side || got.Close != want.Close || got.FullClose != want.FullClose ||
					math.Abs(got.Quantity-want.Quantity) > 1e-9 || math.Abs(got.CoveredAfter-want.CoveredAfter) > 1e-9 {
					t.Fatalf("orders[%d] = %+v, want %+v", i, got, want)
				}
			}
		})
	}
}

// Regression: the ongoing target is quantity-anchored (covered watermark),
// never re-derived from live marks — a mark move with an unchanged primary
// quantity must not emit an order, or the engine would churn every cycle.
func TestPlanHedgeConvergenceNoChurnOnMarkMoves(t *testing.T) {
	primary := &hedgePrimarySnapshot{Symbol: "ETH", Side: "long", Quantity: 4}
	hedge := &hedgeLegSnapshot{Symbol: "BTC", Side: "short", Quantity: 0.1, Covered: 4}
	for _, marks := range [][2]float64{{2500, 50000}, {3800, 41000}, {900, 93000}, {0, 0}} {
		plan, err := planHedgeConvergence(0.5, primary, hedge, marks[0], marks[1])
		if err != nil {
			t.Fatalf("marks %v: %v", marks, err)
		}
		if len(plan.Orders) != 0 || plan.StampCovered != nil {
			t.Fatalf("marks %v: expected no-op plan, got %+v", marks, plan)
		}
	}
}

func TestPlanHedgeConvergenceAdoptsLegacyCovered(t *testing.T) {
	primary := &hedgePrimarySnapshot{Symbol: "ETH", Side: "long", Quantity: 4}
	hedge := &hedgeLegSnapshot{Symbol: "BTC", Side: "short", Quantity: 0.1, Covered: 0}
	plan, err := planHedgeConvergence(0.5, primary, hedge, 2500, 50000)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Orders) != 0 || plan.StampCovered == nil || *plan.StampCovered != 4 {
		t.Fatalf("expected adopt-covered stamp 4, got %+v", plan)
	}
}

func TestPlanHedgeConvergenceOpenNeedsMarks(t *testing.T) {
	primary := &hedgePrimarySnapshot{Symbol: "ETH", Side: "long", Quantity: 4}
	if _, err := planHedgeConvergence(0.5, primary, nil, 2500, 0); err == nil {
		t.Fatal("expected sizing error when hedge mark is missing")
	}
	// Closes must NOT need marks: primary flat, no marks at all.
	hedge := &hedgeLegSnapshot{Symbol: "BTC", Side: "short", Quantity: 0.1, Covered: 4}
	plan, err := planHedgeConvergence(0.5, nil, hedge, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Orders) != 1 || !plan.Orders[0].FullClose {
		t.Fatalf("expected mark-free full close, got %+v", plan)
	}
}

func TestPlanHedgeConvergenceRejectsCorruptHedge(t *testing.T) {
	primary := &hedgePrimarySnapshot{Symbol: "ETH", Side: "long", Quantity: 4}
	if _, err := planHedgeConvergence(0.5, primary, &hedgeLegSnapshot{Symbol: "BTC", Side: "sideways", Quantity: 0.1}, 2500, 50000); err == nil {
		t.Fatal("expected corrupt-hedge error")
	}
}

// --- validation ---------------------------------------------------------------

func TestValidateHedgeConfigsRejectsCollisions(t *testing.T) {
	manualBTC := StrategyConfig{ID: "man-btc", Type: "manual", Platform: "hyperliquid", Symbol: "BTC"}
	tests := []struct {
		name       string
		strategies []StrategyConfig
		want       string
	}{
		{"own primary coin", []StrategyConfig{hedgeTestConfig("a", "ETH", "ETH")}, "matches its own primary coin"},
		{"another strategy's coin", []StrategyConfig{hedgeTestConfig("a", "ETH", "BTC"), hedgeTestConfig("b", "BTC", "SOL")}, "matches configured strategy"},
		{"manual strategy's coin", []StrategyConfig{hedgeTestConfig("a", "ETH", "BTC"), manualBTC}, "matches configured strategy"},
		{"shared hedge coin", []StrategyConfig{hedgeTestConfig("a", "ETH", "BTC"), hedgeTestConfig("b", "SOL", "BTC")}, "already the hedge coin"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			errs := validateHedgeConfigs(tc.strategies)
			if !hedgeErrsContain(errs, tc.want) {
				t.Fatalf("errs = %v, want substring %q", errs, tc.want)
			}
		})
	}
}

func TestValidateHedgeConfigsRejectsUnsupportedShape(t *testing.T) {
	tests := []struct {
		name string
		edit func(*StrategyConfig)
		want string
	}{
		{"paper primary", func(sc *StrategyConfig) { sc.Args[2] = "--mode=paper" }, "live Hyperliquid perps"},
		{"non-perps hedge type", func(sc *StrategyConfig) { sc.Hedge.Type = "spot" }, "type=\"perps\""},
		{"non-inverse side", func(sc *StrategyConfig) { sc.Hedge.Side = "same" }, "side must be"},
		{"zero ratio", func(sc *StrategyConfig) { sc.Hedge.Ratio = 0 }, "ratio must be > 0"},
		{"nan ratio", func(sc *StrategyConfig) { sc.Hedge.Ratio = math.NaN() }, "ratio must be > 0"},
		{"bad margin mode", func(sc *StrategyConfig) { sc.Hedge.MarginMode = "" }, "margin_mode"},
		{"zero leverage", func(sc *StrategyConfig) { sc.Hedge.Leverage = 0 }, "leverage must be"},
		{"missing symbol", func(sc *StrategyConfig) { sc.Hedge.Symbol = " " }, "symbol is required"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			sc := hedgeTestConfig("a", "ETH", "BTC")
			tc.edit(&sc)
			errs := validateHedgeConfigs([]StrategyConfig{sc})
			if !hedgeErrsContain(errs, tc.want) {
				t.Fatalf("errs = %v, want substring %q", errs, tc.want)
			}
		})
	}
}

func TestValidateHedgeConfigsAcceptsValidAndDisabled(t *testing.T) {
	valid := hedgeTestConfig("a", "ETH", "BTC")
	if errs := validateHedgeConfigs([]StrategyConfig{valid}); len(errs) != 0 {
		t.Fatalf("valid config rejected: %v", errs)
	}
	disabled := hedgeTestConfig("a", "ETH", "ETH") // colliding but disabled → ignored
	disabled.Hedge.Enabled = false
	if errs := validateHedgeConfigs([]StrategyConfig{disabled}); len(errs) != 0 {
		t.Fatalf("disabled block rejected: %v", errs)
	}
}

func TestValidateConfigWiresHedgeChecks(t *testing.T) {
	cfg := &Config{Strategies: []StrategyConfig{hedgeTestConfig("a", "ETH", "ETH")}}
	err := validateConfig(cfg, true)
	if err == nil || !strings.Contains(err.Error(), "matches its own primary coin") {
		t.Fatalf("validateConfig error = %v, want hedge collision", err)
	}
}

func hedgeErrsContain(errs []string, want string) bool {
	for _, e := range errs {
		if strings.Contains(e, want) {
			return true
		}
	}
	return false
}

// --- hot reload ----------------------------------------------------------------

func TestHedgeHotReloadBlockedWhileHedgeLegOpen(t *testing.T) {
	old := hedgeTestConfig("a", "ETH", "BTC")
	next := old
	clone := *old.Hedge
	clone.Ratio = 0.75
	next.Hedge = &clone
	state := &AppState{Strategies: map[string]*StrategyState{"a": {
		Positions: map[string]*Position{
			"BTC": {Symbol: "BTC", Quantity: 0.1, Side: "short", IsHedge: true, HedgePrimarySymbol: "ETH"},
		},
	}}}
	err := validateHotReloadStateCompatible(&Config{Strategies: []StrategyConfig{old}}, &Config{Strategies: []StrategyConfig{next}}, state)
	if err == nil || !strings.Contains(err.Error(), "hedge changed with an open hedge leg") {
		t.Fatalf("error = %v, want hedge-open block", err)
	}
}

func TestHedgeHotReloadAllowedWhileHedgeFlat(t *testing.T) {
	// Primary open but no hedge leg (e.g. hedge just being enabled): the
	// change must pass the state-compat gate (issue constraint 7).
	old := hedgeTestConfig("a", "ETH", "BTC")
	old.Hedge = nil
	next := hedgeTestConfig("a", "ETH", "BTC")
	state := &AppState{Strategies: map[string]*StrategyState{"a": {
		Positions: map[string]*Position{
			"ETH": {Symbol: "ETH", Quantity: 4, Side: "long"},
		},
	}}}
	if err := validateHotReloadStateCompatible(&Config{Strategies: []StrategyConfig{old}}, &Config{Strategies: []StrategyConfig{next}}, state); err != nil {
		t.Fatalf("expected hedge enable to hot-reload while hedge flat, got %v", err)
	}
}

// --- risk-streak exclusion -------------------------------------------------------

func TestHedgeCloseSkipsLossStreakButFeedsDailyPnL(t *testing.T) {
	ss := hedgeTestState("a")
	ss.Positions["BTC"] = &Position{
		Symbol: "BTC", Quantity: 0.1, InitialQuantity: 0.1, AvgCost: 50000,
		Side: "short", Multiplier: 1, IsHedge: true, HedgePrimarySymbol: "ETH",
		OwnerStrategyID: "a", OpenedAt: time.Now().UTC(),
	}
	ss.RiskState.ConsecutiveLosses = 2
	// Losing hedge close: short from 50000, closed at 51000 → -100 gross.
	applyHyperliquidCircuitCloseFill(ss, "BTC", 0.1, 51000, 1.0, 0, 777, "hedge_sync")
	if ss.RiskState.ConsecutiveLosses != 2 {
		t.Fatalf("hedge close mutated loss streak: %d", ss.RiskState.ConsecutiveLosses)
	}
	if ss.RiskState.DailyPnL >= 0 {
		t.Fatalf("hedge loss missing from daily PnL: %g", ss.RiskState.DailyPnL)
	}
	if _, open := ss.Positions["BTC"]; open {
		t.Fatal("hedge position not removed on full close")
	}
	// Non-hedge closes keep the legacy streak behavior.
	ss.Positions["ETH"] = &Position{Symbol: "ETH", Quantity: 1, AvgCost: 2500, Side: "long", Multiplier: 1, OwnerStrategyID: "a"}
	applyHyperliquidCircuitCloseFill(ss, "ETH", 1, 2400, 1.0, 0, 778, "")
	if ss.RiskState.ConsecutiveLosses != 3 {
		t.Fatalf("primary losing close should extend streak: %d", ss.RiskState.ConsecutiveLosses)
	}
}

// --- circuit breaker / kill switch coupling ---------------------------------------

func TestCircuitBreakerPendingIncludesHedgeLeg(t *testing.T) {
	sc := hedgeTestConfig("a", "ETH", "BTC")
	ss := hedgeTestState("a")
	ss.Positions["ETH"] = &Position{Symbol: "ETH", Quantity: 4, AvgCost: 2500, Side: "long", Multiplier: 1}
	ss.Positions["BTC"] = &Position{Symbol: "BTC", Quantity: 0.1, AvgCost: 50000, Side: "short", Multiplier: 1, IsHedge: true, HedgePrimarySymbol: "ETH", HedgeCoveredPrimaryQty: 4}
	assist := &PlatformRiskAssist{
		HLPositions: []HLPosition{{Coin: "ETH", Size: 4}, {Coin: "BTC", Size: -0.1}},
		HLLiveAll:   []StrategyConfig{sc},
	}
	setHyperliquidCircuitBreakerPending(&sc, ss, assist)
	pending := ss.RiskState.getPendingCircuitClose(PlatformPendingCloseHyperliquid)
	if pending == nil || len(pending.Symbols) != 2 {
		t.Fatalf("pending = %+v, want primary + hedge symbols", pending)
	}
	if pending.Symbols[0].Symbol != "ETH" || pending.Symbols[1].Symbol != "BTC" || pending.Symbols[1].Size != 0.1 {
		t.Fatalf("pending symbols = %+v", pending.Symbols)
	}
}

func TestKillSwitchClosesHedgeCoins(t *testing.T) {
	sc := hedgeTestConfig("a", "ETH", "BTC")
	positions := []HLPosition{{Coin: "ETH", Size: 4}, {Coin: "BTC", Size: -0.1}, {Coin: "DOGE", Size: 100}}
	var closed []string
	closer := func(coin string, partial *float64, cancelOIDs []int64) (*HyperliquidCloseResult, error) {
		closed = append(closed, coin)
		return &HyperliquidCloseResult{Close: &HyperliquidClose{Symbol: coin, Fill: &HyperliquidCloseFill{AvgPx: 1, TotalSz: 1}}}, nil
	}
	report := forceCloseHyperliquidLive(context.Background(), positions, []StrategyConfig{sc}, closer, nil, nil)
	if len(report.Errors) != 0 {
		t.Fatalf("errors: %v", report.Errors)
	}
	if !containsString(closed, "ETH") || !containsString(closed, "BTC") {
		t.Fatalf("closed = %v, want ETH and BTC (hedge coin)", closed)
	}
	if containsString(closed, "DOGE") {
		t.Fatalf("closed unowned DOGE: %v", closed)
	}

	// State-claimed hedge coin with no config hedge block (removed across a
	// restart) still closes via the extra-coins roster.
	scNoHedge := hedgeTestConfig("a", "ETH", "BTC")
	scNoHedge.Hedge = nil
	closed = nil
	report = forceCloseHyperliquidLive(context.Background(), positions, []StrategyConfig{scNoHedge}, closer, nil, []string{"BTC"})
	if !containsString(closed, "BTC") {
		t.Fatalf("state-claimed hedge coin not closed: %v", closed)
	}
}

func TestApplyHedgeKillSwitchCloseFill(t *testing.T) {
	sc := hedgeTestConfig("a", "ETH", "BTC")
	ss := hedgeTestState("a")
	ss.Positions["BTC"] = &Position{Symbol: "BTC", Quantity: 0.1, AvgCost: 50000, Side: "short", Multiplier: 1, IsHedge: true, HedgePrimarySymbol: "ETH", OwnerStrategyID: "a"}
	fills := map[string]HyperliquidCloseFill{
		"BTC": {AvgPx: 49000, TotalSz: 0.1, OID: 42, Fee: 0.5},
	}
	if !applyHedgeKillSwitchCloseFill(ss, sc, fills) {
		t.Fatal("hedge kill-switch fill not booked")
	}
	if _, open := ss.Positions["BTC"]; open {
		t.Fatal("hedge position not cleared")
	}
	last := ss.TradeHistory[len(ss.TradeHistory)-1]
	if !last.IsClose || last.Symbol != "BTC" || last.ExchangeFee != 0.5 || math.Abs(last.RealizedPnL-100) > 1e-9 {
		t.Fatalf("hedge kill-switch close trade = %+v", last)
	}
}

// --- reconcile --------------------------------------------------------------------

func TestReconcileHedgeLegExternalClose(t *testing.T) {
	sc := hedgeTestConfig("a", "ETH", "BTC")
	ss := hedgeTestState("a")
	ss.Positions["BTC"] = &Position{Symbol: "BTC", Quantity: 0.1, AvgCost: 50000, Side: "short", Multiplier: 1, IsHedge: true, HedgePrimarySymbol: "ETH", OwnerStrategyID: "a"}
	resolve := func(coin string, oid int64, qty float64) (HLFillLookup, bool) {
		return HLFillLookup{Px: 48000, FilledQty: qty, Fee: 0.4}, true
	}
	changed := reconcileHedgeLegForStrategy(sc, ss, nil, resolve, hedgeSilentLogger("a"))
	if !changed {
		t.Fatal("expected reconcile to book the external hedge close")
	}
	if _, open := ss.Positions["BTC"]; open {
		t.Fatal("hedge position not removed after external close")
	}
	last := ss.TradeHistory[len(ss.TradeHistory)-1]
	if !last.IsClose || last.Symbol != "BTC" {
		t.Fatalf("external close trade = %+v", last)
	}
	if math.Abs(last.RealizedPnL-(0.1*(50000-48000))) > 1e-6 {
		t.Fatalf("external close gross PnL = %g, want 200", last.RealizedPnL)
	}
}

func TestReconcileHedgeLegResyncsDrift(t *testing.T) {
	sc := hedgeTestConfig("a", "ETH", "BTC")
	ss := hedgeTestState("a")
	ss.Positions["BTC"] = &Position{Symbol: "BTC", Quantity: 0.1, AvgCost: 50000, Side: "short", Multiplier: 0, IsHedge: true, HedgePrimarySymbol: "ETH", HedgeCoveredPrimaryQty: 4, OwnerStrategyID: "a"}
	onChain := []HLPosition{{Coin: "BTC", Size: -0.08, EntryPrice: 50100, Leverage: 2}}
	resolve := func(coin string, oid int64, qty float64) (HLFillLookup, bool) { return HLFillLookup{}, false }
	if !reconcileHedgeLegForStrategy(sc, ss, onChain, resolve, hedgeSilentLogger("a")) {
		t.Fatal("expected drift resync")
	}
	pos := ss.Positions["BTC"]
	if pos.Quantity != 0.08 || pos.Side != "short" || pos.AvgCost != 50100 || pos.Multiplier != 1 {
		t.Fatalf("resynced pos = %+v", pos)
	}
	// A qty resync rescales the covered watermark proportionally (review
	// finding on #1333): 4 covered by 0.10 → 0.08 on-chain ⇒ covers 3.2.
	// Zeroing (adopt) would mark the shrunken hedge as FULL coverage and
	// leave the position silently under-hedged; rescaling re-triggers the
	// shortfall add while still absorbing lost-add upward drift.
	if math.Abs(pos.HedgeCoveredPrimaryQty-3.2) > 1e-9 {
		t.Fatalf("covered watermark = %g after drift resync, want proportional 3.2", pos.HedgeCoveredPrimaryQty)
	}
}

// --- direction validation exclusion ------------------------------------------------

func TestValidatePerpsDirectionConfigSkipsHedgeLegs(t *testing.T) {
	sc := hedgeTestConfig("a", "ETH", "BTC")
	sc.Direction = "long"
	state := &AppState{Strategies: map[string]*StrategyState{"a": {
		ID: "a",
		Positions: map[string]*Position{
			"ETH": {Symbol: "ETH", Quantity: 4, Side: "long", OwnerStrategyID: "a"},
			// Inverse hedge: would be a "state-vs-config gap" without the skip.
			"BTC": {Symbol: "BTC", Quantity: 0.1, Side: "short", IsHedge: true, HedgePrimarySymbol: "ETH", OwnerStrategyID: "a"},
		},
	}}}
	warnings := ValidatePerpsDirectionConfig(state, &Config{Strategies: []StrategyConfig{sc}})
	if len(warnings) != 0 {
		t.Fatalf("hedge leg flagged as direction conflict: %v", warnings)
	}
}

// --- orphaned-hedge startup warnings -----------------------------------------------

func TestValidateHedgeStateConsistency(t *testing.T) {
	mk := func() *AppState {
		return &AppState{Strategies: map[string]*StrategyState{"a": {
			ID: "a",
			Positions: map[string]*Position{
				"BTC": {Symbol: "BTC", Quantity: 0.1, Side: "short", IsHedge: true, HedgePrimarySymbol: "ETH"},
			},
		}}}
	}
	scOK := hedgeTestConfig("a", "ETH", "BTC")
	if w := validateHedgeStateConsistency(mk(), &Config{Strategies: []StrategyConfig{scOK}}); len(w) != 0 {
		t.Fatalf("healthy hedge flagged: %v", w)
	}
	scDisabled := hedgeTestConfig("a", "ETH", "BTC")
	scDisabled.Hedge = nil
	if w := validateHedgeStateConsistency(mk(), &Config{Strategies: []StrategyConfig{scDisabled}}); len(w) != 1 || !strings.Contains(w[0], "hedge block is disabled/removed") {
		t.Fatalf("disabled-block warning = %v", w)
	}
	scMoved := hedgeTestConfig("a", "ETH", "SOL")
	if w := validateHedgeStateConsistency(mk(), &Config{Strategies: []StrategyConfig{scMoved}}); len(w) != 1 || !strings.Contains(w[0], "coin mismatch") {
		t.Fatalf("coin-mismatch warning = %v", w)
	}
	if w := validateHedgeStateConsistency(mk(), &Config{}); len(w) != 1 || !strings.Contains(w[0], "no longer configured") {
		t.Fatalf("unconfigured warning = %v", w)
	}
}

// --- persistence --------------------------------------------------------------------

func TestHedgePositionPersistenceRoundTrip(t *testing.T) {
	db := openTestDB(t)
	now := time.Now().UTC()
	state := &AppState{Strategies: map[string]*StrategyState{"a": {
		ID: "a", Type: "perps", Platform: "hyperliquid", Cash: 1000, InitialCapital: 1000,
		Positions: map[string]*Position{
			"BTC": {
				Symbol: "BTC", Quantity: 0.1, InitialQuantity: 0.1, AvgCost: 50000,
				Side: "short", Multiplier: 1, Leverage: 2, OwnerStrategyID: "a", OpenedAt: now,
				IsHedge: true, HedgePrimarySymbol: "ETH", HedgeCoveredPrimaryQty: 4,
			},
		},
		OptionPositions: map[string]*OptionPosition{},
	}}}
	if err := db.SaveState(state); err != nil {
		t.Fatalf("SaveState: %v", err)
	}
	loaded, err := db.LoadState()
	if err != nil {
		t.Fatalf("LoadState: %v", err)
	}
	pos := loaded.Strategies["a"].Positions["BTC"]
	if pos == nil {
		t.Fatal("hedge position not loaded")
	}
	if !pos.IsHedge || pos.HedgePrimarySymbol != "ETH" || pos.HedgeCoveredPrimaryQty != 4 {
		t.Fatalf("hedge metadata lost on round trip: %+v", pos)
	}
}

func containsString(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}

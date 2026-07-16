package main

import (
	"testing"
)

func testHedgeStrategy(id, coin, hedgeCoin string) StrategyConfig {
	return StrategyConfig{
		ID:       id,
		Type:     "perps",
		Platform: "hyperliquid",
		Script:   "shared_scripts/check_hyperliquid.py",
		Args:     []string{"tema_cross_bd", coin, "1h", "--mode=live"},
		Hedge: &HedgeConfig{
			Enabled:    true,
			Symbol:     hedgeCoin,
			Side:       HedgeSideInverse,
			Ratio:      1.0,
			MarginMode: "isolated",
			Leverage:   3,
		},
	}
}

// ─── pure helpers ────────────────────────────────────────────────────────────

func TestHedgeSideForPrimary(t *testing.T) {
	cases := []struct {
		primary string
		want    string
	}{
		{"long", "short"},
		{"short", "long"},
		{"", ""},
		{"flat", ""},
	}
	for _, c := range cases {
		if got := hedgeSideForPrimary(c.primary); got != c.want {
			t.Errorf("hedgeSideForPrimary(%q) = %q, want %q", c.primary, got, c.want)
		}
	}
}

func TestHedgeCoinForStrategy(t *testing.T) {
	cases := []struct {
		name string
		sc   StrategyConfig
		want string
	}{
		{"disabled", StrategyConfig{Hedge: nil}, ""},
		{"explicit disabled", StrategyConfig{Hedge: &HedgeConfig{Enabled: false, Symbol: "BTC"}}, ""},
		{"bare ticker", StrategyConfig{Hedge: &HedgeConfig{Enabled: true, Symbol: "BTC"}}, "BTC"},
		{"ccxt form", StrategyConfig{Hedge: &HedgeConfig{Enabled: true, Symbol: "BTC/USDC:USDC"}}, "BTC"},
		{"lowercase", StrategyConfig{Hedge: &HedgeConfig{Enabled: true, Symbol: "btc"}}, "BTC"},
		{"whitespace", StrategyConfig{Hedge: &HedgeConfig{Enabled: true, Symbol: "  btc  "}}, "BTC"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := hedgeCoinForStrategy(c.sc); got != c.want {
				t.Errorf("hedgeCoinForStrategy(%+v) = %q, want %q", c.sc.Hedge, got, c.want)
			}
		})
	}
}

func TestHedgeOpenQty(t *testing.T) {
	cases := []struct {
		name                                    string
		primaryQty, primaryPx, ratio, hedgeMark float64
		want                                    float64
	}{
		{"basic 1:1 ratio", 1.0, 3000, 1.0, 60000, 0.05},
		{"half ratio", 1.0, 3000, 0.5, 60000, 0.025},
		{"double ratio", 1.0, 3000, 2.0, 60000, 0.1},
		{"zero qty refuses", 0, 3000, 1.0, 60000, 0},
		{"negative qty refuses", -1, 3000, 1.0, 60000, 0},
		{"zero px refuses", 1.0, 0, 1.0, 60000, 0},
		{"zero ratio refuses", 1.0, 3000, 0, 60000, 0},
		{"zero mark refuses", 1.0, 3000, 1.0, 0, 0},
		{"negative mark refuses", 1.0, 3000, 1.0, -60000, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := hedgeOpenQty(c.primaryQty, c.primaryPx, c.ratio, c.hedgeMark)
			if diff := got - c.want; diff > 1e-9 || diff < -1e-9 {
				t.Errorf("hedgeOpenQty(%v,%v,%v,%v) = %v, want %v", c.primaryQty, c.primaryPx, c.ratio, c.hedgeMark, got, c.want)
			}
		})
	}
}

func TestHedgeCoherenceDecisionIgnoresMarkDrift(t *testing.T) {
	// Regression for review round 1, finding 1: syncHedgeCoherence must
	// NEVER be triggered by primary/hedge coins simply moving at different
	// rates — only by an actual quantity desync. Long BTC 1.0, hedge short
	// ETH 25 (ratio 1.0, sized at open when BTC=$100k/ETH=$4k). BTC then
	// rallies to $110k while ETH stays flat — a purely mark-driven
	// "expected" recompute (the pre-fix bug) would read this as
	// under-hedged and trim the primary by ~9%. The ratio-based decision
	// must see zero drift and take no action at all.
	ratio := 25.0 / 1.0 // captured once at open, exactly as applyHedgeOpen would
	needsBootstrap, reduceSymbol, _, _ := hedgeCoherenceDecision(1.0, 25.0, ratio, "BTC", "ETH")
	if needsBootstrap {
		t.Fatal("expected an established basis, not a bootstrap")
	}
	if reduceSymbol != "" {
		t.Errorf("expected no action on mark drift alone, got reduceSymbol=%q", reduceSymbol)
	}
}

func TestHedgeCoherenceDecisionBootstrap(t *testing.T) {
	// ratio<=0 means no basis has been established (brand-new position, or
	// one that predates HedgeQtyRatio) — must never be treated as "expect
	// zero hedge" (which would trigger an immediate, spurious full reduce).
	needsBootstrap, reduceSymbol, reduceQty, _ := hedgeCoherenceDecision(1.0, 25.0, 0, "BTC", "ETH")
	if !needsBootstrap {
		t.Fatal("expected needsBootstrap=true for ratio<=0")
	}
	if reduceSymbol != "" || reduceQty != 0 {
		t.Errorf("bootstrap must never also propose a reduction, got symbol=%q qty=%v", reduceSymbol, reduceQty)
	}
}

func TestHedgeCoherenceDecisionOverAndUnderHedged(t *testing.T) {
	ratio := 25.0 / 1.0
	t.Run("over-hedged reduces the hedge to expected", func(t *testing.T) {
		// Primary shrank (e.g. an on-chain SL fill) without a mirrored
		// hedge reduction: hedge is now too big for the smaller primary.
		needsBootstrap, reduceSymbol, reduceQty, expected := hedgeCoherenceDecision(0.5, 25.0, ratio, "BTC", "ETH")
		if needsBootstrap {
			t.Fatal("unexpected bootstrap")
		}
		if reduceSymbol != "ETH" {
			t.Errorf("expected to reduce the hedge (ETH), got %q", reduceSymbol)
		}
		wantExpected := 12.5
		if diff := expected - wantExpected; diff > 1e-9 || diff < -1e-9 {
			t.Errorf("expected = %v, want %v", expected, wantExpected)
		}
		wantReduceQty := 25.0 - wantExpected
		if diff := reduceQty - wantReduceQty; diff > 1e-9 || diff < -1e-9 {
			t.Errorf("reduceQty = %v, want %v", reduceQty, wantReduceQty)
		}
	})
	t.Run("under-hedged reduces the primary, never adds hedge exposure", func(t *testing.T) {
		// Hedge shrank externally (liquidation/manual close) without the
		// primary changing: primary now exceeds what the actual hedge
		// covers.
		needsBootstrap, reduceSymbol, reduceQty, _ := hedgeCoherenceDecision(1.0, 10.0, ratio, "BTC", "ETH")
		if needsBootstrap {
			t.Fatal("unexpected bootstrap")
		}
		if reduceSymbol != "BTC" {
			t.Errorf("expected to reduce the primary (BTC), got %q", reduceSymbol)
		}
		wantTargetPrimaryQty := 10.0 / ratio
		wantReduceQty := 1.0 - wantTargetPrimaryQty
		if diff := reduceQty - wantReduceQty; diff > 1e-9 || diff < -1e-9 {
			t.Errorf("reduceQty = %v, want %v", reduceQty, wantReduceQty)
		}
	})
	t.Run("within tolerance takes no action", func(t *testing.T) {
		expected := ratio * 1.0
		withinTol := expected * (hedgeCoverageTolerance / 2)
		needsBootstrap, reduceSymbol, _, _ := hedgeCoherenceDecision(1.0, expected+withinTol, ratio, "BTC", "ETH")
		if needsBootstrap {
			t.Fatal("unexpected bootstrap")
		}
		if reduceSymbol != "" {
			t.Errorf("expected no action within tolerance, got %q", reduceSymbol)
		}
	})
	t.Run("malformed inputs take no action", func(t *testing.T) {
		if _, reduceSymbol, _, _ := hedgeCoherenceDecision(0, 25.0, ratio, "BTC", "ETH"); reduceSymbol != "" {
			t.Errorf("zero primary qty must not propose a reduction, got %q", reduceSymbol)
		}
		if _, reduceSymbol, _, _ := hedgeCoherenceDecision(1.0, 0, ratio, "BTC", "ETH"); reduceSymbol != "" {
			t.Errorf("zero hedge qty must not propose a reduction, got %q", reduceSymbol)
		}
	})
}

func TestHedgeConfigEqual(t *testing.T) {
	a := &HedgeConfig{Enabled: true, Symbol: "BTC", Side: "inverse", Ratio: 1.0, MarginMode: "isolated", Leverage: 3}
	b := &HedgeConfig{Enabled: true, Symbol: "BTC", Side: "inverse", Ratio: 1.0, MarginMode: "isolated", Leverage: 3}
	c := &HedgeConfig{Enabled: true, Symbol: "ETH", Side: "inverse", Ratio: 1.0, MarginMode: "isolated", Leverage: 3}

	if !hedgeConfigEqual(nil, nil) {
		t.Error("hedgeConfigEqual(nil, nil) = false, want true")
	}
	if hedgeConfigEqual(a, nil) || hedgeConfigEqual(nil, a) {
		t.Error("hedgeConfigEqual(x, nil) and (nil, x) must be false")
	}
	if !hedgeConfigEqual(a, b) {
		t.Error("hedgeConfigEqual(a, b) = false, want true for identical configs")
	}
	if hedgeConfigEqual(a, c) {
		t.Error("hedgeConfigEqual(a, c) = true, want false for differing symbol")
	}
}

// ─── hedgePreOpenGate ────────────────────────────────────────────────────────

func TestHedgePreOpenGate(t *testing.T) {
	sc := testHedgeStrategy("hl-eth", "ETH", "BTC")

	t.Run("refuses empty coin", func(t *testing.T) {
		ok, reason := hedgePreOpenGate(sc, "", 60000, nil, nil)
		if ok || reason == "" {
			t.Errorf("expected refusal with a reason, got ok=%v reason=%q", ok, reason)
		}
	})

	t.Run("refuses missing mark", func(t *testing.T) {
		ok, _ := hedgePreOpenGate(sc, "BTC", 0, nil, nil)
		if ok {
			t.Error("expected refusal with mark=0")
		}
	})

	t.Run("refuses negative mark", func(t *testing.T) {
		ok, _ := hedgePreOpenGate(sc, "BTC", -1, nil, nil)
		if ok {
			t.Error("expected refusal with negative mark")
		}
	})

	t.Run("refuses existing virtual hedge", func(t *testing.T) {
		existing := &Position{Symbol: "BTC", Quantity: 0.01}
		ok, _ := hedgePreOpenGate(sc, "BTC", 60000, nil, existing)
		if ok {
			t.Error("expected refusal when a virtual hedge position already exists")
		}
	})

	t.Run("allows zero-quantity stale virtual hedge", func(t *testing.T) {
		existing := &Position{Symbol: "BTC", Quantity: 0}
		ok, reason := hedgePreOpenGate(sc, "BTC", 60000, nil, existing)
		if !ok {
			t.Errorf("expected gate to pass with a flat stale virtual row, got reason=%q", reason)
		}
	})

	t.Run("refuses foreign on-chain position with no virtual row", func(t *testing.T) {
		hlPositions := []HLPosition{{Coin: "BTC", Size: 0.5}}
		ok, reason := hedgePreOpenGate(sc, "BTC", 60000, hlPositions, nil)
		if ok || reason == "" {
			t.Errorf("expected refusal for foreign on-chain position, got ok=%v reason=%q", ok, reason)
		}
	})

	t.Run("allows flat on-chain position", func(t *testing.T) {
		hlPositions := []HLPosition{{Coin: "BTC", Size: 0}}
		ok, reason := hedgePreOpenGate(sc, "BTC", 60000, hlPositions, nil)
		if !ok {
			t.Errorf("expected gate to pass with a flat on-chain position, got reason=%q", reason)
		}
	})

	t.Run("allows clean flat state", func(t *testing.T) {
		ok, reason := hedgePreOpenGate(sc, "BTC", 60000, nil, nil)
		if !ok {
			t.Errorf("expected gate to pass with no on-chain or virtual state, got reason=%q", reason)
		}
	})
}

// ─── config validation ───────────────────────────────────────────────────────

func TestHyperliquidHedgeConfigErrors(t *testing.T) {
	t.Run("valid single hedge strategy passes", func(t *testing.T) {
		strategies := []StrategyConfig{testHedgeStrategy("hl-eth", "ETH", "BTC")}
		if errs := hyperliquidHedgeConfigErrors(strategies); len(errs) != 0 {
			t.Errorf("expected no errors, got %v", errs)
		}
	})

	t.Run("hedge coin equal to own primary coin is rejected", func(t *testing.T) {
		strategies := []StrategyConfig{testHedgeStrategy("hl-eth", "ETH", "ETH")}
		errs := hyperliquidHedgeConfigErrors(strategies)
		if len(errs) == 0 {
			t.Fatal("expected a rejection for hedge coin == primary coin")
		}
	})

	t.Run("hedge coin colliding with another strategy's primary coin is rejected", func(t *testing.T) {
		strategies := []StrategyConfig{
			testHedgeStrategy("hl-eth", "ETH", "BTC"),
			{ID: "hl-btc", Type: "perps", Platform: "hyperliquid", Script: "x", Args: []string{"tema_cross_bd", "BTC", "1h", "--mode=live"}},
		}
		errs := hyperliquidHedgeConfigErrors(strategies)
		if len(errs) == 0 {
			t.Fatal("expected a rejection for hedge coin colliding with another strategy's primary coin")
		}
	})

	t.Run("hedge-vs-hedge collision is rejected", func(t *testing.T) {
		strategies := []StrategyConfig{
			testHedgeStrategy("hl-eth", "ETH", "SOL"),
			testHedgeStrategy("hl-avax", "AVAX", "SOL"),
		}
		errs := hyperliquidHedgeConfigErrors(strategies)
		if len(errs) == 0 {
			t.Fatal("expected a rejection for two strategies sharing a hedge coin")
		}
	})

	t.Run("direction=both is rejected", func(t *testing.T) {
		sc := testHedgeStrategy("hl-eth", "ETH", "BTC")
		sc.Direction = DirectionBoth
		errs := hyperliquidHedgeConfigErrors([]StrategyConfig{sc})
		if len(errs) == 0 {
			t.Fatal("expected a rejection for direction=both")
		}
	})

	t.Run("non-live is rejected", func(t *testing.T) {
		sc := testHedgeStrategy("hl-eth", "ETH", "BTC")
		sc.Args = []string{"tema_cross_bd", "ETH", "1h"} // no --mode=live
		errs := hyperliquidHedgeConfigErrors([]StrategyConfig{sc})
		if len(errs) == 0 {
			t.Fatal("expected a rejection for non-live hedge-enabled strategy")
		}
	})

	t.Run("manual type is rejected", func(t *testing.T) {
		sc := testHedgeStrategy("hl-eth", "ETH", "BTC")
		sc.Type = "manual"
		errs := hyperliquidHedgeConfigErrors([]StrategyConfig{sc})
		if len(errs) == 0 {
			t.Fatal("expected a rejection for type=manual")
		}
	})

	t.Run("bad side value is rejected", func(t *testing.T) {
		sc := testHedgeStrategy("hl-eth", "ETH", "BTC")
		sc.Hedge.Side = "same"
		errs := hyperliquidHedgeConfigErrors([]StrategyConfig{sc})
		if len(errs) == 0 {
			t.Fatal("expected a rejection for hedge.side != inverse")
		}
	})

	t.Run("ratio out of bounds is rejected", func(t *testing.T) {
		sc := testHedgeStrategy("hl-eth", "ETH", "BTC")
		sc.Hedge.Ratio = 10
		errs := hyperliquidHedgeConfigErrors([]StrategyConfig{sc})
		if len(errs) == 0 {
			t.Fatal("expected a rejection for hedge.ratio > max")
		}
	})

	t.Run("missing margin_mode is rejected", func(t *testing.T) {
		sc := testHedgeStrategy("hl-eth", "ETH", "BTC")
		sc.Hedge.MarginMode = ""
		errs := hyperliquidHedgeConfigErrors([]StrategyConfig{sc})
		if len(errs) == 0 {
			t.Fatal("expected a rejection for missing hedge.margin_mode")
		}
	})

	t.Run("missing leverage is rejected", func(t *testing.T) {
		sc := testHedgeStrategy("hl-eth", "ETH", "BTC")
		sc.Hedge.Leverage = 0
		errs := hyperliquidHedgeConfigErrors([]StrategyConfig{sc})
		if len(errs) == 0 {
			t.Fatal("expected a rejection for missing hedge.leverage")
		}
	})

	t.Run("disabled hedge on any strategy never errors", func(t *testing.T) {
		sc := testHedgeStrategy("hl-eth", "ETH", "")
		sc.Hedge.Enabled = false
		if errs := hyperliquidHedgeConfigErrors([]StrategyConfig{sc}); len(errs) != 0 {
			t.Errorf("expected no errors for disabled hedge, got %v", errs)
		}
	})
}

// ─── apply functions (state mutation, no subprocess I/O) ───────────────────

func TestApplyHedgeOpen(t *testing.T) {
	sc := testHedgeStrategy("hl-eth", "ETH", "BTC")
	s := &StrategyState{
		ID:        "hl-eth",
		Cash:      10000,
		Positions: map[string]*Position{"ETH": {Symbol: "ETH", Quantity: 1, Side: "long", OwnerStrategyID: "hl-eth"}},
	}
	fill := &HyperliquidFill{AvgPx: 60000, TotalSz: 0.05, Fee: 3, OID: 42}
	trade := applyHedgeOpen(s, sc, "ETH", "BTC", "short", fill)
	if trade == nil {
		t.Fatal("applyHedgeOpen returned nil trade")
	}
	hedgePos := s.Positions["BTC"]
	if hedgePos == nil {
		t.Fatal("hedge position was not created")
	}
	if !hedgePos.IsHedge || hedgePos.HedgeFor != "ETH" || hedgePos.Side != "short" {
		t.Errorf("hedge position metadata wrong: %+v", hedgePos)
	}
	if hedgePos.Quantity != 0.05 || hedgePos.AvgCost != 60000 {
		t.Errorf("hedge position fill data wrong: qty=%v avgCost=%v", hedgePos.Quantity, hedgePos.AvgCost)
	}
	if s.Positions["ETH"].HedgeSymbol != "BTC" {
		t.Errorf("primary HedgeSymbol not stamped: %q", s.Positions["ETH"].HedgeSymbol)
	}
	if diff := s.Positions["ETH"].HedgeQtyRatio - 0.05; diff > 1e-9 || diff < -1e-9 {
		t.Errorf("primary HedgeQtyRatio not stamped: got %v, want 0.05", s.Positions["ETH"].HedgeQtyRatio)
	}
	if s.Cash != 10000-3 {
		t.Errorf("cash not debited by fee: got %v, want %v", s.Cash, 10000-3.0)
	}
	if len(s.TradeHistory) != 1 {
		t.Errorf("expected 1 trade recorded, got %d", len(s.TradeHistory))
	}
}

func TestApplyHedgeOpenRejectsZeroFill(t *testing.T) {
	sc := testHedgeStrategy("hl-eth", "ETH", "BTC")
	s := &StrategyState{ID: "hl-eth", Positions: map[string]*Position{}}
	if trade := applyHedgeOpen(s, sc, "ETH", "BTC", "short", nil); trade != nil {
		t.Error("expected nil trade for nil fill")
	}
	if trade := applyHedgeOpen(s, sc, "ETH", "BTC", "short", &HyperliquidFill{AvgPx: 0, TotalSz: 1}); trade != nil {
		t.Error("expected nil trade for zero price")
	}
	if len(s.Positions) != 0 {
		t.Errorf("expected no positions created on rejected fill, got %d", len(s.Positions))
	}
}

func TestApplyHedgeScaleIn(t *testing.T) {
	sc := testHedgeStrategy("hl-eth", "ETH", "BTC")
	_ = sc
	s := &StrategyState{
		ID:   "hl-eth",
		Cash: 10000,
		Positions: map[string]*Position{
			"ETH": {Symbol: "ETH", Quantity: 1.2, Side: "long", OwnerStrategyID: "hl-eth", HedgeSymbol: "BTC", HedgeQtyRatio: 0.05},
			"BTC": {Symbol: "BTC", Quantity: 0.05, InitialQuantity: 0.05, AvgCost: 60000, Side: "short", IsHedge: true, HedgeFor: "ETH"},
		},
	}
	fill := &HyperliquidFill{AvgPx: 61000, TotalSz: 0.02, Fee: 1.5, OID: 43}
	trade := applyHedgeScaleIn(s, "ETH", "BTC", fill)
	if trade == nil {
		t.Fatal("applyHedgeScaleIn returned nil trade")
	}
	pos := s.Positions["BTC"]
	if pos.Quantity != 0.07 {
		t.Errorf("hedge quantity not blended: got %v, want 0.07", pos.Quantity)
	}
	if pos.InitialQuantity != 0.07 {
		t.Errorf("hedge InitialQuantity not grown: got %v, want 0.07", pos.InitialQuantity)
	}
	// Regression for review round 1, finding 1: the primary's HedgeQtyRatio
	// must be re-anchored to the post-add quantities (an add's hedge sizing
	// uses the price at add time, which generally differs from open time),
	// not left at its pre-add value — else syncHedgeCoherence would misread
	// this legitimate, already-mirrored add as desync next cycle.
	wantRatio := 0.07 / 1.2
	if diff := s.Positions["ETH"].HedgeQtyRatio - wantRatio; diff > 1e-9 || diff < -1e-9 {
		t.Errorf("primary HedgeQtyRatio not re-anchored: got %v, want %v", s.Positions["ETH"].HedgeQtyRatio, wantRatio)
	}
}

func TestApplyHedgeScaleInNoOpWithoutExistingPosition(t *testing.T) {
	s := &StrategyState{ID: "hl-eth", Positions: map[string]*Position{}}
	fill := &HyperliquidFill{AvgPx: 61000, TotalSz: 0.02, Fee: 1.5}
	if trade := applyHedgeScaleIn(s, "ETH", "BTC", fill); trade != nil {
		t.Error("expected nil trade when no existing hedge position")
	}
}

// ─── reconcile lane ──────────────────────────────────────────────────────────

func TestReconcileHedgeLegsForStrategy(t *testing.T) {
	sc := testHedgeStrategy("hl-eth", "ETH", "BTC")
	logger := newTestLogger(t)

	t.Run("no-op when hedge disabled", func(t *testing.T) {
		disabled := sc
		disabled.Hedge = nil
		s := &StrategyState{ID: "hl-eth", Positions: map[string]*Position{}}
		if changed := reconcileHedgeLegsForStrategy(disabled, s, nil, noFillFeeResolver, logger); changed {
			t.Error("expected no-op when hedge disabled")
		}
	})

	t.Run("no-op when no virtual hedge position", func(t *testing.T) {
		s := &StrategyState{ID: "hl-eth", Positions: map[string]*Position{}}
		if changed := reconcileHedgeLegsForStrategy(sc, s, nil, noFillFeeResolver, logger); changed {
			t.Error("expected no-op with no virtual hedge position")
		}
	})

	t.Run("books external full close when on-chain is flat", func(t *testing.T) {
		s := &StrategyState{
			ID:   "hl-eth",
			Cash: 1000,
			Positions: map[string]*Position{
				"ETH": {Symbol: "ETH", Quantity: 1, Side: "long", HedgeSymbol: "BTC"},
				"BTC": {Symbol: "BTC", Quantity: 0.05, InitialQuantity: 0.05, AvgCost: 60000, Side: "short", IsHedge: true, HedgeFor: "ETH"},
			},
		}
		changed := reconcileHedgeLegsForStrategy(sc, s, nil /* no on-chain positions at all */, noFillFeeResolver, logger)
		if !changed {
			t.Fatal("expected external close to be booked")
		}
		if _, ok := s.Positions["BTC"]; ok {
			t.Error("hedge position should have been removed after external close")
		}
		// Regression for review round 2, finding 2: HedgeSymbol must stay
		// set after the hedge disappears externally — it's exactly what
		// gates syncHedgeCoherence's "primary exists, hedge gone" reduce-
		// only close. Clearing it here would silently downgrade the primary
		// to a permanently-unhedged ordinary position instead of letting
		// coherence drive it flat next cycle.
		if s.Positions["ETH"].HedgeSymbol != "BTC" {
			t.Errorf("primary HedgeSymbol must remain set so coherence can close it, got %q", s.Positions["ETH"].HedgeSymbol)
		}
	})

	t.Run("no-op when on-chain matches virtual", func(t *testing.T) {
		s := &StrategyState{
			ID:   "hl-eth",
			Cash: 1000,
			Positions: map[string]*Position{
				"BTC": {Symbol: "BTC", Quantity: 0.05, InitialQuantity: 0.05, AvgCost: 60000, Side: "short", IsHedge: true, HedgeFor: "ETH"},
			},
		}
		hlPositions := []HLPosition{{Coin: "BTC", Size: -0.05}}
		changed := reconcileHedgeLegsForStrategy(sc, s, hlPositions, noFillFeeResolver, logger)
		if changed {
			t.Error("expected no-op when on-chain matches virtual exactly")
		}
		if s.Positions["BTC"].Quantity != 0.05 {
			t.Errorf("hedge quantity should be unchanged, got %v", s.Positions["BTC"].Quantity)
		}
	})

	t.Run("books external partial reduce when on-chain shrank", func(t *testing.T) {
		s := &StrategyState{
			ID:   "hl-eth",
			Cash: 1000,
			Positions: map[string]*Position{
				"BTC": {Symbol: "BTC", Quantity: 0.05, InitialQuantity: 0.05, AvgCost: 60000, Side: "short", IsHedge: true, HedgeFor: "ETH"},
			},
		}
		hlPositions := []HLPosition{{Coin: "BTC", Size: -0.03}}
		changed := reconcileHedgeLegsForStrategy(sc, s, hlPositions, noFillFeeResolver, logger)
		if !changed {
			t.Fatal("expected external partial reduce to be booked")
		}
		if diff := s.Positions["BTC"].Quantity - 0.03; diff > 1e-9 || diff < -1e-9 {
			t.Errorf("hedge quantity after partial reduce = %v, want 0.03", s.Positions["BTC"].Quantity)
		}
	})

	t.Run("clears stale row on direction flip", func(t *testing.T) {
		s := &StrategyState{
			ID:   "hl-eth",
			Cash: 1000,
			Positions: map[string]*Position{
				"ETH": {Symbol: "ETH", Quantity: 1, Side: "long", HedgeSymbol: "BTC"},
				"BTC": {Symbol: "BTC", Quantity: 0.05, InitialQuantity: 0.05, AvgCost: 60000, Side: "short", IsHedge: true, HedgeFor: "ETH"},
			},
		}
		// On-chain now shows a LONG BTC position — direction no longer
		// matches our virtual short hedge.
		hlPositions := []HLPosition{{Coin: "BTC", Size: 0.05}}
		changed := reconcileHedgeLegsForStrategy(sc, s, hlPositions, noFillFeeResolver, logger)
		if !changed {
			t.Fatal("expected direction-flip to clear the stale row")
		}
		if _, ok := s.Positions["BTC"]; ok {
			t.Error("stale hedge row should have been cleared")
		}
		// Same round-2 finding-2 regression as the external-close case:
		// clearing the HEDGE row must not also clear the PRIMARY's
		// HedgeSymbol, or coherence's close-primary branch can never fire.
		if s.Positions["ETH"].HedgeSymbol != "BTC" {
			t.Errorf("primary HedgeSymbol must remain set so coherence can close it, got %q", s.Positions["ETH"].HedgeSymbol)
		}
	})

	t.Run("never adopts a foreign on-chain position with no virtual row", func(t *testing.T) {
		s := &StrategyState{ID: "hl-eth", Positions: map[string]*Position{
			"ETH": {Symbol: "ETH", Quantity: 1, Side: "long"},
		}}
		hlPositions := []HLPosition{{Coin: "BTC", Size: -0.5}}
		changed := reconcileHedgeLegsForStrategy(sc, s, hlPositions, noFillFeeResolver, logger)
		if changed {
			t.Error("expected no-op — a foreign on-chain position must never be adopted")
		}
		if _, ok := s.Positions["BTC"]; ok {
			t.Error("a foreign on-chain position must not create a virtual hedge row")
		}
	})
}

// ─── correlation bucketing (#1159 hedge exposure visibility) ───────────────

func TestComputeAssetDeltasBucketsHedgeUnderOwnAsset(t *testing.T) {
	sc := testHedgeStrategy("hl-eth", "ETH", "BTC")
	strategies := map[string]*StrategyState{
		"hl-eth": {
			ID: "hl-eth",
			Positions: map[string]*Position{
				"ETH": {Symbol: "ETH", Quantity: 1, Side: "long", AvgCost: 3000},
				"BTC": {Symbol: "BTC", Quantity: 0.05, Side: "short", AvgCost: 60000, IsHedge: true, HedgeFor: "ETH"},
			},
		},
	}
	prices := map[string]float64{"ETH": 3100, "BTC": 61000}
	assets, _ := computeAssetDeltas(strategies, []StrategyConfig{sc}, prices)

	ethAE, ok := assets["ETH"]
	if !ok {
		t.Fatal("expected ETH asset exposure entry for the primary leg")
	}
	if diff := ethAE.NetDeltaUSD - 3100; diff > 1e-6 || diff < -1e-6 {
		t.Errorf("ETH net delta = %v, want 3100", ethAE.NetDeltaUSD)
	}

	btcAE, ok := assets["BTC"]
	if !ok {
		t.Fatal("expected BTC asset exposure entry for the hedge leg — it must not be silently skipped")
	}
	wantBTCDelta := -0.05 * 61000 // short hedge => negative delta
	if diff := btcAE.NetDeltaUSD - wantBTCDelta; diff > 1e-6 || diff < -1e-6 {
		t.Errorf("BTC (hedge) net delta = %v, want %v", btcAE.NetDeltaUSD, wantBTCDelta)
	}
	if len(btcAE.Strategies) != 1 || btcAE.Strategies[0].StrategyID != "hl-eth" {
		t.Errorf("expected hedge exposure attributed to hl-eth, got %+v", btcAE.Strategies)
	}
}

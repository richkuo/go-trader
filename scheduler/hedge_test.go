package main

import (
	"strings"
	"testing"
)

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

func TestHedgeOpenQty(t *testing.T) {
	cases := []struct {
		name                                   string
		primaryPx, primaryQty, ratio, hedgeMid float64
		want                                   float64
	}{
		{"basic 1:1", 100, 2, 1.0, 50, 4}, // notional 200, /50 = 4
		{"ratio 0.5", 100, 2, 0.5, 50, 2}, // notional 100, /50 = 2
		{"zero price fails closed", 0, 2, 1, 50, 0},
		{"zero qty fails closed", 100, 0, 1, 50, 0},
		{"zero ratio fails closed", 100, 2, 0, 50, 0},
		{"zero mid fails closed", 100, 2, 1, 0, 0},
		{"negative mid fails closed", 100, 2, 1, -50, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := hedgeOpenQty(c.primaryPx, c.primaryQty, c.ratio, c.hedgeMid)
			if got != c.want {
				t.Errorf("hedgeOpenQty(%v,%v,%v,%v) = %v, want %v", c.primaryPx, c.primaryQty, c.ratio, c.hedgeMid, got, c.want)
			}
		})
	}
}

func TestHedgeTargetQty(t *testing.T) {
	if got := hedgeTargetQty(10, 0.5); got != 5 {
		t.Errorf("hedgeTargetQty(10,0.5) = %v, want 5", got)
	}
	if got := hedgeTargetQty(0, 0.5); got != 0 {
		t.Errorf("hedgeTargetQty(0,0.5) = %v, want 0 (fail closed)", got)
	}
	if got := hedgeTargetQty(10, 0); got != 0 {
		t.Errorf("hedgeTargetQty(10,0) = %v, want 0 (no ratio stamped)", got)
	}
}

func TestHedgeAdjustDelta(t *testing.T) {
	// Within tolerance band: no adjustment.
	if reduce, under := hedgeAdjustDelta(10.02, 10); reduce != 0 || under {
		t.Errorf("within tolerance: got reduce=%v under=%v, want 0,false", reduce, under)
	}
	// Clearly over target: reduce by the delta.
	if reduce, under := hedgeAdjustDelta(15, 10); reduce != 5 || under {
		t.Errorf("over target: got reduce=%v under=%v, want 5,false", reduce, under)
	}
	// Clearly under target: alert-only, never grow.
	if reduce, under := hedgeAdjustDelta(5, 10); reduce != 0 || !under {
		t.Errorf("under target: got reduce=%v under=%v, want 0,true", reduce, under)
	}
	// Zero current hedge with a positive target: under-hedged (nothing to reduce).
	if reduce, under := hedgeAdjustDelta(0, 10); reduce != 0 || !under {
		t.Errorf("zero current, positive target: got reduce=%v under=%v, want 0,true", reduce, under)
	}
	// Zero current, zero target: nothing to do, not "under" (flat matches flat).
	if reduce, under := hedgeAdjustDelta(0, 0); reduce != 0 || under {
		t.Errorf("zero current, zero target: got reduce=%v under=%v, want 0,false", reduce, under)
	}
}

func hedgeTestStrategy(overrides func(*StrategyConfig)) StrategyConfig {
	sc := StrategyConfig{
		ID:       "eth-long",
		Type:     "perps",
		Platform: "hyperliquid",
		Script:   "check_hyperliquid.py",
		Args:     []string{"check_hyperliquid.py", "ETH", "--mode=live"},
		Hedge: &HedgeConfig{
			Enabled:  true,
			Symbol:   "BTC",
			Ratio:    1.0,
			Leverage: 3,
		},
	}
	if overrides != nil {
		overrides(&sc)
	}
	return sc
}

func TestValidateHedgeConfig(t *testing.T) {
	t.Run("valid config passes", func(t *testing.T) {
		sc := hedgeTestStrategy(nil)
		if errs := validateHedgeConfig("strategy[eth-long]", sc, true); len(errs) != 0 {
			t.Errorf("expected no errors, got %v", errs)
		}
	})
	t.Run("disabled hedge is never validated", func(t *testing.T) {
		sc := hedgeTestStrategy(func(sc *StrategyConfig) { sc.Hedge.Enabled = false })
		if errs := validateHedgeConfig("strategy[eth-long]", sc, true); len(errs) != 0 {
			t.Errorf("expected no errors for disabled hedge, got %v", errs)
		}
	})
	t.Run("nil hedge is never validated", func(t *testing.T) {
		sc := hedgeTestStrategy(func(sc *StrategyConfig) { sc.Hedge = nil })
		if errs := validateHedgeConfig("strategy[eth-long]", sc, true); len(errs) != 0 {
			t.Errorf("expected no errors for nil hedge, got %v", errs)
		}
	})
	t.Run("rejects non-perps", func(t *testing.T) {
		sc := hedgeTestStrategy(func(sc *StrategyConfig) { sc.Type = "spot" })
		if errs := validateHedgeConfig("p", sc, true); len(errs) == 0 {
			t.Error("expected rejection for type=spot")
		}
	})
	t.Run("rejects non-hyperliquid platform", func(t *testing.T) {
		sc := hedgeTestStrategy(func(sc *StrategyConfig) { sc.Platform = "okx" })
		if errs := validateHedgeConfig("p", sc, true); len(errs) == 0 {
			t.Error("expected rejection for platform=okx")
		}
	})
	t.Run("rejects non-inverse side", func(t *testing.T) {
		sc := hedgeTestStrategy(func(sc *StrategyConfig) { sc.Hedge.Side = "same" })
		if errs := validateHedgeConfig("p", sc, true); len(errs) == 0 {
			t.Error("expected rejection for side=same")
		}
	})
	t.Run("rejects out-of-range ratio", func(t *testing.T) {
		sc := hedgeTestStrategy(func(sc *StrategyConfig) { sc.Hedge.Ratio = 6 })
		if errs := validateHedgeConfig("p", sc, true); len(errs) == 0 {
			t.Error("expected rejection for ratio=6")
		}
	})
	t.Run("rejects negative ratio", func(t *testing.T) {
		sc := hedgeTestStrategy(func(sc *StrategyConfig) { sc.Hedge.Ratio = -1 })
		if errs := validateHedgeConfig("p", sc, true); len(errs) == 0 {
			t.Error("expected rejection for ratio=-1")
		}
	})
	t.Run("rejects bad margin_mode", func(t *testing.T) {
		sc := hedgeTestStrategy(func(sc *StrategyConfig) { sc.Hedge.MarginMode = "bogus" })
		if errs := validateHedgeConfig("p", sc, true); len(errs) == 0 {
			t.Error("expected rejection for bad margin_mode")
		}
	})
	t.Run("rejects out-of-range leverage", func(t *testing.T) {
		sc := hedgeTestStrategy(func(sc *StrategyConfig) { sc.Hedge.Leverage = 200 })
		if errs := validateHedgeConfig("p", sc, true); len(errs) == 0 {
			t.Error("expected rejection for leverage=200")
		}
	})
	t.Run("rejects empty symbol", func(t *testing.T) {
		sc := hedgeTestStrategy(func(sc *StrategyConfig) { sc.Hedge.Symbol = "" })
		if errs := validateHedgeConfig("p", sc, true); len(errs) == 0 {
			t.Error("expected rejection for empty symbol")
		}
	})
	t.Run("accepts ccxt-style symbol", func(t *testing.T) {
		sc := hedgeTestStrategy(func(sc *StrategyConfig) { sc.Hedge.Symbol = "BTC/USDC:USDC" })
		if errs := validateHedgeConfig("p", sc, true); len(errs) != 0 {
			t.Errorf("expected no errors for ccxt-style symbol, got %v", errs)
		}
	})
}

func TestHyperliquidHedgeCoin(t *testing.T) {
	cases := []struct {
		name   string
		symbol string
		want   string
	}{
		{"bare coin", "BTC", "BTC"},
		{"ccxt form", "BTC/USDC:USDC", "BTC"},
		{"lowercase", "btc", "BTC"},
		{"whitespace", "  BTC  ", "BTC"},
	}
	for _, c := range cases {
		sc := hedgeTestStrategy(func(sc *StrategyConfig) { sc.Hedge.Symbol = c.symbol })
		if got := hyperliquidHedgeCoin(sc); got != c.want {
			t.Errorf("%s: hyperliquidHedgeCoin(%q) = %q, want %q", c.name, c.symbol, got, c.want)
		}
	}
}

func TestHyperliquidHedgeStrategyErrors(t *testing.T) {
	t.Run("no collision is clean", func(t *testing.T) {
		strategies := []StrategyConfig{
			hedgeTestStrategy(nil), // eth-long, primary ETH, hedge BTC
		}
		if errs := hyperliquidHedgeStrategyErrors(strategies); len(errs) != 0 {
			t.Errorf("expected no errors, got %v", errs)
		}
	})
	t.Run("hedge coin equals own primary coin", func(t *testing.T) {
		sc := hedgeTestStrategy(func(sc *StrategyConfig) { sc.Hedge.Symbol = "ETH" })
		if errs := hyperliquidHedgeStrategyErrors([]StrategyConfig{sc}); len(errs) == 0 {
			t.Error("expected rejection for hedge symbol == own primary coin")
		}
	})
	t.Run("hedge coin collides with peer primary coin", func(t *testing.T) {
		ethLong := hedgeTestStrategy(nil) // hedge BTC
		btcShort := StrategyConfig{
			ID: "btc-short", Type: "perps", Platform: "hyperliquid",
			Args: []string{"check_hyperliquid.py", "BTC", "--mode=live"},
		}
		errs := hyperliquidHedgeStrategyErrors([]StrategyConfig{ethLong, btcShort})
		if len(errs) == 0 {
			t.Error("expected rejection for hedge coin colliding with peer's primary coin")
		}
	})
	t.Run("hedge-vs-hedge collision", func(t *testing.T) {
		a := hedgeTestStrategy(func(sc *StrategyConfig) { sc.ID = "eth-a"; sc.Args[1] = "ETH" })
		b := hedgeTestStrategy(func(sc *StrategyConfig) { sc.ID = "sol-b"; sc.Args[1] = "SOL" })
		errs := hyperliquidHedgeStrategyErrors([]StrategyConfig{a, b})
		if len(errs) == 0 {
			t.Error("expected rejection for two hedge-enabled strategies sharing a hedge coin")
		}
	})
	t.Run("collision errors are deterministic across runs", func(t *testing.T) {
		a := hedgeTestStrategy(func(sc *StrategyConfig) { sc.ID = "eth-a"; sc.Args[1] = "ETH" })
		b := hedgeTestStrategy(func(sc *StrategyConfig) { sc.ID = "sol-b"; sc.Args[1] = "SOL" })
		e1 := hyperliquidHedgeStrategyErrors([]StrategyConfig{a, b})
		e2 := hyperliquidHedgeStrategyErrors([]StrategyConfig{a, b})
		if len(e1) != len(e2) {
			t.Fatalf("non-deterministic error count: %d vs %d", len(e1), len(e2))
		}
		for i := range e1 {
			if e1[i] != e2[i] {
				t.Errorf("non-deterministic error ordering at %d: %q vs %q", i, e1[i], e2[i])
			}
		}
	})
}

func TestHedgeConfigEqual(t *testing.T) {
	a := &HedgeConfig{Enabled: true, Symbol: "BTC", Ratio: 1.0, Leverage: 3}
	b := &HedgeConfig{Enabled: true, Symbol: "BTC", Ratio: 1.0, Leverage: 3}
	c := &HedgeConfig{Enabled: true, Symbol: "SOL", Ratio: 1.0, Leverage: 3}
	if !hedgeConfigEqual(a, b) {
		t.Error("expected equal configs to compare equal")
	}
	if hedgeConfigEqual(a, c) {
		t.Error("expected different symbols to compare unequal")
	}
	if hedgeConfigEqual(a, nil) {
		t.Error("expected nil vs non-nil to compare unequal")
	}
	if !hedgeConfigEqual(nil, nil) {
		t.Error("expected nil vs nil to compare equal")
	}
}

func TestApplyHedgeOpenToState_FreshOpen(t *testing.T) {
	s := NewStrategyState(StrategyConfig{ID: "eth-long", Capital: 1000})
	s.Positions["ETH"] = &Position{Symbol: "ETH", Quantity: 2, Side: "long", AvgCost: 3000}

	applyHedgeOpenToState(s, "ETH", "BTC", "short", 0.5, 60000, 5, 12345, 3, 2)

	pos, ok := s.Positions["BTC"]
	if !ok {
		t.Fatal("expected BTC hedge position to be created")
	}
	if !pos.IsHedge {
		t.Error("expected IsHedge=true")
	}
	if pos.Quantity != 0.5 {
		t.Errorf("expected quantity 0.5, got %v", pos.Quantity)
	}
	if pos.AvgCost != 60000 {
		t.Errorf("expected avg_cost 60000, got %v", pos.AvgCost)
	}
	if pos.HedgeRatioQty != 0.25 { // 0.5 hedge / 2 primary
		t.Errorf("expected hedge_ratio_qty 0.25, got %v", pos.HedgeRatioQty)
	}
	if pos.OwnerStrategyID != "eth-long" {
		t.Errorf("expected owner_strategy_id eth-long, got %v", pos.OwnerStrategyID)
	}
	if len(s.TradeHistory) != 1 || !s.TradeHistory[0].IsHedge {
		t.Fatalf("expected one IsHedge trade recorded, got %+v", s.TradeHistory)
	}
}

func TestApplyHedgeOpenToState_Add(t *testing.T) {
	s := NewStrategyState(StrategyConfig{ID: "eth-long", Capital: 1000})
	s.Positions["ETH"] = &Position{Symbol: "ETH", Quantity: 3, Side: "long", AvgCost: 3000}
	s.Positions["BTC"] = &Position{Symbol: "BTC", Quantity: 0.5, Side: "short", AvgCost: 60000, IsHedge: true, HedgeRatioQty: 0.25}

	applyHedgeOpenToState(s, "ETH", "BTC", "short", 0.25, 62000, 2, 0, 3, 3)

	pos := s.Positions["BTC"]
	if pos.Quantity != 0.75 {
		t.Errorf("expected blended quantity 0.75, got %v", pos.Quantity)
	}
	wantAvg := (0.5*60000 + 0.25*62000) / 0.75
	if pos.AvgCost < wantAvg-0.01 || pos.AvgCost > wantAvg+0.01 {
		t.Errorf("expected blended avg_cost ~%v, got %v", wantAvg, pos.AvgCost)
	}
	if pos.HedgeRatioQty != 0.25 { // 0.75 / 3
		t.Errorf("expected re-stamped hedge_ratio_qty 0.25, got %v", pos.HedgeRatioQty)
	}
}

func TestBookHedgeCloseFill_PartialAndFull(t *testing.T) {
	s := NewStrategyState(StrategyConfig{ID: "eth-long", Capital: 1000})
	s.Positions["BTC"] = &Position{Symbol: "BTC", Quantity: 1.0, Side: "short", AvgCost: 60000, IsHedge: true}

	// Partial reduce: short closes profitably when price drops.
	ok := bookHedgeCloseFill(s, "BTC", 0.4, 58000, 2, 0, "hedge_coherence")
	if !ok {
		t.Fatal("expected partial close to succeed")
	}
	pos, exists := s.Positions["BTC"]
	if !exists {
		t.Fatal("expected residual BTC hedge position after partial close")
	}
	if pos.Quantity != 0.6 {
		t.Errorf("expected residual quantity 0.6, got %v", pos.Quantity)
	}
	wantPnL := 0.4*(60000-58000) - 2
	if s.Cash != 1000+wantPnL {
		t.Errorf("expected cash 1000+%v = %v, got %v", wantPnL, 1000+wantPnL, s.Cash)
	}

	// Full close of the residual.
	ok = bookHedgeCloseFill(s, "BTC", 10 /* exceeds residual, clamps */, 59000, 1, 999, "hedge_mirror_close")
	if !ok {
		t.Fatal("expected full close to succeed")
	}
	if _, exists := s.Positions["BTC"]; exists {
		t.Error("expected BTC hedge position to be fully removed")
	}
	closeTrades := 0
	hedgeTrades := 0
	for _, tr := range s.TradeHistory {
		if tr.IsClose {
			closeTrades++
		}
		if tr.IsHedge {
			hedgeTrades++
		}
	}
	if closeTrades != 2 {
		t.Errorf("expected 2 close trades, got %d", closeTrades)
	}
	if hedgeTrades != 2 {
		t.Errorf("expected both trades stamped IsHedge, got %d", hedgeTrades)
	}
}

func TestBookHedgeCloseFill_NoPositionFailsClosed(t *testing.T) {
	s := NewStrategyState(StrategyConfig{ID: "eth-long", Capital: 1000})
	if ok := bookHedgeCloseFill(s, "BTC", 1, 60000, 0, 0, "hedge_coherence"); ok {
		t.Error("expected bookHedgeCloseFill to fail closed with no matching position")
	}
	if len(s.TradeHistory) != 0 {
		t.Error("expected no trade recorded when there's no position to close")
	}
}

func TestHedgeCoinsForKillSwitch(t *testing.T) {
	hedged := hedgeTestStrategy(nil) // ETH primary, BTC hedge
	unhedged := StrategyConfig{ID: "sol-x", Type: "perps", Platform: "hyperliquid", Args: []string{"check_hyperliquid.py", "SOL", "--mode=live"}}
	coins := hedgeCoinsForKillSwitch([]StrategyConfig{hedged, unhedged})
	if !coins["BTC"] {
		t.Error("expected BTC hedge coin to be included")
	}
	if len(coins) != 1 {
		t.Errorf("expected exactly 1 hedge coin, got %v", coins)
	}
}

func TestApplyHedgeKillSwitchCloseFill(t *testing.T) {
	sc := hedgeTestStrategy(nil)
	s := NewStrategyState(StrategyConfig{ID: sc.ID, Capital: 1000})
	s.Positions["BTC"] = &Position{Symbol: "BTC", Quantity: 1.0, Side: "short", AvgCost: 60000, IsHedge: true}

	fills := map[string]HyperliquidCloseFill{
		"BTC": {AvgPx: 59000, TotalSz: 1.0, Fee: 3, OID: 777},
	}
	if ok := applyHedgeKillSwitchCloseFill(s, sc, fills); !ok {
		t.Fatal("expected kill-switch hedge fill to book successfully")
	}
	if _, exists := s.Positions["BTC"]; exists {
		t.Error("expected hedge position fully closed")
	}

	// Duplicate OID must not double-book.
	s.Positions["BTC"] = &Position{Symbol: "BTC", Quantity: 1.0, Side: "short", AvgCost: 60000, IsHedge: true}
	if ok := applyHedgeKillSwitchCloseFill(s, sc, fills); ok {
		t.Error("expected duplicate OID to be rejected (already booked)")
	}
}

func TestApplyHedgeKillSwitchCloseFill_NotHedgeEnabled(t *testing.T) {
	sc := StrategyConfig{ID: "plain", Type: "perps", Platform: "hyperliquid", Args: []string{"check_hyperliquid.py", "ETH", "--mode=live"}}
	s := NewStrategyState(StrategyConfig{ID: sc.ID, Capital: 1000})
	fills := map[string]HyperliquidCloseFill{"BTC": {AvgPx: 59000, TotalSz: 1.0, Fee: 3}}
	if ok := applyHedgeKillSwitchCloseFill(s, sc, fills); ok {
		t.Error("expected no-op for a strategy without hedge enabled")
	}
}

func TestValidateHedgeConfig_RejectsPaperMode(t *testing.T) {
	sc := hedgeTestStrategy(func(sc *StrategyConfig) {
		sc.Args = []string{"check_hyperliquid.py", "ETH", "--mode=paper"}
	})
	errs := validateHedgeConfig("p", sc, true)
	if len(errs) == 0 {
		t.Fatal("expected rejection for hedge.enabled=true on paper args")
	}
	found := false
	for _, e := range errs {
		if strings.Contains(e, "live-only") {
			found = true
		}
	}
	if !found {
		t.Errorf("expected a live-only rejection message, got %v", errs)
	}
}

func TestApplyHedgeOpenToState_OppositeSideNetsInsteadOfBlending(t *testing.T) {
	t.Run("excess after full net opens fresh on new side", func(t *testing.T) {
		s := NewStrategyState(StrategyConfig{ID: "eth-hedged", Capital: 1000})
		s.Positions["ETH"] = &Position{Symbol: "ETH", Quantity: 1, Side: "long", AvgCost: 3000}
		// Stale short residual (e.g. survived a partial flip-close).
		s.Positions["BTC"] = &Position{Symbol: "BTC", Quantity: 0.2, Side: "short", AvgCost: 60000, IsHedge: true}

		// A LONG fill of 0.5 arrives — nets 0.2 against the short residual
		// (closing it), then opens 0.3 fresh long with the remainder.
		applyHedgeOpenToState(s, "ETH", "BTC", "long", 0.5, 61000, 3, 0, 1, 1)

		pos, ok := s.Positions["BTC"]
		if !ok {
			t.Fatal("expected a fresh BTC hedge position after netting")
		}
		if pos.Side != "long" {
			t.Errorf("expected Side=long after netting past the residual, got %q", pos.Side)
		}
		if pos.Quantity != 0.3 {
			t.Errorf("expected Quantity=0.3 (0.5 fill - 0.2 netted), got %v", pos.Quantity)
		}
		if pos.AvgCost != 61000 {
			t.Errorf("expected fresh AvgCost=61000 (not blended with the closed short), got %v", pos.AvgCost)
		}
		// The netting close must have booked a real close trade (short residual
		// bought back at 61000) in addition to the fresh-open trade.
		closeTrades, openTrades := 0, 0
		for _, tr := range s.TradeHistory {
			if tr.IsClose {
				closeTrades++
			} else {
				openTrades++
			}
		}
		if closeTrades != 1 || openTrades != 1 {
			t.Errorf("expected 1 close + 1 open trade, got close=%d open=%d", closeTrades, openTrades)
		}
	})

	t.Run("fill smaller than residual only reduces, never flips or blends", func(t *testing.T) {
		s := NewStrategyState(StrategyConfig{ID: "eth-hedged", Capital: 1000})
		s.Positions["BTC"] = &Position{Symbol: "BTC", Quantity: 1.0, Side: "short", AvgCost: 60000, IsHedge: true}

		applyHedgeOpenToState(s, "ETH", "BTC", "long", 0.3, 61000, 1, 0, 1, 1)

		pos, ok := s.Positions["BTC"]
		if !ok {
			t.Fatal("expected residual BTC hedge position to remain")
		}
		if pos.Side != "short" {
			t.Errorf("expected Side to remain short (fill fully absorbed as a reduction), got %q", pos.Side)
		}
		if pos.Quantity != 0.7 {
			t.Errorf("expected Quantity=0.7 (1.0 - 0.3 netted), got %v", pos.Quantity)
		}
		if pos.AvgCost != 60000 {
			t.Errorf("expected AvgCost to remain 60000 (never blended with the opposite-side fill), got %v", pos.AvgCost)
		}
	})

	t.Run("same-side add still blends normally (no regression)", func(t *testing.T) {
		s := NewStrategyState(StrategyConfig{ID: "eth-hedged", Capital: 1000})
		s.Positions["BTC"] = &Position{Symbol: "BTC", Quantity: 0.5, Side: "short", AvgCost: 60000, IsHedge: true}

		applyHedgeOpenToState(s, "ETH", "BTC", "short", 0.5, 62000, 2, 0, 1, 2)

		pos := s.Positions["BTC"]
		if pos.Quantity != 1.0 {
			t.Errorf("expected blended quantity 1.0, got %v", pos.Quantity)
		}
		wantAvg := (0.5*60000 + 0.5*62000) / 1.0
		if pos.AvgCost != wantAvg {
			t.Errorf("expected blended avg_cost %v, got %v", wantAvg, pos.AvgCost)
		}
	})
}

func TestApplyHedgeOpenToState_DebitsCashForFee(t *testing.T) {
	t.Run("fresh open debits the fill fee", func(t *testing.T) {
		s := NewStrategyState(StrategyConfig{ID: "eth-hedged", Capital: 1000})
		s.Positions["ETH"] = &Position{Symbol: "ETH", Quantity: 1, Side: "long", AvgCost: 3000}

		applyHedgeOpenToState(s, "ETH", "BTC", "short", 0.5, 60000, 5, 0, 1, 1)

		if s.Cash != 995 {
			t.Errorf("expected cash 1000-5=995 after the hedge open fee, got %v", s.Cash)
		}
	})

	t.Run("scale-in add debits its own fee", func(t *testing.T) {
		s := NewStrategyState(StrategyConfig{ID: "eth-hedged", Capital: 1000})
		s.Positions["ETH"] = &Position{Symbol: "ETH", Quantity: 2, Side: "long", AvgCost: 3000}
		s.Positions["BTC"] = &Position{Symbol: "BTC", Quantity: 0.5, Side: "short", AvgCost: 60000, IsHedge: true}

		applyHedgeOpenToState(s, "ETH", "BTC", "short", 0.25, 62000, 2, 0, 1, 3)

		if s.Cash != 998 {
			t.Errorf("expected cash 1000-2=998 after the add's own fee, got %v", s.Cash)
		}
	})

	t.Run("flip-net excess open debits only the excess-open fee, not the netted portion", func(t *testing.T) {
		s := NewStrategyState(StrategyConfig{ID: "eth-hedged", Capital: 1000})
		s.Positions["ETH"] = &Position{Symbol: "ETH", Quantity: 1, Side: "long", AvgCost: 3000}
		s.Positions["BTC"] = &Position{Symbol: "BTC", Quantity: 0.2, Side: "short", AvgCost: 60000, IsHedge: true}

		// fillFee=3 on a 0.5 fill; 0.2 nets against the residual (net fee
		// 3*0.2/0.5=1.2, applied via bookHedgeCloseFill's own PnL-fee
		// deduction), the remaining 0.3 opens fresh and should debit its
		// pro-rated share (3-1.2=1.8) from cash directly.
		applyHedgeOpenToState(s, "ETH", "BTC", "long", 0.5, 61000, 3, 0, 1, 1)

		// Close leg: pnl = 0.2*(60000-61000) - 1.2 = -201.2 -> cash += pnl.
		// Open leg: cash -= 1.8.
		wantCash := 1000 + (0.2*(60000-61000) - 1.2) - 1.8
		if diff := s.Cash - wantCash; diff > 1e-9 || diff < -1e-9 {
			t.Errorf("expected cash %v (net-close PnL/fee + excess-open fee), got %v", wantCash, s.Cash)
		}
	})
}

func TestBookHedgeCloseFill_DoesNotAffectConsecutiveLosses(t *testing.T) {
	s := NewStrategyState(StrategyConfig{ID: "eth-hedged", Capital: 1000})
	s.RiskState.ConsecutiveLosses = 3
	s.Positions["BTC"] = &Position{Symbol: "BTC", Quantity: 1.0, Side: "short", AvgCost: 60000, IsHedge: true}

	// Short closes at a loss (price rose): pnl = 1.0*(60000-61000) - 1 = -1001.
	ok := bookHedgeCloseFill(s, "BTC", 1.0, 61000, 1, 0, "hedge_coherence")
	if !ok {
		t.Fatal("expected close to succeed")
	}
	if s.RiskState.ConsecutiveLosses != 3 {
		t.Errorf("expected ConsecutiveLosses to stay untouched by a hedge close (was 3), got %d", s.RiskState.ConsecutiveLosses)
	}
	wantPnL := 1.0*(60000-61000) - 1
	if s.RiskState.DailyPnL != wantPnL {
		t.Errorf("expected DailyPnL to still reflect the real hedge PnL %v, got %v", wantPnL, s.RiskState.DailyPnL)
	}
	if s.Cash != 1000+wantPnL {
		t.Errorf("expected cash to reflect the hedge close PnL, got %v want %v", s.Cash, 1000+wantPnL)
	}

	// A losing primary alongside a winning hedge must still accumulate the
	// loss-streak counter via the PRIMARY's own RecordTradeResult call
	// elsewhere — this test only asserts the hedge leg itself never resets
	// or increments it.
	s2 := NewStrategyState(StrategyConfig{ID: "eth-hedged", Capital: 1000})
	s2.RiskState.ConsecutiveLosses = 4
	s2.Positions["BTC"] = &Position{Symbol: "BTC", Quantity: 1.0, Side: "short", AvgCost: 60000, IsHedge: true}
	// Short closes at a WIN this time (price dropped) — under the old
	// RecordTradeResult wiring this would have reset ConsecutiveLosses to 0,
	// masking a real losing streak on the primary.
	bookHedgeCloseFill(s2, "BTC", 1.0, 59000, 1, 0, "hedge_coherence")
	if s2.RiskState.ConsecutiveLosses != 4 {
		t.Errorf("expected a winning hedge close to NOT reset ConsecutiveLosses (was 4), got %d", s2.RiskState.ConsecutiveLosses)
	}
}

func TestHedgeLegSideMismatched(t *testing.T) {
	cases := []struct {
		name         string
		hedgeSide    string
		expectedSide string
		want         bool
	}{
		{"correctly inverse", "short", "short", false}, // expectedSide is already hedgeSideForPrimary's output
		{"same direction as primary (bug state)", "long", "short", true},
		{"no resolvable primary side never mismatches", "long", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := hedgeLegSideMismatched(tc.hedgeSide, tc.expectedSide); got != tc.want {
				t.Errorf("hedgeLegSideMismatched(%q, %q) = %v, want %v", tc.hedgeSide, tc.expectedSide, got, tc.want)
			}
		})
	}
}

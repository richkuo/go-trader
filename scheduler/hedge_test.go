package main

import (
	"reflect"
	"strings"
	"testing"
	"time"
)

// --- helpers -----------------------------------------------------------------

func hedgeTestStrategy(id, coin, hedgeSym string) StrategyConfig {
	return StrategyConfig{
		ID: id, Type: "perps", Platform: "hyperliquid",
		Script: "check.py", Args: []string{"strat", coin, "1h"},
		Capital: 1000, MaxDrawdownPct: 10, Leverage: 2, Direction: "long",
		Hedge: &HedgeConfig{Enabled: true, Symbol: hedgeSym},
	}
}

// --- config surface -----------------------------------------------------------

func TestHedgeCoinNormalization(t *testing.T) {
	cases := []struct{ in, want string }{
		{"BTC", "BTC"},
		{" btc ", "BTC"},
		{"BTC/USDC:USDC", "BTC"},
		{"", ""},
	}
	for _, tc := range cases {
		sc := StrategyConfig{Hedge: &HedgeConfig{Enabled: true, Symbol: tc.in}}
		if got := hedgeCoin(sc); got != tc.want {
			t.Errorf("hedgeCoin(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
	if hedgeCoin(StrategyConfig{}) != "" {
		t.Error("hedgeCoin with nil block must be empty")
	}
}

func TestHedgeAccessorDefaults(t *testing.T) {
	sc := StrategyConfig{Hedge: &HedgeConfig{Enabled: true, Symbol: "BTC"}}
	if hedgeRatio(sc) != 1.0 {
		t.Errorf("default ratio = %v, want 1.0", hedgeRatio(sc))
	}
	if hedgeExchangeLeverage(sc) != 1 {
		t.Errorf("default leverage = %v, want 1", hedgeExchangeLeverage(sc))
	}
	sc.Hedge.Ratio = 0.5
	sc.Hedge.Leverage = 3
	if hedgeRatio(sc) != 0.5 || hedgeExchangeLeverage(sc) != 3 {
		t.Error("explicit ratio/leverage not honored")
	}
}

func TestValidateHedgeConfigsCollisionMatrix(t *testing.T) {
	base := func() []StrategyConfig {
		return []StrategyConfig{
			hedgeTestStrategy("hl-eth", "ETH", "BTC"),
			{ID: "hl-sol", Type: "perps", Platform: "hyperliquid", Args: []string{"s", "SOL", "1h"}},
			{ID: "hl-manual-doge", Type: "manual", Platform: "hyperliquid", Symbol: "DOGE"},
		}
	}

	if errs := validateHedgeConfigs(base()); len(errs) != 0 {
		t.Fatalf("clean config rejected: %v", errs)
	}

	cases := []struct {
		name    string
		mutate  func([]StrategyConfig) []StrategyConfig
		wantSub string
	}{
		{"own coin", func(s []StrategyConfig) []StrategyConfig { s[0].Hedge.Symbol = "ETH"; return s }, "own coin"},
		{"peer perps coin", func(s []StrategyConfig) []StrategyConfig { s[0].Hedge.Symbol = "SOL"; return s }, "collides with configured strategy coin"},
		{"manual peer coin", func(s []StrategyConfig) []StrategyConfig { s[0].Hedge.Symbol = "DOGE"; return s }, "collides with configured strategy coin"},
		{"ccxt form collides too", func(s []StrategyConfig) []StrategyConfig { s[0].Hedge.Symbol = "SOL/USDC:USDC"; return s }, "collides"},
		{"hedge-vs-hedge", func(s []StrategyConfig) []StrategyConfig {
			s = append(s, hedgeTestStrategy("hl-avax", "AVAX", "BTC"))
			return s
		}, "shared by multiple hedge-enabled strategies"},
		{"empty symbol", func(s []StrategyConfig) []StrategyConfig { s[0].Hedge.Symbol = ""; return s }, "hedge.symbol is required"},
		{"bad side", func(s []StrategyConfig) []StrategyConfig { s[0].Hedge.Side = "same"; return s }, "hedge.side"},
		{"bad ratio", func(s []StrategyConfig) []StrategyConfig { s[0].Hedge.Ratio = 11; return s }, "hedge.ratio"},
		{"bad margin mode", func(s []StrategyConfig) []StrategyConfig { s[0].Hedge.MarginMode = "portfolio"; return s }, "hedge.margin_mode"},
		{"bad platform", func(s []StrategyConfig) []StrategyConfig { s[0].Hedge.Platform = "okx"; return s }, "hedge.platform"},
		{"bad type", func(s []StrategyConfig) []StrategyConfig { s[0].Hedge.Type = "spot"; return s }, "hedge.type"},
		{"direction both", func(s []StrategyConfig) []StrategyConfig { s[0].Direction = "both"; return s }, "direction \"both\""},
		{"non-perps host", func(s []StrategyConfig) []StrategyConfig { s[0].Type = "manual"; return s }, "only supported for hyperliquid perps"},
		{"non-HL host", func(s []StrategyConfig) []StrategyConfig { s[0].Platform = "okx"; return s }, "only supported for hyperliquid perps"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			errs := validateHedgeConfigs(tc.mutate(base()))
			found := false
			for _, e := range errs {
				if strings.Contains(e, tc.wantSub) {
					found = true
				}
			}
			if !found {
				t.Fatalf("want error containing %q, got %v", tc.wantSub, errs)
			}
		})
	}

	// Disabled block is inert regardless of shape.
	s := base()
	s[0].Hedge.Enabled = false
	s[0].Hedge.Symbol = "ETH"
	if errs := validateHedgeConfigs(s); len(errs) != 0 {
		t.Fatalf("disabled hedge block must be inert, got %v", errs)
	}
}

func TestHedgeUnknownKeyGuard(t *testing.T) {
	raw := []byte(`{"strategies":[{"id":"hl-eth","hedge":{"enabled":true,"symbol":"BTC","ration":2.0}}]}`)
	errs := validateStrategyJSONKeys(raw)
	found := false
	for _, e := range errs {
		if strings.Contains(e, `hedge: unknown field "ration"`) {
			found = true
		}
	}
	if !found {
		t.Fatalf("typo'd hedge key must reject, got %v", errs)
	}
	clean := []byte(`{"strategies":[{"id":"hl-eth","hedge":{"enabled":true,"symbol":"BTC","ratio":2.0,"margin_mode":"cross","leverage":3,"side":"inverse","platform":"hyperliquid","type":"perps"}}]}`)
	if errs := validateStrategyJSONKeys(clean); len(errs) != 0 {
		t.Fatalf("all declared hedge keys must pass, got %v", errs)
	}
}

// --- decision core -------------------------------------------------------------

func TestHedgeTargetDecisionInverseOpenSizing(t *testing.T) {
	sc := hedgeTestStrategy("hl-eth", "ETH", "BTC")
	sc.Hedge.Ratio = 0.5
	// primary long 2 ETH @ $2000 mark, hedge BTC @ $50000: notional 4000×0.5 → 0.04 BTC short
	a := hedgeTargetDecision(sc, hedgeSnapshot{PrimaryQty: 2, PrimarySide: "long"}, 2000, 50000)
	if a.Kind != hedgeActionOpen || a.PosSide != "short" || !approxEq(a.Qty, 0.04) {
		t.Fatalf("open decision = %+v, want open short 0.04", a)
	}
	// primary short → hedge long
	b := hedgeTargetDecision(sc, hedgeSnapshot{PrimaryQty: 2, PrimarySide: "short"}, 2000, 50000)
	if b.Kind != hedgeActionOpen || b.PosSide != "long" {
		t.Fatalf("short primary decision = %+v, want open long", b)
	}
}

func TestHedgeTargetDecisionUnusablePriceFailsClosed(t *testing.T) {
	sc := hedgeTestStrategy("hl-eth", "ETH", "BTC")
	a := hedgeTargetDecision(sc, hedgeSnapshot{PrimaryQty: 2, PrimarySide: "long"}, 2000, 0)
	if a.Kind != hedgeActionNone || a.Reason == "" {
		t.Fatalf("missing hedge mark must fail closed with a reason, got %+v", a)
	}
}

func TestHedgeTargetDecisionPrimaryFlatClosesHedge(t *testing.T) {
	sc := hedgeTestStrategy("hl-eth", "ETH", "BTC")
	a := hedgeTargetDecision(sc, hedgeSnapshot{HedgeExists: true, HedgeQty: 0.04, HedgeSide: "short", HedgeBasis: 2}, 2000, 50000)
	if a.Kind != hedgeActionCloseFull || !approxEq(a.Qty, 0.04) {
		t.Fatalf("primary flat must close hedge fully, got %+v", a)
	}
}

func TestHedgeTargetDecisionAddAndReduce(t *testing.T) {
	sc := hedgeTestStrategy("hl-eth", "ETH", "BTC")
	held := hedgeSnapshot{
		PrimaryQty: 3, PrimarySide: "long",
		HedgeExists: true, HedgeQty: 0.08, HedgeSide: "short", HedgeBasis: 2,
	}
	// primary grew 2 → 3: add 1×2000/50000 = 0.04
	a := hedgeTargetDecision(sc, held, 2000, 50000)
	if a.Kind != hedgeActionAdd || !approxEq(a.Qty, 0.04) || !approxEq(a.PrimaryDelta, 1) {
		t.Fatalf("add decision = %+v, want add 0.04 (delta 1)", a)
	}
	// primary shrank 2 → 1: reduce hedge by 50%
	held.PrimaryQty = 1
	b := hedgeTargetDecision(sc, held, 2000, 50000)
	if b.Kind != hedgeActionReduce || !approxEq(b.Qty, 0.04) {
		t.Fatalf("reduce decision = %+v, want reduce 0.04 (half)", b)
	}
	// in-sync: nothing
	held.PrimaryQty = 2
	if c := hedgeTargetDecision(sc, held, 2000, 50000); c.Kind != hedgeActionNone || c.Reason != "" {
		t.Fatalf("in-sync must be a quiet no-op, got %+v", c)
	}
	// primary → ~0 via basis: full close
	held.PrimaryQty = 0
	if d := hedgeTargetDecision(sc, held, 2000, 50000); d.Kind != hedgeActionCloseFull {
		t.Fatalf("qty to zero must close fully, got %+v", d)
	}
}

func TestHedgeTargetDecisionWrongSideCloses(t *testing.T) {
	sc := hedgeTestStrategy("hl-eth", "ETH", "BTC")
	a := hedgeTargetDecision(sc, hedgeSnapshot{
		PrimaryQty: 2, PrimarySide: "long",
		HedgeExists: true, HedgeQty: 0.04, HedgeSide: "long", HedgeBasis: 2,
	}, 2000, 50000)
	if a.Kind != hedgeActionCloseFull {
		t.Fatalf("wrong-side hedge must flatten first, got %+v", a)
	}
}

func TestHedgeTargetDecisionDustReduceDefers(t *testing.T) {
	sc := hedgeTestStrategy("hl-eth", "ETH", "BTC")
	// tiny reduce: 1% of 0.01 BTC @ 50000 = $5 < $10 min
	a := hedgeTargetDecision(sc, hedgeSnapshot{
		PrimaryQty: 1.98, PrimarySide: "long",
		HedgeExists: true, HedgeQty: 0.01, HedgeSide: "short", HedgeBasis: 2,
	}, 2000, 50000)
	if a.Kind != hedgeActionNone || !strings.Contains(a.Reason, "deferred") {
		t.Fatalf("dust reduce must defer without advancing basis, got %+v", a)
	}
}

func TestHedgeAdvancedBasisPartialFill(t *testing.T) {
	// open: requested 0.04, filled 0.02 → basis covers half the primary qty
	if got := hedgeAdvancedBasis(0, 2, 0.04, 0.02, hedgeActionOpen); !approxEq(got, 1) {
		t.Errorf("partial open basis = %v, want 1", got)
	}
	// reduce: basis 2 → toward 1, half filled → 1.5
	if got := hedgeAdvancedBasis(2, 1, 0.04, 0.02, hedgeActionReduce); !approxEq(got, 1.5) {
		t.Errorf("partial reduce basis = %v, want 1.5", got)
	}
	// full fill add
	if got := hedgeAdvancedBasis(2, 1, 0.04, 0.04, hedgeActionAdd); !approxEq(got, 3) {
		t.Errorf("full add basis = %v, want 3", got)
	}
}

func TestHedgeOrderSkipReasonForeignPosition(t *testing.T) {
	action := hedgeAction{Kind: hedgeActionOpen, Qty: 0.04, PosSide: "short"}
	snap := hedgeSnapshot{PrimaryQty: 2, PrimarySide: "long"}
	if r := hedgeOrderSkipReason(action, snap, 0.5, true); !strings.Contains(r, "foreign") {
		t.Fatalf("foreign on-chain position must fail closed, got %q", r)
	}
	if r := hedgeOrderSkipReason(action, snap, 0, true); r != "" {
		t.Fatalf("flat hedge coin must allow the open, got %q", r)
	}
	// unknown coin coverage (not in snapshot) also allows — reconcile owns drift
	if r := hedgeOrderSkipReason(action, snap, 0, false); r != "" {
		t.Fatalf("unknown on-chain coverage must allow the open, got %q", r)
	}
}

// --- booking -------------------------------------------------------------------

func hedgeTestState() *StrategyState {
	return &StrategyState{
		ID: "hl-eth", Type: "perps", Platform: "hyperliquid", Cash: 1000,
		Positions:       map[string]*Position{},
		OptionPositions: map[string]*OptionPosition{},
	}
}

func TestApplyHedgeOpenFillCreatesOwnedLeg(t *testing.T) {
	sc := hedgeTestStrategy("hl-eth", "ETH", "BTC")
	s := hedgeTestState()
	action := hedgeAction{Kind: hedgeActionOpen, Qty: 0.04, PosSide: "short", PrimaryDelta: 2}
	if n := applyHedgeOpenFill(s, sc, "ETH", action, 0.04, 50000, 0.7, true, "123", nil); n != 1 {
		t.Fatalf("applyHedgeOpenFill booked %d trades, want 1", n)
	}
	pos := s.Positions["BTC"]
	if pos == nil || pos.HedgeFor != "ETH" || pos.Side != "short" || !approxEq(pos.HedgePrimaryQtyBasis, 2) {
		t.Fatalf("hedge position = %+v, want short BTC hedging ETH with basis 2", pos)
	}
	if pos.Multiplier != 1 || pos.OwnerStrategyID != "hl-eth" {
		t.Fatalf("hedge leg must carry perps multiplier + owner, got %+v", pos)
	}
	if !approxEq(s.Cash, 1000-0.7) {
		t.Fatalf("only the fee may leave cash, got %v", s.Cash)
	}
	tr := s.TradeHistory[len(s.TradeHistory)-1]
	if tr.TradeType != "hedge" || tr.IsClose || tr.Side != "sell" {
		t.Fatalf("hedge open trade = %+v, want open-side sell trade_type=hedge", tr)
	}
	if !strings.Contains(tr.Details, "hedge(ETH)") {
		t.Fatalf("hedge trade details must name the primary, got %q", tr.Details)
	}
}

func TestApplyHedgeOpenFillPartialFillAdvancesBasisProportionally(t *testing.T) {
	sc := hedgeTestStrategy("hl-eth", "ETH", "BTC")
	s := hedgeTestState()
	action := hedgeAction{Kind: hedgeActionOpen, Qty: 0.04, PosSide: "short", PrimaryDelta: 2}
	applyHedgeOpenFill(s, sc, "ETH", action, 0.02, 50000, 0.3, true, "123", nil)
	pos := s.Positions["BTC"]
	if !approxEq(pos.HedgePrimaryQtyBasis, 1) {
		t.Fatalf("half-filled open must set basis to half the primary delta, got %v", pos.HedgePrimaryQtyBasis)
	}
}

func TestApplyHedgeCloseFillFullAndReduce(t *testing.T) {
	sc := hedgeTestStrategy("hl-eth", "ETH", "BTC")
	s := hedgeTestState()
	s.Positions["BTC"] = &Position{
		Symbol: "BTC", Quantity: 0.04, InitialQuantity: 0.04, AvgCost: 50000, Side: "short",
		Multiplier: 1, OwnerStrategyID: "hl-eth", HedgeFor: "ETH", HedgePrimaryQtyBasis: 2,
		TradePositionID: "hl-eth:BTC:x",
	}
	// reduce half at a profit for the short (px down)
	action := hedgeAction{Kind: hedgeActionReduce, Qty: 0.02, PrimaryDelta: 1}
	if n := applyHedgeCloseFill(s, sc, action, 0.02, 49000, 0.3, true, "77", nil); n != 1 {
		t.Fatalf("reduce booked %d trades, want 1", n)
	}
	pos := s.Positions["BTC"]
	if pos == nil || !approxEq(pos.Quantity, 0.02) || !approxEq(pos.HedgePrimaryQtyBasis, 1) {
		t.Fatalf("after reduce: %+v, want qty 0.02 basis 1", pos)
	}
	tr := s.TradeHistory[len(s.TradeHistory)-1]
	if tr.TradeType != "hedge" || !tr.IsClose {
		t.Fatalf("hedge reduce trade = %+v, want close-side trade_type=hedge", tr)
	}
	// full close of the remainder
	action = hedgeAction{Kind: hedgeActionCloseFull, Qty: 0.02, PrimaryDelta: 1}
	if n := applyHedgeCloseFill(s, sc, action, 0.02, 49000, 0.3, true, "78", nil); n != 1 {
		t.Fatal("full close must book")
	}
	if _, held := s.Positions["BTC"]; held {
		t.Fatal("hedge leg must be deleted after full close")
	}
}

func TestHedgeCloseNeverTouchesLossStreak(t *testing.T) {
	sc := hedgeTestStrategy("hl-eth", "ETH", "BTC")
	s := hedgeTestState()
	s.RiskState.ConsecutiveLosses = 3
	s.Positions["BTC"] = &Position{
		Symbol: "BTC", Quantity: 0.04, AvgCost: 50000, Side: "short", Multiplier: 1,
		HedgeFor: "ETH", HedgePrimaryQtyBasis: 2, TradePositionID: "hl-eth:BTC:x",
	}
	// hedge WIN (short closed lower) must NOT reset the primary loss streak
	applyHedgeCloseFill(s, sc, hedgeAction{Kind: hedgeActionCloseFull, Qty: 0.04}, 0.04, 49000, 0, false, "", nil)
	if s.RiskState.ConsecutiveLosses != 3 {
		t.Fatalf("hedge win reset the loss streak: %d, want 3", s.RiskState.ConsecutiveLosses)
	}
	// hedge LOSS must not increment it either
	s.Positions["BTC"] = &Position{
		Symbol: "BTC", Quantity: 0.04, AvgCost: 50000, Side: "short", Multiplier: 1,
		HedgeFor: "ETH", HedgePrimaryQtyBasis: 2, TradePositionID: "hl-eth:BTC:y",
	}
	applyHedgeCloseFill(s, sc, hedgeAction{Kind: hedgeActionCloseFull, Qty: 0.04}, 0.04, 51000, 0, false, "", nil)
	if s.RiskState.ConsecutiveLosses != 3 {
		t.Fatalf("hedge loss changed the loss streak: %d, want 3", s.RiskState.ConsecutiveLosses)
	}
	// DailyPnL still moves (accounting integrity)
	if s.RiskState.DailyPnL == 0 {
		t.Fatal("hedge closes must book DailyPnL")
	}
}

func TestClassifyPositionTradeTypeHedge(t *testing.T) {
	s := &StrategyState{Platform: "hyperliquid", Type: "perps"}
	pos := &Position{Multiplier: 1, HedgeFor: "ETH"}
	if got := classifyPositionTradeType(s, pos); got != "hedge" {
		t.Fatalf("classifyPositionTradeType = %q, want hedge", got)
	}
	pos.HedgeFor = ""
	if got := classifyPositionTradeType(s, pos); got != "perps" {
		t.Fatalf("classifyPositionTradeType = %q, want perps", got)
	}
}

// --- persistence + stats --------------------------------------------------------

func TestHedgePositionPersistsRoundTrip(t *testing.T) {
	db := openTestDB(t)
	now := time.Now().UTC().Truncate(time.Nanosecond)
	state := &AppState{
		Strategies: map[string]*StrategyState{
			"hl-eth": {
				ID: "hl-eth", Type: "perps", Platform: "hyperliquid", Cash: 1000,
				Positions: map[string]*Position{
					"BTC": {
						Symbol: "BTC", Quantity: 0.04, InitialQuantity: 0.04, AvgCost: 50000,
						Side: "short", Multiplier: 1, OwnerStrategyID: "hl-eth", OpenedAt: now,
						HedgeFor: "ETH", HedgePrimaryQtyBasis: 2,
					},
				},
				OptionPositions: map[string]*OptionPosition{},
				TradeHistory:    []Trade{},
			},
		},
	}
	if err := db.SaveState(state); err != nil {
		t.Fatalf("SaveState: %v", err)
	}
	loaded, err := db.LoadState()
	if err != nil {
		t.Fatalf("LoadState: %v", err)
	}
	pos := loaded.Strategies["hl-eth"].Positions["BTC"]
	if pos.HedgeFor != "ETH" || !approxEq(pos.HedgePrimaryQtyBasis, 2) {
		t.Fatalf("hedge ownership metadata lost on round-trip: %+v", pos)
	}
}

func TestHedgeTradesExcludedFromLifetimeStats(t *testing.T) {
	db := openTestDB(t)
	now := time.Now().UTC().Truncate(time.Nanosecond)
	pid := "hl-eth:ETH:1"
	hpid := "hl-eth:BTC:1"
	state := &AppState{
		Strategies: map[string]*StrategyState{
			"hl-eth": {
				ID: "hl-eth", Type: "perps", Platform: "hyperliquid", Cash: 1000,
				Positions: map[string]*Position{}, OptionPositions: map[string]*OptionPosition{},
				TradeHistory: []Trade{
					{Timestamp: now.Add(-4 * time.Hour), StrategyID: "hl-eth", Symbol: "ETH", PositionID: pid, Side: "buy", Quantity: 2, Price: 2000, Value: 4000, TradeType: "perps"},
					{Timestamp: now.Add(-4 * time.Hour), StrategyID: "hl-eth", Symbol: "BTC", PositionID: hpid, Side: "sell", Quantity: 0.04, Price: 50000, Value: 2000, TradeType: "hedge"},
					{Timestamp: now.Add(-1 * time.Hour), StrategyID: "hl-eth", Symbol: "ETH", PositionID: pid, Side: "sell", Quantity: 2, Price: 2300, Value: 4600, TradeType: "perps", IsClose: true, RealizedPnL: 600},
					// hedge round-trip LOST money — must not count as a loss in W/L
					{Timestamp: now.Add(-1 * time.Hour), StrategyID: "hl-eth", Symbol: "BTC", PositionID: hpid, Side: "buy", Quantity: 0.04, Price: 51000, Value: 2040, TradeType: "hedge", IsClose: true, RealizedPnL: -40},
				},
			},
		},
	}
	if err := db.SaveState(state); err != nil {
		t.Fatalf("SaveState: %v", err)
	}
	stats, err := db.LifetimeTradeStatsAll()
	if err != nil {
		t.Fatalf("LifetimeTradeStatsAll: %v", err)
	}
	got := stats["hl-eth"]
	if got.PositionsOpened != 1 {
		t.Errorf("PositionsOpened = %d, want 1 (hedge open excluded)", got.PositionsOpened)
	}
	if got.Wins != 1 || got.Losses != 0 {
		t.Errorf("W/L = %d/%d, want 1/0 (hedge round-trip excluded)", got.Wins, got.Losses)
	}
	one, err := db.LifetimeTradeStatsForStrategy("hl-eth")
	if err != nil {
		t.Fatalf("LifetimeTradeStatsForStrategy: %v", err)
	}
	if one.PositionsOpened != 1 || one.Wins != 1 || one.Losses != 0 {
		t.Errorf("per-strategy stats = %+v, want opens 1 W1 L0", one)
	}
}

// --- startup checks --------------------------------------------------------------

func TestValidatePerpsDirectionConfigSkipsHedgeLegs(t *testing.T) {
	cfg := &Config{Strategies: []StrategyConfig{hedgeTestStrategy("hl-eth", "ETH", "BTC")}}
	state := &AppState{Strategies: map[string]*StrategyState{
		"hl-eth": {ID: "hl-eth", Positions: map[string]*Position{
			// inverse hedge leg: SHORT under direction="long" — must not warn
			"BTC": {Symbol: "BTC", Quantity: 0.04, AvgCost: 50000, Side: "short", HedgeFor: "ETH"},
		}},
	}}
	if warnings := ValidatePerpsDirectionConfig(state, cfg); len(warnings) != 0 {
		t.Fatalf("hedge leg tripped the direction gap warning: %v", warnings)
	}
}

func TestValidateHedgeStateConsistency(t *testing.T) {
	mkState := func() *AppState {
		return &AppState{Strategies: map[string]*StrategyState{
			"hl-eth": {ID: "hl-eth", Positions: map[string]*Position{
				"BTC": {Symbol: "BTC", Quantity: 0.04, Side: "short", HedgeFor: "ETH"},
			}},
		}}
	}
	// consistent: no warnings
	cfg := &Config{Strategies: []StrategyConfig{hedgeTestStrategy("hl-eth", "ETH", "BTC")}}
	if w := validateHedgeStateConsistency(mkState(), cfg); len(w) != 0 {
		t.Fatalf("consistent state warned: %v", w)
	}
	// hedge disabled after restart
	off := &Config{Strategies: []StrategyConfig{{ID: "hl-eth", Type: "perps", Platform: "hyperliquid"}}}
	if w := validateHedgeStateConsistency(mkState(), off); len(w) != 1 || !strings.Contains(w[0], "no longer enables") {
		t.Fatalf("disabled hedge with persisted leg must warn, got %v", w)
	}
	// hedge symbol changed after restart
	moved := &Config{Strategies: []StrategyConfig{hedgeTestStrategy("hl-eth", "ETH", "SOL")}}
	if w := validateHedgeStateConsistency(mkState(), moved); len(w) != 1 || !strings.Contains(w[0], "hedge.symbol now resolves") {
		t.Fatalf("changed hedge symbol must warn, got %v", w)
	}
	// unstamped position on the configured hedge coin
	unstamped := &AppState{Strategies: map[string]*StrategyState{
		"hl-eth": {ID: "hl-eth", Positions: map[string]*Position{
			"BTC": {Symbol: "BTC", Quantity: 0.04, Side: "short"},
		}},
	}}
	if w := validateHedgeStateConsistency(unstamped, cfg); len(w) != 1 || !strings.Contains(w[0], "no hedge ownership stamp") {
		t.Fatalf("unstamped position on hedge coin must warn, got %v", w)
	}
}

// --- kill switch / CB ------------------------------------------------------------

func TestSnapshotVirtualQuantitiesIncludesHedgeLegs(t *testing.T) {
	hlLiveAll := []StrategyConfig{hedgeTestStrategy("hl-eth", "ETH", "BTC")}
	strategies := map[string]*StrategyState{
		"hl-eth": {ID: "hl-eth", Positions: map[string]*Position{
			"ETH": {Symbol: "ETH", Quantity: 2, Side: "long"},
			"BTC": {Symbol: "BTC", Quantity: 0.04, Side: "short", HedgeFor: "ETH"},
		}},
	}
	out := snapshotHyperliquidVirtualQuantities(strategies, hlLiveAll)
	if out["ETH"]["hl-eth"] != 2 || out["BTC"]["hl-eth"] != 0.04 {
		t.Fatalf("snapshot = %v, want both primary and hedge legs", out)
	}
}

func TestForceCloseHyperliquidLiveHedgeRosterGating(t *testing.T) {
	hlLiveAll := []StrategyConfig{hedgeTestStrategy("hl-eth", "ETH", "BTC")}
	positions := []HLPosition{
		{Coin: "ETH", Size: 2},
		{Coin: "BTC", Size: -0.04},
	}
	var closed []string
	closer := func(coin string, partial *float64, oids []int64) (*HyperliquidCloseResult, error) {
		closed = append(closed, coin)
		return &HyperliquidCloseResult{Close: &HyperliquidClose{Symbol: coin}}, nil
	}
	// hedge leg HELD → BTC joins the roster
	report := forceCloseHyperliquidLive(t.Context(), positions, hlLiveAll, closer, nil, map[string]bool{"BTC": true})
	if len(report.ClosedCoins) != 2 {
		t.Fatalf("held hedge coin must be closed, got %v", report.ClosedCoins)
	}
	// hedge declared but NOT held → BTC is a foreign position, untouched
	closed = nil
	report = forceCloseHyperliquidLive(t.Context(), positions, hlLiveAll, closer, nil, nil)
	if len(report.ClosedCoins) != 1 || report.ClosedCoins[0] != "ETH" {
		t.Fatalf("flat hedge coin must never liquidate a foreign position, got %v", report.ClosedCoins)
	}
}

func TestHedgePendingCircuitCloseSymbols(t *testing.T) {
	s := &StrategyState{ID: "hl-eth", Positions: map[string]*Position{
		"ETH": {Symbol: "ETH", Quantity: 2, Side: "long"},
		"BTC": {Symbol: "BTC", Quantity: 0.04, Side: "short", HedgeFor: "ETH"},
	}}
	// on-chain size wins over virtual
	got := hedgePendingCircuitCloseSymbols(s, []HLPosition{{Coin: "BTC", Size: -0.05}})
	if len(got) != 1 || got[0].Symbol != "BTC" || !approxEq(got[0].Size, 0.05) {
		t.Fatalf("pending symbols = %+v, want BTC sized to on-chain 0.05", got)
	}
	// falls back to virtual when the snapshot misses the coin
	got = hedgePendingCircuitCloseSymbols(s, nil)
	if len(got) != 1 || !approxEq(got[0].Size, 0.04) {
		t.Fatalf("pending symbols = %+v, want virtual fallback 0.04", got)
	}
}

func TestSetHyperliquidCircuitBreakerPendingIncludesHedge(t *testing.T) {
	sc := hedgeTestStrategy("hl-eth", "ETH", "BTC")
	sc.Args = []string{"strat", "ETH", "1h", "--mode=live"}
	s := &StrategyState{ID: "hl-eth", Positions: map[string]*Position{
		"ETH": {Symbol: "ETH", Quantity: 2, Side: "long"},
		"BTC": {Symbol: "BTC", Quantity: 0.04, Side: "short", HedgeFor: "ETH"},
	}}
	assist := &PlatformRiskAssist{
		HLPositions: []HLPosition{{Coin: "ETH", Size: 2}, {Coin: "BTC", Size: -0.04}},
		HLLiveAll:   []StrategyConfig{sc},
	}
	setHyperliquidCircuitBreakerPending(&sc, s, assist)
	p := s.RiskState.getPendingCircuitClose(PlatformPendingCloseHyperliquid)
	if p == nil || len(p.Symbols) != 2 {
		t.Fatalf("pending = %+v, want primary + hedge symbols", p)
	}
	if p.Symbols[0].Symbol != "ETH" || p.Symbols[1].Symbol != "BTC" {
		t.Fatalf("pending order = %+v, want [ETH BTC]", p.Symbols)
	}
}

func TestSetHyperliquidCircuitBreakerPendingSharedPrimaryStillEnqueuesHedge(t *testing.T) {
	sc := hedgeTestStrategy("hl-eth", "ETH", "BTC")
	sc.Args = []string{"strat", "ETH", "1h", "--mode=live"}
	peer := StrategyConfig{ID: "hl-eth-2", Type: "perps", Platform: "hyperliquid", Args: []string{"s", "ETH", "1h", "--mode=live"}}
	s := &StrategyState{ID: "hl-eth", Positions: map[string]*Position{
		"ETH": {Symbol: "ETH", Quantity: 2, Side: "long"},
		"BTC": {Symbol: "BTC", Quantity: 0.04, Side: "short", HedgeFor: "ETH"},
	}}
	assist := &PlatformRiskAssist{
		HLPositions: []HLPosition{{Coin: "ETH", Size: 4}, {Coin: "BTC", Size: -0.04}},
		HLLiveAll:   []StrategyConfig{sc, peer},
	}
	setHyperliquidCircuitBreakerPending(&sc, s, assist)
	p := s.RiskState.getPendingCircuitClose(PlatformPendingCloseHyperliquid)
	if p == nil || len(p.Symbols) != 1 || p.Symbols[0].Symbol != "BTC" {
		t.Fatalf("shared primary must still enqueue the hedge-only pending, got %+v", p)
	}
}

// --- hot reload -------------------------------------------------------------------

func TestValidateHotReloadStateCompatibleHedge(t *testing.T) {
	mk := func(h *HedgeConfig) *Config {
		sc := StrategyConfig{
			ID: "hl-eth", Type: "perps", Platform: "hyperliquid", Script: "x.py",
			Args: []string{"a", "ETH", "1h"}, Capital: 1000, MaxDrawdownPct: 10,
			Leverage: 5, MarginMode: "isolated", Hedge: h,
		}
		return minimalReloadConfig([]StrategyConfig{sc})
	}
	openState := &AppState{Strategies: map[string]*StrategyState{
		"hl-eth": {ID: "hl-eth", Positions: map[string]*Position{
			"ETH": {Symbol: "ETH", Quantity: 0.5, AvgCost: 3000, Side: "long"},
		}},
	}}
	// residual hedge leg only (primary flat) also counts as open
	hedgeOnlyState := &AppState{Strategies: map[string]*StrategyState{
		"hl-eth": {ID: "hl-eth", Positions: map[string]*Position{
			"BTC": {Symbol: "BTC", Quantity: 0.04, AvgCost: 50000, Side: "short", HedgeFor: "ETH"},
		}},
	}}
	flatState := &AppState{Strategies: map[string]*StrategyState{
		"hl-eth": {ID: "hl-eth", Positions: map[string]*Position{}},
	}}
	on := &HedgeConfig{Enabled: true, Symbol: "BTC"}
	changed := &HedgeConfig{Enabled: true, Symbol: "BTC", Ratio: 0.5}

	for _, tc := range []struct {
		name     string
		old, new *HedgeConfig
		state    *AppState
		blocked  bool
	}{
		{"enable while open", nil, on, openState, true},
		{"disable while open", on, nil, openState, true},
		{"retune while open", on, changed, openState, true},
		{"retune while only hedge leg open", on, changed, hedgeOnlyState, true},
		{"retune while flat", on, changed, flatState, false},
		{"no-op while open", on, on, openState, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := validateHotReloadStateCompatible(mk(tc.old), mk(tc.new), tc.state)
			if tc.blocked && (err == nil || !strings.Contains(err.Error(), "hedge block changed")) {
				t.Fatalf("want hedge block error, got %v", err)
			}
			if !tc.blocked && err != nil {
				t.Fatalf("want accept, got %v", err)
			}
		})
	}
}

func TestStrategyRestartShapeMasksHedge(t *testing.T) {
	a := StrategyConfig{ID: "hl-a", Hedge: &HedgeConfig{Enabled: true, Symbol: "BTC"}}
	b := StrategyConfig{ID: "hl-a"}
	if !reflect.DeepEqual(strategyRestartShape(a), strategyRestartShape(b)) {
		t.Fatal("hedge change must not flag restart-required (hot-reloadable when flat)")
	}
}

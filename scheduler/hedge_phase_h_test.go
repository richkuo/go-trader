package main

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// Phase H — kill switch
// ---------------------------------------------------------------------------

func TestHeldHedgeCoinsForKillSwitch_GatesOnHeldLeg(t *testing.T) {
	sc := hlHedgeTestStrategy("hl-eth", &HedgeConfig{Enabled: true, Symbol: "BTC"})

	// Held leg (stamp + qty) → coin included.
	state := map[string]*StrategyState{"hl-eth": hedgeTestStrategyState()}
	if got := heldHedgeCoinsForKillSwitch(state, []StrategyConfig{sc}); !got["BTC"] {
		t.Errorf("held hedge leg must join the kill-switch roster, got %v", got)
	}

	// Declared but flat → excluded (a foreign position on the coin must not
	// be liquidated).
	flat := hedgeTestStrategyState()
	delete(flat.Positions, "BTC")
	if got := heldHedgeCoinsForKillSwitch(map[string]*StrategyState{"hl-eth": flat}, []StrategyConfig{sc}); len(got) != 0 {
		t.Errorf("declared-but-flat hedge coin must be excluded, got %v", got)
	}

	// Position on the hedge coin WITHOUT the stamp → excluded (ownership
	// comes from the persisted HedgeFor stamp, never the coin).
	unstamped := hedgeTestStrategyState()
	unstamped.Positions["BTC"].HedgeFor = ""
	if got := heldHedgeCoinsForKillSwitch(map[string]*StrategyState{"hl-eth": unstamped}, []StrategyConfig{sc}); len(got) != 0 {
		t.Errorf("unstamped hedge-coin position must be excluded, got %v", got)
	}

	// No hedge block → excluded.
	plain := hlHedgeTestStrategy("hl-eth", nil)
	if got := heldHedgeCoinsForKillSwitch(state, []StrategyConfig{plain}); len(got) != 0 {
		t.Errorf("no hedge block → empty set, got %v", got)
	}
}

func TestForceCloseHyperliquidLive_HedgeCoinRosterGating(t *testing.T) {
	sc := hlHedgeTestStrategy("hl-eth", &HedgeConfig{Enabled: true, Symbol: "BTC"})
	positions := []HLPosition{
		{Coin: "ETH", Size: 1.5, EntryPrice: 3000},
		{Coin: "BTC", Size: -0.05, EntryPrice: 90000},
	}

	// Held hedge leg → BTC joins the close roster and appears in ClosedCoins
	// (formatKillSwitchMessage then surfaces it naturally).
	closer, calls := fakeCloser(nil)
	report := forceCloseHyperliquidLive(context.Background(), positions, []StrategyConfig{sc}, closer, nil, map[string]bool{"BTC": true})
	if len(report.Errors) != 0 {
		t.Fatalf("unexpected errors: %v", report.Errors)
	}
	if got, want := strings.Join(report.ClosedCoins, ","), "ETH,BTC"; got != want {
		t.Errorf("ClosedCoins = %v, want %v", got, want)
	}
	if got, want := strings.Join(*calls, ","), "ETH,BTC"; got != want {
		t.Errorf("closer calls = %v, want %v", got, want)
	}

	// Declared-but-flat (no held leg) → BTC is left alone like any unowned coin.
	closer, calls = fakeCloser(nil)
	report = forceCloseHyperliquidLive(context.Background(), positions, []StrategyConfig{sc}, closer, nil, nil)
	if got, want := strings.Join(report.ClosedCoins, ","), "ETH"; got != want {
		t.Errorf("ClosedCoins = %v, want %v", got, want)
	}
	if got, want := strings.Join(*calls, ","), "ETH"; got != want {
		t.Errorf("closer calls = %v, want %v", got, want)
	}
}

func TestSnapshotHyperliquidVirtualQuantities_IncludesHedgeLegs(t *testing.T) {
	sc := hlHedgeTestStrategy("hl-eth", &HedgeConfig{Enabled: true, Symbol: "BTC"})
	state := map[string]*StrategyState{"hl-eth": hedgeTestStrategyState()}

	snap := snapshotHyperliquidVirtualQuantities(state, []StrategyConfig{sc})
	if got := snap["ETH"]["hl-eth"]; got != 1.5 {
		t.Errorf("primary snapshot = %g, want 1.5", got)
	}
	if got := snap["BTC"]["hl-eth"]; got != 0.05 {
		t.Errorf("hedge snapshot = %g, want 0.05", got)
	}

	// Unstamped hedge-coin position → not snapshotted as a hedge leg.
	state["hl-eth"].Positions["BTC"].HedgeFor = ""
	snap = snapshotHyperliquidVirtualQuantities(state, []StrategyConfig{sc})
	if _, ok := snap["BTC"]; ok {
		t.Errorf("unstamped hedge-coin position must not be snapshotted, got %v", snap["BTC"])
	}
}

func TestApplyHyperliquidKillSwitchCloseFill_BooksHedgeFill(t *testing.T) {
	sc := hlHedgeTestStrategy("hl-eth", &HedgeConfig{Enabled: true, Symbol: "BTC"})
	sc.Args = []string{"sma_crossover", "ETH", "1h", "--mode=live"}
	hlLiveAll := []StrategyConfig{sc}

	s := hedgeTestStrategyState()
	virtualQty := snapshotHyperliquidVirtualQuantities(map[string]*StrategyState{"hl-eth": s}, hlLiveAll)
	fills := map[string]HyperliquidCloseFill{
		"ETH": {TotalSz: 1.5, AvgPx: 2900, Fee: 0.5, OID: 101},
		"BTC": {TotalSz: 0.05, AvgPx: 92000, Fee: 0.1, OID: 202},
	}

	// Production ordering: fill attribution first, generic cleanup second
	// (forceCloseKillSwitchPositions). Cleanup must find nothing residual.
	forceCloseKillSwitchPositions(s, sc, nil, fills, hlLiveAll, virtualQty, nil)

	if len(s.Positions) != 0 {
		t.Errorf("both legs should be closed, still have: %v", s.Positions)
	}
	if len(s.TradeHistory) != 2 {
		t.Fatalf("want exactly 2 close trades (no double-book), got %d: %+v", len(s.TradeHistory), s.TradeHistory)
	}
	tradeTypeBySymbol := map[string]string{}
	for _, tr := range s.TradeHistory {
		tradeTypeBySymbol[tr.Symbol] = tr.TradeType
		if !tr.IsClose {
			t.Errorf("kill-switch rows must be close legs: %+v", tr)
		}
	}
	if tradeTypeBySymbol["ETH"] != "perps" {
		t.Errorf("primary trade_type = %q, want perps", tradeTypeBySymbol["ETH"])
	}
	if tradeTypeBySymbol["BTC"] != hedgeTradeType {
		t.Errorf("hedge trade_type = %q, want %q — stats exclusion would leak", tradeTypeBySymbol["BTC"], hedgeTradeType)
	}
	// ETH: 1.5*(2900-3000)-0.5 = -150.5 (streak). BTC hedge:
	// 0.05*(90000-92000)-0.1 = -100.1 (DailyPnL only).
	if got, want := s.RiskState.DailyPnL, -250.6; got != want {
		t.Errorf("DailyPnL = %g, want %g", got, want)
	}
	if s.RiskState.ConsecutiveLosses != 1 {
		t.Errorf("ConsecutiveLosses = %d, want 1 (primary leg only)", s.RiskState.ConsecutiveLosses)
	}
	if got, want := s.Cash, 1000-250.6; got != want {
		t.Errorf("Cash = %g, want %g", got, want)
	}

	// Reverse ordering guard: the reconciler already booked the BTC fill
	// (won the race, position row gone) — the kill-switch apply must NOT
	// book a second, defensive row for the same OID (#954).
	s2 := hedgeTestStrategyState()
	btcPos := s2.Positions["BTC"]
	RecordTrade(s2, Trade{
		Timestamp: time.Now().UTC(), StrategyID: s2.ID, Symbol: "BTC",
		Side: "buy", Quantity: 0.05, Price: 92000, IsClose: true,
		TradeType: hedgeTradeType, ExchangeOrderID: "202",
	})
	delete(s2.Positions, "BTC")
	_ = btcPos
	tradesBefore := len(s2.TradeHistory)
	cashBefore := s2.Cash
	applyHyperliquidKillSwitchCloseFill(s2, sc, fills, hlLiveAll, virtualQty)
	var btcRows int
	for _, tr := range s2.TradeHistory {
		if tr.Symbol == "BTC" {
			btcRows++
		}
	}
	if btcRows != 1 {
		t.Errorf("reconcile-prebooked BTC fill must not double-book, got %d BTC rows", btcRows)
	}
	if len(s2.TradeHistory) != tradesBefore+1 { // ETH booked normally
		t.Errorf("TradeHistory grew by %d, want exactly 1 (ETH only)", len(s2.TradeHistory)-tradesBefore)
	}
	if s2.Cash != cashBefore+(1.5*(2900-3000)-0.5) {
		t.Errorf("Cash = %g, ETH close PnL only expected", s2.Cash)
	}
}

// ---------------------------------------------------------------------------
// Phase H — per-strategy circuit breaker
// ---------------------------------------------------------------------------

func TestSetHyperliquidCircuitBreakerPending_IncludesHedgeSymbol(t *testing.T) {
	sc := hlHedgeTestStrategy("hl-eth", &HedgeConfig{Enabled: true, Symbol: "BTC"})
	sc.Args = []string{"sma_crossover", "ETH", "1h", "--mode=live"}
	assist := &PlatformRiskAssist{
		HLPositions: []HLPosition{
			{Coin: "ETH", Size: 1.5, EntryPrice: 3000},
			{Coin: "BTC", Size: -0.05, EntryPrice: 90000},
		},
		HLLiveAll: []StrategyConfig{sc},
	}

	s := hedgeTestStrategyState()
	setHyperliquidCircuitBreakerPending(&sc, s, assist)
	p := s.RiskState.getPendingCircuitClose(PlatformPendingCloseHyperliquid)
	if p == nil {
		t.Fatal("pending close not set")
	}
	if len(p.Symbols) != 2 {
		t.Fatalf("pending symbols = %+v, want ETH + BTC hedge leg", p.Symbols)
	}
	if p.Symbols[0].Symbol != "ETH" || p.Symbols[0].Size != 1.5 {
		t.Errorf("primary pending = %+v, want {ETH 1.5}", p.Symbols[0])
	}
	if p.Symbols[1].Symbol != "BTC" || p.Symbols[1].Size != 0.05 {
		t.Errorf("hedge pending = %+v, want {BTC 0.05} (on-chain abs)", p.Symbols[1])
	}

	// Control: no held hedge leg → primary only.
	s2 := hedgeTestStrategyState()
	delete(s2.Positions, "BTC")
	setHyperliquidCircuitBreakerPending(&sc, s2, assist)
	p2 := s2.RiskState.getPendingCircuitClose(PlatformPendingCloseHyperliquid)
	if p2 == nil || len(p2.Symbols) != 1 || p2.Symbols[0].Symbol != "ETH" {
		t.Errorf("flat hedge → primary-only pending, got %+v", p2)
	}
}

func TestRunPendingHyperliquidCircuitCloses_StuckCBRecoversHedgeSymbol(t *testing.T) {
	sc := hlHedgeTestStrategy("hl-eth", &HedgeConfig{Enabled: true, Symbol: "BTC"})
	sc.Args = []string{"sma_crossover", "ETH", "1h", "--mode=live"}

	state := NewAppState()
	s := hedgeTestStrategyState()
	s.RiskState.CircuitBreaker = true // latched, but no pending — the stuck case
	state.Strategies["hl-eth"] = s

	hlPositions := []HLPosition{
		{Coin: "ETH", Size: 1.5, EntryPrice: 3000},
		{Coin: "BTC", Size: -0.05, EntryPrice: 90000},
	}

	var calls []string
	closer := func(symbol string, partialSz *float64, cancelOIDs []int64) (*HyperliquidCloseResult, error) {
		sz := 0.0
		if partialSz != nil {
			sz = *partialSz
		}
		calls = append(calls, symbol)
		px := map[string]float64{"ETH": 2900, "BTC": 92000}[symbol]
		oid := map[string]int64{"ETH": 1001, "BTC": 1002}[symbol]
		return &HyperliquidCloseResult{
			Close: &HyperliquidClose{Symbol: symbol, Fill: &HyperliquidCloseFill{TotalSz: sz, AvgPx: px, OID: oid}},
		}, nil
	}

	var mu sync.RWMutex
	runPendingHyperliquidCircuitCloses(
		context.Background(), state, []StrategyConfig{sc}, "addr",
		hlPositions, true, nil, closer, time.Minute, &mu, nil)

	if got, want := strings.Join(calls, ","), "ETH,BTC"; got != want {
		t.Errorf("drain closer calls = %v, want %v (hedge symbol reconstructed)", got, want)
	}
	if p := s.RiskState.getPendingCircuitClose(PlatformPendingCloseHyperliquid); p != nil {
		t.Errorf("pending should be cleared after a clean drain, got %+v", p)
	}
	if len(s.Positions) != 0 {
		t.Errorf("both legs should be closed, still have: %v", s.Positions)
	}
	tradeTypeBySymbol := map[string]string{}
	for _, tr := range s.TradeHistory {
		tradeTypeBySymbol[tr.Symbol] = tr.TradeType
	}
	if tradeTypeBySymbol["BTC"] != hedgeTradeType {
		t.Errorf("CB-drained hedge close trade_type = %q, want %q", tradeTypeBySymbol["BTC"], hedgeTradeType)
	}
	// ETH: 1.5*(2900-3000) = -150 (streak). BTC hedge: 0.05*(90000-92000) =
	// -100 (DailyPnL only).
	if s.RiskState.ConsecutiveLosses != 1 {
		t.Errorf("ConsecutiveLosses = %d, want 1 (primary leg only)", s.RiskState.ConsecutiveLosses)
	}
	if got, want := s.RiskState.DailyPnL, -250.0; got != want {
		t.Errorf("DailyPnL = %g, want %g", got, want)
	}
}

// ---------------------------------------------------------------------------
// Phase H — manual force-close
// ---------------------------------------------------------------------------

// forceCloseHedgeTestDeps builds the manual-core deps for a hedge-enabled
// live HL perps strategy holding ETH 1.5 long + BTC 0.05 short (hedge).
func forceCloseHedgeTestDeps(t *testing.T, sc StrategyConfig, closer HyperliquidLiveCloser) (manualCoreDeps, *StateDB) {
	t.Helper()
	db := openTestDB(t)
	positions := hedgeTestStrategyState().Positions
	return manualCoreDeps{
		cfg:     &Config{Strategies: []StrategyConfig{sc}},
		stateDB: db,
		loadState: func(strategyID, symbol string) (manualStateView, error) {
			return manualStateView{HasStrategy: true, Pos: positions[symbol]}, nil
		},
		closer: closer,
	}, db
}

// fillByCoinCloser records (coin, sz) calls and fills the requested size (or
// the map qty for full closes) at the map price with a per-coin OID — OIDs
// must differ per coin or the drain's #954 dup-OID guard would skip the
// second adoption row.
func fillByCoinCloser(calls *[]string, qty, px map[string]float64, failCoin string) HyperliquidLiveCloser {
	oid := int64(5000)
	return func(coin string, sz *float64, cancelOIDs []int64) (*HyperliquidCloseResult, error) {
		oid++
		if sz != nil {
			*calls = append(*calls, coin+":partial")
		} else {
			*calls = append(*calls, coin+":full")
		}
		if coin == failCoin {
			return nil, errors.New("simulated hedge close failure")
		}
		q := qty[coin]
		if sz != nil {
			q = *sz
		}
		return &HyperliquidCloseResult{
			Close: &HyperliquidClose{Symbol: coin, Fill: &HyperliquidCloseFill{TotalSz: q, AvgPx: px[coin], OID: oid}},
		}, nil
	}
}

func TestForceCloseCore_FullIntentClosesHedgeLeg(t *testing.T) {
	sc := hlHedgeTestStrategy("hl-eth", &HedgeConfig{Enabled: true, Symbol: "BTC"})
	sc.Args = []string{"sma_crossover", "ETH", "1h", "--mode=live"}

	var calls []string
	deps, db := forceCloseHedgeTestDeps(t, sc, fillByCoinCloser(&calls,
		map[string]float64{"ETH": 1.5, "BTC": 0.05},
		map[string]float64{"ETH": 2900, "BTC": 92000}, ""))

	res, err := forceCloseCore(deps, sc, "ETH", forceCloseInputs{StrategyID: "hl-eth"})
	if err != nil {
		t.Fatalf("forceCloseCore: %v", err)
	}
	if !res.queued {
		t.Error("primary close should be queued")
	}
	if got, want := strings.Join(calls, ","), "ETH:full,BTC:full"; got != want {
		t.Errorf("closer calls = %v, want %v", got, want)
	}
	for _, l := range res.lines {
		if l.stderr && strings.Contains(l.text, "[CRITICAL]") {
			t.Errorf("unexpected CRITICAL line: %s", l.text)
		}
	}

	actions, err := db.LoadPendingManualActions()
	if err != nil {
		t.Fatalf("LoadPendingManualActions: %v", err)
	}
	if len(actions) != 2 {
		t.Fatalf("want 2 queued close rows (primary + hedge), got %d: %+v", len(actions), actions)
	}
	primary, hedge := actions[0], actions[1]
	if primary.Symbol != "ETH" || !primary.IsFullClose || primary.Quantity != 1.5 {
		t.Errorf("primary row = %+v", primary)
	}
	if hedge.Symbol != "BTC" || hedge.Side != "buy" || !hedge.IsFullClose || hedge.Quantity != 0.05 {
		t.Errorf("hedge row = %+v", hedge)
	}
	// Hedge PnL: 0.05*(90000-92000) - 0 fee = -100.
	if hedge.RealizedPnL != -100 {
		t.Errorf("hedge RealizedPnL = %g, want -100", hedge.RealizedPnL)
	}
	if hedge.ExchangeOrderID == "" || hedge.ExchangeOrderID == primary.ExchangeOrderID {
		t.Errorf("hedge row needs its own OID, got %q (primary %q)", hedge.ExchangeOrderID, primary.ExchangeOrderID)
	}
}

func TestForceCloseCore_PartialIntentTrimsHedgeLegProportionally(t *testing.T) {
	sc := hlHedgeTestStrategy("hl-eth", &HedgeConfig{Enabled: true, Symbol: "BTC"})
	sc.Args = []string{"sma_crossover", "ETH", "1h", "--mode=live"}

	var calls []string
	deps, db := forceCloseHedgeTestDeps(t, sc, fillByCoinCloser(&calls,
		map[string]float64{"ETH": 1.5, "BTC": 0.05},
		map[string]float64{"ETH": 2900, "BTC": 92000}, ""))

	// Half close: 0.75 of 1.5 → hedge trims 0.025 of 0.05.
	res, err := forceCloseCore(deps, sc, "ETH", forceCloseInputs{StrategyID: "hl-eth", Qty: 0.75})
	if err != nil {
		t.Fatalf("forceCloseCore: %v", err)
	}
	if !res.queued {
		t.Error("primary close should be queued")
	}
	if got, want := strings.Join(calls, ","), "ETH:partial,BTC:partial"; got != want {
		t.Errorf("closer calls = %v, want %v", got, want)
	}
	actions, err := db.LoadPendingManualActions()
	if err != nil {
		t.Fatalf("LoadPendingManualActions: %v", err)
	}
	if len(actions) != 2 {
		t.Fatalf("want 2 queued close rows, got %d", len(actions))
	}
	hedge := actions[1]
	if hedge.Symbol != "BTC" || hedge.IsFullClose || hedge.Quantity != 0.025 {
		t.Errorf("hedge row = %+v, want proportional 0.025 partial", hedge)
	}
	if hedge.RealizedPnL != -50 { // 0.025*(90000-92000)
		t.Errorf("hedge RealizedPnL = %g, want -50", hedge.RealizedPnL)
	}
}

func TestForceCloseCore_HedgeCloseFailureLeavesBackstop(t *testing.T) {
	sc := hlHedgeTestStrategy("hl-eth", &HedgeConfig{Enabled: true, Symbol: "BTC"})
	sc.Args = []string{"sma_crossover", "ETH", "1h", "--mode=live"}

	var calls []string
	deps, db := forceCloseHedgeTestDeps(t, sc, fillByCoinCloser(&calls,
		map[string]float64{"ETH": 1.5, "BTC": 0.05},
		map[string]float64{"ETH": 2900, "BTC": 92000}, "BTC"))

	res, err := forceCloseCore(deps, sc, "ETH", forceCloseInputs{StrategyID: "hl-eth"})
	if err != nil {
		t.Fatalf("hedge failure must not fail the primary close: %v", err)
	}
	if !res.queued {
		t.Error("primary close should still be queued")
	}
	var critical bool
	for _, l := range res.lines {
		if l.stderr && strings.Contains(l.text, "[CRITICAL]") && strings.Contains(l.text, "BTC") && strings.Contains(l.text, "backstop") {
			critical = true
		}
	}
	if !critical {
		t.Errorf("want a CRITICAL hedge-failure line naming the backstop, got %+v", res.lines)
	}
	actions, err := db.LoadPendingManualActions()
	if err != nil {
		t.Fatalf("LoadPendingManualActions: %v", err)
	}
	if len(actions) != 1 || actions[0].Symbol != "ETH" {
		t.Errorf("only the primary row should be queued, got %+v", actions)
	}
}

// TestDrainPendingManualActions_HedgeCloseRow pins the adoption side: a
// queued hedge-symbol close row materializes with the hedge trade_type and
// the phase-B risk routing (DailyPnL only, never the streak).
func TestDrainPendingManualActions_HedgeCloseRow(t *testing.T) {
	sc := hlHedgeTestStrategy("hl-eth", &HedgeConfig{Enabled: true, Symbol: "BTC"})
	sc.Args = []string{"sma_crossover", "ETH", "1h", "--mode=live"}

	db := openTestDB(t)
	now := time.Now().UTC()
	for _, a := range []PendingManualAction{
		{StrategyID: "hl-eth", Action: "close", Symbol: "ETH", Side: "sell", Quantity: 1.5, FillPrice: 2900, RealizedPnL: -150, IsFullClose: true, ExchangeOrderID: "1001", CreatedAt: now},
		{StrategyID: "hl-eth", Action: "close", Symbol: "BTC", Side: "buy", Quantity: 0.05, FillPrice: 92000, RealizedPnL: -100, IsFullClose: true, ExchangeOrderID: "1002", CreatedAt: now},
	} {
		if err := db.InsertPendingManualAction(a); err != nil {
			t.Fatalf("InsertPendingManualAction: %v", err)
		}
	}

	state := NewAppState()
	state.Strategies["hl-eth"] = hedgeTestStrategyState()
	cfg := &Config{Strategies: []StrategyConfig{sc}}

	drainPendingManualActions(state, cfg, db)

	s := state.Strategies["hl-eth"]
	if len(s.Positions) != 0 {
		t.Errorf("both legs should be adopted as closed, still have: %v", s.Positions)
	}
	tradeTypeBySymbol := map[string]string{}
	for _, tr := range s.TradeHistory {
		tradeTypeBySymbol[tr.Symbol] = tr.TradeType
	}
	if tradeTypeBySymbol["ETH"] != "perps" {
		t.Errorf("primary trade_type = %q, want perps", tradeTypeBySymbol["ETH"])
	}
	if tradeTypeBySymbol["BTC"] != hedgeTradeType {
		t.Errorf("adopted hedge close trade_type = %q, want %q", tradeTypeBySymbol["BTC"], hedgeTradeType)
	}
	if s.RiskState.ConsecutiveLosses != 1 {
		t.Errorf("ConsecutiveLosses = %d, want 1 (primary leg only)", s.RiskState.ConsecutiveLosses)
	}
	if got, want := s.RiskState.DailyPnL, -250.0; got != want {
		t.Errorf("DailyPnL = %g, want %g", got, want)
	}
}

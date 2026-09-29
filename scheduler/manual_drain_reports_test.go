package main

import (
	"go/ast"
	"strings"
	"sync"
	"testing"
	"time"
)

type drainReportFixture struct {
	state *AppState
	cfg   *Config
	db    *StateDB
	store *StateStore
	id    string
	sc    StrategyConfig
}

func newDrainReportFixture(t *testing.T) *drainReportFixture {
	t.Helper()
	hlStepNoRecorder(t)
	db, err := OpenStateDB(":memory:")
	if err != nil {
		t.Fatalf("open state db: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	id := "hl-manual-eth-live"
	sc := StrategyConfig{ID: id, Type: "manual", Platform: "hyperliquid", Symbol: "ETH", Leverage: 10, Args: []string{"manual", "ETH", "--mode=live"}}
	state := &AppState{Strategies: map[string]*StrategyState{
		id: {ID: id, Platform: "hyperliquid", Type: "manual", Positions: map[string]*Position{}, Cash: 10000},
	}}
	return &drainReportFixture{state: state, cfg: &Config{Strategies: []StrategyConfig{sc}}, db: db, store: openTestStore(t, db), id: id, sc: sc}
}

func (f *drainReportFixture) queue(t *testing.T, a PendingManualAction) {
	t.Helper()
	a.StrategyID = f.id
	if a.Symbol == "" {
		a.Symbol = "ETH"
	}
	if a.CreatedAt.IsZero() {
		a.CreatedAt = time.Now().UTC()
	}
	if err := f.db.InsertPendingManualAction(a); err != nil {
		t.Fatalf("queue %s: %v", a.Action, err)
	}
}

func (f *drainReportFixture) drain(t *testing.T) []manualAlert {
	t.Helper()
	alerts, _ := drainPendingManualActions(f.state, f.cfg, f.store)
	return alerts
}

func (f *drainReportFixture) seedClose(oid string) {
	ss := f.state.Strategies[f.id]
	ss.TradeHistory = append(ss.TradeHistory, Trade{StrategyID: f.id, Symbol: "ETH", Side: "sell", Quantity: 1, Price: 2100, IsClose: true, ExchangeOrderID: oid})
}

func TestDrainReportsOnlyBookedPublicRows(t *testing.T) {
	f := newDrainReportFixture(t)
	f.seedClose("dup-1")
	f.queue(t, PendingManualAction{Action: "close", Side: "long", Quantity: 1, FillPrice: 2100, IsFullClose: true, ExchangeOrderID: "dup-1"})
	if alerts := f.drain(t); len(alerts) != 0 {
		t.Fatalf("a duplicate close alone produced %d alert entries, want 0", len(alerts))
	}

	f.queue(t, PendingManualAction{Action: "close", Side: "long", Quantity: 1, FillPrice: 2100, IsFullClose: true, ExchangeOrderID: "dup-1"})
	f.queue(t, PendingManualAction{Action: "open", Side: "long", Quantity: 0.5, FillPrice: 2000, EntryATR: 50, ExchangeOrderID: "open-1"})
	alerts := f.drain(t)
	if len(alerts) != 1 || len(alerts[0].rows) != 1 || alerts[0].trades != 1 {
		t.Fatalf("duplicate close plus open = %+v, want one entry with one row", alerts)
	}
	if alerts[0].rows[0].IsClose || alerts[0].rows[0].ExchangeOrderID != "open-1" {
		t.Fatalf("reported row %+v, want the new open and not the seeded close", alerts[0].rows[0])
	}
}

func TestDrainReportsOpenAddCloseRows(t *testing.T) {
	f := newDrainReportFixture(t)
	f.queue(t, PendingManualAction{Action: "open", Side: "long", Quantity: 1, FillPrice: 2000, EntryATR: 50, ExchangeOrderID: "o1"})
	f.queue(t, PendingManualAction{Action: "add", Side: "long", Quantity: 0.5, FillPrice: 2010, ExchangeOrderID: "a1"})
	f.queue(t, PendingManualAction{Action: "close", Side: "long", Quantity: 1.5, FillPrice: 2100, IsFullClose: true, RealizedPnL: 10, ExchangeOrderID: "c1"})
	alerts := f.drain(t)
	if len(alerts) != 1 || len(alerts[0].rows) != 3 {
		t.Fatalf("open, add and close = %+v, want one entry with three rows", alerts)
	}
	wantClose := []bool{false, false, true}
	for i, row := range alerts[0].rows {
		if row.IsClose != wantClose[i] {
			t.Fatalf("row %d IsClose=%v, want %v", i, row.IsClose, wantClose[i])
		}
	}
}

func TestDrainStopEditsAndHedgeRowsAddNoPublicResult(t *testing.T) {
	f := newDrainReportFixture(t)
	f.state.Strategies[f.id].Positions["ETH"] = &Position{Symbol: "ETH", Side: "long", Quantity: 1, InitialQuantity: 1, AvgCost: 2000, Multiplier: 1, OwnerStrategyID: f.id, StopLossOID: 5, StopLossTriggerPx: 1900}
	f.queue(t, PendingManualAction{Action: "update-sl", Side: "long", StopLossOID: 6, StopLossTriggerPx: 1950})
	f.queue(t, PendingManualAction{Action: "cancel-sl", Side: "long"})
	if alerts := f.drain(t); len(alerts) != 0 {
		t.Fatalf("stop edit and cancellation produced %d alert entries, want 0", len(alerts))
	}

	ss := f.state.Strategies[f.id]
	before := len(ss.TradeHistory)
	ss.TradeHistory = append(ss.TradeHistory, Trade{StrategyID: f.id, Symbol: "BTC", Side: "sell", IsClose: true, TradeType: hedgeTradeType})
	if rows := drainedPublicRows(ss, before); len(rows) != 0 {
		t.Fatalf("a hedge-leg row was selected as a public row: %+v", rows)
	}
	ss.TradeHistory = append(ss.TradeHistory, Trade{StrategyID: f.id, Symbol: "ETH", Side: "buy", TradeType: "perps"})
	if rows := drainedPublicRows(ss, before); len(rows) != 1 || rows[0].Symbol != "ETH" {
		t.Fatalf("hedge row plus public row selected %+v, want only the public row", rows)
	}
}

func TestFoldManualDrainTradesCountsEachRowAndGatesChannel(t *testing.T) {
	mn := NewMultiNotifier(notifierBackend{
		notifier: &mockNotifier{},
		channels: map[string]string{"hyperliquid": "live-ch", "hyperliquid-paper": "paper-ch"},
	})
	live := StrategyConfig{ID: "hl-live", Type: "manual", Platform: "hyperliquid", Args: []string{"manual", "ETH", "--mode=live"}}
	liveBTC := StrategyConfig{ID: "hl-live-btc", Type: "manual", Platform: "hyperliquid", Args: []string{"manual", "BTC", "--mode=live"}}
	paper := StrategyConfig{ID: "hl-paper", Type: "manual", Platform: "hyperliquid", Args: []string{"manual", "ETH"}}
	row := func(sym string) Trade { return Trade{Symbol: sym, Side: "buy", Quantity: 1, Price: 100} }
	reports := []manualDrainReport{
		{sc: live, rows: []Trade{row("ETH"), row("ETH")}},
		{sc: liveBTC, rows: []Trade{row("BTC")}},
		{sc: paper, rows: []Trade{row("ETH")}},
		{sc: live},
	}
	total := 0
	channelTrades := map[string]int{}
	details := map[string][]string{}
	drained := foldManualDrainTrades(reports, mn, &total, channelTrades, details)
	if total != 4 || channelTrades["hyperliquid"] != 3 || channelTrades["hyperliquid-paper"] != 1 {
		t.Fatalf("total=%d channels=%v, want 4 total with 3 live and 1 paper", total, channelTrades)
	}
	if len(details["hyperliquid|ETH"]) != 2 || len(details["hyperliquid|BTC"]) != 1 || len(details["hyperliquid-paper|ETH"]) != 1 {
		t.Fatalf("details=%v, want one entry per row under each asset", details)
	}
	for _, line := range details["hyperliquid|ETH"] {
		if !strings.Contains(line, "[hl-live]") || !strings.Contains(line, "LIVE ") {
			t.Fatalf("live detail %q lacks the strategy tag or LIVE marker", line)
		}
	}
	if !drained["hyperliquid"] || !drained["hyperliquid-paper"] {
		t.Fatalf("drained channels = %v, want both", drained)
	}

	if !summaryChannelActive(mn, "hyperliquid", nil, drained) {
		t.Fatal("a drained channel with no due strategy must pass the activity gate")
	}
	if summaryChannelActive(mn, "hyperliquid", nil, map[string]bool{}) {
		t.Fatal("a channel with neither a due strategy nor drained rows must stay gated")
	}
	if !summaryChannelActive(mn, "hyperliquid", []StrategyConfig{live}, map[string]bool{}) {
		t.Fatal("a channel with a due strategy must pass the activity gate")
	}
	if summaryChannelActive(mn, "hyperliquid-paper", []StrategyConfig{live}, map[string]bool{}) {
		t.Fatal("a due live strategy must not activate the paper channel")
	}
}

func TestManualDrainAlertsSendOutsideLock(t *testing.T) {
	_, fn := hlStepMainFunc(t)
	var drainCall, unlockCall, sendCall ast.Node
	ast.Inspect(fn, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		switch hlStepCallName(call) {
		case "drainPendingManualActions":
			drainCall = call
		case "sendTradeAlertRows":
			if drainCall != nil && sendCall == nil {
				sendCall = call
			}
		}
		return true
	})
	if drainCall == nil || sendCall == nil {
		t.Fatal("the drain call and its row sender were not found in func main")
	}
	unlocked := false
	ast.Inspect(fn, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok || hlStepCallName(call) != "Unlock" {
			return true
		}
		if call.Pos() > drainCall.Pos() && call.Pos() < sendCall.Pos() && !unlocked {
			unlockCall = call
			unlocked = true
		}
		return true
	})
	if unlockCall == nil {
		t.Fatal("the drain row sender must run after mu.Unlock")
	}

	mock := &mockNotifier{}
	var mu sync.RWMutex
	router := &hlStepRecordingRouter{mock: mock, mu: &mu}
	sc := StrategyConfig{ID: "hl-manual-eth-live", Type: "manual", Platform: "hyperliquid", Args: []string{"manual", "ETH", "--mode=live"}}
	rows := []Trade{{Symbol: "ETH", Side: "buy", Quantity: 1, Price: 100}, {Symbol: "ETH", Side: "sell", Quantity: 1, Price: 110, IsClose: true}}
	sendTradeAlertRows(sc, rows, router, nil)
	if len(mock.messages) != 2 {
		t.Fatalf("sent %d alerts for %d rows, want one each", len(mock.messages), len(rows))
	}
	for _, free := range router.lockFree {
		if !free {
			t.Fatal("a drain alert was sent while mu was held")
		}
	}
}

func TestManualCycleWindowBooksEachStopRowOnce(t *testing.T) {
	hlStepNoRecorder(t)
	orig := runHyperliquidUpdateStopLossFunc
	t.Cleanup(func() { runHyperliquidUpdateStopLossFunc = orig })
	runHyperliquidUpdateStopLossFunc = func(_, _, _ string, size, trigger float64, _ int64) (*HyperliquidStopLossUpdateResult, string, error) {
		return &HyperliquidStopLossUpdateResult{StopLossFilledImmediately: true, StopLossTriggerPx: trigger, StopLossSize: size}, "", nil
	}
	sc := ratchetAlertSC(3.0, tier(1.0, 0, 2.0))
	sc.ID = "hl-manual-eth"
	sc.Type = "manual"
	sc.Symbol = "ETH"
	sc.Args = []string{"--mode=live"}
	pos := &Position{Symbol: "ETH", Side: "long", Quantity: 2, InitialQuantity: 2, AvgCost: 100, EntryATR: 10, Multiplier: 1, OwnerStrategyID: sc.ID}
	st := &StrategyState{ID: sc.ID, Type: "manual", Platform: "hyperliquid", Cash: 1000, Positions: map[string]*Position{"ETH": pos}}
	st.TradeHistory = append(st.TradeHistory, Trade{StrategyID: sc.ID, Symbol: "ETH", Side: "sell", IsClose: true, Details: "earlier row"})
	var mu sync.RWMutex
	step := beginHyperliquidStepTradeAlerts(sc, st, &mu)

	trailAt := step.historyLen(&mu)
	fills, d := runManualTrailingStopUpdate(sc, st, map[string]*StrategyState{sc.ID: st}, []StrategyConfig{sc}, nil, nil, nil, 105, false, &mu, nil, silentStrategyLogger(sc.ID))
	if fills != 1 {
		t.Fatalf("manual trailing fills = %d, want 1", fills)
	}
	step.bindWindow(&mu, trailAt, d)

	db := openTestDB(t)
	closePos := &Position{Symbol: "ETH", Side: "long", Quantity: 1, InitialQuantity: 1, AvgCost: 100, Multiplier: 1, OwnerStrategyID: sc.ID}
	st.Positions["ETH"] = closePos
	closeAt := step.historyLen(&mu)
	execResult := &HyperliquidExecuteResult{Execution: &HyperliquidExecution{Action: "sell", Symbol: "ETH", Size: 1, Fill: &HyperliquidFill{AvgPx: 110, TotalSz: 1, OID: 9}}}
	closeTrades, closeDetail, _ := settleManualCycleClose(sc, st, db, closePos, "sell", 1, true, execResult, nil, nil, hlCloseRearmContext{}, &mu, nil, silentStrategyLogger(sc.ID))
	if closeTrades != 0 || closeDetail != "" {
		t.Fatalf("queued close = (%d, %q), want nothing at queue time", closeTrades, closeDetail)
	}
	step.bindWindow(&mu, closeAt)

	mock := &mockNotifier{}
	router := &hlStepRecordingRouter{mock: mock, mu: &mu}
	n, lines := step.finishLines(&mu, router, nil, silentStrategyLogger(sc.ID))
	if n != 1 || len(lines) != 1 || !strings.Contains(lines[0], "LIVE TRAILING SL") {
		t.Fatalf("finishLines = (%d, %v), want only the booked trailing stop row", n, lines)
	}
	if len(mock.messages) != 1 {
		t.Fatalf("sent %d alerts, want 1", len(mock.messages))
	}
	pending, err := db.LoadPendingManualActions()
	if err != nil || len(pending) != 1 {
		t.Fatalf("queued close rows = %d (err %v), want 1 kept for the drain", len(pending), err)
	}
}

func TestManualCaseUsesStepWindowContract(t *testing.T) {
	_, fn := hlStepMainFunc(t)
	manual := hlStepManualClause(t, fn)
	begins := hlStepCalls(manual, "beginHyperliquidStepTradeAlerts")
	syncs := hlStepCalls(manual, "runHyperliquidProtectionSync")
	if len(begins) != 1 || len(syncs) != 1 || begins[0].Pos() > syncs[0].Pos() {
		t.Fatalf("want one step window opened before the protection sync, got %d begins and %d syncs", len(begins), len(syncs))
	}
	if got := hlStepIdentCount(manual, "trades"); got != 0 {
		t.Errorf("the manual case names trades %d times, want 0: rows are counted by the step window", got)
	}
	if got := hlStepIdentCount(manual, "detail"); got != 0 {
		t.Errorf("the manual case names detail %d times, want 0: lines come from the step window", got)
	}
	if got := len(hlStepCalls(manual, "finishLines")); got != 0 {
		t.Errorf("the manual case finishes the window %d times inside the clause, want 0 because break exits early", got)
	}
	if got := len(hlStepCalls(fn, "finishLines")); got != 1 {
		t.Errorf("func main finishes a manual window %d times, want 1 after the switch", got)
	}
}

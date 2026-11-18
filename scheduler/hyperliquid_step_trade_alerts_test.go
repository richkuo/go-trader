package main

import (
	"bytes"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"strings"
	"sync"
	"testing"
	"time"
)

func hlStepMainFunc(t *testing.T) (*token.FileSet, *ast.FuncDecl) {
	t.Helper()
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "main.go", nil, 0)
	if err != nil {
		t.Fatalf("parse main.go: %v", err)
	}
	for _, decl := range file.Decls {
		if fn, ok := decl.(*ast.FuncDecl); ok && fn.Recv == nil && fn.Name.Name == "main" {
			return fset, fn
		}
	}
	t.Fatal("func main not found in main.go")
	return nil, nil
}

func hlStepCallName(call *ast.CallExpr) string {
	switch fn := call.Fun.(type) {
	case *ast.Ident:
		return fn.Name
	case *ast.SelectorExpr:
		return fn.Sel.Name
	}
	return ""
}

func hlStepCalls(node ast.Node, name string) []*ast.CallExpr {
	var out []*ast.CallExpr
	ast.Inspect(node, func(n ast.Node) bool {
		if call, ok := n.(*ast.CallExpr); ok && hlStepCallName(call) == name {
			out = append(out, call)
		}
		return true
	})
	return out
}

func hlStepIdentCount(node ast.Node, name string) int {
	count := 0
	ast.Inspect(node, func(n ast.Node) bool {
		if id, ok := n.(*ast.Ident); ok && id.Name == name {
			count++
		}
		return true
	})
	return count
}

func hlStepManualClause(t *testing.T, fn *ast.FuncDecl) *ast.CaseClause {
	t.Helper()
	var clause *ast.CaseClause
	ast.Inspect(fn, func(n ast.Node) bool {
		cc, ok := n.(*ast.CaseClause)
		if !ok {
			return true
		}
		for _, e := range cc.List {
			if lit, ok := e.(*ast.BasicLit); ok && lit.Value == `"manual"` {
				clause = cc
				return false
			}
		}
		return true
	})
	if clause == nil {
		t.Fatal(`the "manual" strategy case was not found in func main`)
	}
	return clause
}

type hlStepRecordingRouter struct {
	mock     *mockNotifier
	mu       *sync.RWMutex
	lockFree []bool
}

func (r *hlStepRecordingRouter) tradeAlertRoutes(platform, stratType string, isLive bool, source string) []tradeAlertRoute {
	if r.mu != nil {
		free := r.mu.TryLock()
		if free {
			r.mu.Unlock()
		}
		r.lockFree = append(r.lockFree, free)
	}
	return []tradeAlertRoute{{notifier: r.mock, channel: "trade-alerts"}}
}

func hlStepNoRecorder(t *testing.T) {
	t.Helper()
	prev := tradeRecorder
	tradeRecorder = nil
	t.Cleanup(func() { tradeRecorder = prev })
}

func hlStepLivePerps(id string) StrategyConfig {
	return StrategyConfig{ID: id, Type: "perps", Platform: "hyperliquid", Args: []string{"sma", "ETH", "1h", "--mode=live"}, Capital: 1000, Leverage: 1}
}

func hlStepPaperPerps(id string) StrategyConfig {
	return StrategyConfig{ID: id, Type: "perps", Platform: "hyperliquid", Args: []string{"sma", "ETH", "1h"}, Capital: 1000, Leverage: 1}
}

func hlStepState(id string, pos *Position) *StrategyState {
	st := &StrategyState{ID: id, Type: "perps", Platform: "hyperliquid", Cash: 10000, Positions: map[string]*Position{}}
	if pos != nil {
		st.Positions[pos.Symbol] = pos
	}
	return st
}

func hlStepLongETH(qty float64) *Position {
	return &Position{Symbol: "ETH", Side: "long", Quantity: qty, InitialQuantity: qty, AvgCost: 2000, EntryATR: 40, RiskAnchorPrice: 2000, Multiplier: 1}
}

func hlStepZeroSignalExecute(t *testing.T, sc StrategyConfig, st *StrategyState, mu *sync.RWMutex, step *hlStepTradeAlerts, price float64) {
	t.Helper()
	mu.Lock()
	defer mu.Unlock()
	at := step.historyLenLocked()
	execTrades, execDetail, openTrade, _ := executeHyperliquidResultDeferredOpen(sc, st, &HyperliquidResult{Symbol: "ETH", Signal: 0, Price: price}, nil, "HOLD", price, nil, nil, HurstGateDecision{}, silentStrategyLogger(sc.ID))
	if execTrades != 0 || openTrade != nil {
		t.Fatalf("zero-signal execute booked %d trades (open %v), want 0", execTrades, openTrade)
	}
	step.bindExecuteLocked(at, execDetail)
}

func hlStepFinish(t *testing.T, step *hlStepTradeAlerts, mu *sync.RWMutex) (int, string, *mockNotifier, *hlStepRecordingRouter) {
	t.Helper()
	mock := &mockNotifier{}
	router := &hlStepRecordingRouter{mock: mock, mu: mu}
	n, detail := step.finish(mu, router, nil, silentStrategyLogger(step.sc.ID))
	return n, detail, mock, router
}

func hlStepDetailLines(detail string) []string {
	if detail == "" {
		return nil
	}
	return strings.Split(detail, "; ")
}

func TestHLStepAlerts(t *testing.T) {
	t.Run("BookedStopFillsAfterZeroSignalExecute", func(t *testing.T) {
		hlStepNoRecorder(t)
		mult := 1.5
		cases := []struct {
			name   string
			sc     StrategyConfig
			pos    func() *Position
			book   func(t *testing.T, sc StrategyConfig, st *StrategyState, mu *sync.RWMutex, step *hlStepTradeAlerts)
			label  string
			remain float64
		}{
			{
				name: "live trailing stop filled at placement",
				sc:   hlStepLivePerps("hl-trail"),
				pos:  func() *Position { return hlStepLongETH(10) },
				book: func(t *testing.T, sc StrategyConfig, st *StrategyState, mu *sync.RWMutex, step *hlStepTradeAlerts) {
					mu.Lock()
					at := step.historyLenLocked()
					if ok, px := applyTrailingStopUpdateResult(st, "ETH", "long", 0, 0, true, &HyperliquidStopLossUpdateResult{StopLossFilledImmediately: true, StopLossTriggerPx: 1900}, "trailing_stop_loss_immediate", nil, 10); ok {
						step.bindWindowLocked(at, fmt.Sprintf("[hl-trail] LIVE TRAILING SL ETH @ $%.2f", px))
					}
					mu.Unlock()
				},
				label: "LIVE TRAILING SL",
			},
			{
				name: "live fixed ATR stop filled at placement",
				sc:   hlStepLivePerps("hl-fixed"),
				pos:  func() *Position { return hlStepLongETH(10) },
				book: func(t *testing.T, sc StrategyConfig, st *StrategyState, mu *sync.RWMutex, step *hlStepTradeAlerts) {
					mu.Lock()
					at := step.historyLenLocked()
					if recordPerpsStopLossCloseQty(st, "ETH", hlPlacedStopQty(10, 10), 1940, "stop_loss_atr_immediate", nil) {
						step.bindWindowLocked(at, "[hl-fixed] LIVE FIXED ATR SL ETH @ $1940.00")
					}
					mu.Unlock()
				},
				label: "LIVE FIXED ATR SL",
			},
			{
				name: "protection sync stop filled at submit",
				sc: func() StrategyConfig {
					sc := hlStepLivePerps("hl-sync")
					sc.CloseStrategy = &StrategyRef{Name: "tiered_tp_atr_live"}
					sc.StopLossATRMult = &mult
					return sc
				}(),
				pos: func() *Position {
					p := hlStepLongETH(0.4)
					p.AvgCost, p.EntryATR, p.StopLossOID, p.StopLossTriggerPx = 3000, 100, 4242, 2325
					return p
				},
				book: func(t *testing.T, sc StrategyConfig, st *StrategyState, mu *sync.RWMutex, step *hlStepTradeAlerts) {
					withStubbedSyncHyperliquidProtection(t, func(_ StrategyConfig, _ hlProtectionPlan, _ *MultiNotifier, _ *StrategyLogger, _ []byte) (*HyperliquidProtectionSyncResult, bool) {
						return &HyperliquidProtectionSyncResult{CancelStopLossSucceeded: true, StopLossFilledImmediately: true, StopLossTriggerPx: 2318.5}, true
					})
					at := step.historyLen(mu)
					if _, fillPx := runHyperliquidProtectionSync(sc, st, nil, "ETH", mu, nil, silentStrategyLogger(sc.ID), "test", nil, nil, nil, hlProtectionGuardFull, nil); fillPx > 0 {
						step.bindWindow(mu, at, "[hl-sync] LIVE PROTECTION SYNC SL ETH @ $2318.50")
					} else {
						t.Fatal("protection sync did not report the submit fill")
					}
				},
				label: "LIVE PROTECTION SYNC SL",
			},
			{
				name: "post take-profit stop filled for part of the remainder",
				sc:   unifiedSLStrategy("tiered_tp_atr_live_regime_dynamic", "perps", true, "breakeven"),
				pos: func() *Position {
					return &Position{
						Symbol: "ETH", Side: "long", Quantity: 1, InitialQuantity: 2,
						AvgCost: 2000, EntryATR: 40, Regime: "ranging", RegimeAppliedLabel: "ranging",
						StopLossOID: 7, StopLossTriggerPx: 1900,
						TPConsumptions: []TPConsumption{{Label: "ranging", Tier: 0, Stage: tpConsumptionBooked, UpdatedAt: time.Now().UTC()}},
					}
				},
				book: func(t *testing.T, sc StrategyConfig, st *StrategyState, mu *sync.RWMutex, step *hlStepTradeAlerts) {
					orig := runHyperliquidUpdateStopLossFunc
					t.Cleanup(func() { runHyperliquidUpdateStopLossFunc = orig })
					runHyperliquidUpdateStopLossFunc = func(_, _, _ string, _, _ float64, _ int64) (*HyperliquidStopLossUpdateResult, string, error) {
						return &HyperliquidStopLossUpdateResult{StopLossFilledImmediately: true, StopLossTriggerPx: 2000, StopLossSize: 0.4}, "", nil
					}
					at := step.historyLen(mu)
					_, fills, slDetail := runPostTPStopLossAdjustment(sc, st, "ETH", 2100, nil, mu, nil, &StrategyLogger{stratID: sc.ID, writer: &bytes.Buffer{}}, nil, nil, nil)
					if fills != 1 {
						t.Fatalf("post take-profit fills = %d, want 1", fills)
					}
					step.bindWindow(mu, at, slDetail)
				},
				remain: 0.6,
			},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				var mu sync.RWMutex
				st := hlStepState(tc.sc.ID, tc.pos())
				step := beginHyperliquidStepTradeAlerts(tc.sc, st, &mu)
				tc.book(t, tc.sc, st, &mu, step)
				hlStepZeroSignalExecute(t, tc.sc, st, &mu, step, 2000)
				n, detail, mock, _ := hlStepFinish(t, step, &mu)
				if n != 1 {
					t.Fatalf("selected rows = %d, want the one booked stop row counted once after the zero-signal execute", n)
				}
				if len(mock.messages) != 1 || !strings.Contains(mock.messages[0].content, "CLOSED") {
					t.Fatalf("trade alerts = %+v, want one close alert", mock.messages)
				}
				lines := hlStepDetailLines(detail)
				if len(lines) != 1 || (tc.label != "" && !strings.Contains(lines[0], tc.label)) {
					t.Fatalf("summary lines = %q, want one line carrying %q", lines, tc.label)
				}
				pos := st.Positions["ETH"]
				if tc.remain > 0 && (pos == nil || !approxEq(pos.Quantity, tc.remain)) {
					t.Fatalf("remaining position %+v, want quantity %v", pos, tc.remain)
				}
			})
		}
	})
	t.Run("PartialStopFillKeepsResidue", func(t *testing.T) {
		hlStepNoRecorder(t)
		var mu sync.RWMutex
		sc := hlStepLivePerps("hl-partial")
		st := hlStepState(sc.ID, hlStepLongETH(10))
		step := beginHyperliquidStepTradeAlerts(sc, st, &mu)
		mu.Lock()
		at := step.historyLenLocked()
		ok, px := applyTrailingStopUpdateResult(st, "ETH", "long", 0, 0, true, &HyperliquidStopLossUpdateResult{StopLossFilledImmediately: true, StopLossTriggerPx: 1900}, "trailing_stop_loss_immediate", nil, 8)
		if !ok {
			t.Fatal("placement fill was not booked")
		}
		step.bindWindowLocked(at, fmt.Sprintf("[%s] LIVE TRAILING SL ETH @ $%.2f", sc.ID, px))
		mu.Unlock()
		hlStepZeroSignalExecute(t, sc, st, &mu, step, 1950)
		n, detail, mock, _ := hlStepFinish(t, step, &mu)
		if n != 1 || len(mock.messages) != 1 {
			t.Fatalf("rows %d alerts %d, want one partial stop row alerted once", n, len(mock.messages))
		}
		if row := st.TradeHistory[len(st.TradeHistory)-1]; !approxEq(row.Quantity, 8) || !row.IsClose {
			t.Fatalf("booked row %+v, want a close of the placed 8", row)
		}
		if pos := st.Positions["ETH"]; pos == nil || !approxEq(pos.Quantity, 2) {
			t.Fatalf("residue %+v, want 2 left open", pos)
		}
		if !strings.Contains(detail, "LIVE TRAILING SL") {
			t.Fatalf("detail %q", detail)
		}
	})
	t.Run("MultipleRowsOneStep", func(t *testing.T) {
		hlStepNoRecorder(t)
		var mu sync.RWMutex
		sc := hlStepLivePerps("hl-multi")
		st := hlStepState(sc.ID, hlStepLongETH(10))
		step := beginHyperliquidStepTradeAlerts(sc, st, &mu)
		mu.Lock()
		at := step.historyLenLocked()
		if ok, px := applyTrailingStopUpdateResult(st, "ETH", "long", 0, 0, true, &HyperliquidStopLossUpdateResult{StopLossFilledImmediately: true, StopLossTriggerPx: 1900}, "trailing_stop_loss_immediate", nil, 8); ok {
			step.bindWindowLocked(at, fmt.Sprintf("[%s] LIVE TRAILING SL ETH @ $%.2f", sc.ID, px))
		}
		at = step.historyLenLocked()
		if recordPerpsStopLossCloseQty(st, "ETH", 2, 1890, "stop_loss_atr_immediate", nil) {
			step.bindWindowLocked(at, fmt.Sprintf("[%s] LIVE FIXED ATR SL ETH @ $%.2f", sc.ID, 1890.0))
		}
		mu.Unlock()
		hlStepZeroSignalExecute(t, sc, st, &mu, step, 1890)
		n, detail, mock, _ := hlStepFinish(t, step, &mu)
		lines := hlStepDetailLines(detail)
		if n != 2 || len(mock.messages) != 2 || len(lines) != 2 {
			t.Fatalf("rows %d alerts %d lines %q, want two of each", n, len(mock.messages), lines)
		}
		if !strings.Contains(lines[0], "LIVE TRAILING SL") || !strings.Contains(lines[1], "LIVE FIXED ATR SL") {
			t.Fatalf("lines %q, want one line per row in booking order", lines)
		}
	})
	t.Run("HedgeRowsExcluded", func(t *testing.T) {
		hlStepNoRecorder(t)
		hlStepOpen := func(t *testing.T, sc StrategyConfig, st *StrategyState, mu *sync.RWMutex, step *hlStepTradeAlerts, qty float64) string {
			t.Helper()
			execResult := &HyperliquidExecuteResult{Execution: &HyperliquidExecution{Action: "buy", Symbol: "ETH", Size: qty, Fill: &HyperliquidFill{AvgPx: testPrimaryPx, TotalSz: qty, OID: 11}}}
			mu.Lock()
			defer mu.Unlock()
			at := step.historyLenLocked()
			execTrades, execDetail, openTrade, _ := executeHyperliquidResultDeferredOpen(sc, st, &HyperliquidResult{Symbol: "ETH", Signal: 1, Price: testPrimaryPx}, execResult, "BUY", testPrimaryPx, nil, nil, HurstGateDecision{}, silentStrategyLogger(sc.ID))
			if execTrades != 1 || openTrade == nil {
				t.Fatalf("open execute trades %d open %v", execTrades, openTrade)
			}
			recordPositionOpen(st, sc, openTrade, st.Positions["ETH"])
			step.bindExecuteLocked(at, execDetail)
			return execDetail
		}
		hedgeRows := func(st *StrategyState) int {
			n := 0
			for _, tr := range st.TradeHistory {
				if tr.TradeType == hedgeTradeType {
					n++
				}
			}
			return n
		}

		t.Run("primary stop close then hedge close", func(t *testing.T) {
			var mu sync.RWMutex
			sc := hedgeTestConfig()
			sc.Args = []string{"--symbol", "ETH"}
			st := hedgeTestState(sc.ID)
			st.Positions["ETH"] = primaryPos(10, "long")
			st.Positions["BTC"] = hedgePos(0.4, "short", 10)
			step := beginHyperliquidStepTradeAlerts(sc, st, &mu)
			mu.Lock()
			at := step.historyLenLocked()
			if recordPerpsStopLossClose(st, "ETH", 1950, paperStopReasonTrailing, nil) {
				step.bindWindowLocked(at, "[eth-long] PAPER TRAILING SL ETH @ $1950.00")
			}
			mu.Unlock()
			f := &fakeHedgeExec{}
			runHedgeSync(sc, st, &mu, f.executor(), hedgeSyncInputs{PrimaryPx: 1950, HedgePx: testHedgePx, Live: false}, nil, silentStrategyLogger(sc.ID))
			if hedgeRows(st) != 1 {
				t.Fatalf("hedge rows = %d, want the hedge close booked", hedgeRows(st))
			}
			n, detail, mock, _ := hlStepFinish(t, step, &mu)
			if n != 1 || len(mock.messages) != 1 || !strings.Contains(detail, "PAPER TRAILING SL") {
				t.Fatalf("rows %d alerts %d detail %q, want only the primary stop close", n, len(mock.messages), detail)
			}
		})

		t.Run("primary open then hedge open", func(t *testing.T) {
			var mu sync.RWMutex
			sc := hedgeTestConfig()
			sc.Args = []string{"--symbol", "ETH"}
			st := hedgeTestState(sc.ID)
			step := beginHyperliquidStepTradeAlerts(sc, st, &mu)
			execDetail := hlStepOpen(t, sc, st, &mu, step, 10)
			f := &fakeHedgeExec{}
			runHedgeSync(sc, st, &mu, f.executor(), hedgeSyncInputs{PrimaryPx: testPrimaryPx, HedgePx: testHedgePx, FreshExposureQty: 10, Live: false}, nil, silentStrategyLogger(sc.ID))
			if hedgeRows(st) != 1 {
				t.Fatalf("hedge rows = %d, want the hedge open booked", hedgeRows(st))
			}
			n, detail, mock, _ := hlStepFinish(t, step, &mu)
			if n != 1 || len(mock.messages) != 1 || detail != execDetail {
				t.Fatalf("rows %d alerts %d detail %q, want only the primary open %q", n, len(mock.messages), detail, execDetail)
			}
		})

		t.Run("primary open then failed hedge unwind", func(t *testing.T) {
			var mu sync.RWMutex
			sc := hedgeTestConfig()
			st := hedgeTestState(sc.ID)
			step := beginHyperliquidStepTradeAlerts(sc, st, &mu)
			execDetail := hlStepOpen(t, sc, st, &mu, step, 10)
			f := &fakeHedgeExec{openResult: &HyperliquidExecuteResult{Error: "insufficient margin"}, unwindResult: closeFill(testPrimaryPx, 10, 8)}
			runHedgeSync(sc, st, &mu, f.executor(), hedgeSyncInputs{
				PrimaryPx: testPrimaryPx, HedgePx: testHedgePx, FreshExposureQty: 10,
				PrimaryPeers: hedgePrimaryPeers{Known: true, Side: "long"}, Live: true,
			}, nil, silentStrategyLogger(sc.ID))
			if len(f.unwindCalls) != 1 || st.Positions["ETH"] != nil {
				t.Fatalf("unwind calls %v primary %+v, want one full unwind", f.unwindCalls, st.Positions["ETH"])
			}
			n, detail, mock, _ := hlStepFinish(t, step, &mu)
			lines := hlStepDetailLines(detail)
			if n != 2 || len(mock.messages) != 2 || len(lines) != 2 {
				t.Fatalf("rows %d alerts %d lines %q, want the open and the unwind", n, len(mock.messages), lines)
			}
			if lines[0] != execDetail || lines[1] == execDetail || !strings.Contains(lines[1], "hedge open failed") {
				t.Fatalf("lines %q, want the execute line then a separate unwind line", lines)
			}
		})
	})
	t.Run("PaperAndReplayProducers", func(t *testing.T) {
		hlStepNoRecorder(t)

		t.Run("paper breach with signal zero", func(t *testing.T) {
			pf := func(v float64) *float64 { return &v }
			sc := StrategyConfig{ID: "hl-paper", Platform: "hyperliquid", Type: "perps", Args: []string{"sma", "ETH", "1h"}, StopLossPct: pf(3), Direction: DirectionBoth, Leverage: 1, SizingLeverage: 1}
			pos := &Position{Symbol: "ETH", Quantity: 1, AvgCost: 2000, EntryATR: 40, Side: "long", Regime: "ranging"}
			st := paperStopTestState(sc, pos)
			var mu sync.RWMutex
			if breach, _, _ := armPaperStopLossAtOpen(sc, st, "ETH", 2000, silentStrategyLogger(sc.ID)); breach {
				t.Fatal("arm breached at open")
			}
			step := beginHyperliquidStepTradeAlerts(sc, st, &mu)
			at := step.historyLen(&mu)
			mark := pos.StopLossTriggerPx - 5
			n, d := applyPaperStopLossBreach(sc, st, "ETH", "long", mark, &mu, silentStrategyLogger(sc.ID))
			if n != 1 {
				t.Fatalf("breach trades %d", n)
			}
			step.bindWindow(&mu, at, d)
			hlStepZeroSignalExecute(t, sc, st, &mu, step, mark)
			got, detail, mock, _ := hlStepFinish(t, step, &mu)
			if got != 1 || len(mock.messages) != 1 || detail != d {
				t.Fatalf("rows %d alerts %d detail %q, want the breach row with label %q", got, len(mock.messages), detail, d)
			}
		})

		t.Run("paper dynamic regime close", func(t *testing.T) {
			dynamic := &StrategyRef{Name: dynamicCloseStrategyName, Params: unifiedBlock()}
			dynamic.Params["regime_confirm_cycles"] = 2
			sc := StrategyConfig{ID: "hl-dyn", Type: "perps", Platform: "hyperliquid", Args: []string{"sma", "ETH", "1h", "--mode=paper"}, CloseStrategy: dynamic}
			pos := &Position{Symbol: "ETH", Side: "long", Quantity: 1, AvgCost: 2000, EntryATR: 40, Regime: "trending_down", RegimeAppliedLabel: "trending_up", StopLossTriggerPx: 1940}
			st := &StrategyState{ID: sc.ID, Cash: 1000, Regime: "ranging", Positions: map[string]*Position{"ETH": pos}}
			var mu sync.RWMutex
			if n, _ := advancePaperDynamicCloseRegime(sc, st, nil, "ETH", 1950, &mu, nil); n != 0 {
				t.Fatalf("first cycle closed (%d)", n)
			}
			step := beginHyperliquidStepTradeAlerts(sc, st, &mu)
			at := step.historyLen(&mu)
			n, d := advancePaperDynamicCloseRegime(sc, st, nil, "ETH", 1950, &mu, nil)
			if n != 1 {
				t.Fatalf("confirmed flip trades %d, want the crossed stop closed", n)
			}
			step.bindWindow(&mu, at, d)
			hlStepZeroSignalExecute(t, sc, st, &mu, step, 1950)
			got, _, mock, _ := hlStepFinish(t, step, &mu)
			if got != 1 || len(mock.messages) != 1 {
				t.Fatalf("rows %d alerts %d, want the dynamic close alerted once", got, len(mock.messages))
			}
		})

		t.Run("replay rows", func(t *testing.T) {
			sc, st, logger := replayMirrorTestSetup(t, "hl-paper-eth")
			var mu sync.RWMutex
			step := beginHyperliquidStepTradeAlerts(sc, st, &mu)
			now := time.Now().UTC()
			pending := []ReplayDecision{
				{DecisionID: 1, StrategyID: sc.ID, DecisionType: ReplayDecisionOpen, DecidedAt: now, Symbol: "ETH", Side: "long", Quantity: 0.5, ReferencePrice: 1908.25},
				{DecisionID: 2, StrategyID: sc.ID, DecisionType: ReplayDecisionFullClose, DecidedAt: now.Add(time.Second), Symbol: "ETH", Side: "long", Quantity: 0.5, ReferencePrice: 1900.5, CloseReason: "hl_sync_stop_loss"},
			}
			mu.Lock()
			at := step.historyLenLocked()
			_, trades, details, _ := applyReplayedLiveDecisions(sc, st, pending, 1902, replayTestResult(), &Config{}, logger)
			step.bindWindowLocked(at, details...)
			mu.Unlock()
			if trades != 2 || len(details) != 2 {
				t.Fatalf("replay trades %d details %v", trades, details)
			}
			got, detail, mock, _ := hlStepFinish(t, step, &mu)
			lines := hlStepDetailLines(detail)
			if got != 2 || len(mock.messages) != 2 || len(lines) != 2 || lines[0] != details[0] || lines[1] != details[1] {
				t.Fatalf("rows %d alerts %d lines %q, want the two replay rows with their own details %q", got, len(mock.messages), lines, details)
			}
		})

		mult := 1.5
		for _, rc := range []struct {
			name string
			fill hlCloseFillOutcome
			book float64
		}{
			{"failed-close recovery", hlCloseFillOutcome{}, 1},
			{"partial-close recovery", hlCloseFillOutcome{Filled: 0.4, Known: true}, 1},
		} {
			t.Run(rc.name, func(t *testing.T) {
				withStubbedSyncHyperliquidProtection(t, func(_ StrategyConfig, _ hlProtectionPlan, _ *MultiNotifier, _ *StrategyLogger, _ []byte) (*HyperliquidProtectionSyncResult, bool) {
					return &HyperliquidProtectionSyncResult{StopLossFilledImmediately: true, StopLossTriggerPx: 2850}, true
				})
				sc := hlStepLivePerps("hl-rearm")
				sc.CloseStrategy = &StrategyRef{Name: "tiered_tp_atr_live"}
				sc.StopLossATRMult = &mult
				remaining := rc.book
				if rc.fill.Known {
					remaining -= rc.fill.Filled
				}
				pos := hlStepLongETH(remaining)
				pos.AvgCost, pos.EntryATR = 3000, 100
				st := hlStepState(sc.ID, pos)
				var mu sync.RWMutex
				step := beginHyperliquidStepTradeAlerts(sc, st, &mu)
				rearm := hlCloseRearmContext{Price: 2900, Backing: hlCloseBacking{PreSend: hlCloseView{Signed: rc.book, Known: true, Source: hlCloseViewCycleStart}}}
				at := step.historyLen(&mu)
				n, d := rearmAfterSizedClose(sc, st, nil, "ETH", "long", rc.book, rc.fill, rearm, &mu, nil, silentStrategyLogger(sc.ID))
				if n != 1 {
					t.Fatalf("re-arm trades %d detail %q, want the submit fill booked", n, d)
				}
				step.bindWindow(&mu, at, d)
				got, detail, mock, _ := hlStepFinish(t, step, &mu)
				if got != 1 || len(mock.messages) != 1 || detail != d {
					t.Fatalf("rows %d alerts %d detail %q, want the re-armed stop fill with %q", got, len(mock.messages), detail, d)
				}
			})
		}
	})
	t.Run("OpenThenImmediateStopInsideExecute", func(t *testing.T) {
		hlStepNoRecorder(t)
		var mu sync.RWMutex
		sc := hlStepLivePerps("hl-open-stop")
		st := hlStepState(sc.ID, nil)
		step := beginHyperliquidStepTradeAlerts(sc, st, &mu)
		execResult := &HyperliquidExecuteResult{
			Execution:                 &HyperliquidExecution{Action: "buy", Symbol: "ETH", Size: 0.1, Fill: &HyperliquidFill{AvgPx: 3200, TotalSz: 0.1, OID: 1, StopLossTriggerPx: 3104}},
			StopLossFilledImmediately: true,
		}
		mu.Lock()
		at := step.historyLenLocked()
		execTrades, execDetail, openTrade, _ := executeHyperliquidResultDeferredOpen(sc, st, &HyperliquidResult{Symbol: "ETH", Signal: 1, Price: 3200}, execResult, "BUY", 3200, nil, nil, HurstGateDecision{}, silentStrategyLogger(sc.ID))
		if openTrade != nil {
			recordPositionOpen(st, sc, openTrade, st.Positions["ETH"])
		}
		step.bindExecuteLocked(at, execDetail)
		mu.Unlock()
		if execTrades != 2 {
			t.Fatalf("execute trades %d, want open plus immediate stop", execTrades)
		}
		n, detail, mock, _ := hlStepFinish(t, step, &mu)
		lines := hlStepDetailLines(detail)
		if n != 2 || len(mock.messages) != 2 || len(lines) != 2 {
			t.Fatalf("rows %d alerts %d lines %q, want two rows", n, len(mock.messages), lines)
		}
		if lines[0] != execDetail || lines[1] == execDetail || strings.Count(detail, execDetail) != 1 {
			t.Fatalf("lines %q, want the execute line once on the open and a separate stop line", lines)
		}
		if !strings.Contains(lines[1], "SELL") {
			t.Fatalf("stop line %q, want the row-derived close side", lines[1])
		}
	})
	t.Run("IsolationAndInvalidBaseline", func(t *testing.T) {
		hlStepNoRecorder(t)
		var mu sync.RWMutex
		sc := hlStepLivePerps("hl-a")
		a := hlStepState("hl-a", hlStepLongETH(1))
		other := hlStepState("hl-b", hlStepLongETH(1))
		paperOther := hlStepState("hl-paper-b", hlStepLongETH(1))
		step := beginHyperliquidStepTradeAlerts(sc, a, &mu)
		mu.Lock()
		recordPerpsStopLossClose(other, "ETH", 1900, "stop_loss_atr_immediate", nil)
		recordPerpsStopLossClose(paperOther, "ETH", 1900, paperStopReasonTrailing, nil)
		recordPerpsStopLossClose(a, "ETH", 1900, "stop_loss_atr_immediate", nil)
		mu.Unlock()
		n, _, mock, _ := hlStepFinish(t, step, &mu)
		if n != 1 || len(mock.messages) != 1 {
			t.Fatalf("rows %d alerts %d, want only this strategy's row", n, len(mock.messages))
		}

		var buf bytes.Buffer
		bad := &hlStepTradeAlerts{sc: sc, ss: a, baseline: len(a.TradeHistory) + 3, labels: map[int]string{}}
		mock2 := &mockNotifier{}
		got, detail := bad.finish(&mu, &hlStepRecordingRouter{mock: mock2}, nil, &StrategyLogger{stratID: sc.ID, writer: &buf})
		if got != 0 || detail != "" || len(mock2.messages) != 0 {
			t.Fatalf("invalid baseline selected %d rows (%q) and sent %d alerts, want none", got, detail, len(mock2.messages))
		}
		if !strings.Contains(buf.String(), "invariant failure") {
			t.Fatalf("log %q, want the invariant failure", buf.String())
		}
	})
	t.Run("SendsOutsideLock", func(t *testing.T) {
		hlStepNoRecorder(t)
		var mu sync.RWMutex
		sc := hlStepLivePerps("hl-lock")
		st := hlStepState(sc.ID, hlStepLongETH(1))
		step := beginHyperliquidStepTradeAlerts(sc, st, &mu)
		mu.Lock()
		recordPerpsStopLossClose(st, "ETH", 1900, "stop_loss_atr_immediate", nil)
		mu.Unlock()
		_, _, _, router := hlStepFinish(t, step, &mu)
		if len(router.lockFree) == 0 {
			t.Fatal("no alert route was resolved")
		}
		for _, free := range router.lockFree {
			if !free {
				t.Fatal("trade alerts were sent while mu was held")
			}
		}
	})
}

func TestManualTrailingStopFillIsCountedAndMerged(t *testing.T) {
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
	strategies := map[string]*StrategyState{sc.ID: st}
	var mu sync.RWMutex
	before := len(st.TradeHistory)
	fills, d, _ := runManualTrailingStopUpdate(sc, st, strategies, []StrategyConfig{sc}, nil, nil, nil, 105, false, &mu, nil, silentStrategyLogger(sc.ID))
	if fills != 1 || !strings.Contains(d, "LIVE TRAILING SL") {
		t.Fatalf("manual trailing fill = (%d, %q), want one counted fill", fills, d)
	}
	if len(st.TradeHistory) != before+1 || !st.TradeHistory[before].IsClose {
		t.Fatalf("history %+v, want the manual stop fill booked as one close row", st.TradeHistory)
	}
	prior := "[hl-manual-eth] LIVE PROTECTION SYNC SL ETH @ $95.00"
	merged := mergeTradeDetails(prior, d)
	if !strings.Contains(merged, prior) || !strings.Contains(merged, d) {
		t.Fatalf("merged detail %q lost a booked-stop line", merged)
	}

	db := openTestDB(t)
	closePos := &Position{Symbol: "ETH", Side: "long", Quantity: 1, InitialQuantity: 1, AvgCost: 100, Multiplier: 1, OwnerStrategyID: sc.ID}
	st.Positions["ETH"] = closePos
	historyBefore := len(st.TradeHistory)
	execResult := &HyperliquidExecuteResult{Execution: &HyperliquidExecution{Action: "sell", Symbol: "ETH", Size: 1, Fill: &HyperliquidFill{AvgPx: 110, TotalSz: 1, OID: 9}}}
	closeTrades, closeDetail, _ := settleManualCycleClose(sc, st, db, closePos, "sell", 1, true, execResult, nil, nil, hlCloseRearmContext{}, &mu, nil, silentStrategyLogger(sc.ID))
	if closeTrades != 0 || closeDetail != "" {
		t.Fatalf("queued close = (%d, %q), want no count or line at queue time", closeTrades, closeDetail)
	}
	if len(st.TradeHistory) != historyBefore {
		t.Fatalf("queued close appended %d history rows; it books on the next drain, which is the separate queued-close defect", len(st.TradeHistory)-historyBefore)
	}
}

package main

import (
	"bytes"
	"fmt"
	"strings"
	"sync"
	"testing"
)

// hedgeSyncTestRig stubs the exec + mark seams and returns restore funcs.
type hedgeSyncSeams struct {
	execCalls   int
	execErr     error
	execFill    *HyperliquidFill
	closeCalls  int
	closeFill   *HyperliquidCloseFill
	unwindCalls int
	unwindQty   float64
	unwindOIDs  []int64
	unwindErr   error
	unwindFill  *HyperliquidCloseFill
}

func (seams *hedgeSyncSeams) install(t *testing.T, mark float64) {
	t.Helper()
	prevMark := hedgeMarkForSyncFn
	prevExec := runHyperliquidHedgeExecuteFn
	prevClose := runHyperliquidHedgeCloseFn
	prevUnwind := runHyperliquidUnwindCloseFn
	t.Cleanup(func() {
		hedgeMarkForSyncFn = prevMark
		runHyperliquidHedgeExecuteFn = prevExec
		runHyperliquidHedgeCloseFn = prevClose
		runHyperliquidUnwindCloseFn = prevUnwind
	})
	hedgeMarkForSyncFn = func(sc StrategyConfig, prices map[string]float64) float64 { return mark }
	runHyperliquidHedgeExecuteFn = func(script, symbol, side string, size, stopLossPct float64, cancelStopLossOID int64, prevPosQty float64, marginMode string, leverage float64, closeFullPosition bool, snapshot hlExecuteSnapshot, extraCancelOIDs ...int64) (*HyperliquidExecuteResult, string, error) {
		seams.execCalls++
		if seams.execErr != nil {
			return nil, "", seams.execErr
		}
		fill := seams.execFill
		if fill == nil {
			fill = &HyperliquidFill{AvgPx: 90000, TotalSz: size, OID: 1, Fee: 1}
		}
		return &HyperliquidExecuteResult{Execution: &HyperliquidExecution{Fill: fill}}, "", nil
	}
	runHyperliquidHedgeCloseFn = func(script, symbol string, partialSz *float64, cancelStopLossOIDs []int64) (*HyperliquidCloseResult, string, error) {
		seams.closeCalls++
		fill := seams.closeFill
		if fill == nil {
			fill = &HyperliquidCloseFill{AvgPx: 90000, TotalSz: *partialSz, OID: 2, Fee: 0.5}
		}
		return &HyperliquidCloseResult{Close: &HyperliquidClose{Fill: fill}}, "", nil
	}
	runHyperliquidUnwindCloseFn = func(script, symbol string, partialSz *float64, cancelStopLossOIDs []int64) (*HyperliquidCloseResult, string, error) {
		seams.unwindCalls++
		seams.unwindQty = *partialSz
		seams.unwindOIDs = cancelStopLossOIDs
		if seams.unwindErr != nil {
			return nil, "", seams.unwindErr
		}
		fill := seams.unwindFill
		if fill == nil {
			fill = &HyperliquidCloseFill{AvgPx: 3000, TotalSz: *partialSz, OID: 3, Fee: 1}
		}
		return &HyperliquidCloseResult{Close: &HyperliquidClose{Fill: fill}}, "", nil
	}
}

func TestRunHedgeSync_PaperOpensHedgeOnManageCycle(t *testing.T) {
	sc := hlHedgeTestStrategy("hl-eth", &HedgeConfig{Enabled: true, Symbol: "BTC"})
	s := hedgeTestStrategyState()
	delete(s.Positions, "BTC")
	seams := &hedgeSyncSeams{}
	seams.install(t, 90000)

	var mu sync.RWMutex
	// live=false (paper), freshOpenQty=0 — a plain Signal==0 manage cycle.
	runHedgeSync(sc, s, 3000, nil, false, 0, &mu, nil, nil)

	if seams.execCalls != 0 {
		t.Errorf("paper path must not spawn the live exec, got %d calls", seams.execCalls)
	}
	pos := s.Positions["BTC"]
	if pos == nil || pos.HedgeFor != "ETH" || pos.HedgePrimaryQtyBasis != 1.5 {
		t.Fatalf("paper hedge leg = %+v", pos)
	}
	if pos.Side != "short" {
		t.Errorf("paper hedge side = %s, want short (inverse of long primary)", pos.Side)
	}
}

func TestRunHedgeSync_NotGatedByPause(t *testing.T) {
	// Paused (#1150) holds entry SIGNALS upstream; hedge sync is a coupled
	// risk leg and must still converge (here: open the missing hedge).
	sc := hlHedgeTestStrategy("hl-eth", &HedgeConfig{Enabled: true, Symbol: "BTC"})
	sc.Paused = true
	s := hedgeTestStrategyState()
	delete(s.Positions, "BTC")
	seams := &hedgeSyncSeams{}
	seams.install(t, 90000)

	var mu sync.RWMutex
	runHedgeSync(sc, s, 3000, nil, false, 0, &mu, nil, nil)
	if s.Positions["BTC"] == nil {
		t.Error("paused strategy must still open its hedge leg — hedge orders are not signals")
	}
}

func TestRunHedgeSync_DisabledNoop(t *testing.T) {
	sc := hlHedgeTestStrategy("hl-eth", nil)
	s := hedgeTestStrategyState()
	seams := &hedgeSyncSeams{}
	seams.install(t, 90000)
	var mu sync.RWMutex
	runHedgeSync(sc, s, 3000, nil, true, 0, &mu, nil, nil)
	if seams.execCalls != 0 || seams.closeCalls != 0 || seams.unwindCalls != 0 {
		t.Errorf("no hedge block must be a no-op, got exec=%d close=%d unwind=%d", seams.execCalls, seams.closeCalls, seams.unwindCalls)
	}
}

func TestRunHedgeSync_FailedPrimaryOpenSpawnsNothing(t *testing.T) {
	// Plan phase-E item 4: a failed primary open leaves both legs flat →
	// decision none → no hedge order is ever placed.
	sc := hlHedgeTestStrategy("hl-eth", &HedgeConfig{Enabled: true, Symbol: "BTC"})
	s := hedgeTestStrategyState()
	delete(s.Positions, "ETH")
	delete(s.Positions, "BTC")
	seams := &hedgeSyncSeams{}
	seams.install(t, 90000)
	var mu sync.RWMutex
	runHedgeSync(sc, s, 3000, nil, true, 0, &mu, nil, nil)
	if seams.execCalls != 0 || seams.closeCalls != 0 || seams.unwindCalls != 0 {
		t.Errorf("flat primary + flat hedge must spawn nothing, got exec=%d close=%d unwind=%d", seams.execCalls, seams.closeCalls, seams.unwindCalls)
	}
}

func TestRunHedgeSync_FreshOpenHedgeFailureUnwindsPrimary(t *testing.T) {
	sc := hlHedgeTestStrategy("hl-eth", &HedgeConfig{Enabled: true, Symbol: "BTC"})
	s := hedgeTestStrategyState()
	delete(s.Positions, "BTC")
	// The just-opened primary carries its freshly-armed protection OIDs —
	// the unwind must cancel them.
	s.Positions["ETH"].StopLossOID = 111
	s.Positions["ETH"].TPOIDs = []int64{222}

	seams := &hedgeSyncSeams{execErr: fmt.Errorf("insufficient margin")}
	seams.install(t, 90000)

	var mu sync.RWMutex
	// live + freshOpenQty=1.5: this cycle opened the primary; hedge open failed.
	runHedgeSync(sc, s, 3000, nil, true, 1.5, &mu, nil, nil)

	if seams.execCalls != 1 {
		t.Errorf("hedge open attempts = %d, want 1", seams.execCalls)
	}
	if seams.unwindCalls != 1 {
		t.Fatalf("fresh-open hedge failure must unwind the primary, unwind calls = %d", seams.unwindCalls)
	}
	if seams.unwindQty != 1.5 {
		t.Errorf("unwind qty = %g, want the primary fill 1.5", seams.unwindQty)
	}
	if len(seams.unwindOIDs) != 2 || seams.unwindOIDs[0] != 111 || seams.unwindOIDs[1] != 222 {
		t.Errorf("unwind cancel OIDs = %v, want [111 222]", seams.unwindOIDs)
	}
	if _, ok := s.Positions["ETH"]; ok {
		t.Error("primary must be flat after the fail-closed unwind")
	}
	tr := s.TradeHistory[len(s.TradeHistory)-1]
	if !strings.Contains(tr.Details, "primary unwind") {
		t.Errorf("unwind trade = %+v", tr)
	}
}

func TestRunHedgeSync_ManageCycleHedgeFailureAlertsAndRetries(t *testing.T) {
	sc := hlHedgeTestStrategy("hl-eth", &HedgeConfig{Enabled: true, Symbol: "BTC"})
	s := hedgeTestStrategyState()
	delete(s.Positions, "BTC")
	seams := &hedgeSyncSeams{execErr: fmt.Errorf("venue down")}
	seams.install(t, 90000)

	var mu sync.RWMutex
	// freshOpenQty=0 → manage cycle: alert + retry, NEVER unwind.
	runHedgeSync(sc, s, 3000, nil, true, 0, &mu, nil, nil)
	if seams.execCalls != 1 {
		t.Errorf("hedge open attempts = %d, want 1", seams.execCalls)
	}
	if seams.unwindCalls != 0 {
		t.Errorf("manage-cycle hedge failure must not unwind the primary, unwind calls = %d", seams.unwindCalls)
	}
	if s.Positions["ETH"] == nil || s.Positions["ETH"].Quantity != 1.5 {
		t.Error("primary must be untouched on a manage-cycle hedge failure")
	}
	if s.Positions["BTC"] != nil {
		t.Error("failed hedge order must not mutate hedge state")
	}
}

func TestRunHedgeSync_ScaleInAddHedgeFailureUnwindsAddLegOnly(t *testing.T) {
	sc := hlHedgeTestStrategy("hl-eth", &HedgeConfig{Enabled: true, Symbol: "BTC"})
	s := hedgeTestStrategyState()
	// Scale-in grew the primary 1.5 → 2.0 this cycle (add fill 0.5); the
	// hedge add fails → unwind ONLY the add leg, not the whole position.
	s.Positions["ETH"].Quantity = 2.0
	s.Positions["ETH"].InitialQuantity = 2.0

	seams := &hedgeSyncSeams{execErr: fmt.Errorf("margin exhausted")}
	seams.install(t, 90000)

	var mu sync.RWMutex
	runHedgeSync(sc, s, 3000, nil, true, 0.5, &mu, nil, nil)

	if seams.unwindCalls != 1 || seams.unwindQty != 0.5 {
		t.Fatalf("unwind = %d calls qty %g, want 1 call of the 0.5 add leg", seams.unwindCalls, seams.unwindQty)
	}
	if s.Positions["ETH"] == nil || s.Positions["ETH"].Quantity != 1.5 {
		t.Errorf("primary = %+v, want reduced back to 1.5", s.Positions["ETH"])
	}
}

func TestRunHedgeSync_UnwindFailureLeavesStateAndAlerts(t *testing.T) {
	sc := hlHedgeTestStrategy("hl-eth", &HedgeConfig{Enabled: true, Symbol: "BTC"})
	s := hedgeTestStrategyState()
	delete(s.Positions, "BTC")
	seams := &hedgeSyncSeams{
		execErr:   fmt.Errorf("hedge venue down"),
		unwindErr: fmt.Errorf("primary venue down"),
	}
	seams.install(t, 90000)

	var buf bytes.Buffer
	var mu sync.RWMutex
	runHedgeSync(sc, s, 3000, nil, true, 1.5, &mu, nil, hedgeTestLogger(&buf))

	if seams.unwindCalls != 1 {
		t.Fatalf("unwind attempts = %d, want 1", seams.unwindCalls)
	}
	if s.Positions["ETH"] == nil || s.Positions["ETH"].Quantity != 1.5 {
		t.Error("failed unwind must leave the primary position unchanged")
	}
	if !strings.Contains(buf.String(), "left open and unhedged") {
		t.Errorf("want degraded-loop error log, got: %s", buf.String())
	}
}

func TestRunHedgeSync_ReducesHedgeOnManageCycle(t *testing.T) {
	sc := hlHedgeTestStrategy("hl-eth", &HedgeConfig{Enabled: true, Symbol: "BTC"})
	s := hedgeTestStrategyState()
	s.Positions["ETH"].Quantity = 0.75 // primary halved by an evaluator close
	seams := &hedgeSyncSeams{}
	seams.install(t, 90000)

	var mu sync.RWMutex
	runHedgeSync(sc, s, 3000, nil, true, 0, &mu, nil, nil)

	if seams.closeCalls != 1 {
		t.Fatalf("hedge reduce calls = %d, want 1", seams.closeCalls)
	}
	pos := s.Positions["BTC"]
	if pos == nil || pos.Quantity != 0.025 {
		t.Errorf("hedge leg = %+v, want reduced to 0.025", pos)
	}
	if seams.unwindCalls != 0 {
		t.Errorf("a converging reduce must never unwind the primary")
	}
}

func TestRunHedgeSync_FlattensOrphanHedgeWhenPrimaryFlat(t *testing.T) {
	sc := hlHedgeTestStrategy("hl-eth", &HedgeConfig{Enabled: true, Symbol: "BTC"})
	s := hedgeTestStrategyState()
	delete(s.Positions, "ETH") // primary closed on-chain (SL fill) — reconcile booked it
	seams := &hedgeSyncSeams{}
	seams.install(t, 90000)

	var mu sync.RWMutex
	runHedgeSync(sc, s, 3000, nil, true, 0, &mu, nil, nil)

	if seams.closeCalls != 1 {
		t.Fatalf("orphan hedge flatten calls = %d, want 1", seams.closeCalls)
	}
	if _, ok := s.Positions["BTC"]; ok {
		t.Error("orphan hedge leg must be flattened when the primary is flat")
	}
}

// TestHedgeLegNeverStampedByPrimaryDispatchHelpers pins phase-E item 6: the
// fresh-open stamp/queue helpers are keyed on result.Symbol (the PRIMARY), so
// a hedge leg sitting in the same Positions map never picks up entry ATR,
// regime, or an LLM analysis request.
func TestHedgeLegNeverStampedByPrimaryDispatchHelpers(t *testing.T) {
	sc := hlHedgeTestStrategy("hl-eth", &HedgeConfig{Enabled: true, Symbol: "BTC"})
	sc.LLMEntryAnalysis = &LLMEntryAnalysisConfig{Enabled: true}
	s := hedgeTestStrategyState()
	hedge := s.Positions["BTC"]

	var enqueued []llmEntryAnalysisJob
	prevEnqueue := llmEntryAnalysisEnqueue
	t.Cleanup(func() { llmEntryAnalysisEnqueue = prevEnqueue })
	llmEntryAnalysisEnqueue = func(job llmEntryAnalysisJob) bool {
		enqueued = append(enqueued, job)
		return true
	}

	// The dispatch applies these for result.Symbol = "ETH" (primary) after a
	// fresh open.
	openTrade := &Trade{Symbol: "ETH", Side: "buy"}
	stampEntryATRIfOpened(s, "ETH", map[string]interface{}{"atr": 25.0})
	stampPositionRegimeIfOpened(s, "ETH", RegimePayload{Legacy: "trending_up"}, sc, nil)
	queueLLMEntryAnalysisIfOpened(sc, s, "ETH", 1, openTrade, map[string]interface{}{"atr": 25.0})

	if hedge.EntryATR != 0 {
		t.Errorf("hedge leg picked up entry ATR %g — stamp must key on the primary symbol", hedge.EntryATR)
	}
	if hedge.Regime != "" {
		t.Errorf("hedge leg picked up regime %q", hedge.Regime)
	}
	if hedge.LLMAnalysisRequested {
		t.Error("hedge leg picked up an LLM analysis request")
	}
	for _, job := range enqueued {
		if job.Symbol == "BTC" {
			t.Errorf("LLM analysis enqueued for the hedge coin: %+v", job)
		}
	}
	// Controls: the PRIMARY leg was stamped.
	if s.Positions["ETH"].EntryATR != 25.0 {
		t.Errorf("primary entry ATR = %g, want 25", s.Positions["ETH"].EntryATR)
	}
	if !s.Positions["ETH"].LLMAnalysisRequested {
		t.Error("primary LLM analysis request missing (control)")
	}
}

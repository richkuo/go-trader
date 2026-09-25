package main

import (
	"sync"
	"testing"
)

func liqWalkerStrategy() StrategyConfig {
	trail := 3.0
	minMove := 0.5
	return StrategyConfig{
		ID: "hl-eth", Type: "perps", Platform: "hyperliquid", Script: "x.py",
		Args:                   []string{"x.py", "ETH", "1h", "--mode=live"},
		TrailingStopPct:        &trail,
		TrailingStopMinMovePct: &minMove,
	}
}

func TestTrailingWalkerClampsCandidateInsideLiquidation(t *testing.T) {
	old := runHyperliquidUpdateStopLossFunc
	defer func() { runHyperliquidUpdateStopLossFunc = old }()
	clearHLLiquidationAlert("hl-eth", "ETH")
	defer clearHLLiquidationAlert("hl-eth", "ETH")

	sc := liqWalkerStrategy()
	var gotTrigger float64
	calls := 0
	runHyperliquidUpdateStopLossFunc = func(script, symbol, side string, size, triggerPx float64, cancelStopLossOID int64) (*HyperliquidStopLossUpdateResult, string, error) {
		calls++
		gotTrigger = triggerPx
		return &HyperliquidStopLossUpdateResult{StopLossOID: 7001, StopLossTriggerPx: triggerPx}, "", nil
	}
	pos := &Position{AvgCost: 2400, RiskAnchorPrice: 2400}
	_, _, ok := runHyperliquidTrailingStopUpdate(sc, "ETH", "long", 1.0, pos, 2400, 2400, 2320, 4242,
		trailingReplacePolicy{liquidationPx: 2340.5}, nil, newTestLogger(t))
	if !ok {
		t.Fatal("walker must confirm the clamped replacement")
	}
	if calls != 1 {
		t.Fatalf("placement calls = %d, want 1", calls)
	}
	want := 2340.5 * (1 + hlLiquidationStopBufferPct/100.0)
	if !approxEqLiq(gotTrigger, want) {
		t.Errorf("placed trigger = %g, want %g (just inside liquidation)", gotTrigger, want)
	}
}

func TestTrailingWalkerHealsRestingStopPastLiquidation(t *testing.T) {
	old := runHyperliquidUpdateStopLossFunc
	defer func() { runHyperliquidUpdateStopLossFunc = old }()
	clearHLLiquidationAlert("hl-eth", "ETH")
	defer clearHLLiquidationAlert("hl-eth", "ETH")

	sc := liqWalkerStrategy()
	var gotTrigger float64
	var gotCancelOID int64
	calls := 0
	runHyperliquidUpdateStopLossFunc = func(script, symbol, side string, size, triggerPx float64, cancelStopLossOID int64) (*HyperliquidStopLossUpdateResult, string, error) {
		calls++
		gotTrigger, gotCancelOID = triggerPx, cancelStopLossOID
		return &HyperliquidStopLossUpdateResult{StopLossOID: 7002, StopLossTriggerPx: triggerPx}, "", nil
	}
	pos := &Position{AvgCost: 2400, RiskAnchorPrice: 2400}
	_, _, ok := runHyperliquidTrailingStopUpdate(sc, "ETH", "long", 1.0, pos, 2400, 2400, 2330, 4242,
		trailingReplacePolicy{liquidationPx: 2340.5}, nil, newTestLogger(t))
	if !ok {
		t.Fatal("walker must confirm the heal")
	}
	if calls != 1 {
		t.Fatalf("placement calls = %d, want 1 (heal an unreachable resting stop)", calls)
	}
	if gotCancelOID != 4242 {
		t.Errorf("cancel OID = %d, want 4242", gotCancelOID)
	}
	want := 2340.5 * (1 + hlLiquidationStopBufferPct/100.0)
	if !approxEqLiq(gotTrigger, want) {
		t.Errorf("healed trigger = %g, want %g", gotTrigger, want)
	}
}

func TestTrailingWalkerClampNeverWidens(t *testing.T) {
	old := runHyperliquidUpdateStopLossFunc
	defer func() { runHyperliquidUpdateStopLossFunc = old }()

	sc := liqWalkerStrategy()
	calls := 0
	runHyperliquidUpdateStopLossFunc = func(script, symbol, side string, size, triggerPx float64, cancelStopLossOID int64) (*HyperliquidStopLossUpdateResult, string, error) {
		calls++
		return &HyperliquidStopLossUpdateResult{StopLossOID: 7004, StopLossTriggerPx: triggerPx}, "", nil
	}
	pos := &Position{AvgCost: 2400, RiskAnchorPrice: 2400}
	_, _, ok := runHyperliquidTrailingStopUpdate(sc, "ETH", "long", 1.0, pos, 2400, 2400, 2330, 4242,
		trailingReplacePolicy{liquidationPx: 1500}, nil, newTestLogger(t))
	if !ok {
		t.Fatal("walker must report ok")
	}
	if calls != 0 {
		t.Fatalf("placement calls = %d, want 0 — a reachable stop must never be re-placed (and never widened)", calls)
	}
}

func TestProtectionPlanClampsSLMultPastLiquidation(t *testing.T) {
	mult := 2.5
	sc := StrategyConfig{
		ID: "hl-eth", Type: "perps", Platform: "hyperliquid", Script: "x.py",
		Args:            []string{"x.py", "ETH", "1h", "--mode=live"},
		StopLossATRMult: &mult,
	}
	pos := &Position{
		Symbol: "ETH", Side: "long", Quantity: 1.0,
		AvgCost: 2400, RiskAnchorPrice: 2400, EntryATR: 30,
		StopLossOID: 4242, StopLossTriggerPx: 2325,
	}

	basePlan, ok := buildHyperliquidProtectionPlan(sc, pos, 0)
	if !ok {
		t.Fatal("expected a plan")
	}
	if !approxEqLiq(basePlan.StopLossATRMult, 2.5) {
		t.Fatalf("baseline mult = %g, want the configured 2.5", basePlan.StopLossATRMult)
	}
	if basePlan.ForceSLReplace {
		t.Error("an unknown liquidation price must not force a replace")
	}

	plan, ok := buildHyperliquidProtectionPlan(sc, pos, 2340.5)
	if !ok {
		t.Fatal("expected a plan")
	}
	if plan.StopLossATRMult >= 2.5 {
		t.Errorf("clamped mult = %g, want strictly below the configured 2.5", plan.StopLossATRMult)
	}
	wantTrigger := 2340.5 * (1 + hlLiquidationStopBufferPct/100.0)
	if got := plan.AvgCost - plan.StopLossATRMult*plan.EntryATR; !approxEqLiq(got, wantTrigger) {
		t.Errorf("plan derives trigger %g, want %g", got, wantTrigger)
	}
	if !plan.ForceSLReplace {
		t.Error("a clamped SL must force the cancel+replace, or the unreachable order keeps resting")
	}
}

func TestProtectionPlanForcesReplaceForRestingStopPastLiquidation(t *testing.T) {
	mult := 0.5
	sc := StrategyConfig{
		ID: "hl-eth", Type: "perps", Platform: "hyperliquid", Script: "x.py",
		Args:            []string{"x.py", "ETH", "1h", "--mode=live"},
		StopLossATRMult: &mult,
	}
	pos := &Position{
		Symbol: "ETH", Side: "long", Quantity: 1.0,
		AvgCost: 2400, RiskAnchorPrice: 2400, EntryATR: 30,
		StopLossOID: 4242, StopLossTriggerPx: 2325,
	}
	plan, ok := buildHyperliquidProtectionPlan(sc, pos, 2340.5)
	if !ok {
		t.Fatal("expected a plan")
	}
	if !approxEqLiq(plan.StopLossATRMult, 0.5) {
		t.Errorf("mult = %g, want the configured 0.5 (already reachable)", plan.StopLossATRMult)
	}
	if !plan.ForceSLReplace {
		t.Fatal("a resting trigger past liquidation must force a replace even when the resolved multiple is fine")
	}
}

func TestProtectionPlanRefusesFarSideLongRewrite(t *testing.T) {
	mult := 2.5
	sc := StrategyConfig{
		ID: "hl-eth", Type: "perps", Platform: "hyperliquid", Script: "x.py",
		Args:            []string{"x.py", "ETH", "1h", "--mode=live"},
		StopLossATRMult: &mult,
		MarginMode:      "cross",
	}
	pos := &Position{
		Symbol: "ETH", Side: "long", Quantity: 1.0,
		AvgCost: 2400, RiskAnchorPrice: 2400, EntryATR: 30,
		StopLossOID: 4242, StopLossTriggerPx: 2325,
	}
	const liqPx = 2400.0

	newMult, clamped := hlClampProtectionSLMult("long", 2400, 30, 2.5, liqPx)
	if clamped {
		t.Fatalf("far-side long clamp must be refused, got mult %g", newMult)
	}
	if !approxEqLiq(newMult, 2.5) {
		t.Errorf("a refused rewrite must return the configured multiple, got %g", newMult)
	}

	plan, ok := buildHyperliquidProtectionPlan(sc, pos, liqPx)
	if !ok {
		t.Fatal("expected a plan")
	}
	if !approxEqLiq(plan.StopLossATRMult, 2.5) {
		t.Errorf("plan mult = %g, want the configured 2.5 — no mirrored rewrite", plan.StopLossATRMult)
	}
	if plan.ForceSLReplace {
		t.Fatal("an unclampable geometry must NOT force a replace — that is the unbounded per-cycle cancel+replace loop")
	}
}

func TestProtectionPlanClampConvergesAfterOneReplace(t *testing.T) {
	mult := 2.5
	sc := StrategyConfig{
		ID: "hl-eth", Type: "perps", Platform: "hyperliquid", Script: "x.py",
		Args:            []string{"x.py", "ETH", "1h", "--mode=live"},
		StopLossATRMult: &mult,
	}
	const liqPx = 2340.5
	wantTrigger := liqPx * (1 + hlLiquidationStopBufferPct/100.0)

	pos := &Position{
		Symbol: "ETH", Side: "long", Quantity: 1.0,
		AvgCost: 2400, RiskAnchorPrice: 2400, EntryATR: 30,
		StopLossOID: 4242, StopLossTriggerPx: 2325,
	}
	plan, ok := buildHyperliquidProtectionPlan(sc, pos, liqPx)
	if !ok {
		t.Fatal("expected a plan")
	}
	if !plan.ForceSLReplace {
		t.Fatal("cycle 1 must force the replace: the resting trigger is past liquidation")
	}
	got := plan.AvgCost - plan.StopLossATRMult*plan.EntryATR
	if !approxEqLiq(got, wantTrigger) {
		t.Fatalf("cycle 1 derives %g, want %g", got, wantTrigger)
	}

	pos.StopLossTriggerPx = wantTrigger
	plan2, ok := buildHyperliquidProtectionPlan(sc, pos, liqPx)
	if !ok {
		t.Fatal("expected a plan")
	}
	if plan2.ForceSLReplace {
		t.Fatal("cycle 2 must NOT force another replace — the resting trigger already equals the clamped one")
	}
	if got2 := plan2.AvgCost - plan2.StopLossATRMult*plan2.EntryATR; !approxEqLiq(got2, wantTrigger) {
		t.Errorf("cycle 2 derives %g, want the same clamped %g (no state drift)", got2, wantTrigger)
	}
}

func lastLiqAlertAction(strategyID, symbol string) hlLiquidationAlertAction {
	v, ok := hlLiquidationAlerts.Load(hlLiquidationAlertKey(strategyID, symbol))
	if !ok {
		return ""
	}
	st, ok := v.(hlLiquidationAlertState)
	if !ok {
		return ""
	}
	return st.LastAction
}

func TestTrailingWalkerClampReportsProtectionLostWhenPlacementRejected(t *testing.T) {
	old := runHyperliquidUpdateStopLossFunc
	defer func() { runHyperliquidUpdateStopLossFunc = old }()
	clearHLLiquidationAlert("hl-eth", "ETH")
	defer clearHLLiquidationAlert("hl-eth", "ETH")

	sc := liqWalkerStrategy()
	runHyperliquidUpdateStopLossFunc = func(script, symbol, side string, size, triggerPx float64, cancelStopLossOID int64) (*HyperliquidStopLossUpdateResult, string, error) {
		return &HyperliquidStopLossUpdateResult{
			CancelStopLossSucceeded: true,
			StopLossError:           "Order would exceed the open order limit",
		}, "", nil
	}
	pos := &Position{AvgCost: 2400, RiskAnchorPrice: 2400}
	_, _, ok := runHyperliquidTrailingStopUpdate(sc, "ETH", "long", 1.0, pos, 2400, 2400, 2330, 4242,
		trailingReplacePolicy{liquidationPx: 2340.5}, nil, newTestLogger(t))
	if !ok {
		t.Fatal("the cancelled OID must still be cleared from state")
	}
	if got := lastLiqAlertAction("hl-eth", "ETH"); got != hlLiquidationActionProtectionLost {
		t.Errorf("alert action = %q, want %q", got, hlLiquidationActionProtectionLost)
	}
}

func TestTrailingWalkerClampReportsDeferredWhenCancelFails(t *testing.T) {
	old := runHyperliquidUpdateStopLossFunc
	defer func() { runHyperliquidUpdateStopLossFunc = old }()
	clearHLLiquidationAlert("hl-eth", "ETH")
	defer clearHLLiquidationAlert("hl-eth", "ETH")

	sc := liqWalkerStrategy()
	runHyperliquidUpdateStopLossFunc = func(script, symbol, side string, size, triggerPx float64, cancelStopLossOID int64) (*HyperliquidStopLossUpdateResult, string, error) {
		return &HyperliquidStopLossUpdateResult{CancelStopLossError: "cancel rejected"}, "", nil
	}
	pos := &Position{AvgCost: 2400, RiskAnchorPrice: 2400}
	if _, _, ok := runHyperliquidTrailingStopUpdate(sc, "ETH", "long", 1.0, pos, 2400, 2400, 2330, 4242,
		trailingReplacePolicy{liquidationPx: 2340.5}, nil, newTestLogger(t)); ok {
		t.Fatal("a failed cancel must not confirm the update")
	}
	if got := lastLiqAlertAction("hl-eth", "ETH"); got != hlLiquidationActionReplaceDeferred {
		t.Errorf("alert action = %q, want %q", got, hlLiquidationActionReplaceDeferred)
	}
}

func TestProtectionSyncEchoKeepsTheRestingTrigger(t *testing.T) {
	cases := []struct {
		name         string
		side         string
		liqPx        float64
		healedRestPx float64
	}{
		{"long with liquidation above the anchor", "long", 2500, 2500 * (1 + hlLiquidationStopBufferPct/100.0)},
		{"short with liquidation below the anchor", "short", 2300, 2300 * (1 - hlLiquidationStopBufferPct/100.0)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mult := 2.5
			sc := StrategyConfig{
				ID: "hl-eth", Type: "perps", Platform: "hyperliquid", Script: "x.py",
				Args:            []string{"x.py", "ETH", "1h", "--mode=live"},
				StopLossATRMult: &mult,
			}
			pos := &Position{
				Symbol: "ETH", Side: tc.side, Quantity: 1.0,
				AvgCost: 2400, RiskAnchorPrice: 2400, EntryATR: 30,
				StopLossOID: 4242, StopLossTriggerPx: tc.healedRestPx,
			}

			plan, ok := buildHyperliquidProtectionPlan(sc, pos, tc.liqPx)
			if !ok {
				t.Fatal("expected a plan")
			}
			if plan.ForceSLReplace {
				t.Error("an unclampable far-side geometry must not force a replace")
			}

			applyHyperliquidProtectionSync(pos, &HyperliquidProtectionSyncResult{StopLossOID: 4242}, nil)
			if !approxEqLiq(pos.StopLossTriggerPx, tc.healedRestPx) {
				t.Fatalf("recorded trigger = %g, want the resting %g — an echo must not rewrite it",
					pos.StopLossTriggerPx, tc.healedRestPx)
			}

			acts := planHyperliquidLiquidationAudit([]hlLiquidationAuditCandidate{{
				StrategyID: "hl-eth", Symbol: "ETH", Side: tc.side, Qty: 1,
				StopLossOID: 4242, StopLossTriggerPx: pos.StopLossTriggerPx,
				LiquidationPx: tc.liqPx, BookConsistent: true,
			}})
			if len(acts) != 0 {
				t.Errorf("audit actions = %+v, want none — the resting stop is already inside liquidation", acts)
			}
		})
	}
}

func TestTrailingWalkerClampRetriesPlacementItStripped(t *testing.T) {
	old := runHyperliquidUpdateStopLossFunc
	defer func() { runHyperliquidUpdateStopLossFunc = old }()
	clearHLLiquidationAlert("hl-eth", "ETH")
	defer clearHLLiquidationAlert("hl-eth", "ETH")

	sc := liqWalkerStrategy()
	type call struct {
		cancelOID int64
	}
	var calls []call
	callN := 0
	runHyperliquidUpdateStopLossFunc = func(script, symbol, side string, size, triggerPx float64, cancelStopLossOID int64) (*HyperliquidStopLossUpdateResult, string, error) {
		callN++
		calls = append(calls, call{cancelOID: cancelStopLossOID})
		if callN == 1 {
			return &HyperliquidStopLossUpdateResult{
				CancelStopLossSucceeded: true,
				StopLossError:           "Order would exceed the open order limit",
			}, "", nil
		}
		return &HyperliquidStopLossUpdateResult{StopLossOID: 8100, StopLossTriggerPx: triggerPx}, "", nil
	}
	pos := &Position{AvgCost: 2400, RiskAnchorPrice: 2400}
	newHighWater, result, ok := runHyperliquidTrailingStopUpdate(sc, "ETH", "long", 1.0, pos, 2400, 2400, 2330, 4242,
		trailingReplacePolicy{liquidationPx: 2340.5}, nil, newTestLogger(t))
	if !ok || result == nil || result.StopLossOID != 8100 {
		t.Fatalf("walker must adopt the RETRY result (oid=%v ok=%v)", result, ok)
	}
	_ = newHighWater
	if len(calls) != 2 || calls[1].cancelOID != 0 {
		t.Errorf("calls = %+v, want exactly one fresh retry with cancelOID 0", calls)
	}
	if got := lastLiqAlertAction("hl-eth", "ETH"); got != hlLiquidationActionClamped {
		t.Errorf("alert action = %q, want %q — the position IS protected again", got, hlLiquidationActionClamped)
	}
}

func TestTrailingWalkerErrorPayloadAfterCancelLandedRunsRetry(t *testing.T) {
	old := runHyperliquidUpdateStopLossFunc
	defer func() { runHyperliquidUpdateStopLossFunc = old }()
	clearHLLiquidationAlert("hl-eth", "ETH")
	defer clearHLLiquidationAlert("hl-eth", "ETH")

	sc := liqWalkerStrategy()
	calls := 0
	runHyperliquidUpdateStopLossFunc = func(script, symbol, side string, size, triggerPx float64, cancelStopLossOID int64) (*HyperliquidStopLossUpdateResult, string, error) {
		calls++
		if calls == 1 {
			return &HyperliquidStopLossUpdateResult{Error: "boom after cancel", CancelStopLossSucceeded: true}, "", nil
		}
		if cancelStopLossOID != 0 {
			t.Errorf("retry cancel OID = %d, want 0 (fresh placement)", cancelStopLossOID)
		}
		return &HyperliquidStopLossUpdateResult{StopLossOID: 7009, StopLossTriggerPx: triggerPx}, "", nil
	}
	pos := &Position{AvgCost: 2400, RiskAnchorPrice: 2400}
	_, _, ok := runHyperliquidTrailingStopUpdate(sc, "ETH", "long", 1.0, pos, 2400, 2400, 2330, 4242,
		trailingReplacePolicy{liquidationPx: 2340.5}, nil, newTestLogger(t))
	if !ok {
		t.Fatal("walker must treat error-after-cancel as cancel-landed and confirm via the retry")
	}
	if calls != 2 {
		t.Fatalf("placement calls = %d, want 2 (failed replace + in-cycle retry)", calls)
	}
}

func TestLiquidationClampReplaceClassifiesErrorAfterCancelLanded(t *testing.T) {
	old := runHyperliquidUpdateStopLossFunc
	defer func() { runHyperliquidUpdateStopLossFunc = old }()

	candidate := hlLiquidationAuditCandidate{
		Script: "x.py", StrategyID: "hl-eth", Symbol: "ETH", Side: "long",
		Qty: 1.0, StopLossOID: 4242, StopLossTriggerPx: 2330,
	}

	runHyperliquidUpdateStopLossFunc = func(script, symbol, side string, size, triggerPx float64, cancelStopLossOID int64) (*HyperliquidStopLossUpdateResult, string, error) {
		return &HyperliquidStopLossUpdateResult{Error: "boom after cancel", CancelStopLossSucceeded: true}, "", nil
	}
	if _, outcome := hlLiquidationClampReplace(candidate, 2335, newTestLogger(t), nil, nil); outcome != hlReplaceProtectionLost {
		t.Errorf("outcome = %v, want hlReplaceProtectionLost (cancel landed, nothing rests)", outcome)
	}

	runHyperliquidUpdateStopLossFunc = func(script, symbol, side string, size, triggerPx float64, cancelStopLossOID int64) (*HyperliquidStopLossUpdateResult, string, error) {
		return &HyperliquidStopLossUpdateResult{Error: "pre-cancel boom"}, "", nil
	}
	if _, outcome := hlLiquidationClampReplace(candidate, 2335, newTestLogger(t), nil, nil); outcome != hlReplaceDeferred {
		t.Errorf("outcome = %v, want hlReplaceDeferred (no landed cancel — the old order may rest)", outcome)
	}

	runHyperliquidUpdateStopLossFunc = func(script, symbol, side string, size, triggerPx float64, cancelStopLossOID int64) (*HyperliquidStopLossUpdateResult, string, error) {
		return &HyperliquidStopLossUpdateResult{CancelStopLossError: "cancel down"}, "", nil
	}
	if _, outcome := hlLiquidationClampReplace(candidate, 2335, newTestLogger(t), nil, nil); outcome != hlReplaceDeferred {
		t.Errorf("outcome = %v, want hlReplaceDeferred for a FAILED cancel", outcome)
	}
}

func TestConvergeHedgesAfterAuditClose(t *testing.T) {
	old := postAuditHedgeSyncFn
	defer func() { postAuditHedgeSyncFn = old }()

	hedged := StrategyConfig{
		ID: "hl-btc", Type: "perps", Platform: "hyperliquid",
		Args:  []string{"x.py", "BTC", "1h"},
		Hedge: &HedgeConfig{Symbol: "ETH", Enabled: true},
	}
	unhedged := liqWalkerStrategy()

	var synced []string
	postAuditHedgeSyncFn = func(sc StrategyConfig, s *StrategyState, mu *sync.RWMutex, exec hedgeExecutor, in hedgeSyncInputs, notifier *MultiNotifier, logger *StrategyLogger) hedgeActionKind {
		synced = append(synced, sc.ID)
		if in.PrimaryPx != 100 || in.HedgePx != 50 {
			t.Errorf("%s: prices PrimaryPx=%g HedgePx=%g, want 100/50", sc.ID, in.PrimaryPx, in.HedgePx)
		}
		if in.FreshExposureQty != 0 {
			t.Errorf("%s: FreshExposureQty=%g, want 0 (the audit only reduces)", sc.ID, in.FreshExposureQty)
		}
		return hedgeActionNone
	}

	details := []hlLiquidationCloseDetail{
		{SC: hedged, Symbol: "BTC", FillPx: 100},
		{SC: unhedged, Symbol: "ETH", FillPx: 2335},
	}
	states := map[string]*StrategyState{"hl-btc": {Positions: map[string]*Position{}}}
	n := convergeHedgesAfterAuditClose(details, states, &sync.RWMutex{},
		map[string]float64{"BTC": 100, "ETH": 50}, nil,
		func(string) (*StrategyLogger, error) { return newTestLogger(t), nil })
	if n != 1 || len(synced) != 1 || synced[0] != "hl-btc" {
		t.Errorf("converged=%d synced=%v, want exactly the hedged strategy hl-btc", n, synced)
	}

	n = convergeHedgesAfterAuditClose(details[:1], map[string]*StrategyState{}, &sync.RWMutex{},
		map[string]float64{"BTC": 100}, nil,
		func(string) (*StrategyLogger, error) { return newTestLogger(t), nil })
	if n != 0 {
		t.Errorf("converged=%d, want 0 when strategy state is missing", n)
	}
}

func TestLiquidationClampReplaceOutcomeUnknownSuppressesRetry(t *testing.T) {
	old := runHyperliquidUpdateStopLossFunc
	defer func() { runHyperliquidUpdateStopLossFunc = old }()

	candidate := hlLiquidationAuditCandidate{
		Script: "x.py", StrategyID: "hl-eth", Symbol: "ETH", Side: "long",
		Qty: 1.0, StopLossOID: 4242, StopLossTriggerPx: 2330,
	}
	calls := 0
	runHyperliquidUpdateStopLossFunc = func(script, symbol, side string, size, triggerPx float64, cancelStopLossOID int64) (*HyperliquidStopLossUpdateResult, string, error) {
		calls++
		return &HyperliquidStopLossUpdateResult{
			CancelStopLossSucceeded: true,
			StopLossError:           "place_stop_loss SDK error: boom",
			StopLossOutcomeUnknown:  true,
		}, "", nil
	}
	result, outcome := hlLiquidationClampReplace(candidate, 2335, newTestLogger(t), nil, nil)
	if outcome != hlReplaceOutcomeUnknown {
		t.Errorf("outcome = %v, want hlReplaceOutcomeUnknown", outcome)
	}
	if calls != 1 {
		t.Fatalf("placement calls = %d, want 1", calls)
	}
	if hlLiquidationMayRetryReplace(result) {
		t.Error("outcome-unknown must suppress the fresh-placement retry")
	}
	if !hlLiquidationMayRetryReplace(&HyperliquidStopLossUpdateResult{CancelStopLossSucceeded: true}) {
		t.Error("a positively-rejected placement must still allow the retry")
	}
}

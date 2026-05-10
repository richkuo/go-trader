package main

import (
	"math"
	"path/filepath"
	"testing"
	"time"
)

func ts(unixSec int64) time.Time {
	return time.Unix(unixSec, 0).UTC()
}

func approxEq(a, b float64) bool {
	return math.Abs(a-b) < 1e-6
}

func TestBackfillUserFillsStartTimeSubtractsLookback(t *testing.T) {
	earliest := time.Date(2026, 5, 5, 12, 0, 0, 0, time.UTC)
	got := backfillUserFillsStartTime(earliest)
	want := earliest.Add(-backfillHLUserFillsLookback)
	if !got.Equal(want) {
		t.Fatalf("backfillUserFillsStartTime() = %s, want %s", got, want)
	}
	if !got.Before(earliest) {
		t.Fatalf("query start should predate earliest trade: got %s earliest %s", got, earliest)
	}
}

func TestBackfillUserFillsStartTimeClampsNearUnixEpoch(t *testing.T) {
	got := backfillUserFillsStartTime(time.Unix(1, 0).UTC())
	want := time.UnixMilli(1).UTC()
	if !got.Equal(want) {
		t.Fatalf("backfillUserFillsStartTime() = %s, want %s", got, want)
	}
}

// TestPlanBackfillRewritesFeeAndPnLOnCloseLeg verifies the core correction
// math: a close leg with a real fee that differs from the modeled fee gets
// realized_pnl adjusted by (modeledFee - realFee), and the row's exchange_fee
// is rewritten to the real value.
func TestPlanBackfillRewritesFeeAndPnLOnCloseLeg(t *testing.T) {
	openValue := 1000.0
	closeValue := 1010.0
	modeledCloseFee := closeValue * HyperliquidTakerFeePct // 0.3535
	// realized_pnl as written at execution time: pnl_pre_fee - modeledFee.
	// long position: closeValue - openValue = 10; minus modeledCloseFee.
	storedRealizedPnL := 10.0 - modeledCloseFee

	trades := []TradeBackfillRow{
		{RowID: 1, Timestamp: ts(100), Symbol: "ETH", PositionID: "p1", Value: openValue,
			IsClose: false, ExchangeOrderID: "111", ExchangeFee: 0, RealizedPnL: 0},
		{RowID: 2, Timestamp: ts(200), Symbol: "ETH", PositionID: "p1", Value: closeValue,
			IsClose: true, ExchangeOrderID: "222", ExchangeFee: 0, RealizedPnL: storedRealizedPnL},
	}
	realOpenFee := 0.40  // higher than modeled
	realCloseFee := 0.30 // lower than modeled
	fillMap := map[string]HLFillSummary{
		"111": {Fee: realOpenFee, ClosedPnLGross: 0, Count: 1},
		"222": {Fee: realCloseFee, ClosedPnLGross: 9.7, Count: 1},
	}

	plan := planBackfillForStrategy("hl-eth", trades, fillMap, 1000.0, 1000.0)

	if got, want := len(plan.TradeChanges), 2; got != want {
		t.Fatalf("expected %d trade changes, got %d", want, got)
	}
	if plan.MissingOIDCount != 0 || plan.UnmatchedOIDCount != 0 {
		t.Fatalf("unexpected skips: missing=%d unmatched=%d", plan.MissingOIDCount, plan.UnmatchedOIDCount)
	}

	openChange := plan.TradeChanges[0]
	if !approxEq(openChange.NewFee, realOpenFee) {
		t.Fatalf("open: NewFee=%v want %v", openChange.NewFee, realOpenFee)
	}
	if !approxEq(openChange.NewRealizedPnL, 0) {
		t.Fatalf("open: NewRealizedPnL should stay 0 (open leg), got %v", openChange.NewRealizedPnL)
	}

	closeChange := plan.TradeChanges[1]
	if !approxEq(closeChange.NewFee, realCloseFee) {
		t.Fatalf("close: NewFee=%v want %v", closeChange.NewFee, realCloseFee)
	}
	expectedNewPnL := storedRealizedPnL + (modeledCloseFee - realCloseFee)
	if !approxEq(closeChange.NewRealizedPnL, expectedNewPnL) {
		t.Fatalf("close: NewRealizedPnL=%v want %v (stored %v + modeled %v - real %v)",
			closeChange.NewRealizedPnL, expectedNewPnL, storedRealizedPnL, modeledCloseFee, realCloseFee)
	}
}

// TestPlanBackfillCashReplay verifies the cash recompute walks trades in
// chronological order: open fee debit (real) + close pnl credit (corrected).
func TestPlanBackfillCashReplay(t *testing.T) {
	trades := []TradeBackfillRow{
		{RowID: 1, Timestamp: ts(100), Symbol: "ETH", PositionID: "p1", Value: 1000,
			IsClose: false, ExchangeOrderID: "111", ExchangeFee: 0},
		{RowID: 2, Timestamp: ts(200), Symbol: "ETH", PositionID: "p1", Value: 1010,
			IsClose: true, ExchangeOrderID: "222", ExchangeFee: 0,
			RealizedPnL: 10.0 - 1010*HyperliquidTakerFeePct},
	}
	fillMap := map[string]HLFillSummary{
		"111": {Fee: 0.5},
		"222": {Fee: 0.4},
	}

	plan := planBackfillForStrategy("hl-eth", trades, fillMap, 1000.0, 999.0)

	// Expected cash:
	//   start = 1000
	//   - real open fee 0.5 = 999.5
	//   + corrected close pnl: 10 - 0.4 = 9.6 → 1009.1
	expectedCash := 1000.0 - 0.5 + (10.0 - 0.4)
	if !approxEq(plan.NewCash, expectedCash) {
		t.Fatalf("NewCash=%v want %v", plan.NewCash, expectedCash)
	}
	if plan.OldCash != 999.0 {
		t.Fatalf("OldCash should be passed through, got %v", plan.OldCash)
	}
}

// TestPlanBackfillSkipsAlreadyRealFee guards against double-corrected rows:
// if exchange_fee != 0, the row was written by post-#587 code and must NOT
// be touched.
func TestPlanBackfillSkipsAlreadyRealFee(t *testing.T) {
	trades := []TradeBackfillRow{
		{RowID: 1, Timestamp: ts(100), Symbol: "ETH", Value: 1000,
			IsClose: false, ExchangeOrderID: "111", ExchangeFee: 0.32, RealizedPnL: 0},
	}
	fillMap := map[string]HLFillSummary{
		"111": {Fee: 0.40},
	}
	plan := planBackfillForStrategy("hl-eth", trades, fillMap, 1000.0, 999.68)
	if len(plan.TradeChanges) != 0 {
		t.Fatalf("expected 0 trade changes (already-real guard), got %d", len(plan.TradeChanges))
	}
	skipped := false
	for _, s := range plan.Skipped {
		if s.Reason == "already_real_fee" {
			skipped = true
		}
	}
	if !skipped {
		t.Fatalf("expected an already_real_fee skip entry, got %+v", plan.Skipped)
	}
}

// TestPlanBackfillMissingOID covers pre-#453/#461 rows with empty
// exchange_order_id: they're skipped (can't match) and the cash replay falls
// back to the modeled fee for them.
func TestPlanBackfillMissingOID(t *testing.T) {
	trades := []TradeBackfillRow{
		{RowID: 1, Timestamp: ts(100), Symbol: "ETH", Value: 1000,
			IsClose: false, ExchangeOrderID: "", ExchangeFee: 0},
	}
	fillMap := map[string]HLFillSummary{}
	plan := planBackfillForStrategy("hl-eth", trades, fillMap, 1000.0, 999.65)
	if len(plan.TradeChanges) != 0 {
		t.Fatalf("expected no trade changes for missing-OID row, got %d", len(plan.TradeChanges))
	}
	if plan.MissingOIDCount != 1 {
		t.Fatalf("MissingOIDCount=%d want 1", plan.MissingOIDCount)
	}
	// Cash replay falls back to modeled fee.
	expectedCash := 1000.0 - 1000*HyperliquidTakerFeePct
	if !approxEq(plan.NewCash, expectedCash) {
		t.Fatalf("NewCash=%v want %v (modeled fee fallback)", plan.NewCash, expectedCash)
	}
}

// TestPlanBackfillUnmatchedOID: row has an OID but HL didn't return a fill
// for it (e.g. fill predates the userFills query window). Skip + fall back
// to modeled fee in the cash replay.
func TestPlanBackfillUnmatchedOID(t *testing.T) {
	trades := []TradeBackfillRow{
		{RowID: 1, Timestamp: ts(100), Symbol: "ETH", Value: 1000,
			IsClose: false, ExchangeOrderID: "999", ExchangeFee: 0},
	}
	fillMap := map[string]HLFillSummary{
		"111": {Fee: 0.4},
	}
	plan := planBackfillForStrategy("hl-eth", trades, fillMap, 1000.0, 999.65)
	if plan.UnmatchedOIDCount != 1 {
		t.Fatalf("UnmatchedOIDCount=%d want 1", plan.UnmatchedOIDCount)
	}
	expectedCash := 1000.0 - 1000*HyperliquidTakerFeePct
	if !approxEq(plan.NewCash, expectedCash) {
		t.Fatalf("NewCash=%v want %v", plan.NewCash, expectedCash)
	}
}

// TestPlanClosedPositionRecomputesMatchByTimestamp verifies the closed_positions
// pass: each closed_positions row matches the corrected close-leg trade with
// the same (symbol, closed_at == trade.timestamp), then reads its position_id
// and emits the new aggregate.
func TestPlanClosedPositionRecomputesMatchByTimestamp(t *testing.T) {
	corrected := []TradeBackfillRow{
		{RowID: 1, Timestamp: ts(100), Symbol: "ETH", PositionID: "p1",
			IsClose: false, RealizedPnL: 0},
		{RowID: 2, Timestamp: ts(200), Symbol: "ETH", PositionID: "p1",
			IsClose: true, RealizedPnL: 9.6},
	}
	closedRows := []ClosedPositionRow{
		{ID: 11, Symbol: "ETH", ClosedAt: ts(200), RealizedPnL: 9.65},
	}
	out := planClosedPositionRecomputes(corrected, closedRows)
	if len(out) != 1 {
		t.Fatalf("expected 1 recompute, got %d (%+v)", len(out), out)
	}
	if out[0].RowID != 11 {
		t.Fatalf("RowID=%d want 11", out[0].RowID)
	}
	if !approxEq(out[0].NewPnL, 9.6) {
		t.Fatalf("NewPnL=%v want 9.6", out[0].NewPnL)
	}
	if out[0].PositionID != "p1" {
		t.Fatalf("PositionID=%q want p1", out[0].PositionID)
	}
}

// TestPlanClosedPositionRecomputesSkipsBelowTolerance: when corrected
// realized_pnl matches the stored value within 0.001 USD, no recompute is
// emitted (avoid floating-point noise rewrites).
func TestPlanClosedPositionRecomputesSkipsBelowTolerance(t *testing.T) {
	corrected := []TradeBackfillRow{
		{RowID: 1, Timestamp: ts(100), Symbol: "ETH", PositionID: "p1",
			IsClose: true, RealizedPnL: 9.6500001},
	}
	closedRows := []ClosedPositionRow{
		{ID: 11, Symbol: "ETH", ClosedAt: ts(100), RealizedPnL: 9.65},
	}
	out := planClosedPositionRecomputes(corrected, closedRows)
	if len(out) != 0 {
		t.Fatalf("expected 0 recomputes (tolerance), got %d", len(out))
	}
}

// TestApplyBackfillPlanRoundTrip seeds a fresh SQLite, runs the planner end
// to end against a synthetic fill map, applies, and reads back to confirm
// the trade rows, closed_positions, and strategies.cash were rewritten as
// the planner promised.
func TestApplyBackfillPlanRoundTrip(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "state.db")
	db, err := OpenStateDB(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	// Seed: one strategy with one round trip — open + close legs that have
	// modeled fees written to realized_pnl but exchange_fee=0.
	openTs := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	closeTs := openTs.Add(2 * time.Hour)
	closeValue := 1010.0
	modeledCloseFee := closeValue * HyperliquidTakerFeePct

	state := &AppState{
		CycleCount: 1,
		Strategies: map[string]*StrategyState{
			"hl-eth-live": {
				ID: "hl-eth-live", Type: "perps", Platform: "hyperliquid",
				Cash: 999.65, InitialCapital: 1000,
				Positions: make(map[string]*Position), OptionPositions: make(map[string]*OptionPosition),
				ClosedPositions: []ClosedPosition{
					{StrategyID: "hl-eth-live", Symbol: "ETH", Quantity: 0.1, AvgCost: 10000,
						Side: "long", Multiplier: 1, OpenedAt: openTs, ClosedAt: closeTs,
						ClosePrice: 10100, RealizedPnL: 10.0 - modeledCloseFee, CloseReason: "manual_close"},
				},
			},
		},
	}
	if err := db.SaveState(state); err != nil {
		t.Fatal(err)
	}

	openTrade := Trade{
		Timestamp: openTs, StrategyID: "hl-eth-live", Symbol: "ETH",
		PositionID: "p1", Side: "buy", Quantity: 0.1, Price: 10000, Value: 1000,
		TradeType: "perps", Details: "open", ExchangeOrderID: "111", ExchangeFee: 0,
	}
	closeTrade := Trade{
		Timestamp: closeTs, StrategyID: "hl-eth-live", Symbol: "ETH",
		PositionID: "p1", Side: "sell", Quantity: 0.1, Price: 10100, Value: 1010,
		TradeType: "perps", Details: "close", ExchangeOrderID: "222", ExchangeFee: 0,
		IsClose: true, RealizedPnL: 10.0 - modeledCloseFee,
	}
	if err := db.InsertTrade("hl-eth-live", openTrade); err != nil {
		t.Fatal(err)
	}
	if err := db.InsertTrade("hl-eth-live", closeTrade); err != nil {
		t.Fatal(err)
	}

	// Real fees from the exchange (close fee was overstated by the model).
	realOpenFee := 0.40
	realCloseFee := 0.30
	fillMap := map[string]HLFillSummary{
		"111": {Fee: realOpenFee, Count: 1},
		"222": {Fee: realCloseFee, Count: 1},
	}

	trades, err := db.ListTradesForBackfill("hl-eth-live")
	if err != nil {
		t.Fatal(err)
	}
	if len(trades) != 2 {
		t.Fatalf("ListTradesForBackfill returned %d rows, want 2", len(trades))
	}
	closedRows, err := db.LoadClosedPositionRows("hl-eth-live")
	if err != nil {
		t.Fatal(err)
	}
	if len(closedRows) != 1 {
		t.Fatalf("LoadClosedPositionRows returned %d rows, want 1", len(closedRows))
	}

	plan := planBackfillForStrategy("hl-eth-live", trades, fillMap, 1000.0, 999.65)
	changeByRowID := make(map[int64]TradeChange, len(plan.TradeChanges))
	for _, c := range plan.TradeChanges {
		changeByRowID[c.RowID] = c
	}
	correctedTrades := make([]TradeBackfillRow, 0, len(trades))
	for _, tr := range trades {
		row := tr
		if c, ok := changeByRowID[tr.RowID]; ok {
			row.ExchangeFee = c.NewFee
			row.RealizedPnL = c.NewRealizedPnL
		}
		correctedTrades = append(correctedTrades, row)
	}
	plan.ClosedPositions = planClosedPositionRecomputes(correctedTrades, closedRows)

	if err := db.ApplyBackfillPlan(plan); err != nil {
		t.Fatalf("ApplyBackfillPlan failed: %v", err)
	}

	// Verify trades.exchange_fee + realized_pnl rewritten.
	post, err := db.ListTradesForBackfill("hl-eth-live")
	if err != nil {
		t.Fatal(err)
	}
	for _, tr := range post {
		if tr.IsClose {
			if !approxEq(tr.ExchangeFee, realCloseFee) {
				t.Errorf("close exchange_fee=%v want %v", tr.ExchangeFee, realCloseFee)
			}
			expectedPnL := (10.0 - modeledCloseFee) + (modeledCloseFee - realCloseFee)
			if !approxEq(tr.RealizedPnL, expectedPnL) {
				t.Errorf("close realized_pnl=%v want %v", tr.RealizedPnL, expectedPnL)
			}
		} else {
			if !approxEq(tr.ExchangeFee, realOpenFee) {
				t.Errorf("open exchange_fee=%v want %v", tr.ExchangeFee, realOpenFee)
			}
		}
	}

	// Verify closed_positions.realized_pnl rewritten.
	postCP, err := db.LoadClosedPositionRows("hl-eth-live")
	if err != nil {
		t.Fatal(err)
	}
	expectedCPPnL := (10.0 - modeledCloseFee) + (modeledCloseFee - realCloseFee)
	if !approxEq(postCP[0].RealizedPnL, expectedCPPnL) {
		t.Errorf("closed_positions realized_pnl=%v want %v", postCP[0].RealizedPnL, expectedCPPnL)
	}

	// Verify strategies.cash rewritten to plan.NewCash.
	cfg := &Config{DBFile: dbPath}
	loaded, err := LoadStateWithDB(cfg, db)
	if err != nil {
		t.Fatal(err)
	}
	ss := loaded.Strategies["hl-eth-live"]
	if ss == nil {
		t.Fatal("strategy not loaded")
	}
	expectedCash := 1000.0 - realOpenFee + (10.0 - realCloseFee)
	if !approxEq(ss.Cash, expectedCash) {
		t.Errorf("strategies.cash=%v want %v", ss.Cash, expectedCash)
	}
}

// TestPlanClosedPositionRecomputesAggregatesPartialCloses: a single
// closed_positions row whose position_id has multiple corrected close legs
// gets the sum.
func TestPlanClosedPositionRecomputesAggregatesPartialCloses(t *testing.T) {
	corrected := []TradeBackfillRow{
		{RowID: 1, Timestamp: ts(100), Symbol: "ETH", PositionID: "p1",
			IsClose: true, RealizedPnL: 5.0},
		{RowID: 2, Timestamp: ts(200), Symbol: "ETH", PositionID: "p1",
			IsClose: true, RealizedPnL: 4.6}, // final close
	}
	closedRows := []ClosedPositionRow{
		{ID: 11, Symbol: "ETH", ClosedAt: ts(200), RealizedPnL: 9.65},
	}
	out := planClosedPositionRecomputes(corrected, closedRows)
	if len(out) != 1 {
		t.Fatalf("expected 1 recompute, got %d", len(out))
	}
	if !approxEq(out[0].NewPnL, 9.6) {
		t.Fatalf("NewPnL=%v want 9.6 (sum of partial closes)", out[0].NewPnL)
	}
}

// TestPlanBackfillAlreadyRealFeeCount: post-#587 rows with non-zero
// exchange_fee bump AlreadyRealFeeCount so the report's skip breakdown adds up.
func TestPlanBackfillAlreadyRealFeeCount(t *testing.T) {
	trades := []TradeBackfillRow{
		{RowID: 1, Timestamp: ts(100), Symbol: "ETH", Value: 1000,
			IsClose: false, ExchangeOrderID: "111", ExchangeFee: 0.32},
		{RowID: 2, Timestamp: ts(200), Symbol: "ETH", Value: 1000,
			IsClose: false, ExchangeOrderID: "222", ExchangeFee: 0.40},
	}
	fillMap := map[string]HLFillSummary{
		"111": {Fee: 0.30},
		"222": {Fee: 0.45},
	}
	plan := planBackfillForStrategy("hl-eth", trades, fillMap, 1000.0, 999.28)
	if plan.AlreadyRealFeeCount != 2 {
		t.Fatalf("AlreadyRealFeeCount=%d want 2", plan.AlreadyRealFeeCount)
	}
	if got := plan.MissingOIDCount + plan.UnmatchedOIDCount + plan.AlreadyRealFeeCount; got != len(plan.Skipped) {
		t.Fatalf("skip breakdown does not add up: missing=%d + unmatched=%d + already=%d != len(Skipped)=%d",
			plan.MissingOIDCount, plan.UnmatchedOIDCount, plan.AlreadyRealFeeCount, len(plan.Skipped))
	}
}

// TestPlanBackfillCashBaselineDivergent flags a strategy whose stored
// strategies.cash diverges from the pre-correction replay (modeled-fee
// fallback) by more than $1 — typically a SIGHUP capital top-up that didn't
// emit a trade row. Forward-replay-from-initial-capital cannot reproduce that
// mutation, so --apply must require --reset-cash to acknowledge the loss.
func TestPlanBackfillCashBaselineDivergent(t *testing.T) {
	trades := []TradeBackfillRow{
		{RowID: 1, Timestamp: ts(100), Symbol: "ETH", Value: 1000,
			IsClose: false, ExchangeOrderID: "111", ExchangeFee: 0},
	}
	// Stored cash is 1500 — way above what initial_capital(1000) - modeledFee
	// (~0.35) would predict. Operator must have raised capital mid-run.
	plan := planBackfillForStrategy("hl-eth", trades, map[string]HLFillSummary{}, 1000.0, 1500.0)
	if !plan.CashBaselineDivergent {
		t.Fatalf("expected CashBaselineDivergent=true (replayed=%v vs old=%v)",
			plan.ReplayedCash, plan.OldCash)
	}
	expectedReplay := 1000.0 - 1000*HyperliquidTakerFeePct
	if !approxEq(plan.ReplayedCash, expectedReplay) {
		t.Fatalf("ReplayedCash=%v want %v", plan.ReplayedCash, expectedReplay)
	}
}

// TestPlanBackfillCashBaselineWithinTolerance: a row whose stored cash is
// within $1 of the pre-correction replay should NOT trip the divergence flag
// (e.g. tiny fee-rounding skew should not gate --apply).
func TestPlanBackfillCashBaselineWithinTolerance(t *testing.T) {
	trades := []TradeBackfillRow{
		{RowID: 1, Timestamp: ts(100), Symbol: "ETH", Value: 1000,
			IsClose: false, ExchangeOrderID: "111", ExchangeFee: 0},
	}
	// Cash sits within $1 of (1000 - modeled_fee).
	plan := planBackfillForStrategy("hl-eth", trades, map[string]HLFillSummary{}, 1000.0, 999.65)
	if plan.CashBaselineDivergent {
		t.Fatalf("did not expect divergence (replayed=%v old=%v)",
			plan.ReplayedCash, plan.OldCash)
	}
}

// TestPlanClosedPositionRecomputesRejectsAmbiguousFallback: when the exact-ns
// match misses and two close legs sit within the 5s tolerance window on the
// same symbol, the row stays unmatched rather than picking the nearest leg —
// a partial-then-final close back-to-back must not silently land on the wrong
// position_id.
func TestPlanClosedPositionRecomputesRejectsAmbiguousFallback(t *testing.T) {
	corrected := []TradeBackfillRow{
		{RowID: 1, Timestamp: ts(101), Symbol: "ETH", PositionID: "pA",
			IsClose: true, RealizedPnL: 5.0},
		{RowID: 2, Timestamp: ts(103), Symbol: "ETH", PositionID: "pB",
			IsClose: true, RealizedPnL: 7.0},
	}
	// closed_positions row at ts(100): exact match misses; both close legs
	// fall within the 5s window. Old code would pick pA; tightened code
	// refuses to guess.
	closedRows := []ClosedPositionRow{
		{ID: 11, Symbol: "ETH", ClosedAt: ts(100), RealizedPnL: 4.9},
	}
	out := planClosedPositionRecomputes(corrected, closedRows)
	if len(out) != 0 {
		t.Fatalf("expected 0 recomputes (ambiguous match), got %d (%+v)", len(out), out)
	}
}

// TestPlanClosedPositionRecomputesRejectsBackwardFallback: when the only
// candidate leg lands BEFORE the closed_positions row, refuse to match —
// close legs are written before/with their closed_positions row, never after.
func TestPlanClosedPositionRecomputesRejectsBackwardFallback(t *testing.T) {
	corrected := []TradeBackfillRow{
		{RowID: 1, Timestamp: ts(95), Symbol: "ETH", PositionID: "pA",
			IsClose: true, RealizedPnL: 5.0},
	}
	closedRows := []ClosedPositionRow{
		{ID: 11, Symbol: "ETH", ClosedAt: ts(100), RealizedPnL: 4.9},
	}
	out := planClosedPositionRecomputes(corrected, closedRows)
	if len(out) != 0 {
		t.Fatalf("expected 0 recomputes (backward leg), got %d (%+v)", len(out), out)
	}
}

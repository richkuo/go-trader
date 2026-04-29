package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"
)

// silentStrategyLogger returns a StrategyLogger that writes to io.Discard
// so tests don't pollute test output. The constructor name follows the
// project convention of platform/feature-prefixed test helpers (CLAUDE.md).
func silentStrategyLogger(id string) *StrategyLogger {
	return &StrategyLogger{stratID: id, writer: io.Discard}
}

// Tests for the stop-loss plumbing added in #412. We exercise the pure
// output parser (which Go CI can run without .venv/bin/python3) plus
// StrategyConfig/Position serialization round-trips so struct-tag
// regressions on the new fields surface here.

func TestParseHyperliquidExecuteOutput_StopLossFields(t *testing.T) {
	stdout := []byte(`{
		"execution": {
			"action": "buy",
			"symbol": "ETH",
			"size": 0.25,
			"fill": {
				"avg_px": 3200.5,
				"total_sz": 0.25,
				"oid": 987654,
				"fee": 0.40,
				"stop_loss_oid": 12345678,
				"stop_loss_trigger_px": 3104.485
			}
		},
		"platform": "hyperliquid",
		"timestamp": "2026-04-23T12:00:00+00:00"
	}`)

	result, stderr, err := parseHyperliquidExecuteOutput(stdout, "", nil)
	if err != nil {
		t.Fatalf("parse failed: %v", err)
	}
	if stderr != "" {
		t.Errorf("expected empty stderr, got %q", stderr)
	}
	if result.Execution == nil || result.Execution.Fill == nil {
		t.Fatalf("missing execution/fill: %+v", result)
	}
	fill := result.Execution.Fill
	if fill.OID != 987654 {
		t.Errorf("OID: got %d, want 987654", fill.OID)
	}
	if fill.StopLossOID != 12345678 {
		t.Errorf("StopLossOID: got %d, want 12345678", fill.StopLossOID)
	}
	if fill.StopLossTriggerPx != 3104.485 {
		t.Errorf("StopLossTriggerPx: got %v, want 3104.485", fill.StopLossTriggerPx)
	}
}

func TestParseHyperliquidExecuteOutput_NonFatalSLErrors(t *testing.T) {
	// When SL placement fails but the main fill succeeds, the Python side
	// emits top-level stop_loss_error / cancel_stop_loss_error strings and
	// keeps the execution block intact. Parser must surface both so the
	// scheduler can log them without aborting state updates.
	stdout := []byte(`{
		"execution": {
			"action": "sell",
			"symbol": "BTC",
			"size": 0.01,
			"fill": {"avg_px": 67000, "total_sz": 0.01, "oid": 42}
		},
		"platform": "hyperliquid",
		"timestamp": "2026-04-23T12:00:00+00:00",
		"cancel_stop_loss_error": "trigger already cancelled",
		"stop_loss_error": "placement rejected: max triggers reached"
	}`)

	result, _, err := parseHyperliquidExecuteOutput(stdout, "warn: something", nil)
	if err != nil {
		t.Fatalf("parse failed: %v", err)
	}
	if result.CancelStopLossError != "trigger already cancelled" {
		t.Errorf("CancelStopLossError: got %q", result.CancelStopLossError)
	}
	if result.StopLossError != "placement rejected: max triggers reached" {
		t.Errorf("StopLossError: got %q", result.StopLossError)
	}
	if result.Execution == nil || result.Execution.Fill.OID != 42 {
		t.Errorf("main fill should still parse: %+v", result)
	}
}

func TestParseHyperliquidExecuteOutput_ErrorJSONPreserved(t *testing.T) {
	// Python script exits 1 with an {"error": "..."} payload; runErr is
	// non-nil but parser should return the decoded result so the scheduler
	// can log the reason without treating it as an unparseable failure.
	stdout := []byte(`{"execution": null, "platform": "hyperliquid", "timestamp": "2026-04-23T12:00:00+00:00", "error": "--execute requires --mode=live"}`)
	runErr := errors.New("exit status 1")
	result, _, err := parseHyperliquidExecuteOutput(stdout, "", runErr)
	if err != nil {
		t.Fatalf("parse should swallow runErr when JSON carries .error: %v", err)
	}
	if result == nil || result.Error == "" {
		t.Fatalf("expected error payload, got %+v", result)
	}
}

func TestStrategyConfig_StopLossPctJSON(t *testing.T) {
	sc := StrategyConfig{
		ID:          "hl-donch-btc",
		Platform:    "hyperliquid",
		Type:        "perps",
		StopLossPct: 3.5,
	}
	b, err := json.Marshal(sc)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var round StrategyConfig
	if err := json.Unmarshal(b, &round); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if round.StopLossPct != 3.5 {
		t.Errorf("round-trip StopLossPct: got %v, want 3.5", round.StopLossPct)
	}
	// omitempty check: default-value config must not emit the field.
	b2, _ := json.Marshal(StrategyConfig{ID: "x", Platform: "hyperliquid", Type: "perps"})
	if containsKey(b2, "stop_loss_pct") {
		t.Errorf("zero StopLossPct should be omitted; got %s", b2)
	}
}

func TestPosition_StopLossOIDJSON(t *testing.T) {
	p := Position{Symbol: "ETH", Quantity: 1, AvgCost: 3000, Side: "long", StopLossOID: 42, StopLossTriggerPx: 2900}
	b, err := json.Marshal(p)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var round Position
	if err := json.Unmarshal(b, &round); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if round.StopLossOID != 42 {
		t.Errorf("round-trip StopLossOID: got %v", round.StopLossOID)
	}
	if round.StopLossTriggerPx != 2900 {
		t.Errorf("round-trip StopLossTriggerPx: got %v", round.StopLossTriggerPx)
	}
	// omitempty: zero should drop from JSON.
	b2, _ := json.Marshal(Position{Symbol: "ETH", Quantity: 1, AvgCost: 3000, Side: "long"})
	if containsKey(b2, "stop_loss_oid") {
		t.Errorf("zero StopLossOID should be omitted; got %s", b2)
	}
	if containsKey(b2, "stop_loss_trigger_px") {
		t.Errorf("zero StopLossTriggerPx should be omitted; got %s", b2)
	}
}

func containsKey(b []byte, key string) bool {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(b, &m); err != nil {
		return false
	}
	_, ok := m[key]
	return ok
}

func TestParseHyperliquidExecuteOutput_StopLossFilledImmediately(t *testing.T) {
	// Issue 421: when price is already through the trigger at submit, HL
	// fills the SL immediately. The Python side surfaces this as
	// stop_loss_filled_immediately=true (no OID) so the scheduler can
	// reconcile virtual state instead of treating it as a placement error.
	stdout := []byte(`{
		"execution": {"action": "buy", "symbol": "ETH", "size": 0.1, "fill": {"avg_px": 3200, "total_sz": 0.1, "oid": 1}},
		"platform": "hyperliquid",
		"timestamp": "2026-04-25T00:00:00+00:00",
		"stop_loss_filled_immediately": true
	}`)
	result, _, err := parseHyperliquidExecuteOutput(stdout, "", nil)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if !result.StopLossFilledImmediately {
		t.Errorf("expected StopLossFilledImmediately=true, got %+v", result)
	}
	if result.Execution.Fill.StopLossOID != 0 {
		t.Errorf("instant-fill should have no resting OID; got %d", result.Execution.Fill.StopLossOID)
	}
}

func TestParseHyperliquidExecuteOutput_CancelSucceededOnFailure(t *testing.T) {
	// Issue 421 (review point 3): when the cancel succeeds but the subsequent
	// open fails, the Python error path still emits cancel_stop_loss_succeeded
	// so the scheduler can drop the dead OID from pos.StopLossOID.
	stdout := []byte(`{
		"execution": null,
		"platform": "hyperliquid",
		"timestamp": "2026-04-25T00:00:00+00:00",
		"error": "market_open: insufficient balance",
		"cancel_stop_loss_succeeded": true
	}`)
	runErr := errors.New("exit status 1")
	result, _, err := parseHyperliquidExecuteOutput(stdout, "", runErr)
	if err != nil {
		t.Fatalf("parse should swallow runErr when JSON carries .error: %v", err)
	}
	if !result.CancelStopLossSucceeded {
		t.Errorf("CancelStopLossSucceeded should be true, got %+v", result)
	}
	if result.Error == "" {
		t.Errorf("error payload should be preserved")
	}
}

func TestIsHLTriggerCapRejection(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want bool
	}{
		{"too many", "Too many open trigger orders", true},
		{"rate limit", "trigger order rate limit exceeded", true},
		{"max", "max trigger orders per day reached", true},
		{"unrelated", "insufficient margin", false},
		{"empty", "", false},
		{"trigger only", "trigger price out of range", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := isHLTriggerCapRejection(c.in); got != c.want {
				t.Errorf("isHLTriggerCapRejection(%q)=%v, want %v", c.in, got, c.want)
			}
		})
	}
}

func TestValidateConfig_StopLossPctBounds(t *testing.T) {
	// Issue 421 (review point 4): hand-edited configs with out-of-range
	// stop_loss_pct must fail validation rather than silently break the
	// safety feature.
	cases := []struct {
		name      string
		pct       float64
		platform  string
		typ       string
		wantError bool
	}{
		{"zero ok", 0, "hyperliquid", "perps", false},
		{"in range", 5, "hyperliquid", "perps", false},
		{"max boundary", 50, "hyperliquid", "perps", false},
		{"too high", 200, "hyperliquid", "perps", true},
		{"negative", -1, "hyperliquid", "perps", true},
		{"non-HL platform", 5, "okx", "perps", true},
		{"non-perps type", 5, "hyperliquid", "spot", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cfg := &Config{
				IntervalSeconds: 60,
				Strategies: []StrategyConfig{
					{
						ID:             "test",
						Type:           c.typ,
						Platform:       c.platform,
						Script:         "shared_scripts/check_hyperliquid.py",
						Capital:        1000,
						MaxDrawdownPct: 10,
						StopLossPct:    c.pct,
					},
				},
				PortfolioRisk: &PortfolioRiskConfig{MaxDrawdownPct: 25, WarnThresholdPct: 60},
			}
			err := ValidateConfig(cfg)
			gotErr := err != nil && containsStopLossErr(err.Error())
			if gotErr != c.wantError {
				t.Errorf("got err=%v wantStopLossErr=%v (full err: %v)", gotErr, c.wantError, err)
			}
		})
	}
}

func containsStopLossErr(s string) bool {
	return strings.Contains(s, "stop_loss_pct")
}

// #421 review point 2: when StopLossFilledImmediately is true, the on-chain
// position is flat (the trigger fired at submit). executeHyperliquidResult
// must reconcile virtual state by synthesizing a close at trigger_px,
// otherwise the next reconcile cycle silently delete()s the phantom
// position with PnL=0 and the realized loss is dropped from history.
func TestExecuteHyperliquidResult_StopLossFilledImmediately_ReconcilesState(t *testing.T) {
	sc := StrategyConfig{
		ID:       "hl-test-eth",
		Platform: "hyperliquid",
		Type:     "perps",
		Capital:  1000,
		Leverage: 5,
	}
	state := &StrategyState{
		ID:        "hl-test-eth",
		Platform:  "hyperliquid",
		Type:      "perps",
		Cash:      1000,
		Positions: map[string]*Position{},
	}
	result := &HyperliquidResult{Symbol: "ETH", Signal: 1, Price: 3200}
	execResult := &HyperliquidExecuteResult{
		Execution: &HyperliquidExecution{
			Action: "buy",
			Symbol: "ETH",
			Size:   0.1,
			Fill: &HyperliquidFill{
				AvgPx:             3200,
				TotalSz:           0.1,
				OID:               1,
				StopLossTriggerPx: 3104.0,
				// note: no StopLossOID — instant fill leaves no resting OID
			},
		},
		StopLossFilledImmediately: true,
	}

	logger := silentStrategyLogger("hl-test-eth")
	defer logger.Close()
	trades, _ := executeHyperliquidResult(sc, state, result, execResult, "BUY", 3200, logger)

	// Open + synthetic close = 2 trades.
	if trades != 2 {
		t.Errorf("trades=%d, want 2 (open + synthetic close)", trades)
	}
	// On-chain is flat → virtual state must also be flat.
	if _, exists := state.Positions["ETH"]; exists {
		t.Errorf("Position should have been deleted; got %+v", state.Positions["ETH"])
	}
	// One ClosedPosition entry recorded with the trigger price as ClosePrice
	// and the realized PnL on the books (not zero).
	if len(state.ClosedPositions) != 1 {
		t.Fatalf("ClosedPositions=%d, want 1", len(state.ClosedPositions))
	}
	cp := state.ClosedPositions[0]
	if cp.CloseReason != "stop_loss_immediate" {
		t.Errorf("CloseReason=%q, want stop_loss_immediate", cp.CloseReason)
	}
	if cp.ClosePrice != 3104.0 {
		t.Errorf("ClosePrice=%v, want 3104", cp.ClosePrice)
	}
	if cp.RealizedPnL >= 0 {
		t.Errorf("RealizedPnL=%v should be negative for a long stopped out below entry", cp.RealizedPnL)
	}
}

// Defensive: when the instant-fill flag is set but trigger_px is missing
// (shouldn't happen with the current Python contract), the reconcile is
// skipped and the position is left as opened — better than crashing on a
// divide-by-zero or producing nonsense PnL.
func TestExecuteHyperliquidResult_StopLossFilledImmediately_NoTriggerPxIsNoOp(t *testing.T) {
	sc := StrategyConfig{ID: "hl", Platform: "hyperliquid", Type: "perps", Leverage: 1}
	state := &StrategyState{ID: "hl", Platform: "hyperliquid", Type: "perps", Cash: 1000, Positions: map[string]*Position{}}
	result := &HyperliquidResult{Symbol: "ETH", Signal: 1, Price: 3200}
	execResult := &HyperliquidExecuteResult{
		Execution:                 &HyperliquidExecution{Action: "buy", Symbol: "ETH", Size: 0.1, Fill: &HyperliquidFill{AvgPx: 3200, TotalSz: 0.1}},
		StopLossFilledImmediately: true,
	}
	logger := silentStrategyLogger("hl")
	defer logger.Close()
	trades, _ := executeHyperliquidResult(sc, state, result, execResult, "BUY", 3200, logger)
	if trades != 1 {
		t.Errorf("trades=%d, want 1 (only open recorded; reconcile skipped)", trades)
	}
	if _, ok := state.Positions["ETH"]; !ok {
		t.Errorf("Position should still exist when trigger_px is missing")
	}
}

func TestReconcileHyperliquidPositions_RestingStopLossFillBooksPnL(t *testing.T) {
	state := &StrategyState{
		ID:       "hl-test-eth",
		Platform: "hyperliquid",
		Type:     "perps",
		Cash:     1000,
		Positions: map[string]*Position{
			"ETH": {
				Symbol:            "ETH",
				Quantity:          0.1,
				AvgCost:           3200,
				Side:              "long",
				Multiplier:        1,
				Leverage:          5,
				OwnerStrategyID:   "hl-test-eth",
				OpenedAt:          time.Now().UTC().Add(-time.Hour),
				StopLossOID:       12345,
				StopLossTriggerPx: 3104,
			},
		},
	}
	logger := silentStrategyLogger("hl-test-eth")
	defer logger.Close()

	changed := reconcileHyperliquidPositions(state, "ETH", nil, logger)
	if !changed {
		t.Fatalf("expected reconcile to report a state change")
	}
	if _, ok := state.Positions["ETH"]; ok {
		t.Fatalf("position should be removed after tracked SL fill: %+v", state.Positions["ETH"])
	}
	if len(state.ClosedPositions) != 1 {
		t.Fatalf("ClosedPositions=%d, want 1", len(state.ClosedPositions))
	}
	cp := state.ClosedPositions[0]
	if cp.CloseReason != "stop_loss" {
		t.Errorf("CloseReason=%q, want stop_loss", cp.CloseReason)
	}
	if cp.ClosePrice != 3104 {
		t.Errorf("ClosePrice=%v, want 3104", cp.ClosePrice)
	}
	if cp.RealizedPnL >= 0 {
		t.Errorf("RealizedPnL=%v should be negative for stopped long", cp.RealizedPnL)
	}
	if state.Cash >= 1000 {
		t.Errorf("Cash=%v should decrease by the realized stop loss", state.Cash)
	}
	if len(state.TradeHistory) != 1 || state.TradeHistory[0].Side != "sell" {
		t.Errorf("expected one synthetic sell trade, got %+v", state.TradeHistory)
	}
	if state.RiskState.TotalTrades != 1 || state.RiskState.LosingTrades != 1 {
		t.Errorf("risk stats not updated for SL fill: %+v", state.RiskState)
	}
}

func TestReconcileHyperliquidPositions_RestingStopLossFillClosesShortWithBuy(t *testing.T) {
	state := &StrategyState{
		ID:       "hl-test-eth",
		Platform: "hyperliquid",
		Type:     "perps",
		Cash:     1000,
		Positions: map[string]*Position{
			"ETH": {
				Symbol:            "ETH",
				Quantity:          0.1,
				AvgCost:           3200,
				Side:              "short",
				Multiplier:        1,
				Leverage:          5,
				OwnerStrategyID:   "hl-test-eth",
				OpenedAt:          time.Now().UTC().Add(-time.Hour),
				StopLossOID:       12345,
				StopLossTriggerPx: 3296,
			},
		},
	}
	logger := silentStrategyLogger("hl-test-eth")
	defer logger.Close()

	changed := reconcileHyperliquidPositions(state, "ETH", nil, logger)
	if !changed {
		t.Fatalf("expected reconcile to report a state change")
	}
	if _, ok := state.Positions["ETH"]; ok {
		t.Fatalf("position should be removed after tracked SL fill: %+v", state.Positions["ETH"])
	}
	if len(state.TradeHistory) != 1 {
		t.Fatalf("TradeHistory len=%d, want 1", len(state.TradeHistory))
	}
	trade := state.TradeHistory[0]
	if trade.Side != "buy" {
		t.Errorf("Trade.Side=%q, want buy for stopped short", trade.Side)
	}
	if !trade.IsClose {
		t.Error("Trade.IsClose=false, want true")
	}
	if trade.RealizedPnL >= 0 {
		t.Errorf("RealizedPnL=%v should be negative for stopped short", trade.RealizedPnL)
	}
}

// #421 review point 1: per-strategy circuit-breaker drain must thread
// pos.StopLossOID through to the closer so the resting trigger is
// cancelled before the close fires. Otherwise it sits orphaned on HL's
// book consuming one of the 10/day account-wide trigger slots.
func TestRunPendingHyperliquidCircuitCloses_CancelsStopLossOID(t *testing.T) {
	state := &AppState{
		Strategies: map[string]*StrategyState{
			"hl-a": {
				ID: "hl-a",
				Positions: map[string]*Position{
					"ETH": {Symbol: "ETH", Quantity: 0.5, AvgCost: 3000, Side: "long",
						Multiplier: 1, Leverage: 5, StopLossOID: 99887766},
				},
				RiskState: RiskState{
					PendingCircuitCloses: map[string]*PendingCircuitClose{
						PlatformPendingCloseHyperliquid: {
							Symbols: []PendingCircuitCloseSymbol{{Symbol: "ETH", Size: 0.5}},
						},
					},
				},
			},
		},
	}
	cfg := []StrategyConfig{
		{ID: "hl-a", Platform: "hyperliquid", Type: "perps",
			Args: []string{"sma", "ETH", "1h", "--mode=live"}},
	}
	var mu sync.RWMutex

	var seenCancelOID int64
	closer := func(sym string, partialSz *float64, cancelStopLossOIDs []int64) (*HyperliquidCloseResult, error) {
		seenCancelOID = firstPositiveStopLossOID(cancelStopLossOIDs)
		return &HyperliquidCloseResult{
			Close:                   &HyperliquidClose{Symbol: sym, Fill: &HyperliquidCloseFill{TotalSz: *partialSz, AvgPx: 3000}},
			Platform:                "hyperliquid",
			CancelStopLossSucceeded: seenCancelOID > 0,
		}, nil
	}

	runPendingHyperliquidCircuitCloses(
		context.Background(), state, cfg, "0xabc",
		[]HLPosition{{Coin: "ETH", Size: 0.5, EntryPrice: 3000}}, true,
		nil, closer, 30*time.Second, &mu,
		nil,
	)

	if seenCancelOID != 99887766 {
		t.Errorf("closer received cancelStopLossOID=%d, want 99887766", seenCancelOID)
	}
	// #418: a successful full-fill close now decrements virtual quantity to
	// zero and removes the position via recordClosedPosition. The StopLossOID
	// implicitly travels with the deleted position, so the original assertion
	// (StopLossOID == 0) is replaced with a "position fully closed" check.
	if _, ok := state.Strategies["hl-a"].Positions["ETH"]; ok {
		t.Errorf("ETH position should be removed after full-fill CB close, but it's still present")
	}
}

// #421 review point 1: kill-switch close must thread the per-coin
// StopLossOID map through forceCloseHyperliquidLive so resting SL triggers
// are cancelled along with the close.
func TestForceCloseHyperliquidLive_ThreadsStopLossOIDs(t *testing.T) {
	hlLiveAll := []StrategyConfig{
		{ID: "hl-eth", Platform: "hyperliquid", Type: "perps",
			Args: []string{"sma", "ETH", "1h", "--mode=live"}},
		{ID: "hl-btc", Platform: "hyperliquid", Type: "perps",
			Args: []string{"sma", "BTC", "1h", "--mode=live"}},
	}
	positions := []HLPosition{
		{Coin: "ETH", Size: 0.5, EntryPrice: 3000},
		{Coin: "BTC", Size: 0.01, EntryPrice: 60000},
	}
	slOIDs := map[string][]int64{"ETH": {1111}, "BTC": nil} // BTC has no resting SL

	seen := map[string][]int64{}
	closer := func(sym string, partialSz *float64, cancelStopLossOIDs []int64) (*HyperliquidCloseResult, error) {
		seen[sym] = append([]int64(nil), cancelStopLossOIDs...)
		return &HyperliquidCloseResult{
			Close:    &HyperliquidClose{Symbol: sym, Fill: &HyperliquidCloseFill{TotalSz: 1, AvgPx: 1}},
			Platform: "hyperliquid",
		}, nil
	}

	report := forceCloseHyperliquidLive(context.Background(), positions, hlLiveAll, closer, slOIDs)
	if len(report.Errors) != 0 {
		t.Fatalf("expected no errors, got %v", report.Errors)
	}
	if got := seen["ETH"]; len(got) != 1 || got[0] != 1111 {
		t.Errorf("ETH closer got cancelStopLossOIDs=%v, want [1111]", got)
	}
	if got := seen["BTC"]; len(got) != 0 {
		t.Errorf("BTC closer got cancelStopLossOIDs=%v, want [] (no SL)", got)
	}
}

func TestForceCloseHyperliquidLive_CancelsAllSharedCoinStopLossOIDs(t *testing.T) {
	hlLiveAll := []StrategyConfig{
		{ID: "hl-a-eth", Platform: "hyperliquid", Type: "perps",
			Args: []string{"sma", "ETH", "1h", "--mode=live"}},
		{ID: "hl-b-eth", Platform: "hyperliquid", Type: "perps",
			Args: []string{"ema", "ETH", "1h", "--mode=live"}},
	}
	positions := []HLPosition{{Coin: "ETH", Size: 0.5, EntryPrice: 3000}}
	slOIDs := map[string][]int64{"ETH": {1111, 2222}}

	var calls int
	var seen []int64
	closer := func(sym string, partialSz *float64, cancelStopLossOIDs []int64) (*HyperliquidCloseResult, error) {
		calls++
		seen = append([]int64(nil), cancelStopLossOIDs...)
		return &HyperliquidCloseResult{
			Close:                   &HyperliquidClose{Symbol: sym, Fill: &HyperliquidCloseFill{TotalSz: 1, AvgPx: 1}},
			Platform:                "hyperliquid",
			CancelStopLossSucceeded: len(cancelStopLossOIDs) > 0,
		}, nil
	}

	report := forceCloseHyperliquidLive(context.Background(), positions, hlLiveAll, closer, slOIDs)
	if len(report.Errors) != 0 {
		t.Fatalf("expected no errors, got %v", report.Errors)
	}
	if calls != 1 {
		t.Fatalf("closer calls=%d, want 1 market close for shared ETH", calls)
	}
	if len(seen) != 2 || seen[0] != 1111 || seen[1] != 2222 {
		t.Errorf("closer saw cancel OIDs=%v, want [1111 2222]", seen)
	}
}

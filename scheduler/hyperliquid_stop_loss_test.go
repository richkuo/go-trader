package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"
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
// output parser (which Go CI can run without spawning Python) plus
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
	v := 3.5
	sc := StrategyConfig{
		ID:          "hl-donch-btc",
		Platform:    "hyperliquid",
		Type:        "perps",
		StopLossPct: &v,
	}
	b, err := json.Marshal(sc)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var round StrategyConfig
	if err := json.Unmarshal(b, &round); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if round.StopLossPct == nil || *round.StopLossPct != 3.5 {
		t.Errorf("round-trip StopLossPct: got %v, want 3.5", round.StopLossPct)
	}
	// omitempty check: nil pointer must not emit the field.
	b2, _ := json.Marshal(StrategyConfig{ID: "x", Platform: "hyperliquid", Type: "perps"})
	if containsKey(b2, "stop_loss_pct") {
		t.Errorf("nil StopLossPct should be omitted; got %s", b2)
	}
	// #484: pointer-vs-omitted distinction — explicit 0 must round-trip and
	// re-emit, since it carries the operator's "disabled" semantic.
	zero := 0.0
	scZero := StrategyConfig{ID: "x", Platform: "hyperliquid", Type: "perps", StopLossPct: &zero}
	b3, _ := json.Marshal(scZero)
	if !containsKey(b3, "stop_loss_pct") {
		t.Errorf("explicit zero StopLossPct must be preserved in JSON; got %s", b3)
	}
	var roundZero StrategyConfig
	if err := json.Unmarshal(b3, &roundZero); err != nil {
		t.Fatalf("unmarshal zero: %v", err)
	}
	if roundZero.StopLossPct == nil || *roundZero.StopLossPct != 0 {
		t.Errorf("round-trip explicit-zero StopLossPct: got %v, want 0 (non-nil)", roundZero.StopLossPct)
	}
}

func TestPosition_StopLossOIDJSON(t *testing.T) {
	p := Position{Symbol: "ETH", Quantity: 1, AvgCost: 3000, Side: "long", StopLossOID: 42, StopLossTriggerPx: 2900, StopLossHighWaterPx: 3100}
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
	if round.StopLossHighWaterPx != 3100 {
		t.Errorf("round-trip StopLossHighWaterPx: got %v", round.StopLossHighWaterPx)
	}
	// omitempty: zero should drop from JSON.
	b2, _ := json.Marshal(Position{Symbol: "ETH", Quantity: 1, AvgCost: 3000, Side: "long"})
	if containsKey(b2, "stop_loss_oid") {
		t.Errorf("zero StopLossOID should be omitted; got %s", b2)
	}
	if containsKey(b2, "stop_loss_trigger_px") {
		t.Errorf("zero StopLossTriggerPx should be omitted; got %s", b2)
	}
	if containsKey(b2, "stop_loss_high_water_px") {
		t.Errorf("zero StopLossHighWaterPx should be omitted; got %s", b2)
	}
}

func TestComputeTrailingStopUpdate(t *testing.T) {
	cases := []struct {
		name           string
		side           string
		mark           float64
		highWater      float64
		trailingPct    float64
		minMovePct     float64
		currentTrigger float64
		wantHighWater  float64
		wantTrigger    float64
		wantReplace    bool
	}{
		{"long ratchets on favorable mark", "long", 110, 100, 3, 0.5, 97, 110, 106.7, true},
		{"long high water updates without churn below threshold", "long", 100.4, 100, 3, 0.5, 97, 100.4, 0, false},
		{"long never lowers trigger", "long", 99, 100, 3, 0.5, 97, 100, 0, false},
		{"short ratchets down", "short", 90, 100, 3, 0.5, 103, 90, 92.7, true},
		{"short never raises trigger", "short", 101, 100, 3, 0.5, 103, 100, 0, false},
		{"missing current trigger places one", "long", 100, 100, 3, 0.5, 0, 100, 97, true},
		// #1234 audit: movePct >= minMovePct — a move exactly AT the threshold
		// must replace (regression guard for >= flipped to >). trailingPct=50
		// makes the trigger factor an exact binary 0.5 (long) / 1.5 (short),
		// so movePct computes to an exact 1.0 in float64 (0.5/50*100).
		{"long replaces at exact min-move boundary", "long", 101, 100, 50, 1.0, 50, 101, 50.5, true},
		{"short replaces at exact min-move boundary", "short", 33, 100, 50, 1.0, 50, 33, 49.5, true},
		{"long holds just under min-move boundary", "long", 100.8, 100, 50, 1.0, 50, 100.8, 0, false},
		{"short holds just under min-move boundary", "short", 33.2, 100, 50, 1.0, 50, 33.2, 0, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			gotHighWater, gotTrigger, gotReplace := computeTrailingStopUpdate(c.side, c.mark, c.highWater, c.trailingPct, c.minMovePct, c.currentTrigger)
			if gotHighWater != c.wantHighWater || floatDiff(gotTrigger, c.wantTrigger) > 1e-9 || gotReplace != c.wantReplace {
				t.Fatalf("computeTrailingStopUpdate = (%v, %v, %v), want (%v, %v, %v)",
					gotHighWater, gotTrigger, gotReplace, c.wantHighWater, c.wantTrigger, c.wantReplace)
			}
		})
	}
}

func floatDiff(a, b float64) float64 {
	if a > b {
		return a - b
	}
	return b - a
}

func TestRunHyperliquidTrailingStopUpdate_CancelThenPlaceArgs(t *testing.T) {
	old := runHyperliquidUpdateStopLossFunc
	defer func() { runHyperliquidUpdateStopLossFunc = old }()

	var gotSymbol, gotSide string
	var gotSize, gotTrigger float64
	var gotCancelOID int64
	runHyperliquidUpdateStopLossFunc = func(script, symbol, side string, size, triggerPx float64, cancelStopLossOID int64) (*HyperliquidStopLossUpdateResult, string, error) {
		gotSymbol = symbol
		gotSide = side
		gotSize = size
		gotTrigger = triggerPx
		gotCancelOID = cancelStopLossOID
		return &HyperliquidStopLossUpdateResult{
			StopLossOID:       222,
			StopLossTriggerPx: triggerPx,
		}, "", nil
	}

	trail := 3.0
	minMove := 0.25
	sc := StrategyConfig{ID: "hl-test", Platform: "hyperliquid", Type: "perps", Script: "shared_scripts/check_hyperliquid.py", TrailingStopPct: &trail, TrailingStopMinMovePct: &minMove}
	logger := silentStrategyLogger("hl-test")
	defer logger.Close()

	newHighWater, result, ok := runHyperliquidTrailingStopUpdate(sc, "ETH", "long", 0.5, &Position{AvgCost: 100}, 110, 100, 97, 111, false, nil, logger)
	if !ok {
		t.Fatalf("runHyperliquidTrailingStopUpdate returned ok=false")
	}
	if newHighWater != 110 {
		t.Fatalf("newHighWater=%v, want 110", newHighWater)
	}
	if result == nil || result.StopLossOID != 222 {
		t.Fatalf("result=%+v, want OID 222", result)
	}
	if gotSymbol != "ETH" || gotSide != "long" || gotSize != 0.5 || gotTrigger != 106.7 || gotCancelOID != 111 {
		t.Fatalf("updater args=(%s,%s,%v,%v,%d), want (ETH,long,0.5,106.7,111)",
			gotSymbol, gotSide, gotSize, gotTrigger, gotCancelOID)
	}
}

func TestRunHyperliquidTrailingStopUpdate_RatchetFallbackNormalizeWidensOnce(t *testing.T) {
	old := runHyperliquidUpdateStopLossFunc
	defer func() { runHyperliquidUpdateStopLossFunc = old }()

	trail := 3.0
	minMove := 0.25
	sc := StrategyConfig{
		ID: "hl-test", Platform: "hyperliquid", Type: "manual", Script: "shared_scripts/check_hyperliquid.py",
		CloseStrategy: &StrategyRef{Name: trailingTPRatchetCloseName}, TrailingStopPct: &trail, TrailingStopMinMovePct: &minMove,
	}
	logger := silentStrategyLogger("hl-test")
	defer logger.Close()

	cases := []struct {
		name           string
		side           string
		currentTrigger float64
		wantTrigger    float64
	}{
		{"long fallback too tight", "long", 98, 97},
		{"short fallback too tight", "short", 102, 103},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var called bool
			var gotTrigger float64
			runHyperliquidUpdateStopLossFunc = func(script, symbol, side string, size, triggerPx float64, cancelStopLossOID int64) (*HyperliquidStopLossUpdateResult, string, error) {
				called = true
				gotTrigger = triggerPx
				return &HyperliquidStopLossUpdateResult{StopLossOID: 222, StopLossTriggerPx: triggerPx}, "", nil
			}

			_, result, ok := runHyperliquidTrailingStopUpdate(sc, "ETH", tc.side, 0.5, &Position{AvgCost: 100}, 100, 100, tc.currentTrigger, 111, false, nil, logger)
			if !ok || result != nil || called {
				t.Fatalf("without marker expected no replace: ok=%v result=%+v called=%v", ok, result, called)
			}

			_, result, ok = runHyperliquidTrailingStopUpdate(sc, "ETH", tc.side, 0.5, &Position{AvgCost: 100, RatchetFallbackNormalizePending: true}, 100, 100, tc.currentTrigger, 111, false, nil, logger)
			if !ok || result == nil || !called {
				t.Fatalf("with marker expected widen replace: ok=%v result=%+v called=%v", ok, result, called)
			}
			if !approxEq(gotTrigger, tc.wantTrigger) {
				t.Fatalf("replacement trigger = %v, want %v", gotTrigger, tc.wantTrigger)
			}
		})
	}
}

func TestRunHyperliquidTrailingStopUpdate_DefersOnCancelFailure(t *testing.T) {
	old := runHyperliquidUpdateStopLossFunc
	defer func() { runHyperliquidUpdateStopLossFunc = old }()

	runHyperliquidUpdateStopLossFunc = func(script, symbol, side string, size, triggerPx float64, cancelStopLossOID int64) (*HyperliquidStopLossUpdateResult, string, error) {
		return &HyperliquidStopLossUpdateResult{
			CancelStopLossError: "order not found",
		}, "", nil
	}

	trail := 3.0
	sc := StrategyConfig{ID: "hl-test", Platform: "hyperliquid", Type: "perps", Script: "shared_scripts/check_hyperliquid.py", TrailingStopPct: &trail}
	logger := silentStrategyLogger("hl-test")
	defer logger.Close()
	mock := &mockNotifier{}
	notifier := NewMultiNotifier(notifierBackend{
		notifier: mock,
		channels: map[string]string{"hyperliquid": "chan"},
		ownerID:  "owner",
	})

	newHighWater, result, ok := runHyperliquidTrailingStopUpdate(sc, "ETH", "long", 0.5, &Position{AvgCost: 100}, 110, 100, 97, 111, false, notifier, logger)
	if ok || result == nil {
		t.Fatalf("runHyperliquidTrailingStopUpdate = (%+v, %v), want deferred result", result, ok)
	}
	if newHighWater != 100 {
		t.Fatalf("newHighWater=%v, want unchanged 100", newHighWater)
	}
	if result.StopLossOID != 0 {
		t.Fatalf("StopLossOID=%d, want no replacement", result.StopLossOID)
	}
	if len(mock.messages) != 1 || !strings.Contains(mock.messages[0].content, "old trigger OID 111") || !strings.Contains(mock.messages[0].content, "not replaced") {
		t.Fatalf("broadcast messages=%+v, want deferred cancel-failure alert", mock.messages)
	}
	if len(mock.dms) != 1 || !strings.Contains(mock.dms[0].content, "order not found") {
		t.Fatalf("DMs=%+v, want owner alert with cancel error", mock.dms)
	}
}

func TestRunHyperliquidTrailingStopUpdate_DefersOnOpenOrderCheckFailure(t *testing.T) {
	old := runHyperliquidUpdateStopLossFunc
	defer func() { runHyperliquidUpdateStopLossFunc = old }()

	runHyperliquidUpdateStopLossFunc = func(script, symbol, side string, size, triggerPx float64, cancelStopLossOID int64) (*HyperliquidStopLossUpdateResult, string, error) {
		return &HyperliquidStopLossUpdateResult{OpenOrderCheckError: "indexer down"}, "", nil
	}

	trail := 3.0
	sc := StrategyConfig{ID: "hl-test", Platform: "hyperliquid", Type: "perps", Script: "shared_scripts/check_hyperliquid.py", TrailingStopPct: &trail}
	logger := silentStrategyLogger("hl-test")
	defer logger.Close()

	newHighWater, result, ok := runHyperliquidTrailingStopUpdate(sc, "ETH", "long", 0.5, &Position{AvgCost: 100}, 110, 100, 97, 111, false, nil, logger)
	if ok || result == nil {
		t.Fatalf("runHyperliquidTrailingStopUpdate = (%+v, %v), want deferred result", result, ok)
	}
	if newHighWater != 100 {
		t.Fatalf("newHighWater=%v, want unchanged 100", newHighWater)
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

func TestIsHLOpenOrderCapRejection(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want bool
	}{
		{"too many", "Too many open trigger orders", true},
		{"rate limit", "trigger order rate limit exceeded", true},
		{"max", "max trigger orders per day reached", true},
		{"generic too many open orders", "Too many open orders", true},
		{"generic open orders limit", "open orders limit exceeded", true},
		{"unrelated", "insufficient margin", false},
		{"empty", "", false},
		{"trigger only", "trigger price out of range", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := isHLOpenOrderCapRejection(c.in); got != c.want {
				t.Errorf("isHLOpenOrderCapRejection(%q)=%v, want %v", c.in, got, c.want)
			}
		})
	}
}

func TestConfigValidation_StopLossPctBounds(t *testing.T) {
	// Issue 421 (review point 4): hand-edited configs with out-of-range
	// stop_loss_pct must fail validation rather than silently break the
	// safety feature. Pointer-aware (#484): explicit 0 is the operator
	// opt-out, valid; nil = field omitted (auto-derive path).
	cases := []struct {
		name      string
		pct       float64
		platform  string
		typ       string
		wantError bool
	}{
		{"explicit zero ok (disabled)", 0, "hyperliquid", "perps", false},
		{"in range", 5, "hyperliquid", "perps", false},
		{"max boundary", 50, "hyperliquid", "perps", false},
		{"too high", 200, "hyperliquid", "perps", true},
		{"negative", -1, "hyperliquid", "perps", true},
		{"non-HL platform", 5, "okx", "perps", true},
		{"non-perps type", 5, "hyperliquid", "spot", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			pct := c.pct
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
						StopLossPct:    &pct,
					},
				},
				PortfolioRisk: &PortfolioRiskConfig{MaxDrawdownPct: 25, WarnThresholdPct: 60},
			}
			err := validateConfig(cfg, false)
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
	trades, _ := executeHyperliquidResult(sc, state, result, execResult, "BUY", 3200, nil, nil, logger)

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
	trades, _ := executeHyperliquidResult(sc, state, result, execResult, "BUY", 3200, nil, nil, logger)
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

	// #685: SL-confirmed resolver. Without a userFills hit on the SL OID, the
	// gated fallback now routes to hl_sync_external; production paths always
	// supply a resolver, so tests do the same.
	resolver := hlReconcileFillResolver(func(_ string, oid int64, _ float64) (HLFillLookup, bool) {
		return HLFillLookup{Fee: 0.05, FilledQty: 0.1, Px: 3104, Count: 1, OID: oid}, true
	})
	changed := reconcileHyperliquidPositionsWithResolver(state, "ETH", nil, resolver, logger, nil, nil, StrategyConfig{})
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
	if state.RiskState.ConsecutiveLosses != 1 {
		t.Errorf("consecutive losses not updated for SL fill: %+v", state.RiskState)
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

	// #685: SL-confirmed resolver — see RestingStopLossFillBooksPnL.
	resolver := hlReconcileFillResolver(func(_ string, oid int64, _ float64) (HLFillLookup, bool) {
		return HLFillLookup{Fee: 0.05, FilledQty: 0.1, Px: 3296, Count: 1, OID: oid}, true
	})
	changed := reconcileHyperliquidPositionsWithResolver(state, "ETH", nil, resolver, logger, nil, nil, StrategyConfig{})
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
// book consuming one of the 1000 account-wide open-order slots (#479).
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

// #487/#484: EffectiveStopLossPct returns the price % to send to the HL execute
// helper. Resolution order: explicit StopLossPct → StopLossMarginPct/Leverage →
// MaxDrawdownPct fallback (capped at MaxAutoStopLossPct). Each pointer field
// distinguishes nil (omitted, fall through) from explicit 0 (disabled).
// Non-HL/non-perps strategies always return 0.
func TestEffectiveStopLossPct(t *testing.T) {
	hlPerps := func(sc StrategyConfig) StrategyConfig {
		sc.Platform = "hyperliquid"
		sc.Type = "perps"
		return sc
	}
	pf := func(v float64) *float64 { return &v }
	cases := []struct {
		name string
		sc   StrategyConfig
		want float64
	}{
		{"non-HL returns 0", StrategyConfig{Platform: "okx", Type: "perps", StopLossPct: pf(1.5)}, 0},
		{"non-perps returns 0", StrategyConfig{Platform: "hyperliquid", Type: "spot", StopLossPct: pf(1.5)}, 0},
		{"unset and no drawdown", hlPerps(StrategyConfig{Leverage: 5}), 0},
		{"explicit pct", hlPerps(StrategyConfig{StopLossPct: pf(1.5), Leverage: 5}), 1.5},
		{"trailing pct wins", hlPerps(StrategyConfig{TrailingStopPct: pf(2.5), Leverage: 5}), 2.5},
		{"trailing zero is disabled (no fallback)", hlPerps(StrategyConfig{TrailingStopPct: pf(0), MaxDrawdownPct: 5, Leverage: 5}), 0},
		{"explicit zero is disabled (no fallback)", hlPerps(StrategyConfig{StopLossPct: pf(0), MaxDrawdownPct: 5, Leverage: 5}), 0},
		{"margin pct at 20x", hlPerps(StrategyConfig{StopLossMarginPct: pf(20), Leverage: 20}), 1.0},
		{"margin pct at 10x rescales", hlPerps(StrategyConfig{StopLossMarginPct: pf(20), Leverage: 10}), 2.0},
		{"margin pct without leverage fails safe", hlPerps(StrategyConfig{StopLossMarginPct: pf(20)}), 0},
		{"explicit-zero margin disables (no fallback)", hlPerps(StrategyConfig{StopLossMarginPct: pf(0), MaxDrawdownPct: 7, Leverage: 5}), 0},
		{"explicit wins over margin", hlPerps(StrategyConfig{StopLossPct: pf(3), StopLossMarginPct: pf(20), Leverage: 10}), 3},
		{"trailing wins over explicit before validation", hlPerps(StrategyConfig{TrailingStopPct: pf(4), StopLossPct: pf(3), Leverage: 10}), 4},
		// #484 fallback path.
		{"drawdown fallback when both nil", hlPerps(StrategyConfig{MaxDrawdownPct: 5, Leverage: 5}), 5},
		{"drawdown fallback capped at 50", hlPerps(StrategyConfig{MaxDrawdownPct: 60, Leverage: 5}), 50},
		{"drawdown fallback at cap boundary", hlPerps(StrategyConfig{MaxDrawdownPct: 50, Leverage: 5}), 50},
		{"drawdown fallback ignored when explicit set", hlPerps(StrategyConfig{StopLossPct: pf(2), MaxDrawdownPct: 10}), 2},
		{"margin fallthrough beats drawdown", hlPerps(StrategyConfig{StopLossMarginPct: pf(20), MaxDrawdownPct: 5, Leverage: 20}), 1.0},
		// #1234 audit: a resolved regime-owned stop DEFERS (returns 0) and
		// must never fall through to the max-drawdown fallback — the trigger
		// is armed post-open once EntryATR and Regime are stamped (#733).
		{"stop_loss_atr_regime defers, no drawdown fallback",
			hlPerps(StrategyConfig{StopLossATRRegime: &RegimeATRBlock{TrendRegime: map[string]RegimeATREntry{"trending": {ATR: 2}}}, MaxDrawdownPct: 5, Leverage: 5}), 0},
		{"trailing_stop_atr_regime defers, no drawdown fallback",
			hlPerps(StrategyConfig{TrailingStopATRRegime: &RegimeATRBlock{TrendRegime: map[string]RegimeATREntry{"trending": {ATR: 2}}}, MaxDrawdownPct: 5, Leverage: 5}), 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := EffectiveStopLossPct(c.sc)
			if got != c.want {
				t.Errorf("EffectiveStopLossPct(%+v) = %g, want %g", c.sc, got, c.want)
			}
		})
	}
}

// #487: stop_loss_margin_pct is mutually exclusive with stop_loss_pct, must be
// in (0, 100], and is HL-perps-only. validateConfig must reject every other
// shape so a hand-edited config can't silently disable the SL feature.
func TestConfigValidation_StopLossMarginPctBounds(t *testing.T) {
	cases := []struct {
		name      string
		marginPct float64
		setMargin bool
		pricePct  float64
		setPrice  bool
		leverage  float64
		platform  string
		typ       string
		wantError bool
	}{
		{"explicit zero disables", 0, true, 0, false, 10, "hyperliquid", "perps", false},
		{"in range", 20, true, 0, false, 10, "hyperliquid", "perps", false},
		{"max boundary at 10x leverage", 100, true, 0, false, 10, "hyperliquid", "perps", false},
		{"too high", 150, true, 0, false, 10, "hyperliquid", "perps", true},
		{"negative", -1, true, 0, false, 10, "hyperliquid", "perps", true},
		{"non-HL platform", 20, true, 0, false, 10, "okx", "perps", true},
		{"non-perps type", 20, true, 0, false, 10, "hyperliquid", "spot", true},
		{"mutually exclusive", 20, true, 1, true, 10, "hyperliquid", "perps", true},
		// #484/#487: both fields explicit-zero is benign — both mean "disabled"
		// and neither places a trigger at runtime, so the mutual-exclusion
		// guard must not fire. Operators may end up here after migrating from
		// the legacy float StopLossPct semantics.
		{"both explicit zero is benign", 0, true, 0, true, 10, "hyperliquid", "perps", false},
		// Derived price stop must mirror the #421 [0, 50] cap: at leverage=1
		// a marginPct of 80 implies an 80% price stop, which would land the
		// HL trigger at entry×0 (long) or entry×1.8 (short) and silently
		// never fire.
		{"derived price stop exceeds 50% cap", 80, true, 0, false, 1, "hyperliquid", "perps", true},
		// Edge of the derived cap: marginPct=50 at leverage=1 is exactly 50%
		// and must be accepted (matches the inclusive #421 upper bound).
		{"derived price stop at 50% cap", 50, true, 0, false, 1, "hyperliquid", "perps", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			sc := StrategyConfig{
				ID:             "test",
				Type:           c.typ,
				Platform:       c.platform,
				Script:         "shared_scripts/check_hyperliquid.py",
				Capital:        1000,
				MaxDrawdownPct: 10,
				Leverage:       c.leverage,
			}
			if c.setMargin {
				m := c.marginPct
				sc.StopLossMarginPct = &m
			}
			if c.setPrice {
				p := c.pricePct
				sc.StopLossPct = &p
			}
			cfg := &Config{
				IntervalSeconds: 60,
				Strategies:      []StrategyConfig{sc},
				PortfolioRisk:   &PortfolioRiskConfig{MaxDrawdownPct: 25, WarnThresholdPct: 60},
			}
			err := validateConfig(cfg, false)
			gotErr := err != nil && strings.Contains(err.Error(), "stop_loss")
			if gotErr != c.wantError {
				t.Errorf("got err=%v wantStopLossErr=%v (full err: %v)", gotErr, c.wantError, err)
			}
		})
	}
}

func TestConfigValidation_TrailingStopPctBoundsAndExclusion(t *testing.T) {
	cases := []struct {
		name      string
		trailing  float64
		setFixed  bool
		fixed     float64
		setMargin bool
		margin    float64
		platform  string
		typ       string
		wantError bool
	}{
		{"explicit zero disables", 0, false, 0, false, 0, "hyperliquid", "perps", false},
		{"in range", 3, false, 0, false, 0, "hyperliquid", "perps", false},
		{"max boundary", 50, false, 0, false, 0, "hyperliquid", "perps", false},
		{"too high", 51, false, 0, false, 0, "hyperliquid", "perps", true},
		{"negative", -1, false, 0, false, 0, "hyperliquid", "perps", true},
		{"non-HL platform", 3, false, 0, false, 0, "okx", "perps", true},
		{"non-perps type", 3, false, 0, false, 0, "hyperliquid", "spot", true},
		{"mutually exclusive fixed", 3, true, 2, false, 0, "hyperliquid", "perps", true},
		{"mutually exclusive margin", 3, false, 0, true, 20, "hyperliquid", "perps", true},
		{"fixed zero benign", 3, true, 0, false, 0, "hyperliquid", "perps", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			trailing := c.trailing
			sc := StrategyConfig{
				ID:              "test",
				Type:            c.typ,
				Platform:        c.platform,
				Script:          "shared_scripts/check_hyperliquid.py",
				Capital:         1000,
				MaxDrawdownPct:  10,
				Leverage:        10,
				TrailingStopPct: &trailing,
			}
			if c.setFixed {
				fixed := c.fixed
				sc.StopLossPct = &fixed
			}
			if c.setMargin {
				margin := c.margin
				sc.StopLossMarginPct = &margin
			}
			cfg := &Config{
				IntervalSeconds: 60,
				Strategies:      []StrategyConfig{sc},
				PortfolioRisk:   &PortfolioRiskConfig{MaxDrawdownPct: 25, WarnThresholdPct: 60},
			}
			err := validateConfig(cfg, false)
			gotErr := err != nil && strings.Contains(err.Error(), "trailing_stop_pct")
			if gotErr != c.wantError {
				t.Errorf("got err=%v wantTrailingErr=%v (full err: %v)", gotErr, c.wantError, err)
			}
		})
	}
}

func TestConfigValidation_TrailingStopMinMovePct(t *testing.T) {
	cases := []struct {
		name        string
		minMove     float64
		trailingPct float64
		platform    string
		typ         string
		wantError   bool
	}{
		{"zero allowed", 0, 3, "hyperliquid", "perps", false},
		{"in range", 0.25, 3, "hyperliquid", "perps", false},
		{"max boundary", 100, 3, "hyperliquid", "perps", false},
		{"negative", -0.1, 3, "hyperliquid", "perps", true},
		{"too high", 101, 3, "hyperliquid", "perps", true},
		{"requires trailing", 0.5, 0, "hyperliquid", "perps", true},
		{"non-HL platform", 0.5, 3, "okx", "perps", true},
		{"non-perps type", 0.5, 3, "hyperliquid", "spot", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			trailing := c.trailingPct
			minMove := c.minMove
			cfg := &Config{
				IntervalSeconds: 60,
				Strategies: []StrategyConfig{{
					ID:                     "test",
					Type:                   c.typ,
					Platform:               c.platform,
					Script:                 "shared_scripts/check_hyperliquid.py",
					Capital:                1000,
					MaxDrawdownPct:         10,
					Leverage:               10,
					TrailingStopPct:        &trailing,
					TrailingStopMinMovePct: &minMove,
				}},
				PortfolioRisk: &PortfolioRiskConfig{MaxDrawdownPct: 25, WarnThresholdPct: 60},
			}
			err := validateConfig(cfg, false)
			gotErr := err != nil && strings.Contains(err.Error(), "trailing_stop_min_move_pct")
			if gotErr != c.wantError {
				t.Errorf("got err=%v wantMinMoveErr=%v (full err: %v)", gotErr, c.wantError, err)
			}
		})
	}
}

func TestConfigValidation_HLPeersTrailingAndFixedStopLossAllowed(t *testing.T) {
	trailing := 3.0
	fixed := 2.0
	cfg := &Config{
		IntervalSeconds: 60,
		Strategies: []StrategyConfig{
			{
				ID:              "hl-eth-trend",
				Type:            "perps",
				Platform:        "hyperliquid",
				Script:          "shared_scripts/check_hyperliquid.py",
				Args:            []string{"trend", "ETH", "1h", "--mode=paper"},
				Capital:         1000,
				MaxDrawdownPct:  10,
				Leverage:        10,
				MarginMode:      "isolated",
				TrailingStopPct: &trailing,
			},
			{
				ID:             "hl-eth-breakout",
				Type:           "perps",
				Platform:       "hyperliquid",
				Script:         "shared_scripts/check_hyperliquid.py",
				Args:           []string{"breakout", "ETH", "1h", "--mode=paper"},
				Capital:        1000,
				MaxDrawdownPct: 10,
				Leverage:       10,
				MarginMode:     "isolated",
				StopLossPct:    &fixed,
			},
		},
		PortfolioRisk: &PortfolioRiskConfig{MaxDrawdownPct: 25, WarnThresholdPct: 60},
	}
	err := validateConfig(cfg, false)
	if err != nil {
		t.Fatalf("validateConfig failed: %v", err)
	}
}

// #487: zero StopLossMarginPct must be omitted from the JSON encoding so
// existing configs don't grow a noisy field after a round-trip.
func TestStrategyConfig_StopLossMarginPctJSON(t *testing.T) {
	v := 25.0
	sc := StrategyConfig{
		ID:                "hl-test",
		Type:              "perps",
		Platform:          "hyperliquid",
		Leverage:          20,
		StopLossMarginPct: &v,
	}
	b, err := json.Marshal(sc)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(b), `"stop_loss_margin_pct":25`) {
		t.Errorf("expected stop_loss_margin_pct in JSON; got %s", b)
	}
	var round StrategyConfig
	if err := json.Unmarshal(b, &round); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if round.StopLossMarginPct == nil || *round.StopLossMarginPct != 25 {
		t.Errorf("round-trip StopLossMarginPct: got %v, want 25", round.StopLossMarginPct)
	}

	// nil pointer (omitted) must not emit the field — operator hasn't opted
	// in or out, auto-derive path applies.
	sc.StopLossMarginPct = nil
	b2, err := json.Marshal(sc)
	if err != nil {
		t.Fatalf("marshal nil: %v", err)
	}
	if strings.Contains(string(b2), "stop_loss_margin_pct") {
		t.Errorf("nil StopLossMarginPct should be omitted; got %s", b2)
	}

	// #484: explicit zero is the "operator opt-out" semantic and must be
	// preserved in JSON so a config round-trip doesn't silently re-enable
	// the auto-SL fallback.
	zero := 0.0
	sc.StopLossMarginPct = &zero
	b3, err := json.Marshal(sc)
	if err != nil {
		t.Fatalf("marshal zero: %v", err)
	}
	if !strings.Contains(string(b3), `"stop_loss_margin_pct":0`) {
		t.Errorf("explicit zero StopLossMarginPct must round-trip; got %s", b3)
	}
}

// #505: TrailingStopATRMult derives the trailing distance from the entry ATR
// and avg cost of the open position. Once derived the percentage is fixed for
// the life of the position. effectiveTrailingStopPct must:
//   - return 0 (no-op) when EntryATR or AvgCost is zero so the initial-trigger
//     placement is deferred to the cycle after stampEntryATRIfOpened populates
//     the position rather than crashing or arming with bogus distance,
//   - return mult * entry_atr / avg_cost * 100 once both are set,
//   - prefer an explicit fixed TrailingStopPct over the ATR multiplier when
//     both are present (validation rejects this combo at config-load time but
//     the helper must still resolve deterministically),
//   - stay HL-perps-only.
func TestEffectiveTrailingStopPct_ATRMult(t *testing.T) {
	pf := func(v float64) *float64 { return &v }
	hl := func(sc StrategyConfig) StrategyConfig {
		sc.Platform = "hyperliquid"
		sc.Type = "perps"
		return sc
	}
	cases := []struct {
		name string
		sc   StrategyConfig
		pos  *Position
		want float64
	}{
		{"non-HL returns 0", StrategyConfig{Platform: "okx", Type: "perps", TrailingStopATRMult: pf(1)}, &Position{AvgCost: 100, EntryATR: 2}, 0},
		{"non-perps returns 0", StrategyConfig{Platform: "hyperliquid", Type: "spot", TrailingStopATRMult: pf(1)}, &Position{AvgCost: 100, EntryATR: 2}, 0},
		{"nil position returns 0", hl(StrategyConfig{TrailingStopATRMult: pf(1.5)}), nil, 0},
		{"zero EntryATR returns 0", hl(StrategyConfig{TrailingStopATRMult: pf(1.5)}), &Position{AvgCost: 100, EntryATR: 0}, 0},
		{"zero AvgCost returns 0", hl(StrategyConfig{TrailingStopATRMult: pf(1.5)}), &Position{AvgCost: 0, EntryATR: 2}, 0},
		{"explicit zero mult disabled", hl(StrategyConfig{TrailingStopATRMult: pf(0)}), &Position{AvgCost: 100, EntryATR: 2}, 0},
		{"derives 3% at mult=1.5 atr=2 cost=100", hl(StrategyConfig{TrailingStopATRMult: pf(1.5)}), &Position{AvgCost: 100, EntryATR: 2}, 3.0},
		{"derives 5% at mult=2 atr=1 cost=40", hl(StrategyConfig{TrailingStopATRMult: pf(2)}), &Position{AvgCost: 40, EntryATR: 1}, 5.0},
		{"fixed pct wins over ATR mult", hl(StrategyConfig{TrailingStopPct: pf(2.5), TrailingStopATRMult: pf(99)}), &Position{AvgCost: 100, EntryATR: 50}, 2.5},
		{"fixed pct zero disables before ATR fallback", hl(StrategyConfig{TrailingStopPct: pf(0), TrailingStopATRMult: pf(1.5)}), &Position{AvgCost: 100, EntryATR: 2}, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := effectiveTrailingStopPct(c.sc, c.pos)
			// Compare with a small epsilon to keep the table values readable.
			if d := got - c.want; d > 1e-9 || d < -1e-9 {
				t.Errorf("effectiveTrailingStopPct = %g, want %g", got, c.want)
			}
		})
	}
}

// #505: ATR-derived trailing stops must not arm at order-placement time
// because EntryATR is stamped on the Position only after the fill. Until
// EntryATR exists, EffectiveStopLossPct must return 0 so the live execute
// path skips the initial trigger and the trailing loop arms it on the next
// cycle.
func TestEffectiveStopLossPct_TrailingATRMultDefersInitialTrigger(t *testing.T) {
	pf := func(v float64) *float64 { return &v }
	mult := pf(1.5)
	sc := StrategyConfig{
		Platform:            "hyperliquid",
		Type:                "perps",
		Leverage:            10,
		MaxDrawdownPct:      5, // would otherwise fall through to a 5% auto stop
		TrailingStopATRMult: mult,
	}
	if got := EffectiveStopLossPct(sc); got != 0 {
		t.Errorf("EffectiveStopLossPct with TrailingStopATRMult set = %g, want 0 (deferred to trailing loop)", got)
	}
}

// #505: trailing_stop_atr_mult shape validation. Acceptance criteria:
//   - HL perps only.
//   - mutually exclusive with trailing_stop_pct, stop_loss_pct, and
//     stop_loss_margin_pct (each conflict surfaces a trailing_stop_atr_mult
//     error string).
//   - negative values rejected; zero is a benign opt-out.
func TestConfigValidation_TrailingStopATRMult(t *testing.T) {
	pf := func(v float64) *float64 { return &v }
	cases := []struct {
		name      string
		sc        StrategyConfig
		wantError bool
	}{
		{"in range", StrategyConfig{
			ID: "hl-test", Type: "perps", Platform: "hyperliquid",
			Script: "shared_scripts/check_hyperliquid.py", Capital: 1000, MaxDrawdownPct: 10, Leverage: 5,
			TrailingStopATRMult: pf(1.5),
		}, false},
		{"explicit zero disables (benign)", StrategyConfig{
			ID: "hl-test", Type: "perps", Platform: "hyperliquid",
			Script: "shared_scripts/check_hyperliquid.py", Capital: 1000, MaxDrawdownPct: 10, Leverage: 5,
			TrailingStopATRMult: pf(0),
		}, false},
		{"negative rejected", StrategyConfig{
			ID: "hl-test", Type: "perps", Platform: "hyperliquid",
			Script: "shared_scripts/check_hyperliquid.py", Capital: 1000, MaxDrawdownPct: 10, Leverage: 5,
			TrailingStopATRMult: pf(-0.5),
		}, true},
		{"non-HL platform rejected", StrategyConfig{
			ID: "ok-test", Type: "perps", Platform: "okx",
			Script: "shared_scripts/check_okx.py", Capital: 1000, MaxDrawdownPct: 10, Leverage: 5,
			TrailingStopATRMult: pf(1.5),
		}, true},
		{"non-perps type rejected", StrategyConfig{
			ID: "hl-test", Type: "spot", Platform: "hyperliquid",
			Script: "shared_scripts/check_hyperliquid.py", Capital: 1000, MaxDrawdownPct: 10,
			TrailingStopATRMult: pf(1.5),
		}, true},
		{"mutually exclusive with trailing_stop_pct", StrategyConfig{
			ID: "hl-test", Type: "perps", Platform: "hyperliquid",
			Script: "shared_scripts/check_hyperliquid.py", Capital: 1000, MaxDrawdownPct: 10, Leverage: 5,
			TrailingStopATRMult: pf(1.5), TrailingStopPct: pf(2),
		}, true},
		{"mutually exclusive with stop_loss_pct", StrategyConfig{
			ID: "hl-test", Type: "perps", Platform: "hyperliquid",
			Script: "shared_scripts/check_hyperliquid.py", Capital: 1000, MaxDrawdownPct: 10, Leverage: 5,
			TrailingStopATRMult: pf(1.5), StopLossPct: pf(2),
		}, true},
		{"mutually exclusive with stop_loss_margin_pct", StrategyConfig{
			ID: "hl-test", Type: "perps", Platform: "hyperliquid",
			Script: "shared_scripts/check_hyperliquid.py", Capital: 1000, MaxDrawdownPct: 10, Leverage: 5,
			TrailingStopATRMult: pf(1.5), StopLossMarginPct: pf(20),
		}, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cfg := &Config{
				IntervalSeconds: 60,
				Strategies:      []StrategyConfig{c.sc},
				PortfolioRisk:   &PortfolioRiskConfig{MaxDrawdownPct: 25, WarnThresholdPct: 60},
			}
			err := validateConfig(cfg, false)
			gotErr := err != nil && strings.Contains(err.Error(), "trailing_stop_atr_mult")
			if gotErr != c.wantError {
				t.Errorf("got err=%v wantATRMultErr=%v (full err: %v)", gotErr, c.wantError, err)
			}
		})
	}
}

// #601: peer stop ownership is allowed because protection orders are sized per
// strategy.
func TestConfigValidation_HLPeersATRTrailingAllowed(t *testing.T) {
	mult := 1.5
	fixed := 2.0
	cfg := &Config{
		IntervalSeconds: 60,
		Strategies: []StrategyConfig{
			{
				ID:                  "hl-eth-trend",
				Type:                "perps",
				Platform:            "hyperliquid",
				Script:              "shared_scripts/check_hyperliquid.py",
				Args:                []string{"trend", "ETH", "1h", "--mode=paper"},
				Capital:             1000,
				MaxDrawdownPct:      10,
				Leverage:            10,
				MarginMode:          "isolated",
				TrailingStopATRMult: &mult,
			},
			{
				ID:             "hl-eth-breakout",
				Type:           "perps",
				Platform:       "hyperliquid",
				Script:         "shared_scripts/check_hyperliquid.py",
				Args:           []string{"breakout", "ETH", "1h", "--mode=paper"},
				Capital:        1000,
				MaxDrawdownPct: 10,
				Leverage:       10,
				MarginMode:     "isolated",
				StopLossPct:    &fixed,
			},
		},
		PortfolioRisk: &PortfolioRiskConfig{MaxDrawdownPct: 25, WarnThresholdPct: 60},
	}
	err := validateConfig(cfg, false)
	if err != nil {
		t.Fatalf("validateConfig failed: %v", err)
	}
}

// #601: peer normalization is now a no-op; shared-coin peers keep normal
// stop-loss defaulting because protection orders are sized per strategy.
func TestNormalizeHyperliquidPeerStopLosses_TrailingATRMultOwnerNoop(t *testing.T) {
	mult := 1.5
	strategies := []StrategyConfig{
		{
			ID:                  "hl-eth-trend",
			Type:                "perps",
			Platform:            "hyperliquid",
			Args:                []string{"trend", "ETH", "1h"},
			Leverage:            5,
			MaxDrawdownPct:      10,
			TrailingStopATRMult: &mult,
		},
		{
			ID:             "hl-eth-breakout",
			Type:           "perps",
			Platform:       "hyperliquid",
			Args:           []string{"breakout", "ETH", "1h"},
			Leverage:       5,
			MaxDrawdownPct: 10,
		},
	}
	normalizeHyperliquidPeerStopLosses(strategies)

	if strategies[0].StopLossPct != nil {
		t.Errorf("ATR-mult owner should not gain a normalized StopLossPct; got %v", strategies[0].StopLossPct)
	}
	if strategies[1].StopLossPct != nil {
		t.Fatalf("peer StopLossPct = %v, want nil", strategies[1].StopLossPct)
	}
}

// #505: trailing_stop_atr_mult round-trips through JSON only when explicit
// (omitempty drops nil) and is a hot-reloadable field via formatFloatPtr.
func TestStrategyConfig_TrailingStopATRMultJSON(t *testing.T) {
	v := 1.5
	sc := StrategyConfig{
		ID:                  "hl-test",
		Type:                "perps",
		Platform:            "hyperliquid",
		Leverage:            10,
		TrailingStopATRMult: &v,
	}
	b, err := json.Marshal(sc)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(b), `"trailing_stop_atr_mult":1.5`) {
		t.Errorf("expected trailing_stop_atr_mult in JSON; got %s", b)
	}

	sc.TrailingStopATRMult = nil
	b2, err := json.Marshal(sc)
	if err != nil {
		t.Fatalf("marshal nil: %v", err)
	}
	if strings.Contains(string(b2), "trailing_stop_atr_mult") {
		t.Errorf("nil TrailingStopATRMult should be omitted; got %s", b2)
	}

	zero := 0.0
	sc.TrailingStopATRMult = &zero
	b3, err := json.Marshal(sc)
	if err != nil {
		t.Fatalf("marshal zero: %v", err)
	}
	if !strings.Contains(string(b3), `"trailing_stop_atr_mult":0`) {
		t.Errorf("explicit zero TrailingStopATRMult must round-trip; got %s", b3)
	}
}

// #505 review: a volatile coin (e.g. mult=3 with ATR ≈ 30% of price) would
// otherwise produce a derived 90% trailing distance and a long-side trigger
// price <= 0 that HL silently rejects. effectiveTrailingStopPct must clamp the
// derived percentage to MaxAutoStopLossPct (50) to mirror the cap on the other
// auto-derive paths in EffectiveStopLossPct.
func TestEffectiveTrailingStopPct_ATRMultCappedAtMaxAutoStopLossPct(t *testing.T) {
	pf := func(v float64) *float64 { return &v }
	sc := StrategyConfig{
		Platform:            "hyperliquid",
		Type:                "perps",
		TrailingStopATRMult: pf(3),
	}
	pos := &Position{AvgCost: 100, EntryATR: 30} // raw derived = 3 * 30 / 100 * 100 = 90%
	got := effectiveTrailingStopPct(sc, pos)
	if got != MaxAutoStopLossPct {
		t.Errorf("effectiveTrailingStopPct = %g, want %g (capped at MaxAutoStopLossPct)", got, MaxAutoStopLossPct)
	}

	// Just under the cap stays exactly the derived value.
	sc.TrailingStopATRMult = pf(1.5)
	pos = &Position{AvgCost: 100, EntryATR: 20} // raw derived = 30%
	got = effectiveTrailingStopPct(sc, pos)
	if d := got - 30.0; d > 1e-9 || d < -1e-9 {
		t.Errorf("effectiveTrailingStopPct = %g, want 30 (under cap, no clamp)", got)
	}
}

// #505 review: explicit-zero TrailingStopATRMult must fall through to the
// next priority instead of short-circuiting EffectiveStopLossPct. A config
// like {trailing_stop_atr_mult: 0, stop_loss_pct: 2} passes validation
// (mutex check skips when ATR mult == 0) and the explicit fixed stop should
// still arm the on-chain trigger.
func TestEffectiveStopLossPct_ATRMultExplicitZeroFallsThrough(t *testing.T) {
	pf := func(v float64) *float64 { return &v }
	sc := StrategyConfig{
		Platform:            "hyperliquid",
		Type:                "perps",
		Leverage:            5,
		MaxDrawdownPct:      10,
		TrailingStopATRMult: pf(0),
		StopLossPct:         pf(2),
	}
	if got := EffectiveStopLossPct(sc); got != 2 {
		t.Errorf("EffectiveStopLossPct with ATR mult=0 + stop_loss_pct=2 = %g, want 2 (fall through)", got)
	}

	// And with no other field set, mult=0 falls through to MaxDrawdownPct.
	sc.StopLossPct = nil
	sc.MaxDrawdownPct = 8
	if got := EffectiveStopLossPct(sc); got != 8 {
		t.Errorf("EffectiveStopLossPct with ATR mult=0 + MaxDrawdownPct=8 = %g, want 8 (fall through to DD)", got)
	}
}

// #505 review: atrMultMissingEntryATR detects the silent foot-gun where an
// ATR-mult-configured strategy opens a position but the entry candle did not
// produce an ATR indicator, so EntryATR stays 0 and the trailing loop never
// arms an on-chain trigger.
func TestATRMultMissingEntryATR(t *testing.T) {
	pf := func(v float64) *float64 { return &v }
	hl := func(sc StrategyConfig) StrategyConfig {
		sc.Platform = "hyperliquid"
		sc.Type = "perps"
		return sc
	}
	cases := []struct {
		name string
		sc   StrategyConfig
		pos  *Position
		want bool
	}{
		{"non-HL platform", StrategyConfig{Platform: "okx", Type: "perps", TrailingStopATRMult: pf(1.5)}, &Position{AvgCost: 100, EntryATR: 0}, false},
		{"non-perps type", StrategyConfig{Platform: "hyperliquid", Type: "spot", TrailingStopATRMult: pf(1.5)}, &Position{AvgCost: 100, EntryATR: 0}, false},
		{"ATR mult unset", hl(StrategyConfig{}), &Position{AvgCost: 100, EntryATR: 0}, false},
		{"ATR mult explicit zero", hl(StrategyConfig{TrailingStopATRMult: pf(0)}), &Position{AvgCost: 100, EntryATR: 0}, false},
		{"fixed pct wins", hl(StrategyConfig{TrailingStopPct: pf(3), TrailingStopATRMult: pf(1.5)}), &Position{AvgCost: 100, EntryATR: 0}, false},
		{"nil position", hl(StrategyConfig{TrailingStopATRMult: pf(1.5)}), nil, false},
		{"EntryATR stamped", hl(StrategyConfig{TrailingStopATRMult: pf(1.5)}), &Position{AvgCost: 100, EntryATR: 2}, false},
		{"missing EntryATR", hl(StrategyConfig{TrailingStopATRMult: pf(1.5)}), &Position{AvgCost: 100, EntryATR: 0}, true},
		{"missing AvgCost", hl(StrategyConfig{TrailingStopATRMult: pf(1.5)}), &Position{AvgCost: 0, EntryATR: 2}, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := atrMultMissingEntryATR(c.sc, c.pos); got != c.want {
				t.Errorf("atrMultMissingEntryATR = %v, want %v", got, c.want)
			}
		})
	}
}

// #505 review: notifyATRMultMissingEntryATROnce must emit exactly one
// alert per (strategy, symbol). Repeated cycles must be suppressed so the
// alert channel is not flooded; clearATRMultMissingEntryATRWarning resets
// the throttle for re-opens.
func TestNotifyATRMultMissingEntryATROnce_ThrottlesPerStrategySymbol(t *testing.T) {
	mock := &mockNotifier{}
	notifier := NewMultiNotifier(notifierBackend{
		notifier: mock,
		channels: map[string]string{"hyperliquid": "chan"},
		ownerID:  "owner",
	})
	logger := silentStrategyLogger("hl-test")
	defer logger.Close()
	sc := StrategyConfig{ID: "hl-test", Platform: "hyperliquid", Type: "perps"}

	// Reset between subtests so other tests don't leak warning state.
	defer clearATRMultMissingEntryATRWarning(sc.ID, "ETH")
	defer clearATRMultMissingEntryATRWarning(sc.ID, "BTC")

	notifyATRMultMissingEntryATROnce(sc, "ETH", notifier, logger)
	notifyATRMultMissingEntryATROnce(sc, "ETH", notifier, logger)
	notifyATRMultMissingEntryATROnce(sc, "ETH", notifier, logger)

	if got := len(mock.messages); got != 1 {
		t.Errorf("expected 1 broadcast for ETH, got %d (%+v)", got, mock.messages)
	}
	if got := len(mock.dms); got != 1 {
		t.Errorf("expected 1 owner DM for ETH, got %d (%+v)", got, mock.dms)
	}
	if len(mock.messages) > 0 && !strings.Contains(mock.messages[0].content, "MISSING ENTRY ATR") {
		t.Errorf("alert content missing MISSING ENTRY ATR phrase: %q", mock.messages[0].content)
	}

	// A different symbol on the same strategy must alert independently.
	notifyATRMultMissingEntryATROnce(sc, "BTC", notifier, logger)
	if got := len(mock.messages); got != 2 {
		t.Errorf("expected 2 broadcasts after BTC alert, got %d", got)
	}

	// Clearing the throttle re-arms the alert.
	clearATRMultMissingEntryATRWarning(sc.ID, "ETH")
	notifyATRMultMissingEntryATROnce(sc, "ETH", notifier, logger)
	if got := len(mock.messages); got != 3 {
		t.Errorf("expected 3 broadcasts after clear+re-alert, got %d", got)
	}
}

// #505 review follow-up: clearATRMultMissingEntryATRWarningOnHLPerpsClose
// is the production-path shortcut wired into HL perps close sites
// (recordPerpsStopLossClose, ExecutePerpsSignalWithLeverage close-long/short,
// forceCloseAllPositions, hyperliquid_balance circuit-breaker close). It
// must clear the throttle for HL perps and no-op for any other state, so
// non-HL strategy closes don't accidentally drop a peer's throttle key.
func TestClearATRMultMissingEntryATRWarningOnHLPerpsClose(t *testing.T) {
	defer clearATRMultMissingEntryATRWarning("hl-test", "ETH")
	defer clearATRMultMissingEntryATRWarning("spot-test", "ETH")

	atrMultMissingEntryATRWarned.Store(atrMultMissingEntryATRKey("hl-test", "ETH"), struct{}{})
	atrMultMissingEntryATRWarned.Store(atrMultMissingEntryATRKey("spot-test", "ETH"), struct{}{})

	// Nil state must be safe.
	clearATRMultMissingEntryATRWarningOnHLPerpsClose(nil, "ETH")
	if _, ok := atrMultMissingEntryATRWarned.Load(atrMultMissingEntryATRKey("hl-test", "ETH")); !ok {
		t.Fatalf("nil state should not have cleared HL key")
	}

	// Non-HL platform must not clear anything.
	spotState := &StrategyState{ID: "spot-test", Platform: "binanceus", Type: "spot"}
	clearATRMultMissingEntryATRWarningOnHLPerpsClose(spotState, "ETH")
	if _, ok := atrMultMissingEntryATRWarned.Load(atrMultMissingEntryATRKey("spot-test", "ETH")); !ok {
		t.Fatalf("non-HL close should not have cleared spot-test key")
	}

	// HL spot must not clear (the throttle only fires for HL perps).
	hlSpot := &StrategyState{ID: "hl-test", Platform: "hyperliquid", Type: "spot"}
	clearATRMultMissingEntryATRWarningOnHLPerpsClose(hlSpot, "ETH")
	if _, ok := atrMultMissingEntryATRWarned.Load(atrMultMissingEntryATRKey("hl-test", "ETH")); !ok {
		t.Fatalf("HL-spot close should not have cleared HL-perps key")
	}

	// HL perps clears the matching key.
	hlPerps := &StrategyState{ID: "hl-test", Platform: "hyperliquid", Type: "perps"}
	clearATRMultMissingEntryATRWarningOnHLPerpsClose(hlPerps, "ETH")
	if _, ok := atrMultMissingEntryATRWarned.Load(atrMultMissingEntryATRKey("hl-test", "ETH")); ok {
		t.Fatalf("HL perps close should have cleared the throttle key")
	}
}

// #505 review follow-up: clearATRMultMissingEntryATRWarningsForStrategy is
// invoked from the hot-reload disable path. It must drop every key for the
// target strategy ID and leave other strategies' keys untouched (including
// strategies whose IDs share a common prefix).
func TestClearATRMultMissingEntryATRWarningsForStrategy(t *testing.T) {
	keys := []struct{ strategyID, symbol string }{
		{"hl-momo", "ETH"},
		{"hl-momo", "BTC"},
		{"hl-momo-fast", "ETH"}, // share prefix; must NOT be cleared
		{"hl-other", "ETH"},
	}
	for _, k := range keys {
		atrMultMissingEntryATRWarned.Store(atrMultMissingEntryATRKey(k.strategyID, k.symbol), struct{}{})
		defer clearATRMultMissingEntryATRWarning(k.strategyID, k.symbol)
	}

	clearATRMultMissingEntryATRWarningsForStrategy("hl-momo")

	for _, k := range keys {
		_, ok := atrMultMissingEntryATRWarned.Load(atrMultMissingEntryATRKey(k.strategyID, k.symbol))
		shouldRemain := k.strategyID != "hl-momo"
		if ok != shouldRemain {
			t.Errorf("after clearing hl-momo: key %s:%s present=%v want present=%v",
				k.strategyID, k.symbol, ok, shouldRemain)
		}
	}
}

// #522: tieredTPATRMissingEntryATR detects open positions with EntryATR == 0
// when tiered_tp_atr is in close_strategies (platform-agnostic).
func TestTieredTPATRMissingEntryATR(t *testing.T) {
	// #842: a strategy has a single close; withCS takes 0 or 1 close name.
	withCS := func(name string) StrategyConfig {
		sc := StrategyConfig{Platform: "hyperliquid", Type: "perps"}
		if name != "" {
			sc.CloseStrategy = &StrategyRef{Name: name}
		}
		return sc
	}
	cases := []struct {
		name string
		sc   StrategyConfig
		pos  *Position
		want bool
	}{
		{"no close strategies", withCS(""), &Position{AvgCost: 100, EntryATR: 0}, false},
		{"different close strategy", withCS("tp_at_pct"), &Position{AvgCost: 100, EntryATR: 0}, false},
		{"tiered_tp_atr present, EntryATR missing", withCS("tiered_tp_atr"), &Position{AvgCost: 100, EntryATR: 0}, true},
		{"tiered_tp_atr present, EntryATR stamped", withCS("tiered_tp_atr"), &Position{AvgCost: 100, EntryATR: 5}, false},
		{"tiered_tp_atr present, no open position (AvgCost==0)", withCS("tiered_tp_atr"), &Position{AvgCost: 0, EntryATR: 0}, false},
		{"nil position", withCS("tiered_tp_atr"), nil, false},
		{"works on non-HL platform", StrategyConfig{Platform: "binanceus", Type: "spot", CloseStrategy: &StrategyRef{Name: "tiered_tp_atr"}}, &Position{AvgCost: 100, EntryATR: 0}, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := tieredTPATRMissingEntryATR(c.sc, c.pos); got != c.want {
				t.Errorf("tieredTPATRMissingEntryATR = %v, want %v", got, c.want)
			}
		})
	}
}

// #522: notifyTieredTPATRMissingEntryATROnce throttles alerts per (strategy,
// symbol) and shares the throttle map with the ATR-mult path so a single
// strategy that triggers both variants only emits one alert.
func TestNotifyTieredTPATRMissingEntryATROnce_ThrottlesAndShares(t *testing.T) {
	mock := &mockNotifier{}
	notifier := NewMultiNotifier(notifierBackend{
		notifier: mock,
		channels: map[string]string{"hyperliquid": "chan"},
		ownerID:  "owner",
	})
	logger := silentStrategyLogger("hl-tiered-test")
	defer logger.Close()
	sc := StrategyConfig{ID: "hl-tiered-test", Platform: "hyperliquid", Type: "perps",
		CloseStrategy: &StrategyRef{Name: "tiered_tp_atr"}}

	defer clearATRMultMissingEntryATRWarning(sc.ID, "ETH")
	defer clearATRMultMissingEntryATRWarning(sc.ID, "BTC")

	notifyTieredTPATRMissingEntryATROnce(sc, "ETH", notifier, logger)
	notifyTieredTPATRMissingEntryATROnce(sc, "ETH", notifier, logger)
	notifyTieredTPATRMissingEntryATROnce(sc, "ETH", notifier, logger)

	if got := len(mock.messages); got != 1 {
		t.Errorf("expected 1 broadcast for ETH, got %d", got)
	}
	if got := len(mock.dms); got != 1 {
		t.Errorf("expected 1 owner DM for ETH, got %d", got)
	}
	if len(mock.messages) > 0 && !strings.Contains(mock.messages[0].content, "tiered_tp_atr") {
		t.Errorf("alert content missing tiered_tp_atr: %q", mock.messages[0].content)
	}

	// A different symbol alerts independently.
	notifyTieredTPATRMissingEntryATROnce(sc, "BTC", notifier, logger)
	if got := len(mock.messages); got != 2 {
		t.Errorf("expected 2 broadcasts after BTC alert, got %d", got)
	}

	// ATR-mult notifier on the same (strategy, symbol) is suppressed because the
	// throttle map key is shared — one alert per (strategy, symbol) regardless of
	// which variant fires first.
	clearATRMultMissingEntryATRWarning(sc.ID, "ETH")
	notifyATRMultMissingEntryATROnce(sc, "ETH", notifier, logger)
	if got := len(mock.messages); got != 3 {
		t.Errorf("expected 3 broadcasts after atr-mult alert, got %d", got)
	}
	notifyTieredTPATRMissingEntryATROnce(sc, "ETH", notifier, logger)
	if got := len(mock.messages); got != 3 {
		t.Errorf("tiered alert after atr-mult should be suppressed (shared throttle), got %d", got)
	}
}

// #532: trailingStopBreached reports whether the current mark has crossed the
// unfavorable side of the existing trigger. Live mode delegates this to the
// exchange, so the helper only matters for the paper-mode loop.
func TestTrailingStopBreached(t *testing.T) {
	cases := []struct {
		name           string
		side           string
		mark           float64
		currentTrigger float64
		want           bool
	}{
		{"long mark above trigger no breach", "long", 105, 97, false},
		{"long mark equals trigger triggers fill", "long", 97, 97, true},
		{"long mark below trigger breached", "long", 90, 97, true},
		{"short mark below trigger no breach", "short", 95, 103, false},
		{"short mark equals trigger triggers fill", "short", 103, 103, true},
		{"short mark above trigger breached", "short", 110, 103, true},
		{"zero trigger never breaches (not yet armed)", "long", 80, 0, false},
		{"zero mark never breaches", "long", 0, 97, false},
		{"unknown side never breaches", "neutral", 50, 100, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := trailingStopBreached(c.side, c.mark, c.currentTrigger); got != c.want {
				t.Errorf("trailingStopBreached(%s, %v, %v) = %v, want %v",
					c.side, c.mark, c.currentTrigger, got, c.want)
			}
		})
	}
}

// #532: runHyperliquidTrailingStopPaper composes effectiveTrailingStopPct,
// trailingStopBreached, and computeTrailingStopUpdate into a single per-cycle
// decision for paper mode. We exercise (a) the breach path that fires a
// synthetic close, (b) the trigger-replacement path that ratchets, (c) the
// no-op path that only advances the high-water mark, (d) the bootstrap path
// where the first cycle establishes a trigger from AvgCost, and (e) the
// guard paths that skip when trailing is unconfigured or mark is zero.
func TestRunHyperliquidTrailingStopPaper(t *testing.T) {
	pct := func(v float64) *float64 { return &v }
	scWithTrailing := StrategyConfig{
		Platform:        "hyperliquid",
		Type:            "perps",
		TrailingStopPct: pct(3.0),
	}
	scNoTrailing := StrategyConfig{Platform: "hyperliquid", Type: "perps"}
	scNonHL := StrategyConfig{Platform: "okx", Type: "perps", TrailingStopPct: pct(3.0)}

	type want struct {
		newHighWater float64
		newTrigger   float64
		breach       bool
		breachPx     float64
	}
	cases := []struct {
		name           string
		sc             StrategyConfig
		side           string
		pos            *Position
		mark           float64
		highWater      float64
		currentTrigger float64
		want           want
	}{
		{
			name:           "long breach closes at trigger",
			sc:             scWithTrailing,
			side:           "long",
			pos:            &Position{AvgCost: 100},
			mark:           96,
			highWater:      110,
			currentTrigger: 106.7,
			want:           want{newHighWater: 110, newTrigger: 0, breach: true, breachPx: 106.7},
		},
		{
			name:           "short breach closes at trigger",
			sc:             scWithTrailing,
			side:           "short",
			pos:            &Position{AvgCost: 100},
			mark:           104,
			highWater:      90,
			currentTrigger: 92.7,
			want:           want{newHighWater: 90, newTrigger: 0, breach: true, breachPx: 92.7},
		},
		{
			name:           "long ratchets favorable trigger",
			sc:             scWithTrailing,
			side:           "long",
			pos:            &Position{AvgCost: 100},
			mark:           110,
			highWater:      100,
			currentTrigger: 97,
			want:           want{newHighWater: 110, newTrigger: 106.7, breach: false, breachPx: 0},
		},
		{
			name:           "long no-op below min-move debounce",
			sc:             scWithTrailing,
			side:           "long",
			pos:            &Position{AvgCost: 100},
			mark:           100.4,
			highWater:      100,
			currentTrigger: 97,
			want:           want{newHighWater: 100.4, newTrigger: 0, breach: false, breachPx: 0},
		},
		{
			name:           "first cycle bootstraps trigger from AvgCost",
			sc:             scWithTrailing,
			side:           "long",
			pos:            &Position{AvgCost: 100},
			mark:           100,
			highWater:      0,
			currentTrigger: 0,
			want:           want{newHighWater: 100, newTrigger: 97, breach: false, breachPx: 0},
		},
		{
			name:           "no trailing config is no-op",
			sc:             scNoTrailing,
			side:           "long",
			pos:            &Position{AvgCost: 100},
			mark:           50,
			highWater:      100,
			currentTrigger: 97,
			want:           want{newHighWater: 100, newTrigger: 0, breach: false, breachPx: 0},
		},
		{
			name:           "non-HL platform returns no-op",
			sc:             scNonHL,
			side:           "long",
			pos:            &Position{AvgCost: 100},
			mark:           50,
			highWater:      100,
			currentTrigger: 97,
			want:           want{newHighWater: 100, newTrigger: 0, breach: false, breachPx: 0},
		},
		{
			name:           "zero mark is no-op",
			sc:             scWithTrailing,
			side:           "long",
			pos:            &Position{AvgCost: 100},
			mark:           0,
			highWater:      100,
			currentTrigger: 97,
			want:           want{newHighWater: 100, newTrigger: 0, breach: false, breachPx: 0},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			gotHW, gotTrig, gotBreach, gotPx := runHyperliquidTrailingStopPaper(c.sc, c.side, c.pos, c.mark, c.highWater, c.currentTrigger)
			if floatDiff(gotHW, c.want.newHighWater) > 1e-9 ||
				floatDiff(gotTrig, c.want.newTrigger) > 1e-9 ||
				gotBreach != c.want.breach ||
				floatDiff(gotPx, c.want.breachPx) > 1e-9 {
				t.Fatalf("runHyperliquidTrailingStopPaper = (hw=%v trig=%v breach=%v px=%v), want (hw=%v trig=%v breach=%v px=%v)",
					gotHW, gotTrig, gotBreach, gotPx,
					c.want.newHighWater, c.want.newTrigger, c.want.breach, c.want.breachPx)
			}
		})
	}
}

func TestRunHyperliquidTrailingStopPaper_RegimeSnapshotArms(t *testing.T) {
	regimeBlock := &RegimeATRBlock{TrendRegime: map[string]RegimeATREntry{
		"trending": {ATR: 2.0},
		"ranging":  {ATR: 1.0},
	}}
	sc := StrategyConfig{
		ID:                    "hl-regime-paper",
		Platform:              "hyperliquid",
		Type:                  "perps",
		RegimeATRWindow:       "medium",
		TrailingStopATRRegime: regimeBlock,
	}
	pos := &Position{
		AvgCost:         2100,
		EntryATR:        50,
		RiskAnchorPrice: 2000,
		Regime:          "ranging",
		RegimeWindows:   map[string]string{"medium": "trending"},
	}

	incomplete := &Position{AvgCost: pos.AvgCost, EntryATR: pos.EntryATR}
	_, gotTrig, gotBreach, gotPx := runHyperliquidTrailingStopPaper(sc, "long", incomplete, 2000, 0, 0)
	if gotTrig != 0 || gotBreach || gotPx != 0 {
		t.Fatalf("incomplete snapshot unexpectedly armed: trig=%v breach=%v px=%v", gotTrig, gotBreach, gotPx)
	}

	snapshot := hyperliquidProtectionPositionSnapshot(pos)
	gotHW, gotTrig, gotBreach, gotPx := runHyperliquidTrailingStopPaper(sc, "long", snapshot, 2000, 0, 0)
	if !approxEq(gotHW, 2000) || !approxEq(gotTrig, 1900) || gotBreach || gotPx != 0 {
		t.Fatalf("regime snapshot paper trail = (hw=%v trig=%v breach=%v px=%v), want (2000, 1900, false, 0)",
			gotHW, gotTrig, gotBreach, gotPx)
	}
}

// #532: paper-mode trailing stop close must operate only on the strategy's
// own virtual position. Two strategies on the same symbol with independent
// StrategyState maps must remain isolated when one breaches.
func TestRunHyperliquidTrailingStopPaper_StrategyIsolated(t *testing.T) {
	sA := &StrategyState{
		ID:       "hl-a",
		Platform: "hyperliquid",
		Type:     "perps",
		Cash:     1000,
		Positions: map[string]*Position{
			"BTC": {Symbol: "BTC", Quantity: 0.5, AvgCost: 100, Side: "long"},
		},
	}
	sB := &StrategyState{
		ID:       "hl-b",
		Platform: "hyperliquid",
		Type:     "perps",
		Cash:     1000,
		Positions: map[string]*Position{
			"BTC": {Symbol: "BTC", Quantity: 0.3, AvgCost: 99, Side: "long"},
		},
	}

	closed := recordPerpsStopLossClose(sA, "BTC", 97, "trailing_stop_loss_paper", silentStrategyLogger("hl-a"))
	if !closed {
		t.Fatalf("recordPerpsStopLossClose should have closed strategy A's position")
	}
	if _, ok := sA.Positions["BTC"]; ok {
		t.Errorf("strategy A's BTC position should have been removed")
	}
	if _, ok := sB.Positions["BTC"]; !ok {
		t.Errorf("strategy B's BTC position should be untouched by A's trailing-stop close")
	}
	if sB.Positions["BTC"].Quantity != 0.3 {
		t.Errorf("strategy B's BTC quantity should remain 0.3, got %v", sB.Positions["BTC"].Quantity)
	}
}

// #562: StopLossATRMult > 0 must defer the initial trigger placement just like
// TrailingStopATRMult — EntryATR/AvgCost are not yet on the position at order-
// placement time. Arming runs on the cycle after open.
func TestEffectiveStopLossPct_FixedATRMultDefersInitialTrigger(t *testing.T) {
	pf := func(v float64) *float64 { return &v }
	sc := StrategyConfig{
		Platform:        "hyperliquid",
		Type:            "perps",
		Leverage:        10,
		MaxDrawdownPct:  5, // would otherwise fall through to a 5% auto stop
		StopLossATRMult: pf(1.5),
	}
	if got := EffectiveStopLossPct(sc); got != 0 {
		t.Errorf("EffectiveStopLossPct with StopLossATRMult set = %g, want 0 (deferred)", got)
	}
}

// #562: explicit 0 StopLossATRMult must fall through to the next priority so
// that a config like {stop_loss_atr_mult: 0, stop_loss_pct: 2} arms the
// explicit fixed stop. Mirrors the TrailingStopATRMult fallthrough rule.
func TestEffectiveStopLossPct_FixedATRMultExplicitZeroFallsThrough(t *testing.T) {
	pf := func(v float64) *float64 { return &v }
	sc := StrategyConfig{
		Platform:        "hyperliquid",
		Type:            "perps",
		Leverage:        10,
		StopLossATRMult: pf(0),
		StopLossPct:     pf(2),
	}
	if got := EffectiveStopLossPct(sc); got != 2 {
		t.Errorf("EffectiveStopLossPct with explicit-zero StopLossATRMult and stop_loss_pct=2 = %g, want 2", got)
	}
}

// #562: effectiveFixedStopLossATRPct derives mult * EntryATR / AvgCost * 100,
// returns 0 when EntryATR/AvgCost is missing, and caps the result at
// MaxAutoStopLossPct so an outsized ATR (e.g. mult=3 on an ATR ≈ 30% of price)
// can't produce a long-side trigger price <= 0.
func TestEffectiveFixedStopLossATRPct(t *testing.T) {
	pf := func(v float64) *float64 { return &v }
	cases := []struct {
		name string
		sc   StrategyConfig
		pos  *Position
		want float64
	}{
		{"derived from EntryATR/AvgCost", StrategyConfig{
			Platform: "hyperliquid", Type: "perps", StopLossATRMult: pf(1.5),
		}, &Position{AvgCost: 2000, EntryATR: 40}, 1.5 * 40 / 2000 * 100},
		{"unset returns 0", StrategyConfig{
			Platform: "hyperliquid", Type: "perps",
		}, &Position{AvgCost: 2000, EntryATR: 40}, 0},
		{"explicit zero returns 0", StrategyConfig{
			Platform: "hyperliquid", Type: "perps", StopLossATRMult: pf(0),
		}, &Position{AvgCost: 2000, EntryATR: 40}, 0},
		{"missing EntryATR returns 0", StrategyConfig{
			Platform: "hyperliquid", Type: "perps", StopLossATRMult: pf(1.5),
		}, &Position{AvgCost: 2000, EntryATR: 0}, 0},
		{"nil position returns 0", StrategyConfig{
			Platform: "hyperliquid", Type: "perps", StopLossATRMult: pf(1.5),
		}, nil, 0},
		{"non-HL platform returns 0", StrategyConfig{
			Platform: "okx", Type: "perps", StopLossATRMult: pf(1.5),
		}, &Position{AvgCost: 2000, EntryATR: 40}, 0},
		{"capped at MaxAutoStopLossPct", StrategyConfig{
			Platform: "hyperliquid", Type: "perps", StopLossATRMult: pf(3),
		}, &Position{AvgCost: 100, EntryATR: 30}, MaxAutoStopLossPct},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := effectiveFixedStopLossATRPct(c.sc, c.pos); got != c.want {
				t.Errorf("effectiveFixedStopLossATRPct = %g, want %g", got, c.want)
			}
		})
	}
}

// #562: fixedStopLossATRTriggerPx returns AvgCost ± mult*EntryATR for long/short.
func TestFixedStopLossATRTriggerPx(t *testing.T) {
	pf := func(v float64) *float64 { return &v }
	sc := StrategyConfig{
		Platform:        "hyperliquid",
		Type:            "perps",
		StopLossATRMult: pf(1.5),
	}
	pos := &Position{AvgCost: 2000, EntryATR: 40}
	wantPct := 1.5 * 40 / 2000 * 100 // 3%

	if got := fixedStopLossATRTriggerPx(sc, "long", pos); got != 2000*(1-wantPct/100) {
		t.Errorf("long trigger = %g, want %g", got, 2000*(1-wantPct/100))
	}
	if got := fixedStopLossATRTriggerPx(sc, "short", pos); got != 2000*(1+wantPct/100) {
		t.Errorf("short trigger = %g, want %g", got, 2000*(1+wantPct/100))
	}
	if got := fixedStopLossATRTriggerPx(sc, "unknown", pos); got != 0 {
		t.Errorf("unknown side trigger = %g, want 0", got)
	}
}

// #562: validation rules for stop_loss_atr_mult — HL perps only, mutually
// exclusive with the four other stop fields.
func TestConfigValidation_StopLossATRMult(t *testing.T) {
	pf := func(v float64) *float64 { return &v }
	cases := []struct {
		name      string
		sc        StrategyConfig
		wantError bool
	}{
		{"in range", StrategyConfig{
			ID: "hl-test", Type: "perps", Platform: "hyperliquid",
			Script: "shared_scripts/check_hyperliquid.py", Capital: 1000, MaxDrawdownPct: 10, Leverage: 5,
			StopLossATRMult: pf(1.5),
		}, false},
		{"explicit zero disables (benign)", StrategyConfig{
			ID: "hl-test", Type: "perps", Platform: "hyperliquid",
			Script: "shared_scripts/check_hyperliquid.py", Capital: 1000, MaxDrawdownPct: 10, Leverage: 5,
			StopLossATRMult: pf(0),
		}, false},
		{"negative rejected", StrategyConfig{
			ID: "hl-test", Type: "perps", Platform: "hyperliquid",
			Script: "shared_scripts/check_hyperliquid.py", Capital: 1000, MaxDrawdownPct: 10, Leverage: 5,
			StopLossATRMult: pf(-0.5),
		}, true},
		{"non-HL platform rejected", StrategyConfig{
			ID: "ok-test", Type: "perps", Platform: "okx",
			Script: "shared_scripts/check_okx.py", Capital: 1000, MaxDrawdownPct: 10, Leverage: 5,
			StopLossATRMult: pf(1.5),
		}, true},
		{"non-perps type rejected", StrategyConfig{
			ID: "hl-test", Type: "spot", Platform: "hyperliquid",
			Script: "shared_scripts/check_hyperliquid.py", Capital: 1000, MaxDrawdownPct: 10,
			StopLossATRMult: pf(1.5),
		}, true},
		{"mutually exclusive with stop_loss_pct", StrategyConfig{
			ID: "hl-test", Type: "perps", Platform: "hyperliquid",
			Script: "shared_scripts/check_hyperliquid.py", Capital: 1000, MaxDrawdownPct: 10, Leverage: 5,
			StopLossATRMult: pf(1.5), StopLossPct: pf(2),
		}, true},
		{"mutually exclusive with stop_loss_margin_pct", StrategyConfig{
			ID: "hl-test", Type: "perps", Platform: "hyperliquid",
			Script: "shared_scripts/check_hyperliquid.py", Capital: 1000, MaxDrawdownPct: 10, Leverage: 5,
			StopLossATRMult: pf(1.5), StopLossMarginPct: pf(20),
		}, true},
		{"mutually exclusive with trailing_stop_pct", StrategyConfig{
			ID: "hl-test", Type: "perps", Platform: "hyperliquid",
			Script: "shared_scripts/check_hyperliquid.py", Capital: 1000, MaxDrawdownPct: 10, Leverage: 5,
			StopLossATRMult: pf(1.5), TrailingStopPct: pf(2),
		}, true},
		{"mutually exclusive with trailing_stop_atr_mult", StrategyConfig{
			ID: "hl-test", Type: "perps", Platform: "hyperliquid",
			Script: "shared_scripts/check_hyperliquid.py", Capital: 1000, MaxDrawdownPct: 10, Leverage: 5,
			StopLossATRMult: pf(1.5), TrailingStopATRMult: pf(2),
		}, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cfg := &Config{Strategies: []StrategyConfig{c.sc}}
			err := validateConfig(cfg, false)
			if c.wantError && err == nil {
				t.Errorf("expected validation error, got nil")
			}
			if !c.wantError && err != nil {
				t.Errorf("expected no error, got: %v", err)
			}
			if c.wantError && err != nil && !strings.Contains(err.Error(), "stop_loss_atr_mult") {
				t.Errorf("error did not mention stop_loss_atr_mult: %v", err)
			}
		})
	}
}

// #562: peer normalization treats StopLossATRMult ownership the same as the
// other stop-loss owners — peers without any stop field set get StopLossPct=0
// so the MaxDrawdownPct auto-derive is suppressed for them.
func TestNormalizeHyperliquidPeerStopLosses_FixedATRMultOwner(t *testing.T) {
	mult := 1.5
	strategies := []StrategyConfig{
		{
			ID:              "hl-eth-trend",
			Type:            "perps",
			Platform:        "hyperliquid",
			Args:            []string{"trend", "ETH", "1h"},
			Leverage:        5,
			MaxDrawdownPct:  10,
			StopLossATRMult: &mult,
		},
		{
			ID:             "hl-eth-breakout",
			Type:           "perps",
			Platform:       "hyperliquid",
			Args:           []string{"breakout", "ETH", "1h"},
			Leverage:       5,
			MaxDrawdownPct: 10,
		},
	}
	normalizeHyperliquidPeerStopLosses(strategies)

	if strategies[0].StopLossPct != nil {
		t.Errorf("fixed ATR-mult owner should not gain a normalized StopLossPct; got %v", strategies[0].StopLossPct)
	}
	if strategies[1].StopLossPct != nil {
		t.Fatalf("peer StopLossPct = %v, want nil", strategies[1].StopLossPct)
	}
}

// #964: multiple same-coin trailing peers are allowed once the live trailing
// updater serializes replacement and the reconciler attributes exact OID fills.
func TestHyperliquidPeerStrategyErrors_TrailingStopPeersAllowed(t *testing.T) {
	a := 0.05 // 5% trailing
	b := 0.03
	strategies := []StrategyConfig{
		{
			ID: "hl-eth-trail-a", Type: "perps", Platform: "hyperliquid",
			Args: []string{"trend", "ETH", "1h"}, Leverage: 5,
			TrailingStopPct: &a,
		},
		{
			ID: "hl-eth-trail-b", Type: "perps", Platform: "hyperliquid",
			Args: []string{"breakout", "ETH", "1h"}, Leverage: 5,
			TrailingStopPct: &b,
		},
	}
	errs := hyperliquidPeerStrategyErrors(strategies)
	if len(errs) != 0 {
		t.Fatalf("unexpected trailing-stop peer errors: %v", errs)
	}
}

func TestHyperliquidPeerStrategyErrors_TrailingATRPeersAllowed(t *testing.T) {
	a := 1.0
	b := 1.5
	strategies := []StrategyConfig{
		{
			ID: "hl-btc-a", Type: "perps", Platform: "hyperliquid",
			Args: []string{"trend", "BTC", "1h"}, Leverage: 3,
			TrailingStopATRMult: &a,
		},
		{
			ID: "hl-btc-b", Type: "perps", Platform: "hyperliquid",
			Args: []string{"breakout", "BTC", "1h"}, Leverage: 3,
			TrailingStopATRMult: &b,
		},
	}
	errs := hyperliquidPeerStrategyErrors(strategies)
	if len(errs) != 0 {
		t.Fatalf("unexpected trailing-ATR peer errors: %v", errs)
	}
}

// #601: hyperliquidPeerStrategyErrors allows multiple same-coin peers with
// fixed ATR stops because each order is sized to that strategy's virtual qty.
func TestHyperliquidPeerStrategyErrors_FixedATRMultAllowed(t *testing.T) {
	a := 1.5
	b := 2.0
	strategies := []StrategyConfig{
		{
			ID: "hl-eth-a", Type: "perps", Platform: "hyperliquid",
			Args: []string{"trend", "ETH", "1h"}, Leverage: 5,
			StopLossATRMult: &a,
		},
		{
			ID: "hl-eth-b", Type: "perps", Platform: "hyperliquid",
			Args: []string{"breakout", "ETH", "1h"}, Leverage: 5,
			StopLossATRMult: &b,
		},
	}
	errs := hyperliquidPeerStrategyErrors(strategies)
	if len(errs) != 0 {
		t.Fatalf("unexpected peer errors: %v", errs)
	}
}

// #562/#601/#605: LoadConfig defaults HL perps strategies with no explicit
// stop fields to default_stop_loss_atr_mult (1.0× ATR by default), including
// shared-coin peers since #601 sizes reduce-only stops per strategy.
func TestLoadConfig_DefaultsStopLossATRMultForSoleOwner(t *testing.T) {
	dir := t.TempDir()
	cfgJSON := `{
		"strategies": [{
			"id": "hl-sole",
			"type": "perps",
			"platform": "hyperliquid",
			"script": "shared_scripts/check_hyperliquid.py",
			"args": ["sma_crossover", "ETH", "1h", "--mode=paper"],
			"capital": 1000,
			"max_drawdown_pct": 10,
			"leverage": 5
		}]
	}`
	path := writeTestConfig(t, dir, cfgJSON)
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig failed: %v", err)
	}
	sc := cfg.Strategies[0]
	if sc.StopLossATRMult == nil {
		t.Fatal("expected default StopLossATRMult applied, got nil")
	}
	if *sc.StopLossATRMult != DefaultStopLossATRMult {
		t.Errorf("default StopLossATRMult = %g, want %g", *sc.StopLossATRMult, DefaultStopLossATRMult)
	}
}

func TestLoadConfig_UsesConfiguredDefaultStopLossATRMult(t *testing.T) {
	dir := t.TempDir()
	cfgJSON := `{
		"default_stop_loss_atr_mult": 1.5,
		"strategies": [{
			"id": "hl-sole",
			"type": "perps",
			"platform": "hyperliquid",
			"script": "shared_scripts/check_hyperliquid.py",
			"args": ["sma_crossover", "ETH", "1h", "--mode=paper"],
			"capital": 1000,
			"max_drawdown_pct": 10,
			"leverage": 5
		}]
	}`
	path := writeTestConfig(t, dir, cfgJSON)
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig failed: %v", err)
	}
	if cfg.DefaultStopLossATRMult == nil || *cfg.DefaultStopLossATRMult != 1.5 {
		t.Fatalf("DefaultStopLossATRMult = %v, want 1.5", cfg.DefaultStopLossATRMult)
	}
	sc := cfg.Strategies[0]
	if sc.StopLossATRMult == nil || *sc.StopLossATRMult != 1.5 {
		t.Fatalf("strategy StopLossATRMult = %v, want 1.5", sc.StopLossATRMult)
	}
}

// #562: sole-owner with an explicit stop_loss_pct does NOT get the default
// stop_loss_atr_mult — explicit config wins.
func TestLoadConfig_NoDefaultStopLossATRMultWhenExplicitFieldSet(t *testing.T) {
	dir := t.TempDir()
	cfgJSON := `{
		"strategies": [{
			"id": "hl-explicit",
			"type": "perps",
			"platform": "hyperliquid",
			"script": "shared_scripts/check_hyperliquid.py",
			"args": ["sma_crossover", "ETH", "1h", "--mode=paper"],
			"capital": 1000,
			"max_drawdown_pct": 10,
			"leverage": 5,
			"stop_loss_pct": 3
		}]
	}`
	path := writeTestConfig(t, dir, cfgJSON)
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig failed: %v", err)
	}
	if cfg.Strategies[0].StopLossATRMult != nil {
		t.Errorf("StopLossATRMult should remain nil when stop_loss_pct is explicit, got %v", cfg.Strategies[0].StopLossATRMult)
	}
}

// #601/#605: peer strategies on the same coin receive the default ATR stop
// because exchange-side orders are sized per strategy.
func TestLoadConfig_DefaultStopLossATRMultForPeers(t *testing.T) {
	dir := t.TempDir()
	cfgJSON := `{
		"strategies": [
			{
				"id": "hl-eth-trend",
				"type": "perps",
				"platform": "hyperliquid",
				"script": "shared_scripts/check_hyperliquid.py",
				"args": ["trend", "ETH", "1h"],
				"capital": 1000,
				"leverage": 5,
				"stop_loss_atr_mult": 1.5
			},
			{
				"id": "hl-eth-breakout",
				"type": "perps",
				"platform": "hyperliquid",
				"script": "shared_scripts/check_hyperliquid.py",
				"args": ["breakout", "ETH", "1h"],
				"capital": 1000,
				"leverage": 5
			}
		]
	}`
	path := writeTestConfig(t, dir, cfgJSON)
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig failed: %v", err)
	}
	for _, sc := range cfg.Strategies {
		if sc.ID == "hl-eth-breakout" {
			if sc.StopLossATRMult == nil || *sc.StopLossATRMult != DefaultStopLossATRMult {
				t.Errorf("peer StopLossATRMult = %v, want default %.1f", sc.StopLossATRMult, DefaultStopLossATRMult)
			}
			if sc.StopLossPct != nil {
				t.Errorf("peer StopLossPct = %v, want nil", sc.StopLossPct)
			}
		}
	}
}

// #601/#605: when no peer owns an explicit stop, every peer receives the
// configured top-level default — #601 sizes reduce-only protection per
// strategy, so peers no longer share a single owner.
func TestLoadConfig_ConfiguredDefaultAppliesToAllPeers(t *testing.T) {
	dir := t.TempDir()
	cfgJSON := `{
		"default_stop_loss_atr_mult": 1.5,
		"strategies": [
			{
				"id": "hl-eth-ztrend",
				"type": "perps",
				"platform": "hyperliquid",
				"script": "shared_scripts/check_hyperliquid.py",
				"args": ["ztrend", "ETH", "1h"],
				"capital": 1000,
				"leverage": 5
			},
			{
				"id": "hl-eth-breakout",
				"type": "perps",
				"platform": "hyperliquid",
				"script": "shared_scripts/check_hyperliquid.py",
				"args": ["breakout", "ETH", "1h"],
				"capital": 1000,
				"leverage": 5
			}
		]
	}`
	path := writeTestConfig(t, dir, cfgJSON)
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig failed: %v", err)
	}
	for _, sc := range cfg.Strategies {
		if sc.StopLossATRMult == nil || *sc.StopLossATRMult != 1.5 {
			t.Errorf("%s StopLossATRMult = %v, want 1.5", sc.ID, sc.StopLossATRMult)
		}
		if sc.StopLossPct != nil {
			t.Errorf("%s StopLossPct = %v, want nil", sc.ID, sc.StopLossPct)
		}
	}
}

// #605: explicit default_stop_loss_atr_mult=0 opts out of the auto-default
// entirely; HL perps strategies with all stop fields omitted keep nil so
// EffectiveStopLossPct's MaxDrawdownPct fallback can still apply.
func TestLoadConfig_DefaultStopLossATRMultZeroOptsOut(t *testing.T) {
	dir := t.TempDir()
	cfgJSON := `{
		"default_stop_loss_atr_mult": 0,
		"strategies": [{
			"id": "hl-optout",
			"type": "perps",
			"platform": "hyperliquid",
			"script": "shared_scripts/check_hyperliquid.py",
			"args": ["sma_crossover", "ETH", "1h", "--mode=paper"],
			"capital": 1000,
			"max_drawdown_pct": 10,
			"leverage": 5
		}]
	}`
	path := writeTestConfig(t, dir, cfgJSON)
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig failed: %v", err)
	}
	sc := cfg.Strategies[0]
	if sc.StopLossATRMult != nil {
		t.Errorf("StopLossATRMult = %v, want nil (default opted out)", sc.StopLossATRMult)
	}
	if sc.StopLossPct != nil {
		t.Errorf("StopLossPct = %v, want nil", sc.StopLossPct)
	}
}

// #562: paper-mode arming returns the trigger on the first cycle (currentTrigger=0)
// and breach=true once mark crosses the trigger on a subsequent cycle.
func TestRunHyperliquidFixedATRStopLossPaper(t *testing.T) {
	pf := func(v float64) *float64 { return &v }
	sc := StrategyConfig{
		Platform:        "hyperliquid",
		Type:            "perps",
		StopLossATRMult: pf(1.5),
	}
	pos := &Position{AvgCost: 2000, EntryATR: 40}
	// expected: pct = 1.5 * 40 / 2000 * 100 = 3%; long trigger = 2000 * 0.97 = 1940
	wantTrigger := 1940.0

	// Cycle 1: not yet armed — return trigger px, no breach.
	newTrigger, breach, breachPx := runHyperliquidFixedATRStopLossPaper(sc, "long", pos, 2010, 0)
	if breach {
		t.Errorf("cycle1 breach=true, want false")
	}
	if newTrigger != wantTrigger {
		t.Errorf("cycle1 newTrigger = %g, want %g", newTrigger, wantTrigger)
	}
	if breachPx != 0 {
		t.Errorf("cycle1 breachPx = %g, want 0", breachPx)
	}

	// Cycle 2 above trigger: trigger already armed; no new trigger; no breach.
	newTrigger, breach, _ = runHyperliquidFixedATRStopLossPaper(sc, "long", pos, 2050, wantTrigger)
	if breach {
		t.Errorf("cycle2 breach=true, want false (mark above trigger)")
	}
	if newTrigger != 0 {
		t.Errorf("cycle2 newTrigger = %g, want 0 (already armed)", newTrigger)
	}

	// Cycle 3 mark crosses trigger: breach.
	newTrigger, breach, breachPx = runHyperliquidFixedATRStopLossPaper(sc, "long", pos, 1939, wantTrigger)
	if !breach {
		t.Error("cycle3 breach=false, want true")
	}
	if newTrigger != 0 {
		t.Errorf("cycle3 newTrigger = %g, want 0", newTrigger)
	}
	if breachPx != wantTrigger {
		t.Errorf("cycle3 breachPx = %g, want %g", breachPx, wantTrigger)
	}

	// short side — mark above trigger triggers breach.
	shortTrigger := 2060.0 // 2000 * 1.03
	newTrigger, breach, _ = runHyperliquidFixedATRStopLossPaper(sc, "short", pos, 1990, 0)
	if breach {
		t.Errorf("short cycle1 breach=true, want false")
	}
	if newTrigger != shortTrigger {
		t.Errorf("short cycle1 newTrigger = %g, want %g", newTrigger, shortTrigger)
	}
	newTrigger, breach, breachPx = runHyperliquidFixedATRStopLossPaper(sc, "short", pos, 2061, shortTrigger)
	if !breach {
		t.Error("short cycle3 breach=false, want true")
	}
	if breachPx != shortTrigger {
		t.Errorf("short cycle3 breachPx = %g, want %g", breachPx, shortTrigger)
	}
}

// #562: when StopLossATRMult is unset the paper helper short-circuits.
func TestRunHyperliquidFixedATRStopLossPaper_Unset(t *testing.T) {
	sc := StrategyConfig{Platform: "hyperliquid", Type: "perps"}
	pos := &Position{AvgCost: 2000, EntryATR: 40}
	newTrigger, breach, breachPx := runHyperliquidFixedATRStopLossPaper(sc, "long", pos, 2010, 0)
	if newTrigger != 0 || breach || breachPx != 0 {
		t.Errorf("unset short-circuit: trigger=%g breach=%v breachPx=%g, want 0,false,0", newTrigger, breach, breachPx)
	}
}

// #562: stop_loss_atr_mult round-trips through JSON only when explicit
// (omitempty drops nil) and is rendered via formatFloatPtr in hot-reload diffs.
func TestStrategyConfig_StopLossATRMultJSON(t *testing.T) {
	v := 1.5
	sc := StrategyConfig{
		ID:              "hl-test",
		Type:            "perps",
		Platform:        "hyperliquid",
		Leverage:        10,
		StopLossATRMult: &v,
	}
	b, err := json.Marshal(sc)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(b), `"stop_loss_atr_mult":1.5`) {
		t.Errorf("expected stop_loss_atr_mult in JSON; got %s", b)
	}

	sc.StopLossATRMult = nil
	b2, err := json.Marshal(sc)
	if err != nil {
		t.Fatalf("marshal nil: %v", err)
	}
	if strings.Contains(string(b2), "stop_loss_atr_mult") {
		t.Errorf("nil StopLossATRMult should be omitted; got %s", b2)
	}

	zero := 0.0
	sc.StopLossATRMult = &zero
	b3, err := json.Marshal(sc)
	if err != nil {
		t.Fatalf("marshal zero: %v", err)
	}
	if !strings.Contains(string(b3), `"stop_loss_atr_mult":0`) {
		t.Errorf("explicit zero StopLossATRMult must round-trip; got %s", b3)
	}
}

func TestHyperliquidArmFixedATRStopLossLive_PlacesWithCorrectArgs(t *testing.T) {
	old := runHyperliquidUpdateStopLossFunc
	defer func() { runHyperliquidUpdateStopLossFunc = old }()

	var gotSymbol, gotSide string
	var gotSize, gotTrigger float64
	var gotCancelOID int64
	runHyperliquidUpdateStopLossFunc = func(script, symbol, side string, size, triggerPx float64, cancelStopLossOID int64) (*HyperliquidStopLossUpdateResult, string, error) {
		gotSymbol = symbol
		gotSide = side
		gotSize = size
		gotTrigger = triggerPx
		gotCancelOID = cancelStopLossOID
		return &HyperliquidStopLossUpdateResult{
			StopLossOID:       42,
			StopLossTriggerPx: triggerPx,
		}, "", nil
	}

	mult := 1.5
	sc := StrategyConfig{ID: "hl-fixed-atr", Platform: "hyperliquid", Type: "perps", Script: "shared_scripts/check_hyperliquid.py", StopLossATRMult: &mult}
	logger := silentStrategyLogger("hl-fixed-atr")
	defer logger.Close()

	result, ok := hyperliquidArmFixedATRStopLossLive(sc, "BTC", "long", 0.1, 95000.0, nil, logger)
	if !ok {
		t.Fatalf("hyperliquidArmFixedATRStopLossLive returned ok=false")
	}
	if result == nil || result.StopLossOID != 42 {
		t.Fatalf("result=%+v, want OID 42", result)
	}
	if gotSymbol != "BTC" || gotSide != "long" || gotSize != 0.1 || gotTrigger != 95000.0 || gotCancelOID != 0 {
		t.Fatalf("arm args=(%s,%s,%v,%v,%d), want (BTC,long,0.1,95000.0,0)",
			gotSymbol, gotSide, gotSize, gotTrigger, gotCancelOID)
	}
}

func TestHyperliquidArmFixedATRStopLossLive_NotifiesOnError(t *testing.T) {
	old := runHyperliquidUpdateStopLossFunc
	defer func() { runHyperliquidUpdateStopLossFunc = old }()

	runHyperliquidUpdateStopLossFunc = func(script, symbol, side string, size, triggerPx float64, cancelStopLossOID int64) (*HyperliquidStopLossUpdateResult, string, error) {
		return nil, "", fmt.Errorf("connection refused")
	}

	mult := 1.0
	sc := StrategyConfig{ID: "hl-fixed-err", Platform: "hyperliquid", Type: "perps", Script: "shared_scripts/check_hyperliquid.py", StopLossATRMult: &mult}
	logger := silentStrategyLogger("hl-fixed-err")
	defer logger.Close()
	mock := &mockNotifier{}
	notifier := NewMultiNotifier(notifierBackend{
		notifier: mock,
		channels: map[string]string{"hyperliquid": "ch1"},
		ownerID:  "owner",
	})

	_, ok := hyperliquidArmFixedATRStopLossLive(sc, "ETH", "long", 0.5, 3000.0, notifier, logger)
	if ok {
		t.Fatal("expected ok=false on arm error")
	}
	if len(mock.messages) == 0 {
		t.Fatal("expected a notification on arm error, got none")
	}
}

// --- #621: hlSLEffectiveQty caps SL size at on-chain qty ---

func TestHLSLEffectiveQty_NoCapWhenOnChainGeVirtual(t *testing.T) {
	onChain := map[string]float64{"ETH": 0.422}
	got, capped := hlSLEffectiveQty("ETH", 0.422, onChain)
	if capped {
		t.Error("capped=true, want false when on-chain == virtual")
	}
	if got != 0.422 {
		t.Errorf("qty = %g, want 0.422", got)
	}

	onChain2 := map[string]float64{"ETH": 0.500}
	got2, capped2 := hlSLEffectiveQty("ETH", 0.422, onChain2)
	if capped2 {
		t.Error("capped=true, want false when on-chain > virtual")
	}
	if got2 != 0.422 {
		t.Errorf("qty = %g, want 0.422", got2)
	}
}

func TestHLSLEffectiveQty_CapsWhenOnChainLtVirtual(t *testing.T) {
	onChain := map[string]float64{"ETH": 0.211}
	got, capped := hlSLEffectiveQty("ETH", 0.422, onChain)
	if !capped {
		t.Error("capped=false, want true when on-chain < virtual")
	}
	if got < 0.211-1e-9 || got > 0.211+1e-9 {
		t.Errorf("qty = %g, want 0.211 (on-chain qty)", got)
	}
}

func TestHLSLEffectiveQty_NoCapWhenSymbolNotInMap(t *testing.T) {
	onChain := map[string]float64{"BTC": 0.01}
	got, capped := hlSLEffectiveQty("ETH", 0.422, onChain)
	if capped {
		t.Error("capped=true, want false when symbol not in on-chain map")
	}
	if got != 0.422 {
		t.Errorf("qty = %g, want 0.422", got)
	}
}

func TestHLSLEffectiveQty_NoCapWhenOnChainZero(t *testing.T) {
	// On-chain zero means Detector 1 territory; don't cap to zero.
	onChain := map[string]float64{"ETH": 0}
	got, capped := hlSLEffectiveQty("ETH", 0.422, onChain)
	if capped {
		t.Error("capped=true, want false when on-chain qty is zero")
	}
	if got != 0.422 {
		t.Errorf("qty = %g, want 0.422", got)
	}
}

// #562: atrMultMissingEntryATR fires when StopLossATRMult is configured but
// the open candle didn't produce an ATR — same alert behavior as
// TrailingStopATRMult.
func TestATRMultMissingEntryATR_FixedATRMult(t *testing.T) {
	pf := func(v float64) *float64 { return &v }
	sc := StrategyConfig{
		Platform:        "hyperliquid",
		Type:            "perps",
		StopLossATRMult: pf(1.5),
	}
	posMissing := &Position{AvgCost: 2000, EntryATR: 0}
	if !atrMultMissingEntryATR(sc, posMissing) {
		t.Error("expected atrMultMissingEntryATR=true for fixed StopLossATRMult with missing EntryATR")
	}
	posOK := &Position{AvgCost: 2000, EntryATR: 40}
	if atrMultMissingEntryATR(sc, posOK) {
		t.Error("expected atrMultMissingEntryATR=false when EntryATR is stamped")
	}
}

// TestHyperliquidProtectionPositionSnapshot_CarriesFullSurface pins the #1015
// invariant field-by-field: the lock-free protection walker's snapshot must
// carry the FULL protection surface (regime label/windows/transition state,
// the frozen #873 risk anchor, and post-TP/ratchet state). A field added to
// the protection surface but not propagated here silently unarms paper regime
// trailing SLs — the original #1015 bug class.
func TestHyperliquidProtectionPositionSnapshot_CarriesFullSurface(t *testing.T) {
	postTP := 1.25
	src := &Position{
		AvgCost:                         3100,
		EntryATR:                        42,
		RiskAnchorPrice:                 3000,
		Regime:                          "trending",
		RegimeWindows:                   map[string]string{"daily": "trending", "weekly": "ranging"},
		RegimeAppliedLabel:              "trending",
		RegimePendingLabel:              "ranging",
		RegimePendingCount:              2,
		SLAdjustedTiersProcessed:        3,
		RatchetFallbackNormalizePending: true,
		PostTPTrailingATRMult:           &postTP,
	}
	snap := hyperliquidProtectionPositionSnapshot(src)
	if snap == nil {
		t.Fatal("snapshot is nil for non-nil position")
	}
	if snap.AvgCost != 3100 || snap.EntryATR != 42 || snap.RiskAnchorPrice != 3000 {
		t.Errorf("price surface = (AvgCost %v, EntryATR %v, RiskAnchorPrice %v); want (3100, 42, 3000)",
			snap.AvgCost, snap.EntryATR, snap.RiskAnchorPrice)
	}
	if snap.Regime != "trending" || snap.RegimeAppliedLabel != "trending" ||
		snap.RegimePendingLabel != "ranging" || snap.RegimePendingCount != 2 {
		t.Errorf("regime surface = (%q, %q, %q, %d); want (trending, trending, ranging, 2)",
			snap.Regime, snap.RegimeAppliedLabel, snap.RegimePendingLabel, snap.RegimePendingCount)
	}
	if !reflect.DeepEqual(snap.RegimeWindows, src.RegimeWindows) {
		t.Errorf("RegimeWindows = %v; want %v", snap.RegimeWindows, src.RegimeWindows)
	}
	if snap.SLAdjustedTiersProcessed != 3 || !snap.RatchetFallbackNormalizePending {
		t.Errorf("ratchet surface = (%d, %v); want (3, true)",
			snap.SLAdjustedTiersProcessed, snap.RatchetFallbackNormalizePending)
	}
	if snap.PostTPTrailingATRMult == nil || *snap.PostTPTrailingATRMult != 1.25 {
		t.Errorf("PostTPTrailingATRMult = %v; want 1.25", snap.PostTPTrailingATRMult)
	}

	// Deep-copy guarantees: the walker is lock-free, so mutating the source
	// after snapshotting must not leak into the snapshot.
	src.RegimeWindows["daily"] = "ranging"
	if snap.RegimeWindows["daily"] != "trending" {
		t.Error("RegimeWindows was shallow-copied; walker snapshot mutated by main loop")
	}
	*src.PostTPTrailingATRMult = 9.9
	if *snap.PostTPTrailingATRMult != 1.25 {
		t.Error("PostTPTrailingATRMult pointer was shared; walker snapshot mutated by main loop")
	}

	if hyperliquidProtectionPositionSnapshot(nil) != nil {
		t.Error("snapshot of nil position must be nil")
	}
}

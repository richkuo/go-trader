package main

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

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
	p := Position{Symbol: "ETH", Quantity: 1, AvgCost: 3000, Side: "long", StopLossOID: 42}
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
	// omitempty: zero should drop from JSON.
	b2, _ := json.Marshal(Position{Symbol: "ETH", Quantity: 1, AvgCost: 3000, Side: "long"})
	if containsKey(b2, "stop_loss_oid") {
		t.Errorf("zero StopLossOID should be omitted; got %s", b2)
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

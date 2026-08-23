package main

import "testing"

// #1456 review round 16 (Needs Fixing 1): the walker/audit sibling of the
// round-15 protection-sync fix. A cancel that LANDED followed by a placement
// whose outcome could not be read must keep the recorded stop state — clearing
// it made the position read as Unprotected, which licensed a re-arm that placed
// a SECOND reduce-only stop whose OID was never recorded, so the scheduler
// could never cancel it.
func TestApplyTrailingStopUpdateResultKeepsStateOnOutcomeUnknown(t *testing.T) {
	newSS := func() *StrategyState {
		return &StrategyState{
			ID:        "hl-live",
			Cash:      1000,
			Positions: map[string]*Position{"ETH": {Symbol: "ETH", Side: "long", Quantity: 1, AvgCost: 2000, EntryATR: 25, StopLossOID: 111, StopLossTriggerPx: 1850}},
		}
	}
	logger := newTestLogger(t)

	// (a) cancel landed, placement outcome unreadable — round 19: state stops
	// pointing at the CANCELLED OID and records the REQUESTED trigger instead.
	// The old keep-the-dead-OID semantics made every later cycle re-cancel and
	// re-place, stacking untracked stops. {oid 0, reachable trigger} is never
	// an Unprotected re-arm candidate (the audit reads it armed-at-a-reachable-
	// trigger) and licenses no re-place.
	ss := newSS()
	fill, _ := applyTrailingStopUpdateResult(ss, "ETH", "long", 111, 0, true,
		&HyperliquidStopLossUpdateResult{CancelStopLossSucceeded: true, StopLossOutcomeUnknown: true, StopLossTriggerPx: 1860}, "", logger, 0)
	if fill {
		t.Fatalf("immediateFill = true, want false")
	}
	pos := ss.Positions["ETH"]
	if pos.StopLossOID != 0 || pos.StopLossTriggerPx != 1860 {
		t.Fatalf("outcome-unknown left oid=%d trigger=%.2f, want 0 / 1860 (dead OID unrecorded, requested trigger kept)", pos.StopLossOID, pos.StopLossTriggerPx)
	}
	// The audit's Unprotected test is StopLossOID == 0 && StopLossTriggerPx <= 0.
	if pos.StopLossOID == 0 && pos.StopLossTriggerPx <= 0 {
		t.Errorf("position reads as Unprotected after an outcome-unknown placement — a re-arm would stack a second untracked stop")
	}
	// No requested trigger in the payload: the old trigger survives so the
	// position stays armed-shaped (report-only), never Unprotected.
	ss = newSS()
	if _, _ = applyTrailingStopUpdateResult(ss, "ETH", "long", 111, 0, true,
		&HyperliquidStopLossUpdateResult{CancelStopLossSucceeded: true, StopLossOutcomeUnknown: true}, "", logger, 0); ss.Positions["ETH"].StopLossOID != 0 || ss.Positions["ETH"].StopLossTriggerPx != 1850 {
		t.Errorf("trigger-less payload: oid=%d trigger=%.2f, want 0 / 1850", ss.Positions["ETH"].StopLossOID, ss.Positions["ETH"].StopLossTriggerPx)
	}

	// (b) cancel landed, placement positively rejected — cleared as before, so
	// the in-cycle retry and the next-cycle re-arm still run.
	ss = newSS()
	if _, _ = applyTrailingStopUpdateResult(ss, "ETH", "long", 111, 0, true,
		&HyperliquidStopLossUpdateResult{CancelStopLossSucceeded: true}, "", logger, 0); ss.Positions["ETH"].StopLossOID != 0 || ss.Positions["ETH"].StopLossTriggerPx != 0 {
		t.Errorf("rejected placement left stale state: OID %d trigger %.2f, want 0 / 0", ss.Positions["ETH"].StopLossOID, ss.Positions["ETH"].StopLossTriggerPx)
	}

	// (c) an outcome-unknown result that nonetheless carries a resting OID is a
	// normal placement — the OID branch wins and state is adopted.
	ss = newSS()
	if _, _ = applyTrailingStopUpdateResult(ss, "ETH", "long", 111, 0, true,
		&HyperliquidStopLossUpdateResult{CancelStopLossSucceeded: true, StopLossOutcomeUnknown: true, StopLossOID: 222, StopLossTriggerPx: 1900}, "", logger, 0); ss.Positions["ETH"].StopLossOID != 222 {
		t.Errorf("resolved placement not adopted: OID %d, want 222", ss.Positions["ETH"].StopLossOID)
	}

	// (d) a fill at submit still books the close — outcome-unknown must not
	// shadow the flatten.
	ss = newSS()
	if fill, fillPx := applyTrailingStopUpdateResult(ss, "ETH", "long", 111, 0, true,
		&HyperliquidStopLossUpdateResult{CancelStopLossSucceeded: true, StopLossOutcomeUnknown: true, StopLossFilledImmediately: true, StopLossTriggerPx: 1850}, "", logger, 0); !fill || fillPx != 1850 {
		t.Errorf("fill = %v @ %.2f, want true @ 1850", fill, fillPx)
	}
}

package main

import (
	"testing"
	"time"
)

func TestSLPlacementFailureLeftNaked(t *testing.T) {
	cases := []struct {
		name            string
		cancelSucceeded bool
		oldOID          int64
		wantNaked       bool
	}{
		{"cancel succeeded then place failed = naked", true, 1001, true},
		{"no prior stop-loss then place failed = naked", false, 0, true},
		{"cancel failed so old SL still resting = safe", false, 1001, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := slPlacementFailureLeftNaked(tc.cancelSucceeded, tc.oldOID); got != tc.wantNaked {
				t.Fatalf("slPlacementFailureLeftNaked(%v,%d) = %v, want %v", tc.cancelSucceeded, tc.oldOID, got, tc.wantNaked)
			}
		})
	}
}

func TestPendingSLActionExists(t *testing.T) {
	db, err := OpenStateDB(":memory:")
	if err != nil {
		t.Fatalf("OpenStateDB: %v", err)
	}
	defer db.Close()
	now := time.Now().UTC()

	if err := db.InsertPendingManualAction(PendingManualAction{StrategyID: "hl-eth", Action: "open", Symbol: "ETH", Side: "long", Quantity: 1, FillPrice: 2000, CreatedAt: now}); err != nil {
		t.Fatalf("insert open: %v", err)
	}
	if err := db.InsertPendingManualAction(PendingManualAction{StrategyID: "hl-btc", Action: "update-sl", Symbol: "BTC", Side: "long", Quantity: 1, StopLossOID: 7, CreatedAt: now}); err != nil {
		t.Fatalf("insert other-strategy update-sl: %v", err)
	}

	if pending, err := pendingSLActionExists(openTestStore(t, db), "hl-eth", "ETH", nil); err != nil || pending {
		t.Fatalf("expected no pending SL action for hl-eth/ETH (open + other-strategy only), got pending=%v err=%v", pending, err)
	}

	if err := db.InsertPendingManualAction(PendingManualAction{StrategyID: "hl-eth", Action: "update-sl", Symbol: "ETH", Side: "long", Quantity: 1, StopLossOID: 9, StopLossTriggerPx: 1950, CreatedAt: now}); err != nil {
		t.Fatalf("insert same update-sl: %v", err)
	}
	if pending, err := pendingSLActionExists(openTestStore(t, db), "hl-eth", "eth", nil); err != nil || !pending {
		t.Fatalf("expected pending SL action for hl-eth/eth, got pending=%v err=%v", pending, err)
	}

	if err := db.InsertPendingManualAction(PendingManualAction{StrategyID: "hl-sol", Action: "cancel-sl", Symbol: "SOL", Side: "long", CreatedAt: now}); err != nil {
		t.Fatalf("insert cancel-sl: %v", err)
	}
	if pending, err := pendingSLActionExists(openTestStore(t, db), "hl-sol", "SOL", nil); err != nil || !pending {
		t.Fatalf("expected pending cancel-sl action for hl-sol/SOL, got pending=%v err=%v", pending, err)
	}

	adopted := &Position{Symbol: "ETH", StopLossOID: 9, StopLossTriggerPx: 1950}
	if pending, err := pendingSLActionExists(openTestStore(t, db), "hl-eth", "ETH", adopted); err != nil || pending {
		t.Fatalf("a queued update-sl the book already holds must not gate, got pending=%v err=%v", pending, err)
	}
	for _, drift := range []*Position{
		{Symbol: "ETH", StopLossOID: 9, StopLossTriggerPx: 1940},
		{Symbol: "ETH", StopLossOID: 10, StopLossTriggerPx: 1950},
		{Symbol: "ETH"},
	} {
		if pending, err := pendingSLActionExists(openTestStore(t, db), "hl-eth", "ETH", drift); err != nil || !pending {
			t.Fatalf("a queued update-sl the book has not adopted (%+v) must gate, got pending=%v err=%v", drift, pending, err)
		}
	}
	unknownOutcome := &Position{Symbol: "SOL", StopLossOID: 0, StopLossTriggerPx: 1950}
	if pending, err := pendingSLActionExists(openTestStore(t, db), "hl-sol", "SOL", unknownOutcome); err != nil || !pending {
		t.Fatalf("a cancel-sl action must gate regardless of the book, got pending=%v err=%v", pending, err)
	}
}

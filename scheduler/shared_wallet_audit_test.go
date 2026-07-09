package main

// Audit-gap regression tests for the shared-wallet drift alarm, shared-wallet
// membership, trade-ledger migration, and the kill-switch fill splitter.

import (
	"math"
	"testing"
	"time"
)

// Below the cent tolerance, reportSharedWalletDrift must never record a
// tracker entry (the within-tolerance path Clears instead of Records) and,
// with no entry, no alert can ever fire.
func TestReportSharedWalletDrift_BelowToleranceRecordsNothing(t *testing.T) {
	prev := sharedWalletDriftTracker
	sharedWalletDriftTracker = &SharedWalletDriftTracker{}
	defer func() { sharedWalletDriftTracker = prev }()

	key := SharedWalletKey{Platform: "hyperliquid", Account: "0xabc"}
	results := []sharedWalletDriftResult{
		{Key: key, Drift: 0.004, Balance: 1000, MemberSum: 1000.004},
	}
	reportSharedWalletDrift(nil, results)
	reportSharedWalletDrift(nil, results)

	if n := len(sharedWalletDriftTracker.entries); n != 0 {
		t.Fatalf("below-tolerance drift recorded %d tracker entries, want 0: %+v",
			n, sharedWalletDriftTracker.entries)
	}
}

// The over-tolerance guard is strict `>`: drift of exactly the $0.01 tolerance
// must not trip. A $0.02 control wallet confirms on cycle 2, proving the
// harness would have caught a trip.
func TestReportSharedWalletDrift_ExactToleranceDoesNotTrip(t *testing.T) {
	prev := sharedWalletDriftTracker
	sharedWalletDriftTracker = &SharedWalletDriftTracker{}
	defer func() { sharedWalletDriftTracker = prev }()

	exactKey := SharedWalletKey{Platform: "hyperliquid", Account: "0xexact"}
	ctrlKey := SharedWalletKey{Platform: "hyperliquid", Account: "0xctrl"}
	results := []sharedWalletDriftResult{
		{Key: exactKey, Drift: sharedWalletDriftTolerance, Balance: 1000, MemberSum: 1000.01},
		{Key: ctrlKey, Drift: 0.02, Balance: 1000, MemberSum: 1000.02},
	}
	reportSharedWalletDrift(nil, results)
	reportSharedWalletDrift(nil, results)

	if e := sharedWalletDriftTracker.entries[sharedWalletKeyLabel(exactKey)]; e != nil {
		t.Fatalf("drift exactly at tolerance must not record/trip (guard is strict >): %+v", e)
	}
	ctrl := sharedWalletDriftTracker.entries[sharedWalletKeyLabel(ctrlKey)]
	if ctrl == nil || ctrl.cycles != 2 || !ctrl.alerted {
		t.Fatalf("control wallet at $0.02 must confirm and alert on cycle 2: %+v", ctrl)
	}
}

// A sign-flipping drift is a materially changed drift: after alerting at +$5
// (anchor +500 cents), a -$5 reading moves 1000 cents against the anchor and
// must re-alert immediately; a follow-up -$5.05 (5 cents vs the new -500
// anchor, under the 10% ratio) must stay throttled.
func TestSharedWalletDriftTracker_SignFlipReAlerts(t *testing.T) {
	tr := &SharedWalletDriftTracker{}
	now := time.Now().UTC()

	if notify, _, _ := tr.Record("hyperliquid/0xabc", 5.00, []string{"BTC"}, now); notify {
		t.Fatal("cycle 1 is inside the confirmation window; must not notify")
	}
	if notify, _, _ := tr.Record("hyperliquid/0xabc", 5.00, []string{"BTC"}, now.Add(time.Second)); !notify {
		t.Fatal("cycle 2 must fire the confirmation alert (anchor +500 cents)")
	}
	if notify, _, _ := tr.Record("hyperliquid/0xabc", -5.00, []string{"BTC"}, now.Add(2*time.Second)); !notify {
		t.Fatal("sign flip +$5 → -$5 is a 1000-cent move against the anchor; must re-alert")
	}
	if notify, _, _ := tr.Record("hyperliquid/0xabc", -5.05, []string{"BTC"}, now.Add(3*time.Second)); notify {
		t.Fatal("-$5.05 vs the -$5.00 anchor is a 5-cent move (< 10%% ratio); must stay throttled")
	}
}

// sameAccountLiveManualMembers folds in live HL manual strategies ONLY when
// the wallet key's account matches this process's HYPERLIQUID_ACCOUNT_ADDRESS
// — a key for a different account must return nothing.
func TestSameAccountLiveManualMembers_ExcludesDifferentAccount(t *testing.T) {
	t.Setenv("HYPERLIQUID_ACCOUNT_ADDRESS", "0xAAA")

	strategies := []StrategyConfig{
		{ID: "hl-manual-1", Platform: "hyperliquid", Type: "manual", Symbol: "BTC",
			Args: []string{"hold", "BTC", "--mode=live"}},
	}

	other := SharedWalletKey{Platform: "hyperliquid", Account: "0xBBB"}
	if got := sameAccountLiveManualMembers(other, strategies); len(got) != 0 {
		t.Fatalf("different-account key must yield no members, got %v", got)
	}

	same := SharedWalletKey{Platform: "hyperliquid", Account: "0xAAA"}
	got := sameAccountLiveManualMembers(same, strategies)
	if len(got) != 1 || got[0] != "hl-manual-1" {
		t.Fatalf("same-account key must include the live manual strategy, got %v", got)
	}
}

// Migration-only pass (empty userFills map): a legacy net close row migrates
// to gross with a modeled fee, matches nothing, and — critically — the net
// ledger effect (NewPnL − NewFee) and NewCash are IDENTICAL to the legacy net
// replay. Migration must never move money.
func TestPlanTradeLedgerForStrategy_MigrationOnlyPreservesNetSum(t *testing.T) {
	base := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	trades := []TradeBackfillRow{
		{RowID: 1, Timestamp: base, Symbol: "BTC", Side: "sell", Quantity: 0.1,
			Price: 61000, Value: 6100, IsClose: true, RealizedPnL: 95,
			ExchangeOrderID: "200"}, // legacy: PnLGross=false, ExchangeFee=0
	}

	plan := planTradeLedgerForStrategyWithOIDTotals("hl-x", trades, map[string]HLFillSummary{}, 1000, 0, nil)

	if plan.MigratedCount != 1 || plan.MatchedCount != 0 {
		t.Fatalf("migrated=%d matched=%d, want 1/0 (no fills to true-up)", plan.MigratedCount, plan.MatchedCount)
	}
	if plan.UnmatchedOIDCount != 1 {
		t.Fatalf("UnmatchedOIDCount = %d, want 1", plan.UnmatchedOIDCount)
	}
	if len(plan.Changes) != 1 {
		t.Fatalf("want exactly 1 change, got %d", len(plan.Changes))
	}
	c := plan.Changes[0]
	modeledFee := 6100 * HyperliquidTakerFeePct
	if c.WasGross { // row was legacy net
		t.Fatalf("WasGross = %v, want false", c.WasGross)
	}
	if math.Abs(c.NewFee-modeledFee) > 1e-9 {
		t.Errorf("NewFee = %v, want modeled %v", c.NewFee, modeledFee)
	}
	// Gross migration: PnL becomes net + fee; net effect unchanged.
	if math.Abs((c.NewPnL-c.NewFee)-95) > 1e-9 {
		t.Errorf("net effect NewPnL-NewFee = %v, want 95 (migration must not move money)", c.NewPnL-c.NewFee)
	}
	if math.Abs(plan.NewCash-1095) > 1e-9 {
		t.Errorf("NewCash = %v, want 1095 (initial 1000 + net 95, identical to legacy replay)", plan.NewCash)
	}
	if math.Abs(plan.ReplayedCash-plan.NewCash) > 1e-9 {
		t.Errorf("pre-migration replay %v != post-migration cash %v; migration moved money", plan.ReplayedCash, plan.NewCash)
	}
}

// A caller passing an sc that is not among the coin's live peers, or a peer
// with zero virtual quantity, must get (0,0) — never the whole portfolio fill.
func TestHyperliquidKillSwitchFillShare_NonPeerFailsClosed(t *testing.T) {
	peers := []StrategyConfig{
		{ID: "hl-a", Platform: "hyperliquid", Type: "perps", Args: []string{"chk", "BTC", "--mode=live"}},
		{ID: "hl-b", Platform: "hyperliquid", Type: "perps", Args: []string{"chk", "BTC", "--mode=live"}},
	}
	vq := hlVirtualQuantitySnapshot{"BTC": {"hl-a": 1.0, "hl-b": 1.0}}

	// sc not in the peer set at all (different ID, absent from snapshot).
	outsider := StrategyConfig{ID: "hl-c", Platform: "hyperliquid", Type: "perps",
		Args: []string{"chk", "ETH", "--mode=live"}}
	if sz, fee := hyperliquidKillSwitchFillShare(outsider, "BTC", 2.0, 0.2, peers, vq); sz != 0 || fee != 0 {
		t.Fatalf("non-peer sc must fail closed to (0,0), got (%v,%v)", sz, fee)
	}

	// sc is a peer but has zero virtual quantity in the snapshot.
	vqZero := hlVirtualQuantitySnapshot{"BTC": {"hl-a": 0.0, "hl-b": 1.0}}
	if sz, fee := hyperliquidKillSwitchFillShare(peers[0], "BTC", 2.0, 0.2, peers, vqZero); sz != 0 || fee != 0 {
		t.Fatalf("zero-virtual-qty peer must fail closed to (0,0), got (%v,%v)", sz, fee)
	}
}

// A single live strategy on the coin owns the entire fill unchanged.
func TestHyperliquidKillSwitchFillShare_SinglePeerGetsFullFill(t *testing.T) {
	sc := StrategyConfig{ID: "hl-a", Platform: "hyperliquid", Type: "perps",
		Args: []string{"chk", "BTC", "--mode=live"}}
	sz, fee := hyperliquidKillSwitchFillShare(sc, "BTC", 1.5, 0.3,
		[]StrategyConfig{sc}, hlVirtualQuantitySnapshot{})
	if sz != 1.5 || fee != 0.3 {
		t.Fatalf("single peer must receive the full fill unchanged, got (%v,%v)", sz, fee)
	}
}

// Uneven virtual quantities split proportionally and the shares sum exactly
// to the portfolio fill totals.
func TestHyperliquidKillSwitchFillShare_UnevenSplitSumsToTotal(t *testing.T) {
	a := StrategyConfig{ID: "hl-a", Platform: "hyperliquid", Type: "perps",
		Args: []string{"chk", "BTC", "--mode=live"}}
	b := StrategyConfig{ID: "hl-b", Platform: "hyperliquid", Type: "perps",
		Args: []string{"chk", "BTC", "--mode=live"}}
	peers := []StrategyConfig{a, b}
	vq := hlVirtualQuantitySnapshot{"BTC": {"hl-a": 1.0, "hl-b": 2.0}}

	szA, feeA := hyperliquidKillSwitchFillShare(a, "BTC", 1.0, 0.1, peers, vq)
	szB, feeB := hyperliquidKillSwitchFillShare(b, "BTC", 1.0, 0.1, peers, vq)

	if math.Abs(szA-1.0/3.0) > 1e-9 || math.Abs(szB-2.0/3.0) > 1e-9 {
		t.Errorf("size shares = %v/%v, want 1/3 and 2/3", szA, szB)
	}
	if math.Abs(feeA-0.1/3.0) > 1e-9 || math.Abs(feeB-0.2/3.0) > 1e-9 {
		t.Errorf("fee shares = %v/%v, want 0.1/3 and 0.2/3", feeA, feeB)
	}
	if math.Abs((szA+szB)-1.0) > 1e-9 || math.Abs((feeA+feeB)-0.1) > 1e-9 {
		t.Errorf("shares must sum to the fill totals: sz %v fee %v", szA+szB, feeA+feeB)
	}
}

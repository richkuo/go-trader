package main

import (
	"fmt"
	"math"
	"path/filepath"
	"testing"
	"time"
)

// #954 trade-ledger display path: gross PnL convention helpers, ledger-derived
// shared-wallet member values, funding/transfer ingestion, OID dedup, and the
// `backfill trade-ledger` planner.

// --- Gross/net convention helpers ---

func TestTradeNetPnLAndLedgerDelta(t *testing.T) {
	cases := []struct {
		name      string
		tr        Trade
		wantNet   float64
		wantDelta float64
	}{
		{
			name:      "gross close: net = pnl - fee",
			tr:        Trade{IsClose: true, RealizedPnL: 100, ExchangeFee: 0.7, PnLGross: true},
			wantNet:   99.3,
			wantDelta: 99.3,
		},
		{
			name:      "legacy close: realized_pnl is already net",
			tr:        Trade{IsClose: true, RealizedPnL: 99.3, ExchangeFee: 0.7, PnLGross: false},
			wantNet:   99.3,
			wantDelta: 99.3,
		},
		{
			name:      "gross open: pnl=0, delta = -fee",
			tr:        Trade{IsClose: false, RealizedPnL: 0, ExchangeFee: 0.5, PnLGross: true},
			wantNet:   -0.5,
			wantDelta: -0.5,
		},
		{
			name:      "legacy open with stamped fee: delta = -fee",
			tr:        Trade{IsClose: false, RealizedPnL: 0, ExchangeFee: 0.4, PnLGross: false},
			wantNet:   0,
			wantDelta: -0.4,
		},
		{
			name:      "funding row (gross, no fee): delta = amount",
			tr:        Trade{TradeType: TradeTypeFunding, RealizedPnL: -1.25, PnLGross: true},
			wantNet:   -1.25,
			wantDelta: -1.25,
		},
		{
			name:      "gross close with maker rebate (negative fee)",
			tr:        Trade{IsClose: true, RealizedPnL: 50, ExchangeFee: -0.1, PnLGross: true},
			wantNet:   50.1,
			wantDelta: 50.1,
		},
	}
	for _, tc := range cases {
		if got := tradeNetPnL(tc.tr); math.Abs(got-tc.wantNet) > 1e-9 {
			t.Errorf("%s: tradeNetPnL = %v, want %v", tc.name, got, tc.wantNet)
		}
		if got := tradeLedgerDelta(tc.tr); math.Abs(got-tc.wantDelta) > 1e-9 {
			t.Errorf("%s: tradeLedgerDelta = %v, want %v", tc.name, got, tc.wantDelta)
		}
	}
}

func newLedgerTestDB(t *testing.T) *StateDB {
	t.Helper()
	db, err := OpenStateDB(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatalf("OpenStateDB: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

// The SQL ledger sum must mirror tradeLedgerDelta row-for-row across both
// conventions plus funding rows, grouped per strategy.
func TestLedgerNetByStrategy_ConventionAware(t *testing.T) {
	db := newLedgerTestDB(t)
	now := time.Now().UTC()
	rows := []Trade{
		// hl-a: gross open (fee 0.5) + gross close (100 gross, fee 0.7) + funding -1.25.
		{Timestamp: now, StrategyID: "hl-a", Symbol: "BTC", Side: "buy", TradeType: "perps", ExchangeFee: 0.5, PnLGross: true, FeeSource: FeeSourceUserFills},
		{Timestamp: now, StrategyID: "hl-a", Symbol: "BTC", Side: "sell", TradeType: "perps", IsClose: true, RealizedPnL: 100, ExchangeFee: 0.7, PnLGross: true, FeeSource: ""},
		{Timestamp: now, StrategyID: "hl-a", Symbol: "BTC", Side: "funding", TradeType: TradeTypeFunding, RealizedPnL: -1.25, PnLGross: true},
		// hl-b: legacy open with stamped fee + legacy close (net).
		{Timestamp: now, StrategyID: "hl-b", Symbol: "ETH", Side: "buy", TradeType: "perps", ExchangeFee: 0.4},
		{Timestamp: now, StrategyID: "hl-b", Symbol: "ETH", Side: "sell", TradeType: "perps", IsClose: true, RealizedPnL: 19.6},
	}
	for _, tr := range rows {
		if err := db.InsertTrade(tr.StrategyID, tr); err != nil {
			t.Fatalf("InsertTrade: %v", err)
		}
	}
	got, err := db.LedgerNetByStrategy([]string{"hl-a", "hl-b", "hl-empty"})
	if err != nil {
		t.Fatalf("LedgerNetByStrategy: %v", err)
	}
	if want := -0.5 + (100 - 0.7) + -1.25; math.Abs(got["hl-a"]-want) > 1e-9 {
		t.Errorf("hl-a ledger = %v, want %v", got["hl-a"], want)
	}
	if want := -0.4 + 19.6; math.Abs(got["hl-b"]-want) > 1e-9 {
		t.Errorf("hl-b ledger = %v, want %v", got["hl-b"], want)
	}
	if _, ok := got["hl-empty"]; ok {
		t.Error("strategy with no rows must be absent (treated as 0)")
	}
}

// --- Ledger-derived member values (pure math) ---

func TestLedgerSharedWalletMemberValues_Math(t *testing.T) {
	in := ledgerWalletInputs{
		Members:     []string{"hl-btc", "hl-eth"},
		InitialByID: map[string]float64{"hl-btc": 600, "hl-eth": 400},
		LedgerByID:  map[string]float64{"hl-btc": 25, "hl-eth": -10},
		Positions: []SharedWalletPosition{
			{Coin: "BTC", UnrealizedPnL: 50},
			{Coin: "ETH", UnrealizedPnL: -20},
			{Coin: "DOGE", UnrealizedPnL: 7}, // orphan: no virtual owner
		},
		VirtualQty: map[string]map[string]float64{
			"BTC": {"hl-btc": 0.1},
			"ETH": {"hl-eth": 2},
		},
		// Ledger-derived sum = (600+25+50) + (400-10-20) = 675 + 370 = 1045.
		// Balance includes a $100 deposit (NonTradeFlows) and the orphan's +7.
		AccountBalance: 1045 + 100 + 7,
		NonTradeFlows:  100,
		BaselineOffset: 0,
		BaselineSet:    true,
	}
	res, rawDrift := ledgerSharedWalletMemberValues(in)
	if math.Abs(res.Values["hl-btc"]-675) > 0.001 {
		t.Errorf("hl-btc = %v, want 675 (initial+ledger+ownedUPnL)", res.Values["hl-btc"])
	}
	if math.Abs(res.Values["hl-eth"]-370) > 0.001 {
		t.Errorf("hl-eth = %v, want 370", res.Values["hl-eth"])
	}
	// rawDrift = balance − Σvalues − flows = 1152 − 1045 − 100 = 7 (the orphan).
	if math.Abs(rawDrift-7) > 0.001 {
		t.Errorf("rawDrift = %v, want 7 (orphan uPnL)", rawDrift)
	}
	if math.Abs(res.Drift-7) > 0.001 {
		t.Errorf("Drift = %v, want 7 (baseline 0)", res.Drift)
	}
	if len(res.OrphanCoins) != 1 || res.OrphanCoins[0] != "DOGE" {
		t.Errorf("OrphanCoins = %v, want [DOGE]", res.OrphanCoins)
	}
}

// Member values come from the ledger, NOT a balance split: an idle member's
// value must be exactly initial+0+0 regardless of how wrong the balance is.
func TestLedgerSharedWalletMemberValues_IdleMemberIndependentOfBalance(t *testing.T) {
	in := ledgerWalletInputs{
		Members:        []string{"hl-idle", "hl-active"},
		InitialByID:    map[string]float64{"hl-idle": 500, "hl-active": 500},
		LedgerByID:     map[string]float64{"hl-active": -123.45},
		AccountBalance: 700, // badly drifted balance
		BaselineSet:    true,
	}
	res, _ := ledgerSharedWalletMemberValues(in)
	if res.Values["hl-idle"] != 500 {
		t.Errorf("idle member = %v, want exactly 500 (no drift inherited)", res.Values["hl-idle"])
	}
	if math.Abs(res.Drift-(700-500-376.55)) > 0.001 {
		t.Errorf("Drift = %v, want %v", res.Drift, 700-500-376.55)
	}
}

func TestLedgerSharedWalletMemberValues_BaselineAnchorsDrift(t *testing.T) {
	in := ledgerWalletInputs{
		Members:        []string{"hl-a"},
		InitialByID:    map[string]float64{"hl-a": 1000},
		AccountBalance: 950, // $50 of pre-adoption history not in the ledger
	}
	// First cycle (BaselineSet=false): drift reads 0, rawDrift returned for storage.
	res, rawDrift := ledgerSharedWalletMemberValues(in)
	if res.Drift != 0 {
		t.Errorf("first-cycle drift = %v, want 0", res.Drift)
	}
	if math.Abs(rawDrift-(-50)) > 0.001 {
		t.Fatalf("rawDrift = %v, want -50", rawDrift)
	}
	// Later cycles measure NEW divergence vs the stored baseline.
	in.BaselineSet = true
	in.BaselineOffset = rawDrift
	res, _ = ledgerSharedWalletMemberValues(in)
	if math.Abs(res.Drift) > 0.001 {
		t.Errorf("steady-state drift = %v, want 0 (anchored)", res.Drift)
	}
	in.AccountBalance = 950 - 3 // a fill the ledger missed
	res, _ = ledgerSharedWalletMemberValues(in)
	if math.Abs(res.Drift-(-3)) > 0.001 {
		t.Errorf("new-divergence drift = %v, want -3", res.Drift)
	}
}

// --- Full reconcile cycle through the DB-backed HL ledger path ---

func TestReconcileSharedWalletDisplayValues_HLLedgerPath(t *testing.T) {
	t.Setenv("HYPERLIQUID_ACCOUNT_ADDRESS", "0xtest")
	db := newLedgerTestDB(t)
	now := time.Now().UTC()
	// hl-btc has a gross close (+40 gross, fee 1) and an open fee (0.5):
	// ledger = +38.5. hl-eth has no rows: ledger = 0.
	for _, tr := range []Trade{
		{Timestamp: now, StrategyID: "hl-btc", Symbol: "BTC", Side: "buy", TradeType: "perps", ExchangeFee: 0.5, PnLGross: true},
		{Timestamp: now, StrategyID: "hl-btc", Symbol: "BTC", Side: "sell", TradeType: "perps", IsClose: true, RealizedPnL: 40, ExchangeFee: 1, PnLGross: true},
	} {
		if err := db.InsertTrade(tr.StrategyID, tr); err != nil {
			t.Fatalf("InsertTrade: %v", err)
		}
	}
	if err := db.InsertWalletTransfer("hyperliquid", "0xtest", now.UnixMilli(), "deposit", 100, "dep1"); err != nil {
		t.Fatalf("InsertWalletTransfer: %v", err)
	}
	// fetchWalletLedgerEvents owns first-contact init of the watermark row;
	// seed it here as that cycle would (reconcile never originates the row —
	// see TestReconcileSharedWalletDisplayValues_HLMissingLedgerStateFallsBack).
	if err := db.UpsertWalletLedgerState("hyperliquid", "0xtest", WalletLedgerState{
		FundingSinceMs: now.UnixMilli(), TransfersSinceMs: now.UnixMilli(),
	}); err != nil {
		t.Fatalf("seed ledger state: %v", err)
	}

	strategies := []StrategyConfig{
		{ID: "hl-btc", Platform: "hyperliquid", Type: "perps", Args: []string{"sma", "BTC", "1h", "--mode=live"}, Capital: 600},
		{ID: "hl-eth", Platform: "hyperliquid", Type: "perps", Args: []string{"rsi", "ETH", "1h", "--mode=live"}, Capital: 400},
	}
	state := &AppState{Strategies: map[string]*StrategyState{
		"hl-btc": {ID: "hl-btc", Cash: 638.5, Positions: map[string]*Position{
			"BTC": {Symbol: "BTC", Side: "long", Quantity: 0.1, AvgCost: 60000},
		}, InitialCapital: 600},
		"hl-eth": {ID: "hl-eth", Cash: 400, Positions: map[string]*Position{}, InitialCapital: 400},
	}}
	sharedWallets := detectSharedWallets(strategies)
	key := SharedWalletKey{Platform: "hyperliquid", Account: "0xtest"}
	hlPositions := []HLPosition{{Coin: "BTC", Size: 0.1, UnrealizedPnL: 12}}
	// Expected: btc = 600 + 38.5 + 12 = 650.5; eth = 400.
	// Clean balance = Σvalues + flows = 1050.5 + 100 = 1150.5; start drifted +$5.
	walletBalances := map[SharedWalletKey]float64{key: 1155.5}

	// Cycle 1: baseline anchors at rawDrift=+5, reported drift 0.
	results := reconcileSharedWalletDisplayValues(strategies, state, db, sharedWallets, walletBalances, hlPositions, nil, false)
	if len(results) != 1 || math.Abs(results[0].Drift) > 0.001 {
		t.Fatalf("cycle 1: want drift 0 (baseline anchor), got %+v", results)
	}
	if got := state.Strategies["hl-btc"].SharedWalletValue; math.Abs(got-650.5) > 0.001 {
		t.Errorf("hl-btc display = %v, want 650.5 (initial+ledger+uPnL)", got)
	}
	if got := state.Strategies["hl-eth"].SharedWalletValue; math.Abs(got-400) > 0.001 {
		t.Errorf("hl-eth display = %v, want 400 (idle: initial only)", got)
	}
	if !state.Strategies["hl-btc"].SharedWalletValueSet || !state.Strategies["hl-eth"].SharedWalletValueSet {
		t.Error("both members must be gated on")
	}
	st, found, err := db.GetWalletLedgerState("hyperliquid", "0xtest")
	if err != nil || !found || !st.BaselineSet || math.Abs(st.BaselineOffset-5) > 0.001 {
		t.Fatalf("baseline not stored: found=%v err=%v st=%+v", found, err, st)
	}
	if st.FundingSinceMs != now.UnixMilli() || st.TransfersSinceMs != now.UnixMilli() {
		t.Fatalf("baseline upsert clobbered watermarks: %+v", st)
	}

	// Cycle 2: balance loses $3 the ledger didn't book → drift -3.
	walletBalances[key] = 1152.5
	results = reconcileSharedWalletDisplayValues(strategies, state, db, sharedWallets, walletBalances, hlPositions, nil, false)
	if len(results) != 1 || math.Abs(results[0].Drift-(-3)) > 0.001 {
		t.Fatalf("cycle 2: want drift -3 vs baseline, got %+v", results)
	}
	// Member values are ledger-derived → unchanged by the balance move.
	if got := state.Strategies["hl-btc"].SharedWalletValue; math.Abs(got-650.5) > 0.001 {
		t.Errorf("hl-btc display moved with balance: %v, want 650.5", got)
	}

	// Baseline reset (backfill --apply) → next cycle re-anchors, drift 0 again.
	if err := db.ResetWalletLedgerBaseline("hyperliquid", "0xtest"); err != nil {
		t.Fatalf("ResetWalletLedgerBaseline: %v", err)
	}
	results = reconcileSharedWalletDisplayValues(strategies, state, db, sharedWallets, walletBalances, hlPositions, nil, false)
	if len(results) != 1 || math.Abs(results[0].Drift) > 0.001 {
		t.Fatalf("post-reset: want drift 0 (re-anchored), got %+v", results)
	}
}

// A nil StateDB must fall back to the #918 split (rows stay populated).
func TestReconcileSharedWalletDisplayValues_HLNilDBFallsBackToSplit(t *testing.T) {
	t.Setenv("HYPERLIQUID_ACCOUNT_ADDRESS", "0xtest")
	strategies := []StrategyConfig{
		{ID: "hl-a", Platform: "hyperliquid", Type: "perps", Args: []string{"sma", "BTC", "1h", "--mode=live"}, Capital: 600},
		{ID: "hl-b", Platform: "hyperliquid", Type: "perps", Args: []string{"rsi", "ETH", "1h", "--mode=live"}, Capital: 400},
	}
	state := &AppState{Strategies: map[string]*StrategyState{
		"hl-a": {ID: "hl-a", Cash: 600, Positions: map[string]*Position{}},
		"hl-b": {ID: "hl-b", Cash: 400, Positions: map[string]*Position{}},
	}}
	sharedWallets := detectSharedWallets(strategies)
	key := SharedWalletKey{Platform: "hyperliquid", Account: "0xtest"}
	walletBalances := map[SharedWalletKey]float64{key: 1000}

	results := reconcileSharedWalletDisplayValues(strategies, state, nil, sharedWallets, walletBalances, nil, nil, false)
	if len(results) != 1 {
		t.Fatalf("want 1 result via split fallback, got %d", len(results))
	}
	// Split semantics: capital-weight share of the balance.
	if got := state.Strategies["hl-a"].SharedWalletValue; math.Abs(got-600) > 0.001 {
		t.Errorf("fallback hl-a = %v, want 600 (0.6 × 1000)", got)
	}
	if !state.Strategies["hl-a"].SharedWalletValueSet {
		t.Error("fallback must still gate members on")
	}
}

// Reconcile must never originate the watermark row (#969 review): if
// fetchWalletLedgerEvents' first-contact init failed this cycle, the row is
// absent here and upserting a baseline would persist watermarks of 0 — the
// next fetch would then replay the wallet's entire funding history past a
// baseline that never accounted for it. Absent row → split fallback, no row
// written; once fetch re-inits the row, the ledger path anchors normally.
func TestReconcileSharedWalletDisplayValues_HLMissingLedgerStateFallsBack(t *testing.T) {
	t.Setenv("HYPERLIQUID_ACCOUNT_ADDRESS", "0xtest")
	db := newLedgerTestDB(t)
	now := time.Now().UTC()
	strategies := []StrategyConfig{
		{ID: "hl-a", Platform: "hyperliquid", Type: "perps", Args: []string{"sma", "BTC", "1h", "--mode=live"}, Capital: 600},
		{ID: "hl-b", Platform: "hyperliquid", Type: "perps", Args: []string{"rsi", "ETH", "1h", "--mode=live"}, Capital: 400},
	}
	state := &AppState{Strategies: map[string]*StrategyState{
		"hl-a": {ID: "hl-a", Cash: 600, Positions: map[string]*Position{}, InitialCapital: 600},
		"hl-b": {ID: "hl-b", Cash: 400, Positions: map[string]*Position{}, InitialCapital: 400},
	}}
	sharedWallets := detectSharedWallets(strategies)
	key := SharedWalletKey{Platform: "hyperliquid", Account: "0xtest"}
	// Balance $100 above Σinitial distinguishes the paths: the split spreads
	// it capital-weight (hl-a 660); the ledger path keeps values at initial
	// (hl-a 600) and would anchor the +100 into the baseline.
	walletBalances := map[SharedWalletKey]float64{key: 1100}

	// Cycle 1: no watermark row → split fallback, and no row may be created.
	results := reconcileSharedWalletDisplayValues(strategies, state, db, sharedWallets, walletBalances, nil, nil, false)
	if len(results) != 1 {
		t.Fatalf("want 1 result via split fallback, got %d", len(results))
	}
	if got := state.Strategies["hl-a"].SharedWalletValue; math.Abs(got-660) > 0.001 {
		t.Errorf("fallback hl-a = %v, want 660 (0.6 × 1100 split)", got)
	}
	if _, found, err := db.GetWalletLedgerState("hyperliquid", "0xtest"); err != nil || found {
		t.Fatalf("reconcile originated the watermark row: found=%v err=%v", found, err)
	}

	// Cycle 2: fetch init succeeded (row anchored at now) → ledger path
	// baselines the +100 and reports drift 0 with ledger-derived values.
	if err := db.UpsertWalletLedgerState("hyperliquid", "0xtest", WalletLedgerState{
		FundingSinceMs: now.UnixMilli(), TransfersSinceMs: now.UnixMilli(),
	}); err != nil {
		t.Fatalf("seed ledger state: %v", err)
	}
	results = reconcileSharedWalletDisplayValues(strategies, state, db, sharedWallets, walletBalances, nil, nil, false)
	if len(results) != 1 || math.Abs(results[0].Drift) > 0.001 {
		t.Fatalf("cycle 2: want drift 0 (baseline anchor), got %+v", results)
	}
	if got := state.Strategies["hl-a"].SharedWalletValue; math.Abs(got-600) > 0.001 {
		t.Errorf("ledger hl-a = %v, want 600 (initial, no ledger rows)", got)
	}
	st, found, err := db.GetWalletLedgerState("hyperliquid", "0xtest")
	if err != nil || !found || !st.BaselineSet || math.Abs(st.BaselineOffset-100) > 0.001 {
		t.Fatalf("baseline not anchored after row init: found=%v err=%v st=%+v", found, err, st)
	}
	if st.FundingSinceMs != now.UnixMilli() {
		t.Fatalf("watermark clobbered: %+v", st)
	}
}

// --- signedPerpFlowUSD ---

func TestSignedPerpFlowUSD(t *testing.T) {
	acct := "0xME"
	cases := []struct {
		name   string
		d      hlLedgerEventDelta
		want   float64
		wantOK bool
	}{
		{"deposit", hlLedgerEventDelta{Type: "deposit", USDC: "100.5"}, 100.5, true},
		{"withdraw includes fee", hlLedgerEventDelta{Type: "withdraw", USDC: "50", Fee: "1"}, -51, true},
		{"class transfer to perp", hlLedgerEventDelta{Type: "accountClassTransfer", USDC: "25", ToPerp: true}, 25, true},
		{"class transfer to spot", hlLedgerEventDelta{Type: "accountClassTransfer", USDC: "25", ToPerp: false}, -25, true},
		{"internal transfer inbound", hlLedgerEventDelta{Type: "internalTransfer", USDC: "10", Destination: "0xme"}, 10, true},
		{"internal transfer outbound", hlLedgerEventDelta{Type: "internalTransfer", USDC: "10", Destination: "0xother"}, -10, true},
		{"subaccount inbound", hlLedgerEventDelta{Type: "subAccountTransfer", USDC: "7", Destination: "0xME"}, 7, true},
		{"vault deposit", hlLedgerEventDelta{Type: "vaultDeposit", USDC: "30"}, -30, true},
		// vaultWithdraw carries NO usdc field — the net amount is credited
		// (real shape: requestedUsd/commission/closingCost/netWithdrawnUsd).
		{"vault withdraw nets after commission", hlLedgerEventDelta{Type: "vaultWithdraw", NetWithdrawnUSD: "688.5"}, 688.5, true},
		{"vault create includes fee", hlLedgerEventDelta{Type: "vaultCreate", USDC: "100", Fee: "0.5"}, -100.5, true},
		{"spot transfer no perp effect", hlLedgerEventDelta{Type: "spotTransfer", USDC: "99"}, 0, true},
		{"core USDC send inbound", hlLedgerEventDelta{Type: "send", Token: "USDC", Amount: "20", Destination: "0xME"}, 20, true},
		{"core USDC send outbound with fee", hlLedgerEventDelta{Type: "send", Token: "USDC", Amount: "20", Fee: "1", Destination: "0xpeer"}, -21, true},
		{"non-USDC send is spot-side", hlLedgerEventDelta{Type: "send", Token: "PURR", Amount: "50000", Destination: "0xpeer"}, 0, true},
		{"dex-routed USDC send unmapped", hlLedgerEventDelta{Type: "send", Token: "USDC", Amount: "20", DestinationDex: "builder"}, 0, false},
		{"outbound internal transfer includes fee", hlLedgerEventDelta{Type: "internalTransfer", USDC: "10", Fee: "1", Destination: "0xother"}, -11, true},
		{"USDC rewards claim credits", hlLedgerEventDelta{Type: "rewardsClaim", Token: "USDC", Amount: "12.5"}, 12.5, true},
		{"token rewards claim spot-side", hlLedgerEventDelta{Type: "rewardsClaim", Token: "HYPE", Amount: "3"}, 0, true},
		{"gas auction spot-side", hlLedgerEventDelta{Type: "gossipPriorityGasAuction", Token: "HYPE", Amount: "0.98"}, 0, true},
		{"staking transfer spot-side", hlLedgerEventDelta{Type: "cStakingTransfer", Token: "HYPE", Amount: "10"}, 0, true},
		{"liquidation informational (impact via fills)", hlLedgerEventDelta{Type: "liquidation"}, 0, true},
		{"unknown kind", hlLedgerEventDelta{Type: "mysteryNewKind", USDC: "5"}, 0, false},
	}
	for _, tc := range cases {
		got, ok := signedPerpFlowUSD(tc.d, acct)
		if math.Abs(got-tc.want) > 1e-9 || ok != tc.wantOK {
			t.Errorf("%s: got (%v, %v), want (%v, %v)", tc.name, got, ok, tc.want, tc.wantOK)
		}
	}
}

// --- Funding ingestion ---

func TestIngestFundingEvent_SplitsByQtyShareAndDedups(t *testing.T) {
	db := newLedgerTestDB(t)
	prev := tradeRecorder
	tradeRecorder = db.InsertTrade
	defer func() { tradeRecorder = prev }()

	state := &AppState{Strategies: map[string]*StrategyState{
		"hl-a": {ID: "hl-a", Positions: map[string]*Position{
			"BTC": {Symbol: "BTC", Side: "long", Quantity: 0.3, AvgCost: 60000},
		}},
		"hl-b": {ID: "hl-b", Positions: map[string]*Position{
			"BTC": {Symbol: "BTC", Side: "long", Quantity: 0.1, AvgCost: 61000},
		}},
	}}
	key := SharedWalletKey{Platform: "hyperliquid", Account: "0xtest"}
	virtualQty := map[string]map[string]float64{"BTC": {"hl-a": 0.3, "hl-b": 0.1}}
	ev := hlLedgerEvent{Time: 1700000000000, Hash: "0xabc", Delta: hlLedgerEventDelta{Type: "funding", Coin: "BTC", USDC: "-4.0"}}

	if ok := ingestFundingEvent(db, state, key, ev, virtualQty); !ok {
		t.Fatal("ingestFundingEvent returned false")
	}
	a, b := state.Strategies["hl-a"], state.Strategies["hl-b"]
	if len(a.TradeHistory) != 1 || len(b.TradeHistory) != 1 {
		t.Fatalf("want 1 funding row each, got %d/%d", len(a.TradeHistory), len(b.TradeHistory))
	}
	// −4.0 split 0.3:0.1 → −3.0 / −1.0, gross convention, funding type.
	if tr := a.TradeHistory[0]; math.Abs(tr.RealizedPnL-(-3)) > 1e-9 || tr.TradeType != TradeTypeFunding || !tr.PnLGross {
		t.Errorf("hl-a funding row = %+v, want RealizedPnL -3 gross funding", tr)
	}
	if tr := b.TradeHistory[0]; math.Abs(tr.RealizedPnL-(-1)) > 1e-9 {
		t.Errorf("hl-b funding share = %v, want -1", tr.RealizedPnL)
	}

	// Re-ingest the same event (watermark-overlap re-read) → no new rows.
	if ok := ingestFundingEvent(db, state, key, ev, virtualQty); !ok {
		t.Fatal("re-ingest returned false")
	}
	if len(a.TradeHistory) != 1 || len(b.TradeHistory) != 1 {
		t.Fatalf("dedup failed: got %d/%d rows", len(a.TradeHistory), len(b.TradeHistory))
	}
	// And the ledger sums see the shares.
	sums, err := db.LedgerNetByStrategy([]string{"hl-a", "hl-b"})
	if err != nil {
		t.Fatalf("LedgerNetByStrategy: %v", err)
	}
	if math.Abs(sums["hl-a"]-(-3)) > 1e-9 || math.Abs(sums["hl-b"]-(-1)) > 1e-9 {
		t.Errorf("ledger sums = %v, want hl-a:-3 hl-b:-1", sums)
	}
}

// A funding row whose eager DB insert fails must HOLD the watermark — a
// crash before the SaveState flush would otherwise lose the row behind an
// advanced watermark (permanent ledger shortfall, drift alarm forever).
// Already-persisted co-owners must not double-book on the retry.
func TestIngestFundingEvent_PersistFailureHoldsWatermark(t *testing.T) {
	db := newLedgerTestDB(t)
	prev := tradeRecorder
	defer func() { tradeRecorder = prev }()

	state := &AppState{Strategies: map[string]*StrategyState{
		"hl-a": {ID: "hl-a", Positions: map[string]*Position{
			"BTC": {Symbol: "BTC", Side: "long", Quantity: 0.3, AvgCost: 60000},
		}},
		"hl-b": {ID: "hl-b", Positions: map[string]*Position{
			"BTC": {Symbol: "BTC", Side: "long", Quantity: 0.1, AvgCost: 61000},
		}},
	}}
	key := SharedWalletKey{Platform: "hyperliquid", Account: "0xtest"}
	virtualQty := map[string]map[string]float64{"BTC": {"hl-a": 0.3, "hl-b": 0.1}}
	ev := hlLedgerEvent{Time: 1700000000000, Hash: "0xfail", Delta: hlLedgerEventDelta{Type: "funding", Coin: "BTC", USDC: "-4.0"}}

	// hl-a persists, hl-b's eager insert fails (owners iterate sorted).
	tradeRecorder = func(strategyID string, trade Trade) error {
		if strategyID == "hl-b" {
			return errInjectedPersist
		}
		return db.InsertTrade(strategyID, trade)
	}
	if ok := ingestFundingEvent(db, state, key, ev, virtualQty); ok {
		t.Fatal("persist failure must return false (hold the watermark)")
	}
	if got := len(state.Strategies["hl-a"].TradeHistory); got != 1 {
		t.Fatalf("hl-a rows = %d, want 1 (persisted before the failure)", got)
	}

	// Same cycle retry: hl-b's row is in TradeHistory but NOT on disk —
	// still held (the watermark may only advance once it is durable).
	tradeRecorder = db.InsertTrade
	if ok := ingestFundingEvent(db, state, key, ev, virtualQty); ok {
		t.Fatal("unpersisted in-memory row must keep holding the watermark")
	}

	// SaveState-equivalent flush lands the row → retry succeeds, advances.
	bss := state.Strategies["hl-b"]
	if n := len(bss.TradeHistory); n != 1 {
		t.Fatalf("hl-b rows = %d, want 1 (booked in memory despite failed persist)", n)
	}
	if err := db.InsertTrade("hl-b", bss.TradeHistory[0]); err != nil {
		t.Fatalf("flush: %v", err)
	}
	bss.TradeHistory[0].persisted = true
	if ok := ingestFundingEvent(db, state, key, ev, virtualQty); !ok {
		t.Fatal("fully persisted split must release the watermark")
	}
	// No double-booking anywhere.
	if len(state.Strategies["hl-a"].TradeHistory) != 1 || len(bss.TradeHistory) != 1 {
		t.Fatalf("retry double-booked: %d/%d rows", len(state.Strategies["hl-a"].TradeHistory), len(bss.TradeHistory))
	}
	sums, err := db.LedgerNetByStrategy([]string{"hl-a", "hl-b"})
	if err != nil || math.Abs(sums["hl-a"]-(-3)) > 1e-9 || math.Abs(sums["hl-b"]-(-1)) > 1e-9 {
		t.Errorf("ledger sums = %v (err %v), want hl-a:-3 hl-b:-1", sums, err)
	}
}

// End-to-end through ingestWalletLedgerEvents: the funding watermark must not
// advance while a row's persist fails, and must advance after recovery.
func TestIngestWalletLedgerEvents_FundingWatermarkHeldOnPersistFailure(t *testing.T) {
	db := newLedgerTestDB(t)
	prev := tradeRecorder
	defer func() { tradeRecorder = prev }()
	key := SharedWalletKey{Platform: "hyperliquid", Account: "0xtest"}
	st := WalletLedgerState{FundingSinceMs: 1000, TransfersSinceMs: 1000}
	if err := db.UpsertWalletLedgerState(key.Platform, key.Account, st); err != nil {
		t.Fatalf("UpsertWalletLedgerState: %v", err)
	}
	state := &AppState{Strategies: map[string]*StrategyState{
		"hl-a": {ID: "hl-a", Positions: map[string]*Position{
			"BTC": {Symbol: "BTC", Side: "long", Quantity: 0.3, AvgCost: 60000},
		}},
	}}
	virtualQty := map[string]map[string]float64{"BTC": {"hl-a": 0.3}}
	res := walletLedgerFetchResult{
		Key: key, State: st, StateFound: true,
		Funding: []hlLedgerEvent{
			{Time: 2000, Hash: "0xf1", Delta: hlLedgerEventDelta{Type: "funding", Coin: "BTC", USDC: "-1.0"}},
		},
		FundingFetched: true,
	}

	tradeRecorder = func(string, Trade) error { return errInjectedPersist }
	ingestWalletLedgerEvents(db, state, res, virtualQty)
	got, _, err := db.GetWalletLedgerState(key.Platform, key.Account)
	if err != nil || got.FundingSinceMs != 1000 {
		t.Fatalf("funding watermark = %d (err %v), want 1000 (held on persist failure)", got.FundingSinceMs, err)
	}

	// Recovery: flush the stranded row, then the next cycle advances.
	ass := state.Strategies["hl-a"]
	if err := db.InsertTrade("hl-a", ass.TradeHistory[0]); err != nil {
		t.Fatalf("flush: %v", err)
	}
	ass.TradeHistory[0].persisted = true
	tradeRecorder = db.InsertTrade
	ingestWalletLedgerEvents(db, state, res, virtualQty)
	got, _, _ = db.GetWalletLedgerState(key.Platform, key.Account)
	if got.FundingSinceMs != 2001 {
		t.Errorf("funding watermark = %d, want 2001 after recovery", got.FundingSinceMs)
	}
	if len(ass.TradeHistory) != 1 {
		t.Errorf("recovery double-booked: %d rows", len(ass.TradeHistory))
	}
}

// With no eager persistence configured (tradeRecorder=nil, batch SaveState
// flush), a successful split must still advance — persisted=false is the
// legitimate steady state there, not a failure signal.
func TestIngestFundingEvent_NoEagerPersistStillAdvances(t *testing.T) {
	db := newLedgerTestDB(t)
	prev := tradeRecorder
	tradeRecorder = nil
	defer func() { tradeRecorder = prev }()
	state := &AppState{Strategies: map[string]*StrategyState{
		"hl-a": {ID: "hl-a", Positions: map[string]*Position{
			"BTC": {Symbol: "BTC", Side: "long", Quantity: 0.3, AvgCost: 60000},
		}},
	}}
	key := SharedWalletKey{Platform: "hyperliquid", Account: "0xtest"}
	ev := hlLedgerEvent{Time: 1700000000000, Hash: "0xnil", Delta: hlLedgerEventDelta{Type: "funding", Coin: "BTC", USDC: "-2.0"}}
	if ok := ingestFundingEvent(db, state, key, ev, map[string]map[string]float64{"BTC": {"hl-a": 0.3}}); !ok {
		t.Fatal("batch-persist context must not hold the watermark")
	}
}

func TestIngestFundingEvent_OrphanCoinGoesToWalletTransfers(t *testing.T) {
	db := newLedgerTestDB(t)
	state := &AppState{Strategies: map[string]*StrategyState{}}
	key := SharedWalletKey{Platform: "hyperliquid", Account: "0xtest"}
	ev := hlLedgerEvent{Time: 1700000000000, Hash: "0xdef", Delta: hlLedgerEventDelta{Type: "funding", Coin: "DOGE", USDC: "2.5"}}

	if ok := ingestFundingEvent(db, state, key, ev, nil); !ok {
		t.Fatal("orphan funding ingest returned false")
	}
	sum, err := db.SumWalletTransfers("hyperliquid", "0xtest")
	if err != nil || math.Abs(sum-2.5) > 1e-9 {
		t.Fatalf("orphan funding flow = %v (err %v), want 2.5 in wallet_transfers", sum, err)
	}
	// Idempotent on re-read (UNIQUE dedup_id).
	if ok := ingestFundingEvent(db, state, key, ev, nil); !ok {
		t.Fatal("orphan re-ingest returned false")
	}
	sum, _ = db.SumWalletTransfers("hyperliquid", "0xtest")
	if math.Abs(sum-2.5) > 1e-9 {
		t.Fatalf("orphan funding duplicated: sum = %v, want 2.5", sum)
	}
}

func TestIngestWalletLedgerEvents_TransfersAndWatermarks(t *testing.T) {
	db := newLedgerTestDB(t)
	key := SharedWalletKey{Platform: "hyperliquid", Account: "0xtest"}
	st := WalletLedgerState{FundingSinceMs: 1000, TransfersSinceMs: 1000}
	if err := db.UpsertWalletLedgerState(key.Platform, key.Account, st); err != nil {
		t.Fatalf("UpsertWalletLedgerState: %v", err)
	}
	state := &AppState{Strategies: map[string]*StrategyState{}}
	res := walletLedgerFetchResult{
		Key: key, State: st, StateFound: true,
		Transfers: []hlLedgerEvent{
			{Time: 500, Hash: "0xold", Delta: hlLedgerEventDelta{Type: "deposit", USDC: "999"}}, // pre-watermark: skipped
			{Time: 2000, Hash: "0xd1", Delta: hlLedgerEventDelta{Type: "deposit", USDC: "100"}},
			{Time: 3000, Hash: "0xw1", Delta: hlLedgerEventDelta{Type: "withdraw", USDC: "40", Fee: "1"}},
		},
		TransfersFetched: true,
	}
	ingestWalletLedgerEvents(db, state, res, nil)

	sum, err := db.SumWalletTransfers(key.Platform, key.Account)
	if err != nil || math.Abs(sum-(100-41)) > 1e-9 {
		t.Fatalf("transfer sum = %v (err %v), want 59", sum, err)
	}
	got, found, err := db.GetWalletLedgerState(key.Platform, key.Account)
	if err != nil || !found {
		t.Fatalf("GetWalletLedgerState: found=%v err=%v", found, err)
	}
	if got.TransfersSinceMs != 3001 {
		t.Errorf("transfers watermark = %d, want 3001 (max processed + 1)", got.TransfersSinceMs)
	}
	if got.FundingSinceMs != 1000 {
		t.Errorf("funding watermark moved without a funding fetch: %d, want 1000", got.FundingSinceMs)
	}

	// Replaying the same fetch result inserts nothing new (dedup_id UNIQUE).
	ingestWalletLedgerEvents(db, state, res, nil)
	sum, _ = db.SumWalletTransfers(key.Platform, key.Account)
	if math.Abs(sum-59) > 1e-9 {
		t.Fatalf("replay duplicated transfers: sum = %v, want 59", sum)
	}
}

// --- Booking convention + OID dedup at the perps close site ---

func TestBookPerpsClose_GrossConventionAndOIDDedup(t *testing.T) {
	s := &StrategyState{
		ID: "hl-x", Platform: "hyperliquid", Type: "perps", Cash: 1000,
		Positions: map[string]*Position{
			"BTC": {Symbol: "BTC", Side: "long", Quantity: 0.1, AvgCost: 60000},
		},
	}
	// Real fill: closePx 61000, fee $2.10 from userFills.
	if ok := bookPerpsCloseWithFillFee(s, "BTC", 61000, 2.10, true, "12345", "test_close", "test", "test", nil); !ok {
		t.Fatal("bookPerpsCloseWithFillFee returned false")
	}
	if len(s.TradeHistory) != 1 {
		t.Fatalf("want 1 trade, got %d", len(s.TradeHistory))
	}
	tr := s.TradeHistory[0]
	// Gross convention: RealizedPnL is PRE-FEE (0.1×1000 = 100); fee stamped.
	if !tr.PnLGross || math.Abs(tr.RealizedPnL-100) > 1e-9 || math.Abs(tr.ExchangeFee-2.10) > 1e-9 {
		t.Errorf("trade = pnl %v fee %v gross %v, want 100 / 2.10 / true", tr.RealizedPnL, tr.ExchangeFee, tr.PnLGross)
	}
	if tr.FeeSource != FeeSourceUserFills {
		t.Errorf("FeeSource = %q, want %q", tr.FeeSource, FeeSourceUserFills)
	}
	// Cash moves by NET (100 − 2.10).
	if math.Abs(s.Cash-1097.90) > 1e-9 {
		t.Errorf("cash = %v, want 1097.90 (net)", s.Cash)
	}
	if math.Abs(tradeNetPnL(tr)-97.90) > 1e-9 {
		t.Errorf("tradeNetPnL = %v, want 97.90", tradeNetPnL(tr))
	}

	// Same OID arrives again via a racing path with a re-materialized position:
	// must clear the position WITHOUT a second Trade or cash change.
	s.Positions["BTC"] = &Position{Symbol: "BTC", Side: "long", Quantity: 0.1, AvgCost: 60000}
	cashBefore := s.Cash
	if ok := bookPerpsCloseWithFillFee(s, "BTC", 61000, 2.10, true, "12345", "test_close", "test", "test", nil); !ok {
		t.Fatal("dup-OID close must still report handled (true)")
	}
	if len(s.TradeHistory) != 1 {
		t.Fatalf("dup OID booked a second Trade: %d rows", len(s.TradeHistory))
	}
	if s.Cash != cashBefore {
		t.Errorf("dup OID moved cash: %v → %v", cashBefore, s.Cash)
	}
	if _, still := s.Positions["BTC"]; still {
		t.Error("dup OID must still clear the virtual position")
	}
}

func TestBookPerpsClose_ModeledFeeStampsGrossRow(t *testing.T) {
	s := &StrategyState{
		ID: "hl-x", Platform: "hyperliquid", Type: "perps", Cash: 1000,
		Positions: map[string]*Position{
			"ETH": {Symbol: "ETH", Side: "short", Quantity: 1, AvgCost: 3000},
		},
	}
	if ok := bookPerpsCloseWithFillFee(s, "2900", 2900, 0, false, "", "test_close", "t", "t", nil); ok {
		t.Fatal("bad symbol must not book") // guard the helper wiring below
	}
	if ok := bookPerpsCloseWithFillFee(s, "ETH", 2900, 0, false, "", "test_close", "t", "t", nil); !ok {
		t.Fatal("close failed")
	}
	tr := s.TradeHistory[0]
	wantFee := CalculatePlatformSpotFee("hyperliquid", 2900)
	if !tr.PnLGross || math.Abs(tr.ExchangeFee-wantFee) > 1e-9 || tr.FeeSource != FeeSourceModeled {
		t.Errorf("modeled-fee row = fee %v src %q gross %v, want %v / modeled / true", tr.ExchangeFee, tr.FeeSource, tr.PnLGross, wantFee)
	}
	if math.Abs(tr.RealizedPnL-100) > 1e-9 { // short: 1×(3000−2900) gross
		t.Errorf("gross pnl = %v, want 100", tr.RealizedPnL)
	}
}

// --- backfill trade-ledger planner ---

func TestPlanTradeLedgerForStrategy_MigratesAndTruesUp(t *testing.T) {
	base := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	trades := []TradeBackfillRow{
		// Legacy open, fee unstamped (modeled at booking): 0.1×60000×0.00035 = 2.1.
		{RowID: 1, Timestamp: base, Symbol: "BTC", Side: "buy", Quantity: 0.1, Price: 60000, Value: 6000, ExchangeOrderID: "100"},
		// Legacy close, net pnl 95, fee unstamped.
		{RowID: 2, Timestamp: base.Add(time.Hour), Symbol: "BTC", Side: "sell", Quantity: 0.1, Price: 61000, Value: 6100, IsClose: true, RealizedPnL: 95, ExchangeOrderID: "200"},
		// Funding row: untouched, never in cash.
		{RowID: 3, Timestamp: base.Add(2 * time.Hour), Symbol: "BTC", TradeType: TradeTypeFunding, RealizedPnL: -1, PnLGross: true},
	}
	fills := map[string]HLFillSummary{
		"100": {Fee: 1.95, Qty: 0.1, Px: 60010},
		"200": {Fee: 2.05, ClosedPnLGross: 99, Qty: 0.1, Px: 60990},
	}
	plan := planTradeLedgerForStrategy("hl-x", trades, fills, 1000, 0)

	if plan.MigratedCount != 2 || plan.MatchedCount != 2 {
		t.Fatalf("migrated=%d matched=%d, want 2/2", plan.MigratedCount, plan.MatchedCount)
	}
	if len(plan.Changes) != 2 {
		t.Fatalf("want 2 changes (funding untouched), got %d", len(plan.Changes))
	}
	byRow := map[int64]TradeLedgerChange{}
	for _, c := range plan.Changes {
		byRow[c.RowID] = c
	}
	open, close := byRow[1], byRow[2]
	if math.Abs(open.NewFee-1.95) > 1e-9 || math.Abs(open.NewPrice-60010) > 1e-9 {
		t.Errorf("open true-up: fee %v px %v, want 1.95 / 60010", open.NewFee, open.NewPrice)
	}
	if math.Abs(close.NewPnL-99) > 1e-9 || math.Abs(close.NewFee-2.05) > 1e-9 {
		t.Errorf("close true-up: pnl %v fee %v, want gross 99 / 2.05", close.NewPnL, close.NewFee)
	}
	// Cash replay: 1000 − 1.95 + (99 − 2.05) = 1095.
	if math.Abs(plan.NewCash-1095) > 1e-9 {
		t.Errorf("NewCash = %v, want 1095", plan.NewCash)
	}
}

func TestPlanTradeLedgerForStrategy_SkipsReconcileAdjustmentRows(t *testing.T) {
	base := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	trades := []TradeBackfillRow{
		{
			RowID: 1, Timestamp: base, Symbol: "ETH", Side: "sell", Quantity: 0.5,
			Price: 2100, Value: 1050, TradeType: "perps", IsClose: true,
			RealizedPnL: 50, PnLGross: true, FeeSource: FeeSourceReconcileAdjustment,
		},
		{
			RowID: 2, Timestamp: base.Add(time.Minute), Symbol: "BTC", Side: "sell", Quantity: 0.1,
			Price: 61000, Value: 6100, TradeType: "perps", IsClose: true,
			RealizedPnL: 95, ExchangeOrderID: "matched-adjustment", FeeSource: FeeSourceReconcileAdjustment,
		},
	}
	fills := map[string]HLFillSummary{
		"matched-adjustment": {Fee: 2.05, ClosedPnLGross: 99, Qty: 0.1, Px: 60990},
	}

	plan := planTradeLedgerForStrategy("hl-residual", trades, fills, 1000, 1145)
	if len(plan.Changes) != 0 {
		t.Fatalf("changes = %d, want 0 for model-only adjustment", len(plan.Changes))
	}
	if plan.MigratedCount != 0 || plan.MatchedCount != 0 {
		t.Fatalf("migrated/matched = %d/%d, want 0/0 for adjustment rows", plan.MigratedCount, plan.MatchedCount)
	}
	if plan.ReconcileAdjustCount != 2 || plan.MissingOIDCount != 0 || plan.UnmatchedOIDCount != 0 {
		t.Fatalf("skip counts = reconcile_adjustment %d missing %d unmatched %d, want 2/0/0",
			plan.ReconcileAdjustCount, plan.MissingOIDCount, plan.UnmatchedOIDCount)
	}
	if len(plan.Skipped) != 2 {
		t.Fatalf("skipped = %+v, want two reconcile_adjustment skips", plan.Skipped)
	}
	for _, skipped := range plan.Skipped {
		if skipped.Reason != "reconcile_adjustment" {
			t.Fatalf("skipped = %+v, want only reconcile_adjustment skips", plan.Skipped)
		}
	}
	if math.Abs(plan.NewCash-1145) > 1e-9 {
		t.Errorf("NewCash = %v, want 1145 (rows are replayed, only userFills true-up is skipped)", plan.NewCash)
	}

	second := planTradeLedgerForStrategy("hl-residual", trades, fills, 1000, plan.NewCash)
	if len(second.Changes) != 0 || second.ReconcileAdjustCount != 2 || second.CashBaselineDivergent {
		t.Fatalf("second pass = changes %d reconcile_adjustment %d divergent %v, want 0 / 2 / false",
			len(second.Changes), second.ReconcileAdjustCount, second.CashBaselineDivergent)
	}
}

// Running the planner a second time over its own output must be a no-op.
func TestPlanTradeLedgerForStrategy_Idempotent(t *testing.T) {
	base := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	trades := []TradeBackfillRow{
		{RowID: 1, Timestamp: base, Symbol: "BTC", Side: "buy", Quantity: 0.1, Price: 60000, Value: 6000, ExchangeOrderID: "100"},
		{RowID: 2, Timestamp: base.Add(time.Hour), Symbol: "BTC", Side: "sell", Quantity: 0.1, Price: 61000, Value: 6100, IsClose: true, RealizedPnL: 95, ExchangeOrderID: "200"},
	}
	fills := map[string]HLFillSummary{
		"100": {Fee: 1.95, Qty: 0.1, Px: 60010},
		"200": {Fee: 2.05, ClosedPnLGross: 99, Qty: 0.1, Px: 60990},
	}
	first := planTradeLedgerForStrategy("hl-x", trades, fills, 1000, 0)

	// Apply the plan to the rows (what ApplyTradeLedgerPlan writes to disk).
	byRow := map[int64]TradeLedgerChange{}
	for _, c := range first.Changes {
		byRow[c.RowID] = c
	}
	applied := make([]TradeBackfillRow, len(trades))
	copy(applied, trades)
	for i := range applied {
		if c, ok := byRow[applied[i].RowID]; ok {
			applied[i].Price = c.NewPrice
			applied[i].Value = c.NewValue
			applied[i].ExchangeFee = c.NewFee
			applied[i].RealizedPnL = c.NewPnL
			applied[i].PnLGross = true
			applied[i].FeeSource = c.NewFeeSource
		}
	}
	second := planTradeLedgerForStrategy("hl-x", applied, fills, 1000, first.NewCash)
	if len(second.Changes) != 0 {
		t.Fatalf("second run not idempotent: %d changes (%+v)", len(second.Changes), second.Changes)
	}
	if second.CashBaselineDivergent {
		t.Error("second-run pre-replay must match the corrected cash")
	}
	if math.Abs(second.NewCash-first.NewCash) > 1e-9 {
		t.Errorf("cash drifted across runs: %v vs %v", second.NewCash, first.NewCash)
	}
}

// Two partial close legs sharing one OID apportion the userFills aggregate by
// quantity share instead of each absorbing the full fee/closedPnl.
func TestPlanTradeLedgerForStrategy_SharedOIDApportionsByQty(t *testing.T) {
	base := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	trades := []TradeBackfillRow{
		{RowID: 1, Timestamp: base, Symbol: "BTC", Side: "sell", Quantity: 0.3, Price: 61000, Value: 18300, IsClose: true, RealizedPnL: 70, ExchangeOrderID: "500"},
		{RowID: 2, Timestamp: base.Add(time.Minute), Symbol: "BTC", Side: "sell", Quantity: 0.1, Price: 61000, Value: 6100, IsClose: true, RealizedPnL: 23, ExchangeOrderID: "500"},
	}
	fills := map[string]HLFillSummary{
		"500": {Fee: 4, ClosedPnLGross: 100, Qty: 0.4, Px: 61000},
	}
	plan := planTradeLedgerForStrategy("hl-x", trades, fills, 1000, 0)
	byRow := map[int64]TradeLedgerChange{}
	for _, c := range plan.Changes {
		byRow[c.RowID] = c
	}
	if c := byRow[1]; math.Abs(c.NewFee-3) > 1e-9 || math.Abs(c.NewPnL-75) > 1e-9 {
		t.Errorf("leg 1 (0.3/0.4): fee %v pnl %v, want 3 / 75", c.NewFee, c.NewPnL)
	}
	if c := byRow[2]; math.Abs(c.NewFee-1) > 1e-9 || math.Abs(c.NewPnL-25) > 1e-9 {
		t.Errorf("leg 2 (0.1/0.4): fee %v pnl %v, want 1 / 25", c.NewFee, c.NewPnL)
	}
}

func TestPlanTradeLedgerForStrategy_SharedWalletOIDTotalsAcrossStrategies(t *testing.T) {
	t.Setenv("HYPERLIQUID_ACCOUNT_ADDRESS", "0xabc")
	base := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	tradesA := []TradeBackfillRow{
		{RowID: 1, Timestamp: base, Symbol: "ETH", Side: "sell", Quantity: 1.5, Price: 3100, Value: 4650,
			IsClose: true, RealizedPnL: 150, ExchangeFee: 0, PnLGross: true, ExchangeOrderID: "98765"},
	}
	tradesB := []TradeBackfillRow{
		{RowID: 2, Timestamp: base, Symbol: "ETH", Side: "sell", Quantity: 0.5, Price: 3100, Value: 1550,
			IsClose: true, RealizedPnL: 50, ExchangeFee: 0, PnLGross: true, ExchangeOrderID: "98765"},
	}
	strategies := []StrategyConfig{
		{ID: "hl-a", Platform: "hyperliquid", Type: "perps", Args: []string{"tema", "ETH", "1h", "--mode=live"}},
		{ID: "hl-b", Platform: "hyperliquid", Type: "perps", Args: []string{"rmc", "ETH", "1h", "--mode=live"}},
	}
	totalsByID := tradeLedgerSharedWalletOIDTotals(strategies, map[string][]TradeBackfillRow{
		"hl-a": tradesA,
		"hl-b": tradesB,
	})
	fills := map[string]HLFillSummary{
		"98765": {Fee: 4, ClosedPnLGross: 400, Qty: 2, Px: 3200},
	}

	planA := planTradeLedgerForStrategyWithOIDTotals("hl-a", tradesA, fills, 1000, 0, totalsByID["hl-a"])
	planB := planTradeLedgerForStrategyWithOIDTotals("hl-b", tradesB, fills, 500, 0, totalsByID["hl-b"])

	if len(planA.Changes) != 1 || len(planB.Changes) != 1 {
		t.Fatalf("changes A/B = %d/%d, want 1/1", len(planA.Changes), len(planB.Changes))
	}
	a, b := planA.Changes[0], planB.Changes[0]
	if math.Abs(a.NewFee-3) > 1e-9 || math.Abs(a.NewPnL-300) > 1e-9 {
		t.Errorf("strategy A share = fee %v pnl %v, want 3 / 300", a.NewFee, a.NewPnL)
	}
	if math.Abs(b.NewFee-1) > 1e-9 || math.Abs(b.NewPnL-100) > 1e-9 {
		t.Errorf("strategy B share = fee %v pnl %v, want 1 / 100", b.NewFee, b.NewPnL)
	}
	if math.Abs(a.NewFee+b.NewFee-4) > 1e-9 {
		t.Errorf("fee sum = %v, want aggregate 4", a.NewFee+b.NewFee)
	}
	if math.Abs(a.NewPnL+b.NewPnL-400) > 1e-9 {
		t.Errorf("pnl sum = %v, want aggregate 400", a.NewPnL+b.NewPnL)
	}
	if math.Abs(planA.NewCash-1297) > 1e-9 || math.Abs(planB.NewCash-599) > 1e-9 {
		t.Errorf("cash A/B = %v/%v, want 1297/599", planA.NewCash, planB.NewCash)
	}
}

// A flip order's close and open legs share one OID: the fee apportions
// across BOTH legs (the exchange charged it on the whole order) while the
// closedPnl lands entirely on the close leg.
func TestPlanTradeLedgerForStrategy_FlipOIDFeeAcrossLegsPnLOnClose(t *testing.T) {
	base := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	trades := []TradeBackfillRow{
		{RowID: 1, Timestamp: base, Symbol: "ETH", Side: "buy", Quantity: 0.5, Price: 2000, Value: 1000, IsClose: true,
			RealizedPnL: 60, ExchangeFee: 0.2625, PnLGross: true, FeeSource: FeeSourceUserFills, ExchangeOrderID: "900"},
		{RowID: 2, Timestamp: base, Symbol: "ETH", Side: "buy", Quantity: 0.3, Price: 2000, Value: 600,
			ExchangeFee: 0.1575, PnLGross: true, FeeSource: FeeSourceUserFills, ExchangeOrderID: "900"},
	}
	fills := map[string]HLFillSummary{
		"900": {Fee: 0.42, ClosedPnLGross: 55, Qty: 0.8, Px: 2000},
	}
	plan := planTradeLedgerForStrategy("hl-x", trades, fills, 1000, 0)
	byRow := map[int64]TradeLedgerChange{}
	for _, c := range plan.Changes {
		byRow[c.RowID] = c
	}
	// Close leg: fee 0.42×(0.5/0.8)=0.2625 (already right), pnl trued to the full 55.
	if c, ok := byRow[1]; !ok || math.Abs(c.NewFee-0.2625) > 1e-9 || math.Abs(c.NewPnL-55) > 1e-9 {
		t.Errorf("close leg = %+v, want fee 0.2625 pnl 55 (full closedPnl)", c)
	}
	// Open leg: fee 0.42×(0.3/0.8)=0.1575 — must NOT absorb the full 0.42.
	if c, ok := byRow[2]; ok && math.Abs(c.NewFee-0.1575) > 1e-9 {
		t.Errorf("open leg fee = %v, want 0.1575 (qty share of the whole order)", c.NewFee)
	}
}

// `backfill hl-fees` must not reinterpret #954 gross rows — they are owned by
// `backfill trade-ledger`.
func TestPlanBackfillForStrategy_SkipsGrossConventionRows(t *testing.T) {
	base := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	trades := []TradeBackfillRow{
		{RowID: 1, Timestamp: base, Symbol: "BTC", Quantity: 0.1, Value: 6100, IsClose: true,
			RealizedPnL: 100, ExchangeFee: 2, PnLGross: true, ExchangeOrderID: "100", FeeSource: FeeSourceUserFills},
	}
	fills := map[string]HLFillSummary{"100": {Fee: 9, ClosedPnLGross: 50}}
	plan := planBackfillForStrategy("hl-x", trades, fills, 1000, 1098)
	if len(plan.TradeChanges) != 0 {
		t.Fatalf("hl-fees must not rewrite gross rows, got %d changes", len(plan.TradeChanges))
	}
	found := false
	for _, sk := range plan.Skipped {
		if sk.RowID == 1 && sk.Reason == "gross_convention_row" {
			found = true
		}
	}
	if !found {
		t.Errorf("want gross_convention_row skip, got %+v", plan.Skipped)
	}
	// Replayed as net: cash = 1000 + (100−2) = 1098.
	if math.Abs(plan.NewCash-1098) > 1e-9 {
		t.Errorf("NewCash = %v, want 1098 (net replay of gross row)", plan.NewCash)
	}
}

// --- Apply path round-trip through SQLite ---

func TestApplyTradeLedgerPlan_RoundTrip(t *testing.T) {
	db := newLedgerTestDB(t)
	now := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	// Seed a legacy close row + the strategy cash row via SaveState-equivalent inserts.
	if err := db.InsertTrade("hl-x", Trade{
		Timestamp: now, StrategyID: "hl-x", Symbol: "BTC", Side: "sell", Quantity: 0.1,
		Price: 61000, Value: 6100, TradeType: "perps", IsClose: true, RealizedPnL: 95, ExchangeOrderID: "200",
	}); err != nil {
		t.Fatalf("InsertTrade: %v", err)
	}
	if _, err := db.db.Exec(`INSERT INTO strategies (id, type, cash) VALUES ('hl-x', 'perps', 1095)`); err != nil {
		t.Fatalf("seed strategies: %v", err)
	}

	rows, err := db.ListTradesForBackfill("hl-x")
	if err != nil || len(rows) != 1 {
		t.Fatalf("ListTradesForBackfill: %v (%d rows)", err, len(rows))
	}
	if rows[0].PnLGross {
		t.Fatal("seed row must be legacy")
	}
	fills := map[string]HLFillSummary{"200": {Fee: 2.05, ClosedPnLGross: 99, Qty: 0.1, Px: 60990}}
	plan := planTradeLedgerForStrategy("hl-x", rows, fills, 1000, 1095)
	if err := db.ApplyTradeLedgerPlan(plan); err != nil {
		t.Fatalf("ApplyTradeLedgerPlan: %v", err)
	}

	got, err := db.ListTradesForBackfill("hl-x")
	if err != nil || len(got) != 1 {
		t.Fatalf("re-list: %v", err)
	}
	r := got[0]
	if !r.PnLGross || math.Abs(r.RealizedPnL-99) > 1e-9 || math.Abs(r.ExchangeFee-2.05) > 1e-9 || r.FeeSource != FeeSourceUserFills {
		t.Errorf("applied row = %+v, want gross 99 / fee 2.05 / userfills", r)
	}
	var cash float64
	if err := db.db.QueryRow(`SELECT cash FROM strategies WHERE id='hl-x'`).Scan(&cash); err != nil {
		t.Fatalf("read cash: %v", err)
	}
	if want := 1000 + 99 - 2.05; math.Abs(cash-want) > 1e-9 {
		t.Errorf("cash = %v, want %v", cash, want)
	}
	// Ledger sum now reflects the corrected net.
	sums, err := db.LedgerNetByStrategy([]string{"hl-x"})
	if err != nil || math.Abs(sums["hl-x"]-(99-2.05)) > 1e-9 {
		t.Errorf("ledger sum = %v (err %v), want 96.95", sums["hl-x"], err)
	}
}

// errInjectedPersist simulates an eager trade-persist failure.
var errInjectedPersist = fmt.Errorf("injected persist failure")

// A matched row whose ONLY stale column is value (price already equals the
// VWAP) must still be rewritten — value is one of the columns the repair owns.
func TestPlanTradeLedgerForStrategy_StaleValueAloneTriggersRewrite(t *testing.T) {
	base := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	trades := []TradeBackfillRow{
		{RowID: 1, Timestamp: base, Symbol: "BTC", Side: "buy", Quantity: 0.1, Price: 60010,
			Value:       5000, // inconsistent with qty*price (= 6001)
			ExchangeFee: 1.95, PnLGross: true, FeeSource: FeeSourceUserFills, ExchangeOrderID: "100"},
	}
	fills := map[string]HLFillSummary{"100": {Fee: 1.95, Qty: 0.1, Px: 60010}}
	plan := planTradeLedgerForStrategy("hl-x", trades, fills, 1000, 0)
	if len(plan.Changes) != 1 {
		t.Fatalf("stale value alone must trigger a rewrite, got %d changes", len(plan.Changes))
	}
	if c := plan.Changes[0]; math.Abs(c.NewValue-6001) > 1e-9 {
		t.Errorf("NewValue = %v, want 6001 (qty × VWAP)", c.NewValue)
	}
	// Idempotent after the repair; fully-correct rows stay untouched.
	trades[0].Value = 6001
	if second := planTradeLedgerForStrategy("hl-x", trades, fills, 1000, plan.NewCash); len(second.Changes) != 0 {
		t.Errorf("corrected row re-flagged: %+v", second.Changes)
	}
}

// The post-apply baseline reset must touch ONLY wallets whose members were
// repaired — clearing an untouched wallet's baseline would fold its genuine
// standing drift into a fresh offset and silence a real alarm.
func TestResetWalletBaselinesForAppliedStrategies_Scoped(t *testing.T) {
	t.Setenv("HYPERLIQUID_ACCOUNT_ADDRESS", "0xmain")
	db := newLedgerTestDB(t)
	seed := WalletLedgerState{FundingSinceMs: 1, TransfersSinceMs: 1, BaselineOffset: 7.5, BaselineSet: true}
	for _, acct := range []string{"0xmain", "0xother"} {
		if err := db.UpsertWalletLedgerState("hyperliquid", acct, seed); err != nil {
			t.Fatalf("seed %s: %v", acct, err)
		}
	}
	// Two wallets via per-strategy account overrides (walletKeyRegistry reads
	// hl_account_address from args-independent config Account fields); use
	// the strategies' resolved accounts as detectSharedWallets sees them.
	strategies := []StrategyConfig{
		{ID: "hl-a1", Platform: "hyperliquid", Type: "perps", Args: []string{"sma", "BTC", "1h", "--mode=live"}},
		{ID: "hl-a2", Platform: "hyperliquid", Type: "perps", Args: []string{"rsi", "ETH", "1h", "--mode=live"}},
	}
	resetWalletBaselinesForAppliedStrategies(db, strategies, map[string]bool{"hl-a1": true})

	got, _, err := db.GetWalletLedgerState("hyperliquid", "0xmain")
	if err != nil || got.BaselineSet {
		t.Errorf("repaired wallet baseline = %+v (err %v), want cleared", got, err)
	}
	other, _, err := db.GetWalletLedgerState("hyperliquid", "0xother")
	if err != nil || !other.BaselineSet || math.Abs(other.BaselineOffset-7.5) > 1e-9 {
		t.Errorf("untouched wallet baseline = %+v (err %v), want preserved 7.5", other, err)
	}
	// No applied members → nothing reset.
	if err := db.UpsertWalletLedgerState("hyperliquid", "0xmain", seed); err != nil {
		t.Fatalf("re-seed: %v", err)
	}
	resetWalletBaselinesForAppliedStrategies(db, strategies, map[string]bool{})
	got, _, _ = db.GetWalletLedgerState("hyperliquid", "0xmain")
	if !got.BaselineSet {
		t.Error("no-op apply must reset no baselines")
	}
}

// A failure on an event sharing a millisecond with an already-processed
// sibling must NOT advance the watermark past that millisecond — otherwise
// the failed event is never re-fetched (permanent ledger shortfall). HL
// emits same-ms events routinely (two coins funding at one hourly tick).
func TestIngestWalletLedgerEvents_SameMsFailureDoesNotSkipEvent(t *testing.T) {
	db := newLedgerTestDB(t)
	prev := tradeRecorder
	defer func() { tradeRecorder = prev }()
	key := SharedWalletKey{Platform: "hyperliquid", Account: "0xtest"}
	st := WalletLedgerState{FundingSinceMs: 1000, TransfersSinceMs: 1000}
	if err := db.UpsertWalletLedgerState(key.Platform, key.Account, st); err != nil {
		t.Fatalf("UpsertWalletLedgerState: %v", err)
	}
	state := &AppState{Strategies: map[string]*StrategyState{
		"hl-a": {ID: "hl-a", Positions: map[string]*Position{
			"BTC": {Symbol: "BTC", Side: "long", Quantity: 0.3, AvgCost: 60000},
		}},
		"hl-b": {ID: "hl-b", Positions: map[string]*Position{
			"ETH": {Symbol: "ETH", Side: "long", Quantity: 1, AvgCost: 3000},
		}},
	}}
	virtualQty := map[string]map[string]float64{"BTC": {"hl-a": 0.3}, "ETH": {"hl-b": 1}}
	res := walletLedgerFetchResult{
		Key: key, State: st, StateFound: true,
		Funding: []hlLedgerEvent{
			{Time: 2000, Hash: "0xbtc", Delta: hlLedgerEventDelta{Type: "funding", Coin: "BTC", USDC: "-1.0"}},
			{Time: 2000, Hash: "0xeth", Delta: hlLedgerEventDelta{Type: "funding", Coin: "ETH", USDC: "-2.0"}},
		},
		FundingFetched: true,
	}
	// hl-a (BTC) persists; hl-b (ETH) fails — both events share t=2000.
	tradeRecorder = func(strategyID string, trade Trade) error {
		if strategyID == "hl-b" {
			return errInjectedPersist
		}
		return db.InsertTrade(strategyID, trade)
	}
	ingestWalletLedgerEvents(db, state, res, virtualQty)
	got, _, err := db.GetWalletLedgerState(key.Platform, key.Account)
	if err != nil || got.FundingSinceMs > 2000 {
		t.Fatalf("funding watermark = %d (err %v), must not pass 2000 (failed same-ms event)", got.FundingSinceMs, err)
	}

	// SaveState-equivalent flush lands hl-b's stranded in-memory row (the
	// conservative hold keeps the watermark until the row is durable), then
	// the recovery cycle re-fetches both events: BTC dedup-skips, ETH's
	// flushed row dedup-skips too, and the watermark releases.
	bss := state.Strategies["hl-b"]
	if err := db.InsertTrade("hl-b", bss.TradeHistory[0]); err != nil {
		t.Fatalf("flush: %v", err)
	}
	bss.TradeHistory[0].persisted = true
	tradeRecorder = db.InsertTrade
	res.State = got
	ingestWalletLedgerEvents(db, state, res, virtualQty)
	got, _, _ = db.GetWalletLedgerState(key.Platform, key.Account)
	if got.FundingSinceMs != 2001 {
		t.Errorf("funding watermark = %d, want 2001 after recovery", got.FundingSinceMs)
	}
	if len(state.Strategies["hl-a"].TradeHistory) != 1 || len(state.Strategies["hl-b"].TradeHistory) != 1 {
		t.Errorf("rows = %d/%d, want 1/1 (no loss, no double-book)",
			len(state.Strategies["hl-a"].TradeHistory), len(state.Strategies["hl-b"].TradeHistory))
	}
	sums, err := db.LedgerNetByStrategy([]string{"hl-a", "hl-b"})
	if err != nil || math.Abs(sums["hl-a"]-(-1)) > 1e-9 || math.Abs(sums["hl-b"]-(-2)) > 1e-9 {
		t.Errorf("ledger sums = %v (err %v), want hl-a:-1 hl-b:-2", sums, err)
	}
}

// A failure at a LATER millisecond must still advance the watermark past the
// processed prefix (no regression to never-advancing).
func TestIngestWalletLedgerEvents_LaterFailureStillAdvancesPastPrefix(t *testing.T) {
	db := newLedgerTestDB(t)
	prev := tradeRecorder
	defer func() { tradeRecorder = prev }()
	key := SharedWalletKey{Platform: "hyperliquid", Account: "0xtest"}
	st := WalletLedgerState{FundingSinceMs: 1000, TransfersSinceMs: 1000}
	if err := db.UpsertWalletLedgerState(key.Platform, key.Account, st); err != nil {
		t.Fatalf("UpsertWalletLedgerState: %v", err)
	}
	state := &AppState{Strategies: map[string]*StrategyState{
		"hl-a": {ID: "hl-a", Positions: map[string]*Position{
			"BTC": {Symbol: "BTC", Side: "long", Quantity: 0.3, AvgCost: 60000},
		}},
		"hl-b": {ID: "hl-b", Positions: map[string]*Position{
			"ETH": {Symbol: "ETH", Side: "long", Quantity: 1, AvgCost: 3000},
		}},
	}}
	virtualQty := map[string]map[string]float64{"BTC": {"hl-a": 0.3}, "ETH": {"hl-b": 1}}
	res := walletLedgerFetchResult{
		Key: key, State: st, StateFound: true,
		Funding: []hlLedgerEvent{
			{Time: 2000, Hash: "0xbtc", Delta: hlLedgerEventDelta{Type: "funding", Coin: "BTC", USDC: "-1.0"}},
			{Time: 2005, Hash: "0xeth", Delta: hlLedgerEventDelta{Type: "funding", Coin: "ETH", USDC: "-2.0"}},
		},
		FundingFetched: true,
	}
	tradeRecorder = func(strategyID string, trade Trade) error {
		if strategyID == "hl-b" {
			return errInjectedPersist
		}
		return db.InsertTrade(strategyID, trade)
	}
	ingestWalletLedgerEvents(db, state, res, virtualQty)
	got, _, err := db.GetWalletLedgerState(key.Platform, key.Account)
	if err != nil || got.FundingSinceMs != 2001 {
		t.Fatalf("funding watermark = %d (err %v), want 2001 (past processed prefix, before failed 2005)", got.FundingSinceMs, err)
	}
}

// The #455 boot-time Details-parse migration must never touch gross-convention
// rows: a zero-gross close (no-mark-price AvgCost booking, exact breakeven)
// has realized_pnl=0 with a NET "PnL: $..." token in Details — parsing it in
// while pnl_gross=1 stays set would double-subtract the fee via tradeNetPnL.
func TestBackfillTradeCloseFlags_SkipsGrossConventionRows(t *testing.T) {
	db := newLedgerTestDB(t)
	now := time.Now().UTC()
	// Gross zero-PnL close (the #954 no-mark-price booking shape).
	if err := db.InsertTrade("hl-x", Trade{
		Timestamp: now, StrategyID: "hl-x", Symbol: "ETH", Side: "sell", Quantity: 0.5,
		Price: 2000, Value: 1000, TradeType: "perps", IsClose: true,
		Details:     "External close @ mark, PnL: $-0.18 (fee $0.18)",
		RealizedPnL: 0, ExchangeFee: 0.18, PnLGross: true, FeeSource: FeeSourceModeled,
	}); err != nil {
		t.Fatalf("InsertTrade gross: %v", err)
	}
	// Legacy close row that the migration SHOULD repair.
	if err := db.InsertTrade("hl-x", Trade{
		Timestamp: now, StrategyID: "hl-x", Symbol: "BTC", Side: "sell", Quantity: 0.1,
		Price: 61000, Value: 6100, TradeType: "perps", IsClose: true,
		Details:     "Close long, PnL: $42.50 (fee $0.50)",
		RealizedPnL: 0,
	}); err != nil {
		t.Fatalf("InsertTrade legacy: %v", err)
	}
	// Run twice — restarts must be idempotent for the gross row.
	for i := 0; i < 2; i++ {
		if err := db.backfillTradeCloseFlags(); err != nil {
			t.Fatalf("backfillTradeCloseFlags run %d: %v", i+1, err)
		}
	}
	rows, err := db.ListTradesForBackfill("hl-x")
	if err != nil || len(rows) != 2 {
		t.Fatalf("rows: %v (err %v)", len(rows), err)
	}
	for _, r := range rows {
		switch r.Symbol {
		case "ETH":
			if r.RealizedPnL != 0 || !r.PnLGross {
				t.Errorf("gross row rewritten by legacy migration: pnl=%v gross=%v, want 0/true", r.RealizedPnL, r.PnLGross)
			}
		case "BTC":
			if math.Abs(r.RealizedPnL-42.50) > 1e-9 {
				t.Errorf("legacy row not migrated: pnl=%v, want 42.50", r.RealizedPnL)
			}
		}
	}
}

package main

import (
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

func withFailingHLLiveExposure(t *testing.T, err error) {
	t.Helper()
	t.Setenv("HYPERLIQUID_ACCOUNT_ADDRESS", "0xlimittest")
	orig := fetchHyperliquidStateFn
	fetchHyperliquidStateFn = func(string) (float64, []HLPosition, error) {
		return 0, nil, err
	}
	limitFillExposureAlerts.reset()
	t.Cleanup(func() { fetchHyperliquidStateFn = orig })
}

func withUnsetHLAccountAddress(t *testing.T) {
	t.Helper()
	t.Setenv("HYPERLIQUID_ACCOUNT_ADDRESS", "")
	limitFillExposureAlerts.reset()
	t.Cleanup(func() { limitFillExposureAlerts.reset() })
}

func seedLimitExposureRow(t *testing.T, db *StateDB, strategyID string, filled float64) PendingLimitOrder {
	t.Helper()
	row := PendingLimitOrder{
		StrategyID: strategyID, Symbol: "ETH", Side: "long", OrderOID: 9001,
		LimitPrice: 2000, OrderSize: 0.5, TIF: "Alo", EntryATR: 50,
		FilledSize: filled, CreatedAt: time.Now().UTC(),
	}
	id, err := db.InsertPendingLimitOrder(row)
	if err != nil {
		t.Fatalf("seed pending limit order: %v", err)
	}
	row.ID = id
	return row
}

func offBookFullFillStatus(filled float64) func(string, string, []int64, int64) (*HyperliquidLimitStatusResult, string, error) {
	return func(string, string, []int64, int64) (*HyperliquidLimitStatusResult, string, error) {
		return &HyperliquidLimitStatusResult{Orders: []HyperliquidLimitOrderStatus{
			{OID: 9001, Resting: limitTestBoolPtr(false), FilledSize: filled, AvgPx: 2000, Fee: 0.7, Count: 1},
		}}, "", nil
	}
}

func noCancelExpected(t *testing.T) func(string, string, int64) (*HyperliquidCancelOrderResult, string, error) {
	return func(string, string, int64) (*HyperliquidCancelOrderResult, string, error) {
		t.Error("cancel must not be called on an off-book row")
		return &HyperliquidCancelOrderResult{}, "", nil
	}
}

func TestClassifyLimitFillLiveExposureReadings(t *testing.T) {
	readErr := errors.New("http 503")
	cases := []struct {
		name        string
		bookedNet   float64
		signedDelta float64
		onChainNet  float64
		readErr     error
		want        limitFillExposureVerdict
	}{
		{"live exposure exactly backs a fresh fill", 0, 0.5, 0.5, nil, limitFillExposureLive},
		{"live exposure exceeds the fill", 0, 0.5, 0.9, nil, limitFillExposureLive},
		{"live exposure backs a short fill", 0, -0.5, -0.5, nil, limitFillExposureLive},
		{"shared coin whose account net covers both legs", 1.0, 0.5, 1.5, nil, limitFillExposureLive},
		{"account is flat on the coin", 0, 0.5, 0, nil, limitFillExposureUnbacked},
		{"peer leg remains but this fill was closed by hand", 1.0, 0.5, 1.0, nil, limitFillExposureUnbacked},
		{"account holds less than the fill claims", 0, 0.5, 0.2, nil, limitFillExposureUnbacked},
		{"account holds the opposite direction", 0, 0.5, -0.5, nil, limitFillExposureUnbacked},
		{"opposite legs would net the book to zero", -0.5, 0.5, 0.5, nil, limitFillExposureUnbacked},
		{"account state unreadable", 0, 0.5, 0, readErr, limitFillExposureUnreadable},
		{"account unreadable even where a net was cached", 1.0, 0.5, 1.5, readErr, limitFillExposureUnreadable},
		{"zero delta has nothing to confirm", 0, 0, 0.5, nil, limitFillExposureUnreadable},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := classifyLimitFillLiveExposure("ETH", tc.bookedNet, tc.signedDelta, tc.onChainNet, tc.readErr, 1)
			if got.Verdict != tc.want {
				t.Fatalf("verdict = %v, want %v (reason %q)", got.Verdict, tc.want, got.Reason)
			}
			if got.Verdict != limitFillExposureLive && got.Reason == "" {
				t.Error("a refusal must carry a reason the operator can act on")
			}
		})
	}
}

func TestHyperliquidBookedSignedNetForCoinCountsOnlyLiveHyperliquidLegs(t *testing.T) {
	live := StrategyConfig{ID: "hl-live", Platform: "hyperliquid", Type: "manual", Symbol: "ETH",
		Args: []string{"hold", "ETH", "30m", "--mode=live"}}
	short := StrategyConfig{ID: "hl-short", Platform: "hyperliquid", Type: "perps", Symbol: "ETH",
		Args: []string{"ema_crossover", "ETH", "1h", "--mode=live"}}
	paper := StrategyConfig{ID: "hl-paper", Platform: "hyperliquid", Type: "perps", Symbol: "ETH",
		Args: []string{"ema_crossover", "ETH", "1h"}}
	okx := StrategyConfig{ID: "okx-eth", Platform: "okx", Type: "perps", Symbol: "ETH",
		Args: []string{"ema_crossover", "ETH", "1h", "--mode=live"}}

	state := &AppState{Strategies: map[string]*StrategyState{
		"hl-live":  {ID: "hl-live", Positions: map[string]*Position{"ETH": {Symbol: "ETH", Side: "long", Quantity: 1.0}}},
		"hl-short": {ID: "hl-short", Positions: map[string]*Position{"ETH": {Symbol: "ETH", Side: "short", Quantity: 0.25}}},
		"hl-paper": {ID: "hl-paper", Positions: map[string]*Position{"ETH": {Symbol: "ETH", Side: "long", Quantity: 5.0}}},
		"okx-eth":  {ID: "okx-eth", Positions: map[string]*Position{"ETH": {Symbol: "ETH", Side: "long", Quantity: 7.0}}},
	}}

	got := hyperliquidBookedSignedNetForCoin([]StrategyConfig{live, short, paper, okx}, state, "ETH")
	if got != 0.75 {
		t.Fatalf("booked signed net = %g, want 0.75 (live Hyperliquid legs only, signed by side)", got)
	}
	if owners := hyperliquidLiveOwnersForCoin([]StrategyConfig{live, short, paper, okx}, "ETH"); owners != 2 {
		t.Fatalf("owners = %d, want 2 (paper and non-Hyperliquid legs carry no on-chain exposure)", owners)
	}
}

func TestHyperliquidOnChainNetForCoin(t *testing.T) {
	positions := []HLPosition{{Coin: "BTC", Size: 2.0}, {Coin: "ETH", Size: -0.5}}
	if got := hyperliquidOnChainNetForCoin(positions, "ETH"); got != -0.5 {
		t.Fatalf("ETH net = %g, want -0.5", got)
	}
	if got := hyperliquidOnChainNetForCoin(positions, "SOL"); got != 0 {
		t.Fatalf("absent coin net = %g, want 0", got)
	}
}

func TestReconcilePendingLimitOrdersRefusesFillWithNoLiveExposure(t *testing.T) {
	sc, state := newLimitTestStrategy()
	cfg := &Config{Strategies: []StrategyConfig{sc}}
	db := newLimitTestStateDB(t)
	var mu sync.RWMutex
	withStubbedHLLiveExposure(t)
	seedLimitExposureRow(t, db, sc.ID, 0)

	withStubbedLimitDeps(t, offBookFullFillStatus(0.5), noCancelExpected(t))

	notifier, mock := newOrphanLaneNotifier()
	alerts := reconcilePendingLimitOrders(state, cfg, db, &mu, notifier, nil)

	if len(alerts) != 0 {
		t.Fatalf("alerts = %+v, want none — a fill with no live exposure must book nothing", alerts)
	}
	if pos := state.Strategies[sc.ID].Positions["ETH"]; pos != nil {
		t.Fatalf("position = %+v, want none — the scheduler must not report a position the exchange does not hold", pos)
	}
	orders, err := db.LoadPendingLimitOrders()
	if err != nil {
		t.Fatalf("load rows: %v", err)
	}
	if len(orders) != 1 {
		t.Fatalf("rows = %+v, want the recovery record kept — no automatic path deletes a row carrying an unbooked fill", orders)
	}
	if orders[0].FilledSize != 0 {
		t.Fatalf("watermark = %g, want 0 — nothing was booked so nothing may advance the watermark", orders[0].FilledSize)
	}
	if len(mock.dms) != 1 || !strings.Contains(mock.dms[0].content, "NO LIVE EXPOSURE") {
		t.Fatalf("owner DM = %+v, want one refusal alert naming the missing exposure", mock.dms)
	}
	if !strings.Contains(mock.dms[0].content, "manual-clear-limit-row 9001 --flattened") {
		t.Fatalf("owner DM must name the only path that clears the record, got: %s", mock.dms[0].content)
	}
}

func TestReconcilePendingLimitOrdersRefusesFillAfterAManualCloseAndStrategyRestore(t *testing.T) {
	sc, state := newLimitTestStrategy()
	cfg := &Config{Strategies: []StrategyConfig{sc}}
	db := newLimitTestStateDB(t)
	var mu sync.RWMutex
	withStubbedHLLiveExposure(t, HLPosition{Coin: "BTC", Size: 3.0})
	seedLimitExposureRow(t, db, sc.ID, 0)

	withStubbedLimitDeps(t, offBookFullFillStatus(0.5), noCancelExpected(t))

	reconcilePendingLimitOrders(state, cfg, db, &mu, nil, nil)

	if pos := state.Strategies[sc.ID].Positions["ETH"]; pos != nil {
		t.Fatalf("position = %+v — restoring a strategy after a hand close must never re-open the position", pos)
	}
	orders, _ := db.LoadPendingLimitOrders()
	if len(orders) != 1 {
		t.Fatalf("rows = %+v, want the recovery record kept", orders)
	}
}

func TestReconcilePendingLimitOrdersDefersFillWhenAccountUnreadable(t *testing.T) {
	sc, state := newLimitTestStrategy()
	cfg := &Config{Strategies: []StrategyConfig{sc}}
	db := newLimitTestStateDB(t)
	var mu sync.RWMutex
	withFailingHLLiveExposure(t, errors.New("http 503 from api.hyperliquid.xyz"))
	seedLimitExposureRow(t, db, sc.ID, 0)

	withStubbedLimitDeps(t, offBookFullFillStatus(0.5), noCancelExpected(t))

	notifier, mock := newOrphanLaneNotifier()
	reconcilePendingLimitOrders(state, cfg, db, &mu, notifier, nil)

	if pos := state.Strategies[sc.ID].Positions["ETH"]; pos != nil {
		t.Fatalf("position = %+v — an unreadable account must defer, never book", pos)
	}
	orders, _ := db.LoadPendingLimitOrders()
	if len(orders) != 1 {
		t.Fatalf("rows = %+v — an unreadable account must defer, never delete", orders)
	}
	if len(mock.dms) != 1 || !strings.Contains(mock.dms[0].content, "account state unreadable") {
		t.Fatalf("owner DM = %+v, want one deferral alert", mock.dms)
	}
}

func TestReconcilePendingLimitOrdersDefersFillWhenAccountAddressUnset(t *testing.T) {
	sc, state := newLimitTestStrategy()
	cfg := &Config{Strategies: []StrategyConfig{sc}}
	db := newLimitTestStateDB(t)
	var mu sync.RWMutex
	withUnsetHLAccountAddress(t)
	seedLimitExposureRow(t, db, sc.ID, 0)

	withStubbedLimitDeps(t, offBookFullFillStatus(0.5), noCancelExpected(t))

	reconcilePendingLimitOrders(state, cfg, db, &mu, nil, nil)

	if pos := state.Strategies[sc.ID].Positions["ETH"]; pos != nil {
		t.Fatalf("position = %+v — with no account address there is no evidence of live exposure", pos)
	}
	if orders, _ := db.LoadPendingLimitOrders(); len(orders) != 1 {
		t.Fatalf("rows = %+v — the recovery record must survive a deferral", orders)
	}
}

func TestReconcilePendingLimitOrdersRefusesAPartialAddWithNoRoomOnChain(t *testing.T) {
	sc, state := newLimitTestStrategy()
	state.Strategies[sc.ID].Positions["ETH"] = &Position{
		Symbol: "ETH", Side: "long", Quantity: 0.4, InitialQuantity: 0.4, AvgCost: 2000,
		Multiplier: 1, OwnerStrategyID: sc.ID, EntryATR: 50,
	}
	cfg := &Config{Strategies: []StrategyConfig{sc}}
	db := newLimitTestStateDB(t)
	var mu sync.RWMutex
	withStubbedHLLiveExposure(t, HLPosition{Coin: "ETH", Size: 0.4})
	seedLimitExposureRow(t, db, sc.ID, 0.4)

	withStubbedLimitDeps(t,
		func(string, string, []int64, int64) (*HyperliquidLimitStatusResult, string, error) {
			return &HyperliquidLimitStatusResult{Orders: []HyperliquidLimitOrderStatus{
				{OID: 9001, Resting: limitTestBoolPtr(true), FilledSize: 1.0, AvgPx: 2005, Fee: 0.5, Count: 2},
			}}, "", nil
		},
		func(string, string, int64) (*HyperliquidCancelOrderResult, string, error) {
			return &HyperliquidCancelOrderResult{}, "", nil
		},
	)

	reconcilePendingLimitOrders(state, cfg, db, &mu, nil, nil)

	pos := state.Strategies[sc.ID].Positions["ETH"]
	if pos == nil || pos.Quantity != 0.4 {
		t.Fatalf("position = %+v, want the already-backed 0.4 untouched", pos)
	}
	orders, _ := db.LoadPendingLimitOrders()
	if len(orders) != 1 || orders[0].FilledSize != 0.4 {
		t.Fatalf("rows = %+v, want the watermark held at the backed size", orders)
	}
}

func TestReconcilePendingLimitOrdersAdoptsAFillSharedCoinNetBacks(t *testing.T) {
	sc, state := newLimitTestStrategy()
	peer := StrategyConfig{ID: "hl-ema-eth", Platform: "hyperliquid", Type: "perps", Symbol: "ETH",
		Script: "shared_scripts/check_hyperliquid.py", Args: []string{"ema_crossover", "ETH", "1h", "--mode=live"}}
	state.Strategies[peer.ID] = &StrategyState{
		ID: peer.ID, Platform: "hyperliquid", Type: "perps", Cash: 10000,
		Positions: map[string]*Position{"ETH": {Symbol: "ETH", Side: "long", Quantity: 1.0,
			InitialQuantity: 1.0, AvgCost: 1900, Multiplier: 1, OwnerStrategyID: peer.ID}},
	}
	cfg := &Config{Strategies: []StrategyConfig{sc, peer}}
	db := newLimitTestStateDB(t)
	var mu sync.RWMutex
	withStubbedHLLiveExposure(t, HLPosition{Coin: "ETH", Size: 1.5})
	seedLimitExposureRow(t, db, sc.ID, 0)

	withStubbedLimitDeps(t, offBookFullFillStatus(0.5), noCancelExpected(t))

	alerts := reconcilePendingLimitOrders(state, cfg, db, &mu, nil, nil)

	if len(alerts) != 1 {
		t.Fatalf("alerts = %+v, want one — a fill the account net fully backs is adopted as before", alerts)
	}
	pos := state.Strategies[sc.ID].Positions["ETH"]
	if pos == nil || pos.Quantity != 0.5 {
		t.Fatalf("position = %+v, want the 0.5 fill booked", pos)
	}
	if orders, _ := db.LoadPendingLimitOrders(); len(orders) != 0 {
		t.Fatalf("rows = %+v, want the terminal row cleared once the fill is booked", orders)
	}
}

func TestLimitFillExposureAlertThrottleEscalates(t *testing.T) {
	limitFillExposureAlerts.reset()
	t.Cleanup(func() { limitFillExposureAlerts.reset() })
	o := PendingLimitOrder{StrategyID: "hl-manual-eth-live", Symbol: "ETH", OrderOID: 9001}
	k := limitFillExposureKeyFor(o)
	now := time.Now().UTC()

	if !limitFillExposureAlerts.Record(k, limitFillExposureUnreadable, now) {
		t.Fatal("the first alert must reach the owner")
	}
	if limitFillExposureAlerts.Record(k, limitFillExposureUnreadable, now.Add(time.Minute)) {
		t.Error("an unchanged reading must stay throttled inside the window")
	}
	if !limitFillExposureAlerts.Record(k, limitFillExposureUnbacked, now.Add(2*time.Minute)) {
		t.Error("an escalation to a confirmed missing position must reach the owner inside the window")
	}
	if limitFillExposureAlerts.Record(k, limitFillExposureUnreadable, now.Add(3*time.Minute)) {
		t.Error("a de-escalation must stay throttled")
	}
	if !limitFillExposureAlerts.Record(k, limitFillExposureUnreadable, now.Add(effectiveAlertThrottleInterval()+time.Hour)) {
		t.Error("severity must not latch across windows")
	}
}

func TestLimitFillExposureAlertsRetainDropsClearedRows(t *testing.T) {
	limitFillExposureAlerts.reset()
	t.Cleanup(func() { limitFillExposureAlerts.reset() })
	o := PendingLimitOrder{StrategyID: "hl-manual-eth-live", Symbol: "ETH", OrderOID: 9001}
	k := limitFillExposureKeyFor(o)
	now := time.Now().UTC()

	limitFillExposureAlerts.Record(k, limitFillExposureUnbacked, now)
	limitFillExposureAlerts.Retain(nil)
	if !limitFillExposureAlerts.Record(k, limitFillExposureUnbacked, now.Add(time.Minute)) {
		t.Error("a row that left the queue must not keep a throttle slot alive")
	}
}

func TestReconcilePendingLimitOrdersRereadsTheAccountBeforeRefusing(t *testing.T) {
	sc, state := newLimitTestStrategy()
	cfg := &Config{Strategies: []StrategyConfig{sc}}
	db := newLimitTestStateDB(t)
	var mu sync.RWMutex
	seedLimitExposureRow(t, db, sc.ID, 0)

	t.Setenv("HYPERLIQUID_ACCOUNT_ADDRESS", "0xlimittest")
	limitFillExposureAlerts.reset()
	t.Cleanup(func() { limitFillExposureAlerts.reset() })
	origFetch := fetchHyperliquidStateFn
	t.Cleanup(func() { fetchHyperliquidStateFn = origFetch })
	fetches := 0
	fetchHyperliquidStateFn = func(string) (float64, []HLPosition, error) {
		fetches++
		if fetches == 1 {
			return 0, nil, nil
		}
		return 0, []HLPosition{{Coin: "ETH", Size: 0.5}}, nil
	}

	withStubbedLimitDeps(t, offBookFullFillStatus(0.5), noCancelExpected(t))

	notifier, mock := newOrphanLaneNotifier()
	alerts := reconcilePendingLimitOrders(state, cfg, db, &mu, notifier, nil)

	if fetches != 2 {
		t.Fatalf("account fetches = %d, want 2 — a snapshot older than the order status must be re-read before a refusal", fetches)
	}
	if len(alerts) != 1 {
		t.Fatalf("alerts = %+v, want one — the fresh reading backs the fill", alerts)
	}
	pos := state.Strategies[sc.ID].Positions["ETH"]
	if pos == nil || pos.Quantity != 0.5 {
		t.Fatalf("position = %+v, want the fill booked on the fresh reading", pos)
	}
	if len(mock.dms) != 0 {
		t.Fatalf("owner DMs = %+v, want none — a stale snapshot must not raise a false alarm", mock.dms)
	}
}

func TestReconcilePendingLimitOrdersRereadsTheAccountOnlyOncePerPass(t *testing.T) {
	sc, state := newLimitTestStrategy()
	cfg := &Config{Strategies: []StrategyConfig{sc}}
	db := newLimitTestStateDB(t)
	var mu sync.RWMutex
	seedLimitExposureRow(t, db, sc.ID, 0)

	t.Setenv("HYPERLIQUID_ACCOUNT_ADDRESS", "0xlimittest")
	limitFillExposureAlerts.reset()
	t.Cleanup(func() { limitFillExposureAlerts.reset() })
	origFetch := fetchHyperliquidStateFn
	t.Cleanup(func() { fetchHyperliquidStateFn = origFetch })
	fetches := 0
	fetchHyperliquidStateFn = func(string) (float64, []HLPosition, error) {
		fetches++
		return 0, nil, nil
	}

	withStubbedLimitDeps(t, offBookFullFillStatus(0.5), noCancelExpected(t))

	reconcilePendingLimitOrders(state, cfg, db, &mu, nil, nil)

	if fetches != 2 {
		t.Fatalf("account fetches = %d, want 2 — the re-read is bounded to one per reconcile pass", fetches)
	}
	if pos := state.Strategies[sc.ID].Positions["ETH"]; pos != nil {
		t.Fatalf("position = %+v, want none — a confirmed-flat account still refuses the fill", pos)
	}
}

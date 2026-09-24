package main

import (
	"errors"
	"sort"
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

func TestReconcilePendingLimitOrdersRefusesFillWithNoLiveExposure(t *testing.T) {
	sc, state := newLimitTestStrategy()
	cfg := &Config{Strategies: []StrategyConfig{sc}}
	db := newLimitTestStateDB(t)
	var mu sync.RWMutex
	withStubbedHLLiveExposure(t)
	seedLimitExposureRow(t, db, sc.ID, 0)

	withStubbedLimitDeps(t, offBookFullFillStatus(0.5), noCancelExpected(t))

	notifier, mock := newOrphanLaneNotifier()
	alerts := reconcilePendingLimitOrders(state, cfg, openTestStore(t, db), &mu, notifier, nil)

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

func TestReconcilePendingLimitOrdersDefersFillWhenAccountUnreadable(t *testing.T) {
	sc, state := newLimitTestStrategy()
	cfg := &Config{Strategies: []StrategyConfig{sc}}
	db := newLimitTestStateDB(t)
	var mu sync.RWMutex
	withFailingHLLiveExposure(t, errors.New("http 503 from api.hyperliquid.xyz"))
	seedLimitExposureRow(t, db, sc.ID, 0)

	withStubbedLimitDeps(t, offBookFullFillStatus(0.5), noCancelExpected(t))

	notifier, mock := newOrphanLaneNotifier()
	reconcilePendingLimitOrders(state, cfg, openTestStore(t, db), &mu, notifier, nil)

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

	reconcilePendingLimitOrders(state, cfg, openTestStore(t, db), &mu, nil, nil)

	pos := state.Strategies[sc.ID].Positions["ETH"]
	if pos == nil || pos.Quantity != 0.4 {
		t.Fatalf("position = %+v, want the already-backed 0.4 untouched", pos)
	}
	orders, _ := db.LoadPendingLimitOrders()
	if len(orders) != 1 || orders[0].FilledSize != 0.4 {
		t.Fatalf("rows = %+v, want the watermark held at the backed size", orders)
	}
}

func limitExposureCoinStrategy(id, coin string) (StrategyConfig, *StrategyState) {
	sc := StrategyConfig{
		ID:       id,
		Type:     "manual",
		Platform: "hyperliquid",
		Symbol:   coin,
		Script:   "shared_scripts/check_hyperliquid.py",
		Leverage: 10,
		Args:     []string{"hold", coin, "30m", "--mode=live"},
	}
	ss := &StrategyState{
		ID:        id,
		Platform:  "hyperliquid",
		Type:      "manual",
		Positions: map[string]*Position{},
		Cash:      10000,
	}
	return sc, ss
}

func seedLimitExposureCoinRow(t *testing.T, db *StateDB, strategyID, coin string, oid int64, size float64) {
	t.Helper()
	if _, err := db.InsertPendingLimitOrder(PendingLimitOrder{
		StrategyID: strategyID, Symbol: coin, Side: "long", OrderOID: oid,
		LimitPrice: 2000, OrderSize: size, TIF: "Alo", EntryATR: 50,
		CreatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("seed pending limit order for %s: %v", coin, err)
	}
}

type limitExposureTrace struct {
	events []string
}

func (tr *limitExposureTrace) fetches() int {
	n := 0
	for _, e := range tr.events {
		if e == "fetch" {
			n++
		}
	}
	return n
}

func (tr *limitExposureTrace) assertNoFetchBeforeAnyPoll(t *testing.T) {
	t.Helper()
	lastPoll := -1
	for i, e := range tr.events {
		if strings.HasPrefix(e, "poll:") {
			lastPoll = i
		}
	}
	for i, e := range tr.events {
		if e == "fetch" && i < lastPoll {
			t.Fatalf("account read at step %d precedes the status poll at step %d — every booking decision must rest on a reading newer than every row's own status poll; events = %v", i, lastPoll, tr.events)
		}
	}
	if tr.fetches() == 0 {
		t.Fatalf("no account read happened at all; events = %v", tr.events)
	}
}

func countingHLStateStub(t *testing.T, tr *limitExposureTrace, onChain *[]HLPosition) {
	t.Helper()
	t.Setenv("HYPERLIQUID_ACCOUNT_ADDRESS", "0xlimittest")
	limitFillExposureAlerts.reset()
	t.Cleanup(func() { limitFillExposureAlerts.reset() })
	orig := fetchHyperliquidStateFn
	t.Cleanup(func() { fetchHyperliquidStateFn = orig })
	fetchHyperliquidStateFn = func(string) (float64, []HLPosition, error) {
		tr.events = append(tr.events, "fetch")
		return 0, append([]HLPosition(nil), (*onChain)...), nil
	}
}

func twoCoinLimitStatusStub(tr *limitExposureTrace, ethFilled, btcFilled float64, onFill func(coin string)) func(string, string, []int64, int64) (*HyperliquidLimitStatusResult, string, error) {
	return func(_ string, symbol string, _ []int64, _ int64) (*HyperliquidLimitStatusResult, string, error) {
		if tr != nil {
			tr.events = append(tr.events, "poll:"+symbol)
		}
		if onFill != nil {
			onFill(symbol)
		}
		if symbol == "BTC" {
			return &HyperliquidLimitStatusResult{Orders: []HyperliquidLimitOrderStatus{
				{OID: 9002, Resting: limitTestBoolPtr(false), FilledSize: btcFilled, AvgPx: 60000, Fee: 0.4, Count: 1},
			}}, "", nil
		}
		return &HyperliquidLimitStatusResult{Orders: []HyperliquidLimitOrderStatus{
			{OID: 9001, Resting: limitTestBoolPtr(false), FilledSize: ethFilled, AvgPx: 2000, Fee: 0.7, Count: 1},
		}}, "", nil
	}
}

func twoCoinLimitFixture(t *testing.T) (*AppState, *Config, *StateDB, StrategyConfig, StrategyConfig) {
	t.Helper()
	ethSC, ethState := limitExposureCoinStrategy("hl-manual-eth-live", "ETH")
	btcSC, btcState := limitExposureCoinStrategy("hl-manual-btc-live", "BTC")
	state := &AppState{Strategies: map[string]*StrategyState{ethSC.ID: ethState, btcSC.ID: btcState}}
	cfg := &Config{Strategies: []StrategyConfig{ethSC, btcSC}}
	db := newLimitTestStateDB(t)
	seedLimitExposureCoinRow(t, db, ethSC.ID, "ETH", 9001, 0.5)
	seedLimitExposureCoinRow(t, db, btcSC.ID, "BTC", 9002, 0.25)
	return state, cfg, db, ethSC, btcSC
}

func TestReconcilePendingLimitOrdersGivesEachRowASnapshotNewerThanItsOwnStatusPoll(t *testing.T) {
	state, cfg, db, ethSC, btcSC := twoCoinLimitFixture(t)
	var mu sync.RWMutex

	var onChain []HLPosition
	var tr limitExposureTrace
	countingHLStateStub(t, &tr, &onChain)

	withStubbedLimitDeps(t, twoCoinLimitStatusStub(&tr, 0.5, 0.25, func(coin string) {
		if coin == "BTC" {
			onChain = append(onChain, HLPosition{Coin: "BTC", Size: 0.25})
		}
	}), noCancelExpected(t))

	notifier, mock := newOrphanLaneNotifier()
	reconcilePendingLimitOrders(state, cfg, openTestStore(t, db), &mu, notifier, nil)

	if pos := state.Strategies[ethSC.ID].Positions["ETH"]; pos != nil {
		t.Fatalf("ETH position = %+v, want none — that fill really has no live exposure", pos)
	}
	pos := state.Strategies[btcSC.ID].Positions["BTC"]
	if pos == nil || pos.Quantity != 0.25 {
		t.Fatalf("BTC position = %+v, want the 0.25 fill booked — an earlier row's refusal must not spend this row's re-read", pos)
	}
	tr.assertNoFetchBeforeAnyPoll(t)
	if len(mock.dms) != 1 || !strings.Contains(mock.dms[0].content, "ETH") {
		t.Fatalf("owner DMs = %+v, want exactly one, for the genuinely unbacked ETH row", mock.dms)
	}
	if orders, _ := db.LoadPendingLimitOrders(); len(orders) != 1 || orders[0].Symbol != "ETH" {
		t.Fatalf("rows = %+v, want only the refused ETH recovery record kept", orders)
	}
}

func twoRowMidPassFetches(t *testing.T, btcAfterPoll []HLPosition) (*AppState, *StateDB, StrategyConfig, StrategyConfig, *limitExposureTrace, *mockNotifier) {
	t.Helper()
	state, cfg, db, ethSC, btcSC := twoCoinLimitFixture(t)
	var mu sync.RWMutex

	onChain := []HLPosition{{Coin: "ETH", Size: 0.5}, {Coin: "BTC", Size: 0.25}}
	tr := &limitExposureTrace{}
	countingHLStateStub(t, tr, &onChain)

	withStubbedLimitDeps(t, twoCoinLimitStatusStub(tr, 0.5, 0.25, func(coin string) {
		if coin == "BTC" && btcAfterPoll != nil {
			onChain = btcAfterPoll
		}
	}), noCancelExpected(t))

	notifier, mock := newOrphanLaneNotifier()
	reconcilePendingLimitOrders(state, cfg, openTestStore(t, db), &mu, notifier, nil)
	return state, db, ethSC, btcSC, tr, mock
}

func TestReconcilePendingLimitOrdersRefusesALaterRowWhoseCoinClosedMidPass(t *testing.T) {
	state, db, ethSC, btcSC, tr, mock := twoRowMidPassFetches(t, []HLPosition{{Coin: "ETH", Size: 0.5}})

	if pos := state.Strategies[ethSC.ID].Positions["ETH"]; pos == nil || pos.Quantity != 0.5 {
		t.Fatalf("ETH position = %+v, want the backed fill booked", pos)
	}
	if pos := state.Strategies[btcSC.ID].Positions["BTC"]; pos != nil {
		t.Fatalf("BTC position = %+v, want none — its coin was flattened after the first row's snapshot, so the fill must not be booked", pos)
	}
	tr.assertNoFetchBeforeAnyPoll(t)
	orders, _ := db.LoadPendingLimitOrders()
	if len(orders) != 1 || orders[0].Symbol != "BTC" {
		t.Fatalf("rows = %+v, want only the BTC recovery record kept — a refused row is never deleted", orders)
	}
	if len(mock.dms) != 1 || !strings.Contains(mock.dms[0].content, "NO LIVE EXPOSURE") {
		t.Fatalf("owner DMs = %+v, want one refusal alert for BTC", mock.dms)
	}
}

type sharedCoinLeg struct {
	strategyID string
	oid        int64
	fill       float64
}

type sharedCoinOutcome struct {
	booked map[string]float64
	rows   []int64
	dms    int
}

func runSharedCoinPass(t *testing.T, legs []sharedCoinLeg, onChainETH float64) sharedCoinOutcome {
	t.Helper()
	state := &AppState{Strategies: map[string]*StrategyState{}}
	cfg := &Config{}
	for _, leg := range legs {
		sc, ss := limitExposureCoinStrategy(leg.strategyID, "ETH")
		cfg.Strategies = append(cfg.Strategies, sc)
		state.Strategies[leg.strategyID] = ss
	}
	db := newLimitTestStateDB(t)
	var mu sync.RWMutex
	fillByOID := make(map[int64]float64, len(legs))
	for _, leg := range legs {
		seedLimitExposureCoinRow(t, db, leg.strategyID, "ETH", leg.oid, leg.fill)
		fillByOID[leg.oid] = leg.fill
	}

	onChain := []HLPosition{}
	if onChainETH != 0 {
		onChain = append(onChain, HLPosition{Coin: "ETH", Size: onChainETH})
	}
	tr := &limitExposureTrace{}
	countingHLStateStub(t, tr, &onChain)

	withStubbedLimitDeps(t,
		func(_ string, _ string, oids []int64, _ int64) (*HyperliquidLimitStatusResult, string, error) {
			oid := oids[0]
			return &HyperliquidLimitStatusResult{Orders: []HyperliquidLimitOrderStatus{
				{OID: oid, Resting: limitTestBoolPtr(false), FilledSize: fillByOID[oid], AvgPx: 2000, Fee: 0.7, Count: 1},
			}}, "", nil
		},
		noCancelExpected(t),
	)

	notifier, mock := newOrphanLaneNotifier()
	reconcilePendingLimitOrders(state, cfg, openTestStore(t, db), &mu, notifier, nil)

	out := sharedCoinOutcome{booked: map[string]float64{}, dms: len(mock.dms)}
	for _, leg := range legs {
		if pos := state.Strategies[leg.strategyID].Positions["ETH"]; pos != nil {
			out.booked[leg.strategyID] = pos.Quantity
		}
	}
	rows, _ := db.LoadPendingLimitOrders()
	for _, r := range rows {
		out.rows = append(out.rows, r.OrderOID)
	}
	sort.Slice(out.rows, func(i, j int) bool { return out.rows[i] < out.rows[j] })
	return out
}

func TestReconcilePendingLimitOrdersRefusesEverySharedCoinLegTheAccountCannotBack(t *testing.T) {
	real := sharedCoinLeg{strategyID: "hl-manual-eth-a", oid: 9001, fill: 1.0}
	phantom := sharedCoinLeg{strategyID: "hl-manual-eth-b", oid: 9002, fill: 0.5}

	got := runSharedCoinPass(t, []sharedCoinLeg{real, phantom}, 1.0)

	if len(got.booked) != 0 {
		t.Fatalf("booked = %+v, want none — the account's 1.0 cannot back 1.5 of pending fills, and no leg may take the shared budget", got.booked)
	}
	if len(got.rows) != 2 {
		t.Fatalf("rows = %+v, want both recovery records kept", got.rows)
	}
	if got.dms != 2 {
		t.Fatalf("owner DMs = %d, want one per refused leg", got.dms)
	}
}

func TestReconcilePendingLimitOrdersAdoptsAndUnmarksOnceExposureReturns(t *testing.T) {
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
	var onChain []HLPosition
	fetchHyperliquidStateFn = func(string) (float64, []HLPosition, error) {
		return 0, append([]HLPosition(nil), onChain...), nil
	}

	withStubbedLimitDeps(t,
		func(string, string, []int64, int64) (*HyperliquidLimitStatusResult, string, error) {
			return &HyperliquidLimitStatusResult{Orders: []HyperliquidLimitOrderStatus{
				{OID: 9001, Resting: limitTestBoolPtr(true), FilledSize: 0.5, AvgPx: 2000, Fee: 0.7, Count: 1},
			}}, "", nil
		},
		func(string, string, int64) (*HyperliquidCancelOrderResult, string, error) {
			return &HyperliquidCancelOrderResult{}, "", nil
		},
	)

	reconcilePendingLimitOrders(state, cfg, openTestStore(t, db), &mu, nil, nil)
	orders, _ := db.LoadPendingLimitOrders()
	if len(orders) != 1 || orders[0].OperatorRequiredSince.IsZero() {
		t.Fatalf("rows = %+v, want the first pass to mark the row", orders)
	}

	onChain = []HLPosition{{Coin: "ETH", Size: 0.5}}
	reconcilePendingLimitOrders(state, cfg, openTestStore(t, db), &mu, nil, nil)

	pos := state.Strategies[sc.ID].Positions["ETH"]
	if pos == nil || pos.Quantity != 0.5 {
		t.Fatalf("position = %+v — a marked row must still be polled every cycle and adopt once the exposure returns", pos)
	}
	orders, _ = db.LoadPendingLimitOrders()
	if len(orders) != 1 || !orders[0].OperatorRequiredSince.IsZero() {
		t.Fatalf("rows = %+v, want the marker cleared once the fill was adopted", orders)
	}
	if refusal := clearOperatorRequiredLimitRowRefusal(cfg, orders[0], true); !strings.Contains(refusal, "adopts this fill itself") {
		t.Fatalf("an adopted row must go back to refusing a hand clear, got %q", refusal)
	}
}

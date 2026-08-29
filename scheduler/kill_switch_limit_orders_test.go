package main

import (
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

func killSwitchLimitTestStrategy() StrategyConfig {
	return StrategyConfig{
		ID:       "hl-manual-eth-live",
		Type:     "manual",
		Platform: "hyperliquid",
		Symbol:   "ETH",
		Script:   "shared_scripts/check_hyperliquid.py",
		Leverage: 10,
		Args:     []string{"hold", "ETH", "30m", "--mode=live"},
	}
}

func seedKillSwitchLimitRow(t *testing.T, db *StateDB, strategyID string) int64 {
	t.Helper()
	id, err := db.InsertPendingLimitOrder(PendingLimitOrder{
		StrategyID: strategyID, Symbol: "ETH", Side: "long", OrderOID: 9001,
		LimitPrice: 2000, OrderSize: 0.5, TIF: "Alo", EntryATR: 50,
		CreatedAt: time.Now().UTC(),
	})
	if err != nil {
		t.Fatalf("seed pending limit order: %v", err)
	}
	return id
}

type killSwitchLimitStubs struct {
	cancelCalls []int64
	statusCalls []int64
	deleted     []int64
}

func (s *killSwitchLimitStubs) deps(
	cancel func(script, symbol string, oid int64) (*HyperliquidCancelOrderResult, string, error),
	status func(script, symbol string, oids []int64, sinceMs int64) (*HyperliquidLimitStatusResult, string, error),
	del func(id int64) error,
) killSwitchLimitOrderDeps {
	return killSwitchLimitOrderDeps{
		Cancel: func(script, symbol string, oid int64) (*HyperliquidCancelOrderResult, string, error) {
			s.cancelCalls = append(s.cancelCalls, oid)
			return cancel(script, symbol, oid)
		},
		Status: func(script, symbol string, oids []int64, sinceMs int64) (*HyperliquidLimitStatusResult, string, error) {
			if len(oids) > 0 {
				s.statusCalls = append(s.statusCalls, oids[0])
			}
			return status(script, symbol, oids, sinceMs)
		},
		Delete: func(id int64) error {
			s.deleted = append(s.deleted, id)
			return del(id)
		},
	}
}

func okCancel(string, string, int64) (*HyperliquidCancelOrderResult, string, error) {
	return &HyperliquidCancelOrderResult{OID: 9001, Cancelled: true}, "", nil
}

func offBookStatus(filled float64) func(string, string, []int64, int64) (*HyperliquidLimitStatusResult, string, error) {
	return func(string, string, []int64, int64) (*HyperliquidLimitStatusResult, string, error) {
		return &HyperliquidLimitStatusResult{Orders: []HyperliquidLimitOrderStatus{
			{OID: 9001, Resting: limitTestBoolPtr(false), FilledSize: filled, AvgPx: 2000, Count: 1},
		}}, "", nil
	}
}

func killSwitchLimitInputs(db *StateDB, roster []StrategyConfig, deps killSwitchLimitOrderDeps) KillSwitchCloseInputs {
	return KillSwitchCloseInputs{
		PortfolioReason:    "portfolio drawdown 25.0% exceeds limit 20.0%",
		CloseTimeout:       time.Second,
		HLLimitOrderLoader: db.LoadPendingLimitOrders,
		HLLimitOrderRoster: roster,
		HLLimitOrderDeps:   deps,
	}
}

func TestPlanKillSwitchClose_CancelsRestingLimitOrderWithNoPosition(t *testing.T) {
	sc := killSwitchLimitTestStrategy()
	db := newLimitTestStateDB(t)
	id := seedKillSwitchLimitRow(t, db, sc.ID)

	stubs := &killSwitchLimitStubs{}
	deps := stubs.deps(okCancel, offBookStatus(0), db.DeletePendingLimitOrder)

	plan := planKillSwitchClose(killSwitchLimitInputs(db, []StrategyConfig{sc}, deps))

	if !plan.OnChainConfirmedFlat {
		t.Fatalf("expected confirmed flat, got %+v", plan.LimitOrderReport)
	}
	if !plan.CanAutoResetWithoutOwner() {
		t.Error("expected auto-reset to stay available after a confirmed cancellation")
	}
	if len(stubs.cancelCalls) != 1 || stubs.cancelCalls[0] != 9001 {
		t.Errorf("cancel calls = %v, want [9001]", stubs.cancelCalls)
	}
	if len(stubs.statusCalls) != 1 || stubs.statusCalls[0] != 9001 {
		t.Errorf("status calls = %v, want [9001]", stubs.statusCalls)
	}
	if len(stubs.deleted) != 1 || stubs.deleted[0] != id {
		t.Errorf("deleted rows = %v, want [%d]", stubs.deleted, id)
	}
	if orders, _ := db.LoadPendingLimitOrders(); len(orders) != 0 {
		t.Errorf("expected queue row removed, got %d", len(orders))
	}
	if got := plan.LimitOrderReport.Cancelled; len(got) != 1 || got[0] != "hl-manual-eth-live/ETH oid=9001" {
		t.Errorf("Cancelled = %v", got)
	}
	if !strings.Contains(plan.DiscordMessage, "cancelled resting limit orders: hl-manual-eth-live/ETH oid=9001") {
		t.Errorf("message must name the cancelled order, got: %s", plan.DiscordMessage)
	}
}

func TestPlanKillSwitchClose_StillRestingLimitOrderBlocksFlatAndAutoReset(t *testing.T) {
	sc := killSwitchLimitTestStrategy()
	db := newLimitTestStateDB(t)
	seedKillSwitchLimitRow(t, db, sc.ID)

	stubs := &killSwitchLimitStubs{}
	deps := stubs.deps(okCancel,
		func(string, string, []int64, int64) (*HyperliquidLimitStatusResult, string, error) {
			return &HyperliquidLimitStatusResult{Orders: []HyperliquidLimitOrderStatus{
				{OID: 9001, Resting: limitTestBoolPtr(true), Count: 1},
			}}, "", nil
		},
		db.DeletePendingLimitOrder)

	plan := planKillSwitchClose(killSwitchLimitInputs(db, []StrategyConfig{sc}, deps))

	if plan.OnChainConfirmedFlat {
		t.Fatal("a still-resting order must block confirmed-flat")
	}
	if plan.CanAutoResetWithoutOwner() {
		t.Fatal("a still-resting order must suppress no-owner auto-reset")
	}
	if len(stubs.deleted) != 0 {
		t.Errorf("queue row must be kept, deleted = %v", stubs.deleted)
	}
	if orders, _ := db.LoadPendingLimitOrders(); len(orders) != 1 {
		t.Errorf("expected queue row retained, got %d", len(orders))
	}
	if !strings.Contains(plan.DiscordMessage, "LATCHED") {
		t.Errorf("expected latched message, got: %s", plan.DiscordMessage)
	}
	for _, want := range []string{"hl-manual-eth-live/ETH oid=9001", "still resting"} {
		if !strings.Contains(plan.DiscordMessage, want) {
			t.Errorf("message missing %q, got: %s", want, plan.DiscordMessage)
		}
	}
}

func TestPlanKillSwitchClose_LimitCancelErrorBlocksFlat(t *testing.T) {
	sc := killSwitchLimitTestStrategy()
	db := newLimitTestStateDB(t)
	seedKillSwitchLimitRow(t, db, sc.ID)

	stubs := &killSwitchLimitStubs{}
	deps := stubs.deps(
		func(string, string, int64) (*HyperliquidCancelOrderResult, string, error) {
			return nil, "boom", errors.New("subprocess failed")
		},
		offBookStatus(0),
		db.DeletePendingLimitOrder)

	plan := planKillSwitchClose(killSwitchLimitInputs(db, []StrategyConfig{sc}, deps))

	if plan.OnChainConfirmedFlat || plan.CanAutoResetWithoutOwner() {
		t.Fatal("a failed cancel must block confirmed-flat and auto-reset")
	}
	if len(stubs.statusCalls) != 0 {
		t.Errorf("status must not be polled after a failed cancel, got %v", stubs.statusCalls)
	}
	if orders, _ := db.LoadPendingLimitOrders(); len(orders) != 1 {
		t.Errorf("expected queue row retained, got %d", len(orders))
	}
	if !strings.Contains(plan.DiscordMessage, "oid=9001") {
		t.Errorf("message must name the order, got: %s", plan.DiscordMessage)
	}
}

func TestPlanKillSwitchClose_LimitCancelUnverifiedBlocksFlat(t *testing.T) {
	cases := []struct {
		name   string
		status func(string, string, []int64, int64) (*HyperliquidLimitStatusResult, string, error)
		want   string
	}{
		{
			name: "status subprocess failed",
			status: func(string, string, []int64, int64) (*HyperliquidLimitStatusResult, string, error) {
				return nil, "", errors.New("status failed")
			},
			want: "cancel unverified",
		},
		{
			name: "open orders unknown",
			status: func(string, string, []int64, int64) (*HyperliquidLimitStatusResult, string, error) {
				return &HyperliquidLimitStatusResult{OpenOrdersError: "429 rate limited"}, "", nil
			},
			want: "open-orders state unknown",
		},
		{
			name: "order absent from response",
			status: func(string, string, []int64, int64) (*HyperliquidLimitStatusResult, string, error) {
				return &HyperliquidLimitStatusResult{Orders: []HyperliquidLimitOrderStatus{{OID: 42}}}, "", nil
			},
			want: "did not include the order",
		},
		{
			name: "fills unreadable",
			status: func(string, string, []int64, int64) (*HyperliquidLimitStatusResult, string, error) {
				return &HyperliquidLimitStatusResult{Orders: []HyperliquidLimitOrderStatus{
					{OID: 9001, Resting: limitTestBoolPtr(false), FillsError: "fills fetch failed"},
				}}, "", nil
			},
			want: "fills unreadable",
		},
		{
			name: "resting state unknown",
			status: func(string, string, []int64, int64) (*HyperliquidLimitStatusResult, string, error) {
				return &HyperliquidLimitStatusResult{Orders: []HyperliquidLimitOrderStatus{{OID: 9001}}}, "", nil
			},
			want: "still resting",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sc := killSwitchLimitTestStrategy()
			db := newLimitTestStateDB(t)
			seedKillSwitchLimitRow(t, db, sc.ID)
			stubs := &killSwitchLimitStubs{}
			deps := stubs.deps(okCancel, tc.status, db.DeletePendingLimitOrder)

			plan := planKillSwitchClose(killSwitchLimitInputs(db, []StrategyConfig{sc}, deps))

			if plan.OnChainConfirmedFlat || plan.CanAutoResetWithoutOwner() {
				t.Fatal("an unverified cancellation must block confirmed-flat and auto-reset")
			}
			if len(stubs.deleted) != 0 {
				t.Errorf("queue row must be kept, deleted = %v", stubs.deleted)
			}
			if !strings.Contains(plan.DiscordMessage, tc.want) {
				t.Errorf("message missing %q, got: %s", tc.want, plan.DiscordMessage)
			}
		})
	}
}

func TestPlanKillSwitchClose_UnadoptedLimitFillKeepsRowAndBlocksFlat(t *testing.T) {
	sc := killSwitchLimitTestStrategy()
	db := newLimitTestStateDB(t)
	seedKillSwitchLimitRow(t, db, sc.ID)

	stubs := &killSwitchLimitStubs{}
	deps := stubs.deps(okCancel, offBookStatus(0.2), db.DeletePendingLimitOrder)

	plan := planKillSwitchClose(killSwitchLimitInputs(db, []StrategyConfig{sc}, deps))

	if plan.OnChainConfirmedFlat || plan.CanAutoResetWithoutOwner() {
		t.Fatal("an unadopted fill must block confirmed-flat and auto-reset")
	}
	if len(stubs.deleted) != 0 {
		t.Fatalf("queue row must survive so the reconciler can book the fill, deleted = %v", stubs.deleted)
	}
	orders, _ := db.LoadPendingLimitOrders()
	if len(orders) != 1 || orders[0].FilledSize != 0 {
		t.Fatalf("expected the untouched queue row to remain, got %+v", orders)
	}
	if !strings.Contains(plan.DiscordMessage, "unadopted fill") {
		t.Errorf("message must name the unadopted fill, got: %s", plan.DiscordMessage)
	}
}

func TestPlanKillSwitchClose_LimitRowDeleteFailureBlocksFlat(t *testing.T) {
	sc := killSwitchLimitTestStrategy()
	db := newLimitTestStateDB(t)
	seedKillSwitchLimitRow(t, db, sc.ID)

	stubs := &killSwitchLimitStubs{}
	deps := stubs.deps(okCancel, offBookStatus(0),
		func(int64) error { return errors.New("db is locked") })

	plan := planKillSwitchClose(killSwitchLimitInputs(db, []StrategyConfig{sc}, deps))

	if plan.OnChainConfirmedFlat || plan.CanAutoResetWithoutOwner() {
		t.Fatal("a queue row that could not be cleared must block confirmed-flat and auto-reset")
	}
	if !strings.Contains(plan.DiscordMessage, "queue row could not be cleared") {
		t.Errorf("message must explain the failure, got: %s", plan.DiscordMessage)
	}
}

func TestPlanKillSwitchClose_LimitQueueLoadErrorBlocksFlat(t *testing.T) {
	sc := killSwitchLimitTestStrategy()
	in := KillSwitchCloseInputs{
		PortfolioReason: "portfolio drawdown 25.0% exceeds limit 20.0%",
		CloseTimeout:    time.Second,
		HLLimitOrderLoader: func() ([]PendingLimitOrder, error) {
			return nil, errors.New("database is locked")
		},
		HLLimitOrderRoster: []StrategyConfig{sc},
		HLLimitOrderDeps: killSwitchLimitOrderDeps{
			Cancel: func(string, string, int64) (*HyperliquidCancelOrderResult, string, error) {
				t.Error("cancel must not run when the queue could not be read")
				return nil, "", nil
			},
			Status: func(string, string, []int64, int64) (*HyperliquidLimitStatusResult, string, error) {
				return nil, "", nil
			},
			Delete: func(int64) error { return nil },
		},
	}

	plan := planKillSwitchClose(in)

	if plan.OnChainConfirmedFlat || plan.CanAutoResetWithoutOwner() {
		t.Fatal("an unreadable limit-order queue must block confirmed-flat and auto-reset")
	}
	if !strings.Contains(plan.DiscordMessage, "queue unreadable") {
		t.Errorf("message must surface the unreadable queue, got: %s", plan.DiscordMessage)
	}
}

func TestPlanKillSwitchClose_OrphanedLimitRowCancelledViaFallbackScript(t *testing.T) {
	peer := StrategyConfig{ID: "hl-ema-eth", Platform: "hyperliquid", Type: "perps",
		Script: "shared_scripts/check_hyperliquid.py", Args: []string{"ema_crossover", "ETH", "1h", "--mode=live"}}
	db := newLimitTestStateDB(t)
	id := seedKillSwitchLimitRow(t, db, "hl-manual-eth-deleted")

	var usedScript string
	stubs := &killSwitchLimitStubs{}
	deps := stubs.deps(
		func(script, symbol string, oid int64) (*HyperliquidCancelOrderResult, string, error) {
			usedScript = script
			return &HyperliquidCancelOrderResult{OID: oid, Cancelled: false, CancelError: "order already cancelled"}, "", nil
		},
		offBookStatus(0),
		db.DeletePendingLimitOrder)

	plan := planKillSwitchClose(killSwitchLimitInputs(db, []StrategyConfig{peer}, deps))

	if !plan.OnChainConfirmedFlat || !plan.CanAutoResetWithoutOwner() {
		t.Fatalf("an orphaned row confirmed off-book must clear, got %+v", plan.LimitOrderReport)
	}
	if usedScript != peer.Script {
		t.Errorf("orphaned row must fall back to a peer script, got %q", usedScript)
	}
	if len(stubs.deleted) != 1 || stubs.deleted[0] != id {
		t.Errorf("orphaned row must be deleted once off-book, deleted = %v", stubs.deleted)
	}
	if orders, _ := db.LoadPendingLimitOrders(); len(orders) != 0 {
		t.Errorf("expected the stale row removed, got %d", len(orders))
	}
}

func TestPlanKillSwitchClose_OrphanedLimitRowWithFillEscalates(t *testing.T) {
	peer := StrategyConfig{ID: "hl-ema-eth", Platform: "hyperliquid", Type: "perps",
		Script: "shared_scripts/check_hyperliquid.py", Args: []string{"ema_crossover", "ETH", "1h", "--mode=live"}}
	db := newLimitTestStateDB(t)
	seedKillSwitchLimitRow(t, db, "hl-manual-eth-deleted")

	stubs := &killSwitchLimitStubs{}
	deps := stubs.deps(okCancel, offBookStatus(0.2), db.DeletePendingLimitOrder)

	plan := planKillSwitchClose(killSwitchLimitInputs(db, []StrategyConfig{peer}, deps))

	if plan.OnChainConfirmedFlat || plan.CanAutoResetWithoutOwner() {
		t.Fatal("an unbookable fill must block confirmed-flat and auto-reset")
	}
	if len(stubs.deleted) != 0 {
		t.Fatalf("a row carrying an unadopted fill must never be auto-deleted, deleted = %v", stubs.deleted)
	}
	for _, want := range []string{"NO automatic path can book", "absent from this config", "manual intervention required"} {
		if !strings.Contains(plan.DiscordMessage, want) {
			t.Errorf("message missing %q, got: %s", want, plan.DiscordMessage)
		}
	}
	if strings.Contains(plan.DiscordMessage, "queue row kept so the scheduler books the fill") {
		t.Errorf("message must not promise a booking that cannot happen, got: %s", plan.DiscordMessage)
	}
}

func TestPlanKillSwitchClose_AdoptionIneligibleLimitFillNamesTheBlock(t *testing.T) {
	cases := []struct {
		name string
		sc   StrategyConfig
		want string
	}{
		{
			name: "strategy flipped to paper",
			sc: StrategyConfig{ID: "hl-manual-eth-live", Platform: "hyperliquid", Type: "manual",
				Script: "shared_scripts/check_hyperliquid.py", Args: []string{"hold", "ETH", "30m"}},
			want: "not Hyperliquid-live",
		},
		{
			name: "strategy type changed away from manual",
			sc: StrategyConfig{ID: "hl-manual-eth-live", Platform: "hyperliquid", Type: "perps",
				Script: "shared_scripts/check_hyperliquid.py", Args: []string{"ema_crossover", "ETH", "1h", "--mode=live"}},
			want: `type is "perps"`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			db := newLimitTestStateDB(t)
			seedKillSwitchLimitRow(t, db, "hl-manual-eth-live")
			stubs := &killSwitchLimitStubs{}
			deps := stubs.deps(okCancel, offBookStatus(0.2), db.DeletePendingLimitOrder)

			plan := planKillSwitchClose(killSwitchLimitInputs(db, []StrategyConfig{tc.sc}, deps))

			if plan.OnChainConfirmedFlat || plan.CanAutoResetWithoutOwner() {
				t.Fatal("an unbookable fill must block confirmed-flat and auto-reset")
			}
			if len(stubs.deleted) != 0 {
				t.Fatalf("a row carrying an unadopted fill must never be auto-deleted, deleted = %v", stubs.deleted)
			}
			if !strings.Contains(plan.DiscordMessage, tc.want) ||
				!strings.Contains(plan.DiscordMessage, "NO automatic path can book") {
				t.Errorf("message must name why nothing can book the fill, got: %s", plan.DiscordMessage)
			}
		})
	}
}

func TestPlanKillSwitchClose_AdoptionEligibleLimitFillStillDefersToReconciler(t *testing.T) {
	sc := killSwitchLimitTestStrategy()
	db := newLimitTestStateDB(t)
	seedKillSwitchLimitRow(t, db, sc.ID)

	stubs := &killSwitchLimitStubs{}
	deps := stubs.deps(okCancel, offBookStatus(0.2), db.DeletePendingLimitOrder)

	plan := planKillSwitchClose(killSwitchLimitInputs(db, []StrategyConfig{sc}, deps))

	if plan.OnChainConfirmedFlat || plan.CanAutoResetWithoutOwner() {
		t.Fatal("an unadopted fill must block confirmed-flat and auto-reset")
	}
	if !strings.Contains(plan.DiscordMessage, "queue row kept so the scheduler books the fill") {
		t.Errorf("an eligible row must still defer to the reconciler, got: %s", plan.DiscordMessage)
	}
	if strings.Contains(plan.DiscordMessage, "NO automatic path can book") {
		t.Errorf("an eligible row must not escalate, got: %s", plan.DiscordMessage)
	}
}

func TestPlanKillSwitchClose_NoHyperliquidScriptInConfigBlocksFlat(t *testing.T) {
	db := newLimitTestStateDB(t)
	seedKillSwitchLimitRow(t, db, "hl-manual-eth-deleted")

	stubs := &killSwitchLimitStubs{}
	deps := stubs.deps(okCancel, offBookStatus(0), db.DeletePendingLimitOrder)

	plan := planKillSwitchClose(killSwitchLimitInputs(db, nil, deps))

	if plan.OnChainConfirmedFlat || plan.CanAutoResetWithoutOwner() {
		t.Fatal("a resting order we cannot cancel must block confirmed-flat and auto-reset")
	}
	if len(stubs.cancelCalls) != 0 {
		t.Errorf("cancel must not run without a resolved script, got %v", stubs.cancelCalls)
	}
	if !strings.Contains(plan.DiscordMessage, "manual intervention required") ||
		!strings.Contains(plan.DiscordMessage, "oid=9001") {
		t.Errorf("message must name the order and demand intervention, got: %s", plan.DiscordMessage)
	}
}

func TestPlanKillSwitchClose_LimitOrderCancellerUnwiredBlocksFlat(t *testing.T) {
	sc := killSwitchLimitTestStrategy()
	db := newLimitTestStateDB(t)
	seedKillSwitchLimitRow(t, db, sc.ID)

	plan := planKillSwitchClose(killSwitchLimitInputs(db, []StrategyConfig{sc}, killSwitchLimitOrderDeps{}))

	if plan.OnChainConfirmedFlat || plan.CanAutoResetWithoutOwner() {
		t.Fatal("an unwired canceller must block confirmed-flat and auto-reset")
	}
	if !strings.Contains(plan.DiscordMessage, "canceller unwired") {
		t.Errorf("message must surface the wiring gap, got: %s", plan.DiscordMessage)
	}
}

func TestPlanKillSwitchClose_CancelsLimitOrdersBeforeFlatten(t *testing.T) {
	sc := killSwitchLimitTestStrategy()
	perps := StrategyConfig{ID: "hl-ema-eth", Platform: "hyperliquid", Type: "perps",
		Args: []string{"ema_crossover", "ETH", "1h", "--mode=live"}}
	db := newLimitTestStateDB(t)
	seedKillSwitchLimitRow(t, db, sc.ID)

	var seq []string
	closer := func(symbol string, partialSz *float64, cancelStopLossOIDs []int64) (*HyperliquidCloseResult, error) {
		seq = append(seq, "close:"+symbol)
		return &HyperliquidCloseResult{
			Close:    &HyperliquidClose{Symbol: symbol, Fill: &HyperliquidCloseFill{TotalSz: 1.0, AvgPx: 100}},
			Platform: "hyperliquid",
		}, nil
	}
	deps := killSwitchLimitOrderDeps{
		Cancel: func(script, symbol string, oid int64) (*HyperliquidCancelOrderResult, string, error) {
			seq = append(seq, "cancel-limit")
			return okCancel(script, symbol, oid)
		},
		Status: offBookStatus(0),
		Delete: db.DeletePendingLimitOrder,
	}

	in := KillSwitchCloseInputs{
		HLAddr:             "0xaddr",
		HLStateFetched:     true,
		HLPositions:        []HLPosition{{Coin: "ETH", Size: 0.5, EntryPrice: 3000}},
		HLLiveAll:          []StrategyConfig{perps},
		HLCloser:           closer,
		PortfolioReason:    "portfolio drawdown 25.0% exceeds limit 20.0%",
		CloseTimeout:       time.Second,
		HLLimitOrderLoader: db.LoadPendingLimitOrders,
		HLLimitOrderRoster: []StrategyConfig{sc, perps},
		HLLimitOrderDeps:   deps,
	}

	plan := planKillSwitchClose(in)

	if !plan.OnChainConfirmedFlat {
		t.Fatalf("expected confirmed flat, got %+v", plan.LimitOrderReport)
	}
	if len(seq) != 2 || seq[0] != "cancel-limit" || seq[1] != "close:ETH" {
		t.Fatalf("resting orders must be cancelled before the flatten, sequence = %v", seq)
	}
}

func TestPlanKillSwitchClose_NoLimitOrdersLeavesPlanConfirmedFlat(t *testing.T) {
	db := newLimitTestStateDB(t)
	deps := killSwitchLimitOrderDeps{
		Cancel: func(string, string, int64) (*HyperliquidCancelOrderResult, string, error) {
			t.Error("cancel must not run with an empty queue")
			return nil, "", nil
		},
		Status: offBookStatus(0),
		Delete: db.DeletePendingLimitOrder,
	}

	plan := planKillSwitchClose(killSwitchLimitInputs(db, []StrategyConfig{killSwitchLimitTestStrategy()}, deps))

	if !plan.OnChainConfirmedFlat || !plan.CanAutoResetWithoutOwner() {
		t.Fatalf("an empty queue must not affect the plan, got %+v", plan.LimitOrderReport)
	}
	if len(plan.LimitOrderReport.LogLines) != 0 {
		t.Errorf("expected no log lines, got %v", plan.LimitOrderReport.LogLines)
	}
}

func TestPlanKillSwitchClose_NilLimitOrderLoaderIsSafe(t *testing.T) {
	closer, _ := stubHLLiveCloser(nil)
	fetcher, _ := stubHLStateFetcher(nil, nil)
	plan := planKillSwitchClose(defaultHLInputs("0xaddr", true,
		[]HLPosition{{Coin: "ETH", Size: 0.5, EntryPrice: 3000}},
		[]StrategyConfig{{ID: "hl-ema-eth", Platform: "hyperliquid", Type: "perps",
			Args: []string{"ema_crossover", "ETH", "1h", "--mode=live"}}},
		"reason", time.Second, closer, fetcher))

	if !plan.OnChainConfirmedFlat || !plan.CanAutoResetWithoutOwner() {
		t.Fatalf("nil limit-order loader must not latch the plan, got %+v", plan.LimitOrderReport)
	}
}

func TestCollectKillSwitchLimitOrderCandidates(t *testing.T) {
	sc := killSwitchLimitTestStrategy()
	paper := StrategyConfig{ID: "hl-manual-btc-paper", Platform: "hyperliquid", Type: "manual",
		Script: "shared_scripts/check_hyperliquid.py", Args: []string{"hold", "BTC", "30m"}}
	perps := StrategyConfig{ID: "hl-ema-sol", Platform: "hyperliquid", Type: "perps",
		Script: "shared_scripts/check_hyperliquid.py", Args: []string{"ema_crossover", "SOL", "1h", "--mode=live"}}
	orders := []PendingLimitOrder{
		{ID: 1, StrategyID: sc.ID, Symbol: "ETH", OrderOID: 9001},
		{ID: 2, StrategyID: "gone", Symbol: "SOL", OrderOID: 9002},
		{ID: 3, StrategyID: paper.ID, Symbol: "BTC", OrderOID: 9003},
		{ID: 4, StrategyID: perps.ID, Symbol: "SOL", OrderOID: 9004},
	}

	candidates, unscripted := collectKillSwitchLimitOrderCandidates(orders, []StrategyConfig{sc, paper, perps})

	if len(candidates) != 4 || len(unscripted) != 0 {
		t.Fatalf("every row must be cancellable while any Hyperliquid script exists: candidates=%d unscripted=%d", len(candidates), len(unscripted))
	}
	for _, c := range candidates {
		if c.Script != sc.Script {
			t.Errorf("row %d resolved script %q", c.Row.ID, c.Script)
		}
	}
	if !candidates[0].adoptionEligible() {
		t.Errorf("a live manual row must stay adoption-eligible: %+v", candidates[0])
	}
	for _, i := range []int{1, 2, 3} {
		if candidates[i].adoptionEligible() {
			t.Errorf("row %d must be adoption-ineligible: %+v", candidates[i].Row.ID, candidates[i])
		}
	}
	if !strings.Contains(candidates[1].AdoptionBlock, "absent from this config") ||
		!strings.Contains(candidates[2].AdoptionBlock, "not Hyperliquid-live") ||
		!strings.Contains(candidates[3].AdoptionBlock, `type is "perps"`) {
		t.Errorf("adoption blocks must name the reason: %q %q %q",
			candidates[1].AdoptionBlock, candidates[2].AdoptionBlock, candidates[3].AdoptionBlock)
	}
}

func TestCollectKillSwitchLimitOrderCandidatesWithoutAnyScript(t *testing.T) {
	scriptless := StrategyConfig{ID: "hl-no-script", Platform: "hyperliquid", Type: "manual"}
	orders := []PendingLimitOrder{{ID: 1, StrategyID: scriptless.ID, Symbol: "ETH", OrderOID: 9001}}

	candidates, unscripted := collectKillSwitchLimitOrderCandidates(orders, []StrategyConfig{scriptless})

	if len(candidates) != 0 || len(unscripted) != 1 || unscripted[0].ID != 1 {
		t.Fatalf("a row with no usable script anywhere must fail closed: candidates=%+v unscripted=%+v", candidates, unscripted)
	}
}

func TestKillSwitchLimitOrderRoster(t *testing.T) {
	live := killSwitchLimitTestStrategy()
	paper := StrategyConfig{ID: "hl-manual-btc-paper", Platform: "hyperliquid", Type: "manual",
		Script: "shared_scripts/check_hyperliquid.py", Args: []string{"hold", "BTC", "30m"}}
	perps := StrategyConfig{ID: "hl-ema-eth", Platform: "hyperliquid", Type: "perps",
		Script: "shared_scripts/check_hyperliquid.py", Args: []string{"ema_crossover", "ETH", "1h", "--mode=live"}}
	scriptless := StrategyConfig{ID: "hl-no-script", Platform: "hyperliquid", Type: "manual"}
	other := StrategyConfig{ID: "okx-ema", Platform: "okx", Type: "perps", Script: "shared_scripts/check_okx.py"}

	got := killSwitchLimitOrderRoster([]StrategyConfig{live, paper, perps, scriptless, other})

	var ids []string
	for _, sc := range got {
		ids = append(ids, sc.ID)
	}
	want := []string{live.ID, paper.ID, perps.ID, scriptless.ID}
	if strings.Join(ids, ",") != strings.Join(want, ",") {
		t.Fatalf("roster = %v, want %v (every Hyperliquid strategy, so no queued order is left uncancellable)", ids, want)
	}
}

func TestReconcilePendingLimitOrdersAdoptsFillWhileKillSwitchLatched(t *testing.T) {
	sc, state := newLimitTestStrategy()
	state.PortfolioRisk.KillSwitchActive = true
	cfg := &Config{Strategies: []StrategyConfig{sc}}
	db := newLimitTestStateDB(t)
	var mu sync.RWMutex

	db.InsertPendingLimitOrder(PendingLimitOrder{
		StrategyID: sc.ID, Symbol: "ETH", Side: "long", OrderOID: 9001,
		LimitPrice: 2000, OrderSize: 0.5, TIF: "Alo", EntryATR: 50, CreatedAt: time.Now().UTC(),
	})

	withStubbedLimitDeps(t,
		func(string, string, []int64, int64) (*HyperliquidLimitStatusResult, string, error) {
			return &HyperliquidLimitStatusResult{Orders: []HyperliquidLimitOrderStatus{
				{OID: 9001, Resting: limitTestBoolPtr(false), FilledSize: 0.5, AvgPx: 2000, Fee: 0.7, Count: 1},
			}}, "", nil
		},
		func(string, string, int64) (*HyperliquidCancelOrderResult, string, error) {
			return &HyperliquidCancelOrderResult{}, "", nil
		},
	)

	alerts := reconcilePendingLimitOrders(state, cfg, db, &mu, nil, nil)
	if len(alerts) != 1 || alerts[0].trades != 1 {
		t.Fatalf("a latched kill switch must not suppress fill adoption, alerts = %+v", alerts)
	}
	pos := state.Strategies[sc.ID].Positions["ETH"]
	if pos == nil || pos.Quantity != 0.5 || pos.AvgCost != 2000 {
		t.Fatalf("fill must reach the book so the flatten can close it, position = %+v", pos)
	}
}

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
	marked      []string
	flushes     int

	clock     time.Time
	callCost  time.Duration
	flushErr  error
	markErr   error
	clockUsed bool
}

func (s *killSwitchLimitStubs) tick() {
	if s.callCost > 0 {
		s.clock = s.clock.Add(s.callCost)
	}
}

func (s *killSwitchLimitStubs) deps(
	cancel func(script, symbol string, oid int64) (*HyperliquidCancelOrderResult, string, error),
	status func(script, symbol string, oids []int64, sinceMs int64) (*HyperliquidLimitStatusResult, string, error),
	del func(id int64) error,
) killSwitchLimitOrderDeps {
	if s.clock.IsZero() {
		s.clock = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	}
	return killSwitchLimitOrderDeps{
		Cancel: func(script, symbol string, oid int64) (*HyperliquidCancelOrderResult, string, error) {
			s.cancelCalls = append(s.cancelCalls, oid)
			s.tick()
			return cancel(script, symbol, oid)
		},
		Status: func(script, symbol string, oids []int64, sinceMs int64) (*HyperliquidLimitStatusResult, string, error) {
			if len(oids) > 0 {
				s.statusCalls = append(s.statusCalls, oids[0])
			}
			s.tick()
			return status(script, symbol, oids, sinceMs)
		},
		Delete: func(id int64) error {
			s.deleted = append(s.deleted, id)
			return del(id)
		},
		Flush: func() error {
			s.flushes++
			return s.flushErr
		},
		MarkCancelRequested: func(strategyID, symbol string) (int64, error) {
			s.marked = append(s.marked, strategyID+"/"+symbol)
			if s.markErr != nil {
				return 0, s.markErr
			}
			return 1, nil
		},
		Now: func() time.Time {
			s.clockUsed = true
			return s.clock
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
	if !strings.Contains(plan.DiscordMessage, "queue row kept so the scheduler books the fill") {
		t.Errorf("an eligible row must still defer to the reconciler, got: %s", plan.DiscordMessage)
	}
	if strings.Contains(plan.DiscordMessage, "NO automatic path can book") {
		t.Errorf("an eligible row must not escalate, got: %s", plan.DiscordMessage)
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

func TestPlanKillSwitchClose_AdoptionIneligibleLimitFillNamesTheBlock(t *testing.T) {
	cases := []struct {
		name  string
		rowID string
		sc    StrategyConfig
		want  []string
	}{
		{
			name: "strategy flipped to paper", rowID: "hl-manual-eth-live",
			sc: StrategyConfig{ID: "hl-manual-eth-live", Platform: "hyperliquid", Type: "manual",
				Script: "shared_scripts/check_hyperliquid.py", Args: []string{"hold", "ETH", "30m"}},
			want: []string{"not Hyperliquid-live"},
		},
		{
			name: "strategy type changed away from manual", rowID: "hl-manual-eth-live",
			sc: StrategyConfig{ID: "hl-manual-eth-live", Platform: "hyperliquid", Type: "perps",
				Script: "shared_scripts/check_hyperliquid.py", Args: []string{"ema_crossover", "ETH", "1h", "--mode=live"}},
			want: []string{`type is "perps"`},
		},
		{
			name: "orphaned row whose strategy is absent from config", rowID: "hl-manual-eth-deleted",
			sc: StrategyConfig{ID: "hl-ema-eth", Platform: "hyperliquid", Type: "perps",
				Script: "shared_scripts/check_hyperliquid.py", Args: []string{"ema_crossover", "ETH", "1h", "--mode=live"}},
			want: []string{"absent from this config", "manual intervention required"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			db := newLimitTestStateDB(t)
			seedKillSwitchLimitRow(t, db, tc.rowID)
			stubs := &killSwitchLimitStubs{}
			deps := stubs.deps(okCancel, offBookStatus(0.2), db.DeletePendingLimitOrder)

			plan := planKillSwitchClose(killSwitchLimitInputs(db, []StrategyConfig{tc.sc}, deps))

			if plan.OnChainConfirmedFlat || plan.CanAutoResetWithoutOwner() {
				t.Fatal("an unbookable fill must block confirmed-flat and auto-reset")
			}
			if len(stubs.deleted) != 0 {
				t.Fatalf("a row carrying an unadopted fill must never be auto-deleted, deleted = %v", stubs.deleted)
			}
			for _, want := range append(tc.want, "NO automatic path can book") {
				if !strings.Contains(plan.DiscordMessage, want) {
					t.Errorf("message must name why nothing can book the fill, missing %q, got: %s", want, plan.DiscordMessage)
				}
			}
			if strings.Contains(plan.DiscordMessage, "queue row kept so the scheduler books the fill") {
				t.Errorf("message must not promise a booking that cannot happen, got: %s", plan.DiscordMessage)
			}
		})
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
	stubs := &killSwitchLimitStubs{}
	deps := stubs.deps(
		func(script, symbol string, oid int64) (*HyperliquidCancelOrderResult, string, error) {
			seq = append(seq, "cancel-limit")
			return okCancel(script, symbol, oid)
		},
		offBookStatus(0),
		db.DeletePendingLimitOrder)

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
	stubs := &killSwitchLimitStubs{}
	deps := stubs.deps(
		func(string, string, int64) (*HyperliquidCancelOrderResult, string, error) {
			t.Error("cancel must not run with an empty queue")
			return nil, "", nil
		},
		offBookStatus(0),
		db.DeletePendingLimitOrder)

	plan := planKillSwitchClose(killSwitchLimitInputs(db, []StrategyConfig{killSwitchLimitTestStrategy()}, deps))

	if !plan.OnChainConfirmedFlat || !plan.CanAutoResetWithoutOwner() {
		t.Fatalf("an empty queue must not affect the plan, got %+v", plan.LimitOrderReport)
	}
	if len(plan.LimitOrderReport.LogLines) != 0 {
		t.Errorf("expected no log lines, got %v", plan.LimitOrderReport.LogLines)
	}
	if stubs.clockUsed {
		t.Error("an empty queue must not start a budget or add latency")
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

func killSwitchLimitFlattenInputs(db *StateDB, roster []StrategyConfig, deps killSwitchLimitOrderDeps, closer HyperliquidLiveCloser) KillSwitchCloseInputs {
	in := killSwitchLimitInputs(db, roster, deps)
	in.HLAddr = "0xaddr"
	in.HLStateFetched = true
	in.HLPositions = []HLPosition{{Coin: "ETH", Size: 0.5, EntryPrice: 3000}}
	in.HLLiveAll = []StrategyConfig{{ID: "hl-ema-eth", Platform: "hyperliquid", Type: "perps",
		Args: []string{"ema_crossover", "ETH", "1h", "--mode=live"}}}
	in.HLCloser = closer
	return in
}

func TestPlanKillSwitchClose_LimitCancelBudgetStillLetsTheFlattenRun(t *testing.T) {
	sc := killSwitchLimitTestStrategy()
	db := newLimitTestStateDB(t)
	for _, oid := range []int64{9002, 9003, 9004} {
		if _, err := db.InsertPendingLimitOrder(PendingLimitOrder{
			StrategyID: sc.ID, Symbol: "ETH", Side: "long", OrderOID: oid,
			LimitPrice: 2000, OrderSize: 0.5, TIF: "Alo", CreatedAt: time.Now().UTC(),
		}); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	seedKillSwitchLimitRow(t, db, sc.ID)

	stubs := &killSwitchLimitStubs{callCost: scriptTimeout}
	deps := stubs.deps(okCancel,
		func(string, string, []int64, int64) (*HyperliquidLimitStatusResult, string, error) {
			return nil, "", errors.New("script timed out after 30s")
		},
		db.DeletePendingLimitOrder)

	var closed []string
	closer := func(symbol string, partialSz *float64, cancelStopLossOIDs []int64) (*HyperliquidCloseResult, error) {
		closed = append(closed, symbol)
		return &HyperliquidCloseResult{
			Close:    &HyperliquidClose{Symbol: symbol, Fill: &HyperliquidCloseFill{TotalSz: 0.5, AvgPx: 3000}},
			Platform: "hyperliquid",
		}, nil
	}

	plan := planKillSwitchClose(killSwitchLimitFlattenInputs(db, []StrategyConfig{sc}, deps, closer))

	if !stubs.clockUsed {
		t.Fatal("budget must be measured against the injected clock")
	}
	if len(closed) != 1 || closed[0] != "ETH" {
		t.Fatalf("the flatten must still run this cycle, closed = %v", closed)
	}
	if plan.OnChainConfirmedFlat || plan.CanAutoResetWithoutOwner() {
		t.Fatal("unfinished cancellations must block confirmed-flat and auto-reset")
	}
	if len(stubs.cancelCalls) != 1 {
		t.Fatalf("the budget must stop the pass after the first row, cancels = %v", stubs.cancelCalls)
	}
	if got := len(plan.LimitOrderReport.Unresolved); got != 4 {
		t.Fatalf("every row must be reported unresolved, got %d: %v", got, plan.LimitOrderReport.Unresolved)
	}
	if !strings.Contains(plan.DiscordMessage, "exhausted its 1m0s budget before reaching this order") {
		t.Errorf("message must name the budget, got: %s", plan.DiscordMessage)
	}
}

func TestPlanKillSwitchClose_LimitCancelBudgetAllowsOneSlowRow(t *testing.T) {
	sc := killSwitchLimitTestStrategy()
	db := newLimitTestStateDB(t)
	id := seedKillSwitchLimitRow(t, db, sc.ID)

	stubs := &killSwitchLimitStubs{callCost: scriptTimeout - time.Second}
	deps := stubs.deps(okCancel, offBookStatus(0), db.DeletePendingLimitOrder)

	plan := planKillSwitchClose(killSwitchLimitInputs(db, []StrategyConfig{sc}, deps))

	if !plan.OnChainConfirmedFlat {
		t.Fatalf("a slow-but-successful row must not be abandoned by the budget, got %+v", plan.LimitOrderReport)
	}
	if len(stubs.deleted) != 1 || stubs.deleted[0] != id {
		t.Errorf("the row must still be finalized, deleted = %v", stubs.deleted)
	}
}

func TestPlanKillSwitchClose_LimitCancelBudgetExhaustedBeforeVerification(t *testing.T) {
	sc := killSwitchLimitTestStrategy()
	db := newLimitTestStateDB(t)
	seedKillSwitchLimitRow(t, db, sc.ID)

	stubs := &killSwitchLimitStubs{callCost: scriptTimeout + time.Second}
	deps := stubs.deps(okCancel,
		func(string, string, []int64, int64) (*HyperliquidLimitStatusResult, string, error) {
			t.Error("status must not run once the budget cannot cover it")
			return nil, "", nil
		},
		db.DeletePendingLimitOrder)

	plan := planKillSwitchClose(killSwitchLimitInputs(db, []StrategyConfig{sc}, deps))

	if plan.OnChainConfirmedFlat || plan.CanAutoResetWithoutOwner() {
		t.Fatal("an unverified cancellation must block confirmed-flat and auto-reset")
	}
	if len(stubs.deleted) != 0 {
		t.Errorf("an unverified row must be kept, deleted = %v", stubs.deleted)
	}
	if !strings.Contains(plan.DiscordMessage, "cancel issued but the pass exhausted its") {
		t.Errorf("message must explain the truncation, got: %s", plan.DiscordMessage)
	}
}

func TestKillSwitchLimitOrderBudgetFloor(t *testing.T) {
	if got := killSwitchLimitOrderBudget(time.Second); got != 2*scriptTimeout {
		t.Errorf("budget floor = %v, want %v (one row must always fit)", got, 2*scriptTimeout)
	}
	if got := killSwitchLimitOrderBudget(5 * time.Minute); got != 5*time.Minute {
		t.Errorf("an explicit budget above the floor must be honoured, got %v", got)
	}
}

func TestPlanKillSwitchClose_FlushesAdoptedFillBeforeDeletingTheRow(t *testing.T) {
	sc := killSwitchLimitTestStrategy()
	db := newLimitTestStateDB(t)
	id := seedKillSwitchLimitRow(t, db, sc.ID)
	if err := db.UpdatePendingLimitOrderFill(id, 0.2, 2000, 0.3); err != nil {
		t.Fatalf("seed watermark: %v", err)
	}

	stubs := &killSwitchLimitStubs{}
	deps := stubs.deps(okCancel, offBookStatus(0.2), db.DeletePendingLimitOrder)

	plan := planKillSwitchClose(killSwitchLimitInputs(db, []StrategyConfig{sc}, deps))

	if !plan.OnChainConfirmedFlat {
		t.Fatalf("a flushed row must resolve, got %+v", plan.LimitOrderReport)
	}
	if stubs.flushes != 1 {
		t.Fatalf("the adopted fill must be flushed exactly once before delete, flushes = %d", stubs.flushes)
	}
	if len(stubs.deleted) != 1 || stubs.deleted[0] != id {
		t.Errorf("delete must still proceed after a successful flush, deleted = %v", stubs.deleted)
	}
}

func TestPlanKillSwitchClose_FailedFlushKeepsTheRecoveryRow(t *testing.T) {
	sc := killSwitchLimitTestStrategy()
	db := newLimitTestStateDB(t)
	id := seedKillSwitchLimitRow(t, db, sc.ID)
	if err := db.UpdatePendingLimitOrderFill(id, 0.2, 2000, 0.3); err != nil {
		t.Fatalf("seed watermark: %v", err)
	}

	stubs := &killSwitchLimitStubs{flushErr: errors.New("disk full")}
	deps := stubs.deps(okCancel, offBookStatus(0.2), db.DeletePendingLimitOrder)

	plan := planKillSwitchClose(killSwitchLimitInputs(db, []StrategyConfig{sc}, deps))

	if plan.OnChainConfirmedFlat || plan.CanAutoResetWithoutOwner() {
		t.Fatal("a failed flush must block confirmed-flat and auto-reset")
	}
	if len(stubs.deleted) != 0 {
		t.Fatalf("the queue row is the recovery record and must survive a failed flush, deleted = %v", stubs.deleted)
	}
	if orders, _ := db.LoadPendingLimitOrders(); len(orders) != 1 {
		t.Fatalf("expected the row retained, got %d", len(orders))
	}
	if !strings.Contains(plan.DiscordMessage, "could not be flushed to the state DB") {
		t.Errorf("message must name the flush failure, got: %s", plan.DiscordMessage)
	}
}

func TestPlanKillSwitchClose_NoFillNeedsNoFlush(t *testing.T) {
	peer := StrategyConfig{ID: "hl-ema-eth", Platform: "hyperliquid", Type: "perps",
		Script: "shared_scripts/check_hyperliquid.py", Args: []string{"ema_crossover", "ETH", "1h", "--mode=live"}}
	db := newLimitTestStateDB(t)
	id := seedKillSwitchLimitRow(t, db, "hl-manual-eth-deleted")

	stubs := &killSwitchLimitStubs{flushErr: errors.New("would fail if called")}
	deps := stubs.deps(okCancel, offBookStatus(0), db.DeletePendingLimitOrder)

	plan := planKillSwitchClose(killSwitchLimitInputs(db, []StrategyConfig{peer}, deps))

	if !plan.OnChainConfirmedFlat {
		t.Fatalf("a row with nothing to flush must still resolve, got %+v", plan.LimitOrderReport)
	}
	if stubs.flushes != 0 {
		t.Errorf("a zero-fill row must not flush, flushes = %d", stubs.flushes)
	}
	if len(stubs.deleted) != 1 || stubs.deleted[0] != id {
		t.Errorf("an adoption-ineligible zero-fill row must not latch forever, deleted = %v", stubs.deleted)
	}
}

func TestPlanKillSwitchClose_RecordsCancelIntentBeforeCancelling(t *testing.T) {
	sc := killSwitchLimitTestStrategy()
	db := newLimitTestStateDB(t)
	seedKillSwitchLimitRow(t, db, sc.ID)

	var seq []string
	stubs := &killSwitchLimitStubs{}
	deps := stubs.deps(
		func(script, symbol string, oid int64) (*HyperliquidCancelOrderResult, string, error) {
			seq = append(seq, "cancel")
			return okCancel(script, symbol, oid)
		},
		func(string, string, []int64, int64) (*HyperliquidLimitStatusResult, string, error) {
			return nil, "", errors.New("status unreadable")
		},
		db.DeletePendingLimitOrder)
	inner := deps.MarkCancelRequested
	deps.MarkCancelRequested = func(strategyID, symbol string) (int64, error) {
		seq = append(seq, "mark")
		return inner(strategyID, symbol)
	}

	plan := planKillSwitchClose(killSwitchLimitInputs(db, []StrategyConfig{sc}, deps))

	if len(seq) != 2 || seq[0] != "mark" || seq[1] != "cancel" {
		t.Fatalf("cancel intent must be persisted before the cancel is issued, sequence = %v", seq)
	}
	if len(stubs.marked) != 1 || stubs.marked[0] != sc.ID+"/ETH" {
		t.Fatalf("mark = %v", stubs.marked)
	}
	orders, _ := db.LoadPendingLimitOrders()
	if len(orders) != 1 {
		t.Fatalf("expected the unresolved row retained, got %d", len(orders))
	}
	if plan.OnChainConfirmedFlat {
		t.Error("an unverified cancellation must block confirmed-flat")
	}
}

func TestPlanKillSwitchClose_FailedCancelIntentIsSurfaced(t *testing.T) {
	sc := killSwitchLimitTestStrategy()
	db := newLimitTestStateDB(t)
	seedKillSwitchLimitRow(t, db, sc.ID)

	stubs := &killSwitchLimitStubs{markErr: errors.New("db is locked")}
	deps := stubs.deps(okCancel,
		func(string, string, []int64, int64) (*HyperliquidLimitStatusResult, string, error) {
			return nil, "", errors.New("status unreadable")
		},
		db.DeletePendingLimitOrder)

	plan := planKillSwitchClose(killSwitchLimitInputs(db, []StrategyConfig{sc}, deps))

	if len(stubs.cancelCalls) != 1 {
		t.Fatalf("a failed mark must not stop the cancel, cancels = %v", stubs.cancelCalls)
	}
	if plan.OnChainConfirmedFlat {
		t.Error("an unverified cancellation must block confirmed-flat")
	}
	if !strings.Contains(plan.DiscordMessage, "cancel intent could not be persisted") ||
		!strings.Contains(plan.DiscordMessage, "latch reset would abandon") {
		t.Errorf("message must warn that a reset would abandon the cancel, got: %s", plan.DiscordMessage)
	}
}

func TestReconcilePendingLimitOrdersAdoptsFillOnACancelRequestedRow(t *testing.T) {
	sc, state := newLimitTestStrategy()
	cfg := &Config{Strategies: []StrategyConfig{sc}}
	db := newLimitTestStateDB(t)
	var mu sync.RWMutex
	withStubbedHLLiveExposure(t, HLPosition{Coin: "ETH", Size: 0.3})

	db.InsertPendingLimitOrder(PendingLimitOrder{
		StrategyID: sc.ID, Symbol: "ETH", Side: "long", OrderOID: 9001,
		LimitPrice: 2000, OrderSize: 0.5, TIF: "Alo", EntryATR: 50,
		CancelRequested: true, CreatedAt: time.Now().UTC(),
	})

	withStubbedLimitDeps(t,
		func(string, string, []int64, int64) (*HyperliquidLimitStatusResult, string, error) {
			return &HyperliquidLimitStatusResult{Orders: []HyperliquidLimitOrderStatus{
				{OID: 9001, Resting: limitTestBoolPtr(true), FilledSize: 0.3, AvgPx: 2000, Fee: 0.4, Count: 1},
			}}, "", nil
		},
		func(string, string, int64) (*HyperliquidCancelOrderResult, string, error) {
			return &HyperliquidCancelOrderResult{OID: 9001, Cancelled: true}, "", nil
		},
	)

	alerts := reconcilePendingLimitOrders(state, cfg, db, &mu, nil, nil)
	if len(alerts) != 1 {
		t.Fatalf("a cancel-requested row must still adopt a fill that landed, alerts = %+v", alerts)
	}
	pos := state.Strategies[sc.ID].Positions["ETH"]
	if pos == nil || pos.Quantity != 0.3 {
		t.Fatalf("fill must reach the book, position = %+v", pos)
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
	state.scopeRisk(ScopeLive).KillSwitchActive = true
	cfg := &Config{Strategies: []StrategyConfig{sc}}
	db := newLimitTestStateDB(t)
	var mu sync.RWMutex
	withStubbedHLLiveExposure(t, HLPosition{Coin: "ETH", Size: 0.5})

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

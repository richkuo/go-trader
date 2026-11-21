package main

import (
	"context"
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
	del func(o PendingLimitOrder) error,
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
		Delete: func(o PendingLimitOrder) error {
			s.deleted = append(s.deleted, o.ID)
			return del(o)
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
		state := "cancelled"
		if filled >= 0.5 {
			state = "filled"
		}
		return &HyperliquidLimitStatusResult{Orders: []HyperliquidLimitOrderStatus{
			provenLimitStatus(9001, "ETH", state, 0.5, filled, 2000, 0),
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

func TestPlanKillSwitchCloseLimitOrders(t *testing.T) {
	t.Run("CancelsRestingLimitOrderWithNoPosition", func(t *testing.T) {
		sc := killSwitchLimitTestStrategy()
		db := newLimitTestStateDB(t)
		id := seedKillSwitchLimitRow(t, db, sc.ID)

		stubs := &killSwitchLimitStubs{}
		deps := stubs.deps(okCancel, offBookStatus(0), db.DeletePendingLimitOrderIfUnchanged)

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
	})
	t.Run("StillRestingLimitOrderBlocksFlatAndAutoReset", func(t *testing.T) {
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
			db.DeletePendingLimitOrderIfUnchanged)

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
	})
	t.Run("LimitCancelUnverifiedBlocksFlat", func(t *testing.T) {
		cases := []struct {
			name   string
			status func(string, string, []int64, int64) (*HyperliquidLimitStatusResult, string, error)
			booked float64
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
					return &HyperliquidLimitStatusResult{OpenOrdersError: "429 rate limited", Orders: []HyperliquidLimitOrderStatus{
						limitStatusWithUnknownBook(provenLimitStatus(9001, "ETH", "cancelled", 0.5, 0, 0, 0)),
					}}, "", nil
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
				name: "fills unproven",
				status: func(string, string, []int64, int64) (*HyperliquidLimitStatusResult, string, error) {
					return &HyperliquidLimitStatusResult{Orders: []HyperliquidLimitOrderStatus{
						limitStatusWithUnprovenFills(provenLimitStatus(9001, "ETH", "cancelled", 0.5, 0, 0, 0), "fill history unreadable: (429, 'rate limited')", "retryable"),
					}}, "", nil
				},
				want: "fills unproven",
			},
			{
				name: "legacy reply without proof fields",
				status: func(string, string, []int64, int64) (*HyperliquidLimitStatusResult, string, error) {
					return &HyperliquidLimitStatusResult{Orders: []HyperliquidLimitOrderStatus{{OID: 9001, Resting: limitTestBoolPtr(false)}}}, "", nil
				},
				want: "no order-status or fill proof",
			},
			{
				name: "venue executed less than the book holds",
				status: func(string, string, []int64, int64) (*HyperliquidLimitStatusResult, string, error) {
					return &HyperliquidLimitStatusResult{Orders: []HyperliquidLimitOrderStatus{
						provenLimitStatus(9001, "ETH", "cancelled", 0.5, 0, 0, 0),
					}}, "", nil
				},
				booked: 0.2,
				want:   "less than the",
			},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				sc := killSwitchLimitTestStrategy()
				db := newLimitTestStateDB(t)
				id := seedKillSwitchLimitRow(t, db, sc.ID)
				if tc.booked > 0 {
					if err := db.UpdatePendingLimitOrderFill(id, tc.booked, 2000, 0.1); err != nil {
						t.Fatalf("seed watermark: %v", err)
					}
				}
				stubs := &killSwitchLimitStubs{}
				deps := stubs.deps(okCancel, tc.status, db.DeletePendingLimitOrderIfUnchanged)

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
	})
	t.Run("UnadoptedLimitFillKeepsRowAndBlocksFlat", func(t *testing.T) {
		sc := killSwitchLimitTestStrategy()
		db := newLimitTestStateDB(t)
		seedKillSwitchLimitRow(t, db, sc.ID)

		stubs := &killSwitchLimitStubs{}
		deps := stubs.deps(okCancel, offBookStatus(0.2), db.DeletePendingLimitOrderIfUnchanged)

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
	})
	t.Run("LimitQueueLoadErrorBlocksFlat", func(t *testing.T) {
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
				Delete: func(PendingLimitOrder) error { return nil },
			},
		}

		plan := planKillSwitchClose(in)

		if plan.OnChainConfirmedFlat || plan.CanAutoResetWithoutOwner() {
			t.Fatal("an unreadable limit-order queue must block confirmed-flat and auto-reset")
		}
		if !strings.Contains(plan.DiscordMessage, "queue unreadable") {
			t.Errorf("message must surface the unreadable queue, got: %s", plan.DiscordMessage)
		}
	})
	t.Run("OrphanedLimitRowCancelledViaFallbackScript", func(t *testing.T) {
		peer := StrategyConfig{ID: "hl-ema-eth", Platform: "hyperliquid", Type: "perps",
			Script: "shared_scripts/check_hyperliquid.py", Args: []string{"ema_crossover", "ETH", "1h", "--mode=live"}}
		db := newLimitTestStateDB(t)
		id := seedKillSwitchLimitRow(t, db, "hl-manual-eth-deleted")

		var usedScript string
		stubs := &killSwitchLimitStubs{}
		deps := stubs.deps(
			func(script, symbol string, oid int64) (*HyperliquidCancelOrderResult, string, error) {
				return &HyperliquidCancelOrderResult{OID: oid, Cancelled: false, CancelError: "order already cancelled"}, "", nil
			},
			func(script, symbol string, oids []int64, sinceMs int64) (*HyperliquidLimitStatusResult, string, error) {
				usedScript = script
				return offBookStatus(0)(script, symbol, oids, sinceMs)
			},
			db.DeletePendingLimitOrderIfUnchanged)

		plan := planKillSwitchClose(killSwitchLimitInputs(db, []StrategyConfig{peer}, deps))

		if !plan.OnChainConfirmedFlat || !plan.CanAutoResetWithoutOwner() {
			t.Fatalf("an orphaned row confirmed off-book must clear, got %+v", plan.LimitOrderReport)
		}
		if usedScript != peer.Script {
			t.Errorf("orphaned row must fall back to a peer script, got %q", usedScript)
		}
		if len(stubs.cancelCalls) != 0 {
			t.Errorf("an orphaned row the exchange reports off-book must send no cancel, got %v", stubs.cancelCalls)
		}
		if len(stubs.deleted) != 1 || stubs.deleted[0] != id {
			t.Errorf("orphaned row must be deleted once off-book, deleted = %v", stubs.deleted)
		}
		if orders, _ := db.LoadPendingLimitOrders(); len(orders) != 0 {
			t.Errorf("expected the stale row removed, got %d", len(orders))
		}
	})
	t.Run("CancelsLimitOrdersBeforeFlatten", func(t *testing.T) {
		sc := killSwitchLimitTestStrategy()
		perps := StrategyConfig{ID: "hl-ema-eth", Platform: "hyperliquid", Type: "perps",
			Args: []string{"ema_crossover", "ETH", "1h", "--mode=live"}}
		db := newLimitTestStateDB(t)
		seedKillSwitchLimitRow(t, db, sc.ID)

		var seq []string
		closer := func(_ hlWriteAuth, symbol string, partialSz *float64, cancelStopLossOIDs []int64) (*HyperliquidCloseResult, error) {
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
			db.DeletePendingLimitOrderIfUnchanged)

		in := KillSwitchCloseInputs{
			HLAddr:             "0xaddr",
			HLEndpoint:         stubHLEndpoint,
			HLReader:           hlSequenceReader(&seq, []HLPosition{{Coin: "ETH", Size: 0.5, EntryPrice: 3000}}),
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
		want := []string{"cancel-limit", "read", "close:ETH", "read"}
		if strings.Join(seq, ",") != strings.Join(want, ",") {
			t.Fatalf("resting orders must be cancelled before the pre-close read and the flatten, and a fresh read must follow the close; sequence = %v, want %v", seq, want)
		}
	})
	t.Run("LimitCancelBudgetStillLetsTheFlattenRun", func(t *testing.T) {
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
			db.DeletePendingLimitOrderIfUnchanged)

		var closed []string
		closer := func(_ hlWriteAuth, symbol string, partialSz *float64, cancelStopLossOIDs []int64) (*HyperliquidCloseResult, error) {
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
	})
	t.Run("FlushesAdoptedFillBeforeDeletingTheRow", func(t *testing.T) {
		sc := killSwitchLimitTestStrategy()
		db := newLimitTestStateDB(t)
		id := seedKillSwitchLimitRow(t, db, sc.ID)
		if err := db.UpdatePendingLimitOrderFill(id, 0.2, 2000, 0.3); err != nil {
			t.Fatalf("seed watermark: %v", err)
		}

		stubs := &killSwitchLimitStubs{}
		deps := stubs.deps(okCancel, offBookStatus(0.2), db.DeletePendingLimitOrderIfUnchanged)

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
	})
	t.Run("FailedFlushKeepsTheRecoveryRow", func(t *testing.T) {
		sc := killSwitchLimitTestStrategy()
		db := newLimitTestStateDB(t)
		id := seedKillSwitchLimitRow(t, db, sc.ID)
		if err := db.UpdatePendingLimitOrderFill(id, 0.2, 2000, 0.3); err != nil {
			t.Fatalf("seed watermark: %v", err)
		}

		stubs := &killSwitchLimitStubs{flushErr: errors.New("disk full")}
		deps := stubs.deps(okCancel, offBookStatus(0.2), db.DeletePendingLimitOrderIfUnchanged)

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
	})
	t.Run("RecordsCancelIntentBeforeCancelling", func(t *testing.T) {
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
			db.DeletePendingLimitOrderIfUnchanged)
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
	})
}

func killSwitchLimitFlattenInputs(db *StateDB, roster []StrategyConfig, deps killSwitchLimitOrderDeps, closer HyperliquidLiveCloser) KillSwitchCloseInputs {
	in := killSwitchLimitInputs(db, roster, deps)
	in.HLAddr = "0xaddr"
	in.HLEndpoint = stubHLEndpoint
	var reads []string
	in.HLReader = hlSequenceReader(&reads, []HLPosition{{Coin: "ETH", Size: 0.5, EntryPrice: 3000}})
	in.HLLiveAll = []StrategyConfig{{ID: "hl-ema-eth", Platform: "hyperliquid", Type: "perps",
		Args: []string{"ema_crossover", "ETH", "1h", "--mode=live"}}}
	in.HLCloser = closer
	return in
}

func TestReconcilePendingLimitOrdersAdoptsFillOnACancelRequestedRowAndWhileKillSwitchLatched(t *testing.T) {
	cases := []struct {
		name            string
		latched         bool
		cancelRequested bool
		resting         bool
		filled          float64
		fee             float64
		cancel          HyperliquidCancelOrderResult
		why             string
	}{
		{name: "cancel-requested row", cancelRequested: true, resting: true, filled: 0.3, fee: 0.4,
			cancel: HyperliquidCancelOrderResult{OID: 9001, Cancelled: true},
			why:    "a cancel-requested row must still adopt a fill that landed"},
		{name: "kill switch latched", latched: true, filled: 0.5, fee: 0.7,
			why: "a latched kill switch must not suppress fill adoption, so the flatten can close it"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sc, state := newLimitTestStrategy()
			if tc.latched {
				state.partitionRisk(livePartition).KillSwitchActive = true
			}
			cfg := &Config{Strategies: []StrategyConfig{sc}}
			db := newLimitTestStateDB(t)
			var mu sync.RWMutex
			withStubbedHLLiveExposure(t, HLPosition{Coin: "ETH", Size: tc.filled})

			db.InsertPendingLimitOrder(PendingLimitOrder{
				StrategyID: sc.ID, Symbol: "ETH", Side: "long", OrderOID: 9001,
				LimitPrice: 2000, OrderSize: 0.5, TIF: "Alo", EntryATR: 50,
				CancelRequested: tc.cancelRequested, CreatedAt: time.Now().UTC(),
			})

			withStubbedLimitDeps(t,
				func(string, string, []int64, int64) (*HyperliquidLimitStatusResult, string, error) {
					state := "filled"
					if tc.resting {
						state = "live"
					}
					return &HyperliquidLimitStatusResult{Orders: []HyperliquidLimitOrderStatus{
						provenLimitStatus(9001, "ETH", state, 0.5, tc.filled, 2000, tc.fee),
					}}, "", nil
				},
				func(string, string, int64) (*HyperliquidCancelOrderResult, string, error) {
					res := tc.cancel
					return &res, "", nil
				},
			)

			alerts := reconcilePendingLimitOrders(state, cfg, openTestStore(t, db), &mu, nil, nil)
			if len(alerts) != 1 || alerts[0].trades != 1 {
				t.Fatalf("%s, alerts = %+v", tc.why, alerts)
			}
			pos := state.Strategies[sc.ID].Positions["ETH"]
			if pos == nil || pos.Quantity != tc.filled || pos.AvgCost != 2000 {
				t.Fatalf("fill must reach the book (%s), position = %+v", tc.why, pos)
			}
		})
	}
}

func TestKillSwitchLimitOrderRoster(t *testing.T) {
	live := killSwitchLimitTestStrategy()
	paper := StrategyConfig{ID: "hl-manual-btc-paper", Platform: "hyperliquid", Type: "manual",
		Script: "shared_scripts/check_hyperliquid.py", Args: []string{"hold", "BTC", "30m"}}
	perps := StrategyConfig{ID: "hl-ema-eth", Platform: "hyperliquid", Type: "perps",
		Script: "shared_scripts/check_hyperliquid.py", Args: []string{"ema_crossover", "ETH", "1h", "--mode=live"}}
	scriptless := StrategyConfig{ID: "hl-no-script", Platform: "hyperliquid", Type: "manual"}
	other := StrategyConfig{ID: "rh-ema", Platform: "robinhood", Type: "perps", Script: "shared_scripts/check_robinhood.py"}

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

func hlSequenceReader(seq *[]string, baseline []HLPosition) HLStrictPositionReader {
	reads := 0
	return func(ctx context.Context, target hlPositionTarget) (hlStrictPositionSnapshot, error) {
		*seq = append(*seq, "read")
		reads++
		if reads == 1 {
			return hlStrictPositionSnapshot{Target: target, Positions: baseline}, nil
		}
		return hlStrictPositionSnapshot{Target: target}, nil
	}
}

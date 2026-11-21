package main

import (
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

type limitProofHarness struct {
	t      *testing.T
	sc     StrategyConfig
	cfg    *Config
	db     *StateDB
	store  *StateStore
	state  *AppState
	mu     sync.RWMutex
	rowID  int64
	status func() (*HyperliquidLimitStatusResult, error)
	cancel int
	mock   *mockNotifier
	notify *MultiNotifier
}

func newLimitProofHarness(t *testing.T, orderSize float64) *limitProofHarness {
	t.Helper()
	sc, state := newLimitTestStrategy()
	db := newLimitTestStateDB(t)
	if err := db.SaveState(state); err != nil {
		t.Fatalf("seed save: %v", err)
	}
	id, err := db.InsertPendingLimitOrder(PendingLimitOrder{
		StrategyID: sc.ID, Symbol: "ETH", Side: "long", OrderOID: 9001,
		LimitPrice: 2000, OrderSize: orderSize, TIF: "Alo", EntryATR: 50, CreatedAt: time.Now().UTC(),
	})
	if err != nil {
		t.Fatalf("seed row: %v", err)
	}
	h := &limitProofHarness{t: t, sc: sc, cfg: &Config{DBFile: db.path, Strategies: []StrategyConfig{sc}}, db: db, store: openTestStore(t, db), state: state, rowID: id}
	h.notify, h.mock = newOrphanLaneNotifier()
	limitFillExposureAlerts.reset()
	orphanLimitCancelAlerts.reset()
	t.Cleanup(func() {
		limitFillExposureAlerts.reset()
		orphanLimitCancelAlerts.reset()
		publishLimitEntryResyncHolds(nil)
	})
	withStubbedLimitDeps(t,
		func(string, string, []int64, int64) (*HyperliquidLimitStatusResult, string, error) {
			res, err := h.status()
			return res, "", err
		},
		func(string, string, int64) (*HyperliquidCancelOrderResult, string, error) {
			h.cancel++
			return &HyperliquidCancelOrderResult{OID: 9001, Cancelled: true}, "", nil
		},
	)
	return h
}

func (h *limitProofHarness) reply(st HyperliquidLimitOrderStatus) {
	h.status = func() (*HyperliquidLimitStatusResult, error) {
		return &HyperliquidLimitStatusResult{Orders: []HyperliquidLimitOrderStatus{st}}, nil
	}
}

func (h *limitProofHarness) run() []manualAlert {
	return reconcilePendingLimitOrders(h.state, h.cfg, h.store, &h.mu, h.notify, nil)
}

func (h *limitProofHarness) rows() []PendingLimitOrder {
	h.t.Helper()
	rows, err := h.db.LoadPendingLimitOrders()
	if err != nil {
		h.t.Fatalf("load rows: %v", err)
	}
	return rows
}

func (h *limitProofHarness) restart() {
	h.t.Helper()
	path := h.db.path
	fresh, err := OpenStateDB(path)
	if err != nil {
		h.t.Fatalf("reopen state db: %v", err)
	}
	h.t.Cleanup(func() { fresh.Close() })
	h.db = fresh
	h.store = openTestStore(h.t, fresh)
	loaded, err := LoadStateWithDB(h.cfg, fresh)
	if err != nil {
		h.t.Fatalf("reload state: %v", err)
	}
	h.state = loaded
	limitFillExposureAlerts.reset()
	orphanLimitCancelAlerts.reset()
}

func (h *limitProofHarness) position() *Position {
	ss := h.state.Strategies[h.sc.ID]
	if ss == nil {
		return nil
	}
	return ss.Positions["ETH"]
}

func (h *limitProofHarness) durableEntryQty() float64 {
	h.t.Helper()
	qty, _, err := h.db.ManualLimitEntryTradeTotals(h.sc.ID, "9001")
	if err != nil {
		h.t.Fatalf("durable trades: %v", err)
	}
	return qty
}

func (h *limitProofHarness) dmsContaining(sub string) int {
	n := 0
	for _, dm := range h.mock.dms {
		if strings.Contains(dm.content, sub) {
			n++
		}
	}
	return n
}

func TestLimitFillProofLifecycle(t *testing.T) {
	t.Run("PartialFillThenCancelBooksOnceAndFinalizes", func(t *testing.T) {
		h := newLimitProofHarness(t, 1.0)
		withStubbedHLLiveExposure(t, HLPosition{Coin: "ETH", Size: 0.4})
		h.reply(provenLimitStatus(9001, "ETH", "live", 1.0, 0.4, 2000, 0.2))
		h.run()
		if pos := h.position(); pos == nil || pos.Quantity != 0.4 {
			t.Fatalf("partial fill while live: position = %+v", pos)
		}
		rows := h.rows()
		if len(rows) != 1 || rows[0].FilledSize != 0.4 || rows[0].FillProofVersion != limitFillProofVersionAtomic {
			t.Fatalf("a verified partial fill must stay recorded while the remainder is live: %+v", rows)
		}
		if got := h.durableEntryQty(); got != 0.4 {
			t.Fatalf("durable entry trades = %g, want 0.4 committed with the watermark", got)
		}

		h.reply(provenLimitStatus(9001, "ETH", "cancelled", 1.0, 0.4, 2000, 0.2))
		h.run()
		if rows := h.rows(); len(rows) != 0 {
			t.Fatalf("a proven terminal row equal to the book must finalize: %+v", rows)
		}
		if pos := h.position(); pos == nil || pos.Quantity != 0.4 {
			t.Fatalf("finalizing must not change the book: %+v", pos)
		}
		if got := h.durableEntryQty(); got != 0.4 {
			t.Fatalf("durable entry trades = %g, want the fill booked once", got)
		}
		if h.dmsContaining("remainder cancelled on-chain") != 1 {
			t.Fatalf("the remainder notice must follow the proven finalization once, dms = %+v", h.mock.dms)
		}
	})

	t.Run("RemainderFillDuringAFailedReadIsBookedAfterProof", func(t *testing.T) {
		h := newLimitProofHarness(t, 1.0)
		withStubbedHLLiveExposure(t, HLPosition{Coin: "ETH", Size: 0.4})
		h.reply(provenLimitStatus(9001, "ETH", "live", 1.0, 0.4, 2000, 0.2))
		h.run()

		h.status = func() (*HyperliquidLimitStatusResult, error) { return nil, errors.New("script timed out") }
		h.run()
		if rows := h.rows(); len(rows) != 1 || rows[0].FilledSize != 0.4 {
			t.Fatalf("a failed read must change nothing: %+v", rows)
		}

		h.reply(limitStatusWithUnprovenFills(provenLimitStatus(9001, "ETH", "filled", 1.0, 1.0, 0, 0), "fill history unreadable: (429, 'rate limited')", "retryable"))
		h.run()
		if rows := h.rows(); len(rows) != 1 || rows[0].FilledSize != 0.4 {
			t.Fatalf("an HTTP 429 on the fill read must neither book nor delete: %+v", rows)
		}
		if h.dmsContaining("cancelled with no fill") != 0 || h.dmsContaining("remainder cancelled") != 0 {
			t.Fatalf("missing proof must suppress the finalization notices, dms = %+v", h.mock.dms)
		}
		if h.dmsContaining("Limit fill NOT booked") != 1 {
			t.Fatalf("the first proven unbooked execution must alert the owner once, dms = %+v", h.mock.dms)
		}

		withStubbedHLLiveExposure(t, HLPosition{Coin: "ETH", Size: 1.0})
		h.reply(provenLimitStatus(9001, "ETH", "filled", 1.0, 1.0, 2006, 0.5))
		h.run()
		if pos := h.position(); pos == nil || pos.Quantity != 1.0 {
			t.Fatalf("the remainder must be booked once proven: %+v", pos)
		}
		if got := h.durableEntryQty(); got < 0.999999 || got > 1.000001 {
			t.Fatalf("durable entry trades = %g, want the open and the remainder add", got)
		}
		if rows := h.rows(); len(rows) != 0 {
			t.Fatalf("the proven full fill must finalize: %+v", rows)
		}
	})

	t.Run("FullFillWithDelayedHistoryAlertsOnceThenBooks", func(t *testing.T) {
		h := newLimitProofHarness(t, 0.5)
		withStubbedHLLiveExposure(t, HLPosition{Coin: "ETH", Size: 0.5})
		h.reply(limitStatusWithUnprovenFills(provenLimitStatus(9001, "ETH", "filled", 0.5, 0.5, 0, 0), "the fill history shows 0 of the 0.5 the order executed (fill index lag or a missing fill)", "retryable"))
		for i := 0; i < 3; i++ {
			h.run()
		}
		if pos := h.position(); pos != nil {
			t.Fatalf("an empty fill history must not book a filled order: %+v", pos)
		}
		if rows := h.rows(); len(rows) != 1 || rows[0].FilledSize != 0 || !rows[0].OperatorRequiredSince.IsZero() {
			t.Fatalf("a retryable proof failure must keep the row unmarked: %+v", rows)
		}
		if got := h.dmsContaining("Limit fill NOT booked"); got != 1 {
			t.Fatalf("the same condition must alert at most once per throttle interval, got %d dms: %+v", got, h.mock.dms)
		}

		h.reply(provenLimitStatus(9001, "ETH", "filled", 0.5, 0.5, 2000, 0.25))
		alerts := h.run()
		if len(alerts) != 1 || alerts[0].trades != 1 {
			t.Fatalf("the delayed history must book exactly one trade, alerts = %+v", alerts)
		}
		if rows := h.rows(); len(rows) != 0 {
			t.Fatalf("the proven fill must finalize the row: %+v", rows)
		}
		h.run()
		if got := h.durableEntryQty(); got != 0.5 {
			t.Fatalf("durable entry trades = %g, want the fill booked once", got)
		}
	})

	t.Run("CancelledZeroExecutionFinalizesWithItsNotice", func(t *testing.T) {
		h := newLimitProofHarness(t, 0.5)
		h.reply(provenLimitStatus(9001, "ETH", "cancelled", 0.5, 0, 0, 0))
		h.run()
		if rows := h.rows(); len(rows) != 0 {
			t.Fatalf("explicit zero execution with a proven empty history must finalize: %+v", rows)
		}
		if h.dmsContaining("cancelled with no fill") != 1 {
			t.Fatalf("the no-fill notice must follow the finalization, dms = %+v", h.mock.dms)
		}
	})

	t.Run("UnprovenOrContradictoryRepliesHoldTheRow", func(t *testing.T) {
		zero := 0.0
		bad := []struct {
			name string
			st   HyperliquidLimitOrderStatus
		}{
			{"legacy reply without proof fields", HyperliquidLimitOrderStatus{OID: 9001, Resting: limitTestBoolPtr(false)}},
			{"unfamiliar status", func() HyperliquidLimitOrderStatus {
				st := provenLimitStatus(9001, "ETH", "cancelled", 0.5, 0, 0, 0)
				st.OrderState = "mysteryCanceled"
				return st
			}()},
			{"wrong coin", provenLimitStatus(9001, "BTC", "cancelled", 0.5, 0, 0, 0)},
			{"missing executed size", func() HyperliquidLimitOrderStatus {
				st := provenLimitStatus(9001, "ETH", "cancelled", 0.5, 0, 0, 0)
				st.ExecutedSize = nil
				return st
			}()},
			{"original size disagrees with the request", provenLimitStatus(9001, "ETH", "cancelled", 0.7, 0, 0, 0)},
			{"status error from the venue reads", func() HyperliquidLimitOrderStatus {
				st := limitStatusWithUnknownBook(provenLimitStatus(9001, "ETH", "cancelled", 0.5, 0, 0, 0))
				st.StatusError = "the order status reports 'canceled' but the open-order list still carries it"
				return st
			}()},
			{"fill total disagrees with execution", func() HyperliquidLimitOrderStatus {
				st := provenLimitStatus(9001, "ETH", "cancelled", 0.5, 0, 0, 0)
				st.FilledSize = 0.1
				return st
			}()},
			{"terminal but off-book unverified", limitStatusWithUnknownBook(provenLimitStatus(9001, "ETH", "cancelled", 0.5, 0, 0, 0))},
			{"explicit zero still distinguishable from missing", func() HyperliquidLimitOrderStatus {
				st := provenLimitStatus(9001, "ETH", "cancelled", 0.5, 0, 0, 0)
				st.ExecutedSize = &zero
				st.OrigSize = nil
				return st
			}()},
		}
		for _, tc := range bad {
			t.Run(tc.name, func(t *testing.T) {
				h := newLimitProofHarness(t, 0.5)
				h.reply(tc.st)
				h.run()
				if rows := h.rows(); len(rows) != 1 || rows[0].FilledSize != 0 {
					t.Fatalf("an unproven reply must neither book nor delete: %+v", rows)
				}
				if pos := h.position(); pos != nil {
					t.Fatalf("an unproven reply must book nothing: %+v", pos)
				}
				if h.dmsContaining("cancelled with no fill") != 0 {
					t.Fatalf("missing proof must suppress the no-fill notice, dms = %+v", h.mock.dms)
				}
			})
		}
	})

	t.Run("FailedCommitBooksNothingAndRetries", func(t *testing.T) {
		h := newLimitProofHarness(t, 0.5)
		withStubbedHLLiveExposure(t, HLPosition{Coin: "ETH", Size: 0.5})
		h.reply(provenLimitStatus(9001, "ETH", "filled", 0.5, 0.5, 2000, 0.25))
		orig := storeCommitHook
		storeCommitHook = func(storageRole) error { return errors.New("disk I/O error") }
		h.run()
		storeCommitHook = orig
		if pos := h.position(); pos != nil {
			t.Fatalf("a failed commit must leave memory untouched: %+v", pos)
		}
		if rows := h.rows(); len(rows) != 1 || rows[0].FilledSize != 0 {
			t.Fatalf("a failed commit must leave the row retryable: %+v", rows)
		}
		if got := h.durableEntryQty(); got != 0 {
			t.Fatalf("a failed commit must write no trade, got %g", got)
		}

		h.restart()
		withStubbedHLLiveExposure(t, HLPosition{Coin: "ETH", Size: 0.5})
		h.run()
		if pos := h.position(); pos == nil || pos.Quantity != 0.5 {
			t.Fatalf("the retry after restart must book the fill: %+v", pos)
		}
		if got := h.durableEntryQty(); got != 0.5 {
			t.Fatalf("durable entry trades = %g, want one booking", got)
		}
	})

	t.Run("RestartAfterCommitDoesNotRebook", func(t *testing.T) {
		h := newLimitProofHarness(t, 1.0)
		withStubbedHLLiveExposure(t, HLPosition{Coin: "ETH", Size: 0.4})
		h.reply(provenLimitStatus(9001, "ETH", "live", 1.0, 0.4, 2000, 0.2))
		h.run()
		h.restart()
		if pos := h.position(); pos == nil || pos.Quantity != 0.4 {
			t.Fatalf("the committed book must survive a restart: %+v", pos)
		}
		h.run()
		if got := h.durableEntryQty(); got != 0.4 {
			t.Fatalf("a restart must not rebook the same fill, durable = %g", got)
		}
		if rows := h.rows(); len(rows) != 1 || rows[0].FilledSize != 0.4 {
			t.Fatalf("the live row must keep its watermark across the restart: %+v", rows)
		}
	})

	t.Run("ConcurrentManualActionDefersBookingAndFinalization", func(t *testing.T) {
		h := newLimitProofHarness(t, 0.5)
		withStubbedHLLiveExposure(t, HLPosition{Coin: "ETH", Size: 0.5})
		h.reply(provenLimitStatus(9001, "ETH", "filled", 0.5, 0.5, 2000, 0.25))
		unlock, err := acquireManualActionFileLock(h.db.path)
		if err != nil {
			t.Fatalf("take manual-action lock: %v", err)
		}
		h.run()
		unlock()
		if pos := h.position(); pos != nil {
			t.Fatalf("a held manual-action lock must defer booking: %+v", pos)
		}
		if rows := h.rows(); len(rows) != 1 {
			t.Fatalf("a held manual-action lock must defer finalization: %+v", rows)
		}
		h.run()
		if pos := h.position(); pos == nil || pos.Quantity != 0.5 {
			t.Fatalf("the next pass must book once the lock is free: %+v", pos)
		}
		if rows := h.rows(); len(rows) != 0 {
			t.Fatalf("the next pass must finalize once the lock is free: %+v", rows)
		}
	})

	t.Run("PermanentProofFailureMarkerSurvivesAndClears", func(t *testing.T) {
		h := newLimitProofHarness(t, 0.5)
		withStubbedHLLiveExposure(t, HLPosition{Coin: "ETH", Size: 0.5})
		h.reply(limitStatusWithUnprovenFills(provenLimitStatus(9001, "ETH", "filled", 0.5, 0.5, 0, 0), "the venue retains only its most recent 10000 fills", "permanent"))
		h.run()
		rows := h.rows()
		if len(rows) != 1 || rows[0].OperatorRequiredSince.IsZero() {
			t.Fatalf("a terminal, off-book permanent proof failure must record a marker: %+v", rows)
		}
		ev, ok := parseLimitOperatorEvidence(rows[0].OperatorEvidenceJSON)
		if !ok || !ev.OffBook || !ev.Permanent || ev.OrderOID != 9001 {
			t.Fatalf("the marker must carry row-bound terminal, off-book evidence: %+v", ev)
		}
		if h.dmsContaining("marked it as needing an operator") != 1 {
			t.Fatalf("the first durable marker write must notify the owner once, dms = %+v", h.mock.dms)
		}
		since := rows[0].OperatorRequiredSince

		h.status = func() (*HyperliquidLimitStatusResult, error) { return nil, errors.New("timeout") }
		h.run()
		h.restart()
		rows = h.rows()
		if len(rows) != 1 || !rows[0].OperatorRequiredSince.Equal(since) || rows[0].OperatorEvidenceJSON == "" {
			t.Fatalf("the marker must survive a retryable failure and a restart: %+v", rows)
		}
		if got := clearOperatorRequiredLimitRowRefusal(h.cfg, rows[0], false); !strings.Contains(got, "--flattened") {
			t.Fatalf("the clear must require the flatten assertion, got %q", got)
		}
		if got := clearOperatorRequiredLimitRowRefusal(h.cfg, rows[0], true); got != "" {
			t.Fatalf("the clear must accept the marked row with --flattened, got %q", got)
		}
		if err := h.store.DeletePendingLimitOrderIfUnchanged(rows[0]); err != nil {
			t.Fatalf("clear: %v", err)
		}
		if got := h.durableEntryQty(); got != 0 {
			t.Fatalf("the accepted clear must add no trade, got %g", got)
		}
	})

	t.Run("UncertainSharedCoinPeerHoldsAdoption", func(t *testing.T) {
		h := newLimitProofHarness(t, 0.5)
		if _, err := h.db.InsertPendingLimitOrder(PendingLimitOrder{
			StrategyID: "hl-manual-eth-retired", Symbol: "ETH", Side: "long", OrderOID: 9002,
			LimitPrice: 2000, OrderSize: 0.5, TIF: "Alo", CreatedAt: time.Now().UTC(),
		}); err != nil {
			t.Fatalf("seed peer: %v", err)
		}
		withStubbedHLLiveExposure(t, HLPosition{Coin: "ETH", Size: 0.5})
		peerStatus := limitStatusWithUnprovenFills(provenLimitStatus(9002, "ETH", "filled", 0.5, 0.5, 0, 0), "fill index lag", "retryable")
		h.status = func() (*HyperliquidLimitStatusResult, error) {
			return &HyperliquidLimitStatusResult{Orders: []HyperliquidLimitOrderStatus{
				provenLimitStatus(9001, "ETH", "filled", 0.5, 0.5, 2000, 0.25), peerStatus,
			}}, nil
		}

		h.run()
		if pos := h.position(); pos != nil {
			t.Fatalf("a peer row with uncertain execution on the coin must hold adoption: %+v", pos)
		}
		if len(h.rows()) != 2 {
			t.Fatalf("both rows must be kept: %+v", h.rows())
		}
		if h.dmsContaining("uncertain execution") != 1 {
			t.Fatalf("the held adoption must alert the owner with the peer reason, dms = %+v", h.mock.dms)
		}

		peerStatus = provenLimitStatus(9002, "ETH", "cancelled", 0.5, 0, 0, 0)
		h.run()
		if pos := h.position(); pos == nil || pos.Quantity != 0.5 {
			t.Fatalf("adoption must proceed once the peer is proven settled: %+v", pos)
		}
	})
}

func TestSoleOwnerResyncHoldsAPendingLimitEntryIncrease(t *testing.T) {
	sc, state := newLimitTestStrategy()
	ss := state.Strategies[sc.ID]
	ss.Positions["ETH"] = &Position{Symbol: "ETH", Side: "long", Quantity: 0.4, InitialQuantity: 0.4, AvgCost: 2000, Multiplier: 1, OwnerStrategyID: sc.ID}
	logger := newTestLogger(t)
	publishLimitEntryResyncHolds([]PendingLimitOrder{{StrategyID: sc.ID, Symbol: "ETH", OrderOID: 9001}})
	t.Cleanup(func() { publishLimitEntryResyncHolds(nil) })

	reconcileHyperliquidPositionsWithResolver(ss, "ETH", []HLPosition{{Coin: "ETH", Size: 0.7, EntryPrice: 2005}}, noFillFeeResolver, logger, nil, nil, sc)
	if got := ss.Positions["ETH"].Quantity; got != 0.4 {
		t.Fatalf("an unresolved pending limit increment must not be absorbed by resync, qty = %g", got)
	}
	reconcileHyperliquidPositionsWithResolver(ss, "ETH", []HLPosition{{Coin: "ETH", Size: -0.4, EntryPrice: 2005}}, noFillFeeResolver, logger, nil, nil, sc)
	if got := ss.Positions["ETH"].Side; got != "long" {
		t.Fatalf("a pending limit entry must block a side reassignment, side = %q", got)
	}
	reconcileHyperliquidPositionsWithResolver(ss, "ETH", []HLPosition{{Coin: "ETH", Size: 0.3, EntryPrice: 2000}}, noFillFeeResolver, logger, nil, nil, sc)
	if got := ss.Positions["ETH"].Quantity; got != 0.3 {
		t.Fatalf("a reduction must still resync while the limit row is pending, qty = %g", got)
	}

	publishLimitEntryResyncHolds(nil)
	reconcileHyperliquidPositionsWithResolver(ss, "ETH", []HLPosition{{Coin: "ETH", Size: 0.6, EntryPrice: 2000}}, noFillFeeResolver, logger, nil, nil, sc)
	if got := ss.Positions["ETH"].Quantity; got != 0.6 {
		t.Fatalf("with no pending row the ordinary resync must apply, qty = %g", got)
	}
}

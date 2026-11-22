package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func newLimitRecoveryHarness(t *testing.T, orderSize float64, split bool, peers ...StrategyConfig) *limitProofHarness {
	t.Helper()
	if !split {
		h := newLimitProofHarness(t, orderSize)
		for _, peer := range peers {
			h.cfg.Strategies = append(h.cfg.Strategies, peer)
			h.state.Strategies[peer.ID] = &StrategyState{ID: peer.ID, Platform: "hyperliquid", Type: "manual", Positions: map[string]*Position{}, Cash: 10000}
		}
		return h
	}
	sc, state := newLimitTestStrategy()
	dir := t.TempDir()
	paper := StrategyConfig{ID: "hl-paper-eth", Type: "perps", Platform: "hyperliquid", Symbol: "ETH", Args: []string{"--mode=paper"}}
	source := StrategyConfig{ID: "hl-src-eth", Type: "perps", Platform: "hyperliquid", Symbol: "ETH", Args: []string{"--mode=paper"}, PaperSource: "src", StorageStrategyID: "hl-src"}
	cfg := &Config{
		DBFile:       filepath.Join(dir, "live.db"),
		PaperDBFile:  filepath.Join(dir, "paper.db"),
		PaperSources: []PaperSourceConfig{{ID: "src", DBFile: filepath.Join(dir, "src.db")}},
		Strategies:   append([]StrategyConfig{sc, paper, source}, peers...),
	}
	for _, p := range []StrategyConfig{paper, source} {
		state.Strategies[p.ID] = &StrategyState{ID: p.ID, Platform: "hyperliquid", Type: "perps", Positions: map[string]*Position{}, OptionPositions: map[string]*OptionPosition{}, Cash: 1000}
	}
	for _, peer := range peers {
		state.Strategies[peer.ID] = &StrategyState{ID: peer.ID, Platform: "hyperliquid", Type: "manual", Positions: map[string]*Position{}, Cash: 10000}
	}
	store := openSplitStore(t, cfg)
	if err := SaveStateWithStore(state, store); err != nil {
		t.Fatalf("seed split save: %v", err)
	}
	db := store.files[storageRolePrimary]
	id, err := store.InsertPendingLimitOrder(PendingLimitOrder{
		StrategyID: sc.ID, Symbol: "ETH", Side: "long", OrderOID: 9001,
		LimitPrice: 2000, OrderSize: orderSize, TIF: "Alo", EntryATR: 50, CreatedAt: time.Now().UTC(),
	})
	if err != nil {
		t.Fatalf("seed row: %v", err)
	}
	h := &limitProofHarness{t: t, sc: sc, cfg: cfg, db: db, store: store, state: state, rowID: id}
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

func (h *limitProofHarness) reopenedLive() (*AppState, []PendingLimitOrder) {
	h.t.Helper()
	fresh, err := OpenStateDB(h.db.path)
	if err != nil {
		h.t.Fatalf("reopen live file: %v", err)
	}
	defer fresh.Close()
	liveCfg := &Config{DBFile: h.db.path, Strategies: []StrategyConfig{h.sc}}
	state, err := LoadStateWithDB(liveCfg, fresh)
	if err != nil {
		h.t.Fatalf("reload live file: %v", err)
	}
	rows, err := fresh.LoadPendingLimitOrders()
	if err != nil {
		h.t.Fatalf("reload rows: %v", err)
	}
	return state, rows
}

func (h *limitProofHarness) liveHeld() string {
	return h.store.persistenceHoldReason(livePartition)
}

func (h *limitProofHarness) seedUnboundWatermark(filled, avgPx, fee float64) {
	h.t.Helper()
	if err := h.db.UpdatePendingLimitOrderFill(h.rowID, filled, avgPx, fee); err != nil {
		h.t.Fatalf("seed an older writer's watermark: %v", err)
	}
}

func (h *limitProofHarness) setPosition(qty float64, positionID string) {
	h.t.Helper()
	h.mu.Lock()
	h.state.Strategies[h.sc.ID].Positions["ETH"] = &Position{
		Symbol: "ETH", Quantity: qty, InitialQuantity: qty, AvgCost: 2000, Side: "long",
		Multiplier: 1, Leverage: h.sc.Leverage, OwnerStrategyID: h.sc.ID, OpenedAt: time.Now().UTC(),
		TradePositionID: positionID, EntryATR: 50,
	}
	err := SaveStateWithStore(h.state, h.store)
	h.mu.Unlock()
	if err != nil {
		h.t.Fatalf("save the position: %v", err)
	}
}

func (h *limitProofHarness) killSwitchPass() killSwitchLimitOrderReport {
	h.t.Helper()
	deps := killSwitchLimitOrderDeps{
		Cancel: func(script, symbol string, oid int64) (*HyperliquidCancelOrderResult, string, error) {
			return runHyperliquidCancelOrderFn(script, symbol, oid)
		},
		Status: func(script, symbol string, oids []int64, sinceMs int64) (*HyperliquidLimitStatusResult, string, error) {
			return runHyperliquidLimitStatusFn(script, symbol, oids, sinceMs)
		},
		Delete: func(o PendingLimitOrder) error {
			unlock, err := h.store.tryLiveManualActionLock()
			if err != nil {
				return err
			}
			defer unlock()
			if why := recheckLimitRow(h.store, o); why != "" {
				return errors.New(why)
			}
			return h.store.DeletePendingLimitOrderIfUnchanged(o)
		},
		MarkCancelRequested: h.store.MarkPendingLimitOrderCancelRequested,
	}
	return cancelKillSwitchRestingLimitOrders(h.store.LoadPendingLimitOrders, killSwitchLimitOrderRoster(h.cfg.Strategies), deps, time.Minute)
}

func forEachLimitLayout(t *testing.T, name string, body func(t *testing.T, split bool)) {
	for _, layout := range []struct {
		label string
		split bool
	}{{"one-file", false}, {"split-with-paper-source", true}} {
		layout := layout
		t.Run(name+"/"+layout.label, func(t *testing.T) { body(t, layout.split) })
	}
}

func TestLimitFillRecoveryHold(t *testing.T) {
	forEachLimitLayout(t, "FailedCommitHoldsLiveEntriesUntilItsRetryCommits", func(t *testing.T, split bool) {
		h := newLimitRecoveryHarness(t, 0.5, split)
		h.store.beginLimitRecovery()
		if why := h.liveHeld(); !strings.Contains(why, "first pass") {
			t.Fatalf("startup must hold the live partition, got %q", why)
		}
		withStubbedHLLiveExposure(t, HLPosition{Coin: "ETH", Size: 0.5})
		h.reply(provenLimitStatus(9001, "ETH", "filled", 0.5, 0.5, 2000, 0.25))
		cashBefore := h.state.Strategies[h.sc.ID].Cash
		orig := storeCommitHook
		storeCommitHook = func(storageRole) error { return errors.New("disk I/O error") }
		alerts := h.run()
		storeCommitHook = orig

		if len(alerts) != 0 {
			t.Fatalf("a failed commit must send no fill alert, got %+v", alerts)
		}
		why := h.liveHeld()
		if !strings.Contains(why, "could not be committed") || strings.Contains(why, "first pass") || strings.Contains(why, "save failed") {
			t.Fatalf("the failed commit, and only it, must hold the live partition, got %q", why)
		}
		if h.store.saveFailures(livePartition) != 0 {
			t.Fatalf("a failed fill commit must not count as a save failure")
		}
		h.mu.Lock()
		saveErr := SaveStateWithStore(h.state, h.store)
		h.mu.Unlock()
		if saveErr != nil {
			t.Fatalf("an unrelated live save: %v", saveErr)
		}
		if why := h.liveHeld(); !strings.Contains(why, "could not be committed") {
			t.Fatalf("an unrelated successful save must not release the fill-commit hold, got %q", why)
		}
		for _, p := range activePartitions(h.cfg.Strategies) {
			if p != livePartition && h.store.persistenceHoldsPartition(p) {
				t.Fatalf("the live recovery hold leaked into the %s partition", partitionLabel(p))
			}
		}
		if pos := h.position(); pos != nil || h.state.Strategies[h.sc.ID].Cash != cashBefore {
			t.Fatalf("the published book must keep its last committed values: pos %+v cash %v", pos, h.state.Strategies[h.sc.ID].Cash)
		}
		reopened, rows := h.reopenedLive()
		if pos := reopened.Strategies[h.sc.ID].Positions["ETH"]; pos != nil || reopened.Strategies[h.sc.ID].Cash != cashBefore {
			t.Fatalf("the reopened book must keep its last committed values: pos %+v cash %v", pos, reopened.Strategies[h.sc.ID].Cash)
		}
		if len(rows) != 1 || rows[0].FilledSize != 0 || rows[0].FillFee != 0 || rows[0].FillProofBound {
			t.Fatalf("the reopened row must keep its last committed watermark: %+v", rows)
		}
		if got := h.durableEntryQty(); got != 0 {
			t.Fatalf("the failed commit must write no trade, got %g", got)
		}
		if h.dmsContaining("NO LIVE EXPOSURE") != 0 || h.dmsContaining("LIMIT FILL REFUSED") != 0 || h.dmsContaining("Limit fill NOT booked") != 0 {
			t.Fatalf("a failed commit must send no refusal or held-fill alert, dms = %+v", h.mock.dms)
		}

		alerts = h.run()
		if len(alerts) != 1 || alerts[0].trades < 1 {
			t.Fatalf("the idle retry must book the fill once, alerts = %+v", alerts)
		}
		if pos := h.position(); pos == nil || pos.Quantity != 0.5 {
			t.Fatalf("the retry must publish the committed fill: %+v", pos)
		}
		if got := h.state.Strategies[h.sc.ID].Cash; got != cashBefore-0.25 {
			t.Fatalf("the retry must charge the fee once: cash %v, want %v", got, cashBefore-0.25)
		}
		if got := h.durableEntryQty(); got != 0.5 {
			t.Fatalf("durable entry trades = %g, want one booking", got)
		}
		if why := h.liveHeld(); why != "" {
			t.Fatalf("the committed retry must release its recovery hold, got %q", why)
		}
		h.run()
		if got := h.durableEntryQty(); got != 0.5 {
			t.Fatalf("a later pass must not rebook the fill, durable = %g", got)
		}
	})

	t.Run("OnlyItsOwnRowsReleaseAFailedCommitHold", func(t *testing.T) {
		h := newLimitProofHarness(t, 0.5)
		h.store.noteLimitFillCommitFailed(map[int64]int64{h.rowID: 9001, 77: 9077})
		h.reply(provenLimitStatus(9001, "ETH", "live", 0.5, 0, 0, 0))
		h.run()
		if pending := h.store.limitFillCommitPending(); len(pending) != 1 || pending[h.rowID] != 9001 {
			t.Fatalf("a readable queue must drop only the vanished row's obligation, got %v", pending)
		}
		if !h.store.persistenceHoldsPartition(livePartition) {
			t.Fatal("the present row must keep holding the live partition")
		}
		stateSaveOutcomeFault = func(p RiskPartition) error {
			if p == livePartition {
				return errors.New("controlled save failure")
			}
			return nil
		}
		h.mu.Lock()
		_ = SaveStateWithStore(h.state, h.store)
		h.mu.Unlock()
		stateSaveOutcomeFault = func(RiskPartition) error { return nil }
		if why := h.liveHeld(); !strings.Contains(why, "save failed") || !strings.Contains(why, "could not be committed") {
			t.Fatalf("both holds must be reported, got %q", why)
		}
		h.store.clearLimitFillCommitFailed(h.rowID)
		if why := h.liveHeld(); !strings.Contains(why, "save failed") || strings.Contains(why, "could not be committed") {
			t.Fatalf("clearing the commit obligation must leave the independent save hold, got %q", why)
		}
		h.mu.Lock()
		_ = SaveStateWithStore(h.state, h.store)
		h.mu.Unlock()
		if why := h.liveHeld(); why != "" {
			t.Fatalf("a successful save must clear the save hold, got %q", why)
		}
	})

	t.Run("BusyLockDefersTheFirstPassWhileAFillIsReady", func(t *testing.T) {
		h := newLimitProofHarness(t, 0.5)
		h.store.beginLimitRecovery()
		withStubbedHLLiveExposure(t, HLPosition{Coin: "ETH", Size: 0.5})
		h.reply(provenLimitStatus(9001, "ETH", "filled", 0.5, 0.5, 2000, 0.25))
		unlock, err := acquireManualActionFileLock(h.db.path)
		if err != nil {
			t.Fatalf("take manual-action lock: %v", err)
		}
		h.run()
		unlock()
		if why := h.liveHeld(); !strings.Contains(why, "first pass") {
			t.Fatalf("a busy lock with a ready fill must keep the startup hold, got %q", why)
		}
		h.run()
		if pos := h.position(); pos == nil || pos.Quantity != 0.5 {
			t.Fatalf("the next pass must book the fill: %+v", pos)
		}
		if why := h.liveHeld(); why != "" {
			t.Fatalf("a complete first pass must release the startup hold, got %q", why)
		}
	})

	t.Run("UnreadableQueueHoldsEntriesAndResyncGrowth", func(t *testing.T) {
		h := newLimitProofHarness(t, 0.5)
		h.store.beginLimitRecovery()
		if _, err := h.db.db.Exec("ALTER TABLE pending_limit_orders RENAME TO pending_limit_orders_hidden"); err != nil {
			t.Fatalf("hide the queue: %v", err)
		}
		h.run()
		if why := h.liveHeld(); !strings.Contains(why, "could not be read") || !strings.Contains(why, "first pass") {
			t.Fatalf("an unreadable first pass must keep both holds, got %q", why)
		}
		if !limitEntryResyncHeld("hl-ema-btc", "BTC") {
			t.Fatal("an unreadable queue must hold resync growth for every strategy, even with no known row")
		}
		sc := StrategyConfig{ID: "hl-ema-btc"}
		ss := &StrategyState{ID: sc.ID, Positions: map[string]*Position{"BTC": {Symbol: "BTC", Quantity: 0.2, Side: "long", AvgCost: 100, Multiplier: 1}}}
		logger := newTestLogger(t)
		if reconcileHyperliquidPositionsWithResolver(ss, "BTC", []HLPosition{{Coin: "BTC", Size: 0.5, EntryPrice: 101}}, noFillFeeResolver, logger, nil, nil, sc) || ss.Positions["BTC"].Quantity != 0.2 {
			t.Fatalf("an unreadable queue must stop resync growth: %+v", ss.Positions["BTC"])
		}
		if reconcileHyperliquidPositionsWithResolver(ss, "BTC", []HLPosition{{Coin: "BTC", Size: -0.2, EntryPrice: 101}}, noFillFeeResolver, logger, nil, nil, sc) || ss.Positions["BTC"].Side != "long" {
			t.Fatalf("an unreadable queue must stop a resync side change: %+v", ss.Positions["BTC"])
		}
		if !reconcileHyperliquidPositionsWithResolver(ss, "BTC", []HLPosition{{Coin: "BTC", Size: 0.1, EntryPrice: 101}}, noFillFeeResolver, logger, nil, nil, sc) || ss.Positions["BTC"].Quantity != 0.1 {
			t.Fatalf("a reduction must still resync: %+v", ss.Positions["BTC"])
		}

		if _, err := h.db.db.Exec("ALTER TABLE pending_limit_orders_hidden RENAME TO pending_limit_orders"); err != nil {
			t.Fatalf("restore the queue: %v", err)
		}
		h.reply(provenLimitStatus(9001, "ETH", "live", 0.5, 0, 0, 0))
		h.run()
		if why := h.liveHeld(); why != "" {
			t.Fatalf("a readable pass must release both holds, got %q", why)
		}
		if limitEntryResyncHeld("hl-ema-btc", "BTC") || !limitEntryResyncHeld(h.sc.ID, "ETH") {
			t.Fatal("a readable pass must keep only the row-specific resync hold")
		}
	})
}

func TestLimitWatermarkCommitProof(t *testing.T) {
	forEachLimitLayout(t, "PreAtomicWatermarkWithAMatchingEagerTradeStaysUnresolved", func(t *testing.T, split bool) {
		peer, _ := newLimitTestStrategy()
		peer.ID = "hl-manual-eth-peer"
		h := newLimitRecoveryHarness(t, 0.5, split, peer)
		h.seedUnboundWatermark(0.3, 2000, 0.15)
		seedLimitFillLineage(t, h.db, 0, h.sc.ID, "ETH", 9001, 0.2, 2000, "legacy-position")
		seedLimitFillLineage(t, h.db, 0, h.sc.ID, "ETH", 9001, 0.1, 2000, "legacy-position")
		h.setPosition(0.2, "legacy-position")
		if got := h.durableEntryQty(); got < 0.2999999 || got > 0.3000001 {
			t.Fatalf("the seeded trades must match the watermark, got %g", got)
		}
		if _, err := h.store.InsertPendingLimitOrder(PendingLimitOrder{
			StrategyID: peer.ID, Symbol: "ETH", Side: "long", OrderOID: 9002,
			LimitPrice: 2000, OrderSize: 0.5, TIF: "Alo", EntryATR: 50, CreatedAt: time.Now().UTC(),
		}); err != nil {
			t.Fatalf("seed peer row: %v", err)
		}
		withStubbedHLLiveExposure(t, HLPosition{Coin: "ETH", Size: 1.0})
		h.status = func() (*HyperliquidLimitStatusResult, error) {
			return &HyperliquidLimitStatusResult{Orders: []HyperliquidLimitOrderStatus{
				provenLimitStatus(9001, "ETH", "cancelled", 0.5, 0.3, 2000, 0.15),
				provenLimitStatus(9002, "ETH", "filled", 0.5, 0.5, 2000, 0.25),
			}}, nil
		}
		h.run()
		rows := h.rows()
		own, found := findPendingLimitOrderByOID(rows, 9001)
		if len(rows) != 2 || !found || own.FilledSize != 0.3 || own.OperatorRequiredSince.IsZero() || own.FillProofBound {
			t.Fatalf("the unproven row must stay, unbound, and be marked for an operator: %+v", rows)
		}
		if ev, ok := parseLimitOperatorEvidence(own.OperatorEvidenceJSON); !ok || !ev.Unproven || !ev.Unbookable || !ev.OffBook {
			t.Fatalf("the marker must record the unproven watermark with terminal, off-book evidence: %+v", ev)
		}
		if pos := h.state.Strategies[peer.ID].Positions["ETH"]; pos != nil {
			t.Fatalf("an unproven row must not establish peer certainty: %+v", pos)
		}
		if pos := h.position(); pos == nil || pos.Quantity != 0.2 {
			t.Fatalf("the incomplete book must stay as it is: %+v", pos)
		}
		if h.dmsContaining("Limit row NOT finalized") != 1 || h.dmsContaining("cannot prove that the 0.300000 ETH fill this row records reached its book") != 1 {
			t.Fatalf("the owner must be told once that the watermark is unproven, dms = %+v", h.mock.dms)
		}

		report := h.killSwitchPass()
		if report.ConfirmedClear() || !strings.Contains(strings.Join(report.Unresolved, "\n"), "carries no proof that it committed") {
			t.Fatalf("the kill switch must keep the unproven row unresolved: %+v", report)
		}
		if _, found := findPendingLimitOrderByOID(h.rows(), 9001); !found {
			t.Fatal("the kill switch must not delete the unproven row")
		}

		d := newCLIManualCoreDeps(h.cfg, h.store, nil)
		res := &manualCoreResult{}
		if _, err := clearRestingLimitRemainderForPositionAction(d, res, h.sc, "manual-close", h.sc.ID, "ETH", nil); err == nil || !strings.Contains(err.Error(), "carries no proof that it committed") {
			t.Fatalf("the manual remainder path must refuse the unproven row, got %v", err)
		}

		saved := h.cfg.Strategies
		h.cfg.Strategies = []StrategyConfig{orphanLaneRoster(), peer}
		h.run()
		if _, found := findPendingLimitOrderByOID(h.rows(), 9001); !found {
			t.Fatal("the orphan lane must not delete the unproven row")
		}
		h.cfg.Strategies = saved

		marked, _ := findPendingLimitOrderByOID(h.rows(), 9001)
		if note := clearedLimitRowLedgerNote(marked); !strings.Contains(note, "does not assume that the recorded fill of 0.300000 reached the book") {
			t.Fatalf("the clear note must not assume the watermark reached the book, got %q", note)
		}
		if code := h.clearCommand("9001", "--flattened"); code != 0 {
			t.Fatalf("manual-clear-limit-row --flattened must accept the marked row, exit = %d", code)
		}
		if _, found := findPendingLimitOrderByOID(h.rows(), 9001); found {
			t.Fatal("the accepted clear must remove the row")
		}
		if pos := h.position(); pos == nil || pos.Quantity != 0.2 {
			t.Fatalf("the clear must leave the ledger as it is: %+v", pos)
		}
	})

	t.Run("DowngradeAdvancedVersionOneRowStaysUnresolved", func(t *testing.T) {
		h := newLimitProofHarness(t, 1.0)
		withStubbedHLLiveExposure(t, HLPosition{Coin: "ETH", Size: 0.4})
		h.reply(provenLimitStatus(9001, "ETH", "live", 1.0, 0.4, 2000, 0.2))
		h.run()
		if rows := h.rows(); len(rows) != 1 || !rows[0].FillProofBound || rows[0].FillProofVersion != limitFillProofVersionAtomic {
			t.Fatalf("the atomic booking must bind its proof: %+v", rows)
		}
		h.seedUnboundWatermark(0.6, 2000, 0.3)
		h.restart()
		rows := h.rows()
		if len(rows) != 1 || rows[0].FillProofVersion != limitFillProofVersionAtomic || rows[0].FillProofBound {
			t.Fatalf("an older writer's advance must keep the version but lose the proof: %+v", rows)
		}
		withStubbedHLLiveExposure(t, HLPosition{Coin: "ETH", Size: 0.6})
		h.reply(provenLimitStatus(9001, "ETH", "cancelled", 1.0, 0.6, 2000, 0.3))
		h.run()
		rows = h.rows()
		if len(rows) != 1 || rows[0].OperatorRequiredSince.IsZero() {
			t.Fatalf("the downgrade-advanced row must stay and be marked: %+v", rows)
		}
		if pos := h.position(); pos == nil || pos.Quantity != 0.4 {
			t.Fatalf("the book must keep its committed 0.4: %+v", pos)
		}
		if h.dmsContaining("remainder cancelled on-chain") != 0 {
			t.Fatalf("a held row must send no finalization notice, dms = %+v", h.mock.dms)
		}
		if report := h.killSwitchPass(); report.ConfirmedClear() {
			t.Fatalf("the kill switch must keep the row unresolved: %+v", report)
		}
	})

	t.Run("CurrentProofSurvivesRestartAndBooksALaterIncrementOnce", func(t *testing.T) {
		h := newLimitProofHarness(t, 1.0)
		withStubbedHLLiveExposure(t, HLPosition{Coin: "ETH", Size: 0.4})
		h.reply(provenLimitStatus(9001, "ETH", "live", 1.0, 0.4, 2000, 0.2))
		h.run()
		h.restart()
		if rows := h.rows(); len(rows) != 1 || !rows[0].FillProofBound {
			t.Fatalf("the commit proof must survive a restart: %+v", rows)
		}
		withStubbedHLLiveExposure(t, HLPosition{Coin: "ETH", Size: 1.0})
		h.reply(provenLimitStatus(9001, "ETH", "filled", 1.0, 1.0, 2005, 0.5))
		h.run()
		h.run()
		if pos := h.position(); pos == nil || pos.Quantity != 1.0 {
			t.Fatalf("the later increment must book once: %+v", pos)
		}
		if got := h.durableEntryQty(); got < 0.9999999 || got > 1.0000001 {
			t.Fatalf("durable entry trades = %g, want the open and one add", got)
		}
		if rows := h.rows(); len(rows) != 0 {
			t.Fatalf("the proven full fill must finalize: %+v", rows)
		}
	})

	t.Run("PositiveRowOfAnIneligibleOwnerStaysForReview", func(t *testing.T) {
		h := newLimitProofHarness(t, 1.0)
		withStubbedHLLiveExposure(t, HLPosition{Coin: "ETH", Size: 0.4})
		h.reply(provenLimitStatus(9001, "ETH", "live", 1.0, 0.4, 2000, 0.2))
		h.run()
		paperOwner := h.sc
		paperOwner.Args = []string{"hold", "ETH", "30m", "--mode=paper"}
		peer := h.sc
		peer.ID = "hl-manual-eth-peer"
		h.cfg.Strategies = []StrategyConfig{paperOwner, peer}
		h.state.Strategies[peer.ID] = &StrategyState{ID: peer.ID, Platform: "hyperliquid", Type: "manual", Positions: map[string]*Position{}, Cash: 10000}
		if _, err := h.store.InsertPendingLimitOrder(PendingLimitOrder{
			StrategyID: peer.ID, Symbol: "ETH", Side: "long", OrderOID: 9002,
			LimitPrice: 2000, OrderSize: 0.5, TIF: "Alo", EntryATR: 50, CreatedAt: time.Now().UTC(),
		}); err != nil {
			t.Fatalf("seed peer row: %v", err)
		}
		withStubbedHLLiveExposure(t, HLPosition{Coin: "ETH", Size: 0.9})
		h.status = func() (*HyperliquidLimitStatusResult, error) {
			return &HyperliquidLimitStatusResult{Orders: []HyperliquidLimitOrderStatus{
				provenLimitStatus(9001, "ETH", "cancelled", 1.0, 0.4, 2000, 0.2),
				provenLimitStatus(9002, "ETH", "filled", 0.5, 0.5, 2000, 0.25),
			}}, nil
		}
		h.run()
		own, found := findPendingLimitOrderByOID(h.rows(), 9001)
		if !found || own.OperatorRequiredSince.IsZero() {
			t.Fatalf("a positive row whose owner cannot carry it must stay and be marked for review: %+v", h.rows())
		}
		if pos := h.state.Strategies[peer.ID].Positions["ETH"]; pos != nil {
			t.Fatalf("an ineligible positive peer must hold the coin's other fills: %+v", pos)
		}
		report := h.killSwitchPass()
		if report.ConfirmedClear() || !strings.Contains(strings.Join(report.Unresolved, "\n"), "owner cannot carry") {
			t.Fatalf("the kill switch must keep the ineligible positive row: %+v", report)
		}
		if _, found := findPendingLimitOrderByOID(h.rows(), 9001); !found {
			t.Fatal("the kill switch must not delete the ineligible positive row")
		}
	})

	t.Run("ReductionThenTerminalPartialReportsTheRecordedFill", func(t *testing.T) {
		h := newLimitProofHarness(t, 1.0)
		withStubbedHLLiveExposure(t, HLPosition{Coin: "ETH", Size: 0.4})
		h.reply(provenLimitStatus(9001, "ETH", "live", 1.0, 0.4, 2000, 0.2))
		h.run()
		h.mu.Lock()
		h.state.Strategies[h.sc.ID].Positions["ETH"].Quantity = 0.1
		err := SaveStateWithStore(h.state, h.store)
		h.mu.Unlock()
		if err != nil {
			t.Fatalf("save the reduction: %v", err)
		}
		h.reply(provenLimitStatus(9001, "ETH", "cancelled", 1.0, 0.4, 2000, 0.2))
		h.run()
		if rows := h.rows(); len(rows) != 0 {
			t.Fatalf("the proven terminal row must finalize: %+v", rows)
		}
		if h.dmsContaining("recorded fill of 0.400000 of 1.000000") != 1 {
			t.Fatalf("the notice must report the recorded fill, dms = %+v", h.mock.dms)
		}
		if h.dmsContaining("position tracked at filled size") != 0 {
			t.Fatalf("the notice must not claim the book tracks the cumulative fill, dms = %+v", h.mock.dms)
		}
		if pos := h.position(); pos == nil || pos.Quantity != 0.1 {
			t.Fatalf("finalizing must leave the reduced book: %+v", pos)
		}
	})
}

func limitAdmissionConfig(t *testing.T) (*Config, string) {
	t.Helper()
	dir := t.TempDir()
	live := StrategyConfig{ID: "hl-manual-eth-live", Type: "manual", Platform: "hyperliquid", Symbol: "ETH", Script: "shared_scripts/check_hyperliquid.py", Leverage: 10, Args: []string{"hold", "ETH", "30m", "--mode=live"}}
	paper := live
	paper.ID = "hl-manual-eth-paper"
	paper.Args = []string{"hold", "ETH", "30m", "--mode=paper"}
	return &Config{DBFile: filepath.Join(dir, "state.db"), Strategies: []StrategyConfig{live, paper}}, dir
}

func TestLimitRecoveryEntryAdmission(t *testing.T) {
	t.Run("StandaloneLiveEntryReadsTheQueue", func(t *testing.T) {
		cfg, _ := limitAdmissionConfig(t)
		seed, err := openToolStateStore(cfg)
		if err != nil {
			t.Fatal(err)
		}
		seed.Close()

		admission, err := admitManualEntry(cfg, "hl-manual-eth-live")
		if err != nil {
			t.Fatalf("standalone admission: %v", err)
		}
		if err := admission.check("before placement"); err == nil || !strings.Contains(err.Error(), "was not read") {
			t.Fatalf("a standalone live entry must not pass placement before the queue is read, got %v", err)
		}
		store, err := openToolStateStore(cfg)
		if err != nil {
			t.Fatal(err)
		}
		defer store.Close()
		if err := admission.attachStore(store); err != nil {
			t.Fatalf("an empty readable live queue must let the admission continue: %v", err)
		}
		if _, err := store.InsertPendingLimitOrder(PendingLimitOrder{StrategyID: "hl-manual-eth-live", Symbol: "ETH", Side: "long", OrderOID: 4701, LimitPrice: 2000, OrderSize: 0.5, TIF: "Alo", CreatedAt: time.Now().UTC()}); err != nil {
			t.Fatal(err)
		}
		if err := admission.check("before placement"); err == nil || !strings.Contains(err.Error(), "holds 1 row(s)") || !strings.Contains(err.Error(), "zero included") {
			t.Fatalf("a zero-watermark row must refuse a standalone live entry, got %v", err)
		}
		if _, err := store.files[storageRolePrimary].db.Exec("ALTER TABLE pending_limit_orders RENAME TO pending_limit_orders_hidden"); err != nil {
			t.Fatal(err)
		}
		if err := admission.check("before placement"); err == nil || !strings.Contains(err.Error(), "could not be read") {
			t.Fatalf("an unreadable queue must refuse a standalone live entry, got %v", err)
		}
		admission.Release()

		paperAdmission, err := admitManualEntry(cfg, "hl-manual-eth-paper")
		if err != nil {
			t.Fatalf("standalone paper admission: %v", err)
		}
		defer paperAdmission.Release()
		if err := paperAdmission.attachStore(store); err != nil {
			t.Fatalf("a paper entry must not read the live queue: %v", err)
		}
	})

	t.Run("DaemonWitnessNamesTheVerifiedCause", func(t *testing.T) {
		cfg, _ := limitAdmissionConfig(t)
		store, err := openToolStateStore(cfg)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { store.Close() })
		owned, err := acquireStateOwnership(store.layout.Files)
		if err != nil {
			t.Fatal(err)
		}
		store.beginLimitRecovery()
		if err := globalManualRegimeOwner.publishHeld(store.layout, store.persistenceHoldCauses); err != nil {
			t.Fatal(err)
		}
		store.setHoldCauseSignal(globalManualRegimeOwner.signalPersistenceCauses)
		t.Cleanup(func() {
			globalManualRegimeOwner.withdraw()
			owned.Release()
		})
		expect := func(id, want string) {
			t.Helper()
			a, err := admitManualEntry(cfg, id)
			if want == "" {
				if err != nil {
					t.Fatalf("%s must be admitted: %v", id, err)
				}
				a.Release()
				return
			}
			if err == nil {
				a.Release()
				t.Fatalf("%s must refuse with %q", id, want)
			}
			if !strings.Contains(err.Error(), want) {
				t.Fatalf("%s refusal %q lacks %q", id, err, want)
			}
		}
		expect("hl-manual-eth-live", "has not finished its first pass")
		expect("hl-manual-eth-paper", "")
		store.finishLimitRecoveryPass(limitRecoveryPass{read: true})
		expect("hl-manual-eth-live", "")
		store.noteLimitFillCommitFailed(map[int64]int64{5: 4705})
		expect("hl-manual-eth-live", "could not be committed with its book")
		causePath := manualRegimeHoldCausePath(cfg.DBFile, livePartition)
		if err := os.WriteFile(causePath, []byte(`{"version":1,"owner_nonce":"x","partition":"live","causes":["limit_fill_commit"]}`), 0o600); err != nil {
			t.Fatal(err)
		}
		expect("hl-manual-eth-live", "attests no clear persistence guard (a state save failed")
		store.finishLimitRecoveryPass(limitRecoveryPass{read: true})
		expect("hl-manual-eth-live", "")
		if _, err := os.Stat(causePath); err == nil {
			t.Fatal("a clear partition must withdraw its cause file")
		}
	})

	t.Run("InProcessOpenAndAddRecheckUnderTheLock", func(t *testing.T) {
		cfg, _ := limitAdmissionConfig(t)
		store, err := openToolStateStore(cfg)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { store.Close() })
		state := NewAppState()
		for _, sc := range cfg.Strategies {
			state.Strategies[sc.ID] = NewStrategyState(sc)
		}
		if err := SaveStateWithStore(state, store); err != nil {
			t.Fatal(err)
		}
		ss := &StatusServer{state: state, stateDB: store, mu: &sync.RWMutex{}}
		live, _ := scByIDLookup(cfg, "hl-manual-eth-live")
		sent := 0
		deps := func(raise bool) manualCoreDeps {
			d := ss.daemonManualCoreDeps(cfg)
			d.fetchMids = func([]string) (map[string]float64, error) { return map[string]float64{"ETH": 2000}, nil }
			d.execute = func(string, string, string, float64, float64, int64, float64, string, float64, hlCloseMode, hlExecuteSnapshot, ...int64) (*HyperliquidExecuteResult, string, error) {
				sent++
				return nil, "", errors.New("must not send")
			}
			real := d.lockManualActions
			d.lockManualActions = func(id string) (func(), error) {
				unlock, err := real(id)
				if raise && err == nil {
					store.noteLimitFillCommitFailed(map[int64]int64{9: 4709})
				}
				return unlock, err
			}
			return d
		}
		store.beginLimitRecovery()
		if _, err := manualOpenCore(deps(false), live, manualOpenInputs{StrategyID: live.ID, Size: 0.01, Side: "long"}); err == nil || !strings.Contains(err.Error(), "first pass") {
			t.Fatalf("an in-process open must refuse during the startup hold, got %v", err)
		}
		store.finishLimitRecoveryPass(limitRecoveryPass{read: true})
		if _, err := manualOpenCore(deps(true), live, manualOpenInputs{StrategyID: live.ID, Size: 0.01, Side: "long"}); err == nil || !strings.Contains(err.Error(), "before placement") || !strings.Contains(err.Error(), "could not be committed") {
			t.Fatalf("an in-process open must recheck the hold under the lock, got %v", err)
		}
		store.clearLimitFillCommitFailed(9)
		ss.mu.Lock()
		state.Strategies[live.ID].Positions["ETH"] = &Position{Symbol: "ETH", Quantity: 0.01, InitialQuantity: 0.01, AvgCost: 2000, Side: "long", Multiplier: 1, Leverage: 10, OwnerStrategyID: live.ID, OpenedAt: time.Now().UTC(), TradePositionID: "pos-add"}
		ss.mu.Unlock()
		if _, err := manualAddCore(deps(true), live, manualAddInputs{StrategyID: live.ID, Size: 0.01}); err == nil || !strings.Contains(err.Error(), "before placement") || !strings.Contains(err.Error(), "could not be committed") {
			t.Fatalf("an in-process add must recheck the hold under the lock, got %v", err)
		}
		if sent != 0 {
			t.Fatalf("held in-process entries must send nothing, sent = %d", sent)
		}
	})
}

func (h *limitProofHarness) seedUnprovenRowAboveItsBook() {
	h.t.Helper()
	h.seedUnboundWatermark(0.3, 2000, 0.15)
	seedLimitFillLineage(h.t, h.db, 0, h.sc.ID, "ETH", 9001, 0.2, 2000, "legacy-position")
	h.setPosition(0.2, "legacy-position")
}

func (h *limitProofHarness) dmText() string {
	parts := make([]string, 0, len(h.mock.dms))
	for _, dm := range h.mock.dms {
		parts = append(parts, dm.content)
	}
	return strings.Join(parts, "\n")
}

func assertNoUnprovenBookedSizeClaim(t *testing.T, surface, text string) {
	t.Helper()
	for _, banned := range []string{
		"book holds 0.300000", "book holds only 0.300000", "booked 0.300000", "unbooked 0.100000", "ONLY that unbooked",
		"0.100000 was never booked", "tracked 0.300000", "scheduler books the fill", "adopt the fill", "adopts the fill",
	} {
		if strings.Contains(text, banned) {
			t.Fatalf("%s must not state or do arithmetic on an unproven booked size (%q): %s", surface, banned, text)
		}
	}
}

func TestLimitUnprovenWatermarkOperatorText(t *testing.T) {
	t.Run("VenueExecutionAboveAnUnboundWatermarkStatesNoBookedSize", func(t *testing.T) {
		h := newLimitProofHarness(t, 0.5)
		h.seedUnprovenRowAboveItsBook()
		withStubbedHLLiveExposure(t, HLPosition{Coin: "ETH", Size: 0.4})
		h.reply(provenLimitStatus(9001, "ETH", "cancelled", 0.5, 0.4, 2000, 0.2))

		report := h.killSwitchPass()
		unresolved := strings.Join(report.Unresolved, "\n")
		if report.ConfirmedClear() || !strings.Contains(unresolved, "states no booked size") || !strings.Contains(unresolved, "NO automatic path books or clears the row") {
			t.Fatalf("the kill switch must keep the unproven row and state no booked size: %+v", report)
		}
		assertNoUnprovenBookedSizeClaim(t, "the kill-switch reason", unresolved)
		if rows := h.rows(); len(rows) != 1 || !rows[0].OperatorRequiredSince.IsZero() {
			t.Fatalf("the kill-switch pass must run before any marker exists and keep the row: %+v", rows)
		}

		d := newCLIManualCoreDeps(h.cfg, h.store, nil)
		_, err := clearRestingLimitRemainderForPositionAction(d, &manualCoreResult{}, h.sc, "manual-close", h.sc.ID, "ETH", nil)
		if err == nil || !strings.Contains(err.Error(), "states no booked size") {
			t.Fatalf("the manual remainder path must refuse the unproven row without a booked size, got %v", err)
		}
		assertNoUnprovenBookedSizeClaim(t, "the manual remainder refusal", err.Error())

		h.run()
		rows := h.rows()
		if len(rows) != 1 || rows[0].FilledSize != 0.3 || rows[0].FillProofBound || rows[0].OperatorRequiredSince.IsZero() {
			t.Fatalf("the unproven row must stay, unbound, and be marked for an operator: %+v", rows)
		}
		if pos := h.position(); pos == nil || pos.Quantity != 0.2 {
			t.Fatalf("the book must stay at its 0.2: %+v", pos)
		}
		text := h.dmText()
		if h.dmsContaining("Limit row NOT finalized") != 1 || h.dmsContaining("the venue reports an executed size of 0.400000 and the row records a fill of 0.300000 that the scheduler cannot prove its book holds") != 1 {
			t.Fatalf("the held-fill alert must report the venue size and the unproven record, dms = %s", text)
		}
		if h.dmsContaining("flatten ONLY size the book does not hold") != 1 || h.dmsContaining("states no booked size for this order") != 1 {
			t.Fatalf("the held-fill alert must send the operator to compare the venue with the book, dms = %s", text)
		}
		assertNoUnprovenBookedSizeClaim(t, "the held-fill alert", text)
	})

	t.Run("OrphanLaneAlertForAnUnboundWatermarkStatesNoBookedSize", func(t *testing.T) {
		h := newLimitProofHarness(t, 0.5)
		h.seedUnprovenRowAboveItsBook()
		if _, err := h.store.MarkPendingLimitOrderCancelRequested(h.sc.ID, "ETH"); err != nil {
			t.Fatalf("queue the cancellation: %v", err)
		}
		withStubbedHLLiveExposure(t, HLPosition{Coin: "ETH", Size: 0.4})
		h.reply(provenLimitStatus(9001, "ETH", "cancelled", 0.5, 0.4, 2000, 0.2))
		h.cfg.Strategies = []StrategyConfig{orphanLaneRoster()}
		h.run()
		rows := h.rows()
		if len(rows) != 1 || rows[0].FilledSize != 0.3 || rows[0].OperatorRequiredSince.IsZero() {
			t.Fatalf("the orphan lane must keep the unproven row and mark it: %+v", rows)
		}
		if ev, ok := parseLimitOperatorEvidence(rows[0].OperatorEvidenceJSON); !ok || !ev.Unproven || !ev.Unbookable || !ev.OffBook || ev.ExecutedSize != 0.4 {
			t.Fatalf("the orphan marker must record the unproven watermark with the venue size: %+v", ev)
		}
		text := h.dmText()
		if h.dmsContaining("Limit order queue row not cleared") != 1 || h.dmsContaining("nothing left to cancel") != 1 || h.dmsContaining("flatten ONLY size the book does not hold") != 1 {
			t.Fatalf("the orphan alert must hold the row for an operator and send them to compare the venue with the book, dms = %s", text)
		}
		if h.dmsContaining("UNTRACKED HYPERLIQUID POSITION") != 0 || h.dmsContaining("no exchange action is needed") != 0 {
			t.Fatalf("the orphan alert must not size an untracked position from the unproven record, dms = %s", text)
		}
		assertNoUnprovenBookedSizeClaim(t, "the orphan alert", text)
	})

	t.Run("BoundWatermarkBelowTheVenueKeepsTheExactFlattenSize", func(t *testing.T) {
		h := newLimitProofHarness(t, 0.5)
		withStubbedHLLiveExposure(t, HLPosition{Coin: "ETH", Size: 0.3})
		h.reply(provenLimitStatus(9001, "ETH", "live", 0.5, 0.3, 2000, 0.15))
		h.run()
		if rows := h.rows(); len(rows) != 1 || rows[0].FilledSize != 0.3 || !rows[0].FillProofBound {
			t.Fatalf("the 0.3 fill must be booked with a bound proof first: %+v", rows)
		}
		h.closeBookedPosition()
		withStubbedHLLiveExposure(t, HLPosition{Coin: "ETH", Size: 0.1})
		h.reply(provenLimitStatus(9001, "ETH", "cancelled", 0.5, 0.4, 2000, 0.2))
		h.run()
		rows := h.rows()
		if len(rows) != 1 || rows[0].FilledSize != 0.3 || rows[0].OperatorRequiredSince.IsZero() {
			t.Fatalf("the refused booking must keep and mark the row: %+v", rows)
		}
		if h.dmsContaining("Limit fill NOT booked") != 1 || h.dmsContaining("the scheduler booked 0.300000 of it, so 0.100000 was never booked. Flatten on Hyperliquid ONLY that unbooked 0.100000") != 1 {
			t.Fatalf("a proven watermark must keep the exact flatten size, dms = %s", h.dmText())
		}
	})

	t.Run("UnreadVenueStatusClaimsNoVenueProof", func(t *testing.T) {
		h := newLimitProofHarness(t, 0.5)
		h.seedUnprovenRowAboveItsBook()
		h.status = func() (*HyperliquidLimitStatusResult, error) {
			return nil, errors.New("HTTP 429")
		}
		h.run()
		rows := h.rows()
		if len(rows) != 1 || !rows[0].OperatorRequiredSince.IsZero() {
			t.Fatalf("an unread row must stay and carry no terminal marker: %+v", rows)
		}
		text := h.dmText()
		if h.dmsContaining("Limit row NOT finalized") != 1 || h.dmsContaining("this pass did not read an executed size for the order from the venue") != 1 {
			t.Fatalf("the alert must say the venue was not read, dms = %s", text)
		}
		for _, banned := range []string{"the venue proves", "the venue reports an executed size", "The venue reports an executed size", "the venue proof is complete", "The venue confirms", "0.000000", "Then clear the record"} {
			if strings.Contains(text, banned) {
				t.Fatalf("the alert must state no venue fact that this pass did not read (%q): %s", banned, text)
			}
		}
		if h.dmsContaining("only after the scheduler proves the order is terminal and off the book") != 1 {
			t.Fatalf("the alert must say the row is not clearable yet, dms = %s", text)
		}
	})

	t.Run("RestingUnprovenRowSaysToCancelFirst", func(t *testing.T) {
		h := newLimitProofHarness(t, 0.5)
		h.seedUnprovenRowAboveItsBook()
		withStubbedHLLiveExposure(t, HLPosition{Coin: "ETH", Size: 0.3})
		h.reply(provenLimitStatus(9001, "ETH", "live", 0.5, 0.3, 2000, 0.15))
		h.run()
		rows := h.rows()
		if len(rows) != 1 || !rows[0].OperatorRequiredSince.IsZero() {
			t.Fatalf("a resting row must stay and carry no terminal marker: %+v", rows)
		}
		text := h.dmText()
		if h.dmsContaining("Limit row NOT finalized") != 1 || h.dmsContaining("cancel the order with `go-trader manual-cancel "+h.sc.ID+"` if it still rests") != 1 {
			t.Fatalf("the alert must tell the operator to cancel the resting order first, dms = %s", text)
		}
		if strings.Contains(text, "Then clear the record") || strings.Contains(text, "the venue proves") {
			t.Fatalf("the alert must not send the operator to the clear command while the order rests: %s", text)
		}
	})
}

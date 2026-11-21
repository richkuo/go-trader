package main

import (
	"errors"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

const (
	settleOwner = "hl-manual-eth-live"
	settlePeer  = "hl-manual-eth-peer"
)

type limitSettleEnv struct {
	t        *testing.T
	split    bool
	cfg      *Config
	store    *StateStore
	state    *AppState
	mu       sync.RWMutex
	notifier *MultiNotifier
	mock     *mockNotifier

	statusMu   sync.Mutex
	status     map[int64]HyperliquidLimitOrderStatus
	exposure   []HLPosition
	plans      []hlProtectionPlan
	cancels    []int64
	statusRead int
}

func settleManualSC(id string) StrategyConfig {
	slMult := 1.5
	return StrategyConfig{
		ID: id, Type: "manual", Platform: "hyperliquid", Symbol: "ETH",
		Script: "shared_scripts/check_hyperliquid.py", Leverage: 10,
		Args:            []string{"hold", "ETH", "30m", "--mode=live"},
		StopLossATRMult: &slMult,
	}
}

func newLimitSettleEnv(t *testing.T, split bool, withPeer bool) *limitSettleEnv {
	t.Helper()
	dir := t.TempDir()
	cfg := &Config{DBFile: filepath.Join(dir, "live.db")}
	cfg.Strategies = []StrategyConfig{settleManualSC(settleOwner)}
	if withPeer {
		cfg.Strategies = append(cfg.Strategies, settleManualSC(settlePeer))
	}
	cfg.Strategies = append(cfg.Strategies, StrategyConfig{ID: "hl-paper", Type: "perps", Platform: "hyperliquid", Symbol: "ETH", Args: []string{"momentum", "ETH", "1h", "--mode=paper"}})
	if split {
		cfg.PaperDBFile = filepath.Join(dir, "paper.db")
		cfg.PaperSources = []PaperSourceConfig{{ID: "src", DBFile: filepath.Join(dir, "src.db")}}
		cfg.Strategies = append(cfg.Strategies, StrategyConfig{ID: "hl-paper-src", Type: "perps", Platform: "hyperliquid", Symbol: "ETH", Args: []string{"momentum", "ETH", "1h", "--mode=paper"}, PaperSource: "src"})
	}
	e := &limitSettleEnv{t: t, split: split, cfg: cfg, status: make(map[int64]HyperliquidLimitOrderStatus)}
	e.store = e.open()
	e.state = NewAppState()
	for _, sc := range cfg.Strategies {
		e.state.Strategies[sc.ID] = &StrategyState{ID: sc.ID, Type: sc.Type, Platform: "hyperliquid", Cash: 10000, InitialCapital: 10000,
			Positions: map[string]*Position{}, OptionPositions: map[string]*OptionPosition{}}
	}
	if err := SaveStateWithStore(e.state, e.store); err != nil {
		t.Fatalf("seed save: %v", err)
	}
	e.notifier, e.mock = newOrphanLaneNotifier()
	limitFillExposureAlerts.reset()
	limitSettlementAlerts.reset()
	limitRowReviewAlerts.reset()
	limitApplyFailureAlerts.reset()
	limitRowUnresolvedAlerts.reset()
	retainLimitRowUnresolved(nil)
	orphanLimitCancelAlerts.reset()

	t.Setenv("HYPERLIQUID_ACCOUNT_ADDRESS", "0xsettle")
	origFetch := fetchHyperliquidStateFn
	origStatus := runHyperliquidLimitStatusFn
	origCancel := runHyperliquidCancelOrderFn
	origSync := syncHyperliquidProtection
	origRecorder := tradeRecorder
	origHook := storeCommitHook
	fetchHyperliquidStateFn = func(string) (float64, []HLPosition, error) {
		e.statusMu.Lock()
		defer e.statusMu.Unlock()
		return 0, append([]HLPosition(nil), e.exposure...), nil
	}
	runHyperliquidLimitStatusFn = func(_, symbol string, oids []int64, _ int64, _ ...limitStatusWatermark) (*HyperliquidLimitStatusResult, string, error) {
		e.statusMu.Lock()
		defer e.statusMu.Unlock()
		e.statusRead++
		res := &HyperliquidLimitStatusResult{}
		for _, oid := range oids {
			if st, ok := e.status[oid]; ok {
				res.Orders = append(res.Orders, limitTestEvidence(symbol, st))
			}
		}
		return res, "", nil
	}
	runHyperliquidCancelOrderFn = func(_, _ string, oid int64) (*HyperliquidCancelOrderResult, string, error) {
		e.cancels = append(e.cancels, oid)
		return &HyperliquidCancelOrderResult{OID: oid, Cancelled: true}, "", nil
	}
	syncHyperliquidProtection = func(_ StrategyConfig, plan hlProtectionPlan, _ *MultiNotifier, _ *StrategyLogger, _ []byte) (*HyperliquidProtectionSyncResult, bool) {
		e.plans = append(e.plans, plan)
		return &HyperliquidProtectionSyncResult{}, true
	}
	tradeRecorder = e.store.InsertTrade
	t.Cleanup(func() {
		fetchHyperliquidStateFn = origFetch
		runHyperliquidLimitStatusFn = origStatus
		runHyperliquidCancelOrderFn = origCancel
		syncHyperliquidProtection = origSync
		tradeRecorder = origRecorder
		storeCommitHook = origHook
	})
	return e
}

func (e *limitSettleEnv) open() *StateStore {
	e.t.Helper()
	store := openSplitStore(e.t, e.cfg)
	if store.Split() != e.split {
		e.t.Fatalf("layout split = %v, want %v", store.Split(), e.split)
	}
	return store
}

func (e *limitSettleEnv) insertRow(strategyID string, oid int64, size float64) PendingLimitOrder {
	e.t.Helper()
	row := PendingLimitOrder{StrategyID: strategyID, Symbol: "ETH", Side: "long", OrderOID: oid,
		LimitPrice: 2000, OrderSize: size, TIF: "Alo", EntryATR: 40, CreatedAt: time.Now().UTC()}
	id, err := e.store.InsertPendingLimitOrder(row)
	if err != nil {
		e.t.Fatalf("insert row: %v", err)
	}
	row.ID = id
	return row
}

func (e *limitSettleEnv) venue(oid int64, resting bool, filled, px, fee float64) {
	e.statusMu.Lock()
	defer e.statusMu.Unlock()
	e.status[oid] = HyperliquidLimitOrderStatus{OID: oid, Resting: limitTestBoolPtr(resting), FilledSize: filled, AvgPx: px, Fee: fee, Count: 1}
}

func (e *limitSettleEnv) account(net float64) {
	e.statusMu.Lock()
	defer e.statusMu.Unlock()
	e.exposure = []HLPosition{{Coin: "ETH", Size: net, EntryPrice: 2000}}
}

func (e *limitSettleEnv) reconcile() []manualAlert {
	return reconcilePendingLimitOrders(e.state, e.cfg, e.store, &e.mu, e.notifier, nil)
}

type limitSettleDurable struct {
	state  *AppState
	rows   []PendingLimitOrder
	trades map[string]int
}

func (e *limitSettleEnv) reopen() limitSettleDurable {
	e.t.Helper()
	store := e.open()
	state, _, err := LoadStateWithStore(e.cfg, store)
	if err != nil {
		e.t.Fatalf("reload: %v", err)
	}
	rows, err := store.LoadPendingLimitOrders()
	if err != nil {
		e.t.Fatalf("reload rows: %v", err)
	}
	out := limitSettleDurable{state: state, rows: rows, trades: map[string]int{}}
	primary, _ := store.liveFile()
	for _, id := range []string{settleOwner, settlePeer} {
		var n int
		if err := primary.db.QueryRow("SELECT COUNT(*) FROM trades WHERE strategy_id = ?", id).Scan(&n); err != nil {
			e.t.Fatalf("count trades: %v", err)
		}
		out.trades[id] = n
	}
	return out
}

func (e *limitSettleEnv) dmsContaining(sub string) int {
	n := 0
	for _, dm := range e.mock.dms {
		if strings.Contains(dm.content, sub) {
			n++
		}
	}
	return n
}

func eachLimitSettleLayout(t *testing.T, run func(t *testing.T, split bool)) {
	for _, split := range []bool{false, true} {
		name := "one-file"
		if split {
			name = "split-with-paper-source"
		}
		t.Run(name, func(t *testing.T) { run(t, split) })
	}
}

func TestManualLimitFillSettlementIntegration(t *testing.T) {
	t.Run("FailureBeforeCommitKeepsTheSavedStateAndRetriesOnce", func(t *testing.T) {
		eachLimitSettleLayout(t, func(t *testing.T, split bool) {
			e := newLimitSettleEnv(t, split, false)
			row := e.insertRow(settleOwner, 9001, 1.0)
			e.venue(9001, true, 0.4, 2000, 0.2)
			e.account(0.4)

			storeCommitHook = func(storageRole) error { return errors.New("injected crash before commit") }
			for pass := 0; pass < 3; pass++ {
				e.reconcile()
				if pos := e.state.Strategies[settleOwner].Positions["ETH"]; pos != nil {
					t.Fatalf("pass %d published a speculative position %+v", pass, pos)
				}
				if got := e.state.Strategies[settleOwner].Cash; got != 10000 {
					t.Fatalf("pass %d charged the fee before commit: cash %v", pass, got)
				}
				if !e.store.persistenceHoldsPartition(livePartition) {
					t.Fatalf("pass %d: the live partition must stay held while the fill is unsaved", pass)
				}
				d := e.reopen()
				if p := d.state.Strategies[settleOwner].Positions["ETH"]; p != nil || d.state.Strategies[settleOwner].Cash != 10000 || d.trades[settleOwner] != 0 {
					t.Fatalf("pass %d durable book moved: pos=%+v cash=%v trades=%d", pass, p, d.state.Strategies[settleOwner].Cash, d.trades[settleOwner])
				}
				if len(d.rows) != 1 || d.rows[0].FilledSize != 0 || d.rows[0].BookFilledSize != 0 {
					t.Fatalf("pass %d durable watermark moved: %+v", pass, d.rows)
				}
			}
			storeCommitHook = nil
			if err := SaveStateWithStore(e.state, e.store); err != nil {
				t.Fatalf("unrelated save: %v", err)
			}
			if !e.store.persistenceHoldsPartition(livePartition) {
				t.Fatal("an unrelated successful save must not release the recovery hold")
			}
			if n := e.dmsContaining("NO LIVE EXPOSURE"); n != 0 {
				t.Fatalf("failed passes must not raise a false no-exposure alert, got %d", n)
			}
			if n := e.dmsContaining("Limit fill not saved"); n != 1 {
				t.Fatalf("the save failure must page the owner once per throttle window, got %d", n)
			}

			e.reconcile()
			if e.store.persistenceHoldsPartition(livePartition) {
				t.Fatal("a successful settlement must release the recovery hold")
			}
			d := e.reopen()
			pos := d.state.Strategies[settleOwner].Positions["ETH"]
			if pos == nil || pos.Quantity != 0.4 || pos.AvgCost != 2000 {
				t.Fatalf("durable position = %+v, want 0.4 @ 2000", pos)
			}
			if got := d.state.Strategies[settleOwner].Cash; got != 10000-0.2 {
				t.Fatalf("durable cash = %v, want the fee charged once", got)
			}
			if d.trades[settleOwner] != 1 {
				t.Fatalf("durable trades = %d, want exactly one open", d.trades[settleOwner])
			}
			if len(d.rows) != 1 || d.rows[0].ID != row.ID || d.rows[0].FilledSize != 0.4 || !d.rows[0].fillSealed() {
				t.Fatalf("durable row = %+v, want a sealed 0.4 watermark", d.rows)
			}
			if d.state.Strategies["hl-paper"] == nil || d.state.Strategies["hl-paper"].Cash != 10000 {
				t.Fatalf("paper book must be untouched: %+v", d.state.Strategies["hl-paper"])
			}
			if split && (d.state.Strategies["hl-paper-src"] == nil || d.state.Strategies["hl-paper-src"].Cash != 10000) {
				t.Fatalf("paper source book must be untouched: %+v", d.state.Strategies["hl-paper-src"])
			}
		})
	})

	t.Run("CommitBeforeProtectionRecoversOnRestartWithoutRebooking", func(t *testing.T) {
		eachLimitSettleLayout(t, func(t *testing.T, split bool) {
			e := newLimitSettleEnv(t, split, false)
			e.insertRow(settleOwner, 9001, 1.0)
			e.venue(9001, true, 0.4, 2000, 0.2)
			e.account(0.4)
			syncCalls := 0
			syncHyperliquidProtection = func(StrategyConfig, hlProtectionPlan, *MultiNotifier, *StrategyLogger, []byte) (*HyperliquidProtectionSyncResult, bool) {
				syncCalls++
				d := e.reopen()
				if p := d.state.Strategies[settleOwner].Positions["ETH"]; p == nil || p.Quantity != 0.4 {
					t.Errorf("protection ran before the fill was durable: %+v", p)
				}
				if len(d.rows) != 1 || d.rows[0].FilledSize != 0.4 || !d.rows[0].fillSealed() || d.trades[settleOwner] != 1 {
					t.Errorf("protection ran before the seal and trade were durable: rows=%+v trades=%d", d.rows, d.trades[settleOwner])
				}
				return &HyperliquidProtectionSyncResult{}, true
			}
			e.reconcile()
			if syncCalls != 1 {
				t.Fatalf("protection sync calls = %d, want one after the commit", syncCalls)
			}

			restarted := newLimitSettleRestart(t, e)
			restarted.reconcile()
			d := e.reopen()
			if d.trades[settleOwner] != 1 {
				t.Fatalf("restart booked the committed fill again: trades=%d", d.trades[settleOwner])
			}
			if pos := restarted.state.Strategies[settleOwner].Positions["ETH"]; pos == nil || pos.Quantity != 0.4 {
				t.Fatalf("restart book = %+v, want the committed 0.4", pos)
			}
			if restarted.store.persistenceHoldsPartition(livePartition) {
				t.Fatal("restart must release the startup hold once the queue pass completes")
			}
		})
	})

	t.Run("LaterFillAfterAReductionAddsTheDeltaAndChargesItsFeeOnce", func(t *testing.T) {
		e := newLimitSettleEnv(t, false, false)
		e.insertRow(settleOwner, 9001, 1.0)
		e.venue(9001, true, 0.4, 2000, 0.2)
		e.account(0.4)
		e.reconcile()
		e.mu.Lock()
		e.state.Strategies[settleOwner].Positions["ETH"].Quantity = 0.1
		e.mu.Unlock()
		e.venue(9001, false, 0.7, 2010, 0.35)
		e.account(0.4)
		e.reconcile()
		pos := e.state.Strategies[settleOwner].Positions["ETH"]
		if pos == nil || pos.Quantity < 0.4-1e-9 || pos.Quantity > 0.4+1e-9 {
			t.Fatalf("position = %+v, want 0.1 remaining + 0.3 delta", pos)
		}
		if got := e.state.Strategies[settleOwner].Cash; got < 10000-0.35-1e-9 || got > 10000-0.35+1e-9 {
			t.Fatalf("cash = %v, want both fee slices charged once", got)
		}
		d := e.reopen()
		if len(d.rows) != 0 {
			t.Fatalf("a sealed, terminal, fully booked row must finalize: %+v", d.rows)
		}
		if n := e.dmsContaining("recorded 0.700000 of 1.000000"); n != 1 {
			t.Fatalf("partial-fill message must state the recorded fill once, dms=%+v", e.mock.dms)
		}
		if n := e.dmsContaining("position tracked"); n != 0 {
			t.Fatal("the partial-fill message must not claim the position is still open")
		}
	})

	t.Run("SharedCoinFillsSettleTogetherWithOwnProtectionSizes", func(t *testing.T) {
		eachLimitSettleLayout(t, func(t *testing.T, split bool) {
			e := newLimitSettleEnv(t, split, true)
			e.insertRow(settleOwner, 9001, 1.0)
			e.insertRow(settlePeer, 9002, 1.0)
			e.venue(9001, true, 0.3, 2000, 0.1)
			e.venue(9002, true, 0.2, 2002, 0.05)
			e.account(0.5)
			alerts := e.reconcile()
			if len(alerts) != 2 {
				t.Fatalf("alerts = %+v, want one per strategy", alerts)
			}
			d := e.reopen()
			a := d.state.Strategies[settleOwner].Positions["ETH"]
			b := d.state.Strategies[settlePeer].Positions["ETH"]
			if a == nil || a.Quantity != 0.3 || b == nil || b.Quantity != 0.2 {
				t.Fatalf("attribution = %+v / %+v, want 0.3 / 0.2", a, b)
			}
			for _, r := range d.rows {
				if !r.fillSealed() || r.FilledSize == 0 {
					t.Fatalf("row %+v must be sealed at its own fill", r)
				}
			}
			if len(e.plans) != 2 {
				t.Fatalf("protection plans = %+v, want one per committed fill", e.plans)
			}
			for _, plan := range e.plans {
				if plan.Size != 0.3 && plan.Size != 0.2 {
					t.Fatalf("protection sized %v, want each strategy's own committed share", plan.Size)
				}
			}

			e.account(0.4)
			e.venue(9001, true, 0.6, 2000, 0.2)
			e.reconcile()
			if p := e.state.Strategies[settleOwner].Positions["ETH"]; p.Quantity != 0.3 {
				t.Fatalf("an unbacked later fill must not move the book, got %+v", p)
			}
		})
	})
}

func newLimitSettleRestart(t *testing.T, prev *limitSettleEnv) *limitSettleEnv {
	t.Helper()
	e := &limitSettleEnv{t: t, split: prev.split, cfg: prev.cfg, status: prev.status, exposure: prev.exposure}
	e.store = e.open()
	state, _, err := LoadStateWithStore(e.cfg, e.store)
	if err != nil {
		t.Fatalf("restart load: %v", err)
	}
	e.state = state
	e.notifier, e.mock = newOrphanLaneNotifier()
	e.store.beginLimitRecovery()
	if !e.store.persistenceHoldsPartition(livePartition) {
		t.Fatal("a restarted scheduler must hold live entries until the first queue pass")
	}
	runHyperliquidLimitStatusFn = func(_, symbol string, oids []int64, _ int64, _ ...limitStatusWatermark) (*HyperliquidLimitStatusResult, string, error) {
		res := &HyperliquidLimitStatusResult{}
		for _, oid := range oids {
			if st, ok := e.status[oid]; ok {
				res.Orders = append(res.Orders, limitTestEvidence(symbol, st))
			}
		}
		return res, "", nil
	}
	syncHyperliquidProtection = func(StrategyConfig, hlProtectionPlan, *MultiNotifier, *StrategyLogger, []byte) (*HyperliquidProtectionSyncResult, bool) {
		t.Error("restart must not re-run the fill's protection sync as a new fill")
		return &HyperliquidProtectionSyncResult{}, true
	}
	tradeRecorder = e.store.InsertTrade
	return e
}

func TestManualLimitRowFinalizeIntegration(t *testing.T) {
	t.Run("EveryAutomaticDeletePathChecksTheSeal", func(t *testing.T) {
		e := newLimitSettleEnv(t, false, true)
		zero := e.insertRow(settleOwner, 9001, 1.0)
		sealed := e.insertRow(settlePeer, 9002, 1.0)
		e.state.Strategies[settlePeer].Positions["ETH"] = &Position{Symbol: "ETH", Side: "long", Quantity: 0.4, InitialQuantity: 0.4, AvgCost: 2000, Multiplier: 1, OwnerStrategyID: settlePeer}
		sealLimitRowFixture(t, e.store.primary(), e.state, sealed.ID, 0.4, 2000, 0.1)
		rows, _ := e.store.LoadPendingLimitOrders()
		var sealedRow PendingLimitOrder
		for _, r := range rows {
			if r.ID == sealed.ID {
				sealedRow = r
			}
		}
		legacy := e.insertRow(settleOwner, 9003, 1.0)
		if err := e.store.UpdatePendingLimitOrderFill(legacy.ID, 0.2, 2000, 0.1); err != nil {
			t.Fatal(err)
		}
		rows, _ = e.store.LoadPendingLimitOrders()
		var legacyRow PendingLimitOrder
		for _, r := range rows {
			if r.ID == legacy.ID {
				legacyRow = r
			}
		}
		changed := sealedRow
		changed.FilledSize = 0.3
		changed.BookFilledSize = 0.3

		cases := []struct {
			name string
			f    limitRowFinalize
			ok   bool
			want string
		}{
			{"unsealed legacy row", limitRowFinalize{Row: legacyRow, VenueFilled: 0.2, Eligible: true}, false, "not sealed"},
			{"changed row", limitRowFinalize{Row: changed, VenueFilled: 0.3, Eligible: true}, false, errLimitRowChanged.Error()},
			{"later unbooked fill", limitRowFinalize{Row: sealedRow, VenueFilled: 0.5, Eligible: true}, false, "later fill"},
			{"no eligible owner", limitRowFinalize{Row: sealedRow, VenueFilled: 0.4}, false, "no configured"},
			{"operator override on an unmarked row", limitRowFinalize{Row: legacyRow, OperatorSeal: true}, false, "not marked"},
			{"zero row after terminal status", limitRowFinalize{Row: zero, VenueFilled: 0}, true, ""},
			{"sealed row", limitRowFinalize{Row: sealedRow, VenueFilled: 0.4, Eligible: true}, true, ""},
		}
		for _, tc := range cases {
			err := e.store.FinalizePendingLimitOrder(tc.f)
			if tc.ok != (err == nil) || (err != nil && !strings.Contains(err.Error(), tc.want)) {
				t.Errorf("%s: err = %v, want ok=%v containing %q", tc.name, err, tc.ok, tc.want)
			}
		}
		rows, _ = e.store.LoadPendingLimitOrders()
		if len(rows) != 1 || rows[0].ID != legacy.ID {
			t.Fatalf("only the unsealed legacy row may remain, got %+v", rows)
		}

		stubs := &killSwitchLimitStubs{}
		deps := stubs.deps(okCancel, func(_, _ string, oids []int64, _ int64) (*HyperliquidLimitStatusResult, string, error) {
			return &HyperliquidLimitStatusResult{Orders: []HyperliquidLimitOrderStatus{{OID: oids[0], Resting: limitTestBoolPtr(false), FilledSize: 0.2, AvgPx: 2000}}}, "", nil
		}, e.store.FinalizePendingLimitOrder)
		report := cancelKillSwitchRestingLimitOrders(e.store.LoadPendingLimitOrders, killSwitchLimitOrderRoster(e.cfg.Strategies), deps, time.Minute)
		if report.ConfirmedClear() || len(stubs.deleted) != 0 {
			t.Fatalf("the kill switch must keep an unsealed row and block reset: %+v", report)
		}
	})

	t.Run("OperatorClearAndSettlementCannotBothWinOnAStaleRow", func(t *testing.T) {
		for _, winner := range []string{"clear", "settle"} {
			t.Run(winner+"-first", func(t *testing.T) {
				e := newLimitSettleEnv(t, false, false)
				row := e.insertRow(settleOwner, 9001, 1.0)
				primary, _ := e.store.liveFile()
				if _, err := primary.MarkPendingLimitOrderOperatorRequiredOnce(row.ID, time.Now().UTC()); err != nil {
					t.Fatal(err)
				}
				rows, _ := e.store.LoadPendingLimitOrders()
				stale := rows[0]

				other, err := OpenStateDB(primary.path)
				if err != nil {
					t.Fatal(err)
				}
				defer other.Close()

				view := newLimitSettlementBatch(e.state)
				staged := view.stage(settleOwner)
				cand := &limitFillCandidate{order: stale, sc: settleManualSC(settleOwner), status: HyperliquidLimitOrderStatus{FilledSize: 0.4, Fee: 0.2}, avgPx: 2000}
				if _, _, err := applyLimitFillProgressLogged(view.view, cand.sc, stale, 0.4, 2000, 0.2, 40, ATRMethodSimple, time.Now().UTC()); err != nil {
					t.Fatal(err)
				}
				settle := func() error {
					return e.store.SettleManualLimitFills(view.view, view.order, []limitFillSeal{{Prev: stale, FilledSize: 0.4, AvgFillPrice: 2000, FillFee: 0.2}})
				}
				clear := func() error {
					return other.FinalizePendingLimitOrder(limitRowFinalize{Row: stale, OperatorSeal: true})
				}
				if winner == "clear" {
					if err := clear(); err != nil {
						t.Fatalf("clear: %v", err)
					}
					if err := settle(); err == nil || !errors.Is(err, errLimitRowChanged) {
						t.Fatalf("settlement on a cleared row must roll back, err = %v", err)
					}
					if staged.Positions["ETH"] == nil || e.state.Strategies[settleOwner].Positions["ETH"] != nil {
						t.Fatal("the staged book must never reach the published state after a rollback")
					}
					if err := SaveStateWithStore(e.state, e.store); err != nil {
						t.Fatal(err)
					}
					d := e.reopen()
					if d.state.Strategies[settleOwner].Positions["ETH"] != nil || d.trades[settleOwner] != 0 || d.state.Strategies[settleOwner].Cash != 10000 {
						t.Fatalf("a later save leaked the speculative fill: %+v trades=%d", d.state.Strategies[settleOwner], d.trades[settleOwner])
					}
					return
				}
				if err := settle(); err != nil {
					t.Fatalf("settle: %v", err)
				}
				if err := clear(); err == nil || !errors.Is(err, errLimitRowChanged) {
					t.Fatalf("a clear based on the old row must refuse, err = %v", err)
				}
				d := e.reopen()
				if len(d.rows) != 1 || d.rows[0].FilledSize != 0.4 {
					t.Fatalf("the settled row must survive the stale clear: %+v", d.rows)
				}
			})
		}
	})

	t.Run("OperatorOverrideRefusesWhileTheOrderCanFill", func(t *testing.T) {
		e := newLimitSettleEnv(t, false, false)
		row := e.insertRow(settleOwner, 9001, 1.0)
		e.venue(9001, true, 0, 0, 0)
		if got := clearLimitRowTerminalRefusal(e.cfg, row); !strings.Contains(got, "can still fill") {
			t.Fatalf("a resting order must refuse the override, got %q", got)
		}
		e.statusMu.Lock()
		e.status[9001] = HyperliquidLimitOrderStatus{OID: 9001, Coin: "ETH", OrderStatus: "open", Resting: limitTestBoolPtr(false), FillsComplete: true}
		e.statusMu.Unlock()
		if got := clearLimitRowTerminalRefusal(e.cfg, row); !strings.Contains(got, "could not be verified") {
			t.Fatalf("a live order missing from the open list is unresolved, got %q", got)
		}
		e.venue(9001, false, 0, 0, 0)
		if got := clearLimitRowTerminalRefusal(e.cfg, row); got != "" {
			t.Fatalf("a confirmed terminal order must pass, got %q", got)
		}
	})
}

func TestManualLimitLegacyRowsIntegration(t *testing.T) {
	t.Run("MigrationIsIdempotentAndAReadOnlyOlderFileReadsUnsealed", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "state.db")
		db, err := OpenStateDB(path)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := db.db.Exec("ALTER TABLE pending_limit_orders DROP COLUMN book_filled_size"); err != nil {
			t.Fatalf("simulate the older schema: %v", err)
		}
		if _, err := db.db.Exec(`INSERT INTO pending_limit_orders (strategy_id, symbol, side, order_oid, limit_price, order_size, tif, filled_size, avg_fill_price, fill_fee, entry_atr, cancel_requested, expires_at, created_at)
			VALUES (?, 'ETH', 'long', 9001, 2000, 1, 'Alo', 0.4, 2000, 0.2, 40, 0, '', ?)`, settleOwner, formatTime(time.Now().UTC())); err != nil {
			t.Fatal(err)
		}
		db.Close()

		ro, err := openStateDBReadOnly(path)
		if err != nil {
			t.Fatal(err)
		}
		rows, err := ro.LoadPendingLimitOrders()
		ro.Close()
		if err != nil || len(rows) != 1 || rows[0].fillSealed() || rows[0].BookFilledSize != 0 {
			t.Fatalf("read-only older file must read the row as unsealed: %+v %v", rows, err)
		}
		for i := 0; i < 2; i++ {
			again, err := OpenStateDB(path)
			if err != nil {
				t.Fatalf("migration %d: %v", i, err)
			}
			rows, err = again.LoadPendingLimitOrders()
			again.Close()
			if err != nil || len(rows) != 1 || rows[0].fillSealed() {
				t.Fatalf("migration %d must keep the legacy row unsealed: %+v %v", i, rows, err)
			}
		}
	})

	cases := []struct {
		name  string
		setup func(e *limitSettleEnv, row PendingLimitOrder)
		venue float64
	}{
		{"lost first fill", func(e *limitSettleEnv, row PendingLimitOrder) {
			if err := e.store.UpdatePendingLimitOrderFill(row.ID, 0.4, 2000, 0.2); err != nil {
				e.t.Fatal(err)
			}
		}, 0.4},
		{"lost later fill with an eager trade", func(e *limitSettleEnv, row PendingLimitOrder) {
			pos := &Position{Symbol: "ETH", Side: "long", Quantity: 0.4, InitialQuantity: 0.4, AvgCost: 2000, Multiplier: 1, OwnerStrategyID: settleOwner, TradePositionID: "pos-shared"}
			e.state.Strategies[settleOwner].Positions["ETH"] = pos
			if err := SaveStateWithStore(e.state, e.store); err != nil {
				e.t.Fatal(err)
			}
			if err := e.store.InsertTrade(settleOwner, Trade{StrategyID: settleOwner, Symbol: "ETH", Side: "buy", Quantity: 0.3, Price: 2000, PositionID: "pos-shared", TradeType: scaleInTradeType, Timestamp: time.Now().UTC()}); err != nil {
				e.t.Fatal(err)
			}
			if err := e.store.UpdatePendingLimitOrderFill(row.ID, 0.7, 2000, 0.3); err != nil {
				e.t.Fatal(err)
			}
		}, 0.7},
		{"watermark advanced by an older build after a downgrade", func(e *limitSettleEnv, row PendingLimitOrder) {
			pos := &Position{Symbol: "ETH", Side: "long", Quantity: 0.4, InitialQuantity: 0.4, AvgCost: 2000, Multiplier: 1, OwnerStrategyID: settleOwner}
			e.state.Strategies[settleOwner].Positions["ETH"] = pos
			sealLimitRowFixture(e.t, e.store.primary(), e.state, row.ID, 0.4, 2000, 0.2)
			if err := e.store.UpdatePendingLimitOrderFill(row.ID, 0.6, 2000, 0.25); err != nil {
				e.t.Fatal(err)
			}
		}, 0.6},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newLimitSettleEnv(t, false, true)
			row := e.insertRow(settleOwner, 9001, 1.0)
			tc.setup(e, row)
			peerRow := e.insertRow(settlePeer, 9002, 1.0)
			e.venue(9001, false, tc.venue, 2000, 0.3)
			e.venue(9002, true, 0.1, 2000, 0.05)
			e.account(tc.venue + 0.1)
			before := e.reopen()

			e.reconcile()
			first := e.reopen()
			var marked time.Time
			for _, r := range first.rows {
				if r.ID == row.ID {
					marked = r.OperatorRequiredSince
				}
				if r.ID == peerRow.ID && r.FilledSize != 0 {
					t.Fatalf("a fill on a coin with an unresolved legacy row must be deferred, got %+v", r)
				}
			}
			if len(first.rows) != 2 || marked.IsZero() {
				t.Fatalf("the legacy row must remain and be marked: %+v", first.rows)
			}
			if first.trades[settleOwner] != before.trades[settleOwner] {
				t.Fatalf("no trade may be booked from a legacy row")
			}
			bookBefore := before.state.Strategies[settleOwner].Positions["ETH"]
			bookAfter := first.state.Strategies[settleOwner].Positions["ETH"]
			if (bookBefore == nil) != (bookAfter == nil) || (bookAfter != nil && bookAfter.Quantity != bookBefore.Quantity) {
				t.Fatalf("the book must not move: %+v -> %+v", bookBefore, bookAfter)
			}

			e.reconcile()
			second := e.reopen()
			for _, r := range second.rows {
				if r.ID == row.ID && !r.OperatorRequiredSince.Equal(marked) {
					t.Fatalf("the first operator-required time must be kept: %v -> %v", marked, r.OperatorRequiredSince)
				}
			}
			if n := e.dmsContaining("needs review"); n != 1 {
				t.Fatalf("review alerts must be throttled to one, got %d", n)
			}

			if bookAfter != nil {
				hlLimitGrowthHolds.refresh(e.store.LoadPendingLimitOrders)
				defer hlLimitGrowthHolds.clear()
				ss := e.state.Strategies[settleOwner]
				logger := newTestLogger(t)
				reconcileHyperliquidPositionsWithResolver(ss, "ETH", []HLPosition{{Coin: "ETH", Size: tc.venue, EntryPrice: 2000}}, nil, logger, nil, nil, settleManualSC(settleOwner))
				if ss.Positions["ETH"].Quantity != bookAfter.Quantity {
					t.Fatalf("generic reconcile grew the book from venue size alone: %v", ss.Positions["ETH"].Quantity)
				}
			}
		})
	}
}

func TestStandaloneEntryRefusesAnUnsealedQueueRow(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	db, err := OpenStateDB(path)
	if err != nil {
		t.Fatal(err)
	}
	id, err := db.InsertPendingLimitOrder(PendingLimitOrder{StrategyID: settleOwner, Symbol: "ETH", Side: "long", OrderOID: 9001, LimitPrice: 2000, OrderSize: 1, TIF: "Alo", CreatedAt: time.Now().UTC()})
	if err != nil {
		t.Fatal(err)
	}
	layout := storageLayout{Files: []storageFileSpec{{Role: storageRolePrimary, Path: path, Partitions: []RiskPartition{livePartition, defaultPaperPartition}}}}
	if err := standaloneLimitQueueRefusal(layout, []StrategyConfig{settleManualSC(settleOwner)}); err != nil {
		t.Fatalf("a zero-fill row must not refuse: %v", err)
	}
	if err := db.UpdatePendingLimitOrderFill(id, 0.2, 2000, 0.1); err != nil {
		t.Fatal(err)
	}
	if err := standaloneLimitQueueRefusal(layout, []StrategyConfig{settleManualSC(settleOwner)}); err == nil || !strings.Contains(err.Error(), "not sealed") {
		t.Fatalf("an unsealed fill must refuse a standalone live entry, got %v", err)
	}
	if _, err := db.db.Exec("DROP TABLE pending_limit_orders"); err != nil {
		t.Fatal(err)
	}
	db.Close()
	if err := standaloneLimitQueueRefusal(layout, []StrategyConfig{settleManualSC(settleOwner)}); err == nil || !strings.Contains(err.Error(), "cannot be verified") {
		t.Fatalf("an unreadable queue must refuse, got %v", err)
	}
}

func (e *limitSettleEnv) row(id int64) (PendingLimitOrder, bool) {
	e.t.Helper()
	rows, err := e.store.LoadPendingLimitOrders()
	if err != nil {
		e.t.Fatalf("load rows: %v", err)
	}
	for _, r := range rows {
		if r.ID == id {
			return r, true
		}
	}
	return PendingLimitOrder{}, false
}

func (e *limitSettleEnv) rawStatus(st HyperliquidLimitOrderStatus) {
	e.statusMu.Lock()
	defer e.statusMu.Unlock()
	e.status[st.OID] = st
}

func (e *limitSettleEnv) sealedOwnerWithDeletedPosition() PendingLimitOrder {
	e.t.Helper()
	row := e.insertRow(settleOwner, 9001, 1.0)
	e.venue(9001, true, 0.3, 2000, 0.1)
	e.account(0.3)
	e.reconcile()
	if pos := e.state.Strategies[settleOwner].Positions["ETH"]; pos == nil || pos.Quantity != 0.3 {
		e.t.Fatalf("setup: owner book = %+v, want 0.3", pos)
	}
	e.mu.Lock()
	delete(e.state.Strategies[settleOwner].Positions, "ETH")
	err := SaveStateWithStore(e.state, e.store)
	e.mu.Unlock()
	if err != nil {
		e.t.Fatalf("setup save: %v", err)
	}
	e.plans = nil
	return row
}

func TestManualLimitUnappliableRowIntegration(t *testing.T) {
	t.Run("PeerFillBooksAndTheFailedRowGoesToTheOperator", func(t *testing.T) {
		eachLimitSettleLayout(t, func(t *testing.T, split bool) {
			e := newLimitSettleEnv(t, split, true)
			a := e.sealedOwnerWithDeletedPosition()
			b := e.insertRow(settlePeer, 9002, 1.0)
			e.venue(9001, true, 0.5, 2000, 0.15)
			e.venue(9002, true, 0.2, 2002, 0.05)
			e.account(0.4)

			e.reconcile()
			d := e.reopen()
			if pos := d.state.Strategies[settlePeer].Positions["ETH"]; pos == nil || pos.Quantity != 0.2 {
				t.Fatalf("peer book = %+v, want its approved 0.2 fill booked", pos)
			}
			if d.trades[settlePeer] != 1 {
				t.Fatalf("peer trades = %d, want one", d.trades[settlePeer])
			}
			if pos := d.state.Strategies[settleOwner].Positions["ETH"]; pos != nil {
				t.Fatalf("the failed row must not re-create a position: %+v", pos)
			}
			bRow, _ := e.row(b.ID)
			if bRow.FilledSize != 0.2 || !bRow.fillSealed() {
				t.Fatalf("peer row = %+v, want sealed at 0.2", bRow)
			}
			aRow, ok := e.row(a.ID)
			if !ok || aRow.FilledSize != 0.3 || aRow.OperatorRequiredSince.IsZero() {
				t.Fatalf("failed row = %+v (present=%v), want kept at 0.3 with operator_required_since", aRow, ok)
			}
			if len(e.plans) != 1 || e.plans[0].Size != 0.2 {
				t.Fatalf("protection plans = %+v, want the peer's stop sized to its booked 0.2", e.plans)
			}
			marked := aRow.OperatorRequiredSince

			e.reconcile()
			if n := e.dmsContaining("Limit fill cannot be booked"); n != 1 {
				t.Fatalf("apply-failure alerts = %d, want one per throttle window", n)
			}
			d = e.reopen()
			if d.trades[settlePeer] != 1 {
				t.Fatalf("the peer fill was booked again: trades = %d", d.trades[settlePeer])
			}
			aRow, _ = e.row(a.ID)
			if !aRow.OperatorRequiredSince.Equal(marked) {
				t.Fatalf("the first operator-required time must be kept: %v -> %v", marked, aRow.OperatorRequiredSince)
			}
			if got := clearLimitRowTerminalRefusal(e.cfg, aRow); !strings.Contains(got, "can still fill") {
				t.Fatalf("the override must refuse while the order rests, got %q", got)
			}

			e.venue(9001, false, 0.5, 2000, 0.15)
			e.reconcile()
			aRow, ok = e.row(a.ID)
			if !ok || aRow.FilledSize != 0.3 {
				t.Fatalf("a terminal order must not delete the failed row automatically: %+v present=%v", aRow, ok)
			}
			if got := clearOperatorRequiredLimitRowRefusal(e.cfg, aRow, true); got != "" {
				t.Fatalf("manual-clear-limit-row must accept the marked row, got %q", got)
			}
			if got := clearLimitRowTerminalRefusal(e.cfg, aRow); got != "" {
				t.Fatalf("manual-clear-limit-row must accept the terminal order, got %q", got)
			}
			if err := e.store.FinalizePendingLimitOrder(limitRowFinalize{Row: aRow, OperatorSeal: true}); err != nil {
				t.Fatalf("operator clear: %v", err)
			}
			if _, ok := e.row(a.ID); ok {
				t.Fatal("the operator clear must delete the row")
			}
		})
	})

	t.Run("CommitFailureThenRetryBooksThePeerOnceAndReleasesTheHold", func(t *testing.T) {
		e := newLimitSettleEnv(t, false, true)
		a := e.sealedOwnerWithDeletedPosition()
		e.insertRow(settlePeer, 9002, 1.0)
		e.venue(9001, true, 0.5, 2000, 0.15)
		e.venue(9002, true, 0.2, 2002, 0.05)
		e.account(0.4)

		storeCommitHook = func(storageRole) error { return errors.New("injected crash before commit") }
		e.reconcile()
		if pos := e.state.Strategies[settlePeer].Positions["ETH"]; pos != nil {
			t.Fatalf("a failed commit published the peer fill: %+v", pos)
		}
		if !e.store.persistenceHoldsPartition(livePartition) {
			t.Fatal("the live partition must be held while the peer fill is unsaved")
		}
		e.store.recordLimitSettlementFailure([]int64{a.ID}, errors.New("earlier failed commit that included this row"))

		storeCommitHook = nil
		e.reconcile()
		d := e.reopen()
		if pos := d.state.Strategies[settlePeer].Positions["ETH"]; pos == nil || pos.Quantity != 0.2 || d.trades[settlePeer] != 1 {
			t.Fatalf("peer book = %+v trades = %d, want 0.2 booked once", pos, d.trades[settlePeer])
		}
		if e.store.persistenceHoldsPartition(livePartition) {
			t.Fatalf("the hold must release once the peer is saved and the failed row is marked: %q", e.store.persistenceHoldReason(livePartition))
		}
		e.reconcile()
		if d = e.reopen(); d.trades[settlePeer] != 1 {
			t.Fatalf("a later pass booked the peer again: trades = %d", d.trades[settlePeer])
		}
		if e.store.persistenceHoldsPartition(livePartition) {
			t.Fatal("the failed row must not hold the partition on later passes")
		}
	})
}

func TestManualLimitIncompleteEvidenceIntegration(t *testing.T) {
	incomplete := func(resting bool) HyperliquidLimitOrderStatus {
		st := HyperliquidLimitOrderStatus{OID: 9001, Coin: "ETH", OrderStatus: "open", Resting: limitTestBoolPtr(resting),
			FillsComplete: false, FillsError: "10000 fills since the attempt window reach the 10000 fills the venue retains"}
		if !resting {
			st.OrderStatus = "canceled"
			st.Terminal = true
		}
		return st
	}

	t.Run("CancelReachesTheVenueAndTheRowWaitsForCompleteEvidence", func(t *testing.T) {
		e := newLimitSettleEnv(t, false, false)
		row := e.insertRow(settleOwner, 9001, 1.0)
		if _, err := e.store.MarkPendingLimitOrderCancelRequested(settleOwner, "ETH"); err != nil {
			t.Fatal(err)
		}
		e.rawStatus(incomplete(true))
		e.reconcile()
		if len(e.cancels) != 1 || e.cancels[0] != 9001 {
			t.Fatalf("cancels = %v, want the queued cancel sent while fills are incomplete", e.cancels)
		}
		if _, ok := e.row(row.ID); !ok {
			t.Fatal("the row must stay while the evidence is incomplete")
		}
		e.rawStatus(incomplete(false))
		e.reconcile()
		if _, ok := e.row(row.ID); !ok {
			t.Fatal("an off-book order with incomplete fills must keep its row")
		}
		if len(e.cancels) != 1 {
			t.Fatalf("cancels = %v, want no cancel for an order that is not resting", e.cancels)
		}
		e.venue(9001, false, 0, 0, 0)
		e.reconcile()
		if _, ok := e.row(row.ID); ok {
			t.Fatal("complete off-book evidence must finalize the zero-fill row")
		}
	})

	t.Run("CancelReachesTheVenueOnTheReviewBranch", func(t *testing.T) {
		e := newLimitSettleEnv(t, false, false)
		row := e.insertRow(settleOwner, 9001, 1.0)
		if err := e.store.UpdatePendingLimitOrderFill(row.ID, 0.2, 2000, 0.1); err != nil {
			t.Fatal(err)
		}
		if _, err := e.store.MarkPendingLimitOrderCancelRequested(settleOwner, "ETH"); err != nil {
			t.Fatal(err)
		}
		e.rawStatus(incomplete(true))
		e.reconcile()
		if len(e.cancels) != 1 {
			t.Fatalf("cancels = %v, want the cancel sent for a review row with incomplete fills", e.cancels)
		}
		if _, ok := e.row(row.ID); !ok {
			t.Fatal("the review row must stay")
		}
	})

	t.Run("UnresolvedRowReachesTheOwnerAndTheOverride", func(t *testing.T) {
		e := newLimitSettleEnv(t, false, false)
		orig := limitUnresolvedPassThreshold
		limitUnresolvedPassThreshold = 2
		t.Cleanup(func() { limitUnresolvedPassThreshold = orig })
		row := e.insertRow(settleOwner, 9001, 1.0)
		e.rawStatus(incomplete(true))
		for pass := 0; pass < limitUnresolvedPassThreshold; pass++ {
			e.reconcile()
		}
		if r, _ := e.row(row.ID); !r.OperatorRequiredSince.IsZero() || e.dmsContaining("Limit order cannot be verified") != 0 {
			t.Fatalf("the row must not be classified at the threshold: %+v", r)
		}
		e.reconcile()
		first, _ := e.row(row.ID)
		if first.OperatorRequiredSince.IsZero() {
			t.Fatal("a row unresolved past the threshold must get operator_required_since")
		}
		for pass := 0; pass < 4; pass++ {
			e.reconcile()
		}
		later, _ := e.row(row.ID)
		if !later.OperatorRequiredSince.Equal(first.OperatorRequiredSince) {
			t.Fatalf("operator_required_since must be set once: %v -> %v", first.OperatorRequiredSince, later.OperatorRequiredSince)
		}
		if n := e.dmsContaining("Limit order cannot be verified"); n != 1 {
			t.Fatalf("unresolved alerts = %d, want exactly one per throttle window", n)
		}
		if len(e.cancels) != 0 {
			t.Fatalf("no cancel was queued, got %v", e.cancels)
		}

		if got := clearLimitRowTerminalRefusal(e.cfg, later); !strings.Contains(got, "can still fill") {
			t.Fatalf("the override must refuse a resting order, got %q", got)
		}
		e.rawStatus(incomplete(false))
		before := e.reopen()
		if got := clearOperatorRequiredLimitRowRefusal(e.cfg, later, true); got != "" {
			t.Fatalf("the override must accept the marked row, got %q", got)
		}
		if got := clearLimitRowTerminalRefusal(e.cfg, later); got != "" {
			t.Fatalf("a recognized terminal off-book state must pass without complete fills, got %q", got)
		}
		if err := e.store.FinalizePendingLimitOrder(limitRowFinalize{Row: later, OperatorSeal: true}); err != nil {
			t.Fatalf("operator clear: %v", err)
		}
		after := e.reopen()
		if len(after.rows) != 0 {
			t.Fatalf("the override must delete the row: %+v", after.rows)
		}
		for id, ss := range after.state.Strategies {
			was := before.state.Strategies[id]
			if ss.Cash != was.Cash || len(ss.Positions) != len(was.Positions) {
				t.Fatalf("book %s changed: cash %v -> %v positions %d -> %d", id, was.Cash, ss.Cash, len(was.Positions), len(ss.Positions))
			}
		}
		if after.trades[settleOwner] != before.trades[settleOwner] {
			t.Fatalf("the override booked a trade: %d -> %d", before.trades[settleOwner], after.trades[settleOwner])
		}
	})

	t.Run("ResolvedEvidenceClearsTheUnresolvedMarker", func(t *testing.T) {
		e := newLimitSettleEnv(t, false, false)
		orig := limitUnresolvedPassThreshold
		limitUnresolvedPassThreshold = 1
		t.Cleanup(func() { limitUnresolvedPassThreshold = orig })
		row := e.insertRow(settleOwner, 9001, 1.0)
		e.rawStatus(incomplete(true))
		e.reconcile()
		e.reconcile()
		if r, _ := e.row(row.ID); r.OperatorRequiredSince.IsZero() {
			t.Fatal("setup: the row must be marked")
		}
		e.venue(9001, true, 0, 0, 0)
		e.reconcile()
		if r, ok := e.row(row.ID); !ok || !r.OperatorRequiredSince.IsZero() {
			t.Fatalf("a resting order with complete evidence must drop the marker: %+v present=%v", r, ok)
		}
	})
}

func TestManualEntryHoldReasonIntegration(t *testing.T) {
	openErr := func(e *limitSettleEnv) string {
		t.Helper()
		sc := settleManualSC(settleOwner)
		d := manualCoreDeps{cfg: e.cfg, stateDB: e.store, loadState: func(strategyID, symbol string) (manualStateView, error) {
			e.mu.RLock()
			defer e.mu.RUnlock()
			return manualStateViewFromStateWithStore(e.cfg, e.state, e.store, strategyID, symbol), nil
		}}
		_, err := manualOpenCore(d, sc, manualOpenInputs{StrategyID: settleOwner, Side: "long", Size: 0.1, ATR: 40})
		if err == nil {
			t.Fatal("manual-open must refuse while the live partition is held")
		}
		return err.Error()
	}

	t.Run("UnsavedLimitFillNamesItsRow", func(t *testing.T) {
		e := newLimitSettleEnv(t, false, false)
		row := e.insertRow(settleOwner, 9001, 1.0)
		e.venue(9001, true, 0.4, 2000, 0.2)
		e.account(0.4)
		storeCommitHook = func(storageRole) error { return errors.New("injected crash before commit") }
		e.reconcile()
		storeCommitHook = nil
		got := openErr(e)
		want := "a confirmed limit fill on queue row " + strconv.FormatInt(row.ID, 10) + " is not saved yet"
		if !strings.Contains(got, want) || strings.Contains(got, "last state save") {
			t.Fatalf("refusal = %q, want it to name the unsaved limit fill and row %d", got, row.ID)
		}
	})

	t.Run("StartupNamesTheQueuePass", func(t *testing.T) {
		e := newLimitSettleEnv(t, false, false)
		restarted := newLimitSettleRestart(t, e)
		got := openErr(restarted)
		if !strings.Contains(got, "has not completed a full pass over the resting limit order queue") || strings.Contains(got, "last state save") {
			t.Fatalf("refusal = %q, want it to name the startup queue pass", got)
		}
	})
}

func TestManualLimitReviewAlertCancelAdviceIntegration(t *testing.T) {
	t.Run("RemovedOwnerIsNotToldToRunManualCancel", func(t *testing.T) {
		e := newLimitSettleEnv(t, false, true)
		row := e.insertRow(settlePeer, 9002, 1.0)
		e.state.Strategies[settlePeer].Positions["ETH"] = &Position{Symbol: "ETH", Side: "long", Quantity: 0.4, InitialQuantity: 0.4, AvgCost: 2000, Multiplier: 1, OwnerStrategyID: settlePeer}
		sealLimitRowFixture(t, e.store.primary(), e.state, row.ID, 0.4, 2000, 0.1)
		kept := e.cfg.Strategies[:0:0]
		for _, sc := range e.cfg.Strategies {
			if sc.ID != settlePeer {
				kept = append(kept, sc)
			}
		}
		e.cfg.Strategies = kept
		e.reconcile()
		if n := e.dmsContaining("needs review"); n != 1 {
			t.Fatalf("review alerts = %d, want one", n)
		}
		if n := e.dmsContaining("go-trader manual-cancel " + settlePeer); n != 0 {
			t.Fatalf("the alert recommends manual-cancel for a strategy the command cannot find: %+v", e.mock.dms)
		}
		if n := e.dmsContaining("Cancel order 9002 on the Hyperliquid UI"); n != 1 {
			t.Fatalf("the alert must give a working cancel step: %+v", e.mock.dms)
		}
	})

	t.Run("EligibleOwnerIsStillToldToRunManualCancel", func(t *testing.T) {
		e := newLimitSettleEnv(t, false, false)
		row := e.insertRow(settleOwner, 9001, 1.0)
		if err := e.store.UpdatePendingLimitOrderFill(row.ID, 0.2, 2000, 0.1); err != nil {
			t.Fatal(err)
		}
		e.reconcile()
		if n := e.dmsContaining("go-trader manual-cancel " + settleOwner); n != 1 {
			t.Fatalf("the alert must still recommend manual-cancel for an eligible owner: %+v", e.mock.dms)
		}
	})
}

func TestStandaloneEntryRefusesAnOwnerlessPositiveRow(t *testing.T) {
	e := newLimitSettleEnv(t, false, true)
	zero := e.insertRow(settleOwner, 9001, 1.0)
	sealed := e.insertRow(settlePeer, 9002, 1.0)
	e.state.Strategies[settlePeer].Positions["ETH"] = &Position{Symbol: "ETH", Side: "long", Quantity: 0.4, InitialQuantity: 0.4, AvgCost: 2000, Multiplier: 1, OwnerStrategyID: settlePeer}
	sealLimitRowFixture(t, e.store.primary(), e.state, sealed.ID, 0.4, 2000, 0.1)
	layout := e.store.layout
	if err := standaloneLimitQueueRefusal(layout, e.cfg.Strategies); err != nil {
		t.Fatalf("a sealed row with an eligible owner and a zero-fill row must pass: %v", err)
	}
	owner := []StrategyConfig{settleManualSC(settleOwner)}
	err := standaloneLimitQueueRefusal(layout, owner)
	if err == nil || !strings.Contains(err.Error(), "no configured Hyperliquid-live type=manual owner") || !strings.Contains(err.Error(), strconv.FormatInt(sealed.ID, 10)) {
		t.Fatalf("a sealed positive row whose strategy left the config must refuse a standalone live entry, got %v", err)
	}
	if err := e.store.FinalizePendingLimitOrder(limitRowFinalize{Row: func() PendingLimitOrder { r, _ := e.row(sealed.ID); return r }(), VenueFilled: 0.4, Eligible: true}); err != nil {
		t.Fatal(err)
	}
	if err := standaloneLimitQueueRefusal(layout, nil); err != nil {
		t.Fatalf("zero-fill row %d must still pass with no configured owner: %v", zero.ID, err)
	}
}

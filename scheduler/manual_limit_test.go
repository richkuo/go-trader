package main

import (
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestReconcilePendingLimitOrders(t *testing.T) {
	t.Run("ApplyLimitFillProgressGrow", func(t *testing.T) {
		sc, state := newLimitTestStrategy()
		origRecorder := tradeRecorder
		tradeRecorder = func(string, Trade) error { return nil }
		defer func() { tradeRecorder = origRecorder }()
		now := time.Now().UTC()

		o := PendingLimitOrder{ID: 1, StrategyID: sc.ID, Symbol: "ETH", Side: "long", OrderOID: 9001, LimitPrice: 2000, OrderSize: 1.0, FilledSize: 0}
		if _, err := applyLimitFillProgress(state, sc, o, 0.4, 2000, 0.2, 50, ATRMethodSimple, now); err != nil {
			t.Fatalf("first fill: %v", err)
		}
		o.FilledSize, o.AvgFillPrice, o.FillFee = 0.4, 2000, 0.2

		n, err := applyLimitFillProgress(state, sc, o, 1.0, 2010, 0.5, 50, ATRMethodSimple, now)
		if err != nil {
			t.Fatalf("grow: %v", err)
		}
		if n != 1 {
			t.Errorf("trades booked = %d, want 1", n)
		}
		pos := state.Strategies[sc.ID].Positions["ETH"]
		if pos.Quantity != 1.0 {
			t.Errorf("pos.Quantity = %g, want 1.0", pos.Quantity)
		}
		if pos.AvgCost != 2010 {
			t.Errorf("pos.AvgCost = %g, want 2010 (cumulative VWAP)", pos.AvgCost)
		}
		if pos.InitialQuantity != 1.0 {
			t.Errorf("pos.InitialQuantity = %g, want 1.0", pos.InitialQuantity)
		}
		if got := state.Strategies[sc.ID].Cash; got != 10000-0.5 {
			t.Errorf("cash = %g, want %g", got, 10000-0.5)
		}

		hist := state.Strategies[sc.ID].TradeHistory
		if len(hist) != 2 {
			t.Fatalf("expected 2 trade legs, got %d", len(hist))
		}
		if hist[0].TradeType != "perps" || hist[0].IsClose {
			t.Errorf("first leg should be an open perps trade, got type=%q is_close=%v", hist[0].TradeType, hist[0].IsClose)
		}
		if hist[1].TradeType != scaleInTradeType {
			t.Errorf("growth leg should be tagged %q (excluded from open-count), got %q", scaleInTradeType, hist[1].TradeType)
		}
		if hist[0].PositionID == "" || hist[0].PositionID != hist[1].PositionID {
			t.Errorf("legs must share position_id: %q vs %q", hist[0].PositionID, hist[1].PositionID)
		}
	})
	t.Run("FullFillFlushesPositionBeforeRowDelete", func(t *testing.T) {
		sc, state := newLimitTestStrategy()
		dbPath := filepath.Join(t.TempDir(), "state.db")
		db, err := OpenStateDB(dbPath)
		if err != nil {
			t.Fatalf("open db: %v", err)
		}
		t.Cleanup(func() { db.Close() })
		if err := db.SaveState(state); err != nil {
			t.Fatalf("seed save: %v", err)
		}
		cfg := &Config{DBFile: dbPath, Strategies: []StrategyConfig{sc}}
		var mu sync.RWMutex
		withStubbedHLLiveExposure(t, HLPosition{Coin: "ETH", Size: 0.5})

		db.InsertPendingLimitOrder(PendingLimitOrder{
			StrategyID: sc.ID, Symbol: "ETH", Side: "long", OrderOID: 9001,
			LimitPrice: 2000, OrderSize: 0.5, TIF: "Alo", EntryATR: 50, CreatedAt: time.Now().UTC(),
		})

		withStubbedLimitDeps(t,
			func(string, string, []int64, int64) (*HyperliquidLimitStatusResult, string, error) {
				return &HyperliquidLimitStatusResult{Orders: []HyperliquidLimitOrderStatus{
					provenLimitStatus(9001, "ETH", "filled", 0.5, 0.5, 2000, 0.7),
				}}, "", nil
			},
			func(string, string, int64) (*HyperliquidCancelOrderResult, string, error) {
				t.Error("cancel should not be called on a clean fill")
				return &HyperliquidCancelOrderResult{}, "", nil
			},
		)

		reconcilePendingLimitOrders(state, cfg, openTestStore(t, db), &mu, nil, nil)

		if orders, _ := db.LoadPendingLimitOrders(); len(orders) != 0 {
			t.Fatalf("terminal row not deleted: %+v", orders)
		}
		fresh, err := LoadStateWithDB(cfg, db)
		if err != nil {
			t.Fatalf("reload: %v", err)
		}
		ss := fresh.Strategies[sc.ID]
		if ss == nil || ss.Positions["ETH"] == nil {
			t.Fatalf("position not flushed to state.db before the terminal row was deleted: %+v", fresh.Strategies[sc.ID])
		}
		if got := ss.Positions["ETH"].Quantity; got != 0.5 {
			t.Fatalf("flushed position quantity = %g, want 0.5 (true adopted fill)", got)
		}
	})
	t.Run("PartialThenComplete", func(t *testing.T) {
		sc, state := newLimitTestStrategy()
		cfg := &Config{Strategies: []StrategyConfig{sc}}
		db := newLimitTestStateDB(t)
		var mu sync.RWMutex
		db.InsertPendingLimitOrder(PendingLimitOrder{
			StrategyID: sc.ID, Symbol: "ETH", Side: "long", OrderOID: 9001,
			LimitPrice: 2000, OrderSize: 1.0, TIF: "Alo", EntryATR: 50, CreatedAt: time.Now().UTC(),
		})

		withStubbedHLLiveExposure(t, HLPosition{Coin: "ETH", Size: 0.4})
		withStubbedLimitDeps(t,
			func(string, string, []int64, int64) (*HyperliquidLimitStatusResult, string, error) {
				return &HyperliquidLimitStatusResult{Orders: []HyperliquidLimitOrderStatus{
					provenLimitStatus(9001, "ETH", "live", 1.0, 0.4, 2000, 0.2),
				}}, "", nil
			},
			func(string, string, int64) (*HyperliquidCancelOrderResult, string, error) {
				return &HyperliquidCancelOrderResult{}, "", nil
			},
		)
		reconcilePendingLimitOrders(state, cfg, openTestStore(t, db), &mu, nil, nil)
		pos := state.Strategies[sc.ID].Positions["ETH"]
		if pos == nil || pos.Quantity != 0.4 {
			t.Fatalf("after partial: pos=%+v", pos)
		}
		if orders, _ := db.LoadPendingLimitOrders(); len(orders) != 1 || orders[0].FilledSize != 0.4 {
			t.Fatalf("watermark not persisted: %+v", orders)
		}

		withStubbedHLLiveExposure(t, HLPosition{Coin: "ETH", Size: 1.0})
		withStubbedLimitDeps(t,
			func(string, string, []int64, int64) (*HyperliquidLimitStatusResult, string, error) {
				return &HyperliquidLimitStatusResult{Orders: []HyperliquidLimitOrderStatus{
					provenLimitStatus(9001, "ETH", "filled", 1.0, 1.0, 2005, 0.5),
				}}, "", nil
			},
			func(string, string, int64) (*HyperliquidCancelOrderResult, string, error) {
				return &HyperliquidCancelOrderResult{}, "", nil
			},
		)
		reconcilePendingLimitOrders(state, cfg, openTestStore(t, db), &mu, nil, nil)
		pos = state.Strategies[sc.ID].Positions["ETH"]
		if pos.Quantity != 1.0 || pos.AvgCost != 2005 {
			t.Fatalf("after complete: pos=%+v", pos)
		}
		if orders, _ := db.LoadPendingLimitOrders(); len(orders) != 0 {
			t.Errorf("expected row deleted after full fill, got %d", len(orders))
		}
	})
	t.Run("CancelRequested", func(t *testing.T) {
		sc, state := newLimitTestStrategy()
		cfg := &Config{Strategies: []StrategyConfig{sc}}
		db := newLimitTestStateDB(t)
		var mu sync.RWMutex
		db.InsertPendingLimitOrder(PendingLimitOrder{
			StrategyID: sc.ID, Symbol: "ETH", Side: "long", OrderOID: 9001,
			LimitPrice: 2000, OrderSize: 0.5, TIF: "Alo", EntryATR: 50,
			CancelRequested: true, CreatedAt: time.Now().UTC(),
		})

		cancelCalls := 0
		withStubbedLimitDeps(t,
			func(string, string, []int64, int64) (*HyperliquidLimitStatusResult, string, error) {
				return &HyperliquidLimitStatusResult{Orders: []HyperliquidLimitOrderStatus{
					provenLimitStatus(9001, "ETH", "live", 0.5, 0, 0, 0),
				}}, "", nil
			},
			func(string, string, int64) (*HyperliquidCancelOrderResult, string, error) {
				cancelCalls++
				return &HyperliquidCancelOrderResult{OID: 9001, Cancelled: true}, "", nil
			},
		)
		reconcilePendingLimitOrders(state, cfg, openTestStore(t, db), &mu, nil, nil)
		if cancelCalls != 1 {
			t.Errorf("cancel calls = %d, want 1", cancelCalls)
		}
		if orders, _ := db.LoadPendingLimitOrders(); len(orders) != 1 {
			t.Errorf("row should be retained pending finalize, got %d", len(orders))
		}
		if pos := state.Strategies[sc.ID].Positions["ETH"]; pos != nil {
			t.Error("no position should exist for an unfilled cancelled order")
		}

		withStubbedLimitDeps(t,
			func(string, string, []int64, int64) (*HyperliquidLimitStatusResult, string, error) {
				return &HyperliquidLimitStatusResult{Orders: []HyperliquidLimitOrderStatus{
					provenLimitStatus(9001, "ETH", "cancelled", 0.5, 0, 0, 0),
				}}, "", nil
			},
			func(string, string, int64) (*HyperliquidCancelOrderResult, string, error) {
				return &HyperliquidCancelOrderResult{}, "", nil
			},
		)
		reconcilePendingLimitOrders(state, cfg, openTestStore(t, db), &mu, nil, nil)
		if orders, _ := db.LoadPendingLimitOrders(); len(orders) != 0 {
			t.Errorf("expected row deleted after cancel finalize, got %d", len(orders))
		}
	})
	t.Run("DeferOnUnknownBook", func(t *testing.T) {
		sc, state := newLimitTestStrategy()
		cfg := &Config{Strategies: []StrategyConfig{sc}}
		db := newLimitTestStateDB(t)
		var mu sync.RWMutex
		db.InsertPendingLimitOrder(PendingLimitOrder{
			StrategyID: sc.ID, Symbol: "ETH", Side: "long", OrderOID: 9001,
			LimitPrice: 2000, OrderSize: 0.5, TIF: "Alo", EntryATR: 50, CreatedAt: time.Now().UTC(),
		})
		withStubbedLimitDeps(t,
			func(string, string, []int64, int64) (*HyperliquidLimitStatusResult, string, error) {
				return &HyperliquidLimitStatusResult{OpenOrdersError: "boom", Orders: []HyperliquidLimitOrderStatus{
					limitStatusWithUnknownBook(provenLimitStatus(9001, "ETH", "cancelled", 0.5, 0, 0, 0)),
				}}, "", nil
			},
			func(string, string, int64) (*HyperliquidCancelOrderResult, string, error) {
				return &HyperliquidCancelOrderResult{}, "", nil
			},
		)
		reconcilePendingLimitOrders(state, cfg, openTestStore(t, db), &mu, nil, nil)
		if orders, _ := db.LoadPendingLimitOrders(); len(orders) != 1 {
			t.Errorf("row must be retained when book state is unknown, got %d", len(orders))
		}
	})
}

func withStubbedLimitOpen(t *testing.T, open func(_ hlWriteAuth, script, symbol, side string, size, limitPx float64, tif, marginMode string, leverage float64, snapshot hlExecuteSnapshot) (*HyperliquidLimitOpenResult, string, error)) {
	t.Helper()
	origOpen := runHyperliquidLimitOpenFn
	runHyperliquidLimitOpenFn = open
	t.Cleanup(func() { runHyperliquidLimitOpenFn = origOpen })
}

func newPartialLimitPositionHarness(t *testing.T) (*Config, StrategyConfig, *StateDB) {
	t.Helper()
	sc, state := newLimitTestStrategy()
	state.Strategies[sc.ID].Positions[sc.Symbol] = &Position{
		Symbol:          sc.Symbol,
		Quantity:        0.4,
		InitialQuantity: 0.4,
		AvgCost:         2000,
		EntryATR:        50,
		Side:            "long",
		Multiplier:      1,
		Leverage:        sc.Leverage,
		OwnerStrategyID: sc.ID,
		OpenedAt:        time.Now().UTC().Add(-time.Hour),
		TradePositionID: "pos-limit-partial",
	}
	dbPath := filepath.Join(t.TempDir(), "state.db")
	db, err := OpenStateDB(dbPath)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	if err := db.SaveState(state); err != nil {
		t.Fatalf("save state: %v", err)
	}
	rowID, err := db.InsertPendingLimitOrder(PendingLimitOrder{
		StrategyID: sc.ID, Symbol: sc.Symbol, Side: "long", OrderOID: 9001,
		LimitPrice: 1990, OrderSize: 1.0, TIF: "Alo", FilledSize: 0.4,
		AvgFillPrice: 2000, FillFee: 0.2, EntryATR: 50,
		CreatedAt: time.Now().UTC().Add(-10 * time.Minute),
	})
	if err != nil {
		t.Fatalf("insert pending limit: %v", err)
	}
	seedLimitFillLineage(t, db, rowID, sc.ID, sc.Symbol, 9001, 0.4, 2000, "pos-limit-partial")
	cfg := &Config{DBFile: dbPath, Strategies: []StrategyConfig{sc}}
	return cfg, sc, db
}

func TestManualCloseAndAddWithPendingLimit(t *testing.T) {
	t.Run("CancelPartialLimitRemainderBeforeActing", func(t *testing.T) {
		cases := []struct {
			name       string
			fill       HyperliquidFill
			act        func(deps manualCoreDeps, sc StrategyConfig) error
			wantAction string
			wantQty    float64
			stubMids   bool
		}{
			{
				name: "close cancels before flatten",
				fill: HyperliquidFill{AvgPx: 2010, TotalSz: 0.4, OID: 4242, Fee: 0.4},
				act: func(deps manualCoreDeps, sc StrategyConfig) error {
					_, err := manualCloseCore(deps, sc, manualCloseInputs{StrategyID: sc.ID})
					return err
				},
				wantAction: "close", wantQty: 0.4,
			},
			{
				name: "add cancels before averaging",
				fill: HyperliquidFill{AvgPx: 1995, TotalSz: 0.05, OID: 5252, Fee: 0.1},
				act: func(deps manualCoreDeps, sc StrategyConfig) error {
					_, err := manualAddCore(deps, sc, manualAddInputs{StrategyID: sc.ID, Margin: 10})
					return err
				},
				wantAction: "add", stubMids: true,
			},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				cfg, sc, db := newPartialLimitPositionHarness(t)
				cancelCalls := 0
				statusCalls := 0
				withStubbedLimitDeps(t,
					func(string, string, []int64, int64) (*HyperliquidLimitStatusResult, string, error) {
						statusCalls++
						return &HyperliquidLimitStatusResult{Orders: []HyperliquidLimitOrderStatus{
							provenLimitStatus(9001, "ETH", "cancelled", 1.0, 0.4, 2000, 0.2),
						}}, "", nil
					},
					func(string, string, int64) (*HyperliquidCancelOrderResult, string, error) {
						cancelCalls++
						return &HyperliquidCancelOrderResult{OID: 9001, Cancelled: true}, "", nil
					},
				)
				deps := newCLIManualCoreDeps(cfg, openTestStore(t, db), nil)
				if tc.stubMids {
					deps.fetchMids = func([]string) (map[string]float64, error) {
						return map[string]float64{sc.Symbol: 2000}, nil
					}
				}
				execCalls := 0
				fill := tc.fill
				deps.execute = func(hlWriteAuth, string, string, string, float64, float64, int64, float64, string, float64, hlCloseMode, hlExecuteSnapshot, ...int64) (*HyperliquidExecuteResult, string, error) {
					execCalls++
					return &HyperliquidExecuteResult{Execution: &HyperliquidExecution{Fill: &fill}}, "", nil
				}

				if err := tc.act(deps, sc); err != nil {
					t.Fatalf("manual %s with partial limit remainder: %v", tc.wantAction, err)
				}
				if cancelCalls != 1 || statusCalls != 1 || execCalls != 1 {
					t.Fatalf("calls cancel=%d status=%d exec=%d, want 1/1/1", cancelCalls, statusCalls, execCalls)
				}
				if orders, _ := db.LoadPendingLimitOrders(); len(orders) != 0 {
					t.Fatalf("pending limit rows = %+v, want deleted after proven cancelled", orders)
				}
				actions, _ := db.LoadPendingManualActions()
				if len(actions) != 1 || actions[0].Action != tc.wantAction || (tc.wantQty > 0 && actions[0].Quantity != tc.wantQty) {
					t.Fatalf("pending manual actions = %+v, want one %s for current position", actions, tc.wantAction)
				}
			})
		}
	})
	t.Run("CloseRefusesALegacyWatermarkInsteadOfEnlargingTheSnapshot", func(t *testing.T) {
		cfg, sc, db := newPartialLimitPositionHarness(t)
		orders, _ := db.LoadPendingLimitOrders()
		if len(orders) != 1 {
			t.Fatalf("want one resting row, got %d", len(orders))
		}
		if err := db.UpdatePendingLimitOrderFill(orders[0].ID, 0.7, 2005, 0.35); err != nil {
			t.Fatalf("advance watermark: %v", err)
		}
		if _, err := db.db.Exec("UPDATE pending_limit_orders SET fill_proof_version = 0 WHERE id = ?", orders[0].ID); err != nil {
			t.Fatalf("model a legacy watermark: %v", err)
		}
		seedLimitFillLineage(t, db, 0, sc.ID, sc.Symbol, 9001, 0.3, 2011.6666666666667, "pos-limit-partial")
		withStubbedLimitDeps(t,
			func(string, string, []int64, int64) (*HyperliquidLimitStatusResult, string, error) {
				return &HyperliquidLimitStatusResult{Orders: []HyperliquidLimitOrderStatus{
					provenLimitStatus(9001, "ETH", "cancelled", 1.0, 0.7, 2005, 0.35),
				}}, "", nil
			},
			func(string, string, int64) (*HyperliquidCancelOrderResult, string, error) {
				return &HyperliquidCancelOrderResult{OID: 9001, Cancelled: true}, "", nil
			},
		)
		deps := newCLIManualCoreDeps(cfg, openTestStore(t, db), nil)
		execCalls := 0
		deps.execute = func(hlWriteAuth, string, string, string, float64, float64, int64, float64, string, float64, hlCloseMode, hlExecuteSnapshot, ...int64) (*HyperliquidExecuteResult, string, error) {
			execCalls++
			return nil, "", errors.New("must not send")
		}

		_, err := manualCloseCore(deps, sc, manualCloseInputs{StrategyID: sc.ID})
		if err == nil || !strings.Contains(err.Error(), "carries no proof that it committed") {
			t.Fatalf("a legacy watermark must refuse the close, got %v", err)
		}
		if execCalls != 0 {
			t.Fatalf("the refused close must send nothing, exec calls = %d", execCalls)
		}
		if orders, _ := db.LoadPendingLimitOrders(); len(orders) != 1 || orders[0].FilledSize != 0.7 {
			t.Fatalf("the unproven row must be kept, got %+v", orders)
		}
		if actions, _ := db.LoadPendingManualActions(); len(actions) != 0 {
			t.Fatalf("the refused close must queue nothing, got %+v", actions)
		}
	})
	t.Run("CloseAfterABookedReductionUsesTheCommittedRemainder", func(t *testing.T) {
		reduced := func(t *testing.T) (*Config, StrategyConfig, *StateDB) {
			cfg, sc, db := newPartialLimitPositionHarness(t)
			st, err := LoadStateWithDB(cfg, db)
			if err != nil {
				t.Fatalf("load: %v", err)
			}
			p := st.Strategies[sc.ID].Positions[sc.Symbol]
			p.Quantity = 0.1
			if err := db.SaveState(st); err != nil {
				t.Fatalf("save the booked reduction: %v", err)
			}
			cfg.Strategies = append(cfg.Strategies, StrategyConfig{
				ID: "hl-manual-eth-peer", Type: "manual", Platform: "hyperliquid",
				Symbol: "ETH", Script: "shared_scripts/check_hyperliquid.py", Leverage: 10,
				Args: []string{"hold", "ETH", "30m", "--mode=live"},
			})
			withStubbedLimitDeps(t,
				func(string, string, []int64, int64) (*HyperliquidLimitStatusResult, string, error) {
					return &HyperliquidLimitStatusResult{Orders: []HyperliquidLimitOrderStatus{
						provenLimitStatus(9001, "ETH", "cancelled", 1.0, 0.4, 2000, 0.2),
					}}, "", nil
				},
				func(string, string, int64) (*HyperliquidCancelOrderResult, string, error) {
					return &HyperliquidCancelOrderResult{OID: 9001, Cancelled: true}, "", nil
				},
			)
			return cfg, sc, db
		}
		sizedDeps := func(cfg *Config, db *StateDB, got *float64, calls *int) manualCoreDeps {
			deps := newCLIManualCoreDeps(cfg, openTestStore(t, db), nil)
			deps.execute = func(_ hlWriteAuth, _ string, _ string, _ string, size float64, _ float64, _ int64, _ float64, _ string, _ float64, _ hlCloseMode, _ hlExecuteSnapshot, _ ...int64) (*HyperliquidExecuteResult, string, error) {
				*got = size
				*calls++
				return &HyperliquidExecuteResult{Execution: &HyperliquidExecution{Fill: &HyperliquidFill{AvgPx: 2010, TotalSz: size, OID: 4242, Fee: 0.01}}}, "", nil
			}
			return deps
		}

		cfg, sc, db := reduced(t)
		var gotQty float64
		calls := 0
		res, err := manualCloseCore(sizedDeps(cfg, db, &gotQty, &calls), sc, manualCloseInputs{StrategyID: sc.ID})
		if err != nil {
			t.Fatalf("manual close after the reduction: %v", err)
		}
		if calls != 1 || gotQty != 0.1 {
			t.Fatalf("the close must size from the committed 0.1 remainder, not the 0.4 recorded fill: size = %g, calls = %d", gotQty, calls)
		}
		if orders, _ := db.LoadPendingLimitOrders(); len(orders) != 0 {
			t.Fatalf("the proven terminal row must be cleared, got %+v", orders)
		}
		if actions, _ := db.LoadPendingManualActions(); len(actions) != 1 || actions[0].Quantity != 0.1 {
			t.Fatalf("queued close = %+v, want one close of the 0.1 remainder", actions)
		}
		if !strings.Contains(res.uiMessage(), "recorded fill 0.400000 of 1.000000") {
			t.Fatalf("the remainder notice must report the recorded fill, got %q", res.uiMessage())
		}

		cfg, sc, db = reduced(t)
		gotQty, calls = 0, 0
		if _, err := manualCloseCore(sizedDeps(cfg, db, &gotQty, &calls), sc, manualCloseInputs{StrategyID: sc.ID, Qty: 0.3}); err == nil || !strings.Contains(err.Error(), "exceeds open position 0.100000") {
			t.Fatalf("a quantity above the committed remainder must refuse, got %v", err)
		}
		if calls != 0 {
			t.Fatalf("the refused close must send nothing, calls = %d", calls)
		}
	})
	t.Run("CloseRereadsFreshPositionWhenRowDeletedBeforeClearResting", func(t *testing.T) {
		sc, deps, db := staleReadRowGoneCloseHarness(t)
		var gotCloseQty float64
		var gotFullClose bool
		deps.execute = func(_ hlWriteAuth, _ string, _ string, _ string, size float64, _ float64, _ int64, _ float64, _ string, _ float64, closeMode hlCloseMode, _ hlExecuteSnapshot, _ ...int64) (*HyperliquidExecuteResult, string, error) {
			gotCloseQty = size
			gotFullClose = closeMode == hlCloseModeWhole
			return &HyperliquidExecuteResult{Execution: &HyperliquidExecution{Fill: &HyperliquidFill{AvgPx: 2010, TotalSz: size, OID: 4242, Fee: 0.4}}}, "", nil
		}

		if _, err := manualCloseCore(deps, sc, manualCloseInputs{StrategyID: sc.ID}); err != nil {
			t.Fatalf("manual close: %v", err)
		}
		if gotFullClose {
			t.Fatalf("shared coin must take the sized-close path (closeFullPosition=false), got market_close")
		}
		if gotCloseQty != 0.7 {
			t.Fatalf("on-chain close size = %g, want 0.7 (fresh re-read, not stale 0.4 snapshot)", gotCloseQty)
		}
		actions, _ := db.LoadPendingManualActions()
		if len(actions) != 1 || !actions[0].IsFullClose || actions[0].Quantity != 0.7 {
			t.Fatalf("queued action = %+v, want one full close of 0.7", actions)
		}
	})
	t.Run("DeferWhenLimitCancelIsUnresolved", func(t *testing.T) {
		cases := []struct {
			name     string
			status   HyperliquidLimitStatusResult
			act      func(deps manualCoreDeps, sc StrategyConfig) error
			wantErr  string
			stubMids bool
		}{
			{
				name: "close defers on an unadopted fill",
				status: HyperliquidLimitStatusResult{Orders: []HyperliquidLimitOrderStatus{
					provenLimitStatus(9001, "ETH", "cancelled", 1.0, 0.6, 1998, 0.3),
				}},
				act: func(deps manualCoreDeps, sc StrategyConfig) error {
					_, err := manualCloseCore(deps, sc, manualCloseInputs{StrategyID: sc.ID})
					return err
				},
				wantErr: "unadopted fill",
			},
			{
				name: "add defers on an unknown book",
				status: HyperliquidLimitStatusResult{OpenOrdersError: "open orders unavailable", Orders: []HyperliquidLimitOrderStatus{
					limitStatusWithUnknownBook(provenLimitStatus(9001, "ETH", "cancelled", 1.0, 0.4, 2000, 0.2)),
				}},
				act: func(deps manualCoreDeps, sc StrategyConfig) error {
					_, err := manualAddCore(deps, sc, manualAddInputs{StrategyID: sc.ID, Margin: 10})
					return err
				},
				wantErr:  "open-orders state unknown",
				stubMids: true,
			},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				cfg, sc, db := newPartialLimitPositionHarness(t)
				status := tc.status
				withStubbedLimitDeps(t,
					func(string, string, []int64, int64) (*HyperliquidLimitStatusResult, string, error) {
						return &status, "", nil
					},
					func(string, string, int64) (*HyperliquidCancelOrderResult, string, error) {
						return &HyperliquidCancelOrderResult{OID: 9001, Cancelled: true}, "", nil
					},
				)
				deps := newCLIManualCoreDeps(cfg, openTestStore(t, db), nil)
				if tc.stubMids {
					deps.fetchMids = func([]string) (map[string]float64, error) {
						return map[string]float64{sc.Symbol: 2000}, nil
					}
				}
				deps.execute = func(hlWriteAuth, string, string, string, float64, float64, int64, float64, string, float64, hlCloseMode, hlExecuteSnapshot, ...int64) (*HyperliquidExecuteResult, string, error) {
					t.Error("execute must not run while the limit cancel is unresolved")
					return nil, "", errors.New("execute called")
				}

				err := tc.act(deps, sc)
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err = %v, want refusal containing %q", err, tc.wantErr)
				}
				orders, _ := db.LoadPendingLimitOrders()
				if len(orders) != 1 || !orders[0].CancelRequested {
					t.Fatalf("pending limit rows = %+v, want retained with cancel_requested", orders)
				}
				if actions, _ := db.LoadPendingManualActions(); len(actions) != 0 {
					t.Fatalf("pending manual actions = %+v, want none", actions)
				}
			})
		}
	})
}

func staleReadRowGoneCloseHarness(t *testing.T) (StrategyConfig, manualCoreDeps, *StateDB) {
	t.Helper()
	cfg, sc, db := newPartialLimitPositionHarness(t)
	orders, _ := db.LoadPendingLimitOrders()
	if len(orders) != 1 {
		t.Fatalf("want one resting row, got %d", len(orders))
	}
	if err := db.DeletePendingLimitOrder(orders[0].ID); err != nil {
		t.Fatalf("delete row: %v", err)
	}
	st, err := LoadStateWithDB(cfg, db)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	p := st.Strategies[sc.ID].Positions[sc.Symbol]
	p.Quantity, p.InitialQuantity, p.AvgCost = 0.7, 0.7, 2005
	if err := db.SaveState(st); err != nil {
		t.Fatalf("save grown position: %v", err)
	}
	cfg.Strategies = append(cfg.Strategies, StrategyConfig{
		ID: "hl-manual-eth-peer", Type: "manual", Platform: "hyperliquid",
		Symbol: "ETH", Script: "shared_scripts/check_hyperliquid.py", Leverage: 10,
		Args: []string{"hold", "ETH", "30m", "--mode=live"},
	})

	deps := newCLIManualCoreDeps(cfg, openTestStore(t, db), nil)
	realLoad := deps.loadState
	callN := 0
	deps.loadState = func(id, sym string) (manualStateView, error) {
		callN++
		if callN == 1 {
			return manualStateView{HasStrategy: true, Pos: &Position{
				Symbol: sc.Symbol, Quantity: 0.4, InitialQuantity: 0.4, AvgCost: 2000,
				EntryATR: 50, Side: "long", Multiplier: 1, Leverage: sc.Leverage,
				OwnerStrategyID: sc.ID, OpenedAt: time.Now().UTC().Add(-time.Hour),
				TradePositionID: "pos-limit-partial",
			}}, nil
		}
		return realLoad(id, sym)
	}
	return sc, deps, db
}

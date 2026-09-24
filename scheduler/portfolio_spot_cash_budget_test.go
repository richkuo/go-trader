package main

import (
	"io"
	"math"
	"path/filepath"
	"strings"
	"testing"
)

func TestExecuteSpotLiveBuyCashBudget(t *testing.T) {
	lm, err := NewLogManager("")
	if err != nil {
		t.Fatal(err)
	}
	logger, err := lm.GetStrategyLogger("test")
	if err != nil {
		t.Fatal(err)
	}
	defer logger.Close()

	newState := func(cash float64) *StrategyState {
		return &StrategyState{
			ID:              "rh-momentum-btc",
			Cash:            cash,
			Platform:        "robinhood",
			Positions:       make(map[string]*Position),
			OptionPositions: make(map[string]*OptionPosition),
			TradeHistory:    []Trade{},
			RiskState:       RiskState{},
		}
	}

	t.Run("within_budget", func(t *testing.T) {
		s := newState(1000)
		fillQty := 0.01
		fillPrice := 50000.0
		fillFee := 0.10
		exec, err := ExecuteSpotSignalWithFillFeeDeferredOpen(s, 1, "BTC", fillPrice, fillQty, fillFee, "oid-ok", 0, logger)
		if err != nil {
			t.Fatal(err)
		}
		if exec.TradesExecuted != 1 {
			t.Fatalf("trades = %d, want 1", exec.TradesExecuted)
		}
		if exec.CashReconcileRequired || s.CashReconcileRequired {
			t.Fatal("within-budget fill must not latch CashReconcileRequired")
		}
		if exec.CashOverBudgetAlert != "" {
			t.Fatalf("unexpected alert: %q", exec.CashOverBudgetAlert)
		}
		wantCash := 1000 - fillQty*fillPrice - fillFee
		if math.Abs(s.Cash-wantCash) > 1e-9 {
			t.Fatalf("cash = %g, want %g", s.Cash, wantCash)
		}
	})

	t.Run("tolerance_covered_overshoot", func(t *testing.T) {
		fillQty := 0.01
		fillPrice := 50000.0
		fillFee := 0.0
		cash := fillQty*fillPrice - spotLiveCashBudgetTolerance
		s := newState(cash)
		exec, err := ExecuteSpotSignalWithFillFeeDeferredOpen(s, 1, "BTC", fillPrice, fillQty, fillFee, "oid-tol", 0, logger)
		if err != nil {
			t.Fatal(err)
		}
		if exec.TradesExecuted != 1 {
			t.Fatalf("trades = %d, want 1 (must book)", exec.TradesExecuted)
		}
		if exec.CashReconcileRequired || s.CashReconcileRequired {
			t.Fatal("tolerance-covered overshoot must not latch reconcile")
		}
		if s.Positions["BTC"] == nil {
			t.Fatal("position must exist after tolerance-covered book")
		}
	})

	t.Run("clear_overshoot", func(t *testing.T) {
		s := newState(100)
		fillQty := 0.01
		fillPrice := 50000.0
		fillFee := 0.25
		exec, err := ExecuteSpotSignalWithFillFeeDeferredOpen(s, 1, "BTC", fillPrice, fillQty, fillFee, "oid-over", 0, logger)
		if err != nil {
			t.Fatal(err)
		}
		if exec.TradesExecuted != 1 {
			t.Fatalf("trades = %d, want 1 (must book over-budget fill)", exec.TradesExecuted)
		}
		if !exec.CashReconcileRequired || !s.CashReconcileRequired {
			t.Fatal("clear overshoot must latch CashReconcileRequired")
		}
		if exec.CashOverBudgetAlert == "" {
			t.Fatal("clear overshoot must produce CashOverBudgetAlert")
		}
		if !strings.Contains(exec.CashOverBudgetAlert, "CRITICAL: LIVE SPOT CASH OVER BUDGET") {
			t.Fatalf("alert missing CRITICAL headline: %q", exec.CashOverBudgetAlert)
		}
		wantCash := 100 - fillQty*fillPrice - fillFee
		if math.Abs(s.Cash-wantCash) > 1e-9 {
			t.Fatalf("cash = %g, want %g (negative is expected)", s.Cash, wantCash)
		}
		if s.Cash >= 0 {
			t.Fatalf("cash = %g, want negative after clear overshoot", s.Cash)
		}
		tr := s.TradeHistory
		if len(tr) != 0 {
		}
		if exec.OpenTrade == nil || !strings.Contains(exec.OpenTrade.Details, "CASH OVER BUDGET") {
			t.Fatalf("OpenTrade details should mark reconcile; got %#v", exec.OpenTrade)
		}
	})

	t.Run("live_fill_with_sub_dollar_cash", func(t *testing.T) {
		s := newState(0.50)
		fillQty := 0.002
		fillPrice := 50000.0
		fillFee := 0.0
		exec, err := ExecuteSpotSignalWithFillFeeDeferredOpen(s, 1, "BTC", fillPrice, fillQty, fillFee, "oid-sub", 0, logger)
		if err != nil {
			t.Fatal(err)
		}
		if exec.TradesExecuted != 1 {
			t.Fatalf("trades = %d, want 1 — live fill must book even when cash < $1", exec.TradesExecuted)
		}
		if s.Positions["BTC"] == nil {
			t.Fatal("position must exist after sub-dollar cash live fill")
		}
		if !exec.CashReconcileRequired || !s.CashReconcileRequired {
			t.Fatal("sub-dollar cash live fill that overshoots must latch reconcile")
		}
	})

	t.Run("paper_still_skips_sub_dollar_cash", func(t *testing.T) {
		s := newState(0.50)
		trades, err := ExecuteSpotSignalWithFillFee(s, 1, "BTC", 50000, 0, 0, "", 0, logger)
		if err != nil {
			t.Fatal(err)
		}
		if trades != 0 {
			t.Fatalf("paper trades = %d, want 0 when cash < $1", trades)
		}
		if s.CashReconcileRequired {
			t.Fatal("paper skip must not latch CashReconcileRequired")
		}
	})
}

func TestCashReconcileRequiredSaveLoadRoundTrip(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "state.db")
	db, err := OpenStateDB(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	state := &AppState{
		CycleCount: 1,
		Strategies: map[string]*StrategyState{
			"rh-btc": {
				ID: "rh-btc", Type: "spot", Platform: "robinhood",
				Cash: 0.005, InitialCapital: 1000,
				CashReconcileRequired: true,
				Positions:             make(map[string]*Position),
				OptionPositions:       make(map[string]*OptionPosition),
				TradeHistory:          []Trade{},
			},
			"okx-btc": {
				ID: "okx-btc", Type: "spot", Platform: "okx",
				Cash: -12.5, InitialCapital: 1000,
				CashReconcileRequired: true,
				Positions:             make(map[string]*Position),
				OptionPositions:       make(map[string]*OptionPosition),
				TradeHistory:          []Trade{},
			},
		},
	}
	if err := SaveStateWithDB(state, &Config{}, db); err != nil {
		t.Fatal(err)
	}

	loaded, err := db.LoadState()
	if err != nil {
		t.Fatal(err)
	}
	rh := loaded.Strategies["rh-btc"]
	if rh == nil {
		t.Fatal("rh-btc missing after LoadState")
	}
	if !rh.CashReconcileRequired {
		t.Fatal("CashReconcileRequired must survive Save/Load when cash is in [0, 0.01)")
	}
	if rh.Cash != 0.005 {
		t.Fatalf("cash = %g, want 0.005 (not clamped)", rh.Cash)
	}
	okx := loaded.Strategies["okx-btc"]
	if okx == nil || !okx.CashReconcileRequired {
		t.Fatal("negative-cash strategy must also keep CashReconcileRequired across Save/Load")
	}

	ValidateState(loaded, nil)
	if loaded.Strategies["rh-btc"].Cash != 0.005 {
	}
	if !loaded.Strategies["okx-btc"].CashReconcileRequired || loaded.Strategies["okx-btc"].Cash != 0 {
		t.Fatalf("okx after ValidateState: cash=%g latch=%v", loaded.Strategies["okx-btc"].Cash, loaded.Strategies["okx-btc"].CashReconcileRequired)
	}
	loaded.Strategies["rh-btc"].Cash = 0
	loaded.Strategies["rh-btc"].CashReconcileRequired = true
	if err := SaveStateWithDB(loaded, &Config{}, db); err != nil {
		t.Fatal(err)
	}
	loaded2, err := db.LoadState()
	if err != nil {
		t.Fatal(err)
	}
	if !loaded2.Strategies["rh-btc"].CashReconcileRequired {
		t.Fatal("second Save/Load with cash=0 must still restore CashReconcileRequired from SQLite")
	}
	ValidateState(loaded2, nil)
	if !loaded2.Strategies["rh-btc"].CashReconcileRequired {
		t.Fatal("ValidateState must not clear a persisted latch when cash is 0")
	}
}

func TestSellDoesNotClearCashReconcileWhenSolvent(t *testing.T) {
	lm, err := NewLogManager("")
	if err != nil {
		t.Fatal(err)
	}
	logger, err := lm.GetStrategyLogger("test")
	if err != nil {
		t.Fatal(err)
	}
	defer logger.Close()

	s := &StrategyState{
		ID:       "rh-btc",
		Cash:     0,
		Platform: "robinhood",
		Positions: map[string]*Position{
			"BTC": {Symbol: "BTC", Quantity: 0.01, InitialQuantity: 0.01, AvgCost: 50000, Side: "long", OwnerStrategyID: "rh-btc"},
		},
		OptionPositions:       make(map[string]*OptionPosition),
		TradeHistory:          []Trade{},
		CashReconcileRequired: true,
	}
	trades, err := ExecuteSpotSignalWithFillFee(s, -1, "BTC", 60000, 0.01, 0, "oid-close", 0, logger)
	if err != nil {
		t.Fatal(err)
	}
	if trades != 1 {
		t.Fatalf("trades = %d, want 1", trades)
	}
	if s.Cash < spotLiveCashBudgetTolerance {
		t.Fatalf("cash = %g, want solvent after sell", s.Cash)
	}
	if !s.CashReconcileRequired {
		t.Fatal("solvent sell must NOT clear CashReconcileRequired (#1400)")
	}
}

func TestRunRobinhoodExecuteOrder_CashReconcileGate(t *testing.T) {
	orig := robinhoodExecuteFn
	t.Cleanup(func() { robinhoodExecuteFn = orig })

	var calls []string
	robinhoodExecuteFn = func(script, symbol, side string, amountUSD, quantity float64) (*RobinhoodExecuteResult, string, error) {
		calls = append(calls, side)
		return &RobinhoodExecuteResult{
			Execution: &RobinhoodExecution{Action: side, Symbol: symbol, Fill: &RobinhoodFill{AvgPx: 100, Quantity: quantity}},
		}, "", nil
	}
	logger := &StrategyLogger{stratID: "rh-btc", writer: io.Discard}
	sc := StrategyConfig{ID: "rh-btc", Type: "spot", Platform: "robinhood", Script: "check_robinhood.py"}

	calls = nil
	er, ok := runRobinhoodExecuteOrder(sc, &RobinhoodResult{Symbol: "BTC", Signal: 1}, 100, 50, true, 0, "", HurstGateDecision{}, nil, logger)
	if ok || er != nil {
		t.Fatalf("latched buy: got ok=%v er=%v, want held", ok, er != nil)
	}
	if len(calls) != 0 {
		t.Fatalf("latched buy must not place order, calls=%v", calls)
	}

	calls = nil
	er, ok = runRobinhoodExecuteOrder(sc, &RobinhoodResult{Symbol: "BTC", Signal: -1}, 100, 0, true, 0.5, "long", HurstGateDecision{}, nil, logger)
	if !ok || er == nil {
		t.Fatalf("latched sell: got ok=%v er=%v, want proceed", ok, er != nil)
	}
	if len(calls) != 1 || calls[0] != "sell" {
		t.Fatalf("latched sell calls=%v, want [sell]", calls)
	}

	calls = nil
	er, ok = runRobinhoodExecuteOrder(sc, &RobinhoodResult{Symbol: "BTC", Signal: 1}, 100, 50, false, 0, "", HurstGateDecision{}, nil, logger)
	if !ok || er == nil {
		t.Fatalf("unlatched buy: got ok=%v er=%v, want proceed", ok, er != nil)
	}
	if len(calls) != 1 || calls[0] != "buy" {
		t.Fatalf("unlatched buy calls=%v, want [buy]", calls)
	}
}

func TestRunOKXExecuteOrder_CashReconcileGate(t *testing.T) {
	orig := okxExecuteFn
	t.Cleanup(func() { okxExecuteFn = orig })

	var calls []string
	okxExecuteFn = func(script, symbol, side string, size float64, instType string) (*OKXExecuteResult, string, error) {
		calls = append(calls, side)
		return &OKXExecuteResult{
			Execution: &OKXExecution{Action: side, Symbol: symbol, Size: size, Fill: &OKXFill{AvgPx: 100, TotalSz: size}},
		}, "", nil
	}
	logger := &StrategyLogger{stratID: "okx-btc", writer: io.Discard}
	sc := StrategyConfig{ID: "okx-btc", Type: "spot", Platform: "okx", Script: "check_okx.py"}

	calls = nil
	er, ok := runOKXExecuteOrder(sc, &OKXResult{Symbol: "BTC-USDT", Signal: 1, Price: 100}, 100, 50, false, true, 0, "", 0, 0, HurstGateDecision{}, nil, logger)
	if ok || er != nil {
		t.Fatalf("latched buy: got ok=%v er=%v, want held", ok, er != nil)
	}
	if len(calls) != 0 {
		t.Fatalf("latched buy must not place order, calls=%v", calls)
	}

	calls = nil
	er, ok = runOKXExecuteOrder(sc, &OKXResult{Symbol: "BTC-USDT", Signal: -1, Price: 100}, 100, 0, false, true, 0.5, "long", 100, 0, HurstGateDecision{}, nil, logger)
	if !ok || er == nil {
		t.Fatalf("latched sell: got ok=%v er=%v, want proceed", ok, er != nil)
	}
	if len(calls) != 1 || calls[0] != "sell" {
		t.Fatalf("latched sell calls=%v, want [sell]", calls)
	}

	calls = nil
	er, ok = runOKXExecuteOrder(sc, &OKXResult{Symbol: "BTC-USDT", Signal: 1, Price: 100}, 100, 50, false, false, 0, "", 0, 0, HurstGateDecision{}, nil, logger)
	if !ok || er == nil {
		t.Fatalf("unlatched buy: got ok=%v er=%v, want proceed", ok, er != nil)
	}
	if len(calls) != 1 || calls[0] != "buy" {
		t.Fatalf("unlatched buy calls=%v, want [buy]", calls)
	}
}

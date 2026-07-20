package main

import (
	"math"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestSpotLiveBuyExceedsCash(t *testing.T) {
	tol := spotLiveCashBudgetTolerance
	cases := []struct {
		name       string
		cash       float64
		totalDebit float64
		want       bool
	}{
		{"within_budget", 1000, 750, false},
		{"exact_cash", 100, 100, false},
		{"within_tolerance", 100, 100 + tol, false},
		{"just_over_tolerance", 100, 100 + tol + 1e-9, true},
		{"clear_overshoot", 100, 150, true},
		{"sub_dollar_cash_overshoot", 0.50, 50, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := spotLiveBuyExceedsCash(tc.cash, tc.totalDebit)
			if got != tc.want {
				t.Fatalf("spotLiveBuyExceedsCash(%g, %g) = %v, want %v", tc.cash, tc.totalDebit, got, tc.want)
			}
		})
	}
}

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
		// Debit exceeds cash by exactly the tolerance — booked, not latched.
		fillQty := 0.01
		fillPrice := 50000.0
		fillFee := 0.0 // robinhood modeled fee is 0
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
			// DeferredOpen does not RecordTrade; OpenTrade carries details.
		}
		if exec.OpenTrade == nil || !strings.Contains(exec.OpenTrade.Details, "CASH OVER BUDGET") {
			t.Fatalf("OpenTrade details should mark reconcile; got %#v", exec.OpenTrade)
		}
	})

	t.Run("live_fill_with_sub_dollar_cash", func(t *testing.T) {
		// Pre-fix: budget < $1 returned early and dropped the venue fill (#298).
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

func TestFormatSpotLiveCashOverBudgetAlert(t *testing.T) {
	msg := formatSpotLiveCashOverBudgetAlert("rh-btc", "BTC", 100, 500.25, -400.25, 0.01, 50000, 0.25)
	for _, want := range []string{
		"CRITICAL: LIVE SPOT CASH OVER BUDGET",
		"rh-btc",
		"BTC",
		"reconcile",
	} {
		if !strings.Contains(msg, want) {
			t.Fatalf("alert missing %q: %s", want, msg)
		}
	}
}

type recordingOwnerDM struct {
	msgs []string
}

func (r *recordingOwnerDM) SendOwnerDM(content string) {
	r.msgs = append(r.msgs, content)
}

func TestNotifySpotLiveCashOverBudget(t *testing.T) {
	rec := &recordingOwnerDM{}
	notifySpotLiveCashOverBudget(rec, "")
	if len(rec.msgs) != 0 {
		t.Fatalf("empty msg should no-op, got %v", rec.msgs)
	}
	notifySpotLiveCashOverBudget(nil, "alert")
	notifySpotLiveCashOverBudget(rec, "alert-body")
	if len(rec.msgs) != 1 || rec.msgs[0] != "alert-body" {
		t.Fatalf("msgs = %v, want [alert-body]", rec.msgs)
	}
}

func TestMaybeClearCashReconcileRequired(t *testing.T) {
	s := &StrategyState{Cash: 0, CashReconcileRequired: true}
	maybeClearCashReconcileRequired(s)
	if !s.CashReconcileRequired {
		t.Fatal("cash=0 must keep latch (ValidateState clamp must not auto-clear)")
	}
	s.Cash = spotLiveCashBudgetTolerance
	maybeClearCashReconcileRequired(s)
	if s.CashReconcileRequired {
		t.Fatal("solvent cash must clear latch")
	}
}

func TestValidateStateLatchesCashReconcileOnNegativeClamp(t *testing.T) {
	state := &AppState{Strategies: map[string]*StrategyState{
		"rh-btc": {ID: "rh-btc", Cash: -40.5, Positions: map[string]*Position{}},
	}}
	ValidateState(state)
	s := state.Strategies["rh-btc"]
	if s.Cash != 0 {
		t.Fatalf("cash = %g, want 0 after clamp", s.Cash)
	}
	if !s.CashReconcileRequired {
		t.Fatal("ValidateState must latch CashReconcileRequired when clamping negative cash")
	}
}

func TestCashReconcileBlocksLiveBuy(t *testing.T) {
	if !cashReconcileBlocksLiveBuy(true, true) {
		t.Fatal("latched buy must block")
	}
	if cashReconcileBlocksLiveBuy(true, false) {
		t.Fatal("latched sell/close must NOT block")
	}
	if cashReconcileBlocksLiveBuy(false, true) {
		t.Fatal("unlatched buy must not block")
	}
}

func TestFormatSpotLiveCashReconcileReminder(t *testing.T) {
	msg := formatSpotLiveCashReconcileReminder([]string{"a", "b"}, map[string]float64{"a": 0, "b": -1.5})
	for _, want := range []string{"CRITICAL: CASH RECONCILE STILL REQUIRED", "a", "b", "Further live spot buys are held"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("reminder missing %q: %s", want, msg)
		}
	}
}

func TestSpotCashReconcileReminderTracker(t *testing.T) {
	tr := &spotCashReconcileReminderTracker{}
	now := time.Date(2026, 7, 20, 12, 0, 0, 0, time.UTC)
	if !tr.ShouldNotify("a,b", now) {
		t.Fatal("first sighting must notify")
	}
	if tr.ShouldNotify("a,b", now.Add(time.Minute)) {
		t.Fatal("same sig inside throttle must not notify")
	}
	if !tr.ShouldNotify("a", now.Add(2*time.Minute)) {
		t.Fatal("sig change must notify")
	}
	if tr.ShouldNotify("", now.Add(3*time.Minute)) {
		t.Fatal("empty set must not notify")
	}
	if !tr.ShouldNotify("a", now.Add(3*time.Minute)) {
		t.Fatal("re-onset after clear must notify")
	}
}

func TestSpotCashReconcileReminderMarkNotifiedSuppressesSameCycle(t *testing.T) {
	tr := &spotCashReconcileReminderTracker{}
	now := time.Date(2026, 7, 20, 15, 0, 0, 0, time.UTC)
	// Per-fill CRITICAL seeds the tracker; same-cycle reminder must not fire.
	tr.MarkNotified("rh-btc", now)
	if tr.ShouldNotify("rh-btc", now.Add(time.Second)) {
		t.Fatal("MarkNotified must suppress same-cycle reminder for the founding sig")
	}
	// Re-onset after clear is still legitimate.
	tr.ShouldNotify("", now.Add(2*time.Second))
	if !tr.ShouldNotify("rh-btc", now.Add(3*time.Second)) {
		t.Fatal("re-onset after clear must still notify")
	}
}

func TestCashReconcileRequiredSaveLoadRoundTrip(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "state.db")
	db, err := OpenStateDB(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	// cash in [0, 0.01) — the range where ValidateState will NOT re-latch from
	// negative cash, so persistence (not clamp side-effect) must carry the flag.
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

	// Second restart simulation: clamp cash to 0 via ValidateState, save again,
	// reload — latch must still be true (persisted column, not negative-cash re-derive).
	ValidateState(loaded)
	if loaded.Strategies["rh-btc"].Cash != 0.005 {
		// 0.005 >= 0 so ValidateState does not clamp; okx clamps to 0 and keeps latch
	}
	if !loaded.Strategies["okx-btc"].CashReconcileRequired || loaded.Strategies["okx-btc"].Cash != 0 {
		t.Fatalf("okx after ValidateState: cash=%g latch=%v", loaded.Strategies["okx-btc"].Cash, loaded.Strategies["okx-btc"].CashReconcileRequired)
	}
	// Force rh into the post-clamp solvent-but-sub-tolerance zone and re-save.
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
	ValidateState(loaded2)
	if !loaded2.Strategies["rh-btc"].CashReconcileRequired {
		t.Fatal("ValidateState must not clear a persisted latch when cash is 0")
	}
}

func TestUIStrategyOverviewSurfacesCashReconcileRequired(t *testing.T) {
	state := &AppState{Strategies: map[string]*StrategyState{
		"rh-btc": {
			ID: "rh-btc", Type: "spot", Platform: "robinhood", Cash: 0,
			CashReconcileRequired: true,
			Positions:             make(map[string]*Position),
			OptionPositions:       make(map[string]*OptionPosition),
		},
		"okx-btc": {
			ID: "okx-btc", Type: "spot", Platform: "okx", Cash: 100,
			CashReconcileRequired: false,
			Positions:             make(map[string]*Position),
			OptionPositions:       make(map[string]*OptionPosition),
		},
	}}
	cfgs := []StrategyConfig{
		{ID: "rh-btc", Type: "spot", Platform: "robinhood", Args: []string{"momentum", "BTC"}},
		{ID: "okx-btc", Type: "spot", Platform: "okx", Args: []string{"momentum", "BTC"}},
	}
	ss := newOpsTestServer(t, cfgs, state, false)

	ov, _, ok := ss.uiStrategyOverview("rh-btc")
	if !ok {
		t.Fatal("rh-btc overview missing")
	}
	if !ov.CashReconcileRequired {
		t.Fatal("overview must surface CashReconcileRequired=true")
	}
	ov2, _, ok := ss.uiStrategyOverview("okx-btc")
	if !ok || ov2.CashReconcileRequired {
		t.Fatal("unlatched strategy must surface CashReconcileRequired=false/omitted")
	}
}

func TestCollectCashReconcileRequiredSnapshots(t *testing.T) {
	state := &AppState{Strategies: map[string]*StrategyState{
		"z": {CashReconcileRequired: true, Cash: 0.1},
		"a": {CashReconcileRequired: true, Cash: -2},
		"m": {CashReconcileRequired: false, Cash: 50},
	}}
	ids, cash := collectCashReconcileRequiredSnapshots(state)
	if len(ids) != 2 || ids[0] != "a" || ids[1] != "z" {
		t.Fatalf("ids = %v, want [a z]", ids)
	}
	if cash["a"] != -2 || cash["z"] != 0.1 {
		t.Fatalf("cash map = %v", cash)
	}
}

func TestSellClearsCashReconcileWhenSolvent(t *testing.T) {
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
	// Sell at a price that restores cash well above tolerance.
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
	if s.CashReconcileRequired {
		t.Fatal("solvent sell must clear CashReconcileRequired")
	}
}

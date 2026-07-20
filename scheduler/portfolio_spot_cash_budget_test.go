package main

import (
	"math"
	"strings"
	"testing"
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

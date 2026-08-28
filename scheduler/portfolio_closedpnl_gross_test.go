package main

import (
	"math"
	"testing"
)

func TestBookPerpsPartialCloseWithFillFee_NetOfFee(t *testing.T) {
	const (
		entryQty  = 0.428
		entryPx   = 2331.5
		tp1Qty    = 0.214
		tp1Px     = 2354.8
		tp1Fee    = 0.072565
		tp1NetPnL = 4.913635
		tp1Gross  = 4.9862
		tp2Qty    = 0.214
		tp2Px     = 2366.5
		tp2Fee    = 0.072926
		tp2NetPnL = 7.417074
		tp2Gross  = 7.49
	)

	s := &StrategyState{
		ID: "hl-manual-eth", Platform: "hyperliquid", Type: "manual",
		Cash: 1000,
		Positions: map[string]*Position{
			"ETH": {
				Symbol:          "ETH",
				Quantity:        entryQty,
				InitialQuantity: entryQty,
				AvgCost:         entryPx,
				Side:            "long",
			},
		},
	}

	if !bookPerpsPartialCloseWithFillFee(s, "ETH", tp1Qty, tp1Px, tp1Fee, true, "tp1-oid", "tp_partial_test", "TP1", "TP1", nil) {
		t.Fatal("TP1 booking returned false")
	}
	tp1Trade := s.TradeHistory[len(s.TradeHistory)-1]
	if !tp1Trade.PnLGross || math.Abs(tp1Trade.RealizedPnL-tp1Gross) > 1e-6 {
		t.Errorf("TP1 RealizedPnL = %.6f (gross=%v), want local gross %.4f", tp1Trade.RealizedPnL, tp1Trade.PnLGross, tp1Gross)
	}
	if math.Abs(tradeNetPnL(tp1Trade)-tp1NetPnL) > 1e-6 {
		t.Errorf("TP1 tradeNetPnL = %.6f, want %.6f (local fee-net, #698)", tradeNetPnL(tp1Trade), tp1NetPnL)
	}
	if math.Abs(tp1Trade.ExchangeFee-tp1Fee) > 1e-9 || tp1Trade.FeeSource != FeeSourceUserFills {
		t.Errorf("TP1 ExchangeFee = %.6f (src %q), want %.6f userfills", tp1Trade.ExchangeFee, tp1Trade.FeeSource, tp1Fee)
	}

	if !bookPerpsPartialCloseWithFillFee(s, "ETH", tp2Qty, tp2Px, tp2Fee, true, "tp2-oid", "tp_partial_test", "TP2", "TP2", nil) {
		t.Fatal("TP2 booking returned false")
	}
	tp2Trade := s.TradeHistory[len(s.TradeHistory)-1]
	if !tp2Trade.PnLGross || math.Abs(tp2Trade.RealizedPnL-tp2Gross) > 1e-6 {
		t.Errorf("TP2 RealizedPnL = %.6f (gross=%v), want local gross %.4f", tp2Trade.RealizedPnL, tp2Trade.PnLGross, tp2Gross)
	}
	if math.Abs(tradeNetPnL(tp2Trade)-tp2NetPnL) > 1e-6 {
		t.Errorf("TP2 tradeNetPnL = %.6f, want %.6f (local fee-net, #698)", tradeNetPnL(tp2Trade), tp2NetPnL)
	}

	if _, ok := s.Positions["ETH"]; ok {
		t.Errorf("position still open after both TPs filled; want flat")
	}
	if len(s.ClosedPositions) != 1 {
		t.Errorf("ClosedPositions = %d, want 1", len(s.ClosedPositions))
	}

	gotCashDelta := s.Cash - 1000
	wantCashDelta := tp1NetPnL + tp2NetPnL
	if math.Abs(gotCashDelta-wantCashDelta) > 1e-6 {
		t.Errorf("cash delta = %.6f, want %.6f (sum of fee-net PnL)", gotCashDelta, wantCashDelta)
	}
	if math.Abs(gotCashDelta-(tp1Gross+tp2Gross)) < 0.01 {
		t.Errorf("cash delta = %.6f matches sum of gross closedPnl %.4f — must be fee-net (#698)", gotCashDelta, tp1Gross+tp2Gross)
	}
}

func TestHLFillLookup_ClosedPnLGrossNotUsedForBooking(t *testing.T) {
	s := &StrategyState{
		ID: "hl-test", Platform: "hyperliquid", Type: "perps",
		Cash: 1000,
		Positions: map[string]*Position{
			"BTC": {
				Symbol:          "BTC",
				Quantity:        0.1,
				InitialQuantity: 0.1,
				AvgCost:         60000,
				Side:            "long",
			},
		},
	}

	lookup := HLFillLookup{
		Fee:            5.0,
		ClosedPnLGross: 9999.0,
		FilledQty:      0.05,
		Px:             61000,
		Count:          1,
		OID:            123,
	}
	if !bookPerpsPartialCloseWithFillFee(s, "BTC", lookup.FilledQty, lookup.Px, lookup.Fee, true, "123", "tp_partial_test", "TP", "TP", nil) {
		t.Fatal("booking returned false")
	}

	wantLocalGross := (lookup.Px - 60000) * lookup.FilledQty
	wantNet := wantLocalGross - lookup.Fee
	tr := s.TradeHistory[len(s.TradeHistory)-1]
	if math.Abs(tr.RealizedPnL-wantLocalGross) > 1e-9 {
		t.Errorf("RealizedPnL = %.6f, want %.6f (LOCAL geometric gross)", tr.RealizedPnL, wantLocalGross)
	}
	if math.Abs(tr.RealizedPnL-lookup.ClosedPnLGross) < 1.0 {
		t.Errorf("RealizedPnL = %.6f leaked from HLFillLookup.ClosedPnLGross %.2f (#698)", tr.RealizedPnL, lookup.ClosedPnLGross)
	}
	if math.Abs(tradeNetPnL(tr)-wantNet) > 1e-9 {
		t.Errorf("tradeNetPnL = %.6f, want %.6f (local fee-net)", tradeNetPnL(tr), wantNet)
	}
	if math.Abs(s.Cash-1000-wantNet) > 1e-9 {
		t.Errorf("cash delta = %.6f, want %.6f (fee-net)", s.Cash-1000, wantNet)
	}
}

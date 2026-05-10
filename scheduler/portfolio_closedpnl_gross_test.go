package main

import (
	"math"
	"testing"
)

// TestBookPerpsPartialCloseWithFillFee_NetOfFee locks in the contract that
// realized PnL is computed locally as `(closePx - avgCost) * qty - fillFee`
// (long) — i.e. NET of the real exchange fee. Hyperliquid's `userFills.closedPnl`
// is gross of fees (#698); any future refactor that wires HLFillLookup.ClosedPnLGross
// into Trade.RealizedPnL would overstate PnL by exactly the fee and fail this test.
//
// Numbers below are the live manual-eth example from #698:
//
//	entry: 0.428 ETH @ 2331.5
//	TP1:   0.214 ETH @ 2354.8, fee 0.072565 → net PnL 4.913635 (HL closedPnl was 4.9862)
//	TP2:   0.214 ETH @ 2366.5, fee 0.072926 → net PnL 7.417074 (HL closedPnl was 7.49)
func TestBookPerpsPartialCloseWithFillFee_NetOfFee(t *testing.T) {
	const (
		entryQty  = 0.428
		entryPx   = 2331.5
		tp1Qty    = 0.214
		tp1Px     = 2354.8
		tp1Fee    = 0.072565
		tp1NetPnL = 4.913635 // (2354.8 - 2331.5) * 0.214 - 0.072565
		tp1Gross  = 4.9862   // what HL userFills.closedPnl reports
		tp2Qty    = 0.214
		tp2Px     = 2366.5
		tp2Fee    = 0.072926
		tp2NetPnL = 7.417074 // (2366.5 - 2331.5) * 0.214 - 0.072926
		tp2Gross  = 7.49     // what HL userFills.closedPnl reports
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
	if math.Abs(tp1Trade.RealizedPnL-tp1NetPnL) > 1e-6 {
		t.Errorf("TP1 RealizedPnL = %.6f, want %.6f (local fee-net)", tp1Trade.RealizedPnL, tp1NetPnL)
	}
	if math.Abs(tp1Trade.RealizedPnL-tp1Gross) < 0.01 {
		t.Errorf("TP1 RealizedPnL = %.6f matches HL closedPnl gross %.4f — booking must use fee-net, not gross (#698)", tp1Trade.RealizedPnL, tp1Gross)
	}
	if math.Abs(tp1Trade.ExchangeFee-tp1Fee) > 1e-9 {
		t.Errorf("TP1 ExchangeFee = %.6f, want %.6f", tp1Trade.ExchangeFee, tp1Fee)
	}

	if !bookPerpsPartialCloseWithFillFee(s, "ETH", tp2Qty, tp2Px, tp2Fee, true, "tp2-oid", "tp_partial_test", "TP2", "TP2", nil) {
		t.Fatal("TP2 booking returned false")
	}
	tp2Trade := s.TradeHistory[len(s.TradeHistory)-1]
	if math.Abs(tp2Trade.RealizedPnL-tp2NetPnL) > 1e-6 {
		t.Errorf("TP2 RealizedPnL = %.6f, want %.6f (local fee-net)", tp2Trade.RealizedPnL, tp2NetPnL)
	}
	if math.Abs(tp2Trade.RealizedPnL-tp2Gross) < 0.01 {
		t.Errorf("TP2 RealizedPnL = %.6f matches HL closedPnl gross %.4f — booking must use fee-net, not gross (#698)", tp2Trade.RealizedPnL, tp2Gross)
	}

	// After both partial closes the position should be flat and recorded as closed.
	if _, ok := s.Positions["ETH"]; ok {
		t.Errorf("position still open after both TPs filled; want flat")
	}
	if len(s.ClosedPositions) != 1 {
		t.Errorf("ClosedPositions = %d, want 1", len(s.ClosedPositions))
	}

	// Cash should be entry + net PnL (entry cash unchanged at the open here since
	// we constructed the StrategyState mid-trade). Verify the sum credited to cash
	// equals the local fee-net total, not the gross total.
	gotCashDelta := s.Cash - 1000
	wantCashDelta := tp1NetPnL + tp2NetPnL
	if math.Abs(gotCashDelta-wantCashDelta) > 1e-6 {
		t.Errorf("cash delta = %.6f, want %.6f (sum of fee-net PnL)", gotCashDelta, wantCashDelta)
	}
	if math.Abs(gotCashDelta-(tp1Gross+tp2Gross)) < 0.01 {
		t.Errorf("cash delta = %.6f matches sum of gross closedPnl %.4f — must be fee-net (#698)", gotCashDelta, tp1Gross+tp2Gross)
	}
}

// TestHLFillLookup_ClosedPnLGrossNotUsedForBooking verifies the static contract
// that HLFillLookup.ClosedPnLGross is never read by bookPerpsPartialCloseWithFillFee.
// We pass a HLFillLookup-shaped payload with a clearly-wrong gross PnL and
// confirm the booked PnL ignores it entirely.
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

	// Simulate a reconciler hand-off: lookup reports a wildly inflated gross PnL.
	lookup := HLFillLookup{
		Fee:            5.0,
		ClosedPnLGross: 9999.0, // intentionally absurd; must NOT leak into Trade.RealizedPnL
		FilledQty:      0.05,
		Px:             61000,
		Count:          1,
		OID:            123,
	}
	if !bookPerpsPartialCloseWithFillFee(s, "BTC", lookup.FilledQty, lookup.Px, lookup.Fee, true, "123", "tp_partial_test", "TP", "TP", nil) {
		t.Fatal("booking returned false")
	}

	wantNet := (lookup.Px-60000)*lookup.FilledQty - lookup.Fee // 1000*0.05 - 5 = 45
	got := s.TradeHistory[len(s.TradeHistory)-1].RealizedPnL
	if math.Abs(got-wantNet) > 1e-9 {
		t.Errorf("RealizedPnL = %.6f, want %.6f (local fee-net)", got, wantNet)
	}
	if math.Abs(got-lookup.ClosedPnLGross) < 1.0 {
		t.Errorf("RealizedPnL = %.6f leaked from HLFillLookup.ClosedPnLGross %.2f (#698)", got, lookup.ClosedPnLGross)
	}
}

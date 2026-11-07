package main

import (
	"io"
	"math"
	"testing"
)

func TestCalculatePlatformFee(t *testing.T) {
	t.Run("Futures", func(t *testing.T) {
		sc := StrategyConfig{
			FuturesConfig: &FuturesConfig{FeePerContract: 1.50},
		}
		got := CalculatePlatformFuturesFee(sc, 3)
		if math.Abs(got-4.50) > 0.001 {
			t.Errorf("expected 4.50, got %.2f", got)
		}

		sc2 := StrategyConfig{}
		got2 := CalculatePlatformFuturesFee(sc2, 3)
		if got2 != 0 {
			t.Errorf("expected 0 with no FuturesConfig, got %.2f", got2)
		}
	})
}

func TestOrdinaryPerpsBookingKeepsVenueCostsAndStamp(t *testing.T) {
	logger := &StrategyLogger{stratID: "plain", writer: io.Discard}
	near := func(a, b float64) bool { return math.Abs(a-b) <= 1e-9*math.Max(1, math.Abs(b)) }
	for _, c := range []struct {
		platform string
		fee      float64
	}{{"hyperliquid", HyperliquidTakerFeePct}} {
		s := NewStrategyState(StrategyConfig{ID: "plain-" + c.platform, Platform: c.platform, Type: "perps", Capital: 1000})
		if _, err := ExecutePerpsSignalWithLeverage(s, 1, "BTC", 60000, PerpsSizing{SizingLeverage: 1}, 0, "", 0, DirectionLong, 0, logger); err != nil {
			t.Fatal(err)
		}
		pos := s.Positions["BTC"]
		px := ApplyAdverseSlippage(60000, true)
		if pos == nil || pos.AvgCost != px {
			t.Fatalf("%s: an ordinary open must keep the process slippage: %+v", c.platform, pos)
		}
		if _, err := ExecutePerpsSignalWithLeverage(s, -1, "BTC", 61000, PerpsSizing{SizingLeverage: 1}, 0, "", 0, DirectionLong, 0, logger); err != nil {
			t.Fatal(err)
		}
		for _, tr := range s.TradeHistory {
			if tr.CostModelVersion != FillCostModelVersion || tr.CostPolicyID != "" {
				t.Fatalf("%s: an ordinary row %+v must keep the process stamp", c.platform, tr)
			}
			if !near(tr.ExchangeFee, tr.Quantity*tr.Price*c.fee) {
				t.Fatalf("%s: an ordinary row %+v must keep the venue fee rate %v", c.platform, tr, c.fee)
			}
		}
	}
}

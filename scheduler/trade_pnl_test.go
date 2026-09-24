package main

import (
	"math"
	"testing"
	"time"
)

func TestTradeNetPnLSQL_MirrorsGoHelper(t *testing.T) {
	sdb := openTestDB(t)
	now := time.Now().UTC()
	trades := []Trade{
		{PositionID: "gross-close", TradeType: "perps", IsClose: true, PnLGross: true, RealizedPnL: 100, ExchangeFee: 0.7},
		{PositionID: "legacy-close", TradeType: "perps", IsClose: true, PnLGross: false, RealizedPnL: 99.3, ExchangeFee: 0.7},
		{PositionID: "gross-open", TradeType: "perps", IsClose: false, PnLGross: true, RealizedPnL: 0, ExchangeFee: 0.5},
		{PositionID: "legacy-open", TradeType: "perps", IsClose: false, PnLGross: false, RealizedPnL: 0, ExchangeFee: 0.5},
		{PositionID: "funding", TradeType: TradeTypeFunding, IsClose: false, PnLGross: true, RealizedPnL: 1.25, ExchangeFee: 0},
		{PositionID: "rebate", TradeType: "perps", IsClose: true, PnLGross: true, RealizedPnL: 10, ExchangeFee: -0.02},
		{PositionID: "gross-neg-close", TradeType: "perps", IsClose: true, PnLGross: true, RealizedPnL: -42.20, ExchangeFee: 0.08},
		{PositionID: "gross-zero-fee", TradeType: "perps", IsClose: true, PnLGross: true, RealizedPnL: 33, ExchangeFee: 0},
	}
	for i, tr := range trades {
		tr.StrategyID = "s1"
		tr.Timestamp = now.Add(time.Duration(i) * time.Second)
		tr.Symbol = "BTC"
		tr.Side = "sell"
		if err := sdb.InsertTrade(tr.StrategyID, tr); err != nil {
			t.Fatalf("InsertTrade %s: %v", tr.PositionID, err)
		}
	}

	for _, tr := range trades {
		var sqlNet, sqlDelta float64
		if err := sdb.db.QueryRow(
			`SELECT `+tradeNetPnLSQL+`, `+tradeLedgerDeltaSQL+` FROM trades WHERE position_id = ?`,
			tr.PositionID).Scan(&sqlNet, &sqlDelta); err != nil {
			t.Fatalf("query %s: %v", tr.PositionID, err)
		}
		if goNet := tradeNetPnL(tr); math.Abs(sqlNet-goNet) > 1e-9 {
			t.Errorf("%s: tradeNetPnLSQL = %v, tradeNetPnL = %v", tr.PositionID, sqlNet, goNet)
		}
		if goDelta := tradeLedgerDelta(tr); math.Abs(sqlDelta-goDelta) > 1e-9 {
			t.Errorf("%s: tradeLedgerDeltaSQL = %v, tradeLedgerDelta = %v", tr.PositionID, sqlDelta, goDelta)
		}
	}
}

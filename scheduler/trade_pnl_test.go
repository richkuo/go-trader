package main

import (
	"math"
	"testing"
	"time"
)

// TestLifetimeTradeStatsAll_GrossConventionRowsNetOfFee verifies W/L counting
// reads NET PnL through tradeNetPnLSQL on gross-convention (#954) rows: a
// small gross win whose fee exceeds it must count as a loss, not a win.
func TestLifetimeTradeStatsAll_GrossConventionRowsNetOfFee(t *testing.T) {
	sdb := openTestDB(t)
	now := time.Now().UTC()
	trades := []Trade{
		{StrategyID: "s1", Timestamp: now, Symbol: "BTC", PositionID: "p1", Side: "sell", Quantity: 0.1, Price: 51000, Value: 5100, TradeType: "perps", Details: "Close long", IsClose: true, PnLGross: true, RealizedPnL: 100, ExchangeFee: 0.7},
		{StrategyID: "s1", Timestamp: now.Add(time.Second), Symbol: "BTC", PositionID: "p2", Side: "sell", Quantity: 0.1, Price: 50000, Value: 5000, TradeType: "perps", Details: "Close long", IsClose: true, PnLGross: true, RealizedPnL: 0.05, ExchangeFee: 0.10},
	}
	for _, tr := range trades {
		if err := sdb.InsertTrade(tr.StrategyID, tr); err != nil {
			t.Fatalf("InsertTrade: %v", err)
		}
	}

	stats, err := sdb.LifetimeTradeStatsAll()
	if err != nil {
		t.Fatalf("LifetimeTradeStatsAll: %v", err)
	}
	if got := stats["s1"]; got.Wins != 1 || got.Losses != 1 {
		t.Errorf("s1 stats = %+v, want Wins=1 Losses=1 (raw realized_pnl summing would give Wins=2)", got)
	}
}

// TestTradeNetPnLSQL_MirrorsGoHelper verifies the SQL expressions
// tradeNetPnLSQL / tradeLedgerDeltaSQL agree with their Go mirrors
// tradeNetPnL / tradeLedgerDelta on every row convention.
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

func TestHasTradeWithExchangeOrderID(t *testing.T) {
	sdb := openTestDB(t)
	tr := Trade{StrategyID: "hl-a", Timestamp: time.Now().UTC(), Symbol: "BTC", Side: "buy", Quantity: 0.1, Price: 50000, Value: 5000, TradeType: "perps", PositionID: "p1", ExchangeOrderID: "OID1"}
	if err := sdb.InsertTrade(tr.StrategyID, tr); err != nil {
		t.Fatalf("InsertTrade: %v", err)
	}

	cases := []struct {
		strategyID, oid string
		want            bool
	}{
		{"hl-a", "OID1", true},
		{"hl-a", "OID2", false},
		{"hl-b", "OID1", false},
		{"hl-a", "", false},
	}
	for _, c := range cases {
		got, err := sdb.HasTradeWithExchangeOrderID(c.strategyID, c.oid)
		if err != nil {
			t.Fatalf("HasTradeWithExchangeOrderID(%q,%q): %v", c.strategyID, c.oid, err)
		}
		if got != c.want {
			t.Errorf("HasTradeWithExchangeOrderID(%q,%q) = %v, want %v", c.strategyID, c.oid, got, c.want)
		}
	}
}

func TestTradeBackfillRowNetPnL(t *testing.T) {
	cases := []struct {
		name string
		row  TradeBackfillRow
		want float64
	}{
		{"gross subtracts fee", TradeBackfillRow{PnLGross: true, RealizedPnL: 100, ExchangeFee: 0.7}, 99.3},
		{"legacy returns raw", TradeBackfillRow{PnLGross: false, RealizedPnL: 99.3, ExchangeFee: 0.7}, 99.3},
		{"gross zero fee", TradeBackfillRow{PnLGross: true, RealizedPnL: 33, ExchangeFee: 0}, 33},
		{"gross negative pnl", TradeBackfillRow{PnLGross: true, RealizedPnL: -42.20, ExchangeFee: 0.08}, -42.28},
	}
	for _, c := range cases {
		if got := tradeBackfillRowNetPnL(c.row); math.Abs(got-c.want) > 1e-9 {
			t.Errorf("%s: tradeBackfillRowNetPnL = %v, want %v", c.name, got, c.want)
		}
	}
}

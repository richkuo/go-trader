package main

import (
	"path/filepath"
	"testing"
	"time"
)

func ts(unixSec int64) time.Time {
	return time.Unix(unixSec, 0).UTC()
}

func TestPlanBackfillSkipsAlreadyRealFee(t *testing.T) {
	trades := []TradeBackfillRow{
		{RowID: 1, Timestamp: ts(100), Symbol: "ETH", Value: 1000,
			IsClose: false, ExchangeOrderID: "111", ExchangeFee: 0.32, RealizedPnL: 0},
	}
	fillMap := map[string]HLFillSummary{
		"111": {Fee: 0.40},
	}
	plan := planBackfillForStrategy("hl-eth", trades, fillMap, 1000.0, 999.68)
	if len(plan.TradeChanges) != 0 {
		t.Fatalf("expected 0 trade changes (already-real guard), got %d", len(plan.TradeChanges))
	}
	skipped := false
	for _, s := range plan.Skipped {
		if s.Reason == "already_real_fee" {
			skipped = true
		}
	}
	if !skipped {
		t.Fatalf("expected an already_real_fee skip entry, got %+v", plan.Skipped)
	}
}

func TestApplyBackfillPlanRoundTrip(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "state.db")
	db, err := OpenStateDB(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	openTs := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	closeTs := openTs.Add(2 * time.Hour)
	closeValue := 1010.0
	modeledCloseFee := closeValue * HyperliquidTakerFeePct

	state := &AppState{
		CycleCount: 1,
		Strategies: map[string]*StrategyState{
			"hl-eth-live": {
				ID: "hl-eth-live", Type: "perps", Platform: "hyperliquid",
				Cash: 999.65, InitialCapital: 1000,
				Positions: make(map[string]*Position), OptionPositions: make(map[string]*OptionPosition),
				ClosedPositions: []ClosedPosition{
					{StrategyID: "hl-eth-live", Symbol: "ETH", Quantity: 0.1, AvgCost: 10000,
						Side: "long", Multiplier: 1, OpenedAt: openTs, ClosedAt: closeTs,
						ClosePrice: 10100, RealizedPnL: 10.0 - modeledCloseFee, CloseReason: "manual_close"},
				},
			},
		},
	}
	if err := db.SaveState(state); err != nil {
		t.Fatal(err)
	}

	openTrade := Trade{
		Timestamp: openTs, StrategyID: "hl-eth-live", Symbol: "ETH",
		PositionID: "p1", Side: "buy", Quantity: 0.1, Price: 10000, Value: 1000,
		TradeType: "perps", Details: "open", ExchangeOrderID: "111", ExchangeFee: 0,
	}
	closeTrade := Trade{
		Timestamp: closeTs, StrategyID: "hl-eth-live", Symbol: "ETH",
		PositionID: "p1", Side: "sell", Quantity: 0.1, Price: 10100, Value: 1010,
		TradeType: "perps", Details: "close", ExchangeOrderID: "222", ExchangeFee: 0,
		IsClose: true, RealizedPnL: 10.0 - modeledCloseFee,
	}
	if err := db.InsertTrade("hl-eth-live", openTrade); err != nil {
		t.Fatal(err)
	}
	if err := db.InsertTrade("hl-eth-live", closeTrade); err != nil {
		t.Fatal(err)
	}

	realOpenFee := 0.40
	realCloseFee := 0.30
	fillMap := map[string]HLFillSummary{
		"111": {Fee: realOpenFee, Count: 1},
		"222": {Fee: realCloseFee, Count: 1},
	}

	trades, err := db.ListTradesForBackfill("hl-eth-live")
	if err != nil {
		t.Fatal(err)
	}
	if len(trades) != 2 {
		t.Fatalf("ListTradesForBackfill returned %d rows, want 2", len(trades))
	}
	closedRows, err := db.LoadClosedPositionRows("hl-eth-live")
	if err != nil {
		t.Fatal(err)
	}
	if len(closedRows) != 1 {
		t.Fatalf("LoadClosedPositionRows returned %d rows, want 1", len(closedRows))
	}

	plan := planBackfillForStrategy("hl-eth-live", trades, fillMap, 1000.0, 999.65)
	changeByRowID := make(map[int64]TradeChange, len(plan.TradeChanges))
	for _, c := range plan.TradeChanges {
		changeByRowID[c.RowID] = c
	}
	correctedTrades := make([]TradeBackfillRow, 0, len(trades))
	for _, tr := range trades {
		row := tr
		if c, ok := changeByRowID[tr.RowID]; ok {
			row.ExchangeFee = c.NewFee
			row.RealizedPnL = c.NewRealizedPnL
		}
		correctedTrades = append(correctedTrades, row)
	}
	plan.ClosedPositions = planClosedPositionRecomputes(correctedTrades, closedRows)

	if err := db.ApplyBackfillPlan(plan); err != nil {
		t.Fatalf("ApplyBackfillPlan failed: %v", err)
	}

	post, err := db.ListTradesForBackfill("hl-eth-live")
	if err != nil {
		t.Fatal(err)
	}
	for _, tr := range post {
		if tr.IsClose {
			if !approxEq(tr.ExchangeFee, realCloseFee) {
				t.Errorf("close exchange_fee=%v want %v", tr.ExchangeFee, realCloseFee)
			}
			expectedPnL := (10.0 - modeledCloseFee) + (modeledCloseFee - realCloseFee)
			if !approxEq(tr.RealizedPnL, expectedPnL) {
				t.Errorf("close realized_pnl=%v want %v", tr.RealizedPnL, expectedPnL)
			}
		} else {
			if !approxEq(tr.ExchangeFee, realOpenFee) {
				t.Errorf("open exchange_fee=%v want %v", tr.ExchangeFee, realOpenFee)
			}
		}
	}

	postCP, err := db.LoadClosedPositionRows("hl-eth-live")
	if err != nil {
		t.Fatal(err)
	}
	expectedCPPnL := (10.0 - modeledCloseFee) + (modeledCloseFee - realCloseFee)
	if !approxEq(postCP[0].RealizedPnL, expectedCPPnL) {
		t.Errorf("closed_positions realized_pnl=%v want %v", postCP[0].RealizedPnL, expectedCPPnL)
	}

	cfg := &Config{DBFile: dbPath}
	loaded, err := LoadStateWithDB(cfg, db)
	if err != nil {
		t.Fatal(err)
	}
	ss := loaded.Strategies["hl-eth-live"]
	if ss == nil {
		t.Fatal("strategy not loaded")
	}
	expectedCash := 1000.0 - realOpenFee + (10.0 - realCloseFee)
	if !approxEq(ss.Cash, expectedCash) {
		t.Errorf("strategies.cash=%v want %v", ss.Cash, expectedCash)
	}
}

func TestPlanClosedPositionRecomputesAggregatesPartialCloses(t *testing.T) {
	corrected := []TradeBackfillRow{
		{RowID: 1, Timestamp: ts(100), Symbol: "ETH", PositionID: "p1",
			IsClose: true, RealizedPnL: 5.0},
		{RowID: 2, Timestamp: ts(200), Symbol: "ETH", PositionID: "p1",
			IsClose: true, RealizedPnL: 4.6},
	}
	closedRows := []ClosedPositionRow{
		{ID: 11, Symbol: "ETH", ClosedAt: ts(200), RealizedPnL: 9.65},
	}
	out := planClosedPositionRecomputes(corrected, closedRows)
	if len(out) != 1 {
		t.Fatalf("expected 1 recompute, got %d", len(out))
	}
	if !approxEq(out[0].NewPnL, 9.6) {
		t.Fatalf("NewPnL=%v want 9.6 (sum of partial closes)", out[0].NewPnL)
	}
}

func TestPlanClosedPositionRecomputesRejectsAmbiguousFallback(t *testing.T) {
	corrected := []TradeBackfillRow{
		{RowID: 1, Timestamp: ts(101), Symbol: "ETH", PositionID: "pA",
			IsClose: true, RealizedPnL: 5.0},
		{RowID: 2, Timestamp: ts(103), Symbol: "ETH", PositionID: "pB",
			IsClose: true, RealizedPnL: 7.0},
	}
	closedRows := []ClosedPositionRow{
		{ID: 11, Symbol: "ETH", ClosedAt: ts(100), RealizedPnL: 4.9},
	}
	out := planClosedPositionRecomputes(corrected, closedRows)
	if len(out) != 0 {
		t.Fatalf("expected 0 recomputes (ambiguous match), got %d (%+v)", len(out), out)
	}
}

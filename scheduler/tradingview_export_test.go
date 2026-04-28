package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestTradingViewExportRowsSelectedStrategies(t *testing.T) {
	ts := time.Date(2026, 4, 28, 12, 34, 56, 0, time.UTC)
	strategies := []StrategyConfig{
		{ID: "okx-btc", Platform: "okx", Type: "spot"},
		{ID: "bus-eth", Platform: "binanceus", Type: "spot"},
	}
	trades := []Trade{
		{Timestamp: ts, StrategyID: "okx-btc", Symbol: "BTC/USDT", Side: "buy", Quantity: 0.25, Price: 60000, ExchangeFee: 1.5},
		{Timestamp: ts.Add(time.Minute), StrategyID: "bus-eth", Symbol: "ETH/USDT", Side: "sell", Quantity: 2, Price: 3200},
	}

	rows, err := buildTradingViewCSVRows(strategies, trades, nil, true)
	if err != nil {
		t.Fatalf("buildTradingViewCSVRows: %v", err)
	}
	want := [][]string{
		{"OKX:BTCUSDT", "buy", "0.25", "Filled", "60000", "1.5", "2026-04-28 12:34:56"},
		{"BINANCEUS:ETHUSDT", "sell", "2", "Filled", "3200", "0", "2026-04-28 12:35:56"},
	}
	if len(rows) != len(want) {
		t.Fatalf("rows len = %d, want %d", len(rows), len(want))
	}
	for i := range want {
		for j := range want[i] {
			if rows[i][j] != want[i][j] {
				t.Fatalf("rows[%d][%d] = %q, want %q (rows=%v)", i, j, rows[i][j], want[i][j], rows)
			}
		}
	}
}

func TestTradingViewExportRowsExplicitStrategyRequiresData(t *testing.T) {
	strategies := []StrategyConfig{
		{ID: "okx-btc", Platform: "okx", Type: "spot"},
		{ID: "bus-eth", Platform: "binanceus", Type: "spot"},
	}
	trades := []Trade{
		{Timestamp: time.Now().UTC(), StrategyID: "okx-btc", Symbol: "BTC/USDT", Side: "buy", Quantity: 1, Price: 60000},
	}

	_, err := buildTradingViewCSVRows(strategies, trades, nil, true)
	if err == nil || !strings.Contains(err.Error(), "bus-eth") {
		t.Fatalf("expected missing strategy data error for bus-eth, got %v", err)
	}
}

func TestTradingViewSymbolOverridesAndUnsupported(t *testing.T) {
	trade := Trade{StrategyID: "hl-btc", Symbol: "BTC"}
	symbol, err := tradingViewSymbol(
		StrategyConfig{ID: "hl-btc", Platform: "hyperliquid", Type: "perps"},
		trade,
		map[string]string{"hl:BTC": "BYBIT:BTCUSDT"},
	)
	if err != nil {
		t.Fatalf("tradingViewSymbol override: %v", err)
	}
	if symbol != "BYBIT:BTCUSDT" {
		t.Fatalf("symbol = %q, want BYBIT:BTCUSDT", symbol)
	}

	_, err = tradingViewSymbol(StrategyConfig{ID: "rh-aapl", Platform: "robinhood", Type: "spot"}, Trade{StrategyID: "rh-aapl", Symbol: "AAPL"}, nil)
	if err == nil || !strings.Contains(err.Error(), "symbol_overrides") {
		t.Fatalf("expected unsupported mapping error, got %v", err)
	}
}

func TestQueryTradingViewExportTradesChronological(t *testing.T) {
	db := openTestDB(t)
	t1 := time.Date(2026, 4, 28, 12, 0, 0, 0, time.UTC)
	t2 := t1.Add(time.Minute)
	if err := db.InsertTrade("s1", Trade{Timestamp: t2, Symbol: "ETH/USDT", Side: "buy", Quantity: 1, Price: 3000}); err != nil {
		t.Fatalf("InsertTrade t2: %v", err)
	}
	if err := db.InsertTrade("s1", Trade{Timestamp: t1, Symbol: "BTC/USDT", Side: "buy", Quantity: 1, Price: 60000}); err != nil {
		t.Fatalf("InsertTrade t1: %v", err)
	}
	if err := db.InsertTrade("s2", Trade{Timestamp: t1, Symbol: "SOL/USDT", Side: "buy", Quantity: 1, Price: 150}); err != nil {
		t.Fatalf("InsertTrade s2: %v", err)
	}

	trades, err := db.QueryTradingViewExportTrades([]string{"s1"})
	if err != nil {
		t.Fatalf("QueryTradingViewExportTrades: %v", err)
	}
	if len(trades) != 2 {
		t.Fatalf("trades len = %d, want 2", len(trades))
	}
	if trades[0].Symbol != "BTC/USDT" || trades[1].Symbol != "ETH/USDT" {
		t.Fatalf("trades not chronological or filtered: %+v", trades)
	}
}

func TestWriteTradingViewCSV(t *testing.T) {
	var buf bytes.Buffer
	rows := [][]string{{"OKX:BTCUSDT", "buy", "1", "Filled", "60000", "0", "2026-04-28 12:00:00"}}
	if err := writeTradingViewCSV(&buf, rows); err != nil {
		t.Fatalf("writeTradingViewCSV: %v", err)
	}
	got := buf.String()
	if !strings.HasPrefix(got, "Symbol,Side,Qty,Status,Fill Price,Commission,Closing Time\n") {
		t.Fatalf("missing TradingView header, got:\n%s", got)
	}
	if !strings.Contains(got, "OKX:BTCUSDT,buy,1,Filled,60000,0,2026-04-28 12:00:00\n") {
		t.Fatalf("missing row, got:\n%s", got)
	}
}

func TestExportTradingViewCSVFileEndToEnd(t *testing.T) {
	db := openTestDB(t)
	ts := time.Date(2026, 4, 28, 12, 0, 0, 0, time.UTC)
	if err := db.InsertTrade("okx-btc", Trade{Timestamp: ts, Symbol: "BTC/USDT", Side: "buy", Quantity: 1, Price: 60000}); err != nil {
		t.Fatalf("InsertTrade: %v", err)
	}
	out := filepath.Join(t.TempDir(), "tradingview.csv")
	cfg := &Config{
		Strategies: []StrategyConfig{{ID: "okx-btc", Platform: "okx", Type: "spot"}},
	}

	n, err := exportTradingViewCSVFile(db, cfg, tradingViewExportOptions{All: true, OutputPath: out})
	if err != nil {
		t.Fatalf("exportTradingViewCSVFile: %v", err)
	}
	if n != 1 {
		t.Fatalf("exported rows = %d, want 1", n)
	}
	data, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if !strings.Contains(string(data), "OKX:BTCUSDT,buy,1,Filled,60000,0,2026-04-28 12:00:00") {
		t.Fatalf("unexpected CSV:\n%s", data)
	}
}

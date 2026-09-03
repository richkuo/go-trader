package main

import (
	"bytes"
	"fmt"
	"math"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

func yesterday() string {
	return time.Now().UTC().AddDate(0, 0, -1).Format("2006-01-02")
}

func todayUTC() string {
	return time.Now().UTC().Format("2006-01-02")
}

func newRiskState(date string, dailyPnL float64) RiskState {
	return RiskState{
		DailyPnLDate: date,
		DailyPnL:     dailyPnL,
	}
}

func TestRolloverDailyPnL(t *testing.T) {
	cases := []struct {
		name     string
		date     string
		dailyPnL float64
		wantPnL  float64
	}{
		{"same day keeps accumulated pnl", todayUTC(), 123.45, 123.45},
		{"new day resets pnl", yesterday(), 99.99, 0},
		{"empty date resets pnl", "", 50.0, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := newRiskState(tc.date, tc.dailyPnL)
			rolloverDailyPnL(&r)
			if r.DailyPnL != tc.wantPnL {
				t.Errorf("DailyPnL = %.2f, want %.2f", r.DailyPnL, tc.wantPnL)
			}
			if r.DailyPnLDate != todayUTC() {
				t.Errorf("DailyPnLDate = %s, want %s", r.DailyPnLDate, todayUTC())
			}
		})
	}
}

func TestRecordTradeResult(t *testing.T) {
	cases := []struct {
		name    string
		date    string
		start   float64
		pnls    []float64
		wantPnL float64
	}{
		{"midnight crossing resets before booking", yesterday(), 200.0, []float64{50.0}, 50.0},
		{"same day accumulates", todayUTC(), 100.0, []float64{30.0, -10.0}, 120.0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := newRiskState(tc.date, tc.start)
			for _, pnl := range tc.pnls {
				RecordTradeResult(&r, pnl)
			}
			if r.DailyPnL != tc.wantPnL {
				t.Errorf("DailyPnL = %.2f, want %.2f", r.DailyPnL, tc.wantPnL)
			}
			if r.DailyPnLDate != todayUTC() {
				t.Errorf("DailyPnLDate = %s, want %s", r.DailyPnLDate, todayUTC())
			}
		})
	}
}

func TestCheckRisk_RollsOverDailyPnL(t *testing.T) {
	s := &StrategyState{
		RiskState:       newRiskState(yesterday(), 500.0),
		Positions:       make(map[string]*Position),
		OptionPositions: make(map[string]*OptionPosition),
	}
	s.RiskState.PeakValue = 1000.0
	s.RiskState.MaxDrawdownPct = 50.0

	CheckRisk(nil, s, 1000.0, nil, nil, nil)

	if s.RiskState.DailyPnL != 0 {
		t.Errorf("expected DailyPnL reset to 0 by CheckRisk; got %.2f", s.RiskState.DailyPnL)
	}
	if s.RiskState.DailyPnLDate != todayUTC() {
		t.Errorf("expected DailyPnLDate=%s; got %s", todayUTC(), s.RiskState.DailyPnLDate)
	}
}

func TestCheckRisk_ForceCloseOnDrawdown(t *testing.T) {
	s := &StrategyState{
		ID:   "test-strategy",
		Cash: 5000.0,
		RiskState: RiskState{
			PeakValue:      10000.0,
			MaxDrawdownPct: 20.0,
			DailyPnLDate:   todayUTC(),
		},
		InitialCapital: 10000.0,
		Positions: map[string]*Position{
			"BTC": {Symbol: "BTC", Quantity: 0.1, AvgCost: 50000.0, Side: "long"},
		},
		OptionPositions: map[string]*OptionPosition{
			"BTC-call-60000-2026-03-01": {
				ID:              "BTC-call-60000-2026-03-01",
				Action:          "buy",
				Quantity:        1,
				EntryPremiumUSD: 1000.0,
				CurrentValueUSD: 500.0,
			},
			"BTC-put-50000-2026-03-01": {
				ID:              "BTC-put-50000-2026-03-01",
				Action:          "sell",
				Quantity:        1,
				EntryPremiumUSD: 600.0,
				CurrentValueUSD: -800.0,
			},
		},
		TradeHistory: []Trade{},
	}

	prices := map[string]float64{"BTC": 30000.0}
	pv := PortfolioValue(s, prices)

	allowed, reason := CheckRisk(nil, s, pv, prices, nil, nil)

	if allowed {
		t.Error("expected CheckRisk to return false on drawdown breach")
	}
	if len(reason) == 0 {
		t.Error("expected non-empty reason")
	}

	if len(s.Positions) != 0 {
		t.Errorf("expected Positions empty after force-close; got %d entries", len(s.Positions))
	}
	if len(s.OptionPositions) != 0 {
		t.Errorf("expected OptionPositions empty after force-close; got %d entries", len(s.OptionPositions))
	}

	if len(s.TradeHistory) != 3 {
		t.Errorf("expected 3 trades in history; got %d", len(s.TradeHistory))
	}

	expectedCash := 7700.0
	if s.Cash != expectedCash {
		t.Errorf("expected Cash=%.2f after force-close; got %.2f", expectedCash, s.Cash)
	}
}

func TestCheckPortfolioRisk_DrawdownKillSwitch(t *testing.T) {
	cfg := &PortfolioRiskConfig{MaxDrawdownPct: 25, MaxNotionalUSD: 0, WarnThresholdPct: 80}
	prs := &PortfolioRiskState{PeakValue: 10000.0}

	allowed, nb, _, reason := CheckPortfolioRisk(prs, cfg, 7600.0, 0, 0, 0)
	if !allowed {
		t.Errorf("expected allowed below threshold; got reason=%s", reason)
	}
	if nb {
		t.Error("expected notionalBlocked=false")
	}

	if prs.PeakValue != 10000.0 {
		t.Errorf("expected peak=10000; got %.2f", prs.PeakValue)
	}

	allowed, nb, _, reason = CheckPortfolioRisk(prs, cfg, 7400.0, 0, 0, 0)
	if allowed {
		t.Error("expected kill switch to fire at 26% drawdown")
	}
	if nb {
		t.Error("expected notionalBlocked=false when kill switch fires")
	}
	if reason == "" {
		t.Error("expected non-empty reason")
	}
	if !prs.KillSwitchActive {
		t.Error("expected KillSwitchActive=true after firing")
	}
	if prs.KillSwitchAt.IsZero() {
		t.Error("expected KillSwitchAt to be set")
	}

	allowed, _, _, _ = CheckPortfolioRisk(prs, cfg, 10000.0, 0, 0, 0)
	if allowed {
		t.Error("expected kill switch to remain latched on subsequent call")
	}
}

func TestCheckPortfolioRisk_NotionalCap(t *testing.T) {
	cfg := &PortfolioRiskConfig{MaxDrawdownPct: 25, MaxNotionalUSD: 50000, WarnThresholdPct: 80}
	prs := &PortfolioRiskState{PeakValue: 10000.0}

	allowed, nb, _, _ := CheckPortfolioRisk(prs, cfg, 10000.0, 30000.0, 0, 0)
	if !allowed {
		t.Error("expected allowed under notional cap")
	}
	if nb {
		t.Error("expected notionalBlocked=false under cap")
	}

	allowed, nb, _, reason := CheckPortfolioRisk(prs, cfg, 10000.0, 60000.0, 0, 0)
	if !allowed {
		t.Error("expected allowed=true (notional cap doesn't kill switch)")
	}
	if !nb {
		t.Errorf("expected notionalBlocked=true over cap; reason=%s", reason)
	}
	if prs.KillSwitchActive {
		t.Error("expected kill switch NOT fired for notional cap breach")
	}
}

func TestCheckPortfolioRisk_PeakTracking(t *testing.T) {
	cfg := &PortfolioRiskConfig{MaxDrawdownPct: 50, MaxNotionalUSD: 0, WarnThresholdPct: 80}
	prs := &PortfolioRiskState{PeakValue: 5000.0}

	CheckPortfolioRisk(prs, cfg, 8000.0, 0, 0, 0)
	if prs.PeakValue != 8000.0 {
		t.Errorf("expected peak=8000 after rise; got %.2f", prs.PeakValue)
	}

	CheckPortfolioRisk(prs, cfg, 6000.0, 0, 0, 0)
	if prs.PeakValue != 8000.0 {
		t.Errorf("expected peak=8000 unchanged after drop; got %.2f", prs.PeakValue)
	}

	CheckPortfolioRisk(prs, cfg, 9000.0, 0, 0, 0)
	if prs.PeakValue != 9000.0 {
		t.Errorf("expected peak=9000 after new high; got %.2f", prs.PeakValue)
	}

	CheckPortfolioRisk(prs, cfg, 6000.0, 0, 0, 0)
	expectedDD := (9000.0 - 6000.0) / 9000.0 * 100
	if prs.CurrentDrawdownPct < expectedDD-0.01 || prs.CurrentDrawdownPct > expectedDD+0.01 {
		t.Errorf("expected drawdown≈%.2f%%; got %.2f%%", expectedDD, prs.CurrentDrawdownPct)
	}
}

func TestPortfolioNotional(t *testing.T) {
	cases := []struct {
		name       string
		strategies map[string]*StrategyState
		prices     map[string]float64
		want       float64
		frozen     float64
	}{
		{
			name: "spot plus options",
			strategies: map[string]*StrategyState{
				"spot-strat": {
					Positions: map[string]*Position{
						"BTC": {Symbol: "BTC", Quantity: 0.5, AvgCost: 40000.0, Side: "long"},
						"ETH": {Symbol: "ETH", Quantity: 10.0, AvgCost: 3000.0, Side: "long"},
					},
					OptionPositions: make(map[string]*OptionPosition),
				},
				"options-strat": {
					Positions: make(map[string]*Position),
					OptionPositions: map[string]*OptionPosition{
						"BTC-put-40000-sell": {Action: "sell", Strike: 40000.0, Quantity: 2.0, CurrentValueUSD: -500.0},
						"BTC-call-50000-buy": {Action: "buy", Strike: 50000.0, Quantity: 1.0, CurrentValueUSD: 800.0},
					},
				},
			},
			prices: map[string]float64{"BTC": 50000.0, "ETH": 3500.0},
			want:   140800.0,
		},
		{
			name: "includes perps at live mark",
			strategies: map[string]*StrategyState{
				"hl-momentum-btc": {
					Type:            "perps",
					Positions:       map[string]*Position{"BTC": {Symbol: "BTC", Quantity: 0.4, AvgCost: 40000.0, Side: "long"}},
					OptionPositions: make(map[string]*OptionPosition),
				},
				"spot-btc": {
					Type:            "spot",
					Positions:       map[string]*Position{"BTC/USDT": {Symbol: "BTC/USDT", Quantity: 0.1, AvgCost: 45000.0, Side: "long"}},
					OptionPositions: make(map[string]*OptionPosition),
				},
			},
			prices: map[string]float64{"BTC/USDT": 50000.0, "BTC": 50000.0},
			want:   25000.0,
		},
		{
			name: "includes futures at live mark with multiplier",
			strategies: map[string]*StrategyState{
				"ts-trend-es": {
					Type:            "futures",
					Positions:       map[string]*Position{"ES": {Symbol: "ES", Quantity: 2, AvgCost: 5000.0, Side: "long", Multiplier: 50}},
					OptionPositions: make(map[string]*OptionPosition),
				},
				"ts-mr-nq": {
					Type:            "futures",
					Positions:       map[string]*Position{"NQ": {Symbol: "NQ", Quantity: 1, AvgCost: 18000.0, Side: "short", Multiplier: 20}},
					OptionPositions: make(map[string]*OptionPosition),
				},
			},
			prices: map[string]float64{"ES": 5100.0, "NQ": 18500.0},
			want:   880000.0,
			frozen: 860000.0,
		},
		{
			name: "futures mark miss falls back to entry",
			strategies: map[string]*StrategyState{
				"ts-trend-cl": {
					Type:            "futures",
					Positions:       map[string]*Position{"CL": {Symbol: "CL", Quantity: 1, AvgCost: 80.0, Side: "long", Multiplier: 1000}},
					OptionPositions: make(map[string]*OptionPosition),
				},
			},
			prices: map[string]float64{},
			want:   80000.0,
		},
		{
			name: "includes perps short at live mark",
			strategies: map[string]*StrategyState{
				"hl-mean-rev-eth": {
					Type:            "perps",
					Positions:       map[string]*Position{"ETH": {Symbol: "ETH", Quantity: 2.0, AvgCost: 3000.0, Side: "short"}},
					OptionPositions: make(map[string]*OptionPosition),
				},
			},
			prices: map[string]float64{"ETH/USDT": 3200.0, "ETH": 3200.0},
			want:   6400.0,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			notional := PortfolioNotional(tc.strategies, tc.prices)
			if math.Abs(notional-tc.want) > 0.01 {
				t.Errorf("expected notional=%.2f; got %.2f", tc.want, notional)
			}
			if tc.frozen != 0 && notional == tc.frozen {
				t.Errorf("notional equals frozen-entry value %.2f: mark price was not applied", tc.frozen)
			}
		})
	}
}

func TestCollectFuturesMarkSymbols(t *testing.T) {
	cases := []struct {
		name       string
		strategies []StrategyConfig
		want       []string
	}{
		{
			name: "topstep futures only, deduplicated and sorted",
			strategies: []StrategyConfig{
				{ID: "ts-trend-es", Type: "futures", Platform: "topstep", Args: []string{"trend", "ES", "1h"}},
				{ID: "ts-mr-es", Type: "futures", Platform: "topstep", Args: []string{"mean_rev", "ES", "15m"}},
				{ID: "ts-trend-nq", Type: "futures", Platform: "topstep", Args: []string{"trend", "NQ", "1h"}},
				{ID: "ts-trend-mes", Type: "futures", Platform: "topstep", Args: []string{"trend", "MES", "1h"}},
				{ID: "sma-btc", Type: "spot", Platform: "binanceus", Args: []string{"sma", "BTC/USDT", "1h"}},
				{ID: "hl-eth", Type: "perps", Platform: "hyperliquid", Args: []string{"momentum", "ETH", "1h"}},
				{ID: "deribit-vol-btc", Type: "options", Platform: "deribit", Args: []string{"vol", "BTC"}},
				{ID: "ts-short", Type: "futures", Platform: "topstep", Args: []string{"trend"}},
				{ID: "ts-empty-sym", Type: "futures", Platform: "topstep", Args: []string{"trend", "", "1h"}},
				{ID: "ibkr-trend-cl", Type: "futures", Platform: "ibkr", Args: []string{"trend", "CL", "1h"}},
			},
			want: []string{"ES", "MES", "NQ"},
		},
		{
			name: "ignores manual strategies",
			strategies: []StrategyConfig{
				{ID: "manual-hl-eth", Type: "manual", Platform: "hyperliquid", Symbol: "ETH",
					Args: []string{"hold", "ETH", "1h", "--mode=live"}},
				{ID: "ts-trend-es", Type: "futures", Platform: "topstep", Args: []string{"trend", "ES", "1h"}},
			},
			want: []string{"ES"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := collectFuturesMarkSymbols(tc.strategies)
			if len(got) != len(tc.want) {
				t.Fatalf("got %d symbols %v, want %d %v", len(got), got, len(tc.want), tc.want)
			}
			for i, sym := range tc.want {
				if got[i] != sym {
					t.Errorf("got[%d]=%q, want %q (full: %v)", i, got[i], sym, got)
				}
			}
		})
	}
}

func TestMergeFuturesMarks(t *testing.T) {
	prices := map[string]float64{
		"BTC/USDT": 50000.0,
		"ES":       5120.5,
	}
	marks := map[string]float64{
		"ES":  5100.0,
		"NQ":  18500.0,
		"MES": 0.0,
		"CL":  -1,
	}

	mergeFuturesMarks(prices, marks)

	if prices["BTC/USDT"] != 50000.0 {
		t.Errorf("prices[BTC/USDT] = %v, want 50000 (unrelated entry mutated)", prices["BTC/USDT"])
	}
	if prices["ES"] != 5120.5 {
		t.Errorf("prices[ES] = %v, want 5120.5 (existing live mark must win)", prices["ES"])
	}
	if prices["NQ"] != 18500.0 {
		t.Errorf("prices[NQ] = %v, want 18500 (new mark must be merged)", prices["NQ"])
	}
	if _, ok := prices["MES"]; ok {
		t.Errorf("prices[MES] should not be set when mark is zero (got %v)", prices["MES"])
	}
	if _, ok := prices["CL"]; ok {
		t.Errorf("prices[CL] should not be set when mark is negative (got %v)", prices["CL"])
	}
}

func TestCollectPriceSymbols(t *testing.T) {
	strategies := []StrategyConfig{
		{ID: "sma-btc", Type: "spot", Platform: "binanceus", Args: []string{"sma", "BTC/USDT", "1h"}},
		{ID: "sma-eth", Type: "spot", Platform: "binanceus", Args: []string{"sma", "ETH/USDT", "1h"}},
		{ID: "hl-momentum-btc", Type: "perps", Platform: "hyperliquid", Args: []string{"momentum", "BTC", "1h"}},
		{ID: "okx-ema-sol-perp", Type: "perps", Platform: "okx", Args: []string{"ema", "SOL", "1h"}},
		{ID: "deribit-vol-btc", Type: "options", Platform: "deribit", Args: []string{"vol", "BTC"}},
		{ID: "short", Type: "spot", Args: []string{"sma"}},
	}

	symbols := collectPriceSymbols(strategies)

	got := make(map[string]bool, len(symbols))
	for _, s := range symbols {
		got[s] = true
	}

	wantSymbols := []string{"BTC/USDT", "ETH/USDT"}
	for _, sym := range wantSymbols {
		if !got[sym] {
			t.Errorf("symbols missing %q; got %v", sym, symbols)
		}
	}
	if len(symbols) != len(wantSymbols) {
		t.Errorf("symbols len = %d (%v), want %d (%v)", len(symbols), symbols, len(wantSymbols), wantSymbols)
	}

	for _, notWanted := range []string{"BTC", "SOL", "BTC/USDT:USDT", "SOL/USDT"} {
		if got[notWanted] {
			t.Errorf("symbol %q should not be in the BinanceUS fetch list (perps now venue-native)", notWanted)
		}
	}
}

func TestCollectPerpsMarkSymbols(t *testing.T) {
	cases := []struct {
		name       string
		strategies []StrategyConfig
		wantHL     []string
		wantOKX    []string
	}{
		{
			name: "perps split by venue, deduplicated and sorted",
			strategies: []StrategyConfig{
				{ID: "hl-momentum-btc", Type: "perps", Platform: "hyperliquid", Args: []string{"momentum", "BTC", "1h"}},
				{ID: "hl-mr-btc", Type: "perps", Platform: "hyperliquid", Args: []string{"mean_rev", "BTC", "15m"}},
				{ID: "hl-trend-eth", Type: "perps", Platform: "hyperliquid", Args: []string{"trend", "ETH", "1h"}},
				{ID: "okx-ema-sol-perp", Type: "perps", Platform: "okx", Args: []string{"ema", "SOL", "1h"}},
				{ID: "okx-ema-btc-perp", Type: "perps", Platform: "okx", Args: []string{"ema", "BTC", "1h"}},
				{ID: "sma-btc", Type: "spot", Platform: "binanceus", Args: []string{"sma", "BTC/USDT", "1h"}},
				{ID: "deribit-vol-btc", Type: "options", Platform: "deribit", Args: []string{"vol", "BTC"}},
				{ID: "ts-trend-es", Type: "futures", Platform: "topstep", Args: []string{"trend", "ES", "1h"}},
				{ID: "hl-short", Type: "perps", Platform: "hyperliquid", Args: []string{"trend"}},
				{ID: "hl-empty", Type: "perps", Platform: "hyperliquid", Args: []string{"trend", "", "1h"}},
			},
			wantHL:  []string{"BTC", "ETH"},
			wantOKX: []string{"BTC", "SOL"},
		},
		{
			name: "no perps yields empty lists",
			strategies: []StrategyConfig{
				{ID: "sma-btc", Type: "spot", Platform: "binanceus", Args: []string{"sma", "BTC/USDT", "1h"}},
			},
		},
		{
			name: "manual hyperliquid keyed on sc.Symbol, okx manual ignored",
			strategies: []StrategyConfig{
				{ID: "manual-hl-eth", Type: "manual", Platform: "hyperliquid", Symbol: "ETH",
					Args: []string{"hold", "WRONGCOIN", "1h", "--mode=live"}},
				{ID: "manual-hl-hype", Type: "manual", Platform: "hyperliquid", Symbol: "HYPE",
					Args: []string{"hold", "HYPE", "1h", "--mode=paper"}},
				{ID: "hl-trend-btc", Type: "perps", Platform: "hyperliquid", Args: []string{"trend", "BTC", "1h"}},
				{ID: "manual-hl-btc", Type: "manual", Platform: "hyperliquid", Symbol: "BTC",
					Args: []string{"hold", "BTC", "1h", "--mode=live"}},
				{ID: "manual-okx-sol", Type: "manual", Platform: "okx", Symbol: "SOL",
					Args: []string{"hold", "SOL", "1h", "--mode=live"}},
				{ID: "manual-hl-empty", Type: "manual", Platform: "hyperliquid", Symbol: "",
					Args: []string{"hold", "DOGE", "1h", "--mode=live"}},
			},
			wantHL: []string{"BTC", "ETH", "HYPE"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			hlCoins, okxCoins := collectPerpsMarkSymbols(tc.strategies)
			if len(hlCoins) != len(tc.wantHL) {
				t.Fatalf("hlCoins = %v, want %v", hlCoins, tc.wantHL)
			}
			for i, c := range tc.wantHL {
				if hlCoins[i] != c {
					t.Errorf("hlCoins[%d] = %q, want %q", i, hlCoins[i], c)
				}
			}
			if len(okxCoins) != len(tc.wantOKX) {
				t.Fatalf("okxCoins = %v, want %v", okxCoins, tc.wantOKX)
			}
			for i, c := range tc.wantOKX {
				if okxCoins[i] != c {
					t.Errorf("okxCoins[%d] = %q, want %q", i, okxCoins[i], c)
				}
			}
		})
	}
}

func TestMergePerpsMarks(t *testing.T) {
	prices := map[string]float64{
		"BTC/USDT": 50000.0,
		"ETH":      3199.5,
	}
	marks := map[string]float64{
		"ETH":  3200.1,
		"BTC":  67500.5,
		"SOL":  0,
		"DOGE": -1,
	}

	mergePerpsMarks(prices, marks)

	if prices["BTC/USDT"] != 50000.0 {
		t.Errorf("prices[BTC/USDT] = %v, want 50000 (unrelated entry mutated)", prices["BTC/USDT"])
	}
	if prices["ETH"] != 3199.5 {
		t.Errorf("prices[ETH] = %v, want 3199.5 (existing live mark must win)", prices["ETH"])
	}
	if prices["BTC"] != 67500.5 {
		t.Errorf("prices[BTC] = %v, want 67500.5 (new mark must be merged)", prices["BTC"])
	}
	if _, ok := prices["SOL"]; ok {
		t.Errorf("prices[SOL] should not be set when mark is zero (got %v)", prices["SOL"])
	}
	if _, ok := prices["DOGE"]; ok {
		t.Errorf("prices[DOGE] should not be set when mark is negative (got %v)", prices["DOGE"])
	}
}

func TestCheckRisk_ConsecutiveLossesForceClose(t *testing.T) {
	s := &StrategyState{
		ID:   "test-strategy",
		Cash: 5000.0,
		RiskState: RiskState{
			PeakValue:         10000.0,
			MaxDrawdownPct:    50.0,
			ConsecutiveLosses: 5,
			DailyPnLDate:      todayUTC(),
		},
		Positions: map[string]*Position{
			"BTC": {Symbol: "BTC", Quantity: 0.1, AvgCost: 50000.0, Side: "long"},
		},
		OptionPositions: make(map[string]*OptionPosition),
		TradeHistory:    []Trade{},
	}

	prices := map[string]float64{"BTC": 50000.0}
	pv := PortfolioValue(s, prices)

	allowed, reason := CheckRisk(nil, s, pv, prices, nil, nil)

	if allowed {
		t.Errorf("expected circuit breaker to fire; reason=%s", reason)
	}

	if len(s.Positions) != 0 {
		t.Errorf("expected Positions empty after force-close; got %d entries", len(s.Positions))
	}
	if len(s.TradeHistory) != 1 {
		t.Errorf("expected 1 trade recorded for force-close; got %d", len(s.TradeHistory))
	}
	expectedCash := 10000.0
	if s.Cash != expectedCash {
		t.Errorf("expected Cash=%.2f after force-close; got %.2f", expectedCash, s.Cash)
	}
}

func TestForceCloseAllPositionsRecordsDirectionalTradeSides(t *testing.T) {
	s := &StrategyState{
		ID:   "test-strategy",
		Cash: 10000,
		Positions: map[string]*Position{
			"BTC": {Symbol: "BTC", Quantity: 0.1, AvgCost: 50000, Side: "long"},
			"ETH": {Symbol: "ETH", Quantity: 0.5, AvgCost: 3000, Side: "short", Multiplier: 1, Leverage: 10},
		},
		OptionPositions: map[string]*OptionPosition{
			"long-call": {
				ID: "long-call", Action: "buy", Quantity: 1, EntryPremiumUSD: 1000, CurrentValueUSD: 1200,
			},
			"short-put": {
				ID: "short-put", Action: "sell", Quantity: 1, EntryPremiumUSD: 1000, CurrentValueUSD: -700,
			},
		},
		TradeHistory: []Trade{},
		RiskState:    RiskState{},
	}

	forceCloseAllPositions(s, nil, map[string]float64{"BTC": 51000, "ETH": 2800}, nil)

	if len(s.TradeHistory) != 4 {
		t.Fatalf("TradeHistory len = %d, want 4", len(s.TradeHistory))
	}
	gotSide := map[string]string{}
	for _, tr := range s.TradeHistory {
		if !tr.IsClose {
			t.Errorf("Trade %s IsClose = false, want true", tr.Symbol)
		}
		if tr.FeeSource != FeeSourceReconcileAdjustment {
			t.Errorf("Trade %s FeeSource = %q, want %q", tr.Symbol, tr.FeeSource, FeeSourceReconcileAdjustment)
		}
		gotSide[tr.Symbol] = tr.Side
	}
	wantSide := map[string]string{
		"BTC":       "sell",
		"ETH":       "buy",
		"long-call": "sell",
		"short-put": "buy",
	}
	for symbol, want := range wantSide {
		if got := gotSide[symbol]; got != want {
			t.Errorf("Trade.Side[%s] = %q, want %q", symbol, got, want)
		}
	}
}

func TestForceCloseAllPositions_CorruptPositionBooksZeroPnL(t *testing.T) {
	cases := []struct {
		name string
		pos  *Position
	}{
		{"negative qty long", &Position{Symbol: "ETH", Quantity: -0.595, AvgCost: 2000, Side: "long", Multiplier: 1, Leverage: 1}},
		{"negative qty short", &Position{Symbol: "ETH", Quantity: -0.595, AvgCost: 2000, Side: "short", Multiplier: 1, Leverage: 1}},
		{"zero avg cost long", &Position{Symbol: "ETH", Quantity: 0.5, AvgCost: 0, Side: "long", Multiplier: 1, Leverage: 1}},
		{"zero avg cost spot long", &Position{Symbol: "BTC", Quantity: 0.5, AvgCost: 0, Side: "long"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			startCash := 10000.0
			s := &StrategyState{
				ID:              "corrupt-strat",
				Cash:            startCash,
				Positions:       map[string]*Position{tc.pos.Symbol: tc.pos},
				OptionPositions: map[string]*OptionPosition{},
				TradeHistory:    []Trade{},
				ClosedPositions: []ClosedPosition{},
				RiskState:       RiskState{},
			}

			forceCloseAllPositions(s, nil, map[string]float64{tc.pos.Symbol: 2150}, nil)

			if len(s.TradeHistory) != 1 {
				t.Fatalf("TradeHistory len = %d, want 1", len(s.TradeHistory))
			}
			tr := s.TradeHistory[0]
			if tr.RealizedPnL != 0 {
				t.Errorf("Trade.RealizedPnL = %g, want 0 (corrupt position must book zero PnL)", tr.RealizedPnL)
			}
			if tr.Quantity < 0 {
				t.Errorf("Trade.Quantity = %g, must not be negative", tr.Quantity)
			}
			if len(s.ClosedPositions) != 1 {
				t.Fatalf("ClosedPositions len = %d, want 1", len(s.ClosedPositions))
			}
			cp := s.ClosedPositions[0]
			if math.Abs(tr.RealizedPnL-cp.RealizedPnL) > 1e-9 {
				t.Errorf("Trade.RealizedPnL %g != ClosedPosition.RealizedPnL %g (must reconcile)", tr.RealizedPnL, cp.RealizedPnL)
			}
			if cp.CloseReason != "circuit_breaker_corrupt" {
				t.Errorf("CloseReason = %q, want circuit_breaker_corrupt", cp.CloseReason)
			}
			if s.Cash != startCash {
				t.Errorf("Cash = %g, want %g (no phantom PnL/proceeds credited)", s.Cash, startCash)
			}
			if _, ok := s.Positions[tc.pos.Symbol]; ok {
				t.Error("corrupt position should be cleared")
			}
		})
	}
}

func TestForceCloseAllPositions_HealthyPositionReconciles(t *testing.T) {
	s := &StrategyState{
		ID:              "healthy-strat",
		Cash:            10000,
		Positions:       map[string]*Position{"ETH": {Symbol: "ETH", Quantity: 0.5, AvgCost: 2000, Side: "long", Multiplier: 1, Leverage: 1}},
		OptionPositions: map[string]*OptionPosition{},
		TradeHistory:    []Trade{},
		ClosedPositions: []ClosedPosition{},
		RiskState:       RiskState{},
	}
	forceCloseAllPositions(s, nil, map[string]float64{"ETH": 2100}, nil)
	if len(s.TradeHistory) != 1 || len(s.ClosedPositions) != 1 {
		t.Fatalf("history=%d closed=%d, want 1/1", len(s.TradeHistory), len(s.ClosedPositions))
	}
	wantPnL := 0.5 * (2100.0 - 2000.0)
	tr := s.TradeHistory[0]
	if math.Abs(tr.RealizedPnL-wantPnL) > 1e-9 {
		t.Errorf("Trade.RealizedPnL = %g, want %g", tr.RealizedPnL, wantPnL)
	}
	if math.Abs(tr.RealizedPnL-s.ClosedPositions[0].RealizedPnL) > 1e-9 {
		t.Errorf("trade PnL %g != closed_positions PnL %g", tr.RealizedPnL, s.ClosedPositions[0].RealizedPnL)
	}
}

func TestForceCloseAllPositions_ResidualRowMarkedReconcileAdjustment(t *testing.T) {
	s := &StrategyState{
		ID:              "hl-residual",
		Platform:        "hyperliquid",
		Type:            "perps",
		Cash:            10000,
		Positions:       map[string]*Position{"ETH": {Symbol: "ETH", Quantity: 0.5, AvgCost: 2000, Side: "long", Multiplier: 1, Leverage: 1}},
		OptionPositions: map[string]*OptionPosition{},
		TradeHistory:    []Trade{},
		ClosedPositions: []ClosedPosition{},
		RiskState:       RiskState{},
	}
	forceCloseAllPositions(s, nil, map[string]float64{"ETH": 2100}, nil)
	if len(s.TradeHistory) != 1 {
		t.Fatalf("TradeHistory len = %d, want 1", len(s.TradeHistory))
	}
	tr := s.TradeHistory[0]
	if tr.ExchangeOrderID != "" {
		t.Errorf("ExchangeOrderID = %q, want empty for model-only residual cleanup", tr.ExchangeOrderID)
	}
	if tr.ExchangeFee != 0 || !tr.PnLGross || tr.FeeSource != FeeSourceReconcileAdjustment {
		t.Errorf("force-close row fee metadata = fee %v gross %v source %q, want 0 / true / %q",
			tr.ExchangeFee, tr.PnLGross, tr.FeeSource, FeeSourceReconcileAdjustment)
	}
	if !strings.Contains(tr.Details, "model-only reconciliation adjustment") {
		t.Errorf("Details = %q, want model-only reconciliation marker", tr.Details)
	}
	if got, want := tradeLedgerDelta(tr), tr.RealizedPnL; math.Abs(got-want) > 1e-9 {
		t.Errorf("ledger delta = %v, want gross==net PnL %v", got, want)
	}
}

func TestForceCloseAllPositions_OptionRowsMarkedReconcileAdjustment(t *testing.T) {
	s := &StrategyState{
		ID:        "options-residual",
		Cash:      5000,
		Positions: map[string]*Position{},
		OptionPositions: map[string]*OptionPosition{
			"BTC-call-60000": {ID: "BTC-call-60000", Action: "buy", Quantity: 2, EntryPremiumUSD: 1000, CurrentValueUSD: 1200},
			"BTC-put-50000":  {ID: "BTC-put-50000", Action: "sell", Quantity: 1, EntryPremiumUSD: 700, CurrentValueUSD: -900},
		},
		TradeHistory:    []Trade{},
		ClosedPositions: []ClosedPosition{},
		RiskState:       RiskState{},
	}
	forceCloseAllPositions(s, nil, nil, nil)
	if len(s.TradeHistory) != 2 {
		t.Fatalf("TradeHistory len = %d, want 2", len(s.TradeHistory))
	}
	for _, tr := range s.TradeHistory {
		if tr.TradeType != "options" || !tr.IsClose {
			t.Errorf("trade = type %q close %v, want options close", tr.TradeType, tr.IsClose)
		}
		if tr.ExchangeOrderID != "" || tr.ExchangeFee != 0 || !tr.PnLGross || tr.FeeSource != FeeSourceReconcileAdjustment {
			t.Errorf("option force-close metadata for %s = oid %q fee %v gross %v source %q, want empty / 0 / true / %q",
				tr.Symbol, tr.ExchangeOrderID, tr.ExchangeFee, tr.PnLGross, tr.FeeSource, FeeSourceReconcileAdjustment)
		}
	}
}

func TestCheckPortfolioRisk_WarningFires(t *testing.T) {
	cfg := &PortfolioRiskConfig{MaxDrawdownPct: 25, WarnThresholdPct: 80}
	prs := &PortfolioRiskState{PeakValue: 10000.0}

	_, _, warning, reason := CheckPortfolioRisk(prs, cfg, 7900.0, 0, 0, 0)
	if !warning {
		t.Error("expected warning=true at 21% drawdown (warn threshold=20%)")
	}
	if reason == "" {
		t.Error("expected non-empty reason for warning")
	}
	if !prs.WarningSent {
		t.Error("expected WarningSent=true after warning fires")
	}
	if prs.WarnBandEnteredAt.IsZero() {
		t.Error("expected WarnBandEnteredAt to be stamped after warning fires")
	}

	_, _, warning, reason = CheckPortfolioRisk(prs, cfg, 7900.0, 0, 0, 0)
	if !warning {
		t.Error("expected warning=true on second call while still in warning band")
	}
	if reason == "" {
		t.Error("expected non-empty reason for repeated warning")
	}
	if prs.LastWarningEquityDDPct < 20.9 || prs.LastWarningEquityDDPct > 21.1 {
		t.Errorf("LastWarningEquityDDPct = %.2f, want about 21", prs.LastWarningEquityDDPct)
	}
	if math.Abs(prs.WarningEquityDeltaPct) > 0.01 {
		t.Errorf("WarningEquityDeltaPct = %.2f, want 0 for unchanged drawdown", prs.WarningEquityDeltaPct)
	}
}

func TestCheckPortfolioRisk_WarningRepeatsAcrossCycles(t *testing.T) {
	cfg := &PortfolioRiskConfig{MaxDrawdownPct: 25, WarnThresholdPct: 80}
	prs := &PortfolioRiskState{PeakValue: 10000.0}

	for i := 0; i < 5; i++ {
		_, _, warning, reason := CheckPortfolioRisk(prs, cfg, 7900.0, 0, 0, 0)
		if !warning {
			t.Errorf("cycle %d: expected warning=true while in warn band", i)
		}
		if reason == "" {
			t.Errorf("cycle %d: expected non-empty reason", i)
		}
		if !prs.WarningSent {
			t.Errorf("cycle %d: expected WarningSent=true while in warn band", i)
		}
	}
}

func TestCheckPortfolioRisk_WarnBandEnteredTransition(t *testing.T) {
	cfg := &PortfolioRiskConfig{MaxDrawdownPct: 25, WarnThresholdPct: 80}
	prs := &PortfolioRiskState{PeakValue: 10000.0}

	for i := 0; i < 5; i++ {
		prevWarningSent := prs.WarningSent
		_, _, warning, _ := CheckPortfolioRisk(prs, cfg, 7900.0, 0, 0, 0)
		enteredWarnBand := warning && !prevWarningSent
		if i == 0 {
			if !enteredWarnBand {
				t.Error("cycle 0: expected enteredWarnBand=true on first entry")
			}
		} else {
			if enteredWarnBand {
				t.Errorf("cycle %d: expected enteredWarnBand=false while already in warn band", i)
			}
		}
	}

	CheckPortfolioRisk(prs, cfg, 8500.0, 0, 0, 0)
	prevWarningSent := prs.WarningSent
	_, _, warning, _ := CheckPortfolioRisk(prs, cfg, 7900.0, 0, 0, 0)
	if !warning {
		t.Error("expected warning=true after re-crossing warn threshold")
	}
	if !warning || prevWarningSent {
		t.Error("expected enteredWarnBand=true on re-entry after recovery")
	}
}

func TestCheckPortfolioRisk_WarningResetOnRecovery(t *testing.T) {
	cfg := &PortfolioRiskConfig{MaxDrawdownPct: 25, WarnThresholdPct: 80}
	prs := &PortfolioRiskState{PeakValue: 10000.0}

	CheckPortfolioRisk(prs, cfg, 7900.0, 0, 0, 0)
	if !prs.WarningSent {
		t.Fatal("expected WarningSent=true after first warning")
	}

	CheckPortfolioRisk(prs, cfg, 8500.0, 0, 0, 0)
	if prs.WarningSent {
		t.Error("expected WarningSent=false after recovery below warn threshold")
	}
	if !prs.WarnBandEnteredAt.IsZero() {
		t.Error("expected WarnBandEnteredAt reset after recovery below warn threshold")
	}

	_, _, warning, _ := CheckPortfolioRisk(prs, cfg, 7900.0, 0, 0, 0)
	if !warning {
		t.Error("expected warning=true after recovery and re-crossing threshold")
	}
}

func TestCheckPortfolioRisk_WarningNotAfterKillSwitch(t *testing.T) {
	cfg := &PortfolioRiskConfig{MaxDrawdownPct: 25, WarnThresholdPct: 80}
	prs := &PortfolioRiskState{PeakValue: 10000.0}

	allowed, _, warning, _ := CheckPortfolioRisk(prs, cfg, 7400.0, 0, 0, 0)
	if allowed {
		t.Error("expected kill switch to fire")
	}
	if warning {
		t.Error("expected warning=false when kill switch fires (kill takes precedence)")
	}
}

func TestAddKillSwitchEvent_MaxCap(t *testing.T) {
	prs := &PortfolioRiskState{}

	for i := 0; i < 60; i++ {
		addKillSwitchEvent(prs, "warning", "equity", float64(i), 1000, 2000, "test")
	}

	if len(prs.Events) != maxKillSwitchEvents {
		t.Errorf("expected %d events; got %d", maxKillSwitchEvents, len(prs.Events))
	}
	if prs.Events[0].DrawdownPct != 10 {
		t.Errorf("expected oldest event drawdown=10; got %.0f", prs.Events[0].DrawdownPct)
	}
}

func TestCheckPortfolioRisk_EventLoggedOnTrigger(t *testing.T) {
	cfg := &PortfolioRiskConfig{MaxDrawdownPct: 25, WarnThresholdPct: 80}
	prs := &PortfolioRiskState{PeakValue: 10000.0}

	CheckPortfolioRisk(prs, cfg, 7400.0, 0, 0, 0)

	if len(prs.Events) != 1 {
		t.Fatalf("expected 1 event; got %d", len(prs.Events))
	}
	if prs.Events[0].Type != "triggered" {
		t.Errorf("expected event type='triggered'; got %q", prs.Events[0].Type)
	}
	if prs.Events[0].PortfolioValue != 7400.0 {
		t.Errorf("expected portfolio_value=7400; got %.2f", prs.Events[0].PortfolioValue)
	}
}

func resetSchedulerStarted(t *testing.T) {
	t.Helper()
	schedulerStarted.Store(false)
	t.Cleanup(func() { schedulerStarted.Store(false) })
}

func latchedSharedWalletState() *AppState {
	return &AppState{
		Strategies: map[string]*StrategyState{},
		PortfolioRisk: map[PortfolioScope]*PortfolioRiskState{ScopeLive: {
			PeakValue:                10000,
			CurrentDrawdownPct:       50,
			CurrentMarginDrawdownPct: 26.84,
			KillSwitchActive:         true,
			KillSwitchAt:             time.Date(2026, 4, 1, 12, 0, 0, 0, time.UTC),
		}},
	}
}

func sharedHLStrategies(t *testing.T) []StrategyConfig {
	t.Helper()
	t.Setenv("HYPERLIQUID_ACCOUNT_ADDRESS", "0xshared")
	return []StrategyConfig{
		{ID: "hl-a", Platform: "hyperliquid", Type: "perps", CapitalPct: 0.5, Capital: 1000, Args: []string{"sma", "BTC", "1h", "--mode=live"}},
		{ID: "hl-b", Platform: "hyperliquid", Type: "perps", CapitalPct: 0.5, Capital: 1000, Args: []string{"tema", "ETH", "1h", "--mode=live"}},
	}
}

func TestClearLatchedKillSwitchSharedWallet_Success(t *testing.T) {
	resetSchedulerStarted(t)
	state := latchedSharedWalletState()
	strategies := sharedHLStrategies(t)

	calls := 0
	fetcher := func(platform string) (float64, error) {
		calls++
		if platform != "hyperliquid" {
			t.Errorf("expected fetcher called for hyperliquid; got %q", platform)
		}
		return 4500, nil
	}

	cleared := ClearLatchedKillSwitchSharedWallet(state, strategies, fetcher)
	if !cleared {
		t.Fatal("expected ClearLatchedKillSwitchSharedWallet to return true")
	}
	if calls != 1 {
		t.Errorf("expected 1 fetcher call; got %d", calls)
	}
	if state.scopeRisk(ScopeLive).KillSwitchActive {
		t.Error("expected KillSwitchActive=false after clear")
	}
	if !state.scopeRisk(ScopeLive).KillSwitchAt.IsZero() {
		t.Errorf("expected KillSwitchAt zeroed; got %v", state.scopeRisk(ScopeLive).KillSwitchAt)
	}
	if state.scopeRisk(ScopeLive).WarningSent {
		t.Error("expected WarningSent reset to false")
	}
	if state.scopeRisk(ScopeLive).PeakValue != 4500 {
		t.Errorf("expected PeakValue re-baselined to 4500; got %.2f", state.scopeRisk(ScopeLive).PeakValue)
	}
	if state.scopeRisk(ScopeLive).CurrentDrawdownPct != 0 {
		t.Errorf("expected CurrentDrawdownPct reset to 0; got %.2f", state.scopeRisk(ScopeLive).CurrentDrawdownPct)
	}
	if state.scopeRisk(ScopeLive).CurrentMarginDrawdownPct != 0 {
		t.Errorf("expected CurrentMarginDrawdownPct reset to 0; got %.2f", state.scopeRisk(ScopeLive).CurrentMarginDrawdownPct)
	}
	if len(state.scopeRisk(ScopeLive).Events) != 1 {
		t.Fatalf("expected 1 audit event; got %d", len(state.scopeRisk(ScopeLive).Events))
	}
	evt := state.scopeRisk(ScopeLive).Events[0]
	if evt.Type != "auto_reset" {
		t.Errorf("expected event type=auto_reset; got %q", evt.Type)
	}
	if evt.PortfolioValue != 4500 {
		t.Errorf("expected event portfolio_value=4500 (fetched balance); got %.2f", evt.PortfolioValue)
	}
	if evt.PeakValue != 4500 {
		t.Errorf("expected event peak_value=4500 (re-baselined); got %.2f", evt.PeakValue)
	}
}

func TestClearLatchedKillSwitchSharedWallet_NonLegacyMembersPreserveLatch(t *testing.T) {
	t.Setenv("HYPERLIQUID_ACCOUNT_ADDRESS", "0xshared")
	marginCap := 100.0
	tests := []struct {
		name       string
		strategies []StrategyConfig
	}{
		{
			name: "fixed capital",
			strategies: []StrategyConfig{
				{ID: "hl-a", Platform: "hyperliquid", Type: "perps", Capital: 1000, Args: []string{"sma", "BTC", "1h", "--mode=live"}},
				{ID: "hl-b", Platform: "hyperliquid", Type: "perps", Capital: 1000, Args: []string{"tema", "ETH", "1h", "--mode=live"}},
			},
		},
		{
			name: "zero-baseline pool",
			strategies: []StrategyConfig{
				{ID: "hl-a", Platform: "hyperliquid", Type: "perps", Args: []string{"sma", "BTC", "1h", "--mode=live"}, MarginPerTradeUSD: &marginCap, sharedWalletPoolBudget: true},
				{ID: "hl-b", Platform: "hyperliquid", Type: "perps", Args: []string{"tema", "ETH", "1h", "--mode=live"}, MarginPerTradeUSD: &marginCap, sharedWalletPoolBudget: true},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resetSchedulerStarted(t)
			state := latchedSharedWalletState()
			calls := 0
			cleared := ClearLatchedKillSwitchSharedWallet(state, tt.strategies, func(platform string) (float64, error) {
				calls++
				return 4500, nil
			})
			if cleared || calls != 0 || !state.scopeRisk(ScopeLive).KillSwitchActive {
				t.Fatalf("non-legacy wallet must preserve latch: cleared=%v calls=%d active=%v", cleared, calls, state.scopeRisk(ScopeLive).KillSwitchActive)
			}
		})
	}
}

func TestClearLatchedKillSwitchSharedWallet_NoRelatchOnNextTick(t *testing.T) {
	resetSchedulerStarted(t)
	state := &AppState{
		Strategies: map[string]*StrategyState{},
		PortfolioRisk: map[PortfolioScope]*PortfolioRiskState{ScopeLive: {
			PeakValue:          20000,
			CurrentDrawdownPct: 75,
			KillSwitchActive:   true,
			KillSwitchAt:       time.Date(2026, 4, 1, 12, 0, 0, 0, time.UTC),
		}},
	}
	strategies := sharedHLStrategies(t)

	fetcher := func(platform string) (float64, error) {
		return 5000, nil
	}

	if cleared := ClearLatchedKillSwitchSharedWallet(state, strategies, fetcher); !cleared {
		t.Fatal("expected auto-clear to succeed")
	}

	cfg := &PortfolioRiskConfig{MaxDrawdownPct: 25, WarnThresholdPct: 80}
	allowed, _, _, reason := CheckPortfolioRisk(state.scopeRisk(ScopeLive), cfg, 5000, 0, 0, 0)
	if !allowed {
		t.Fatalf("expected kill switch to stay cleared after auto-clear; got reason=%s", reason)
	}
	if state.scopeRisk(ScopeLive).KillSwitchActive {
		t.Error("expected KillSwitchActive=false after first post-clear tick — stale peak re-latched the switch")
	}
}

func TestClearLatchedKillSwitchSharedWallet_FetchFailurePreservesLatch(t *testing.T) {
	resetSchedulerStarted(t)
	state := latchedSharedWalletState()
	strategies := sharedHLStrategies(t)
	originalLatchedAt := state.scopeRisk(ScopeLive).KillSwitchAt

	fetcher := func(platform string) (float64, error) {
		return 0, fmt.Errorf("simulated network failure")
	}

	cleared := ClearLatchedKillSwitchSharedWallet(state, strategies, fetcher)
	if cleared {
		t.Fatal("expected ClearLatchedKillSwitchSharedWallet to return false on fetch failure")
	}
	if !state.scopeRisk(ScopeLive).KillSwitchActive {
		t.Error("expected KillSwitchActive to remain true after fetch failure")
	}
	if !state.scopeRisk(ScopeLive).KillSwitchAt.Equal(originalLatchedAt) {
		t.Errorf("expected KillSwitchAt unchanged; got %v", state.scopeRisk(ScopeLive).KillSwitchAt)
	}
	if len(state.scopeRisk(ScopeLive).Events) != 0 {
		t.Errorf("expected no audit event on failure; got %d", len(state.scopeRisk(ScopeLive).Events))
	}
}

func TestClearLatchedKillSwitchSharedWallet_NoOp(t *testing.T) {
	cases := []struct {
		name       string
		state      func() *AppState
		strategies func(t *testing.T) []StrategyConfig
	}{
		{
			name:  "no shared wallet detected",
			state: latchedSharedWalletState,
			strategies: func(t *testing.T) []StrategyConfig {
				return []StrategyConfig{
					{ID: "spot-a", Platform: "binanceus", Capital: 1000},
					{ID: "spot-b", Platform: "binanceus", Capital: 1000},
					{ID: "hl-solo", Platform: "hyperliquid", CapitalPct: 0.5, Capital: 1000},
				}
			},
		},
		{
			name: "switch already inactive",
			state: func() *AppState {
				return &AppState{PortfolioRisk: map[PortfolioScope]*PortfolioRiskState{ScopeLive: {PeakValue: 10000, KillSwitchActive: false}}}
			},
			strategies: sharedHLStrategies,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resetSchedulerStarted(t)
			state := tc.state()
			wantActive := state.scopeRisk(ScopeLive).KillSwitchActive
			calls := 0
			fetcher := func(platform string) (float64, error) {
				calls++
				return 5000, nil
			}
			if cleared := ClearLatchedKillSwitchSharedWallet(state, tc.strategies(t), fetcher); cleared {
				t.Error("expected no clear")
			}
			if calls != 0 {
				t.Errorf("expected fetcher NOT called; got %d calls", calls)
			}
			if state.scopeRisk(ScopeLive).KillSwitchActive != wantActive {
				t.Errorf("KillSwitchActive = %v, want unchanged %v", state.scopeRisk(ScopeLive).KillSwitchActive, wantActive)
			}
		})
	}
}

func TestClearLatchedKillSwitchSharedWallet_MultiPlatformAllSuccess(t *testing.T) {
	resetSchedulerStarted(t)
	t.Setenv("HYPERLIQUID_ACCOUNT_ADDRESS", "0xshared")
	t.Setenv("OKX_API_KEY", "okx-shared")
	state := latchedSharedWalletState()
	strategies := []StrategyConfig{
		{ID: "hl-a", Platform: "hyperliquid", Type: "perps", CapitalPct: 0.5, Capital: 1000, Args: []string{"sma", "BTC", "1h", "--mode=live"}},
		{ID: "hl-b", Platform: "hyperliquid", Type: "perps", CapitalPct: 0.5, Capital: 1000, Args: []string{"tema", "ETH", "1h", "--mode=live"}},
		{ID: "okx-a", Platform: "okx", Type: "perps", CapitalPct: 0.3, Capital: 300, Args: []string{"sma", "BTC", "1h", "--mode=live"}},
		{ID: "okx-b", Platform: "okx", Type: "perps", CapitalPct: 0.7, Capital: 700, Args: []string{"tema", "ETH", "1h", "--mode=live"}},
	}

	fetcher := func(platform string) (float64, error) {
		switch platform {
		case "hyperliquid":
			return 3000, nil
		case "okx":
			return 2000, nil
		}
		return 0, fmt.Errorf("unexpected platform %q", platform)
	}

	if cleared := ClearLatchedKillSwitchSharedWallet(state, strategies, fetcher); !cleared {
		t.Fatal("expected kill switch to clear when all platforms fetch successfully")
	}
	if state.scopeRisk(ScopeLive).KillSwitchActive {
		t.Error("expected KillSwitchActive=false")
	}
	if state.scopeRisk(ScopeLive).PeakValue != 5000 {
		t.Errorf("expected PeakValue=5000 (sum of hyperliquid+okx); got %.2f", state.scopeRisk(ScopeLive).PeakValue)
	}
	if len(state.scopeRisk(ScopeLive).Events) != 1 {
		t.Fatalf("expected 1 audit event; got %d", len(state.scopeRisk(ScopeLive).Events))
	}
	if state.scopeRisk(ScopeLive).Events[0].PortfolioValue != 5000 {
		t.Errorf("expected audit event portfolio_value=5000 (total); got %.2f",
			state.scopeRisk(ScopeLive).Events[0].PortfolioValue)
	}
}

func TestClearLatchedKillSwitchSharedWallet_MultiPlatformAnyFailPreservesLatch(t *testing.T) {
	resetSchedulerStarted(t)
	t.Setenv("HYPERLIQUID_ACCOUNT_ADDRESS", "0xshared")
	t.Setenv("OKX_API_KEY", "okx-shared")
	state := latchedSharedWalletState()
	originalLatchedAt := state.scopeRisk(ScopeLive).KillSwitchAt
	originalPeak := state.scopeRisk(ScopeLive).PeakValue
	strategies := []StrategyConfig{
		{ID: "hl-a", Platform: "hyperliquid", Type: "perps", CapitalPct: 0.5, Capital: 1000, Args: []string{"sma", "BTC", "1h", "--mode=live"}},
		{ID: "hl-b", Platform: "hyperliquid", Type: "perps", CapitalPct: 0.5, Capital: 1000, Args: []string{"tema", "ETH", "1h", "--mode=live"}},
		{ID: "okx-a", Platform: "okx", Type: "perps", CapitalPct: 0.3, Capital: 300, Args: []string{"sma", "BTC", "1h", "--mode=live"}},
		{ID: "okx-b", Platform: "okx", Type: "perps", CapitalPct: 0.7, Capital: 700, Args: []string{"tema", "ETH", "1h", "--mode=live"}},
	}

	fetcher := func(platform string) (float64, error) {
		if platform == "hyperliquid" {
			return 0, fmt.Errorf("hyperliquid unreachable")
		}
		return 2000, nil
	}

	if cleared := ClearLatchedKillSwitchSharedWallet(state, strategies, fetcher); cleared {
		t.Fatal("expected kill switch to remain latched when any platform fails")
	}
	if !state.scopeRisk(ScopeLive).KillSwitchActive {
		t.Error("expected KillSwitchActive to remain true")
	}
	if !state.scopeRisk(ScopeLive).KillSwitchAt.Equal(originalLatchedAt) {
		t.Error("expected KillSwitchAt unchanged")
	}
	if state.scopeRisk(ScopeLive).PeakValue != originalPeak {
		t.Errorf("expected PeakValue unchanged; got %.2f", state.scopeRisk(ScopeLive).PeakValue)
	}
	if len(state.scopeRisk(ScopeLive).Events) != 0 {
		t.Errorf("expected no audit event on partial failure; got %d", len(state.scopeRisk(ScopeLive).Events))
	}
}

func TestClearLatchedKillSwitchSharedWallet_PanicsAfterSchedulerStarted(t *testing.T) {
	resetSchedulerStarted(t)
	state := latchedSharedWalletState()
	originalPeak := state.scopeRisk(ScopeLive).PeakValue
	originalLatchedAt := state.scopeRisk(ScopeLive).KillSwitchAt
	strategies := sharedHLStrategies(t)

	markSchedulerStarted()

	fetcherCalls := 0
	fetcher := func(platform string) (float64, error) {
		fetcherCalls++
		return 4500, nil
	}

	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("expected panic when ClearLatchedKillSwitchSharedWallet runs after markSchedulerStarted")
		}
		msg, ok := r.(string)
		if !ok || !strings.Contains(msg, "after scheduler started") {
			t.Errorf("unexpected panic value: %#v", r)
		}
		if fetcherCalls != 0 {
			t.Errorf("expected fetcher not called on panic path; got %d calls", fetcherCalls)
		}
		if !state.scopeRisk(ScopeLive).KillSwitchActive {
			t.Error("expected latch untouched after panic")
		}
		if state.scopeRisk(ScopeLive).PeakValue != originalPeak {
			t.Errorf("expected PeakValue untouched; got %.2f", state.scopeRisk(ScopeLive).PeakValue)
		}
		if !state.scopeRisk(ScopeLive).KillSwitchAt.Equal(originalLatchedAt) {
			t.Error("expected KillSwitchAt untouched after panic")
		}
		if len(state.scopeRisk(ScopeLive).Events) != 0 {
			t.Errorf("expected no audit event on panic path; got %d", len(state.scopeRisk(ScopeLive).Events))
		}
	}()

	ClearLatchedKillSwitchSharedWallet(state, strategies, fetcher)
}

func TestAutoResetConfirmedFlatKillSwitch_Success(t *testing.T) {
	latchedAt := time.Date(2026, 4, 27, 12, 0, 0, 0, time.UTC)
	prs := &PortfolioRiskState{
		PeakValue:                1261.87,
		CurrentDrawdownPct:       3.63,
		CurrentMarginDrawdownPct: 26.84,
		KillSwitchActive:         true,
		KillSwitchAt:             latchedAt,
		WarningSent:              true,
	}

	if ok := AutoResetConfirmedFlatKillSwitch(prs, 1216.07, true, "confirmed flat; no owner configured"); !ok {
		t.Fatal("expected auto-reset to return true")
	}
	if prs.KillSwitchActive {
		t.Error("expected KillSwitchActive=false after confirmed-flat auto-reset")
	}
	if !prs.KillSwitchAt.IsZero() {
		t.Errorf("expected KillSwitchAt zeroed; got %v", prs.KillSwitchAt)
	}
	if prs.WarningSent {
		t.Error("expected WarningSent=false after confirmed-flat auto-reset")
	}
	if prs.PeakValue != 1216.07 {
		t.Errorf("expected PeakValue re-baselined to post-close value 1216.07; got %.2f", prs.PeakValue)
	}
	if prs.CurrentDrawdownPct != 0 {
		t.Errorf("expected CurrentDrawdownPct=0; got %.2f", prs.CurrentDrawdownPct)
	}
	if prs.CurrentMarginDrawdownPct != 0 {
		t.Errorf("expected CurrentMarginDrawdownPct=0; got %.2f", prs.CurrentMarginDrawdownPct)
	}
	if len(prs.Events) != 1 {
		t.Fatalf("expected 1 audit event; got %d", len(prs.Events))
	}
	evt := prs.Events[0]
	if evt.Type != "auto_reset" {
		t.Errorf("expected event type auto_reset; got %q", evt.Type)
	}
	if evt.DrawdownPct != 0 {
		t.Errorf("expected event drawdown 0; got %.2f", evt.DrawdownPct)
	}
	if evt.PortfolioValue != 1216.07 || evt.PeakValue != 1216.07 {
		t.Errorf("expected event portfolio/peak re-baselined to 1216.07; got portfolio=%.2f peak=%.2f",
			evt.PortfolioValue, evt.PeakValue)
	}
	if !strings.Contains(evt.Details, "previous equity drawdown=3.63%") ||
		!strings.Contains(evt.Details, "previous margin drawdown=26.84%") {
		t.Errorf("expected previous drawdowns in event details; got %q", evt.Details)
	}
}

func TestAutoResetConfirmedFlatKillSwitch_UntrustedEquityRetainsPeak(t *testing.T) {
	prs := &PortfolioRiskState{
		PeakValue:                10000,
		CurrentDrawdownPct:       99,
		CurrentMarginDrawdownPct: 30,
		KillSwitchActive:         true,
		KillSwitchAt:             time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC),
	}

	if ok := AutoResetConfirmedFlatKillSwitch(
		prs, 0, false, "confirmed flat on missing-balance cycle",
	); !ok {
		t.Fatal("expected auto-reset to clear the ownerless latch")
	}
	if prs.KillSwitchActive {
		t.Fatal("expected latch cleared after confirmed-flat close")
	}
	if prs.PeakValue != 10000 {
		t.Fatalf("untrusted equity changed peak: got %.2f want 10000", prs.PeakValue)
	}
	if len(prs.Events) != 1 {
		t.Fatalf("expected one audit event, got %d", len(prs.Events))
	}
	evt := prs.Events[0]
	if evt.PortfolioValue != 0 || evt.PeakValue != 10000 {
		t.Fatalf("event must preserve observed fallback and retained peak: %+v", evt)
	}
	if !strings.Contains(evt.Details, "peak retained") ||
		!strings.Contains(evt.Details, "current equity is not trustworthy") {
		t.Fatalf("event must explain retained peak: %q", evt.Details)
	}
}

func TestPortfolioPeakRebaselineAvailable(t *testing.T) {
	tests := []struct {
		name                 string
		usedPVFallback       bool
		usedStaleRiskBalance bool
		pooledEquityComplete bool
		want                 bool
	}{
		{name: "fresh complete equity", pooledEquityComplete: true, want: true},
		{name: "modeled fallback", usedPVFallback: true, pooledEquityComplete: true},
		{name: "accepted prior snapshot", usedStaleRiskBalance: true, pooledEquityComplete: true},
		{name: "missing pooled equity"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := portfolioPeakRebaselineAvailable(
				tt.usedPVFallback, tt.usedStaleRiskBalance, tt.pooledEquityComplete,
			); got != tt.want {
				t.Fatalf("available=%v, want %v", got, tt.want)
			}
		})
	}
}

func TestAutoResetConfirmedFlatKillSwitch_NoOpWhenInactive(t *testing.T) {
	prs := &PortfolioRiskState{
		PeakValue:                5000,
		CurrentDrawdownPct:       4,
		CurrentMarginDrawdownPct: 8,
	}

	if ok := AutoResetConfirmedFlatKillSwitch(prs, 4500, true, "no-op"); ok {
		t.Fatal("expected inactive kill switch to be a no-op")
	}
	if prs.PeakValue != 5000 {
		t.Errorf("expected PeakValue unchanged; got %.2f", prs.PeakValue)
	}
	if prs.CurrentDrawdownPct != 4 || prs.CurrentMarginDrawdownPct != 8 {
		t.Errorf("expected drawdown fields unchanged; got equity=%.2f margin=%.2f",
			prs.CurrentDrawdownPct, prs.CurrentMarginDrawdownPct)
	}
	if len(prs.Events) != 0 {
		t.Errorf("expected no event on no-op; got %d", len(prs.Events))
	}
}

func TestAutoResetConfirmedFlatKillSwitch_NoRelatchOnNextTick(t *testing.T) {
	prs := &PortfolioRiskState{
		PeakValue:          10000,
		CurrentDrawdownPct: 30,
		KillSwitchActive:   true,
		KillSwitchAt:       time.Date(2026, 4, 27, 12, 0, 0, 0, time.UTC),
	}

	AutoResetConfirmedFlatKillSwitch(prs, 7000, true, "confirmed flat; no owner configured")

	cfg := &PortfolioRiskConfig{MaxDrawdownPct: 25, WarnThresholdPct: 80}
	allowed, _, _, reason := CheckPortfolioRisk(prs, cfg, 7000, 0, 0, 0)
	if !allowed {
		t.Fatalf("expected next tick to resume trading after auto-reset; got reason=%s", reason)
	}
	if prs.KillSwitchActive {
		t.Error("expected kill switch to remain inactive on next tick")
	}
	if prs.PeakValue != 7000 {
		t.Errorf("expected PeakValue to remain at re-baselined value 7000; got %.2f", prs.PeakValue)
	}
}

func TestPerpsMarginDrawdownInputs(t *testing.T) {
	cases := []struct {
		name       string
		positions  map[string]*Position
		leverage   float64
		prices     map[string]float64
		wantLoss   float64
		wantMargin float64
	}{
		{
			name: "only perps count, gain books no loss",
			positions: map[string]*Position{
				"ETH":      {Symbol: "ETH", Quantity: 0.2, AvgCost: 2000, Side: "long", Multiplier: 1, Leverage: 20},
				"BTC/USDT": {Symbol: "BTC/USDT", Quantity: 0.05, AvgCost: 50000, Side: "long"},
			},
			leverage: 20, prices: map[string]float64{"ETH": 3000, "BTC/USDT": 60000, "ES": 4500},
			wantLoss: 0, wantMargin: 30,
		},
		{
			name: "only underwater legs add to loss",
			positions: map[string]*Position{
				"ETH": {Symbol: "ETH", Quantity: 1, AvgCost: 3000, Side: "long", Multiplier: 1, Leverage: 10},
				"BTC": {Symbol: "BTC", Quantity: 0.1, AvgCost: 50000, Side: "short", Multiplier: 1, Leverage: 10},
			},
			leverage: 10, prices: map[string]float64{"ETH": 2700, "BTC": 47500},
			wantLoss: 300, wantMargin: 745,
		},
		{
			name: "missing price falls back to avg cost",
			positions: map[string]*Position{
				"HYPE": {Symbol: "HYPE", Quantity: 100, AvgCost: 20, Side: "long", Multiplier: 1, Leverage: 10},
			},
			leverage: 10, prices: map[string]float64{},
			wantLoss: 0, wantMargin: 200,
		},
		{
			name: "zero price falls back to avg cost",
			positions: map[string]*Position{
				"HYPE": {Symbol: "HYPE", Quantity: 100, AvgCost: 20, Side: "long", Multiplier: 1, Leverage: 10},
			},
			leverage: 10, prices: map[string]float64{"HYPE": 0},
			wantLoss: 0, wantMargin: 200,
		},
		{
			name:      "no positions",
			positions: map[string]*Position{},
			leverage:  10,
		},
		{
			name: "uses config leverage not position leverage",
			positions: map[string]*Position{
				"ETH": {Symbol: "ETH", Quantity: 1.0, AvgCost: 3000, Side: "long", Multiplier: 1, Leverage: 20},
			},
			leverage: 2, prices: map[string]float64{"ETH": 2900},
			wantLoss: 100, wantMargin: 1450,
		},
		{
			name: "zero config leverage returns zero",
			positions: map[string]*Position{
				"ETH": {Symbol: "ETH", Quantity: 1.0, AvgCost: 3000, Side: "long", Multiplier: 1, Leverage: 20},
			},
			leverage: 0, prices: map[string]float64{"ETH": 2900},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			loss, margin := perpsMarginDrawdownInputs(&StrategyState{Positions: tc.positions}, tc.leverage, tc.prices)
			if math.Abs(margin-tc.wantMargin) > 1e-6 {
				t.Errorf("margin = %.4f; want %.4f", margin, tc.wantMargin)
			}
			if math.Abs(loss-tc.wantLoss) > 1e-6 {
				t.Errorf("loss = %.4f; want %.4f", loss, tc.wantLoss)
			}
		})
	}
}

func TestAggregatePerpsMarginInputs(t *testing.T) {
	ethLong := map[string]*Position{
		"ETH": {Symbol: "ETH", Quantity: 1, AvgCost: 3000, Side: "long", Multiplier: 1, Leverage: 20},
	}
	cases := []struct {
		name       string
		strategies map[string]*StrategyState
		configs    []StrategyConfig
		prices     map[string]float64
		wantLoss   float64
		wantMargin float64
	}{
		{
			name: "sums perps only across strategies",
			strategies: map[string]*StrategyState{
				"hl-btc": {Type: "perps", Positions: map[string]*Position{
					"BTC": {Symbol: "BTC", Quantity: 1, AvgCost: 40000, Side: "short", Multiplier: 1, Leverage: 10},
				}},
				"hl-eth": {Type: "perps", Positions: map[string]*Position{
					"ETH": {Symbol: "ETH", Quantity: 10, AvgCost: 3000, Side: "long", Multiplier: 1, Leverage: 5},
				}},
				"spot-sol": {Type: "spot", Positions: map[string]*Position{
					"SOL/USDT": {Symbol: "SOL/USDT", Quantity: 100, AvgCost: 150, Side: "long"},
				}},
				"ts-es": {Type: "futures", Positions: map[string]*Position{
					"ES": {Symbol: "ES", Quantity: 1, AvgCost: 5000, Side: "long", Multiplier: 50},
				}},
			},
			configs:  []StrategyConfig{{ID: "hl-btc", Leverage: 10}, {ID: "hl-eth", Leverage: 5}},
			prices:   map[string]float64{"BTC": 42000, "ETH": 3100, "SOL/USDT": 200, "ES": 5100},
			wantLoss: 2000, wantMargin: 10400,
		},
		{
			name: "no perps returns zero",
			strategies: map[string]*StrategyState{
				"spot-btc": {Type: "spot", Positions: map[string]*Position{
					"BTC/USDT": {Symbol: "BTC/USDT", Quantity: 0.5, AvgCost: 40000, Side: "long"},
				}},
			},
			prices: map[string]float64{"BTC/USDT": 50000},
		},
		{
			name:       "uses config leverage not position leverage",
			strategies: map[string]*StrategyState{"hl-eth": {Type: "perps", Positions: ethLong}},
			configs:    []StrategyConfig{{ID: "hl-eth", Leverage: 2}},
			prices:     map[string]float64{"ETH": 2900},
			wantLoss:   100, wantMargin: 1450,
		},
		{
			name:       "uses exchange leverage not sizing_leverage",
			strategies: map[string]*StrategyState{"hl-eth": {Type: "perps", Positions: ethLong}},
			configs:    []StrategyConfig{{ID: "hl-eth", Leverage: 20, SizingLeverage: 2}},
			prices:     map[string]float64{"ETH": 2900},
			wantLoss:   100, wantMargin: 145,
		},
		{
			name:       "missing config skips the strategy",
			strategies: map[string]*StrategyState{"hl-orphan": {Type: "perps", Positions: ethLong}},
			prices:     map[string]float64{"ETH": 2900},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			loss, margin := AggregatePerpsMarginInputs(tc.strategies, tc.configs, tc.prices)
			if math.Abs(margin-tc.wantMargin) > 1e-6 {
				t.Errorf("margin = %.4f; want %.4f", margin, tc.wantMargin)
			}
			if math.Abs(loss-tc.wantLoss) > 1e-6 {
				t.Errorf("loss = %.4f; want %.4f", loss, tc.wantLoss)
			}
		})
	}
}

func TestCheckRisk_DrawdownBasis(t *testing.T) {
	hlSC := &StrategyConfig{ID: "hl-test", Platform: "hyperliquid", Type: "perps", Leverage: 20}
	perpsState := func(cash, peak, maxDD float64, positions map[string]*Position) *StrategyState {
		return &StrategyState{
			ID:   "hl-test",
			Type: "perps",
			Cash: cash,
			RiskState: RiskState{
				PeakValue:      peak,
				MaxDrawdownPct: maxDD,
				DailyPnLDate:   todayUTC(),
			},
			Positions:       positions,
			OptionPositions: make(map[string]*OptionPosition),
			TradeHistory:    []Trade{},
		}
	}
	cases := []struct {
		name          string
		state         *StrategyState
		sc            *StrategyConfig
		prices        map[string]float64
		wantAllowed   bool
		ddLo, ddHi    float64
		wantOpenCount int
	}{
		{
			name: "perps margin basis fires early on unrealized loss",
			state: perpsState(584, 589, 25, map[string]*Position{
				"ETH": {Symbol: "ETH", Quantity: 0.236, AvgCost: 2357.0, Side: "long", Multiplier: 1, Leverage: 20},
			}),
			sc: hlSC, prices: map[string]float64{"ETH": 2307.5},
			ddLo: 40, ddHi: math.Inf(1),
		},
		{
			name: "perps drawdown fires before any closed trades",
			state: perpsState(500, 500, 10, map[string]*Position{
				"ETH": {Symbol: "ETH", Quantity: 1, AvgCost: 100, Side: "long", Multiplier: 1, Leverage: 20},
			}),
			sc: hlSC, prices: map[string]float64{"ETH": 80},
			ddLo: 10, ddHi: math.Inf(1),
		},
		{
			name: "perps margin basis below threshold stays allowed",
			state: perpsState(584, 589, 25, map[string]*Position{
				"ETH": {Symbol: "ETH", Quantity: 0.236, AvgCost: 2357.0, Side: "long", Multiplier: 1, Leverage: 20},
			}),
			sc: hlSC, prices: map[string]float64{"ETH": 2355.0},
			wantAllowed: true, ddLo: 0, ddHi: 24.999, wantOpenCount: 1,
		},
		{
			name: "prior realized losses do not inflate the perps drawdown",
			state: perpsState(900, 1000, 25, map[string]*Position{
				"ETH": {Symbol: "ETH", Quantity: 0.001, AvgCost: 3000, Side: "long", Multiplier: 1, Leverage: 20},
			}),
			sc: hlSC, prices: map[string]float64{"ETH": 3000},
			wantAllowed: true, ddLo: 0, ddHi: 0.001, wantOpenCount: 1,
		},
		{
			name:  "perps with no open positions falls back to peak basis",
			state: perpsState(700, 1000, 25, map[string]*Position{}),
			ddLo:  29, ddHi: 31,
		},
		{
			name: "spot stays on peak basis",
			state: &StrategyState{
				Type: "spot",
				Cash: 500.0,
				RiskState: RiskState{
					PeakValue:      1000.0,
					MaxDrawdownPct: 25.0,
					DailyPnLDate:   todayUTC(),
				},
				Positions: map[string]*Position{
					"BTC/USDT": {Symbol: "BTC/USDT", Quantity: 0.01, AvgCost: 50000, Side: "long"},
				},
				OptionPositions: make(map[string]*OptionPosition),
			},
			prices:      map[string]float64{"BTC/USDT": 30000},
			wantAllowed: true, ddLo: 19.5, ddHi: 20.5, wantOpenCount: 1,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := tc.state
			pv := PortfolioValue(s, tc.prices)
			allowed, reason := CheckRisk(tc.sc, s, pv, tc.prices, nil, nil)
			if allowed != tc.wantAllowed {
				t.Fatalf("allowed = %v, want %v (reason=%q dd=%.2f)", allowed, tc.wantAllowed, reason, s.RiskState.CurrentDrawdownPct)
			}
			if !tc.wantAllowed && !strings.HasPrefix(reason, RiskReasonMaxDrawdownExceeded) {
				t.Fatalf("reason = %q, want %q prefix", reason, RiskReasonMaxDrawdownExceeded)
			}
			if s.RiskState.CircuitBreaker == tc.wantAllowed {
				t.Errorf("CircuitBreaker = %v, want %v", s.RiskState.CircuitBreaker, !tc.wantAllowed)
			}
			if dd := s.RiskState.CurrentDrawdownPct; dd < tc.ddLo || dd > tc.ddHi {
				t.Errorf("CurrentDrawdownPct = %.2f, want within [%.3f, %.3f]", dd, tc.ddLo, tc.ddHi)
			}
			if len(s.Positions) != tc.wantOpenCount {
				t.Errorf("open positions = %d, want %d", len(s.Positions), tc.wantOpenCount)
			}
		})
	}
}

func TestCheckRisk_SharedWalletPoolUsesMarginWithoutFakePeak(t *testing.T) {
	marginCap := 100.0
	sc := StrategyConfig{
		ID: "hl-pool", Platform: "hyperliquid", Type: "perps",
		Args:                   []string{"sma", "BTC", "1h", "--mode=live"},
		Leverage:               5,
		MarginPerTradeUSD:      &marginCap,
		sharedWalletPoolBudget: true,
	}
	s := &StrategyState{
		ID: "hl-pool", Platform: "hyperliquid", Type: "perps",
		Positions: map[string]*Position{
			"BTC": {Symbol: "BTC", Quantity: 1, AvgCost: 100, Side: "long", Multiplier: 1, Leverage: 5},
		},
		RiskState: RiskState{PeakValue: 0, MaxDrawdownPct: 50},
	}
	allowed, reason := CheckRisk(&sc, s, -20, map[string]float64{"BTC": 80}, newTestLogger(t), nil)
	if allowed || !strings.HasPrefix(reason, RiskReasonMaxDrawdownExceeded) {
		t.Fatalf("pooled margin loss should fire without a fake peak: allowed=%v reason=%q", allowed, reason)
	}
}

func TestCheckRisk_LiveHLCircuitBreaker_SharedCoinVsSoleOwner(t *testing.T) {
	peer := StrategyConfig{ID: "hl-rmc", Platform: "hyperliquid", Type: "perps",
		CapitalPct: 0.5, Capital: 500, Leverage: 20,
		Args: []string{"rsi_macd", "ETH", "1h", "--mode=live"}}
	cases := []struct {
		name        string
		capitalPct  float64
		withPeer    bool
		hlPositions []HLPosition
		wantClose   bool
	}{
		{"shared coin pauses without close", 0.5, true, []HLPosition{{Coin: "ETH", Size: 0.517, EntryPrice: 3000}}, false},
		{"shared coin pauses without close when HL fetch failed", 0.5, true, nil, false},
		{"sole owner still force-closes", 0, false, []HLPosition{{Coin: "ETH", Size: 0.517, EntryPrice: 3000}}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sc := StrategyConfig{
				ID: "hl-tema", Platform: "hyperliquid", Type: "perps",
				CapitalPct: tc.capitalPct, Capital: 500, Leverage: 20,
				Args: []string{"triple_ema", "ETH", "1h", "--mode=live"},
			}
			hlLiveAll := []StrategyConfig{sc}
			if tc.withPeer {
				hlLiveAll = append(hlLiveAll, peer)
			}
			assist := &PlatformRiskAssist{HLPositions: tc.hlPositions, HLLiveAll: hlLiveAll}
			s := &StrategyState{
				ID:       sc.ID,
				Type:     "perps",
				Platform: "hyperliquid",
				Cash:     584.0,
				RiskState: RiskState{
					PeakValue:      589.0,
					MaxDrawdownPct: 25.0,
					DailyPnLDate:   todayUTC(),
				},
				Positions: map[string]*Position{
					"ETH": {Symbol: "ETH", Quantity: 0.236, AvgCost: 2357.0, Side: "long", Multiplier: 1, Leverage: 20},
				},
				OptionPositions: make(map[string]*OptionPosition),
				TradeHistory:    []Trade{},
			}
			prices := map[string]float64{"ETH": 2307.5}

			allowed, _ := CheckRisk(&sc, s, PortfolioValue(s, prices), prices, nil, assist)

			if allowed {
				t.Fatal("expected risk block")
			}
			if !s.RiskState.CircuitBreaker {
				t.Fatal("expected circuit breaker to be active")
			}
			p := s.RiskState.getPendingCircuitClose(PlatformPendingCloseHyperliquid)
			_, open := s.Positions["ETH"]
			if tc.wantClose {
				if p == nil || len(p.Symbols) != 1 || p.Symbols[0].Symbol != "ETH" {
					t.Fatalf("expected Hyperliquid pending close for sole owner; got %+v", p)
				}
				if open {
					t.Fatal("expected sole-owner virtual position to be force-closed")
				}
				if len(s.TradeHistory) != 1 {
					t.Fatalf("expected one circuit-breaker close trade; got %d", len(s.TradeHistory))
				}
				return
			}
			if p != nil {
				t.Fatalf("expected no Hyperliquid pending close for shared coin; got %+v", p)
			}
			if !open {
				t.Fatal("expected shared-coin virtual position to remain open")
			}
			if len(s.TradeHistory) != 0 {
				t.Fatalf("expected no circuit-breaker close trade for shared coin; got %d", len(s.TradeHistory))
			}
		})
	}
}

func TestCheckRisk_LiveTopStepCB_PendingFlatten(t *testing.T) {
	cases := []struct {
		name        string
		withPeer    bool
		tsSize      int
		posQty      float64
		wantPending bool
		wantSize    float64
	}{
		{"sole peer sets pending full flatten", false, 3, 3, true, 3},
		{"multi-peer contract sets no pending", true, 5, 2, false, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sc := StrategyConfig{
				ID: "ts-a", Platform: "topstep", Type: "futures",
				Capital: 5000,
				Args:    []string{"sma", "ES", "15m", "--mode=live"},
			}
			tsLiveAll := []StrategyConfig{sc}
			if tc.withPeer {
				tsLiveAll = append(tsLiveAll, StrategyConfig{ID: "ts-b", Platform: "topstep", Type: "futures",
					Capital: 5000,
					Args:    []string{"rsi", "ES", "15m", "--mode=live"}})
			}
			assist := &PlatformRiskAssist{
				TSPositions: []TopStepPosition{{Coin: "ES", Size: tc.tsSize, Side: "long"}},
				TSLiveAll:   tsLiveAll,
			}
			s := &StrategyState{
				ID:   sc.ID,
				Type: "futures",
				Cash: 3000.0,
				RiskState: RiskState{
					PeakValue:      5000.0,
					MaxDrawdownPct: 25.0,
					DailyPnLDate:   todayUTC(),
				},
				Positions: map[string]*Position{
					"ES": {Symbol: "ES", Quantity: tc.posQty, AvgCost: 5000, Side: "long", Multiplier: 50},
				},
				OptionPositions: make(map[string]*OptionPosition),
			}
			prices := map[string]float64{"ES": 4995}

			allowed, _ := CheckRisk(&sc, s, PortfolioValue(s, prices), prices, nil, assist)
			if allowed {
				t.Fatal("expected CB fire (drawdown exceeds 25%)")
			}
			p := s.RiskState.getPendingCircuitClose(PlatformPendingCloseTopStep)
			if !tc.wantPending {
				if p != nil {
					t.Errorf("expected no pending TS entry for multi-peer contract; got %+v", p)
				}
				return
			}
			if p == nil {
				t.Fatal("expected PendingCircuitCloses[topstep] after CB fire")
			}
			if len(p.Symbols) != 1 {
				t.Fatalf("expected 1 pending symbol, got %d", len(p.Symbols))
			}
			if p.Symbols[0].Symbol != "ES" {
				t.Errorf("symbol=%q want ES", p.Symbols[0].Symbol)
			}
			if p.Symbols[0].Size != tc.wantSize {
				t.Errorf("pending size=%.0f want %.0f (full flatten for sole peer)", p.Symbols[0].Size, tc.wantSize)
			}
		})
	}
}

func TestDetectSharedWalletPlatforms(t *testing.T) {
	t.Setenv("HYPERLIQUID_ACCOUNT_ADDRESS", "0xshared")
	t.Setenv("OKX_API_KEY", "okx-shared")
	strategies := []StrategyConfig{
		{ID: "hl-a", Platform: "hyperliquid", Type: "perps", Capital: 1000, CapitalPct: 0.5, Args: []string{"sma", "BTC", "1h", "--mode=live"}},
		{ID: "hl-b", Platform: "hyperliquid", Type: "perps", Capital: 1000, CapitalPct: 0.5, Args: []string{"tema", "ETH", "1h", "--mode=live"}},
		{ID: "okx-solo", Platform: "okx", Type: "perps", Capital: 1000, Args: []string{"sma", "BTC", "1h", "--mode=live"}},
		{ID: "spot-a", Platform: "binanceus", Capital: 1000},
		{ID: "spot-b", Platform: "binanceus", Capital: 1000},
	}

	got := detectSharedWalletPlatforms(strategies)
	if len(got) != 1 || got[0] != "hyperliquid" {
		t.Errorf("expected [hyperliquid]; got %v", got)
	}
}

func TestDetectSharedWalletPlatformsCountsLegacyManualMember(t *testing.T) {
	t.Setenv("HYPERLIQUID_ACCOUNT_ADDRESS", "0xshared")
	strategies := []StrategyConfig{
		{ID: "hl-perps", Platform: "hyperliquid", Type: "perps", Capital: 500, CapitalPct: 0.5, Args: []string{"sma", "BTC", "1h", "--mode=live"}},
		{ID: "hl-manual", Platform: "hyperliquid", Type: "manual", Capital: 500, CapitalPct: 0.5, Args: []string{"hold", "ETH", "1h", "--mode=live"}},
	}

	got := detectSharedWalletPlatforms(strategies)
	if len(got) != 1 || got[0] != "hyperliquid" {
		t.Fatalf("legacy perps+manual wallet must qualify for #244 auto-clear; got %v", got)
	}

	strategies[1].CapitalPct = 0
	if got := detectSharedWalletPlatforms(strategies); len(got) != 0 {
		t.Fatalf("mixed percentage/fixed wallet must not widen auto-clear; got %v", got)
	}

	strategies[0].CapitalPct = 0
	strategies[0].sharedWalletPoolBudget = true
	if got := detectSharedWalletPlatforms(strategies); len(got) != 0 {
		t.Fatalf("fixed/pool wallet with manual member must never auto-clear; got %v", got)
	}
}

func TestDetectSharedWalletPlatformsRequiresEveryRiskPathMemberLegacyPct(t *testing.T) {
	t.Setenv("HYPERLIQUID_ACCOUNT_ADDRESS", "0xshared")
	livePerps := func(id string, pct float64, pooled bool) StrategyConfig {
		return StrategyConfig{
			ID: id, Platform: "hyperliquid", Type: "perps",
			CapitalPct: pct, Args: []string{"sma", "BTC", "1h", "--mode=live"},
			sharedWalletPoolBudget: pooled,
		}
	}
	liveManual := func(id string) StrategyConfig {
		return StrategyConfig{
			ID: id, Platform: "hyperliquid", Type: "manual", CapitalPct: 0.5,
			Args: []string{"hold", "ETH", "1h", "--mode=live"},
		}
	}

	tests := []struct {
		name       string
		strategies []StrategyConfig
		want       bool
	}{
		{
			name: "pooled perps cannot be masked by percentage manuals",
			strategies: []StrategyConfig{
				livePerps("pool-a", 0, true),
				livePerps("pool-b", 0, true),
				liveManual("manual-a"),
				liveManual("manual-b"),
			},
		},
		{
			name: "all percentage perps and manual remain eligible",
			strategies: []StrategyConfig{
				livePerps("pct-a", 0.5, false),
				livePerps("pct-b", 0.5, false),
				liveManual("manual"),
			},
			want: true,
		},
		{
			name: "one pooled perps member suppresses auto-clear",
			strategies: []StrategyConfig{
				livePerps("pool", 0, true),
				livePerps("pct", 0.5, false),
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := detectSharedWalletPlatforms(tt.strategies)
			if tt.want {
				if len(got) != 1 || got[0] != "hyperliquid" {
					t.Fatalf("expected eligible Hyperliquid wallet, got %v", got)
				}
				return
			}
			if len(got) != 0 {
				t.Fatalf("unsafe mixed wallet must not auto-clear a kill switch, got %v", got)
			}
		})
	}
}

func TestCheckPortfolioRisk_AllPerps_MarginDrawdownWarnsWithoutLatch(t *testing.T) {
	cfg := &PortfolioRiskConfig{MaxDrawdownPct: 25, WarnThresholdPct: 80}
	prs := &PortfolioRiskState{PeakValue: 10000}

	totalValue := 9500.0
	perpsLoss := 500.0
	perpsMargin := 1000.0

	allowed, notionalBlocked, warning, reason := CheckPortfolioRisk(prs, cfg, totalValue, 0, perpsLoss, perpsMargin)
	if !allowed {
		t.Errorf("expected 50%% margin drawdown at 5%% equity drawdown to be allowed; got allowed=false, reason=%s", reason)
	}
	if prs.KillSwitchActive {
		t.Error("expected KillSwitchActive=false — margin drawdown must not latch the portfolio while the equity guard is armed")
	}
	if notionalBlocked {
		t.Error("expected notionalBlocked=false — the margin signal never holds opens")
	}
	if !warning {
		t.Errorf("expected warning=true so the operator still sees the margin blow-up; reason=%q", reason)
	}
	if !strings.Contains(reason, "margin") {
		t.Errorf("expected reason to reference perps margin drawdown; got %q", reason)
	}
	if !strings.Contains(reason, "exceeds") {
		t.Errorf("expected reason to say the margin limit is exceeded, not approached; got %q", reason)
	}
	if prs.CurrentDrawdownPct < 4.9 || prs.CurrentDrawdownPct > 5.1 {
		t.Errorf("expected CurrentDrawdownPct (equity)≈5%%; got %.2f", prs.CurrentDrawdownPct)
	}
	if prs.CurrentMarginDrawdownPct < 49.9 || prs.CurrentMarginDrawdownPct > 50.1 {
		t.Errorf("expected CurrentMarginDrawdownPct≈50%%; got %.2f", prs.CurrentMarginDrawdownPct)
	}
	if len(prs.Events) != 0 {
		t.Fatalf("expected no kill-switch events on a warning-only cycle; got %+v", prs.Events)
	}
	if !prs.WarningSent {
		t.Error("expected WarningSent=true")
	}
	if prs.LastWarningMarginDDPct < 49.9 || prs.LastWarningMarginDDPct > 50.1 {
		t.Errorf("expected LastWarningMarginDDPct≈50%%; got %.2f", prs.LastWarningMarginDDPct)
	}
}

func TestCheckPortfolioRiskMissingPooledEquitySuppressesOnlyEquityArm(t *testing.T) {
	cfg := &PortfolioRiskConfig{MaxDrawdownPct: 25, WarnThresholdPct: 80}
	prs := &PortfolioRiskState{PeakValue: 10000, CurrentDrawdownPct: 7}

	allowed, _, warning, reason := checkPortfolioRiskWithEquityAvailability(prs, cfg, 0, 0, 0, 0, false, false)
	if !allowed || warning || reason != "" || prs.KillSwitchActive {
		t.Fatalf("missing equity must not false-fire: allowed=%v warning=%v reason=%q state=%+v", allowed, warning, reason, prs)
	}
	if prs.PeakValue != 10000 || prs.CurrentDrawdownPct != 7 {
		t.Fatalf("missing equity must preserve the last valid equity tuple: %+v", prs)
	}

	allowed, _, _, reason = checkPortfolioRiskWithEquityAvailability(prs, cfg, 0, 0, 300, 1000, false, false)
	if allowed || !prs.KillSwitchActive || !strings.Contains(reason, "equity unavailable") {
		t.Fatalf("margin blow-up must still fire without equity: allowed=%v reason=%q state=%+v", allowed, reason, prs)
	}
	if len(prs.Events) != 1 || prs.Events[0].Type != "triggered" || prs.Events[0].Source != "margin" {
		t.Fatalf("expected one triggered event with Source=margin; got %+v", prs.Events)
	}
}

func TestCheckPortfolioRisk_LatchSource(t *testing.T) {
	type pctRange struct{ lo, hi float64 }
	cases := []struct {
		name         string
		peak         float64
		totalValue   float64
		perpsLoss    float64
		perpsMargin  float64
		wantSource   string
		reasonMargin bool
		equityDD     *pctRange
		marginDD     *pctRange
		eventDD      *pctRange
	}{
		{
			name: "mixed account: spot equity drawdown still latches",
			peak: 10000, totalValue: 7000, perpsLoss: 0, perpsMargin: 500,
			wantSource: "equity",
		},
		{
			name: "mixed account: equity governs when both breach",
			peak: 10000, totalValue: 7000, perpsLoss: 600, perpsMargin: 1000,
			wantSource: "equity",
			equityDD:   &pctRange{29.9, 30.1}, marginDD: &pctRange{59.9, 60.1}, eventDD: &pctRange{29.9, 30.1},
		},
		{
			name: "cold-start peak zero: margin can still fire",
			peak: 0, totalValue: 0, perpsLoss: 500, perpsMargin: 1000,
			wantSource: "margin", reasonMargin: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &PortfolioRiskConfig{MaxDrawdownPct: 25, WarnThresholdPct: 80}
			prs := &PortfolioRiskState{PeakValue: tc.peak}

			allowed, _, _, reason := CheckPortfolioRisk(prs, cfg, tc.totalValue, 0, tc.perpsLoss, tc.perpsMargin)
			if allowed {
				t.Errorf("expected kill switch to fire; got allowed=true reason=%q", reason)
			}
			if !prs.KillSwitchActive {
				t.Error("expected KillSwitchActive=true")
			}
			if strings.Contains(reason, "margin") != tc.reasonMargin {
				t.Errorf("reason mentions margin = %v, want %v; got %q", !tc.reasonMargin, tc.reasonMargin, reason)
			}
			if len(prs.Events) != 1 {
				t.Fatalf("expected exactly one event; got %+v", prs.Events)
			}
			if prs.Events[0].Source != tc.wantSource {
				t.Errorf("triggered event Source = %q, want %q", prs.Events[0].Source, tc.wantSource)
			}
			check := func(label string, got float64, want *pctRange) {
				if want != nil && (got < want.lo || got > want.hi) {
					t.Errorf("%s = %.2f, want within [%.1f, %.1f]", label, got, want.lo, want.hi)
				}
			}
			check("CurrentDrawdownPct", prs.CurrentDrawdownPct, tc.equityDD)
			check("CurrentMarginDrawdownPct", prs.CurrentMarginDrawdownPct, tc.marginDD)
			check("Events[0].DrawdownPct", prs.Events[0].DrawdownPct, tc.eventDD)
		})
	}
}

func TestCheckPortfolioRisk_Incident1448_MarginTripAvertedWhenEquityHealthy(t *testing.T) {
	cfg := &PortfolioRiskConfig{MaxDrawdownPct: 30, WarnThresholdPct: 80}
	prs := &PortfolioRiskState{PeakValue: 1014.25}

	allowed, notionalBlocked, warning, reason := CheckPortfolioRisk(prs, cfg, 914.97, 0, 31.62, 48.42)
	if !allowed {
		t.Fatalf("the live incident must no longer latch the book: allowed=false, reason=%s", reason)
	}
	if prs.KillSwitchActive {
		t.Fatal("expected KillSwitchActive=false at 9.8% equity drawdown against a 30% limit")
	}
	if notionalBlocked {
		t.Error("expected notionalBlocked=false")
	}
	if !warning {
		t.Errorf("expected the margin blow-up to still warn the operator; reason=%q", reason)
	}
	if !strings.Contains(reason, "margin") || !strings.Contains(reason, "exceeds") {
		t.Errorf("expected a margin reason that says the margin limit is exceeded; got %q", reason)
	}
	if !strings.Contains(reason, "#1448") {
		t.Errorf("expected the reason to point at #1448 so an operator can find the rationale; got %q", reason)
	}
	if len(prs.Events) != 0 {
		t.Fatalf("expected no kill-switch events; got %+v", prs.Events)
	}
	if prs.CurrentDrawdownPct < 9.7 || prs.CurrentDrawdownPct > 9.9 {
		t.Errorf("expected equity drawdown≈9.8%%; got %.2f", prs.CurrentDrawdownPct)
	}
	if prs.CurrentMarginDrawdownPct < 65.2 || prs.CurrentMarginDrawdownPct > 65.4 {
		t.Errorf("expected margin drawdown≈65.3%%; got %.2f", prs.CurrentMarginDrawdownPct)
	}

	allowed, _, _, reason = CheckPortfolioRisk(prs, cfg, 700, 0, 31.62, 48.42)
	if allowed || !prs.KillSwitchActive {
		t.Fatalf("equity drawdown above the limit must still latch: allowed=%v reason=%q", allowed, reason)
	}
	if len(prs.Events) != 1 || prs.Events[0].Source != "equity" {
		t.Fatalf("expected one triggered event with Source=equity; got %+v", prs.Events)
	}
	if prs.Events[0].DrawdownPct < 30.9 || prs.Events[0].DrawdownPct > 31.1 {
		t.Errorf("expected event DrawdownPct≈31%% (equity signal); got %.2f", prs.Events[0].DrawdownPct)
	}
}

func TestCheckPortfolioRisk_MarginAboveLimit_WarnBookkeepingAcrossCycles(t *testing.T) {
	cfg := &PortfolioRiskConfig{MaxDrawdownPct: 30, WarnThresholdPct: 80}
	prs := &PortfolioRiskState{PeakValue: 10000}

	_, _, warning, reason := CheckPortfolioRisk(prs, cfg, 10000, 0, 400, 1000)
	if !warning || prs.KillSwitchActive {
		t.Fatalf("cycle 1: expected warning without latch; warning=%v active=%v reason=%q", warning, prs.KillSwitchActive, reason)
	}
	if !prs.WarningSent {
		t.Fatal("cycle 1: expected WarningSent=true")
	}
	entered := prs.WarnBandEnteredAt
	if entered.IsZero() {
		t.Fatal("cycle 1: expected WarnBandEnteredAt to be stamped on entry")
	}
	if prs.WarningMarginDeltaPct != 0 {
		t.Errorf("cycle 1: expected zero delta on band entry; got %.2f", prs.WarningMarginDeltaPct)
	}
	if prs.LastWarningMarginDDPct < 39.9 || prs.LastWarningMarginDDPct > 40.1 {
		t.Errorf("cycle 1: expected LastWarningMarginDDPct≈40%%; got %.2f", prs.LastWarningMarginDDPct)
	}

	_, _, warning, reason = CheckPortfolioRisk(prs, cfg, 10000, 0, 500, 1000)
	if !warning || prs.KillSwitchActive {
		t.Fatalf("cycle 2: expected warning without latch; warning=%v active=%v reason=%q", warning, prs.KillSwitchActive, reason)
	}
	if !prs.WarnBandEnteredAt.Equal(entered) {
		t.Errorf("cycle 2: WarnBandEnteredAt must not be re-stamped while in band; got %v want %v", prs.WarnBandEnteredAt, entered)
	}
	if prs.WarningMarginDeltaPct < 9.9 || prs.WarningMarginDeltaPct > 10.1 {
		t.Errorf("cycle 2: expected WarningMarginDeltaPct≈+10; got %.2f", prs.WarningMarginDeltaPct)
	}
	if prs.LastWarningMarginDDPct < 49.9 || prs.LastWarningMarginDDPct > 50.1 {
		t.Errorf("cycle 2: expected LastWarningMarginDDPct≈50%%; got %.2f", prs.LastWarningMarginDDPct)
	}

	_, _, warning, _ = CheckPortfolioRisk(prs, cfg, 10000, 0, 100, 1000)
	if warning {
		t.Error("cycle 3: expected warning=false below the warn threshold")
	}
	if prs.WarningSent || !prs.WarnBandEnteredAt.IsZero() || prs.LastWarningMarginDDPct != 0 || prs.WarningMarginDeltaPct != 0 {
		t.Errorf("cycle 3: expected the warn band to clear; got %+v", prs)
	}
	if prs.KillSwitchActive {
		t.Error("cycle 3: expected no latch at any point in this sequence")
	}
}

func TestCheckPortfolioRisk_AfterManualMarkBasisRebaseline_MarginDoesNotLatch(t *testing.T) {
	newPeak, ok := manualMarkBasisPeakAdjustment(1000, 950, 1000)
	if !ok {
		t.Fatalf("expected the #1444 basis migration to apply; got ok=false, peak=%.2f", newPeak)
	}
	if newPeak < 949.9 || newPeak > 950.1 {
		t.Fatalf("expected migrated peak≈950; got %.2f", newPeak)
	}

	cfg := &PortfolioRiskConfig{MaxDrawdownPct: 30, WarnThresholdPct: 80}
	prs := &PortfolioRiskState{PeakValue: newPeak, ManualMarkBasisRebaselined: true}

	allowed, _, warning, reason := CheckPortfolioRisk(prs, cfg, 940, 0, 200, 300)
	if !allowed || prs.KillSwitchActive {
		t.Fatalf("margin drawdown must not latch against a migrated peak: allowed=%v active=%v reason=%q", allowed, prs.KillSwitchActive, reason)
	}
	if !warning {
		t.Errorf("expected the margin signal to still warn; reason=%q", reason)
	}
	if len(prs.Events) != 0 {
		t.Fatalf("expected no kill-switch events; got %+v", prs.Events)
	}

	allowed, _, _, reason = CheckPortfolioRisk(prs, cfg, 600, 0, 200, 300)
	if allowed || !prs.KillSwitchActive {
		t.Fatalf("equity drawdown above the limit must still latch after migration: allowed=%v reason=%q", allowed, reason)
	}
	if len(prs.Events) != 1 || prs.Events[0].Source != "equity" {
		t.Fatalf("expected one triggered event with Source=equity; got %+v", prs.Events)
	}
	if prs.Events[0].PeakValue < 949.9 || prs.Events[0].PeakValue > 950.1 {
		t.Errorf("expected the event to record the migrated peak≈950; got %.2f", prs.Events[0].PeakValue)
	}
	if prs.Events[0].DrawdownPct < 36.7 || prs.Events[0].DrawdownPct > 36.9 {
		t.Errorf("expected event DrawdownPct≈36.8%%; got %.2f", prs.Events[0].DrawdownPct)
	}
}

func TestCheckPortfolioRisk_NoPerps_EquityBehaviorUnchanged(t *testing.T) {
	cfg := &PortfolioRiskConfig{MaxDrawdownPct: 25, WarnThresholdPct: 80}
	prs := &PortfolioRiskState{PeakValue: 10000}

	allowed, _, _, _ := CheckPortfolioRisk(prs, cfg, 8000, 0, 0, 0)
	if !allowed {
		t.Error("expected allowed=true at 20%% equity drawdown with no perps")
	}

	allowed, _, _, reason := CheckPortfolioRisk(prs, cfg, 7400, 0, 0, 0)
	if allowed {
		t.Error("expected kill switch at 26%% equity drawdown")
	}
	if strings.Contains(reason, "margin") {
		t.Errorf("expected equity-drawdown reason (no perps deployed); got %q", reason)
	}
}

func TestCheckPortfolioRisk_MarginWarningReasons(t *testing.T) {
	cases := []struct {
		name            string
		totalValue      float64
		perpsLoss       float64
		perpsMargin     float64
		cycles          int
		wantWarning     bool
		wantContains    []string
		wantNotContains []string
		marginDDLo      float64
		marginDDHi      float64
	}{
		{
			name:       "margin drawdown in warn band warns on every cycle without latch",
			totalValue: 10000, perpsLoss: 210, perpsMargin: 1000, cycles: 2,
			wantWarning: true, wantContains: []string{"margin"},
			marginDDLo: 20.9, marginDDHi: 21.1,
		},
		{
			name:       "margin drawdown below warn band populates the field without warning",
			totalValue: 10000, perpsLoss: 100, perpsMargin: 1000, cycles: 1,
			marginDDLo: 9.9, marginDDHi: 10.1,
		},
		{
			name:       "both signals in warn band name both",
			totalValue: 7800, perpsLoss: 230, perpsMargin: 1000, cycles: 1,
			wantWarning: true, wantContains: []string{"equity=", "margin="},
			marginDDLo: 22.9, marginDDHi: 23.1,
		},
		{
			name:       "margin above limit while equity warns names equity governance (#1448)",
			totalValue: 7800, perpsLoss: 400, perpsMargin: 1000, cycles: 1,
			wantWarning:     true,
			wantContains:    []string{"equity=", "margin=", "exceeds limit", "#1448"},
			wantNotContains: []string{"approaching"},
			marginDDLo:      39.9, marginDDHi: 40.1,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &PortfolioRiskConfig{MaxDrawdownPct: 25, WarnThresholdPct: 80}
			prs := &PortfolioRiskState{PeakValue: 10000}
			for i := 0; i < tc.cycles; i++ {
				allowed, _, warning, reason := CheckPortfolioRisk(prs, cfg, tc.totalValue, 0, tc.perpsLoss, tc.perpsMargin)
				if !allowed || prs.KillSwitchActive {
					t.Fatalf("cycle %d: expected no latch; allowed=%v active=%v reason=%q", i, allowed, prs.KillSwitchActive, reason)
				}
				if warning != tc.wantWarning {
					t.Fatalf("cycle %d: warning = %v, want %v; reason=%q", i, warning, tc.wantWarning, reason)
				}
				for _, want := range tc.wantContains {
					if !strings.Contains(reason, want) {
						t.Errorf("cycle %d: expected reason to contain %q; got %q", i, want, reason)
					}
				}
				for _, unwanted := range tc.wantNotContains {
					if strings.Contains(reason, unwanted) {
						t.Errorf("cycle %d: reason must not contain %q; got %q", i, unwanted, reason)
					}
				}
			}
			if prs.CurrentMarginDrawdownPct < tc.marginDDLo || prs.CurrentMarginDrawdownPct > tc.marginDDHi {
				t.Errorf("CurrentMarginDrawdownPct = %.2f, want within [%.1f, %.1f]", prs.CurrentMarginDrawdownPct, tc.marginDDLo, tc.marginDDHi)
			}
		})
	}
}

func TestBuildPortfolioWarningMessage_IncludesTriageSections(t *testing.T) {
	now := time.Date(2026, 6, 6, 6, 5, 0, 0, time.UTC)
	state := &AppState{
		PortfolioRisk: map[PortfolioScope]*PortfolioRiskState{ScopeLive: {
			PeakValue:                10060,
			CurrentDrawdownPct:       16.5,
			CurrentMarginDrawdownPct: 18.2,
			WarningSent:              true,
			WarnBandEnteredAt:        now.Add(-18 * time.Minute),
			WarningEquityDeltaPct:    1.2,
			WarningMarginDeltaPct:    0.8,
		}},
		Strategies: map[string]*StrategyState{
			"hl-btc-sma-30": {
				ID:             "hl-btc-sma-30",
				Type:           "perps",
				Cash:           1000,
				InitialCapital: 1500,
				RiskState:      RiskState{CurrentDrawdownPct: 9.1},
				Positions: map[string]*Position{
					"BTC": {Symbol: "BTC", Quantity: 0.5, AvgCost: 67800, Side: "short", Multiplier: 1},
				},
				OptionPositions: map[string]*OptionPosition{},
			},
			"hl-eth-ema-30": {
				ID:              "hl-eth-ema-30",
				Type:            "perps",
				Cash:            905,
				InitialCapital:  1000,
				RiskState:       RiskState{CurrentDrawdownPct: 4.1},
				Positions:       map[string]*Position{},
				OptionPositions: map[string]*OptionPosition{},
			},
		},
	}
	msg := BuildPortfolioWarningMessage(PortfolioWarningMessageInputs{
		Config:      &PortfolioRiskConfig{MaxDrawdownPct: 25, WarnThresholdPct: 60},
		State:       state,
		Prices:      map[string]float64{"BTC": 68080},
		TotalValue:  8400,
		PerpsLoss:   250,
		PerpsMargin: 1500,
		Recent: []Trade{
			{Timestamp: now.Add(-14 * time.Minute), StrategyID: "hl-btc-sma-30", Symbol: "BTC", Side: "sell", Quantity: 0.5, Price: 67800, TradeType: "perps", Details: "signal flip"},
		},
		Now:              now,
		EquityGuardArmed: true,
	})

	for _, want := range []string{
		"**PORTFOLIO WARNING**",
		"Kill switch: 25.0% drawdown | Warn threshold: 15.0%",
		"In band since: 2026-06-06 05:47 UTC (18m)",
		"Current: equity=16.5% ($8400 / peak $10060) | perps margin=18.2% ($250 loss on $1500 margin)",
		"Distance to kill switch: 8.5% equity | perps margin 6.8% from limit",
		"Trend: WORSENING - equity dd +1.2% since last cycle; margin dd +0.8%",
		"Top contributors:",
		"```",
		"hl-btc-sma-30",
		"pos: short 0.5 BTC @ $67800 (-$140 unrealized)",
		"Recent activity (last 15m):",
		"05:51  perps  hl-btc-sma-30",
		"Recommended:",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("warning message missing %q:\n%s", want, msg)
		}
	}
	if len(msg) >= 2000 {
		t.Fatalf("warning message len = %d, want under Discord limit; msg:\n%s", len(msg), msg)
	}
}

func TestBuildPortfolioWarningMessage_DailyPnLFallbackLabel(t *testing.T) {
	now := time.Date(2026, 6, 6, 6, 5, 0, 0, time.UTC)
	state := &AppState{
		PortfolioRisk: map[PortfolioScope]*PortfolioRiskState{ScopeLive: {
			PeakValue:          1000,
			CurrentDrawdownPct: 20,
			WarningSent:        true,
			WarnBandEnteredAt:  now.Add(-5 * time.Minute),
		}},
		Strategies: map[string]*StrategyState{
			"no-initial-cap": {
				ID:              "no-initial-cap",
				Cash:            900,
				InitialCapital:  0,
				RiskState:       RiskState{DailyPnL: -75, CurrentDrawdownPct: 7.5},
				Positions:       map[string]*Position{},
				OptionPositions: map[string]*OptionPosition{},
			},
		},
	}
	msg := BuildPortfolioWarningMessage(PortfolioWarningMessageInputs{
		Config:           &PortfolioRiskConfig{MaxDrawdownPct: 25, WarnThresholdPct: 60},
		State:            state,
		TotalValue:       800,
		Now:              now,
		EquityGuardArmed: true,
	})
	if !strings.Contains(msg, "daily P&L -$75") {
		t.Fatalf("expected daily P&L fallback label in warning message:\n%s", msg)
	}
}

func TestBuildPortfolioWarningMessage_PoolIgnoresStaleInitialCapital(t *testing.T) {
	state := &AppState{Strategies: map[string]*StrategyState{
		"hl-pool": {
			ID: "hl-pool", Type: "perps",
			InitialCapital:              1000,
			SharedWalletPoolBudget:      true,
			SharedWalletPerformanceOnly: true,
			SharedWalletValueSet:        true,
			SharedWalletValue:           -75,
			Positions:                   map[string]*Position{},
			OptionPositions:             map[string]*OptionPosition{},
		},
	}}
	msg := BuildPortfolioWarningMessage(PortfolioWarningMessageInputs{
		Config:           &PortfolioRiskConfig{MaxDrawdownPct: 25, WarnThresholdPct: 60},
		State:            state,
		EquityGuardArmed: true,
	})
	if !strings.Contains(msg, "net P&L") || !strings.Contains(msg, "-$75") {
		t.Fatalf("expected pool net P&L without stale baseline:\n%s", msg)
	}
	if strings.Contains(msg, "-$1075") {
		t.Fatalf("stale initial capital leaked into pool warning:\n%s", msg)
	}
}

func TestTruncateWarningField_UTF8Safe(t *testing.T) {
	got := truncateWarningField("alpha-éclair", 9)
	if !utf8.ValidString(got) {
		t.Fatalf("truncateWarningField returned invalid UTF-8: %q", got)
	}
	if got != "alpha-..." {
		t.Fatalf("truncateWarningField = %q, want %q", got, "alpha-...")
	}
}

func TestRiskState_PendingCircuitClose_Marshal_EmptyReturnsBlank(t *testing.T) {
	cases := []struct {
		name string
		r    *RiskState
	}{
		{"nil receiver", nil},
		{"nil map", &RiskState{}},
		{"empty map", &RiskState{PendingCircuitCloses: map[string]*PendingCircuitClose{}}},
		{"entry with nil value", &RiskState{PendingCircuitCloses: map[string]*PendingCircuitClose{"hyperliquid": nil}}},
		{"entry with empty symbols", &RiskState{PendingCircuitCloses: map[string]*PendingCircuitClose{
			"hyperliquid": {Symbols: nil},
		}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := tc.r.MarshalPendingCircuitClosesJSON()
			if got != "" {
				t.Errorf("expected empty marshal for %s; got %q", tc.name, got)
			}
		})
	}
}

func TestRiskState_PendingCircuitClose_RoundTrip(t *testing.T) {
	notifiedAt := time.Date(2026, 4, 27, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		name    string
		pending map[string]*PendingCircuitClose
	}{
		{"two symbols on one platform", map[string]*PendingCircuitClose{
			PlatformPendingCloseHyperliquid: {Symbols: []PendingCircuitCloseSymbol{
				{Symbol: "ETH", Size: 0.2585},
				{Symbol: "BTC", Size: 0.01},
			}},
		}},
		{"multi platform", map[string]*PendingCircuitClose{
			"hyperliquid": {Symbols: []PendingCircuitCloseSymbol{{Symbol: "ETH", Size: 0.1}}},
			"okx":         {Symbols: []PendingCircuitCloseSymbol{{Symbol: "BTC-USDT-SWAP", Size: 0.01}}},
		}},
		{"consecutive failures and last notified", map[string]*PendingCircuitClose{
			PlatformPendingCloseHyperliquid: {
				Symbols:             []PendingCircuitCloseSymbol{{Symbol: "ETH", Size: 0.25}},
				ConsecutiveFailures: 7,
				LastNotifiedAt:      notifiedAt,
			},
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			src := &RiskState{PendingCircuitCloses: tc.pending}
			blob := src.MarshalPendingCircuitClosesJSON()
			if blob == "" {
				t.Fatal("non-empty marshal expected")
			}
			var dst RiskState
			dst.UnmarshalPendingCircuitClosesJSON(blob)
			for platform, want := range tc.pending {
				got := dst.getPendingCircuitClose(platform)
				if got == nil || len(got.Symbols) != len(want.Symbols) {
					t.Fatalf("%s entry lost in round-trip: %+v", platform, got)
				}
				byName := map[string]float64{}
				for _, s := range got.Symbols {
					byName[s.Symbol] = s.Size
				}
				for _, s := range want.Symbols {
					if byName[s.Symbol] != s.Size {
						t.Errorf("%s size for %s = %g, want %g", platform, s.Symbol, byName[s.Symbol], s.Size)
					}
				}
				if got.ConsecutiveFailures != want.ConsecutiveFailures {
					t.Errorf("%s ConsecutiveFailures = %d, want %d", platform, got.ConsecutiveFailures, want.ConsecutiveFailures)
				}
				if !got.LastNotifiedAt.Equal(want.LastNotifiedAt) {
					t.Errorf("%s LastNotifiedAt = %v, want %v", platform, got.LastNotifiedAt, want.LastNotifiedAt)
				}
			}
		})
	}
}

func TestRiskState_PendingCircuitClose_Unmarshal(t *testing.T) {
	seeded := func() RiskState {
		return RiskState{PendingCircuitCloses: map[string]*PendingCircuitClose{
			PlatformPendingCloseHyperliquid: {Symbols: []PendingCircuitCloseSymbol{{Symbol: "ETH", Size: 1}}},
		}}
	}
	cases := []struct {
		name       string
		start      RiskState
		blob       string
		wantNilMap bool
		wantSymbol string
		wantSize   float64
	}{
		{"legacy hl coins shape converts", RiskState{}, `{"coins":[{"coin":"ETH","sz":0.2585}]}`, false, "ETH", 0.2585},
		{"legacy row defaults zero consecutive failures", RiskState{}, `{"hyperliquid":{"symbols":[{"symbol":"ETH","size":0.25}]}}`, false, "ETH", 0.25},
		{"empty blob clears", seeded(), "", true, "", 0},
		{"malformed blob clears", seeded(), `not-json{`, true, "", 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := tc.start
			r.UnmarshalPendingCircuitClosesJSON(tc.blob)
			if tc.wantNilMap {
				if r.PendingCircuitCloses != nil {
					t.Errorf("expected nil map after unmarshal; got %+v", r.PendingCircuitCloses)
				}
				return
			}
			p := r.getPendingCircuitClose(PlatformPendingCloseHyperliquid)
			if p == nil || len(p.Symbols) != 1 {
				t.Fatalf("entry not loaded: %+v", p)
			}
			if p.Symbols[0].Symbol != tc.wantSymbol || p.Symbols[0].Size != tc.wantSize {
				t.Errorf("got symbol=%q size=%g, want %q/%g", p.Symbols[0].Symbol, p.Symbols[0].Size, tc.wantSymbol, tc.wantSize)
			}
			if p.ConsecutiveFailures != 0 {
				t.Errorf("legacy row must default ConsecutiveFailures=0, got %d", p.ConsecutiveFailures)
			}
			if !p.LastNotifiedAt.IsZero() {
				t.Errorf("legacy row must default LastNotifiedAt=zero, got %v", p.LastNotifiedAt)
			}
		})
	}
}

func TestRiskState_PendingCircuitClose_SetClearGet(t *testing.T) {
	var r RiskState

	if r.getPendingCircuitClose("hyperliquid") != nil {
		t.Fatal("expected nil for unset key")
	}

	r.setPendingCircuitClose("hyperliquid", &PendingCircuitClose{
		Symbols: []PendingCircuitCloseSymbol{{Symbol: "ETH", Size: 0.5}},
	})
	if got := r.getPendingCircuitClose("hyperliquid"); got == nil || got.Symbols[0].Size != 0.5 {
		t.Errorf("setter did not store value: %+v", got)
	}

	r.setPendingCircuitClose("hyperliquid", &PendingCircuitClose{Symbols: nil})
	if r.getPendingCircuitClose("hyperliquid") != nil {
		t.Error("empty-symbols set should have cleared entry")
	}
	if r.PendingCircuitCloses != nil {
		t.Error("map should be nil after last entry cleared")
	}

	r.clearPendingCircuitClose("hyperliquid")
}

func TestCheckRisk_ManualStrategyAlwaysAllowed(t *testing.T) {
	sc := &StrategyConfig{ID: "hl-manual-eth-live", Type: "manual", Platform: "hyperliquid", Symbol: "ETH", Leverage: 10}
	s := &StrategyState{Type: "manual", RiskState: RiskState{PeakValue: 100, MaxDrawdownPct: 60}}
	allowed, reason := CheckRisk(sc, s, 5.0, nil, nil, nil)
	if !allowed {
		t.Errorf("manual strategy should always pass CheckRisk, got reason=%q", reason)
	}
	if reason != "" {
		t.Errorf("expected empty reason, got %q", reason)
	}
	if s.RiskState.CircuitBreaker {
		t.Error("CheckRisk must not set CircuitBreaker for manual strategy")
	}
}

func TestFormatPerStrategyCircuitBreakerBlock_IncludesTriageSections(t *testing.T) {
	now := time.Date(2026, 6, 6, 6, 8, 0, 0, time.UTC)
	sc := StrategyConfig{
		ID:         "hl-btc-sma-30",
		Type:       "perps",
		Platform:   "hyperliquid",
		Args:       []string{"sma_cross", "BTC", "30m", "--mode=live"},
		Leverage:   5,
		CapitalPct: 0.417,
	}
	state := &StrategyState{
		ID:              sc.ID,
		Type:            sc.Type,
		Platform:        sc.Platform,
		Cash:            4200,
		InitialCapital:  4500,
		Positions:       map[string]*Position{},
		OptionPositions: map[string]*OptionPosition{},
		RiskState: RiskState{
			PeakValue:           4500,
			CurrentDrawdownPct:  8.2,
			MaxDrawdownPct:      5,
			ConsecutiveLosses:   5,
			CircuitBreaker:      true,
			CircuitBreakerUntil: now.Add(24 * time.Hour),
			PendingCircuitCloses: map[string]*PendingCircuitClose{
				PlatformPendingCloseOKXSpot: {
					Symbols:          []PendingCircuitCloseSymbol{{Symbol: "BTC-USDT", Size: 0.5}},
					OperatorRequired: true,
				},
			},
		},
		ClosedPositions: []ClosedPosition{
			{StrategyID: sc.ID, Symbol: "BTC", Quantity: 0.5, Side: "short", ClosePrice: 67800, RealizedPnL: -180, CloseReason: "circuit_breaker"},
		},
	}
	snap := snapshotPerStrategyCircuitBreaker(state, map[string]float64{"BTC": 67800})
	snap.Now = now
	msg := formatPerStrategyCircuitBreakerBlock(perStrategyCircuitBreakerFormatInput{
		Strategy:            sc,
		Snapshot:            snap,
		Reason:              RiskReasonMaxDrawdownExceeded + " (8.2% > 5.0%, portfolio=$4200.00 peak=$4500.00, denom=margin=$300.00)",
		StrategyValue:       4200,
		TotalPortfolioValue: 10060,
		RecentTrades: []Trade{
			{Timestamp: now.Add(-7 * time.Minute), StrategyID: sc.ID, Symbol: "BTC", Side: "buy", Quantity: 0.5, Price: 67800, IsClose: true, RealizedPnL: -180, Details: "Circuit breaker close short, PnL: $-180.00"},
		},
	})

	for _, want := range []string{
		"**CIRCUIT BREAKER** [hl-btc-sma-30] - Hyperliquid, BTC, 30m, sma_cross, perps",
		"Trigger: max drawdown exceeded - 8.2% > 5.0% (denom: margin=$300.00)",
		"Cooldown: 1d0h (until 2026-06-07 06:08 UTC)",
		"Portfolio impact: ~$4195 of ~$10060 (41.7%)",
		"Perps context: 5x leverage, margin deployed=$300.00",
		"Positions force-closed:",
		"short 0.5 BTC @ $67800  P&L -$180",
		"Pending operator closes:",
		"okx_spot BTC-USDT size 0.5 (manual flatten required)",
		"Recent trades:",
		"06:01  close  buy 0.5 BTC @ $67800 P&L -$180",
		"Reason: Investigate whether the signal is still valid or the regime has flipped.",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("circuit-breaker message missing %q:\n%s", want, msg)
		}
	}
	if header, _, _ := strings.Cut(msg, "\n"); strings.Contains(header, "leverage") {
		t.Fatalf("circuit-breaker header should not duplicate leverage context: %q", header)
	}
	if len(msg) >= 2000 {
		t.Fatalf("circuit-breaker message len = %d, want under Discord limit; msg:\n%s", len(msg), msg)
	}
}

func TestCircuitBreakerStrategyLabel(t *testing.T) {
	cases := []struct {
		name            string
		sc              StrategyConfig
		wantContains    []string
		wantNotContains []string
	}{
		{
			name: "skips flag as timeframe",
			sc: StrategyConfig{ID: "deribit-btc-options", Type: "options", Platform: "deribit",
				Args: []string{"wheel", "BTC", "--platform=deribit"}},
			wantContains:    []string{"Deribit", "BTC", "wheel", "options"},
			wantNotContains: []string{"--platform=deribit"},
		},
		{
			name: "strips spot quote suffix",
			sc: StrategyConfig{ID: "spot-btc", Type: "spot", Platform: "binanceus",
				Args: []string{"sma_cross", "BTC/USDT", "30m"}},
			wantContains:    []string{"BinanceUS, BTC, 30m, sma_cross, spot"},
			wantNotContains: []string{"BTC/USDT"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := circuitBreakerStrategyLabel(tc.sc)
			for _, want := range tc.wantContains {
				if !strings.Contains(got, want) {
					t.Errorf("strategy label missing %q: %q", want, got)
				}
			}
			for _, unwanted := range tc.wantNotContains {
				if strings.Contains(got, unwanted) {
					t.Errorf("strategy label must not contain %q: %q", unwanted, got)
				}
			}
		})
	}
}

func TestForceCloseAllPositions_TradeType_PerpsVsFutures(t *testing.T) {
	cases := []struct {
		name       string
		platform   string
		stratType  string
		multiplier float64
		want       string
	}{
		{"hl-perps-multiplier-1", "hyperliquid", "perps", 1, "perps"},
		{"hl-manual-multiplier-1", "hyperliquid", "manual", 1, "perps"},
		{"okx-perps-multiplier-1", "okx", "perps", 1, "perps"},
		{"hl-perps-multiplier-0", "hyperliquid", "perps", 0, "spot"},
		{"spot-multiplier-0", "binanceus", "spot", 0, "spot"},
		{"topstep-futures-multiplier-50", "topstep", "futures", 50, "futures"},
		{"legacy-futures-multiplier-5", "ibkr", "futures", 5, "futures"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := &StrategyState{
				ID:       "test-" + tc.name,
				Platform: tc.platform,
				Type:     tc.stratType,
				Positions: map[string]*Position{
					"BTC": {Symbol: "BTC", Quantity: 0.1, AvgCost: 50000, Side: "long", Multiplier: tc.multiplier},
				},
				TradeHistory: []Trade{},
				RiskState:    RiskState{},
			}
			forceCloseAllPositions(s, nil, map[string]float64{"BTC": 51000}, nil)
			if len(s.TradeHistory) != 1 {
				t.Fatalf("TradeHistory len = %d, want 1", len(s.TradeHistory))
			}
			if got := s.TradeHistory[0].TradeType; got != tc.want {
				t.Errorf("TradeType = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestCircuitBreakerPermitsManagement(t *testing.T) {
	cases := []struct {
		name      string
		reason    string
		platform  string
		stratType string
		posQty    float64
		want      bool
	}{
		{"latched CB, open HL perps -> manage", RiskReasonCircuitBreakerActive, "hyperliquid", "perps", 0.5, true},
		{"latched CB, no open position -> skip", RiskReasonCircuitBreakerActive, "hyperliquid", "perps", 0, false},
		{"latched CB, flat-ish negative qty guard -> skip", RiskReasonCircuitBreakerActive, "hyperliquid", "perps", -0.1, false},
		{"first-fire max drawdown -> skip (mid-transition)", RiskReasonMaxDrawdownExceeded, "hyperliquid", "perps", 0.5, false},
		{"first-fire max drawdown formatted -> skip", RiskReasonMaxDrawdownExceeded + " (40.0% > 18.0%)", "hyperliquid", "perps", 0.5, false},
		{"first-fire consecutive losses -> skip", RiskReasonConsecutiveLosses, "hyperliquid", "perps", 0.5, false},
		{"latched CB, OKX perps -> skip (no HL walker)", RiskReasonCircuitBreakerActive, "okx", "perps", 0.5, false},
		{"latched CB, HL futures -> skip", RiskReasonCircuitBreakerActive, "hyperliquid", "futures", 0.5, false},
		{"latched CB, HL spot -> skip", RiskReasonCircuitBreakerActive, "hyperliquid", "spot", 0.5, false},
		{"empty reason (allowed) -> skip", "", "hyperliquid", "perps", 0.5, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := circuitBreakerPermitsManagement(tc.reason, tc.platform, tc.stratType, tc.posQty); got != tc.want {
				t.Errorf("circuitBreakerPermitsManagement(%q, %q, %q, %v) = %v, want %v",
					tc.reason, tc.platform, tc.stratType, tc.posQty, got, tc.want)
			}
		})
	}
}

func TestCheckRisk_CircuitBreakerDisabled_SuppressesBothArms(t *testing.T) {
	falseVal, trueVal := false, true
	liveArgs := []string{"momentum", "ETH", "1h", "--mode=live"}
	paperArgs := []string{"momentum", "ETH", "1h", "--mode=paper"}

	newDrawdownState := func() *StrategyState {
		return &StrategyState{
			ID:   "hl-eth",
			Type: "perps",
			Cash: 7700,
			RiskState: RiskState{
				PeakValue:      10000,
				MaxDrawdownPct: 20,
				DailyPnLDate:   todayUTC(),
			},
			Positions:       map[string]*Position{},
			OptionPositions: map[string]*OptionPosition{},
			TradeHistory:    []Trade{},
		}
	}
	newLossState := func() *StrategyState {
		return &StrategyState{
			ID:   "hl-eth",
			Type: "perps",
			Cash: 10000,
			RiskState: RiskState{
				PeakValue:         10000,
				MaxDrawdownPct:    20,
				ConsecutiveLosses: 5,
				DailyPnLDate:      todayUTC(),
			},
			Positions:       map[string]*Position{},
			OptionPositions: map[string]*OptionPosition{},
			TradeHistory:    []Trade{},
		}
	}

	cases := []struct {
		name     string
		cb       *bool
		args     []string
		wantFire bool
	}{
		{"disabled-live", &falseVal, liveArgs, false},
		{"disabled-paper", &falseVal, paperArgs, false},
		{"nil-live", nil, liveArgs, true},
		{"nil-paper", nil, paperArgs, true},
		{"explicitTrue-live", &trueVal, liveArgs, true},
		{"explicitTrue-paper", &trueVal, paperArgs, true},
	}

	for _, tc := range cases {
		sc := func(args []string) *StrategyConfig {
			return &StrategyConfig{
				ID: "hl-eth", Type: "perps", Platform: "hyperliquid",
				Args: args, MaxDrawdownPct: 20, CircuitBreaker: tc.cb,
			}
		}

		t.Run("drawdown/"+tc.name, func(t *testing.T) {
			s := newDrawdownState()
			allowed, reason := CheckRisk(sc(tc.args), s, PortfolioValue(s, nil), nil, nil, nil)
			if fired := !allowed; fired != tc.wantFire {
				t.Fatalf("drawdown fire = %v (reason=%q), want %v", fired, reason, tc.wantFire)
			}
			if got := s.RiskState.CurrentDrawdownPct; got < 22.9 || got > 23.1 {
				t.Fatalf("CurrentDrawdownPct = %.2f, want ~23 even when CB disabled", got)
			}
		})

		t.Run("losses/"+tc.name, func(t *testing.T) {
			s := newLossState()
			allowed, reason := CheckRisk(sc(tc.args), s, PortfolioValue(s, nil), nil, nil, nil)
			if fired := !allowed; fired != tc.wantFire {
				t.Fatalf("consecutive-loss fire = %v (reason=%q), want %v", fired, reason, tc.wantFire)
			}
		})
	}
}

func TestCheckRisk_CircuitBreakerDisabled_StillHonorsExistingLatch(t *testing.T) {
	off := false
	s := &StrategyState{
		ID:   "hl-eth",
		Type: "perps",
		Cash: 1000,
		RiskState: RiskState{
			PeakValue:           1000,
			MaxDrawdownPct:      20,
			CircuitBreaker:      true,
			CircuitBreakerUntil: time.Now().UTC().Add(time.Hour),
			DailyPnLDate:        todayUTC(),
		},
		Positions:       map[string]*Position{},
		OptionPositions: map[string]*OptionPosition{},
		TradeHistory:    []Trade{},
	}
	sc := &StrategyConfig{
		ID: "hl-eth", Type: "perps", Platform: "hyperliquid",
		Args: []string{"momentum", "ETH", "1h", "--mode=live"}, MaxDrawdownPct: 20, CircuitBreaker: &off,
	}
	allowed, reason := CheckRisk(sc, s, PortfolioValue(s, nil), nil, nil, nil)
	if allowed {
		t.Fatal("disabling CB must not bypass an already-latched circuit breaker")
	}
	if reason != RiskReasonCircuitBreakerActive {
		t.Fatalf("reason = %q, want %q", reason, RiskReasonCircuitBreakerActive)
	}
}

func TestCheckRisk_CircuitBreakerDisabled_WarnsOncePerEpisode(t *testing.T) {
	off, on := false, true
	id := "hl-cb-suppress-warn"
	circuitBreakerSuppressedWarned.Delete(id)

	newState := func() *StrategyState {
		return &StrategyState{
			ID:   id,
			Type: "perps",
			Cash: 7700,
			RiskState: RiskState{
				PeakValue:         10000,
				MaxDrawdownPct:    20,
				ConsecutiveLosses: 5,
				DailyPnLDate:      todayUTC(),
			},
			Positions:       map[string]*Position{},
			OptionPositions: map[string]*OptionPosition{},
			TradeHistory:    []Trade{},
		}
	}
	scWith := func(cb *bool) *StrategyConfig {
		return &StrategyConfig{
			ID: id, Type: "perps", Platform: "hyperliquid",
			Args: []string{"momentum", "ETH", "1h", "--mode=live"}, MaxDrawdownPct: 20, CircuitBreaker: cb,
		}
	}
	run := func(cb *bool) (bool, string) {
		var buf bytes.Buffer
		logger := &StrategyLogger{stratID: id, writer: &buf}
		s := newState()
		allowed, _ := CheckRisk(scWith(cb), s, PortfolioValue(s, nil), nil, logger, nil)
		return allowed, buf.String()
	}

	allowed, out := run(&off)
	if !allowed {
		t.Fatal("disabled CB should allow trading")
	}
	for _, want := range []string{"WARN", "DISABLED", "NO circuit breaker", "warning only", "drawdown 23.0% > 20.0%", "5 consecutive losses"} {
		if !strings.Contains(out, want) {
			t.Fatalf("first suppression cycle missing %q in: %s", want, out)
		}
	}

	if _, out := run(&off); strings.Contains(out, "circuit breaker is DISABLED") {
		t.Fatalf("expected dedup (no repeat warning) on the second cycle, got: %s", out)
	}

	allowed, out = run(&on)
	if allowed {
		t.Fatal("re-enabled CB on a breached state should fire")
	}
	if strings.Contains(out, "circuit breaker is DISABLED") {
		t.Fatalf("enabled CB must not emit a suppression warning, got: %s", out)
	}
	if _, ok := circuitBreakerSuppressedWarned.Load(id); ok {
		t.Fatal("re-enabling should clear the suppression throttle")
	}

	if _, out := run(&off); !strings.Contains(out, "circuit breaker is DISABLED") {
		t.Fatalf("a fresh suppression episode after re-enable should warn again, got: %s", out)
	}

	circuitBreakerSuppressedWarned.Delete(id)
}

func TestCheckRisk_CircuitBreakerDisabled_ThrottleClearsWhenBreachClears(t *testing.T) {
	off := false
	id := "hl-cb-suppress-clear"
	circuitBreakerSuppressedWarned.Delete(id)
	defer circuitBreakerSuppressedWarned.Delete(id)

	sc := &StrategyConfig{
		ID: id, Type: "perps", Platform: "hyperliquid",
		Args: []string{"momentum", "ETH", "1h", "--mode=live"}, MaxDrawdownPct: 20, CircuitBreaker: &off,
	}
	breached := &StrategyState{
		ID: id, Type: "perps", Cash: 7700,
		RiskState:       RiskState{PeakValue: 10000, MaxDrawdownPct: 20, DailyPnLDate: todayUTC()},
		Positions:       map[string]*Position{},
		OptionPositions: map[string]*OptionPosition{},
	}
	CheckRisk(sc, breached, PortfolioValue(breached, nil), nil, &StrategyLogger{stratID: id, writer: &bytes.Buffer{}}, nil)
	if _, ok := circuitBreakerSuppressedWarned.Load(id); !ok {
		t.Fatal("expected throttle set after a breached disabled cycle")
	}

	healthy := &StrategyState{
		ID: id, Type: "perps", Cash: 10000,
		RiskState:       RiskState{PeakValue: 10000, MaxDrawdownPct: 20, DailyPnLDate: todayUTC()},
		Positions:       map[string]*Position{},
		OptionPositions: map[string]*OptionPosition{},
	}
	CheckRisk(sc, healthy, PortfolioValue(healthy, nil), nil, &StrategyLogger{stratID: id, writer: &bytes.Buffer{}}, nil)
	if _, ok := circuitBreakerSuppressedWarned.Load(id); ok {
		t.Fatal("throttle should clear once the breach clears")
	}
}

func TestCollectMissingMarkPositions(t *testing.T) {
	hlPerps := func(id, coin string, mode string) StrategyConfig {
		args := []string{"trend", coin, "1h"}
		if mode != "" {
			args = append(args, "--mode="+mode)
		}
		return StrategyConfig{ID: id, Type: "perps", Platform: "hyperliquid", Args: args}
	}
	hlManual := func(id, coin, mode string) StrategyConfig {
		return StrategyConfig{ID: id, Type: "manual", Platform: "hyperliquid", Symbol: coin,
			Args: []string{"hold", coin, "1h", "--mode=" + mode}}
	}
	type miss struct {
		strategyID, symbol string
		live               bool
		platform, typ      string
		disabledManagers   int
	}
	cases := []struct {
		name        string
		strategies  []StrategyConfig
		openSymbols map[string][]string
		prices      map[string]float64
		want        []miss
	}{
		{
			name: "mixed book reports only mark-less HL perps and manual positions",
			strategies: []StrategyConfig{
				hlManual("manual-hl-eth", "ETH", "live"),
				hlManual("manual-hl-record", "HYPE", "paper"),
				hlPerps("hl-trend-btc", "BTC", ""),
				hlPerps("hl-trend-sol", "SOL", ""),
				{ID: "sma-btc", Type: "spot", Platform: "binanceus", Args: []string{"sma", "BTC/USDT", "1h"}},
				{ID: "deribit-vol-btc", Type: "options", Platform: "deribit", Args: []string{"vol", "BTC"}},
				hlPerps("flat-strategy", "DOGE", ""),
				hlPerps("not-in-state", "AVAX", ""),
			},
			openSymbols: map[string][]string{
				"manual-hl-eth":    {"ETH"},
				"manual-hl-record": {"HYPE"},
				"hl-trend-btc":     {"BTC"},
				"hl-trend-sol":     {"SOL"},
				"sma-btc":          {"BTC/USDT"},
				"deribit-vol-btc":  {"BTC-PERP"},
				"flat-strategy":    {},
			},
			prices: map[string]float64{"BTC": 67500.0, "BTC/USDT": 67510.0, "ETH": 0},
			want: []miss{
				{"manual-hl-eth", "ETH", true, "hyperliquid", "manual", 2},
				{"manual-hl-record", "HYPE", false, "hyperliquid", "manual", 2},
				{"hl-trend-sol", "SOL", false, "hyperliquid", "perps", 2},
			},
		},
		{
			name:        "symbols sorted per strategy",
			strategies:  []StrategyConfig{hlPerps("hl-trend-btc", "BTC", "")},
			openSymbols: map[string][]string{"hl-trend-btc": {"SOL", "BTC", "ETH"}},
			prices:      map[string]float64{},
			want: []miss{
				{"hl-trend-btc", "BTC", false, "hyperliquid", "perps", 2},
				{"hl-trend-btc", "ETH", false, "hyperliquid", "perps", 2},
				{"hl-trend-btc", "SOL", false, "hyperliquid", "perps", 2},
			},
		},
		{
			name:       "no open positions",
			strategies: []StrategyConfig{hlPerps("hl-trend-btc", "BTC", "")},
		},
		{
			name:        "live flag drives escalation",
			strategies:  []StrategyConfig{hlPerps("hl-live", "BTC", "live"), hlPerps("hl-paper", "SOL", "paper")},
			openSymbols: map[string][]string{"hl-live": {"BTC"}, "hl-paper": {"SOL"}},
			prices:      map[string]float64{},
			want: []miss{
				{"hl-live", "BTC", true, "hyperliquid", "perps", 2},
				{"hl-paper", "SOL", false, "hyperliquid", "perps", 2},
			},
		},
		{
			name:        "record-only manual position under a non-config symbol",
			strategies:  []StrategyConfig{hlManual("manual-hl", "ETH", "live")},
			openSymbols: map[string][]string{"manual-hl": {"HYPE"}},
			prices:      map[string]float64{"ETH": 3400},
			want:        []miss{{"manual-hl", "HYPE", true, "hyperliquid", "manual", 2}},
		},
		{
			name:        "flat record-only manual is silent",
			strategies:  []StrategyConfig{hlManual("manual-hl-record", "HYPE", "paper")},
			openSymbols: map[string][]string{"manual-hl-record": {}},
		},
		{
			name: "carries the venue management surface",
			strategies: []StrategyConfig{
				hlPerps("hl-live-btc", "BTC", "live"),
				{ID: "sma-eth", Type: "spot", Platform: "binanceus", Args: []string{"sma", "ETH", "1h", "--mode=live"}},
			},
			openSymbols: map[string][]string{"hl-live-btc": {"BTC"}, "sma-eth": {"ETH"}},
			prices:      map[string]float64{},
			want: []miss{
				{"hl-live-btc", "BTC", true, "hyperliquid", "perps", 2},
				{"sma-eth", "ETH", true, "binanceus", "spot", 0},
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := collectMissingMarkPositions(tc.strategies, tc.openSymbols, tc.prices)
			if len(got) != len(tc.want) {
				t.Fatalf("collectMissingMarkPositions = %+v, want %+v", got, tc.want)
			}
			for i, want := range tc.want {
				g := got[i]
				if g.StrategyID != want.strategyID || g.Symbol != want.symbol || g.Live != want.live ||
					g.Platform != want.platform || g.Type != want.typ || len(g.DisabledManagers) != want.disabledManagers {
					t.Errorf("[%d] = %+v, want %+v", i, g, want)
				}
			}
		})
	}
}

func TestManualOnlyMarkSymbols(t *testing.T) {
	cases := []struct {
		name       string
		strategies []StrategyConfig
		want       []string
	}{
		{
			name: "excludes coins donated by perps rails",
			strategies: []StrategyConfig{
				{ID: "manual-hype", Type: "manual", Platform: "hyperliquid", Symbol: "HYPE",
					Args: []string{"hold", "HYPE", "1h", "--mode=live"}},
				{ID: "manual-btc", Type: "manual", Platform: "hyperliquid", Symbol: "BTC",
					Args: []string{"hold", "BTC", "1h", "--mode=live"}},
				{ID: "hl-trend-btc", Type: "perps", Platform: "hyperliquid", Args: []string{"trend", "BTC", "1h"}},
				{ID: "manual-sol", Type: "manual", Platform: "hyperliquid", Symbol: "SOL",
					Args: []string{"hold", "SOL", "1h", "--mode=live"}},
				{ID: "okx-trend-sol", Type: "perps", Platform: "okx", Args: []string{"trend", "SOL", "1h"}},
				{ID: "manual-okx", Type: "manual", Platform: "okx", Symbol: "DOGE",
					Args: []string{"hold", "DOGE", "1h", "--mode=live"}},
			},
			want: []string{"HYPE"},
		},
		{
			name: "no manual strategies",
			strategies: []StrategyConfig{
				{ID: "hl-trend-btc", Type: "perps", Platform: "hyperliquid", Args: []string{"trend", "BTC", "1h"}},
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := manualOnlyMarkSymbols(tc.strategies)
			if len(got) != len(tc.want) {
				t.Fatalf("manualOnlyMarkSymbols = %v, want %v", got, tc.want)
			}
			for i := range tc.want {
				if got[i] != tc.want[i] {
					t.Errorf("[%d] = %q, want %q", i, got[i], tc.want[i])
				}
			}
		})
	}
}

func TestPricesWithoutSymbols_DeletesRatherThanZeroes(t *testing.T) {
	prices := map[string]float64{"BTC": 67500, "HYPE": 24.5}
	got := pricesWithoutSymbols(prices, []string{"HYPE"})
	if _, ok := got["HYPE"]; ok {
		t.Errorf("HYPE still present in %v, want deleted", got)
	}
	if got["BTC"] != 67500 {
		t.Errorf("BTC = %v, want 67500", got["BTC"])
	}
	if _, ok := prices["HYPE"]; !ok {
		t.Errorf("source map was mutated: %v", prices)
	}
	same := pricesWithoutSymbols(prices, nil)
	if len(same) != len(prices) {
		t.Errorf("empty drop list changed the map: %v", same)
	}
}

func TestManualMarkBasisPeakAdjustment(t *testing.T) {
	tests := []struct {
		name                            string
		oldPeak, liveTotal, legacyTotal float64
		wantPeak                        float64
		wantApply                       bool
	}{
		{
			name:    "underwater manual lowers the peak by exactly the delta",
			oldPeak: 60000, liveTotal: 56000, legacyTotal: 60000,
			wantPeak: 56000, wantApply: true,
		},
		{
			name:    "profitable manual raises the peak by the delta",
			oldPeak: 60000, liveTotal: 63000, legacyTotal: 60000,
			wantPeak: 63000, wantApply: true,
		},
		{
			name:    "real drawdown under the old basis survives the migration",
			oldPeak: 60000, liveTotal: 50000, legacyTotal: 54000,
			wantPeak: 56000, wantApply: true,
		},
		{
			name:    "no manual position moved: zero delta, no change",
			oldPeak: 60000, liveTotal: 58000, legacyTotal: 58000,
			wantPeak: 60000, wantApply: false,
		},
		{
			name:    "cold-start peak has no legacy basis to correct",
			oldPeak: 0, liveTotal: 56000, legacyTotal: 60000,
			wantPeak: 0, wantApply: false,
		},
		{
			name:    "negative peak is never written",
			oldPeak: 1000, liveTotal: 100, legacyTotal: 5000,
			wantPeak: 1000, wantApply: false,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			gotPeak, gotApply := manualMarkBasisPeakAdjustment(tc.oldPeak, tc.liveTotal, tc.legacyTotal)
			if gotApply != tc.wantApply {
				t.Errorf("apply = %v, want %v", gotApply, tc.wantApply)
			}
			if gotPeak != tc.wantPeak {
				t.Errorf("peak = %v, want %v", gotPeak, tc.wantPeak)
			}
		})
	}
}

func TestSnapshotOpenSymbolsByStrategy(t *testing.T) {
	state := &AppState{Strategies: map[string]*StrategyState{
		"open":    {Positions: map[string]*Position{"BTC": {Quantity: 0.5}}},
		"flat":    {Positions: map[string]*Position{}},
		"corrupt": {Positions: map[string]*Position{"SOL": {Quantity: 0}}},
		"nil":     nil,
	}}
	got := snapshotOpenSymbolsByStrategy(state)
	if len(got) != 1 {
		t.Fatalf("snapshotOpenSymbolsByStrategy = %+v, want one entry", got)
	}
	if len(got["open"]) != 1 || got["open"][0] != "BTC" {
		t.Errorf(`got["open"] = %v, want ["BTC"]`, got["open"])
	}
	if snapshotOpenSymbolsByStrategy(nil) != nil {
		t.Error("nil state should snapshot nil")
	}
}

func TestMissingManualOnlyMarks(t *testing.T) {
	manual := func(id, coin string) StrategyConfig {
		return StrategyConfig{ID: id, Type: "manual", Platform: "hyperliquid", Symbol: coin,
			Args: []string{"hold", coin, "1h", "--mode=live"}}
	}
	cases := []struct {
		name        string
		strategies  []StrategyConfig
		openSymbols map[string][]string
		prices      map[string]float64
		want        []string
	}{
		{
			name: "non-manual outages cancel out of the delta and never defer",
			strategies: []StrategyConfig{
				manual("manual-hl-hype", "HYPE"),
				{ID: "ts-es", Type: "futures", Platform: "topstep", Args: []string{"trend", "ES", "1h"}},
				{ID: "okx-sol", Type: "perps", Platform: "okx", Args: []string{"trend", "SOL", "1h"}},
				{ID: "sma-btc", Type: "spot", Platform: "binanceus", Args: []string{"sma", "BTC/USDT", "1h"}},
			},
			openSymbols: map[string][]string{"manual-hl-hype": {"HYPE"}, "ts-es": {"ES"}, "okx-sol": {"SOL"}, "sma-btc": {"BTC/USDT"}},
			prices:      map[string]float64{"HYPE": 42.0},
		},
		{
			name:        "manual-only outage defers, sorted",
			strategies:  []StrategyConfig{manual("manual-hl-hype", "HYPE"), manual("manual-hl-eth", "ETH")},
			openSymbols: map[string][]string{"manual-hl-hype": {"HYPE"}, "manual-hl-eth": {"ETH"}},
			prices:      map[string]float64{"ETH": 0},
			want:        []string{"ETH", "HYPE"},
		},
		{
			name: "no manual-only coin runs immediately",
			strategies: []StrategyConfig{
				{ID: "hl-trend-btc", Type: "perps", Platform: "hyperliquid", Args: []string{"trend", "BTC", "1h"}},
				{ID: "sma-eth", Type: "spot", Platform: "binanceus", Args: []string{"sma", "ETH", "1h"}},
			},
			openSymbols: map[string][]string{"hl-trend-btc": {"BTC"}, "sma-eth": {"ETH"}},
			prices:      map[string]float64{},
		},
		{
			name: "donor coin never gates",
			strategies: []StrategyConfig{
				manual("manual-hl-btc", "BTC"),
				{ID: "hl-trend-btc", Type: "perps", Platform: "hyperliquid", Args: []string{"trend", "BTC", "1h"}},
			},
			openSymbols: map[string][]string{"manual-hl-btc": {"BTC"}},
			prices:      map[string]float64{},
		},
		{
			name:        "unheld manual coin never gates",
			strategies:  []StrategyConfig{manual("manual-hl-hype", "HYPE")},
			openSymbols: map[string][]string{"manual-hl-hype": {}},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := missingManualOnlyMarks(tc.strategies, tc.openSymbols, tc.prices)
			if len(got) != len(tc.want) {
				t.Fatalf("missingManualOnlyMarks = %v, want %v", got, tc.want)
			}
			for i := range tc.want {
				if got[i] != tc.want[i] {
					t.Errorf("[%d] = %q, want %q", i, got[i], tc.want[i])
				}
			}
		})
	}
}

func TestMarkGatedManagers_ScopedToHyperliquidPerpsAndManual(t *testing.T) {
	cases := []struct {
		name    string
		sc      StrategyConfig
		wantAny bool
	}{
		{"hl perps", StrategyConfig{Type: "perps", Platform: "hyperliquid"}, true},
		{"hl manual", StrategyConfig{Type: "manual", Platform: "hyperliquid"}, true},
		{"okx perps", StrategyConfig{Type: "perps", Platform: "okx"}, false},
		{"binanceus spot", StrategyConfig{Type: "spot", Platform: "binanceus"}, false},
		{"topstep futures", StrategyConfig{Type: "futures", Platform: "topstep"}, false},
		{"hl spot", StrategyConfig{Type: "spot", Platform: "hyperliquid"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := markGatedManagers(tc.sc)
			if tc.wantAny {
				if len(got) != 2 {
					t.Fatalf("markGatedManagers = %v, want the walker and the ratchet", got)
				}
				return
			}
			if len(got) != 0 {
				t.Errorf("markGatedManagers = %v, want empty — this venue runs no mark-gated manager", got)
			}
		})
	}
}

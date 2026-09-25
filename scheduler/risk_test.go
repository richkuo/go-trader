package main

import (
	"fmt"
	"math"
	"strings"
	"testing"
	"time"
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

func resetSchedulerStarted(t *testing.T) {
	t.Helper()
	schedulerStarted.Store(false)
	t.Cleanup(func() { schedulerStarted.Store(false) })
}

func latchedSharedWalletState() *AppState {
	return &AppState{
		Strategies: map[string]*StrategyState{},
		PortfolioRisk: map[RiskPartition]*PortfolioRiskState{livePartition: {
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
	if state.partitionRisk(livePartition).KillSwitchActive {
		t.Error("expected KillSwitchActive=false after clear")
	}
	if !state.partitionRisk(livePartition).KillSwitchAt.IsZero() {
		t.Errorf("expected KillSwitchAt zeroed; got %v", state.partitionRisk(livePartition).KillSwitchAt)
	}
	if state.partitionRisk(livePartition).WarningSent {
		t.Error("expected WarningSent reset to false")
	}
	if state.partitionRisk(livePartition).PeakValue != 4500 {
		t.Errorf("expected PeakValue re-baselined to 4500; got %.2f", state.partitionRisk(livePartition).PeakValue)
	}
	if state.partitionRisk(livePartition).CurrentDrawdownPct != 0 {
		t.Errorf("expected CurrentDrawdownPct reset to 0; got %.2f", state.partitionRisk(livePartition).CurrentDrawdownPct)
	}
	if state.partitionRisk(livePartition).CurrentMarginDrawdownPct != 0 {
		t.Errorf("expected CurrentMarginDrawdownPct reset to 0; got %.2f", state.partitionRisk(livePartition).CurrentMarginDrawdownPct)
	}
	if len(state.partitionRisk(livePartition).Events) != 1 {
		t.Fatalf("expected 1 audit event; got %d", len(state.partitionRisk(livePartition).Events))
	}
	evt := state.partitionRisk(livePartition).Events[0]
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
			if cleared || calls != 0 || !state.partitionRisk(livePartition).KillSwitchActive {
				t.Fatalf("non-legacy wallet must preserve latch: cleared=%v calls=%d active=%v", cleared, calls, state.partitionRisk(livePartition).KillSwitchActive)
			}
		})
	}
}

func TestClearLatchedKillSwitchSharedWallet_FetchFailurePreservesLatch(t *testing.T) {
	resetSchedulerStarted(t)
	state := latchedSharedWalletState()
	strategies := sharedHLStrategies(t)
	originalLatchedAt := state.partitionRisk(livePartition).KillSwitchAt

	fetcher := func(platform string) (float64, error) {
		return 0, fmt.Errorf("simulated network failure")
	}

	cleared := ClearLatchedKillSwitchSharedWallet(state, strategies, fetcher)
	if cleared {
		t.Fatal("expected ClearLatchedKillSwitchSharedWallet to return false on fetch failure")
	}
	if !state.partitionRisk(livePartition).KillSwitchActive {
		t.Error("expected KillSwitchActive to remain true after fetch failure")
	}
	if !state.partitionRisk(livePartition).KillSwitchAt.Equal(originalLatchedAt) {
		t.Errorf("expected KillSwitchAt unchanged; got %v", state.partitionRisk(livePartition).KillSwitchAt)
	}
	if len(state.partitionRisk(livePartition).Events) != 0 {
		t.Errorf("expected no audit event on failure; got %d", len(state.partitionRisk(livePartition).Events))
	}
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

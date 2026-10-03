package main

import (
	"context"
	"fmt"
	"math"
	"sync"
	"testing"
	"time"
)

func TestForceCloseOKXLive_ClosesOwnedCoinsOnly(t *testing.T) {
	okxLive := []StrategyConfig{
		{ID: "okx-btc", Platform: "okx", Type: "perps",
			Args: []string{"sma", "BTC", "1h", "--mode=live"}},
	}
	positions := []OKXPosition{
		{Coin: "BTC", Size: 0.01, Side: "long"},
		{Coin: "SOL", Size: 50, Side: "long"},
	}
	var calls []string
	closer := func(sym string, partialSz *float64) (*OKXCloseResult, error) {
		calls = append(calls, sym)
		return &OKXCloseResult{Close: &OKXClose{Symbol: sym}}, nil
	}

	report := forceCloseOKXLive(context.Background(), positions, okxLive, closer)

	if !report.ConfirmedFlat() {
		t.Errorf("expected ConfirmedFlat, got errors=%v", report.Errors)
	}
	if len(calls) != 1 || calls[0] != "BTC" {
		t.Errorf("expected closer to be called only for owned coin BTC, got %v", calls)
	}
	if len(report.ClosedCoins) != 1 || report.ClosedCoins[0] != "BTC" {
		t.Errorf("ClosedCoins = %v, want [BTC]", report.ClosedCoins)
	}
	if len(report.Unconfigured) != 1 || report.Unconfigured[0].Coin != "SOL" {
		t.Errorf("Unconfigured = %v, want [SOL]", report.Unconfigured)
	}
}

func TestComputeOKXCircuitCloseQty(t *testing.T) {
	soleOwner := []StrategyConfig{
		{ID: "okx-eth", Platform: "okx", Type: "perps",
			Args: []string{"sma", "ETH", "1h", "--mode=live"}},
	}
	sharedPct := []StrategyConfig{
		{ID: "okx-a", Platform: "okx", Type: "perps", CapitalPct: 0.5, Capital: 1000,
			Args: []string{"sma", "ETH", "1h", "--mode=live"}},
		{ID: "okx-b", Platform: "okx", Type: "perps", CapitalPct: 0.5, Capital: 1000,
			Args: []string{"ema", "ETH", "1h", "--mode=live"}},
	}
	mixedUnits := []StrategyConfig{
		{ID: "okx-a", Platform: "okx", Type: "perps", CapitalPct: 0.5,
			Args: []string{"sma", "ETH", "1h", "--mode=live"}},
		{ID: "okx-b", Platform: "okx", Type: "perps", Capital: 1000,
			Args: []string{"ema", "ETH", "1h", "--mode=live"}},
	}
	cases := []struct {
		name       string
		strategyID string
		roster     []StrategyConfig
		positions  []OKXPosition
		wantQty    float64
		wantOK     bool
	}{
		{"sole owner takes full abs size", "okx-eth", soleOwner, []OKXPosition{{Coin: "ETH", Size: -0.4, EntryPrice: 3000, Side: "short"}}, 0.4, true},
		{"shared 50/50 splits by capital pct", "okx-a", sharedPct, []OKXPosition{{Coin: "ETH", Size: 0.517, EntryPrice: 3000, Side: "long"}}, 0.517 * 0.5, true},
		{"mixed units falls back to equal weights", "okx-a", mixedUnits, []OKXPosition{{Coin: "ETH", Size: 0.5, EntryPrice: 3000, Side: "long"}}, 0.25, true},
		{"no on-chain position for coin", "okx-eth", soleOwner, []OKXPosition{{Coin: "BTC", Size: 0.1, EntryPrice: 42000, Side: "long"}}, 0, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			q, ok := computeOKXCircuitCloseQty("ETH", tc.strategyID, tc.positions, tc.roster)
			if ok != tc.wantOK {
				t.Fatalf("ok=%v want %v (qty=%v)", ok, tc.wantOK, q)
			}
			if tc.wantOK && math.Abs(q-tc.wantQty) > 1e-9 {
				t.Errorf("qty=%.6f want %.6f", q, tc.wantQty)
			}
		})
	}
}

func TestRunPendingOKXCircuitCloses_RecoversStuckCB(t *testing.T) {
	state := &AppState{
		Strategies: map[string]*StrategyState{
			"okx-a": {
				ID: "okx-a",
				RiskState: RiskState{
					CircuitBreaker:       true,
					CircuitBreakerUntil:  time.Now().Add(24 * time.Hour),
					PendingCircuitCloses: nil,
				},
			},
		},
	}
	cfg := []StrategyConfig{
		{ID: "okx-a", Platform: "okx", Type: "perps",
			Args: []string{"sma", "ETH", "1h", "--mode=live"}},
	}
	var mu sync.RWMutex
	var calls []string
	closer := func(sym string, partialSz *float64) (*OKXCloseResult, error) {
		if partialSz != nil {
			calls = append(calls, fmt.Sprintf("%s:%g", sym, *partialSz))
		} else {
			calls = append(calls, sym)
		}
		return &OKXCloseResult{
			Close:    &OKXClose{Symbol: sym, Fill: &OKXCloseFill{TotalSz: 0.4, AvgPx: 1}},
			Platform: "okx",
		}, nil
	}
	runPendingOKXCircuitCloses(
		context.Background(),
		state,
		cfg,
		true,
		[]OKXPosition{{Coin: "ETH", Size: 0.4, EntryPrice: 1, Side: "long"}},
		true,
		nil,
		closer,
		30*time.Second,
		&mu,
		nil,
	)
	if len(calls) != 1 || calls[0] != "ETH:0.4" {
		t.Errorf("closer calls=%v want [ETH:0.4] (recovered pending should drain full abs size as sole owner)", calls)
	}
	if state.Strategies["okx-a"].RiskState.getPendingCircuitClose(PlatformPendingCloseOKX) != nil {
		t.Error("expected pending cleared after successful recovery close")
	}
}

func TestRunPendingOKXCircuitCloses_ClearsOnSuccess(t *testing.T) {
	state := &AppState{
		Strategies: map[string]*StrategyState{
			"okx-a": {
				ID: "okx-a",
				RiskState: RiskState{
					PendingCircuitCloses: map[string]*PendingCircuitClose{
						PlatformPendingCloseOKX: {
							Symbols: []PendingCircuitCloseSymbol{{Symbol: "ETH", Size: 0.1}},
						},
					},
				},
			},
		},
	}
	cfg := []StrategyConfig{
		{ID: "okx-a", Platform: "okx", Type: "perps",
			Args: []string{"sma", "ETH", "1h", "--mode=live"}},
	}
	var mu sync.RWMutex
	var calls []string
	closer := func(sym string, partialSz *float64) (*OKXCloseResult, error) {
		if partialSz != nil {
			calls = append(calls, fmt.Sprintf("%s:%g", sym, *partialSz))
		} else {
			calls = append(calls, sym)
		}
		return &OKXCloseResult{
			Close:    &OKXClose{Symbol: sym, Fill: &OKXCloseFill{TotalSz: 0.1, AvgPx: 1}},
			Platform: "okx",
		}, nil
	}
	runPendingOKXCircuitCloses(
		context.Background(),
		state,
		cfg,
		true,
		[]OKXPosition{{Coin: "ETH", Size: 0.5, EntryPrice: 1, Side: "long"}},
		true,
		nil,
		closer,
		30*time.Second,
		&mu,
		nil,
	)
	if state.Strategies["okx-a"].RiskState.getPendingCircuitClose(PlatformPendingCloseOKX) != nil {
		t.Error("expected pending cleared after successful close")
	}
	if len(calls) != 1 || calls[0] != "ETH:0.1" {
		t.Errorf("closer calls=%v want [ETH:0.1]", calls)
	}
}

func TestRunPendingOKXCircuitCloses_PendingPreservedOnFailure(t *testing.T) {
	state := &AppState{
		Strategies: map[string]*StrategyState{
			"okx-a": {
				ID: "okx-a",
				RiskState: RiskState{
					PendingCircuitCloses: map[string]*PendingCircuitClose{
						PlatformPendingCloseOKX: {
							Symbols: []PendingCircuitCloseSymbol{{Symbol: "ETH", Size: 0.1}},
						},
					},
				},
			},
		},
	}
	cfg := []StrategyConfig{
		{ID: "okx-a", Platform: "okx", Type: "perps",
			Args: []string{"sma", "ETH", "1h", "--mode=live"}},
	}
	var mu sync.RWMutex
	closer := func(sym string, partialSz *float64) (*OKXCloseResult, error) {
		return nil, fmt.Errorf("okx 503")
	}
	runPendingOKXCircuitCloses(
		context.Background(),
		state,
		cfg,
		true,
		[]OKXPosition{{Coin: "ETH", Size: 0.5, EntryPrice: 1, Side: "long"}},
		true,
		nil,
		closer,
		30*time.Second,
		&mu,
		nil,
	)
	if state.Strategies["okx-a"].RiskState.getPendingCircuitClose(PlatformPendingCloseOKX) == nil {
		t.Error("expected pending preserved after closer failure (latch semantic)")
	}
}

func TestSetOKXCircuitBreakerPending_SkipsPaperMode(t *testing.T) {
	sc := StrategyConfig{ID: "okx-paper", Platform: "okx", Type: "perps",
		Args: []string{"sma", "ETH", "1h"}}
	s := &StrategyState{
		ID: "okx-paper",
		Positions: map[string]*Position{
			"ETH": {Quantity: 0.25, Side: "long"},
		},
	}
	assist := &PlatformRiskAssist{
		OKXPositions: []OKXPosition{{Coin: "ETH", Size: 0.25, Side: "long"}},
		OKXLiveAll:   []StrategyConfig{sc},
	}
	setOKXCircuitBreakerPending(&sc, s, assist)
	if s.RiskState.getPendingCircuitClose(PlatformPendingCloseOKX) != nil {
		t.Error("paper-mode OKX strategy must not enqueue pending")
	}
}

package main

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"
)

func TestRunPendingRobinhoodCircuitCloses_RecoversStuckCB(t *testing.T) {
	state := &AppState{
		Strategies: map[string]*StrategyState{
			"rh-sma-btc": {
				ID: "rh-sma-btc",
				RiskState: RiskState{
					CircuitBreaker:       true,
					CircuitBreakerUntil:  time.Now().Add(24 * time.Hour),
					PendingCircuitCloses: nil,
				},
			},
		},
	}
	cfg := []StrategyConfig{
		{ID: "rh-sma-btc", Platform: "robinhood", Type: "spot",
			Args: []string{"sma_crossover", "BTC", "1h", "--mode=live"}},
	}
	var mu sync.RWMutex
	var calls []string
	closer := func(sym string) (*RobinhoodCloseResult, error) {
		calls = append(calls, sym)
		return &RobinhoodCloseResult{Close: &RobinhoodClose{Symbol: sym}, Platform: "robinhood"}, nil
	}

	runPendingRobinhoodCircuitCloses(
		context.Background(),
		state,
		cfg,
		[]RobinhoodPosition{{Coin: "BTC", Size: 0.02, AvgPrice: 42000}},
		true,
		nil,
		closer,
		nil,
		30*time.Second,
		&mu,
	)

	if len(calls) != 1 || calls[0] != "BTC" {
		t.Errorf("expected recovered close for BTC, got %v", calls)
	}
	if state.Strategies["rh-sma-btc"].RiskState.getPendingCircuitClose(PlatformPendingCloseRobinhood) != nil {
		t.Error("expected pending cleared after recovered close")
	}
}

func TestRunPendingRobinhoodCircuitCloses_SubmitErrorRetainsPending(t *testing.T) {
	state := &AppState{
		Strategies: map[string]*StrategyState{
			"rh-sma-btc": {
				ID: "rh-sma-btc",
				RiskState: RiskState{
					PendingCircuitCloses: map[string]*PendingCircuitClose{
						PlatformPendingCloseRobinhood: {
							Symbols: []PendingCircuitCloseSymbol{{Symbol: "BTC", Size: 0.02}},
						},
					},
				},
			},
		},
	}
	cfg := []StrategyConfig{
		{ID: "rh-sma-btc", Platform: "robinhood", Type: "spot",
			Args: []string{"sma_crossover", "BTC", "1h", "--mode=live"}},
	}
	var mu sync.RWMutex
	closer := func(sym string) (*RobinhoodCloseResult, error) {
		return nil, fmt.Errorf("robin_stocks 503")
	}

	runPendingRobinhoodCircuitCloses(
		context.Background(),
		state,
		cfg,
		[]RobinhoodPosition{{Coin: "BTC", Size: 0.02}},
		true,
		nil,
		closer,
		nil,
		30*time.Second,
		&mu,
	)

	pending := state.Strategies["rh-sma-btc"].RiskState.getPendingCircuitClose(PlatformPendingCloseRobinhood)
	if pending == nil {
		t.Fatal("expected pending preserved on submit error so next cycle retries")
	}
	if len(pending.Symbols) != 1 || pending.Symbols[0].Symbol != "BTC" {
		t.Errorf("pending = %+v, want [BTC]", pending)
	}
}

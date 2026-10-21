package main

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"
)

func TestRunPendingRobinhoodCircuitCloses_FlatBookLatchedCBSellsNothing(t *testing.T) {
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
	closer := func(req RobinhoodCloseRequest) (*RobinhoodCloseResult, error) {
		calls = append(calls, req.Symbol)
		return &RobinhoodCloseResult{Close: &RobinhoodClose{Symbol: req.Symbol}, Platform: "robinhood"}, nil
	}

	runPendingRobinhoodCircuitCloses(
		context.Background(),
		state,
		cfg,
		[]RobinhoodPosition{{Coin: "BTC", Size: 0.02, AvgPrice: 42000}},
		true,
		func() ([]RobinhoodPosition, error) { return nil, nil },
		closer,
		nil,
		30*time.Second,
		&mu,
		robinhoodCircuitCloseDeps{},
	)

	if len(calls) != 0 {
		t.Errorf("a latched CB with a flat book must sell nothing from the account, got closer calls %v", calls)
	}
	if p := state.Strategies["rh-sma-btc"].RiskState.getPendingCircuitClose(PlatformPendingCloseRobinhood); p != nil {
		t.Errorf("a latched CB with a flat book must create no pending close, got %+v", p)
	}
}

func TestRunPendingRobinhoodCircuitCloses_SubmitErrorRetainsPending(t *testing.T) {
	state := &AppState{
		Strategies: map[string]*StrategyState{
			"rh-sma-btc": {
				ID:        "rh-sma-btc",
				Positions: map[string]*Position{"BTC": {Symbol: "BTC", Quantity: 0.02, Side: "long"}},
				RiskState: RiskState{
					PendingCircuitCloses: map[string]*PendingCircuitClose{
						PlatformPendingCloseRobinhood: {
							Symbols:   []PendingCircuitCloseSymbol{{Symbol: "BTC", Size: 0.02}},
							SizeBasis: robinhoodPendingSizeBasisBook,
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
	closer := func(req RobinhoodCloseRequest) (*RobinhoodCloseResult, error) {
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
		robinhoodCircuitCloseDeps{},
	)

	pending := state.Strategies["rh-sma-btc"].RiskState.getPendingCircuitClose(PlatformPendingCloseRobinhood)
	if pending == nil {
		t.Fatal("expected pending preserved on submit error so next cycle retries")
	}
	if len(pending.Symbols) != 1 || pending.Symbols[0].Symbol != "BTC" {
		t.Errorf("pending = %+v, want [BTC]", pending)
	}
}

package main

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"
)

func TestTopStepCircuitClose(t *testing.T) {
	t.Run("ForceCloseLiveCloseErrorLatches", func(t *testing.T) {
		tsLive := []StrategyConfig{
			{ID: "ts-momentum-es", Platform: "topstep", Type: "futures",
				Args: []string{"momentum", "ES", "1h", "--mode=live"}},
		}
		positions := []TopStepPosition{{Coin: "ES", Size: 2}}
		closer := func(sym string) (*TopStepCloseResult, error) {
			return nil, fmt.Errorf("topstepx 503")
		}

		report := forceCloseTopStepLive(context.Background(), positions, tsLive, closer)

		if report.ConfirmedFlat() {
			t.Fatal("expected NOT ConfirmedFlat on close error")
		}
		if _, ok := report.Errors["ES"]; !ok {
			t.Errorf("expected ES in errors, got %v", report.Errors)
		}
	})
	t.Run("ComputeCloseQty", func(t *testing.T) {
		soleOwner := []StrategyConfig{
			{ID: "ts-es", Platform: "topstep", Type: "futures",
				Args: []string{"sma", "ES", "15m", "--mode=live"}},
		}
		multiPeer := []StrategyConfig{
			{ID: "ts-a", Platform: "topstep", Type: "futures",
				Args: []string{"sma", "ES", "15m", "--mode=live"}},
			{ID: "ts-b", Platform: "topstep", Type: "futures",
				Args: []string{"rsi", "ES", "15m", "--mode=live"}},
		}
		cases := []struct {
			name       string
			strategyID string
			roster     []StrategyConfig
			positions  []TopStepPosition
			wantQty    int
			wantOK     bool
		}{
			{"sole peer full flatten", "ts-es", soleOwner, []TopStepPosition{{Coin: "ES", Size: 3, AvgPrice: 5000, Side: "long"}}, 3, true},
			{"sole peer short full flatten", "ts-es", soleOwner, []TopStepPosition{{Coin: "ES", Size: -2, AvgPrice: 5000, Side: "short"}}, 2, true},
			{"multi peer skipped", "ts-a", multiPeer, []TopStepPosition{{Coin: "ES", Size: 5, Side: "long"}}, 0, false},
			{"no on-account position", "ts-es", soleOwner, nil, 0, false},
			{"zero size position", "ts-es", soleOwner, []TopStepPosition{{Coin: "ES", Size: 0, Side: "long"}}, 0, false},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				q, ok := computeTopStepCircuitCloseQty("ES", tc.strategyID, tc.positions, tc.roster)
				if ok != tc.wantOK {
					t.Fatalf("ok=%v want %v (qty=%d)", ok, tc.wantOK, q)
				}
				if q != tc.wantQty {
					t.Errorf("qty=%d want %d", q, tc.wantQty)
				}
			})
		}
	})
	t.Run("RunPendingRecoversStuckCB", func(t *testing.T) {
		state := &AppState{
			Strategies: map[string]*StrategyState{
				"ts-es": {
					ID: "ts-es",
					RiskState: RiskState{
						CircuitBreaker:       true,
						CircuitBreakerUntil:  time.Now().Add(24 * time.Hour),
						PendingCircuitCloses: nil,
					},
				},
			},
		}
		cfg := []StrategyConfig{
			{ID: "ts-es", Platform: "topstep", Type: "futures",
				Args: []string{"sma", "ES", "15m", "--mode=live"}},
		}
		var mu sync.RWMutex
		var calls []string
		closer := func(sym string) (*TopStepCloseResult, error) {
			calls = append(calls, sym)
			return &TopStepCloseResult{Close: &TopStepClose{Symbol: sym}}, nil
		}
		runPendingTopStepCircuitCloses(
			context.Background(),
			state,
			cfg,
			[]TopStepPosition{{Coin: "ES", Size: 3, Side: "long"}},
			true,
			nil,
			closer,
			30*time.Second,
			&mu,
			nil,
		)
		if len(calls) != 1 || calls[0] != "ES" {
			t.Errorf("closer calls=%v want [ES] (recovered pending should flatten full size)", calls)
		}
		if state.Strategies["ts-es"].RiskState.getPendingCircuitClose(PlatformPendingCloseTopStep) != nil {
			t.Error("expected pending cleared after successful recovery close")
		}
	})
	t.Run("RunPendingCloseErrorLatchesPending", func(t *testing.T) {
		state := &AppState{
			Strategies: map[string]*StrategyState{
				"ts-es": {
					ID: "ts-es",
					RiskState: RiskState{
						PendingCircuitCloses: map[string]*PendingCircuitClose{
							PlatformPendingCloseTopStep: {
								Symbols: []PendingCircuitCloseSymbol{{Symbol: "ES", Size: 3}},
							},
						},
					},
				},
			},
		}
		cfg := []StrategyConfig{
			{ID: "ts-es", Platform: "topstep", Type: "futures",
				Args: []string{"sma", "ES", "15m", "--mode=live"}},
		}
		var mu sync.RWMutex
		closer := func(sym string) (*TopStepCloseResult, error) {
			return nil, fmt.Errorf("market closed — outside RTH")
		}
		runPendingTopStepCircuitCloses(
			context.Background(),
			state,
			cfg,
			[]TopStepPosition{{Coin: "ES", Size: 3, Side: "long"}},
			true,
			nil,
			closer,
			30*time.Second,
			&mu,
			nil,
		)
		pending := state.Strategies["ts-es"].RiskState.getPendingCircuitClose(PlatformPendingCloseTopStep)
		if pending == nil {
			t.Fatal("expected pending to remain latched after close error (session-gate / venue error)")
		}
		if len(pending.Symbols) != 1 || pending.Symbols[0].Symbol != "ES" {
			t.Errorf("pending.Symbols=%v want [{ES,3}]", pending.Symbols)
		}
	})
	t.Run("RunPendingFetcherErrorBails", func(t *testing.T) {
		state := &AppState{
			Strategies: map[string]*StrategyState{
				"ts-es": {
					ID: "ts-es",
					RiskState: RiskState{
						PendingCircuitCloses: map[string]*PendingCircuitClose{
							PlatformPendingCloseTopStep: {
								Symbols: []PendingCircuitCloseSymbol{{Symbol: "ES", Size: 3}},
							},
						},
					},
				},
			},
		}
		cfg := []StrategyConfig{
			{ID: "ts-es", Platform: "topstep", Type: "futures",
				Args: []string{"sma", "ES", "15m", "--mode=live"}},
		}
		var mu sync.RWMutex
		var calls []string
		closer := func(sym string) (*TopStepCloseResult, error) {
			calls = append(calls, sym)
			return &TopStepCloseResult{Close: &TopStepClose{Symbol: sym}}, nil
		}
		fetcher := func() ([]TopStepPosition, error) {
			return nil, fmt.Errorf("topstep api 500")
		}
		runPendingTopStepCircuitCloses(
			context.Background(),
			state,
			cfg,
			nil,
			false,
			fetcher,
			closer,
			30*time.Second,
			&mu,
			nil,
		)
		if len(calls) != 0 {
			t.Errorf("closer should not be called when fetcher errors, got %v", calls)
		}
		if state.Strategies["ts-es"].RiskState.getPendingCircuitClose(PlatformPendingCloseTopStep) == nil {
			t.Error("expected pending to remain latched when fetcher errors")
		}
	})
}

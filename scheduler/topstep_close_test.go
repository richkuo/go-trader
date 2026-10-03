package main

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"
)

func TestForceCloseTopStepLive_CloseErrorLatches(t *testing.T) {
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
}

func TestParseTopStepCloseOutput(t *testing.T) {
	cases := []struct {
		name    string
		stdout  string
		stderr  string
		exitErr error
		wantErr bool
		check   func(t *testing.T, result *TopStepCloseResult)
	}{
		{
			name:   "clean success",
			stdout: `{"close":{"symbol":"ES","fill":{"avg_px":5000,"total_contracts":2,"oid":"ord-1"}},"platform":"topstep","timestamp":"2026-04-19T10:00:00Z"}`,
			check: func(t *testing.T, result *TopStepCloseResult) {
				if result == nil || result.Close == nil || result.Close.Symbol != "ES" {
					t.Errorf("unexpected result: %+v", result)
				}
				if result.Close.Fill == nil || result.Close.Fill.TotalContracts != 2 {
					t.Errorf("Fill = %+v, want TotalContracts=2", result.Close.Fill)
				}
			},
		},
		{
			name:    "exit 0 with error field",
			stdout:  `{"close":{"symbol":"ES","fill":{}},"platform":"topstep","timestamp":"x","error":"venue down"}`,
			wantErr: true,
		},
		{
			name:    "non-zero exit with error envelope",
			stdout:  `{"close":{"symbol":"ES","fill":{}},"platform":"topstep","timestamp":"x","error":"market closed"}`,
			exitErr: fmt.Errorf("exit 1"),
			wantErr: true,
		},
		{
			name:    "non-zero exit with no error field",
			stdout:  `{"close":{"symbol":"ES","fill":{}},"platform":"topstep","timestamp":"x"}`,
			stderr:  "stderr msg",
			exitErr: fmt.Errorf("exit 2"),
			wantErr: true,
		},
		{
			name:    "malformed json",
			stdout:  `not json`,
			wantErr: true,
			check: func(t *testing.T, result *TopStepCloseResult) {
				if result != nil {
					t.Errorf("expected nil result on parse failure, got %+v", result)
				}
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			result, _, err := parseTopStepCloseOutput([]byte(tc.stdout), tc.stderr, tc.exitErr)
			if tc.wantErr && err == nil {
				t.Fatalf("expected non-nil err")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("expected nil err, got %v", err)
			}
			if tc.check != nil {
				tc.check(t, result)
			}
		})
	}
}

func TestComputeTopStepCircuitCloseQty(t *testing.T) {
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
}

func TestRunPendingTopStepCircuitCloses_RecoversStuckCB(t *testing.T) {
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
}

func TestRunPendingTopStepCircuitCloses_CloseErrorLatchesPending(t *testing.T) {
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
}

func TestRunPendingTopStepCircuitCloses_FetcherErrorBails(t *testing.T) {
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
}

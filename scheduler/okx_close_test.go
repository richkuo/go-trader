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

func TestForceCloseOKXLive_CloseErrorLatches(t *testing.T) {
	okxLive := []StrategyConfig{
		{ID: "okx-btc", Platform: "okx", Type: "perps",
			Args: []string{"sma", "BTC", "1h", "--mode=live"}},
	}
	positions := []OKXPosition{{Coin: "BTC", Size: 0.01, Side: "long"}}
	closer := func(sym string, partialSz *float64) (*OKXCloseResult, error) {
		return nil, fmt.Errorf("okx 503")
	}

	report := forceCloseOKXLive(context.Background(), positions, okxLive, closer)

	if report.ConfirmedFlat() {
		t.Fatal("expected NOT ConfirmedFlat on close error")
	}
	if _, ok := report.Errors["BTC"]; !ok {
		t.Errorf("expected BTC in errors, got %v", report.Errors)
	}
}

func TestForceCloseOKXLive_ZeroSizeMarkedAlreadyFlat(t *testing.T) {

	okxLive := []StrategyConfig{
		{ID: "okx-btc", Platform: "okx", Type: "perps",
			Args: []string{"sma", "BTC", "1h", "--mode=live"}},
	}
	positions := []OKXPosition{{Coin: "BTC", Size: 0, Side: ""}}
	var calls []string
	closer := func(sym string, partialSz *float64) (*OKXCloseResult, error) {
		calls = append(calls, sym)
		return &OKXCloseResult{}, nil
	}

	report := forceCloseOKXLive(context.Background(), positions, okxLive, closer)

	if len(calls) != 0 {
		t.Errorf("zero-size position must short-circuit before closer, got calls=%v", calls)
	}
	if len(report.AlreadyFlat) != 1 || report.AlreadyFlat[0] != "BTC" {
		t.Errorf("AlreadyFlat = %v, want [BTC]", report.AlreadyFlat)
	}
	if !report.ConfirmedFlat() {
		t.Errorf("zero-size short-circuit must be ConfirmedFlat, got errors=%v", report.Errors)
	}
}

func TestForceCloseOKXLive_CtxExpiredBeforeSubmit(t *testing.T) {

	okxLive := []StrategyConfig{
		{ID: "okx-btc", Platform: "okx", Type: "perps",
			Args: []string{"sma", "BTC", "1h", "--mode=live"}},
	}
	positions := []OKXPosition{{Coin: "BTC", Size: 0.01, Side: "long"}}
	ctx, cancel := context.WithTimeout(context.Background(), 0)
	cancel()
	var calls []string
	closer := func(sym string, partialSz *float64) (*OKXCloseResult, error) {
		calls = append(calls, sym)
		return &OKXCloseResult{}, nil
	}

	report := forceCloseOKXLive(ctx, positions, okxLive, closer)

	if len(calls) != 0 {
		t.Errorf("closer must not be called after ctx expires, got %v", calls)
	}
	if _, ok := report.Errors["BTC"]; !ok {
		t.Errorf("expected BTC to be marked as error on ctx expiry, got %v", report.Errors)
	}
}

func TestForceCloseOKXLive_SpotStrategiesIgnored(t *testing.T) {

	mixed := []StrategyConfig{
		{ID: "okx-btc-spot", Platform: "okx", Type: "spot",
			Args: []string{"sma", "BTC", "1h", "--mode=live", "--inst-type=spot"}},
	}
	positions := []OKXPosition{{Coin: "BTC", Size: 0.01, Side: "long"}}
	var calls []string
	closer := func(sym string, partialSz *float64) (*OKXCloseResult, error) {
		calls = append(calls, sym)
		return &OKXCloseResult{}, nil
	}

	report := forceCloseOKXLive(context.Background(), positions, mixed, closer)

	if len(calls) != 0 {
		t.Errorf("spot strategy must not drive perps close, got calls=%v", calls)
	}
	if !report.ConfirmedFlat() {
		t.Errorf("spot-only config with non-traded perps position is ConfirmedFlat for the OKX perps branch, got errors=%v", report.Errors)
	}
}

func TestForceCloseOKXLive_AdapterAlreadyFlatRoutedCorrectly(t *testing.T) {
	okxLive := []StrategyConfig{
		{ID: "okx-btc", Platform: "okx", Type: "perps",
			Args: []string{"sma", "BTC", "1h", "--mode=live"}},
	}
	positions := []OKXPosition{{Coin: "BTC", Size: 0.01, Side: "long"}}
	var calls []string
	closer := func(sym string, partialSz *float64) (*OKXCloseResult, error) {
		calls = append(calls, sym)
		return &OKXCloseResult{
			Close:    &OKXClose{Symbol: sym, AlreadyFlat: true},
			Platform: "okx",
		}, nil
	}

	report := forceCloseOKXLive(context.Background(), positions, okxLive, closer)

	if !report.ConfirmedFlat() {
		t.Errorf("expected ConfirmedFlat, got errors=%v", report.Errors)
	}
	if len(report.ClosedCoins) != 0 {
		t.Errorf("ClosedCoins should be empty when adapter reports already_flat, got %v", report.ClosedCoins)
	}
	if len(report.AlreadyFlat) != 1 || report.AlreadyFlat[0] != "BTC" {
		t.Errorf("AlreadyFlat = %v, want [BTC]", report.AlreadyFlat)
	}
	if len(calls) != 1 || calls[0] != "BTC" {
		t.Errorf("closer should be called once (Go side saw non-zero size), got %v", calls)
	}
}

func TestOKXLiveCloseReport_SortedErrorCoins(t *testing.T) {
	r := OKXLiveCloseReport{Errors: map[string]error{
		"SOL": fmt.Errorf("e"), "BTC": fmt.Errorf("e"), "ETH": fmt.Errorf("e"),
	}}
	coins := r.SortedErrorCoins()
	want := []string{"BTC", "ETH", "SOL"}
	if len(coins) != len(want) {
		t.Fatalf("len = %d, want %d", len(coins), len(want))
	}
	for i, c := range coins {
		if c != want[i] {
			t.Errorf("coins[%d] = %q, want %q", i, c, want[i])
		}
	}
}

func TestComputeOKXCircuitCloseQty_SoleOwnerFullSzi(t *testing.T) {
	okxLive := []StrategyConfig{
		{ID: "okx-eth", Platform: "okx", Type: "perps",
			Args: []string{"sma", "ETH", "1h", "--mode=live"}},
	}
	pos := []OKXPosition{{Coin: "ETH", Size: -0.4, EntryPrice: 3000, Side: "short"}}
	q, ok := computeOKXCircuitCloseQty("ETH", "okx-eth", pos, okxLive)
	if !ok {
		t.Fatal("expected ok")
	}
	if math.Abs(q-0.4) > 1e-9 {
		t.Errorf("qty=%.6f want 0.4 (full abs size for sole owner)", q)
	}
}

func TestComputeOKXCircuitCloseQty_Shared50_50(t *testing.T) {
	okxLive := []StrategyConfig{
		{ID: "okx-a", Platform: "okx", Type: "perps", CapitalPct: 0.5, Capital: 1000,
			Args: []string{"sma", "ETH", "1h", "--mode=live"}},
		{ID: "okx-b", Platform: "okx", Type: "perps", CapitalPct: 0.5, Capital: 1000,
			Args: []string{"ema", "ETH", "1h", "--mode=live"}},
	}
	pos := []OKXPosition{{Coin: "ETH", Size: 0.517, EntryPrice: 3000, Side: "long"}}
	q, ok := computeOKXCircuitCloseQty("ETH", "okx-a", pos, okxLive)
	if !ok {
		t.Fatal("expected ok")
	}
	want := 0.517 * 0.5
	if math.Abs(q-want) > 1e-9 {
		t.Errorf("qty=%.6f want %.6f", q, want)
	}
}

func TestComputeOKXCircuitCloseQty_MixedUnitsFallsBackToEqualWeights(t *testing.T) {
	okxLive := []StrategyConfig{
		{ID: "okx-a", Platform: "okx", Type: "perps", CapitalPct: 0.5,
			Args: []string{"sma", "ETH", "1h", "--mode=live"}},
		{ID: "okx-b", Platform: "okx", Type: "perps", Capital: 1000,
			Args: []string{"ema", "ETH", "1h", "--mode=live"}},
	}
	pos := []OKXPosition{{Coin: "ETH", Size: 0.5, EntryPrice: 3000, Side: "long"}}
	q, ok := computeOKXCircuitCloseQty("ETH", "okx-a", pos, okxLive)
	if !ok {
		t.Fatal("expected ok")
	}

	want := 0.25
	if math.Abs(q-want) > 1e-9 {
		t.Errorf("qty=%.6f want %.6f (equal-weight fallback on mixed units)", q, want)
	}
}

func TestComputeOKXCircuitCloseQty_NoPositionReturnsFalse(t *testing.T) {
	okxLive := []StrategyConfig{
		{ID: "okx-eth", Platform: "okx", Type: "perps",
			Args: []string{"sma", "ETH", "1h", "--mode=live"}},
	}
	pos := []OKXPosition{{Coin: "BTC", Size: 0.1, EntryPrice: 42000, Side: "long"}}
	q, ok := computeOKXCircuitCloseQty("ETH", "okx-eth", pos, okxLive)
	if ok {
		t.Errorf("expected ok=false when no on-chain position for coin, got qty=%v", q)
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

func TestRunPendingOKXCircuitCloses_StuckCBNoOnChainPositionIsNoOp(t *testing.T) {
	state := &AppState{
		Strategies: map[string]*StrategyState{
			"okx-a": {
				ID: "okx-a",
				RiskState: RiskState{
					CircuitBreaker:      true,
					CircuitBreakerUntil: time.Now().Add(24 * time.Hour),
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
		calls = append(calls, sym)
		return &OKXCloseResult{Close: &OKXClose{Symbol: sym}, Platform: "okx"}, nil
	}
	runPendingOKXCircuitCloses(
		context.Background(),
		state,
		cfg,
		true,
		nil,
		true,
		nil,
		closer,
		30*time.Second,
		&mu,
		nil,
	)
	if len(calls) != 0 {
		t.Errorf("expected no closer calls when no on-chain position, got %v", calls)
	}
	if state.Strategies["okx-a"].RiskState.getPendingCircuitClose(PlatformPendingCloseOKX) != nil {
		t.Error("pending should remain nil when recovery has no on-chain position to close")
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

func TestRunPendingOKXCircuitCloses_StaleStrategyClearsPending(t *testing.T) {
	state := &AppState{
		Strategies: map[string]*StrategyState{
			"okx-gone": {
				ID: "okx-gone",
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
	cfg := []StrategyConfig{}
	var mu sync.RWMutex
	var calls []string
	closer := func(sym string, partialSz *float64) (*OKXCloseResult, error) {
		calls = append(calls, sym)
		return &OKXCloseResult{Close: &OKXClose{Symbol: sym}}, nil
	}
	runPendingOKXCircuitCloses(
		context.Background(),
		state,
		cfg,
		true,
		[]OKXPosition{{Coin: "ETH", Size: 0.5, Side: "long"}},
		true,
		nil,
		closer,
		30*time.Second,
		&mu,
		nil,
	)
	if len(calls) != 0 {
		t.Errorf("closer must not be called for stale strategy, got %v", calls)
	}
	if state.Strategies["okx-gone"].RiskState.getPendingCircuitClose(PlatformPendingCloseOKX) != nil {
		t.Error("stale pending should be cleared")
	}
}

func TestSetOKXCircuitBreakerPending_EnqueuesForLivePerps(t *testing.T) {
	sc := StrategyConfig{ID: "okx-a", Platform: "okx", Type: "perps",
		Args: []string{"sma", "ETH", "1h", "--mode=live"}}
	s := &StrategyState{
		ID: "okx-a",
		Positions: map[string]*Position{
			"ETH": {Quantity: 0.25, Side: "long"},
		},
	}
	assist := &PlatformRiskAssist{
		OKXPositions: []OKXPosition{{Coin: "ETH", Size: 0.25, Side: "long"}},
		OKXLiveAll:   []StrategyConfig{sc},
	}
	setOKXCircuitBreakerPending(&sc, s, assist)
	p := s.RiskState.getPendingCircuitClose(PlatformPendingCloseOKX)
	if p == nil {
		t.Fatal("expected pending entry to be enqueued")
	}
	if len(p.Symbols) != 1 || p.Symbols[0].Symbol != "ETH" || math.Abs(p.Symbols[0].Size-0.25) > 1e-9 {
		t.Errorf("pending=%+v, want [ETH:0.25]", p.Symbols)
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

func TestSetOKXCircuitBreakerPending_NilAssistIsNoOp(t *testing.T) {
	sc := StrategyConfig{ID: "okx-a", Platform: "okx", Type: "perps",
		Args: []string{"sma", "ETH", "1h", "--mode=live"}}
	s := &StrategyState{ID: "okx-a",
		Positions: map[string]*Position{"ETH": {Quantity: 0.25, Side: "long"}}}
	setOKXCircuitBreakerPending(&sc, s, nil)
	if s.RiskState.getPendingCircuitClose(PlatformPendingCloseOKX) != nil {
		t.Error("nil assist must no-op")
	}
}

func TestParseOKXBalanceOutput_CleanSuccess(t *testing.T) {
	stdout := []byte(`{"balance":1234.56,"platform":"okx","timestamp":"2026-04-20T00:00:00Z"}`)
	r, _, err := parseOKXBalanceOutput(stdout, "", nil)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if math.Abs(r.Balance-1234.56) > 1e-9 {
		t.Errorf("balance=%v want 1234.56", r.Balance)
	}
}

func TestParseOKXBalanceOutput_ErrorEnvelopeSurfacesAsErr(t *testing.T) {
	stdout := []byte(`{"balance":0,"platform":"okx","timestamp":"x","error":"auth failed"}`)
	_, _, err := parseOKXBalanceOutput(stdout, "", fmt.Errorf("exit 1"))
	if err == nil {
		t.Fatal("expected non-nil err for error envelope")
	}
}

func TestRunPendingOKXCircuitCloses_FailureIncrementsCountAndNotifies(t *testing.T) {
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
	var dmMsgs []string
	ownerDM := func(msg string) { dmMsgs = append(dmMsgs, msg) }
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
		ownerDM,
	)
	p := state.Strategies["okx-a"].RiskState.getPendingCircuitClose(PlatformPendingCloseOKX)
	if p == nil {
		t.Fatal("pending should be preserved on failure")
	}
	if p.ConsecutiveFailures != 1 {
		t.Errorf("ConsecutiveFailures: got %d, want 1", p.ConsecutiveFailures)
	}
	if len(dmMsgs) != 1 {
		t.Errorf("expected 1 DM on first failure, got %d", len(dmMsgs))
	}
}

func TestRunPendingOKXCircuitCloses_RepeatedFailureThrottlesNotifier(t *testing.T) {
	state := &AppState{
		Strategies: map[string]*StrategyState{
			"okx-a": {
				ID: "okx-a",
				RiskState: RiskState{
					PendingCircuitCloses: map[string]*PendingCircuitClose{
						PlatformPendingCloseOKX: {
							Symbols:             []PendingCircuitCloseSymbol{{Symbol: "ETH", Size: 0.1}},
							ConsecutiveFailures: 1,
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
	var dmMsgs []string
	ownerDM := func(msg string) { dmMsgs = append(dmMsgs, msg) }

	state.Strategies["okx-a"].RiskState.PendingCircuitCloses[PlatformPendingCloseOKX].LastNotifiedAt = time.Now()

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
		ownerDM,
	)
	if len(dmMsgs) != 0 {
		t.Errorf("expected 0 DMs on failure #2 (suppressed), got %d", len(dmMsgs))
	}
}

func TestRunPendingOKXCircuitCloses_TenthFailureNotifies(t *testing.T) {
	state := &AppState{
		Strategies: map[string]*StrategyState{
			"okx-a": {
				ID: "okx-a",
				RiskState: RiskState{
					PendingCircuitCloses: map[string]*PendingCircuitClose{
						PlatformPendingCloseOKX: {
							Symbols:             []PendingCircuitCloseSymbol{{Symbol: "ETH", Size: 0.1}},
							ConsecutiveFailures: 9,
							LastNotifiedAt:      time.Now(),
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
	var dmMsgs []string
	ownerDM := func(msg string) { dmMsgs = append(dmMsgs, msg) }
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
		ownerDM,
	)
	p := state.Strategies["okx-a"].RiskState.getPendingCircuitClose(PlatformPendingCloseOKX)
	if p == nil || p.ConsecutiveFailures != 10 {
		t.Fatalf("expected ConsecutiveFailures=10, got %v", p)
	}
	if len(dmMsgs) != 1 {
		t.Errorf("expected 1 DM on failure #10 (every-10th cadence), got %d", len(dmMsgs))
	}
}

func TestRunPendingOKXCircuitCloses_CtxExpiryMidLoopDoesNotCountAsFailure(t *testing.T) {
	state := &AppState{
		Strategies: map[string]*StrategyState{
			"okx-a": {
				ID: "okx-a",
				RiskState: RiskState{
					PendingCircuitCloses: map[string]*PendingCircuitClose{
						PlatformPendingCloseOKX: {
							Symbols: []PendingCircuitCloseSymbol{
								{Symbol: "BTC", Size: 0.01},
								{Symbol: "ETH", Size: 0.1},
							},
							ConsecutiveFailures: 3,
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
	ctx, cancel := context.WithCancel(context.Background())

	var calls []string
	closer := func(sym string, partialSz *float64) (*OKXCloseResult, error) {
		calls = append(calls, sym)

		cancel()
		return &OKXCloseResult{Close: &OKXClose{Symbol: sym}}, nil
	}
	var dmMsgs []string
	ownerDM := func(msg string) { dmMsgs = append(dmMsgs, msg) }

	runPendingOKXCircuitCloses(
		ctx,
		state,
		cfg,
		true,
		[]OKXPosition{
			{Coin: "BTC", Size: 0.01, EntryPrice: 1, Side: "long"},
			{Coin: "ETH", Size: 0.5, EntryPrice: 1, Side: "long"},
		},
		true,
		nil,
		closer,
		30*time.Second,
		&mu,
		ownerDM,
	)

	if len(calls) != 1 {
		t.Errorf("expected exactly 1 closer call before ctx expiry, got %d (%v)", len(calls), calls)
	}
	if len(dmMsgs) != 0 {
		t.Errorf("expected 0 DMs on mid-loop ctx expiry (no real failure), got %d (%v)", len(dmMsgs), dmMsgs)
	}
	p := state.Strategies["okx-a"].RiskState.getPendingCircuitClose(PlatformPendingCloseOKX)
	if p == nil {
		t.Fatal("pending must be preserved on ctx expiry")
	}
	if p.ConsecutiveFailures != 3 {
		t.Errorf("ConsecutiveFailures must not increment on ctx expiry: got %d, want 3", p.ConsecutiveFailures)
	}
}

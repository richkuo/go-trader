package main

import (
	"context"
	"fmt"
	"math"
	"sync"
	"testing"
	"time"
)

// forceCloseOKXLive unit tests — mirror the HL tests in
// hyperliquid_balance_test.go (TestForceCloseHyperliquidLive_*). Each
// test asserts a single branch of the decision logic so a regression
// produces a targeted failure rather than a conflated ambiguous signal.

func TestForceCloseOKXLive_ClosesOwnedCoinsOnly(t *testing.T) {
	okxLive := []StrategyConfig{
		{ID: "okx-btc", Platform: "okx", Type: "perps",
			Args: []string{"sma", "BTC", "1h", "--mode=live"}},
	}
	positions := []OKXPosition{
		{Coin: "BTC", Size: 0.01, Side: "long"},
		{Coin: "SOL", Size: 50, Side: "long"}, // unowned: no configured strategy
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
	// Unowned positions must be surfaced via Unconfigured so the kill-switch
	// plan can latch for manual intervention (dedup: single source of truth
	// for the traded-coins partition lives in forceCloseOKXLive, not the
	// plan builder).
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
	// Defense-in-depth: fetcher filters size==0 upstream, but if it ever
	// loosens we must not submit a zero-size order (OKX would reject it
	// and the kill switch would latch on a false failure).
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
	// When the overall budget expires, remaining coins must be flagged as
	// errors so the kill switch latches and retries next cycle — rather
	// than silently skipping them and clearing virtual state.
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
	// Spot strategies live in OKXLiveAllSpot and must NOT appear in the
	// perps close loop — forceCloseOKXLive should not attempt to "close"
	// spot coins. Guards against a future refactor that flattens the
	// perps/spot partition and triggers unsafe sell-all behavior.
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

// Adapter-side AlreadyFlat: closer returns success with already_flat=true
// (eventual-consistency window between Go-side fetch and adapter submit).
// The coin must land in AlreadyFlat, NOT ClosedCoins, so operator messaging
// distinguishes "we sent a close order" from "nothing to close" (#350).
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

// SortedErrorCoins determinism — same rationale as HL
// (HyperliquidLiveCloseReport.SortedErrorCoins): Go map iteration is
// randomized and the Discord output must be byte-stable across calls for
// operator triage.
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

// ─────────────────────────────────────────────────────────────────────
// Per-strategy circuit-breaker close (phase 2 of #357, issue #360).
// Mirrors the HL coverage in hyperliquid_balance_test.go — sizing math,
// stuck-CB recovery, and clear-on-success are the three load-bearing
// invariants for per-strategy CB on shared OKX wallets.
// ─────────────────────────────────────────────────────────────────────

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

// Mixed-units weight normalization: when peers on a shared coin declare
// weights in different fields (fractional CapitalPct vs absolute Capital),
// the sum is nonsensical. Fall back to equal weights so the firing strategy
// still gets a meaningful share. Mirrors the HL invariant (#356 review
// finding 3) — regression here would silently collapse OKX shared-coin
// closes to near-zero reduce-only orders.
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
	// With equal 1.0/1.0 fallback, okx-a gets half of |size| = 0.25.
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

// Recovery after OKX-fetch-fail at CB fire time. When the position fetch
// fails on the cycle a CB first fires, setOKXCircuitBreakerPending bails on
// the nil assist and the pending close is never set. Subsequent cycles must
// detect the stuck state (CB active, pending nil, live OKX perps, non-zero
// on-chain position) and reconstruct the pending so the reduce-only close
// eventually fires. Mirror of the HL recovery test.
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

// If the stuck-CB strategy has no on-chain position (e.g. operator already
// closed it manually), recovery must be a no-op rather than submitting a
// zero-size order.
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

// On closer failure, pending must NOT be cleared — the kill switch latches
// and retries next cycle. Same contract as the HL drain.
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

// When a pending entry references a strategy that is no longer configured as
// live OKX perps (e.g. operator removed it from config between cycles), drain
// must silently clear the stale entry and not submit any close.
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

// setOKXCircuitBreakerPending enqueues for a live OKX perps strategy with
// an open virtual position and non-zero on-chain position.
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

// Paper-mode OKX strategies must NOT enqueue a pending close — kill switch
// is meaningful only against live exposure.
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

// Nil assist (e.g. OKX fetch failed this cycle) must no-op so the stuck-CB
// recovery path in runPendingOKXCircuitCloses can reconstruct later.
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

// Parser for fetch_okx_balance.py must surface any error (exit nonzero,
// populated error field, or unparseable) as non-nil error so the
// shared-wallet auto-clear path preserves the kill switch on uncertainty.
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

// TestRunPendingOKXCircuitCloses_FailureIncrementsCountAndNotifies verifies that
// a single failed close attempt increments ConsecutiveFailures to 1 and fires the
// notifier exactly once (#427).
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

// TestRunPendingOKXCircuitCloses_RepeatedFailureThrottlesNotifier verifies that
// failures 2–9 do not fire the notifier (throttle suppresses repeats).
func TestRunPendingOKXCircuitCloses_RepeatedFailureThrottlesNotifier(t *testing.T) {
	state := &AppState{
		Strategies: map[string]*StrategyState{
			"okx-a": {
				ID: "okx-a",
				RiskState: RiskState{
					PendingCircuitCloses: map[string]*PendingCircuitClose{
						PlatformPendingCloseOKX: {
							Symbols:             []PendingCircuitCloseSymbol{{Symbol: "ETH", Size: 0.1}},
							ConsecutiveFailures: 1, // first failure already recorded
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
	// Force LastNotifiedAt to just now so hourly gate doesn't fire.
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

// TestRunPendingOKXCircuitCloses_TenthFailureNotifies verifies that failure #10
// fires the notifier (every-10th cadence).
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

// Regression: when ctxOverall trips mid-symbol-loop (e.g. previous symbol's
// closer consumed the budget), the inner per-symbol ctx check sets
// allOK=false but failedErr stays nil. The post-loop block must NOT
// increment ConsecutiveFailures and must NOT dereference failedErr (that
// would panic). Mirrors the HL drain's `drainError` flag semantic and the
// RH drain's `else if failedErr != nil` guard. See PR #435 review.
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
		// First closer call succeeds AND cancels the budget so the inner
		// ctx check fires before the second symbol runs — exactly the
		// nil-failedErr-with-allOK=false branch the bug reproduces.
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

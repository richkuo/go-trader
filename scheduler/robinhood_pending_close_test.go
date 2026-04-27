package main

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

// Sole-owner full-close sizing: single live configured RH crypto strategy on
// the coin → drain submits a market_sell for the entire on-account balance.
func TestRunPendingRobinhoodCircuitCloses_SoleOwnerFullClose(t *testing.T) {
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
	var calls []string
	closer := func(sym string) (*RobinhoodCloseResult, error) {
		calls = append(calls, sym)
		return &RobinhoodCloseResult{
			Close:    &RobinhoodClose{Symbol: sym},
			Platform: "robinhood",
		}, nil
	}
	var dms []string
	dm := func(msg string) { dms = append(dms, msg) }

	runPendingRobinhoodCircuitCloses(
		context.Background(),
		state,
		cfg,
		[]RobinhoodPosition{{Coin: "BTC", Size: 0.02, AvgPrice: 42000}},
		true,
		nil,
		closer,
		dm,
		30*time.Second,
		&mu,
		nil,
	)

	if len(calls) != 1 || calls[0] != "BTC" {
		t.Errorf("closer calls=%v want [BTC]", calls)
	}
	if len(dms) != 0 {
		t.Errorf("unexpected DMs on sole-owner path: %v", dms)
	}
	if state.Strategies["rh-sma-btc"].RiskState.getPendingCircuitClose(PlatformPendingCloseRobinhood) != nil {
		t.Error("expected pending cleared after successful close")
	}
}

// Stuck-CB recovery: CB active + no pending + on-account position + fetch
// succeeds this cycle → drain reconstructs pending and closes it.
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
		nil,
	)

	if len(calls) != 1 || calls[0] != "BTC" {
		t.Errorf("expected recovered close for BTC, got %v", calls)
	}
	if state.Strategies["rh-sma-btc"].RiskState.getPendingCircuitClose(PlatformPendingCloseRobinhood) != nil {
		t.Error("expected pending cleared after recovered close")
	}
}

// Stuck-CB recovery with no on-account position (operator manually closed)
// must be a no-op rather than submitting a zero-size sell.
func TestRunPendingRobinhoodCircuitCloses_StuckCBNoOnAccountPositionIsNoOp(t *testing.T) {
	state := &AppState{
		Strategies: map[string]*StrategyState{
			"rh-sma-btc": {
				ID: "rh-sma-btc",
				RiskState: RiskState{
					CircuitBreaker:      true,
					CircuitBreakerUntil: time.Now().Add(24 * time.Hour),
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
		return &RobinhoodCloseResult{Close: &RobinhoodClose{Symbol: sym}}, nil
	}

	runPendingRobinhoodCircuitCloses(
		context.Background(),
		state,
		cfg,
		nil,
		true,
		nil,
		closer,
		nil,
		30*time.Second,
		&mu,
		nil,
	)

	if len(calls) != 0 {
		t.Errorf("expected no closer calls when no on-account position, got %v", calls)
	}
	if state.Strategies["rh-sma-btc"].RiskState.getPendingCircuitClose(PlatformPendingCloseRobinhood) != nil {
		t.Error("pending should remain nil when recovery has no position to close")
	}
}

// Shared-ownership gate: two live RH crypto strategies trade the same coin on
// the same account → drain must NOT submit a close, must DM the owner, and
// must clear the pending so stuck-CB recovery controls the next cycle's DM.
func TestRunPendingRobinhoodCircuitCloses_SharedOwnershipSkipsAndDMs(t *testing.T) {
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
		{ID: "rh-ema-btc", Platform: "robinhood", Type: "spot",
			Args: []string{"ema_crossover", "BTC", "1h", "--mode=live"}},
	}
	var mu sync.RWMutex
	var calls []string
	closer := func(sym string) (*RobinhoodCloseResult, error) {
		calls = append(calls, sym)
		return &RobinhoodCloseResult{Close: &RobinhoodClose{Symbol: sym}}, nil
	}
	var dms []string
	dm := func(msg string) { dms = append(dms, msg) }

	runPendingRobinhoodCircuitCloses(
		context.Background(),
		state,
		cfg,
		[]RobinhoodPosition{{Coin: "BTC", Size: 0.02, AvgPrice: 42000}},
		true,
		nil,
		closer,
		dm,
		30*time.Second,
		&mu,
		nil,
	)

	if len(calls) != 0 {
		t.Errorf("expected NO closer submissions on shared-ownership, got %v", calls)
	}
	if len(dms) != 1 {
		t.Fatalf("expected exactly one owner DM, got %d: %v", len(dms), dms)
	}
	// DM must name firing strategy, coin, and list both peer IDs in sorted order.
	msg := dms[0]
	if !strings.Contains(msg, "rh-sma-btc") {
		t.Errorf("DM missing firing strategy ID: %q", msg)
	}
	if !strings.Contains(msg, "BTC") {
		t.Errorf("DM missing coin: %q", msg)
	}
	if !strings.Contains(msg, "rh-ema-btc") {
		t.Errorf("DM missing peer strategy: %q", msg)
	}
	if state.Strategies["rh-sma-btc"].RiskState.getPendingCircuitClose(PlatformPendingCloseRobinhood) != nil {
		t.Error("expected pending cleared on shared-ownership skip so recovery controls next-cycle DMs")
	}
}

// Stuck-CB recovery must apply the sole-ownership gate before enqueueing —
// otherwise shared-coin setups would silently latch pending state that the
// submit phase would then immediately clear, producing churn without a DM.
func TestRunPendingRobinhoodCircuitCloses_StuckCBSharedOwnershipSkipDMs(t *testing.T) {
	state := &AppState{
		Strategies: map[string]*StrategyState{
			"rh-sma-btc": {
				ID: "rh-sma-btc",
				RiskState: RiskState{
					CircuitBreaker:      true,
					CircuitBreakerUntil: time.Now().Add(24 * time.Hour),
				},
			},
		},
	}
	cfg := []StrategyConfig{
		{ID: "rh-sma-btc", Platform: "robinhood", Type: "spot",
			Args: []string{"sma_crossover", "BTC", "1h", "--mode=live"}},
		{ID: "rh-ema-btc", Platform: "robinhood", Type: "spot",
			Args: []string{"ema_crossover", "BTC", "1h", "--mode=live"}},
	}
	var mu sync.RWMutex
	var calls []string
	closer := func(sym string) (*RobinhoodCloseResult, error) {
		calls = append(calls, sym)
		return &RobinhoodCloseResult{Close: &RobinhoodClose{Symbol: sym}}, nil
	}
	var dms []string
	dm := func(msg string) { dms = append(dms, msg) }

	runPendingRobinhoodCircuitCloses(
		context.Background(),
		state,
		cfg,
		[]RobinhoodPosition{{Coin: "BTC", Size: 0.02, AvgPrice: 42000}},
		true,
		nil,
		closer,
		dm,
		30*time.Second,
		&mu,
		nil,
	)

	if len(calls) != 0 {
		t.Errorf("expected NO submissions on shared-ownership stuck CB, got %v", calls)
	}
	if len(dms) != 1 {
		t.Fatalf("expected one DM from recovery-time shared-owner gate, got %d: %v", len(dms), dms)
	}
	if state.Strategies["rh-sma-btc"].RiskState.getPendingCircuitClose(PlatformPendingCloseRobinhood) != nil {
		t.Error("shared-ownership recovery must NOT enqueue pending")
	}
}

// Submit error preserves pending so the next cycle retries (parity with HL).
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
		nil,
	)

	pending := state.Strategies["rh-sma-btc"].RiskState.getPendingCircuitClose(PlatformPendingCloseRobinhood)
	if pending == nil {
		t.Fatal("expected pending preserved on submit error so next cycle retries")
	}
	if len(pending.Symbols) != 1 || pending.Symbols[0].Symbol != "BTC" {
		t.Errorf("pending = %+v, want [BTC]", pending)
	}
}

// AlreadyFlat response from the adapter must clear the pending without
// erroring, mirroring the HL/OKX contract.
func TestRunPendingRobinhoodCircuitCloses_AlreadyFlatClearsPending(t *testing.T) {
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
		return &RobinhoodCloseResult{
			Close:    &RobinhoodClose{Symbol: sym, AlreadyFlat: true},
			Platform: "robinhood",
		}, nil
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
		nil,
	)

	if state.Strategies["rh-sma-btc"].RiskState.getPendingCircuitClose(PlatformPendingCloseRobinhood) != nil {
		t.Error("expected pending cleared on already_flat response")
	}
}

// setRobinhoodCircuitBreakerPending enqueues on fresh CB fire when assist has
// the cycle's RH positions. Validates the sc/state preconditions.
func TestSetRobinhoodCircuitBreakerPending_EnqueuesWhenOnAccountPositionExists(t *testing.T) {
	sc := &StrategyConfig{
		ID: "rh-sma-btc", Platform: "robinhood", Type: "spot",
		Args: []string{"sma_crossover", "BTC", "1h", "--mode=live"},
	}
	s := &StrategyState{
		ID:        "rh-sma-btc",
		Positions: map[string]*Position{"BTC": {Quantity: 0.02, Side: "long", AvgCost: 42000}},
	}
	assist := &PlatformRiskAssist{
		RHPositions: []RobinhoodPosition{{Coin: "BTC", Size: 0.02}},
		RHLiveAll:   []StrategyConfig{*sc},
	}

	setRobinhoodCircuitBreakerPending(sc, s, assist)

	pending := s.RiskState.getPendingCircuitClose(PlatformPendingCloseRobinhood)
	if pending == nil {
		t.Fatal("expected pending enqueued")
	}
	if len(pending.Symbols) != 1 || pending.Symbols[0].Symbol != "BTC" {
		t.Errorf("pending = %+v, want [BTC]", pending)
	}
	if pending.Symbols[0].Size != 0.02 {
		t.Errorf("pending size = %v, want 0.02 (full on-account)", pending.Symbols[0].Size)
	}
}

// setRobinhoodCircuitBreakerPending is a no-op when assist lacks the RH
// positions snapshot (cycle-local fetch failure). Stuck-CB recovery picks it
// up on the next cycle.
func TestSetRobinhoodCircuitBreakerPending_NoOpWhenAssistMissingPositions(t *testing.T) {
	sc := &StrategyConfig{
		ID: "rh-sma-btc", Platform: "robinhood", Type: "spot",
		Args: []string{"sma_crossover", "BTC", "1h", "--mode=live"},
	}
	s := &StrategyState{
		ID:        "rh-sma-btc",
		Positions: map[string]*Position{"BTC": {Quantity: 0.02, Side: "long", AvgCost: 42000}},
	}
	assist := &PlatformRiskAssist{}

	setRobinhoodCircuitBreakerPending(sc, s, assist)

	if s.RiskState.getPendingCircuitClose(PlatformPendingCloseRobinhood) != nil {
		t.Error("expected no-op when assist.RHPositions is nil")
	}
}

// Paper-mode and cross-platform strategies must never enqueue RH pending.
func TestSetRobinhoodCircuitBreakerPending_IgnoresNonLiveAndNonRobinhood(t *testing.T) {
	paper := &StrategyConfig{
		ID: "rh-paper", Platform: "robinhood", Type: "spot",
		Args: []string{"sma_crossover", "BTC", "1h"},
	}
	hl := &StrategyConfig{
		ID: "hl-eth", Platform: "hyperliquid", Type: "perps",
		Args: []string{"sma_crossover", "ETH", "1h", "--mode=live"},
	}
	s := &StrategyState{
		Positions: map[string]*Position{"BTC": {Quantity: 0.02}, "ETH": {Quantity: 0.1}},
	}
	assist := &PlatformRiskAssist{
		RHPositions: []RobinhoodPosition{{Coin: "BTC", Size: 0.02}, {Coin: "ETH", Size: 0.1}},
	}

	setRobinhoodCircuitBreakerPending(paper, s, assist)
	setRobinhoodCircuitBreakerPending(hl, s, assist)

	if s.RiskState.getPendingCircuitClose(PlatformPendingCloseRobinhood) != nil {
		t.Error("expected no pending for paper or non-RH strategies")
	}
}

// rhLiveStrategiesForCoin basics — used by the drain's sole-owner gate.
func TestRhLiveStrategiesForCoin(t *testing.T) {
	roster := []StrategyConfig{
		{ID: "rh-sma-btc", Args: []string{"sma", "BTC", "1h", "--mode=live"}},
		{ID: "rh-ema-btc", Args: []string{"ema", "BTC", "1h", "--mode=live"}},
		{ID: "rh-sma-eth", Args: []string{"sma", "ETH", "1h", "--mode=live"}},
	}
	got := rhLiveStrategiesForCoin("BTC", roster)
	if len(got) != 2 {
		t.Fatalf("BTC peers = %d, want 2", len(got))
	}
	if rhLiveStrategiesForCoin("ETH", roster)[0].ID != "rh-sma-eth" {
		t.Error("ETH peer mismatch")
	}
	if len(rhLiveStrategiesForCoin("DOGE", roster)) != 0 {
		t.Error("expected no peers for unconfigured coin")
	}
}

// formatRobinhoodSharedOwnerDM must emit sorted peer IDs so repeat fires
// produce byte-identical output (same contract as the #342 HL review —
// operator-facing text iterating over strategies must be deterministic).
func TestFormatRobinhoodSharedOwnerDM_DeterministicPeerOrder(t *testing.T) {
	peers := []StrategyConfig{
		{ID: "rh-zeta-btc"},
		{ID: "rh-alpha-btc"},
		{ID: "rh-mid-btc"},
	}
	msg := formatRobinhoodSharedOwnerDM("rh-zeta-btc", "BTC", peers)

	// The firing strategy ID also appears in the peer list, and strings.Index
	// would find it in the "strategy X tripped" prefix first. Scope the order
	// check to the peer-list substring between '[' and ']'.
	listStart := strings.Index(msg, "[")
	listEnd := strings.Index(msg, "]")
	if listStart < 0 || listEnd < 0 || listEnd <= listStart {
		t.Fatalf("DM missing peer-list brackets: %q", msg)
	}
	peerList := msg[listStart+1 : listEnd]
	alphaIdx := strings.Index(peerList, "rh-alpha-btc")
	midIdx := strings.Index(peerList, "rh-mid-btc")
	zetaIdx := strings.Index(peerList, "rh-zeta-btc")
	if alphaIdx < 0 || midIdx < 0 || zetaIdx < 0 {
		t.Fatalf("peer list missing expected IDs: %q (full DM: %q)", peerList, msg)
	}
	if !(alphaIdx < midIdx && midIdx < zetaIdx) {
		t.Errorf("peer IDs not in sorted order in peer list: %q", peerList)
	}
}

// captureRHNotifier implements operatorRequiredNotifier for tests.
type captureRHNotifier struct {
	channels []string
	dms      []string
}

func (n *captureRHNotifier) HasBackends() bool          { return true }
func (n *captureRHNotifier) SendToAllChannels(c string) { n.channels = append(n.channels, c) }
func (n *captureRHNotifier) SendOwnerDM(c string)       { n.dms = append(n.dms, c) }

func TestRunPendingRobinhoodCircuitCloses_FailureIncrementsCountAndNotifies(t *testing.T) {
	state := &AppState{
		Strategies: map[string]*StrategyState{
			"rh-a": {
				ID: "rh-a",
				RiskState: RiskState{
					PendingCircuitCloses: map[string]*PendingCircuitClose{
						PlatformPendingCloseRobinhood: {
							Symbols: []PendingCircuitCloseSymbol{{Symbol: "BTC", Size: 0.01}},
						},
					},
				},
			},
		},
	}
	cfg := []StrategyConfig{
		{ID: "rh-a", Platform: "robinhood", Type: "spot",
			Args: []string{"sma", "BTC", "1h", "--mode=live"}},
	}
	var mu sync.RWMutex
	closer := func(sym string) (*RobinhoodCloseResult, error) {
		return nil, fmt.Errorf("rh timeout")
	}
	notifier := &captureRHNotifier{}
	runPendingRobinhoodCircuitCloses(
		context.Background(),
		state,
		cfg,
		[]RobinhoodPosition{{Coin: "BTC", Size: 0.01}},
		true,
		nil,
		closer,
		nil,
		30*time.Second,
		&mu,
		notifier,
	)
	p := state.Strategies["rh-a"].RiskState.getPendingCircuitClose(PlatformPendingCloseRobinhood)
	if p == nil {
		t.Fatal("pending should be preserved on failure")
	}
	if p.FailureCount != 1 {
		t.Errorf("FailureCount: got %d, want 1", p.FailureCount)
	}
	if len(notifier.dms) != 1 {
		t.Errorf("expected 1 DM on first failure, got %d", len(notifier.dms))
	}
}

func TestRunPendingRobinhoodCircuitCloses_RepeatedFailureThrottlesNotifier(t *testing.T) {
	state := &AppState{
		Strategies: map[string]*StrategyState{
			"rh-a": {
				ID: "rh-a",
				RiskState: RiskState{
					PendingCircuitCloses: map[string]*PendingCircuitClose{
						PlatformPendingCloseRobinhood: {
							Symbols:        []PendingCircuitCloseSymbol{{Symbol: "BTC", Size: 0.01}},
							FailureCount:   1,
							LastNotifiedAt: time.Now(),
						},
					},
				},
			},
		},
	}
	cfg := []StrategyConfig{
		{ID: "rh-a", Platform: "robinhood", Type: "spot",
			Args: []string{"sma", "BTC", "1h", "--mode=live"}},
	}
	var mu sync.RWMutex
	closer := func(sym string) (*RobinhoodCloseResult, error) {
		return nil, fmt.Errorf("rh timeout")
	}
	notifier := &captureRHNotifier{}
	runPendingRobinhoodCircuitCloses(
		context.Background(),
		state,
		cfg,
		[]RobinhoodPosition{{Coin: "BTC", Size: 0.01}},
		true,
		nil,
		closer,
		nil,
		30*time.Second,
		&mu,
		notifier,
	)
	if len(notifier.dms) != 0 {
		t.Errorf("expected 0 DMs on failure #2 (suppressed), got %d", len(notifier.dms))
	}
}

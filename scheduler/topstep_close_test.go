package main

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"
)

// forceCloseTopStepLive unit tests — mirror the Robinhood/OKX tests. Each
// test asserts a single branch of the decision logic so a regression
// produces a targeted failure rather than a conflated signal (#347).

func TestForceCloseTopStepLive_ClosesOwnedSymbolsOnly(t *testing.T) {
	tsLive := []StrategyConfig{
		{ID: "ts-momentum-es", Platform: "topstep", Type: "futures",
			Args: []string{"momentum", "ES", "1h", "--mode=live"}},
	}
	positions := []TopStepPosition{
		{Coin: "ES", Size: 2, AvgPrice: 5000, Side: "long"},
		{Coin: "NQ", Size: 1, AvgPrice: 17000, Side: "long"}, // unowned
	}
	var calls []string
	closer := func(sym string) (*TopStepCloseResult, error) {
		calls = append(calls, sym)
		return &TopStepCloseResult{Close: &TopStepClose{Symbol: sym}}, nil
	}

	report := forceCloseTopStepLive(context.Background(), positions, tsLive, closer)

	if !report.ConfirmedFlat() {
		t.Errorf("expected ConfirmedFlat, got errors=%v", report.Errors)
	}
	if len(calls) != 1 || calls[0] != "ES" {
		t.Errorf("expected closer to be called only for owned symbol ES, got %v", calls)
	}
	if len(report.ClosedCoins) != 1 || report.ClosedCoins[0] != "ES" {
		t.Errorf("ClosedCoins = %v, want [ES]", report.ClosedCoins)
	}
	if len(report.Unconfigured) != 1 || report.Unconfigured[0].Coin != "NQ" {
		t.Errorf("Unconfigured = %v, want [NQ]", report.Unconfigured)
	}
}

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

func TestForceCloseTopStepLive_ZeroSizeMarkedAlreadyFlat(t *testing.T) {
	tsLive := []StrategyConfig{
		{ID: "ts-momentum-es", Platform: "topstep", Type: "futures",
			Args: []string{"momentum", "ES", "1h", "--mode=live"}},
	}
	positions := []TopStepPosition{{Coin: "ES", Size: 0}}
	var calls []string
	closer := func(sym string) (*TopStepCloseResult, error) {
		calls = append(calls, sym)
		return &TopStepCloseResult{}, nil
	}

	report := forceCloseTopStepLive(context.Background(), positions, tsLive, closer)

	if len(calls) != 0 {
		t.Errorf("zero-size position must short-circuit before closer, got calls=%v", calls)
	}
	if len(report.AlreadyFlat) != 1 || report.AlreadyFlat[0] != "ES" {
		t.Errorf("AlreadyFlat = %v, want [ES]", report.AlreadyFlat)
	}
	if !report.ConfirmedFlat() {
		t.Errorf("zero-size short-circuit must be ConfirmedFlat, got errors=%v", report.Errors)
	}
}

func TestForceCloseTopStepLive_CtxExpiredBeforeSubmit(t *testing.T) {
	tsLive := []StrategyConfig{
		{ID: "ts-momentum-es", Platform: "topstep", Type: "futures",
			Args: []string{"momentum", "ES", "1h", "--mode=live"}},
	}
	positions := []TopStepPosition{{Coin: "ES", Size: 2}}
	ctx, cancel := context.WithTimeout(context.Background(), 0)
	cancel()
	var calls []string
	closer := func(sym string) (*TopStepCloseResult, error) {
		calls = append(calls, sym)
		return &TopStepCloseResult{}, nil
	}

	report := forceCloseTopStepLive(ctx, positions, tsLive, closer)

	if len(calls) != 0 {
		t.Errorf("closer must not be called after ctx expires, got %v", calls)
	}
	if _, ok := report.Errors["ES"]; !ok {
		t.Errorf("expected ES to be marked as error on ctx expiry, got %v", report.Errors)
	}
}

func TestForceCloseTopStepLive_ShortPositionClosed(t *testing.T) {
	// Futures support bidirectional positions (unlike Robinhood crypto).
	// A negative size (short) for an owned symbol must trigger a close —
	// market_close flattens regardless of direction. Guards against a
	// future refactor that copies Robinhood's Size > 0 check, which would
	// leave shorts open on kill-switch fire.
	tsLive := []StrategyConfig{
		{ID: "ts-momentum-es", Platform: "topstep", Type: "futures",
			Args: []string{"momentum", "ES", "1h", "--mode=live"}},
	}
	positions := []TopStepPosition{{Coin: "ES", Size: -2, Side: "short"}}
	var calls []string
	closer := func(sym string) (*TopStepCloseResult, error) {
		calls = append(calls, sym)
		return &TopStepCloseResult{Close: &TopStepClose{Symbol: sym}}, nil
	}

	report := forceCloseTopStepLive(context.Background(), positions, tsLive, closer)

	if len(calls) != 1 || calls[0] != "ES" {
		t.Errorf("short must trigger close, got calls=%v", calls)
	}
	if !report.ConfirmedFlat() {
		t.Errorf("successful short close must be ConfirmedFlat, got errors=%v", report.Errors)
	}
}

func TestForceCloseTopStepLive_NonFuturesStrategiesIgnored(t *testing.T) {
	// Non-futures entries in TSLiveAll must not drive closes (type partition
	// guard). Mirrors the options-ignored test on the Robinhood close path.
	mixed := []StrategyConfig{
		{ID: "hl-mom-btc", Platform: "hyperliquid", Type: "perps",
			Args: []string{"momentum", "BTC", "1h", "--mode=live"}},
	}
	positions := []TopStepPosition{{Coin: "ES", Size: 2}}
	var calls []string
	closer := func(sym string) (*TopStepCloseResult, error) {
		calls = append(calls, sym)
		return &TopStepCloseResult{}, nil
	}

	report := forceCloseTopStepLive(context.Background(), positions, mixed, closer)

	if len(calls) != 0 {
		t.Errorf("non-futures strategy must not drive TopStep close, got calls=%v", calls)
	}
	if len(report.Unconfigured) != 1 || report.Unconfigured[0].Coin != "ES" {
		t.Errorf("unowned ES must be Unconfigured, got %+v", report.Unconfigured)
	}
}

// SortedErrorCoins determinism — same rationale as HL / OKX / Robinhood.
func TestTopStepLiveCloseReport_SortedErrorCoins(t *testing.T) {
	r := TopStepLiveCloseReport{Errors: map[string]error{
		"NQ": fmt.Errorf("e"), "ES": fmt.Errorf("e"), "CL": fmt.Errorf("e"),
	}}
	coins := r.SortedErrorCoins()
	want := []string{"CL", "ES", "NQ"}
	if len(coins) != len(want) {
		t.Fatalf("len = %d, want %d", len(coins), len(want))
	}
	for i, c := range coins {
		if c != want[i] {
			t.Errorf("coins[%d] = %q, want %q", i, c, want[i])
		}
	}
}

// parseTopStepCloseOutput tests — 5-case matrix mirroring HL / OKX / Robinhood.

func TestParseTopStepCloseOutput_CleanSuccess(t *testing.T) {
	stdout := []byte(`{"close":{"symbol":"ES","fill":{"avg_px":5000,"total_contracts":2,"oid":"ord-1"}},"platform":"topstep","timestamp":"2026-04-19T10:00:00Z"}`)
	result, _, err := parseTopStepCloseOutput(stdout, "", nil)
	if err != nil {
		t.Fatalf("expected nil err, got %v", err)
	}
	if result == nil || result.Close == nil || result.Close.Symbol != "ES" {
		t.Errorf("unexpected result: %+v", result)
	}
	if result.Close.Fill == nil || result.Close.Fill.TotalContracts != 2 {
		t.Errorf("Fill = %+v, want TotalContracts=2", result.Close.Fill)
	}
}

func TestParseTopStepCloseOutput_Exit0WithErrorField(t *testing.T) {
	stdout := []byte(`{"close":{"symbol":"ES","fill":{}},"platform":"topstep","timestamp":"x","error":"venue down"}`)
	_, _, err := parseTopStepCloseOutput(stdout, "", nil)
	if err == nil {
		t.Fatal("expected non-nil err when error field populated, even on exit 0")
	}
}

func TestParseTopStepCloseOutput_ExitNonZeroWithErrorEnvelope(t *testing.T) {
	stdout := []byte(`{"close":{"symbol":"ES","fill":{}},"platform":"topstep","timestamp":"x","error":"market closed"}`)
	_, _, err := parseTopStepCloseOutput(stdout, "", fmt.Errorf("exit 1"))
	if err == nil {
		t.Fatal("expected non-nil err on error envelope")
	}
}

func TestParseTopStepCloseOutput_ExitNonZeroNoErrorField(t *testing.T) {
	stdout := []byte(`{"close":{"symbol":"ES","fill":{}},"platform":"topstep","timestamp":"x"}`)
	_, _, err := parseTopStepCloseOutput(stdout, "stderr msg", fmt.Errorf("exit 2"))
	if err == nil {
		t.Fatal("expected non-nil err on non-zero exit with no error field")
	}
}

func TestParseTopStepCloseOutput_MalformedJSON(t *testing.T) {
	result, _, err := parseTopStepCloseOutput([]byte(`not json`), "", nil)
	if err == nil {
		t.Fatal("expected non-nil err on malformed JSON")
	}
	if result != nil {
		t.Errorf("expected nil result on parse failure, got %+v", result)
	}
}

// parseTopStepPositionsOutput tests.

func TestParseTopStepPositionsOutput_CleanSuccess(t *testing.T) {
	stdout := []byte(`{"positions":[{"coin":"ES","size":2,"avg_price":5000,"side":"long"}],"platform":"topstep","timestamp":"x"}`)
	result, _, err := parseTopStepPositionsOutput(stdout, "", nil)
	if err != nil {
		t.Fatalf("expected nil err, got %v", err)
	}
	if len(result.Positions) != 1 || result.Positions[0].Coin != "ES" {
		t.Errorf("unexpected positions: %+v", result.Positions)
	}
	if result.Positions[0].Size != 2 {
		t.Errorf("Size = %d, want 2", result.Positions[0].Size)
	}
}

func TestParseTopStepPositionsOutput_ErrorEnvelopeLatchesSwitch(t *testing.T) {
	// Load-bearing contract: a failed fetch must surface as err so the kill
	// switch latches. A silent parse that returned {Positions: nil, err: nil}
	// would look like "no positions" and clear virtual state while live
	// exposure remained (the #341/#345/#346 bug class, now #347).
	stdout := []byte(`{"positions":[],"platform":"topstep","timestamp":"x","error":"credentials missing"}`)
	_, _, err := parseTopStepPositionsOutput(stdout, "", fmt.Errorf("exit 1"))
	if err == nil {
		t.Fatal("expected non-nil err when error envelope populated")
	}
}

func TestParseTopStepPositionsOutput_MalformedJSON(t *testing.T) {
	_, _, err := parseTopStepPositionsOutput([]byte(`garbage`), "", nil)
	if err == nil {
		t.Fatal("expected non-nil err on malformed JSON")
	}
}

// --- #362 phase 4: per-strategy circuit-breaker close tests ---

func TestComputeTopStepCircuitCloseQty_SolePeerFullFlatten(t *testing.T) {
	tsLive := []StrategyConfig{
		{ID: "ts-es", Platform: "topstep", Type: "futures",
			Args: []string{"sma", "ES", "15m", "--mode=live"}},
	}
	pos := []TopStepPosition{{Coin: "ES", Size: 3, AvgPrice: 5000, Side: "long"}}
	q, ok := computeTopStepCircuitCloseQty("ES", "ts-es", pos, tsLive)
	if !ok {
		t.Fatal("expected ok for sole peer")
	}
	if q != 3 {
		t.Errorf("qty=%d want 3 (full abs size for sole peer)", q)
	}
}

func TestComputeTopStepCircuitCloseQty_SolePeerShortFullFlatten(t *testing.T) {
	tsLive := []StrategyConfig{
		{ID: "ts-es", Platform: "topstep", Type: "futures",
			Args: []string{"sma", "ES", "15m", "--mode=live"}},
	}
	// Short position reported as negative size.
	pos := []TopStepPosition{{Coin: "ES", Size: -2, AvgPrice: 5000, Side: "short"}}
	q, ok := computeTopStepCircuitCloseQty("ES", "ts-es", pos, tsLive)
	if !ok {
		t.Fatal("expected ok")
	}
	if q != 2 {
		t.Errorf("qty=%d want 2 (abs of -2)", q)
	}
}

// TopStep has no partial-size market_close. When two live strategies share a
// contract, we skip the enqueue so market_close doesn't flatten the peer's
// share on behalf of the firing strategy. Operator intervenes manually.
func TestComputeTopStepCircuitCloseQty_MultiPeerSkipped(t *testing.T) {
	tsLive := []StrategyConfig{
		{ID: "ts-a", Platform: "topstep", Type: "futures",
			Args: []string{"sma", "ES", "15m", "--mode=live"}},
		{ID: "ts-b", Platform: "topstep", Type: "futures",
			Args: []string{"rsi", "ES", "15m", "--mode=live"}},
	}
	pos := []TopStepPosition{{Coin: "ES", Size: 5, Side: "long"}}
	q, ok := computeTopStepCircuitCloseQty("ES", "ts-a", pos, tsLive)
	if ok {
		t.Fatalf("expected ok=false when multiple peers share contract, got qty=%d", q)
	}
	if q != 0 {
		t.Errorf("qty=%d want 0 for multi-peer skip", q)
	}
}

func TestComputeTopStepCircuitCloseQty_NoOnAccountPosition(t *testing.T) {
	tsLive := []StrategyConfig{
		{ID: "ts-es", Platform: "topstep", Type: "futures",
			Args: []string{"sma", "ES", "15m", "--mode=live"}},
	}
	q, ok := computeTopStepCircuitCloseQty("ES", "ts-es", nil, tsLive)
	if ok {
		t.Errorf("expected ok=false when no position found, got qty=%d", q)
	}
}

func TestComputeTopStepCircuitCloseQty_ZeroSizePosition(t *testing.T) {
	tsLive := []StrategyConfig{
		{ID: "ts-es", Platform: "topstep", Type: "futures",
			Args: []string{"sma", "ES", "15m", "--mode=live"}},
	}
	pos := []TopStepPosition{{Coin: "ES", Size: 0, Side: "long"}}
	q, ok := computeTopStepCircuitCloseQty("ES", "ts-es", pos, tsLive)
	if ok {
		t.Errorf("expected ok=false for zero-size position, got qty=%d", q)
	}
}

func TestSetTopStepCircuitBreakerPending_SolePeerEnqueues(t *testing.T) {
	sc := &StrategyConfig{
		ID: "ts-es", Platform: "topstep", Type: "futures",
		Args: []string{"sma", "ES", "15m", "--mode=live"},
	}
	s := &StrategyState{
		ID:        "ts-es",
		Positions: map[string]*Position{"ES": {Side: "long", Quantity: 3}},
	}
	assist := &PlatformRiskAssist{
		TSPositions: []TopStepPosition{{Coin: "ES", Size: 3, Side: "long"}},
		TSLiveAll:   []StrategyConfig{*sc},
	}
	setTopStepCircuitBreakerPending(sc, s, assist)

	pending := s.RiskState.getPendingCircuitClose(PlatformPendingCloseTopStep)
	if pending == nil {
		t.Fatal("expected pending entry enqueued")
	}
	if len(pending.Symbols) != 1 || pending.Symbols[0].Symbol != "ES" || pending.Symbols[0].Size != 3 {
		t.Errorf("pending=%+v want one ES sz=3", pending.Symbols)
	}
}

func TestSetTopStepCircuitBreakerPending_NilAssistBails(t *testing.T) {
	sc := &StrategyConfig{
		ID: "ts-es", Platform: "topstep", Type: "futures",
		Args: []string{"sma", "ES", "15m", "--mode=live"},
	}
	s := &StrategyState{
		ID:        "ts-es",
		Positions: map[string]*Position{"ES": {Side: "long", Quantity: 3}},
	}
	// Nil assist — simulates a TS-fetch failure at CB fire time.
	setTopStepCircuitBreakerPending(sc, s, nil)
	if s.RiskState.getPendingCircuitClose(PlatformPendingCloseTopStep) != nil {
		t.Error("expected no enqueue when assist is nil (stuck-CB path will recover)")
	}
}

func TestSetTopStepCircuitBreakerPending_PaperModeSkipped(t *testing.T) {
	sc := &StrategyConfig{
		ID: "ts-es", Platform: "topstep", Type: "futures",
		Args: []string{"sma", "ES", "15m", "--mode=paper"},
	}
	s := &StrategyState{
		ID:        "ts-es",
		Positions: map[string]*Position{"ES": {Side: "long", Quantity: 3}},
	}
	assist := &PlatformRiskAssist{
		TSPositions: []TopStepPosition{{Coin: "ES", Size: 3, Side: "long"}},
		TSLiveAll:   []StrategyConfig{},
	}
	setTopStepCircuitBreakerPending(sc, s, assist)
	if s.RiskState.getPendingCircuitClose(PlatformPendingCloseTopStep) != nil {
		t.Error("expected no enqueue for paper-mode strategy")
	}
}

func TestRunPendingTopStepCircuitCloses_DrainsAndClearsPending(t *testing.T) {
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
		t.Errorf("closer calls=%v want [ES]", calls)
	}
	if state.Strategies["ts-es"].RiskState.getPendingCircuitClose(PlatformPendingCloseTopStep) != nil {
		t.Error("expected pending cleared after successful close")
	}
}

// Stuck-CB recovery (mirrors HL #356 finding 1): if TS fetch failed at CB
// fire time, pending is nil; the drain must detect CircuitBreaker=true +
// pending==nil + on-account position and enqueue on a later cycle.
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

// Session-gate defer: a TopStepX close that fails (outside RTH, venue error)
// must keep the pending latched so the next cycle retries.
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

// If the drain runs but the on-account position already went flat between
// enqueue and drain (operator manual close, eventual consistency), the
// closer must NOT be called — otherwise market_close on a flat position
// would error and latch the pending forever.
func TestRunPendingTopStepCircuitCloses_AlreadyFlatSkipsCloserAndClears(t *testing.T) {
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
	runPendingTopStepCircuitCloses(
		context.Background(),
		state,
		cfg,
		nil, // no positions on account
		true,
		nil,
		closer,
		30*time.Second,
		&mu,
		nil,
	)
	if len(calls) != 0 {
		t.Errorf("closer should not be called when position is already flat, got %v", calls)
	}
	if state.Strategies["ts-es"].RiskState.getPendingCircuitClose(PlatformPendingCloseTopStep) != nil {
		t.Error("expected pending cleared after already-flat skip")
	}
}

func TestRunPendingTopStepCircuitCloses_StuckCBMultiPeerSkipped(t *testing.T) {
	state := &AppState{
		Strategies: map[string]*StrategyState{
			"ts-a": {
				ID: "ts-a",
				RiskState: RiskState{
					CircuitBreaker:      true,
					CircuitBreakerUntil: time.Now().Add(24 * time.Hour),
				},
			},
		},
	}
	// Two live peers on contract ES — computeTopStepCircuitCloseQty returns
	// (0, false), so stuck-CB recovery must NOT reconstruct a pending.
	cfg := []StrategyConfig{
		{ID: "ts-a", Platform: "topstep", Type: "futures",
			Args: []string{"sma", "ES", "15m", "--mode=live"}},
		{ID: "ts-b", Platform: "topstep", Type: "futures",
			Args: []string{"rsi", "ES", "15m", "--mode=live"}},
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
		[]TopStepPosition{{Coin: "ES", Size: 5}},
		true,
		nil,
		closer,
		30*time.Second,
		&mu,
		nil,
	)
	if len(calls) != 0 {
		t.Errorf("closer should not be called for multi-peer contract, got %v", calls)
	}
	if state.Strategies["ts-a"].RiskState.getPendingCircuitClose(PlatformPendingCloseTopStep) != nil {
		t.Error("expected no pending reconstruction for multi-peer contract")
	}
}

// When the fetcher is needed (tsStateFetched=false) and it returns an error,
// the drain must bail without mutating pending entries.
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
	// Pending must remain so the next cycle retries.
	if state.Strategies["ts-es"].RiskState.getPendingCircuitClose(PlatformPendingCloseTopStep) == nil {
		t.Error("expected pending to remain latched when fetcher errors")
	}
}

// captureTSNotifier implements operatorRequiredNotifier for tests.
type captureTSNotifier struct {
	channels []string
	dms      []string
}

func (n *captureTSNotifier) HasBackends() bool          { return true }
func (n *captureTSNotifier) SendToAllChannels(c string) { n.channels = append(n.channels, c) }
func (n *captureTSNotifier) SendOwnerDM(c string)       { n.dms = append(n.dms, c) }

func TestRunPendingTopStepCircuitCloses_FailureIncrementsCountAndNotifies(t *testing.T) {
	state := &AppState{
		Strategies: map[string]*StrategyState{
			"ts-es": {
				ID: "ts-es",
				RiskState: RiskState{
					PendingCircuitCloses: map[string]*PendingCircuitClose{
						PlatformPendingCloseTopStep: {
							Symbols: []PendingCircuitCloseSymbol{{Symbol: "ES", Size: 1}},
						},
					},
				},
			},
		},
	}
	cfg := []StrategyConfig{
		{ID: "ts-es", Platform: "topstep", Type: "futures",
			Args: []string{"ts-es", "ES", "1h", "--mode=live"}},
	}
	var mu sync.RWMutex
	closer := func(sym string) (*TopStepCloseResult, error) {
		return nil, fmt.Errorf("topstep API 503")
	}
	notifier := &captureTSNotifier{}
	runPendingTopStepCircuitCloses(
		context.Background(),
		state,
		cfg,
		[]TopStepPosition{{Coin: "ES", Size: 1}},
		true,
		nil,
		closer,
		30*time.Second,
		&mu,
		notifier,
	)
	p := state.Strategies["ts-es"].RiskState.getPendingCircuitClose(PlatformPendingCloseTopStep)
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

func TestRunPendingTopStepCircuitCloses_RepeatedFailureThrottlesNotifier(t *testing.T) {
	state := &AppState{
		Strategies: map[string]*StrategyState{
			"ts-es": {
				ID: "ts-es",
				RiskState: RiskState{
					PendingCircuitCloses: map[string]*PendingCircuitClose{
						PlatformPendingCloseTopStep: {
							Symbols:        []PendingCircuitCloseSymbol{{Symbol: "ES", Size: 1}},
							FailureCount:   1,
							LastNotifiedAt: time.Now(),
						},
					},
				},
			},
		},
	}
	cfg := []StrategyConfig{
		{ID: "ts-es", Platform: "topstep", Type: "futures",
			Args: []string{"ts-es", "ES", "1h", "--mode=live"}},
	}
	var mu sync.RWMutex
	closer := func(sym string) (*TopStepCloseResult, error) {
		return nil, fmt.Errorf("topstep API 503")
	}
	notifier := &captureTSNotifier{}
	runPendingTopStepCircuitCloses(
		context.Background(),
		state,
		cfg,
		[]TopStepPosition{{Coin: "ES", Size: 1}},
		true,
		nil,
		closer,
		30*time.Second,
		&mu,
		notifier,
	)
	if len(notifier.dms) != 0 {
		t.Errorf("expected 0 DMs on failure #2 (suppressed), got %d", len(notifier.dms))
	}
}

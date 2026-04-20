package main

import (
	"strings"
	"sync"
	"testing"
	"time"
)

// TestSetOperatorRequiredCircuitBreakerPending_OKXSpot verifies #363 phase 5:
// a live OKX spot strategy tripping its circuit breaker enqueues a pending
// close with OperatorRequired=true under the PlatformPendingCloseOKXSpot key
// — NOT under the auto-close "okx" key, so the portfolio-kill OKX perps drain
// never dequeues and auto-closes it.
func TestSetOperatorRequiredCircuitBreakerPending_OKXSpot(t *testing.T) {
	sc := &StrategyConfig{
		ID: "okx-sma-btc", Platform: "okx", Type: "spot",
		Args: []string{"sma_crossover", "BTC-USDT", "1h", "--mode=live"},
	}
	s := &StrategyState{
		ID: sc.ID, Type: "spot", Platform: "okx",
		Positions: map[string]*Position{
			"BTC-USDT": {Symbol: "BTC-USDT", Quantity: 0.0125, AvgCost: 80000, Side: "long"},
		},
		OptionPositions: map[string]*OptionPosition{},
	}

	setOperatorRequiredCircuitBreakerPending(sc, s)

	p := s.RiskState.getPendingCircuitClose(PlatformPendingCloseOKXSpot)
	if p == nil {
		t.Fatal("expected PendingCircuitCloses[okx_spot] after CB fire")
	}
	if !p.OperatorRequired {
		t.Error("OperatorRequired=false; want true")
	}
	if len(p.Symbols) != 1 || p.Symbols[0].Symbol != "BTC-USDT" || p.Symbols[0].Size != 0.0125 {
		t.Errorf("unexpected pending symbols: %+v", p.Symbols)
	}
	// Defensive: must NOT land under the auto-close "okx" key.
	if s.RiskState.getPendingCircuitClose("okx") != nil {
		t.Error("enqueue leaked into the auto-close okx key — portfolio-kill drain would auto-close this")
	}
}

// TestSetOperatorRequiredCircuitBreakerPending_RobinhoodOptions verifies each
// open option leg is captured as a separate PendingCircuitCloseSymbol (not the
// underlier) so the operator sees which specific positions need manual close.
func TestSetOperatorRequiredCircuitBreakerPending_RobinhoodOptions(t *testing.T) {
	sc := &StrategyConfig{
		ID: "rh-ccall-spy", Platform: "robinhood", Type: "options",
		Args: []string{"covered_call", "SPY", "1d", "--mode=live"},
	}
	s := &StrategyState{
		ID: sc.ID, Type: "options", Platform: "robinhood",
		Positions: map[string]*Position{},
		OptionPositions: map[string]*OptionPosition{
			"SPY-2026-05-15-450-C": {Quantity: 2},
			"SPY-2026-06-19-460-C": {Quantity: 1},
		},
	}

	setOperatorRequiredCircuitBreakerPending(sc, s)

	p := s.RiskState.getPendingCircuitClose(PlatformPendingCloseRobinhoodOptions)
	if p == nil {
		t.Fatal("expected PendingCircuitCloses[robinhood_options] after CB fire")
	}
	if !p.OperatorRequired {
		t.Error("OperatorRequired=false; want true")
	}
	if len(p.Symbols) != 2 {
		t.Fatalf("expected 2 option legs, got %d: %+v", len(p.Symbols), p.Symbols)
	}
	// Deterministic sort guarantees alphabetic order.
	if p.Symbols[0].Symbol != "SPY-2026-05-15-450-C" || p.Symbols[1].Symbol != "SPY-2026-06-19-460-C" {
		t.Errorf("legs not sorted alphabetically: %+v", p.Symbols)
	}
	if s.RiskState.getPendingCircuitClose("robinhood") != nil {
		t.Error("enqueue leaked into the auto-close robinhood key")
	}
}

// TestSetOperatorRequiredCircuitBreakerPending_RobinhoodOptions_NoOpenLegs
// locks in the marker-entry fallback: when options CB fires before any leg is
// opened, we still enqueue a single underlier marker so /status and
// notifications surface the fire.
func TestSetOperatorRequiredCircuitBreakerPending_RobinhoodOptions_NoOpenLegs(t *testing.T) {
	sc := &StrategyConfig{
		ID: "rh-vol-qqq", Platform: "robinhood", Type: "options",
		Args: []string{"vol_scalp", "QQQ", "1d", "--mode=live"},
	}
	s := &StrategyState{
		ID: sc.ID, Type: "options", Platform: "robinhood",
		Positions:       map[string]*Position{},
		OptionPositions: map[string]*OptionPosition{},
	}

	setOperatorRequiredCircuitBreakerPending(sc, s)

	p := s.RiskState.getPendingCircuitClose(PlatformPendingCloseRobinhoodOptions)
	if p == nil || len(p.Symbols) != 1 {
		t.Fatalf("expected 1 marker symbol when no legs open; got %+v", p)
	}
	if p.Symbols[0].Symbol != "QQQ" || p.Symbols[0].Size != 0 {
		t.Errorf("marker entry wrong: %+v", p.Symbols[0])
	}
}

// TestSetOperatorRequiredCircuitBreakerPending_PaperMode_NoEnqueue verifies
// paper-mode strategies do NOT enqueue — there is no real venue exposure and
// surfacing a warning would be noise.
func TestSetOperatorRequiredCircuitBreakerPending_PaperMode_NoEnqueue(t *testing.T) {
	// OKX spot, paper mode (no --mode=live).
	sc := &StrategyConfig{
		ID: "okx-paper", Platform: "okx", Type: "spot",
		Args: []string{"sma_crossover", "BTC-USDT", "1h", "--mode=paper"},
	}
	s := &StrategyState{
		ID: sc.ID, Positions: map[string]*Position{"BTC-USDT": {Quantity: 0.01}},
	}
	setOperatorRequiredCircuitBreakerPending(sc, s)
	if s.RiskState.getPendingCircuitClose(PlatformPendingCloseOKXSpot) != nil {
		t.Error("paper-mode OKX spot enqueued operator-required pending; want nil")
	}

	// RH options, paper mode.
	sc2 := &StrategyConfig{
		ID: "rh-paper", Platform: "robinhood", Type: "options",
		Args: []string{"covered_call", "SPY", "1d", "--mode=paper"},
	}
	s2 := &StrategyState{
		ID: sc2.ID, OptionPositions: map[string]*OptionPosition{"SPY-leg": {Quantity: 1}},
	}
	setOperatorRequiredCircuitBreakerPending(sc2, s2)
	if s2.RiskState.getPendingCircuitClose(PlatformPendingCloseRobinhoodOptions) != nil {
		t.Error("paper-mode RH options enqueued operator-required pending; want nil")
	}
}

// TestSetOperatorRequiredCircuitBreakerPending_IgnoresOtherPlatforms verifies
// the helper is a no-op for HL / TopStep / BinanceUS / OKX perps / RH crypto
// — those either have an automated close path or fall under a different
// helper.
func TestSetOperatorRequiredCircuitBreakerPending_IgnoresOtherPlatforms(t *testing.T) {
	for _, sc := range []*StrategyConfig{
		{ID: "hl-1", Platform: "hyperliquid", Type: "perps",
			Args: []string{"triple_ema", "ETH", "1h", "--mode=live"}},
		{ID: "ts-1", Platform: "topstep", Type: "futures",
			Args: []string{"breakout", "ESM25", "15m", "--mode=live"}},
		{ID: "bu-1", Platform: "binanceus", Type: "spot",
			Args: []string{"sma_crossover", "BTC/USDT", "1h"}},
		{ID: "okx-perps", Platform: "okx", Type: "perps",
			Args: []string{"triple_ema", "BTC-USDT-SWAP", "1h", "--mode=live"}},
		{ID: "rh-crypto", Platform: "robinhood", Type: "spot",
			Args: []string{"sma_crossover", "BTC", "1h", "--mode=live"}},
	} {
		s := &StrategyState{ID: sc.ID, Positions: map[string]*Position{}, OptionPositions: map[string]*OptionPosition{}}
		setOperatorRequiredCircuitBreakerPending(sc, s)
		if len(s.RiskState.PendingCircuitCloses) != 0 {
			t.Errorf("%s: unexpected enqueue %+v", sc.ID, s.RiskState.PendingCircuitCloses)
		}
	}
}

// TestCheckRisk_LiveOKXSpot_SetsOperatorRequiredPending verifies the full
// CheckRisk → setOperatorRequiredCircuitBreakerPending wiring for OKX spot.
// Drawdown is intentionally above the max threshold so the CB fires.
func TestCheckRisk_LiveOKXSpot_SetsOperatorRequiredPending(t *testing.T) {
	sc := StrategyConfig{
		ID: "okx-sma-btc", Platform: "okx", Type: "spot",
		Args: []string{"sma_crossover", "BTC-USDT", "1h", "--mode=live"},
	}
	s := &StrategyState{
		ID: sc.ID, Type: "spot", Platform: "okx",
		Cash: 0,
		RiskState: RiskState{
			PeakValue: 1000, MaxDrawdownPct: 10, TotalTrades: 1, DailyPnLDate: todayUTC(),
		},
		Positions: map[string]*Position{
			"BTC-USDT": {Symbol: "BTC-USDT", Quantity: 0.01, AvgCost: 80000, Side: "long"},
		},
		OptionPositions: map[string]*OptionPosition{},
	}
	// Price drop sends PV to $500 (50% drawdown from peak 1000 — well past max 10%).
	prices := map[string]float64{"BTC-USDT": 50000}

	allowed, reason := CheckRisk(&sc, s, PortfolioValue(s, prices), prices, nil, nil)
	if allowed {
		t.Fatalf("CheckRisk allowed=true; want false after drawdown breach (reason=%q)", reason)
	}
	p := s.RiskState.getPendingCircuitClose(PlatformPendingCloseOKXSpot)
	if p == nil {
		t.Fatal("expected PendingCircuitCloses[okx_spot] after CheckRisk CB fire")
	}
	if !p.OperatorRequired {
		t.Error("OperatorRequired=false; want true")
	}
}

// --- Drain + formatter tests ---

type captureNotifier struct {
	hasBackends bool
	channels    []string
	dms         []string
}

func (n *captureNotifier) HasBackends() bool          { return n.hasBackends }
func (n *captureNotifier) SendToAllChannels(c string) { n.channels = append(n.channels, c) }
func (n *captureNotifier) SendOwnerDM(c string)       { n.dms = append(n.dms, c) }

// TestPlanOperatorRequiredWarning_EmptyStateNoEntries confirms the formatter
// is silent when no operator-required pending is present.
func TestPlanOperatorRequiredWarning_EmptyStateNoEntries(t *testing.T) {
	state := &AppState{Strategies: map[string]*StrategyState{
		"hl-1": {ID: "hl-1", RiskState: RiskState{}},
	}}
	plan := planOperatorRequiredWarning(state)
	if plan.HasEntries() {
		t.Errorf("expected no entries; got %+v", plan.Entries)
	}
	if plan.Message != "" {
		t.Errorf("expected empty message; got %q", plan.Message)
	}
}

// TestPlanOperatorRequiredWarning_IgnoresAutomatedPlatforms verifies that
// PendingCircuitCloses entries WITHOUT OperatorRequired=true (i.e. HL auto-close
// pending) do not produce operator warnings — the HL drain owns those.
func TestPlanOperatorRequiredWarning_IgnoresAutomatedPlatforms(t *testing.T) {
	state := &AppState{Strategies: map[string]*StrategyState{
		"hl-1": {
			ID: "hl-1",
			RiskState: RiskState{PendingCircuitCloses: map[string]*PendingCircuitClose{
				PlatformPendingCloseHyperliquid: {
					Symbols: []PendingCircuitCloseSymbol{{Symbol: "ETH", Size: 0.1}},
				},
			}},
		},
	}}
	plan := planOperatorRequiredWarning(state)
	if plan.HasEntries() {
		t.Errorf("HL auto-close pending leaked into operator warning: %+v", plan.Entries)
	}
}

// TestPlanOperatorRequiredWarning_FormatsOKXAndRH locks in the message
// structure: sorted by strategy ID then platform, leg list, CB-until suffix,
// explicit "No automated close" footer.
func TestPlanOperatorRequiredWarning_FormatsOKXAndRH(t *testing.T) {
	cbUntil := time.Date(2026, 4, 21, 3, 30, 0, 0, time.UTC)
	state := &AppState{Strategies: map[string]*StrategyState{
		"okx-sma-btc": {
			ID: "okx-sma-btc",
			RiskState: RiskState{
				CurrentDrawdownPct:  12.5,
				CircuitBreaker:      true,
				CircuitBreakerUntil: cbUntil,
				PendingCircuitCloses: map[string]*PendingCircuitClose{
					PlatformPendingCloseOKXSpot: {
						Symbols:          []PendingCircuitCloseSymbol{{Symbol: "BTC-USDT", Size: 0.0125}},
						OperatorRequired: true,
					},
				},
			},
		},
		"rh-ccall-spy": {
			ID: "rh-ccall-spy",
			RiskState: RiskState{
				CurrentDrawdownPct:  8.0,
				CircuitBreaker:      true,
				CircuitBreakerUntil: cbUntil,
				PendingCircuitCloses: map[string]*PendingCircuitClose{
					PlatformPendingCloseRobinhoodOptions: {
						Symbols: []PendingCircuitCloseSymbol{
							{Symbol: "SPY-2026-06-19-460-C", Size: 1},
							{Symbol: "SPY-2026-05-15-450-C", Size: 2},
						},
						OperatorRequired: true,
					},
				},
			},
		},
	}}

	plan := planOperatorRequiredWarning(state)
	if !plan.HasEntries() || len(plan.Entries) != 2 {
		t.Fatalf("expected 2 entries, got %d: %+v", len(plan.Entries), plan.Entries)
	}
	// Deterministic ordering: strategy ID alphabetical.
	if plan.Entries[0].StrategyID != "okx-sma-btc" || plan.Entries[1].StrategyID != "rh-ccall-spy" {
		t.Errorf("entries not sorted by StrategyID: got %s, %s", plan.Entries[0].StrategyID, plan.Entries[1].StrategyID)
	}
	// Legs within an entry sorted alphabetically.
	rhLegs := plan.Entries[1].Symbols
	if rhLegs[0].Symbol != "SPY-2026-05-15-450-C" || rhLegs[1].Symbol != "SPY-2026-06-19-460-C" {
		t.Errorf("RH legs not sorted: %+v", rhLegs)
	}

	msg := plan.Message
	for _, want := range []string{
		"CIRCUIT BREAKER — OPERATOR INTERVENTION REQUIRED",
		"2 strategy-platform pairs",
		"okx-sma-btc [OKX spot]",
		"rh-ccall-spy [Robinhood options]",
		"BTC-USDT (size=0.012500)",
		"SPY-2026-05-15-450-C",
		"drawdown 12.5%",
		"CB until 2026-04-21T03:30:00Z",
		"No automated close will be attempted",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("message missing %q; got:\n%s", want, msg)
		}
	}

	// LogLines: one CRITICAL per entry, containing strategy ID + platform key.
	if len(plan.LogLines) != 2 {
		t.Fatalf("expected 2 log lines, got %d", len(plan.LogLines))
	}
	for _, line := range plan.LogLines {
		if !strings.HasPrefix(line, "[CRITICAL] operator-required-close:") {
			t.Errorf("log line missing CRITICAL prefix: %q", line)
		}
	}
}

// TestDrainOperatorRequired_DeliversToNotifier confirms the drain wrapper
// forwards to SendToAllChannels and SendOwnerDM exactly once per cycle when
// entries are present.
func TestDrainOperatorRequired_DeliversToNotifier(t *testing.T) {
	state := &AppState{Strategies: map[string]*StrategyState{
		"okx-s": {
			ID: "okx-s",
			RiskState: RiskState{PendingCircuitCloses: map[string]*PendingCircuitClose{
				PlatformPendingCloseOKXSpot: {
					Symbols:          []PendingCircuitCloseSymbol{{Symbol: "BTC-USDT", Size: 0.01}},
					OperatorRequired: true,
				},
			}},
		},
	}}
	n := &captureNotifier{hasBackends: true}
	var mu sync.RWMutex

	drainOperatorRequiredPendingCloses(state, n, &mu)

	if len(n.channels) != 1 || len(n.dms) != 1 {
		t.Fatalf("expected 1 channel send + 1 owner DM; got channels=%d dms=%d", len(n.channels), len(n.dms))
	}
	if !strings.Contains(n.channels[0], "OPERATOR INTERVENTION REQUIRED") {
		t.Errorf("channel message missing header: %s", n.channels[0])
	}

	// Pending must remain after drain — auto-clearing would hide the gap.
	if state.Strategies["okx-s"].RiskState.getPendingCircuitClose(PlatformPendingCloseOKXSpot) == nil {
		t.Error("drain cleared the pending; it should persist until operator intervenes")
	}
}

// TestDrainOperatorRequired_NoBackendsSafe confirms the drain tolerates a
// notifier with no backends (Discord + Telegram disabled) without panicking
// and without producing output.
func TestDrainOperatorRequired_NoBackendsSafe(t *testing.T) {
	state := &AppState{Strategies: map[string]*StrategyState{
		"okx-s": {
			ID: "okx-s",
			RiskState: RiskState{PendingCircuitCloses: map[string]*PendingCircuitClose{
				PlatformPendingCloseOKXSpot: {
					Symbols:          []PendingCircuitCloseSymbol{{Symbol: "BTC-USDT", Size: 0.01}},
					OperatorRequired: true,
				},
			}},
		},
	}}
	n := &captureNotifier{hasBackends: false}
	var mu sync.RWMutex
	drainOperatorRequiredPendingCloses(state, n, &mu) // must not panic
	if len(n.channels)+len(n.dms) != 0 {
		t.Errorf("expected no sends when HasBackends()=false; got %d/%d", len(n.channels), len(n.dms))
	}
}

// TestDrainOperatorRequired_NilNotifierSafe locks in the nil-notifier guard.
func TestDrainOperatorRequired_NilNotifierSafe(t *testing.T) {
	state := &AppState{Strategies: map[string]*StrategyState{
		"okx-s": {
			ID: "okx-s",
			RiskState: RiskState{PendingCircuitCloses: map[string]*PendingCircuitClose{
				PlatformPendingCloseOKXSpot: {
					Symbols:          []PendingCircuitCloseSymbol{{Symbol: "BTC-USDT", Size: 0.01}},
					OperatorRequired: true,
				},
			}},
		},
	}}
	var mu sync.RWMutex
	drainOperatorRequiredPendingCloses(state, nil, &mu) // must not panic
}

// TestPendingCircuitClose_OperatorRequired_JSONRoundTrip locks in that the
// OperatorRequired flag round-trips through Marshal/Unmarshal (serialized to
// SQLite as part of risk_pending_circuit_closes_json). A bug where the flag
// gets dropped would silently re-promote the entry to auto-close on reload.
func TestPendingCircuitClose_OperatorRequired_JSONRoundTrip(t *testing.T) {
	src := &RiskState{PendingCircuitCloses: map[string]*PendingCircuitClose{
		PlatformPendingCloseOKXSpot: {
			Symbols:          []PendingCircuitCloseSymbol{{Symbol: "BTC-USDT", Size: 0.01}},
			OperatorRequired: true,
		},
		PlatformPendingCloseHyperliquid: {
			Symbols: []PendingCircuitCloseSymbol{{Symbol: "ETH", Size: 0.1}},
			// OperatorRequired omitted — expect false after roundtrip.
		},
	}}
	blob := src.MarshalPendingCircuitClosesJSON()
	if blob == "" {
		t.Fatal("empty blob; expected marshaled JSON")
	}
	dst := &RiskState{}
	dst.UnmarshalPendingCircuitClosesJSON(blob)

	okx := dst.getPendingCircuitClose(PlatformPendingCloseOKXSpot)
	if okx == nil || !okx.OperatorRequired {
		t.Errorf("OperatorRequired flag lost on reload: %+v", okx)
	}
	hl := dst.getPendingCircuitClose(PlatformPendingCloseHyperliquid)
	if hl == nil || hl.OperatorRequired {
		t.Errorf("HL entry OperatorRequired flipped true unexpectedly: %+v", hl)
	}
}

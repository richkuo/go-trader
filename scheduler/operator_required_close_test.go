package main

import (
	"strings"
	"sync"
	"testing"
	"time"
)

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
	if s.RiskState.getPendingCircuitClose("okx") != nil {
		t.Error("enqueue leaked into the auto-close okx key — portfolio-kill drain would auto-close this")
	}
}

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
	if p.Symbols[0].Symbol != "SPY-2026-05-15-450-C" || p.Symbols[1].Symbol != "SPY-2026-06-19-460-C" {
		t.Errorf("legs not sorted alphabetically: %+v", p.Symbols)
	}
	if s.RiskState.getPendingCircuitClose("robinhood") != nil {
		t.Error("enqueue leaked into the auto-close robinhood key")
	}
}

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

func TestSetOperatorRequiredCircuitBreakerPending_PaperMode_NoEnqueue(t *testing.T) {
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

func TestCheckRisk_LiveOKXSpot_SetsOperatorRequiredPending(t *testing.T) {
	sc := StrategyConfig{
		ID: "okx-sma-btc", Platform: "okx", Type: "spot",
		Args: []string{"sma_crossover", "BTC-USDT", "1h", "--mode=live"},
	}
	s := &StrategyState{
		ID: sc.ID, Type: "spot", Platform: "okx",
		Cash: 0,
		RiskState: RiskState{
			PeakValue: 1000, MaxDrawdownPct: 10, DailyPnLDate: todayUTC(),
		},
		Positions: map[string]*Position{
			"BTC-USDT": {Symbol: "BTC-USDT", Quantity: 0.01, AvgCost: 80000, Side: "long"},
		},
		OptionPositions: map[string]*OptionPosition{},
	}
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

type captureNotifier struct {
	hasBackends bool
	channels    []string
	dms         []string
}

func (n *captureNotifier) HasBackends() bool          { return n.hasBackends }
func (n *captureNotifier) SendToAllChannels(c string) { n.channels = append(n.channels, c) }
func (n *captureNotifier) SendOwnerDM(c string)       { n.dms = append(n.dms, c) }

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
	if plan.Entries[0].StrategyID != "okx-sma-btc" || plan.Entries[1].StrategyID != "rh-ccall-spy" {
		t.Errorf("entries not sorted by StrategyID: got %s, %s", plan.Entries[0].StrategyID, plan.Entries[1].StrategyID)
	}
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
		"BTC-USDT (size=0.012500, virtual)",
		"SPY-2026-05-15-450-C",
		"drawdown 12.5%",
		"CB until 2026-04-21T03:30:00Z",
		"No automated close will be attempted",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("message missing %q; got:\n%s", want, msg)
		}
	}

	if len(plan.LogLines) != 2 {
		t.Fatalf("expected 2 log lines, got %d", len(plan.LogLines))
	}
	for _, line := range plan.LogLines {
		if !strings.HasPrefix(line, "[CRITICAL] operator-required-close:") {
			t.Errorf("log line missing CRITICAL prefix: %q", line)
		}
	}
}

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

	if state.Strategies["okx-s"].RiskState.getPendingCircuitClose(PlatformPendingCloseOKXSpot) == nil {
		t.Error("drain cleared the pending; it should persist until operator intervenes")
	}
}

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
	drainOperatorRequiredPendingCloses(state, n, &mu)
	if len(n.channels)+len(n.dms) != 0 {
		t.Errorf("expected no sends when HasBackends()=false; got %d/%d", len(n.channels), len(n.dms))
	}
}

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
	drainOperatorRequiredPendingCloses(state, nil, &mu)
}

func TestPendingCircuitClose_OperatorRequired_JSONRoundTrip(t *testing.T) {
	src := &RiskState{PendingCircuitCloses: map[string]*PendingCircuitClose{
		PlatformPendingCloseOKXSpot: {
			Symbols:          []PendingCircuitCloseSymbol{{Symbol: "BTC-USDT", Size: 0.01}},
			OperatorRequired: true,
		},
		PlatformPendingCloseHyperliquid: {
			Symbols: []PendingCircuitCloseSymbol{{Symbol: "ETH", Size: 0.1}},
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

func TestPendingCircuitClose_RHOptionsMarkerEntry_JSONRoundTrip(t *testing.T) {
	src := &RiskState{PendingCircuitCloses: map[string]*PendingCircuitClose{
		PlatformPendingCloseRobinhoodOptions: {
			Symbols:          []PendingCircuitCloseSymbol{{Symbol: "QQQ", Size: 0}},
			OperatorRequired: true,
		},
	}}
	blob := src.MarshalPendingCircuitClosesJSON()
	if blob == "" {
		t.Fatal("marker-entry got filtered by marshal; expected non-empty blob")
	}
	dst := &RiskState{}
	dst.UnmarshalPendingCircuitClosesJSON(blob)

	rh := dst.getPendingCircuitClose(PlatformPendingCloseRobinhoodOptions)
	if rh == nil {
		t.Fatal("RH options marker entry missing after reload")
	}
	if !rh.OperatorRequired {
		t.Error("OperatorRequired flag dropped on reload; want true")
	}
	if len(rh.Symbols) != 1 || rh.Symbols[0].Symbol != "QQQ" || rh.Symbols[0].Size != 0 {
		t.Errorf("marker symbol wrong after reload: %+v", rh.Symbols)
	}
}

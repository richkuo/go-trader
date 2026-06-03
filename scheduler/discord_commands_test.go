package main

import (
	"strings"
	"testing"
	"time"
)

func TestDiscordBackend(t *testing.T) {
	// No Discord backend present.
	mn := NewMultiNotifier()
	if got := mn.DiscordBackend(); got != nil {
		t.Fatalf("expected nil DiscordBackend on empty notifier, got %v", got)
	}

	// Discord backend present (zero-value *DiscordNotifier is fine for identity).
	d := &DiscordNotifier{}
	mn2 := NewMultiNotifier(notifierBackend{notifier: d})
	if got := mn2.DiscordBackend(); got != d {
		t.Fatalf("expected DiscordBackend to return the registered *DiscordNotifier, got %v", got)
	}
}

func TestAuthorizeCommand(t *testing.T) {
	const owner = "owner123"
	cases := []struct {
		name, invoker, guildID string
		wantOK                 bool
	}{
		{"status", "anyone", "guild1", true}, // read-only in guild OK
		{"status", "anyone", "", true},       // read-only in DM OK
		{"positions", "anyone", "guild1", true},
		{"logs", "anyone", "guild1", true},
		{"restart", owner, "", true},        // ops: owner in DM OK
		{"restart", owner, "guild1", false}, // ops: owner in guild rejected (must be DM)
		{"restart", "intruder", "", false},  // ops: non-owner in DM rejected
		{"backtest", owner, "", true},
		{"backtest", "intruder", "", false},
		{"unknown", owner, "", false}, // unknown command rejected
	}
	for _, c := range cases {
		ok, reason := authorizeCommand(c.name, c.invoker, c.guildID, owner)
		if ok != c.wantOK {
			t.Errorf("authorizeCommand(%q, %q, guild=%q) = %v (%q), want %v",
				c.name, c.invoker, c.guildID, ok, reason, c.wantOK)
		}
		if !ok && reason == "" {
			t.Errorf("authorizeCommand(%q,...) denied without a reason", c.name)
		}
	}
}

func TestFormatHealthResponse(t *testing.T) {
	now := time.Date(2026, 6, 3, 12, 0, 0, 0, time.UTC)

	never := formatHealthResponse(time.Time{}, 0, "v1", now)
	if !strings.Contains(never, "never") {
		t.Errorf("expected 'never' for zero last cycle, got: %s", never)
	}

	ok := formatHealthResponse(now.Add(-1*time.Minute), 42, "v1", now)
	if !strings.Contains(ok, "ok") || !strings.Contains(ok, "42") {
		t.Errorf("expected ok status with cycle count, got: %s", ok)
	}

	stale := formatHealthResponse(now.Add(-31*time.Minute), 42, "v1", now)
	if !strings.Contains(stale, "stale") {
		t.Errorf("expected stale status, got: %s", stale)
	}
}

func TestFormatStatusResponse(t *testing.T) {
	state := &AppState{Strategies: map[string]*StrategyState{
		"hl-a": {ID: "hl-a", Platform: "hyperliquid", Cash: 100,
			Positions: map[string]*Position{"BTC": {Symbol: "BTC", Quantity: 1, AvgCost: 50, Side: "long"}},
			Regime:    "trend_up"},
	}}
	prices := map[string]float64{"BTC": 60}
	got := formatStatusResponse(state, prices)
	if !strings.Contains(got, "positions=1") {
		t.Errorf("expected 1 position in status, got: %s", got)
	}
	if !strings.Contains(got, "regime=trend_up") {
		t.Errorf("expected regime in status, got: %s", got)
	}
}

func testPnLState() *AppState {
	return &AppState{Strategies: map[string]*StrategyState{
		"hl-a": {ID: "hl-a", Platform: "hyperliquid", Cash: 0, InitialCapital: 50,
			Positions: map[string]*Position{"BTC": {Symbol: "BTC", Quantity: 1, AvgCost: 50, Side: "long"}}},
		"hl-b": {ID: "hl-b", Platform: "hyperliquid", Cash: 50, InitialCapital: 50,
			Positions: map[string]*Position{}},
	}}
}

func TestFormatPositionsResponse(t *testing.T) {
	empty := formatPositionsResponse(&AppState{Strategies: map[string]*StrategyState{}}, nil)
	if !strings.Contains(empty, "No open positions") {
		t.Errorf("expected empty message, got: %s", empty)
	}

	got := formatPositionsResponse(testPnLState(), map[string]float64{"BTC": 60})
	if !strings.Contains(got, "BTC") || !strings.Contains(got, "hl-a") {
		t.Errorf("expected BTC position owned by hl-a, got: %s", got)
	}
}

func TestFormatPnLResponse(t *testing.T) {
	// hl-a: pv = 1*60 = 60, cap 50 -> +10 (+20%). hl-b: pv = 50, cap 50 -> 0.
	got := formatPnLResponse(testPnLState(), map[string]float64{"BTC": 60}, nil)
	if !strings.Contains(got, "+10.00") || !strings.Contains(got, "+20.00%") {
		t.Errorf("expected hl-a pnl +10 (+20%%), got: %s", got)
	}
	if !strings.Contains(got, "Total") {
		t.Errorf("expected a Total line, got: %s", got)
	}
}

func TestFormatCircuitBreakersResponse(t *testing.T) {
	now := time.Date(2026, 6, 3, 12, 0, 0, 0, time.UTC)
	none := formatCircuitBreakersResponse(&AppState{Strategies: map[string]*StrategyState{}}, now)
	if !strings.Contains(none, "No active") {
		t.Errorf("expected no-breakers message, got: %s", none)
	}

	state := &AppState{
		Strategies: map[string]*StrategyState{
			"hl-a": {ID: "hl-a", RiskState: RiskState{CircuitBreaker: true, CircuitBreakerUntil: now.Add(10 * time.Minute)}},
		},
		PortfolioRisk: PortfolioRiskState{KillSwitchActive: true},
	}
	got := formatCircuitBreakersResponse(state, now)
	if !strings.Contains(got, "hl-a") {
		t.Errorf("expected breaker for hl-a, got: %s", got)
	}
	if !strings.Contains(strings.ToLower(got), "kill switch") {
		t.Errorf("expected kill-switch note, got: %s", got)
	}
}

func TestFormatDeadStrategiesResponse(t *testing.T) {
	state := &AppState{Strategies: map[string]*StrategyState{"hl-a": {ID: "hl-a"}, "hl-b": {ID: "hl-b"}}}
	lifetime := map[string]LifetimeTradeStats{"hl-a": {PositionsOpened: 3}} // hl-b is dead
	got := formatDeadStrategiesResponse(state, lifetime)
	if !strings.Contains(got, "hl-b") || strings.Contains(got, "hl-a") {
		t.Errorf("expected only hl-b listed as dead, got: %s", got)
	}
}

func TestFormatCorrelationResponse(t *testing.T) {
	if got := formatCorrelationResponse(nil); !strings.Contains(got, "No correlation") {
		t.Errorf("expected nil-snapshot message, got: %s", got)
	}
	snap := &CorrelationSnapshot{
		PortfolioGrossUSD: 1000,
		Warnings:          []string{"BTC concentration 80%"},
		Assets:            map[string]*AssetExposure{"BTC": {NetDeltaUSD: 800, ConcentrationPct: 80}},
	}
	got := formatCorrelationResponse(snap)
	if !strings.Contains(got, "BTC") || !strings.Contains(got, "80") {
		t.Errorf("expected BTC concentration, got: %s", got)
	}
}

func TestFormatCorrelationResponseDeterministicTies(t *testing.T) {
	snap := &CorrelationSnapshot{
		PortfolioGrossUSD: 1000,
		Assets: map[string]*AssetExposure{
			"ETH": {NetDeltaUSD: 500, ConcentrationPct: 50},
			"BTC": {NetDeltaUSD: 500, ConcentrationPct: 50},
			"SOL": {NetDeltaUSD: 500, ConcentrationPct: 50},
		},
	}
	// Equal concentration -> tie-break by asset name ascending, stable across runs.
	first := formatCorrelationResponse(snap)
	for i := 0; i < 20; i++ {
		if got := formatCorrelationResponse(snap); got != first {
			t.Fatalf("non-deterministic output on tied concentration:\n%s\n---\n%s", first, got)
		}
	}
	bi := strings.Index(first, "BTC")
	ei := strings.Index(first, "ETH")
	si := strings.Index(first, "SOL")
	if !(bi < ei && ei < si) {
		t.Errorf("expected BTC<ETH<SOL ordering on ties, got: %s", first)
	}
}

package main

import (
	"regexp"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
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
		{"logs", "anyone", "guild1", false}, // logs is ops now: guild rejected
		{"logs", "intruder", "", false},     // logs is ops now: non-owner DM rejected
		{"logs", owner, "", true},           // logs is ops now: owner DM OK
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

// discordCommandNameRe is Discord's CHAT_INPUT command-name constraint for our
// ASCII command set: 1..32 chars, lowercase letters, digits, dash, underscore.
var discordCommandNameRe = regexp.MustCompile(`^[a-z0-9_-]{1,32}$`)

// TestSlashCommandsNamespaced locks the #891 namespacing invariants: every
// registered command is prefixed with commandPrefix, is a valid Discord command
// name, and strips back (as interactionCreate does) to a bare ID that is exactly
// one of the routable commands in readOnlyCommandNames or opsCommandNames. The
// stripped set must equal the union of those maps — so a command added to
// slashCommands() without a classification (or vice versa) fails the build.
func TestSlashCommandsNamespaced(t *testing.T) {
	registered := map[string]bool{}
	for _, c := range slashCommands() {
		if !strings.HasPrefix(c.Name, commandPrefix) {
			t.Errorf("command %q is not prefixed with %q", c.Name, commandPrefix)
		}
		if !discordCommandNameRe.MatchString(c.Name) {
			t.Errorf("command %q is not a valid Discord command name (len/charset)", c.Name)
		}
		bare := strings.TrimPrefix(c.Name, commandPrefix)
		if bare == "" {
			t.Errorf("command %q strips to an empty ID", c.Name)
			continue
		}
		if !readOnlyCommandNames[bare] && !opsCommandNames[bare] {
			t.Errorf("command %q strips to %q, which is in neither readOnlyCommandNames nor opsCommandNames", c.Name, bare)
		}
		if registered[bare] {
			t.Errorf("command ID %q registered more than once", bare)
		}
		registered[bare] = true
	}

	for name := range readOnlyCommandNames {
		if !registered[name] {
			t.Errorf("readOnlyCommandNames has %q but slashCommands() never registers %q", name, commandPrefix+name)
		}
	}
	for name := range opsCommandNames {
		if !registered[name] {
			t.Errorf("opsCommandNames has %q but slashCommands() never registers %q", name, commandPrefix+name)
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

func TestFormatPositionsResponseLabelsHedgeLeg(t *testing.T) {
	state := &AppState{Strategies: map[string]*StrategyState{
		"hl-eth": {ID: "hl-eth", Platform: "hyperliquid", Positions: map[string]*Position{
			"BTC": {Symbol: "BTC", Quantity: 0.02, AvgCost: 100000, Side: "short", HedgeFor: "ETH", HedgePrimaryQtyBasis: 1.5},
		}},
	}}
	got := formatPositionsResponse(state, map[string]float64{"BTC": 100000})
	if !strings.Contains(got, "hedge for ETH") || !strings.Contains(got, "primary basis 1.5000") {
		t.Fatalf("hedge label missing from positions response: %s", got)
	}
}

func TestFormatPnLResponse(t *testing.T) {
	// hl-a: pv = 1*60 = 60, cap 50 -> +10 (+20%). hl-b: pv = 50, cap 50 -> 0.
	got := formatPnLResponse(testPnLState(), map[string]float64{"BTC": 60})
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

func TestFormatLeaderboardResponse(t *testing.T) {
	cfg := &Config{
		IntervalSeconds: 3600,
		Strategies: []StrategyConfig{
			{ID: "hl-a", Platform: "hyperliquid"},
			{ID: "hl-b", Platform: "hyperliquid"},
		},
	}
	state := testPnLState() // hl-a +20%, hl-b 0%
	got := formatLeaderboardResponse(cfg, state, map[string]float64{"BTC": 60}, nil, 5)
	// hl-a should rank above hl-b.
	ai := strings.Index(got, "hl-a")
	bi := strings.Index(got, "hl-b")
	if ai < 0 || bi < 0 || ai > bi {
		t.Errorf("expected hl-a ranked above hl-b, got: %s", got)
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

func TestParseBacktestSummary(t *testing.T) {
	report := strings.Join([]string{
		"  RETURNS",
		"    Total Return:    +12.34%",
		"  RISK METRICS",
		"    Sharpe Ratio:    1.234",
		"    Max Drawdown:    8.50%",
		"  TRADE STATS",
		"    Total Trades:    17",
		"    Win Rate:        58.8%",
	}, "\n")
	got := parseBacktestSummary(report)
	for _, want := range []string{"+12.34%", "1.234", "8.50%", "17", "58.8%"} {
		if !strings.Contains(got, want) {
			t.Errorf("summary missing %q; got: %s", want, got)
		}
	}

	// Missing labels degrade to a dash rather than erroring.
	if got := parseBacktestSummary("no metrics here"); !strings.Contains(got, "—") {
		t.Errorf("expected dash for missing metrics, got: %s", got)
	}
}

func TestTruncateForDiscord(t *testing.T) {
	// Short input is returned unchanged.
	if got := truncateForDiscord("hello"); got != "hello" {
		t.Errorf("short input mutated: %q", got)
	}

	// Over-limit ASCII input is capped to 2000 bytes with an ellipsis.
	long := strings.Repeat("a", 2500)
	got := truncateForDiscord(long)
	if len(got) > 2000 {
		t.Errorf("truncated output exceeds 2000 bytes: %d", len(got))
	}
	if !strings.HasSuffix(got, "...") {
		t.Errorf("expected ellipsis suffix, got tail: %q", got[len(got)-5:])
	}

	// Multibyte runes at the cut boundary must not be split (no invalid bytes).
	multibyte := strings.Repeat("🛑", 1000) // 4 bytes each = 4000 bytes
	got = truncateForDiscord(multibyte)
	if len(got) > 2000 {
		t.Errorf("truncated multibyte output exceeds 2000 bytes: %d", len(got))
	}
	if !utf8.ValidString(got) {
		t.Errorf("truncation split a rune (invalid UTF-8): %q", got)
	}
}

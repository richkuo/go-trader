# Discord Slash Commands Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add Discord slash commands to go-trader's in-process bot for common operator actions — read-only monitoring (guild + DM, anyone) and owner-DM-only ops (`/restart`, `/backtest`).

**Architecture:** A new `scheduler/discord_commands.go` owns the slash-command definitions, a pure `authorizeCommand` gate, pure response builders that read in-process `*AppState`, the `interactionCreate` gateway handler, and global command registration. The existing `DiscordNotifier` (which already holds the open `discordgo.Session`) gains a `RegisterSlashCommands(ss *StatusServer, cfg *Config)` method, wired from `main.go` after the notifier and `StatusServer` exist. Read-only commands read live state via the `*StatusServer` (same process); ops use Discord deferred responses.

**Tech Stack:** Go, `github.com/bwmarrin/discordgo` v0.29.0 (already a dependency), SQLite (`StateDB`), Python subprocess (`run_backtest.py`), `journalctl`/`systemctl`.

---

## Background facts (verified against the codebase)

- `DiscordNotifier` (`scheduler/discord.go`) wraps `session *discordgo.Session` (gateway already opened in `NewDiscordNotifier`) and stores `ownerID string`. Same `package main`, so new files can use its unexported fields.
- `StatusServer` (`scheduler/server.go`) holds `state *AppState`, `mu *sync.RWMutex`, `stateDB *StateDB`, `strategies []StrategyConfig`, `regime *RegimeConfig`, and the price rails. `func (ss *StatusServer) fetchLiveMarkPrices() map[string]float64` must be called **without** holding `ss.mu`.
- `MultiNotifier` (`scheduler/notifier.go`) has `backends []notifierBackend`; each `notifierBackend.notifier` is a `Notifier`. The Discord backend's concrete type is `*DiscordNotifier`.
- Config hot-reload (`config_reload.go`) mutates `cfg` **in place** (never swaps the pointer) and blocks strategy add/remove — so storing the `*Config` pointer is safe across SIGHUP.
- `main.go`: `server := NewStatusServer(...)` at ~line 248, `server.Start(statusPort)` at ~line 250, `notifier, cleanupNotifier := buildNotifierFromConfig(cfg)` at ~line 276. Wiring goes right after line ~278 (`notifier.BackendCount()` print), in the daemon path only.
- `runPython`'s default `scriptTimeout` is **30s** — too short for a backtest. Use `runPythonWithTimeout(shutdownReadOnlyCtx, "backtest/run_backtest.py", args, nil, 5*time.Minute)` (read-only ctx + long timeout). `shutdownReadOnlyCtx` is a package var in `shutdown.go`. The daemon's working directory is the repo root, and `runPython` invokes `.venv/bin/python3 <script>` relative to it, so script path `backtest/run_backtest.py` resolves correctly.
- `run_backtest.py --mode single` prints a human-readable report (`backtest/reporter.py::format_single_report`) containing the lines `Total Return:`, `Sharpe Ratio:`, `Max Drawdown:`, `Total Trades:`, `Win Rate:`. No JSON output mode — parse those labels.
- Key types: `AppState{CycleCount int, LastCycle time.Time, Strategies map[string]*StrategyState, PortfolioRisk PortfolioRiskState, CorrelationSnapshot *CorrelationSnapshot}`. `StrategyState{ID, Type, Platform, Cash, InitialCapital, Positions map[string]*Position, OptionPositions map[string]*OptionPosition, TradeHistory []Trade, RiskState RiskState, Regime string}`. `Position{Symbol, Quantity, AvgCost, Side, Multiplier}`. `RiskState{CircuitBreaker bool, CircuitBreakerUntil time.Time, PendingCircuitCloses map[string]*PendingCircuitClose}`. `PortfolioRiskState{KillSwitchActive bool, KillSwitchAt time.Time, CurrentDrawdownPct float64}`. `CorrelationSnapshot{Assets map[string]*AssetExposure, PortfolioGrossUSD float64, Warnings []string}`. `AssetExposure{NetDeltaUSD, ConcentrationPct float64}`. `LifetimeTradeStats{PositionsOpened, Wins, Losses int}`.
- Reusable funcs: `PortfolioValue(s *StrategyState, prices map[string]float64) float64`, `EffectiveInitialCapital(sc StrategyConfig, ss *StrategyState) float64`, `newLeaderboardEntry(sc, ss, pv, initCap, pnl, pnlPct, sharpeByStrategy, lifetimeStats, globalIntervalSeconds) LeaderboardEntry`, `formatStatusLine(cash, posCount, value, trades, regime) string`, `(*StateDB).LifetimeTradeStatsAll() (map[string]LifetimeTradeStats, error)`.
- discordgo APIs: `ApplicationCommand{Type, Name, Description, Options, Contexts *[]InteractionContextType}`, `ApplicationCommandOption{Type, Name, Description, Required}`, option type consts `ApplicationCommandOptionString`(=3)/`ApplicationCommandOptionInteger`(=4), `InteractionContextBotDM`(=1), `(*Session).ApplicationCommandBulkOverwrite(appID, guildID string, cmds []*ApplicationCommand)`, `(*Session).AddHandler(any)`, `(*Session).InteractionRespond(*Interaction, *InteractionResponse)`, `(*Session).FollowupMessageCreate(*Interaction, wait bool, *WebhookParams)`, `InteractionResponseData{Content string, Flags MessageFlags, Files []*File}`, `WebhookParams{Content string, Files []*File}`, `File{Name, ContentType string, Reader io.Reader}`, `MessageFlagsEphemeral`. The app ID is `session.State.User.ID` after the gateway opens. Option value accessors `(o).StringValue()` / `(o).IntValue()` **panic** if called on the wrong declared type — only call them on options declared with that type.

---

## File Structure

- **Create** `scheduler/discord_commands.go` — command defs, `authorizeCommand`, pure builders, helpers, `interactionCreate`, `RegisterSlashCommands`, ops handlers.
- **Create** `scheduler/discord_commands_test.go` — unit tests for all pure functions.
- **Modify** `scheduler/notifier.go` — add `(*MultiNotifier) DiscordBackend() *DiscordNotifier`.
- **Modify** `scheduler/discord.go` — add `ss *StatusServer` and `cfg *Config` fields to `DiscordNotifier`.
- **Modify** `scheduler/main.go` — call `RegisterSlashCommands` after the notifier is built.
- **Modify** `SKILL.md` — document the commands, auth model, and `applications.commands` invite scope.

---

## Task 1: `MultiNotifier.DiscordBackend()` accessor

**Files:**
- Modify: `scheduler/notifier.go`
- Test: `scheduler/discord_commands_test.go` (create)

- [ ] **Step 1: Write the failing test**

Create `scheduler/discord_commands_test.go`:

```go
package main

import "testing"

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
```

- [ ] **Step 2: Run test to verify it fails**

Run: `cd /Users/richardkuo/Work/go-trader && go -C scheduler test -run TestDiscordBackend ./...`
Expected: FAIL — `mn.DiscordBackend undefined`.

- [ ] **Step 3: Add the accessor**

Append to `scheduler/notifier.go`:

```go
// DiscordBackend returns the registered *DiscordNotifier, or nil if Discord is
// not configured. Used to attach slash-command handling after startup.
func (m *MultiNotifier) DiscordBackend() *DiscordNotifier {
	m.mu.RLock()
	defer m.mu.RUnlock()
	for _, b := range m.backends {
		if d, ok := b.notifier.(*DiscordNotifier); ok {
			return d
		}
	}
	return nil
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `cd /Users/richardkuo/Work/go-trader && go -C scheduler test -run TestDiscordBackend ./...`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
cd /Users/richardkuo/Work/go-trader
gofmt -w scheduler/notifier.go scheduler/discord_commands_test.go
git add scheduler/notifier.go scheduler/discord_commands_test.go
git commit -m "feat(discord): add MultiNotifier.DiscordBackend accessor (#212)"
```

---

## Task 2: `authorizeCommand` gate

**Files:**
- Create: `scheduler/discord_commands.go`
- Test: `scheduler/discord_commands_test.go`

- [ ] **Step 1: Write the failing test**

Add to `scheduler/discord_commands_test.go`:

```go
func TestAuthorizeCommand(t *testing.T) {
	const owner = "owner123"
	cases := []struct {
		name, invoker, guildID string
		wantOK                 bool
	}{
		{"status", "anyone", "guild1", true},   // read-only in guild OK
		{"status", "anyone", "", true},         // read-only in DM OK
		{"positions", "anyone", "guild1", true},
		{"logs", "anyone", "guild1", true},
		{"restart", owner, "", true},           // ops: owner in DM OK
		{"restart", owner, "guild1", false},    // ops: owner in guild rejected (must be DM)
		{"restart", "intruder", "", false},     // ops: non-owner in DM rejected
		{"backtest", owner, "", true},
		{"backtest", "intruder", "", false},
		{"unknown", owner, "", false},          // unknown command rejected
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
```

- [ ] **Step 2: Run test to verify it fails**

Run: `cd /Users/richardkuo/Work/go-trader && go -C scheduler test -run TestAuthorizeCommand ./...`
Expected: FAIL — `authorizeCommand undefined`.

- [ ] **Step 3: Create the file with command sets and the gate**

Create `scheduler/discord_commands.go`:

```go
package main

import (
	"fmt"

	"github.com/bwmarrin/discordgo"
)

// readOnlyCommandNames are usable in a guild or in DMs by anyone.
var readOnlyCommandNames = map[string]bool{
	"status":           true,
	"health":           true,
	"positions":        true,
	"pnl":              true,
	"leaderboard":      true,
	"circuit-breakers": true,
	"dead-strategies":  true,
	"correlation":      true,
	"logs":             true,
}

// opsCommandNames mutate state or run heavy work; restricted to the owner in a DM.
var opsCommandNames = map[string]bool{
	"restart":  true,
	"backtest": true,
}

// authorizeCommand decides whether invokerID may run command `name`. Read-only
// commands are always allowed. Ops commands require the invoker to be the owner
// AND the interaction to be a DM (guildID == ""). Returns (false, reason) on deny.
func authorizeCommand(name, invokerID, guildID, ownerID string) (bool, string) {
	if readOnlyCommandNames[name] {
		return true, ""
	}
	if opsCommandNames[name] {
		if ownerID == "" {
			return false, "owner is not configured; ops commands are disabled"
		}
		if invokerID != ownerID {
			return false, "not authorized — this command is owner-only"
		}
		if guildID != "" {
			return false, "this command is only available in a DM with the bot"
		}
		return true, ""
	}
	return false, fmt.Sprintf("unknown command: %s", name)
}

// interactionUserID extracts the invoking user's ID from either a guild
// (i.Member.User) or DM (i.User) interaction.
func interactionUserID(i *discordgo.InteractionCreate) string {
	if i.Member != nil && i.Member.User != nil {
		return i.Member.User.ID
	}
	if i.User != nil {
		return i.User.ID
	}
	return ""
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `cd /Users/richardkuo/Work/go-trader && go -C scheduler test -run TestAuthorizeCommand ./...`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
cd /Users/richardkuo/Work/go-trader
gofmt -w scheduler/discord_commands.go scheduler/discord_commands_test.go
git add scheduler/discord_commands.go scheduler/discord_commands_test.go
git commit -m "feat(discord): add slash command authorization gate (#212)"
```

---

## Task 3: Shared helpers + `/health` and `/status` builders

**Files:**
- Modify: `scheduler/discord_commands.go`
- Test: `scheduler/discord_commands_test.go`

- [ ] **Step 1: Write the failing test**

Add to `scheduler/discord_commands_test.go`:

```go
import (
	"strings"
	"time"
)

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
```

- [ ] **Step 2: Run test to verify it fails**

Run: `cd /Users/richardkuo/Work/go-trader && go -C scheduler test -run 'TestFormatHealthResponse|TestFormatStatusResponse' ./...`
Expected: FAIL — `formatHealthResponse undefined`, `formatStatusResponse undefined`.

- [ ] **Step 3: Add helpers and builders**

Append to `scheduler/discord_commands.go` (and add `"sort"`, `"strings"`, `"time"` to its imports):

```go
// sortedStrategyIDs returns the strategy IDs of state in deterministic order.
func sortedStrategyIDs(state *AppState) []string {
	ids := make([]string, 0, len(state.Strategies))
	for id := range state.Strategies {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

// strategyPlatformLabel returns a human label for grouping (platform, else type).
func strategyPlatformLabel(s *StrategyState) string {
	if s.Platform != "" {
		return s.Platform
	}
	return s.Type
}

// positionMultiplier returns the PnL multiplier for a position (1 for spot).
func positionMultiplier(p *Position) float64 {
	if p.Multiplier > 0 {
		return p.Multiplier
	}
	return 1
}

// formatHealthResponse summarizes daemon liveness. `now` is injected for tests.
func formatHealthResponse(lastCycle time.Time, cycleCount int, version string, now time.Time) string {
	var sb strings.Builder
	sb.WriteString("**go-trader health**\n")
	sb.WriteString(fmt.Sprintf("version: %s\n", version))
	sb.WriteString(fmt.Sprintf("cycles completed: %d\n", cycleCount))
	if lastCycle.IsZero() {
		sb.WriteString("last cycle: never (no cycle completed yet)\n")
		sb.WriteString("status: starting")
		return sb.String()
	}
	age := now.Sub(lastCycle).Round(time.Second)
	status := "ok"
	if age > 30*time.Minute {
		status = "unhealthy (main loop stale)"
	}
	sb.WriteString(fmt.Sprintf("last cycle: %s ago\n", age))
	sb.WriteString(fmt.Sprintf("status: %s", status))
	return sb.String()
}

// formatStatusResponse builds a portfolio-wide one-line status. Call under RLock.
func formatStatusResponse(state *AppState, prices map[string]float64) string {
	var cash, value float64
	posCount, trades := 0, 0
	regime := ""
	for _, id := range sortedStrategyIDs(state) {
		s := state.Strategies[id]
		cash += s.Cash
		value += PortfolioValue(s, prices)
		posCount += len(s.Positions) + len(s.OptionPositions)
		trades += len(s.TradeHistory)
		if regime == "" && s.Regime != "" {
			regime = s.Regime
		}
	}
	return formatStatusLine(cash, posCount, value, trades, regime)
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `cd /Users/richardkuo/Work/go-trader && go -C scheduler test -run 'TestFormatHealthResponse|TestFormatStatusResponse' ./...`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
cd /Users/richardkuo/Work/go-trader
gofmt -w scheduler/discord_commands.go scheduler/discord_commands_test.go
git add scheduler/discord_commands.go scheduler/discord_commands_test.go
git commit -m "feat(discord): add /health and /status response builders (#212)"
```

---

## Task 4: `/positions` and `/pnl` builders

**Files:**
- Modify: `scheduler/discord_commands.go`
- Test: `scheduler/discord_commands_test.go`

- [ ] **Step 1: Write the failing test**

Add to `scheduler/discord_commands_test.go`:

```go
func testPnLState() *AppState {
	return &AppState{Strategies: map[string]*StrategyState{
		"hl-a": {ID: "hl-a", Platform: "hyperliquid", Cash: 0, InitialCapital: 50,
			Positions: map[string]*Position{"BTC": {Symbol: "BTC", Quantity: 1, AvgCost: 50, Side: "long", Multiplier: 1}}},
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
```

- [ ] **Step 2: Run test to verify it fails**

Run: `cd /Users/richardkuo/Work/go-trader && go -C scheduler test -run 'TestFormatPositionsResponse|TestFormatPnLResponse' ./...`
Expected: FAIL — `formatPositionsResponse undefined`, `formatPnLResponse undefined`.

- [ ] **Step 3: Add the builders**

Append to `scheduler/discord_commands.go`:

```go
// formatPositionsResponse lists open positions grouped by platform. Call under RLock.
func formatPositionsResponse(state *AppState, prices map[string]float64) string {
	lines := map[string][]string{} // platform -> position lines
	platforms := []string{}
	for _, id := range sortedStrategyIDs(state) {
		s := state.Strategies[id]
		syms := make([]string, 0, len(s.Positions))
		for sym := range s.Positions {
			syms = append(syms, sym)
		}
		sort.Strings(syms)
		for _, sym := range syms {
			p := s.Positions[sym]
			if p.Quantity == 0 {
				continue
			}
			price := prices[sym]
			if price == 0 {
				price = p.AvgCost
			}
			mv := price * p.Quantity * positionMultiplier(p)
			plat := strategyPlatformLabel(s)
			if _, ok := lines[plat]; !ok {
				platforms = append(platforms, plat)
			}
			lines[plat] = append(lines[plat], fmt.Sprintf(
				"  %s %s %.4f @ $%.2f (mv $%.2f) [%s]", sym, p.Side, p.Quantity, p.AvgCost, mv, id))
		}
	}
	if len(platforms) == 0 {
		return "No open positions."
	}
	sort.Strings(platforms)
	var sb strings.Builder
	sb.WriteString("**Open positions**\n")
	for _, plat := range platforms {
		sb.WriteString("__" + plat + "__\n")
		sb.WriteString(strings.Join(lines[plat], "\n"))
		sb.WriteString("\n")
	}
	return strings.TrimRight(sb.String(), "\n")
}

// formatPnLResponse reports total / per-platform / per-strategy P&L. Call under RLock.
func formatPnLResponse(state *AppState, prices map[string]float64, lifetime map[string]LifetimeTradeStats) string {
	type agg struct{ value, capital float64 }
	byPlatform := map[string]*agg{}
	platforms := []string{}
	var totVal, totCap float64
	var perStrat []string
	for _, id := range sortedStrategyIDs(state) {
		s := state.Strategies[id]
		pv := PortfolioValue(s, prices)
		cap := s.InitialCapital
		pnl := pv - cap
		pnlPct := 0.0
		if cap > 0 {
			pnlPct = pnl / cap * 100
		}
		totVal += pv
		totCap += cap
		plat := strategyPlatformLabel(s)
		if byPlatform[plat] == nil {
			byPlatform[plat] = &agg{}
			platforms = append(platforms, plat)
		}
		byPlatform[plat].value += pv
		byPlatform[plat].capital += cap
		perStrat = append(perStrat, fmt.Sprintf("  %s: $%+.2f (%+.2f%%)", id, pnl, pnlPct))
	}
	sort.Strings(platforms)
	var sb strings.Builder
	sb.WriteString("**P&L**\n")
	totPnL := totVal - totCap
	totPct := 0.0
	if totCap > 0 {
		totPct = totPnL / totCap * 100
	}
	sb.WriteString(fmt.Sprintf("Total: $%+.2f (%+.2f%%) — value $%.2f / capital $%.2f\n", totPnL, totPct, totVal, totCap))
	sb.WriteString("__By platform__\n")
	for _, plat := range platforms {
		a := byPlatform[plat]
		pnl := a.value - a.capital
		pct := 0.0
		if a.capital > 0 {
			pct = pnl / a.capital * 100
		}
		sb.WriteString(fmt.Sprintf("  %s: $%+.2f (%+.2f%%)\n", plat, pnl, pct))
	}
	sb.WriteString("__By strategy__\n")
	sb.WriteString(strings.Join(perStrat, "\n"))
	return strings.TrimRight(sb.String(), "\n")
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `cd /Users/richardkuo/Work/go-trader && go -C scheduler test -run 'TestFormatPositionsResponse|TestFormatPnLResponse' ./...`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
cd /Users/richardkuo/Work/go-trader
gofmt -w scheduler/discord_commands.go scheduler/discord_commands_test.go
git add scheduler/discord_commands.go scheduler/discord_commands_test.go
git commit -m "feat(discord): add /positions and /pnl response builders (#212)"
```

---

## Task 5: `/circuit-breakers`, `/dead-strategies`, `/correlation` builders

**Files:**
- Modify: `scheduler/discord_commands.go`
- Test: `scheduler/discord_commands_test.go`

- [ ] **Step 1: Write the failing test**

Add to `scheduler/discord_commands_test.go`:

```go
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
```

- [ ] **Step 2: Run test to verify it fails**

Run: `cd /Users/richardkuo/Work/go-trader && go -C scheduler test -run 'TestFormatCircuitBreakersResponse|TestFormatDeadStrategiesResponse|TestFormatCorrelationResponse' ./...`
Expected: FAIL — the three functions are undefined.

- [ ] **Step 3: Add the builders**

Append to `scheduler/discord_commands.go`:

```go
// formatCircuitBreakersResponse lists open per-strategy breakers + portfolio kill switch. Call under RLock.
func formatCircuitBreakersResponse(state *AppState, now time.Time) string {
	var lines []string
	for _, id := range sortedStrategyIDs(state) {
		rs := state.Strategies[id].RiskState
		if rs.CircuitBreaker {
			until := "no expiry set"
			if !rs.CircuitBreakerUntil.IsZero() {
				if rs.CircuitBreakerUntil.After(now) {
					until = "clears in " + rs.CircuitBreakerUntil.Sub(now).Round(time.Second).String()
				} else {
					until = "expired (clears next cycle)"
				}
			}
			lines = append(lines, fmt.Sprintf("  %s: OPEN (%s)", id, until))
		}
		if len(rs.PendingCircuitCloses) > 0 {
			lines = append(lines, fmt.Sprintf("  %s: pending circuit close (%d venue)", id, len(rs.PendingCircuitCloses)))
		}
	}
	var sb strings.Builder
	if state.PortfolioRisk.KillSwitchActive {
		sb.WriteString(fmt.Sprintf("🛑 Portfolio kill switch ACTIVE (drawdown %.2f%%)\n", state.PortfolioRisk.CurrentDrawdownPct))
	}
	if len(lines) == 0 {
		if sb.Len() == 0 {
			return "No active circuit breakers."
		}
		return strings.TrimRight(sb.String(), "\n")
	}
	sb.WriteString("**Active circuit breakers**\n")
	sb.WriteString(strings.Join(lines, "\n"))
	return strings.TrimRight(sb.String(), "\n")
}

// formatDeadStrategiesResponse lists strategies that have never opened a position. Call under RLock.
func formatDeadStrategiesResponse(state *AppState, lifetime map[string]LifetimeTradeStats) string {
	var dead []string
	for _, id := range sortedStrategyIDs(state) {
		if lifetime[id].PositionsOpened == 0 {
			dead = append(dead, "  "+id)
		}
	}
	if len(dead) == 0 {
		return "All strategies have opened at least one position."
	}
	return fmt.Sprintf("**Dead strategies (0 positions opened) — %d**\n%s", len(dead), strings.Join(dead, "\n"))
}

// formatCorrelationResponse renders the latest correlation/concentration snapshot.
func formatCorrelationResponse(snap *CorrelationSnapshot) string {
	if snap == nil {
		return "No correlation snapshot yet (computed during the trading cycle)."
	}
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("**Correlation / concentration** (gross $%.2f)\n", snap.PortfolioGrossUSD))
	if len(snap.Warnings) > 0 {
		sb.WriteString("⚠️ Warnings:\n")
		for _, w := range snap.Warnings {
			sb.WriteString("  " + w + "\n")
		}
	} else {
		sb.WriteString("No warnings.\n")
	}
	assets := make([]string, 0, len(snap.Assets))
	for a := range snap.Assets {
		assets = append(assets, a)
	}
	sort.Slice(assets, func(i, j int) bool {
		return snap.Assets[assets[i]].ConcentrationPct > snap.Assets[assets[j]].ConcentrationPct
	})
	for _, a := range assets {
		e := snap.Assets[a]
		sb.WriteString(fmt.Sprintf("  %s: net $%.2f, concentration %.1f%%\n", a, e.NetDeltaUSD, e.ConcentrationPct))
	}
	return strings.TrimRight(sb.String(), "\n")
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `cd /Users/richardkuo/Work/go-trader && go -C scheduler test -run 'TestFormatCircuitBreakersResponse|TestFormatDeadStrategiesResponse|TestFormatCorrelationResponse' ./...`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
cd /Users/richardkuo/Work/go-trader
gofmt -w scheduler/discord_commands.go scheduler/discord_commands_test.go
git add scheduler/discord_commands.go scheduler/discord_commands_test.go
git commit -m "feat(discord): add /circuit-breakers, /dead-strategies, /correlation builders (#212)"
```

---

## Task 6: `/leaderboard` builder

**Files:**
- Modify: `scheduler/discord_commands.go`
- Test: `scheduler/discord_commands_test.go`

- [ ] **Step 1: Write the failing test**

Add to `scheduler/discord_commands_test.go`:

```go
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
```

- [ ] **Step 2: Run test to verify it fails**

Run: `cd /Users/richardkuo/Work/go-trader && go -C scheduler test -run TestFormatLeaderboardResponse ./...`
Expected: FAIL — `formatLeaderboardResponse undefined`.

- [ ] **Step 3: Add the builder**

Append to `scheduler/discord_commands.go`:

```go
// formatLeaderboardResponse ranks all strategies by PnL% (descending), top N.
// Reuses newLeaderboardEntry for per-strategy metrics. Call under RLock.
func formatLeaderboardResponse(cfg *Config, state *AppState, prices map[string]float64, lifetime map[string]LifetimeTradeStats, topN int) string {
	if topN <= 0 {
		topN = 5
	}
	var entries []LeaderboardEntry
	for _, sc := range cfg.Strategies {
		ss := state.Strategies[sc.ID]
		if ss == nil {
			continue
		}
		pv := PortfolioValue(ss, prices)
		initCap := EffectiveInitialCapital(sc, ss)
		pnl := pv - initCap
		pnlPct := 0.0
		if initCap > 0 {
			pnlPct = pnl / initCap * 100
		}
		entries = append(entries, newLeaderboardEntry(sc, ss, pv, initCap, pnl, pnlPct, nil, lifetime, cfg.IntervalSeconds))
	}
	if len(entries) == 0 {
		return "No strategies to rank."
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].PnLPct > entries[j].PnLPct })
	if topN > len(entries) {
		topN = len(entries)
	}
	var sb strings.Builder
	sb.WriteString("**Leaderboard (by PnL%)**\n")
	for i := 0; i < topN; i++ {
		e := entries[i]
		sb.WriteString(fmt.Sprintf("  %d. %s — %+.2f%% ($%+.2f)\n", i+1, e.ID, e.PnLPct, e.PnL))
	}
	return strings.TrimRight(sb.String(), "\n")
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `cd /Users/richardkuo/Work/go-trader && go -C scheduler test -run TestFormatLeaderboardResponse ./...`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
cd /Users/richardkuo/Work/go-trader
gofmt -w scheduler/discord_commands.go scheduler/discord_commands_test.go
git add scheduler/discord_commands.go scheduler/discord_commands_test.go
git commit -m "feat(discord): add /leaderboard response builder (#212)"
```

---

## Task 7: `/backtest` output parser

**Files:**
- Modify: `scheduler/discord_commands.go`
- Test: `scheduler/discord_commands_test.go`

- [ ] **Step 1: Write the failing test**

Add to `scheduler/discord_commands_test.go`:

```go
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
```

- [ ] **Step 2: Run test to verify it fails**

Run: `cd /Users/richardkuo/Work/go-trader && go -C scheduler test -run TestParseBacktestSummary ./...`
Expected: FAIL — `parseBacktestSummary undefined`.

- [ ] **Step 3: Add the parser**

Append to `scheduler/discord_commands.go`:

```go
// parseBacktestSummary extracts headline metrics from run_backtest.py's
// single-mode text report (backtest/reporter.py::format_single_report).
// Missing labels render as "—" so a partial report still produces output.
func parseBacktestSummary(report string) string {
	lines := strings.Split(report, "\n")
	grab := func(label string) string {
		for _, ln := range lines {
			if idx := strings.Index(ln, label); idx >= 0 {
				return strings.TrimSpace(ln[idx+len(label):])
			}
		}
		return "—"
	}
	return fmt.Sprintf("Total Return: %s | Sharpe: %s | Max DD: %s | Trades: %s | Win Rate: %s",
		grab("Total Return:"), grab("Sharpe Ratio:"), grab("Max Drawdown:"), grab("Total Trades:"), grab("Win Rate:"))
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `cd /Users/richardkuo/Work/go-trader && go -C scheduler test -run TestParseBacktestSummary ./...`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
cd /Users/richardkuo/Work/go-trader
gofmt -w scheduler/discord_commands.go scheduler/discord_commands_test.go
git add scheduler/discord_commands.go scheduler/discord_commands_test.go
git commit -m "feat(discord): add backtest report summary parser (#212)"
```

---

## Task 8: Command definitions + DiscordNotifier fields + registration

**Files:**
- Modify: `scheduler/discord.go` (add struct fields)
- Modify: `scheduler/discord_commands.go` (defs + registration)

- [ ] **Step 1: Add fields to DiscordNotifier**

In `scheduler/discord.go`, add two fields to the `DiscordNotifier` struct (after `mu sync.Mutex`):

```go
	// Slash-command context, set by RegisterSlashCommands; nil until then.
	ss  *StatusServer
	cfg *Config
```

- [ ] **Step 2: Add command definitions**

Append to `scheduler/discord_commands.go`:

```go
// dmContext restricts a command to DMs with the bot (used for ops commands).
func dmContext() *[]discordgo.InteractionContextType {
	return &[]discordgo.InteractionContextType{discordgo.InteractionContextBotDM}
}

// slashCommands returns the full set of application commands to register globally.
func slashCommands() []*discordgo.ApplicationCommand {
	return []*discordgo.ApplicationCommand{
		{Name: "status", Description: "Live portfolio status (cash, positions, value, regime)"},
		{Name: "health", Description: "Daemon health: running, last cycle, version"},
		{Name: "positions", Description: "Open positions across platforms"},
		{Name: "pnl", Description: "Portfolio P&L (total, per-platform, per-strategy)"},
		{Name: "leaderboard", Description: "Strategies ranked by P&L%", Options: []*discordgo.ApplicationCommandOption{
			{Type: discordgo.ApplicationCommandOptionInteger, Name: "top", Description: "How many to show (default 5)"},
		}},
		{Name: "circuit-breakers", Description: "Active circuit breakers and kill-switch state"},
		{Name: "dead-strategies", Description: "Strategies that have never opened a position"},
		{Name: "correlation", Description: "Correlation / concentration warnings"},
		{Name: "logs", Description: "Recent journalctl lines", Options: []*discordgo.ApplicationCommandOption{
			{Type: discordgo.ApplicationCommandOptionInteger, Name: "n", Description: "Number of lines (default 50, max 200)"},
		}},
		// Ops — owner-DM-only (restricted by Contexts; re-checked in the handler).
		{Name: "restart", Description: "Restart the go-trader service (owner DM only)", Contexts: dmContext()},
		{Name: "backtest", Description: "Run a single backtest (owner DM only)", Contexts: dmContext(), Options: []*discordgo.ApplicationCommandOption{
			{Type: discordgo.ApplicationCommandOptionString, Name: "strategy", Description: "Strategy name", Required: true},
			{Type: discordgo.ApplicationCommandOptionString, Name: "symbol", Description: "Symbol, e.g. BTC/USDT", Required: true},
			{Type: discordgo.ApplicationCommandOptionString, Name: "timeframe", Description: "Timeframe (default 1h)"},
		}},
	}
}

// RegisterSlashCommands stores the data references the handlers need, attaches the
// interaction handler, and registers commands globally. Non-fatal on failure: the
// caller logs/DMs and the daemon keeps running.
func (d *DiscordNotifier) RegisterSlashCommands(ss *StatusServer, cfg *Config) error {
	if d == nil || d.session == nil {
		return fmt.Errorf("discord session not initialized")
	}
	if d.session.State == nil || d.session.State.User == nil {
		return fmt.Errorf("discord gateway not ready (no application identity)")
	}
	d.ss = ss
	d.cfg = cfg
	d.session.AddHandler(d.interactionCreate)
	appID := d.session.State.User.ID
	if _, err := d.session.ApplicationCommandBulkOverwrite(appID, "", slashCommands()); err != nil {
		return fmt.Errorf("bulk overwrite commands: %w", err)
	}
	return nil
}
```

- [ ] **Step 3: Build to verify it compiles**

Run: `cd /Users/richardkuo/Work/go-trader && go -C scheduler build ./...`
Expected: builds (handler `interactionCreate` is added in Task 9; until then this step fails with `d.interactionCreate undefined` — that is expected, proceed to Task 9 before building. To keep this task self-contained, temporarily comment out the `d.session.AddHandler(d.interactionCreate)` line, build, then restore it at the start of Task 9.)

- [ ] **Step 4: Commit**

```bash
cd /Users/richardkuo/Work/go-trader
gofmt -w scheduler/discord.go scheduler/discord_commands.go
git add scheduler/discord.go scheduler/discord_commands.go
git commit -m "feat(discord): define slash commands and registration (#212)"
```

---

## Task 9: Interaction handler + response plumbing + ops handlers

**Files:**
- Modify: `scheduler/discord_commands.go`

- [ ] **Step 1: Add the dispatch handler and response helpers**

Append to `scheduler/discord_commands.go` (add `"context"` — no; use `shutdownReadOnlyCtx`; add `"bytes"`, `"os/exec"`, `"strconv"`, `"time"` already present — ensure final import block includes `bytes`, `fmt`, `os/exec`, `sort`, `strconv`, `strings`, `time`, and `github.com/bwmarrin/discordgo`):

```go
// interactionCreate is the gateway handler for slash commands.
func (d *DiscordNotifier) interactionCreate(s *discordgo.Session, i *discordgo.InteractionCreate) {
	if i.Type != discordgo.InteractionApplicationCommand {
		return
	}
	data := i.ApplicationCommandData()
	name := data.Name
	ok, reason := authorizeCommand(name, interactionUserID(i), i.GuildID, d.ownerID)
	if !ok {
		respondEphemeral(s, i, reason)
		return
	}
	switch name {
	case "status":
		respondText(s, i, d.buildReadOnly(formatStatusResponse))
	case "positions":
		respondText(s, i, d.buildReadOnly(formatPositionsResponse))
	case "health":
		respondText(s, i, d.buildHealth())
	case "pnl":
		respondText(s, i, d.buildPnL())
	case "leaderboard":
		respondText(s, i, d.buildLeaderboard(optionInt(data.Options, "top", 5)))
	case "circuit-breakers":
		respondText(s, i, d.buildCircuitBreakers())
	case "dead-strategies":
		respondText(s, i, d.buildDeadStrategies())
	case "correlation":
		respondText(s, i, d.buildCorrelation())
	case "logs":
		respondText(s, i, runLogs(optionInt(data.Options, "n", 50)))
	case "restart":
		d.handleRestart(s, i)
	case "backtest":
		d.handleBacktest(s, i, data)
	default:
		respondEphemeral(s, i, "unknown command")
	}
}

// optionInt reads an integer option by name, with a default and a 1..200 clamp.
func optionInt(opts []*discordgo.ApplicationCommandInteractionDataOption, name string, def int) int {
	for _, o := range opts {
		if o.Name == name && o.Type == discordgo.ApplicationCommandOptionInteger {
			v := int(o.IntValue())
			if v < 1 {
				v = 1
			}
			if v > 200 {
				v = 200
			}
			return v
		}
	}
	return def
}

// optionString reads a string option by name with a default.
func optionString(opts []*discordgo.ApplicationCommandInteractionDataOption, name, def string) string {
	for _, o := range opts {
		if o.Name == name && o.Type == discordgo.ApplicationCommandOptionString {
			if v := strings.TrimSpace(o.StringValue()); v != "" {
				return v
			}
		}
	}
	return def
}

// truncateForDiscord caps content to Discord's 2000-char message limit.
func truncateForDiscord(s string) string {
	const max = 2000
	if len(s) <= max {
		return s
	}
	return s[:max-3] + "..."
}

func respondText(s *discordgo.Session, i *discordgo.InteractionCreate, content string) {
	if content == "" {
		content = "(no output)"
	}
	_ = s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseChannelMessageWithSource,
		Data: &discordgo.InteractionResponseData{Content: truncateForDiscord(content)},
	})
}

func respondEphemeral(s *discordgo.Session, i *discordgo.InteractionCreate, content string) {
	_ = s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseChannelMessageWithSource,
		Data: &discordgo.InteractionResponseData{Content: truncateForDiscord(content), Flags: discordgo.MessageFlagsEphemeral},
	})
}

// buildReadOnly runs a (state, prices) builder under RLock with live prices.
func (d *DiscordNotifier) buildReadOnly(fn func(*AppState, map[string]float64) string) string {
	if d.ss == nil {
		return "status server not wired"
	}
	prices := d.ss.fetchLiveMarkPrices() // must run without holding mu
	d.ss.mu.RLock()
	defer d.ss.mu.RUnlock()
	return fn(d.ss.state, prices)
}

func (d *DiscordNotifier) buildHealth() string {
	if d.ss == nil {
		return "status server not wired"
	}
	d.ss.mu.RLock()
	lastCycle := d.ss.state.LastCycle
	cycles := d.ss.state.CycleCount
	d.ss.mu.RUnlock()
	return formatHealthResponse(lastCycle, cycles, Version, time.Now())
}

func (d *DiscordNotifier) buildPnL() string {
	if d.ss == nil {
		return "status server not wired"
	}
	lifetime := d.lifetimeStats()
	prices := d.ss.fetchLiveMarkPrices()
	d.ss.mu.RLock()
	defer d.ss.mu.RUnlock()
	return formatPnLResponse(d.ss.state, prices, lifetime)
}

func (d *DiscordNotifier) buildLeaderboard(topN int) string {
	if d.ss == nil || d.cfg == nil {
		return "status server not wired"
	}
	lifetime := d.lifetimeStats()
	prices := d.ss.fetchLiveMarkPrices()
	d.ss.mu.RLock()
	defer d.ss.mu.RUnlock()
	return formatLeaderboardResponse(d.cfg, d.ss.state, prices, lifetime, topN)
}

func (d *DiscordNotifier) buildCircuitBreakers() string {
	if d.ss == nil {
		return "status server not wired"
	}
	d.ss.mu.RLock()
	defer d.ss.mu.RUnlock()
	return formatCircuitBreakersResponse(d.ss.state, time.Now())
}

func (d *DiscordNotifier) buildDeadStrategies() string {
	if d.ss == nil {
		return "status server not wired"
	}
	lifetime := d.lifetimeStats()
	d.ss.mu.RLock()
	defer d.ss.mu.RUnlock()
	return formatDeadStrategiesResponse(d.ss.state, lifetime)
}

func (d *DiscordNotifier) buildCorrelation() string {
	if d.ss == nil {
		return "status server not wired"
	}
	d.ss.mu.RLock()
	defer d.ss.mu.RUnlock()
	return formatCorrelationResponse(d.ss.state.CorrelationSnapshot)
}

// lifetimeStats fetches per-strategy lifetime stats from SQLite (independent of mu).
func (d *DiscordNotifier) lifetimeStats() map[string]LifetimeTradeStats {
	if d.ss == nil || d.ss.stateDB == nil {
		return nil
	}
	stats, err := d.ss.stateDB.LifetimeTradeStatsAll()
	if err != nil {
		return nil
	}
	return stats
}

// runLogs returns the last n journalctl lines for the go-trader unit.
func runLogs(n int) string {
	out, err := exec.Command("journalctl", "-u", "go-trader", "-n", strconv.Itoa(n), "--no-pager").CombinedOutput()
	if err != nil {
		return fmt.Sprintf("journalctl failed: %v\n%s", err, string(out))
	}
	body := strings.TrimSpace(string(out))
	if body == "" {
		return "(no log output)"
	}
	return "```\n" + body + "\n```"
}
```

- [ ] **Step 2: Add the ops handlers (deferred responses)**

Append to `scheduler/discord_commands.go`:

```go
// deferAck acknowledges an interaction so the bot has 15 minutes to follow up.
func deferAck(s *discordgo.Session, i *discordgo.InteractionCreate) {
	_ = s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseDeferredChannelMessageWithSource,
	})
}

// handleRestart restarts the systemd service. Best-effort follow-up: the process
// is replaced by systemd, so the confirmation may not arrive — that is expected.
func (d *DiscordNotifier) handleRestart(s *discordgo.Session, i *discordgo.InteractionCreate) {
	deferAck(s, i)
	_, _ = s.FollowupMessageCreate(i.Interaction, true, &discordgo.WebhookParams{
		Content: "Restarting go-trader service… (this instance will go offline; the new one resumes the cycle)",
	})
	// Fire-and-forget; this process is about to be replaced.
	go func() {
		_ = exec.Command("systemctl", "restart", "go-trader").Run()
	}()
}

// handleBacktest runs run_backtest.py and replies with a summary plus the full report file.
func (d *DiscordNotifier) handleBacktest(s *discordgo.Session, i *discordgo.InteractionCreate, data discordgo.ApplicationCommandInteractionData) {
	strategy := optionString(data.Options, "strategy", "")
	symbol := optionString(data.Options, "symbol", "")
	timeframe := optionString(data.Options, "timeframe", "1h")
	deferAck(s, i)

	args := []string{"--strategy", strategy, "--symbol", symbol, "--timeframe", timeframe, "--mode", "single"}
	stdout, stderr, err := runPythonWithTimeout(shutdownReadOnlyCtx, "backtest/run_backtest.py", args, nil, 5*time.Minute)
	report := string(stdout)
	if err != nil {
		_, _ = s.FollowupMessageCreate(i.Interaction, true, &discordgo.WebhookParams{
			Content: truncateForDiscord(fmt.Sprintf("Backtest failed: %v\n```\n%s\n```", err, strings.TrimSpace(string(stderr)))),
		})
		return
	}
	summary := parseBacktestSummary(report)
	_, _ = s.FollowupMessageCreate(i.Interaction, true, &discordgo.WebhookParams{
		Content: truncateForDiscord(fmt.Sprintf("**Backtest %s on %s (%s)**\n%s", strategy, symbol, timeframe, summary)),
		Files: []*discordgo.File{{
			Name:        "backtest.txt",
			ContentType: "text/plain",
			Reader:      bytes.NewReader([]byte(report)),
		}},
	})
}
```

- [ ] **Step 3: Restore the AddHandler line from Task 8 (if commented) and build**

Ensure `d.session.AddHandler(d.interactionCreate)` is present in `RegisterSlashCommands`.

Run: `cd /Users/richardkuo/Work/go-trader && go -C scheduler build ./...`
Expected: builds with no errors.

- [ ] **Step 4: Run the full package test suite**

Run: `cd /Users/richardkuo/Work/go-trader && go -C scheduler test ./...`
Expected: PASS (all builder/auth/parser tests + existing tests).

- [ ] **Step 5: Commit**

```bash
cd /Users/richardkuo/Work/go-trader
gofmt -w scheduler/discord_commands.go
git add scheduler/discord_commands.go
git commit -m "feat(discord): add interaction dispatch and ops handlers (#212)"
```

---

## Task 10: Wire registration into main.go

**Files:**
- Modify: `scheduler/main.go`

- [ ] **Step 1: Add the wiring after the notifier is built**

In `scheduler/main.go`, immediately after the line that prints `Notification backends: %d active` (right after `notifier, cleanupNotifier := buildNotifierFromConfig(cfg)` / `defer cleanupNotifier()` / the `BackendCount()` print, ~line 278), insert:

```go
	// Attach Discord slash commands (issue #212). Non-fatal: registration
	// failures are logged + DM'd to the owner but never stop the daemon.
	if d := notifier.DiscordBackend(); d != nil {
		if err := d.RegisterSlashCommands(server, cfg); err != nil {
			fmt.Printf("[WARN] Discord slash command registration failed: %v\n", err)
			if notifier.HasOwner() {
				notifier.SendOwnerDM("[slash] registration failed: " + err.Error())
			}
		} else {
			fmt.Println("Discord slash commands registered")
		}
	}
```

(Confirm `server` is the `*StatusServer` variable in scope — it is created at ~line 248 via `NewStatusServer`. If the local variable has a different name, use that.)

- [ ] **Step 2: Build**

Run: `cd /Users/richardkuo/Work/go-trader && go -C scheduler build ./...`
Expected: builds.

- [ ] **Step 3: Smoke test the daemon once**

Run: `cd /Users/richardkuo/Work/go-trader && go -C scheduler build -ldflags "-X main.Version=$(git describe --tags --always --dirty=-mod)" -o /tmp/go-trader-212 . && /tmp/go-trader-212 --config scheduler/config.json --once`
Expected: completes a single cycle without panics. (If Discord is configured, you should see `Discord slash commands registered`; if not, the wiring is a no-op.)

- [ ] **Step 4: Commit**

```bash
cd /Users/richardkuo/Work/go-trader
gofmt -w scheduler/main.go
git add scheduler/main.go
git commit -m "feat(discord): wire slash command registration into daemon startup (#212)"
```

---

## Task 11: Documentation (SKILL.md)

**Files:**
- Modify: `SKILL.md`

- [ ] **Step 1: Add a Discord slash commands section**

Add a new section to `SKILL.md` documenting:

```markdown
## Discord Slash Commands (#212)

The bot registers global slash commands at startup (`scheduler/discord_commands.go`,
wired in `main.go` via `DiscordNotifier.RegisterSlashCommands`). Global registration
covers every guild the bot is in plus DMs; first-time command-shape changes can take up
to ~1h to propagate.

**Setup:** the bot must be invited with the `applications.commands` OAuth scope (in
addition to `bot`) for the commands to appear. No code/config change — re-invite via the
Discord developer portal OAuth2 URL generator.

**Read-only** (usable in a guild OR a DM, by anyone):
`/status`, `/health`, `/positions`, `/pnl`, `/leaderboard [top]`, `/circuit-breakers`,
`/dead-strategies`, `/correlation`, `/logs [n]`. These read live in-process state via the
`StatusServer` (no HTTP round-trip).

> Note: `/logs` surfaces `journalctl` output to anyone in the guild — keep the bot in a
> trusted channel.

**Ops** (owner-only AND DM-only; restricted via command `Contexts: [BotDM]` and re-checked
in the handler by `authorizeCommand`):
- `/restart` — `systemctl restart go-trader` (ACKs, then this instance is replaced).
- `/backtest <strategy> <symbol> [timeframe]` — runs `backtest/run_backtest.py --mode single`
  (5-min timeout via `runPythonWithTimeout` + `shutdownReadOnlyCtx`); replies with a summary
  and attaches the full report as `backtest.txt`.

Auth lives in `authorizeCommand`; command set in `slashCommands()`; pure response builders
(`format*Response`) are unit-tested in `discord_commands_test.go`. Registration failure is
non-fatal (logged + owner DM).
```

- [ ] **Step 2: Commit**

```bash
cd /Users/richardkuo/Work/go-trader
git add SKILL.md
git commit -m "docs: document Discord slash commands (#212)"
```

---

## Task 12: Final verification

**Files:** none (verification only)

- [ ] **Step 1: Full build + test + format check**

Run:
```bash
cd /Users/richardkuo/Work/go-trader
gofmt -l scheduler/        # expect: no files listed
go -C scheduler build ./...
go -C scheduler test ./...
```
Expected: `gofmt -l` prints nothing; build succeeds; all tests PASS.

- [ ] **Step 2: Confirm probe surface unchanged**

`run_backtest.py` is not a startup-probed check script, and no new runtime-required CLI flags were added to check scripts, so `version_probe.go` needs no change. Confirm:

Run: `cd /Users/richardkuo/Work/go-trader && grep -n "run_backtest" scheduler/version_probe.go`
Expected: no matches (nothing to add).

- [ ] **Step 3: Open the PR**

```bash
cd /Users/richardkuo/Work/go-trader
git push -u origin issue-212-discord-slash-commands
gh pr create --title "Discord slash commands for common actions (#212)" --body "$(cat <<'EOF'
Implements issue #212 as real Discord slash commands instead of SKILL.md/OpenClaw skills.

## Scope
- **Read-only** (guild + DM, anyone): `/status`, `/health`, `/positions`, `/pnl`, `/leaderboard`, `/circuit-breakers`, `/dead-strategies`, `/correlation`, `/logs`.
- **Ops** (owner-DM-only): `/restart` (systemctl restart), `/backtest` (run_backtest.py, summary + full report attachment).

## Design
- New `scheduler/discord_commands.go`: command defs, `authorizeCommand` gate, pure response builders, interaction dispatch, global registration.
- Reads live in-process state via `StatusServer`; ops use Discord deferred responses.
- Registration failure is non-fatal. Ops commands restricted via `Contexts: [BotDM]` + handler re-check.

## Tests
- Unit tests for `authorizeCommand`, all `format*Response` builders, and `parseBacktestSummary` (no Discord gateway / subprocess needed).

Closes #212

LLM: Opus 4.8 | high | Harness: Claude Code
EOF
)"
```

Expected: PR created against `main`.

---

## Self-Review notes

- **Spec coverage:** every command in the approved spec's scope table has a builder/handler task (Tasks 3–9), auth model → Task 2, registration/global → Task 8, wiring → Task 10, docs incl. `applications.commands` scope → Task 11. Out-of-scope mutating commands are intentionally excluded.
- **Type consistency:** builder signatures used in Task 9 (`formatStatusResponse(*AppState, map[string]float64)`, `formatPnLResponse(..., lifetime)`, `formatLeaderboardResponse(cfg, state, prices, lifetime, topN)`, `parseBacktestSummary(string)`) match their definitions in Tasks 3–7. `DiscordBackend()`, `RegisterSlashCommands`, `authorizeCommand`, `interactionUserID`, `optionInt/optionString` names are consistent across tasks.
- **Imports:** `discord_commands.go` final import set is `bytes`, `fmt`, `os/exec`, `sort`, `strconv`, `strings`, `time`, `github.com/bwmarrin/discordgo`. Add incrementally per task; `gofmt`/`go build` will catch any unused import (e.g. `bytes`/`os/exec`/`strconv` first appear in Task 9).
- **Concurrency:** every state-reading builder is called under `ss.mu.RLock`; `fetchLiveMarkPrices()` and `LifetimeTradeStatsAll()` are always called *before* taking the lock (both acquire/round-trip independently).
- **Known acceptance:** `/logs` is readable by anyone in the guild (per approved auth model); flagged in SKILL.md.

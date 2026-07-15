package main

import (
	"bytes"
	"fmt"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/bwmarrin/discordgo"
)

// commandPrefix namespaces every Discord slash command away from other bots in
// the same guild (#891). It is the bot's wire name only: slashCommands() builds
// each registered command as commandPrefix+<id>, and interactionCreate strips it
// back to the bare <id> before auth/dispatch, so readOnlyCommandNames,
// opsCommandNames, and the dispatch switch all keep operating on bare command
// IDs. Keep the prefix defined here as the single source of truth.
const commandPrefix = "go-trader-"

// readOnlyCommandNames are usable in a guild or in DMs by anyone.
var readOnlyCommandNames = map[string]bool{
	"status":             true,
	"health":             true,
	"positions":          true,
	"pnl":                true,
	"leaderboard":        true,
	"circuit-breakers":   true,
	"dead-strategies":    true,
	"correlation":        true,
	"closing-strategies": true,
}

// opsCommandNames mutate state, run heavy work, or expose operator-sensitive
// output; restricted to the owner in a DM. `logs` is here (not read-only)
// because journalctl can carry wallet addresses and error payloads. The #868
// mutating set (config/add-strategy/remove-strategy/add-platform/paper-to-live)
// changes the config file, so it gets the same owner-DM-only gate.
var opsCommandNames = map[string]bool{
	"restart":           true,
	"backtest":          true,
	"logs":              true,
	"report-an-issue":   true,
	"config":            true,
	"add-strategy":      true,
	"remove-strategy":   true,
	"add-platform":      true,
	"paper-to-live":     true,
	"apply-regime-gate": true,
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

// sortedAppStateIDs returns the strategy IDs of state in deterministic order.
func sortedAppStateIDs(state *AppState) []string {
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
	for _, id := range sortedAppStateIDs(state) {
		s := state.Strategies[id]
		cash += s.Cash
		value += displayStrategyValue(s, prices)
		posCount += len(s.Positions) + len(s.OptionPositions)
		trades += len(s.TradeHistory)
		if regime == "" && s.Regime != "" {
			regime = s.Regime
		}
	}
	return formatStatusLine(cash, posCount, value, trades, regime)
}

// formatPositionsResponse lists open positions grouped by platform. Call under RLock.
func formatPositionsResponse(state *AppState, prices map[string]float64) string {
	lines := map[string][]string{} // platform -> position lines
	platforms := []string{}
	for _, id := range sortedAppStateIDs(state) {
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
			suffix := ""
			if p.IsHedge {
				suffix = fmt.Sprintf(" ⚖️ AUTO-HEDGE for %s", p.HedgeForSymbol)
			}
			lines[plat] = append(lines[plat], fmt.Sprintf(
				"  %s %s %.4f @ $%.2f (mv $%.2f) [%s]%s", sym, p.Side, p.Quantity, p.AvgCost, mv, id, suffix))
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
func formatPnLResponse(state *AppState, prices map[string]float64) string {
	type agg struct{ value, capital float64 }
	byPlatform := map[string]*agg{}
	platforms := []string{}
	var totVal, totCap float64
	var perStrat []string
	for _, id := range sortedAppStateIDs(state) {
		s := state.Strategies[id]
		pv := displayStrategyValue(s, prices)
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

// formatCircuitBreakersResponse lists open per-strategy breakers + portfolio kill switch. Call under RLock.
func formatCircuitBreakersResponse(state *AppState, now time.Time) string {
	var lines []string
	for _, id := range sortedAppStateIDs(state) {
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
			lines = append(lines, fmt.Sprintf("  %s: pending circuit close (%d venue(s))", id, len(rs.PendingCircuitCloses)))
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
	for _, id := range sortedAppStateIDs(state) {
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
	sort.SliceStable(assets, func(i, j int) bool {
		ci, cj := snap.Assets[assets[i]].ConcentrationPct, snap.Assets[assets[j]].ConcentrationPct
		if ci != cj {
			return ci > cj
		}
		return assets[i] < assets[j]
	})
	for _, a := range assets {
		e := snap.Assets[a]
		sb.WriteString(fmt.Sprintf("  %s: net $%.2f, concentration %.1f%%\n", a, e.NetDeltaUSD, e.ConcentrationPct))
	}
	return strings.TrimRight(sb.String(), "\n")
}

// formatLeaderboardResponse ranks all strategies by PnL% (descending), top N.
// Reuses newLeaderboardEntry for per-strategy metrics. Call under RLock.
func formatLeaderboardResponse(cfg *Config, state *AppState, prices map[string]float64, lifetime map[string]LifetimeTradeStats, topN int) string {
	if topN <= 0 {
		topN = 5
	}
	entries := buildLeaderboardEntries(cfg.Strategies, state, prices, nil, lifetime, cfg.IntervalSeconds)
	if len(entries) == 0 {
		return "No strategies to rank."
	}
	sortLeaderboardEntriesByPnLPct(entries)
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

// dmContext restricts a command to DMs with the bot (used for ops commands).
func dmContext() *[]discordgo.InteractionContextType {
	return &[]discordgo.InteractionContextType{discordgo.InteractionContextBotDM}
}

// slashCommands returns the full set of application commands to register globally.
// slashCommands builds the registered command set. Every top-level Name carries
// commandPrefix (#891) so the bot's commands are namespaced in shared guilds;
// interactionCreate strips the prefix back to the bare ID for auth/dispatch.
// Subcommand and option names are not prefixed — only the top-level command.
func slashCommands() []*discordgo.ApplicationCommand {
	return []*discordgo.ApplicationCommand{
		{Name: commandPrefix + "status", Description: "Live portfolio status (cash, positions, value, regime)"},
		{Name: commandPrefix + "health", Description: "Daemon health: running, last cycle, version"},
		{Name: commandPrefix + "positions", Description: "Open positions across platforms"},
		{Name: commandPrefix + "pnl", Description: "Portfolio P&L (total, per-platform, per-strategy)"},
		{Name: commandPrefix + "leaderboard", Description: "Strategies ranked by P&L%", Options: []*discordgo.ApplicationCommandOption{
			{Type: discordgo.ApplicationCommandOptionInteger, Name: "top", Description: "How many to show (default 5)"},
		}},
		{Name: commandPrefix + "circuit-breakers", Description: "Active circuit breakers and kill-switch state"},
		{Name: commandPrefix + "dead-strategies", Description: "Strategies that have never opened a position"},
		{Name: commandPrefix + "correlation", Description: "Correlation / concentration warnings"},
		{Name: commandPrefix + "closing-strategies", Description: "Registered close evaluators and their config params"},
		{Name: commandPrefix + "logs", Description: "Recent journalctl lines (owner DM only)", Contexts: dmContext(), Options: []*discordgo.ApplicationCommandOption{
			{Type: discordgo.ApplicationCommandOptionInteger, Name: "n", Description: "Number of lines (default 50, max 200)"},
		}},
		{Name: commandPrefix + "restart", Description: "Restart the go-trader service (owner DM only)", Contexts: dmContext()},
		{Name: commandPrefix + "report-an-issue", Description: "File a GitHub issue (owner DM only)", Contexts: dmContext(), Options: []*discordgo.ApplicationCommandOption{
			{Type: discordgo.ApplicationCommandOptionString, Name: "title", Description: "Issue title", Required: true},
			{Type: discordgo.ApplicationCommandOptionString, Name: "body", Description: "Issue description", Required: true},
			{Type: discordgo.ApplicationCommandOptionString, Name: "label", Description: "Optional label (applied if it exists on the repo)"},
		}},
		{Name: commandPrefix + "backtest", Description: "Run a single backtest (owner DM only)", Contexts: dmContext(), Options: []*discordgo.ApplicationCommandOption{
			{Type: discordgo.ApplicationCommandOptionString, Name: "strategy", Description: "Strategy name", Required: true},
			{Type: discordgo.ApplicationCommandOptionString, Name: "symbol", Description: "Symbol, e.g. BTC/USDT", Required: true},
			{Type: discordgo.ApplicationCommandOptionString, Name: "timeframe", Description: "Timeframe (default 1h)"},
		}},
		// Mutating ops — owner-DM-only (#868). Restricted by Contexts; re-checked in the handler.
		{Name: commandPrefix + "config", Description: "Show or change configuration (owner DM only)", Contexts: dmContext(), Options: []*discordgo.ApplicationCommandOption{
			{Type: discordgo.ApplicationCommandOptionSubCommand, Name: "show", Description: "Show the current config (secrets redacted)"},
			{Type: discordgo.ApplicationCommandOptionSubCommand, Name: "set", Description: "Set a config key", Options: []*discordgo.ApplicationCommandOption{
				{Type: discordgo.ApplicationCommandOptionString, Name: "key", Description: "e.g. interval_seconds or strategies.<id>.leverage", Required: true},
				{Type: discordgo.ApplicationCommandOptionString, Name: "value", Description: "New value", Required: true},
			}},
		}},
		{Name: commandPrefix + "add-strategy", Description: "Add a strategy to the config (owner DM only)", Contexts: dmContext(), Options: []*discordgo.ApplicationCommandOption{
			{Type: discordgo.ApplicationCommandOptionString, Name: "name", Description: "Strategy name, e.g. momentum", Required: true},
			{Type: discordgo.ApplicationCommandOptionString, Name: "platform", Description: "hyperliquid or binanceus", Required: true},
			{Type: discordgo.ApplicationCommandOptionString, Name: "asset", Description: "Ticker, e.g. BTC", Required: true},
		}},
		{Name: commandPrefix + "remove-strategy", Description: "Remove a strategy from the config (owner DM only)", Contexts: dmContext(), Options: []*discordgo.ApplicationCommandOption{
			{Type: discordgo.ApplicationCommandOptionString, Name: "id", Description: "Strategy ID to remove", Required: true},
		}},
		{Name: commandPrefix + "add-platform", Description: "Guided platform setup instructions (owner DM only)", Contexts: dmContext(), Options: []*discordgo.ApplicationCommandOption{
			{Type: discordgo.ApplicationCommandOptionString, Name: "name", Description: "Platform name, e.g. hyperliquid", Required: true},
		}},
		{Name: commandPrefix + "paper-to-live", Description: "Switch a strategy from paper to live (owner DM only)", Contexts: dmContext(), Options: []*discordgo.ApplicationCommandOption{
			{Type: discordgo.ApplicationCommandOptionString, Name: "strategy", Description: "Strategy ID to switch to live", Required: true},
		}},
		{Name: commandPrefix + "apply-regime-gate", Description: "Interactively wire a regime entry-gate onto a strategy (owner DM only)", Contexts: dmContext(), Options: []*discordgo.ApplicationCommandOption{
			{Type: discordgo.ApplicationCommandOptionString, Name: "gate", Description: "Gate preset (default comp_up_clean_p21)"},
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

// interactionCreate is the gateway handler for slash commands.
func (d *DiscordNotifier) interactionCreate(s *discordgo.Session, i *discordgo.InteractionCreate) {
	if i.Type != discordgo.InteractionApplicationCommand {
		return
	}
	data := i.ApplicationCommandData()
	// Commands register under commandPrefix (#891); strip it to the bare command
	// ID so auth + dispatch below operate on the unprefixed names.
	name := strings.TrimPrefix(data.Name, commandPrefix)
	ok, reason := authorizeCommand(name, interactionUserID(i), i.GuildID, d.ownerID)
	if !ok {
		respondEphemeral(s, i, reason)
		return
	}
	switch name {
	// Mark-fetching read-only commands: fetchLiveMarkPrices spawns a Python
	// subprocess + venue HTTP, which can exceed Discord's 3s deadline — so ACK
	// first (deferred), then deliver the built response via a follow-up.
	case "status":
		d.respondReadOnlyDeferred(s, i, d.buildDiscordStatus)
	case "positions":
		d.respondReadOnlyDeferred(s, i, func() string { return d.buildReadOnly(formatPositionsResponse) })
	case "pnl":
		d.respondReadOnlyDeferred(s, i, d.buildPnL)
	case "leaderboard":
		top := optionInt(data.Options, "top", 5)
		d.respondReadOnlyDeferred(s, i, func() string { return d.buildLeaderboard(top) })
	// Fast read-only commands (no live-mark fetch): answer inline within 3s.
	case "health":
		d.respondReadOnlyInline(s, i, d.buildHealth())
	case "circuit-breakers":
		d.respondReadOnlyInline(s, i, d.buildCircuitBreakers())
	case "dead-strategies":
		d.respondReadOnlyInline(s, i, d.buildDeadStrategies())
	case "correlation":
		d.respondReadOnlyInline(s, i, d.buildCorrelation())
	case "closing-strategies":
		d.handleClosingStrategies(s, i)
	// Ops (owner DM only).
	case "logs":
		respondText(s, i, runLogs(optionInt(data.Options, "n", 50)))
	case "restart":
		d.handleRestart(s, i)
	case "backtest":
		d.handleBacktest(s, i, data)
	case "report-an-issue":
		d.handleReport(s, i, data)
	// Mutating ops (#868) — owner DM only.
	case "config":
		sub, subOpts := subcommandOptions(data)
		switch sub {
		case "show":
			d.handleConfigShow(s, i)
		case "set":
			d.handleConfigSet(s, i, subOpts)
		default:
			respondEphemeral(s, i, "usage: /go-trader-config show | /go-trader-config set <key> <value>")
		}
	case "add-strategy":
		d.handleAddStrategy(s, i, data.Options)
	case "remove-strategy":
		d.handleRemoveStrategy(s, i, data.Options)
	case "add-platform":
		d.handleAddPlatform(s, i, data.Options)
	case "paper-to-live":
		d.handlePaperToLive(s, i, data.Options)
	case "apply-regime-gate":
		d.handleApplyRegimeGate(s, i, data.Options)
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

// truncateForDiscord caps content to Discord's 2000-char message limit, cutting
// on a rune boundary so multibyte glyphs (the 🛑/⚠️ emoji in some replies) are
// never split into an invalid trailing byte.
func truncateForDiscord(s string) string {
	const max = 2000
	if len(s) <= max {
		return s
	}
	cut := max - 3 // reserve 3 bytes for the "..." ellipsis
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + "..."
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

// readOnlyReplyFlags returns MessageFlagsEphemeral when discord.ephemeral_replies
// is set, else 0 (public in-channel). Applies to read-only command replies only;
// ops replies are DM-only, where the ephemeral flag has no effect.
func (d *DiscordNotifier) readOnlyReplyFlags() discordgo.MessageFlags {
	if d.cfg != nil && d.cfg.Discord.EphemeralReplies {
		return discordgo.MessageFlagsEphemeral
	}
	return 0
}

// respondReadOnlyInline answers a fast read-only command immediately (no live-mark
// fetch), honoring the ephemeral-replies config flag.
func (d *DiscordNotifier) respondReadOnlyInline(s *discordgo.Session, i *discordgo.InteractionCreate, content string) {
	if content == "" {
		content = "(no output)"
	}
	_ = s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseChannelMessageWithSource,
		Data: &discordgo.InteractionResponseData{Content: truncateForDiscord(content), Flags: d.readOnlyReplyFlags()},
	})
}

// respondReadOnlyDeferred ACKs within Discord's 3s window, then runs the (slow,
// live-mark-fetching) builder and delivers the result via a follow-up. Without
// this, /status, /positions, /pnl, and /leaderboard would miss the deadline
// because fetchLiveMarkPrices spawns a Python subprocess and venue HTTP calls.
func (d *DiscordNotifier) respondReadOnlyDeferred(s *discordgo.Session, i *discordgo.InteractionCreate, build func() string) {
	flags := d.readOnlyReplyFlags()
	_ = s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseDeferredChannelMessageWithSource,
		Data: &discordgo.InteractionResponseData{Flags: flags},
	})
	content := build()
	if content == "" {
		content = "(no output)"
	}
	_, _ = s.FollowupMessageCreate(i.Interaction, true, &discordgo.WebhookParams{
		Content: truncateForDiscord(content),
		Flags:   flags,
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

// buildDiscordStatus is the /status slash-command builder: portfolio summary plus
// any uncertified/expired regime_directional_policy notes (#1157).
func (d *DiscordNotifier) buildDiscordStatus() string {
	if d.ss == nil || d.cfg == nil {
		return "status server not wired"
	}
	prices := d.ss.fetchLiveMarkPrices()
	d.ss.mu.RLock()
	defer d.ss.mu.RUnlock()
	base := formatStatusResponse(d.ss.state, prices)
	base += pausedStrategiesNote(d.cfg.Strategies)
	base += dailyLossStatusNote(d.cfg.PortfolioRisk, d.ss.state.Strategies, time.Now())
	base += exposureCapStatusNote(d.cfg.PortfolioRisk, d.ss.state, d.cfg.Strategies, prices)
	base += recentRegimeTransitionsNote(d.ss.stateDB, d.cfg.Regime, time.Now())
	if note := directionalCertOperatorNotes(d.cfg.Strategies, d.cfg.Regime); note != "" {
		return base + note
	}
	return base
}

// pausedStrategiesNote lists paused strategies (#1150) for /status. Empty
// string when none are paused. IDs are sorted for stable operator output.
func pausedStrategiesNote(strategies []StrategyConfig) string {
	var paused []string
	for _, sc := range strategies {
		if sc.Paused {
			paused = append(paused, sc.ID)
		}
	}
	if len(paused) == 0 {
		return ""
	}
	sort.Strings(paused)
	return fmt.Sprintf("\n⏸️ paused: %s", strings.Join(paused, ", "))
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
	prices := d.ss.fetchLiveMarkPrices()
	d.ss.mu.RLock()
	defer d.ss.mu.RUnlock()
	return formatPnLResponse(d.ss.state, prices)
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

// handleClosingStrategies answers /closing-strategies (#1203) with the full
// close-evaluator catalog. Deferred + multi-followup because the first call
// after startup spawns the close-registry subprocess (cached after that, see
// fetchCloseRegistryCatalog) and the catalog may span more than one Discord
// message.
func (d *DiscordNotifier) handleClosingStrategies(s *discordgo.Session, i *discordgo.InteractionCreate) {
	flags := d.readOnlyReplyFlags()
	_ = s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseDeferredChannelMessageWithSource,
		Data: &discordgo.InteractionResponseData{Flags: flags},
	})
	entries, err := fetchCloseRegistryCatalog()
	if err != nil {
		_, _ = s.FollowupMessageCreate(i.Interaction, true, &discordgo.WebhookParams{
			Content: truncateForDiscord(fmt.Sprintf("closing-strategies: %v", err)),
			Flags:   flags,
		})
		return
	}
	// cfg.UserDefaults is a hot-reloadable field mutated under d.ss.mu.Lock()
	// on SIGHUP (config_reload.go); hold the read lock across the format call
	// (not across the subprocess above, which can run up to scriptTimeout).
	var pages []string
	if d.ss == nil {
		pages = formatClosingStrategiesResponse(d.cfg, entries)
	} else {
		d.ss.mu.RLock()
		pages = formatClosingStrategiesResponse(d.cfg, entries)
		d.ss.mu.RUnlock()
	}
	for _, page := range pages {
		_, _ = s.FollowupMessageCreate(i.Interaction, true, &discordgo.WebhookParams{
			Content: truncateForDiscord(page),
			Flags:   flags,
		})
	}
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
		_ = restartSelf()
	}()
}

// handleBacktest runs run_backtest.py and replies with a summary plus the full report file.
func (d *DiscordNotifier) handleBacktest(s *discordgo.Session, i *discordgo.InteractionCreate, data discordgo.ApplicationCommandInteractionData) {
	strategy := optionString(data.Options, "strategy", "")
	symbol := optionString(data.Options, "symbol", "")
	timeframe := optionString(data.Options, "timeframe", "1h")
	deferAck(s, i)

	args := []string{"--strategy", strategy, "--symbol", symbol, "--timeframe", timeframe, "--mode", "single"}
	// Holds one of the 4 pythonSemaphore slots (executor.go) for up to 5 min —
	// i.e. 25% of the Python concurrency the trading loop shares. Acceptable
	// because /backtest is owner-gated and can't be spammed by guild members.
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

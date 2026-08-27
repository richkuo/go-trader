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

const commandPrefix = "go-trader-"

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

var opsCommandNames = map[string]bool{
	"restart":              true,
	"backtest":             true,
	"logs":                 true,
	"report-an-issue":      true,
	"config":               true,
	"add-strategy":         true,
	"remove-strategy":      true,
	"add-platform":         true,
	"paper-to-live":        true,
	"apply-regime-gate":    true,
	"clear-cash-reconcile": true,
}

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

func interactionUserID(i *discordgo.InteractionCreate) string {
	if i.Member != nil && i.Member.User != nil {
		return i.Member.User.ID
	}
	if i.User != nil {
		return i.User.ID
	}
	return ""
}

func sortedAppStateIDs(state *AppState) []string {
	ids := make([]string, 0, len(state.Strategies))
	for id := range state.Strategies {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

func strategyPlatformLabel(s *StrategyState) string {
	if s.Platform != "" {
		return s.Platform
	}
	return s.Type
}

func positionMultiplier(p *Position) float64 {
	if p.Multiplier > 0 {
		return p.Multiplier
	}
	return 1
}

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

func formatStatusResponse(state *AppState, prices map[string]float64) string {
	var cash float64
	posCount, trades := 0, 0
	regime := ""
	var reconcileIDs []string
	for _, id := range sortedAppStateIDs(state) {
		s := state.Strategies[id]
		cash += s.Cash
		posCount += len(s.Positions) + len(s.OptionPositions)
		trades += len(s.TradeHistory)
		if regime == "" && s.Regime != "" {
			regime = s.Regime
		}
		if s.CashReconcileRequired {
			reconcileIDs = append(reconcileIDs, id)
		}
	}
	value := latestDisplayTotal(state, prices)
	line := formatStatusLine(cash, posCount, value, trades, regime)
	if len(state.LatestSharedWalletBalances) > 0 {
		line += "\nℹ️ shared-wallet equity is counted once in value; cash remains the virtual strategy-book sum."
	}
	if len(reconcileIDs) == 0 {
		return line
	}
	return line + "\n**CASH RECONCILE REQUIRED:** " + strings.Join(reconcileIDs, ", ")
}

func formatPositionsResponse(state *AppState, prices map[string]float64) string {
	lines := map[string][]string{}
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

func formatPnLResponse(state *AppState, prices map[string]float64) string {
	type agg struct {
		pnl, capital float64
		includesPool bool
	}
	byPlatform := map[string]*agg{}
	platforms := []string{}
	var totPnL, totCap float64
	var totalIncludesPool bool
	var perStrat []string
	for _, id := range sortedAppStateIDs(state) {
		s := state.Strategies[id]
		pv := displayStrategyValue(s, prices)
		cap := s.InitialCapital
		poolBudget := s.SharedWalletPoolBudget || s.SharedWalletPerformanceOnly
		if poolBudget {
			cap = 0
		}
		pnl := pv - cap
		totPnL += pnl
		totCap += cap
		totalIncludesPool = totalIncludesPool || poolBudget
		plat := strategyPlatformLabel(s)
		if byPlatform[plat] == nil {
			byPlatform[plat] = &agg{}
			platforms = append(platforms, plat)
		}
		byPlatform[plat].pnl += pnl
		byPlatform[plat].capital += cap
		byPlatform[plat].includesPool = byPlatform[plat].includesPool || poolBudget
		perStrat = append(perStrat, fmt.Sprintf(
			"  %s: $%+.2f (%s)", id, pnl, formatPnLPercent(pnl, cap, poolBudget),
		))
	}
	sort.Strings(platforms)
	var sb strings.Builder
	sb.WriteString("**P&L**\n")
	totalDisplayValue := latestDisplayTotal(state, prices)
	sb.WriteString(fmt.Sprintf(
		"Total: $%+.2f (%s) — value $%.2f / capital $%.2f\n",
		totPnL, formatPnLPercent(totPnL, totCap, totalIncludesPool), totalDisplayValue, totCap,
	))
	sb.WriteString("__By platform__\n")
	for _, plat := range platforms {
		a := byPlatform[plat]
		sb.WriteString(fmt.Sprintf(
			"  %s: $%+.2f (%s)\n",
			plat, a.pnl, formatPnLPercent(a.pnl, a.capital, a.includesPool),
		))
	}
	sb.WriteString("__By strategy__\n")
	sb.WriteString(strings.Join(perStrat, "\n"))
	return strings.TrimRight(sb.String(), "\n")
}

func formatPnLPercent(pnl, capital float64, includesPool bool) string {
	if includesPool {
		return "—"
	}
	pct := 0.0
	if capital > 0 {
		pct = pnl / capital * 100
	}
	return fmt.Sprintf("%+.2f%%", pct)
}

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
	if !state.PortfolioRisk.KillSwitchActive && !state.PortfolioRisk.UntrustedOverLimitSince.IsZero() {
		sb.WriteString(fmt.Sprintf("⚠️ Portfolio latch DEFERRED: equity drawdown %.2f%% is over the limit on an untrusted total (since %s); escalates %s unless a trusted measurement lands first\n",
			state.PortfolioRisk.CurrentDrawdownPct,
			state.PortfolioRisk.UntrustedOverLimitSince.Format("2006-01-02 15:04 UTC"),
			state.PortfolioRisk.UntrustedOverLimitSince.Add(untrustedEquityLatchDeferral).Format("2006-01-02 15:04 UTC")))
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
	sb.WriteString("**Leaderboard (by PnL%; pool rows unranked)**\n")
	rank := 0
	for i := 0; i < topN; i++ {
		e := entries[i]
		if e.PoolBudget {
			sb.WriteString(fmt.Sprintf("  — %s — pool net $%+.2f (PnL%% unavailable)\n", e.ID, e.PnL))
			continue
		}
		rank++
		sb.WriteString(fmt.Sprintf("  %d. %s — %+.2f%% ($%+.2f)\n", rank, e.ID, e.PnLPct, e.PnL))
	}
	return strings.TrimRight(sb.String(), "\n")
}

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

func dmContext() *[]discordgo.InteractionContextType {
	return &[]discordgo.InteractionContextType{discordgo.InteractionContextBotDM}
}

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
		{Name: commandPrefix + "clear-cash-reconcile", Description: "Clear CashReconcileRequired after books match the venue (owner DM only)", Contexts: dmContext(), Options: []*discordgo.ApplicationCommandOption{
			{Type: discordgo.ApplicationCommandOptionString, Name: "strategy", Description: "Strategy ID whose cash-reconcile latch to clear", Required: true},
		}},
	}
}

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

func (d *DiscordNotifier) interactionCreate(s *discordgo.Session, i *discordgo.InteractionCreate) {
	if i.Type != discordgo.InteractionApplicationCommand {
		return
	}
	data := i.ApplicationCommandData()
	name := strings.TrimPrefix(data.Name, commandPrefix)
	ok, reason := authorizeCommand(name, interactionUserID(i), i.GuildID, d.ownerID)
	if !ok {
		respondEphemeral(s, i, reason)
		return
	}
	switch name {
	case "status":
		d.respondReadOnlyDeferred(s, i, d.buildDiscordStatus)
	case "positions":
		d.respondReadOnlyDeferred(s, i, func() string { return d.buildReadOnly(formatPositionsResponse) })
	case "pnl":
		d.respondReadOnlyDeferred(s, i, d.buildPnL)
	case "leaderboard":
		top := optionInt(data.Options, "top", 5)
		d.respondReadOnlyDeferred(s, i, func() string { return d.buildLeaderboard(top) })
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
	case "logs":
		respondText(s, i, runLogs(optionInt(data.Options, "n", 50)))
	case "restart":
		d.handleRestart(s, i)
	case "backtest":
		d.handleBacktest(s, i, data)
	case "report-an-issue":
		d.handleReport(s, i, data)
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
	case "clear-cash-reconcile":
		d.handleClearCashReconcile(s, i, data.Options)
	default:
		respondEphemeral(s, i, "unknown command")
	}
}

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

func truncateForDiscord(s string) string {
	const max = 2000
	if len(s) <= max {
		return s
	}
	cut := max - 3
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

func (d *DiscordNotifier) readOnlyReplyFlags() discordgo.MessageFlags {
	if d.cfg != nil && d.cfg.Discord.EphemeralReplies {
		return discordgo.MessageFlagsEphemeral
	}
	return 0
}

func (d *DiscordNotifier) respondReadOnlyInline(s *discordgo.Session, i *discordgo.InteractionCreate, content string) {
	if content == "" {
		content = "(no output)"
	}
	_ = s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseChannelMessageWithSource,
		Data: &discordgo.InteractionResponseData{Content: truncateForDiscord(content), Flags: d.readOnlyReplyFlags()},
	})
}

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

func (d *DiscordNotifier) buildReadOnly(fn func(*AppState, map[string]float64) string) string {
	if d.ss == nil {
		return "status server not wired"
	}
	prices := d.ss.fetchLiveMarkPrices()
	d.ss.mu.RLock()
	defer d.ss.mu.RUnlock()
	return fn(d.ss.state, prices)
}

func (d *DiscordNotifier) buildDiscordStatus() string {
	if d.ss == nil || d.cfg == nil {
		return "status server not wired"
	}
	prices := d.ss.fetchLiveMarkPrices()
	d.ss.mu.RLock()
	defer d.ss.mu.RUnlock()
	base := formatStatusResponse(d.ss.state, prices)
	base += pausedStrategiesNote(d.cfg.Strategies)
	base += hedgeStatusNote(d.cfg.Strategies, d.ss.state)
	base += dailyLossStatusNote(d.cfg.PortfolioRisk, d.ss.state.Strategies, d.cfg.Strategies, time.Now())
	base += exposureCapStatusNote(d.cfg.PortfolioRisk, d.ss.state, d.cfg.Strategies, prices)
	base += recentRegimeTransitionsNote(d.ss.stateDB, d.cfg.Regime, time.Now())
	if note := directionalCertOperatorNotes(d.cfg.Strategies, d.cfg.Regime); note != "" {
		return base + note
	}
	return base
}

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

func hedgeStatusNote(strategies []StrategyConfig, state *AppState) string {
	type row struct {
		id   string
		line string
	}
	var rows []row
	for _, sc := range strategies {
		if !HedgeEnabled(sc) {
			continue
		}
		var ss *StrategyState
		if state != nil {
			ss = state.Strategies[sc.ID]
		}
		rows = append(rows, row{id: sc.ID, line: hedgeStatusLine(sc, ss)})
	}
	if len(rows) == 0 {
		return ""
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].id < rows[j].id })
	var b strings.Builder
	b.WriteString("\n🛡️ hedge legs (auto-managed, coupled to their primary):")
	for _, r := range rows {
		fmt.Fprintf(&b, "\n  • %s: %s", r.id, r.line)
	}
	return b.String()
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

func deferAck(s *discordgo.Session, i *discordgo.InteractionCreate) {
	_ = s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseDeferredChannelMessageWithSource,
	})
}

func (d *DiscordNotifier) handleRestart(s *discordgo.Session, i *discordgo.InteractionCreate) {
	deferAck(s, i)
	_, _ = s.FollowupMessageCreate(i.Interaction, true, &discordgo.WebhookParams{
		Content: "Restarting go-trader service… (this instance will go offline; the new one resumes the cycle)",
	})
	go func() {
		_ = restartSelf()
	}()
}

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

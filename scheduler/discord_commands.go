package main

import (
	"fmt"
	"sort"
	"strings"
	"time"

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
		value += PortfolioValue(s, prices)
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
	for _, id := range sortedAppStateIDs(state) {
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

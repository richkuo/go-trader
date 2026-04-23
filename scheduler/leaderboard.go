package main

import (
	"fmt"
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// LeaderboardEntry holds computed PnL data for one strategy.
type LeaderboardEntry struct {
	ID      string  `json:"id"`
	Type    string  `json:"type"`
	Value   float64 `json:"value"`
	Capital float64 `json:"capital"`
	PnL     float64 `json:"pnl"`
	PnLPct  float64 `json:"pnl_pct"`
	Trades  int     `json:"trades"`
	Sharpe  float64 `json:"sharpe"` // #397 — annualized Sharpe; 0 = undefined/no data. Kept in the serialized form (no omitempty) so consumers can distinguish "present but zero" from "omitted".
}

// leaderboardTopN returns the configured top-N count, defaulting to 5 when unset.
func leaderboardTopN(cfg *Config) int {
	if cfg.Discord.LeaderboardTopN > 0 {
		return cfg.Discord.LeaderboardTopN
	}
	return 5
}

// BuildLeaderboardMessages computes the aggregate top-N / bottom-N leaderboard
// messages from the current state. Returned map keys are "top" and "bottom";
// the actual entry count is controlled by Discord.LeaderboardTopN. Returns nil
// if no strategies have state. Issue #313 moved this to an on-demand compute
// (previously written to leaderboard.json every cycle).
func BuildLeaderboardMessages(cfg *Config, state *AppState, prices map[string]float64, sharpeByStrategy map[string]float64) map[string]string {
	var allEntries []LeaderboardEntry

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
			pnlPct = (pnl / initCap) * 100
		}

		allEntries = append(allEntries, LeaderboardEntry{
			ID:      sc.ID,
			Type:    sc.Type,
			Value:   pv,
			Capital: initCap,
			PnL:     pnl,
			PnLPct:  pnlPct,
			Trades:  len(ss.TradeHistory),
			Sharpe:  sharpeByStrategy[sc.ID],
		})
	}

	if len(allEntries) == 0 {
		return nil
	}

	topN := leaderboardTopN(cfg)
	return map[string]string{
		"top":    formatAllTimeMessage("🏆", "Top All-Time Performers", allEntries, true, topN),
		"bottom": formatAllTimeMessage("💀", "Bottom All-Time Performers", allEntries, false, topN),
	}
}

// formatLeaderboardMessage formats a leaderboard message for a sorted slice of entries.
// Used by formatAllTimeMessage (top/bottom) and BuildLeaderboardSummary (per-platform summaries).
// Callers are responsible for passing a positive topN (see leaderboardTopN).
func formatLeaderboardMessage(icon, title string, entries []LeaderboardEntry, showType bool, topN int) string {
	// Sort by PnL% descending.
	sort.Slice(entries, func(i, j int) bool {
		return entries[i].PnLPct > entries[j].PnLPct
	})

	dateStr := time.Now().Format("January 2, 2006")

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("%s **%s**\n", icon, title))
	sb.WriteString(fmt.Sprintf("Daily Report | %s\n", dateStr))

	// Totals across ALL strategies in this category.
	var totalValue, totalCapital float64
	totalTrades := 0
	winning, losing, flat := 0, 0, 0
	for _, e := range entries {
		totalValue += e.Value
		totalCapital += e.Capital
		totalTrades += e.Trades
		if e.PnLPct > 0 {
			winning++
		} else if e.PnLPct < 0 {
			losing++
		} else {
			flat++
		}
	}
	totalPnl := totalValue - totalCapital
	totalPnlPct := 0.0
	if totalCapital > 0 {
		totalPnlPct = (totalPnl / totalCapital) * 100
	}

	// Show top N entries.
	top := entries
	if len(top) > topN {
		top = top[:topN]
	}

	const sep = "--------------------------------------------------------------------------------"
	sb.WriteString("\n```\n")
	if showType {
		sb.WriteString(fmt.Sprintf("%-22s %-6s %10s %10s %7s %8s %7s\n", "Strategy", "Type", "Value", "PnL", "PnL%", "Trades", "Sharpe"))
	} else {
		sb.WriteString(fmt.Sprintf("%-26s %10s %10s %7s %8s %7s\n", "Strategy", "Value", "PnL", "PnL%", "Trades", "Sharpe"))
	}
	sb.WriteString(sep + "\n")

	for _, e := range top {
		label := e.ID
		valStr := "$" + fmtComma(e.Value)
		pnlStr := fmtSignedDollar(e.PnL)
		pctStr := fmtSignedPct(e.PnLPct)
		tradesStr := fmt.Sprintf("%d", e.Trades)
		sharpeStr := fmtSharpe(e.Sharpe)
		if showType {
			if len(label) > 22 {
				label = label[:22]
			}
			sb.WriteString(fmt.Sprintf("%-22s %-6s %10s %10s %7s %8s %7s\n", label, e.Type, valStr, pnlStr, pctStr, tradesStr, sharpeStr))
		} else {
			if len(label) > 26 {
				label = label[:26]
			}
			sb.WriteString(fmt.Sprintf("%-26s %10s %10s %7s %8s %7s\n", label, valStr, pnlStr, pctStr, tradesStr, sharpeStr))
		}
	}
	sb.WriteString(sep + "\n")

	totalLabel := fmt.Sprintf("TOTAL (%d strategies)", len(entries))
	totValStr := "$" + fmtComma(totalValue)
	totPnlStr := fmtSignedDollar(totalPnl)
	totPctStr := fmtSignedPct(totalPnlPct)
	totTradesStr := fmt.Sprintf("%d", totalTrades)
	if showType {
		sb.WriteString(fmt.Sprintf("%-22s %-6s %10s %10s %7s %8s %7s\n", totalLabel, "", totValStr, totPnlStr, totPctStr, totTradesStr, ""))
	} else {
		sb.WriteString(fmt.Sprintf("%-26s %10s %10s %7s %8s %7s\n", totalLabel, totValStr, totPnlStr, totPctStr, totTradesStr, ""))
	}
	sb.WriteString("```\n")
	sb.WriteString(fmt.Sprintf("🟢 %d winning · 🔴 %d losing · ⚪ %d flat\n", winning, losing, flat))

	return sb.String()
}

// formatAllTimeMessage formats the top/bottom all-time leaderboard.
// isTop controls sort direction; topN controls how many entries to show.
func formatAllTimeMessage(icon, title string, entries []LeaderboardEntry, isTop bool, topN int) string {
	// Sort: top = descending PnL%, bottom = ascending PnL%.
	sorted := make([]LeaderboardEntry, len(entries))
	copy(sorted, entries)
	if isTop {
		sort.Slice(sorted, func(i, j int) bool {
			return sorted[i].PnLPct > sorted[j].PnLPct
		})
	} else {
		sort.Slice(sorted, func(i, j int) bool {
			return sorted[i].PnLPct < sorted[j].PnLPct
		})
	}

	n := topN
	if len(sorted) < n {
		n = len(sorted)
	}
	top := sorted[:n]

	return formatLeaderboardMessage(icon, title, top, true, n)
}

// PostLeaderboard computes the leaderboard on-demand and posts all messages to
// the configured notification backends. Issue #313 moved this from reading a
// pre-computed leaderboard.json (which was rewritten every cycle) to computing
// fresh data at post time — the data is only used by the daily cron post and
// the --leaderboard flag, so there is no benefit to pre-computation.
func PostLeaderboard(cfg *Config, state *AppState, prices map[string]float64, sharpeByStrategy map[string]float64, notifier *MultiNotifier) error {
	return postLeaderboardMessages(BuildLeaderboardMessages(cfg, state, prices, sharpeByStrategy), notifier)
}

// postLeaderboardMessages posts pre-built leaderboard messages. Separated from
// PostLeaderboard so callers that need to build under a state lock and post
// outside it (e.g. the scheduler cycle) can split the two phases.
func postLeaderboardMessages(messages map[string]string, notifier *MultiNotifier) error {
	if len(messages) == 0 {
		return fmt.Errorf("no strategies to leaderboard")
	}

	// Post aggregate top/bottom messages with 1s delay between them.
	// Routing is decided per-backend inside the notifier: backends with a
	// dedicated leaderboard channel route there, others fall back to broadcast
	// across all configured channels. Issue #310.
	order := []string{"top", "bottom"}
	first := true
	for _, key := range order {
		msg, ok := messages[key]
		if !ok || msg == "" {
			continue
		}
		if !first {
			time.Sleep(1 * time.Second)
		}
		first = false

		notifier.PostLeaderboardBroadcast(msg)
		fmt.Println(msg)
	}

	fmt.Printf("Leaderboard posted (computed at %s)\n", time.Now().UTC().Format(time.RFC3339))
	return nil
}

// titleCase capitalizes the first rune of s and lowercases the rest.
// Rune-aware so non-ASCII platform names don't produce mojibake.
func titleCase(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	if s == "" {
		return s
	}
	r, size := utf8.DecodeRuneInString(s)
	return string(unicode.ToUpper(r)) + s[size:]
}

// platformIcon returns a short icon for a platform; used by leaderboard summaries.
func platformIcon(platform string) string {
	switch strings.ToLower(platform) {
	case "hyperliquid":
		return "⚡"
	case "deribit", "ibkr":
		return "🎯"
	case "topstep":
		return "🏦"
	case "binanceus", "okx", "robinhood", "luno":
		return "📈"
	default:
		return "📊"
	}
}

// BuildLeaderboardSummary constructs a leaderboard message for a single
// LeaderboardSummaryConfig entry: strategies filtered by platform (and
// optionally ticker) sorted by PnL% descending, truncated to TopN. Returns ""
// if no strategies match — caller should skip posting in that case.
// Issue #308.
func BuildLeaderboardSummary(lc LeaderboardSummaryConfig, cfg *Config, state *AppState, prices map[string]float64, sharpeByStrategy map[string]float64) string {
	topN := lc.TopN
	if topN <= 0 {
		topN = 5
	}
	tickerFilter := strings.ToUpper(strings.TrimSpace(lc.Ticker))

	var entries []LeaderboardEntry
	for _, sc := range cfg.Strategies {
		if !strings.EqualFold(sc.Platform, lc.Platform) {
			continue
		}
		if tickerFilter != "" && extractAsset(sc) != tickerFilter {
			continue
		}
		ss := state.Strategies[sc.ID]
		if ss == nil {
			continue
		}
		pv := PortfolioValue(ss, prices)
		initCap := EffectiveInitialCapital(sc, ss)
		pnl := pv - initCap
		pnlPct := 0.0
		if initCap > 0 {
			pnlPct = (pnl / initCap) * 100
		}
		entries = append(entries, LeaderboardEntry{
			ID:      sc.ID,
			Type:    sc.Type,
			Value:   pv,
			Capital: initCap,
			PnL:     pnl,
			PnLPct:  pnlPct,
			Trades:  len(ss.TradeHistory),
			Sharpe:  sharpeByStrategy[sc.ID],
		})
	}

	if len(entries) == 0 {
		return ""
	}

	sort.Slice(entries, func(i, j int) bool {
		return entries[i].PnLPct > entries[j].PnLPct
	})
	n := topN
	if len(entries) < n {
		n = len(entries)
	}

	platformTitle := titleCase(lc.Platform)
	title := fmt.Sprintf("%s Top %d", platformTitle, n)
	if tickerFilter != "" {
		title = fmt.Sprintf("%s %s Top %d", platformTitle, tickerFilter, n)
	}
	return formatLeaderboardMessage(platformIcon(lc.Platform), title, entries[:n], false, n)
}

// fmtSignedDollar formats a dollar value with +/- prefix.
func fmtSignedDollar(v float64) string {
	if v >= 0 {
		return "$+" + fmtComma(v)
	}
	return "$-" + fmtComma(-v)
}

// fmtSignedPct formats a percentage with +/- prefix.
func fmtSignedPct(v float64) string {
	if v >= 0 {
		return fmt.Sprintf("+%.1f%%", v)
	}
	return fmt.Sprintf("%.1f%%", v)
}

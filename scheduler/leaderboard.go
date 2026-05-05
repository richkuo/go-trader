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
	ID              string  `json:"id"`
	Type            string  `json:"type"`
	Value           float64 `json:"value"`
	Capital         float64 `json:"capital"`
	PnL             float64 `json:"pnl"`
	PnLPct          float64 `json:"pnl_pct"`
	Trades          int     `json:"trades"`
	Sharpe          float64 `json:"sharpe"`           // #397 — annualized Sharpe; 0 = undefined/no data. Kept in the serialized form (no omitempty) so consumers can distinguish "present but zero" from "omitted".
	Timeframe       string  `json:"timeframe"`        // #580 — candle timeframe (Args[2]) or "—".
	Interval        string  `json:"interval"`         // #580 — formatted check interval (e.g. "1h").
	PositionsOpened int     `json:"positions_opened"` // #607 — lifetime open-leg count (is_close=0 rows) from trades table; survives RiskState resets. Replaces the round-trip count for the #T column.
	Wins            int     `json:"wins"`             // #580 — closed round trips with net realized PnL > 0.
	Losses          int     `json:"losses"`           // #580 — closed round trips with net realized PnL < 0.
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
// (previously written to leaderboard.json every cycle). lifetimeStats (#580)
// is keyed by strategy ID; missing keys render zero round trips because
// SQLite trades are authoritative — pass nil when unavailable.
func BuildLeaderboardMessages(cfg *Config, state *AppState, prices map[string]float64, sharpeByStrategy map[string]float64, lifetimeStats map[string]LifetimeTradeStats) map[string]string {
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

		allEntries = append(allEntries, newLeaderboardEntry(sc, ss, pv, initCap, pnl, pnlPct, sharpeByStrategy, lifetimeStats, cfg.IntervalSeconds))
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

// newLeaderboardEntry assembles a LeaderboardEntry, pulling timeframe/interval
// from the strategy config and lifetime open-leg / W-L counts from the
// lifetime stats map (#580/#607). Missing lifetimeStats entry → zero counts.
func newLeaderboardEntry(sc StrategyConfig, ss *StrategyState, pv, initCap, pnl, pnlPct float64, sharpeByStrategy map[string]float64, lifetimeStats map[string]LifetimeTradeStats, globalIntervalSeconds int) LeaderboardEntry {
	effectiveInterval := sc.IntervalSeconds
	if effectiveInterval <= 0 {
		effectiveInterval = globalIntervalSeconds
	}
	lt := lifetimeStats[sc.ID]
	return LeaderboardEntry{
		ID:              sc.ID,
		Type:            sc.Type,
		Value:           pv,
		Capital:         initCap,
		PnL:             pnl,
		PnLPct:          pnlPct,
		Trades:          len(ss.TradeHistory),
		Sharpe:          sharpeByStrategy[sc.ID],
		Timeframe:       extractTimeframe(sc),
		Interval:        formatInterval(effectiveInterval),
		PositionsOpened: lt.PositionsOpened,
		Wins:            lt.Wins,
		Losses:          lt.Losses,
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
	totalPositionsOpened, totalWins, totalLosses := 0, 0, 0
	winning, losing, flat := 0, 0, 0
	for _, e := range entries {
		totalValue += e.Value
		totalCapital += e.Capital
		totalPositionsOpened += e.PositionsOpened
		totalWins += e.Wins
		totalLosses += e.Losses
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

	// Tf/Int/#T/W/L columns added in #580 to match FormatCategorySummary's
	// per-channel A→Z table. Sharpe stays at the end as the leaderboard's
	// distinguishing column.
	var (
		header   string
		rowFmt   string
		labelMax int
	)
	if showType {
		header = fmt.Sprintf("%-18s %-6s %10s %10s %7s %4s %4s %4s %5s %7s",
			"Strategy", "Type", "Value", "PnL", "PnL%", "Tf", "Int", "#T", "W/L", "Sharpe")
		rowFmt = "%-18s %-6s %10s %10s %7s %4s %4s %4d %5s %7s\n"
		labelMax = 18
	} else {
		// Non-showType keeps the wider 26-char Strategy column from origin/main
		// so IDs like "hl-btc-tiered-atr-paper" (23 chars) render in full. The
		// showType branch shrinks to 18 to match catTableStrategyWidth.
		header = fmt.Sprintf("%-26s %10s %10s %7s %4s %4s %4s %5s %7s",
			"Strategy", "Value", "PnL", "PnL%", "Tf", "Int", "#T", "W/L", "Sharpe")
		rowFmt = "%-26s %10s %10s %7s %4s %4s %4d %5s %7s\n"
		labelMax = 26
	}
	sep := strings.Repeat("-", len(header))
	sb.WriteString("\n```\n")
	sb.WriteString(header + "\n")
	sb.WriteString(sep + "\n")

	for _, e := range top {
		label := e.ID
		if len(label) > labelMax {
			label = label[:labelMax]
		}
		valStr := "$" + fmtComma(e.Value)
		pnlStr := fmtSignedDollar(e.PnL)
		pctStr := fmtSignedPct(e.PnLPct)
		tfStr := truncateRunes(e.Timeframe, 4)
		intStr := truncateRunes(e.Interval, 4)
		wlStr := fmtWinLossRatio(e.Wins, e.Losses)
		sharpeStr := fmtSharpe(e.Sharpe)
		if showType {
			sb.WriteString(fmt.Sprintf(rowFmt, label, e.Type, valStr, pnlStr, pctStr, tfStr, intStr, e.PositionsOpened, wlStr, sharpeStr))
		} else {
			sb.WriteString(fmt.Sprintf(rowFmt, label, valStr, pnlStr, pctStr, tfStr, intStr, e.PositionsOpened, wlStr, sharpeStr))
		}
	}
	sb.WriteString(sep + "\n")

	totalLabel := fmt.Sprintf("TOTAL (%d strategies)", len(entries))
	if len(totalLabel) > labelMax {
		totalLabel = totalLabel[:labelMax]
	}
	totValStr := "$" + fmtComma(totalValue)
	totPnlStr := fmtSignedDollar(totalPnl)
	totPctStr := fmtSignedPct(totalPnlPct)
	totWlStr := fmtWinLossRatio(totalWins, totalLosses)
	if showType {
		sb.WriteString(fmt.Sprintf(rowFmt, totalLabel, "", totValStr, totPnlStr, totPctStr, "", "", totalPositionsOpened, totWlStr, ""))
	} else {
		sb.WriteString(fmt.Sprintf(rowFmt, totalLabel, totValStr, totPnlStr, totPctStr, "", "", totalPositionsOpened, totWlStr, ""))
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
func PostLeaderboard(cfg *Config, state *AppState, prices map[string]float64, sharpeByStrategy map[string]float64, lifetimeStats map[string]LifetimeTradeStats, notifier *MultiNotifier) error {
	return postLeaderboardMessages(BuildLeaderboardMessages(cfg, state, prices, sharpeByStrategy, lifetimeStats), notifier)
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
// Issue #308. lifetimeStats may be nil; missing keys render zero round trips.
func BuildLeaderboardSummary(lc LeaderboardSummaryConfig, cfg *Config, state *AppState, prices map[string]float64, sharpeByStrategy map[string]float64, lifetimeStats map[string]LifetimeTradeStats) string {
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
		entries = append(entries, newLeaderboardEntry(sc, ss, pv, initCap, pnl, pnlPct, sharpeByStrategy, lifetimeStats, cfg.IntervalSeconds))
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

// truncateRunes returns s clipped to at most max runes. Rune-aware so multi-byte
// glyphs (e.g. "—") aren't sliced into invalid UTF-8. Used for the Tf/Int
// column width guard. Callers that pass `extractTimeframe` / `formatInterval`
// values can rely on those helpers to produce non-empty output.
func truncateRunes(s string, max int) string {
	if utf8.RuneCountInString(s) <= max {
		return s
	}
	runes := []rune(s)
	return string(runes[:max])
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

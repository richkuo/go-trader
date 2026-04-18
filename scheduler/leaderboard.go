package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// LeaderboardEntry holds pre-computed PnL data for one strategy.
type LeaderboardEntry struct {
	ID      string  `json:"id"`
	Type    string  `json:"type"`
	Value   float64 `json:"value"`
	Capital float64 `json:"capital"`
	PnL     float64 `json:"pnl"`
	PnLPct  float64 `json:"pnl_pct"`
	Trades  int     `json:"trades"`
}

// LeaderboardData is the pre-computed leaderboard written to disk each cycle.
type LeaderboardData struct {
	Timestamp time.Time `json:"timestamp"`
	// Messages keys: "top10" and "bottom10". The names are retained for
	// backwards compatibility with the on-disk leaderboard.json schema; the
	// entry count is actually controlled by Discord.LeaderboardTopN.
	Messages map[string]string `json:"messages"`
}

// leaderboardPath returns the path for the pre-computed leaderboard file,
// stored next to the SQLite state DB.
func leaderboardPath(cfg *Config) string {
	dir := filepath.Dir(cfg.DBFile)
	return filepath.Join(dir, "leaderboard.json")
}

// leaderboardTopN returns the configured top-N count, defaulting to 5 when unset.
func leaderboardTopN(cfg *Config) int {
	if cfg.Discord.LeaderboardTopN > 0 {
		return cfg.Discord.LeaderboardTopN
	}
	return 5
}

// PrecomputeLeaderboard builds leaderboard messages from current state and writes
// them to leaderboard.json. Called after each cycle's state save so the data is
// always fresh. The cron job just reads and posts this file.
func PrecomputeLeaderboard(cfg *Config, state *AppState, prices map[string]float64) error {
	// Build entries for all strategies.
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
		})
	}

	messages := make(map[string]string)
	topN := leaderboardTopN(cfg)

	// All-time top-N and bottom-N across all strategies. Per-product sections
	// (spot/perps/options/futures) are delivered via BuildLeaderboardSummary
	// to individual platform channels; the dedicated leaderboard channel shows
	// only the aggregate view. Issue #310.
	if len(allEntries) > 0 {
		messages["top10"] = formatAllTimeMessage("🏆", "Top All-Time Performers", allEntries, true, topN)
		messages["bottom10"] = formatAllTimeMessage("💀", "Bottom All-Time Performers", allEntries, false, topN)
	}

	data := LeaderboardData{
		Timestamp: time.Now().UTC(),
		Messages:  messages,
	}

	path := leaderboardPath(cfg)
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("create leaderboard dir: %w", err)
	}

	raw, err := json.MarshalIndent(data, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal leaderboard: %w", err)
	}
	tmpPath := path + ".tmp"
	if err := os.WriteFile(tmpPath, raw, 0600); err != nil {
		return fmt.Errorf("write leaderboard: %w", err)
	}
	return os.Rename(tmpPath, path)
}

// formatLeaderboardMessage formats a leaderboard message for a sorted slice of entries.
// Used by formatAllTimeMessage (top10/bottom10) and BuildLeaderboardSummary (per-platform summaries).
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

	const sep = "--------------------------------------------------------------------"
	sb.WriteString("\n```\n")
	if showType {
		sb.WriteString(fmt.Sprintf("%-22s %-6s %10s %10s %7s %8s\n", "Strategy", "Type", "Value", "PnL", "PnL%", "Trades"))
	} else {
		sb.WriteString(fmt.Sprintf("%-26s %10s %10s %7s %8s\n", "Strategy", "Value", "PnL", "PnL%", "Trades"))
	}
	sb.WriteString(sep + "\n")

	for _, e := range top {
		label := e.ID
		valStr := "$" + fmtComma(e.Value)
		pnlStr := fmtSignedDollar(e.PnL)
		pctStr := fmtSignedPct(e.PnLPct)
		tradesStr := fmt.Sprintf("%d", e.Trades)
		if showType {
			if len(label) > 22 {
				label = label[:22]
			}
			sb.WriteString(fmt.Sprintf("%-22s %-6s %10s %10s %7s %8s\n", label, e.Type, valStr, pnlStr, pctStr, tradesStr))
		} else {
			if len(label) > 26 {
				label = label[:26]
			}
			sb.WriteString(fmt.Sprintf("%-26s %10s %10s %7s %8s\n", label, valStr, pnlStr, pctStr, tradesStr))
		}
	}
	sb.WriteString(sep + "\n")

	totalLabel := fmt.Sprintf("TOTAL (%d strategies)", len(entries))
	totValStr := "$" + fmtComma(totalValue)
	totPnlStr := fmtSignedDollar(totalPnl)
	totPctStr := fmtSignedPct(totalPnlPct)
	totTradesStr := fmt.Sprintf("%d", totalTrades)
	if showType {
		sb.WriteString(fmt.Sprintf("%-22s %-6s %10s %10s %7s %8s\n", totalLabel, "", totValStr, totPnlStr, totPctStr, totTradesStr))
	} else {
		sb.WriteString(fmt.Sprintf("%-26s %10s %10s %7s %8s\n", totalLabel, totValStr, totPnlStr, totPctStr, totTradesStr))
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

// LoadLeaderboard reads the pre-computed leaderboard from disk.
func LoadLeaderboard(cfg *Config) (*LeaderboardData, error) {
	path := leaderboardPath(cfg)
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read leaderboard: %w", err)
	}
	var lb LeaderboardData
	if err := json.Unmarshal(data, &lb); err != nil {
		return nil, fmt.Errorf("parse leaderboard: %w", err)
	}
	return &lb, nil
}

// PostLeaderboard reads the pre-computed leaderboard and posts all messages
// to the configured notification backends. This is the fast path for the cron job.
func PostLeaderboard(cfg *Config, notifier *MultiNotifier) error {
	lb, err := LoadLeaderboard(cfg)
	if err != nil {
		return err
	}

	// Post aggregate top/bottom messages with 1s delay between them.
	// Routing is decided per-backend inside the notifier: backends with a
	// dedicated leaderboard channel route there, others fall back to broadcast
	// across all configured channels. Issue #310.
	order := []string{"top10", "bottom10"}
	first := true
	for _, key := range order {
		msg, ok := lb.Messages[key]
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

	fmt.Printf("Leaderboard posted (pre-computed at %s)\n", lb.Timestamp.Format(time.RFC3339))
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
func BuildLeaderboardSummary(lc LeaderboardSummaryConfig, cfg *Config, state *AppState, prices map[string]float64) string {
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

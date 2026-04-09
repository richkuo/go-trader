package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
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
	Timestamp time.Time         `json:"timestamp"`
	Messages  map[string]string `json:"messages"` // keyed by category: "spot", "perps", "options", "futures", "top10", "bottom10"
}

// leaderboardPath returns the path for the pre-computed leaderboard file,
// stored next to the state file.
func leaderboardPath(cfg *Config) string {
	dir := filepath.Dir(cfg.StateFile)
	return filepath.Join(dir, "leaderboard.json")
}

// PrecomputeLeaderboard builds leaderboard messages from current state and writes
// them to leaderboard.json. Called after each cycle's state save so the data is
// always fresh. The cron job just reads and posts this file.
func PrecomputeLeaderboard(cfg *Config, state *AppState, prices map[string]float64) error {
	// Build entries for all strategies.
	var allEntries []LeaderboardEntry
	typeEntries := make(map[string][]LeaderboardEntry)

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

		entry := LeaderboardEntry{
			ID:      sc.ID,
			Type:    sc.Type,
			Value:   pv,
			Capital: initCap,
			PnL:     pnl,
			PnLPct:  pnlPct,
			Trades:  len(ss.TradeHistory),
		}
		allEntries = append(allEntries, entry)
		typeEntries[sc.Type] = append(typeEntries[sc.Type], entry)
	}

	messages := make(map[string]string)

	// Per-category leaderboards.
	categories := []struct {
		key   string
		icon  string
		title string
	}{
		{"spot", "📈", "Spot Leaderboard"},
		{"perps", "⚡", "Perps Leaderboard (Hyperliquid)"},
		{"options", "🎯", "Options Leaderboard (Deribit/IBKR)"},
		{"futures", "🏦", "Futures Leaderboard (TopStep/IBKR)"},
	}

	for _, cat := range categories {
		entries := typeEntries[cat.key]
		if len(entries) == 0 {
			continue
		}
		messages[cat.key] = formatLeaderboardMessage(cat.icon, cat.title, entries, false)
	}

	// All-time top 10 and bottom 10 across all categories.
	if len(allEntries) > 0 {
		messages["top10"] = formatAllTimeMessage("🏆", "Top 10 All-Time Performers", allEntries, true)
		messages["bottom10"] = formatAllTimeMessage("💀", "Bottom 10 All-Time Performers", allEntries, false)
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

// formatLeaderboardMessage formats a single category leaderboard message.
func formatLeaderboardMessage(icon, title string, entries []LeaderboardEntry, showType bool) string {
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

	// Show top 5.
	top := entries
	if len(top) > 5 {
		top = top[:5]
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
func formatAllTimeMessage(icon, title string, entries []LeaderboardEntry, topN bool) string {
	// Sort: top = descending PnL%, bottom = ascending PnL%.
	sorted := make([]LeaderboardEntry, len(entries))
	copy(sorted, entries)
	if topN {
		sort.Slice(sorted, func(i, j int) bool {
			return sorted[i].PnLPct > sorted[j].PnLPct
		})
	} else {
		sort.Slice(sorted, func(i, j int) bool {
			return sorted[i].PnLPct < sorted[j].PnLPct
		})
	}

	n := 10
	if len(sorted) < n {
		n = len(sorted)
	}
	top := sorted[:n]

	return formatLeaderboardMessage(icon, title, top, true)
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

	// Post category messages in a fixed order with 1s delay between them.
	order := []string{"spot", "perps", "options", "futures", "top10", "bottom10"}
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

		// Route to the matching channel. For category messages, use the type as
		// the channel key. For top10/bottom10, broadcast to all channels.
		switch key {
		case "top10", "bottom10":
			notifier.SendToAllChannels(msg)
		default:
			notifier.SendToChannel(key, key, msg)
		}
		fmt.Println(msg)
	}

	fmt.Printf("Leaderboard posted (pre-computed at %s)\n", lb.Timestamp.Format(time.RFC3339))
	return nil
}

// FormatHyperliquidTop10 builds a top-10 summary message for hyperliquid strategies,
// sorted by PnL% descending. Returns "" if no hyperliquid strategies exist.
func FormatHyperliquidTop10(cfg *Config, state *AppState, prices map[string]float64) string {
	var entries []LeaderboardEntry
	for _, sc := range cfg.Strategies {
		if sc.Platform != "hyperliquid" {
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

	// Sort by PnL% descending, take top 10.
	sort.Slice(entries, func(i, j int) bool {
		return entries[i].PnLPct > entries[j].PnLPct
	})
	n := 10
	if len(entries) < n {
		n = len(entries)
	}

	return formatLeaderboardMessage("⚡", "Hyperliquid Top 10", entries[:n], false)
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

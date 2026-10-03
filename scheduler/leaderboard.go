package main

import (
	"fmt"
	"math"
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

type LeaderboardEntry struct {
	ID              string  `json:"id"`
	Type            string  `json:"type"`
	Asset           string  `json:"asset,omitempty"`
	Value           float64 `json:"value"`
	Capital         float64 `json:"capital"`
	PnL             float64 `json:"pnl"`
	PnLPct          float64 `json:"pnl_pct"`
	PoolBudget      bool    `json:"pool_budget,omitempty"`
	Trades          int     `json:"trades"`
	Sharpe          float64 `json:"sharpe"`
	Timeframe       string  `json:"timeframe"`
	Interval        string  `json:"interval"`
	PositionsOpened int     `json:"positions_opened"`
	Wins            int     `json:"wins"`
	Losses          int     `json:"losses"`
}

func leaderboardTopN(cfg *Config) int {
	if cfg.Discord.LeaderboardTopN > 0 {
		return cfg.Discord.LeaderboardTopN
	}
	return 5
}

func BuildLeaderboardMessages(cfg *Config, state *AppState, prices map[string]float64, sharpeByStrategy map[string]float64, lifetimeStats map[string]LifetimeTradeStats, walletBalances map[SharedWalletKey]float64, accountShared map[SharedWalletKey][]string) map[string]string {
	configByID := make(map[string]StrategyConfig, len(cfg.Strategies))
	for _, sc := range cfg.Strategies {
		configByID[sc.ID] = sc
	}
	allEntries := buildLeaderboardEntries(cfg.Strategies, state, prices, sharpeByStrategy, lifetimeStats, cfg.IntervalSeconds)

	if len(allEntries) == 0 {
		return nil
	}

	topN := leaderboardTopN(cfg)
	return map[string]string{
		"top":    formatAllTimeMessage("🏆", "Top All-Time Performers", allEntries, true, topN, prices, cfg.Regime, state, cfg, configByID, walletBalances, accountShared),
		"bottom": formatAllTimeMessage("💀", "Bottom All-Time Performers", allEntries, false, topN, prices, cfg.Regime, state, cfg, configByID, walletBalances, accountShared),
	}
}

func buildLeaderboardEntries(strategies []StrategyConfig, state *AppState, prices map[string]float64, sharpeByStrategy map[string]float64, lifetimeStats map[string]LifetimeTradeStats, globalIntervalSeconds int) []LeaderboardEntry {
	var entries []LeaderboardEntry
	for _, sc := range strategies {
		ss := state.Strategies[sc.ID]
		if ss == nil {
			continue
		}
		pv := displayStrategyValue(ss, prices)
		initCap := EffectiveInitialCapital(sc, ss)
		pnl := pv - initCap
		pnlPct := 0.0
		if initCap > 0 {
			pnlPct = (pnl / initCap) * 100
		}
		entries = append(entries, newLeaderboardEntry(sc, ss, pv, initCap, pnl, pnlPct, sharpeByStrategy, lifetimeStats, globalIntervalSeconds))
	}
	return entries
}

func sortLeaderboardEntriesByPnLPct(entries []LeaderboardEntry) {
	sort.SliceStable(entries, func(i, j int) bool {
		return leaderboardEntryBefore(entries[i], entries[j], true)
	})
}

func leaderboardEntryBefore(a, b LeaderboardEntry, descending bool) bool {
	if a.PoolBudget != b.PoolBudget {
		return !a.PoolBudget
	}
	if a.PoolBudget {
		return a.ID < b.ID
	}
	if a.PnLPct != b.PnLPct {
		if descending {
			return a.PnLPct > b.PnLPct
		}
		return a.PnLPct < b.PnLPct
	}
	return a.ID < b.ID
}

func newLeaderboardEntry(sc StrategyConfig, ss *StrategyState, pv, initCap, pnl, pnlPct float64, sharpeByStrategy map[string]float64, lifetimeStats map[string]LifetimeTradeStats, globalIntervalSeconds int) LeaderboardEntry {
	effectiveInterval := sc.IntervalSeconds
	if effectiveInterval <= 0 {
		effectiveInterval = globalIntervalSeconds
	}
	lt := lifetimeStats[sc.ID]
	return LeaderboardEntry{
		ID:              sc.ID,
		Type:            sc.Type,
		Asset:           extractAsset(sc),
		Value:           pv,
		Capital:         initCap,
		PnL:             pnl,
		PnLPct:          pnlPct,
		PoolBudget:      usesSharedWalletPoolBudget(sc),
		Trades:          len(ss.TradeHistory),
		Sharpe:          sharpeByStrategy[sc.ID],
		Timeframe:       extractTimeframe(sc),
		Interval:        formatInterval(effectiveInterval),
		PositionsOpened: lt.PositionsOpened,
		Wins:            lt.Wins,
		Losses:          lt.Losses,
	}
}

func leaderboardAssetUsesFuturesName(entries []LeaderboardEntry, asset string) bool {
	for _, e := range entries {
		if e.Asset != asset {
			continue
		}
		if e.Type == "futures" {
			return true
		}
	}
	return false
}

func writeLeaderboardHeaderPrices(sb *strings.Builder, entries []LeaderboardEntry, prices map[string]float64, regime *RegimeConfig, state *AppState, cfg *Config) {
	if len(prices) == 0 || len(entries) == 0 {
		return
	}
	seen := make(map[string]struct{})
	var assets []string
	for _, e := range entries {
		a := strings.TrimSpace(e.Asset)
		if a == "" {
			continue
		}
		if _, ok := seen[a]; ok {
			continue
		}
		seen[a] = struct{}{}
		assets = append(assets, a)
	}
	if len(assets) == 0 {
		return
	}
	sort.SliceStable(assets, func(i, j int) bool {
		return assetSortKey(assets[i]) < assetSortKey(assets[j])
	})
	var regimeByBase map[string]string
	if cfg != nil && state != nil {
		regimeByBase = buildRegimeByBaseAsset(cfg.Strategies, state, regime)
	}
	parts := make([]string, 0, len(assets))
	for _, asset := range assets {
		price, short, ok := priceForAsset(prices, asset)
		if !ok {
			continue
		}
		priceStr := fmtComma2(price)
		var part string
		if leaderboardAssetUsesFuturesName(entries, asset) {
			if fullName, ok := futuresFullNames[strings.ToUpper(short)]; ok {
				part = fmt.Sprintf("%s (%s): $%s", short, fullName, priceStr)
			} else {
				part = fmt.Sprintf("%s: $%s", short, priceStr)
			}
		} else {
			part = fmt.Sprintf("%s: $%s", short, priceStr)
		}
		if regimeByBase != nil {
			if rl := regimeByBase[strings.ToUpper(asset)]; rl != "" {
				part += " | " + rl
			}
		}
		parts = append(parts, part)
	}
	if len(parts) == 0 {
		return
	}
	sb.WriteString(strings.Join(parts, " | "))
	sb.WriteString("\n")
}

func leaderboardAdjustedTotal(
	entries []LeaderboardEntry,
	configByID map[string]StrategyConfig,
	state *AppState,
	prices map[string]float64,
	walletBalances map[SharedWalletKey]float64,
	accountShared map[SharedWalletKey][]string,
) float64 {
	if len(configByID) == 0 || len(entries) == 0 {
		return -1
	}
	var subset []StrategyConfig
	for _, e := range entries {
		if sc, ok := configByID[e.ID]; ok {
			subset = append(subset, sc)
		}
	}
	if len(subset) == 0 {
		return -1
	}
	adj, _ := computeSubsetDisplayValue(subset, state, prices, walletBalances, accountShared)
	return adj
}

func formatLeaderboardMessage(icon, title string, entries []LeaderboardEntry, showType bool, topN int, prices map[string]float64, regime *RegimeConfig, state *AppState, cfg *Config, adjustedTotal float64) string {
	sort.Slice(entries, func(i, j int) bool {
		return leaderboardEntryBefore(entries[i], entries[j], true)
	})

	dateStr := time.Now().Format("January 2, 2006")

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("%s **%s**\n", icon, title))
	sb.WriteString(fmt.Sprintf("Daily Report | %s\n", dateStr))
	writeLeaderboardHeaderPrices(&sb, entries, prices, regime, state, cfg)

	var totalValue, totalCapital float64
	totalPositionsOpened, totalWins, totalLosses := 0, 0, 0
	winning, losing, flat := 0, 0, 0
	hasPoolBudget := false
	for _, e := range entries {
		totalValue += e.Value
		totalCapital += e.Capital
		totalPositionsOpened += e.PositionsOpened
		totalWins += e.Wins
		totalLosses += e.Losses
		if e.PoolBudget {
			hasPoolBudget = true
			if e.PnL > 0 {
				winning++
			} else if e.PnL < 0 {
				losing++
			} else {
				flat++
			}
		} else if e.PnLPct > 0 {
			winning++
		} else if e.PnLPct < 0 {
			losing++
		} else {
			flat++
		}
	}
	totalDisplayValue := totalValue
	if adjustedTotal >= 0 {
		totalDisplayValue = adjustedTotal
	}
	totalPnl := totalDisplayValue - totalCapital
	totalPnlPct := 0.0
	if hasPoolBudget {
		totalPnl = math.NaN()
		totalPnlPct = math.NaN()
	} else if totalCapital > 0 {
		totalPnlPct = (totalPnl / totalCapital) * 100
	}

	top := entries
	if len(top) > topN {
		top = top[:topN]
	}

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
		if e.PoolBudget {
			label += "*"
			pctStr = "—"
		}
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
	totValStr := "$" + fmtComma(totalDisplayValue)
	totPnlStr := fmtSignedDollar(totalPnl)
	totPctStr := fmtSignedPct(totalPnlPct)
	totWlStr := fmtWinLossRatio(totalWins, totalLosses)
	if showType {
		sb.WriteString(fmt.Sprintf(rowFmt, totalLabel, "", totValStr, totPnlStr, totPctStr, "", "", totalPositionsOpened, totWlStr, ""))
	} else {
		sb.WriteString(fmt.Sprintf(rowFmt, totalLabel, totValStr, totPnlStr, totPctStr, "", "", totalPositionsOpened, totWlStr, ""))
	}
	sb.WriteString("```\n")
	if hasPoolBudget {
		sb.WriteString("* POOL row: Value/PnL is attributed net performance; PnL% and TOTAL return are unavailable because wallet deposits are not strategy capital.\n")
	}
	sb.WriteString(fmt.Sprintf("🟢 %d winning · 🔴 %d losing · ⚪ %d flat\n", winning, losing, flat))

	return sb.String()
}

func formatAllTimeMessage(icon, title string, entries []LeaderboardEntry, isTop bool, topN int, prices map[string]float64, regime *RegimeConfig, state *AppState, cfg *Config, configByID map[string]StrategyConfig, walletBalances map[SharedWalletKey]float64, accountShared map[SharedWalletKey][]string) string {
	sorted := make([]LeaderboardEntry, len(entries))
	copy(sorted, entries)
	if isTop {
		sort.Slice(sorted, func(i, j int) bool {
			return leaderboardEntryBefore(sorted[i], sorted[j], true)
		})
	} else {
		sort.Slice(sorted, func(i, j int) bool {
			return leaderboardEntryBefore(sorted[i], sorted[j], false)
		})
	}

	n := topN
	if len(sorted) < n {
		n = len(sorted)
	}
	top := sorted[:n]

	adj := leaderboardAdjustedTotal(top, configByID, state, prices, walletBalances, accountShared)
	return formatLeaderboardMessage(icon, title, top, true, n, prices, regime, state, cfg, adj)
}

func PostLeaderboard(cfg *Config, state *AppState, prices map[string]float64, sharpeByStrategy map[string]float64, lifetimeStats map[string]LifetimeTradeStats, notifier *MultiNotifier) error {
	walletBalances, _ := fetchSharedWalletBalances(cfg.Strategies, nil)
	accountShared := detectSharedWallets(cfg.Strategies)
	return postLeaderboardMessages(BuildLeaderboardMessages(cfg, state, prices, sharpeByStrategy, lifetimeStats, walletBalances, accountShared), notifier)
}

func postLeaderboardMessages(messages map[string]string, notifier *MultiNotifier) error {
	if len(messages) == 0 {
		return fmt.Errorf("no strategies to leaderboard")
	}

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

func titleCase(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	if s == "" {
		return s
	}
	r, size := utf8.DecodeRuneInString(s)
	return string(unicode.ToUpper(r)) + s[size:]
}

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

func BuildLeaderboardSummary(lc LeaderboardSummaryConfig, cfg *Config, state *AppState, prices map[string]float64, sharpeByStrategy map[string]float64, lifetimeStats map[string]LifetimeTradeStats, walletBalances map[SharedWalletKey]float64, accountShared map[SharedWalletKey][]string) string {
	topN := lc.TopN
	if topN <= 0 {
		topN = 5
	}
	tickerFilter := strings.ToUpper(strings.TrimSpace(lc.Ticker))

	var entries []LeaderboardEntry
	configByID := make(map[string]StrategyConfig)
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
		pv := displayStrategyValue(ss, prices)
		initCap := EffectiveInitialCapital(sc, ss)
		pnl := pv - initCap
		pnlPct := 0.0
		if initCap > 0 {
			pnlPct = (pnl / initCap) * 100
		}
		entries = append(entries, newLeaderboardEntry(sc, ss, pv, initCap, pnl, pnlPct, sharpeByStrategy, lifetimeStats, cfg.IntervalSeconds))
		configByID[sc.ID] = sc
	}

	if len(entries) == 0 {
		return ""
	}

	sort.Slice(entries, func(i, j int) bool {
		return leaderboardEntryBefore(entries[i], entries[j], true)
	})
	n := topN
	if len(entries) < n {
		n = len(entries)
	}

	adj := leaderboardAdjustedTotal(entries[:n], configByID, state, prices, walletBalances, accountShared)

	platformTitle := titleCase(lc.Platform)
	title := fmt.Sprintf("%s Top %d", platformTitle, n)
	if tickerFilter != "" {
		title = fmt.Sprintf("%s %s Top %d", platformTitle, tickerFilter, n)
	}
	return formatLeaderboardMessage(platformIcon(lc.Platform), title, entries[:n], false, n, prices, cfg.Regime, state, cfg, adj)
}

func truncateRunes(s string, max int) string {
	if utf8.RuneCountInString(s) <= max {
		return s
	}
	runes := []rune(s)
	return string(runes[:max])
}

func fmtSignedDollar(v float64) string {
	if math.IsNaN(v) {
		return "—"
	}
	if v >= 0 {
		return "$+" + fmtComma(v)
	}
	return "$-" + fmtComma(-v)
}

func fmtSignedPct(v float64) string {
	if math.IsNaN(v) {
		return "—"
	}
	if v >= 0 {
		return fmt.Sprintf("+%.1f%%", v)
	}
	return fmt.Sprintf("%.1f%%", v)
}

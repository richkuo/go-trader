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
	Asset           string  `json:"asset,omitempty"` // base symbol for header prices (#741)
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
// BuildLeaderboardMessages builds the daily top/bottom leaderboard messages.
// walletBalances and accountShared are used to compute shared-wallet-adjusted
// TOTAL rows (#915); pass nil maps to skip adjustment (falls back to the naive
// per-strategy sum for the TOTAL row).
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

// buildLeaderboardEntries computes one LeaderboardEntry per configured
// strategy that has state — the structured data layer beneath both the Discord
// leaderboard surfaces and the #1231 /api/leaderboard endpoint. Entries are
// returned unsorted (config order); rank-order is a presentation concern.
// Caller must hold the state read lock; prices/lifetimeStats/sharpeByStrategy
// may be nil (missing keys render zero, matching the Discord command).
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

// sortLeaderboardEntriesByPnLPct orders entries by PnL% descending, ID
// ascending on ties — the canonical leaderboard rank order shared by the
// Discord command and /api/leaderboard.
func sortLeaderboardEntriesByPnLPct(entries []LeaderboardEntry) {
	sort.SliceStable(entries, func(i, j int) bool {
		if entries[i].PnLPct != entries[j].PnLPct {
			return entries[i].PnLPct > entries[j].PnLPct
		}
		return entries[i].ID < entries[j].ID
	})
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
		Asset:           extractAsset(sc),
		Value:           pv,
		Capital:         initCap,
		PnL:             pnl,
		PnLPct:          pnlPct,
		Trades:          independentAlphaTradeCount(ss.TradeHistory),
		Sharpe:          sharpeByStrategy[sc.ID],
		Timeframe:       extractTimeframe(sc),
		Interval:        formatInterval(effectiveInterval),
		PositionsOpened: lt.PositionsOpened,
		Wins:            lt.Wins,
		Losses:          lt.Losses,
	}
}

// leaderboardAssetUsesFuturesName reports whether any entry for asset uses
// type=futures so the header price line can show full contract names (#741).
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

// writeLeaderboardHeaderPrices emits an inline "SYM: $price [| regime]" line after
// the daily banner when prices are available (#741).
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

// formatLeaderboardMessage formats a leaderboard message for a sorted slice of entries.
// Used by formatAllTimeMessage (top/bottom) and BuildLeaderboardSummary (per-platform summaries).
// Callers are responsible for passing a positive topN (see leaderboardTopN).
// leaderboardAdjustedTotal returns the shared-wallet-adjusted portfolio value
// for a set of leaderboard entries. Entries whose strategy configs cannot be
// found in configByID contribute their e.Value directly (no dedup).
// Returns -1 (the "no adjustment available" sentinel — a portfolio value is
// never negative) when no wallet dedup can be performed (e.g. nil configByID)
// so the caller falls through to the naive sum. A real adjusted value of $0
// (drained shared wallet) is returned as 0 and used as-is.
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
	// Sort by PnL% descending.
	sort.Slice(entries, func(i, j int) bool {
		return entries[i].PnLPct > entries[j].PnLPct
	})

	dateStr := time.Now().Format("January 2, 2006")

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("%s **%s**\n", icon, title))
	sb.WriteString(fmt.Sprintf("Daily Report | %s\n", dateStr))
	writeLeaderboardHeaderPrices(&sb, entries, prices, regime, state, cfg)

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
	// Use the caller-supplied shared-wallet-adjusted total when available so
	// the TOTAL row doesn't double-count virtual cash in shared-wallet setups
	// (#915). Per-strategy rows above are unaffected.
	//
	// Sentinel: a negative adjustedTotal means "no adjustment available" (fall
	// back to the naive Σ e.Value). A portfolio value is never negative, so a
	// legitimately drained shared wallet (real balance $0) displays $0 instead
	// of being mistaken for "unset" (#917 review item 3).
	totalDisplayValue := totalValue
	if adjustedTotal >= 0 {
		totalDisplayValue = adjustedTotal
	}
	totalPnl := totalDisplayValue - totalCapital
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
	sb.WriteString(fmt.Sprintf("🟢 %d winning · 🔴 %d losing · ⚪ %d flat\n", winning, losing, flat))

	return sb.String()
}

// formatAllTimeMessage formats the top/bottom all-time leaderboard.
// isTop controls sort direction; topN controls how many entries to show.
// configByID, walletBalances, and accountShared are used to compute a
// shared-wallet-adjusted TOTAL row (#915); pass nil maps to skip adjustment.
func formatAllTimeMessage(icon, title string, entries []LeaderboardEntry, isTop bool, topN int, prices map[string]float64, regime *RegimeConfig, state *AppState, cfg *Config, configByID map[string]StrategyConfig, walletBalances map[SharedWalletKey]float64, accountShared map[SharedWalletKey][]string) string {
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

	adj := leaderboardAdjustedTotal(top, configByID, state, prices, walletBalances, accountShared)
	return formatLeaderboardMessage(icon, title, top, true, n, prices, regime, state, cfg, adj)
}

// PostLeaderboard computes the leaderboard on-demand and posts all messages to
// the configured notification backends. Issue #313 moved this from reading a
// pre-computed leaderboard.json (which was rewritten every cycle) to computing
// fresh data at post time — the data is only used by the daily cron post and
// the --leaderboard flag, so there is no benefit to pre-computation.
func PostLeaderboard(cfg *Config, state *AppState, prices map[string]float64, sharpeByStrategy map[string]float64, lifetimeStats map[string]LifetimeTradeStats, notifier *MultiNotifier) error {
	// Fetch shared-wallet balances for adjusted TOTAL rows (#915). Best-effort:
	// failures produce nil maps and BuildLeaderboardMessages falls back to the
	// naive sum (same as before #915).
	walletBalances, _ := fetchSharedWalletBalances(cfg.Strategies, nil)
	accountShared := detectSharedWallets(cfg.Strategies)
	return postLeaderboardMessages(BuildLeaderboardMessages(cfg, state, prices, sharpeByStrategy, lifetimeStats, walletBalances, accountShared), notifier)
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
//
// walletBalances/accountShared feed the shared-wallet-adjusted TOTAL (#915) and
// are supplied by the caller — NOT fetched here — because this runs under the
// state write lock on the per-cycle path (collectDueLeaderboardSummaries); the
// network I/O of fetchSharedWalletBalances must stay outside the lock. The
// CLI exit path (runLeaderboardSummariesAndExit) fetches once before the loop
// and passes the result in. Pass nil for both to fall back to the naive sum.
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
		return entries[i].PnLPct > entries[j].PnLPct
	})
	n := topN
	if len(entries) < n {
		n = len(entries)
	}

	// Compute shared-wallet-adjusted TOTAL for the shown entries (#915). Wallet
	// balances are supplied by the caller (already fetched outside the lock) so
	// this path performs no network I/O — critical because the per-cycle caller
	// holds the state write lock. nil balances → naive sum fallback.
	adj := leaderboardAdjustedTotal(entries[:n], configByID, state, prices, walletBalances, accountShared)

	platformTitle := titleCase(lc.Platform)
	title := fmt.Sprintf("%s Top %d", platformTitle, n)
	if tickerFilter != "" {
		title = fmt.Sprintf("%s %s Top %d", platformTitle, tickerFilter, n)
	}
	return formatLeaderboardMessage(platformIcon(lc.Platform), title, entries[:n], false, n, prices, cfg.Regime, state, cfg, adj)
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

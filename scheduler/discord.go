package main

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/bwmarrin/discordgo"
)

// ErrDMTimeout is returned when no DM response arrives within the deadline.
var ErrDMTimeout = errors.New("DM response timeout")

type dmHandler struct {
	userID  string
	ch      chan string
	expires time.Time
}

// DiscordNotifier wraps a discordgo.Session for sending messages and two-way DM communication.
type DiscordNotifier struct {
	session    *discordgo.Session
	ownerID    string
	dmHandlers []dmHandler
	mu         sync.Mutex
}

// NewDiscordNotifier creates a discordgo session, registers the DM message handler, and opens the gateway.
func NewDiscordNotifier(token, ownerID string) (*DiscordNotifier, error) {
	session, err := discordgo.New("Bot " + token)
	if err != nil {
		return nil, fmt.Errorf("create session: %w", err)
	}

	d := &DiscordNotifier{
		session: session,
		ownerID: ownerID,
	}
	session.Identify.Intents = discordgo.IntentsDirectMessages
	session.AddHandler(d.messageCreate)

	if err := session.Open(); err != nil {
		return nil, fmt.Errorf("open gateway: %w", err)
	}

	return d, nil
}

// Close shuts down the gateway connection.
func (d *DiscordNotifier) Close() {
	d.session.Close()
}

// SendMessage posts content to a channel. Truncates to 2000 chars.
func (d *DiscordNotifier) SendMessage(channelID string, content string) error {
	if len(content) > 2000 {
		content = content[:1997] + "..."
	}
	_, err := d.session.ChannelMessageSend(channelID, content)
	return err
}

// SendDM opens a DM channel with userID and sends content.
func (d *DiscordNotifier) SendDM(userID, content string) error {
	ch, err := d.session.UserChannelCreate(userID)
	if err != nil {
		return fmt.Errorf("create DM channel: %w", err)
	}
	if len(content) > 2000 {
		content = content[:1997] + "..."
	}
	_, err = d.session.ChannelMessageSend(ch.ID, content)
	return err
}

// AskDM sends question to userID via DM and waits up to timeout for a reply.
// Returns ErrDMTimeout if no response arrives in time.
func (d *DiscordNotifier) AskDM(userID, question string, timeout time.Duration) (string, error) {
	if err := d.SendDM(userID, question); err != nil {
		return "", fmt.Errorf("send DM: %w", err)
	}

	ch := make(chan string, 1)
	h := dmHandler{
		userID:  userID,
		ch:      ch,
		expires: time.Now().Add(timeout),
	}

	d.mu.Lock()
	d.dmHandlers = append(d.dmHandlers, h)
	d.mu.Unlock()

	select {
	case resp := <-ch:
		return resp, nil
	case <-time.After(timeout):
		d.mu.Lock()
		for i, hh := range d.dmHandlers {
			if hh.ch == ch {
				d.dmHandlers = append(d.dmHandlers[:i], d.dmHandlers[i+1:]...)
				break
			}
		}
		d.mu.Unlock()
		return "", ErrDMTimeout
	}
}

// messageCreate handles incoming Discord messages, routing DM replies to waiting AskDM callers.
func (d *DiscordNotifier) messageCreate(s *discordgo.Session, m *discordgo.MessageCreate) {
	if m.Author == nil || s.State == nil || s.State.User == nil {
		return
	}
	if m.Author.ID == s.State.User.ID {
		return // ignore own messages
	}
	if m.GuildID != "" {
		return // only handle DMs
	}

	d.mu.Lock()
	defer d.mu.Unlock()

	now := time.Now()
	dispatched := false
	var remaining []dmHandler
	for _, h := range d.dmHandlers {
		if h.expires.Before(now) {
			continue // drop expired
		}
		if !dispatched && h.userID == m.Author.ID {
			select {
			case h.ch <- m.Content:
			default:
			}
			dispatched = true
			// consumed: not added to remaining
		} else {
			remaining = append(remaining, h)
		}
	}
	d.dmHandlers = remaining
}

// resolveChannel returns the Discord channel ID for a strategy.
// Lookup order: channels[platform] -> channels[stratType] -> "" (no channel).
func resolveChannel(channels map[string]string, platform, stratType string) string {
	if ch, ok := channels[platform]; ok && ch != "" {
		return ch
	}
	if ch, ok := channels[stratType]; ok && ch != "" {
		return ch
	}
	return ""
}

// channelKeyFromID returns the map key for a given channel ID (reverse lookup for display labels).
func channelKeyFromID(channels map[string]string, chID string) string {
	for k, v := range channels {
		if v == chID {
			return k
		}
	}
	return chID
}

// isOptionsType returns true if any strategy in the list is an options strategy.
func isOptionsType(strats []StrategyConfig) bool {
	for _, sc := range strats {
		if sc.Type == "options" {
			return true
		}
	}
	return false
}

// isFuturesType returns true if any strategy in the list is a futures strategy.
func isFuturesType(strats []StrategyConfig) bool {
	for _, sc := range strats {
		if sc.Type == "futures" {
			return true
		}
	}
	return false
}

// isPerpsType returns true if any strategy in the list is a perps strategy.
func isPerpsType(strats []StrategyConfig) bool {
	for _, sc := range strats {
		if sc.Type == "perps" {
			return true
		}
	}
	return false
}

// futuresFullNames maps ticker symbols to their full contract names.
var futuresFullNames = map[string]string{
	"MES": "Micro E-mini S&P 500",
	"MNQ": "Micro E-mini Nasdaq-100",
	"ES":  "E-mini S&P 500",
	"NQ":  "E-mini Nasdaq-100",
}

// futuresDisplayName returns "TICKER (Full Name)" if known, else just the ticker.
func futuresDisplayName(ticker string) string {
	if name, ok := futuresFullNames[strings.ToUpper(ticker)]; ok {
		return fmt.Sprintf("%s (%s)", strings.ToUpper(ticker), name)
	}
	return strings.ToUpper(ticker)
}

// FormatCategorySummary creates a Discord message for a set of strategies sharing a channel.
// channelStrategies is pre-filtered by the caller; channelKey is the display label.
// asset, when non-empty, appends " — <ASSET>" to the title and filters the prices line.
func FormatCategorySummary(
	cycle int,
	elapsed time.Duration,
	strategiesRun int,
	totalTrades int,
	totalValue float64,
	prices map[string]float64,
	tradeDetails []string,
	channelStrategies []StrategyConfig,
	state *AppState,
	channelKey string,
	asset string,
) string {
	var sb strings.Builder

	// Icon and title based on strategy types and channel key.
	isFutures := isFuturesType(channelStrategies) || channelKey == "futures" || channelKey == "ibkr"
	icon := "📊"
	if isOptionsType(channelStrategies) {
		icon = "🎯"
	} else if channelKey == "spot" {
		icon = "📈"
	} else if channelKey == "perps" || channelKey == "hyperliquid" {
		icon = "⚡"
	} else if isFutures {
		icon = "🏦"
	}
	title := strings.ToUpper(channelKey[:1]) + channelKey[1:]
	assetSuffix := ""
	if asset != "" {
		if isFutures {
			assetSuffix = " — " + futuresDisplayName(asset)
		} else {
			assetSuffix = " — " + asset
		}
	}
	if totalTrades > 0 {
		sb.WriteString(fmt.Sprintf("%s **%s TRADES%s**\n", icon, strings.ToUpper(title), assetSuffix))
	} else {
		sb.WriteString(fmt.Sprintf("%s **%s Summary%s**\n", icon, title, assetSuffix))
	}

	sb.WriteString(fmt.Sprintf("Cycle #%d | %.1fs\n", cycle, elapsed.Seconds()))

	// Prices inline — filter to just this asset when asset is specified.
	displayPrices := prices
	if asset != "" {
		displayPrices = make(map[string]float64)
		for sym, price := range prices {
			base := strings.ToUpper(strings.SplitN(sym, "/", 2)[0])
			if base == asset {
				displayPrices[sym] = price
			}
		}
	}
	if len(displayPrices) > 0 {
		syms := make([]string, 0, len(displayPrices))
		for s := range displayPrices {
			syms = append(syms, s)
		}
		sort.Strings(syms)
		parts := make([]string, 0, len(syms))
		for _, sym := range syms {
			short := strings.TrimSuffix(sym, "/USDT")
			if isFutures {
				if fullName, ok := futuresFullNames[strings.ToUpper(short)]; ok {
					parts = append(parts, fmt.Sprintf("%s (%s) $%.0f", short, fullName, displayPrices[sym]))
				} else {
					parts = append(parts, fmt.Sprintf("%s $%.0f", short, displayPrices[sym]))
				}
			} else {
				parts = append(parts, fmt.Sprintf("%s $%.0f", short, displayPrices[sym]))
			}
		}
		sb.WriteString(strings.Join(parts, " | "))
		sb.WriteString("\n")
	}

	// Detect shared wallet groups: strategies on same platform with CapitalPct > 0.
	walletCapital := make(map[string]float64) // platform -> sum of capitals
	walletCount := make(map[string]int)       // platform -> count of strategies
	for _, sc := range channelStrategies {
		if sc.CapitalPct > 0 {
			walletCapital[sc.Platform] += sc.Capital
			walletCount[sc.Platform]++
		}
	}
	hasSharedWallet := false
	for _, n := range walletCount {
		if n > 1 {
			hasSharedWallet = true
			break
		}
	}

	// Build flat bot list from the provided channel strategies.
	var tableBots []botInfo
	var totalInitCap, filteredValue float64
	for _, sc := range channelStrategies {
		ss := state.Strategies[sc.ID]
		if ss == nil {
			continue
		}
		pv := PortfolioValue(ss, prices)
		walletPct := 0.0

		// Shared wallet indicator (no value scaling — cash is already split by capital_pct).
		if sc.CapitalPct > 0 && walletCount[sc.Platform] > 1 {
			walletPct = sc.CapitalPct * 100
		}

		initCap := ss.InitialCapital
		if initCap == 0 {
			initCap = sc.Capital // fallback for strategies without InitialCapital set
		}
		totalInitCap += initCap
		filteredValue += pv
		pnl := pv - initCap
		openPos := len(ss.Positions) + len(ss.OptionPositions)
		stratName := extractStrategyName(sc)
		pnlPct := 0.0
		if initCap > 0 {
			pnlPct = (pnl / initCap) * 100
		}
		asset := extractAsset(sc)
		tableBots = append(tableBots, botInfo{
			id:            sc.ID,
			strategy:      stratName,
			asset:         asset,
			value:         pv,
			initialCap:    initCap,
			pnl:           pnl,
			pnlPct:        pnlPct,
			walletPct:     walletPct,
			trades:        len(ss.TradeHistory),
			openPositions: openPos,
			closedTrades:  ss.RiskState.TotalTrades,
			tradeHistory:  ss.TradeHistory,
		})
	}

	totalPnl := filteredValue - totalInitCap
	totalPnlPct := 0.0
	if totalInitCap > 0 {
		totalPnlPct = (totalPnl / totalInitCap) * 100
	}
	writeCatTable(&sb, tableBots, filteredValue, totalPnl, totalPnlPct, hasSharedWallet)

	// Positions summary (#145, #162)
	totalOpenPos := 0
	for _, bot := range tableBots {
		totalOpenPos += bot.openPositions
	}
	if totalOpenPos == 0 {
		sb.WriteString("Positions: no open positions\n")
	} else {
		sb.WriteString(fmt.Sprintf("Positions: %d open\n", totalOpenPos))
		for _, sc := range channelStrategies {
			ss := state.Strategies[sc.ID]
			if ss == nil {
				continue
			}
			for _, line := range collectPositions(sc.ID, ss, prices) {
				sb.WriteString(fmt.Sprintf("  • %s\n", line))
			}
		}
	}

	// Trade details (always shown)
	if len(tradeDetails) > 0 {
		sb.WriteString("\n**Trades:**\n")
		for _, td := range tradeDetails {
			sb.WriteString(fmt.Sprintf("• %s\n", td))
		}
	}

	return sb.String()
}

type botInfo struct {
	id            string
	strategy      string
	asset         string
	value         float64
	initialCap    float64
	pnl           float64
	pnlPct        float64
	walletPct     float64 // 0 = not a shared wallet; >0 = strategy's share of the wallet
	trades        int
	openPositions int
	closedTrades  int
	tradeHistory  []Trade
}

func extractStrategyName(sc StrategyConfig) string {
	if sc.Type == "options" && len(sc.Args) > 0 {
		return sc.Args[0]
	}
	parts := strings.Split(sc.ID, "-")
	// For perps: "hl-sma-btc" -> "sma" (skip "hl" prefix and asset suffix)
	if sc.Type == "perps" && len(parts) >= 3 && parts[0] == "hl" {
		return parts[1]
	}
	// For spot: "momentum-btc" -> "momentum"
	if len(parts) > 0 {
		return parts[0]
	}
	return "unknown"
}

func extractAsset(sc StrategyConfig) string {
	// Args[1] is the canonical asset source for all strategy types.
	// Spot uses "BTC/USDT" style symbols; strip the quote currency.
	if len(sc.Args) > 1 {
		asset := strings.ToUpper(sc.Args[1])
		return strings.TrimSuffix(asset, "/USDT")
	}
	return ""
}

// assetSortKey returns a stable sort key so BTC/ETH/SOL/BNB appear first.
func assetSortKey(asset string) string {
	switch asset {
	case "BTC":
		return "0"
	case "ETH":
		return "1"
	case "SOL":
		return "2"
	case "BNB":
		return "3"
	default:
		return "4" + asset
	}
}

// groupByAsset groups strategies by asset and returns sorted asset keys.
// Strategies with no extractable asset are grouped under "".
func groupByAsset(strats []StrategyConfig) (map[string][]StrategyConfig, []string) {
	groups := make(map[string][]StrategyConfig)
	for _, sc := range strats {
		asset := extractAsset(sc)
		groups[asset] = append(groups[asset], sc)
	}
	keys := make([]string, 0, len(groups))
	for k := range groups {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		return assetSortKey(keys[i]) < assetSortKey(keys[j])
	})
	return groups, keys
}

// fmtComma formats a float as a comma-separated integer string (e.g. 1234567 -> "1,234,567").
func fmtComma(v float64) string {
	n := int(v)
	if n < 0 {
		return "-" + fmtComma(-v)
	}
	s := fmt.Sprintf("%d", n)
	if len(s) <= 3 {
		return s
	}
	var out []byte
	for i, c := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			out = append(out, ',')
		}
		out = append(out, byte(c))
	}
	return string(out)
}

// writeCatTable writes a monospace code-block table to sb.
// When showWalletPct is true, an extra "Wallet%" column is rendered for shared-wallet strategies.
func writeCatTable(sb *strings.Builder, bots []botInfo, totalValue, totalPnl, totalPnlPct float64, showWalletPct bool) {
	if len(bots) == 0 {
		return
	}
	sb.WriteString("\n```\n")
	if showWalletPct {
		const sep = "------------------------------------------------------------"
		sb.WriteString(fmt.Sprintf("%-12s %10s %10s %10s %7s %8s\n", "Strategy", "Init", "Value", "PnL", "PnL%", "Wallet%"))
		sb.WriteString(sep + "\n")
		var totalInit float64
		for _, bot := range bots {
			label := bot.id
			if len(label) > 12 {
				label = label[:12]
			}
			valStr := "$ " + fmtComma(bot.value)
			initStr := "$ " + fmtComma(bot.initialCap)
			pnlStr := fmtPnl(bot.pnl)
			pctStr := fmtPnlPct(bot.pnlPct)
			wpStr := ""
			if bot.walletPct > 0 {
				wpStr = fmt.Sprintf("%.1f%%", bot.walletPct)
			}
			totalInit += bot.initialCap
			sb.WriteString(fmt.Sprintf("%-12s %10s %10s %10s %7s %8s\n", label, initStr, valStr, pnlStr, pctStr, wpStr))
		}
		sb.WriteString(sep + "\n")
		totValStr := "$ " + fmtComma(totalValue)
		totInitStr := "$ " + fmtComma(totalInit)
		totPnlStr := fmtPnl(totalPnl)
		totPctStr := fmtPnlPct(totalPnlPct)
		sb.WriteString(fmt.Sprintf("%-12s %10s %10s %10s %7s %8s\n", "TOTAL", totInitStr, totValStr, totPnlStr, totPctStr, "100.0%"))
	} else {
		const sep = "--------------------------------------------------"
		sb.WriteString(fmt.Sprintf("%-12s %10s %10s %10s %7s\n", "Strategy", "Init", "Value", "PnL", "PnL%"))
		sb.WriteString(sep + "\n")
		var totalInit float64
		for _, bot := range bots {
			label := bot.id
			if len(label) > 12 {
				label = label[:12]
			}
			valStr := "$ " + fmtComma(bot.value)
			initStr := "$ " + fmtComma(bot.initialCap)
			pnlStr := fmtPnl(bot.pnl)
			pctStr := fmtPnlPct(bot.pnlPct)
			totalInit += bot.initialCap
			sb.WriteString(fmt.Sprintf("%-12s %10s %10s %10s %7s\n", label, initStr, valStr, pnlStr, pctStr))
		}
		sb.WriteString(sep + "\n")
		totValStr := "$ " + fmtComma(totalValue)
		totInitStr := "$ " + fmtComma(totalInit)
		totPnlStr := fmtPnl(totalPnl)
		totPctStr := fmtPnlPct(totalPnlPct)
		sb.WriteString(fmt.Sprintf("%-12s %10s %10s %10s %7s\n", "TOTAL", totInitStr, totValStr, totPnlStr, totPctStr))
	}
	sb.WriteString("```\n")
}

func fmtPnl(pnl float64) string {
	sign := "+"
	abs := pnl
	if pnl < 0 {
		sign = "-"
		abs = -pnl
	}
	return "$ " + sign + fmtComma(abs)
}

func fmtPnlPct(pct float64) string {
	sign := "+"
	if pct < 0 {
		sign = ""
	}
	return fmt.Sprintf("%s%.1f%%", sign, pct)
}

// collectPositions returns human-readable position lines for a strategy.
func collectPositions(stratID string, ss *StrategyState, prices map[string]float64) []string {
	var lines []string
	for sym, pos := range ss.Positions {
		currentPrice := prices[sym]
		if currentPrice == 0 {
			currentPrice = pos.AvgCost
		}
		pnl := pos.Quantity * (currentPrice - pos.AvgCost)
		if pos.Side != "long" {
			pnl = pos.Quantity * (pos.AvgCost - currentPrice)
		}
		sign := "+"
		if pnl < 0 {
			sign = ""
		}
		lines = append(lines, fmt.Sprintf("%s %s %s x%g @ $%.2f (%s$%.0f)", stratID, pos.Side, sym, pos.Quantity, pos.AvgCost, sign, pnl))
	}
	for key, opt := range ss.OptionPositions {
		lines = append(lines, fmt.Sprintf("%s OPT %s ($%.0f)", stratID, key, opt.CurrentValueUSD))
	}
	return lines
}

// FormatTradeDM formats a Trade into a concise DM message for the bot owner.
func FormatTradeDM(sc StrategyConfig, trade Trade, mode string) string {
	isClose := strings.Contains(trade.Details, "Close")

	icon := "🟢"
	header := "TRADE EXECUTED"
	if isClose {
		icon = "🔴"
		header = "TRADE CLOSED"
	}

	platformLabel := sc.Platform
	if len(platformLabel) > 0 {
		platformLabel = strings.ToUpper(platformLabel[:1]) + platformLabel[1:]
	}
	typeLabel := sc.Type

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("%s **%s**\n", icon, header))
	sb.WriteString(fmt.Sprintf("Strategy: %s (%s %s)\n", sc.ID, platformLabel, typeLabel))
	sb.WriteString(fmt.Sprintf("%s — %s %.6g @ $%s\n", trade.Symbol, strings.ToUpper(trade.Side), trade.Quantity, fmtComma(trade.Price)))

	valueLine := fmt.Sprintf("Value: $%s", fmtComma(trade.Value))
	if isClose {
		if pnl, ok := extractPnL(trade.Details); ok {
			valueLine += fmt.Sprintf(" | PnL: $%s", pnl)
		}
	}
	valueLine += fmt.Sprintf(" | Mode: %s", mode)
	sb.WriteString(valueLine)

	return sb.String()
}

// extractPnL parses the PnL value from a trade Details string.
// Handles both "PnL: $123.45" and "PnL=$123.45" formats.
func extractPnL(details string) (string, bool) {
	for _, prefix := range []string{"PnL: $", "PnL=$"} {
		if idx := strings.Index(details, prefix); idx >= 0 {
			pnlStr := details[idx+len(prefix):]
			if end := strings.Index(pnlStr, " "); end >= 0 {
				pnlStr = pnlStr[:end]
			}
			return pnlStr, true
		}
	}
	return "", false
}

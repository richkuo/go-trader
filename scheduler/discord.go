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

// resolveTradeChannel resolves the channel ID for a trade alert.
// For paper trades: tries "<platform>-paper" first, then falls back to resolveChannel.
// For live trades: uses resolveChannel directly (platform -> stratType).
// Presence of a channel ID means alerts are enabled; absence means disabled.
func resolveTradeChannel(channels map[string]string, platform, stratType string, isLive bool) string {
	if !isLive {
		if ch, ok := channels[platform+"-paper"]; ok && ch != "" {
			return ch
		}
	}
	return resolveChannel(channels, platform, stratType)
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

// discordCharLimit is the maximum characters per Discord message.
const discordCharLimit = 2000

// discordSplitThreshold is the soft limit at which we start splitting messages.
const discordSplitThreshold = 1980

// catTableMaxRows caps how many strategy rows render per Discord message before
// the table is continued in a follow-up message. Sized so the rendered table
// (including header/sep/totals) plus the base summary header and the per-channel
// position section stays under the 2000-char limit (#381 added #T, #434 added
// W/L, and #436 added DD; 15 rows remains within the Discord limit).
const catTableMaxRows = 15

// FormatCategorySummary creates Discord messages for a set of strategies sharing a channel.
// Returns a slice of messages; when the content exceeds Discord's 2000-char limit,
// the position list is split across multiple messages.
// channelStrategies is pre-filtered by the caller; channelKey is the display label.
// asset, when non-empty, appends " — <ASSET>" to the title and filters the prices line.
// globalIntervalSeconds is the config-level default interval used when a strategy has no per-strategy override.
// lifetimeStats is keyed by strategy ID; missing keys fall back to the
// in-memory RiskState counters (#455). When nil, all rows fall back —
// preserves existing test call sites that pre-date this parameter.
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
	globalIntervalSeconds int,
	categorySharpe float64,
	lifetimeStats map[string]LifetimeTradeStats,
) []string {
	var sb strings.Builder

	// Summaries scan better with strategies ordered A→Z by ID (#354). Callers often
	// pass config file order, which is not necessarily alphabetical.
	strategies := append([]StrategyConfig(nil), channelStrategies...)
	sort.SliceStable(strategies, func(i, j int) bool {
		return strategies[i].ID < strategies[j].ID
	})

	// Icon and title based on strategy types and channel key.
	isFutures := isFuturesType(strategies) || channelKey == "futures" || channelKey == "ibkr"
	icon := "📊"
	if isOptionsType(strategies) {
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
	verSuffix := ""
	if Version != "" {
		verSuffix = " (" + Version + ")"
	}
	if totalTrades > 0 {
		sb.WriteString(fmt.Sprintf("%s **%s TRADES%s**%s\n", icon, strings.ToUpper(title), assetSuffix, verSuffix))
	} else {
		sb.WriteString(fmt.Sprintf("%s **%s Summary%s**%s\n", icon, title, assetSuffix, verSuffix))
	}

	sb.WriteString(fmt.Sprintf("Cycle #%d | %.1fs\n", cycle, elapsed.Seconds()))

	// Circuit breaker status — show warning for any strategy with active breaker.
	var cbActive []string
	now := time.Now().UTC()
	for _, sc := range strategies {
		ss := state.Strategies[sc.ID]
		if ss == nil {
			continue
		}
		if ss.RiskState.CircuitBreaker && now.Before(ss.RiskState.CircuitBreakerUntil) {
			remaining := ss.RiskState.CircuitBreakerUntil.Sub(now).Truncate(time.Minute)
			cbActive = append(cbActive, fmt.Sprintf("%s (resumes in %s)", sc.ID, remaining))
		}
	}
	if len(cbActive) > 0 {
		sb.WriteString("🚫 **Circuit breaker active — trading disabled**\n")
		for _, cb := range cbActive {
			sb.WriteString(fmt.Sprintf("  • %s\n", cb))
		}
	} else {
		sb.WriteString("✅ **Trading active**\n")
	}

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
			priceStr := fmtComma2(displayPrices[sym])
			if isFutures {
				if fullName, ok := futuresFullNames[strings.ToUpper(short)]; ok {
					parts = append(parts, fmt.Sprintf("%s (%s): $%s", short, fullName, priceStr))
				} else {
					parts = append(parts, fmt.Sprintf("%s: $%s", short, priceStr))
				}
			} else {
				parts = append(parts, fmt.Sprintf("%s: $%s", short, priceStr))
			}
		}
		sb.WriteString(strings.Join(parts, " | "))
		sb.WriteString("\n")
	}

	// Detect shared wallet groups: strategies on same platform with CapitalPct > 0.
	walletCapital := make(map[string]float64) // platform -> sum of capitals
	walletCount := make(map[string]int)       // platform -> count of strategies
	for _, sc := range strategies {
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
	for _, sc := range strategies {
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

		initCap := EffectiveInitialCapital(sc, ss)
		totalInitCap += initCap
		filteredValue += pv
		pnl := pv - initCap
		openPos := len(ss.Positions) + len(ss.OptionPositions)
		stratName := extractStrategyName(sc)
		pnlPct := 0.0
		if initCap > 0 {
			pnlPct = (pnl / initCap) * 100
		}
		botAsset := extractAsset(sc)
		tf := extractTimeframe(sc)
		effectiveInterval := sc.IntervalSeconds
		if effectiveInterval <= 0 {
			effectiveInterval = globalIntervalSeconds
		}
		// Lifetime round-trip stats from the trades table (#455). Survives
		// kill-switch and circuit-breaker resets; counts each open+close
		// pair as a single trade. Falls back to the in-memory RiskState
		// counters when the DB hasn't reported a stat for this strategy
		// (e.g. tests that don't wire a DB, or first-run before any
		// trade has been recorded).
		closedT := ss.RiskState.TotalTrades
		winT := ss.RiskState.WinningTrades
		lossT := ss.RiskState.LosingTrades
		if lifetimeStats != nil {
			if lt, ok := lifetimeStats[sc.ID]; ok {
				closedT = lt.RoundTrips
				winT = lt.Wins
				lossT = lt.Losses
			}
		}
		tableBots = append(tableBots, botInfo{
			id:             sc.ID,
			strategy:       stratName,
			asset:          botAsset,
			timeframe:      tf,
			interval:       formatInterval(effectiveInterval),
			value:          pv,
			initialCap:     initCap,
			pnl:            pnl,
			pnlPct:         pnlPct,
			maxDrawdownPct: sc.MaxDrawdownPct,
			walletPct:      walletPct,
			trades:         len(ss.TradeHistory),
			openPositions:  openPos,
			closedTrades:   closedT,
			winningTrades:  winT,
			losingTrades:   lossT,
			tradeHistory:   ss.TradeHistory,
		})
	}

	totalPnl := filteredValue - totalInitCap
	totalPnlPct := 0.0
	if totalInitCap > 0 {
		totalPnlPct = (totalPnl / totalInitCap) * 100
	}

	// Render the strategy table in chunks of catTableMaxRows. The first chunk
	// is appended to the in-message header; any extra chunks become standalone
	// continuation messages so the table never overflows the 2000-char limit.
	tableChunks := writeCatTableChunks(tableBots, filteredValue, totalPnl, totalPnlPct, hasSharedWallet)
	if len(tableChunks) > 0 {
		sb.WriteString(tableChunks[0])
	}

	// Book Sharpe ratio (#397). "Book" meaning the pooled portfolio of every
	// strategy in this channel/asset, not any one strategy's figure — per-strategy
	// Sharpes are rendered in the leaderboard column. Computed from realized
	// daily returns with zero-fill on flat days (see sharpe.go).
	if categorySharpe != 0 {
		sb.WriteString(fmt.Sprintf("📐 Book Sharpe (realized, annualized): %s\n", fmtSharpe(categorySharpe)))
	}

	header := sb.String()

	var continuationTables []string
	for i := 1; i < len(tableChunks); i++ {
		rowStart := i*catTableMaxRows + 1
		rowEnd := i*catTableMaxRows + catTableMaxRows
		if rowEnd > len(tableBots) {
			rowEnd = len(tableBots)
		}
		label := fmt.Sprintf("📊 **Strategies (cont'd %d–%d/%d)**", rowStart, rowEnd, len(tableBots))
		continuationTables = append(continuationTables, label+tableChunks[i])
	}

	// Collect position lines.
	totalOpenPos := 0
	for _, bot := range tableBots {
		totalOpenPos += bot.openPositions
	}
	var posLines []string
	if totalOpenPos > 0 {
		for _, sc := range strategies {
			ss := state.Strategies[sc.ID]
			if ss == nil {
				continue
			}
			posLines = append(posLines, collectPositions(sc.ID, ss, prices)...)
		}
	}

	// Collect trade detail lines.
	var tradeLines []string
	for _, td := range tradeDetails {
		tradeLines = append(tradeLines, fmt.Sprintf("• %s", td))
	}

	return splitCategorySummary(header, totalOpenPos, posLines, tradeLines, continuationTables)
}

// splitCategorySummary assembles the header, position lines, and trade lines into
// one or more Discord messages, splitting at logical boundaries to stay under the
// 2000-char Discord limit. continuationTables are extra strategy-table chunks
// (already formatted as their own code blocks) that get inserted right after the
// first message so the rest of the table is the very next thing the user sees.
func splitCategorySummary(header string, totalOpenPos int, posLines []string, tradeLines []string, continuationTables []string) []string {
	msgs := splitCategorySummaryCore(header, totalOpenPos, posLines, tradeLines)
	if len(continuationTables) == 0 {
		return msgs
	}
	// Insert continuation table chunks immediately after the first message so
	// readers see the rest of the strategy table before any position overflow.
	out := make([]string, 0, len(msgs)+len(continuationTables))
	out = append(out, msgs[0])
	out = append(out, continuationTables...)
	out = append(out, msgs[1:]...)
	return out
}

// splitCategorySummaryCore builds the message list without considering
// strategy-table continuation chunks. Callers wanting continuation handling
// should call splitCategorySummary instead.
func splitCategorySummaryCore(header string, totalOpenPos int, posLines []string, tradeLines []string) []string {
	var sb strings.Builder
	sb.WriteString(header)

	// Position header
	if totalOpenPos == 0 {
		sb.WriteString("Positions: no open positions\n")
	} else {
		sb.WriteString(fmt.Sprintf("Positions: %d open\n", totalOpenPos))
	}

	// Trade details section
	var tradeSuffix string
	if len(tradeLines) > 0 {
		var tsb strings.Builder
		tsb.WriteString("\n**Trades:**\n")
		for _, tl := range tradeLines {
			tsb.WriteString(tl + "\n")
		}
		tradeSuffix = tsb.String()
	}

	// If no positions, just append trades and return.
	if totalOpenPos == 0 || len(posLines) == 0 {
		sb.WriteString(tradeSuffix)
		return []string{sb.String()}
	}

	// Try fitting everything in one message.
	fullMsg := sb.String()
	for _, line := range posLines {
		fullMsg += fmt.Sprintf("  • %s\n", line)
	}
	fullMsg += tradeSuffix
	if len(fullMsg) <= discordSplitThreshold {
		return []string{fullMsg}
	}

	// Need to split: add positions until we approach the limit.
	firstMsg := sb.String() // header + "Positions: N open\n"
	included := 0
	for _, line := range posLines {
		candidate := fmt.Sprintf("  • %s\n", line)
		// Reserve room for the "... and N more" indicator.
		remaining := len(posLines) - included - 1
		moreIndicator := ""
		if remaining > 0 {
			moreIndicator = fmt.Sprintf("  ... and %d more\n", remaining)
		}
		if len(firstMsg)+len(candidate)+len(moreIndicator) > discordSplitThreshold {
			break
		}
		firstMsg += candidate
		included++
	}

	if included < len(posLines) {
		firstMsg += fmt.Sprintf("  ... and %d more\n", len(posLines)-included)
	}

	// If all positions fit (edge case where trades push us over), include trades in second message.
	if included >= len(posLines) {
		// Trades caused the overflow. Put all positions in first message, trades in second.
		firstMsg = sb.String()
		for _, line := range posLines {
			firstMsg += fmt.Sprintf("  • %s\n", line)
		}
		if len(tradeSuffix) > 0 {
			return []string{firstMsg, tradeSuffix}
		}
		return []string{firstMsg}
	}

	// Build continuation message(s) with remaining positions + trades.
	var msgs []string
	msgs = append(msgs, firstMsg)

	contMsg := fmt.Sprintf("Positions (cont'd %d/%d):\n", included+1, len(posLines))
	for _, line := range posLines[included:] {
		candidate := fmt.Sprintf("  • %s\n", line)
		if len(contMsg)+len(candidate) > discordSplitThreshold {
			msgs = append(msgs, contMsg)
			contMsg = fmt.Sprintf("Positions (cont'd):\n")
		}
		contMsg += candidate
	}
	if len(contMsg)+len(tradeSuffix) > discordSplitThreshold {
		msgs = append(msgs, contMsg)
		if tradeSuffix != "" {
			msgs = append(msgs, tradeSuffix)
		}
	} else {
		contMsg += tradeSuffix
		msgs = append(msgs, contMsg)
	}

	return msgs
}

type botInfo struct {
	id             string
	strategy       string
	asset          string
	timeframe      string // e.g. "1h" or "—" for spot/options
	interval       string // e.g. "10m", formatted from effective interval seconds
	value          float64
	initialCap     float64
	pnl            float64
	pnlPct         float64
	maxDrawdownPct float64
	walletPct      float64 // 0 = not a shared wallet; >0 = strategy's share of the wallet
	trades         int
	openPositions  int
	closedTrades   int
	winningTrades  int
	losingTrades   int
	tradeHistory   []Trade
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

// extractTimeframe returns the candle timeframe for a strategy, or "—" if none.
// Perps and futures scripts (check_hyperliquid.py, check_topstep.py, check_robinhood.py,
// check_okx.py) use args[2] as the timeframe (e.g. "1h").
// Spot (check_strategy.py) and options scripts have no timeframe argument.
func extractTimeframe(sc StrategyConfig) string {
	if len(sc.Args) > 2 && !strings.HasPrefix(sc.Args[2], "--") {
		return sc.Args[2]
	}
	return "—"
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

// insertCommas inserts thousands separators into a non-negative integer string.
// e.g. "1234567" -> "1,234,567". Input must contain only digits.
func insertCommas(intStr string) string {
	if len(intStr) <= 3 {
		return intStr
	}
	var out []byte
	for i := 0; i < len(intStr); i++ {
		if i > 0 && (len(intStr)-i)%3 == 0 {
			out = append(out, ',')
		}
		out = append(out, intStr[i])
	}
	return string(out)
}

// fmtComma formats a float as a comma-separated integer string (e.g. 1234567 -> "1,234,567").
func fmtComma(v float64) string {
	n := int(v)
	if n < 0 {
		return "-" + insertCommas(fmt.Sprintf("%d", -n))
	}
	return insertCommas(fmt.Sprintf("%d", n))
}

// fmtComma2 formats a float with thousands separators and two decimal places,
// e.g. 2240.5 → "2,240.50". Negative values get a leading minus sign.
func fmtComma2(v float64) string {
	neg := v < 0
	if neg {
		v = -v
	}
	s := fmt.Sprintf("%.2f", v)
	dot := strings.Index(s, ".")
	result := insertCommas(s[:dot]) + s[dot:]
	if neg {
		return "-" + result
	}
	return result
}

// formatInterval converts a duration in seconds to a short human-readable string.
// Examples: 60 → "1m", 600 → "10m", 3600 → "1h", 86400 → "1d".
func formatInterval(seconds int) string {
	if seconds <= 0 {
		return "—"
	}
	if seconds%86400 == 0 {
		return fmt.Sprintf("%dd", seconds/86400)
	}
	if seconds%3600 == 0 {
		return fmt.Sprintf("%dh", seconds/3600)
	}
	if seconds%60 == 0 {
		return fmt.Sprintf("%dm", seconds/60)
	}
	return fmt.Sprintf("%ds", seconds)
}

// writeCatTablePartial writes a single code-block table containing the supplied
// bots. When includeTotals is true the trailing TOTAL row is appended using the
// supplied totals (which should be computed from the FULL bot list, not just
// this chunk). totalClosed is the sum of closedTrades across the full bot list
// and is rendered in the #T column of the TOTAL row. totalWins/totalLosses
// drive the W/L column in the TOTAL row. Used by writeCatTableChunks.
func writeCatTablePartial(sb *strings.Builder, bots []botInfo, showWalletPct, includeTotals bool, totalInit, totalValue, totalPnl, totalPnlPct float64, totalClosed, totalWins, totalLosses int) {
	if len(bots) == 0 {
		return
	}
	sb.WriteString("\n```\n")
	if showWalletPct {
		sep := strings.Repeat("-", 83)
		sb.WriteString(fmt.Sprintf("%-16s%9s %6s %6s %8s%5s %8s%5s %4s %4s %5s\n", "Strategy", "Init", "Value", "PnL", "PnL%", "DD", "Wallet%", "Tf", "Int", "#T", "W/L"))
		sb.WriteString(sep + "\n")
		for _, bot := range bots {
			label := bot.id
			if len(label) > 16 {
				label = label[:16]
			}
			valStr := fmtComma(bot.value)
			initStr := fmtComma(bot.initialCap)
			pnlStr := fmtPnl(bot.pnl)
			pctStr := fmtPnlPct(bot.pnlPct)
			maxDDStr := fmtDrawdownPct(bot.maxDrawdownPct)
			wpStr := ""
			if bot.walletPct > 0 {
				wpStr = fmt.Sprintf("%.1f%%", bot.walletPct)
			}
			wlStr := fmtWinLossRatio(bot.winningTrades, bot.losingTrades)
			sb.WriteString(fmt.Sprintf("%-16s%9s %6s %6s %8s%5s %8s%5s %4s %4d %5s\n", label, initStr, valStr, pnlStr, pctStr, maxDDStr, wpStr, bot.timeframe, bot.interval, bot.closedTrades, wlStr))
		}
		if includeTotals {
			sb.WriteString(sep + "\n")
			totValStr := fmtComma(totalValue)
			totInitStr := fmtComma(totalInit)
			totPnlStr := fmtPnl(totalPnl)
			totPctStr := fmtPnlPct(totalPnlPct)
			totWlStr := fmtWinLossRatio(totalWins, totalLosses)
			sb.WriteString(fmt.Sprintf("%-16s%9s %6s %6s %8s%5s %8s%5s %4s %4d %5s\n", "TOTAL", totInitStr, totValStr, totPnlStr, totPctStr, "", "100.0%", "", "", totalClosed, totWlStr))
		}
	} else {
		sep := strings.Repeat("-", 75)
		sb.WriteString(fmt.Sprintf("%-16s%9s %6s %6s %8s%5s %5s %4s %4s %5s\n", "Strategy", "Init", "Value", "PnL", "PnL%", "DD", "Tf", "Int", "#T", "W/L"))
		sb.WriteString(sep + "\n")
		for _, bot := range bots {
			label := bot.id
			if len(label) > 16 {
				label = label[:16]
			}
			valStr := fmtComma(bot.value)
			initStr := fmtComma(bot.initialCap)
			pnlStr := fmtPnl(bot.pnl)
			pctStr := fmtPnlPct(bot.pnlPct)
			wlStr := fmtWinLossRatio(bot.winningTrades, bot.losingTrades)
			maxDDStr := fmtDrawdownPct(bot.maxDrawdownPct)
			sb.WriteString(fmt.Sprintf("%-16s%9s %6s %6s %8s%5s %5s %4s %4d %5s\n", label, initStr, valStr, pnlStr, pctStr, maxDDStr, bot.timeframe, bot.interval, bot.closedTrades, wlStr))
		}
		if includeTotals {
			sb.WriteString(sep + "\n")
			totValStr := fmtComma(totalValue)
			totInitStr := fmtComma(totalInit)
			totPnlStr := fmtPnl(totalPnl)
			totPctStr := fmtPnlPct(totalPnlPct)
			totWlStr := fmtWinLossRatio(totalWins, totalLosses)
			sb.WriteString(fmt.Sprintf("%-16s%9s %6s %6s %8s%5s %5s %4s %4d %5s\n", "TOTAL", totInitStr, totValStr, totPnlStr, totPctStr, "", "", "", totalClosed, totWlStr))
		}
	}
	sb.WriteString("```\n")
}

// writeCatTableChunks splits bots into catTableMaxRows-sized chunks and returns
// one rendered code-block table per chunk. The TOTAL row appears only in the
// final chunk so totals always show against the same numbers regardless of how
// the table was split. Returns nil if bots is empty.
func writeCatTableChunks(bots []botInfo, totalValue, totalPnl, totalPnlPct float64, showWalletPct bool) []string {
	if len(bots) == 0 {
		return nil
	}
	var totalInit float64
	var totalClosed, totalWins, totalLosses int
	for _, bot := range bots {
		totalInit += bot.initialCap
		totalClosed += bot.closedTrades
		totalWins += bot.winningTrades
		totalLosses += bot.losingTrades
	}
	var chunks []string
	for start := 0; start < len(bots); start += catTableMaxRows {
		end := start + catTableMaxRows
		if end > len(bots) {
			end = len(bots)
		}
		isLast := end == len(bots)
		var sb strings.Builder
		writeCatTablePartial(&sb, bots[start:end], showWalletPct, isLast, totalInit, totalValue, totalPnl, totalPnlPct, totalClosed, totalWins, totalLosses)
		chunks = append(chunks, sb.String())
	}
	return chunks
}

// fmtWinLossRatio formats a wins/losses pair as a Win-Loss ratio string for the
// strategy summary table. Returns "—" when no trades have closed, "∞" when
// every closed trade won (no losses to divide by), and "N.NN" otherwise.
func fmtWinLossRatio(wins, losses int) string {
	if wins == 0 && losses == 0 {
		return "—"
	}
	if losses == 0 {
		return "∞"
	}
	return fmt.Sprintf("%.2f", float64(wins)/float64(losses))
}

func fmtPnl(pnl float64) string {
	sign := "+"
	abs := pnl
	if pnl < 0 {
		sign = "-"
		abs = -pnl
	}
	return sign + fmtComma(abs)
}

func fmtPnlPct(pct float64) string {
	sign := "+"
	if pct < 0 {
		sign = ""
	}
	return fmt.Sprintf("%s%.1f%%", sign, pct)
}

func fmtDrawdownPct(pct float64) string {
	if pct <= 0 {
		return "n/a"
	}
	return fmt.Sprintf("%.0f%%", pct)
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
		absPnl := pnl
		if pnl < 0 {
			sign = "-"
			absPnl = -pnl
		}
		dateStr := ""
		if !pos.OpenedAt.IsZero() {
			dateStr = fmt.Sprintf(" [%s]", pos.OpenedAt.Format("Jan 02 15:04"))
		}
		lines = append(lines, fmt.Sprintf("%s %s %s x%g @ $%s (%s$%s)%s", stratID, strings.ToUpper(pos.Side), sym, pos.Quantity, fmtComma2(pos.AvgCost), sign, fmtComma2(absPnl), dateStr))
	}
	for key, opt := range ss.OptionPositions {
		dateStr := ""
		if !opt.OpenedAt.IsZero() {
			dateStr = fmt.Sprintf(" [%s]", opt.OpenedAt.Format("Jan 02 15:04"))
		}
		lines = append(lines, fmt.Sprintf("%s OPT %s ($%s)%s", stratID, key, fmtComma2(opt.CurrentValueUSD), dateStr))
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
	sb.WriteString(fmt.Sprintf("%s — %s %.6g @ $%s\n", trade.Symbol, tradeDirectionLabel(trade), trade.Quantity, fmtComma(trade.Price)))

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

// tradeSideToDirection converts buy/sell trade sides to LONG/SHORT direction labels.
func tradeSideToDirection(side string) string {
	switch strings.ToLower(side) {
	case "buy":
		return "LONG"
	case "sell":
		return "SHORT"
	default:
		return strings.ToUpper(side)
	}
}

// tradeDirectionLabel returns the LONG/SHORT label describing the *position*
// the trade opens or closes. Details carries "Open long" / "Close long" /
// "Open short" / "Close short" — authoritative for spot/perps/futures. Falls
// back to mapping the execution Side (buy/sell) when Details has no such
// marker (e.g. options wheel fills, circuit-breaker force-close).
//
// Why: close trades invert execution side vs position side — selling to close
// a long execution Side="sell" would render as SHORT, but the position being
// exited was LONG. See #386.
func tradeDirectionLabel(trade Trade) string {
	d := strings.ToLower(trade.Details)
	switch {
	case strings.Contains(d, "open long"), strings.Contains(d, "close long"):
		return "LONG"
	case strings.Contains(d, "open short"), strings.Contains(d, "close short"):
		return "SHORT"
	}
	return tradeSideToDirection(trade.Side)
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

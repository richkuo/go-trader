package main

import (
	"errors"
	"fmt"
	"math"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/bwmarrin/discordgo"
)

var ErrDMTimeout = errors.New("DM response timeout")

type dmHandler struct {
	userID  string
	ch      chan string
	expires time.Time
}

type DiscordNotifier struct {
	session    *discordgo.Session
	ownerID    string
	dmHandlers []dmHandler
	mu         sync.Mutex

	ss  *StatusServer
	cfg *Config
}

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

func (d *DiscordNotifier) Close() {
	d.session.Close()
}

func (d *DiscordNotifier) SendMessage(channelID string, content string) error {
	if len(content) > 2000 {
		content = content[:1997] + "..."
	}
	_, err := d.session.ChannelMessageSend(channelID, content)
	return err
}

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

func (d *DiscordNotifier) messageCreate(s *discordgo.Session, m *discordgo.MessageCreate) {
	if m.Author == nil || s.State == nil || s.State.User == nil {
		return
	}
	if m.Author.ID == s.State.User.ID {
		return
	}
	if m.GuildID != "" {
		return
	}

	d.mu.Lock()
	defer d.mu.Unlock()

	now := time.Now()
	dispatched := false
	var remaining []dmHandler
	for _, h := range d.dmHandlers {
		if h.expires.Before(now) {
			continue
		}
		if !dispatched && h.userID == m.Author.ID {
			select {
			case h.ch <- m.Content:
			default:
			}
			dispatched = true

		} else {
			remaining = append(remaining, h)
		}
	}
	d.dmHandlers = remaining
}

func resolveChannel(channels map[string]string, platform, stratType string) string {
	if ch, ok := channels[platform]; ok && ch != "" {
		return ch
	}
	if ch, ok := channels[stratType]; ok && ch != "" {
		return ch
	}
	return ""
}

func resolveTradeChannel(channels map[string]string, platform, stratType string, isLive bool) string {
	if !isLive {
		if ch, ok := channels[platform+"-paper"]; ok && ch != "" {
			return ch
		}
	}
	return resolveChannel(channels, platform, stratType)
}

func resolveTradeAlertChannel(override, channels map[string]string, platform, stratType string, isLive bool) string {
	if len(override) > 0 {
		if !isLive {
			if ch, ok := override[platform+"-paper"]; ok && ch != "" {
				return ch
			}
		} else {
			if ch, ok := override[platform+"-live"]; ok && ch != "" {
				return ch
			}
		}
		if ch, ok := override[platform]; ok && ch != "" {
			return ch
		}
		if ch, ok := override[stratType]; ok && ch != "" {
			return ch
		}
	}
	return resolveTradeChannel(channels, platform, stratType, isLive)
}

func channelKeyFromID(channels map[string]string, chID string) string {
	for k, v := range channels {
		if v == chID {
			return k
		}
	}
	return chID
}

func isOptionsType(strats []StrategyConfig) bool {
	for _, sc := range strats {
		if sc.Type == "options" {
			return true
		}
	}
	return false
}

func isFuturesType(strats []StrategyConfig) bool {
	for _, sc := range strats {
		if sc.Type == "futures" {
			return true
		}
	}
	return false
}

func isPerpsType(strats []StrategyConfig) bool {
	for _, sc := range strats {
		if sc.Type == "perps" || sc.Type == "manual" {
			return true
		}
	}
	return false
}

var futuresFullNames = map[string]string{
	"MES": "Micro E-mini S&P 500",
	"MNQ": "Micro E-mini Nasdaq-100",
	"ES":  "E-mini S&P 500",
	"NQ":  "E-mini Nasdaq-100",
}

func futuresDisplayName(ticker string) string {
	if name, ok := futuresFullNames[strings.ToUpper(ticker)]; ok {
		return fmt.Sprintf("%s (%s)", strings.ToUpper(ticker), name)
	}
	return strings.ToUpper(ticker)
}

const discordCharLimit = 2000

const discordSplitThreshold = 1980

const (
	catTableMaxRows       = 15
	catTableStrategyWidth = 18
)

var summaryStrategyLabelAliases = map[string]string{
	"tiered-atr": "tatr",
	"tiered-pct": "tpct",
}

func regimeDisplayEnabled(rc *RegimeConfig) bool {
	return rc != nil && rc.Enabled
}

func buildRegimeByBaseAsset(strategies []StrategyConfig, state *AppState, regime *RegimeConfig) map[string]string {
	if !regimeDisplayEnabled(regime) || state == nil {
		return nil
	}
	out := make(map[string]string)
	for _, sc := range strategies {
		ss := state.Strategies[sc.ID]
		if ss == nil || ss.Regime == "" {
			continue
		}
		base := extractAsset(sc)
		if base == "" {
			continue
		}
		if _, ok := out[base]; !ok {
			out[base] = formatStrategyRegimeDisplay(ss, regime)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func priceForAsset(prices map[string]float64, asset string) (float64, string, bool) {
	asset = strings.ToUpper(strings.TrimSpace(asset))
	if asset == "" || len(prices) == 0 {
		return 0, "", false
	}
	if p, ok := prices[asset+"/USDT"]; ok {
		return p, asset, true
	}
	if p, ok := prices[asset]; ok {
		return p, asset, true
	}
	for k, p := range prices {
		base := strings.TrimSuffix(strings.ToUpper(k), "/USDT")
		if base == asset {
			return p, asset, true
		}
	}
	return 0, "", false
}

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
	regime *RegimeConfig,
) []string {
	var sb strings.Builder

	strategies := append([]StrategyConfig(nil), channelStrategies...)
	sort.SliceStable(strategies, func(i, j int) bool {
		return strategies[i].ID < strategies[j].ID
	})

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
		verSuffix = " (" + Version + ""
	}
	pid := os.Getpid()
	if pid != 0 {
		if verSuffix != "" {
			verSuffix += fmt.Sprintf(" pid:%d)", pid)
		} else {
			verSuffix = fmt.Sprintf(" (pid:%d)", pid)
		}
	} else if verSuffix != "" {
		verSuffix += ")"
	}
	if totalTrades > 0 {
		sb.WriteString(fmt.Sprintf("%s **%s TRADES%s**%s\n", icon, strings.ToUpper(title), assetSuffix, verSuffix))
	} else {
		sb.WriteString(fmt.Sprintf("%s **%s Summary%s**%s\n", icon, title, assetSuffix, verSuffix))
	}

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
		regimeByBase := buildRegimeByBaseAsset(strategies, state, regime)
		parts := make([]string, 0, len(syms))
		for _, sym := range syms {
			short := strings.TrimSuffix(sym, "/USDT")
			priceStr := fmtComma2(displayPrices[sym])
			var part string
			if isFutures {
				if fullName, ok := futuresFullNames[strings.ToUpper(short)]; ok {
					part = fmt.Sprintf("%s (%s): $%s", short, fullName, priceStr)
				} else {
					part = fmt.Sprintf("%s: $%s", short, priceStr)
				}
			} else {
				part = fmt.Sprintf("%s: $%s", short, priceStr)
			}
			if regimeByBase != nil {
				base := strings.ToUpper(short)
				if rl := regimeByBase[base]; rl != "" {
					part += " | " + rl
				}
			}
			parts = append(parts, part)
		}
		sb.WriteString(strings.Join(parts, " | "))
		sb.WriteString("\n")
	}

	walletCapital := make(map[string]float64)
	walletCount := make(map[string]int)
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
	hasPoolBudget := false
	for _, sc := range strategies {
		if usesSharedWalletPoolBudget(sc) {
			hasPoolBudget = true
			break
		}
	}

	var tableBots []botInfo
	var totalInitCap, filteredValue float64
	for _, sc := range strategies {
		ss := state.Strategies[sc.ID]
		if ss == nil {
			continue
		}
		pv := displayStrategyValue(ss, prices)
		walletPct := 0.0

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

		closedT, winT, lossT := 0, 0, 0
		if lt, ok := lifetimeStats[sc.ID]; ok {
			closedT = lt.PositionsOpened
			winT = lt.Wins
			lossT = lt.Losses
		}
		tableBots = append(tableBots, botInfo{
			id:             sc.ID,
			strategy:       stratName,
			asset:          botAsset,
			timeframe:      tf,
			interval:       formatInterval(effectiveInterval),
			value:          pv,
			pnl:            pnl,
			pnlPct:         pnlPct,
			poolBudget:     usesSharedWalletPoolBudget(sc),
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

	totalRowValue := filteredValue
	if totalValue >= 0 {
		totalRowValue = totalValue
	}
	totalPnl := totalRowValue - totalInitCap
	totalPnlPct := 0.0
	if hasPoolBudget {

		totalPnl = math.NaN()
		totalPnlPct = math.NaN()
	} else if totalInitCap > 0 {
		totalPnlPct = (totalPnl / totalInitCap) * 100
	}

	if hasPoolBudget {
		sb.WriteString(fmt.Sprintf("Cycle #%d | %.1fs | Allocated initial capital: $%s\n", cycle, elapsed.Seconds(), fmtComma(totalInitCap)))
		sb.WriteString("POOL rows show attributed net performance; deposits are counted only in TOTAL value and PnL% is unavailable.\n")
	} else {
		sb.WriteString(fmt.Sprintf("Cycle #%d | %.1fs | Initial capital: $%s\n", cycle, elapsed.Seconds(), fmtComma(totalInitCap)))
	}

	tableChunks := writeCatTableChunks(tableBots, totalRowValue, totalPnl, totalPnlPct, hasSharedWallet, hasPoolBudget)
	if len(tableChunks) > 0 {
		sb.WriteString(tableChunks[0])
	}

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
			posLines = append(posLines, collectPositions(sc, ss, prices)...)
		}
	}

	var tradeLines []string
	for _, td := range tradeDetails {
		tradeLines = append(tradeLines, fmt.Sprintf("• %s", td))
	}

	return splitCategorySummary(header, totalOpenPos, posLines, tradeLines, continuationTables)
}

func splitCategorySummary(header string, totalOpenPos int, posLines []string, tradeLines []string, continuationTables []string) []string {
	headerOnly := buildSummaryHeader(header, totalOpenPos)
	tradeSuffix := buildTradeSuffix(tradeLines)

	if totalOpenPos == 0 || len(posLines) == 0 {
		msg := headerOnly + tradeSuffix
		if len(continuationTables) == 0 {
			return []string{msg}
		}
		out := make([]string, 0, 1+len(continuationTables))
		out = append(out, msg)
		out = append(out, continuationTables...)
		return out
	}

	if len(continuationTables) == 0 {
		var single strings.Builder
		single.WriteString(headerOnly)
		for _, line := range posLines {
			single.WriteString(fmt.Sprintf("  • %s\n", line))
		}
		single.WriteString(tradeSuffix)
		if single.Len() <= discordSplitThreshold {
			return []string{single.String()}
		}
	}

	posMsgs := buildPositionMessages(posLines)

	leadMsgs := []string{headerOnly}
	if tradeSuffix != "" {
		if len(headerOnly)+len(tradeSuffix) > discordSplitThreshold {
			leadMsgs = append(leadMsgs, tradeSuffix)
		} else {
			leadMsgs[0] = headerOnly + tradeSuffix
		}
	}

	out := make([]string, 0, len(leadMsgs)+len(continuationTables)+len(posMsgs))
	out = append(out, leadMsgs...)
	out = append(out, continuationTables...)
	out = append(out, posMsgs...)
	return out
}

func buildSummaryHeader(header string, totalOpenPos int) string {
	var sb strings.Builder
	sb.WriteString(header)
	if totalOpenPos == 0 {
		sb.WriteString("Positions: no open positions\n")
	} else {
		sb.WriteString(fmt.Sprintf("Positions: %d open\n", totalOpenPos))
	}
	return sb.String()
}

func buildTradeSuffix(tradeLines []string) string {
	if len(tradeLines) == 0 {
		return ""
	}
	var sb strings.Builder
	sb.WriteString("\n**Trades:**\n")
	for _, tl := range tradeLines {
		sb.WriteString(tl + "\n")
	}
	return sb.String()
}

func buildPositionMessages(posLines []string) []string {
	if len(posLines) == 0 {
		return nil
	}
	var msgs []string
	cur := "Positions:\n"
	curHasContent := false
	for _, line := range posLines {
		bullet := fmt.Sprintf("  • %s\n", line)
		if curHasContent && len(cur)+len(bullet) > discordSplitThreshold {
			msgs = append(msgs, cur)
			cur = "Positions (cont'd):\n"
			curHasContent = false
		}
		cur += bullet
		curHasContent = true
	}
	if curHasContent {
		msgs = append(msgs, cur)
	}
	return msgs
}

type botInfo struct {
	id             string
	strategy       string
	asset          string
	timeframe      string
	interval       string
	value          float64
	pnl            float64
	pnlPct         float64
	poolBudget     bool
	maxDrawdownPct float64
	walletPct      float64
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

	if sc.Type == "perps" && len(parts) >= 3 && parts[0] == "hl" {
		return parts[1]
	}

	if len(parts) > 0 {
		return parts[0]
	}
	return "unknown"
}

func summaryStrategyLabel(id string) string {
	label := id
	for old, new := range summaryStrategyLabelAliases {
		label = strings.ReplaceAll(label, old, new)
	}
	if len(label) > catTableStrategyWidth {
		label = label[:catTableStrategyWidth]
	}
	return label
}

func extractAsset(sc StrategyConfig) string {

	if len(sc.Args) > 1 {
		asset := strings.ToUpper(sc.Args[1])
		return strings.TrimSuffix(asset, "/USDT")
	}
	return ""
}

func extractTimeframe(sc StrategyConfig) string {
	if len(sc.Args) > 2 && !strings.HasPrefix(sc.Args[2], "--") {
		return sc.Args[2]
	}
	return "—"
}

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

func fmtComma(v float64) string {
	n := int(v)
	if n < 0 {
		return "-" + insertCommas(fmt.Sprintf("%d", -n))
	}
	return insertCommas(fmt.Sprintf("%d", n))
}

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

func writeCatTablePartial(sb *strings.Builder, bots []botInfo, showWalletPct, includeTotals bool, totalValue, totalPnl, totalPnlPct float64, totalClosed, totalWins, totalLosses int, hasPoolBudget bool) {
	if len(bots) == 0 {
		return
	}
	sb.WriteString("\n```\n")
	if hasPoolBudget {

		if showWalletPct {
			header := fmt.Sprintf("%-*s %6s %8s%5s %8s%5s %4s %4s %5s", catTableStrategyWidth, "Strategy", "PnL", "PnL%", "DD", "Wallet%", "Tf", "Int", "#T", "W/L")
			sep := strings.Repeat("-", len(header))
			sb.WriteString(header + "\n")
			sb.WriteString(sep + "\n")
			for _, bot := range bots {
				label := summaryStrategyLabel(bot.id)
				if bot.poolBudget {
					label += "*"
				}
				pnlStr := fmtPnl(bot.pnl)
				pctStr := "—"
				maxDDStr := fmtDrawdownPct(bot.maxDrawdownPct)
				wpStr := ""
				if bot.walletPct > 0 {
					wpStr = fmt.Sprintf("%.1f%%", bot.walletPct)
				}
				wlStr := fmtWinLossRatio(bot.winningTrades, bot.losingTrades)
				sb.WriteString(fmt.Sprintf("%-*s %6s %8s%5s %8s%5s %4s %4d %5s\n", catTableStrategyWidth, label, pnlStr, pctStr, maxDDStr, wpStr, bot.timeframe, bot.interval, bot.closedTrades, wlStr))
			}
			if includeTotals {
				sb.WriteString(sep + "\n")
				totPnlStr := fmtPnl(totalPnl)
				totPctStr := "—"
				totWlStr := fmtWinLossRatio(totalWins, totalLosses)
				sb.WriteString(fmt.Sprintf("%-*s %6s %8s%5s %8s%5s %4s %4d %5s\n", catTableStrategyWidth, "TOTAL", totPnlStr, totPctStr, "", "100.0%", "", "", totalClosed, totWlStr))
			}
		} else {
			header := fmt.Sprintf("%-*s %6s %8s%5s %5s %4s %4s %5s", catTableStrategyWidth, "Strategy", "PnL", "PnL%", "DD", "Tf", "Int", "#T", "W/L")
			sep := strings.Repeat("-", len(header))
			sb.WriteString(header + "\n")
			sb.WriteString(sep + "\n")
			for _, bot := range bots {
				label := summaryStrategyLabel(bot.id)
				if bot.poolBudget {
					label += "*"
				}
				pnlStr := fmtPnl(bot.pnl)
				pctStr := "—"
				wlStr := fmtWinLossRatio(bot.winningTrades, bot.losingTrades)
				maxDDStr := fmtDrawdownPct(bot.maxDrawdownPct)
				sb.WriteString(fmt.Sprintf("%-*s %6s %8s%5s %5s %4s %4d %5s\n", catTableStrategyWidth, label, pnlStr, pctStr, maxDDStr, bot.timeframe, bot.interval, bot.closedTrades, wlStr))
			}
			if includeTotals {
				sb.WriteString(sep + "\n")
				totPnlStr := fmtPnl(totalPnl)
				totPctStr := "—"
				totWlStr := fmtWinLossRatio(totalWins, totalLosses)
				sb.WriteString(fmt.Sprintf("%-*s %6s %8s%5s %5s %4s %4d %5s\n", catTableStrategyWidth, "TOTAL", totPnlStr, totPctStr, "", "", "", totalClosed, totWlStr))
			}
		}
	} else if showWalletPct {
		header := fmt.Sprintf("%-*s %6s %6s %8s%5s %8s%5s %4s %4s %5s", catTableStrategyWidth, "Strategy", "Value", "PnL", "PnL%", "DD", "Wallet%", "Tf", "Int", "#T", "W/L")
		sep := strings.Repeat("-", len(header))
		sb.WriteString(header + "\n")
		sb.WriteString(sep + "\n")
		for _, bot := range bots {
			label := summaryStrategyLabel(bot.id)
			if bot.poolBudget {
				label += "*"
			}
			valStr := fmtComma(bot.value)
			pnlStr := fmtPnl(bot.pnl)
			pctStr := fmtPnlPct(bot.pnlPct)
			if bot.poolBudget {
				pctStr = "—"
			}
			maxDDStr := fmtDrawdownPct(bot.maxDrawdownPct)
			wpStr := ""
			if bot.walletPct > 0 {
				wpStr = fmt.Sprintf("%.1f%%", bot.walletPct)
			}
			wlStr := fmtWinLossRatio(bot.winningTrades, bot.losingTrades)
			sb.WriteString(fmt.Sprintf("%-*s %6s %6s %8s%5s %8s%5s %4s %4d %5s\n", catTableStrategyWidth, label, valStr, pnlStr, pctStr, maxDDStr, wpStr, bot.timeframe, bot.interval, bot.closedTrades, wlStr))
		}
		if includeTotals {
			sb.WriteString(sep + "\n")
			totValStr := fmtComma(totalValue)
			totPnlStr := fmtPnl(totalPnl)
			totPctStr := fmtPnlPct(totalPnlPct)
			totWlStr := fmtWinLossRatio(totalWins, totalLosses)
			sb.WriteString(fmt.Sprintf("%-*s %6s %6s %8s%5s %8s%5s %4s %4d %5s\n", catTableStrategyWidth, "TOTAL", totValStr, totPnlStr, totPctStr, "", "100.0%", "", "", totalClosed, totWlStr))
		}
	} else {
		header := fmt.Sprintf("%-*s %6s %6s %8s%5s %5s %4s %4s %5s", catTableStrategyWidth, "Strategy", "Value", "PnL", "PnL%", "DD", "Tf", "Int", "#T", "W/L")
		sep := strings.Repeat("-", len(header))
		sb.WriteString(header + "\n")
		sb.WriteString(sep + "\n")
		for _, bot := range bots {
			label := summaryStrategyLabel(bot.id)
			if bot.poolBudget {
				label += "*"
			}
			valStr := fmtComma(bot.value)
			pnlStr := fmtPnl(bot.pnl)
			pctStr := fmtPnlPct(bot.pnlPct)
			if bot.poolBudget {
				pctStr = "—"
			}
			wlStr := fmtWinLossRatio(bot.winningTrades, bot.losingTrades)
			maxDDStr := fmtDrawdownPct(bot.maxDrawdownPct)
			sb.WriteString(fmt.Sprintf("%-*s %6s %6s %8s%5s %5s %4s %4d %5s\n", catTableStrategyWidth, label, valStr, pnlStr, pctStr, maxDDStr, bot.timeframe, bot.interval, bot.closedTrades, wlStr))
		}
		if includeTotals {
			sb.WriteString(sep + "\n")
			totValStr := fmtComma(totalValue)
			totPnlStr := fmtPnl(totalPnl)
			totPctStr := fmtPnlPct(totalPnlPct)
			totWlStr := fmtWinLossRatio(totalWins, totalLosses)
			sb.WriteString(fmt.Sprintf("%-*s %6s %6s %8s%5s %5s %4s %4d %5s\n", catTableStrategyWidth, "TOTAL", totValStr, totPnlStr, totPctStr, "", "", "", totalClosed, totWlStr))
		}
	}
	sb.WriteString("```\n")
}

func writeCatTableChunks(bots []botInfo, totalValue, totalPnl, totalPnlPct float64, showWalletPct, hasPoolBudget bool) []string {
	if len(bots) == 0 {
		return nil
	}
	var totalClosed, totalWins, totalLosses int
	for _, bot := range bots {
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
		writeCatTablePartial(&sb, bots[start:end], showWalletPct, isLast, totalValue, totalPnl, totalPnlPct, totalClosed, totalWins, totalLosses, hasPoolBudget)
		chunks = append(chunks, sb.String())
	}
	return chunks
}

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
	if math.IsNaN(pnl) {
		return "—"
	}
	sign := "+"
	abs := pnl
	if pnl < 0 {
		sign = "-"
		abs = -pnl
	}
	return sign + fmtComma(abs)
}

func fmtPnlPct(pct float64) string {
	if math.IsNaN(pct) {
		return "—"
	}
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

func percentFromEntry(side string, entry, target float64) float64 {
	if entry == 0 {
		return 0
	}
	pct := (target - entry) / entry * 100
	if strings.ToLower(side) == "short" {
		pct = -pct
	}
	return pct
}

func ratchetTargetPrice(side string, entry, entryATR, multiple float64) float64 {
	if strings.ToLower(side) == "short" {
		return entry - multiple*entryATR
	}
	return entry + multiple*entryATR
}

func positionMargin(qty, avgCost, leverage float64) float64 {
	if leverage <= 0 {
		return 0
	}
	return (qty * avgCost) / leverage
}

func strategyUsesTieredTPATRClose(sc StrategyConfig) bool {
	for _, ref := range sc.closeRefs() {
		if isTieredTPATRCloseName(ref.Name) {
			return true
		}
	}
	return false
}

func closeStrategySummaryName(sc StrategyConfig) string {
	if sc.CloseStrategy == nil {
		return ""
	}
	return strings.TrimSpace(sc.CloseStrategy.Name)
}

func collectPositions(sc StrategyConfig, ss *StrategyState, prices map[string]float64) []string {
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
		extras := ""
		if name := closeStrategySummaryName(sc); name != "" {
			extras += fmt.Sprintf(" | close: %s", name)
		}

		anchor := pos.riskAnchorPrice()
		if pos.EntryATR > 0 {
			extras += fmt.Sprintf(" | ATR: $%s", fmtComma2(pos.EntryATR))
		}
		if pos.ScaleInCount > 0 {
			extras += fmt.Sprintf(" | scaled-in: %d (+$%s)", pos.ScaleInCount, fmtComma2(pos.AddedNotionalUSD))
		}
		if pos.StopLossTriggerPx > 0 {
			slPct := percentFromEntry(pos.Side, anchor, pos.StopLossTriggerPx)
			if pos.StopLossATRMult != nil {
				extras += fmt.Sprintf(" | SL: $%s (%s) (%gx)", fmtComma2(pos.StopLossTriggerPx), fmtPnlPct(slPct), *pos.StopLossATRMult)
			} else {
				extras += fmt.Sprintf(" | SL: $%s (%s)", fmtComma2(pos.StopLossTriggerPx), fmtPnlPct(slPct))
			}
		}
		tiers := strategyTPTiersForRegime(sc, positionATRRegimeLabel(pos, sc))
		tps := tieredTPATRPricesFromTiers(tiers, pos.Side, anchor, pos.EntryATR)
		if len(tps) > 0 {

			partiallyClosed := pos.InitialQuantity > 0 && pos.Quantity+1e-9 < pos.InitialQuantity
			for i, tp := range tps {
				multSuffix := ""
				if i < len(tiers) {
					multSuffix = fmt.Sprintf(" (%gx)", tiers[i].Multiple)
				}
				if partiallyClosed && i < len(pos.TPOIDs) && pos.TPOIDs[i] == 0 {
					extras += fmt.Sprintf(" | TP%d: $%s%s ✓", i+1, fmtComma2(tp), multSuffix)
					continue
				}
				pct := percentFromEntry(pos.Side, anchor, tp)
				extras += fmt.Sprintf(" | TP%d: $%s (%s)%s", i+1, fmtComma2(tp), fmtPnlPct(pct), multSuffix)
			}
		}
		ratchetTiers := trailingRatchetTiersForRegime(sc, positionATRRegimeLabel(pos, sc))
		if len(tps) == 0 && len(ratchetTiers) > 0 && pos.EntryATR > 0 && pos.AvgCost > 0 {
			processed := pos.SLAdjustedTiersProcessed
			if processed < 0 {
				processed = 0
			}
			if processed > len(ratchetTiers) {
				processed = len(ratchetTiers)
			}
			if trail := effectiveTrailingRatchetMult(pos, sc); trail > 0 {
				extras += fmt.Sprintf(" | Ratchet: %d/%d | Trail: %gx ATR", processed, len(ratchetTiers), trail)
			} else {
				extras += fmt.Sprintf(" | Ratchet: %d/%d", processed, len(ratchetTiers))
			}
			for i, tier := range ratchetTiers {
				target := ratchetTargetPrice(pos.Side, anchor, pos.EntryATR, tier.ATRMultiple)
				pct := percentFromEntry(pos.Side, anchor, target)
				extras += fmt.Sprintf(" | RT%d: $%s (%s) (%gx -> %gx trail)", i+1, fmtComma2(target), fmtPnlPct(pct), tier.ATRMultiple, tier.TrailingMultAfter)
			}
		}
		if pos.Leverage > 1 {
			margin := positionMargin(pos.Quantity, pos.AvgCost, pos.Leverage)
			extras += fmt.Sprintf(" | %gx ($%s margin)", pos.Leverage, fmtComma(math.Round(margin)))
		}
		lines = append(lines, fmt.Sprintf("%s %s %s x%.3f @ $%s (%s$%s)%s%s", sc.ID, strings.ToUpper(pos.Side), sym, pos.Quantity, fmtComma2(pos.AvgCost), sign, fmtComma2(absPnl), extras, dateStr))
	}
	for key, opt := range ss.OptionPositions {
		dateStr := ""
		if !opt.OpenedAt.IsZero() {
			dateStr = fmt.Sprintf(" [%s]", opt.OpenedAt.Format("Jan 02 15:04"))
		}
		lines = append(lines, fmt.Sprintf("%s OPT %s ($%s)%s", sc.ID, key, fmtComma2(opt.CurrentValueUSD), dateStr))
	}
	return lines
}

func isTradeCloseDetails(details string) bool {
	return strings.Contains(strings.ToLower(details), "close")
}

func FormatTradeDM(sc StrategyConfig, trade Trade, mode string) string {
	isClose := isTradeCloseDetails(trade.Details)

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
	sb.WriteString(fmt.Sprintf("%s **%s - %s**\n", icon, header, strings.ToUpper(mode)))
	sb.WriteString(fmt.Sprintf("Strategy: %s (%s %s)\n", sc.ID, platformLabel, typeLabel))
	sb.WriteString(fmt.Sprintf("%s — %s %.3f @ $%s | Value: $%s", trade.Symbol, tradeDirectionLabel(trade), trade.Quantity, fmtComma(trade.Price), fmtComma(trade.Value)))
	if oid := strings.TrimSpace(trade.ExchangeOrderID); oid != "" {
		sb.WriteString(fmt.Sprintf(" | OID: %s", oid))
	}
	sb.WriteString("\n")

	if extras := tradeAlertExtras(sc, trade, isClose); len(extras) > 0 {
		sb.WriteString(strings.Join(extras, " | "))
	}

	return sb.String()
}

func tradeAlertExtras(sc StrategyConfig, trade Trade, isClose bool) []string {
	var extras []string
	if isClose {
		if src := tradeAlertCloseSource(trade.Details); src != "" {
			extras = append(extras, "Source: "+src)
		}
		if pnl, ok := extractPnL(trade.Details); ok {
			extras = append(extras, fmt.Sprintf("PnL: $%s", pnl))
		}
	}
	if trade.Regime != "" {
		extras = append(extras, "Regime: "+trade.Regime)
	}
	if trade.RegimeDivergenceNote != "" {
		extras = append(extras, trade.RegimeDivergenceNote)
	}
	if trade.RegimeProfileNote != "" {
		extras = append(extras, trade.RegimeProfileNote)
	}
	direction := strings.ToLower(tradeDirectionLabel(trade))
	var tiers []hlProtectionTier
	var tps []float64
	if !isClose && trade.EntryATR > 0 {
		tiers = strategyTPTiersForRegime(sc, trade.Regime)
		tps = tieredTPATRPricesFromTiers(tiers, direction, trade.Price, trade.EntryATR)
		if len(tps) > 0 {
			extras = append(extras, fmt.Sprintf("ATR: $%s", fmtComma2(trade.EntryATR)))
		}
	}
	if trade.StopLossTriggerPx > 0 {
		slPct := percentFromEntry(direction, trade.Price, trade.StopLossTriggerPx)
		if trade.StopLossATRMult != nil {
			extras = append(extras, fmt.Sprintf("SL: $%s (%s) (%gx)", fmtComma2(trade.StopLossTriggerPx), fmtPnlPct(slPct), *trade.StopLossATRMult))
		} else {
			extras = append(extras, fmt.Sprintf("SL: $%s (%s)", fmtComma2(trade.StopLossTriggerPx), fmtPnlPct(slPct)))
		}
	}
	for i, tp := range tps {
		extras = append(extras, fmt.Sprintf("TP%d: $%s (%gx)", i+1, fmtComma2(tp), tiers[i].Multiple))
	}
	return extras
}

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

func tradeAlertCloseSource(details string) string {
	d := strings.ToLower(details)
	switch {

	case strings.Contains(d, "liquidation-clamp sl close"):
		return "liquidation-clamp SL"

	case strings.Contains(d, "paper trailing sl close"):
		return "paper trailing SL"
	case strings.Contains(d, "trailing sl close"):
		return "trailing SL"
	case strings.Contains(d, "paper sl close"):
		return "paper SL"
	case strings.Contains(d, "stop loss close"):
		return "exchange SL"
	case strings.HasPrefix(d, "tp") && strings.Contains(d, "fill close"):

		end := strings.Index(d, " ")
		if end > 0 {
			return "exchange " + strings.ToUpper(details[:end])
		}
		return "exchange TP"
	case strings.Contains(d, "circuit breaker"):
		return "circuit breaker"
	case strings.Contains(d, "external partial close"):
		return "external (peer / manual UI, partial)"
	case strings.Contains(d, "external close"):
		return "external (peer / manual UI)"
	case strings.HasPrefix(d, "close long") || strings.HasPrefix(d, "close short"):
		return "close-strategy exit"
	}
	return ""
}

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

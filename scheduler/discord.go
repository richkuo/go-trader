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

// FormatCategorySummary creates a Discord message for a set of strategies sharing a channel.
// channelStrategies is pre-filtered by the caller; channelKey is the display label.
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
) string {
	var sb strings.Builder

	// Icon and title based on strategy types and channel key.
	icon := "📊"
	if isOptionsType(channelStrategies) {
		icon = "🎯"
	} else if channelKey == "spot" {
		icon = "📈"
	}
	title := strings.ToUpper(channelKey[:1]) + channelKey[1:]
	if totalTrades > 0 {
		sb.WriteString(fmt.Sprintf("%s **%s TRADES**\n", icon, strings.ToUpper(title)))
	} else {
		sb.WriteString(fmt.Sprintf("%s **%s Summary**\n", icon, title))
	}

	sb.WriteString(fmt.Sprintf("Cycle #%d | %.1fs\n", cycle, elapsed.Seconds()))

	// Prices inline
	if len(prices) > 0 {
		syms := make([]string, 0, len(prices))
		for s := range prices {
			syms = append(syms, s)
		}
		sort.Strings(syms)
		parts := make([]string, 0, len(syms))
		for _, sym := range syms {
			short := strings.TrimSuffix(sym, "/USDT")
			parts = append(parts, fmt.Sprintf("%s $%.0f", short, prices[sym]))
		}
		sb.WriteString(strings.Join(parts, " | "))
		sb.WriteString("\n")
	}

	// Build flat bot list from the provided channel strategies.
	var tableBots []botInfo
	var totalCap, filteredValue float64
	for _, sc := range channelStrategies {
		ss := state.Strategies[sc.ID]
		if ss == nil {
			continue
		}
		pv := PortfolioValue(ss, prices)
		totalCap += sc.Capital
		filteredValue += pv
		pnl := pv - sc.Capital
		openPos := len(ss.Positions) + len(ss.OptionPositions)
		stratName := extractStrategyName(sc)
		pnlPct := 0.0
		if sc.Capital > 0 {
			pnlPct = (pnl / sc.Capital) * 100
		}
		asset := extractAsset(sc)
		tableBots = append(tableBots, botInfo{
			id:            sc.ID,
			strategy:      stratName,
			asset:         asset,
			value:         pv,
			pnl:           pnl,
			pnlPct:        pnlPct,
			trades:        len(ss.TradeHistory),
			openPositions: openPos,
			closedTrades:  ss.RiskState.TotalTrades,
			tradeHistory:  ss.TradeHistory,
		})
	}

	totalPnl := filteredValue - totalCap
	totalPnlPct := 0.0
	if totalCap > 0 {
		totalPnlPct = (totalPnl / totalCap) * 100
	}
	writeCatTable(&sb, tableBots, filteredValue, totalPnl, totalPnlPct)

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
	pnl           float64
	pnlPct        float64
	trades        int
	openPositions int
	closedTrades  int
	tradeHistory  []Trade
}

func extractStrategyName(sc StrategyConfig) string {
	if sc.Type == "options" && len(sc.Args) > 0 {
		return sc.Args[0]
	}
	// For spot, extract from ID (e.g., "momentum-btc" -> "momentum")
	parts := strings.Split(sc.ID, "-")
	if len(parts) > 0 {
		return parts[0]
	}
	return "unknown"
}

func extractAsset(sc StrategyConfig) string {
	// Extract asset from strategy ID
	// Examples: "momentum-btc" -> "BTC", "deribit-vol-eth" -> "ETH", "ibkr-wheel-btc" -> "BTC"
	parts := strings.Split(sc.ID, "-")
	if len(parts) > 0 {
		lastPart := strings.ToUpper(parts[len(parts)-1])
		// Check if it's a known asset
		if lastPart == "BTC" || lastPart == "ETH" || lastPart == "SOL" {
			return lastPart
		}
	}
	// For options, try args
	if sc.Type == "options" && len(sc.Args) > 1 {
		return strings.ToUpper(sc.Args[1])
	}
	return ""
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
func writeCatTable(sb *strings.Builder, bots []botInfo, totalValue, totalPnl, totalPnlPct float64) {
	if len(bots) == 0 {
		return
	}
	const sep = "-----------------------------------------------"
	sb.WriteString("\n```\n")
	sb.WriteString(fmt.Sprintf("%-20s %10s %10s %7s\n", "Strategy", "Value", "PnL", "PnL%"))
	sb.WriteString(sep + "\n")
	for _, bot := range bots {
		label := bot.id
		if len(label) > 20 {
			label = label[:20]
		}
		valStr := "$ " + fmtComma(bot.value)
		pnlSign := "+"
		absPnl := bot.pnl
		if bot.pnl < 0 {
			pnlSign = "-"
			absPnl = -bot.pnl
		}
		pnlStr := "$ " + pnlSign + fmtComma(absPnl)
		pctSign := "+"
		if bot.pnlPct < 0 {
			pctSign = ""
		}
		pctStr := fmt.Sprintf("%s%.1f%%", pctSign, bot.pnlPct)
		sb.WriteString(fmt.Sprintf("%-20s %10s %10s %7s\n", label, valStr, pnlStr, pctStr))
	}
	sb.WriteString(sep + "\n")
	// TOTAL row
	totValStr := "$ " + fmtComma(totalValue)
	totPnlSign := "+"
	absTotPnl := totalPnl
	if totalPnl < 0 {
		totPnlSign = "-"
		absTotPnl = -totalPnl
	}
	totPnlStr := "$ " + totPnlSign + fmtComma(absTotPnl)
	totPctSign := "+"
	if totalPnlPct < 0 {
		totPctSign = ""
	}
	totPctStr := fmt.Sprintf("%s%.1f%%", totPctSign, totalPnlPct)
	sb.WriteString(fmt.Sprintf("%-20s %10s %10s %7s\n", "TOTAL", totValStr, totPnlStr, totPctStr))
	sb.WriteString("```\n")
}

// collectPositions returns human-readable position lines for a strategy (used by trade alerts)
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
		lines = append(lines, fmt.Sprintf("%s %s %s (%s$%.0f)", stratID, strings.ToUpper(pos.Side), sym, sign, pnl))
	}
	for key, opt := range ss.OptionPositions {
		lines = append(lines, fmt.Sprintf("%s OPT %s ($%.0f)", stratID, key, opt.CurrentValueUSD))
	}
	return lines
}

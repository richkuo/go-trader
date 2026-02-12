package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"
)

const (
	discordAPIBase = "https://discord.com/api/v10"
	tradingChannel = "1471072739472183430"
)

type DiscordNotifier struct {
	Token     string
	ChannelID string
	client    *http.Client
}

func NewDiscordNotifier(token, channelID string) *DiscordNotifier {
	return &DiscordNotifier{
		Token:     token,
		ChannelID: channelID,
		client:    &http.Client{Timeout: 10 * time.Second},
	}
}

func (d *DiscordNotifier) SendMessage(content string) error {
	// Discord has a 2000 character limit, truncate if needed
	if len(content) > 2000 {
		content = content[:1997] + "..."
	}
	
	url := fmt.Sprintf("%s/channels/%s/messages", discordAPIBase, d.ChannelID)

	payload := map[string]string{"content": content}
	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshal: %w", err)
	}

	req, err := http.NewRequest("POST", url, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("request: %w", err)
	}

	req.Header.Set("Authorization", "Bot "+d.Token)
	req.Header.Set("Content-Type", "application/json")

	resp, err := d.client.Do(req)
	if err != nil {
		return fmt.Errorf("send: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		return fmt.Errorf("discord API error: %d", resp.StatusCode)
	}
	return nil
}

// stratCategory returns "spot", "deribit", or "ibkr" based on strategy ID
func stratCategory(id string) string {
	if strings.HasPrefix(id, "deribit-") {
		return "deribit"
	}
	if strings.HasPrefix(id, "ibkr-") {
		return "ibkr"
	}
	return "spot"
}

// FormatCategorySummary creates a Discord message for specific categories (spot or options)
func FormatCategorySummary(
	cycle int,
	elapsed time.Duration,
	strategiesRun int,
	totalTrades int,
	totalValue float64,
	prices map[string]float64,
	tradeDetails []string,
	strategies []StrategyConfig,
	state *AppState,
	categoryFilter string, // "spot" or "options"
) string {
	var sb strings.Builder

	// Title based on category
	if categoryFilter == "spot" {
		if totalTrades > 0 {
			sb.WriteString("📈 **SPOT TRADES**\n")
		} else {
			sb.WriteString("📈 **Spot Summary**\n")
		}
	} else {
		if totalTrades > 0 {
			sb.WriteString("🎯 **OPTIONS TRADES**\n")
		} else {
			sb.WriteString("🎯 **Options Summary**\n")
		}
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

	// Split strategies into spot / deribit / ibkr (filtered by categoryFilter)
	cats := map[string]*catInfo{
		"spot":    {bots: []botInfo{}},
		"deribit": {bots: []botInfo{}},
		"ibkr":    {bots: []botInfo{}},
	}

	for _, sc := range strategies {
		ss := state.Strategies[sc.ID]
		if ss == nil {
			continue
		}
		cat := stratCategory(sc.ID)
		
		// Filter based on categoryFilter
		if categoryFilter == "spot" && cat != "spot" {
			continue
		}
		if categoryFilter == "options" && cat == "spot" {
			continue
		}
		
		ci := cats[cat]
		ci.count++
		ci.capital += sc.Capital
		pv := PortfolioValue(ss, prices)
		ci.value += pv
		pnl := pv - sc.Capital
		ci.pnl += pnl
		ci.posCount += len(ss.Positions) + len(ss.OptionPositions)
		
		// Extract strategy name from args or ID
		stratName := extractStrategyName(sc)
		pnlPct := 0.0
		if sc.Capital > 0 {
			pnlPct = (pnl / sc.Capital) * 100
		}
		
		asset := extractAsset(sc)
		ci.bots = append(ci.bots, botInfo{
			id:           sc.ID,
			strategy:     stratName,
			asset:        asset,
			pnlPct:       pnlPct,
			trades:       len(ss.TradeHistory),
			tradeHistory: ss.TradeHistory,
		})
	}

	// Category lines with bot details (filtered)
	if categoryFilter == "spot" {
		writeCatLineDetailed(&sb, "📈 Spot", cats["spot"])
	} else {
		writeCatLineDetailed(&sb, "🎯 Deribit", cats["deribit"])
		writeCatLineDetailed(&sb, "🏦 IBKR", cats["ibkr"])
	}

	// Total for filtered categories
	var totalCap float64
	if categoryFilter == "spot" {
		totalCap = cats["spot"].capital
	} else {
		totalCap = cats["deribit"].capital + cats["ibkr"].capital
	}
	
	// Calculate filtered total value
	var filteredValue float64
	if categoryFilter == "spot" {
		filteredValue = cats["spot"].value
	} else {
		filteredValue = cats["deribit"].value + cats["ibkr"].value
	}
	
	totalPnl := filteredValue - totalCap
	pnlPct := 0.0
	if totalCap > 0 {
		pnlPct = (totalPnl / totalCap) * 100
	}
	pnlSign := "+"
	if totalPnl < 0 {
		pnlSign = ""
	}
	sb.WriteString(fmt.Sprintf("\n**Starting: $%.0f → Current: $%.0f** (%s$%.0f / %s%.1f%%)\n",
		totalCap, filteredValue, pnlSign, totalPnl, pnlSign, pnlPct))

	// Trade details (always shown)
	if len(tradeDetails) > 0 {
		sb.WriteString("\n**Trades:**\n")
		for _, td := range tradeDetails {
			sb.WriteString(fmt.Sprintf("• %s\n", td))
		}
	}

	return sb.String()
}

type catInfo struct {
	value    float64
	count    int
	posCount int
	pnl      float64
	capital  float64
	bots     []botInfo
}

type botInfo struct {
	id           string
	strategy     string
	asset        string
	pnlPct       float64
	trades       int
	tradeHistory []Trade
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

func writeCatLineDetailed(sb *strings.Builder, label string, ci *catInfo) {
	if ci.count == 0 {
		return
	}
	pnlSign := "+"
	if ci.pnl < 0 {
		pnlSign = ""
	}
	pnlPct := 0.0
	if ci.capital > 0 {
		pnlPct = (ci.pnl / ci.capital) * 100
	}
	
	// Category header
	sb.WriteString(fmt.Sprintf("\n%s: **$%.0f → $%.0f** (%s$%.0f / %s%.1f%%)\n",
		label, ci.capital, ci.value, pnlSign, ci.pnl, pnlSign, pnlPct))
	
	// Individual bots
	for _, bot := range ci.bots {
		sign := "+"
		if bot.pnlPct < 0 {
			sign = ""
		}
		assetLabel := ""
		if bot.asset != "" {
			assetLabel = bot.asset + " "
		}
		sb.WriteString(fmt.Sprintf("  • %s%s (%s%.1f%%) — %d trades\n", assetLabel, bot.strategy, sign, bot.pnlPct, bot.trades))
		
		// Show last 3 trades only (to keep message under 2000 char Discord limit)
		if len(bot.tradeHistory) > 0 {
			start := 0
			if len(bot.tradeHistory) > 3 {
				start = len(bot.tradeHistory) - 3
			}
			for i := start; i < len(bot.tradeHistory); i++ {
				trade := bot.tradeHistory[i]
				sb.WriteString(fmt.Sprintf("    - %s %s @ $%.0f (%s)\n",
					strings.ToUpper(trade.Side),
					trade.Symbol,
					trade.Price,
					trade.Timestamp.Format("Jan 02 15:04")))
			}
		}
	}
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

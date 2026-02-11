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

// FormatCycleSummary creates a Discord message from cycle results
func FormatCycleSummary(
	cycle int,
	elapsed time.Duration,
	strategiesRun int,
	totalTrades int,
	totalValue float64,
	prices map[string]float64,
	tradeDetails []string,
	strategies []StrategyConfig,
	state *AppState,
) string {
	var sb strings.Builder

	if totalTrades > 0 {
		sb.WriteString("🚨 **TRADES EXECUTED**\n")
	} else {
		sb.WriteString("📊 **Cycle Summary**\n")
	}

	sb.WriteString(fmt.Sprintf("Cycle #%d | %d strategies | %.1fs\n", cycle, strategiesRun, elapsed.Seconds()))

	// Prices
	if len(prices) > 0 {
		sb.WriteString("```\n")
		syms := make([]string, 0, len(prices))
		for s := range prices {
			syms = append(syms, s)
		}
		sort.Strings(syms)
		for _, sym := range syms {
			sb.WriteString(fmt.Sprintf("%-10s $%.2f\n", sym, prices[sym]))
		}
		sb.WriteString("```\n")
	}

	// Split strategies into spot / deribit / ibkr
	cats := map[string]*catData{
		"spot":    {},
		"deribit": {},
		"ibkr":    {},
	}

	for _, sc := range strategies {
		ss := state.Strategies[sc.ID]
		if ss == nil {
			continue
		}
		cat := stratCategory(sc.ID)
		cd := cats[cat]
		cd.count++
		pv := PortfolioValue(ss, prices)
		cd.value += pv
		cd.positions = append(cd.positions, collectPositions(sc.ID, ss, prices)...)
	}

	// Spot
	writeCatSection(&sb, "📈 Spot", cats["spot"])
	// Deribit Options
	writeCatSection(&sb, "🎯 Deribit Options", cats["deribit"])
	// IBKR Options
	writeCatSection(&sb, "🏦 IBKR Options", cats["ibkr"])

	// Total
	sb.WriteString(fmt.Sprintf("\n**Total: $%.2f** | Trades: **%d**\n", totalValue, totalTrades))

	// Trade details
	if len(tradeDetails) > 0 {
		sb.WriteString("\n**Trades:**\n")
		for _, td := range tradeDetails {
			sb.WriteString(fmt.Sprintf("• %s\n", td))
		}
	}

	return sb.String()
}

type catData struct {
	value     float64
	positions []string
	count     int
}

func writeCatSection(sb *strings.Builder, label string, cd *catData) {
	sb.WriteString(fmt.Sprintf("\n%s (%d bots): **$%.2f**\n", label, cd.count, cd.value))
	if len(cd.positions) > 0 {
		for _, p := range cd.positions {
			sb.WriteString(fmt.Sprintf("• %s\n", p))
		}
	} else {
		sb.WriteString("No open positions\n")
	}
}

// collectPositions returns human-readable position lines for a strategy
func collectPositions(stratID string, ss *StrategyState, prices map[string]float64) []string {
	var lines []string

	for sym, pos := range ss.Positions {
		currentPrice := prices[sym]
		if currentPrice == 0 {
			currentPrice = pos.AvgCost
		}
		value := pos.Quantity * currentPrice
		pnl := 0.0
		if pos.Side == "long" {
			pnl = pos.Quantity * (currentPrice - pos.AvgCost)
		} else {
			pnl = pos.Quantity * (pos.AvgCost - currentPrice)
		}
		pnlSign := "+"
		if pnl < 0 {
			pnlSign = ""
		}
		lines = append(lines, fmt.Sprintf("[%s] %s %s %.6f @ $%.2f → $%.2f (%s$%.2f)",
			stratID, strings.ToUpper(pos.Side), sym, pos.Quantity, pos.AvgCost, value, pnlSign, pnl))
	}

	for key, opt := range ss.OptionPositions {
		lines = append(lines, fmt.Sprintf("[%s] OPT %s (val: $%.2f)", stratID, key, opt.CurrentValueUSD))
	}

	return lines
}

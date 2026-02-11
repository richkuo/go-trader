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

	// Split strategies into spot / deribit / ibkr
	cats := map[string]*catInfo{
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
		ci := cats[cat]
		ci.count++
		ci.capital += sc.Capital
		pv := PortfolioValue(ss, prices)
		ci.value += pv
		ci.pnl += pv - sc.Capital
		ci.posCount += len(ss.Positions) + len(ss.OptionPositions)
	}

	// Compact category lines
	writeCatLine(&sb, "📈 Spot", cats["spot"])
	writeCatLine(&sb, "🎯 Deribit", cats["deribit"])
	writeCatLine(&sb, "🏦 IBKR", cats["ibkr"])

	// Total
	totalCap := cats["spot"].capital + cats["deribit"].capital + cats["ibkr"].capital
	totalPnl := totalValue - totalCap
	pnlPct := 0.0
	if totalCap > 0 {
		pnlPct = (totalPnl / totalCap) * 100
	}
	pnlSign := "+"
	if totalPnl < 0 {
		pnlSign = ""
	}
	sb.WriteString(fmt.Sprintf("\n**Total: $%.0f** (%s$%.0f / %s%.1f%%) | Trades: **%d**\n",
		totalValue, pnlSign, totalPnl, pnlSign, pnlPct, totalTrades))

	// Trade details (always shown)
	if len(tradeDetails) > 0 {
		sb.WriteString("\n**Trades:**\n")
		for _, td := range tradeDetails {
			sb.WriteString(fmt.Sprintf("• %s\n", td))
		}
	}

	return sb.String()
}

func writeCatLine(sb *strings.Builder, label string, ci *catInfo) {
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
	sb.WriteString(fmt.Sprintf("%s: **$%.0f** (%s$%.0f / %s%.1f%%) — %d bots, %d positions\n",
		label, ci.value, pnlSign, ci.pnl, pnlSign, pnlPct, ci.count, ci.posCount))
}

type catInfo struct {
	value    float64
	count    int
	posCount int
	pnl      float64
	capital  float64
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

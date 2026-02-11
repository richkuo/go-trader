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

// isSmallBot returns true if a strategy is a "$200 bot" (capital < 500)
func isSmallBot(cfg StrategyConfig) bool {
	return cfg.Capital < 500
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

	// Split strategies into main and $200 bots
	var mainValue, smallValue float64
	var mainPositions, smallPositions []string

	for _, sc := range strategies {
		ss := state.Strategies[sc.ID]
		if ss == nil {
			continue
		}
		pv := PortfolioValue(ss, prices)
		posLines := collectPositions(sc.ID, ss, prices)

		if isSmallBot(sc) {
			smallValue += pv
			smallPositions = append(smallPositions, posLines...)
		} else {
			mainValue += pv
			mainPositions = append(mainPositions, posLines...)
		}
	}

	// Main portfolio
	sb.WriteString(fmt.Sprintf("\n💰 **Main Portfolio** ($1K bots): **$%.2f**\n", mainValue))
	if len(mainPositions) > 0 {
		sb.WriteString("**Open Positions:**\n")
		for _, p := range mainPositions {
			sb.WriteString(fmt.Sprintf("• %s\n", p))
		}
	} else {
		sb.WriteString("No open positions\n")
	}

	// $200 bots
	sb.WriteString(fmt.Sprintf("\n🪙 **$200 Bots**: **$%.2f**\n", smallValue))
	if len(smallPositions) > 0 {
		sb.WriteString("**Open Positions:**\n")
		for _, p := range smallPositions {
			sb.WriteString(fmt.Sprintf("• %s\n", p))
		}
	} else {
		sb.WriteString("No open positions\n")
	}

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

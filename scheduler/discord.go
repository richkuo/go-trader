package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
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

// FormatCycleSummary creates a Discord message from cycle results
func FormatCycleSummary(cycle int, elapsed time.Duration, strategiesRun int, totalTrades int, totalValue float64, prices map[string]float64, tradeDetails []string) string {
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
		for sym, price := range prices {
			sb.WriteString(fmt.Sprintf("%-10s $%.2f\n", sym, price))
		}
		sb.WriteString("```\n")
	}

	// Portfolio
	sb.WriteString(fmt.Sprintf("💰 Portfolio: **$%.2f** | Trades: **%d**\n", totalValue, totalTrades))

	// Trade details
	if len(tradeDetails) > 0 {
		sb.WriteString("\n**Trades:**\n")
		for _, td := range tradeDetails {
			sb.WriteString(fmt.Sprintf("• %s\n", td))
		}
	}

	return sb.String()
}

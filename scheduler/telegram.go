package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

const telegramAPIBase = "https://api.telegram.org/bot"
const telegramMaxMessageLen = 4096

// TelegramNotifier implements Notifier using the Telegram Bot API.
type TelegramNotifier struct {
	botToken    string
	ownerChatID string
	client      *http.Client
	baseURL     string // API base URL (defaults to telegramAPIBase)
	lastUpdate  int64  // offset for getUpdates polling
	mu          sync.Mutex
	closed      bool
}

// NewTelegramNotifier creates a new Telegram bot notifier.
func NewTelegramNotifier(botToken, ownerChatID string) (*TelegramNotifier, error) {
	t := &TelegramNotifier{
		botToken:    botToken,
		ownerChatID: ownerChatID,
		client:      &http.Client{Timeout: 35 * time.Second},
		baseURL:     telegramAPIBase,
	}

	// Verify the bot token with a getMe call.
	resp, err := t.apiCall("getMe", nil)
	if err != nil {
		return nil, fmt.Errorf("telegram getMe failed: %w", err)
	}
	if !resp.OK {
		return nil, fmt.Errorf("telegram getMe: %s", resp.Description)
	}

	return t, nil
}

// telegramResponse is the generic Telegram Bot API response envelope.
type telegramResponse struct {
	OK          bool            `json:"ok"`
	Description string          `json:"description,omitempty"`
	Result      json.RawMessage `json:"result,omitempty"`
}

// telegramUpdate represents a single update from getUpdates.
type telegramUpdate struct {
	UpdateID int64           `json:"update_id"`
	Message  *telegramMsg    `json:"message,omitempty"`
	Callback *telegramCBData `json:"callback_query,omitempty"`
}

type telegramMsg struct {
	MessageID int64         `json:"message_id"`
	From      *telegramUser `json:"from,omitempty"`
	Chat      telegramChat  `json:"chat"`
	Date      int64         `json:"date"`
	Text      string        `json:"text"`
}

type telegramUser struct {
	ID int64 `json:"id"`
}

type telegramChat struct {
	ID int64 `json:"id"`
}

type telegramCBData struct {
	ID      string        `json:"id"`
	From    *telegramUser `json:"from,omitempty"`
	Message *telegramMsg  `json:"message,omitempty"`
	Data    string        `json:"data"`
}

// apiCall makes a POST request to the Telegram Bot API.
func (t *TelegramNotifier) apiCall(method string, payload interface{}) (*telegramResponse, error) {
	url := t.baseURL + t.botToken + "/" + method

	var body io.Reader
	if payload != nil {
		data, err := json.Marshal(payload)
		if err != nil {
			return nil, fmt.Errorf("marshal payload: %w", err)
		}
		body = bytes.NewReader(data)
	}

	req, err := http.NewRequest("POST", url, body)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := t.client.Do(req)
	if err != nil {
		// Redact bot token from error to prevent leaking in logs.
		safeMsg := strings.ReplaceAll(err.Error(), t.botToken, "[REDACTED]")
		return nil, fmt.Errorf("telegram %s: %s", method, safeMsg)
	}
	defer resp.Body.Close()

	var result telegramResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}
	return &result, nil
}

// SendMessage sends a message to a Telegram chat. Truncates to 4096 chars.
func (t *TelegramNotifier) SendMessage(chatID string, content string) error {
	if len(content) > telegramMaxMessageLen {
		content = content[:telegramMaxMessageLen-3] + "..."
	}

	payload := map[string]interface{}{
		"chat_id": chatID,
		"text":    content,
	}

	resp, err := t.apiCall("sendMessage", payload)
	if err != nil {
		return fmt.Errorf("telegram sendMessage: %w", err)
	}
	if !resp.OK {
		return fmt.Errorf("telegram sendMessage: %s", resp.Description)
	}
	return nil
}

// SendDM sends a direct message to a user via their chat ID.
// In Telegram, DMs and channel messages use the same sendMessage API.
func (t *TelegramNotifier) SendDM(userID, content string) error {
	return t.SendMessage(userID, content)
}

// AskDM sends a question to the user and polls for a reply within the timeout.
func (t *TelegramNotifier) AskDM(userID, question string, timeout time.Duration) (string, error) {
	sentAt := time.Now().Unix()

	if err := t.SendDM(userID, question); err != nil {
		return "", fmt.Errorf("send question: %w", err)
	}

	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		t.mu.Lock()
		if t.closed {
			t.mu.Unlock()
			return "", ErrDMTimeout
		}
		t.mu.Unlock()

		remaining := time.Until(deadline)
		pollTimeout := 10 // seconds for long-polling
		if remaining < time.Duration(pollTimeout)*time.Second {
			pollTimeout = int(remaining.Seconds())
			if pollTimeout < 1 {
				pollTimeout = 1
			}
		}

		updates, err := t.getUpdates(pollTimeout)
		if err != nil {
			// Transient error — retry after a short wait.
			time.Sleep(1 * time.Second)
			continue
		}

		for _, u := range updates {
			if u.Message != nil && u.Message.From != nil {
				fromID := fmt.Sprintf("%d", u.Message.From.ID)
				if fromID == userID && u.Message.Date >= sentAt-2 {
					return strings.TrimSpace(u.Message.Text), nil
				}
			}
		}
	}

	return "", ErrDMTimeout
}

// getUpdates polls for new messages using Telegram long polling.
func (t *TelegramNotifier) getUpdates(timeoutSec int) ([]telegramUpdate, error) {
	payload := map[string]interface{}{
		"timeout": timeoutSec,
	}
	t.mu.Lock()
	if t.lastUpdate > 0 {
		payload["offset"] = t.lastUpdate + 1
	}
	t.mu.Unlock()

	resp, err := t.apiCall("getUpdates", payload)
	if err != nil {
		return nil, err
	}
	if !resp.OK {
		return nil, fmt.Errorf("getUpdates: %s", resp.Description)
	}

	var updates []telegramUpdate
	if err := json.Unmarshal(resp.Result, &updates); err != nil {
		return nil, fmt.Errorf("unmarshal updates: %w", err)
	}

	t.mu.Lock()
	for _, u := range updates {
		if u.UpdateID > t.lastUpdate {
			t.lastUpdate = u.UpdateID
		}
	}
	t.mu.Unlock()

	return updates, nil
}

// Close marks the notifier as closed and stops any pending polling.
func (t *TelegramNotifier) Close() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.closed = true
}

// FormatTradeDMPlain formats a Trade into a plain-text DM (no Discord markdown).
func FormatTradeDMPlain(sc StrategyConfig, trade Trade, mode string) string {
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
	sb.WriteString(fmt.Sprintf("%s %s\n", icon, header))
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

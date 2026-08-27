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

type TelegramNotifier struct {
	botToken    string
	ownerChatID string
	client      *http.Client
	baseURL     string
	lastUpdate  int64
	mu          sync.Mutex
	closed      bool
}

func NewTelegramNotifier(botToken, ownerChatID string) (*TelegramNotifier, error) {
	t := &TelegramNotifier{
		botToken:    botToken,
		ownerChatID: ownerChatID,
		client:      &http.Client{Timeout: 35 * time.Second},
		baseURL:     telegramAPIBase,
	}

	resp, err := t.apiCall("getMe", nil)
	if err != nil {
		return nil, fmt.Errorf("telegram getMe failed: %w", err)
	}
	if !resp.OK {
		return nil, fmt.Errorf("telegram getMe: %s", resp.Description)
	}

	return t, nil
}

type telegramResponse struct {
	OK          bool            `json:"ok"`
	Description string          `json:"description,omitempty"`
	Result      json.RawMessage `json:"result,omitempty"`
}

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

func (t *TelegramNotifier) SendDM(userID, content string) error {
	return t.SendMessage(userID, content)
}

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
		pollTimeout := 10
		if remaining < time.Duration(pollTimeout)*time.Second {
			pollTimeout = int(remaining.Seconds())
			if pollTimeout < 1 {
				pollTimeout = 1
			}
		}

		updates, err := t.getUpdates(pollTimeout)
		if err != nil {

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

func (t *TelegramNotifier) Close() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.closed = true
}

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
	sb.WriteString(fmt.Sprintf("%s %s - %s\n", icon, header, strings.ToUpper(mode)))
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

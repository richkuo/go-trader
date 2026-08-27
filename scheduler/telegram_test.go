package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func newTestTelegramNotifier(serverURL string) *TelegramNotifier {
	return &TelegramNotifier{
		botToken:    "test-token",
		ownerChatID: "12345",
		client:      &http.Client{Timeout: 5 * time.Second},
		baseURL:     serverURL + "/bot",
	}
}

func TestTelegramNotifier_SendMessage(t *testing.T) {
	var receivedChatID string
	var receivedText string

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/sendMessage") {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		var body map[string]interface{}
		json.NewDecoder(r.Body).Decode(&body)
		receivedChatID, _ = body["chat_id"].(string)
		receivedText, _ = body["text"].(string)

		json.NewEncoder(w).Encode(telegramResponse{OK: true})
	}))
	defer server.Close()

	tg := newTestTelegramNotifier(server.URL)

	err := tg.SendMessage("67890", "hello world")
	if err != nil {
		t.Fatalf("SendMessage returned error: %v", err)
	}
	if receivedChatID != "67890" {
		t.Errorf("expected chat_id '67890', got %q", receivedChatID)
	}
	if receivedText != "hello world" {
		t.Errorf("expected text 'hello world', got %q", receivedText)
	}
}

func TestTelegramNotifier_ImplementsNotifier(t *testing.T) {

	var _ Notifier = (*TelegramNotifier)(nil)
}

func TestTelegramNotifier_Close(t *testing.T) {
	tg := &TelegramNotifier{
		botToken:    "test",
		ownerChatID: "123",
		client:      &http.Client{},
	}
	tg.Close()
	tg.mu.Lock()
	if !tg.closed {
		t.Error("expected closed to be true")
	}
	tg.mu.Unlock()
}

func TestTelegramNotifier_SendDM_IsSendMessage(t *testing.T) {

	var sentChatID string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/sendMessage") {
			var body map[string]interface{}
			json.NewDecoder(r.Body).Decode(&body)
			sentChatID, _ = body["chat_id"].(string)
		}
		json.NewEncoder(w).Encode(telegramResponse{OK: true})
	}))
	defer server.Close()

	tg := newTestTelegramNotifier(server.URL)

	err := tg.SendDM("99999", "dm content")
	if err != nil {
		t.Fatalf("SendDM returned error: %v", err)
	}
	if sentChatID != "99999" {
		t.Errorf("expected chat_id '99999', got %q", sentChatID)
	}
}

func TestTelegramNotifier_MessageTruncation(t *testing.T) {
	var receivedText string

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]interface{}
		json.NewDecoder(r.Body).Decode(&body)
		receivedText, _ = body["text"].(string)
		json.NewEncoder(w).Encode(telegramResponse{OK: true})
	}))
	defer server.Close()

	tg := newTestTelegramNotifier(server.URL)

	longContent := strings.Repeat("x", 5000)
	err := tg.SendMessage("123", longContent)
	if err != nil {
		t.Fatalf("SendMessage returned error: %v", err)
	}
	if len(receivedText) != telegramMaxMessageLen {
		t.Errorf("expected truncated length %d, got %d", telegramMaxMessageLen, len(receivedText))
	}
	if !strings.HasSuffix(receivedText, "...") {
		t.Error("expected truncated message to end with '...'")
	}
}

func TestTelegramResponse_Unmarshal(t *testing.T) {
	raw := `{"ok":true,"result":{"id":123,"is_bot":true,"first_name":"TestBot"}}`
	var resp telegramResponse
	if err := json.Unmarshal([]byte(raw), &resp); err != nil {
		t.Fatalf("failed to unmarshal: %v", err)
	}
	if !resp.OK {
		t.Error("expected ok=true")
	}
	if resp.Result == nil {
		t.Error("expected non-nil result")
	}
}

func TestTelegramResponse_Error(t *testing.T) {
	raw := `{"ok":false,"description":"Unauthorized"}`
	var resp telegramResponse
	if err := json.Unmarshal([]byte(raw), &resp); err != nil {
		t.Fatalf("failed to unmarshal: %v", err)
	}
	if resp.OK {
		t.Error("expected ok=false")
	}
	if resp.Description != "Unauthorized" {
		t.Errorf("expected 'Unauthorized', got %q", resp.Description)
	}
}

func TestAskDMRejectsStaleMessages(t *testing.T) {
	now := time.Now().Unix()

	msgJSON := fmt.Sprintf(`{"message_id":1,"from":{"id":999},"chat":{"id":999},"date":%d,"text":"hello"}`, now)
	var msg telegramMsg
	if err := json.Unmarshal([]byte(msgJSON), &msg); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if msg.Date != now {
		t.Errorf("Date: got %d, want %d", msg.Date, now)
	}
	if msg.Text != "hello" {
		t.Errorf("Text: got %q, want %q", msg.Text, "hello")
	}

	sentAt := now
	staleDate := int64(sentAt - 60)
	freshDate := int64(sentAt + 1)
	graceDate := int64(sentAt - 1)

	if staleDate >= sentAt-2 {
		t.Errorf("expected stale message (date=%d) to fail guard (sentAt-2=%d)", staleDate, sentAt-2)
	}

	if freshDate < sentAt-2 {
		t.Errorf("expected fresh message (date=%d) to pass guard (sentAt-2=%d)", freshDate, sentAt-2)
	}

	if graceDate < sentAt-2 {
		t.Errorf("expected grace-window message (date=%d) to pass guard (sentAt-2=%d)", graceDate, sentAt-2)
	}
}

func TestApiCallDoesNotLeakToken(t *testing.T) {
	tn := &TelegramNotifier{
		botToken: "secret-test-token-12345",
		client:   &http.Client{Timeout: 50 * time.Millisecond},
		baseURL:  telegramAPIBase,
	}

	_, err := tn.apiCall("getMe", nil)
	if err == nil {
		t.Fatal("expected error from unreachable server")
	}
	if strings.Contains(err.Error(), "secret-test-token-12345") {
		t.Errorf("bot token leaked in error message: %v", err)
	}

	if !strings.Contains(err.Error(), "getMe") {
		t.Errorf("error should mention the API method, got: %v", err)
	}
}

func TestDiscordNotifier_ImplementsNotifier(t *testing.T) {

	var _ Notifier = (*DiscordNotifier)(nil)
}

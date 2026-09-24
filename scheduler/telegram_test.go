package main

import (
	"encoding/json"
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
	var sendCalls int

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/sendMessage") {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		sendCalls++
		var body map[string]interface{}
		json.NewDecoder(r.Body).Decode(&body)
		receivedChatID, _ = body["chat_id"].(string)
		receivedText, _ = body["text"].(string)

		if sendCalls == 1 {
			json.NewEncoder(w).Encode(telegramResponse{OK: true})
			return
		}
		json.NewEncoder(w).Encode(telegramResponse{Description: "Unauthorized"})
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
	if err := tg.SendMessage("67891", "rejected"); err == nil || !strings.Contains(err.Error(), "Unauthorized") {
		t.Errorf("expected Telegram API error, got %v", err)
	}
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

func TestAskDMRejectsStaleMessages(t *testing.T) {
	var sendCalls int
	var updateCalls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/sendMessage"):
			sendCalls++
			json.NewEncoder(w).Encode(telegramResponse{OK: true})
		case strings.HasSuffix(r.URL.Path, "/getUpdates"):
			updateCalls++
			date := time.Now().Unix()
			text := "fresh"
			if updateCalls == 1 {
				date -= 60
				text = "stale"
			}
			updates := []telegramUpdate{{
				UpdateID: int64(updateCalls),
				Message: &telegramMsg{
					From: &telegramUser{ID: 999},
					Date: date,
					Text: text,
				},
			}}
			result, err := json.Marshal(updates)
			if err != nil {
				t.Fatalf("marshal updates: %v", err)
			}
			json.NewEncoder(w).Encode(telegramResponse{OK: true, Result: result})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	tg := newTestTelegramNotifier(server.URL)
	got, err := tg.AskDM("999", "question", 2*time.Second)
	if err != nil {
		t.Fatalf("AskDM returned error: %v", err)
	}
	if got != "fresh" {
		t.Fatalf("AskDM returned %q, want fresh response after stale update", got)
	}
	if sendCalls != 1 {
		t.Errorf("AskDM sent %d questions, want 1", sendCalls)
	}
	if updateCalls != 2 {
		t.Errorf("AskDM polled %d times, want stale and fresh updates", updateCalls)
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

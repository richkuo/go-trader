package main

import (
	"fmt"
	"sync"
	"testing"
	"time"
)

// mockNotifier is a test double that records all calls.
type mockNotifier struct {
	mu         sync.Mutex
	messages   []mockMessage
	dms        []mockDM
	askResp    string
	askErr     error
	closed     bool
	failSendDM bool // when true, SendDM errors (exercises SendMessage fallback in sendTradeDestination)
}

type mockMessage struct {
	channelID string
	content   string
}

type mockDM struct {
	userID  string
	content string
}

func (m *mockNotifier) SendMessage(channelID string, content string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.messages = append(m.messages, mockMessage{channelID, content})
	return nil
}

func (m *mockNotifier) SendDM(userID, content string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.failSendDM {
		return fmt.Errorf("mock SendDM failed")
	}
	m.dms = append(m.dms, mockDM{userID, content})
	return nil
}

func (m *mockNotifier) AskDM(userID, question string, timeout time.Duration) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.dms = append(m.dms, mockDM{userID, question})
	return m.askResp, m.askErr
}

func (m *mockNotifier) Close() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.closed = true
}

func TestSendTradeDestination_PrefersSendDM(t *testing.T) {
	m := &mockNotifier{}
	if err := sendTradeDestination(m, "user-1", "hello"); err != nil {
		t.Fatal(err)
	}
	if len(m.dms) != 1 || m.dms[0].userID != "user-1" {
		t.Fatalf("expected 1 DM, got %#v", m.dms)
	}
	if len(m.messages) != 0 {
		t.Fatalf("expected no channel messages, got %#v", m.messages)
	}
}

func TestSendTradeDestination_FallsBackToSendMessage(t *testing.T) {
	m := &mockNotifier{failSendDM: true}
	if err := sendTradeDestination(m, "channel-99", "hello"); err != nil {
		t.Fatal(err)
	}
	if len(m.dms) != 0 {
		t.Fatalf("expected no DMs when SendDM fails, got %#v", m.dms)
	}
	if len(m.messages) != 1 || m.messages[0].channelID != "channel-99" {
		t.Fatalf("expected 1 channel message, got %#v", m.messages)
	}
}

func TestMultiNotifier_NoBackends(t *testing.T) {
	mn := NewMultiNotifier()
	if mn.HasBackends() {
		t.Error("expected no backends")
	}
	if mn.BackendCount() != 0 {
		t.Errorf("expected 0 backends, got %d", mn.BackendCount())
	}
	if mn.HasOwner() {
		t.Error("expected no owner")
	}

	// Operations should not panic.
	mn.SendToAllChannels("test")
	mn.SendOwnerDM("test")
	mn.Close()
}

func TestMultiNotifier_SingleBackend(t *testing.T) {
	mock := &mockNotifier{}
	mn := NewMultiNotifier(notifierBackend{
		notifier: mock,
		channels: map[string]string{"spot": "ch1", "hyperliquid": "ch2"},
		ownerID:  "owner1",
	})

	if !mn.HasBackends() {
		t.Error("expected backends")
	}
	if mn.BackendCount() != 1 {
		t.Errorf("expected 1 backend, got %d", mn.BackendCount())
	}
	if !mn.HasOwner() {
		t.Error("expected owner")
	}
	if mn.OwnerID() != "owner1" {
		t.Errorf("expected owner1, got %s", mn.OwnerID())
	}

	// SendToChannel
	mn.SendToChannel("binanceus", "spot", "spot message")
	if len(mock.messages) != 1 || mock.messages[0].channelID != "ch1" {
		t.Errorf("expected message to ch1, got %v", mock.messages)
	}

	mn.SendToChannel("hyperliquid", "perps", "perps message")
	if len(mock.messages) != 2 || mock.messages[1].channelID != "ch2" {
		t.Errorf("expected message to ch2, got %v", mock.messages)
	}

	// SendToChannel with no match
	mn.SendToChannel("unknown", "unknown", "no match")
	if len(mock.messages) != 2 {
		t.Errorf("expected no new messages, got %d", len(mock.messages))
	}

	// SendOwnerDM
	mn.SendOwnerDM("hello owner")
	if len(mock.dms) != 1 || mock.dms[0].userID != "owner1" {
		t.Errorf("expected DM to owner1, got %v", mock.dms)
	}

	// SendToAllChannels
	mock.messages = nil
	mn.SendToAllChannels("broadcast")
	if len(mock.messages) != 2 {
		t.Errorf("expected 2 broadcasts (ch1 + ch2), got %d", len(mock.messages))
	}
}

func TestMultiNotifier_DualBackends(t *testing.T) {
	discord := &mockNotifier{}
	telegram := &mockNotifier{}

	mn := NewMultiNotifier(
		notifierBackend{
			notifier: discord,
			channels: map[string]string{"spot": "discord-ch1"},
			ownerID:  "discord-owner",
		},
		notifierBackend{
			notifier: telegram,
			channels: map[string]string{"spot": "telegram-ch1"},
			ownerID:  "telegram-owner",
		},
	)

	if mn.BackendCount() != 2 {
		t.Errorf("expected 2 backends, got %d", mn.BackendCount())
	}

	// SendToChannel sends to both backends
	mn.SendToChannel("binanceus", "spot", "spot msg")
	if len(discord.messages) != 1 || discord.messages[0].channelID != "discord-ch1" {
		t.Errorf("expected discord message to discord-ch1, got %v", discord.messages)
	}
	if len(telegram.messages) != 1 || telegram.messages[0].channelID != "telegram-ch1" {
		t.Errorf("expected telegram message to telegram-ch1, got %v", telegram.messages)
	}

	// SendOwnerDM sends to both owners
	mn.SendOwnerDM("update available")
	if len(discord.dms) != 1 || discord.dms[0].userID != "discord-owner" {
		t.Errorf("expected discord DM to discord-owner, got %v", discord.dms)
	}
	if len(telegram.dms) != 1 || telegram.dms[0].userID != "telegram-owner" {
		t.Errorf("expected telegram DM to telegram-owner, got %v", telegram.dms)
	}

	// AskOwnerDM uses first backend with owner
	discord.askResp = "yes"
	resp, err := mn.AskOwnerDM("upgrade?", 5*time.Second)
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}
	if resp != "yes" {
		t.Errorf("expected 'yes', got %q", resp)
	}

	// Close shuts down all
	mn.Close()
	if !discord.closed || !telegram.closed {
		t.Error("expected both backends closed")
	}
}

func TestMultiNotifier_ResolveChannelKey(t *testing.T) {
	mn := NewMultiNotifier(
		notifierBackend{
			notifier: &mockNotifier{},
			channels: map[string]string{"spot": "ch1", "hyperliquid": "ch2"},
		},
	)

	if key := mn.resolveChannelKey("hyperliquid", "perps"); key != "hyperliquid" {
		t.Errorf("expected 'hyperliquid', got %q", key)
	}
	if key := mn.resolveChannelKey("binanceus", "spot"); key != "spot" {
		t.Errorf("expected 'spot', got %q", key)
	}
	if key := mn.resolveChannelKey("unknown", "unknown"); key != "" {
		t.Errorf("expected '', got %q", key)
	}
}

func TestMultiNotifier_HasChannel(t *testing.T) {
	mn := NewMultiNotifier(
		notifierBackend{
			notifier: &mockNotifier{},
			channels: map[string]string{"spot": "ch1"},
		},
	)

	if !mn.HasChannel("binanceus", "spot") {
		t.Error("expected HasChannel to be true for spot")
	}
	if mn.HasChannel("unknown", "unknown") {
		t.Error("expected HasChannel to be false for unknown")
	}
}

func TestMultiNotifier_NilBackendFiltered(t *testing.T) {
	mock := &mockNotifier{}
	mn := NewMultiNotifier(
		notifierBackend{notifier: nil, channels: nil},
		notifierBackend{notifier: mock, channels: map[string]string{"spot": "ch1"}, ownerID: "o1"},
	)
	if mn.BackendCount() != 1 {
		t.Errorf("expected 1 backend (nil filtered), got %d", mn.BackendCount())
	}
}

func TestMultiNotifier_AskOwnerDM_NoOwner(t *testing.T) {
	mn := NewMultiNotifier(notifierBackend{
		notifier: &mockNotifier{},
		channels: map[string]string{"spot": "ch1"},
		ownerID:  "",
	})
	_, err := mn.AskOwnerDM("question?", 1*time.Second)
	if err != ErrDMTimeout {
		t.Errorf("expected ErrDMTimeout, got %v", err)
	}
}

func TestMultiNotifier_SendToAllChannels_Deduplicated(t *testing.T) {
	mock := &mockNotifier{}
	mn := NewMultiNotifier(notifierBackend{
		notifier: mock,
		channels: map[string]string{"spot": "ch1", "perps": "ch1"}, // same channel
		ownerID:  "o1",
	})

	mn.SendToAllChannels("broadcast")
	// Should only send once to ch1 (deduplicated)
	if len(mock.messages) != 1 {
		t.Errorf("expected 1 message (deduplicated), got %d: %v", len(mock.messages), mock.messages)
	}
}

func TestMultiNotifier_AllChannelKeys(t *testing.T) {
	mn := NewMultiNotifier(
		notifierBackend{
			notifier: &mockNotifier{},
			channels: map[string]string{"spot": "ch1"},
		},
		notifierBackend{
			notifier: &mockNotifier{},
			channels: map[string]string{"spot": "ch2", "hyperliquid": "ch3"},
		},
	)
	keys := mn.AllChannelKeys()
	if len(keys) != 2 {
		t.Errorf("expected 2 keys (spot, hyperliquid), got %d: %v", len(keys), keys)
	}
	if !keys["spot"] || !keys["hyperliquid"] {
		t.Errorf("missing expected keys: %v", keys)
	}
}

func TestMultiNotifier_AskDM_MatchesOwner(t *testing.T) {
	discord := &mockNotifier{askResp: "discord-reply"}
	telegram := &mockNotifier{askResp: "telegram-reply"}

	mn := NewMultiNotifier(
		notifierBackend{notifier: discord, ownerID: "discord-owner"},
		notifierBackend{notifier: telegram, ownerID: "telegram-owner"},
	)

	// AskDM with matching owner should route correctly
	resp, err := mn.AskDM("telegram-owner", "question?", 5*time.Second)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp != "telegram-reply" {
		t.Errorf("expected 'telegram-reply', got %q", resp)
	}
}

// errNotifier always returns errors.
type errNotifier struct{}

func (e *errNotifier) SendMessage(channelID string, content string) error {
	return fmt.Errorf("send failed")
}
func (e *errNotifier) SendDM(userID, content string) error {
	return fmt.Errorf("dm failed")
}
func (e *errNotifier) AskDM(userID, question string, timeout time.Duration) (string, error) {
	return "", fmt.Errorf("ask failed")
}
func (e *errNotifier) Close() {}

func TestMultiNotifier_SendMessage_ReturnsFirstError(t *testing.T) {
	mn := NewMultiNotifier(
		notifierBackend{notifier: &errNotifier{}, channels: map[string]string{"spot": "ch1"}},
		notifierBackend{notifier: &mockNotifier{}, channels: map[string]string{"spot": "ch1"}},
	)

	err := mn.SendMessage("ch1", "test")
	if err == nil {
		t.Error("expected error from first backend")
	}
}

func TestMultiNotifier_SendMessage_RoutesPerBackend(t *testing.T) {
	discord := &mockNotifier{}
	telegram := &mockNotifier{}

	mn := NewMultiNotifier(
		notifierBackend{
			notifier: discord,
			channels: map[string]string{"spot": "discord-ch1"},
		},
		notifierBackend{
			notifier: telegram,
			channels: map[string]string{"spot": "telegram-ch1"},
		},
	)

	// Sending to a Discord channel should NOT reach Telegram.
	mn.SendMessage("discord-ch1", "hello discord")
	if len(discord.messages) != 1 {
		t.Errorf("expected 1 discord message, got %d", len(discord.messages))
	}
	if len(telegram.messages) != 0 {
		t.Errorf("expected 0 telegram messages, got %d", len(telegram.messages))
	}

	// Sending to a Telegram channel should NOT reach Discord.
	mn.SendMessage("telegram-ch1", "hello telegram")
	if len(discord.messages) != 1 {
		t.Errorf("expected still 1 discord message, got %d", len(discord.messages))
	}
	if len(telegram.messages) != 1 {
		t.Errorf("expected 1 telegram message, got %d", len(telegram.messages))
	}
}

func TestMultiNotifier_SendDM_RoutesPerBackend(t *testing.T) {
	discord := &mockNotifier{}
	telegram := &mockNotifier{}

	mn := NewMultiNotifier(
		notifierBackend{
			notifier: discord,
			ownerID:  "discord-owner",
		},
		notifierBackend{
			notifier: telegram,
			ownerID:  "telegram-owner",
		},
	)

	// DM to Discord owner should NOT reach Telegram.
	mn.SendDM("discord-owner", "hello discord")
	if len(discord.dms) != 1 {
		t.Errorf("expected 1 discord DM, got %d", len(discord.dms))
	}
	if len(telegram.dms) != 0 {
		t.Errorf("expected 0 telegram DMs, got %d", len(telegram.dms))
	}

	// DM to Telegram owner should NOT reach Discord.
	mn.SendDM("telegram-owner", "hello telegram")
	if len(discord.dms) != 1 {
		t.Errorf("expected still 1 discord DM, got %d", len(discord.dms))
	}
	if len(telegram.dms) != 1 {
		t.Errorf("expected 1 telegram DM, got %d", len(telegram.dms))
	}
}

func TestMultiNotifier_PostLeaderboardBroadcast_DedicatedChannel(t *testing.T) {
	mock := &mockNotifier{}
	mn := NewMultiNotifier(notifierBackend{
		notifier:           mock,
		channels:           map[string]string{"spot": "spot-ch", "perps": "perps-ch"},
		leaderboardChannel: "lb-ch",
	})

	mn.PostLeaderboardBroadcast("top10 board")

	// Should send exactly once to lb-ch (not broadcast to spot-ch + perps-ch).
	if len(mock.messages) != 1 {
		t.Fatalf("expected 1 message on dedicated channel, got %d: %v", len(mock.messages), mock.messages)
	}
	if mock.messages[0].channelID != "lb-ch" {
		t.Errorf("expected message on lb-ch, got %s", mock.messages[0].channelID)
	}
}

func TestMultiNotifier_PostLeaderboardBroadcast_FallbackBroadcast(t *testing.T) {
	mock := &mockNotifier{}
	mn := NewMultiNotifier(notifierBackend{
		notifier: mock,
		channels: map[string]string{"spot": "spot-ch", "perps": "perps-ch"},
	})

	mn.PostLeaderboardBroadcast("top10 board")

	// Should broadcast to both unique channels.
	if len(mock.messages) != 2 {
		t.Fatalf("expected 2 broadcast messages, got %d: %v", len(mock.messages), mock.messages)
	}
	seen := map[string]bool{}
	for _, m := range mock.messages {
		seen[m.channelID] = true
	}
	if !seen["spot-ch"] || !seen["perps-ch"] {
		t.Errorf("expected broadcast to spot-ch and perps-ch, got %v", seen)
	}
}

func TestMultiNotifier_PostLeaderboardBroadcast_PerBackend(t *testing.T) {
	discord := &mockNotifier{}
	telegram := &mockNotifier{}

	// Discord has dedicated channel; Telegram should still receive a broadcast
	// across its own channels.
	mn := NewMultiNotifier(
		notifierBackend{
			notifier:           discord,
			channels:           map[string]string{"spot": "discord-spot", "perps": "discord-perps"},
			leaderboardChannel: "discord-lb",
		},
		notifierBackend{
			notifier: telegram,
			channels: map[string]string{"spot": "telegram-spot", "perps": "telegram-perps"},
		},
	)

	mn.PostLeaderboardBroadcast("top10 board")

	// Discord: 1 message on lb-ch.
	if len(discord.messages) != 1 || discord.messages[0].channelID != "discord-lb" {
		t.Errorf("expected discord 1 message on discord-lb, got %v", discord.messages)
	}
	// Telegram: 2 messages, broadcast to both channels.
	if len(telegram.messages) != 2 {
		t.Fatalf("expected telegram 2 broadcast messages, got %d: %v", len(telegram.messages), telegram.messages)
	}
	seen := map[string]bool{}
	for _, m := range telegram.messages {
		seen[m.channelID] = true
	}
	if !seen["telegram-spot"] || !seen["telegram-perps"] {
		t.Errorf("expected telegram broadcast to telegram-spot and telegram-perps, got %v", seen)
	}
}

func TestMultiNotifier_SendMessage_UnknownChannel(t *testing.T) {
	mock := &mockNotifier{}
	mn := NewMultiNotifier(
		notifierBackend{notifier: mock, channels: map[string]string{"spot": "ch1"}},
	)

	// Unknown channel ID should not be sent anywhere.
	err := mn.SendMessage("unknown-channel", "test")
	if err != nil {
		t.Errorf("expected nil error for unmatched channel, got %v", err)
	}
	if len(mock.messages) != 0 {
		t.Errorf("expected 0 messages for unknown channel, got %d", len(mock.messages))
	}
}

func TestMultiNotifier_ReloadConfigConcurrentRoutingReads(t *testing.T) {
	mock := &mockNotifier{}
	cfgA := &Config{
		Discord: DiscordConfig{
			Channels:           map[string]string{"spot": "spot-a", "hyperliquid": "hl-a", "hyperliquid-live": "hl-live-a"},
			DMChannels:         map[string]string{"hyperliquid": "dm-a"},
			LeaderboardChannel: "lb-a",
		},
	}
	cfgB := &Config{
		Discord: DiscordConfig{
			Channels:           map[string]string{"spot": "spot-b", "hyperliquid": "hl-b", "hyperliquid-live": "hl-live-b"},
			DMChannels:         map[string]string{"hyperliquid": "dm-b"},
			LeaderboardChannel: "lb-b",
		},
	}
	mn := NewMultiNotifier(notifierBackend{
		notifier:           mock,
		channels:           cfgA.Discord.Channels,
		dmChannels:         cfgA.Discord.DMChannels,
		leaderboardChannel: cfgA.Discord.LeaderboardChannel,
		ownerID:            "owner-1",
	})

	sc := StrategyConfig{
		ID:       "hl-btc",
		Platform: "hyperliquid",
		Type:     "perps",
		Args:     []string{"--live"},
	}
	stratState := &StrategyState{
		TradeHistory: []Trade{{
			Timestamp:  time.Now(),
			StrategyID: "hl-btc",
			Symbol:     "BTC",
			Side:       "buy",
			Quantity:   1,
			Price:      100,
			Value:      100,
			TradeType:  "perps",
		}},
	}

	var stateMu sync.RWMutex
	start := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		<-start
		for i := 0; i < 250; i++ {
			if i%2 == 0 {
				mn.ReloadConfig(cfgA)
			} else {
				mn.ReloadConfig(cfgB)
			}
		}
	}()
	go func() {
		defer wg.Done()
		<-start
		for i := 0; i < 250; i++ {
			_ = mn.SendMessage("hl-a", "direct")
			mn.SendDM("owner-1", "dm")
			mn.SendToChannel("hyperliquid", "perps", "platform")
			mn.PostLeaderboardBroadcast("leaderboard")
			mn.SendToAllChannels("broadcast")
			mn.SendOwnerDM("owner")
			_ = mn.HasChannel("hyperliquid", "perps")
			_ = mn.resolveChannelKey("hyperliquid", "perps")
			_ = mn.AllChannelKeys()
			sendTradeAlerts(sc, stratState, 1, &stateMu, mn)
		}
	}()
	close(start)
	wg.Wait()
}

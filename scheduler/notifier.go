package main

import (
	"fmt"
	"strings"
	"sync"
	"time"
)

// Notifier is the abstract interface for sending messages and two-way DM communication.
// Both Discord and Telegram implement this interface.
type Notifier interface {
	SendMessage(channelID string, content string) error
	SendDM(userID, content string) error
	AskDM(userID, question string, timeout time.Duration) (string, error)
	Close()
}

// notifierBackend pairs a Notifier with its provider-specific config.
type notifierBackend struct {
	notifier           Notifier
	channels           map[string]string // channel map from config (keyed by platform/type; "<platform>-paper" for paper-specific)
	tradeAlertChannels map[string]string // optional override: route trade alerts to different channels than summaries
	ownerID            string
	leaderboardChannel string            // dedicated leaderboard channel ID (optional); when set, leaderboard posts route here
	dmChannels         map[string]string // per-platform DM-style trade alerts (#248)
	plainText          bool              // use plain-text formatting (no markdown)
}

// MultiNotifier fans out calls to all configured notification providers.
// It is aware of each provider's channel config and owner ID for proper routing.
type MultiNotifier struct {
	mu       sync.RWMutex
	backends []notifierBackend
}

// NewMultiNotifier creates a MultiNotifier from backend descriptors.
func NewMultiNotifier(backends ...notifierBackend) *MultiNotifier {
	var valid []notifierBackend
	for _, b := range backends {
		if b.notifier != nil {
			b.channels = cloneStringMap(b.channels)
			b.tradeAlertChannels = cloneStringMap(b.tradeAlertChannels)
			b.dmChannels = cloneStringMap(b.dmChannels)
			valid = append(valid, b)
		}
	}
	return &MultiNotifier{backends: valid}
}

func (m *MultiNotifier) snapshotBackends() []notifierBackend {
	if m == nil {
		return nil
	}
	m.mu.RLock()
	defer m.mu.RUnlock()

	out := make([]notifierBackend, len(m.backends))
	for i, b := range m.backends {
		b.channels = cloneStringMap(b.channels)
		b.tradeAlertChannels = cloneStringMap(b.tradeAlertChannels)
		b.dmChannels = cloneStringMap(b.dmChannels)
		out[i] = b
	}
	return out
}

// SendMessage sends content to backends that own the given channel/chat ID.
// A backend receives the message only if channelID appears in its channel map.
// Returns the first error encountered; all per-backend errors are logged.
func (m *MultiNotifier) SendMessage(channelID string, content string) error {
	var firstErr error
	for _, b := range m.snapshotBackends() {
		if !backendOwnsChannel(b, channelID) {
			continue
		}
		if err := b.notifier.SendMessage(channelID, content); err != nil {
			fmt.Printf("[WARN] SendMessage to channel %s failed: %v\n", channelID, err)
			if firstErr == nil {
				firstErr = err
			}
		}
	}
	return firstErr
}

// SendDM sends content as a direct message to backends whose ownerID matches userID.
// Returns the first error encountered; all per-backend errors are logged.
func (m *MultiNotifier) SendDM(userID, content string) error {
	var firstErr error
	for _, b := range m.snapshotBackends() {
		if b.ownerID != userID {
			continue
		}
		if err := b.notifier.SendDM(userID, content); err != nil {
			fmt.Printf("[WARN] SendDM to user %s failed: %v\n", userID, err)
			if firstErr == nil {
				firstErr = err
			}
		}
	}
	return firstErr
}

// AskDM sends a question and waits for a reply. Uses the first backend with a matching owner.
func (m *MultiNotifier) AskDM(userID, question string, timeout time.Duration) (string, error) {
	backends := m.snapshotBackends()
	for _, b := range backends {
		if b.ownerID == userID {
			return b.notifier.AskDM(userID, question, timeout)
		}
	}
	if len(backends) > 0 {
		return backends[0].notifier.AskDM(userID, question, timeout)
	}
	return "", fmt.Errorf("no notification backends configured")
}

// Close shuts down all backends.
func (m *MultiNotifier) Close() {
	for _, b := range m.snapshotBackends() {
		b.notifier.Close()
	}
}

// HasBackends returns true if at least one backend is configured.
func (m *MultiNotifier) HasBackends() bool {
	if m == nil {
		return false
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	return len(m.backends) > 0
}

// BackendCount returns the number of active backends.
func (m *MultiNotifier) BackendCount() int {
	if m == nil {
		return 0
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	return len(m.backends)
}

// ReloadConfig refreshes per-provider routing maps after a hot config reload.
// Backend construction (tokens, gateway sessions, owner identity) is intentionally
// restart-only; this updates only the channel settings that can be changed safely.
func (m *MultiNotifier) ReloadConfig(cfg *Config) {
	if m == nil || cfg == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for i := range m.backends {
		b := &m.backends[i]
		if b.plainText {
			b.channels = cloneStringMap(cfg.Telegram.Channels)
			b.tradeAlertChannels = cloneStringMap(cfg.Telegram.TradeAlertChannels)
			b.dmChannels = cloneStringMap(cfg.Telegram.DMChannels)
			continue
		}
		b.channels = cloneStringMap(cfg.Discord.Channels)
		b.tradeAlertChannels = cloneStringMap(cfg.Discord.TradeAlertChannels)
		b.dmChannels = cloneStringMap(cfg.Discord.DMChannels)
		b.leaderboardChannel = cfg.Discord.LeaderboardChannel
	}
}

// OwnerID returns the first configured owner ID across all backends.
func (m *MultiNotifier) OwnerID() string {
	for _, b := range m.snapshotBackends() {
		if b.ownerID != "" {
			return b.ownerID
		}
	}
	return ""
}

// HasOwner returns true if any backend has an owner configured.
func (m *MultiNotifier) HasOwner() bool {
	return m.OwnerID() != ""
}

// backendOwnsChannel returns true if channelID is one of the backend's configured channel values.
func backendOwnsChannel(b notifierBackend, channelID string) bool {
	for _, ch := range b.channels {
		if ch == channelID {
			return true
		}
	}
	return false
}

// SendToChannel sends content to all backends that have a channel configured
// for the given platform and strategy type.
func (m *MultiNotifier) SendToChannel(platform, stratType, content string) {
	for _, b := range m.snapshotBackends() {
		if ch := resolveChannel(b.channels, platform, stratType); ch != "" {
			if err := b.notifier.SendMessage(ch, content); err != nil {
				fmt.Printf("[WARN] Notifier send to channel failed: %v\n", err)
			}
		}
	}
}

// PostLeaderboardBroadcast routes an all-time leaderboard message
// (top/bottom) on a per-backend basis. For each backend: if a dedicated
// leaderboardChannel is configured, the message is sent there once; otherwise
// it broadcasts to all unique channels on that backend.
func (m *MultiNotifier) PostLeaderboardBroadcast(content string) {
	for _, b := range m.snapshotBackends() {
		if b.leaderboardChannel != "" {
			if err := b.notifier.SendMessage(b.leaderboardChannel, content); err != nil {
				fmt.Printf("[WARN] Notifier send to leaderboard channel %s failed: %v\n", b.leaderboardChannel, err)
			}
			continue
		}
		seen := make(map[string]bool)
		for _, ch := range b.channels {
			if ch != "" && !seen[ch] {
				seen[ch] = true
				if err := b.notifier.SendMessage(ch, content); err != nil {
					fmt.Printf("[WARN] Notifier broadcast failed: %v\n", err)
				}
			}
		}
	}
}

// SendToAllChannels sends content to all unique channels across all backends.
// Used for broadcast messages (kill switch, correlation warnings).
func (m *MultiNotifier) SendToAllChannels(content string) {
	for _, b := range m.snapshotBackends() {
		seen := make(map[string]bool)
		for _, ch := range b.channels {
			if ch != "" && !seen[ch] {
				seen[ch] = true
				if err := b.notifier.SendMessage(ch, content); err != nil {
					fmt.Printf("[WARN] Notifier broadcast failed: %v\n", err)
				}
			}
		}
	}
}

// SendOwnerDM sends a DM to the owner on all backends that have an owner configured.
func (m *MultiNotifier) SendOwnerDM(content string) {
	for _, b := range m.snapshotBackends() {
		if b.ownerID != "" {
			if err := b.notifier.SendDM(b.ownerID, content); err != nil {
				fmt.Printf("[WARN] Owner DM failed: %v\n", err)
			}
		}
	}
}

// AskOwnerDM sends a question to the owner and waits for a reply.
// Uses the first backend that has an owner configured.
func (m *MultiNotifier) AskOwnerDM(question string, timeout time.Duration) (string, error) {
	for _, b := range m.snapshotBackends() {
		if b.ownerID != "" {
			return b.notifier.AskDM(b.ownerID, question, timeout)
		}
	}
	return "", ErrDMTimeout
}

// HasChannel returns true if any backend has a channel configured for the given platform/type.
func (m *MultiNotifier) HasChannel(platform, stratType string) bool {
	for _, b := range m.snapshotBackends() {
		if resolveChannel(b.channels, platform, stratType) != "" {
			return true
		}
	}
	return false
}

// resolveChannelKey returns the logical channel key for a strategy.
// Uses the same lookup order as resolveChannel: platform first, then stratType.
// Returns "" if no channel is configured on any backend.
func (m *MultiNotifier) resolveChannelKey(platform, stratType string) string {
	for _, b := range m.snapshotBackends() {
		if _, ok := b.channels[platform]; ok {
			return platform
		}
		if _, ok := b.channels[stratType]; ok {
			return stratType
		}
	}
	return ""
}

// AllChannelKeys returns all unique channel keys across all backends.
func (m *MultiNotifier) AllChannelKeys() map[string]bool {
	keys := make(map[string]bool)
	for _, b := range m.snapshotBackends() {
		for k := range b.channels {
			keys[k] = true
		}
	}
	return keys
}

type tradeAlertRoute struct {
	notifier  Notifier
	plainText bool
	dmDest    string
	channel   string
	liveChan  string
}

func (m *MultiNotifier) tradeAlertRoutes(platform, stratType string, isLive bool) []tradeAlertRoute {
	var routes []tradeAlertRoute
	dmKey := platform
	if !isLive {
		dmKey = platform + "-paper"
	}
	for _, b := range m.snapshotBackends() {
		dmDest := ""
		if b.dmChannels != nil {
			dmDest = b.dmChannels[dmKey]
		}
		ch := resolveTradeAlertChannel(b.tradeAlertChannels, b.channels, platform, stratType, isLive)

		var liveCh string
		if isLive {
			// Direct lookup: the "-live" suffix is already the full key so no isLive re-suffix needed.
			liveCh = b.tradeAlertChannels[platform+"-live"]
			if liveCh == "" {
				liveCh = b.channels[platform+"-live"]
			}
			if liveCh == ch {
				liveCh = ""
			}
		}

		if dmDest == "" && ch == "" && liveCh == "" {
			continue
		}
		routes = append(routes, tradeAlertRoute{
			notifier:  b.notifier,
			plainText: b.plainText,
			dmDest:    dmDest,
			channel:   ch,
			liveChan:  liveCh,
		})
	}
	return routes
}

// sendTradeDestination delivers a trade alert to a user ID (DM) or channel ID.
// Discord requires UserChannelCreate for DMs, so we try SendDM first and fall back to SendMessage.
// Logs the original SendDM error before falling back so transient DM failures on valid user IDs
// are visible instead of being masked by a misleading "Unknown Channel" from the fallback.
func sendTradeDestination(n Notifier, id, content string) error {
	id = strings.TrimSpace(id)
	if id == "" {
		return nil
	}
	if err := n.SendDM(id, content); err == nil {
		return nil
	} else {
		fmt.Printf("[notify] SendDM(%s) failed, falling back to SendMessage: %v\n", id, err)
	}
	return n.SendMessage(id, content)
}

package main

import (
	"fmt"
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
	notifier      Notifier
	channels      map[string]string // channel map from config (keyed by platform/type)
	ownerID       string
	dmPaperTrades bool // send DM on paper trade execution
	dmLiveTrades  bool // send DM on live trade execution
	plainText     bool // use plain-text formatting (no markdown)
}

// MultiNotifier fans out calls to all configured notification providers.
// It is aware of each provider's channel config and owner ID for proper routing.
type MultiNotifier struct {
	backends []notifierBackend
}

// NewMultiNotifier creates a MultiNotifier from backend descriptors.
func NewMultiNotifier(backends ...notifierBackend) *MultiNotifier {
	var valid []notifierBackend
	for _, b := range backends {
		if b.notifier != nil {
			valid = append(valid, b)
		}
	}
	return &MultiNotifier{backends: valid}
}

// SendMessage sends content to the given channel/chat ID on all backends.
// Used when the caller already has a resolved channel ID.
func (m *MultiNotifier) SendMessage(channelID string, content string) error {
	var firstErr error
	for _, b := range m.backends {
		if err := b.notifier.SendMessage(channelID, content); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// SendDM sends content as a direct message on all backends using each backend's owner ID.
func (m *MultiNotifier) SendDM(userID, content string) error {
	var firstErr error
	for _, b := range m.backends {
		if err := b.notifier.SendDM(userID, content); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// AskDM sends a question and waits for a reply. Uses the first backend with a matching owner.
func (m *MultiNotifier) AskDM(userID, question string, timeout time.Duration) (string, error) {
	for _, b := range m.backends {
		if b.ownerID == userID {
			return b.notifier.AskDM(userID, question, timeout)
		}
	}
	if len(m.backends) > 0 {
		return m.backends[0].notifier.AskDM(userID, question, timeout)
	}
	return "", fmt.Errorf("no notification backends configured")
}

// Close shuts down all backends.
func (m *MultiNotifier) Close() {
	for _, b := range m.backends {
		b.notifier.Close()
	}
}

// HasBackends returns true if at least one backend is configured.
func (m *MultiNotifier) HasBackends() bool {
	return len(m.backends) > 0
}

// BackendCount returns the number of active backends.
func (m *MultiNotifier) BackendCount() int {
	return len(m.backends)
}

// OwnerID returns the first configured owner ID across all backends.
func (m *MultiNotifier) OwnerID() string {
	for _, b := range m.backends {
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

// SendToChannel sends content to all backends that have a channel configured
// for the given platform and strategy type.
func (m *MultiNotifier) SendToChannel(platform, stratType, content string) {
	for _, b := range m.backends {
		if ch := resolveChannel(b.channels, platform, stratType); ch != "" {
			if err := b.notifier.SendMessage(ch, content); err != nil {
				fmt.Printf("[WARN] Notifier send to channel failed: %v\n", err)
			}
		}
	}
}

// SendToAllChannels sends content to all unique channels across all backends.
// Used for broadcast messages (kill switch, correlation warnings).
func (m *MultiNotifier) SendToAllChannels(content string) {
	for _, b := range m.backends {
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
	for _, b := range m.backends {
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
	for _, b := range m.backends {
		if b.ownerID != "" {
			return b.notifier.AskDM(b.ownerID, question, timeout)
		}
	}
	return "", ErrDMTimeout
}

// HasChannel returns true if any backend has a channel configured for the given platform/type.
func (m *MultiNotifier) HasChannel(platform, stratType string) bool {
	for _, b := range m.backends {
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
	for _, b := range m.backends {
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
	for _, b := range m.backends {
		for k := range b.channels {
			keys[k] = true
		}
	}
	return keys
}

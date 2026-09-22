package main

import (
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
)

type Notifier interface {
	SendMessage(channelID string, content string) error
	SendDM(userID, content string) error
	AskDM(userID, question string, timeout time.Duration) (string, error)
	Close()
}

type notifierBackend struct {
	notifier           Notifier
	channels           map[string]string
	tradeAlertChannels map[string]string
	ownerID            string
	leaderboardChannel string
	dmChannels         map[string]string
	plainText          bool
	// partitionChannels is each paper partition's own destination set, rebuilt
	// from that partition's roster on every ReloadConfig.
	partitionChannels map[RiskPartition][]string
}

type MultiNotifier struct {
	mu       sync.RWMutex
	backends []notifierBackend
}

func NewMultiNotifier(backends ...notifierBackend) *MultiNotifier {
	var valid []notifierBackend
	for _, b := range backends {
		if b.notifier != nil {
			b.channels = cloneStringMap(b.channels)
			b.tradeAlertChannels = cloneStringMap(b.tradeAlertChannels)
			b.dmChannels = cloneStringMap(b.dmChannels)
			b.partitionChannels = clonePartitionChannels(b.partitionChannels)
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
		b.partitionChannels = clonePartitionChannels(b.partitionChannels)
		out[i] = b
	}
	return out
}

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

func (m *MultiNotifier) Close() {
	for _, b := range m.snapshotBackends() {
		b.notifier.Close()
	}
}

func (m *MultiNotifier) HasBackends() bool {
	if m == nil {
		return false
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	return len(m.backends) > 0
}

func (m *MultiNotifier) BackendCount() int {
	if m == nil {
		return 0
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	return len(m.backends)
}

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
			b.partitionChannels = partitionChannelValues(b.channels, cfg.Strategies)
			continue
		}
		b.channels = cloneStringMap(cfg.Discord.Channels)
		b.tradeAlertChannels = cloneStringMap(cfg.Discord.TradeAlertChannels)
		b.dmChannels = cloneStringMap(cfg.Discord.DMChannels)
		b.leaderboardChannel = cfg.Discord.LeaderboardChannel
		b.partitionChannels = partitionChannelValues(b.channels, cfg.Strategies)
	}
}

func (m *MultiNotifier) OwnerID() string {
	for _, b := range m.snapshotBackends() {
		if b.ownerID != "" {
			return b.ownerID
		}
	}
	return ""
}

func (m *MultiNotifier) HasOwner() bool {
	return m.OwnerID() != ""
}

func backendOwnsChannel(b notifierBackend, channelID string) bool {
	for _, ch := range b.channels {
		if ch == channelID {
			return true
		}
	}
	return false
}

func (m *MultiNotifier) SendToChannel(platform, stratType, content string) {
	for _, b := range m.snapshotBackends() {
		if ch := resolveChannel(b.channels, platform, stratType); ch != "" {
			if err := b.notifier.SendMessage(ch, content); err != nil {
				fmt.Printf("[WARN] Notifier send to channel failed: %v\n", err)
			}
		}
	}
}

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

func (m *MultiNotifier) SendOwnerDM(content string) {
	for _, b := range m.snapshotBackends() {
		if b.ownerID != "" {
			if err := b.notifier.SendDM(b.ownerID, content); err != nil {
				fmt.Printf("[WARN] Owner DM failed: %v\n", err)
			}
		}
	}
}

func (m *MultiNotifier) AskOwnerDM(question string, timeout time.Duration) (string, error) {
	for _, b := range m.snapshotBackends() {
		if b.ownerID != "" {
			return b.notifier.AskDM(b.ownerID, question, timeout)
		}
	}
	return "", ErrDMTimeout
}

func (m *MultiNotifier) HasChannel(platform, stratType string) bool {
	for _, b := range m.snapshotBackends() {
		if resolveChannel(b.channels, platform, stratType) != "" {
			return true
		}
	}
	return false
}

func (m *MultiNotifier) resolveChannelKey(platform, stratType string, isLive bool, source string) string {
	backends := m.snapshotBackends()
	if !isLive {
		for _, paperKey := range paperChannelKeys(platform, source) {
			for _, b := range backends {
				if ch, ok := b.channels[paperKey]; ok && ch != "" {
					return paperKey
				}
			}
		}
	}
	for _, b := range backends {
		if _, ok := b.channels[platform]; ok {
			return platform
		}
		if _, ok := b.channels[stratType]; ok {
			return stratType
		}
	}
	return ""
}

// SendToPartitionChannels reaches only the destinations that partition's own
// roster routes to. An empty set falls back to every channel, so a partition
// with no resolvable route is never silent.
func (m *MultiNotifier) SendToPartitionChannels(part RiskPartition, content string) {
	if part.IsLive() {
		m.SendToAllChannels(content)
		return
	}
	sent := false
	for _, b := range m.snapshotBackends() {
		seen := make(map[string]bool)
		for _, ch := range b.partitionChannels[part] {
			if ch == "" || seen[ch] {
				continue
			}
			seen[ch] = true
			sent = true
			if err := b.notifier.SendMessage(ch, content); err != nil {
				fmt.Printf("[WARN] Notifier broadcast failed: %v\n", err)
			}
		}
	}
	if !sent {
		m.SendToAllChannels(content)
	}
}

func partitionChannelValues(channels map[string]string, strats []StrategyConfig) map[RiskPartition][]string {
	out := make(map[RiskPartition][]string)
	for _, part := range activePartitions(strats) {
		if part.IsLive() {
			continue
		}
		seen := make(map[string]bool)
		var dests []string
		for _, sc := range strategiesInPartition(strats, part) {
			ch := resolveTradeChannel(channels, sc.Platform, sc.Type, false, sc.PaperSource)
			if ch == "" || seen[ch] {
				continue
			}
			seen[ch] = true
			dests = append(dests, ch)
		}
		sort.Strings(dests)
		out[part] = dests
	}
	return out
}

func clonePartitionChannels(m map[RiskPartition][]string) map[RiskPartition][]string {
	if m == nil {
		return nil
	}
	out := make(map[RiskPartition][]string, len(m))
	for part, dests := range m {
		out[part] = cloneStringSlice(dests)
	}
	return out
}

func cloneStringSlice(s []string) []string {
	if s == nil {
		return nil
	}
	out := make([]string, len(s))
	copy(out, s)
	return out
}

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

type tradeAlertRouter interface {
	tradeAlertRoutes(platform, stratType string, isLive bool, source string) []tradeAlertRoute
}

func (m *MultiNotifier) tradeAlertRoutes(platform, stratType string, isLive bool, source string) []tradeAlertRoute {
	var routes []tradeAlertRoute
	// The DM key stays an exact match with no fallback: a sourced strategy
	// reads only its own key, so a moved book never DMs another source's route.
	dmKey := platform
	if !isLive {
		dmKey = paperChannelKeys(platform, source)[0]
	}
	for _, b := range m.snapshotBackends() {
		dmDest := ""
		if b.dmChannels != nil {
			dmDest = b.dmChannels[dmKey]
		}
		ch := resolveTradeAlertChannel(b.tradeAlertChannels, b.channels, platform, stratType, isLive, source)

		var liveCh string
		if isLive {
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

func (m *MultiNotifier) DiscordBackend() *DiscordNotifier {
	m.mu.RLock()
	defer m.mu.RUnlock()
	for _, b := range m.backends {
		if d, ok := b.notifier.(*DiscordNotifier); ok {
			return d
		}
	}
	return nil
}

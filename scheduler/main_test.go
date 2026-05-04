package main

import (
	"math"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestShouldSkipZeroCapital(t *testing.T) {
	cases := []struct {
		name       string
		capitalPct float64
		capital    float64
		want       bool
	}{
		{"capital_pct set and capital is zero", 0.5, 0, true},
		{"capital_pct set and capital is negative", 0.25, -100, true},
		{"capital_pct set and capital resolved", 0.5, 500, false},
		{"no capital_pct (fixed capital)", 0, 1000, false},
		{"no capital_pct and no capital", 0, 0, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sc := StrategyConfig{
				ID:         "test-strategy",
				CapitalPct: tc.capitalPct,
				Capital:    tc.capital,
			}
			if got := shouldSkipZeroCapital(sc); got != tc.want {
				t.Errorf("shouldSkipZeroCapital(pct=%g, cap=%g) = %v, want %v",
					tc.capitalPct, tc.capital, got, tc.want)
			}
		})
	}
}

func TestNotifyPerStrategyCircuitBreaker_BroadcastsFreshTriggers(t *testing.T) {
	cases := []struct {
		name                string
		reason              string
		wantTrailingPortVal bool
	}{
		{
			name: "max drawdown",
			reason: RiskReasonMaxDrawdownExceeded +
				" (30.0% > 25.0%, portfolio=$700.00 peak=$1000.00, denom=peak=$1000.00)",
			// Reason already embeds portfolio=$700.00 — formatter must not
			// duplicate the value with a trailing (portfolio=$1234.56).
			wantTrailingPortVal: false,
		},
		{
			name:                "consecutive losses",
			reason:              RiskReasonConsecutiveLosses,
			wantTrailingPortVal: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mock := &mockNotifier{}
			notifier := &MultiNotifier{
				backends: []notifierBackend{
					{
						notifier: mock,
						ownerID:  "owner123",
						channels: map[string]string{
							"spot":        "ch-spot",
							"hyperliquid": "ch-hl",
						},
					},
				},
			}
			sc := StrategyConfig{ID: "test-strategy", Platform: "binanceus", Type: "spot"}

			notifyPerStrategyCircuitBreaker(sc, tc.reason, 1234.56, notifier, false)

			if len(mock.messages) != 2 {
				t.Fatalf("expected 2 channel messages, got %d", len(mock.messages))
			}
			if len(mock.dms) != 1 {
				t.Fatalf("expected 1 owner DM, got %d", len(mock.dms))
			}
			for _, msg := range []string{mock.messages[0].content, mock.messages[1].content, mock.dms[0].content} {
				if !strings.Contains(msg, "**CIRCUIT BREAKER**") ||
					!strings.Contains(msg, "[test-strategy]") ||
					!strings.Contains(msg, tc.reason) {
					t.Fatalf("notification missing required context: %q", msg)
				}
				hasTrailing := strings.Contains(msg, "(portfolio=$1234.56)")
				if tc.wantTrailingPortVal && !hasTrailing {
					t.Fatalf("expected trailing (portfolio=$1234.56) in %q", msg)
				}
				if !tc.wantTrailingPortVal && hasTrailing {
					t.Fatalf("portfolio value duplicated when reason already embeds one: %q", msg)
				}
			}
		})
	}
}

func TestNotifyPerStrategyCircuitBreaker_SuppressesNonFreshAndPortfolioKill(t *testing.T) {
	cases := []struct {
		name                string
		reason              string
		portfolioKillFired  bool
		notifierHasBackends bool
		nilNotifier         bool
		wantChannelMessages int
		wantOwnerDMs        int
	}{
		{
			name:                "latched circuit breaker no spam",
			reason:              RiskReasonCircuitBreakerActive,
			notifierHasBackends: true,
		},
		{
			name:                "unknown reason strings are dropped",
			reason:              "daily loss limit exceeded",
			notifierHasBackends: true,
		},
		{
			name:                "portfolio kill owns notification",
			reason:              RiskReasonMaxDrawdownExceeded + " (30.0% > 25.0%)",
			portfolioKillFired:  true,
			notifierHasBackends: true,
		},
		{
			name:                "no backends",
			reason:              RiskReasonConsecutiveLosses,
			notifierHasBackends: false,
		},
		{
			name:        "nil notifier",
			reason:      RiskReasonConsecutiveLosses,
			nilNotifier: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mock := &mockNotifier{}
			var notifier *MultiNotifier
			if !tc.nilNotifier {
				notifier = &MultiNotifier{}
				if tc.notifierHasBackends {
					notifier.backends = []notifierBackend{
						{
							notifier: mock,
							ownerID:  "owner123",
							channels: map[string]string{"spot": "ch-spot"},
						},
					}
				}
			}
			sc := StrategyConfig{ID: "test-strategy", Platform: "binanceus", Type: "spot"}

			notifyPerStrategyCircuitBreaker(sc, tc.reason, 1234.56, notifier, tc.portfolioKillFired)

			if len(mock.messages) != tc.wantChannelMessages {
				t.Fatalf("channel messages = %d, want %d", len(mock.messages), tc.wantChannelMessages)
			}
			if len(mock.dms) != tc.wantOwnerDMs {
				t.Fatalf("owner DMs = %d, want %d", len(mock.dms), tc.wantOwnerDMs)
			}
		})
	}
}

func TestIsLiveArgs(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want bool
	}{
		{"live mode", []string{"sma", "BTC", "1h", "--mode=live"}, true},
		{"paper mode", []string{"sma", "BTC", "1h", "--mode=paper"}, false},
		{"no mode flag", []string{"sma", "BTC", "1h"}, false},
		{"empty args", []string{}, false},
		{"live at start", []string{"--mode=live", "sma"}, true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isLiveArgs(tc.args); got != tc.want {
				t.Errorf("isLiveArgs(%v) = %v, want %v", tc.args, got, tc.want)
			}
		})
	}
}

func TestHyperliquidIsLive(t *testing.T) {
	if hyperliquidIsLive([]string{"sma", "BTC", "1h", "--mode=live"}) != true {
		t.Error("expected true for --mode=live")
	}
	if hyperliquidIsLive([]string{"sma", "BTC", "1h"}) != false {
		t.Error("expected false without --mode=live")
	}
}

func TestHyperliquidSymbol(t *testing.T) {
	cases := []struct {
		args []string
		want string
	}{
		{[]string{"sma", "BTC", "1h"}, "BTC"},
		{[]string{"rsi", "ETH", "4h"}, "ETH"},
		{[]string{"sma"}, ""},
		{[]string{}, ""},
	}

	for _, tc := range cases {
		t.Run(tc.want, func(t *testing.T) {
			got := hyperliquidSymbol(tc.args)
			if got != tc.want {
				t.Errorf("hyperliquidSymbol(%v) = %q, want %q", tc.args, got, tc.want)
			}
		})
	}
}

func TestTopstepIsLive(t *testing.T) {
	if topstepIsLive([]string{"sma", "ES", "15m", "--mode=live"}) != true {
		t.Error("expected true for --mode=live")
	}
	if topstepIsLive([]string{"sma", "ES", "15m"}) != false {
		t.Error("expected false without --mode=live")
	}
}

func TestTopstepSymbol(t *testing.T) {
	cases := []struct {
		args []string
		want string
	}{
		{[]string{"sma", "ES", "15m"}, "ES"},
		{[]string{"rsi", "NQ", "5m"}, "NQ"},
		{[]string{"sma"}, ""},
		{[]string{}, ""},
	}

	for _, tc := range cases {
		t.Run(tc.want, func(t *testing.T) {
			got := topstepSymbol(tc.args)
			if got != tc.want {
				t.Errorf("topstepSymbol(%v) = %q, want %q", tc.args, got, tc.want)
			}
		})
	}
}

func TestRobinhoodIsLive(t *testing.T) {
	if robinhoodIsLive([]string{"sma", "BTC", "1h", "--mode=live"}) != true {
		t.Error("expected true for --mode=live")
	}
	if robinhoodIsLive([]string{"sma", "BTC", "1h"}) != false {
		t.Error("expected false without --mode=live")
	}
}

func TestRobinhoodSymbol(t *testing.T) {
	cases := []struct {
		args []string
		want string
	}{
		{[]string{"sma", "BTC", "1h"}, "BTC"},
		{[]string{"rsi"}, ""},
		{[]string{}, ""},
	}

	for _, tc := range cases {
		t.Run(tc.want, func(t *testing.T) {
			got := robinhoodSymbol(tc.args)
			if got != tc.want {
				t.Errorf("robinhoodSymbol(%v) = %q, want %q", tc.args, got, tc.want)
			}
		})
	}
}

func TestOKXIsLive(t *testing.T) {
	if okxIsLive([]string{"sma", "BTC", "1h", "--mode=live"}) != true {
		t.Error("expected true for --mode=live")
	}
	if okxIsLive([]string{"sma", "BTC", "1h"}) != false {
		t.Error("expected false without --mode=live")
	}
}

func TestOKXSymbol(t *testing.T) {
	cases := []struct {
		args []string
		want string
	}{
		{[]string{"sma", "BTC", "1h"}, "BTC"},
		{[]string{"rsi"}, ""},
		{[]string{}, ""},
	}

	for _, tc := range cases {
		t.Run(tc.want, func(t *testing.T) {
			got := okxSymbol(tc.args)
			if got != tc.want {
				t.Errorf("okxSymbol(%v) = %q, want %q", tc.args, got, tc.want)
			}
		})
	}
}

func TestOKXInstType(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want string
	}{
		{"swap default", []string{"sma", "BTC", "1h"}, "swap"},
		{"explicit swap", []string{"sma", "BTC", "1h", "--inst-type=swap"}, "swap"},
		{"spot", []string{"sma", "BTC", "1h", "--inst-type=spot"}, "spot"},
		{"empty args", []string{}, "swap"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := okxInstType(tc.args)
			if got != tc.want {
				t.Errorf("okxInstType(%v) = %q, want %q", tc.args, got, tc.want)
			}
		})
	}
}

// helper to build a trade for testing sendTradeAlerts
func testTrade() Trade {
	return Trade{
		Timestamp:  time.Now(),
		StrategyID: "test-spot-sma",
		Symbol:     "BTC/USDT",
		Side:       "buy",
		Quantity:   0.01,
		Price:      50000,
		Value:      500,
		TradeType:  "spot",
		Details:    "Open long BTC/USDT",
	}
}

func TestSendTradeAlerts_DMAndChannel(t *testing.T) {
	mock := &mockNotifier{}
	sc := StrategyConfig{
		ID:       "test-spot-sma",
		Type:     "spot",
		Platform: "binanceus",
		Args:     []string{"sma", "BTC/USDT", "1h", "--mode=paper"},
	}
	state := &StrategyState{
		TradeHistory: []Trade{testTrade()},
	}
	var mu sync.RWMutex
	notifier := &MultiNotifier{
		backends: []notifierBackend{
			{
				notifier:   mock,
				ownerID:    "owner123",
				channels:   map[string]string{"spot": "ch-spot-123"},
				dmChannels: map[string]string{"binanceus-paper": "owner123"},
			},
		},
	}

	sendTradeAlerts(sc, state, 1, &mu, notifier)

	if len(mock.dms) != 1 {
		t.Errorf("expected 1 DM message, got %d", len(mock.dms))
	}
	if len(mock.messages) != 1 {
		t.Errorf("expected 1 channel message, got %d", len(mock.messages))
	}
	if len(mock.messages) > 0 && mock.messages[0].channelID != "ch-spot-123" {
		t.Errorf("expected channel message to ch-spot-123, got %s", mock.messages[0].channelID)
	}
}

func TestSendTradeAlerts_DMOnly(t *testing.T) {
	// DM enabled but no channel configured for platform — only DM sent.
	mock := &mockNotifier{}
	sc := StrategyConfig{
		ID:       "test-spot-sma",
		Type:     "spot",
		Platform: "binanceus",
		Args:     []string{"sma", "BTC/USDT", "1h", "--mode=paper"},
	}
	state := &StrategyState{
		TradeHistory: []Trade{testTrade()},
	}
	var mu sync.RWMutex
	notifier := &MultiNotifier{
		backends: []notifierBackend{
			{
				notifier:   mock,
				ownerID:    "owner123",
				channels:   map[string]string{}, // no channels configured
				dmChannels: map[string]string{"binanceus-paper": "owner123"},
			},
		},
	}

	sendTradeAlerts(sc, state, 1, &mu, notifier)

	if len(mock.dms) != 1 {
		t.Errorf("expected 1 DM message, got %d", len(mock.dms))
	}
	if len(mock.messages) != 0 {
		t.Errorf("expected no channel messages, got %d", len(mock.messages))
	}
}

func TestSendTradeAlerts_ChannelOnly(t *testing.T) {
	// Channel configured but DM disabled — only channel message sent.
	mock := &mockNotifier{}
	sc := StrategyConfig{
		ID:       "test-spot-sma",
		Type:     "spot",
		Platform: "binanceus",
		Args:     []string{"sma", "BTC/USDT", "1h", "--mode=paper"},
	}
	state := &StrategyState{
		TradeHistory: []Trade{testTrade()},
	}
	var mu sync.RWMutex
	notifier := &MultiNotifier{
		backends: []notifierBackend{
			{
				notifier: mock,
				ownerID:  "owner123",
				channels: map[string]string{"spot": "ch-spot-123"},
			},
		},
	}

	sendTradeAlerts(sc, state, 1, &mu, notifier)

	if len(mock.dms) != 0 {
		t.Errorf("expected no DM messages, got %d", len(mock.dms))
	}
	if len(mock.messages) != 1 {
		t.Errorf("expected 1 channel message, got %d", len(mock.messages))
	}
}

func TestSendTradeAlerts_NeitherEnabled(t *testing.T) {
	// No DM enabled, no channel configured — nothing sent.
	mock := &mockNotifier{}
	sc := StrategyConfig{
		ID:       "test-spot-sma",
		Type:     "spot",
		Platform: "binanceus",
		Args:     []string{"sma", "BTC/USDT", "1h", "--mode=paper"},
	}
	state := &StrategyState{
		TradeHistory: []Trade{testTrade()},
	}
	var mu sync.RWMutex
	notifier := &MultiNotifier{
		backends: []notifierBackend{
			{
				notifier: mock,
				ownerID:  "owner123",
				channels: map[string]string{}, // no channels configured
			},
		},
	}

	sendTradeAlerts(sc, state, 1, &mu, notifier)

	if len(mock.dms) != 0 {
		t.Errorf("expected no DM messages, got %d", len(mock.dms))
	}
	if len(mock.messages) != 0 {
		t.Errorf("expected no channel messages, got %d", len(mock.messages))
	}
}

func TestSendTradeAlerts_NoChannelForPlatform(t *testing.T) {
	// Channel map has "spot" but not "hyperliquid" or "perps" — no messages.
	mock := &mockNotifier{}
	sc := StrategyConfig{
		ID:       "hl-perps-sma",
		Type:     "perps",
		Platform: "hyperliquid",
		Args:     []string{"sma", "BTC", "1h", "--mode=paper"},
	}
	state := &StrategyState{
		TradeHistory: []Trade{testTrade()},
	}
	var mu sync.RWMutex
	notifier := &MultiNotifier{
		backends: []notifierBackend{
			{
				notifier: mock,
				ownerID:  "owner123",
				channels: map[string]string{"spot": "ch-spot-123"},
			},
		},
	}

	sendTradeAlerts(sc, state, 1, &mu, notifier)

	if len(mock.dms) != 0 {
		t.Errorf("expected no DM messages, got %d", len(mock.dms))
	}
	if len(mock.messages) != 0 {
		t.Errorf("expected no channel messages, got %d", len(mock.messages))
	}
}

func TestSendTradeAlerts_LiveChannelRouting(t *testing.T) {
	// Live trades should post to both the primary channel and the <platform>-live channel.
	mock := &mockNotifier{}
	sc := StrategyConfig{
		ID:       "hl-sma-btc",
		Type:     "perps",
		Platform: "hyperliquid",
		Args:     []string{"sma", "BTC", "1h", "--mode=live"},
	}
	state := &StrategyState{
		TradeHistory: []Trade{testTrade()},
	}
	var mu sync.RWMutex
	notifier := &MultiNotifier{
		backends: []notifierBackend{
			{
				notifier:   mock,
				ownerID:    "owner123",
				channels:   map[string]string{"hyperliquid": "ch-hl", "hyperliquid-live": "ch-hl-live"},
				dmChannels: map[string]string{"hyperliquid": "owner123"},
			},
		},
	}

	sendTradeAlerts(sc, state, 1, &mu, notifier)

	// Should get 1 DM + 2 channel messages (primary + live)
	if len(mock.dms) != 1 {
		t.Errorf("expected 1 DM, got %d", len(mock.dms))
	}
	if len(mock.messages) != 2 {
		t.Errorf("expected 2 channel messages (primary + live), got %d", len(mock.messages))
	}
	channels := map[string]bool{}
	for _, m := range mock.messages {
		channels[m.channelID] = true
	}
	if !channels["ch-hl"] {
		t.Error("expected message to primary channel ch-hl")
	}
	if !channels["ch-hl-live"] {
		t.Error("expected message to live channel ch-hl-live")
	}
}

func TestSendTradeAlerts_LiveChannelDedup(t *testing.T) {
	// When <platform>-live resolves to the same channel as <platform>, no double-post.
	mock := &mockNotifier{}
	sc := StrategyConfig{
		ID:       "hl-sma-btc",
		Type:     "perps",
		Platform: "hyperliquid",
		Args:     []string{"sma", "BTC", "1h", "--mode=live"},
	}
	state := &StrategyState{
		TradeHistory: []Trade{testTrade()},
	}
	var mu sync.RWMutex
	notifier := &MultiNotifier{
		backends: []notifierBackend{
			{
				notifier: mock,
				ownerID:  "",
				channels: map[string]string{"hyperliquid": "ch-hl", "hyperliquid-live": "ch-hl"}, // same channel
			},
		},
	}

	sendTradeAlerts(sc, state, 1, &mu, notifier)

	if len(mock.messages) != 1 {
		t.Errorf("expected 1 channel message (dedup), got %d", len(mock.messages))
	}
}

func TestSendTradeAlerts_PaperNoLiveChannel(t *testing.T) {
	// Paper trades should NOT post to the <platform>-live channel; they use
	// <platform>-paper (or fall back to base platform channel).
	mock := &mockNotifier{}
	sc := StrategyConfig{
		ID:       "hl-sma-btc",
		Type:     "perps",
		Platform: "hyperliquid",
		Args:     []string{"sma", "BTC", "1h", "--mode=paper"},
	}
	state := &StrategyState{
		TradeHistory: []Trade{testTrade()},
	}
	var mu sync.RWMutex
	notifier := &MultiNotifier{
		backends: []notifierBackend{
			{
				notifier: mock,
				ownerID:  "",
				channels: map[string]string{"hyperliquid": "ch-hl", "hyperliquid-live": "ch-hl-live"},
			},
		},
	}

	sendTradeAlerts(sc, state, 1, &mu, notifier)

	if len(mock.messages) != 1 {
		t.Errorf("expected 1 channel message (primary only), got %d", len(mock.messages))
	}
	if len(mock.messages) > 0 && mock.messages[0].channelID != "ch-hl" {
		t.Errorf("expected message to primary channel ch-hl, got %s", mock.messages[0].channelID)
	}
}

func TestSendTradeAlerts_PaperChannelRouting(t *testing.T) {
	// Paper trades should route to <platform>-paper channel when configured.
	mock := &mockNotifier{}
	sc := StrategyConfig{
		ID:       "hl-sma-btc",
		Type:     "perps",
		Platform: "hyperliquid",
		Args:     []string{"sma", "BTC", "1h", "--mode=paper"},
	}
	state := &StrategyState{
		TradeHistory: []Trade{testTrade()},
	}
	var mu sync.RWMutex
	notifier := &MultiNotifier{
		backends: []notifierBackend{
			{
				notifier: mock,
				ownerID:  "",
				channels: map[string]string{
					"hyperliquid":       "ch-hl-live",
					"hyperliquid-paper": "ch-hl-paper",
				},
			},
		},
	}

	sendTradeAlerts(sc, state, 1, &mu, notifier)

	if len(mock.messages) != 1 {
		t.Errorf("expected 1 channel message, got %d", len(mock.messages))
	}
	if len(mock.messages) > 0 && mock.messages[0].channelID != "ch-hl-paper" {
		t.Errorf("expected message to paper channel ch-hl-paper, got %s", mock.messages[0].channelID)
	}
}

func TestSendTradeAlerts_PaperFallbackToBase(t *testing.T) {
	// Paper trades fall back to base platform channel when no <platform>-paper key exists.
	mock := &mockNotifier{}
	sc := StrategyConfig{
		ID:       "hl-sma-btc",
		Type:     "perps",
		Platform: "hyperliquid",
		Args:     []string{"sma", "BTC", "1h", "--mode=paper"},
	}
	state := &StrategyState{
		TradeHistory: []Trade{testTrade()},
	}
	var mu sync.RWMutex
	notifier := &MultiNotifier{
		backends: []notifierBackend{
			{
				notifier: mock,
				ownerID:  "",
				channels: map[string]string{"hyperliquid": "ch-hl"},
			},
		},
	}

	sendTradeAlerts(sc, state, 1, &mu, notifier)

	if len(mock.messages) != 1 {
		t.Errorf("expected 1 channel message, got %d", len(mock.messages))
	}
	if len(mock.messages) > 0 && mock.messages[0].channelID != "ch-hl" {
		t.Errorf("expected message to base channel ch-hl, got %s", mock.messages[0].channelID)
	}
}

func TestSendTradeAlerts_DMChannelPaper(t *testing.T) {
	mock := &mockNotifier{}
	sc := StrategyConfig{
		ID:       "hl-sma-btc",
		Type:     "perps",
		Platform: "hyperliquid",
		Args:     []string{"sma", "BTC", "1h", "--mode=paper"},
	}
	state := &StrategyState{TradeHistory: []Trade{testTrade()}}
	var mu sync.RWMutex
	notifier := &MultiNotifier{
		backends: []notifierBackend{
			{
				notifier:   mock,
				dmChannels: map[string]string{"hyperliquid-paper": "user-paper-dm"},
			},
		},
	}
	sendTradeAlerts(sc, state, 1, &mu, notifier)
	if len(mock.dms) != 1 || mock.dms[0].userID != "user-paper-dm" {
		t.Errorf("expected 1 DM to user-paper-dm, got %#v", mock.dms)
	}
}

func TestSendTradeAlerts_DMChannelLive(t *testing.T) {
	mock := &mockNotifier{}
	sc := StrategyConfig{
		ID:       "hl-sma-btc",
		Type:     "perps",
		Platform: "hyperliquid",
		Args:     []string{"sma", "BTC", "1h", "--mode=live"},
	}
	state := &StrategyState{TradeHistory: []Trade{testTrade()}}
	var mu sync.RWMutex
	notifier := &MultiNotifier{
		backends: []notifierBackend{
			{
				notifier:   mock,
				dmChannels: map[string]string{"hyperliquid": "user-live-dm"},
			},
		},
	}
	sendTradeAlerts(sc, state, 1, &mu, notifier)
	if len(mock.dms) != 1 || mock.dms[0].userID != "user-live-dm" {
		t.Errorf("expected 1 DM to user-live-dm, got %#v", mock.dms)
	}
}

func TestSendTradeAlerts_DMMissingKey(t *testing.T) {
	// Paper trade but only live key in dm_channels — no DM, no channel.
	mock := &mockNotifier{}
	sc := StrategyConfig{
		ID:       "hl-sma-btc",
		Type:     "perps",
		Platform: "hyperliquid",
		Args:     []string{"sma", "BTC", "1h", "--mode=paper"},
	}
	state := &StrategyState{TradeHistory: []Trade{testTrade()}}
	var mu sync.RWMutex
	notifier := &MultiNotifier{
		backends: []notifierBackend{
			{
				notifier:   mock,
				dmChannels: map[string]string{"hyperliquid": "only-live"},
				channels:   map[string]string{},
			},
		},
	}
	sendTradeAlerts(sc, state, 1, &mu, notifier)
	if len(mock.dms) != 0 || len(mock.messages) != 0 {
		t.Errorf("expected no messages, dms=%d messages=%d", len(mock.dms), len(mock.messages))
	}
}

func TestSendTradeAlerts_DMChannelFallback(t *testing.T) {
	mock := &mockNotifier{failSendDM: true}
	sc := StrategyConfig{
		ID:       "hl-sma-btc",
		Type:     "perps",
		Platform: "hyperliquid",
		Args:     []string{"sma", "BTC", "1h", "--mode=paper"},
	}
	state := &StrategyState{TradeHistory: []Trade{testTrade()}}
	var mu sync.RWMutex
	notifier := &MultiNotifier{
		backends: []notifierBackend{
			{
				notifier:   mock,
				dmChannels: map[string]string{"hyperliquid-paper": "private-log-channel"},
			},
		},
	}
	sendTradeAlerts(sc, state, 1, &mu, notifier)
	if len(mock.dms) != 0 {
		t.Errorf("expected SendDM to fail without recording DM, got %d dms", len(mock.dms))
	}
	if len(mock.messages) != 1 || mock.messages[0].channelID != "private-log-channel" {
		t.Errorf("expected 1 channel message to private-log-channel, got %#v", mock.messages)
	}
}

func TestExecuteHyperliquidResult_StampsExchangeData(t *testing.T) {
	s := &StrategyState{
		ID:              "hl-test-btc",
		Type:            "perps",
		Platform:        "hyperliquid",
		Cash:            1000,
		InitialCapital:  1000,
		Positions:       make(map[string]*Position),
		OptionPositions: make(map[string]*OptionPosition),
		TradeHistory:    []Trade{},
		RiskState:       RiskState{PeakValue: 1000},
	}
	sc := StrategyConfig{ID: "hl-test-btc", Type: "perps", Platform: "hyperliquid"}
	result := &HyperliquidResult{Signal: 1, Symbol: "BTC", Price: 50000}
	execResult := &HyperliquidExecuteResult{
		Execution: &HyperliquidExecution{
			Action: "buy", Symbol: "BTC", Size: 0.015,
			Fill: &HyperliquidFill{AvgPx: 50000.5, TotalSz: 0.015, OID: 1234567890, Fee: 1.75},
		},
		Platform: "hyperliquid",
	}

	lm, _ := NewLogManager("")
	logger, _ := lm.GetStrategyLogger("test")
	defer logger.Close()

	trades, _ := executeHyperliquidResult(sc, s, nil, result, execResult, "BUY", 50000, logger)
	if trades != 1 {
		t.Fatalf("trades = %d, want 1", trades)
	}
	if len(s.TradeHistory) != 1 {
		t.Fatalf("TradeHistory len = %d, want 1", len(s.TradeHistory))
	}

	tr := s.TradeHistory[0]
	if tr.ExchangeOrderID != "1234567890" {
		t.Errorf("ExchangeOrderID = %q, want %q", tr.ExchangeOrderID, "1234567890")
	}
	if tr.ExchangeFee != 1.75 {
		t.Errorf("ExchangeFee = %g, want 1.75", tr.ExchangeFee)
	}
}

func TestExecuteHyperliquidResult_PaperModeNoExchangeData(t *testing.T) {
	s := &StrategyState{
		ID:              "hl-paper-btc",
		Type:            "perps",
		Platform:        "hyperliquid",
		Cash:            1000,
		InitialCapital:  1000,
		Positions:       make(map[string]*Position),
		OptionPositions: make(map[string]*OptionPosition),
		TradeHistory:    []Trade{},
		RiskState:       RiskState{PeakValue: 1000},
	}
	sc := StrategyConfig{ID: "hl-paper-btc", Type: "perps", Platform: "hyperliquid"}
	result := &HyperliquidResult{Signal: 1, Symbol: "BTC", Price: 50000}

	lm, _ := NewLogManager("")
	logger, _ := lm.GetStrategyLogger("test")
	defer logger.Close()

	// Paper mode: execResult is nil
	trades, _ := executeHyperliquidResult(sc, s, nil, result, nil, "BUY", 50000, logger)
	if trades != 1 {
		t.Fatalf("trades = %d, want 1", trades)
	}

	tr := s.TradeHistory[0]
	if tr.ExchangeOrderID != "" {
		t.Errorf("ExchangeOrderID should be empty in paper mode, got %q", tr.ExchangeOrderID)
	}
	if tr.ExchangeFee != 0 {
		t.Errorf("ExchangeFee should be 0 in paper mode, got %g", tr.ExchangeFee)
	}
}

func TestExecuteOKXResult_PerpsStampsExchangeData(t *testing.T) {
	s := &StrategyState{
		ID:              "okx-perps-btc",
		Type:            "perps",
		Platform:        "okx",
		Cash:            1000,
		InitialCapital:  1000,
		Positions:       make(map[string]*Position),
		OptionPositions: make(map[string]*OptionPosition),
		TradeHistory:    []Trade{},
		RiskState:       RiskState{PeakValue: 1000},
	}
	sc := StrategyConfig{ID: "okx-perps-btc", Type: "perps", Platform: "okx", Leverage: 2}
	result := &OKXResult{Signal: 1, Symbol: "BTC", Price: 50000}
	execResult := &OKXExecuteResult{
		Execution: &OKXExecution{
			Action: "buy", Symbol: "BTC", Size: 0.02,
			Fill: &OKXFill{AvgPx: 50000.5, TotalSz: 0.02, OID: "okx-perps-oid", Fee: 1.25},
		},
		Platform: "okx",
	}

	lm, _ := NewLogManager("")
	logger, _ := lm.GetStrategyLogger("test")
	defer logger.Close()

	trades, _ := executeOKXResult(sc, s, nil, result, execResult, "BUY", 50000, logger)
	if trades != 1 {
		t.Fatalf("trades = %d, want 1", trades)
	}
	tr := s.TradeHistory[0]
	if tr.ExchangeOrderID != "okx-perps-oid" {
		t.Errorf("ExchangeOrderID = %q, want okx-perps-oid", tr.ExchangeOrderID)
	}
	if tr.ExchangeFee != 1.25 {
		t.Errorf("ExchangeFee = %g, want 1.25", tr.ExchangeFee)
	}
}

func TestExecuteOKXResult_SpotStampsExchangeData(t *testing.T) {
	s := &StrategyState{
		ID:              "okx-spot-btc",
		Type:            "spot",
		Platform:        "okx",
		Cash:            1000,
		InitialCapital:  1000,
		Positions:       make(map[string]*Position),
		OptionPositions: make(map[string]*OptionPosition),
		TradeHistory:    []Trade{},
		RiskState:       RiskState{PeakValue: 1000},
	}
	sc := StrategyConfig{ID: "okx-spot-btc", Type: "spot", Platform: "okx"}
	result := &OKXResult{Signal: 1, Symbol: "BTC", Price: 50000}
	execResult := &OKXExecuteResult{
		Execution: &OKXExecution{
			Action: "buy", Symbol: "BTC", Size: 0.01,
			Fill: &OKXFill{AvgPx: 50000.5, TotalSz: 0.01, OID: "okx-spot-oid", Fee: 0.18},
		},
		Platform: "okx",
	}

	lm, _ := NewLogManager("")
	logger, _ := lm.GetStrategyLogger("test")
	defer logger.Close()

	trades, _ := executeOKXResult(sc, s, nil, result, execResult, "BUY", 50000, logger)
	if trades != 1 {
		t.Fatalf("trades = %d, want 1", trades)
	}
	tr := s.TradeHistory[0]
	if tr.ExchangeOrderID != "okx-spot-oid" {
		t.Errorf("ExchangeOrderID = %q, want okx-spot-oid", tr.ExchangeOrderID)
	}
	if tr.ExchangeFee != 0.18 {
		t.Errorf("ExchangeFee = %g, want 0.18", tr.ExchangeFee)
	}
}

func TestExecuteRobinhoodResult_StampsExchangeData(t *testing.T) {
	s := &StrategyState{
		ID:              "rh-momentum-btc",
		Type:            "spot",
		Platform:        "robinhood",
		Cash:            1000,
		InitialCapital:  1000,
		Positions:       make(map[string]*Position),
		OptionPositions: make(map[string]*OptionPosition),
		TradeHistory:    []Trade{},
		RiskState:       RiskState{PeakValue: 1000},
	}
	sc := StrategyConfig{ID: "rh-momentum-btc", Type: "spot", Platform: "robinhood"}
	result := &RobinhoodResult{Signal: 1, Symbol: "BTC", Price: 50000}
	execResult := &RobinhoodExecuteResult{
		Execution: &RobinhoodExecution{
			Action: "buy", Symbol: "BTC", AmountUSD: 500,
			Fill: &RobinhoodFill{AvgPx: 50000.5, Quantity: 0.01, OID: "rh-live-oid", Fee: 0.07},
		},
		Platform: "robinhood",
	}

	lm, _ := NewLogManager("")
	logger, _ := lm.GetStrategyLogger("test")
	defer logger.Close()

	trades, _ := executeRobinhoodResult(sc, s, nil, result, execResult, "BUY", 50000, logger)
	if trades != 1 {
		t.Fatalf("trades = %d, want 1", trades)
	}
	tr := s.TradeHistory[0]
	if tr.ExchangeOrderID != "rh-live-oid" {
		t.Errorf("ExchangeOrderID = %q, want rh-live-oid", tr.ExchangeOrderID)
	}
	if tr.ExchangeFee != 0.07 {
		t.Errorf("ExchangeFee = %g, want 0.07", tr.ExchangeFee)
	}
}

func TestStampEntryATRIfOpened(t *testing.T) {
	newState := func(avgCost, existingATR float64) *StrategyState {
		s := &StrategyState{Positions: map[string]*Position{
			"BTC": {AvgCost: avgCost, EntryATR: existingATR},
		}}
		return s
	}
	indicators := func(atr float64) map[string]interface{} {
		return map[string]interface{}{"atr": atr}
	}

	cases := []struct {
		name    string
		s       *StrategyState
		symbol  string
		inds    map[string]interface{}
		wantATR float64
	}{
		{"stamps valid atr on open", newState(50000, 0), "BTC", indicators(500), 500},
		{"backfills when no new trade this cycle", newState(50000, 0), "BTC", indicators(500), 500},
		{"no-op when atr already set", newState(50000, 300), "BTC", indicators(500), 300},
		{"no-op when atr already set and no new trade", newState(50000, 300), "BTC", indicators(500), 300},
		{"rejects zero atr", newState(50000, 0), "BTC", indicators(0), 0},
		{"rejects negative atr", newState(50000, 0), "BTC", indicators(-1), 0},
		{"rejects NaN atr", newState(50000, 0), "BTC", indicators(math.NaN()), 0},
		{"rejects +Inf atr", newState(50000, 0), "BTC", indicators(math.Inf(1)), 0},
		{"rejects atr > 50% avgCost", newState(50000, 0), "BTC", indicators(25001), 0},
		{"accepts atr == 50% avgCost boundary", newState(50000, 0), "BTC", indicators(25000), 25000},
		{"no upper-bound check when avgCost == 0", newState(0, 0), "BTC", indicators(999), 999},
		{"no-op for missing symbol", newState(50000, 0), "ETH", indicators(500), 0},
		{"no-op for nil state", nil, "BTC", indicators(500), 0},
		{"no-op for nil indicators", newState(50000, 0), "BTC", nil, 0},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stampEntryATRIfOpened(tc.s, tc.symbol, tc.inds)
			if tc.s == nil {
				return
			}
			if pos := tc.s.Positions[tc.symbol]; pos != nil {
				if pos.EntryATR != tc.wantATR {
					t.Errorf("EntryATR = %g, want %g", pos.EntryATR, tc.wantATR)
				}
			} else if tc.wantATR != 0 {
				t.Errorf("position for %s not found, want EntryATR %g", tc.symbol, tc.wantATR)
			}
		})
	}
}

func TestExecuteTopStepResult_StampsExchangeData(t *testing.T) {
	s := &StrategyState{
		ID:              "ts-momentum-es",
		Type:            "futures",
		Platform:        "topstep",
		Cash:            10000,
		InitialCapital:  10000,
		Positions:       make(map[string]*Position),
		OptionPositions: make(map[string]*OptionPosition),
		TradeHistory:    []Trade{},
		RiskState:       RiskState{PeakValue: 10000},
	}
	sc := StrategyConfig{
		ID:       "ts-momentum-es",
		Type:     "futures",
		Platform: "topstep",
		FuturesConfig: &FuturesConfig{
			FeePerContract: 2.5,
			MaxContracts:   5,
		},
	}
	spec := ContractSpec{TickSize: 0.25, TickValue: 12.5, Multiplier: 50, Margin: 500}
	result := &TopStepResult{Signal: 1, Symbol: "ES", Price: 5000, ContractSpec: spec}
	execResult := &TopStepExecuteResult{
		Execution: &TopStepExecution{
			Action: "buy", Symbol: "ES", Contracts: 2,
			Fill: &TopStepFill{AvgPx: 5000.25, TotalContracts: 2, OID: "ts-live-oid", Fee: 4.12},
		},
		Platform: "topstep",
	}

	lm, _ := NewLogManager("")
	logger, _ := lm.GetStrategyLogger("test")
	defer logger.Close()

	trades, _ := executeTopStepResult(sc, s, nil, result, execResult, "BUY", 5000, logger)
	if trades != 1 {
		t.Fatalf("trades = %d, want 1", trades)
	}
	tr := s.TradeHistory[0]
	if tr.ExchangeOrderID != "ts-live-oid" {
		t.Errorf("ExchangeOrderID = %q, want ts-live-oid", tr.ExchangeOrderID)
	}
	if tr.ExchangeFee != 4.12 {
		t.Errorf("ExchangeFee = %g, want 4.12", tr.ExchangeFee)
	}
}

package main

import (
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
				notifier:           mock,
				ownerID:            "owner123",
				channels:           map[string]string{"spot": "ch-spot-123"},
				dmPaperTrades:      true,
				channelPaperTrades: true,
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
				notifier:           mock,
				ownerID:            "owner123",
				channels:           map[string]string{"spot": "ch-spot-123"},
				dmPaperTrades:      true,
				channelPaperTrades: false,
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
				notifier:           mock,
				ownerID:            "owner123",
				channels:           map[string]string{"spot": "ch-spot-123"},
				dmPaperTrades:      false,
				channelPaperTrades: true,
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
				notifier:           mock,
				ownerID:            "owner123",
				channels:           map[string]string{"spot": "ch-spot-123"},
				dmPaperTrades:      false,
				channelPaperTrades: false,
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

func TestSendTradeAlerts_ChannelEnabledButNotConfigured(t *testing.T) {
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
	// Channel map has "spot" but not "hyperliquid" or "perps"
	notifier := &MultiNotifier{
		backends: []notifierBackend{
			{
				notifier:           mock,
				ownerID:            "owner123",
				channels:           map[string]string{"spot": "ch-spot-123"},
				dmPaperTrades:      false,
				channelPaperTrades: true,
			},
		},
	}

	sendTradeAlerts(sc, state, 1, &mu, notifier)

	// Channel enabled but no channel for platform=hyperliquid type=perps, so no messages sent
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
				notifier:          mock,
				ownerID:           "owner123",
				channels:          map[string]string{"hyperliquid": "ch-hl", "hyperliquid-live": "ch-hl-live"},
				dmLiveTrades:      true,
				channelLiveTrades: true,
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
				notifier:          mock,
				ownerID:           "",
				channels:          map[string]string{"hyperliquid": "ch-hl", "hyperliquid-live": "ch-hl"}, // same channel
				channelLiveTrades: true,
			},
		},
	}

	sendTradeAlerts(sc, state, 1, &mu, notifier)

	if len(mock.messages) != 1 {
		t.Errorf("expected 1 channel message (dedup), got %d", len(mock.messages))
	}
}

func TestSendTradeAlerts_PaperNoLiveChannel(t *testing.T) {
	// Paper trades should NOT post to the <platform>-live channel.
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
				notifier:           mock,
				ownerID:            "",
				channels:           map[string]string{"hyperliquid": "ch-hl", "hyperliquid-live": "ch-hl-live"},
				channelPaperTrades: true,
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

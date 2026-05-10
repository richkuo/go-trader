package main

import (
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"testing"
	"time"
)

func TestResolveChannel(t *testing.T) {
	channels := map[string]string{
		"spot":        "ch-spot",
		"hyperliquid": "ch-hl",
		"options":     "ch-opts",
	}

	// platform match takes priority
	if got := resolveChannel(channels, "hyperliquid", "perps"); got != "ch-hl" {
		t.Errorf("expected ch-hl, got %s", got)
	}
	// fall through to stratType
	if got := resolveChannel(channels, "binanceus", "spot"); got != "ch-spot" {
		t.Errorf("expected ch-spot, got %s", got)
	}
	// options type
	if got := resolveChannel(channels, "deribit", "options"); got != "ch-opts" {
		t.Errorf("expected ch-opts for deribit options, got %s", got)
	}
	// unknown → empty
	if got := resolveChannel(channels, "unknown", "unknown"); got != "" {
		t.Errorf("expected empty, got %s", got)
	}
}

func TestResolveTradeChannel(t *testing.T) {
	channels := map[string]string{
		"hyperliquid":       "ch-hl",
		"hyperliquid-paper": "ch-hl-paper",
		"spot":              "ch-spot",
	}

	// Paper trade: uses <platform>-paper when present.
	if got := resolveTradeChannel(channels, "hyperliquid", "perps", false); got != "ch-hl-paper" {
		t.Errorf("paper with -paper key: expected ch-hl-paper, got %s", got)
	}

	// Live trade: uses base platform key (ignores -paper).
	if got := resolveTradeChannel(channels, "hyperliquid", "perps", true); got != "ch-hl" {
		t.Errorf("live trade: expected ch-hl, got %s", got)
	}

	// Paper trade with no -paper key: falls back to base platform.
	if got := resolveTradeChannel(channels, "binanceus", "spot", false); got != "ch-spot" {
		t.Errorf("paper fallback to stratType: expected ch-spot, got %s", got)
	}

	// Paper trade with no channel at all.
	if got := resolveTradeChannel(channels, "unknown", "unknown", false); got != "" {
		t.Errorf("paper no channel: expected empty, got %s", got)
	}

	// Live trade falls back to stratType.
	if got := resolveTradeChannel(channels, "binanceus", "spot", true); got != "ch-spot" {
		t.Errorf("live fallback to stratType: expected ch-spot, got %s", got)
	}
}

func TestChannelKeyFromID(t *testing.T) {
	channels := map[string]string{
		"spot":        "111",
		"hyperliquid": "222",
	}
	if got := channelKeyFromID(channels, "111"); got != "spot" {
		t.Errorf("expected spot, got %s", got)
	}
	if got := channelKeyFromID(channels, "222"); got != "hyperliquid" {
		t.Errorf("expected hyperliquid, got %s", got)
	}
	// unknown channel ID falls back to itself
	if got := channelKeyFromID(channels, "999"); got != "999" {
		t.Errorf("expected 999, got %s", got)
	}
}

func TestIsOptionsType(t *testing.T) {
	spot := []StrategyConfig{{Type: "spot"}, {Type: "perps"}}
	opts := []StrategyConfig{{Type: "spot"}, {Type: "options"}}
	if isOptionsType(spot) {
		t.Error("expected false for spot/perps only")
	}
	if !isOptionsType(opts) {
		t.Error("expected true when options present")
	}
}

func TestExtractAsset(t *testing.T) {
	cases := []struct {
		sc   StrategyConfig
		want string
	}{
		// spot: Args[1] is "BTC/USDT" → strip suffix → "BTC"
		{StrategyConfig{Type: "spot", Args: []string{"sma_crossover", "BTC/USDT"}}, "BTC"},
		// options: Args[1] is the underlying symbol
		{StrategyConfig{Type: "options", Args: []string{"wheel", "ETH", "--platform=deribit"}}, "ETH"},
		// perps: Args[1] is coin name
		{StrategyConfig{Type: "perps", Args: []string{"momentum", "SOL", "1h"}}, "SOL"},
		// perps BNB
		{StrategyConfig{Type: "perps", Args: []string{"rsi", "BNB", "1h"}}, "BNB"},
		// empty args → ""
		{StrategyConfig{Type: "spot", Args: []string{}}, ""},
		// only one arg → ""
		{StrategyConfig{Type: "perps", Args: []string{"strategy"}}, ""},
	}
	for _, c := range cases {
		got := extractAsset(c.sc)
		if got != c.want {
			t.Errorf("extractAsset(%v) = %q, want %q", c.sc.Args, got, c.want)
		}
	}
}

func TestGroupByAsset(t *testing.T) {
	strats := []StrategyConfig{
		{ID: "hl-rsi-eth", Type: "perps", Args: []string{"rsi", "ETH", "1h"}},
		{ID: "hl-mom-btc", Type: "perps", Args: []string{"momentum", "BTC", "1h"}},
		{ID: "hl-ema-sol", Type: "perps", Args: []string{"ema", "SOL", "1h"}},
		{ID: "hl-rsi-bnb", Type: "perps", Args: []string{"rsi", "BNB", "1h"}},
		{ID: "hl-sma-btc", Type: "perps", Args: []string{"sma", "BTC", "1h"}},
	}
	groups, keys := groupByAsset(strats)

	// 4 distinct assets
	if len(keys) != 4 {
		t.Fatalf("expected 4 asset keys, got %d: %v", len(keys), keys)
	}
	// BTC first, ETH second, SOL third, BNB fourth
	if keys[0] != "BTC" || keys[1] != "ETH" || keys[2] != "SOL" || keys[3] != "BNB" {
		t.Errorf("unexpected key order: %v", keys)
	}
	// BTC group has 2 strategies
	if len(groups["BTC"]) != 2 {
		t.Errorf("expected 2 BTC strategies, got %d", len(groups["BTC"]))
	}

	// Single asset case
	single := []StrategyConfig{
		{ID: "hl-rsi-eth", Type: "perps", Args: []string{"rsi", "ETH", "1h"}},
	}
	_, keys2 := groupByAsset(single)
	if len(keys2) != 1 || keys2[0] != "ETH" {
		t.Errorf("single asset: expected [ETH], got %v", keys2)
	}
}

func TestFormatCategorySummary_WithAsset(t *testing.T) {
	strats := []StrategyConfig{
		{ID: "hl-rsi-btc", Type: "perps", Args: []string{"rsi", "BTC", "1h"}, Capital: 1000},
	}
	state := &AppState{
		Strategies: map[string]*StrategyState{
			"hl-rsi-btc": {Cash: 1000},
		},
	}
	prices := map[string]float64{"BTC/USDT": 50000, "ETH/USDT": 3000}

	// With asset — title should contain " — BTC" and only BTC price shown
	msgs := FormatCategorySummary(1, 0, 1, 0, 1000, prices, nil, strats, state, "hyperliquid", "BTC", 600, 0, nil)
	msg := strings.Join(msgs, "\n")
	if !strings.Contains(msg, "— BTC") {
		t.Errorf("expected '— BTC' in title, got:\n%s", msg)
	}
	if strings.Contains(msg, "ETH") {
		t.Errorf("ETH price should be filtered out for asset=BTC, got:\n%s", msg)
	}

	// Without asset — no suffix in title
	msgs2 := FormatCategorySummary(1, 0, 1, 0, 1000, prices, nil, strats, state, "hyperliquid", "", 600, 0, nil)
	msg2 := strings.Join(msgs2, "\n")
	if strings.Contains(msg2, "— ") {
		t.Errorf("expected no asset suffix when asset='', got:\n%s", msg2)
	}
}

// TestFormatCategorySummary_VersionSuffix guards that summary and trade titles
// include the package-level Version so /upgrade can surface which revision is
// running. Also covers the empty-Version edge case where the suffix should be
// omitted entirely (no trailing "()").
func TestFormatCategorySummary_VersionSuffix(t *testing.T) {
	strats := []StrategyConfig{
		{ID: "hl-rsi-btc", Type: "perps", Args: []string{"rsi", "BTC", "1h"}, Capital: 1000},
	}
	state := &AppState{
		Strategies: map[string]*StrategyState{
			"hl-rsi-btc": {Cash: 1000},
		},
	}
	prices := map[string]float64{"BTC/USDT": 50000}

	orig := Version
	defer func() { Version = orig }()

	Version = "v9.9.9-test"
	msgs := FormatCategorySummary(1, 0, 1, 0, 1000, prices, nil, strats, state, "hyperliquid", "BTC", 600, 0, nil)
	summary := strings.Join(msgs, "\n")
	if !strings.Contains(summary, Version) {
		t.Errorf("expected version %q in summary title, got:\n%s", Version, summary)
	}

	msgs = FormatCategorySummary(1, 0, 1, 3, 1000, prices, nil, strats, state, "hyperliquid", "BTC", 600, 0, nil)
	trades := strings.Join(msgs, "\n")
	if !strings.Contains(trades, Version) {
		t.Errorf("expected version %q in trades title, got:\n%s", Version, trades)
	}

	Version = ""
	msgs = FormatCategorySummary(1, 0, 1, 0, 1000, prices, nil, strats, state, "hyperliquid", "BTC", 600, 0, nil)
	empty := strings.Join(msgs, "\n")
	if strings.Contains(empty, "()") {
		t.Errorf("empty Version should omit the suffix, got:\n%s", empty)
	}
}

func TestFormatCategorySummary_CircuitBreakerActive(t *testing.T) {
	strats := []StrategyConfig{
		{ID: "hl-rsi-btc", Type: "perps", Args: []string{"rsi", "BTC", "1h"}, Capital: 1000},
		{ID: "hl-sma-btc", Type: "perps", Args: []string{"sma", "BTC", "1h"}, Capital: 1000},
	}
	state := &AppState{
		Strategies: map[string]*StrategyState{
			"hl-rsi-btc": {
				Cash: 1000,
				RiskState: RiskState{
					CircuitBreaker:      true,
					CircuitBreakerUntil: time.Now().UTC().Add(30 * time.Minute),
				},
			},
			"hl-sma-btc": {Cash: 1000},
		},
	}
	prices := map[string]float64{"BTC/USDT": 50000}

	msgs := FormatCategorySummary(1, 0, 2, 0, 2000, prices, nil, strats, state, "hyperliquid", "BTC", 600, 0, nil)
	msg := strings.Join(msgs, "\n")

	if !strings.Contains(msg, "Circuit breaker active") {
		t.Errorf("expected circuit breaker warning, got:\n%s", msg)
	}
	if !strings.Contains(msg, "hl-rsi-btc") {
		t.Errorf("expected hl-rsi-btc in circuit breaker list, got:\n%s", msg)
	}
	if !strings.Contains(msg, "resumes in") {
		t.Errorf("expected 'resumes in' time remaining, got:\n%s", msg)
	}
	// hl-sma-btc should NOT be in the circuit breaker list
	if strings.Contains(msg, "hl-sma-btc") && strings.Contains(msg, "hl-sma-btc (resumes") {
		t.Errorf("hl-sma-btc should not have circuit breaker warning, got:\n%s", msg)
	}
	if strings.Contains(msg, "Trading active") {
		t.Errorf("should not show 'Trading active' when circuit breaker is active, got:\n%s", msg)
	}
}

func TestFormatCategorySummary_StrategiesSortedByID(t *testing.T) {
	// Issue #354: table rows should follow strategy ID order, not config order.
	strats := []StrategyConfig{
		{ID: "hl-zebra-btc", Type: "perps", Args: []string{"zebra", "BTC", "1h"}, Capital: 1000},
		{ID: "hl-adx-btc", Type: "perps", Args: []string{"adx", "BTC", "1h"}, Capital: 1000},
	}
	state := &AppState{
		Strategies: map[string]*StrategyState{
			"hl-zebra-btc": {Cash: 1000},
			"hl-adx-btc":   {Cash: 1000},
		},
	}
	prices := map[string]float64{"BTC/USDT": 50000}
	msgs := FormatCategorySummary(1, 0, 1, 0, 2000, prices, nil, strats, state, "hyperliquid", "BTC", 600, 0, nil)
	msg := strings.Join(msgs, "\n")
	idxAdx := strings.Index(msg, "hl-adx-btc")
	idxZebra := strings.Index(msg, "hl-zebra-btc")
	if idxAdx < 0 || idxZebra < 0 {
		t.Fatalf("expected both strategy IDs in output:\n%s", msg)
	}
	if idxAdx >= idxZebra {
		t.Errorf("expected hl-adx-btc before hl-zebra-btc (sorted by ID), got adx@%d zebra@%d", idxAdx, idxZebra)
	}
}

func TestFormatCategorySummary_NoCircuitBreaker(t *testing.T) {
	strats := []StrategyConfig{
		{ID: "hl-rsi-btc", Type: "perps", Args: []string{"rsi", "BTC", "1h"}, Capital: 1000},
	}
	state := &AppState{
		Strategies: map[string]*StrategyState{
			"hl-rsi-btc": {Cash: 1000},
		},
	}
	prices := map[string]float64{"BTC/USDT": 50000}

	msgs := FormatCategorySummary(1, 0, 1, 0, 1000, prices, nil, strats, state, "hyperliquid", "BTC", 600, 0, nil)
	msg := strings.Join(msgs, "\n")

	if strings.Contains(msg, "Circuit breaker") {
		t.Errorf("should not show circuit breaker when none active, got:\n%s", msg)
	}
	if !strings.Contains(msg, "Trading active") {
		t.Errorf("expected 'Trading active' status when no circuit breaker, got:\n%s", msg)
	}
}

func TestDiscordChannels_BackwardsCompatJSON(t *testing.T) {
	// Old config format {"spot":"x","options":"y"} should still parse into map[string]string.
	raw := `{"enabled":true,"token":"","channels":{"spot":"ch1","options":"ch2"}}`
	var dc DiscordConfig
	if err := json.Unmarshal([]byte(raw), &dc); err != nil {
		t.Fatalf("unmarshal failed: %v", err)
	}
	if dc.Channels["spot"] != "ch1" {
		t.Errorf("expected ch1, got %s", dc.Channels["spot"])
	}
	if dc.Channels["options"] != "ch2" {
		t.Errorf("expected ch2, got %s", dc.Channels["options"])
	}
	// New key works too
	raw2 := `{"enabled":true,"token":"","channels":{"spot":"ch1","options":"ch2","hyperliquid":"ch3"}}`
	var dc2 DiscordConfig
	if err := json.Unmarshal([]byte(raw2), &dc2); err != nil {
		t.Fatalf("unmarshal failed: %v", err)
	}
	if dc2.Channels["hyperliquid"] != "ch3" {
		t.Errorf("expected ch3, got %s", dc2.Channels["hyperliquid"])
	}
}

func TestFormatTradeDM_OpenTrade(t *testing.T) {
	sc := StrategyConfig{ID: "hl-sma-btc", Platform: "hyperliquid", Type: "perps"}
	trade := Trade{
		Symbol:   "BTC",
		Side:     "buy",
		Quantity: 0.15,
		Price:    67845.00,
		Value:    10176.75,
		Details:  "Open long 0.150000 @ $67845.00 (fee $10.18)",
	}
	msg := FormatTradeDM(sc, trade, "paper")

	if !strings.Contains(msg, "TRADE EXECUTED") {
		t.Errorf("expected 'TRADE EXECUTED', got:\n%s", msg)
	}
	if !strings.Contains(msg, "hl-sma-btc") {
		t.Errorf("expected strategy ID, got:\n%s", msg)
	}
	if !strings.Contains(msg, "LONG") {
		t.Errorf("expected LONG, got:\n%s", msg)
	}
	if !strings.Contains(msg, "TRADE EXECUTED - PAPER") {
		t.Errorf("expected 'TRADE EXECUTED - PAPER' in header, got:\n%s", msg)
	}
	if strings.Contains(msg, "PnL") {
		t.Errorf("open trade should not contain PnL, got:\n%s", msg)
	}
}

func TestFormatTradeDM_CloseTrade(t *testing.T) {
	sc := StrategyConfig{ID: "hl-rmc-eth", Platform: "hyperliquid", Type: "perps"}
	trade := Trade{
		Symbol:   "ETH",
		Side:     "sell",
		Quantity: 0.47,
		Price:    3077.70,
		Value:    1446.52,
		Details:  "Close long, PnL: $34.35 (fee $1.23)",
	}
	msg := FormatTradeDM(sc, trade, "live")

	if !strings.Contains(msg, "TRADE CLOSED") {
		t.Errorf("expected 'TRADE CLOSED', got:\n%s", msg)
	}
	// Regression for #386: close alert must report the *position* side, not
	// the execution side. Selling to close a long must render LONG, not SHORT.
	if !strings.Contains(msg, "LONG") {
		t.Errorf("expected LONG (position side), got:\n%s", msg)
	}
	if strings.Contains(msg, "SHORT") {
		t.Errorf("close-long trade must not render SHORT, got:\n%s", msg)
	}
	if !strings.Contains(msg, "PnL: $34.35") {
		t.Errorf("expected PnL in close trade, got:\n%s", msg)
	}
	if !strings.Contains(msg, "TRADE CLOSED - LIVE") {
		t.Errorf("expected 'TRADE CLOSED - LIVE' in header, got:\n%s", msg)
	}
}

// Issue #530: partial closes use lowercase "close" (e.g. "Partial-close long …").
func TestFormatTradeDM_PartialClose(t *testing.T) {
	sc := StrategyConfig{ID: "hl-sma-eth", Platform: "hyperliquid", Type: "perps"}
	trade := Trade{
		Symbol:   "ETH",
		Side:     "sell",
		Quantity: 0.1,
		Price:    2800,
		Value:    280,
		Details:  "Partial-close long ETH, PnL: $12.34 (fee $0.05)",
	}
	msg := FormatTradeDM(sc, trade, "live")
	if !strings.Contains(msg, "TRADE CLOSED") {
		t.Errorf("expected 'TRADE CLOSED' for partial close, got:\n%s", msg)
	}
	if !strings.Contains(msg, "PnL: $12.34") {
		t.Errorf("expected PnL line for partial close, got:\n%s", msg)
	}
	if !strings.Contains(msg, "LONG") {
		t.Errorf("expected LONG position side, got:\n%s", msg)
	}
}

func TestFormatTradeDM_CloseShort(t *testing.T) {
	sc := StrategyConfig{ID: "hl-bidir-eth", Platform: "hyperliquid", Type: "perps"}
	trade := Trade{
		Symbol:   "ETH",
		Side:     "buy",
		Quantity: 0.47,
		Price:    3077.70,
		Value:    1446.52,
		Details:  "Close short, PnL: $12.50 (fee $1.23)",
	}
	msg := FormatTradeDM(sc, trade, "live")
	if !strings.Contains(msg, "SHORT") {
		t.Errorf("expected SHORT (position side), got:\n%s", msg)
	}
	if strings.Contains(msg, "LONG") {
		t.Errorf("close-short trade must not render LONG, got:\n%s", msg)
	}
}

func TestFormatTradeDM_FuturesTrade(t *testing.T) {
	sc := StrategyConfig{ID: "ts-es-scalp", Platform: "topstep", Type: "futures"}
	trade := Trade{
		Symbol:   "ES",
		Side:     "buy",
		Quantity: 2,
		Price:    5342.50,
		Value:    534250.00,
		Details:  "Open long 2 contracts @ $5342.50 (fee $4.12)",
	}
	msg := FormatTradeDM(sc, trade, "paper")

	if !strings.Contains(msg, "Topstep futures") {
		t.Errorf("expected 'Topstep futures', got:\n%s", msg)
	}
	if !strings.Contains(msg, "ES") {
		t.Errorf("expected ES symbol, got:\n%s", msg)
	}
}

func TestFormatTradeDM_OptionsPnLFormat(t *testing.T) {
	sc := StrategyConfig{ID: "deribit-wheel-btc", Platform: "deribit", Type: "options"}
	trade := Trade{
		Symbol:   "BTC",
		Side:     "sell",
		Quantity: 1,
		Price:    500,
		Value:    500,
		Details:  "Close BTC-call-50000-2026-01-17 PnL=$123.45",
	}
	msg := FormatTradeDM(sc, trade, "paper")

	if !strings.Contains(msg, "PnL: $123.45") {
		t.Errorf("expected PnL extracted from options format (PnL=$), got:\n%s", msg)
	}
}

func TestExtractPnL(t *testing.T) {
	cases := []struct {
		details string
		want    string
		ok      bool
	}{
		{"Close long, PnL: $34.35 (fee $1.23)", "34.35", true},
		{"Close BTC-call-50000 PnL=$123.45", "123.45", true},
		{"Theta harvest close BTC-put PnL=$-50.00", "-50.00", true},
		{"Open long 0.15 @ $67845.00 (fee $10.18)", "", false},
	}
	for _, c := range cases {
		got, ok := extractPnL(c.details)
		if ok != c.ok || got != c.want {
			t.Errorf("extractPnL(%q) = (%q, %v), want (%q, %v)", c.details, got, ok, c.want, c.ok)
		}
	}
}

func TestFormatTradeDM_EmptyPlatform(t *testing.T) {
	sc := StrategyConfig{ID: "test", Platform: "", Type: "spot"}
	trade := Trade{Symbol: "BTC", Side: "buy", Quantity: 1, Price: 100, Value: 100, Details: "Open long"}
	// Should not panic
	msg := FormatTradeDM(sc, trade, "paper")
	if !strings.Contains(msg, "TRADE EXECUTED") {
		t.Errorf("expected message, got:\n%s", msg)
	}
}

func TestTradeSideToDirection(t *testing.T) {
	cases := []struct{ side, want string }{
		{"buy", "LONG"},
		{"BUY", "LONG"},
		{"sell", "SHORT"},
		{"SELL", "SHORT"},
		{"other", "OTHER"},
	}
	for _, c := range cases {
		got := tradeSideToDirection(c.side)
		if got != c.want {
			t.Errorf("tradeSideToDirection(%q) = %q, want %q", c.side, got, c.want)
		}
	}
}

func TestTradeDirectionLabel(t *testing.T) {
	cases := []struct {
		name    string
		side    string
		details string
		want    string
	}{
		{"close_long_from_sell", "sell", "Close long, PnL: $34.35 (fee $1.23)", "LONG"},
		{"close_short_from_buy", "buy", "Close short, PnL: $12.50 (fee $1.23)", "SHORT"},
		{"open_long", "buy", "Open long 0.15 @ $67845.00 (fee $10.18)", "LONG"},
		{"open_short", "sell", "Open short 0.15 @ $67845.00 (fee $10.18)", "SHORT"},
		{"futures_close_long", "sell", "Close long 2 contracts, PnL: $50.00 (fee $4.12)", "LONG"},
		{"futures_close_short", "buy", "Close short 2 contracts, PnL: $50.00 (fee $4.12)", "SHORT"},
		{"circuit_breaker_fallback", "close", "Circuit breaker force-close, PnL: $-12.00", "CLOSE"},
		{"options_close_falls_back_to_side", "sell", "Close BTC-call-50000 PnL=$123.45", "SHORT"},
		{"empty_details_falls_back", "buy", "", "LONG"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := tradeDirectionLabel(Trade{Side: c.side, Details: c.details})
			if got != c.want {
				t.Errorf("tradeDirectionLabel(side=%q, details=%q) = %q, want %q", c.side, c.details, got, c.want)
			}
		})
	}
}

func TestFormatInterval(t *testing.T) {
	cases := []struct {
		seconds int
		want    string
	}{
		{60, "1m"},
		{300, "5m"},
		{600, "10m"},
		{900, "15m"},
		{1800, "30m"},
		{3600, "1h"},
		{7200, "2h"},
		{14400, "4h"},
		{21600, "6h"},
		{43200, "12h"},
		{86400, "1d"},
		{172800, "2d"},
		{90, "90s"}, // not divisible by 60 → falls through to seconds
		{45, "45s"}, // non-round seconds
		{0, "—"},
		{-1, "—"},
	}
	for _, c := range cases {
		got := formatInterval(c.seconds)
		if got != c.want {
			t.Errorf("formatInterval(%d) = %q, want %q", c.seconds, got, c.want)
		}
	}
}

func TestExtractTimeframe(t *testing.T) {
	cases := []struct {
		sc   StrategyConfig
		want string
	}{
		// Perps: args[2] is timeframe
		{StrategyConfig{Type: "perps", Args: []string{"rsi", "BTC", "1h"}}, "1h"},
		{StrategyConfig{Type: "perps", Args: []string{"sma", "ETH", "4h"}}, "4h"},
		// Futures: args[2] is timeframe
		{StrategyConfig{Type: "futures", Args: []string{"sma", "ES", "15m"}}, "15m"},
		// OKX spot with timeframe
		{StrategyConfig{Type: "spot", Args: []string{"sma", "BTC", "1h"}}, "1h"},
		// Spot via check_strategy.py: only 2 args → no timeframe
		{StrategyConfig{Type: "spot", Args: []string{"sma_crossover", "BTC/USDT"}}, "—"},
		// Options: args[2] starts with "--"
		{StrategyConfig{Type: "options", Args: []string{"wheel", "ETH", "--platform=deribit"}}, "—"},
		// Only 1 arg
		{StrategyConfig{Type: "perps", Args: []string{"rsi"}}, "—"},
		// Empty args
		{StrategyConfig{Type: "spot", Args: []string{}}, "—"},
	}
	for _, c := range cases {
		got := extractTimeframe(c.sc)
		if got != c.want {
			t.Errorf("extractTimeframe(%v, %v) = %q, want %q", c.sc.Type, c.sc.Args, got, c.want)
		}
	}
}

func TestFormatCategorySummary_TfIntColumn(t *testing.T) {
	// Perps strategy with timeframe "1h" and per-strategy interval 600s.
	strats := []StrategyConfig{
		{ID: "hl-rsi-btc", Type: "perps", Args: []string{"rsi", "BTC", "1h"}, Capital: 1000, IntervalSeconds: 600},
	}
	state := &AppState{
		Strategies: map[string]*StrategyState{
			"hl-rsi-btc": {Cash: 1000},
		},
	}
	prices := map[string]float64{"BTC/USDT": 50000}

	msgs := FormatCategorySummary(1, 0, 1, 0, 1000, prices, nil, strats, state, "hyperliquid", "BTC", 3600, 0, nil)
	msg := strings.Join(msgs, "\n")

	// Separate Tf and Int column headers should be present (at end of table).
	if !strings.Contains(msg, "Tf") || !strings.Contains(msg, "Int") {
		t.Errorf("expected 'Tf' and 'Int' column headers, got:\n%s", msg)
	}
	// Row should render timeframe "1h" and interval "10m" as separate values.
	if !strings.Contains(msg, "1h") || !strings.Contains(msg, "10m") {
		t.Errorf("expected '1h' and '10m' for perps with 1h timeframe and 600s interval, got:\n%s", msg)
	}
}

func TestFormatCategorySummary_TfIntGlobalFallback(t *testing.T) {
	// Spot strategy — no timeframe in args, falls back to global interval.
	strats := []StrategyConfig{
		{ID: "sma-btc", Type: "spot", Args: []string{"sma_crossover", "BTC/USDT"}, Capital: 1000},
	}
	state := &AppState{
		Strategies: map[string]*StrategyState{
			"sma-btc": {Cash: 1000},
		},
	}
	prices := map[string]float64{"BTC/USDT": 50000}

	msgs := FormatCategorySummary(1, 0, 1, 0, 1000, prices, nil, strats, state, "spot", "", 3600, 0, nil)
	msg := strings.Join(msgs, "\n")

	// No timeframe for spot → "—"; global interval 3600s → "1h". Separate columns now.
	if !strings.Contains(msg, "—") || !strings.Contains(msg, "1h") {
		t.Errorf("expected '—' and '1h' for spot with global 3600s interval, got:\n%s", msg)
	}
}

func TestFormatCategorySummary_StrategyLabelWidthAndTieredAliases(t *testing.T) {
	strats := []StrategyConfig{
		{ID: "hl-123456789012345", Type: "perps", Args: []string{"rsi", "BTC", "1h"}, Capital: 1000},
		{ID: "hl-tiered-atr-btc", Type: "perps", Args: []string{"tiered_atr", "BTC", "1h"}, Capital: 1000},
		{ID: "hl-tiered-pct-btc", Type: "perps", Args: []string{"tiered_pct", "BTC", "1h"}, Capital: 1000},
	}
	state := &AppState{
		Strategies: map[string]*StrategyState{
			"hl-123456789012345": {Cash: 1000},
			"hl-tiered-atr-btc":  {Cash: 1000},
			"hl-tiered-pct-btc":  {Cash: 1000},
		},
	}
	prices := map[string]float64{"BTC/USDT": 50000}

	msgs := FormatCategorySummary(1, 0, 3, 0, 3000, prices, nil, strats, state, "hyperliquid", "BTC", 600, 0, nil)
	msg := strings.Join(msgs, "\n")

	if !strings.Contains(msg, "hl-123456789012345") {
		t.Errorf("expected 18-char strategy label to render without truncation, got:\n%s", msg)
	}
	if !strings.Contains(msg, "hl-tatr-btc") || strings.Contains(msg, "tiered-atr") {
		t.Errorf("expected tiered-atr summary label alias tatr, got:\n%s", msg)
	}
	if !strings.Contains(msg, "hl-tpct-btc") || strings.Contains(msg, "tiered-pct") {
		t.Errorf("expected tiered-pct summary label alias tpct, got:\n%s", msg)
	}
}

func TestFormatCategorySummary_MaxDrawdownColumn(t *testing.T) {
	// Issue #436: summary tables surface the effective max_drawdown_pct already
	// resolved onto StrategyConfig by LoadConfig (strategy → platform → type).
	strats := []StrategyConfig{
		{ID: "hl-rsi-btc", Type: "perps", Args: []string{"rsi", "BTC", "1h"}, Capital: 1000, MaxDrawdownPct: 12.5},
		{ID: "hl-sma-btc", Type: "perps", Args: []string{"sma", "BTC", "1h"}, Capital: 1000, MaxDrawdownPct: 50},
	}
	state := &AppState{
		Strategies: map[string]*StrategyState{
			"hl-rsi-btc": {Cash: 1000},
			"hl-sma-btc": {Cash: 1000},
		},
	}
	prices := map[string]float64{"BTC/USDT": 50000}

	msgs := FormatCategorySummary(1, 0, 2, 0, 2000, prices, nil, strats, state, "hyperliquid", "BTC", 600, 0, nil)
	msg := strings.Join(msgs, "\n")

	if !strings.Contains(msg, " DD ") {
		t.Errorf("expected DD column header, got:\n%s", msg)
	}
	pnlIdx := strings.Index(msg, "PnL%")
	ddIdx := strings.Index(msg, " DD ")
	tfIdx := strings.Index(msg, "Tf")
	if pnlIdx < 0 || ddIdx < pnlIdx || tfIdx < ddIdx {
		t.Errorf("expected DD column between PnL%% and Tf, got PnL%%@%d DD@%d Tf@%d:\n%s", pnlIdx, ddIdx, tfIdx, msg)
	}
	if !strings.Contains(msg, "12%") || !strings.Contains(msg, "50%") {
		t.Errorf("expected resolved max drawdown values 12%% and 50%%, got:\n%s", msg)
	}
}

func TestFormatCategorySummary_MaxDrawdownColumn_SharedWallet(t *testing.T) {
	// Shared-wallet tables have a Wallet% column, so keep DD anchored before
	// it and keep the TOTAL wallet percentage from shifting.
	strats := []StrategyConfig{
		{ID: "hl-rmc-eth", Type: "perps", Platform: "hyperliquid", Capital: 500, CapitalPct: 0.5, Args: []string{"rmc", "ETH", "1h"}, MaxDrawdownPct: 25},
		{ID: "hl-tema-eth", Type: "perps", Platform: "hyperliquid", Capital: 500, CapitalPct: 0.5, Args: []string{"tema", "ETH", "1h"}, MaxDrawdownPct: 35},
	}
	state := &AppState{
		Strategies: map[string]*StrategyState{
			"hl-rmc-eth":  {Cash: 500, InitialCapital: 500},
			"hl-tema-eth": {Cash: 500, InitialCapital: 500},
		},
	}
	prices := map[string]float64{"ETH/USDT": 3000}

	msgs := FormatCategorySummary(1, 0, 2, 0, 0, prices, nil, strats, state, "hyperliquid", "ETH", 600, 0, nil)
	msg := strings.Join(msgs, "\n")
	lines := strings.Split(msg, "\n")
	var headerLine, totalLine string
	for _, line := range lines {
		if strings.Contains(line, " DD ") && strings.Contains(line, "Wallet%") {
			headerLine = line
		}
		if strings.HasPrefix(line, "TOTAL") {
			totalLine = line
		}
	}
	if headerLine == "" || totalLine == "" {
		t.Fatalf("expected shared-wallet header and TOTAL row, got:\n%s", msg)
	}
	pnlIdx := strings.Index(headerLine, "PnL%")
	ddIdx := strings.Index(headerLine, " DD ")
	walletIdx := strings.Index(headerLine, "Wallet%")
	if pnlIdx < 0 || ddIdx < pnlIdx || walletIdx < ddIdx {
		t.Errorf("expected DD column between PnL%% and Wallet%%, got PnL%%@%d DD@%d Wallet%%@%d:\n%s", pnlIdx, ddIdx, walletIdx, msg)
	}
	if !strings.Contains(msg, "25%") || !strings.Contains(msg, "35%") {
		t.Errorf("expected resolved max drawdown values 25%% and 35%%, got:\n%s", msg)
	}
	if len(totalLine) <= walletIdx || !strings.Contains(totalLine[walletIdx:], "100.0%") {
		t.Errorf("expected TOTAL row to keep 100.0%% under Wallet%% column, got header=%q total=%q", headerLine, totalLine)
	}
}

func TestFormatCategorySummary_ClosedTradesColumn(t *testing.T) {
	// Issue #381: strategy table should show closed-trade count per strategy.
	// Standard variant (no shared wallet).
	strats := []StrategyConfig{
		{ID: "hl-rsi-btc", Type: "perps", Args: []string{"rsi", "BTC", "1h"}, Capital: 1000},
		{ID: "hl-sma-btc", Type: "perps", Args: []string{"sma", "BTC", "1h"}, Capital: 1000},
		{ID: "hl-mom-btc", Type: "perps", Args: []string{"mom", "BTC", "1h"}, Capital: 1000},
	}
	state := &AppState{
		Strategies: map[string]*StrategyState{
			"hl-rsi-btc": {Cash: 1000},
			"hl-sma-btc": {Cash: 1000},
			"hl-mom-btc": {Cash: 1000},
		},
	}
	prices := map[string]float64{"BTC/USDT": 50000}
	lifetime := map[string]LifetimeTradeStats{
		"hl-rsi-btc": {PositionsOpened: 7},
		"hl-sma-btc": {PositionsOpened: 12},
		"hl-mom-btc": {PositionsOpened: 0},
	}

	msgs := FormatCategorySummary(1, 0, 3, 0, 3000, prices, nil, strats, state, "hyperliquid", "BTC", 600, 0, lifetime)
	msg := strings.Join(msgs, "\n")

	// Header should include #T column.
	if !strings.Contains(msg, "#T") {
		t.Errorf("expected '#T' column header, got:\n%s", msg)
	}
	// #T should appear AFTER Int (last column).
	intIdx := strings.LastIndex(msg, "Int")
	tIdx := strings.Index(msg, "#T")
	if intIdx < 0 || tIdx < 0 || tIdx < intIdx {
		t.Errorf("expected #T column to follow Int column, got Int@%d #T@%d:\n%s", intIdx, tIdx, msg)
	}

	// Each strategy row should render its DB-sourced round-trip count
	// right-justified in 5 chars, followed by W/L (issue #434).
	if !strings.Contains(msg, "    7     —\n") {
		t.Errorf("expected closed-trade count '7' for hl-rsi-btc, got:\n%s", msg)
	}
	if !strings.Contains(msg, "   12     —\n") {
		t.Errorf("expected closed-trade count '12' for hl-sma-btc, got:\n%s", msg)
	}
	// Strategy with zero trades should still render '0'.
	if !strings.Contains(msg, "    0     —\n") {
		t.Errorf("expected closed-trade count '0' for hl-mom-btc, got:\n%s", msg)
	}
	// TOTAL row should sum to 19 (7+12+0). W/L on TOTAL is "—" with no wins/losses.
	totalIdx := strings.Index(msg, "TOTAL")
	if totalIdx < 0 {
		t.Fatalf("expected TOTAL row, got:\n%s", msg)
	}
	totalLine := msg[totalIdx:]
	if newline := strings.Index(totalLine, "\n"); newline >= 0 {
		totalLine = totalLine[:newline]
	}
	if !strings.HasSuffix(totalLine, "   19     —") {
		t.Errorf("expected TOTAL row to end with closed-trade sum '19' followed by W/L '—', got TOTAL line: %q", totalLine)
	}
}

func TestFormatCategorySummary_ClosedTradesColumn_SharedWallet(t *testing.T) {
	// Issue #381: shared-wallet variant must also render #T column and TOTAL.
	strats := []StrategyConfig{
		{ID: "hl-rmc-eth", Type: "perps", Platform: "hyperliquid", Capital: 500, CapitalPct: 0.5, Args: []string{"rmc", "ETH", "1h"}},
		{ID: "hl-tema-eth", Type: "perps", Platform: "hyperliquid", Capital: 500, CapitalPct: 0.5, Args: []string{"tema", "ETH", "1h"}},
	}
	state := &AppState{
		Strategies: map[string]*StrategyState{
			"hl-rmc-eth":  {Cash: 500, InitialCapital: 500},
			"hl-tema-eth": {Cash: 500, InitialCapital: 500},
		},
	}
	prices := map[string]float64{"ETH/USDT": 3000}
	lifetime := map[string]LifetimeTradeStats{
		"hl-rmc-eth":  {PositionsOpened: 4},
		"hl-tema-eth": {PositionsOpened: 9},
	}

	msgs := FormatCategorySummary(1, 0, 2, 0, 0, prices, nil, strats, state, "hyperliquid", "ETH", 600, 0, lifetime)
	msg := strings.Join(msgs, "\n")

	if !strings.Contains(msg, "#T") {
		t.Errorf("expected '#T' column header in shared-wallet variant, got:\n%s", msg)
	}
	// #T should appear AFTER Wallet% (the shared-wallet-only column).
	walletIdx := strings.Index(msg, "Wallet%")
	tIdx := strings.Index(msg, "#T")
	if walletIdx < 0 || tIdx < walletIdx {
		t.Errorf("expected #T after Wallet%% in shared-wallet variant, got Wallet%%@%d #T@%d:\n%s", walletIdx, tIdx, msg)
	}
	// Per-strategy counts; W/L column (issue #434) renders "—" with no wins/losses set.
	if !strings.Contains(msg, "    4     —\n") {
		t.Errorf("expected closed-trade count '4' for hl-rmc-eth, got:\n%s", msg)
	}
	if !strings.Contains(msg, "    9     —\n") {
		t.Errorf("expected closed-trade count '9' for hl-tema-eth, got:\n%s", msg)
	}
	// TOTAL row should end with sum 13 followed by W/L "—".
	totalIdx := strings.Index(msg, "TOTAL")
	if totalIdx < 0 {
		t.Fatalf("expected TOTAL row, got:\n%s", msg)
	}
	totalLine := msg[totalIdx:]
	if newline := strings.Index(totalLine, "\n"); newline >= 0 {
		totalLine = totalLine[:newline]
	}
	if !strings.HasSuffix(totalLine, "   13     —") {
		t.Errorf("expected TOTAL row to end with closed-trade sum '13' followed by W/L '—', got TOTAL line: %q", totalLine)
	}
}

func TestFmtWinLossRatio(t *testing.T) {
	cases := []struct {
		name     string
		wins     int
		losses   int
		expected string
	}{
		{"no trades closed", 0, 0, "—"},
		{"all wins, no losses", 3, 0, "∞"},
		{"all losses, no wins", 0, 5, "0.00"},
		{"even split", 4, 4, "1.00"},
		{"more wins than losses", 7, 4, "1.75"},
		{"more losses than wins", 1, 4, "0.25"},
		{"large counts round to 2dp", 100, 33, "3.03"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := fmtWinLossRatio(tc.wins, tc.losses)
			if got != tc.expected {
				t.Errorf("fmtWinLossRatio(%d, %d) = %q, want %q", tc.wins, tc.losses, got, tc.expected)
			}
		})
	}
}

func TestFormatCategorySummary_WinLossColumn(t *testing.T) {
	strats := []StrategyConfig{
		{ID: "hl-rsi-btc", Type: "perps", Args: []string{"rsi", "BTC", "1h"}, Capital: 1000},
		{ID: "hl-sma-btc", Type: "perps", Args: []string{"sma", "BTC", "1h"}, Capital: 1000},
		{ID: "hl-mom-btc", Type: "perps", Args: []string{"mom", "BTC", "1h"}, Capital: 1000},
	}
	state := &AppState{
		Strategies: map[string]*StrategyState{
			"hl-rsi-btc": {Cash: 1000},
			"hl-sma-btc": {Cash: 1000},
			"hl-mom-btc": {Cash: 1000},
		},
	}
	prices := map[string]float64{"BTC/USDT": 50000}
	lifetime := map[string]LifetimeTradeStats{
		"hl-rsi-btc": {PositionsOpened: 10, Wins: 7, Losses: 3},
		"hl-sma-btc": {PositionsOpened: 5, Wins: 5, Losses: 0},
		"hl-mom-btc": {PositionsOpened: 0, Wins: 0, Losses: 0},
	}

	msgs := FormatCategorySummary(1, 0, 3, 0, 3000, prices, nil, strats, state, "hyperliquid", "BTC", 600, 0, lifetime)
	msg := strings.Join(msgs, "\n")

	if !strings.Contains(msg, "W/L") {
		t.Errorf("expected 'W/L' column header, got:\n%s", msg)
	}
	// W/L should follow #T in the header.
	tIdx := strings.Index(msg, "#T")
	wlIdx := strings.Index(msg, "W/L")
	if tIdx < 0 || wlIdx < 0 || wlIdx < tIdx {
		t.Errorf("expected W/L column to follow #T, got #T@%d W/L@%d:\n%s", tIdx, wlIdx, msg)
	}

	// Per-strategy W/L values.
	if !strings.Contains(msg, "  2.33\n") {
		t.Errorf("expected W/L '2.33' for hl-rsi-btc (7/3), got:\n%s", msg)
	}
	if !strings.Contains(msg, "    ∞\n") {
		t.Errorf("expected W/L '∞' for hl-sma-btc (5/0), got:\n%s", msg)
	}
	if !strings.Contains(msg, "    —\n") {
		t.Errorf("expected W/L '—' for hl-mom-btc (no trades), got:\n%s", msg)
	}

	// TOTAL row aggregates wins/losses across strategies: (7+5+0)/(3+0+0) = 4.00.
	totalIdx := strings.Index(msg, "TOTAL")
	if totalIdx < 0 {
		t.Fatalf("expected TOTAL row, got:\n%s", msg)
	}
	totalLine := msg[totalIdx:]
	if newline := strings.Index(totalLine, "\n"); newline >= 0 {
		totalLine = totalLine[:newline]
	}
	if !strings.HasSuffix(totalLine, "  4.00") {
		t.Errorf("expected TOTAL row to end with W/L '4.00' (12 wins / 3 losses), got TOTAL line: %q", totalLine)
	}
}

func TestFormatCategorySummary_SharedWallet(t *testing.T) {
	// Two strategies share a Hyperliquid wallet via capital_pct=0.5 each.
	// Wallet balance = $1085, so each strategy's Capital = $542.50.
	// Each strategy's cash is its proportional share (no double-scaling).
	strats := []StrategyConfig{
		{ID: "hl-rmc-eth", Type: "perps", Platform: "hyperliquid", Capital: 542.50, CapitalPct: 0.5, Args: []string{"rmc", "ETH", "1h"}},
		{ID: "hl-tema-eth", Type: "perps", Platform: "hyperliquid", Capital: 542.50, CapitalPct: 0.5, Args: []string{"tema", "ETH", "1h"}},
	}
	state := &AppState{
		Strategies: map[string]*StrategyState{
			"hl-rmc-eth":  {Cash: 542.50, InitialCapital: 500},
			"hl-tema-eth": {Cash: 542.50, InitialCapital: 500},
		},
	}
	prices := map[string]float64{"ETH/USDT": 3000}

	msgs := FormatCategorySummary(1, 0, 2, 0, 0, prices, nil, strats, state, "hyperliquid", "ETH", 600, 0, nil)
	msg := strings.Join(msgs, "\n")

	// Should contain Wallet% column
	if !strings.Contains(msg, "Wallet%") {
		t.Errorf("expected 'Wallet%%' column header, got:\n%s", msg)
	}
	// Should contain Init column
	if !strings.Contains(msg, "Init") {
		t.Errorf("expected 'Init' column header, got:\n%s", msg)
	}
	// Should contain 50.0% for each strategy
	if !strings.Contains(msg, "50.0%") {
		t.Errorf("expected '50.0%%' wallet share, got:\n%s", msg)
	}
	// Should contain 100.0% in TOTAL row
	if !strings.Contains(msg, "100.0%") {
		t.Errorf("expected '100.0%%' total wallet share, got:\n%s", msg)
	}
	// TOTAL value should be ~$1,085 (sum of both strategy values)
	if !strings.Contains(msg, "1,085") {
		t.Errorf("expected total value ~1,085, got:\n%s", msg)
	}
	// Individual values should be ~$542
	if !strings.Contains(msg, "542") {
		t.Errorf("expected individual value ~542, got:\n%s", msg)
	}
	// PnL should use InitialCapital ($500), not runtime Capital ($542.50)
	if !strings.Contains(msg, "500") {
		t.Errorf("expected initial capital '500', got:\n%s", msg)
	}
	// Column order should be Init | Value (not Value | Init)
	initIdx := strings.Index(msg, "Init")
	valueIdx := strings.Index(msg, "Value")
	if initIdx >= valueIdx {
		t.Errorf("expected Init column before Value column, got:\n%s", msg)
	}
}

func TestFormatCategorySummary_WalletPctFromConfig(t *testing.T) {
	// Wallet% should reflect capital_pct from config, not dynamic share.
	// capital_pct is 0.3 and 0.7, but actual capitals are equal ($500 each).
	// Old behavior: walletPct = 500/1000 * 100 = 50.0% each (wrong).
	// New behavior: walletPct = 0.3*100=30.0% and 0.7*100=70.0% (correct).
	strats := []StrategyConfig{
		{ID: "hl-rmc-eth", Type: "perps", Platform: "hyperliquid", Capital: 500, CapitalPct: 0.3, Args: []string{"rmc", "ETH", "1h"}},
		{ID: "hl-tema-eth", Type: "perps", Platform: "hyperliquid", Capital: 500, CapitalPct: 0.7, Args: []string{"tema", "ETH", "1h"}},
	}
	state := &AppState{
		Strategies: map[string]*StrategyState{
			"hl-rmc-eth":  {Cash: 1000},
			"hl-tema-eth": {Cash: 1000},
		},
	}
	prices := map[string]float64{"ETH/USDT": 3000}

	msgs := FormatCategorySummary(1, 0, 2, 0, 0, prices, nil, strats, state, "hyperliquid", "ETH", 600, 0, nil)
	msg := strings.Join(msgs, "\n")

	if !strings.Contains(msg, "30.0%") {
		t.Errorf("expected '30.0%%' from capital_pct=0.3, got:\n%s", msg)
	}
	if !strings.Contains(msg, "70.0%") {
		t.Errorf("expected '70.0%%' from capital_pct=0.7, got:\n%s", msg)
	}
}

func TestFormatCategorySummary_NoSharedWallet(t *testing.T) {
	// Strategies without capital_pct should not show Wallet% column.
	strats := []StrategyConfig{
		{ID: "hl-rmc-eth", Type: "perps", Platform: "hyperliquid", Capital: 500, Args: []string{"rmc", "ETH", "1h"}},
		{ID: "hl-tema-eth", Type: "perps", Platform: "hyperliquid", Capital: 500, Args: []string{"tema", "ETH", "1h"}},
	}
	state := &AppState{
		Strategies: map[string]*StrategyState{
			"hl-rmc-eth":  {Cash: 500, InitialCapital: 500},
			"hl-tema-eth": {Cash: 600, InitialCapital: 500},
		},
	}
	prices := map[string]float64{"ETH/USDT": 3000}

	msgs := FormatCategorySummary(1, 0, 2, 0, 0, prices, nil, strats, state, "hyperliquid", "ETH", 600, 0, nil)
	msg := strings.Join(msgs, "\n")

	if strings.Contains(msg, "Wallet%") {
		t.Errorf("should not show Wallet%% column without shared wallet, got:\n%s", msg)
	}
	// Should still show Init column even without shared wallet
	if !strings.Contains(msg, "Init") {
		t.Errorf("expected 'Init' column header, got:\n%s", msg)
	}
}

func TestFormatCategorySummary_MessageSplitting(t *testing.T) {
	// Create enough positions to exceed Discord's 2000-char limit.
	strats := make([]StrategyConfig, 20)
	strategies := make(map[string]*StrategyState, 20)
	for i := 0; i < 20; i++ {
		id := fmt.Sprintf("hl-strat%02d-btc", i)
		strats[i] = StrategyConfig{ID: id, Type: "perps", Platform: "hyperliquid", Capital: 500, Args: []string{fmt.Sprintf("strat%02d", i), "BTC", "1h"}}
		strategies[id] = &StrategyState{
			Cash: 450,
			Positions: map[string]*Position{
				"BTC/USDT": {Symbol: "BTC/USDT", Quantity: 0.01, AvgCost: 50000, Side: "long", OpenedAt: time.Date(2026, 3, 15, 10, 30, 0, 0, time.UTC)},
			},
		}
	}
	state := &AppState{Strategies: strategies}
	prices := map[string]float64{"BTC/USDT": 51000}

	msgs := FormatCategorySummary(1, 0, 20, 0, 10000, prices, nil, strats, state, "hyperliquid", "BTC", 600, 0, nil)

	// Should produce multiple messages.
	if len(msgs) < 2 {
		totalLen := 0
		for _, m := range msgs {
			totalLen += len(m)
		}
		t.Errorf("expected multiple messages for 20 positions, got %d (total chars: %d)", len(msgs), totalLen)
	}

	// First message should contain "... and N more".
	if !strings.Contains(msgs[0], "... and") {
		t.Errorf("first message should contain '... and N more' indicator, got:\n%s", msgs[0])
	}

	// First message should not exceed the split threshold.
	if len(msgs[0]) > discordCharLimit {
		t.Errorf("first message exceeds %d chars: %d", discordCharLimit, len(msgs[0]))
	}

	// Second message should contain continuation header.
	if !strings.Contains(msgs[1], "cont'd") {
		t.Errorf("second message should contain continuation header, got:\n%s", msgs[1])
	}

	// All position lines should appear across all messages.
	allMsgs := strings.Join(msgs, "\n")
	if !strings.Contains(allMsgs, "Positions: 20 open") {
		t.Errorf("expected 'Positions: 20 open' header, got:\n%s", allMsgs)
	}
}

func TestFormatCategorySummary_NoSplitWhenShort(t *testing.T) {
	// A small number of positions should produce a single message.
	strats := []StrategyConfig{
		{ID: "hl-rsi-btc", Type: "perps", Platform: "hyperliquid", Capital: 1000, Args: []string{"rsi", "BTC", "1h"}},
	}
	state := &AppState{
		Strategies: map[string]*StrategyState{
			"hl-rsi-btc": {
				Cash: 900,
				Positions: map[string]*Position{
					"BTC/USDT": {Symbol: "BTC/USDT", Quantity: 0.01, AvgCost: 50000, Side: "long"},
				},
			},
		},
	}
	prices := map[string]float64{"BTC/USDT": 51000}

	msgs := FormatCategorySummary(1, 0, 1, 0, 1000, prices, nil, strats, state, "hyperliquid", "BTC", 600, 0, nil)

	if len(msgs) != 1 {
		t.Errorf("expected single message for 1 position, got %d", len(msgs))
	}
	if !strings.Contains(msgs[0], "Positions: 1 open") {
		t.Errorf("expected 'Positions: 1 open', got:\n%s", msgs[0])
	}
}

func TestCollectPositions_WithTimestamp(t *testing.T) {
	opened := time.Date(2026, 3, 15, 10, 30, 0, 0, time.UTC)
	ss := &StrategyState{
		Positions: map[string]*Position{
			"BTC/USDT": {Symbol: "BTC/USDT", Quantity: 0.5, AvgCost: 50000, Side: "long", OpenedAt: opened},
		},
	}
	prices := map[string]float64{"BTC/USDT": 51000}

	lines := collectPositions(StrategyConfig{ID: "hl-rsi-btc"}, ss, prices)
	if len(lines) != 1 {
		t.Fatalf("expected 1 line, got %d", len(lines))
	}
	if !strings.Contains(lines[0], "[Mar 15 10:30]") {
		t.Errorf("expected timestamp '[Mar 15 10:30]', got: %s", lines[0])
	}
	if !strings.Contains(lines[0], "LONG") {
		t.Errorf("expected 'LONG' direction label, got: %s", lines[0])
	}
}

func TestCollectPositions_WithoutTimestamp(t *testing.T) {
	// Legacy positions without OpenedAt should not show a date.
	ss := &StrategyState{
		Positions: map[string]*Position{
			"BTC/USDT": {Symbol: "BTC/USDT", Quantity: 0.5, AvgCost: 50000, Side: "long"},
		},
	}
	prices := map[string]float64{"BTC/USDT": 51000}

	lines := collectPositions(StrategyConfig{ID: "hl-rsi-btc"}, ss, prices)
	if len(lines) != 1 {
		t.Fatalf("expected 1 line, got %d", len(lines))
	}
	if strings.Contains(lines[0], "[") {
		t.Errorf("legacy position without OpenedAt should not show date, got: %s", lines[0])
	}
}

func TestCollectPositions_OptionTimestamp(t *testing.T) {
	opened := time.Date(2026, 4, 1, 8, 0, 0, 0, time.UTC)
	ss := &StrategyState{
		OptionPositions: map[string]*OptionPosition{
			"BTC-call-50000": {ID: "BTC-call-50000", CurrentValueUSD: 500, OpenedAt: opened},
		},
	}
	prices := map[string]float64{}

	lines := collectPositions(StrategyConfig{ID: "deribit-wheel-btc"}, ss, prices)
	if len(lines) != 1 {
		t.Fatalf("expected 1 line, got %d", len(lines))
	}
	if !strings.Contains(lines[0], "[Apr 01 08:00]") {
		t.Errorf("expected option timestamp '[Apr 01 08:00]', got: %s", lines[0])
	}
}

// TestCollectPositions_OptionValueFormat verifies option position lines format
// CurrentValueUSD with thousands separators and two decimal places (matching the
// spot/perps line format), so small values like $12.34 render precisely.
func TestCollectPositions_OptionValueFormat(t *testing.T) {
	ss := &StrategyState{
		OptionPositions: map[string]*OptionPosition{
			"BTC-call-50000": {ID: "BTC-call-50000", CurrentValueUSD: 12345.67},
		},
	}
	lines := collectPositions(StrategyConfig{ID: "deribit-wheel-btc"}, ss, nil)
	if len(lines) != 1 {
		t.Fatalf("expected 1 line, got %d", len(lines))
	}
	if !strings.Contains(lines[0], "($12,345.67)") {
		t.Errorf("expected option value '($12,345.67)' in line, got: %s", lines[0])
	}
}

// TestCollectPositions_EntryPrice verifies issue #259: position lines include
// the entry price (`@ $AvgCost`) alongside PnL so users can compare entry vs
// current price at a glance.
func TestCollectPositions_EntryPrice(t *testing.T) {
	ss := &StrategyState{
		Positions: map[string]*Position{
			"ETH/USDT": {Symbol: "ETH/USDT", Quantity: 1.5, AvgCost: 2213.08, Side: "long"},
		},
	}
	prices := map[string]float64{"ETH/USDT": 2214.88}

	lines := collectPositions(StrategyConfig{ID: "hl-rsi-eth"}, ss, prices)
	if len(lines) != 1 {
		t.Fatalf("expected 1 line, got %d", len(lines))
	}
	// Entry price: 2213.08 with comma/decimal formatting.
	if !strings.Contains(lines[0], "@ $2,213.08") {
		t.Errorf("expected entry price '@ $2,213.08' in line, got: %s", lines[0])
	}
	// PnL: 1.5 * (2214.88 - 2213.08) = 2.70
	if !strings.Contains(lines[0], "(+$2.70)") {
		t.Errorf("expected PnL '(+$2.70)' in line, got: %s", lines[0])
	}
	if !strings.Contains(lines[0], "LONG") {
		t.Errorf("expected 'LONG' direction label, got: %s", lines[0])
	}
}

// TestCollectPositions_ShortEntryPrice verifies entry price + PnL rendering for
// short positions (PnL flips sign).
func TestCollectPositions_ShortEntryPrice(t *testing.T) {
	ss := &StrategyState{
		Positions: map[string]*Position{
			"BTC/USDT": {Symbol: "BTC/USDT", Quantity: 0.1, AvgCost: 50000, Side: "short"},
		},
	}
	prices := map[string]float64{"BTC/USDT": 51000}

	lines := collectPositions(StrategyConfig{ID: "hl-rsi-btc"}, ss, prices)
	if len(lines) != 1 {
		t.Fatalf("expected 1 line, got %d", len(lines))
	}
	// Entry price formatted with comma.
	if !strings.Contains(lines[0], "@ $50,000.00") {
		t.Errorf("expected entry price '@ $50,000.00' in line, got: %s", lines[0])
	}
	// Short at 50k, price up to 51k → loss of 0.1 * 1000 = 100.
	if !strings.Contains(lines[0], "(-$100.00)") {
		t.Errorf("expected PnL '(-$100.00)' in line, got: %s", lines[0])
	}
	if !strings.Contains(lines[0], "SHORT") {
		t.Errorf("expected 'SHORT' direction label, got: %s", lines[0])
	}
}

// TestCollectPositions_StopLossLong verifies SL price + percent rendering for
// a long position. SL below entry → negative percent.
func TestCollectPositions_StopLossLong(t *testing.T) {
	ss := &StrategyState{
		Positions: map[string]*Position{
			"BTC/USDT": {Symbol: "BTC/USDT", Quantity: 0.025, AvgCost: 63500, Side: "long", StopLossOID: 12345, StopLossTriggerPx: 61595},
		},
	}
	prices := map[string]float64{"BTC/USDT": 63500}

	lines := collectPositions(StrategyConfig{ID: "hl-btc-sma"}, ss, prices)
	if len(lines) != 1 {
		t.Fatalf("expected 1 line, got %d", len(lines))
	}
	// (61595 - 63500) / 63500 = -0.03 → -3.0%
	if !strings.Contains(lines[0], "| SL: $61,595.00 (-3.0%)") {
		t.Errorf("expected SL fragment 'SL: $61,595.00 (-3.0%%)', got: %s", lines[0])
	}
}

// TestCollectPositions_StopLossShort verifies SL percent stays negative for a
// short whose stop sits above entry (loss if hit).
func TestCollectPositions_StopLossShort(t *testing.T) {
	ss := &StrategyState{
		Positions: map[string]*Position{
			"BTC/USDT": {Symbol: "BTC/USDT", Quantity: 0.025, AvgCost: 63500, Side: "short", StopLossOID: 99, StopLossTriggerPx: 65405},
		},
	}
	prices := map[string]float64{"BTC/USDT": 63500}

	lines := collectPositions(StrategyConfig{ID: "hl-btc-sma"}, ss, prices)
	// (65405 - 63500) / 63500 = +3.0%, then sign-flipped for short → -3.0%
	if !strings.Contains(lines[0], "| SL: $65,405.00 (-3.0%)") {
		t.Errorf("expected SL fragment 'SL: $65,405.00 (-3.0%%)' for short, got: %s", lines[0])
	}
}

// TestCollectPositions_StopLossTriggerPxWithoutOID verifies #528: SL price is
// shown whenever StopLossTriggerPx is known, even without a resting HL OID.
func TestCollectPositions_StopLossTriggerPxWithoutOID(t *testing.T) {
	ss := &StrategyState{
		Positions: map[string]*Position{
			"BTC/USDT": {Symbol: "BTC/USDT", Quantity: 0.025, AvgCost: 63500, Side: "long", StopLossOID: 0, StopLossTriggerPx: 61595},
		},
	}
	lines := collectPositions(StrategyConfig{ID: "hl-btc-sma"}, ss, map[string]float64{"BTC/USDT": 63500})
	if !strings.Contains(lines[0], "| SL: $61,595.00 (-3.0%)") {
		t.Errorf("expected SL from trigger price when OID=0, got: %s", lines[0])
	}
}

// TestCollectPositions_StopLossOmittedWhenNoTriggerPx verifies the SL fragment
// is omitted when StopLossTriggerPx is unset (0).
func TestCollectPositions_StopLossOmittedWhenNoTriggerPx(t *testing.T) {
	ss := &StrategyState{
		Positions: map[string]*Position{
			"BTC/USDT": {Symbol: "BTC/USDT", Quantity: 0.025, AvgCost: 63500, Side: "long", StopLossOID: 12345, StopLossTriggerPx: 0},
		},
	}
	lines := collectPositions(StrategyConfig{ID: "hl-btc-sma"}, ss, map[string]float64{"BTC/USDT": 63500})
	if strings.Contains(lines[0], "SL:") {
		t.Errorf("SL fragment should be omitted when StopLossTriggerPx=0, got: %s", lines[0])
	}
}

// TestCollectPositions_StopLossATRMultiplier verifies the SL line shows the
// stamped ATR multiplier from pos.StopLossATRMult (the config value resolved
// at trade-open), not a back-computed value derived from the rounded on-chain
// trigger price (#687).
func TestCollectPositions_StopLossATRMultiplier(t *testing.T) {
	pf := func(v float64) *float64 { return &v }
	cases := []struct {
		name    string
		side    string
		avg     float64
		sl      float64
		atr     float64
		mult    *float64
		wantSub string
	}{
		{name: "long_1x", side: "long", avg: 10000, sl: 9000, atr: 1000, mult: pf(1.0), wantSub: "| SL: $9,000.00 (-10.0%) (1x)"},
		{name: "long_1.5x", side: "long", avg: 10000, sl: 9700, atr: 200, mult: pf(1.5), wantSub: "| SL: $9,700.00 (-3.0%) (1.5x)"},
		// #687: rounded trigger px would back-compute to ~1.489x; stamped 1.5 wins.
		{name: "long_1.5x_rounded_trigger", side: "long", avg: 2335.10, sl: 2323.30, atr: 7.92, mult: pf(1.5), wantSub: "| SL: $2,323.30 (-0.5%) (1.5x)"},
		{name: "long_1.25x", side: "long", avg: 10000, sl: 9750, atr: 200, mult: pf(1.25), wantSub: "| SL: $9,750.00 (-2.5%) (1.25x)"},
		{name: "short_1x", side: "short", avg: 10000, sl: 11000, atr: 1000, mult: pf(1.0), wantSub: "| SL: $11,000.00 (-10.0%) (1x)"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ss := &StrategyState{
				Positions: map[string]*Position{
					"BTC/USDT": {Symbol: "BTC/USDT", Quantity: 0.01, AvgCost: tc.avg, Side: tc.side, StopLossTriggerPx: tc.sl, EntryATR: tc.atr, StopLossATRMult: tc.mult},
				},
			}
			lines := collectPositions(StrategyConfig{ID: "hl-btc-sma"}, ss, map[string]float64{"BTC/USDT": tc.avg})
			if !strings.Contains(lines[0], tc.wantSub) {
				t.Errorf("expected SL fragment %q, got: %s", tc.wantSub, lines[0])
			}
		})
	}
}

// TestCollectPositions_StopLossNoATRMultiplierWhenMultNil verifies the SL line
// omits the (Nx) suffix when StopLossATRMult is nil — i.e. SL was armed via
// stop_loss_pct / stop_loss_margin_pct / trailing_stop_pct rather than ATR
// (#687). Pre-#669 positions opened before snapshot stamping also fall here.
func TestCollectPositions_StopLossNoATRMultiplierWhenMultNil(t *testing.T) {
	ss := &StrategyState{
		Positions: map[string]*Position{
			"BTC/USDT": {Symbol: "BTC/USDT", Quantity: 0.01, AvgCost: 10000, Side: "long", StopLossTriggerPx: 9500, EntryATR: 250, StopLossATRMult: nil},
		},
	}
	lines := collectPositions(StrategyConfig{ID: "hl-btc-sma"}, ss, map[string]float64{"BTC/USDT": 10000})
	if !strings.Contains(lines[0], "| SL: $9,500.00 (-5.0%)") {
		t.Errorf("expected SL fragment without multiplier, got: %s", lines[0])
	}
	if strings.Contains(lines[0], "x)") {
		t.Errorf("expected no multiplier suffix when StopLossATRMult=nil, got: %s", lines[0])
	}
}

// TestCollectPositions_LeverageMargin verifies leverage + margin rendering for
// perps with Leverage > 1.
func TestCollectPositions_LeverageMargin(t *testing.T) {
	ss := &StrategyState{
		Positions: map[string]*Position{
			"BTC/USDT": {Symbol: "BTC/USDT", Quantity: 0.025, AvgCost: 63500, Side: "long", Leverage: 5},
		},
	}
	lines := collectPositions(StrategyConfig{ID: "hl-btc-sma"}, ss, map[string]float64{"BTC/USDT": 63500})
	// margin = 0.025 * 63500 / 5 = 317.5 → rounded to 318.
	if !strings.Contains(lines[0], "| 5x ($318 margin)") {
		t.Errorf("expected '5x ($318 margin)' fragment, got: %s", lines[0])
	}
}

// TestCollectPositions_LeverageOmittedForSpot verifies that the leverage+margin
// fragment is omitted when Leverage is 0 (spot) or 1 (1x perps — margin equals
// notional, so the fragment is noise).
func TestCollectPositions_LeverageOmittedForSpot(t *testing.T) {
	ss := &StrategyState{
		Positions: map[string]*Position{
			"BTC/USDT": {Symbol: "BTC/USDT", Quantity: 0.025, AvgCost: 63500, Side: "long", Leverage: 0},
			"ETH/USDT": {Symbol: "ETH/USDT", Quantity: 1, AvgCost: 2200, Side: "long", Leverage: 1},
		},
	}
	lines := collectPositions(StrategyConfig{ID: "hl-spot"}, ss, map[string]float64{"BTC/USDT": 63500, "ETH/USDT": 2200})
	for _, l := range lines {
		if strings.Contains(l, "margin") {
			t.Errorf("leverage+margin fragment should be omitted for spot/1x, got: %s", l)
		}
	}
}

// TestCollectPositions_AllFragments verifies SL and leverage+margin land
// together on the same line in the documented order: PnL | SL | leverage |
// date.
func TestCollectPositions_AllFragments(t *testing.T) {
	opened := time.Date(2026, 4, 28, 14, 32, 0, 0, time.UTC)
	ss := &StrategyState{
		Positions: map[string]*Position{
			"BTC/USDT": {
				Symbol: "BTC/USDT", Quantity: 0.025, AvgCost: 63500, Side: "long",
				Leverage: 5, StopLossOID: 7, StopLossTriggerPx: 61595, OpenedAt: opened,
			},
		},
	}
	lines := collectPositions(StrategyConfig{ID: "hl-btc-sma"}, ss, map[string]float64{"BTC/USDT": 63500})
	got := lines[0]
	slIdx := strings.Index(got, "| SL:")
	levIdx := strings.Index(got, "| 5x")
	dateIdx := strings.Index(got, "[Apr 28")
	if slIdx < 0 || levIdx < 0 || dateIdx < 0 {
		t.Fatalf("expected SL, leverage, and date fragments all present, got: %s", got)
	}
	if !(slIdx < levIdx && levIdx < dateIdx) {
		t.Errorf("expected SL → leverage → date ordering, got: %s", got)
	}
}

// TestCollectPositions_TieredTPATR_Long verifies #528 TP1/TP2 hints from entry
// ATR when close_strategies includes tiered_tp_atr.
func TestCollectPositions_TieredTPATR_Long(t *testing.T) {
	sc := StrategyConfig{
		ID:              "hl-tatr-btc",
		CloseStrategies: []StrategyRef{{Name: "tiered_tp_atr"}},
	}
	ss := &StrategyState{
		Positions: map[string]*Position{
			"BTC/USDT": {Symbol: "BTC/USDT", Quantity: 0.025, AvgCost: 63500, Side: "long", EntryATR: 1000},
		},
	}
	lines := collectPositions(sc, ss, map[string]float64{"BTC/USDT": 63500})
	if len(lines) != 1 {
		t.Fatalf("expected 1 line, got %d", len(lines))
	}
	if !strings.Contains(lines[0], "| ATR: $1,000.00") {
		t.Errorf("expected ATR fragment, got: %s", lines[0])
	}
	if !strings.Contains(lines[0], "| TP1: $64,500.00 (+1.6%) | TP2: $65,500.00 (+3.1%)") {
		t.Errorf("expected tiered TP fragments for long, got: %s", lines[0])
	}
}

func TestCollectPositions_TieredTPATR_Short(t *testing.T) {
	sc := StrategyConfig{
		ID:              "hl-tatr-btc",
		CloseStrategies: []StrategyRef{{Name: "tiered_tp_atr"}},
	}
	ss := &StrategyState{
		Positions: map[string]*Position{
			"BTC/USDT": {Symbol: "BTC/USDT", Quantity: 0.025, AvgCost: 63500, Side: "short", EntryATR: 1000},
		},
	}
	lines := collectPositions(sc, ss, map[string]float64{"BTC/USDT": 63500})
	if !strings.Contains(lines[0], "| ATR: $1,000.00") {
		t.Errorf("expected ATR fragment, got: %s", lines[0])
	}
	if !strings.Contains(lines[0], "| TP1: $62,500.00 (+1.6%) | TP2: $61,500.00 (+3.1%)") {
		t.Errorf("expected tiered TP fragments for short, got: %s", lines[0])
	}
}

// TestCollectPositions_TieredTPATRLive_Long verifies TP1/TP2 hints also appear
// for tiered_tp_atr_live (same default tiers as tiered_tp_atr; PR #529 review).
func TestCollectPositions_TieredTPATRLive_Long(t *testing.T) {
	sc := StrategyConfig{
		ID:              "hl-tatr-live-btc",
		CloseStrategies: []StrategyRef{{Name: "tiered_tp_atr_live"}},
	}
	ss := &StrategyState{
		Positions: map[string]*Position{
			"BTC/USDT": {Symbol: "BTC/USDT", Quantity: 0.025, AvgCost: 63500, Side: "long", EntryATR: 1000},
		},
	}
	lines := collectPositions(sc, ss, map[string]float64{"BTC/USDT": 63500})
	if len(lines) != 1 {
		t.Fatalf("expected 1 line, got %d", len(lines))
	}
	if !strings.Contains(lines[0], "| ATR: $1,000.00") {
		t.Errorf("expected ATR fragment, got: %s", lines[0])
	}
	want := "| TP1: $64,500.00 (+1.6%) | TP2: $65,500.00 (+3.1%)"
	if !strings.Contains(lines[0], want) {
		t.Errorf("expected tiered TP fragments for tiered_tp_atr_live long, got: %s", lines[0])
	}
}

func TestCollectPositions_TieredTPATR_OmittedWithoutCloseStrategy(t *testing.T) {
	sc := StrategyConfig{ID: "hl-rsi-btc", CloseStrategies: []StrategyRef{{Name: "tiered_tp_pct"}}}
	ss := &StrategyState{
		Positions: map[string]*Position{
			"BTC/USDT": {Symbol: "BTC/USDT", Quantity: 0.025, AvgCost: 63500, Side: "long", EntryATR: 1000},
		},
	}
	lines := collectPositions(sc, ss, map[string]float64{"BTC/USDT": 63500})
	if strings.Contains(lines[0], "TP1:") {
		t.Errorf("TP hints should be omitted without tiered_tp_atr close, got: %s", lines[0])
	}
}

func TestCollectPositions_TieredTPATR_OmittedWhenEntryATRZero(t *testing.T) {
	sc := StrategyConfig{ID: "hl-tatr-btc", CloseStrategies: []StrategyRef{{Name: "tiered_tp_atr"}}}
	ss := &StrategyState{
		Positions: map[string]*Position{
			"BTC/USDT": {Symbol: "BTC/USDT", Quantity: 0.025, AvgCost: 63500, Side: "long", EntryATR: 0},
		},
	}
	lines := collectPositions(sc, ss, map[string]float64{"BTC/USDT": 63500})
	if strings.Contains(lines[0], "TP1:") {
		t.Errorf("TP hints should be omitted when EntryATR=0, got: %s", lines[0])
	}
}

// TestCollectPositions_TieredTPATR_FilledTierMarked verifies #662: once a TP
// tier has filled (TPOID zeroed AND position quantity dropped below
// InitialQuantity), the summary marks that tier with ✓ instead of rendering it
// as still pending. Pending tiers retain the price-and-percent format.
func TestCollectPositions_TieredTPATR_FilledTierMarked(t *testing.T) {
	sc := StrategyConfig{
		ID:              "hl-tatr-btc",
		CloseStrategies: []StrategyRef{{Name: "tiered_tp_atr"}},
	}
	ss := &StrategyState{
		Positions: map[string]*Position{
			"BTC/USDT": {
				Symbol:          "BTC/USDT",
				Quantity:        0.0125,
				InitialQuantity: 0.025,
				AvgCost:         63500,
				Side:            "long",
				EntryATR:        1000,
				TPOIDs:          []int64{0, 99999},
			},
		},
	}
	lines := collectPositions(sc, ss, map[string]float64{"BTC/USDT": 63500})
	if !strings.Contains(lines[0], "| TP1: $64,500.00 ✓") {
		t.Errorf("expected TP1 marked filled, got: %s", lines[0])
	}
	if !strings.Contains(lines[0], "| TP2: $65,500.00 (+3.1%)") {
		t.Errorf("expected TP2 still pending, got: %s", lines[0])
	}
}

// TestCollectPositions_TieredTPATR_NoFillBeforeProtectionSync verifies #662
// edge case: TPOIDs all-zero before the first protection-sync places tiers
// must NOT be rendered as filled (no shrink vs InitialQuantity).
func TestCollectPositions_TieredTPATR_NoFillBeforeProtectionSync(t *testing.T) {
	sc := StrategyConfig{
		ID:              "hl-tatr-btc",
		CloseStrategies: []StrategyRef{{Name: "tiered_tp_atr"}},
	}
	ss := &StrategyState{
		Positions: map[string]*Position{
			"BTC/USDT": {
				Symbol:          "BTC/USDT",
				Quantity:        0.025,
				InitialQuantity: 0.025,
				AvgCost:         63500,
				Side:            "long",
				EntryATR:        1000,
				TPOIDs:          []int64{0, 0},
			},
		},
	}
	lines := collectPositions(sc, ss, map[string]float64{"BTC/USDT": 63500})
	if strings.Contains(lines[0], "✓") {
		t.Errorf("filled marker leaked before any TP fill, got: %s", lines[0])
	}
	if !strings.Contains(lines[0], "| TP1: $64,500.00 (+1.6%) | TP2: $65,500.00 (+3.1%)") {
		t.Errorf("expected both tiers pending, got: %s", lines[0])
	}
}

// TestCollectPositions_AllFragments_WithTieredTP verifies ATR → SL → TP1 → TP2 →
// leverage → date ordering when tiered_tp_atr is configured (#528).
func TestCollectPositions_AllFragments_WithTieredTP(t *testing.T) {
	opened := time.Date(2026, 4, 28, 14, 32, 0, 0, time.UTC)
	sc := StrategyConfig{ID: "hl-tatr-btc", CloseStrategies: []StrategyRef{{Name: "tiered_tp_atr"}}}
	ss := &StrategyState{
		Positions: map[string]*Position{
			"BTC/USDT": {
				Symbol: "BTC/USDT", Quantity: 0.025, AvgCost: 63500, Side: "long", EntryATR: 1000,
				Leverage: 5, StopLossTriggerPx: 61595, OpenedAt: opened,
			},
		},
	}
	got := collectPositions(sc, ss, map[string]float64{"BTC/USDT": 63500})[0]
	slIdx := strings.Index(got, "| SL:")
	atrIdx := strings.Index(got, "| ATR:")
	tp1Idx := strings.Index(got, "| TP1:")
	tp2Idx := strings.Index(got, "| TP2:")
	levIdx := strings.Index(got, "| 5x")
	dateIdx := strings.Index(got, "[Apr 28")
	if slIdx < 0 || atrIdx < 0 || tp1Idx < 0 || tp2Idx < 0 || levIdx < 0 || dateIdx < 0 {
		t.Fatalf("expected SL, ATR, TP1, TP2, leverage, and date fragments, got: %s", got)
	}
	if !(atrIdx < slIdx && slIdx < tp1Idx && tp1Idx < tp2Idx && tp2Idx < levIdx && levIdx < dateIdx) {
		t.Errorf("expected ATR → SL → TP1 → TP2 → leverage → date ordering, got: %s", got)
	}
}

// TestPercentFromEntry covers sign-flip behavior for shorts.
func TestPercentFromEntry(t *testing.T) {
	cases := []struct {
		side   string
		entry  float64
		target float64
		want   float64
	}{
		{"long", 100, 97, -3},
		{"long", 100, 103, 3},
		{"short", 100, 103, -3}, // SL above entry → loss for short
		{"short", 100, 97, 3},   // TP below entry → gain for short
		{"long", 0, 100, 0},     // guard: zero entry
	}
	for _, c := range cases {
		got := percentFromEntry(c.side, c.entry, c.target)
		if math.Abs(got-c.want) > 1e-9 {
			t.Errorf("percentFromEntry(%s, %g, %g) = %g, want %g", c.side, c.entry, c.target, got, c.want)
		}
	}
}

// TestPositionMargin verifies notional/leverage math and the leverage<=0 guard.
func TestPositionMargin(t *testing.T) {
	if got := positionMargin(0.025, 63500, 5); math.Abs(got-317.5) > 1e-9 {
		t.Errorf("positionMargin(0.025, 63500, 5) = %g, want 317.5", got)
	}
	if got := positionMargin(1, 100, 0); got != 0 {
		t.Errorf("positionMargin with leverage=0 should be 0, got %g", got)
	}
}

// TestSplitCategorySummary_LongPositionLines verifies splitCategorySummary still
// splits cleanly when each position line carries the new SL + leverage
// fragments (regression for the per-message length budget).
func TestSplitCategorySummary_LongPositionLines(t *testing.T) {
	header := "Cycle 1 | Mode: paper"
	var posLines []string
	for i := 0; i < 50; i++ {
		posLines = append(posLines, fmt.Sprintf("hl-strat-%02d LONG BTC/USDT x0.025 @ $63,500.00 (+$45.20) | SL: $61,595.00 (-3.0%%) | 5x ($318 margin) [Apr 28 14:32]", i))
	}
	msgs := splitCategorySummary(header, len(posLines), posLines, nil, nil)
	if len(msgs) == 0 {
		t.Fatal("expected at least one message")
	}
	for i, m := range msgs {
		if len(m) > 2000 {
			t.Errorf("message %d exceeds 2000-char Discord limit (%d chars)", i, len(m))
		}
	}
}

// TestFormatCategorySummary_HeaderPriceFormat verifies issue #259: the header
// prices line uses `SYMBOL: $X,XXX.XX` format — colon separator, thousands
// comma, two decimal places.
func TestFormatCategorySummary_HeaderPriceFormat(t *testing.T) {
	strats := []StrategyConfig{
		{ID: "hl-rsi-eth", Type: "perps", Args: []string{"rsi", "ETH", "1h"}, Capital: 1000},
	}
	state := &AppState{
		Strategies: map[string]*StrategyState{
			"hl-rsi-eth": {Cash: 1000},
		},
	}
	prices := map[string]float64{"ETH/USDT": 2240.5}

	msgs := FormatCategorySummary(1, 0, 1, 0, 1000, prices, nil, strats, state, "hyperliquid", "ETH", 600, 0, nil)
	msg := strings.Join(msgs, "\n")
	if !strings.Contains(msg, "ETH: $2,240.50") {
		t.Errorf("expected header price 'ETH: $2,240.50', got:\n%s", msg)
	}
	// Old format `ETH $2240` (no colon) must be gone.
	if strings.Contains(msg, "ETH $2240") {
		t.Errorf("old header format 'ETH $2240' should be removed, got:\n%s", msg)
	}
}

func TestFmtComma2(t *testing.T) {
	cases := []struct {
		in   float64
		want string
	}{
		{0, "0.00"},
		{1.5, "1.50"},
		{123.456, "123.46"},
		{1234.5, "1,234.50"},
		{1234567.89, "1,234,567.89"},
		{-2213.08, "-2,213.08"},
		{2240.5, "2,240.50"},
		{-12345.67, "-12,345.67"},
		{-1234567.89, "-1,234,567.89"},
	}
	for _, c := range cases {
		if got := fmtComma2(c.in); got != c.want {
			t.Errorf("fmtComma2(%v) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestSplitCategorySummary_SingleMessage(t *testing.T) {
	header := "Header line\n"
	posLines := []string{"pos1", "pos2"}
	tradeLines := []string{"• trade1"}

	msgs := splitCategorySummary(header, 2, posLines, tradeLines, nil)
	if len(msgs) != 1 {
		t.Fatalf("expected 1 message, got %d", len(msgs))
	}
	if !strings.Contains(msgs[0], "pos1") || !strings.Contains(msgs[0], "pos2") {
		t.Errorf("single message should contain all positions, got:\n%s", msgs[0])
	}
	if !strings.Contains(msgs[0], "trade1") {
		t.Errorf("single message should contain trades, got:\n%s", msgs[0])
	}
}

func TestFormatCategorySummary_LargeTableChunked(t *testing.T) {
	// Reproduces #249: 28 perps strategies on a single asset produces a table
	// that, prior to the fix, exceeded Discord's 2000-char limit and was silently
	// truncated mid-code-block. The fix caps the table at catTableMaxRows rows
	// per message and emits the rest as continuation messages, each wrapped in
	// its own code block. (#381 reduced the cap from 20 to 15 after the row
	// gained a #T column.)
	const stratCount = 28
	strats := make([]StrategyConfig, stratCount)
	strategies := make(map[string]*StrategyState, stratCount)
	for i := 0; i < stratCount; i++ {
		id := fmt.Sprintf("hl-strat%02d-btc", i)
		strats[i] = StrategyConfig{ID: id, Type: "perps", Platform: "hyperliquid", Capital: 500, Args: []string{fmt.Sprintf("strat%02d", i), "BTC", "1h"}}
		strategies[id] = &StrategyState{Cash: 500}
	}
	state := &AppState{Strategies: strategies}
	prices := map[string]float64{"BTC/USDT": 51000}

	msgs := FormatCategorySummary(1, 0, stratCount, 0, 14000, prices, nil, strats, state, "hyperliquid", "BTC", 600, 0, nil)

	if len(msgs) < 2 {
		t.Fatalf("expected at least 2 messages for %d strategies, got %d", stratCount, len(msgs))
	}
	for i, m := range msgs {
		if len(m) > discordCharLimit {
			t.Errorf("msg[%d] exceeds Discord limit: %d chars", i, len(m))
		}
		// Every message containing table content must have a closed code block.
		if strings.Count(m, "```")%2 != 0 {
			t.Errorf("msg[%d] has unbalanced code-block fences:\n%s", i, m)
		}
	}

	// First message holds rows 1–catTableMaxRows; the totals row stays with the LAST table chunk.
	firstChunkLast := fmt.Sprintf("hl-strat%02d-b", catTableMaxRows-1)
	contChunkFirst := fmt.Sprintf("hl-strat%02d-b", catTableMaxRows)
	if !strings.Contains(msgs[0], "hl-strat00-b") {
		t.Errorf("first message should contain first strategy row, got:\n%s", msgs[0])
	}
	if !strings.Contains(msgs[0], firstChunkLast) {
		t.Errorf("first message should contain row %d (%s), got:\n%s", catTableMaxRows, firstChunkLast, msgs[0])
	}
	if strings.Contains(msgs[0], "TOTAL") {
		t.Errorf("first message should NOT contain TOTAL row when table is split, got:\n%s", msgs[0])
	}

	// Second message is the table continuation: own code block + label + remaining rows + TOTAL.
	if !strings.Contains(msgs[1], "cont'd") {
		t.Errorf("second message should be the continuation table label, got:\n%s", msgs[1])
	}
	if !strings.Contains(msgs[1], "```") {
		t.Errorf("continuation table must be wrapped in a code block, got:\n%s", msgs[1])
	}
	if !strings.Contains(msgs[1], contChunkFirst) {
		t.Errorf("continuation should contain row %d (%s), got:\n%s", catTableMaxRows+1, contChunkFirst, msgs[1])
	}
	// Final row must appear in one of the continuation messages.
	finalRow := fmt.Sprintf("hl-strat%02d-b", stratCount-1)
	finalSeen := false
	totalSeen := false
	for _, m := range msgs[1:] {
		if strings.Contains(m, finalRow) {
			finalSeen = true
		}
		if strings.Contains(m, "TOTAL") {
			totalSeen = true
		}
	}
	if !finalSeen {
		t.Errorf("continuation should contain final row %s, got:\n%s", finalRow, strings.Join(msgs[1:], "\n---\n"))
	}
	if !totalSeen {
		t.Errorf("final continuation chunk must contain the TOTAL row, got:\n%s", strings.Join(msgs[1:], "\n---\n"))
	}

	// All 28 strategy rows should appear across the messages.
	all := strings.Join(msgs, "\n")
	for i := 0; i < stratCount; i++ {
		want := fmt.Sprintf("hl-strat%02d-b", i)
		if !strings.Contains(all, want) {
			t.Errorf("strategy row %s missing from messages", want)
		}
	}
}

func TestSplitCategorySummary_ContinuationTablesInserted(t *testing.T) {
	// Continuation tables should be spliced in immediately after the first message.
	header := "Header line\n"
	posLines := []string{"pos1", "pos2"}
	conts := []string{"```\nchunk2\n```\n", "```\nchunk3\n```\n"}

	msgs := splitCategorySummary(header, 2, posLines, nil, conts)
	if len(msgs) != 3 {
		t.Fatalf("expected 3 messages (1 main + 2 continuation tables), got %d", len(msgs))
	}
	if !strings.Contains(msgs[0], "Header line") {
		t.Errorf("msg[0] should contain header, got: %s", msgs[0])
	}
	if msgs[1] != conts[0] {
		t.Errorf("msg[1] should be first continuation table, got: %s", msgs[1])
	}
	if msgs[2] != conts[1] {
		t.Errorf("msg[2] should be second continuation table, got: %s", msgs[2])
	}
}

func TestFormatTradeDMPlain_OpenTrade(t *testing.T) {
	sc := StrategyConfig{ID: "hl-sma-btc", Platform: "hyperliquid", Type: "perps"}
	trade := Trade{
		Symbol:   "BTC",
		Side:     "buy",
		Quantity: 0.15,
		Price:    67845.00,
		Value:    10176.75,
		Details:  "Open long 0.150000 @ $67845.00 (fee $10.18)",
	}
	msg := FormatTradeDMPlain(sc, trade, "paper")

	if !strings.Contains(msg, "TRADE EXECUTED") {
		t.Errorf("expected 'TRADE EXECUTED', got:\n%s", msg)
	}
	if !strings.Contains(msg, "hl-sma-btc") {
		t.Errorf("expected strategy ID, got:\n%s", msg)
	}
	if !strings.Contains(msg, "LONG") {
		t.Errorf("expected LONG, got:\n%s", msg)
	}
	if !strings.Contains(msg, "TRADE EXECUTED - PAPER") {
		t.Errorf("expected 'TRADE EXECUTED - PAPER' in header, got:\n%s", msg)
	}
	if strings.Contains(msg, "PnL") {
		t.Errorf("open trade should not contain PnL, got:\n%s", msg)
	}
	// Plain format: no Discord bold markdown (**).
	if strings.Contains(msg, "**") {
		t.Errorf("plain format should not contain Discord markdown '**', got:\n%s", msg)
	}
}

func TestFormatTradeDMPlain_CloseTrade(t *testing.T) {
	sc := StrategyConfig{ID: "hl-rmc-eth", Platform: "hyperliquid", Type: "perps"}
	trade := Trade{
		Symbol:   "ETH",
		Side:     "sell",
		Quantity: 0.47,
		Price:    3077.70,
		Value:    1446.52,
		Details:  "Close long, PnL: $34.35 (fee $1.23)",
	}
	msg := FormatTradeDMPlain(sc, trade, "live")

	if !strings.Contains(msg, "TRADE CLOSED") {
		t.Errorf("expected 'TRADE CLOSED', got:\n%s", msg)
	}
	// Regression for #386: close alert must report the *position* side.
	if !strings.Contains(msg, "LONG") {
		t.Errorf("expected LONG (position side), got:\n%s", msg)
	}
	if strings.Contains(msg, "SHORT") {
		t.Errorf("close-long trade must not render SHORT, got:\n%s", msg)
	}
	if !strings.Contains(msg, "PnL: $34.35") {
		t.Errorf("expected PnL in close trade, got:\n%s", msg)
	}
	if !strings.Contains(msg, "TRADE CLOSED - LIVE") {
		t.Errorf("expected 'TRADE CLOSED - LIVE' in header, got:\n%s", msg)
	}
	// Plain format: no Discord bold markdown (**).
	if strings.Contains(msg, "**") {
		t.Errorf("plain format should not contain Discord markdown '**', got:\n%s", msg)
	}
}

// TestFormatTradeDMPlain_OpenWithCustomTiers mirrors the Discord test for #659:
// telegram plain DM must read tier multiples from config, not hardcode 1×/2×.
func TestFormatTradeDMPlain_OpenWithCustomTiers(t *testing.T) {
	sc := StrategyConfig{
		ID:       "hl-tema-eth-live",
		Platform: "hyperliquid",
		Type:     "perps",
		CloseStrategies: []StrategyRef{{
			Name: "tiered_tp_atr_live",
			Params: map[string]interface{}{
				"tiers": []interface{}{
					map[string]interface{}{"atr_multiple": 2.0, "close_fraction": 0.5},
					map[string]interface{}{"atr_multiple": 3.0, "close_fraction": 1.0},
				},
			},
		}},
	}
	trade := Trade{
		Symbol: "ETH", Side: "buy", Quantity: 0.1, Price: 2316.90, Value: 231.69,
		EntryATR: 12.01,
		Details:  "Open long 0.100000 @ $2316.90",
	}
	msg := FormatTradeDMPlain(sc, trade, "live")
	if !strings.Contains(msg, "TP1: $2,340.92") {
		t.Errorf("expected TP1=2,340.92 (2× ATR) in plain DM, got:\n%s", msg)
	}
	if !strings.Contains(msg, "TP2: $2,352.93") {
		t.Errorf("expected TP2=2,352.93 (3× ATR) in plain DM, got:\n%s", msg)
	}
}

// Issue #530: telegram plain DM must treat partial-close like full close.
func TestFormatTradeDMPlain_PartialClose(t *testing.T) {
	sc := StrategyConfig{ID: "hl-sma-eth", Platform: "hyperliquid", Type: "perps"}
	trade := Trade{
		Symbol:   "ETH",
		Side:     "sell",
		Quantity: 0.1,
		Price:    2800,
		Value:    280,
		Details:  "Partial-close long ETH, PnL: $12.34 (fee $0.05)",
	}
	msg := FormatTradeDMPlain(sc, trade, "live")
	if !strings.Contains(msg, "TRADE CLOSED") {
		t.Errorf("expected 'TRADE CLOSED' for partial close, got:\n%s", msg)
	}
	if !strings.Contains(msg, "PnL: $12.34") {
		t.Errorf("expected PnL line for partial close, got:\n%s", msg)
	}
}

func TestSplitCategorySummary_MultiMessage(t *testing.T) {
	// Create a header that uses ~1900 chars, leaving very little room for positions.
	header := strings.Repeat("x", 1900) + "\n"
	posLines := []string{"position-line-1-aaaa", "position-line-2-bbbb", "position-line-3-cccc"}

	msgs := splitCategorySummary(header, 3, posLines, nil, nil)
	if len(msgs) < 2 {
		t.Fatalf("expected multiple messages with large header, got %d", len(msgs))
	}
	// First message should have "... and N more"
	if !strings.Contains(msgs[0], "... and") {
		t.Errorf("expected '... and N more' in first message, got:\n%s", msgs[0][:100])
	}
	// All positions should appear across messages
	all := strings.Join(msgs, "\n")
	for _, pl := range posLines {
		if !strings.Contains(all, pl) {
			t.Errorf("position %q missing from messages", pl)
		}
	}
}

// TestFormatCategorySummary_LifetimeStatsOverride verifies that
// FormatCategorySummary renders lifetime stats from the trades table (#455).
func TestFormatCategorySummary_LifetimeStatsOverride(t *testing.T) {
	prices := map[string]float64{"ETH/USDT": 2000.0}
	strats := []StrategyConfig{
		{ID: "hl-rmc-eth-live", Type: "perps", Platform: "hyperliquid", Args: []string{"rmc", "ETH/USDT", "1h"}},
	}
	state := &AppState{
		Strategies: map[string]*StrategyState{
			"hl-rmc-eth-live": {
				Cash: 1000,
			},
		},
	}
	lifetime := map[string]LifetimeTradeStats{
		"hl-rmc-eth-live": {PositionsOpened: 17, Wins: 10, Losses: 7},
	}
	msgs := FormatCategorySummary(1, 0, 1, 0, 1000, prices, nil, strats, state, "hyperliquid", "ETH", 600, 0, lifetime)
	if len(msgs) == 0 {
		t.Fatal("expected at least one message")
	}
	msg := msgs[0]
	// Lifetime #T should appear (17).
	if !strings.Contains(msg, " 17 ") {
		t.Errorf("expected lifetime #T=17 in summary, got:\n%s", msg)
	}
	// W/L renders as wins/losses ratio (10/7 ≈ 1.43).
	if !strings.Contains(msg, "1.43") {
		t.Errorf("expected lifetime W/L ratio (10/7=1.43) in summary, got:\n%s", msg)
	}
}

// TestFormatCategorySummary_LifetimeStatsNoFallback verifies that missing DB
// lifetime stats render as zero instead of using stale in-memory counters.
func TestFormatCategorySummary_LifetimeStatsNoFallback(t *testing.T) {
	prices := map[string]float64{"ETH/USDT": 2000.0}
	strats := []StrategyConfig{
		{ID: "hl-rmc-eth-live", Type: "perps", Platform: "hyperliquid", Args: []string{"rmc", "ETH/USDT", "1h"}},
	}
	state := &AppState{
		Strategies: map[string]*StrategyState{
			"hl-rmc-eth-live": {
				Cash: 1000,
			},
		},
	}
	// Nil map, such as a DB query failure, renders zero lifetime stats.
	msgs := FormatCategorySummary(1, 0, 1, 0, 1000, prices, nil, strats, state, "hyperliquid", "ETH", 600, 0, nil)
	if !strings.Contains(msgs[0], " 0     —") {
		t.Errorf("expected zero #T/W-L without lifetime stats, got:\n%s", msgs[0])
	}
	// Empty map (DB returned no rows for this strategy) also renders zero.
	msgs2 := FormatCategorySummary(1, 0, 1, 0, 1000, prices, nil, strats, state, "hyperliquid", "ETH", 600, 0, map[string]LifetimeTradeStats{})
	if !strings.Contains(msgs2[0], " 0     —") {
		t.Errorf("expected zero #T/W-L from empty lifetime stats map, got:\n%s", msgs2[0])
	}
}

// TestFormatTradeDM_OpenWithATRAndTP verifies that when a strategy uses
// tiered_tp_atr close and the trade has EntryATR set, the DM includes ATR,
// TP1, and TP2 on the extras line (#561).
func TestFormatTradeDM_OpenWithATRAndTP(t *testing.T) {
	sc := StrategyConfig{
		ID:              "hl-tatr-btc",
		Platform:        "hyperliquid",
		Type:            "perps",
		CloseStrategies: []StrategyRef{{Name: "tiered_tp_atr"}},
	}
	trade := Trade{
		Symbol:   "BTC",
		Side:     "buy",
		Quantity: 0.01,
		Price:    63500.0,
		Value:    635.0,
		EntryATR: 1000.0,
		Details:  "Open long 0.010000 @ $63500.00 (fee $0.22)",
	}
	msg := FormatTradeDM(sc, trade, "live")
	if !strings.Contains(msg, "ATR: $1,000.00") {
		t.Errorf("expected 'ATR: $1,000.00' in DM, got:\n%s", msg)
	}
	if !strings.Contains(msg, "TP1: $64,500.00") {
		t.Errorf("expected 'TP1: $64,500.00' in DM, got:\n%s", msg)
	}
	if !strings.Contains(msg, "TP2: $65,500.00") {
		t.Errorf("expected 'TP2: $65,500.00' in DM, got:\n%s", msg)
	}
}

// TestFormatTradeDM_OpenWithCustomTiers verifies that the trade DM reads tier
// multiples from sc.CloseStrategies[].Params["tiers"] rather than hardcoded
// 1×/2× (#659). Reproduces the original issue where 2×/3× config showed 1×/2×.
func TestFormatTradeDM_OpenWithCustomTiers(t *testing.T) {
	sc := StrategyConfig{
		ID:       "hl-tema-eth-live",
		Platform: "hyperliquid",
		Type:     "perps",
		CloseStrategies: []StrategyRef{{
			Name: "tiered_tp_atr_live",
			Params: map[string]interface{}{
				"tiers": []interface{}{
					map[string]interface{}{"atr_multiple": 2.0, "close_fraction": 0.5},
					map[string]interface{}{"atr_multiple": 3.0, "close_fraction": 1.0},
				},
			},
		}},
	}
	trade := Trade{
		Symbol:   "ETH",
		Side:     "buy",
		Quantity: 0.1,
		Price:    2316.90,
		Value:    231.69,
		EntryATR: 12.01,
		Details:  "Open long 0.100000 @ $2316.90",
	}
	msg := FormatTradeDM(sc, trade, "live")
	// 2316.90 + 2*12.01 = 2340.92, 2316.90 + 3*12.01 = 2352.93
	if !strings.Contains(msg, "TP1: $2,340.92") {
		t.Errorf("expected 'TP1: $2,340.92' (2× ATR) in DM, got:\n%s", msg)
	}
	if !strings.Contains(msg, "TP2: $2,352.93") {
		t.Errorf("expected 'TP2: $2,352.93' (3× ATR) in DM, got:\n%s", msg)
	}
}

// TestFormatTradeDM_OpenWithThreeTiers verifies the DM renders TP1/TP2/TP3
// when three tiers are configured (#659 — N-tier display).
func TestFormatTradeDM_OpenWithThreeTiers(t *testing.T) {
	sc := StrategyConfig{
		ID:       "hl-tatr-btc",
		Platform: "hyperliquid",
		Type:     "perps",
		CloseStrategies: []StrategyRef{{
			Name: "tiered_tp_atr",
			Params: map[string]interface{}{
				"tiers": []interface{}{
					map[string]interface{}{"atr_multiple": 1.0, "close_fraction": 0.3},
					map[string]interface{}{"atr_multiple": 2.0, "close_fraction": 0.6},
					map[string]interface{}{"atr_multiple": 3.0, "close_fraction": 1.0},
				},
			},
		}},
	}
	trade := Trade{
		Symbol: "BTC", Side: "buy", Quantity: 0.01, Price: 63500.0, Value: 635.0,
		EntryATR: 1000.0,
		Details:  "Open long 0.010000 @ $63500.00",
	}
	msg := FormatTradeDM(sc, trade, "live")
	for _, want := range []string{"TP1: $64,500.00", "TP2: $65,500.00", "TP3: $66,500.00"} {
		if !strings.Contains(msg, want) {
			t.Errorf("expected %q in DM, got:\n%s", want, msg)
		}
	}
}

// TestCollectPositions_TieredTPATR_CustomTiers verifies position-extras read
// tier multiples from config (#659).
func TestCollectPositions_TieredTPATR_CustomTiers(t *testing.T) {
	sc := StrategyConfig{
		ID: "hl-tema-eth-live",
		CloseStrategies: []StrategyRef{{
			Name: "tiered_tp_atr_live",
			Params: map[string]interface{}{
				"tiers": []interface{}{
					map[string]interface{}{"atr_multiple": 2.0, "close_fraction": 0.5},
					map[string]interface{}{"atr_multiple": 3.0, "close_fraction": 1.0},
				},
			},
		}},
	}
	ss := &StrategyState{
		Positions: map[string]*Position{
			"ETH/USDT": {Symbol: "ETH/USDT", Quantity: 0.1, AvgCost: 2316.90, Side: "long", EntryATR: 12.01},
		},
	}
	lines := collectPositions(sc, ss, map[string]float64{"ETH/USDT": 2316.90})
	if len(lines) != 1 {
		t.Fatalf("expected 1 line, got %d", len(lines))
	}
	if !strings.Contains(lines[0], "TP1: $2,340.92") {
		t.Errorf("expected TP1=2,340.92 (2× ATR) in line, got: %s", lines[0])
	}
	if !strings.Contains(lines[0], "TP2: $2,352.93") {
		t.Errorf("expected TP2=2,352.93 (3× ATR) in line, got: %s", lines[0])
	}
}

// TestFormatTradeDM_OpenWithSL verifies that a trade with StopLossTriggerPx
// set shows the SL price and a negative percent on an open trade (#561).
func TestFormatTradeDM_OpenWithSL(t *testing.T) {
	sc := StrategyConfig{ID: "hl-sma-btc", Platform: "hyperliquid", Type: "perps"}
	trade := Trade{
		Symbol:            "BTC",
		Side:              "buy",
		Quantity:          0.01,
		Price:             63500.0,
		Value:             635.0,
		StopLossTriggerPx: 62000.0,
		Details:           "Open long 0.010000 @ $63500.00 (fee $0.22)",
	}
	msg := FormatTradeDM(sc, trade, "live")
	if !strings.Contains(msg, "SL: $62,000.00") {
		t.Errorf("expected 'SL: $62,000.00' in DM, got:\n%s", msg)
	}
	// SL is below entry for a long — should be a negative percent.
	if !strings.Contains(msg, "(-") {
		t.Errorf("expected negative SL percent for long trade, got:\n%s", msg)
	}
}

// TestFormatTradeDM_IncludesOID verifies that when a live-fill Trade has a
// non-empty ExchangeOrderID it is rendered on the symbol/value line as
// `| OID: <id>` (#665).
func TestFormatTradeDM_IncludesOID(t *testing.T) {
	sc := StrategyConfig{ID: "hl-eth-perps", Platform: "hyperliquid", Type: "perps"}
	trade := Trade{
		Symbol:          "ETH",
		Side:            "buy",
		Quantity:        0.432,
		Price:           2306.00,
		Value:           996.0,
		ExchangeOrderID: "418206313303",
		Details:         "Open long 0.432 @ $2306.00",
	}
	msg := FormatTradeDM(sc, trade, "live")
	if !strings.Contains(msg, "| Value: $996 | OID: 418206313303") {
		t.Errorf("expected OID appended to symbol/value line, got:\n%s", msg)
	}
}

// TestFormatTradeDM_PaperOmitsOID verifies that paper trades (empty
// ExchangeOrderID) render no `| OID: …` segment so paper output is unchanged
// from pre-#665 (#665 implementation note).
func TestFormatTradeDM_PaperOmitsOID(t *testing.T) {
	sc := StrategyConfig{ID: "hl-eth-perps", Platform: "hyperliquid", Type: "perps"}
	trade := Trade{
		Symbol:   "ETH",
		Side:     "buy",
		Quantity: 0.432,
		Price:    2306.00,
		Value:    996.0,
		Details:  "Open long 0.432 @ $2306.00",
	}
	msg := FormatTradeDM(sc, trade, "paper")
	if strings.Contains(msg, "OID:") {
		t.Errorf("paper trade should not render OID segment, got:\n%s", msg)
	}
}

// TestFormatTradeDM_SLATRMultiplier verifies the SL line renders the stamped
// trade.StopLossATRMult instead of a back-computed value derived from the
// rounded on-chain trigger price (#687). Pre-#687 this used (avg-sl)/atr.
func TestFormatTradeDM_SLATRMultiplier(t *testing.T) {
	pf := func(v float64) *float64 { return &v }
	sc := StrategyConfig{ID: "hl-sma-btc", Platform: "hyperliquid", Type: "perps"}
	trade := Trade{
		Symbol:            "BTC",
		Side:              "buy",
		Quantity:          0.01,
		Price:             63500.0,
		Value:             635.0,
		EntryATR:          1500.0,
		StopLossTriggerPx: 62000.0,
		StopLossATRMult:   pf(1.0),
		Details:           "Open long 0.010000 @ $63500.00",
	}
	msg := FormatTradeDM(sc, trade, "live")
	if !strings.Contains(msg, "SL: $62,000.00 (-2.4%) (1x)") {
		t.Errorf("expected SL line with stamped ATR multiplier, got:\n%s", msg)
	}
}

// TestFormatTradeDM_SLATRMultiplierFromConfigNotBackComputed is the regression
// guard for #687: the rendered multiplier matches the stamped config value
// (1.5x) even when (avg-sl)/atr back-computes to a slightly different number
// because the on-chain trigger price was rounded by the exchange.
func TestFormatTradeDM_SLATRMultiplierFromConfigNotBackComputed(t *testing.T) {
	pf := func(v float64) *float64 { return &v }
	sc := StrategyConfig{ID: "hl-eth-perps", Platform: "hyperliquid", Type: "perps"}
	trade := Trade{
		Symbol:            "ETH",
		Side:              "buy",
		Quantity:          0.4,
		Price:             2335.10,
		Value:             934.0,
		EntryATR:          7.92,
		StopLossTriggerPx: 2323.30,
		StopLossATRMult:   pf(1.5),
		Details:           "Open long 0.4 @ $2335.10",
	}
	msg := FormatTradeDM(sc, trade, "live")
	if !strings.Contains(msg, "(1.5x)") {
		t.Errorf("expected stamped (1.5x), got:\n%s", msg)
	}
	// Back-computed (2335.10-2323.30)/7.92 = 1.489…x — must not appear.
	if strings.Contains(msg, "1.489") {
		t.Errorf("back-computed multiplier leaked into output:\n%s", msg)
	}
}

// TestFormatTradeDM_SLNoATRMultiplier verifies the SL line drops the `(Nx)`
// suffix when trade.StopLossATRMult is nil (pct/margin/trailing-pct arming, or
// pre-#669 positions without the snapshot) — rendering a fallback would lie
// about how SL was armed (#687).
func TestFormatTradeDM_SLNoATRMultiplier(t *testing.T) {
	sc := StrategyConfig{ID: "hl-sma-btc", Platform: "hyperliquid", Type: "perps"}
	trade := Trade{
		Symbol:            "BTC",
		Side:              "buy",
		Quantity:          0.01,
		Price:             63500.0,
		Value:             635.0,
		EntryATR:          1500.0,
		StopLossTriggerPx: 62000.0,
		StopLossATRMult:   nil,
		Details:           "Open long 0.010000 @ $63500.00",
	}
	msg := FormatTradeDM(sc, trade, "live")
	if !strings.Contains(msg, "SL: $62,000.00 (-2.4%)") {
		t.Errorf("expected legacy SL line, got:\n%s", msg)
	}
	if strings.Contains(msg, "(0x)") || strings.Contains(msg, "(infx)") || strings.Contains(msg, "(1x)") {
		t.Errorf("SL line must not render mult when StopLossATRMult is nil, got:\n%s", msg)
	}
}

// TestFormatTradeDM_TPATRMultipliers verifies that each TP tier renders its
// own ATR multiplier (#665). Default tiers are 1×/2×.
func TestFormatTradeDM_TPATRMultipliers(t *testing.T) {
	sc := StrategyConfig{
		ID:              "hl-tatr-btc",
		Platform:        "hyperliquid",
		Type:            "perps",
		CloseStrategies: []StrategyRef{{Name: "tiered_tp_atr"}},
	}
	trade := Trade{
		Symbol:   "BTC",
		Side:     "buy",
		Quantity: 0.01,
		Price:    63500.0,
		Value:    635.0,
		EntryATR: 1000.0,
		Details:  "Open long 0.010000 @ $63500.00",
	}
	msg := FormatTradeDM(sc, trade, "live")
	if !strings.Contains(msg, "TP1: $64,500.00 (1x)") {
		t.Errorf("expected TP1 with 1× multiplier, got:\n%s", msg)
	}
	if !strings.Contains(msg, "TP2: $65,500.00 (2x)") {
		t.Errorf("expected TP2 with 2× multiplier, got:\n%s", msg)
	}
}

// TestFormatTradeDM_TPATRMultipliersFractional verifies %g preserves
// fractional tier multiples without rounding artifacts (#665 review). %.2g
// would render 1.25→1.3 and 12.5→12.
func TestFormatTradeDM_TPATRMultipliersFractional(t *testing.T) {
	sc := StrategyConfig{
		ID:       "hl-tatr-btc",
		Platform: "hyperliquid",
		Type:     "perps",
		CloseStrategies: []StrategyRef{{
			Name: "tiered_tp_atr",
			Params: map[string]interface{}{
				"tiers": []interface{}{
					map[string]interface{}{"atr_multiple": 1.25, "close_fraction": 0.5},
					map[string]interface{}{"atr_multiple": 2.5, "close_fraction": 1.0},
				},
			},
		}},
	}
	trade := Trade{
		Symbol: "BTC", Side: "buy", Quantity: 0.01, Price: 63500.0, Value: 635.0,
		EntryATR: 1000.0,
		Details:  "Open long 0.010000 @ $63500.00",
	}
	msg := FormatTradeDM(sc, trade, "live")
	if !strings.Contains(msg, "TP1: $64,750.00 (1.25x)") {
		t.Errorf("expected TP1 with fractional 1.25× preserved, got:\n%s", msg)
	}
	if !strings.Contains(msg, "TP2: $66,000.00 (2.5x)") {
		t.Errorf("expected TP2 with fractional 2.5× preserved, got:\n%s", msg)
	}
}

// TestFormatTradeDM_ExtrasOrder verifies the new ordering: ATR → SL → TP1 → TP2
// (#665). SL was previously appended after the TP tiers.
func TestFormatTradeDM_ExtrasOrder(t *testing.T) {
	sc := StrategyConfig{
		ID:              "hl-tatr-btc",
		Platform:        "hyperliquid",
		Type:            "perps",
		CloseStrategies: []StrategyRef{{Name: "tiered_tp_atr"}},
	}
	trade := Trade{
		Symbol:            "BTC",
		Side:              "buy",
		Quantity:          0.01,
		Price:             63500.0,
		Value:             635.0,
		EntryATR:          1000.0,
		StopLossTriggerPx: 62500.0,
		Details:           "Open long 0.010000 @ $63500.00",
	}
	msg := FormatTradeDM(sc, trade, "live")
	atrIdx := strings.Index(msg, "ATR:")
	slIdx := strings.Index(msg, "SL:")
	tp1Idx := strings.Index(msg, "TP1:")
	tp2Idx := strings.Index(msg, "TP2:")
	if atrIdx < 0 || slIdx < 0 || tp1Idx < 0 || tp2Idx < 0 {
		t.Fatalf("missing one of ATR/SL/TP1/TP2, got:\n%s", msg)
	}
	if !(atrIdx < slIdx && slIdx < tp1Idx && tp1Idx < tp2Idx) {
		t.Errorf("expected ATR < SL < TP1 < TP2 ordering, got idx ATR=%d SL=%d TP1=%d TP2=%d:\n%s",
			atrIdx, slIdx, tp1Idx, tp2Idx, msg)
	}
}

// TestFormatTradeDMPlain_IncludesOID is the Telegram parity test for #665 —
// OID, SL ordering, and ATR mults must apply to both Discord and Telegram
// since they share `tradeAlertExtras`.
func TestFormatTradeDMPlain_IncludesOID(t *testing.T) {
	pf := func(v float64) *float64 { return &v }
	sc := StrategyConfig{
		ID:              "hl-tatr-eth",
		Platform:        "hyperliquid",
		Type:            "perps",
		CloseStrategies: []StrategyRef{{Name: "tiered_tp_atr"}},
	}
	trade := Trade{
		Symbol:            "ETH",
		Side:              "buy",
		Quantity:          0.5,
		Price:             2300.0,
		Value:             1150.0,
		EntryATR:          15.0,
		StopLossTriggerPx: 2285.0,
		StopLossATRMult:   pf(1.0),
		ExchangeOrderID:   "987654321",
		Details:           "Open long 0.5 @ $2300.00",
	}
	msg := FormatTradeDMPlain(sc, trade, "live")
	if !strings.Contains(msg, "| OID: 987654321") {
		t.Errorf("expected OID on Telegram DM, got:\n%s", msg)
	}
	if !strings.Contains(msg, "SL: $2,285.00 (-0.7%) (1x)") {
		t.Errorf("expected SL with mult on Telegram DM, got:\n%s", msg)
	}
	if !strings.Contains(msg, "TP1: $2,315.00 (1x)") {
		t.Errorf("expected TP1 with mult on Telegram DM, got:\n%s", msg)
	}
}

// TestFormatTradeDM_CloseNoATR verifies that ATR/TP hints are NOT injected on
// close legs even when EntryATR is set and the strategy uses tiered_tp_atr (#561).
func TestFormatTradeDM_CloseNoATR(t *testing.T) {
	sc := StrategyConfig{
		ID:              "hl-tatr-btc",
		Platform:        "hyperliquid",
		Type:            "perps",
		CloseStrategies: []StrategyRef{{Name: "tiered_tp_atr"}},
	}
	trade := Trade{
		Symbol:   "BTC",
		Side:     "sell",
		Quantity: 0.01,
		Price:    64500.0,
		Value:    645.0,
		EntryATR: 1000.0,
		IsClose:  true,
		Details:  "Close long, PnL: $10.00 (fee $0.23)",
	}
	msg := FormatTradeDM(sc, trade, "live")
	if strings.Contains(msg, "ATR:") {
		t.Errorf("close trade should not include ATR hint, got:\n%s", msg)
	}
	if strings.Contains(msg, "TP1:") {
		t.Errorf("close trade should not include TP1 hint, got:\n%s", msg)
	}
	if strings.Contains(msg, "TP2:") {
		t.Errorf("close trade should not include TP2 hint, got:\n%s", msg)
	}
}

// TestStampOpenTradeFromPosition verifies the backfill helper for EntryATR and
// StopLossTriggerPx on the most-recent open trade for a symbol (#561).
func TestStampOpenTradeFromPosition(t *testing.T) {
	// (a) updates the most recent open trade for symbol.
	s := &StrategyState{ID: "s1", TradeHistory: []Trade{
		{Symbol: "ETH", IsClose: false, EntryATR: 0, StopLossTriggerPx: 0, Timestamp: time.Now().UTC()},
	}}
	pos := &Position{EntryATR: 500.0, StopLossTriggerPx: 61000.0}
	stampOpenTradeFromPosition(s, nil, "ETH", pos)
	if s.TradeHistory[0].EntryATR != 500.0 {
		t.Error("EntryATR not stamped")
	}
	if s.TradeHistory[0].StopLossTriggerPx != 61000.0 {
		t.Error("StopLossTriggerPx not stamped")
	}

	// (b) idempotent: won't overwrite non-zero values.
	stampOpenTradeFromPosition(s, nil, "ETH", &Position{EntryATR: 999.0, StopLossTriggerPx: 99.0})
	if s.TradeHistory[0].EntryATR != 500.0 {
		t.Error("EntryATR overwritten on second call")
	}
	if s.TradeHistory[0].StopLossTriggerPx != 61000.0 {
		t.Error("StopLossTriggerPx overwritten on second call")
	}

	// (c) skips when most recent matching trade is a close.
	s2 := &StrategyState{ID: "s2", TradeHistory: []Trade{
		{Symbol: "ETH", IsClose: false},
		{Symbol: "ETH", IsClose: true}, // most recent is a close
	}}
	stampOpenTradeFromPosition(s2, nil, "ETH", &Position{EntryATR: 500.0})
	if s2.TradeHistory[0].EntryATR != 0 {
		t.Error("should not backfill when most recent trade for symbol is a close")
	}

	// (d) nil pos returns immediately.
	s3 := &StrategyState{ID: "s3", TradeHistory: []Trade{
		{Symbol: "ETH", IsClose: false},
	}}
	stampOpenTradeFromPosition(s3, nil, "ETH", nil)
	if s3.TradeHistory[0].EntryATR != 0 {
		t.Error("nil pos should be a no-op")
	}

	// (e) updates the persisted SQLite row for the already-inserted open trade.
	db, err := OpenStateDB(":memory:")
	if err != nil {
		t.Fatalf("OpenStateDB: %v", err)
	}
	defer db.Close()
	ts := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	s4 := &StrategyState{ID: "s4", TradeHistory: []Trade{
		{Symbol: "ETH", IsClose: false, EntryATR: 0, StopLossTriggerPx: 0, Timestamp: ts},
	}}
	if err := db.InsertTrade(s4.ID, s4.TradeHistory[0]); err != nil {
		t.Fatalf("InsertTrade: %v", err)
	}
	stampOpenTradeFromPosition(s4, db, "ETH", &Position{EntryATR: 250.0, StopLossTriggerPx: 2950.0})

	var entryATR, stopLossTriggerPx float64
	if err := db.db.QueryRow(
		`SELECT entry_atr, stop_loss_trigger_px FROM trades WHERE strategy_id = ? AND timestamp = ?`,
		s4.ID, formatTime(ts),
	).Scan(&entryATR, &stopLossTriggerPx); err != nil {
		t.Fatalf("query stamped trade: %v", err)
	}
	if entryATR != 250.0 || stopLossTriggerPx != 2950.0 {
		t.Fatalf("persisted EntryATR/StopLossTriggerPx = %v/%v, want 250/2950", entryATR, stopLossTriggerPx)
	}
}

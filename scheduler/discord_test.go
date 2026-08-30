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

	if got := resolveChannel(channels, "hyperliquid", "perps"); got != "ch-hl" {
		t.Errorf("expected ch-hl, got %s", got)
	}
	if got := resolveChannel(channels, "binanceus", "spot"); got != "ch-spot" {
		t.Errorf("expected ch-spot, got %s", got)
	}
	if got := resolveChannel(channels, "deribit", "options"); got != "ch-opts" {
		t.Errorf("expected ch-opts for deribit options, got %s", got)
	}
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

	if got := resolveTradeChannel(channels, "hyperliquid", "perps", false); got != "ch-hl-paper" {
		t.Errorf("paper with -paper key: expected ch-hl-paper, got %s", got)
	}

	if got := resolveTradeChannel(channels, "hyperliquid", "perps", true); got != "ch-hl" {
		t.Errorf("live trade: expected ch-hl, got %s", got)
	}

	if got := resolveTradeChannel(channels, "binanceus", "spot", false); got != "ch-spot" {
		t.Errorf("paper fallback to stratType: expected ch-spot, got %s", got)
	}

	if got := resolveTradeChannel(channels, "unknown", "unknown", false); got != "" {
		t.Errorf("paper no channel: expected empty, got %s", got)
	}

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

func TestIsPerpsType(t *testing.T) {
	spotFutures := []StrategyConfig{{Type: "spot"}, {Type: "futures"}}
	spotManual := []StrategyConfig{{Type: "spot"}, {Type: "manual"}}
	spotPerps := []StrategyConfig{{Type: "spot"}, {Type: "perps"}}

	if isPerpsType(spotFutures) {
		t.Error("expected false for spot/futures only")
	}
	if !isPerpsType(spotManual) {
		t.Error("expected true when manual present")
	}
	if !isPerpsType(spotPerps) {
		t.Error("expected true when perps present")
	}
}

func TestExtractAsset(t *testing.T) {
	cases := []struct {
		sc   StrategyConfig
		want string
	}{
		{StrategyConfig{Type: "spot", Args: []string{"sma_crossover", "BTC/USDT"}}, "BTC"},
		{StrategyConfig{Type: "options", Args: []string{"wheel", "ETH", "--platform=deribit"}}, "ETH"},
		{StrategyConfig{Type: "perps", Args: []string{"momentum", "SOL", "1h"}}, "SOL"},
		{StrategyConfig{Type: "perps", Args: []string{"rsi", "BNB", "1h"}}, "BNB"},
		{StrategyConfig{Type: "spot", Args: []string{}}, ""},
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

	if len(keys) != 4 {
		t.Fatalf("expected 4 asset keys, got %d: %v", len(keys), keys)
	}
	if keys[0] != "BTC" || keys[1] != "ETH" || keys[2] != "SOL" || keys[3] != "BNB" {
		t.Errorf("unexpected key order: %v", keys)
	}
	if len(groups["BTC"]) != 2 {
		t.Errorf("expected 2 BTC strategies, got %d", len(groups["BTC"]))
	}

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

	msgs := FormatCategorySummary(1, 0, 1, 0, 1000, prices, nil, strats, state, "hyperliquid", "BTC", 600, 0, nil, nil)
	msg := strings.Join(msgs, "\n")
	if !strings.Contains(msg, "— BTC") {
		t.Errorf("expected '— BTC' in title, got:\n%s", msg)
	}
	if strings.Contains(msg, "ETH") {
		t.Errorf("ETH price should be filtered out for asset=BTC, got:\n%s", msg)
	}

	msgs2 := FormatCategorySummary(1, 0, 1, 0, 1000, prices, nil, strats, state, "hyperliquid", "", 600, 0, nil, nil)
	msg2 := strings.Join(msgs2, "\n")
	if strings.Contains(msg2, "— ") {
		t.Errorf("expected no asset suffix when asset='', got:\n%s", msg2)
	}
}

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
	msgs := FormatCategorySummary(1, 0, 1, 0, 1000, prices, nil, strats, state, "hyperliquid", "BTC", 600, 0, nil, nil)
	summary := strings.Join(msgs, "\n")
	if !strings.Contains(summary, Version) {
		t.Errorf("expected version %q in summary title, got:\n%s", Version, summary)
	}

	msgs = FormatCategorySummary(1, 0, 1, 3, 1000, prices, nil, strats, state, "hyperliquid", "BTC", 600, 0, nil, nil)
	trades := strings.Join(msgs, "\n")
	if !strings.Contains(trades, Version) {
		t.Errorf("expected version %q in trades title, got:\n%s", Version, trades)
	}

	Version = ""
	msgs = FormatCategorySummary(1, 0, 1, 0, 1000, prices, nil, strats, state, "hyperliquid", "BTC", 600, 0, nil, nil)
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

	msgs := FormatCategorySummary(1, 0, 2, 0, 2000, prices, nil, strats, state, "hyperliquid", "BTC", 600, 0, nil, nil)
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
	if strings.Contains(msg, "hl-sma-btc") && strings.Contains(msg, "hl-sma-btc (resumes") {
		t.Errorf("hl-sma-btc should not have circuit breaker warning, got:\n%s", msg)
	}
	if strings.Contains(msg, "Trading active") {
		t.Errorf("should not show 'Trading active' when circuit breaker is active, got:\n%s", msg)
	}
}

func TestFormatCategorySummary_StrategiesSortedByID(t *testing.T) {
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
	msgs := FormatCategorySummary(1, 0, 1, 0, 2000, prices, nil, strats, state, "hyperliquid", "BTC", 600, 0, nil, nil)
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

	msgs := FormatCategorySummary(1, 0, 1, 0, 1000, prices, nil, strats, state, "hyperliquid", "BTC", 600, 0, nil, nil)
	msg := strings.Join(msgs, "\n")

	if strings.Contains(msg, "Circuit breaker") {
		t.Errorf("should not show circuit breaker when none active, got:\n%s", msg)
	}
	if !strings.Contains(msg, "Trading active") {
		t.Errorf("expected 'Trading active' status when no circuit breaker, got:\n%s", msg)
	}
}

func TestDiscordChannels_BackwardsCompatJSON(t *testing.T) {
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
		{90, "90s"},
		{45, "45s"},
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
		{StrategyConfig{Type: "perps", Args: []string{"rsi", "BTC", "1h"}}, "1h"},
		{StrategyConfig{Type: "perps", Args: []string{"sma", "ETH", "4h"}}, "4h"},
		{StrategyConfig{Type: "futures", Args: []string{"sma", "ES", "15m"}}, "15m"},
		{StrategyConfig{Type: "spot", Args: []string{"sma", "BTC", "1h"}}, "1h"},
		{StrategyConfig{Type: "spot", Args: []string{"sma_crossover", "BTC/USDT"}}, "—"},
		{StrategyConfig{Type: "options", Args: []string{"wheel", "ETH", "--platform=deribit"}}, "—"},
		{StrategyConfig{Type: "perps", Args: []string{"rsi"}}, "—"},
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
	strats := []StrategyConfig{
		{ID: "hl-rsi-btc", Type: "perps", Args: []string{"rsi", "BTC", "1h"}, Capital: 1000, IntervalSeconds: 600},
	}
	state := &AppState{
		Strategies: map[string]*StrategyState{
			"hl-rsi-btc": {Cash: 1000},
		},
	}
	prices := map[string]float64{"BTC/USDT": 50000}

	msgs := FormatCategorySummary(1, 0, 1, 0, 1000, prices, nil, strats, state, "hyperliquid", "BTC", 3600, 0, nil, nil)
	msg := strings.Join(msgs, "\n")

	if !strings.Contains(msg, "Tf") || !strings.Contains(msg, "Int") {
		t.Errorf("expected 'Tf' and 'Int' column headers, got:\n%s", msg)
	}
	if !strings.Contains(msg, "1h") || !strings.Contains(msg, "10m") {
		t.Errorf("expected '1h' and '10m' for perps with 1h timeframe and 600s interval, got:\n%s", msg)
	}
}

func TestFormatCategorySummary_TfIntGlobalFallback(t *testing.T) {
	strats := []StrategyConfig{
		{ID: "sma-btc", Type: "spot", Args: []string{"sma_crossover", "BTC/USDT"}, Capital: 1000},
	}
	state := &AppState{
		Strategies: map[string]*StrategyState{
			"sma-btc": {Cash: 1000},
		},
	}
	prices := map[string]float64{"BTC/USDT": 50000}

	msgs := FormatCategorySummary(1, 0, 1, 0, 1000, prices, nil, strats, state, "spot", "", 3600, 0, nil, nil)
	msg := strings.Join(msgs, "\n")

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

	msgs := FormatCategorySummary(1, 0, 3, 0, 3000, prices, nil, strats, state, "hyperliquid", "BTC", 600, 0, nil, nil)
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

	msgs := FormatCategorySummary(1, 0, 2, 0, 2000, prices, nil, strats, state, "hyperliquid", "BTC", 600, 0, nil, nil)
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

	msgs := FormatCategorySummary(1, 0, 2, 0, -1, prices, nil, strats, state, "hyperliquid", "ETH", 600, 0, nil, nil)
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

	msgs := FormatCategorySummary(1, 0, 3, 0, 3000, prices, nil, strats, state, "hyperliquid", "BTC", 600, 0, lifetime, nil)
	msg := strings.Join(msgs, "\n")

	if !strings.Contains(msg, "#T") {
		t.Errorf("expected '#T' column header, got:\n%s", msg)
	}
	intIdx := strings.LastIndex(msg, "Int")
	tIdx := strings.Index(msg, "#T")
	if intIdx < 0 || tIdx < 0 || tIdx < intIdx {
		t.Errorf("expected #T column to follow Int column, got Int@%d #T@%d:\n%s", intIdx, tIdx, msg)
	}

	if !strings.Contains(msg, "    7     —\n") {
		t.Errorf("expected closed-trade count '7' for hl-rsi-btc, got:\n%s", msg)
	}
	if !strings.Contains(msg, "   12     —\n") {
		t.Errorf("expected closed-trade count '12' for hl-sma-btc, got:\n%s", msg)
	}
	if !strings.Contains(msg, "    0     —\n") {
		t.Errorf("expected closed-trade count '0' for hl-mom-btc, got:\n%s", msg)
	}
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

	msgs := FormatCategorySummary(1, 0, 2, 0, -1, prices, nil, strats, state, "hyperliquid", "ETH", 600, 0, lifetime, nil)
	msg := strings.Join(msgs, "\n")

	if !strings.Contains(msg, "#T") {
		t.Errorf("expected '#T' column header in shared-wallet variant, got:\n%s", msg)
	}
	walletIdx := strings.Index(msg, "Wallet%")
	tIdx := strings.Index(msg, "#T")
	if walletIdx < 0 || tIdx < walletIdx {
		t.Errorf("expected #T after Wallet%% in shared-wallet variant, got Wallet%%@%d #T@%d:\n%s", walletIdx, tIdx, msg)
	}
	if !strings.Contains(msg, "    4     —\n") {
		t.Errorf("expected closed-trade count '4' for hl-rmc-eth, got:\n%s", msg)
	}
	if !strings.Contains(msg, "    9     —\n") {
		t.Errorf("expected closed-trade count '9' for hl-tema-eth, got:\n%s", msg)
	}
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

	msgs := FormatCategorySummary(1, 0, 3, 0, 3000, prices, nil, strats, state, "hyperliquid", "BTC", 600, 0, lifetime, nil)
	msg := strings.Join(msgs, "\n")

	if !strings.Contains(msg, "W/L") {
		t.Errorf("expected 'W/L' column header, got:\n%s", msg)
	}
	tIdx := strings.Index(msg, "#T")
	wlIdx := strings.Index(msg, "W/L")
	if tIdx < 0 || wlIdx < 0 || wlIdx < tIdx {
		t.Errorf("expected W/L column to follow #T, got #T@%d W/L@%d:\n%s", tIdx, wlIdx, msg)
	}

	if !strings.Contains(msg, "  2.33\n") {
		t.Errorf("expected W/L '2.33' for hl-rsi-btc (7/3), got:\n%s", msg)
	}
	if !strings.Contains(msg, "    ∞\n") {
		t.Errorf("expected W/L '∞' for hl-sma-btc (5/0), got:\n%s", msg)
	}
	if !strings.Contains(msg, "    —\n") {
		t.Errorf("expected W/L '—' for hl-mom-btc (no trades), got:\n%s", msg)
	}

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

	msgs := FormatCategorySummary(1, 0, 2, 0, -1, prices, nil, strats, state, "hyperliquid", "ETH", 600, 0, nil, nil)
	msg := strings.Join(msgs, "\n")

	if !strings.Contains(msg, "Wallet%") {
		t.Errorf("expected 'Wallet%%' column header, got:\n%s", msg)
	}
	if !strings.Contains(msg, "Initial capital: $1,000") {
		t.Errorf("expected aggregate initial capital in header, got:\n%s", msg)
	}
	if strings.Contains(msg, " Init ") {
		t.Errorf("Init column should be removed from table, got:\n%s", msg)
	}
	if !strings.Contains(msg, "50.0%") {
		t.Errorf("expected '50.0%%' wallet share, got:\n%s", msg)
	}
	if !strings.Contains(msg, "100.0%") {
		t.Errorf("expected '100.0%%' total wallet share, got:\n%s", msg)
	}
	if !strings.Contains(msg, "1,085") {
		t.Errorf("expected total value ~1,085, got:\n%s", msg)
	}
	if !strings.Contains(msg, "542") {
		t.Errorf("expected individual value ~542, got:\n%s", msg)
	}
	if !strings.Contains(msg, "+42") && !strings.Contains(msg, "+43") {
		t.Errorf("expected positive PnL from InitialCapital baseline, got:\n%s", msg)
	}
}

func TestFormatCategorySummary_WalletPctFromConfig(t *testing.T) {
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

	msgs := FormatCategorySummary(1, 0, 2, 0, -1, prices, nil, strats, state, "hyperliquid", "ETH", 600, 0, nil, nil)
	msg := strings.Join(msgs, "\n")

	if !strings.Contains(msg, "30.0%") {
		t.Errorf("expected '30.0%%' from capital_pct=0.3, got:\n%s", msg)
	}
	if !strings.Contains(msg, "70.0%") {
		t.Errorf("expected '70.0%%' from capital_pct=0.7, got:\n%s", msg)
	}
}

func TestFormatCategorySummary_NoSharedWallet(t *testing.T) {
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

	msgs := FormatCategorySummary(1, 0, 2, 0, -1, prices, nil, strats, state, "hyperliquid", "ETH", 600, 0, nil, nil)
	msg := strings.Join(msgs, "\n")

	if strings.Contains(msg, "Wallet%") {
		t.Errorf("should not show Wallet%% column without shared wallet, got:\n%s", msg)
	}
	if !strings.Contains(msg, "Initial capital: $1,000") {
		t.Errorf("expected aggregate initial capital in header, got:\n%s", msg)
	}
	if strings.Contains(msg, " Init ") {
		t.Errorf("Init column should be removed from table, got:\n%s", msg)
	}
}

func TestFormatCategorySummary_MessageSplitting(t *testing.T) {
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

	msgs := FormatCategorySummary(1, 0, 20, 0, 10000, prices, nil, strats, state, "hyperliquid", "BTC", 600, 0, nil, nil)

	if len(msgs) < 2 {
		totalLen := 0
		for _, m := range msgs {
			totalLen += len(m)
		}
		t.Errorf("expected multiple messages for 20 positions, got %d (total chars: %d)", len(msgs), totalLen)
	}

	if strings.Contains(msgs[0], "  • ") {
		t.Errorf("first message should not contain position bullets, got:\n%s", msgs[0])
	}
	if strings.Contains(msgs[0], "... and") {
		t.Errorf("first message should not contain '... and N more' indicator under #728 layout, got:\n%s", msgs[0])
	}
	if !strings.Contains(msgs[0], "Positions: 20 open") {
		t.Errorf("first message should still contain 'Positions: 20 open' summary line, got:\n%s", msgs[0])
	}

	for i, m := range msgs {
		if len(m) > discordCharLimit {
			t.Errorf("msg[%d] exceeds %d chars: %d", i, discordCharLimit, len(m))
		}
	}

	posStart := -1
	for i := 1; i < len(msgs); i++ {
		if strings.HasPrefix(msgs[i], "Positions:\n") {
			posStart = i
			break
		}
	}
	if posStart == -1 {
		t.Fatalf("expected a 'Positions:' message after msg 0, got:\n%s", strings.Join(msgs, "\n---\n"))
	}

	posBlock := strings.Join(msgs[posStart:], "\n")
	for i := 0; i < 20; i++ {
		want := fmt.Sprintf("hl-strat%02d-btc", i)
		if !strings.Contains(posBlock, want) {
			t.Errorf("position for %s missing from positions block:\n%s", want, posBlock)
		}
	}
}

func TestFormatCategorySummary_NoSplitWhenShort(t *testing.T) {
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

	msgs := FormatCategorySummary(1, 0, 1, 0, 1000, prices, nil, strats, state, "hyperliquid", "BTC", 600, 0, nil, nil)

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
	if !strings.Contains(lines[0], "@ $2,213.08") {
		t.Errorf("expected entry price '@ $2,213.08' in line, got: %s", lines[0])
	}
	if !strings.Contains(lines[0], "(+$2.70)") {
		t.Errorf("expected PnL '(+$2.70)' in line, got: %s", lines[0])
	}
	if !strings.Contains(lines[0], "LONG") {
		t.Errorf("expected 'LONG' direction label, got: %s", lines[0])
	}
}

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
	if !strings.Contains(lines[0], "@ $50,000.00") {
		t.Errorf("expected entry price '@ $50,000.00' in line, got: %s", lines[0])
	}
	if !strings.Contains(lines[0], "(-$100.00)") {
		t.Errorf("expected PnL '(-$100.00)' in line, got: %s", lines[0])
	}
	if !strings.Contains(lines[0], "SHORT") {
		t.Errorf("expected 'SHORT' direction label, got: %s", lines[0])
	}
}

func TestCollectPositions_StopLoss(t *testing.T) {
	pointer := func(v float64) *float64 { return &v }
	cases := []struct {
		name       string
		side       string
		avg        float64
		sl         float64
		atr        float64
		mult       *float64
		oid        int64
		want       string
		wantAbsent bool
	}{
		{name: "long", side: "long", avg: 63500, sl: 61595, oid: 12345, want: "| SL: $61,595.00 (-3.0%)"},
		{name: "short", side: "short", avg: 63500, sl: 65405, oid: 99, want: "| SL: $65,405.00 (-3.0%)"},
		{name: "trigger_without_oid", side: "long", avg: 63500, sl: 61595, want: "| SL: $61,595.00 (-3.0%)"},
		{name: "without_trigger", side: "long", avg: 63500, oid: 12345, wantAbsent: true},
		{name: "long_1x", side: "long", avg: 10000, sl: 9000, atr: 1000, mult: pointer(1.0), want: "| SL: $9,000.00 (-10.0%) (1x)"},
		{name: "long_1.5x", side: "long", avg: 10000, sl: 9700, atr: 200, mult: pointer(1.5), want: "| SL: $9,700.00 (-3.0%) (1.5x)"},
		{name: "long_1.5x_rounded_trigger", side: "long", avg: 2335.10, sl: 2323.30, atr: 7.92, mult: pointer(1.5), want: "| SL: $2,323.30 (-0.5%) (1.5x)"},
		{name: "long_1.25x", side: "long", avg: 10000, sl: 9750, atr: 200, mult: pointer(1.25), want: "| SL: $9,750.00 (-2.5%) (1.25x)"},
		{name: "short_1x", side: "short", avg: 10000, sl: 11000, atr: 1000, mult: pointer(1.0), want: "| SL: $11,000.00 (-10.0%) (1x)"},
		{name: "without_multiplier", side: "long", avg: 10000, sl: 9500, atr: 250, want: "| SL: $9,500.00 (-5.0%)"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ss := &StrategyState{
				Positions: map[string]*Position{
					"BTC/USDT": {
						Symbol: "BTC/USDT", Quantity: 0.01, AvgCost: tc.avg, Side: tc.side,
						StopLossOID: tc.oid, StopLossTriggerPx: tc.sl, EntryATR: tc.atr, StopLossATRMult: tc.mult,
					},
				},
			}
			lines := collectPositions(StrategyConfig{ID: "hl-btc-sma"}, ss, map[string]float64{"BTC/USDT": tc.avg})
			if len(lines) != 1 {
				t.Fatalf("expected 1 line, got %d", len(lines))
			}
			if tc.wantAbsent {
				if strings.Contains(lines[0], "SL:") {
					t.Errorf("SL fragment should be omitted when StopLossTriggerPx=0, got: %s", lines[0])
				}
				return
			}
			if !strings.Contains(lines[0], tc.want) {
				t.Errorf("expected SL fragment %q, got: %s", tc.want, lines[0])
			}
			if tc.mult == nil && strings.Contains(lines[0], "x)") {
				t.Errorf("expected no multiplier suffix when StopLossATRMult=nil, got: %s", lines[0])
			}
		})
	}
}

func TestCollectPositions_LeverageMargin(t *testing.T) {
	ss := &StrategyState{
		Positions: map[string]*Position{
			"BTC/USDT": {Symbol: "BTC/USDT", Quantity: 0.025, AvgCost: 63500, Side: "long", Leverage: 5},
		},
	}
	lines := collectPositions(StrategyConfig{ID: "hl-btc-sma"}, ss, map[string]float64{"BTC/USDT": 63500})
	if !strings.Contains(lines[0], "| 5x ($318 margin)") {
		t.Errorf("expected '5x ($318 margin)' fragment, got: %s", lines[0])
	}
}

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

func TestCollectPositions_TieredTPATR(t *testing.T) {
	cases := []struct {
		name        string
		id          string
		closeName   string
		side        string
		wantTargets string
	}{
		{
			name:        "long",
			id:          "hl-tatr-btc",
			closeName:   "tiered_tp_atr",
			side:        "long",
			wantTargets: "| TP1: $65,000.00 (+2.4%) (1.5x) | TP2: $66,500.00 (+4.7%) (3x)",
		},
		{
			name:        "short",
			id:          "hl-tatr-btc",
			closeName:   "tiered_tp_atr",
			side:        "short",
			wantTargets: "| TP1: $62,000.00 (+2.4%) (1.5x) | TP2: $60,500.00 (+4.7%) (3x)",
		},
		{
			name:        "live_long",
			id:          "hl-tatr-live-btc",
			closeName:   "tiered_tp_atr_live",
			side:        "long",
			wantTargets: "| TP1: $65,000.00 (+2.4%) (1.5x) | TP2: $66,500.00 (+4.7%) (3x)",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sc := StrategyConfig{ID: tc.id, CloseStrategy: &StrategyRef{Name: tc.closeName}}
			ss := &StrategyState{
				Positions: map[string]*Position{
					"BTC/USDT": {Symbol: "BTC/USDT", Quantity: 0.025, AvgCost: 63500, Side: tc.side, EntryATR: 1000},
				},
			}
			lines := collectPositions(sc, ss, map[string]float64{"BTC/USDT": 63500})
			if len(lines) != 1 {
				t.Fatalf("expected 1 line, got %d", len(lines))
			}
			if !strings.Contains(lines[0], "| ATR: $1,000.00") {
				t.Errorf("expected ATR fragment, got: %s", lines[0])
			}
			if !strings.Contains(lines[0], tc.wantTargets) {
				t.Errorf("expected tiered TP fragments, got: %s", lines[0])
			}
		})
	}
}

func TestCollectPositions_TrailingTPRatchetShowsRatchetState(t *testing.T) {
	initialTrail := 3.0
	currentTrail := 1.0
	sc := StrategyConfig{
		ID: "hl-ratchet-btc",
		CloseStrategy: &StrategyRef{
			Name: "trailing_tp_ratchet",
			Params: map[string]interface{}{
				"tp_tiers": []interface{}{
					map[string]interface{}{
						"atr_multiple": 1.0, "close_fraction": 0.0, "trailing_mult_after": 2.0,
					},
					map[string]interface{}{
						"atr_multiple": 2.0, "close_fraction": 0.0, "trailing_mult_after": 1.0,
					},
				},
			},
		},
		TrailingStopATRMult: &initialTrail,
	}
	ss := &StrategyState{
		Positions: map[string]*Position{
			"BTC/USDT": {
				Symbol: "BTC/USDT", Quantity: 0.025, AvgCost: 63500, Side: "long", EntryATR: 1000,
				SLAdjustedTiersProcessed: 1, PostTPTrailingATRMult: &currentTrail,
			},
		},
	}
	lines := collectPositions(sc, ss, map[string]float64{"BTC/USDT": 63500})
	if len(lines) != 1 {
		t.Fatalf("expected 1 line, got %d", len(lines))
	}
	for _, want := range []string{
		"| Ratchet: 1/2 | Trail: 1x ATR",
		"| RT1: $64,500.00 (+1.6%) (1x -> 2x trail)",
		"| RT2: $65,500.00 (+3.1%) (2x -> 1x trail)",
	} {
		if !strings.Contains(lines[0], want) {
			t.Errorf("expected ratchet fragment %q, got: %s", want, lines[0])
		}
	}
}

func TestCollectPositions_ShowsCloseStrategyName(t *testing.T) {
	sc := StrategyConfig{
		ID:            "manual-eth",
		CloseStrategy: &StrategyRef{Name: "trailing_tp_ratchet"},
	}
	ss := &StrategyState{
		Positions: map[string]*Position{
			"ETH/USDT": {Symbol: "ETH/USDT", Quantity: 0.516, AvgCost: 1938, Side: "short", EntryATR: 30},
		},
	}
	lines := collectPositions(sc, ss, map[string]float64{"ETH/USDT": 1938})
	if len(lines) != 1 {
		t.Fatalf("expected 1 line, got %d", len(lines))
	}
	if !strings.Contains(lines[0], "| close: trailing_tp_ratchet") {
		t.Errorf("expected close-strategy name fragment, got: %s", lines[0])
	}
	closeIdx := strings.Index(lines[0], "| close:")
	atrIdx := strings.Index(lines[0], "| ATR:")
	if closeIdx < 0 || atrIdx < 0 || closeIdx > atrIdx {
		t.Errorf("expected close label before ATR param, got: %s", lines[0])
	}
}

func TestCollectPositions_OmitsCloseLabelWhenOpenAsClose(t *testing.T) {
	sc := StrategyConfig{ID: "hl-rsi-btc"}
	ss := &StrategyState{
		Positions: map[string]*Position{
			"BTC/USDT": {Symbol: "BTC/USDT", Quantity: 0.025, AvgCost: 63500, Side: "long"},
		},
	}
	lines := collectPositions(sc, ss, map[string]float64{"BTC/USDT": 63500})
	if strings.Contains(lines[0], "close:") {
		t.Errorf("open-as-close should add no close label, got: %s", lines[0])
	}
}

func TestCollectPositions_TieredTPATR_OmittedWithoutCloseStrategy(t *testing.T) {
	sc := StrategyConfig{ID: "hl-rsi-btc", CloseStrategy: &StrategyRef{Name: "tiered_tp_pct"}}
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
	sc := StrategyConfig{ID: "hl-tatr-btc", CloseStrategy: &StrategyRef{Name: "tiered_tp_atr"}}
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

func TestCollectPositions_TieredTPATR_FilledTierMarked(t *testing.T) {
	sc := StrategyConfig{
		ID:            "hl-tatr-btc",
		CloseStrategy: &StrategyRef{Name: "tiered_tp_atr"},
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
	if !strings.Contains(lines[0], "| TP1: $65,000.00 (1.5x) ✓") {
		t.Errorf("expected TP1 marked filled, got: %s", lines[0])
	}
	if !strings.Contains(lines[0], "| TP2: $66,500.00 (+4.7%) (3x)") {
		t.Errorf("expected TP2 still pending, got: %s", lines[0])
	}
}

func TestCollectPositions_TieredTPATR_NoFillBeforeProtectionSync(t *testing.T) {
	sc := StrategyConfig{
		ID:            "hl-tatr-btc",
		CloseStrategy: &StrategyRef{Name: "tiered_tp_atr"},
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
	if !strings.Contains(lines[0], "| TP1: $65,000.00 (+2.4%) (1.5x) | TP2: $66,500.00 (+4.7%) (3x)") {
		t.Errorf("expected both tiers pending, got: %s", lines[0])
	}
}

func TestCollectPositions_TieredTPATRRegime_StampedRegime(t *testing.T) {
	sc := StrategyConfig{
		ID: "hl-reg-btc",
		CloseStrategy: &StrategyRef{
			Name:   "tiered_tp_atr_regime",
			Params: map[string]interface{}{"use_defaults": true},
		},
	}
	ss := &StrategyState{
		Positions: map[string]*Position{
			"BTC/USDT": {
				Symbol:   "BTC/USDT",
				Quantity: 0.025,
				AvgCost:  63500,
				Side:     "long",
				EntryATR: 1000,
				Regime:   "trending_up",
			},
		},
	}
	lines := collectPositions(sc, ss, map[string]float64{"BTC/USDT": 63500})
	if !strings.Contains(lines[0], "| TP1: $65,000.00") || !strings.Contains(lines[0], "| TP2: $66,500.00") {
		t.Errorf("expected regime-resolved TP prices, got: %s", lines[0])
	}
}

func TestCollectPositions_AllFragments_WithTieredTP(t *testing.T) {
	opened := time.Date(2026, 4, 28, 14, 32, 0, 0, time.UTC)
	sc := StrategyConfig{ID: "hl-tatr-btc", CloseStrategy: &StrategyRef{Name: "tiered_tp_atr"}}
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

func TestPercentFromEntry(t *testing.T) {
	cases := []struct {
		side   string
		entry  float64
		target float64
		want   float64
	}{
		{"long", 100, 97, -3},
		{"long", 100, 103, 3},
		{"short", 100, 103, -3},
		{"short", 100, 97, 3},
		{"long", 0, 100, 0},
	}
	for _, c := range cases {
		got := percentFromEntry(c.side, c.entry, c.target)
		if math.Abs(got-c.want) > 1e-9 {
			t.Errorf("percentFromEntry(%s, %g, %g) = %g, want %g", c.side, c.entry, c.target, got, c.want)
		}
	}
}

func TestPositionMargin(t *testing.T) {
	if got := positionMargin(0.025, 63500, 5); math.Abs(got-317.5) > 1e-9 {
		t.Errorf("positionMargin(0.025, 63500, 5) = %g, want 317.5", got)
	}
	if got := positionMargin(1, 100, 0); got != 0 {
		t.Errorf("positionMargin with leverage=0 should be 0, got %g", got)
	}
}

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

	msgs := FormatCategorySummary(1, 0, 1, 0, 1000, prices, nil, strats, state, "hyperliquid", "ETH", 600, 0, nil, nil)
	msg := strings.Join(msgs, "\n")
	if !strings.Contains(msg, "ETH: $2,240.50") {
		t.Errorf("expected header price 'ETH: $2,240.50', got:\n%s", msg)
	}
	if strings.Contains(msg, "ETH $2240") {
		t.Errorf("old header format 'ETH $2240' should be removed, got:\n%s", msg)
	}
}

func TestFormatCategorySummary_RegimePriceLine741(t *testing.T) {
	strats := []StrategyConfig{
		{ID: "hl-a-eth", Type: "perps", Args: []string{"rsi", "ETH/USDT", "1h"}, Capital: 1000, Platform: "hyperliquid"},
		{ID: "hl-b-eth", Type: "perps", Args: []string{"sma", "ETH/USDT", "1h"}, Capital: 1000, Platform: "hyperliquid"},
	}
	state := &AppState{
		Strategies: map[string]*StrategyState{
			"hl-a-eth": {Cash: 1000, Regime: "trending_down"},
			"hl-b-eth": {Cash: 1000, Regime: "trending_down"},
		},
	}
	prices := map[string]float64{"ETH/USDT": 2277.25}
	regimeOn := &RegimeConfig{Enabled: true, Period: 14, ADXThreshold: 20}

	msgs := FormatCategorySummary(1, 0, 2, 0, 2000, prices, nil, strats, state, "hyperliquid", "ETH", 600, 0, nil, regimeOn)
	msg := strings.Join(msgs, "\n")
	if !strings.Contains(msg, "ETH: $2,277.25 | trending_down") {
		t.Errorf("expected regime suffix on single ETH price segment, got:\n%s", msg)
	}
	if strings.Count(msg, "trending_down") != 1 {
		t.Errorf("expected exactly one trending_down on price line, got:\n%s", msg)
	}

	msgsOff := FormatCategorySummary(1, 0, 2, 0, 2000, prices, nil, strats, state, "hyperliquid", "ETH", 600, 0, nil, nil)
	msgOff := strings.Join(msgsOff, "\n")
	if !strings.Contains(msgOff, "ETH: $2,277.25") || strings.Contains(msgOff, "trending_down") {
		t.Errorf("expected price line without regime when cfg.regime nil, got:\n%s", msgOff)
	}

	state.Strategies["hl-a-eth"].Regime = ""
	state.Strategies["hl-b-eth"].Regime = ""
	msgsEmpty := FormatCategorySummary(1, 0, 2, 0, 2000, prices, nil, strats, state, "hyperliquid", "ETH", 600, 0, nil, regimeOn)
	msgEmpty := strings.Join(msgsEmpty, "\n")
	if !strings.Contains(msgEmpty, "ETH: $2,277.25") || strings.Contains(msgEmpty, "ETH: $2,277.25 |") {
		t.Errorf("expected price line without regime suffix when labels empty, got:\n%s", msgEmpty)
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

	msgs := FormatCategorySummary(1, 0, stratCount, 0, 14000, prices, nil, strats, state, "hyperliquid", "BTC", 600, 0, nil, nil)

	if len(msgs) < 2 {
		t.Fatalf("expected at least 2 messages for %d strategies, got %d", stratCount, len(msgs))
	}
	for i, m := range msgs {
		if len(m) > discordCharLimit {
			t.Errorf("msg[%d] exceeds Discord limit: %d chars", i, len(m))
		}
		if strings.Count(m, "```")%2 != 0 {
			t.Errorf("msg[%d] has unbalanced code-block fences:\n%s", i, m)
		}
	}

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

	if !strings.Contains(msgs[1], "cont'd") {
		t.Errorf("second message should be the continuation table label, got:\n%s", msgs[1])
	}
	if !strings.Contains(msgs[1], "```") {
		t.Errorf("continuation table must be wrapped in a code block, got:\n%s", msgs[1])
	}
	if !strings.Contains(msgs[1], contChunkFirst) {
		t.Errorf("continuation should contain row %d (%s), got:\n%s", catTableMaxRows+1, contChunkFirst, msgs[1])
	}
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

	all := strings.Join(msgs, "\n")
	for i := 0; i < stratCount; i++ {
		want := fmt.Sprintf("hl-strat%02d-b", i)
		if !strings.Contains(all, want) {
			t.Errorf("strategy row %s missing from messages", want)
		}
	}
}

func TestSplitCategorySummary_ContinuationTablesInserted(t *testing.T) {
	header := "Header line\n"
	posLines := []string{"pos1", "pos2"}
	conts := []string{"```\nchunk2\n```\n", "```\nchunk3\n```\n"}

	msgs := splitCategorySummary(header, 2, posLines, nil, conts)
	if len(msgs) != 4 {
		t.Fatalf("expected 4 messages (header + 2 table conts + positions), got %d:\n%s", len(msgs), strings.Join(msgs, "\n---\n"))
	}
	if !strings.Contains(msgs[0], "Header line") {
		t.Errorf("msg[0] should contain header, got: %s", msgs[0])
	}
	if strings.Contains(msgs[0], "  • ") {
		t.Errorf("msg[0] should not contain position bullets, got: %s", msgs[0])
	}
	if msgs[1] != conts[0] {
		t.Errorf("msg[1] should be first continuation table, got: %s", msgs[1])
	}
	if msgs[2] != conts[1] {
		t.Errorf("msg[2] should be second continuation table, got: %s", msgs[2])
	}
	if !strings.HasPrefix(msgs[3], "Positions:\n") {
		t.Errorf("msg[3] should be positions block, got: %s", msgs[3])
	}
	if !strings.Contains(msgs[3], "pos1") || !strings.Contains(msgs[3], "pos2") {
		t.Errorf("msg[3] should contain all positions, got: %s", msgs[3])
	}
}

func TestSplitCategorySummary_PeelTradesWhenMsg1Overflows(t *testing.T) {
	header := strings.Repeat("h", 1950) + "\n"
	posLines := []string{"alpha", "beta"}
	var tradeLines []string
	for i := 0; i < 5; i++ {
		tradeLines = append(tradeLines, fmt.Sprintf("  • TRADE %d EXECUTED LONG BTC/USDT x0.025 @ $63,500.00", i))
	}

	msgs := splitCategorySummary(header, len(posLines), posLines, tradeLines, nil)
	if len(msgs) < 3 {
		t.Fatalf("expected at least 3 messages (header / trades / positions), got %d:\n%s", len(msgs), strings.Join(msgs, "\n---\n"))
	}
	for i, m := range msgs {
		if len(m) > discordCharLimit {
			t.Errorf("msg[%d] exceeds Discord hard limit %d: %d", i, discordCharLimit, len(m))
		}
	}
	if strings.Contains(msgs[0], "**Trades:**") {
		t.Errorf("msg[0] should NOT contain trades section when peeled, got tail:\n%s", msgs[0][len(msgs[0])-200:])
	}
	if strings.Contains(msgs[0], "  • ") {
		t.Errorf("msg[0] should NOT contain bullets, got tail:\n%s", msgs[0][len(msgs[0])-200:])
	}
	if !strings.Contains(msgs[1], "**Trades:**") {
		t.Errorf("msg[1] should be the trades section, got:\n%s", msgs[1])
	}
	last := msgs[len(msgs)-1]
	if !strings.HasPrefix(last, "Positions:\n") {
		t.Errorf("final message should be positions block, got:\n%s", last)
	}
	for _, want := range posLines {
		if !strings.Contains(last, want) {
			t.Errorf("final message missing position %q:\n%s", want, last)
		}
	}
}

func TestSplitCategorySummary_PositionsAlwaysInSeparateMessage_Issue728(t *testing.T) {
	header := strings.Repeat("h", 1900) + "\n"
	posLines := []string{"alpha", "beta", "gamma"}
	tradeLines := []string{"  • TRADE EXECUTED LONG BTC"}

	msgs := splitCategorySummary(header, len(posLines), posLines, tradeLines, nil)
	if len(msgs) < 2 {
		t.Fatalf("expected split, got %d msgs", len(msgs))
	}

	if !strings.Contains(msgs[0], "Positions: 3 open") {
		t.Errorf("msg[0] should contain position count line, got tail:\n%s", msgs[0][len(msgs[0])-200:])
	}
	if !strings.Contains(msgs[0], "**Trades:**") {
		t.Errorf("msg[0] should contain trades section, got tail:\n%s", msgs[0][len(msgs[0])-200:])
	}
	if strings.Contains(msgs[0], "  • alpha") || strings.Contains(msgs[0], "  • beta") || strings.Contains(msgs[0], "  • gamma") {
		t.Errorf("msg[0] should NOT contain any position bullet under #728 layout, got tail:\n%s", msgs[0][len(msgs[0])-200:])
	}
	if strings.Contains(msgs[0], "... and") {
		t.Errorf("msg[0] should NOT contain '... and N more' truncation marker, got tail:\n%s", msgs[0][len(msgs[0])-200:])
	}

	if !strings.HasPrefix(msgs[1], "Positions:\n") {
		t.Errorf("msg[1] should begin with 'Positions:' header, got:\n%s", msgs[1])
	}
	for _, want := range []string{"alpha", "beta", "gamma"} {
		if !strings.Contains(msgs[1], want) {
			t.Errorf("msg[1] should contain position %q, got:\n%s", want, msgs[1])
		}
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
	if strings.Contains(msg, "**") {
		t.Errorf("plain format should not contain Discord markdown '**', got:\n%s", msg)
	}
}

func TestFormatTradeDMPlain_OpenWithCustomTiers(t *testing.T) {
	sc := StrategyConfig{
		ID:       "hl-tema-eth-live",
		Platform: "hyperliquid",
		Type:     "perps",
		CloseStrategy: &StrategyRef{
			Name: "tiered_tp_atr_live",
			Params: map[string]interface{}{
				"tp_tiers": []interface{}{
					map[string]interface{}{"atr_multiple": 2.0, "close_fraction": 0.5},
					map[string]interface{}{"atr_multiple": 3.0, "close_fraction": 1.0},
				},
			},
		},
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
	header := strings.Repeat("x", 1900) + "\n"
	posLines := []string{"position-line-1-aaaa", "position-line-2-bbbb", "position-line-3-cccc"}

	msgs := splitCategorySummary(header, 3, posLines, nil, nil)
	if len(msgs) < 2 {
		t.Fatalf("expected multiple messages with large header, got %d", len(msgs))
	}
	if strings.Contains(msgs[0], "  • ") {
		t.Errorf("first message should not contain position bullets, got tail:\n%s", msgs[0][len(msgs[0])-200:])
	}
	if strings.Contains(msgs[0], "... and") {
		t.Errorf("first message should not contain '... and N more' under #728 layout, got tail:\n%s", msgs[0][len(msgs[0])-200:])
	}
	posBlock := strings.Join(msgs[1:], "\n")
	for _, pl := range posLines {
		if !strings.Contains(posBlock, pl) {
			t.Errorf("position %q missing from positions block:\n%s", pl, posBlock)
		}
	}
	if !strings.HasPrefix(msgs[1], "Positions:\n") {
		t.Errorf("msg[1] should start with 'Positions:' header, got:\n%s", msgs[1])
	}
}

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
	msgs := FormatCategorySummary(1, 0, 1, 0, 1000, prices, nil, strats, state, "hyperliquid", "ETH", 600, 0, lifetime, nil)
	if len(msgs) == 0 {
		t.Fatal("expected at least one message")
	}
	msg := msgs[0]
	if !strings.Contains(msg, " 17 ") {
		t.Errorf("expected lifetime #T=17 in summary, got:\n%s", msg)
	}
	if !strings.Contains(msg, "1.43") {
		t.Errorf("expected lifetime W/L ratio (10/7=1.43) in summary, got:\n%s", msg)
	}
}

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
	msgs := FormatCategorySummary(1, 0, 1, 0, 1000, prices, nil, strats, state, "hyperliquid", "ETH", 600, 0, nil, nil)
	if !strings.Contains(msgs[0], " 0     —") {
		t.Errorf("expected zero #T/W-L without lifetime stats, got:\n%s", msgs[0])
	}
	msgs2 := FormatCategorySummary(1, 0, 1, 0, 1000, prices, nil, strats, state, "hyperliquid", "ETH", 600, 0, map[string]LifetimeTradeStats{}, nil)
	if !strings.Contains(msgs2[0], " 0     —") {
		t.Errorf("expected zero #T/W-L from empty lifetime stats map, got:\n%s", msgs2[0])
	}
}

func TestFormatTradeDM_OpenWithCustomTiers(t *testing.T) {
	sc := StrategyConfig{
		ID:       "hl-tema-eth-live",
		Platform: "hyperliquid",
		Type:     "perps",
		CloseStrategy: &StrategyRef{
			Name: "tiered_tp_atr_live",
			Params: map[string]interface{}{
				"tp_tiers": []interface{}{
					map[string]interface{}{"atr_multiple": 2.0, "close_fraction": 0.5},
					map[string]interface{}{"atr_multiple": 3.0, "close_fraction": 1.0},
				},
			},
		},
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
	if !strings.Contains(msg, "TP1: $2,340.92") {
		t.Errorf("expected 'TP1: $2,340.92' (2× ATR) in DM, got:\n%s", msg)
	}
	if !strings.Contains(msg, "TP2: $2,352.93") {
		t.Errorf("expected 'TP2: $2,352.93' (3× ATR) in DM, got:\n%s", msg)
	}
}

func TestFormatTradeDM_OpenWithThreeTiers(t *testing.T) {
	sc := StrategyConfig{
		ID:       "hl-tatr-btc",
		Platform: "hyperliquid",
		Type:     "perps",
		CloseStrategy: &StrategyRef{
			Name: "tiered_tp_atr",
			Params: map[string]interface{}{
				"tp_tiers": []interface{}{
					map[string]interface{}{"atr_multiple": 1.0, "close_fraction": 0.3},
					map[string]interface{}{"atr_multiple": 2.0, "close_fraction": 0.6},
					map[string]interface{}{"atr_multiple": 3.0, "close_fraction": 1.0},
				},
			},
		},
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

func TestCollectPositions_TieredTPATR_CustomTiers(t *testing.T) {
	sc := StrategyConfig{
		ID: "hl-tema-eth-live",
		CloseStrategy: &StrategyRef{
			Name: "tiered_tp_atr_live",
			Params: map[string]interface{}{
				"tp_tiers": []interface{}{
					map[string]interface{}{"atr_multiple": 2.0, "close_fraction": 0.5},
					map[string]interface{}{"atr_multiple": 3.0, "close_fraction": 1.0},
				},
			},
		},
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
	if !strings.Contains(lines[0], "TP1: $2,340.92 (+1.0%) (2x)") {
		t.Errorf("expected TP1=2,340.92 (2× ATR) with mult suffix in line, got: %s", lines[0])
	}
	if !strings.Contains(lines[0], "TP2: $2,352.93 (+1.6%) (3x)") {
		t.Errorf("expected TP2=2,352.93 (3× ATR) with mult suffix in line, got: %s", lines[0])
	}
}

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
	if !strings.Contains(msg, "(-") {
		t.Errorf("expected negative SL percent for long trade, got:\n%s", msg)
	}
}

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
	if strings.Contains(msg, "1.489") {
		t.Errorf("back-computed multiplier leaked into output:\n%s", msg)
	}
}

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

func TestFormatTradeDM_TPATRMultipliers(t *testing.T) {
	cases := []struct {
		name   string
		params map[string]interface{}
		want   []string
	}{
		{
			name: "defaults",
			want: []string{"TP1: $65,000.00 (1.5x)", "TP2: $66,500.00 (3x)"},
		},
		{
			name: "fractional",
			params: map[string]interface{}{
				"tp_tiers": []interface{}{
					map[string]interface{}{"atr_multiple": 1.25, "close_fraction": 0.5},
					map[string]interface{}{"atr_multiple": 2.5, "close_fraction": 1.0},
				},
			},
			want: []string{"TP1: $64,750.00 (1.25x)", "TP2: $66,000.00 (2.5x)"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sc := StrategyConfig{
				ID:            "hl-tatr-btc",
				Platform:      "hyperliquid",
				Type:          "perps",
				CloseStrategy: &StrategyRef{Name: "tiered_tp_atr", Params: tc.params},
			}
			trade := Trade{
				Symbol: "BTC", Side: "buy", Quantity: 0.01, Price: 63500.0, Value: 635.0,
				EntryATR: 1000.0,
				Details:  "Open long 0.010000 @ $63500.00",
			}
			msg := FormatTradeDM(sc, trade, "live")
			for _, want := range tc.want {
				if !strings.Contains(msg, want) {
					t.Errorf("expected %q in DM, got:\n%s", want, msg)
				}
			}
		})
	}
}

func TestFormatTradeDM_TieredTPATRRegime(t *testing.T) {
	sc := StrategyConfig{
		ID:       "hl-reg-btc",
		Platform: "hyperliquid",
		Type:     "perps",
		CloseStrategy: &StrategyRef{
			Name:   "tiered_tp_atr_regime",
			Params: map[string]interface{}{"use_defaults": true},
		},
	}
	trade := Trade{
		Symbol:   "BTC",
		Side:     "buy",
		Quantity: 0.01,
		Price:    63500.0,
		Value:    635.0,
		EntryATR: 1000.0,
		Regime:   "trending_up",
		Details:  "Open long 0.010000 @ $63500.00",
	}
	msg := FormatTradeDM(sc, trade, "live")
	if !strings.Contains(msg, "TP1: $65,000.00") || !strings.Contains(msg, "TP2: $66,500.00") {
		t.Errorf("expected regime-resolved TP lines in DM, got:\n%s", msg)
	}
}

func TestFormatTradeDM_ExtrasOrder(t *testing.T) {
	sc := StrategyConfig{
		ID:            "hl-tatr-btc",
		Platform:      "hyperliquid",
		Type:          "perps",
		CloseStrategy: &StrategyRef{Name: "tiered_tp_atr"},
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

func TestFormatTradeDMPlain_IncludesOID(t *testing.T) {
	pf := func(v float64) *float64 { return &v }
	sc := StrategyConfig{
		ID:            "hl-tatr-eth",
		Platform:      "hyperliquid",
		Type:          "perps",
		CloseStrategy: &StrategyRef{Name: "tiered_tp_atr"},
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
	if !strings.Contains(msg, "TP1: $2,322.50 (1.5x)") {
		t.Errorf("expected TP1 with mult on Telegram DM, got:\n%s", msg)
	}
}

func TestFormatTradeDM_CloseNoATR(t *testing.T) {
	sc := StrategyConfig{
		ID:            "hl-tatr-btc",
		Platform:      "hyperliquid",
		Type:          "perps",
		CloseStrategy: &StrategyRef{Name: "tiered_tp_atr"},
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

func TestFormatTradeDM_RatchetShowsATRAndRungTargets(t *testing.T) {
	pf := func(v float64) *float64 { return &v }
	sc := StrategyConfig{
		ID:            "hl-vwap-eth-60",
		Platform:      "hyperliquid",
		Type:          "perps",
		CloseStrategy: &StrategyRef{Name: "trailing_tp_ratchet_regime", Params: map[string]interface{}{"use_defaults": true}},
		TrailingStopATRMultRegime: &RegimeATRBlock{
			UseDefaults: false,
			TrendRegime: map[string]RegimeATREntry{
				"ranging": {ATR: 2.5},
			},
		},
	}
	trade := Trade{
		Symbol:            "ETH",
		Side:              "buy",
		Quantity:          0.403,
		Price:             2479,
		Value:             999,
		EntryATR:          23.64,
		StopLossTriggerPx: 2419.90,
		StopLossATRMult:   pf(2.5),
		ExchangeOrderID:   "525653910900",
		Regime:            "ranging",
		Details:           "Open long 0.403 @ $2479",
	}
	msg := FormatTradeDM(sc, trade, "live")
	t.Logf("\n--- rendered DM ---\n%s--- end ---", msg)
	for _, want := range []string{
		"ATR: $23.64",
		"SL: $2,419.90 (-2.4%) (2.5x)",
		"Ratchet: 0/3 | Trail: 2.5x",
		"RT1: $2,496.73 (+0.7%) (0.75x -> 1x trail)",
		"RT2: $2,514.46 (+1.4%) (1.5x -> 0.75x trail)",
		"RT3: $2,526.28 (+1.9%) (2x -> 0.75x trail)",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("expected %q in DM, got:\n%s", want, msg)
		}
	}
	for _, notWant := range []string{"TP1:", "TP2:"} {
		if strings.Contains(msg, notWant) {
			t.Errorf("ratchet strategy DM should not include %s, got:\n%s", notWant, msg)
		}
	}
}

func TestFormatTradeDM_RatchetScalarTrail(t *testing.T) {
	pf := func(v float64) *float64 { return &v }
	initialTrail := 3.0
	sc := StrategyConfig{
		ID:       "hl-ratchet-btc",
		Platform: "hyperliquid",
		Type:     "perps",
		CloseStrategy: &StrategyRef{Name: "trailing_tp_ratchet", Params: map[string]interface{}{
			"tp_tiers": []interface{}{
				map[string]interface{}{"atr_multiple": 1.0, "close_fraction": 0.0, "trailing_mult_after": 2.0},
				map[string]interface{}{"atr_multiple": 2.0, "close_fraction": 0.0, "trailing_mult_after": 1.0},
			},
		}},
		TrailingStopATRMult: &initialTrail,
	}
	trade := Trade{
		Symbol:            "BTC",
		Side:              "buy",
		Quantity:          0.025,
		Price:             63500,
		Value:             1587.5,
		EntryATR:          1000,
		StopLossTriggerPx: 62000,
		StopLossATRMult:   pf(3.0),
		Regime:            "trending_up",
		Details:           "Open long 0.025 @ $63500",
	}
	msg := FormatTradeDM(sc, trade, "live")
	for _, want := range []string{
		"ATR: $1,000.00",
		"Ratchet: 0/2 | Trail: 3x",
		"RT1: $64,500.00 (+1.6%) (1x -> 2x trail)",
		"RT2: $65,500.00 (+3.1%) (2x -> 1x trail)",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("expected %q in DM, got:\n%s", want, msg)
		}
	}
}

func TestFormatTradeDM_TieredTPATRStillShowsTPAndATR(t *testing.T) {
	sc := StrategyConfig{
		ID:            "hl-tatr-btc",
		Platform:      "hyperliquid",
		Type:          "perps",
		CloseStrategy: &StrategyRef{Name: "tiered_tp_atr"},
	}
	trade := Trade{
		Symbol:   "BTC",
		Side:     "buy",
		Quantity: 0.01,
		Price:    63500,
		Value:    635,
		EntryATR: 1000,
		Details:  "Open long 0.010000 @ $63500",
	}
	msg := FormatTradeDM(sc, trade, "live")
	for _, want := range []string{"ATR: $1,000.00", "TP1: $65,000.00 (1.5x)", "TP2: $66,500.00 (3x)"} {
		if !strings.Contains(msg, want) {
			t.Errorf("expected %q in tiered_tp_atr DM, got:\n%s", want, msg)
		}
	}
	if strings.Contains(msg, "Ratchet:") || strings.Contains(msg, "RT1:") {
		t.Errorf("tiered_tp_atr DM should not include ratchet block, got:\n%s", msg)
	}
}

func TestFormatTradeDM_ATRShownWithoutTiers(t *testing.T) {
	pf := func(v float64) *float64 { return &v }
	sc := StrategyConfig{
		ID:                  "hl-sl-only-eth",
		Platform:            "hyperliquid",
		Type:                "perps",
		CloseStrategy:       &StrategyRef{Name: "trailing_stop_atr"},
		TrailingStopATRMult: pf(1.5),
	}
	trade := Trade{
		Symbol:            "ETH",
		Side:              "buy",
		Quantity:          0.5,
		Price:             2300,
		Value:             1150,
		EntryATR:          15,
		StopLossTriggerPx: 2277.5, // 2300 - 1.5*15
		StopLossATRMult:   pf(1.5),
		Details:           "Open long 0.5 @ $2300",
	}
	msg := FormatTradeDM(sc, trade, "live")
	if !strings.Contains(msg, "ATR: $15.00") {
		t.Errorf("expected ATR on trailing-stop-only DM, got:\n%s", msg)
	}
	if strings.Contains(msg, "TP1:") || strings.Contains(msg, "Ratchet:") {
		t.Errorf("trailing-stop DM should not include TP/Ratchet blocks, got:\n%s", msg)
	}
}

func TestFormatTradeDM_RatchetSuppressedOnScaleIns(t *testing.T) {
	cases := []struct {
		name    string
		price   float64
		value   float64
		stop    float64
		details string
		wantATR bool
	}{
		{
			name:    "scale_in",
			price:   2500,
			value:   250,
			stop:    2440.90,
			details: "Scale-in long 0.100000 @ $2500.00 (add #2, new qty 0.503000, avg $2485.50, fee $0.05)",
			wantATR: true,
		},
		{
			name:    "manual_scale_in",
			price:   2500,
			value:   250,
			stop:    2440.90,
			details: "manual scale-in long 0.100000 @ $2500.00 (add #2, new qty 0.503000)",
		},
		{
			name:    "manual_limit_add",
			price:   2490,
			value:   249,
			stop:    2430.90,
			details: "manual limit add long 0.100000 @ $2490.00 (cumulative VWAP $2485.50)",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			trail := 2.5
			sc := StrategyConfig{
				ID:            "hl-vwap-eth-60",
				Platform:      "hyperliquid",
				Type:          "perps",
				CloseStrategy: &StrategyRef{Name: "trailing_tp_ratchet_regime", Params: map[string]interface{}{"use_defaults": true}},
				TrailingStopATRMultRegime: &RegimeATRBlock{
					UseDefaults: false,
					TrendRegime: map[string]RegimeATREntry{"ranging": {ATR: 2.5}},
				},
			}
			trade := Trade{
				Symbol:            "ETH",
				Side:              "buy",
				Quantity:          0.1,
				Price:             tc.price,
				Value:             tc.value,
				EntryATR:          23.64,
				StopLossTriggerPx: tc.stop,
				StopLossATRMult:   &trail,
				Regime:            "ranging",
				TradeType:         scaleInTradeType,
				Details:           tc.details,
			}
			msg := FormatTradeDM(sc, trade, "live")
			if strings.Contains(msg, "Ratchet:") {
				t.Errorf("scale-in trade should not render ratchet block, got:\n%s", msg)
			}
			for _, notWant := range []string{"RT1:", "RT2:", "RT3:", "Trail:"} {
				if strings.Contains(msg, notWant) {
					t.Errorf("scale-in DM should not contain %s, got:\n%s", notWant, msg)
				}
			}
			if tc.wantATR && !strings.Contains(msg, "ATR: $23.64") {
				t.Errorf("ATR should still surface on scale-in DM, got:\n%s", msg)
			}
		})
	}
}

func TestFormatTradeDM_RatchetSuppressedOnNonDefaultATRWindow(t *testing.T) {
	pf := func(v float64) *float64 { return &v }
	sc := StrategyConfig{
		ID:              "hl-vwap-eth-60",
		Platform:        "hyperliquid",
		Type:            "perps",
		RegimeATRWindow: "daily",
		CloseStrategy:   &StrategyRef{Name: "trailing_tp_ratchet_regime", Params: map[string]interface{}{"use_defaults": true}},
		TrailingStopATRMultRegime: &RegimeATRBlock{
			UseDefaults: false,
			TrendRegime: map[string]RegimeATREntry{
				"ranging": {ATR: 2.5},
			},
		},
	}
	trade := Trade{
		Symbol:            "ETH",
		Side:              "buy",
		Quantity:          0.403,
		Price:             2479,
		Value:             999,
		EntryATR:          23.64,
		StopLossTriggerPx: 2419.90,
		StopLossATRMult:   pf(2.5),
		Regime:            "ranging",
		Details:           "Open long 0.403 @ $2479",
	}
	msg := FormatTradeDM(sc, trade, "live")
	if strings.Contains(msg, "Ratchet:") {
		t.Errorf("non-default regime_atr_window should suppress ratchet block, got:\n%s", msg)
	}
	for _, notWant := range []string{"RT1:", "RT2:", "RT3:", "Trail:"} {
		if strings.Contains(msg, notWant) {
			t.Errorf("non-default ATR window DM should not contain %s, got:\n%s", notWant, msg)
		}
	}
	if !strings.Contains(msg, "ATR: $23.64") {
		t.Errorf("ATR should still surface when ratchet suppressed, got:\n%s", msg)
	}
}

// TestStampOpenTradeFromPosition verifies the backfill helper for EntryATR and
// StopLossTriggerPx on the most-recent open trade for a symbol (#561).
func TestStampOpenTradeFromPosition(t *testing.T) {
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

	stampOpenTradeFromPosition(s, nil, "ETH", &Position{EntryATR: 999.0, StopLossTriggerPx: 99.0})
	if s.TradeHistory[0].EntryATR != 500.0 {
		t.Error("EntryATR overwritten on second call")
	}
	if s.TradeHistory[0].StopLossTriggerPx != 61000.0 {
		t.Error("StopLossTriggerPx overwritten on second call")
	}

	s2 := &StrategyState{ID: "s2", TradeHistory: []Trade{
		{Symbol: "ETH", IsClose: false},
		{Symbol: "ETH", IsClose: true},
	}}
	stampOpenTradeFromPosition(s2, nil, "ETH", &Position{EntryATR: 500.0})
	if s2.TradeHistory[0].EntryATR != 0 {
		t.Error("should not backfill when most recent trade for symbol is a close")
	}

	s3 := &StrategyState{ID: "s3", TradeHistory: []Trade{
		{Symbol: "ETH", IsClose: false},
	}}
	stampOpenTradeFromPosition(s3, nil, "ETH", nil)
	if s3.TradeHistory[0].EntryATR != 0 {
		t.Error("nil pos should be a no-op")
	}

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

func TestFormatCategorySummary_AdjustedTotalOverridesNaiveSum(t *testing.T) {
	strats := []StrategyConfig{
		{ID: "hl-btc", Type: "perps", Platform: "hyperliquid", Capital: 5000, CapitalPct: 0.5, Args: []string{"sma", "BTC", "1h"}},
		{ID: "hl-eth", Type: "perps", Platform: "hyperliquid", Capital: 5000, CapitalPct: 0.5, Args: []string{"rsi", "ETH", "1h"}},
	}
	state := &AppState{
		Strategies: map[string]*StrategyState{
			"hl-btc": {Cash: 5000, InitialCapital: 5000},
			"hl-eth": {Cash: 5000, InitialCapital: 5000},
		},
	}
	prices := map[string]float64{"BTC/USDT": 50000, "ETH/USDT": 3000}

	adjustedTotal := 8000.0

	msgs := FormatCategorySummary(1, 0, 2, 0, adjustedTotal, prices, nil, strats, state, "hyperliquid", "BTC", 600, 0, nil, nil)
	msg := strings.Join(msgs, "\n")

	var totalLine string
	for _, line := range strings.Split(msg, "\n") {
		if strings.HasPrefix(line, "TOTAL") {
			totalLine = line
			break
		}
	}
	if totalLine == "" {
		t.Fatalf("no TOTAL row found in:\n%s", msg)
	}

	if !strings.Contains(totalLine, "8,000") {
		t.Errorf("TOTAL row should show adjusted $8,000; got: %q\nfull msg:\n%s", totalLine, msg)
	}
	if !strings.Contains(totalLine, "-20.0%") {
		t.Errorf("TOTAL row PnL%% should be -20.0%% (value=8000 vs init=10000); got: %q", totalLine)
	}

	perStratRows := 0
	for _, line := range strings.Split(msg, "\n") {
		if (strings.HasPrefix(line, "hl-btc") || strings.HasPrefix(line, "hl-eth")) && strings.Contains(line, "5,000") {
			perStratRows++
		}
	}
	if perStratRows != 2 {
		t.Errorf("expected 2 per-strategy rows showing $5,000; found %d in:\n%s", perStratRows, msg)
	}
}

func TestFormatCategorySummary_NegativeAdjustedTotalFallsBackToNaiveSum(t *testing.T) {
	strats := []StrategyConfig{
		{ID: "spot-btc", Type: "spot", Capital: 3000, Args: []string{"sma", "BTC", "1h"}},
		{ID: "spot-eth", Type: "spot", Capital: 2000, Args: []string{"rsi", "ETH", "1h"}},
	}
	state := &AppState{
		Strategies: map[string]*StrategyState{
			"spot-btc": {Cash: 3000, InitialCapital: 3000},
			"spot-eth": {Cash: 2000, InitialCapital: 2000},
		},
	}
	prices := map[string]float64{}

	totalLineOf := func(msgs []string) string {
		for _, line := range strings.Split(strings.Join(msgs, "\n"), "\n") {
			if strings.HasPrefix(line, "TOTAL") {
				return line
			}
		}
		return ""
	}

	fallbackLine := totalLineOf(FormatCategorySummary(1, 0, 2, 0, -1, prices, nil, strats, state, "spot", "", 600, 0, nil, nil))
	if fallbackLine == "" {
		t.Fatal("no TOTAL row found for negative-sentinel case")
	}
	if !strings.Contains(fallbackLine, "5,000") {
		t.Errorf("TOTAL row should fall back to naive sum $5,000 when totalValue<0; got: %q", fallbackLine)
	}

	drainedLine := totalLineOf(FormatCategorySummary(1, 0, 2, 0, 0, prices, nil, strats, state, "spot", "", 600, 0, nil, nil))
	if drainedLine == "" {
		t.Fatal("no TOTAL row found for $0-adjustment case")
	}
	if !strings.Contains(drainedLine, "-100.0%") {
		t.Errorf("TOTAL row should show drained $0 (PnL%% -100.0%%), not the naive $5,000; got: %q", drainedLine)
	}
}

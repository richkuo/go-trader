package main

import (
	"encoding/json"
	"fmt"
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
	msgs := FormatCategorySummary(1, 0, 1, 0, 1000, prices, nil, strats, state, "hyperliquid", "BTC", 600)
	msg := strings.Join(msgs, "\n")
	if !strings.Contains(msg, "— BTC") {
		t.Errorf("expected '— BTC' in title, got:\n%s", msg)
	}
	if strings.Contains(msg, "ETH") {
		t.Errorf("ETH price should be filtered out for asset=BTC, got:\n%s", msg)
	}

	// Without asset — no suffix in title
	msgs2 := FormatCategorySummary(1, 0, 1, 0, 1000, prices, nil, strats, state, "hyperliquid", "", 600)
	msg2 := strings.Join(msgs2, "\n")
	if strings.Contains(msg2, "— ") {
		t.Errorf("expected no asset suffix when asset='', got:\n%s", msg2)
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

	msgs := FormatCategorySummary(1, 0, 2, 0, 2000, prices, nil, strats, state, "hyperliquid", "BTC", 600)
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

	msgs := FormatCategorySummary(1, 0, 1, 0, 1000, prices, nil, strats, state, "hyperliquid", "BTC", 600)
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
	if !strings.Contains(msg, "BUY") {
		t.Errorf("expected BUY, got:\n%s", msg)
	}
	if !strings.Contains(msg, "Mode: paper") {
		t.Errorf("expected 'Mode: paper', got:\n%s", msg)
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
	if !strings.Contains(msg, "SELL") {
		t.Errorf("expected SELL, got:\n%s", msg)
	}
	if !strings.Contains(msg, "PnL: $34.35") {
		t.Errorf("expected PnL in close trade, got:\n%s", msg)
	}
	if !strings.Contains(msg, "Mode: live") {
		t.Errorf("expected 'Mode: live', got:\n%s", msg)
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

	msgs := FormatCategorySummary(1, 0, 1, 0, 1000, prices, nil, strats, state, "hyperliquid", "BTC", 3600)
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

	msgs := FormatCategorySummary(1, 0, 1, 0, 1000, prices, nil, strats, state, "spot", "", 3600)
	msg := strings.Join(msgs, "\n")

	// No timeframe for spot → "—"; global interval 3600s → "1h". Separate columns now.
	if !strings.Contains(msg, "—") || !strings.Contains(msg, "1h") {
		t.Errorf("expected '—' and '1h' for spot with global 3600s interval, got:\n%s", msg)
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

	msgs := FormatCategorySummary(1, 0, 2, 0, 0, prices, nil, strats, state, "hyperliquid", "ETH", 600)
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
	if !strings.Contains(msg, "$ 1,085") {
		t.Errorf("expected total value ~$1,085, got:\n%s", msg)
	}
	// Individual values should be ~$542
	if !strings.Contains(msg, "$ 542") {
		t.Errorf("expected individual value ~$542, got:\n%s", msg)
	}
	// PnL should use InitialCapital ($500), not runtime Capital ($542.50)
	if !strings.Contains(msg, "$ 500") {
		t.Errorf("expected initial capital '$ 500', got:\n%s", msg)
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

	msgs := FormatCategorySummary(1, 0, 2, 0, 0, prices, nil, strats, state, "hyperliquid", "ETH", 600)
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

	msgs := FormatCategorySummary(1, 0, 2, 0, 0, prices, nil, strats, state, "hyperliquid", "ETH", 600)
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

	msgs := FormatCategorySummary(1, 0, 20, 0, 10000, prices, nil, strats, state, "hyperliquid", "BTC", 600)

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

	msgs := FormatCategorySummary(1, 0, 1, 0, 1000, prices, nil, strats, state, "hyperliquid", "BTC", 600)

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

	lines := collectPositions("hl-rsi-btc", ss, prices)
	if len(lines) != 1 {
		t.Fatalf("expected 1 line, got %d", len(lines))
	}
	if !strings.Contains(lines[0], "[Mar 15 10:30]") {
		t.Errorf("expected timestamp '[Mar 15 10:30]', got: %s", lines[0])
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

	lines := collectPositions("hl-rsi-btc", ss, prices)
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

	lines := collectPositions("deribit-wheel-btc", ss, prices)
	if len(lines) != 1 {
		t.Fatalf("expected 1 line, got %d", len(lines))
	}
	if !strings.Contains(lines[0], "[Apr 01 08:00]") {
		t.Errorf("expected option timestamp '[Apr 01 08:00]', got: %s", lines[0])
	}
}

func TestSplitCategorySummary_SingleMessage(t *testing.T) {
	header := "Header line\n"
	posLines := []string{"pos1", "pos2"}
	tradeLines := []string{"• trade1"}

	msgs := splitCategorySummary(header, 2, posLines, tradeLines)
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

func TestSplitCategorySummary_MultiMessage(t *testing.T) {
	// Create a header that uses ~1900 chars, leaving very little room for positions.
	header := strings.Repeat("x", 1900) + "\n"
	posLines := []string{"position-line-1-aaaa", "position-line-2-bbbb", "position-line-3-cccc"}

	msgs := splitCategorySummary(header, 3, posLines, nil)
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

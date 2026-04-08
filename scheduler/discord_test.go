package main

import (
	"encoding/json"
	"strings"
	"testing"
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
	msg := FormatCategorySummary(1, 0, 1, 0, 1000, prices, nil, strats, state, "hyperliquid", "BTC")
	if !strings.Contains(msg, "— BTC") {
		t.Errorf("expected '— BTC' in title, got:\n%s", msg)
	}
	if strings.Contains(msg, "ETH") {
		t.Errorf("ETH price should be filtered out for asset=BTC, got:\n%s", msg)
	}

	// Without asset — no suffix in title
	msg2 := FormatCategorySummary(1, 0, 1, 0, 1000, prices, nil, strats, state, "hyperliquid", "")
	if strings.Contains(msg2, "— ") {
		t.Errorf("expected no asset suffix when asset='', got:\n%s", msg2)
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

	msg := FormatCategorySummary(1, 0, 2, 0, 0, prices, nil, strats, state, "hyperliquid", "ETH")

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

	msg := FormatCategorySummary(1, 0, 2, 0, 0, prices, nil, strats, state, "hyperliquid", "ETH")

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

	msg := FormatCategorySummary(1, 0, 2, 0, 0, prices, nil, strats, state, "hyperliquid", "ETH")

	if strings.Contains(msg, "Wallet%") {
		t.Errorf("should not show Wallet%% column without shared wallet, got:\n%s", msg)
	}
	// Should still show Init column even without shared wallet
	if !strings.Contains(msg, "Init") {
		t.Errorf("expected 'Init' column header, got:\n%s", msg)
	}
}

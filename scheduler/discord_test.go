package main

import (
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

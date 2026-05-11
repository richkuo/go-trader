package main

import (
	"strings"
	"testing"
)

func TestTradeAlertCloseSourceClassification(t *testing.T) {
	cases := []struct {
		details string
		want    string
	}{
		{"Stop loss close ETH, PnL: $-22.45 (fee $1.10)", "exchange SL"},
		{"TP1 fill close, PnL: $34.35 (fee $1.23)", "exchange TP1"},
		{"TP2 fill close, PnL: $50.00 (fee $1.50)", "exchange TP2"},
		{"Circuit breaker on-chain close (no virtual position), fill=0.5 fee=$0.20", "circuit breaker"},
		{"External close @ mark $3077, PnL: $0.00 (fee $0.00)", "external (peer / manual UI)"},
		{"External partial close @ mark $3077", "external (peer / manual UI, partial)"},
		{"Close long, PnL: $34.35 (fee $1.23)", "close-strategy exit"},
		{"Close short, PnL: $12.50 (fee $1.23)", "close-strategy exit"},
		{"Partial-close long ETH, PnL: $12.34 (fee $0.05)", ""}, // partial-close from signal: not a trigger, no annotation
		{"", ""},
	}
	for _, c := range cases {
		got := tradeAlertCloseSource(c.details)
		if got != c.want {
			t.Errorf("details=%q → %q, want %q", c.details, got, c.want)
		}
	}
}

func TestFormatTradeDMCloseTradeIncludesSource(t *testing.T) {
	sc := StrategyConfig{ID: "hl-rmc-eth-live", Platform: "hyperliquid", Type: "perps"}
	trade := Trade{
		Symbol:   "ETH",
		Side:     "sell",
		Quantity: 0.47,
		Price:    3077.70,
		Value:    1446.52,
		Details:  "Stop loss close ETH, PnL: $-22.45 (fee $1.10)",
	}
	msg := FormatTradeDM(sc, trade, "live")
	if !strings.Contains(msg, "Source: exchange SL") {
		t.Errorf("expected Source line for SL close, got:\n%s", msg)
	}
}

func TestFormatTradeDMOpenTradeOmitsSource(t *testing.T) {
	sc := StrategyConfig{ID: "hl-rmc-eth-live", Platform: "hyperliquid", Type: "perps"}
	trade := Trade{
		Symbol:   "ETH",
		Side:     "buy",
		Quantity: 0.47,
		Price:    3077.70,
		Value:    1446.52,
		Details:  "Open long 0.47 @ $3077.70 (5x, fee $1.10)",
	}
	msg := FormatTradeDM(sc, trade, "live")
	if strings.Contains(msg, "Source:") {
		t.Errorf("open trade should not carry a Source line, got:\n%s", msg)
	}
}

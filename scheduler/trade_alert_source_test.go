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
		{"Paper trailing SL close ETH, PnL: $-22.45 (fee $1.10)", "paper trailing SL"},
		{"Trailing SL close ETH, PnL: $-22.45 (fee $1.10)", "trailing SL"},
		{"Paper SL close ETH, PnL: $-22.45 (fee $1.10)", "paper SL"},
		{"Liquidation-clamp SL close ETH, PnL: $-22.45 (fee $1.10)", "liquidation-clamp SL"},
		{"TP1 fill close, PnL: $34.35 (fee $1.23)", "exchange TP1"},
		{"TP2 fill close, PnL: $50.00 (fee $1.50)", "exchange TP2"},
		{"Circuit breaker on-chain close (no virtual position), fill=0.5 fee=$0.20", "circuit breaker"},
		{"External close @ mark $3077, PnL: $0.00 (fee $0.00)", "external (peer / manual UI)"},
		{"External partial close @ mark $3077", "external (peer / manual UI, partial)"},
		{"Close long, PnL: $34.35 (fee $1.23)", "close-strategy exit"},
		{"Close short, PnL: $12.50 (fee $1.23)", "close-strategy exit"},
		{"Partial-close long ETH, PnL: $12.34 (fee $0.05)", ""},
		{"", ""},
	}
	for _, c := range cases {
		got := tradeAlertCloseSource(c.details)
		if got != c.want {
			t.Errorf("details=%q → %q, want %q", c.details, got, c.want)
		}
	}
}

func TestFormatTradeDMSourceLine(t *testing.T) {
	cases := []struct {
		name         string
		sc           StrategyConfig
		trade        Trade
		wantContains []string
		wantOmits    []string
	}{
		{
			name: "close trade carries source",
			sc:   StrategyConfig{ID: "hl-rmc-eth-live", Platform: "hyperliquid", Type: "perps"},
			trade: Trade{
				Symbol:   "ETH",
				Side:     "sell",
				Quantity: 0.47,
				Price:    3077.70,
				Value:    1446.52,
				Details:  "Stop loss close ETH, PnL: $-22.45 (fee $1.10)",
			},
			wantContains: []string{"Source: exchange SL"},
		},
		{
			name: "hyperliquid reconcile SL carries OID and source",
			sc:   StrategyConfig{ID: "hl-sync-eth", Platform: "hyperliquid", Type: "perps"},
			trade: Trade{
				IsClose:         true,
				Symbol:          "ETH",
				Side:            "sell",
				Quantity:        1,
				Price:           2900,
				Value:           2900,
				Details:         "Stop loss close, PnL: $-100.05 (fee $0.05)",
				ExchangeOrderID: "42",
			},
			wantContains: []string{"TRADE CLOSED - LIVE", "OID: 42", "Source: exchange SL"},
		},
		{
			name: "open trade omits source",
			sc:   StrategyConfig{ID: "hl-rmc-eth-live", Platform: "hyperliquid", Type: "perps"},
			trade: Trade{
				Symbol:   "ETH",
				Side:     "buy",
				Quantity: 0.47,
				Price:    3077.70,
				Value:    1446.52,
				Details:  "Open long 0.47 @ $3077.70 (5x, fee $1.10)",
			},
			wantOmits: []string{"Source:"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			msg := FormatTradeDM(tc.sc, tc.trade, "live")
			for _, want := range tc.wantContains {
				if !strings.Contains(msg, want) {
					t.Errorf("message missing %q:\n%s", want, msg)
				}
			}
			for _, omit := range tc.wantOmits {
				if strings.Contains(msg, omit) {
					t.Errorf("message should not contain %q:\n%s", omit, msg)
				}
			}
		})
	}
}

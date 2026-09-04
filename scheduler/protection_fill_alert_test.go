package main

import (
	"strings"
	"testing"
)

func TestFormatProtectionFillAlert_BranchMarkers(t *testing.T) {
	cases := []struct {
		name    string
		alert   ProtectionFillAlert
		want    []string
		notWant []string
	}{
		{
			name: "full SL with pnl",
			alert: ProtectionFillAlert{
				StrategyID: "hl-tema-eth-live", Symbol: "ETH", Side: "long", FillType: "SL",
				FillPrice: 1800.50, CloseQty: 0.42, RealizedPnL: -42.10, HasPnL: true,
			},
			want:    []string{"PnL="},
			notWant: []string{"(partial)", "(oid=", "(fill price unknown)"},
		},
		{
			name: "partial TP short",
			alert: ProtectionFillAlert{
				StrategyID: "hl-bear-btc-live", Symbol: "BTC", Side: "short", FillType: "TP2",
				IsPartial: true, FillPrice: 65000, CloseQty: 0.005, RemainingQty: 0.005,
				RealizedPnL: 12.34, HasPnL: true,
			},
			want:    []string{"(partial)", "PnL="},
			notWant: []string{"(oid="},
		},
		{
			name: "no pnl",
			alert: ProtectionFillAlert{
				StrategyID: "hl-x", Symbol: "BTC", Side: "long", FillType: "SL",
				FillPrice: 50000, CloseQty: 0.1,
			},
			notWant: []string{"PnL=", "(oid=", "(fill price unknown)"},
		},
		{
			name: "with oid",
			alert: ProtectionFillAlert{
				StrategyID: "manual-eth", Symbol: "ETH", Side: "long", FillType: "SL",
				FillPrice: 2301, CloseQty: 0.429, RealizedPnL: -12.31, HasPnL: true,
				ExchangeOrderID: "420267328218",
			},
			want:    []string{"(oid=420267328218)"},
			notWant: []string{"(partial)"},
		},
		{
			name: "with oid partial",
			alert: ProtectionFillAlert{
				StrategyID: "hl-bear-btc-live", Symbol: "BTC", Side: "short", FillType: "TP1",
				IsPartial: true, FillPrice: 65000, CloseQty: 0.005, RemainingQty: 0.005,
				ExchangeOrderID: "999000111",
			},
			want: []string{"(oid=999000111)", "(partial)"},
		},
		{
			name: "unknown fill price",
			alert: ProtectionFillAlert{
				StrategyID: "hl-x", Symbol: "BTC", Side: "long", FillType: "SL",
				FillPrice: 0, CloseQty: 0.1,
			},
			want: []string{"(fill price unknown)"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out := formatProtectionFillAlert(tc.alert)
			for _, want := range tc.want {
				if !strings.Contains(out, want) {
					t.Errorf("missing %q in:\n%s", want, out)
				}
			}
			for _, bad := range tc.notWant {
				if strings.Contains(out, bad) {
					t.Errorf("unexpected %q in:\n%s", bad, out)
				}
			}
		})
	}
}

type countingDMSender struct {
	count int
	last  string
}

func (c *countingDMSender) SendOwnerDM(s string) {
	c.count++
	c.last = s
}

func TestNotifyProtectionFill_Gating(t *testing.T) {
	alert := ProtectionFillAlert{
		StrategyID: "hl-x", Symbol: "BTC", Side: "long", FillType: "SL", FillPrice: 100, CloseQty: 0.1,
	}

	notifyProtectionFill(nil, true, alert)
	var mn *MultiNotifier
	notifyProtectionFill(mn, true, alert)

	c := &countingDMSender{}
	notifyProtectionFill(c, false, alert)
	if c.count != 0 {
		t.Fatalf("disabled flag must suppress; got %d invocations", c.count)
	}
	notifyProtectionFill(c, true, alert)
	if c.count != 1 {
		t.Fatalf("enabled must emit once; got %d", c.count)
	}
}

func TestLastBookedTradePnL(t *testing.T) {
	if got := lastBookedTradePnL(nil); got != 0 {
		t.Errorf("nil state: got %v, want 0", got)
	}
	s := &StrategyState{}
	if got := lastBookedTradePnL(s); got != 0 {
		t.Errorf("empty history: got %v, want 0", got)
	}
	s.TradeHistory = []Trade{
		{RealizedPnL: 1.5},
		{RealizedPnL: -2.5},
	}
	if got := lastBookedTradePnL(s); got != -2.5 {
		t.Errorf("last trade pnl: got %v, want -2.5", got)
	}
}

func TestNotifyTPSLFillsEnabled_DefaultsToTrue(t *testing.T) {
	var c *Config
	if !c.NotifyTPSLFillsEnabled() {
		t.Error("nil config must default to enabled")
	}
	c = &Config{}
	if !c.NotifyTPSLFillsEnabled() {
		t.Error("nil pointer field must default to enabled")
	}
	f := false
	c.NotifyTPSLFills = &f
	if c.NotifyTPSLFillsEnabled() {
		t.Error("explicit false must disable")
	}
	tr := true
	c.NotifyTPSLFills = &tr
	if !c.NotifyTPSLFillsEnabled() {
		t.Error("explicit true must enable")
	}
}

func TestHyperliquidClearedTPTier_TierIndex(t *testing.T) {
	sc := tieredTPATRSC()
	pos := &Position{Quantity: 0.422, TPOIDs: []int64{0, 222}}
	if idx, ok := hyperliquidClearedTPTier(sc, pos, 0.211); !ok || idx != 0 {
		t.Errorf("tier 0 cleared: idx=%d ok=%v, want 0,true", idx, ok)
	}
	pos = &Position{Quantity: 0.422, TPOIDs: []int64{111, 0}}
	if idx, ok := hyperliquidClearedTPTier(sc, pos, 0.211); !ok || idx != 1 {
		t.Errorf("tier 1 cleared: idx=%d ok=%v, want 1,true", idx, ok)
	}
	pos = &Position{Quantity: 0.422, TPOIDs: []int64{0, 0}}
	if idx, ok := hyperliquidClearedTPTier(sc, pos, 0.422); !ok || idx != 1 {
		t.Errorf("final tier: idx=%d ok=%v, want 1,true", idx, ok)
	}
	pos = &Position{Quantity: 0.422, TPOIDs: []int64{0, 0}, TPArmedTiers: []bool{false, false}}
	if _, ok := hyperliquidClearedTPTier(sc, pos, 0.1); ok {
		t.Error("ambiguous all-zero with mismatched qty must not attribute when tiers never armed")
	}
	pos = &Position{Quantity: 0.422, TPOIDs: []int64{0, 0}, TPArmedTiers: []bool{true, true}}
	if !hyperliquidAllTiersArmedAndCleared(sc, pos) {
		t.Error("expected all tiers armed and cleared for dust path")
	}
	if _, ok := hyperliquidClearedTPTier(sc, pos, 0.211); ok {
		t.Error("hyperliquidClearedTPTier must not attribute dust drift directly")
	}
}

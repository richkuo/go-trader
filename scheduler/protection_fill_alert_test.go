package main

import (
	"strings"
	"testing"
)

func TestFormatProtectionFillAlert_FullSL(t *testing.T) {
	out := formatProtectionFillAlert(ProtectionFillAlert{
		StrategyID:   "hl-tema-eth-live",
		Symbol:       "ETH",
		Side:         "long",
		FillType:     "SL",
		IsPartial:    false,
		FillPrice:    1800.50,
		CloseQty:     0.42,
		RemainingQty: 0,
		RealizedPnL:  -42.10,
		HasPnL:       true,
	})
	for _, want := range []string{
		"SL filled — hl-tema-eth-live",
		"ETH LONG",
		"@ $1800.5000",
		"Remaining: 0.000000 ETH",
		"PnL=-$42.10",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in alert body:\n%s", want, out)
		}
	}
	if strings.Contains(out, "(partial)") {
		t.Errorf("full-close alert should not contain '(partial)':\n%s", out)
	}
}

func TestFormatProtectionFillAlert_PartialTPShort(t *testing.T) {
	out := formatProtectionFillAlert(ProtectionFillAlert{
		StrategyID:   "hl-bear-btc-live",
		Symbol:       "BTC",
		Side:         "short",
		FillType:     "TP2",
		IsPartial:    true,
		FillPrice:    65000.0,
		CloseQty:     0.005,
		RemainingQty: 0.005,
		RealizedPnL:  12.34,
		HasPnL:       true,
	})
	for _, want := range []string{
		"TP2 filled — hl-bear-btc-live (partial)",
		"BTC SHORT",
		"PnL=$12.34",
		"Remaining: 0.005000 BTC",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in alert body:\n%s", want, out)
		}
	}
}

func TestFormatProtectionFillAlert_NoPnL(t *testing.T) {
	out := formatProtectionFillAlert(ProtectionFillAlert{
		StrategyID: "hl-x", Symbol: "BTC", Side: "long", FillType: "SL",
		FillPrice: 50000, CloseQty: 0.1, HasPnL: false,
	})
	if strings.Contains(out, "PnL=") {
		t.Errorf("expected no PnL line when HasPnL=false:\n%s", out)
	}
}

func TestFormatProtectionFillAlert_UnknownPrice(t *testing.T) {
	out := formatProtectionFillAlert(ProtectionFillAlert{
		StrategyID: "hl-x", Symbol: "BTC", Side: "long", FillType: "SL",
		FillPrice: 0, CloseQty: 0.1,
	})
	if !strings.Contains(out, "(fill price unknown)") {
		t.Errorf("expected unknown-price marker:\n%s", out)
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

func TestNotifyProtectionFill_NilSenderIsNoop(t *testing.T) {
	// Untyped nil interface — must not panic.
	notifyProtectionFill(nil, true, ProtectionFillAlert{StrategyID: "x"})
	// Typed nil *MultiNotifier wrapped in non-nil interface — must not panic.
	var mn *MultiNotifier
	notifyProtectionFill(mn, true, ProtectionFillAlert{StrategyID: "x"})
}

func TestNotifyProtectionFill_DisabledFlagSuppresses(t *testing.T) {
	c := &countingDMSender{}
	notifyProtectionFill(c, false, ProtectionFillAlert{
		StrategyID: "x", Symbol: "BTC", Side: "long", FillType: "SL", FillPrice: 100, CloseQty: 0.1,
	})
	if c.count != 0 {
		t.Errorf("disabled flag must suppress; got %d invocations", c.count)
	}
}

func TestNotifyProtectionFill_EnabledEmits(t *testing.T) {
	c := &countingDMSender{}
	notifyProtectionFill(c, true, ProtectionFillAlert{
		StrategyID: "hl-x", Symbol: "BTC", Side: "long", FillType: "SL", FillPrice: 100, CloseQty: 0.1,
	})
	if c.count != 1 {
		t.Fatalf("enabled must emit once; got %d", c.count)
	}
	if !strings.Contains(c.last, "SL filled — hl-x") {
		t.Errorf("body missing headline: %s", c.last)
	}
}

func TestTPTierLabel(t *testing.T) {
	cases := map[int]string{0: "TP1", 1: "TP2", 4: "TP5"}
	for in, want := range cases {
		if got := tpTierLabel(in); got != want {
			t.Errorf("tpTierLabel(%d) = %q, want %q", in, got, want)
		}
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
	// Tier 0 cleared, tier 1 active → idx=0
	pos := &Position{Quantity: 0.422, TPOIDs: []int64{0, 222}}
	if idx, ok := hyperliquidClearedTPTier(sc, pos, 0.211); !ok || idx != 0 {
		t.Errorf("tier 0 cleared: idx=%d ok=%v, want 0,true", idx, ok)
	}
	// Tier 0 active, tier 1 cleared → idx=1
	pos = &Position{Quantity: 0.422, TPOIDs: []int64{111, 0}}
	if idx, ok := hyperliquidClearedTPTier(sc, pos, 0.211); !ok || idx != 1 {
		t.Errorf("tier 1 cleared: idx=%d ok=%v, want 1,true", idx, ok)
	}
	// All tiers zero, full-close qty match → final tier (idx=1)
	pos = &Position{Quantity: 0.422, TPOIDs: []int64{0, 0}}
	if idx, ok := hyperliquidClearedTPTier(sc, pos, 0.422); !ok || idx != 1 {
		t.Errorf("final tier: idx=%d ok=%v, want 1,true", idx, ok)
	}
	// All zero but qty mismatch → not attributable
	if _, ok := hyperliquidClearedTPTier(sc, pos, 0.1); ok {
		t.Error("ambiguous all-zero with mismatched qty must not attribute")
	}
}

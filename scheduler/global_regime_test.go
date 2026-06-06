package main

import (
	"testing"
)

func TestProjectADXLabel(t *testing.T) {
	raw := RegimeBundleRaw{ADX: 30, PlusDI: 25, MinusDI: 10}
	if got := projectADXLabel(raw, 20); got != "trending_up" {
		t.Fatalf("projectADXLabel trending_up = %q", got)
	}
	raw = RegimeBundleRaw{ADX: 10, PlusDI: 25, MinusDI: 10}
	if got := projectADXLabel(raw, 20); got != "ranging" {
		t.Fatalf("projectADXLabel ranging = %q", got)
	}
}

// TestProjectADXLabelParityVectors mirrors shared_tools/regime.py map_adx_label cases.
func TestProjectADXLabelParityVectors(t *testing.T) {
	cases := []struct {
		raw  RegimeBundleRaw
		th   float64
		want string
	}{
		{RegimeBundleRaw{ADX: 30, PlusDI: 25, MinusDI: 10}, 20, "trending_up"},
		{RegimeBundleRaw{ADX: 30, PlusDI: 5, MinusDI: 20}, 20, "trending_down"},
		{RegimeBundleRaw{ADX: 10, PlusDI: 25, MinusDI: 10}, 20, "ranging"},
		{RegimeBundleRaw{ADX: 30, PlusDI: 10, MinusDI: 10}, 20, "ranging"},
	}
	for _, tc := range cases {
		if got := projectADXLabel(tc.raw, tc.th); got != tc.want {
			t.Fatalf("projectADXLabel(%+v, %g) = %q, want %q", tc.raw, tc.th, got, tc.want)
		}
	}
}

func TestProjectCompositeLabel(t *testing.T) {
	th := defaultCompositeThresholds
	raw := RegimeBundleRaw{
		ReturnEff:    0.10,
		CompositeADX: 30,
		RangeEff:     0.01,
		Efficiency:   0.8,
	}
	if got := projectCompositeLabel(raw, th); got != "trending_up_clean" {
		t.Fatalf("projectCompositeLabel = %q, want trending_up_clean", got)
	}
}

func TestGlobalRegimeStorePayloadLegacy(t *testing.T) {
	store := newGlobalRegimeStore()
	rk := regimeRawKey{Platform: "hyperliquid", Type: "perps", Symbol: "BTC", Interval: "1h", Period: 14}
	lk := regimeLabelKey{
		Raw:          rk,
		Classifier:   regimeClassifierADX,
		Period:       14,
		ADXThreshold: 20,
	}
	store.raw[rk] = &regimeBundleEntry{
		Raw: RegimeBundleRaw{ADX: 30, PlusDI: 20, MinusDI: 5},
	}
	store.labels[lk] = "trending_up"

	rc := &RegimeConfig{Enabled: true, Period: 14, ADXThreshold: 20}
	sc := StrategyConfig{
		ID:       "hl-btc",
		Type:     "perps",
		Platform: "hyperliquid",
		Args:     []string{"momentum", "BTC", "1h"},
	}
	p := store.PayloadForStrategy(sc, rc)
	if p.Legacy != "trending_up" {
		t.Fatalf("Legacy = %q, want trending_up", p.Legacy)
	}
}

func TestMarketRegimeEntriesSorted(t *testing.T) {
	store := newGlobalRegimeStore()
	rk := regimeRawKey{Platform: "hyperliquid", Type: "perps", Symbol: "ETH", Interval: "1h", Period: 14}
	lk := regimeLabelKey{Raw: rk, Classifier: regimeClassifierADX, Period: 14, ADXThreshold: 20}
	store.labels[lk] = "ranging"
	rk2 := regimeRawKey{Platform: "hyperliquid", Type: "perps", Symbol: "BTC", Interval: "1h", Period: 14}
	lk2 := regimeLabelKey{Raw: rk2, Classifier: regimeClassifierADX, Period: 14, ADXThreshold: 20}
	store.labels[lk2] = "trending_up"

	entries := store.MarketRegimeEntries()
	if len(entries) != 2 {
		t.Fatalf("len = %d, want 2", len(entries))
	}
	if entries[0].Symbol != "BTC" || entries[0].Label != "trending_up" {
		t.Fatalf("first entry = %#v, want BTC trending_up", entries[0])
	}
}

func TestCollectRegimeCycleNeedsOptions(t *testing.T) {
	cfg := &Config{
		Regime: &RegimeConfig{Enabled: true, Period: 14},
	}
	sc := StrategyConfig{
		ID:       "opt-btc",
		Type:     "options",
		Platform: "deribit",
		Args:     []string{"short_put", "BTC"},
	}
	raw, labels := collectRegimeCycleNeeds([]StrategyConfig{sc}, cfg)
	if len(raw) != 1 || raw[0].Interval != optionsRegimeInterval || raw[0].Period != optionsRegimePeriod {
		t.Fatalf("raw keys = %#v", raw)
	}
	if len(labels) != 1 {
		t.Fatalf("label keys = %#v", labels)
	}
}

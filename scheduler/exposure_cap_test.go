package main

import (
	"strings"
	"testing"
)

func TestExposureCapBlocksSignal(t *testing.T) {
	bothCapped := ExposureCapStatus{Configured: true, CapUSD: 100, LongUSD: 500, ShortUSD: 500, LongBlocked: true, ShortBlocked: true}
	longCapped := ExposureCapStatus{Configured: true, CapUSD: 100, LongUSD: 500, LongBlocked: true}
	shortCapped := ExposureCapStatus{Configured: true, CapUSD: 100, ShortUSD: 500, ShortBlocked: true}
	longBook := ExposureCapStatus{Configured: true, CapUSD: 15000, LongUSD: 19000, LongBlocked: true}
	cases := []struct {
		name          string
		st            ExposureCapStatus
		signal        int
		closeFraction float64
		posQty        float64
		posSide       string
		allowsLong    bool
		allowsShort   bool
		wantBlocked   bool
		wantReason    []string
	}{
		{"manage_only_signal0_passes", bothCapped, 0, 0, 1, "long", true, true, false, nil},
		{"close_action_passes", bothCapped, -1, 1.0, 1, "long", true, true, false, nil},
		{"pure_close_sell_on_long_passes", bothCapped, -1, 0, 1, "long", true, false, false, nil},
		{"pure_close_buy_on_short_passes", bothCapped, 1, 0, 1, "short", false, true, false, nil},
		{"scale_in_add_on_long_blocked_while_longs_capped", longCapped, 1, 0, 1, "long", true, true, true, nil},
		{"long_to_short_flip_passes_while_only_longs_capped", longCapped, -1, 0, 1, "long", true, true, false, nil},
		{"long_to_short_flip_held_while_shorts_capped", shortCapped, -1, 0, 1, "long", true, true, true, nil},
		{"fresh_short_open_blocked_while_shorts_capped", shortCapped, -1, 0, 0, "", true, true, true, nil},
		{"fresh_short_open_passes_while_only_longs_capped", longCapped, -1, 0, 0, "", true, true, false, nil},
		{"fresh_long_open_blocked_with_amounts_in_reason", longBook, 1, 0, 0, "", true, true, true, []string{"new long opens blocked", "$19000.00", "$15000.00"}},
		{"fresh_short_open_passes_on_long_capped_book", longBook, -1, 0, 0, "", true, true, false, nil},
		{"disabled_cap_never_blocks", ExposureCapStatus{}, 1, 0, 0, "", true, true, false, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			blocked, why := exposureCapBlocksSignal(tc.st, "BTC", tc.signal, tc.closeFraction, tc.posQty, tc.posSide, tc.allowsLong, tc.allowsShort)
			if blocked != tc.wantBlocked {
				t.Fatalf("blocked = %v, want %v (why=%q)", blocked, tc.wantBlocked, why)
			}
			for _, want := range tc.wantReason {
				if !strings.Contains(why, want) {
					t.Errorf("reason %q missing %q", why, want)
				}
			}
		})
	}
}

func exposureTestStates() map[string]*StrategyState {
	return map[string]*StrategyState{
		"hl-a-btc": {
			ID:   "hl-a-btc",
			Type: "perps",
			Positions: map[string]*Position{
				"BTC": {Symbol: "BTC", Quantity: 0.2, Side: "long", AvgCost: 48000},
			},
			OptionPositions: make(map[string]*OptionPosition),
		},
		"hl-b-eth": {
			ID:   "hl-b-eth",
			Type: "perps",
			Positions: map[string]*Position{
				"ETH": {Symbol: "ETH", Quantity: 2, Side: "long", AvgCost: 2900},
			},
			OptionPositions: make(map[string]*OptionPosition),
		},
		"hl-c-sol": {
			ID:   "hl-c-sol",
			Type: "perps",
			Positions: map[string]*Position{
				"SOL": {Symbol: "SOL", Quantity: 20, Side: "long", AvgCost: 140},
			},
			OptionPositions: make(map[string]*OptionPosition),
		},
	}
}

func exposureTestConfigs() []StrategyConfig {
	return []StrategyConfig{
		{ID: "hl-a-btc", Type: "perps", Platform: "hyperliquid", Args: []string{"momentum", "BTC", "1h"}},
		{ID: "hl-b-eth", Type: "perps", Platform: "hyperliquid", Args: []string{"momentum", "ETH", "1h"}},
		{ID: "hl-c-sol", Type: "perps", Platform: "hyperliquid", Args: []string{"momentum", "SOL", "1h"}},
	}
}

func exposureTestPrices() map[string]float64 {
	return map[string]float64{"BTC": 50000, "ETH": 3000, "SOL": 150}
}

func perpsPosState(id, coin string, qty float64, side string, avgCost float64) *StrategyState {
	return &StrategyState{
		ID: id, Type: "perps",
		Positions:       map[string]*Position{coin: {Symbol: coin, Quantity: qty, Side: side, AvgCost: avgCost}},
		OptionPositions: make(map[string]*OptionPosition),
	}
}

func perpsCfg(id, strategy, coin string) StrategyConfig {
	return StrategyConfig{ID: id, Type: "perps", Platform: "hyperliquid", Args: []string{strategy, coin, "1h"}}
}

func TestEvaluateExposureCap_Buckets(t *testing.T) {
	bucketCap := &PortfolioRiskConfig{MaxDrawdownPct: 25, MaxSameDirectionNotionalUSD: 15000}
	cases := []struct {
		name               string
		pr                 *PortfolioRiskConfig
		states             map[string]*StrategyState
		cfgs               []StrategyConfig
		prices             map[string]float64
		pv                 float64
		wantConfigured     bool
		wantLong           float64
		wantShort          float64
		wantLongBlocked    bool
		wantShortBlocked   bool
		wantSkipped        []string
		wantSkippedWarning string
	}{
		{name: "all_long_book_blocks_longs_only",
			pr: bucketCap, states: exposureTestStates(), cfgs: exposureTestConfigs(), prices: exposureTestPrices(), pv: 20000,
			wantConfigured: true, wantLong: 19000, wantShort: 0, wantLongBlocked: true},
		{name: "netting_per_asset",
			pr: bucketCap,
			states: map[string]*StrategyState{
				"hl-a-btc": perpsPosState("hl-a-btc", "BTC", 0.2, "long", 48000),
				"hl-b-eth": perpsPosState("hl-b-eth", "ETH", 2, "short", 2900),
			},
			cfgs:   []StrategyConfig{perpsCfg("hl-a-btc", "momentum", "BTC"), perpsCfg("hl-b-eth", "momentum", "ETH")},
			prices: map[string]float64{"BTC": 50000, "ETH": 5000}, pv: 20000,
			wantConfigured: true, wantLong: 10000, wantShort: 10000},
		{name: "same_asset_nets_before_bucketing",
			pr: bucketCap,
			states: map[string]*StrategyState{
				"hl-a-btc": perpsPosState("hl-a-btc", "BTC", 0.3, "long", 48000),
				"hl-b-btc": perpsPosState("hl-b-btc", "BTC", 0.1, "short", 48000),
			},
			cfgs:   []StrategyConfig{perpsCfg("hl-a-btc", "momentum", "BTC"), perpsCfg("hl-b-btc", "triple_ema", "BTC")},
			prices: map[string]float64{"BTC": 50000}, pv: 20000,
			wantConfigured: true, wantLong: 10000, wantShort: 0},
		{name: "disabled_by_default_zero_thresholds",
			pr: &PortfolioRiskConfig{MaxDrawdownPct: 25}, states: exposureTestStates(), cfgs: exposureTestConfigs(), prices: exposureTestPrices(), pv: 20000,
			wantConfigured: false},
		{name: "disabled_nil_config",
			pr: nil, states: exposureTestStates(), cfgs: exposureTestConfigs(), prices: exposureTestPrices(), pv: 20000,
			wantConfigured: false},
		{name: "fail_safe_exclusions_do_not_inflate_sum",
			pr: bucketCap,
			states: map[string]*StrategyState{
				"hl-a-btc": perpsPosState("hl-a-btc", "BTC", 0.2, "long", 48000),
				"hl-b-xyz": perpsPosState("hl-b-xyz", "XYZ", 5, "long", 0),
				"hl-c-eth": perpsPosState("hl-c-eth", "ETH", -1, "long", 3000),
			},
			cfgs:   []StrategyConfig{perpsCfg("hl-a-btc", "momentum", "BTC"), perpsCfg("hl-b-xyz", "momentum", "XYZ"), perpsCfg("hl-c-eth", "momentum", "ETH")},
			prices: map[string]float64{"BTC": 50000, "ETH": 3000}, pv: 20000,
			wantConfigured: true, wantLong: 10000, wantShort: 0,
			wantSkipped:        []string{"hl-b-xyz/XYZ: no usable price", "hl-c-eth/ETH: non-positive quantity"},
			wantSkippedWarning: "2 position(s) excluded"},
		{name: "avg_cost_fallback_without_prices",
			pr: bucketCap, states: exposureTestStates(), cfgs: exposureTestConfigs(), prices: nil, pv: 0,
			wantConfigured: true, wantLong: 18200, wantShort: 0, wantLongBlocked: true},
		{name: "manual_positions_counted",
			pr:     &PortfolioRiskConfig{MaxDrawdownPct: 25, MaxSameDirectionNotionalUSD: 10000},
			states: map[string]*StrategyState{"hl-manual": {ID: "hl-manual", Type: "manual", Positions: map[string]*Position{"ETH": {Symbol: "ETH", Quantity: 4, Side: "long", AvgCost: 2900}}, OptionPositions: make(map[string]*OptionPosition)}},
			cfgs:   []StrategyConfig{{ID: "hl-manual", Type: "manual", Platform: "hyperliquid", Args: []string{"hold", "ETH"}}},
			prices: map[string]float64{"ETH": 3000}, pv: 20000,
			wantConfigured: true, wantLong: 12000, wantShort: 0, wantLongBlocked: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			st := evaluateExposureCap(tc.pr, tc.states, tc.cfgs, tc.prices, tc.pv)
			if st.Configured != tc.wantConfigured {
				t.Fatalf("Configured = %v, want %v", st.Configured, tc.wantConfigured)
			}
			if !tc.wantConfigured {
				if st.LongBlocked || st.ShortBlocked {
					t.Fatalf("disabled cap must not mark buckets blocked: %+v", st)
				}
				return
			}
			if st.LongUSD != tc.wantLong {
				t.Errorf("LongUSD = %f, want %f", st.LongUSD, tc.wantLong)
			}
			if st.ShortUSD != tc.wantShort {
				t.Errorf("ShortUSD = %f, want %f", st.ShortUSD, tc.wantShort)
			}
			if st.LongBlocked != tc.wantLongBlocked {
				t.Errorf("LongBlocked = %v, want %v", st.LongBlocked, tc.wantLongBlocked)
			}
			if st.ShortBlocked != tc.wantShortBlocked {
				t.Errorf("ShortBlocked = %v, want %v", st.ShortBlocked, tc.wantShortBlocked)
			}
			if len(st.SkippedPositions) != len(tc.wantSkipped) {
				t.Fatalf("SkippedPositions = %v, want %d entries %v", st.SkippedPositions, len(tc.wantSkipped), tc.wantSkipped)
			}
			joined := strings.Join(st.SkippedPositions, "; ")
			for _, want := range tc.wantSkipped {
				if !strings.Contains(joined, want) {
					t.Errorf("missing skip entry %q in %v", want, st.SkippedPositions)
				}
			}
			if tc.wantSkippedWarning != "" {
				if msg := exposureCapSkippedWarning(st); !strings.Contains(msg, tc.wantSkippedWarning) {
					t.Errorf("unexpected skipped warning: %q", msg)
				}
			}
		})
	}
}

func TestEvaluateExposureCap_Concentration(t *testing.T) {
	pr := &PortfolioRiskConfig{MaxDrawdownPct: 25, MaxAssetConcentrationPct: 40}
	st := evaluateExposureCap(pr, exposureTestStates(), exposureTestConfigs(), exposureTestPrices(), 20000)

	if st.LongBlocked || st.ShortBlocked {
		t.Error("bucket arm is disabled (cap 0) — no bucket block expected")
	}
	stat, ok := st.OverConcentrated["BTC"]
	if !ok {
		t.Fatalf("expected BTC over-concentrated, got %v", st.OverConcentrated)
	}
	if stat.Direction != "long" || stat.Pct != 50 {
		t.Errorf("BTC stat = %+v, want long 50%%", stat)
	}
	if _, ok := st.OverConcentrated["ETH"]; ok {
		t.Error("ETH must not be over-concentrated at 30%")
	}

	if blocked, why := exposureCapBlocksSignal(st, "BTC", 1, 0, 0, "", true, true); !blocked {
		t.Error("expected BTC long open blocked by concentration")
	} else if !strings.Contains(why, "BTC") || !strings.Contains(why, "50.0%") {
		t.Errorf("unexpected reason: %q", why)
	}
	if blocked, _ := exposureCapBlocksSignal(st, "BTC", -1, 0, 0, "", true, true); blocked {
		t.Error("BTC short entry must pass — it reduces the long concentration")
	}
	if blocked, _ := exposureCapBlocksSignal(st, "ETH", 1, 0, 0, "", true, true); blocked {
		t.Error("ETH long entry must pass — only the over-concentrated asset is held")
	}
}

func manualExposureTestConfig() *Config {
	return &Config{
		PortfolioRisk: &PortfolioRiskConfig{MaxDrawdownPct: 25, MaxAssetConcentrationPct: 40},
		Strategies: append(exposureTestConfigs(),
			StrategyConfig{ID: "hl-manual", Type: "manual", Platform: "hyperliquid", Args: []string{"hold", "BTC"}}),
	}
}

func TestManualExposureCapStatus_ConcentrationOnlyEnforced(t *testing.T) {
	cfg := manualExposureTestConfig()
	state := &AppState{Strategies: exposureTestStates()}

	st := manualExposureCapStatus(cfg, state, defaultPaperPartition)
	if !st.Configured {
		t.Fatal("expected Configured=true")
	}
	if st.PortfolioValue != 18200 {
		t.Errorf("PortfolioValue = %f, want 18200 (AvgCost basis)", st.PortfolioValue)
	}
	if st.PVBasisMiss {
		t.Error("expected PVBasisMiss=false — the manual path must derive a basis")
	}
	stat, ok := st.OverConcentrated["BTC"]
	if !ok || stat.Direction != "long" {
		t.Fatalf("expected BTC over-concentrated long, got %+v", st.OverConcentrated)
	}
	if _, ok := st.OverConcentrated["ETH"]; ok {
		t.Error("ETH (31.9%) must not be over a 40% cap")
	}

	blocked, why := exposureCapManualEntryBlock(st, "BTC", "long")
	if !blocked {
		t.Fatal("expected manual long BTC entry blocked by the concentration arm")
	}
	if !strings.Contains(why, "BTC net long") || !strings.Contains(why, "cap 40.0%") {
		t.Errorf("unexpected reason: %q", why)
	}
	if blocked, _ := exposureCapManualEntryBlock(st, "BTC", "short"); blocked {
		t.Error("short BTC entry must pass — concentration blocks the net direction only")
	}
	if blocked, _ := exposureCapManualEntryBlock(st, "SOL", "long"); blocked {
		t.Error("SOL long entry must pass (15.4% < 40%)")
	}
}

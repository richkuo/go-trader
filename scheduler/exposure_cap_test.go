package main

import (
	"errors"
	"strings"
	"testing"
	"time"
)

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

func TestEvaluateExposureCap_PVBasisMiss(t *testing.T) {
	pr := &PortfolioRiskConfig{MaxDrawdownPct: 25, MaxAssetConcentrationPct: 40}
	st := evaluateExposureCap(pr, exposureTestStates(), exposureTestConfigs(), exposureTestPrices(), 0)
	if !st.PVBasisMiss {
		t.Error("expected PVBasisMiss=true with zero portfolio value")
	}
	if len(st.OverConcentrated) != 0 {
		t.Error("concentration must not evaluate against a zero basis")
	}
	if blocked, _ := exposureCapBlocksSignal(st, "BTC", 1, 0, 0, "", true, true); blocked {
		t.Error("a non-evaluable concentration arm must not block (fail-safe, surfaced loudly instead)")
	}
}

func TestExposureCapOptionsActions(t *testing.T) {
	st := ExposureCapStatus{
		Configured: true, CapUSD: 100, LongUSD: 500, LongBlocked: true,
	}
	actions := []OptionsAction{
		{Action: "buy", OptionType: "call"},
		{Action: "buy", OptionType: "put"},
		{Action: "sell", OptionType: "call"},
		{Action: "sell", OptionType: "put"},
		{Action: "close", OptionType: "call"},
		{Action: "buy", OptionType: "put", Greeks: OptGreeks{Delta: 0.4}},
	}
	kept, dropped, reason := exposureCapOptionsActions(st, "BTC", actions)
	if dropped != 3 {
		t.Fatalf("dropped = %d, want 3 (kept: %+v)", dropped, kept)
	}
	if len(kept) != 3 {
		t.Fatalf("kept = %d actions, want 3: %+v", len(kept), kept)
	}
	if !strings.Contains(reason, "long-delta option opens blocked") {
		t.Errorf("unexpected reason: %q", reason)
	}
	kept, dropped, _ = exposureCapOptionsActions(ExposureCapStatus{}, "BTC", actions)
	if dropped != 0 || len(kept) != len(actions) {
		t.Error("unconfigured cap must not drop option actions")
	}
}

func TestExposureCapOptionsActions_ConcentrationScopedToAsset(t *testing.T) {
	st := ExposureCapStatus{
		Configured:       true,
		ConcentrationPct: 40,
		PortfolioValue:   20000,
		OverConcentrated: map[string]ExposureCapAssetStat{"BTC": {Direction: "long", Pct: 55, NetUSD: 11000}},
	}
	buyCall := []OptionsAction{{Action: "buy", OptionType: "call"}}
	if _, dropped, _ := exposureCapOptionsActions(st, "BTC", buyCall); dropped != 1 {
		t.Error("BTC long-delta open must be dropped for the over-concentrated underlying")
	}
	if _, dropped, _ := exposureCapOptionsActions(st, "ETH", buyCall); dropped != 0 {
		t.Error("ETH long-delta open must pass — concentration is per-asset")
	}
	sellCall := []OptionsAction{{Action: "sell", OptionType: "call"}}
	if _, dropped, _ := exposureCapOptionsActions(st, "BTC", sellCall); dropped != 0 {
		t.Error("BTC short-delta open must pass — it reduces the long concentration")
	}
}

func TestExposureCapAlertMessage_EdgeTriggered(t *testing.T) {
	now := time.Date(2026, 7, 9, 12, 0, 0, 0, time.UTC)
	blocked := ExposureCapStatus{
		Configured: true, CapUSD: 15000, LongUSD: 19000, LongBlocked: true,
	}
	msg, alertState := exposureCapAlertMessage(blocked, exposureCapAlertState{}, now)
	if msg == "" || !strings.Contains(msg, "new long opens blocked") {
		t.Fatalf("expected first-block DM, got %q", msg)
	}
	msg, alertState = exposureCapAlertMessage(blocked, alertState, now)
	if msg != "" {
		t.Fatalf("expected no repeat DM while still blocked, got %q", msg)
	}
	clear := ExposureCapStatus{Configured: true, CapUSD: 15000, LongUSD: 9000}
	msg, alertState = exposureCapAlertMessage(clear, alertState, now)
	if msg != "" {
		t.Fatalf("expected no DM on clear, got %q", msg)
	}
	msg, _ = exposureCapAlertMessage(blocked, alertState, now)
	if msg == "" {
		t.Fatal("expected DM on re-block after clearing")
	}
}

func TestExposureCapAlertMessage_ConcentrationAndBasisMiss(t *testing.T) {
	now := time.Date(2026, 7, 9, 12, 0, 0, 0, time.UTC)
	st := ExposureCapStatus{
		Configured:       true,
		ConcentrationPct: 40,
		PortfolioValue:   20000,
		OverConcentrated: map[string]ExposureCapAssetStat{"BTC": {Direction: "long", Pct: 50, NetUSD: 10000}},
	}
	msg, alertState := exposureCapAlertMessage(st, exposureCapAlertState{}, now)
	if !strings.Contains(msg, "BTC net long") {
		t.Fatalf("expected concentration DM, got %q", msg)
	}
	if msg, _ = exposureCapAlertMessage(st, alertState, now); msg != "" {
		t.Fatalf("expected no repeat concentration DM, got %q", msg)
	}

	miss := ExposureCapStatus{Configured: true, ConcentrationPct: 40, PVBasisMiss: true}
	msg, alertState = exposureCapAlertMessage(miss, exposureCapAlertState{}, now)
	if !strings.Contains(msg, "CANNOT evaluate") {
		t.Fatalf("expected basis-miss DM, got %q", msg)
	}
	if msg, _ = exposureCapAlertMessage(miss, alertState, now); msg != "" {
		t.Fatalf("expected no repeat basis-miss DM, got %q", msg)
	}
}

func TestExposureCapStartupSummaryLine(t *testing.T) {
	if line := exposureCapStartupSummaryLine(&PortfolioRiskConfig{MaxDrawdownPct: 25}); line != "" {
		t.Errorf("expected empty line when disabled, got %q", line)
	}
	line := exposureCapStartupSummaryLine(&PortfolioRiskConfig{MaxSameDirectionNotionalUSD: 15000, MaxAssetConcentrationPct: 40})
	for _, want := range []string{"same_direction=$15000.00", "asset_concentration=40.0%", "capped-direction opens only"} {
		if !strings.Contains(line, want) {
			t.Errorf("startup line missing %q: %q", want, line)
		}
	}
}

func TestExposureCapStatusNote(t *testing.T) {
	state := &AppState{Strategies: exposureTestStates()}
	prices := exposureTestPrices()
	if note := exposureCapStatusNote(&PortfolioRiskConfig{MaxDrawdownPct: 25}, state, exposureTestConfigs(), prices); note != "" {
		t.Errorf("expected empty note when disabled, got %q", note)
	}
	armed := exposureCapStatusNote(&PortfolioRiskConfig{MaxSameDirectionNotionalUSD: 50000}, state, exposureTestConfigs(), prices)
	if !strings.Contains(armed, "🟢 exposure cap armed") || !strings.Contains(armed, "long $19000.00") {
		t.Errorf("unexpected armed note: %q", armed)
	}
	hot := exposureCapStatusNote(&PortfolioRiskConfig{MaxSameDirectionNotionalUSD: 15000}, state, exposureTestConfigs(), prices)
	if !strings.Contains(hot, "🛑 exposure cap") || !strings.Contains(hot, "new long opens blocked") {
		t.Errorf("unexpected blocking note: %q", hot)
	}
}

func TestExposureCapHoldDetail(t *testing.T) {
	st := ExposureCapStatus{Configured: true, CapUSD: 15000, LongUSD: 19000, LongBlocked: true}
	detail := exposureCapHoldDetail(st)
	if !strings.Contains(detail, "long $19000.00") || !strings.Contains(detail, "cap $15000.00") {
		t.Errorf("unexpected hold detail: %q", detail)
	}
	if exposureCapHoldDetail(ExposureCapStatus{Configured: true}) != "" {
		t.Error("expected empty detail when nothing is blocked")
	}
}

func TestExposureCapFieldsHotReloadable(t *testing.T) {
	mkCfg := func(pr *PortfolioRiskConfig) *Config {
		return &Config{
			IntervalSeconds: 60,
			PortfolioRisk:   pr,
			Strategies: []StrategyConfig{{
				ID:             "spot-btc",
				Type:           "spot",
				Platform:       "binanceus",
				Script:         "shared_scripts/check_strategy.py",
				Args:           []string{"momentum", "BTC/USDT", "1h"},
				Capital:        1000,
				MaxDrawdownPct: 10,
			}},
		}
	}
	cfg := mkCfg(&PortfolioRiskConfig{MaxDrawdownPct: 25, WarnThresholdPct: 60})
	next := mkCfg(&PortfolioRiskConfig{MaxDrawdownPct: 25, WarnThresholdPct: 60,
		MaxSameDirectionNotionalUSD: 15000, MaxAssetConcentrationPct: 40})
	if err := validateHotReloadCompatible(cfg, next); err != nil {
		t.Fatalf("exposure-cap threshold changes must be hot-reloadable, got: %v", err)
	}
}

func TestValidateConfig_ExposureCapBounds(t *testing.T) {
	cfg := Config{
		PortfolioRisk: &PortfolioRiskConfig{
			MaxDrawdownPct:              25,
			WarnThresholdPct:            60,
			MaxSameDirectionNotionalUSD: -1,
			MaxAssetConcentrationPct:    120,
		},
	}
	err := validateConfig(&cfg, false)
	if err == nil {
		t.Fatal("expected validation errors")
	}
	for _, want := range []string{"max_same_direction_notional_usd must be >= 0", "max_asset_concentration_pct must be in [0, 100]"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("validation error missing %q: %v", want, err)
		}
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

	st := manualExposureCapStatus(cfg, state)
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

func TestExposureCapManualEntryBlock_BucketArm(t *testing.T) {
	pr := &PortfolioRiskConfig{MaxDrawdownPct: 25, MaxSameDirectionNotionalUSD: 15000}
	st := evaluateExposureCap(pr, exposureTestStates(), exposureTestConfigs(), exposureTestPrices(), 20000)
	if blocked, why := exposureCapManualEntryBlock(st, "BTC", "long"); !blocked || !strings.Contains(why, "exceeds cap") {
		t.Errorf("expected long entry blocked by bucket arm, got blocked=%v why=%q", blocked, why)
	}
	if blocked, _ := exposureCapManualEntryBlock(st, "BTC", "short"); blocked {
		t.Error("short entry must pass while only the long bucket is capped")
	}
}

func TestManualStateView_CarriesConcentrationArm(t *testing.T) {
	cfg := manualExposureTestConfig()
	states := exposureTestStates()
	states["hl-manual"] = &StrategyState{
		ID: "hl-manual", Type: "manual",
		Positions:       map[string]*Position{"BTC": {Symbol: "BTC", Quantity: 0.05, Side: "long", AvgCost: 48000}},
		OptionPositions: make(map[string]*OptionPosition),
	}
	state := &AppState{Strategies: states}

	view := manualStateViewFromState(cfg, state, "hl-manual", "BTC")
	if view.ExposureCapAsset != "BTC" {
		t.Fatalf("ExposureCapAsset = %q, want BTC", view.ExposureCapAsset)
	}
	if _, ok := view.ExposureCap.OverConcentrated["BTC"]; !ok {
		t.Fatal("expected BTC over-concentrated in the view status")
	}
	if blocked, _ := exposureCapManualEntryBlock(view.ExposureCap, view.ExposureCapAsset, "long"); !blocked {
		t.Error("manual-add long on an over-concentrated asset must refuse")
	}
	if blocked, _ := exposureCapManualEntryBlock(view.ExposureCap, view.ExposureCapAsset, "short"); blocked {
		t.Error("short direction must pass")
	}
}

func TestManualExposureCapStatus_PVBasisMissSurfaced(t *testing.T) {
	cfg := manualExposureTestConfig()
	st := manualExposureCapStatus(cfg, &AppState{Strategies: map[string]*StrategyState{}})
	if !st.PVBasisMiss {
		t.Error("expected PVBasisMiss=true on a zero-value book")
	}
	if blocked, _ := exposureCapManualEntryBlock(st, "BTC", "long"); blocked {
		t.Error("PVBasisMiss must never block — loudly inert only")
	}
}

func exposureCapE2EDeps(t *testing.T, view manualStateView, executed *bool) manualCoreDeps {
	t.Helper()
	return manualCoreDeps{
		cfg: &Config{},
		loadState: func(strategyID, symbol string) (manualStateView, error) {
			return view, nil
		},
		execute: func(string, string, string, float64, float64, int64, float64, string, float64, bool, hlExecuteSnapshot, ...int64) (*HyperliquidExecuteResult, string, error) {
			*executed = true
			return nil, "", errSentinelStopAfterGuards
		},
		fetchMids: func([]string) (map[string]float64, error) {
			return map[string]float64{"ETH": 2000}, nil
		},
	}
}

var errSentinelStopAfterGuards = errors.New("sentinel: guards passed, stop before state update")

func TestManualCoreRefusesExposureCap(t *testing.T) {
	bucketView := manualStateView{HasStrategy: true, ExposureCapAsset: "ETH",
		ExposureCap: ExposureCapStatus{Configured: true, CapUSD: 15000, LongUSD: 19000, LongBlocked: true}}
	concStatus := ExposureCapStatus{Configured: true, ConcentrationPct: 40, PortfolioValue: 18200,
		OverConcentrated: map[string]ExposureCapAssetStat{
			"ETH": {Direction: "long", Pct: 52.7, NetUSD: 9600},
		}}
	concView := func(side string) manualStateView {
		return manualStateView{HasStrategy: true, ExposureCap: concStatus, ExposureCapAsset: "ETH",
			Pos: &Position{Symbol: "ETH", Quantity: 1, AvgCost: 2000, Side: side}}
	}
	openSC := StrategyConfig{ID: "m", Type: "manual", Platform: "hyperliquid", Symbol: "ETH", Leverage: 3, Direction: "both"}
	addSC := StrategyConfig{ID: "m", Type: "manual", Platform: "hyperliquid", Symbol: "ETH", Leverage: 3}
	cases := []struct {
		name         string
		view         manualStateView
		run          func(deps manualCoreDeps) error
		wantErr      string
		wantExecuted bool
	}{
		{"open_long_refused_by_bucket_arm", bucketView, func(deps manualCoreDeps) error {
			_, err := manualOpenCore(deps, openSC, manualOpenInputs{StrategyID: "m", Side: "long", Margin: 50})
			return err
		}, "manual-open (long) blocked", false},
		{"open_short_reaches_execute_while_only_longs_capped", bucketView, func(deps manualCoreDeps) error {
			_, err := manualOpenCore(deps, openSC, manualOpenInputs{StrategyID: "m", Side: "short", Margin: 50})
			return err
		}, "", true},
		{"add_long_refused_by_concentration_only", concView("long"), func(deps manualCoreDeps) error {
			_, err := manualAddCore(deps, addSC, manualAddInputs{StrategyID: "m", Margin: 50})
			return err
		}, "manual-add (long) blocked", false},
		{"add_short_reaches_execute_when_long_over_concentrated", concView("short"), func(deps manualCoreDeps) error {
			_, err := manualAddCore(deps, addSC, manualAddInputs{StrategyID: "m", Margin: 50})
			return err
		}, "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			executed := false
			err := tc.run(exposureCapE2EDeps(t, tc.view, &executed))
			if tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)) {
				t.Fatalf("err = %v, want refusal containing %q", err, tc.wantErr)
			}
			if executed != tc.wantExecuted {
				t.Fatalf("executed = %v, want %v (err = %v)", executed, tc.wantExecuted, err)
			}
		})
	}
}

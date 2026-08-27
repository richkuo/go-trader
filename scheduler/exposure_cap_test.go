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


func TestEvaluateExposureCap_AllLongBookBlocksLongsOnly(t *testing.T) {
	pr := &PortfolioRiskConfig{MaxDrawdownPct: 25, MaxSameDirectionNotionalUSD: 15000}
	st := evaluateExposureCap(pr, exposureTestStates(), exposureTestConfigs(), exposureTestPrices(), 20000)

	if !st.Configured {
		t.Fatal("expected Configured=true")
	}
	if st.LongUSD != 19000 {
		t.Errorf("LongUSD = %f, want 19000", st.LongUSD)
	}
	if st.ShortUSD != 0 {
		t.Errorf("ShortUSD = %f, want 0", st.ShortUSD)
	}
	if !st.LongBlocked {
		t.Error("expected LongBlocked=true (19000 > 15000)")
	}
	if st.ShortBlocked {
		t.Error("expected ShortBlocked=false")
	}

	blocked, why := exposureCapBlocksSignal(st, "BTC", 1, 0, 0, "", true, true)
	if !blocked {
		t.Fatal("expected fresh long open blocked")
	}
	if !strings.Contains(why, "new long opens blocked") || !strings.Contains(why, "$19000.00") || !strings.Contains(why, "$15000.00") {
		t.Errorf("unexpected reason: %q", why)
	}
	if blocked, _ := exposureCapBlocksSignal(st, "BTC", -1, 0, 0, "", true, true); blocked {
		t.Error("short entry must not be blocked when only the long bucket is capped")
	}
}

func TestEvaluateExposureCap_NettingPerAsset(t *testing.T) {
	states := map[string]*StrategyState{
		"hl-a-btc": {
			ID: "hl-a-btc", Type: "perps",
			Positions: map[string]*Position{
				"BTC": {Symbol: "BTC", Quantity: 0.2, Side: "long", AvgCost: 48000},
			},
			OptionPositions: make(map[string]*OptionPosition),
		},
		"hl-b-eth": {
			ID: "hl-b-eth", Type: "perps",
			Positions: map[string]*Position{
				"ETH": {Symbol: "ETH", Quantity: 2, Side: "short", AvgCost: 2900},
			},
			OptionPositions: make(map[string]*OptionPosition),
		},
	}
	cfgs := []StrategyConfig{
		{ID: "hl-a-btc", Type: "perps", Platform: "hyperliquid", Args: []string{"momentum", "BTC", "1h"}},
		{ID: "hl-b-eth", Type: "perps", Platform: "hyperliquid", Args: []string{"momentum", "ETH", "1h"}},
	}
	prices := map[string]float64{"BTC": 50000, "ETH": 5000}
	pr := &PortfolioRiskConfig{MaxDrawdownPct: 25, MaxSameDirectionNotionalUSD: 15000}

	st := evaluateExposureCap(pr, states, cfgs, prices, 20000)
	if st.LongUSD != 10000 {
		t.Errorf("LongUSD = %f, want 10000", st.LongUSD)
	}
	if st.ShortUSD != 10000 {
		t.Errorf("ShortUSD = %f, want 10000", st.ShortUSD)
	}
	if st.LongBlocked || st.ShortBlocked {
		t.Error("neither bucket may block under a 15000 cap")
	}
}

func TestEvaluateExposureCap_SameAssetNetsBeforeBucketing(t *testing.T) {
	states := map[string]*StrategyState{
		"hl-a-btc": {
			ID: "hl-a-btc", Type: "perps",
			Positions: map[string]*Position{
				"BTC": {Symbol: "BTC", Quantity: 0.3, Side: "long", AvgCost: 48000},
			},
			OptionPositions: make(map[string]*OptionPosition),
		},
		"hl-b-btc": {
			ID: "hl-b-btc", Type: "perps",
			Positions: map[string]*Position{
				"BTC": {Symbol: "BTC", Quantity: 0.1, Side: "short", AvgCost: 48000},
			},
			OptionPositions: make(map[string]*OptionPosition),
		},
	}
	cfgs := []StrategyConfig{
		{ID: "hl-a-btc", Type: "perps", Platform: "hyperliquid", Args: []string{"momentum", "BTC", "1h"}},
		{ID: "hl-b-btc", Type: "perps", Platform: "hyperliquid", Args: []string{"triple_ema", "BTC", "1h"}},
	}
	prices := map[string]float64{"BTC": 50000}
	pr := &PortfolioRiskConfig{MaxDrawdownPct: 25, MaxSameDirectionNotionalUSD: 15000}

	st := evaluateExposureCap(pr, states, cfgs, prices, 20000)
	if st.LongUSD != 10000 {
		t.Errorf("LongUSD = %f, want 10000", st.LongUSD)
	}
	if st.ShortUSD != 0 {
		t.Errorf("ShortUSD = %f, want 0 (same-asset short nets against the long)", st.ShortUSD)
	}
}

func TestEvaluateExposureCap_DisabledByDefault(t *testing.T) {
	pr := &PortfolioRiskConfig{MaxDrawdownPct: 25}
	st := evaluateExposureCap(pr, exposureTestStates(), exposureTestConfigs(), exposureTestPrices(), 20000)
	if st.Configured {
		t.Error("expected Configured=false with zero thresholds")
	}
	if blocked, _ := exposureCapBlocksSignal(st, "BTC", 1, 0, 0, "", true, true); blocked {
		t.Error("disabled cap must not block")
	}
	if st.LongBlocked || st.ShortBlocked {
		t.Error("disabled cap must not mark buckets blocked")
	}
	st = evaluateExposureCap(nil, exposureTestStates(), exposureTestConfigs(), exposureTestPrices(), 20000)
	if st.Configured {
		t.Error("expected Configured=false with nil config")
	}
}

func TestEvaluateExposureCap_FailSafeExclusions(t *testing.T) {
	states := map[string]*StrategyState{
		"hl-a-btc": {
			ID: "hl-a-btc", Type: "perps",
			Positions: map[string]*Position{
				"BTC": {Symbol: "BTC", Quantity: 0.2, Side: "long", AvgCost: 48000},
			},
			OptionPositions: make(map[string]*OptionPosition),
		},
		"hl-b-xyz": {
			ID: "hl-b-xyz", Type: "perps",
			Positions: map[string]*Position{
				"XYZ": {Symbol: "XYZ", Quantity: 5, Side: "long", AvgCost: 0},
			},
			OptionPositions: make(map[string]*OptionPosition),
		},
		"hl-c-eth": {
			ID: "hl-c-eth", Type: "perps",
			Positions: map[string]*Position{
				"ETH": {Symbol: "ETH", Quantity: -1, Side: "long", AvgCost: 3000},
			},
			OptionPositions: make(map[string]*OptionPosition),
		},
	}
	cfgs := []StrategyConfig{
		{ID: "hl-a-btc", Type: "perps", Platform: "hyperliquid", Args: []string{"momentum", "BTC", "1h"}},
		{ID: "hl-b-xyz", Type: "perps", Platform: "hyperliquid", Args: []string{"momentum", "XYZ", "1h"}},
		{ID: "hl-c-eth", Type: "perps", Platform: "hyperliquid", Args: []string{"momentum", "ETH", "1h"}},
	}
	prices := map[string]float64{"BTC": 50000, "ETH": 3000}
	pr := &PortfolioRiskConfig{MaxDrawdownPct: 25, MaxSameDirectionNotionalUSD: 15000}

	st := evaluateExposureCap(pr, states, cfgs, prices, 20000)
	if st.LongUSD != 10000 {
		t.Errorf("LongUSD = %f, want 10000 (only the priceable BTC leg counts)", st.LongUSD)
	}
	if st.LongBlocked {
		t.Error("expected LongBlocked=false — exclusions must not inflate the sum")
	}
	if len(st.SkippedPositions) != 2 {
		t.Fatalf("expected 2 skipped positions, got %v", st.SkippedPositions)
	}
	joined := strings.Join(st.SkippedPositions, "; ")
	if !strings.Contains(joined, "hl-b-xyz/XYZ: no usable price") {
		t.Errorf("missing unpriceable skip entry: %v", st.SkippedPositions)
	}
	if !strings.Contains(joined, "hl-c-eth/ETH: non-positive quantity") {
		t.Errorf("missing corrupt-quantity skip entry: %v", st.SkippedPositions)
	}
	if msg := exposureCapSkippedWarning(st); !strings.Contains(msg, "2 position(s) excluded") {
		t.Errorf("unexpected skipped warning: %q", msg)
	}
}

func TestEvaluateExposureCap_AvgCostFallback(t *testing.T) {
	pr := &PortfolioRiskConfig{MaxDrawdownPct: 25, MaxSameDirectionNotionalUSD: 15000}
	st := evaluateExposureCap(pr, exposureTestStates(), exposureTestConfigs(), nil, 0)
	if st.LongUSD != 18200 {
		t.Errorf("LongUSD = %f, want 18200 (AvgCost valuation)", st.LongUSD)
	}
	if !st.LongBlocked {
		t.Error("expected LongBlocked=true at AvgCost valuation")
	}
	if len(st.SkippedPositions) != 0 {
		t.Errorf("expected no skips with positive AvgCosts, got %v", st.SkippedPositions)
	}
}

func TestEvaluateExposureCap_ManualPositionsCounted(t *testing.T) {
	states := map[string]*StrategyState{
		"hl-manual": {
			ID: "hl-manual", Type: "manual",
			Positions: map[string]*Position{
				"ETH": {Symbol: "ETH", Quantity: 4, Side: "long", AvgCost: 2900},
			},
			OptionPositions: make(map[string]*OptionPosition),
		},
	}
	cfgs := []StrategyConfig{
		{ID: "hl-manual", Type: "manual", Platform: "hyperliquid", Args: []string{"hold", "ETH"}},
	}
	pr := &PortfolioRiskConfig{MaxDrawdownPct: 25, MaxSameDirectionNotionalUSD: 10000}
	st := evaluateExposureCap(pr, states, cfgs, map[string]float64{"ETH": 3000}, 20000)
	if st.LongUSD != 12000 {
		t.Errorf("LongUSD = %f, want 12000 (manual positions count)", st.LongUSD)
	}
	if !st.LongBlocked {
		t.Error("expected LongBlocked=true")
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


func TestExposureCapBlocksSignal_ManageAndReducePassThrough(t *testing.T) {
	st := ExposureCapStatus{
		Configured: true, CapUSD: 100, LongUSD: 500, ShortUSD: 500,
		LongBlocked: true, ShortBlocked: true,
	}
	if blocked, _ := exposureCapBlocksSignal(st, "BTC", 0, 0, 1, "long", true, true); blocked {
		t.Error("signal==0 must pass (manage-only path keeps running)")
	}
	if blocked, _ := exposureCapBlocksSignal(st, "BTC", -1, 1.0, 1, "long", true, true); blocked {
		t.Error("close action must pass")
	}
	if blocked, _ := exposureCapBlocksSignal(st, "BTC", -1, 0, 1, "long", true, false); blocked {
		t.Error("pure-close sell on a long must pass")
	}
	if blocked, _ := exposureCapBlocksSignal(st, "BTC", 1, 0, 1, "short", false, true); blocked {
		t.Error("pure-close buy on a short must pass")
	}
}

func TestExposureCapBlocksSignal_DirectionalIncreases(t *testing.T) {
	longCapped := ExposureCapStatus{Configured: true, CapUSD: 100, LongUSD: 500, LongBlocked: true}
	shortCapped := ExposureCapStatus{Configured: true, CapUSD: 100, ShortUSD: 500, ShortBlocked: true}

	if blocked, _ := exposureCapBlocksSignal(longCapped, "BTC", 1, 0, 1, "long", true, true); !blocked {
		t.Error("scale-in add on a long must be blocked while longs are capped")
	}
	if blocked, _ := exposureCapBlocksSignal(longCapped, "BTC", -1, 0, 1, "long", true, true); blocked {
		t.Error("long→short flip must pass while only the long bucket is capped")
	}
	if blocked, _ := exposureCapBlocksSignal(shortCapped, "BTC", -1, 0, 1, "long", true, true); !blocked {
		t.Error("long→short flip must be held while the short bucket is capped")
	}
	if blocked, _ := exposureCapBlocksSignal(shortCapped, "BTC", -1, 0, 0, "", true, true); !blocked {
		t.Error("fresh short open must be blocked while shorts are capped")
	}
	if blocked, _ := exposureCapBlocksSignal(longCapped, "BTC", -1, 0, 0, "", true, true); blocked {
		t.Error("fresh short open must pass while only longs are capped")
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

func TestManualOpenCoreRefusesExposureCap(t *testing.T) {
	sc := StrategyConfig{ID: "m", Type: "manual", Platform: "hyperliquid", Symbol: "ETH", Leverage: 3, Direction: "both"}
	view := manualStateView{HasStrategy: true, ExposureCapAsset: "ETH",
		ExposureCap: ExposureCapStatus{Configured: true, CapUSD: 15000, LongUSD: 19000, LongBlocked: true}}

	executed := false
	deps := exposureCapE2EDeps(t, view, &executed)
	_, err := manualOpenCore(deps, sc, manualOpenInputs{StrategyID: "m", Side: "long", Margin: 50})
	if err == nil || !strings.Contains(err.Error(), "manual-open (long) blocked") {
		t.Fatalf("manual-open err = %v, want exposure-cap refusal", err)
	}
	if executed {
		t.Fatal("execute must not be called while the long bucket is capped")
	}

	executed = false
	deps = exposureCapE2EDeps(t, view, &executed)
	_, err = manualOpenCore(deps, sc, manualOpenInputs{StrategyID: "m", Side: "short", Margin: 50})
	if !executed {
		t.Fatalf("short entry must reach execute while only the long bucket is capped (err = %v)", err)
	}
}

func TestManualAddCoreRefusesConcentrationOnly(t *testing.T) {
	sc := StrategyConfig{ID: "m", Type: "manual", Platform: "hyperliquid", Symbol: "ETH", Leverage: 3}
	concStatus := ExposureCapStatus{Configured: true, ConcentrationPct: 40, PortfolioValue: 18200,
		OverConcentrated: map[string]ExposureCapAssetStat{
			"ETH": {Direction: "long", Pct: 52.7, NetUSD: 9600},
		}}
	longPos := &Position{Symbol: "ETH", Quantity: 1, AvgCost: 2000, Side: "long"}
	view := manualStateView{HasStrategy: true, Pos: longPos,
		ExposureCap: concStatus, ExposureCapAsset: "ETH"}

	executed := false
	deps := exposureCapE2EDeps(t, view, &executed)
	_, err := manualAddCore(deps, sc, manualAddInputs{StrategyID: "m", Margin: 50})
	if err == nil || !strings.Contains(err.Error(), "manual-add (long) blocked") {
		t.Fatalf("manual-add err = %v, want concentration refusal", err)
	}
	if executed {
		t.Fatal("execute must not be called on an over-concentrated add")
	}

	shortPos := &Position{Symbol: "ETH", Quantity: 1, AvgCost: 2000, Side: "short"}
	view.Pos = shortPos
	executed = false
	deps = exposureCapE2EDeps(t, view, &executed)
	_, err = manualAddCore(deps, sc, manualAddInputs{StrategyID: "m", Margin: 50})
	if !executed {
		t.Fatalf("short add must reach execute when ETH is long-over-concentrated (err = %v)", err)
	}
}

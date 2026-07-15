package main

import (
	"errors"
	"strings"
	"testing"
	"time"
)

// --- test fixtures -----------------------------------------------------------

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

// --- aggregation -------------------------------------------------------------

// Acceptance: a long BTC + long ETH + long SOL book over the cap blocks the
// next long open; a short entry on the same cycle is not blocked.
func TestEvaluateExposureCap_AllLongBookBlocksLongsOnly(t *testing.T) {
	pr := &PortfolioRiskConfig{MaxDrawdownPct: 25, MaxSameDirectionNotionalUSD: 15000}
	// Long: 0.2*50000 + 2*3000 + 20*150 = 10000 + 6000 + 3000 = 19000
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

	// Fresh long open from flat is blocked with an explicit reason...
	blocked, why := exposureCapBlocksSignal(st, "BTC", 1, 0, 0, "", true, true)
	if !blocked {
		t.Fatal("expected fresh long open blocked")
	}
	if !strings.Contains(why, "new long opens blocked") || !strings.Contains(why, "$19000.00") || !strings.Contains(why, "$15000.00") {
		t.Errorf("unexpected reason: %q", why)
	}
	// ...while a short entry on the same cycle passes.
	if blocked, _ := exposureCapBlocksSignal(st, "BTC", -1, 0, 0, "", true, true); blocked {
		t.Error("short entry must not be blocked when only the long bucket is capped")
	}
}

// Acceptance: netting is honored — long $10k BTC + short $10k ETH contributes
// $10k long / $10k short and neither bucket blocks under a $15k cap.
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

// Same-asset netting: a long and a short on the SAME asset net before
// bucketing — they must not double-count into both buckets.
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
	// Net BTC: 0.3*50000 - 0.1*50000 = 10000 long; short bucket stays empty.
	if st.LongUSD != 10000 {
		t.Errorf("LongUSD = %f, want 10000", st.LongUSD)
	}
	if st.ShortUSD != 0 {
		t.Errorf("ShortUSD = %f, want 0 (same-asset short nets against the long)", st.ShortUSD)
	}
}

// Acceptance: disabled by default — zero-valued fields gate nothing.
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
	// nil config too.
	st = evaluateExposureCap(nil, exposureTestStates(), exposureTestConfigs(), exposureTestPrices(), 20000)
	if st.Configured {
		t.Error("expected Configured=false with nil config")
	}
}

// Acceptance: corrupt / unpriceable positions fail safe — excluded from the
// sums and recorded, never blocking everything or nothing.
func TestEvaluateExposureCap_FailSafeExclusions(t *testing.T) {
	states := map[string]*StrategyState{
		"hl-a-btc": {
			ID: "hl-a-btc", Type: "perps",
			Positions: map[string]*Position{
				"BTC": {Symbol: "BTC", Quantity: 0.2, Side: "long", AvgCost: 48000},
			},
			OptionPositions: make(map[string]*OptionPosition),
		},
		"hl-b-xyz": { // no live price AND zero AvgCost → excluded
			ID: "hl-b-xyz", Type: "perps",
			Positions: map[string]*Position{
				"XYZ": {Symbol: "XYZ", Quantity: 5, Side: "long", AvgCost: 0},
			},
			OptionPositions: make(map[string]*OptionPosition),
		},
		"hl-c-eth": { // corrupt: non-positive quantity → excluded
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

// nil prices (manual CLI path) value positions at AvgCost.
func TestEvaluateExposureCap_AvgCostFallback(t *testing.T) {
	pr := &PortfolioRiskConfig{MaxDrawdownPct: 25, MaxSameDirectionNotionalUSD: 15000}
	// AvgCost book: 0.2*48000 + 2*2900 + 20*140 = 9600 + 5800 + 2800 = 18200
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

// Manual-type strategies contribute to the bucket like perps.
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

func TestEvaluateExposureCap_CorrelatedHedgeCountsItsOwnAsset(t *testing.T) {
	states := map[string]*StrategyState{
		"hl-eth": {
			ID: "hl-eth", Type: "perps",
			Positions: map[string]*Position{
				"ETH": {Symbol: "ETH", Quantity: 2, Side: "long", AvgCost: 3000, Multiplier: 1},
				"BTC": {Symbol: "BTC", Quantity: 0.1, Side: "short", AvgCost: 50000, Multiplier: 1, HedgeFor: "ETH"},
			},
		},
	}
	cfgs := []StrategyConfig{{ID: "hl-eth", Type: "perps", Platform: "hyperliquid", Args: []string{"hold", "ETH", "1h"}}}
	st := evaluateExposureCap(&PortfolioRiskConfig{MaxSameDirectionNotionalUSD: 4000}, states, cfgs, map[string]float64{"ETH": 3000, "BTC": 50000}, 20000)
	if st.LongUSD != 6000 || st.ShortUSD != 5000 {
		t.Fatalf("same-direction buckets = long $%.2f short $%.2f, want $6000/$5000", st.LongUSD, st.ShortUSD)
	}
	if !st.LongBlocked || !st.ShortBlocked {
		t.Fatalf("both independent hedge assets must be visible to the cap: %#v", st)
	}
}

// --- concentration arm ---------------------------------------------------------

func TestEvaluateExposureCap_Concentration(t *testing.T) {
	pr := &PortfolioRiskConfig{MaxDrawdownPct: 25, MaxAssetConcentrationPct: 40}
	// PV 20000: BTC net long 10000 = 50% (over), ETH 6000 = 30%, SOL 3000 = 15%.
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

	// BTC long entries blocked; BTC shorts (reduce concentration) and ETH longs pass.
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

// --- gate decision -------------------------------------------------------------

func TestExposureCapBlocksSignal_ManageAndReducePassThrough(t *testing.T) {
	st := ExposureCapStatus{
		Configured: true, CapUSD: 100, LongUSD: 500, ShortUSD: 500,
		LongBlocked: true, ShortBlocked: true,
	}
	// signal==0 manage cycle (also the cbManageOnly carve-out shape: the CB
	// forces Signal=0 before this gate runs) — never blocked.
	if blocked, _ := exposureCapBlocksSignal(st, "BTC", 0, 0, 1, "long", true, true); blocked {
		t.Error("signal==0 must pass (manage-only path keeps running)")
	}
	// Close action from the open/close registry — passes even fully capped.
	if blocked, _ := exposureCapBlocksSignal(st, "BTC", -1, 1.0, 1, "long", true, true); blocked {
		t.Error("close action must pass")
	}
	// Pure-close directional exit: sell on a long with shorts disallowed.
	if blocked, _ := exposureCapBlocksSignal(st, "BTC", -1, 0, 1, "long", true, false); blocked {
		t.Error("pure-close sell on a long must pass")
	}
	// Pure-close buy on a short with longs disallowed.
	if blocked, _ := exposureCapBlocksSignal(st, "BTC", 1, 0, 1, "short", false, true); blocked {
		t.Error("pure-close buy on a short must pass")
	}
}

func TestExposureCapBlocksSignal_DirectionalIncreases(t *testing.T) {
	longCapped := ExposureCapStatus{Configured: true, CapUSD: 100, LongUSD: 500, LongBlocked: true}
	shortCapped := ExposureCapStatus{Configured: true, CapUSD: 100, ShortUSD: 500, ShortBlocked: true}

	// Same-side add on a long is blocked when the long bucket is capped.
	if blocked, _ := exposureCapBlocksSignal(longCapped, "BTC", 1, 0, 1, "long", true, true); !blocked {
		t.Error("scale-in add on a long must be blocked while longs are capped")
	}
	// Flip long→short opens SHORT exposure: passes when only longs are capped...
	if blocked, _ := exposureCapBlocksSignal(longCapped, "BTC", -1, 0, 1, "long", true, true); blocked {
		t.Error("long→short flip must pass while only the long bucket is capped")
	}
	// ...and is held when shorts are capped (the new exposure is short).
	if blocked, _ := exposureCapBlocksSignal(shortCapped, "BTC", -1, 0, 1, "long", true, true); !blocked {
		t.Error("long→short flip must be held while the short bucket is capped")
	}
	// Inverse scenario: fresh short open blocked only under the short cap.
	if blocked, _ := exposureCapBlocksSignal(shortCapped, "BTC", -1, 0, 0, "", true, true); !blocked {
		t.Error("fresh short open must be blocked while shorts are capped")
	}
	if blocked, _ := exposureCapBlocksSignal(longCapped, "BTC", -1, 0, 0, "", true, true); blocked {
		t.Error("fresh short open must pass while only longs are capped")
	}
}

// --- options filter --------------------------------------------------------------

func TestExposureCapOptionsActions(t *testing.T) {
	st := ExposureCapStatus{
		Configured: true, CapUSD: 100, LongUSD: 500, LongBlocked: true,
	}
	actions := []OptionsAction{
		{Action: "buy", OptionType: "call"},                               // long delta → dropped
		{Action: "buy", OptionType: "put"},                                // short delta → kept
		{Action: "sell", OptionType: "call"},                              // short delta → kept
		{Action: "sell", OptionType: "put"},                               // long delta → dropped
		{Action: "close", OptionType: "call"},                             // close → kept
		{Action: "buy", OptionType: "put", Greeks: OptGreeks{Delta: 0.4}}, // marked greeks override coarse: buy +0.4 = long → dropped
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
	// Unconfigured: everything passes untouched.
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

// --- operator surfaces ------------------------------------------------------------

func TestExposureCapAlertMessage_EdgeTriggered(t *testing.T) {
	now := time.Date(2026, 7, 9, 12, 0, 0, 0, time.UTC)
	blocked := ExposureCapStatus{
		Configured: true, CapUSD: 15000, LongUSD: 19000, LongBlocked: true,
	}
	// First cycle blocked → DM.
	msg, alertState := exposureCapAlertMessage(blocked, exposureCapAlertState{}, now)
	if msg == "" || !strings.Contains(msg, "new long opens blocked") {
		t.Fatalf("expected first-block DM, got %q", msg)
	}
	// Second cycle still blocked → no repeat DM.
	msg, alertState = exposureCapAlertMessage(blocked, alertState, now)
	if msg != "" {
		t.Fatalf("expected no repeat DM while still blocked, got %q", msg)
	}
	// Clears → no DM, state re-arms.
	clear := ExposureCapStatus{Configured: true, CapUSD: 15000, LongUSD: 9000}
	msg, alertState = exposureCapAlertMessage(clear, alertState, now)
	if msg != "" {
		t.Fatalf("expected no DM on clear, got %q", msg)
	}
	// Re-blocks → DM fires again.
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
	// Disabled → empty.
	if note := exposureCapStatusNote(&PortfolioRiskConfig{MaxDrawdownPct: 25}, state, exposureTestConfigs(), prices); note != "" {
		t.Errorf("expected empty note when disabled, got %q", note)
	}
	// Armed under the cap.
	armed := exposureCapStatusNote(&PortfolioRiskConfig{MaxSameDirectionNotionalUSD: 50000}, state, exposureTestConfigs(), prices)
	if !strings.Contains(armed, "🟢 exposure cap armed") || !strings.Contains(armed, "long $19000.00") {
		t.Errorf("unexpected armed note: %q", armed)
	}
	// Blocking.
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

// --- config plumbing ---------------------------------------------------------------

// Threshold changes must be SIGHUP hot-reloadable (deliberately unlike
// max_notional_usd, whose restart-required behavior is pinned elsewhere).
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

// --- manual path (#1301 review): both arms enforced on manual entries ---------

// manualExposureTestConfig builds a config whose concentration arm alone is
// set (bucket arm = 0) plus a manual strategy on BTC, so these tests prove the
// concentration arm protects the manual path with no bucket arm configured.
func manualExposureTestConfig() *Config {
	return &Config{
		PortfolioRisk: &PortfolioRiskConfig{MaxDrawdownPct: 25, MaxAssetConcentrationPct: 40},
		Strategies: append(exposureTestConfigs(),
			StrategyConfig{ID: "hl-manual", Type: "manual", Platform: "hyperliquid", Args: []string{"hold", "BTC"}}),
	}
}

// Acceptance: with ONLY max_asset_concentration_pct set, the manual path
// derives an AvgCost portfolio-value basis and enforces the concentration arm
// (it is not silently inert), in the asset's net direction only.
func TestManualExposureCapStatus_ConcentrationOnlyEnforced(t *testing.T) {
	cfg := manualExposureTestConfig()
	state := &AppState{Strategies: exposureTestStates()}

	st := manualExposureCapStatus(cfg, state)
	if !st.Configured {
		t.Fatal("expected Configured=true")
	}
	// AvgCost basis: 0.2*48000 + 2*2900 + 20*140 = 9600 + 5800 + 2800 = 18200.
	if st.PortfolioValue != 18200 {
		t.Errorf("PortfolioValue = %f, want 18200 (AvgCost basis)", st.PortfolioValue)
	}
	if st.PVBasisMiss {
		t.Error("expected PVBasisMiss=false — the manual path must derive a basis")
	}
	// BTC 9600/18200 = 52.7% > 40%; ETH 31.9% and SOL 15.4% under.
	stat, ok := st.OverConcentrated["BTC"]
	if !ok || stat.Direction != "long" {
		t.Fatalf("expected BTC over-concentrated long, got %+v", st.OverConcentrated)
	}
	if _, ok := st.OverConcentrated["ETH"]; ok {
		t.Error("ETH (31.9%) must not be over a 40% cap")
	}

	// manual-open long BTC refuses with the concentration reason...
	blocked, why := exposureCapManualEntryBlock(st, "BTC", "long")
	if !blocked {
		t.Fatal("expected manual long BTC entry blocked by the concentration arm")
	}
	if !strings.Contains(why, "BTC net long") || !strings.Contains(why, "cap 40.0%") {
		t.Errorf("unexpected reason: %q", why)
	}
	// ...while the opposite direction and other assets pass.
	if blocked, _ := exposureCapManualEntryBlock(st, "BTC", "short"); blocked {
		t.Error("short BTC entry must pass — concentration blocks the net direction only")
	}
	if blocked, _ := exposureCapManualEntryBlock(st, "SOL", "long"); blocked {
		t.Error("SOL long entry must pass (15.4% < 40%)")
	}
}

// Acceptance: the bucket arm still refuses through the same helper, in the
// blocked direction only (parity with the pre-#1301 manual guard).
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

// Acceptance: manualStateViewFromState carries the full status + asset key, so
// a manual-add on an over-concentrated asset refuses in the position's
// direction (integration of the view plumbing).
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

// Acceptance: an empty book (no strategies, basis 0) surfaces PVBasisMiss on
// the manual path instead of silently enforcing nothing — and blocks nothing.
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

// --- end-to-end guard wiring (#1301 review round 2): the refusals must fire
// through the actual manualOpenCore/manualAddCore call sites (mirrors the
// #1269 TestManual{Open,Add}CoreRefusesDailyLossHold pair), and the inverse
// direction must reach execute — the gate must not over-block. ----------------

// exposureCapE2EDeps builds bare-core deps whose loadState injects the given
// view and whose execute records the call, returning a sentinel error so the
// core stops right after the guards pass.
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

// (1) manualOpenCore refuses a long entry while the long bucket is capped;
// (3a) the inverse short entry passes the gate and reaches execute.
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

	// Inverse direction: a short entry must pass the gate and reach execute.
	executed = false
	deps = exposureCapE2EDeps(t, view, &executed)
	_, err = manualOpenCore(deps, sc, manualOpenInputs{StrategyID: "m", Side: "short", Margin: 50})
	if !executed {
		t.Fatalf("short entry must reach execute while only the long bucket is capped (err = %v)", err)
	}
}

// (2) manualAddCore on an existing long refuses under a concentration-only
// config (bucket arm off); (3b) a short position's add reaches execute.
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

	// Inverse direction: a short position's add is the asset's non-net
	// direction — it must pass the gate and reach execute.
	shortPos := &Position{Symbol: "ETH", Quantity: 1, AvgCost: 2000, Side: "short"}
	view.Pos = shortPos
	executed = false
	deps = exposureCapE2EDeps(t, view, &executed)
	_, err = manualAddCore(deps, sc, manualAddInputs{StrategyID: "m", Margin: 50})
	if !executed {
		t.Fatalf("short add must reach execute when ETH is long-over-concentrated (err = %v)", err)
	}
}

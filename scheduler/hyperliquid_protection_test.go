package main

import (
	"math"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestBuildHyperliquidProtectionPlanUsesDefaultTieredATR(t *testing.T) {
	mult := 1.0
	sc := StrategyConfig{
		ID:              "hl-eth",
		Type:            "perps",
		Platform:        "hyperliquid",
		StopLossATRMult: &mult,
		CloseStrategies: []StrategyRef{{Name: "tiered_tp_atr_live"}},
	}
	pos := &Position{
		Symbol:   "ETH",
		Quantity: 2,
		AvgCost:  3000,
		EntryATR: 50,
		Side:     "long",
		TPOIDs:   []int64{101, 202},
	}
	plan, ok := buildHyperliquidProtectionPlan(sc, pos)
	if !ok {
		t.Fatal("buildHyperliquidProtectionPlan returned ok=false")
	}
	if plan.StopLossATRMult != 1 {
		t.Errorf("StopLossATRMult = %g, want 1", plan.StopLossATRMult)
	}
	wantTiers := []hlProtectionTier{{Multiple: 1, Fraction: 0.5}, {Multiple: 2, Fraction: 1}}
	if !reflect.DeepEqual(plan.Tiers, wantTiers) {
		t.Errorf("tiers = %+v, want %+v", plan.Tiers, wantTiers)
	}
	if !reflect.DeepEqual(plan.TPOIDs, []int64{101, 202}) {
		t.Errorf("TP OIDs = %v, want [101 202]", plan.TPOIDs)
	}
}

func TestBuildHyperliquidProtectionPlanManualStrategy(t *testing.T) {
	mult := 1.5
	sc := StrategyConfig{
		ID:              "hl-manual-eth",
		Type:            "manual",
		Platform:        "hyperliquid",
		CloseStrategies: []StrategyRef{{Name: "tiered_tp_atr_live"}},
		StopLossATRMult: &mult,
	}
	pos := &Position{
		Symbol:      "ETH",
		Quantity:    0.4,
		AvgCost:     3000,
		EntryATR:    100,
		Side:        "long",
		StopLossOID: 123,
		TPOIDs:      []int64{456, 789},
	}
	plan, ok := buildHyperliquidProtectionPlan(sc, pos)
	if !ok {
		t.Fatal("buildHyperliquidProtectionPlan returned ok=false for manual strategy")
	}
	if plan.Symbol != "ETH" || plan.Size != 0.4 || plan.StopLossATRMult != 1.5 {
		t.Errorf("manual plan = %+v", plan)
	}
	if !reflect.DeepEqual(plan.TPOIDs, []int64{456, 789}) {
		t.Errorf("manual TP OIDs = %v, want [456 789]", plan.TPOIDs)
	}
}

// TestApplyHyperliquidProtectionSyncPreservesExistingOIDs verifies the
// "OID still resting" branch of run_sync_protection: when the result echoes
// the existing OID back (via open_orders verification), pos.TPOIDs
// must remain set. A bug where the apply path overwrote with 0 would lose
// the OID and trigger a duplicate-place on the next cycle.
func TestApplyHyperliquidProtectionSyncPreservesExistingOIDs(t *testing.T) {
	pos := &Position{
		Symbol:      "ETH",
		StopLossOID: 100,
		TPOIDs:      []int64{200, 300},
	}
	result := &HyperliquidProtectionSyncResult{
		StopLossOID:       100,
		StopLossTriggerPx: 2900,
		TPOIDs:            []int64{200, 300},
	}
	applyHyperliquidProtectionSync(pos, result)
	if pos.StopLossOID != 100 || !reflect.DeepEqual(pos.TPOIDs, []int64{200, 300}) {
		t.Errorf("OIDs mutated: SL=%d TPs=%v, want 100/[200 300]", pos.StopLossOID, pos.TPOIDs)
	}
	if pos.StopLossTriggerPx != 2900 {
		t.Errorf("StopLossTriggerPx = %g, want 2900", pos.StopLossTriggerPx)
	}
}

// TestApplyHyperliquidProtectionSyncRetainsOnZeroFields covers the case
// where the Python side couldn't fetch open_orders (so it omits OID fields
// from the result) — pos.TPOIDs must NOT be cleared, otherwise the
// next cycle would re-place against an OID that's still resting.
func TestApplyHyperliquidProtectionSyncRetainsOnZeroFields(t *testing.T) {
	pos := &Position{Symbol: "ETH", StopLossOID: 11, TPOIDs: []int64{22, 33}}
	applyHyperliquidProtectionSync(pos, &HyperliquidProtectionSyncResult{
		OpenOrderCheckError: "indexer down",
	})
	if pos.StopLossOID != 11 || !reflect.DeepEqual(pos.TPOIDs, []int64{22, 33}) {
		t.Errorf("zero-field result mutated OIDs: SL=%d TPs=%v, want 11/[22 33]", pos.StopLossOID, pos.TPOIDs)
	}
}

// TestApplyHyperliquidProtectionSyncClearsFilledExternally is the over-close
// guard: when the Python side detected the OID actually filled on-chain
// (via userFills), the apply path must clear the filled TP OID so the next cycle
// does not re-place against stale virtual qty (#604 review #1).
func TestApplyHyperliquidProtectionSyncClearsFilledExternally(t *testing.T) {
	pos := &Position{Symbol: "ETH", StopLossOID: 11, TPOIDs: []int64{22, 33}}
	applyHyperliquidProtectionSync(pos, &HyperliquidProtectionSyncResult{
		StopLossFilledExternally: true,
		TPFilledExternally:       []bool{true, false},
		// TP2 still resting in this scenario.
		TPOIDs: []int64{0, 33},
	})
	if pos.StopLossOID != 0 {
		t.Errorf("StopLossOID = %d, want 0 (cleared because filled externally)", pos.StopLossOID)
	}
	if !reflect.DeepEqual(pos.TPOIDs, []int64{0, 33}) {
		t.Errorf("TPOIDs = %v, want [0 33] (TP1 cleared because filled externally)", pos.TPOIDs)
	}
}

// #716 item 2 — applyHyperliquidProtectionSync must record TPArmedTiers[i]=true
// whenever Python returns a positive OID for tier i, so a future cycle that
// observes OID=0 there can distinguish "filled" from "never armed". A filled
// tier (TPFilledExternally=true) is also armed by definition.
func TestApplyHyperliquidProtectionSyncStampsTPArmedTiers(t *testing.T) {
	t.Run("positive OIDs stamp armed", func(t *testing.T) {
		pos := &Position{Symbol: "ETH"}
		applyHyperliquidProtectionSync(pos, &HyperliquidProtectionSyncResult{
			TPOIDs: []int64{111, 222},
		})
		if !reflect.DeepEqual(pos.TPArmedTiers, []bool{true, true}) {
			t.Errorf("TPArmedTiers = %v, want [true true]", pos.TPArmedTiers)
		}
	})

	t.Run("zero OID does not stamp armed", func(t *testing.T) {
		pos := &Position{Symbol: "ETH"}
		applyHyperliquidProtectionSync(pos, &HyperliquidProtectionSyncResult{
			TPOIDs: []int64{0, 222},
		})
		if !reflect.DeepEqual(pos.TPArmedTiers, []bool{false, true}) {
			t.Errorf("TPArmedTiers = %v, want [false true]", pos.TPArmedTiers)
		}
	})

	t.Run("armed survives later fill that zeros OID", func(t *testing.T) {
		pos := &Position{Symbol: "ETH", TPArmedTiers: []bool{true, true}}
		applyHyperliquidProtectionSync(pos, &HyperliquidProtectionSyncResult{
			TPOIDs:             []int64{0, 222},
			TPFilledExternally: []bool{true, false},
		})
		if !reflect.DeepEqual(pos.TPArmedTiers, []bool{true, true}) {
			t.Errorf("TPArmedTiers = %v, want [true true] (filled-externally implies armed)", pos.TPArmedTiers)
		}
	})

	t.Run("legacy TP1FilledExternally/TP2FilledExternally extends armed slice", func(t *testing.T) {
		pos := &Position{Symbol: "ETH"}
		applyHyperliquidProtectionSync(pos, &HyperliquidProtectionSyncResult{
			TP1OID:              33,
			TP2OID:              44,
			TP1FilledExternally: true,
		})
		if len(pos.TPArmedTiers) != 2 || !pos.TPArmedTiers[0] || !pos.TPArmedTiers[1] {
			t.Errorf("TPArmedTiers = %v, want [true true]", pos.TPArmedTiers)
		}
	})
}

func TestFilterCloseStrategiesForHLOnChainProtection(t *testing.T) {
	mult := 1.0
	hlLiveArgs := []string{"bollinger_bands", "ETH", "30m", "--mode=live"}
	hlPaperArgs := []string{"bollinger_bands", "ETH", "30m", "--mode=paper"}
	cases := []struct {
		name     string
		sc       StrategyConfig
		expected []string
	}{
		{
			name: "tiered_tp_atr_live filtered when TP plan emitted",
			sc: StrategyConfig{
				Args:            hlLiveArgs,
				Type:            "perps",
				Platform:        "hyperliquid",
				StopLossATRMult: &mult,
				CloseStrategies: []StrategyRef{{Name: "tiered_tp_atr_live"}, {Name: "tp_at_pct"}},
			},
			expected: []string{"tp_at_pct"},
		},
		{
			name: "manual tiered_tp_atr_live filtered when TP plan emitted",
			sc: StrategyConfig{
				Args:            hlLiveArgs,
				Type:            "manual",
				Platform:        "hyperliquid",
				StopLossATRMult: &mult,
				CloseStrategies: []StrategyRef{{Name: "tiered_tp_atr_live"}, {Name: "tp_at_pct"}},
			},
			expected: []string{"tp_at_pct"},
		},
		{
			name: "no filter when no on-chain TPs (no tiered close strategy)",
			sc: StrategyConfig{
				Type:            "perps",
				Platform:        "hyperliquid",
				StopLossATRMult: &mult,
				CloseStrategies: []StrategyRef{{Name: "tp_at_pct"}},
			},
			expected: []string{"tp_at_pct"},
		},
		{
			name: "non-perps untouched",
			sc: StrategyConfig{
				Type:            "spot",
				Platform:        "hyperliquid",
				CloseStrategies: []StrategyRef{{Name: "tiered_tp_atr_live"}},
			},
			expected: []string{"tiered_tp_atr_live"},
		},
		{
			name: "tiered_tp_atr also filtered",
			sc: StrategyConfig{
				Args:            hlLiveArgs,
				Type:            "perps",
				Platform:        "hyperliquid",
				StopLossATRMult: &mult,
				CloseStrategies: []StrategyRef{{Name: "tiered_tp_atr"}},
			},
			expected: []string{},
		},
		{
			name: "paper tiered_tp_atr not filtered (#781)",
			sc: StrategyConfig{
				Args:            hlPaperArgs,
				Type:            "perps",
				Platform:        "hyperliquid",
				StopLossATRMult: &mult,
				CloseStrategies: []StrategyRef{{Name: "tiered_tp_atr"}, {Name: "tp_at_pct"}},
			},
			expected: []string{"tiered_tp_atr", "tp_at_pct"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := filterCloseStrategiesForHLOnChainProtection(tc.sc)
			if len(got) != len(tc.expected) {
				t.Fatalf("filtered = %v, want %v", got, tc.expected)
			}
			for i, ref := range got {
				if ref.Name != tc.expected[i] {
					t.Errorf("filtered[%d] = %q, want %q", i, ref.Name, tc.expected[i])
				}
			}
		})
	}
}

// TestCloseStrategiesSuppressedMatchesTieredTPATRClose enforces that
// closeStrategiesSuppressedByOnChainProtection and strategyUsesTieredTPATRClose
// stay in sync. If a new ATR-tiered close evaluator is added to the suppression
// set without updating strategyUsesTieredTPATRClose (or vice versa), this test
// fails immediately.
func TestCloseStrategiesSuppressedMatchesTieredTPATRClose(t *testing.T) {
	// Every name in the suppression set must be recognized by strategyUsesTieredTPATRClose.
	for name := range closeStrategiesSuppressedByOnChainProtection {
		sc := StrategyConfig{CloseStrategies: []StrategyRef{{Name: name}}}
		if !strategyUsesTieredTPATRClose(sc) {
			t.Errorf("strategyUsesTieredTPATRClose returned false for %q, which is in closeStrategiesSuppressedByOnChainProtection — add it to strategyUsesTieredTPATRClose", name)
		}
	}

	// A config with only non-suppressed close strategies must return false.
	sc := StrategyConfig{CloseStrategies: []StrategyRef{{Name: "tp_at_pct"}, {Name: "tiered_tp_pct"}}}
	if strategyUsesTieredTPATRClose(sc) {
		t.Error("strategyUsesTieredTPATRClose returned true for non-suppressed close strategies")
	}
}

func TestFloatFromAnyCheckedRejectsStrings(t *testing.T) {
	if _, err := floatFromAnyChecked("1.5"); err == nil {
		t.Error("expected error for string input, got nil")
	}
	if _, err := floatFromAnyChecked(nil); err == nil {
		t.Error("expected error for nil input, got nil")
	}
	if _, err := floatFromAnyChecked(true); err == nil {
		t.Error("expected error for bool input, got nil")
	}
	if v, err := floatFromAnyChecked(1.5); err != nil || v != 1.5 {
		t.Errorf("float64 1.5: got (%g, %v), want (1.5, nil)", v, err)
	}
	if v, err := floatFromAnyChecked(2); err != nil || v != 2 {
		t.Errorf("int 2: got (%g, %v), want (2, nil)", v, err)
	}
}

func TestParseHLProtectionTiersSkipsInvalidValues(t *testing.T) {
	raw := []interface{}{
		map[string]interface{}{"atr_multiple": "1.5", "close_fraction": 0.5}, // string rejected
		map[string]interface{}{"atr_multiple": 2.0, "close_fraction": 1.0},
	}
	tiers := parseHLProtectionTiers(raw)
	if len(tiers) != 1 {
		t.Fatalf("len(tiers) = %d, want 1 (string-typed tier should be skipped)", len(tiers))
	}
	if tiers[0].Multiple != 2 || tiers[0].Fraction != 1 {
		t.Errorf("surviving tier = (%g, %g), want (2, 1)", tiers[0].Multiple, tiers[0].Fraction)
	}
}

func TestBuildHyperliquidProtectionPlanCustomTiers(t *testing.T) {
	mult := 1.25
	sc := StrategyConfig{
		Type:            "perps",
		Platform:        "hyperliquid",
		StopLossATRMult: &mult,
		CloseStrategies: []StrategyRef{{Name: "tiered_tp_atr_live", Params: map[string]interface{}{
			"tiers": []interface{}{
				map[string]interface{}{"atr_multiple": 3.0, "close_fraction": 1.0},
				map[string]interface{}{"atr_multiple": 2.0, "close_fraction": 0.4},
			},
		}}},
	}
	pos := &Position{Symbol: "ETH", Quantity: 1, AvgCost: 2500, EntryATR: 25, Side: "short"}
	plan, ok := buildHyperliquidProtectionPlan(sc, pos)
	if !ok {
		t.Fatal("buildHyperliquidProtectionPlan returned ok=false")
	}
	wantTiers := []hlProtectionTier{{Multiple: 2, Fraction: 0.4}, {Multiple: 3, Fraction: 1}}
	if !reflect.DeepEqual(plan.Tiers, wantTiers) {
		t.Errorf("custom tiers = %+v, want %+v", plan.Tiers, wantTiers)
	}
}

func TestBuildHyperliquidProtectionPlanThreeTiers(t *testing.T) {
	mult := 1.0
	sc := StrategyConfig{
		Type:            "perps",
		Platform:        "hyperliquid",
		StopLossATRMult: &mult,
		CloseStrategies: []StrategyRef{{Name: "tiered_tp_atr_live", Params: map[string]interface{}{
			"tiers": []interface{}{
				map[string]interface{}{"atr_multiple": 1.0, "close_fraction": 0.5},
				map[string]interface{}{"atr_multiple": 2.0, "close_fraction": 0.8},
				map[string]interface{}{"atr_multiple": 3.0, "close_fraction": 1.0},
			},
		}}},
	}
	pos := &Position{
		Symbol:   "ETH",
		Quantity: 1,
		AvgCost:  2500,
		EntryATR: 25,
		Side:     "long",
		TPOIDs:   []int64{101, 202, 303},
	}
	plan, ok := buildHyperliquidProtectionPlan(sc, pos)
	if !ok {
		t.Fatal("buildHyperliquidProtectionPlan returned ok=false")
	}
	wantTiers := []hlProtectionTier{
		{Multiple: 1, Fraction: 0.5},
		{Multiple: 2, Fraction: 0.8},
		{Multiple: 3, Fraction: 1},
	}
	if !reflect.DeepEqual(plan.Tiers, wantTiers) {
		t.Errorf("tiers = %+v, want %+v", plan.Tiers, wantTiers)
	}
	if !reflect.DeepEqual(plan.TPOIDs, []int64{101, 202, 303}) {
		t.Errorf("TP OIDs = %v, want [101 202 303]", plan.TPOIDs)
	}
}

func TestHyperliquidProtectionTiersCoercesFinalTierToFullCoverage(t *testing.T) {
	sc := StrategyConfig{
		Type:     "perps",
		Platform: "hyperliquid",
		CloseStrategies: []StrategyRef{{Name: "tiered_tp_atr_live", Params: map[string]interface{}{
			"tiers": []interface{}{
				map[string]interface{}{"atr_multiple": 1.0, "close_fraction": 0.5},
				map[string]interface{}{"atr_multiple": 2.0, "close_fraction": 0.7},
			},
		}}},
	}
	want := []hlProtectionTier{{Multiple: 1, Fraction: 0.5}, {Multiple: 2, Fraction: 1}}
	if got := strategyTPTiers(sc); !reflect.DeepEqual(got, want) {
		t.Errorf("tiers = %+v, want %+v", got, want)
	}
}

func TestHyperliquidProtectionTiersRejectsNonIncreasingAfterSort(t *testing.T) {
	sc := StrategyConfig{
		Type:     "perps",
		Platform: "hyperliquid",
		CloseStrategies: []StrategyRef{{Name: "tiered_tp_atr_live", Params: map[string]interface{}{
			"tiers": []interface{}{
				map[string]interface{}{"atr_multiple": 1.0, "close_fraction": 0.5},
				map[string]interface{}{"atr_multiple": 0.5, "close_fraction": 0.7},
			},
		}}},
	}
	if got := strategyTPTiers(sc); len(got) != 0 {
		t.Errorf("tiers = %+v, want nil/empty for non-increasing sorted fractions", got)
	}
}

func TestHyperliquidProtectionTiersPreservesDuplicateMultipleOrder(t *testing.T) {
	sc := StrategyConfig{
		Type:     "perps",
		Platform: "hyperliquid",
		CloseStrategies: []StrategyRef{{Name: "tiered_tp_atr_live", Params: map[string]interface{}{
			"tiers": []interface{}{
				map[string]interface{}{"atr_multiple": 1.0, "close_fraction": 0.4},
				map[string]interface{}{"atr_multiple": 1.0, "close_fraction": 0.6},
				map[string]interface{}{"atr_multiple": 2.0, "close_fraction": 0.9},
			},
		}}},
	}
	want := []hlProtectionTier{
		{Multiple: 1, Fraction: 0.4},
		{Multiple: 1, Fraction: 0.6},
		{Multiple: 2, Fraction: 1},
	}
	if got := strategyTPTiers(sc); !reflect.DeepEqual(got, want) {
		t.Errorf("tiers = %+v, want stable duplicate-multiple order %+v", got, want)
	}
}

// withStubbedSyncHyperliquidProtection swaps in a fake protection sync for the
// duration of the test, restoring the original on cleanup.
func withStubbedSyncHyperliquidProtection(
	t *testing.T,
	stub func(sc StrategyConfig, plan hlProtectionPlan, notifier *MultiNotifier, logger *StrategyLogger, reconcileFillHintsJSON []byte) (*HyperliquidProtectionSyncResult, bool),
) {
	t.Helper()
	orig := syncHyperliquidProtection
	syncHyperliquidProtection = stub
	t.Cleanup(func() { syncHyperliquidProtection = orig })
}

func TestRunHyperliquidProtectionSyncManualAppliesOIDs(t *testing.T) {
	mult := 1.5
	sc := StrategyConfig{
		ID:              "hl-manual-eth",
		Type:            "manual",
		Platform:        "hyperliquid",
		CloseStrategies: []StrategyRef{{Name: "tiered_tp_atr_live"}},
		StopLossATRMult: &mult,
	}
	state := &StrategyState{
		Positions: map[string]*Position{
			"ETH": {Symbol: "ETH", Quantity: 0.4, AvgCost: 3000, EntryATR: 100, Side: "long"},
		},
	}
	calls := 0
	withStubbedSyncHyperliquidProtection(t, func(_ StrategyConfig, _ hlProtectionPlan, _ *MultiNotifier, _ *StrategyLogger, _ []byte) (*HyperliquidProtectionSyncResult, bool) {
		calls++
		return &HyperliquidProtectionSyncResult{
			StopLossOID: 999,
			TPOIDs:      []int64{111, 222},
		}, true
	})

	var mu sync.RWMutex
	if !runHyperliquidProtectionSync(sc, state, nil, "ETH", &mu, nil, nil, "test", nil) {
		t.Fatal("expected runHyperliquidProtectionSync to apply")
	}
	if calls != 1 {
		t.Errorf("syncHyperliquidProtection calls = %d, want 1", calls)
	}
	pos := state.Positions["ETH"]
	if pos.StopLossOID != 999 {
		t.Errorf("StopLossOID = %d, want 999", pos.StopLossOID)
	}
	if !reflect.DeepEqual(pos.TPOIDs, []int64{111, 222}) {
		t.Errorf("TPOIDs = %v, want [111 222]", pos.TPOIDs)
	}
}

// TestRunHyperliquidProtectionSyncSkipsWhenNoPlan verifies the early exit when
// buildHyperliquidProtectionPlan returns ok=false (e.g. position EntryATR=0).
// The subprocess MUST NOT run.
func TestRunHyperliquidProtectionSyncSkipsWhenNoPlan(t *testing.T) {
	sc := StrategyConfig{
		ID: "hl-manual-eth", Type: "manual", Platform: "hyperliquid",
	}
	state := &StrategyState{
		Positions: map[string]*Position{
			"ETH": {Symbol: "ETH", Quantity: 0.4, AvgCost: 3000, EntryATR: 0, Side: "long"},
		},
	}
	called := false
	withStubbedSyncHyperliquidProtection(t, func(_ StrategyConfig, _ hlProtectionPlan, _ *MultiNotifier, _ *StrategyLogger, _ []byte) (*HyperliquidProtectionSyncResult, bool) {
		called = true
		return nil, false
	})

	var mu sync.RWMutex
	if runHyperliquidProtectionSync(sc, state, nil, "ETH", &mu, nil, nil, "test", nil) {
		t.Fatal("expected runHyperliquidProtectionSync to skip when no plan")
	}
	if called {
		t.Fatal("syncHyperliquidProtection must not be called when build returns ok=false")
	}
}

// TestRunHyperliquidProtectionSyncSkipsApplyAfterExternalClose verifies the
// post-subprocess re-validation: if the position was flattened or flipped
// while the subprocess was in flight, the OID apply MUST be skipped (otherwise
// we'd write protection OIDs onto state that no longer matches the on-chain
// position).
func TestRunHyperliquidProtectionSyncSkipsApplyAfterExternalClose(t *testing.T) {
	mult := 1.5
	sc := StrategyConfig{
		ID:              "hl-manual-eth",
		Type:            "manual",
		Platform:        "hyperliquid",
		CloseStrategies: []StrategyRef{{Name: "tiered_tp_atr_live"}},
		StopLossATRMult: &mult,
	}
	state := &StrategyState{
		Positions: map[string]*Position{
			"ETH": {Symbol: "ETH", Quantity: 0.4, AvgCost: 3000, EntryATR: 100, Side: "long"},
		},
	}
	withStubbedSyncHyperliquidProtection(t, func(_ StrategyConfig, _ hlProtectionPlan, _ *MultiNotifier, _ *StrategyLogger, _ []byte) (*HyperliquidProtectionSyncResult, bool) {
		// Simulate an external close racing the subprocess.
		state.Positions["ETH"].Quantity = 0
		return &HyperliquidProtectionSyncResult{StopLossOID: 999, TPOIDs: []int64{111}}, true
	})

	var mu sync.RWMutex
	if runHyperliquidProtectionSync(sc, state, nil, "ETH", &mu, nil, nil, "test", nil) {
		t.Fatal("expected apply to be skipped after position closed externally")
	}
	pos := state.Positions["ETH"]
	if pos.StopLossOID != 0 || len(pos.TPOIDs) != 0 {
		t.Errorf("OIDs leaked into closed position: sl=%d tp=%v", pos.StopLossOID, pos.TPOIDs)
	}
}

// TestRunHyperliquidProtectionSyncStampsTradeInDB regresses #625: when
// protection sync places the SL post-open, the SQLite trade row's
// stop_loss_trigger_px must be backfilled (not just the in-memory TradeHistory).
func TestRunHyperliquidProtectionSyncStampsTradeInDB(t *testing.T) {
	mult := 1.5
	sc := StrategyConfig{
		ID:              "hl-eth",
		Type:            "perps",
		Platform:        "hyperliquid",
		CloseStrategies: []StrategyRef{{Name: "tiered_tp_atr_live"}},
		StopLossATRMult: &mult,
	}
	ts := time.Date(2026, 5, 6, 12, 0, 0, 0, time.UTC)
	state := &StrategyState{
		ID: sc.ID,
		Positions: map[string]*Position{
			"ETH": {Symbol: "ETH", Quantity: 0.4, AvgCost: 3000, EntryATR: 100, Side: "long"},
		},
		TradeHistory: []Trade{
			{Symbol: "ETH", IsClose: false, Timestamp: ts},
		},
	}
	db, err := OpenStateDB(":memory:")
	if err != nil {
		t.Fatalf("OpenStateDB: %v", err)
	}
	defer db.Close()
	if err := db.InsertTrade(state.ID, state.TradeHistory[0]); err != nil {
		t.Fatalf("InsertTrade: %v", err)
	}

	withStubbedSyncHyperliquidProtection(t, func(_ StrategyConfig, _ hlProtectionPlan, _ *MultiNotifier, _ *StrategyLogger, _ []byte) (*HyperliquidProtectionSyncResult, bool) {
		return &HyperliquidProtectionSyncResult{
			StopLossOID:       999,
			StopLossTriggerPx: 2850.0,
			TPOIDs:            []int64{111, 222},
		}, true
	})

	var mu sync.RWMutex
	if !runHyperliquidProtectionSync(sc, state, db, "ETH", &mu, nil, nil, "test", nil) {
		t.Fatal("expected runHyperliquidProtectionSync to apply")
	}

	if got := state.TradeHistory[0].StopLossTriggerPx; got != 2850.0 {
		t.Errorf("in-memory StopLossTriggerPx = %v, want 2850", got)
	}
	var stopLossTriggerPx float64
	if err := db.db.QueryRow(
		`SELECT stop_loss_trigger_px FROM trades WHERE strategy_id = ? AND timestamp = ?`,
		state.ID, formatTime(ts),
	).Scan(&stopLossTriggerPx); err != nil {
		t.Fatalf("query stamped trade: %v", err)
	}
	if stopLossTriggerPx != 2850.0 {
		t.Errorf("persisted stop_loss_trigger_px = %v, want 2850", stopLossTriggerPx)
	}
}

func TestBuildHyperliquidProtectionPlanPadsTPArmedTiers(t *testing.T) {
	mult := 1.5
	sc := StrategyConfig{
		ID:              "hl-eth",
		Type:            "perps",
		Platform:        "hyperliquid",
		StopLossATRMult: &mult,
		CloseStrategies: []StrategyRef{{Name: "tiered_tp_atr_live"}},
	}
	pos := &Position{
		Symbol:       "ETH",
		Quantity:     0.22,
		AvgCost:      3000,
		EntryATR:     100,
		Side:         "long",
		TPOIDs:       []int64{0, 300},
		TPArmedTiers: []bool{true, true},
	}
	plan, ok := buildHyperliquidProtectionPlan(sc, pos)
	if !ok {
		t.Fatal("expected plan ok=true")
	}
	if want := []bool{true, true}; !reflect.DeepEqual(plan.TPArmedTiers, want) {
		t.Errorf("TPArmedTiers = %v, want %v", plan.TPArmedTiers, want)
	}
	if want := []int64{0, 300}; !reflect.DeepEqual(plan.TPOIDs, want) {
		t.Errorf("TPOIDs = %v, want %v", plan.TPOIDs, want)
	}
	// Shorter TPArmedTiers slice pads with false (#749 / #716 contract).
	pos.TPArmedTiers = []bool{true}
	plan, ok = buildHyperliquidProtectionPlan(sc, pos)
	if !ok {
		t.Fatal("expected plan ok=true (padded armed tiers)")
	}
	if want := []bool{true, false}; !reflect.DeepEqual(plan.TPArmedTiers, want) {
		t.Errorf("padded TPArmedTiers = %v, want %v", plan.TPArmedTiers, want)
	}
}

func TestHyperliquidProtectionTiersRejectsSingleTier(t *testing.T) {
	sc := StrategyConfig{
		Type:     "perps",
		Platform: "hyperliquid",
		CloseStrategies: []StrategyRef{{Name: "tiered_tp_atr_live", Params: map[string]interface{}{
			"tiers": []interface{}{
				map[string]interface{}{"atr_multiple": 1.0, "close_fraction": 1.0},
			},
		}}},
	}
	if got := strategyTPTiers(sc); len(got) != 0 {
		t.Errorf("tiers = %+v, want nil/empty for single-tier config", got)
	}
}

func TestHyperliquidPlacesOnChainTPs_RegimeAwareWithoutStampedRegime(t *testing.T) {
	sc := StrategyConfig{
		Args:     []string{"bollinger_bands", "ETH", "30m", "--mode=live"},
		Type:     "perps",
		Platform: "hyperliquid",
		CloseStrategies: []StrategyRef{{
			Name:   "tiered_tp_atr_regime",
			Params: map[string]interface{}{"use_defaults": true},
		}},
	}
	if len(strategyTPTiers(sc)) != 0 {
		t.Fatalf("strategyTPTiers(sc) should be nil before regime is stamped, got %#v", strategyTPTiers(sc))
	}
	if !hyperliquidPlacesOnChainTPs(sc) {
		t.Fatal("hyperliquidPlacesOnChainTPs must be true for regime tiered TP so HL on-chain suppression/filter gates apply (#750)")
	}
}

func TestHyperliquidPlacesOnChainTPs_ScalarTiered(t *testing.T) {
	sc := StrategyConfig{
		Args:            []string{"bollinger_bands", "ETH", "30m", "--mode=live"},
		Type:            "perps",
		Platform:        "hyperliquid",
		CloseStrategies: []StrategyRef{{Name: "tiered_tp_atr"}},
	}
	if !hyperliquidPlacesOnChainTPs(sc) {
		t.Fatal("expected true for scalar tiered_tp_atr in live mode")
	}
}

func TestHyperliquidPlacesOnChainTPs_PaperFalse(t *testing.T) {
	sc := StrategyConfig{
		Args:            []string{"bollinger_bands", "ETH", "30m", "--mode=paper"},
		Type:            "perps",
		Platform:        "hyperliquid",
		CloseStrategies: []StrategyRef{{Name: "tiered_tp_atr"}},
	}
	if hyperliquidPlacesOnChainTPs(sc) {
		t.Fatal("paper HL perps must not place on-chain TPs (#781)")
	}
}

func TestStrategyConfigWithOnChainProtectionFilter_PaperKeepsTieredTP(t *testing.T) {
	sc := StrategyConfig{
		Args: []string{"bollinger_bands", "ETH", "30m", "--mode=paper"},
		Type: "perps", Platform: "hyperliquid",
		OpenStrategy: StrategyRef{Name: "bollinger_bands"},
		CloseStrategies: []StrategyRef{{
			Name: "tiered_tp_atr",
			Params: map[string]interface{}{
				"tiers": []interface{}{
					map[string]interface{}{"atr_multiple": 2.0, "close_fraction": 0.5},
					map[string]interface{}{"atr_multiple": 3.0, "close_fraction": 1.0},
				},
			},
		}},
	}
	filtered := strategyConfigWithOnChainProtectionFilter(sc)
	if len(filtered.CloseStrategies) != 1 || filtered.CloseStrategies[0].Name != "tiered_tp_atr" {
		t.Fatalf("paper close strategies = %#v, want tiered_tp_atr retained", filtered.CloseStrategies)
	}
	got, err := buildStrategyRefsArg(filtered)
	if err != nil {
		t.Fatalf("buildStrategyRefsArg: %v", err)
	}
	if len(got) != 2 || got[0] != "--strategy-refs" {
		t.Fatalf("got %#v, want --strategy-refs", got)
	}
	if !strings.Contains(got[1], `"tiered_tp_atr"`) {
		t.Fatalf("strategy-refs missing tiered_tp_atr close: %s", got[1])
	}
}

func TestTieredTPATRPricesForRegimeUsesFleetDefaults(t *testing.T) {
	sc := StrategyConfig{
		Platform: "hyperliquid",
		Type:     "perps",
		CloseStrategies: []StrategyRef{{
			Name:   "tiered_tp_atr_regime",
			Params: map[string]interface{}{"use_defaults": true},
		}},
	}
	got := tieredTPATRPricesForRegime(sc, "long", 100, 10, "trending_up")
	want := []float64{120, 140} // 2× and 4× ATR @ trending_up fleet baseline
	if len(got) != len(want) {
		t.Fatalf("len(prices)=%d, want %d; got=%v", len(got), len(want), got)
	}
	for i := range want {
		if math.Abs(got[i]-want[i]) > 1e-9 {
			t.Errorf("prices[%d]=%g, want %g (full %v)", i, got[i], want[i], got)
		}
	}
	if empty := tieredTPATRPricesForRegime(sc, "long", 100, 10, ""); len(empty) != 0 {
		t.Errorf("empty regime should yield no TP prices, got %v", empty)
	}
}

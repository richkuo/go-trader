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
		CloseStrategy:   &StrategyRef{Name: "tiered_tp_atr_live"},
	}
	pos := &Position{
		Symbol:   "ETH",
		Quantity: 2,
		AvgCost:  3000,
		EntryATR: 50,
		Side:     "long",
		TPOIDs:   []int64{101, 202},
	}
	plan, ok := buildHyperliquidProtectionPlan(sc, pos, 0)
	if !ok {
		t.Fatal("buildHyperliquidProtectionPlan returned ok=false")
	}
	if plan.StopLossATRMult != 1 {
		t.Errorf("StopLossATRMult = %g, want 1", plan.StopLossATRMult)
	}
	wantTiers := []hlProtectionTier{{Multiple: 1.5, Fraction: 0.4}, {Multiple: 3, Fraction: 0.8}, {Multiple: 5, Fraction: 1}}
	if !reflect.DeepEqual(plan.Tiers, wantTiers) {
		t.Errorf("tiers = %+v, want %+v", plan.Tiers, wantTiers)
	}
	if !reflect.DeepEqual(plan.TPOIDs, []int64{101, 202, 0}) {
		t.Errorf("TP OIDs = %v, want [101 202 0]", plan.TPOIDs)
	}
}

func TestBuildHyperliquidProtectionPlanManualStrategy(t *testing.T) {
	mult := 1.5
	sc := StrategyConfig{
		ID:              "hl-manual-eth",
		Type:            "manual",
		Platform:        "hyperliquid",
		CloseStrategy:   &StrategyRef{Name: "tiered_tp_atr_live"},
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
	plan, ok := buildHyperliquidProtectionPlan(sc, pos, 0)
	if !ok {
		t.Fatal("buildHyperliquidProtectionPlan returned ok=false for manual strategy")
	}
	if plan.Symbol != "ETH" || plan.Size != 0.4 || plan.StopLossATRMult != 1.5 {
		t.Errorf("manual plan = %+v", plan)
	}
	if !reflect.DeepEqual(plan.TPOIDs, []int64{456, 789, 0}) {
		t.Errorf("manual TP OIDs = %v, want [456 789 0]", plan.TPOIDs)
	}
}

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
	applyHyperliquidProtectionSync(pos, result, nil)
	if pos.StopLossOID != 100 || !reflect.DeepEqual(pos.TPOIDs, []int64{200, 300}) {
		t.Errorf("OIDs mutated: SL=%d TPs=%v, want 100/[200 300]", pos.StopLossOID, pos.TPOIDs)
	}
	if pos.StopLossTriggerPx != 2900 {
		t.Errorf("StopLossTriggerPx = %g, want 2900", pos.StopLossTriggerPx)
	}
}

func TestApplyHyperliquidProtectionSyncRetainsOnZeroFields(t *testing.T) {
	pos := &Position{Symbol: "ETH", StopLossOID: 11, TPOIDs: []int64{22, 33}}
	applyHyperliquidProtectionSync(pos, &HyperliquidProtectionSyncResult{
		OpenOrderCheckError: "indexer down",
	}, nil)
	if pos.StopLossOID != 11 || !reflect.DeepEqual(pos.TPOIDs, []int64{22, 33}) {
		t.Errorf("zero-field result mutated OIDs: SL=%d TPs=%v, want 11/[22 33]", pos.StopLossOID, pos.TPOIDs)
	}
}

func TestApplyHyperliquidProtectionSyncClearsFilledExternally(t *testing.T) {
	pos := &Position{Symbol: "ETH", StopLossOID: 11, TPOIDs: []int64{22, 33}}
	applyHyperliquidProtectionSync(pos, &HyperliquidProtectionSyncResult{
		StopLossFilledExternally: true,
		TPFilledExternally:       []bool{true, false},
		TPOIDs:                   []int64{0, 33},
	}, nil)
	if pos.StopLossOID != 0 {
		t.Errorf("StopLossOID = %d, want 0 (cleared because filled externally)", pos.StopLossOID)
	}
	if !reflect.DeepEqual(pos.TPOIDs, []int64{0, 33}) {
		t.Errorf("TPOIDs = %v, want [0 33] (TP1 cleared because filled externally)", pos.TPOIDs)
	}
}

func TestApplyHyperliquidProtectionSyncStampsTPArmedTiers(t *testing.T) {
	t.Run("positive OIDs stamp armed", func(t *testing.T) {
		pos := &Position{Symbol: "ETH"}
		applyHyperliquidProtectionSync(pos, &HyperliquidProtectionSyncResult{
			TPOIDs: []int64{111, 222},
		}, nil)
		if !reflect.DeepEqual(pos.TPArmedTiers, []bool{true, true}) {
			t.Errorf("TPArmedTiers = %v, want [true true]", pos.TPArmedTiers)
		}
	})

	t.Run("zero OID does not stamp armed", func(t *testing.T) {
		pos := &Position{Symbol: "ETH"}
		applyHyperliquidProtectionSync(pos, &HyperliquidProtectionSyncResult{
			TPOIDs: []int64{0, 222},
		}, nil)
		if !reflect.DeepEqual(pos.TPArmedTiers, []bool{false, true}) {
			t.Errorf("TPArmedTiers = %v, want [false true]", pos.TPArmedTiers)
		}
	})

	t.Run("armed survives later fill that zeros OID", func(t *testing.T) {
		pos := &Position{Symbol: "ETH", TPArmedTiers: []bool{true, true}}
		applyHyperliquidProtectionSync(pos, &HyperliquidProtectionSyncResult{
			TPOIDs:             []int64{0, 222},
			TPFilledExternally: []bool{true, false},
		}, nil)
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
		}, nil)
		if len(pos.TPArmedTiers) != 2 || !pos.TPArmedTiers[0] || !pos.TPArmedTiers[1] {
			t.Errorf("TPArmedTiers = %v, want [true true]", pos.TPArmedTiers)
		}
	})
}

func TestApplySurplusTPCancelOutcome(t *testing.T) {
	t.Run("re-appends failed surplus OID", func(t *testing.T) {
		pos := &Position{Symbol: "ETH", TPOIDs: []int64{10, 20}, TPArmedTiers: []bool{true, true}}
		applyHyperliquidProtectionSync(pos, &HyperliquidProtectionSyncResult{
			TPOIDs:             []int64{10, 20},
			TPCancelFailedOIDs: []int64{303},
		}, []int64{303})
		if !reflect.DeepEqual(pos.TPOIDs, []int64{10, 20, 303}) {
			t.Errorf("TPOIDs = %v, want [10 20 303]", pos.TPOIDs)
		}
		if len(pos.TPArmedTiers) != 3 || !pos.TPArmedTiers[2] {
			t.Errorf("TPArmedTiers = %v, want third tier armed", pos.TPArmedTiers)
		}
	})

	t.Run("does not duplicate OID already present", func(t *testing.T) {
		pos := &Position{Symbol: "ETH", TPOIDs: []int64{10, 20, 303}}
		applySurplusTPCancelOutcome(pos, &HyperliquidProtectionSyncResult{
			TPCancelFailedOIDs: []int64{303},
		}, []int64{303})
		if !reflect.DeepEqual(pos.TPOIDs, []int64{10, 20, 303}) {
			t.Errorf("TPOIDs = %v, want unchanged [10 20 303]", pos.TPOIDs)
		}
	})

	t.Run("clears successfully canceled surplus OID", func(t *testing.T) {
		pos := &Position{Symbol: "ETH", TPOIDs: []int64{10, 20, 303}, TPArmedTiers: []bool{true, true, true}}
		applySurplusTPCancelOutcome(pos, &HyperliquidProtectionSyncResult{}, []int64{303})
		if !reflect.DeepEqual(pos.TPOIDs, []int64{10, 20, 0}) {
			t.Errorf("TPOIDs = %v, want [10 20 0]", pos.TPOIDs)
		}
		if !pos.TPArmedTiers[2] {
			t.Errorf("surplus slot should stay armed after clear")
		}
	})

	t.Run("clears filled surplus OID", func(t *testing.T) {
		pos := &Position{Symbol: "ETH", TPOIDs: []int64{10, 20, 303}, TPArmedTiers: []bool{true, true, true}}
		applySurplusTPCancelOutcome(pos, &HyperliquidProtectionSyncResult{
			TPCancelFilledOIDs: []int64{303},
		}, []int64{303})
		if !reflect.DeepEqual(pos.TPOIDs, []int64{10, 20, 0}) {
			t.Errorf("TPOIDs = %v, want [10 20 0]", pos.TPOIDs)
		}
	})
}

func TestStrategyConfigWithOnChainProtectionFilter(t *testing.T) {
	mult := 1.0
	hlLiveArgs := []string{"bollinger_bands", "ETH", "30m", "--mode=live"}
	hlPaperArgs := []string{"bollinger_bands", "ETH", "30m", "--mode=paper"}
	cases := []struct {
		name    string
		sc      StrategyConfig
		dropped bool
	}{
		{
			name: "tiered_tp_atr_live live → dropped (placed on-chain)",
			sc: StrategyConfig{
				Args: hlLiveArgs, Type: "perps", Platform: "hyperliquid",
				StopLossATRMult: &mult,
				CloseStrategy:   &StrategyRef{Name: "tiered_tp_atr_live"},
			},
			dropped: true,
		},
		{
			name: "manual tiered_tp_atr_live live → dropped",
			sc: StrategyConfig{
				Args: hlLiveArgs, Type: "manual", Platform: "hyperliquid",
				StopLossATRMult: &mult,
				CloseStrategy:   &StrategyRef{Name: "tiered_tp_atr_live"},
			},
			dropped: true,
		},
		{
			name: "tiered_tp_atr live → dropped",
			sc: StrategyConfig{
				Args: hlLiveArgs, Type: "perps", Platform: "hyperliquid",
				StopLossATRMult: &mult,
				CloseStrategy:   &StrategyRef{Name: "tiered_tp_atr"},
			},
			dropped: true,
		},
		{
			name: "non-tiered close (tp_at_pct) → kept",
			sc: StrategyConfig{
				Type: "perps", Platform: "hyperliquid",
				StopLossATRMult: &mult,
				CloseStrategy:   &StrategyRef{Name: "tp_at_pct"},
			},
			dropped: false,
		},
		{
			name: "non-perps (spot) → kept",
			sc: StrategyConfig{
				Type: "spot", Platform: "hyperliquid",
				CloseStrategy: &StrategyRef{Name: "tiered_tp_atr_live"},
			},
			dropped: false,
		},
		{
			name: "paper tiered_tp_atr → kept (#781)",
			sc: StrategyConfig{
				Args: hlPaperArgs, Type: "perps", Platform: "hyperliquid",
				StopLossATRMult: &mult,
				CloseStrategy:   &StrategyRef{Name: "tiered_tp_atr"},
			},
			dropped: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := strategyConfigWithOnChainProtectionFilter(tc.sc)
			if tc.dropped {
				if got.CloseStrategy != nil {
					t.Fatalf("close ref = %v, want nil (dropped for on-chain TP)", got.CloseStrategy)
				}
				return
			}
			if got.CloseStrategy == nil || tc.sc.CloseStrategy == nil ||
				got.CloseStrategy.Name != tc.sc.CloseStrategy.Name {
				t.Fatalf("close ref = %v, want %v retained", got.CloseStrategy, tc.sc.CloseStrategy)
			}
		})
	}
}

func TestCloseStrategiesSuppressedMatchesTieredTPATRClose(t *testing.T) {
	for name := range closeStrategiesSuppressedByOnChainProtection {
		sc := StrategyConfig{CloseStrategy: &StrategyRef{Name: name}}
		if !strategyUsesTieredTPATRClose(sc) {
			t.Errorf("strategyUsesTieredTPATRClose returned false for %q, which is in closeStrategiesSuppressedByOnChainProtection — add it to strategyUsesTieredTPATRClose", name)
		}
	}

	sc := StrategyConfig{CloseStrategy: &StrategyRef{Name: "tiered_tp_pct"}}
	if strategyUsesTieredTPATRClose(sc) {
		t.Error("strategyUsesTieredTPATRClose returned true for a non-suppressed close strategy")
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
		map[string]interface{}{"atr_multiple": "1.5", "close_fraction": 0.5},
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
		CloseStrategy: &StrategyRef{Name: "tiered_tp_atr_live", Params: map[string]interface{}{
			"tp_tiers": []interface{}{
				map[string]interface{}{"atr_multiple": 3.0, "close_fraction": 1.0},
				map[string]interface{}{"atr_multiple": 2.0, "close_fraction": 0.4},
			},
		}},
	}
	pos := &Position{Symbol: "ETH", Quantity: 1, AvgCost: 2500, EntryATR: 25, Side: "short"}
	plan, ok := buildHyperliquidProtectionPlan(sc, pos, 0)
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
		CloseStrategy: &StrategyRef{Name: "tiered_tp_atr_live", Params: map[string]interface{}{
			"tp_tiers": []interface{}{
				map[string]interface{}{"atr_multiple": 1.0, "close_fraction": 0.5},
				map[string]interface{}{"atr_multiple": 2.0, "close_fraction": 0.8},
				map[string]interface{}{"atr_multiple": 3.0, "close_fraction": 1.0},
			},
		}},
	}
	pos := &Position{
		Symbol:   "ETH",
		Quantity: 1,
		AvgCost:  2500,
		EntryATR: 25,
		Side:     "long",
		TPOIDs:   []int64{101, 202, 303},
	}
	plan, ok := buildHyperliquidProtectionPlan(sc, pos, 0)
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
		CloseStrategy: &StrategyRef{Name: "tiered_tp_atr_live", Params: map[string]interface{}{
			"tp_tiers": []interface{}{
				map[string]interface{}{"atr_multiple": 1.0, "close_fraction": 0.5},
				map[string]interface{}{"atr_multiple": 2.0, "close_fraction": 0.7},
			},
		}},
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
		CloseStrategy: &StrategyRef{Name: "tiered_tp_atr_live", Params: map[string]interface{}{
			"tp_tiers": []interface{}{
				map[string]interface{}{"atr_multiple": 1.0, "close_fraction": 0.5},
				map[string]interface{}{"atr_multiple": 0.5, "close_fraction": 0.7},
			},
		}},
	}
	if got := strategyTPTiers(sc); len(got) != 0 {
		t.Errorf("tiers = %+v, want nil/empty for non-increasing sorted fractions", got)
	}
}

func TestHyperliquidProtectionTiersPreservesDuplicateMultipleOrder(t *testing.T) {
	sc := StrategyConfig{
		Type:     "perps",
		Platform: "hyperliquid",
		CloseStrategy: &StrategyRef{Name: "tiered_tp_atr_live", Params: map[string]interface{}{
			"tp_tiers": []interface{}{
				map[string]interface{}{"atr_multiple": 1.0, "close_fraction": 0.4},
				map[string]interface{}{"atr_multiple": 1.0, "close_fraction": 0.6},
				map[string]interface{}{"atr_multiple": 2.0, "close_fraction": 0.9},
			},
		}},
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
		CloseStrategy:   &StrategyRef{Name: "tiered_tp_atr_live"},
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
	synced, fillPx := runHyperliquidProtectionSync(sc, state, nil, "ETH", &mu, nil, nil, "test", nil, nil, nil)
	if !synced || fillPx != 0 {
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
	if syncedNeg, _ := runHyperliquidProtectionSync(sc, state, nil, "ETH", &mu, nil, nil, "test", nil, nil, nil); syncedNeg {
		t.Fatal("expected runHyperliquidProtectionSync to skip when no plan")
	}
	if called {
		t.Fatal("syncHyperliquidProtection must not be called when build returns ok=false")
	}
}

func TestRunHyperliquidProtectionSyncSkipsApplyAfterExternalClose(t *testing.T) {
	mult := 1.5
	sc := StrategyConfig{
		ID:              "hl-manual-eth",
		Type:            "manual",
		Platform:        "hyperliquid",
		CloseStrategy:   &StrategyRef{Name: "tiered_tp_atr_live"},
		StopLossATRMult: &mult,
	}
	state := &StrategyState{
		Positions: map[string]*Position{
			"ETH": {Symbol: "ETH", Quantity: 0.4, AvgCost: 3000, EntryATR: 100, Side: "long"},
		},
	}
	withStubbedSyncHyperliquidProtection(t, func(_ StrategyConfig, _ hlProtectionPlan, _ *MultiNotifier, _ *StrategyLogger, _ []byte) (*HyperliquidProtectionSyncResult, bool) {
		state.Positions["ETH"].Quantity = 0
		return &HyperliquidProtectionSyncResult{StopLossOID: 999, TPOIDs: []int64{111}}, true
	})

	var mu sync.RWMutex
	if syncedNeg, _ := runHyperliquidProtectionSync(sc, state, nil, "ETH", &mu, nil, nil, "test", nil, nil, nil); syncedNeg {
		t.Fatal("expected apply to be skipped after position closed externally")
	}
	pos := state.Positions["ETH"]
	if pos.StopLossOID != 0 || len(pos.TPOIDs) != 0 {
		t.Errorf("OIDs leaked into closed position: sl=%d tp=%v", pos.StopLossOID, pos.TPOIDs)
	}
}

func TestRunHyperliquidProtectionSyncStampsTradeInDB(t *testing.T) {
	mult := 1.5
	sc := StrategyConfig{
		ID:              "hl-eth",
		Type:            "perps",
		Platform:        "hyperliquid",
		CloseStrategy:   &StrategyRef{Name: "tiered_tp_atr_live"},
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
	synced2, _ := runHyperliquidProtectionSync(sc, state, db, "ETH", &mu, nil, nil, "test", nil, nil, nil)
	if !synced2 {
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
		CloseStrategy:   &StrategyRef{Name: "tiered_tp_atr_live"},
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
	plan, ok := buildHyperliquidProtectionPlan(sc, pos, 0)
	if !ok {
		t.Fatal("expected plan ok=true")
	}
	if want := []bool{true, true, false}; !reflect.DeepEqual(plan.TPArmedTiers, want) {
		t.Errorf("TPArmedTiers = %v, want %v", plan.TPArmedTiers, want)
	}
	if want := []int64{0, 300, 0}; !reflect.DeepEqual(plan.TPOIDs, want) {
		t.Errorf("TPOIDs = %v, want %v", plan.TPOIDs, want)
	}
	pos.TPArmedTiers = []bool{true}
	plan, ok = buildHyperliquidProtectionPlan(sc, pos, 0)
	if !ok {
		t.Fatal("expected plan ok=true (padded armed tiers)")
	}
	if want := []bool{true, false, false}; !reflect.DeepEqual(plan.TPArmedTiers, want) {
		t.Errorf("padded TPArmedTiers = %v, want %v", plan.TPArmedTiers, want)
	}
}

func TestHyperliquidProtectionTiersRejectsSingleTier(t *testing.T) {
	sc := StrategyConfig{
		Type:     "perps",
		Platform: "hyperliquid",
		CloseStrategy: &StrategyRef{Name: "tiered_tp_atr_live", Params: map[string]interface{}{
			"tp_tiers": []interface{}{
				map[string]interface{}{"atr_multiple": 1.0, "close_fraction": 1.0},
			},
		}},
	}
	if got := strategyTPTiers(sc); len(got) != 0 {
		t.Errorf("tiers = %+v, want nil/empty for single-tier config", got)
	}
}

func TestHyperliquidPlacesOnChainTPs(t *testing.T) {
	cases := []struct {
		name          string
		mode          string
		closeStrategy *StrategyRef
		wantNoTiers   bool
		want          bool
	}{
		{
			name:          "regime tiered TP before a regime is stamped",
			mode:          "--mode=live",
			closeStrategy: &StrategyRef{Name: "tiered_tp_atr_regime", Params: map[string]interface{}{"use_defaults": true}},
			wantNoTiers:   true,
			want:          true,
		},
		{
			name:          "scalar tiered TP in live mode",
			mode:          "--mode=live",
			closeStrategy: &StrategyRef{Name: "tiered_tp_atr"},
			want:          true,
		},
		{
			name:          "paper HL perps never place on-chain TPs",
			mode:          "--mode=paper",
			closeStrategy: &StrategyRef{Name: "tiered_tp_atr"},
			want:          false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sc := StrategyConfig{
				Args:          []string{"bollinger_bands", "ETH", "30m", tc.mode},
				Type:          "perps",
				Platform:      "hyperliquid",
				CloseStrategy: tc.closeStrategy,
			}
			if tc.wantNoTiers && len(strategyTPTiers(sc)) != 0 {
				t.Fatalf("strategyTPTiers(sc) should be nil before regime is stamped, got %#v", strategyTPTiers(sc))
			}
			if got := hyperliquidPlacesOnChainTPs(sc); got != tc.want {
				t.Fatalf("hyperliquidPlacesOnChainTPs = %v, want %v (#750, #781)", got, tc.want)
			}
		})
	}
}
func TestStrategyConfigWithOnChainProtectionFilter_PaperKeepsTieredTP(t *testing.T) {
	sc := StrategyConfig{
		Args: []string{"bollinger_bands", "ETH", "30m", "--mode=paper"},
		Type: "perps", Platform: "hyperliquid",
		OpenStrategy: StrategyRef{Name: "bollinger_bands"},
		CloseStrategy: &StrategyRef{
			Name: "tiered_tp_atr",
			Params: map[string]interface{}{
				"tp_tiers": []interface{}{
					map[string]interface{}{"atr_multiple": 2.0, "close_fraction": 0.5},
					map[string]interface{}{"atr_multiple": 3.0, "close_fraction": 1.0},
				},
			},
		},
	}
	filtered := strategyConfigWithOnChainProtectionFilter(sc)
	if filtered.CloseStrategy == nil || filtered.CloseStrategy.Name != "tiered_tp_atr" {
		t.Fatalf("paper close strategy = %#v, want tiered_tp_atr retained", filtered.CloseStrategy)
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
		CloseStrategy: &StrategyRef{
			Name:   "tiered_tp_atr_regime",
			Params: map[string]interface{}{"use_defaults": true},
		},
	}
	got := tieredTPATRPricesForRegime(sc, "long", 100, 10, "trending_up")
	want := []float64{115, 130, 150}
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

func TestStrategyTPTiersForRegime_UnifiedBlock(t *testing.T) {
	sc := StrategyConfig{
		ID:       "hl-unified-eth",
		Platform: "hyperliquid",
		Type:     "perps",
		CloseStrategy: &StrategyRef{
			Name: "tiered_tp_atr_live_regime",
			Params: map[string]interface{}{
				"atr_source": "live",
				regimeClassifierKey: map[string]interface{}{
					"trending_up": map[string]interface{}{
						"stop_loss_atr": 1.5,
						"tp_tiers": []interface{}{
							map[string]interface{}{"atr_multiple": 2.0, "close_fraction": 0.5},
							map[string]interface{}{"atr_multiple": 4.0, "close_fraction": 1.0},
						},
					},
					"trending_down": map[string]interface{}{
						"tp_tiers": []interface{}{
							map[string]interface{}{"atr_multiple": 1.5, "close_fraction": 0.5},
							map[string]interface{}{"atr_multiple": 3.0, "close_fraction": 1.0},
						},
					},
					"ranging": map[string]interface{}{
						"tp_tiers": []interface{}{
							map[string]interface{}{"atr_multiple": 1.0, "close_fraction": 0.5},
							map[string]interface{}{"atr_multiple": 2.0, "close_fraction": 1.0},
						},
					},
				},
			},
		},
	}

	up := strategyTPTiersForRegime(sc, "trending_up")
	if len(up) != 2 || up[0].Multiple != 2.0 || up[1].Multiple != 4.0 {
		t.Fatalf("trending_up tiers = %+v, want [2,4]", up)
	}
	rng := strategyTPTiersForRegime(sc, "ranging")
	if len(rng) != 2 || rng[0].Multiple != 1.0 || rng[1].Multiple != 2.0 {
		t.Fatalf("ranging tiers = %+v, want [1,2]", rng)
	}
	if got := strategyTPTiersForRegime(sc, ""); got != nil {
		t.Fatalf("empty regime tiers = %+v, want nil", got)
	}
}

func TestRatchetExcludedFromOnChainTPGates(t *testing.T) {
	for _, name := range []string{"trailing_tp_ratchet", "trailing_tp_ratchet_regime"} {
		if isTieredTPATRCloseName(name) {
			t.Errorf("isTieredTPATRCloseName(%q) = true; ratchet must never be treated as a tiered-TP-ATR close", name)
		}
		sc := StrategyConfig{CloseStrategy: &StrategyRef{Name: name}}
		if strategyUsesTieredTPATRClose(sc) {
			t.Errorf("strategyUsesTieredTPATRClose = true for %q; ratchet must not arm on-chain TP tiers", name)
		}
		if _, ok := closeStrategiesSuppressedByOnChainProtection[name]; ok {
			t.Errorf("closeStrategiesSuppressedByOnChainProtection contains %q; suppressing the ratchet evaluator would disable its exit logic entirely", name)
		}
		if !isTrailingTPRatchetCloseName(name) {
			t.Errorf("isTrailingTPRatchetCloseName(%q) = false; want true", name)
		}
	}
}

func TestBuildHyperliquidProtectionPlanAnchorsToRiskAnchorPrice(t *testing.T) {
	mult := 1.0
	sc := StrategyConfig{
		ID:              "hl-eth",
		Type:            "perps",
		Platform:        "hyperliquid",
		StopLossATRMult: &mult,
		CloseStrategy:   &StrategyRef{Name: "tiered_tp_atr_live"},
	}
	pos := &Position{
		Symbol:          "ETH",
		Quantity:        3,
		AvgCost:         3050,
		RiskAnchorPrice: 2900,
		EntryATR:        50,
		Side:            "long",
	}
	plan, ok := buildHyperliquidProtectionPlan(sc, pos, 0)
	if !ok {
		t.Fatal("buildHyperliquidProtectionPlan returned ok=false")
	}
	if plan.AvgCost != 2900 {
		t.Errorf("plan.AvgCost = %g; want frozen RiskAnchorPrice 2900, not blended AvgCost 3050", plan.AvgCost)
	}
	if plan.Size != 3 {
		t.Errorf("plan.Size = %g; want re-sized total quantity 3", plan.Size)
	}
}

func TestApplyHyperliquidProtectionSyncClearsDeadSLOnCancelLandedPlaceFailed(t *testing.T) {
	pos := &Position{Symbol: "ETH", StopLossOID: 5150, StopLossTriggerPx: 2325}
	applyHyperliquidProtectionSync(pos, &HyperliquidProtectionSyncResult{
		CancelStopLossSucceeded: true,
		StopLossError:           "place_stop_loss SDK error: open order cap",
	}, nil)
	if pos.StopLossOID != 0 || pos.StopLossTriggerPx != 0 {
		t.Errorf("SL = oid %d @ %g after cancel-landed/place-failed, want both cleared", pos.StopLossOID, pos.StopLossTriggerPx)
	}
}

func TestApplyHyperliquidProtectionSyncForceReplaceSuccessUnchanged(t *testing.T) {
	pos := &Position{Symbol: "ETH", StopLossOID: 5150, StopLossTriggerPx: 2325}
	applyHyperliquidProtectionSync(pos, &HyperliquidProtectionSyncResult{
		CancelStopLossSucceeded: true,
		StopLossOID:             6000,
		StopLossTriggerPx:       2300,
	}, nil)
	if pos.StopLossOID != 6000 || pos.StopLossTriggerPx != 2300 {
		t.Errorf("SL = oid %d @ %g, want 6000 @ 2300 from the successful replacement", pos.StopLossOID, pos.StopLossTriggerPx)
	}

	pos2 := &Position{Symbol: "ETH", StopLossOID: 5150, StopLossTriggerPx: 2325}
	applyHyperliquidProtectionSync(pos2, &HyperliquidProtectionSyncResult{
		StopLossError: "force replace cancel: timeout",
	}, nil)
	if pos2.StopLossOID != 5150 || pos2.StopLossTriggerPx != 2325 {
		t.Errorf("failed-cancel result mutated SL to oid %d @ %g, want 5150 @ 2325 kept (order may still rest)", pos2.StopLossOID, pos2.StopLossTriggerPx)
	}
}

func TestHLProtectionLostExchangeStop(t *testing.T) {
	cases := []struct {
		name   string
		result *HyperliquidProtectionSyncResult
		want   bool
	}{
		{"cancel landed, place failed", &HyperliquidProtectionSyncResult{CancelStopLossSucceeded: true}, true},
		{"cancel landed, replacement rests", &HyperliquidProtectionSyncResult{CancelStopLossSucceeded: true, StopLossOID: 7}, false},
		{"cancel landed, filled at submit", &HyperliquidProtectionSyncResult{CancelStopLossSucceeded: true, StopLossFilledImmediately: true}, false},
		{"cancel failed", &HyperliquidProtectionSyncResult{}, false},
		{"nil result", nil, false},
	}
	for _, tc := range cases {
		if got := hlProtectionLostExchangeStop(tc.result); got != tc.want {
			t.Errorf("%s: got %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestRunHyperliquidProtectionSyncBooksFillAtSubmit(t *testing.T) {
	mult := 1.5
	sc := StrategyConfig{
		ID:              "hl-manual-eth",
		Type:            "manual",
		Platform:        "hyperliquid",
		CloseStrategy:   &StrategyRef{Name: "tiered_tp_atr_live"},
		StopLossATRMult: &mult,
	}
	pos := &Position{Symbol: "ETH", Quantity: 0.4, AvgCost: 3000, EntryATR: 100, Side: "long", StopLossOID: 4242, StopLossTriggerPx: 2325}
	state := &StrategyState{Positions: map[string]*Position{"ETH": pos}}
	withStubbedSyncHyperliquidProtection(t, func(_ StrategyConfig, _ hlProtectionPlan, _ *MultiNotifier, _ *StrategyLogger, _ []byte) (*HyperliquidProtectionSyncResult, bool) {
		return &HyperliquidProtectionSyncResult{
			CancelStopLossSucceeded:   true,
			StopLossFilledImmediately: true,
			StopLossTriggerPx:         2318.5,
		}, true
	})
	var mu sync.RWMutex
	synced, fillPx := runHyperliquidProtectionSync(sc, state, nil, "ETH", &mu, nil, nil, "test", nil, nil, nil)
	if !synced {
		t.Fatal("expected sync to report success after booking the submit-fill close")
	}
	if fillPx != 2318.5 {
		t.Errorf("fillPx = %g, want 2318.5 (the price that filled)", fillPx)
	}
	if _, stillOpen := state.Positions["ETH"]; stillOpen {
		t.Error("position must be gone after the submit-fill close is booked")
	}
}

func TestRunHyperliquidProtectionSyncRestingPlacementBooksNoClose(t *testing.T) {
	mult := 1.5
	sc := StrategyConfig{
		ID:              "hl-manual-eth",
		Type:            "manual",
		Platform:        "hyperliquid",
		CloseStrategy:   &StrategyRef{Name: "tiered_tp_atr_live"},
		StopLossATRMult: &mult,
	}
	state := &StrategyState{Positions: map[string]*Position{
		"ETH": {Symbol: "ETH", Quantity: 0.4, AvgCost: 3000, EntryATR: 100, Side: "long", StopLossTriggerPx: 2325},
	}}
	withStubbedSyncHyperliquidProtection(t, func(_ StrategyConfig, _ hlProtectionPlan, _ *MultiNotifier, _ *StrategyLogger, _ []byte) (*HyperliquidProtectionSyncResult, bool) {
		return &HyperliquidProtectionSyncResult{
			StopLossOID:       999,
			StopLossTriggerPx: 2300,
		}, true
	})
	var mu sync.RWMutex
	synced, fillPx := runHyperliquidProtectionSync(sc, state, nil, "ETH", &mu, nil, nil, "test", nil, nil, nil)
	if !synced || fillPx != 0 {
		t.Errorf("synced=%v fillPx=%g, want true/0 for a resting replacement", synced, fillPx)
	}
	if pos := state.Positions["ETH"]; pos == nil || pos.StopLossOID != 999 {
		t.Error("resting placement must keep the position open with the new OID")
	}
}

func TestProtectionSyncOutcomeUnknownDefersInsteadOfClearing(t *testing.T) {
	newPos := func() *Position {
		return &Position{Symbol: "ETH", Side: "long", Quantity: 1, AvgCost: 2000, EntryATR: 25, StopLossOID: 111, StopLossTriggerPx: 1850}
	}

	unknown := &HyperliquidProtectionSyncResult{CancelStopLossSucceeded: true, StopLossOutcomeUnknown: true, StopLossError: "place_stop_loss returned no usable status"}
	if hlProtectionLostExchangeStop(unknown) {
		t.Errorf("outcome-unknown classified as protection lost — that CRITICAL would be false")
	}
	if !hlProtectionStopOutcomeUnknown(unknown) {
		t.Errorf("outcome-unknown not classified as such — the operator gets no alert at all")
	}
	pos := newPos()
	applyHyperliquidProtectionSync(pos, unknown, nil)
	if pos.StopLossOID != 111 || pos.StopLossTriggerPx != 1850 {
		t.Errorf("outcome-unknown cleared recorded state: OID %d trigger %.2f, want 111 / 1850", pos.StopLossOID, pos.StopLossTriggerPx)
	}

	rejected := &HyperliquidProtectionSyncResult{CancelStopLossSucceeded: true, StopLossError: "place_stop_loss SDK error: insufficient margin"}
	if !hlProtectionLostExchangeStop(rejected) {
		t.Errorf("a positively rejected placement must still read as protection lost")
	}
	if hlProtectionStopOutcomeUnknown(rejected) {
		t.Errorf("a positively rejected placement must not read as outcome unknown")
	}
	pos = newPos()
	applyHyperliquidProtectionSync(pos, rejected, nil)
	if pos.StopLossOID != 0 || pos.StopLossTriggerPx != 0 {
		t.Errorf("rejected placement left stale state: OID %d trigger %.2f, want 0 / 0", pos.StopLossOID, pos.StopLossTriggerPx)
	}

	rested := &HyperliquidProtectionSyncResult{CancelStopLossSucceeded: true, StopLossOID: 222, StopLossTriggerPx: 1900}
	if hlProtectionLostExchangeStop(rested) || hlProtectionStopOutcomeUnknown(rested) {
		t.Errorf("a resting replacement must raise neither alert")
	}
	pos = newPos()
	applyHyperliquidProtectionSync(pos, rested, nil)
	if pos.StopLossOID != 222 || pos.StopLossTriggerPx != 1900 {
		t.Errorf("resting replacement not adopted: OID %d trigger %.2f, want 222 / 1900", pos.StopLossOID, pos.StopLossTriggerPx)
	}

	both := &HyperliquidProtectionSyncResult{CancelStopLossSucceeded: true, StopLossOID: 333, StopLossOutcomeUnknown: true}
	if hlProtectionLostExchangeStop(both) || hlProtectionStopOutcomeUnknown(both) {
		t.Errorf("a resolved placement must raise neither alert")
	}
}

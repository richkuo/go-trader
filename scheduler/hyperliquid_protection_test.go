package main

import (
	"encoding/json"
	"math"
	"path/filepath"
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

func TestHLOnChainTPState(t *testing.T) {
	liveArgs := []string{"bollinger_bands", "ETH", "30m", "--mode=live"}
	tiered := func(name string, params map[string]interface{}) StrategyConfig {
		return StrategyConfig{
			ID: "hl-tp-state", Args: liveArgs, Type: "perps", Platform: "hyperliquid",
			CloseStrategy: &StrategyRef{Name: name, Params: params},
		}
	}
	unified := map[string]interface{}{
		regimeClassifierKey: map[string]interface{}{
			"trending_up": map[string]interface{}{
				"tp_tiers": []interface{}{
					map[string]interface{}{"atr_multiple": 2.0, "close_fraction": 0.5},
					map[string]interface{}{"atr_multiple": 4.0, "close_fraction": 1.0},
				},
			},
		},
	}
	singleTier := map[string]interface{}{"tp_tiers": []interface{}{
		map[string]interface{}{"atr_multiple": 2.0, "close_fraction": 1.0},
	}}
	cases := []struct {
		name        string
		sc          StrategyConfig
		pos         *Position
		wantResting bool
		wantBlocked string
	}{
		{"resting tier oid", tiered("tiered_tp_atr", nil),
			&Position{Symbol: "ETH", Side: "long", Quantity: 1, AvgCost: 100, EntryATR: 2, TPOIDs: []int64{0, 55, 0}},
			true, ""},
		{"armed tier with oid 0 counts as resting", tiered("tiered_tp_atr", nil),
			&Position{Symbol: "ETH", Side: "long", Quantity: 1, AvgCost: 100, TPOIDs: []int64{0, 0, 0}, TPArmedTiers: []bool{false, true, false}},
			true, hlOnChainTPBlockedEntryATR},
		{"entry ATR missing blocks placement", tiered("tiered_tp_atr", nil),
			&Position{Symbol: "ETH", Side: "long", Quantity: 1, AvgCost: 100},
			false, hlOnChainTPBlockedEntryATR},
		{"single-tier ladder is unplaceable", tiered("tiered_tp_atr", singleTier),
			&Position{Symbol: "ETH", Side: "long", Quantity: 1, AvgCost: 100, EntryATR: 2},
			false, hlOnChainTPBlockedTiersUnplaced},
		{"default ladder is plannable", tiered("tiered_tp_atr", nil),
			&Position{Symbol: "ETH", Side: "long", Quantity: 1, AvgCost: 100, EntryATR: 2},
			false, ""},
		{"regime ref with an unresolved label is unplaceable", tiered("tiered_tp_atr_live_regime", unified),
			&Position{Symbol: "ETH", Side: "long", Quantity: 1, AvgCost: 100, EntryATR: 2, Regime: "ranging"},
			false, hlOnChainTPBlockedTiersUnplaced},
		{"dynamic ref ignores the label that moves inside the sync", tiered(dynamicCloseStrategyName, unified),
			&Position{Symbol: "ETH", Side: "long", Quantity: 1, AvgCost: 100, EntryATR: 2, Regime: "ranging"},
			false, ""},
		{"dynamic ref still needs entry ATR", tiered(dynamicCloseStrategyName, unified),
			&Position{Symbol: "ETH", Side: "long", Quantity: 1, AvgCost: 100, Regime: "trending_up"},
			false, hlOnChainTPBlockedEntryATR},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resting, blocked := hlOnChainTPState(tc.sc, tc.pos)
			if resting != tc.wantResting || blocked != tc.wantBlocked {
				t.Fatalf("hlOnChainTPState = (%v, %q), want (%v, %q)", resting, blocked, tc.wantResting, tc.wantBlocked)
			}
			ctx := positionCtxForCheck(tc.sc, tc.pos, nil)
			if ctx.OnChainTPResting != tc.wantResting || ctx.OnChainTPBlocked != tc.wantBlocked {
				t.Fatalf("positionCtxForCheck on-chain TP state = (%v, %q), want (%v, %q)", ctx.OnChainTPResting, ctx.OnChainTPBlocked, tc.wantResting, tc.wantBlocked)
			}
		})
	}
}

func TestHLCloseOwnerForCheck(t *testing.T) {
	mult := 1.0
	liveArgs := []string{"bollinger_bands", "ETH", "30m", "--mode=live"}
	paperArgs := []string{"bollinger_bands", "ETH", "30m", "--mode=paper"}
	build := func(typ string, args []string, closeName string, params map[string]interface{}) StrategyConfig {
		sc := StrategyConfig{ID: "hl-owner", Args: args, Type: typ, Platform: "hyperliquid", StopLossATRMult: &mult}
		if closeName != "" {
			sc.CloseStrategy = &StrategyRef{Name: closeName, Params: params}
		}
		return sc
	}
	singleTier := map[string]interface{}{"tp_tiers": []interface{}{
		map[string]interface{}{"atr_multiple": 2.0, "close_fraction": 1.0},
	}}
	dynamicUnified := map[string]interface{}{
		regimeClassifierKey: map[string]interface{}{
			"trending_up": map[string]interface{}{
				"tp_tiers": []interface{}{
					map[string]interface{}{"atr_multiple": 2.0, "close_fraction": 0.5},
					map[string]interface{}{"atr_multiple": 4.0, "close_fraction": 1.0},
				},
			},
		},
	}
	open := func(entryATR float64, oids []int64, armed []bool) *Position {
		return &Position{Symbol: "ETH", Side: "long", Quantity: 1, AvgCost: 100, EntryATR: entryATR, TPOIDs: oids, TPArmedTiers: armed}
	}
	cases := []struct {
		name string
		sc   StrategyConfig
		pos  *Position
		want string
	}{
		{"paper tiered keeps the in-process evaluator", build("perps", paperArgs, "tiered_tp_atr", nil), open(2, nil, nil), ""},
		{"live tiered while flat is on-chain owned", build("perps", liveArgs, "tiered_tp_atr", nil), nil, hlCloseOwnerOnChainTP},
		{"live tiered with a resting tier is on-chain owned", build("perps", liveArgs, "tiered_tp_atr", nil), open(2, []int64{11, 12, 13}, nil), hlCloseOwnerOnChainTP},
		{"live tiered with an armed oid-0 tier stays on-chain owned", build("perps", liveArgs, "tiered_tp_atr", nil), open(0, []int64{0, 0, 0}, []bool{true, false, false}), hlCloseOwnerOnChainTP},
		{"live tiered with no entry ATR falls back to the evaluator", build("perps", liveArgs, "tiered_tp_atr_live", nil), open(0, nil, nil), ""},
		{"live tiered plannable but not yet placed stays on-chain owned", build("perps", liveArgs, "tiered_tp_atr", nil), open(2, nil, nil), hlCloseOwnerOnChainTP},
		{"live single-tier ladder falls back to the evaluator", build("perps", liveArgs, "tiered_tp_atr", singleTier), open(2, nil, nil), ""},
		{"live dynamic ref stays on-chain owned", build("perps", liveArgs, dynamicCloseStrategyName, dynamicUnified), open(2, nil, nil), hlCloseOwnerOnChainTP},
		{"trailing ratchet is not an on-chain TP", build("perps", liveArgs, "trailing_tp_ratchet", nil), open(2, nil, nil), ""},
		{"tp_at_pct is not an on-chain TP", build("perps", liveArgs, "tp_at_pct", nil), open(2, nil, nil), ""},
		{"no close ref", build("perps", liveArgs, "", nil), open(2, nil, nil), ""},
		{"manual live tiered is on-chain owned", build("manual", []string{"hold", "ETH", "1h", "--mode=live"}, "tiered_tp_atr_live", nil), open(2, []int64{21, 22, 23}, nil), hlCloseOwnerOnChainTP},
		{"spot never places on-chain TPs", build("spot", liveArgs, "tiered_tp_atr_live", nil), open(2, nil, nil), ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := hlCloseOwnerForCheck(tc.sc, positionCtxForCheck(tc.sc, tc.pos, nil))
			if got != tc.want {
				t.Fatalf("hlCloseOwnerForCheck = %q, want %q", got, tc.want)
			}
			if tc.sc.CloseStrategy == nil {
				return
			}
			refs, err := buildStrategyRefsArg(tc.sc, got)
			if err != nil {
				t.Fatalf("buildStrategyRefsArg: %v", err)
			}
			var payload struct {
				Closes     []StrategyRef `json:"closes"`
				CloseOwner string        `json:"close_owner"`
			}
			if err := json.Unmarshal([]byte(refs[1]), &payload); err != nil {
				t.Fatalf("payload unmarshal: %v", err)
			}
			if len(payload.Closes) != 1 || payload.Closes[0].Name != tc.sc.CloseStrategy.Name {
				t.Fatalf("closes = %+v, want the configured %q ref kept", payload.Closes, tc.sc.CloseStrategy.Name)
			}
			if payload.CloseOwner != tc.want {
				t.Fatalf("close_owner = %q, want %q", payload.CloseOwner, tc.want)
			}
		})
	}
}

func TestNotifyHLOnChainTPUnplaceableAlertsOnce(t *testing.T) {
	sc := StrategyConfig{ID: "hl-unplaceable-once", CloseStrategy: &StrategyRef{Name: "tiered_tp_atr_live"}}
	t.Cleanup(func() { clearHLOnChainTPUnplaceable(sc.ID, "ETH") })
	steps := []struct {
		name  string
		clear bool
		want  bool
	}{
		{"first takeover alerts", false, true},
		{"repeat takeover stays silent", false, false},
		{"after the owner returns on-chain the next takeover alerts again", true, true},
	}
	for _, step := range steps {
		if step.clear {
			clearHLOnChainTPUnplaceable(sc.ID, "ETH")
		}
		if got := notifyHLOnChainTPUnplaceable(nil, nil, sc, "ETH", hlOnChainTPBlockedEntryATR); got != step.want {
			t.Fatalf("%s: alerted = %v, want %v", step.name, got, step.want)
		}
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
	synced, fillPx := runHyperliquidProtectionSync(sc, state, nil, "ETH", &mu, nil, nil, "test", nil, nil, nil, hlProtectionGuardFull)
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
	if syncedNeg, _ := runHyperliquidProtectionSync(sc, state, nil, "ETH", &mu, nil, nil, "test", nil, nil, nil, hlProtectionGuardFull); syncedNeg {
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
	if syncedNeg, _ := runHyperliquidProtectionSync(sc, state, nil, "ETH", &mu, nil, nil, "test", nil, nil, nil, hlProtectionGuardFull); syncedNeg {
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
	synced2, _ := runHyperliquidProtectionSync(sc, state, db, "ETH", &mu, nil, nil, "test", nil, nil, nil, hlProtectionGuardFull)
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
	synced, fillPx := runHyperliquidProtectionSync(sc, state, nil, "ETH", &mu, nil, nil, "test", nil, nil, nil, hlProtectionGuardFull)
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
	synced, fillPx := runHyperliquidProtectionSync(sc, state, nil, "ETH", &mu, nil, nil, "test", nil, nil, nil, hlProtectionGuardFull)
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

func TestRunHyperliquidProtectionSyncManualActionGuard(t *testing.T) {
	for _, tc := range []struct {
		name     string
		action   string
		strategy string
		symbol   string
		locked   bool
		broken   bool
		blocked  bool
	}{
		{name: "in flight command", locked: true, blocked: true},
		{name: "restored orders awaiting adoption", action: "restore-tp", blocked: true},
		{name: "open awaiting adoption", action: "open", blocked: true},
		{name: "add awaiting adoption", action: "add", blocked: true},
		{name: "close awaiting adoption", action: "close", blocked: true},
		{name: "stop edit awaiting adoption", action: "update-sl", blocked: true},
		{name: "stop cancel awaiting adoption", action: "cancel-sl", blocked: true},
		{name: "different strategy", action: "restore-tp", strategy: "peer"},
		{name: "different symbol", action: "restore-tp", symbol: "BTC"},
		{name: "unreadable queue", broken: true, blocked: true},
		{name: "no pending action"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dbPath := filepath.Join(t.TempDir(), "state.db")
			db, err := OpenStateDB(dbPath)
			if err != nil {
				t.Fatal(err)
			}
			sc := StrategyConfig{ID: "manual-eth", Type: "manual", Platform: "hyperliquid", CloseStrategy: tieredTPCloseStrategy()}
			if tc.action != "" {
				id, symbol := tc.strategy, tc.symbol
				if id == "" {
					id = sc.ID
				}
				if symbol == "" {
					symbol = "eth"
				}
				if err := singleFileStore(db).InsertPendingManualAction(PendingManualAction{StrategyID: id, Symbol: symbol, Action: tc.action, CreatedAt: time.Now().UTC()}); err != nil {
					t.Fatal(err)
				}
			}
			db.Close()
			db, err = OpenStateDB(dbPath)
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			if tc.broken {
				db.Close()
			}
			if tc.locked {
				unlock, err := acquireManualActionFileLock(dbPath)
				if err != nil {
					t.Fatal(err)
				}
				defer unlock()
			}
			clearHLProtectionGuardBlocks(sc.ID, "ETH")
			t.Cleanup(func() { clearHLProtectionGuardBlocks(sc.ID, "ETH") })
			calls := 0
			withStubbedSyncHyperliquidProtection(t, func(StrategyConfig, hlProtectionPlan, *MultiNotifier, *StrategyLogger, []byte) (*HyperliquidProtectionSyncResult, bool) {
				calls++
				unlock, err := acquireManualActionFileLockWithWait(dbPath, 0)
				if err == nil {
					unlock()
					t.Error("placement did not hold the manual-action file lock")
				}
				return &HyperliquidProtectionSyncResult{TPOIDs: []int64{901, 902, 903}}, true
			})
			pos := &Position{Symbol: "ETH", Quantity: 1, AvgCost: 2000, EntryATR: 50, Side: "long", TPOIDs: []int64{701, 702, 703}}
			state := &StrategyState{ID: sc.ID, Positions: map[string]*Position{"ETH": pos}}
			var mu sync.RWMutex
			synced, _ := runHyperliquidProtectionSync(sc, state, db, "ETH", &mu, nil, nil, "test", nil, nil, nil, hlProtectionGuardFull)
			if tc.blocked {
				if synced || calls != 0 || !reflect.DeepEqual(pos.TPOIDs, []int64{701, 702, 703}) {
					t.Fatalf("blocked sync mutated protection: synced=%v calls=%d oids=%v", synced, calls, pos.TPOIDs)
				}
			} else if !synced || calls != 1 || !reflect.DeepEqual(pos.TPOIDs, []int64{901, 902, 903}) {
				t.Fatalf("allowed sync failed: synced=%v calls=%d oids=%v", synced, calls, pos.TPOIDs)
			}
		})
	}
}

func TestApplyHyperliquidProtectionSyncImmediateTiers(t *testing.T) {
	for _, raw := range []string{
		`{"tp_oids":[0,702],"tp_filled_immediately":[true,false]}`,
		`{"tp_oids":[0,0],"tp_filled_immediately":[true,false],"tp_filled_externally":[false,true]}`,
		`{"tp2_oid":702,"tp_filled_immediately":[true,false]}`,
		`{"tp_filled_immediately":[true,false],"tp2_filled_externally":true}`,
	} {
		t.Run(raw, func(t *testing.T) {
			var result HyperliquidProtectionSyncResult
			if err := json.Unmarshal([]byte(raw), &result); err != nil {
				t.Fatal(err)
			}
			pos := &Position{Symbol: "ETH", Quantity: 1, AvgCost: 2000, EntryATR: 50, Side: "long"}
			applyHyperliquidProtectionSync(pos, &result, nil)
			if len(pos.TPOIDs) != 2 || pos.TPOIDs[0] != 0 || !reflect.DeepEqual(pos.TPArmedTiers, []bool{true, true}) {
				t.Fatalf("completed tier lost: %+v", pos)
			}
			if pos.Quantity != 1 || pos.AvgCost != 2000 {
				t.Fatal("protection result booked an unconfirmed fill")
			}
			sc := StrategyConfig{Type: "manual", Platform: "hyperliquid", CloseStrategy: &StrategyRef{Name: "tiered_tp_atr"}}
			plan, ok := buildHyperliquidProtectionPlan(sc, pos, 0)
			if !ok || plan.TPOIDs[0] != 0 || !plan.TPArmedTiers[0] {
				t.Fatalf("next cycle would replace the completed tier: %+v", plan)
			}
		})
	}
}

func TestHLProtectionGuardBlockAlertsOnRepeat(t *testing.T) {
	clearHLProtectionGuardBlocks("manual-eth", "ETH")
	t.Cleanup(func() { clearHLProtectionGuardBlocks("manual-eth", "ETH") })
	for attempt := 1; attempt <= hlProtectionGuardAlertAfterBlocks+2; attempt++ {
		blocks, alert := recordHLProtectionGuardBlock("manual-eth", "ETH")
		wantAlert := attempt == hlProtectionGuardAlertAfterBlocks
		if blocks != attempt || alert != wantAlert {
			t.Fatalf("attempt %d: blocks=%d alert=%v want blocks=%d alert=%v", attempt, blocks, alert, attempt, wantAlert)
		}
	}
	if _, alert := recordHLProtectionGuardBlock("manual-eth", "BTC"); alert {
		t.Fatal("a different symbol inherited the blocked count")
	}
	clearHLProtectionGuardBlocks("manual-eth", "ETH")
	for attempt := 1; attempt < hlProtectionGuardAlertAfterBlocks; attempt++ {
		if _, alert := recordHLProtectionGuardBlock("manual-eth", "ETH"); alert {
			t.Fatalf("a cleared symbol alerted again after %d blocks", attempt)
		}
	}
	if _, alert := recordHLProtectionGuardBlock("manual-eth", "ETH"); !alert {
		t.Fatal("a cleared symbol never alerted again")
	}
}

func TestRunHyperliquidProtectionSyncStopLegAfterFailedClose(t *testing.T) {
	for _, tc := range []struct {
		name      string
		stopOwned bool
		wantCalls int
	}{
		{name: "protection sync owns the stop", stopOwned: true, wantCalls: 1},
		{name: "another owner holds the stop", wantCalls: 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dbPath := filepath.Join(t.TempDir(), "state.db")
			db, err := OpenStateDB(dbPath)
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			sc := StrategyConfig{ID: "manual-eth", Type: "manual", Platform: "hyperliquid", CloseStrategy: tieredTPCloseStrategy()}
			if tc.stopOwned {
				mult := 2.0
				sc.StopLossATRMult = &mult
			}
			if err := singleFileStore(db).InsertPendingManualAction(PendingManualAction{StrategyID: sc.ID, Symbol: "ETH", Action: "restore-tp", CreatedAt: time.Now().UTC()}); err != nil {
				t.Fatal(err)
			}
			clearHLProtectionGuardBlocks(sc.ID, "ETH")
			t.Cleanup(func() { clearHLProtectionGuardBlocks(sc.ID, "ETH") })
			calls := 0
			var seen hlProtectionPlan
			withStubbedSyncHyperliquidProtection(t, func(_ StrategyConfig, plan hlProtectionPlan, _ *MultiNotifier, _ *StrategyLogger, _ []byte) (*HyperliquidProtectionSyncResult, bool) {
				calls++
				seen = plan
				return &HyperliquidProtectionSyncResult{StopLossOID: 555, StopLossTriggerPx: 1900}, true
			})
			pos := &Position{Symbol: "ETH", Quantity: 1, AvgCost: 2000, EntryATR: 50, Side: "long", TPOIDs: []int64{701, 702, 703}, TPArmedTiers: []bool{true, true, true}}
			state := &StrategyState{ID: sc.ID, Positions: map[string]*Position{"ETH": pos}}
			var mu sync.RWMutex
			runHyperliquidProtectionSync(sc, state, db, "ETH", &mu, nil, nil, "test", nil, nil, nil, hlProtectionGuardStopLegAfterFailedClose)
			if calls != tc.wantCalls {
				t.Fatalf("placement calls=%d want %d", calls, tc.wantCalls)
			}
			if !reflect.DeepEqual(pos.TPOIDs, []int64{701, 702, 703}) {
				t.Fatalf("the gated take-profit tiers were rewritten: %v", pos.TPOIDs)
			}
			if tc.wantCalls == 0 {
				if pos.StopLossOID != 0 {
					t.Fatalf("a stop was placed for a strategy the sync does not own: %d", pos.StopLossOID)
				}
				return
			}
			if len(seen.Tiers) != 0 || len(seen.TPOIDs) != 0 || len(seen.TPArmedTiers) != 0 || len(seen.CancelTPOIDs) != 0 {
				t.Fatalf("the stop-leg plan carried take-profit inputs: %+v", seen)
			}
			if seen.StopLossATRMult <= 0 {
				t.Fatalf("the stop-leg plan carried no stop: %+v", seen)
			}
			if pos.StopLossOID != 555 || pos.StopLossTriggerPx != 1900 {
				t.Fatalf("the re-armed stop was not booked: %+v", pos)
			}
		})
	}
}

func TestHLProtectionSyncUnknownTPPlacementAlertLane(t *testing.T) {
	for _, tc := range []struct {
		name       string
		guard      hlProtectionGuardMode
		wantAlerts int
	}{
		{name: "ordinary protection sync sends the unresolved placement alert", guard: hlProtectionGuardFull, wantAlerts: 1},
		{name: "close re-arm leaves the unresolved placement to its one re-arm alert", guard: hlProtectionGuardStopLegAfterFailedClose, wantAlerts: 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sc := StrategyConfig{ID: "manual-eth", Type: "manual", Platform: "hyperliquid", CloseStrategy: tieredTPCloseStrategy()}
			withStubbedSyncHyperliquidProtection(t, func(_ StrategyConfig, _ hlProtectionPlan, _ *MultiNotifier, _ *StrategyLogger, _ []byte) (*HyperliquidProtectionSyncResult, bool) {
				return &HyperliquidProtectionSyncResult{TPOIDs: []int64{0, 802, 803}, TPErrors: []string{"read timeout", "", ""}, TPOutcomeUnknown: []bool{true, false, false}}, true
			})
			pos := &Position{Symbol: "ETH", Quantity: 1, AvgCost: 2000, EntryATR: 50, Side: "long"}
			state := &StrategyState{ID: sc.ID, Positions: map[string]*Position{"ETH": pos}}
			notifier, backend := confirmationNotifier()
			var mu sync.RWMutex
			runHyperliquidProtectionSync(sc, state, nil, "ETH", &mu, notifier, nil, "test", nil, nil, nil, tc.guard)
			if pos.TPOIDs[0] != 0 || !pos.TPArmedTiers[0] {
				t.Fatalf("the unresolved tier is replaceable: oids=%v armed=%v", pos.TPOIDs, pos.TPArmedTiers)
			}
			backend.mu.Lock()
			defer backend.mu.Unlock()
			alerts := 0
			for _, m := range backend.messages {
				if strings.Contains(m.content, "never resolved the placement of take-profit tier(s) [1]") {
					alerts++
				}
			}
			if alerts != tc.wantAlerts {
				t.Fatalf("unresolved placement alerts = %d, want %d: %+v", alerts, tc.wantAlerts, backend.messages)
			}
		})
	}
}

func TestApplyUnknownTPPlacementOutcome(t *testing.T) {
	for _, raw := range []string{
		`{"tp_oids":[0,802],"tp_outcome_unknown":[true,false],"tp_errors":["read timeout",""]}`,
		`{"tp_outcome_unknown":[true,false]}`,
	} {
		t.Run(raw, func(t *testing.T) {
			var result HyperliquidProtectionSyncResult
			if err := json.Unmarshal([]byte(raw), &result); err != nil {
				t.Fatal(err)
			}
			pos := &Position{Symbol: "ETH", Quantity: 1, AvgCost: 2000, EntryATR: 50, Side: "long"}
			applyHyperliquidProtectionSync(pos, &result, nil)
			if pos.TPOIDs[0] != 0 || !pos.TPArmedTiers[0] {
				t.Fatalf("an unresolved placement left the tier replaceable: oids=%v armed=%v", pos.TPOIDs, pos.TPArmedTiers)
			}
			sc := StrategyConfig{Type: "manual", Platform: "hyperliquid", CloseStrategy: tieredTPCloseStrategy()}
			plan, ok := buildHyperliquidProtectionPlan(sc, pos, 0)
			if !ok || plan.TPOIDs[0] != 0 || !plan.TPArmedTiers[0] {
				t.Fatalf("the next cycle would place a second order at the same price: %+v", plan)
			}
			if got := unknownTPPlacementTiers(&result); !reflect.DeepEqual(got, []int{1}) {
				t.Fatalf("the operator alert named tiers %v", got)
			}
		})
	}
}

package main

import (
	"math"
	"strings"
	"sync"
	"testing"
)

// postTPSLTestStrategy builds a perps strategy with a fixed ATR SL and a
// tiered TP close ref carrying sl_after rules. Used by orchestrator tests.
func postTPSLTestStrategy(slAfter interface{}, tiers []interface{}) StrategyConfig {
	atrMult := 1.0
	params := map[string]interface{}{
		"tiers": tiers,
	}
	if slAfter != nil {
		params["sl_after"] = slAfter
	}
	return StrategyConfig{
		ID:              "hl-sl-after",
		Platform:        "hyperliquid",
		Type:            "perps",
		Script:          "shared_scripts/check_hyperliquid.py",
		StopLossATRMult: &atrMult,
		CloseStrategies: []StrategyRef{{
			Name:   "tiered_tp_atr_live",
			Params: params,
		}},
	}
}

func TestComputePostTPStopLossTrigger_Breakeven(t *testing.T) {
	cases := []struct {
		name string
		side string
		avg  float64
	}{
		{"long", "long", 100},
		{"short", "short", 200},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			px, mode, ok := computePostTPStopLossTrigger(
				SLAfterRule{Kind: "breakeven"}, tc.side, tc.avg, 5, 0,
			)
			if !ok {
				t.Fatalf("expected ok=true")
			}
			if px != tc.avg {
				t.Fatalf("trigger px = %v, want %v", px, tc.avg)
			}
			if mode != "breakeven" {
				t.Fatalf("mode = %q, want breakeven", mode)
			}
		})
	}
}

func TestComputePostTPStopLossTrigger_ATROffset(t *testing.T) {
	const avg, atr = 100.0, 5.0
	cases := []struct {
		name string
		side string
		mult float64
		want float64
	}{
		{"long_positive_locks_profit", "long", 0.25, avg + 0.25*atr},
		{"long_negative_loosens", "long", -0.5, avg - 0.5*atr},
		{"long_zero_eq_breakeven", "long", 0, avg},
		{"short_positive_locks_profit", "short", 0.25, avg - 0.25*atr},
		{"short_negative_loosens", "short", -0.5, avg + 0.5*atr},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			px, _, ok := computePostTPStopLossTrigger(
				SLAfterRule{Kind: "atr_offset", ATRMult: tc.mult}, tc.side, avg, atr, 0,
			)
			if !ok {
				t.Fatalf("expected ok=true")
			}
			if math.Abs(px-tc.want) > 1e-9 {
				t.Fatalf("trigger px = %v, want %v", px, tc.want)
			}
		})
	}
}

func TestComputePostTPStopLossTrigger_ATROffsetMode(t *testing.T) {
	cases := []struct {
		mult float64
		want string
	}{
		// {atr_mult: 0} preserves "atr+0" so logs/DMs reflect the operator's
		// explicit kind; Kind=="breakeven" still renders as "breakeven" via
		// the trigger helper's own branch.
		{0, "atr+0"},
		{0.25, "atr+0.25"},
		{-0.5, "atr-0.5"},
		{1, "atr+1"},
	}
	for _, tc := range cases {
		_, mode, _ := computePostTPStopLossTrigger(
			SLAfterRule{Kind: "atr_offset", ATRMult: tc.mult}, "long", 100, 5, 0,
		)
		if mode != tc.want {
			t.Fatalf("mult=%v: mode = %q, want %q", tc.mult, mode, tc.want)
		}
	}
}

func TestComputePostTPStopLossTrigger_TrailFromHere(t *testing.T) {
	const avg, atr = 100.0, 5.0
	cases := []struct {
		name string
		side string
		mark float64
		mult float64
		want float64
	}{
		{"long", "long", 110, 1.0, 110 - 1.0*atr},
		{"short", "short", 90, 1.5, 90 + 1.5*atr},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			px, mode, ok := computePostTPStopLossTrigger(
				SLAfterRule{Kind: "trail_from_here", TrailATRMult: tc.mult},
				tc.side, avg, atr, tc.mark,
			)
			if !ok {
				t.Fatalf("expected ok=true")
			}
			if math.Abs(px-tc.want) > 1e-9 {
				t.Fatalf("trigger px = %v, want %v", px, tc.want)
			}
			if mode == "" {
				t.Fatalf("expected non-empty mode label")
			}
		})
	}
}

func TestComputePostTPStopLossTrigger_RejectsBadInputs(t *testing.T) {
	cases := []struct {
		name string
		rule SLAfterRule
		side string
		avg  float64
		atr  float64
		mark float64
	}{
		{"empty rule", SLAfterRule{}, "long", 100, 5, 0},
		{"unknown side", SLAfterRule{Kind: "breakeven"}, "neutral", 100, 5, 0},
		{"non-positive avgCost", SLAfterRule{Kind: "breakeven"}, "long", 0, 5, 0},
		{"atr_offset missing ATR", SLAfterRule{Kind: "atr_offset", ATRMult: 0.25}, "long", 100, 0, 0},
		{"trail missing ATR", SLAfterRule{Kind: "trail_from_here", TrailATRMult: 1}, "long", 100, 0, 110},
		{"trail missing mark", SLAfterRule{Kind: "trail_from_here", TrailATRMult: 1}, "long", 100, 5, 0},
		{"trail non-positive mult", SLAfterRule{Kind: "trail_from_here", TrailATRMult: 0}, "long", 100, 5, 110},
		{"unknown kind", SLAfterRule{Kind: "weird"}, "long", 100, 5, 110},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, _, ok := computePostTPStopLossTrigger(tc.rule, tc.side, tc.avg, tc.atr, tc.mark)
			if ok {
				t.Fatalf("expected ok=false")
			}
		})
	}
}

func TestValidateSLAfterRule(t *testing.T) {
	okCases := []SLAfterRule{
		{},
		{Kind: "breakeven"},
		{Kind: "atr_offset", ATRMult: 0.25},
		{Kind: "atr_offset", ATRMult: 0},
		{Kind: "atr_offset", ATRMult: -0.5},
		{Kind: "trail_from_here", TrailATRMult: 1.0},
	}
	for _, r := range okCases {
		if err := validateSLAfterRule(r); err != nil {
			t.Fatalf("expected nil error for %+v, got %v", r, err)
		}
	}
	badCases := []SLAfterRule{
		{Kind: "trail_from_here"},
		{Kind: "trail_from_here", TrailATRMult: 0},
		{Kind: "trail_from_here", TrailATRMult: -1},
		{Kind: "weird"},
	}
	for _, r := range badCases {
		if err := validateSLAfterRule(r); err == nil {
			t.Fatalf("expected error for %+v", r)
		}
	}
}

func TestSLAfterRule_IsEmpty(t *testing.T) {
	if !(SLAfterRule{}).IsEmpty() {
		t.Fatal("zero value should be empty")
	}
	if (SLAfterRule{Kind: "breakeven"}).IsEmpty() {
		t.Fatal("breakeven rule should not be empty")
	}
}

func TestParseSLAfterRule(t *testing.T) {
	cases := []struct {
		name string
		raw  interface{}
		want SLAfterRule
	}{
		{"nil", nil, SLAfterRule{}},
		{"empty_string", "", SLAfterRule{}},
		{"string_breakeven", "breakeven", SLAfterRule{Kind: "breakeven"}},
		{"string_breakeven_case", "BREAKEVEN", SLAfterRule{Kind: "breakeven"}},
		{
			"implicit_atr_offset",
			map[string]interface{}{"atr_mult": 0.25},
			SLAfterRule{Kind: "atr_offset", ATRMult: 0.25},
		},
		{
			"implicit_atr_offset_negative",
			map[string]interface{}{"atr_mult": -0.5},
			SLAfterRule{Kind: "atr_offset", ATRMult: -0.5},
		},
		{
			"explicit_kind_atr_offset",
			map[string]interface{}{"kind": "atr_offset", "atr_mult": 0.25},
			SLAfterRule{Kind: "atr_offset", ATRMult: 0.25},
		},
		{
			"explicit_kind_breakeven",
			map[string]interface{}{"kind": "breakeven"},
			SLAfterRule{Kind: "breakeven"},
		},
		{
			"nested_trail_from_here",
			map[string]interface{}{
				"trail_from_here": map[string]interface{}{"atr_mult": 1.0},
			},
			SLAfterRule{Kind: "trail_from_here", TrailATRMult: 1.0},
		},
		{
			"explicit_kind_trail_from_here",
			map[string]interface{}{"kind": "trail_from_here", "atr_mult": 1.5},
			SLAfterRule{Kind: "trail_from_here", TrailATRMult: 1.5},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseSLAfterRule(tc.raw)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.want {
				t.Fatalf("got %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestParseSLAfterRule_Errors(t *testing.T) {
	cases := []struct {
		name string
		raw  interface{}
	}{
		{"unknown_string", "hold"},
		{"unknown_kind", map[string]interface{}{"kind": "weird"}},
		{"trail_negative_mult", map[string]interface{}{
			"trail_from_here": map[string]interface{}{"atr_mult": -1.0},
		}},
		{"trail_zero_mult", map[string]interface{}{
			"trail_from_here": map[string]interface{}{"atr_mult": 0.0},
		}},
		{"trail_missing_mult", map[string]interface{}{
			"trail_from_here": map[string]interface{}{},
		}},
		{"empty_object", map[string]interface{}{}},
		{"wrong_type", 42},
		{"kind_not_string", map[string]interface{}{"kind": 1}},
		{"trail_not_object", map[string]interface{}{"trail_from_here": "1.0"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := parseSLAfterRule(tc.raw)
			if err == nil {
				t.Fatalf("expected error for %v", tc.raw)
			}
		})
	}
}

func TestParseStrategyTPSLAfterRules(t *testing.T) {
	// strategy-level default + per-tier override
	sc := StrategyConfig{
		Type:     "perps",
		Platform: "hyperliquid",
		CloseStrategies: []StrategyRef{{
			Name: "tiered_tp_atr_live",
			Params: map[string]interface{}{
				"sl_after": "breakeven",
				"tiers": []interface{}{
					// out of order intentionally — should sort by multiple
					map[string]interface{}{"atr_multiple": 3, "close_fraction": 1.0, "sl_after": map[string]interface{}{"atr_mult": 0.25}},
					map[string]interface{}{"atr_multiple": 2, "close_fraction": 0.5},
				},
			},
		}},
	}
	rules, errs := parseStrategyTPSLAfterRules(sc)
	if len(errs) != 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
	if rules.Default.Kind != "breakeven" {
		t.Fatalf("default = %+v, want breakeven", rules.Default)
	}
	if len(rules.PerTier) != 2 {
		t.Fatalf("per-tier len = %d, want 2", len(rules.PerTier))
	}
	if !rules.PerTier[0].IsEmpty() {
		t.Fatalf("tier 0 (mult=2) should inherit default, got %+v", rules.PerTier[0])
	}
	if rules.PerTier[1].Kind != "atr_offset" || rules.PerTier[1].ATRMult != 0.25 {
		t.Fatalf("tier 1 (mult=3) = %+v, want atr_offset/0.25", rules.PerTier[1])
	}
	if !rules.HasAny() {
		t.Fatal("HasAny() should be true")
	}
	// ForTier returns override or default
	if got := rules.ForTier(0); got.Kind != "breakeven" {
		t.Fatalf("ForTier(0) = %+v, want breakeven (inherited)", got)
	}
	if got := rules.ForTier(1); got.Kind != "atr_offset" {
		t.Fatalf("ForTier(1) = %+v, want atr_offset (override)", got)
	}
	if got := rules.ForTier(5); got.Kind != "breakeven" {
		t.Fatalf("ForTier(out-of-range) = %+v, want default", got)
	}
}

func TestParseStrategyTPSLAfterRules_NoTieredTP(t *testing.T) {
	sc := StrategyConfig{Type: "perps", Platform: "hyperliquid"}
	rules, errs := parseStrategyTPSLAfterRules(sc)
	if len(errs) != 0 || rules.HasAny() {
		t.Fatalf("expected empty/no-errors for strategy without tiered TP")
	}
}

func TestParseStrategyTPSLAfterRules_ReportsMalformed(t *testing.T) {
	sc := StrategyConfig{
		Type:     "perps",
		Platform: "hyperliquid",
		CloseStrategies: []StrategyRef{{
			Name: "tiered_tp_atr",
			Params: map[string]interface{}{
				"sl_after": "unknown-string",
				"tiers": []interface{}{
					map[string]interface{}{"atr_multiple": 2, "close_fraction": 0.5, "sl_after": map[string]interface{}{"kind": "weird"}},
					map[string]interface{}{"atr_multiple": 3, "close_fraction": 1.0},
				},
			},
		}},
	}
	_, errs := parseStrategyTPSLAfterRules(sc)
	if len(errs) < 2 {
		t.Fatalf("expected at least 2 errors (default + tier), got %v", errs)
	}
}

func TestValidatePostTPStopLossRules_RejectsTrailing(t *testing.T) {
	trail := 1.5
	atrSL := 1.0
	sc := StrategyConfig{
		Type:                "perps",
		Platform:            "hyperliquid",
		TrailingStopATRMult: &trail,
		StopLossATRMult:     &atrSL,
		CloseStrategies: []StrategyRef{{
			Name: "tiered_tp_atr_live",
			Params: map[string]interface{}{
				"sl_after": "breakeven",
				"tiers": []interface{}{
					map[string]interface{}{"atr_multiple": 2, "close_fraction": 0.5},
					map[string]interface{}{"atr_multiple": 3, "close_fraction": 1.0},
				},
			},
		}},
	}
	errs := validatePostTPStopLossRules(sc)
	found := false
	for _, e := range errs {
		if strings.Contains(e, "trailing_stop") {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected trailing_stop conflict error, got %v", errs)
	}
}

func TestValidatePostTPStopLossRules_RejectsNoFixedSL(t *testing.T) {
	sc := StrategyConfig{
		Type:     "perps",
		Platform: "hyperliquid",
		CloseStrategies: []StrategyRef{{
			Name: "tiered_tp_atr_live",
			Params: map[string]interface{}{
				"sl_after": "breakeven",
				"tiers": []interface{}{
					map[string]interface{}{"atr_multiple": 2, "close_fraction": 0.5},
					map[string]interface{}{"atr_multiple": 3, "close_fraction": 1.0},
				},
			},
		}},
	}
	errs := validatePostTPStopLossRules(sc)
	found := false
	for _, e := range errs {
		if strings.Contains(e, "fixed stop-loss") {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected no-fixed-SL error, got %v", errs)
	}
}

func TestValidatePostTPStopLossRules_AcceptsValid(t *testing.T) {
	atrSL := 1.0
	sc := StrategyConfig{
		Type:            "perps",
		Platform:        "hyperliquid",
		StopLossATRMult: &atrSL,
		CloseStrategies: []StrategyRef{{
			Name: "tiered_tp_atr_live",
			Params: map[string]interface{}{
				"sl_after": "breakeven",
				"tiers": []interface{}{
					map[string]interface{}{"atr_multiple": 2, "close_fraction": 0.5},
					map[string]interface{}{"atr_multiple": 3, "close_fraction": 1.0, "sl_after": map[string]interface{}{"atr_mult": 0.5}},
				},
			},
		}},
	}
	if errs := validatePostTPStopLossRules(sc); len(errs) != 0 {
		t.Fatalf("expected no errors, got %v", errs)
	}
}

func TestFormatSLAdjustmentAlert(t *testing.T) {
	cases := []struct {
		name string
		a    SLAdjustmentAlert
		want []string // substrings expected in the output
	}{
		{
			"breakeven",
			SLAdjustmentAlert{
				StrategyID: "hl-eth-st", Symbol: "ETH", Side: "long",
				TierIdx: 0, OldTriggerPx: 95, NewTriggerPx: 100, Mode: "breakeven",
			},
			[]string{"SL adjusted post-TP1", "hl-eth-st", "ETH LONG", "$95.0000 → $100.0000", "(breakeven)"},
		},
		{
			"atr_offset_short",
			SLAdjustmentAlert{
				StrategyID: "hl-btc-st", Symbol: "BTC", Side: "short",
				TierIdx: 1, OldTriggerPx: 105, NewTriggerPx: 99.5, Mode: "atr+0.5",
			},
			[]string{"SL adjusted post-TP2", "BTC SHORT", "$105.0000 → $99.5000", "(atr+0.5)"},
		},
		{
			"trail_transition",
			SLAdjustmentAlert{
				StrategyID: "hl-eth-st", Symbol: "ETH", Side: "long",
				TierIdx: 0, OldTriggerPx: 95, NewTriggerPx: 108,
				Mode: "trail 1.00×ATR", TransitionToTrailing: true,
			},
			[]string{"SL adjusted post-TP1 → trailing", "$95.0000 → $108.0000"},
		},
		{
			"no_old_trigger",
			SLAdjustmentAlert{
				StrategyID: "hl-eth-st", Symbol: "ETH", Side: "long",
				TierIdx: 0, OldTriggerPx: 0, NewTriggerPx: 100, Mode: "breakeven",
			},
			[]string{"SL: $100.0000 (breakeven)"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := formatSLAdjustmentAlert(tc.a)
			for _, s := range tc.want {
				if !strings.Contains(got, s) {
					t.Errorf("output missing %q\nfull output:\n%s", s, got)
				}
			}
		})
	}
}

type fakeOwnerDMSender struct {
	messages []string
}

func (f *fakeOwnerDMSender) SendOwnerDM(c string) { f.messages = append(f.messages, c) }

func TestNotifySLAdjustment_GatedOnEnabled(t *testing.T) {
	f := &fakeOwnerDMSender{}
	alert := SLAdjustmentAlert{StrategyID: "hl-eth-st", Symbol: "ETH", Side: "long", NewTriggerPx: 100, Mode: "breakeven"}

	notifySLAdjustment(f, false, alert)
	if len(f.messages) != 0 {
		t.Fatalf("expected no DM when disabled, got %d", len(f.messages))
	}
	notifySLAdjustment(f, true, alert)
	if len(f.messages) != 1 {
		t.Fatalf("expected 1 DM when enabled, got %d", len(f.messages))
	}
}

func TestEffectiveTrailingStopPct_HonorsPostTPTrailingATRMult(t *testing.T) {
	// Strategy has no sc.TrailingStop* configured — only post-TP transition.
	sc := StrategyConfig{Platform: "hyperliquid", Type: "perps"}
	mult := 1.0
	pos := &Position{AvgCost: 100, EntryATR: 5, PostTPTrailingATRMult: &mult}
	// Expected pct = 1.0 * 5 / 100 * 100 = 5%
	got := effectiveTrailingStopPct(sc, pos)
	if math.Abs(got-5.0) > 1e-9 {
		t.Fatalf("effectiveTrailingStopPct = %v, want 5", got)
	}
}

func TestEffectiveTrailingStopPct_PostTPMissingATRReturnsZero(t *testing.T) {
	sc := StrategyConfig{Platform: "hyperliquid", Type: "perps"}
	mult := 1.0
	pos := &Position{AvgCost: 100, EntryATR: 0, PostTPTrailingATRMult: &mult}
	if got := effectiveTrailingStopPct(sc, pos); got != 0 {
		t.Fatalf("effectiveTrailingStopPct = %v, want 0", got)
	}
}

func TestEffectiveTrailingStopPct_PostTPTakesPrecedenceOverStrategy(t *testing.T) {
	// Hypothetically if both were set (validator would block, but the helper
	// must still resolve unambiguously), post-TP wins because it represents
	// state that has already transitioned post-fill.
	trail := 2.0
	sc := StrategyConfig{Platform: "hyperliquid", Type: "perps", TrailingStopPct: &trail}
	mult := 1.0
	pos := &Position{AvgCost: 100, EntryATR: 5, PostTPTrailingATRMult: &mult}
	got := effectiveTrailingStopPct(sc, pos)
	if math.Abs(got-5.0) > 1e-9 {
		t.Fatalf("effectiveTrailingStopPct = %v, want 5 (post-TP)", got)
	}
}

func TestValidatePostTPStopLossRules_RejectsTrailFromHereOnManual(t *testing.T) {
	atrSL := 1.5
	sc := StrategyConfig{
		Type:            "manual",
		Platform:        "hyperliquid",
		StopLossATRMult: &atrSL,
		CloseStrategies: []StrategyRef{{
			Name: "tiered_tp_atr_live",
			Params: map[string]interface{}{
				"sl_after": map[string]interface{}{
					"trail_from_here": map[string]interface{}{"atr_mult": 1.0},
				},
				"tiers": []interface{}{
					map[string]interface{}{"atr_multiple": 2, "close_fraction": 0.5},
					map[string]interface{}{"atr_multiple": 3, "close_fraction": 1.0},
				},
			},
		}},
	}
	errs := validatePostTPStopLossRules(sc)
	found := false
	for _, e := range errs {
		if strings.Contains(e, "trail_from_here is not supported on manual") {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected manual+trail_from_here rejection, got %v", errs)
	}
}

// #716 item 2 regression: a non-TP partial close (e.g. close-evaluator firing
// signal=-1 to half the position) on a position whose tier 0 was never armed
// (transient placement failure left OID=0 with armed[0]=false) must NOT
// trigger the sl_after rule for tier 0. Pre-#716, findHighestClearedTier
// looked only at OID==0 and would return idx=0 here, firing breakeven against
// a tier that never existed.
func TestRunPostTPStopLossAdjustment_SkipsNeverArmedTier(t *testing.T) {
	old := runHyperliquidUpdateStopLossFunc
	defer func() { runHyperliquidUpdateStopLossFunc = old }()

	calls := 0
	runHyperliquidUpdateStopLossFunc = func(string, string, string, float64, float64, int64) (*HyperliquidStopLossUpdateResult, string, error) {
		calls++
		return &HyperliquidStopLossUpdateResult{StopLossOID: 999, StopLossTriggerPx: 100}, "", nil
	}

	sc := postTPSLTestStrategy("breakeven", []interface{}{
		map[string]interface{}{"atr_multiple": 2, "close_fraction": 0.5},
		map[string]interface{}{"atr_multiple": 3, "close_fraction": 1.0},
	})
	pos := &Position{
		Symbol: "ETH", Quantity: 0.5, InitialQuantity: 1.0,
		AvgCost: 100, EntryATR: 5, Side: "long",
		StopLossOID: 111, StopLossTriggerPx: 95,
		// Tier 0 placement failed transiently — OID=0 with armed[0]=false.
		// Tier 1 is resting with OID=222. A non-TP partial close has shrunk
		// Quantity to 0.5 (half InitialQuantity).
		TPOIDs:                   []int64{0, 222},
		TPArmedTiers:             []bool{false, true},
		SLAdjustedTiersProcessed: 0,
	}
	state := &StrategyState{ID: sc.ID, Positions: map[string]*Position{"ETH": pos}}
	var mu sync.RWMutex

	if runPostTPStopLossAdjustment(sc, state, "ETH", 105, nil, &mu, nil, nil) {
		t.Fatal("expected runPostTPStopLossAdjustment to skip never-armed tier")
	}
	if calls != 0 {
		t.Errorf("subprocess should not be called for never-armed tier; got %d calls", calls)
	}
	if pos.StopLossOID != 111 {
		t.Errorf("StopLossOID=%d, want 111 (unchanged)", pos.StopLossOID)
	}
	if pos.SLAdjustedTiersProcessed != 0 {
		t.Errorf("SLAdjustedTiersProcessed=%d, want 0 (no advance)", pos.SLAdjustedTiersProcessed)
	}
}

func TestRunPostTPStopLossAdjustment_BreakevenAfterTP1(t *testing.T) {
	old := runHyperliquidUpdateStopLossFunc
	defer func() { runHyperliquidUpdateStopLossFunc = old }()

	var gotSymbol, gotSide string
	var gotQty, gotTrigger float64
	var gotCancelOID int64
	runHyperliquidUpdateStopLossFunc = func(script, symbol, side string, size, triggerPx float64, cancelStopLossOID int64) (*HyperliquidStopLossUpdateResult, string, error) {
		gotSymbol, gotSide, gotQty, gotTrigger, gotCancelOID = symbol, side, size, triggerPx, cancelStopLossOID
		return &HyperliquidStopLossUpdateResult{StopLossOID: 999, StopLossTriggerPx: triggerPx}, "", nil
	}

	sc := postTPSLTestStrategy("breakeven", []interface{}{
		map[string]interface{}{"atr_multiple": 2, "close_fraction": 0.5},
		map[string]interface{}{"atr_multiple": 3, "close_fraction": 1.0},
	})
	pos := &Position{
		Symbol:                   "ETH",
		Quantity:                 0.5, // half of initial = TP1 filled
		InitialQuantity:          1.0,
		AvgCost:                  100,
		EntryATR:                 5,
		Side:                     "long",
		StopLossOID:              111,
		StopLossTriggerPx:        95,
		TPOIDs:                   []int64{0, 222}, // tier 0 filled
		TPArmedTiers:             []bool{true, true},
		SLAdjustedTiersProcessed: 0,
	}
	state := &StrategyState{ID: sc.ID, Positions: map[string]*Position{"ETH": pos}}
	var mu sync.RWMutex

	if !runPostTPStopLossAdjustment(sc, state, "ETH", 105, nil, &mu, nil, nil, nil) {
		t.Fatal("expected runPostTPStopLossAdjustment to apply")
	}

	if gotSymbol != "ETH" || gotSide != "long" || gotQty != 0.5 || gotTrigger != 100 || gotCancelOID != 111 {
		t.Fatalf("update args=(%s,%s,%v,%v,%d), want (ETH,long,0.5,100,111)",
			gotSymbol, gotSide, gotQty, gotTrigger, gotCancelOID)
	}
	if pos.StopLossOID != 999 {
		t.Errorf("StopLossOID=%d, want 999", pos.StopLossOID)
	}
	if pos.StopLossTriggerPx != 100 {
		t.Errorf("StopLossTriggerPx=%v, want 100 (breakeven)", pos.StopLossTriggerPx)
	}
	if pos.SLAdjustedTiersProcessed != 1 {
		t.Errorf("SLAdjustedTiersProcessed=%d, want 1", pos.SLAdjustedTiersProcessed)
	}
}

func TestRunPostTPStopLossAdjustment_Idempotent(t *testing.T) {
	old := runHyperliquidUpdateStopLossFunc
	defer func() { runHyperliquidUpdateStopLossFunc = old }()

	calls := 0
	runHyperliquidUpdateStopLossFunc = func(string, string, string, float64, float64, int64) (*HyperliquidStopLossUpdateResult, string, error) {
		calls++
		return &HyperliquidStopLossUpdateResult{StopLossOID: 999, StopLossTriggerPx: 100}, "", nil
	}

	sc := postTPSLTestStrategy("breakeven", []interface{}{
		map[string]interface{}{"atr_multiple": 2, "close_fraction": 0.5},
		map[string]interface{}{"atr_multiple": 3, "close_fraction": 1.0},
	})
	pos := &Position{
		Symbol: "ETH", Quantity: 0.5, InitialQuantity: 1.0,
		AvgCost: 100, EntryATR: 5, Side: "long",
		StopLossOID: 111, StopLossTriggerPx: 95,
		TPOIDs:                   []int64{0, 222},
		TPArmedTiers:             []bool{true, true},
		SLAdjustedTiersProcessed: 0,
	}
	state := &StrategyState{ID: sc.ID, Positions: map[string]*Position{"ETH": pos}}
	var mu sync.RWMutex

	runPostTPStopLossAdjustment(sc, state, "ETH", 105, nil, &mu, nil, nil, nil)
	runPostTPStopLossAdjustment(sc, state, "ETH", 105, nil, &mu, nil, nil, nil)
	if calls != 1 {
		t.Fatalf("expected exactly 1 subprocess call, got %d", calls)
	}
}

func TestRunPostTPStopLossAdjustment_TrailFromHereTransition(t *testing.T) {
	old := runHyperliquidUpdateStopLossFunc
	defer func() { runHyperliquidUpdateStopLossFunc = old }()

	runHyperliquidUpdateStopLossFunc = func(_, _, _ string, _, triggerPx float64, _ int64) (*HyperliquidStopLossUpdateResult, string, error) {
		return &HyperliquidStopLossUpdateResult{StopLossOID: 999, StopLossTriggerPx: triggerPx}, "", nil
	}

	sc := postTPSLTestStrategy(nil, []interface{}{
		map[string]interface{}{"atr_multiple": 2, "close_fraction": 0.5, "sl_after": map[string]interface{}{
			"trail_from_here": map[string]interface{}{"atr_mult": 1.0},
		}},
		map[string]interface{}{"atr_multiple": 3, "close_fraction": 1.0},
	})
	pos := &Position{
		Symbol: "ETH", Quantity: 0.5, InitialQuantity: 1.0,
		AvgCost: 100, EntryATR: 5, Side: "long",
		StopLossOID: 111, StopLossTriggerPx: 95,
		TPOIDs:                   []int64{0, 222},
		TPArmedTiers:             []bool{true, true},
		SLAdjustedTiersProcessed: 0,
	}
	state := &StrategyState{ID: sc.ID, Positions: map[string]*Position{"ETH": pos}}
	var mu sync.RWMutex

	if !runPostTPStopLossAdjustment(sc, state, "ETH", 110, nil, &mu, nil, nil, nil) {
		t.Fatal("expected runPostTPStopLossAdjustment to apply")
	}
	// trail_from_here at mark=110, ATR=5, mult=1.0 → trigger = 110 - 5 = 105
	if pos.StopLossTriggerPx != 105 {
		t.Errorf("StopLossTriggerPx=%v, want 105", pos.StopLossTriggerPx)
	}
	if pos.PostTPTrailingATRMult == nil || *pos.PostTPTrailingATRMult != 1.0 {
		t.Errorf("PostTPTrailingATRMult=%v, want 1.0", pos.PostTPTrailingATRMult)
	}
	if pos.StopLossHighWaterPx != 110 {
		t.Errorf("StopLossHighWaterPx=%v, want 110 (seeded at mark)", pos.StopLossHighWaterPx)
	}
}

func TestRunPostTPStopLossAdjustment_TrailDefersWithoutMark(t *testing.T) {
	old := runHyperliquidUpdateStopLossFunc
	defer func() { runHyperliquidUpdateStopLossFunc = old }()

	calls := 0
	runHyperliquidUpdateStopLossFunc = func(string, string, string, float64, float64, int64) (*HyperliquidStopLossUpdateResult, string, error) {
		calls++
		return &HyperliquidStopLossUpdateResult{}, "", nil
	}

	sc := postTPSLTestStrategy(map[string]interface{}{
		"trail_from_here": map[string]interface{}{"atr_mult": 1.0},
	}, []interface{}{
		map[string]interface{}{"atr_multiple": 2, "close_fraction": 0.5},
		map[string]interface{}{"atr_multiple": 3, "close_fraction": 1.0},
	})
	pos := &Position{
		Symbol: "ETH", Quantity: 0.5, InitialQuantity: 1.0,
		AvgCost: 100, EntryATR: 5, Side: "long",
		StopLossOID: 111, StopLossTriggerPx: 95,
		TPOIDs:                   []int64{0, 222},
		TPArmedTiers:             []bool{true, true},
		SLAdjustedTiersProcessed: 0,
	}
	state := &StrategyState{ID: sc.ID, Positions: map[string]*Position{"ETH": pos}}
	var mu sync.RWMutex

	if runPostTPStopLossAdjustment(sc, state, "ETH", 0, nil, &mu, nil, nil, nil) {
		t.Fatal("expected runPostTPStopLossAdjustment to defer without mark")
	}
	if calls != 0 {
		t.Errorf("subprocess should not be called when mark missing; got %d calls", calls)
	}
	if pos.SLAdjustedTiersProcessed != 0 {
		t.Errorf("watermark should not advance when deferring; got %d", pos.SLAdjustedTiersProcessed)
	}
}

func TestRunPostTPStopLossAdjustment_NoRulesShortCircuits(t *testing.T) {
	old := runHyperliquidUpdateStopLossFunc
	defer func() { runHyperliquidUpdateStopLossFunc = old }()

	calls := 0
	runHyperliquidUpdateStopLossFunc = func(string, string, string, float64, float64, int64) (*HyperliquidStopLossUpdateResult, string, error) {
		calls++
		return &HyperliquidStopLossUpdateResult{}, "", nil
	}

	sc := postTPSLTestStrategy(nil, []interface{}{
		map[string]interface{}{"atr_multiple": 2, "close_fraction": 0.5},
		map[string]interface{}{"atr_multiple": 3, "close_fraction": 1.0},
	})
	pos := &Position{
		Symbol: "ETH", Quantity: 0.5, InitialQuantity: 1.0,
		AvgCost: 100, EntryATR: 5, Side: "long",
		StopLossOID: 111, TPOIDs: []int64{0, 222}, TPArmedTiers: []bool{true, true},
	}
	state := &StrategyState{ID: sc.ID, Positions: map[string]*Position{"ETH": pos}}
	var mu sync.RWMutex

	if runPostTPStopLossAdjustment(sc, state, "ETH", 105, nil, &mu, nil, nil, nil) {
		t.Fatal("expected runPostTPStopLossAdjustment to return false when no rules configured")
	}
	if calls != 0 {
		t.Errorf("subprocess should not be called; got %d calls", calls)
	}
}

func TestRunPostTPStopLossAdjustment_DefersWhenSLNotArmed(t *testing.T) {
	old := runHyperliquidUpdateStopLossFunc
	defer func() { runHyperliquidUpdateStopLossFunc = old }()

	calls := 0
	runHyperliquidUpdateStopLossFunc = func(string, string, string, float64, float64, int64) (*HyperliquidStopLossUpdateResult, string, error) {
		calls++
		return &HyperliquidStopLossUpdateResult{}, "", nil
	}

	sc := postTPSLTestStrategy("breakeven", []interface{}{
		map[string]interface{}{"atr_multiple": 2, "close_fraction": 0.5},
		map[string]interface{}{"atr_multiple": 3, "close_fraction": 1.0},
	})
	pos := &Position{
		Symbol: "ETH", Quantity: 0.5, InitialQuantity: 1.0,
		AvgCost: 100, EntryATR: 5, Side: "long",
		StopLossOID:  0, // not yet armed
		TPOIDs:       []int64{0, 222},
		TPArmedTiers: []bool{true, true},
	}
	state := &StrategyState{ID: sc.ID, Positions: map[string]*Position{"ETH": pos}}
	var mu sync.RWMutex

	if runPostTPStopLossAdjustment(sc, state, "ETH", 105, nil, &mu, nil, nil, nil) {
		t.Fatal("expected defer when SL OID is 0")
	}
	if calls != 0 {
		t.Errorf("subprocess should not be called; got %d", calls)
	}
}

// #714: SL replace must cap at on-chain qty when virtual > on-chain (e.g. a
// manual TP shrank on-chain before the reconciler caught up). Mirrors the
// trailing/fixed-ATR SL placement sites that already use hlSLEffectiveQty.
func TestRunPostTPStopLossAdjustment_CapsAtOnChainQty(t *testing.T) {
	old := runHyperliquidUpdateStopLossFunc
	defer func() { runHyperliquidUpdateStopLossFunc = old }()

	var gotQty float64
	runHyperliquidUpdateStopLossFunc = func(_, _, _ string, size, triggerPx float64, _ int64) (*HyperliquidStopLossUpdateResult, string, error) {
		gotQty = size
		return &HyperliquidStopLossUpdateResult{StopLossOID: 999, StopLossTriggerPx: triggerPx}, "", nil
	}

	sc := postTPSLTestStrategy("breakeven", []interface{}{
		map[string]interface{}{"atr_multiple": 2, "close_fraction": 0.5},
		map[string]interface{}{"atr_multiple": 3, "close_fraction": 1.0},
	})
	pos := &Position{
		Symbol: "ETH", Quantity: 1.0, InitialQuantity: 2.0,
		AvgCost: 100, EntryATR: 5, Side: "long",
		StopLossOID: 111, StopLossTriggerPx: 95,
		TPOIDs:                   []int64{0, 222},
		SLAdjustedTiersProcessed: 0,
	}
	state := &StrategyState{ID: sc.ID, Positions: map[string]*Position{"ETH": pos}}
	var mu sync.RWMutex

	onChain := map[string]float64{"ETH": 0.7}
	if !runPostTPStopLossAdjustment(sc, state, "ETH", 105, nil, &mu, nil, nil, onChain) {
		t.Fatal("expected runPostTPStopLossAdjustment to apply")
	}
	if gotQty != 0.7 {
		t.Fatalf("subprocess size=%v, want 0.7 (capped at on-chain)", gotQty)
	}
}

// Confirms the no-cap path: when on-chain >= virtual (or map is nil/missing),
// the subprocess receives the virtual qty unchanged.
func TestRunPostTPStopLossAdjustment_NoCapWhenOnChainGEVirtual(t *testing.T) {
	old := runHyperliquidUpdateStopLossFunc
	defer func() { runHyperliquidUpdateStopLossFunc = old }()

	var gotQty float64
	runHyperliquidUpdateStopLossFunc = func(_, _, _ string, size, triggerPx float64, _ int64) (*HyperliquidStopLossUpdateResult, string, error) {
		gotQty = size
		return &HyperliquidStopLossUpdateResult{StopLossOID: 999, StopLossTriggerPx: triggerPx}, "", nil
	}

	sc := postTPSLTestStrategy("breakeven", []interface{}{
		map[string]interface{}{"atr_multiple": 2, "close_fraction": 0.5},
		map[string]interface{}{"atr_multiple": 3, "close_fraction": 1.0},
	})
	pos := &Position{
		Symbol: "ETH", Quantity: 1.0, InitialQuantity: 2.0,
		AvgCost: 100, EntryATR: 5, Side: "long",
		StopLossOID: 111, StopLossTriggerPx: 95,
		TPOIDs:                   []int64{0, 222},
		SLAdjustedTiersProcessed: 0,
	}
	state := &StrategyState{ID: sc.ID, Positions: map[string]*Position{"ETH": pos}}
	var mu sync.RWMutex

	if !runPostTPStopLossAdjustment(sc, state, "ETH", 105, nil, &mu, nil, nil, map[string]float64{"ETH": 1.0}) {
		t.Fatal("expected runPostTPStopLossAdjustment to apply")
	}
	if gotQty != 1.0 {
		t.Fatalf("subprocess size=%v, want 1.0 (uncapped)", gotQty)
	}
}

func TestFindHighestClearedTier(t *testing.T) {
	// All cases here assume tiers were armed at some point (`armed` matches
	// `oids` shape, all true). The "never armed" variant is exercised
	// separately in TestFindHighestClearedTier_NeverArmedSkipped (#716 item 2).
	mkArmed := func(oids []int64) []bool {
		out := make([]bool, len(oids))
		for i := range out {
			out[i] = true
		}
		return out
	}
	cases := []struct {
		name      string
		oids      []int64
		from      int
		wantIdx   int
		wantClear bool
	}{
		{"empty", nil, 0, 0, false},
		{"none cleared", []int64{1, 2, 3}, 0, 0, false},
		{"first cleared", []int64{0, 2, 3}, 0, 0, true},
		{"last cleared", []int64{1, 2, 0}, 0, 2, true},
		{"multiple cleared takes highest", []int64{0, 0, 3}, 0, 1, true},
		{"all cleared takes highest", []int64{0, 0, 0}, 0, 2, true},
		{"from idx skips already-processed", []int64{0, 0, 3}, 2, 0, false},
		{"from idx finds later cleared", []int64{0, 2, 0}, 1, 2, true},
		{"negative from clamps to 0", []int64{0, 2, 3}, -5, 0, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gotIdx, gotClear := findHighestClearedTier(tc.oids, mkArmed(tc.oids), tc.from)
			if gotClear != tc.wantClear {
				t.Fatalf("cleared=%v, want %v", gotClear, tc.wantClear)
			}
			if tc.wantClear && gotIdx != tc.wantIdx {
				t.Fatalf("idx=%d, want %d", gotIdx, tc.wantIdx)
			}
		})
	}
}

// #716 item 2 — a tier that was never armed (OID=0 with armed[i]=false) must
// not count as cleared, even if a partial close occurred from some other path.
func TestFindHighestClearedTier_NeverArmedSkipped(t *testing.T) {
	cases := []struct {
		name      string
		oids      []int64
		armed     []bool
		wantIdx   int
		wantClear bool
	}{
		{
			name:  "never armed tier 0, armed tier 1 still resting",
			oids:  []int64{0, 222},
			armed: []bool{false, true},
			// Neither qualifies: tier 0 never armed, tier 1 still has a positive OID.
			wantClear: false,
		},
		{
			name:      "tier 0 filled (was armed), tier 1 still resting",
			oids:      []int64{0, 222},
			armed:     []bool{true, true},
			wantIdx:   0,
			wantClear: true,
		},
		{
			name:      "tier 0 never armed, tier 1 filled",
			oids:      []int64{0, 0},
			armed:     []bool{false, true},
			wantIdx:   1,
			wantClear: true,
		},
		{
			name:      "armed slice shorter than oids (legacy/transitional)",
			oids:      []int64{0, 0, 0},
			armed:     []bool{true, true},
			wantIdx:   1,
			wantClear: true,
		},
		{
			name:      "armed slice nil (legacy row before backfill)",
			oids:      []int64{0, 0},
			armed:     nil,
			wantClear: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gotIdx, gotClear := findHighestClearedTier(tc.oids, tc.armed, 0)
			if gotClear != tc.wantClear {
				t.Fatalf("cleared=%v, want %v", gotClear, tc.wantClear)
			}
			if tc.wantClear && gotIdx != tc.wantIdx {
				t.Fatalf("idx=%d, want %d", gotIdx, tc.wantIdx)
			}
		})
	}
}

func TestValidatePostTPStopLossRules_RejectsSLAfterOnNonTieredCloseRef(t *testing.T) {
	atrSL := 1.0
	sc := StrategyConfig{
		Type:            "perps",
		Platform:        "hyperliquid",
		StopLossATRMult: &atrSL,
		CloseStrategies: []StrategyRef{{
			Name: "tp_at_pct",
			Params: map[string]interface{}{
				"pct":      0.05,
				"sl_after": "breakeven", // not honored — should be flagged
			},
		}},
	}
	errs := validatePostTPStopLossRules(sc)
	found := false
	for _, e := range errs {
		if strings.Contains(e, "only honored on tiered_tp_atr") {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected rejection of sl_after on non-tiered close ref, got %v", errs)
	}
}

func TestValidatePostTPStopLossRules_RejectsSLAfterOnNonTieredTier(t *testing.T) {
	atrSL := 1.0
	sc := StrategyConfig{
		Type:            "perps",
		Platform:        "hyperliquid",
		StopLossATRMult: &atrSL,
		CloseStrategies: []StrategyRef{{
			Name: "tiered_tp_pct", // not the ATR variant
			Params: map[string]interface{}{
				"tiers": []interface{}{
					map[string]interface{}{"pct": 0.05, "close_fraction": 0.5, "sl_after": "breakeven"},
				},
			},
		}},
	}
	errs := validatePostTPStopLossRules(sc)
	found := false
	for _, e := range errs {
		if strings.Contains(e, "no effect") && strings.Contains(e, "tiered_tp_pct") {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected rejection of per-tier sl_after under non-tiered ref, got %v", errs)
	}
}

func TestValidatePostTPStopLossRules_NoOpWhenAbsent(t *testing.T) {
	atrSL := 1.0
	sc := StrategyConfig{
		Type:            "perps",
		Platform:        "hyperliquid",
		StopLossATRMult: &atrSL,
		CloseStrategies: []StrategyRef{{
			Name: "tiered_tp_atr_live",
			Params: map[string]interface{}{
				"tiers": []interface{}{
					map[string]interface{}{"atr_multiple": 2, "close_fraction": 0.5},
					map[string]interface{}{"atr_multiple": 3, "close_fraction": 1.0},
				},
			},
		}},
	}
	if errs := validatePostTPStopLossRules(sc); len(errs) != 0 {
		t.Fatalf("expected no errors when sl_after is absent, got %v", errs)
	}
}

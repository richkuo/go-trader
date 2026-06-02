package main

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func ratchetPtr(v float64) *float64 { return &v }

func ratchetTier(fields map[string]interface{}) map[string]interface{} { return fields }

// ratchetStrategy builds a perps trailing_tp_ratchet strategy with the given
// close-ref params and a positive initial trailing_stop_atr_mult.
func ratchetStrategy(name string, params map[string]interface{}) StrategyConfig {
	return StrategyConfig{
		ID:                  "hl-ratchet",
		Platform:            "hyperliquid",
		Type:                "perps",
		Script:              "shared_scripts/check_hyperliquid.py",
		TrailingStopATRMult: ratchetPtr(3.0),
		CloseStrategy:       &StrategyRef{Name: name, Params: params},
	}
}

func ratchetState(pos *Position) *StrategyState {
	return &StrategyState{ID: "hl-ratchet", Positions: map[string]*Position{"ETH": pos}}
}

// ---------------------------------------------------------------------------
// Tier parsing
// ---------------------------------------------------------------------------

func TestParseTrailingTPRatchetTierList_ValidAndSorted(t *testing.T) {
	raw := []interface{}{
		ratchetTier(map[string]interface{}{"atr_multiple": 2.0, "close_fraction": 0.3, "tp_atr_fraction": 0.5}),
		ratchetTier(map[string]interface{}{"atr_multiple": 1.0, "trailing_mult_after": 2.0}),
	}
	tiers, errs := parseTrailingTPRatchetTierList(raw, "ctx")
	if len(errs) != 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
	if len(tiers) != 2 || tiers[0].ATRMultiple != 1.0 || tiers[1].ATRMultiple != 2.0 {
		t.Fatalf("tiers not sorted ascending: %+v", tiers)
	}
	if tiers[0].CloseFraction != 0.0 || tiers[0].TrailMultAfter != 2.0 {
		t.Errorf("tier0 wrong: %+v", tiers[0])
	}
	// Absolute form resolves to itself; relative form resolves to fraction*mult.
	if got := tiers[0].resolvedTrailMult(); got != 2.0 {
		t.Errorf("tier0 resolvedTrailMult=%v want 2.0", got)
	}
	if got := tiers[1].resolvedTrailMult(); got != 1.0 { // 0.5 * 2.0
		t.Errorf("tier1 resolvedTrailMult=%v want 1.0", got)
	}
}

func TestParseTrailingTPRatchetTierList_Errors(t *testing.T) {
	cases := []struct {
		name string
		tier map[string]interface{}
		want string
	}{
		{"both trail specs", map[string]interface{}{"atr_multiple": 1.0, "trailing_mult_after": 2.0, "tp_atr_fraction": 0.5}, "exactly one"},
		{"neither trail spec", map[string]interface{}{"atr_multiple": 1.0}, "exactly one"},
		{"bad atr_multiple", map[string]interface{}{"atr_multiple": 0.0, "trailing_mult_after": 1.0}, "atr_multiple must be a positive"},
		{"bad close_fraction", map[string]interface{}{"atr_multiple": 1.0, "close_fraction": 1.5, "trailing_mult_after": 1.0}, "close_fraction must be"},
		{"bad trailing_mult_after", map[string]interface{}{"atr_multiple": 1.0, "trailing_mult_after": -1.0}, "trailing_mult_after must be a positive"},
		{"unknown key", map[string]interface{}{"atr_multiple": 1.0, "trailing_mult_after": 1.0, "bogus": 1}, "unknown key"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, errs := parseTrailingTPRatchetTierList([]interface{}{tc.tier}, "ctx")
			if !containsSubstr(errs, tc.want) {
				t.Fatalf("errors %v do not contain %q", errs, tc.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Cleared-tier resolution
// ---------------------------------------------------------------------------

func TestHighestClearedTrailingTPRatchetMult(t *testing.T) {
	tiers := []trailingTPRatchetTier{
		{ATRMultiple: 1.0, TrailMultAfter: 2.0},
		{ATRMultiple: 2.0, TrailMultAfter: 1.0},
	}
	if m, ok := highestClearedTrailingTPRatchetMult(tiers, 0.5); ok {
		t.Errorf("atr_profit 0.5 should clear nothing, got %v", m)
	}
	if m, ok := highestClearedTrailingTPRatchetMult(tiers, 1.5); !ok || m != 2.0 {
		t.Errorf("atr_profit 1.5 should clear tier0 (mult 2.0), got %v ok=%v", m, ok)
	}
	if m, ok := highestClearedTrailingTPRatchetMult(tiers, 3.0); !ok || m != 1.0 {
		t.Errorf("atr_profit 3.0 should clear tier1 (mult 1.0), got %v ok=%v", m, ok)
	}
}

// ---------------------------------------------------------------------------
// Cycle adjustment: stamping + monotonic tightening
// ---------------------------------------------------------------------------

func ratchetRunStrategy() StrategyConfig {
	return ratchetStrategy("trailing_tp_ratchet", map[string]interface{}{
		"tp_tiers": []interface{}{
			ratchetTier(map[string]interface{}{"atr_multiple": 1.0, "trailing_mult_after": 2.0}),
			ratchetTier(map[string]interface{}{"atr_multiple": 2.0, "trailing_mult_after": 1.0}),
		},
	})
}

func TestRunTrailingTPRatchetAdjustment_StampsAndTightens(t *testing.T) {
	sc := ratchetRunStrategy()
	pos := &Position{Side: "long", AvgCost: 100, EntryATR: 5, Quantity: 1, InitialQuantity: 1}
	state := ratchetState(pos)
	var mu sync.RWMutex

	// No tier cleared yet (mark 102 -> atr_profit 0.4): no stamp.
	if got := runTrailingTPRatchetAdjustment(sc, state, "ETH", 102, &mu, nil); got != nil {
		t.Fatalf("expected nil (no tier cleared), got %v", *got)
	}
	if pos.PostTPTrailingATRMult != nil {
		t.Fatalf("PostTPTrailingATRMult should be nil, got %v", *pos.PostTPTrailingATRMult)
	}

	// Tier0 cleared (mark 106 -> atr_profit 1.2): stamp 2.0.
	got := runTrailingTPRatchetAdjustment(sc, state, "ETH", 106, &mu, nil)
	if got == nil || *got != 2.0 || pos.PostTPTrailingATRMult == nil || *pos.PostTPTrailingATRMult != 2.0 {
		t.Fatalf("after tier0: want 2.0, got ret=%v stamped=%v", got, pos.PostTPTrailingATRMult)
	}

	// Tier1 cleared (mark 112 -> atr_profit 2.4): tighten to 1.0.
	got = runTrailingTPRatchetAdjustment(sc, state, "ETH", 112, &mu, nil)
	if got == nil || *got != 1.0 || *pos.PostTPTrailingATRMult != 1.0 {
		t.Fatalf("after tier1: want 1.0, got ret=%v stamped=%v", got, *pos.PostTPTrailingATRMult)
	}

	// Price falls back to tier0 level: monotonic — must NOT loosen back to 2.0.
	got = runTrailingTPRatchetAdjustment(sc, state, "ETH", 106, &mu, nil)
	if got == nil || *got != 1.0 || *pos.PostTPTrailingATRMult != 1.0 {
		t.Fatalf("monotonic violated: want stay 1.0, got ret=%v stamped=%v", got, *pos.PostTPTrailingATRMult)
	}
}

func TestRunTrailingTPRatchetAdjustment_ShortSide(t *testing.T) {
	sc := ratchetRunStrategy()
	pos := &Position{Side: "short", AvgCost: 100, EntryATR: 5, Quantity: 1, InitialQuantity: 1}
	state := ratchetState(pos)
	var mu sync.RWMutex
	// Short profit when mark falls: mark 90 -> atr_profit (100-90)/5 = 2.0 -> tier1 -> 1.0.
	got := runTrailingTPRatchetAdjustment(sc, state, "ETH", 90, &mu, nil)
	if got == nil || *got != 1.0 {
		t.Fatalf("short: want 1.0, got %v", got)
	}
}

func TestRunTrailingTPRatchetAdjustment_FractionForm(t *testing.T) {
	sc := ratchetStrategy("trailing_tp_ratchet", map[string]interface{}{
		"tp_tiers": []interface{}{
			ratchetTier(map[string]interface{}{"atr_multiple": 2.0, "tp_atr_fraction": 0.5}),
		},
	})
	pos := &Position{Side: "long", AvgCost: 100, EntryATR: 5, Quantity: 1, InitialQuantity: 1}
	state := ratchetState(pos)
	var mu sync.RWMutex
	// mark 110 -> atr_profit 2.0 -> tier clears; trail = 0.5 * 2.0 = 1.0.
	got := runTrailingTPRatchetAdjustment(sc, state, "ETH", 110, &mu, nil)
	if got == nil || *got != 1.0 {
		t.Fatalf("fraction form: want 1.0, got %v", got)
	}
}

func TestRunTrailingTPRatchetAdjustment_RegimeFrozenAtOpen(t *testing.T) {
	sc := ratchetStrategy("trailing_tp_ratchet_regime", map[string]interface{}{
		"tp_tiers": map[string]interface{}{
			"trending_up": []interface{}{ratchetTier(map[string]interface{}{"atr_multiple": 1.0, "trailing_mult_after": 2.0})},
			"ranging":     []interface{}{ratchetTier(map[string]interface{}{"atr_multiple": 1.0, "trailing_mult_after": 0.5})},
		},
	})
	// Position opened in 'ranging' -> uses the ranging table (0.5).
	pos := &Position{Side: "long", AvgCost: 100, EntryATR: 5, Quantity: 1, InitialQuantity: 1, Regime: "ranging"}
	state := ratchetState(pos)
	var mu sync.RWMutex
	got := runTrailingTPRatchetAdjustment(sc, state, "ETH", 106, &mu, nil)
	if got == nil || *got != 0.5 {
		t.Fatalf("regime ranging: want 0.5, got %v", got)
	}
}

func TestRunTrailingTPRatchetAdjustment_NoOps(t *testing.T) {
	var mu sync.RWMutex
	// Not a ratchet strategy.
	plain := StrategyConfig{Platform: "hyperliquid", Type: "perps"}
	pos := &Position{Side: "long", AvgCost: 100, EntryATR: 5, Quantity: 1, InitialQuantity: 1}
	if got := runTrailingTPRatchetAdjustment(plain, ratchetState(pos), "ETH", 110, &mu, nil); got != nil {
		t.Errorf("non-ratchet strategy should no-op, got %v", *got)
	}
	// Missing entry ATR.
	sc := ratchetRunStrategy()
	noATR := &Position{Side: "long", AvgCost: 100, EntryATR: 0, Quantity: 1, InitialQuantity: 1}
	if got := runTrailingTPRatchetAdjustment(sc, ratchetState(noATR), "ETH", 110, &mu, nil); got != nil {
		t.Errorf("missing entry ATR should no-op, got %v", *got)
	}
	// Regime form but position regime not in table.
	scR := ratchetStrategy("trailing_tp_ratchet_regime", map[string]interface{}{
		"tp_tiers": map[string]interface{}{"trending_up": []interface{}{ratchetTier(map[string]interface{}{"atr_multiple": 1.0, "trailing_mult_after": 2.0})}},
	})
	posR := &Position{Side: "long", AvgCost: 100, EntryATR: 5, Quantity: 1, InitialQuantity: 1, Regime: "ranging"}
	if got := runTrailingTPRatchetAdjustment(scR, ratchetState(posR), "ETH", 110, &mu, nil); got != nil {
		t.Errorf("unconfigured regime should no-op, got %v", *got)
	}
}

// ---------------------------------------------------------------------------
// Validation
// ---------------------------------------------------------------------------

func TestValidateTrailingTPRatchetCloseRef_Plain(t *testing.T) {
	labels := []string{"trending_up", "trending_down", "ranging"}

	// Valid.
	sc := ratchetStrategy("trailing_tp_ratchet", map[string]interface{}{
		"tp_tiers": []interface{}{ratchetTier(map[string]interface{}{"atr_multiple": 1.0, "trailing_mult_after": 2.0})},
	})
	ref, _ := trailingTPRatchetCloseRef(sc)
	if errs, ur := validateTrailingTPRatchetCloseRef(sc, ref, labels, "strategy[hl]"); len(errs) != 0 || ur {
		t.Fatalf("valid plain should pass: errs=%v usesRegime=%v", errs, ur)
	}

	// Missing trailing_stop_atr_mult.
	scNoTrail := sc
	scNoTrail.TrailingStopATRMult = nil
	if errs, _ := validateTrailingTPRatchetCloseRef(scNoTrail, ref, labels, "strategy[hl]"); !containsSubstr(errs, "requires a positive strategy-level trailing_stop_atr_mult") {
		t.Errorf("missing trailing_stop_atr_mult not flagged: %v", errs)
	}

	// Manual rejected (perps only).
	scManual := sc
	scManual.Type = "manual"
	if errs, _ := validateTrailingTPRatchetCloseRef(scManual, ref, labels, "strategy[hl]"); !containsSubstr(errs, "HL perps only") {
		t.Errorf("manual not rejected: %v", errs)
	}
}

func TestValidateTrailingTPRatchetCloseRef_Regime(t *testing.T) {
	labels := []string{"trending_up", "trending_down", "ranging"}

	// Exhaustive + valid.
	full := map[string]interface{}{
		"tp_tiers": map[string]interface{}{
			"trending_up":   []interface{}{ratchetTier(map[string]interface{}{"atr_multiple": 1.0, "trailing_mult_after": 2.0})},
			"trending_down": []interface{}{ratchetTier(map[string]interface{}{"atr_multiple": 1.0, "trailing_mult_after": 1.0})},
			"ranging":       []interface{}{ratchetTier(map[string]interface{}{"atr_multiple": 1.0, "trailing_mult_after": 0.5})},
		},
	}
	sc := ratchetStrategy("trailing_tp_ratchet_regime", full)
	ref, _ := trailingTPRatchetCloseRef(sc)
	if errs, ur := validateTrailingTPRatchetCloseRef(sc, ref, labels, "strategy[hl]"); len(errs) != 0 || !ur {
		t.Fatalf("valid regime should pass + set usesRegime: errs=%v ur=%v", errs, ur)
	}

	// Missing a label.
	partial := map[string]interface{}{
		"tp_tiers": map[string]interface{}{
			"trending_up": []interface{}{ratchetTier(map[string]interface{}{"atr_multiple": 1.0, "trailing_mult_after": 2.0})},
		},
	}
	scPartial := ratchetStrategy("trailing_tp_ratchet_regime", partial)
	refP, _ := trailingTPRatchetCloseRef(scPartial)
	if errs, _ := validateTrailingTPRatchetCloseRef(scPartial, refP, labels, "strategy[hl]"); !containsSubstr(errs, "missing tier table for regime") {
		t.Errorf("missing label not flagged: %v", errs)
	}

	// Unknown label.
	bad := map[string]interface{}{
		"tp_tiers": map[string]interface{}{
			"trending_up":   []interface{}{ratchetTier(map[string]interface{}{"atr_multiple": 1.0, "trailing_mult_after": 2.0})},
			"trending_down": []interface{}{ratchetTier(map[string]interface{}{"atr_multiple": 1.0, "trailing_mult_after": 1.0})},
			"ranging":       []interface{}{ratchetTier(map[string]interface{}{"atr_multiple": 1.0, "trailing_mult_after": 0.5})},
			"bogus_regime":  []interface{}{ratchetTier(map[string]interface{}{"atr_multiple": 1.0, "trailing_mult_after": 0.5})},
		},
	}
	scBad := ratchetStrategy("trailing_tp_ratchet_regime", bad)
	refB, _ := trailingTPRatchetCloseRef(scBad)
	if errs, _ := validateTrailingTPRatchetCloseRef(scBad, refB, labels, "strategy[hl]"); !containsSubstr(errs, "not in the window vocabulary") {
		t.Errorf("unknown label not flagged: %v", errs)
	}
}

func TestTrailingTPRatchetParamsEqualForReload(t *testing.T) {
	mk := func(mult float64) StrategyConfig {
		return ratchetStrategy("trailing_tp_ratchet", map[string]interface{}{
			"tp_tiers": []interface{}{ratchetTier(map[string]interface{}{"atr_multiple": 1.0, "trailing_mult_after": mult})},
		})
	}
	if !trailingTPRatchetParamsEqualForReload(mk(2.0), mk(2.0)) {
		t.Error("identical ratchet params should be equal")
	}
	if trailingTPRatchetParamsEqualForReload(mk(2.0), mk(1.0)) {
		t.Error("changed tier table should not be equal")
	}
}

func TestValidateRegimeATRConfig_RatchetIntegration(t *testing.T) {
	// Plain form, no regime config: validates cleanly (no regime.enabled needed).
	plain := ratchetStrategy("trailing_tp_ratchet", map[string]interface{}{
		"tp_tiers": []interface{}{ratchetTier(map[string]interface{}{"atr_multiple": 1.0, "trailing_mult_after": 2.0})},
	})
	if errs := validateRegimeATRConfig(&Config{Strategies: []StrategyConfig{plain}}); len(errs) != 0 {
		t.Fatalf("plain ratchet (no regime) should validate, got: %v", errs)
	}

	// Regime form under an ADX window covering all 3 labels: validates.
	adxTiers := map[string]interface{}{}
	for _, l := range canonicalTrendRegimeLabels {
		adxTiers[l] = []interface{}{ratchetTier(map[string]interface{}{"atr_multiple": 1.0, "trailing_mult_after": 2.0})}
	}
	scReg := ratchetStrategy("trailing_tp_ratchet_regime", map[string]interface{}{"tp_tiers": adxTiers})
	scReg.RegimeATRWindow = "daily"
	regCfg := &Config{
		Regime:     &RegimeConfig{Enabled: true, Windows: RegimeWindowsMap{"daily": {Classifier: regimeClassifierADX, Period: 14}}},
		Strategies: []StrategyConfig{scReg},
	}
	if errs := validateRegimeATRConfig(regCfg); len(errs) != 0 {
		t.Fatalf("regime ratchet should validate, got: %v", errs)
	}

	// Regime form but regime disabled: must require regime.enabled=true.
	if errs := validateRegimeATRConfig(&Config{Strategies: []StrategyConfig{scReg}}); !containsSubstr(errs, "regime.enabled=true") {
		t.Fatalf("regime ratchet without regime.enabled should error, got: %v", errs)
	}
}

// TestLoadConfig_TrailingTPRatchet exercises the full load path (migration +
// unknown-key guard + validateConfig) for a real ratchet config.
func TestLoadConfig_TrailingTPRatchet(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.json")
	dbPath := filepath.Join(dir, "state.db")
	cfgBody := `{
		"db_file": "` + strings.ReplaceAll(dbPath, "\\", "\\\\") + `",
		"strategies": [{
			"id": "hl-ratchet",
			"type": "perps",
			"platform": "hyperliquid",
			"script": "shared_scripts/check_hyperliquid.py",
			"args": ["donchian_breakout", "BTC", "1h", "--mode=paper"],
			"capital": 1000,
			"max_drawdown_pct": 25,
			"leverage": 1,
			"trailing_stop_atr_mult": 3.0,
			"close_strategy": {
				"name": "trailing_tp_ratchet",
				"params": {"tp_tiers": [
					{"atr_multiple": 1.0, "close_fraction": 0.0, "trailing_mult_after": 2.0},
					{"atr_multiple": 2.0, "close_fraction": 0.3, "tp_atr_fraction": 0.5}
				]}
			}
		}]
	}`
	if err := os.WriteFile(cfgPath, []byte(cfgBody), 0644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	cfg, err := LoadConfig(cfgPath)
	if err != nil {
		t.Fatalf("LoadConfig must accept a trailing_tp_ratchet config, got: %v", err)
	}
	sc := cfg.Strategies[0]
	if !strategyUsesTrailingTPRatchetClose(sc) {
		t.Fatalf("close ref not recognized as trailing_tp_ratchet: %+v", sc.CloseStrategy)
	}
	tiers, ok := resolveTrailingTPRatchetTiers(sc, "")
	if !ok || len(tiers) != 2 {
		t.Fatalf("tiers did not resolve after load: ok=%v tiers=%+v", ok, tiers)
	}
	if tiers[1].resolvedTrailMult() != 1.0 { // tp_atr_fraction 0.5 * atr_multiple 2.0
		t.Errorf("tier[1] trail mult = %v, want 1.0", tiers[1].resolvedTrailMult())
	}
}

func containsSubstr(errs []string, want string) bool {
	for _, e := range errs {
		if strings.Contains(e, want) {
			return true
		}
	}
	return false
}

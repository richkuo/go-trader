package main

import (
	"encoding/json"
	"testing"
)

// twoProfileAlloc builds a resolved allocation with two profiles for the
// composite vocabulary used by most tests.
func twoProfileAlloc(confirm int) *RegimeProfileAllocation {
	return &RegimeProfileAllocation{
		Window: "profile_long",
		Profiles: map[string]string{
			"trending_up_clean":    "trend",
			"trending_up_choppy":   "trend",
			"trending_down_clean":  "trend",
			"trending_down_choppy": "trend",
			"ranging_quiet":        "fade",
			"ranging_volatile":     "fade",
			"ranging_directional":  "fade",
		},
		ParamSets: map[string]map[string]interface{}{
			"fade":  {"trend_entry": "off"},
			"trend": {"trend_entry": "breakout", "trend_drift_confirm": 0.1},
		},
		ConfirmBars:    3,
		InitialProfile: "fade",
	}
}

func TestResolveRegimeProfile_ColdStartNoInstantSwitch(t *testing.T) {
	alloc := twoProfileAlloc(3)
	// Cold start (prev=nil), flat, regime says "trend" — must NOT switch this
	// bar; active stays at initial fade and the counter starts at 1.
	active, next := resolveRegimeProfile(alloc, "trending_up_clean", "t0", nil, 0, "")
	if active != "fade" {
		t.Fatalf("cold start active=%q, want fade", active)
	}
	if next.ActiveProfile != "fade" || next.PendingProfile != "trend" || next.PendingBarsSeen != 1 {
		t.Fatalf("cold start next=%+v, want active=fade pending=trend seen=1", next)
	}
}

func TestResolveRegimeProfile_FlatSwitchAfterConfirmBars(t *testing.T) {
	alloc := twoProfileAlloc(3)
	state := &RegimeProfileState{ActiveProfile: "fade"}
	bars := []string{"t1", "t2", "t3"}
	var active string
	for i, bt := range bars {
		active, *state = resolveRegimeProfile(alloc, "trending_up_clean", bt, state, 0, "")
		if i < 2 && active != "fade" {
			t.Fatalf("bar %d active=%q, want fade (not yet confirmed)", i, active)
		}
	}
	// Third confirming bar commits the switch.
	if active != "trend" {
		t.Fatalf("after 3 confirm bars active=%q, want trend", active)
	}
	if state.ActiveProfile != "trend" || state.PendingProfile != "" || state.PendingBarsSeen != 0 {
		t.Fatalf("post-switch state=%+v, want active=trend pending='' seen=0", *state)
	}
}

func TestResolveRegimeProfile_BarNotAdvancedDoesNotCount(t *testing.T) {
	alloc := twoProfileAlloc(3)
	state := &RegimeProfileState{ActiveProfile: "fade"}
	// Same barTime across three scheduler cycles: counter must not advance.
	for i := 0; i < 3; i++ {
		_, *state = resolveRegimeProfile(alloc, "trending_up_clean", "t1", state, 0, "")
	}
	if state.PendingBarsSeen != 1 {
		t.Fatalf("within-bar cycles advanced counter to %d, want 1", state.PendingBarsSeen)
	}
}

func TestResolveRegimeProfile_OpenPositionFreezesAndDefers(t *testing.T) {
	alloc := twoProfileAlloc(3)
	state := &RegimeProfileState{ActiveProfile: "fade"}
	// Position open the whole time; regime is trend for many bars. The active
	// profile must stay frozen to the position's open profile (fade), but the
	// counter accrues so the first flat bar commits immediately.
	for i, bt := range []string{"t1", "t2", "t3", "t4"} {
		active, ns := resolveRegimeProfile(alloc, "trending_up_clean", bt, state, 1, "fade")
		*state = ns
		if active != "fade" {
			t.Fatalf("bar %d open active=%q, want frozen fade", i, active)
		}
		if state.ActiveProfile != "fade" {
			t.Fatalf("bar %d committed a switch while open: %+v", i, *state)
		}
	}
	if state.PendingBarsSeen < 3 {
		t.Fatalf("counter did not accrue while open: seen=%d", state.PendingBarsSeen)
	}
	// Now flat on the next bar: switch commits immediately.
	active, ns := resolveRegimeProfile(alloc, "trending_up_clean", "t5", state, 0, "")
	*state = ns
	if active != "trend" || state.ActiveProfile != "trend" {
		t.Fatalf("first flat bar did not commit: active=%q state=%+v", active, *state)
	}
}

func TestResolveRegimeProfile_EmptyLabelFreezes(t *testing.T) {
	alloc := twoProfileAlloc(3)
	state := &RegimeProfileState{ActiveProfile: "fade", PendingProfile: "trend", PendingBarsSeen: 2}
	// Bundle failure → empty label → freeze; counter and active unchanged.
	active, next := resolveRegimeProfile(alloc, "", "t9", state, 0, "")
	if active != "fade" {
		t.Fatalf("empty-label active=%q, want fade", active)
	}
	if next.PendingBarsSeen != 2 || next.PendingProfile != "trend" {
		t.Fatalf("empty label disturbed counter: %+v", next)
	}
}

func TestResolveRegimeProfile_DesiredEqualsActiveResetsPending(t *testing.T) {
	alloc := twoProfileAlloc(3)
	state := &RegimeProfileState{ActiveProfile: "fade", PendingProfile: "trend", PendingBarsSeen: 2}
	// Regime flips back to the active profile's regime → clear pending switch.
	_, next := resolveRegimeProfile(alloc, "ranging_quiet", "t2", state, 0, "")
	if next.PendingProfile != "" || next.PendingBarsSeen != 0 {
		t.Fatalf("desired==active did not reset pending: %+v", next)
	}
}

func TestApplyRegimeProfileParams_MergesAndDoesNotMutateBase(t *testing.T) {
	alloc := twoProfileAlloc(3)
	base := map[string]interface{}{"trend_entry": "default_value", "shared": 1}
	sc := StrategyConfig{OpenStrategy: StrategyRef{Name: "regime_adaptive_htf", Params: base}}
	applyRegimeProfileParams(&sc, alloc, "trend")
	if got := sc.OpenStrategy.Params["trend_entry"]; got != "breakout" {
		t.Fatalf("override not applied: trend_entry=%v", got)
	}
	if got := sc.OpenStrategy.Params["shared"]; got != 1 {
		t.Fatalf("base param lost: shared=%v", got)
	}
	// The shared base map must be untouched (loop-local sc aliases cfg's map).
	if base["trend_entry"] != "default_value" {
		t.Fatalf("base map mutated: trend_entry=%v", base["trend_entry"])
	}
}

func TestApplyRegimeProfileParams_MissingProfileNoOp(t *testing.T) {
	alloc := twoProfileAlloc(3)
	base := map[string]interface{}{"x": 1}
	sc := StrategyConfig{OpenStrategy: StrategyRef{Params: base}}
	applyRegimeProfileParams(&sc, alloc, "nonexistent")
	if len(sc.OpenStrategy.Params) != 1 || sc.OpenStrategy.Params["x"] != 1 {
		t.Fatalf("missing-profile apply changed params: %+v", sc.OpenStrategy.Params)
	}
}

// resolveRawFromJSON unmarshals a regime_profile_allocation block and resolves
// it against the given classifier labels, returning the errors.
func resolveRawFromJSON(t *testing.T, raw string, labels []string) (*RegimeProfileAllocation, []string) {
	t.Helper()
	var a RegimeProfileAllocation
	if err := json.Unmarshal([]byte(raw), &a); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	return &a, a.ResolveRaw("strategy[x].regime_profile_allocation", labels)
}

var compositeLabels = []string{
	"trending_up_clean", "trending_up_choppy", "trending_down_clean",
	"trending_down_choppy", "ranging_quiet", "ranging_volatile", "ranging_directional",
}

func TestResolveRaw_Valid(t *testing.T) {
	raw := `{
		"window": "profile_long",
		"profiles": {
			"trending_up_clean": "trend", "trending_up_choppy": "trend",
			"trending_down_clean": "trend", "trending_down_choppy": "trend",
			"ranging_quiet": "fade", "ranging_volatile": "fade", "ranging_directional": "fade"
		},
		"param_sets": {"fade": {"trend_entry": "off"}, "trend": {"trend_entry": "breakout"}},
		"confirm_bars": 24,
		"initial_profile": "fade"
	}`
	a, errs := resolveRawFromJSON(t, raw, compositeLabels)
	if len(errs) != 0 {
		t.Fatalf("valid block rejected: %v", errs)
	}
	if a.Window != "profile_long" || a.ConfirmBars != 24 || a.InitialProfile != "fade" {
		t.Fatalf("resolved fields wrong: %+v", a)
	}
}

func TestResolveRaw_WrongParamSetCount(t *testing.T) {
	raw := `{
		"window": "w", "confirm_bars": 24, "initial_profile": "a",
		"profiles": {"trending_up_clean":"a","trending_up_choppy":"a","trending_down_clean":"a","trending_down_choppy":"a","ranging_quiet":"a","ranging_volatile":"a","ranging_directional":"a"},
		"param_sets": {"a": {}, "b": {}, "c": {}}
	}`
	_, errs := resolveRawFromJSON(t, raw, compositeLabels)
	if !containsSubstr(errs, "exactly 2 profiles") {
		t.Fatalf("expected param_sets count error, got %v", errs)
	}
}

func TestResolveRaw_MissingLabelCoverage(t *testing.T) {
	raw := `{
		"window": "w", "confirm_bars": 24, "initial_profile": "fade",
		"profiles": {"trending_up_clean":"trend","ranging_quiet":"fade"},
		"param_sets": {"fade": {}, "trend": {}}
	}`
	_, errs := resolveRawFromJSON(t, raw, compositeLabels)
	if !containsSubstr(errs, "missing mapping for regime label") {
		t.Fatalf("expected label-coverage error, got %v", errs)
	}
}

func TestResolveRaw_BadInitialProfile(t *testing.T) {
	raw := `{
		"window": "w", "confirm_bars": 24, "initial_profile": "ghost",
		"profiles": {"trending_up_clean":"trend","trending_up_choppy":"trend","trending_down_clean":"trend","trending_down_choppy":"trend","ranging_quiet":"fade","ranging_volatile":"fade","ranging_directional":"fade"},
		"param_sets": {"fade": {}, "trend": {}}
	}`
	_, errs := resolveRawFromJSON(t, raw, compositeLabels)
	if !containsSubstr(errs, "initial_profile") {
		t.Fatalf("expected initial_profile error, got %v", errs)
	}
}

func TestResolveRaw_UnknownKeyAndMissingRequired(t *testing.T) {
	raw := `{"window": "w", "bogus": 1}`
	_, errs := resolveRawFromJSON(t, raw, nil)
	if !containsSubstr(errs, "unknown key") {
		t.Fatalf("expected unknown-key error, got %v", errs)
	}
	if !containsSubstr(errs, "missing required key") {
		t.Fatalf("expected missing-required error, got %v", errs)
	}
}

func TestResolveRaw_ProfileValueNotInParamSets(t *testing.T) {
	raw := `{
		"window": "w", "confirm_bars": 24, "initial_profile": "fade",
		"profiles": {"trending_up_clean":"ghost","trending_up_choppy":"trend","trending_down_clean":"trend","trending_down_choppy":"trend","ranging_quiet":"fade","ranging_volatile":"fade","ranging_directional":"fade"},
		"param_sets": {"fade": {}, "trend": {}}
	}`
	_, errs := resolveRawFromJSON(t, raw, compositeLabels)
	if !containsSubstr(errs, "is not a param_sets profile") {
		t.Fatalf("expected profile-reference error, got %v", errs)
	}
}

func TestRegimeProfileAllocation_EqualForReload(t *testing.T) {
	a := twoProfileAlloc(3)
	b := twoProfileAlloc(3)
	if !a.EqualForReload(b) {
		t.Fatal("identical allocations should be equal")
	}
	b.ConfirmBars = 5
	if a.EqualForReload(b) {
		t.Fatal("confirm_bars change should differ")
	}
	c := twoProfileAlloc(3)
	c.ParamSets["trend"]["trend_drift_confirm"] = 0.2
	if a.EqualForReload(c) {
		t.Fatal("param tweak should differ")
	}
	var nilA *RegimeProfileAllocation
	var nilB *RegimeProfileAllocation
	if !nilA.EqualForReload(nilB) {
		t.Fatal("nil==nil should be equal")
	}
	if nilA.EqualForReload(a) {
		t.Fatal("nil vs configured should differ")
	}
}

func TestValidateStrategyRegimeVocabulary_RejectsBadProfileWindow(t *testing.T) {
	var a RegimeProfileAllocation
	if err := json.Unmarshal([]byte(`{
		"window": "does_not_exist",
		"profiles": {"trending_up_clean":"trend","trending_up_choppy":"trend","trending_down_clean":"trend","trending_down_choppy":"trend","ranging_quiet":"fade","ranging_volatile":"fade","ranging_directional":"fade","ranging_directional_up":"fade","ranging_directional_down":"fade"},
		"param_sets": {"fade": {"trend_entry":"off"}, "trend": {"trend_entry":"breakout"}},
		"confirm_bars": 24,
		"initial_profile": "fade"
	}`), &a); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	cfg := &Config{
		Regime: &RegimeConfig{Enabled: true, Windows: RegimeWindowsMap{
			"profile_long":     {Classifier: regimeClassifierComposite, Period: 200},
			"composite_medium": {Classifier: regimeClassifierComposite, Period: 50},
		}},
		Strategies: []StrategyConfig{{ID: "hl-test", RegimeProfileAllocation: &a}},
	}
	errs := validateStrategyRegimeVocabulary(cfg)
	if !containsSubstr(errs, "not found in regime.windows") {
		t.Fatalf("expected window-not-found error, got %v", errs)
	}
}

func TestValidateStrategyRegimeVocabulary_AcceptsGoodProfileAllocation(t *testing.T) {
	var a RegimeProfileAllocation
	if err := json.Unmarshal([]byte(`{
		"window": "profile_long",
		"profiles": {"trending_up_clean":"trend","trending_up_choppy":"trend","trending_down_clean":"trend","trending_down_choppy":"trend","ranging_quiet":"fade","ranging_volatile":"fade","ranging_directional":"fade","ranging_directional_up":"fade","ranging_directional_down":"fade"},
		"param_sets": {"fade": {"trend_entry":"off"}, "trend": {"trend_entry":"breakout"}},
		"confirm_bars": 24,
		"initial_profile": "fade"
	}`), &a); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	cfg := &Config{
		Regime: &RegimeConfig{Enabled: true, Windows: RegimeWindowsMap{
			"profile_long":     {Classifier: regimeClassifierComposite, Period: 200},
			"composite_medium": {Classifier: regimeClassifierComposite, Period: 50},
		}},
		Strategies: []StrategyConfig{{ID: "hl-test", RegimeProfileAllocation: &a}},
	}
	for _, e := range validateStrategyRegimeVocabulary(cfg) {
		if indexOf(e, "regime_profile_allocation") >= 0 {
			t.Errorf("unexpected profile-allocation validation error: %s", e)
		}
	}
}

// DB round-trip: open_profile on a position and active_profile on a strategy
// survive a save+load cycle.
func TestProfileAllocation_DBRoundTrip(t *testing.T) {
	dir := t.TempDir()
	sdb, err := OpenStateDB(dir + "/state.db")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer sdb.Close()
	state := &AppState{Strategies: map[string]*StrategyState{
		"hl-x": {
			ID: "hl-x", Type: "perps", Platform: "hyperliquid", Cash: 1000,
			RegimeProfile: &RegimeProfileState{ActiveProfile: "trend", PendingProfile: "fade", PendingBarsSeen: 2},
			Positions: map[string]*Position{
				"BTC": {Symbol: "BTC", Quantity: 1, AvgCost: 50000, Side: "long", OpenProfile: "trend"},
			},
			OptionPositions: map[string]*OptionPosition{},
		},
	}}
	if err := sdb.SaveState(state); err != nil {
		t.Fatalf("save: %v", err)
	}
	loaded, err := sdb.LoadState()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	s := loaded.Strategies["hl-x"]
	if s == nil || s.RegimeProfile == nil || s.RegimeProfile.ActiveProfile != "trend" {
		t.Fatalf("active_profile not restored: %+v", s)
	}
	// The pending counter is intentionally NOT persisted (re-arms on restart).
	if s.RegimeProfile.PendingBarsSeen != 0 {
		t.Fatalf("pending counter should not persist, got %d", s.RegimeProfile.PendingBarsSeen)
	}
	if pos := s.Positions["BTC"]; pos == nil || pos.OpenProfile != "trend" {
		t.Fatalf("open_profile not restored: %+v", pos)
	}
}

// fullProfileAllocConfig builds a complete HL-perps + regime config with a
// valid regime_profile_allocation for validateConfig integration tests.
func fullProfileAllocConfig(t *testing.T, palJSON string) Config {
	t.Helper()
	var a RegimeProfileAllocation
	if err := json.Unmarshal([]byte(palJSON), &a); err != nil {
		t.Fatalf("unmarshal pal: %v", err)
	}
	return Config{
		Strategies: []StrategyConfig{{
			ID:                      "hl-test-btc",
			Type:                    "perps",
			Platform:                "hyperliquid",
			Script:                  "shared_scripts/check_hyperliquid.py",
			Args:                    []string{"regime_adaptive_htf", "BTC", "1h", "--mode=paper"},
			Capital:                 1000,
			Leverage:                5,
			MaxDrawdownPct:          60,
			Direction:               "long",
			OpenStrategy:            StrategyRef{Name: "regime_adaptive_htf"},
			RegimeProfileAllocation: &a,
		}},
		Regime: &RegimeConfig{Enabled: true, Period: 14, ADXThreshold: 20, Windows: RegimeWindowsMap{
			"profile_long":     {Classifier: regimeClassifierComposite, Period: 200},
			"composite_medium": {Classifier: regimeClassifierComposite, Period: 50},
		}},
		PortfolioRisk: &PortfolioRiskConfig{MaxDrawdownPct: 25, WarnThresholdPct: 80},
	}
}

const validPALJSON = `{
	"window": "profile_long",
	"profiles": {"trending_up_clean":"trend","trending_up_choppy":"trend","trending_down_clean":"trend","trending_down_choppy":"trend","ranging_quiet":"fade","ranging_volatile":"fade","ranging_directional":"fade","ranging_directional_up":"fade","ranging_directional_down":"fade"},
	"param_sets": {"fade": {"trend_entry":"off"}, "trend": {"trend_entry":"breakout"}},
	"confirm_bars": 24,
	"initial_profile": "fade"
}`

func TestConfigValidation_ProfileAllocation_Valid(t *testing.T) {
	cfg := fullProfileAllocConfig(t, validPALJSON)
	if err := validateConfig(&cfg, false); err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}
}

func TestConfigValidation_ProfileAllocation_RejectsRegimeDisabled(t *testing.T) {
	cfg := fullProfileAllocConfig(t, validPALJSON)
	cfg.Regime.Enabled = false
	err := validateConfig(&cfg, false)
	if err == nil || !indexOfErr(err, "regime_profile_allocation requires top-level regime.enabled=true") {
		t.Fatalf("expected regime-enabled error, got: %v", err)
	}
}

func TestConfigValidation_ProfileAllocation_RejectsNonHL(t *testing.T) {
	cfg := fullProfileAllocConfig(t, validPALJSON)
	cfg.Strategies[0].Platform = "okx"
	cfg.Strategies[0].Args = []string{"regime_adaptive_htf", "BTC-USDT-SWAP", "1h"}
	cfg.Strategies[0].Script = "shared_scripts/check_okx.py"
	err := validateConfig(&cfg, false)
	if err == nil || !indexOfErr(err, "regime_profile_allocation is only supported for HL perps") {
		t.Fatalf("expected HL-perps-only error, got: %v", err)
	}
}

func TestConfigValidation_ProfileAllocation_RejectsThreeProfiles(t *testing.T) {
	cfg := fullProfileAllocConfig(t, `{
		"window": "profile_long",
		"profiles": {"trending_up_clean":"a","trending_up_choppy":"a","trending_down_clean":"a","trending_down_choppy":"a","ranging_quiet":"b","ranging_volatile":"b","ranging_directional":"c"},
		"param_sets": {"a": {}, "b": {}, "c": {}},
		"confirm_bars": 24,
		"initial_profile": "a"
	}`)
	err := validateConfig(&cfg, false)
	if err == nil || !indexOfErr(err, "exactly 2 profiles") {
		t.Fatalf("expected param_sets count error, got: %v", err)
	}
}

func indexOfErr(err error, sub string) bool {
	return err != nil && indexOf(err.Error(), sub) >= 0
}

// TestLoadConfig_ProfileAllocation_FromFile exercises the full JSON path:
// UnmarshalJSON capture, the StrategyConfig unknown-key guard, and validation.
func TestLoadConfig_ProfileAllocation_FromFile(t *testing.T) {
	dir := t.TempDir()
	cfg := `{
		"config_version": 15,
		"regime": {"enabled": true, "windows": {
			"profile_long": {"classifier": "composite", "period": 200},
			"composite_medium": {"classifier": "composite", "period": 50}
		}},
		"portfolio_risk": {"max_drawdown_pct": 25, "warn_threshold_pct": 80},
		"strategies": [{
			"id": "hl-radhtf-btc",
			"type": "perps",
			"platform": "hyperliquid",
			"script": "shared_scripts/check_hyperliquid.py",
			"args": ["regime_adaptive_htf", "BTC", "1h", "--mode=paper"],
			"open_strategy": {"name": "regime_adaptive_htf"},
			"capital": 1000,
			"leverage": 5,
			"max_drawdown_pct": 60,
			"direction": "long",
			"regime_profile_allocation": {
				"window": "profile_long",
				"profiles": {"trending_up_clean":"trend","trending_up_choppy":"trend","trending_down_clean":"trend","trending_down_choppy":"trend","ranging_quiet":"fade","ranging_volatile":"fade","ranging_directional":"fade","ranging_directional_up":"fade","ranging_directional_down":"fade"},
				"param_sets": {"fade": {"trend_entry":"off"}, "trend": {"trend_entry":"breakout","trend_drift_confirm":0.1}},
				"confirm_bars": 24,
				"initial_profile": "fade"
			}
		}]
	}`
	path := writeTestConfig(t, dir, cfg)
	loaded, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig rejected a valid regime_profile_allocation: %v", err)
	}
	sc := loaded.Strategies[0]
	if !sc.RegimeProfileAllocation.IsConfigured() {
		t.Fatal("regime_profile_allocation not parsed onto the strategy")
	}
	if sc.RegimeProfileAllocation.ConfirmBars != 24 || sc.RegimeProfileAllocation.InitialProfile != "fade" {
		t.Fatalf("resolved fields wrong: %+v", sc.RegimeProfileAllocation)
	}
}

func containsSubstr(errs []string, sub string) bool {
	for _, e := range errs {
		if len(sub) > 0 && indexOf(e, sub) >= 0 {
			return true
		}
	}
	return false
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

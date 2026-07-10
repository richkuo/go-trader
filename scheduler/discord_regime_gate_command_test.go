package main

import (
	"encoding/json"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// ─── #1205: /apply-regime-gate interactive command ──────────────────────────
//
// These tests pin the pure helpers behind the interactive Discord command:
// preset lookup, type eligibility, selection parsing, the config mutation, and
// — most importantly — that the written config still passes the real validator
// (LoadConfigForProbe) and reloads to the exact #1197 comp_up_clean_p21 shape.

func TestRegimeGatePresetLookup(t *testing.T) {
	p, ok := regimeGatePresetByName("comp_up_clean_p21")
	if !ok {
		t.Fatal("default preset must resolve")
	}
	if p.WindowKey != "comp_p21" || p.WindowSpec.effectiveClassifier() != regimeClassifierComposite || p.WindowSpec.Period != 21 {
		t.Errorf("preset window mismatch: %+v", p)
	}
	if len(p.AllowedRegimes) != 1 || p.AllowedRegimes[0] != "trending_up_clean" {
		t.Errorf("preset allowed_regimes mismatch: %v", p.AllowedRegimes)
	}
	// Case/space-insensitive.
	if _, ok := regimeGatePresetByName("  Comp_Up_Clean_P21 "); !ok {
		t.Error("lookup should be case/space-insensitive")
	}
	if _, ok := regimeGatePresetByName("does-not-exist"); ok {
		t.Error("unknown preset must not resolve")
	}
	if defaultRegimeGatePresetName != "comp_up_clean_p21" {
		t.Errorf("default preset name drifted: %q", defaultRegimeGatePresetName)
	}
}

func TestStrategyEligibleForRegimeGate(t *testing.T) {
	preset, _ := regimeGatePresetByName(defaultRegimeGatePresetName)
	cases := []struct {
		typ  string
		want bool
	}{
		{"futures", true},
		{"perps", true},
		{"spot", false},
		{"options", false},
		{"manual", false},
		{"", false},
	}
	for _, c := range cases {
		got := strategyEligibleForRegimeGate(StrategyConfig{Type: c.typ}, preset)
		if got != c.want {
			t.Errorf("type %q eligibility: got %v want %v", c.typ, got, c.want)
		}
	}
}

func TestParseRegimeGateSelection(t *testing.T) {
	cases := []struct {
		reply   string
		n       int
		wantIdx int
		wantOK  bool
	}{
		{"1", 3, 0, true},
		{" 3 ", 3, 2, true},
		{"3", 3, 2, true},
		{"0", 3, 0, false},
		{"4", 3, 0, false},
		{"cancel", 3, 0, false},
		{"", 3, 0, false},
		{"1.5", 3, 0, false},
		{"two", 3, 0, false},
	}
	for _, c := range cases {
		idx, ok := parseRegimeGateSelection(c.reply, c.n)
		if ok != c.wantOK || (ok && idx != c.wantIdx) {
			t.Errorf("parse(%q, %d) = (%d, %v); want (%d, %v)", c.reply, c.n, idx, ok, c.wantIdx, c.wantOK)
		}
	}
}

// applyRegimeGateToRoot must add the composite window and set the strategy's
// gate fields, and the result must pass the real validator and reload to the
// canonical #1197 shape.
func TestApplyRegimeGateToRoot_RoundTripValidates(t *testing.T) {
	preset, _ := regimeGatePresetByName(defaultRegimeGatePresetName)
	// Reuse minimalConfigJSON but make the perps strategy a gateable type
	// (it already is: "perps"). Point the gate at hl-momentum-eth.
	root := rootFromJSON(t, minimalConfigJSON)
	if err := applyRegimeGateToRoot(root, "hl-momentum-eth", preset); err != nil {
		t.Fatalf("applyRegimeGateToRoot: %v", err)
	}

	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	if err := writeValidatedConfigRoot(path, root); err != nil {
		t.Fatalf("gated config failed validation: %v", err)
	}
	cfg, err := LoadConfigForProbe(path)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if cfg.Regime == nil || !cfg.Regime.Enabled {
		t.Fatalf("regime must be enabled after wiring, got %+v", cfg.Regime)
	}
	spec, ok := cfg.Regime.Windows["comp_p21"]
	if !ok {
		t.Fatalf("comp_p21 window missing after wiring: %+v", cfg.Regime.Windows)
	}
	if spec.effectiveClassifier() != regimeClassifierComposite || spec.Period != 21 {
		t.Errorf("comp_p21 window wrong spec: %+v", spec)
	}
	found := false
	for _, sc := range cfg.Strategies {
		if sc.ID == "hl-momentum-eth" {
			found = true
			if sc.RegimeGateWindow != "comp_p21" {
				t.Errorf("regime_gate_window not set: %q", sc.RegimeGateWindow)
			}
			if len(sc.AllowedRegimes) != 1 || sc.AllowedRegimes[0] != "trending_up_clean" {
				t.Errorf("allowed_regimes not set: %v", sc.AllowedRegimes)
			}
		}
	}
	if !found {
		t.Fatal("target strategy missing after reload")
	}
	// The untouched spot strategy must keep no gate.
	for _, sc := range cfg.Strategies {
		if sc.ID == "sma-btc" && (sc.RegimeGateWindow != "" || len(sc.AllowedRegimes) != 0) {
			t.Errorf("untouched strategy gained a gate: %+v", sc)
		}
	}
}

// An ineligible strategy type must be refused by the mutation itself
// (defense-in-depth beyond the picker filter).
func TestApplyRegimeGateToRoot_RejectsIneligibleType(t *testing.T) {
	preset, _ := regimeGatePresetByName(defaultRegimeGatePresetName)
	root := rootFromJSON(t, minimalConfigJSON)
	err := applyRegimeGateToRoot(root, "sma-btc", preset) // spot
	if err == nil {
		t.Fatal("gating a spot strategy must be refused")
	}
	// The config must be untouched: no regime block added.
	if _, ok := root["regime"]; ok {
		t.Error("regime block must not be added when the mutation is refused")
	}
}

func TestApplyRegimeGateToRoot_UnknownStrategy(t *testing.T) {
	preset, _ := regimeGatePresetByName(defaultRegimeGatePresetName)
	root := rootFromJSON(t, minimalConfigJSON)
	if err := applyRegimeGateToRoot(root, "nope", preset); err == nil {
		t.Fatal("unknown strategy must be refused")
	}
}

// Re-applying the same gate is idempotent: an already-present matching window is
// left alone and the config still validates.
func TestApplyRegimeGateToRoot_IdempotentReapply(t *testing.T) {
	preset, _ := regimeGatePresetByName(defaultRegimeGatePresetName)
	root := rootFromJSON(t, minimalConfigJSON)
	if err := applyRegimeGateToRoot(root, "hl-momentum-eth", preset); err != nil {
		t.Fatalf("first apply: %v", err)
	}
	first, _ := json.Marshal(root["regime"])
	if err := applyRegimeGateToRoot(root, "hl-momentum-eth", preset); err != nil {
		t.Fatalf("second apply: %v", err)
	}
	second, _ := json.Marshal(root["regime"])
	if string(first) != string(second) {
		t.Errorf("re-apply changed the regime block:\n%s\n->\n%s", first, second)
	}
}

// A pre-existing window under the preset's key with a conflicting spec must be
// refused rather than clobbered — other strategies may reference it.
func TestEnsureRegimeGateWindow_RefusesConflictingExistingWindow(t *testing.T) {
	preset, _ := regimeGatePresetByName(defaultRegimeGatePresetName)
	root := rootFromJSON(t, minimalConfigJSON)
	// Seed regime.windows.comp_p21 as an ADX window (bare-int shorthand).
	root["regime"] = json.RawMessage(`{"enabled":true,"windows":{"comp_p21":14}}`)
	err := ensureRegimeGateWindow(root, preset)
	if err == nil {
		t.Fatal("a conflicting existing comp_p21 window must be refused")
	}
}

// A pre-existing window that already matches the preset spec is accepted.
func TestEnsureRegimeGateWindow_AcceptsMatchingExistingWindow(t *testing.T) {
	preset, _ := regimeGatePresetByName(defaultRegimeGatePresetName)
	root := rootFromJSON(t, minimalConfigJSON)
	root["regime"] = json.RawMessage(`{"enabled":false,"windows":{"comp_p21":{"classifier":"composite","period":21}}}`)
	if err := ensureRegimeGateWindow(root, preset); err != nil {
		t.Fatalf("matching existing window should be accepted: %v", err)
	}
	// enabled must be flipped on.
	var regime map[string]json.RawMessage
	_ = json.Unmarshal(root["regime"], &regime)
	var enabled bool
	_ = json.Unmarshal(regime["enabled"], &enabled)
	if !enabled {
		t.Error("regime.enabled must be true after ensure")
	}
}

// Applying alongside an existing ADX "medium" window (the realistic deployment
// shape from regime_comp_up_clean_gate_test.go) must keep the ADX window and
// validate.
func TestApplyRegimeGateToRoot_AlongsideExistingADXWindow(t *testing.T) {
	preset, _ := regimeGatePresetByName(defaultRegimeGatePresetName)
	root := rootFromJSON(t, minimalConfigJSON)
	root["regime"] = json.RawMessage(`{"enabled":true,"period":14,"adx_threshold":20,"windows":{"medium":{"classifier":"adx","period":14,"adx_threshold":20}}}`)
	if err := applyRegimeGateToRoot(root, "hl-momentum-eth", preset); err != nil {
		t.Fatalf("apply alongside ADX window: %v", err)
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	if err := writeValidatedConfigRoot(path, root); err != nil {
		t.Fatalf("failed validation: %v", err)
	}
	cfg, err := LoadConfigForProbe(path)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if _, ok := cfg.Regime.Windows["medium"]; !ok {
		t.Error("existing ADX medium window must be preserved")
	}
	if _, ok := cfg.Regime.Windows["comp_p21"]; !ok {
		t.Error("comp_p21 window must be added")
	}
}

// ─── #1205 review: enabling regime detection must surface the blast radius ───
//
// Wiring a gate onto one strategy flips regime.enabled on for the whole config,
// which silently activates every OTHER strategy that already carries a dormant
// allowed_regimes gate. These tests pin that the confirm prompt lists that blast
// radius (and only when the flip actually happens).

// When the apply flips regime.enabled false→true, every OTHER strategy carrying
// allowed_regimes is newly activated and must be listed — whether via a
// regime_gate_window or the legacy single-lookback gate (allowed_regimes with no
// window). The target itself, empty allowed_regimes, and ungated strategies are
// excluded.
func TestRegimeGateSideEffectStrategies_FlipActivatesOthers(t *testing.T) {
	root := rootFromJSON(t, `{
	  "regime": {"enabled": false, "windows": {"comp_p21": {"classifier": "composite", "period": 21}}},
	  "strategies": [
	    {"id": "target", "type": "perps", "allowed_regimes": ["trending_up_clean"], "regime_gate_window": "comp_p21"},
	    {"id": "gated-window", "type": "perps", "allowed_regimes": ["trending_up_clean"], "regime_gate_window": "comp_p21"},
	    {"id": "gated-legacy", "type": "futures", "allowed_regimes": ["trending"]},
	    {"id": "ungated-empty", "type": "perps", "allowed_regimes": []},
	    {"id": "ungated-none", "type": "spot"}
	  ]
	}`)
	got, err := regimeGateSideEffectStrategies(root, "target")
	if err != nil {
		t.Fatalf("regimeGateSideEffectStrategies: %v", err)
	}
	want := []string{"gated-legacy", "gated-window"} // sorted; excludes target/empty/none
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

// Must-survive (1): when regime detection is already enabled there is no flip, so
// no strategy is newly activated and the prompt must not falsely warn.
func TestRegimeGateSideEffectStrategies_AlreadyEnabledNoWarn(t *testing.T) {
	root := rootFromJSON(t, `{
	  "regime": {"enabled": true, "windows": {"comp_p21": {"classifier": "composite", "period": 21}}},
	  "strategies": [
	    {"id": "target", "type": "perps"},
	    {"id": "other", "type": "perps", "allowed_regimes": ["trending_up_clean"]}
	  ]
	}`)
	got, err := regimeGateSideEffectStrategies(root, "target")
	if err != nil {
		t.Fatalf("regimeGateSideEffectStrategies: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("regime already enabled → no side effect expected, got %v", got)
	}
}

// An absent regime block reads as disabled, so the flip still happens and other
// gated strategies are activated.
func TestRegimeGateSideEffectStrategies_AbsentRegimeBlockActivates(t *testing.T) {
	root := rootFromJSON(t, `{
	  "strategies": [
	    {"id": "target", "type": "perps"},
	    {"id": "other", "type": "perps", "allowed_regimes": ["trending_up_clean"]}
	  ]
	}`)
	got, err := regimeGateSideEffectStrategies(root, "target")
	if err != nil {
		t.Fatalf("regimeGateSideEffectStrategies: %v", err)
	}
	if len(got) != 1 || got[0] != "other" {
		t.Errorf("absent regime block reads as disabled → other must be listed, got %v", got)
	}
}

func TestBuildRegimeGateConfirmMessage(t *testing.T) {
	preset, _ := regimeGatePresetByName(defaultRegimeGatePresetName)

	// No side effects → the warning block must be absent.
	msg := buildRegimeGateConfirmMessage(preset, "hl-momentum-eth", nil)
	if strings.Contains(msg, "also activates") {
		t.Errorf("no side effects but warning present:\n%s", msg)
	}
	for _, want := range []string{"hl-momentum-eth", "comp_up_clean_p21", "comp_p21"} {
		if !strings.Contains(msg, want) {
			t.Errorf("confirm message missing core field %q:\n%s", want, msg)
		}
	}

	// Side effects → the warning lists the other strategies and their count.
	msg = buildRegimeGateConfirmMessage(preset, "hl-momentum-eth", []string{"gated-legacy", "gated-window"})
	for _, want := range []string{"also activates", "2 other", "`gated-legacy`", "`gated-window`"} {
		if !strings.Contains(msg, want) {
			t.Errorf("confirm message missing %q:\n%s", want, msg)
		}
	}
}

// Must-survive (3): a strategy newly gated by the regime flip must still manage
// an open position it already holds — regimeBlocksOpen short-circuits on
// posQty>0 regardless of the (dis)allowed regime, so only fresh entries are
// gated. This backs the "Open positions are unaffected" claim in the prompt.
func TestRegimeGateDoesNotBlockOpenPositionManagement(t *testing.T) {
	allowed := []string{"trending_up_clean"}
	// Sanity: a disallowed current regime blocks a fresh entry (flat).
	if !regimeBlocksOpen(allowed, "ranging", 0, false) {
		t.Fatal("a disallowed regime must block a fresh entry (posQty=0)")
	}
	// With an open position it must NOT block — management passes through.
	if regimeBlocksOpen(allowed, "ranging", 1.5, false) {
		t.Error("a newly-activated gate must not block management of an open position")
	}
}

// ─── #1206 review: the confirm-time blast radius must be re-verified, not
// trusted stale, immediately before the write ────────────────────────────────
//
// The confirm message's blast-radius list is computed once, before the (up to
// 60s) confirm wait. regimeGateBlastRadiusGrew is what the handler calls right
// before the write to decide whether a concurrent edit during that wait
// invalidated what the operator agreed to.

// Must-survive (1): a concurrent edit adds allowed_regimes to a strategy the
// operator was never shown — the fresh set gains an id, which must be
// reported as growth so the handler refuses the write.
func TestRegimeGateBlastRadiusGrew_ConcurrentAddIsGrowth(t *testing.T) {
	shown := []string{"gated-window"}
	fresh := []string{"gated-window", "surprise-strategy"}
	got := regimeGateBlastRadiusGrew(fresh, shown)
	if !reflect.DeepEqual(got, []string{"surprise-strategy"}) {
		t.Errorf("got %v, want [surprise-strategy]", got)
	}
}

// Must-survive (2): a concurrent edit removes allowed_regimes from a
// previously-listed strategy — fresh shrinks relative to shown, which must
// NOT be treated as growth (the write must still proceed).
func TestRegimeGateBlastRadiusGrew_ConcurrentRemovalIsNotGrowth(t *testing.T) {
	shown := []string{"gated-legacy", "gated-window"}
	fresh := []string{"gated-window"}
	if got := regimeGateBlastRadiusGrew(fresh, shown); len(got) != 0 {
		t.Errorf("a shrunk set must not be reported as growth, got %v", got)
	}
}

// Must-survive (3): regime detection itself gets enabled by a concurrent edit
// during the confirm wait, turning what was a real flip (shown non-empty)
// into a no-op (fresh empty, since regimeGateSideEffectStrategies returns nil
// once already enabled). That is a shrink to empty, not growth — the write
// must still proceed with the now-safe (no-op) flip.
func TestRegimeGateBlastRadiusGrew_ConcurrentEnableCollapsesToNoGrowth(t *testing.T) {
	shown := []string{"gated-legacy", "gated-window"}
	var fresh []string // regime.enabled flipped true meanwhile → no side effect
	if got := regimeGateBlastRadiusGrew(fresh, shown); len(got) != 0 {
		t.Errorf("a concurrent enable collapsing the flip to a no-op must not be growth, got %v", got)
	}
}

// Baseline: an unchanged blast radius is never reported as growth.
func TestRegimeGateBlastRadiusGrew_UnchangedIsNotGrowth(t *testing.T) {
	shown := []string{"gated-legacy", "gated-window"}
	fresh := []string{"gated-legacy", "gated-window"}
	if got := regimeGateBlastRadiusGrew(fresh, shown); len(got) != 0 {
		t.Errorf("an unchanged set must not be reported as growth, got %v", got)
	}
}

func TestBuildRegimeGatePickerMessage(t *testing.T) {
	preset, _ := regimeGatePresetByName(defaultRegimeGatePresetName)
	candidates := []gateCandidate{
		{sc: StrategyConfig{ID: "hl-breakout-btc", Type: "perps", Platform: "hyperliquid"}, live: true, hasOpen: false},
		{sc: StrategyConfig{ID: "ts-breakout-btc", Type: "futures", Platform: "topstep"}, live: true, hasOpen: true},
	}
	msg := buildRegimeGatePickerMessage(candidates, preset)
	for _, want := range []string{
		"comp_up_clean_p21",
		"1. `hl-breakout-btc`",
		"perps/hyperliquid (live)",
		"flat",
		"2. `ts-breakout-btc`",
		"open position",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("picker message missing %q:\n%s", want, msg)
		}
	}
}

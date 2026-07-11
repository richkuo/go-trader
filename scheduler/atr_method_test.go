package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// --- resolveATRMethod precedence (#1277) -----------------------------------

func TestResolveATRMethodPrecedence(t *testing.T) {
	cases := []struct {
		name     string
		strategy string
		global   string
		want     string
	}{
		{"both empty -> simple", "", "", ATRMethodSimple},
		{"global only", "", "wilder", ATRMethodWilder},
		{"per-strategy only", "wilder", "", ATRMethodWilder},
		{"per-strategy wins over global", "simple", "wilder", ATRMethodSimple},
		{"normalization: case + whitespace", " Wilder ", "", ATRMethodWilder},
		{"whitespace-only strategy value inherits global", "  ", "wilder", ATRMethodWilder},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sc := StrategyConfig{ID: "s", ATRMethod: tc.strategy}
			cfg := &Config{ATRMethod: tc.global}
			if got := resolveATRMethod(sc, cfg); got != tc.want {
				t.Fatalf("resolveATRMethod(%q, %q) = %q, want %q", tc.strategy, tc.global, got, tc.want)
			}
		})
	}
	// nil cfg must not panic and falls back to the per-strategy value / default.
	if got := resolveATRMethod(StrategyConfig{ATRMethod: "wilder"}, nil); got != ATRMethodWilder {
		t.Fatalf("nil cfg with per-strategy wilder: got %q", got)
	}
	if got := resolveATRMethod(StrategyConfig{}, nil); got != ATRMethodSimple {
		t.Fatalf("nil cfg default: got %q", got)
	}
}

func TestValidATRMethodValue(t *testing.T) {
	for _, ok := range []string{"", "simple", "wilder", " WILDER ", "Simple"} {
		if !validATRMethodValue(ok) {
			t.Errorf("validATRMethodValue(%q) = false, want true", ok)
		}
	}
	for _, bad := range []string{"rma", "ema", "wilders", "simple mean", "0"} {
		if validATRMethodValue(bad) {
			t.Errorf("validATRMethodValue(%q) = true, want false", bad)
		}
	}
}

// --- config validation ------------------------------------------------------

func TestValidateConfigRejectsUnknownATRMethod(t *testing.T) {
	cfg := Config{
		ATRMethod: "rma",
		Strategies: []StrategyConfig{{
			ID: "hl-eth", Type: "perps", Platform: "hyperliquid",
			Script: "check.py", Args: []string{"a", "ETH", "1h"},
			Capital: 1000, MaxDrawdownPct: 10, Leverage: 2,
			ATRMethod: "bogus",
		}},
	}
	err := validateConfig(&cfg, false)
	if err == nil {
		t.Fatal("expected unknown atr_method values to be rejected")
	}
	msg := err.Error()
	if !strings.Contains(msg, `atr_method must be "simple" or "wilder", got "rma"`) {
		t.Errorf("global rejection missing: %v", msg)
	}
	if !strings.Contains(msg, `strategy[hl-eth]: atr_method must be "simple" or "wilder", got "bogus"`) {
		t.Errorf("per-strategy rejection missing: %v", msg)
	}
}

func TestValidateConfigRejectsATRMethodOnOptions(t *testing.T) {
	cfg := Config{
		Strategies: []StrategyConfig{{
			ID: "opt-btc", Type: "options", Platform: "deribit",
			Script: "check_options.py", Args: []string{"strangle", "BTC"},
			Capital: 1000, MaxDrawdownPct: 10,
			ATRMethod: "wilder",
		}},
	}
	err := validateConfig(&cfg, false)
	if err == nil || !strings.Contains(err.Error(), "atr_method is not supported on options strategies") {
		t.Fatalf("expected options atr_method rejection, got: %v", err)
	}
}

func TestValidateConfigAcceptsWilderATRMethod(t *testing.T) {
	cfg := Config{
		ATRMethod: "wilder",
		Strategies: []StrategyConfig{{
			ID: "hl-eth", Type: "perps", Platform: "hyperliquid",
			Script: "check.py", Args: []string{"a", "ETH", "1h"},
			Capital: 1000, MaxDrawdownPct: 10, Leverage: 2,
			ATRMethod: "simple",
		}},
	}
	if err := validateConfig(&cfg, false); err != nil {
		t.Fatalf("valid atr_method values rejected: %v", err)
	}
}

// --- hot reload -------------------------------------------------------------

// The effective ATR smoothing method feeds EntryATR stamping and the live
// close-evaluator ATR; flipping it while a position is open would re-base
// in-flight stop/TP geometry. Blocked while open, allowed when flat — and the
// guard must fire on the RESOLVED value, so a global flip is caught for
// inheriting strategies too.
func TestValidateHotReloadStateCompatibleATRMethod(t *testing.T) {
	mkCfg := func(global, strategy string) *Config {
		cfg := minimalReloadConfig([]StrategyConfig{{
			ID: "hl-eth", Type: "perps", Platform: "hyperliquid", Script: "x.py",
			Args: []string{"a", "ETH", "1h"}, Capital: 1000, MaxDrawdownPct: 10,
			Leverage: 5, MarginMode: "isolated", ATRMethod: strategy,
		}})
		cfg.ATRMethod = global
		return cfg
	}
	openState := &AppState{Strategies: map[string]*StrategyState{
		"hl-eth": {ID: "hl-eth", Positions: map[string]*Position{
			"ETH": {Symbol: "ETH", Quantity: 0.5, AvgCost: 3000, Side: "long"},
		}},
	}}
	flatState := &AppState{Strategies: map[string]*StrategyState{
		"hl-eth": {ID: "hl-eth", Positions: map[string]*Position{}},
	}}

	cases := []struct {
		name                string
		oldGlobal, oldStrat string
		newGlobal, newStrat string
		wantBlockedOpen     bool
	}{
		{"per-strategy simple->wilder", "", "", "", "wilder", true},
		{"per-strategy wilder->removed (inherits simple)", "", "wilder", "", "", true},
		{"global simple->wilder caught for inheriting strategy", "", "", "wilder", "", true},
		{"global flip masked by explicit per-strategy value", "", "simple", "wilder", "simple", false},
		{"no-op: explicit simple added, resolved unchanged", "", "", "", "simple", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			old := mkCfg(tc.oldGlobal, tc.oldStrat)
			next := mkCfg(tc.newGlobal, tc.newStrat)
			err := validateHotReloadStateCompatible(old, next, openState)
			if tc.wantBlockedOpen {
				if err == nil || !strings.Contains(err.Error(), "effective atr_method changed with open positions") {
					t.Fatalf("open position: want atr_method block, got: %v", err)
				}
			} else if err != nil {
				t.Fatalf("open position: resolved method unchanged, want accept, got: %v", err)
			}
			// Every shape must be accepted while flat.
			if err := validateHotReloadStateCompatible(old, next, flatState); err != nil {
				t.Fatalf("flat: want accept, got: %v", err)
			}
		})
	}
}

// A global atr_method flip must never trip the open-position guard on an
// options strategy — options have no ATR surface (the per-strategy field is
// rejected at load), so an open options position must not block the fleet.
func TestValidateHotReloadStateCompatibleATRMethodSkipsOptions(t *testing.T) {
	mk := func(global string) *Config {
		cfg := minimalReloadConfig([]StrategyConfig{{
			ID: "opt-btc", Type: "options", Platform: "deribit", Script: "check_options.py",
			Args: []string{"strangle", "BTC"}, Capital: 1000, MaxDrawdownPct: 10,
		}})
		cfg.ATRMethod = global
		return cfg
	}
	openState := &AppState{Strategies: map[string]*StrategyState{
		"opt-btc": {ID: "opt-btc", Positions: map[string]*Position{
			"BTC-CALL": {Symbol: "BTC-CALL", Quantity: 1, AvgCost: 100, Side: "long"},
		}},
	}}
	if err := validateHotReloadStateCompatible(mk(""), mk("wilder"), openState); err != nil {
		t.Fatalf("global atr_method flip must not block on an open options position: %v", err)
	}
}

func TestStrategyRestartShapeMasksATRMethod(t *testing.T) {
	a := StrategyConfig{ID: "hl-a", ATRMethod: "wilder"}
	b := StrategyConfig{ID: "hl-a", ATRMethod: ""}
	if !reflect.DeepEqual(strategyRestartShape(a), strategyRestartShape(b)) {
		t.Fatal("atr_method change must not flag restart-required (hot-reloadable when flat)")
	}
}

func TestApplyHotReloadConfigAppliesATRMethodWhenFlat(t *testing.T) {
	old := minimalReloadConfig([]StrategyConfig{{
		ID: "hl-eth", Type: "perps", Platform: "hyperliquid", Script: "x.py",
		Args: []string{"a", "ETH", "1h"}, Capital: 1000, MaxDrawdownPct: 10,
		Leverage: 5, MarginMode: "isolated",
	}})
	next := minimalReloadConfig([]StrategyConfig{{
		ID: "hl-eth", Type: "perps", Platform: "hyperliquid", Script: "x.py",
		Args: []string{"a", "ETH", "1h"}, Capital: 1000, MaxDrawdownPct: 10,
		Leverage: 5, MarginMode: "isolated", ATRMethod: "wilder",
	}})
	next.ATRMethod = "wilder"
	flatState := &AppState{Strategies: map[string]*StrategyState{
		"hl-eth": {ID: "hl-eth", Positions: map[string]*Position{}},
	}}
	changes, err := applyHotReloadConfig(old, next, flatState, nil, nil)
	if err != nil {
		t.Fatalf("flat atr_method reload rejected: %v", err)
	}
	if old.ATRMethod != "wilder" {
		t.Errorf("global atr_method not applied: %q", old.ATRMethod)
	}
	if old.Strategies[0].ATRMethod != "wilder" {
		t.Errorf("per-strategy atr_method not applied: %q", old.Strategies[0].ATRMethod)
	}
	joined := strings.Join(changes, "\n")
	if !strings.Contains(joined, `atr_method: "" -> "wilder"`) {
		t.Errorf("global change line missing: %v", changes)
	}
	if !strings.Contains(joined, `strategy[hl-eth].atr_method: "" -> "wilder"`) {
		t.Errorf("per-strategy change line missing: %v", changes)
	}
}

// --- argv / probe contract ---------------------------------------------------

func TestAppendATRMethodArg(t *testing.T) {
	got := appendATRMethodArg([]string{"a", "ETH", "1h"}, ATRMethodWilder)
	want := []string{"a", "ETH", "1h", "--atr-method=wilder"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("appendATRMethodArg = %v, want %v", got, want)
	}
}

// The runtime signal-check argv unconditionally carries --atr-method, so the
// startup probe argvs that mirror it must carry the flag too — otherwise an
// asymmetric deploy (new Go, stale Python) passes the probe and dies on the
// first real cycle. fetch-atr gained the flag as well (#1277 manual-open
// parity). The execute argv never carries it, so executeProbeArgv must NOT.
func TestProbeArgvsCarryATRMethod(t *testing.T) {
	has := func(argv []string) bool {
		for _, a := range argv {
			if strings.HasPrefix(a, "--atr-method") {
				return true
			}
		}
		return false
	}
	if !has(probeArgv) {
		t.Error("probeArgv missing --atr-method")
	}
	if !has(probeCompositeArgv) {
		t.Error("probeCompositeArgv missing --atr-method")
	}
	if !has(fetchATRProbeArgv) {
		t.Error("fetchATRProbeArgv missing --atr-method")
	}
	if has(executeProbeArgv) {
		t.Error("executeProbeArgv must stay a faithful mirror of the execute argv (no --atr-method)")
	}
}

// --- migration ---------------------------------------------------------------

// v17 is additive (atr_method with an absent-field default); migration only
// re-stamps the version. A v16 config must come out stamped 17 with its
// contents otherwise intact.
func TestMigrateConfigStampsV17(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	raw := `{"config_version": 16, "interval_seconds": 600, "log_dir": "logs", "strategies": []}`
	if err := os.WriteFile(path, []byte(raw), 0600); err != nil {
		t.Fatal(err)
	}
	if err := MigrateConfig(path, nil, nil); err != nil {
		t.Fatalf("v16 -> v17 migration failed: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]interface{}
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatalf("migrated config unparseable: %v", err)
	}
	if v, _ := out["config_version"].(float64); int(v) != CurrentConfigVersion {
		t.Fatalf("config_version = %v, want %d", out["config_version"], CurrentConfigVersion)
	}
	if int64(out["interval_seconds"].(float64)) != 600 {
		t.Errorf("interval_seconds mangled: %v", out["interval_seconds"])
	}
}

// --- init --json -------------------------------------------------------------

func TestGenerateConfigEmitsATRMethod(t *testing.T) {
	cfg := generateConfig(InitOptions{ATRMethod: "wilder"})
	if cfg.ATRMethod != "wilder" {
		t.Fatalf("generateConfig atr_method = %q, want wilder", cfg.ATRMethod)
	}
	blob, err := json.Marshal(generateConfig(InitOptions{}))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(blob), "atr_method") {
		t.Error("default init must omit atr_method (simple is implicit)")
	}
}

// --- stamp-at-open / restart-drift hardening (#1277 optional) --------------

// stampATRMethodAtOpenIfOpened freezes the resolved method on a FRESH open
// only — mirrors RiskAnchorPrice/EntryATR/DirectionCertifiedAtOpen
// freeze-at-entry semantics so a later config change never silently re-bases
// what an already-open position was sized under.
func TestStampATRMethodAtOpenIfOpenedFreshOpen(t *testing.T) {
	cases := []struct {
		name     string
		strategy string
		global   string
		want     string
	}{
		{"default simple", "", "", ATRMethodSimple},
		{"global wilder", "", "wilder", ATRMethodWilder},
		{"per-strategy wins", "simple", "wilder", ATRMethodSimple},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := &StrategyState{ID: "s", Positions: map[string]*Position{
				"BTC": {Symbol: "BTC", Quantity: 1},
			}}
			sc := StrategyConfig{ID: "s", ATRMethod: tc.strategy}
			cfg := &Config{ATRMethod: tc.global}
			stampATRMethodAtOpenIfOpened(s, "BTC", true, sc, cfg)
			if got := s.Positions["BTC"].ATRMethodAtOpen; got != tc.want {
				t.Fatalf("ATRMethodAtOpen = %q, want %q", got, tc.want)
			}
		})
	}
}

// A scale-in add (opened=false) must never re-stamp — the frozen value must
// keep reflecting what the ORIGINAL entry was sized under, exactly like
// RiskAnchorPrice is not updated on adds.
func TestStampATRMethodAtOpenIfOpenedSkipsAdds(t *testing.T) {
	s := &StrategyState{ID: "s", Positions: map[string]*Position{
		"BTC": {Symbol: "BTC", Quantity: 2, ATRMethodAtOpen: ATRMethodSimple},
	}}
	sc := StrategyConfig{ID: "s", ATRMethod: "wilder"}
	cfg := &Config{}
	stampATRMethodAtOpenIfOpened(s, "BTC", false, sc, cfg)
	if got := s.Positions["BTC"].ATRMethodAtOpen; got != ATRMethodSimple {
		t.Fatalf("scale-in add must not re-stamp: got %q, want simple", got)
	}
}

// Defensive: nil state, missing symbol, and a nil position must never panic.
func TestStampATRMethodAtOpenIfOpenedNoOp(t *testing.T) {
	stampATRMethodAtOpenIfOpened(nil, "BTC", true, StrategyConfig{}, &Config{})
	s := &StrategyState{ID: "s", Positions: map[string]*Position{}}
	stampATRMethodAtOpenIfOpened(s, "BTC", true, StrategyConfig{}, &Config{})
	if _, ok := s.Positions["BTC"]; ok {
		t.Fatal("must not fabricate a position that ExecuteXxxSignal didn't open")
	}
}

// checkATRMethodDriftAtStartup is the only place that catches a config edit +
// process restart (not SIGHUP) that changed a strategy's effective atr_method
// while a position stayed open — validateHotReloadStateCompatible only runs
// on the SIGHUP path and has no "old" resolved value to diff against a fresh
// process's config load.
func TestCheckATRMethodDriftAtStartup(t *testing.T) {
	mkState := func(atrMethodAtOpen string, qty float64) *AppState {
		return &AppState{Strategies: map[string]*StrategyState{
			"hl-eth": {ID: "hl-eth", Positions: map[string]*Position{
				"ETH": {Symbol: "ETH", Quantity: qty, ATRMethodAtOpen: atrMethodAtOpen},
			}},
		}}
	}
	mkCfg := func(atrMethod string) *Config {
		return minimalReloadConfig([]StrategyConfig{{
			ID: "hl-eth", Type: "perps", Platform: "hyperliquid", Script: "x.py",
			Args: []string{"a", "ETH", "1h"}, Capital: 1000, MaxDrawdownPct: 10,
			ATRMethod: atrMethod,
		}})
	}

	t.Run("drift detected: opened simple, now resolves wilder", func(t *testing.T) {
		warnings := checkATRMethodDriftAtStartup(mkState(ATRMethodSimple, 1), mkCfg("wilder"))
		if len(warnings) != 1 {
			t.Fatalf("warnings = %v, want exactly 1", warnings)
		}
		for _, want := range []string{"hl-eth", "ETH", `"simple"`, `"wilder"`} {
			if !strings.Contains(warnings[0], want) {
				t.Errorf("warning missing %q: %s", want, warnings[0])
			}
		}
	})

	t.Run("global flip caught for an inheriting strategy", func(t *testing.T) {
		state := mkState(ATRMethodSimple, 1)
		cfg := mkCfg("") // per-strategy empty, inherits global
		cfg.ATRMethod = "wilder"
		warnings := checkATRMethodDriftAtStartup(state, cfg)
		if len(warnings) != 1 {
			t.Fatalf("warnings = %v, want exactly 1 (global flip must be caught via resolveATRMethod)", warnings)
		}
	})

	t.Run("no drift when resolved method unchanged", func(t *testing.T) {
		if warnings := checkATRMethodDriftAtStartup(mkState(ATRMethodWilder, 1), mkCfg("wilder")); len(warnings) != 0 {
			t.Fatalf("unexpected warnings for unchanged method: %v", warnings)
		}
	})

	t.Run("never-stamped pre-#1277 position is skipped", func(t *testing.T) {
		if warnings := checkATRMethodDriftAtStartup(mkState("", 1), mkCfg("wilder")); len(warnings) != 0 {
			t.Fatalf("pre-#1277 position (never stamped) must not warn: %v", warnings)
		}
	})

	t.Run("flat position is skipped", func(t *testing.T) {
		if warnings := checkATRMethodDriftAtStartup(mkState(ATRMethodSimple, 0), mkCfg("wilder")); len(warnings) != 0 {
			t.Fatalf("flat (qty<=0) position must not warn: %v", warnings)
		}
	})

	t.Run("options strategy is skipped even with a global flip", func(t *testing.T) {
		state := &AppState{Strategies: map[string]*StrategyState{
			"opt-btc": {ID: "opt-btc", Positions: map[string]*Position{
				"BTC-CALL": {Symbol: "BTC-CALL", Quantity: 1, ATRMethodAtOpen: ATRMethodSimple},
			}},
		}}
		cfg := minimalReloadConfig([]StrategyConfig{{
			ID: "opt-btc", Type: "options", Platform: "deribit", Script: "check_options.py",
			Args: []string{"strangle", "BTC"}, Capital: 1000, MaxDrawdownPct: 10,
		}})
		cfg.ATRMethod = "wilder"
		if warnings := checkATRMethodDriftAtStartup(state, cfg); len(warnings) != 0 {
			t.Fatalf("options strategy must never warn (no ATR surface): %v", warnings)
		}
	})

	t.Run("nil state and nil cfg do not panic", func(t *testing.T) {
		if warnings := checkATRMethodDriftAtStartup(nil, mkCfg("wilder")); warnings != nil {
			t.Fatalf("nil state: want nil, got %v", warnings)
		}
		if warnings := checkATRMethodDriftAtStartup(mkState(ATRMethodSimple, 1), nil); warnings != nil {
			t.Fatalf("nil cfg: want nil, got %v", warnings)
		}
	})
}

// --- summary line -------------------------------------------------------------

func TestSummaryLineSurfacesNonDefaultATRMethod(t *testing.T) {
	sc := StrategyConfig{ID: "hl-eth", Type: "perps", Platform: "hyperliquid", ATRMethod: "wilder"}
	if line := formatStrategySummaryLine(sc, nil, nil); !strings.Contains(line, "atr=wilder") {
		t.Errorf("per-strategy wilder missing atr= tag: %s", line)
	}
	plain := StrategyConfig{ID: "hl-eth", Type: "perps", Platform: "hyperliquid"}
	if line := formatStrategySummaryLine(plain, nil, nil); strings.Contains(line, "atr=") {
		t.Errorf("default simple must not be tagged: %s", line)
	}
	globalWilder := &Config{ATRMethod: "wilder"}
	if line := formatStrategySummaryLine(plain, nil, globalWilder); !strings.Contains(line, "atr=wilder") {
		t.Errorf("inherited global wilder missing atr= tag: %s", line)
	}
	opt := StrategyConfig{ID: "opt", Type: "options", Platform: "deribit"}
	if line := formatStrategySummaryLine(opt, nil, globalWilder); strings.Contains(line, "atr=") {
		t.Errorf("options must never carry the atr= tag: %s", line)
	}
}

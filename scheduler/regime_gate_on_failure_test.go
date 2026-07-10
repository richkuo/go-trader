package main

// #1278: configurable regime entry-gate failure policy. The allowed_regimes
// gate historically failed OPEN when the regime store could not produce a
// label (#879 policy b) — regime_gate_on_failure makes that policy explicit
// and adds "closed" (hold fresh opens while the regime is unknown). These
// tests cover the policy x label-availability x position-state x
// gate-configured matrix, the resolution order (strategy > global > default),
// load-time validation, hot reload, and the operator-facing surfaces.

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

// ─── regimeBlocksOpen — policy x label x position x gate matrix ──────────────

func TestRegimeBlocksOpenFailurePolicyMatrix(t *testing.T) {
	gate := []string{"trending_up"}
	cases := []struct {
		name       string
		allowed    []string
		current    string
		posQty     float64
		failClosed bool
		want       bool
	}{
		// Fail-open (legacy #879): empty label admits the entry.
		{"open policy, empty label, flat, gated", gate, "", 0, false, false},
		// Fail-closed: empty label holds the fresh open.
		{"closed policy, empty label, flat, gated", gate, "", 0, true, true},
		// Whitespace-only label is "unknown" too.
		{"closed policy, whitespace label, flat, gated", gate, "  ", 0, true, true},
		// A KNOWN label is decided by set membership under BOTH policies.
		{"open policy, matching label", gate, "trending_up", 0, false, false},
		{"closed policy, matching label", gate, "trending_up", 0, true, false},
		{"open policy, mismatching label", gate, "ranging", 0, false, true},
		{"closed policy, mismatching label", gate, "ranging", 0, true, true},
		// posQty>0 (open position: closes + management) is NEVER blocked,
		// including the compound fail-closed + empty-label + gated state.
		{"closed policy, empty label, open position", gate, "", 1.5, true, false},
		{"closed policy, mismatching label, open position", gate, "ranging", 0.5, true, false},
		// No gate configured → fail-closed never fires.
		{"closed policy, empty label, no gate", nil, "", 0, true, false},
		{"closed policy, empty label, empty gate", []string{}, "", 0, true, false},
	}
	for _, tc := range cases {
		if got := regimeBlocksOpen(tc.allowed, tc.current, tc.posQty, tc.failClosed); got != tc.want {
			t.Errorf("%s: regimeBlocksOpen = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// ─── resolution order: strategy > global > default ───────────────────────────

func TestResolveRegimeGateOnFailure(t *testing.T) {
	cases := []struct {
		name     string
		strategy string
		global   string
		want     string
	}{
		{"both empty defaults open", "", "", RegimeGateOnFailureOpen},
		{"global closed applies", "", "closed", RegimeGateOnFailureClosed},
		{"strategy overrides global open", "closed", "open", RegimeGateOnFailureClosed},
		{"strategy overrides global closed", "open", "closed", RegimeGateOnFailureOpen},
		{"case and whitespace normalized", "  Closed ", "", RegimeGateOnFailureClosed},
	}
	for _, tc := range cases {
		sc := StrategyConfig{RegimeGateOnFailure: tc.strategy}
		rc := &RegimeConfig{Enabled: true, GateOnFailure: tc.global}
		if got := resolveRegimeGateOnFailure(sc, rc); got != tc.want {
			t.Errorf("%s: resolved %q, want %q", tc.name, got, tc.want)
		}
	}
	// nil RegimeConfig must not panic and must default open.
	if got := resolveRegimeGateOnFailure(StrategyConfig{}, nil); got != RegimeGateOnFailureOpen {
		t.Errorf("nil regime config: resolved %q, want open", got)
	}
}

func TestParseRegimeGateOnFailure(t *testing.T) {
	for _, ok := range []string{"", "open", "closed", " CLOSED "} {
		if _, err := parseRegimeGateOnFailure(ok); err != nil {
			t.Errorf("%q: unexpected error %v", ok, err)
		}
	}
	for _, bad := range []string{"close", "fail-closed", "true", "0"} {
		if _, err := parseRegimeGateOnFailure(bad); err == nil {
			t.Errorf("%q: expected rejection", bad)
		}
	}
}

// ─── applyRegimeGate — the single funnel all 6 dispatch sites use ─────────────

func TestApplyRegimeGateFailClosedOnEmptyStorePayload(t *testing.T) {
	rc := testRegimeConfig()
	gated := StrategyConfig{AllowedRegimes: []string{"trending_up"}, RegimeGateOnFailure: "closed"}

	// Empty payload (store failure / sealed budget / missing window) + flat →
	// blocked under "closed", admitted under "open" (and under the default).
	if label, blocked := applyRegimeGate(gated, RegimePayload{}, rc, 0); !blocked || label != "" {
		t.Errorf("fail-closed empty payload: (label=%q, blocked=%v), want blocked with empty label", label, blocked)
	}
	openPolicy := gated
	openPolicy.RegimeGateOnFailure = "open"
	if _, blocked := applyRegimeGate(openPolicy, RegimePayload{}, rc, 0); blocked {
		t.Error("explicit fail-open must admit the entry on an empty payload")
	}
	defaulted := gated
	defaulted.RegimeGateOnFailure = ""
	if _, blocked := applyRegimeGate(defaulted, RegimePayload{}, rc, 0); blocked {
		t.Error("omitted policy must preserve the legacy #879 fail-open behavior")
	}

	// Open position: the fail-closed arm must never touch management cycles.
	if _, blocked := applyRegimeGate(gated, RegimePayload{}, rc, 2.0); blocked {
		t.Error("fail-closed must never block while a position is open (posQty>0)")
	}
	// No gate configured: fail-closed never fires.
	ungated := StrategyConfig{RegimeGateOnFailure: "closed"}
	if _, blocked := applyRegimeGate(ungated, RegimePayload{}, rc, 0); blocked {
		t.Error("fail-closed must never fire without an allowed_regimes gate")
	}
	// A present label still resolves by set membership under fail-closed.
	if _, blocked := applyRegimeGate(gated, RegimePayload{Legacy: "trending_up"}, rc, 0); blocked {
		t.Error("matching label must admit the entry under fail-closed")
	}
	if _, blocked := applyRegimeGate(gated, RegimePayload{Legacy: "ranging"}, rc, 0); !blocked {
		t.Error("mismatching label must block under fail-closed")
	}
}

func TestApplyRegimeGateGlobalDefaultFailClosed(t *testing.T) {
	rc := testRegimeConfig()
	rc.GateOnFailure = "closed"
	sc := StrategyConfig{AllowedRegimes: []string{"trending_up"}}
	if _, blocked := applyRegimeGate(sc, RegimePayload{}, rc, 0); !blocked {
		t.Error("global regime.gate_on_failure=closed must apply to strategies without a per-strategy value")
	}
	sc.RegimeGateOnFailure = "open"
	if _, blocked := applyRegimeGate(sc, RegimePayload{}, rc, 0); blocked {
		t.Error("per-strategy open must override the global closed default")
	}
}

// End-to-end through the store: a failed bundle blocks the gated fail-closed
// strategy while its fail-open peer on the SAME signature is admitted.
func TestRegimeStoreFailureRespectsPerStrategyPolicy(t *testing.T) {
	rc := testRegimeConfig()
	closedSC := StrategyConfig{ID: "hl-closed", Type: "perps", Platform: "hyperliquid",
		Args: []string{"momentum", "BTC", "1h"}, AllowedRegimes: []string{"trending_up"}, RegimeGateOnFailure: "closed"}
	openSC := StrategyConfig{ID: "hl-open", Type: "perps", Platform: "hyperliquid",
		Args: []string{"mean_reversion", "BTC", "1h"}, AllowedRegimes: []string{"trending_up"}}
	stubRegimeBundleCheck(t, func(_ context.Context, req regimeBundleRequest) (*RegimeBundle, error) {
		return nil, fmt.Errorf("regime bundle %s: boom", req.Key)
	})
	store := &RegimeStore{}
	populateRegimeStore(store, []StrategyConfig{closedSC, openSC}, rc, nil)

	if _, blocked := applyRegimeGate(closedSC, store.PayloadForStrategy(closedSC, rc), rc, 0); !blocked {
		t.Error("fail-closed strategy must be blocked when its regime bundle failed")
	}
	if _, blocked := applyRegimeGate(openSC, store.PayloadForStrategy(openSC, rc), rc, 0); blocked {
		t.Error("fail-open peer must be admitted when its regime bundle failed")
	}
}

// ─── dispatch log line ───────────────────────────────────────────────────────

func TestRegimeGateBlockDetail(t *testing.T) {
	if got := regimeGateBlockDetail(""); got != "regime unknown, fail-closed" {
		t.Errorf("empty label detail = %q", got)
	}
	if got := regimeGateBlockDetail("ranging"); got != "regime=ranging" {
		t.Errorf("known label detail = %q", got)
	}
}

// ─── config load validation ──────────────────────────────────────────────────

func testGatePolicyConfig(strategyPolicy, globalPolicy string, regimeEnabled bool) *Config {
	return &Config{
		Strategies: []StrategyConfig{{
			ID:                  "hl-eth",
			Type:                "perps",
			Platform:            "hyperliquid",
			Script:              "shared_scripts/check_hyperliquid.py",
			Args:                []string{"momentum", "ETH", "1h", "--mode=paper"},
			Capital:             500,
			MaxDrawdownPct:      50,
			AllowedRegimes:      []string{"trending_up"},
			RegimeGateOnFailure: strategyPolicy,
		}},
		Regime: &RegimeConfig{Enabled: regimeEnabled, Period: 14, ADXThreshold: 20, GateOnFailure: globalPolicy},
	}
}

func TestValidateConfigRejectsUnknownGateOnFailure(t *testing.T) {
	cfg := testGatePolicyConfig("fail-closed", "", true)
	err := validateConfig(cfg, true)
	if err == nil || !strings.Contains(err.Error(), "regime_gate_on_failure") {
		t.Fatalf("unknown per-strategy value must be rejected, got %v", err)
	}

	cfg = testGatePolicyConfig("", "sideways", true)
	err = validateConfig(cfg, true)
	if err == nil || !strings.Contains(err.Error(), "regime.gate_on_failure") {
		t.Fatalf("unknown global value must be rejected, got %v", err)
	}
}

func TestValidateConfigAcceptsGateOnFailureValues(t *testing.T) {
	for _, policy := range []string{"", "open", "closed"} {
		cfg := testGatePolicyConfig(policy, "", true)
		if err := validateConfig(cfg, true); err != nil && strings.Contains(err.Error(), "gate_on_failure") {
			t.Errorf("policy %q: unexpected gate_on_failure error: %v", policy, err)
		}
	}
}

func TestValidateConfigRejectsFailClosedWithRegimeDisabled(t *testing.T) {
	// Fail-closed + allowed_regimes + regime.enabled=false is a PERMANENT
	// entry block (the gate label is deterministically empty every cycle) —
	// a load error, not the legacy no-op warning.
	cfg := testGatePolicyConfig("closed", "", false)
	err := validateConfig(cfg, true)
	if err == nil || !strings.Contains(err.Error(), "could never open") {
		t.Fatalf("fail-closed with regime disabled must be rejected, got %v", err)
	}
	// The same shape resolved through the GLOBAL default is equally permanent.
	cfg = testGatePolicyConfig("", "closed", false)
	err = validateConfig(cfg, true)
	if err == nil || !strings.Contains(err.Error(), "could never open") {
		t.Fatalf("global fail-closed with regime disabled must be rejected, got %v", err)
	}
	// Fail-open with regime disabled keeps loading (legacy warn-only no-op).
	cfg = testGatePolicyConfig("", "", false)
	if err := validateConfig(cfg, true); err != nil && strings.Contains(err.Error(), "gate_on_failure") {
		t.Fatalf("fail-open with regime disabled must keep loading, got %v", err)
	}
}

// ─── hot reload — always allowed, including while a position is open ──────────

func TestHotReloadGateOnFailureWhileOpen(t *testing.T) {
	base := func(policy, globalPolicy string) *Config {
		return &Config{
			IntervalSeconds: 600,
			DBFile:          "scheduler/state.db",
			Strategies: []StrategyConfig{{
				ID:                  "hl-eth",
				Type:                "perps",
				Platform:            "hyperliquid",
				Script:              "shared_scripts/check_hyperliquid.py",
				Args:                []string{"momentum", "ETH", "1h", "--mode=paper"},
				Capital:             500,
				MaxDrawdownPct:      50,
				IntervalSeconds:     600,
				Leverage:            2,
				AllowedRegimes:      []string{"trending_up"},
				RegimeGateOnFailure: policy,
			}},
			Regime: &RegimeConfig{Enabled: true, Period: 14, ADXThreshold: 20, GateOnFailure: globalPolicy},
		}
	}
	cfg := base("", "")
	next := base("closed", "closed")
	state := &AppState{Strategies: map[string]*StrategyState{
		"hl-eth": {ID: "hl-eth", Cash: 100, Positions: map[string]*Position{
			"ETH": {Quantity: 1.5, AvgCost: 2000, Side: "long"},
		}},
	}}
	var mu sync.RWMutex
	server := NewStatusServer(state, &mu, "", cfg.Strategies, nil)
	changes, err := applyHotReloadConfig(cfg, next, state, nil, server)
	if err != nil {
		t.Fatalf("regime_gate_on_failure must hot-reload while a position is open: %v", err)
	}
	if got := cfg.Strategies[0].RegimeGateOnFailure; got != "closed" {
		t.Errorf("per-strategy policy not applied, got %q", got)
	}
	if got := cfg.Regime.GateOnFailure; got != "closed" {
		t.Errorf("global policy not applied, got %q", got)
	}
	joined := strings.Join(changes, "\n")
	if !strings.Contains(joined, "regime_gate_on_failure") || !strings.Contains(joined, "regime.gate_on_failure") {
		t.Errorf("change log must name both fields, got %q", joined)
	}

	// And back again (closed -> open) with the position still open.
	reverted := base("open", "")
	if _, err := applyHotReloadConfig(cfg, reverted, state, nil, server); err != nil {
		t.Fatalf("reverting to fail-open must hot-reload while open: %v", err)
	}
	if got := cfg.Strategies[0].RegimeGateOnFailure; got != "open" {
		t.Errorf("revert not applied, got %q", got)
	}
}

// ─── operator surfaces ───────────────────────────────────────────────────────

func TestRegimeGateOutagePolicyNote(t *testing.T) {
	rc := testRegimeConfig()
	mk := func(id, coin, policy string, gated bool) StrategyConfig {
		sc := StrategyConfig{ID: id, Type: "perps", Platform: "hyperliquid",
			Args: []string{"momentum", coin, "1h"}, RegimeGateOnFailure: policy}
		if gated {
			sc.AllowedRegimes = []string{"trending_up"}
		}
		return sc
	}
	btc := mk("hl-btc", "BTC", "closed", true)
	key, ok := strategyRegimeBundleRequest(btc, rc)
	if !ok {
		t.Fatal("expected a regime signature for the HL perps strategy")
	}
	due := []StrategyConfig{
		mk("hl-zzz", "BTC", "closed", true), // same key, closed — sorted after hl-btc
		btc,
		mk("hl-open", "BTC", "", true),           // same key, fail-open
		mk("hl-ungated", "BTC", "closed", false), // no gate → excluded
		mk("hl-eth", "ETH", "closed", true),      // different signature → excluded
	}
	note := regimeGateOutagePolicyNote(key.Key, due, rc)
	want := "; entry gates — fail-closed (opens held): hl-btc, hl-zzz; fail-open (entries ungated): hl-open"
	if note != want {
		t.Errorf("note = %q, want %q", note, want)
	}
	// No gated strategies on the key → empty note.
	if got := regimeGateOutagePolicyNote(key.Key, []StrategyConfig{mk("hl-ungated", "BTC", "closed", false)}, rc); got != "" {
		t.Errorf("ungated-only note = %q, want empty", got)
	}
}

func TestRegimeGateFailClosedActive(t *testing.T) {
	rc := testRegimeConfig()
	origStore := globalRegimeStore
	globalRegimeStore = &RegimeStore{}
	t.Cleanup(func() { globalRegimeStore = origStore })

	sc := StrategyConfig{ID: "hl-btc", Type: "perps", Platform: "hyperliquid",
		Args: []string{"momentum", "BTC", "1h"}, AllowedRegimes: []string{"trending_up"}, RegimeGateOnFailure: "closed"}
	flat := &StrategyState{ID: "hl-btc"}

	// Empty store (failed/missing bundle) + flat + closed policy → active.
	if !regimeGateFailClosedActive(sc, flat, rc) {
		t.Error("empty store + flat + fail-closed must report an active fail-closed gate")
	}
	// Open position → never active (management is untouched).
	open := &StrategyState{ID: "hl-btc", Positions: map[string]*Position{"BTC": {Quantity: 1}}}
	if regimeGateFailClosedActive(sc, open, rc) {
		t.Error("an open position must clear the fail-closed marker")
	}
	// Fail-open policy → never active.
	openPolicy := sc
	openPolicy.RegimeGateOnFailure = ""
	if regimeGateFailClosedActive(openPolicy, flat, rc) {
		t.Error("default fail-open policy must never mark the gate closed")
	}
	// No gate → never active.
	ungated := sc
	ungated.AllowedRegimes = nil
	if regimeGateFailClosedActive(ungated, flat, rc) {
		t.Error("no allowed_regimes gate must never mark the gate closed")
	}
	// Store produces a label → not active.
	req, ok := strategyRegimeBundleRequest(sc, rc)
	if !ok {
		t.Fatal("expected a regime signature")
	}
	gen := globalRegimeStore.resetForCycle(time.Now().UTC())
	globalRegimeStore.set(&RegimeBundle{Key: req.Key, Payload: RegimePayload{Legacy: "trending_up"}, At: time.Now().UTC()}, gen)
	if regimeGateFailClosedActive(sc, flat, rc) {
		t.Error("a resolvable gate label must clear the fail-closed marker")
	}
}

func TestDecorateRegimeLabelGateClosed(t *testing.T) {
	if got := decorateRegimeLabelGateClosed(""); got != "? (gate closed)" {
		t.Errorf("empty label = %q", got)
	}
	if got := decorateRegimeLabelGateClosed("ranging"); got != "ranging (gate closed)" {
		t.Errorf("stale label = %q", got)
	}
}

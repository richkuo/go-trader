package main

import (
	"fmt"
	"strings"
)

// Regime entry-gate failure policy (#1278). Controls what the allowed_regimes
// gate does when the regime store cannot produce a gate label this cycle
// (regime-subprocess failure, sealed phase budget, or missing window):
//   - "open"   (default): admit the entry — the legacy #879 fail-open policy.
//   - "closed": hold NEW opens while the regime is unknown. Never affects
//     closes or open-position management (regimeBlocksOpen's posQty>0
//     short-circuit runs first), and never fires when no gate is configured.
const (
	RegimeGateOnFailureOpen   = "open"
	RegimeGateOnFailureClosed = "closed"
)

// normalizeRegimeGateOnFailure canonicalizes a regime_gate_on_failure value.
// Empty stays empty (meaning "inherit"); unknown values are the caller's
// problem — parseRegimeGateOnFailure rejects them at config load.
func normalizeRegimeGateOnFailure(v string) string {
	return strings.ToLower(strings.TrimSpace(v))
}

// parseRegimeGateOnFailure validates a regime_gate_on_failure value at config
// load. Empty is valid (inherit / default open).
func parseRegimeGateOnFailure(v string) (string, error) {
	n := normalizeRegimeGateOnFailure(v)
	switch n {
	case "", RegimeGateOnFailureOpen, RegimeGateOnFailureClosed:
		return n, nil
	}
	return "", fmt.Errorf("regime_gate_on_failure must be %q or %q, got %q", RegimeGateOnFailureOpen, RegimeGateOnFailureClosed, v)
}

// resolveRegimeGateOnFailure resolves the effective entry-gate failure policy
// for a strategy: per-strategy regime_gate_on_failure wins, else the global
// regime.gate_on_failure default, else "open" (the legacy #879 behavior, so
// existing configs are byte-identical).
func resolveRegimeGateOnFailure(sc StrategyConfig, rc *RegimeConfig) string {
	if v := normalizeRegimeGateOnFailure(sc.RegimeGateOnFailure); v != "" {
		return v
	}
	if rc != nil {
		if v := normalizeRegimeGateOnFailure(rc.GateOnFailure); v != "" {
			return v
		}
	}
	return RegimeGateOnFailureOpen
}

// regimeAllowsEntry reports whether the current market regime permits a new
// entry for a strategy. Returns true when:
//   - allowed is empty (no gate configured), OR
//   - current is empty (regime not available from check script), OR
//   - current is in the allowed set.
//
// This is the core regime check; callers gate it on "is this an open?" via
// regimeBlocksOpen so close legs always pass through.
//
// #1124: a bare `ranging_directional` in `allowed` matches its `_up`/`_down`
// sub-labels (the producer relabels non-zero drift). The expansion is
// one-directional — bare→subs — so an operator listing an explicit `_up`
// still gates out `_down`. This mirrors the family rule applied to the
// regime-keyed close blocks so the producer relabeling never silently re-gates
// a previously-entering strategy.
func regimeAllowsEntry(allowed []string, current string) bool {
	if len(allowed) == 0 || current == "" {
		return true
	}
	cur := strings.TrimSpace(current)
	for _, label := range allowed {
		if label == cur {
			return true
		}
	}
	// bare ranging_directional covers its #1124 sub-labels.
	if regimeDirectionalSubs[cur] {
		for _, label := range allowed {
			if label == regimeDirectionalBare {
				return true
			}
		}
	}
	return false
}

// regimeBlocksOpen reports whether the regime gate should suppress this
// dispatch. Closes (any non-zero existing position quantity) always pass
// through — the gate only fires when no position exists, so the strategy is
// attempting to open. This preserves the PR contract that "existing positions
// are always managed by close paths regardless".
//
// #1278: failClosed selects the failure policy for an UNKNOWN regime (empty
// gate label — regime-store failure, sealed phase budget, or missing window).
// It fires only when a gate is actually configured (len(allowed)>0) and the
// strategy is flat; strategies without a gate and all posQty>0 management are
// untouched by construction. failClosed=false preserves the legacy #879
// fail-open behavior exactly.
func regimeBlocksOpen(allowed []string, current string, posQty float64, failClosed bool) bool {
	if posQty > 0 {
		return false
	}
	if failClosed && len(allowed) > 0 && strings.TrimSpace(current) == "" {
		return true
	}
	return !regimeAllowsEntry(allowed, current)
}

// regimeGateBlockDetail renders the parenthesized reason for the dispatch-site
// "Regime gate: open signal blocked (…)" log line: an unknown-regime
// fail-closed block is named explicitly so an operator reading the log can
// tell a store outage from a normal not-in-allowed-set block (#1278).
func regimeGateBlockDetail(gateLabel string) string {
	if strings.TrimSpace(gateLabel) == "" {
		return "regime unknown, fail-closed"
	}
	return "regime=" + gateLabel
}

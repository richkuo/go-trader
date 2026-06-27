package main

import "strings"

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
func regimeBlocksOpen(allowed []string, current string, posQty float64) bool {
	if posQty > 0 {
		return false
	}
	return !regimeAllowsEntry(allowed, current)
}

package main

// regimeAllowsEntry reports whether the current market regime permits a new
// entry for a strategy. Returns true when:
//   - allowed is empty (no gate configured), OR
//   - current is empty (regime not available from check script), OR
//   - current is in the allowed set.
//
// This is the core regime check; callers gate it on "is this an open?" via
// regimeBlocksOpen so close legs always pass through.
func regimeAllowsEntry(allowed []string, current string) bool {
	if len(allowed) == 0 || current == "" {
		return true
	}
	for _, label := range allowed {
		if label == current {
			return true
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

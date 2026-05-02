package main

// regimeAllowsEntry reports whether the current market regime permits a new
// entry for a strategy. Returns true when:
//   - allowed is empty (no gate configured), OR
//   - current is empty (regime not available from check script), OR
//   - current is in the allowed set.
//
// This is the single gate checked at every dispatch site before placing an
// order. Existing positions are always managed by close paths regardless.
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

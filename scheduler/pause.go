package main

// #1150: per-strategy pause/resume.
//
// A paused strategy (StrategyConfig.Paused) is NOT skipped in the
// dueStrategies loop — trailing-SL updates, paper SL/TP simulation, the TP
// ratchet, and protection sync all run inside the per-strategy dispatch, so a
// bare skip would leave paper positions with no SL/TP at all and live
// positions with stale software management. Instead the dispatch runs its
// full cycle and pausedBlocksSignal forces position-INCREASING check signals
// to hold (0), mirroring the #1046 latched-circuit-breaker manage-only shape.
// Unlike #1046 (which suppresses closes too), pause lets position-REDUCING
// actions through so an open position rides its natural exit
// (SL/TP/close_strategy).

// pausedBlocksSignal reports whether a paused strategy's check signal must be
// forced to hold (0) this cycle. It blocks everything that could increase
// exposure and passes everything that can only reduce it:
//
// Blocked:
//   - any non-zero signal while flat (fresh open);
//   - a same-side signal on an open position (scale-in add / re-affirm);
//   - an opposite-side signal that the direction gate would turn into a flip
//     (direction="both": close + reopen on the other side) or a fresh open
//     (the legacy buy-on-short-under-"long" edge, which fresh-sizes without
//     offset — see perpsLiveOrderSize / #656).
//
// Passed through:
//   - signal == 0 (nothing to block; the manage path runs as normal);
//   - a close action from the open/close registry (closeFraction > 0,
//     partial or full — compose_signal only emits these while a position is
//     open);
//   - a pure-close directional exit, mirroring perpsCloseActionSuppressesNewSL:
//     sell on a long with shorts disallowed, or buy on a short with longs
//     disallowed (spot is long-only — ExecuteSpotSignalWithFillFee's sell branch only
//     ever closes a long — so spot sells always qualify).
//
// posSide is the open position's side ("long"/"short"; spot positions are
// "long"). allowsLong/allowsShort describe what the EXECUTOR can open for an
// opposite-side signal, not just the config gate: PerpsAllowsLong/
// PerpsAllowsShort for perps, true/false for long-only spot, and true/true
// for futures — ExecuteFuturesSignalWithFillFee is unconditionally bidirectional (a sell
// on a long with closeFraction 0 closes the long AND opens a fresh short, and
// a buy on a short mirrors it), so a futures opposite-side signal is never a
// pure close and must be held.
func pausedBlocksSignal(signal int, closeFraction, posQty float64, posSide string, allowsLong, allowsShort bool) bool {
	if signal == 0 {
		return false
	}
	if posQty <= 0 {
		// Fresh open from flat. A stale close action (closeFraction > 0) with
		// no position would no-op in the executor anyway; holding it is safe.
		return true
	}
	if closeFraction > 0 {
		return false // close action from the open/close registry
	}
	if signal == -1 && posSide != "short" && !allowsShort {
		return false // long-only exit: sell can only close the long
	}
	if signal == 1 && posSide == "short" && !allowsLong {
		return false // short-only exit: buy can only close the short
	}
	return true // add, flip, or legacy fresh-open edge — all grow exposure
}

// pausedOptionsActions filters an options result's action list down to the
// close actions a paused strategy may still execute. "buy" and "sell" both
// OPEN option positions (long and written legs); only "close" reduces
// exposure. Returns the surviving actions and how many were dropped.
func pausedOptionsActions(actions []OptionsAction) (kept []OptionsAction, dropped int) {
	for _, a := range actions {
		if a.Action == "close" {
			kept = append(kept, a)
		} else {
			dropped++
		}
	}
	return kept, dropped
}

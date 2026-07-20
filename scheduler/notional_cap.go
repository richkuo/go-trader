package main

// #1344: portfolio gross notional cap (#42) is an ENTRY hold, not a cycle skip.
//
// CheckPortfolioRisk still returns notionalBlocked=true when total notional
// exceeds portfolio_risk.max_notional_usd. The pre-#1344 consumer in main.go
// answered that with a strategy-level `continue`, which skipped the entire
// check-script dispatch — including close/reduce signal evaluation and the
// perps trailing-SL / TP-ratchet / protection-sync manage paths that only
// run inside that branch. Over-cap therefore suppressed risk-reducing work.
//
// The correct shape matches #1150/#1269/#1270: the strategy cycle always
// runs, and position-INCREASING signals are forced to hold via
// pausedBlocksSignal at the six regime-gated dispatch sites (plus
// pausedOptionsActions for options opens). Position-REDUCING actions and
// Signal==0 manage-only paths pass by construction. Manual open/add/
// limit-open refuse next to their kill-switch / daily-loss / exposure-cap
// guards. Nothing is ever force-closed.
//
// notionalCapSkipsStrategyCycle locks the invariant that the dispatch loop
// must never `continue` past a strategy solely because the notional cap is
// breached (the #1046 cbManageOnly carve-out is obsolete for this gate —
// Signal==0 already no-ops the hold predicate).

import "fmt"

// notionalCapSkipsStrategyCycle reports whether the per-strategy dispatch
// loop should skip the rest of the cycle when the notional cap is breached.
// #1344: always false — holds are per-signal; never skip close/SL maintenance.
//
// main.go MUST call this (and only this) for any whole-strategy notional skip
// so TestNotionalCapNeverSkipsStrategyCycle has production teeth: changing
// the return to true fails CI, and a raw `if notionalBlocked { continue }`
// bypass is caught by the main.go source scan in the same test.
func notionalCapSkipsStrategyCycle(notionalBlocked bool) bool {
	_ = notionalBlocked
	return false
}

// notionalCapHoldDetail is the operator-facing reason when the gross notional
// cap is breached. Matches CheckPortfolioRisk's notional reason shape, with
// the #1344 clarification that exits keep running.
func notionalCapHoldDetail(totalNotional, capUSD float64) string {
	return fmt.Sprintf("portfolio notional $%.2f exceeds cap $%.2f — new opens blocked, exits continue",
		totalNotional, capUSD)
}

// evaluateNotionalCapHold is a pure read of the #42/#1344 gross notional
// entry hold for paths that cannot call CheckPortfolioRisk (manual CLI /
// dashboard — that helper also mutates peak/drawdown kill-switch state).
// nil prices fall back to AvgCost inside PortfolioNotional, matching the
// exposure-cap manual path. Safe under mu.RLock.
func evaluateNotionalCapHold(pr *PortfolioRiskConfig, strategies map[string]*StrategyState, prices map[string]float64) (held bool, detail string) {
	if pr == nil || pr.MaxNotionalUSD <= 0 {
		return false, ""
	}
	total := PortfolioNotional(strategies, prices)
	if total > pr.MaxNotionalUSD {
		return true, notionalCapHoldDetail(total, pr.MaxNotionalUSD)
	}
	return false, ""
}

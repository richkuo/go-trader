package main

// #1269: portfolio-wide hard daily loss limit.
//
// When the day's aggregate realized PnL across ALL strategies falls past a
// configured loss threshold (portfolio_risk.daily_max_loss_usd and/or
// daily_max_loss_pct), every position-INCREASING action is held for the rest
// of the UTC day: the six regime-gated dispatch sites reuse the #1150
// pausedBlocksSignal predicate (fresh opens, scale-in adds, and flips are
// forced to hold; close-registry / pure-close exits pass through), options
// open actions are dropped via pausedOptionsActions, and the manual CLI
// open/add paths refuse next to their existing kill-switch / pending-CB
// guards. Nothing is ever force-closed by this mechanism, and it never
// interferes with the kill switch or circuit breakers — it only suppresses
// new exposure.
//
// The gate is UNLATCHED by design: it is recomputed each evaluation from the
// per-strategy RiskState.DailyPnL values (persisted in SQLite), so it
// survives restarts for free and clears automatically at the UTC rollover
// already implemented by rolloverDailyPnL — no separate latch state exists.
// DailyPnL is fed by RecordTradeResult with the same PRE-FEE realized PnL the
// trades ledger stores (#918: fees are stamped separately and read via
// tradeNetPnL), so the threshold measures pre-fee realized loss; size it
// accordingly.
//
// Stale per-strategy days are handled without mutation: a strategy whose
// DailyPnLDate is not today contributes 0 to the aggregate — exactly what
// rolloverDailyPnL would reset it to — so the evaluation is a pure read and
// can run under mu.RLock.

import (
	"fmt"
	"time"
)

// DailyLossLimitStatus is the once-per-cycle evaluation of the #1269 daily
// loss limit. LossUSD is the day's aggregate realized loss (>= 0; wins offset
// losses — a net-positive day is LossUSD 0).
type DailyLossLimitStatus struct {
	Configured   bool    // at least one of the two thresholds is set
	Tripped      bool    // aggregate loss has reached a configured threshold
	DailyPnL     float64 // aggregate realized PnL today across all strategies (pre-fee)
	LossUSD      float64 // max(0, -DailyPnL)
	CapitalBasis float64 // sum of per-strategy InitialCapital (the pct basis)
	ThresholdUSD float64 // effective USD threshold (lowest configured arm; 0 when pct-only with no basis)
	PctBasisMiss bool    // pct arm configured but CapitalBasis <= 0, so it cannot evaluate
}

// dailyLossLimitConfigured reports whether either daily-loss threshold is set.
func dailyLossLimitConfigured(pr *PortfolioRiskConfig) bool {
	return pr != nil && (pr.DailyMaxLossUSD > 0 || pr.DailyMaxLossPct > 0)
}

// evaluateDailyLossLimit aggregates today's realized PnL across every
// strategy state and compares the loss against the configured thresholds.
// Pure read — never mutates state (see the stale-day note in the file
// comment); safe under mu.RLock. now must be UTC-meaningful (callers pass
// time.Now().UTC()); the day key matches rolloverDailyPnL's format.
func evaluateDailyLossLimit(pr *PortfolioRiskConfig, states map[string]*StrategyState, now time.Time) DailyLossLimitStatus {
	st := DailyLossLimitStatus{Configured: dailyLossLimitConfigured(pr)}
	today := now.UTC().Format("2006-01-02")
	for _, ss := range states {
		if ss == nil {
			continue
		}
		if ss.RiskState.DailyPnLDate == today {
			st.DailyPnL += ss.RiskState.DailyPnL
		}
		if ss.InitialCapital > 0 {
			st.CapitalBasis += ss.InitialCapital
		}
	}
	if st.DailyPnL < 0 {
		st.LossUSD = -st.DailyPnL
	}
	if !st.Configured {
		return st
	}
	// Effective USD threshold = the lowest (most protective) configured arm.
	// The pct arm needs a positive capital basis to resolve; when the basis is
	// zero (fresh state, no initial_capital anywhere) it cannot evaluate and
	// is flagged so callers can surface the gap instead of silently ignoring
	// a configured protection.
	if pr.DailyMaxLossUSD > 0 {
		st.ThresholdUSD = pr.DailyMaxLossUSD
	}
	if pr.DailyMaxLossPct > 0 {
		if st.CapitalBasis > 0 {
			pctUSD := st.CapitalBasis * pr.DailyMaxLossPct / 100
			if st.ThresholdUSD == 0 || pctUSD < st.ThresholdUSD {
				st.ThresholdUSD = pctUSD
			}
		} else {
			st.PctBasisMiss = true
		}
	}
	st.Tripped = st.ThresholdUSD > 0 && st.LossUSD >= st.ThresholdUSD
	return st
}

// dailyLossHoldDetail is the one-line operator explanation used by cycle
// logs, the manual-CLI refusal, and the /status note.
func dailyLossHoldDetail(st DailyLossLimitStatus) string {
	return fmt.Sprintf("daily loss limit tripped: today's realized loss $%.2f >= threshold $%.2f (pre-fee; basis=$%.2f initial capital)",
		st.LossUSD, st.ThresholdUSD, st.CapitalBasis)
}

// formatDailyLossTripDM builds the once-per-day owner DM sent on trip.
func formatDailyLossTripDM(st DailyLossLimitStatus, now time.Time) string {
	return fmt.Sprintf(
		"🛑 **Daily loss limit tripped** (%s UTC)\n"+
			"Today's aggregate realized PnL: $%.2f (pre-fee, across all strategies)\n"+
			"Threshold: $%.2f (capital basis $%.2f)\n"+
			"All fresh opens, scale-in adds, and flips are held for the rest of the UTC day — including manual-open/manual-add. "+
			"Open positions keep being managed (closes, trailing SL, ratchet, protection sync) and nothing is force-closed. "+
			"Entries resume automatically at the next UTC rollover.",
		now.UTC().Format("2006-01-02 15:04"), st.DailyPnL, st.ThresholdUSD, st.CapitalBasis)
}

// dailyLossAlertDue reports whether the trip DM should fire: once per UTC day
// while tripped. lastAlertDate is the day key of the last DM sent ("" =
// never); a process restart re-arms it, which re-DMs at most once — acceptable
// (and arguably useful) for an auto-protective halt.
func dailyLossAlertDue(tripped bool, lastAlertDate, today string) bool {
	return tripped && lastAlertDate != today
}

// dailyLossLastAlertDate throttles the trip DM to once per UTC day. Written
// only from the main trading loop (single goroutine).
var dailyLossLastAlertDate string

// dailyLossStartupSummaryLine is the one-line [config] summary printed at
// startup when a daily loss limit is configured. Empty when disabled.
func dailyLossStartupSummaryLine(pr *PortfolioRiskConfig) string {
	if !dailyLossLimitConfigured(pr) {
		return ""
	}
	parts := ""
	if pr.DailyMaxLossUSD > 0 {
		parts = fmt.Sprintf("usd=$%.2f", pr.DailyMaxLossUSD)
	}
	if pr.DailyMaxLossPct > 0 {
		if parts != "" {
			parts += " "
		}
		parts += fmt.Sprintf("pct=%.2f%% of initial capital", pr.DailyMaxLossPct)
	}
	return fmt.Sprintf("[config] portfolio: daily_max_loss %s (pre-fee realized; blocks new entries for the rest of the UTC day when tripped)", parts)
}

// dailyLossStatusNote renders the /status line for the daily loss limit.
// Empty when no limit is configured; shows the live tripped/armed state
// otherwise. Callers hold mu.RLock (pure read of states).
func dailyLossStatusNote(pr *PortfolioRiskConfig, states map[string]*StrategyState, now time.Time) string {
	if !dailyLossLimitConfigured(pr) {
		return ""
	}
	st := evaluateDailyLossLimit(pr, states, now)
	var note string
	switch {
	case st.Tripped:
		note = fmt.Sprintf("\n🛑 daily loss limit TRIPPED: loss $%.2f >= $%.2f — entries held until UTC rollover", st.LossUSD, st.ThresholdUSD)
	case st.ThresholdUSD > 0:
		note = fmt.Sprintf("\n🟢 daily loss limit armed: today $%.2f / threshold $%.2f", st.DailyPnL, st.ThresholdUSD)
	}
	// An inert pct arm is surfaced UNCONDITIONALLY — even while the USD arm
	// enforces ("armed" above) the operator must see that the configured pct
	// protection is not evaluating (review on #1291).
	if st.PctBasisMiss {
		note += "\n" + dailyLossPctBasisMissWarning
	}
	return note
}

// dailyLossPctBasisMissWarning is the shared operator text for a configured
// pct arm that cannot evaluate. Used verbatim by /status, the per-cycle
// [WARN], and the once-per-day DM so log greps and operator reports match.
const dailyLossPctBasisMissWarning = "⚠️ daily loss limit: daily_max_loss_pct is configured but no strategy has initial_capital > 0 — the pct arm CANNOT evaluate and enforces nothing (set initial_capital or use daily_max_loss_usd)"

// dailyLossPctBasisMissAlertDate throttles the inert-pct-arm owner DM to once
// per UTC day, mirroring dailyLossLastAlertDate. Written only from the main
// trading loop (single goroutine).
var dailyLossPctBasisMissAlertDate string

// formatDailyLossPctBasisMissDM builds the once-per-day owner DM sent while a
// configured pct arm cannot evaluate. A silent auto-protective gap must reach
// an active operator channel, not only the pull-based /status view.
func formatDailyLossPctBasisMissDM(st DailyLossLimitStatus, now time.Time) string {
	usdNote := "No other arm is configured — the daily loss limit is fully inert."
	if st.ThresholdUSD > 0 {
		usdNote = fmt.Sprintf("The USD arm still enforces at $%.2f.", st.ThresholdUSD)
	}
	return fmt.Sprintf(
		"%s\n%s\nToday's aggregate realized PnL: $%.2f. This DM repeats once per UTC day while the gap persists. (%s UTC)",
		dailyLossPctBasisMissWarning, usdNote, st.DailyPnL, now.UTC().Format("2006-01-02 15:04"))
}

package main

import (
	"fmt"
	"sort"
	"strings"
	"sync"
)

// OperatorRequiredEntry is one strategy-platform pair whose per-strategy
// circuit breaker fired but which the scheduler cannot auto-close (OKX spot,
// Robinhood options). Surfaced as a record so tests and /status can inspect
// the set directly without scanning PendingCircuitCloses in two places.
type OperatorRequiredEntry struct {
	StrategyID  string
	Platform    string // PlatformPendingCloseOKXSpot / PlatformPendingCloseRobinhoodOptions
	Symbols     []PendingCircuitCloseSymbol
	CBUntil     string // ISO timestamp — empty when CB not latched (defensive)
	DrawdownPct float64
}

// OperatorRequiredWarningPlan is the output of planOperatorRequiredWarning.
// Callers deliver LogLines to stdout and, when HasEntries() is true, send
// Message through every configured notifier backend.
//
// Pure data — no goroutines, no notifier side effects. Separated from the
// delivery wrapper so the formatting is unit-testable against a static input
// (#363 phase 5; mirrors the planKillSwitchClose pattern from #341).
type OperatorRequiredWarningPlan struct {
	Entries  []OperatorRequiredEntry
	Message  string
	LogLines []string
}

// HasEntries reports whether any operator-required pending close is present.
// Caller uses this as the gate for notifier delivery so an empty cycle stays
// silent.
func (p OperatorRequiredWarningPlan) HasEntries() bool { return len(p.Entries) > 0 }

// planOperatorRequiredWarning scans every strategy's PendingCircuitCloses map
// for entries with OperatorRequired=true and builds the per-cycle warning.
// Runs under the caller's RLock (read-only access to state).
//
// Ordering is deterministic: entries are sorted by (StrategyID, Platform) so
// the operator-facing log line, /status serialization, and Discord message all
// produce byte-identical output across runs — required by the CLAUDE.md rule
// against map-iteration randomness in operator-facing output.
func planOperatorRequiredWarning(state *AppState) OperatorRequiredWarningPlan {
	var plan OperatorRequiredWarningPlan
	if state == nil {
		return plan
	}

	for _, s := range state.Strategies {
		if s == nil || len(s.RiskState.PendingCircuitCloses) == 0 {
			continue
		}
		for platform, pending := range s.RiskState.PendingCircuitCloses {
			if pending == nil || !pending.OperatorRequired {
				continue
			}
			legs := make([]PendingCircuitCloseSymbol, len(pending.Symbols))
			copy(legs, pending.Symbols)
			sort.Slice(legs, func(i, j int) bool { return legs[i].Symbol < legs[j].Symbol })

			entry := OperatorRequiredEntry{
				StrategyID:  s.ID,
				Platform:    platform,
				Symbols:     legs,
				DrawdownPct: s.RiskState.CurrentDrawdownPct,
			}
			if !s.RiskState.CircuitBreakerUntil.IsZero() {
				entry.CBUntil = s.RiskState.CircuitBreakerUntil.UTC().Format("2006-01-02T15:04:05Z")
			}
			plan.Entries = append(plan.Entries, entry)
		}
	}

	sort.Slice(plan.Entries, func(i, j int) bool {
		if plan.Entries[i].StrategyID != plan.Entries[j].StrategyID {
			return plan.Entries[i].StrategyID < plan.Entries[j].StrategyID
		}
		return plan.Entries[i].Platform < plan.Entries[j].Platform
	})

	if len(plan.Entries) == 0 {
		return plan
	}

	for _, e := range plan.Entries {
		plan.LogLines = append(plan.LogLines, fmt.Sprintf(
			"[CRITICAL] operator-required-close: strategy %s platform %s — %s (circuit breaker fired, venue lacks safe auto-close; operator must flatten manually)",
			e.StrategyID, e.Platform, formatOperatorRequiredLegs(e.Symbols),
		))
	}
	plan.Message = formatOperatorRequiredWarningMessage(plan.Entries)
	return plan
}

// formatOperatorRequiredLegs renders a pending-close symbol list as
// "SYM1 (size=N, virtual), SYM2 (size=N, virtual)" with deterministic order.
// Size is the scheduler's virtual position quantity — operators should
// cross-check at the venue before acting on it.
func formatOperatorRequiredLegs(legs []PendingCircuitCloseSymbol) string {
	parts := make([]string, 0, len(legs))
	for _, l := range legs {
		parts = append(parts, fmt.Sprintf("%s (size=%.6f, virtual)", l.Symbol, l.Size))
	}
	return strings.Join(parts, ", ")
}

// operatorRequiredPlatformLabel maps an internal pending-close platform key to
// its operator-facing label. Keeps the message self-describing — "okx_spot"
// and "robinhood_options" are internal identifiers; the formatted message
// should say "OKX spot" and "Robinhood options" to match runbook language.
func operatorRequiredPlatformLabel(platform string) string {
	switch platform {
	case PlatformPendingCloseOKXSpot:
		return "OKX spot"
	case PlatformPendingCloseRobinhoodOptions:
		return "Robinhood options"
	default:
		return platform
	}
}

// formatOperatorRequiredWarningMessage builds the Discord/Telegram message
// from the sorted entries. Header mirrors FormatKillSwitchMessage's
// "GAPS — VERIFY MANUALLY" language so operators skimming notifications see
// the same severity marker they already recognize from portfolio kill events
// (#345 / #346).
func formatOperatorRequiredWarningMessage(entries []OperatorRequiredEntry) string {
	if len(entries) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("**CIRCUIT BREAKER — OPERATOR INTERVENTION REQUIRED**\n")
	suffix := "s"
	if len(entries) == 1 {
		suffix = ""
	}
	b.WriteString(fmt.Sprintf("%d strategy-platform pair%s hit a per-strategy circuit breaker on a venue the scheduler cannot auto-close. Flatten manually.\n",
		len(entries), suffix))
	for _, e := range entries {
		cbSuffix := ""
		if e.CBUntil != "" {
			cbSuffix = fmt.Sprintf(" (CB until %s)", e.CBUntil)
		}
		b.WriteString(fmt.Sprintf("• %s [%s]: %s — drawdown %.1f%%%s\n",
			e.StrategyID, operatorRequiredPlatformLabel(e.Platform),
			formatOperatorRequiredLegs(e.Symbols), e.DrawdownPct, cbSuffix))
	}
	b.WriteString("No automated close will be attempted. Pending remains set until operator clears positions.")
	return b.String()
}

// operatorRequiredNotifier is the narrow notifier surface used by
// drainOperatorRequiredPendingCloses. *MultiNotifier satisfies it in
// production; tests supply a capturing stub so the drain can be exercised
// without a live Discord/Telegram connection.
type operatorRequiredNotifier interface {
	HasBackends() bool
	SendToAllChannels(content string)
	SendOwnerDM(content string)
}

// drainOperatorRequiredPendingCloses emits a CRITICAL warning for every
// strategy with an OperatorRequired=true pending close, once per cycle. Does
// NOT attempt any automated close — the pending entry stays populated in
// RiskState and surfaces through /status, Discord, and Telegram on every
// cycle until the operator flattens manually and the CB resets.
//
// Called from the main loop after all automated-close drains (HL, OKX perps,
// RH crypto, TopStep) in phase 5 of #363. Takes the state mutex as an
// RWMutex; only a read lock is ever held (state is not mutated here — the
// drain's job is to surface the condition, not clear it).
func drainOperatorRequiredPendingCloses(state *AppState, notifier operatorRequiredNotifier, mu *sync.RWMutex) {
	if state == nil {
		return
	}
	mu.RLock()
	plan := planOperatorRequiredWarning(state)
	mu.RUnlock()

	if !plan.HasEntries() {
		return
	}
	for _, line := range plan.LogLines {
		fmt.Println(line)
	}
	if notifier != nil && notifier.HasBackends() && plan.Message != "" {
		notifier.SendToAllChannels(plan.Message)
		notifier.SendOwnerDM(plan.Message)
	}
}

package main

import (
	"fmt"
	"sort"
	"strings"
	"sync"
)

type OperatorRequiredEntry struct {
	StrategyID  string
	Platform    string
	Symbols     []PendingCircuitCloseSymbol
	CBUntil     string
	DrawdownPct float64
}

type OperatorRequiredWarningPlan struct {
	Entries  []OperatorRequiredEntry
	Message  string
	LogLines []string
}

func (p OperatorRequiredWarningPlan) HasEntries() bool { return len(p.Entries) > 0 }

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

func formatOperatorRequiredLegs(legs []PendingCircuitCloseSymbol) string {
	parts := make([]string, 0, len(legs))
	for _, l := range legs {
		parts = append(parts, fmt.Sprintf("%s (size=%.6f, virtual)", l.Symbol, l.Size))
	}
	return strings.Join(parts, ", ")
}

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

type operatorRequiredNotifier interface {
	HasBackends() bool
	SendToAllChannels(content string)
	SendOwnerDM(content string)
}

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

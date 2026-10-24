package main

import (
	"fmt"
	"math"
	"sort"
	"strings"
	"sync"
	"time"
)

const operatorCloseCommandName = "clear-operator-close"

const operatorRequiredTimeFormat = "2006-01-02T15:04:05Z"

type OperatorRequiredEntry struct {
	StrategyID     string
	Platform       string
	Symbols        []PendingCircuitCloseSymbol
	CBUntil        string
	CBActive       bool
	DrawdownPct    float64
	Episode        string
	FiredAt        time.Time
	LastNotifiedAt time.Time
	Due            bool
	pending        *PendingCircuitClose
}

type OperatorRequiredWarningPlan struct {
	Entries  []OperatorRequiredEntry
	Due      []OperatorRequiredEntry
	Message  string
	LogLines []string
}

func (p OperatorRequiredWarningPlan) HasEntries() bool { return len(p.Entries) > 0 }

func sortedOperatorRequiredLegs(legs []PendingCircuitCloseSymbol) []PendingCircuitCloseSymbol {
	out := make([]PendingCircuitCloseSymbol, len(legs))
	copy(out, legs)
	sort.SliceStable(out, func(i, j int) bool { return out[i].Symbol < out[j].Symbol })
	return out
}

func planOperatorRequiredWarning(state *AppState, now time.Time) OperatorRequiredWarningPlan {
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
			entry := OperatorRequiredEntry{
				StrategyID:     s.ID,
				Platform:       platform,
				Symbols:        sortedOperatorRequiredLegs(pending.Symbols),
				DrawdownPct:    s.RiskState.CurrentDrawdownPct,
				Episode:        pending.Episode,
				FiredAt:        pending.FiredAt,
				LastNotifiedAt: pending.LastNotifiedAt,
				Due:            alertThrottleDue(pending.LastNotifiedAt, now),
				pending:        pending,
			}
			if !s.RiskState.CircuitBreakerUntil.IsZero() {
				entry.CBUntil = s.RiskState.CircuitBreakerUntil.UTC().Format(operatorRequiredTimeFormat)
				entry.CBActive = s.RiskState.CircuitBreaker && now.Before(s.RiskState.CircuitBreakerUntil)
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
		schedule := "warning due"
		if !e.Due {
			schedule = "next warning after " + e.LastNotifiedAt.Add(effectiveAlertThrottleInterval()).UTC().Format(operatorRequiredTimeFormat)
		}
		plan.LogLines = append(plan.LogLines, fmt.Sprintf(
			"[CRITICAL] operator-required-close: strategy %s platform %s — %s (circuit breaker fired, venue lacks safe auto-close; operator must flatten manually); %s",
			e.StrategyID, e.Platform, formatOperatorRequiredLegs(e.Symbols), schedule,
		))
		if e.Due {
			plan.Due = append(plan.Due, e)
		}
	}
	plan.Message = formatOperatorRequiredWarningMessage(plan.Due)
	return plan
}

func formatOperatorRequiredLegs(legs []PendingCircuitCloseSymbol) string {
	parts := make([]string, 0, len(legs))
	for _, l := range legs {
		part := fmt.Sprintf("%s (virtual size at fire %.6f)", l.Symbol, l.Size)
		if l.Size == 0 {
			part = fmt.Sprintf("%s (virtual size at fire %.6f; the book was flat at that fire)", l.Symbol, l.Size)
		}
		parts = append(parts, part)
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

func operatorCloseCommandUsage() string {
	return "/" + commandPrefix + operatorCloseCommandName + " <strategy> [platform]"
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
		switch {
		case e.CBActive:
			cbSuffix = fmt.Sprintf(" (CB until %s)", e.CBUntil)
		case e.CBUntil != "":
			cbSuffix = fmt.Sprintf(" (CB cooldown ended %s; trading resumed)", e.CBUntil)
		}
		detail := ""
		if !e.FiredAt.IsZero() {
			detail += " · last fire " + e.FiredAt.UTC().Format(operatorRequiredTimeFormat)
		}
		if e.Episode != "" {
			detail += " · episode " + e.Episode
		}
		b.WriteString(fmt.Sprintf("• %s [%s]: %s — drawdown %.1f%%%s%s\n",
			e.StrategyID, operatorRequiredPlatformLabel(e.Platform),
			formatOperatorRequiredLegs(e.Symbols), e.DrawdownPct, cbSuffix, detail))
	}
	b.WriteString("Sizes are the virtual book at fire time, summed across the fires not cleared; they do not show the current venue position. No automated close will be attempted.\n")
	fmt.Fprintf(&b, "After you flatten the position on the venue, run `%s` in a DM to the bot to clear the entry.\n", operatorCloseCommandUsage())
	fmt.Fprintf(&b, "Until then this warning repeats every %s (`alert_throttle_interval`) and at once on a new circuit-breaker fire; the CRITICAL log line repeats every cycle. Circuit-breaker expiry does not clear the entry.",
		effectiveAlertThrottleInterval())
	return b.String()
}

type operatorRequiredNotifier interface {
	HasBackends() bool
	SendToAllChannelsDelivered(content string) bool
	SendOwnerDMDelivered(content string) bool
}

func drainOperatorRequiredPendingCloses(state *AppState, notifier operatorRequiredNotifier, mu *sync.RWMutex, store *StateStore) {
	if state == nil || mu == nil {
		return
	}
	now := tradingNow().UTC()
	mu.RLock()
	plan := planOperatorRequiredWarning(state, now)
	mu.RUnlock()

	if !plan.HasEntries() {
		return
	}
	for _, line := range plan.LogLines {
		fmt.Println(line)
	}
	if len(plan.Due) == 0 || plan.Message == "" {
		return
	}
	if notifier == nil || !notifier.HasBackends() {
		fmt.Printf("[CRITICAL] operator-required-close: no notification backend is configured; the warning for %d entr%s was not sent and nothing was stamped\n",
			len(plan.Due), pluralSuffix(len(plan.Due), "y", "ies"))
		return
	}
	channelDelivered := notifier.SendToAllChannelsDelivered(plan.Message)
	dmDelivered := notifier.SendOwnerDMDelivered(plan.Message)
	if !channelDelivered && !dmDelivered {
		fmt.Println("[CRITICAL] operator-required-close: the warning reached no channel and no owner DM; nothing was stamped and the next drain retries")
		return
	}
	stampOperatorRequiredReminders(state, mu, store, plan.Due, now)
}

func pluralSuffix(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

func stampOperatorRequiredReminders(state *AppState, mu *sync.RWMutex, store *StateStore, due []OperatorRequiredEntry, now time.Time) {
	mu.Lock()
	defer mu.Unlock()
	stamped := make(map[string]*StrategyState)
	for _, e := range due {
		ss := state.Strategies[e.StrategyID]
		if ss == nil {
			continue
		}
		cur := ss.RiskState.getPendingCircuitClose(e.Platform)
		if cur == nil || cur != e.pending || !cur.OperatorRequired || cur.Episode != e.Episode || !cur.LastNotifiedAt.Equal(e.LastNotifiedAt) {
			fmt.Printf("[WARN] operator-required-close: strategy %s platform %s: the entry changed while the warning was sent; its reminder was not stamped\n", e.StrategyID, e.Platform)
			continue
		}
		cur.LastNotifiedAt = now
		stamped[e.StrategyID] = ss
	}
	if store == nil {
		return
	}
	ids := make([]string, 0, len(stamped))
	for id := range stamped {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		if err := store.SaveStrategyBook(stamped[id]); err != nil {
			fmt.Printf("[CRITICAL] operator-required-close: strategy %s: saving the reminder stamp failed: %v; the warning was sent, the stamp stays in memory and the cycle save retries it (a restart before that save can repeat this warning once)\n", id, err)
		}
	}
}

type operatorCloseAck struct {
	StrategyID string
	Platform   string
	Episode    string
	FiredAt    time.Time
	Legs       []PendingCircuitCloseSymbol
}

func isOperatorClosePlatform(platform string) bool {
	return platform == PlatformPendingCloseOKXSpot || platform == PlatformPendingCloseRobinhoodOptions
}

func operatorCloseLegsEqual(a, b []PendingCircuitCloseSymbol) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].Symbol != b[i].Symbol || a[i].PositionID != b[i].PositionID || math.Float64bits(a[i].Size) != math.Float64bits(b[i].Size) {
			return false
		}
	}
	return true
}

func operatorCloseEpisodeLabel(episode string) string {
	if episode == "" {
		return "none (saved before episodes)"
	}
	return episode
}

func operatorCloseFiredLabel(firedAt time.Time) string {
	if firedAt.IsZero() {
		return "unknown (saved before fire times)"
	}
	return firedAt.UTC().Format(operatorRequiredTimeFormat)
}

func prepareOperatorRequiredCloseAck(state *AppState, mu *sync.RWMutex, strategyID, platform string) (*operatorCloseAck, string) {
	strategyID = strings.TrimSpace(strategyID)
	platform = strings.TrimSpace(platform)
	if strategyID == "" {
		return nil, "usage: " + operatorCloseCommandUsage()
	}
	if platform != "" && !isOperatorClosePlatform(platform) {
		return nil, fmt.Sprintf("Refused: platform %q is not %s or %s. This command clears only operator-required circuit-breaker entries; every other pending close has an automatic close owner.",
			platform, PlatformPendingCloseOKXSpot, PlatformPendingCloseRobinhoodOptions)
	}
	if state == nil || mu == nil {
		return nil, "status server not ready"
	}
	mu.RLock()
	defer mu.RUnlock()
	ss := state.Strategies[strategyID]
	if ss == nil {
		return nil, fmt.Sprintf("Refused: strategy `%s` is not in the running state. Nothing was cleared.", strategyID)
	}
	var candidates []string
	for _, key := range []string{PlatformPendingCloseOKXSpot, PlatformPendingCloseRobinhoodOptions} {
		if platform != "" && key != platform {
			continue
		}
		if p := ss.RiskState.getPendingCircuitClose(key); p != nil && p.OperatorRequired {
			candidates = append(candidates, key)
		}
	}
	if len(candidates) == 0 {
		where := ""
		if platform != "" {
			where = " under " + platform
		}
		msg := fmt.Sprintf("Refused: strategy `%s` has no operator-required circuit-breaker entry%s. Nothing was cleared.", strategyID, where)
		var others []string
		for key, p := range ss.RiskState.PendingCircuitCloses {
			if p == nil || (isOperatorClosePlatform(key) && p.OperatorRequired) {
				continue
			}
			others = append(others, key)
		}
		sort.Strings(others)
		if len(others) > 0 {
			msg += fmt.Sprintf(" Other pending closes (%s) have automatic close owners; this command never clears them.", strings.Join(others, ", "))
		}
		return nil, msg
	}
	if len(candidates) > 1 {
		return nil, fmt.Sprintf("Refused: strategy `%s` holds operator-required entries under %s. Run the command again with the platform option. Nothing was cleared.",
			strategyID, strings.Join(candidates, " and "))
	}
	key := candidates[0]
	p := ss.RiskState.getPendingCircuitClose(key)
	ack := &operatorCloseAck{
		StrategyID: strategyID,
		Platform:   key,
		Episode:    p.Episode,
		FiredAt:    p.FiredAt,
		Legs:       sortedOperatorRequiredLegs(p.Symbols),
	}
	return ack, formatOperatorCloseAckPrompt(*ack)
}

func formatOperatorCloseAckPrompt(ack operatorCloseAck) string {
	var b strings.Builder
	b.WriteString("**Clear an operator-required circuit-breaker close?**\n")
	fmt.Fprintf(&b, "• Strategy: `%s`\n", ack.StrategyID)
	fmt.Fprintf(&b, "• Platform: %s (`%s`)\n", operatorRequiredPlatformLabel(ack.Platform), ack.Platform)
	fmt.Fprintf(&b, "• Episode: %s\n", operatorCloseEpisodeLabel(ack.Episode))
	fmt.Fprintf(&b, "• Last fire: %s\n", operatorCloseFiredLabel(ack.FiredAt))
	fmt.Fprintf(&b, "• Legs: %s\n", formatOperatorRequiredLegs(ack.Legs))
	b.WriteString("This command does NOT read the venue. Flatten these positions on the venue and check the venue first. The sizes are the virtual book at fire time, summed across the fires not cleared, and do not show the venue position.\n")
	b.WriteString("Confirming deletes this record and stops its reminder. It sends no order and changes no cash, trade, position or circuit-breaker state.")
	return b.String()
}

func applyOperatorRequiredCloseAck(state *AppState, mu *sync.RWMutex, store *StateStore, operatorID string, ack operatorCloseAck) string {
	if strings.TrimSpace(operatorID) == "" {
		return "Refused: no operator id. Nothing was cleared."
	}
	if state == nil || mu == nil || store == nil {
		return "Refused: the state store is not ready. Nothing was cleared."
	}
	mu.Lock()
	defer mu.Unlock()
	ss := state.Strategies[ack.StrategyID]
	var cur *PendingCircuitClose
	if ss != nil {
		cur = ss.RiskState.getPendingCircuitClose(ack.Platform)
	}
	if cur == nil || !cur.OperatorRequired || cur.Episode != ack.Episode || !cur.FiredAt.Equal(ack.FiredAt) ||
		!operatorCloseLegsEqual(sortedOperatorRequiredLegs(cur.Symbols), ack.Legs) {
		return fmt.Sprintf("Refused: the operator-required entry for `%s` [%s] changed while you were confirming (a new circuit-breaker fire, a kill-switch fire or another clear). Nothing was cleared. Run the command again to see the current entry.",
			ack.StrategyID, ack.Platform)
	}
	prev := cur
	ss.RiskState.clearPendingCircuitClose(ack.Platform)
	if err := store.SaveStrategyBook(ss); err != nil {
		if ss.RiskState.PendingCircuitCloses == nil {
			ss.RiskState.PendingCircuitCloses = make(map[string]*PendingCircuitClose)
		}
		ss.RiskState.PendingCircuitCloses[ack.Platform] = prev
		fmt.Printf("[CRITICAL] operator-required-close: strategy %s platform %s: saving the operator clear failed: %v; the entry is restored\n", ack.StrategyID, ack.Platform, err)
		return fmt.Sprintf("Refused: saving the clear failed (%v). The entry is restored and its reminder continues. A retry is safe.", err)
	}
	legs := formatOperatorRequiredLegs(ack.Legs)
	fmt.Printf("[CRITICAL] operator-required-close: strategy %s platform %s cleared by operator %s (episode %s; legs %s); the venue was not read\n",
		ack.StrategyID, ack.Platform, operatorID, operatorCloseEpisodeLabel(ack.Episode), legs)
	return fmt.Sprintf("Cleared the operator-required entry for `%s` [%s] (episode %s; legs %s). Its reminder stops. The venue was not read, and no order, cash, trade, position or circuit-breaker state changed.",
		ack.StrategyID, ack.Platform, operatorCloseEpisodeLabel(ack.Episode), legs)
}

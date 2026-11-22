package main

import (
	"fmt"
	"strings"
	"time"
)

type killSwitchLimitOrderCandidate struct {
	Row    PendingLimitOrder
	Script string

	AdoptionBlock string
}

func (c killSwitchLimitOrderCandidate) adoptionEligible() bool {
	return c.AdoptionBlock == ""
}

type killSwitchLimitOrderDeps struct {
	Cancel              func(script, symbol string, oid int64) (*HyperliquidCancelOrderResult, string, error)
	Status              func(script, symbol string, oids []int64, sinceMs int64) (*HyperliquidLimitStatusResult, string, error)
	Delete              func(o PendingLimitOrder) error
	MarkCancelRequested func(strategyID, symbol string) (int64, error)
	Now                 func() time.Time
}

func (d killSwitchLimitOrderDeps) wired() bool {
	return d.Cancel != nil && d.Status != nil && d.Delete != nil && d.MarkCancelRequested != nil
}

func (d killSwitchLimitOrderDeps) now() time.Time {
	if d.Now != nil {
		return d.Now()
	}
	return time.Now()
}

const killSwitchLimitOrderCallReserve = scriptTimeout

func killSwitchLimitOrderBudget(d time.Duration) time.Duration {
	if floor := 2 * killSwitchLimitOrderCallReserve; d < floor {
		return floor
	}
	return d
}

type killSwitchLimitOrderReport struct {
	Cancelled  []string
	Unresolved []string
	LogLines   []string
}

func (r killSwitchLimitOrderReport) ConfirmedClear() bool {
	return len(r.Unresolved) == 0
}

func killSwitchLimitOrderLabel(o PendingLimitOrder) string {
	return fmt.Sprintf("%s/%s oid=%d", o.StrategyID, o.Symbol, o.OrderOID)
}

func killSwitchLimitOrderRoster(cfgs []StrategyConfig) []StrategyConfig {
	var out []StrategyConfig
	for _, sc := range cfgs {
		if sc.Platform != "hyperliquid" {
			continue
		}
		out = append(out, sc)
	}
	return out
}

func killSwitchLimitOrderAdoptionBlock(sc StrategyConfig, known bool) string {
	switch {
	case !known:
		return "the strategy is absent from this config"
	case sc.Type != "manual":
		return fmt.Sprintf("the strategy type is %q, not \"manual\"", sc.Type)
	case !hyperliquidIsLive(sc.Args):
		return "the strategy is not Hyperliquid-live"
	}
	return ""
}

func collectKillSwitchLimitOrderCandidates(orders []PendingLimitOrder, roster []StrategyConfig) ([]killSwitchLimitOrderCandidate, []PendingLimitOrder) {
	byID := make(map[string]StrategyConfig, len(roster))
	fallbackScript := ""
	for _, sc := range roster {
		byID[sc.ID] = sc
		if fallbackScript == "" {
			fallbackScript = strings.TrimSpace(sc.Script)
		}
	}
	var candidates []killSwitchLimitOrderCandidate
	var unscripted []PendingLimitOrder
	for _, o := range orders {
		sc, known := byID[o.StrategyID]
		script := strings.TrimSpace(sc.Script)
		if script == "" {
			script = fallbackScript
		}
		if script == "" {
			unscripted = append(unscripted, o)
			continue
		}
		candidates = append(candidates, killSwitchLimitOrderCandidate{
			Row:           o,
			Script:        script,
			AdoptionBlock: limitRowOwnerBlock(sc, known, o),
		})
	}
	return candidates, unscripted
}

func cancelKillSwitchRestingLimitOrders(loader func() ([]PendingLimitOrder, error), roster []StrategyConfig, deps killSwitchLimitOrderDeps, budget time.Duration) killSwitchLimitOrderReport {
	var report killSwitchLimitOrderReport
	if loader == nil {
		return report
	}

	orders, err := loader()
	if err != nil {
		report.Unresolved = append(report.Unresolved,
			fmt.Sprintf("resting limit order queue unreadable (%v)", err))
		report.LogLines = append(report.LogLines,
			fmt.Sprintf("[CRITICAL] ks-limit: could not load pending limit orders: %v — cannot confirm no resting order survives the flatten (kill switch will retry next cycle)", err))
		return report
	}
	if len(orders) == 0 {
		return report
	}

	candidates, unscripted := collectKillSwitchLimitOrderCandidates(orders, roster)

	for _, o := range unscripted {
		label := killSwitchLimitOrderLabel(o)
		report.Unresolved = append(report.Unresolved,
			fmt.Sprintf("%s: no Hyperliquid strategy with a script remains in this config to cancel it — manual intervention required", label))
		report.LogLines = append(report.LogLines,
			fmt.Sprintf("[CRITICAL] ks-limit: resting limit order %s cannot be cancelled — no Hyperliquid strategy with a script remains in this config, manual intervention required (kill switch will retry next cycle)", label))
	}

	if len(candidates) > 0 && !deps.wired() {
		for _, c := range candidates {
			label := killSwitchLimitOrderLabel(c.Row)
			report.Unresolved = append(report.Unresolved,
				fmt.Sprintf("%s: order canceller unwired", label))
			report.LogLines = append(report.LogLines,
				fmt.Sprintf("[CRITICAL] ks-limit: resting limit order %s could not be cancelled — canceller unwired (kill switch will retry next cycle)", label))
		}
		return report
	}

	effectiveBudget := killSwitchLimitOrderBudget(budget)
	deadline := deps.now().Add(effectiveBudget)
	roomForCall := func() bool {
		return !deps.now().Add(killSwitchLimitOrderCallReserve).After(deadline)
	}

	for i, c := range candidates {
		o := c.Row
		label := killSwitchLimitOrderLabel(o)

		var markErr error
		unresolve := func(reason string) {
			if markErr != nil {
				reason = fmt.Sprintf("%s; cancel intent could not be persisted (%v), so a latch reset would abandon this cancellation", reason, markErr)
			}
			report.Unresolved = append(report.Unresolved, fmt.Sprintf("%s: %s", label, reason))
			report.LogLines = append(report.LogLines,
				fmt.Sprintf("[CRITICAL] ks-limit: resting limit order %s not confirmed cancelled: %s (kill switch will retry next cycle)", label, reason))
		}

		if !roomForCall() {
			for _, rest := range candidates[i:] {
				restLabel := killSwitchLimitOrderLabel(rest.Row)
				reason := fmt.Sprintf("cancel pass exhausted its %s budget before reaching this order — the flatten proceeded so live exposure is not held up; the kill switch will retry next cycle", effectiveBudget)
				report.Unresolved = append(report.Unresolved, fmt.Sprintf("%s: %s", restLabel, reason))
				report.LogLines = append(report.LogLines,
					fmt.Sprintf("[CRITICAL] ks-limit: resting limit order %s not reached: %s", restLabel, reason))
			}
			break
		}

		if _, err := deps.MarkCancelRequested(o.StrategyID, o.Symbol); err != nil {
			markErr = err
			report.LogLines = append(report.LogLines,
				fmt.Sprintf("[WARN] ks-limit: %s could not persist cancel intent: %v", label, err))
		}

		cancelRes, cstderr, cerr := deps.Cancel(c.Script, o.Symbol, o.OrderOID)
		if cstderr != "" {
			report.LogLines = append(report.LogLines,
				fmt.Sprintf("[WARN] ks-limit: %s cancel stderr: %s", label, cstderr))
		}
		if cerr != nil || cancelRes == nil || cancelRes.Error != "" {
			msg := ""
			if cancelRes != nil {
				msg = cancelRes.Error
			}
			unresolve(strings.TrimSpace(fmt.Sprintf("cancel failed: %v %s", cerr, msg)))
			continue
		}

		if !roomForCall() {
			unresolve(fmt.Sprintf("cancel issued but the pass exhausted its %s budget before it could be verified — the flatten proceeded; the kill switch will retry next cycle", effectiveBudget))
			continue
		}

		statusRes, sstderr, serr := deps.Status(c.Script, o.Symbol, []int64{o.OrderOID}, limitStatusSinceMs(o.CreatedAt))
		if sstderr != "" {
			report.LogLines = append(report.LogLines,
				fmt.Sprintf("[WARN] ks-limit: %s status stderr: %s", label, sstderr))
		}
		proof := decideLimitRowProof(o, statusRes, serr)
		if proof.Live {
			unresolve("order is still resting on the exchange")
			continue
		}
		unprovenWatermark := limitWatermarkUnproven(o)
		if unprovenWatermark != "" && proof.ProvenAboveBooked() {
			unresolve(fmt.Sprintf("order executed %.6f on the exchange but %s, so the scheduler states no booked size for this order and NO automatic path books or clears the row — queue row kept as the recovery record for an operator; compare the %s position on Hyperliquid with the scheduler's book, flatten only size the book does not hold, then clear the row with `go-trader manual-clear-limit-row %d --flattened` once the scheduler has marked it; the flatten proceeded and automatic reset waits until the row is cleared",
				proof.Executed, unprovenWatermark, o.Symbol, o.OrderOID))
			continue
		}
		if proof.ProvenAboveBooked() {
			if !o.OperatorRequiredSince.IsZero() && o.OperatorEvidenceJSON != "" {
				unresolve(fmt.Sprintf("order executed %.6f where the book holds %.6f (unadopted fill) and the scheduler marked the row as needing an operator — follow the owner alert for this order (flatten only size the scheduler's book does not hold), then clear the row with `go-trader manual-clear-limit-row %d --flattened`",
					proof.Executed, o.FilledSize, o.OrderOID))
			} else if c.adoptionEligible() {
				unresolve(fmt.Sprintf("order executed %.6f where the book holds %.6f (unadopted fill) — queue row kept so the scheduler books the fill before the next flatten",
					proof.Executed, o.FilledSize))
			} else {
				unresolve(fmt.Sprintf("order executed %.6f where the book holds %.6f (unadopted fill) that NO automatic path can book because %s — restore it to this config as a Hyperliquid-live type=manual strategy so the scheduler adopts the fill; manual intervention required",
					proof.Executed, o.FilledSize, c.AdoptionBlock))
			}
			continue
		}
		if !proof.OffBook || !proof.CanFinalize() {
			unresolve("cancel unverified: " + limitOffBookUnverified(proof))
			continue
		}
		if unprovenWatermark != "" {
			unresolve("order is off-book but " + unprovenWatermark + " — queue row kept as the recovery record for an operator; the flatten proceeded and automatic reset waits until the row is cleared")
			continue
		}
		if !c.adoptionEligible() && o.FilledSize > limitFillEpsilon {
			unresolve(fmt.Sprintf("order is off-book but its owner cannot carry the %.6f this order booked because %s — queue row kept for review; the flatten proceeded and automatic reset waits until the row is cleared", o.FilledSize, c.AdoptionBlock))
			continue
		}
		if err := deps.Delete(o); err != nil {
			unresolve(fmt.Sprintf("order is off-book but the queue row could not be cleared (%v)", err))
			continue
		}

		report.Cancelled = append(report.Cancelled, label)
		report.LogLines = append(report.LogLines,
			fmt.Sprintf("[CRITICAL] ks-limit: cancelled resting limit order %s (%s %.6f @ $%.4f)", label, o.Side, o.OrderSize, o.LimitPrice))
	}

	return report
}

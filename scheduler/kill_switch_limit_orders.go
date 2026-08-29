package main

import (
	"fmt"
	"strings"
)

type killSwitchLimitOrderCandidate struct {
	Row    PendingLimitOrder
	Script string
}

type killSwitchLimitOrderDeps struct {
	Cancel func(script, symbol string, oid int64) (*HyperliquidCancelOrderResult, string, error)
	Status func(script, symbol string, oids []int64, sinceMs int64) (*HyperliquidLimitStatusResult, string, error)
	Delete func(id int64) error
}

func (d killSwitchLimitOrderDeps) wired() bool {
	return d.Cancel != nil && d.Status != nil && d.Delete != nil
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
		if sc.Platform != "hyperliquid" || strings.TrimSpace(sc.Script) == "" {
			continue
		}
		out = append(out, sc)
	}
	return out
}

func collectKillSwitchLimitOrderCandidates(orders []PendingLimitOrder, roster []StrategyConfig) ([]killSwitchLimitOrderCandidate, []PendingLimitOrder) {
	scripts := make(map[string]string, len(roster))
	for _, sc := range roster {
		if strings.TrimSpace(sc.Script) == "" {
			continue
		}
		scripts[sc.ID] = sc.Script
	}
	var candidates []killSwitchLimitOrderCandidate
	var unconfigured []PendingLimitOrder
	for _, o := range orders {
		script, ok := scripts[o.StrategyID]
		if !ok {
			unconfigured = append(unconfigured, o)
			continue
		}
		candidates = append(candidates, killSwitchLimitOrderCandidate{Row: o, Script: script})
	}
	return candidates, unconfigured
}

func cancelKillSwitchRestingLimitOrders(loader func() ([]PendingLimitOrder, error), roster []StrategyConfig, deps killSwitchLimitOrderDeps) killSwitchLimitOrderReport {
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

	candidates, unconfigured := collectKillSwitchLimitOrderCandidates(orders, roster)

	for _, o := range unconfigured {
		label := killSwitchLimitOrderLabel(o)
		report.Unresolved = append(report.Unresolved,
			fmt.Sprintf("%s: strategy is not a live Hyperliquid strategy in this config — manual intervention required", label))
		report.LogLines = append(report.LogLines,
			fmt.Sprintf("[CRITICAL] ks-limit: resting limit order %s belongs to no live Hyperliquid strategy — cannot cancel, manual intervention required (kill switch will retry next cycle)", label))
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

	for _, c := range candidates {
		o := c.Row
		label := killSwitchLimitOrderLabel(o)

		unresolve := func(reason string) {
			report.Unresolved = append(report.Unresolved, fmt.Sprintf("%s: %s", label, reason))
			report.LogLines = append(report.LogLines,
				fmt.Sprintf("[CRITICAL] ks-limit: resting limit order %s not confirmed cancelled: %s (kill switch will retry next cycle)", label, reason))
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

		statusRes, sstderr, serr := deps.Status(c.Script, o.Symbol, []int64{o.OrderOID}, limitStatusSinceMs(o.CreatedAt))
		if sstderr != "" {
			report.LogLines = append(report.LogLines,
				fmt.Sprintf("[WARN] ks-limit: %s status stderr: %s", label, sstderr))
		}
		if serr != nil || statusRes == nil || statusRes.Error != "" {
			msg := ""
			if statusRes != nil {
				msg = statusRes.Error
			}
			unresolve(strings.TrimSpace(fmt.Sprintf("cancel unverified: %v %s", serr, msg)))
			continue
		}
		if statusRes.OpenOrdersError != "" {
			unresolve(fmt.Sprintf("cancel unverified: open-orders state unknown (%s)", statusRes.OpenOrdersError))
			continue
		}
		st, ok := limitStatusForOID(statusRes, o.OrderOID)
		if !ok {
			unresolve("cancel unverified: status response did not include the order")
			continue
		}
		if st.FillsError != "" {
			unresolve(fmt.Sprintf("cancel unverified: fills unreadable (%s)", st.FillsError))
			continue
		}
		if st.Resting == nil || *st.Resting {
			unresolve("order is still resting on the exchange")
			continue
		}
		if st.FilledSize > o.FilledSize+limitFillEpsilon {
			unresolve(fmt.Sprintf("order is off-book with an unadopted fill (tracked %.6f, exchange %.6f) — queue row kept so the scheduler books the fill before the next flatten",
				o.FilledSize, st.FilledSize))
			continue
		}
		if err := deps.Delete(o.ID); err != nil {
			unresolve(fmt.Sprintf("order is off-book but the queue row could not be cleared (%v)", err))
			continue
		}

		report.Cancelled = append(report.Cancelled, label)
		report.LogLines = append(report.LogLines,
			fmt.Sprintf("[CRITICAL] ks-limit: cancelled resting limit order %s (%s %.6f @ $%.4f)", label, o.Side, o.OrderSize, o.LimitPrice))
	}

	return report
}

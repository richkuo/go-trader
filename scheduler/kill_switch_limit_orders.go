package main

import (
	"fmt"
	"strings"
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
			AdoptionBlock: killSwitchLimitOrderAdoptionBlock(sc, known),
		})
	}
	return candidates, unscripted
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
			if c.adoptionEligible() {
				unresolve(fmt.Sprintf("order is off-book with an unadopted fill (tracked %.6f, exchange %.6f) — queue row kept so the scheduler books the fill before the next flatten",
					o.FilledSize, st.FilledSize))
			} else {
				unresolve(fmt.Sprintf("order is off-book with an unadopted fill (tracked %.6f, exchange %.6f) that NO automatic path can book because %s — restore it to this config as a Hyperliquid-live type=manual strategy so the scheduler adopts the fill; manual intervention required",
					o.FilledSize, st.FilledSize, c.AdoptionBlock))
			}
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

package main

import (
	"strings"
)

var productionRefusedStrategies = map[string]string{
	cumulativeVolumeDeltaStrategyName: "backtest/candidates/cumulative_volume_delta_divergence_1638/REPORT.md",
	"macro_event_breakout":            "backtest/candidates/macro_event_breakout_1654/REPORT.md",
	bitcoinLeadLagStrategyName:        "backtest/candidates/bitcoin_lead_lag_1640/REPORT.md",
	volumeProfileReclaimStrategyName:  "backtest/candidates/volume_profile_reclaim_1665/REPORT.md",
	"perpetual_premium_reversion":     "backtest/candidates/perpetual_premium_reversion_1639/REPORT.md",
	"order_book_imbalance":            "backtest/candidates/order_book_imbalance_1635/REPORT.md",
	"inventory_market_making":         "backtest/candidates/inventory_market_making_1636/REPORT.md",
}

func productionRefusedReferences(sc StrategyConfig) []edgeReference {
	if sc.Type == "options" {
		return nil
	}
	candidates := []edgeReference{
		{Role: edgeRefRoleOpen, Name: strategyNameFromArgs(sc.Args)},
		{Role: edgeRefRoleOpen, Name: effectiveOpenStrategy(sc)},
	}
	for _, ref := range sc.closeRefs() {
		candidates = append(candidates, edgeReference{Role: edgeRefRoleClose, Name: canonicalCloseStrategyName(ref.Name)})
	}
	var out []edgeReference
	seen := map[edgeReference]bool{}
	for _, ref := range candidates {
		ref.Name = strings.TrimSpace(ref.Name)
		if ref.Name == "" || seen[ref] {
			continue
		}
		seen[ref] = true
		if _, refused := productionRefusedStrategies[ref.Name]; refused {
			out = append(out, ref)
		}
	}
	return out
}

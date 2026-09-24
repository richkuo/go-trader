package main

import (
	"strings"
	"sync"
	"testing"
)

func withStubbedSyncHyperliquidProtection(
	t *testing.T,
	stub func(sc StrategyConfig, plan hlProtectionPlan, notifier *MultiNotifier, logger *StrategyLogger, reconcileFillHintsJSON []byte) (*HyperliquidProtectionSyncResult, bool),
) {
	t.Helper()
	orig := syncHyperliquidProtection
	syncHyperliquidProtection = stub
	t.Cleanup(func() { syncHyperliquidProtection = orig })
}

func TestHLProtectionSyncUnknownTPPlacementAlertLane(t *testing.T) {
	for _, tc := range []struct {
		name       string
		guard      hlProtectionGuardMode
		wantAlerts int
	}{
		{name: "ordinary protection sync sends the unresolved placement alert", guard: hlProtectionGuardFull, wantAlerts: 1},
		{name: "close re-arm leaves the unresolved placement to its one re-arm alert", guard: hlProtectionGuardStopLegAfterFailedClose, wantAlerts: 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sc := StrategyConfig{ID: "manual-eth", Type: "manual", Platform: "hyperliquid", CloseStrategy: tieredTPCloseStrategy()}
			withStubbedSyncHyperliquidProtection(t, func(_ StrategyConfig, _ hlProtectionPlan, _ *MultiNotifier, _ *StrategyLogger, _ []byte) (*HyperliquidProtectionSyncResult, bool) {
				return &HyperliquidProtectionSyncResult{TPOIDs: []int64{0, 802, 803}, TPErrors: []string{"read timeout", "", ""}, TPOutcomeUnknown: []bool{true, false, false}}, true
			})
			pos := &Position{Symbol: "ETH", Quantity: 1, AvgCost: 2000, EntryATR: 50, Side: "long"}
			state := &StrategyState{ID: sc.ID, Positions: map[string]*Position{"ETH": pos}}
			notifier, backend := confirmationNotifier()
			var mu sync.RWMutex
			runHyperliquidProtectionSync(sc, state, nil, "ETH", &mu, notifier, nil, "test", nil, nil, nil, tc.guard)
			if pos.TPOIDs[0] != 0 || !pos.TPArmedTiers[0] {
				t.Fatalf("the unresolved tier is replaceable: oids=%v armed=%v", pos.TPOIDs, pos.TPArmedTiers)
			}
			backend.mu.Lock()
			defer backend.mu.Unlock()
			alerts := 0
			for _, m := range backend.messages {
				if strings.Contains(m.content, "never resolved the placement of take-profit tier(s) [1]") {
					alerts++
				}
			}
			if alerts != tc.wantAlerts {
				t.Fatalf("unresolved placement alerts = %d, want %d: %+v", alerts, tc.wantAlerts, backend.messages)
			}
		})
	}
}

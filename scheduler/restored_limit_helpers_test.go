package main

import (
	"path/filepath"
	"testing"
)

func limitTestBoolPtr(b bool) *bool { return &b }

func newLimitTestStateDB(t *testing.T) *StateDB {
	t.Helper()
	db, err := OpenStateDB(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func newLimitTestStrategy() (StrategyConfig, *AppState) {
	sc := StrategyConfig{
		ID:       "hl-manual-eth-live",
		Type:     "manual",
		Platform: "hyperliquid",
		Symbol:   "ETH",
		Script:   "shared_scripts/check_hyperliquid.py",
		Leverage: 10,
		Args:     []string{"hold", "ETH", "30m", "--mode=live"},
	}
	state := &AppState{
		Strategies: map[string]*StrategyState{
			sc.ID: {
				ID:        sc.ID,
				Platform:  "hyperliquid",
				Type:      "manual",
				Positions: map[string]*Position{},
				Cash:      10000,
			},
		},
	}
	return sc, state
}

func withStubbedHLLiveExposure(t *testing.T, positions ...HLPosition) {
	t.Helper()
	t.Setenv("HYPERLIQUID_ACCOUNT_ADDRESS", "0xlimittest")
	orig := fetchHyperliquidStateFn
	snapshot := append([]HLPosition(nil), positions...)
	fetchHyperliquidStateFn = func(string) (float64, []HLPosition, error) {
		return 0, snapshot, nil
	}
	limitFillExposureAlerts.reset()
	t.Cleanup(func() { fetchHyperliquidStateFn = orig })
}

func withStubbedLimitDeps(t *testing.T, status func(script, symbol string, oids []int64, sinceMs int64) (*HyperliquidLimitStatusResult, string, error), cancel func(script, symbol string, oid int64) (*HyperliquidCancelOrderResult, string, error)) {
	t.Helper()
	origStatus := runHyperliquidLimitStatusFn
	origCancel := runHyperliquidCancelOrderFn
	origSync := syncHyperliquidProtection
	origRecorder := tradeRecorder
	runHyperliquidLimitStatusFn = status
	runHyperliquidCancelOrderFn = cancel
	syncHyperliquidProtection = func(StrategyConfig, hlProtectionPlan, *MultiNotifier, *StrategyLogger, []byte) (*HyperliquidProtectionSyncResult, bool) {
		return &HyperliquidProtectionSyncResult{}, true
	}
	tradeRecorder = func(string, Trade) error { return nil }
	t.Cleanup(func() {
		runHyperliquidLimitStatusFn = origStatus
		runHyperliquidCancelOrderFn = origCancel
		syncHyperliquidProtection = origSync
		tradeRecorder = origRecorder
	})
}

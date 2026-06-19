package main

import (
	"strings"
	"testing"
	"time"
)

// TestManualSLAutoManaged verifies the guard that blocks manual SL edits on
// strategies whose automated protection would re-pin/re-arm the stop-loss on
// the next cycle (#1050). A manual edit is only coherent when the strategy has
// opted out of auto-SL.
func TestManualSLAutoManaged(t *testing.T) {
	basePos := func() *Position {
		return &Position{
			Symbol:   "ETH",
			Quantity: 1,
			AvgCost:  2000,
			EntryATR: 50,
			Side:     "long",
		}
	}
	slMult := 1.5
	trailMult := 2.0
	regimeBlock := func() *RegimeATRBlock {
		return &RegimeATRBlock{TrendRegime: map[string]RegimeATREntry{
			"trending": {ATR: 2.0},
			"ranging":  {ATR: 1.0},
		}}
	}
	regimePos := func(label string) *Position {
		return &Position{Symbol: "ETH", Quantity: 1, AvgCost: 2000, EntryATR: 50, Side: "long", Regime: label}
	}

	cases := []struct {
		name       string
		sc         StrategyConfig
		pos        *Position
		wantManage bool
	}{
		{
			name:       "ATR stop armed reverts manual edit",
			sc:         StrategyConfig{ID: "x", Type: "manual", Platform: "hyperliquid", Symbol: "ETH", StopLossATRMult: &slMult},
			pos:        basePos(),
			wantManage: true,
		},
		{
			name:       "trailing stop manages SL",
			sc:         StrategyConfig{ID: "x", Type: "manual", Platform: "hyperliquid", Symbol: "ETH", CloseStrategy: &StrategyRef{Name: "trailing_tp_ratchet"}, TrailingStopATRMult: &trailMult},
			pos:        basePos(),
			wantManage: true,
		},
		{
			name:       "no auto stop allows manual edit",
			sc:         StrategyConfig{ID: "x", Type: "manual", Platform: "hyperliquid", Symbol: "ETH"},
			pos:        basePos(),
			wantManage: false,
		},
		{
			name: "missing EntryATR cannot auto-arm so manual edit allowed",
			sc:   StrategyConfig{ID: "x", Type: "manual", Platform: "hyperliquid", Symbol: "ETH", StopLossATRMult: &slMult},
			pos: &Position{
				Symbol:   "ETH",
				Quantity: 1,
				AvgCost:  2000,
				EntryATR: 0, // no ATR -> plan returns false -> sync cannot recompute an SL
				Side:     "long",
			},
			wantManage: false,
		},
		{
			// #1052 review: a regime SL whose label is transiently the #879
			// fail-open "-" resolves to a zero multiplier in the value pass, so
			// the config-presence pass must still reject it — a later resolution
			// + force-SL-replace cycle would re-pin the operator's trigger.
			name:       "regime stop-loss with unresolved label rejected by config presence",
			sc:         StrategyConfig{ID: "x", Type: "manual", Platform: "hyperliquid", Symbol: "ETH", StopLossATRRegime: regimeBlock()},
			pos:        regimePos("-"),
			wantManage: true,
		},
		{
			name:       "regime stop-loss with resolved label still rejected (no regression)",
			sc:         StrategyConfig{ID: "x", Type: "manual", Platform: "hyperliquid", Symbol: "ETH", StopLossATRRegime: regimeBlock()},
			pos:        regimePos("trending"),
			wantManage: true,
		},
		{
			name:       "regime trailing stop with unresolved label rejected by config presence",
			sc:         StrategyConfig{ID: "x", Type: "manual", Platform: "hyperliquid", Symbol: "ETH", TrailingStopATRRegime: regimeBlock()},
			pos:        regimePos("-"),
			wantManage: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			managed, reason := manualSLAutoManaged(tc.sc, tc.pos)
			if managed != tc.wantManage {
				t.Fatalf("manualSLAutoManaged = %v (%q), want %v", managed, reason, tc.wantManage)
			}
			if managed && reason == "" {
				t.Fatal("expected a non-empty reason when auto-managed")
			}
		})
	}
}

func TestSLTriggerWouldFillImmediately(t *testing.T) {
	cases := []struct {
		name    string
		side    string
		trigger float64
		mark    float64
		want    bool
	}{
		{"long trigger below mark is safe", "long", 1900, 2000, false},
		{"long trigger at mark fills", "long", 2000, 2000, true},
		{"long trigger above mark fills", "long", 2100, 2000, true},
		{"short trigger above mark is safe", "short", 2100, 2000, false},
		{"short trigger at mark fills", "short", 2000, 2000, true},
		{"short trigger below mark fills", "short", 1900, 2000, true},
		{"unknown mark never blocks", "long", 2100, 0, false},
		{"zero trigger never blocks", "long", 0, 2000, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := slTriggerWouldFillImmediately(tc.side, tc.trigger, tc.mark); got != tc.want {
				t.Fatalf("slTriggerWouldFillImmediately(%q,%g,%g) = %v, want %v", tc.side, tc.trigger, tc.mark, got, tc.want)
			}
		})
	}
}

// TestSLPlacementFailureLeftNaked verifies a no-OID placement failure is
// classified naked only when the old stop-loss was actually removed (#1052).
func TestSLPlacementFailureLeftNaked(t *testing.T) {
	cases := []struct {
		name            string
		cancelSucceeded bool
		oldOID          int64
		wantNaked       bool
	}{
		{"cancel succeeded then place failed = naked", true, 1001, true},
		{"no prior stop-loss then place failed = naked", false, 0, true},
		{"cancel failed so old SL still resting = safe", false, 1001, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := slPlacementFailureLeftNaked(tc.cancelSucceeded, tc.oldOID); got != tc.wantNaked {
				t.Fatalf("slPlacementFailureLeftNaked(%v,%d) = %v, want %v", tc.cancelSucceeded, tc.oldOID, got, tc.wantNaked)
			}
		})
	}
}

// TestPendingSLActionExists verifies the same-cycle orphan guard: a second SL
// edit is detected only when an un-drained update-sl/cancel-sl action for the
// SAME strategy+symbol is already queued (#1052).
func TestPendingSLActionExists(t *testing.T) {
	db, err := OpenStateDB(":memory:")
	if err != nil {
		t.Fatalf("OpenStateDB: %v", err)
	}
	defer db.Close()
	now := time.Now().UTC()

	// Unrelated queued actions must NOT trip the guard.
	if err := db.InsertPendingManualAction(PendingManualAction{StrategyID: "hl-eth", Action: "open", Symbol: "ETH", Side: "long", Quantity: 1, FillPrice: 2000, CreatedAt: now}); err != nil {
		t.Fatalf("insert open: %v", err)
	}
	if err := db.InsertPendingManualAction(PendingManualAction{StrategyID: "hl-btc", Action: "update-sl", Symbol: "BTC", Side: "long", Quantity: 1, StopLossOID: 7, CreatedAt: now}); err != nil {
		t.Fatalf("insert other-strategy update-sl: %v", err)
	}

	if pending, err := pendingSLActionExists(db, "hl-eth", "ETH"); err != nil || pending {
		t.Fatalf("expected no pending SL action for hl-eth/ETH (open + other-strategy only), got pending=%v err=%v", pending, err)
	}

	// A queued update-sl for the same strategy+symbol trips it (case-insensitive).
	if err := db.InsertPendingManualAction(PendingManualAction{StrategyID: "hl-eth", Action: "update-sl", Symbol: "ETH", Side: "long", Quantity: 1, StopLossOID: 9, StopLossTriggerPx: 1950, CreatedAt: now}); err != nil {
		t.Fatalf("insert same update-sl: %v", err)
	}
	if pending, err := pendingSLActionExists(db, "hl-eth", "eth"); err != nil || !pending {
		t.Fatalf("expected pending SL action for hl-eth/eth, got pending=%v err=%v", pending, err)
	}
}

func TestManualActionRecordsTrade(t *testing.T) {
	records := map[string]bool{"open": true, "close": true, "add": true}
	noRecords := []string{"update-sl", "cancel-sl"}
	for action, want := range records {
		if got := manualActionRecordsTrade(action); got != want {
			t.Errorf("manualActionRecordsTrade(%q) = %v, want %v", action, got, want)
		}
	}
	for _, action := range noRecords {
		if manualActionRecordsTrade(action) {
			t.Errorf("manualActionRecordsTrade(%q) = true, want false (no trade for SL ops)", action)
		}
	}
}

func manualSLTestState(slOID int64, trigger float64) (*AppState, map[string]StrategyConfig) {
	state := &AppState{
		Strategies: map[string]*StrategyState{
			"hl-manual-eth-live": {
				ID:       "hl-manual-eth-live",
				Platform: "hyperliquid",
				Type:     "manual",
				Positions: map[string]*Position{
					"ETH": {
						Symbol:            "ETH",
						Quantity:          1,
						AvgCost:           2000,
						EntryATR:          50,
						Side:              "long",
						OwnerStrategyID:   "hl-manual-eth-live",
						StopLossOID:       slOID,
						StopLossTriggerPx: trigger,
					},
				},
				Cash: 10000,
			},
		},
	}
	scByID := map[string]StrategyConfig{
		"hl-manual-eth-live": {ID: "hl-manual-eth-live", Type: "manual", Platform: "hyperliquid", Symbol: "ETH", Leverage: 10},
	}
	return state, scByID
}

func TestApplyManualActionUpdateSL(t *testing.T) {
	state, scByID := manualSLTestState(1001, 1900)

	origRecorder := tradeRecorder
	tradeRecorder = func(_ string, _ Trade) error {
		t.Fatal("update-sl must not record a trade")
		return nil
	}
	defer func() { tradeRecorder = origRecorder }()

	err := applyManualAction(state, scByID, PendingManualAction{
		StrategyID:        "hl-manual-eth-live",
		Action:            "update-sl",
		Symbol:            "ETH",
		Side:              "long",
		Quantity:          1,
		StopLossOID:       1002,
		StopLossTriggerPx: 1950,
		CreatedAt:         time.Now().UTC(),
	})
	if err != nil {
		t.Fatalf("applyManualAction update-sl: %v", err)
	}
	pos := state.Strategies["hl-manual-eth-live"].Positions["ETH"]
	if pos.StopLossOID != 1002 {
		t.Errorf("pos.StopLossOID = %d, want 1002", pos.StopLossOID)
	}
	if pos.StopLossTriggerPx != 1950 {
		t.Errorf("pos.StopLossTriggerPx = %g, want 1950", pos.StopLossTriggerPx)
	}
	if pos.Quantity != 1 {
		t.Errorf("pos.Quantity = %g, want 1 (unchanged)", pos.Quantity)
	}
}

func TestApplyManualActionCancelSL(t *testing.T) {
	state, scByID := manualSLTestState(1001, 1900)

	err := applyManualAction(state, scByID, PendingManualAction{
		StrategyID: "hl-manual-eth-live",
		Action:     "cancel-sl",
		Symbol:     "ETH",
		CreatedAt:  time.Now().UTC(),
	})
	if err != nil {
		t.Fatalf("applyManualAction cancel-sl: %v", err)
	}
	pos := state.Strategies["hl-manual-eth-live"].Positions["ETH"]
	if pos.StopLossOID != 0 {
		t.Errorf("pos.StopLossOID = %d, want 0", pos.StopLossOID)
	}
	if pos.StopLossTriggerPx != 0 {
		t.Errorf("pos.StopLossTriggerPx = %g, want 0", pos.StopLossTriggerPx)
	}
}

func TestApplyManualActionUpdateSLRejectsOwnerMismatch(t *testing.T) {
	state, scByID := manualSLTestState(1001, 1900)
	state.Strategies["hl-manual-eth-live"].Positions["ETH"].OwnerStrategyID = "hl-other-eth-live"

	err := applyManualAction(state, scByID, PendingManualAction{
		StrategyID:        "hl-manual-eth-live",
		Action:            "update-sl",
		Symbol:            "ETH",
		StopLossOID:       1002,
		StopLossTriggerPx: 1950,
		CreatedAt:         time.Now().UTC(),
	})
	if err == nil || !strings.Contains(err.Error(), "owned by") {
		t.Fatalf("expected owner mismatch error, got: %v", err)
	}
	pos := state.Strategies["hl-manual-eth-live"].Positions["ETH"]
	if pos.StopLossOID != 1001 {
		t.Errorf("pos.StopLossOID = %d, want 1001 (untouched on owner mismatch)", pos.StopLossOID)
	}
}

func TestApplyManualActionSLNoPosition(t *testing.T) {
	state, scByID := manualSLTestState(1001, 1900)
	delete(state.Strategies["hl-manual-eth-live"].Positions, "ETH")

	for _, action := range []string{"update-sl", "cancel-sl"} {
		err := applyManualAction(state, scByID, PendingManualAction{
			StrategyID: "hl-manual-eth-live",
			Action:     action,
			Symbol:     "ETH",
			CreatedAt:  time.Now().UTC(),
		})
		if err == nil || !strings.Contains(err.Error(), "no open position") {
			t.Fatalf("%s: expected no-position error, got: %v", action, err)
		}
	}
}

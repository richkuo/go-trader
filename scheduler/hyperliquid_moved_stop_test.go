package main

import (
	"bytes"
	"math"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
)

func movedStopTestStrategy() StrategyConfig {
	return postTPSLTestStrategy("breakeven", []interface{}{
		map[string]interface{}{"atr_multiple": 1, "close_fraction": 0.5},
		map[string]interface{}{"atr_multiple": 2, "close_fraction": 1.0},
	})
}

func TestBuildHyperliquidProtectionPlanPreservesMovedStop(t *testing.T) {
	sc := movedStopTestStrategy()
	cases := []struct {
		name         string
		side         string
		moved        bool
		oid          int64
		trigger      float64
		preserved    float64
		liqPx        float64
		wantMult     bool
		wantPreserve bool
		wantTrigger  float64
		wantForce    bool
	}{
		{name: "unmoved keeps the label", side: "long", oid: 7, trigger: 1960, wantMult: true},
		{name: "moved long breakeven resting", side: "long", moved: true, oid: 7, trigger: 2000, wantMult: true, wantPreserve: true, wantTrigger: 2000},
		{name: "moved short breakeven resting", side: "short", moved: true, oid: 7, trigger: 2000, wantMult: true, wantPreserve: true, wantTrigger: 2000},
		{name: "moved long beyond breakeven", side: "long", moved: true, oid: 7, trigger: 2030, wantMult: true, wantPreserve: true, wantTrigger: 2030},
		{name: "moved short beyond breakeven", side: "short", moved: true, oid: 7, trigger: 1970, wantMult: true, wantPreserve: true, wantTrigger: 1970},
		{name: "moved confirmed missing uses the preserved trigger", side: "long", moved: true, preserved: 2000, wantMult: true, wantPreserve: true, wantTrigger: 2000},
		{name: "moved resting stop wins over an older preserved trigger", side: "long", moved: true, oid: 7, trigger: 2030, preserved: 2000, wantMult: true, wantPreserve: true, wantTrigger: 2030},
		{name: "moved unconfirmed outcome keeps no stop leg", side: "long", moved: true, trigger: 2000, preserved: 2000},
		{name: "moved trigger lost", side: "long", moved: true, wantMult: true, wantPreserve: true},
		{name: "moved non-finite preserved trigger", side: "long", moved: true, preserved: math.NaN(), wantMult: true, wantPreserve: true},
		{name: "moved negative preserved trigger", side: "long", moved: true, preserved: -5, wantMult: true, wantPreserve: true},
		{name: "moved long label tighter wins", side: "long", moved: true, preserved: 1900, wantMult: true, wantPreserve: true, wantTrigger: 1960},
		{name: "moved short label tighter wins", side: "short", moved: true, preserved: 2100, wantMult: true, wantPreserve: true, wantTrigger: 2040},
		{name: "moved long clamped inside liquidation", side: "long", moved: true, oid: 7, trigger: 2000, liqPx: 2005, wantMult: true, wantPreserve: true, wantTrigger: 2005 * (1 + hlLiquidationStopBufferPct/100), wantForce: true},
		{name: "moved short clamped inside liquidation", side: "short", moved: true, oid: 7, trigger: 2000, liqPx: 1995, wantMult: true, wantPreserve: true, wantTrigger: 1995 * (1 - hlLiquidationStopBufferPct/100), wantForce: true},
		{name: "moved long far from liquidation is not clamped", side: "long", moved: true, oid: 7, trigger: 2000, liqPx: 1500, wantMult: true, wantPreserve: true, wantTrigger: 2000},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			pos := &Position{
				Symbol: "ETH", Side: c.side, Quantity: 1, InitialQuantity: 2, AvgCost: 2000, EntryATR: 40,
				SLAfterMoved: c.moved, StopLossOID: c.oid, StopLossTriggerPx: c.trigger, SLAfterTriggerPx: c.preserved,
			}
			plan, ok := buildHyperliquidProtectionPlan(sc, pos, c.liqPx)
			if !ok {
				t.Fatal("plan not built")
			}
			if (plan.StopLossATRMult > 0) != c.wantMult {
				t.Fatalf("stop leg mult = %g, want leg=%v", plan.StopLossATRMult, c.wantMult)
			}
			if plan.PreserveMovedStop != c.wantPreserve {
				t.Fatalf("preserve = %v, want %v", plan.PreserveMovedStop, c.wantPreserve)
			}
			if math.Abs(plan.StopLossTriggerPx-c.wantTrigger) > 1e-9 {
				t.Fatalf("trigger = %.6f, want %.6f", plan.StopLossTriggerPx, c.wantTrigger)
			}
			if plan.ForceSLReplace != c.wantForce {
				t.Fatalf("force = %v, want %v", plan.ForceSLReplace, c.wantForce)
			}
		})
	}
}

func TestRunHyperliquidProtectionSyncForRemainderPreservesMovedTrigger(t *testing.T) {
	sc := movedStopTestStrategy()
	newState := func(pos Position) *StrategyState {
		p := pos
		p.TPOIDs = append([]int64(nil), pos.TPOIDs...)
		p.TPArmedTiers = append([]bool(nil), pos.TPArmedTiers...)
		return &StrategyState{ID: sc.ID, Positions: map[string]*Position{"ETH": &p}}
	}
	moved := Position{
		Symbol: "ETH", Side: "long", Quantity: 1, InitialQuantity: 2, AvgCost: 2000, EntryATR: 40,
		StopLossOID: 7, StopLossTriggerPx: 2000, SLAfterMoved: true, SLAfterTriggerPx: 2000,
		TPOIDs: []int64{0, 22}, TPArmedTiers: []bool{true, true},
	}
	var plans []hlProtectionPlan
	stubResult := func(res *HyperliquidProtectionSyncResult) {
		withStubbedSyncHyperliquidProtection(t, func(_ StrategyConfig, plan hlProtectionPlan, _ *MultiNotifier, _ *StrategyLogger, _ []byte) (*HyperliquidProtectionSyncResult, bool) {
			plans = append(plans, plan)
			return res, true
		})
	}
	run := func(cfg StrategyConfig, st *StrategyState, stopQty float64, afterFill bool, prevOID int64, out *bytes.Buffer) hlStopRearmResult {
		var mu sync.RWMutex
		_, _, stopRes, _ := runHyperliquidProtectionSyncForRemainder(cfg, st, nil, "ETH", &mu, nil, &StrategyLogger{stratID: cfg.ID, writer: out}, "test", nil, nil, nil, hlProtectionGuardFull, stopQty, afterFill, prevOID, hlCloseUnconfirmed{})
		return stopRes
	}

	t.Run("partial fill re-places at the moved trigger", func(t *testing.T) {
		plans = nil
		stubResult(&HyperliquidProtectionSyncResult{CancelStopLossSucceeded: true, StopLossOID: 8, StopLossTriggerPx: 2000, TPOIDs: []int64{0, 22}})
		st := newState(moved)
		run(sc, st, 0.5, true, 7, &bytes.Buffer{})
		if len(plans) != 1 {
			t.Fatalf("sync calls = %d, want 1", len(plans))
		}
		p := plans[0]
		if !p.ForceSLReplace || !p.PreserveMovedStop || p.StopLossTriggerPx != 2000 || p.Size != 0.5 {
			t.Fatalf("plan = force %v preserve %v trigger %g size %g, want a forced 0.5 stop at 2000", p.ForceSLReplace, p.PreserveMovedStop, p.StopLossTriggerPx, p.Size)
		}
		got := st.Positions["ETH"]
		if got.StopLossOID != 8 || got.StopLossTriggerPx != 2000 || got.SLAfterTriggerPx != 2000 {
			t.Fatalf("book = oid %d trigger %g preserved %g, want 8 / 2000 / 2000", got.StopLossOID, got.StopLossTriggerPx, got.SLAfterTriggerPx)
		}
	})

	t.Run("unroundable trigger on a forced replace reports the pre-close stop resting", func(t *testing.T) {
		plans = nil
		msg := "the forced replace was refused before the cancel: the supplied stop trigger 2000.004 cannot be rounded to a venue price without loosening it; no stop was cancelled or placed"
		stubResult(&HyperliquidProtectionSyncResult{CancelStopLossError: msg, StopLossError: msg, TPOIDs: []int64{0, 22}})
		st := newState(moved)
		res := run(sc, st, 0.5, true, 7, &bytes.Buffer{})
		if res.Status != hlStopRearmPreCloseStopResting {
			t.Fatalf("status = %v, want hlStopRearmPreCloseStopResting", res.Status)
		}
		got := st.Positions["ETH"]
		if got.StopLossOID != 7 || got.StopLossTriggerPx != 2000 || got.SLAfterTriggerPx != 2000 {
			t.Fatalf("book = oid %d trigger %g preserved %g, want the pre-close stop 7 / 2000 / 2000 kept", got.StopLossOID, got.StopLossTriggerPx, got.SLAfterTriggerPx)
		}
		report, critical := formatCloseStopRearmReport(sc, "ETH", hlCloseRemainderStop{Remainder: 0.5, Qty: 0.5, Basis: hlRemainderBasisFresh}, hlCloseFillOutcome{Filled: 0.5, Known: true}, res, 7)
		if !critical || !strings.Contains(report, "pre-close stop (OID=7) was not cancelled") || strings.Contains(report, "now rests") {
			t.Fatalf("report critical=%v %q, want a critical pre-close-stop-resting report", critical, report)
		}
	})

	t.Run("cancel landed and placement failed keeps the trigger for the retry", func(t *testing.T) {
		plans = nil
		stubResult(&HyperliquidProtectionSyncResult{CancelStopLossSucceeded: true, StopLossError: "place_stop_loss SDK error: open order cap"})
		st := newState(moved)
		st.Positions["ETH"].SLAfterTriggerPx = 0
		run(sc, st, 0.5, true, 7, &bytes.Buffer{})
		got := st.Positions["ETH"]
		if got.StopLossOID != 0 || got.StopLossTriggerPx != 0 || got.SLAfterTriggerPx != 2000 {
			t.Fatalf("book = oid %d trigger %g preserved %g, want 0 / 0 / 2000", got.StopLossOID, got.StopLossTriggerPx, got.SLAfterTriggerPx)
		}
		stubResult(&HyperliquidProtectionSyncResult{StopLossOID: 9, StopLossTriggerPx: 2000, TPOIDs: []int64{0, 22}})
		run(sc, st, 0, false, 0, &bytes.Buffer{})
		if len(plans) != 2 {
			t.Fatalf("sync calls = %d, want 2", len(plans))
		}
		retry := plans[1]
		if retry.StopLossOID != 0 || !retry.PreserveMovedStop || retry.StopLossTriggerPx != 2000 || retry.StopLossATRMult <= 0 {
			t.Fatalf("retry plan = oid %d preserve %v trigger %g mult %g, want a fresh stop at 2000", retry.StopLossOID, retry.PreserveMovedStop, retry.StopLossTriggerPx, retry.StopLossATRMult)
		}
		if got.StopLossOID != 9 || got.StopLossTriggerPx != 2000 {
			t.Fatalf("book after retry = oid %d trigger %g, want 9 @ 2000", got.StopLossOID, got.StopLossTriggerPx)
		}
	})

	t.Run("rejected cancel keeps the resting stop and the trigger", func(t *testing.T) {
		plans = nil
		stubResult(&HyperliquidProtectionSyncResult{CancelStopLossError: "rejected", StopLossError: "force replace cancel rejected: rejected"})
		st := newState(moved)
		run(sc, st, 0.5, true, 7, &bytes.Buffer{})
		got := st.Positions["ETH"]
		if got.StopLossOID != 7 || got.StopLossTriggerPx != 2000 || got.SLAfterTriggerPx != 2000 {
			t.Fatalf("book = oid %d trigger %g preserved %g, want 7 / 2000 / 2000", got.StopLossOID, got.StopLossTriggerPx, got.SLAfterTriggerPx)
		}
	})

	t.Run("unknown placement keeps the recorded stop and the moved trigger", func(t *testing.T) {
		plans = nil
		stubResult(&HyperliquidProtectionSyncResult{CancelStopLossSucceeded: true, StopLossOutcomeUnknown: true})
		st := newState(moved)
		run(sc, st, 0.5, true, 7, &bytes.Buffer{})
		got := st.Positions["ETH"]
		if got.StopLossOID != 7 || got.StopLossTriggerPx != 2000 || got.SLAfterTriggerPx != 2000 {
			t.Fatalf("book = oid %d trigger %g preserved %g, want 7 / 2000 / 2000", got.StopLossOID, got.StopLossTriggerPx, got.SLAfterTriggerPx)
		}
		run(sc, st, 0, false, 0, &bytes.Buffer{})
		if len(plans) != 2 || plans[1].StopLossTriggerPx != 2000 || !plans[1].PreserveMovedStop {
			t.Fatalf("next plans = %+v, want the moved trigger 2000", plans)
		}
	})

	t.Run("lost trigger refuses the stop leg and reports it", func(t *testing.T) {
		plans = nil
		stubResult(&HyperliquidProtectionSyncResult{TPOIDs: []int64{0, 22}})
		lost := moved
		lost.StopLossOID, lost.StopLossTriggerPx, lost.SLAfterTriggerPx = 0, 0, 0
		st := newState(lost)
		out := &bytes.Buffer{}
		res := run(sc, st, 0.5, true, 7, out)
		if len(plans) != 1 {
			t.Fatalf("sync calls = %d, want 1 for the take-profit legs", len(plans))
		}
		p := plans[0]
		if p.StopLossATRMult != 0 || p.PreserveMovedStop || p.StopLossTriggerPx != 0 || p.ForceSLReplace {
			t.Fatalf("plan kept a stop leg: mult %g preserve %v trigger %g force %v", p.StopLossATRMult, p.PreserveMovedStop, p.StopLossTriggerPx, p.ForceSLReplace)
		}
		if res.Status != hlStopRearmMovedTriggerLost || !res.failed() {
			t.Fatalf("stop result = %+v, want a moved-trigger-lost failure", res)
		}
		if !strings.Contains(out.String(), "CRITICAL") {
			t.Fatalf("log = %q, want a CRITICAL line", out.String())
		}
		for _, a := range buildHyperliquidSyncProtectionArgv(p, nil) {
			if strings.HasPrefix(a, "--stop-loss-trigger-px") || a == "--preserve-moved-stop" {
				t.Fatalf("argv carries %q after the refusal", a)
			}
		}
	})

	t.Run("lost trigger with no take-profit legs makes no sync call", func(t *testing.T) {
		plans = nil
		stubResult(&HyperliquidProtectionSyncResult{})
		noTiers := sc
		noTiers.CloseStrategy = nil
		lost := moved
		lost.StopLossOID, lost.StopLossTriggerPx, lost.SLAfterTriggerPx = 0, 0, 0
		res := run(noTiers, newState(lost), 0, false, 0, &bytes.Buffer{})
		if len(plans) != 0 {
			t.Fatalf("sync calls = %d, want 0", len(plans))
		}
		if res.Status != hlStopRearmMovedTriggerLost {
			t.Fatalf("stop result = %+v, want moved-trigger-lost", res)
		}
	})

	t.Run("unmoved stop keeps the label contract", func(t *testing.T) {
		plans = nil
		stubResult(&HyperliquidProtectionSyncResult{CancelStopLossSucceeded: true, StopLossOID: 8, StopLossTriggerPx: 1960, TPOIDs: []int64{0, 22}})
		plain := moved
		plain.SLAfterMoved, plain.StopLossTriggerPx, plain.SLAfterTriggerPx = false, 1960, 0
		st := newState(plain)
		run(sc, st, 0.5, true, 7, &bytes.Buffer{})
		if len(plans) != 1 || plans[0].PreserveMovedStop || plans[0].StopLossTriggerPx != 0 || !plans[0].ForceSLReplace {
			t.Fatalf("plans = %+v, want a forced label replacement with no trigger", plans)
		}
		if st.Positions["ETH"].SLAfterTriggerPx != 0 {
			t.Fatalf("unmoved stop recorded preserved trigger %g", st.Positions["ETH"].SLAfterTriggerPx)
		}
	})
}

func TestBuildHyperliquidSyncProtectionArgvCarriesTheMovedStopContract(t *testing.T) {
	flagsOf := func(argv []string) map[string]bool {
		out := map[string]bool{}
		for _, a := range argv {
			name, _, _ := strings.Cut(a, "=")
			out[name] = true
		}
		return out
	}
	base := hlProtectionPlan{Symbol: "ETH", Side: "long", Size: 1, AvgCost: 2000, EntryATR: 40, StopLossATRMult: 1}

	unmoved := flagsOf(buildHyperliquidSyncProtectionArgv(base, nil))
	if unmoved["--stop-loss-trigger-px"] || unmoved["--preserve-moved-stop"] {
		t.Fatalf("unmoved argv carries the moved-stop contract: %v", unmoved)
	}

	moved := base
	moved.PreserveMovedStop = true
	moved.StopLossTriggerPx = 2000.5
	argv := buildHyperliquidSyncProtectionArgv(moved, nil)
	if !slices.Contains(argv, "--stop-loss-trigger-px=2000.5") || !slices.Contains(argv, "--preserve-moved-stop") {
		t.Fatalf("moved argv = %v, want the trigger and the preserve flag", argv)
	}

	lost := base
	lost.PreserveMovedStop = true
	argv = buildHyperliquidSyncProtectionArgv(lost, nil)
	if !slices.Contains(argv, "--stop-loss-trigger-px=0") || !slices.Contains(argv, "--preserve-moved-stop") {
		t.Fatalf("lost-trigger argv = %v, want trigger 0 with the preserve flag so Python refuses", argv)
	}

	noLeg := moved
	noLeg.StopLossATRMult = 0
	if f := flagsOf(buildHyperliquidSyncProtectionArgv(noLeg, nil)); f["--stop-loss-trigger-px"] || f["--preserve-moved-stop"] {
		t.Fatalf("argv without a stop leg carries the moved-stop contract: %v", f)
	}

	full := moved
	full.StopLossOID = 7
	full.Tiers = []hlProtectionTier{{Multiple: 1, Fraction: 0.5}, {Multiple: 2, Fraction: 1}}
	full.TPOIDs = []int64{1, 2}
	full.TPArmedTiers = []bool{true, false}
	full.ForceSLReplace = true
	full.ForceTPReplace = []bool{true, false}
	full.CancelTPOIDs = []int64{3}
	probe := flagsOf(syncProtectionProbeArgv)
	for name := range flagsOf(buildHyperliquidSyncProtectionArgv(full, []byte(`[]`))) {
		if !probe[name] {
			t.Fatalf("builder flag %s is missing from the startup probe argv", name)
		}
	}
	if !probe["--probe-only"] {
		t.Fatal("probe argv lacks --probe-only")
	}
}

func TestBackfillMovedStopMarkers(t *testing.T) {
	ruleOnFirst := postTPSLTestStrategy(nil, []interface{}{
		map[string]interface{}{"atr_multiple": 1, "close_fraction": 0.5, "sl_after": "breakeven"},
		map[string]interface{}{"atr_multiple": 2, "close_fraction": 1.0},
	})
	ruleOnFirst.ID = "hl-rule-first"
	ruleOnSecond := postTPSLTestStrategy(nil, []interface{}{
		map[string]interface{}{"atr_multiple": 1, "close_fraction": 0.5},
		map[string]interface{}{"atr_multiple": 2, "close_fraction": 1.0, "sl_after": "breakeven"},
	})
	ruleOnSecond.ID = "hl-rule-second"
	pos := func() *Position {
		return &Position{Symbol: "ETH", Side: "long", Quantity: 1, InitialQuantity: 2, AvgCost: 2000, EntryATR: 40, StopLossOID: 7, StopLossTriggerPx: 2000, SLAdjustedTiersProcessed: 1}
	}
	state := &AppState{Strategies: map[string]*StrategyState{
		ruleOnFirst.ID:  {ID: ruleOnFirst.ID, Positions: map[string]*Position{"ETH": pos()}},
		ruleOnSecond.ID: {ID: ruleOnSecond.ID, Positions: map[string]*Position{"ETH": pos()}},
	}}
	cfg := &Config{Strategies: []StrategyConfig{ruleOnFirst, ruleOnSecond}}
	var mu sync.RWMutex
	if n := backfillMovedStopMarkers(cfg, state, &mu); n != 1 {
		t.Fatalf("marked = %d, want 1", n)
	}
	first := state.Strategies[ruleOnFirst.ID].Positions["ETH"]
	if !first.SLAfterMoved || first.SLAfterTriggerPx != 2000 {
		t.Fatalf("rule tier position = moved %v preserved %g, want true / 2000", first.SLAfterMoved, first.SLAfterTriggerPx)
	}
	second := state.Strategies[ruleOnSecond.ID].Positions["ETH"]
	if second.SLAfterMoved || second.SLAfterTriggerPx != 0 {
		t.Fatalf("empty-rule tier position = moved %v preserved %g, want untouched", second.SLAfterMoved, second.SLAfterTriggerPx)
	}
	if n := backfillMovedStopMarkers(cfg, state, &mu); n != 0 {
		t.Fatalf("second backfill marked %d, want 0", n)
	}
}

func TestMovedStopTriggerSurvivesRestart(t *testing.T) {
	db, err := OpenStateDB(filepath.Join(t.TempDir(), "moved.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()
	st := &AppState{Strategies: map[string]*StrategyState{
		"hl-sl-after": {ID: "hl-sl-after", Type: "perps", Platform: "hyperliquid", Cash: 1000, InitialCapital: 1000, Positions: map[string]*Position{
			"ETH": {Symbol: "ETH", Side: "long", Quantity: 1, InitialQuantity: 2, AvgCost: 2000, EntryATR: 40, SLAfterMoved: true, SLAfterTriggerPx: 2000.5},
		}},
	}}
	if err := db.SaveState(st); err != nil {
		t.Fatalf("save: %v", err)
	}
	loaded, err := db.LoadState()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	got := loaded.Strategies["hl-sl-after"].Positions["ETH"]
	if !got.SLAfterMoved || got.SLAfterTriggerPx != 2000.5 || got.StopLossOID != 0 || got.StopLossTriggerPx != 0 {
		t.Fatalf("reloaded = moved %v preserved %g oid %d trigger %g, want true / 2000.5 / no resting order", got.SLAfterMoved, got.SLAfterTriggerPx, got.StopLossOID, got.StopLossTriggerPx)
	}
	plan, ok := buildHyperliquidProtectionPlan(movedStopTestStrategy(), got, 0)
	if !ok || !plan.PreserveMovedStop || plan.StopLossTriggerPx != 2000.5 {
		t.Fatalf("plan after restart = %+v, want the preserved trigger 2000.5", plan)
	}
}

func TestMigratePositionsAddsMovedStopTrigger(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy.db")
	fresh, err := OpenStateDB(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if _, err := fresh.db.Exec("ALTER TABLE positions DROP COLUMN sl_after_trigger_px"); err != nil {
		t.Fatalf("drop: %v", err)
	}
	fresh.Close()

	db, err := OpenStateDB(path)
	if err != nil {
		t.Fatalf("migrate: %v", err)
	}
	defer db.Close()
	if err := db.migrateSchema(); err != nil {
		t.Fatalf("second migrate: %v", err)
	}
	st := &AppState{Strategies: map[string]*StrategyState{
		"hl-sl-after": {ID: "hl-sl-after", Type: "perps", Platform: "hyperliquid", Cash: 1000, InitialCapital: 1000, Positions: map[string]*Position{
			"ETH": {Symbol: "ETH", Side: "short", Quantity: 1, InitialQuantity: 2, AvgCost: 2000, EntryATR: 40, StopLossOID: 7, StopLossTriggerPx: 1990, SLAfterMoved: true, SLAfterTriggerPx: 1990},
		}},
	}}
	if err := db.SaveState(st); err != nil {
		t.Fatalf("save: %v", err)
	}
	loaded, err := db.LoadState()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if got := loaded.Strategies["hl-sl-after"].Positions["ETH"]; got.SLAfterTriggerPx != 1990 || got.StopLossOID != 7 {
		t.Fatalf("reloaded preserved %g oid %d, want 1990 / 7", got.SLAfterTriggerPx, got.StopLossOID)
	}
}

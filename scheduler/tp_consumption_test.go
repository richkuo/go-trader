package main

import (
	"bytes"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func unifiedSLBlock(rule interface{}) map[string]interface{} {
	tier := func(mult, frac float64, sl interface{}) map[string]interface{} {
		m := map[string]interface{}{"atr_multiple": mult, "close_fraction": frac}
		if sl != nil {
			m["sl_after"] = sl
		}
		return m
	}
	label := func(sl float64, tiers ...map[string]interface{}) map[string]interface{} {
		raw := make([]interface{}, len(tiers))
		for i := range tiers {
			raw[i] = tiers[i]
		}
		return map[string]interface{}{"stop_loss_atr": sl, "tp_tiers": raw}
	}
	return map[string]interface{}{
		regimeClassifierKey: map[string]interface{}{
			"trending_up":   label(1.5, tier(1, 0.5, rule), tier(2, 1, nil)),
			"trending_down": label(1.5, tier(1, 0.5, rule), tier(2, 1, nil)),
			"ranging":       label(1.0, tier(1.5, 0.5, rule), tier(3, 1, nil)),
		},
	}
}

func unifiedSLStrategy(name, typ string, live bool, rule interface{}) StrategyConfig {
	args := []string{"sma", "ETH", "1h"}
	if live {
		args = append(args, "--mode=live")
	}
	return StrategyConfig{
		ID:       "hl-unified-sl",
		Platform: "hyperliquid",
		Type:     typ,
		Script:   "shared_scripts/check_hyperliquid.py",
		Args:     args,
		CloseStrategy: &StrategyRef{
			Name:   name,
			Params: unifiedSLBlock(rule),
		},
	}
}

func TestUnifiedDiscoveryDoesNotBook(t *testing.T) {
	sc := unifiedSLStrategy("tiered_tp_atr_live_regime", "perps", true, "breakeven")
	pos := &Position{
		Symbol: "ETH", Side: "long", Quantity: 1, InitialQuantity: 2,
		AvgCost: 2000, EntryATR: 40, Regime: "ranging",
		StopLossOID: 7, StopLossTriggerPx: 1960,
		TPOIDs: []int64{11, 22}, TPArmedTiers: []bool{true, true},
	}
	orig := syncHyperliquidProtection
	t.Cleanup(func() { syncHyperliquidProtection = orig })

	run := func(result *HyperliquidProtectionSyncResult) *Position {
		t.Helper()
		syncHyperliquidProtection = func(StrategyConfig, hlProtectionPlan, *MultiNotifier, *StrategyLogger, []byte) (*HyperliquidProtectionSyncResult, bool) {
			return result, true
		}
		p := *pos
		p.TPOIDs = append([]int64(nil), pos.TPOIDs...)
		p.TPArmedTiers = append([]bool(nil), pos.TPArmedTiers...)
		p.TPConsumptions = nil
		st := &StrategyState{ID: sc.ID, Positions: map[string]*Position{"ETH": &p}}
		var mu sync.RWMutex
		runHyperliquidProtectionSyncForRemainder(sc, st, nil, "ETH", &mu, nil, &StrategyLogger{stratID: sc.ID, writer: &bytes.Buffer{}}, "test", nil, nil, nil, hlProtectionGuardFull, 0, false, 0, hlCloseUnconfirmed{})
		return &p
	}

	got := run(&HyperliquidProtectionSyncResult{TPFilledExternally: []bool{true, false}, TPOIDs: []int64{0, 22}})
	if n := len(got.TPConsumptions); n != 1 || got.TPConsumptions[0].Stage != tpConsumptionDiscovered || got.TPConsumptions[0].Tier != 0 || got.TPConsumptions[0].OID != 11 {
		t.Fatalf("filled externally = %+v, want one discovered tier 0 oid 11", got.TPConsumptions)
	}
	if got.SLAfterMoved {
		t.Fatal("discovery moved the stop")
	}

	unknown := run(&HyperliquidProtectionSyncResult{TPOutcomeUnknown: []bool{true, false}, TPFilledExternally: []bool{true, false}})
	if len(unknown.TPConsumptions) != 0 {
		t.Fatalf("unknown placement created %+v", unknown.TPConsumptions)
	}
	missing := run(&HyperliquidProtectionSyncResult{})
	if len(missing.TPConsumptions) != 0 || missing.SLAfterMoved {
		t.Fatalf("missing tp_oids created %+v moved %v", missing.TPConsumptions, missing.SLAfterMoved)
	}
}

func TestBookedConsumptionSurvivesDelayRestartAndRepeat(t *testing.T) {
	sc := unifiedSLStrategy("tiered_tp_atr_regime", "perps", true, "breakeven")
	pos := &Position{
		Symbol: "ETH", Side: "long", Quantity: 1, InitialQuantity: 2,
		AvgCost: 2000, EntryATR: 40, Regime: "ranging",
		TPOIDs: []int64{11, 22},
	}
	now := time.Now().UTC().Truncate(time.Second)
	recordDiscoveredTPConsumptions(pos, "ranging", pos.TPOIDs, &HyperliquidProtectionSyncResult{TPFilledExternally: []bool{true, false}})
	if len(pos.TPConsumptions) != 1 || pos.TPConsumptions[0].Stage != tpConsumptionDiscovered {
		t.Fatalf("discover = %+v", pos.TPConsumptions)
	}
	recordTPConsumptionAtBooking(sc, pos, 0.5, 11, -1)
	if len(pos.TPConsumptions) != 1 || pos.TPConsumptions[0].Stage != tpConsumptionBooked || pos.TPConsumptions[0].BookedQty != 0.5 {
		t.Fatalf("delayed book = %+v", pos.TPConsumptions)
	}
	recordTPConsumptionAtBooking(sc, pos, 0.25, 11, -1)
	if len(pos.TPConsumptions) != 1 || pos.TPConsumptions[0].BookedQty != 0.75 || pos.TPConsumptions[0].Stage != tpConsumptionBooked {
		t.Fatalf("second book = %+v", pos.TPConsumptions)
	}

	path := filepath.Join(t.TempDir(), "state.db")
	db, err := OpenStateDB(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	pos.TPConsumptions[0].UpdatedAt = now
	st := &AppState{Strategies: map[string]*StrategyState{
		sc.ID: {ID: sc.ID, Type: "perps", Platform: "hyperliquid", Cash: 1000, InitialCapital: 1000, Positions: map[string]*Position{"ETH": pos}},
	}}
	if err := db.SaveState(st); err != nil {
		t.Fatalf("save: %v", err)
	}
	db.Close()
	db, err = OpenStateDB(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer db.Close()
	loaded, err := db.LoadState()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	got := loaded.Strategies[sc.ID].Positions["ETH"]
	recordTPConsumptionAtBooking(sc, got, 0.1, 11, -1)
	if len(got.TPConsumptions) != 1 || got.TPConsumptions[0].Stage != tpConsumptionBooked || got.TPConsumptions[0].BookedQty != 0.85 {
		t.Fatalf("after restart = %+v", got.TPConsumptions)
	}
}

func TestMigratePositionsKeepsConsumption(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy.db")
	fresh, err := OpenStateDB(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	for _, col := range []string{"sl_after_moved", "tp_consumptions_json"} {
		if _, err := fresh.db.Exec("ALTER TABLE positions DROP COLUMN " + col); err != nil {
			t.Fatalf("drop %s: %v", col, err)
		}
	}
	fresh.Close()

	db, err := OpenStateDB(path)
	if err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if err := db.migrateSchema(); err != nil {
		t.Fatalf("second migrate: %v", err)
	}
	now := time.Now().UTC().Truncate(time.Microsecond)
	pos := &Position{
		Symbol: "ETH", Quantity: 1, InitialQuantity: 2, AvgCost: 2000, Side: "long",
		SLAfterMoved: true,
		TPConsumptions: []TPConsumption{
			{Label: "ranging", Tier: 0, Stage: tpConsumptionBooked, BookedQty: 0.4, UpdatedAt: now},
			{Label: "ranging", Tier: 1, Stage: tpConsumptionDone, BookedQty: 0.6, UpdatedAt: now},
			{Label: "", Tier: -1, Stage: tpConsumptionDeferred, DeferReason: tpDeferUnattributed, Count: 2, BookedQty: 0.1, UpdatedAt: now},
		},
	}
	st := &AppState{Strategies: map[string]*StrategyState{
		"hl-unified-sl": {ID: "hl-unified-sl", Type: "perps", Platform: "hyperliquid", Cash: 1000, InitialCapital: 1000, Positions: map[string]*Position{"ETH": pos}},
	}}
	if err := db.SaveState(st); err != nil {
		t.Fatalf("save: %v", err)
	}
	loaded, err := db.LoadState()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	db.Close()
	got := loaded.Strategies["hl-unified-sl"].Positions["ETH"]
	if !got.SLAfterMoved || len(got.TPConsumptions) != 3 {
		t.Fatalf("reloaded marker %v records %+v", got.SLAfterMoved, got.TPConsumptions)
	}
	want := pos.TPConsumptions
	for i := range want {
		g := got.TPConsumptions[i]
		if g.Label != want[i].Label || g.Tier != want[i].Tier || g.Stage != want[i].Stage || g.DeferReason != want[i].DeferReason || g.BookedQty != want[i].BookedQty || g.Count != want[i].Count || !g.UpdatedAt.Equal(want[i].UpdatedAt) {
			t.Fatalf("record %d = %+v, want %+v", i, g, want[i])
		}
	}
}

func TestLiveUnifiedReplyClasses(t *testing.T) {
	sc := unifiedSLStrategy("tiered_tp_atr_live_regime_dynamic", "perps", true, "breakeven")
	orig := runHyperliquidUpdateStopLossFunc
	t.Cleanup(func() { runHyperliquidUpdateStopLossFunc = orig })
	cases := []struct {
		name    string
		reply   *HyperliquidStopLossUpdateResult
		wantOn  bool
		wantN   int
		stage   string
		moved   bool
		remains bool
	}{
		{"resting", &HyperliquidStopLossUpdateResult{StopLossOID: 900, StopLossTriggerPx: 2000}, true, 0, tpConsumptionDone, true, true},
		{"filled remainder", &HyperliquidStopLossUpdateResult{StopLossFilledImmediately: true, StopLossTriggerPx: 2000, StopLossSize: 0.4}, true, 1, tpConsumptionDone, true, true},
		{"unknown", &HyperliquidStopLossUpdateResult{StopLossOutcomeUnknown: true, StopLossOldStillOpen: true}, false, 0, tpConsumptionBooked, false, true},
		{"protection lost", &HyperliquidStopLossUpdateResult{CancelStopLossSucceeded: true}, false, 0, tpConsumptionBooked, false, true},
		{"rejected", &HyperliquidStopLossUpdateResult{StopLossError: "rejected"}, false, 0, tpConsumptionBooked, false, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			runHyperliquidUpdateStopLossFunc = func(_, _, _ string, _, triggerPx float64, _ int64) (*HyperliquidStopLossUpdateResult, string, error) {
				return tc.reply, "", nil
			}
			pos := &Position{
				Symbol: "ETH", Side: "long", Quantity: 1, InitialQuantity: 2,
				AvgCost: 2000, EntryATR: 40, Regime: "ranging", RegimeAppliedLabel: "ranging",
				StopLossOID: 7, StopLossTriggerPx: 1900,
				TPConsumptions: []TPConsumption{{Label: "ranging", Tier: 0, Stage: tpConsumptionBooked, UpdatedAt: time.Now().UTC()}},
			}
			st := &StrategyState{ID: sc.ID, Cash: 1000, Positions: map[string]*Position{"ETH": pos}}
			var mu sync.RWMutex
			applied, fills, _ := runPostTPStopLossAdjustment(sc, st, "ETH", 2100, nil, &mu, nil, &StrategyLogger{stratID: sc.ID, writer: &bytes.Buffer{}}, nil, nil, nil)
			if applied != tc.wantOn || fills != tc.wantN {
				t.Fatalf("applied %v fills %d, want %v %d", applied, fills, tc.wantOn, tc.wantN)
			}
			cur := st.Positions["ETH"]
			if tc.remains && (cur == nil || len(cur.TPConsumptions) != 1 || cur.TPConsumptions[0].Stage != tc.stage || cur.SLAfterMoved != tc.moved) {
				t.Fatalf("position %+v", cur)
			}
		})
	}
}

func TestPaperUnifiedRulesForThreeNames(t *testing.T) {
	names := []string{"tiered_tp_atr_regime", "tiered_tp_atr_live_regime", "tiered_tp_atr_live_regime_dynamic"}
	rules := []struct {
		name  string
		rule  interface{}
		mark  float64
		want  float64
		trail float64
	}{
		{"breakeven", "breakeven", 2100, 2000, 0},
		{"atr_offset", map[string]interface{}{"kind": "atr_offset", "atr_mult": -0.25}, 2100, 1990, 0},
		{"trail_from_here", map[string]interface{}{"kind": "trail_from_here", "atr_mult": 1}, 2100, 2060, 1},
		{"tp_atr_fraction", map[string]interface{}{"kind": "trail_from_here", "tp_atr_fraction": 0.5}, 2100, 2070, 0.75},
	}
	for _, name := range names {
		for _, rule := range rules {
			t.Run(name+"/"+rule.name, func(t *testing.T) {
				sc := unifiedSLStrategy(name, "perps", false, rule.rule)
				pos := &Position{
					Symbol: "ETH", Side: "long", Quantity: 1, InitialQuantity: 2,
					AvgCost: 2000, EntryATR: 40, Regime: "ranging", RegimeAppliedLabel: "ranging",
					StopLossTriggerPx: 1900,
					TPConsumptions:    []TPConsumption{{Label: "ranging", Tier: 0, Stage: tpConsumptionBooked, UpdatedAt: time.Now().UTC()}},
				}
				st := &StrategyState{ID: sc.ID, Positions: map[string]*Position{"ETH": pos}}
				var mu sync.RWMutex
				if !runPaperPostTPStopLossAdjustment(sc, st, "ETH", rule.mark, nil, &mu, nil, nil) {
					t.Fatal("expected a paper move")
				}
				if pos.StopLossTriggerPx != rule.want || !pos.SLAfterMoved || pos.TPConsumptions[0].Stage != tpConsumptionDone {
					t.Fatalf("trigger %v moved %v stage %s, want %v", pos.StopLossTriggerPx, pos.SLAfterMoved, pos.TPConsumptions[0].Stage, rule.want)
				}
				gotTrail := 0.0
				if pos.PostTPTrailingATRMult != nil {
					gotTrail = *pos.PostTPTrailingATRMult
				}
				if gotTrail != rule.trail {
					t.Fatalf("trail %v, want %v", gotTrail, rule.trail)
				}
			})
		}
	}
}

func TestPaperFractionChangeDoesNotReplayConsumption(t *testing.T) {
	sc := unifiedSLStrategy("tiered_tp_atr_live_regime_dynamic", "perps", false, "breakeven")
	pos := &Position{
		Symbol: "ETH", Side: "long", Quantity: 1, InitialQuantity: 2,
		AvgCost: 2000, EntryATR: 40, Regime: "ranging", RegimeAppliedLabel: "trending_up",
		StopLossTriggerPx: 1900,
	}
	recordPaperUnifiedTPConsumption(sc, pos, 2, 2)
	if len(pos.TPConsumptions) != 1 || pos.TPConsumptions[0].Label != "trending_up" || pos.TPConsumptions[0].Tier != 0 {
		t.Fatalf("first booking = %+v", pos.TPConsumptions)
	}
	st := &StrategyState{ID: sc.ID, Positions: map[string]*Position{"ETH": pos}}
	var mu sync.RWMutex
	runPaperPostTPStopLossAdjustment(sc, st, "ETH", 2100, nil, &mu, nil, nil)
	pos.RegimeAppliedLabel = "ranging"
	if runPaperPostTPStopLossAdjustment(sc, st, "ETH", 2100, nil, &mu, nil, nil) {
		t.Fatal("label B fractions must not run a second rule without a new booking")
	}
}

func TestReplayTakeProfitPartialCreatesOneRecord(t *testing.T) {
	sc := unifiedSLStrategy("tiered_tp_atr_regime", "perps", false, "breakeven")
	pos := &Position{Symbol: "ETH", Side: "long", Quantity: 2, InitialQuantity: 2, AvgCost: 2000, EntryATR: 40, Regime: "ranging"}
	st := &StrategyState{ID: sc.ID, Cash: 5000, Positions: map[string]*Position{"ETH": pos}}
	row := ReplayDecision{DecisionID: 1, DecisionType: ReplayDecisionPartialClose, Symbol: "ETH", Side: "long", Quantity: 1, CloseReason: "hl_sync_tp1_fill"}
	applyReplayedLiveDecisions(sc, st, []ReplayDecision{row}, 2100, &HyperliquidResult{Symbol: "ETH"}, &Config{}, &StrategyLogger{stratID: sc.ID, writer: &bytes.Buffer{}})
	if len(pos.TPConsumptions) != 1 || pos.TPConsumptions[0].Stage != tpConsumptionBooked {
		t.Fatalf("replay record = %+v", pos.TPConsumptions)
	}
	other := &Position{Symbol: "ETH", Side: "long", Quantity: 2, InitialQuantity: 2, AvgCost: 2000, EntryATR: 40, Regime: "ranging"}
	st2 := &StrategyState{ID: sc.ID, Cash: 5000, Positions: map[string]*Position{"ETH": other}}
	row.DecisionID = 2
	row.CloseReason = "stop_loss"
	applyReplayedLiveDecisions(sc, st2, []ReplayDecision{row}, 2100, &HyperliquidResult{Symbol: "ETH"}, &Config{}, &StrategyLogger{stratID: sc.ID, writer: &bytes.Buffer{}})
	if len(other.TPConsumptions) != 0 {
		t.Fatalf("non-take-profit replay created %+v", other.TPConsumptions)
	}
}

func TestDynamicRegimeStopMovesAgrees(t *testing.T) {
	sc := unifiedSLStrategy(dynamicCloseStrategyName, "perps", false, nil)
	sc.Args = []string{"sma", "ETH", "1h", "--mode=paper"}
	type row struct {
		name      string
		side      string
		moved     bool
		current   float64
		want      bool
		trail     bool
		strategy  bool
		candidate float64
	}
	// Ranging fixed stop is 1968 long / 2032 short from entry 2000 and ATR 40.
	rows := []row{
		{"unmarked tighter long", "long", false, 1940, true, false, false, 0},
		{"unmarked looser long", "long", false, 1990, true, false, false, 0},
		{"marked breakeven faces looser long", "long", true, 2000, false, false, false, 0},
		{"marked negative offset faces tighter long", "long", true, 1900, true, false, false, 0},
		{"inside min-move gate long", "long", false, 1967, false, false, false, 0},
		{"post-profit trailing owner long", "long", true, 1990, false, true, false, 0},
		{"strategy trailing owner long", "long", false, 1990, false, false, true, 0},
		{"unmarked tighter short", "short", false, 2060, true, false, false, 0},
		{"marked breakeven faces looser short", "short", true, 2000, false, false, false, 0},
		{"marked negative offset faces tighter short", "short", true, 2100, true, false, false, 0},
		{"liquidation-clamped candidate long", "long", true, 1900, true, false, false, 1980},
		{"liquidation-clamped candidate short", "short", true, 2100, true, false, false, 2020},
	}
	for _, tc := range rows {
		t.Run(tc.name, func(t *testing.T) {
			pos := &Position{
				Symbol: "ETH", Side: tc.side, Quantity: 1, AvgCost: 2000, EntryATR: 40,
				RegimeAppliedLabel: "ranging", StopLossTriggerPx: tc.current, StopLossOID: 7,
				SLAfterMoved: tc.moved,
			}
			if tc.trail {
				mult := 1.0
				pos.PostTPTrailingATRMult = &mult
			}
			liveSC := sc
			if tc.strategy {
				mult := 1.5
				liveSC.TrailingStopATRMult = &mult
			}
			_, paperMove := paperDynamicFlipStopTrigger(liveSC, pos)
			plan, ok := buildHyperliquidProtectionPlan(liveSC, pos, 0)
			if !ok {
				t.Fatal("plan did not build")
			}
			forceSL, _ := dynamicProtectionForceReplace(liveSC, pos, plan, "trending_up", true)
			candidate := tc.candidate
			if candidate == 0 {
				if paperMove != forceSL {
					t.Fatalf("paper %v live %v", paperMove, forceSL)
				}
				if paperMove != tc.want {
					t.Fatalf("move %v, want %v", paperMove, tc.want)
				}
				return
			}
			shared := dynamicRegimeStopMoves(liveSC, pos, tc.current, candidate)
			if shared != tc.want {
				t.Fatalf("shared decision %v, want %v", shared, tc.want)
			}
			if dynamicRegimeStopMoves(liveSC, pos, tc.current, candidate) != shared {
				t.Fatal("live and paper shared decision disagree")
			}
		})
	}
}

func TestConsumptionHoldsThenAdvances(t *testing.T) {
	block := unifiedSLBlock("breakeven")
	trend := block[regimeClassifierKey].(map[string]interface{})
	up := trend["trending_up"].(map[string]interface{})
	tiers := []interface{}{
		map[string]interface{}{"atr_multiple": 1.0, "close_fraction": 0.25},
		map[string]interface{}{"atr_multiple": 2.0, "close_fraction": 0.5},
		map[string]interface{}{"atr_multiple": 3.0, "close_fraction": 0.75},
		map[string]interface{}{"atr_multiple": 4.0, "close_fraction": 1.0, "sl_after": "breakeven"},
	}
	up["tp_tiers"] = tiers
	sc := StrategyConfig{
		ID: "hl-hold", Platform: "hyperliquid", Type: "perps",
		Script: "shared_scripts/check_hyperliquid.py",
		Args:   []string{"sma", "ETH", "1h", "--mode=live"},
		CloseStrategy: &StrategyRef{Name: dynamicCloseStrategyName, Params: func() map[string]interface{} {
			block["regime_confirm_cycles"] = 1
			return block
		}()},
	}
	pos := &Position{
		Symbol: "ETH", Side: "long", Quantity: 1, InitialQuantity: 2,
		AvgCost: 2000, EntryATR: 40, RegimeAppliedLabel: "trending_up",
		StopLossOID: 7, StopLossTriggerPx: 1900,
		TPOIDs: []int64{1, 2, 3, 4},
		TPConsumptions: []TPConsumption{{
			Label: "trending_up", Tier: 3, Stage: tpConsumptionBooked, UpdatedAt: time.Now().UTC(),
		}},
	}
	st := &StrategyState{ID: sc.ID, Regime: "ranging", Cash: 1000, Positions: map[string]*Position{"ETH": pos}}
	orig := syncHyperliquidProtection
	t.Cleanup(func() { syncHyperliquidProtection = orig })
	syncHyperliquidProtection = func(StrategyConfig, hlProtectionPlan, *MultiNotifier, *StrategyLogger, []byte) (*HyperliquidProtectionSyncResult, bool) {
		return &HyperliquidProtectionSyncResult{StopLossOID: 7, StopLossTriggerPx: 1900, TPOIDs: []int64{1, 2, 3, 4}}, true
	}
	var mu sync.RWMutex
	lg := &StrategyLogger{stratID: sc.ID, writer: &bytes.Buffer{}}
	runHyperliquidProtectionSyncForRemainder(sc, st, nil, "ETH", &mu, nil, lg, "test", nil, nil, nil, hlProtectionGuardFull, 0, false, 0, hlCloseUnconfirmed{})
	if pos.RegimeAppliedLabel != "trending_up" || pos.RegimePendingCount != 0 {
		t.Fatalf("held label %q pending %d", pos.RegimeAppliedLabel, pos.RegimePendingCount)
	}
	origUpdate := runHyperliquidUpdateStopLossFunc
	t.Cleanup(func() { runHyperliquidUpdateStopLossFunc = origUpdate })
	runHyperliquidUpdateStopLossFunc = func(_, _, _ string, _, triggerPx float64, _ int64) (*HyperliquidStopLossUpdateResult, string, error) {
		return &HyperliquidStopLossUpdateResult{StopLossOID: 8, StopLossTriggerPx: triggerPx}, "", nil
	}
	if _, _, _ = runPostTPStopLossAdjustment(sc, st, "ETH", 2100, nil, &mu, nil, lg, nil, nil, nil); pos.TPConsumptions[0].Stage != tpConsumptionDone {
		t.Fatalf("completion stage %s", pos.TPConsumptions[0].Stage)
	}
	runHyperliquidProtectionSyncForRemainder(sc, st, nil, "ETH", &mu, nil, lg, "test", nil, nil, nil, hlProtectionGuardFull, 0, false, 0, hlCloseUnconfirmed{})
	if pos.RegimeAppliedLabel != "ranging" {
		t.Fatalf("advanced label %q, want ranging", pos.RegimeAppliedLabel)
	}
}

func TestLegacyPartialCloseDoesNotMoveStop(t *testing.T) {
	sc := unifiedSLStrategy("tiered_tp_atr_live_regime", "perps", false, "breakeven")
	pos := &Position{
		Symbol: "ETH", Side: "long", Quantity: 1, InitialQuantity: 2,
		AvgCost: 2000, EntryATR: 40, Regime: "ranging", StopLossTriggerPx: 1900,
		TPOIDs: []int64{0, 22}, TPArmedTiers: []bool{true, true},
	}
	var buf bytes.Buffer
	st := &AppState{Strategies: map[string]*StrategyState{
		sc.ID: {ID: sc.ID, Positions: map[string]*Position{"ETH": pos}},
	}}
	var mu sync.RWMutex
	reportLegacyUnifiedSLAfterGaps(&Config{Strategies: []StrategyConfig{sc}}, st, &mu, nil, func(string) *StrategyLogger {
		return &StrategyLogger{stratID: sc.ID, writer: &buf}
	})
	if !bytes.Contains(buf.Bytes(), []byte("earlier take-profit fills do not move the stop")) || !bytes.Contains(buf.Bytes(), []byte("manual stop edit")) {
		t.Fatalf("diagnostic = %s", buf.String())
	}
	if runPaperPostTPStopLossAdjustment(sc, st.Strategies[sc.ID], "ETH", 2100, nil, &mu, nil, nil) || pos.StopLossTriggerPx != 1900 {
		t.Fatal("legacy partial close moved the stop")
	}
	stopFill := &Position{
		Symbol: "BTC", Side: "long", Quantity: 1, InitialQuantity: 2,
		AvgCost: 2000, EntryATR: 40, Regime: "ranging", StopLossTriggerPx: 1900,
		TPOIDs: []int64{11, 22}, TPArmedTiers: []bool{true, true},
	}
	var quiet bytes.Buffer
	st2 := &AppState{Strategies: map[string]*StrategyState{
		sc.ID: {ID: sc.ID, Positions: map[string]*Position{"BTC": stopFill}},
	}}
	reportLegacyUnifiedSLAfterGaps(&Config{Strategies: []StrategyConfig{sc}}, st2, &mu, nil, func(string) *StrategyLogger {
		return &StrategyLogger{stratID: sc.ID, writer: &quiet}
	})
	if quiet.Len() != 0 {
		t.Fatalf("partial stop fill with live take-profit ids notified: %s", quiet.String())
	}
}

func TestUnifiedCrossLabelRuleDoesNotLoosen(t *testing.T) {
	sc := unifiedSLStrategy(dynamicCloseStrategyName, "perps", false, "breakeven")
	now := time.Now().UTC()
	pos := &Position{
		Symbol: "ETH", Side: "long", Quantity: 1, InitialQuantity: 2,
		AvgCost: 2000, EntryATR: 40, Regime: "ranging", RegimeAppliedLabel: "ranging",
		StopLossTriggerPx: 2080, SLAfterMoved: true,
		PostTPTrailingATRMult: func() *float64 { v := 1.0; return &v }(),
		TPConsumptions: []TPConsumption{
			{Label: "trending_up", Tier: 1, Stage: tpConsumptionDone, UpdatedAt: now},
			{Label: "ranging", Tier: 0, Stage: tpConsumptionBooked, UpdatedAt: now.Add(time.Second)},
		},
	}
	st := &StrategyState{ID: sc.ID, Positions: map[string]*Position{"ETH": pos}}
	var mu sync.RWMutex
	if runPaperPostTPStopLossAdjustment(sc, st, "ETH", 2100, nil, &mu, nil, nil) {
		t.Fatal("a looser breakeven under the new label moved the stop")
	}
	if pos.StopLossTriggerPx != 2080 || pos.TPConsumptions[1].Stage != tpConsumptionDone || !pos.SLAfterMoved {
		t.Fatalf("after looser rule: trigger %v stage %s moved %v", pos.StopLossTriggerPx, pos.TPConsumptions[1].Stage, pos.SLAfterMoved)
	}
	if pos.PostTPTrailingATRMult == nil || *pos.PostTPTrailingATRMult != 1 {
		t.Fatal("trail owner was cleared")
	}

	tighter := unifiedSLStrategy(dynamicCloseStrategyName, "perps", false, map[string]interface{}{"kind": "atr_offset", "atr_mult": 2})
	pos2 := &Position{
		Symbol: "ETH", Side: "long", Quantity: 1, InitialQuantity: 2,
		AvgCost: 2000, EntryATR: 40, Regime: "ranging", RegimeAppliedLabel: "ranging",
		StopLossTriggerPx: 2040, SLAfterMoved: true,
		TPConsumptions: []TPConsumption{
			{Label: "trending_up", Tier: 0, Stage: tpConsumptionDone, UpdatedAt: now},
			{Label: "ranging", Tier: 0, Stage: tpConsumptionBooked, UpdatedAt: now.Add(time.Second)},
		},
	}
	st2 := &StrategyState{ID: tighter.ID, Positions: map[string]*Position{"ETH": pos2}}
	if !runPaperPostTPStopLossAdjustment(tighter, st2, "ETH", 2200, nil, &mu, nil, nil) || pos2.StopLossTriggerPx != 2080 {
		t.Fatalf("tighter cross-label rule trigger %v, want 2080", pos2.StopLossTriggerPx)
	}

	first := unifiedSLStrategy("tiered_tp_atr_live_regime", "perps", false, map[string]interface{}{"kind": "atr_offset", "atr_mult": -1})
	pos3 := &Position{
		Symbol: "ETH", Side: "long", Quantity: 1, InitialQuantity: 2,
		AvgCost: 2000, EntryATR: 40, Regime: "ranging",
		StopLossTriggerPx: 2000,
		TPConsumptions:    []TPConsumption{{Label: "ranging", Tier: 0, Stage: tpConsumptionBooked, UpdatedAt: now}},
	}
	st3 := &StrategyState{ID: first.ID, Positions: map[string]*Position{"ETH": pos3}}
	if !runPaperPostTPStopLossAdjustment(first, st3, "ETH", 2100, nil, &mu, nil, nil) || pos3.StopLossTriggerPx != 1960 {
		t.Fatalf("unmarked first rule trigger %v, want 1960", pos3.StopLossTriggerPx)
	}
}

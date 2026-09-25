package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func postTPSLTestStrategy(slAfter interface{}, tiers []interface{}) StrategyConfig {
	atrMult := 1.0
	params := map[string]interface{}{
		"tp_tiers": tiers,
	}
	if slAfter != nil {
		params["sl_after"] = slAfter
	}
	return StrategyConfig{
		ID:              "hl-sl-after",
		Platform:        "hyperliquid",
		Type:            "perps",
		Script:          "shared_scripts/check_hyperliquid.py",
		StopLossATRMult: &atrMult,
		CloseStrategy: &StrategyRef{
			Name:   "tiered_tp_atr_live",
			Params: params,
		},
	}
}

func TestRunPostTPStopLossAdjustment_CapsAtOnChainQty(t *testing.T) {
	old := runHyperliquidUpdateStopLossFunc
	defer func() { runHyperliquidUpdateStopLossFunc = old }()

	var gotQty float64
	runHyperliquidUpdateStopLossFunc = func(_, _, _ string, size, triggerPx float64, _ int64) (*HyperliquidStopLossUpdateResult, string, error) {
		gotQty = size
		return &HyperliquidStopLossUpdateResult{StopLossOID: 999, StopLossTriggerPx: triggerPx}, "", nil
	}

	sc := postTPSLTestStrategy("breakeven", []interface{}{
		map[string]interface{}{"atr_multiple": 2, "close_fraction": 0.5},
		map[string]interface{}{"atr_multiple": 3, "close_fraction": 1.0},
	})
	pos := &Position{
		Symbol: "ETH", Quantity: 1.0, InitialQuantity: 2.0,
		AvgCost: 100, EntryATR: 5, Side: "long",
		StopLossOID: 111, StopLossTriggerPx: 95,
		TPOIDs:                   []int64{0, 222},
		TPArmedTiers:             []bool{true, true},
		SLAdjustedTiersProcessed: 0,
	}
	state := &StrategyState{ID: sc.ID, Positions: map[string]*Position{"ETH": pos}}
	var mu sync.RWMutex

	onChain := map[string]float64{"ETH": 0.7}
	applied, _, _ := runPostTPStopLossAdjustment(sc, state, "ETH", 105, nil, &mu, nil, nil, onChain, nil, nil)
	if !applied {
		t.Fatal("expected runPostTPStopLossAdjustment to apply")
	}
	if gotQty != 0.7 {
		t.Fatalf("subprocess size=%v, want 0.7 (capped at on-chain)", gotQty)
	}
}

type slAfterParityPosition struct {
	Side                     string  `json:"side"`
	AvgCost                  float64 `json:"avg_cost"`
	EntryATR                 float64 `json:"entry_atr"`
	Quantity                 float64 `json:"quantity"`
	InitialQuantity          float64 `json:"initial_quantity"`
	StopLossTriggerPx        float64 `json:"stop_loss_trigger_px"`
	SLAdjustedTiersProcessed int     `json:"sl_adjusted_tiers_processed"`
	Regime                   string  `json:"regime"`
	StopLossHighWaterPx      float64 `json:"stop_loss_high_water_px"`
}

type slAfterParityWant struct {
	StopLossTriggerPx        float64  `json:"stop_loss_trigger_px"`
	SLAdjustedTiersProcessed int      `json:"sl_adjusted_tiers_processed"`
	PostTPTrailingATRMult    *float64 `json:"post_tp_trailing_atr_mult"`
	StopLossHighWaterPx      float64  `json:"stop_loss_high_water_px"`
}

type slAfterParityFixture struct {
	Ladders     map[string]json.RawMessage `json:"ladders"`
	ClearedTier []struct {
		Name        string  `json:"name"`
		Ladder      string  `json:"ladder"`
		Regime      string  `json:"regime"`
		ClosedRatio float64 `json:"closed_ratio"`
		FromIdx     int     `json:"from_idx"`
		WantIdx     int     `json:"want_idx"`
	} `json:"cleared_tier"`
	SLAfter []struct {
		Name            string                 `json:"name"`
		Ladder          string                 `json:"ladder"`
		StopLossATRMult float64                `json:"stop_loss_atr_mult"`
		SLAfter         interface{}            `json:"sl_after"`
		TierSLAfter     map[string]interface{} `json:"tier_sl_after"`
		Position        slAfterParityPosition  `json:"position"`
		Mark            float64                `json:"mark"`
		Want            slAfterParityWant      `json:"want"`
	} `json:"sl_after"`
}

func loadSLAfterParityFixture(t *testing.T) slAfterParityFixture {
	t.Helper()
	blob, err := os.ReadFile(filepath.Join("..", "backtest", "testdata", "sl_after_paper_parity.json"))
	if err != nil {
		t.Fatalf("read parity fixture: %v", err)
	}
	var f slAfterParityFixture
	if err := json.Unmarshal(blob, &f); err != nil {
		t.Fatalf("decode parity fixture: %v", err)
	}
	return f
}

func (f slAfterParityFixture) strategy(t *testing.T, ladder string, slMult float64, slAfter interface{}, tierSLAfter map[string]interface{}) StrategyConfig {
	t.Helper()
	raw, ok := f.Ladders[ladder]
	if !ok {
		t.Fatalf("parity fixture has no ladder %q", ladder)
	}
	var ref StrategyRef
	if err := json.Unmarshal(raw, &ref); err != nil {
		t.Fatalf("decode ladder %q: %v", ladder, err)
	}
	tiers, _ := ref.Params["tp_tiers"].([]interface{})
	for key, rule := range tierSLAfter {
		var idx int
		if err := json.Unmarshal([]byte(key), &idx); err != nil || idx < 0 || idx >= len(tiers) {
			t.Fatalf("ladder %q tier key %q is not a tier index", ladder, key)
		}
		tiers[idx].(map[string]interface{})["sl_after"] = rule
	}
	if slAfter != nil {
		ref.Params["sl_after"] = slAfter
	}
	return StrategyConfig{
		ID:              "hl-paper-sl-after",
		Platform:        "hyperliquid",
		Type:            "perps",
		Script:          "shared_scripts/check_hyperliquid.py",
		Args:            []string{"sma", "ETH", "1h"},
		StopLossATRMult: &slMult,
		CloseStrategy:   &ref,
	}
}

func TestPaperSLAfterClearedTier(t *testing.T) {
	f := loadSLAfterParityFixture(t)
	if len(f.ClearedTier) == 0 {
		t.Fatal("parity fixture has no cleared_tier cases")
	}
	for _, c := range f.ClearedTier {
		t.Run(c.Name, func(t *testing.T) {
			sc := f.strategy(t, c.Ladder, 1, nil, nil)
			idx, ok := findHighestClearedTierByClosedRatio(paperSLAfterTierThresholds(sc, c.Regime), c.ClosedRatio, c.FromIdx)
			got := -1
			if ok {
				got = idx
			}
			if got != c.WantIdx {
				t.Fatalf("cleared tier = %d, want %d", got, c.WantIdx)
			}
		})
	}
}

func TestRunPaperPostTPStopLossAdjustment(t *testing.T) {
	f := loadSLAfterParityFixture(t)
	if len(f.SLAfter) == 0 {
		t.Fatal("parity fixture has no sl_after cases")
	}
	type scope struct {
		name  string
		args  []string
		typ   string
		moves bool
	}
	scopes := []scope{
		{name: "paper perps", args: []string{"sma", "ETH", "1h"}, typ: "perps", moves: true},
		{name: "live perps", args: []string{"sma", "ETH", "1h", "--mode=live"}, typ: "perps"},
		{name: "paper manual", args: []string{"sma", "ETH", "1h"}, typ: "manual"},
	}
	for _, c := range f.SLAfter {
		for _, sp := range scopes {
			t.Run(c.Name+"/"+sp.name, func(t *testing.T) {
				sc := f.strategy(t, c.Ladder, c.StopLossATRMult, c.SLAfter, c.TierSLAfter)
				sc.Args = sp.args
				sc.Type = sp.typ
				p := c.Position
				pos := &Position{
					Symbol:                   "ETH",
					Side:                     p.Side,
					AvgCost:                  p.AvgCost,
					EntryATR:                 p.EntryATR,
					Quantity:                 p.Quantity,
					InitialQuantity:          p.InitialQuantity,
					StopLossTriggerPx:        p.StopLossTriggerPx,
					SLAdjustedTiersProcessed: p.SLAdjustedTiersProcessed,
					Regime:                   p.Regime,
					StopLossHighWaterPx:      p.StopLossHighWaterPx,
				}
				state := &StrategyState{ID: sc.ID, Positions: map[string]*Position{"ETH": pos}}
				var mu sync.RWMutex
				want := c.Want
				if !sp.moves {
					want = slAfterParityWant{StopLossTriggerPx: p.StopLossTriggerPx, SLAdjustedTiersProcessed: p.SLAdjustedTiersProcessed, StopLossHighWaterPx: p.StopLossHighWaterPx}
				}
				wantMoved := want.StopLossTriggerPx != p.StopLossTriggerPx || want.PostTPTrailingATRMult != nil
				assertState := func(call string) {
					t.Helper()
					gotTrail, wantTrail := 0.0, 0.0
					if pos.PostTPTrailingATRMult != nil {
						gotTrail = *pos.PostTPTrailingATRMult
					}
					if want.PostTPTrailingATRMult != nil {
						wantTrail = *want.PostTPTrailingATRMult
					}
					if !approxEq(pos.StopLossTriggerPx, want.StopLossTriggerPx) ||
						pos.SLAdjustedTiersProcessed != want.SLAdjustedTiersProcessed ||
						(pos.PostTPTrailingATRMult == nil) != (want.PostTPTrailingATRMult == nil) ||
						!approxEq(gotTrail, wantTrail) ||
						!approxEq(pos.StopLossHighWaterPx, want.StopLossHighWaterPx) {
						t.Fatalf("%s: trigger %v processed %d trail %v high water %v, want trigger %v processed %d trail %v high water %v",
							call, pos.StopLossTriggerPx, pos.SLAdjustedTiersProcessed, pos.PostTPTrailingATRMult, pos.StopLossHighWaterPx,
							want.StopLossTriggerPx, want.SLAdjustedTiersProcessed, want.PostTPTrailingATRMult, want.StopLossHighWaterPx)
					}
				}
				if got := runPaperPostTPStopLossAdjustment(sc, state, "ETH", c.Mark, nil, &mu, nil, nil); got != wantMoved {
					t.Fatalf("first call moved = %v, want %v", got, wantMoved)
				}
				assertState("first call")
				if runPaperPostTPStopLossAdjustment(sc, state, "ETH", c.Mark+1, nil, &mu, nil, nil) {
					t.Fatal("second call on the same tier moved the stop again")
				}
				assertState("second call")
			})
		}
	}
}

func TestPaperPartialCloseMovesStopBeforeNextBreach(t *testing.T) {
	prev := tradeRecorder
	tradeRecorder = nil
	t.Cleanup(func() { tradeRecorder = prev })
	cases := []struct {
		name          string
		slAfter       interface{}
		wantTrigger   float64
		walkMark      float64
		breachMark    float64
		wantBreachPx  float64
		wantTrailMult float64
	}{
		{name: "breakeven", slAfter: "breakeven", wantTrigger: 100, breachMark: 99.9, wantBreachPx: 99.9},
		{name: "trail from here", slAfter: map[string]interface{}{"trail_from_here": map[string]interface{}{"atr_mult": 1.0}}, wantTrigger: 100, walkMark: 110, breachMark: 107, wantBreachPx: 107, wantTrailMult: 1},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			slMult := 1.0
			sc := StrategyConfig{
				ID: "hl-paper-sl-after", Platform: "hyperliquid", Type: "perps",
				Args: []string{"sma", "ETH", "1h"}, StopLossATRMult: &slMult,
				Direction: DirectionBoth, Leverage: 1, SizingLeverage: 1,
				CloseStrategy: &StrategyRef{Name: "tiered_tp_atr", Params: map[string]interface{}{
					"sl_after": c.slAfter,
					"tp_tiers": []interface{}{
						map[string]interface{}{"atr_multiple": 1.0, "close_fraction": 0.5},
						map[string]interface{}{"atr_multiple": 2.0, "close_fraction": 1.0},
					},
				}},
			}
			s := paperStopTestState(sc, &Position{Symbol: "ETH", Quantity: 1, InitialQuantity: 1, AvgCost: 100, EntryATR: 2, Side: "long", StopLossTriggerPx: 98})
			logger := silentStrategyLogger(sc.ID)
			var mu sync.RWMutex
			result := &HyperliquidResult{Symbol: "ETH", Signal: -1, Price: 102}
			result.CloseFraction = 0.5
			if trades, _, _, _ := executeHyperliquidResultDeferredOpen(sc, s, result, nil, "SELL", 102, nil, &Config{}, HurstGateDecision{}, logger); trades != 1 {
				t.Fatalf("paper partial close trades = %d, want 1", trades)
			}
			if !runPaperPostTPStopLossAdjustment(sc, s, "ETH", 102, &Config{}, &mu, nil, logger) {
				t.Fatal("paper partial close did not move the stop")
			}
			pos := s.Positions["ETH"]
			if pos == nil || !approxEq(pos.Quantity, 0.5) || !approxEq(pos.StopLossTriggerPx, c.wantTrigger) {
				t.Fatalf("after the tier = %+v, want 0.5 left with the stop at %v", pos, c.wantTrigger)
			}
			if c.wantTrailMult > 0 {
				snap := hyperliquidProtectionPositionSnapshot(pos)
				if effectiveTrailingStopPct(sc, snap) <= 0 {
					t.Fatal("trail handoff: the fixed paper block still owns the stop, want the trailing walker")
				}
				hw, trigger, breach, _ := runHyperliquidTrailingStopPaper(sc, pos.Side, snap, c.walkMark, pos.StopLossHighWaterPx, pos.StopLossTriggerPx, trailingReplacePolicy{})
				if breach || hw != c.walkMark || trigger <= c.wantTrigger {
					t.Fatalf("walker from the seeded high water = (hw %v trigger %v breach %v), want hw %v and a trigger above %v", hw, trigger, breach, c.walkMark, c.wantTrigger)
				}
				pos.StopLossHighWaterPx, pos.StopLossTriggerPx = hw, trigger
			}
			n, _ := applyPaperStopLossBreach(sc, s, "ETH", "long", c.breachMark, &mu, logger)
			if n != 1 || s.Positions["ETH"] != nil || len(s.ClosedPositions) != 1 || !approxEq(s.ClosedPositions[0].ClosePrice, c.wantBreachPx) {
				t.Fatalf("next-cycle breach = trades %d closed %+v, want the rest closed @ %v", n, s.ClosedPositions, c.wantBreachPx)
			}
		})
	}
}

package main

import (
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"testing"
)

type tpTierParityLadder struct {
	Name   string                 `json:"name"`
	Params map[string]interface{} `json:"params"`
}

type tpTierParityFixture struct {
	Ladders    map[string]tpTierParityLadder `json:"ladders"`
	LadderRule []struct {
		ID          string        `json:"id"`
		Tiers       []interface{} `json:"tiers"`
		Want        [][2]float64  `json:"want"`
		WantResting bool          `json:"want_resting"`
	} `json:"ladder_rule"`
	Geometry []struct {
		ID             string                 `json:"id"`
		Ladder         string                 `json:"ladder"`
		ParamsOverride map[string]interface{} `json:"params_override"`
		Position       struct {
			Side               string  `json:"side"`
			AvgCost            float64 `json:"avg_cost"`
			RiskAnchorPrice    float64 `json:"risk_anchor_price"`
			EntryATR           float64 `json:"entry_atr"`
			Regime             string  `json:"regime"`
			RegimeAppliedLabel string  `json:"regime_applied_label"`
			Quantity           float64 `json:"quantity"`
			InitialQuantity    float64 `json:"initial_quantity"`
		} `json:"position"`
		Want struct {
			GoPlan     bool      `json:"go_plan"`
			Anchor     float64   `json:"anchor"`
			ATR        float64   `json:"atr"`
			Regime     string    `json:"regime"`
			TierPrices []float64 `json:"tier_prices"`
			Fractions  []float64 `json:"fractions"`
		} `json:"want"`
	} `json:"geometry"`
	LadderLoad []struct {
		ID         string        `json:"id"`
		Ladder     string        `json:"ladder"`
		Path       []interface{} `json:"path"`
		Value      interface{}   `json:"value"`
		WantReject bool          `json:"want_reject"`
	} `json:"ladder_load"`
}

func loadTPTierParityFixture(t *testing.T) tpTierParityFixture {
	t.Helper()
	blob, err := os.ReadFile(filepath.Join("..", "backtest", "testdata", "tp_tier_parity.json"))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	var f tpTierParityFixture
	if err := json.Unmarshal(blob, &f); err != nil {
		t.Fatalf("decode fixture: %v", err)
	}
	return f
}

func tpTierParityStrategy(name string, params map[string]interface{}) StrategyConfig {
	return StrategyConfig{
		ID:            "hl-tp-parity",
		Type:          "perps",
		Platform:      "hyperliquid",
		Args:          []string{"sma", "BTC", "1h", "--mode=paper"},
		CloseStrategy: &StrategyRef{Name: name, Params: params},
	}
}

func floatsNear(a, b []float64) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if math.Abs(a[i]-b[i]) > 1e-9 {
			return false
		}
	}
	return true
}

func TestTPTierParityFixtureMatchesTheOnChainPlan(t *testing.T) {
	f := loadTPTierParityFixture(t)
	for _, tc := range f.LadderRule {
		t.Run("ladder/"+tc.ID, func(t *testing.T) {
			sc := tpTierParityStrategy("tiered_tp_atr_live", map[string]interface{}{"tp_tiers": tc.Tiers})
			tiers := strategyTPTiers(sc)
			if !tc.WantResting {
				if tiers != nil {
					t.Fatalf("resting ladder = %+v, want none", tiers)
				}
				return
			}
			got := make([][2]float64, len(tiers))
			for i, tier := range tiers {
				got[i] = [2]float64{tier.Multiple, tier.Fraction}
			}
			if len(got) != len(tc.Want) {
				t.Fatalf("resting ladder = %v, want %v", got, tc.Want)
			}
			for i := range got {
				if !floatsNear(got[i][:], tc.Want[i][:]) {
					t.Fatalf("resting ladder = %v, want %v", got, tc.Want)
				}
			}
		})
	}
	for _, tc := range f.Geometry {
		t.Run("geometry/"+tc.ID, func(t *testing.T) {
			ref := f.Ladders[tc.Ladder]
			params := map[string]interface{}{}
			for k, v := range ref.Params {
				params[k] = v
			}
			for k, v := range tc.ParamsOverride {
				params[k] = v
			}
			sc := tpTierParityStrategy(ref.Name, params)
			p := tc.Position
			pos := &Position{
				Symbol: "BTC", Side: p.Side, AvgCost: p.AvgCost, RiskAnchorPrice: p.RiskAnchorPrice,
				EntryATR: p.EntryATR, Regime: p.Regime, RegimeAppliedLabel: p.RegimeAppliedLabel,
				Quantity: p.Quantity, InitialQuantity: p.InitialQuantity,
			}

			ctx := positionCtxForCheck(sc, pos, nil)
			if ctx.Regime != tc.Want.Regime {
				t.Fatalf("check position regime = %q, want %q", ctx.Regime, tc.Want.Regime)
			}
			if ctx.RiskAnchorPrice != p.RiskAnchorPrice {
				t.Fatalf("check risk anchor = %g, want %g", ctx.RiskAnchorPrice, p.RiskAnchorPrice)
			}

			plan, ok := buildHyperliquidProtectionPlan(sc, pos, 0)
			if ok != tc.Want.GoPlan {
				t.Fatalf("plan ok = %v, want %v", ok, tc.Want.GoPlan)
			}
			if !ok {
				return
			}
			if math.Abs(plan.AvgCost-tc.Want.Anchor) > 1e-9 || math.Abs(plan.EntryATR-tc.Want.ATR) > 1e-9 {
				t.Fatalf("plan anchor/atr = %g/%g, want %g/%g", plan.AvgCost, plan.EntryATR, tc.Want.Anchor, tc.Want.ATR)
			}
			fractions := make([]float64, len(plan.Tiers))
			for i, tier := range plan.Tiers {
				fractions[i] = tier.Fraction
			}
			if !floatsNear(fractions, tc.Want.Fractions) {
				t.Fatalf("plan fractions = %v, want %v", fractions, tc.Want.Fractions)
			}
			if prices := tieredTPATRPricesFromTiers(plan.Tiers, plan.Side, plan.AvgCost, plan.EntryATR); !floatsNear(prices, tc.Want.TierPrices) {
				t.Fatalf("plan tier prices = %v, want %v", prices, tc.Want.TierPrices)
			}
		})
	}
}

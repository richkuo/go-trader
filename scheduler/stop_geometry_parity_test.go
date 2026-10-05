package main

import (
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"sort"
	"testing"
)

const stopGeometryFixturePath = "../backtest/testdata/stop_geometry_parity.json"

type stopGeometryPosition struct {
	Side     string  `json:"side"`
	Anchor   float64 `json:"anchor"`
	EntryATR float64 `json:"entry_atr"`
	Regime   string  `json:"regime"`
}

type stopGeometryAdmissionCase struct {
	ID       string                 `json:"id"`
	Regime   bool                   `json:"regime"`
	Unified  bool                   `json:"unified"`
	Config   map[string]interface{} `json:"config"`
	Strategy map[string]interface{} `json:"strategy"`
	GoAccept bool                   `json:"go_accept"`
}

type stopGeometryCase struct {
	ID       string                 `json:"id"`
	Regime   bool                   `json:"regime"`
	Unified  bool                   `json:"unified"`
	Ratchet  bool                   `json:"ratchet"`
	Risk     bool                   `json:"risk"`
	Config   map[string]interface{} `json:"config"`
	Strategy map[string]interface{} `json:"strategy"`
	Position stopGeometryPosition   `json:"position"`
	Marks    []float64              `json:"marks"`
	PostTP   *struct {
		MarkIndex    int     `json:"mark_index"`
		TrailATRMult float64 `json:"trail_atr_mult"`
	} `json:"post_tp"`
	ScaleIn *struct {
		MarkIndex int     `json:"mark_index"`
		Price     float64 `json:"price"`
	} `json:"scale_in"`
}

type stopGeometryResolverCase struct {
	ID                          string  `json:"id"`
	BaseCase                    string  `json:"base_case"`
	InjectStopLossATRMult       float64 `json:"inject_stop_loss_atr_mult"`
	InjectStopLossATRMultRegime bool    `json:"inject_stop_loss_atr_mult_regime"`
	DropUnified                 bool    `json:"drop_unified"`
}

type stopGeometryStep struct {
	Mark      float64 `json:"mark"`
	HighWater float64 `json:"high_water"`
	Trigger   float64 `json:"trigger"`
	Replaced  bool    `json:"replaced"`
	Bypass    bool    `json:"bypass_min_move"`
}

type stopGeometryOutput struct {
	Resolved     map[string]interface{} `json:"resolved"`
	Owner        string                 `json:"owner"`
	TrailingPct  float64                `json:"trailing_pct"`
	FixedATRPct  float64                `json:"fixed_atr_pct"`
	PercentPct   float64                `json:"percent_pct"`
	MinMovePct   float64                `json:"min_move_pct"`
	ArmTrigger   float64                `json:"arm_trigger"`
	ArmHighWater float64                `json:"arm_high_water"`
	Path         []stopGeometryStep     `json:"path"`
	SLAfter      *stopGeometryStep      `json:"sl_after,omitempty"`
	RiskDistance *float64               `json:"risk_distance,omitempty"`
}

type stopGeometryExpected struct {
	Admission     map[string]bool               `json:"admission"`
	Geometry      map[string]stopGeometryOutput `json:"geometry"`
	ResolverOrder map[string]float64            `json:"resolver_order"`
}

type stopGeometryFixture struct {
	SchemaVersion int                         `json:"schema_version"`
	Tolerance     float64                     `json:"tolerance"`
	BaseConfig    map[string]interface{}      `json:"base_config"`
	BaseStrategy  map[string]interface{}      `json:"base_strategy"`
	RegimeEnabled map[string]interface{}      `json:"regime_enabled"`
	UnifiedClose  map[string]interface{}      `json:"unified_close"`
	Admission     []stopGeometryAdmissionCase `json:"admission"`
	Geometry      []stopGeometryCase          `json:"geometry"`
	ResolverOrder []stopGeometryResolverCase  `json:"resolver_order"`
	Expected      *stopGeometryExpected       `json:"expected"`
}

func loadStopGeometryFixture(t *testing.T) (stopGeometryFixture, map[string]json.RawMessage) {
	t.Helper()
	blob, err := os.ReadFile(stopGeometryFixturePath)
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	var f stopGeometryFixture
	if err := json.Unmarshal(blob, &f); err != nil {
		t.Fatalf("decode fixture: %v", err)
	}
	if f.SchemaVersion != 1 {
		t.Fatalf("fixture schema_version = %d, want 1", f.SchemaVersion)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(blob, &raw); err != nil {
		t.Fatalf("decode fixture sections: %v", err)
	}
	return f, raw
}

func cloneFixtureMap(t *testing.T, m map[string]interface{}) map[string]interface{} {
	t.Helper()
	out := map[string]interface{}{}
	if m == nil {
		return out
	}
	blob, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("clone: %v", err)
	}
	if err := json.Unmarshal(blob, &out); err != nil {
		t.Fatalf("clone: %v", err)
	}
	return out
}

func stopGeometryConfigJSON(t *testing.T, f stopGeometryFixture, regime, unified bool, config, strategy map[string]interface{}) string {
	t.Helper()
	cfg := cloneFixtureMap(t, f.BaseConfig)
	if regime {
		for k, v := range cloneFixtureMap(t, f.RegimeEnabled) {
			cfg[k] = v
		}
	}
	for k, v := range cloneFixtureMap(t, config) {
		cfg[k] = v
	}
	sc := cloneFixtureMap(t, f.BaseStrategy)
	if unified {
		sc["close_strategy"] = cloneFixtureMap(t, f.UnifiedClose)
	}
	for k, v := range cloneFixtureMap(t, strategy) {
		sc[k] = v
	}
	cfg["strategies"] = []interface{}{sc}
	blob, err := json.Marshal(cfg)
	if err != nil {
		t.Fatalf("encode config: %v", err)
	}
	return string(blob)
}

func loadStopGeometryStrategy(t *testing.T, cfgJSON string) (StrategyConfig, error) {
	t.Helper()
	if os.Getenv("HYPERLIQUID_SECRET_KEY") != "" {
		t.Fatalf("stop geometry fixtures must load without any Hyperliquid secret in the environment")
	}
	cfg, err := LoadConfigReadOnly(writeTestConfig(t, t.TempDir(), cfgJSON))
	if err != nil {
		return StrategyConfig{}, err
	}
	if len(cfg.Strategies) != 1 || hyperliquidIsLive(cfg.Strategies[0].Args) {
		t.Fatalf("fixture must load exactly one paper-mode strategy")
	}
	return cfg.Strategies[0], nil
}

func stopGeometryResolved(sc StrategyConfig) map[string]interface{} {
	ptr := func(v *float64) interface{} {
		if v == nil {
			return nil
		}
		return *v
	}
	return map[string]interface{}{
		"stop_loss_atr_mult":            ptr(sc.StopLossATRMult),
		"stop_loss_pct":                 ptr(sc.StopLossPct),
		"stop_loss_margin_pct":          ptr(sc.StopLossMarginPct),
		"trailing_stop_atr_mult":        ptr(sc.TrailingStopATRMult),
		"trailing_stop_pct":             ptr(sc.TrailingStopPct),
		"stop_loss_atr_mult_regime":     sc.StopLossATRMultRegime != nil && !sc.StopLossATRMultRegime.IsZero(),
		"trailing_stop_atr_mult_regime": sc.TrailingStopATRMultRegime != nil && !sc.TrailingStopATRMultRegime.IsZero(),
		"leverage":                      sc.Leverage,
		"max_drawdown_pct":              sc.MaxDrawdownPct,
		"trailing_stop_min_move_pct":    ptr(sc.TrailingStopMinMovePct),
	}
}

func runStopGeometryCase(t *testing.T, sc StrategyConfig, c stopGeometryCase) stopGeometryOutput {
	t.Helper()
	side := c.Position.Side
	anchor := c.Position.Anchor
	pos := &Position{
		Symbol:          "BTC",
		Side:            side,
		Quantity:        1,
		InitialQuantity: 1,
		AvgCost:         anchor,
		EntryATR:        c.Position.EntryATR,
		Regime:          c.Position.Regime,
	}
	out := stopGeometryOutput{
		Resolved:    stopGeometryResolved(sc),
		TrailingPct: effectiveTrailingStopPct(sc, pos),
		FixedATRPct: effectiveFixedStopLossATRPct(sc, pos),
		PercentPct:  EffectiveStopLossPct(sc),
		MinMovePct:  effectiveTrailingStopMinMovePct(sc),
		Path:        []stopGeometryStep{},
	}
	if c.Risk {
		dist, ok, reason := PerpsRiskStopDistance(sc, anchor, c.Position.EntryATR)
		if !ok {
			t.Fatalf("PerpsRiskStopDistance refused: %s", reason)
		}
		out.RiskDistance = &dist
	}
	trigger, highWater := 0.0, 0.0
	switch {
	case out.TrailingPct > 0:
		out.Owner = "trailing"
		highWater, trigger, _ = computeTrailingStopUpdateInternal(side, c.Marks[0], anchor, out.TrailingPct, out.MinMovePct, 0, false, false)
	case out.FixedATRPct > 0:
		out.Owner = "fixed_atr"
		trigger = fixedStopLossATRTriggerPx(sc, side, pos)
	case out.PercentPct > 0:
		out.Owner = "percent"
		trigger = percentStopLossTriggerPx(sc, side, pos.riskAnchorPrice())
	default:
		out.Owner = "none"
	}
	out.ArmTrigger, out.ArmHighWater = trigger, highWater
	for i, mark := range c.Marks {
		if c.ScaleIn != nil && c.ScaleIn.MarkIndex == i {
			pos.RiskAnchorPrice = anchor
			pos.AvgCost = (anchor + c.ScaleIn.Price) / 2
		}
		if c.PostTP != nil && c.PostTP.MarkIndex == i {
			rule := SLAfterRule{Kind: "trail_from_here", TrailATRMult: c.PostTP.TrailATRMult}
			seed, _, ok := computePostTPStopLossTrigger(rule, side, pos.riskAnchorPrice(), pos.EntryATR, mark)
			if !ok {
				t.Fatalf("computePostTPStopLossTrigger refused the post-TP seed")
			}
			mult := c.PostTP.TrailATRMult
			pos.PostTPTrailingATRMult = &mult
			trigger, highWater = seed, mark
			out.SLAfter = &stopGeometryStep{Mark: mark, HighWater: highWater, Trigger: trigger, Replaced: true}
			continue
		}
		bypass := false
		if c.Ratchet {
			bypass, _ = applyTrailingTPRatchetToPosition(sc, pos, "BTC", mark, nil)
		}
		pct := effectiveTrailingStopPct(sc, pos)
		if pct <= 0 {
			continue
		}
		hw := highWater
		if hw <= 0 {
			hw = pos.riskAnchorPrice()
		}
		newHW, candidate, replaced := computeTrailingStopUpdateInternal(side, mark, hw, pct, out.MinMovePct, trigger, false, bypass)
		highWater = newHW
		if replaced {
			trigger = candidate
		}
		out.Path = append(out.Path, stopGeometryStep{Mark: mark, HighWater: highWater, Trigger: trigger, Replaced: replaced, Bypass: bypass})
	}
	return out
}

func stopGeometryResolverEvidence(t *testing.T, f stopGeometryFixture, rc stopGeometryResolverCase) float64 {
	t.Helper()
	var base *stopGeometryCase
	for i := range f.Geometry {
		if f.Geometry[i].ID == rc.BaseCase {
			base = &f.Geometry[i]
		}
	}
	if base == nil {
		t.Fatalf("resolver case %s: unknown base case %s", rc.ID, rc.BaseCase)
	}
	sc, err := loadStopGeometryStrategy(t, stopGeometryConfigJSON(t, f, base.Regime, base.Unified, base.Config, base.Strategy))
	if err != nil {
		t.Fatalf("resolver base %s must be an accepted config: %v", rc.BaseCase, err)
	}
	mult := rc.InjectStopLossATRMult
	sc.StopLossATRMult = &mult
	if rc.InjectStopLossATRMultRegime {
		block := &RegimeATRBlock{raw: map[string]interface{}{"use_defaults": true}}
		if errs := block.ResolveSurface("resolver_evidence", regimeSurfaceStopLoss); len(errs) > 0 {
			t.Fatalf("resolver regime block: %v", errs)
		}
		sc.StopLossATRMultRegime = block
	}
	if rc.DropUnified {
		sc.CloseStrategy = nil
	}
	pos := &Position{Symbol: "BTC", Side: base.Position.Side, Quantity: 1, AvgCost: base.Position.Anchor, EntryATR: base.Position.EntryATR, Regime: base.Position.Regime}
	return effectiveFixedStopLossATRPct(sc, pos)
}

func computeStopGeometryExpected(t *testing.T, f stopGeometryFixture) stopGeometryExpected {
	t.Helper()
	exp := stopGeometryExpected{
		Admission:     map[string]bool{},
		Geometry:      map[string]stopGeometryOutput{},
		ResolverOrder: map[string]float64{},
	}
	for _, c := range f.Admission {
		_, err := loadStopGeometryStrategy(t, stopGeometryConfigJSON(t, f, c.Regime, c.Unified, c.Config, c.Strategy))
		exp.Admission[c.ID] = err == nil
	}
	for _, c := range f.Geometry {
		sc, err := loadStopGeometryStrategy(t, stopGeometryConfigJSON(t, f, c.Regime, c.Unified, c.Config, c.Strategy))
		if err != nil {
			t.Fatalf("geometry case %s must load: %v", c.ID, err)
		}
		exp.Geometry[c.ID] = runStopGeometryCase(t, sc, c)
	}
	for _, rc := range f.ResolverOrder {
		exp.ResolverOrder[rc.ID] = stopGeometryResolverEvidence(t, f, rc)
	}
	return exp
}

func stopGeometryNear(a, b, tol float64) bool {
	return math.Abs(a-b) <= tol*math.Max(1, math.Max(math.Abs(a), math.Abs(b)))
}

func compareStopGeometryJSON(t *testing.T, label string, got, want interface{}, tol float64) {
	t.Helper()
	switch w := want.(type) {
	case map[string]interface{}:
		g, ok := got.(map[string]interface{})
		if !ok || len(g) != len(w) {
			t.Errorf("%s: got %v, want %v", label, got, want)
			return
		}
		keys := make([]string, 0, len(w))
		for k := range w {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			compareStopGeometryJSON(t, label+"."+k, g[k], w[k], tol)
		}
	case []interface{}:
		g, ok := got.([]interface{})
		if !ok || len(g) != len(w) {
			t.Errorf("%s: got %v, want %v", label, got, want)
			return
		}
		for i := range w {
			compareStopGeometryJSON(t, label, g[i], w[i], tol)
		}
	case float64:
		g, ok := got.(float64)
		if !ok || math.IsNaN(g) || math.IsInf(g, 0) || !stopGeometryNear(g, w, tol) {
			t.Errorf("%s: got %v, want %v", label, got, want)
		}
	default:
		if got != want {
			t.Errorf("%s: got %v, want %v", label, got, want)
		}
	}
}

func TestStopGeometryParityFixture(t *testing.T) {
	f, raw := loadStopGeometryFixture(t)
	got := computeStopGeometryExpected(t, f)
	if os.Getenv("STOP_GEOMETRY_FIXTURE_UPDATE") == "1" {
		blob, err := json.Marshal(got)
		if err != nil {
			t.Fatalf("encode expected: %v", err)
		}
		raw["expected"] = blob
		out, err := json.MarshalIndent(raw, "", "  ")
		if err != nil {
			t.Fatalf("encode fixture: %v", err)
		}
		if err := os.WriteFile(filepath.Clean(stopGeometryFixturePath), append(out, '\n'), 0o644); err != nil {
			t.Fatalf("write fixture: %v", err)
		}
		t.Logf("rewrote %s expected section from the real Go functions", stopGeometryFixturePath)
		return
	}
	if f.Expected == nil {
		t.Fatalf("fixture has no expected section; regenerate with STOP_GEOMETRY_FIXTURE_UPDATE=1")
	}
	gotBlob, err := json.Marshal(got)
	if err != nil {
		t.Fatalf("encode got: %v", err)
	}
	var gotAny, wantAny interface{}
	if err := json.Unmarshal(gotBlob, &gotAny); err != nil {
		t.Fatalf("decode got: %v", err)
	}
	if err := json.Unmarshal(raw["expected"], &wantAny); err != nil {
		t.Fatalf("decode expected: %v", err)
	}
	compareStopGeometryJSON(t, "expected", gotAny, wantAny, f.Tolerance)
	for _, c := range f.Admission {
		if got.Admission[c.ID] != c.GoAccept {
			t.Errorf("admission %s: LoadConfigReadOnly accept=%v, fixture go_accept=%v", c.ID, got.Admission[c.ID], c.GoAccept)
		}
	}
}

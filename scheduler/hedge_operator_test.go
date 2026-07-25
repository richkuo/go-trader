package main

import (
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// Phase J — operator surfaces
// ---------------------------------------------------------------------------

func TestFormatStrategySummaryLine_Hedge(t *testing.T) {
	sc := hlHedgeTestStrategy("hl-eth", &HedgeConfig{Enabled: true, Symbol: "BTC"})
	line := formatStrategySummaryLine(sc, map[string]bool{}, &Config{})
	if !strings.Contains(line, "hedge=BTC×1.0(inverse,isolated,1.0x)") {
		t.Errorf("summary line missing resolved hedge geometry: %s", line)
	}

	sc2 := hlHedgeTestStrategy("hl-eth", &HedgeConfig{Enabled: true, Symbol: "BTC", Ratio: 2.5, MarginMode: "cross", Leverage: 3})
	line = formatStrategySummaryLine(sc2, map[string]bool{}, &Config{})
	if !strings.Contains(line, "hedge=BTC×2.5(inverse,cross,3.0x)") {
		t.Errorf("summary line missing configured hedge geometry: %s", line)
	}

	plain := formatStrategySummaryLine(hlHedgeTestStrategy("hl-eth", nil), map[string]bool{}, &Config{})
	if strings.Contains(plain, "hedge=") {
		t.Errorf("no hedge block → no hedge tag, got: %s", plain)
	}
	disabled := formatStrategySummaryLine(hlHedgeTestStrategy("hl-eth", &HedgeConfig{Enabled: false, Symbol: "BTC"}), map[string]bool{}, &Config{})
	if strings.Contains(disabled, "hedge=") {
		t.Errorf("disabled hedge block → no hedge tag, got: %s", disabled)
	}
}

func TestBuildStrategyInspectionJSON_HedgeKey(t *testing.T) {
	sc := hlHedgeTestStrategy("hl-eth", &HedgeConfig{Enabled: true, Symbol: "BTC", Ratio: 2, MarginMode: "cross", Leverage: 3})
	out := buildStrategyInspectionJSON(sc, nil, &Config{}, nil)
	raw, ok := out["hedge"]
	if !ok {
		t.Fatalf("inspect JSON missing hedge key: %v", out)
	}
	h, ok := raw.(map[string]interface{})
	if !ok {
		t.Fatalf("hedge key has unexpected shape: %T", raw)
	}
	if h["symbol"] != "BTC" || h["side"] != "inverse" || h["ratio"] != 2.0 || h["margin_mode"] != "cross" || h["leverage"] != 3.0 {
		t.Errorf("hedge JSON = %v", h)
	}

	// Resolved defaults when only the symbol is set.
	out = buildStrategyInspectionJSON(hlHedgeTestStrategy("hl-eth", &HedgeConfig{Enabled: true, Symbol: "BTC"}), nil, &Config{}, nil)
	h = out["hedge"].(map[string]interface{})
	if h["ratio"] != 1.0 || h["margin_mode"] != "isolated" || h["leverage"] != 1.0 {
		t.Errorf("resolved defaults wrong: %v", h)
	}

	// No block (or disabled) → key omitted.
	if _, ok := buildStrategyInspectionJSON(hlHedgeTestStrategy("hl-eth", nil), nil, &Config{}, nil)["hedge"]; ok {
		t.Error("no hedge block → hedge key must be omitted")
	}
	if _, ok := buildStrategyInspectionJSON(hlHedgeTestStrategy("hl-eth", &HedgeConfig{Enabled: false, Symbol: "BTC"}), nil, &Config{}, nil)["hedge"]; ok {
		t.Error("disabled hedge block → hedge key must be omitted")
	}
}

func TestHedgeStatusForStrategy(t *testing.T) {
	sc := hlHedgeTestStrategy("hl-eth", &HedgeConfig{Enabled: true, Symbol: "BTC"})

	// No hedge block → nil (field omitted from /status JSON).
	if got := hedgeStatusForStrategy(hlHedgeTestStrategy("hl-eth", nil), hedgeTestStrategyState()); got != nil {
		t.Errorf("no hedge block → nil HedgeStatus, got %+v", got)
	}

	// Flat: configured geometry only, no held fields.
	flat := hedgeTestStrategyState()
	delete(flat.Positions, "BTC")
	got := hedgeStatusForStrategy(sc, flat)
	if got == nil || got.Symbol != "BTC" || got.Side != "inverse" || got.Ratio != 1.0 {
		t.Fatalf("flat HedgeStatus = %+v", got)
	}
	if got.HeldQty != 0 || got.HeldSide != "" || got.HedgeFor != "" || got.PrimaryQtyBasis != 0 {
		t.Errorf("flat HedgeStatus must not carry held-leg fields: %+v", got)
	}

	// Held leg: qty/side/basis/coupling populated from the persisted stamp.
	got = hedgeStatusForStrategy(sc, hedgeTestStrategyState())
	if got.HeldQty != 0.05 || got.HeldSide != "short" || got.HedgeFor != "ETH" || got.PrimaryQtyBasis != 1.5 {
		t.Errorf("held HedgeStatus = %+v", got)
	}

	// Unstamped position on the hedge coin is NOT reported as a held leg.
	unstamped := hedgeTestStrategyState()
	unstamped.Positions["BTC"].HedgeFor = ""
	got = hedgeStatusForStrategy(sc, unstamped)
	if got.HeldQty != 0 || got.HedgeFor != "" {
		t.Errorf("unstamped position must not surface as a held hedge leg: %+v", got)
	}
}

func TestHedgeStatusNote(t *testing.T) {
	// No hedge strategies → empty.
	plain := hlHedgeTestStrategy("hl-zzz", nil)
	if got := hedgeStatusNote([]StrategyConfig{plain}, NewAppState()); got != "" {
		t.Errorf("no hedge blocks → empty note, got %q", got)
	}

	hedged := hlHedgeTestStrategy("hl-eth", &HedgeConfig{Enabled: true, Symbol: "BTC"})
	hedged2 := hlHedgeTestStrategy("hl-aaa", &HedgeConfig{Enabled: true, Symbol: "SOL", Ratio: 0.5})
	hedged2.Args = []string{"sma_crossover", "ETH", "1h"}

	state := NewAppState()
	state.Strategies["hl-eth"] = hedgeTestStrategyState()

	note := hedgeStatusNote([]StrategyConfig{hedged, hedged2, plain}, state)
	if !strings.HasPrefix(note, "\n") || !strings.Contains(note, "hedge: ") {
		t.Fatalf("note shape: %q", note)
	}
	// Sorted by strategy ID: hl-aaa before hl-eth.
	aaaIdx := strings.Index(note, "hl-aaa→SOL×0.5")
	ethIdx := strings.Index(note, "hl-eth→BTC×1.0")
	if aaaIdx < 0 || ethIdx < 0 || aaaIdx > ethIdx {
		t.Errorf("note must list sorted strategy IDs with coin×ratio: %q", note)
	}
	// hl-eth holds its leg; hl-aaa is flat.
	if !strings.Contains(note, "short 0.05 held for ETH, basis 1.5") {
		t.Errorf("held-leg annotation missing: %q", note)
	}
	if !strings.Contains(note, "hl-aaa→SOL×0.5 (flat)") {
		t.Errorf("flat annotation missing: %q", note)
	}

	// Nil state → flat annotations, no panic.
	note = hedgeStatusNote([]StrategyConfig{hedged}, nil)
	if !strings.Contains(note, "hl-eth→BTC×1.0 (flat)") {
		t.Errorf("nil state should degrade to flat annotations: %q", note)
	}
}

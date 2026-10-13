package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

var tsiFrozenLadder = []interface{}{
	map[string]interface{}{"profit_pct": 0.01, "close_fraction": 0.25},
	map[string]interface{}{"profit_pct": 0.02, "close_fraction": 0.5},
	map[string]interface{}{"profit_pct": 0.03, "close_fraction": 0.75},
	map[string]interface{}{"profit_pct": 0.04, "close_fraction": 1.0},
}

const tsiName = "true_strength_index"

func tsiLoadBoth(t *testing.T, sc map[string]interface{}) (*Config, error) {
	t.Helper()
	path := writeNoEdgeConfig(t, CurrentConfigVersion, sc)
	cfg, loadErr := LoadConfig(path)
	_, probeErr := LoadConfigForProbe(path)
	if (loadErr == nil) != (probeErr == nil) {
		t.Fatalf("LoadConfig err=%v but LoadConfigForProbe err=%v", loadErr, probeErr)
	}
	return cfg, loadErr
}

func TestTrueStrengthIndexAdmissionRoutesOpenAndCloseFallbackReferences(t *testing.T) {
	setNoEdgeLiveCredentials(t)
	open := func(args ...string) map[string]interface{} {
		return noEdgeHLStrategy("hl-tsi-eth", append([]string{tsiName, "ETH", "1h"}, args...)...)
	}
	fallback := func(args ...string) map[string]interface{} {
		sc := noEdgeHLStrategy("hl-bo-eth", append([]string{"breakout", "ETH", "1h"}, args...)...)
		sc["close_strategy"] = map[string]interface{}{"name": tsiName}
		return sc
	}
	paper, err := tsiLoadBoth(t, open("--mode=paper"))
	if err != nil {
		t.Fatalf("explicit paper must load without acknowledgement: %v", err)
	}
	if got := portfolioScopeFor(paper.Strategies[0]); got != ScopePaper {
		t.Fatalf("scope = %q", got)
	}
	if _, err := tsiLoadBoth(t, open()); err == nil || !strings.Contains(err.Error(), "missing --mode") {
		t.Fatalf("missing mode must be refused, got %v", err)
	}
	if _, err := tsiLoadBoth(t, open("--mode=live")); err == nil || !strings.Contains(err.Error(), `open strategy "`+tsiName+`"`) {
		t.Fatalf("unacknowledged live open must be refused, got %v", err)
	}
	_, liveErr := tsiLoadBoth(t, fallback("--mode=live"))
	if liveErr == nil || !strings.Contains(liveErr.Error(), `close strategy "`+tsiName+`"`) {
		t.Fatalf("unacknowledged live close fallback must be refused, got %v", liveErr)
	}
	acked := open("--mode=live")
	acked["allow_no_edge"] = true
	if _, err := tsiLoadBoth(t, acked); err != nil {
		t.Fatalf("acknowledged live use must stay available: %v", err)
	}
	cfg, err := tsiLoadBoth(t, fallback("--mode=paper"))
	if err != nil {
		t.Fatalf("paper close fallback must load: %v", err)
	}
	refs := noEdgeReferences(cfg.Strategies[0])
	if len(refs) != 1 || refs[0] != (edgeReference{Role: edgeRefRoleClose, Name: tsiName}) {
		t.Fatalf("no-edge references = %+v", refs)
	}
}

func TestTrueStrengthIndexOperatorAddCreatesALoadableExplicitPaperEntry(t *testing.T) {
	setNoEdgeLiveCredentials(t)
	id, entry, err := buildAddStrategyEntry(tsiName, "hyperliquid", "sol")
	if err != nil || id != "hl-tsi-sol" {
		t.Fatalf("build = %q, %v", id, err)
	}
	var raw map[string]interface{}
	if err := json.Unmarshal(entry, &raw); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(raw["args"], []interface{}{tsiName, "SOL", "1h", "--mode=paper"}) {
		t.Fatalf("args = %v", raw["args"])
	}
	wantClose := map[string]interface{}{"name": "tiered_tp_pct", "params": map[string]interface{}{"tp_tiers": tsiFrozenLadder}}
	if !reflect.DeepEqual(raw["close_strategy"], wantClose) {
		t.Fatalf("close_strategy = %v", raw["close_strategy"])
	}
	if raw["closed_bar_decisions"] != true || raw["stop_loss_atr_mult"] != 1.0 || raw["direction"] != DirectionBoth {
		t.Fatalf("entry = %v", raw)
	}
	if _, has := raw["allow_no_edge"]; has {
		t.Fatal("paper add must not write an acknowledgement")
	}
	if _, _, err := buildAddStrategyEntry(tsiName, "binanceus", "BTC"); err == nil {
		t.Fatal("futures-only candidate was accepted for binanceus")
	}
	path := writeNoEdgeConfig(t, CurrentConfigVersion, raw)
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("added paper entry must load without acknowledgement: %v", err)
	}
	if _, err := LoadConfigForProbe(path); err != nil {
		t.Fatalf("added paper entry must pass the probe loader: %v", err)
	}
	sc := cfg.Strategies[0]
	if sc.AllowNoEdge != nil || portfolioScopeFor(sc) != ScopePaper || !sc.ClosedBarDecisions || sc.Direction != DirectionBoth {
		t.Fatalf("%s must be explicit closed-bar paper in both directions with no acknowledgement", sc.ID)
	}
	refs := sc.closeRefs()
	if len(refs) != 1 || refs[0].Name != "tiered_tp_pct" {
		t.Fatalf("close owners = %+v", refs)
	}
	if sc.StopLossATRMult == nil || *sc.StopLossATRMult != 1.0 || sc.StopLossPct != nil || sc.TrailingStopATRMult != nil {
		t.Fatalf("stop owner = atr %v pct %v trailing %v", sc.StopLossATRMult, sc.StopLossPct, sc.TrailingStopATRMult)
	}
}

func TestTrueStrengthIndexStudyParityConfigsLoadAsExplicitPaper(t *testing.T) {
	dir := filepath.Join("..", "backtest", "candidates", "true_strength_index_1667")
	for _, tf := range []string{"1h", "4h"} {
		data, err := os.ReadFile(filepath.Join(dir, "paper_parity_"+tf+".json"))
		if err != nil {
			t.Fatal(err)
		}
		var root map[string]interface{}
		if err := json.Unmarshal(data, &root); err != nil {
			t.Fatal(err)
		}
		root["db_file"] = filepath.Join(t.TempDir(), "state.db")
		blob, _ := json.Marshal(root)
		path := filepath.Join(t.TempDir(), "config.json")
		if err := os.WriteFile(path, blob, 0600); err != nil {
			t.Fatal(err)
		}
		cfg, err := LoadConfig(path)
		if err != nil {
			t.Fatalf("%s parity config must load: %v", tf, err)
		}
		sc := cfg.Strategies[0]
		if portfolioScopeFor(sc) != ScopePaper || sc.AllowNoEdge != nil || !sc.ClosedBarDecisions || sc.Direction != DirectionBoth {
			t.Fatalf("%s parity config must be explicit closed-bar paper in both directions", tf)
		}
		wantParams := map[string]interface{}{"slow_period": 25.0, "fast_period": 13.0, "signal_period": 7.0, "recursion_length": 180.0}
		if sc.OpenStrategy.Name != tsiName || !reflect.DeepEqual(sc.OpenStrategy.Params, wantParams) {
			t.Fatalf("%s open strategy = %+v", tf, sc.OpenStrategy)
		}
		refs := sc.closeRefs()
		if len(refs) != 1 || refs[0].Name != "tiered_tp_pct" || !reflect.DeepEqual(refs[0].Params["tp_tiers"], tsiFrozenLadder) {
			t.Fatalf("%s close owners = %+v, want the frozen tiered_tp_pct ladder", tf, refs)
		}
		if sc.StopLossATRMult == nil || *sc.StopLossATRMult != 1.0 || sc.StopLossPct != nil || sc.TrailingStopATRMult != nil {
			t.Fatalf("%s stop owner = atr %v pct %v trailing %v", tf, sc.StopLossATRMult, sc.StopLossPct, sc.TrailingStopATRMult)
		}
	}
}

func TestV20NoEdgeMigrationNeverStampsTrueStrengthIndex(t *testing.T) {
	open := noEdgeHLStrategy("hl-tsi-open", tsiName, "ETH", "1h", "--mode=live")
	fallback := noEdgeHLStrategy("hl-tsi-close", "breakout", "ETH", "1h", "--mode=live")
	fallback["close_strategy"] = map[string]interface{}{"name": tsiName}
	control := noEdgeHLStrategy("hl-control", "rsi", "BTC", "1h", "--mode=live")
	raw := map[string]interface{}{"strategies": []interface{}{}}
	for _, sc := range []map[string]interface{}{open, fallback, control} {
		blob, _ := json.Marshal(sc)
		var generic map[string]interface{}
		_ = json.Unmarshal(blob, &generic)
		raw["strategies"] = append(raw["strategies"].([]interface{}), generic)
	}
	report, err := migrateV20NoEdge(raw, 19)
	if err != nil {
		t.Fatal(err)
	}
	if got := stampedIDs(report); !reflect.DeepEqual(got, []string{"hl-control"}) {
		t.Fatalf("stamped = %v, want only the non-candidate control", got)
	}
	if want := []string{"hl-tsi-close", "hl-tsi-open"}; !reflect.DeepEqual(report.VetoedResearch, want) {
		t.Fatalf("vetoed = %v, want %v", report.VetoedResearch, want)
	}
}

package main

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

const (
	elderForceName = "elder_force_pullback"
	elderForceRef  = "backtest/candidates/elder_force_pullback_1668/REPORT.md"
)

func elderForceLoadBoth(t *testing.T, sc map[string]interface{}) (*Config, error) {
	t.Helper()
	path := writeNoEdgeConfig(t, CurrentConfigVersion, sc)
	cfg, loadErr := LoadConfig(path)
	_, probeErr := LoadConfigForProbe(path)
	if (loadErr == nil) != (probeErr == nil) {
		t.Fatalf("LoadConfig err=%v but LoadConfigForProbe err=%v", loadErr, probeErr)
	}
	return cfg, loadErr
}

func TestElderForcePullbackRegistrationTables(t *testing.T) {
	if knownShortNames[elderForceName] != "efp" || !isBidirectionalPerpsStrategy(elderForceName) {
		t.Fatalf("short name %q, bidirectional %v", knownShortNames[elderForceName], isBidirectionalPerpsStrategy(elderForceName))
	}
	if !openStrategyRegisteredFor(elderForceName, "futures") || openStrategyRegisteredFor(elderForceName, "spot") {
		t.Fatal("elder_force_pullback must be registered for futures only")
	}
	var owners []string
	for other, short := range knownShortNames {
		if short == "efp" {
			owners = append(owners, other)
		}
	}
	if !reflect.DeepEqual(owners, []string{elderForceName}) {
		t.Fatalf("short name efp owners = %v", owners)
	}
	ev, ok := noEdgeEvidenceFor(elderForceName)
	if !ok || ev.Ref != elderForceRef {
		t.Fatalf("no-edge evidence = %+v, %v", ev, ok)
	}
	if _, frozen := v20StampEligibleStrategies[elderForceName]; frozen {
		t.Fatal("elder_force_pullback must not join the frozen v20 auto-acknowledgement set")
	}
	if _, ok := fixedIntervalStrategyDefaults[elderForceName]; ok {
		t.Fatal("elder_force_pullback reads the declared frame cadence and has no fixed-interval parameter")
	}
}

func TestElderForcePullbackAdmissionRoutesOpenAndCloseFallbackReferences(t *testing.T) {
	setNoEdgeLiveCredentials(t)
	open := func(args ...string) map[string]interface{} {
		return noEdgeHLStrategy("hl-efp-eth", append([]string{elderForceName, "ETH", "1h"}, args...)...)
	}
	fallback := func(args ...string) map[string]interface{} {
		sc := noEdgeHLStrategy("hl-bo-eth", append([]string{"breakout", "ETH", "1h"}, args...)...)
		sc["close_strategy"] = map[string]interface{}{"name": elderForceName}
		return sc
	}
	paper, err := elderForceLoadBoth(t, open("--mode=paper"))
	if err != nil {
		t.Fatalf("explicit paper must load without acknowledgement: %v", err)
	}
	if got := portfolioScopeFor(paper.Strategies[0]); got != ScopePaper {
		t.Fatalf("scope = %q", got)
	}
	if _, err := elderForceLoadBoth(t, open()); err == nil || !strings.Contains(err.Error(), "missing --mode") {
		t.Fatalf("missing mode must be refused, got %v", err)
	}
	if _, err := elderForceLoadBoth(t, open("--mode=live")); err == nil || !strings.Contains(err.Error(), `open strategy "`+elderForceName+`"`) {
		t.Fatalf("unacknowledged live open must be refused, got %v", err)
	}
	_, liveErr := elderForceLoadBoth(t, fallback("--mode=live"))
	if liveErr == nil || !strings.Contains(liveErr.Error(), `close strategy "`+elderForceName+`"`) {
		t.Fatalf("unacknowledged live close fallback must be refused, got %v", liveErr)
	}
	acked := open("--mode=live")
	acked["allow_no_edge"] = true
	if _, err := elderForceLoadBoth(t, acked); err != nil {
		t.Fatalf("acknowledged live use must stay available: %v", err)
	}
	cfg, err := elderForceLoadBoth(t, fallback("--mode=paper"))
	if err != nil {
		t.Fatalf("paper close fallback must load: %v", err)
	}
	refs := noEdgeReferences(cfg.Strategies[0])
	if len(refs) != 1 || refs[0] != (edgeReference{Role: edgeRefRoleClose, Name: elderForceName}) {
		t.Fatalf("no-edge references = %+v", refs)
	}
}

func TestElderForcePullbackOperatorAddCreatesALoadableExplicitPaperEntry(t *testing.T) {
	setNoEdgeLiveCredentials(t)
	id, entry, err := buildAddStrategyEntry(elderForceName, "hyperliquid", "sol")
	if err != nil || id != "hl-efp-sol" {
		t.Fatalf("build = %q, %v", id, err)
	}
	var raw map[string]interface{}
	if err := json.Unmarshal(entry, &raw); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(raw["args"], []interface{}{elderForceName, "SOL", "1h", "--mode=paper"}) {
		t.Fatalf("args = %v", raw["args"])
	}
	wantClose := map[string]interface{}{"name": "tiered_tp_pct", "params": map[string]interface{}{
		"tp_tiers": []interface{}{
			map[string]interface{}{"profit_pct": 0.01, "close_fraction": 0.25},
			map[string]interface{}{"profit_pct": 0.02, "close_fraction": 0.5},
			map[string]interface{}{"profit_pct": 0.03, "close_fraction": 0.75},
			map[string]interface{}{"profit_pct": 0.04, "close_fraction": 1.0},
		}}}
	if !reflect.DeepEqual(raw["close_strategy"], wantClose) {
		t.Fatalf("close_strategy = %v", raw["close_strategy"])
	}
	if raw["closed_bar_decisions"] != true || raw["stop_loss_atr_mult"] != 1.0 || raw["direction"] != DirectionBoth {
		t.Fatalf("entry = %v", raw)
	}
	if _, has := raw["allow_no_edge"]; has {
		t.Fatal("paper add must not write an acknowledgement")
	}
	if _, _, err := buildAddStrategyEntry(elderForceName, "binanceus", "BTC"); err == nil {
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

func TestV20NoEdgeMigrationNeverStampsElderForcePullback(t *testing.T) {
	open := noEdgeHLStrategy("hl-efp-open", elderForceName, "ETH", "1h", "--mode=live")
	fallback := noEdgeHLStrategy("hl-efp-close", "breakout", "ETH", "1h", "--mode=live")
	fallback["close_strategy"] = map[string]interface{}{"name": elderForceName}
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
	if want := []string{"hl-efp-close", "hl-efp-open"}; !reflect.DeepEqual(report.VetoedResearch, want) {
		t.Fatalf("vetoed = %v, want %v", report.VetoedResearch, want)
	}
}

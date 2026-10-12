package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

const (
	calendarEntryName = "calendar_conditioned_entry"
	calendarEntryRef  = "backtest/candidates/calendar_conditioned_entry_1653/REPORT.md"
)

func calendarHL(id, timeframe string, args ...string) map[string]interface{} {
	sc := noEdgeHLStrategy(id, append([]string{calendarEntryName, "ETH", timeframe}, args...)...)
	sc["closed_bar_decisions"] = true
	return sc
}

func calendarLoad(t *testing.T, sc map[string]interface{}) (*Config, error) {
	t.Helper()
	path := writeNoEdgeConfig(t, CurrentConfigVersion, sc)
	readOnly, readErr := LoadConfigReadOnly(path)
	cfg, loadErr := LoadConfig(path)
	if (readErr == nil) != (loadErr == nil) {
		t.Fatalf("LoadConfigReadOnly err=%v but LoadConfig err=%v", readErr, loadErr)
	}
	if readErr == nil && len(readOnly.Strategies) != len(cfg.Strategies) {
		t.Fatalf("read-only loader kept %d strategies, LoadConfig %d", len(readOnly.Strategies), len(cfg.Strategies))
	}
	return cfg, loadErr
}

func TestCalendarConditionedEntryRegistrationTables(t *testing.T) {
	if knownShortNames[calendarEntryName] != "cce" || !isBidirectionalPerpsStrategy(calendarEntryName) {
		t.Fatalf("short name %q, bidirectional %v", knownShortNames[calendarEntryName], isBidirectionalPerpsStrategy(calendarEntryName))
	}
	if !openStrategyRegisteredFor(calendarEntryName, "futures") || openStrategyRegisteredFor(calendarEntryName, "spot") {
		t.Fatal("calendar_conditioned_entry must be registered for futures only")
	}
	var owners []string
	for other, short := range knownShortNames {
		if short == "cce" {
			owners = append(owners, other)
		}
	}
	if !reflect.DeepEqual(owners, []string{calendarEntryName}) {
		t.Fatalf("short name cce owners = %v", owners)
	}
	ev, ok := noEdgeEvidenceFor(calendarEntryName)
	if !ok || ev.Ref != calendarEntryRef {
		t.Fatalf("no-edge evidence = %+v, %v", ev, ok)
	}
	if fixedIntervalStrategyDefaults[calendarEntryName] != 3_600_000 {
		t.Fatalf("fixed-interval default = %d", fixedIntervalStrategyDefaults[calendarEntryName])
	}
	if _, ok := closedBarRequiredOpenStrategies[calendarEntryName]; !ok {
		t.Fatal("calendar_conditioned_entry must require Hyperliquid closed-bar decisions")
	}
	if _, frozen := v20StampEligibleStrategies[calendarEntryName]; frozen {
		t.Fatal("calendar_conditioned_entry must not join the frozen v20 auto-acknowledgement set")
	}
}

func TestCalendarConditionedEntryAdmissionThroughTheConfigLoaders(t *testing.T) {
	setNoEdgeLiveCredentials(t)
	paper, err := calendarLoad(t, calendarHL("hl-cce-eth", "1h", "--mode=paper"))
	if err != nil {
		t.Fatalf("explicit paper must load without acknowledgement: %v", err)
	}
	if sc := paper.Strategies[0]; portfolioScopeFor(sc) != ScopePaper || sc.AllowNoEdge != nil {
		t.Fatalf("scope = %q, allow_no_edge = %v", portfolioScopeFor(sc), sc.AllowNoEdge)
	}
	if _, err := calendarLoad(t, calendarHL("hl-cce-eth", "1h")); err == nil || !strings.Contains(err.Error(), `open strategy "`+calendarEntryName+`" is edge_status=no_edge`) {
		t.Fatalf("missing mode must be refused for the open reference, got %v", err)
	}
	fallback := func(args ...string) map[string]interface{} {
		sc := noEdgeHLStrategy("hl-bo-eth", append([]string{"breakout", "ETH", "1h"}, args...)...)
		sc["closed_bar_decisions"] = true
		sc["close_strategy"] = map[string]interface{}{"name": calendarEntryName}
		return sc
	}
	if _, err := calendarLoad(t, fallback("--mode=live")); err == nil || !strings.Contains(err.Error(), `close strategy "`+calendarEntryName+`" is edge_status=no_edge`) {
		t.Fatalf("unacknowledged live close fallback must be refused, got %v", err)
	}
	cfg, err := calendarLoad(t, fallback("--mode=paper"))
	if err != nil {
		t.Fatalf("paper close fallback must load: %v", err)
	}
	refs := noEdgeReferences(cfg.Strategies[0])
	if len(refs) != 1 || refs[0] != (edgeReference{Role: edgeRefRoleClose, Name: calendarEntryName}) {
		t.Fatalf("no-edge references = %+v", refs)
	}
	acked := calendarHL("hl-cce-eth", "1h", "--mode=live")
	acked["allow_no_edge"] = true
	live, err := calendarLoad(t, acked)
	if err != nil {
		t.Fatalf("an acknowledged live reference keeps the shared admission rule: %v", err)
	}
	if sc := live.Strategies[0]; !sc.AllowNoEdgeAcknowledged() || portfolioScopeFor(sc) != ScopeLive {
		t.Fatalf("acknowledged live = %v scope %q", sc.AllowNoEdgeAcknowledged(), portfolioScopeFor(sc))
	}
}

func TestCalendarConditionedEntryNeedsClosedBarsAndTheMatchingCadence(t *testing.T) {
	setNoEdgeLiveCredentials(t)
	off := calendarHL("hl-cce-eth", "1h", "--mode=paper")
	off["closed_bar_decisions"] = false
	if _, err := calendarLoad(t, off); err == nil || !strings.Contains(err.Error(), `runs only on a Hyperliquid perps strategy with "closed_bar_decisions": true`) {
		t.Fatalf("an open reference without closed-bar decisions must be refused, got %v", err)
	}
	if _, err := calendarLoad(t, calendarHL("hl-cce-eth", "4h", "--mode=paper")); err == nil || !strings.Contains(err.Error(), "registry default expected_interval_ms=3600000 does not match timeframe 4h") {
		t.Fatalf("the 1h registry default on a 4h strategy must be refused, got %v", err)
	}
	four := calendarHL("hl-cce-eth", "4h", "--mode=paper")
	four["open_strategy"] = map[string]interface{}{"name": calendarEntryName, "params": map[string]interface{}{"expected_interval_ms": 14_400_000}}
	if _, err := calendarLoad(t, four); err != nil {
		t.Fatalf("an explicit 4h cadence on a 4h strategy must load: %v", err)
	}
}

func TestCalendarConditionedEntryStudyParityConfigsLoadAsExplicitPaper(t *testing.T) {
	dir := filepath.Join("..", "backtest", "candidates", "calendar_conditioned_entry_1653")
	for tf, want := range map[string]struct {
		interval int64
		horizon  float64
	}{"1h": {3_600_000, 4}, "4h": {14_400_000, 1}} {
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
		cfg, err := LoadConfigReadOnly(path)
		if err != nil {
			t.Fatalf("%s parity config must load: %v", tf, err)
		}
		sc := cfg.Strategies[0]
		got, explicit, err := fixedIntervalParamValue(sc.OpenStrategy.Params)
		if err != nil || !explicit || got != want.interval {
			t.Fatalf("%s cadence = %d explicit=%v err=%v", tf, got, explicit, err)
		}
		if portfolioScopeFor(sc) != ScopePaper || sc.AllowNoEdge != nil || !sc.ClosedBarDecisions || sc.Direction != DirectionBoth {
			t.Fatalf("%s parity config must be explicit closed-bar paper in both directions", tf)
		}
		refs := sc.closeRefs()
		if len(refs) != 1 || refs[0].Name != "time_stop" || refs[0].Params["max_bars"] != want.horizon {
			t.Fatalf("%s close owners = %+v", tf, refs)
		}
		if sc.StopLossATRMult == nil || *sc.StopLossATRMult != 3.0 || sc.StopLossPct != nil || sc.TrailingStopATRMult != nil {
			t.Fatalf("%s stop owner = atr %v pct %v trailing %v", tf, sc.StopLossATRMult, sc.StopLossPct, sc.TrailingStopATRMult)
		}
	}
}

func TestV20NoEdgeMigrationNeverStampsCalendarConditionedEntry(t *testing.T) {
	open := noEdgeHLStrategy("hl-cce-open", calendarEntryName, "ETH", "1h", "--mode=live")
	fallback := noEdgeHLStrategy("hl-cce-close", "breakout", "ETH", "1h", "--mode=live")
	fallback["close_strategy"] = map[string]interface{}{"name": calendarEntryName}
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
	if want := []string{"hl-cce-close", "hl-cce-open"}; !reflect.DeepEqual(report.VetoedResearch, want) {
		t.Fatalf("vetoed = %v, want %v", report.VetoedResearch, want)
	}
}

func TestCalendarConditionedEntryOperatorAddCreatesALoadableExplicitPaperEntry(t *testing.T) {
	setNoEdgeLiveCredentials(t)
	id, entry, err := buildAddStrategyEntry(calendarEntryName, "hyperliquid", "eth")
	if err != nil || id != "hl-cce-eth" {
		t.Fatalf("build = %q, %v", id, err)
	}
	var raw map[string]interface{}
	if err := json.Unmarshal(entry, &raw); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(raw["args"], []interface{}{calendarEntryName, "ETH", "1h", "--mode=paper"}) {
		t.Fatalf("args = %v", raw["args"])
	}
	wantClose := map[string]interface{}{"name": "time_stop", "params": map[string]interface{}{"max_bars": 4.0}}
	if !reflect.DeepEqual(raw["close_strategy"], wantClose) {
		t.Fatalf("close_strategy = %v", raw["close_strategy"])
	}
	if raw["closed_bar_decisions"] != true || raw["stop_loss_atr_mult"] != 3.0 || raw["direction"] != DirectionBoth {
		t.Fatalf("entry = %v", raw)
	}
	if _, has := raw["allow_no_edge"]; has {
		t.Fatal("paper add must not write an acknowledgement")
	}
	cfg, err := LoadConfigReadOnly(writeNoEdgeConfig(t, CurrentConfigVersion, raw))
	if err != nil {
		t.Fatalf("added paper entry must load without acknowledgement: %v", err)
	}
	if sc := cfg.Strategies[0]; sc.AllowNoEdge != nil || portfolioScopeFor(sc) != ScopePaper || !sc.ClosedBarDecisions {
		t.Fatalf("%s must be explicit closed-bar paper with no acknowledgement", sc.ID)
	}
}

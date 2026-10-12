package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
)

const camarillaRef = "backtest/candidates/camarilla_pivot_reversal_1669/REPORT.md"

func camarillaHL(id, timeframe string, args ...string) map[string]interface{} {
	sc := noEdgeHLStrategy(id, append([]string{camarillaPivotReversalStrategyName, "ETH", timeframe}, args...)...)
	sc["closed_bar_decisions"] = true
	return sc
}

func loadBoth(t *testing.T, sc map[string]interface{}) (*Config, error) {
	t.Helper()
	path := writeNoEdgeConfig(t, CurrentConfigVersion, sc)
	cfg, loadErr := LoadConfig(path)
	_, probeErr := LoadConfigForProbe(path)
	if (loadErr == nil) != (probeErr == nil) {
		t.Fatalf("LoadConfig err=%v but LoadConfigForProbe err=%v", loadErr, probeErr)
	}
	return cfg, loadErr
}

func TestCamarillaPivotReversalRegistrationTables(t *testing.T) {
	name := camarillaPivotReversalStrategyName
	if knownShortNames[name] != "cpr" || !isBidirectionalPerpsStrategy(name) {
		t.Fatalf("short name %q, bidirectional %v", knownShortNames[name], isBidirectionalPerpsStrategy(name))
	}
	if !openStrategyRegisteredFor(name, "futures") || openStrategyRegisteredFor(name, "spot") {
		t.Fatal("camarilla_pivot_reversal must be registered for futures only")
	}
	var owners []string
	for other, short := range knownShortNames {
		if short == "cpr" {
			owners = append(owners, other)
		}
	}
	if !reflect.DeepEqual(owners, []string{name}) {
		t.Fatalf("short name cpr owners = %v", owners)
	}
	ev, ok := noEdgeEvidenceFor(name)
	if !ok || ev.Ref != camarillaRef {
		t.Fatalf("no-edge evidence = %+v, %v", ev, ok)
	}
	if fixedIntervalStrategyDefaults[name] != 3_600_000 {
		t.Fatalf("fixed-interval default = %d", fixedIntervalStrategyDefaults[name])
	}
	if _, ok := closedBarRequiredOpenStrategies[name]; !ok {
		t.Fatal("camarilla_pivot_reversal must require Hyperliquid closed-bar decisions")
	}
	if _, frozen := v20StampEligibleStrategies[name]; frozen {
		t.Fatal("camarilla_pivot_reversal must not join the frozen v20 auto-acknowledgement set")
	}
	var supported []int64
	for _, ms := range camarillaPivotReversalIntervals {
		supported = append(supported, ms)
	}
	sort.Slice(supported, func(i, j int) bool { return supported[i] < supported[j] })
	want := []int64{900_000, 1_800_000, 3_600_000, 7_200_000, 14_400_000, 28_800_000, 43_200_000}
	if !reflect.DeepEqual(supported, want) {
		t.Fatalf("supported intervals = %v, want %v", supported, want)
	}
	for tf, ms := range camarillaPivotReversalIntervals {
		if closedBarDecisionIntervalsMs[tf] != ms {
			t.Fatalf("%s maps to %d, closed-bar table has %d", tf, ms, closedBarDecisionIntervalsMs[tf])
		}
		if _, ok := hlCandleIntervalMs(tf); !ok {
			t.Fatalf("%s is not a Hyperliquid candle interval", tf)
		}
	}
}

func TestCamarillaPivotReversalAdmissionRoutesOpenAndCloseFallbackReferences(t *testing.T) {
	setNoEdgeLiveCredentials(t)
	fallback := func(args ...string) map[string]interface{} {
		sc := noEdgeHLStrategy("hl-bo-eth", append([]string{"breakout", "ETH", "1h"}, args...)...)
		sc["closed_bar_decisions"] = true
		sc["close_strategy"] = map[string]interface{}{"name": camarillaPivotReversalStrategyName}
		return sc
	}
	paper, err := loadBoth(t, camarillaHL("hl-cpr-eth", "1h", "--mode=paper"))
	if err != nil {
		t.Fatalf("explicit paper must load without acknowledgement: %v", err)
	}
	if got := portfolioScopeFor(paper.Strategies[0]); got != ScopePaper {
		t.Fatalf("scope = %q", got)
	}
	if _, err := loadBoth(t, camarillaHL("hl-cpr-eth", "1h")); err == nil || !strings.Contains(err.Error(), "missing --mode") {
		t.Fatalf("missing mode must be refused, got %v", err)
	}
	_, liveErr := loadBoth(t, fallback("--mode=live"))
	if liveErr == nil || !strings.Contains(liveErr.Error(), `close strategy "`+camarillaPivotReversalStrategyName+`"`) {
		t.Fatalf("unacknowledged live close fallback must be refused, got %v", liveErr)
	}
	cfg, err := loadBoth(t, fallback("--mode=paper"))
	if err != nil {
		t.Fatalf("paper close fallback must load: %v", err)
	}
	refs := noEdgeReferences(cfg.Strategies[0])
	if len(refs) != 1 || refs[0] != (edgeReference{Role: edgeRefRoleClose, Name: camarillaPivotReversalStrategyName}) {
		t.Fatalf("no-edge references = %+v", refs)
	}
}

func TestCamarillaPivotReversalRequiresHyperliquidPerpsClosedBarDecisions(t *testing.T) {
	setNoEdgeLiveCredentials(t)
	off := camarillaHL("hl-cpr-eth", "1h", "--mode=paper")
	off["closed_bar_decisions"] = false
	closeOff := noEdgeHLStrategy("hl-bo-eth", "breakout", "ETH", "1h", "--mode=paper")
	closeOff["close_strategy"] = map[string]interface{}{"name": camarillaPivotReversalStrategyName}
	okx := map[string]interface{}{
		"id": "okx-cpr", "type": "perps", "platform": "okx", "script": "shared_scripts/check_okx.py",
		"args": []string{camarillaPivotReversalStrategyName, "ETH", "1h", "--mode=paper"}, "capital": 100.0,
		"max_drawdown_pct": 10.0, "leverage": 1.0, "closed_bar_decisions": true,
	}
	manual := map[string]interface{}{
		"id": "hl-cpr-manual", "type": "manual", "platform": "hyperliquid", "symbol": "ETH", "timeframe": "1h",
		"leverage": 1.0, "capital": 100.0, "allow_no_edge": true,
		"close_strategy": map[string]interface{}{"name": camarillaPivotReversalStrategyName},
	}
	for name, sc := range map[string]map[string]interface{}{
		"open without closed bars":           off,
		"close fallback without closed bars": closeOff,
		"okx with closed bars":               okx,
		"hyperliquid manual close":           manual,
	} {
		t.Run(name, func(t *testing.T) {
			_, loadErr := loadBoth(t, sc)
			if loadErr == nil {
				t.Fatal("expected both loaders to refuse")
			}
			if !strings.Contains(loadErr.Error(), `runs only on a Hyperliquid perps strategy with "closed_bar_decisions": true`) {
				t.Fatalf("refusal names the wrong rule: %v", loadErr)
			}
		})
	}
}

func TestCamarillaPivotReversalCadenceMatchesTheTimeframeAndTheSupportedSet(t *testing.T) {
	setNoEdgeLiveCredentials(t)
	for tf, ms := range camarillaPivotReversalIntervals {
		sc := camarillaHL("hl-cpr-eth-"+tf, tf, "--mode=paper")
		sc["open_strategy"] = map[string]interface{}{"name": camarillaPivotReversalStrategyName, "params": map[string]interface{}{"expected_interval_ms": ms}}
		if _, err := loadBoth(t, sc); err != nil {
			t.Fatalf("%s with expected_interval_ms=%d must load: %v", tf, ms, err)
		}
	}
	if _, err := loadBoth(t, camarillaHL("hl-cpr-eth", "1h", "--mode=paper")); err != nil {
		t.Fatalf("1h with the registry default must load: %v", err)
	}
	withParams := func(tf string, value interface{}) map[string]interface{} {
		sc := camarillaHL("hl-cpr-eth", tf, "--mode=paper")
		sc["open_strategy"] = map[string]interface{}{"name": camarillaPivotReversalStrategyName, "params": map[string]interface{}{"expected_interval_ms": value}}
		return sc
	}
	fallback := noEdgeHLStrategy("hl-bo-eth", "breakout", "ETH", "4h", "--mode=paper")
	fallback["closed_bar_decisions"] = true
	fallback["close_strategy"] = map[string]interface{}{"name": camarillaPivotReversalStrategyName}
	refused := []struct {
		name string
		sc   map[string]interface{}
		want string
	}{
		{"registry default on 4h", camarillaHL("hl-cpr-eth", "4h", "--mode=paper"), "registry default expected_interval_ms=3600000 does not match timeframe 4h"},
		{"explicit mismatch", withParams("4h", 3_600_000), "explicit expected_interval_ms=3600000 does not match timeframe 4h"},
		{"unsupported 6h", withParams("6h", 21_600_000), `timeframe "6h" is not supported`},
		{"unsupported 5m", withParams("5m", 300_000), `timeframe "5m" is not supported`},
		{"boolean cadence", withParams("1h", true), "must be a positive integer"},
		{"string cadence", withParams("1h", "3600000"), "must be a positive integer"},
		{"fractional cadence", withParams("1h", 3_600_000.5), "must be a positive integer"},
		{"close fallback default on 4h", fallback, "close strategy \"" + camarillaPivotReversalStrategyName + "\": registry default"},
	}
	for _, tc := range refused {
		t.Run(tc.name, func(t *testing.T) {
			_, loadErr := loadBoth(t, tc.sc)
			if loadErr == nil {
				t.Fatalf("expected refusal containing %q", tc.want)
			}
			if !strings.Contains(loadErr.Error(), tc.want) {
				t.Fatalf("refusal missing %q: %v", tc.want, loadErr)
			}
		})
	}
}

func TestCamarillaPivotReversalOperatorAddCreatesALoadableExplicitPaperEntry(t *testing.T) {
	setNoEdgeLiveCredentials(t)
	id, entry, err := buildAddStrategyEntry(camarillaPivotReversalStrategyName, "hyperliquid", "sol")
	if err != nil || id != "hl-cpr-sol" {
		t.Fatalf("build = %q, %v", id, err)
	}
	var raw map[string]interface{}
	if err := json.Unmarshal(entry, &raw); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(raw["args"], []interface{}{camarillaPivotReversalStrategyName, "SOL", "1h", "--mode=paper"}) {
		t.Fatalf("args = %v", raw["args"])
	}
	wantClose := map[string]interface{}{"name": "tiered_tp_pct", "params": map[string]interface{}{
		"tp_tiers": []interface{}{map[string]interface{}{"profit_pct": 0.03, "close_fraction": 1.0}}}}
	if !reflect.DeepEqual(raw["close_strategy"], wantClose) {
		t.Fatalf("close_strategy = %v", raw["close_strategy"])
	}
	if raw["closed_bar_decisions"] != true || raw["stop_loss_atr_mult"] != 1.0 || raw["direction"] != DirectionBoth {
		t.Fatalf("entry = %v", raw)
	}
	if _, has := raw["allow_no_edge"]; has {
		t.Fatal("paper add must not write an acknowledgement")
	}
	if _, _, err := buildAddStrategyEntry(camarillaPivotReversalStrategyName, "binanceus", "BTC"); err == nil {
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
	if sc.AllowNoEdge != nil || portfolioScopeFor(sc) != ScopePaper || !sc.ClosedBarDecisions {
		t.Fatalf("%s must be explicit closed-bar paper with no acknowledgement", sc.ID)
	}
	refs := sc.closeRefs()
	if len(refs) != 1 || refs[0].Name != "tiered_tp_pct" {
		t.Fatalf("close owners = %+v", refs)
	}
	if sc.StopLossATRMult == nil || *sc.StopLossATRMult != 1.0 || sc.StopLossPct != nil || sc.TrailingStopATRMult != nil {
		t.Fatalf("stop owner = atr %v pct %v trailing %v", sc.StopLossATRMult, sc.StopLossPct, sc.TrailingStopATRMult)
	}
}

func TestCamarillaPivotReversalStudyParityConfigsLoadAsExplicitPaper(t *testing.T) {
	dir := filepath.Join("..", "backtest", "candidates", "camarilla_pivot_reversal_1669")
	for tf, ms := range camarillaPivotReversalIntervals {
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
		got, explicit, err := fixedIntervalParamValue(sc.OpenStrategy.Params)
		if err != nil || !explicit || got != ms {
			t.Fatalf("%s cadence = %d explicit=%v err=%v", tf, got, explicit, err)
		}
		if portfolioScopeFor(sc) != ScopePaper || sc.AllowNoEdge != nil || !sc.ClosedBarDecisions || sc.Direction != DirectionBoth {
			t.Fatalf("%s parity config must be explicit closed-bar paper in both directions", tf)
		}
	}
}

func TestV20NoEdgeMigrationNeverStampsCamarillaPivotReversal(t *testing.T) {
	open := noEdgeHLStrategy("hl-cpr-open", camarillaPivotReversalStrategyName, "ETH", "1h", "--mode=live")
	fallback := noEdgeHLStrategy("hl-cpr-close", "breakout", "ETH", "1h", "--mode=live")
	fallback["close_strategy"] = map[string]interface{}{"name": camarillaPivotReversalStrategyName}
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
	if want := []string{"hl-cpr-close", "hl-cpr-open"}; !reflect.DeepEqual(report.VetoedResearch, want) {
		t.Fatalf("vetoed = %v, want %v", report.VetoedResearch, want)
	}
}

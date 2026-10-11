package main

import (
	"encoding/json"
	"os"
	"reflect"
	"slices"
	"strings"
	"testing"
)

const schaffName = "schaff_trend_cycle"

func TestSchaffTrendCycleAdmissionThroughRealLoaders(t *testing.T) {
	setNoEdgeLiveCredentials(t)
	const ref = "backtest/candidates/schaff_trend_cycle_1651/REPORT.md"
	stc := func(args ...string) map[string]interface{} {
		return noEdgeHLStrategy("hl-stc-eth", append([]string{schaffName, "ETH", "4h"}, args...)...)
	}
	ack := func(sc map[string]interface{}) map[string]interface{} {
		sc["allow_no_edge"] = true
		return sc
	}
	openRef := func(sc map[string]interface{}) map[string]interface{} {
		sc["open_strategy"] = map[string]interface{}{"name": schaffName}
		return sc
	}
	closeRef := func(sc map[string]interface{}) map[string]interface{} {
		sc["close_strategy"] = map[string]interface{}{"name": schaffName}
		return sc
	}
	withArgs := func(sc map[string]interface{}, args ...string) map[string]interface{} {
		sc["args"] = append([]string{"breakout", "ETH", "4h"}, args...)
		return sc
	}
	ev, ok := noEdgeEvidenceFor(schaffName)
	if !ok || ev.Ref != ref {
		t.Fatalf("no-edge evidence = %+v, %v", ev, ok)
	}
	source := "source " + ev.Source
	cases := []struct {
		name    string
		sc      map[string]interface{}
		wantErr []string
		scope   PortfolioScope
	}{
		{"explicit paper", stc("--mode=paper"), nil, ScopePaper},
		{"explicit paper space form", stc("--mode", "paper"), nil, ScopePaper},
		{"live refused", stc("--mode=live"), []string{`open strategy "` + schaffName + `"`, source, ref, "explicit live mode"}, ""},
		{"missing mode refused", stc(), []string{"missing --mode"}, ""},
		{"live acknowledged", ack(stc("--mode=live")), nil, ScopeLive},
		{"missing mode acknowledged stays paper", ack(stc()), nil, ScopePaper},
		{"repeated mode refused", ack(stc("--mode=paper", "--mode=paper")), []string{"invalid mode", "given 2 times"}, ""},
		{"unknown mode refused", stc("--mode=Paper"), []string{"invalid mode"}, ""},
		{"conflicting mode refused", ack(stc("--mode=paper", "--mode=live")), []string{"invalid mode"}, ""},
		{"effective open live refused", withArgs(openRef(stc()), "--mode=live"), []string{`open strategy "` + schaffName + `"`, "explicit live mode"}, ""},
		{"effective open missing mode refused", withArgs(openRef(stc())), []string{"missing --mode"}, ""},
		{"effective open paper", withArgs(openRef(stc()), "--mode=paper"), nil, ScopePaper},
		{"effective open acknowledged", ack(withArgs(openRef(stc()), "--mode=live")), nil, ScopeLive},
		{"close fallback live refused", withArgs(closeRef(stc()), "--mode=live"), []string{`close strategy "` + schaffName + `"`, "explicit live mode"}, ""},
		{"close fallback missing mode refused", withArgs(closeRef(stc())), []string{"missing --mode"}, ""},
		{"close fallback repeated mode refused", withArgs(closeRef(stc()), "--mode=live", "--mode=live"), []string{"invalid mode"}, ""},
		{"close fallback paper", withArgs(closeRef(stc()), "--mode=paper"), nil, ScopePaper},
		{"close fallback acknowledged", ack(withArgs(closeRef(stc()), "--mode=live")), nil, ScopeLive},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := writeNoEdgeConfig(t, CurrentConfigVersion, tc.sc)
			cfg, loadErr := LoadConfig(path)
			_, probeErr := LoadConfigForProbe(path)
			if (loadErr == nil) != (probeErr == nil) {
				t.Fatalf("LoadConfig err=%v but LoadConfigForProbe err=%v", loadErr, probeErr)
			}
			if len(tc.wantErr) == 0 {
				if loadErr != nil {
					t.Fatalf("unexpected refusal: %v", loadErr)
				}
				if got := portfolioScopeFor(cfg.Strategies[0]); got != tc.scope {
					t.Fatalf("portfolio scope = %q, want %q", got, tc.scope)
				}
				return
			}
			if loadErr == nil {
				t.Fatalf("expected refusal containing %q", tc.wantErr)
			}
			for _, want := range tc.wantErr {
				if !strings.Contains(loadErr.Error(), want) || !strings.Contains(probeErr.Error(), want) {
					t.Fatalf("errors missing %q:\nload:  %v\nprobe: %v", want, loadErr, probeErr)
				}
			}
		})
	}
}

func TestSchaffTrendCycleAcknowledgementMustBeABoolean(t *testing.T) {
	setNoEdgeLiveCredentials(t)
	for _, raw := range []interface{}{"true", 1, map[string]interface{}{}} {
		sc := noEdgeHLStrategy("hl-stc-eth", schaffName, "ETH", "4h", "--mode=live")
		sc["allow_no_edge"] = raw
		if _, err := LoadConfig(writeNoEdgeConfig(t, CurrentConfigVersion, sc)); err == nil {
			t.Fatalf("allow_no_edge=%v must be refused", raw)
		}
	}
}

func TestSchaffTrendCycleRegistrationTables(t *testing.T) {
	if knownShortNames[schaffName] != "stc" || !isBidirectionalPerpsStrategy(schaffName) {
		t.Fatalf("short name %q, bidirectional %v", knownShortNames[schaffName], isBidirectionalPerpsStrategy(schaffName))
	}
	if !openStrategyRegisteredFor(schaffName, "futures") || openStrategyRegisteredFor(schaffName, "spot") {
		t.Fatal("schaff_trend_cycle must be registered for futures only")
	}
	owners := []string{}
	for other, short := range knownShortNames {
		if short == "stc" {
			owners = append(owners, other)
		}
	}
	if !reflect.DeepEqual(owners, []string{schaffName}) {
		t.Fatalf("short name stc owners = %v", owners)
	}
	if _, frozen := v20StampEligibleStrategies[schaffName]; frozen {
		t.Fatal("schaff_trend_cycle must not join the frozen v20 auto-acknowledgement set")
	}
}

func TestV20NoEdgeMigrationNeverStampsSchaffTrendCycle(t *testing.T) {
	open := noEdgeHLStrategy("hl-stc-open", schaffName, "ETH", "4h", "--mode=live")
	fallback := noEdgeHLStrategy("hl-stc-close", "breakout", "ETH", "4h", "--mode=live")
	fallback["close_strategy"] = map[string]interface{}{"name": schaffName}
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
	if want := []string{"hl-stc-close", "hl-stc-open"}; !reflect.DeepEqual(report.VetoedResearch, want) {
		t.Fatalf("vetoed = %v, want %v", report.VetoedResearch, want)
	}
}

func assertSchaffPaperEntry(t *testing.T, sc StrategyConfig, id string) {
	t.Helper()
	if sc.ID != id || sc.Direction != DirectionBoth || sc.MarginMode != "isolated" {
		t.Fatalf("entry identity = %s direction=%q margin=%q", sc.ID, sc.Direction, sc.MarginMode)
	}
	if sc.AllowNoEdge != nil || sc.AllowNoEdgeAcknowledged() || portfolioScopeFor(sc) != ScopePaper || edgeGateModeForStrategy(sc).Kind != edgeGateModePaper {
		t.Fatalf("%s must be explicit paper with no acknowledgement", sc.ID)
	}
	if !sc.ClosedBarDecisions {
		t.Fatalf("%s must decide on closed bars", sc.ID)
	}
	if args := appendClosedBarDecisionArgs(sc.Args, sc, nil); !slices.Contains(args, closedBarDecisionsFlag) {
		t.Fatalf("%s check args %v lack %s", sc.ID, args, closedBarDecisionsFlag)
	}
	refs := sc.closeRefs()
	if len(refs) != 1 || refs[0].Name != "tiered_tp_pct" {
		t.Fatalf("close owners = %+v, want exactly tiered_tp_pct", refs)
	}
	if sc.StopLossATRMult == nil || *sc.StopLossATRMult != 1.0 {
		t.Fatalf("stop owner = %v, want explicit stop_loss_atr_mult 1", sc.StopLossATRMult)
	}
	if sc.StopLossPct != nil || sc.StopLossMarginPct != nil || sc.TrailingStopPct != nil || sc.TrailingStopATRMult != nil ||
		sc.StopLossATRMultRegime.IsConfigured() || sc.TrailingStopATRMultRegime.IsConfigured() || strategyUsesUnifiedRegimeClose(sc) {
		t.Fatalf("%s has a second stop owner", sc.ID)
	}
	if refsEdge := noEdgeReferences(sc); len(refsEdge) != 1 || refsEdge[0] != (edgeReference{Role: edgeRefRoleOpen, Name: schaffName}) {
		t.Fatalf("no-edge references = %+v", refsEdge)
	}
}

func TestSchaffTrendCycleOperatorAddPathsCreateExplicitPaperEntries(t *testing.T) {
	setNoEdgeLiveCredentials(t)
	id, entry, err := buildAddStrategyEntry(schaffName, "hyperliquid", "sol")
	if err != nil || id != "hl-stc-sol" {
		t.Fatalf("build = %q, %v", id, err)
	}
	var raw map[string]interface{}
	if err := json.Unmarshal(entry, &raw); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(raw["args"], []interface{}{schaffName, "SOL", "1h", "--mode=paper"}) {
		t.Fatalf("args = %v", raw["args"])
	}
	if !reflect.DeepEqual(raw["close_strategy"], map[string]interface{}{"name": "tiered_tp_pct"}) {
		t.Fatalf("close_strategy = %v", raw["close_strategy"])
	}
	if raw["closed_bar_decisions"] != true || raw["stop_loss_atr_mult"] != 1.0 || raw["direction"] != DirectionBoth {
		t.Fatalf("entry = %v", raw)
	}
	if _, has := raw["allow_no_edge"]; has {
		t.Fatal("paper add must not write an acknowledgement")
	}
	if _, _, err := buildAddStrategyEntry(schaffName, "binanceus", "BTC"); err == nil {
		t.Fatal("futures-only candidate was accepted for binanceus")
	}
	_, eom, err := buildAddStrategyEntry("ease_of_movement_breakout", "hyperliquid", "BTC")
	if err != nil || strings.Contains(string(eom), "stop_loss_atr_mult") || !strings.Contains(string(eom), `"tiered_tp_atr"`) {
		t.Fatalf("the existing candidate default must stay unchanged: %s %v", eom, err)
	}
	if _, momentum, err := buildAddStrategyEntry("momentum", "hyperliquid", "BTC"); err != nil || strings.Contains(string(momentum), "close_strategy") || strings.Contains(string(momentum), "stop_loss_atr_mult") {
		t.Fatalf("other strategies must keep their existing add entry: %s %v", momentum, err)
	}

	for _, root := range []struct {
		name  string
		extra map[string]interface{}
	}{
		{"plain root", nil},
		{"root scalar stop default disabled", map[string]interface{}{"default_stop_loss_atr_mult": 0.0}},
		{"root scalar stop default raised", map[string]interface{}{"default_stop_loss_atr_mult": 2.5}},
		{"websocket feed", map[string]interface{}{"market_feed": "websocket"}},
	} {
		t.Run(root.name, func(t *testing.T) {
			path := writeNoEdgeConfigRoot(t, CurrentConfigVersion, root.extra, raw)
			cfg, err := LoadConfig(path)
			if err != nil {
				t.Fatalf("added paper entry must load without acknowledgement: %v", err)
			}
			if _, err := LoadConfigForProbe(path); err != nil {
				t.Fatalf("added paper entry must pass the probe loader: %v", err)
			}
			assertSchaffPaperEntry(t, cfg.Strategies[0], "hl-stc-sol")
		})
	}

	t.Run("builder applied to a root", func(t *testing.T) {
		path := writeNoEdgeConfig(t, CurrentConfigVersion, noEdgeHLStrategy("hl-rsi-eth", "rsi", "ETH", "1h", "--mode=paper"))
		root := readConfigRoot(t, path)
		if _, err := addStrategyToRoot(root, schaffName, "hyperliquid", "SOL"); err != nil {
			t.Fatal(err)
		}
		if _, err := addStrategyToRoot(root, schaffName, "hyperliquid", "SOL"); err == nil {
			t.Fatal("duplicate add must be refused")
		}
		data, err := json.MarshalIndent(root, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, data, 0600); err != nil {
			t.Fatal(err)
		}
		cfg, err := LoadConfig(path)
		if err != nil {
			t.Fatalf("builder-added candidate must load without acknowledgement: %v", err)
		}
		assertSchaffPaperEntry(t, cfg.Strategies[1], "hl-stc-sol")
	})
}

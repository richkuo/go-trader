package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
)

const amaName = "adaptive_moving_average"

func TestAdaptiveMovingAverageAdmissionThroughRealLoaders(t *testing.T) {
	setNoEdgeLiveCredentials(t)
	const ref = "backtest/candidates/adaptive_moving_average_1641/REPORT.md"
	ama := func(args ...string) map[string]interface{} {
		return noEdgeHLStrategy("hl-ama-btc", append([]string{amaName, "BTC", "4h"}, args...)...)
	}
	ack := func(sc map[string]interface{}, v interface{}) map[string]interface{} {
		sc["allow_no_edge"] = v
		return sc
	}
	asClose := func(args ...string) map[string]interface{} {
		sc := noEdgeHLStrategy("hl-bo-btc", append([]string{"breakout", "BTC", "4h"}, args...)...)
		sc["close_strategy"] = map[string]interface{}{"name": amaName}
		return sc
	}
	ev, ok := noEdgeEvidenceFor(amaName)
	if !ok || ev.Ref != ref {
		t.Fatalf("no-edge evidence = %+v, %v", ev, ok)
	}
	if short := knownShortNames[amaName]; short != "ama" {
		t.Fatalf("short name = %q", short)
	}
	for other, short := range knownShortNames {
		if short == "ama" && other != amaName {
			t.Fatalf("short name ama is shared with %s", other)
		}
	}
	if !openStrategyRegisteredFor(amaName, "futures") || openStrategyRegisteredFor(amaName, "spot") || !isBidirectionalPerpsStrategy(amaName) {
		t.Fatal("candidate must be futures-only and bidirectional")
	}
	if _, eligible := v20StampEligibleStrategies[amaName]; eligible {
		t.Fatal("a name registered after v20 must never be stamp-eligible")
	}
	cases := []struct {
		label     string
		sc        map[string]interface{}
		wantErr   []string
		wantScope PortfolioScope
	}{
		{"explicit paper", ama("--mode=paper"), nil, ScopePaper},
		{"explicit live refused", ama("--mode=live"), []string{`open strategy "` + amaName + `"`, ref, "explicit live mode"}, ""},
		{"missing mode refused", ama(), []string{"missing --mode"}, ""},
		{"repeated mode refused", ama("--mode=paper", "--mode=paper"), []string{"invalid mode"}, ""},
		{"malformed mode refused", ama("--mode="), []string{"invalid mode"}, ""},
		{"non-boolean acknowledgement refused", ack(ama("--mode=live"), "true"), []string{"allow_no_edge"}, ""},
		{"explicit live acknowledged", ack(ama("--mode=live"), true), nil, ScopeLive},
		{"close fallback live refused", asClose("--mode=live"), []string{`close strategy "` + amaName + `"`, "explicit live mode"}, ""},
		{"close fallback missing mode refused", asClose(), []string{`close strategy "` + amaName + `"`, "missing --mode"}, ""},
		{"close fallback paper", asClose("--mode=paper"), nil, ScopePaper},
		{"close fallback acknowledged", ack(asClose("--mode=live"), true), nil, ScopeLive},
	}
	for _, tc := range cases {
		t.Run(tc.label, func(t *testing.T) {
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
				if got := portfolioScopeFor(cfg.Strategies[0]); got != tc.wantScope {
					t.Fatalf("portfolio scope = %q, want %q", got, tc.wantScope)
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

func TestAdaptiveMovingAverageLegacyV20MigrationNeverGrantsLiveUse(t *testing.T) {
	setNoEdgeLiveCredentials(t)
	open := noEdgeHLStrategy("hl-ama", amaName, "BTC", "4h", "--mode=live")
	fallback := noEdgeHLStrategy("hl-bo-ama", "breakout", "ETH", "4h", "--mode=live")
	fallback["close_strategy"] = map[string]interface{}{"name": amaName}
	eligible := noEdgeHLStrategy("hl-rsi", "rsi", "SOL", "4h", "--mode=live")
	path := writeNoEdgeConfig(t, 19, open, fallback, eligible)
	report, err := migrateV20NoEdge(readRawConfig(t, path), 19)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"hl-ama", "hl-bo-ama"}; !reflect.DeepEqual(report.VetoedResearch, want) {
		t.Fatalf("unstamped = %v, want %v", report.VetoedResearch, want)
	}
	if got := stampedIDs(report); !reflect.DeepEqual(got, []string{"hl-rsi"}) {
		t.Fatalf("stamped = %v", got)
	}
	_, err = LoadConfig(path)
	if err == nil {
		t.Fatal("legacy live references to the candidate must stay refused")
	}
	for _, id := range []string{"hl-ama", "hl-bo-ama"} {
		if !strings.Contains(err.Error(), "strategy "+id+":") {
			t.Fatalf("%s should be refused: %v", id, err)
		}
		if v, has := rawStrategiesByID(t, path)[id]["allow_no_edge"]; has {
			t.Fatalf("%s gained allow_no_edge=%v", id, v)
		}
	}
}

func assertAdaptiveMovingAveragePaperEntry(t *testing.T, sc StrategyConfig, wantID string) {
	t.Helper()
	if sc.ID != wantID || sc.Direction != DirectionBoth {
		t.Fatalf("entry identity = %s direction=%q", sc.ID, sc.Direction)
	}
	if sc.AllowNoEdge != nil || portfolioScopeFor(sc) != ScopePaper || edgeGateModeForStrategy(sc).Kind != edgeGateModePaper {
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
	if sc.StopLossATRMult == nil || *sc.StopLossATRMult != DefaultStopLossATRMult {
		t.Fatalf("stop owner = %v, want stop_loss_atr_mult %g", sc.StopLossATRMult, DefaultStopLossATRMult)
	}
	if sc.StopLossPct != nil || sc.StopLossMarginPct != nil || sc.TrailingStopPct != nil || sc.TrailingStopATRMult != nil ||
		sc.StopLossATRMultRegime.IsConfigured() || sc.TrailingStopATRMultRegime.IsConfigured() || strategyUsesUnifiedRegimeClose(sc) {
		t.Fatalf("%s has a second stop owner", sc.ID)
	}
	if refsEdge := noEdgeReferences(sc); len(refsEdge) != 1 || refsEdge[0] != (edgeReference{Role: edgeRefRoleOpen, Name: amaName}) {
		t.Fatalf("no-edge references = %+v", refsEdge)
	}
}

func TestAdaptiveMovingAverageStudyPaperConfigsLoadAsUnacknowledgedPaper(t *testing.T) {
	for _, tf := range []string{"1h", "4h"} {
		t.Run(tf, func(t *testing.T) {
			src := filepath.Join("..", "backtest", "candidates", "adaptive_moving_average_1641", "paper_parity_"+tf+".json")
			data, err := os.ReadFile(src)
			if err != nil {
				t.Fatal(err)
			}
			var root map[string]interface{}
			if err := json.Unmarshal(data, &root); err != nil {
				t.Fatal(err)
			}
			dir := t.TempDir()
			root["db_file"] = filepath.Join(dir, "state.db")
			out, err := json.MarshalIndent(root, "", "  ")
			if err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(dir, "config.json")
			if err := os.WriteFile(path, out, 0600); err != nil {
				t.Fatal(err)
			}
			cfg, err := LoadConfig(path)
			if err != nil {
				t.Fatalf("study paper config must load without acknowledgement: %v", err)
			}
			assertAdaptiveMovingAveragePaperEntry(t, cfg.Strategies[0], "hl-ama-btc-"+tf)
		})
	}
}

func TestAdaptiveMovingAverageOperatorAddPathsCreateExplicitPaperEntries(t *testing.T) {
	setNoEdgeLiveCredentials(t)
	id, entry, err := buildAddStrategyEntry(amaName, "hyperliquid", "btc")
	if err != nil || id != "hl-ama-btc" {
		t.Fatalf("build = %q, %v", id, err)
	}
	var raw map[string]interface{}
	if err := json.Unmarshal(entry, &raw); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(raw["args"], []interface{}{amaName, "BTC", "1h", "--mode=paper"}) {
		t.Fatalf("args = %v", raw["args"])
	}
	if !reflect.DeepEqual(raw["close_strategy"], map[string]interface{}{"name": "tiered_tp_pct"}) || raw["closed_bar_decisions"] != true {
		t.Fatalf("close_strategy = %v closed_bar_decisions = %v", raw["close_strategy"], raw["closed_bar_decisions"])
	}
	if raw["direction"] != DirectionBoth {
		t.Fatalf("direction = %v", raw["direction"])
	}
	if _, _, err := buildAddStrategyEntry(amaName, "binanceus", "BTC"); err == nil {
		t.Fatal("futures-only candidate was accepted for binanceus")
	}

	t.Run("discord", func(t *testing.T) {
		path := writeNoEdgeConfig(t, CurrentConfigVersion, noEdgeHLStrategy("hl-rsi-eth", "rsi", "ETH", "1h", "--mode=paper"))
		root := readConfigRoot(t, path)
		if _, err := addStrategyToRoot(root, amaName, "hyperliquid", "BTC"); err != nil {
			t.Fatal(err)
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
			t.Fatalf("Discord-added candidate must load without acknowledgement: %v", err)
		}
		assertAdaptiveMovingAveragePaperEntry(t, cfg.Strategies[1], "hl-ama-btc")
	})
}

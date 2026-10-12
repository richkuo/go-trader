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

const trixName = "trix_momentum"

var trixFrozenLadder = []interface{}{
	map[string]interface{}{"profit_pct": 0.01, "close_fraction": 0.25},
	map[string]interface{}{"profit_pct": 0.02, "close_fraction": 0.5},
	map[string]interface{}{"profit_pct": 0.03, "close_fraction": 0.75},
	map[string]interface{}{"profit_pct": 0.04, "close_fraction": 1.0},
}

func TestTrixMomentumRegistrationTables(t *testing.T) {
	ev, ok := noEdgeEvidenceFor(trixName)
	if !ok || ev.Ref != "backtest/candidates/trix_momentum_1659/REPORT.md" {
		t.Fatalf("no-edge evidence = %+v, %v", ev, ok)
	}
	if short := knownShortNames[trixName]; short != "trix" {
		t.Fatalf("short name = %q", short)
	}
	for other, short := range knownShortNames {
		if short == "trix" && other != trixName {
			t.Fatalf("short name trix is shared with %s", other)
		}
	}
	if !openStrategyRegisteredFor(trixName, "futures") || openStrategyRegisteredFor(trixName, "spot") || !isBidirectionalPerpsStrategy(trixName) {
		t.Fatal("candidate must be futures-only and bidirectional")
	}
	if _, eligible := v20StampEligibleStrategies[trixName]; eligible {
		t.Fatal("a name registered after v20 must never be stamp-eligible")
	}
	if _, research := v20FormerResearchStrategies[trixName]; research {
		t.Fatal("the frozen v20 research set must stay unchanged")
	}
}

func TestTrixMomentumAdmissionThroughRealLoaders(t *testing.T) {
	setNoEdgeLiveCredentials(t)
	const ref = "backtest/candidates/trix_momentum_1659/REPORT.md"
	trix := func(args ...string) map[string]interface{} {
		sc := noEdgeHLStrategy("hl-trix-btc", append([]string{trixName, "BTC", "4h"}, args...)...)
		sc["closed_bar_decisions"] = true
		return sc
	}
	ack := func(sc map[string]interface{}, v interface{}) map[string]interface{} {
		sc["allow_no_edge"] = v
		return sc
	}
	asClose := func(args ...string) map[string]interface{} {
		sc := noEdgeHLStrategy("hl-bo-btc", append([]string{"breakout", "BTC", "4h"}, args...)...)
		sc["close_strategy"] = map[string]interface{}{"name": trixName}
		return sc
	}
	twoStops := func() map[string]interface{} {
		sc := trix("--mode=paper")
		sc["stop_loss_atr_mult"] = 1.0
		sc["stop_loss_pct"] = 2.0
		return sc
	}
	cases := []struct {
		label     string
		sc        map[string]interface{}
		wantErr   []string
		wantScope PortfolioScope
	}{
		{"explicit paper", trix("--mode=paper"), nil, ScopePaper},
		{"explicit live refused", trix("--mode=live"), []string{`open strategy "` + trixName + `"`, ref, "explicit live mode"}, ""},
		{"missing mode refused", trix(), []string{"missing --mode"}, ""},
		{"explicit live acknowledged", ack(trix("--mode=live"), true), nil, ScopeLive},
		{"close fallback live refused", asClose("--mode=live"), []string{`close strategy "` + trixName + `"`, "explicit live mode"}, ""},
		{"close fallback paper", asClose("--mode=paper"), nil, ScopePaper},
		{"close fallback acknowledged", ack(asClose("--mode=live"), true), nil, ScopeLive},
		{"two stop owners refused", twoStops(), []string{"stop_loss_atr_mult is mutually exclusive"}, ""},
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

func TestTrixMomentumLegacyV20MigrationNeverGrantsLiveUse(t *testing.T) {
	setNoEdgeLiveCredentials(t)
	open := noEdgeHLStrategy("hl-trix", trixName, "BTC", "4h", "--mode=live")
	fallback := noEdgeHLStrategy("hl-bo-trix", "breakout", "ETH", "4h", "--mode=live")
	fallback["close_strategy"] = map[string]interface{}{"name": trixName}
	eligible := noEdgeHLStrategy("hl-rsi", "rsi", "SOL", "4h", "--mode=live")
	path := writeNoEdgeConfig(t, 19, open, fallback, eligible)
	report, err := migrateV20NoEdge(readRawConfig(t, path), 19)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"hl-bo-trix", "hl-trix"}; !reflect.DeepEqual(report.VetoedResearch, want) {
		t.Fatalf("unstamped = %v, want %v", report.VetoedResearch, want)
	}
	if got := stampedIDs(report); !reflect.DeepEqual(got, []string{"hl-rsi"}) {
		t.Fatalf("stamped = %v", got)
	}
	_, err = LoadConfig(path)
	if err == nil {
		t.Fatal("legacy live references to the candidate must stay refused")
	}
	for _, id := range []string{"hl-trix", "hl-bo-trix"} {
		if !strings.Contains(err.Error(), "strategy "+id+":") {
			t.Fatalf("%s should be refused: %v", id, err)
		}
		if v, has := rawStrategiesByID(t, path)[id]["allow_no_edge"]; has {
			t.Fatalf("%s gained allow_no_edge=%v", id, v)
		}
	}
	if v, has := rawStrategiesByID(t, path)["hl-rsi"]["allow_no_edge"]; !has || v != true {
		t.Fatalf("the eligible control must keep its v20 stamp, got %v (present=%v)", v, has)
	}
}

func assertTrixMomentumPaperEntry(t *testing.T, sc StrategyConfig, wantID string) {
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
	if got := refs[0].Params["tp_tiers"]; !reflect.DeepEqual(got, trixFrozenLadder) {
		t.Fatalf("close ladder = %#v, want the frozen ladder", got)
	}
	if sc.StopLossATRMult == nil || *sc.StopLossATRMult != 1.0 {
		t.Fatalf("stop owner = %v, want stop_loss_atr_mult 1.0", sc.StopLossATRMult)
	}
	if sc.StopLossPct != nil || sc.StopLossMarginPct != nil || sc.TrailingStopPct != nil || sc.TrailingStopATRMult != nil ||
		sc.StopLossATRMultRegime.IsConfigured() || sc.TrailingStopATRMultRegime.IsConfigured() || strategyUsesUnifiedRegimeClose(sc) {
		t.Fatalf("%s has a second stop owner", sc.ID)
	}
	if refsEdge := noEdgeReferences(sc); len(refsEdge) != 1 || refsEdge[0] != (edgeReference{Role: edgeRefRoleOpen, Name: trixName}) {
		t.Fatalf("no-edge references = %+v", refsEdge)
	}
}

func TestTrixMomentumStudyPaperConfigsLoadAsUnacknowledgedPaper(t *testing.T) {
	for _, tf := range []string{"1h", "4h"} {
		t.Run(tf, func(t *testing.T) {
			src := filepath.Join("..", "backtest", "candidates", "trix_momentum_1659", "paper_parity_"+tf+".json")
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
			assertTrixMomentumPaperEntry(t, cfg.Strategies[0], "hl-trix-btc-"+tf)
		})
	}
}

func TestTrixMomentumOperatorAddPathsCreateExplicitPaperEntries(t *testing.T) {
	setNoEdgeLiveCredentials(t)
	id, entry, err := buildAddStrategyEntry(trixName, "hyperliquid", "btc")
	if err != nil || id != "hl-trix-btc" {
		t.Fatalf("build = %q, %v", id, err)
	}
	var raw map[string]interface{}
	if err := json.Unmarshal(entry, &raw); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(raw["args"], []interface{}{trixName, "BTC", "1h", "--mode=paper"}) {
		t.Fatalf("args = %v", raw["args"])
	}
	wantClose := map[string]interface{}{"name": "tiered_tp_pct", "params": map[string]interface{}{"tp_tiers": trixFrozenLadder}}
	if !reflect.DeepEqual(raw["close_strategy"], wantClose) || raw["closed_bar_decisions"] != true || raw["stop_loss_atr_mult"] != 1.0 {
		t.Fatalf("close_strategy = %v closed_bar_decisions = %v stop_loss_atr_mult = %v", raw["close_strategy"], raw["closed_bar_decisions"], raw["stop_loss_atr_mult"])
	}
	if raw["direction"] != DirectionBoth {
		t.Fatalf("direction = %v", raw["direction"])
	}
	if _, has := raw["allow_no_edge"]; has {
		t.Fatal("the add path must never write an acknowledgement")
	}
	if _, _, err := buildAddStrategyEntry(trixName, "binanceus", "BTC"); err == nil {
		t.Fatal("futures-only candidate was accepted for binanceus")
	}

	path := writeNoEdgeConfig(t, CurrentConfigVersion, noEdgeHLStrategy("hl-rsi-eth", "rsi", "ETH", "1h", "--mode=paper"))
	root := readConfigRoot(t, path)
	if _, err := addStrategyToRoot(root, trixName, "hyperliquid", "BTC"); err != nil {
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
	assertTrixMomentumPaperEntry(t, cfg.Strategies[1], "hl-trix-btc")
	if _, err := LoadConfigForProbe(path); err != nil {
		t.Fatalf("probe loader refused the Discord-added candidate: %v", err)
	}
}

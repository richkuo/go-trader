package main

import (
	"encoding/json"
	"os"
	"reflect"
	"slices"
	"strings"
	"testing"
)

const (
	elderRayName = "elder_ray_pullback"
	elderRayRef  = "backtest/candidates/elder_ray_pullback_1648/REPORT.md"
)

func TestElderRayPullbackAdmissionThroughRealLoaders(t *testing.T) {
	setNoEdgeLiveCredentials(t)
	erp := func(args ...string) map[string]interface{} {
		return noEdgeHLStrategy("hl-erp-eth", append([]string{elderRayName, "ETH", "4h"}, args...)...)
	}
	ack := func(sc map[string]interface{}) map[string]interface{} {
		sc["allow_no_edge"] = true
		return sc
	}
	openRef := func(sc map[string]interface{}) map[string]interface{} {
		sc["open_strategy"] = map[string]interface{}{"name": elderRayName}
		return sc
	}
	closeRef := func(sc map[string]interface{}) map[string]interface{} {
		sc["close_strategy"] = map[string]interface{}{"name": elderRayName}
		return sc
	}
	withArgs := func(sc map[string]interface{}, args ...string) map[string]interface{} {
		sc["args"] = append([]string{"breakout", "ETH", "4h"}, args...)
		return sc
	}
	cases := []struct {
		name    string
		sc      map[string]interface{}
		wantErr []string
		scope   PortfolioScope
	}{
		{"explicit paper", erp("--mode=paper"), nil, ScopePaper},
		{"explicit paper space form", erp("--mode", "paper"), nil, ScopePaper},
		{"live refused", erp("--mode=live"), []string{`open strategy "` + elderRayName + `"`, "source study_fail", elderRayRef, "explicit live mode"}, ""},
		{"missing mode refused", erp(), []string{"missing --mode"}, ""},
		{"live acknowledged", ack(erp("--mode=live")), nil, ScopeLive},
		{"missing mode acknowledged stays paper", ack(erp()), nil, ScopePaper},
		{"repeated mode refused", ack(erp("--mode=paper", "--mode=paper")), []string{"invalid mode", "given 2 times"}, ""},
		{"unknown mode refused", erp("--mode=Paper"), []string{"invalid mode"}, ""},
		{"conflicting mode refused", ack(erp("--mode=paper", "--mode=live")), []string{"invalid mode"}, ""},
		{"effective open live refused", withArgs(openRef(erp()), "--mode=live"), []string{`open strategy "` + elderRayName + `"`, "explicit live mode"}, ""},
		{"effective open missing mode refused", withArgs(openRef(erp())), []string{"missing --mode"}, ""},
		{"effective open paper", withArgs(openRef(erp()), "--mode=paper"), nil, ScopePaper},
		{"close fallback live refused", withArgs(closeRef(erp()), "--mode=live"), []string{`close strategy "` + elderRayName + `"`, "explicit live mode"}, ""},
		{"close fallback missing mode refused", withArgs(closeRef(erp())), []string{"missing --mode"}, ""},
		{"close fallback paper", withArgs(closeRef(erp()), "--mode=paper"), nil, ScopePaper},
		{"close fallback acknowledged", ack(withArgs(closeRef(erp()), "--mode=live")), nil, ScopeLive},
		{"native close keeps live admission", func() map[string]interface{} {
			sc := withArgs(erp(), "--mode=live")
			sc["close_strategy"] = map[string]interface{}{"name": "tiered_tp_pct"}
			return sc
		}(), nil, ScopeLive},
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

func TestElderRayPullbackAcknowledgementMustBeABoolean(t *testing.T) {
	setNoEdgeLiveCredentials(t)
	for _, raw := range []interface{}{"true", 1, map[string]interface{}{}} {
		sc := noEdgeHLStrategy("hl-erp-eth", elderRayName, "ETH", "4h", "--mode=live")
		sc["allow_no_edge"] = raw
		if _, err := LoadConfig(writeNoEdgeConfig(t, CurrentConfigVersion, sc)); err == nil {
			t.Fatalf("allow_no_edge=%v must be refused", raw)
		}
	}
}

func assertElderRayPaperEntry(t *testing.T, sc StrategyConfig) {
	t.Helper()
	if sc.ID != "hl-erp-btc" || sc.Direction != DirectionBoth || sc.MarginMode != "isolated" {
		t.Fatalf("entry identity = %s direction=%q margin=%q", sc.ID, sc.Direction, sc.MarginMode)
	}
	if sc.AllowNoEdge != nil || sc.AllowNoEdgeAcknowledged() || portfolioScopeFor(sc) != ScopePaper || edgeGateModeForStrategy(sc).Kind != edgeGateModePaper {
		t.Fatalf("%s must be explicit paper with no acknowledgement", sc.ID)
	}
	if !reflect.DeepEqual(sc.Args, []string{elderRayName, "BTC", "1h", "--mode=paper"}) {
		t.Fatalf("args = %v", sc.Args)
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
		t.Fatalf("stop owner = %v, want the loader default stop_loss_atr_mult %g", sc.StopLossATRMult, DefaultStopLossATRMult)
	}
	if sc.StopLossPct != nil || sc.StopLossMarginPct != nil || sc.TrailingStopPct != nil || sc.TrailingStopATRMult != nil ||
		sc.StopLossATRMultRegime.IsConfigured() || sc.TrailingStopATRMultRegime.IsConfigured() || strategyUsesUnifiedRegimeClose(sc) {
		t.Fatalf("%s has a second stop owner", sc.ID)
	}
	if refsEdge := noEdgeReferences(sc); len(refsEdge) != 1 || refsEdge[0] != (edgeReference{Role: edgeRefRoleOpen, Name: elderRayName}) {
		t.Fatalf("no-edge references = %+v", refsEdge)
	}
}

func TestElderRayPullbackOperatorAddPathsCreateExplicitPaperEntries(t *testing.T) {
	setNoEdgeLiveCredentials(t)
	id, entry, err := buildAddStrategyEntry(elderRayName, "hyperliquid", "btc")
	if err != nil || id != "hl-erp-btc" {
		t.Fatalf("build = %q, %v", id, err)
	}
	var raw map[string]interface{}
	if err := json.Unmarshal(entry, &raw); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(raw["close_strategy"], map[string]interface{}{"name": "tiered_tp_pct"}) {
		t.Fatalf("close_strategy = %v", raw["close_strategy"])
	}
	if raw["closed_bar_decisions"] != true || raw["direction"] != DirectionBoth {
		t.Fatalf("closed_bar_decisions = %v direction = %v", raw["closed_bar_decisions"], raw["direction"])
	}
	if _, has := raw["allow_no_edge"]; has {
		t.Fatal("add must not write an acknowledgement")
	}
	if _, _, err := buildAddStrategyEntry(elderRayName, "binanceus", "BTC"); err == nil {
		t.Fatal("futures-only candidate was accepted for binanceus")
	}

	t.Run("discord", func(t *testing.T) {
		path := writeNoEdgeConfig(t, CurrentConfigVersion, noEdgeHLStrategy("hl-rsi-eth", "rsi", "ETH", "1h", "--mode=paper"))
		root := readConfigRoot(t, path)
		if _, err := addStrategyToRoot(root, elderRayName, "hyperliquid", "BTC"); err != nil {
			t.Fatal(err)
		}
		if _, err := addStrategyToRoot(root, elderRayName, "hyperliquid", "BTC"); err == nil {
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
			t.Fatalf("Discord-added candidate must load without acknowledgement: %v", err)
		}
		if _, err := LoadConfigForProbe(path); err != nil {
			t.Fatalf("Discord-added candidate must pass the probe loader: %v", err)
		}
		assertElderRayPaperEntry(t, cfg.Strategies[1])
	})

	t.Run("status page", func(t *testing.T) {
		ss, path, _ := newStructuralTestServer(t)
		params := `{"name":"elder_ray_pullback","platform":"hyperliquid","asset":"BTC"}`
		confirm := structuralConfirm(t, ss, "add-strategy", "", params)
		if confirm.ConfirmPhrase != "hl-erp-btc" {
			t.Fatalf("confirm phrase = %q", confirm.ConfirmPhrase)
		}
		body := `{"nonce":"` + confirm.Nonce + `","params":` + params + `}`
		if w := mutationPost(ss, ss.handleAPIAddStrategy, "/api/config/add-strategy", body, nil); w.Code != 200 {
			t.Fatalf("add-strategy status = %d, body %s", w.Code, w.Body.String())
		}
		cfg, err := LoadConfigForProbe(path)
		if err != nil {
			t.Fatalf("status-page-added candidate must load: %v", err)
		}
		found := false
		for _, sc := range cfg.Strategies {
			if sc.ID == "hl-erp-btc" {
				found = true
				assertElderRayPaperEntry(t, sc)
			}
		}
		if !found {
			t.Fatal("status-page add did not write hl-erp-btc")
		}
	})
}

func TestElderRayPullbackLegacyLiveConfigsGainNoMigrationAcknowledgement(t *testing.T) {
	setNoEdgeLiveCredentials(t)
	closeFallback := noEdgeHLStrategy("hl-bo-erp", "breakout", "BTC", "4h", "--mode=live")
	closeFallback["close_strategy"] = map[string]interface{}{"name": elderRayName}
	eligible := noEdgeHLStrategy("hl-rsi-sol", "rsi", "SOL", "4h", "--mode=live")
	path := writeNoEdgeConfig(t, 19,
		noEdgeHLStrategy("hl-erp-eth", elderRayName, "ETH", "4h", "--mode=live"),
		closeFallback,
		eligible,
	)
	report, err := migrateV20NoEdge(readRawConfig(t, path), 19)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"hl-bo-erp", "hl-erp-eth"}; !reflect.DeepEqual(report.VetoedResearch, want) {
		t.Fatalf("vetoed research = %v, want %v", report.VetoedResearch, want)
	}
	if got := stampedIDs(report); !reflect.DeepEqual(got, []string{"hl-rsi-sol"}) {
		t.Fatalf("stamped = %v, want only the pre-v20 rsi strategy", got)
	}
	_, err = LoadConfig(path)
	if err == nil {
		t.Fatal("legacy live elder_ray_pullback references must stay refused after migration")
	}
	for _, id := range []string{"hl-erp-eth", "hl-bo-erp"} {
		if !strings.Contains(err.Error(), "strategy "+id+":") {
			t.Fatalf("%s should be refused after migration: %v", id, err)
		}
	}
	if strings.Contains(err.Error(), "strategy hl-rsi-sol:") {
		t.Fatalf("pre-v20 rsi strategy must be stamped and admitted: %v", err)
	}
	for id, sc := range rawStrategiesByID(t, path) {
		_, has := sc["allow_no_edge"]
		if want := id == "hl-rsi-sol"; has != want {
			t.Fatalf("%s allow_no_edge present=%v, want %v", id, has, want)
		}
	}
}

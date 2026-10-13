package main

import (
	"encoding/json"
	"os"
	"reflect"
	"slices"
	"strings"
	"testing"
)

const klingerName = "klinger_divergence"

func TestKlingerDivergenceAdmissionThroughRealLoaders(t *testing.T) {
	setNoEdgeLiveCredentials(t)
	const ref = "backtest/candidates/klinger_divergence_1662/REPORT.md"
	kd := func(args ...string) map[string]interface{} {
		sc := noEdgeHLStrategy("hl-klingerdiv-eth", append([]string{klingerName, "ETH", "4h"}, args...)...)
		sc["closed_bar_decisions"] = true
		return sc
	}
	ack := func(sc map[string]interface{}) map[string]interface{} {
		sc["allow_no_edge"] = true
		return sc
	}
	openRef := func(sc map[string]interface{}) map[string]interface{} {
		sc["open_strategy"] = map[string]interface{}{"name": klingerName}
		return sc
	}
	closeRef := func(sc map[string]interface{}) map[string]interface{} {
		sc["close_strategy"] = map[string]interface{}{"name": klingerName}
		return sc
	}
	withArgs := func(sc map[string]interface{}, args ...string) map[string]interface{} {
		sc["args"] = append([]string{"breakout", "ETH", "4h"}, args...)
		return sc
	}
	ev, ok := noEdgeEvidenceFor(klingerName)
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
		{"explicit paper", kd("--mode=paper"), nil, ScopePaper},
		{"explicit paper space form", kd("--mode", "paper"), nil, ScopePaper},
		{"live refused", kd("--mode=live"), []string{`open strategy "` + klingerName + `"`, source, ref, "explicit live mode"}, ""},
		{"missing mode refused", kd(), []string{"missing --mode"}, ""},
		{"live acknowledged", ack(kd("--mode=live")), nil, ScopeLive},
		{"missing mode acknowledged stays paper", ack(kd()), nil, ScopePaper},
		{"repeated mode refused", ack(kd("--mode=paper", "--mode=paper")), []string{"invalid mode", "given 2 times"}, ""},
		{"unknown mode refused", kd("--mode=Paper"), []string{"invalid mode"}, ""},
		{"conflicting mode refused", ack(kd("--mode=paper", "--mode=live")), []string{"invalid mode"}, ""},
		{"effective open live refused", withArgs(openRef(kd()), "--mode=live"), []string{`open strategy "` + klingerName + `"`, "explicit live mode"}, ""},
		{"effective open missing mode refused", withArgs(openRef(kd())), []string{"missing --mode"}, ""},
		{"effective open paper", withArgs(openRef(kd()), "--mode=paper"), nil, ScopePaper},
		{"effective open acknowledged", ack(withArgs(openRef(kd()), "--mode=live")), nil, ScopeLive},
		{"close fallback live refused", withArgs(closeRef(kd()), "--mode=live"), []string{`close strategy "` + klingerName + `"`, "explicit live mode"}, ""},
		{"close fallback missing mode refused", withArgs(closeRef(kd())), []string{"missing --mode"}, ""},
		{"close fallback repeated mode refused", withArgs(closeRef(kd()), "--mode=live", "--mode=live"), []string{"invalid mode"}, ""},
		{"close fallback paper", withArgs(closeRef(kd()), "--mode=paper"), nil, ScopePaper},
		{"close fallback acknowledged", ack(withArgs(closeRef(kd()), "--mode=live")), nil, ScopeLive},
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

func TestKlingerDivergenceRequiresHyperliquidClosedBarDecisions(t *testing.T) {
	setNoEdgeLiveCredentials(t)
	t.Setenv("TOPSTEP_API_KEY", "k")
	t.Setenv("TOPSTEP_API_SECRET", "s")
	t.Setenv("TOPSTEP_ACCOUNT_ID", "1")
	closed := func(sc map[string]interface{}, on bool) map[string]interface{} {
		sc["closed_bar_decisions"] = on
		return sc
	}
	closeRef := func(sc map[string]interface{}) map[string]interface{} {
		sc["close_strategy"] = map[string]interface{}{"name": klingerName}
		return sc
	}
	openRef := func(sc map[string]interface{}) map[string]interface{} {
		sc["open_strategy"] = map[string]interface{}{"name": klingerName}
		return sc
	}
	okx := func() map[string]interface{} {
		return map[string]interface{}{
			"id": "okx-klingerdiv", "type": "perps", "platform": "okx", "script": "shared_scripts/check_okx.py",
			"args": []string{klingerName, "ETH", "4h", "--mode=paper"}, "capital": 100.0, "max_drawdown_pct": 10.0,
			"leverage": 1.0,
		}
	}
	topstep := map[string]interface{}{
		"id": "ts-klingerdiv", "type": "futures", "platform": "topstep", "script": "shared_scripts/check_topstep.py",
		"args": []string{klingerName, "ES", "1h", "--mode=paper"}, "capital": 1000.0, "max_drawdown_pct": 10.0,
	}
	manual := map[string]interface{}{
		"id": "hl-klingerdiv-manual", "type": "manual", "platform": "hyperliquid", "symbol": "ETH", "timeframe": "4h",
		"leverage": 1.0, "capital": 100.0, "allow_no_edge": true, "close_strategy": map[string]interface{}{"name": klingerName},
	}
	const needs = `runs only on a Hyperliquid perps strategy with "closed_bar_decisions": true`
	cases := []struct {
		name    string
		sc      map[string]interface{}
		wantErr []string
	}{
		{"hyperliquid close fallback without closed bars refused",
			closeRef(noEdgeHLStrategy("hl-klingerdiv-close", "breakout", "ETH", "4h", "--mode=paper")),
			[]string{`close strategy "` + klingerName + `"`, needs, "closed_bar_decisions=false"}},
		{"hyperliquid close fallback with closed bars off refused",
			closed(closeRef(noEdgeHLStrategy("hl-klingerdiv-close", "breakout", "ETH", "4h", "--mode=paper")), false),
			[]string{`close strategy "` + klingerName + `"`, needs}},
		{"hyperliquid positional open without closed bars refused",
			noEdgeHLStrategy("hl-klingerdiv-eth", klingerName, "ETH", "4h", "--mode=paper"),
			[]string{`open strategy "` + klingerName + `"`, needs}},
		{"hyperliquid effective open without closed bars refused",
			openRef(noEdgeHLStrategy("hl-klingerdiv-open", "breakout", "ETH", "4h", "--mode=paper")),
			[]string{`open strategy "` + klingerName + `"`, needs}},
		{"okx positional open without closed bars refused", okx(),
			[]string{`open strategy "` + klingerName + `"`, needs, "platform=okx"}},
		{"okx positional open with closed bars refused", closed(okx(), true),
			[]string{`open strategy "` + klingerName + `"`, needs, "platform=okx", "closed_bar_decisions=true"}},
		{"topstep positional open refused", topstep,
			[]string{`open strategy "` + klingerName + `"`, needs, "platform=topstep"}},
		{"hyperliquid manual close refused", manual,
			[]string{`close strategy "` + klingerName + `"`, needs, "type=manual"}},
		{"hyperliquid close fallback with closed bars loads",
			closed(closeRef(noEdgeHLStrategy("hl-klingerdiv-close", "breakout", "ETH", "4h", "--mode=paper")), true), nil},
		{"hyperliquid positional open with closed bars loads",
			closed(noEdgeHLStrategy("hl-klingerdiv-eth", klingerName, "ETH", "4h", "--mode=paper"), true), nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := writeNoEdgeConfig(t, CurrentConfigVersion, tc.sc)
			_, loadErr := LoadConfig(path)
			_, probeErr := LoadConfigForProbe(path)
			if len(tc.wantErr) == 0 {
				if loadErr != nil || probeErr != nil {
					t.Fatalf("unexpected refusal:\nload:  %v\nprobe: %v", loadErr, probeErr)
				}
				return
			}
			if loadErr == nil || probeErr == nil {
				t.Fatalf("expected both loaders to refuse, load=%v probe=%v", loadErr, probeErr)
			}
			for _, want := range append([]string{"closed_bar_decisions"}, tc.wantErr...) {
				if !strings.Contains(loadErr.Error(), want) || !strings.Contains(probeErr.Error(), want) {
					t.Fatalf("errors missing %q:\nload:  %v\nprobe: %v", want, loadErr, probeErr)
				}
			}
		})
	}
}

func TestKlingerDivergenceAcknowledgementMustBeABoolean(t *testing.T) {
	setNoEdgeLiveCredentials(t)
	for _, raw := range []interface{}{"true", 1, map[string]interface{}{}} {
		sc := noEdgeHLStrategy("hl-klingerdiv-eth", klingerName, "ETH", "4h", "--mode=live")
		sc["closed_bar_decisions"] = true
		sc["allow_no_edge"] = raw
		if _, err := LoadConfig(writeNoEdgeConfig(t, CurrentConfigVersion, sc)); err == nil {
			t.Fatalf("allow_no_edge=%v must be refused", raw)
		}
	}
}

func TestV20NoEdgeMigrationNeverStampsKlingerDivergence(t *testing.T) {
	open := noEdgeHLStrategy("hl-klingerdiv-open", klingerName, "ETH", "4h", "--mode=live")
	fallback := noEdgeHLStrategy("hl-klingerdiv-close", "breakout", "ETH", "4h", "--mode=live")
	fallback["close_strategy"] = map[string]interface{}{"name": klingerName}
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
	if want := []string{"hl-klingerdiv-close", "hl-klingerdiv-open"}; !reflect.DeepEqual(report.VetoedResearch, want) {
		t.Fatalf("vetoed = %v, want %v", report.VetoedResearch, want)
	}
}

func assertKlingerPaperEntry(t *testing.T, sc StrategyConfig, id string) {
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
	if tiers, ok := refs[0].Params["tp_tiers"].([]interface{}); !ok || len(tiers) != 1 {
		t.Fatalf("close params = %+v, want one 2%% full-close tier", refs[0].Params)
	}
	if sc.StopLossATRMult == nil || *sc.StopLossATRMult != 1.0 {
		t.Fatalf("stop owner = %v, want explicit stop_loss_atr_mult 1", sc.StopLossATRMult)
	}
	if sc.StopLossPct != nil || sc.StopLossMarginPct != nil || sc.TrailingStopPct != nil || sc.TrailingStopATRMult != nil ||
		sc.StopLossATRMultRegime.IsConfigured() || sc.TrailingStopATRMultRegime.IsConfigured() || strategyUsesUnifiedRegimeClose(sc) {
		t.Fatalf("%s has a second stop owner", sc.ID)
	}
	if refsEdge := noEdgeReferences(sc); len(refsEdge) != 1 || refsEdge[0] != (edgeReference{Role: edgeRefRoleOpen, Name: klingerName}) {
		t.Fatalf("no-edge references = %+v", refsEdge)
	}
}

func TestKlingerDivergenceOperatorAddPathsCreateExplicitPaperEntries(t *testing.T) {
	setNoEdgeLiveCredentials(t)
	id, entry, err := buildAddStrategyEntry(klingerName, "hyperliquid", "sol")
	if err != nil || id != "hl-klingerdiv-sol" {
		t.Fatalf("build = %q, %v", id, err)
	}
	var raw map[string]interface{}
	if err := json.Unmarshal(entry, &raw); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(raw["args"], []interface{}{klingerName, "SOL", "1h", "--mode=paper"}) {
		t.Fatalf("args = %v", raw["args"])
	}
	wantClose := map[string]interface{}{"name": "tiered_tp_pct", "params": map[string]interface{}{
		"tp_tiers": []interface{}{map[string]interface{}{"profit_pct": 0.02, "close_fraction": 1.0}}}}
	if !reflect.DeepEqual(raw["close_strategy"], wantClose) {
		t.Fatalf("close_strategy = %v", raw["close_strategy"])
	}
	if raw["closed_bar_decisions"] != true || raw["stop_loss_atr_mult"] != 1.0 || raw["direction"] != DirectionBoth {
		t.Fatalf("entry = %v", raw)
	}
	if _, has := raw["allow_no_edge"]; has {
		t.Fatal("paper add must not write an acknowledgement")
	}
	if _, _, err := buildAddStrategyEntry(klingerName, "binanceus", "BTC"); err == nil {
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
			assertKlingerPaperEntry(t, cfg.Strategies[0], "hl-klingerdiv-sol")
		})
	}

	t.Run("builder applied to a root", func(t *testing.T) {
		path := writeNoEdgeConfig(t, CurrentConfigVersion, noEdgeHLStrategy("hl-rsi-eth", "rsi", "ETH", "1h", "--mode=paper"))
		root := readConfigRoot(t, path)
		if _, err := addStrategyToRoot(root, klingerName, "hyperliquid", "SOL"); err != nil {
			t.Fatal(err)
		}
		if _, err := addStrategyToRoot(root, klingerName, "hyperliquid", "SOL"); err == nil {
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
		assertKlingerPaperEntry(t, cfg.Strategies[1], "hl-klingerdiv-sol")
	})
}

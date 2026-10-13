package main

import (
	"encoding/json"
	"reflect"
	"slices"
	"strings"
	"testing"
)

const (
	smiName = "stochastic_momentum_index"
	smiRef  = "backtest/candidates/stochastic_momentum_index_1663/REPORT.md"
)

func smiHL(id string, args ...string) map[string]interface{} {
	sc := noEdgeHLStrategy(id, append([]string{smiName, "ETH", "4h"}, args...)...)
	sc["closed_bar_decisions"] = true
	return sc
}

func TestStochasticMomentumIndexAdmissionRoutesOpenAndCloseFallbackReferences(t *testing.T) {
	setNoEdgeLiveCredentials(t)
	fallback := func(args ...string) map[string]interface{} {
		sc := noEdgeHLStrategy("hl-bo-eth", append([]string{"breakout", "ETH", "4h"}, args...)...)
		sc["closed_bar_decisions"] = true
		sc["close_strategy"] = map[string]interface{}{"name": smiName}
		return sc
	}
	paper, err := loadBoth(t, smiHL("hl-smi-eth", "--mode=paper"))
	if err != nil {
		t.Fatalf("explicit paper must load without acknowledgement: %v", err)
	}
	if got := portfolioScopeFor(paper.Strategies[0]); got != ScopePaper {
		t.Fatalf("scope = %q", got)
	}
	_, missingErr := loadBoth(t, smiHL("hl-smi-eth"))
	if missingErr == nil || !strings.Contains(missingErr.Error(), "missing --mode") ||
		!strings.Contains(missingErr.Error(), `open strategy "`+smiName+`"`) || !strings.Contains(missingErr.Error(), smiRef) {
		t.Fatalf("missing mode must be refused with the candidate evidence, got %v", missingErr)
	}
	_, liveErr := loadBoth(t, fallback("--mode=live"))
	if liveErr == nil || !strings.Contains(liveErr.Error(), `close strategy "`+smiName+`"`) {
		t.Fatalf("unacknowledged live close fallback must be refused, got %v", liveErr)
	}
	cfg, err := loadBoth(t, fallback("--mode=paper"))
	if err != nil {
		t.Fatalf("paper close fallback must load: %v", err)
	}
	refs := noEdgeReferences(cfg.Strategies[0])
	if len(refs) != 1 || refs[0] != (edgeReference{Role: edgeRefRoleClose, Name: smiName}) {
		t.Fatalf("no-edge references = %+v", refs)
	}
	acked := smiHL("hl-smi-eth", "--mode=live")
	acked["allow_no_edge"] = true
	live, err := loadBoth(t, acked)
	if err != nil {
		t.Fatalf("acknowledged live must load: %v", err)
	}
	if got := portfolioScopeFor(live.Strategies[0]); got != ScopeLive {
		t.Fatalf("scope = %q", got)
	}
}

func TestStochasticMomentumIndexRequiresHyperliquidPerpsClosedBarDecisions(t *testing.T) {
	setNoEdgeLiveCredentials(t)
	off := smiHL("hl-smi-eth", "--mode=paper")
	off["closed_bar_decisions"] = false
	openRef := noEdgeHLStrategy("hl-bo-eth", "breakout", "ETH", "4h", "--mode=paper")
	openRef["open_strategy"] = map[string]interface{}{"name": smiName}
	closeOff := noEdgeHLStrategy("hl-bo-eth", "breakout", "ETH", "4h", "--mode=paper")
	closeOff["close_strategy"] = map[string]interface{}{"name": smiName}
	okx := map[string]interface{}{
		"id": "okx-smi", "type": "perps", "platform": "okx", "script": "shared_scripts/check_okx.py",
		"args": []string{smiName, "ETH", "4h", "--mode=paper"}, "capital": 100.0,
		"max_drawdown_pct": 10.0, "leverage": 1.0, "closed_bar_decisions": true,
	}
	manual := map[string]interface{}{
		"id": "hl-smi-manual", "type": "manual", "platform": "hyperliquid", "symbol": "ETH", "timeframe": "4h",
		"leverage": 1.0, "capital": 100.0, "allow_no_edge": true,
		"close_strategy": map[string]interface{}{"name": smiName},
	}
	for name, sc := range map[string]map[string]interface{}{
		"open without closed bars":           off,
		"effective open without closed bars": openRef,
		"close fallback without closed bars": closeOff,
		"okx with closed bars":               okx,
		"hyperliquid manual close":           manual,
	} {
		t.Run(name, func(t *testing.T) {
			_, loadErr := loadBoth(t, sc)
			if loadErr == nil {
				t.Fatal("expected both loaders to refuse")
			}
			if !strings.Contains(loadErr.Error(), `runs only on a Hyperliquid perps strategy with "closed_bar_decisions": true`) ||
				!strings.Contains(loadErr.Error(), `"`+smiName+`"`) {
				t.Fatalf("refusal names the wrong rule: %v", loadErr)
			}
		})
	}
}

func TestStochasticMomentumIndexOperatorAddCreatesALoadableExplicitPaperEntry(t *testing.T) {
	setNoEdgeLiveCredentials(t)
	id, entry, err := buildAddStrategyEntry(smiName, "hyperliquid", "sol")
	if err != nil || id != "hl-smi-sol" {
		t.Fatalf("build = %q, %v", id, err)
	}
	var raw map[string]interface{}
	if err := json.Unmarshal(entry, &raw); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(raw["args"], []interface{}{smiName, "SOL", "1h", "--mode=paper"}) {
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
	if _, _, err := buildAddStrategyEntry(smiName, "binanceus", "BTC"); err == nil {
		t.Fatal("futures-only candidate was accepted for binanceus")
	}
	for _, root := range []map[string]interface{}{nil, {"default_stop_loss_atr_mult": 0.0}} {
		path := writeNoEdgeConfigRoot(t, CurrentConfigVersion, root, raw)
		cfg, err := LoadConfig(path)
		if err != nil {
			t.Fatalf("added paper entry must load without acknowledgement (root %v): %v", root, err)
		}
		if _, err := LoadConfigForProbe(path); err != nil {
			t.Fatalf("added paper entry must pass the probe loader: %v", err)
		}
		sc := cfg.Strategies[0]
		if sc.AllowNoEdge != nil || portfolioScopeFor(sc) != ScopePaper || !sc.ClosedBarDecisions || sc.Direction != DirectionBoth {
			t.Fatalf("%s must be explicit closed-bar paper in both directions with no acknowledgement", sc.ID)
		}
		if args := appendClosedBarDecisionArgs(sc.Args, sc, nil); !slices.Contains(args, closedBarDecisionsFlag) {
			t.Fatalf("%s check args %v lack %s", sc.ID, args, closedBarDecisionsFlag)
		}
		refs := sc.closeRefs()
		if len(refs) != 1 || refs[0].Name != "tiered_tp_pct" {
			t.Fatalf("close owners = %+v", refs)
		}
		if sc.StopLossATRMult == nil || *sc.StopLossATRMult != 1.0 || sc.StopLossPct != nil || sc.StopLossMarginPct != nil ||
			sc.TrailingStopPct != nil || sc.TrailingStopATRMult != nil || sc.StopLossATRMultRegime.IsConfigured() ||
			sc.TrailingStopATRMultRegime.IsConfigured() || strategyUsesUnifiedRegimeClose(sc) {
			t.Fatalf("%s must have exactly one stop owner (stop_loss_atr_mult 1)", sc.ID)
		}
	}
	if _, momentum, err := buildAddStrategyEntry("momentum", "hyperliquid", "BTC"); err != nil ||
		strings.Contains(string(momentum), "close_strategy") || strings.Contains(string(momentum), "stop_loss_atr_mult") {
		t.Fatalf("other strategies must keep their existing add entry: %s %v", momentum, err)
	}
}

func TestV20NoEdgeMigrationNeverStampsStochasticMomentumIndex(t *testing.T) {
	open := noEdgeHLStrategy("hl-smi-open", smiName, "ETH", "4h", "--mode=live")
	fallback := noEdgeHLStrategy("hl-smi-close", "breakout", "ETH", "4h", "--mode=live")
	fallback["close_strategy"] = map[string]interface{}{"name": smiName}
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
	if want := []string{"hl-smi-close", "hl-smi-open"}; !reflect.DeepEqual(report.VetoedResearch, want) {
		t.Fatalf("vetoed = %v, want %v", report.VetoedResearch, want)
	}
}

package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
)

func TestFisherReversalAdmissionThroughRealLoaders(t *testing.T) {
	setNoEdgeLiveCredentials(t)
	const name = "fisher_reversal"
	const ref = "backtest/candidates/fisher_reversal_1646/REPORT.md"
	fisher := func(args ...string) map[string]interface{} {
		return noEdgeHLStrategy("hl-fisher-eth", append([]string{name, "ETH", "4h"}, args...)...)
	}
	ack := func(sc map[string]interface{}) map[string]interface{} {
		sc["allow_no_edge"] = true
		return sc
	}
	openRef := func(sc map[string]interface{}) map[string]interface{} {
		sc["open_strategy"] = map[string]interface{}{"name": name}
		return sc
	}
	closeRef := func(sc map[string]interface{}) map[string]interface{} {
		sc["close_strategy"] = map[string]interface{}{"name": name}
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
		{"explicit paper", fisher("--mode=paper"), nil, ScopePaper},
		{"explicit paper space form", fisher("--mode", "paper"), nil, ScopePaper},
		{"live refused", fisher("--mode=live"), []string{`open strategy "` + name + `"`, "source study_inconclusive", ref, "explicit live mode"}, ""},
		{"missing mode refused", fisher(), []string{"missing --mode"}, ""},
		{"live acknowledged", ack(fisher("--mode=live")), nil, ScopeLive},
		{"missing mode acknowledged stays paper", ack(fisher()), nil, ScopePaper},
		{"repeated mode refused", ack(fisher("--mode=paper", "--mode=paper")), []string{"invalid mode", "given 2 times"}, ""},
		{"unknown mode refused", fisher("--mode=Paper"), []string{"invalid mode"}, ""},
		{"conflicting mode refused", ack(fisher("--mode=paper", "--mode=live")), []string{"invalid mode"}, ""},
		{"effective open live refused", withArgs(openRef(fisher()), "--mode=live"), []string{`open strategy "` + name + `"`, "explicit live mode"}, ""},
		{"effective open missing mode refused", withArgs(openRef(fisher())), []string{"missing --mode"}, ""},
		{"effective open paper", withArgs(openRef(fisher()), "--mode=paper"), nil, ScopePaper},
		{"close fallback live refused", withArgs(closeRef(fisher()), "--mode=live"), []string{`close strategy "` + name + `"`, "explicit live mode"}, ""},
		{"close fallback missing mode refused", withArgs(closeRef(fisher())), []string{"missing --mode"}, ""},
		{"close fallback paper", withArgs(closeRef(fisher()), "--mode=paper"), nil, ScopePaper},
		{"close fallback acknowledged", ack(withArgs(closeRef(fisher()), "--mode=live")), nil, ScopeLive},
		{"native close keeps live admission", func() map[string]interface{} {
			sc := withArgs(fisher(), "--mode=live")
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

func TestFisherReversalAcknowledgementMustBeABoolean(t *testing.T) {
	setNoEdgeLiveCredentials(t)
	for _, raw := range []interface{}{"true", 1, map[string]interface{}{}} {
		sc := noEdgeHLStrategy("hl-fisher-eth", "fisher_reversal", "ETH", "4h", "--mode=live")
		sc["allow_no_edge"] = raw
		if _, err := LoadConfig(writeNoEdgeConfig(t, CurrentConfigVersion, sc)); err == nil {
			t.Fatalf("allow_no_edge=%v must be refused", raw)
		}
	}
}

func TestFisherReversalPaperAddThroughTheDiscordBuilder(t *testing.T) {
	setNoEdgeLiveCredentials(t)
	const name = "fisher_reversal"
	if knownShortNames[name] != "fisher" || !isBidirectionalPerpsStrategy(name) {
		t.Fatalf("short name %q, bidirectional %v", knownShortNames[name], isBidirectionalPerpsStrategy(name))
	}
	if !openStrategyRegisteredFor(name, "futures") || openStrategyRegisteredFor(name, "spot") {
		t.Fatal("fisher_reversal must be registered for futures only")
	}
	owners := 0
	for other, short := range knownShortNames {
		if short == "fisher" {
			owners++
			if other != name {
				t.Fatalf("short name fisher is also used by %s", other)
			}
		}
	}
	if owners != 1 {
		t.Fatalf("short name fisher owners = %d", owners)
	}
	id, entry, err := buildAddStrategyEntry(name, "hyperliquid", "sol")
	if err != nil {
		t.Fatalf("paper add refused: %v", err)
	}
	if id != "hl-fisher-sol" {
		t.Fatalf("add id = %q, want hl-fisher-sol", id)
	}
	var sc map[string]interface{}
	if err := json.Unmarshal(entry, &sc); err != nil {
		t.Fatal(err)
	}
	if sc["direction"] != DirectionBoth {
		t.Fatalf("direction = %v, want both", sc["direction"])
	}
	if got := sc["args"].([]interface{}); len(got) != 4 || got[0] != name || got[3] != "--mode=paper" {
		t.Fatalf("args = %v, want explicit paper", got)
	}
	if _, has := sc["allow_no_edge"]; has {
		t.Fatal("paper add must not write an acknowledgement")
	}
	if _, _, err := buildAddStrategyEntry(name, "binanceus", "BTC"); err == nil {
		t.Fatal("futures-only candidate must not be addable to binanceus")
	}
	root := map[string]json.RawMessage{"strategies": json.RawMessage(`[]`)}
	addedID, err := addStrategyToRoot(root, name, "hyperliquid", "SOL")
	if err != nil || addedID != id {
		t.Fatalf("shared add path = %q, %v", addedID, err)
	}
	if _, err := addStrategyToRoot(root, name, "hyperliquid", "SOL"); err == nil {
		t.Fatal("duplicate add must be refused")
	}
	list, err := configStrategies(root)
	if err != nil || len(list) != 1 {
		t.Fatalf("strategies after add = %v, %v", list, err)
	}
	var added map[string]interface{}
	if err := json.Unmarshal(list[0], &added); err != nil {
		t.Fatal(err)
	}
	path := writeNoEdgeConfigRoot(t, CurrentConfigVersion, map[string]interface{}{"market_feed": "websocket"}, added)
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("added paper entry must load without acknowledgement: %v", err)
	}
	if _, err := LoadConfigForProbe(path); err != nil {
		t.Fatalf("added paper entry must pass the probe loader: %v", err)
	}
	got := cfg.Strategies[0]
	if portfolioScopeFor(got) != ScopePaper || edgeGateModeForStrategy(got).Kind != edgeGateModePaper || got.AllowNoEdgeAcknowledged() {
		t.Fatalf("added entry is not explicit unacknowledged paper: %+v", got)
	}
	if got.Direction != DirectionBoth {
		t.Fatalf("loaded direction = %q, want both", got.Direction)
	}
}

func TestFisherReversalStatusPageAddCreatesExplicitPaper(t *testing.T) {
	ss, path, _ := newStructuralTestServer(t)
	params := `{"name":"fisher_reversal","platform":"hyperliquid","asset":"BTC"}`
	confirm := structuralConfirm(t, ss, "add-strategy", "", params)
	if confirm.ConfirmPhrase != "hl-fisher-btc" {
		t.Fatalf("confirm phrase = %q, want hl-fisher-btc", confirm.ConfirmPhrase)
	}
	body := fmt.Sprintf(`{"nonce":%q,"params":%s}`, confirm.Nonce, params)
	w := mutationPost(ss, ss.handleAPIAddStrategy, "/api/config/add-strategy", body, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("add-strategy status = %d, body %s", w.Code, w.Body.String())
	}
	list, err := configStrategies(readConfigRoot(t, path))
	if err != nil {
		t.Fatal(err)
	}
	var added map[string]interface{}
	for _, raw := range list {
		if strategyRawID(raw) == "hl-fisher-btc" {
			if err := json.Unmarshal(raw, &added); err != nil {
				t.Fatal(err)
			}
		}
	}
	if added == nil {
		t.Fatalf("hl-fisher-btc missing from config: %s", mustReadFile(t, path))
	}
	args := added["args"].([]interface{})
	if added["direction"] != DirectionBoth || args[0] != "fisher_reversal" || args[len(args)-1] != "--mode=paper" {
		t.Fatalf("added entry = %v, want direction both and explicit paper", added)
	}
	if _, has := added["allow_no_edge"]; has {
		t.Fatal("status-page add must not write an acknowledgement")
	}
	if _, err := LoadConfigForProbe(path); err != nil {
		t.Fatalf("config after add-strategy fails validation: %v", err)
	}
}

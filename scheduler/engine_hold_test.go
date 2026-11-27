package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func paperResearchServer(t *testing.T, root map[string]interface{}, strategies ...map[string]interface{}) (*StatusServer, string) {
	t.Helper()
	dir := t.TempDir()
	base := map[string]interface{}{
		"config_version":   CurrentConfigVersion,
		"interval_seconds": 60,
		"log_dir":          filepath.Join(dir, "logs"),
		"db_file":          filepath.Join(dir, "state.db"),
		"strategies":       strategies,
	}
	for k, v := range root {
		base[k] = v
	}
	data, err := json.MarshalIndent(base, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfigForProbe(path)
	if err != nil {
		t.Fatalf("fixture config must load: %v", err)
	}
	ss := NewStatusServer(NewAppState(), &sync.RWMutex{}, "", cfg.Strategies, nil)
	ss.SetConfigContext(path, cfg)
	ss.restartFn = func() error { return nil }
	return ss, path
}

func paperResearchMomentum() map[string]interface{} {
	return map[string]interface{}{"id": "hl-momentum-eth", "type": "perps", "platform": "hyperliquid", "script": hyperliquidCheckScript,
		"args": []string{"momentum", "ETH", "1h", "--mode=paper"}, "capital": 100, "max_drawdown_pct": 10, "leverage": 1, "direction": "long", "margin_mode": "isolated"}
}

func TestPaperResearchEngineHoldStatusAndReload(t *testing.T) {
	owner := researchQuoteOwnerFixture("--mode=paper")
	_, path := paperResearchServer(t, map[string]interface{}{"market_feed": "websocket"}, paperResearchMomentum(), owner)
	cfg, err := LoadConfigForProbe(path)
	if err != nil {
		t.Fatal(err)
	}
	holds := engineHeldStrategyIDs(cfg.Strategies)
	if len(holds) != 1 || holds["hl-invmm-btc"].Reason != quoteEngineHoldReason {
		t.Fatalf("held set = %+v", holds)
	}
	due := append([]StrategyConfig(nil), cfg.Strategies...)
	marks := map[string]feedEvaluationMark{"hl-invmm-btc": {ID: "x"}, "hl-momentum-eth": {ID: "x"}}
	supported, held := splitEngineHeldDue(due)
	if len(supported) != 1 || supported[0].ID != "hl-momentum-eth" || len(held) != 1 {
		t.Fatalf("split = %d supported, %d held", len(supported), len(held))
	}
	lastRun := map[string]time.Time{}
	lastEval := map[string]feedEvaluationMark{}
	reportEngineHeldDue(held, true, marks, lastRun, lastEval, time.Now())
	if _, stillMarked := marks["hl-invmm-btc"]; stillMarked {
		t.Fatal("a held entry must leave the shared-feed evaluation marks")
	}
	if _, ok := lastEval["hl-invmm-btc"]; !ok {
		t.Fatal("a held entry advances its due bookkeeping")
	}
	view := globalRunEvidence.healthView()["held"].(map[string]map[string]string)
	if view["hl-invmm-btc"]["reason"] != quoteEngineHoldReason || view["hl-invmm-btc"]["tracking_url"] != quoteEngineHoldTrackingURL {
		t.Fatalf("health view = %+v", view["hl-invmm-btc"])
	}
	globalRunEvidence.markHeld("hl-invmm-btc", runEvidenceHeldSaveBlocked, time.Now())
	globalRunEvidence.clearHeld("hl-invmm-btc")
	if h, ok := globalRunEvidence.engineHoldFor("hl-invmm-btc"); !ok || h.reason != quoteEngineHoldReason {
		t.Fatal("a persistence hold must not overwrite or clear the engine hold")
	}
	next := *cfg
	next.Strategies = append([]StrategyConfig(nil), cfg.Strategies...)
	for i := range next.Strategies {
		if next.Strategies[i].ID == "hl-invmm-btc" {
			next.Strategies[i].Quote = nil
			next.Strategies[i].Args = []string{"momentum", "BTC", "1m", "--mode=paper"}
			next.Strategies[i].OpenStrategy = StrategyRef{Name: "momentum"}
			next.Strategies[i].CloseStrategy = nil
		}
	}
	if errs := engineHoldReloadErrors(cfg, &next); len(errs) != 1 || !strings.Contains(errs[0], "restart required") {
		t.Fatalf("a reload that changes the held set must be refused: %v", errs)
	}
	if errs := engineHoldReloadErrors(cfg, cfg); len(errs) != 0 {
		t.Fatalf("an unchanged reload keeps the hold: %v", errs)
	}
	globalRunEvidence.retainEngineHolds(map[string]engineHold{})
	if _, ok := globalRunEvidence.engineHoldFor("hl-invmm-btc"); ok {
		t.Fatal("a removed hold must clear")
	}
}

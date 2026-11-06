//go:build pyintegration

package main

import (
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

const paperResearchHoldLine = "[hold] hl-invmm-btc: " + quoteEngineHoldReason

func paperResearchOwnerDoc(source string) map[string]any {
	sc := researchQuoteOwnerFixture("--mode=paper")
	if source != "" {
		sc["paper_source"] = source
	}
	return sc
}

func paperResearchPremiumDoc() map[string]any {
	return map[string]any{
		"id": "hl-premrev-btc", "type": "perps", "platform": "hyperliquid", "script": hyperliquidCheckScript,
		"args": []any{perpetualPremiumStrategyName, "BTC", "1m", "--mode=paper"}, "capital": 100, "max_drawdown_pct": 10,
		"interval_seconds": 60, "leverage": 1, "direction": "both", "margin_mode": "isolated", "stop_loss_atr_mult": 1,
		"close_strategy": map[string]any{"name": "tiered_tp_pct", "params": researchTPPct()},
	}
}

func paperResearchOnce(t *testing.T, e *mrpEnv, cfgPath string, extraEnv ...string) string {
	t.Helper()
	cmd := exec.Command(e.bin, "--config", cfgPath, "--once", "--status-port", strconv.Itoa(htfITFreePort(t)))
	cmd.Dir = e.root
	cmd.Env = e.env(extraEnv...)
	done := make(chan struct{})
	var out []byte
	var err error
	go func() {
		out, err = cmd.CombinedOutput()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(240 * time.Second):
		cmd.Process.Kill()
		<-done
		t.Fatalf("--once did not exit:\n%s", out)
	}
	if err != nil {
		t.Fatalf("--once failed: %v\n%s", err, out)
	}
	return string(out)
}

func paperResearchNoHeldWork(t *testing.T, label, log string) {
	t.Helper()
	for _, line := range strings.Split(log, "\n") {
		if !strings.Contains(line, "hl-invmm-btc") {
			continue
		}
		if strings.Contains(line, "[hold] hl-invmm-btc: "+quoteEngineHoldReason) || strings.Contains(line, "hl-invmm-btc |") || strings.HasPrefix(strings.TrimSpace(line), "[") && strings.Contains(line, "invmm") && (strings.Contains(line, "config") || strings.Contains(line, "summary")) {
			continue
		}
		for _, bad := range []string{"signal", "check_hyperliquid", "BUY", "SELL", "order", "fill", "stop", "protection", "evaluated", "quote-owned"} {
			if strings.Contains(line, bad) {
				t.Fatalf("%s: the held owner ran %q work:\n%s", label, bad, line)
			}
		}
	}
}

func TestPaperResearchSchedulerEngineHold(t *testing.T) {
	e := newMRPEnv(t, 40)
	t.Run("held_only_once_reports_the_hold_and_exits", func(t *testing.T) {
		t.Parallel()
		cfg := e.writeConfig("held-only.json", func(doc map[string]any) {
			delete(doc, "regime")
			doc["strategies"] = []any{paperResearchOwnerDoc("")}
		})
		log := paperResearchOnce(t, e, cfg)
		if !strings.Contains(log, paperResearchHoldLine) || !strings.Contains(log, "--once flag set, exiting after single cycle.") {
			t.Fatalf("the held-only --once must report the hold and reach the end-of-cycle exit:\n%s", log)
		}
		if strings.Contains(log, "[feed] eval=") {
			t.Fatalf("a held-only cycle must not request an evaluation snapshot:\n%s", log)
		}
		paperResearchNoHeldWork(t, "held-only", log)
		_, store := e.readStore(cfg)
		db, err := store.dbForStrategy("hl-invmm-btc")
		if err != nil {
			t.Fatal(err)
		}
		for _, table := range []string{"positions", "trades", "pending_limit_orders", "pending_manual_actions", "quote_contracts"} {
			var n int
			if err := db.db.QueryRow("SELECT COUNT(*) FROM " + table + " WHERE strategy_id = 'hl-invmm-btc'").Scan(&n); err != nil {
				t.Fatal(err)
			}
			if n != 0 {
				t.Fatalf("the held owner wrote %d %s rows", n, table)
			}
		}
		store.Close()
	})
	t.Run("mixed_once_runs_ordinary_work_and_unavailable_research_input_holds", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		cfg := e.writeConfig("mixed.json", func(doc map[string]any) {
			delete(doc, "regime")
			doc["db_file"], doc["log_dir"] = filepath.Join(dir, "state.db"), filepath.Join(dir, "logs")
			doc["paper_sources"] = []any{map[string]any{"id": "lab", "label": "Lab", "db_file": filepath.Join(dir, "lab.db")}}
			doc["strategies"] = []any{paperResearchOwnerDoc("lab"), paperResearchPremiumDoc()}
			doc["log_level"] = "debug"
		})
		log := paperResearchOnce(t, e, cfg)
		if !strings.Contains(log, paperResearchHoldLine) || !strings.Contains(log, "--once flag set, exiting after single cycle.") {
			t.Fatalf("the mixed run must report the hold and exit:\n%s", log)
		}
		if !strings.Contains(log, "[hl-premrev-btc] [DEBUG] Running: python3 shared_scripts/check_hyperliquid.py [perpetual_premium_reversion BTC 1m --mode=paper") ||
			!strings.Contains(log, "--market-stdin]") || !strings.Contains(log, "[hl-premrev-btc] [DEBUG] Signal: HOLD") {
			t.Fatalf("the mixed run must dispatch the explicit-paper research strategy to its sealed check and hold on the missing premium input:\n%s", log)
		}
		if strings.Contains(log, "Running: python3 shared_scripts/check_hyperliquid.py [inventory_market_making") {
			t.Fatalf("the held owner was dispatched:\n%s", log)
		}
		if strings.Contains(log, "production_refused") {
			t.Fatalf("the explicit-paper research strategy must pass admission:\n%s", log)
		}
		paperResearchNoHeldWork(t, "mixed", log)
		t.Logf("mixed log:\n%s", log)
	})
	t.Run("daemon_reports_the_hold_each_cycle_after_reload_and_in_status", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		port := htfITFreePort(t)
		cfg := e.writeConfig("daemon.json", func(doc map[string]any) {
			delete(doc, "regime")
			doc["db_file"], doc["log_dir"] = filepath.Join(dir, "state.db"), filepath.Join(dir, "logs")
			doc["status_port"] = port
			doc["strategies"] = []any{paperResearchOwnerDoc("")}
		})
		d := e.startDaemon(cfg, port)
		d.waitLog("a hold cycle", 120*time.Second, func(s string) bool { return strings.Count(s, paperResearchHoldLine) >= 2 })
		if err := d.cmd.Process.Signal(syscall.SIGHUP); err != nil {
			t.Fatal(err)
		}
		d.waitLog("reload", 60*time.Second, func(s string) bool { return strings.Contains(s, "[reload] "+paperResearchHoldLine) })
		before := strings.Count(d.output(), paperResearchHoldLine)
		d.waitLog("a hold after reload", 60*time.Second, func(s string) bool { return strings.Count(s, paperResearchHoldLine) >= before+1 })
		req, _ := http.NewRequest(http.MethodGet, "http://127.0.0.1:"+strconv.Itoa(port)+"/health", nil)
		req.Header.Set("Authorization", "Bearer "+e.token)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		body := make([]byte, 1<<16)
		n, _ := resp.Body.Read(body)
		resp.Body.Close()
		if !strings.Contains(string(body[:n]), quoteEngineHoldReason) || !strings.Contains(string(body[:n]), quoteEngineHoldTrackingURL) {
			t.Fatalf("/health must show the engine hold: %s", body[:n])
		}
		paperResearchNoHeldWork(t, "daemon", d.output())
		d.stop()
		if _, err := os.Stat(filepath.Join(dir, "state.db")); err != nil {
			t.Fatal(err)
		}
	})
}

func TestPaperResearchSchedulerCatalogConsumers(t *testing.T) {
	root := pyRepoRoot(t)
	t.Chdir(root)
	if err := discoverStrategies(); err != nil {
		t.Fatalf("the canonical catalog must load: %v", err)
	}
	if strategyCatalogNote != "" || len(strategyCatalogEntries) == 0 {
		t.Fatalf("the wizard must read the real catalog, not the built-in lists: %q", strategyCatalogNote)
	}
	research := map[string]bool{}
	for _, s := range researchPerpsStrategies {
		research[s.ID] = true
	}
	for name := range addStrategyResearchCandidates {
		if !research[name] {
			t.Fatalf("research perps choices miss %s: %v", name, researchPerpsStrategies)
		}
	}
	if research[inventoryMarketMakingStrategyName] {
		t.Fatal("the quote capability is never a scalar research choice")
	}
	for _, list := range [][]stratDef{spotStrategies, perpsStrategies} {
		for _, s := range list {
			if _, refused := productionRefusedStrategies[s.ID]; refused || s.ID == "pairs_spread" {
				t.Fatalf("default list holds %s", s.ID)
			}
			if _, noEdge := noEdgeEvidenceFor(s.ID); noEdge {
				t.Fatalf("default list holds no-edge %s", s.ID)
			}
		}
	}
	if !strings.Contains(catalogChoiceLabel(bitcoinLeadLagStrategyName), "live refused") {
		t.Fatalf("research choices carry their labels: %s", catalogChoiceLabel(bitcoinLeadLagStrategyName))
	}
	closeRegistryCatalogMu.Lock()
	closeRegistryCatalog = nil
	closeRegistryCatalogMu.Unlock()
	entries, err := fetchCloseRegistryCatalog()
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, e := range entries {
		if e.Name == inventoryQuoteExitCloseName {
			found = true
			if e.OwnerRole != "quote_coordinator" || !strings.Contains(strings.Join(e.Labels, ","), "engine held") {
				t.Fatalf("the quote-owned close must carry its ownership and hold labels: %+v", e)
			}
		}
	}
	if !found {
		t.Fatal("the close list must include the research-owned close")
	}
	pages := strings.Join(formatClosingStrategiesResponse(&Config{}, entries), "\n")
	if !strings.Contains(pages, inventoryQuoteExitCloseName) || !strings.Contains(pages, quoteEngineHoldReason) || !strings.Contains(pages, quoteEngineHoldTrackingURL) {
		t.Fatalf("the operator close list must show the held quote close:\n%s", pages)
	}
	if _, err := runInitFromJSONForCatalogTest(t); err != nil {
		t.Fatal(err)
	}
}

func runInitFromJSONForCatalogTest(t *testing.T) (string, error) {
	t.Helper()
	out := filepath.Join(t.TempDir(), "config.json")
	blob := `{"assets":["BTC"],"enableSpot":false,"enablePerps":true,"perpsMode":"live","perpsStrategies":["momentum"],"perpsLeverage":1,"perpsCapital":500,"perpsDrawdown":10,"allowNoEdge":true,"researchStrategies":["order_book_imbalance"]}`
	if code := runInitFromJSON(blob, out); code != 0 {
		t.Fatalf("wizard exited %d", code)
	}
	cfg, err := LoadConfigForProbe(out)
	if err != nil {
		t.Fatalf("wizard output must load: %v", err)
	}
	for _, sc := range cfg.Strategies {
		if sc.ID == "hl-obimb-btc" {
			if rawEdgeGateMode(sc.Args).Kind != edgeGateModePaper || cfg.MarketFeed != marketFeedWebsocket {
				t.Fatalf("wizard research entry under a live perps mode: %v feed %q", sc.Args, cfg.MarketFeed)
			}
			return out, nil
		}
	}
	t.Fatal("wizard output lacks the research entry")
	return "", nil
}

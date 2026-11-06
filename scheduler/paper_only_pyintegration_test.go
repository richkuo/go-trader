//go:build pyintegration

package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func paperOnlyLiveStrategy() map[string]any {
	sc := mrpManualStrategy("mr-live", "short")
	sc["args"] = []any{"hold", "BTC", "1m", "--mode=live"}
	return sc
}

func TestPaperOnlyConfigIntegration(t *testing.T) {
	e := newMRPEnv(t, 40)
	t.Run("live_strategy_refused_by_probe_and_once", func(t *testing.T) {
		t.Parallel()
		cfg := e.writeConfig("paper-only-live.json", func(doc map[string]any) {
			doc["paper_only"] = true
			doc["strategies"] = []any{mrpManualStrategy("mr-ready", "short"), paperOnlyLiveStrategy()}
		})
		want := "strategy[mr-live]: paper_only is true, so a live strategy (--mode=live) is refused"
		for _, args := range [][]string{{"probe"}, {"--once", "--status-port", strconv.Itoa(htfITFreePort(t))}, {"cpu-class"}} {
			run := e.command(cfg, nil, args...)
			if run.code == 0 || !strings.Contains(run.stderr, want) {
				t.Fatalf("%v must refuse the live strategy by name:\n%s", args, run)
			}
			if strings.Contains(run.stderr, "mr-ready: paper_only") {
				t.Fatalf("%v named the paper strategy:\n%s", args, run)
			}
		}
	})
	t.Run("paper_only_once_runs_as_paper_only_process", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		cfg := e.writeConfig("paper-only-once.json", func(doc map[string]any) {
			doc["paper_only"] = true
			doc["db_file"], doc["log_dir"] = filepath.Join(dir, "state.db"), filepath.Join(dir, "logs")
		})
		run := e.command(cfg, nil, "--once", "--status-port", strconv.Itoa(htfITFreePort(t)))
		if run.code != 0 {
			t.Fatalf("--once failed:\n%s", run)
		}
		if !strings.Contains(run.stdout, "[priority] paper-only process: paper check children run at the scheduler nice +") {
			t.Fatalf("a paper_only process must lower its paper check priority:\n%s", run)
		}
		if class := e.command(cfg, nil, "cpu-class"); class.code != 0 || class.stdout != "paper\n" {
			t.Fatalf("cpu-class must print exactly paper:\n%s", class)
		}
	})
	t.Run("sighup_adding_paper_only_is_refused", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		port := htfITFreePort(t)
		mutate := func(paperOnly bool) func(map[string]any) {
			return func(doc map[string]any) {
				doc["db_file"], doc["log_dir"] = filepath.Join(dir, "state.db"), filepath.Join(dir, "logs")
				doc["status_port"] = port
				if paperOnly {
					doc["paper_only"] = true
				}
			}
		}
		cfg := e.writeConfig("paper-only-reload.json", mutate(false))
		d := e.startDaemon(cfg, port)
		d.waitLog("the first cycle", 120*time.Second, func(s string) bool { return strings.Contains(s, "Cycle 1 complete") })
		e.writeConfig("paper-only-reload.json", mutate(true))
		if err := d.cmd.Process.Signal(syscall.SIGHUP); err != nil {
			t.Fatal(err)
		}
		d.waitLog("the reload refusal", 60*time.Second, func(s string) bool {
			return strings.Contains(s, "[reload] ERROR: reload rejected; keeping previous config") && strings.Contains(s, "paper_only changed (false -> true; restart required)")
		})
		d.stop()
	})
	t.Run("feed_config_rejects_paper_only", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		doc := map[string]any{
			"config_version": CurrentConfigVersion, "role": configRoleFeed, "paper_only": true, "strategies": []any{},
			"feed": map[string]any{"source": "hyperliquid", "socket_path": filepath.Join(dir, "feed.sock"), "consumer_configs": []any{}},
		}
		blob, err := json.Marshal(doc)
		if err != nil {
			t.Fatal(err)
		}
		cfg := filepath.Join(dir, "feed.json")
		if err := os.WriteFile(cfg, blob, 0o600); err != nil {
			t.Fatal(err)
		}
		run := e.command(cfg, nil, "probe")
		if run.code == 0 || !strings.Contains(run.stderr+run.stdout, `root key "paper_only" is not allowed in a role="feed" config`) {
			t.Fatalf("a feed config must refuse paper_only:\n%s", run)
		}
	})
}

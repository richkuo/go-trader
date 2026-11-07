//go:build pyintegration

package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

type pcpRecord struct {
	PID     int            `json:"pid"`
	Pgrp    int            `json:"pgrp"`
	Argv    []string       `json:"argv"`
	Threads map[string]int `json:"threads"`
}

func (r pcpRecord) has(s string) bool {
	return slices.ContainsFunc(r.Argv, func(a string) bool { return strings.Contains(a, s) })
}

func pcpBaseNice(t *testing.T) int {
	t.Helper()
	if runtime.GOOS == "linux" {
		raw, err := os.ReadFile("/proc/self/stat")
		if err != nil {
			t.Fatal(err)
		}
		s := string(raw)
		n, err := strconv.Atoi(strings.Fields(s[strings.LastIndex(s, ")")+2:])[16])
		if err != nil {
			t.Fatal(err)
		}
		return n
	}
	p, err := unix.Getpriority(unix.PRIO_PROCESS, 0)
	if err != nil {
		t.Fatal(err)
	}
	return int(int32(p))
}

func pcpRecorder(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("HTF_OFFLINE_NICE_DIR", dir)
	return dir
}

func pcpRecords(t *testing.T, dir string) []pcpRecord {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join(dir, "*.json"))
	if err != nil {
		t.Fatal(err)
	}
	var out []pcpRecord
	for _, p := range paths {
		raw, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		var r pcpRecord
		if err := json.Unmarshal(raw, &r); err != nil {
			t.Fatalf("%s: %v", p, err)
		}
		out = append(out, r)
	}
	return out
}

func pcpExpect(t *testing.T, recs []pcpRecord, label string, want int, match func(pcpRecord) bool) {
	t.Helper()
	found := 0
	for _, r := range recs {
		if !match(r) {
			continue
		}
		found++
		if r.Pgrp != r.PID {
			t.Errorf("%s: child %d is not its own process group leader (pgrp %d)", label, r.PID, r.Pgrp)
		}
		if runtime.GOOS == "linux" && len(r.Threads) < 2 {
			t.Errorf("%s: child %d recorded %d thread(s); the recorder thread is missing: %v", label, r.PID, len(r.Threads), r.Threads)
		}
		for tid, nice := range r.Threads {
			if nice != want {
				t.Errorf("%s: child %d thread %s runs at nice %d, want %d (argv %v)", label, r.PID, tid, nice, want, r.Argv)
			}
		}
	}
	if found == 0 {
		t.Errorf("%s: no child was recorded; records: %+v", label, recs)
	}
}

func pcpGate(t *testing.T, strategies []StrategyConfig, increment int) string {
	t.Helper()
	prevEligible := paperPriorityProcessEligible.Load()
	prevIncrement := paperChildNiceIncrement
	paperChildNiceIncrement = func() int { return increment }
	t.Cleanup(func() {
		paperPriorityProcessEligible.Store(prevEligible)
		paperChildNiceIncrement = prevIncrement
	})
	return configurePaperChildPriority(strategies, false)
}

func pcpStrategy(id, mode string) map[string]any {
	return map[string]any{
		"id": id, "type": "perps", "platform": "hyperliquid", "script": hyperliquidCheckScript,
		"args": []any{"breakout", "BTC", "1h", "--mode=" + mode}, "capital": 1000, "leverage": 1, "max_drawdown_pct": 90,
		"close_strategy": map[string]any{"name": "tiered_tp_pct"},
	}
}

func pcpBatch(t *testing.T, env rsITEnv, due []StrategyConfig) {
	t.Helper()
	resetBatchFallback(t)
	state := NewAppState()
	for _, sc := range env.cfg.Strategies {
		state.Strategies[sc.ID] = NewStrategyState(sc)
	}
	runHyperliquidBatchPrePass(due, state, &sync.RWMutex{}, env.cfg, env.prices, NewMultiNotifier(), func(format string, a ...any) { fmt.Fprintf(env.log, format+"\n", a...) }, env.feed)
}

func pcpStrategies(cfg *Config, ids ...string) []StrategyConfig {
	var out []StrategyConfig
	for _, sc := range cfg.Strategies {
		if slices.Contains(ids, sc.ID) {
			out = append(out, sc)
		}
	}
	return out
}

func pcpSingle(mode string) func(pcpRecord) bool {
	return func(r pcpRecord) bool {
		return r.has(hyperliquidCheckScript) && r.has("--mode="+mode) && !r.has("--batch-check")
	}
}

func pcpBatchChild(r pcpRecord) bool { return r.has("--batch-check") }

func TestPaperChildPriorityIntegration(t *testing.T) {
	base := pcpBaseNice(t)
	paper := min(maxNiceValue, base+10)
	if paper == base {
		t.Skipf("the test process already runs at nice %d, so a paper child cannot run lower", base)
	}
	t.Setenv("HYPERLIQUID_ACCOUNT_ADDRESS", "0x0000000000000000000000000000000000000001")
	t.Setenv("HYPERLIQUID_SECRET_KEY", "0x"+strings.Repeat("11", 32))

	t.Run("PaperOnlyProcessLowersOnlyPaperChecks", func(t *testing.T) {
		env := rsITSetup(t, rsITSnap{}, pcpStrategy("pcp-a", "paper"), pcpStrategy("pcp-b", "paper"))
		rsITSetRegime(t, env.cfg, "ranging_quiet", true)
		if line := pcpGate(t, env.cfg.Strategies, 10); !strings.Contains(line, "paper-only process") {
			t.Fatalf("a paper-only roster must turn the gate on, got %q", line)
		}
		dir := pcpRecorder(t)
		env.single(t, "pcp-a", PositionCtx{})
		pcpBatch(t, env, env.cfg.Strategies)
		if req, ok := strategyRegimeBundleRequest(env.cfg.Strategies[0], env.cfg.Regime); ok {
			runRegimeBundleCheck(shutdownReadOnlyCtx, req, nil)
		} else {
			t.Fatal("no regime bundle request")
		}
		runPythonSideEffect(hyperliquidCheckScript, []string{"pcp-side-effect"})
		runPythonReadOnly(hyperliquidCheckScript, []string{"pcp-read-only"})
		recs := pcpRecords(t, dir)
		pcpExpect(t, recs, "paper single check", paper, pcpSingle("paper"))
		pcpExpect(t, recs, "all-paper batch", paper, pcpBatchChild)
		pcpExpect(t, recs, "regime bundle check", base, func(r pcpRecord) bool { return r.has(regimeCheckScript) })
		pcpExpect(t, recs, "side-effect spawn", base, func(r pcpRecord) bool { return r.has("pcp-side-effect") })
		pcpExpect(t, recs, "shared read-only spawn", base, func(r pcpRecord) bool { return r.has("pcp-read-only") })
		if t.Failed() {
			t.Log(env.log.String())
		}
	})

	t.Run("MixedProcessKeepsEveryChildAtDefault", func(t *testing.T) {
		env := rsITSetup(t, rsITSnap{}, pcpStrategy("pcp-live", "live"), pcpStrategy("pcp-a", "paper"), pcpStrategy("pcp-b", "paper"))
		rsITSetRegime(t, env.cfg, "ranging_quiet", true)
		if line := pcpGate(t, env.cfg.Strategies, 10); !strings.Contains(line, "has a live strategy") {
			t.Fatalf("a roster with a live strategy must keep the gate off, got %q", line)
		}
		dir := pcpRecorder(t)
		env.single(t, "pcp-live", PositionCtx{})
		env.single(t, "pcp-a", PositionCtx{})
		pcpBatch(t, env, env.cfg.Strategies)
		pcpBatch(t, env, pcpStrategies(env.cfg, "pcp-a", "pcp-b"))
		recs := pcpRecords(t, dir)
		pcpExpect(t, recs, "live single check", base, pcpSingle("live"))
		pcpExpect(t, recs, "paper single check", base, pcpSingle("paper"))
		batches := 0
		for _, r := range recs {
			if pcpBatchChild(r) {
				batches++
			}
		}
		if batches != 2 {
			t.Errorf("want a mixed and an all-paper batch child, got %d batch children", batches)
		}
		pcpExpect(t, recs, "mixed and all-paper batches", base, pcpBatchChild)
		if t.Failed() {
			t.Log(env.log.String())
		}
	})

	t.Run("ForcedGateStillClassifiesEachCall", func(t *testing.T) {
		env := rsITSetup(t, rsITSnap{}, pcpStrategy("pcp-live", "live"), pcpStrategy("pcp-a", "paper"))
		rsITSetRegime(t, env.cfg, "ranging_quiet", true)
		pcpGate(t, env.cfg.Strategies, 10)
		paperPriorityProcessEligible.Store(true)
		dir := pcpRecorder(t)
		env.single(t, "pcp-live", PositionCtx{})
		env.single(t, "pcp-a", PositionCtx{})
		pcpBatch(t, env, env.cfg.Strategies)
		recs := pcpRecords(t, dir)
		pcpExpect(t, recs, "live single check", base, pcpSingle("live"))
		pcpExpect(t, recs, "paper single check", paper, pcpSingle("paper"))
		pcpExpect(t, recs, "mixed batch", base, pcpBatchChild)
		if t.Failed() {
			t.Log(env.log.String())
		}
	})

	t.Run("SchedulerOnceHonoursTheIncrement", func(t *testing.T) {
		root := htfITRoot(t)
		bin := pyintegrationBinary(t)
		dir := t.TempDir()
		cfgPath := filepath.Join(dir, "config.json")
		blob, err := json.MarshalIndent(map[string]any{
			"config_version": CurrentConfigVersion, "interval_seconds": 60,
			"db_file": filepath.Join(dir, "state.db"), "log_dir": filepath.Join(dir, "logs"),
			"strategies": []any{map[string]any{
				"id": "pcp-spot", "type": "spot", "platform": "binanceus", "script": "shared_scripts/check_strategy.py",
				"args": []string{"atr_breakout", "BTC/USDT", "1h"}, "capital": 1000, "max_drawdown_pct": 90, "interval_seconds": 60,
			}},
		}, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(cfgPath, blob, 0o600); err != nil {
			t.Fatal(err)
		}
		fixture := htfITWriteFixture(t, filepath.Join(dir, "offline.json"), htfITFixture{HTF: "neutral", Spot: "up", Fut: "up"})
		run := func(value string) (string, []pcpRecord) {
			t.Helper()
			recDir := t.TempDir()
			cmd := exec.Command(bin, "--config", cfgPath, "--once", "--status-port", strconv.Itoa(htfITFreePort(t)))
			cmd.Dir = root
			cmd.Env = append(os.Environ(),
				"PYTHONPATH="+filepath.Join(root, "shared_scripts", "testdata", "htf_offline"),
				"HTF_OFFLINE_FIXTURE="+fixture, "HTF_OFFLINE_NICE_DIR="+recDir,
				"DISCORD_BOT_TOKEN=", "TELEGRAM_BOT_TOKEN=", paperChildNiceEnv+"="+value)
			done := make(chan struct{})
			var out []byte
			go func() {
				out, err = cmd.CombinedOutput()
				close(done)
			}()
			select {
			case <-done:
			case <-time.After(240 * time.Second):
				cmd.Process.Kill()
				<-done
				t.Fatalf("%s=%s: --once did not exit:\n%s", paperChildNiceEnv, value, out)
			}
			if err != nil {
				t.Fatalf("%s=%s: --once failed: %v\n%s", paperChildNiceEnv, value, err, out)
			}
			return string(out), pcpRecords(t, recDir)
		}
		check := func(r pcpRecord) bool { return r.has("check_strategy.py") && !r.has("--probe-only") }
		probe := func(r pcpRecord) bool { return r.has("--probe-only") }

		log, recs := run("0")
		if !strings.Contains(log, "[priority] paper check children run at the default priority: "+paperChildNiceEnv+"=0 turns the feature off") {
			t.Errorf("=0 must log that the feature is off:\n%s", log)
		}
		pcpExpect(t, recs, "=0 paper check", base, check)
		pcpExpect(t, recs, "=0 startup probe", base, probe)

		log, recs = run("abc")
		if n := strings.Count(log, "[WARN] "+paperChildNiceEnv+"="); n != 1 {
			t.Errorf("an invalid value must log exactly one WARN, got %d:\n%s", n, log)
		}
		if n := strings.Count(log, "[priority] paper-only process: paper check children run at the scheduler nice +10"); n != 1 {
			t.Errorf("want one [priority] line with +10, got %d:\n%s", n, log)
		}
		pcpExpect(t, recs, "=abc paper check", paper, check)
		pcpExpect(t, recs, "=abc startup probe", base, probe)
		pcpExpect(t, recs, "=abc every other child", base, func(r pcpRecord) bool { return !check(r) })
		if t.Failed() {
			t.Log(log)
		}
	})
}

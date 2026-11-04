//go:build pyintegration

package main

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

const robinhoodAdmissionOptionsRoute = `a stock or ETF underlying runs only as a robinhood type "options" strategy`

func robinhoodAdmissionConfig(t *testing.T, dir, name string, args []string, extra map[string]any) string {
	t.Helper()
	strat := map[string]any{"args": args}
	for k, v := range extra {
		strat[k] = v
	}
	cfg := robinhoodTimeframeITConfig(t, dir, name, strat, map[string]any{"enabled": true, "timeframe": "12h"})
	return cfg
}

func robinhoodAdmissionRun(t *testing.T, root, bin string, args ...string) (string, error) {
	t.Helper()
	cmd := exec.Command(bin, args...)
	cmd.Dir = root
	cmd.Env = append(os.Environ(), "ROBINHOOD_USERNAME=", "DISCORD_BOT_TOKEN=", "TELEGRAM_BOT_TOKEN=")
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func TestRobinhoodSpotCoinAdmissionRealBinary(t *testing.T) {
	root := htfITRoot(t)
	bin := pyintegrationBinary(t)
	dir := t.TempDir()
	supported := "(" + robinhoodSupportedCryptoCoins() + ")"
	closedBar := map[string]any{"closed_bar_decisions": true}
	cases := []struct {
		name  string
		args  []string
		extra map[string]any
		want  string
	}{
		{"stock-ticker", []string{"atr_breakout", "AAPL", "2h", "--mode=paper"}, closedBar, `strategy[rh-tf]: robinhood spot coin "AAPL" is not a supported crypto coin: write one bare coin code from the verified table ` + supported + "; " + robinhoodAdmissionOptionsRoute},
		{"etf-ticker", []string{"atr_breakout", "SPY", "1h", "--mode=paper"}, closedBar, `robinhood spot coin "SPY" is not a supported crypto coin`},
		{"dash-pair", []string{"atr_breakout", "XRP-USD", "1h", "--mode=paper"}, closedBar, `robinhood spot coin "XRP-USD" is not a supported crypto coin`},
		{"slash-pair", []string{"atr_breakout", "BTC/USD", "1h", "--mode=paper"}, closedBar, `robinhood spot coin "BTC/USD" is not a supported crypto coin`},
		{"removed-matic", []string{"atr_breakout", "matic", "1h", "--mode=paper"}, closedBar, `robinhood spot coin "MATIC" is not a supported crypto coin`},
		{"missing-coin", []string{"atr_breakout"}, map[string]any{"allow_no_edge": true}, `strategy[rh-tf]: robinhood spot coin args[1] is missing: args must be <strategy> <coin> <timeframe>; supported coins: ` + robinhoodSupportedCryptoCoins() + "; " + robinhoodAdmissionOptionsRoute},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cfg := robinhoodAdmissionConfig(t, dir, c.name, c.args, c.extra)
			for _, run := range []struct {
				label string
				argv  []string
				fail  string
			}{
				{"startup", []string{"--config", cfg, "--once"}, "Failed to load config:"},
				{"probe", []string{"probe", "--config", cfg}, "probe: failed to load config"},
				{"read-only-inspect", []string{"inspect", "--config", cfg, "--all"}, "inspect: failed to load config"},
			} {
				log, err := robinhoodAdmissionRun(t, root, bin, run.argv...)
				if err == nil {
					t.Fatalf("%s accepted the config\n%s", run.label, log)
				}
				if !strings.Contains(log, run.fail) || !strings.Contains(log, c.want) {
					t.Fatalf("%s: want %q and %q\n%s", run.label, run.fail, c.want, log)
				}
				if n := strings.Count(log, "strategy[rh-tf]"); n != 1 {
					t.Fatalf("%s: want exactly one error for the refused strategy, got %d\n%s", run.label, n, log)
				}
				if strings.Contains(log, "[rh-tf] [") || strings.Contains(log, "check_robinhood.py") {
					t.Fatalf("%s: a strategy cycle or check ran before the config error\n%s", run.label, log)
				}
			}
			if _, err := os.Stat(filepath.Join(dir, c.name+".db")); err == nil {
				t.Fatalf("a refused config created its state file")
			}
		})
	}
	t.Run("earlier-normalization-error-stays-alone", func(t *testing.T) {
		cfg := robinhoodAdmissionConfig(t, dir, "blank-coin", []string{"atr_breakout", "   ", "1h", "--mode=paper"}, nil)
		log, err := robinhoodAdmissionRun(t, root, bin, "probe", "--config", cfg)
		if err == nil || !strings.Contains(log, `strategy[rh-tf]: robinhood spot coin args[1] "   " is empty after trimming`) {
			t.Fatalf("want the normalization error\n%s", log)
		}
		if strings.Contains(log, "is not a supported crypto coin") || strings.Count(log, "strategy[rh-tf]") != 1 {
			t.Fatalf("admission hid or duplicated the normalization error\n%s", log)
		}
	})
	t.Run("normalized-supported-coin-passes", func(t *testing.T) {
		cfg := robinhoodAdmissionConfig(t, dir, "uni", []string{"atr_breakout", " uni ", "4h", "--mode=paper"}, map[string]any{"htf_filter": true})
		for _, argv := range [][]string{{"probe", "--config", cfg}, {"inspect", "--config", cfg, "--all"}} {
			if log, err := robinhoodAdmissionRun(t, root, bin, argv...); err != nil {
				t.Fatalf("%s refused a normalized supported coin: %v\n%s", argv[0], err, log)
			}
		}
	})
}

type robinhoodAdmissionDaemon struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (d *robinhoodAdmissionDaemon) Write(p []byte) (int, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.buf.Write(p)
}

func (d *robinhoodAdmissionDaemon) String() string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.buf.String()
}

func TestRobinhoodSpotCoinAdmissionRefusedReloadKeepsThePreviousConfig(t *testing.T) {
	root := htfITRoot(t)
	bin := pyintegrationBinary(t)
	dir := t.TempDir()
	fixture := filepath.Join(dir, "fixture.json")
	if err := os.WriteFile(fixture, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := robinhoodAdmissionConfig(t, dir, "daemon", []string{"atr_breakout", "BTC", "1h", "--mode=paper"}, nil)
	good, err := os.ReadFile(cfg)
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(bin, "--config", cfg, "--status-port", strconv.Itoa(htfITFreePort(t)))
	cmd.Dir = root
	cmd.Env = append(os.Environ(), "ROBINHOOD_USERNAME=", "DISCORD_BOT_TOKEN=", "TELEGRAM_BOT_TOKEN=",
		"PYTHONPATH="+filepath.Join(root, "shared_scripts", "testdata", "htf_offline"), "HTF_OFFLINE_FIXTURE="+fixture)
	log := &robinhoodAdmissionDaemon{}
	cmd.Stdout, cmd.Stderr = log, log
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	exited := make(chan struct{})
	go func() {
		cmd.Wait()
		close(exited)
	}()
	t.Cleanup(func() {
		select {
		case <-exited:
		default:
			cmd.Process.Kill()
			<-exited
		}
	})
	waitFor := func(what string, cond func(string) bool) {
		t.Helper()
		deadline := time.Now().Add(120 * time.Second)
		for time.Now().Before(deadline) {
			if cond(log.String()) {
				return
			}
			select {
			case <-exited:
				t.Fatalf("daemon exited while waiting for %s:\n%s", what, log.String())
			case <-time.After(100 * time.Millisecond):
			}
		}
		t.Fatalf("timed out waiting for %s:\n%s", what, log.String())
	}
	const cycleLine = "All prices are zero/missing"
	waitFor("a first cycle", func(s string) bool { return strings.Contains(s, cycleLine) })
	var doc map[string]any
	if err := json.Unmarshal(good, &doc); err != nil {
		t.Fatal(err)
	}
	doc["strategies"].([]any)[0].(map[string]any)["args"] = []string{"atr_breakout", "AAPL", "1h", "--mode=paper"}
	bad, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cfg, bad, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Process.Signal(syscall.SIGHUP); err != nil {
		t.Fatal(err)
	}
	waitFor("the refused reload", func(s string) bool {
		return strings.Contains(s, `[reload] ERROR: reload failed; keeping previous config: config validation errors:`) &&
			strings.Contains(s, `strategy[rh-tf]: robinhood spot coin "AAPL" is not a supported crypto coin`)
	})
	before := strings.Count(log.String(), cycleLine)
	waitFor("a cycle after the refused reload", func(s string) bool { return strings.Count(s, cycleLine) > before })
	if strings.Contains(log.String(), "Config reload applied") {
		t.Fatalf("the refused reload applied a change:\n%s", log.String())
	}
	cmd.Process.Signal(os.Interrupt)
	select {
	case <-exited:
	case <-time.After(60 * time.Second):
		t.Fatalf("daemon did not drain:\n%s", log.String())
	}
	if !strings.Contains(log.String(), "[shutdown] Complete.") {
		t.Fatalf("daemon did not shut down cleanly after the refused reload:\n%s", log.String())
	}
}

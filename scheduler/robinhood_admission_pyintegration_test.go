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

func robinhoodAdmissionConfig(t *testing.T, dir, name string, args []string, extra map[string]any) string {
	t.Helper()
	strat := map[string]any{"args": args}
	for k, v := range extra {
		strat[k] = v
	}
	cfg := robinhoodTimeframeITConfig(t, dir, name, strat, map[string]any{"enabled": true, "timeframe": "12h"})
	return cfg
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

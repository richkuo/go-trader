package main

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"
)

// probeArgv is the sentinel argv shape passed to every configured check
// script at startup (#645). It mirrors the runtime argv produced by
// buildStrategyRefsArg + the per-platform check dispatchers, so an
// argparse-strict script that doesn't accept these flags will reject the
// probe and surface the binary/Python version mismatch before the trading
// loop starts.
//
// When the binary's check-script CLI gains a new required flag, append it
// here so a stale on-disk script fails the probe instead of crashing
// during a real cycle.
// The --strategy-refs payload mirrors buildStrategyRefsArg: top-level keys
// are "open" and "closes" (plural) and "closes" carries at least one ref
// so a stale parser that drops or rejects the close-ref shape fails the
// probe instead of silently treating closes as empty.
var probeArgv = []string{
	"probe", "BTC", "1h",
	"--strategy-refs", `{"open":{"name":"probe","params":{}},"closes":[{"name":"probe_close","params":{}}]}`,
	// #768: new Go forwards --mark-price on every HL signal-check; probe it
	// so a stale Python that doesn't accept the flag fails startup loudly
	// instead of every cycle's argparse rejecting the cycle's argv.
	"--mark-price=0",
	"--probe-only",
}

// fetchATRProbeArgv probes check_hyperliquid.py's --fetch-atr mode (#689) so a
// stale Python missing run_fetch_atr fails startup loudly instead of degrading
// silently to computeFallbackATR on every manual-open.
var fetchATRProbeArgv = []string{
	"--fetch-atr", "--symbol=BTC", "--timeframe=1h", "--period=14", "--probe-only",
}

// fetchCandlesProbeArgv probes the dashboard's on-demand OHLCV helper. The
// helper is not a configured strategy script, so it needs its own argv shape to
// catch stale Python deploys before the dashboard starts returning 500s.
var fetchCandlesProbeArgv = []string{
	"--platform=binanceus", "--type=spot", "--symbol=BTC/USDT", "--timeframe=1h", "--limit=1", "--probe-only",
}

const probeTimeout = 15 * time.Second

// probeCheckScripts invokes each unique check script configured in cfg
// with --probe-only. Returns nil if every script accepts the probe argv;
// returns an error describing the first failing script otherwise.
//
// Manual-argv scripts (check_strategy.py, check_options.py) short-circuit
// on --probe-only without parsing, so they always pass — the probe's
// signal value is highest for argparse-strict scripts (HL/TopStep/RH/OKX),
// where unknown flags cause the same exit-2 the May 7 outage exhibited.
// probeOneCheckScriptFn is the per-script probe invoker — package var so
// tests can stub it without standing up a real .venv (Go CI doesn't have
// one — see CLAUDE.md → Testing). The argv parameter lets a single script
// be probed against multiple argv shapes (e.g. signal-check + --fetch-atr).
var probeOneCheckScriptFn = probeOneCheckScript

func probeCheckScripts(cfg *Config) error {
	scripts := uniqueCheckScripts(cfg)
	for _, script := range scripts {
		if err := probeOneCheckScriptFn(script, probeArgv); err != nil {
			return err
		}
		// HL exposes --fetch-atr (#689) for manual-open ATR auto-fetch; probe
		// it so an old Python without run_fetch_atr fails the probe rather
		// than silently degrading every manual-open to computeFallbackATR.
		if filepath.Base(script) == "check_hyperliquid.py" {
			if err := probeOneCheckScriptFn(script, fetchATRProbeArgv); err != nil {
				return err
			}
		}
	}
	if len(scripts) > 0 {
		if err := probeOneCheckScriptFn("shared_scripts/fetch_candles.py", fetchCandlesProbeArgv); err != nil {
			return err
		}
	}
	return nil
}

func uniqueCheckScripts(cfg *Config) []string {
	seen := map[string]bool{}
	for _, sc := range cfg.Strategies {
		if sc.Script == "" || seen[sc.Script] {
			continue
		}
		seen[sc.Script] = true
	}
	out := make([]string, 0, len(seen))
	for s := range seen {
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}

func probeOneCheckScript(script string, argv []string) error {
	ctx, cancel := context.WithTimeout(context.Background(), probeTimeout)
	defer cancel()

	cmdArgs := append([]string{script}, argv...)
	cmd := exec.CommandContext(ctx, ".venv/bin/python3", cmdArgs...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()
	if ctx.Err() == context.DeadlineExceeded {
		if cmd.Process != nil {
			syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		}
		return fmt.Errorf("%s: probe timed out after %s", script, probeTimeout)
	}
	if err != nil {
		return formatProbeFailure(script, err, stderr.String(), stdout.String())
	}
	return nil
}

func formatProbeFailure(script string, runErr error, stderr, stdout string) error {
	stderr = strings.TrimSpace(stderr)
	stdout = strings.TrimSpace(stdout)
	detail := stderr
	if detail == "" {
		detail = stdout
	}
	if detail == "" {
		detail = runErr.Error()
	}
	return fmt.Errorf("%s rejected --probe-only argv (binary/Python version mismatch?): %s", script, detail)
}

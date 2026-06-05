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
	"--ohlcv-limit", "200",
	"--regime-windows-spec-json", `{"default":{"classifier":"adx","period":14,"adx_threshold":20}}`,
	"--regime-atr-window", "",
	// #879: scheduler injects the precomputed regime payload; probe the new
	// flags so a stale check script that doesn't accept them fails startup
	// instead of every cycle's argv rejecting the injection.
	"--regime-injected", "--regime-payload-json", `{"default":{"regime":"trending_up","score":0.4}}`,
	"--probe-only",
}

// probeCompositeArgv exercises classifier=composite in parse_regime_windows_spec_json
// so a stale Python missing the composite branch fails startup (#795 review).
var probeCompositeArgv = []string{
	"probe", "BTC", "1h",
	"--strategy-refs", `{"open":{"name":"probe","params":{}},"closes":[{"name":"probe_close","params":{}}]}`,
	"--mark-price=0",
	"--ohlcv-limit", "200",
	"--regime-windows-spec-json", `{"macro":{"classifier":"composite","period":14,"thresholds":{"return_pct":0.05,"range_pct":0.03,"adx":25}}}`,
	"--regime-atr-window", "",
	"--regime-injected", "--regime-payload-json", `{"macro":{"regime":"ranging_quiet","score":0.1}}`,
	"--probe-only",
}

// fetchRegimeProbeArgv probes the #879 dedicated regime subprocess. Like
// fetch_candles.py it is not a configured strategy script, so it needs its own
// probe to catch stale Python before the trading loop's per-cycle store build.
var fetchRegimeProbeArgv = []string{
	"--platform=hyperliquid", "--symbol=BTC", "--interval=1h",
	"--regime-windows-spec-json", `{"default":{"classifier":"adx","period":14,"adx_threshold":20}}`,
	"--ohlcv-limit=200", "--probe-only",
}

// fetchATRProbeArgv probes check_hyperliquid.py's --fetch-atr mode (#689) so a
// stale Python missing run_fetch_atr fails startup loudly instead of degrading
// silently to computeFallbackATR on every manual-open.
var fetchATRProbeArgv = []string{
	"--fetch-atr", "--symbol=BTC", "--timeframe=1h", "--period=14", "--probe-only",
}

// executeProbeArgv probes check_hyperliquid.py's --execute mode (PR #769
// review point 1). The signal-check probe doesn't cover the execute branch,
// so without this an asymmetric deploy (new Go binary forwarding
// --account-leverage / --account-margin-mode to a stale Python) would only
// fail on the first signal-fire rather than at startup. --mode=paper so the
// probe never enters the live-credentials branch; --probe-only short-circuits
// at the top of run_execute before any adapter or order code runs.
var executeProbeArgv = []string{
	"--execute",
	"--symbol=BTC", "--side=buy", "--size=0",
	"--mode=paper",
	"--margin-mode=cross", "--leverage=1",
	"--account-leverage=1", "--account-margin-mode=cross",
	"--probe-only",
}

// limitOpenProbeArgv / limitStatusProbeArgv / cancelOrderProbeArgv probe the
// #883 resting-limit-order modes so an asymmetric deploy (new Go binary
// forwarding --limit-open / --limit-status / --cancel-order to a stale Python)
// fails at startup rather than on the first manual-open --limit-price.
// --probe-only short-circuits before any adapter or order code runs.
var limitOpenProbeArgv = []string{
	"--limit-open",
	"--symbol=BTC", "--side=buy", "--size=0.01", "--limit-price=1",
	"--tif=Alo", "--margin-mode=cross", "--leverage=1",
	"--account-leverage=1", "--account-margin-mode=cross",
	"--probe-only",
}

var limitStatusProbeArgv = []string{
	"--limit-status", "--symbol=BTC", "--oids-json=[1]", "--probe-only",
}

var cancelOrderProbeArgv = []string{
	"--cancel-order", "--symbol=BTC", "--oid=1", "--probe-only",
}

// fetchCandlesProbeArgv probes the dashboard's on-demand OHLCV helper. The
// helper is not a configured strategy script, so it needs its own argv shape to
// catch stale Python deploys before the dashboard starts returning 500s.
var fetchCandlesProbeArgv = []string{
	"--platform=binanceus", "--type=spot", "--symbol=BTC/USDT", "--timeframe=1h", "--limit=1", "--probe-only",
}

var strategyTunerSchemaProbeArgv = []string{
	"--type=spot", "--strategy=sma", "--probe-only",
}

var simulateStrategyProbeArgv = []string{"--probe-only"}

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
		if err := probeOneCheckScriptFn(script, probeCompositeArgv); err != nil {
			return err
		}
		// HL exposes --fetch-atr (#689) for manual-open ATR auto-fetch; probe
		// it so an old Python without run_fetch_atr fails the probe rather
		// than silently degrading every manual-open to computeFallbackATR.
		if filepath.Base(script) == "check_hyperliquid.py" {
			if err := probeOneCheckScriptFn(script, fetchATRProbeArgv); err != nil {
				return err
			}
			// PR #769: also probe --execute so the new --account-leverage /
			// --account-margin-mode flags fail loudly at startup if Python is
			// stale, rather than on the first signal-fire.
			if err := probeOneCheckScriptFn(script, executeProbeArgv); err != nil {
				return err
			}
			// #883: probe the resting-limit-order modes so manual-open
			// --limit-price / manual-cancel / the scheduler fill poll fail at
			// startup on a stale Python rather than at first use.
			if err := probeOneCheckScriptFn(script, limitOpenProbeArgv); err != nil {
				return err
			}
			if err := probeOneCheckScriptFn(script, limitStatusProbeArgv); err != nil {
				return err
			}
			if err := probeOneCheckScriptFn(script, cancelOrderProbeArgv); err != nil {
				return err
			}
		}
	}
	if len(scripts) > 0 {
		if err := probeOneCheckScriptFn("shared_scripts/fetch_candles.py", fetchCandlesProbeArgv); err != nil {
			return err
		}
		// #879: probe the dedicated regime subprocess so an asymmetric deploy
		// (new Go binary invoking fetch_regime.py against a stale/missing Python)
		// fails at startup rather than on the first per-cycle store build.
		if err := probeOneCheckScriptFn("shared_scripts/fetch_regime.py", fetchRegimeProbeArgv); err != nil {
			return err
		}
		if err := probeOneCheckScriptFn("shared_scripts/strategy_tuner_schema.py", strategyTunerSchemaProbeArgv); err != nil {
			return err
		}
		if err := probeOneCheckScriptFn("shared_scripts/simulate_strategy.py", simulateStrategyProbeArgv); err != nil {
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
	if probeFailureScriptMissing(detail) {
		return fmt.Errorf("%s missing from deploy tree (sync Python with binary, e.g. scripts/update.sh): %s", script, detail)
	}
	return fmt.Errorf("%s rejected --probe-only argv (binary/Python version mismatch?): %s", script, detail)
}

func probeFailureScriptMissing(detail string) bool {
	// Python reports a missing probe script as "can't open file '…': [Errno 2] …".
	// Avoid broader ENOENT substrings so internal FileNotFoundError from a real
	// script is not mislabeled as a deploy-tree gap.
	return strings.Contains(detail, "can't open file")
}

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

var probeArgv = []string{
	"probe", "BTC", "1h",
	"--strategy-refs", `{"open":{"name":"probe","params":{}},"closes":[{"name":"probe_close","params":{}}]}`,

	"--mark-price=0",
	"--ohlcv-limit", "200",
	"--regime-windows-spec-json", `{"default":{"classifier":"adx","period":14,"adx_threshold":20}}`,
	"--regime-atr-window", "",

	"--regime-payload-json", `{"default":{"regime":"trending_up","score":0.5,"classifier":"adx","metrics":{"adx":25.0,"plus_di":20.0,"minus_di":10.0,"atr_pct":1.0}}}`,

	"--atr-method=simple",
	"--probe-only",
}

var probeCompositeArgv = []string{
	"probe", "BTC", "1h",
	"--strategy-refs", `{"open":{"name":"probe","params":{}},"closes":[{"name":"probe_close","params":{}}]}`,
	"--mark-price=0",
	"--ohlcv-limit", "200",
	"--regime-windows-spec-json", `{"macro":{"classifier":"composite","period":14,"thresholds":{"return_eff":0.05,"range_eff":0.03,"adx":25}}}`,
	"--regime-atr-window", "",
	"--regime-payload-json", `{"macro":{"regime":"trending_up_clean","score":0.5,"classifier":"composite","metrics":{"adx":30.0}}}`,
	"--atr-method=simple",
	"--probe-only",
}

var fetchATRProbeArgv = []string{
	"--fetch-atr", "--symbol=BTC", "--timeframe=1h", "--period=14", "--atr-method=simple", "--probe-only",
}

var llmReviewProbeArgv = []string{"--probe-only"}

var executeProbeArgv = []string{
	"--execute",
	"--symbol=BTC", "--side=buy", "--size=0",
	"--mode=paper",
	"--margin-mode=cross", "--leverage=1",
	"--account-leverage=1", "--account-margin-mode=cross",
	"--probe-only",
}

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

var hyperliquidBatchProbeArgv = []string{
	"--batch-check", "--symbol=BTC", "--timeframe=1h",
	"--ohlcv-limit", "200", "--atr-method=simple", "--mark-price=0",
	"--regime-windows-spec-json", `{"default":{"classifier":"adx","period":14,"adx_threshold":20}}`,
	"--regime-payload-json", `{"default":{"regime":"trending_up","score":0.5,"classifier":"adx","metrics":{"adx":25.0}}}`,
	"--probe-only",
}

var fetchCandlesProbeArgv = []string{
	"--platform=binanceus", "--type=spot", "--symbol=BTC/USDT", "--timeframe=1h", "--limit=1", "--probe-only",
}

var checkRegimeProbeArgv = []string{
	"--platform=binanceus", "--symbol=BTC/USDT", "--timeframe=1h",
	"--regime-windows-spec-json", `{"default":{"classifier":"adx","period":14,"adx_threshold":20}}`,
	"--ohlcv-limit", "200", "--min-bars", "30",
	"--probe-only",
}

var strategyTunerSchemaProbeArgv = []string{
	"--type=spot", "--strategy=sma", "--probe-only",
}

var simulateStrategyProbeArgv = []string{"--probe-only"}

const probeTimeout = 15 * time.Second

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

		if filepath.Base(script) == "check_hyperliquid.py" {
			if err := probeOneCheckScriptFn(script, fetchATRProbeArgv); err != nil {
				return err
			}

			if err := probeOneCheckScriptFn(script, executeProbeArgv); err != nil {
				return err
			}

			if err := probeOneCheckScriptFn(script, limitOpenProbeArgv); err != nil {
				return err
			}
			if err := probeOneCheckScriptFn(script, limitStatusProbeArgv); err != nil {
				return err
			}
			if err := probeOneCheckScriptFn(script, cancelOrderProbeArgv); err != nil {
				return err
			}

			if err := probeOneCheckScriptFn(script, hyperliquidBatchProbeArgv); err != nil {
				return err
			}
		}
	}

	if anyStrategyUsesLLMEntryAnalysis(cfg) {
		if err := probeOneCheckScriptFn(llmEntryAnalysisScript, llmReviewProbeArgv); err != nil {
			return err
		}
	}
	if len(scripts) > 0 {
		if err := probeOneCheckScriptFn("shared_scripts/fetch_candles.py", fetchCandlesProbeArgv); err != nil {
			return err
		}
		if err := probeOneCheckScriptFn("shared_scripts/strategy_tuner_schema.py", strategyTunerSchemaProbeArgv); err != nil {
			return err
		}
		if err := probeOneCheckScriptFn(regimeCheckScript, checkRegimeProbeArgv); err != nil {
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

	return strings.Contains(detail, "can't open file")
}

package main

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
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
var probeArgv = []string{
	"probe", "BTC", "1h",
	"--strategy-refs", `{"open":{"name":"probe","params":{}},"close":[]}`,
	"--probe-only",
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
func probeCheckScripts(cfg *Config) error {
	scripts := uniqueCheckScripts(cfg)
	for _, script := range scripts {
		if err := probeOneCheckScript(script); err != nil {
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

func probeOneCheckScript(script string) error {
	ctx, cancel := context.WithTimeout(context.Background(), probeTimeout)
	defer cancel()

	cmdArgs := append([]string{script}, probeArgv...)
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

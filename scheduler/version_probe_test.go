package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeProbeStub creates a Python stub at <dir>/<name> that exits 0 when
// --probe-only is in argv, otherwise echoes argparse-style rejection on
// stderr and exits 2 (mimicking the May 7 outage signature).
func writeProbeStub(t *testing.T, dir, name string, accept bool) string {
	t.Helper()
	path := filepath.Join(dir, name)
	body := `#!/usr/bin/env python3
import sys
if "--probe-only" in sys.argv:
    sys.exit(0)
sys.stderr.write("error: unrecognized arguments: --probe-only\n")
sys.exit(2)
`
	if !accept {
		body = `#!/usr/bin/env python3
import sys
sys.stderr.write("error: unrecognized arguments: --strategy-refs --probe-only\n")
sys.exit(2)
`
	}
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
		t.Fatalf("write stub: %v", err)
	}
	return path
}

// runProbeWithCwd runs probeCheckScripts from a tmp cwd containing a
// fake .venv/bin/python3 shim that just execs whichever script it's
// handed. Returns the probe's error.
func runProbeWithCwd(t *testing.T, cfg *Config) error {
	t.Helper()
	tmp := t.TempDir()
	venvBin := filepath.Join(tmp, ".venv", "bin")
	if err := os.MkdirAll(venvBin, 0o755); err != nil {
		t.Fatalf("mkdir venv: %v", err)
	}
	// Shim: forward argv unchanged to the real python3 found on PATH.
	shim := `#!/usr/bin/env bash
exec /usr/bin/env python3 "$@"
`
	pyShim := filepath.Join(venvBin, "python3")
	if err := os.WriteFile(pyShim, []byte(shim), 0o755); err != nil {
		t.Fatalf("write shim: %v", err)
	}
	fetchDir := filepath.Join(tmp, "shared_scripts")
	if err := os.MkdirAll(fetchDir, 0o755); err != nil {
		t.Fatalf("mkdir shared_scripts: %v", err)
	}
	writeProbeStub(t, fetchDir, "compute_regime_bundle.py", true)
	writeProbeStub(t, fetchDir, "fetch_candles.py", true)
	writeProbeStub(t, fetchDir, "strategy_tuner_schema.py", true)
	writeProbeStub(t, fetchDir, "simulate_strategy.py", true)

	prevCwd, _ := os.Getwd()
	if err := os.Chdir(tmp); err != nil {
		t.Fatalf("chdir tmp: %v", err)
	}
	t.Cleanup(func() { _ = os.Chdir(prevCwd) })

	// Script paths in cfg are already absolute (from writeProbeStub) and
	// stay valid regardless of cwd; the chdir above only matters so the
	// relative ".venv/bin/python3" path inside probeOneCheckScript hits
	// the shim we just dropped in tmp/.venv/bin/.
	return probeCheckScripts(cfg)
}

func TestProbeAcceptsCompatibleScript(t *testing.T) {
	tmp := t.TempDir()
	writeProbeStub(t, tmp, "check_good.py", true)
	cfg := &Config{
		Strategies: []StrategyConfig{
			{Script: filepath.Join(tmp, "check_good.py")},
		},
	}
	if err := runProbeWithCwd(t, cfg); err != nil {
		t.Fatalf("probe should accept compatible script: %v", err)
	}
}

func TestProbeRejectsStaleScript(t *testing.T) {
	tmp := t.TempDir()
	writeProbeStub(t, tmp, "check_stale.py", false)
	cfg := &Config{
		Strategies: []StrategyConfig{
			{Script: filepath.Join(tmp, "check_stale.py")},
		},
	}
	err := runProbeWithCwd(t, cfg)
	if err == nil {
		t.Fatal("probe should reject stale script that exits non-zero")
	}
	if !strings.Contains(err.Error(), "unrecognized arguments") {
		t.Errorf("error should surface argparse stderr; got %q", err.Error())
	}
	if !strings.Contains(err.Error(), "version mismatch") {
		t.Errorf("error should explain version-mismatch hypothesis; got %q", err.Error())
	}
}

func TestProbeDeduplicatesScripts(t *testing.T) {
	tmp := t.TempDir()
	writeProbeStub(t, tmp, "check_a.py", true)
	writeProbeStub(t, tmp, "check_b.py", true)
	cfg := &Config{
		Strategies: []StrategyConfig{
			{Script: filepath.Join(tmp, "check_a.py")},
			{Script: filepath.Join(tmp, "check_a.py")},
			{Script: filepath.Join(tmp, "check_b.py")},
		},
	}
	scripts := uniqueCheckScripts(cfg)
	if len(scripts) != 2 {
		t.Fatalf("expected 2 unique scripts, got %d: %v", len(scripts), scripts)
	}
}

func TestProbeIgnoresEmptyScripts(t *testing.T) {
	cfg := &Config{
		Strategies: []StrategyConfig{
			{Script: ""},
			{Script: ""},
		},
	}
	if err := probeCheckScripts(cfg); err != nil {
		t.Fatalf("empty-script strategies should be skipped, got: %v", err)
	}
}

// TestProbeRunsExtraArgvForHL verifies the extra --fetch-atr (#689) and
// --execute (PR #769) probes are dispatched only for check_hyperliquid.py.
// Stubs probeOneCheckScriptFn to record argv shapes per script without
// requiring a real .venv.
func TestProbeRunsExtraArgvForHL(t *testing.T) {
	orig := probeOneCheckScriptFn
	defer func() { probeOneCheckScriptFn = orig }()
	calls := map[string][]string{}
	probeOneCheckScriptFn = func(script string, argv []string) error {
		mode := "signal"
		for _, a := range argv {
			switch a {
			case "--fetch-atr":
				mode = "fetch-atr"
			case "--execute":
				mode = "execute"
			case "--limit-open":
				mode = "limit-open"
			case "--limit-status":
				mode = "limit-status"
			case "--cancel-order":
				mode = "cancel-order"
			}
			if mode != "signal" {
				break
			}
		}
		calls[script] = append(calls[script], mode)
		return nil
	}
	cfg := &Config{
		Strategies: []StrategyConfig{
			{Script: "shared_scripts/check_hyperliquid.py"},
			{Script: "shared_scripts/check_strategy.py"},
		},
	}
	if err := probeCheckScripts(cfg); err != nil {
		t.Fatalf("probe failed: %v", err)
	}
	hl := calls["shared_scripts/check_hyperliquid.py"]
	wantHL := []string{"signal", "signal", "fetch-atr", "execute", "limit-open", "limit-status", "cancel-order"}
	if len(hl) != len(wantHL) {
		t.Errorf("HL should be probed %v, got %v", wantHL, hl)
	} else {
		for i, w := range wantHL {
			if hl[i] != w {
				t.Errorf("HL probe[%d] = %q, want %q (full: %v)", i, hl[i], w, hl)
			}
		}
	}
	spot := calls["shared_scripts/check_strategy.py"]
	if len(spot) != 2 || spot[0] != "signal" || spot[1] != "signal" {
		t.Errorf("non-HL should be probed signal(adx)+signal(composite), got %v", spot)
	}
	candles := calls["shared_scripts/fetch_candles.py"]
	if len(candles) != 1 || candles[0] != "signal" {
		t.Errorf("dashboard candle helper should be probed once, got %v", candles)
	}
	schema := calls["shared_scripts/strategy_tuner_schema.py"]
	if len(schema) != 1 || schema[0] != "signal" {
		t.Errorf("strategy tuner schema helper should be probed once, got %v", schema)
	}
	simulate := calls["shared_scripts/simulate_strategy.py"]
	if len(simulate) != 1 || simulate[0] != "signal" {
		t.Errorf("simulate helper should be probed once, got %v", simulate)
	}
}

func TestFormatProbeFailureFallsBackThroughChannels(t *testing.T) {
	// stderr present -> uses stderr
	err := formatProbeFailure("check.py", os.ErrInvalid, " stderr-msg\n", "stdout-msg")
	if !strings.Contains(err.Error(), "stderr-msg") {
		t.Errorf("should prefer stderr; got %q", err.Error())
	}
	// stderr empty -> falls back to stdout
	err = formatProbeFailure("check.py", os.ErrInvalid, "  ", "stdout-msg")
	if !strings.Contains(err.Error(), "stdout-msg") {
		t.Errorf("should fall back to stdout when stderr empty; got %q", err.Error())
	}
	// both empty -> falls back to runErr
	err = formatProbeFailure("check.py", os.ErrInvalid, "", "")
	if !strings.Contains(err.Error(), os.ErrInvalid.Error()) {
		t.Errorf("should fall back to runErr; got %q", err.Error())
	}
}

func TestFormatProbeFailureScriptMissing(t *testing.T) {
	stderr := ".venv/bin/python3: can't open file 'shared_scripts/strategy_tuner_schema.py': [Errno 2] No such file or directory"
	err := formatProbeFailure("shared_scripts/strategy_tuner_schema.py", os.ErrInvalid, stderr, "")
	if !strings.Contains(err.Error(), "missing from deploy tree") {
		t.Errorf("want missing-script wording; got %q", err.Error())
	}
	if strings.Contains(err.Error(), "rejected --probe-only") {
		t.Errorf("should not label missing file as CLI mismatch; got %q", err.Error())
	}
}

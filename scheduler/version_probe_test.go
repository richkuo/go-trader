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
	writeProbeStub(t, fetchDir, "fetch_candles.py", true)

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
			if a == "--fetch-atr" {
				mode = "fetch-atr"
				break
			}
			if a == "--execute" {
				mode = "execute"
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
	if len(hl) != 4 || hl[0] != "signal" || hl[1] != "signal" || hl[2] != "fetch-atr" || hl[3] != "execute" {
		t.Errorf("HL should be probed signal(adx)+signal(composite)+fetch-atr+execute, got %v", hl)
	}
	spot := calls["shared_scripts/check_strategy.py"]
	if len(spot) != 2 || spot[0] != "signal" || spot[1] != "signal" {
		t.Errorf("non-HL should be probed signal(adx)+signal(composite), got %v", spot)
	}
	candles := calls["shared_scripts/fetch_candles.py"]
	if len(candles) != 1 || candles[0] != "signal" {
		t.Errorf("dashboard candle helper should be probed once, got %v", candles)
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

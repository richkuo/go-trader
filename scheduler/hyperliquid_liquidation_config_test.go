package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// #1450 — the bankruptcy-bound check must actually reach validateConfig, not
// only the pure helper. A pure-helper-only test would pass while the wiring was
// missing, which is exactly the class of gap this issue is about.
func liqValidationConfig(stopPct, leverage float64, mode, marginMode string) Config {
	sc := StrategyConfig{
		ID:             "hl-eth",
		Type:           "perps",
		Platform:       "hyperliquid",
		Script:         "shared_scripts/check_hyperliquid.py",
		Args:           []string{"sma_crossover", "ETH", "1h", "--mode=" + mode},
		Capital:        1000,
		MaxDrawdownPct: 40,
		Leverage:       leverage,
		MarginMode:     marginMode,
		StopLossPct:    floatPtr(stopPct),
	}
	return Config{
		Strategies:    []StrategyConfig{sc},
		PortfolioRisk: &PortfolioRiskConfig{MaxDrawdownPct: 25, WarnThresholdPct: 80},
	}
}

func TestConfigValidationRejectsStopPastBankruptcyDistance(t *testing.T) {
	// 10% stop at 20x: the position is bankrupt at 5%, so the stop can never fill.
	cfg := liqValidationConfig(10, 20, "live", "isolated")
	err := validateConfig(&cfg, true)
	if err == nil {
		t.Fatal("expected a validation error for a stop past the bankruptcy distance")
	}
	if !strings.Contains(err.Error(), "bankruptcy") {
		t.Fatalf("error %q should explain the bankruptcy distance", err.Error())
	}
}

func TestConfigValidationAcceptsAggressiveButReachableStops(t *testing.T) {
	cases := []struct {
		name                string
		stopPct, leverage   float64
		mode, marginMode    string
		expectValidationErr bool
	}{
		{"aggressive but valid at 20x", 4.9, 20, "live", "isolated", false},
		// The acceptance criterion: a valid low-leverage configuration passes.
		{"valid low leverage", 45, 2, "live", "isolated", false},
		{"paper skips entirely", 10, 20, "paper", "isolated", false},
		{"cross margin skips entirely", 10, 20, "live", "cross", false},
		{"impossible at 20x", 10, 20, "live", "isolated", true},
	}
	for _, c := range cases {
		cfg := liqValidationConfig(c.stopPct, c.leverage, c.mode, c.marginMode)
		err := validateConfig(&cfg, true)
		gotErr := err != nil && strings.Contains(err.Error(), "bankruptcy")
		if gotErr != c.expectValidationErr {
			t.Errorf("%s: bankruptcy error = %v, want %v (err=%v)", c.name, gotErr, c.expectValidationErr, err)
		}
	}
}

// #1450 review (optional 3): the fleet preflight
// (scripts/check-hl-stop-bankruptcy-bound.sh) exists so an operator can find
// offending deployments BEFORE the restart that would enforce the new fatal
// rule. It re-implements the bound in Python, so it can drift from the Go
// check; this test runs the real script against the same fixtures the Go check
// is asserted on and requires the two to agree.
func TestHLBankruptcyBoundPreflightMatchesGoCheck(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not available")
	}
	script, err := filepath.Abs(filepath.Join("..", "scripts", "check-hl-stop-bankruptcy-bound.sh"))
	if err != nil {
		t.Fatalf("resolve script path: %v", err)
	}
	if _, err := os.Stat(script); err != nil {
		t.Fatalf("preflight script missing: %v", err)
	}

	cases := []struct {
		name              string
		stopPct, leverage float64
		mode, marginMode  string
	}{
		// (a) an offending strategy that is PAPER must not be flagged.
		{"paper offender", 10, 20, "paper", "isolated"},
		// (b) the same numbers in CROSS margin must not be flagged.
		{"cross offender", 10, 20, "live", "cross"},
		// (c) the live isolated offender must be reported.
		{"live isolated offender", 10, 20, "live", "isolated"},
		{"aggressive but valid", 4.9, 20, "live", "isolated"},
		{"low leverage", 45, 2, "live", "isolated"},
	}
	for _, c := range cases {
		cfg := liqValidationConfig(c.stopPct, c.leverage, c.mode, c.marginMode)
		goRejects := false
		if err := validateConfig(&cfg, true); err != nil && strings.Contains(err.Error(), "bankruptcy") {
			goRejects = true
		}

		dir := t.TempDir()
		if err := os.MkdirAll(filepath.Join(dir, "scheduler"), 0o755); err != nil {
			t.Fatalf("%s: mkdir: %v", c.name, err)
		}
		cfg.ConfigVersion = CurrentConfigVersion
		blob, err := json.Marshal(cfg)
		if err != nil {
			t.Fatalf("%s: marshal: %v", c.name, err)
		}
		if err := os.WriteFile(filepath.Join(dir, "scheduler", "config.json"), blob, 0o644); err != nil {
			t.Fatalf("%s: write config: %v", c.name, err)
		}

		out, runErr := exec.Command("bash", script, dir).CombinedOutput()
		scriptRejects := false
		if ee, ok := runErr.(*exec.ExitError); ok {
			if ee.ExitCode() != 1 {
				t.Fatalf("%s: preflight exited %d, want 0 or 1:\n%s", c.name, ee.ExitCode(), out)
			}
			scriptRejects = true
		} else if runErr != nil {
			t.Fatalf("%s: preflight failed to run: %v\n%s", c.name, runErr, out)
		}
		if scriptRejects != goRejects {
			t.Errorf("%s: preflight rejects = %v, Go check rejects = %v — the two have drifted:\n%s",
				c.name, scriptRejects, goRejects, out)
		}
	}
}

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

// #1456 review (optional 3): the boot bound is an ENTRY-ANCHORED rule. A
// trailing trigger ratchets with the mark, so a trailing_stop_pct above
// 100/leverage is reachable once the position moves in favor — it must LOAD,
// and the runtime clamp handles the pre-move window.
func TestConfigValidationAllowsTrailingPctPastBankruptcyDistance(t *testing.T) {
	cfg := liqValidationConfig(0, 20, "live", "isolated")
	cfg.Strategies[0].StopLossPct = nil
	trailing := 10.0
	cfg.Strategies[0].TrailingStopPct = &trailing
	if err := validateConfig(&cfg, true); err != nil && strings.Contains(err.Error(), "bankruptcy") {
		t.Fatalf("trailing_stop_pct past 100/leverage must not be rejected at boot (anchor ratchets), got %v", err)
	}
}

// #1456 review (optional 3): the MaxDrawdownPct fallback IS entry-anchored and
// must satisfy the same bound as stop_loss_pct when it owns the stop.
func TestConfigValidationRejectsMaxDrawdownFallbackPastBankruptcyDistance(t *testing.T) {
	cfg := liqValidationConfig(0, 20, "live", "isolated")
	cfg.Strategies[0].StopLossPct = nil // all seven explicit owners absent → fallback owns
	cfg.Strategies[0].MaxDrawdownPct = 15
	err := validateConfig(&cfg, true)
	if err == nil || !strings.Contains(err.Error(), "bankruptcy") {
		t.Fatalf("max_drawdown_pct fallback of 15%% at 20x must be rejected like stop_loss_pct: 15%%, got %v", err)
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
		goRejects         bool
	}{
		// (a) an offending strategy that is PAPER must not be flagged.
		{"paper offender", 10, 20, "paper", "isolated", false},
		// (b) the same numbers in CROSS margin must not be flagged.
		{"cross offender", 10, 20, "live", "cross", false},
		// (c) the live isolated offender must be reported.
		{"live isolated offender", 10, 20, "live", "isolated", true},
		{"aggressive but valid", 4.9, 20, "live", "isolated", false},
		{"low leverage", 45, 2, "live", "isolated", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cfg := liqValidationConfig(c.stopPct, c.leverage, c.mode, c.marginMode)
			goRejects, _ := bankruptcyPreflightParity(t, cfg)
			if goRejects != c.goRejects {
				t.Fatalf("Go (LoadConfigForProbe) rejects=%v, want %v", goRejects, c.goRejects)
			}
		})
	}
}

// #1456 review (optional 3): the preflight must agree with the Go check on the
// NEW scope too — trailing_stop_pct no longer flagged, the max_drawdown_pct
// fallback now flagged.
func TestHLBankruptcyBoundPreflightMatchesGoCheckNewScope(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not available")
	}
	cases := []struct {
		name      string
		mutate    func(*Config)
		goRejects bool
	}{
		{"trailing above bound loads", func(c *Config) {
			trail := 10.0
			c.Strategies[0].StopLossPct = nil
			c.Strategies[0].TrailingStopPct = &trail
		}, false},
		{"max_drawdown fallback above bound rejected", func(c *Config) {
			// default_stop_loss_atr_mult=0 is what makes the fallback the
			// actual owner on a real load — otherwise LoadConfig's auto-default
			// attaches an ATR stop and the fallback never resolves.
			c.DefaultStopLossATRMult = floatPtr(0)
			c.Strategies[0].StopLossPct = nil
			c.Strategies[0].MaxDrawdownPct = 15
		}, true},
		{"max_drawdown fallback below bound loads", func(c *Config) {
			c.DefaultStopLossATRMult = floatPtr(0)
			c.Strategies[0].StopLossPct = nil
			c.Strategies[0].MaxDrawdownPct = 4
		}, false},
		{"stop_loss_pct still rejected", func(c *Config) {}, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			sc := StrategyConfig{
				ID: "hl-eth", Type: "perps", Platform: "hyperliquid",
				Script:  "shared_scripts/check_hyperliquid.py",
				Args:    []string{"sma_crossover", "ETH", "1h", "--mode=live"},
				Capital: 1000, MaxDrawdownPct: 40, Leverage: 20,
				MarginMode: "isolated", StopLossPct: floatPtr(10),
			}
			cfg := Config{
				Strategies:    []StrategyConfig{sc},
				PortfolioRisk: &PortfolioRiskConfig{MaxDrawdownPct: 25, WarnThresholdPct: 80},
			}
			c.mutate(&cfg)
			goRejects, _ := bankruptcyPreflightParity(t, cfg)
			if goRejects != c.goRejects {
				t.Fatalf("Go (LoadConfigForProbe) rejects=%v, want %v", goRejects, c.goRejects)
			}
		})
	}
}

// regimeATRBlockFromRaw builds an UNRESOLVED block from a raw JSON shape — the
// exact state validateConfig's per-strategy loop sees, since ResolveSurface only
// runs later in validateRegimeATRConfig. MarshalJSON renders the raw shape back
// out, so the fixture round-trips to the file the preflight script reads.
func regimeATRBlockFromRaw(raw map[string]interface{}) *RegimeATRBlock {
	return &RegimeATRBlock{raw: raw}
}

// bankruptcyPreflightParity writes cfg to a throwaway deployment tree and
// returns (goRejects, scriptRejects) for the SAME file.
//
// #1456 review round 4: the Go side goes through LoadConfigForProbe, not a bare
// validateConfig. validateConfig runs LAST in loadConfig, after two injections
// that attach a stop owner the strategy's own JSON never carries (the
// #562/#601/#605 default_stop_loss_atr_mult auto-default and the #1133
// user_defaults.close ratchet-regime trail). Comparing the script against a
// pre-resolution validateConfig would assert parity with a state the daemon
// never boots from, which is exactly the drift class this pinning exists for.
func bankruptcyPreflightParity(t *testing.T, cfg Config) (bool, bool) {
	t.Helper()
	script, err := filepath.Abs(filepath.Join("..", "scripts", "check-hl-stop-bankruptcy-bound.sh"))
	if err != nil {
		t.Fatalf("resolve script path: %v", err)
	}
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "scheduler"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	cfg.ConfigVersion = CurrentConfigVersion
	blob, err := json.Marshal(cfg)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	path := filepath.Join(dir, "scheduler", "config.json")
	if err := os.WriteFile(path, blob, 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	goRejects := false
	if _, err := LoadConfigForProbe(path); err != nil {
		if !strings.Contains(err.Error(), "bankruptcy") {
			// A fixture that fails to load for an unrelated reason carries no
			// parity signal — fail loudly instead of scoring it as "accepted".
			t.Fatalf("fixture failed to load for a non-bankruptcy reason: %v\nconfig: %s", err, blob)
		}
		goRejects = true
	}

	out, runErr := exec.Command("bash", script, dir).CombinedOutput()
	scriptRejects := false
	if ee, ok := runErr.(*exec.ExitError); ok {
		if ee.ExitCode() != 1 {
			t.Fatalf("preflight exited %d, want 0 or 1:\n%s", ee.ExitCode(), out)
		}
		scriptRejects = true
	} else if runErr != nil {
		t.Fatalf("preflight failed to run: %v\n%s", runErr, out)
	}
	if scriptRejects != goRejects {
		t.Errorf("preflight rejects=%v, Go (LoadConfigForProbe) rejects=%v — drifted:\nconfig: %s\n%s",
			scriptRejects, goRejects, blob, out)
	}
	return goRejects, scriptRejects
}

// #1456 review round 4: the preflight reads raw per-strategy JSON, but the Go
// check runs after LoadConfig has resolved stop ownership. Three shapes drift if
// the script does not mirror that resolution — an explicit-zero ATR scalar
// (which falls THROUGH to the max_drawdown_pct fallback, the dangerous
// direction: the preflight would clear a config the restart then rejects), and
// the two load-time injections (which attach an owner the JSON never shows, so
// the preflight would raise a false alarm and block a healthy update).
func TestHLBankruptcyBoundPreflightMatchesGoCheckLoadTimeResolution(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not available")
	}

	// max_drawdown_pct 15% at 20x leverage: bound is 5%, so whenever the
	// fallback OWNS the stop the config must be rejected, and whenever some
	// other owner resolves it must load.
	base := func() StrategyConfig {
		return StrategyConfig{
			ID: "hl-eth", Type: "perps", Platform: "hyperliquid",
			Script:  "shared_scripts/check_hyperliquid.py",
			Args:    []string{"sma_crossover", "ETH", "1h", "--mode=live"},
			Capital: 1000, MaxDrawdownPct: 15, Leverage: 20,
			MarginMode: "isolated",
		}
	}

	cases := []struct {
		name      string
		mutate    func(*Config)
		goRejects bool
	}{
		{
			// Reviewer's case (a). trailing_stop_atr_mult: 0 is a legal shape
			// (validateConfig only requires >= 0) and EffectiveStopLossPct
			// documents that it falls through — so max_drawdown_pct owns the
			// stop and the bound applies.
			name: "explicit-zero trailing_stop_atr_mult falls through to the fallback",
			mutate: func(c *Config) {
				c.Strategies[0].TrailingStopATRMult = floatPtr(0)
			},
			goRejects: true,
		},
		{
			name: "explicit-zero stop_loss_atr_mult falls through to the fallback",
			mutate: func(c *Config) {
				c.Strategies[0].StopLossATRMult = floatPtr(0)
			},
			goRejects: true,
		},
		{
			// A positive explicit ATR scalar DOES own the stop, so the fallback
			// never resolves and the bound must not fire.
			name: "positive stop_loss_atr_mult owns the stop",
			mutate: func(c *Config) {
				c.Strategies[0].StopLossATRMult = floatPtr(1.5)
			},
			goRejects: false,
		},
		{
			// All stop fields omitted: LoadConfig's default_stop_loss_atr_mult
			// auto-default (1.0 when unset) attaches an ATR owner, so the
			// fallback does NOT resolve and the config loads clean.
			name:      "auto-default attaches an ATR owner when every stop field is omitted",
			mutate:    func(c *Config) {},
			goRejects: false,
		},
		{
			// The documented way to actually reach the fallback: opt out of the
			// auto-default. Now max_drawdown_pct owns the stop and 15% >= 5%.
			name: "default_stop_loss_atr_mult=0 lets the fallback own the stop",
			mutate: func(c *Config) {
				c.DefaultStopLossATRMult = floatPtr(0)
			},
			goRejects: true,
		},
		{
			// Same opt-out, but under the bound — must load in both.
			name: "default_stop_loss_atr_mult=0 with the fallback inside the bound",
			mutate: func(c *Config) {
				c.DefaultStopLossATRMult = floatPtr(0)
				c.Strategies[0].MaxDrawdownPct = 4
			},
			goRejects: false,
		},
		{
			// #1133: the ratchet-regime trail is injected from user_defaults and
			// never appears in the strategy's own JSON, so a raw-JSON read would
			// score the fallback as the owner and raise a false alarm.
			name: "user_defaults ratchet-regime trail owns the stop",
			mutate: func(c *Config) {
				c.DefaultStopLossATRMult = floatPtr(0)
				c.Regime = &RegimeConfig{Enabled: true, Period: 14, ADXThreshold: 20}
				c.Strategies[0].CloseStrategy = &StrategyRef{
					Name:   trailingTPRatchetRegimeCloseName,
					Params: map[string]interface{}{"use_defaults": true},
				}
				c.UserDefaults = &UserDefaultsConfig{Close: CloseDefaultsMap{
					trailingTPRatchetRegimeCloseName: map[string]interface{}{
						"tp_tiers":                 ratchetRegimeUserTiers(),
						"trailing_stop_atr_regime": ratchetRegimeTrailRaw(2.25, 2.25, 1.25),
					},
				}}
			},
			goRejects: false,
		},
		{
			// The Go bound runs from validateConfig's per-strategy loop, BEFORE
			// validateRegimeATRConfig resolves the raw regime blocks. An explicit
			// strategy-level regime owner must still be seen as the owner — the
			// pre-resolution IsZero() read scored it as absent and refused to
			// boot a legal config (#1456 review round 4).
			name: "explicit stop_loss_atr_regime owns the stop",
			mutate: func(c *Config) {
				c.DefaultStopLossATRMult = floatPtr(0)
				c.Regime = &RegimeConfig{Enabled: true, Period: 14, ADXThreshold: 20}
				c.Strategies[0].StopLossATRRegime = regimeATRBlockFromRaw(ratchetRegimeTrailRaw(2.0, 2.0, 1.5))
			},
			goRejects: false,
		},
		{
			// Same shape on the trailing surface.
			name: "explicit trailing_stop_atr_regime owns the stop",
			mutate: func(c *Config) {
				c.DefaultStopLossATRMult = floatPtr(0)
				c.Regime = &RegimeConfig{Enabled: true, Period: 14, ADXThreshold: 20}
				c.Strategies[0].TrailingStopATRRegime = regimeATRBlockFromRaw(ratchetRegimeTrailRaw(2.0, 2.0, 1.5))
			},
			goRejects: false,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cfg := Config{
				Strategies:    []StrategyConfig{base()},
				PortfolioRisk: &PortfolioRiskConfig{MaxDrawdownPct: 25, WarnThresholdPct: 80},
			}
			c.mutate(&cfg)
			goRejects, _ := bankruptcyPreflightParity(t, cfg)
			if goRejects != c.goRejects {
				t.Fatalf("Go (LoadConfigForProbe) rejects=%v, want %v", goRejects, c.goRejects)
			}
		})
	}
}

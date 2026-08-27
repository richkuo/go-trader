package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

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

func TestConfigValidationAllowsTrailingPctPastBankruptcyDistance(t *testing.T) {
	cfg := liqValidationConfig(0, 20, "live", "isolated")
	cfg.Strategies[0].StopLossPct = nil
	trailing := 10.0
	cfg.Strategies[0].TrailingStopPct = &trailing
	if err := validateConfig(&cfg, true); err != nil && strings.Contains(err.Error(), "bankruptcy") {
		t.Fatalf("trailing_stop_pct past 100/leverage must not be rejected at boot (anchor ratchets), got %v", err)
	}
}

func TestConfigValidationRejectsMaxDrawdownFallbackPastBankruptcyDistance(t *testing.T) {
	cfg := liqValidationConfig(0, 20, "live", "isolated")
	cfg.Strategies[0].StopLossPct = nil
	cfg.Strategies[0].MaxDrawdownPct = 15
	err := validateConfig(&cfg, true)
	if err == nil || !strings.Contains(err.Error(), "bankruptcy") {
		t.Fatalf("max_drawdown_pct fallback of 15%% at 20x must be rejected like stop_loss_pct: 15%%, got %v", err)
	}
}

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
		{"paper offender", 10, 20, "paper", "isolated", false},
		{"cross offender", 10, 20, "live", "cross", false},
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

func regimeATRBlockFromRaw(raw map[string]interface{}) *RegimeATRBlock {
	return &RegimeATRBlock{raw: raw}
}

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

func TestHLBankruptcyBoundPreflightMatchesGoCheckLoadTimeResolution(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not available")
	}

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
			name: "positive stop_loss_atr_mult owns the stop",
			mutate: func(c *Config) {
				c.Strategies[0].StopLossATRMult = floatPtr(1.5)
			},
			goRejects: false,
		},
		{
			name:      "auto-default attaches an ATR owner when every stop field is omitted",
			mutate:    func(c *Config) {},
			goRejects: false,
		},
		{
			name: "default_stop_loss_atr_mult=0 lets the fallback own the stop",
			mutate: func(c *Config) {
				c.DefaultStopLossATRMult = floatPtr(0)
			},
			goRejects: true,
		},
		{
			name: "default_stop_loss_atr_mult=0 with the fallback inside the bound",
			mutate: func(c *Config) {
				c.DefaultStopLossATRMult = floatPtr(0)
				c.Strategies[0].MaxDrawdownPct = 4
			},
			goRejects: false,
		},
		{
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
			name: "explicit stop_loss_atr_regime owns the stop",
			mutate: func(c *Config) {
				c.DefaultStopLossATRMult = floatPtr(0)
				c.Regime = &RegimeConfig{Enabled: true, Period: 14, ADXThreshold: 20}
				c.Strategies[0].StopLossATRRegime = regimeATRBlockFromRaw(ratchetRegimeTrailRaw(2.0, 2.0, 1.5))
			},
			goRejects: false,
		},
		{
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

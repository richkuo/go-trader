package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadConfigReadOnlyNeverRewritesTheFile(t *testing.T) {
	cases := []struct {
		name string
		raw  string
	}{
		{"v18 file behind a symlink", v19LegacyStrategyConfigJSON},
		{"v19 file with a legacy key", `{"config_version": 19, "regime": {"enabled": true, "period": 14, "adx_threshold": 20}, "strategies": [{"id": "hl-a", "type": "perps", "platform": "hyperliquid", "script": "shared_scripts/check_hyperliquid.py", "args": ["sma_crossover", "ETH", "1h", "--mode=paper"], "capital": 1000, "leverage": 3, "trail_stop_atr_regime": {"trend_regime": {"trending_up": {"atr_multiple": 2.5}, "trending_down": {"atr_multiple": 2.5}, "ranging": {"atr_multiple": 2.0}}}}]}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			real := filepath.Join(dir, "real.json")
			if err := os.WriteFile(real, []byte(tc.raw), 0o644); err != nil {
				t.Fatal(err)
			}
			link := filepath.Join(dir, "config.json")
			if err := os.Symlink(real, link); err != nil {
				t.Fatal(err)
			}
			before, err := os.Lstat(link)
			if err != nil {
				t.Fatal(err)
			}
			cfg, err := LoadConfigReadOnly(link)
			if err != nil {
				t.Fatalf("LoadConfigReadOnly: %v", err)
			}
			if len(cfg.Strategies) != 1 || cfg.Strategies[0].TrailingStopATRMultRegime == nil {
				t.Fatalf("read-only load did not apply the migration in memory: %+v", cfg.Strategies)
			}
			after, err := os.Lstat(link)
			if err != nil {
				t.Fatal(err)
			}
			if after.Mode()&os.ModeSymlink == 0 {
				t.Fatalf("read-only load replaced the config symlink with a regular file")
			}
			got, err := os.ReadFile(real)
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != tc.raw {
				t.Fatalf("read-only load rewrote the config file:\n%s", got)
			}
			if !before.ModTime().Equal(after.ModTime()) {
				t.Fatalf("read-only load touched the config symlink")
			}
			if _, err := os.Stat(real + ".tmp"); !os.IsNotExist(err) {
				t.Fatalf("read-only load left a migration temp file")
			}
			written, err := LoadConfig(link)
			if err != nil {
				t.Fatalf("LoadConfig: %v", err)
			}
			if written.Strategies[0].TrailingStopATRMultRegime == nil {
				t.Fatalf("writing loader lost the migrated field")
			}
		})
	}
}

package main

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestFormatStorageAliasLines(t *testing.T) {
	dir := t.TempDir()
	cfg := &Config{
		DBFile:      filepath.Join(dir, "live.db"),
		PaperDBFile: filepath.Join(dir, "paper.db"),
		Strategies: []StrategyConfig{
			{ID: "hl-x", Type: "perps", Platform: "hyperliquid", Args: []string{"vwap", "ETH", "1h", "--mode=live"}},
			{ID: "hl-x-paper", StorageStrategyID: "hl-x", Type: "perps", Platform: "hyperliquid", Args: []string{"vwap", "ETH", "1h"}},
			{ID: "hl-b-paper", StorageStrategyID: "hl-a", Type: "perps", Platform: "hyperliquid", Args: []string{"vwap", "BTC", "1h"}},
			{ID: "spot-y", Type: "spot", Args: []string{"sma", "BTC/USDT", "1h"}},
		},
	}
	layout, err := resolveStorageLayout(cfg)
	if err != nil {
		t.Fatal(err)
	}
	ident, err := buildStorageIdentityMap(cfg, layout)
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Join(formatStorageAliasLines(ident, layout), "\n")
	want := "[storage]   alias paper: hl-b-paper -> hl-a\n[storage]   alias paper: hl-x-paper -> hl-x"
	if got != want {
		t.Fatalf("alias lines:\n%s\nwant:\n%s", got, want)
	}
	single := &Config{DBFile: filepath.Join(dir, "one.db"), Strategies: cfg.Strategies[:1]}
	layout, err = resolveStorageLayout(single)
	if err != nil {
		t.Fatal(err)
	}
	ident, err = buildStorageIdentityMap(single, layout)
	if err != nil {
		t.Fatal(err)
	}
	if lines := formatStorageAliasLines(ident, layout); len(lines) != 0 {
		t.Fatalf("no alias expected, got %v", lines)
	}
}

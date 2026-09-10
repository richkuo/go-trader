package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestConfigExampleCopyDoesNotTriggerTheUpgradePath(t *testing.T) {
	t.Setenv("HYPERLIQUID_SECRET_KEY", "example")
	for _, example := range []string{"config.example.json", "config.live-paper.example.json"} {
		t.Run(example, func(t *testing.T) {
			orig, err := os.ReadFile(example)
			if err != nil {
				t.Fatalf("read %s: %v", example, err)
			}
			path := filepath.Join(t.TempDir(), "config.json")
			if err := os.WriteFile(path, orig, 0644); err != nil {
				t.Fatalf("write fixture: %v", err)
			}
			before, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("read fixture: %v", err)
			}
			cfg, err := LoadConfig(path)
			if err != nil {
				t.Fatalf("LoadConfig on a fresh copy of %s: %v", example, err)
			}
			if got := cfg.MigrationBaseVersion(); got < CurrentConfigVersion {
				t.Fatalf("a fresh copy of %s reports MigrationBaseVersion %d < %d, so main.go's "+
					"startup path would spawn runConfigMigrationDM and DM a brand-new operator that their "+
					"config was upgraded", example, got, CurrentConfigVersion)
			}
			after, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("re-read fixture: %v", err)
			}
			if string(before) != string(after) {
				t.Fatalf("loading a fresh copy of %s rewrote it on disk; the shipped example should "+
					"already be in current form", example)
			}
			if example == "config.live-paper.example.json" {
				layout, err := resolveStorageLayout(cfg)
				if err != nil {
					t.Fatalf("resolveStorageLayout: %v", err)
				}
				if !layout.Split {
					t.Fatalf("the live+paper example must resolve to a split layout, got %s", layout.describe())
				}
				ident, err := buildStorageIdentityMap(cfg, layout)
				if err != nil {
					t.Fatalf("buildStorageIdentityMap: %v", err)
				}
				if got, _ := ident.processFor(storageRolePaper, "hl-vwap-eth-60"); got != "hl-vwap-eth-60-paper" {
					t.Fatalf("paper alias maps to %q, want hl-vwap-eth-60-paper", got)
				}
				if got, _ := ident.processFor(storageRolePaper, "hl-rsi-btc-60"); got != "hl-rsi-btc-60-paper" {
					t.Fatalf("the paper alias with no live twin maps to %q, want hl-rsi-btc-60-paper", got)
				}
			}
		})
	}
}

package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestConfigExampleCopyDoesNotTriggerTheUpgradePath(t *testing.T) {
	orig, err := os.ReadFile("config.example.json")
	if err != nil {
		t.Fatalf("read config.example.json: %v", err)
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
		t.Fatalf("LoadConfig on a fresh copy of the example: %v", err)
	}
	if got := cfg.MigrationBaseVersion(); got < CurrentConfigVersion {
		t.Fatalf("a fresh copy of the example reports MigrationBaseVersion %d < %d, so main.go's "+
			"startup path would spawn runConfigMigrationDM and DM a brand-new operator that their "+
			"config was upgraded", got, CurrentConfigVersion)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("re-read fixture: %v", err)
	}
	if string(before) != string(after) {
		t.Fatalf("loading a fresh copy of the example rewrote it on disk; the shipped example should " +
			"already be in current form")
	}
}

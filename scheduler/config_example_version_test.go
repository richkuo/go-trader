package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestConfigExampleDeclaresCurrentConfigVersion(t *testing.T) {
	data, err := os.ReadFile("config.example.json")
	if err != nil {
		t.Fatalf("read config.example.json: %v", err)
	}
	if got := rawConfigVersion(data); got != CurrentConfigVersion {
		t.Fatalf("config.example.json declares config_version %d, want %d — the example ships the "+
			"current canonical key spellings, so a lower stamp is a version no real config could carry "+
			"and makes a fresh copy announce a spurious upgrade at first boot", got, CurrentConfigVersion)
	}
	if needsV19AtrMultRegimeRename(data) || needsV18TrailStopKeyMigration(data) {
		t.Fatalf("config.example.json still carries a legacy ATR-regime stop spelling")
	}
}

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

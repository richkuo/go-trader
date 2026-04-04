package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestNewLogManager(t *testing.T) {
	dir := t.TempDir()
	logDir := filepath.Join(dir, "logs")

	lm, err := NewLogManager(logDir)
	if err != nil {
		t.Fatalf("NewLogManager failed: %v", err)
	}
	defer lm.Close()

	// Directory should have been created
	if _, err := os.Stat(logDir); err != nil {
		t.Errorf("log directory should exist: %v", err)
	}
}

func TestNewLogManagerEmptyDir(t *testing.T) {
	lm, err := NewLogManager("")
	if err != nil {
		t.Fatalf("NewLogManager('') failed: %v", err)
	}
	defer lm.Close()
	// Should succeed with no directory creation
}

func TestGetStrategyLogger(t *testing.T) {
	dir := t.TempDir()
	logDir := filepath.Join(dir, "logs")

	lm, err := NewLogManager(logDir)
	if err != nil {
		t.Fatal(err)
	}
	defer lm.Close()

	sl, err := lm.GetStrategyLogger("test-strategy")
	if err != nil {
		t.Fatalf("GetStrategyLogger failed: %v", err)
	}
	defer sl.Close()

	// Write some logs
	sl.Info("test info message")
	sl.Error("test error message")
	sl.Warn("test warn message")

	// Flush by closing
	sl.Close()

	// Check log file exists and has content
	logFile := filepath.Join(logDir, "test-strategy.log")
	data, err := os.ReadFile(logFile)
	if err != nil {
		t.Fatalf("log file should exist: %v", err)
	}

	content := string(data)
	if !strings.Contains(content, "[INFO]") {
		t.Error("log should contain [INFO]")
	}
	if !strings.Contains(content, "[ERROR]") {
		t.Error("log should contain [ERROR]")
	}
	if !strings.Contains(content, "[WARN]") {
		t.Error("log should contain [WARN]")
	}
	if !strings.Contains(content, "test-strategy") {
		t.Error("log should contain strategy ID")
	}
	if !strings.Contains(content, "test info message") {
		t.Error("log should contain the message text")
	}
}

func TestGetStrategyLoggerNoDir(t *testing.T) {
	lm, err := NewLogManager("")
	if err != nil {
		t.Fatal(err)
	}
	defer lm.Close()

	sl, err := lm.GetStrategyLogger("test")
	if err != nil {
		t.Fatalf("GetStrategyLogger should succeed even without log dir: %v", err)
	}
	defer sl.Close()

	// Should not panic when logging
	sl.Info("test message")
}

func TestStrategyLoggerCloseIdempotent(t *testing.T) {
	dir := t.TempDir()
	lm, _ := NewLogManager(filepath.Join(dir, "logs"))
	defer lm.Close()

	sl, _ := lm.GetStrategyLogger("test")
	sl.Close()
	sl.Close() // should not panic
}

func TestLogManagerClose(t *testing.T) {
	dir := t.TempDir()
	lm, _ := NewLogManager(filepath.Join(dir, "logs"))
	lm.Close() // should not panic
	lm.Close() // idempotent
}

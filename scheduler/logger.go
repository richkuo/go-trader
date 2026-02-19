package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"
)

// StrategyLogger handles per-strategy logging to stdout and optionally a file.
type StrategyLogger struct {
	stratID string
	writer  io.Writer
	file    *os.File
}

// LogManager creates and manages loggers.
type LogManager struct {
	logDir string
}

func NewLogManager(logDir string) (*LogManager, error) {
	if logDir != "" {
		if err := os.MkdirAll(logDir, 0755); err != nil {
			return nil, fmt.Errorf("failed to create log directory %s: %w", logDir, err)
		}
	}
	return &LogManager{logDir: logDir}, nil
}

func (lm *LogManager) Close() {}

func (lm *LogManager) GetStrategyLogger(stratID string) (*StrategyLogger, error) {
	sl := &StrategyLogger{
		stratID: stratID,
		writer:  os.Stdout,
	}

	if lm.logDir != "" {
		filePath := filepath.Join(lm.logDir, stratID+".log")
		f, err := os.OpenFile(filePath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
		if err != nil {
			fmt.Printf("[WARN] Failed to open log file %s: %v\n", filePath, err)
		} else {
			sl.file = f
			sl.writer = io.MultiWriter(os.Stdout, f)
		}
	}

	return sl, nil
}

func (sl *StrategyLogger) Close() {
	if sl.file != nil {
		sl.file.Close()
		sl.file = nil
	}
}

func (sl *StrategyLogger) log(level, format string, args ...interface{}) {
	now := time.Now().UTC().Format("2006-01-02 15:04:05")
	msg := fmt.Sprintf(format, args...)
	fmt.Fprintf(sl.writer, "[%s] [%s] [%s] %s\n", now, sl.stratID, level, msg)
}

func (sl *StrategyLogger) Info(format string, args ...interface{}) {
	sl.log("INFO", format, args...)
}

func (sl *StrategyLogger) Error(format string, args ...interface{}) {
	sl.log("ERROR", format, args...)
}

func (sl *StrategyLogger) Warn(format string, args ...interface{}) {
	sl.log("WARN", format, args...)
}

// LogSummary writes a cycle summary to stdout.
func (lm *LogManager) LogSummary(cycle int, elapsed time.Duration, stratCount int, trades int, totalValue float64) {
	now := time.Now().UTC().Format("2006-01-02 15:04 UTC")
	fmt.Printf("[%s] Cycle %d complete (%.1fs) | %d strategies checked | %d trades | Total value: $%.2f\n",
		now, cycle, elapsed.Seconds(), stratCount, trades, totalValue)
}

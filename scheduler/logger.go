package main

import (
	"fmt"
	"time"
)

// StrategyLogger handles per-strategy logging to stdout only.
type StrategyLogger struct {
	stratID string
}

// LogManager creates and manages loggers (stdout only, no files).
type LogManager struct{}

func NewLogManager(_ string) (*LogManager, error) {
	return &LogManager{}, nil
}

func (lm *LogManager) Close() {}

func (lm *LogManager) GetStrategyLogger(stratID string) (*StrategyLogger, error) {
	return &StrategyLogger{stratID: stratID}, nil
}

func (sl *StrategyLogger) Close() {}

func (sl *StrategyLogger) log(level, format string, args ...interface{}) {
	now := time.Now().UTC().Format("2006-01-02 15:04:05")
	msg := fmt.Sprintf(format, args...)
	fmt.Printf("[%s] [%s] [%s] %s\n", now, sl.stratID, level, msg)
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

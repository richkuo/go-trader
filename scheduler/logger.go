package main

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"time"
)

// StrategyLogger handles per-strategy and combined logging.
type StrategyLogger struct {
	stratFile   *os.File
	stratLogger *log.Logger
	combined    *log.Logger
	stratID     string
}

// LogManager creates and manages loggers.
type LogManager struct {
	logDir      string
	combinedFile *os.File
	combined    *log.Logger
}

func NewLogManager(logDir string) (*LogManager, error) {
	if err := os.MkdirAll(logDir, 0755); err != nil {
		return nil, fmt.Errorf("create log dir: %w", err)
	}

	combinedPath := filepath.Join(logDir, "scheduler.log")
	f, err := os.OpenFile(combinedPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		return nil, fmt.Errorf("open combined log: %w", err)
	}

	return &LogManager{
		logDir:      logDir,
		combinedFile: f,
		combined:    log.New(f, "", 0),
	}, nil
}

func (lm *LogManager) Close() {
	if lm.combinedFile != nil {
		lm.combinedFile.Close()
	}
}

func (lm *LogManager) GetStrategyLogger(stratID string) (*StrategyLogger, error) {
	path := filepath.Join(lm.logDir, fmt.Sprintf("%s.log", stratID))
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		return nil, fmt.Errorf("open strategy log: %w", err)
	}

	return &StrategyLogger{
		stratFile:   f,
		stratLogger: log.New(f, "", 0),
		combined:    lm.combined,
		stratID:     stratID,
	}, nil
}

func (sl *StrategyLogger) Close() {
	if sl.stratFile != nil {
		sl.stratFile.Close()
	}
}

func (sl *StrategyLogger) log(level, format string, args ...interface{}) {
	now := time.Now().UTC().Format("2006-01-02 15:04:05")
	msg := fmt.Sprintf(format, args...)
	line := fmt.Sprintf("[%s] [%s] [%s] %s", now, sl.stratID, level, msg)

	sl.stratLogger.Println(line)
	sl.combined.Println(line)
	fmt.Println(line)
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

// LogSummary writes a cycle summary to combined log and stdout.
func (lm *LogManager) LogSummary(cycle int, elapsed time.Duration, stratCount int, trades int, totalValue float64) {
	now := time.Now().UTC().Format("2006-01-02 15:04 UTC")
	line := fmt.Sprintf("[%s] Cycle %d complete (%.1fs) | %d strategies checked | %d trades | Total value: $%.2f",
		now, cycle, elapsed.Seconds(), stratCount, trades, totalValue)
	lm.combined.Println(line)
	fmt.Println(line)
}

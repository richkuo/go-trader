package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	logLevelInfo  = "info"
	logLevelDebug = "debug"
)

var debugLogEnabled atomic.Bool

func parseLogLevel(s string) (bool, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", logLevelInfo:
		return false, nil
	case logLevelDebug:
		return true, nil
	}
	return false, fmt.Errorf("invalid log_level %q: want %q or %q", s, logLevelInfo, logLevelDebug)
}

func applyLogLevelFromConfig(cfg *Config) error {
	if cfg == nil {
		return nil
	}
	debug, err := parseLogLevel(cfg.LogLevel)
	if err != nil {
		return err
	}
	debugLogEnabled.Store(debug)
	return nil
}

func debugLogging() bool {
	return debugLogEnabled.Load()
}

func logDebugf(format string, args ...interface{}) {
	if debugLogging() {
		fmt.Printf(format, args...)
	}
}

type logChangeGate struct {
	mu   sync.Mutex
	last map[string]string
}

var logChanges = &logChangeGate{}

func (g *logChangeGate) changed(key, value string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.last == nil {
		g.last = make(map[string]string)
	}
	prev, seen := g.last[key]
	g.last[key] = value
	return !seen || prev != value
}

func logOnChangef(key, value, format string, args ...interface{}) {
	if logChanges.changed(key, value) || debugLogging() {
		fmt.Printf(format, args...)
	}
}

var scriptProgressLine = regexp.MustCompile(`^(Fetching .+\.\.\.|Funding rate \S+: current=\S+ avg7d=\S+|Funding history \S+: \d+ records since bar0|Merged pair: \d+ aligned candles \(.+\))$`)

func stripScriptProgressLines(stderr string) string {
	var kept []string
	for _, line := range strings.Split(stderr, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || scriptProgressLine.MatchString(trimmed) {
			continue
		}
		kept = append(kept, strings.TrimRight(line, "\r"))
	}
	return strings.Join(kept, "\n")
}

func scriptStderrLogText(stderr string) (string, bool, bool) {
	if stderr == "" {
		return "", false, false
	}
	kept := stripScriptProgressLines(stderr)
	if debugLogging() {
		return stderr, true, kept != ""
	}
	return kept, kept != "", kept != ""
}

const logArgElideBytes = 256

func elideLongArgs(args []string) []string {
	out := make([]string, len(args))
	for i, arg := range args {
		if len(arg) <= logArgElideBytes {
			out[i] = arg
			continue
		}
		if name, _, ok := strings.Cut(arg, "="); ok && strings.HasPrefix(name, "--") {
			out[i] = fmt.Sprintf("%s=<%d bytes>", name, len(arg)-len(name)-1)
			continue
		}
		out[i] = fmt.Sprintf("<%d bytes>", len(arg))
	}
	return out
}

type StrategyLogger struct {
	stratID string
	writer  io.Writer
	file    *os.File
}

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

func (sl *StrategyLogger) Debug(format string, args ...interface{}) {
	if debugLogging() {
		sl.log("DEBUG", format, args...)
	}
}

func (sl *StrategyLogger) InfoOrDebug(info bool, format string, args ...interface{}) {
	if info {
		sl.Info(format, args...)
		return
	}
	sl.Debug(format, args...)
}

func (sl *StrategyLogger) Changed(topic, value string) bool {
	return logChanges.changed(sl.stratID+"\x00"+topic, value)
}

func (sl *StrategyLogger) InfoOnChange(topic, value, format string, args ...interface{}) {
	sl.InfoOrDebug(sl.Changed(topic, value), format, args...)
}

func (sl *StrategyLogger) ScriptStderr(stderr string) {
	text, show, info := scriptStderrLogText(stderr)
	if show {
		sl.InfoOrDebug(info, "stderr: %s", text)
	}
}

func (sl *StrategyLogger) Running(script string, args []string) {
	sl.Debug("Running: python3 %s %v", script, args)
}

func (sl *StrategyLogger) RunningOnFailure(script string, args []string) {
	if debugLogging() {
		return
	}
	sl.Info("Running: python3 %s %v", script, elideLongArgs(args))
}

func (lm *LogManager) LogSummary(cycle int, elapsed time.Duration, stratCount int, trades int, totalValue float64) {
	now := time.Now().UTC().Format("2006-01-02 15:04 UTC")
	fmt.Printf("[%s] Cycle %d complete (%.1fs) | %d strategies checked | %d trades | Total value: $%.2f\n",
		now, cycle, elapsed.Seconds(), stratCount, trades, totalValue)
}

func logLevelStartupLine() string {
	if debugLogging() {
		return "Log level: debug (per-check detail on)"
	}
	return "Log level: info (per-check detail hidden; set log_level to \"debug\" to show it)"
}

func formatPricesLogLine(prices map[string]float64) string {
	syms := make([]string, 0, len(prices))
	for sym := range prices {
		syms = append(syms, sym)
	}
	sort.Strings(syms)
	var b strings.Builder
	b.WriteString("Prices:")
	for _, sym := range syms {
		fmt.Fprintf(&b, " %s=$%.2f", sym, prices[sym])
	}
	return b.String()
}

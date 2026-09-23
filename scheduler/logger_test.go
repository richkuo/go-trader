package main

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

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

	sl.Info("test info message")
	sl.Error("test error message")
	sl.Warn("test warn message")

	sl.Close()

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

	sl.Info("test message")
}

func TestStrategyLoggerCloseIdempotent(t *testing.T) {
	dir := t.TempDir()
	lm, _ := NewLogManager(filepath.Join(dir, "logs"))
	defer lm.Close()

	sl, _ := lm.GetStrategyLogger("test")
	sl.Close()
	sl.Close()
}

func TestLogManagerClose(t *testing.T) {
	dir := t.TempDir()
	lm, _ := NewLogManager(filepath.Join(dir, "logs"))
	lm.Close()
	lm.Close()
}

func withLogLevel(t *testing.T, debug bool) {
	t.Helper()
	prev := debugLogEnabled.Load()
	debugLogEnabled.Store(debug)
	t.Cleanup(func() { debugLogEnabled.Store(prev) })
}

func logLineShapes(out string) []string {
	var shapes []string
	for _, line := range strings.Split(strings.TrimRight(out, "\n"), "\n") {
		if line == "" {
			continue
		}
		parts := strings.SplitN(line, "] ", 4)
		if len(parts) != 4 {
			shapes = append(shapes, "cont "+line)
			continue
		}
		level := strings.TrimPrefix(parts[2], "[")
		head, _, _ := strings.Cut(parts[3], ":")
		shapes = append(shapes, level+" "+head)
	}
	return shapes
}

func TestLoadConfigLogLevel(t *testing.T) {
	withLogLevel(t, false)
	cases := []struct {
		name      string
		field     string
		wantErr   bool
		wantDebug bool
	}{
		{name: "omitted is info", field: ``, wantDebug: false},
		{name: "info", field: `"log_level": "info",`, wantDebug: false},
		{name: "debug", field: `"log_level": "debug",`, wantDebug: true},
		{name: "case and space", field: `"log_level": " DEBUG ",`, wantDebug: true},
		{name: "unknown value fails the load", field: `"log_level": "verbose",`, wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			debugLogEnabled.Store(!tc.wantDebug)
			path := filepath.Join(t.TempDir(), "config.json")
			cfgJSON := `{` + tc.field + `
				"strategies": [{
					"id": "hl-sole",
					"type": "perps",
					"platform": "hyperliquid",
					"script": "shared_scripts/check_hyperliquid.py",
					"args": ["sma_crossover", "ETH", "1h", "--mode=paper"],
					"capital": 1000,
					"max_drawdown_pct": 10,
					"leverage": 5
				}]
			}`
			if err := os.WriteFile(path, []byte(cfgJSON), 0o644); err != nil {
				t.Fatalf("write config: %v", err)
			}
			cfg, err := LoadConfigForProbe(path)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("LoadConfigForProbe accepted log_level in %s", tc.field)
				}
				return
			}
			if err != nil {
				t.Fatalf("LoadConfigForProbe: %v", err)
			}
			if err := applyLogLevelFromConfig(cfg); err != nil {
				t.Fatalf("applyLogLevelFromConfig: %v", err)
			}
			if debugLogging() != tc.wantDebug {
				t.Fatalf("debugLogging() = %v, want %v", debugLogging(), tc.wantDebug)
			}
		})
	}

	debugLogEnabled.Store(false)
	cfg := minimalReloadConfig(nil)
	next := minimalReloadConfig(nil)
	next.LogLevel = "debug"
	changes, err := applyHotReloadConfig(cfg, next, NewAppState(), nil, nil)
	if err != nil {
		t.Fatalf("applyHotReloadConfig: %v", err)
	}
	if !debugLogging() || cfg.LogLevel != "debug" || len(changes) != 1 {
		t.Fatalf("SIGHUP to debug: debug=%v log_level=%q changes=%v", debugLogging(), cfg.LogLevel, changes)
	}
}

func TestHyperliquidCheckLogVolumeByLevel(t *testing.T) {
	resetFailureTrackers(t)
	longArg := "--regime-payload-json=" + strings.Repeat("x", 300)
	progress := "Fetching ETH 1h from Hyperliquid (paper)...\nFunding rate ETH: current=0.000010 avg7d=0.000012\n"
	cases := []struct {
		name       string
		debug      bool
		signal     int
		stderr     string
		err        error
		want       []string
		wantInLine string
	}{
		{name: "quiet hold prints nothing", signal: 0, stderr: progress, want: nil},
		{name: "debug hold restores the per-check detail", debug: true, signal: 0, stderr: progress,
			want: []string{"DEBUG Running", "DEBUG stderr", "cont Funding rate ETH: current=0.000010 avg7d=0.000012", "DEBUG Signal"}},
		{name: "quiet buy keeps the signal", signal: 1, stderr: progress, want: []string{"INFO Signal"}},
		{name: "quiet keeps a script warning and drops progress", signal: 0,
			stderr: progress + "[WARN] venue lot size unresolved for ETH: timeout\n",
			want:   []string{"INFO stderr"}, wantInLine: "stderr: [WARN] venue lot size unresolved for ETH: timeout"},
		{name: "quiet failure names the command with long args elided", signal: 0,
			stderr: progress + "Traceback (most recent call last):\nValueError: boom\n", err: errors.New("exit status 1"),
			want:       []string{"INFO Running", "ERROR Script failed", "ERROR stderr", "cont Funding rate ETH: current=0.000010 avg7d=0.000012", "cont Traceback (most recent call last):", "cont ValueError: boom"},
			wantInLine: "--regime-payload-json=<300 bytes>"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			withLogLevel(t, tc.debug)
			orig := runHyperliquidCheckFn
			runHyperliquidCheckFn = func(script string, args []string) (*HyperliquidResult, string, error) {
				if tc.err != nil {
					return nil, tc.stderr, tc.err
				}
				return &HyperliquidResult{Symbol: "ETH", Signal: tc.signal, Price: 2500, Mode: "paper"}, tc.stderr, nil
			}
			t.Cleanup(func() { runHyperliquidCheckFn = orig })

			sc := hlBatchStrategy("log-volume-"+strings.ReplaceAll(tc.name, " ", "-"), "sma_crossover", "ETH", "1h")
			sc.Args = append(sc.Args, longArg)
			var buf bytes.Buffer
			runHyperliquidCheck(&sc, map[string]float64{"ETH": 2500}, PositionCtx{}, nil, "simple", nil, &StrategyLogger{stratID: sc.ID, writer: &buf}, nil, nil)

			if got := logLineShapes(buf.String()); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("log lines = %q, want %q\n%s", got, tc.want, buf.String())
			}
			if tc.wantInLine != "" && !strings.Contains(buf.String(), tc.wantInLine) {
				t.Fatalf("log does not carry %q:\n%s", tc.wantInLine, buf.String())
			}
			if !tc.debug && strings.Contains(buf.String(), strings.Repeat("x", 300)) {
				t.Fatalf("quiet log carries the full multi-KB argument:\n%s", buf.String())
			}
		})
	}
}

func TestStrategyLoggerLevelsReachStdoutAndFileAlike(t *testing.T) {
	cases := []struct {
		debug bool
		want  []string
	}{
		{debug: false, want: []string{"INFO Status", "INFO Status", "INFO event", "WARN warn", "ERROR error"}},
		{debug: true, want: []string{"INFO Status", "DEBUG Status", "INFO Status", "DEBUG Status", "DEBUG detail", "INFO event", "WARN warn", "ERROR error"}},
	}
	for _, tc := range cases {
		withLogLevel(t, tc.debug)
		lm, err := NewLogManager(filepath.Join(t.TempDir(), "logs"))
		if err != nil {
			t.Fatal(err)
		}
		id := "log-levels-quiet"
		if tc.debug {
			id = "log-levels-debug"
		}
		sl, err := lm.GetStrategyLogger(id)
		if err != nil {
			t.Fatal(err)
		}
		for _, v := range []string{"flat", "flat", "long", "long"} {
			sl.InfoOnChange("status", v, "Status: positions=%s", v)
		}
		sl.Debug("detail: per-check")
		sl.Info("event: fill")
		sl.Warn("warn: drift")
		sl.Error("error: failed")
		sl.Close()

		data, err := os.ReadFile(filepath.Join(lm.logDir, id+".log"))
		if err != nil {
			t.Fatalf("read log file: %v", err)
		}
		if got := logLineShapes(string(data)); !reflect.DeepEqual(got, tc.want) {
			t.Fatalf("debug=%v file lines = %q, want %q", tc.debug, got, tc.want)
		}
	}
}

func TestCashflowJournalLinePrintsOnBasisChange(t *testing.T) {
	prevPending := cashflowJournalPendingStreaks
	cashflowJournalPendingStreaks = &cashflowJournalPendingTracker{}
	t.Cleanup(func() { cashflowJournalPendingStreaks = prevPending })

	hlKey := SharedWalletKey{Platform: "hyperliquid", Account: "0xlogvolume"}
	usable := &cashflowJournalReconcile{Key: hlKey, AccountValue: 1000, ExpectedEquity: 1000, Usable: true}
	incomplete := &cashflowJournalReconcile{Key: hlKey, AccountValue: 1000, ExpectedEquity: 990, Drift: 10, Incomplete: true}
	cases := []struct {
		debug bool
		want  []int
	}{
		{debug: false, want: []int{1, 0, 1, 0, 1}},
		{debug: true, want: []int{1, 1, 1, 1, 1}},
	}
	for _, tc := range cases {
		withLogLevel(t, tc.debug)
		logChanges.changed("cashflow-journal\x00"+sharedWalletKeyLabel(hlKey), "")
		var got []int
		for _, rec := range []*cashflowJournalReconcile{usable, usable, incomplete, incomplete, usable} {
			out := captureStdout(t, func() {
				applyCashflowJournalDriftBasis([]sharedWalletDriftResult{{Key: hlKey, Balance: 1000, MemberSum: 1000}}, hlKey, rec, true)
			})
			got = append(got, strings.Count(out, "[cashflow-journal]"))
		}
		if !reflect.DeepEqual(got, tc.want) {
			t.Fatalf("debug=%v journal lines per cycle = %v, want %v", tc.debug, got, tc.want)
		}
	}
}

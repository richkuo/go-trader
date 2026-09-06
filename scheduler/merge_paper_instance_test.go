package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func writeMergeFixtureDB(t *testing.T, path string, scope PortfolioScope, latched bool, pending bool) {
	t.Helper()
	db, err := OpenStateDB(path)
	if err != nil {
		t.Fatalf("OpenStateDB %s: %v", path, err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	state := NewAppState()
	state.Strategies["hl-x"] = &StrategyState{
		ID: "hl-x", Type: "perps", Platform: "hyperliquid",
		Cash: 950.5, InitialCapital: 1000,
		Positions: map[string]*Position{
			"ETH": {Symbol: "ETH", Quantity: 0.4, AvgCost: 2500, Side: "long", OwnerStrategyID: "hl-x", OpenedAt: now.Add(-6 * time.Hour)},
		},
		OptionPositions: map[string]*OptionPosition{},
		TradeHistory: []Trade{
			{Timestamp: now.Add(-6 * time.Hour), StrategyID: "hl-x", Symbol: "ETH", Side: "buy", Quantity: 0.4, Price: 2500, Value: 1000, TradeType: "perps"},
		},
	}
	state.PortfolioRisk[scope] = &PortfolioRiskState{PeakValue: 1000, KillSwitchActive: latched}
	if err := db.SaveState(state); err != nil {
		t.Fatalf("SaveState %s: %v", path, err)
	}
	if pending {
		if err := db.InsertPendingManualAction(PendingManualAction{StrategyID: "hl-x", Action: "update_sl", Symbol: "ETH", StopLossTriggerPx: 2400, CreatedAt: now}); err != nil {
			t.Fatalf("InsertPendingManualAction: %v", err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close %s: %v", path, err)
	}
}

func TestMergePaperInstance(t *testing.T) {
	bash := updateShellBash(t)
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	schedDir := filepath.Dir(thisFile)
	repoRoot := filepath.Join(schedDir, "..")
	tmp := t.TempDir()
	bin := filepath.Join(tmp, "go-trader")
	build := exec.Command("go", "build", "-o", bin, ".")
	build.Dir = schedDir
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}
	fixtures := filepath.Join(tmp, "fixtures")
	if err := os.MkdirAll(fixtures, 0o755); err != nil {
		t.Fatal(err)
	}
	writeMergeFixtureDB(t, filepath.Join(fixtures, "live.db"), ScopeLive, false, false)
	writeMergeFixtureDB(t, filepath.Join(fixtures, "paper.db"), ScopePaper, true, true)

	script := filepath.Join(repoRoot, "scripts", "test_merge_paper_instance.sh")
	cmd := exec.Command(bash, script)
	cmd.Dir = repoRoot
	cmd.Env = append(os.Environ(),
		"GO_TRADER_BIN="+bin,
		"MERGE_PAPER_FIXTURE_DIR="+fixtures,
		"MERGE_PAPER_UNIT_TEMPLATE="+filepath.Join(repoRoot, "systemd", "go-trader@.service"),
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("bash %s: %v\n%s", script, err, out)
	}
	if !strings.Contains(string(out), "OK: merge-paper-instance tests passed") {
		t.Fatalf("missing OK marker:\n%s", out)
	}
}

func TestUpdateCanonicalDBPathMatchesScheduler(t *testing.T) {
	bash := updateShellBash(t)
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	helpers := filepath.Join(filepath.Dir(thisFile), "..", "scripts", "update_helpers.sh")
	tmp := t.TempDir()
	real := filepath.Join(tmp, "real", "dir")
	if err := os.MkdirAll(real, 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(tmp, "link")
	if err := os.Symlink(filepath.Join(tmp, "real"), link); err != nil {
		t.Fatal(err)
	}
	absent := filepath.Join(link, "dir", "state.db")
	present := filepath.Join(link, "dir", "present.db")
	if err := os.WriteFile(filepath.Join(real, "present.db"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	dangling := filepath.Join(real, "dangling.db")
	if err := os.Symlink(filepath.Join(tmp, "missing-target"), dangling); err != nil {
		t.Fatal(err)
	}
	cases := []struct{ name, path string }{
		{"absent file behind a symlinked parent", absent},
		{"present file behind a symlinked parent", present},
		{"dangling symlink", dangling},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cmd := exec.Command(bash, "-c", `source "$1" && update_canonical_db_path "$2"`, "bash", helpers, tc.path)
			out, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("update_canonical_db_path: %v\n%s", err, out)
			}
			got := strings.TrimSpace(string(out))
			want := canonicalDBPath(tc.path)
			if got != want {
				t.Fatalf("shell helper=%q scheduler canonicalDBPath=%q; the handoff would lock a different file than the scheduler", got, want)
			}
		})
	}
}

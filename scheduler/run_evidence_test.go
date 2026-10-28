package main

import (
	"io"
	"os"
	"strings"
	"testing"
	"time"
)

func TestRunEvidenceHoldReplacesEvaluatedAndClearsAfterLatchReset(t *testing.T) {
	prev := globalRunEvidence
	globalRunEvidence = &runEvidence{startedAt: time.Now().UTC()}
	t.Cleanup(func() { globalRunEvidence = prev })

	sc := StrategyConfig{ID: "hl-latch-src", Type: "perps", Platform: "hyperliquid", Args: []string{"vwap", "ETH", "1h", "--mode=paper"}, PaperSource: "btc"}
	part := partitionFor(sc)
	t1 := time.Date(2026, 10, 27, 1, 0, 0, 0, time.UTC)
	t2 := t1.Add(time.Second)

	globalRunEvidence.markEvaluated(sc.ID, t1)
	globalRunEvidence.markHeld(sc.ID, runEvidenceHeldKillSwitch, t2)
	view := globalRunEvidence.healthView()
	held, _ := view["held"].(map[string]map[string]string)
	if held[sc.ID]["reason"] != runEvidenceHeldKillSwitch {
		t.Fatalf("hold = %#v", held)
	}
	evaluated, _ := view["evaluated"].(map[string]string)
	if _, ok := evaluated[sc.ID]; ok {
		t.Fatalf("evaluated still set: %#v", evaluated)
	}

	recordHeldStrategies(nil, []StrategyConfig{sc}, map[RiskPartition]*scopeCycleRisk{
		part: {Partition: part},
	}, nil, t2.Add(time.Second))
	view = globalRunEvidence.healthView()
	held, _ = view["held"].(map[string]map[string]string)
	if _, ok := held[sc.ID]; ok {
		t.Fatalf("hold remained after the latch cleared: %#v", held)
	}
}

func TestKillSwitchHoldDoesNotDelayTheNextCycle(t *testing.T) {
	prev := globalRunEvidence
	globalRunEvidence = &runEvidence{startedAt: time.Now().UTC()}
	t.Cleanup(func() { globalRunEvidence = prev })

	sc := StrategyConfig{ID: "hl-live", Type: "perps", Platform: "hyperliquid", Args: []string{"vwap", "ETH", "1h", "--mode=live"}, IntervalSeconds: 3600}
	part := partitionFor(sc)
	now := time.Date(2026, 10, 27, 2, 0, 0, 0, time.UTC)
	globalRunEvidence.markEvaluated(sc.ID, now.Add(-time.Second))
	recordHeldStrategies([]StrategyConfig{sc}, nil, map[RiskPartition]*scopeCycleRisk{
		part: {Partition: part, KillSwitchFired: true},
	}, nil, now)

	view := globalRunEvidence.healthView()
	held, _ := view["held"].(map[string]map[string]string)
	if held[sc.ID]["reason"] != runEvidenceHeldKillSwitch {
		t.Fatalf("hold = %#v", view["held"])
	}
	if ev, _ := view["evaluated"].(map[string]string); ev[sc.ID] != "" {
		t.Fatalf("evaluated = %#v", view["evaluated"])
	}
	cfg := &Config{Strategies: []StrategyConfig{sc}, IntervalSeconds: 3600}
	intervals := map[string]int{sc.ID: 3600}
	due, _, _ := computeDueSet(now.Add(time.Second), cfg, intervals, map[string]time.Time{}, nil, false, nil)
	if len(due) != 1 || due[0].ID != sc.ID {
		t.Fatalf("latched strategy was not due on the next tick: %+v", due)
	}
}

func TestMigrateFixtureEnvKeepsPaperKillSwitchLatched(t *testing.T) {
	t.Setenv("GO_TRADER_MIGRATE_FIXTURE_HOLD_LATCH", "1")
	part := paperSourcePartition("btc")
	prs := &PortfolioRiskState{KillSwitchActive: true, KillSwitchCloseApplied: true}
	st := &AppState{PortfolioRisk: map[RiskPartition]*PortfolioRiskState{part: prs}}
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	old := os.Stdout
	os.Stdout = w
	applyPaperKillSwitchCycle(st, &Config{}, nil, &scopeCycleRisk{
		Partition: part, KillSwitchFired: true, TotalPV: 100, PeakRebaselineAvailable: true,
	}, false)
	w.Close()
	os.Stdout = old
	logged, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	if !prs.KillSwitchActive {
		t.Fatal("fixture env cleared the paper kill switch")
	}
	if !strings.Contains(string(logged), "GO_TRADER_MIGRATE_FIXTURE_HOLD_LATCH=1") {
		t.Fatalf("log = %q", logged)
	}
}

func TestPaperKillSwitchAutoResetsWithoutFixtureEnv(t *testing.T) {
	part := paperSourcePartition("btc")
	prs := &PortfolioRiskState{KillSwitchActive: true, KillSwitchCloseApplied: true}
	st := &AppState{PortfolioRisk: map[RiskPartition]*PortfolioRiskState{part: prs}}
	applyPaperKillSwitchCycle(st, &Config{}, nil, &scopeCycleRisk{
		Partition: part, KillSwitchFired: true, TotalPV: 100, PeakRebaselineAvailable: true,
	}, false)
	if prs.KillSwitchActive {
		t.Fatal("paper kill switch stayed latched without the fixture env")
	}
}

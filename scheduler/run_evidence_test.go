package main

import (
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

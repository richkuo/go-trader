//go:build pyintegration

package main

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

func replayRebalanceStatus(out basketReplayOutput) map[int64]string {
	got := map[int64]string{}
	for _, st := range out.Steps {
		for _, rb := range st.Rebalances {
			got[rb.DeadlineMs] = rb.Status
		}
	}
	return got
}

func TestBasketReplayParityWithThePaperCoordinator(t *testing.T) {
	root := basketPyRoot(t)
	fx := loadReplayFixture(t)
	sc := replayScenario{
		RunID: "parity", FirstBar: 264, Steps: 12,
		Extra: map[int]basketReplayStep{
			0: {Venue: []basketReplayScriptedOutcome{{Outcome: "filled"}, {Outcome: "partial", FillFraction: 0.5}, {Outcome: "unknown", Detail: "no status"}}},
			1: {Queries: []basketReplayScriptedOutcome{{Outcome: "filled"}}},
			3: {SaveFailure: true},
			8: {Venue: []basketReplayScriptedOutcome{{Outcome: "rejected", Detail: "scripted reject"}}},
		},
		DropMarks: map[int][]string{6: {"ETH"}},
		DupSteps:  []int{4},
	}
	stateDir := filepath.Join(realTempDir(t), "run")
	spec := sc.spec(t, fx, stateDir, basketReplayModeNew)
	p := runReplayAdapter(t, root, spec)
	if p.Code != 0 || p.Out.Status == basketReplayStatusRefused || p.Out.Status == basketReplayStatusError {
		t.Fatalf("adapter exit %d status %s refusal %+v error %s\n%s", p.Code, p.Out.Status, p.Out.Refusal, p.Out.Error, tail(p.Stderr, 6000))
	}
	if len(p.Out.Steps) != len(spec.Steps) {
		t.Fatalf("the adapter applied %d of %d steps", len(p.Out.Steps), len(spec.Steps))
	}

	t.Chdir(root)
	ref := newReplayReference(t, sc.spec(t, fx, filepath.Join(realTempDir(t), "ref"), basketReplayModeNew), p.Out.Cost.Applied)
	for i := range ref.spec.Steps {
		ref.step(i)
	}
	requireReplayParity(t, "paper coordinator against the adapter", ref.records, p.Out.Steps, ref.trades(), p.Out.Trades)
	requireOneBookingPerFill(t, p.Out)

	first := p.Out.Steps[0]
	var partial, unknown bool
	for _, a := range first.Actions {
		if a.ConfirmedQty > 0 && a.ConfirmedQty < a.RequestedQty {
			partial = true
		}
		if a.Status == basketActionUnresolved {
			unknown = true
		}
	}
	if !partial || !unknown {
		t.Fatalf("step 0 must carry a partial fill and an unresolved unknown outcome: %+v", first.Actions)
	}
	resolved := false
	for _, a := range p.Out.Steps[1].Actions {
		if a.Status == basketActionCommitted && a.CreatedAtMs < a.UpdatedAtMs {
			resolved = true
		}
	}
	if !resolved {
		t.Fatalf("step 1 must reconcile the unknown order before any new submission: %+v", p.Out.Steps[1].Actions)
	}
	if len(p.Out.Steps[3].SaveErrors) == 0 {
		t.Fatal("the scripted save failure at step 3 did not fail the state save")
	}
	if p.Out.Steps[4].At != p.Out.Steps[5].At {
		t.Fatal("the duplicated step must replay the same simulated time")
	}
	for _, rb := range p.Out.Steps[5].Rebalances {
		if rb.Status == basketRebalanceRanking || rb.Status == basketRebalanceReserved {
			t.Fatalf("a duplicate step re-reserved a deadline: %+v", rb)
		}
	}
	exception := false
	for _, st := range p.Out.Steps {
		for _, rb := range st.Rebalances {
			if len(rb.Exceptions) > 0 && rb.IncreasesFrozen {
				exception = true
			}
		}
	}
	if !exception {
		t.Fatal("the recorded frames must drive a protective stop between deadlines that freezes increases")
	}
	if p.Out.Coverage.Complete || p.Out.Status != basketReplayStatusIncomplete {
		t.Fatalf("a dropped mark for a held coin must leave the result incomplete, got %s %+v", p.Out.Status, p.Out.Coverage)
	}
	for _, tr := range p.Out.Trades {
		ts, err := time.Parse(time.RFC3339Nano, tr.Timestamp)
		if err != nil {
			t.Fatal(err)
		}
		if ts.After(time.UnixMilli(p.Out.Run.CursorMs)) || ts.Year() != 2025 {
			t.Fatalf("trade %+v is not on the simulated clock", tr)
		}
		if tr.TradeType != TradeTypeFunding && tr.CostModelVersion != FillCostModelVersion {
			t.Fatalf("default parity must keep cost model version %d, got %+v", FillCostModelVersion, tr)
		}
	}
	if p.Out.LotFetches != 0 {
		t.Fatalf("the replay asked for venue metadata %d times", p.Out.LotFetches)
	}
}

func TestBasketReplayHoldingHorizonExpiryMatchesThePaperCoordinator(t *testing.T) {
	root := basketPyRoot(t)
	fx := loadReplayFixture(t)
	const horizon = int64(7200)
	sc := replayScenario{RunID: "expiry", FirstBar: 264, Steps: 6, Horizon: horizon}
	full := runReplayAdapter(t, root, sc.spec(t, fx, filepath.Join(realTempDir(t), "full"), basketReplayModeNew))
	if full.Code != 0 || full.Out.Status != basketReplayStatusComplete {
		t.Fatalf("exit %d status %s %+v %s", full.Code, full.Out.Status, full.Out.Coverage, tail(full.Stderr, 4000))
	}
	rebalanceMs := int64(14400_000)
	fired := 0
	for k, st := range full.Out.Steps {
		at, _ := time.Parse(time.RFC3339Nano, st.At)
		for _, a := range st.Actions {
			if a.Purpose != basketPurposeExpiry || a.Status != basketActionCommitted {
				continue
			}
			fired++
			if at.UnixMilli()%rebalanceMs == 0 {
				t.Fatalf("expiry %s fired at the rank deadline %s; the case needs a step between deadlines", a.ActionID, st.At)
			}
			var entry int64
			if k > 0 {
				for _, m := range full.Out.Steps[k-1].Members {
					if m.StrategyID == a.StrategyID {
						entry = m.LifecycleEntryMs
					}
				}
			}
			for _, m := range st.Members {
				if m.StrategyID == a.StrategyID && m.Qty != 0 {
					t.Fatalf("%s still holds %.10g after its holding-horizon close", m.StrategyID, m.Qty)
				}
			}
			if a.ExpiryMs != entry+horizon*1000 || a.ExpiryMs > at.UnixMilli() || at.UnixMilli()-a.ExpiryMs >= 3_600_000 {
				t.Fatalf("expiry %s at %s: expiry time %d, entry %d, horizon %ds; it must close at the first step at or after entry plus the horizon on the simulated clock", a.ActionID, st.At, a.ExpiryMs, entry, horizon)
			}
			booked := false
			for _, tr := range full.Out.Trades {
				ts, _ := time.Parse(time.RFC3339Nano, tr.Timestamp)
				if tr.StrategyID == a.StrategyID && tr.IsClose && ts.Equal(at) {
					booked = true
				}
			}
			if !booked {
				t.Fatalf("the holding-horizon close of %s booked no closing trade at the simulated step time %s", a.StrategyID, st.At)
			}
		}
	}
	if fired == 0 {
		t.Fatal("no holding-horizon close fired between rank deadlines")
	}

	t.Chdir(root)
	ref := newReplayReference(t, sc.spec(t, fx, filepath.Join(realTempDir(t), "ref"), basketReplayModeNew), full.Out.Cost.Applied)
	for i := range ref.spec.Steps {
		ref.step(i)
	}
	requireReplayParity(t, "holding-horizon paper coordinator against the adapter", ref.records, full.Out.Steps, ref.trades(), full.Out.Trades)

	dir := filepath.Join(realTempDir(t), "resume")
	a := runReplayAdapter(t, root, sc.spec(t, fx, dir, basketReplayModeNew), "--max-steps", "1")
	b := runReplayAdapter(t, root, sc.spec(t, fx, dir, basketReplayModeResume))
	if a.Code != 0 || b.Code != 0 || b.Out.Status != basketReplayStatusComplete {
		t.Fatalf("expiry stop and resume: exits %d %d status %s %s", a.Code, b.Code, b.Out.Status, tail(b.Stderr, 4000))
	}
	requireReplayParity(t, "holding-horizon stop and resume", full.Out.Steps, b.Out.Steps, full.Out.Trades, b.Out.Trades)
}

func TestBasketReplayRestartMatchesAnUninterruptedRun(t *testing.T) {
	root := basketPyRoot(t)
	fx := loadReplayFixture(t)
	sc := replayScenario{RunID: "restart", FirstBar: 264, Steps: 9, Extra: map[int]basketReplayStep{
		0: {Venue: []basketReplayScriptedOutcome{{Outcome: "filled"}, {Outcome: "unknown", Detail: "no status"}}},
		2: {Queries: []basketReplayScriptedOutcome{{Outcome: "partial", FillFraction: 0.5}}},
	}}
	full := runReplayAdapter(t, root, sc.spec(t, fx, filepath.Join(realTempDir(t), "full"), basketReplayModeNew))
	if full.Code != 0 || full.Out.Status != basketReplayStatusComplete {
		t.Fatalf("uninterrupted run: exit %d status %s %+v %s", full.Code, full.Out.Status, full.Out.Coverage, tail(full.Stderr, 4000))
	}

	stopped := filepath.Join(realTempDir(t), "stopped")
	part := runReplayAdapter(t, root, sc.spec(t, fx, stopped, basketReplayModeNew), "--max-steps", "4")
	if part.Code != 0 || part.Out.Status != basketReplayStatusStopped || part.Out.Run.StepsApplied != 4 {
		t.Fatalf("a bounded run must stop resumable after 4 steps: exit %d %+v", part.Code, part.Out.Run)
	}
	rest := runReplayAdapter(t, root, sc.spec(t, fx, stopped, basketReplayModeResume))
	if rest.Code != 0 || rest.Out.Status != basketReplayStatusComplete || rest.Out.Run.ResumedAt != 4 {
		t.Fatalf("resume: exit %d status %s %+v %s", rest.Code, rest.Out.Status, rest.Out.Run, tail(rest.Stderr, 4000))
	}
	requireReplayParity(t, "stop and resume", full.Out.Steps, rest.Out.Steps, full.Out.Trades, rest.Out.Trades)
	requireReplayParity(t, "stored steps of the stopped run", part.Out.Steps, rest.Out.Steps[:len(part.Out.Steps)], nil, nil)

	held := ""
	for _, m := range full.Out.Steps[2].Members {
		if m.Qty != 0 {
			held = m.Coin
			break
		}
	}
	if held == "" {
		t.Fatal("no member holds a coin at step 2 of the restart scenario")
	}
	gapped := sc
	gapped.RunID, gapped.DropMarks = "restart-gap", map[int][]string{2: {held}}
	gapDir := filepath.Join(realTempDir(t), "gapped")
	ga := runReplayAdapter(t, root, gapped.spec(t, fx, gapDir, basketReplayModeNew), "--max-steps", "3")
	gb := runReplayAdapter(t, root, gapped.spec(t, fx, gapDir, basketReplayModeResume))
	if ga.Code != 0 || ga.Out.Status != basketReplayStatusStopped || gb.Code != 0 || gb.Out.Run.ResumedAt != 3 || len(gb.Out.Steps) != sc.Steps {
		t.Fatalf("gapped stop and resume: exits %d %d, status %s, resumed at %d, %d steps", ga.Code, gb.Code, ga.Out.Status, gb.Out.Run.ResumedAt, len(gb.Out.Steps))
	}
	for _, st := range gb.Out.Steps[3:] {
		if len(st.Coverage) > 0 {
			t.Fatalf("the resumed steps must add no gap of their own, step %d has %v", st.Index, st.Coverage)
		}
	}
	gap := fmt.Sprintf("step 2: no frozen mark for %s while it is held or targeted", held)
	if gb.Out.Status != basketReplayStatusIncomplete || !containsString(gb.Out.Coverage.Gaps, gap) {
		t.Fatalf("a gap recorded before the stop must keep the resumed run incomplete: %s %+v", gb.Out.Status, gb.Out.Coverage)
	}

	midSave := sc
	midSave.RunID, midSave.Extra = "restart-midsave", map[int]basketReplayStep{}
	for i, ex := range sc.Extra {
		midSave.Extra[i] = ex
	}
	first := midSave.Extra[0]
	first.SaveFailure = true
	midSave.Extra[0] = first
	ms := runReplayAdapter(t, root, midSave.spec(t, fx, filepath.Join(realTempDir(t), "midsave"), basketReplayModeNew))
	if ms.Code != 0 || ms.Out.Status != basketReplayStatusComplete || len(ms.Out.Steps[0].SaveErrors) == 0 {
		t.Fatalf("a failed save followed by durable saves must stay complete: exit %d %s %+v save errors %v %s", ms.Code, ms.Out.Status, ms.Out.Coverage, ms.Out.Steps[0].SaveErrors, tail(ms.Stderr, 4000))
	}
	requireTradesMatchMemberBooks(t, ms.Out)

	crashed := filepath.Join(realTempDir(t), "crashed")
	crash := runReplayAdapter(t, root, sc.spec(t, fx, crashed, basketReplayModeNew), "--fault", "exit-after-submit:2")
	if crash.Code != basketReplayExitFault {
		t.Fatalf("the fault run must exit %d after the second submit, got %d", basketReplayExitFault, crash.Code)
	}
	again := runReplayAdapter(t, root, sc.spec(t, fx, crashed, basketReplayModeResume))
	if again.Code != 0 || again.Out.Status != basketReplayStatusComplete || again.Out.Run.ResumedAt != 0 {
		t.Fatalf("resume after a crash: exit %d %s %+v %s", again.Code, again.Out.Status, again.Out.Run, tail(again.Stderr, 4000))
	}
	pending := false
	for _, a := range again.Out.Steps[0].Actions {
		if a.Status == basketActionSubmitting && a.ConfirmedQty == 0 {
			pending = true
		}
	}
	if !pending {
		t.Fatalf("the resumed run must find the order sent before the crash still pending and send nothing new for it: %+v", again.Out.Steps[0].Actions)
	}
	requireReplayParity(t, "crash and resume after the order reconciles", full.Out.Steps[2:], again.Out.Steps[2:], full.Out.Trades, again.Out.Trades)
	requireOneBookingPerFill(t, again.Out)

	lost := filepath.Join(realTempDir(t), "lost-fill")
	crash = runReplayAdapter(t, root, sc.spec(t, fx, lost, basketReplayModeNew), "--fault", "exit-after-submit:1")
	if crash.Code != basketReplayExitFault {
		t.Fatalf("the fault run must exit %d after the first submit, got %d", basketReplayExitFault, crash.Code)
	}
	again = runReplayAdapter(t, root, sc.spec(t, fx, lost, basketReplayModeResume))
	if again.Code != 0 || again.Out.Status != basketReplayStatusComplete {
		t.Fatalf("resume after a lost fill response: exit %d %s %s", again.Code, again.Out.Status, tail(again.Stderr, 4000))
	}
	requireReplayParity(t, "lost fill response and resume", full.Out.Steps[1:], again.Out.Steps[1:], full.Out.Trades, again.Out.Trades)
	requireOneBookingPerFill(t, again.Out)
}

func TestBasketReplayRiskClockAcrossSimulatedMidnight(t *testing.T) {
	root := basketPyRoot(t)
	fx := loadReplayFixture(t)
	mcfg := defaultReplayMemberConfig()
	mcfg["stop_loss_pct"] = 0.5
	mcfg["cb_loss_streak_threshold"] = 1
	mcfg["cb_loss_streak_cooldown_minutes"] = 150
	mcfg["close_strategy"] = map[string]interface{}{"name": "tiered_tp_pct", "params": map[string]interface{}{"tp_tiers": []interface{}{map[string]interface{}{"profit_pct": 3.0, "close_fraction": 1.0}}}}
	sc := replayScenario{RunID: "risk", FirstBar: 282, Steps: 12, MemberConfig: mcfg, PortfolioRisk: &PortfolioRiskConfig{MaxDrawdownPct: 90, DailyMaxLossUSD: 1}}
	full := runReplayAdapter(t, root, sc.spec(t, fx, filepath.Join(realTempDir(t), "full"), basketReplayModeNew))
	if full.Code != 0 || full.Out.Status != basketReplayStatusComplete {
		t.Fatalf("exit %d status %s %+v %s", full.Code, full.Out.Status, full.Out.Coverage, tail(full.Stderr, 4000))
	}
	midnight := time.Date(2025, 9, 13, 0, 0, 0, 0, time.UTC)
	var heldBefore, freeAtMidnight, cbAcrossRollover, cbExpired bool
	var lossDate string
	for _, st := range full.Out.Steps {
		at, _ := time.Parse(time.RFC3339Nano, st.At)
		if at.Before(midnight) && st.Partition.DailyLossHeld {
			heldBefore = true
			for _, m := range st.Members {
				if m.DailyPnL < 0 {
					lossDate = m.DailyPnLDate
				}
			}
		}
		if at.Equal(midnight) && !st.Partition.DailyLossHeld {
			freeAtMidnight = true
			for _, m := range st.Members {
				if m.CircuitBreaker {
					until, _ := time.Parse(time.RFC3339Nano, m.CircuitBreakerUntil)
					if until.After(at) {
						cbAcrossRollover = true
					}
				}
			}
		}
		if at.After(midnight.Add(3 * time.Hour)) {
			cbExpired = true
			for _, m := range st.Members {
				if m.CircuitBreaker {
					cbExpired = false
				}
			}
		}
	}
	if !heldBefore || lossDate != "2025-09-12" {
		t.Fatalf("a historical loss must hold entries before simulated midnight under its simulated date (held=%v date=%q)", heldBefore, lossDate)
	}
	if !freeAtMidnight || !cbAcrossRollover || !cbExpired {
		t.Fatalf("rollover at simulated midnight %v, unexpired circuit breaker kept across it %v, expiry after its boundary %v", freeAtMidnight, cbAcrossRollover, cbExpired)
	}
	increaseHeld := false
	for _, st := range full.Out.Steps {
		at, _ := time.Parse(time.RFC3339Nano, st.At)
		if !at.Equal(midnight) {
			continue
		}
		for _, a := range st.Actions {
			for _, m := range st.Members {
				if m.StrategyID == a.StrategyID && m.CircuitBreaker && a.Direction == basketDirectionIncrease {
					t.Fatalf("member %s with an active circuit breaker received increase %s", a.StrategyID, a.ActionID)
				}
			}
		}
		increaseHeld = true
	}
	if !increaseHeld {
		t.Fatal("no step ran at simulated midnight")
	}
	stopStep, stopMember := -1, ""
	for k := 1; k < len(full.Out.Steps) && stopStep < 0; k++ {
		acted := map[string]bool{}
		for _, a := range full.Out.Steps[k].Actions {
			acted[a.StrategyID] = true
		}
		for j, m := range full.Out.Steps[k].Members {
			prev := full.Out.Steps[k-1].Members[j]
			if prev.StrategyID == m.StrategyID && prev.Qty != 0 && m.Qty == 0 && m.Trades > prev.Trades && !acted[m.StrategyID] {
				stopStep, stopMember = k, m.StrategyID
				break
			}
		}
	}
	if stopStep < 0 {
		t.Fatal("no paper stop closed a member between basket actions")
	}
	lastSave := sc
	lastSave.RunID, lastSave.Steps, lastSave.Extra = "risk-last-save", stopStep+1, map[int]basketReplayStep{stopStep: {SaveFailure: true}}
	ls := runReplayAdapter(t, root, lastSave.spec(t, fx, filepath.Join(realTempDir(t), "last-save"), basketReplayModeNew))
	if ls.Code != 0 || len(ls.Out.Steps) != stopStep+1 || len(ls.Out.Steps[stopStep].SaveErrors) == 0 {
		t.Fatalf("last-step save failure run: exit %d, %d steps %s", ls.Code, len(ls.Out.Steps), tail(ls.Stderr, 4000))
	}
	stopped := false
	for j, m := range ls.Out.Steps[stopStep].Members {
		prev := ls.Out.Steps[stopStep-1].Members[j]
		if m.StrategyID == stopMember && prev.Qty != 0 && m.Qty == 0 && m.Trades > prev.Trades {
			stopped = true
		}
	}
	if !stopped {
		t.Fatalf("the paper stop of %s must book on the last step %d", stopMember, stopStep)
	}
	lostSave := fmt.Sprintf("step %d: the state save failed and no later save made the step durable", stopStep)
	if ls.Out.Status == basketReplayStatusComplete || ls.Out.Coverage.Complete || !containsString(ls.Out.Coverage.Gaps, lostSave) {
		t.Fatalf("a failed last save must leave the run unscored: %s %+v", ls.Out.Status, ls.Out.Coverage)
	}
	for _, stop := range []string{"5", "7"} {
		dir := filepath.Join(realTempDir(t), "r"+stop)
		a := runReplayAdapter(t, root, sc.spec(t, fx, dir, basketReplayModeNew), "--max-steps", stop)
		b := runReplayAdapter(t, root, sc.spec(t, fx, dir, basketReplayModeResume))
		if a.Code != 0 || b.Code != 0 {
			t.Fatalf("resume at step %s: exits %d %d", stop, a.Code, b.Code)
		}
		requireReplayParity(t, "risk resume at step "+stop, full.Out.Steps, b.Out.Steps, full.Out.Trades, b.Out.Trades)
		requireReplayParity(t, "risk stored steps at step "+stop, a.Out.Steps, b.Out.Steps[:len(a.Out.Steps)], nil, nil)
	}
	t.Chdir(root)
	ref := newReplayReference(t, sc.spec(t, fx, filepath.Join(realTempDir(t), "ref"), basketReplayModeNew), full.Out.Cost.Applied)
	for i := range ref.spec.Steps {
		ref.step(i)
	}
	requireReplayParity(t, "risk paper coordinator against the adapter", ref.records, full.Out.Steps, ref.trades(), full.Out.Trades)
}

func TestBasketReplayKillSwitchLatchSurvivesRollover(t *testing.T) {
	root := basketPyRoot(t)
	fx := loadReplayFixture(t)
	sc := replayScenario{RunID: "latch", FirstBar: 284, Steps: 8, PortfolioRisk: &PortfolioRiskConfig{MaxDrawdownPct: 0.05}}
	out := runReplayAdapter(t, root, sc.spec(t, fx, filepath.Join(realTempDir(t), "run"), basketReplayModeNew))
	if out.Code != 0 {
		t.Fatalf("exit %d %+v %s", out.Code, out.Out.Refusal, tail(out.Stderr, 4000))
	}
	midnight := time.Date(2025, 9, 13, 0, 0, 0, 0, time.UTC)
	var latchedAt string
	for _, st := range out.Out.Steps {
		at, _ := time.Parse(time.RFC3339Nano, st.At)
		if st.Partition.KillSwitchActive && latchedAt == "" {
			latchedAt = st.Partition.KillSwitchAt
			if latchedAt != st.At {
				t.Fatalf("the latch time %s must be the simulated step time %s", latchedAt, st.At)
			}
		}
		if latchedAt != "" && !st.Partition.KillSwitchActive {
			t.Fatalf("the latched kill switch cleared at %s", st.At)
		}
		if latchedAt != "" {
			for _, m := range st.Members {
				if m.Qty != 0 {
					t.Fatalf("%s holds %.10g while the partition kill switch is latched", m.StrategyID, m.Qty)
				}
			}
			for _, a := range st.Actions {
				if a.Direction == basketDirectionIncrease && a.CreatedAtMs >= at.UnixMilli() {
					t.Fatalf("an increase was placed while latched: %+v", a)
				}
			}
		}
	}
	last, _ := time.Parse(time.RFC3339Nano, out.Out.Steps[len(out.Out.Steps)-1].At)
	if latchedAt == "" || !last.After(midnight) {
		t.Fatalf("the fixture must latch before and run past simulated midnight (latched %q, last %s)", latchedAt, last)
	}
	t.Chdir(root)
	ref := newReplayReference(t, sc.spec(t, fx, filepath.Join(realTempDir(t), "ref"), basketReplayModeNew), out.Out.Cost.Applied)
	for i := range ref.spec.Steps {
		ref.step(i)
	}
	requireReplayParity(t, "kill switch paper coordinator against the adapter", ref.records, out.Out.Steps, ref.trades(), out.Out.Trades)
}

func TestBasketReplayCostPolicyAppliesOncePerFill(t *testing.T) {
	root := basketPyRoot(t)
	fx := loadReplayFixture(t)
	custom := &basketReplayCostPolicy{TakerFeePct: 0.001, SlippagePct: 0.002, FillRole: fillCostRoleTakerAll, Spread: fillCostSpreadInSlippage}
	base := replayScenario{RunID: "cost", FirstBar: 264, Steps: 7}
	def := runReplayAdapter(t, root, base.spec(t, fx, filepath.Join(realTempDir(t), "default"), basketReplayModeNew))
	stress := base
	stress.CostPolicy = custom
	dir := filepath.Join(realTempDir(t), "stress")
	cs := runReplayAdapter(t, root, stress.spec(t, fx, dir, basketReplayModeNew))
	if def.Code != 0 || cs.Code != 0 {
		t.Fatalf("exits %d %d", def.Code, cs.Code)
	}
	if cs.Out.Cost.Applied.ModelVersion != FillCostModelReplayCustomVersion || cs.Out.Cost.Applied.PolicyID == "" || cs.Out.Cost.DefaultParity != defaultFillCostPolicy() {
		t.Fatalf("the cost record must name the applied replay policy and the default parity policy: %+v", cs.Out.Cost)
	}
	if def.Out.Cost.Applied != defaultFillCostPolicy() {
		t.Fatalf("an omitted cost policy must keep default paper costs: %+v", def.Out.Cost.Applied)
	}
	marks := map[string]map[string]float64{}
	for _, st := range cs.Out.Steps {
		marks[st.At] = map[string]float64{}
	}
	for _, s := range stress.spec(t, fx, dir, basketReplayModeNew).Steps {
		at, _ := time.Parse(time.RFC3339, s.At)
		marks[at.UTC().Format(time.RFC3339Nano)] = s.Marks
	}
	for _, run := range []struct {
		name string
		p    replayProcess
		fee  float64
		slip float64
		ver  int
	}{{"default", def, HyperliquidTakerFeePct, SlippagePct, FillCostModelVersion}, {"stress", cs, custom.TakerFeePct, custom.SlippagePct, FillCostModelReplayCustomVersion}} {
		fills := 0
		for _, tr := range run.p.Out.Trades {
			if tr.TradeType == TradeTypeFunding {
				continue
			}
			fills++
			if tr.CostModelVersion != run.ver || (run.ver == FillCostModelReplayCustomVersion) != (tr.CostPolicyID == run.p.Out.Cost.Applied.PolicyID && tr.CostPolicyID != "") {
				t.Fatalf("%s: trade %+v carries the wrong cost identity", run.name, tr)
			}
			if want := tr.Quantity * tr.Price * run.fee; replayAbs(tr.ExchangeFee-want) > 1e-9*max(1, want) {
				t.Fatalf("%s: fee %.10g, want one application %.10g on %+v", run.name, tr.ExchangeFee, want, tr)
			}
			if tr.IsClose && tr.RealizedPnL != 0 {
				continue
			}
			mark := marks[tr.Timestamp][coinOf(tr.StrategyID)]
			want := mark * (1 + run.slip)
			if tr.Side == "sell" {
				want = mark * (1 - run.slip)
			}
			if mark > 0 && replayAbs(tr.Price-want) > 1e-9*want {
				t.Fatalf("%s: open price %.10g, want one adverse slippage %.10g on mark %.10g", run.name, tr.Price, want, mark)
			}
		}
		if fills == 0 {
			t.Fatalf("%s: no fill was booked", run.name)
		}
	}

	for _, run := range []struct {
		name string
		p    replayProcess
		slip float64
	}{{"default", def, SlippagePct}, {"stress", cs, custom.SlippagePct}} {
		final := map[string]basketReplayActionRecord{}
		for _, st := range run.p.Out.Steps {
			for _, a := range st.Actions {
				final[a.ActionID] = a
			}
		}
		checked := 0
		for _, a := range final {
			if a.Status != basketActionCommitted {
				continue
			}
			want := a.MarkPx * (1 + run.slip)
			if a.OrderSide == "sell" {
				want = a.MarkPx * (1 - run.slip)
			}
			at := time.UnixMilli(a.UpdatedAtMs).UTC().Format(time.RFC3339Nano)
			for _, tr := range run.p.Out.Trades {
				if tr.StrategyID == a.StrategyID && tr.Timestamp == at && tr.Side == a.OrderSide && tr.IsClose == (a.Direction == basketDirectionReduce) {
					if replayAbs(tr.Price-want) > 1e-9*want {
						t.Fatalf("%s: %s %s booked at %.10g, want the action mark %.10g with one slippage (%.10g)", run.name, a.Direction, a.ActionID, tr.Price, a.MarkPx, want)
					}
					checked++
				}
			}
		}
		if checked == 0 {
			t.Fatalf("%s: no basket booking was checked", run.name)
		}
	}
	half := filepath.Join(realTempDir(t), "stress-resume")
	a := runReplayAdapter(t, root, stress.spec(t, fx, half, basketReplayModeNew), "--max-steps", "3")
	b := runReplayAdapter(t, root, stress.spec(t, fx, half, basketReplayModeResume))
	if a.Code != 0 || b.Code != 0 {
		t.Fatalf("stress resume exits %d %d", a.Code, b.Code)
	}
	requireReplayParity(t, "cost stress stop and resume", cs.Out.Steps, b.Out.Steps, cs.Out.Trades, b.Out.Trades)
	requireReplayParity(t, "cost stress stored steps", a.Out.Steps, b.Out.Steps[:len(a.Out.Steps)], nil, nil)

	rows, err := readReplayDB(t, dir)
	if err != nil {
		t.Fatal(err)
	}
	if rows.policies != 1 || rows.policyVersion != FillCostModelReplayCustomVersion || rows.customTrades == 0 || rows.defaultTrades != 0 {
		t.Fatalf("the replay state file must hold one policy evidence row and only custom-stamped fills: %+v", rows)
	}
	resume := runReplayAdapter(t, root, stress.spec(t, fx, dir, basketReplayModeResume))
	if resume.Code != 0 || len(resume.Out.Trades) != len(cs.Out.Trades) {
		t.Fatalf("a resume of a finished run must book nothing new: exit %d, %d trades against %d", resume.Code, len(resume.Out.Trades), len(cs.Out.Trades))
	}
	changed := stress
	changed.CostPolicy = &basketReplayCostPolicy{TakerFeePct: 0.002, SlippagePct: 0.002, FillRole: fillCostRoleTakerAll, Spread: fillCostSpreadInSlippage}
	refused := runReplayAdapter(t, root, changed.spec(t, fx, dir, basketReplayModeResume))
	if refused.Code != basketReplayExitRefused || refused.Out.Refusal == nil || refused.Out.Refusal.Reason != "resume_refused" {
		t.Fatalf("a changed cost policy must refuse resume: exit %d %+v", refused.Code, refused.Out.Refusal)
	}

	tight := base
	tight.RunID = "cost-margin"
	mcfg := defaultReplayMemberConfig()
	mcfg["capital"] = 1202
	tight.MemberConfig = mcfg
	tight.Steps = 1
	tight.Weights = map[string]float64{"BTC": 0.4, "ETH": -0.2, "SOL": -0.2}
	tight.Net = 1500
	marginDef := runReplayAdapter(t, root, tight.spec(t, fx, filepath.Join(realTempDir(t), "md"), basketReplayModeNew))
	tight.CostPolicy = custom
	marginCS := runReplayAdapter(t, root, tight.spec(t, fx, filepath.Join(realTempDir(t), "mc"), basketReplayModeNew))
	btcDef, btcCS := memberQty(marginDef.Out, "rp-btc"), memberQty(marginCS.Out, "rp-btc")
	if btcDef <= 0 || btcCS != 0 {
		t.Fatalf("the prospective margin gate must admit the default-cost BTC tranche and hold the stressed one: default %.10g stressed %.10g", btcDef, btcCS)
	}
}

func coinOf(id string) string {
	switch id {
	case "rp-btc":
		return "BTC"
	case "rp-eth":
		return "ETH"
	}
	return "SOL"
}

func replayAbs(v float64) float64 {
	if v < 0 {
		return -v
	}
	return v
}

func containsString(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}

func requireTradesMatchMemberBooks(t *testing.T, out basketReplayOutput) {
	t.Helper()
	stored := map[string]int{}
	for _, tr := range out.Trades {
		stored[tr.StrategyID]++
	}
	last := out.Steps[len(out.Steps)-1]
	for _, m := range last.Members {
		if m.Trades != stored[m.StrategyID] {
			t.Fatalf("%s books %d trades but the state file holds %d", m.StrategyID, m.Trades, stored[m.StrategyID])
		}
	}
}

func memberQty(out basketReplayOutput, id string) float64 {
	if len(out.Steps) == 0 {
		return 0
	}
	for _, m := range out.Steps[len(out.Steps)-1].Members {
		if m.StrategyID == id {
			return m.Qty
		}
	}
	return 0
}

type replayDBRows struct {
	policies, policyVersion, customTrades, defaultTrades int
}

func readReplayDB(t *testing.T, dir string) (replayDBRows, error) {
	t.Helper()
	var r replayDBRows
	db, err := openStateDBReadOnly(filepath.Join(dir, basketReplayDBName))
	if err != nil {
		return r, err
	}
	defer db.Close()
	if err := db.db.QueryRow(`SELECT COUNT(*), COALESCE(MAX(model_version), 0) FROM fill_cost_policies`).Scan(&r.policies, &r.policyVersion); err != nil {
		return r, err
	}
	if err := db.db.QueryRow(`SELECT COUNT(*) FROM trades WHERE trade_type != 'funding' AND cost_model_version = ? AND cost_policy_id != ''`, FillCostModelReplayCustomVersion).Scan(&r.customTrades); err != nil {
		return r, err
	}
	if err := db.db.QueryRow(`SELECT COUNT(*) FROM trades WHERE cost_model_version = ?`, FillCostModelVersion).Scan(&r.defaultTrades); err != nil {
		return r, err
	}
	return r, nil
}

func TestBasketReplayBaselineTradesLongOnlyThroughTheCoordinator(t *testing.T) {
	root := basketPyRoot(t)
	fx := loadReplayFixture(t)
	d0 := time.UnixMilli(replayFixtureBaseMs + 264*3_600_000).UTC().Format(time.RFC3339)
	d1 := time.UnixMilli(replayFixtureBaseMs + 268*3_600_000).UTC().Format(time.RFC3339)
	sc := replayScenario{RunID: "baseline", FirstBar: 264, Steps: 6, Net: 2100,
		TargetInput: basketReplayTargetInput{Mode: basketReplayTargetsBaseline, Weights: map[string]map[string]float64{
			d0: {"BTC": 0.4, "ETH": 0.3},
			d1: {"BTC": 0.2, "ETH": 0.2},
		}}}
	out := runReplayAdapter(t, root, sc.spec(t, fx, filepath.Join(realTempDir(t), "run"), basketReplayModeNew))
	if out.Code != 0 || out.Out.Status == basketReplayStatusRefused {
		t.Fatalf("exit %d %+v %s", out.Code, out.Out.Refusal, tail(out.Stderr, 4000))
	}
	if q := memberQty(out.Out, "rp-btc"); q <= 0 {
		t.Fatalf("the long-only baseline must hold BTC through the coordinator, got %.10g", q)
	}
	if q := memberQty(out.Out, "rp-sol"); q != 0 {
		t.Fatalf("the baseline weights SOL at zero, it holds %.10g", q)
	}
	reduced := false
	for _, st := range out.Out.Steps {
		for _, a := range st.Actions {
			if a.Direction == basketDirectionReduce && a.Status == basketActionCommitted {
				reduced = true
			}
		}
	}
	if !reduced {
		t.Fatal("the second baseline deadline must reduce through the transition owner")
	}
	requireOneBookingPerFill(t, out.Out)

	ranked := sc
	ranked.RunID = "baseline-ranked"
	ranked.TargetInput = basketReplayTargetInput{Mode: basketReplayTargetsRanker}
	ranked.Weights = map[string]float64{"BTC": 0.4, "ETH": 0.3}
	r := runReplayAdapter(t, root, ranked.spec(t, fx, filepath.Join(realTempDir(t), "ranked"), basketReplayModeNew))
	held := false
	for _, st := range r.Out.Steps {
		for _, rb := range st.Rebalances {
			if rb.Status == basketRebalanceHeld && rb.HoldReason == "the short side is empty" {
				held = true
			}
		}
	}
	if !held || memberQty(r.Out, "rp-btc") != 0 {
		t.Fatalf("production ranker admission must still hold a long-only target set: %+v", replayRebalanceStatus(r.Out))
	}

	bad := sc
	bad.RunID = "baseline-bad"
	bad.TargetInput.Weights = map[string]map[string]float64{d0: {"BTC": 0.9, "ETH": 0.3}}
	b := runReplayAdapter(t, root, bad.spec(t, fx, filepath.Join(realTempDir(t), "bad"), basketReplayModeNew))
	if memberQty(b.Out, "rp-btc") != 0 || memberQty(b.Out, "rp-eth") != 0 {
		t.Fatalf("baseline weights above gross and the per-coin cap must freeze no target")
	}
}

func TestBasketReplayRefusesBeforeOpeningState(t *testing.T) {
	root := basketPyRoot(t)
	fx := loadReplayFixture(t)
	sc := replayScenario{RunID: "refuse", FirstBar: 264, Steps: 2}
	existing := filepath.Join(realTempDir(t), "existing")
	if err := os.Mkdir(existing, 0o700); err != nil {
		t.Fatal(err)
	}
	foreignDB := filepath.Join(existing, basketReplayDBName)
	if err := os.WriteFile(foreignDB, []byte("not a replay database"), 0o600); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(foreignDB)
	linkParent := filepath.Join(realTempDir(t), "link")
	if err := os.Symlink(realTempDir(t), linkParent); err != nil {
		t.Fatal(err)
	}
	finished := filepath.Join(realTempDir(t), "finished")
	ok := runReplayAdapter(t, root, sc.spec(t, fx, finished, basketReplayModeNew))
	if ok.Code != 0 {
		t.Fatalf("setup run: exit %d %s", ok.Code, tail(ok.Stderr, 3000))
	}
	finishedDB, _ := os.ReadFile(filepath.Join(finished, basketReplayDBName))
	changedInput := sc
	changedInput.Steps = 3
	backward := sc.spec(t, fx, filepath.Join(realTempDir(t), "backward"), basketReplayModeNew)
	backward.Steps[1].At = "2025-09-11T00:00:00Z"
	unknownVersion := sc.spec(t, fx, filepath.Join(realTempDir(t), "version"), basketReplayModeNew)
	unknownVersion.Version = 99
	liveField := sc.spec(t, fx, filepath.Join(realTempDir(t), "live"), basketReplayModeNew)
	liveField.Members[0].Config["args"] = []string{"basket_member", "BTC", "1h", "--mode=live"}
	routeField := sc.spec(t, fx, filepath.Join(realTempDir(t), "route"), basketReplayModeNew)
	routeField.Members[1].Config["paper_source"] = "elsewhere"
	deadline := time.UnixMilli(replayFixtureBaseMs + 264*3_600_000).UTC()
	offGrid := sc.spec(t, fx, filepath.Join(realTempDir(t), "off-grid"), basketReplayModeNew)
	offGrid.TargetInput = basketReplayTargetInput{Mode: basketReplayTargetsBaseline, Weights: map[string]map[string]float64{
		deadline.Add(30 * time.Minute).Format(time.RFC3339): {"BTC": 0.2},
	}}
	dupKeys := sc.spec(t, fx, filepath.Join(realTempDir(t), "dup-keys"), basketReplayModeNew)
	dupKeys.TargetInput = basketReplayTargetInput{Mode: basketReplayTargetsBaseline, Weights: map[string]map[string]float64{
		deadline.Format(time.RFC3339):                     {"BTC": 0.2},
		deadline.Format("2006-01-02T15:04:05") + "+00:00": {"BTC": 0.3},
	}}
	cases := []struct {
		name   string
		spec   *basketReplaySpec
		reason string
	}{
		{"new run on an existing directory", sc.spec(t, fx, existing, basketReplayModeNew), "state_exists"},
		{"resume of foreign state", sc.spec(t, fx, existing, basketReplayModeResume), "state_foreign"},
		{"resume with a changed input", changedInput.spec(t, fx, finished, basketReplayModeResume), "resume_refused"},
		{"symbolic link parent", sc.spec(t, fx, filepath.Join(linkParent, "run"), basketReplayModeNew), "state_dir_refused"},
		{"deployed path", sc.spec(t, fx, "/var/lib/go-trader/replay", basketReplayModeNew), "state_dir_refused"},
		{"step before the previous step", backward, "spec_invalid"},
		{"unknown protocol version", unknownVersion, "protocol_unsupported"},
		{"member args override", liveField, "spec_invalid"},
		{"member storage route", routeField, "spec_invalid"},
		{"baseline weights off the rebalance deadlines", offGrid, "spec_invalid"},
		{"baseline weights for one deadline under two keys", dupKeys, "spec_invalid"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := runReplayAdapter(t, root, tc.spec)
			if p.Code != basketReplayExitRefused || p.Out.Status != basketReplayStatusRefused || p.Out.Refusal == nil || p.Out.Refusal.Reason != tc.reason {
				t.Fatalf("want refusal %s, got exit %d %+v", tc.reason, p.Code, p.Out.Refusal)
			}
			if tc.spec.Mode == basketReplayModeNew && tc.spec.Run.StateDir != existing {
				if _, err := os.Lstat(tc.spec.Run.StateDir); err == nil {
					t.Fatalf("a refused new run created %s", tc.spec.Run.StateDir)
				}
			}
		})
	}
	after, _ := os.ReadFile(foreignDB)
	if string(after) != string(before) {
		t.Fatal("a refusal changed the foreign state file")
	}
	finishedAfter, _ := os.ReadFile(filepath.Join(finished, basketReplayDBName))
	if string(finishedAfter) != string(finishedDB) {
		t.Fatal("a refused resume changed the run's state file")
	}
	malformed := filepath.Join(realTempDir(t), "malformed.json")
	os.WriteFile(malformed, []byte(`{"protocol": "go-trader.basket-replay", "version": 1, "unknown_field": true}`), 0o600)
	cmd := runReplayRaw(t, root, malformed)
	var out basketReplayOutput
	if err := json.Unmarshal(cmd, &out); err != nil || out.Refusal == nil || out.Refusal.Reason != "spec_malformed" {
		t.Fatalf("an unknown field must refuse as spec_malformed: %s", cmd)
	}

	short := replayScenario{RunID: "short", FirstBar: 120, Steps: 2}
	sp := runReplayAdapter(t, root, short.spec(t, fx, filepath.Join(realTempDir(t), "short"), basketReplayModeNew))
	if sp.Code != 0 || sp.Out.Status != basketReplayStatusIncomplete || sp.Out.Coverage.Complete {
		t.Fatalf("member history shorter than the check lookback must leave the run incomplete: exit %d %s %+v", sp.Code, sp.Out.Status, sp.Out.Coverage)
	}
	for _, st := range sp.Out.Steps {
		for _, a := range st.Actions {
			if a.Direction == basketDirectionIncrease {
				t.Fatalf("an increase ran on short member history: %+v", a)
			}
		}
	}

	lots := replayScenario{RunID: "lots", FirstBar: 264, Steps: 2, Lots: map[string]int{"BTC": 5, "ETH": 4}}
	lp := runReplayAdapter(t, root, lots.spec(t, fx, filepath.Join(realTempDir(t), "lots"), basketReplayModeNew))
	if lp.Code != 0 || lp.Out.LotFetches == 0 || lp.Out.Coverage.Complete {
		t.Fatalf("a coin absent from the frozen lot metadata must refuse the fetch and leave the run incomplete: exit %d fetches %d %+v", lp.Code, lp.Out.LotFetches, lp.Out.Coverage)
	}
	lotDir := filepath.Join(realTempDir(t), "lots-resume")
	la := runReplayAdapter(t, root, lots.spec(t, fx, lotDir, basketReplayModeNew), "--max-steps", "1")
	if la.Code != 0 || la.Out.LotFetches == 0 {
		t.Fatalf("the refused lot lookup must happen before the stop: exit %d fetches %d", la.Code, la.Out.LotFetches)
	}
	lb := runReplayAdapter(t, root, lots.spec(t, fx, lotDir, basketReplayModeResume))
	if lb.Code != 0 || lb.Out.LotFetches != lp.Out.LotFetches || lb.Out.Status == basketReplayStatusComplete || lb.Out.Coverage.Complete {
		t.Fatalf("a resume must keep the lot refusals counted before the stop: exit %d fetches %d (uninterrupted %d) %s %+v", lb.Code, lb.Out.LotFetches, lp.Out.LotFetches, lb.Out.Status, lb.Out.Coverage)
	}
	for _, st := range lp.Out.Steps {
		for _, a := range st.Actions {
			if a.Symbol == "SOL" {
				t.Fatalf("SOL traded without frozen lot metadata: %+v", a)
			}
		}
	}
}

func runReplayRaw(t *testing.T, root, specPath string) []byte {
	t.Helper()
	bin := replayBinary(t)
	cmd := exec.Command(bin, "basket-replay", "--spec", specPath)
	cmd.Dir = root
	out, _ := cmd.Output()
	return out
}

func realTempDir(t *testing.T) string {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return dir
}

func writeReplayCSV(t *testing.T, path, header string, rows [][]float64, format func([]float64) string) string {
	t.Helper()
	var b bytes.Buffer
	gz := gzip.NewWriter(&b)
	fmt.Fprintln(gz, header)
	for _, r := range rows {
		fmt.Fprintln(gz, format(r))
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, b.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(b.Bytes())
	return hex.EncodeToString(sum[:])
}

func TestBasketReplayDriverRecordsScoredAndRefusedRuns(t *testing.T) {
	root := basketPyRoot(t)
	fx := loadReplayFixture(t)
	bin := replayBinary(t)
	work := realTempDir(t)
	frames := map[string]interface{}{}
	funding := map[string]interface{}{}
	for _, coin := range []string{"BTC", "ETH", "SOL"} {
		cp := filepath.Join(work, coin+"_1h_candles.csv.gz")
		sha := writeReplayCSV(t, cp, "timestamp,open,high,low,close,volume", fx.Candles[coin+"|1h"], func(r []float64) string {
			return fmt.Sprintf("%d,%s,%s,%s,%s,%s", int64(r[0]), fmtFloat(r[1]), fmtFloat(r[2]), fmtFloat(r[3]), fmtFloat(r[4]), fmtFloat(r[5]))
		})
		frames[coin+"|1h"] = map[string]string{"path": filepath.Base(cp), "sha256": sha}
		fp := filepath.Join(work, coin+"_funding.csv.gz")
		fsha := writeReplayCSV(t, fp, "timestamp,rate", fx.Funding[coin], func(r []float64) string {
			return fmt.Sprintf("%d,%s", int64(r[0]), fmtFloat(r[1]))
		})
		funding[coin] = map[string]string{"path": filepath.Base(fp), "sha256": fsha, "from": "2025-09-01T00:00:00Z"}
	}
	base := replayScenario{RunID: "driver", FirstBar: 264, Steps: 1}.spec(t, fx, filepath.Join(work, "state"), basketReplayModeNew)
	plan := map[string]interface{}{
		"plan": "go-trader.basket-replay-plan", "version": 1, "run_id": "driver", "state_dir": filepath.Join(work, "state"),
		"source": map[string]string{"frames": "test-written csv from scheduler/testdata/basket_replay_recorded_frames.json"},
		"frames": frames, "funding": funding,
		"window":       map[string]interface{}{"start": "2025-09-12T00:00:00Z", "end": "2025-09-12T06:00:00Z", "step_seconds": 3600},
		"history_bars": 264, "basket": base.Basket, "members": base.Members, "target_input": base.TargetInput, "lot_metadata": base.LotMetadata,
		"venue_script": map[string]interface{}{"2025-09-12T00:00:00Z": map[string]interface{}{"venue": []map[string]interface{}{{"outcome": "partial", "fill_fraction": 0.5}}}},
	}
	write := func(name string, v interface{}) string {
		p := filepath.Join(work, name)
		blob, _ := json.Marshal(v)
		if err := os.WriteFile(p, blob, 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	run := func(planPath, out string, extra ...string) (int, map[string]interface{}) {
		cmd := exec.Command("uv", append([]string{"run", "--no-sync", "python", "backtest/basket_replay.py", "--plan", planPath, "--binary", bin, "--out", out}, extra...)...)
		cmd.Dir = root
		combined, err := cmd.CombinedOutput()
		code := 0
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			code = exitErr.ExitCode()
		} else if err != nil {
			t.Fatalf("driver did not run: %v %s", err, combined)
		}
		blob, err := os.ReadFile(out)
		if err != nil {
			t.Fatalf("the driver wrote no result record (exit %d): %s", code, combined)
		}
		var rec map[string]interface{}
		if err := json.Unmarshal(blob, &rec); err != nil {
			t.Fatal(err)
		}
		return code, rec
	}
	code, rec := run(write("plan.json", plan), filepath.Join(work, "scored.json"))
	if code != 0 || rec["scored"] != true || rec["promotion_eligible"] != true || rec["status"] != basketReplayStatusComplete {
		t.Fatalf("a complete replay must be scored: exit %d %v %v", code, rec["status"], rec["refusal"])
	}
	if curve, _ := rec["equity_curve"].([]interface{}); len(curve) != 6 {
		t.Fatalf("the scored record must carry one equity point per step, got %d", len(curve))
	}
	partialAtStart := false
	if out, ok := rec["adapter_output"].(map[string]interface{}); ok {
		if steps, ok := out["steps"].([]interface{}); ok && len(steps) > 0 {
			first, _ := steps[0].(map[string]interface{})
			actions, _ := first["actions"].([]interface{})
			for _, a := range actions {
				am, _ := a.(map[string]interface{})
				req, _ := am["requested_qty"].(float64)
				conf, _ := am["confirmed_qty"].(float64)
				if conf > 0 && conf < req {
					partialAtStart = true
				}
			}
		}
	}
	if !partialAtStart {
		t.Fatal("the venue_script entry at window.start must apply its partial fill to step 0")
	}
	kept, _ := os.ReadFile(filepath.Join(work, "scored.json"))
	if code, _ := run(write("plan.json", plan), filepath.Join(work, "scored.json")); code != 2 {
		t.Fatalf("an existing result record must refuse the run, exit %d", code)
	}
	if after, _ := os.ReadFile(filepath.Join(work, "scored.json")); !bytes.Equal(after, kept) {
		t.Fatal("the driver replaced an existing result record")
	}
	code, again := run(write("plan.json", plan), filepath.Join(work, "resumed.json"), "--resume")
	firstCurve, _ := json.Marshal(rec["equity_curve"])
	againCurve, _ := json.Marshal(again["equity_curve"])
	if code != 0 || again["scored"] != true || !bytes.Equal(firstCurve, againCurve) {
		t.Fatalf("a resume of a finished run must report every applied step: exit %d scored %v curve %s, first run %s", code, again["scored"], againCurve, firstCurve)
	}

	late := map[string]interface{}{}
	for k, v := range plan {
		late[k] = v
	}
	late["run_id"], late["state_dir"] = "driver-late", filepath.Join(work, "late-state")
	lateFunding := map[string]interface{}{}
	for coin, f := range funding {
		ff := map[string]string{}
		for k, v := range f.(map[string]string) {
			ff[k] = v
		}
		ff["from"] = "2025-09-12T03:00:00Z"
		lateFunding[coin] = ff
	}
	late["funding"] = lateFunding
	code, rec = run(write("late.json", late), filepath.Join(work, "late.json.out"))
	if code == 0 || rec["scored"] != false || rec["promotion_eligible"] != false || rec["status"] != basketReplayStatusIncomplete {
		t.Fatalf("funding coverage that starts after the exposure must leave the replay unscored: exit %d %v", code, rec["status"])
	}

	tampered := map[string]interface{}{}
	for k, v := range plan {
		tampered[k] = v
	}
	tampered["state_dir"] = filepath.Join(work, "tampered-state")
	tf := map[string]interface{}{}
	for k, v := range frames {
		tf[k] = v
	}
	tf["BTC|1h"] = map[string]string{"path": "BTC_1h_candles.csv.gz", "sha256": stringsRepeat("0", 64)}
	tampered["frames"] = tf
	code, rec = run(write("tampered.json", tampered), filepath.Join(work, "tampered.out"))
	if code != 3 || rec["scored"] != false || rec["status"] != "refused" {
		t.Fatalf("a frozen file with a changed hash must refuse before the adapter runs: exit %d %v", code, rec["status"])
	}
	if _, err := os.Lstat(filepath.Join(work, "tampered-state")); err == nil {
		t.Fatal("a refused plan created replay state")
	}
	for _, tc := range []struct{ name, at string }{{"between-steps", "2025-09-12T00:30:00Z"}, {"window-end", "2025-09-12T06:00:00Z"}} {
		off := map[string]interface{}{}
		for k, v := range plan {
			off[k] = v
		}
		stateDir := filepath.Join(work, "script-"+tc.name)
		off["state_dir"] = stateDir
		off["venue_script"] = map[string]interface{}{tc.at: map[string]interface{}{"save_failure": true}}
		code, rec = run(write("script-"+tc.name+".json", off), filepath.Join(work, "script-"+tc.name+".out"))
		refusal, _ := rec["refusal"].(map[string]interface{})
		if code != 3 || rec["scored"] != false || rec["status"] != "refused" || refusal["reason"] != "plan_refused" {
			t.Fatalf("a venue_script key %s that matches no step must refuse the plan: exit %d %v %v", tc.at, code, rec["status"], rec["refusal"])
		}
		if _, err := os.Lstat(stateDir); err == nil {
			t.Fatalf("a refused plan with venue_script key %s created replay state", tc.at)
		}
	}
	dupPath := filepath.Join(work, "dup.json")
	planBlob, _ := json.Marshal(plan)
	dupBlob := append([]byte(`{"run_id": "driver-dup", `), planBlob[1:]...)
	if err := os.WriteFile(dupPath, dupBlob, 0o600); err != nil {
		t.Fatal(err)
	}
	code, rec = run(dupPath, filepath.Join(work, "dup.out"))
	if refusal, _ := rec["refusal"].(map[string]interface{}); code != 3 || rec["status"] != "refused" || refusal["reason"] != "plan_refused" {
		t.Fatalf("a plan that names one key twice must refuse: exit %d %v %v", code, rec["status"], rec["refusal"])
	}
}

func fmtFloat(v float64) string {
	return strconv.FormatFloat(v, 'g', -1, 64)
}

func stringsRepeat(s string, n int) string {
	out := ""
	for i := 0; i < n; i++ {
		out += s
	}
	return out
}

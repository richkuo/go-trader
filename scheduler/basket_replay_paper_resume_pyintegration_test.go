//go:build pyintegration

package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
)

type pairPaperFundingRow struct {
	StrategyID string
	Status     string
	EventMs    int64
	EventJSON  string
}

func pairPaperFundingRows(t *testing.T, stateDir string) []pairPaperFundingRow {
	t.Helper()
	db, err := openStateDBReadOnly(filepath.Join(stateDir, basketReplayDBName))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ok, err := tableExists(db, "pair_funding_events")
	if err != nil || !ok {
		return nil
	}
	rows, err := db.db.Query(`SELECT strategy_id, status, event_ms, event_json FROM pair_funding_events ORDER BY strategy_id, event_ms`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []pairPaperFundingRow
	for rows.Next() {
		var r pairPaperFundingRow
		if err := rows.Scan(&r.StrategyID, &r.Status, &r.EventMs, &r.EventJSON); err != nil {
			t.Fatal(err)
		}
		out = append(out, r)
	}
	return out
}

func pairPaperApplied(rows []pairPaperFundingRow, memberID string, eventMs int64) *pairPaperFundingRow {
	for i := range rows {
		if rows[i].StrategyID == memberID && rows[i].EventMs == eventMs && rows[i].Status == pairFundingStatusApplied {
			return &rows[i]
		}
	}
	return nil
}

func pairPaperRequireResumed(t *testing.T, label string, p pairPaperProc) {
	t.Helper()
	if p.Code == basketReplayExitRefused || p.Code == basketReplayExitFault || p.Out.Run == nil || !p.Out.Run.Resumed || p.Out.Refusal != nil {
		t.Fatalf("%s: the resume must admit and run, got exit %d status %s refusal %+v error %q run %+v\nstderr tail=%s", label, p.Code, p.Out.Status, p.Out.Refusal, p.Out.Error, p.Out.Run, tail(p.Stderr, 6000))
	}
	if p.Out.Comparison != nil {
		for _, r := range p.Out.Comparison.Reasons {
			if strings.Contains(r, "diverged") {
				t.Fatalf("%s: the resumed run must consume its committed stream without a divergence, got %v", label, p.Out.Comparison.Reasons)
			}
		}
	}
}

func pairPaperResumeEpochSeqs(rows []pairRecRow) []int64 {
	var out []int64
	for _, r := range rows {
		if r.Kind == pairRecEventEpoch && r.Event.Epoch != nil && r.Event.Epoch.Mode == "resume" {
			out = append(out, r.Seq)
		}
	}
	return out
}

var pairPaperOncePerStepKinds = map[string]bool{pairRecKindFunding: true, pairRecKindMatchPoll: true, pairRecKindCapability: true, pairRecKindCandles: true, pairRecKindCollect: true, pairRecKindPolicy: true}

func pairPaperNoRecommittedCall(t *testing.T, rows []pairRecRow) {
	t.Helper()
	type key struct {
		step, kind, member string
		slot               int64
	}
	bounds := append(append([]int64{0}, pairPaperResumeEpochSeqs(rows)...), 1<<62)
	for i := 1; i < len(bounds)-1; i++ {
		before := map[key]string{}
		for _, r := range rows {
			if r.Kind == pairRecEventStart && r.Seq < bounds[i] && r.Event.Start.Phase == pairRecPhaseStep && r.Event.Start.ParentID == "" && pairPaperOncePerStepKinds[r.Event.Start.Kind] {
				s := r.Event.Start
				before[key{s.StepID, s.Kind, s.MemberID, s.SlotMs}] = r.CallID
			}
		}
		for _, r := range rows {
			if r.Kind != pairRecEventStart || r.Seq < bounds[i] || r.Seq >= bounds[i+1] || r.Event.Start.Phase != pairRecPhaseStep || r.Event.Start.ParentID != "" {
				continue
			}
			s := r.Event.Start
			if id, ok := before[key{s.StepID, s.Kind, s.MemberID, s.SlotMs}]; ok {
				t.Fatalf("the %s call %s of step %s for %q was committed before the restart at input %d and made again as %s", s.Kind, id, s.StepID, s.MemberID, bounds[i], r.CallID)
			}
		}
	}
}

func TestBasketReplayPairPaperResumesAnAppliedOKXFundingEventFromItsRecordedEvidence(t *testing.T) {
	f := newPairPaperFixture(t, pairPaperOptions2{runID: "paper-okx-funding", collectS: 110, drainS: 15, convergeS: 100, minuteAfterS: 30, capRefresh: 20000,
		fixtureMore: map[string]interface{}{"follow_funding": false}})
	eventMs := f.fromMs + 30000
	fx := map[string]interface{}{}
	_ = json.Unmarshal(pairPaperReadFile(t, f.fixture), &fx)
	fx["okx_funding"] = [][]interface{}{{f.fromMs - 1000, "0.0001"}, {eventMs, "0.0001"}, {f.fromMs + 115000, "0.0001"}}
	fx["hl_funding"] = [][]interface{}{{f.fromMs + 115000, "0.0000125"}}
	pairPaperWriteJSON(t, f.fixture, fx)
	f.scenario.PaperFundingPollMs = 25000
	dir := filepath.Join(f.parent, "paper")
	okx := ""
	for _, sc := range pairPaperMembers(t, f) {
		if strings.Contains(sc, "okx") {
			okx = sc
		}
	}
	first := runPairPaperAdapter(t, f, f.spec(basketReplayModeNew, dir), f.env(false), "--max-steps", "4", "--seal")
	if first.Out.Run == nil || first.Out.Run.Finished || first.Out.Refusal != nil {
		t.Fatalf("the first chunk stops resumable, got exit %d run %+v refusal %+v\nstderr tail=%s", first.Code, first.Out.Run, first.Out.Refusal, tail(first.Stderr, 4000))
	}
	crashed, appliedSeen, resumesAfterApplied := false, false, 0
	var last pairPaperProc
	for chunk := 0; chunk < 60; chunk++ {
		if appliedSeen && !crashed {
			crash := runPairPaperAdapter(t, f, f.spec(basketReplayModeResume, dir), f.env(false), "--fault", "exit-after-book-commit:1")
			if crash.Code != basketReplayExitFault || !strings.Contains(crash.Stderr, "fault exit-after-book-commit:1\n") {
				t.Fatalf("a resume after the applied OKX funding event must admit and reach the book commit fault, got exit %d refusal %+v error %q\nstderr tail=%s", crash.Code, crash.Out.Refusal, crash.Out.Error, tail(crash.Stderr, 6000))
			}
			crashed = true
			resumesAfterApplied++
			continue
		}
		last = runPairPaperAdapter(t, f, f.spec(basketReplayModeResume, dir), f.env(false), "--max-steps", "4", "--seal")
		pairPaperRequireResumed(t, fmt.Sprintf("chunk %d", chunk), last)
		if appliedSeen {
			resumesAfterApplied++
		}
		if last.Out.Run.Finished {
			break
		}
		if pairPaperApplied(pairPaperFundingRows(t, dir), okx, eventMs) != nil {
			appliedSeen = true
		}
	}
	if !last.Out.Run.Finished || !crashed || resumesAfterApplied < 2 {
		t.Fatalf("the chunked run must finish with resumes after the applied event, got finished=%v crashed=%v resumes after applied=%d funding %+v", last.Out.Run.Finished, crashed, resumesAfterApplied, pairPaperFundingRows(t, dir))
	}
	pairPaperRequireComplete(t, "chunked paper run", last)
	if last.Out.Recording == nil || last.Out.Recording.Seal == nil || !last.Out.Recording.Seal.Sealed {
		t.Fatalf("the finished chunked run seals: %+v", last.Out.Recording)
	}
	applied := pairPaperApplied(pairPaperFundingRows(t, dir), okx, eventMs)
	if applied == nil {
		t.Fatalf("the OKX funding event at %d must be applied to the held position, got %+v", eventMs, pairPaperFundingRows(t, dir))
	}
	var ev pairFundingEvent
	if err := json.Unmarshal([]byte(applied.EventJSON), &ev); err != nil {
		t.Fatal(err)
	}
	var doc pairEvidenceDocument
	if err := json.Unmarshal(pairPaperReadFile(t, filepath.Join(dir, pairPaperExportDirName, "evidence.json")), &doc); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, c := range doc.Conversions {
		if c.EvidenceHash == ev.Conversion.EvidenceHash && c.Key == ev.Conversion.Key {
			found = true
		}
	}
	if !found || ev.Conversion.EvidenceHash == "" {
		t.Fatalf("the applied event's conversion %s must be a record of the restored evidence document", ev.Conversion.EvidenceID)
	}
	pairPaperNoRecommittedCall(t, pairPaperLogRows(t, dir, f.runID))
	playback := pairPaperReproduce(t, f, dir, "reproduction")
	if playback.Out.Comparison == nil || playback.Out.Comparison.Hash != last.Out.Comparison.Hash || !playback.Out.Comparison.Eligible {
		t.Fatalf("the offline reproduction of the chunked recording equals it, got paper %+v playback %+v\n%s", last.Out.Comparison, playback.Out.Comparison, pairOfflineDiff(t, last.Out, playback.Out))
	}
}

func TestBasketReplayPairPaperFundingCrashResumeConsumesTheCommittedTail(t *testing.T) {
	cases := []struct {
		name     string
		hl       func(from int64) [][]interface{}
		complete bool
	}{
		{"hyperliquid-window-with-a-record", func(from int64) [][]interface{} {
			return [][]interface{}{{from, "0.0000125"}, {from + 45000, "0.0000125"}}
		}, true},
		{"hyperliquid-window-without-a-record", func(from int64) [][]interface{} { return [][]interface{}{} }, false},
	}
	slots := make(chan struct{}, 2)
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			slots <- struct{}{}
			defer func() { <-slots }()
			f := newPairPaperFixture(t, pairPaperOptions2{runID: "paper-funding-tail", collectS: 30, drainS: 20, convergeS: 16, fixtureMore: map[string]interface{}{"follow_funding": false}})
			fx := map[string]interface{}{}
			_ = json.Unmarshal(pairPaperReadFile(t, f.fixture), &fx)
			fx["hl_funding"] = tc.hl(f.fromMs)
			fx["okx_funding"] = [][]interface{}{{f.fromMs - 1000, "0.0001"}, {f.fromMs + 45000, "0.0001"}}
			pairPaperWriteJSON(t, f.fixture, fx)
			f.scenario.PaperFundingPollMs = 10000
			dir := filepath.Join(f.parent, "paper")
			crash := runPairPaperAdapter(t, f, f.spec(basketReplayModeNew, dir), f.env(false), "--fault", "exit-after-management:2")
			if crash.Code != basketReplayExitFault || !strings.Contains(crash.Stderr, "fault exit-after-management:2\n") {
				t.Fatalf("the fault must exit 75 after the second management cycle, got exit %d\nstderr tail=%s", crash.Code, tail(crash.Stderr, 4000))
			}
			before := pairPaperLogRows(t, dir, f.runID)
			last := before[len(before)-1]
			stepID := ""
			for i := len(before) - 1; i >= 0; i-- {
				if before[i].Kind == pairRecEventStart && before[i].Event.Start.Phase == pairRecPhaseStep {
					stepID = before[i].Event.Start.StepID
					break
				}
			}
			members := map[string]bool{}
			for _, r := range before {
				if r.Kind == pairRecEventStart && r.Event.Start.StepID == stepID && r.Event.Start.Kind == pairRecKindFunding {
					members[r.Event.Start.MemberID] = true
				}
			}
			if len(members) != 2 {
				t.Fatalf("the crashed step %s must hold committed funding reads for both members before the crash at input %d, got %v", stepID, last.Seq, members)
			}
			resumed := runPairPaperAdapter(t, f, f.spec(basketReplayModeResume, dir), f.env(false), "--seal")
			pairPaperRequireResumed(t, "resume", resumed)
			if !resumed.Out.Run.Finished || resumed.Out.Recording == nil || resumed.Out.Recording.Seal == nil || !resumed.Out.Recording.Seal.Sealed {
				t.Fatalf("the resumed run finishes and seals, got run %+v recording %+v reasons %v", resumed.Out.Run, resumed.Out.Recording, resumed.Out.IncompleteReasons)
			}
			after := pairPaperLogRows(t, dir, f.runID)
			for i := range before {
				if after[i].Prefix != before[i].Prefix {
					t.Fatalf("the committed prefix changed at input %d across the restart", before[i].Seq)
				}
			}
			pairPaperNoRecommittedCall(t, after)
			for _, r := range after[len(before):] {
				if r.Kind == pairRecEventStart && r.Event.Start.StepID == stepID && r.Event.Start.Kind == pairRecKindFunding {
					t.Fatalf("the resumed step %s made its committed funding read for %s again as %s", stepID, r.Event.Start.MemberID, r.CallID)
				}
			}
			if tc.complete {
				pairPaperRequireComplete(t, "resumed paper run", resumed)
			}
			playback := pairPaperReproduce(t, f, dir, "reproduction")
			if playback.Out.Comparison == nil || playback.Out.Comparison.Hash != resumed.Out.Comparison.Hash || playback.Out.MechanicallyComplete != resumed.Out.MechanicallyComplete {
				t.Fatalf("the offline reproduction equals the resumed recording, got paper %+v playback %+v\n%s", resumed.Out.Comparison, playback.Out.Comparison, pairOfflineDiff(t, resumed.Out, playback.Out))
			}
			if tc.complete && !playback.Out.Comparison.Eligible {
				t.Fatalf("a complete resumed recording with no interrupted call reproduces with an equality claim: %+v", playback.Out.Comparison)
			}
			for _, r := range playback.Out.Comparison.Reasons {
				if strings.Contains(r, "diverged") || strings.Contains(r, "never consumed") {
					t.Fatalf("the playback consumes every recorded call in order, got %v", playback.Out.Comparison.Reasons)
				}
			}
		})
	}
}

func TestBasketReplayPairPaperBarrierCursorSurvivesLaterCrashes(t *testing.T) {
	slots := make(chan struct{}, 2)
	t.Run("blocked-chunk-then-mid-step-crash", func(t *testing.T) {
		t.Parallel()
		slots <- struct{}{}
		defer func() { <-slots }()
		f := newPairPaperFixture(t, pairPaperOptions2{runID: "paper-barrier-chunk", collectS: 30, drainS: 20, convergeS: 16})
		dir := filepath.Join(f.parent, "paper")
		first := runPairPaperAdapter(t, f, f.spec(basketReplayModeNew, dir), f.env(false), "--fault", "fail-record-append:80", "--max-steps", "10")
		if first.Out.Recording == nil || !first.Out.Recording.BarrierSet || first.Out.Run == nil || first.Out.Run.Finished || first.Out.Run.StepsApplied != 10 {
			t.Fatalf("the chunk ends with the barrier set and the run resumable, got exit %d run %+v recording %+v\nstderr tail=%s", first.Code, first.Out.Run, first.Out.Recording, tail(first.Stderr, 4000))
		}
		critical := strings.Index(first.Stderr, "CRITICAL: the recording append failed")
		refused := strings.Index(first.Stderr, "the match_poll call was not made: the recording barrier refuses every new outside read")
		if critical < 0 || refused < critical {
			t.Fatalf("after the append failure no new match poll is read, as SKILL.md states\nstderr tail=%s", tail(first.Stderr, 4000))
		}
		committed := pairPaperLogRows(t, dir, f.runID)
		stream := 0
		for _, r := range committed {
			if pairRecConsumable(r) {
				stream++
			}
		}
		if got := pairPaperSavedCursor(t, dir, f.runID); got != stream {
			t.Fatalf("the saved recording cursor must count committed stream rows only: saved %d, committed %d", got, stream)
		}
		crash := runPairPaperAdapter(t, f, f.spec(basketReplayModeResume, dir), f.env(false), "--fault", "exit-after-call-start:20")
		if crash.Code != basketReplayExitFault || !strings.Contains(crash.Stderr, "fault exit-after-call-start:20\n") {
			t.Fatalf("the resume clears the barrier and reaches the call start fault, got exit %d refusal %+v\nstderr tail=%s", crash.Code, crash.Out.Refusal, tail(crash.Stderr, 4000))
		}
		final := runPairPaperAdapter(t, f, f.spec(basketReplayModeResume, dir), f.env(false), "--seal")
		pairPaperRequireResumed(t, "final resume", final)
		if !final.Out.Run.Finished || final.Out.Recording == nil || final.Out.Recording.BarrierSet || final.Out.Recording.Seal == nil || !final.Out.Recording.Seal.Sealed {
			t.Fatalf("the run finishes and seals after both restarts, got run %+v recording %+v reasons %v", final.Out.Run, final.Out.Recording, final.Out.IncompleteReasons)
		}
		rows := pairPaperLogRows(t, dir, f.runID)
		pairPaperNoRepeatedCall(t, rows)
		pairPaperNoRecommittedCall(t, rows)
	})
	t.Run("blocked-pending-step-then-later-crash", func(t *testing.T) {
		t.Parallel()
		slots <- struct{}{}
		defer func() { <-slots }()
		f := newPairPaperFixture(t, pairPaperOptions2{runID: "paper-barrier-pending", collectS: 30, drainS: 20, convergeS: 16})
		dir := filepath.Join(f.parent, "paper")
		first := runPairPaperAdapter(t, f, f.spec(basketReplayModeNew, dir), f.env(false), "--fault", "fail-record-append:80", "--fault", "exit-after-management:3")
		critical := strings.Index(first.Stderr, "CRITICAL: the recording append failed")
		fault := strings.Index(first.Stderr, "fault exit-after-management:3\n")
		if first.Code != basketReplayExitFault || critical < 0 || fault < critical {
			t.Fatalf("the process stops inside a step while the barrier holds, got exit %d\nstderr tail=%s", first.Code, tail(first.Stderr, 4000))
		}
		second := runPairPaperAdapter(t, f, f.spec(basketReplayModeResume, dir), f.env(false), "--fault", "exit-after-management:2")
		if second.Code != basketReplayExitFault || !strings.Contains(second.Stderr, "fault exit-after-management:2\n") {
			t.Fatalf("the resume clears the barrier, finishes the pending step and stops in a later step, got exit %d refusal %+v\nstderr tail=%s", second.Code, second.Out.Refusal, tail(second.Stderr, 4000))
		}
		final := runPairPaperAdapter(t, f, f.spec(basketReplayModeResume, dir), f.env(false), "--seal")
		pairPaperRequireResumed(t, "final resume", final)
		if !final.Out.Run.Finished || final.Out.Recording == nil || final.Out.Recording.BarrierSet || final.Out.Recording.Seal == nil || !final.Out.Recording.Seal.Sealed {
			t.Fatalf("the run finishes and seals, got run %+v recording %+v reasons %v", final.Out.Run, final.Out.Recording, final.Out.IncompleteReasons)
		}
		pairPaperNoRecommittedCall(t, pairPaperLogRows(t, dir, f.runID))
	})
}

func pairPaperSavedCursor(t *testing.T, stateDir, runID string) int {
	t.Helper()
	db, err := openStateDBReadOnly(filepath.Join(stateDir, basketReplayDBName))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	c, _, err := db.readPairRecCursor(runID)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func pairPaperMembers(t *testing.T, f *pairPaperFixture) []string {
	t.Helper()
	var shape pairReplayResearchShape
	if err := json.Unmarshal(f.doc, &shape); err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, m := range shape.Pairs[0].Members {
		ids = append(ids, m.StrategyID)
	}
	sort.Strings(ids)
	return ids
}

func pairPaperReadFile(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func pairPaperWriteJSON(t *testing.T, path string, v interface{}) {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
}

type pairPaperOracleFaults struct {
	callStart int
	inputEnd  int
}

func pairPaperResearchOracleFaultCounts(t *testing.T, opts pairPaperResearchOptions) pairPaperOracleFaults {
	t.Helper()
	opts.runID = "paper-research-dry"
	f, _ := newPairPaperResearchFixture(t, opts)
	dir := filepath.Join(f.parent, "dry")
	p := runPairPaperAdapter(t, f, f.spec(basketReplayModeNew, dir), f.env(false))
	if p.Out.Run == nil || !p.Out.Run.Finished {
		t.Fatalf("the dry research run finishes, got exit %d %+v\nstderr tail=%s", p.Code, p.Out.Refusal, tail(p.Stderr, 4000))
	}
	rows := pairPaperLogRows(t, dir, f.runID)
	out := pairPaperOracleFaults{}
	starts := 0
	oracleCall := ""
	for _, r := range rows {
		if r.Kind == pairRecEventStart {
			starts++
			if r.Event.Start.Kind == pairRecKindFundingOracle {
				if oracleCall != "" {
					t.Fatal("the dry run holds one oracle attempt")
				}
				oracleCall, out.callStart = r.CallID, starts
			}
		}
		if r.Kind == pairRecEventEnd && r.CallID == oracleCall {
			out.inputEnd = int(r.Seq)
		}
	}
	if out.callStart == 0 || out.inputEnd == 0 {
		t.Fatalf("the dry run commits an oracle start and terminal, got %+v", out)
	}
	return out
}

func pairPaperOracleRows(rows []pairRecRow) (starts, ends, recoveries int) {
	calls := map[string]bool{}
	for _, r := range rows {
		switch {
		case r.Kind == pairRecEventStart && r.Event.Start.Kind == pairRecKindFundingOracle:
			starts++
			calls[r.CallID] = true
		case r.Kind == pairRecEventEnd && calls[r.CallID]:
			ends++
		case r.Kind == pairRecEventRecovery && calls[r.CallID]:
			recoveries++
		}
	}
	return
}

func TestBasketReplayPairPaperResearchOracleResume(t *testing.T) {
	counts := pairPaperResearchOracleFaultCounts(t, pairPaperResearchOptions{})
	late := pairPaperResearchOptions{fixtureMore: map[string]interface{}{"oracle_response_delay_ms": 5000}}
	lateCounts := pairPaperResearchOracleFaultCounts(t, late)
	slots := make(chan struct{}, 3)
	crash := func(t *testing.T, f *pairPaperFixture, dir, fault string) []pairRecRow {
		t.Helper()
		p := runPairPaperAdapter(t, f, f.spec(basketReplayModeNew, dir), f.env(false), "--fault", fault)
		if p.Code != basketReplayExitFault || !strings.Contains(p.Stderr, "fault "+fault+"\n") {
			t.Fatalf("the fault %s exits 75 at its boundary, got exit %d\nstderr tail=%s", fault, p.Code, tail(p.Stderr, 4000))
		}
		return pairPaperLogRows(t, dir, f.runID)
	}
	t.Run("start-only recovery never repeats the hour", func(t *testing.T) {
		t.Parallel()
		slots <- struct{}{}
		defer func() { <-slots }()
		f, target := newPairPaperResearchFixture(t, pairPaperResearchOptions{runID: "paper-oracle-start-only"})
		dir := filepath.Join(f.parent, "paper")
		before := crash(t, f, dir, fmt.Sprintf("exit-after-call-start:%d", counts.callStart))
		if last := before[len(before)-1]; last.Kind != pairRecEventStart || last.Event.Start.Kind != pairRecKindFundingOracle {
			t.Fatalf("the crash leaves the oracle start as the last committed row, got %s", last.Kind)
		}
		resumed := runPairPaperAdapter(t, f, f.spec(basketReplayModeResume, dir), f.env(false), "--seal")
		pairPaperRequireResumed(t, "start-only resume", resumed)
		after := pairPaperLogRows(t, dir, f.runID)
		if s, e, rcv := pairPaperOracleRows(after); s != 1 || e != 0 || rcv != 1 {
			t.Fatalf("the interrupted oracle start is marked unknown once and never repeated, got %d starts %d ends %d recoveries", s, e, rcv)
		}
		pairPaperNoRepeatedCall(t, after)
		if n := pairPaperMetaReads(f); n != 0 {
			t.Fatalf("the interrupted attempt read nothing and no later read repairs the hour, got %d reads", n)
		}
		event := pairFundingEventID(entryProviderHyperliquid, "BTC", target+3)
		for _, row := range pairPaperFundingRows(t, dir) {
			if row.StrategyID == pairTestHL && row.EventMs == target+3 && row.Status == pairFundingStatusApplied {
				t.Fatalf("the held hour with no sample is never charged: %s", event)
			}
		}
		if resumed.Out.MechanicallyComplete {
			t.Fatal("a held hour whose sample was interrupted keeps the run incomplete")
		}
	})
	t.Run("terminal before publication resumes past the deadline", func(t *testing.T) {
		t.Parallel()
		slots <- struct{}{}
		defer func() { <-slots }()
		f, target := newPairPaperResearchFixture(t, pairPaperResearchOptions{runID: "paper-oracle-terminal"})
		dir := filepath.Join(f.parent, "paper")
		before := crash(t, f, dir, fmt.Sprintf("exit-after-input-commit:%d", counts.inputEnd))
		last := before[len(before)-1]
		if last.Kind != pairRecEventEnd || last.Event.End.Class != pairRecClassResult {
			t.Fatalf("the crash leaves the committed oracle terminal as the last row, got %s", last.Kind)
		}
		clock, _ := strconv.ParseInt(strings.TrimSpace(string(pairPaperReadFile(t, f.clock))), 10, 64)
		if err := os.WriteFile(f.clock, []byte(strconv.FormatInt(clock+15000, 10)), 0o600); err != nil {
			t.Fatal(err)
		}
		deadline, _ := pairFundingOracleDeadline(*f.scenario.FundingPolicy, target)
		if clock+15000 <= deadline {
			t.Fatalf("the restart happens after the sampling deadline %d, clock %d", deadline, clock+15000)
		}
		reads := pairPaperMetaReads(f)
		chunk := filepath.Join(f.parent, "chunk")
		_ = exec.Command("cp", "-R", dir, chunk).Run()
		resumed := runPairPaperAdapter(t, f, f.spec(basketReplayModeResume, dir), f.env(false), "--seal")
		pairPaperRequireResumed(t, "terminal resume", resumed)
		pairPaperRequireComplete(t, "terminal resume", resumed)
		after := pairPaperLogRows(t, dir, f.runID)
		if s, e, rcv := pairPaperOracleRows(after); s != 1 || e != 1 || rcv != 0 {
			t.Fatalf("the committed terminal is consumed from the recording, got %d starts %d ends %d recoveries", s, e, rcv)
		}
		if n := pairPaperMetaReads(f); n != reads {
			t.Fatalf("the resume makes no new outside oracle read, got %d new reads", n-reads)
		}
		applied := pairPaperApplied(pairPaperFundingRows(t, dir), pairTestHL, target+3)
		if applied == nil {
			t.Fatal("the restored sample settles the held hour after a restart past the deadline")
		}
		ev, err := pairDecodeFundingEvent([]byte(applied.EventJSON))
		if err != nil || ev.Research == nil || ev.Research.ObservationTargetEventMs != target {
			t.Fatalf("the restored version 3 event uses the recorded sample: %+v %v", ev, err)
		}
		playback := pairPaperReproduce(t, f, dir, "reproduction")
		if playback.Out.Comparison == nil || playback.Out.Comparison.Hash != resumed.Out.Comparison.Hash || !playback.Out.Comparison.Eligible {
			t.Fatalf("the offline reproduction of the resumed research recording equals it, got paper %+v playback %+v\n%s", resumed.Out.Comparison, playback.Out.Comparison, pairOfflineDiff(t, resumed.Out, playback.Out))
		}
		mid := runPairPaperAdapter(t, f, f.spec(basketReplayModeResume, chunk), f.env(false), "--max-steps", "4")
		pairPaperRequireResumed(t, "chunked terminal resume", mid)
		db, err := OpenStateDB(filepath.Join(chunk, basketReplayDBName))
		if err != nil {
			t.Fatal(err)
		}
		var seq int64
		var raw string
		if err := db.db.QueryRow(`SELECT seq, batch_json FROM pair_funding_chain WHERE batch_json LIKE '%"observations":[{%' ORDER BY seq LIMIT 1`).Scan(&seq, &raw); err != nil {
			t.Fatalf("the admitted oracle batch is saved in the funding chain: %v", err)
		}
		if _, err := db.db.Exec(`DROP TRIGGER trg_pair_funding_chain_immutable`); err != nil {
			t.Fatal(err)
		}
		if _, err := db.db.Exec(`UPDATE pair_funding_chain SET batch_json = ? WHERE seq = ?`, strings.Replace(raw, `"60010.5"`, `"60011.5"`, 1), seq); err != nil {
			t.Fatal(err)
		}
		db.Close()
		refused := runPairPaperAdapter(t, f, f.spec(basketReplayModeResume, chunk), f.env(false), "--max-steps", "4")
		if refused.Code != basketReplayExitRefused {
			t.Fatalf("a saved chain row the recording no longer proves refuses the resume, got exit %d %+v", refused.Code, refused.Out.Refusal)
		}
	})
	t.Run("refused terminal batch keeps its sequence for the next read", func(t *testing.T) {
		t.Parallel()
		slots <- struct{}{}
		defer func() { <-slots }()
		late.runID = "paper-oracle-refused"
		f, _ := newPairPaperResearchFixture(t, late)
		dir := filepath.Join(f.parent, "paper")
		crash(t, f, dir, fmt.Sprintf("exit-after-input-commit:%d", lateCounts.inputEnd))
		resumed := runPairPaperAdapter(t, f, f.spec(basketReplayModeResume, dir), f.env(false), "--seal")
		pairPaperRequireResumed(t, "refused terminal resume", resumed)
		o := pairPaperOracleOutcomeOf(t, dir, f.runID)
		if len(o.starts) != 1 || len(o.refusedSeq) != 1 || len(o.admitted) != 0 {
			t.Fatalf("the late terminal stays only in the recording after the restart: starts %d refused %v admitted %d", len(o.starts), o.refusedSeq, len(o.admitted))
		}
		if _, ok := o.chainSeqs[o.refusedSeq[0]]; !ok {
			t.Fatalf("a later accepted batch reuses the refused sequence %d: %v", o.refusedSeq[0], o.chainSeqs)
		}
		pairPaperNoRepeatedCall(t, pairPaperLogRows(t, dir, f.runID))
	})
}

func pairPaperOracleFailureRows(rows []pairRecRow) int {
	n := 0
	for _, r := range rows {
		if r.Kind == pairRecEventFailure && r.Event.Failure.CallKind == pairRecKindFundingOracle {
			n++
		}
	}
	return n
}

func pairPaperAppendFaultEnv(f *pairPaperFixture, fromMs, toMs int64) []string {
	return append(f.env(false), fmt.Sprintf("%s=%d:%d", pairPaperTestAppendFaultEnv, fromMs, toMs))
}

func TestBasketReplayPairPaperResearchOracleBarrierDecidesFromTheSameWake(t *testing.T) {
	t.Parallel()
	f, target := newPairPaperResearchFixture(t, pairPaperResearchOptions{runID: "paper-oracle-barrier-clear"})
	policy := *f.scenario.FundingPolicy
	prepare, _ := pairPaperOraclePrepareAt(target, policy)
	deadline, _ := pairFundingOracleDeadline(policy, target)
	clearFrom := prepare + 10000
	dir := filepath.Join(f.parent, "paper")
	p := runPairPaperAdapter(t, f, f.spec(basketReplayModeNew, dir), pairPaperAppendFaultEnv(f, prepare, clearFrom), "--seal")
	if p.Code == basketReplayExitRefused || p.Out.Run == nil || !p.Out.Run.Finished || p.Out.Comparison == nil || p.Out.Recording == nil || p.Out.Recording.BarrierSet || p.Out.Recording.Seal == nil || !p.Out.Recording.Seal.Sealed {
		t.Fatalf("a barrier that clears in the same process lets the run finish and seal, got exit %d recording %+v reasons %v refusal %+v\nstderr tail=%s", p.Code, p.Out.Recording, p.Out.IncompleteReasons, p.Out.Refusal, tail(p.Stderr, 6000))
	}
	rows := pairPaperLogRows(t, dir, f.runID)
	clears := 0
	for _, r := range rows {
		if r.Kind == pairRecEventClear {
			clears++
		}
	}
	notMade, missed := 0, 0
	for _, s := range pairOfflineSteps(t, p.Out) {
		for _, tr := range s.Trace {
			switch tr {
			case "oracle_not_made":
				notMade++
				if s.AtMs < prepare || s.AtMs >= clearFrom {
					t.Fatalf("only a step whose wake the barrier held refuses the oracle call, got step at %d outside [%d, %d)", s.AtMs, prepare, clearFrom)
				}
			case "oracle_missed":
				missed++
			}
		}
	}
	failures := pairPaperOracleFailureRows(rows)
	if clears != 1 || notMade == 0 || failures != notMade || missed != 0 {
		t.Fatalf("each held step queues one refused oracle row that the in-process release commits, got %d clears, %d not-made steps, %d failure rows, %d missed", clears, notMade, failures, missed)
	}
	var start, end pairRecRow
	for _, r := range rows {
		if r.Kind == pairRecEventStart && r.Event.Start.Kind == pairRecKindFundingOracle {
			start = r
		}
	}
	for _, r := range rows {
		if r.Kind == pairRecEventEnd && r.CallID == start.CallID {
			end = r
		}
	}
	if s, e, rcv := pairPaperOracleRows(rows); s != 1 || e != 1 || rcv != 0 || start.Event.Start.RawStartMs < clearFrom || end.Event.End.Class != pairRecClassResult {
		t.Fatalf("the hour stays for the first committed wake after the release and reads once, got %d starts %d ends %d recoveries, start %+v", s, e, rcv, start.Event.Start)
	}
	batch, err := pairDecodeFundingBatch(end.Event.End.Result)
	if err != nil || batch.Research == nil || len(batch.Research.Observations) != 1 {
		t.Fatalf("the oracle terminal holds one observation batch: %v", err)
	}
	obs := batch.Research.Observations[0]
	if obs.TargetEventMs != target || obs.RequestMs < target || obs.ReceiptMs > deadline {
		t.Fatalf("the observation lies inside [%d, %d]: %+v", target, deadline, obs)
	}
	row, ev := pairPaperResearchEvent(t, p.Out, target+3)
	if ev.Research == nil || ev.PriceHash != obs.RecordHash || row["status"] != pairFundingStatusApplied {
		t.Fatalf("the held hour settles once from the observation read after the release: %+v %+v", row, ev)
	}
	reads := pairPaperMetaReads(f)
	if reads != 1 {
		t.Fatalf("the paper run reads metaAndAssetCtxs once, got %d", reads)
	}
	playback := pairPaperReproduce(t, f, dir, "reproduction")
	if playback.Code == basketReplayExitRefused || playback.Out.Refusal != nil || playback.Out.Comparison == nil || playback.Code != p.Code || playback.Out.MechanicallyComplete != p.Out.MechanicallyComplete {
		t.Fatalf("the sealed playback with every network route blocked ends like the live run, got exit %d (live %d) refusal %+v reasons %v\nstderr tail=%s", playback.Code, p.Code, playback.Out.Refusal, playback.Out.IncompleteReasons, tail(playback.Stderr, 6000))
	}
	for _, r := range append(append([]string{}, playback.Out.IncompleteReasons...), playback.Out.Comparison.Reasons...) {
		if strings.Contains(r, "diverged") {
			t.Fatalf("the playback consumes the refused oracle rows at the same steps without a divergence, got %s", r)
		}
	}
	if p.Out.Comparison.Eligible || playback.Out.Comparison.Eligible {
		t.Fatalf("a run with refused calls makes no equality claim, got live %+v playback %+v", p.Out.Comparison, playback.Out.Comparison)
	}
	if _, pev := pairPaperResearchEvent(t, playback.Out, target+3); pev.PriceHash != obs.RecordHash || pev.EvidenceHash != ev.EvidenceHash || pev.USDAmount != ev.USDAmount {
		t.Fatalf("the playback settles the same amount from the same observation: live %+v playback %+v", ev, pev)
	}
	if n := pairPaperMetaReads(f); n != reads {
		t.Fatalf("playback never runs the public oracle reader, saw %d new reads", n-reads)
	}
}

func TestBasketReplayPairPaperResearchOracleBarrierRestartReadsTheHeldHourOnce(t *testing.T) {
	t.Parallel()
	f, target := newPairPaperResearchFixture(t, pairPaperResearchOptions{runID: "paper-oracle-barrier-restart"})
	policy := *f.scenario.FundingPolicy
	prepare, _ := pairPaperOraclePrepareAt(target, policy)
	dir := filepath.Join(f.parent, "paper")
	first := runPairPaperAdapter(t, f, f.spec(basketReplayModeNew, dir), pairPaperAppendFaultEnv(f, prepare, f.drain+3600000), "--max-steps", "21")
	if first.Code == basketReplayExitRefused || first.Out.Recording == nil || !first.Out.Recording.BarrierSet {
		t.Fatalf("the first process stops while the barrier holds, got exit %d recording %+v refusal %+v\nstderr tail=%s", first.Code, first.Out.Recording, first.Out.Refusal, tail(first.Stderr, 6000))
	}
	held := false
	for _, s := range pairOfflineSteps(t, first.Out) {
		for _, tr := range s.Trace {
			if tr == "oracle_not_made" {
				held = true
			}
		}
	}
	if !held || pairPaperMetaReads(f) != 0 {
		t.Fatalf("the held process refuses the oracle call before any read, got held=%v reads=%d", held, pairPaperMetaReads(f))
	}
	resumed := runPairPaperAdapter(t, f, f.spec(basketReplayModeResume, dir), f.env(false))
	pairPaperRequireResumed(t, "restart after a held oracle hour", resumed)
	rows := pairPaperLogRows(t, dir, f.runID)
	if s, _, _ := pairPaperOracleRows(rows); s != 1 || pairPaperMetaReads(f) != 1 || pairPaperOracleFailureRows(rows) != 0 {
		t.Fatalf("the restart reads the held hour once and keeps no row of the stopped process, got %d starts, %d reads, %d failure rows", s, pairPaperMetaReads(f), pairPaperOracleFailureRows(rows))
	}
}

func TestBasketReplayPairPaperResearchOracleBarrierOutsidePreparationAddsNoOracleRow(t *testing.T) {
	t.Parallel()
	f, target := newPairPaperResearchFixture(t, pairPaperResearchOptions{runID: "paper-oracle-barrier-early"})
	policy := *f.scenario.FundingPolicy
	prepare, _ := pairPaperOraclePrepareAt(target, policy)
	dir := filepath.Join(f.parent, "paper")
	p := runPairPaperAdapter(t, f, f.spec(basketReplayModeNew, dir), pairPaperAppendFaultEnv(f, prepare-16000, prepare-6000), "--seal")
	if p.Code == basketReplayExitRefused || p.Out.Run == nil || !p.Out.Run.Finished || p.Out.Recording == nil || p.Out.Recording.BarrierSet {
		t.Fatalf("a barrier before preparation clears in process and the run finishes, got exit %d recording %+v refusal %+v\nstderr tail=%s", p.Code, p.Out.Recording, p.Out.Refusal, tail(p.Stderr, 6000))
	}
	rows := pairPaperLogRows(t, dir, f.runID)
	clears := 0
	for _, r := range rows {
		if r.Kind == pairRecEventClear {
			clears++
		}
	}
	if s, e, _ := pairPaperOracleRows(rows); clears != 1 || s != 1 || e != 1 || pairPaperOracleFailureRows(rows) != 0 {
		t.Fatalf("a barrier window with no preparation window adds no oracle row, got %d clears, %d starts, %d ends, %d failure rows", clears, s, e, pairPaperOracleFailureRows(rows))
	}
}

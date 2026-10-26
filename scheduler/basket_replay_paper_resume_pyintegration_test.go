//go:build pyintegration

package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
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

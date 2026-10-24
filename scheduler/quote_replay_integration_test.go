package main

import (
	"encoding/json"
	"fmt"
	"math/big"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync/atomic"
	"testing"

	"trading-scheduler/internal/egresspolicy"
)

type quoteReplayTestEvent struct {
	quoteReplayVenueEventRow
	payload quotePaperEventPayload
}

func quoteReplayEvents(t *testing.T, out quoteReplayOutput) []quoteReplayTestEvent {
	t.Helper()
	var evs []quoteReplayTestEvent
	for _, e := range out.VenueEvents {
		var p quotePaperEventPayload
		if err := json.Unmarshal(e.Event, &p); err != nil {
			t.Fatalf("venue event %d: %v", e.Seq, err)
		}
		evs = append(evs, quoteReplayTestEvent{e, p})
	}
	return evs
}

func quoteReplayDecisionRecords(t *testing.T, out quoteReplayOutput) []quoteReplayDecisionRecord {
	t.Helper()
	var recs []quoteReplayDecisionRecord
	for _, raw := range out.Decisions {
		var rec quoteReplayDecisionRecord
		if err := json.Unmarshal(raw, &rec); err != nil {
			t.Fatal(err)
		}
		recs = append(recs, rec)
	}
	return recs
}

func quoteReplayCheckInvariants(t *testing.T, spec *quoteReplaySpec, out quoteReplayOutput) {
	t.Helper()
	params, err := parseQuotePolicyParams(spec.Params)
	if err != nil {
		t.Fatal(err)
	}
	trades := map[uint64]quoteReplayTradeSpec{}
	for _, tr := range spec.Trades {
		trades[tr.Tid] = tr
	}
	deadlines := map[int64]bool{}
	cycle := int64(spec.Config.IntervalSeconds) * 1000
	for d := spec.Window.FirstDeadlineMs; d <= spec.Window.LastDeadlineMs; d += cycle {
		deadlines[d] = true
	}
	accepted := map[string]int64{}
	orders := map[string]quoteOrderRequest{}
	used := map[uint64]*big.Rat{}
	position := quoteRatZero()
	sent := map[string]int{}
	fillKeys := map[string]bool{}
	for _, e := range quoteReplayEvents(t, out) {
		switch e.Kind {
		case "place":
			sent[e.ClientID]++
			if sent[e.ClientID] > 1 {
				t.Fatalf("client id %s was sent %d times", e.ClientID, sent[e.ClientID])
			}
			d := e.EffectMs - e.EffectMs%cycle
			if !deadlines[d] || e.EffectMs == d || e.EffectMs-d >= cycle {
				t.Fatalf("place %s took effect at %d, not inside a due deadline call", e.ClientID, e.EffectMs)
			}
			orders[strings.ToLower(e.ClientID)] = *e.payload.Request
			if e.payload.Outcome.Status == quoteOutcomeResting {
				accepted[strings.ToLower(e.ClientID)] = e.payload.Outcome.AcceptedAtMs
				if e.payload.Outcome.AcceptedAtMs != e.EffectMs {
					t.Fatalf("acceptance %d is not the effect time %d", e.payload.Outcome.AcceptedAtMs, e.EffectMs)
				}
			}
		case "fill":
			f := e.payload.Fill
			if fillKeys[f.key()] {
				t.Fatalf("fill %s repeats", f.key())
			}
			fillKeys[f.key()] = true
			req := orders[f.ClientOrderID]
			if mustQuoteRat(f.StartPosition).Cmp(position) != 0 {
				t.Fatalf("fill %s starts at %s, the venue position is %s", f.key(), f.StartPosition, quoteRatString(position))
			}
			before := new(big.Rat).Set(position)
			position = quoteRatAdd(position, signedQuoteFillQty(*f))
			if req.ReduceOnly && (before.Sign() == 0 || position.Sign() == -before.Sign()) {
				t.Fatalf("reduce-only fill %s crossed flat (%s to %s)", f.key(), quoteRatString(before), quoteRatString(position))
			}
			if e.payload.Liquidity != "maker" {
				continue
			}
			tr, ok := trades[e.payload.SourceTid]
			if !ok {
				t.Fatalf("maker fill %s names an unknown source trade %d", f.key(), e.payload.SourceTid)
			}
			if tr.RecvMs <= accepted[f.ClientOrderID] || tr.RecvMs != f.TimeMs {
				t.Fatalf("maker fill %s at %d is not strictly after acceptance %d at the trade receipt %d", f.key(), f.TimeMs, accepted[f.ClientOrderID], tr.RecvMs)
			}
			tpx, px := mustQuoteRat(tr.Px), mustQuoteRat(f.Px)
			if px.Cmp(mustQuoteRat(req.Px)) != 0 {
				t.Fatalf("maker fill %s price %s is not the order price %s", f.key(), f.Px, req.Px)
			}
			if (f.Side == "buy" && (tr.Side != "A" || tpx.Cmp(px) >= 0)) || (f.Side == "sell" && (tr.Side != "B" || tpx.Cmp(px) <= 0)) {
				t.Fatalf("maker fill %s has no strict opposing trade-through (trade %s %s, order %s %s)", f.key(), tr.Side, tr.Px, f.Side, f.Px)
			}
			if used[tr.Tid] == nil {
				used[tr.Tid] = quoteRatZero()
			}
			used[tr.Tid] = quoteRatAdd(used[tr.Tid], mustQuoteRat(f.Size))
			limit := quoteRatMul(mustQuoteRat(tr.Sz), mustQuoteRat(spec.Queue.Participation))
			if used[tr.Tid].Cmp(limit) > 0 {
				t.Fatalf("trade %d supplied %s to own orders, above its %s participation bound", tr.Tid, quoteRatString(used[tr.Tid]), quoteRatString(limit))
			}
		}
	}
	settled := quoteRatZero()
	for _, f := range out.Fills {
		if f.SettlementID != 0 {
			settled = quoteRatAdd(settled, signedQuoteFillQty(quoteRawFill{Side: f.Side, Size: f.Size}))
		}
	}
	if out.Book != nil && mustQuoteRat(out.Book.Qty).Cmp(settled) != 0 {
		t.Fatalf("the exact book holds %s, the settled fills sum to %s", out.Book.Qty, quoteRatString(settled))
	}
	recs := quoteReplayDecisionRecords(t, out)
	for i, rec := range recs {
		if !deadlines[rec.DeadlineMs] || (i > 0 && rec.DeadlineMs <= recs[i-1].DeadlineMs) {
			t.Fatalf("decision %d at %d is not an increasing due deadline", i, rec.DeadlineMs)
		}
		for _, k := range []string{"source", "seal", "config", "policy", "portable_config", "run"} {
			if rec.Fingerprints[k] == "" {
				t.Fatalf("decision %d records no %s fingerprint", rec.DeadlineMs, k)
			}
		}
		inv := mustQuoteRat(rec.Inventory)
		age := int64(params.MaxInventoryAgeSeconds) * 1000
		wantExit := inv.Sign() != 0 && rec.FirstOpenMs > 0 && rec.DeadlineMs >= rec.FirstOpenMs+age
		if (rec.Outcome.Form == quotePolicyFormExit) != wantExit {
			t.Fatalf("decision %d form %s, inventory %s opened %d: the age rule says exit=%v", rec.DeadlineMs, rec.Outcome.Form, rec.Inventory, rec.FirstOpenMs, wantExit)
		}
		for _, s := range rec.Sides {
			if s.Status == quoteSubmitResting && s.CancelState == quoteCancelNone && s.AcceptedAtMs > 0 {
				expired := rec.DeadlineMs >= s.AcceptedAtMs+int64(params.QuoteLifetimeSeconds)*1000
				if expired && !strings.HasPrefix(rec.Outcome.Refresh[s.Role], "expired") && rec.Outcome.Form == quotePolicyFormPair {
					t.Fatalf("decision %d keeps an expired %s side accepted at %d", rec.DeadlineMs, s.Role, s.AcceptedAtMs)
				}
				if !expired && strings.HasPrefix(rec.Outcome.Refresh[s.Role], "expired") {
					t.Fatalf("decision %d expired the %s side accepted at %d before its lifetime", rec.DeadlineMs, s.Role, s.AcceptedAtMs)
				}
			}
		}
	}
}

func quoteReplayMustComplete(t *testing.T, out quoteReplayOutput, code int) {
	t.Helper()
	if code != 0 || out.Status != basketReplayStatusComplete {
		t.Fatalf("exit %d status %s refusal %+v error %q stopped %q", code, out.Status, out.Refusal, out.Error, out.Stopped)
	}
}

func TestQuoteReplayRunsThePolicyThroughTheRealCoordinator(t *testing.T) {
	var calls atomic.Int64
	prev := http.DefaultTransport
	http.DefaultTransport = quoteReplayCountingTransport{&calls}
	defer func() { http.DefaultTransport = prev }()
	dir := quoteReplayTempDir(t)
	spec := quoteReplayTestSpec(t, dir, defaultQuoteReplayMarket(), nil)
	out, code := runQuoteReplayForTest(t, spec)
	quoteReplayMustComplete(t, out, code)
	if calls.Load() != 0 || out.LotFetches != 0 {
		t.Fatalf("the replay made %d HTTP requests and %d metadata fetches", calls.Load(), out.LotFetches)
	}
	if !out.Scored || len(out.Decisions) != out.DeadlinesTotal || len(out.Fills) == 0 {
		t.Fatalf("scored %v (%v), %d decisions of %d, %d fills", out.Scored, out.Unscored, len(out.Decisions), out.DeadlinesTotal, len(out.Fills))
	}
	quoteReplayCheckInvariants(t, spec, out)
	recs := quoteReplayDecisionRecords(t, out)
	states := map[int]bool{}
	forms := map[string]bool{}
	kept := 0
	for _, rec := range recs {
		states[mustQuoteRat(rec.Inventory).Sign()] = true
		forms[rec.Outcome.Form] = true
		for _, why := range rec.Outcome.Refresh {
			if strings.HasPrefix(why, "kept") {
				kept++
			}
		}
	}
	if !states[0] || !states[1] || !states[-1] {
		t.Fatalf("the run did not visit flat, long and short inventory: %v", states)
	}
	if !forms[quotePolicyFormPair] || !forms[quotePolicyFormExit] || kept == 0 {
		t.Fatalf("forms %v, kept identities %d", forms, kept)
	}
	waits := 0
	for _, rec := range recs {
		matched := quoteIncreaseHoldMatches(rec.PreviousHolds)
		if len(matched) > 0 && rec.Outcome.Form == quotePolicyFormPair {
			t.Fatalf("decision %d kept the pair form under classified holds %v", rec.DeadlineMs, matched)
		}
		if len(matched) == 0 && !rec.SettlementHeld && rec.Verdict.Status == quoteVerdictHealthy && rec.Outcome.Form == quotePolicyFormReducing {
			t.Fatalf("decision %d chose the reducing form with no classified hold (previous %v)", rec.DeadlineMs, rec.PreviousHolds)
		}
		for _, h := range rec.PreviousHolds {
			if strings.Contains(h, "is not resolved; the replacement waits") && rec.Outcome.Form == quotePolicyFormPair {
				waits++
			}
		}
	}
	if waits == 0 {
		t.Fatal("no generation wait was observed staying in the pair form")
	}
	var funding []quoteReplayTradeRecord
	for _, tr := range out.Trades {
		if tr.TradeType == TradeTypeFunding {
			funding = append(funding, tr)
		}
	}
	if len(funding) != 1 || mustQuoteRat(out.Book.Funding).Sign() == 0 {
		t.Fatalf("funding rows %+v, exact funding %s", funding, out.Book.Funding)
	}
	if out.Timing == nil || out.Timing.Deadlines != out.DeadlinesTotal || out.Timing.MaxNs <= 0 {
		t.Fatalf("timing %+v", out.Timing)
	}
}

type quoteReplayCountingTransport struct{ n *atomic.Int64 }

func (c quoteReplayCountingTransport) RoundTrip(*http.Request) (*http.Response, error) {
	c.n.Add(1)
	return nil, fmt.Errorf("the quote replay must not open a network connection")
}

func TestQuoteReplayResumeMatchesAnUninterruptedRun(t *testing.T) {
	dirA := quoteReplayTempDir(t)
	full, code := runQuoteReplayForTest(t, quoteReplayShortSpec(t, dirA))
	quoteReplayMustComplete(t, full, code)
	want := quoteReplayComparable(t, full)
	dirB := quoteReplayTempDir(t)
	spec := quoteReplayShortSpec(t, dirB)
	part, code := runQuoteReplayForTest(t, spec, "--max-deadlines", "11")
	if code != 0 || part.Status != basketReplayStatusStopped || len(part.Decisions) != 11 {
		t.Fatalf("partial run: exit %d status %s decisions %d", code, part.Status, len(part.Decisions))
	}
	spec.Mode = basketReplayModeResume
	part2, code := runQuoteReplayForTest(t, spec, "--max-deadlines", "13")
	if code != 0 || part2.Status != basketReplayStatusStopped || !part2.Resumed {
		t.Fatalf("second slice: exit %d status %s", code, part2.Status)
	}
	rest, code := runQuoteReplayForTest(t, spec)
	quoteReplayMustComplete(t, rest, code)
	got := quoteReplayComparable(t, rest)
	for k, v := range want {
		if got[k] != v {
			t.Fatalf("resumed %s differs from the uninterrupted run", k)
		}
	}
	if !rest.Scored {
		t.Fatalf("a run resumed at completed checkpoints stays scored: %v", rest.Unscored)
	}
}

func quoteReplayMarketFor(minutes int64) quoteReplayMarket {
	m := defaultQuoteReplayMarket()
	m.endMs = quoteReplayTestStart + (minutes+1)*60_000
	return m
}

func quoteReplayShortSpec(t *testing.T, dir string) *quoteReplaySpec {
	t.Helper()
	return quoteReplayTestSpec(t, dir, quoteReplayMarketFor(3), func(s *quoteReplaySpec) { s.Window.LastDeadlineMs = s.Window.FirstDeadlineMs + 3*60_000 })
}

func quoteReplaySmallSpec(t *testing.T, dir string, mutate func(*quoteReplaySpec)) *quoteReplaySpec {
	t.Helper()
	return quoteReplayTestSpec(t, dir, quoteReplayMarketFor(1), func(s *quoteReplaySpec) {
		s.Window.LastDeadlineMs = s.Window.FirstDeadlineMs + 60_000
		if mutate != nil {
			mutate(s)
		}
	})
}

func quoteReplayFaultCase(t *testing.T, point string, n int) (quoteReplayOutput, quoteReplayOutput, *quoteReplaySpec) {
	t.Helper()
	dir := quoteReplayTempDir(t)
	spec := quoteReplayShortSpec(t, dir)
	crash, code, stderr := runQuoteReplaySubprocess(t, spec, "--fault", fmt.Sprintf("%s:%d", point, n))
	if code != basketReplayExitFault || !strings.Contains(stderr, "fault injection: exiting at "+point) {
		t.Fatalf("%s:%d exited %d (status %s); stderr tail %s", point, n, code, crash.Status, quoteReplayTail(stderr, 600))
	}
	spec.Mode = basketReplayModeResume
	out, code := runQuoteReplayForTest(t, spec)
	quoteReplayMustComplete(t, out, code)
	quoteReplayCheckInvariants(t, spec, out)
	return crash, out, spec
}

func quoteReplayTail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[len(s)-n:]
}

func TestQuoteReplayFaultsRecoverWithTheirStatedClass(t *testing.T) {
	full, code := runQuoteReplayForTest(t, quoteReplayShortSpec(t, quoteReplayTempDir(t)))
	quoteReplayMustComplete(t, full, code)
	want := quoteReplayComparable(t, full)
	convergent := []struct {
		point string
		n     int
	}{
		{"progress-before-write", 11}, {"progress-mid-write", 12}, {"progress-after-write", 13}, {"commit-hook", 14}, {"after-result", 15},
	}
	for _, c := range convergent {
		t.Run("convergent "+c.point, func(t *testing.T) {
			_, out, _ := quoteReplayFaultCase(t, c.point, c.n)
			got := quoteReplayComparable(t, out)
			for k, v := range want {
				if got[k] != v {
					t.Fatalf("%s: resumed %s differs from the uninterrupted run", c.point, k)
				}
			}
			if !out.Scored {
				t.Fatalf("%s: a convergent checkpoint stays scored, got %v", c.point, out.Unscored)
			}
		})
	}
	divergent := []struct {
		point string
		n     int
	}{
		{"after-call", 16}, {"venue-before-persist", 50}, {"venue-after-persist", 52}, {"issue-before-reserve", 18},
	}
	cycle := int64(5_000)
	for _, e := range full.VenueEvents {
		if e.Kind == "fill" && e.EffectMs%cycle > 1_000 {
			divergent = append(divergent, struct {
				point string
				n     int
			}{"venue-after-persist", int(e.Seq)}, struct {
				point string
				n     int
			}{"venue-before-persist", int(e.Seq)})
			break
		}
	}
	classes := map[string]int{}
	for _, c := range divergent {
		t.Run(fmt.Sprintf("%s %d", c.point, c.n), func(t *testing.T) {
			_, out, _ := quoteReplayFaultCase(t, c.point, c.n)
			lost := false
			for _, r := range out.Results {
				if r.State == quoteReplayResultInterrupted {
					lost = true
				}
			}
			if !lost {
				classes["convergent"]++
				got := quoteReplayComparable(t, out)
				for k, v := range want {
					if got[k] != v {
						t.Fatalf("%s: a crash before the deadline progress committed must converge, but %s differs", c.point, k)
					}
				}
				return
			}
			classes["safe-divergent"]++
			if out.Scored {
				t.Fatalf("%s: a run that lost an interrupted result stays unscored", c.point)
			}
			interrupted, recovery := 0, 0
			for _, r := range out.Results {
				switch r.State {
				case quoteReplayResultInterrupted:
					interrupted++
				case quoteReplayResultRecovery:
					recovery++
				}
			}
			if interrupted != 1 || recovery != 1 {
				t.Fatalf("%s: %d interrupted and %d recovery results", c.point, interrupted, recovery)
			}
		})
	}
	t.Run("a repeat that dies before its own progress repeats again", func(t *testing.T) {
		dir := quoteReplayTempDir(t)
		spec := quoteReplayShortSpec(t, dir)
		if _, code, _ := runQuoteReplaySubprocess(t, spec, "--fault", "after-call:16"); code != basketReplayExitFault {
			t.Fatalf("first fault exited %d", code)
		}
		spec.Mode = basketReplayModeResume
		if _, code, _ := runQuoteReplaySubprocess(t, spec, "--fault", "progress-before-write:1"); code != basketReplayExitFault {
			t.Fatalf("second fault exited %d", code)
		}
		out, code := runQuoteReplayForTest(t, spec)
		quoteReplayMustComplete(t, out, code)
		quoteReplayCheckInvariants(t, spec, out)
		states := map[string]int{}
		for _, r := range out.Results {
			if r.DeadlineMs == out.Results[15].DeadlineMs {
				states[r.State]++
			}
		}
		if states[quoteReplayResultInterrupted] != 1 || states[quoteReplayResultRecovery] != 1 || out.Scored {
			t.Fatalf("the interrupted deadline must repeat once more as a recovery: %v scored %v", states, out.Scored)
		}
	})
	if classes["convergent"] == 0 || classes["safe-divergent"] < 3 {
		t.Fatalf("the venue and lifecycle fault points did not cover both restart classes: %v", classes)
	}
}

func TestQuoteReplayRefusesUnsafeStateAndInputs(t *testing.T) {
	t.Run("venue credentials refuse before any state exists", func(t *testing.T) {
		dir := quoteReplayTempDir(t)
		spec := quoteReplaySmallSpec(t, dir, nil)
		path := filepath.Join(dir, "plan.json")
		writeQuoteReplaySpec(t, spec, path)
		t.Setenv("HYPERLIQUID_SECRET_KEY", "0xdeadbeefsecret")
		var buf strings.Builder
		code := runQuoteReplayProcess([]string{"--spec", path}, strings.NewReader(""), &buf)
		if code != basketReplayExitRefused || !strings.Contains(buf.String(), "credentials_refused") || strings.Contains(buf.String(), "deadbeef") {
			t.Fatalf("exit %d output %s", code, buf.String())
		}
		if _, err := os.Stat(spec.Run.StateDir); !os.IsNotExist(err) {
			t.Fatalf("the refused run created state: %v", err)
		}
	})
	t.Run("notifier and token secrets are scrubbed and never printed", func(t *testing.T) {
		dir := quoteReplayTempDir(t)
		spec := quoteReplaySmallSpec(t, dir, func(s *quoteReplaySpec) { s.Window.LastDeadlineMs = s.Window.FirstDeadlineMs + 60_000 })
		path := filepath.Join(dir, "plan.json")
		writeQuoteReplaySpec(t, spec, path)
		t.Setenv("DISCORD_BOT_TOKEN", "discord-sekrit-value")
		t.Setenv("TELEGRAM_BOT_TOKEN", "telegram-sekrit-value")
		t.Setenv("GH_TOKEN", "gh-sekrit-value")
		var buf strings.Builder
		code := runQuoteReplayProcess([]string{"--spec", path}, strings.NewReader(""), &buf)
		if code != 0 || strings.Contains(buf.String(), "sekrit") {
			t.Fatalf("exit %d, output leaks a secret or failed: %s", code, quoteReplayTail(buf.String(), 400))
		}
		for _, k := range []string{"DISCORD_BOT_TOKEN", "TELEGRAM_BOT_TOKEN", "GH_TOKEN"} {
			if v, ok := os.LookupEnv(k); ok {
				t.Fatalf("%s survived the scrub (%d bytes)", k, len(v))
			}
		}
	})
	refuse := func(t *testing.T, spec *quoteReplaySpec, reason string) {
		t.Helper()
		out, code := runQuoteReplayForTest(t, spec)
		if code != basketReplayExitRefused || out.Refusal == nil || out.Refusal.Reason != reason {
			t.Fatalf("want refusal %s, got exit %d status %s refusal %+v error %s", reason, code, out.Status, out.Refusal, out.Error)
		}
	}
	t.Run("deployed and symbolic-link state paths", func(t *testing.T) {
		dir := quoteReplayTempDir(t)
		spec := quoteReplaySmallSpec(t, dir, nil)
		spec.Run.StateDir = "/var/lib/go-trader/quote-replay-run"
		refuse(t, spec, "state_dir_refused")
		real := filepath.Join(dir, "real")
		if err := os.Mkdir(real, 0o700); err != nil {
			t.Fatal(err)
		}
		link := filepath.Join(dir, "link")
		if err := os.Symlink(real, link); err != nil {
			t.Fatal(err)
		}
		spec.Run.StateDir = filepath.Join(link, "run")
		refuse(t, spec, "state_dir_refused")
	})
	t.Run("new mode on an existing path and resume of foreign state", func(t *testing.T) {
		dir := quoteReplayTempDir(t)
		spec := quoteReplaySmallSpec(t, dir, func(s *quoteReplaySpec) { s.Window.LastDeadlineMs = s.Window.FirstDeadlineMs + 60_000 })
		if err := os.Mkdir(spec.Run.StateDir, 0o700); err != nil {
			t.Fatal(err)
		}
		refuse(t, spec, "state_exists")
		spec.Mode = basketReplayModeResume
		refuse(t, spec, "state_foreign")
	})
	t.Run("an unmarked state file in a run directory is foreign", func(t *testing.T) {
		dir := quoteReplayTempDir(t)
		spec := quoteReplaySmallSpec(t, dir, func(s *quoteReplaySpec) { s.Window.LastDeadlineMs = s.Window.FirstDeadlineMs + 60_000 })
		out, code := runQuoteReplayForTest(t, spec)
		quoteReplayMustComplete(t, out, code)
		db, err := OpenStateDB(filepath.Join(spec.Run.StateDir, quoteReplayDBName))
		if err != nil {
			t.Fatal(err)
		}
		for _, q := range []string{"DROP TRIGGER quote_replay_decisions_retained", "DROP TABLE quote_replay_runs"} {
			if _, err := db.db.Exec(q); err != nil {
				t.Fatal(err)
			}
		}
		db.Close()
		spec.Mode = basketReplayModeResume
		refuse(t, spec, "state_foreign")
	})
	t.Run("extra entries, hard links, changed data and a moved directory refuse resume", func(t *testing.T) {
		dir := quoteReplayTempDir(t)
		spec := quoteReplaySmallSpec(t, dir, nil)
		out, code := runQuoteReplayForTest(t, spec, "--max-deadlines", "10")
		if code != 0 || out.Status != basketReplayStatusStopped {
			t.Fatalf("exit %d status %s", code, out.Status)
		}
		spec.Mode = basketReplayModeResume
		extra := filepath.Join(spec.Run.StateDir, "notes.txt")
		if err := os.WriteFile(extra, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		refuse(t, spec, "state_foreign")
		os.Remove(extra)
		link := filepath.Join(dir, "second-name.db")
		if err := os.Link(filepath.Join(spec.Run.StateDir, quoteReplayDBName), link); err != nil {
			t.Fatal(err)
		}
		refuse(t, spec, "state_foreign")
		os.Remove(link)
		changed := quoteReplaySmallSpec(t, dir, nil)
		changed.Mode = basketReplayModeResume
		changed.Trades[5].Sz = "0.6"
		refuse(t, changed, "resume_refused")
		moved := filepath.Join(dir, "moved-run")
		if err := os.Rename(spec.Run.StateDir, moved); err != nil {
			t.Fatal(err)
		}
		spec.Run.StateDir = moved
		refuse(t, spec, "admission_refused")
	})
	t.Run("malformed specifications and parameters refuse before state", func(t *testing.T) {
		dir := quoteReplayTempDir(t)
		cases := map[string]func(*quoteReplaySpec){
			"unknown parameter":         func(s *quoteReplaySpec) { s.Params["edge_boost"] = 1.0 },
			"boolean parameter":         func(s *quoteReplaySpec) { s.Params["risk_aversion"] = true },
			"out of range":              func(s *quoteReplaySpec) { s.Params["risk_aversion"] = 4.5 },
			"integer violation":         func(s *quoteReplaySpec) { s.Params["volatility_window_seconds"] = 60.5 },
			"minimum above maximum":     func(s *quoteReplaySpec) { s.Params["min_spread_bps"] = 50.0; s.Params["max_spread_bps"] = 20.0 },
			"notional above cap/4":      func(s *quoteReplaySpec) { s.Config.MaxInventoryUSD = 300 },
			"lifetime below the cycle":  func(s *quoteReplaySpec) { s.Config.IntervalSeconds = 10; s.Params["quote_lifetime_seconds"] = 5.0 },
			"target above half the cap": func(s *quoteReplaySpec) { s.Params["target_inventory_usd"] = 600.0 },
			"missing parameter":         func(s *quoteReplaySpec) { delete(s.Params, "maker_fee_bps") },
			"crossed book":              func(s *quoteReplaySpec) { s.Books[3].Asks[0][0] = s.Books[3].Bids[0][0] },
			"duplicate trade id":        func(s *quoteReplaySpec) { s.Trades[4].Tid = s.Trades[3].Tid },
			"unbounded window":          func(s *quoteReplaySpec) { s.Window.MaxDeadlines = 10 },
		}
		names := make([]string, 0, len(cases))
		for k := range cases {
			names = append(names, k)
		}
		sort.Strings(names)
		for _, name := range names {
			spec := quoteReplaySmallSpec(t, dir, cases[name])
			spec.Run.StateDir = filepath.Join(dir, strings.ReplaceAll(name, " ", "-"))
			out, code := runQuoteReplayForTest(t, spec)
			if code != basketReplayExitRefused || out.Refusal == nil {
				t.Fatalf("%s: exit %d status %s", name, code, out.Status)
			}
			if _, err := os.Stat(spec.Run.StateDir); !os.IsNotExist(err) {
				t.Fatalf("%s: the refused plan created state", name)
			}
		}
		raw := `{"protocol":"go-trader.quote-replay","version":1,"api_key":"x"}`
		path := filepath.Join(dir, "unknown-field.json")
		if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
			t.Fatal(err)
		}
		var buf strings.Builder
		if code := runQuoteReplayWith([]string{"--spec", path}, strings.NewReader(""), &buf); code != basketReplayExitRefused || !strings.Contains(buf.String(), "spec_malformed") {
			t.Fatalf("unknown field: exit %d %s", code, buf.String())
		}
	})
}

func TestQuoteReplayRunDocumentIsRefusedOutsideTheRunner(t *testing.T) {
	dir := quoteReplayTempDir(t)
	spec := quoteReplaySmallSpec(t, dir, nil)
	if err := validateQuoteReplaySpec(spec); err != nil {
		t.Fatal(err)
	}
	doc, err := spec.configDocument(filepath.Join(dir, "run"))
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "run-document.json")
	if err := os.WriteFile(path, doc, 0o600); err != nil {
		t.Fatal(err)
	}
	setNoEdgeLiveCredentials(t)
	for name, load := range map[string]func(string) (*Config, error){"LoadConfig": LoadConfig, "LoadConfigForProbe": LoadConfigForProbe, "LoadConfigReadOnly": LoadConfigReadOnly} {
		if _, err := load(path); err == nil || !strings.Contains(err.Error(), "production_refused") {
			t.Fatalf("%s accepted the replay run document: %v", name, err)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "run")); !os.IsNotExist(err) {
		t.Fatal("an ordinary loader created replay state")
	}
	stop := 2.0
	sc := StrategyConfig{ID: "imm", Type: "perps", Platform: "hyperliquid", Script: hyperliquidCheckScript, Args: []string{inventoryMarketMakingStrategyName, "BTC", "1m", "--mode=paper"},
		OpenStrategy: StrategyRef{Name: inventoryMarketMakingStrategyName, Params: spec.Params}, StopLossPct: &stop}
	req, err := deriveFeedRequirements(&Config{MarketFeed: marketFeedWebsocket, Strategies: []StrategyConfig{sc}})
	if err != nil {
		t.Fatal(err)
	}
	if need := req.Books[l2BookKeyFor("BTC")]; need.Depth != 20 || need.Capacity != 256 || need.WindowMs != 60_000+1_000+7_500 {
		t.Fatalf("the quote book need %+v", need)
	}
	if err := validateMarketFeedConfig(&Config{MarketFeed: marketFeedREST, Strategies: []StrategyConfig{sc}}); err == nil || !strings.Contains(err.Error(), "cannot supply sealed l2_book observations") {
		t.Fatalf("a REST feed must refuse the quote book need: %v", err)
	}
}

func TestQuoteReplayStateIsRefusedByNormalStartupInspection(t *testing.T) {
	dir := quoteReplayTempDir(t)
	spec := quoteReplaySmallSpec(t, dir, nil)
	_, code, _ := runQuoteReplaySubprocess(t, spec, "--fault", "progress-before-write:1")
	if code != basketReplayExitFault {
		t.Fatalf("the first-deadline fault exited %d", code)
	}
	copyDir := filepath.Join(dir, "copied")
	if err := os.Mkdir(copyDir, 0o700); err != nil {
		t.Fatal(err)
	}
	copied := filepath.Join(copyDir, "state.db")
	for _, suffix := range []string{"", "-wal"} {
		blob, err := os.ReadFile(filepath.Join(spec.Run.StateDir, quoteReplayDBName) + suffix)
		if os.IsNotExist(err) && suffix != "" {
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(copied+suffix, blob, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for _, path := range []string{filepath.Join(spec.Run.StateDir, quoteReplayDBName), copied} {
		cfgPath := writeNoEdgeConfigAt(t, filepath.Dir(path), path)
		cfg, err := LoadConfigReadOnly(cfgPath)
		if err != nil {
			t.Fatal(err)
		}
		layout, err := resolveStorageLayout(cfg)
		if err != nil {
			t.Fatal(err)
		}
		ident, err := buildStorageIdentityMap(cfg, layout)
		if err != nil {
			t.Fatal(err)
		}
		si, err := inspectStorageOwnership(layout, ident, cfg, false)
		if err != nil {
			t.Fatal(err)
		}
		if si.OK() || !strings.Contains(strings.Join(si.Rejections, "\n"), "offline quote replay run") {
			t.Fatalf("storage inspection of %s did not refuse the replay marker: %v", path, si.Rejections)
		}
		if !strings.Contains(formatStorageInspection(si), "OFFLINE QUOTE REPLAY RUN") {
			t.Fatal("the inspection view does not show the replay run")
		}
	}
}

func writeNoEdgeConfigAt(t *testing.T, dir, dbPath string) string {
	t.Helper()
	root := map[string]any{"config_version": CurrentConfigVersion, "interval_seconds": 60, "db_file": dbPath, "log_dir": filepath.Join(dir, "logs"),
		"strategies": []map[string]any{{"id": "hl-sma-btc", "type": "perps", "platform": "hyperliquid", "script": "shared_scripts/check_hyperliquid.py",
			"args": []string{"sma_crossover", "BTC", "1h", "--mode=paper"}, "capital": 1000, "leverage": 1}}}
	blob, err := json.Marshal(root)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, fmt.Sprintf("inspect-%d.json", len(dbPath)))
	if err := os.WriteFile(path, blob, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestQuoteReplayOfflineEgressBranchReadsNoDeployedConfig(t *testing.T) {
	prev := processEgress
	processEgress = &egressRuntime{}
	t.Cleanup(func() { processEgress = prev })
	t.Setenv(egresspolicy.EnvProxy, "http://127.0.0.1:9")
	code, stop := gateSubcommandEgress("quote-replay", []string{"--spec", "/nonexistent/plan.json", "--config", "/nonexistent/config.json"})
	if stop || code != 0 {
		t.Fatalf("the offline branch refused (%d, %v)", code, stop)
	}
	if !processEgress.isActive() {
		t.Fatal("proxy mode stays recorded for the offline command")
	}
	processEgress = &egressRuntime{}
	t.Setenv(egresspolicy.EnvProxy, "")
	if code, stop := gateSubcommandEgress("quote-replay", nil); stop || code != 0 || processEgress.isActive() {
		t.Fatalf("no proxy variable needs no setup (%d, %v)", code, stop)
	}
}

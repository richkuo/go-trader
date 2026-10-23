package main

import (
	"context"
	"encoding/json"
	"math"
	"sort"
	"strings"
	"testing"
)

func quoteReplayRecordAt(t *testing.T, out quoteReplayOutput, d int64) quoteReplayDecisionRecord {
	t.Helper()
	for _, rec := range quoteReplayDecisionRecords(t, out) {
		if rec.DeadlineMs == d {
			return rec
		}
	}
	t.Fatalf("no decision at %d", d)
	return quoteReplayDecisionRecord{}
}

func quoteReplayFlat(base float64) func(int64, float64) float64 {
	return func(int64, float64) float64 { return base }
}

func quoteReplayAddTrades(spec *quoteReplaySpec, extra ...quoteReplayTradeSpec) {
	spec.Trades = append(spec.Trades, extra...)
	sort.SliceStable(spec.Trades, func(i, j int) bool { return spec.Trades[i].RecvMs < spec.Trades[j].RecvMs })
}

func quoteReplayPlaces(t *testing.T, out quoteReplayOutput, role string) []quoteReplayTestEvent {
	t.Helper()
	var outEv []quoteReplayTestEvent
	for _, e := range quoteReplayEvents(t, out) {
		if e.Kind == "place" && e.payload.Request.Role == role {
			outEv = append(outEv, e)
		}
	}
	return outEv
}

func TestQuotePolicyBoundaryCasesThroughTheRealCoordinator(t *testing.T) {
	quiet := func() quoteReplayMarket {
		m := quoteReplayMarketFor(7)
		m.mid = quoteReplayFlat(100000)
		m.sweepEvery = 0
		return m
	}
	t.Run("zero volatility quotes the minimum spread around the midpoint", func(t *testing.T) {
		spec := quoteReplayTestSpec(t, quoteReplayTempDir(t), quiet(), func(s *quoteReplaySpec) {
			s.Window.LastDeadlineMs = s.Window.FirstDeadlineMs + 60_000
			s.Params["quote_lifetime_seconds"] = 120.0
		})
		out, code := runQuoteReplayForTest(t, spec)
		quoteReplayMustComplete(t, out, code)
		quoteReplayCheckInvariants(t, spec, out)
		for _, rec := range quoteReplayDecisionRecords(t, out) {
			v := rec.Outcome.Values
			if v.Variance == nil || *v.Variance != 0 || v.HalfSpread == nil || *v.HalfSpread != 0.0005 {
				t.Fatalf("decision %d: variance %v half-spread %v", rec.DeadlineMs, v.Variance, v.HalfSpread)
			}
			d := rec.Outcome.Desired
			if d.Bid == nil || d.Ask == nil || d.Bid.Px != "99950" || d.Ask.Px != "100050" || d.Bid.Size != "0.001" {
				t.Fatalf("decision %d desired %+v %+v", rec.DeadlineMs, d.Bid, d.Ask)
			}
		}
		if n := len(quoteReplayPlaces(t, out, quoteRoleBid)); n != 1 {
			t.Fatalf("an unchanged quote keeps its identity and queue position; %d bid placements", n)
		}
	})
	t.Run("missing warmup holds increases until the window is continuous", func(t *testing.T) {
		m := quiet()
		m.startMs = quoteReplayTestStart - 30_000
		spec := quoteReplayTestSpec(t, quoteReplayTempDir(t), m, func(s *quoteReplaySpec) { s.Window.LastDeadlineMs = s.Window.FirstDeadlineMs + 60_000 })
		out, code := runQuoteReplayForTest(t, spec)
		quoteReplayMustComplete(t, out, code)
		first := int64(0)
		for _, rec := range quoteReplayDecisionRecords(t, out) {
			held := strings.Contains(strings.Join(rec.Outcome.PolicyHolds, ";"), "volatility warmup has")
			if rec.DeadlineMs < quoteReplayTestStart+30_000 && (!held || rec.Outcome.Desired.Bid != nil || rec.Outcome.Desired.Ask != nil) {
				t.Fatalf("decision %d quoted without warmup: holds %v", rec.DeadlineMs, rec.Outcome.PolicyHolds)
			}
			if !held && first == 0 {
				first = rec.DeadlineMs
			}
		}
		places := quoteReplayPlaces(t, out, quoteRoleBid)
		if first != quoteReplayTestStart+35_000 || len(places) == 0 || places[0].EffectMs < first {
			t.Fatalf("first warm decision %d, first bid effect %v", first, places)
		}
	})
	t.Run("input age holds at one millisecond past the bound and passes at equality", func(t *testing.T) {
		d := quoteReplayTestStart + 30_000
		for _, c := range []struct {
			maxAge float64
			fresh  bool
		}{{2499, false}, {2500, true}, {2501, true}} {
			m := quiet()
			m.dropBooks = [][2]int64{{d - 2499, d}}
			spec := quoteReplayTestSpec(t, quoteReplayTempDir(t), m, func(s *quoteReplaySpec) {
				s.Window.LastDeadlineMs = d
				s.Params["max_input_age_ms"] = c.maxAge
			})
			out, code := runQuoteReplayForTest(t, spec)
			quoteReplayMustComplete(t, out, code)
			rec := quoteReplayRecordAt(t, out, d)
			if (rec.Evidence.Book != nil) != c.fresh || (rec.Mark != nil) != c.fresh || (rec.Verdict.Status == quoteVerdictHealthy) != c.fresh {
				t.Fatalf("max age %v: book %+v mark %+v verdict %s (%s)", c.maxAge, rec.Evidence.Book, rec.Mark, rec.Verdict.Status, rec.Evidence.BookIssue)
			}
			if !c.fresh && rec.Outcome.Form != quotePolicyFormReducing && rec.Outcome.Form != quotePolicyFormNone {
				t.Fatalf("a stale input must not quote increases: form %s", rec.Outcome.Form)
			}
		}
	})
	t.Run("inventory age exits at equality and not one millisecond before", func(t *testing.T) {
		open := quoteReplayTestStart + 20_000
		for _, c := range []struct {
			shift   int64
			exitsAt int64
		}{{0, open + 120_000}, {1, open + 125_000}, {-1, open + 120_000}} {
			spec := quoteReplayTestSpec(t, quoteReplayTempDir(t), quiet(), func(s *quoteReplaySpec) {
				s.Window.LastDeadlineMs = open + 140_000
				quoteReplayAddTrades(s, quoteReplayTradeSpec{RecvMs: open + c.shift, TimeMs: open + c.shift - 10, Tid: 9_000_001, Side: "A", Px: "99800", Sz: "1", Hash: "0xage"})
			})
			out, code := runQuoteReplayForTest(t, spec)
			quoteReplayMustComplete(t, out, code)
			quoteReplayCheckInvariants(t, spec, out)
			firstExit := int64(0)
			for _, rec := range quoteReplayDecisionRecords(t, out) {
				if rec.Outcome.Form == quotePolicyFormExit && firstExit == 0 {
					firstExit = rec.DeadlineMs
					if rec.FirstOpenMs != open+c.shift {
						t.Fatalf("shift %d: inventory opened at %d", c.shift, rec.FirstOpenMs)
					}
				}
			}
			if firstExit != c.exitsAt {
				t.Fatalf("shift %d: first age exit at %d, want %d", c.shift, firstExit, c.exitsAt)
			}
		}
	})
	t.Run("an out-of-cap mark makes the coordinator exit the inventory", func(t *testing.T) {
		m := quiet()
		jump := quoteReplayTestStart + 5*60_000
		m.mid = func(ms int64, _ float64) float64 {
			if ms >= jump {
				return 210000
			}
			return 100000
		}
		spec := quoteReplayTestSpec(t, quoteReplayTempDir(t), m, func(s *quoteReplaySpec) {
			s.Window.LastDeadlineMs = jump + 60_000
			s.Config.MaxInventoryUSD = 400
			s.Params["max_inventory_age_seconds"] = 1800.0
			s.Params["quote_lifetime_seconds"] = 120.0
			var kept []quoteReplayTradeSpec
			for _, tr := range s.Trades {
				if tr.Side == "A" {
					kept = append(kept, tr)
				}
			}
			s.Trades = kept
			for i := int64(0); i < 4; i++ {
				at := quoteReplayTestStart + 20_000 + i*30_000 + 777
				quoteReplayAddTrades(s, quoteReplayTradeSpec{RecvMs: at, TimeMs: at - 5, Tid: uint64(9_100_000 + i), Side: "A", Px: "99800", Sz: "1", Hash: "0xcap"})
			}
		})
		out, code := runQuoteReplayForTest(t, spec)
		quoteReplayMustComplete(t, out, code)
		quoteReplayCheckInvariants(t, spec, out)
		exited := false
		for _, r := range out.Results {
			var res quoteReplayResultRecord
			_ = json.Unmarshal(r.Result, &res)
			for _, h := range res.HoldReasons {
				if strings.Contains(h, "above the $400.00 cap") {
					exited = true
				}
			}
		}
		closes := quoteReplayPlaces(t, out, quoteRoleClose)
		if !exited || len(closes) == 0 || closes[0].EffectMs < jump {
			t.Fatalf("cap exit %v, closes %d", exited, len(closes))
		}
		rec := quoteReplayRecordAt(t, out, jump+5000)
		if !rec.Outcome.Values.AboveCap {
			t.Fatalf("the policy records the cap test: %+v", rec.Outcome.Values)
		}
	})
	t.Run("lot and minimum refusals hold the side", func(t *testing.T) {
		spec := quoteReplayTestSpec(t, quoteReplayTempDir(t), quiet(), func(s *quoteReplaySpec) {
			s.Window.LastDeadlineMs = s.Window.FirstDeadlineMs + 20_000
			s.Params["min_order_value_usd"] = 500.0
		})
		out, code := runQuoteReplayForTest(t, spec)
		quoteReplayMustComplete(t, out, code)
		if len(quoteReplayPlaces(t, out, quoteRoleBid))+len(quoteReplayPlaces(t, out, quoteRoleAsk)) != 0 {
			t.Fatal("a side below the minimum order value was sent")
		}
		if h := strings.Join(quoteReplayRecordAt(t, out, spec.Window.FirstDeadlineMs).Outcome.PolicyHolds, ";"); !strings.Contains(h, "below the 500 minimum order value") {
			t.Fatalf("holds %s", h)
		}
		lot := quoteReplayTestSpec(t, quoteReplayTempDir(t), quiet(), func(s *quoteReplaySpec) {
			s.Window.LastDeadlineMs = s.Window.FirstDeadlineMs + 20_000
			s.Lot.SzDecimals = 2
			s.Params["quote_notional_usd"] = 50.0
			s.Quote.MaxOrderQty = 1
		})
		out, code = runQuoteReplayForTest(t, lot)
		quoteReplayMustComplete(t, out, code)
		if h := strings.Join(quoteReplayRecordAt(t, out, lot.Window.FirstDeadlineMs).Outcome.PolicyHolds, ";"); !strings.Contains(h, "floors to zero") {
			t.Fatalf("lot holds %s", h)
		}
		venue := quoteReplayTestSpec(t, quoteReplayTempDir(t), quiet(), func(s *quoteReplaySpec) {
			s.Window.LastDeadlineMs = s.Window.FirstDeadlineMs + 20_000
			s.Costs.MinOrderValueUSD = "500"
		})
		out, code = runQuoteReplayForTest(t, venue)
		quoteReplayMustComplete(t, out, code)
		asks := quoteReplayPlaces(t, out, quoteRoleAsk)
		if len(asks) == 0 || asks[0].payload.Outcome.Status != quoteOutcomeRejected || !strings.Contains(asks[0].payload.Outcome.Detail, "below the 500 minimum") {
			t.Fatalf("venue minimum: %+v", asks)
		}
		if len(quoteReplayPlaces(t, out, quoteRoleBid)) != 0 {
			t.Fatal("the other side of a refused pair is never sent")
		}
	})
	t.Run("short inventory mirrors long", func(t *testing.T) {
		m := quiet()
		m.mid = func(ms int64, _ float64) float64 { return 99990 + float64((ms/1000)%2)*2 }
		spec := quoteReplayTestSpec(t, quoteReplayTempDir(t), m, func(s *quoteReplaySpec) {
			s.Window.LastDeadlineMs = s.Window.FirstDeadlineMs + 200_000
			quoteReplayAddTrades(s, quoteReplayTradeSpec{RecvMs: quoteReplayTestStart + 20_000, TimeMs: quoteReplayTestStart + 19_990, Tid: 9_200_001, Side: "B", Px: "100200", Sz: "1", Hash: "0xshort"})
		})
		out, code := runQuoteReplayForTest(t, spec)
		quoteReplayMustComplete(t, out, code)
		quoteReplayCheckInvariants(t, spec, out)
		rec := quoteReplayRecordAt(t, out, quoteReplayTestStart+25_000)
		if mustQuoteRat(rec.Inventory).Sign() >= 0 || rec.Outcome.Desired.Bid == nil || !rec.Outcome.Desired.Bid.ReduceOnly || rec.Outcome.Desired.Ask == nil || rec.Outcome.Desired.Ask.ReduceOnly {
			t.Fatalf("short state: inventory %s desired bid %+v ask %+v", rec.Inventory, rec.Outcome.Desired.Bid, rec.Outcome.Desired.Ask)
		}
		if *rec.Outcome.Values.Skew >= 0 || *rec.Outcome.Values.NormalizedInventory >= 0 {
			t.Fatalf("short inventory skews the reservation up: %+v", rec.Outcome.Values)
		}
		var exitSide string
		for _, e := range quoteReplayPlaces(t, out, quoteRoleClose) {
			exitSide = e.payload.Request.Side
		}
		if exitSide != "buy" {
			t.Fatalf("the short age exit buys, got %q", exitSide)
		}
	})
	t.Run("a half-spread above half the maximum holds increases", func(t *testing.T) {
		m := quoteReplayMarketFor(1)
		m.sweepEvery = 0
		m.mid = func(ms int64, base float64) float64 {
			if (ms/1000)%2 == 0 {
				return base * 1.004
			}
			return base * 0.996
		}
		spec := quoteReplayTestSpec(t, quoteReplayTempDir(t), m, func(s *quoteReplaySpec) {
			s.Window.LastDeadlineMs = s.Window.FirstDeadlineMs + 20_000
			s.Params["volatility_multiplier"] = 4.0
		})
		out, code := runQuoteReplayForTest(t, spec)
		quoteReplayMustComplete(t, out, code)
		for _, rec := range quoteReplayDecisionRecords(t, out) {
			if !strings.Contains(strings.Join(rec.Outcome.PolicyHolds, ";"), "is above half the") || rec.Outcome.Desired.Bid != nil || rec.Outcome.Desired.Ask != nil {
				t.Fatalf("decision %d: holds %v desired %+v", rec.DeadlineMs, rec.Outcome.PolicyHolds, rec.Outcome.Desired)
			}
			if h := *rec.Outcome.Values.HalfSpread; !(h > 0.005) || math.IsNaN(h) {
				t.Fatalf("half-spread %v", h)
			}
		}
	})
}

func TestQuoteReplayLaterReceiptsLeaveEarlierDecisionsUnchanged(t *testing.T) {
	cut := quoteReplayTestStart + 4*60_000
	short := func(s *quoteReplaySpec) { s.Window.LastDeadlineMs = s.Window.FirstDeadlineMs + 7*60_000 }
	base := quoteReplayTestSpec(t, quoteReplayTempDir(t), quoteReplayMarketFor(7), short)
	other := quoteReplayTestSpec(t, quoteReplayTempDir(t), quoteReplayMarketFor(7), func(s *quoteReplaySpec) {
		short(s)
		for i := range s.Trades {
			if s.Trades[i].RecvMs > cut {
				s.Trades[i].Sz = "2.5"
				if s.Trades[i].Side == "A" {
					s.Trades[i].Side = "B"
				} else {
					s.Trades[i].Side = "A"
				}
			}
		}
		for i := range s.Books {
			if s.Books[i].RecvMs > cut {
				s.Books[i].Bids = s.Books[i].Bids[:3]
			}
		}
	})
	a, code := runQuoteReplayForTest(t, base)
	quoteReplayMustComplete(t, a, code)
	b, code := runQuoteReplayForTest(t, other)
	quoteReplayMustComplete(t, b, code)
	strip := func(rec quoteReplayDecisionRecord) string {
		for i := range rec.Sides {
			rec.Sides[i].ClientID, rec.Sides[i].Request.ClientOrderID = "", ""
		}
		if rec.Stop != nil {
			rec.Stop.ClientID = ""
		}
		delete(rec.Fingerprints, "config")
		delete(rec.Fingerprints, "run")
		delete(rec.Fingerprints, "data")
		delete(rec.Fingerprints, "source")
		rec.Verdict.ConfigFingerprint, rec.Verdict.Source = "", ""
		blob, _ := json.Marshal(rec)
		return string(blob)
	}
	ra, rb := quoteReplayDecisionRecords(t, a), quoteReplayDecisionRecords(t, b)
	compared, differed := 0, false
	for i := range ra {
		if ra[i].DeadlineMs <= cut {
			compared++
			if strip(ra[i]) != strip(rb[i]) {
				t.Fatalf("decision %d changed because of receipts after %d", ra[i].DeadlineMs, cut)
			}
		} else if strip(ra[i]) != strip(rb[i]) {
			differed = true
		}
	}
	if compared < 45 || !differed {
		t.Fatalf("compared %d decisions; later decisions differed: %v", compared, differed)
	}
}

func TestQuoteReplayCoverageAndTimingRefusals(t *testing.T) {
	t.Run("a raw-trade gap while an order rests ends the window unscored", func(t *testing.T) {
		m := quoteReplayMarketFor(5)
		m.tradeGaps = [][2]int64{{quoteReplayTestStart + 3*60_000 + 100, quoteReplayTestStart + 3*60_000 + 40_000}}
		spec := quoteReplayTestSpec(t, quoteReplayTempDir(t), m, func(s *quoteReplaySpec) { s.Window.LastDeadlineMs = s.Window.FirstDeadlineMs + 5*60_000 })
		out, code := runQuoteReplayForTest(t, spec)
		if code != 0 || out.Status != basketReplayStatusStopped || out.Scored || !strings.Contains(out.Stopped, "raw-trade coverage broke") {
			t.Fatalf("exit %d status %s scored %v stopped %q", code, out.Status, out.Scored, out.Stopped)
		}
		quoteReplayCheckInvariants(t, spec, out)
		spec.Mode = basketReplayModeResume
		again, code := runQuoteReplayForTest(t, spec)
		if code != 0 || again.Status != basketReplayStatusStopped || again.Scored || again.CursorMs != out.CursorMs || len(again.Decisions) != len(out.Decisions) {
			t.Fatalf("a resume after the window ended must stop at the same cursor: exit %d status %s cursor %d/%d decisions %d/%d refusal %+v",
				code, again.Status, again.CursorMs, out.CursorMs, len(again.Decisions), len(out.Decisions), again.Refusal)
		}
	})
	t.Run("a book-only window with no raw trades is unscored", func(t *testing.T) {
		m := quoteReplayMarketFor(1)
		m.tradeGaps = [][2]int64{{m.startMs, m.endMs}}
		spec := quoteReplayTestSpec(t, quoteReplayTempDir(t), m, func(s *quoteReplaySpec) {
			s.Window.LastDeadlineMs = s.Window.FirstDeadlineMs + 60_000
			s.TradeCov = []quoteReplayRangeSpec{}
		})
		first, code := runQuoteReplayForTest(t, spec, "--max-deadlines", "3")
		if code != 0 || first.Status != basketReplayStatusStopped {
			t.Fatalf("exit %d status %s", code, first.Status)
		}
		spec.Mode = basketReplayModeResume
		out, code := runQuoteReplayForTest(t, spec)
		if code != 0 || out.Scored || out.Status != basketReplayStatusComplete {
			t.Fatalf("a resumed book-only run stays unscored: exit %d status %s scored %v", code, out.Status, out.Scored)
		}
		for _, rec := range quoteReplayDecisionRecords(t, out) {
			if rec.Verdict.Status != quoteVerdictUnhealthy || rec.Outcome.Desired.Bid != nil {
				t.Fatalf("decision %d quoted without raw-trade queue evidence", rec.DeadlineMs)
			}
		}
	})
	t.Run("an effect at the next deadline refuses the run", func(t *testing.T) {
		spec := quoteReplayTestSpec(t, quoteReplayTempDir(t), quoteReplayMarketFor(1), func(s *quoteReplaySpec) {
			s.Window.LastDeadlineMs = s.Window.FirstDeadlineMs + 60_000
			s.Latency = quoteReplayLatencySpec{PlaceMs: 2500, CancelMs: 2500}
			s.Quote.RequestExpiryMs = 6000
		})
		out, code := runQuoteReplayForTest(t, spec)
		if code != basketReplayExitRefused || out.Refusal == nil || out.Refusal.Reason != "timing_refused" {
			t.Fatalf("exit %d refusal %+v", code, out.Refusal)
		}
		spec.Mode = basketReplayModeResume
		again, code := runQuoteReplayForTest(t, spec)
		if code != basketReplayExitRefused || again.Refusal == nil || !strings.Contains(strings.Join(again.Refusal.Detail, " "), "refused terminally") {
			t.Fatalf("a resume after a timing refusal must stay refused: exit %d refusal %+v", code, again.Refusal)
		}
	})
	t.Run("a callback fault that settles without progress refuses resume", func(t *testing.T) {
		spec := quoteReplayTestSpec(t, quoteReplayTempDir(t), quoteReplayMarketFor(2), func(s *quoteReplaySpec) { s.Window.LastDeadlineMs = s.Window.FirstDeadlineMs + 2*60_000 })
		out, code := runQuoteReplayForTest(t, spec, "--fault", "decide-error:12")
		if code == 0 || out.Status != basketReplayStatusError {
			t.Fatalf("exit %d status %s", code, out.Status)
		}
		spec.Mode = basketReplayModeResume
		out, code = runQuoteReplayForTest(t, spec)
		if code != basketReplayExitRefused || out.Refusal == nil || !strings.Contains(strings.Join(out.Refusal.Detail, " "), "moved ahead of its audited progress") {
			t.Fatalf("resume after a callback fault: exit %d refusal %+v", code, out.Refusal)
		}
	})
}

func TestQuotePolicyHoldClassifierThroughTheCoordinator(t *testing.T) {
	e := newQuoteTestEnv(t, quoteTestOptions{})
	params, err := parseQuotePolicyParams(map[string]interface{}{"volatility_window_seconds": 60.0, "quote_lifetime_seconds": 120.0})
	if err != nil {
		t.Fatal(err)
	}
	ctx := quotePolicyContext{Params: params, Coin: "BTC", CapUSD: 5000, MaxOrderQty: 0.5, CycleSeconds: 60, Prices: e.adm.prices, PolicyFingerprint: e.adm.PolicyFingerprint()}
	run := func(n int) (quotePolicyOutcome, quoteDeadlineResult) {
		d := e.deadlineMs(n)
		e.clock.Set(timeFromMs(d))
		ev := quotePolicyEvidence{DeadlineMs: d, Healthy: true, Book: &quotePolicyBook{EventMs: d, RecvMs: d, BestBid: "99995", BestAsk: "100005"}}
		for i := 0; i <= 60; i++ {
			ev.Samples = append(ev.Samples, quotePolicySample{SecondMs: d - int64(60-i)*1000, Mid: "100000"})
		}
		var out quotePolicyOutcome
		res, err := RunDeadline(context.Background(), e.adm, quoteDeadlineRequest{DeadlineMs: d, Marks: e.marks(d, 100000), Verdict: e.verdict(d),
			Decide: func(in quoteDecisionInput) (quoteDecision, error) {
				out = decideQuotePolicy(ctx, ev, in)
				dec := out.Decision
				dec.Progress = &quoteTestProgress{env: e}
				return dec, nil
			}})
		if err != nil {
			t.Fatalf("deadline %d: %v", n, err)
		}
		return out, res
	}
	out, res := run(1)
	if out.Form != quotePolicyFormPair || out.Desired.Bid == nil || len(res.HoldReasons) != 0 {
		t.Fatalf("a clean deadline quotes a pair: %s %v", out.Form, res.HoldReasons)
	}
	if err := e.adm.OperatorHold(true, "maintenance"); err != nil {
		t.Fatal(err)
	}
	out, res = run(2)
	if out.Form != quotePolicyFormReducing || !strings.Contains(strings.Join(out.PredictedHold, ";"), "operator hold: maintenance") {
		t.Fatalf("a visible operator hold selects the reducing form: %s %v (coordinator %v)", out.Form, out.PredictedHold, res.HoldReasons)
	}
	if err := e.adm.OperatorHold(false, ""); err != nil {
		t.Fatal(err)
	}
	out, _ = run(3)
	if out.Form != quotePolicyFormReducing {
		t.Fatalf("the previous deadline's operator hold still classifies one deadline after clearance: %s %v", out.Form, out.PredictedHold)
	}
	out, _ = run(4)
	if out.Form != quotePolicyFormPair {
		t.Fatalf("after clearance the pair returns: %s %v", out.Form, out.PredictedHold)
	}
	e.adm.mu.Lock()
	e.adm.state.Strategies[quoteTestOwner].Quote.LastHolds = []string{"generation 4 is not resolved; the replacement waits for terminal proof and settlement",
		"the inventory extreme 0.01 would be $1000.00 marked, above the $500.00 cap"}
	e.adm.mu.Unlock()
	out, _ = run(5)
	if out.Form == quotePolicyFormReducing || len(out.PredictedHold) != 0 {
		t.Fatalf("generation waits and envelope refusals alone never select the reducing form: %s %v", out.Form, out.PredictedHold)
	}
}

package main

import (
	"context"
	"strings"
	"testing"
	"time"
)

func (e *quoteTestEnv) runWithFunding(n int, mark float64, desired quoteDesiredPair, records []feedFundingRecord) quoteDeadlineResult {
	e.t.Helper()
	d := e.deadlineMs(n)
	if e.clock.Now().UnixMilli() < d {
		e.clock.Set(time.UnixMilli(d))
	}
	var in *paperFundingInputs
	if records != nil {
		var kept []feedFundingRecord
		for _, r := range records {
			if r.TimeMs <= d {
				kept = append(kept, r)
			}
		}
		cov := feedFundingCoverage{Coin: "BTC", FromMs: quoteTestStart.UnixMilli()}
		if len(kept) > 0 {
			cov.Records, cov.ToMs = kept, kept[len(kept)-1].TimeMs
		}
		in = &paperFundingInputs{Source: paperFundingSourceSealed, Coverage: map[string]feedFundingCoverage{"BTC": cov}, Detail: map[string]string{}, NotServed: map[string]bool{}}
	}
	res, err := RunDeadline(context.Background(), e.adm, quoteDeadlineRequest{DeadlineMs: d, Marks: e.marks(d, mark), Funding: in, Verdict: e.verdict(d),
		Decide: func(quoteDecisionInput) (quoteDecision, error) {
			return quoteDecision{Desired: desired, Progress: &quoteTestProgress{env: e}}, nil
		}})
	if err != nil {
		e.t.Fatalf("deadline %d: %v", n, err)
	}
	return res
}

func TestQuotePaperFundingSettlesOnlyThroughTheQuoteSettlement(t *testing.T) {
	recordAt := quoteTestStart.Add(30 * time.Minute).UnixMilli()
	laterAt := quoteTestStart.Add(90 * time.Minute).UnixMilli()
	records := []feedFundingRecord{{TimeMs: recordAt, Rate: 0.0001}, {TimeMs: laterAt, Rate: 0.0002}}
	t.Run("a funding-only settlement books each covered record once with the progress record", func(t *testing.T) {
		e := newQuoteTestEnv(t, quoteTestOptions{})
		pair := e.pair("99", "", "0.2")
		e.runWithFunding(1, 100, pair, records)
		e.venue.fill(t, e.sides(1)[quoteRoleBid].ClientOrderID, "0.1", "99", "0.01", e.deadlineMs(1)+10_000)
		e.runWithFunding(2, 100, pair, records)
		res := e.runWithFunding(31, 100, pair, records)
		if res.SettlementKind != quoteSettleFunding {
			t.Fatalf("a deadline with funding progress and no fill commits a funding-only settlement: %+v", res)
		}
		if n := e.count(`SELECT COUNT(*) FROM trades WHERE trade_type = 'funding'`); n != 1 {
			t.Fatalf("funding rows %d, want 1", n)
		}
		c := e.contract()
		ratEq(t, "funding", c.Book.Funding, "-0.001")
		ratEq(t, "cash", c.Book.Cash, "999.989")
		if n := e.count(`SELECT COUNT(*) FROM quote_test_progress WHERE deadline_ms = ? AND kind = 'funding'`, e.deadlineMs(31)); n != 1 {
			t.Fatalf("the progress record must commit inside the funding-only settlement")
		}
		e.runWithFunding(85, 100, pair, records)
		if n := e.count(`SELECT COUNT(*) FROM trades WHERE trade_type = 'funding'`); n != 1 {
			t.Fatalf("a record past the deadline and coverage stays pending: %d funding rows", n)
		}
		e.runWithFunding(91, 100, pair, records)
		e.runWithFunding(92, 100, pair, records)
		if n := e.count(`SELECT COUNT(*) FROM trades WHERE trade_type = 'funding'`); n != 2 {
			t.Fatalf("the later record books once inside its coverage: %d", n)
		}
		ratEq(t, "funding after both records", e.contract().Book.Funding, "-0.003")
		if s := e.owner(); s.Quote == nil || s.PaperFunding == nil {
			t.Fatal("the owner keeps its quote-owned funding state")
		}
		planned := planPaperFunding(e.adm.state, e.cfg.Strategies)
		if _, ok := planned["BTC"]; ok {
			t.Fatal("the generic funding planner must skip the quote owner")
		}
		before := e.contract()
		runPaperFundingAccounting(e.adm.state, e.cfg.Strategies, nil, paperFundingInputs{}, map[string]float64{"BTC": 100}, nil, e.clock.Now())
		if lines := syncPaperFundingEligibility(e.adm.state, e.cfg.Strategies, e.clock.Now()); len(lines) != 0 {
			t.Fatalf("eligibility sync must not touch a quote owner: %v", lines)
		}
		e.adm.mu.Lock()
		err := e.store.SaveStrategyBook(e.adm.state.Strategies[quoteTestOwner])
		e.adm.mu.Unlock()
		if err != nil || e.contract().Book != before.Book {
			t.Fatalf("the generic funding loop changed the quote book: %v", err)
		}
	})
	t.Run("a late-recovered earlier fill gives the correct funding payment once", func(t *testing.T) {
		e := newQuoteTestEnv(t, quoteTestOptions{})
		pair := e.pair("99", "", "0.2")
		e.runWithFunding(1, 100, pair, records)
		e.venue.fill(t, e.sides(1)[quoteRoleBid].ClientOrderID, "0.1", "99", "0.01", e.deadlineMs(1)+10_000)
		e.venue.recoverHook = func(since, until int64) (quoteFillRecovery, error, bool) {
			return quoteFillRecovery{Coverage: quoteFillCoverage{Complete: false, FromMs: since, Reason: "history lag"}}, nil, true
		}
		e.runWithFunding(2, 100, pair, records)
		e.runWithFunding(31, 100, pair, records)
		if n := e.count(`SELECT COUNT(*) FROM trades WHERE trade_type = 'funding'`); n != 0 {
			t.Fatalf("funding past the applied coverage boundary must stay pending: %d", n)
		}
		e.venue.recoverHook = nil
		e.runWithFunding(32, 100, pair, records)
		if n := e.count(`SELECT COUNT(*) FROM trades WHERE trade_type = 'funding'`); n != 1 {
			t.Fatalf("funding rows %d, want 1", n)
		}
		ratEq(t, "funding with the recovered inventory", e.contract().Book.Funding, "-0.001")
		e.runWithFunding(33, 100, pair, records)
		ratEq(t, "funding after a repeat", e.contract().Book.Funding, "-0.001")
	})
}

func TestQuoteOwnerPositionAndFundingShareTheQuoteCoin(t *testing.T) {
	useHLLotMetadataForTest(t, map[string]int{"BTC": 3, "ETH": 4})
	dir := t.TempDir()
	for name, symbol := range map[string]string{"another coin": "ETH", "the quote coin in lower case": "btc"} {
		blob := quoteTestConfigBytes(t, dir, quoteTestOptions{strategy: map[string]any{"symbol": symbol}})
		l, err := newQuoteFixtureLoadAdmission(quoteTestOwner, dir, blob)
		if err != nil {
			t.Fatal(err)
		}
		if cfg, err := loadQuoteAdmittedConfigData(blob, l); err == nil || cfg != nil || !strings.Contains(err.Error(), "exactly the quote coin") {
			t.Fatalf("symbol %q (%s) must be refused next to quote coin BTC: %v", symbol, name, err)
		}
	}
	recordAt := quoteTestStart.Add(30 * time.Minute).UnixMilli()
	records := []feedFundingRecord{{TimeMs: recordAt, Rate: 0.0001}}
	for name, symbol := range map[string]string{"an empty symbol": "", "a symbol equal to the quote coin": "BTC"} {
		t.Run(name, func(t *testing.T) {
			opts := quoteTestOptions{}
			if symbol != "" {
				opts.strategy = map[string]any{"symbol": symbol}
			}
			e := newQuoteTestEnv(t, opts)
			pair := e.pair("99", "", "0.2")
			e.runWithFunding(1, 100, pair, records)
			e.venue.fill(t, e.sides(1)[quoteRoleBid].ClientOrderID, "0.1", "99", "0.01", e.deadlineMs(1)+10_000)
			e.runWithFunding(2, 100, pair, records)
			e.runWithFunding(31, 100, pair, records)
			ratEq(t, "funding on held inventory", e.contract().Book.Funding, "-0.001")
			s := e.owner()
			if len(s.Positions) != 1 || s.Positions["BTC"] == nil {
				t.Fatalf("the position must be keyed by the quote coin: %+v", s.Positions)
			}
		})
	}
}

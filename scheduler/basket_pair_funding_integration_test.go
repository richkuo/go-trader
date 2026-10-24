package main

import (
	"context"
	"encoding/json"
	"math"
	"math/big"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
)

const pairFundingFarMs = 4_102_444_800_000

func pairFundingTestKeys(doc []byte) map[string]pairEvidenceKey {
	out := map[string]pairEvidenceKey{}
	for _, k := range pairTestMemberKeys(doc) {
		out[k.MemberID] = k
	}
	return out
}

func pfRate(k pairEvidenceKey, rule string, eventMs int64, rate string) pairFundingRate {
	return pairFundingRate{V: pairFundingRecordVersion, Key: k, EventID: pairFundingEventID(k.Provider, k.Instrument, eventMs), RuleVersion: rule,
		EventMs: eventMs, VisibleMs: eventMs + 1000, Rate: pairDecimal(rate), Provenance: "research-funding:fixture-rates"}
}

func pfPrice(k pairEvidenceKey, kind string, atMs int64, px string) pairFundingPrice {
	return pairFundingPrice{V: pairFundingRecordVersion, Key: k, EvidenceID: kind + "-" + k.MemberID + "-" + time.UnixMilli(atMs).UTC().Format("150405.000"), PriceKind: kind,
		AtMs: atMs, VisibleMs: atMs + 1000, Price: pairDecimal(px), Provenance: "research-funding:fixture-prices"}
}

func pairFundingAutoCoverage(d *pairFundingDocument, keys map[string]pairEvidenceKey, skip ...string) {
	skipped := map[string]bool{}
	for _, id := range skip {
		skipped[id] = true
	}
	d.Coverage = []pairFundingCoverageRecord{}
	ids := make([]string, 0, len(keys))
	for id := range keys {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		if skipped[id] {
			continue
		}
		k := keys[id]
		var times []pairFundingRate
		for _, r := range d.Rates {
			if r.Key == k {
				times = append(times, r)
			}
		}
		sort.Slice(times, func(i, j int) bool { return times[i].EventMs < times[j].EventMs })
		prev, visible := int64(pairTestFundingFromMs), int64(pairTestFundingFromMs+500)
		for i, r := range times {
			if r.EventMs <= prev {
				continue
			}
			d.Coverage = append(d.Coverage, pairFundingCoverageRecord{V: pairFundingRecordVersion, Key: k, EvidenceID: "cov-" + id + "-" + string(rune('a'+i)),
				RequestedFromMs: prev, RequestedThroughMs: r.EventMs, Proven: []pairFundingInterval{{FromMs: prev, ThroughMs: r.EventMs}}, Gaps: []pairCoverageGap{},
				VisibleMs: r.VisibleMs, Provenance: "research-funding:fixture-coverage"})
			prev, visible = r.EventMs, r.VisibleMs
		}
		d.Coverage = append(d.Coverage, pairFundingCoverageRecord{V: pairFundingRecordVersion, Key: k, EvidenceID: "cov-" + id + "-tail",
			RequestedFromMs: prev, RequestedThroughMs: pairFundingFarMs, Proven: []pairFundingInterval{{FromMs: prev, ThroughMs: pairFundingFarMs}}, Gaps: []pairCoverageGap{},
			VisibleMs: visible, Provenance: "research-funding:fixture-coverage"})
	}
}

func pairFundingConversions(usdc, usdt string) func(d *pairEvidenceDocument) {
	return func(d *pairEvidenceDocument) {
		for i := range d.Conversions {
			switch d.Conversions[i].Currency {
			case "USDC":
				d.Conversions[i].RateUSD = pairDecimal(usdc)
			case "USDT":
				d.Conversions[i].RateUSD = pairDecimal(usdt)
			}
		}
	}
}

func newPairFundingEnv(t *testing.T, fund func(d *pairFundingDocument, k map[string]pairEvidenceKey), ev func(d *pairEvidenceDocument)) *pairTestEnv {
	t.Helper()
	return newPairTestEnvDeps(t, pairSetLimit("max_hold_age_ms", 86400000), func(doc []byte, venues *pairTestVenues) pairResearchDeps {
		keys := pairFundingTestKeys(doc)
		return pairResearchDeps{VenueFor: venues.factory(), Evidence: pairTestEvidence(t, doc, ev), Quotes: newPairFixtureQuotes(t, doc).collector,
			Funding: pairTestFunding(t, doc, func(d *pairFundingDocument) {
				if fund != nil {
					fund(d, keys)
				}
			})}
	})
}

func (e *pairTestEnv) openMatched(hlQty float64) {
	e.t.Helper()
	pairEnterHoldRunner(e, hlQty)
	e.slotStep(0)
	e.slotStep(1)
	e.assertBooks("matched", hlQty, -hlQty)
}

func (e *pairTestEnv) fundingTrades(id string) []Trade {
	var out []Trade
	for _, tr := range e.state.Strategies[id].TradeHistory {
		if tr.TradeType == TradeTypeFunding {
			out = append(out, tr)
		}
	}
	return out
}

func (e *pairTestEnv) fundingRow(member, eventID string) (status, reason, raw string) {
	e.t.Helper()
	err := e.db().db.QueryRow(`SELECT status, reason, event_json FROM pair_funding_events WHERE strategy_id = ? AND event_id = ?`, member, eventID).Scan(&status, &reason, &raw)
	if err != nil {
		return "", "", ""
	}
	return status, reason, raw
}

func (e *pairTestEnv) cash(id string) float64 {
	return e.state.Strategies[id].Cash
}

func (e *pairTestEnv) savedCash(id string) float64 {
	e.t.Helper()
	var c float64
	if err := e.db().db.QueryRow(`SELECT cash FROM strategies WHERE id = ?`, id).Scan(&c); err != nil {
		e.t.Fatalf("read saved cash: %v", err)
	}
	return c
}

func pairDecimalFloat(t *testing.T, d string) float64 {
	t.Helper()
	r, ok := new(big.Rat).SetString(d)
	if !ok {
		t.Fatalf("bad decimal %q", d)
	}
	f, _ := r.Float64()
	return f
}

func (e *pairTestEnv) hasObligation(kind, member string) bool {
	for _, o := range e.rt().Pair.Progress.Obligations {
		if o.Kind == kind && o.MemberID == member {
			return true
		}
	}
	return false
}

func TestBasketPairFundingSettlesEachVenueOnceFromCompleteEvidence(t *testing.T) {
	t0 := pairTestStart()
	tEarly := t0.Add(30 * time.Second).UnixMilli()
	tHL := t0.Add(time.Hour).UnixMilli()
	tOKX := t0.Add(4 * time.Hour).UnixMilli()
	e := newPairFundingEnv(t, func(d *pairFundingDocument, k map[string]pairEvidenceKey) {
		hl, okx := k[pairTestHL], k[pairTestOKX]
		d.Rates = []pairFundingRate{
			pfRate(hl, pairFundingRuleHyperliquid, tEarly, "0.0003"),
			pfRate(okx, pairFundingRuleOKX, tEarly, "0.00005"),
			pfRate(hl, pairFundingRuleHyperliquid, tHL, "0.0001"),
			pfRate(okx, pairFundingRuleOKX, tOKX, "-0.0002"),
		}
		d.Prices = []pairFundingPrice{
			pfPrice(hl, pairFundingPriceOracle, tEarly, "60005"),
			pfPrice(okx, pairFundingPriceMark, tEarly, "60050"),
			pfPrice(hl, pairFundingPriceOracle, tHL, "60010"),
			pfPrice(okx, pairFundingPriceMark, tOKX, "60100"),
			pfPrice(okx, pairFundingPriceOracle, tOKX, "1"),
		}
		pairFundingAutoCoverage(d, k)
	}, pairFundingConversions("1.0002", "0.9995"))
	e.openMatched(0.05)
	risk := func(id string) (float64, int) {
		s := e.state.Strategies[id]
		return s.RiskState.DailyPnL, s.RiskState.ConsecutiveLosses
	}
	hlDaily, hlLosses := risk(pairTestHL)
	okxDaily, okxLosses := risk(pairTestOKX)
	if len(e.fundingTrades(pairTestHL)) != 0 {
		t.Fatal("the hyperliquid member was flat at the early settlement, so it owes nothing")
	}

	check := func(label, member, eventID string, at time.Time, wantNative, wantUSD string, wantQty string) {
		t.Helper()
		before := e.cash(member)
		n := len(e.fundingTrades(member))
		e.step(at)
		if got := len(e.fundingTrades(member)); got != n+1 {
			t.Fatalf("%s: %d funding rows, want %d", label, got, n+1)
		}
		amount := pairDecimalFloat(t, wantUSD)
		if d := e.cash(member) - before; math.Abs(d-amount) > 1e-9 {
			t.Fatalf("%s: cash moved by %v, want %v", label, d, amount)
		}
		tr := e.fundingTrades(member)[n]
		policy := activeFillCostPolicy()
		if tr.Side != "funding" || tr.RealizedPnL != amount || tr.ExchangeOrderID == "" || !strings.HasPrefix(tr.ExchangeOrderID, pairFundingOrderIDPrefix) ||
			!strings.HasSuffix(tr.ExchangeOrderID, eventID) || tr.CostModelVersion != policy.ModelVersion || tr.CostPolicyID != policy.PolicyID {
			t.Fatalf("%s: the funding row is wrong: %+v", label, tr)
		}
		status, _, raw := e.fundingRow(member, eventID)
		if status != pairFundingStatusApplied {
			t.Fatalf("%s: the saved event is %q, want applied", label, status)
		}
		ev, err := pairDecodeFundingEvent([]byte(raw))
		if err != nil {
			t.Fatalf("%s: the saved event bytes must decode strictly: %v", label, err)
		}
		if string(ev.NativeAmount) != wantNative || string(ev.USDAmount) != wantUSD || string(ev.SignedBaseQty) != wantQty || len(ev.Coverage.Gaps) != 0 {
			t.Fatalf("%s: the saved event is %+v", label, ev)
		}
		if e.savedCash(member) != e.cash(member) {
			t.Fatalf("%s: the saved cash %v is not the booked cash %v", label, e.savedCash(member), e.cash(member))
		}
	}
	check("okx early", pairTestOKX, pairFundingEventID("okx", "BTC-USDT-SWAP", tEarly), t0.Add(2*time.Minute), "0.150125", "0.1500499375", "-0.05")
	check("hyperliquid hourly", pairTestHL, pairFundingEventID("hyperliquid", "BTC", tHL), t0.Add(time.Hour+30*time.Second), "-0.30005", "-0.30011001", "0.05")
	check("okx later", pairTestOKX, pairFundingEventID("okx", "BTC-USDT-SWAP", tOKX), t0.Add(4*time.Hour+61*time.Second), "-0.601", "-0.6006995", "-0.05")
	if d, l := risk(pairTestHL); d != hlDaily || l != hlLosses {
		t.Fatalf("funding changed the hyperliquid daily pnl or loss count (%v, %d -> %v, %d)", hlDaily, hlLosses, d, l)
	}
	if d, l := risk(pairTestOKX); d != okxDaily || l != okxLosses {
		t.Fatalf("funding changed the okx daily pnl or loss count (%v, %d -> %v, %d)", okxDaily, okxLosses, d, l)
	}
	hlCash, okxCash := e.cash(pairTestHL), e.cash(pairTestOKX)
	e.step(t0.Add(5 * time.Hour))
	e.restart()
	e.step(t0.Add(6 * time.Hour))
	if n := e.count(`SELECT COUNT(*) FROM pair_funding_events WHERE status = 'applied'`); n != 3 {
		t.Fatalf("each venue event settles once across steps and a restart, got %d applied rows", n)
	}
	if n := e.count(`SELECT COUNT(*) FROM trades WHERE exchange_order_id LIKE 'pair_funding:%'`); n != 3 {
		t.Fatalf("each venue event writes one saved funding row, got %d", n)
	}
	if e.cash(pairTestHL) != hlCash || e.cash(pairTestOKX) != okxCash {
		t.Fatal("a later step or a restart charged funding again")
	}
	for _, o := range e.rt().Pair.Progress.Obligations {
		if pairFundingObligation(o.Kind) {
			t.Fatalf("complete evidence leaves no funding obligation, got %+v", o)
		}
	}
	if _, err := e.db().db.Exec(`INSERT INTO pair_funding_events (scope, source, pair_id, contract_fingerprint, strategy_id, provider, account_key, instrument, event_id, rule_version, event_ms, status,
		    source_hash, evidence_hash, usd_amount, event_json, created_at_ms, updated_at_ms)
		SELECT scope, source, pair_id, contract_fingerprint, strategy_id, provider, account_key, instrument, event_id, 'other_rule_v1', event_ms, 'applied',
		    source_hash, evidence_hash, usd_amount, event_json, created_at_ms, updated_at_ms FROM pair_funding_events WHERE status = 'applied' LIMIT 1`); err == nil {
		t.Fatal("a second applied row for one member and event time must be refused by the storage guard")
	}
	if _, err := e.db().db.Exec(`UPDATE pair_funding_events SET usd_amount = '0' WHERE status = 'applied'`); err == nil {
		t.Fatal("an applied funding event must be immutable")
	}
	if n := e.count(`SELECT COUNT(*) FROM pair_funding_inventory WHERE origin = 'unexplained'`); n != 0 {
		t.Fatalf("every book change went through the perps book owner, but %d changes were unexplained", n)
	}
	if n := e.count(`SELECT COUNT(*) FROM pair_funding_inventory WHERE origin = 'coordinator'`); n != 2 {
		t.Fatalf("both entry fills are saved in the inventory history, got %d", n)
	}
}

func TestBasketPairFundingMissingEvidenceStaysIncomplete(t *testing.T) {
	t0 := pairTestStart()
	tHL := t0.Add(time.Hour).UnixMilli()
	tOKX := t0.Add(4 * time.Hour).UnixMilli()
	okxEvent := pairFundingEventID("okx", "BTC-USDT-SWAP", tOKX)
	base := func(d *pairFundingDocument, k map[string]pairEvidenceKey) {
		d.Rates = []pairFundingRate{pfRate(k[pairTestHL], pairFundingRuleHyperliquid, tHL, "0.0001"), pfRate(k[pairTestOKX], pairFundingRuleOKX, tOKX, "0.0002")}
		d.Prices = []pairFundingPrice{pfPrice(k[pairTestHL], pairFundingPriceOracle, tHL, "60010"), pfPrice(k[pairTestOKX], pairFundingPriceMark, tOKX, "60100")}
	}
	okxConv := func(mutate func(d *pairEvidenceDocument, i int)) func(d *pairEvidenceDocument) {
		return func(d *pairEvidenceDocument) {
			for i := range d.Conversions {
				if d.Conversions[i].Key.MemberID == pairTestOKX && d.Conversions[i].Currency == "USDT" {
					mutate(d, i)
					return
				}
			}
		}
	}
	cases := []struct {
		name   string
		fund   func(d *pairFundingDocument, k map[string]pairEvidenceKey)
		ev     func(d *pairEvidenceDocument)
		event  string
		reason string
		gap    bool
	}{
		{name: "okx coverage missing", fund: func(d *pairFundingDocument, k map[string]pairEvidenceKey) {
			base(d, k)
			pairFundingAutoCoverage(d, k, pairTestOKX)
		}, event: okxEvent, reason: pairFundingReasonCoverageGap, gap: true},
		{name: "hyperliquid rule on the okx member", fund: func(d *pairFundingDocument, k map[string]pairEvidenceKey) {
			base(d, k)
			d.Rates[1].RuleVersion = pairFundingRuleHyperliquid
			pairFundingAutoCoverage(d, k)
		}, event: okxEvent, reason: pairFundingReasonRuleProvider},
		{name: "okx price missing", fund: func(d *pairFundingDocument, k map[string]pairEvidenceKey) {
			base(d, k)
			d.Prices = d.Prices[:1]
			d.Prices = append(d.Prices, pfPrice(k[pairTestOKX], pairFundingPriceOracle, tOKX, "60100"))
			pairFundingAutoCoverage(d, k)
		}, event: okxEvent, reason: pairFundingReasonPriceMissing},
		{name: "okx rule unsupported", fund: func(d *pairFundingDocument, k map[string]pairEvidenceKey) {
			base(d, k)
			d.Rates[1].RuleVersion = "okx_swap_realized_mark_v0"
			pairFundingAutoCoverage(d, k)
		}, event: okxEvent, reason: pairFundingReasonRuleUnsupported},
		{name: "conversion invalid at the event", fund: func(d *pairFundingDocument, k map[string]pairEvidenceKey) {
			base(d, k)
			pairFundingAutoCoverage(d, k)
		}, ev: okxConv(func(d *pairEvidenceDocument, i int) { d.Conversions[i].ValidThroughMs = tOKX - 1 }), event: okxEvent, reason: pairFundingReasonConversionAtEvent},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newPairFundingEnv(t, tc.fund, tc.ev)
			e.openMatched(0.05)
			okxBefore := e.cash(pairTestOKX)
			e.step(t0.Add(5*time.Hour + 2*time.Minute))
			status, reason, _ := e.fundingRow(pairTestOKX, tc.event)
			if status != pairFundingStatusIncomplete || reason != tc.reason {
				t.Fatalf("the okx event must stay durable and incomplete with %s, got %q %q", tc.reason, status, reason)
			}
			if len(e.fundingTrades(pairTestOKX)) != 0 || e.cash(pairTestOKX) != okxBefore {
				t.Fatal("an incomplete event booked a charge or a zero row")
			}
			if !e.hasObligation(pairObligationFundingIncomplete, pairTestOKX) {
				t.Fatal("an incomplete event must hold a durable funding obligation")
			}
			if tc.gap && !e.hasObligation(pairObligationFundingGap, pairTestOKX) {
				t.Fatal("held okx exposure without coverage must record a funding gap")
			}
			if len(e.fundingTrades(pairTestHL)) != 1 {
				t.Fatal("the hyperliquid event has complete evidence and settles independently")
			}
			var raw string
			if err := e.db().db.QueryRow(`SELECT progress_json FROM basket_contracts WHERE basket_id = ?`, pairTestID).Scan(&raw); err != nil || !strings.Contains(raw, pairObligationFundingIncomplete) {
				t.Fatalf("the saved progress must carry the funding obligation: %v", err)
			}
			e.restart()
			e.step(t0.Add(5*time.Hour + 3*time.Minute))
			if status, reason, _ := e.fundingRow(pairTestOKX, tc.event); status != pairFundingStatusIncomplete || reason != tc.reason || len(e.fundingTrades(pairTestOKX)) != 0 {
				t.Fatalf("a restart keeps the event incomplete, got %q %q", status, reason)
			}
		})
	}
}

func TestBasketPairFundingConversionValidityAndCutoffAreSeparate(t *testing.T) {
	t0 := pairTestStart()
	tOKX := t0.Add(4 * time.Hour).UnixMilli()
	receipt := t0.Add(7 * time.Hour).UnixMilli()
	okxEvent := pairFundingEventID("okx", "BTC-USDT-SWAP", tOKX)
	e := newPairFundingEnv(t, func(d *pairFundingDocument, k map[string]pairEvidenceKey) {
		d.Rates = []pairFundingRate{pfRate(k[pairTestOKX], pairFundingRuleOKX, tOKX, "0.0002")}
		d.Prices = []pairFundingPrice{pfPrice(k[pairTestOKX], pairFundingPriceMark, tOKX, "60100")}
		pairFundingAutoCoverage(d, k)
	}, func(d *pairEvidenceDocument) {
		for i := range d.Conversions {
			c := d.Conversions[i]
			if c.Key.MemberID != pairTestOKX || c.Currency != "USDT" {
				continue
			}
			d.Conversions[i].ValidThroughMs = tOKX - 1
			late := c
			late.EvidenceID = "conv-okx-usdt-late"
			late.ObservedMs, late.FirstReceiptMs = receipt, receipt
			late.ValidFromMs, late.ValidThroughMs = tOKX, pairFundingFarMs
			late.RateUSD = "0.999"
			d.Conversions = append(d.Conversions, late)
			return
		}
	})
	e.openMatched(0.05)
	e.step(t0.Add(4*time.Hour + 2*time.Minute))
	if status, reason, _ := e.fundingRow(pairTestOKX, okxEvent); status != pairFundingStatusIncomplete || reason != pairFundingReasonConversionCutoff {
		t.Fatalf("a conversion valid at the event but received after the cutoff must hold, got %q %q", status, reason)
	}
	e.step(time.UnixMilli(receipt - 1))
	if len(e.fundingTrades(pairTestOKX)) != 0 {
		t.Fatal("the conversion is not received yet")
	}
	e.step(time.UnixMilli(receipt))
	status, _, raw := e.fundingRow(pairTestOKX, okxEvent)
	if status != pairFundingStatusApplied {
		t.Fatalf("the event settles once the conversion is received by the cutoff, got %q", status)
	}
	ev, err := pairDecodeFundingEvent([]byte(raw))
	if err != nil || ev.Conversion.EvidenceID != "conv-okx-usdt-late" || ev.Coverage.CutoffMs < receipt || string(ev.USDAmount) != "0.600399" {
		t.Fatalf("the event must use the late conversion record valid at the event time: %+v %v", ev, err)
	}
}

func TestBasketPairFundingUsesEventTimeInventory(t *testing.T) {
	t0 := pairTestStart()
	fillAt := t0.Add(10 * time.Second).UnixMilli()
	tLate := t0.Add(30 * time.Second).UnixMilli()
	tWindow := t0.Add(5 * time.Second).UnixMilli()
	e := newPairFundingEnv(t, func(d *pairFundingDocument, k map[string]pairEvidenceKey) {
		okx := k[pairTestOKX]
		d.Rates = []pairFundingRate{pfRate(okx, pairFundingRuleOKX, tWindow, "0.0001"), pfRate(okx, pairFundingRuleOKX, tLate, "0.0001")}
		d.Prices = []pairFundingPrice{pfPrice(okx, pairFundingPriceMark, tWindow, "60000"), pfPrice(okx, pairFundingPriceMark, tLate, "60000")}
		pairFundingAutoCoverage(d, k)
	}, nil)
	pairEnterHoldRunner(e, 0.05)
	pairOpenVenue(e, pairTestOKX, basketVenueOutcome{Outcome: basketVenueUnknown, Detail: "no answer yet"})
	e.slotStep(0)
	first := e.actionsByPurpose(pairPurposeEntry)
	if len(first) != 1 || first[0].Status != basketActionUnresolved {
		t.Fatalf("the first leg must stay unresolved: %+v", first)
	}
	a := first[0]
	e.step(t0.Add(2 * time.Minute))
	lateEvent := pairFundingEventID("okx", "BTC-USDT-SWAP", tLate)
	if status, reason, _ := e.fundingRow(pairTestOKX, lateEvent); status != pairFundingStatusIncomplete || reason != pairFundingReasonInventory {
		t.Fatalf("an unresolved order straddling the event must hold it as inventory incomplete, got %q %q", status, reason)
	}
	visible := t0.Add(2*time.Minute + 30*time.Second).UnixMilli()
	pairQueryVenue(e, a, basketVenueOutcome{Outcome: basketVenueFilled, CumQty: 0.05, CumNotional: 0.05 * 59800, Terminal: true,
		Fills: []pairVenueFill{{FillID: "okx-fill-1", BaseQty: 0.05, EventMs: fillAt, VisibleMs: visible}}})
	e.step(t0.Add(3 * time.Minute))
	e.assertBooks("late fill booked", 0, -0.05)
	var evMs, evLatest, vis int64
	var fillID string
	if err := e.db().db.QueryRow(`SELECT fill_id, event_ms, event_latest_ms, visible_ms FROM pair_funding_inventory WHERE strategy_id = ? AND origin = 'coordinator'`, pairTestOKX).Scan(&fillID, &evMs, &evLatest, &vis); err != nil {
		t.Fatalf("the late fill must be saved with its event time: %v", err)
	}
	if fillID != "okx-fill-1" || evMs != fillAt || evLatest != fillAt || vis != visible {
		t.Fatalf("the saved fill keeps event time and visibility separately, got %s %d %d %d", fillID, evMs, evLatest, vis)
	}
	status, _, raw := e.fundingRow(pairTestOKX, lateEvent)
	if status != pairFundingStatusApplied {
		t.Fatalf("the late earlier fill completes the inventory, so the event settles, got %q", status)
	}
	ev, err := pairDecodeFundingEvent([]byte(raw))
	if err != nil || ev.SignedBaseQty != "-0.05" || ev.NativeAmount != "0.3" {
		t.Fatalf("funding uses the inventory at the event time, not the receipt time: %+v %v", ev, err)
	}
	if status, reason, _ := e.fundingRow(pairTestOKX, pairFundingEventID("okx", "BTC-USDT-SWAP", tWindow)); status != pairFundingStatusUnresolvable || reason != pairFundingReasonAssessment {
		t.Fatalf("a fill inside the okx assessment window makes the event unresolvable, got %q %q", status, reason)
	}
	if n := len(e.fundingTrades(pairTestOKX)); n != 1 {
		t.Fatalf("only the later event is charged; the unresolvable event books nothing, got %d rows", n)
	}
}

func TestBasketPairFundingSameTimeFillIsExcluded(t *testing.T) {
	t0 := pairTestStart()
	hedgeAt := pairSlotAt(t0, 1).Add(time.Second).UnixMilli()
	e := newPairFundingEnv(t, func(d *pairFundingDocument, k map[string]pairEvidenceKey) {
		hl := k[pairTestHL]
		d.Rates = []pairFundingRate{pfRate(hl, pairFundingRuleHyperliquid, hedgeAt, "0.001"), pfRate(hl, pairFundingRuleHyperliquid, hedgeAt+1, "0.001")}
		d.Prices = []pairFundingPrice{pfPrice(hl, pairFundingPriceOracle, hedgeAt, "60000"), pfPrice(hl, pairFundingPriceOracle, hedgeAt+1, "60000")}
		pairFundingAutoCoverage(d, k)
	}, nil)
	pairEnterHoldRunner(e, 0.05)
	pairOpenVenue(e, pairTestHL, basketVenueOutcome{Outcome: basketVenueFilled, Fills: []pairVenueFill{{FillID: "hl-fill-1", BaseQty: 0.05, EventMs: hedgeAt, VisibleMs: hedgeAt}}})
	e.slotStep(0)
	e.slotStep(1)
	e.assertBooks("matched", 0.05, -0.05)
	e.step(t0.Add(3 * time.Minute))
	same := pairFundingEventID("hyperliquid", "BTC", hedgeAt)
	after := pairFundingEventID("hyperliquid", "BTC", hedgeAt+1)
	if status, _, _ := e.fundingRow(pairTestHL, same); status != "" {
		t.Fatalf("a fill at the event time is excluded, so the event owes nothing and writes nothing, got %q", status)
	}
	status, _, raw := e.fundingRow(pairTestHL, after)
	ev, err := pairDecodeFundingEvent([]byte(raw))
	if status != pairFundingStatusApplied || err != nil || ev.SignedBaseQty != "0.05" || ev.NativeAmount != "-3" {
		t.Fatalf("one millisecond later the fill is included: %q %+v %v", status, ev, err)
	}
	if n := len(e.fundingTrades(pairTestHL)); n != 1 {
		t.Fatalf("one hyperliquid charge, got %d", n)
	}
	var origin, fillID string
	if err := e.db().db.QueryRow(`SELECT origin, fill_id FROM pair_funding_inventory WHERE strategy_id = ?`, pairTestHL).Scan(&origin, &fillID); err != nil || origin != pairInventoryOriginCoordinator || fillID != "hl-fill-1" {
		t.Fatalf("the hyperliquid open is recorded by the perps book owner with its fill identity, got %q %q %v", origin, fillID, err)
	}
}

func TestBasketPairFundingContradictedCompletenessHolds(t *testing.T) {
	t0 := pairTestStart()
	tEvent := t0.Add(2 * time.Minute).UnixMilli()
	e := newPairFundingEnv(t, func(d *pairFundingDocument, k map[string]pairEvidenceKey) {
		d.Rates = []pairFundingRate{pfRate(k[pairTestOKX], pairFundingRuleOKX, tEvent, "0.0001")}
		d.Prices = []pairFundingPrice{pfPrice(k[pairTestOKX], pairFundingPriceMark, tEvent, "60000")}
		pairFundingAutoCoverage(d, k)
	}, nil)
	pairEnterHoldRunner(e, 0.05)
	pairOpenVenue(e, pairTestOKX, basketVenueOutcome{Outcome: pairVenueOpen, CumQty: 0.02, CumNotional: 0.02 * 59800, CoverageThroughMs: t0.Add(10 * time.Minute).UnixMilli()})
	e.slotStep(0)
	a := e.actionsByPurpose(pairPurposeEntry)[0]
	e.step(t0.Add(3*time.Minute + 10*time.Second))
	event := pairFundingEventID("okx", "BTC-USDT-SWAP", tEvent)
	status, _, raw := e.fundingRow(pairTestOKX, event)
	if status != pairFundingStatusApplied {
		t.Fatalf("venue history declared complete through the event lets it settle, got %q", status)
	}
	cash := e.cash(pairTestOKX)
	pairQueryVenue(e, a, basketVenueOutcome{Outcome: pairVenueOpen, CumQty: 0.05, CumNotional: 0.02*59800 + 0.03*59800, CoverageThroughMs: t0.Add(10 * time.Minute).UnixMilli(),
		Fills: []pairVenueFill{{FillID: "f1", BaseQty: 0.02, EventMs: a.SubmittedAtMs, VisibleMs: a.SubmittedAtMs}, {FillID: "f2", BaseQty: 0.03, EventMs: t0.Add(time.Minute).UnixMilli(), VisibleMs: t0.Add(4 * time.Minute).UnixMilli()}}})
	e.step(t0.Add(4 * time.Minute))
	if !e.hasObligation(pairObligationFundingIntegrity, pairTestOKX) || !e.rt().Pair.Progress.IncreasesFrozen {
		t.Fatalf("an earlier fill after accepted completeness must open a durable integrity hold: %+v", e.rt().Pair.Progress)
	}
	if status2, _, raw2 := e.fundingRow(pairTestOKX, event); status2 != pairFundingStatusApplied || raw2 != raw {
		t.Fatal("the applied charge is never rewritten")
	}
	if len(e.fundingTrades(pairTestOKX)) != 1 {
		t.Fatalf("no second charge is booked (cash was %v)", cash)
	}
	pairQueryVenue(e, a, basketVenueOutcome{Outcome: basketVenueFilled, CumQty: 0.05, CumNotional: 0.02*59800 + 0.03*59800, Terminal: true, CoverageThroughMs: t0.Add(10 * time.Minute).UnixMilli(),
		Fills: []pairVenueFill{{FillID: "f1", BaseQty: 0.02, EventMs: a.SubmittedAtMs, VisibleMs: a.SubmittedAtMs}, {FillID: "f2", BaseQty: 0.03, EventMs: t0.Add(time.Minute).UnixMilli(), VisibleMs: t0.Add(4 * time.Minute).UnixMilli()}}})
	for i := 5; i <= 9; i++ {
		e.step(t0.Add(time.Duration(i) * time.Minute))
	}
	e.assertBooks("flattened after the integrity hold", 0, 0)
	if e.lifecycle() != pairLifecycleFlat {
		t.Fatalf("a recorded integrity fault must not hold completion forever once the pair is flat and resolved, got %s", e.lifecycle())
	}
	if !e.hasObligation(pairObligationFundingIntegrity, pairTestOKX) {
		t.Fatal("the integrity fault stays saved after completion")
	}
}

func TestBasketPairFundingFailedSaveRetriesWithoutSecondCharge(t *testing.T) {
	t0 := pairTestStart()
	tOKX := t0.Add(time.Hour).UnixMilli()
	event := pairFundingEventID("okx", "BTC-USDT-SWAP", tOKX)
	fund := func(d *pairFundingDocument, k map[string]pairEvidenceKey) {
		d.Rates = []pairFundingRate{pfRate(k[pairTestOKX], pairFundingRuleOKX, tOKX, "0.0002")}
		d.Prices = []pairFundingPrice{pfPrice(k[pairTestOKX], pairFundingPriceMark, tOKX, "60000")}
		pairFundingAutoCoverage(d, k)
	}
	fail := func(t *testing.T) func() {
		prev := storeCommitHook
		failing := true
		storeCommitHook = func(role storageRole) error {
			if failing {
				return os.ErrDeadlineExceeded
			}
			return nil
		}
		t.Cleanup(func() { storeCommitHook = prev })
		return func() { failing = false }
	}
	for _, mode := range []string{"retry", "restart"} {
		t.Run(mode, func(t *testing.T) {
			e := newPairFundingEnv(t, fund, nil)
			e.openMatched(0.05)
			savedBefore := e.savedCash(pairTestOKX)
			stop := fail(t)
			e.step(t0.Add(time.Hour + 2*time.Minute))
			if len(e.fundingTrades(pairTestOKX)) != 1 || !e.rt().needsDurable {
				t.Fatalf("the charge is booked in memory and the save is retried, got %d rows, durable hold %v", len(e.fundingTrades(pairTestOKX)), e.rt().needsDurable)
			}
			if n := e.count(`SELECT COUNT(*) FROM pair_funding_events WHERE status = 'applied'`); n != 0 || e.savedCash(pairTestOKX) != savedBefore {
				t.Fatal("a failed save must leave neither the event nor the cash change durable")
			}
			stop()
			if mode == "restart" {
				e.restart()
				if len(e.fundingTrades(pairTestOKX)) != 0 || e.cash(pairTestOKX) != savedBefore {
					t.Fatal("a restart after the failed save starts from the saved cash with no charge")
				}
			}
			e.step(t0.Add(time.Hour + 3*time.Minute))
			e.step(t0.Add(time.Hour + 4*time.Minute))
			if n := e.count(`SELECT COUNT(*) FROM pair_funding_events WHERE status = 'applied' AND event_id = ?`, event); n != 1 {
				t.Fatalf("the retried event commits once, got %d", n)
			}
			if n := len(e.fundingTrades(pairTestOKX)); n != 1 {
				t.Fatalf("one funding row in memory, got %d", n)
			}
			if n := e.count(`SELECT COUNT(*) FROM trades WHERE exchange_order_id = ?`, pairFundingOrderIDPrefix+pairFundingRuleOKX+":"+event); n != 1 {
				t.Fatalf("one saved funding row, got %d", n)
			}
			want := savedBefore + pairDecimalFloat(t, "0.6")
			if math.Abs(e.savedCash(pairTestOKX)-want) > 1e-9 {
				t.Fatalf("the saved cash changed once: %v, want %v", e.savedCash(pairTestOKX), want)
			}
		})
	}
}

func TestBasketPairFundingStorageOwnershipAndMigration(t *testing.T) {
	t0 := pairTestStart()
	tOKX := t0.Add(time.Hour).UnixMilli()
	fund := func(d *pairFundingDocument, k map[string]pairEvidenceKey) {
		d.Rates = []pairFundingRate{pfRate(k[pairTestOKX], pairFundingRuleOKX, tOKX, "0.0002")}
		pairFundingAutoCoverage(d, k)
	}
	e := newPairFundingEnv(t, fund, nil)
	e.openMatched(0.05)
	e.step(t0.Add(time.Hour + 2*time.Minute))
	if n := e.count(`SELECT COUNT(*) FROM pair_funding_events WHERE status = 'incomplete'`); n != 1 {
		t.Fatalf("the missing price leaves one incomplete event, got %d", n)
	}
	counts := func() [3]int {
		return [3]int{e.count(`SELECT COUNT(*) FROM pair_funding_events`), e.count(`SELECT COUNT(*) FROM pair_funding_inventory`), e.count(`SELECT COUNT(*) FROM pair_funding_chain`)}
	}
	before := counts()
	e.store.Close()
	for i := 0; i < 2; i++ {
		db, err := OpenStateDB(e.cfg.DBFile)
		if err != nil {
			t.Fatalf("repeated migration %d: %v", i, err)
		}
		db.Close()
	}
	e.restart()
	if after := counts(); after != before {
		t.Fatalf("repeated migration changed the funding rows: %v -> %v", before, after)
	}
	if _, err := e.db().db.Exec(`UPDATE pair_funding_inventory SET delta_base_qty = '1' WHERE seq = 1`); err == nil {
		t.Fatal("inventory history must be append-only")
	}

	cfg := *e.cfg
	cfg.Strategies = append([]StrategyConfig(nil), e.cfg.Strategies...)
	for i := range cfg.Strategies {
		cfg.Strategies[i].PairID = ""
	}
	cfg.Pairs = nil
	cfg.pairAdmission = nil
	e.store.Close()
	manifest := filepath.Join(e.dir, pairResearchManifestName)
	stateDigest, manifestDigest := pairFileDigest(t, e.cfg.DBFile), pairFileDigest(t, manifest)
	si, err := inspectStorageLayoutForConfig(&cfg, false)
	if err != nil {
		t.Fatal(err)
	}
	text := formatStorageInspection(si)
	if !strings.Contains(text, "PENDING PAIR FUNDING") || !strings.Contains(text, "pair funding events: 1 (0 applied)") {
		t.Fatalf("read-only inspection must report the stored pair funding:\n%s", text)
	}
	if pairFileDigest(t, e.cfg.DBFile) != stateDigest || pairFileDigest(t, manifest) != manifestDigest {
		t.Fatal("read-only inspection changed the state file or the run manifest")
	}
	e.store = openSourceStore(t, e.cfg)
	if _, err := e.db().db.Exec(`INSERT INTO pair_funding_inventory (scope, source, pair_id, strategy_id, seq, origin, delta_base_qty, post_base_qty, contract_size, event_ms, event_latest_ms, visible_ms, recorded_at_ms)
		VALUES ('paper', '', ?, 'ghost-member', 1, 'native', '1', '1', '1', 1, 1, 1, 1)`, pairTestID); err != nil {
		t.Fatal(err)
	}
	e.store.Close()
	fileDigest := pairFileDigest(t, e.cfg.DBFile)
	if err := pairLoadBaskets(t, e.cfg); err == nil || !strings.Contains(err.Error(), "ghost-member") {
		t.Fatalf("funding rows for an unknown owner must be refused before any change, got %v", err)
	}
	if pairFileDigest(t, e.cfg.DBFile) != fileDigest {
		t.Fatal("the refused load changed the state file")
	}
}

func TestBasketPairFundingWrongFileIsRefused(t *testing.T) {
	registerPairTestPolicy(t)
	dir := filepath.Join(t.TempDir(), "run")
	doc := pairTestDoc(t, dir, func(root map[string]interface{}) {
		root["paper_db_file"] = filepath.Join(dir, "paper.db")
	})
	venues := newPairTestVenues(false)
	adm, cfg, err := newPairResearchAdmission(doc, "run-1", dir, false, pairTestDeps(t, doc, venues))
	if err != nil {
		t.Fatalf("a split research layout must admit: %v", err)
	}
	t.Cleanup(adm.release)
	if err := pairLoadBaskets(t, cfg); err != nil {
		t.Fatalf("the fresh split layout loads: %v", err)
	}
	primary, err := OpenStateDB(cfg.DBFile)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := primary.db.Exec(`INSERT INTO pair_funding_events (scope, source, pair_id, contract_fingerprint, strategy_id, provider, account_key, instrument, event_id, rule_version, event_ms, status, reason, created_at_ms, updated_at_ms)
		VALUES ('paper', '', ?, 'fp', ?, 'okx', 'synthetic:okx-research', 'BTC-USDT-SWAP', 'okx:BTC-USDT-SWAP:1', ?, 1, 'incomplete', 'coverage_gap', 1, 1)`, pairTestID, pairTestOKX, pairFundingRuleOKX); err != nil {
		t.Fatal(err)
	}
	primary.Close()
	paperPath := filepath.Join(dir, "paper.db")
	primaryDigest, paperDigest := pairFileDigest(t, cfg.DBFile), pairFileDigest(t, paperPath)
	err = pairLoadBaskets(t, cfg)
	if err == nil || !strings.Contains(err.Error(), "primary state file") || !strings.Contains(err.Error(), "funding") {
		t.Fatalf("funding evidence in a file that does not own the members must be refused, got %v", err)
	}
	if pairFileDigest(t, cfg.DBFile) != primaryDigest || pairFileDigest(t, paperPath) != paperDigest {
		t.Fatal("the wrong-file refusal changed a state file")
	}
}

func TestBasketPairFundingNegativeCashSurvivesValidation(t *testing.T) {
	t0 := pairTestStart()
	tHL := t0.Add(time.Hour).UnixMilli()
	tOKX := t0.Add(2 * time.Hour).UnixMilli()
	e := newPairFundingEnv(t, func(d *pairFundingDocument, k map[string]pairEvidenceKey) {
		d.Rates = []pairFundingRate{pfRate(k[pairTestHL], pairFundingRuleHyperliquid, tHL, "3"), pfRate(k[pairTestOKX], pairFundingRuleOKX, tOKX, "-3")}
		d.Prices = []pairFundingPrice{pfPrice(k[pairTestHL], pairFundingPriceOracle, tHL, "60000"), pfPrice(k[pairTestOKX], pairFundingPriceMark, tOKX, "60000")}
		pairFundingAutoCoverage(d, k)
	}, nil)
	e.openMatched(0.05)
	e.step(t0.Add(2*time.Hour + 2*time.Minute))
	hlCash, okxCash := e.cash(pairTestHL), e.cash(pairTestOKX)
	if !(hlCash < 0) || !(okxCash < 0) {
		t.Fatalf("both members must hold negative cash after their funding debits, got %v and %v", hlCash, okxCash)
	}
	e.restart()
	ValidateStateWithPairOwner(e.state, e.cfg.Strategies, e.adm)
	if e.cash(pairTestHL) != hlCash || e.cash(pairTestOKX) != okxCash {
		t.Fatalf("the admitted pair owner keeps both negative balances through validation, got %v and %v", e.cash(pairTestHL), e.cash(pairTestOKX))
	}
	if err := e.store.SavePartition(e.state, e.rt().Partition); err != nil {
		t.Fatal(err)
	}
	e.restart()
	ValidateStateWithPairOwner(e.state, e.cfg.Strategies, e.adm)
	e.step(t0.Add(3 * time.Hour))
	if e.cash(pairTestHL) != hlCash || e.cash(pairTestOKX) != okxCash || e.savedCash(pairTestHL) != hlCash || e.savedCash(pairTestOKX) != okxCash {
		t.Fatal("load, validation, save and reload keep each member's cash unchanged with no second charge")
	}
	if n := e.count(`SELECT COUNT(*) FROM pair_funding_events WHERE status = 'applied'`); n != 2 {
		t.Fatalf("the applied identities are unchanged, got %d", n)
	}

	ordinary := StrategyConfig{ID: "okx-plain", Platform: "okx", Type: "perps", Args: []string{"momentum", "BTC", "1h", "--mode=paper", "--inst-type=swap"}}
	legacy := StrategyConfig{ID: "hl-plain", Platform: "hyperliquid", Type: "perps", Args: []string{"momentum", "ETH", "1h", "--mode=paper"}}
	st := NewAppState()
	st.Strategies[ordinary.ID] = &StrategyState{ID: ordinary.ID, Cash: -5, Positions: map[string]*Position{}, OptionPositions: map[string]*OptionPosition{}}
	st.Strategies[legacy.ID] = &StrategyState{ID: legacy.ID, Cash: -7, Positions: map[string]*Position{}, OptionPositions: map[string]*OptionPosition{}}
	ValidateStateWithPairOwner(st, append([]StrategyConfig{ordinary, legacy}, e.cfg.Strategies...), e.adm)
	if st.Strategies[ordinary.ID].Cash != 0 {
		t.Fatal("an ordinary OKX strategy keeps its existing negative-cash clamp")
	}
	if st.Strategies[legacy.ID].Cash != -7 {
		t.Fatal("legacy Hyperliquid funding eligibility keeps its negative cash")
	}
	ValidateState(e.state, e.cfg.Strategies)
	if e.cash(pairTestHL) != 0 || e.cash(pairTestOKX) != 0 {
		t.Fatal("a pair_id without the admitted owner grants no exemption")
	}
}

func TestBasketPairFundingLegacyOwnersSkipPairMembers(t *testing.T) {
	e := newPairTestEnv(t, nil)
	e.openMatched(0.05)
	lines := syncPaperFundingEligibility(e.state, e.cfg.Strategies, e.now)
	if e.state.Strategies[pairTestHL].PaperFunding != nil {
		t.Fatalf("legacy eligibility sync must not create Hyperliquid funding state for a pair member: %v", lines)
	}
	if need := planPaperFunding(e.state, e.cfg.Strategies); len(need) != 0 {
		t.Fatalf("legacy planning must skip pair members, got %v", need)
	}
	e.state.Strategies[pairTestHL].PaperFunding = newPaperFundingState(e.now.UnixMilli())
	res := runPaperFundingAccounting(e.state, e.cfg.Strategies, nil, paperFundingInputs{Coverage: map[string]feedFundingCoverage{"BTC": {Coin: "BTC", FromMs: 1, ToMs: e.now.Add(48 * time.Hour).UnixMilli(), Records: []feedFundingRecord{{Rate: 0.01, TimeMs: e.now.Add(time.Hour).UnixMilli()}}}}}, map[string]float64{"BTC": 60000}, nil, e.now.Add(49*time.Hour))
	if res.Booked != 0 || len(e.fundingTrades(pairTestHL)) != 0 {
		t.Fatal("the legacy accounting cycle must never charge a pair-owned member")
	}
	if coins := paperFundingCoins(e.cfg.Strategies[0]); len(coins) != 0 {
		t.Fatalf("the legacy owner claims no coin for a pair member, got %v", coins)
	}
}

type pairOpenFundingSource struct{}

func (pairOpenFundingSource) Kind() string     { return "open" }
func (pairOpenFundingSource) SourceID() string { return "open" }
func (pairOpenFundingSource) Collect(context.Context, pairFundingRequest) (pairFundingCollected, error) {
	return pairFundingCollected{}, nil
}
func (pairOpenFundingSource) NextVisibleMs(pairEvidenceKey, int64) (int64, bool) { return 0, false }

func TestBasketPairFundingInputIsBoundBeforeAnyWrite(t *testing.T) {
	registerPairTestPolicy(t)
	doc := pairTestDoc(t, filepath.Join(t.TempDir(), "run"), nil)
	venues := newPairTestVenues(false)
	deps := pairTestDeps(t, doc, venues)
	deps.Funding = nil
	pairAdmitBytesRefused(t, doc, deps, "needs a funding source")
	deps.Funding = pairOpenFundingSource{}
	pairAdmitBytesRefused(t, doc, deps, "accepts only the immutable frozen funding source")

	e := newPairTestEnv(t, nil)
	pairEnterHoldRunner(e, 0.05)
	e.slotStep(0)
	e.store.Close()
	resume := func(funding pairFundingSource) error {
		d := pairTestDeps(t, e.doc, e.venues)
		d.Funding = funding
		adm, _, err := newPairResearchAdmission(e.doc, "run-1", e.dir, true, d)
		if err == nil {
			adm.release()
		}
		return err
	}
	digest := dirDigest(t, e.dir)
	changed := pairTestFunding(t, e.doc, func(d *pairFundingDocument) { d.SourceID = "other-funding" })
	if err := resume(changed); err == nil || !strings.Contains(err.Error(), "funding binding does not match") {
		t.Fatalf("changed frozen funding input must refuse resume, got %v", err)
	}
	pairFundingRulesMu.Lock()
	rule := pairFundingRules[pairFundingRuleOKX]
	edited := rule
	edited.AssessmentWindowMs = 30000
	pairFundingRules[pairFundingRuleOKX] = edited
	pairFundingRulesMu.Unlock()
	err := resume(pairTestFunding(t, e.doc, nil))
	pairFundingRulesMu.Lock()
	pairFundingRules[pairFundingRuleOKX] = rule
	pairFundingRulesMu.Unlock()
	if err == nil || !strings.Contains(err.Error(), "venue funding rules") {
		t.Fatalf("changed venue rules must refuse resume, got %v", err)
	}
	if dirDigest(t, e.dir) != digest {
		t.Fatal("refused resumes changed the saved state")
	}
	manifestPath := filepath.Join(e.dir, pairResearchManifestName)
	saved, _ := os.ReadFile(manifestPath)
	var m map[string]interface{}
	_ = json.Unmarshal(saved, &m)
	delete(m, "funding_binding")
	stripped, _ := json.Marshal(m)
	if err := os.WriteFile(manifestPath, stripped, 0o600); err != nil {
		t.Fatal(err)
	}
	digest = dirDigest(t, e.dir)
	if err := resume(pairTestFunding(t, e.doc, nil)); err == nil || !strings.Contains(err.Error(), "no funding binding") {
		t.Fatalf("a saved run without a funding binding must refuse resume, got %v", err)
	}
	if dirDigest(t, e.dir) != digest {
		t.Fatal("refused resumes changed the saved state")
	}
	if err := os.WriteFile(manifestPath, saved, 0o600); err != nil {
		t.Fatal(err)
	}
	d := pairTestDeps(t, e.doc, e.venues)
	adm, cfg, err := newPairResearchAdmission(e.doc, "run-1", e.dir, true, d)
	if err != nil {
		t.Fatalf("identical funding input and rules resume: %v", err)
	}
	t.Cleanup(adm.release)
	db, err := OpenStateDB(cfg.DBFile)
	if err != nil {
		t.Fatal(err)
	}
	var raw string
	if err := db.db.QueryRow(`SELECT progress_json FROM basket_contracts WHERE basket_id = ?`, pairTestID).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	var prog map[string]json.RawMessage
	_ = json.Unmarshal([]byte(raw), &prog)
	if len(prog["funding_binding"]) == 0 {
		t.Fatal("the first pair write must save the funding binding beside the cost evidence")
	}
	delete(prog, "funding_binding")
	rewritten, _ := json.Marshal(prog)
	if _, err := db.db.Exec(`UPDATE basket_contracts SET progress_json = ? WHERE basket_id = ?`, string(rewritten), pairTestID); err != nil {
		t.Fatal(err)
	}
	db.Close()
	before := pairFileDigest(t, cfg.DBFile)
	if err := pairLoadBaskets(t, cfg); err == nil || !strings.Contains(err.Error(), "no funding binding") {
		t.Fatalf("saved pair state without a funding binding must be refused at load, got %v", err)
	}
	if pairFileDigest(t, cfg.DBFile) != before {
		t.Fatal("the refused load changed the state file")
	}
}

func pairTestBatch(t *testing.T, sourceID string, seq int64, prev string, rates []pairFundingRate, prices []pairFundingPrice, cov []pairFundingCoverageRecord) ([]byte, string) {
	t.Helper()
	for i := range rates {
		rates[i], _ = rates[i].sealed()
	}
	for i := range prices {
		prices[i], _ = prices[i].sealed()
	}
	for i := range cov {
		cov[i], _ = cov[i].sealed()
	}
	if rates == nil {
		rates = []pairFundingRate{}
	}
	if prices == nil {
		prices = []pairFundingPrice{}
	}
	if cov == nil {
		cov = []pairFundingCoverageRecord{}
	}
	b, err := pairFundingBatch{V: pairFundingBatchVersion, SourceID: sourceID, Seq: seq, PrevHash: prev, Rates: rates, Prices: prices, Coverage: cov}.sealed()
	if err != nil {
		t.Fatal(err)
	}
	raw, err := pairFundingBatchBytes(b)
	if err != nil {
		t.Fatal(err)
	}
	return raw, b.BatchHash
}

func TestBasketPairFundingRecordedSourceIsAppendOnly(t *testing.T) {
	t0 := pairTestStart()
	anchor := t0.UnixMilli()
	mid := t0.Add(30 * time.Minute).UnixMilli()
	tOKX := t0.Add(40 * time.Minute).UnixMilli()
	end := t0.Add(50 * time.Minute).UnixMilli()
	covFor := func(k pairEvidenceKey, id string, from, through, visible int64) pairFundingCoverageRecord {
		return pairFundingCoverageRecord{V: pairFundingRecordVersion, Key: k, EvidenceID: id, RequestedFromMs: from, RequestedThroughMs: through,
			Proven: []pairFundingInterval{{FromMs: from, ThroughMs: through}}, Gaps: []pairCoverageGap{}, VisibleMs: visible, Provenance: "research-funding:recorded"}
	}
	var keys map[string]pairEvidenceKey
	build := func(variant string) *pairRecordedFundingSource {
		src, err := newPairRecordedFundingSource("paper-recorder-1", anchor)
		if err != nil {
			t.Fatal(err)
		}
		hl, okx := keys[pairTestHL], keys[pairTestOKX]
		raw1, h1 := pairTestBatch(t, "paper-recorder-1", 1, "", nil, nil, []pairFundingCoverageRecord{covFor(hl, "hl-1"+variant, anchor-1, mid, anchor), covFor(okx, "okx-1"+variant, anchor-1, mid, anchor)})
		if err := src.Append(raw1); err != nil {
			t.Fatalf("batch 1: %v", err)
		}
		if variant != "" {
			raw2, _ := pairTestBatch(t, "paper-recorder-1", 2, h1, nil, nil, nil)
			if err := src.Append(raw2); err != nil {
				t.Fatal(err)
			}
			return src
		}
		conflict, _ := pairTestBatch(t, "paper-recorder-1", 2, h1, []pairFundingRate{pfRate(okx, pairFundingRuleOKX, mid-60000, "0.0001")}, nil, nil)
		if err := src.Append(conflict); err == nil || !strings.Contains(err.Error(), "already proved complete") {
			t.Fatalf("a later batch cannot add a rate inside earlier proven coverage, got %v", err)
		}
		stale, _ := pairTestBatch(t, "paper-recorder-1", 1, "", nil, nil, nil)
		if err := src.Append(stale); err == nil || !strings.Contains(err.Error(), "does not extend the chain") {
			t.Fatalf("a replacement batch is refused, got %v", err)
		}
		raw2, _ := pairTestBatch(t, "paper-recorder-1", 2, h1, []pairFundingRate{pfRate(okx, pairFundingRuleOKX, tOKX, "0.0001")},
			[]pairFundingPrice{pfPrice(okx, pairFundingPriceMark, tOKX, "60000")},
			[]pairFundingCoverageRecord{covFor(okx, "okx-2", mid, end, tOKX+1000), covFor(hl, "hl-2", mid, end, tOKX+1000)})
		if err := src.Append(raw2); err != nil {
			t.Fatalf("a checked appended batch extends coverage: %v", err)
		}
		return src
	}
	var rec *pairRecordedFundingSource
	e := newPairTestEnvDeps(t, pairSetLimit("max_hold_age_ms", 86400000), func(doc []byte, venues *pairTestVenues) pairResearchDeps {
		keys = pairFundingTestKeys(doc)
		rec = build("")
		return pairResearchDeps{VenueFor: venues.factory(), Evidence: pairTestEvidence(t, doc, nil), Quotes: newPairFixtureQuotes(t, doc).collector, Funding: rec}
	})
	e.openMatched(0.05)
	e.step(t0.Add(42 * time.Minute))
	event := pairFundingEventID("okx", "BTC-USDT-SWAP", tOKX)
	if status, _, _ := e.fundingRow(pairTestOKX, event); status != pairFundingStatusApplied {
		t.Fatalf("the recorded batch settles the event, got %q", status)
	}
	if n := e.count(`SELECT COUNT(*) FROM pair_funding_chain`); n != 2 {
		t.Fatalf("both accepted batches are saved, got %d", n)
	}
	e.store.Close()
	digest := dirDigest(t, e.dir)
	other := build("-changed")
	d := pairTestDeps(t, e.doc, e.venues)
	d.Funding = other
	if _, _, err := newPairResearchAdmission(e.doc, "run-1", e.dir, true, d); err == nil || !strings.Contains(err.Error(), "prior evidence is immutable") {
		t.Fatalf("a recorded source that replaces saved batches must refuse resume, got %v", err)
	}
	if dirDigest(t, e.dir) != digest {
		t.Fatal("the refused recorded resume changed the saved state")
	}
	again := build("")
	raw3, _ := pairTestBatch(t, "paper-recorder-1", 3, again.chain()[1].BatchHash, nil, nil, nil)
	if err := again.Append(raw3); err != nil {
		t.Fatal(err)
	}
	d.Funding = again
	adm, _, err := newPairResearchAdmission(e.doc, "run-1", e.dir, true, d)
	if err != nil {
		t.Fatalf("the saved prefix plus a checked append resumes: %v", err)
	}
	adm.release()
}

func TestBasketPairFundingHyperliquidCoverageEndsAtTheLastRecord(t *testing.T) {
	t0 := pairTestStart()
	r1, r2 := t0.Add(time.Hour).UnixMilli()+76, t0.Add(2*time.Hour).UnixMilli()+76
	horizon := t0.Add(5 * time.Hour).UnixMilli()
	prev := fetchHyperliquidFundingHistoryFn
	t.Cleanup(func() { fetchHyperliquidFundingHistoryFn = prev })
	fetchHyperliquidFundingHistoryFn = func(ctx context.Context, coin string, startMs int64) ([]feedFundingRecord, error) {
		var out []feedFundingRecord
		for _, r := range []feedFundingRecord{{Rate: 0.0000125, TimeMs: r1}, {Rate: -0.00002, TimeMs: r2}} {
			if r.TimeMs >= startMs {
				out = append(out, r)
			}
		}
		return out, nil
	}
	cov, err := hlFundingRecordsCoverage(context.Background(), "BTC", t0.UnixMilli(), time.UnixMilli(horizon), 4)
	if err != nil {
		t.Fatal(err)
	}
	var hlCov pairFundingCoverageRecord
	e := newPairFundingEnv(t, func(d *pairFundingDocument, k map[string]pairEvidenceKey) {
		rates, c, err := pairFundingFromHyperliquidCoverage(k[pairTestHL], cov, horizon, horizon, "hl-fundingHistory:fixture", "hl-cov-1")
		if err != nil {
			t.Fatal(err)
		}
		hlCov = c
		d.Rates = rates
		d.Prices = []pairFundingPrice{pfPrice(k[pairTestHL], pairFundingPriceOracle, r1, "60000"), pfPrice(k[pairTestHL], pairFundingPriceOracle, r2, "60000")}
		pairFundingAutoCoverage(d, k, pairTestHL)
		d.Coverage = append(d.Coverage, c)
	}, nil)
	if len(hlCov.Proven) != 1 || hlCov.Proven[0] != (pairFundingInterval{FromMs: t0.UnixMilli() - 1, ThroughMs: r2}) ||
		len(hlCov.Gaps) != 1 || hlCov.Gaps[0] != (pairCoverageGap{FromMs: r2, ThroughMs: horizon, Reason: "after_last_record"}) {
		t.Fatalf("hyperliquid coverage proves only through its last returned record: %+v", hlCov)
	}
	e.openMatched(0.05)
	e.step(time.UnixMilli(horizon).Add(time.Minute))
	if n := len(e.fundingTrades(pairTestHL)); n != 2 {
		t.Fatalf("both hyperliquid records settle, got %d", n)
	}
	if !e.hasObligation(pairObligationFundingGap, pairTestHL) {
		t.Fatal("held exposure after the last returned record is a gap, never a proven zero")
	}
	if e.hasObligation(pairObligationFundingGap, pairTestOKX) || len(e.fundingTrades(pairTestOKX)) != 0 {
		t.Fatal("hyperliquid coverage never proves or charges okx funding")
	}
}

func (e *pairTestEnv) memberAction(id, purpose string) *basketAction {
	e.t.Helper()
	for _, a := range e.actionsByPurpose(purpose) {
		if a.StrategyID == id {
			return a
		}
	}
	e.t.Fatalf("no %s action for %s", purpose, id)
	return nil
}

func (e *pairTestEnv) alertedPrefix(prefix string) bool {
	for _, k := range e.rt().Pair.Progress.Alerts {
		if strings.HasPrefix(k, prefix) {
			return true
		}
	}
	return false
}

func TestBasketPairFundingUnresolvableEventNeverTrapsTheLifecycle(t *testing.T) {
	t0 := pairTestStart()
	cases := []struct {
		name   string
		member string
		event  int64
		reason string
		open   func(e *pairTestEnv)
	}{
		{name: "okx fill inside the assessment window", member: pairTestOKX, event: t0.UnixMilli(), reason: pairFundingReasonAssessment, open: func(e *pairTestEnv) {
			at := t0.Add(time.Second).UnixMilli()
			pairOpenVenue(e, pairTestOKX, basketVenueOutcome{Outcome: basketVenueFilled, Fills: []pairVenueFill{{FillID: "okx-open-1", BaseQty: 0.05, EventMs: at, VisibleMs: at}}})
			e.openMatched(0.05)
		}},
		{name: "hyperliquid fill time spans the event", member: pairTestHL, event: pairSlotAt(t0, 1).Add(30 * time.Second).UnixMilli(), reason: pairFundingReasonFillTime, open: func(e *pairTestEnv) {
			pairEnterHoldRunner(e, 0.05)
			pairOpenVenue(e, pairTestHL, basketVenueOutcome{Outcome: basketVenueUnknown, Detail: "no answer yet"})
			e.slotStep(0)
			e.slotStep(1)
			hedge := e.memberAction(pairTestHL, pairPurposeEntry)
			e.step(pairSlotAt(t0, 1).Add(50 * time.Second))
			if status, reason, _ := e.fundingRow(pairTestHL, pairFundingEventID("hyperliquid", "BTC", pairSlotAt(t0, 1).Add(30*time.Second).UnixMilli())); status == pairFundingStatusUnresolvable || (status != "" && reason != pairFundingReasonInventory) {
				t.Fatalf("while the hedge is unresolved the event only waits for inventory, got %q %q", status, reason)
			}
			pairQueryVenue(e, hedge, basketVenueOutcome{Outcome: basketVenueFilled, CumQty: 0.05, CumNotional: 0.05 * 60000, Terminal: true})
			e.step(pairSlotAt(t0, 2).Add(30 * time.Second))
			e.assertBooks("matched after the late hedge answer", 0.05, -0.05)
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			eventID := pairFundingEventID("hyperliquid", "BTC", tc.event)
			rule, kind, px := pairFundingRuleHyperliquid, pairFundingPriceOracle, "60000"
			if tc.member == pairTestOKX {
				eventID = pairFundingEventID("okx", "BTC-USDT-SWAP", tc.event)
				rule, kind = pairFundingRuleOKX, pairFundingPriceMark
			}
			e := newPairFundingEnv(t, func(d *pairFundingDocument, k map[string]pairEvidenceKey) {
				d.Rates = []pairFundingRate{pfRate(k[tc.member], rule, tc.event, "0.0001")}
				d.Prices = []pairFundingPrice{pfPrice(k[tc.member], kind, tc.event, px)}
				pairFundingAutoCoverage(d, k)
			}, nil)
			tc.open(e)
			pairEnterExitRunner(e, 0.05)
			e.slotStep(5)
			e.assertBooks("exited", 0, 0)
			e.step(pairSlotAt(t0, 6))
			status, reason, _ := e.fundingRow(tc.member, eventID)
			if status != pairFundingStatusUnresolvable || reason != tc.reason {
				t.Fatalf("the event can never settle from the saved history, so it is saved unresolvable with %s, got %q %q", tc.reason, status, reason)
			}
			if n := len(e.fundingTrades(tc.member)); n != 0 {
				t.Fatalf("an unresolvable event books no charge and no zero row, got %d rows", n)
			}
			if e.hasObligation(pairObligationFundingIncomplete, tc.member) {
				t.Fatal("an unresolvable event leaves no open funding obligation")
			}
			if !e.alertedPrefix("funding-unresolvable|") {
				t.Fatal("an unresolvable event raises one critical alert")
			}
			var detail string
			if err := e.db().db.QueryRow(`SELECT detail FROM pair_funding_events WHERE strategy_id = ? AND event_id = ?`, tc.member, eventID).Scan(&detail); err != nil || !strings.Contains(detail, "lies between") {
				t.Fatalf("the saved detail reports the unknown position and charge range, got %q %v", detail, err)
			}
			if e.lifecycle() != pairLifecycleFlat {
				t.Fatalf("the exit filled flat with only an unresolvable funding event, so the lifecycle must leave exiting, got %s", e.lifecycle())
			}
			if _, err := e.db().db.Exec(`UPDATE pair_funding_events SET status = 'incomplete' WHERE event_id = ?`, eventID); err == nil {
				t.Fatal("an unresolvable event is immutable")
			}
			e.restart()
			e.step(pairSlotAt(t0, 7))
			if status, _, _ := e.fundingRow(tc.member, eventID); status != pairFundingStatusUnresolvable || len(e.fundingTrades(tc.member)) != 0 {
				t.Fatalf("a restart keeps the event unresolvable with no charge, got %q", status)
			}
			if e.lifecycle() != pairLifecycleFlat || e.hasObligation(pairObligationFundingIncomplete, tc.member) {
				t.Fatalf("after a restart the pair stays flat with no funding hold, got %s", e.lifecycle())
			}
		})
	}
}

func TestBasketPairFundingEvidenceHoldBlocksCompletionAndKeepsTheHeldPosition(t *testing.T) {
	t0 := pairTestStart()
	tOKX := pairSlotAt(t0, 2).UnixMilli()
	receipt := pairSlotAt(t0, 10).UnixMilli()
	event := pairFundingEventID("okx", "BTC-USDT-SWAP", tOKX)
	e := newPairFundingEnv(t, func(d *pairFundingDocument, k map[string]pairEvidenceKey) {
		d.Rates = []pairFundingRate{pfRate(k[pairTestOKX], pairFundingRuleOKX, tOKX, "0.0002")}
		d.Prices = []pairFundingPrice{pfPrice(k[pairTestOKX], pairFundingPriceMark, tOKX, "60100")}
		pairFundingAutoCoverage(d, k)
	}, func(d *pairEvidenceDocument) {
		for i := range d.Conversions {
			c := d.Conversions[i]
			if c.Key.MemberID != pairTestOKX || c.Currency != "USDT" {
				continue
			}
			d.Conversions[i].ValidThroughMs = tOKX - 1
			late := c
			late.EvidenceID = "conv-okx-usdt-at-event"
			late.ObservedMs, late.FirstReceiptMs = receipt, receipt
			late.ValidFromMs, late.ValidThroughMs = tOKX, tOKX
			late.RateUSD = "0.999"
			after := c
			after.EvidenceID = "conv-okx-usdt-after-event"
			after.ValidFromMs = tOKX + 1
			d.Conversions = append(d.Conversions, late, after)
			return
		}
	})
	e.openMatched(0.05)
	pid := e.state.Strategies[pairTestOKX].Positions["BTC"].TradePositionID
	if pid == "" {
		t.Fatal("the okx member position needs an identity")
	}
	pairEnterExitRunner(e, 0.05)
	e.slotStep(3)
	e.assertBooks("exited", 0, 0)
	e.step(pairSlotAt(t0, 4))
	if status, reason, _ := e.fundingRow(pairTestOKX, event); status != pairFundingStatusIncomplete || reason != pairFundingReasonConversionCutoff {
		t.Fatalf("a conversion received after the cutoff holds the event, got %q %q", status, reason)
	}
	if len(e.fundingTrades(pairTestOKX)) != 0 || !e.hasObligation(pairObligationFundingIncomplete, pairTestOKX) {
		t.Fatal("a held event books nothing, not even zero, and keeps its obligation")
	}
	if e.lifecycle() != pairLifecycleExiting {
		t.Fatalf("an event that waits for evidence still blocks completion, got %s", e.lifecycle())
	}
	e.restart()
	e.step(pairSlotAt(t0, 5))
	if e.lifecycle() != pairLifecycleExiting || len(e.fundingTrades(pairTestOKX)) != 0 {
		t.Fatalf("a restart keeps the evidence hold, got %s", e.lifecycle())
	}
	e.step(time.UnixMilli(receipt))
	if status, _, _ := e.fundingRow(pairTestOKX, event); status != pairFundingStatusApplied {
		t.Fatalf("the event settles once the conversion is received, got %q", status)
	}
	rows := e.fundingTrades(pairTestOKX)
	if len(rows) != 1 || rows[0].PositionID != pid {
		t.Fatalf("the late charge carries the closed position %s that held the inventory at the event, got %+v", pid, rows)
	}
	var saved string
	if err := e.db().db.QueryRow(`SELECT position_id FROM trades WHERE exchange_order_id = ?`, pairFundingOrderIDPrefix+pairFundingRuleOKX+":"+event).Scan(&saved); err != nil || saved != pid {
		t.Fatalf("the saved funding row carries %s, got %q %v", pid, saved, err)
	}
	e.step(time.UnixMilli(receipt).Add(time.Minute))
	if e.lifecycle() != pairLifecycleFlat {
		t.Fatalf("with the evidence settled the lifecycle completes, got %s", e.lifecycle())
	}
}

func TestBasketPairFundingChargeKeepsThePositionHeldAtTheEvent(t *testing.T) {
	t0 := pairTestStart()
	submit := pairSlotAt(t0, 3).Add(time.Second)
	tHL := submit.Add(10 * time.Second).UnixMilli()
	fillAt := tHL + 20000
	event := pairFundingEventID("hyperliquid", "BTC", tHL)
	t.Run("exit fill after the event", func(t *testing.T) {
		e := newPairFundingEnv(t, func(d *pairFundingDocument, k map[string]pairEvidenceKey) {
			d.Rates = []pairFundingRate{pfRate(k[pairTestHL], pairFundingRuleHyperliquid, tHL, "0.0001")}
			d.Prices = []pairFundingPrice{pfPrice(k[pairTestHL], pairFundingPriceOracle, tHL, "60000")}
			pairFundingAutoCoverage(d, k)
		}, nil)
		e.openMatched(0.05)
		pid := e.state.Strategies[pairTestHL].Positions["BTC"].TradePositionID
		pairEnterExitRunner(e, 0.05)
		pairOpenVenue(e, pairTestHL, basketVenueOutcome{Outcome: basketVenueUnknown, Detail: "no answer yet"})
		e.slotStep(3)
		exit := e.memberAction(pairTestHL, pairPurposeExit)
		if exit.SubmittedAtMs >= tHL {
			t.Fatalf("the exit must be submitted before the event (%d >= %d)", exit.SubmittedAtMs, tHL)
		}
		e.step(time.UnixMilli(tHL + 5000))
		if status, reason, _ := e.fundingRow(pairTestHL, event); status != pairFundingStatusIncomplete || reason != pairFundingReasonInventory {
			t.Fatalf("the pending exit holds the event as inventory incomplete, got %q %q", status, reason)
		}
		pairQueryVenue(e, exit, basketVenueOutcome{Outcome: basketVenueFilled, CumQty: 0.05, CumNotional: 0.05 * 60000, Terminal: true,
			Fills: []pairVenueFill{{FillID: "hl-exit-1", BaseQty: 0.05, EventMs: fillAt, VisibleMs: fillAt}}})
		e.step(time.UnixMilli(fillAt + 10000))
		e.assertBooks("exited", 0, 0)
		if status, _, _ := e.fundingRow(pairTestHL, event); status != pairFundingStatusApplied {
			t.Fatalf("the exit fill after the event completes the inventory, got %q", status)
		}
		rows := e.fundingTrades(pairTestHL)
		if len(rows) != 1 || rows[0].PositionID != pid || pid == "" {
			t.Fatalf("the charge settled after the close carries the closed position %s, got %+v", pid, rows)
		}
		e.restart()
		e.step(time.UnixMilli(fillAt + 70000))
		var saved string
		if err := e.db().db.QueryRow(`SELECT position_id FROM trades WHERE exchange_order_id = ?`, pairFundingOrderIDPrefix+pairFundingRuleHyperliquid+":"+event).Scan(&saved); err != nil || saved != pid {
			t.Fatalf("the saved funding row keeps %s across a restart, got %q %v", pid, saved, err)
		}
		if n := len(e.fundingTrades(pairTestHL)); n != 1 {
			t.Fatalf("a restart books no second charge, got %d", n)
		}
	})
	t.Run("close and reopen before the evidence arrives", func(t *testing.T) {
		tOKX := pairSlotAt(t0, 2).UnixMilli()
		receipt := pairSlotAt(t0, 10).UnixMilli()
		okxEvent := pairFundingEventID("okx", "BTC-USDT-SWAP", tOKX)
		e := newPairFundingEnv(t, func(d *pairFundingDocument, k map[string]pairEvidenceKey) {
			d.Rates = []pairFundingRate{pfRate(k[pairTestOKX], pairFundingRuleOKX, tOKX, "0.0002")}
			d.Prices = []pairFundingPrice{pfPrice(k[pairTestOKX], pairFundingPriceMark, tOKX, "60100")}
			pairFundingAutoCoverage(d, k)
		}, func(d *pairEvidenceDocument) {
			for i := range d.Conversions {
				c := d.Conversions[i]
				if c.Key.MemberID != pairTestOKX || c.Currency != "USDT" {
					continue
				}
				d.Conversions[i].ValidThroughMs = tOKX - 1
				late := c
				late.EvidenceID = "conv-okx-usdt-at-event"
				late.ObservedMs, late.FirstReceiptMs = receipt, receipt
				late.ValidFromMs, late.ValidThroughMs = tOKX, tOKX
				after := c
				after.EvidenceID = "conv-okx-usdt-after-event"
				after.ValidFromMs = tOKX + 1
				d.Conversions = append(d.Conversions, late, after)
				return
			}
		})
		e.openMatched(0.05)
		pid := e.state.Strategies[pairTestOKX].Positions["BTC"].TradePositionID
		e.step(pairSlotAt(t0, 3))
		e.now = pairSlotAt(t0, 4)
		e.rt().clock = e.now
		e.externalBookChange(pairTestOKX, 0)
		e.step(pairSlotAt(t0, 4).Add(time.Second))
		e.now = pairSlotAt(t0, 5)
		e.rt().clock = e.now
		e.externalBookChange(pairTestOKX, -0.05)
		reopened := e.state.Strategies[pairTestOKX].Positions["BTC"].TradePositionID
		e.step(pairSlotAt(t0, 5).Add(time.Second))
		if reopened == "" || reopened == pid {
			t.Fatalf("the reopened position needs a new identity, got %q (closed %q)", reopened, pid)
		}
		if status, reason, _ := e.fundingRow(pairTestOKX, okxEvent); status != pairFundingStatusIncomplete || reason != pairFundingReasonConversionCutoff {
			t.Fatalf("the event still waits for its conversion, got %q %q", status, reason)
		}
		e.restart()
		e.step(time.UnixMilli(receipt))
		rows := e.fundingTrades(pairTestOKX)
		if len(rows) != 1 || rows[0].PositionID != pid {
			t.Fatalf("the late charge after a close, a reopen and a restart carries the position %s held at the event, got %+v (open now %s)", pid, rows, reopened)
		}
	})
}

func TestBasketPairFundingSteadyCycleHashesNoAppliedEvent(t *testing.T) {
	t0 := pairTestStart()
	anchor := t0.UnixMilli()
	const events = 2160
	step := 10 * time.Minute
	held := 5
	at := func(i int) int64 { return t0.Add(time.Duration(i+1) * step).UnixMilli() }
	price := func(k pairEvidenceKey, ms int64) pairFundingPrice {
		p := pfPrice(k, pairFundingPriceOracle, ms, "60000")
		p.EvidenceID = "oracle-hl-" + time.UnixMilli(ms).UTC().Format("20060102150405")
		return p
	}
	var keys map[string]pairEvidenceKey
	var rec *pairRecordedFundingSource
	var h1 string
	e := newPairTestEnvDeps(t, pairSetLimit("max_hold_age_ms", 2147483647), func(doc []byte, venues *pairTestVenues) pairResearchDeps {
		keys = pairFundingTestKeys(doc)
		src, err := newPairRecordedFundingSource("paper-recorder-steady", anchor)
		if err != nil {
			t.Fatal(err)
		}
		hl, okx := keys[pairTestHL], keys[pairTestOKX]
		var rates []pairFundingRate
		var prices []pairFundingPrice
		for i := 0; i < events; i++ {
			rates = append(rates, pfRate(hl, pairFundingRuleHyperliquid, at(i), "0.00001"))
			if i != held {
				prices = append(prices, price(hl, at(i)))
			}
		}
		cov := func(k pairEvidenceKey, id string, through, visible int64) pairFundingCoverageRecord {
			return pairFundingCoverageRecord{V: pairFundingRecordVersion, Key: k, EvidenceID: id, RequestedFromMs: anchor - 1, RequestedThroughMs: through,
				Proven: []pairFundingInterval{{FromMs: anchor - 1, ThroughMs: through}}, Gaps: []pairCoverageGap{}, VisibleMs: visible, Provenance: "research-funding:recorded"}
		}
		var raw []byte
		raw, h1 = pairTestBatch(t, "paper-recorder-steady", 1, "", rates, prices, []pairFundingCoverageRecord{cov(hl, "hl-all", at(events-1), at(events-1)+1000), cov(okx, "okx-all", pairFundingFarMs, anchor)})
		if err := src.Append(raw); err != nil {
			t.Fatalf("batch 1: %v", err)
		}
		rec = src
		return pairResearchDeps{VenueFor: venues.factory(), Evidence: pairTestEvidence(t, doc, nil), Quotes: newPairFixtureQuotes(t, doc).collector, Funding: rec}
	})
	e.openMatched(0.05)
	end := time.UnixMilli(at(events - 1)).Add(time.Hour)
	e.step(end)
	if n := e.count(`SELECT COUNT(*) FROM pair_funding_events WHERE status = 'applied'`); n != events-1 {
		t.Fatalf("every event with its price settles, got %d applied", n)
	}
	heldID := pairFundingEventID("hyperliquid", "BTC", at(held))
	if status, reason, _ := e.fundingRow(pairTestHL, heldID); status != pairFundingStatusIncomplete || reason != pairFundingReasonPriceMissing {
		t.Fatalf("the event without a price waits, got %q %q", status, reason)
	}
	misses := func() int64 { return e.rt().Pair.funding.hashMisses }
	before := misses()
	e.step(end.Add(time.Minute))
	if d := misses() - before; d != 0 {
		t.Fatalf("a cycle with no new records computes no source hash, got %d", d)
	}
	e.restart()
	if n := len(e.rt().Pair.funding.hashes); n != events-1 {
		t.Fatalf("load seeds the source hash of every applied event, got %d", n)
	}
	before = misses()
	e.step(end.Add(2 * time.Minute))
	if d := misses() - before; d != 1 {
		t.Fatalf("after a restart only the held event's source hash is computed, got %d", d)
	}
	raw2, _ := pairTestBatch(t, "paper-recorder-steady", 2, h1, nil, []pairFundingPrice{price(keys[pairTestHL], at(held))}, nil)
	if err := rec.Append(raw2); err != nil {
		t.Fatalf("batch 2: %v", err)
	}
	before = misses()
	e.step(end.Add(3 * time.Minute))
	if status, _, _ := e.fundingRow(pairTestHL, heldID); status != pairFundingStatusApplied {
		t.Fatalf("the held event earlier than the newest applied event settles after a restart once its price arrives, got %q", status)
	}
	if d := misses() - before; d != 0 {
		t.Fatalf("settling the held event reuses its remembered source hash, got %d", d)
	}
	if n := e.count(`SELECT COUNT(*) FROM trades WHERE exchange_order_id LIKE 'pair_funding:%'`); n != events {
		t.Fatalf("each event writes one saved row, got %d", n)
	}
}

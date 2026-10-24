package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func pairCollectEnv(t *testing.T, mutate func(root map[string]interface{})) *pairTestEnv {
	t.Helper()
	e := newPairTestEnv(t, mutate)
	e.collect = true
	e.quotes.enable()
	return e
}

func (e *pairTestEnv) slotRow(slotMs int64) (status, hold, input, evidence string) {
	e.t.Helper()
	if err := e.db().db.QueryRow(`SELECT status, hold_reason, input_fingerprint, evidence_json FROM basket_rebalances WHERE deadline_ms = ?`, slotMs).Scan(&status, &hold, &input, &evidence); err != nil {
		e.t.Fatalf("slot %d: %v", slotMs, err)
	}
	return
}

func pairRequestOf(t *testing.T, a *basketAction) map[string]interface{} {
	t.Helper()
	var m map[string]interface{}
	if err := json.Unmarshal([]byte(a.RequestJSON), &m); err != nil {
		t.Fatalf("action %s request: %v", a.ActionID, err)
	}
	return m
}

func TestBasketPairCollectionCollectsFreezesDecidesAndDispatches(t *testing.T) {
	e := pairCollectEnv(t, nil)
	pairEnterHoldRunner(e, 0.06)
	slot0 := pairTestStart()
	e.step(slot0.Add(1500 * time.Millisecond))
	status, hold, input, evidence := e.slotRow(slot0.UnixMilli())
	if status != basketRebalanceExecuting || hold != "" {
		t.Fatalf("the collected slot must be decided and dispatched in its own step, got %s (%q)", status, hold)
	}
	if input != pairSHA256([]byte(evidence)) {
		t.Fatal("the saved input fingerprint is not the SHA-256 of the saved envelope bytes")
	}
	rb := e.rt().Rebalances[slot0.UnixMilli()]
	env, why := verifyPairEnvelope(e.rt(), rb)
	if why != "" {
		t.Fatalf("the frozen envelope must verify: %s", why)
	}
	if env.StartMs != slot0.Add(1500*time.Millisecond).UnixMilli() || env.CutoffMs != env.StartMs || env.DeadlineMs != env.StartMs+2000 || env.SlotMs != slot0.UnixMilli() {
		t.Fatalf("the envelope timing is wrong: %+v", env)
	}
	for _, m := range env.Members {
		o := m.Observation
		if o.VenueTimeMs == nil || o.Coverage.Status != pairCoverageComplete || len(o.NativeBids) != 5 || len(o.Bids) != 5 || o.NormalizationID == "" {
			t.Fatalf("member %s observation is incomplete: %+v", m.Key.MemberID, o)
		}
	}
	okx, _ := env.member(pairTestOKX)
	if okx.Observation.Bids[0].BaseQty != "0.03" || okx.Observation.ContractSize != "0.01" || okx.Observation.PriceCurrency != "USDT" {
		t.Fatalf("OKX base quantities must be contracts times the verified contract size: %+v", okx.Observation.Bids[0])
	}
	hl, _ := env.member(pairTestHL)
	if hl.Observation.Coverage.SourceContract != pairContractHLSealed || hl.Observation.SnapshotID == "" || hl.Observation.SnapshotID != env.SnapshotID {
		t.Fatalf("the sealed member must bind the captured snapshot: %+v", hl.Observation.Coverage)
	}
	if okx.Observation.Coverage.FromMs != okx.Observation.Coverage.ThroughMs || okx.Observation.Coverage.RequiredWindowMs != 0 || okx.Observation.Coverage.RequiredCapacity != 1 {
		t.Fatalf("a public snapshot declares point coverage only: %+v", okx.Observation.Coverage)
	}
	first := e.actionsByPurpose(pairPurposeEntry)
	if len(first) != 1 {
		t.Fatalf("want one first-leg action, got %d", len(first))
	}
	req := pairRequestOf(t, first[0])
	if req["v"].(float64) != 2 || req["evidence_hash"] != input || req["capability_hash"] != okx.Capability.EvidenceHash || req["contract_fingerprint"] != e.rt().Contract.Fingerprint() {
		t.Fatalf("the first-leg request must bind the selected envelope: %s", first[0].RequestJSON)
	}
	if convs, _ := req["conversions"].([]interface{}); len(convs) != 1 {
		t.Fatalf("the first-leg request must copy the member conversions: %s", first[0].RequestJSON)
	}
	e.restart()
	_, _, input2, evidence2 := e.slotRow(slot0.UnixMilli())
	if input2 != input || evidence2 != evidence {
		t.Fatal("restart changed the frozen envelope bytes or fingerprint")
	}
	if _, why := verifyPairEnvelope(e.rt(), e.rt().Rebalances[slot0.UnixMilli()]); why != "" {
		t.Fatalf("the restarted envelope must verify from its saved bytes: %s", why)
	}
	slot1 := pairSlotAt(pairTestStart(), 1)
	e.step(slot1.Add(time.Second))
	hedges := e.actionsByPurpose(pairPurposeEntry)
	var hedge *basketAction
	for _, a := range hedges {
		if a.StrategyID == pairTestHL {
			hedge = a
		}
	}
	if hedge == nil {
		t.Fatalf("the hedge must dispatch on the newer collected event")
	}
	_, _, input1, _ := e.slotRow(slot1.UnixMilli())
	if pairRequestOf(t, hedge)["evidence_hash"] != input1 || input1 == input {
		t.Fatalf("the hedge must bind the newer selected quote event's envelope, not the origin decision: %s", hedge.RequestJSON)
	}
	e.assertBooks("hedged from collected quotes", 0.06, -0.06)
}

func TestBasketPairCollectionFailuresHoldDurably(t *testing.T) {
	cases := []struct {
		name  string
		setup func(e *pairTestEnv)
		want  string
	}{
		{"no quote scripted", func(e *pairTestEnv) { e.quotes.enabled = false }, "no fixture quote is scripted"},
		{"read error", func(e *pairTestEnv) { e.quotes.err = errors.New("venue answered 503") }, "venue answered 503"},
		{"missing seal", func(e *pairTestEnv) { e.collect = false }, "no sealed market snapshot"},
		{"slow read", func(e *pairTestEnv) { e.quotes.block = make(chan struct{}) }, "context deadline exceeded"},
		{"late cutoff", func(e *pairTestEnv) {
			e.quotes.onRead = func(pairMemberRead) { e.now = e.now.Add(3 * time.Second) }
		}, "at or after its logical deadline"},
		{"stale receipt", func(e *pairTestEnv) { e.quotes.receiptLagMs = 5001 }, "older than quote_age_ms"},
		{"crossed book", func(e *pairTestEnv) {
			e.quotes.okxBids = [][2]string{{"60100.2", "1"}, {"60100", "1"}, {"60099", "1"}, {"60098", "1"}, {"60097", "1"}}
		}, "crossed or locked"},
		{"short book", func(e *pairTestEnv) { e.quotes.okxAsks = [][2]string{{"60100.1", "1"}, {"60100.2", "1"}} }, "below the required depth 5"},
		{"records changed", func(e *pairTestEnv) {
			e.quotes.onRead = func(pairMemberRead) {
				e.mu.Lock()
				e.rt().touchPairProgress()
				e.mu.Unlock()
			}
		}, "records changed while the quotes were collected"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := pairCollectEnv(t, nil)
			pairEnterHoldRunner(e, 0.06)
			tc.setup(e)
			at := pairTestStart().Add(time.Second)
			start := time.Now()
			e.step(at)
			if time.Since(start) > 10*time.Second {
				t.Fatalf("a failed collection took %s, beyond its bounded budget", time.Since(start))
			}
			status, hold, input, evidence := e.slotRow(pairTestStart().UnixMilli())
			if status != basketRebalanceHeld || !strings.Contains(hold, tc.want) || input != "" || evidence != "" {
				t.Fatalf("the slot must hold durably with %q and no evidence, got %s %q", tc.want, status, hold)
			}
			if e.runner.calls != 0 || len(e.rt().Actions) != 0 {
				t.Fatalf("a failed collection ran the policy %d times and reserved %d actions", e.runner.calls, len(e.rt().Actions))
			}
			if e.quotes.block != nil {
				close(e.quotes.block)
			}
			e.quotes.mu.Lock()
			e.quotes.enabled, e.quotes.err, e.quotes.block, e.quotes.onRead, e.quotes.receiptLagMs = true, nil, nil, nil, 200
			e.quotes.mu.Unlock()
			e.collect = true
			e.step(at.Add(10 * time.Second))
			if status, _, _, _ := e.slotRow(pairTestStart().UnixMilli()); status != basketRebalanceHeld {
				t.Fatalf("a held slot is never collected again (backfill), got %s", status)
			}
		})
	}
}

func TestBasketPairCollectionFailureKeepsRecoveryQueriesAndProtection(t *testing.T) {
	e := pairCollectEnv(t, nil)
	pairEnterExitRunner(e, 0.06)
	pairOpenVenue(e, pairTestHL, basketVenueOutcome{Outcome: basketVenueUnknown, Detail: "timeout"})
	e.step(pairTestStart().Add(time.Second))
	e.step(pairSlotAt(pairTestStart(), 1).Add(time.Second))
	hedge := e.actionsByPurpose(pairPurposeEntry)
	var pending *basketAction
	for _, a := range hedge {
		if a.StrategyID == pairTestHL && basketActionPending(a.Status) {
			pending = a
		}
	}
	if pending == nil {
		t.Fatal("the hedge must stay unresolved after an unknown outcome")
	}
	e.quotes.mu.Lock()
	e.quotes.err = errors.New("public book outage")
	e.quotes.mu.Unlock()
	before := e.venues.hl.queryCnt[pending.ClientOrderID]
	pairQueryVenue(e, pending, basketVenueOutcome{Outcome: basketVenueFilled, CumQty: 0.06, CumNotional: 0.06 * 60100, Terminal: true})
	e.step(pairSlotAt(pairTestStart(), 2).Add(time.Second))
	if e.venues.hl.queryCnt[pending.ClientOrderID] == before {
		t.Fatal("a quote outage suppressed the saved-action query")
	}
	e.assertBooks("settled during the quote outage", 0.06, -0.06)
	if status, hold, _, _ := e.slotRow(pairSlotAt(pairTestStart(), 2).UnixMilli()); status != basketRebalanceHeld || !strings.Contains(hold, "public book outage") {
		t.Fatalf("the outage slot must hold durably, got %s %q", status, hold)
	}
	e.externalBookChange(pairTestOKX, 0)
	e.step(pairSlotAt(pairTestStart(), 3).Add(time.Second))
	prot := e.actionsByPurpose(pairPurposeProtective)
	if len(prot) != 1 || prot[0].StrategyID != pairTestHL {
		t.Fatalf("recovery must reduce the surviving leg during the quote outage, got %d protective actions", len(prot))
	}
	req := pairRequestOf(t, prot[0])
	if req["v"].(float64) != 2 || req["reduce_only"] != true || req["contract_fingerprint"] != e.rt().Contract.Fingerprint() || req["evidence_hash"] != nil || req["capability_hash"] != nil || req["native_booked"] != nil {
		t.Fatalf("a recovery protective reduction reserves a version 2 request with no evidence fields: %s", prot[0].RequestJSON)
	}
}

func TestBasketPairCollectionSaveFailureSetsDurableGateWithoutRecollection(t *testing.T) {
	e := pairCollectEnv(t, nil)
	e.quotes.mu.Lock()
	e.quotes.err = errors.New("book read failed")
	e.quotes.mu.Unlock()
	at := pairTestStart().Add(time.Second)
	f := &pairCommitFault{from: 3}
	f.install(t)
	e.step(at)
	rb := e.rt().Rebalances[pairTestStart().UnixMilli()]
	if rb == nil || rb.Status != basketRebalanceHeld || !e.rt().needsDurable {
		t.Fatalf("an unsaved hold must set the durable gate, got %+v needsDurable=%v", rb, e.rt().needsDurable)
	}
	if status, _, _, _ := e.slotRow(pairTestStart().UnixMilli()); status != basketRebalanceReserved {
		t.Fatalf("the saved reservation must stay reserved until the hold saves, got %s", status)
	}
	e.quotes.mu.Lock()
	e.quotes.err = nil
	e.quotes.mu.Unlock()
	e.step(pairSlotAt(pairTestStart(), 1).Add(time.Second))
	if _, ok := e.rt().Rebalances[pairSlotAt(pairTestStart(), 1).UnixMilli()]; ok || !e.rt().needsDurable {
		t.Fatal("while the hold cannot be saved, no new slot is collected and the durable gate stays set")
	}
	storeCommitHook = nil
	e.step(pairSlotAt(pairTestStart(), 2).Add(time.Second))
	if status, _, _, _ := e.slotRow(pairTestStart().UnixMilli()); status != basketRebalanceHeld || e.rt().needsDurable {
		t.Fatalf("the retried save must make the hold durable, got %s needsDurable=%v", status, e.rt().needsDurable)
	}
	if status, _, _, _ := e.slotRow(pairSlotAt(pairTestStart(), 2).UnixMilli()); status != basketRebalanceHeld && status != basketRebalanceCompleted {
		t.Fatalf("collection resumes only after the hold is durable, got %s", status)
	}
}

func TestBasketPairCollectionNilOrMismatchedSourceHolds(t *testing.T) {
	closers := &pairNativeClosers{}
	e := newPairLiveShapedEnv(t, closers)
	e.collect = true
	e.step(pairTestStart().Add(time.Second))
	if len(e.rt().Rebalances) != 0 {
		t.Fatalf("a nil quote source must reserve no slot, got %d", len(e.rt().Rebalances))
	}
	if !strings.Contains(e.rt().pairQuoteNote, "needs a quote source") {
		t.Fatalf("a nil quote source must hold with a reason, got %q", e.rt().pairQuoteNote)
	}
	e.adm.deps.Quotes = &pairQuoteCollector{sources: map[string]string{pairTestHL: pairSourceArchive, pairTestOKX: pairSourcePublicSnapshot}, readers: map[string]pairMemberReader{}}
	e.step(pairSlotAt(pairTestStart(), 1).Add(time.Second))
	if len(e.rt().Rebalances) != 0 || !strings.Contains(e.rt().pairQuoteNote, "contract freezes") {
		t.Fatalf("a mismatched quote source must hold without a reservation, got %d slots and %q", len(e.rt().Rebalances), e.rt().pairQuoteNote)
	}
}

func TestBasketPairCollectionAdmissionBindsQuoteSources(t *testing.T) {
	registerPairTestPolicy(t)
	doc := pairTestDoc(t, filepath.Join(t.TempDir(), "run"), nil)
	venues := newPairTestVenues(false)
	good := newPairFixtureQuotes(t, doc).collector
	clone := func(edit func(m map[string]string)) *pairQuoteCollector {
		c := &pairQuoteCollector{sources: good.Sources(), readers: good.readers, limit: good.limit}
		edit(c.sources)
		return c
	}
	cases := []struct {
		name   string
		quotes pairQuoteSource
		want   string
	}{
		{"missing source", nil, "needs a quote source"},
		{"typed nil source", (*pairQuoteCollector)(nil), "needs a quote source"},
		{"missing member", clone(func(m map[string]string) { delete(m, pairTestOKX) }), "no reader for member " + pairTestOKX},
		{"extra member", clone(func(m map[string]string) { m["pair-eth"] = pairSourcePublicSnapshot }), "names pair-eth"},
		{"changed choice", clone(func(m map[string]string) { m[pairTestHL] = pairSourcePublicSnapshot }), "contract freezes \"sealed_feed\""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pairAdmitBytesRefused(t, doc, pairResearchDeps{VenueFor: venues.factory(), Evidence: pairTestEvidence(t, doc, nil), Quotes: tc.quotes}, tc.want)
		})
	}
	dir := filepath.Join(t.TempDir(), "run")
	doc2 := pairTestDoc(t, dir, nil)
	q := newPairFixtureQuotes(t, doc2).collector
	adm, _, err := newPairResearchAdmission(doc2, "run-1", dir, false, pairResearchDeps{VenueFor: venues.factory(), Evidence: pairTestEvidence(t, doc2, nil), Quotes: q})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(adm.release)
	if adm.quoteSource() != pairQuoteSource(q) || adm.evidenceSource() == nil {
		t.Fatal("the admission must expose the bound quote source beside the evidence source")
	}
	src := q.Sources()
	src[pairTestHL] = "changed"
	if q.Sources()[pairTestHL] != pairSourceSealedFeed {
		t.Fatal("Sources must return a copy")
	}
}

func TestBasketPairCollectionFeedNeedsAreKeyed(t *testing.T) {
	e := newPairTestEnv(t, nil)
	needs := pairFeedBookNeeds(e.rt())
	key := l2BookKeyFor("BTC")
	if len(needs) != 1 || needs[key] != contractBookNeed(60000, 256, 5) {
		t.Fatalf("only the sealed member joins the keyed need map: %+v", needs)
	}
	var r feedRequirements
	r.addBook(key, contractBookNeed(120000, 64, 3))
	for k, n := range needs {
		r.addBook(k, n)
	}
	if got := r.Books[key]; got.WindowMs != 120000 || got.Capacity != 256 || got.Depth != 5 || len(r.BookConflicts) != 0 {
		t.Fatalf("the pair need must merge through the existing union: %+v %v", got, r.BookConflicts)
	}
	other := contractBookNeed(60000, 1, 1)
	other.Contract = "other/v1"
	r.addBook(key, other)
	if len(r.BookConflicts) != 1 {
		t.Fatalf("a conflicting need on the same key must be recorded, got %v", r.BookConflicts)
	}
	pub := newPairTestEnv(t, pairSetMember(0, "source", "public_snapshot"))
	if n := pairFeedBookNeeds(pub.rt()); len(n) != 0 {
		t.Fatalf("a public member must not enter the feed book map: %+v", n)
	}
	arch := newPairTestEnv(t, pairAll(pairSetMember(0, "source", "archive"), pairSetMember(0, "modeled_delivery_ms", 250)))
	if n := pairFeedBookNeeds(arch.rt()); len(n) != 0 {
		t.Fatalf("an archive member must not enter the feed book map: %+v", n)
	}
}

func pairReencode(t *testing.T, raw string, edit func(env *pairQuoteEnvelope)) string {
	t.Helper()
	var env pairQuoteEnvelope
	if err := json.Unmarshal([]byte(raw), &env); err != nil {
		t.Fatal(err)
	}
	edit(&env)
	b, err := pairCanonicalJSON(env)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func pairRepayload(t *testing.T, o *pairObservation, edit func(tree map[string]interface{})) {
	t.Helper()
	tree, err := pairStrictParse(o.Payload)
	if err != nil {
		t.Fatal(err)
	}
	edit(tree.(map[string]interface{}))
	var buf bytes.Buffer
	if err := pairWriteCanonical(&buf, tree); err != nil {
		t.Fatal(err)
	}
	o.Payload = buf.Bytes()
	o.PayloadHash = pairSHA256(o.Payload)
}

func pairEnvelopeTampers(t *testing.T) map[string]func(env *pairQuoteEnvelope) {
	okx := func(env *pairQuoteEnvelope) *pairEnvelopeMember {
		for i := range env.Members {
			if env.Members[i].Key.MemberID == pairTestOKX {
				return &env.Members[i]
			}
		}
		t.Fatal("no okx member")
		return nil
	}
	return map[string]func(env *pairQuoteEnvelope){
		"payload with a recomputed hash": func(env *pairQuoteEnvelope) {
			o := &okx(env).Observation
			pairRepayload(t, o, func(tree map[string]interface{}) {
				book := tree["response"].(map[string]interface{})["data"].([]interface{})[0].(map[string]interface{})
				book["bids"].([]interface{})[0].([]interface{})[0] = "60099.95"
			})
		},
		"changed normalization": func(env *pairQuoteEnvelope) { okx(env).Observation.NormalizationID = "contract*0.01@other" },
		"wrong partition":       func(env *pairQuoteEnvelope) { okx(env).Key.Partition = "paper:other" },
		"missing member":        func(env *pairQuoteEnvelope) { env.Members = env.Members[:1] },
		"extra conversion": func(env *pairQuoteEnvelope) {
			m := okx(env)
			m.Conversions = append(m.Conversions, m.Conversions[0])
		},
		"crossed book": func(env *pairQuoteEnvelope) {
			o := &okx(env).Observation
			pairRepayload(t, o, func(tree map[string]interface{}) {
				book := tree["response"].(map[string]interface{})["data"].([]interface{})[0].(map[string]interface{})
				book["bids"].([]interface{})[0].([]interface{})[0] = "60100.1"
			})
		},
		"short book": func(env *pairQuoteEnvelope) {
			o := &okx(env).Observation
			pairRepayload(t, o, func(tree map[string]interface{}) {
				book := tree["response"].(map[string]interface{})["data"].([]interface{})[0].(map[string]interface{})
				book["asks"] = book["asks"].([]interface{})[:4]
			})
		},
		"null venue time":          func(env *pairQuoteEnvelope) { okx(env).Observation.VenueTimeMs = nil },
		"cutoff at the deadline":   func(env *pairQuoteEnvelope) { env.CutoffMs = env.DeadlineMs },
		"deadline off the timeout": func(env *pairQuoteEnvelope) { env.DeadlineMs++ },
		"receipt after the cutoff": func(env *pairQuoteEnvelope) {
			env.CutoffMs = okx(env).Observation.FirstReceiptMs - 1
			env.StartMs = env.CutoffMs
			env.DeadlineMs = env.StartMs + 2000
		},
		"base level not normalized": func(env *pairQuoteEnvelope) { okx(env).Observation.Bids[0].BaseQty = "3" },
		"capability of another member": func(env *pairQuoteEnvelope) {
			m := okx(env)
			for _, other := range env.Members {
				if other.Key.MemberID != pairTestOKX {
					m.Capability = other.Capability
				}
			}
		},
	}
}

func TestBasketPairCollectionVerifierRefusesTampering(t *testing.T) {
	e := newPairTestEnv(t, nil)
	at := pairTestStart()
	rb := e.slot(at, "in-0")
	if _, why := verifyPairEnvelope(e.rt(), rb); why != "" {
		t.Fatalf("the fixture envelope must verify: %s", why)
	}
	exp := pairEnvelopeExpectFor(e.rt())
	for name, edit := range pairEnvelopeTampers(t) {
		t.Run(name, func(t *testing.T) {
			raw := pairReencode(t, rb.EvidenceJSON, edit)
			if _, err := pairCheckEnvelope(exp, []byte(raw)); err == nil {
				t.Fatal("the verifier accepted a tampered envelope")
			}
		})
	}
	for name, raw := range map[string]string{
		"duplicate key":       strings.Replace(rb.EvidenceJSON, `{"contract_fingerprint"`, `{"contract_fingerprint":"x","contract_fingerprint"`, 1),
		"unknown field":       strings.Replace(rb.EvidenceJSON, `{"contract_fingerprint"`, `{"comment":"x","contract_fingerprint"`, 1),
		"non-canonical":       strings.Replace(rb.EvidenceJSON, `{"contract_fingerprint"`, `{ "contract_fingerprint"`, 1),
		"unpadded base64":     strings.Replace(rb.EvidenceJSON, `="`, `"`, 1),
		"unsupported version": strings.Replace(rb.EvidenceJSON, `"v":2}`, `"v":3}`, 1),
	} {
		t.Run(name, func(t *testing.T) {
			if raw == rb.EvidenceJSON {
				t.Fatal("the fixture text did not change")
			}
			if _, err := pairCheckEnvelope(exp, []byte(raw)); err == nil {
				t.Fatal("the strict reader accepted malformed envelope bytes")
			}
		})
	}
	t.Run("age and skew boundaries", func(t *testing.T) {
		for _, tc := range []struct {
			cutoffShift int64
			ok          bool
		}{{0, true}, {1, false}} {
			raw := pairReencode(t, rb.EvidenceJSON, func(env *pairQuoteEnvelope) {
				oldest := env.CutoffMs
				for _, m := range env.Members {
					if m.Observation.FirstReceiptMs < oldest {
						oldest = m.Observation.FirstReceiptMs
					}
				}
				env.CutoffMs = oldest + 5000 + tc.cutoffShift
				env.StartMs = env.CutoffMs - 1
				env.DeadlineMs = env.StartMs + 2000
			})
			_, err := pairCheckEnvelope(exp, []byte(raw))
			if (err == nil) != tc.ok {
				t.Fatalf("receipt age at quote_age_ms+%d: err %v, want ok=%v", tc.cutoffShift, err, tc.ok)
			}
		}
	})
	tampered := pairReencode(t, rb.EvidenceJSON, pairEnvelopeTampers(t)["payload with a recomputed hash"])
	e.store.Close()
	db, err := OpenStateDB(e.cfg.DBFile)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.db.Exec(`UPDATE basket_rebalances SET evidence_json = ?, input_fingerprint = ? WHERE deadline_ms = ?`, tampered, pairSHA256([]byte(tampered)), at.UnixMilli()); err != nil {
		t.Fatal(err)
	}
	db.Close()
	e.restart()
	e.step(at.Add(time.Second))
	rb = e.rt().Rebalances[at.UnixMilli()]
	if rb.Status != basketRebalanceHeld || !strings.Contains(rb.HoldReason, "fails verification") || e.runner.calls != 0 {
		t.Fatalf("damaged saved evidence must hold before the policy runs, got %s %q (%d policy calls)", rb.Status, rb.HoldReason, e.runner.calls)
	}
}

type pairRecordedQuotes struct {
	sources map[string]string
	obs     []pairObservation
}

func (r *pairRecordedQuotes) Sources() map[string]string {
	out := map[string]string{}
	for k, v := range r.sources {
		out[k] = v
	}
	return out
}
func (r *pairRecordedQuotes) Recorded() bool { return true }
func (r *pairRecordedQuotes) Collect(ctx context.Context, req pairCollectRequest) ([]pairObservation, error) {
	out := make([]pairObservation, 0, len(r.obs))
	for _, o := range r.obs {
		out = append(out, pairCloneObservation(o))
	}
	return out, nil
}
func (r *pairRecordedQuotes) Reference(ctx context.Context, req pairReferenceRequest) (pairObservation, error) {
	return pairObservation{}, fmt.Errorf("no recorded reference")
}

func TestBasketPairCollectionRecordedReplayKeepsSavedBytes(t *testing.T) {
	a := pairCollectEnv(t, pairAll(pairSetMember(0, "source", "archive"), pairSetMember(0, "modeled_delivery_ms", 1000)))
	at := pairTestStart().Add(time.Second)
	a.step(at)
	_, _, inputA, evidenceA := a.slotRow(pairTestStart().UnixMilli())
	if evidenceA == "" {
		t.Fatal("the archive collection did not freeze")
	}
	envA, err := pairDecodeQuoteEnvelope([]byte(evidenceA), 0)
	if err != nil {
		t.Fatal(err)
	}
	hl, _ := envA.member(pairTestHL)
	if hl.Observation.SourceCutoffMs+1000 != at.UnixMilli() || hl.Observation.FirstReceiptMs != at.UnixMilli()-100 || hl.Observation.Coverage.ThroughMs != at.UnixMilli() {
		t.Fatalf("the archive delay must translate the receipt and coverage once and keep the source cutoff: %+v", hl.Observation)
	}
	rec := &pairRecordedQuotes{sources: map[string]string{pairTestHL: pairSourceArchive, pairTestOKX: pairSourcePublicSnapshot}}
	for _, m := range envA.Members {
		o := pairCloneObservation(m.Observation)
		o.Bids, o.Asks, o.NativeUnit, o.ContractSize, o.PriceCurrency, o.NormalizationID = []pairBaseLevel{}, []pairBaseLevel{}, "", "", "", ""
		rec.obs = append(rec.obs, o)
	}
	b := newPairTestEnv(t, pairAll(pairSetMember(0, "source", "archive"), pairSetMember(0, "modeled_delivery_ms", 1000)))
	b.adm.deps.Quotes = rec
	b.step(at)
	_, _, inputB, evidenceB := b.slotRow(pairTestStart().UnixMilli())
	if inputB != inputA || evidenceB != evidenceA {
		t.Fatal("a recorded replay of the saved observations must keep the original bytes, receipts and fingerprint")
	}
}

func pairWindowPayloadFor(t *testing.T, coin string, cutoffMs, windowMs int64, timeSemantics, snapshotID, owner string, obs []marketBookRecord) []byte {
	t.Helper()
	p := newMarketL2BookPayload(coin, contractBookNeed(windowMs, 256, 5))
	p.TimeSemantics, p.CutoffMs, p.RetainedFromMs, p.PrefixComplete, p.Instance, p.SnapshotID, p.Available = timeSemantics, cutoffMs, cutoffMs-2*windowMs, true, owner, snapshotID, true
	p.Observations = obs
	return pairFixtureCanonical(p)
}

func TestBasketPairCollectionArchiveAndSealedAgreeForEquivalentEvents(t *testing.T) {
	e := newPairTestEnv(t, nil)
	hl, _ := e.rt().Contract.pairMember(pairTestHL)
	need := e.rt().Contract.Pair.BookNeed
	const delay = int64(1000)
	cutoff := pairTestStart().UnixMilli()
	key := l2BookKeyFor("BTC")
	levels := func(px string) []feedBookLevel {
		out := []feedBookLevel{}
		for i := 0; i < 5; i++ {
			out = append(out, feedBookLevel{Px: fmt.Sprintf("%d", 59999-i), Sz: "0.5", N: 1})
		}
		return out
	}
	asks := func() []feedBookLevel {
		out := []feedBookLevel{}
		for i := 0; i < 5; i++ {
			out = append(out, feedBookLevel{Px: fmt.Sprintf("%d", 60001+i), Sz: "0.5", N: 1})
		}
		return out
	}
	var sealedObs, archiveObs []marketBookRecord
	for i := 0; i < 30; i++ {
		event := cutoff - 59000 + int64(i)*2000
		b, a := levels(""), asks()
		id := bookIdentity(key, event, b, a)
		sealedObs = append(sealedObs, marketBookRecord{Seq: uint64(i + 1), EventMs: event, RecvMs: event + 50 + delay, Session: 1, Identity: id, BidDepth: 5, AskDepth: 5, Bids: b, Asks: a})
		archiveObs = append(archiveObs, marketBookRecord{Seq: uint64(i + 1), EventMs: event, RecvMs: event + 50, Session: 1, Identity: id, BidDepth: 5, AskDepth: 5, Bids: b, Asks: a})
	}
	for len(sealedObs) > 0 && sealedObs[len(sealedObs)-1].RecvMs > cutoff {
		sealedObs = sealedObs[:len(sealedObs)-1]
	}
	for len(archiveObs) > len(sealedObs) {
		archiveObs = archiveObs[:len(archiveObs)-1]
	}
	for len(sealedObs) > 0 && sealedObs[0].RecvMs < cutoff-need.WindowMs {
		sealedObs, archiveObs = sealedObs[1:], archiveObs[1:]
	}
	sealedPayload := pairWindowPayloadFor(t, "BTC", cutoff, need.WindowMs, feedBookTimeSemantics, "snap-eq", "feed-1", sealedObs)
	archivePayload := pairWindowPayloadFor(t, "BTC", cutoff-delay, need.WindowMs, pairArchivePayloadTimeBasis, "", "hyperliquid-archive", archiveObs)
	sealedMember := hl
	archiveMember := hl
	archiveMember.Source, archiveMember.ModeledDeliveryMs = pairSourceArchive, delay
	s, err := pairObservationFromPayload(sealedMember, "paper", need, sealedPayload, 0, 7)
	if err != nil {
		t.Fatal(err)
	}
	a, err := pairObservationFromPayload(archiveMember, "paper", need, archivePayload, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if *s.VenueTimeMs != *a.VenueTimeMs || s.FirstReceiptMs != a.FirstReceiptMs || s.Coverage.FromMs != a.Coverage.FromMs || s.Coverage.ThroughMs != a.Coverage.ThroughMs ||
		s.Coverage.Status != a.Coverage.Status || fmt.Sprint(s.NativeBids, s.NativeAsks) != fmt.Sprint(a.NativeBids, a.NativeAsks) {
		t.Fatalf("equivalent events must give equal book values and eligibility:\nsealed  %+v\narchive %+v", s, a)
	}
	if s.Provenance == a.Provenance || s.Coverage.SourceContract == a.Coverage.SourceContract || s.SourceCutoffMs == a.SourceCutoffMs || s.Coverage.TimeSemantics == a.Coverage.TimeSemantics {
		t.Fatal("the sealed and archive observations must keep distinct provenance and time contracts")
	}
	gap := append([]marketBookRecord(nil), archiveObs[:5]...)
	gap = append(gap, archiveObs[len(archiveObs)-1])
	gapped, err := pairObservationFromPayload(archiveMember, "paper", need, pairWindowPayloadFor(t, "BTC", cutoff-delay, need.WindowMs, pairArchivePayloadTimeBasis, "", "hyperliquid-archive", gap), 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if gapped.Coverage.Status != pairCoverageIncomplete || len(gapped.Coverage.Gaps) != 1 || gapped.Coverage.Gaps[0].FromMs != gap[4].RecvMs+delay {
		t.Fatalf("an archive gap must be translated to modeled delivery once: %+v", gapped.Coverage)
	}
	if _, err := pairCheckObservationSource(gapped, archiveMember, gapped.Key, need); err == nil || !strings.Contains(err.Error(), "never repaired") {
		t.Fatalf("an incomplete archive window must hold, got %v", err)
	}
	maxMember := archiveMember
	maxMember.ModeledDeliveryMs = 1<<62 + (1<<62 - 1)
	if _, err := pairObservationFromPayload(maxMember, "paper", need, archivePayload, 0, 0); err == nil || !strings.Contains(err.Error(), "overflows") {
		t.Fatalf("a delay that overflows the receipt must be refused, got %v", err)
	}
}

func TestBasketPairCollectionRequestRulesAtReservation(t *testing.T) {
	e := newPairTestEnv(t, nil)
	pairEnterHoldRunner(e, 0.06)
	e.slotStep(0)
	first := e.actionsByPurpose(pairPurposeEntry)[0]
	rt := e.rt()
	ev, _ := pairQuoteEventFromSlot(rt.Rebalances[pairTestStart().UnixMilli()])
	mk := func(purpose, direction string, edit func(r *pairActionRequest), note bool) *basketAction {
		a := &basketAction{GroupMode: pairGroupMode, Partition: rt.Partition, BasketID: rt.Contract.BasketID, DeadlineMs: pairTestStart().UnixMilli(),
			StrategyID: pairTestHL, Symbol: "BTC", Direction: direction, OrderSide: "buy", PositionSide: entrySideLong, Status: basketActionReserved,
			RequestedQty: 0.06, Provider: entryProviderHyperliquid, AccountKey: "synthetic:hl-research", ClientOrderID: fmt.Sprintf("probe-%s-%d", purpose, time.Now().UnixNano()),
			Purpose: purpose, Instrument: "BTC", NativeQty: 0.06, ContractSize: 1, AcceptDeadlineMs: pairTestStart().UnixMilli() + 30000, CreatedAtMs: 1, UpdatedAtMs: 1}
		if note {
			a.RecoveryJSON = marshalEntryJSON(pairRecoveryNote{NativeBooked: true, Reason: "probe"})
		}
		if direction == basketDirectionIncrease {
			a.QuoteEventID = ev.ID()
		}
		r := pairActionRequest{V: pairActionRequestVersion, PairID: a.BasketID, Partition: rt.Partition.String(), MemberID: a.StrategyID, Provider: a.Provider,
			AccountKey: a.AccountKey, Instrument: a.Instrument, ClientOrderID: a.ClientOrderID, Purpose: purpose, OrderSide: a.OrderSide, PositionSide: a.PositionSide,
			AcceptDeadlineMs: a.AcceptDeadlineMs, QuoteEventID: a.QuoteEventID, ReduceOnly: direction == basketDirectionReduce, ContractFingerprint: rt.Contract.Fingerprint()}
		if edit != nil {
			edit(&r)
		}
		a.RequestJSON = marshalEntryJSON(r)
		return a
	}
	firstReq := pairRequestOf(t, first)
	var convs []pairConversionEvidence
	raw, _ := json.Marshal(firstReq["conversions"])
	_ = json.Unmarshal(raw, &convs)
	use := func() *pairQuoteEventUse {
		return &pairQuoteEventUse{Partition: rt.Partition, PairID: rt.Contract.BasketID, StrategyID: pairTestHL, Event: ev, EventID: ev.ID(), Status: pairUseReserved}
	}
	refused := map[string]struct {
		a   *basketAction
		use *pairQuoteEventUse
	}{
		"increase without evidence": {mk(pairPurposeEntry, basketDirectionIncrease, nil, false), use()},
		"increase with another envelope": {mk(pairPurposeEntry, basketDirectionIncrease, func(r *pairActionRequest) {
			r.Conversions, r.CapabilityHash, r.EvidenceHash = convs, firstReq["capability_hash"].(string), strings.Repeat("a", 64)
		}, false), use()},
		"increase with the other member's capability": {mk(pairPurposeEntry, basketDirectionIncrease, func(r *pairActionRequest) {
			r.Conversions, r.CapabilityHash, r.EvidenceHash = convs, firstReq["capability_hash"].(string), ev.InputFingerprint
		}, false), use()},
		"reduction with evidence":    {mk(pairPurposeProtective, basketDirectionReduce, func(r *pairActionRequest) { r.EvidenceHash = ev.InputFingerprint }, false), nil},
		"reduction without contract": {mk(pairPurposeProtective, basketDirectionReduce, func(r *pairActionRequest) { r.ContractFingerprint = "" }, false), nil},
		"native without marker":      {mk(pairPurposeKillSwitch, basketDirectionReduce, nil, true), nil},
		"native marker without note": {mk(pairPurposeOperator, basketDirectionReduce, func(r *pairActionRequest) { r.NativeBooked = true }, false), nil},
		"version one":                {mk(pairPurposeProtective, basketDirectionReduce, func(r *pairActionRequest) { r.V = 1 }, false), nil},
	}
	for name, tc := range refused {
		t.Run(name, func(t *testing.T) {
			var intent *entryIntent
			if tc.a.Direction == basketDirectionIncrease {
				intent = &entryIntent{IntentID: "probe-" + name, Partition: rt.Partition, StrategyID: pairTestHL, Status: entryIntentReserved, OwnerMode: pairGroupMode, DecisionKind: entryDecisionKindQuote, QuoteEventID: ev.ID()}
			}
			if err := e.store.ReservePairAction(rt, tc.a, intent, tc.use); err == nil {
				t.Fatal("the reservation accepted a request that breaks its purpose rule")
			}
		})
	}
	native := mk(pairPurposeKillSwitch, basketDirectionReduce, func(r *pairActionRequest) { r.NativeBooked = true }, true)
	native.Direction, native.OrderSide, native.PositionSide = basketDirectionReduce, "sell", entrySideLong
	native.RequestJSON = marshalEntryJSON(func() pairActionRequest {
		var r pairActionRequest
		_ = json.Unmarshal([]byte(native.RequestJSON), &r)
		r.OrderSide, r.PositionSide = "sell", entrySideLong
		return r
	}())
	if err := e.store.ReservePairAction(rt, native, nil, nil); err != nil {
		t.Fatalf("a native-booked reduction reserves with no pair evidence: %v", err)
	}
}

func TestBasketPairCollectionNativeReductionReservesWithoutEvidence(t *testing.T) {
	var sent int
	closers := &pairNativeClosers{hl: func(string, *float64, []int64) (*HyperliquidCloseResult, error) {
		sent++
		return &HyperliquidCloseResult{Close: &HyperliquidClose{Fill: &HyperliquidCloseFill{TotalSz: 0.06, AvgPx: 60000}}}, nil
	}}
	e := newPairLiveShapedEnv(t, closers)
	e.runner.respond = func(r pairPolicyRequest) (pairPolicyResult, error) {
		if r.Lifecycle == pairLifecycleMatched {
			return pairHoldResult(r, "keep"), nil
		}
		return pairEnterResult(r, 0.06), nil
	}
	fill := func(px float64, cur string) basketVenueOutcome {
		return pairFee(basketVenueOutcome{Outcome: basketVenueFilled, CumQty: 0.06, CumNotional: 0.06 * px, Terminal: true}, 1.5, cur, true)
	}
	pairOpenVenue(e, pairTestOKX, fill(59800, "USDT"))
	e.slotStep(0)
	pairOpenVenue(e, pairTestHL, fill(60200, "USDC"))
	e.slotStep(1)
	e.assertBooks("matched", 0.06, -0.06)
	closer := e.bc.pairKillSwitchHLCloser(closers.hl)
	if _, err := closer("BTC", nil, nil); err != nil {
		t.Fatal(err)
	}
	ks := e.actionsByPurpose(pairPurposeKillSwitch)
	if sent != 1 || len(ks) != 1 {
		t.Fatalf("the native close must run once with one saved pair action, got %d sends and %d actions", sent, len(ks))
	}
	req := pairRequestOf(t, ks[0])
	if req["v"].(float64) != 2 || req["native_booked"] != true || req["reduce_only"] != true || req["evidence_hash"] != nil || req["quote_event_id"] != nil {
		t.Fatalf("the native reduction request is wrong: %s", ks[0].RequestJSON)
	}
	for _, k := range e.rt().Pair.Progress.Alerts {
		if strings.HasPrefix(k, "native-unrecorded") {
			t.Fatalf("a native reduction without pair evidence raised %s", k)
		}
	}
}

func TestBasketPairCollectionVersionOneRequestRecoversUnchanged(t *testing.T) {
	e := newPairTestEnv(t, nil)
	pairEnterHoldRunner(e, 0.06)
	pairOpenVenue(e, pairTestOKX, basketVenueOutcome{Outcome: basketVenueUnknown, Detail: "disconnect"})
	e.slotStep(0)
	a := e.actionsByPurpose(pairPurposeEntry)[0]
	v1 := strings.Replace(a.RequestJSON, `"v":2`, `"v":1`, 1)
	var m map[string]interface{}
	_ = json.Unmarshal([]byte(v1), &m)
	for _, k := range []string{"contract_fingerprint", "conversions", "capability_hash", "evidence_hash"} {
		delete(m, k)
	}
	b, _ := json.Marshal(m)
	v1 = string(b)
	e.store.Close()
	db, err := OpenStateDB(e.cfg.DBFile)
	if err != nil {
		t.Fatal(err)
	}
	tx, err := db.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	for _, step := range []struct {
		q    string
		args []interface{}
	}{
		{`CREATE TEMP TABLE old_action AS SELECT * FROM basket_member_actions WHERE action_id = ?`, []interface{}{a.ActionID}},
		{`UPDATE old_action SET request_json = ?`, []interface{}{v1}},
		{`DELETE FROM basket_member_actions WHERE action_id = ?`, []interface{}{a.ActionID}},
		{`INSERT INTO basket_member_actions SELECT * FROM old_action`, nil},
		{`DROP TABLE old_action`, nil},
	} {
		if _, err := tx.Exec(step.q, step.args...); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	db.Close()
	e.restart()
	a = e.rt().Actions[a.ActionID]
	pairQueryVenue(e, a, basketVenueOutcome{Outcome: basketVenueFilled, CumQty: 0.06, CumNotional: 0.06 * 59800, Terminal: true})
	e.step(pairTestStart().Add(2 * time.Second))
	var stored, status string
	if err := e.db().db.QueryRow(`SELECT request_json, status FROM basket_member_actions WHERE action_id = ?`, a.ActionID).Scan(&stored, &status); err != nil {
		t.Fatal(err)
	}
	if stored != v1 || status != basketActionCommitted {
		t.Fatalf("a saved version 1 request must settle unchanged, got %s %s", status, stored)
	}
	if len(e.venues.okx.submits) != 1 {
		t.Fatalf("a saved version 1 request must never be resent, got %d submits", len(e.venues.okx.submits))
	}
	e.assertBooks("version 1 recovery", 0, -0.06)
}

func TestBasketPairCollectionBoundCrossing(t *testing.T) {
	e := newPairTestEnv(t, nil)
	rb := e.slot(pairTestStart(), "in-0")
	env, why := verifyPairEnvelope(e.rt(), rb)
	if why != "" {
		t.Fatal(why)
	}
	for _, tc := range []struct {
		member, side string
		bound        float64
		want         bool
	}{
		{pairTestOKX, "buy", 60100.1, true}, {pairTestOKX, "buy", 60100.05, false}, {pairTestOKX, "sell", 60099.9, true}, {pairTestOKX, "sell", 60099.95, false},
		{pairTestHL, "buy", 60001, true}, {pairTestHL, "sell", 59999.5, false}, {pairTestHL, "hold", 60001, false}, {"none", "buy", 1e9, false},
	} {
		if got := pairBoundCrosses(env, tc.member, tc.side, tc.bound); got != tc.want {
			t.Fatalf("%s %s %v crosses=%v, want %v", tc.member, tc.side, tc.bound, got, tc.want)
		}
	}
}

func TestBasketPairCollectionQuoteReaderProbeRegistration(t *testing.T) {
	var calls []string
	prev := probeOneCheckScriptFn
	probeOneCheckScriptFn = func(script string, argv []string) error {
		if script == pairQuoteReaderScript {
			calls = append(calls, strings.Join(argv, " "))
		}
		return nil
	}
	defer func() { probeOneCheckScriptFn = prev }()
	cfg := &Config{Strategies: []StrategyConfig{
		{ID: "hl-a", Platform: "hyperliquid", Type: "perps", Script: hyperliquidCheckScript},
		{ID: "okx-swap", Platform: "okx", Type: "perps", Script: okxCheckScriptPath, Args: []string{"sma_crossover", "BTC", "1h", "--inst-type=swap"}},
	}}
	if err := probeCheckScripts(cfg); err != nil {
		t.Fatal(err)
	}
	want := []string{strings.Join(pairQuoteHLPublicProbeArgv, " "), strings.Join(pairQuoteHLArchiveProbeArgv, " "), strings.Join(pairQuoteOKXPublicProbeArgv, " ")}
	if strings.Join(calls, "|") != strings.Join(want, "|") {
		t.Fatalf("both providers' quote reader probes must be registered once: %v", calls)
	}
	for _, argv := range [][]string{pairQuoteHLPublicProbeArgv, pairQuoteOKXPublicProbeArgv, pairQuoteHLArchiveProbeArgv} {
		for _, flag := range []string{"--provider=", "--source=", "--instrument=", "--depth="} {
			if !strings.Contains(strings.Join(argv, " "), flag) {
				t.Fatalf("probe argv %v must carry runtime flag %s", argv, flag)
			}
		}
	}
}

func TestBasketPairCollectionBoundedLauncher(t *testing.T) {
	sh := func(script string, limits pythonOutputLimits, timeout time.Duration) ([]byte, []byte, error, time.Duration) {
		start := time.Now()
		out, errOut, err := spawnPythonProcessRawLimited(context.Background(), "/bin/sh", []string{"-c", script}, nil, timeout, nil, limits)
		return out, errOut, err, time.Since(start)
	}
	out, _, err, _ := sh(`head -c 100000 /dev/zero | tr '\0' 'x'; sleep 30`, pythonOutputLimits{Stdout: 1000, Stderr: 1000}, 0)
	var overflow *pythonOutputOverflowError
	if !errors.As(err, &overflow) || overflow.Stream != "stdout" || len(out) != 1000 {
		t.Fatalf("stdout overflow must cancel with a distinct failure, got %v (%d bytes)", err, len(out))
	}
	_, errOut, err, _ := sh(`head -c 100000 /dev/zero | tr '\0' 'y' >&2; sleep 30`, pythonOutputLimits{Stdout: 1000, Stderr: 1000}, 0)
	if !errors.As(err, &overflow) || overflow.Stream != "stderr" || len(errOut) != 1000 {
		t.Fatalf("stderr overflow must be a child failure, got %v", err)
	}
	marker := filepath.Join(t.TempDir(), "late")
	_, _, err, took := sh(`(sleep 3; echo late > `+marker+`) & sleep 30`, pythonOutputLimits{Stdout: 1000, Stderr: 1000}, 500*time.Millisecond)
	if err == nil || took > pythonPipeWaitDelay+3*time.Second {
		t.Fatalf("a cancelled child and its descendants must end within the cleanup bound, got %v after %s", err, took)
	}
	time.Sleep(3500 * time.Millisecond)
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("a descendant of the cancelled process group kept running")
	}
	out, _, err, _ = sh(`printf 'ok'`, pythonOutputLimits{Stdout: 2, Stderr: 2}, 0)
	if err != nil || string(out) != "ok" {
		t.Fatalf("output at the limit must pass, got %v %q", err, out)
	}
	out, _, err, _ = sh(`head -c 5000 /dev/zero | tr '\0' 'z'`, pythonOutputLimits{}, 0)
	if err != nil || len(out) != 5000 {
		t.Fatalf("existing callers keep unbounded output, got %v %d", err, len(out))
	}
	var held []func()
	for i := 0; i < cap(pythonSemaphore); i++ {
		release, err := acquirePythonSlot(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		held = append(held, release)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	start := time.Now()
	if _, err := acquirePythonSlot(ctx); !errors.Is(err, context.DeadlineExceeded) || time.Since(start) > 2*time.Second {
		t.Fatalf("a saturated admission wait must observe cancellation, got %v after %s", err, time.Since(start))
	}
	for _, release := range held {
		release()
	}
	release, err := acquirePythonSlot(context.Background())
	if err != nil {
		t.Fatalf("permits must return after release: %v", err)
	}
	release()
}

func TestBasketPairCollectionHoldsUnderSaturatedAdmission(t *testing.T) {
	e := pairCollectEnv(t, pairSetMember(0, "source", "public_snapshot"))
	e.adm.deps.Quotes = &pairQuoteCollector{sources: map[string]string{pairTestHL: pairSourcePublicSnapshot, pairTestOKX: pairSourcePublicSnapshot},
		readers: map[string]pairMemberReader{pairTestHL: pairPublicReader{}, pairTestOKX: pairPublicReader{}}, limit: 1 << 20}
	var held []func()
	for i := 0; i < cap(pythonSemaphore); i++ {
		release, err := acquirePythonSlot(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		held = append(held, release)
	}
	defer func() {
		for _, release := range held {
			release()
		}
	}()
	prev := processEgress
	processEgress = &egressRuntime{}
	t.Cleanup(func() { processEgress = prev })
	t.Setenv("GO_TRADER_EGRESS_PROXY", "")
	start := time.Now()
	e.step(pairTestStart().Add(time.Second))
	if time.Since(start) > 10*time.Second {
		t.Fatalf("collection under saturated admission must end at its budget, took %s", time.Since(start))
	}
	if status, hold, _, _ := e.slotRow(pairTestStart().UnixMilli()); status != basketRebalanceHeld || !strings.Contains(hold, "waiting for a script slot") {
		t.Fatalf("a saturated admission must hold the slot, got %s %q", status, hold)
	}
}

func TestBasketPairCollectionPublicReadRefusesUnattestedProxy(t *testing.T) {
	prev := processEgress
	processEgress = &egressRuntime{}
	t.Cleanup(func() { processEgress = prev })
	t.Setenv("GO_TRADER_EGRESS_PROXY", "http://user:secret@proxy.example:3128")
	t.Setenv("GO_TRADER_EGRESS_DEPLOYMENT", "nosuchdeployment")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := pairPublicEgressReady(ctx, nil); err == nil || strings.Contains(err.Error(), "secret") {
		t.Fatalf("proxy mode without enforcement must refuse public reads without leaking credentials, got %v", err)
	}
	processEgress = &egressRuntime{}
	if err := establishProcessEgress(ctx, nil, false); err != nil {
		t.Fatal(err)
	}
	if err := pairPublicEgressReady(ctx, nil); err == nil || !strings.Contains(err.Error(), "attestation") {
		t.Fatalf("an earlier unattested resolution must not authorize public reads, got %v", err)
	}
	processEgress.mu.Lock()
	processEgress.attestError = "attestation lost"
	processEgress.mu.Unlock()
	if err := pairPublicEgressReady(ctx, nil); err == nil {
		t.Fatal("a lost attestation must refuse public reads")
	}
	processEgress = &egressRuntime{}
	t.Setenv("GO_TRADER_EGRESS_PROXY", "")
	if err := pairPublicEgressReady(ctx, nil); err != nil {
		t.Fatalf("direct mode with no proxy policy permits public reads: %v", err)
	}
	if err := pairPublicEgressReady(ctx, &EgressConfig{ProxyRequired: true}); err == nil {
		t.Fatal("a required proxy that is absent must refuse public reads")
	}
	_ = base64.StdEncoding
}

func TestBasketPairCollectionReferenceProvesItsSideDepth(t *testing.T) {
	e := pairCollectEnv(t, nil)
	e.quotes.mu.Lock()
	e.quotes.okxBids = [][2]string{{"60099.9", "3"}, {"60099.8", "4"}}
	e.quotes.mu.Unlock()
	rt := e.rt()
	m, _ := rt.Contract.pairMember(pairTestOKX)
	nowMs := pairTestStart().UnixMilli()
	ref := func(side string) (pairObservation, error) {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		return e.quotes.collector.Reference(ctx, pairReferenceRequest{Partition: rt.Partition, PairID: rt.Contract.BasketID, ContractFingerprint: rt.Contract.Fingerprint(),
			Member: m, Side: side, BookNeed: rt.Contract.Pair.BookNeed, SourceContract: pairContractOKXPublic, StartMs: nowMs, DeadlineMs: nowMs + 2000})
	}
	buy, err := ref("buy")
	if err != nil {
		t.Fatalf("a buy reference needs depth on the asks only: %v", err)
	}
	if len(buy.NativeBids) != 2 || len(buy.NativeAsks) != 5 || buy.NormalizationID != "" {
		t.Fatalf("a reference returns both sides with native levels only: %+v", buy)
	}
	if _, err := ref("sell"); err == nil || !strings.Contains(err.Error(), "below the required depth") {
		t.Fatalf("a sell reference must prove depth on the bids, got %v", err)
	}
	if _, err := ref("hold"); err == nil {
		t.Fatal("a reference needs a buy or sell side")
	}
	if err := pairCheckReference(rt, m, &buy, "buy", nowMs); err != nil {
		t.Fatalf("the buy reference must verify for a buy: %v", err)
	}
	if err := pairCheckReference(rt, m, &buy, "sell", nowMs); err == nil {
		t.Fatal("the buy reference must not prove sell depth")
	}
	if err := pairCheckReference(rt, m, &buy, "buy", buy.FirstReceiptMs+5001); err == nil {
		t.Fatal("a reference older than quote_age_ms must be refused")
	}
	stale := buy
	stale.FirstReceiptMs++
	if err := pairCheckReference(rt, m, &stale, "buy", nowMs); err == nil {
		t.Fatal("a reference whose receipt disagrees with its payload must be refused")
	}
}

func TestBasketPairCollectionSealedCoverageFailuresHold(t *testing.T) {
	cases := []struct {
		name string
		edit func(b *marketSnapshotBook)
		want string
	}{
		{"evicted prefix", func(b *marketSnapshotBook) { b.CoverageFromMs = b.CutoffMs - 30000 }, "coverage is incomplete"},
		{"disconnect marker", func(b *marketSnapshotBook) {
			b.Markers = append(b.Markers, feedBookMarker{Seq: 999, AtMs: b.CutoffMs - 20000, Reason: feedBookMarkerDisconnected, Session: 1})
		}, "coverage is incomplete"},
		{"silent stretch", func(b *marketSnapshotBook) {
			var kept []feedBookObservation
			for _, o := range b.Obs {
				if o.RecvMs < b.CutoffMs-40000 || o.RecvMs > b.CutoffMs-25000 {
					kept = append(kept, o)
				}
			}
			b.Obs = kept
		}, "coverage is incomplete"},
		{"seal shallower than the need", func(b *marketSnapshotBook) { b.Need.Depth = 3 }, "seal depth 3"},
		{"seal window shorter than the need", func(b *marketSnapshotBook) { b.Need.WindowMs = 30000 }, "seal window"},
		{"no book in the seal", nil, "is not in the sealed snapshot"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := pairCollectEnv(t, nil)
			at := pairTestStart().Add(time.Second)
			rt := e.rt()
			snap := e.quotes.snapshot(rt.Contract.pairMembersSorted(), rt.Contract.Pair.BookNeed, at.UnixMilli()-50, "feed-x")
			key := l2BookKeyFor("BTC")
			if tc.edit == nil {
				delete(snap.books, key)
			} else {
				tc.edit(snap.books[key])
			}
			e.now = at
			e.bc.beginCycle()
			e.observe("")
			e.bc.step(basketCycleInputs{Now: at, Once: true, PairSnapshotFor: func(*basketRuntime, time.Time) *marketSnapshot { return snap }, PairGeneration: e.feedGeneration})
			status, hold, _, evidence := e.slotRow(pairTestStart().UnixMilli())
			if status != basketRebalanceHeld || evidence != "" || !strings.Contains(hold, tc.want) {
				t.Fatalf("a failed seal must hold without a public substitute, got %s %q", status, hold)
			}
		})
	}
}

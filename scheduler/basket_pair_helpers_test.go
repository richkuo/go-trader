package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

const pairTestPolicy = "pair_scripted_fixture"
const pairTestID = "btc-xvenue"
const pairTestHL = "pair-hl-btc"
const pairTestOKX = "pair-okx-btc"

func registerPairTestPolicy(t *testing.T) {
	t.Helper()
	prevS, hadS := pairPolicyStrategies[pairTestPolicy]
	prevK, hadK := pairPolicyParamKeys[pairTestPolicy]
	pairPolicyStrategies[pairTestPolicy] = true
	pairPolicyParamKeys[pairTestPolicy] = []string{"entry_bps"}
	t.Cleanup(func() {
		if hadS {
			pairPolicyStrategies[pairTestPolicy] = prevS
		} else {
			delete(pairPolicyStrategies, pairTestPolicy)
		}
		if hadK {
			pairPolicyParamKeys[pairTestPolicy] = prevK
		} else {
			delete(pairPolicyParamKeys, pairTestPolicy)
		}
	})
}

func pairTestLimits() map[string]interface{} {
	return map[string]interface{}{
		"gross_budget_usd":             12000,
		"hyperliquid_notional_cap_usd": 7000,
		"okx_notional_cap_usd":         7000,
		"unmatched_cap_usd":            4000,
		"decision_cadence_ms":          60000,
		"acceptance_deadline_ms":       30000,
		"recovery_age_ms":              600000,
		"max_hold_age_ms":              3600000,
		"retry_initial_ms":             5000,
		"retry_max_ms":                 40000,
		"quote_age_ms":                 5000,
		"collection_timeout_ms":        2000,
		"receive_skew_ms":              250,
		"envelope_max_bytes":           1048576,
		"protective_width_bps":         50,
	}
}

func pairTestCostPolicy() map[string]interface{} {
	return map[string]interface{}{"taker_fee_pct": 0.00045, "slippage_pct": 0.0005, "fill_role": "taker_all", "spread": "inside_slippage"}
}

func pairTestEvidenceBytes(t *testing.T, doc []byte, mutate func(d *pairEvidenceDocument)) []byte {
	t.Helper()
	var root struct {
		Strategies []struct {
			ID          string   `json:"id"`
			Platform    string   `json:"platform"`
			Args        []string `json:"args"`
			Leverage    float64  `json:"leverage"`
			PaperSource string   `json:"paper_source"`
		} `json:"strategies"`
		Pairs []struct {
			Members []struct {
				StrategyID string          `json:"strategy_id"`
				AccountKey string          `json:"account_key"`
				Instrument string          `json:"instrument"`
				Meta       json.RawMessage `json:"meta"`
			} `json:"members"`
		} `json:"pairs"`
	}
	_ = json.Unmarshal(doc, &root)
	d := pairEvidenceDocument{V: pairEvidenceVersion, Capabilities: []pairCapabilityEvidence{}, Accounts: []pairAccountEvidence{}, Conversions: []pairConversionEvidence{}}
	stamp := func(k pairEvidenceKey, id, provenance string) pairEvidenceStamp {
		return pairEvidenceStamp{V: pairEvidenceVersion, Key: k, EvidenceID: id, ObservedMs: 1_700_000_000_000, FirstReceiptMs: 1_700_000_000_500,
			ValidFromMs: 1_700_000_000_500, ValidThroughMs: 4_102_444_800_000, Provenance: provenance}
	}
	if len(root.Pairs) > 0 {
		seen := map[string]bool{}
		for _, m := range root.Pairs[0].Members {
			var meta pairInstrumentMeta
			if json.Unmarshal(m.Meta, &meta) != nil || seen[m.StrategyID] {
				continue
			}
			seen[m.StrategyID] = true
			partition, provider, leverage := "paper", "", 1.0
			for _, sc := range root.Strategies {
				if sc.ID != m.StrategyID {
					continue
				}
				provider = pairProviderPlatforms[sc.Platform]
				if sc.Leverage > 0 {
					leverage = sc.Leverage
				}
				for _, a := range sc.Args {
					if a == "--mode=live" {
						partition = "live"
					}
				}
				if sc.PaperSource != "" {
					partition = "paper:" + sc.PaperSource
				}
			}
			if provider == "" {
				continue
			}
			member := pairContractMember{MemberID: m.StrategyID, Provider: provider, AccountKey: m.AccountKey, Instrument: m.Instrument, Meta: meta, Leverage: leverage}
			k := pairEvidenceKey{Partition: partition, Provider: provider, AccountKey: m.AccountKey, Instrument: m.Instrument, MemberID: m.StrategyID}
			wire, err := pairExpectedWireMeta(member)
			if err != nil {
				continue
			}
			d.Capabilities = append(d.Capabilities, pairCapabilityEvidence{pairEvidenceStamp: stamp(k, "cap-"+m.StrategyID, pairDerivationIDs[provider]), Meta: wire, MetadataGeneration: 1})
			lev, _ := pairDecimalFromFloat(leverage)
			d.Accounts = append(d.Accounts, pairAccountEvidence{pairEvidenceStamp: stamp(k, "acct-"+m.StrategyID, "research-schedule:fixture"),
				PositionMode: meta.PositionMode, MarginMode: meta.MarginMode, Leverage: lev, CollateralCurrency: meta.SettlementCurrency,
				Equity: "6000", FreeCollateral: "6000", MarginUsed: "0"})
			for _, cur := range pairRequiredConversionCurrencies(member) {
				d.Conversions = append(d.Conversions, pairConversionEvidence{pairEvidenceStamp: stamp(k, "conv-"+m.StrategyID+"-"+cur, "research-schedule:fixture"), Currency: cur, RateUSD: "1"})
			}
		}
	}
	if mutate != nil {
		mutate(&d)
	}
	for i := range d.Capabilities {
		d.Capabilities[i], _ = d.Capabilities[i].sealed()
	}
	for i := range d.Accounts {
		d.Accounts[i], _ = d.Accounts[i].sealed()
	}
	for i := range d.Conversions {
		d.Conversions[i], _ = d.Conversions[i].sealed()
	}
	blob, err := pairEvidenceDocumentBytes(d)
	if err != nil {
		t.Fatalf("pair evidence fixture: %v", err)
	}
	return blob
}

func pairTestEvidence(t *testing.T, doc []byte, mutate func(d *pairEvidenceDocument)) *pairFrozenEvidenceSource {
	t.Helper()
	blob := pairTestEvidenceBytes(t, doc, mutate)
	src, err := newPairFrozenEvidenceSource(blob, sha256Hex(blob))
	if err != nil {
		t.Fatalf("pair evidence fixture source: %v", err)
	}
	return src
}

func pairTestDeps(t *testing.T, doc []byte, venues *pairTestVenues) pairResearchDeps {
	t.Helper()
	return pairResearchDeps{VenueFor: venues.factory(), Evidence: pairTestEvidence(t, doc, nil), Quotes: newPairFixtureQuotes(t, doc).collector, Funding: pairTestFunding(t, doc, nil)}
}

const (
	pairTestFundingFromMs    = 1_700_000_000_000
	pairTestFundingThroughMs = 4_102_444_800_000
)

func pairTestMemberKeys(doc []byte) []pairEvidenceKey {
	var root struct {
		Strategies []struct {
			ID          string   `json:"id"`
			Platform    string   `json:"platform"`
			Args        []string `json:"args"`
			PaperSource string   `json:"paper_source"`
		} `json:"strategies"`
		Pairs []struct {
			Members []struct {
				StrategyID string `json:"strategy_id"`
				AccountKey string `json:"account_key"`
				Instrument string `json:"instrument"`
			} `json:"members"`
		} `json:"pairs"`
	}
	_ = json.Unmarshal(doc, &root)
	var out []pairEvidenceKey
	if len(root.Pairs) == 0 {
		return out
	}
	seen := map[string]bool{}
	for _, m := range root.Pairs[0].Members {
		if seen[m.StrategyID] {
			continue
		}
		seen[m.StrategyID] = true
		partition, provider := "paper", ""
		for _, sc := range root.Strategies {
			if sc.ID != m.StrategyID {
				continue
			}
			provider = pairProviderPlatforms[sc.Platform]
			for _, a := range sc.Args {
				if a == "--mode=live" {
					partition = "live"
				}
			}
			if sc.PaperSource != "" {
				partition = "paper:" + sc.PaperSource
			}
		}
		if provider != "" {
			out = append(out, pairEvidenceKey{Partition: partition, Provider: provider, AccountKey: m.AccountKey, Instrument: m.Instrument, MemberID: m.StrategyID})
		}
	}
	return out
}

func pairTestFundingDocument(doc []byte) pairFundingDocument {
	d := pairFundingDocument{V: pairFundingDocumentVersion, SourceID: "fixture-funding", Rates: []pairFundingRate{}, Prices: []pairFundingPrice{}, Coverage: []pairFundingCoverageRecord{}}
	for _, k := range pairTestMemberKeys(doc) {
		d.Coverage = append(d.Coverage, pairFundingCoverageRecord{V: pairFundingRecordVersion, Key: k, EvidenceID: "cov-" + k.MemberID,
			RequestedFromMs: pairTestFundingFromMs, RequestedThroughMs: pairTestFundingThroughMs,
			Proven: []pairFundingInterval{{FromMs: pairTestFundingFromMs, ThroughMs: pairTestFundingThroughMs}}, Gaps: []pairCoverageGap{},
			VisibleMs: pairTestFundingFromMs + 500, Provenance: "research-funding:fixture"})
	}
	return d
}

func pairTestFundingSeal(t *testing.T, d *pairFundingDocument) []byte {
	t.Helper()
	for i := range d.Rates {
		d.Rates[i], _ = d.Rates[i].sealed()
	}
	for i := range d.Prices {
		d.Prices[i], _ = d.Prices[i].sealed()
	}
	for i := range d.Coverage {
		d.Coverage[i], _ = d.Coverage[i].sealed()
	}
	blob, err := pairFundingDocumentBytes(*d)
	if err != nil {
		t.Fatalf("pair funding fixture: %v", err)
	}
	return blob
}

func pairTestFundingBytes(t *testing.T, doc []byte, mutate func(d *pairFundingDocument)) []byte {
	t.Helper()
	d := pairTestFundingDocument(doc)
	if mutate != nil {
		mutate(&d)
	}
	return pairTestFundingSeal(t, &d)
}

func pairTestFunding(t *testing.T, doc []byte, mutate func(d *pairFundingDocument)) *pairFrozenFundingSource {
	t.Helper()
	blob := pairTestFundingBytes(t, doc, mutate)
	src, err := newPairFrozenFundingSource(blob, sha256Hex(blob))
	if err != nil {
		t.Fatalf("pair funding fixture source: %v", err)
	}
	return src
}

type pairFixtureQuotes struct {
	mu           sync.Mutex
	enabled      bool
	err          error
	refDisabled  bool
	refErr       error
	refReads     int
	receiptLagMs int64
	reads        int
	block        chan struct{}
	onRead       func(r pairMemberRead)
	okxBids      [][2]string
	okxAsks      [][2]string
	hlBids       [][2]string
	hlAsks       [][2]string
	collector    *pairQuoteCollector
}

func newPairFixtureQuotes(t *testing.T, doc []byte) *pairFixtureQuotes {
	t.Helper()
	var root struct {
		Pairs []struct {
			Members []struct {
				StrategyID string `json:"strategy_id"`
				Source     string `json:"source"`
			} `json:"members"`
		} `json:"pairs"`
	}
	_ = json.Unmarshal(doc, &root)
	q := &pairFixtureQuotes{receiptLagMs: 200,
		okxBids: [][2]string{{"60099.9", "3"}, {"60099.8", "4"}, {"60099.7", "5"}, {"60099.6", "6"}, {"60099.5", "7"}, {"60099.4", "8"}},
		okxAsks: [][2]string{{"60100.1", "3"}, {"60100.2", "4"}, {"60100.3", "5"}, {"60100.4", "6"}, {"60100.5", "7"}, {"60100.6", "8"}},
		hlBids:  [][2]string{{"59999", "0.5"}, {"59998", "0.6"}, {"59997", "0.7"}, {"59996", "0.8"}, {"59995", "0.9"}, {"59994", "1"}},
		hlAsks:  [][2]string{{"60001", "0.5"}, {"60002", "0.6"}, {"60003", "0.7"}, {"60004", "0.8"}, {"60005", "0.9"}, {"60006", "1"}}}
	c := &pairQuoteCollector{sources: map[string]string{}, readers: map[string]pairMemberReader{}, limit: pairMaxEnvelopeBytes}
	if len(root.Pairs) > 0 {
		for _, m := range root.Pairs[0].Members {
			c.sources[m.StrategyID] = m.Source
			if m.Source == pairSourceSealedFeed {
				c.readers[m.StrategyID] = pairSealedReader{}
			} else {
				c.readers[m.StrategyID] = q
			}
		}
	}
	q.collector = c
	return q
}

func (q *pairFixtureQuotes) enable() {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.enabled, q.err = true, nil
}

func (q *pairFixtureQuotes) readQuote(ctx context.Context, r pairMemberRead) (pairObservation, error) {
	q.mu.Lock()
	q.reads++
	enabled, err, lag, block, onRead := q.enabled, q.err, q.receiptLagMs, q.block, q.onRead
	if r.Side != "" {
		q.refReads++
		enabled, err = !q.refDisabled, q.refErr
	}
	okxBids, okxAsks, hlBids, hlAsks := q.okxBids, q.okxAsks, q.hlBids, q.hlAsks
	q.mu.Unlock()
	if onRead != nil {
		onRead(r)
	}
	if block != nil {
		select {
		case <-block:
		case <-ctx.Done():
			return pairObservation{}, ctx.Err()
		}
	}
	if err != nil {
		return pairObservation{}, err
	}
	if !enabled {
		return pairObservation{}, fmt.Errorf("no fixture quote is scripted for %s", r.Member.MemberID)
	}
	receipt := r.StartMs - lag
	var payload []byte
	switch {
	case r.Member.Provider == entryProviderOKX:
		payload = pairFixtureOKXPayload(r.Member.Instrument, r.BookNeed.Depth, receipt-20, okxBids, okxAsks)
	case r.Member.Source == pairSourcePublicSnapshot:
		payload = pairFixtureHLPublicPayload(r.Member.Instrument, r.BookNeed.Depth, receipt-20, hlBids, hlAsks)
	case r.Member.Source == pairSourceArchive:
		receipt = 0
		payload = pairFixtureArchivePayload(r.Member, r.BookNeed, r.StartMs-r.Member.ModeledDeliveryMs, hlBids, hlAsks)
	default:
		return pairObservation{}, fmt.Errorf("no fixture reader for %s", r.Member.Source)
	}
	return pairObservationFromPayloadSide(r.Member, r.Partition.String(), r.BookNeed, payload, receipt, 0, r.Side)
}

func pairFixtureCanonical(v interface{}) []byte {
	raw, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	tree, err := pairStrictParse(raw)
	if err != nil {
		panic(err)
	}
	var buf bytes.Buffer
	if err := pairWriteCanonical(&buf, tree); err != nil {
		panic(err)
	}
	return buf.Bytes()
}

func pairFixtureOKXPayload(inst string, depth int, venueMs int64, bids, asks [][2]string) []byte {
	side := func(levels [][2]string) []interface{} {
		out := []interface{}{}
		for i := 0; i < depth && i < len(levels); i++ {
			out = append(out, []interface{}{levels[i][0], levels[i][1], "0", "2"})
		}
		return out
	}
	return pairFixtureCanonical(map[string]interface{}{
		"request":  map[string]interface{}{"instId": inst, "sz": fmt.Sprint(depth)},
		"response": map[string]interface{}{"code": "0", "msg": "", "data": []interface{}{map[string]interface{}{"bids": side(bids), "asks": side(asks), "ts": fmt.Sprint(venueMs)}}},
	})
}

func pairFixtureHLPublicPayload(coin string, depth int, venueMs int64, bids, asks [][2]string) []byte {
	side := func(levels [][2]string) []interface{} {
		out := []interface{}{}
		for i := 0; i < depth && i < len(levels); i++ {
			out = append(out, map[string]interface{}{"px": levels[i][0], "sz": levels[i][1], "n": 1})
		}
		return out
	}
	return pairFixtureCanonical(map[string]interface{}{
		"request":  map[string]interface{}{"coin": coin, "type": "l2Book"},
		"response": map[string]interface{}{"coin": coin, "time": venueMs, "levels": []interface{}{side(bids), side(asks)}},
	})
}

func pairFixtureBookLevels(levels [][2]string, depth int) []feedBookLevel {
	out := []feedBookLevel{}
	for i := 0; i < depth && i < len(levels); i++ {
		out = append(out, feedBookLevel{Px: levels[i][0], Sz: levels[i][1], N: 1})
	}
	return out
}

func pairFixtureBook(coin string, cutoffMs int64, need pairBookNeed, bids, asks [][2]string) *marketSnapshotBook {
	key := l2BookKeyFor(coin)
	b := &marketSnapshotBook{Key: key, Need: contractBookNeed(need.WindowMs, need.Capacity, need.Depth), Instance: "fixture-feed", CutoffMs: cutoffMs,
		CoverageFromMs: cutoffMs - 2*need.WindowMs, Obs: []feedBookObservation{}, Markers: []feedBookMarker{}}
	var recvs []int64
	for r := cutoffMs - 100; r >= cutoffMs-need.WindowMs; r -= 1000 {
		recvs = append(recvs, r)
	}
	sort.Slice(recvs, func(i, j int) bool { return recvs[i] < recvs[j] })
	for i, r := range recvs {
		bl, al := pairFixtureBookLevels(bids, need.Depth), pairFixtureBookLevels(asks, need.Depth)
		b.Obs = append(b.Obs, feedBookObservation{Seq: uint64(i + 1), EventMs: r - 50, RecvMs: r, Session: 1, Identity: bookIdentity(key, r-50, bl, al),
			BidDepth: len(bl), AskDepth: len(al), Bids: bl, Asks: al})
	}
	return b
}

func (q *pairFixtureQuotes) snapshot(members []pairContractMember, need pairBookNeed, cutoffMs int64, id string) *marketSnapshot {
	q.mu.Lock()
	bids, asks := q.hlBids, q.hlAsks
	q.mu.Unlock()
	snap := &marketSnapshot{EvaluationID: id, ConfigGeneration: 1, Deadline: time.UnixMilli(cutoffMs).UTC(), books: map[feedObservationKey]*marketSnapshotBook{}}
	for _, m := range members {
		if m.Source == pairSourceSealedFeed {
			b := pairFixtureBook(m.Instrument, cutoffMs, need, bids, asks)
			snap.books[b.Key] = b
		}
	}
	return snap
}

func pairFixtureArchivePayload(m pairContractMember, need pairBookNeed, sourceCutoffMs int64, bids, asks [][2]string) []byte {
	b := pairFixtureBook(m.Instrument, sourceCutoffMs, need, bids, asks)
	snap := &marketSnapshot{EvaluationID: "archive", Deadline: time.UnixMilli(sourceCutoffMs).UTC(), books: map[feedObservationKey]*marketSnapshotBook{b.Key: b}}
	p := snap.l2BookPayload(b.Key, b.Need)
	p.TimeSemantics, p.SnapshotID, p.Instance, p.FeedStatus = pairArchivePayloadTimeBasis, "", "hyperliquid-archive", ""
	return pairFixtureCanonical(p)
}

func (e *pairTestEnv) fixtureEnvelope(rt *basketRuntime, slotMs, cutoffMs int64, snapshotID string) pairEnvelope {
	e.t.Helper()
	members := rt.Contract.pairMembersSorted()
	need := rt.Contract.Pair.BookNeed
	start := slotMs
	deadline := start + rt.Pair.Limits.CollectionTimeoutMs
	snap := e.quotes.snapshot(members, need, start-50, snapshotID)
	sealed := false
	for _, m := range members {
		sealed = sealed || m.Source == pairSourceSealedFeed
	}
	gen := uint64(0)
	if !sealed {
		snapshotID = ""
	} else {
		gen = 1
	}
	e.quotes.mu.Lock()
	wasOn := e.quotes.enabled
	e.quotes.enabled = true
	e.quotes.mu.Unlock()
	obs, err := e.quotes.collector.Collect(context.Background(), pairCollectRequest{Partition: rt.Partition, PairID: rt.Contract.BasketID, ContractFingerprint: rt.Contract.Fingerprint(),
		SlotMs: slotMs, StartMs: start, DeadlineMs: deadline, Members: members, BookNeed: need, SourceContracts: pairSourceContractMap(members), Snapshot: snap, SnapshotID: snapshotID, Generation: gen})
	e.quotes.mu.Lock()
	e.quotes.enabled = wasOn
	e.quotes.mu.Unlock()
	if err != nil {
		e.t.Fatalf("fixture quotes: %v", err)
	}
	ev := e.adm.evidenceSource()
	var caps []pairCapabilityEvidence
	var accts []pairAccountEvidence
	var convs []pairConversionEvidence
	for _, m := range members {
		c, err := ev.Capability(context.Background(), m, cutoffMs)
		if err != nil {
			e.t.Fatal(err)
		}
		a, err := ev.Account(context.Background(), m, cutoffMs)
		if err != nil {
			e.t.Fatal(err)
		}
		cs, err := ev.Conversions(context.Background(), m, cutoffMs)
		if err != nil {
			e.t.Fatal(err)
		}
		caps, accts, convs = append(caps, c), append(accts, a), append(convs, cs...)
	}
	env, why := buildPairEnvelope(pairEnvelopeInputs{Partition: rt.Partition, PairID: rt.Contract.BasketID, ContractFingerprint: rt.Contract.Fingerprint(),
		SlotMs: slotMs, StartMs: start, DeadlineMs: deadline, CutoffMs: cutoffMs, Members: members, BookNeed: need, SourceContracts: pairSourceContractMap(members),
		Observations: obs, Capabilities: caps, Conversions: convs, Accounts: accts, SnapshotID: snapshotID, Generation: gen, Limits: rt.Pair.Limits})
	if why != "" {
		e.t.Fatalf("fixture envelope: %s", why)
	}
	return env
}

func pairTestDoc(t *testing.T, dir string, mutate func(root map[string]interface{})) []byte {
	t.Helper()
	root := map[string]interface{}{
		"config_version":   CurrentConfigVersion,
		"interval_seconds": 60,
		"db_file":          filepath.Join(dir, "state.db"),
		"log_dir":          filepath.Join(dir, "logs"),
		"strategies": []interface{}{
			map[string]interface{}{
				"id":             pairTestHL,
				"pair_id":        pairTestID,
				"type":           "perps",
				"platform":       "hyperliquid",
				"script":         "shared_scripts/check_hyperliquid.py",
				"args":           []interface{}{"pair_member", "BTC", "1h", "--mode=paper"},
				"close_strategy": map[string]interface{}{"name": "tiered_tp_pct", "params": map[string]interface{}{"tp_tiers": []interface{}{map[string]interface{}{"profit_pct": 0.5, "close_fraction": 1.0}}}},
				"direction":      "both",
				"capital":        6000,
				"leverage":       2,
				"margin_mode":    "cross",
			},
			map[string]interface{}{
				"id":             pairTestOKX,
				"pair_id":        pairTestID,
				"type":           "perps",
				"platform":       "okx",
				"script":         "shared_scripts/check_okx.py",
				"args":           []interface{}{"pair_member", "BTC", "1h", "--mode=paper", "--inst-type=swap"},
				"close_strategy": map[string]interface{}{"name": "atr_stop", "params": map[string]interface{}{"atr_mult": 3.0}},
				"direction":      "both",
				"capital":        6000,
				"leverage":       2,
			},
		},
		"pairs": []interface{}{map[string]interface{}{
			"id":         pairTestID,
			"underlying": "BTC",
			"policy":     pairTestPolicy,
			"params":     map[string]interface{}{"entry_bps": 5},
			"members": []interface{}{
				map[string]interface{}{
					"strategy_id":         pairTestHL,
					"account_key":         "synthetic:hl-research",
					"instrument":          "BTC",
					"source":              "sealed_feed",
					"modeled_delivery_ms": 0,
					"cost_policy":         pairTestCostPolicy(),
					"meta": map[string]interface{}{
						"native_unit": "base", "base_asset": "BTC", "contract_size": 1, "quote_currency": "USDC", "settlement_currency": "USDC", "price_currency": "USDC",
						"tick_step": 0.001, "lot_step": 0.001, "min_qty": 0.001, "min_notional": 10, "trading_status": "live", "linear": true,
						"position_mode": "one_way", "margin_mode": "cross", "max_leverage": 20, "initial_margin_rate": 0.05, "maintenance_margin_rate": 0.02,
						"price_rule": "hyperliquid_perps_v1", "sz_decimals": 3, "tick_step_text": "", "lot_step_text": "", "fee_currencies": []interface{}{"USDC"},
					},
				},
				map[string]interface{}{
					"strategy_id":         pairTestOKX,
					"account_key":         "synthetic:okx-research",
					"instrument":          "BTC-USDT-SWAP",
					"source":              "public_snapshot",
					"modeled_delivery_ms": 0,
					"cost_policy":         pairTestCostPolicy(),
					"meta": map[string]interface{}{
						"native_unit": "contract", "base_asset": "BTC", "contract_size": 0.01, "quote_currency": "USDT", "settlement_currency": "USDT", "price_currency": "USDT",
						"tick_step": 0.1, "lot_step": 1, "min_qty": 1, "min_notional": 0, "trading_status": "live", "linear": true,
						"position_mode": "net", "margin_mode": "cross", "max_leverage": 20, "initial_margin_rate": 0.05, "maintenance_margin_rate": 0.02,
						"price_rule": "fixed_tick_v1", "sz_decimals": 0, "tick_step_text": "0.1", "lot_step_text": "1", "fee_currencies": []interface{}{"USDT"},
					},
				},
			},
			"limits":    pairTestLimits(),
			"book_need": map[string]interface{}{"depth": 5, "window_ms": 60000, "capacity": 256},
		}},
	}
	if mutate != nil {
		mutate(root)
	}
	blob, err := json.MarshalIndent(root, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	return blob
}

func pairTestStrategy(root map[string]interface{}, id string) map[string]interface{} {
	for _, raw := range root["strategies"].([]interface{}) {
		sc := raw.(map[string]interface{})
		if sc["id"] == id {
			return sc
		}
	}
	return nil
}

func pairTestPair(root map[string]interface{}) map[string]interface{} {
	return root["pairs"].([]interface{})[0].(map[string]interface{})
}

func pairTestMemberEntry(root map[string]interface{}, i int) map[string]interface{} {
	return pairTestPair(root)["members"].([]interface{})[i].(map[string]interface{})
}

type pairScriptedVenue struct {
	mu        *sync.Mutex
	member    string
	provider  string
	account   string
	live      bool
	virtual   bool
	seq       *int
	fillMark  float64
	submits   []basketAction
	queries   map[string][]basketVenueOutcome
	outcomes  []basketVenueOutcome
	submitErr []error
	onSubmit  func(a *basketAction)
	onQuery   func(a *basketAction)
	queryCnt  map[string]int
	now       func() int64
	model     string
	ledger    map[string]float64
	adjust    float64
	position  func(m pairContractMember, nowMs int64) (pairVenuePosition, error)
	posReads  int
}

func (v *pairScriptedVenue) Name() string     { return "pair-scripted-" + v.provider }
func (v *pairScriptedVenue) LiveShaped() bool { return v.live }
func (v *pairScriptedVenue) Virtual() bool    { return v.virtual }
func (v *pairScriptedVenue) ExecutionModel() string {
	switch {
	case v.model != "":
		return v.model
	case v.live:
		return pairExecutionVenueConfirmed
	}
	return pairExecutionVirtualIOCAdverseMax
}
func (v *pairScriptedVenue) Provider() string { return v.provider }
func (v *pairScriptedVenue) AccountKey(RiskPartition) string {
	return v.account
}
func (v *pairScriptedVenue) NewClientOrderID() (string, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	*v.seq++
	return fmt.Sprintf("%s-%06d", v.provider, *v.seq), nil
}
func (v *pairScriptedVenue) venueBase() float64 {
	total := v.adjust
	for _, q := range v.ledger {
		total += q
	}
	return total
}

func (v *pairScriptedVenue) setVenueBase(qty float64) {
	v.mu.Lock()
	defer v.mu.Unlock()
	sum := 0.0
	for _, q := range v.ledger {
		sum += q
	}
	v.adjust = qty - sum
}

func (v *pairScriptedVenue) Position(ctx context.Context, m pairContractMember, nowMs int64) (pairVenuePosition, error) {
	v.mu.Lock()
	v.posReads++
	hook := v.position
	base := v.venueBase()
	v.mu.Unlock()
	if hook != nil {
		return hook(m, nowMs)
	}
	if v.virtual || !v.live {
		return pairVenuePosition{}, fmt.Errorf("a virtual research venue makes no private position read")
	}
	return pairTestVenuePosition(m, base, nowMs, nowMs)
}

func pairTestVenuePosition(m pairContractMember, base float64, observedMs, receiptMs int64) (pairVenuePosition, error) {
	native := math.Round(base/m.Meta.ContractSize*1e9) / 1e9
	q, err := pairDecimalFromFloat(native)
	if err != nil {
		return pairVenuePosition{}, err
	}
	cs, err := pairDecimalFromFloat(m.Meta.ContractSize)
	if err != nil {
		return pairVenuePosition{}, err
	}
	margin := m.Meta.MarginMode
	if q == "0" {
		margin = ""
	}
	p := pairVenuePosition{V: pairVenuePositionVersion, Key: pairEvidenceKey{Partition: "live", Provider: m.Provider, AccountKey: m.AccountKey, Instrument: m.Instrument, MemberID: m.MemberID},
		SignedNativeQty: string(q), ContractSize: string(cs), PositionMode: m.Meta.PositionMode, MarginMode: margin,
		ObservedMs: observedMs, FirstReceiptMs: receiptMs, EvidenceID: fmt.Sprintf("fixture-position:%s:%d", m.MemberID, receiptMs)}
	return p.sealed()
}

func (v *pairScriptedVenue) fill(a *basketAction, out basketVenueOutcome) basketVenueOutcome {
	if out.ClientOrderID == "" {
		out.ClientOrderID = a.ClientOrderID
	}
	if out.AccountKey == "" {
		out.AccountKey = a.AccountKey
	}
	if out.Instrument == "" {
		out.Instrument = a.Instrument
	}
	if out.Side == "" {
		out.Side = a.OrderSide
	}
	if out.FeeEvidenceID == "" && len(out.FeeCoverage) > 0 {
		out.FeeEvidenceID = pairFeeEvidenceID(a)
	}
	if out.AcceptedAtMs == 0 && (out.CumQty > 0 || out.Outcome == pairVenueOpen) {
		out.AcceptedAtMs = a.SubmittedAtMs
		if out.AcceptedAtMs == 0 {
			out.AcceptedAtMs = a.CreatedAtMs
		}
	}
	if out.CumQty > 0 && !a.recoveryNote().NativeBooked {
		if v.ledger == nil {
			v.ledger = map[string]float64{}
		}
		v.ledger[a.ClientOrderID] = a.signedDelta() * out.CumQty
	}
	return out
}
func (v *pairScriptedVenue) Submit(a *basketAction) (basketVenueOutcome, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.submits = append(v.submits, *a)
	if v.onSubmit != nil {
		v.onSubmit(a)
	}
	if len(v.submitErr) > 0 {
		err := v.submitErr[0]
		v.submitErr = v.submitErr[1:]
		if err != nil {
			return basketVenueOutcome{}, err
		}
	}
	if len(v.outcomes) > 0 {
		out := v.outcomes[0]
		v.outcomes = v.outcomes[1:]
		if out.Outcome == basketVenueFilled && out.CumQty == 0 {
			px := a.PriceBound
			if px <= 0 {
				px = v.fillMark
			}
			out.CumQty, out.CumNotional, out.Terminal = a.RequestedQty, a.RequestedQty*px, true
		}
		return v.fill(a, out), nil
	}
	px := a.PriceBound
	if px <= 0 {
		px = v.fillMark
	}
	return v.fill(a, basketVenueOutcome{Outcome: basketVenueFilled, CumQty: a.RequestedQty, CumNotional: a.RequestedQty * px, Terminal: true}), nil
}
func (v *pairScriptedVenue) Query(a *basketAction) (basketVenueOutcome, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.queryCnt == nil {
		v.queryCnt = map[string]int{}
	}
	v.queryCnt[a.ClientOrderID]++
	if v.onQuery != nil {
		v.onQuery(a)
	}
	q := v.queries[a.ClientOrderID]
	if len(q) == 0 {
		return basketVenueOutcome{Outcome: basketVenueUnknown, Detail: "still unknown"}, nil
	}
	out := q[0]
	if len(q) > 1 {
		v.queries[a.ClientOrderID] = q[1:]
	}
	return v.fill(a, out), nil
}

type pairTestVenues struct {
	mu  sync.Mutex
	seq int
	hl  *pairScriptedVenue
	okx *pairScriptedVenue
}

func newPairTestVenues(live bool) *pairTestVenues {
	v := &pairTestVenues{}
	v.hl = &pairScriptedVenue{mu: &v.mu, seq: &v.seq, member: pairTestHL, provider: entryProviderHyperliquid, account: "synthetic:hl-research", live: live, virtual: !live,
		fillMark: 60000, queries: map[string][]basketVenueOutcome{}}
	v.okx = &pairScriptedVenue{mu: &v.mu, seq: &v.seq, member: pairTestOKX, provider: entryProviderOKX, account: "synthetic:okx-research", live: live, virtual: !live,
		fillMark: 60100, queries: map[string][]basketVenueOutcome{}}
	return v
}

func (v *pairTestVenues) factory() func(StrategyConfig) (basketVenue, error) {
	return func(sc StrategyConfig) (basketVenue, error) {
		switch sc.ID {
		case pairTestHL:
			return v.hl, nil
		case pairTestOKX:
			return v.okx, nil
		}
		return nil, fmt.Errorf("no pair test venue for %s", sc.ID)
	}
}

func (v *pairTestVenues) of(id string) *pairScriptedVenue {
	if id == pairTestHL {
		return v.hl
	}
	return v.okx
}

type pairTestRunner struct {
	mu       sync.Mutex
	calls    int
	requests []pairPolicyRequest
	respond  func(req pairPolicyRequest) (pairPolicyResult, error)
	raw      func(req pairPolicyRequest) []byte
	block    chan struct{}
}

func (r *pairTestRunner) run(ctx context.Context, stdin []byte, timeout time.Duration) ([]byte, []byte, error) {
	var req pairPolicyRequest
	if err := json.Unmarshal(stdin, &req); err != nil {
		return nil, nil, err
	}
	r.mu.Lock()
	r.calls++
	r.requests = append(r.requests, req)
	respond, raw, block := r.respond, r.raw, r.block
	r.mu.Unlock()
	if block != nil {
		select {
		case <-block:
		case <-ctx.Done():
			return nil, nil, ctx.Err()
		}
	}
	if raw != nil {
		return raw(req), nil, nil
	}
	res, err := respond(req)
	if err != nil {
		return nil, nil, err
	}
	out, err := json.Marshal(res)
	return out, nil, err
}

func pairEnterResult(req pairPolicyRequest, hlQty float64) pairPolicyResult {
	orientation := pairOrientationLongHLShortOKX
	hlBound, okxBound := 60400.0, 59700.0
	if hlQty < 0 {
		orientation = pairOrientationLongOKXShortHL
		hlBound, okxBound = 59600.0, 60500.0
	}
	return pairPolicyResult{V: pairPolicyRequestVersion, PairID: req.PairID, SlotMs: req.SlotMs, ContractFingerprint: req.ContractFingerprint, InputFingerprint: req.InputFingerprint,
		Status: pairPolicyStatusEnter, Orientation: orientation, Decision: json.RawMessage(`{"reason":"fixture spread"}`),
		Targets: map[string]pairPolicyMemberTarget{pairTestHL: {TargetBaseQty: hlQty, PriceBound: hlBound}, pairTestOKX: {TargetBaseQty: -hlQty, PriceBound: okxBound}}}
}

func pairExitResult(req pairPolicyRequest, hlLong bool) pairPolicyResult {
	hlBound, okxBound := 59500.0, 60600.0
	if !hlLong {
		hlBound, okxBound = 60600.0, 59500.0
	}
	return pairPolicyResult{V: pairPolicyRequestVersion, PairID: req.PairID, SlotMs: req.SlotMs, ContractFingerprint: req.ContractFingerprint, InputFingerprint: req.InputFingerprint,
		Status: pairPolicyStatusExit, Decision: json.RawMessage(`{"reason":"fixture exit"}`),
		Targets: map[string]pairPolicyMemberTarget{pairTestHL: {TargetBaseQty: 0, PriceBound: hlBound}, pairTestOKX: {TargetBaseQty: 0, PriceBound: okxBound}}}
}

func pairHoldResult(req pairPolicyRequest, why string) pairPolicyResult {
	return pairPolicyResult{V: pairPolicyRequestVersion, PairID: req.PairID, SlotMs: req.SlotMs, ContractFingerprint: req.ContractFingerprint, InputFingerprint: req.InputFingerprint,
		Status: pairPolicyStatusHold, HoldReason: why, Decision: json.RawMessage(`{}`)}
}

type pairTestEnv struct {
	t       *testing.T
	dir     string
	doc     []byte
	adm     *pairResearchAdmission
	cfg     *Config
	store   *StateStore
	state   *AppState
	mu      *sync.RWMutex
	bc      *basketCoordinator
	venues  *pairTestVenues
	runner  *pairTestRunner
	quotes  *pairFixtureQuotes
	now     time.Time
	collect bool

	sealMu     sync.Mutex
	sealCalls  []pairSealCall
	generation uint64

	virtual *pairVirtualTestSet
}

type pairVirtualTestSet struct {
	build func() map[string]*pairVirtualVenue
	cur   map[string]*pairVirtualVenue
}

func (s *pairVirtualTestSet) factory() func(StrategyConfig) (basketVenue, error) {
	cur := s.cur
	return func(sc StrategyConfig) (basketVenue, error) {
		if v := cur[sc.ID]; v != nil {
			return v, nil
		}
		return nil, fmt.Errorf("no virtual pair venue for %s", sc.ID)
	}
}

type pairSealCall struct {
	AtMs int64
	ID   string
}

func pairTestStart() time.Time {
	return time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
}

func newPairTestEnv(t *testing.T, mutate func(root map[string]interface{})) *pairTestEnv {
	t.Helper()
	return newPairTestEnvDeps(t, mutate, nil)
}

func newPairTestEnvEvidence(t *testing.T, mutate func(root map[string]interface{}), evidence func(d *pairEvidenceDocument)) *pairTestEnv {
	t.Helper()
	if evidence == nil {
		return newPairTestEnvDeps(t, mutate, nil)
	}
	return newPairTestEnvDeps(t, mutate, func(doc []byte, venues *pairTestVenues) pairResearchDeps {
		deps := pairTestDeps(t, doc, venues)
		deps.Evidence = pairTestEvidence(t, doc, evidence)
		return deps
	})
}

func newPairTestEnvDeps(t *testing.T, mutate func(root map[string]interface{}), depsFor func(doc []byte, venues *pairTestVenues) pairResearchDeps) *pairTestEnv {
	t.Helper()
	return newPairTestEnvWith(t, mutate, depsFor, nil)
}

func newPairTestEnvWith(t *testing.T, mutate func(root map[string]interface{}), depsFor func(doc []byte, venues *pairTestVenues) pairResearchDeps, virtual *pairVirtualTestSet) *pairTestEnv {
	t.Helper()
	registerPairTestPolicy(t)
	dir := filepath.Join(t.TempDir(), "run")
	doc := pairTestDoc(t, dir, mutate)
	venues := newPairTestVenues(false)
	deps := pairTestDeps(t, doc, venues)
	if depsFor != nil {
		deps = depsFor(doc, venues)
	}
	if virtual != nil {
		virtual.cur = virtual.build()
		deps.VenueFor = virtual.factory()
	}
	adm, cfg, err := newPairResearchAdmission(doc, "run-1", dir, false, deps)
	if err != nil {
		t.Fatalf("newPairResearchAdmission: %v", err)
	}
	t.Cleanup(adm.release)
	e := &pairTestEnv{t: t, dir: dir, doc: doc, adm: adm, cfg: cfg, venues: venues, mu: &sync.RWMutex{}, now: pairTestStart(), generation: 1, virtual: virtual}
	e.quotes = pairFixtureQuotesOf(deps.Quotes)
	e.runner = &pairTestRunner{respond: func(req pairPolicyRequest) (pairPolicyResult, error) { return pairHoldResult(req, "no edge"), nil }}
	e.store = openSourceStore(t, cfg)
	e.state = NewAppState()
	for _, sc := range cfg.Strategies {
		e.state.Strategies[sc.ID] = NewStrategyState(sc)
	}
	e.reopen(false)
	return e
}

func (e *pairTestEnv) reopen(persisted bool) {
	e.t.Helper()
	known := map[string]bool{}
	if persisted {
		for id := range e.state.Strategies {
			known[id] = true
		}
	}
	if err := e.store.LoadEntryGuards(e.state, e.cfg.Strategies, known); err != nil {
		e.t.Fatalf("LoadEntryGuards: %v", err)
	}
	runtimes, err := e.store.LoadBaskets(e.state, e.cfg)
	if err != nil {
		e.t.Fatalf("LoadBaskets: %v", err)
	}
	e.bc = newBasketCoordinator(e.cfg, e.state, e.store, e.mu, nil, nil, runtimes)
	e.bc.parent = context.Background()
	e.bc.venueFor = e.venues.factory()
	e.bc.pairPolicyRun = e.runner.run
	e.bc.pairClock = func() time.Time { return e.now }
	if e.virtual != nil {
		if persisted {
			e.virtual.cur = e.virtual.build()
		}
		e.bc.venueFor = e.virtual.factory()
		if err := e.bc.attachPairVirtualVenues(); err != nil {
			e.t.Fatalf("attachPairVirtualVenues: %v", err)
		}
	}
	if err := e.bc.startupCheck(); err != nil {
		e.t.Fatalf("startupCheck: %v", err)
	}
}

func (e *pairTestEnv) restart() {
	e.t.Helper()
	e.store.Close()
	e.store = openSourceStore(e.t, e.cfg)
	loaded, _, err := LoadStateWithStore(e.cfg, e.store)
	if err != nil {
		e.t.Fatalf("LoadStateWithStore: %v", err)
	}
	for _, sc := range e.cfg.Strategies {
		if loaded.Strategies[sc.ID] == nil {
			loaded.Strategies[sc.ID] = NewStrategyState(sc)
		}
	}
	e.state = loaded
	e.reopen(true)
	e.bc.recoverInterrupted(e.now)
}

func (e *pairTestEnv) rt() *basketRuntime {
	return e.bc.runtimes[pairTestID]
}

func (e *pairTestEnv) observe(echoErr string) {
	for _, id := range []string{pairTestHL, pairTestOKX} {
		sc := e.bc.members[id]
		e.mu.Lock()
		e.bc.observePair(sc, StrategyDecisionFields{}, map[string]interface{}{"atr": 300.0}, 60000, echoErr)
		e.mu.Unlock()
	}
}

func (e *pairTestEnv) slot(at time.Time, input string) *basketRebalance {
	e.t.Helper()
	e.mu.Lock()
	defer e.mu.Unlock()
	rt := e.rt()
	rt.clock = at
	rb, err := e.bc.reservePairSlot(rt, at.UnixMilli(), at)
	if err != nil {
		e.t.Fatalf("reservePairSlot: %v", err)
	}
	if err := e.bc.recheckPairSlot(rt, at.UnixMilli(), rt.Contract.Fingerprint()); err != nil {
		e.t.Fatalf("recheckPairSlot: %v", err)
	}
	env := e.fixtureEnvelope(rt, at.UnixMilli(), at.UnixMilli()+500, "snap-"+input)
	if err := e.bc.freezePairEnvelope(rt, at.UnixMilli(), env, at); err != nil {
		e.t.Fatalf("freezePairEnvelope: %v", err)
	}
	return rb
}

func (e *pairTestEnv) step(at time.Time) {
	e.t.Helper()
	e.now = at
	e.bc.beginCycle()
	e.observe("")
	e.prepare(at)
	e.bc.step(e.inputs(at))
}

func (e *pairTestEnv) prepare(at time.Time) {
	e.bc.preparePairRisk(basketCycleInputs{Now: at, Once: true, PairSnapshotFor: e.sealFor, PairGeneration: e.feedGeneration})
}

func (e *pairTestEnv) inputs(at time.Time) basketCycleInputs {
	in := basketCycleInputs{Now: at, Once: true}
	if e.collect {
		in.PairSnapshotFor, in.PairGeneration = e.sealFor, e.feedGeneration
	}
	return in
}

func (e *pairTestEnv) sealFor(rt *basketRuntime, at time.Time) *marketSnapshot {
	snap := e.quotes.snapshot(rt.Contract.pairMembersSorted(), rt.Contract.Pair.BookNeed, at.UnixMilli()-50, fmt.Sprintf("feed-%d", at.UnixMilli()))
	e.sealMu.Lock()
	snap.ConfigGeneration = e.generation
	e.sealCalls = append(e.sealCalls, pairSealCall{AtMs: at.UnixMilli(), ID: snap.EvaluationID})
	e.sealMu.Unlock()
	return snap
}

func (e *pairTestEnv) feedGeneration() uint64 {
	e.sealMu.Lock()
	defer e.sealMu.Unlock()
	return e.generation
}

func pairFixtureQuotesOf(src pairQuoteSource) *pairFixtureQuotes {
	c, ok := src.(*pairQuoteCollector)
	if !ok {
		return nil
	}
	for _, r := range c.readers {
		if q, ok := r.(*pairFixtureQuotes); ok {
			return q
		}
	}
	return nil
}

func (e *pairTestEnv) externalBookChange(id string, qty float64) {
	e.t.Helper()
	e.mu.Lock()
	defer e.mu.Unlock()
	s := e.state.Strategies[id]
	if qty == 0 {
		delete(s.Positions, "BTC")
	} else {
		pos := s.Positions["BTC"]
		if pos == nil {
			side := entrySideLong
			if qty < 0 {
				side = entrySideShort
			}
			pos = &Position{Symbol: "BTC", Side: side, AvgCost: 60000, Multiplier: 1}
			s.Positions["BTC"] = pos
		}
		pos.Quantity = math.Abs(qty)
	}
	if err := e.store.SavePartition(e.state, e.rt().Partition); err != nil {
		e.t.Fatalf("save the external book change: %v", err)
	}
	e.venues.of(id).setVenueBase(qty)
}

func (e *pairTestEnv) signed(id string) float64 {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return basketPositionSigned(e.state.Strategies[id], "BTC")
}

func (e *pairTestEnv) db() *StateDB {
	return e.store.primary()
}

func (e *pairTestEnv) count(query string, args ...interface{}) int {
	e.t.Helper()
	var n int
	if err := e.db().db.QueryRow(query, args...).Scan(&n); err != nil {
		e.t.Fatalf("%s: %v", query, err)
	}
	return n
}

func (e *pairTestEnv) actionsByPurpose(purpose string) []*basketAction {
	var out []*basketAction
	for _, id := range e.rt().sortedActionIDs() {
		if a := e.rt().Actions[id]; a.Purpose == purpose {
			out = append(out, a)
		}
	}
	return out
}

func dirDigest(t *testing.T, dir string) string {
	t.Helper()
	var parts []string
	_ = filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil
		}
		parts = append(parts, path+"="+pairFileDigest(t, path))
		return nil
	})
	sort.Strings(parts)
	return sha256Hex([]byte(strings.Join(parts, "\n")))
}

func pairFileDigest(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return "missing"
		}
		t.Fatal(err)
	}
	return sha256Hex(b)
}

type pairTestHost struct {
	e          *pairTestEnv
	interval   time.Duration
	nextManage time.Time
	fund       func(at time.Time)
	prepare    func(in basketCycleInputs)
	manage     func(at time.Time, risk map[RiskPartition]*scopeCycleRisk)
	marks      map[string]float64
	order      []string
	runs       []pairHostRun
	saveErrs   []string
	nextAt     time.Time
}

type pairHostRun struct {
	At      time.Time
	Managed bool
	Next    time.Duration
}

func newPairTestHost(e *pairTestEnv, interval time.Duration) *pairTestHost {
	h := &pairTestHost{e: e, interval: interval, nextManage: e.now, marks: map[string]float64{"BTC": 60000}}
	h.manage = func(time.Time, map[RiskPartition]*scopeCycleRisk) { e.observe("") }
	h.prepare = func(in basketCycleInputs) { e.bc.preparePairRisk(in) }
	return h
}

func (h *pairTestHost) note(phase string) {
	h.order = append(h.order, phase)
}

func (h *pairTestHost) run(at time.Time) time.Duration {
	e := h.e
	e.t.Helper()
	e.now = at
	h.note("evidence")
	if h.fund != nil {
		h.fund(at)
		h.note("funding")
	}
	managed := !at.Before(h.nextManage)
	in := basketCycleInputs{Now: at, Once: true, PairSnapshotFor: e.sealFor, PairGeneration: e.feedGeneration}
	if h.prepare != nil {
		h.prepare(in)
		h.note("prepare_risk")
	}
	cfg, state, store := e.cfg, e.state, e.store
	if allPartitionsSaveBlocked(store, cfg) {
		in.SaveBlocked = true
		h.note("save_blocked")
		managed = false
	} else {
		parts := activePartitions(cfg.Strategies)
		risk := make(map[RiskPartition]*scopeCycleRisk, len(parts))
		e.mu.RLock()
		pm := e.bc.pairMarksLocked()
		for _, p := range parts {
			risk[p] = measureScopeCycleRiskMarked(p, partitionRiskConfig(cfg, p), cfg.Strategies, state, h.marks, nil, nil, true, false, at, pm)
		}
		e.mu.RUnlock()
		e.mu.Lock()
		for _, p := range parts {
			applyScopeCycleRisk(risk[p], state.partitionRisk(p))
			if !p.IsLive() && risk[p].KillSwitchFired {
				applyPaperKillSwitchCycle(state, cfg, h.marks, risk[p], true)
			}
		}
		e.mu.Unlock()
		h.note("risk")
		if managed {
			e.bc.beginCycle()
			h.manage(at, risk)
			h.note("manage")
			h.nextManage = at.Add(h.interval)
		}
		in.ScopeRisk = risk
	}
	h.note("step")
	e.bc.step(in)
	e.mu.Lock()
	outcomes := store.SaveAll(state)
	e.mu.Unlock()
	for _, out := range sortedPartitionErrors(outcomes) {
		if out.err != nil {
			h.saveErrs = append(h.saveErrs, fmt.Sprintf("%s: %v", partitionLabel(out.part), out.err))
		}
	}
	h.note("save")
	fresh := e.bc.pairNow(basketCycleInputs{Now: at})
	next := h.nextManage.Sub(fresh)
	if w := e.bc.wakeDelay(fresh); w >= 0 && w < next {
		next = w
	}
	if next < 0 {
		next = 0
	}
	h.runs = append(h.runs, pairHostRun{At: at, Managed: managed, Next: next})
	h.nextAt = fresh.Add(next)
	return next
}

func (h *pairTestHost) runUntil(end time.Time) {
	e := h.e
	e.t.Helper()
	at := h.nextAt
	if at.IsZero() {
		at = e.now
	}
	zeros := 0
	for !at.After(end) {
		d := h.run(at)
		if d == 0 {
			zeros++
			if zeros > 2 {
				e.t.Fatalf("the host scheduled %d zero-delay wakes in a row at %s; the wake schedule spins", zeros, at.Format(time.RFC3339Nano))
			}
		} else {
			zeros = 0
		}
		at = h.nextAt
	}
}

func (h *pairTestHost) incomplete() (pairObligation, bool) {
	h.e.mu.RLock()
	defer h.e.mu.RUnlock()
	return pairIncompleteEvidence(h.e.rt())
}

func (h *pairTestHost) savedIncomplete() bool {
	h.e.t.Helper()
	var raw string
	if err := h.e.db().db.QueryRow(`SELECT progress_json FROM basket_contracts WHERE basket_id = ?`, pairTestID).Scan(&raw); err != nil {
		h.e.t.Fatalf("read saved pair progress: %v", err)
	}
	p, err := parsePairProgress(raw)
	if err != nil {
		h.e.t.Fatalf("saved pair progress: %v", err)
	}
	return p.obligation(pairObligationPolicyTimeout, "", "") != nil
}

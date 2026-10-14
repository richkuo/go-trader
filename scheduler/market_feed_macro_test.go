package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const (
	macroTestHourMs   = int64(3_600_000)
	macroTestEventBar = 100
	macroTestNowBar   = 102
)

var macroTestT0 int64

func macroTestSetT0(t *testing.T) {
	t.Helper()
	if raw := os.Getenv("GO_TRADER_MACRO_HARNESS_T0"); raw != "" {
		if _, err := fmt.Sscanf(raw, "%d", &macroTestT0); err != nil {
			t.Fatalf("GO_TRADER_MACRO_HARNESS_T0 %q: %v", raw, err)
		}
		return
	}
	macroTestT0 = time.Now().UTC().UnixMilli()/macroTestHourMs*macroTestHourMs - macroTestNowBar*macroTestHourMs
}

func macroTestRelease() int64 { return macroTestT0 + macroTestEventBar*macroTestHourMs }
func macroTestCutoff() int64  { return macroTestT0 + macroTestNowBar*macroTestHourMs }

func macroTestStrategy(id, coin string, params map[string]interface{}) StrategyConfig {
	return StrategyConfig{
		ID:           id,
		Type:         "perps",
		Platform:     "hyperliquid",
		Script:       hyperliquidCheckScript,
		Args:         []string{macroEventBreakoutStrategyName, coin, "1h", "--mode=paper"},
		OpenStrategy: StrategyRef{Name: macroEventBreakoutStrategyName, Params: params},
	}
}

func macroTestLocal(ms int64) string {
	return time.UnixMilli(ms).UTC().Format("2006-01-02T15:04:05")
}

func macroTestCalendarRecord(eventID string, ms int64, known *int64, revision int64) macroCalendarRecord {
	r := macroCalendarRecord{EventID: eventID, EventType: macroEventTypeFOMC, Revision: revision, Status: macroStatusScheduled,
		ScheduledLocal: macroTestLocal(ms), SourceTimezone: "UTC", ScheduledUTCMs: ms, KnownMs: known,
		PublicationEvidence: "fixture", SourceID: "fixture"}
	r.RecordSHA256 = r.hash()
	return r
}

func macroTestQuote(eventMs, recvMs int64, bid, ask string) macroQuoteRecord {
	r := macroQuoteRecord{EventMs: eventMs, RecvMs: recvMs, Bid: bid, Ask: ask, SourceID: "fixture"}
	r.RecordSHA256 = r.hash()
	return r
}

func macroTestInt(v int64) *int64 { return &v }

func macroTestCalendarDoc() *macroCalendarDoc {
	cutoff := macroTestCutoff()
	recs := []macroCalendarRecord{
		macroTestCalendarRecord("fixture/event", macroTestRelease(), macroTestInt(macroTestT0-200*macroTestHourMs), 1),
		macroTestCalendarRecord("fixture/at-cutoff", macroTestT0+140*macroTestHourMs, macroTestInt(cutoff), 1),
		macroTestCalendarRecord("fixture/after-cutoff", macroTestT0+150*macroTestHourMs, macroTestInt(cutoff+1), 1),
		macroTestCalendarRecord("fixture/unknown", macroTestT0+130*macroTestHourMs, nil, 1),
	}
	sortMacroCalendar(recs)
	return &macroCalendarDoc{Schema: macroCalendarSchema, Kind: "fixture", Importer: macroImporter,
		EventTypes: []string{macroEventTypeFOMC},
		Coverage:   []macroCoverage{{EventType: macroEventTypeFOMC, StartMs: macroTestT0 - 1000*macroTestHourMs, EndMs: macroTestT0 + 1000*macroTestHourMs}},
		Sources:    []macroCalendarSource{{SourceID: "fixture", Publisher: "test", URL: "fixture:macro", RetrievedMs: macroTestT0, SHA256: strings.Repeat("0", 64), Note: "integration fixture"}},
		Records:    recs}
}

func sortMacroCalendar(recs []macroCalendarRecord) {
	for i := 1; i < len(recs); i++ {
		for j := i; j > 0 && macroCalendarSortLess(recs[j], recs[j-1]); j-- {
			recs[j], recs[j-1] = recs[j-1], recs[j]
		}
	}
}

func macroTestQuoteDoc(coin string) *macroQuoteDoc {
	cutoff := macroTestCutoff()
	var recs []macroQuoteRecord
	for i := int64(0); i <= macroTestNowBar; i++ {
		d := macroTestT0 + i*macroTestHourMs
		recs = append(recs, macroTestQuote(d-2_000, d-1_500, "100", "100.01"))
	}
	recs = append(recs, macroTestQuote(cutoff-500, cutoff, "100", "100.02"), macroTestQuote(cutoff-400, cutoff+1, "100", "100.03"))
	for i := 1; i < len(recs); i++ {
		for j := i; j > 0 && macroQuoteSortLess(recs[j], recs[j-1]); j-- {
			recs[j], recs[j-1] = recs[j-1], recs[j]
		}
	}
	return &macroQuoteDoc{Schema: macroQuoteSchema, Kind: "fixture", Importer: macroImporter, Venue: macroVenue, Namespace: feedNamespacePerps,
		Coin: coin, Sources: []macroQuoteSource{{SourceID: "fixture", Publisher: "test", URL: "fixture:macro", RetrievedMs: macroTestT0, SHA256: strings.Repeat("0", 64), Note: "integration fixture"}},
		Records: recs}
}

func macroTestWrite(t *testing.T, path string, v any) string {
	t.Helper()
	blob, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, blob, 0o644); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(blob)
	return hex.EncodeToString(sum[:])
}

type macroTestInputs struct {
	CalendarPath   string `json:"calendar_path"`
	CalendarSHA256 string `json:"calendar_sha256"`
	QuotePath      string `json:"quote_path"`
	QuoteSHA256    string `json:"quote_sha256"`
}

func macroTestArtifacts(t *testing.T, dir string) macroTestInputs {
	t.Helper()
	if in := os.Getenv("GO_TRADER_MACRO_HARNESS_IN"); in != "" {
		var got macroTestInputs
		blob, err := os.ReadFile(filepath.Join(in, "inputs.json"))
		if err != nil {
			t.Fatalf("harness inputs: %v", err)
		}
		if err := json.Unmarshal(blob, &got); err != nil {
			t.Fatal(err)
		}
		got.CalendarPath = filepath.Join(in, got.CalendarPath)
		got.QuotePath = filepath.Join(in, got.QuotePath)
		return got
	}
	out := macroTestInputs{CalendarPath: filepath.Join(dir, "calendar.json"), QuotePath: filepath.Join(dir, "quotes.json")}
	out.CalendarSHA256 = macroTestWrite(t, out.CalendarPath, macroTestCalendarDoc())
	out.QuoteSHA256 = macroTestWrite(t, out.QuotePath, macroTestQuoteDoc("BTC"))
	return out
}

func macroTestParams(in macroTestInputs) map[string]interface{} {
	return map[string]interface{}{
		"expected_interval_ms": float64(macroTestHourMs), "range_bars": float64(6), "post_window_bars": float64(4),
		"confirm_bars": float64(1), "cooldown_bars": float64(24), "holding_bars": float64(6), "max_quote_age_ms": float64(60_000),
		"calendar_path": in.CalendarPath, "calendar_sha256": in.CalendarSHA256, "quote_path": in.QuotePath, "quote_sha256": in.QuoteSHA256,
	}
}

func macroTestStubCandles(t *testing.T, now func() time.Time) {
	stubFeedCandleSnapshot(t, func(coin, interval string, startMs, endMs int64) ([]hlCandleRaw, error) {
		var out []hlCandleRaw
		nowMs := now().UnixMilli()
		for i := int64(0); i < 300; i++ {
			open := macroTestT0 - 140*macroTestHourMs + i*macroTestHourMs
			if open > nowMs {
				break
			}
			px := 100.0
			switch (open - macroTestT0) / macroTestHourMs {
			case macroTestEventBar:
				px = 101.2
			case macroTestEventBar + 1:
				px = 101.5
			}
			out = append(out, hlCandleRaw{OpenMs: open, CloseMs: open + macroTestHourMs - 1, HasClose: open+macroTestHourMs <= nowMs,
				Open: 100, High: px + 0.5, Low: 99.5, Close: px, Volume: 1})
		}
		return out, nil
	})
}

func TestMacroEventFeedPipeline(t *testing.T) {
	macroTestSetT0(t)
	dir := t.TempDir()
	in := macroTestArtifacts(t, dir)
	nowMs := macroTestCutoff() + 30_000
	clock := func() time.Time { return time.UnixMilli(nowMs).UTC() }
	macroTestStubCandles(t, clock)

	btc := macroTestStrategy("macro-btc", "BTC", macroTestParams(in))
	peer := StrategyConfig{ID: "peer-btc", Type: "perps", Platform: "hyperliquid", Script: hyperliquidCheckScript,
		Args: []string{"breakout", "BTC", "1h", "--mode=paper"}}
	relative := macroTestParams(in)
	relative["calendar_path"] = "calendar.json"
	ethRel := macroTestStrategy("macro-eth-relative", "ETH", relative)
	conflictA := macroTestParams(in)
	conflictA["calendar_path"] = filepath.Join(dir, "shared.json")
	conflictB := macroTestParams(in)
	conflictB["calendar_path"] = filepath.Join(dir, "shared.json")
	conflictB["calendar_sha256"] = strings.Repeat("a", 64)
	cfg := &Config{Strategies: []StrategyConfig{btc, peer, ethRel, macroTestStrategy("macro-sol-a", "SOL", conflictA), macroTestStrategy("macro-sol-b", "SOL", conflictB)}}
	req, err := deriveFeedRequirements(cfg)
	if err != nil {
		t.Fatalf("derive requirements: %v", err)
	}
	req.AccountingCoins = nil
	calKey, quoteKey := macroCalendarKeyFor(in.CalendarSHA256), macroQuoteKeyFor("BTC", in.QuoteSHA256)
	entry := req.Strategies["macro-btc"]
	if !entry.MacroEvents || entry.MacroErr != "" || entry.MacroCalendar != calKey || entry.MacroQuotes != quoteKey || !entry.Timed {
		t.Fatalf("macro requirement %+v", entry)
	}
	wantWindow := int64(24+4+6+2)*macroTestHourMs + 60_000
	if req.Macro[quoteKey].WindowMs != wantWindow || req.Macro[quoteKey].Path != in.QuotePath || req.Macro[calKey].Path != in.CalendarPath {
		t.Fatalf("macro needs %+v", req.Macro)
	}
	if e := req.Strategies["macro-eth-relative"]; !strings.Contains(e.MacroErr, "must be absolute") || len(e.macroNeeds()) != 0 {
		t.Fatalf("a relative artifact path must be a diagnostic on that strategy only: %+v", e)
	}
	for _, id := range []string{"macro-sol-a", "macro-sol-b"} {
		if e := req.Strategies[id]; !strings.Contains(e.MacroErr, "pinned to 2 different digests") {
			t.Fatalf("%s: one path pinned to two digests must be refused: %+v", id, e)
		}
	}
	if p := req.Strategies["peer-btc"]; p.MacroEvents || len(p.macroNeeds()) != 0 || p.Signal != entry.Signal {
		t.Fatalf("the peer keeps its own requirements: %+v", p)
	}
	if err := validateMarketFeedConfig(&Config{MarketFeed: marketFeedREST, Strategies: []StrategyConfig{btc}}); err == nil || !strings.Contains(err.Error(), "cannot supply sealed calendar and quote records") {
		t.Fatalf("market_feed=rest with the candidate: %v", err)
	}
	cycle := cycleRequirementsForDue([]StrategyConfig{btc, peer}, req)
	if cycle.Macro[calKey] != 0 || cycle.Macro[quoteKey] != wantWindow || len(cycle.Macro) != 2 {
		t.Fatalf("cycle macro needs %+v", cycle.Macro)
	}
	if peerOnly := cycleRequirementsForDue([]StrategyConfig{peer}, req); len(peerOnly.Macro) != 0 {
		t.Fatalf("a peer-only cycle must not request macro inputs: %+v", peerOnly.Macro)
	}
	if merged := mergeCycleRequirements(cycle, cycleMarketRequirements{}); merged.Macro[quoteKey] != wantWindow {
		t.Fatalf("mergeCycleRequirements dropped macro needs: %+v", merged.Macro)
	}
	union := unionFeedRequirements([]feedConsumer{{Path: "macro", Loaded: true, Req: req}})
	if full := fullCycleRequirements(union); full.Macro[quoteKey] != wantWindow || full.Macro[calKey] != 0 {
		t.Fatalf("full-cycle macro needs %+v", full.Macro)
	}
	if est := estimateFeedSealBytes(union); est > feedSealMaxBytes || est < macroQuoteMaxSealRecords*feedEstimateMacroRecordBytes {
		t.Fatalf("seal estimate %d", est)
	}

	owner := newMarketFeedOwner(clock, t.Logf)
	owner.corrOffsets = nil
	select {
	case <-owner.ApplyGeneration(context.Background(), req):
	case <-time.After(5 * time.Second):
		t.Fatal("generation did not publish")
	}
	owner.IngestMids(map[string]float64{"BTC": 100.0}, clock(), "ws")
	owner.feedMu.Lock()
	prepared := owner.macro[quoteKey]
	owner.feedMu.Unlock()
	if prepared == nil || prepared.Err != "" || prepared.Quotes == nil {
		t.Fatalf("quote artifact was not prepared: %+v", prepared)
	}
	original, err := os.ReadFile(in.QuotePath)
	if err != nil {
		t.Fatal(err)
	}
	if os.Getenv("GO_TRADER_MACRO_HARNESS_IN") == "" {
		replaced := macroTestQuoteDoc("BTC")
		replaced.Records = replaced.Records[:3]
		macroTestWrite(t, in.QuotePath, replaced)
		defer os.WriteFile(in.QuotePath, original, 0o644)
	}

	key := macroTestCutoff() / 1000
	sealer := newFeedSealer(owner, feedSourceWebsocket, "inst-macro", clock, t.Logf)
	sealer.settle = 0
	sealer.prepare = time.Hour
	sealer.setGeneration(union, []int{3600}, time.UnixMilli(macroTestCutoff()-2*macroTestHourMs))
	sealer.startServing(time.UnixMilli(macroTestCutoff() - 2*macroTestHourMs))
	sealer.sealOne(context.Background(), key)
	blob := sealer.sealBytes(key)
	if len(blob) == 0 {
		t.Fatalf("no seal for key %d: %+v", key, sealer.lookup(key))
	}
	doc, err := decodeFeedSeal(blob, key)
	if err != nil {
		t.Fatalf("decode own seal: %v", err)
	}
	if doc.V != feedSealVersionMacro || len(doc.MacroCalendars) != 1 || len(doc.MacroQuotes) != 1 {
		t.Fatalf("seal v%d carries %d calendars and %d quote sets", doc.V, len(doc.MacroCalendars), len(doc.MacroQuotes))
	}
	if os.Getenv("GO_TRADER_MACRO_HARNESS_IN") == "" {
		cal := doc.MacroCalendars[0]
		ids := map[string]bool{}
		for _, r := range cal.Records {
			ids[r.EventID] = true
		}
		if !cal.Available || !ids["fixture/event"] || !ids["fixture/at-cutoff"] || ids["fixture/after-cutoff"] || ids["fixture/unknown"] {
			t.Fatalf("calendar records must be known by the cutoff (inclusive) and carry publication evidence: %v", ids)
		}
		q := doc.MacroQuotes[0]
		last := q.Records[len(q.Records)-1]
		if !q.Available || last.RecvMs != macroTestCutoff() || last.Ask != "100.02" || q.Records[0].RecvMs < macroTestCutoff()-wantWindow {
			t.Fatalf("quote window must end at the cutoff (inclusive) and start at cutoff-window: first %+v last %+v", q.Records[0], last)
		}
		if len(q.Records) <= 3 {
			t.Fatalf("a replaced file on disk must not change the prepared records: %d records sealed", len(q.Records))
		}
	}

	again := sealer.sealBytes(key)
	sealer.sealOne(context.Background(), key)
	if !bytes.Equal(again, sealer.sealBytes(key)) {
		t.Fatal("a second seal attempt replaced immutable bytes")
	}
	<-owner.ApplyGeneration(context.Background(), req)
	owner.feedMu.Lock()
	reused := owner.macro[quoteKey] == prepared
	owner.feedMu.Unlock()
	if !reused {
		t.Fatal("a generation with the same identity must reuse the prepared records instead of rereading the file")
	}
	if os.Getenv("GO_TRADER_MACRO_HARNESS_IN") == "" {
		fresh := newMarketFeedOwner(clock, t.Logf)
		<-fresh.ApplyGeneration(context.Background(), req)
		fresh.feedMu.Lock()
		freshQuote := fresh.macro[quoteKey]
		fresh.feedMu.Unlock()
		if freshQuote == nil || !strings.Contains(freshQuote.Err, "sha256") {
			t.Fatalf("a restarted owner must refuse the replaced bytes by digest: %+v", freshQuote)
		}
		snap := freezeMarketSnapshot(fresh, fullCycleRequirements(union), "fresh", clock(), time.UnixMilli(macroTestCutoff()))
		fctx := &marketFeedContext{Enabled: true, Requirements: req, Snapshot: snap}
		if h := fctx.holdFor(btc); !h.Held || !h.EntryOnly || !strings.Contains(h.Reason, "sha256") {
			t.Fatalf("an unavailable macro input must be an entry-only hold: %+v", h)
		}
		if h := fctx.holdFor(peer); h.Held {
			t.Fatalf("the peer must not inherit the candidate's hold: %+v", h)
		}
		payload, err := fctx.singleCheckPayload(btc)
		if err != nil {
			t.Fatalf("the candidate payload must still build so closes can run: %v", err)
		}
		var env marketStdinEnvelope
		if err := json.Unmarshal(payload, &env); err != nil {
			t.Fatal(err)
		}
		if q := env.Market.MacroQuotes[quoteKey.PayloadID()]; q.Available || !strings.Contains(q.Reason, "sha256") || len(env.Market.Frames) == 0 {
			t.Fatalf("unavailable quotes must reach the payload with their reason: %+v", q)
		}
		os.WriteFile(in.QuotePath, original, 0o644)
	}

	sockDir, err := os.MkdirTemp("", "mac")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(sockDir)
	sock := filepath.Join(sockDir, "f.sock")
	srv, err := listenFeedSocket(sock, sealer, t.Logf)
	if err != nil {
		t.Fatal(err)
	}
	go srv.serve()
	defer srv.close()
	client := newSharedFeedClientWithEndpoints([]sharedFeedEndpoint{{Name: "primary", Socket: sock}})
	client.settle, client.prepare = 0, time.Hour
	ctx := context.Background()
	descs := client.Describe(ctx)
	if len(descs) != 1 || descs[0].Err != nil || len(descs[0].Describe.Macro) != 2 || descs[0].Header.SealVersion != feedSealVersionMacro {
		t.Fatalf("describe must advertise both macro inputs under seal v5: %+v", descs)
	}
	if _, problems := sharedFeedCompatibilityLines("startup", descs, req, []int{3600}); len(problems) != 0 {
		t.Fatalf("the feed must cover this consumer: %v", problems)
	}
	other := req
	other.Macro = map[feedMacroKey]feedMacroNeed{macroQuoteKeyFor("ETH", in.QuoteSHA256): {Path: in.QuotePath, WindowMs: 1}}
	if _, problems := sharedFeedCompatibilityLines("startup", descs, other, []int{3600}); len(problems) != 1 || !strings.Contains(problems[0], "ETH|"+in.QuoteSHA256+" not served") {
		t.Fatalf("an unserved macro input must be a coverage gap: %v", problems)
	}
	shared, report := client.Fetch(ctx, key, cycle)
	if report.Status != feedFetchSealed || len(report.Gaps) != 0 {
		t.Fatalf("shared fetch %+v", report)
	}
	wider := cycleRequirementsForDue([]StrategyConfig{btc}, req)
	wider.Macro[quoteKey] = wantWindow + 1
	if _, wide := client.Fetch(ctx, key, wider); len(wide.Gaps) != 1 || !strings.Contains(wide.Gaps[0], "below the") {
		t.Fatalf("a quote window wider than the seal must be a coverage gap: %+v", wide.Gaps)
	}

	wsSnap := freezeMarketSnapshot(owner, cycle, feedSealEvaluationID(key), clock(), time.UnixMilli(macroTestCutoff()))
	wsSnap.Deadline = time.UnixMilli(macroTestCutoff()).UTC()
	wsCtx := &marketFeedContext{Enabled: true, Requirements: req, Snapshot: wsSnap}
	shCtx := &marketFeedContext{Enabled: true, Requirements: req, Snapshot: shared, SharedKey: key}
	for _, c := range []*marketFeedContext{wsCtx, shCtx} {
		if h := c.holdFor(btc); h.Held {
			t.Fatalf("available macro inputs must not hold the candidate: %+v", h)
		}
	}
	wsPayload, err := wsCtx.singleCheckPayload(btc)
	if err != nil {
		t.Fatal(err)
	}
	shPayload, err := shCtx.singleCheckPayload(btc)
	if err != nil {
		t.Fatal(err)
	}
	var wsEnv, shEnv marketStdinEnvelope
	if err := json.Unmarshal(wsPayload, &wsEnv); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(shPayload, &shEnv); err != nil {
		t.Fatal(err)
	}
	wsMacro, _ := json.Marshal([]any{wsEnv.Market.MacroEvents, wsEnv.Market.MacroQuotes, wsEnv.Market.Frames, wsEnv.Market.DecisionCutoffMs})
	shMacro, _ := json.Marshal([]any{shEnv.Market.MacroEvents, shEnv.Market.MacroQuotes, shEnv.Market.Frames, shEnv.Market.DecisionCutoffMs})
	if !bytes.Equal(wsMacro, shMacro) {
		t.Fatalf("websocket and shared payloads disagree:\nws=%s\nshared=%s", wsMacro, shMacro)
	}
	batch, err := shCtx.batchPayload(hlBatchKey{Symbol: "BTC", Timeframe: "1h", OhlcvLimit: 200}, []StrategyConfig{btc, peer})
	if err != nil {
		t.Fatalf("batch payload: %v", err)
	}
	if _, ok := batch.MacroQuotes[quoteKey.PayloadID()]; !ok || len(batch.MacroEvents) != 1 {
		t.Fatalf("the batch payload must carry the candidate's macro sections: %+v", batch.MacroEvents)
	}
	peerPayload, err := shCtx.singleCheckPayload(peer)
	if err != nil || bytes.Contains(peerPayload, []byte("macro_")) {
		t.Fatalf("a peer payload must keep only its own data keys: %v", err)
	}

	tamper := func(mut func(*feedSealDoc)) error {
		var d feedSealDoc
		if err := json.Unmarshal(blob, &d); err != nil {
			t.Fatal(err)
		}
		mut(&d)
		b, err := json.Marshal(&d)
		if err != nil {
			t.Fatal(err)
		}
		_, err = decodeFeedSeal(b, key)
		return err
	}
	for name, mut := range map[string]func(*feedSealDoc){
		"record hash": func(d *feedSealDoc) { d.MacroQuotes[0].Records[0].Ask = "100.05" },
		"version":     func(d *feedSealDoc) { d.V = feedSealVersionTrades },
		"order": func(d *feedSealDoc) {
			r := d.MacroQuotes[0].Records
			r[0], r[1] = r[1], r[0]
		},
		"duplicate":   func(d *feedSealDoc) { d.MacroQuotes[0].Records[1] = d.MacroQuotes[0].Records[0] },
		"unavailable": func(d *feedSealDoc) { d.MacroCalendars[0].Available = false; d.MacroCalendars[0].Reason = "x" },
		"late record": func(d *feedSealDoc) { d.MacroCalendars[0].CutoffMs = macroTestT0 - 300*macroTestHourMs },
	} {
		if err := tamper(mut); err == nil {
			t.Fatalf("strict decode accepted a seal with a tampered %s", name)
		}
	}
	unknown := bytes.Replace(blob, []byte(`"macro_quotes":[{`), []byte(`"macro_quotes":[{"extra":1,`), 1)
	if _, err := decodeFeedSeal(unknown, key); err == nil {
		t.Fatal("strict decode accepted an unknown macro field")
	}

	changed := macroTestParams(in)
	changed["quote_sha256"] = strings.Repeat("b", 64)
	next := &Config{Strategies: []StrategyConfig{macroTestStrategy("macro-btc", "BTC", changed), peer, ethRel,
		macroTestStrategy("macro-sol-a", "SOL", conflictA), macroTestStrategy("macro-sol-b", "SOL", conflictB)}}
	if errs := macroIdentityReloadErrors(cfg, next); len(errs) != 1 || !strings.Contains(errs[0], "restart required") {
		t.Fatalf("an artifact identity change must be restart-required: %v", errs)
	}
	if err := validateHotReloadCompatible(cfg, next); err == nil || !strings.Contains(err.Error(), "artifact identity changed") {
		t.Fatalf("the scheduler reload must refuse the identity change: %v", err)
	}
	nextReq, _ := deriveFeedRequirements(next)
	if errs := macroGenerationReloadErrors(owner.MacroNeeds(), unionFeedRequirements([]feedConsumer{{Loaded: true, Req: nextReq}})); len(errs) != 2 {
		t.Fatalf("the feed reload must refuse a new and a dropped artifact: %v", errs)
	}
	if errs := macroGenerationReloadErrors(owner.MacroNeeds(), union); len(errs) != 0 {
		t.Fatalf("an unchanged generation must reload: %v", errs)
	}

	if out := os.Getenv("GO_TRADER_MACRO_HARNESS_OUT"); out != "" {
		meta, _ := json.Marshal(map[string]any{"key": key, "cutoff_ms": macroTestCutoff(), "params": macroTestParams(in),
			"seal_sha256": feedSealHash(blob), "strategy": "macro-btc"})
		for name, b := range map[string][]byte{"payload_ws.json": wsPayload, "payload_shared.json": shPayload, "seal.json": blob, "harness.json": meta} {
			if err := os.WriteFile(filepath.Join(out, name), b, 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
}

func TestMacroEventArtifactReaderAcceptsTheStudyCalendarAndRefusesMalformedInput(t *testing.T) {
	var report struct {
		Official struct {
			Path   string `json:"path"`
			SHA256 string `json:"sha256"`
		} `json:"official"`
	}
	blob, err := os.ReadFile(filepath.Join("..", "backtest", "candidates", "macro_event_breakout_1654", "data", "calendar", "import_report.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(blob, &report); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join("..", report.Official.Path)
	p := prepareMacroArtifact(macroCalendarKeyFor(report.Official.SHA256), feedMacroNeed{Path: path}, time.Now())
	if p.Err != "" || p.Calendar == nil || len(p.Calendar.Records) == 0 {
		t.Fatalf("the Python-imported study calendar must pass the Go reader: %+v", p.Err)
	}
	raw, _ := os.ReadFile(path)
	dir := t.TempDir()
	for name, mutate := range map[string]func([]byte) []byte{
		"unknown key": func(b []byte) []byte {
			return bytes.Replace(b, []byte(`{"event_id"`), []byte(`{"extra":1,"event_id"`), 1)
		},
		"missing key": func(b []byte) []byte { return bytes.Replace(b, []byte(`"known_ms":null,`), []byte(``), 1) },
		"bad utc": func(b []byte) []byte {
			return bytes.Replace(b, []byte(`"scheduled_utc_ms":1`), []byte(`"scheduled_utc_ms":2`), 1)
		},
		"float number": func(b []byte) []byte { return bytes.Replace(b, []byte(`"revision":1`), []byte(`"revision":1.0`), 1) },
	} {
		bad := mutate(append([]byte(nil), raw...))
		if bytes.Equal(bad, raw) {
			t.Fatalf("%s: the mutation did not apply", name)
		}
		file := filepath.Join(dir, strings.ReplaceAll(name, " ", "_")+".json")
		os.WriteFile(file, bad, 0o644)
		sum := sha256.Sum256(bad)
		if got := prepareMacroArtifact(macroCalendarKeyFor(hex.EncodeToString(sum[:])), feedMacroNeed{Path: file}, time.Now()); got.Err == "" {
			t.Fatalf("%s: the reader accepted a malformed calendar", name)
		}
	}
	if got := prepareMacroArtifact(macroCalendarKeyFor(strings.Repeat("c", 64)), feedMacroNeed{Path: path}, time.Now()); !strings.Contains(got.Err, "sha256") {
		t.Fatalf("a digest mismatch must be refused: %q", got.Err)
	}
	if _, err := macroLocalToUTCMs("2026-03-08T02:30:00", "America/New_York"); err == nil || !strings.Contains(err.Error(), "does not exist") {
		t.Fatalf("a nonexistent local time must be refused: %v", err)
	}
	if _, err := macroLocalToUTCMs("2026-11-01T01:30:00", "America/New_York"); err == nil || !strings.Contains(err.Error(), "ambiguous") {
		t.Fatalf("an ambiguous local time must be refused: %v", err)
	}
	if got, err := macroLocalToUTCMs("2025-01-29T14:00:00", "America/New_York"); err != nil || got != 1738177200000 {
		t.Fatalf("2 p.m. ET on 2025-01-29 converts to %d (%v)", got, err)
	}
}

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

type pairBookTestEnv struct {
	t        *testing.T
	b        *pairPaperBooks
	rec      *pairRecorder
	wall     atomic.Int64
	d        *pairPaperBookReducer
	contract basketContract
}

func newPairBookTestEnv(t *testing.T) *pairBookTestEnv {
	t.Helper()
	p := newPairTestEnv(t, nil)
	c := p.rt().Contract
	db, err := OpenStateDB(filepath.Join(t.TempDir(), "books.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if err = db.ensurePairRecordingSchema(); err != nil {
		t.Fatal(err)
	}
	e := &pairBookTestEnv{t: t, contract: c}
	e.wall.Store(pairTestStart().UnixMilli())
	e.rec = newPairRecorder(pairRecLive, "book-test", c.Partition, c.Fingerprint(), db, &simulatedClock{}, pairRecFaults{})
	e.rec.schema = pairRecSchemaBooks
	e.rec.now = func() time.Time { return time.UnixMilli(e.wall.Load()) }
	s := newRecordingQuoteSource(e.rec, map[string]string{pairTestHL: pairSourceSealedFeed, pairTestOKX: pairSourcePublicSnapshot}, 1000, nil, func() time.Time { return time.UnixMilli(e.wall.Load()) })
	e.b = newPairPaperBooks(e.rec, s)
	s.books = e.b
	if err = s.bindContract(c); err != nil {
		t.Fatal(err)
	}
	bind := pairRecBind{RunID: e.rec.runID, ContractFP: c.Fingerprint(), Partition: c.Partition, PaperIdentity: "paper", AnchorMs: e.wall.Load(), Family: pairPaperManifestBooksVer}
	if err = e.rec.appendDirect(pairRecEvent{Kind: pairRecEventBind, Bind: &bind}, pairRecEvent{Kind: pairRecEventEpoch, Epoch: &pairRecEpoch{Epoch: 1, WallMs: e.wall.Load(), Mode: "new"}}); err != nil {
		t.Fatal(err)
	}
	e.d = newPairPaperBookReducer(e.b.members[pairTestHL], e.b.need, false)
	e.rec.beginStep("s0", 0)
	t.Cleanup(func() { _ = e.b.stop("test") })
	return e
}

func pairBookTestFrame(at int64, bid string) []byte {
	bids, asks := []map[string]any{}, []map[string]any{}
	for i := 0; i < 20; i++ {
		v, _ := strconv.ParseFloat(bid, 64)
		bids = append(bids, map[string]any{"px": fmt.Sprintf("%.1f", v-float64(i)), "sz": "10", "n": 1})
		asks = append(asks, map[string]any{"px": fmt.Sprintf("%d", 60100+i), "sz": "10", "n": 1})
	}
	return pairPaperJSON(map[string]any{"channel": "l2Book", "data": map[string]any{"coin": "BTC", "time": at, "levels": []any{bids, asks}}})
}

func (e *pairBookTestEnv) append(kind string, at int64, raw []byte) {
	e.t.Helper()
	e.wall.Store(at)
	beforeCursor, beforeLogical := e.rec.cursorNow(), e.rec.logicalNow()
	if kind == "subscribed" && raw == nil {
		raw = pairPaperJSON(map[string]any{"channel": "subscriptionResponse", "data": map[string]any{"method": "subscribe", "subscription": map[string]any{"type": "l2Book", "coin": "BTC", "fast": false}}})
	}
	in := pairPaperBookInput{Binding: e.b.bindings[pairTestHL], Epoch: 1, Kind: kind, Raw: raw, Outcome: kind}
	if err := e.b.append(context.Background(), in, e.d); err != nil {
		e.t.Fatal(err)
	}
	if e.rec.cursorNow() != beforeCursor || e.rec.logicalNow() != beforeLogical {
		e.t.Fatal("background input changed host cursor or logical time")
	}
}

func (e *pairBookTestEnv) log() *pairRecLog {
	e.t.Helper()
	l, err := readVerifiedPairRecLog(e.rec.file, e.rec.runID, e.rec.partition, e.rec.contractFP)
	if err != nil {
		e.t.Fatal(err)
	}
	return l
}

func TestBasketReplayPairPaperBooksWakePrefixSurvivesBackgroundCommit(t *testing.T) {
	e := newPairBookTestEnv(t)
	base := e.wall.Load()
	if _, err := e.rec.wake("s0", 0); err != nil {
		t.Fatal(err)
	}
	wake := e.rec.headNow()
	e.append("epoch", base+1, nil)
	if e.rec.headNow().Seq <= wake.Seq {
		t.Fatal("background input must commit after the host wake")
	}
	if got := e.rec.prefixAt(wake.Seq); got != wake.Prefix {
		t.Fatalf("background input changed the host wake prefix: got %q want %q", got, wake.Prefix)
	}
	l := e.log()
	if got, ok := l.prefixAt(wake.Seq); !ok || got != e.rec.prefixAt(wake.Seq) {
		t.Fatalf("recovery changed the host wake prefix: got %q present %t", got, ok)
	}
}

func TestBasketReplayPairPaperBooksCommittedPrefixAndPlayback(t *testing.T) {
	e := newPairBookTestEnv(t)
	base := e.wall.Load()
	e.append("epoch", base, nil)
	e.append("subscribed", base+1, nil)
	for i := int64(1); i <= 65; i++ {
		e.append("frame", base+i*1000, pairBookTestFrame(base+i*1000, "60000"))
	}
	e.wall.Store(base + 65002)
	e.b.started = true
	e.b.done = make(chan struct{})
	close(e.b.done)
	e.b.ready = e.b.done
	v, err := e.b.read(context.Background(), pairRecKindBookSeal, pairPaperBookRequest{Members: []string{pairTestHL}, ThroughMs: base + 65001})
	if err != nil {
		t.Fatal(err)
	}
	d, err := e.b.reduce(context.Background(), v, pairTestHL, base+65001, false)
	if err != nil {
		t.Fatal(err)
	}
	if d.accepted != 65 {
		t.Fatalf("committed prefix derived %d records", d.accepted)
	}
	captured, release := make(chan struct{}), make(chan struct{})
	pairPaperBookReceiptGate = func() { close(captured); <-release }
	t.Cleanup(func() { pairPaperBookReceiptGate = nil })
	e.wall.Store(base + 66000)
	done := make(chan error, 1)
	go func() {
		done <- e.b.append(context.Background(), pairPaperBookInput{Binding: e.b.bindings[pairTestHL], Epoch: 1, Kind: "frame", Raw: pairBookTestFrame(base+66000, "60001"), Outcome: "frame"}, e.d)
	}()
	<-captured
	e.wall.Store(base + 66002)
	_, err = e.b.read(context.Background(), pairRecKindBookWindow, pairPaperBookRequest{Members: []string{pairTestHL}, FromMs: base + 65001, ThroughMs: base + 66001})
	if err == nil {
		t.Fatal("an uncommitted captured ticket must prevent acknowledgement")
	}
	close(release)
	if err = <-done; err != nil {
		t.Fatal(err)
	}
	pairPaperBookReceiptGate = nil
	d2, err := e.b.reduce(context.Background(), v, pairTestHL, base+66001, false)
	if err != nil || d2.accepted != 65 {
		t.Fatalf("later commit changed selected prefix: %v %+v", err, d2)
	}
	l := e.log()
	play := newPairRecorder(pairRecPlayback, e.rec.runID, e.rec.partition, e.rec.contractFP, nil, &simulatedClock{}, pairRecFaults{})
	play.log = l
	play.schema = pairRecSchemaBooks
	play.beginStep("s0", 0)
	s := newRecordingQuoteSource(play, e.b.src.Sources(), 1000, nil, func() time.Time { return time.UnixMilli(base + 99000) })
	b := newPairPaperBooks(play, s)
	s.books = b
	if err = s.bindContract(e.contract); err != nil {
		t.Fatal(err)
	}
	again, err := b.read(context.Background(), pairRecKindBookSeal, pairPaperBookRequest{Members: []string{pairTestHL}, ThroughMs: base + 65001})
	if err != nil || again != v {
		t.Fatalf("playback changed saved boundary: %v %+v %+v", err, again, v)
	}
	_, err = b.read(context.Background(), pairRecKindBookWindow, pairPaperBookRequest{Members: []string{pairTestHL}, FromMs: base + 65001, ThroughMs: base + 66001})
	if err == nil {
		t.Fatal("playback must retain unavailable attempt")
	}
	pd, err := b.reduce(context.Background(), again, pairTestHL, base+65001, false)
	if err != nil {
		t.Fatal(err)
	}
	if string(pairPaperJSON(pd.stream(play.runID, base+65001).freeze(d.need, base+65001))) != string(pairPaperJSON(d.stream(e.rec.runID, base+65001).freeze(d.need, base+65001))) {
		t.Fatal("paper and playback must derive equal canonical books")
	}
}

func TestBasketReplayPairPaperBooksWebsocketCommitsBeforeUseAndStops(t *testing.T) {
	e := newPairBookTestEnv(t)
	frames := make(chan []byte, 4)
	connected := make(chan struct{})
	up := websocket.Upgrader{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := up.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		var sub map[string]any
		if conn.ReadJSON(&sub) != nil {
			return
		}
		_ = conn.WriteJSON(map[string]any{"channel": "subscriptionResponse", "data": sub})
		close(connected)
		for raw := range frames {
			if conn.WriteMessage(websocket.TextMessage, raw) != nil {
				return
			}
		}
	}))
	t.Cleanup(func() { close(frames); server.Close() })
	prev := pairPaperBookEndpoint
	pairPaperBookEndpoint = func() string { return "ws" + strings.TrimPrefix(server.URL, "http") }
	t.Cleanup(func() { pairPaperBookEndpoint = prev })
	t.Cleanup(func() { e.b.cancelIntake(); _ = e.b.stop("test") })
	if err := e.b.start(); err != nil {
		t.Fatal(err)
	}
	<-connected
	waitFor(t, "committed subscription", func() bool {
		l := e.log()
		for _, r := range l.rows {
			if r.Event.BookInput != nil && r.Event.BookInput.Kind == "subscribed" {
				return true
			}
		}
		return false
	})
	e.wall.Add(1000)
	frames <- pairBookTestFrame(e.wall.Load(), "60000")
	waitFor(t, "committed frame", func() bool {
		l := e.log()
		for _, r := range l.rows {
			if r.Event.BookInput != nil && r.Event.BookInput.Outcome == feedBookOutcomeAccepted {
				return true
			}
		}
		return false
	})
	before := e.rec.headNow()
	e.rec.mu.Lock()
	e.rec.faults = pairRecFaults{kind: "fail-record-append", n: 1}
	e.rec.mu.Unlock()
	e.wall.Add(1000)
	frames <- pairBookTestFrame(e.wall.Load(), "60001")
	waitFor(t, "append barrier", func() bool { return e.rec.blocked.Load() })
	if e.rec.headNow().Seq != before.Seq {
		t.Fatal("failed frame must remain outside the committed prefix")
	}
	_ = e.b.stop("fault")
	if !e.rec.blocked.Load() {
		t.Fatal("append failure must preserve increase barrier")
	}
}

func TestBasketReplayPairPaperBooksReadRecoveryAndCapacity(t *testing.T) {
	e := newPairBookTestEnv(t)
	base := e.wall.Load()
	e.append("epoch", base, nil)
	head := e.rec.headNow()
	in := e.b.intakeNow()
	req := pairPaperJSON(pairPaperBookRequest{Members: []string{pairTestHL}, ThroughMs: base})
	s := pairRecStart{Kind: pairRecKindBookSeal, Phase: pairRecPhaseStep, StepID: "s0", Ordinal: 1, Request: req, RequestHash: sha256Hex(req), BookPrefix: &head, BookIntake: &in}
	if err := e.rec.appendDirect(pairRecEvent{Kind: pairRecEventStart, CallID: "s0/1", Start: &s}); err != nil {
		t.Fatal(err)
	}
	l := e.log()
	e.rec.mode = pairRecResume
	e.rec.log = l
	e.rec.ordinal = 0
	e.rec.cursor = 0
	_, _, err := e.rec.call(context.Background(), pairRecCall{Kind: pairRecKindBookSeal, Request: req}, func(context.Context, *pairRecHandle) pairRecResult {
		t.Fatal("interrupted read must never rerun")
		return pairRecResult{}
	})
	if err == nil {
		t.Fatal("interrupted read must supply no result")
	}
	l = e.log()
	if l.rows[len(l.rows)-1].Kind != pairRecEventRecovery {
		t.Fatal("interrupted read must save durable recovery")
	}
	e.rec.mu.Lock()
	e.rec.encodedBytes = pairPaperBookLogLimit - pairPaperBookReserve
	e.rec.mu.Unlock()
	before := e.rec.headNow()
	err = e.b.append(context.Background(), pairPaperBookInput{Binding: e.b.bindings[pairTestHL], Epoch: 1, Kind: "frame", Raw: pairBookTestFrame(base+1, "60000"), Outcome: "frame"}, e.d)
	if err == nil || !e.rec.limitStopped.Load() || e.rec.headNow() != before {
		t.Fatal("ordinary input must stop before consuming terminal reserve")
	}
}

func TestBasketReplayPairPaperBooksCoverageRecoveryAndRawEvidence(t *testing.T) {
	for _, kind := range []string{"duplicate", "conflict", "tie", "malformed", "future", "stale", "gap", "disconnect", "clock"} {
		t.Run(kind, func(t *testing.T) {
			e := newPairBookTestEnv(t)
			base := e.wall.Load()
			e.append("epoch", base, nil)
			e.append("subscribed", base+1, nil)
			for i := int64(1); i <= 65; i++ {
				e.append("frame", base+i*1000, pairBookTestFrame(base+i*1000, "60000"))
			}
			next := base + 66000
			switch kind {
			case "duplicate":
				e.append("frame", next, pairBookTestFrame(base+65000, "60000"))
			case "conflict":
				e.append("frame", next, pairBookTestFrame(base+65000, "60001"))
			case "tie":
				next = base + 65000
				e.append("frame", next, pairBookTestFrame(next+1, "60001"))
			case "malformed":
				e.append("frame", next, []byte(`{"channel":"l2Book","data":{"coin":"BTC"}}`))
			case "future":
				e.append("frame", next, pairBookTestFrame(next+1000, "60000"))
			case "stale":
				e.append("frame", next, pairBookTestFrame(next-6000, "60000"))
			case "gap":
				next = base + 80000
				e.append("frame", next, pairBookTestFrame(next, "60000"))
			case "disconnect":
				e.append("disconnect", next, nil)
			case "clock":
				next = base + 64000
				e.append("frame", next, pairBookTestFrame(next, "60000"))
			}
			l := e.log()
			if err := e.b.validateBindings(l); err != nil {
				t.Fatal(err)
			}
			v := pairPaperBookBoundary{Prefix: l.head()}
			at := next + 1
			if at < base+65001 {
				at = base + 65001
			}
			d, err := e.b.reduce(context.Background(), v, pairTestHL, at, false)
			if err != nil {
				t.Fatal(err)
			}
			st := d.stream(e.rec.runID, at)
			snap := &marketSnapshot{EvaluationID: "test", ConfigGeneration: 1, Deadline: time.UnixMilli(at), books: map[feedObservationKey]*marketSnapshotBook{st.key: st.freeze(contractBookNeed(e.b.need.WindowMs, e.b.need.Capacity, e.b.need.Depth), at)}}
			payload := snap.l2BookPayload(st.key, contractBookNeed(e.b.need.WindowMs, e.b.need.Capacity, e.b.need.Depth))
			raw, _ := pairCanonicalJSON(payload)
			o, deriveErr := pairObservationFromPayload(e.b.members[pairTestHL], e.rec.partition, e.b.need, raw, 0, 1)
			available := snap.books[st.key].Readiness.Ready && deriveErr == nil && o.Coverage.Status == pairCoverageComplete
			if available != (kind == "duplicate") {
				t.Fatalf("%s availability=%v ready=%+v derive=%v coverage=%+v", kind, available, snap.books[st.key].Readiness, deriveErr, o.Coverage)
			}
			last := l.rows[len(l.rows)-1].Event.BookInput
			if last == nil || kind != "disconnect" && len(last.Raw) == 0 {
				t.Fatal("raw rejection or duplicate evidence must remain durable")
			}
			if kind == "tie" {
				e.append("stop", base+66001, nil)
				if _, err = e.b.exportBook(e.log(), pairTestHL); err == nil {
					t.Fatal("receipt ties must refuse standalone export")
				}
				return
			}
			if kind == "disconnect" {
				e.append("subscribed", base+81000, nil)
			}
			for i := int64(82); i <= 145; i++ {
				e.append("frame", base+i*1000, pairBookTestFrame(base+i*1000, "60000"))
			}
			e.append("stop", base+146000, nil)
			blob, err := e.b.exportBook(e.log(), pairTestHL)
			if err != nil {
				t.Fatal(err)
			}
			var doc pairReplayBookDoc
			if err = json.Unmarshal(blob, &doc); err != nil {
				t.Fatal(err)
			}
			if doc.structural(pairTestHL) != "" {
				t.Fatal("recovered canonical stream must satisfy standalone schema")
			}
			at = base + 145000
			live, err := e.b.reduce(context.Background(), pairPaperBookBoundary{Prefix: e.log().head()}, pairTestHL, at, false)
			if err != nil {
				t.Fatal(err)
			}
			offline := &pairReplaySealedStream{member: e.b.members[pairTestHL], key: l2BookKeyFor("BTC"), doc: doc}
			for i, r := range doc.Records {
				offline.obs = append(offline.obs, feedBookObservation{Seq: uint64(i + 1), EventMs: r.EventMs, RecvMs: r.RecvMs, Session: 1, Identity: bookIdentity(offline.key, r.EventMs, r.Bids, r.Asks), BidDepth: len(r.Bids), AskDepth: len(r.Asks), Bids: r.Bids, Asks: r.Asks})
			}
			for i, m := range doc.Markers {
				offline.marks = append(offline.marks, feedBookMarker{Seq: uint64(i + 1), AtMs: m.AtMs, Reason: m.Reason, Detail: m.Detail, Session: 1})
			}
			need := contractBookNeed(e.b.need.WindowMs, e.b.need.Capacity, e.b.need.Depth)
			payloadBytes := func(st *pairReplaySealedStream) []byte {
				snap := &marketSnapshot{EvaluationID: "parity", ConfigGeneration: 1, Deadline: time.UnixMilli(at), books: map[feedObservationKey]*marketSnapshotBook{st.key: st.freeze(need, at)}}
				raw, _ := pairCanonicalJSON(snap.l2BookPayload(st.key, need))
				return raw
			}
			if string(payloadBytes(live.stream(e.rec.runID, at))) != string(payloadBytes(offline)) {
				t.Fatal("standalone canonical levels, identities, and sequence numbers must equal paper")
			}

		})
	}
}

func TestBasketReplayPairPaperBooksAppendCancellationUsesOwningDatabase(t *testing.T) {
	e := newPairBookTestEnv(t)
	base := e.wall.Load()
	e.append("epoch", base, nil)
	e.rec.mu.Lock()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	done := make(chan error, 1)
	go func() {
		done <- e.b.append(ctx, pairPaperBookInput{Binding: e.b.bindings[pairTestHL], Epoch: 1, Kind: "stop", Outcome: "stop"}, e.d)
	}()
	select {
	case err := <-done:
		if err != context.Canceled {
			t.Fatalf("cancellable recorder acquisition: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("recorder acquisition ignored cancellation")
	}
	e.rec.mu.Unlock()
	before := e.rec.headNow()
	conn, err := e.rec.file.db.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel = context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	_, err = e.rec.file.appendPairRecEventsContext(ctx, e.rec.runID, e.rec.partition, e.rec.contractFP, before, pairRecSchemaBooks, nil, nil)
	_ = conn.Close()
	if err == nil || e.rec.headNow() != before {
		t.Fatal("one-connection stream transaction must cancel without changing its head")
	}
	ctx, cancel = context.WithCancel(context.Background())
	_, err = e.rec.file.appendPairRecEventsContext(ctx, e.rec.runID, e.rec.partition, e.rec.contractFP, before, pairRecSchemaBooks,
		[]pairRecEvent{{Kind: pairRecEventEpoch, Epoch: &pairRecEpoch{Epoch: 2, WallMs: e.wall.Load(), Mode: "resume"}}}, func(int) error {
			cancel()
			return nil
		})
	if err == nil || !strings.Contains(err.Error(), context.Canceled.Error()) {
		t.Fatalf("stream statement must use the transaction cancellation context: %v", err)
	}
	if got := e.log().head(); got != before {
		t.Fatalf("cancelled stream statement changed the committed head: %+v", got)
	}
}

func TestBasketReplayPairPaperBooksFundingPublishesAtConsumption(t *testing.T) {
	e := newPairBookTestEnv(t)
	f := newPairRecOracleFixture(t, 1_800_003_600_000)
	e.wall.Store(f.target)
	if _, err := e.rec.wake("", 0); err != nil {
		t.Fatal(err)
	}
	id, start, err := e.rec.beginAsyncCall(pairRecCall{Kind: pairRecKindFundingOracle, MemberID: f.key.MemberID, SlotMs: f.target, Request: f.request(t)})
	if err != nil {
		t.Fatal(err)
	}
	batch := f.batch(t, f.target, f.target+100)
	decoded, err := pairDecodeFundingBatch(batch)
	if err != nil {
		t.Fatal(err)
	}
	obs, err := pairCanonicalJSON(decoded.Research.Observations[0])
	if err != nil {
		t.Fatal(err)
	}
	before := e.rec.cursorNow()
	e.append("epoch", f.target+10, nil)
	e.append("subscribed", f.target+11, nil)
	e.append("frame", f.target+12, pairBookTestFrame(f.target+12, "60000"))
	end, err := e.rec.appendAsyncTerminal(id, start, pairAsyncOracleResult{res: pairRecResult{Class: pairRecClassResult, Result: obs, ReceiptMs: f.target + 100}, completion: f.target + 200})
	if err != nil {
		t.Fatal(err)
	}
	if e.rec.cursorNow() != before {
		t.Fatal("family-4 oracle terminal must remain outside consumable cursor")
	}
	if err = e.rec.appendConsume(id, pairRecConsume{RequestHash: start.RequestHash, TerminalHash: end.ResultHash, TerminalKind: pairRecEventEnd, WakeSeq: e.rec.lastWakeSeq(), StepID: "s0", LogicalMs: f.target + 300, Disposition: "accepted", Batch: batch, BatchHash: decoded.BatchHash}); err != nil {
		t.Fatal(err)
	}
	l := e.log()
	for _, frontier := range []int{before, e.rec.cursorNow()} {
		src, err := newPairRecordedFundingSourceWithPolicy(f.src.SourceID(), f.policy.FromMs, f.policy)
		if err != nil {
			t.Fatal(err)
		}
		r := &pairReplayRuntime{paper: &pairPaperRun{manifest: pairPaperManifest{Version: pairPaperManifestBooksVer}, scenario: pairPaperScenario{FundingPolicy: &f.policy}, funding: src}}
		if !r.paperAsync() {
			t.Fatal("family 4 with a policy must retain asynchronous funding")
		}
		if err = r.restoreFundingFrom(l, frontier); err != nil {
			t.Fatal(err)
		}
		want := 0
		if frontier > before {
			want = 1
		}
		if len(src.chain()) != want {
			t.Fatalf("frontier %d restored %d batches, expected %d", frontier, len(src.chain()), want)
		}
	}
}

func TestBasketReplayPairPaperBooksAlteredBindingsRefuseBeforePublication(t *testing.T) {
	for _, kind := range []string{"instrument", "source contract", "epoch", "family", "read prefix"} {
		t.Run(kind, func(t *testing.T) {
			e := newPairBookTestEnv(t)
			base := e.wall.Load()
			e.append("epoch", base, nil)
			head := e.rec.headNow()
			intake := e.b.intakeNow()
			req := pairPaperJSON(pairPaperBookRequest{Members: []string{pairTestHL}, ThroughMs: base})
			start := pairRecStart{Kind: pairRecKindBookSeal, Phase: pairRecPhaseStep, StepID: "s0", Ordinal: 1, Request: req, RequestHash: sha256Hex(req), BookPrefix: &head, BookIntake: &intake}
			if err := e.rec.appendDirect(pairRecEvent{Kind: pairRecEventStart, CallID: "s0/1", Start: &start}); err != nil {
				t.Fatal(err)
			}
			l := e.log()
			db, err := OpenStateDB(filepath.Join(t.TempDir(), "altered.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			if err = db.ensurePairRecordingSchema(); err != nil {
				t.Fatal(err)
			}
			copyRec := newPairRecorder(pairRecLive, e.rec.runID, e.rec.partition, e.rec.contractFP, db, &simulatedClock{}, pairRecFaults{})
			copyRec.schema = pairRecSchemaBooks
			for _, row := range l.rows {
				ev := row.Event
				if ev.Bind != nil {
					v := *ev.Bind
					ev.Bind = &v
					if kind == "family" {
						v.Family = pairPaperManifestAsyncVer
					}
				}
				if ev.BookInput != nil {
					v := *ev.BookInput
					ev.BookInput = &v
					switch kind {
					case "instrument":
						v.Binding.Instrument = "ETH"
					case "source contract":
						v.Binding.SourceContract = "foreign"
					case "epoch":
						v.Epoch = 3
					}
				}
				if ev.Start != nil {
					v := *ev.Start
					ev.Start = &v
					prefix := copyRec.headNow()
					v.BookPrefix = &prefix
					if kind == "read prefix" {
						prefix.Prefix = strings.Repeat("0", 64)
					}
				}
				if err = copyRec.appendDirect(ev); err != nil {
					t.Fatal(err)
				}
			}
			before := copyRec.headNow()
			altered, err := readVerifiedPairRecLog(db, e.rec.runID, e.rec.partition, e.rec.contractFP)
			if err == nil {
				err = e.b.validateBindings(altered)
			}
			if err == nil {
				t.Fatal("altered book binding or prefix must refuse before publication")
			}
			if copyRec.headNow() != before {
				t.Fatal("refusal must leave the committed input untouched")
			}
		})
	}
}

func TestBasketReplayPairPaperBooksLivePrefixRejectsCorruptedInput(t *testing.T) {
	e := newPairBookTestEnv(t)
	base := e.wall.Load()
	e.append("epoch", base, nil)
	e.append("subscribed", base+1, nil)
	e.append("frame", base+1000, pairBookTestFrame(base+1000, "60000"))
	l := e.log()
	head := l.head()
	row := l.rows[len(l.rows)-1]
	input := *row.Event.BookInput
	input.Raw = pairBookTestFrame(base+1000, "60001")
	payload, err := pairRecEncodeSchema(pairRecSchemaBooks, pairRecEvent{Kind: pairRecEventBookInput, BookInput: &input})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = e.rec.file.db.Exec(`DROP TRIGGER basket_replay_inputs_immutable`); err != nil {
		t.Fatal(err)
	}
	if _, err = e.rec.file.db.Exec(`UPDATE basket_replay_inputs SET payload=? WHERE run_id=? AND seq=?`, payload, e.rec.runID, row.Seq); err != nil {
		t.Fatal(err)
	}
	if _, err = e.b.reduce(context.Background(), pairPaperBookBoundary{Prefix: head}, pairTestHL, base+1001, false); err == nil {
		t.Fatal("live derivation must verify the full selected hash chain before publication")
	}
}

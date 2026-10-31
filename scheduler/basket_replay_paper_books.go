package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

const (
	pairRecEventBookInput    = "book_input"
	pairRecKindBookSeal      = "book_seal"
	pairRecKindBookWindow    = "book_window"
	pairRecKindBookMark      = "book_mark"
	pairPaperBookLogLimit    = int64(256 << 20)
	pairPaperBookReserve     = int64(32 << 20)
	pairPaperBookObjectLimit = 256 << 20
	pairPaperBookJoin        = 5 * time.Second
)

var pairPaperBookEndpoint = func() string {
	return "wss://" + strings.TrimPrefix(hlMainnetURL, "https://") + "/ws"
}

var errPairPaperBookMemory = errors.New("paper book working memory limit reached")

var pairPaperBookReceiptGate func()

func pairRecAsyncSchema(schema int) bool {
	return schema == pairRecSchemaAsync || schema == pairRecSchemaBooks
}

func pairPaperFamilySchema(family, schema int) bool {
	return schema == pairRecSchemaVersion && (family == 0 || family == pairPaperManifestVer || family == pairPaperManifestResearchVer) ||
		schema == pairRecSchemaAsync && family == pairPaperManifestAsyncVer || schema == pairRecSchemaBooks && family == pairPaperManifestBooksVer
}

func pairPaperHasBooks(shape pairReplayResearchShape) bool {
	for _, p := range shape.Pairs {
		for _, m := range p.Members {
			if m.Source == pairSourceSealedFeed {
				return true
			}
		}
	}
	return false
}

func pairPaperBookCall(kind string) bool {
	return kind == pairRecKindBookSeal || kind == pairRecKindBookWindow || kind == pairRecKindBookMark
}

type pairPaperBookBinding struct {
	MemberID       string `json:"member_id"`
	Provider       string `json:"provider"`
	Instrument     string `json:"instrument"`
	SourceContract string `json:"source_contract"`
}

type pairPaperBookInput struct {
	Binding          pairPaperBookBinding `json:"binding"`
	Epoch            int                  `json:"epoch"`
	Ticket           int64                `json:"ticket"`
	ReceiptMs        int64                `json:"receipt_ms"`
	Kind             string               `json:"kind"`
	Raw              []byte               `json:"raw,omitempty"`
	Outcome          string               `json:"outcome"`
	Reason           string               `json:"reason,omitempty"`
	LostTicketFrom   int64                `json:"lost_ticket_from,omitempty"`
	IncompleteReason string               `json:"incomplete_reason,omitempty"`
	CoverageAtMs     int64                `json:"coverage_at_ms,omitempty"`
	ClockViolation   bool                 `json:"clock_violation,omitempty"`
}

type pairPaperBookIntake struct {
	Captured  int64 `json:"captured"`
	Committed int64 `json:"committed"`
	WallMs    int64 `json:"wall_ms"`
	HighMs    int64 `json:"high_ms"`
	Stopped   bool  `json:"stopped"`
	Failed    bool  `json:"failed"`
}

type pairPaperBookRequest struct {
	Members   []string `json:"members"`
	FromMs    int64    `json:"from_ms"`
	ThroughMs int64    `json:"through_ms"`
}

type pairPaperBookBoundary struct {
	Prefix         pairRecHead `json:"prefix"`
	Ticket         int64       `json:"ticket"`
	AcknowledgedMs int64       `json:"acknowledged_ms"`
	Disposition    string      `json:"disposition"`
	EvaluationID   string      `json:"evaluation_id"`
	Generation     uint64      `json:"generation"`
	Reason         string      `json:"reason,omitempty"`
}

type pairPaperBookReducer struct {
	member      pairContractMember
	need        feedBookNeed
	owner       marketFeedOwner
	state       feedBookState
	accepted    int
	markers     int
	lastReceipt int64
	epoch       int
	incomplete  string
	all         bool
	records     []pairReplayBookRecord
	allMarkers  []pairReplayBookMarkerIn
	onMarker    func(pairReplayBookMarkerIn)
}

func newPairPaperBookReducer(m pairContractMember, need pairBookNeed, all bool) *pairPaperBookReducer {
	fn := contractBookNeed(need.WindowMs, need.Capacity, feedBookMaxLevels)
	return &pairPaperBookReducer{member: m, need: fn, state: feedBookState{Key: l2BookKeyFor(m.Instrument), Need: fn}, all: all}
}

func (d *pairPaperBookReducer) marker(at int64, reason, detail string) {
	if at < d.state.HighRecvMs {
		at = d.state.HighRecvMs
	}
	d.markers++
	m := feedBookMarker{Seq: uint64(d.markers), AtMs: at, Reason: reason, Session: 1, Detail: boundedReason(detail)}
	d.state.Markers = append(d.state.Markers, m)
	if d.onMarker != nil {
		d.onMarker(pairReplayBookMarkerIn{AtMs: at, Reason: reason, Detail: m.Detail})
	}
	if d.all {
		d.allMarkers = append(d.allMarkers, pairReplayBookMarkerIn{AtMs: at, Reason: reason, Detail: m.Detail})
	}
	if len(d.state.Markers) > feedBookMaxMarkers {
		cut := d.state.Markers[0].AtMs + 1
		if cut > d.state.RetainedFromMs {
			d.state.RetainedFromMs = cut
		}
		d.state.Markers = append([]feedBookMarker(nil), d.state.Markers[1:]...)
	}
}

func (d *pairPaperBookReducer) apply(in pairPaperBookInput) (string, string) {
	st := &d.state
	if st.StartedMs == 0 {
		st.StartedMs = in.ReceiptMs
		st.RetainedFromMs = in.ReceiptMs
		st.DepthFromMs = in.ReceiptMs
	}
	if in.ClockViolation || in.ReceiptMs < st.HighRecvMs {
		at := in.CoverageAtMs
		if at < st.HighRecvMs {
			at = st.HighRecvMs
		}
		d.marker(at, feedBookMarkerClock, "receipt clock regressed")
		return feedBookOutcomeRejected, "receipt clock regressed"
	}
	switch in.Kind {
	case "epoch":
		d.epoch = in.Epoch
		d.owner.connected = false
		st.Covered = false
		d.marker(in.ReceiptMs, feedBookMarkerEpoch, "paper socket epoch")
	case "subscribed":
		d.owner.connected = true
		d.owner.obsSession = 1
		st.Session = 1
		st.Covered = true
		d.marker(in.ReceiptMs, feedBookMarkerSubscribed, "acknowledged paper subscription")
	case "disconnect", "stop", "rejected", "silence":
		if in.Kind == "stop" && in.Epoch > d.epoch {
			d.epoch = in.Epoch
		}
		reason := feedBookMarkerInvalid
		if in.Kind == "disconnect" || in.Kind == "stop" {
			reason = feedBookMarkerDisconnected
			st.Covered = false
			d.owner.connected = false
		}
		if in.Kind == "silence" {
			reason = feedBookMarkerSilence
		}
		d.marker(in.ReceiptMs, reason, in.Reason)
		if in.Kind == "rejected" {
			return feedBookOutcomeRejected, in.Reason
		}
	case "frame":
		var env feedSocketEnvelope
		var p feedBookParse
		if json.Unmarshal(in.Raw, &env) != nil || env.Channel != "l2Book" {
			p.Err = errors.New("malformed book envelope")
		} else {
			p = parseBookFrame(l2BookKeyFor, env.Data)
		}
		if p.Coin != d.member.Instrument && p.Err == nil {
			p.Err = errors.New("book instrument differs from admitted member")
		}
		if p.Err == nil {
			if in.ReceiptMs == d.lastReceipt && p.EventMs > st.HighEventMs && st.Covered && d.owner.connected && p.EventMs <= in.ReceiptMs+feedBookMaxFutureSkewMs && in.ReceiptMs-p.EventMs <= feedBookMaxLagMs {
				d.incomplete = "distinct book observations share a receipt millisecond"
				d.marker(in.ReceiptMs, feedBookMarkerEvidenceLoss, d.incomplete)
				return feedBookOutcomeRejected, d.incomplete
			}
		}
		var events feedBookEvents
		d.owner.bookSeq = uint64(d.accepted + d.markers + 1)
		oldMarkers := st.Markers
		st.Markers = nil
		out := d.owner.ingestBookLocked(st, p, in.ReceiptMs, uint64(d.accepted+1), &events)
		st.Markers = oldMarkers
		for _, ev := range events {
			if ev.Kind == "book_marker" {
				reason, _ := ev.Fields["reason"].(string)
				detail, _ := ev.Fields["detail"].(string)
				d.marker(ev.RecvMs, reason, detail)
			}
		}
		if out.Outcome == feedBookOutcomeAccepted {
			d.accepted++
			d.lastReceipt = in.ReceiptMs
			st.Obs[len(st.Obs)-1].Seq = uint64(d.accepted)
			if d.all {
				d.records = append(d.records, pairReplayBookRecord{EventMs: p.EventMs, RecvMs: in.ReceiptMs, Bids: truncateLevels(p.Bids, d.need.Depth), Asks: truncateLevels(p.Asks, d.need.Depth)})
			}
		}
		for i := range st.Markers {
			st.Markers[i].Session = 1
		}
		return out.Outcome, out.Reason
	default:
		return feedBookOutcomeRejected, "unknown book input kind"
	}
	if in.ReceiptMs > st.HighRecvMs {
		st.HighRecvMs = in.ReceiptMs
	}
	st.trim(in.ReceiptMs)
	return in.Kind, ""
}

func (d *pairPaperBookReducer) clone(all bool) *pairPaperBookReducer {
	c := &pairPaperBookReducer{member: d.member, need: d.need, state: d.state, accepted: d.accepted, markers: d.markers, lastReceipt: d.lastReceipt, epoch: d.epoch, incomplete: d.incomplete, all: all}
	c.owner.connected, c.owner.obsSession, c.owner.bookSeq = d.owner.connected, d.owner.obsSession, d.owner.bookSeq
	c.state.Obs = append([]feedBookObservation(nil), d.state.Obs...)
	c.state.Markers = append([]feedBookMarker(nil), d.state.Markers...)
	if all {
		c.records = d.records[:len(d.records):len(d.records)]
		c.allMarkers = d.allMarkers[:len(d.allMarkers):len(d.allMarkers)]
	}
	return c
}

func (d *pairPaperBookReducer) stream(runID string, through int64) *pairReplaySealedStream {
	st := &d.state
	marks := append([]feedBookMarker(nil), st.Markers...)
	for i := range marks {
		marks[i].Seq = uint64(d.markers - len(marks) + i + 1)
		marks[i].Session = 1
	}
	from := st.StartedMs
	if st.RetainedFromMs > through-d.need.WindowMs {
		from = st.RetainedFromMs
	}
	end := through
	if !st.Covered {
		end = st.HighRecvMs - 1
	}
	return &pairReplaySealedStream{member: d.member, key: st.Key,
		doc: pairReplayBookDoc{V: 1, MemberID: d.member.MemberID, Instance: boundedBookInstance("paper/" + runID), CoverageFromMs: from, CoverageThroughMs: end},
		obs: append([]feedBookObservation(nil), st.Obs...), marks: marks}
}

type pairPaperBooks struct {
	mu        sync.Mutex
	rec       *pairRecorder
	src       *recordingQuoteSource
	bindings  map[string]pairPaperBookBinding
	members   map[string]pairContractMember
	need      pairBookNeed
	intake    pairPaperBookIntake
	cancel    context.CancelFunc
	conn      *websocket.Conn
	done      chan struct{}
	ready     chan struct{}
	startErr  error
	started   bool
	terminal  bool
	stopErr   error
	stopCause string
	epoch     int
	tail      *pairRecLog
	cacheLock chan struct{}
	cache     pairPaperBookCache
}

type pairPaperBookCache struct {
	seq      int64
	prefix   string
	size     int64
	high     int64
	overflow bool
	reducers map[string]*pairPaperBookReducer
}

func newPairPaperBooks(rec *pairRecorder, src *recordingQuoteSource) *pairPaperBooks {
	b := &pairPaperBooks{rec: rec, src: src, bindings: map[string]pairPaperBookBinding{}, members: map[string]pairContractMember{}, cacheLock: make(chan struct{}, 1)}
	rec.books = b
	return b
}

func (b *pairPaperBooks) bind(c basketContract) error {
	b.need = c.Pair.BookNeed
	for _, m := range c.pairMembersSorted() {
		if m.Source != pairSourceSealedFeed {
			continue
		}
		if m.Provider != entryProviderHyperliquid {
			return fmt.Errorf("sealed paper books require Hyperliquid")
		}
		b.members[m.MemberID] = m
		b.bindings[m.MemberID] = pairPaperBookBinding{m.MemberID, m.Provider, m.Instrument, pairSourceContractFor(m.Provider, m.Source)}
	}
	if b.workingReserve() > feedBookMemoryBudget {
		return errPairPaperBookMemory
	}
	if b.tail != nil {
		return b.validateBindings(b.tail)
	}
	if b.rec.log != nil {
		return b.validateBindings(b.rec.log)
	}
	return nil
}

func (b *pairPaperBooks) validateBindings(l *pairRecLog) error {
	reducers := map[string]*pairPaperBookReducer{}
	for id, m := range b.members {
		reducers[id] = newPairPaperBookReducer(m, b.need, false)
	}
	for _, row := range l.rows {
		if st := row.Event.Start; st != nil && pairPaperBookCall(st.Kind) {
			var req pairPaperBookRequest
			if err := pairReplayStrictDecode(st.Request, &req); err != nil {
				return err
			}
			for i, id := range req.Members {
				if _, ok := b.members[id]; !ok || i > 0 && req.Members[i-1] >= id {
					return errors.New("book read changes its admitted member set")
				}
			}
		}
		if in := row.Event.BookInput; in != nil {
			if want, ok := b.bindings[in.Binding.MemberID]; !ok || want != in.Binding {
				return fmt.Errorf("book input %d changes the admitted source binding", row.Seq)
			}
			d := reducers[in.Binding.MemberID]
			if in.Kind == "subscribed" && !pairPaperBookAcknowledgement(in.Raw, in.Binding.Instrument) {
				return fmt.Errorf("book input %d has no matching slow subscription acknowledgement", row.Seq)
			}
			if in.Kind == "epoch" && in.Epoch <= d.epoch {
				return fmt.Errorf("book input %d repeats its socket epoch", row.Seq)
			}
			if in.Epoch != d.epoch && in.Kind != "epoch" && !(in.Kind == "stop" && in.LostTicketFrom > 0) {
				return fmt.Errorf("book input %d changes epoch without a coverage break", row.Seq)
			}
			out, reason := d.apply(*in)
			if out != in.Outcome || reason != "" && reason != in.Reason {
				return fmt.Errorf("book input %d changes its canonical disposition", row.Seq)
			}
		}
	}
	return nil
}

func (b *pairPaperBooks) intakeNow() pairPaperBookIntake {
	b.mu.Lock()
	defer b.mu.Unlock()
	v := b.intake
	v.WallMs = b.rec.now().UnixMilli()
	if v.WallMs > b.intake.HighMs {
		b.intake.HighMs = v.WallMs
	}
	return v
}

func (b *pairPaperBooks) restartableLocked() bool {
	if !b.started || b.startErr != nil || b.terminal || b.stopCause != "" || !b.intake.Failed || b.rec.blocked.Load() || b.rec.limitStopped.Load() {
		return false
	}
	select {
	case <-b.done:
		return true
	default:
		return false
	}
}

func (b *pairPaperBooks) start() (err error) {
	b.mu.Lock()
	if b.restartableLocked() {
		ctx, cancel := context.WithCancel(context.Background())
		b.cancel = cancel
		b.done = make(chan struct{})
		b.stopErr = nil
		b.intake.Failed, b.intake.Stopped = false, false
		b.mu.Unlock()
		go b.receive(ctx)
		return nil
	}
	if b.started {
		ready := b.ready
		b.mu.Unlock()
		<-ready
		b.mu.Lock()
		err = b.startErr
		b.mu.Unlock()
		return err
	}
	ctx, cancel := context.WithCancel(context.Background())
	b.cancel = cancel
	b.done = make(chan struct{})
	b.ready = make(chan struct{})
	b.started = true
	ready := b.ready
	b.mu.Unlock()
	defer func() { b.mu.Lock(); b.startErr = err; b.mu.Unlock(); close(ready) }()
	startup, finish := context.WithTimeout(ctx, pairPaperBookJoin)
	defer finish()
	l, err := readVerifiedPairRecLogContext(startup, b.rec.file, b.rec.runID, b.rec.partition, b.rec.contractFP)
	if err != nil {
		cancel()
		close(b.done)
		return err
	}
	if err = b.validateBindings(l); err != nil {
		cancel()
		close(b.done)
		return err
	}
	b.mu.Lock()
	for _, row := range l.rows {
		if in := row.Event.BookInput; in != nil {
			b.intake.Captured = in.Ticket
			b.intake.Committed = in.Ticket
			if in.ReceiptMs > b.intake.HighMs {
				b.intake.HighMs = in.ReceiptMs
			}
			if in.CoverageAtMs > b.intake.HighMs {
				b.intake.HighMs = in.CoverageAtMs
			}
			if in.Epoch > b.epoch {
				b.epoch = in.Epoch
			}
		}
		if st := row.Event.Start; st != nil && st.BookIntake != nil && st.BookIntake.WallMs > b.intake.HighMs {
			b.intake.HighMs = st.BookIntake.WallMs
		}
	}
	b.mu.Unlock()
	go b.receive(ctx)
	return nil
}

func (b *pairPaperBooks) cancelIntake() {
	b.mu.Lock()
	cancel, conn := b.cancel, b.conn
	b.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	if conn != nil {
		_ = conn.Close()
	}
}

func (b *pairPaperBooks) stop(reason string) error {
	b.mu.Lock()
	started, done := b.started, b.done
	if b.stopCause == "" {
		b.stopCause = reason
	}
	b.mu.Unlock()
	if !started {
		return nil
	}
	b.cancelIntake()
	select {
	case <-done:
	case <-time.After(pairPaperBookJoin):
		return fmt.Errorf("paper book receiver did not stop within its join bound")
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.stopErr
}

func (b *pairPaperBooks) append(ctx context.Context, in pairPaperBookInput, d *pairPaperBookReducer) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), pairPaperBookJoin)
	defer cancel()
	b.mu.Lock()
	if in.Kind == "stop" && b.stopErr != nil {
		in.IncompleteReason = boundedReason(b.stopErr.Error())
	}
	if in.Kind == "stop" && b.intake.Captured > b.intake.Committed {
		in.LostTicketFrom = b.intake.Committed + 1
	}
	b.intake.Captured++
	in.Ticket = b.intake.Captured
	in.ReceiptMs = b.rec.now().UnixMilli()
	in.ClockViolation = in.ReceiptMs < b.intake.HighMs
	if in.ClockViolation {
		in.CoverageAtMs = b.intake.HighMs
	}
	if in.ReceiptMs > b.intake.HighMs {
		b.intake.HighMs = in.ReceiptMs
	}
	b.mu.Unlock()
	if d != nil {
		reason := in.Reason
		in.Outcome, in.Reason = d.apply(in)
		if in.Reason == "" {
			in.Reason = reason
		}
		if d.incomplete != "" && (in.Reason == d.incomplete || in.Kind == "stop") {
			in.IncompleteReason = d.incomplete
		}
	}
	if pairPaperBookReceiptGate != nil {
		pairPaperBookReceiptGate()
	}
	for !b.rec.mu.TryLock() {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Millisecond):
		}
	}
	defer b.rec.mu.Unlock()
	if b.rec.blocked.Load() && in.Kind != "stop" {
		return errors.New("the recording barrier holds book intake")
	}
	err := b.rec.appendLockedContext(ctx, pairRecEvent{Kind: pairRecEventBookInput, BookInput: &in})
	if err != nil {
		b.rec.block(err.Error())
		b.mu.Lock()
		b.intake.Failed = true
		b.mu.Unlock()
		return err
	}
	b.rec.fault("exit-after-book-input-commit")
	b.mu.Lock()
	b.intake.Committed = in.Ticket
	b.mu.Unlock()
	return nil
}

func (b *pairPaperBooks) receive(ctx context.Context) {
	defer close(b.done)
	ids := make([]string, 0, len(b.members))
	for id := range b.members {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	reducers, err := b.committedReducers(ctx, b.rec.headNow())
	if err != nil {
		b.setError(err)
		return
	}
	appendAll := func(c context.Context, kind, reason string) error {
		for _, id := range ids {
			in := pairPaperBookInput{Binding: b.bindings[id], Epoch: b.epoch, Kind: kind, Outcome: kind, Reason: boundedReason(reason)}
			if err := b.append(c, in, reducers[id]); err != nil {
				return err
			}
		}
		return nil
	}
	rejectAll := func(raw []byte, reason string) error {
		for _, id := range ids {
			if err := b.append(ctx, pairPaperBookInput{Binding: b.bindings[id], Epoch: b.epoch, Kind: "rejected", Raw: pairRecBound(raw, feedBookMaxFrameBytes), Reason: boundedReason(reason)}, reducers[id]); err != nil {
				return err
			}
		}
		return nil
	}
	defer func() {
		terminal, cancel := context.WithTimeout(context.Background(), pairPaperBookJoin)
		defer cancel()
		b.mu.Lock()
		reason := b.stopCause
		b.mu.Unlock()
		if reason == "" {
			reason = "receiver stopped"
		}
		err := appendAll(terminal, "stop", reason)
		b.mu.Lock()
		b.intake.Stopped = true
		if err != nil {
			b.stopErr = err
		}
		b.mu.Unlock()
	}()
	if b.uncommitted() {
		if err := appendAll(ctx, "stop", "captured input did not commit before the receiver restarted"); err != nil {
			b.setError(err)
			return
		}
	}
	for ctx.Err() == nil {
		b.epoch++
		if err := appendAll(ctx, "epoch", ""); err != nil {
			b.setError(err)
			return
		}
		if err := pairPublicEgressReady(ctx, b.src.egress); err != nil {
			b.setError(err)
			return
		}
		conn, resp, err := egressWebsocketDialer(feedSocketDialTimeout).DialContext(ctx, pairPaperBookEndpoint(), nil)
		if resp != nil && resp.Body != nil {
			_ = resp.Body.Close()
		}
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			if err = appendAll(ctx, "disconnect", "websocket dial failed"); err != nil {
				b.setError(err)
				return
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(time.Second):
				continue
			}
		}
		b.mu.Lock()
		b.conn = conn
		b.mu.Unlock()
		conn.SetReadLimit(feedBookMaxFrameBytes)
		for _, id := range ids {
			_ = conn.SetWriteDeadline(time.Now().Add(time.Second))
			err = conn.WriteJSON(map[string]any{"method": "subscribe", "subscription": map[string]any{"type": "l2Book", "coin": b.members[id].Instrument, "fast": false}})
			if err != nil {
				break
			}
		}
		for err == nil && ctx.Err() == nil {
			_ = conn.SetReadDeadline(time.Now().Add(time.Duration(feedBookMaxGapMs) * time.Millisecond))
			var raw []byte
			_, raw, err = conn.ReadMessage()
			if err != nil {
				if ctx.Err() == nil {
					var timeout net.Error
					switch {
					case errors.Is(err, websocket.ErrReadLimit):
						if saveErr := rejectAll(raw, "frame exceeds the 16 KiB read limit; unread bytes are unavailable"); saveErr != nil {
							err = saveErr
						}
					case errors.As(err, &timeout) && timeout.Timeout():
						if saveErr := appendAll(ctx, "silence", "no received frame within 10000 ms"); saveErr != nil {
							err = saveErr
						}
					case !websocket.IsCloseError(err, websocket.CloseNormalClosure, websocket.CloseGoingAway):
						if saveErr := rejectAll(raw, "socket read failed; only returned bytes were preserved"); saveErr != nil {
							err = saveErr
						}
					}
				}
				break
			}
			var env feedSocketEnvelope
			if json.Unmarshal(raw, &env) != nil {
				err = rejectAll(raw, "malformed websocket envelope")
				continue
			}
			if env.Channel == "subscriptionResponse" {
				var ack struct {
					Method       string `json:"method"`
					Subscription struct {
						Type     string `json:"type"`
						Coin     string `json:"coin"`
						Fast     bool   `json:"fast"`
						NSigFigs *int   `json:"nSigFigs"`
						Mantissa *int   `json:"mantissa"`
					} `json:"subscription"`
				}
				if json.Unmarshal(env.Data, &ack) != nil || ack.Method != "subscribe" || ack.Subscription.Type != "l2Book" || ack.Subscription.Fast || ack.Subscription.NSigFigs != nil || ack.Subscription.Mantissa != nil {
					err = rejectAll(raw, "incompatible subscription acknowledgement")
					continue
				}
				matched := false
				for _, id := range ids {
					if b.members[id].Instrument == ack.Subscription.Coin {
						matched = true
						err = b.append(ctx, pairPaperBookInput{Binding: b.bindings[id], Epoch: b.epoch, Kind: "subscribed", Raw: raw}, reducers[id])
						if err != nil {
							break
						}
					}
				}
				if !matched {
					err = rejectAll(raw, "subscription acknowledgement has no admitted instrument")
				}
			} else if env.Channel == "l2Book" {
				var coin struct {
					Coin string `json:"coin"`
				}
				_ = json.Unmarshal(env.Data, &coin)
				matched := false
				for _, id := range ids {
					if b.members[id].Instrument == coin.Coin {
						matched = true
						for ctx.Err() == nil && b.rec.now().UnixMilli() == reducers[id].lastReceipt {
							pairPaperWall.Sleep(time.Millisecond)
						}
						err = b.append(ctx, pairPaperBookInput{Binding: b.bindings[id], Epoch: b.epoch, Kind: "frame", Raw: raw}, reducers[id])
						if err != nil {
							break
						}
					}
				}
				if !matched {
					err = rejectAll(raw, "book frame has no admitted instrument")
				}
			} else if env.Channel != "pong" {
				err = rejectAll(raw, "unexpected websocket channel")
			}
		}
		_ = conn.Close()
		b.mu.Lock()
		b.conn = nil
		b.mu.Unlock()
		if ctx.Err() != nil {
			return
		}
		if b.rec.blocked.Load() || b.uncommitted() {
			b.setError(errors.New("book append failed"))
			return
		}
		if err = appendAll(ctx, "disconnect", "socket closed, unreadable, oversized, or silent input"); err != nil {
			b.setError(err)
			return
		}
	}
}

func (b *pairPaperBooks) setError(err error) {
	b.mu.Lock()
	b.stopErr = err
	b.intake.Failed = true
	b.mu.Unlock()
}

func (b *pairPaperBooks) uncommitted() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.intake.Captured > b.intake.Committed
}

func (b *pairPaperBooks) forPrefix(ctx context.Context, head pairRecHead, visit func(pairPaperBookInput) error) error {
	if head.Seq <= 0 || !pairReplaySHAPattern.MatchString(head.Prefix) {
		return errors.New("book read has no committed prefix")
	}
	b.rec.mu.Lock()
	l := b.rec.log
	b.rec.mu.Unlock()
	if l != nil && head.Seq <= int64(len(l.rows)) {
		if got, ok := l.prefixAt(head.Seq); !ok || got != head.Prefix {
			return errors.New("book prefix differs from recording")
		}
		for _, row := range l.rows {
			if row.Seq > head.Seq {
				break
			}
			if in := row.Event.BookInput; in != nil {
				if err := visit(*in); err != nil {
					return err
				}
			}
		}
		return nil
	}
	end, err := b.scanCommitted(ctx, pairRecHead{}, 0, head, func(_ pairRecRow, in *pairPaperBookInput, _ int64) error {
		if in == nil {
			return nil
		}
		return visit(*in)
	})
	if err != nil {
		return err
	}
	if end != head {
		return errors.New("book prefix differs from committed storage")
	}
	return nil
}

func (b *pairPaperBooks) scanCommitted(ctx context.Context, after pairRecHead, size int64, head pairRecHead, visit func(pairRecRow, *pairPaperBookInput, int64) error) (pairRecHead, error) {
	rows, err := b.rec.file.db.QueryContext(ctx, `SELECT seq,schema_version,partition,contract_fingerprint,event_kind,call_id,prev_prefix_hash,prefix_hash,payload_hash,payload FROM basket_replay_inputs WHERE run_id=? AND seq>? AND seq<=? ORDER BY seq`, b.rec.runID, after.Seq, head.Seq)
	if err != nil {
		return after, err
	}
	defer rows.Close()
	seq, prefix := after.Seq, after.Prefix
	for rows.Next() {
		if err = ctx.Err(); err != nil {
			return pairRecHead{Seq: seq, Prefix: prefix}, err
		}
		var row pairRecRow
		if err = rows.Scan(&row.Seq, &row.Schema, &row.Partition, &row.ContractFP, &row.Kind, &row.CallID, &row.PrevPrefix, &row.Prefix, &row.PayloadHash, &row.Payload); err != nil {
			return pairRecHead{Seq: seq, Prefix: prefix}, err
		}
		size += int64(len(row.Payload))
		if size > pairPaperBookLogLimit {
			return pairRecHead{Seq: seq, Prefix: prefix}, errors.New("book prefix exceeds recording bound")
		}
		if row.Seq != seq+1 || row.Schema != pairRecSchemaBooks || row.Partition != b.rec.partition || row.ContractFP != b.rec.contractFP || row.PrevPrefix != prefix || sha256Hex(row.Payload) != row.PayloadHash {
			return pairRecHead{Seq: seq, Prefix: prefix}, errors.New("book read encountered an altered committed prefix")
		}
		if pairRecPrefixHash(row.Schema, b.rec.runID, row.Seq, row.Partition, row.ContractFP, row.Kind, row.CallID, row.PrevPrefix, row.PayloadHash) != row.Prefix {
			return pairRecHead{Seq: seq, Prefix: prefix}, errors.New("book read encountered an invalid prefix hash")
		}
		var in *pairPaperBookInput
		if row.Kind == pairRecEventBookInput {
			ev, err := pairRecDecode(row.Payload)
			if err != nil {
				return pairRecHead{Seq: seq, Prefix: prefix}, err
			}
			if ev.BookInput == nil || ev.Kind != row.Kind || ev.V != row.Schema || ev.CallID != row.CallID {
				return pairRecHead{Seq: seq, Prefix: prefix}, errors.New("book input body differs from its committed row")
			}
			in = ev.BookInput
		}
		if err = visit(row, in, size); err != nil {
			return pairRecHead{Seq: seq, Prefix: prefix}, err
		}
		seq, prefix = row.Seq, row.Prefix
	}
	if err = rows.Err(); err != nil {
		return pairRecHead{Seq: seq, Prefix: prefix}, err
	}
	return pairRecHead{Seq: seq, Prefix: prefix}, nil
}

func (b *pairPaperBooks) committedReducers(ctx context.Context, head pairRecHead) (map[string]*pairPaperBookReducer, error) {
	reducers := map[string]*pairPaperBookReducer{}
	from := pairRecHead{}
	var size int64
	if b.cacheLock != nil && head.Seq > 0 && pairReplaySHAPattern.MatchString(head.Prefix) {
		b.rec.mu.Lock()
		recorded := b.rec.log != nil && head.Seq <= int64(len(b.rec.log.rows))
		b.rec.mu.Unlock()
		if !recorded {
			select {
			case b.cacheLock <- struct{}{}:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
			if c := &b.cache; c.reducers != nil && c.seq <= head.Seq {
				from, size = pairRecHead{Seq: c.seq, Prefix: c.prefix}, c.size
				for id, d := range c.reducers {
					reducers[id] = d.clone(false)
				}
			}
			<-b.cacheLock
		}
	}
	for id, m := range b.members {
		if reducers[id] == nil {
			reducers[id] = newPairPaperBookReducer(m, b.need, false)
		}
	}
	if from.Seq == 0 {
		err := b.forPrefix(ctx, head, func(in pairPaperBookInput) error {
			if d := reducers[in.Binding.MemberID]; d != nil {
				d.apply(in)
			}
			return nil
		})
		return reducers, err
	}
	end, err := b.scanCommitted(ctx, from, size, head, func(_ pairRecRow, in *pairPaperBookInput, _ int64) error {
		if in != nil {
			if d := reducers[in.Binding.MemberID]; d != nil {
				d.apply(*in)
			}
		}
		return nil
	})
	if err == nil && end != head {
		err = errors.New("book prefix differs from committed storage")
	}
	return reducers, err
}

func (b *pairPaperBooks) recordsOver(d *pairPaperBookReducer) bool {
	return b.workingReserve()+len(d.records)*(feedBookMemRecordBase+2*feedBookMaxLevels*feedBookMemLevelBytes)+len(d.allMarkers)*feedEstimateBookMarkerBytes > feedBookMemoryBudget
}

func (b *pairPaperBooks) cachedReduce(ctx context.Context, head pairRecHead, id string, at int64, all bool) (*pairPaperBookReducer, bool, error) {
	if head.Seq <= 0 || !pairReplaySHAPattern.MatchString(head.Prefix) || b.rec.file == nil || b.cacheLock == nil {
		return nil, false, nil
	}
	b.rec.mu.Lock()
	recorded := b.rec.log != nil && head.Seq <= int64(len(b.rec.log.rows))
	b.rec.mu.Unlock()
	if recorded {
		return nil, false, nil
	}
	select {
	case b.cacheLock <- struct{}{}:
	case <-ctx.Done():
		return nil, true, ctx.Err()
	}
	defer func() { <-b.cacheLock }()
	c := &b.cache
	if head.Seq < c.seq || at < c.high {
		return nil, false, nil
	}
	if head.Seq == c.seq && head.Prefix != c.prefix {
		return nil, true, errors.New("book prefix differs from committed storage")
	}
	if c.reducers == nil {
		c.reducers = map[string]*pairPaperBookReducer{}
		for mid, m := range b.members {
			c.reducers[mid] = newPairPaperBookReducer(m, b.need, true)
		}
	}
	frontier := true
	var tail []pairPaperBookInput
	end, err := b.scanCommitted(ctx, pairRecHead{Seq: c.seq, Prefix: c.prefix}, c.size, head, func(row pairRecRow, in *pairPaperBookInput, size int64) error {
		if in != nil {
			if want, ok := b.bindings[in.Binding.MemberID]; ok && in.Binding != want {
				return errors.New("book input source binding changed")
			}
			if d := c.reducers[in.Binding.MemberID]; d != nil {
				if frontier && in.ReceiptMs <= at {
					d.apply(*in)
					if in.ReceiptMs > c.high {
						c.high = in.ReceiptMs
					}
					if d.all && b.recordsOver(d) {
						c.overflow = true
						for _, r := range c.reducers {
							r.all, r.records, r.allMarkers = false, nil, nil
						}
					}
				} else {
					frontier = false
					tail = append(tail, *in)
				}
			}
		}
		if frontier {
			c.seq, c.prefix, c.size = row.Seq, row.Prefix, size
		}
		return nil
	})
	if err != nil {
		return nil, true, err
	}
	if end != head {
		return nil, true, errors.New("book prefix differs from committed storage")
	}
	base, ok := c.reducers[id]
	if !ok {
		return nil, true, errors.New("unknown sealed book member")
	}
	if all && c.overflow {
		return nil, true, errPairPaperBookMemory
	}
	d := base.clone(all)
	for _, in := range tail {
		if in.Binding.MemberID != id || in.ReceiptMs > at {
			continue
		}
		d.apply(in)
	}
	if all && b.recordsOver(d) {
		return nil, true, errPairPaperBookMemory
	}
	return d, true, nil
}

func (b *pairPaperBooks) read(ctx context.Context, kind string, req pairPaperBookRequest) (pairPaperBookBoundary, error) {
	var boundary pairPaperBookBoundary
	if !b.rec.consumingNow() && b.rec.mode != pairRecPlayback {
		if err := b.start(); err != nil {
			return boundary, err
		}
		wait := b.src.timeout
		if wait > 250*time.Millisecond {
			wait = 250 * time.Millisecond
		}
		until := time.Now().Add(wait)
		for b.rec.now().UnixMilli() <= req.ThroughMs && time.Now().Before(until) {
			if err := ctx.Err(); err != nil {
				return boundary, err
			}
			pairPaperWall.Sleep(time.Millisecond)
		}
	}
	res, saved, err := b.rec.call(ctx, pairRecCall{Kind: kind, Request: pairPaperJSON(req)}, func(ctx context.Context, h *pairRecHandle) pairRecResult {
		v := pairPaperBookBoundary{EvaluationID: boundedBookInstance("paper/" + b.rec.runID + "/" + h.parentID), Generation: 1, Disposition: "unavailable"}
		if h.start.BookPrefix != nil {
			v.Prefix = *h.start.BookPrefix
		}
		if intake := h.start.BookIntake; intake != nil {
			v.Ticket = intake.Captured
			if !intake.Failed && !intake.Stopped && intake.Captured == intake.Committed && intake.WallMs > req.ThroughMs && intake.WallMs >= intake.HighMs {
				v.AcknowledgedMs = req.ThroughMs
				v.Disposition = "available"
			}
		}
		if v.Disposition == "available" {
			if err := b.checkRead(ctx, v, kind, req); err != nil {
				if errors.Is(err, errPairPaperBookMemory) {
					b.mu.Lock()
					b.stopErr = err
					b.intake.Failed = true
					b.terminal = true
					b.mu.Unlock()
					b.cancelIntake()
				}
				v.Disposition = "unavailable"
				v.Reason = boundedReason(err.Error())
			}
		}
		return pairRecResult{Class: pairRecClassResult, Result: pairPaperJSON(v)}
	})
	if err != nil {
		return boundary, err
	}
	if res.Class != pairRecClassResult {
		return boundary, errors.New(res.Cause)
	}
	if err = pairReplayStrictDecode(res.Result, &boundary); err != nil {
		return boundary, err
	}
	if saved == nil || saved.Start.BookPrefix == nil || boundary.Prefix != *saved.Start.BookPrefix {
		return boundary, errors.New("book read does not bind its selected start prefix")
	}
	if boundary.Disposition != "available" || boundary.AcknowledgedMs != req.ThroughMs {
		return boundary, fmt.Errorf("book read is unavailable: %s", boundary.Reason)
	}
	return boundary, nil
}

func (b *pairPaperBooks) reduce(ctx context.Context, boundary pairPaperBookBoundary, id string, at int64, all bool) (*pairPaperBookReducer, error) {
	m, ok := b.members[id]
	if !ok {
		return nil, errors.New("unknown sealed book member")
	}
	if d, ok, err := b.cachedReduce(ctx, boundary.Prefix, id, at, all); ok {
		return d, err
	}
	d := newPairPaperBookReducer(m, b.need, all)
	err := b.forPrefix(ctx, boundary.Prefix, func(in pairPaperBookInput) error {
		if in.Binding.MemberID != id {
			return nil
		}
		if in.Binding != b.bindings[id] {
			return errors.New("book input source binding changed")
		}
		if in.ReceiptMs > at {
			return nil
		}
		d.apply(in)
		if all && b.workingReserve()+len(d.records)*(feedBookMemRecordBase+2*feedBookMaxLevels*feedBookMemLevelBytes)+len(d.allMarkers)*feedEstimateBookMarkerBytes > feedBookMemoryBudget {
			return errPairPaperBookMemory
		}
		return nil
	})
	return d, err
}

func (b *pairPaperBooks) seal(rt *basketRuntime, at time.Time) *marketSnapshot {
	ids := make([]string, 0, len(b.members))
	for id := range b.members {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	ctx, cancel := context.WithTimeout(context.Background(), b.src.timeout)
	defer cancel()
	v, err := b.read(ctx, pairRecKindBookSeal, pairPaperBookRequest{Members: ids, ThroughMs: at.UnixMilli()})
	snap := &marketSnapshot{Version: marketSnapshotVersion, EvaluationID: v.EvaluationID, ConfigGeneration: 1, SealedAt: at, Deadline: at, books: map[feedObservationKey]*marketSnapshotBook{}}
	if err != nil {
		return snap
	}
	for _, id := range ids {
		d, err := b.reduce(ctx, v, id, at.UnixMilli(), false)
		if err != nil {
			continue
		}
		st := d.stream(b.rec.runID, at.UnixMilli())
		snap.books[st.key] = st.freeze(contractBookNeed(b.need.WindowMs, b.need.Capacity, b.need.Depth), at.UnixMilli())
	}
	return snap
}

func (b *pairPaperBooks) observation(ctx context.Context, id string, at int64, kind string) (pairObservation, error) {
	v, err := b.read(ctx, kind, pairPaperBookRequest{Members: []string{id}, ThroughMs: at})
	if err != nil {
		return pairObservation{}, err
	}
	d, err := b.reduce(ctx, v, id, at, false)
	if err != nil {
		return pairObservation{}, err
	}
	st := d.stream(b.rec.runID, at)
	snap := &marketSnapshot{EvaluationID: v.EvaluationID, ConfigGeneration: 1, SealedAt: time.UnixMilli(at), Deadline: time.UnixMilli(at), books: map[feedObservationKey]*marketSnapshotBook{st.key: st.freeze(contractBookNeed(b.need.WindowMs, b.need.Capacity, b.need.Depth), at)}}
	m := b.members[id]
	partition, err := parseRiskPartition(b.src.partition)
	if err != nil {
		return pairObservation{}, err
	}
	return pairSealedReader{}.readQuote(ctx, pairMemberRead{Partition: partition, Member: m, BookNeed: b.need, SourceContract: b.bindings[id].SourceContract, StartMs: at, Snapshot: snap, SnapshotID: v.EvaluationID, Generation: 1})
}

func (b *pairPaperBooks) events(ctx context.Context, key pairMemberKey, after, through int64) (pairVirtualMarketWindow, error) {
	v, err := b.read(ctx, pairRecKindBookWindow, pairPaperBookRequest{Members: []string{key.MemberID}, FromMs: after, ThroughMs: through})
	if err != nil {
		return pairVirtualMarketWindow{}, err
	}
	return b.windowAt(ctx, v, key, after, through)
}

func (b *pairPaperBooks) windowAt(ctx context.Context, v pairPaperBookBoundary, key pairMemberKey, after, through int64) (pairVirtualMarketWindow, error) {
	if b.recordedPrefix(v.Prefix) {
		ctx = context.WithoutCancel(ctx)
	}
	d, err := b.reduce(ctx, v, key.MemberID, through, true)
	if err != nil {
		return pairVirtualMarketWindow{}, err
	}
	if !d.state.Covered {
		return pairVirtualMarketWindow{}, errors.New("book window has no acknowledged subscription coverage")
	}
	events := 0
	for _, r := range d.records {
		if r.EventMs > after && r.EventMs <= through && r.RecvMs <= through {
			events++
		}
	}
	m := b.members[key.MemberID]
	spec, _ := pairSourceSpecFor(m.Provider, m.Source)
	if b.workingReserve()+len(d.records)*(feedBookMemRecordBase+2*feedBookMaxLevels*feedBookMemLevelBytes)+len(d.allMarkers)*feedEstimateBookMarkerBytes+events*(pairPayloadMaxBytes(spec, m, b.need)+pairObservationFixed) > feedBookMemoryBudget {
		return pairVirtualMarketWindow{}, errPairPaperBookMemory
	}
	st := d.stream(b.rec.runID, through)
	st.obs = nil
	for i, r := range d.records {
		st.obs = append(st.obs, feedBookObservation{Seq: uint64(i + 1), EventMs: r.EventMs, RecvMs: r.RecvMs, Session: 1, Identity: bookIdentity(st.key, r.EventMs, r.Bids, r.Asks), BidDepth: len(r.Bids), AskDepth: len(r.Asks), Bids: r.Bids, Asks: r.Asks})
	}
	st.marks = nil
	for i, m := range d.allMarkers {
		st.marks = append(st.marks, feedBookMarker{Seq: uint64(i + 1), AtMs: m.AtMs, Session: 1, Reason: m.Reason, Detail: m.Detail})
	}
	if len(d.records) > 0 {
		st.doc.CoverageFromMs = d.records[0].RecvMs
		if d.state.StartedMs < st.doc.CoverageFromMs {
			st.doc.CoverageFromMs = d.state.StartedMs
		}
	}
	src := &pairReplayQuoteSource{runID: b.rec.runID, bound: b.rec.contractFP, partition: b.src.partition, need: b.need, members: b.members, sealed: map[string]*pairReplaySealedStream{key.MemberID: st}}
	return (&pairReplayMarket{src: src, clock: func() time.Time { return time.UnixMilli(through) }}).Events(ctx, key, after, through)
}

func (l *pairRecLog) admitBookInput(row pairRecRow) error {
	in := row.Event.BookInput
	if l.schema != pairRecSchemaBooks || in == nil || row.CallID != "" || in.Ticket <= 0 || in.Epoch <= 0 || in.ReceiptMs <= 0 || len(in.Raw) > feedBookMaxFrameBytes {
		return errors.New("invalid book input schema, ticket, epoch, time, or size")
	}
	if in.Binding.MemberID == "" || in.Binding.Provider != entryProviderHyperliquid || in.Binding.SourceContract != pairSourceContractFor(entryProviderHyperliquid, pairSourceSealedFeed) || in.Binding.Instrument == "" {
		return errors.New("invalid book source binding")
	}
	switch in.Kind {
	case "epoch", "subscribed", "frame", "disconnect", "rejected", "silence", "stop":
	default:
		return errors.New("unknown book input disposition")
	}
	if in.Outcome == "" {
		return errors.New("book input has no disposition evidence")
	}
	if (in.Ticket != l.bookTicket+1 && (in.Kind != "stop" || in.LostTicketFrom != l.bookTicket+1 || in.LostTicketFrom >= in.Ticket)) || in.Epoch < l.bookEpoch || in.Epoch > l.bookEpoch+1 || l.bookTicket == 0 && in.Kind != "epoch" {
		return errors.New("book ticket or epoch order changed")
	}
	if in.ClockViolation && in.CoverageAtMs < in.ReceiptMs {
		return errors.New("clock marker precedes its raw receipt")
	}
	if in.ReceiptMs <= l.bookAck && (!in.ClockViolation || in.CoverageAtMs <= l.bookAck) {
		return errors.New("book receipt reopens an acknowledged availability prefix")
	}
	l.bookTicket, l.bookEpoch = in.Ticket, in.Epoch
	return nil
}

func (l *pairRecLog) admitBookStart(row pairRecRow) error {
	s := row.Event.Start
	if !pairPaperBookCall(s.Kind) {
		if s.BookPrefix != nil || s.BookIntake != nil {
			return errors.New("non-book call carries a book boundary")
		}
		return nil
	}
	if l.schema != pairRecSchemaBooks || s.BookPrefix == nil || s.BookIntake == nil || s.BookPrefix.Seq != row.Seq-1 || s.BookPrefix.Prefix != row.PrevPrefix {
		return errors.New("book read start does not bind its immediate committed prefix")
	}
	var req pairPaperBookRequest
	if err := pairReplayStrictDecode(s.Request, &req); err != nil {
		return err
	}
	if len(req.Members) == 0 || req.ThroughMs <= 0 || req.FromMs < 0 || req.FromMs > req.ThroughMs {
		return errors.New("invalid book read interval or members")
	}
	if s.BookIntake.Committed > l.bookTicket || s.BookIntake.Captured < l.bookTicket || s.BookIntake.Captured > l.bookTicket+64 {
		return errors.New("book read intake is outside its committed prefix")
	}
	if s.BookIntake.Committed > s.BookIntake.Captured {
		return errors.New("book read acknowledges an uncaptured ticket")
	}
	return nil
}

func (l *pairRecLog) admitBookEnd(row pairRecRow) error {
	s := l.rows[l.starts[row.CallID]].Event.Start
	if !pairPaperBookCall(s.Kind) || row.Event.End == nil || row.Event.End.Class != pairRecClassResult {
		return nil
	}
	var v pairPaperBookBoundary
	if err := pairReplayStrictDecode(row.Event.End.Result, &v); err != nil {
		return err
	}
	var req pairPaperBookRequest
	_ = json.Unmarshal(s.Request, &req)
	if s.BookPrefix == nil || s.BookIntake == nil || v.Prefix != *s.BookPrefix || v.Ticket != s.BookIntake.Captured || v.Generation != 1 || v.EvaluationID != boundedBookInstance("paper/"+l.runID+"/"+row.CallID) {
		return errors.New("book terminal changes its selected prefix, intake, or evaluation identity")
	}
	if v.Disposition != "available" && v.Disposition != "unavailable" {
		return errors.New("unknown book availability disposition")
	}
	if v.Disposition == "available" && (v.AcknowledgedMs != req.ThroughMs || s.BookIntake.WallMs <= req.ThroughMs || s.BookIntake.WallMs < s.BookIntake.HighMs || s.BookIntake.Captured != s.BookIntake.Committed || s.BookIntake.Stopped || s.BookIntake.Failed) {
		return errors.New("book terminal cannot acknowledge its requested end")
	}
	if v.Disposition == "available" && v.AcknowledgedMs > l.bookAck {
		l.bookAck = v.AcknowledgedMs
	}
	return nil
}

func pairPaperEncodedLogBytes(l *pairRecLog) int64 {
	var total int64
	for _, row := range l.rows {
		line, _ := json.Marshal(pairRecLogLine{row.Seq, row.Schema, row.Partition, row.ContractFP, row.Kind, row.CallID, row.PrevPrefix, row.Prefix, row.PayloadHash, row.Payload})
		total += int64(len(line) + 1)
	}
	return total
}

func (rec *pairRecorder) bookAppendBudget(events []pairRecEvent) (int64, error) {
	if rec.schemaVersion() != pairRecSchemaBooks {
		return 0, nil
	}
	head := rec.head
	var added int64
	ordinary := false
	for _, ev := range events {
		payload, err := pairRecEncodeSchema(pairRecSchemaBooks, ev)
		if err != nil {
			return 0, err
		}
		hash := sha256Hex(payload)
		seq := head.Seq + 1
		prefix := pairRecPrefixHash(pairRecSchemaBooks, rec.runID, seq, rec.partition, rec.contractFP, ev.Kind, ev.CallID, head.Prefix, hash)
		line, err := json.Marshal(pairRecLogLine{seq, pairRecSchemaBooks, rec.partition, rec.contractFP, ev.Kind, ev.CallID, head.Prefix, prefix, hash, payload})
		if err != nil {
			return 0, err
		}
		added += int64(len(line) + 1)
		head = pairRecHead{Seq: seq, Prefix: prefix}
		if ev.Kind == pairRecEventStart || ev.Kind == pairRecEventWake || ev.Kind == pairRecEventBookInput && ev.BookInput.Kind != "stop" {
			ordinary = true
		}
	}
	limit := pairPaperBookLogLimit
	if ordinary {
		limit -= pairPaperBookReserve
	}
	if rec.encodedBytes+added > limit {
		rec.limitStopped.Store(true)
		return 0, errors.New("family-4 recording byte limit reached")
	}
	return added, nil
}

func pairPaperCapturedBook(ctx context.Context, r pairMemberRead) pairRecResult {
	o, err := (pairSealedReader{}).readQuote(ctx, r)
	if err != nil {
		return pairRecResult{Class: pairRecClassInfra, Cause: err.Error()}
	}
	t := o.FirstReceiptMs
	return pairRecResult{Class: pairRecClassResult, ReceiptMs: t, Result: pairPaperJSON(pairPaperReadResult{Payload: o.Payload, StartMs: t, RequestMs: t, ReceiptMs: t, RecvMs: t})}
}

func (b *pairPaperBooks) exportBook(l *pairRecLog, id string) ([]byte, error) {
	d := newPairPaperBookReducer(b.members[id], b.need, false)
	var records, markers bytes.Buffer
	recordN, markerN := 0, 0
	var exportErr error
	appendValue := func(buf *bytes.Buffer, n *int, v any) {
		if exportErr != nil {
			return
		}
		blob, err := json.Marshal(v)
		if err != nil {
			exportErr = err
			return
		}
		if records.Len()+markers.Len()+len(blob)+1 > pairPaperBookObjectLimit {
			exportErr = errors.New("standalone book object exceeds its byte limit")
			return
		}
		if *n > 0 {
			buf.WriteByte(',')
		}
		buf.Write(blob)
		*n++
	}
	var lastMarker int64
	d.onMarker = func(m pairReplayBookMarkerIn) {
		if m.AtMs > lastMarker {
			lastMarker = m.AtMs
		}
		appendValue(&markers, &markerN, m)
	}
	stopped := false
	var end int64
	for _, row := range l.rows {
		in := row.Event.BookInput
		if in == nil || in.Binding.MemberID != id {
			continue
		}
		out, _ := d.apply(*in)
		if out == feedBookOutcomeAccepted {
			o := d.state.Obs[len(d.state.Obs)-1]
			appendValue(&records, &recordN, pairReplayBookRecord{EventMs: o.EventMs, RecvMs: o.RecvMs, Bids: o.Bids, Asks: o.Asks})
		}
		if exportErr != nil {
			return nil, exportErr
		}
		if in.Kind == "stop" {
			stopped = true
			end = in.ReceiptMs
		} else {
			stopped = false
		}
	}
	if !stopped {
		return nil, errors.New("standalone book export requires a stopped input boundary")
	}
	if d.incomplete != "" {
		return nil, errors.New(d.incomplete)
	}
	if recordN == 0 || d.state.StartedMs <= 0 || end < d.state.StartedMs || lastMarker > end {
		return nil, errors.New("standalone book export requires records and a valid coverage interval")
	}
	header, err := json.Marshal(struct {
		V        int    `json:"v"`
		MemberID string `json:"member_id"`
		Instance string `json:"instance"`
		From     int64  `json:"coverage_from_ms"`
		Through  int64  `json:"coverage_through_ms"`
	}{1, id, boundedBookInstance("paper/" + b.rec.runID), d.state.StartedMs, end})
	if err != nil {
		return nil, err
	}
	var out bytes.Buffer
	out.Grow(len(header) + records.Len() + markers.Len() + 32)
	out.Write(header[:len(header)-1])
	out.WriteString(",\"records\":[")
	out.Write(records.Bytes())
	out.WriteString("],\"markers\":[")
	out.Write(markers.Bytes())
	out.WriteString("]}")
	if out.Len() > pairPaperBookObjectLimit {
		return nil, errors.New("standalone book object exceeds its byte limit")
	}
	return out.Bytes(), nil
}

func (b *pairPaperBooks) checkRead(ctx context.Context, v pairPaperBookBoundary, kind string, req pairPaperBookRequest) error {
	for _, id := range req.Members {
		if kind == pairRecKindBookWindow {
			m, ok := b.members[id]
			if !ok {
				return errors.New("unknown book window member")
			}
			part, err := parseRiskPartition(b.src.partition)
			if err != nil {
				return err
			}
			key := pairMemberKey{Partition: part, Provider: m.Provider, AccountKey: m.AccountKey, Instrument: m.Instrument, MemberID: id}
			if _, err = b.windowAt(ctx, v, key, req.FromMs, req.ThroughMs); err != nil {
				return err
			}
			continue
		}
		d, err := b.reduce(ctx, v, id, req.ThroughMs, false)
		if err != nil {
			return err
		}
		st := d.stream(b.rec.runID, req.ThroughMs)
		need := contractBookNeed(b.need.WindowMs, b.need.Capacity, b.need.Depth)
		snap := &marketSnapshot{EvaluationID: v.EvaluationID, ConfigGeneration: 1, Deadline: time.UnixMilli(req.ThroughMs), books: map[feedObservationKey]*marketSnapshotBook{st.key: st.freeze(need, req.ThroughMs)}}
		payload := snap.l2BookPayload(st.key, need)
		if !payload.Available {
			return errors.New(payload.Reason)
		}
		if !snap.books[st.key].Readiness.Ready {
			return errors.New(snap.books[st.key].Readiness.Detail)
		}
		raw, err := pairCanonicalJSON(payload)
		if err != nil {
			return err
		}
		o, err := pairObservationFromPayload(b.members[id], b.src.partition, b.need, raw, 0, 1)
		if err != nil {
			return err
		}
		if o.Coverage.Status != pairCoverageComplete {
			return errors.New("book read has incomplete source coverage")
		}
	}
	return nil
}

func (b *pairPaperBooks) recordedPrefix(head pairRecHead) bool {
	b.rec.mu.Lock()
	defer b.rec.mu.Unlock()
	if b.rec.log == nil {
		return false
	}
	prefix, ok := b.rec.log.prefixAt(head.Seq)
	return ok && prefix == head.Prefix
}

func (b *pairPaperBooks) joined() bool {
	b.mu.Lock()
	started, done := b.started, b.done
	b.mu.Unlock()
	if !started {
		return true
	}
	select {
	case <-done:
		return true
	default:
		return false
	}
}

func pairPaperBookAcknowledgement(raw []byte, coin string) bool {
	var env struct {
		Channel string `json:"channel"`
		Data    struct {
			Method       string `json:"method"`
			Subscription struct {
				Type     string `json:"type"`
				Coin     string `json:"coin"`
				Fast     bool   `json:"fast"`
				NSigFigs *int   `json:"nSigFigs"`
				Mantissa *int   `json:"mantissa"`
			} `json:"subscription"`
		} `json:"data"`
	}
	return json.Unmarshal(raw, &env) == nil && env.Channel == "subscriptionResponse" && env.Data.Method == "subscribe" && env.Data.Subscription.Type == "l2Book" && env.Data.Subscription.Coin == coin && !env.Data.Subscription.Fast && env.Data.Subscription.NSigFigs == nil && env.Data.Subscription.Mantissa == nil
}

func (b *pairPaperBooks) workingReserve() int {
	book := b.need.Capacity*(feedBookMemRecordBase+2*feedBookMaxLevels*feedBookMemLevelBytes) + feedBookMaxMarkers*feedEstimateBookMarkerBytes + feedEstimateBookKeyBytes
	snapshot := b.need.Capacity*(feedBookMemRecordBase+2*b.need.Depth*feedBookMemLevelBytes) + feedBookMaxMarkers*feedEstimateBookMarkerBytes
	return len(b.members)*(book+snapshot) + book + (1 << 20)
}

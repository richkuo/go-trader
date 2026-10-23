package main

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math/big"
	"sort"
	"strconv"
	"strings"
	"sync"
)

const (
	quotePaperNamespace     = "quote-replay"
	quotePaperFeeDecimals   = 12
	quotePaperSlipDecimals  = 8
	quoteQueueModelStrict   = "strict_trade_through_v1"
	quotePaperAggressorSell = "A"
	quotePaperAggressorBuy  = "B"
)

type quoteReplayLevel struct {
	Px *big.Rat
	Sz *big.Rat
}

type quoteReplayBook struct {
	RecvMs   int64
	EventMs  int64
	Session  int64
	Seq      int64
	Identity string
	Bids     []quoteReplayLevel
	Asks     []quoteReplayLevel
	BidText  string
	AskText  string
}

func (b *quoteReplayBook) bestBid() *big.Rat {
	if b == nil || len(b.Bids) == 0 {
		return nil
	}
	return b.Bids[0].Px
}

func (b *quoteReplayBook) bestAsk() *big.Rat {
	if b == nil || len(b.Asks) == 0 {
		return nil
	}
	return b.Asks[0].Px
}

func (b *quoteReplayBook) sizeAt(side string, px *big.Rat) *big.Rat {
	levels := b.Bids
	if side == "sell" {
		levels = b.Asks
	}
	for _, l := range levels {
		if l.Px.Cmp(px) == 0 {
			return new(big.Rat).Set(l.Sz)
		}
	}
	return quoteRatZero()
}

type quoteReplayTrade struct {
	RecvMs int64
	TimeMs int64
	Tid    uint64
	Side   string
	Px     *big.Rat
	Sz     *big.Rat
	PxText string
	SzText string
	Hash   string
}

type quoteMarketEvent struct {
	RecvMs int64
	Book   *quoteReplayBook
	Trade  *quoteReplayTrade
}

type quotePaperCosts struct {
	MakerRate       *big.Rat
	TakerRate       *big.Rat
	SlipRate        *big.Rat
	MinOrderValue   *big.Rat
	Participation   *big.Rat
	PlaceLatencyMs  int64
	CancelLatencyMs int64
}

type quotePaperOrder struct {
	Req            quoteOrderRequest `json:"request"`
	OID            string            `json:"oid"`
	Status         string            `json:"status"`
	AcceptedAtMs   int64             `json:"accepted_at_ms"`
	Filled         string            `json:"filled"`
	QueueAhead     string            `json:"queue_ahead,omitempty"`
	LastSeen       string            `json:"last_seen,omitempty"`
	CancelEffectMs int64             `json:"cancel_effect_ms,omitempty"`
	TerminalMs     int64             `json:"terminal_ms,omitempty"`
	Detail         string            `json:"detail,omitempty"`

	px       *big.Rat
	size     *big.Rat
	filled   *big.Rat
	qa       *big.Rat
	lastSeen *big.Rat
	order    int
}

func (o *quotePaperOrder) rests() bool {
	return o.Status == quoteOutcomeResting
}

func (o *quotePaperOrder) remaining() *big.Rat {
	return quoteRatSub(o.size, o.filled)
}

type quotePaperEventPayload struct {
	Request   *quoteOrderRequest `json:"request,omitempty"`
	Outcome   *quoteOrderOutcome `json:"outcome,omitempty"`
	Fill      *quoteRawFill      `json:"fill,omitempty"`
	SourceTid uint64             `json:"source_tid,omitempty"`
	SourceMs  int64              `json:"source_recv_ms,omitempty"`
	Role      string             `json:"role,omitempty"`
	Liquidity string             `json:"liquidity,omitempty"`
	IssueSeq  int64              `json:"issue_seq,omitempty"`
	ClientID  string             `json:"client_order_id,omitempty"`
	Detail    string             `json:"detail,omitempty"`
}

type quotePaperVenue struct {
	mu        sync.Mutex
	runID     string
	idSeed    string
	partition RiskPartition
	coin      string
	db        *StateDB
	clock     *simulatedClock
	rule      quotePriceRule
	costs     quotePaperCosts
	events    []quoteMarketEvent
	next      int
	processed int64
	cur       *quoteReplayBook
	orders    map[string]*quotePaperOrder
	byOrder   []*quotePaperOrder
	fills     []quoteRawFill
	position  *big.Rat
	seq       int64
	idSeq     int64
	oidSeq    int64
	lastEff   int64
	limitMs   int64
	tradeUse  map[uint64]*big.Rat
	coverage  [][2]int64
	brokenAt  int64
	fatal     string
	replay    []quoteReplayVenueEventRow
	replayPos int
	fault     *quoteReplayFaultState
	persistFn func(seq int64, kind, cid string, effectMs int64, payload any) error
}

func newQuotePaperVenue(runID, idSeed string, part RiskPartition, coin string, db *StateDB, clock *simulatedClock, rule quotePriceRule, costs quotePaperCosts, events []quoteMarketEvent, coverage [][2]int64) *quotePaperVenue {
	v := &quotePaperVenue{runID: runID, idSeed: idSeed, partition: part, coin: coin, db: db, clock: clock, rule: rule, costs: costs, events: events,
		orders: map[string]*quotePaperOrder{}, position: quoteRatZero(), tradeUse: map[uint64]*big.Rat{}, coverage: coverage}
	v.persistFn = func(seq int64, kind, cid string, effectMs int64, payload any) error {
		return v.db.insertQuoteReplayVenueEvent(v.runID, seq, kind, cid, effectMs, payload)
	}
	return v
}

func (v *quotePaperVenue) Name() string                      { return "quote-replay-paper" }
func (v *quotePaperVenue) LiveShaped() bool                  { return false }
func (v *quotePaperVenue) Provider() string                  { return "paper" }
func (v *quotePaperVenue) AccountKey(p RiskPartition) string { return "paper:" + p.String() }
func (v *quotePaperVenue) Namespace() string                 { return quotePaperNamespace }

func (v *quotePaperVenue) nowMs() int64 { return v.clock.Now().UnixMilli() }

func (v *quotePaperVenue) emit(kind, cid string, effectMs int64, payload quotePaperEventPayload) error {
	v.seq++
	blob, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	if v.replayPos < len(v.replay) {
		want := v.replay[v.replayPos]
		v.replayPos++
		if want.Seq != v.seq || want.Kind != kind || want.ClientID != cid || want.EffectMs != effectMs || string(want.Event) != string(blob) {
			return refuseReplay("resume_refused", fmt.Sprintf("the rebuilt venue event %d (%s %s at %d) differs from the saved event %d (%s %s at %d)",
				v.seq, kind, cid, effectMs, want.Seq, want.Kind, want.ClientID, want.EffectMs))
		}
		return nil
	}
	if v.fault != nil {
		v.fault.hit("venue-before-persist")
	}
	if err := v.persistFn(v.seq, kind, cid, effectMs, json.RawMessage(blob)); err != nil {
		v.seq--
		return err
	}
	if v.fault != nil {
		v.fault.hit("venue-after-persist")
	}
	return nil
}

func (v *quotePaperVenue) failf(format string, args ...any) {
	if v.fatal == "" {
		v.fatal = fmt.Sprintf(format, args...)
	}
}

func (v *quotePaperVenue) NewClientOrderID() (string, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.idSeq++
	sum := sha256Hex([]byte(fmt.Sprintf("%s|%d", v.idSeed, v.idSeq)))
	b, _ := hex.DecodeString(sum[:32])
	cid := "0x" + hex.EncodeToString(b)
	if err := v.emit("issue", cid, v.nowMs(), quotePaperEventPayload{IssueSeq: v.idSeq, ClientID: cid}); err != nil {
		v.idSeq--
		return "", err
	}
	if v.fault != nil {
		v.fault.hit("issue-before-reserve")
	}
	return cid, nil
}

func (v *quotePaperVenue) covered(fromMs, toMs int64) bool {
	for _, c := range v.coverage {
		if c[0] <= fromMs && toMs <= c[1] {
			return true
		}
	}
	return false
}

func (v *quotePaperVenue) firstUncoveredAfter(p int64) int64 {
	for _, c := range v.coverage {
		if c[0] <= p && p <= c[1] {
			return c[1] + 1
		}
	}
	return p + 1
}

func (v *quotePaperVenue) anyRests() bool {
	for _, o := range v.byOrder {
		if o.rests() || o.Status == quoteOutcomeTriggered {
			return true
		}
	}
	return false
}

func (v *quotePaperVenue) advanceTo(t int64) error {
	for v.next < len(v.events) && v.events[v.next].RecvMs <= t {
		ev := v.events[v.next]
		if v.brokenAt == 0 && v.anyRests() && !v.covered(v.processed, ev.RecvMs) {
			v.brokenAt = v.firstUncoveredAfter(v.processed)
		}
		v.next++
		v.processed = ev.RecvMs
		if ev.Book != nil {
			v.onBook(ev.Book)
			continue
		}
		if err := v.onTrade(ev.Trade); err != nil {
			return err
		}
	}
	if t > v.processed {
		if v.brokenAt == 0 && v.anyRests() && !v.covered(v.processed, t) {
			v.brokenAt = v.firstUncoveredAfter(v.processed)
		}
		v.processed = t
	}
	return nil
}

func (v *quotePaperVenue) onBook(b *quoteReplayBook) {
	v.cur = b
	for _, o := range v.byOrder {
		if !o.rests() || (o.Req.Role != quoteRoleBid && o.Req.Role != quoteRoleAsk) || o.qa == nil {
			continue
		}
		now := b.sizeAt(o.Req.Side, o.px)
		if now.Cmp(o.lastSeen) > 0 {
			o.qa = quoteRatAdd(o.qa, quoteRatSub(now, o.lastSeen))
		}
		o.lastSeen = now
	}
}

func (v *quotePaperVenue) sortedResting(side string) []*quotePaperOrder {
	var out []*quotePaperOrder
	for _, o := range v.byOrder {
		if o.rests() && o.Req.Side == side && (o.Req.Role == quoteRoleBid || o.Req.Role == quoteRoleAsk) {
			out = append(out, o)
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		c := out[i].px.Cmp(out[j].px)
		if c != 0 {
			if side == "buy" {
				return c > 0
			}
			return c < 0
		}
		if out[i].AcceptedAtMs != out[j].AcceptedAtMs {
			return out[i].AcceptedAtMs < out[j].AcceptedAtMs
		}
		return out[i].order < out[j].order
	})
	return out
}

func (v *quotePaperVenue) reduceCap(side string) *big.Rat {
	if side == "buy" && v.position.Sign() < 0 {
		return quoteRatNeg(v.position)
	}
	if side == "sell" && v.position.Sign() > 0 {
		return new(big.Rat).Set(v.position)
	}
	return quoteRatZero()
}

func (v *quotePaperVenue) cancelReduceOnlyAtFlat(atMs int64) error {
	for _, o := range v.byOrder {
		if !(o.rests() || o.Status == quoteOutcomeTriggered) || !o.Req.ReduceOnly {
			continue
		}
		if v.reduceCap(o.Req.Side).Sign() > 0 {
			continue
		}
		o.Status = quoteOutcomeCancelled
		o.TerminalMs = atMs
		o.Detail = "reduce-only order cancelled: no position left in its reducing direction"
		out := v.outcomeOf(o, atMs)
		if err := v.emit("venue_cancel", o.Req.ClientOrderID, atMs, quotePaperEventPayload{Outcome: &out, Detail: o.Detail}); err != nil {
			return err
		}
	}
	return nil
}

func quoteFeeFor(size, px, rate *big.Rat) *big.Rat {
	return quoteRatCeilTo(quoteRatMul(quoteRatMul(size, px), rate), quotePaperFeeDecimals)
}

func (v *quotePaperVenue) bookFill(o *quotePaperOrder, size, px *big.Rat, atMs int64, tradeID string, liquidity string, sourceTid uint64, sourceMs int64) error {
	rate := v.costs.MakerRate
	if liquidity == "taker" {
		rate = v.costs.TakerRate
	}
	fee := quoteFeeFor(size, px, rate)
	start := quoteDecimalText(v.position)
	if o.Req.Side == "buy" {
		v.position = quoteRatAdd(v.position, size)
	} else {
		v.position = quoteRatSub(v.position, size)
	}
	o.filled = quoteRatAdd(o.filled, size)
	f := quoteRawFill{Account: v.AccountKey(v.partition), Namespace: quotePaperNamespace, Coin: v.coin, Side: o.Req.Side, Px: quoteDecimalText(px),
		Size: quoteDecimalText(size), Fee: quoteDecimalText(fee), FeeAsset: quoteFeeAssetUSDC, StartPosition: start, TimeMs: atMs, TradeID: tradeID,
		OrderID: o.OID, ClientOrderID: strings.ToLower(o.Req.ClientOrderID), Kind: ""}
	raw, _ := json.Marshal(map[string]any{"oid": o.OID, "tid": tradeID, "px": f.Px, "sz": f.Size, "fee": f.Fee, "time": atMs, "liquidity": liquidity, "source_tid": sourceTid})
	f.Raw = string(raw)
	v.fills = append(v.fills, f)
	if err := v.emit("fill", o.Req.ClientOrderID, atMs, quotePaperEventPayload{Fill: &f, SourceTid: sourceTid, SourceMs: sourceMs, Role: o.Req.Role, Liquidity: liquidity}); err != nil {
		return err
	}
	if o.filled.Cmp(o.size) >= 0 {
		o.Status = quoteOutcomeFilled
		o.TerminalMs = atMs
	}
	return v.cancelReduceOnlyAtFlat(atMs)
}

func (v *quotePaperVenue) onTrade(t *quoteReplayTrade) error {
	for _, o := range append([]*quotePaperOrder(nil), v.byOrder...) {
		if !o.rests() || o.Req.Role != quoteRoleStop || o.AcceptedAtMs >= t.RecvMs {
			continue
		}
		trigger := mustQuoteRat(o.Req.TriggerPx)
		hit := (o.Req.Side == "sell" && t.Px.Cmp(trigger) <= 0) || (o.Req.Side == "buy" && t.Px.Cmp(trigger) >= 0)
		if !hit {
			continue
		}
		o.Status = quoteOutcomeTriggered
		trig := v.outcomeOf(o, t.RecvMs)
		if err := v.emit("trigger", o.Req.ClientOrderID, t.RecvMs, quotePaperEventPayload{Outcome: &trig, SourceTid: t.Tid, SourceMs: t.RecvMs}); err != nil {
			return err
		}
		if err := v.takerExecute(o, nil, t.RecvMs, fmt.Sprintf("stop:%s", o.OID)); err != nil {
			return err
		}
		if o.Status == quoteOutcomeTriggered {
			o.Status = quoteOutcomeCancelled
			o.TerminalMs = t.RecvMs
			o.Detail = "the triggered stop found no position or depth to reduce"
			out := v.outcomeOf(o, t.RecvMs)
			if err := v.emit("venue_cancel", o.Req.ClientOrderID, t.RecvMs, quotePaperEventPayload{Outcome: &out, Detail: o.Detail}); err != nil {
				return err
			}
		}
	}
	passive := "buy"
	if t.Side == quotePaperAggressorBuy {
		passive = "sell"
	} else if t.Side != quotePaperAggressorSell {
		return nil
	}
	atPrice := new(big.Rat).Set(t.Sz)
	budget, err := v.rule.floorQuoteSize(quoteRatMul(t.Sz, v.costs.Participation))
	if err != nil {
		return err
	}
	if used := v.tradeUse[t.Tid]; used != nil {
		budget = quoteRatSub(budget, used)
	}
	for _, o := range v.sortedResting(passive) {
		if o.AcceptedAtMs >= t.RecvMs {
			continue
		}
		cmp := t.Px.Cmp(o.px)
		through := (passive == "buy" && cmp < 0) || (passive == "sell" && cmp > 0)
		if cmp == 0 && o.qa != nil {
			consume := quoteRatMin(o.qa, atPrice)
			o.qa = quoteRatSub(o.qa, consume)
			atPrice = quoteRatSub(atPrice, consume)
			continue
		}
		if !through || budget.Sign() <= 0 {
			continue
		}
		size := quoteRatMin(o.remaining(), budget)
		if o.Req.ReduceOnly {
			size = quoteRatMin(size, v.reduceCap(o.Req.Side))
		}
		size, err = v.rule.floorQuoteSize(size)
		if err != nil {
			return err
		}
		if size.Sign() <= 0 {
			continue
		}
		budget = quoteRatSub(budget, size)
		used := v.tradeUse[t.Tid]
		if used == nil {
			used = quoteRatZero()
		}
		v.tradeUse[t.Tid] = quoteRatAdd(used, size)
		tid := fmt.Sprintf("t%d:%s", t.Tid, o.OID)
		if err := v.bookFill(o, size, o.px, t.RecvMs, tid, "maker", t.Tid, t.RecvMs); err != nil {
			return err
		}
	}
	return nil
}

func (v *quotePaperVenue) slipped(side string, px *big.Rat, limit *big.Rat) *big.Rat {
	var out *big.Rat
	if side == "sell" {
		out = quoteRatFloorTo(quoteRatMul(px, quoteRatSub(big.NewRat(1, 1), v.costs.SlipRate)), quotePaperSlipDecimals)
		if limit != nil && out.Cmp(limit) < 0 {
			out = new(big.Rat).Set(limit)
		}
	} else {
		out = quoteRatCeilTo(quoteRatMul(px, quoteRatAdd(big.NewRat(1, 1), v.costs.SlipRate)), quotePaperSlipDecimals)
		if limit != nil && out.Cmp(limit) > 0 {
			out = new(big.Rat).Set(limit)
		}
	}
	return out
}

func (v *quotePaperVenue) takerExecute(o *quotePaperOrder, limit *big.Rat, atMs int64, tidPrefix string) error {
	if v.cur == nil {
		return nil
	}
	want := quoteRatMin(o.remaining(), v.reduceCap(o.Req.Side))
	levels := v.cur.Bids
	if o.Req.Side == "buy" {
		levels = v.cur.Asks
	}
	leg := 0
	for i, l := range levels {
		if want.Sign() <= 0 {
			break
		}
		if limit != nil && ((o.Req.Side == "sell" && l.Px.Cmp(limit) < 0) || (o.Req.Side == "buy" && l.Px.Cmp(limit) > 0)) {
			break
		}
		take := quoteRatMin(want, l.Sz)
		if i == len(levels)-1 && limit == nil {
			take = want
		}
		take, err := v.rule.floorQuoteSize(take)
		if err != nil {
			return err
		}
		if take.Sign() <= 0 {
			continue
		}
		leg++
		px := v.slipped(o.Req.Side, l.Px, limit)
		if err := v.bookFill(o, take, px, atMs, fmt.Sprintf("%s:%d", tidPrefix, leg), "taker", 0, 0); err != nil {
			return err
		}
		want = quoteRatSub(want, take)
		if o.Status != quoteOutcomeResting && o.Status != quoteOutcomeTriggered {
			break
		}
	}
	return nil
}

func (v *quotePaperVenue) outcomeOf(o *quotePaperOrder, atMs int64) quoteOrderOutcome {
	if o == nil {
		return quoteOrderOutcome{Status: quoteOutcomeAbsent, VenueTimeMs: atMs}
	}
	return quoteOrderOutcome{Status: o.Status, VenueOrderID: o.OID, VenueTimeMs: atMs, AcceptedAtMs: o.AcceptedAtMs, FilledQty: quoteDecimalText(o.filled), Detail: o.Detail}
}

func (v *quotePaperVenue) effectTime(latency int64) (int64, bool) {
	now := v.nowMs()
	base := now
	if v.lastEff > base {
		base = v.lastEff
	}
	if latency < 1 {
		latency = 1
	}
	e := base + latency
	if v.limitMs > 0 && e >= v.limitMs {
		v.failf("a venue effect at %d reaches the next deadline %d; the frozen latency does not fit the cycle", e, v.limitMs)
		return e, false
	}
	return e, true
}

func (v *quotePaperVenue) setClock(e int64) {
	if e > v.lastEff {
		v.lastEff = e
	}
	_ = v.clock.Set(timeFromMs(e))
}

func (v *quotePaperVenue) Place(ctx context.Context, req quoteOrderRequest) (quoteOrderOutcome, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	e, ok := v.effectTime(v.costs.PlaceLatencyMs)
	if !ok {
		return quoteOrderOutcome{Status: quoteOutcomeUnknown, Detail: v.fatal}, nil
	}
	return v.execPlace(req, e)
}

func (v *quotePaperVenue) execPlace(req quoteOrderRequest, e int64) (quoteOrderOutcome, error) {
	if err := v.advanceTo(e); err != nil {
		return quoteOrderOutcome{}, err
	}
	v.setClock(e)
	key := strings.ToLower(req.ClientOrderID)
	if existing := v.orders[key]; existing != nil {
		return v.outcomeOf(existing, e), nil
	}
	v.oidSeq++
	o := &quotePaperOrder{Req: req, OID: strconv.FormatInt(v.oidSeq, 10), filled: quoteRatZero(), order: int(v.oidSeq)}
	reject := func(detail string) (quoteOrderOutcome, error) {
		o.Status = quoteOutcomeRejected
		o.TerminalMs = e
		o.Detail = detail
		v.orders[key] = o
		v.byOrder = append(v.byOrder, o)
		out := v.outcomeOf(o, e)
		r := req
		if err := v.emit("place", req.ClientOrderID, e, quotePaperEventPayload{Request: &r, Outcome: &out}); err != nil {
			return quoteOrderOutcome{}, err
		}
		return out, nil
	}
	sz, err := parseQuoteDecimal(req.Size)
	if err != nil || sz.Sign() <= 0 {
		return reject(fmt.Sprintf("size %q is not positive", req.Size))
	}
	o.size = sz
	if e >= req.ExpiryMs {
		return reject(fmt.Sprintf("the request expired at %d before its effect at %d", req.ExpiryMs, e))
	}
	if err := v.rule.check(req); err != nil {
		return reject(err.Error())
	}
	switch req.Role {
	case quoteRoleStop:
		if !req.ReduceOnly {
			return reject("a stop must be reduce-only")
		}
	case quoteRoleClose:
		if !req.ReduceOnly || req.TIF != "Ioc" {
			return reject("an inventory close must be a reduce-only immediate-or-cancel order")
		}
	case quoteRoleBid, quoteRoleAsk:
		if !req.PostOnly || req.TIF != "Alo" {
			return reject("a quote must be post-only (Alo)")
		}
	default:
		return reject(fmt.Sprintf("unknown role %q", req.Role))
	}
	if req.Role != quoteRoleStop {
		px, err := parseQuoteDecimal(req.Px)
		if err != nil || px.Sign() <= 0 {
			return reject(fmt.Sprintf("price %q is not positive", req.Px))
		}
		o.px = px
	}
	if (req.Role == quoteRoleBid || req.Role == quoteRoleAsk) && !req.ReduceOnly && v.costs.MinOrderValue != nil {
		if quoteRatMul(o.size, o.px).Cmp(v.costs.MinOrderValue) < 0 {
			return reject(fmt.Sprintf("order value %s is below the %s minimum", quoteDecimalText(quoteRatMul(o.size, o.px)), quoteDecimalText(v.costs.MinOrderValue)))
		}
	}
	if req.ReduceOnly && v.reduceCap(req.Side).Sign() <= 0 {
		return reject("a reduce-only order has no position to reduce")
	}
	if req.Role == quoteRoleBid || req.Role == quoteRoleAsk {
		if v.cur == nil {
			return reject("no recorded book at the effect time; post-only acceptance cannot be proved")
		}
		bestBid, bestAsk := v.cur.bestBid(), v.cur.bestAsk()
		if bestBid == nil || bestAsk == nil {
			return reject("the recorded book has an empty side at the effect time")
		}
		if req.Side == "buy" {
			if o.px.Cmp(bestAsk) >= 0 {
				return reject("post-only bid would cross the recorded ask")
			}
			deepest := v.cur.Bids[len(v.cur.Bids)-1].Px
			if o.px.Cmp(deepest) < 0 {
				return reject("the bid is outside the recorded depth; its queue rank is unknown")
			}
			if o.px.Cmp(bestBid) > 0 {
				o.qa = quoteRatZero()
			} else {
				o.qa = v.cur.sizeAt("buy", o.px)
			}
		} else {
			if o.px.Cmp(bestBid) <= 0 {
				return reject("post-only ask would cross the recorded bid")
			}
			deepest := v.cur.Asks[len(v.cur.Asks)-1].Px
			if o.px.Cmp(deepest) > 0 {
				return reject("the ask is outside the recorded depth; its queue rank is unknown")
			}
			if o.px.Cmp(bestAsk) < 0 {
				o.qa = quoteRatZero()
			} else {
				o.qa = v.cur.sizeAt("sell", o.px)
			}
		}
		o.lastSeen = v.cur.sizeAt(req.Side, o.px)
	}
	o.Status = quoteOutcomeResting
	o.AcceptedAtMs = e
	v.orders[key] = o
	v.byOrder = append(v.byOrder, o)
	accepted := v.outcomeOf(o, e)
	r := req
	if err := v.emit("place", req.ClientOrderID, e, quotePaperEventPayload{Request: &r, Outcome: &accepted}); err != nil {
		return quoteOrderOutcome{}, err
	}
	if req.Role == quoteRoleClose {
		if err := v.takerExecute(o, o.px, e, fmt.Sprintf("close:%s", o.OID)); err != nil {
			return quoteOrderOutcome{}, err
		}
		if o.rests() {
			o.Status = quoteOutcomeCancelled
			o.TerminalMs = e
			o.Detail = "immediate-or-cancel remainder cancelled"
			out := v.outcomeOf(o, e)
			if err := v.emit("venue_cancel", req.ClientOrderID, e, quotePaperEventPayload{Outcome: &out, Detail: o.Detail}); err != nil {
				return quoteOrderOutcome{}, err
			}
		}
	}
	return v.outcomeOf(o, e), nil
}

func (v *quotePaperVenue) Cancel(ctx context.Context, req quoteOrderRequest, venueOrderID string) (quoteOrderOutcome, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	o := v.orders[strings.ToLower(req.ClientOrderID)]
	if o != nil && o.CancelEffectMs > 0 {
		return v.outcomeOf(o, o.CancelEffectMs), nil
	}
	e, ok := v.effectTime(v.costs.CancelLatencyMs)
	if !ok {
		return quoteOrderOutcome{Status: quoteOutcomeUnknown, Detail: v.fatal}, nil
	}
	return v.execCancel(req, e)
}

func (v *quotePaperVenue) execCancel(req quoteOrderRequest, e int64) (quoteOrderOutcome, error) {
	if err := v.advanceTo(e); err != nil {
		return quoteOrderOutcome{}, err
	}
	v.setClock(e)
	o := v.orders[strings.ToLower(req.ClientOrderID)]
	if o != nil && (o.rests() || o.Status == quoteOutcomeTriggered) {
		o.Status = quoteOutcomeCancelled
		o.TerminalMs = e
	}
	if o != nil {
		o.CancelEffectMs = e
	}
	out := v.outcomeOf(o, e)
	r := req
	if err := v.emit("cancel", req.ClientOrderID, e, quotePaperEventPayload{Request: &r, Outcome: &out}); err != nil {
		return quoteOrderOutcome{}, err
	}
	return out, nil
}

func (v *quotePaperVenue) Status(ctx context.Context, req quoteOrderRequest, venueOrderID string) (quoteOrderOutcome, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	now := v.nowMs()
	if err := v.advanceTo(now); err != nil {
		return quoteOrderOutcome{}, err
	}
	return v.outcomeOf(v.orders[strings.ToLower(req.ClientOrderID)], now), nil
}

func (v *quotePaperVenue) RecoverFills(ctx context.Context, coin string, sinceMs, untilMs int64) (quoteFillRecovery, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	now := v.nowMs()
	if err := v.advanceTo(now); err != nil {
		return quoteFillRecovery{}, err
	}
	through := untilMs
	if through <= 0 || through > now {
		through = now
	}
	rec := quoteFillRecovery{Coverage: quoteFillCoverage{Complete: true, FromMs: sinceMs, ThroughMs: through, Pages: 1}}
	if v.brokenAt > 0 && v.brokenAt <= through {
		rec.Coverage.Complete = false
		rec.Coverage.Reason = fmt.Sprintf("the recorded raw-trade coverage breaks at %d while an own order rests; later fills are unknowable", v.brokenAt)
		return rec, nil
	}
	for _, f := range v.fills {
		if strings.EqualFold(f.Coin, coin) && f.TimeMs >= sinceMs && f.TimeMs <= through {
			rec.Fills = append(rec.Fills, f)
		}
	}
	return rec, nil
}

func (v *quotePaperVenue) Position(ctx context.Context, coin string) (quotePositionRead, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	now := v.nowMs()
	if err := v.advanceTo(now); err != nil {
		return quotePositionRead{}, err
	}
	size := quoteRatZero()
	for _, f := range v.fills {
		if f.TimeMs <= now && strings.EqualFold(f.Coin, coin) {
			size = quoteRatAdd(size, signedQuoteFillQty(f))
		}
	}
	return quotePositionRead{Account: v.AccountKey(v.partition), Coin: coin, Size: quoteDecimalText(size), ReadAtMs: now}, nil
}

type quotePaperCheckpoint struct {
	ProcessedMs int64             `json:"processed_ms"`
	Seq         int64             `json:"venue_seq"`
	IssueSeq    int64             `json:"issue_seq"`
	Position    string            `json:"position"`
	BrokenAtMs  int64             `json:"broken_at_ms,omitempty"`
	Orders      []quotePaperOrder `json:"orders"`
}

func (v *quotePaperVenue) checkpoint() quotePaperCheckpoint {
	cp := quotePaperCheckpoint{ProcessedMs: v.processed, Seq: v.seq, IssueSeq: v.idSeq, Position: quoteDecimalText(v.position), BrokenAtMs: v.brokenAt}
	for _, o := range v.byOrder {
		if !o.rests() && o.Status != quoteOutcomeTriggered {
			continue
		}
		c := *o
		c.Filled = quoteDecimalText(o.filled)
		if o.qa != nil {
			c.QueueAhead = quoteDecimalText(o.qa)
			c.LastSeen = quoteDecimalText(o.lastSeen)
		}
		cp.Orders = append(cp.Orders, c)
	}
	sort.Slice(cp.Orders, func(i, j int) bool { return cp.Orders[i].OID < cp.Orders[j].OID })
	return cp
}

func (v *quotePaperVenue) rebuild(saved []quoteReplayVenueEventRow, checkpointSeq int64, checkpointMs int64, checkpointDigest string) error {
	v.replay = saved
	v.replayPos = 0
	checked := checkpointDigest == ""
	verify := func() error {
		if checked || v.seq != checkpointSeq {
			return nil
		}
		if err := v.advanceTo(checkpointMs); err != nil {
			return err
		}
		blob, _ := json.Marshal(v.checkpoint())
		if sha256Hex(blob) != checkpointDigest {
			return refuseReplay("resume_refused", fmt.Sprintf("the rebuilt venue state at the cursor deadline %d differs from the saved checkpoint", checkpointMs))
		}
		checked = true
		return nil
	}
	if err := verify(); err != nil {
		return err
	}
	for v.replayPos < len(v.replay) {
		e := v.replay[v.replayPos]
		var p quotePaperEventPayload
		if err := json.Unmarshal(e.Event, &p); err != nil {
			return refuseReplay("state_foreign", fmt.Sprintf("venue event %d is unreadable: %v", e.Seq, err))
		}
		before := v.replayPos
		switch e.Kind {
		case "issue":
			v.idSeq++
			sum := sha256Hex([]byte(fmt.Sprintf("%s|%d", v.idSeed, v.idSeq)))
			b, _ := hex.DecodeString(sum[:32])
			cid := "0x" + hex.EncodeToString(b)
			if err := v.emit("issue", cid, e.EffectMs, quotePaperEventPayload{IssueSeq: v.idSeq, ClientID: cid}); err != nil {
				return err
			}
		case "place":
			if p.Request == nil {
				return refuseReplay("state_foreign", fmt.Sprintf("venue event %d names no request", e.Seq))
			}
			if _, err := v.execPlace(*p.Request, e.EffectMs); err != nil {
				return err
			}
		case "cancel":
			if p.Request == nil {
				return refuseReplay("state_foreign", fmt.Sprintf("venue event %d names no request", e.Seq))
			}
			if _, err := v.execCancel(*p.Request, e.EffectMs); err != nil {
				return err
			}
		default:
			if err := v.advanceTo(e.EffectMs); err != nil {
				return err
			}
		}
		if v.replayPos == before {
			return refuseReplay("resume_refused", fmt.Sprintf("the rebuilt venue does not reproduce saved event %d (%s at %d)", e.Seq, e.Kind, e.EffectMs))
		}
		if e.EffectMs > v.lastEff {
			v.lastEff = e.EffectMs
		}
		if err := verify(); err != nil {
			return err
		}
	}
	if !checked {
		return refuseReplay("resume_refused", fmt.Sprintf("the saved venue events end before the checkpoint sequence %d", checkpointSeq))
	}
	v.replay = nil
	v.replayPos = 0
	return nil
}

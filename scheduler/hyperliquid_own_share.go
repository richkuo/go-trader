package main

import (
	"fmt"
	"math"
	"strings"
	"sync"
)

// hlShareBook is one strategy's book on a coin and whether its stop rests.
// A hedge leg is never armed: its stop fields do not protect the primary book.
type hlShareBook struct {
	Qty   float64
	Armed bool
}

type hlShareInput struct {
	Side   string
	Self   hlShareBook
	Same   []hlShareBook
	Opp    float64
	Signed float64
	Known  bool
}

type hlShareResult struct {
	Qty      float64
	Unbacked float64
	Known    bool
	Drift    bool
}

// hlOwnStopShare is the only size of a live Hyperliquid stop or take-profit.
// Q is at most the strategy's own book and at most the own-side chain units.
// An unknown view returns the book and Known false, so the caller neither
// shrinks nor cancels.
func hlOwnStopShare(in hlShareInput) hlShareResult {
	tol := hlSharedCloseQtyTolerance
	b := in.Self.Qty
	if !(b > 0) || math.IsNaN(b) || math.IsInf(b, 0) {
		return hlShareResult{}
	}
	if !in.Known || math.IsNaN(in.Signed) || math.IsInf(in.Signed, 0) {
		return hlShareResult{Qty: b, Known: false}
	}
	s := hlSideSign(in.Side)
	own := math.Max(s*in.Signed, 0)
	opp := in.Opp
	if !(opp > 0) || math.IsNaN(opp) || math.IsInf(opp, 0) {
		opp = 0
	}
	avail := own + opp
	var sameSum float64
	same := make([]hlShareBook, 0, len(in.Same))
	for _, p := range in.Same {
		if !(p.Qty > tol) || math.IsNaN(p.Qty) || math.IsInf(p.Qty, 0) {
			continue
		}
		same = append(same, p)
		sameSum += p.Qty
	}
	var q float64
	drift := b+sameSum > avail+tol
	if !drift {
		q = math.Min(b, own)
	} else {
		q = hlShareDriftQty(in.Self, same, own, avail)
	}
	cap := math.Min(b, own)
	if q > cap {
		q = cap
	}
	if q <= tol {
		q = 0
	}
	return hlShareResult{Qty: q, Unbacked: math.Max(b-q, 0), Known: true, Drift: drift}
}

func hlShareDriftQty(self hlShareBook, same []hlShareBook, own, avail float64) float64 {
	tol := hlSharedCloseQtyTolerance
	books := make([]hlShareBook, 0, 1+len(same))
	books = append(books, self)
	books = append(books, same...)
	var armedSum, unarmedSum float64
	for _, bk := range books {
		if bk.Armed {
			armedSum += bk.Qty
		} else {
			unarmedSum += bk.Qty
		}
	}
	pass1 := make([]float64, len(books))
	var pass1Sum float64
	if armedSum > tol {
		scale := math.Min(1, avail/armedSum)
		for i, bk := range books {
			if !bk.Armed {
				continue
			}
			pass1[i] = math.Min(bk.Qty, math.Min(own, bk.Qty*scale))
			pass1Sum += pass1[i]
		}
	}
	if self.Armed {
		return pass1[0]
	}
	left := math.Max(avail-pass1Sum, 0)
	if unarmedSum <= tol {
		return 0
	}
	scale := math.Min(1, left/unarmedSum)
	return math.Min(self.Qty, math.Min(own, self.Qty*scale))
}

// hlOwnStopShareOnView resolves the signed chain from a coin view. A missing
// side on a non-zero size is an unknown view. A missing or zero size on a
// known view is a flat chain.
func hlOwnStopShareOnView(symbol, side string, self hlShareBook, same []hlShareBook, opp float64, view hlOnChainCoinView) hlShareResult {
	if !view.Known {
		return hlOwnStopShare(hlShareInput{Side: side, Self: self, Same: same, Opp: opp})
	}
	signed, ok := hlOnChainSignedQty(view, symbol)
	if !ok {
		return hlOwnStopShare(hlShareInput{Side: side, Self: self, Same: same, Opp: opp})
	}
	return hlOwnStopShare(hlShareInput{Side: side, Self: self, Same: same, Opp: opp, Signed: signed, Known: true})
}

func hlBookArmed(pos *Position) bool {
	if pos == nil || pos.isHedgeLeg() {
		return false
	}
	return pos.StopLossOID > 0 || pos.StopLossTriggerPx > 0
}

// hlPeerBookListOnCoin lists each same-side peer book. The opposite side is a
// sum. The caller holds mu.RLock over the same read as its own position.
func hlPeerBookListOnCoin(strategies map[string]*StrategyState, hlLiveAll []StrategyConfig, coin, selfID, selfSide string) (same []hlShareBook, oppQty float64) {
	target := strings.ToUpper(strings.TrimSpace(coin))
	if target == "" {
		return nil, 0
	}
	selfSign := hlSideSign(selfSide)
	add := func(pos *Position) {
		if pos == nil || pos.Quantity <= 0 {
			return
		}
		if hlSideSign(pos.Side) == selfSign {
			same = append(same, hlShareBook{Qty: pos.Quantity, Armed: hlBookArmed(pos)})
			return
		}
		oppQty += pos.Quantity
	}
	for _, sc := range hlLiveAll {
		if sc.ID == selfID {
			continue
		}
		ss := strategies[sc.ID]
		if ss == nil {
			continue
		}
		if raw := hyperliquidRawCoin(sc); raw != "" && strings.ToUpper(strings.TrimSpace(raw)) == target {
			add(hlVirtualPositionFor(ss, sc, raw))
		}
		if hCoin := hedgeCoin(sc); hCoin != "" && strings.ToUpper(strings.TrimSpace(hCoin)) == target {
			if hPos := ss.Positions[hCoin]; hPos.isHedgeLeg() {
				add(hPos)
			}
		}
	}
	return same, oppQty
}

func hlShareBookSum(books []hlShareBook) float64 {
	var sum float64
	for _, b := range books {
		sum += b.Qty
	}
	return sum
}

var (
	hlCoinSubmitMu sync.Mutex
	hlCoinSubmit   = map[string]uint64{}
)

func hlCoinKey(coin string) string {
	return strings.ToUpper(strings.TrimSpace(coin))
}

func hlNoteCoinSubmission(coin string) {
	key := hlCoinKey(coin)
	if key == "" {
		return
	}
	hlCoinSubmitMu.Lock()
	hlCoinSubmit[key]++
	hlCoinSubmitMu.Unlock()
}

func hlCoinSubmitCount(coin string) uint64 {
	hlCoinSubmitMu.Lock()
	defer hlCoinSubmitMu.Unlock()
	return hlCoinSubmit[hlCoinKey(coin)]
}

func hlCoinSubmitSnapshot() map[string]uint64 {
	hlCoinSubmitMu.Lock()
	defer hlCoinSubmitMu.Unlock()
	out := make(map[string]uint64, len(hlCoinSubmit))
	for k, v := range hlCoinSubmit {
		out[k] = v
	}
	return out
}

type hlStopQty struct {
	Qty    float64
	Known  bool
	Fresh  bool
	Capped bool
	Netted bool
}

// hlCycleShare is one cycle's account view. A coin whose submission counter
// moved since the last successful read is read again. A failed read is not
// fresh for that coin until its counter moves again, and a not-fresh or
// unknown view places at the book.
type hlCycleShare struct {
	view        hlOnChainCoinView
	snapshot    map[string]uint64
	refetch     func() (hlOnChainCoinView, error)
	strategies  map[string]*StrategyState
	live        []StrategyConfig
	notifier    *MultiNotifier
	mu          sync.Mutex
	refreshView hlOnChainCoinView
	refreshOK   bool
	refreshSnap map[string]uint64
	failedAt    map[string]uint64
	unknownNote map[string]bool
	reconciled  map[string]bool
}

func newHLCycleShare(view hlOnChainCoinView, snapshot map[string]uint64, refetch func() (hlOnChainCoinView, error), strategies map[string]*StrategyState, live []StrategyConfig, notifier *MultiNotifier) *hlCycleShare {
	if snapshot == nil {
		snapshot = map[string]uint64{}
	}
	return &hlCycleShare{
		view: view, snapshot: snapshot, refetch: refetch,
		strategies: strategies, live: live, notifier: notifier,
		failedAt:    map[string]uint64{},
		unknownNote: map[string]bool{},
	}
}

func (c *hlCycleShare) markReconciled(strategies []StrategyConfig) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.reconciled == nil {
		c.reconciled = map[string]bool{}
	}
	for _, sc := range strategies {
		c.reconciled[sc.ID] = true
	}
}

func (c *hlCycleShare) wasReconciled(strategyID string) bool {
	if c == nil {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.reconciled[strategyID]
}

func (c *hlCycleShare) viewFor(coin string) (hlOnChainCoinView, bool) {
	if c == nil {
		return hlOnChainCoinView{}, false
	}
	key := hlCoinKey(coin)
	c.mu.Lock()
	defer c.mu.Unlock()
	count := hlCoinSubmitCount(key)
	if count == c.snapshot[key] {
		return c.view, true
	}
	if c.refreshOK && count == c.refreshSnap[key] {
		return c.refreshView, true
	}
	if failed, ok := c.failedAt[key]; ok && failed == count {
		return hlOnChainCoinView{}, false
	}
	snap := hlCoinSubmitSnapshot()
	if c.refetch != nil {
		v, err := c.refetch()
		if err == nil && v.Known {
			c.refreshView = v
			c.refreshOK = true
			c.refreshSnap = snap
			if hlCoinSubmitCount(key) == snap[key] {
				return v, true
			}
			return hlOnChainCoinView{}, false
		}
	}
	c.failedAt[key] = snap[key]
	return hlOnChainCoinView{}, false
}

func (c *hlCycleShare) peers(coin, selfID, side string) ([]hlShareBook, float64) {
	if c == nil {
		return nil, 0
	}
	return hlPeerBookListOnCoin(c.strategies, c.live, coin, selfID, side)
}

// StopQty sizes one strategy. The caller has already snapshotted peers under
// mu.RLock. Capped means a fresh known Q is below the book.
func (c *hlCycleShare) StopQty(sc StrategyConfig, coin, side string, book float64, armed bool, peers []hlShareBook, opp float64) hlStopQty {
	if !(book > 0) || math.IsNaN(book) || math.IsInf(book, 0) {
		return hlStopQty{}
	}
	if c == nil {
		return hlStopQty{Qty: book, Fresh: true}
	}
	view, fresh := c.viewFor(coin)
	if !fresh {
		return hlStopQty{Qty: book}
	}
	res := hlOwnStopShareOnView(coin, side, hlShareBook{Qty: book, Armed: armed}, peers, opp, view)
	if !res.Known {
		c.noteUnknown(sc, coin, len(peers) > 0 || opp > hlSharedCloseQtyTolerance)
		return hlStopQty{Qty: book, Fresh: true}
	}
	c.clearUnknown(sc, coin)
	c.noteUnbacked(sc, coin, book, res, peers, opp)
	return hlStopQty{
		Qty:    res.Qty,
		Known:  true,
		Fresh:  true,
		Capped: res.Qty < book-hlSharedCloseQtyTolerance,
		Netted: !res.Drift && opp > hlSharedCloseQtyTolerance,
	}
}

// hlReplaceQty sizes a replacement. A not-fresh or unknown view stays at the
// book. A fresh known Q of zero skips the placement (place is false).
func hlReplaceQty(q hlStopQty, book float64) (qty float64, capped, place bool) {
	tol := hlSharedCloseQtyTolerance
	if !q.Fresh || !q.Known {
		return book, false, book > tol
	}
	if q.Qty <= tol {
		return 0, true, false
	}
	return q.Qty, q.Capped, true
}

// hlFreshArmQty sizes an arm that reads the coin again after its own fill.
// A failed read defers. A fresh known Q of zero places nothing.
func hlFreshArmQty(q hlStopQty, book float64) (qty float64, deferArm, place bool) {
	tol := hlSharedCloseQtyTolerance
	if !q.Fresh {
		return 0, true, false
	}
	if !q.Known {
		return book, false, book > tol
	}
	if q.Qty <= tol {
		return 0, false, false
	}
	return q.Qty, false, true
}

var (
	hlShareLatchMu sync.Mutex
	hlShareLatch   = map[string]float64{}

	hlShareUnknownMu      sync.Mutex
	hlShareUnknownBlocks  = map[string]int{}
	hlShareUnknownAlerted = map[string]bool{}

	hlOpenOrderListMu      sync.Mutex
	hlOpenOrderListFails   int
	hlOpenOrderListAlerted bool
)

func hlShareLatchKey(strategyID, coin string) string {
	return strategyID + "|" + hlCoinKey(coin)
}

func (c *hlCycleShare) noteUnbacked(sc StrategyConfig, coin string, book float64, res hlShareResult, peers []hlShareBook, opp float64) {
	tol := hlSharedCloseQtyTolerance
	key := hlShareLatchKey(sc.ID, coin)
	reconciled := c.wasReconciled(sc.ID)
	hlShareLatchMu.Lock()
	defer hlShareLatchMu.Unlock()
	if res.Qty >= book-tol {
		delete(hlShareLatch, key)
		return
	}
	if prev, ok := hlShareLatch[key]; ok && math.Abs(prev-res.Qty) <= tol {
		return
	}
	if len(peers) == 0 && !(opp > tol) && !reconciled {
		fmt.Printf("[INFO] [%s] %s: chain share Q=%.6f is below the book %.6f with no peer on the coin; the strategy's own reconcile books the fill.\n",
			sc.ID, coin, res.Qty, book)
		return
	}
	hlShareLatch[key] = res.Qty
	if !res.Drift {
		fmt.Printf("[INFO] [%s] %s: chain share Q=%.6f is below the book %.6f because an opposite-side book of %.6f nets the coin; no book change is needed.\n",
			sc.ID, coin, res.Qty, book, opp)
		return
	}
	msg := fmt.Sprintf("CRITICAL: [%s] %s: chain share Q=%.6f is below the book %.6f (unbacked %.6f). Same-side peer books: %s. Opposite-side book netting the coin: %.6f.",
		sc.ID, coin, res.Qty, book, res.Unbacked, formatHLShareBooks(peers), opp)
	hlSendShareCritical(c.notifier, msg)
}

func formatHLShareBooks(peers []hlShareBook) string {
	if len(peers) == 0 {
		return "none"
	}
	parts := make([]string, 0, len(peers))
	for _, p := range peers {
		state := "no stop"
		if p.Armed {
			state = "stop resting"
		}
		parts = append(parts, fmt.Sprintf("%.6f (%s)", p.Qty, state))
	}
	return strings.Join(parts, ", ")
}

func hlSendShareCritical(notifier *MultiNotifier, msg string) {
	if notifier != nil && notifier.HasBackends() {
		notifier.SendToAllChannels(msg)
		notifier.SendOwnerDM(msg)
	}
}

func (c *hlCycleShare) noteUnknown(sc StrategyConfig, coin string, hasPeer bool) {
	if c == nil || !hasPeer {
		return
	}
	key := hlShareLatchKey(sc.ID, coin)
	c.mu.Lock()
	if c.unknownNote[key] {
		c.mu.Unlock()
		return
	}
	c.unknownNote[key] = true
	c.mu.Unlock()
	n, alert := recordHLShareUnknown(sc.ID, coin)
	if !alert {
		return
	}
	msg := fmt.Sprintf("CRITICAL: [%s] %s: the on-chain position has been unreadable for %d consecutive cycles while a peer book shares the coin. Placements stay at the book size; no stop was shrunk or cancelled.",
		sc.ID, coin, n)
	hlSendShareCritical(c.notifier, msg)
}

func (c *hlCycleShare) clearUnknown(sc StrategyConfig, coin string) {
	clearHLShareUnknown(sc.ID, coin)
}

func recordHLShareUnknown(strategyID, coin string) (int, bool) {
	key := hlShareLatchKey(strategyID, coin)
	hlShareUnknownMu.Lock()
	defer hlShareUnknownMu.Unlock()
	hlShareUnknownBlocks[key]++
	count := hlShareUnknownBlocks[key]
	if count < hlProtectionGuardAlertAfterBlocks || hlShareUnknownAlerted[key] {
		return count, false
	}
	hlShareUnknownAlerted[key] = true
	return count, true
}

func clearHLShareUnknown(strategyID, coin string) {
	key := hlShareLatchKey(strategyID, coin)
	hlShareUnknownMu.Lock()
	delete(hlShareUnknownBlocks, key)
	delete(hlShareUnknownAlerted, key)
	hlShareUnknownMu.Unlock()
}

func recordHLOpenOrderListFailure() (int, bool) {
	hlOpenOrderListMu.Lock()
	defer hlOpenOrderListMu.Unlock()
	hlOpenOrderListFails++
	if hlOpenOrderListFails < hlProtectionGuardAlertAfterBlocks || hlOpenOrderListAlerted {
		return hlOpenOrderListFails, false
	}
	hlOpenOrderListAlerted = true
	return hlOpenOrderListFails, true
}

func clearHLOpenOrderListFailure() {
	hlOpenOrderListMu.Lock()
	hlOpenOrderListFails = 0
	hlOpenOrderListAlerted = false
	hlOpenOrderListMu.Unlock()
}

// SweepClosed drops latch entries whose position is flat.
func (c *hlCycleShare) SweepClosed(strategies map[string]*StrategyState) {
	if c == nil {
		return
	}
	open := map[string]struct{}{}
	for id, ss := range strategies {
		if ss == nil {
			continue
		}
		for sym, pos := range ss.Positions {
			if pos != nil && pos.Quantity > 0 {
				open[hlShareLatchKey(id, sym)] = struct{}{}
			}
		}
	}
	hlShareLatchMu.Lock()
	defer hlShareLatchMu.Unlock()
	for key := range hlShareLatch {
		if _, ok := open[key]; !ok {
			delete(hlShareLatch, key)
		}
	}
}

func resetHLShareAlerts() {
	hlShareLatchMu.Lock()
	hlShareLatch = map[string]float64{}
	hlShareLatchMu.Unlock()
	hlShareUnknownMu.Lock()
	hlShareUnknownBlocks = map[string]int{}
	hlShareUnknownAlerted = map[string]bool{}
	hlShareUnknownMu.Unlock()
	clearHLOpenOrderListFailure()
}

var (
	hlActiveShareMu sync.Mutex
	hlActiveShare   *hlCycleShare

	hlShareForceMu sync.Mutex
	hlShareForceTP = map[string]bool{}
)

func setHLActiveCycleShare(s *hlCycleShare) {
	hlActiveShareMu.Lock()
	hlActiveShare = s
	hlActiveShareMu.Unlock()
}

func currentHLCycleShare() *hlCycleShare {
	hlActiveShareMu.Lock()
	defer hlActiveShareMu.Unlock()
	return hlActiveShare
}

func hlShareMarkForceTP(strategyID, symbol string) {
	hlShareForceMu.Lock()
	hlShareForceTP[hlShareLatchKey(strategyID, symbol)] = true
	hlShareForceMu.Unlock()
}

func hlShareTakeForceTP(strategyID, symbol string) bool {
	key := hlShareLatchKey(strategyID, symbol)
	hlShareForceMu.Lock()
	defer hlShareForceMu.Unlock()
	if !hlShareForceTP[key] {
		return false
	}
	delete(hlShareForceTP, key)
	return true
}

func hlSharePeekForceTP(strategyID, symbol string) bool {
	hlShareForceMu.Lock()
	defer hlShareForceMu.Unlock()
	return hlShareForceTP[hlShareLatchKey(strategyID, symbol)]
}

func hlShareClearForceTP() {
	hlShareForceMu.Lock()
	hlShareForceTP = map[string]bool{}
	hlShareForceMu.Unlock()
}

func hlQtyFromAccountMaps(symbol, side string, book float64, armed bool, peers []hlShareBook, opp float64, abs map[string]float64, net map[string]string) hlStopQty {
	if !(book > 0) {
		return hlStopQty{}
	}
	if abs == nil && net == nil {
		return hlStopQty{Qty: book, Fresh: true}
	}
	view := hlOnChainCoinView{Known: true, AbsQty: abs, NetSide: net}
	res := hlOwnStopShareOnView(symbol, side, hlShareBook{Qty: book, Armed: armed}, peers, opp, view)
	if !res.Known {
		return hlStopQty{Qty: book, Fresh: true}
	}
	return hlStopQty{
		Qty:    res.Qty,
		Known:  true,
		Fresh:  true,
		Capped: res.Qty < book-hlSharedCloseQtyTolerance,
		Netted: !res.Drift && opp > hlSharedCloseQtyTolerance,
	}
}

func hlStopUpdateMovesChain(res *HyperliquidStopLossUpdateResult) bool {
	if res == nil {
		return false
	}
	return res.StopLossFilledImmediately || res.StopLossOutcomeUnknown
}

func hlProtectionSyncMovesChain(res *HyperliquidProtectionSyncResult) bool {
	if res == nil {
		return false
	}
	return res.StopLossFilledImmediately || res.StopLossOutcomeUnknown
}

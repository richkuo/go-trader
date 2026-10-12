package main

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"math"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	entrySideLong  = "long"
	entrySideShort = "short"
)

const (
	entryIntentReserved   = "reserved"
	entryIntentSubmitting = "submitting"
	entryIntentSettling   = "settling"
	entryIntentCommitted  = "committed"
	entryIntentNoFill     = "no_fill"
)

const (
	entryProviderHyperliquid = "hyperliquid"
	entryProviderOKX         = "okx"
)

const entryCutoffFutureSkew = 60 * time.Second

type entryDecisionKey struct {
	Symbol     string
	IntervalMs int64
	Side       string
}

type entryDecisionRecord struct {
	LatestObservedBarMs   int64
	BootstrapThroughBarMs int64
	ConsumedThroughBarMs  int64
	UpdatedAtMs           int64
}

type entryIntent struct {
	IntentID            string
	Partition           RiskPartition
	StrategyID          string
	Symbol              string
	IntervalMs          int64
	TargetSide          string
	BarOpenMs           int64
	DecisionBoundaryMs  int64
	CutoffMs            int64
	Provider            string
	AccountKey          string
	InstrumentType      string
	ClientOrderID       string
	InitialStopClientID string
	VenueOrderID        string
	Status              string
	RequestJSON         string
	EvidenceJSON        string
	HoldReason          string
	ConfirmedQty        float64
	ConfirmedNotional   float64
	ConfirmedFee        float64
	AppliedQty          float64
	AppliedNotional     float64
	AppliedFee          float64
	SettlementSeq       int64
	ReplayPayloadJSON   string
	ReplayDeliveredSeq  int64
	CreatedAtMs         int64
	UpdatedAtMs         int64

	durableStatus string
}

func (in *entryIntent) unresolved() bool {
	if in == nil {
		return false
	}
	switch in.Status {
	case entryIntentReserved, entryIntentSubmitting, entryIntentSettling:
		return true
	}
	return false
}

func (in *entryIntent) replayPending() bool {
	return in != nil && in.Status == entryIntentCommitted && in.ReplayPayloadJSON != "" && in.ReplayDeliveredSeq < in.SettlementSeq
}

func (in *entryIntent) commitDurable() bool {
	return in != nil && in.durableStatus == entryIntentCommitted
}

func (in *entryIntent) key() entryDecisionKey {
	return entryDecisionKey{Symbol: in.Symbol, IntervalMs: in.IntervalMs, Side: in.TargetSide}
}

func entryIntentStatusRank(status string) int {
	switch status {
	case entryIntentReserved:
		return 0
	case entryIntentSubmitting:
		return 1
	case entryIntentSettling:
		return 2
	case entryIntentCommitted, entryIntentNoFill:
		return 3
	}
	return -1
}

type entryGuardState struct {
	Partition   RiskPartition
	PreExisting bool
	Decisions   map[entryDecisionKey]*entryDecisionRecord
	Intents     map[string]*entryIntent
}

func newEntryGuardState(p RiskPartition, preExisting bool) *entryGuardState {
	return &entryGuardState{
		Partition:   p,
		PreExisting: preExisting,
		Decisions:   make(map[entryDecisionKey]*entryDecisionRecord),
		Intents:     make(map[string]*entryIntent),
	}
}

func (g *entryGuardState) record(k entryDecisionKey) *entryDecisionRecord {
	rec := g.Decisions[k]
	if rec == nil {
		rec = &entryDecisionRecord{}
		g.Decisions[k] = rec
	}
	return rec
}

func (g *entryGuardState) unresolvedIntent() *entryIntent {
	if g == nil {
		return nil
	}
	ids := make([]string, 0, len(g.Intents))
	for id, in := range g.Intents {
		if in.unresolved() {
			ids = append(ids, id)
		}
	}
	if len(ids) == 0 {
		return nil
	}
	sort.Strings(ids)
	return g.Intents[ids[0]]
}

func (g *entryGuardState) sortedDecisionKeys() []entryDecisionKey {
	keys := make([]entryDecisionKey, 0, len(g.Decisions))
	for k := range g.Decisions {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		a, b := keys[i], keys[j]
		if a.Symbol != b.Symbol {
			return a.Symbol < b.Symbol
		}
		if a.IntervalMs != b.IntervalMs {
			return a.IntervalMs < b.IntervalMs
		}
		return a.Side < b.Side
	})
	return keys
}

func (g *entryGuardState) sortedIntentIDs() []string {
	ids := make([]string, 0, len(g.Intents))
	for id := range g.Intents {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

type entryIncreaseKind int

const (
	entryIncreaseNone entryIncreaseKind = iota
	entryIncreaseOpen
	entryIncreaseAdd
	entryIncreaseReverse
)

func (k entryIncreaseKind) String() string {
	switch k {
	case entryIncreaseOpen:
		return "open"
	case entryIncreaseAdd:
		return "scale-in"
	case entryIncreaseReverse:
		return "reversal"
	}
	return "none"
}

func closedBarEntryGuarded(sc StrategyConfig) bool {
	if !sc.ClosedBarDecisions {
		return false
	}
	_, why := closedBarDecisionProvider(sc)
	return why == ""
}

func signalTargetSide(signal int) string {
	switch signal {
	case 1:
		return entrySideLong
	case -1:
		return entrySideShort
	}
	return ""
}

func classifyEntryIncrease(sc StrategyConfig, signal int, closeFraction, posQty float64, posSide string) (entryIncreaseKind, string) {
	target := signalTargetSide(signal)
	if target == "" || closeFraction > 0 {
		return entryIncreaseNone, ""
	}
	holding := posQty > 0
	if sc.Type == "spot" {
		if target != entrySideLong {
			return entryIncreaseNone, ""
		}
		if holding && posSide == entrySideLong {
			return entryIncreaseNone, ""
		}
		return entryIncreaseOpen, target
	}
	allowed := (target == entrySideLong && PerpsAllowsLong(sc)) || (target == entrySideShort && PerpsAllowsShort(sc))
	if !allowed {
		return entryIncreaseNone, ""
	}
	if !holding {
		return entryIncreaseOpen, target
	}
	if posSide == target {
		if sc.AllowScaleIn {
			return entryIncreaseAdd, target
		}
		return entryIncreaseNone, ""
	}
	return entryIncreaseReverse, target
}

type entryAdmission struct {
	Applies    bool
	Kind       entryIncreaseKind
	TargetSide string
	Key        entryDecisionKey
	Partition  RiskPartition
	BarOpenMs  int64
	BoundaryMs int64
	CutoffMs   int64
	Held       bool
	HoldReason string
	CloseOnly  bool
}

func (a *entryAdmission) admitted() bool {
	return a != nil && a.Applies && !a.Held && !a.CloseOnly && (a.Kind == entryIncreaseOpen || a.Kind == entryIncreaseAdd)
}

func closedBarDecisionIdentityError(sc StrategyConfig, cb *ClosedBarDecision, now time.Time) string {
	if cb == nil || !cb.Enabled {
		return "the check returned no closed-bar decision"
	}
	if cb.Held {
		return "the closed-bar decision is held: " + cb.HoldReason
	}
	_, timeframe := strategyArgSymbolTimeframe(sc.Args)
	want, ok := closedBarDecisionIntervalsMs[timeframe]
	if !ok || want <= 0 {
		return fmt.Sprintf("timeframe %q has no fixed decision interval", timeframe)
	}
	if cb.IntervalMs != want {
		return fmt.Sprintf("the decision interval %d ms does not match the configured %s interval (%d ms)", cb.IntervalMs, timeframe, want)
	}
	if cb.BarOpenMs <= 0 {
		return "the decision candle has no opening time"
	}
	if cb.BarOpenMs%cb.IntervalMs != 0 {
		return fmt.Sprintf("the decision candle opening %d is not aligned to the %d ms interval grid", cb.BarOpenMs, cb.IntervalMs)
	}
	if cb.BarOpenMs > math.MaxInt64-cb.IntervalMs {
		return "the decision candle opening overflows the interval arithmetic"
	}
	if cb.DecisionBoundaryMs != cb.BarOpenMs+cb.IntervalMs {
		return fmt.Sprintf("the decision boundary %d is not the candle opening %d plus the interval", cb.DecisionBoundaryMs, cb.BarOpenMs)
	}
	if cb.CutoffMs <= 0 {
		return "the decision has no evaluation cutoff"
	}
	if cb.DecisionBoundaryMs > cb.CutoffMs {
		return "the decision candle closes after the evaluation cutoff"
	}
	if cb.CutoffMs-cb.DecisionBoundaryMs >= cb.IntervalMs {
		return fmt.Sprintf("the decision candle closed at %s is not the newest completed candle at the cutoff %s", formatClosedBarMs(cb.DecisionBoundaryMs), formatClosedBarMs(cb.CutoffMs))
	}
	nowMs := now.UnixMilli()
	if cb.CutoffMs > nowMs+entryCutoffFutureSkew.Milliseconds() {
		return fmt.Sprintf("the evaluation cutoff %s is in the future", formatClosedBarMs(cb.CutoffMs))
	}
	if nowMs-cb.CutoffMs >= cb.IntervalMs {
		return fmt.Sprintf("the evaluation cutoff %s is older than one interval", formatClosedBarMs(cb.CutoffMs))
	}
	return ""
}

func admitClosedBarEntry(sc StrategyConfig, s *StrategyState, mu *sync.RWMutex, symbol string, fields StrategyDecisionFields, signal int, closeFraction, posQty float64, posSide string, now time.Time) entryAdmission {
	if !closedBarEntryGuarded(sc) || s == nil {
		return entryAdmission{}
	}
	kind, target := classifyEntryIncrease(sc, signal, closeFraction, posQty, posSide)
	cb := fields.ClosedBar
	identityErr := closedBarDecisionIdentityError(sc, cb, now)
	if mu != nil {
		mu.Lock()
		defer mu.Unlock()
	}
	guard := s.EntryGuard
	adm := entryAdmission{Kind: kind, TargetSide: target, Partition: partitionFor(sc)}
	if identityErr == "" {
		adm.BarOpenMs = cb.BarOpenMs
		adm.BoundaryMs = cb.DecisionBoundaryMs
		adm.CutoffMs = cb.CutoffMs
	}
	olderThanObserved := false
	if identityErr == "" && guard != nil && guard.Partition == adm.Partition {
		longKey := entryDecisionKey{Symbol: symbol, IntervalMs: cb.IntervalMs, Side: entrySideLong}
		shortKey := entryDecisionKey{Symbol: symbol, IntervalMs: cb.IntervalMs, Side: entrySideShort}
		_, hadLong := guard.Decisions[longKey]
		_, hadShort := guard.Decisions[shortKey]
		if target != "" {
			if rec := guard.Decisions[entryDecisionKey{Symbol: symbol, IntervalMs: cb.IntervalMs, Side: target}]; rec != nil && cb.BarOpenMs < rec.LatestObservedBarMs {
				olderThanObserved = true
			}
		}
		bootstrap := guard.PreExisting && !hadLong && !hadShort
		nowMs := now.UnixMilli()
		for _, k := range []entryDecisionKey{longKey, shortKey} {
			rec := guard.record(k)
			if bootstrap && cb.BarOpenMs > rec.BootstrapThroughBarMs {
				rec.BootstrapThroughBarMs = cb.BarOpenMs
			}
			if cb.BarOpenMs > rec.LatestObservedBarMs {
				rec.LatestObservedBarMs = cb.BarOpenMs
			}
			rec.UpdatedAtMs = nowMs
		}
	}
	if kind == entryIncreaseNone {
		return entryAdmission{}
	}
	adm.Applies = true
	if identityErr == "" {
		adm.Key = entryDecisionKey{Symbol: symbol, IntervalMs: cb.IntervalMs, Side: target}
	}
	if kind == entryIncreaseReverse {
		adm.CloseOnly = true
	}
	switch {
	case guard == nil:
		adm.Held, adm.HoldReason = true, "the durable entry record for this strategy is unavailable"
	case guard.Partition != adm.Partition:
		adm.Held, adm.HoldReason = true, fmt.Sprintf("the durable entry record belongs to the %s partition, not %s", partitionLabel(guard.Partition), partitionLabel(adm.Partition))
	case identityErr != "":
		adm.Held, adm.HoldReason = true, "invalid closed-bar decision identity: "+identityErr
	case olderThanObserved:
		adm.Held, adm.HoldReason = true, fmt.Sprintf("the decision candle opened %s is older than the newest observed candle", formatClosedBarMs(adm.BarOpenMs))
	default:
		rec := guard.record(adm.Key)
		switch {
		case adm.BarOpenMs <= rec.BootstrapThroughBarMs:
			adm.Held, adm.HoldReason = true, fmt.Sprintf("upgrade bootstrap: the candle opened %s is held because no durable entry record existed when it was first observed", formatClosedBarMs(rec.BootstrapThroughBarMs))
		case adm.BarOpenMs <= rec.ConsumedThroughBarMs:
			adm.Held, adm.HoldReason = true, fmt.Sprintf("the %s entry for the candle opened %s was already taken", target, formatClosedBarMs(rec.ConsumedThroughBarMs))
		default:
			if in := guard.unresolvedIntent(); in != nil {
				adm.Held, adm.HoldReason = true, fmt.Sprintf("live entry %s (%s %s, candle %s) has no resolved venue outcome", in.IntentID, in.Symbol, in.TargetSide, formatClosedBarMs(in.BarOpenMs))
			}
		}
	}
	return adm
}

func applyEntryAdmission(adm entryAdmission, signal *int, closeFraction *float64) {
	if !adm.Applies {
		return
	}
	if adm.CloseOnly {
		*closeFraction = 1.0
		return
	}
	if adm.Held {
		*signal = 0
	}
}

func logEntryAdmission(adm entryAdmission, signalStr string, logger *StrategyLogger) {
	if logger == nil || !adm.Applies {
		return
	}
	if adm.CloseOnly {
		if adm.Held {
			logger.Warn("Closed-bar entry: %s reversal closes the open position only — the %s entry is held: %s (#1769)", signalStr, adm.TargetSide, adm.HoldReason)
			return
		}
		logger.Info("Closed-bar entry: %s reversal closes the open position this check; the %s entry for the candle opened %s waits for the next check from a flat book (#1769)", signalStr, adm.TargetSide, formatClosedBarMs(adm.BarOpenMs))
		return
	}
	if adm.Held {
		if logger.Changed("closed-bar-entry-hold", adm.TargetSide+"|"+adm.HoldReason) {
			logger.Warn("Closed-bar entry: %s %s held — %s; exits, stops and protection continue (#1769)", signalStr, adm.Kind, adm.HoldReason)
		}
		return
	}
	logger.Changed("closed-bar-entry-hold", "")
}

func entryTradesConsume(s *StrategyState, key entryDecisionKey, fromIdx int) (bool, float64, float64, float64) {
	if s == nil || fromIdx < 0 || fromIdx > len(s.TradeHistory) {
		return false, 0, 0, 0
	}
	wantSide := "buy"
	if key.Side == entrySideShort {
		wantSide = "sell"
	}
	var qty, notional, fee float64
	found := false
	for i := fromIdx; i < len(s.TradeHistory); i++ {
		t := s.TradeHistory[i]
		if t.Symbol != key.Symbol || t.IsClose || t.TradeType == TradeTypeFunding || t.Side != wantSide {
			continue
		}
		if t.Quantity <= 0 {
			continue
		}
		found = true
		qty += t.Quantity
		notional += t.Quantity * t.Price
		fee += t.ExchangeFee
	}
	return found, qty, notional, fee
}

func settleClosedBarEntryLocked(s *StrategyState, adm entryAdmission, fromIdx int, now time.Time) bool {
	if !adm.admitted() || s == nil || s.EntryGuard == nil {
		return false
	}
	consumed, _, _, _ := entryTradesConsume(s, adm.Key, fromIdx)
	if !consumed {
		return false
	}
	markEntryConsumedLocked(s.EntryGuard, adm.Key, adm.BarOpenMs, now)
	return true
}

func markEntryConsumedLocked(g *entryGuardState, key entryDecisionKey, barOpenMs int64, now time.Time) {
	if g == nil {
		return
	}
	rec := g.record(key)
	if barOpenMs > rec.ConsumedThroughBarMs {
		rec.ConsumedThroughBarMs = barOpenMs
	}
	if barOpenMs > rec.LatestObservedBarMs {
		rec.LatestObservedBarMs = barOpenMs
	}
	rec.UpdatedAtMs = now.UnixMilli()
}

func beginEntryBookingLocked(s *StrategyState, adm entryAdmission, in *entryIntent) func() []ReplayDecision {
	if s == nil || !adm.admitted() {
		return func() []ReplayDecision { return nil }
	}
	if in != nil {
		return beginLiveEntrySettlement(s)
	}
	prev := s.deferTradePersist
	s.deferTradePersist = true
	return func() []ReplayDecision {
		s.deferTradePersist = prev
		return nil
	}
}

func finishEntryBookingLocked(store *StateStore, s *StrategyState, adm entryAdmission, in *entryIntent, fromIdx int, captured []ReplayDecision, ev entryIntentEvidence, logger *StrategyLogger) {
	if !adm.admitted() {
		return
	}
	if in != nil {
		commitLiveEntrySettlementLocked(store, s, in, fromIdx, captured, ev, logger)
		return
	}
	settleClosedBarEntryLocked(s, adm, fromIdx, time.Now())
}

func finishFailedLiveEntry(store *StateStore, sc StrategyConfig, s *StrategyState, mu *sync.RWMutex, in *entryIntent, submitted bool, outcome, detail string, notifier *MultiNotifier, logger *StrategyLogger) {
	if in == nil {
		return
	}
	if noFill, label := entryIntentFromSubmissionOutcome(submitted, outcome); noFill {
		resolveEntryIntentNoFill(store, s, mu, in, entryIntentEvidence{Outcome: label, Detail: detail}, logger)
		return
	}
	reason := fmt.Sprintf("the order call returned no confirmed fill and no rejection (outcome %q)", outcome)
	if detail != "" {
		reason += ": " + detail
	}
	holdEntryIntentUnknown(store, sc, s, mu, in, reason, notifier, logger)
}

func hyperliquidEntryEvidence(er *HyperliquidExecuteResult) entryIntentEvidence {
	ev := entryIntentEvidence{Outcome: "filled"}
	if er == nil || er.Execution == nil || er.Execution.Fill == nil {
		return ev
	}
	f := er.Execution.Fill
	ev.FilledQty = f.TotalSz
	ev.AvgPx = f.AvgPx
	ev.Fee = f.Fee
	if f.OID > 0 {
		ev.VenueOrderID = fmt.Sprintf("%d", f.OID)
	}
	if f.StopLossOID > 0 {
		ev.StopOrderID = fmt.Sprintf("%d", f.StopLossOID)
		ev.StopStatus = "open"
	}
	if er.StopLossFilledImmediately {
		ev.StopStatus = "filled"
	}
	ev.StopTriggerPx = f.StopLossTriggerPx
	return ev
}

func okxEntryEvidence(er *OKXExecuteResult) entryIntentEvidence {
	ev := entryIntentEvidence{Outcome: "filled"}
	if er == nil || er.Execution == nil || er.Execution.Fill == nil {
		return ev
	}
	f := er.Execution.Fill
	ev.FilledQty = f.TotalSz
	ev.AvgPx = f.AvgPx
	ev.Fee = f.Fee
	ev.VenueOrderID = f.OID
	ev.OrderStatus = er.OrderStatus
	return ev
}

func hyperliquidExecuteOutcome(er *HyperliquidExecuteResult) (string, string) {
	if er == nil {
		return "", ""
	}
	return er.OrderOutcome, er.Error
}

func okxExecuteOutcome(er *OKXExecuteResult) (string, string) {
	if er == nil {
		return "", ""
	}
	return er.OrderOutcome, er.Error
}

func newHyperliquidCloid() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return "0x" + hex.EncodeToString(b[:]), nil
}

func newOKXClientOrderID() (string, error) {
	var b [15]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return "gt" + hex.EncodeToString(b[:]), nil
}

func newEntryIntentID() (string, error) {
	var b [12]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return "ei-" + hex.EncodeToString(b[:]), nil
}

func entryIntentSummary(in *entryIntent) string {
	if in == nil {
		return ""
	}
	parts := []string{
		in.IntentID,
		in.Status,
		in.Symbol,
		in.TargetSide,
		"candle " + formatClosedBarMs(in.BarOpenMs),
		in.Provider,
		"client " + in.ClientOrderID,
	}
	if in.VenueOrderID != "" {
		parts = append(parts, "venue "+in.VenueOrderID)
	}
	if in.HoldReason != "" {
		parts = append(parts, "hold: "+in.HoldReason)
	}
	return strings.Join(parts, " | ")
}

package main

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"
	"time"
)

const entryIntentRequestVersion = 1

const (
	hlEntryOrderExpiry = 2 * time.Minute
	hlEntryStopExpiry  = 3 * time.Minute
)

type entryIntentRequest struct {
	Version            int     `json:"version"`
	Kind               string  `json:"kind"`
	Signal             int     `json:"signal"`
	Side               string  `json:"side,omitempty"`
	Size               float64 `json:"size,omitempty"`
	StopLossPct        float64 `json:"stop_loss_pct,omitempty"`
	PrevPosQty         float64 `json:"prev_pos_qty"`
	PrevPosSide        string  `json:"prev_pos_side,omitempty"`
	PrevAvgCost        float64 `json:"prev_avg_cost,omitempty"`
	PrevPositionID     string  `json:"prev_position_id,omitempty"`
	Price              float64 `json:"price"`
	EntryATR           float64 `json:"entry_atr,omitempty"`
	ExpiresAfterMs     int64   `json:"expires_after_ms,omitempty"`
	StopExpiresAfterMs int64   `json:"stop_expires_after_ms,omitempty"`
	Script             string  `json:"script"`
	SubmittedAtMs      int64   `json:"submitted_at_ms,omitempty"`
}

type entryIntentEvidence struct {
	Outcome        string  `json:"outcome"`
	Detail         string  `json:"detail,omitempty"`
	VenueOrderID   string  `json:"venue_order_id,omitempty"`
	OrderStatus    string  `json:"order_status,omitempty"`
	FilledQty      float64 `json:"filled_qty,omitempty"`
	AvgPx          float64 `json:"avg_px,omitempty"`
	Fee            float64 `json:"fee,omitempty"`
	StopOrderID    string  `json:"stop_order_id,omitempty"`
	StopStatus     string  `json:"stop_status,omitempty"`
	StopTriggerPx  float64 `json:"stop_trigger_px,omitempty"`
	ObservedAtMs   int64   `json:"observed_at_ms"`
	RecoveredAfter bool    `json:"recovered_after_restart,omitempty"`
}

func marshalEntryJSON(v interface{}) string {
	b, err := json.Marshal(v)
	if err != nil {
		return ""
	}
	return string(b)
}

func parseEntryIntentRequest(in *entryIntent) (entryIntentRequest, error) {
	var req entryIntentRequest
	if in == nil || strings.TrimSpace(in.RequestJSON) == "" {
		return req, fmt.Errorf("entry intent has no saved request")
	}
	if err := json.Unmarshal([]byte(in.RequestJSON), &req); err != nil {
		return req, fmt.Errorf("entry intent %s request is unreadable: %w", in.IntentID, err)
	}
	if req.Version != entryIntentRequestVersion {
		return req, fmt.Errorf("entry intent %s request version %d is not supported", in.IntentID, req.Version)
	}
	return req, nil
}

func entryAccountKey(sc StrategyConfig) string {
	switch sc.Platform {
	case "hyperliquid":
		return strings.ToLower(strings.TrimSpace(hyperliquidAccountAddress()))
	case "okx":
		return "okx:" + okxInstType(sc.Args)
	}
	return sc.Platform
}

var hyperliquidAccountAddress = func() string {
	return strings.TrimSpace(os.Getenv("HYPERLIQUID_ACCOUNT_ADDRESS"))
}

func reserveLiveEntryIntent(store *StateStore, sc StrategyConfig, s *StrategyState, mu *sync.RWMutex, adm entryAdmission, provider string, req entryIntentRequest, now time.Time) (*entryIntent, error) {
	if store == nil {
		return nil, fmt.Errorf("state store unavailable")
	}
	if !adm.admitted() {
		return nil, fmt.Errorf("entry was not admitted")
	}
	id, err := newEntryIntentID()
	if err != nil {
		return nil, fmt.Errorf("generate entry intent id: %w", err)
	}
	in := &entryIntent{
		IntentID:           id,
		Partition:          adm.Partition,
		StrategyID:         sc.ID,
		Symbol:             adm.Key.Symbol,
		IntervalMs:         adm.Key.IntervalMs,
		TargetSide:         adm.Key.Side,
		BarOpenMs:          adm.BarOpenMs,
		DecisionBoundaryMs: adm.BoundaryMs,
		CutoffMs:           adm.CutoffMs,
		Provider:           provider,
		AccountKey:         entryAccountKey(sc),
		InstrumentType:     sc.Type,
		Status:             entryIntentReserved,
		CreatedAtMs:        now.UnixMilli(),
		UpdatedAtMs:        now.UnixMilli(),
	}
	switch provider {
	case entryProviderHyperliquid:
		if in.ClientOrderID, err = newHyperliquidCloid(); err != nil {
			return nil, err
		}
		if in.InitialStopClientID, err = newHyperliquidCloid(); err != nil {
			return nil, err
		}
	case entryProviderOKX:
		in.InstrumentType = okxInstType(sc.Args)
		if in.ClientOrderID, err = newOKXClientOrderID(); err != nil {
			return nil, err
		}
	default:
		return nil, fmt.Errorf("provider %q has no live entry recovery contract", provider)
	}
	req.Version = entryIntentRequestVersion
	req.Script = sc.Script
	in.RequestJSON = marshalEntryJSON(req)
	mu.Lock()
	defer mu.Unlock()
	g := s.EntryGuard
	if g == nil {
		return nil, fmt.Errorf("durable entry record unavailable")
	}
	if prev := g.unresolvedIntent(); prev != nil {
		return nil, fmt.Errorf("live entry %s is still unresolved", prev.IntentID)
	}
	if err := store.ReserveEntryIntent(s, in); err != nil {
		return nil, err
	}
	g.Intents[in.IntentID] = in
	return in, nil
}

func markEntryIntentSubmitting(store *StateStore, s *StrategyState, mu *sync.RWMutex, in *entryIntent, req entryIntentRequest) error {
	req.Version = entryIntentRequestVersion
	mu.Lock()
	defer mu.Unlock()
	next := *in
	next.RequestJSON = marshalEntryJSON(req)
	if err := store.TransitionEntryIntent(s.ID, &next, []string{entryIntentReserved}, entryIntentSubmitting); err != nil {
		return err
	}
	in.RequestJSON = next.RequestJSON
	in.Status = entryIntentSubmitting
	in.durableStatus = entryIntentSubmitting
	in.UpdatedAtMs = time.Now().UnixMilli()
	return nil
}

func hyperliquidEntrySubmitGate(store *StateStore, s *StrategyState, mu *sync.RWMutex, in *entryIntent) hlEntrySubmitGate {
	return func(side string, size, stopLossPct float64) (hlEntryOrderTags, error) {
		req, err := parseEntryIntentRequest(in)
		if err != nil {
			return hlEntryOrderTags{}, err
		}
		now := time.Now()
		req.Side = side
		req.Size = size
		req.StopLossPct = stopLossPct
		req.SubmittedAtMs = now.UnixMilli()
		req.ExpiresAfterMs = now.Add(hlEntryOrderExpiry).UnixMilli()
		req.StopExpiresAfterMs = now.Add(hlEntryStopExpiry).UnixMilli()
		if err := markEntryIntentSubmitting(store, s, mu, in, req); err != nil {
			return hlEntryOrderTags{}, err
		}
		return hlEntryOrderTags{
			Cloid:              in.ClientOrderID,
			StopCloid:          in.InitialStopClientID,
			ExpiresAfterMs:     req.ExpiresAfterMs,
			StopExpiresAfterMs: req.StopExpiresAfterMs,
		}, nil
	}
}

func okxEntrySubmitGateFor(store *StateStore, s *StrategyState, mu *sync.RWMutex, in *entryIntent) okxEntrySubmitGate {
	return func(side string, size float64) (string, error) {
		req, err := parseEntryIntentRequest(in)
		if err != nil {
			return "", err
		}
		req.Side = side
		req.Size = size
		req.SubmittedAtMs = time.Now().UnixMilli()
		if err := markEntryIntentSubmitting(store, s, mu, in, req); err != nil {
			return "", err
		}
		return in.ClientOrderID, nil
	}
}

func resolveEntryIntentNoFill(store *StateStore, s *StrategyState, mu *sync.RWMutex, in *entryIntent, ev entryIntentEvidence, logger *StrategyLogger) bool {
	if in == nil {
		return false
	}
	ev.ObservedAtMs = time.Now().UnixMilli()
	mu.Lock()
	defer mu.Unlock()
	next := *in
	next.EvidenceJSON = marshalEntryJSON(ev)
	next.HoldReason = ""
	from := []string{entryIntentReserved, entryIntentSubmitting}
	if err := store.TransitionEntryIntent(s.ID, &next, from, entryIntentNoFill); err != nil {
		in.HoldReason = fmt.Sprintf("no-fill evidence (%s) could not be saved: %v", ev.Outcome, err)
		if logger != nil {
			logger.Error("CRITICAL: live entry %s: %s — new entries stay held (#1769)", in.IntentID, in.HoldReason)
		}
		return false
	}
	in.Status = entryIntentNoFill
	in.durableStatus = entryIntentNoFill
	in.EvidenceJSON = next.EvidenceJSON
	in.HoldReason = ""
	delete(s.EntryGuard.Intents, in.IntentID)
	if logger != nil {
		logger.Info("Live entry %s resolved with no fill (%s): the decision candle stays eligible (#1769)", in.IntentID, ev.Outcome)
	}
	return true
}

func holdEntryIntentUnknown(store *StateStore, sc StrategyConfig, s *StrategyState, mu *sync.RWMutex, in *entryIntent, reason string, notifier *MultiNotifier, logger *StrategyLogger) {
	if in == nil {
		return
	}
	mu.Lock()
	in.HoldReason = reason
	in.UpdatedAtMs = time.Now().UnixMilli()
	err := store.PersistEntryGuardProgress(s)
	mu.Unlock()
	msg := fmt.Sprintf("**LIVE ENTRY OUTCOME UNKNOWN** [%s] %s %s entry %s (client order %s): %s. No new entry is sent for this strategy until the venue order is resolved; exits, stops and protection continue.",
		sc.ID, in.Symbol, in.TargetSide, in.IntentID, in.ClientOrderID, reason)
	if logger != nil {
		logger.Error("CRITICAL: %s (#1769)", msg)
		if err != nil {
			logger.Error("CRITICAL: saving the hold reason for entry %s failed: %v (the submitting record already holds the strategy)", in.IntentID, err)
		}
	}
	if notifier != nil && notifier.HasOwner() {
		notifier.SendOwnerDM(msg)
	}
}

func entryIntentFromSubmissionOutcome(submitted bool, outcome string) (resolveNoFill bool, label string) {
	if !submitted {
		return true, "not_sent"
	}
	switch outcome {
	case "rejected", "not_sent":
		return true, outcome
	}
	return false, outcome
}

func beginLiveEntrySettlement(s *StrategyState) func() []ReplayDecision {
	prevDefer := s.deferTradePersist
	prevCapture := s.replayCapture
	captured := []ReplayDecision{}
	s.deferTradePersist = true
	s.replayCapture = &captured
	return func() []ReplayDecision {
		s.deferTradePersist = prevDefer
		s.replayCapture = prevCapture
		return captured
	}
}

type entryReplayEvent struct {
	EventID        string    `json:"event_id"`
	DecisionType   string    `json:"decision_type"`
	DecidedAt      time.Time `json:"decided_at"`
	Symbol         string    `json:"symbol"`
	Side           string    `json:"side"`
	Quantity       float64   `json:"quantity"`
	ReferencePrice float64   `json:"reference_price"`
	CloseReason    string    `json:"close_reason,omitempty"`
	EntryATR       float64   `json:"entry_atr,omitempty"`
	Regime         string    `json:"regime,omitempty"`
}

func commitLiveEntrySettlementLocked(store *StateStore, s *StrategyState, in *entryIntent, fromIdx int, captured []ReplayDecision, ev entryIntentEvidence, logger *StrategyLogger) bool {
	if in == nil || s == nil || s.EntryGuard == nil {
		return false
	}
	now := time.Now()
	consumed, qty, notional, fee := entryTradesConsume(s, in.key(), fromIdx)
	ev.ObservedAtMs = now.UnixMilli()
	in.EvidenceJSON = marshalEntryJSON(ev)
	if ev.VenueOrderID != "" {
		in.VenueOrderID = ev.VenueOrderID
	}
	in.ConfirmedQty = ev.FilledQty
	in.ConfirmedNotional = ev.FilledQty * ev.AvgPx
	in.ConfirmedFee = ev.Fee
	if !consumed {
		in.HoldReason = "the venue confirmed a fill but the book did not apply it"
		if logger != nil {
			logger.Error("CRITICAL: live entry %s: %s; new entries stay held (#1769)", in.IntentID, in.HoldReason)
		}
		if err := store.PersistEntryGuardProgress(s); err != nil && logger != nil {
			logger.Error("CRITICAL: saving entry %s evidence failed: %v", in.IntentID, err)
		}
		return false
	}
	markEntryConsumedLocked(s.EntryGuard, in.key(), in.BarOpenMs, now)
	in.Status = entryIntentCommitted
	in.HoldReason = ""
	in.AppliedQty = qty
	in.AppliedNotional = notional
	in.AppliedFee = fee
	in.SettlementSeq = 1
	in.UpdatedAtMs = now.UnixMilli()
	if len(captured) > 0 {
		events := make([]entryReplayEvent, 0, len(captured))
		for i, d := range captured {
			events = append(events, entryReplayEvent{
				EventID:        fmt.Sprintf("%s:%d:%d", in.IntentID, in.SettlementSeq, i),
				DecisionType:   d.DecisionType,
				DecidedAt:      d.DecidedAt,
				Symbol:         d.Symbol,
				Side:           d.Side,
				Quantity:       d.Quantity,
				ReferencePrice: d.ReferencePrice,
				CloseReason:    d.CloseReason,
				EntryATR:       d.EntryATR,
				Regime:         d.Regime,
			})
		}
		in.ReplayPayloadJSON = marshalEntryJSON(events)
	}
	if err := store.SaveStrategyBook(s); err != nil {
		if logger != nil {
			logger.Error("CRITICAL: live entry %s booked in memory but the settlement save failed: %v — the book, the consumed candle and the trade rows retry with the next save; no new entry is sent (#1769)", in.IntentID, err)
		}
		return true
	}
	if logger != nil {
		logger.Info("Live entry %s committed: %.6f %s at the candle opened %s (#1769)", in.IntentID, qty, in.TargetSide, formatClosedBarMs(in.BarOpenMs))
	}
	publishEntryReplayLocked(store, s, in, logger)
	return true
}

func publishEntryReplayLocked(store *StateStore, s *StrategyState, in *entryIntent, logger *StrategyLogger) {
	if in == nil || !in.replayPending() || !in.commitDurable() {
		return
	}
	if decisionLogWriter == nil {
		return
	}
	var events []entryReplayEvent
	if err := json.Unmarshal([]byte(in.ReplayPayloadJSON), &events); err != nil {
		if logger != nil {
			logger.Error("Replay events for entry %s are unreadable: %v", in.IntentID, err)
		}
		return
	}
	for _, e := range events {
		if err := decisionLogWriter(ReplayDecision{
			StrategyID:         s.ID,
			DecisionType:       e.DecisionType,
			DecidedAt:          e.DecidedAt,
			Symbol:             e.Symbol,
			Side:               e.Side,
			Quantity:           e.Quantity,
			ReferencePrice:     e.ReferencePrice,
			CloseReason:        e.CloseReason,
			EntryATR:           e.EntryATR,
			Regime:             e.Regime,
			SourceEntryEventID: e.EventID,
		}); err != nil {
			if logger != nil {
				logger.Error("Replay publication for entry %s failed at %s: %v — it retries next cycle (#1769)", in.IntentID, e.EventID, err)
			}
			return
		}
	}
	if err := store.MarkEntryIntentReplayDelivered(s.ID, in, in.SettlementSeq); err != nil {
		if logger != nil {
			logger.Error("Replay delivery mark for entry %s failed: %v — republication is idempotent (#1769)", in.IntentID, err)
		}
		return
	}
	in.ReplayDeliveredSeq = in.SettlementSeq
	if !in.unresolved() && !in.replayPending() {
		delete(s.EntryGuard.Intents, in.IntentID)
	}
}

func sortedEntryGuardStrategyIDs(state *AppState) []string {
	ids := make([]string, 0, len(state.Strategies))
	for id, s := range state.Strategies {
		if s != nil && s.EntryGuard != nil && len(s.EntryGuard.Intents) > 0 {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	return ids
}

func entryGuardStartupLines(state *AppState) []string {
	var out []string
	if state == nil {
		return out
	}
	for _, id := range sortedEntryGuardStrategyIDs(state) {
		g := state.Strategies[id].EntryGuard
		for _, intentID := range g.sortedIntentIDs() {
			in := g.Intents[intentID]
			switch {
			case in.unresolved():
				out = append(out, fmt.Sprintf("[entry] %s: unresolved live entry held until venue evidence resolves it — %s", id, entryIntentSummary(in)))
			case in.replayPending():
				out = append(out, fmt.Sprintf("[entry] %s: committed entry %s still owes replay publication", id, in.IntentID))
			}
		}
	}
	return out
}

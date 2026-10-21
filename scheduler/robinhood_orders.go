package main

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	robinhoodAttemptSubmitting = "submitting"
	robinhoodAttemptOpen       = "open"
	robinhoodAttemptUnknown    = "unknown"
	robinhoodAttemptSettling   = "settling"
	robinhoodAttemptSettled    = "settled"
	robinhoodAttemptNoFill     = "no_fill"
)

const (
	robinhoodOriginSignal         = "signal"
	robinhoodOriginCircuitBreaker = "circuit_breaker"
	robinhoodOriginKillSwitch     = "kill_switch"
)

const (
	robinhoodOutcomeFilled          = "filled"
	robinhoodOutcomePartiallyFilled = "partially_filled"
	robinhoodOutcomeNoFill          = "no_fill"
	robinhoodOutcomeRejected        = "rejected"
	robinhoodOutcomeNotSent         = "not_sent"
	robinhoodOutcomeNotFound        = "not_found"
	robinhoodOutcomeOpen            = "open"
	robinhoodOutcomeUnknown         = "unknown"
	robinhoodOutcomeInconsistent    = "inconsistent"
	robinhoodOutcomeAlreadyFlat     = "already_flat"
)

const robinhoodOrderRequestVersion = 1

const robinhoodQtyTolerance = 1e-9

const robinhoodHoldReminderInterval = time.Hour

var robinhoodResolveScript = "shared_scripts/check_robinhood.py"

var robinhoodResolveFn = RunRobinhoodResolveOrder

type robinhoodOrderAttempt struct {
	AttemptID      string
	StrategyID     string
	AccountKey     string
	Symbol         string
	Origin         string
	Side           string
	RefID          string
	VenueOrderID   string
	VenueAccountID string
	Status         string
	RequestJSON    string
	EvidenceJSON   string
	HoldReason     string
	ConfirmedQty   float64
	ConfirmedAvgPx float64
	ConfirmedFee   float64
	AppliedQty     float64
	AppliedFee     float64
	SettlementSeq  int64
	CreatedAtMs    int64
	UpdatedAtMs    int64

	durableStatus string
	multiBook     bool
}

func (a *robinhoodOrderAttempt) unresolved() bool {
	if a == nil {
		return false
	}
	switch a.Status {
	case robinhoodAttemptSubmitting, robinhoodAttemptOpen, robinhoodAttemptUnknown, robinhoodAttemptSettling:
		return true
	}
	return false
}

func robinhoodAttemptStatusRank(status string) int {
	switch status {
	case robinhoodAttemptSubmitting:
		return 0
	case robinhoodAttemptOpen, robinhoodAttemptUnknown:
		return 1
	case robinhoodAttemptSettling:
		return 2
	case robinhoodAttemptSettled, robinhoodAttemptNoFill:
		return 3
	}
	return -1
}

func robinhoodAttemptSummary(a *robinhoodOrderAttempt) string {
	if a == nil {
		return ""
	}
	out := fmt.Sprintf("%s %s %s %s status=%s ref_id=%s", a.AttemptID, a.Origin, a.Side, a.Symbol, a.Status, a.RefID)
	if a.VenueOrderID != "" {
		out += " order=" + a.VenueOrderID
	}
	if a.HoldReason != "" {
		out += " hold=" + a.HoldReason
	}
	return out
}

type robinhoodOrderGuard struct {
	Attempts map[string]*robinhoodOrderAttempt
}

func newRobinhoodOrderGuard() *robinhoodOrderGuard {
	return &robinhoodOrderGuard{Attempts: make(map[string]*robinhoodOrderAttempt)}
}

func (g *robinhoodOrderGuard) sortedAttemptIDs() []string {
	if g == nil {
		return nil
	}
	ids := make([]string, 0, len(g.Attempts))
	for id := range g.Attempts {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

type robinhoodOrderRequest struct {
	Version        int               `json:"version"`
	Origin         string            `json:"origin"`
	Side           string            `json:"side"`
	Signal         int               `json:"signal,omitempty"`
	SignalStr      string            `json:"signal_str,omitempty"`
	CloseFraction  float64           `json:"close_fraction,omitempty"`
	AmountUSD      float64           `json:"amount_usd,omitempty"`
	Quantity       float64           `json:"quantity,omitempty"`
	Price          float64           `json:"price,omitempty"`
	PrevPositionID string            `json:"prev_position_id,omitempty"`
	PrevPosSide    string            `json:"prev_pos_side,omitempty"`
	PrevPosQty     float64           `json:"prev_pos_qty,omitempty"`
	Result         json.RawMessage   `json:"result,omitempty"`
	Hurst          HurstGateDecision `json:"hurst"`
	Peers          []string          `json:"peers,omitempty"`
	BudgetMs       int64             `json:"budget_ms"`
	SpawnedAtMs    int64             `json:"spawned_at_ms,omitempty"`
	Script         string            `json:"script"`
}

type robinhoodOrderEvidenceRecord struct {
	Outcome           string                 `json:"outcome"`
	Detail            string                 `json:"detail,omitempty"`
	Order             RobinhoodOrderEvidence `json:"order"`
	Cancel            RobinhoodOrderCancel   `json:"cancel"`
	RequestedQuantity float64                `json:"requested_quantity,omitempty"`
	AccountID         string                 `json:"account_id,omitempty"`
	ObservedAtMs      int64                  `json:"observed_at_ms"`
}

func marshalRobinhoodJSON(v interface{}) string {
	b, err := json.Marshal(v)
	if err != nil {
		return ""
	}
	return string(b)
}

func parseRobinhoodOrderRequest(a *robinhoodOrderAttempt) (robinhoodOrderRequest, error) {
	var req robinhoodOrderRequest
	if a == nil || strings.TrimSpace(a.RequestJSON) == "" {
		return req, fmt.Errorf("attempt has no frozen request")
	}
	if err := json.Unmarshal([]byte(a.RequestJSON), &req); err != nil {
		return req, fmt.Errorf("frozen request is unreadable: %w", err)
	}
	if req.Version != robinhoodOrderRequestVersion {
		return req, fmt.Errorf("frozen request version %d is not %d", req.Version, robinhoodOrderRequestVersion)
	}
	return req, nil
}

func sanitizeHurstForJSON(d HurstGateDecision) HurstGateDecision {
	if math.IsNaN(d.H) || math.IsInf(d.H, 0) {
		d.H = 0
		d.HKnown = false
	}
	if math.IsNaN(d.SizeMult) || math.IsInf(d.SizeMult, 0) {
		d.SizeMult = 0
	}
	return d
}

var robinhoodUsername = func() string {
	return os.Getenv("ROBINHOOD_USERNAME")
}

func robinhoodAccountKey() string {
	user := strings.ToLower(strings.TrimSpace(robinhoodUsername()))
	if user == "" {
		return ""
	}
	sum := sha256.Sum256([]byte("go-trader/robinhood-username/v1:" + user))
	return "robinhood:user-" + hex.EncodeToString(sum[:8])
}

func newRobinhoodAttemptID() (string, error) {
	var b [12]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return "rh-" + hex.EncodeToString(b[:]), nil
}

func newRobinhoodRefID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	h := hex.EncodeToString(b[:])
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:32], nil
}

var robinhoodCoinLocks sync.Map

func tryLockRobinhoodCoin(accountKey, coin string) (func(), bool) {
	v, _ := robinhoodCoinLocks.LoadOrStore(accountKey+"|"+coin, &sync.Mutex{})
	m := v.(*sync.Mutex)
	if !m.TryLock() {
		return nil, false
	}
	var once sync.Once
	return func() { once.Do(m.Unlock) }, true
}

func isRobinhoodLiveSpot(sc StrategyConfig) bool {
	return sc.Platform == "robinhood" && sc.Type == "spot" && robinhoodIsLive(sc.Args)
}

func robinhoodUnresolvedForCoinLocked(state *AppState, accountKey, coin string) *robinhoodOrderAttempt {
	if state == nil {
		return nil
	}
	ids := make([]string, 0, len(state.Strategies))
	for id := range state.Strategies {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		s := state.Strategies[id]
		if s == nil || s.RobinhoodOrders == nil {
			continue
		}
		for _, aid := range s.RobinhoodOrders.sortedAttemptIDs() {
			a := s.RobinhoodOrders.Attempts[aid]
			if a.unresolved() && a.Symbol == coin {
				return a
			}
		}
	}
	return nil
}

func robinhoodUnresolvedAttemptsLocked(state *AppState) []*robinhoodOrderAttempt {
	var out []*robinhoodOrderAttempt
	if state == nil {
		return nil
	}
	for _, s := range state.Strategies {
		if s == nil || s.RobinhoodOrders == nil {
			continue
		}
		for _, a := range s.RobinhoodOrders.Attempts {
			if a.unresolved() {
				out = append(out, a)
			}
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].CreatedAtMs != out[j].CreatedAtMs {
			return out[i].CreatedAtMs < out[j].CreatedAtMs
		}
		return out[i].AttemptID < out[j].AttemptID
	})
	return out
}

type robinhoodOrderClass int

const (
	robinhoodOrderConfirmed robinhoodOrderClass = iota
	robinhoodOrderNoFill
	robinhoodOrderOpen
	robinhoodOrderUnknown
	robinhoodOrderInconsistent
)

func (c robinhoodOrderClass) String() string {
	switch c {
	case robinhoodOrderConfirmed:
		return "confirmed fill"
	case robinhoodOrderNoFill:
		return "no fill"
	case robinhoodOrderOpen:
		return "open"
	case robinhoodOrderInconsistent:
		return "inconsistent"
	}
	return "unknown"
}

func confirmRobinhoodFill(fill *RobinhoodFill, requestedQty float64) error {
	if fill == nil {
		return fmt.Errorf("venue returned no confirmed fill")
	}
	if !(fill.AvgPx > 0) || math.IsInf(fill.AvgPx, 0) {
		return fmt.Errorf("venue fill price %v is not a finite positive number", fill.AvgPx)
	}
	if !(fill.Quantity > 0) || math.IsInf(fill.Quantity, 0) {
		return fmt.Errorf("venue fill quantity %v is not a finite positive number", fill.Quantity)
	}
	if strings.TrimSpace(fill.OID) == "" {
		return fmt.Errorf("venue fill has no order id")
	}
	if math.IsNaN(fill.Fee) || math.IsInf(fill.Fee, 0) || fill.Fee < 0 {
		return fmt.Errorf("venue fill fee %v is not a finite non-negative number", fill.Fee)
	}
	if requestedQty > 0 && fill.Quantity > requestedQty*(1+robinhoodQtyTolerance) {
		return fmt.Errorf("venue fill quantity %.10g is above the requested %.10g", fill.Quantity, requestedQty)
	}
	return nil
}

func robinhoodExitedWithCode(runErr error, code int) bool {
	var exitErr *exec.ExitError
	return errors.As(runErr, &exitErr) && exitErr.ExitCode() == code
}

func classifyRobinhoodOrderOutcome(res *RobinhoodExecuteResult, runErr error, a *robinhoodOrderAttempt, requestedQty float64) (robinhoodOrderClass, string) {
	if res == nil {
		if runErr != nil {
			return robinhoodOrderUnknown, fmt.Sprintf("the order script gave no readable result: %v", runErr)
		}
		return robinhoodOrderUnknown, "the order script gave no result"
	}
	if a != nil {
		if res.RefID != "" && res.RefID != a.RefID {
			return robinhoodOrderUnknown, fmt.Sprintf("the result names ref_id %q, the attempt %q", res.RefID, a.RefID)
		}
		if res.Order.RefID != "" && res.Order.RefID != a.RefID {
			return robinhoodOrderUnknown, fmt.Sprintf("the venue order carries ref_id %q, the attempt %q", res.Order.RefID, a.RefID)
		}
		if a.VenueOrderID != "" && res.Order.ID != "" && res.Order.ID != a.VenueOrderID {
			return robinhoodOrderUnknown, fmt.Sprintf("the venue order id %q differs from the recorded %q", res.Order.ID, a.VenueOrderID)
		}
		if a.VenueAccountID != "" && res.AccountID != "" && res.AccountID != a.VenueAccountID {
			return robinhoodOrderUnknown, "the venue account differs from the attempt's account"
		}
	}
	detail := strings.TrimSpace(res.Error)
	switch res.OrderOutcome {
	case robinhoodOutcomeFilled, robinhoodOutcomePartiallyFilled:
		if runErr != nil || res.Error != "" {
			return robinhoodOrderUnknown, fmt.Sprintf("outcome %s arrived with a failed exit or an error (%v %s)", res.OrderOutcome, runErr, res.Error)
		}
		var fill *RobinhoodFill
		if res.Execution != nil {
			fill = res.Execution.Fill
		}
		req := res.RequestedQuantity
		if !(req > 0) {
			req = requestedQty
		}
		if err := confirmRobinhoodFill(fill, req); err != nil {
			return robinhoodOrderUnknown, fmt.Sprintf("outcome %s without a usable fill: %v", res.OrderOutcome, err)
		}
		if res.Order.ID != "" && fill.OID != res.Order.ID {
			return robinhoodOrderUnknown, fmt.Sprintf("the fill order id %q differs from the venue order %q", fill.OID, res.Order.ID)
		}
		if a != nil && a.VenueOrderID != "" && fill.OID != a.VenueOrderID {
			return robinhoodOrderUnknown, fmt.Sprintf("the fill order id %q differs from the recorded %q", fill.OID, a.VenueOrderID)
		}
		return robinhoodOrderConfirmed, ""
	case robinhoodOutcomeRejected, robinhoodOutcomeNotSent, robinhoodOutcomeNoFill, robinhoodOutcomeNotFound:
		if !robinhoodExitedWithCode(runErr, 1) {
			return robinhoodOrderUnknown, fmt.Sprintf("outcome %s arrived without the failure exit (%v)", res.OrderOutcome, runErr)
		}
		if res.Execution != nil && res.Execution.Fill != nil {
			return robinhoodOrderUnknown, fmt.Sprintf("outcome %s arrived with a fill", res.OrderOutcome)
		}
		if raw := strings.TrimSpace(res.Order.CumulativeQuantity); raw != "" {
			cum, err := strconv.ParseFloat(raw, 64)
			if err != nil || math.IsNaN(cum) || math.IsInf(cum, 0) || cum != 0 {
				return robinhoodOrderUnknown, fmt.Sprintf("outcome %s with cumulative quantity %q", res.OrderOutcome, raw)
			}
		}
		if detail == "" {
			detail = res.OrderOutcome
		}
		return robinhoodOrderNoFill, detail
	case robinhoodOutcomeOpen:
		if detail == "" {
			detail = "the venue order is still open"
		}
		return robinhoodOrderOpen, detail
	case robinhoodOutcomeInconsistent:
		if detail == "" {
			detail = "the venue evidence is inconsistent"
		}
		return robinhoodOrderInconsistent, detail
	}
	if res.OrderOutcome == "" {
		return robinhoodOrderUnknown, fmt.Sprintf("the result has no order_outcome (exit %v, error %q)", runErr, res.Error)
	}
	return robinhoodOrderUnknown, fmt.Sprintf("unlisted order_outcome %q", res.OrderOutcome)
}

func robinhoodRequestedQty(a *robinhoodOrderAttempt, res *RobinhoodExecuteResult) float64 {
	if res != nil && res.RequestedQuantity > 0 {
		return res.RequestedQuantity
	}
	if a == nil {
		return 0
	}
	var ev robinhoodOrderEvidenceRecord
	if a.EvidenceJSON != "" && json.Unmarshal([]byte(a.EvidenceJSON), &ev) == nil && ev.RequestedQuantity > 0 {
		return ev.RequestedQuantity
	}
	if req, err := parseRobinhoodOrderRequest(a); err == nil && a.Side == "sell" && req.Quantity > 0 {
		return req.Quantity
	}
	return 0
}

func reserveRobinhoodAttempt(store *StateStore, state *AppState, s *StrategyState, mu *sync.RWMutex, symbol, origin, side string, req robinhoodOrderRequest, now time.Time) (*robinhoodOrderAttempt, error) {
	if store == nil {
		return nil, fmt.Errorf("state store unavailable")
	}
	if s == nil {
		return nil, fmt.Errorf("strategy state unavailable")
	}
	accountKey := robinhoodAccountKey()
	if accountKey == "" {
		return nil, fmt.Errorf("ROBINHOOD_USERNAME is not set, so the attempt cannot be bound to an account")
	}
	id, err := newRobinhoodAttemptID()
	if err != nil {
		return nil, fmt.Errorf("generate attempt id: %w", err)
	}
	refID, err := newRobinhoodRefID()
	if err != nil {
		return nil, fmt.Errorf("generate ref_id: %w", err)
	}
	req.Version = robinhoodOrderRequestVersion
	req.Origin = origin
	req.Side = side
	req.Hurst = sanitizeHurstForJSON(req.Hurst)
	req.BudgetMs = robinhoodOrderBudgetMs()
	req.SpawnedAtMs = now.UnixMilli()
	requestJSON := marshalRobinhoodJSON(req)
	if requestJSON == "" {
		return nil, fmt.Errorf("the order request could not be frozen")
	}
	a := &robinhoodOrderAttempt{
		AttemptID:   id,
		StrategyID:  s.ID,
		AccountKey:  accountKey,
		Symbol:      symbol,
		Origin:      origin,
		Side:        side,
		RefID:       refID,
		Status:      robinhoodAttemptSubmitting,
		RequestJSON: requestJSON,
		CreatedAtMs: now.UnixMilli(),
		UpdatedAtMs: now.UnixMilli(),
	}
	mu.Lock()
	defer mu.Unlock()
	if prev := robinhoodUnresolvedForCoinLocked(state, accountKey, symbol); prev != nil {
		return nil, fmt.Errorf("Robinhood order attempt %s for %s is still unresolved", prev.AttemptID, symbol)
	}
	db, err := store.dbForStrategy(s.ID)
	if err != nil {
		return nil, err
	}
	if err := db.reserveRobinhoodAttempt(a); err != nil {
		return nil, err
	}
	if s.RobinhoodOrders == nil {
		s.RobinhoodOrders = newRobinhoodOrderGuard()
	}
	s.RobinhoodOrders.Attempts[a.AttemptID] = a
	return a, nil
}

func robinhoodEvidenceFor(res *RobinhoodExecuteResult, class robinhoodOrderClass, detail string, now time.Time) string {
	ev := robinhoodOrderEvidenceRecord{Outcome: class.String(), Detail: detail, ObservedAtMs: now.UnixMilli()}
	if res != nil {
		if res.OrderOutcome != "" {
			ev.Outcome = res.OrderOutcome
		}
		ev.Order = res.Order
		ev.Cancel = res.Cancel
		if res.RequestedQuantity > 0 && !math.IsInf(res.RequestedQuantity, 0) {
			ev.RequestedQuantity = res.RequestedQuantity
		}
		ev.AccountID = res.AccountID
	}
	return marshalRobinhoodJSON(ev)
}

func recordRobinhoodOrderOutcomeLocked(store *StateStore, s *StrategyState, a *robinhoodOrderAttempt, res *RobinhoodExecuteResult, class robinhoodOrderClass, detail string, now time.Time) error {
	if store == nil || s == nil || a == nil {
		return fmt.Errorf("attempt, strategy or store unavailable")
	}
	db, err := store.dbForStrategy(s.ID)
	if err != nil {
		return err
	}
	next := *a
	next.EvidenceJSON = robinhoodEvidenceFor(res, class, detail, now)
	if res != nil {
		if res.Order.ID != "" {
			next.VenueOrderID = res.Order.ID
		}
		if res.AccountID != "" {
			next.VenueAccountID = res.AccountID
		}
	}
	from := []string{robinhoodAttemptSubmitting, robinhoodAttemptOpen, robinhoodAttemptUnknown}
	var to string
	switch class {
	case robinhoodOrderConfirmed:
		fill := res.Execution.Fill
		next.ConfirmedQty = fill.Quantity
		next.ConfirmedAvgPx = fill.AvgPx
		next.ConfirmedFee = fill.Fee
		next.VenueOrderID = fill.OID
		next.HoldReason = ""
		to = robinhoodAttemptSettling
	case robinhoodOrderNoFill:
		next.HoldReason = ""
		to = robinhoodAttemptNoFill
	case robinhoodOrderOpen:
		next.HoldReason = detail
		to = robinhoodAttemptOpen
	default:
		next.HoldReason = detail
		to = robinhoodAttemptUnknown
	}
	if err := db.transitionRobinhoodAttempt(&next, from, to); err != nil {
		if class != robinhoodOrderConfirmed && class != robinhoodOrderNoFill {
			a.HoldReason = detail
		}
		return err
	}
	next.Status = to
	next.durableStatus = to
	next.UpdatedAtMs = now.UnixMilli()
	*a = next
	if to == robinhoodAttemptNoFill && s.RobinhoodOrders != nil {
		delete(s.RobinhoodOrders.Attempts, a.AttemptID)
	}
	return nil
}

var robinhoodHoldAlerts sync.Map

func formatRobinhoodOrderHoldAlert(strategyID string, a *robinhoodOrderAttempt, reason string) string {
	order := "unknown"
	if a.VenueOrderID != "" {
		order = a.VenueOrderID
	}
	return fmt.Sprintf("**ROBINHOOD ORDER UNRESOLVED** [%s] %s %s attempt %s (origin %s, ref_id %s, venue order %s): %s. No Robinhood order for %s is sent until this order is resolved; recovery retries every cycle, or run /%sresolve-robinhood-order %s %s with the venue order id.",
		strategyID, a.Side, a.Symbol, a.AttemptID, a.Origin, a.RefID, order, reason, a.Symbol, commandPrefix, strategyID, a.AttemptID)
}

func notifyRobinhoodOrderHold(notifier *MultiNotifier, strategyID string, a *robinhoodOrderAttempt, reason string, now time.Time) {
	if a == nil {
		return
	}
	msg := formatRobinhoodOrderHoldAlert(strategyID, a, reason)
	fmt.Printf("[CRITICAL] %s\n", msg)
	if v, ok := robinhoodHoldAlerts.Load(a.AttemptID); ok {
		if last, _ := v.(time.Time); now.Sub(last) < robinhoodHoldReminderInterval {
			return
		}
	}
	robinhoodHoldAlerts.Store(a.AttemptID, now)
	if notifier == nil || !notifier.HasBackends() {
		return
	}
	notifier.SendToAllChannels(msg)
	notifier.SendOwnerDM(msg)
}

func robinhoodSnapshotStale(s *StrategyState, a *robinhoodOrderAttempt, req robinhoodOrderRequest) string {
	pos := s.Positions[a.Symbol]
	switch a.Side {
	case "sell":
		if pos == nil || pos.Side != "long" {
			return "the long position the sell was sized from is gone"
		}
		if req.PrevPositionID != "" && pos.TradePositionID != "" && pos.TradePositionID != req.PrevPositionID {
			return fmt.Sprintf("the position changed from %s to %s", req.PrevPositionID, pos.TradePositionID)
		}
		if req.PrevPosQty > 0 && math.Abs(pos.Quantity-req.PrevPosQty) > robinhoodQtyTolerance*math.Max(math.Abs(req.PrevPosQty), 1e-18) {
			return fmt.Sprintf("the position quantity changed from %.10g to %.10g", req.PrevPosQty, pos.Quantity)
		}
	case "buy":
		if pos != nil && pos.Side == "long" {
			return "a long position already exists for the bought coin"
		}
	}
	return ""
}

func robinhoodTradeHasOrderID(s *StrategyState, oid string) bool {
	if s == nil || oid == "" {
		return false
	}
	for i := range s.TradeHistory {
		if s.TradeHistory[i].ExchangeOrderID == oid {
			return true
		}
	}
	return false
}

func robinhoodSellCloseFraction(bookQty, filled, requested float64) (float64, bool) {
	if !(bookQty > 0) {
		return 0, false
	}
	if filled > bookQty*(1+robinhoodQtyTolerance) {
		return 0, false
	}
	if filled >= bookQty*(1-robinhoodQtyTolerance) {
		return 0, true
	}
	return filled / bookQty, true
}

type robinhoodSettlement struct {
	Trades    int
	Detail    string
	CashAlert string
	Settled   bool
	Hold      string
}

func settleRobinhoodSignalAttemptLocked(store *StateStore, sc StrategyConfig, s *StrategyState, db *StateDB, a *robinhoodOrderAttempt, duplicate bool, cfg *Config, logger *StrategyLogger) robinhoodSettlement {
	var out robinhoodSettlement
	if a == nil || s == nil || a.Origin != robinhoodOriginSignal {
		return out
	}
	if a.Status == robinhoodAttemptSettled || a.SettlementSeq > 0 || a.Status != robinhoodAttemptSettling {
		return out
	}
	hold := func(reason string) robinhoodSettlement {
		a.HoldReason = reason
		a.UpdatedAtMs = time.Now().UnixMilli()
		if logger != nil {
			logger.Error("CRITICAL: Robinhood attempt %s holds: %s — nothing booked (#1815)", a.AttemptID, reason)
		}
		out.Hold = reason
		return out
	}
	if duplicate || robinhoodTradeHasOrderID(s, a.VenueOrderID) {
		return hold(fmt.Sprintf("a trade with venue order %s is already booked; refusing a second booking", a.VenueOrderID))
	}
	req, err := parseRobinhoodOrderRequest(a)
	if err != nil {
		return hold(err.Error())
	}
	var result RobinhoodResult
	if len(req.Result) == 0 || json.Unmarshal(req.Result, &result) != nil {
		return hold("the frozen signal result is unreadable")
	}
	fill := &RobinhoodFill{AvgPx: a.ConfirmedAvgPx, Quantity: a.ConfirmedQty, Fee: a.ConfirmedFee, OID: a.VenueOrderID}
	if err := confirmRobinhoodFill(fill, robinhoodRequestedQty(a, nil)); err != nil {
		return hold(err.Error())
	}
	if why := robinhoodSnapshotStale(s, a, req); why != "" {
		return hold("the book changed since the order was sent: " + why)
	}
	result.Symbol = a.Symbol
	switch a.Side {
	case "sell":
		result.Signal = -1
		frac, ok := robinhoodSellCloseFraction(s.Positions[a.Symbol].Quantity, fill.Quantity, req.Quantity)
		if !ok {
			return hold(fmt.Sprintf("the confirmed sell of %.10g is above the book quantity %.10g; inconsistent", fill.Quantity, s.Positions[a.Symbol].Quantity))
		}
		result.CloseFraction = frac
	case "buy":
		result.Signal = 1
		result.CloseFraction = 0
	default:
		return hold(fmt.Sprintf("unknown side %q", a.Side))
	}
	signalStr := req.SignalStr
	if signalStr == "" {
		signalStr = strings.ToUpper(a.Side)
	}
	price := req.Price
	if !(price > 0) {
		price = fill.AvgPx
	}
	exec := &RobinhoodExecuteResult{Execution: &RobinhoodExecution{Action: a.Side, Symbol: a.Symbol, Fill: fill}}
	prevDefer := s.deferTradePersist
	s.deferTradePersist = true
	before := len(s.TradeHistory)
	var regime *RegimeConfig
	if cfg != nil {
		regime = cfg.Regime
	}
	trades, detail, cashAlert := executeRobinhoodResult(sc, s, db, &result, exec, signalStr, price, regime, cfg, req.Hurst, logger)
	s.deferTradePersist = prevDefer
	if trades <= 0 || len(s.TradeHistory) == before {
		return hold("the confirmed fill booked no trade")
	}
	now := time.Now()
	a.Status = robinhoodAttemptSettled
	a.HoldReason = ""
	a.AppliedQty = fill.Quantity
	a.AppliedFee = fill.Fee
	a.SettlementSeq = 1
	a.UpdatedAtMs = now.UnixMilli()
	out.Trades = trades
	out.Detail = detail
	out.CashAlert = cashAlert
	out.Settled = true
	if store != nil {
		if err := store.SaveStrategyBook(s); err != nil && logger != nil {
			logger.Error("CRITICAL: Robinhood attempt %s booked in memory but the settlement save failed: %v — the book, trades and attempt retry with the next save (#1815)", a.AttemptID, err)
		}
	}
	if logger != nil {
		logger.Info("Robinhood attempt %s settled: %s %.10g %s at $%.6f (order %s) (#1815)", a.AttemptID, a.Side, fill.Quantity, a.Symbol, fill.AvgPx, a.VenueOrderID)
	}
	return out
}

func robinhoodLiveOwnersForCoin(coin string, cfgs []StrategyConfig) []StrategyConfig {
	var out []StrategyConfig
	for _, sc := range cfgs {
		if isRobinhoodLiveSpot(sc) && robinhoodSymbol(sc.Args) == coin {
			out = append(out, sc)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

func settleRobinhoodEmergencyAttemptsLocked(state *AppState, cfgs []StrategyConfig, checks robinhoodBookingCheck, logger func(string)) []string {
	var settledIDs []string
	if state == nil {
		return nil
	}
	logf := func(format string, args ...interface{}) {
		line := fmt.Sprintf(format, args...)
		if logger != nil {
			logger(line)
		} else {
			fmt.Println(line)
		}
	}
	for _, a := range robinhoodUnresolvedAttemptsLocked(state) {
		if a.Origin == robinhoodOriginSignal || a.Status != robinhoodAttemptSettling || a.SettlementSeq > 0 {
			continue
		}
		owners := robinhoodLiveOwnersForCoin(a.Symbol, cfgs)
		fill := &RobinhoodFill{AvgPx: a.ConfirmedAvgPx, Quantity: a.ConfirmedQty, Fee: a.ConfirmedFee, OID: a.VenueOrderID}
		if err := confirmRobinhoodFill(fill, 0); err != nil {
			a.HoldReason = err.Error()
			logf("[CRITICAL] rh-settle: attempt %s holds: %v", a.AttemptID, err)
			continue
		}
		if err := checks.Unreadable[a.VenueOrderID]; err != nil {
			logf("[CRITICAL] rh-settle: attempt %s waits: the duplicate-booking check failed (%v); it retries next cycle", a.AttemptID, err)
			continue
		}
		dup := checks.Booked[a.VenueOrderID]
		type share struct {
			sc  StrategyConfig
			s   *StrategyState
			qty float64
		}
		var books []share
		total := 0.0
		for _, sc := range owners {
			s := state.Strategies[sc.ID]
			if s == nil {
				continue
			}
			if robinhoodTradeHasOrderID(s, a.VenueOrderID) {
				dup = true
			}
			pos := s.Positions[a.Symbol]
			if pos == nil || pos.Side != "long" || !(pos.Quantity > 0) {
				continue
			}
			books = append(books, share{sc: sc, s: s, qty: pos.Quantity})
			total += pos.Quantity
		}
		if dup {
			a.HoldReason = fmt.Sprintf("a trade with venue order %s is already booked; refusing a second booking", a.VenueOrderID)
			logf("[CRITICAL] rh-settle: attempt %s holds: %s", a.AttemptID, a.HoldReason)
			continue
		}
		remaining := fill.Quantity
		feeLeft := fill.Fee
		booked := map[string]bool{}
		for i, b := range books {
			qty := fill.Quantity * b.qty / total
			if i == len(books)-1 {
				qty = remaining
			}
			if qty > b.qty {
				qty = b.qty
			}
			if qty > remaining {
				qty = remaining
			}
			if !(qty > 0) {
				continue
			}
			fee := fill.Fee * qty / fill.Quantity
			if i == len(books)-1 || fee > feeLeft {
				fee = feeLeft
			}
			frac, _ := robinhoodSellCloseFraction(b.qty, qty, 0)
			prevDefer := b.s.deferTradePersist
			b.s.deferTradePersist = true
			before := len(b.s.TradeHistory)
			bookLogger := &StrategyLogger{stratID: b.sc.ID, writer: os.Stdout}
			_, err := ExecuteSpotSignalWithFillFeeSizedDeferredOpen(b.s, -1, a.Symbol, fill.AvgPx, qty, fee, fill.OID, frac, 1.0, bookLogger)
			b.s.deferTradePersist = prevDefer
			if err != nil || len(b.s.TradeHistory) == before {
				logf("[CRITICAL] rh-settle: attempt %s could not book %.10g %s for %s: %v", a.AttemptID, qty, a.Symbol, b.sc.ID, err)
				continue
			}
			t := &b.s.TradeHistory[len(b.s.TradeHistory)-1]
			t.Details = fmt.Sprintf("%s [%s venue close, attempt %s]", t.Details, a.Origin, a.AttemptID)
			booked[b.sc.ID] = true
			remaining -= qty
			feeLeft -= fee
		}
		if remaining > fill.Quantity*robinhoodQtyTolerance {
			logf("[CRITICAL] rh-settle: attempt %s sold %.10g %s beyond the live books (untracked coin on the account)", a.AttemptID, remaining, a.Symbol)
		}
		a.Status = robinhoodAttemptSettled
		a.HoldReason = ""
		a.AppliedQty = fill.Quantity - remaining
		a.AppliedFee = fill.Fee - feeLeft
		a.SettlementSeq = 1
		a.UpdatedAtMs = time.Now().UnixMilli()
		a.multiBook = len(booked) > 1 || (len(booked) == 1 && !booked[a.StrategyID])
		if a.multiBook {
			for _, b := range books {
				if booked[b.sc.ID] {
					b.s.robinhoodPartitionSavePending = true
				}
			}
		}
		settledIDs = append(settledIDs, a.AttemptID)
		logf("[CRITICAL] rh-settle: attempt %s (%s) applied the venue sell of %.10g %s at $%.6f (order %s) to %d book(s)", a.AttemptID, a.Origin, a.AppliedQty, a.Symbol, fill.AvgPx, a.VenueOrderID, len(booked))
	}
	return settledIDs
}

type robinhoodBookingCheck struct {
	Booked     map[string]bool
	Unreadable map[string]error
}

func robinhoodDuplicateOrderIDs(store *StateStore, state *AppState, mu *sync.RWMutex, cfgs []StrategyConfig) robinhoodBookingCheck {
	out := robinhoodBookingCheck{Booked: map[string]bool{}, Unreadable: map[string]error{}}
	if store == nil || state == nil {
		return out
	}
	live := map[string]bool{}
	for _, sc := range cfgs {
		if isRobinhoodLiveSpot(sc) {
			live[sc.ID] = true
		}
	}
	type probe struct{ strategyID, oid string }
	var probes []probe
	mu.RLock()
	for _, a := range robinhoodUnresolvedAttemptsLocked(state) {
		if a.Status != robinhoodAttemptSettling || a.VenueOrderID == "" {
			continue
		}
		seen := map[string]bool{}
		ids := []string{a.StrategyID}
		if req, err := parseRobinhoodOrderRequest(a); err == nil {
			ids = append(ids, req.Peers...)
		}
		for _, id := range ids {
			if seen[id] || !live[id] {
				continue
			}
			seen[id] = true
			probes = append(probes, probe{strategyID: id, oid: a.VenueOrderID})
		}
	}
	mu.RUnlock()
	for _, p := range probes {
		found, err := store.HasTradeWithExchangeOrderID(p.strategyID, p.oid)
		if err != nil {
			fmt.Printf("[CRITICAL] rh-settle: duplicate check for order %s (%s) failed: %v — settlement retries next cycle\n", p.oid, p.strategyID, err)
			out.Unreadable[p.oid] = err
			continue
		}
		if found {
			out.Booked[p.oid] = true
		}
	}
	return out
}

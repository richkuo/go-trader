package main

import (
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"sync"
	"time"
)

const limitLotRepresentationTolerance = 1e-6

const limitMaxLotDecimals = 12

type limitRowProof struct {
	Status    HyperliquidLimitOrderStatus
	Matched   bool
	Live      bool
	Terminal  bool
	OffBook   bool
	ExecKnown bool
	Proven    bool
	Executed  float64
	Booked    float64
	Lots      int
	Increment bool
	Settled   bool
	Permanent bool
	Hold      string

	Unbookable string

	WatermarkUnproven bool

	OpenOrdersError string
}

func (p limitRowProof) CanFinalize() bool {
	return p.Hold == "" && p.OffBook && p.Proven && p.Settled
}

func (p limitRowProof) CanBook() bool {
	return p.Hold == "" && p.Proven && p.Increment
}

func (p limitRowProof) ProvenAboveBooked() bool {
	if !p.ExecKnown || p.Lots < 0 {
		return false
	}
	exec, okE := limitLotsOf(p.Executed, p.Lots)
	booked, okB := limitLotsOf(p.Booked, p.Lots)
	return okE && okB && exec > booked
}

func (p limitRowProof) CertainForPeers() bool {
	return p.Hold == "" && p.Proven && p.Settled
}

func (p limitRowProof) holdOr(fallback string) string {
	if p.Hold != "" {
		return p.Hold
	}
	return fallback
}

func limitLotsOf(q float64, lots int) (int64, bool) {
	if math.IsNaN(q) || math.IsInf(q, 0) || lots < 0 || lots > limitMaxLotDecimals {
		return 0, false
	}
	scaled := q * math.Pow10(lots)
	n := math.Round(scaled)
	if math.Abs(scaled-n) > limitLotRepresentationTolerance*math.Max(1, math.Abs(scaled)) {
		return 0, false
	}
	return int64(n), true
}

func limitWithinHalfLot(a, b float64, lots int) bool {
	if math.IsNaN(a) || math.IsNaN(b) || math.IsInf(a, 0) || math.IsInf(b, 0) || lots < 0 || lots > limitMaxLotDecimals {
		return false
	}
	half := 0.5 * math.Pow10(-lots)
	return math.Abs(a-b) <= half+limitLotRepresentationTolerance*math.Pow10(-lots)
}

func limitStatusFinite(v float64) bool {
	return !math.IsNaN(v) && !math.IsInf(v, 0)
}

func decideLimitRowProof(o PendingLimitOrder, res *HyperliquidLimitStatusResult, callErr error) limitRowProof {
	p := limitRowProof{Booked: o.FilledSize, Lots: -1}
	if callErr != nil || res == nil || res.Error != "" {
		msg := ""
		if res != nil {
			msg = res.Error
		}
		p.Hold = strings.TrimSpace(fmt.Sprintf("order state unreadable: %v %s", callErr, msg))
		return p
	}
	p.OpenOrdersError = res.OpenOrdersError
	matches := 0
	for _, st := range res.Orders {
		if st.OID == o.OrderOID {
			matches++
			p.Status = st
		}
	}
	if matches == 0 {
		p.Hold = "status response did not include the order"
		return p
	}
	if matches > 1 {
		p.Hold = "status response lists the order more than once"
		return p
	}
	p.Matched = true
	st := p.Status
	if st.Resting != nil && *st.Resting {
		p.Live = true
	}
	if st.OrderState == "" || st.FillsProven == nil {
		p.Hold = "the status reply carries no order-status or fill proof (an older check_hyperliquid.py answered), so nothing is booked or deleted until the paired Python is deployed"
		return p
	}
	if st.SzDecimals == nil {
		p.Hold = fmt.Sprintf("the venue lot metadata for %s is unknown (the status reply carries no lot precision), so sizes cannot be compared in venue lots and nothing is booked or deleted until the venue metadata read succeeds", o.Symbol)
		return p
	}
	if st.Coin != o.Symbol {
		p.Hold = fmt.Sprintf("the status reply names coin %q, not the row's %q", st.Coin, o.Symbol)
		return p
	}
	if *st.SzDecimals < 0 || *st.SzDecimals > limitMaxLotDecimals {
		p.Hold = fmt.Sprintf("the status reply carries an invalid lot precision %d", *st.SzDecimals)
		return p
	}
	p.Lots = *st.SzDecimals
	if st.StatusError != "" {
		p.Hold = "order status unproven: " + st.StatusError
		return p
	}
	switch st.OrderState {
	case "live":
	case "filled", "cancelled", "rejected":
		p.Terminal = true
	default:
		p.Hold = fmt.Sprintf("the status reply carries an unfamiliar order state %q", st.OrderState)
		return p
	}
	if st.OrigSize == nil || st.ExecutedSize == nil {
		p.Hold = "the status reply carries no original or executed size"
		return p
	}
	orig, exec := *st.OrigSize, *st.ExecutedSize
	if !limitStatusFinite(orig) || !limitStatusFinite(exec) || orig <= 0 || exec < 0 || exec > orig+0.5*math.Pow10(-p.Lots) {
		p.Hold = fmt.Sprintf("the status reply carries invalid sizes (original %v, executed %v)", orig, exec)
		return p
	}
	if !p.Terminal && !p.Live {
		p.Hold = "the order status reports the order live but the reply does not confirm it rests on the book"
		return p
	}
	if p.Terminal && p.Live {
		p.Hold = fmt.Sprintf("the order status reports %q but the reply says it still rests on the book", st.OrderStatus)
		return p
	}
	if p.Terminal && st.Resting != nil && !*st.Resting {
		p.OffBook = true
	}
	if !limitWithinHalfLot(orig, o.OrderSize, p.Lots) {
		p.Hold = fmt.Sprintf("the venue's original size %.8f does not match the row's requested size %.8f at %d lot decimals", orig, o.OrderSize, p.Lots)
		return p
	}
	execLots, okExec := limitLotsOf(exec, p.Lots)
	if !okExec {
		p.Hold = fmt.Sprintf("the executed size %.10f is not on the venue lot grid", exec)
		return p
	}
	p.Executed = exec
	p.ExecKnown = true
	bookedLots, okBooked := limitLotsOf(o.FilledSize, p.Lots)
	if !okBooked {
		p.Hold = fmt.Sprintf("the booked size %.10f is not on the venue lot grid, so it cannot be compared with the venue", o.FilledSize)
		return p
	}
	if execLots < bookedLots {
		p.Hold = fmt.Sprintf("the venue executed %.8f, less than the %.8f the book holds for this order", exec, o.FilledSize)
		return p
	}
	if !*st.FillsProven {
		p.Permanent = st.FillsReason == "permanent" && p.OffBook && execLots > bookedLots
		reason := st.FillsError
		if reason == "" {
			reason = "the fill history was not proven"
		}
		p.Hold = "fills unproven: " + reason
		return p
	}
	if !limitStatusFinite(st.FilledSize) || !limitStatusFinite(st.AvgPx) || !limitStatusFinite(st.Fee) {
		p.Hold = "the proven fill totals are not finite"
		return p
	}
	filledLots, okFilled := limitLotsOf(st.FilledSize, p.Lots)
	if !okFilled || filledLots != execLots {
		p.Hold = fmt.Sprintf("the proven fill total %.8f does not equal the executed size %.8f", st.FilledSize, exec)
		return p
	}
	if execLots > 0 && st.AvgPx <= 0 {
		p.Hold = "the proven fills carry no average price"
		return p
	}
	p.Proven = true
	if execLots == bookedLots {
		p.Settled = true
	} else {
		p.Increment = true
	}
	return p
}

type limitRowOperatorEvidence struct {
	OrderOID     int64     `json:"oid"`
	Coin         string    `json:"coin"`
	OrderStatus  string    `json:"order_status"`
	OrderState   string    `json:"order_state"`
	OffBook      bool      `json:"off_book"`
	ExecutedSize float64   `json:"executed_size"`
	BookedSize   float64   `json:"booked_size"`
	FillsProven  bool      `json:"fills_proven"`
	Permanent    bool      `json:"fill_proof_permanent"`
	Unbookable   bool      `json:"unbookable"`
	Unproven     bool      `json:"watermark_unproven,omitempty"`
	Reason       string    `json:"reason"`
	ObservedAt   time.Time `json:"observed_at"`
}

func limitOperatorEvidenceFor(o PendingLimitOrder, p limitRowProof, reason string, now time.Time) limitRowOperatorEvidence {
	return limitRowOperatorEvidence{
		OrderOID:     o.OrderOID,
		Coin:         o.Symbol,
		OrderStatus:  p.Status.OrderStatus,
		OrderState:   p.Status.OrderState,
		OffBook:      p.OffBook,
		ExecutedSize: p.Executed,
		BookedSize:   o.FilledSize,
		FillsProven:  p.Proven,
		Permanent:    p.Permanent,
		Unbookable:   p.Unbookable != "",
		Unproven:     p.WatermarkUnproven,
		Reason:       reason,
		ObservedAt:   now.UTC(),
	}
}

func marshalLimitOperatorEvidence(ev limitRowOperatorEvidence) string {
	blob, err := json.Marshal(ev)
	if err != nil {
		return ""
	}
	return string(blob)
}

func parseLimitOperatorEvidence(raw string) (limitRowOperatorEvidence, bool) {
	var ev limitRowOperatorEvidence
	if strings.TrimSpace(raw) == "" {
		return ev, false
	}
	if err := json.Unmarshal([]byte(raw), &ev); err != nil {
		return ev, false
	}
	return ev, true
}

func limitOperatorRequiredWarranted(p limitRowProof) bool {
	if !p.OffBook || !p.Terminal {
		return false
	}
	if p.Permanent {
		return true
	}
	if p.Unbookable != "" {
		return p.Proven
	}
	return p.Hold == "" && p.Proven && p.Increment
}

func limitOperatorEvidenceRefusal(o PendingLimitOrder) string {
	ev, ok := parseLimitOperatorEvidence(o.OperatorEvidenceJSON)
	if !ok {
		return fmt.Sprintf("refusing: the operator-required marker on oid=%d carries no terminal, off-book proof (it predates that evidence or could not be read) — the scheduler revalidates the row on its next poll; retry once it has recorded that proof", o.OrderOID)
	}
	switch {
	case ev.OrderOID != o.OrderOID || ev.Coin != o.Symbol:
		return fmt.Sprintf("refusing: the operator-required evidence names %s oid=%d, not this row's %s oid=%d", ev.Coin, ev.OrderOID, o.Symbol, o.OrderOID)
	case !ev.OffBook:
		return fmt.Sprintf("refusing: the scheduler has not proven that oid=%d is off the book — cancel it on Hyperliquid and let the scheduler record terminal proof first", o.OrderOID)
	case ev.OrderState != "filled" && ev.OrderState != "cancelled" && ev.OrderState != "rejected":
		return fmt.Sprintf("refusing: the recorded order state %q for oid=%d is not terminal", ev.OrderState, o.OrderOID)
	case !ev.Permanent && !(ev.FillsProven && (ev.Unbookable || ev.ExecutedSize > ev.BookedSize)):
		return fmt.Sprintf("refusing: the evidence for oid=%d shows no proven unbooked fill, no proven execution that the scheduler's own book refuses, and no permanent fill-proof failure, so the scheduler can still resolve it", o.OrderOID)
	case math.Abs(ev.BookedSize-o.FilledSize) > limitFillEpsilon:
		return fmt.Sprintf("refusing: the row's booked size %.8f changed since the evidence was recorded (%.8f) — the scheduler revalidates it next poll", o.FilledSize, ev.BookedSize)
	}
	return ""
}

var limitEntryResyncHolds = struct {
	sync.Mutex
	keys       map[string]bool
	unreadable bool
}{keys: map[string]bool{}}

func publishLimitEntryResyncHolds(orders []PendingLimitOrder) {
	keys := make(map[string]bool, len(orders))
	for _, o := range orders {
		keys[hlStopFillKey(o.StrategyID, o.Symbol)] = true
	}
	limitEntryResyncHolds.Lock()
	limitEntryResyncHolds.keys = keys
	limitEntryResyncHolds.unreadable = false
	limitEntryResyncHolds.Unlock()
}

func holdLimitEntryResyncUnreadable() {
	limitEntryResyncHolds.Lock()
	limitEntryResyncHolds.unreadable = true
	limitEntryResyncHolds.Unlock()
}

func limitEntryResyncHeld(strategyID, symbol string) bool {
	if strategyID == "" || symbol == "" {
		return false
	}
	limitEntryResyncHolds.Lock()
	defer limitEntryResyncHolds.Unlock()
	return limitEntryResyncHolds.unreadable || limitEntryResyncHolds.keys[hlStopFillKey(strategyID, symbol)]
}

func limitEntryTradeLineage(store *StateStore, o PendingLimitOrder) (string, string, bool) {
	if store == nil {
		return "", "the state store is unavailable to cross-check the row's booked fills", false
	}
	qty, positionIDs, err := store.ManualLimitEntryTradeTotals(o.StrategyID, fmt.Sprintf("%d", o.OrderOID))
	if err != nil {
		return "", fmt.Sprintf("the row's durable manual limit trades could not be read (%v)", err), false
	}
	if math.Abs(qty-o.FilledSize) > limitFillEpsilon*math.Max(1, o.FilledSize) {
		return "", fmt.Sprintf("the row's fill watermark %.8f does not match the %.8f its durable manual limit trades record", o.FilledSize, qty), true
	}
	if len(positionIDs) != 1 {
		return "", fmt.Sprintf("the row's durable manual limit trades name %d positions, so its lineage is ambiguous", len(positionIDs)), true
	}
	return positionIDs[0], "", false
}

func limitWatermarkUnproven(o PendingLimitOrder) string {
	if o.FilledSize <= limitFillEpsilon || o.FillProofBound {
		return ""
	}
	return fmt.Sprintf("the row's fill watermark %.8f carries no proof that it committed in one transaction with its book, fee and trades (it was written before atomic booking, or an older build advanced it), and neither matching trade totals nor a proof version can show that the book holds it", o.FilledSize)
}

func holdUnprovenLimitWatermark(p *limitRowProof, o PendingLimitOrder) bool {
	why := limitWatermarkUnproven(o)
	if why == "" {
		return false
	}
	if p.Hold == "" {
		p.Hold = why
	}
	p.Unbookable = why
	p.WatermarkUnproven = true
	return true
}

func limitRowOwnerBlock(sc StrategyConfig, known bool, o PendingLimitOrder) string {
	if block := killSwitchLimitOrderAdoptionBlock(sc, known); block != "" {
		return block
	}
	if coin := strings.TrimSpace(sc.Symbol); coin != "" && coin != o.Symbol {
		return fmt.Sprintf("the strategy is configured for %s, not the row's %s", coin, o.Symbol)
	}
	return ""
}

func holdIneligibleLimitOwner(p *limitRowProof, o PendingLimitOrder, block string) bool {
	if block == "" || o.FilledSize <= limitFillEpsilon {
		return false
	}
	why := fmt.Sprintf("its owner cannot carry the %.8f this order already booked because %s, so the row stays for review", o.FilledSize, block)
	if p.Hold == "" {
		p.Hold = why
	}
	p.Unbookable = why
	return true
}

func limitCoinOperatorHold(cfg *Config, store *StateStore, symbol string) (string, error) {
	if store == nil {
		return "", fmt.Errorf("the state store is unavailable")
	}
	rows, err := store.LoadPendingLimitOrders()
	if err != nil {
		return "", err
	}
	for _, o := range rows {
		if o.Symbol != symbol {
			continue
		}
		var held limitRowProof
		sc, known := scByIDLookup(cfg, o.StrategyID)
		if !holdUnprovenLimitWatermark(&held, o) && !holdIneligibleLimitOwner(&held, o, limitRowOwnerBlock(sc, known, o)) {
			continue
		}
		return fmt.Sprintf("pending limit row %s holds every limit fill on %s until an operator clears it: %s. A new limit order on %s could fill and stay unbooked with no stop-loss, so it is refused — follow the owner alert for that row, clear it with `go-trader manual-clear-limit-row %d --flattened` once the scheduler has marked it, then retry",
			killSwitchLimitOrderLabel(o), symbol, held.Unbookable, symbol, o.OrderOID), nil
	}
	return "", nil
}

func verifyLimitIncrementLineage(store *StateStore, state *AppState, o PendingLimitOrder) (string, bool) {
	if o.FilledSize <= limitFillEpsilon {
		return "", false
	}
	positionID, why, final := limitEntryTradeLineage(store, o)
	if why != "" {
		return why, final
	}
	var pos *Position
	if state != nil {
		if ss := state.Strategies[o.StrategyID]; ss != nil {
			pos = ss.Positions[o.Symbol]
		}
	}
	if pos == nil || pos.Quantity <= 0 || pos.Side != o.Side || pos.TradePositionID != positionID || !manualPositionOwnedByStrategy(pos, o.StrategyID) {
		return fmt.Sprintf("the %.8f this order already booked is not carried by an open %s position of the same lineage in the book, so a later fill has nothing to grow", o.FilledSize, o.Side), true
	}
	return "", false
}

func (st *StateStore) tryLiveManualActionLock() (func(), error) {
	path, err := st.liveFilePath()
	if err != nil {
		return nil, err
	}
	return acquireManualActionFileLockWithWait(path, 0)
}

func limitRowUnchanged(a, b PendingLimitOrder) bool {
	return a.ID == b.ID && a.OrderOID == b.OrderOID && a.StrategyID == b.StrategyID && a.storedOwner == b.storedOwner &&
		a.Symbol == b.Symbol && a.Side == b.Side &&
		a.FilledSize == b.FilledSize && a.FillFee == b.FillFee && a.AvgFillPrice == b.AvgFillPrice &&
		a.FillProofVersion == b.FillProofVersion && a.FillProofBinding == b.FillProofBinding &&
		a.OperatorEvidenceJSON == b.OperatorEvidenceJSON
}

func recheckLimitRow(store *StateStore, o PendingLimitOrder) string {
	if store == nil {
		return "the state store is unavailable"
	}
	rows, err := store.LoadPendingLimitOrders()
	if err != nil {
		return fmt.Sprintf("the queue could not be re-read under the manual-action lock (%v)", err)
	}
	for _, r := range rows {
		if r.ID == o.ID {
			if limitRowUnchanged(r, o) {
				return ""
			}
			return "the row changed under a concurrent manual action"
		}
	}
	return "the row vanished under a concurrent manual action"
}

package main

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"maps"
	"math"
	"math/big"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	okxProtectReserved  = "reserved"
	okxProtectArming    = "arming"
	okxProtectArmed     = "armed"
	okxProtectAmending  = "amending"
	okxProtectCanceling = "canceling"
	okxProtectTriggered = "triggered"
	okxProtectReducing  = "reducing"
	okxProtectRetired   = "retired"
)

const okxStopScript = "shared_scripts/check_okx.py"
const okxPaperTakerFee = "0.0005"

type okxProtection struct {
	RecordID       string
	StrategyID     string
	AccountKey     string
	InstID         string
	Symbol         string
	Side           string
	AlgoClientID   string
	EntryClientID  string
	AlgoID         string
	Status         string
	Owner          string
	PolicyJSON     string
	TriggerPx      string
	SizeContracts  string
	RequestJSON    string
	EvidenceJSON   string
	IncidentID     string
	Alerted        bool
	Version        int
	durableVersion int
	PositionID     string
	PlaceAttempts  int
	CreatedAtMs    int64
	UpdatedAtMs    int64
	AppliedFills   map[string]bool
	PendingFills   []okxProtectionFill
	Replaces       *okxProtection
}

type okxProtectionFill struct {
	FillID string
	Side   string
	Qty    float64
	Px     float64
	Fee    float64
	OrdID  string
}

type okxStopCommandResult struct {
	Contract       string        `json:"contract"`
	Op             string        `json:"op"`
	OK             bool          `json:"ok"`
	Reason         string        `json:"reason,omitempty"`
	PosMode        string        `json:"pos_mode,omitempty"`
	Flat           bool          `json:"flat"`
	Cross          bool          `json:"cross"`
	OpenOrders     int           `json:"open_orders"`
	OpenAlgos      int           `json:"open_algos"`
	OtherPositions int           `json:"other_positions"`
	Mark           string        `json:"mark,omitempty"`
	Tick           string        `json:"tick,omitempty"`
	Lot            string        `json:"lot,omitempty"`
	MinSz          string        `json:"min_sz,omitempty"`
	CtVal          string        `json:"ct_val,omitempty"`
	MMR            string        `json:"mmr,omitempty"`
	Equity         string        `json:"equity,omitempty"`
	Fee            string        `json:"fee,omitempty"`
	LiqPx          string        `json:"liq_px,omitempty"`
	LiqKnown       bool          `json:"liq_known"`
	PosContracts   string        `json:"pos_contracts,omitempty"`
	PosSide        string        `json:"pos_side,omitempty"`
	AlgoID         string        `json:"algo_id,omitempty"`
	AlgoState      string        `json:"algo_state,omitempty"`
	AlgoClientID   string        `json:"algo_client_id,omitempty"`
	ChildTerminal  bool          `json:"child_terminal"`
	OrderAbsent    bool          `json:"order_absent"`
	OrderIDs       []string      `json:"order_ids,omitempty"`
	TriggerPx      string        `json:"trigger_px,omitempty"`
	Sz             string        `json:"sz,omitempty"`
	Contracts      string        `json:"contracts,omitempty"`
	Fills          []okxStopFill `json:"fills,omitempty"`
}

type okxStopFill struct {
	FillID string `json:"fill_id"`
	Side   string `json:"side"`
	Qty    string `json:"qty"`
	Px     string `json:"px"`
	Fee    string `json:"fee"`
	OrdID  string `json:"ord_id"`
}

type okxPendingClose struct {
	ClientID    string  `json:"client_id"`
	Quantity    float64 `json:"quantity"`
	Circuit     bool    `json:"circuit,omitempty"`
	ExpiresAtMs int64   `json:"expires_at_ms"`
}

type okxStopRequest struct {
	Close      *okxPendingClose `json:"pending_close,omitempty"`
	Reduce     *okxPendingClose `json:"pending_reduce,omitempty"`
	Entry      string           `json:"entry,omitempty"`
	Mark       string           `json:"mark,omitempty"`
	EntryATR   string           `json:"entry_atr,omitempty"`
	HighWater  string           `json:"high_water,omitempty"`
	Tick       string           `json:"tick,omitempty"`
	Lot        string           `json:"lot,omitempty"`
	CtVal      string           `json:"ct_val,omitempty"`
	MMR        string           `json:"mmr,omitempty"`
	Fee        string           `json:"fee,omitempty"`
	Equity     string           `json:"equity,omitempty"`
	VenueLiq   string           `json:"venue_liq,omitempty"`
	VenueKnown bool             `json:"venue_liq_known,omitempty"`
	Trigger    string           `json:"trigger,omitempty"`
	Contracts  string           `json:"contracts,omitempty"`
}

func (p *okxProtection) unresolved() bool {
	if p == nil {
		return false
	}
	switch p.Status {
	case okxProtectReserved, okxProtectArming, okxProtectArmed, okxProtectAmending, okxProtectCanceling, okxProtectTriggered, okxProtectReducing:
		return true
	}
	return false
}

func (p *okxProtection) holdsNewExposure() bool {
	return p.unresolved() && (p.Status != okxProtectArmed || p.IncidentID != "")
}

func okxOwnsProtectedBook(sc StrategyConfig, s *StrategyState, symbol string) bool {
	return s != nil && okxStopWanted(sc) && okxIsLive(sc.Args) &&
		s.OKXProtection.unresolved() && s.OKXProtection.Symbol == symbol
}

func okxProtectionUnresolved(state *AppState, id string) bool {
	if state == nil || state.Strategies == nil || state.Strategies[id] == nil {
		return false
	}
	return state.Strategies[id].OKXProtection.unresolved()
}

var (
	okxInstrumentMu    sync.Mutex
	okxInstrumentLocks = map[string]*sync.Mutex{}
)

func okxInstrumentLock(account, inst string) *sync.Mutex {
	okxInstrumentMu.Lock()
	defer okxInstrumentMu.Unlock()
	key := account + "|" + inst
	m := okxInstrumentLocks[key]
	if m == nil {
		m = &sync.Mutex{}
		okxInstrumentLocks[key] = m
	}
	return m
}

func (st *StateStore) ReserveOKXProtection(s *StrategyState, p *okxProtection) error {
	if st == nil || s == nil {
		return fmt.Errorf("state store unavailable")
	}
	db, err := st.dbForStrategy(s.ID)
	if err != nil {
		return err
	}
	return db.reserveOKXProtection(s, p)
}

func (st *StateStore) LoadOKXProtections(state *AppState, cfgs []StrategyConfig) error {
	if st == nil || state == nil {
		return fmt.Errorf("state store unavailable")
	}
	primary := st.primary()
	for _, role := range st.order {
		db := st.file(role)
		if db == nil {
			continue
		}
		rows, sids, err := db.loadUnresolvedOKXProtection()
		if err != nil {
			return err
		}
		for i, p := range rows {
			if db != primary || !robinhoodRoleOwnsLive(st.layout, role) {
				return fmt.Errorf("unresolved OKX stop %s sits in the %s state file, which does not own the live partition", p.RecordID, role)
			}
			procID, mapped := db.mapStoredStrategyID(sids[i])
			if !mapped {
				return fmt.Errorf("unresolved OKX stop %s belongs to stored strategy %q, which is not configured", p.RecordID, sids[i])
			}
			s := state.Strategies[procID]
			if s == nil {
				return fmt.Errorf("unresolved OKX stop %s belongs to strategy %q, which has no state", p.RecordID, procID)
			}
			if s.OKXProtection != nil && s.OKXProtection.unresolved() {
				return fmt.Errorf("strategy %q has two unresolved OKX stops", procID)
			}
			p.StrategyID = procID
			s.OKXProtection = p
		}
	}
	return nil
}

func runOKXStop(op string, fields map[string]string) (okxStopCommandResult, error) {
	args := []string{"--okx-stop", "--op=" + op}
	keys := make([]string, 0, len(fields))
	for k := range fields {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if fields[k] == "" {
			continue
		}
		args = append(args, "--"+k+"="+fields[k])
	}
	sideEffect := op == "place" || op == "amend" || op == "cancel" || op == "reduce"
	var stdout, stderr []byte
	var runErr error
	if sideEffect {
		stdout, stderr, runErr = runPythonSideEffect(okxStopScript, args)
	} else {
		stdout, stderr, runErr = runPythonReadOnly(okxStopScript, args)
	}
	var res okxStopCommandResult
	if err := json.Unmarshal(stdout, &res); err != nil {
		if runErr != nil {
			return res, fmt.Errorf("okx stop %s: %w (%s)", op, runErr, strings.TrimSpace(string(stderr)))
		}
		return res, fmt.Errorf("okx stop %s output: %v", op, err)
	}
	if res.Contract != okxStopContract || res.Op != op {
		return res, fmt.Errorf("okx stop %s contract %q", op, res.Contract)
	}
	if !res.OK {
		reason := res.Reason
		if reason == "" && runErr != nil {
			reason = runErr.Error()
		}
		return res, fmt.Errorf("okx stop %s: %s", op, reason)
	}
	return res, nil
}

func okxStopFields(symbol string, extra map[string]string) map[string]string {
	out := map[string]string{"symbol": symbol, "inst-type": "swap"}
	for k, v := range extra {
		out[k] = v
	}
	return out
}

func admitOKXStopEntry(sc StrategyConfig, s *StrategyState, symbol string, signal int, closeFraction, posQty float64, posSide string, now time.Time) entryAdmission {
	if closedBarEntryGuarded(sc) || s == nil || !okxStopWanted(sc) {
		return entryAdmission{}
	}
	kind, target := classifyEntryIncrease(sc, signal, closeFraction, posQty, posSide)
	if kind == entryIncreaseReverse {
		return entryAdmission{Applies: true, Kind: kind, TargetSide: target, CloseOnly: true, Partition: partitionFor(sc)}
	}
	if kind != entryIncreaseOpen {
		return entryAdmission{}
	}
	if !okxIsLive(sc.Args) {
		return entryAdmission{}
	}
	adm := entryAdmission{
		Applies:    true,
		Kind:       kind,
		TargetSide: target,
		Key:        entryDecisionKey{Symbol: symbol, IntervalMs: 1, Side: target},
		Partition:  partitionFor(sc),
		BarOpenMs:  now.UnixMilli(),
		BoundaryMs: now.UnixMilli(),
		CutoffMs:   now.UnixMilli(),
	}
	guard := s.EntryGuard
	switch {
	case guard == nil:
		adm.Held, adm.HoldReason = true, "the durable entry record for this strategy is unavailable"
	case guard.Partition != adm.Partition:
		adm.Held, adm.HoldReason = true, "the durable entry record belongs to another partition"
	default:
		if in := guard.unresolvedIntent(); in != nil {
			adm.Held, adm.HoldReason = true, fmt.Sprintf("live entry %s has no resolved venue outcome", in.IntentID)
		}
	}
	return adm
}

func okxPreflightAllowsEntry(symbol string, plannedContracts ...string) (okxStopCommandResult, error) {
	fields := okxStopFields(symbol, nil)
	if len(plannedContracts) > 0 {
		fields["contracts"] = plannedContracts[0]
	}
	res, err := runOKXStop("preflight", fields)
	if err != nil {
		return res, err
	}
	if !res.OK || res.PosMode != "net_mode" || !res.Flat || !res.Cross || res.OpenOrders > 0 || res.OpenAlgos > 0 || res.OtherPositions > 0 {
		return res, fmt.Errorf("preflight refused the entry")
	}
	return res, nil
}

func reserveOKXStop(store *StateStore, sc StrategyConfig, s *StrategyState, in *entryIntent, owner string) error {
	pol, _, err := okxStopPolicyOf(sc)
	if err != nil {
		return err
	}
	raw, err := json.Marshal(pol)
	if err != nil {
		return err
	}
	now := tradingNow().UnixMilli()
	p := &okxProtection{
		RecordID:      in.IntentID,
		StrategyID:    sc.ID,
		AccountKey:    in.AccountKey,
		InstID:        okxSwapRouteInstID(strings.ToUpper(in.Symbol)),
		Symbol:        in.Symbol,
		Side:          in.TargetSide,
		AlgoClientID:  in.InitialStopClientID,
		EntryClientID: in.ClientOrderID,
		Status:        okxProtectReserved,
		Owner:         owner,
		PolicyJSON:    string(raw),
		Version:       1,
		CreatedAtMs:   now,
		UpdatedAtMs:   now,
		AppliedFills:  map[string]bool{},
	}
	if err := store.ReserveOKXProtection(s, p); err != nil {
		return err
	}
	s.OKXProtection = p
	return nil
}

func (p *okxProtection) bump(status string) {
	p.Status = status
	p.Version++
	p.UpdatedAtMs = tradingNow().UnixMilli()
}

func saveOKXProtection(store *StateStore, s *StrategyState) error {
	if store == nil || s == nil {
		return fmt.Errorf("state store unavailable")
	}
	return store.SaveStrategyBook(s)
}

func armOKXStopAfterEntry(store *StateStore, sc StrategyConfig, s *StrategyState, mu *sync.RWMutex, in *entryIntent, fill *OKXFill, notifier *MultiNotifier, logger *StrategyLogger) {
	if in == nil || fill == nil || s == nil || s.OKXProtection == nil || !okxStopWanted(sc) || okxEntryUnresolved(s, mu) {
		return
	}
	p := s.OKXProtection
	if p.AlgoClientID == "" || p.EntryClientID != in.ClientOrderID {
		return
	}
	lock := okxInstrumentLock(p.AccountKey, p.InstID)
	lock.Lock()
	defer lock.Unlock()
	if p.Status != okxProtectReserved && p.Status != okxProtectArming {
		return
	}
	ref, err := runOKXStop("arm-reference", okxStopFields(p.Symbol, nil))
	if err != nil || ref.Flat || ref.PosSide == "" {
		detail := "the filled position could not be read"
		if err != nil {
			detail = err.Error()
		}
		okxProtectionIncident(store, s, mu, p, detail, notifier, logger)
		return
	}
	if (p.Side != "" && p.Side != ref.PosSide) || !okxFillMatchesContracts(fill.TotalSz, ref.PosContracts) {
		okxProtectionIncident(store, s, mu, p, "the confirmed fill size does not match the venue position", notifier, logger)
		return
	}
	pos := ref
	geom, err := okxGeometryFor(p, pos, ref, fill)
	if err != nil {
		okxProtectionIncident(store, s, mu, p, err.Error(), notifier, logger)
		return
	}
	if atr := okxIntentATR(in); atr != "" {
		geom.EntryATR = atr
	}
	req := okxStopRequest{Entry: geom.Entry, Mark: geom.Mark, EntryATR: geom.EntryATR, HighWater: geom.HighWater, Tick: geom.Tick, Lot: ref.Lot, CtVal: ref.CtVal, MMR: ref.MMR, Fee: ref.Fee, Equity: ref.Equity, VenueLiq: pos.LiqPx, VenueKnown: pos.LiqKnown, Trigger: "", Contracts: pos.PosContracts}
	final, err := okxFinalizeStop(geom)
	if err != nil {
		okxProtectionIncident(store, s, mu, p, err.Error(), notifier, logger)
		return
	}
	req.Trigger = final.Trigger
	raw, _ := json.Marshal(req)
	mu.Lock()
	book := s.Positions[p.Symbol]
	if book == nil || book.Side != pos.PosSide || !okxFillMatchesContracts(book.Quantity, pos.PosContracts) {
		mu.Unlock()
		okxProtectionIncident(store, s, mu, p, "the confirmed entry is not booked against the matching strategy position", notifier, logger)
		return
	}
	p.PositionID = ensurePositionTradeID(s.ID, p.Symbol, book)
	p.RequestJSON = string(raw)
	p.TriggerPx = final.Trigger
	p.SizeContracts = pos.PosContracts
	p.Side = pos.PosSide
	p.bump(okxProtectArming)
	err = saveOKXProtection(store, s)
	mu.Unlock()
	if err != nil {
		if logger != nil {
			logger.Error("CRITICAL: OKX stop request for %s was not saved, so it was not sent: %v", p.Symbol, err)
		}
		return
	}
	placeSavedOKXStop(store, s, mu, p, notifier, logger)
}

func placeSavedOKXStop(store *StateStore, s *StrategyState, mu *sync.RWMutex, p *okxProtection, notifier *MultiNotifier, logger *StrategyLogger) {
	mu.Lock()
	p.PlaceAttempts++
	p.bump(okxProtectArming)
	err := saveOKXProtection(store, s)
	mu.Unlock()
	if err != nil {
		okxProtectionIncident(store, s, mu, p, "the stop attempt could not be saved before sending: "+err.Error(), notifier, logger)
		return
	}
	placed, err := runOKXStop("place", okxStopFields(p.Symbol, map[string]string{
		"position-side": p.Side, "contracts": p.SizeContracts, "trigger-px": p.TriggerPx, "algo-client-id": p.AlgoClientID,
	}))
	if err != nil || placed.AlgoID == "" {
		okxProtectionIncident(store, s, mu, p, fmt.Sprintf("the stop place result was uncertain: %v", err), notifier, logger)
		return
	}
	mu.Lock()
	p.AlgoID = placed.AlgoID
	p.bump(okxProtectArmed)
	p.IncidentID = ""
	p.Alerted = false
	err = saveOKXProtection(store, s)
	mu.Unlock()
	if err != nil {
		okxProtectionIncident(store, s, mu, p, "the armed record could not be saved: "+err.Error(), notifier, logger)
	}
}

func okxGeometryFor(p *okxProtection, pos, ref okxStopCommandResult, fill *OKXFill) (okxStopGeometryInput, error) {
	var pol okxStopPolicy
	if err := json.Unmarshal([]byte(p.PolicyJSON), &pol); err != nil {
		return okxStopGeometryInput{}, fmt.Errorf("saved OKX stop policy is unreadable")
	}
	entry := ""
	atr := ""
	if fill != nil && fill.AvgPx > 0 {
		entry = okxDecimalField(fill.AvgPx)
	}
	var saved okxStopRequest
	_ = json.Unmarshal([]byte(p.RequestJSON), &saved)
	if entry == "" {
		entry = saved.Entry
	}
	if saved.EntryATR != "" {
		atr = saved.EntryATR
	}
	return okxStopGeometryInput{
		Side: pos.PosSide, Owner: pol.Owner, Entry: entry, Mark: ref.Mark, EntryATR: atr,
		HighWater: saved.HighWater, PreviousTrigger: p.TriggerPx, Pct: pol.Pct, ATRMult: pol.ATRMult,
		MinMovePct: pol.MinMovePct, Tick: ref.Tick, Equity: ref.Equity, Contracts: pos.PosContracts,
		CtVal: ref.CtVal, MMR: ref.MMR, Fee: ref.Fee, VenueLiq: pos.LiqPx, VenueLiqKnown: pos.LiqKnown,
		RequireVenueLiq: true,
	}, nil
}

func okxProtectionIncident(store *StateStore, s *StrategyState, mu *sync.RWMutex, p *okxProtection, reason string, notifier *MultiNotifier, logger *StrategyLogger) {
	if mu != nil {
		mu.Lock()
		defer mu.Unlock()
	}
	if p.IncidentID == "" {
		p.IncidentID = p.RecordID
	}
	p.EvidenceJSON = reason
	if !p.Alerted && notifier != nil {
		notifier.SendOwnerDM(fmt.Sprintf("**OKX STOP UNRESOLVED** [%s] %s stop %s: %s. New exposure is held. One alert is sent for this incident.", p.StrategyID, p.Symbol, p.AlgoClientID, reason))
		p.Alerted = true
	}
	if logger != nil {
		logger.Error("CRITICAL: OKX stop %s on %s is unresolved: %s", p.AlgoClientID, p.Symbol, reason)
	}
	p.bump(p.Status)
	if p.Status == okxProtectReserved {
		p.Status = okxProtectArming
	}
	_ = saveOKXProtection(store, s)
}

func okxEntryUnresolved(s *StrategyState, mu *sync.RWMutex) bool {
	mu.RLock()
	defer mu.RUnlock()
	return s.EntryGuard != nil && s.EntryGuard.unresolvedIntent() != nil
}

func reconcileOKXProtection(store *StateStore, sc StrategyConfig, s *StrategyState, mu *sync.RWMutex, notifier *MultiNotifier, logger *StrategyLogger) {
	if s == nil || !okxIsLive(sc.Args) || !okxStopWanted(sc) {
		return
	}
	p := s.OKXProtection
	if !p.unresolved() || okxEntryUnresolved(s, mu) {
		return
	}
	lock := okxInstrumentLock(p.AccountKey, p.InstID)
	lock.Lock()
	defer lock.Unlock()
	if p.AccountKey != okxEntryAccountKey("swap") {
		okxProtectionIncident(store, s, mu, p, "the current OKX credentials do not own the saved stop", notifier, logger)
		return
	}
	var saved okxStopRequest
	if err := json.Unmarshal([]byte(p.RequestJSON), &saved); err != nil && p.RequestJSON != "" {
		okxProtectionIncident(store, s, mu, p, "the saved OKX protection request is unreadable", notifier, logger)
		return
	}
	if saved.Close != nil && settleOKXRequestedClose(store, sc, s, mu, p, saved, notifier, logger) == okxSettlePending {
		return
	}
	releasedReduce := false
	if p.Status == okxProtectReducing {
		if !okxReduceUnprotected(store, sc, s, mu, p, notifier, logger) {
			return
		}
		releasedReduce = true
	}
	found, err := runOKXStop("read", okxStopFields(p.Symbol, map[string]string{"algo-client-id": p.AlgoClientID}))
	if err != nil {
		okxProtectionIncident(store, s, mu, p, "the saved stop could not be read: "+err.Error(), notifier, logger)
		return
	}
	switch found.AlgoState {
	case "live":
		if found.AlgoID == "" || (p.AlgoID != "" && found.AlgoID != p.AlgoID) ||
			!okxSameDecimal(found.TriggerPx, p.TriggerPx) && !okxTriggerTighter(p.Side, found.TriggerPx, p.TriggerPx) {
			okxProtectionIncident(store, s, mu, p, "the resting stop identity or trigger could not be verified", notifier, logger)
			return
		}
		if _, err := okxPositive(found.Sz, "resting size"); err != nil {
			okxProtectionIncident(store, s, mu, p, err.Error(), notifier, logger)
			return
		}
		if p.Status != okxProtectArmed || found.TriggerPx != p.TriggerPx || found.Sz != p.SizeContracts {
			mu.Lock()
			p.AlgoID = found.AlgoID
			p.TriggerPx = found.TriggerPx
			p.SizeContracts = found.Sz
			p.bump(okxProtectArmed)
			_ = saveOKXProtection(store, s)
			mu.Unlock()
		}
		okxTrailAndAmend(store, sc, s, mu, p, notifier, logger)
	case "absent":
		if p.Status == okxProtectTriggered || p.AlgoID != "" {
			okxProtectionIncident(store, s, mu, p, "a previously observed stop is missing from OKX", notifier, logger)
			return
		}
		if p.PlaceAttempts < 2 && p.RequestJSON != "" && p.Status == okxProtectArming {
			ref, err := runOKXStop("position", okxStopFields(p.Symbol, nil))
			if err == nil && !ref.Flat && ref.PosSide == p.Side && okxSameDecimal(ref.PosContracts, p.SizeContracts) {
				placeSavedOKXStop(store, s, mu, p, notifier, logger)
				return
			}
		}
		if releasedReduce {
			return
		}
		okxReduceUnprotected(store, sc, s, mu, p, notifier, logger)
	case "effective", "partially_effective":
		okxBookStopFills(store, sc, s, mu, p, found, notifier, logger)
	case "canceled", "order_failed":
		if len(found.OrderIDs) != 0 {
			okxBookStopFills(store, sc, s, mu, p, found, notifier, logger)
			return
		}
		if releasedReduce {
			return
		}
		okxReduceUnprotected(store, sc, s, mu, p, notifier, logger)
	default:
		okxProtectionIncident(store, s, mu, p, "the saved stop state "+found.AlgoState+" is not conclusive", notifier, logger)
	}
}

func okxSameDecimal(a, b string) bool {
	x, xErr := okxPositive(a, "value")
	y, yErr := okxPositive(b, "value")
	return xErr == nil && yErr == nil && x.Cmp(y) == 0
}

func okxTriggerTighter(side, current, previous string) bool {
	x, xErr := okxPositive(current, "trigger")
	y, yErr := okxPositive(previous, "previous trigger")
	return xErr == nil && yErr == nil && (side == "long" && x.Cmp(y) > 0 || side == "short" && x.Cmp(y) < 0)
}

const okxReduceLifetime = 2 * time.Minute

func okxReduceExpiry(now time.Time) int64 {
	return now.Add(okxReduceLifetime).UnixMilli()
}

func okxStoredRequest(saved okxStopRequest) string {
	if saved == (okxStopRequest{}) {
		return ""
	}
	raw, err := json.Marshal(saved)
	if err != nil {
		return ""
	}
	return string(raw)
}

func okxAbsentCloseHeld(expiresAtMs int64, now time.Time) string {
	if expiresAtMs <= 0 {
		return "absence is not proof the order was never accepted because the request has no saved expiry"
	}
	if now.UnixMilli() <= expiresAtMs {
		return fmt.Sprintf("OKX has no order for the request; it stays pending until the saved expiry %d", expiresAtMs)
	}
	return ""
}

func okxReduceUnprotected(store *StateStore, sc StrategyConfig, s *StrategyState, mu *sync.RWMutex, p *okxProtection, notifier *MultiNotifier, logger *StrategyLogger) bool {
	incident := func(why string) { okxProtectionIncident(store, s, mu, p, why, notifier, logger) }
	orderID := fmt.Sprintf("okr%x", sha256.Sum256([]byte(p.RecordID)))[:32]
	var saved okxStopRequest
	if p.RequestJSON != "" {
		if err := json.Unmarshal([]byte(p.RequestJSON), &saved); err != nil {
			incident("the saved OKX protection request is unreadable")
			return false
		}
	}
	var submitErr error
	if p.Status == okxProtectReducing && saved.Reduce != nil && saved.Reduce.ClientID != "" {
		orderID = saved.Reduce.ClientID
	}
	if p.Status != okxProtectReducing {
		pos, err := runOKXStop("position", okxStopFields(p.Symbol, nil))
		if err != nil {
			incident("owned exposure could not be verified before a reduce: " + err.Error())
			return false
		}
		mu.Lock()
		book := s.Positions[p.Symbol]
		if pos.Flat && (book == nil || book.Quantity == 0) {
			previous := p.Status
			p.bump(okxProtectRetired)
			err := saveOKXProtection(store, s)
			if err != nil {
				p.bump(previous)
			}
			mu.Unlock()
			if err != nil {
				incident("the flat stop record could not be retired: " + err.Error())
			}
			return false
		}
		if pos.Flat || book == nil || book.Side != pos.PosSide || (p.Side != "" && p.Side != book.Side) ||
			(p.PositionID != "" && p.PositionID != book.TradePositionID) ||
			!okxFillMatchesContracts(book.Quantity, pos.PosContracts) {
			mu.Unlock()
			incident("the position and book do not agree before a reduce")
			return false
		}
		qty, qtyErr := strconv.ParseFloat(pos.PosContracts, 64)
		if qtyErr != nil || !okxUsablePrice(qty) {
			mu.Unlock()
			incident("the emergency reduce has no verified venue size")
			return false
		}
		expiresAt := okxReduceExpiry(tradingNow())
		p.Side = book.Side
		saved.Reduce = &okxPendingClose{ClientID: orderID, Quantity: qty, ExpiresAtMs: expiresAt}
		p.RequestJSON = okxStoredRequest(saved)
		p.bump(okxProtectReducing)
		err = saveOKXProtection(store, s)
		mu.Unlock()
		if err != nil {
			incident("the emergency reduce could not be saved before sending: " + err.Error())
			return false
		}
		_, submitErr = runOKXStop("reduce", okxStopFields(p.Symbol, map[string]string{
			"position-side": p.Side, "contracts": pos.PosContracts, "order-id": orderID,
			"exp-time-ms": strconv.FormatInt(expiresAt, 10),
		}))
	}
	out, err := runOKXStop("reduce-fill", okxStopFields(p.Symbol, map[string]string{"order-id": orderID}))
	if err == nil && out.OrderAbsent {
		pending := saved.Reduce
		if pending == nil || pending.ClientID != orderID {
			incident(fmt.Sprintf("the emergency reduce %s is absent and its saved request cannot be matched", orderID))
			return false
		}
		if why := okxAbsentCloseHeld(pending.ExpiresAtMs, tradingNow()); why != "" {
			incident(fmt.Sprintf("the emergency reduce %s %s (submit: %v)", orderID, why, submitErr))
			return false
		}
		return releaseOKXEmergencyReduce(store, s, mu, p, saved, notifier, logger)
	}
	if err != nil || len(out.Fills) != 1 {
		incident(fmt.Sprintf("the emergency reduce has no confirmed fill for order %s (submit: %v, read: %v)", orderID, submitErr, err))
		return false
	}
	settleOKXProtectionFills(store, sc, s, mu, p, out.Fills, okxProtectReducing, true, notifier, logger, nil)
	return false
}

func releaseOKXEmergencyReduce(store *StateStore, s *StrategyState, mu *sync.RWMutex, p *okxProtection, saved okxStopRequest, notifier *MultiNotifier, logger *StrategyLogger) bool {
	mu.Lock()
	previous := *p
	saved.Reduce = nil
	p.RequestJSON = okxStoredRequest(saved)
	next := okxProtectArming
	if p.AlgoID != "" {
		next = okxProtectArmed
	}
	p.bump(next)
	err := saveOKXProtection(store, s)
	if err != nil {
		*p = previous
	}
	mu.Unlock()
	if err != nil {
		okxProtectionIncident(store, s, mu, p, "the expired emergency reduce could not be cleared: "+err.Error(), notifier, logger)
		return false
	}
	return true
}

func submitOKXProtectedClose(store *StateStore, sc StrategyConfig, s *StrategyState, mu *sync.RWMutex, size *float64, circuit bool, notifier *MultiNotifier, logger *StrategyLogger) (*OKXCloseResult, error) {
	p := s.OKXProtection
	if !p.unresolved() || p.AccountKey == "" || p.AccountKey != okxEntryAccountKey("swap") || okxEntryUnresolved(s, mu) {
		return nil, fmt.Errorf("OKX close has no verified protection owner")
	}
	lock := okxInstrumentLock(p.AccountKey, p.InstID)
	lock.Lock()
	defer lock.Unlock()
	var saved okxStopRequest
	if p.RequestJSON != "" {
		if err := json.Unmarshal([]byte(p.RequestJSON), &saved); err != nil {
			return nil, err
		}
	}
	if saved.Close != nil {
		switch settleOKXRequestedClose(store, sc, s, mu, p, saved, notifier, logger) {
		case okxSettlePending:
			return nil, fmt.Errorf("an earlier OKX close still needs venue evidence")
		case okxSettleBooked:
			return okxSettledCloseResult(s, mu, p, circuit)
		case okxSettleReleased:
			saved = okxStopRequest{}
			if p.RequestJSON != "" && json.Unmarshal([]byte(p.RequestJSON), &saved) != nil {
				return nil, fmt.Errorf("the saved OKX protection request is unreadable")
			}
		}
	}
	if p.Status == okxProtectReducing {
		if okxReduceUnprotected(store, sc, s, mu, p, notifier, logger) {
			saved = okxStopRequest{}
			if p.RequestJSON != "" && json.Unmarshal([]byte(p.RequestJSON), &saved) != nil {
				return nil, fmt.Errorf("the saved OKX protection request is unreadable")
			}
		} else if !p.unresolved() {
			return okxSettledCloseResult(s, mu, p, circuit)
		} else {
			return nil, fmt.Errorf("an earlier OKX close still needs venue evidence")
		}
	}
	pos, err := runOKXStop("position", okxStopFields(p.Symbol, nil))
	if err != nil {
		return nil, err
	}
	if pos.Flat {
		return &OKXCloseResult{Close: &OKXClose{Symbol: p.Symbol, AlreadyFlat: true}}, nil
	}
	venueQty, err := strconv.ParseFloat(pos.PosContracts, 64)
	if err != nil || !okxUsablePrice(venueQty) {
		return nil, fmt.Errorf("OKX close has no verified venue size")
	}
	qty, contracts := venueQty, pos.PosContracts
	if size != nil && !okxUsablePrice(*size) {
		return nil, fmt.Errorf("OKX close size is invalid")
	}
	if size != nil && *size < venueQty {
		lot := saved.Lot
		if lot == "" {
			ref, err := okxPaperReference(p.Symbol, "", false)
			if err != nil {
				return nil, err
			}
			lot = ref.Lot
		}
		var err error
		contracts, qty, err = okxRoundCloseSize(*size, lot)
		if err != nil {
			return nil, err
		}
	}
	mu.Lock()
	book := s.Positions[p.Symbol]
	if book == nil || book.Side != pos.PosSide || p.Side != "" && book.Side != p.Side ||
		p.PositionID != "" && p.PositionID != book.TradePositionID ||
		!okxUsablePrice(book.Quantity) || venueQty > book.Quantity && !okxFillQuantityEqual(venueQty, book.Quantity) {
		mu.Unlock()
		return nil, fmt.Errorf("OKX close could not verify the owned position and venue exposure")
	}
	previous := *p
	p.Side = book.Side
	p.PositionID = ensurePositionTradeID(s.ID, p.Symbol, book)
	id := fmt.Sprintf("okc%x", sha256.Sum256([]byte(fmt.Sprintf("%s:%d", p.RecordID, p.Version+1))))[:32]
	expiresAt := okxReduceExpiry(tradingNow())
	saved.Close = &okxPendingClose{ClientID: id, Quantity: qty, Circuit: circuit, ExpiresAtMs: expiresAt}
	p.RequestJSON = okxStoredRequest(saved)
	p.bump(p.Status)
	err = saveOKXProtection(store, s)
	if err != nil {
		*p = previous
	}
	mu.Unlock()
	if err != nil {
		return nil, err
	}
	_, submitErr := runOKXStop("reduce", okxStopFields(p.Symbol, map[string]string{
		"position-side": p.Side, "contracts": contracts, "order-id": id,
		"exp-time-ms": strconv.FormatInt(expiresAt, 10),
	}))
	switch settleOKXRequestedClose(store, sc, s, mu, p, saved, notifier, logger) {
	case okxSettleBooked:
		return okxSettledCloseResult(s, mu, p, circuit)
	default:
		return nil, fmt.Errorf("OKX close %s awaits venue evidence (submission: %v)", id, submitErr)
	}
}

func okxSettledCloseResult(s *StrategyState, mu *sync.RWMutex, p *okxProtection, circuit bool) (*OKXCloseResult, error) {
	mu.RLock()
	hasCircuitRemainder := circuit && s.RiskState.getPendingCircuitClose(PlatformPendingCloseOKX) != nil
	mu.RUnlock()
	if hasCircuitRemainder {
		return nil, fmt.Errorf("OKX circuit close has a remaining request")
	}
	return &OKXCloseResult{Close: &OKXClose{Symbol: p.Symbol}}, nil
}

func okxRoundCloseSize(size float64, lot string) (string, float64, error) {
	qty, err := okxPositive(strconv.FormatFloat(size, 'f', -1, 64), "close size")
	if err != nil {
		return "", 0, err
	}
	step, err := okxPositive(lot, "close lot size")
	if err != nil {
		return "", 0, err
	}
	steps := new(big.Rat).Quo(qty, step)
	count := new(big.Int).Quo(steps.Num(), steps.Denom())
	if count.Sign() == 0 {
		return "", 0, fmt.Errorf("OKX close size is below one contract lot")
	}
	rounded := new(big.Rat).Mul(new(big.Rat).SetInt(count), step)
	text, err := okxFormatRat(rounded, lot)
	value, _ := rounded.Float64()
	return text, value, err
}

type okxSettleKind int

const (
	okxSettlePending okxSettleKind = iota
	okxSettleBooked
	okxSettleReleased
)

func settleOKXRequestedClose(store *StateStore, sc StrategyConfig, s *StrategyState, mu *sync.RWMutex, p *okxProtection, saved okxStopRequest, notifier *MultiNotifier, logger *StrategyLogger) okxSettleKind {
	pending := saved.Close
	out, err := runOKXStop("reduce-fill", okxStopFields(p.Symbol, map[string]string{"order-id": pending.ClientID}))
	if err != nil {
		okxProtectionIncident(store, s, mu, p, fmt.Sprintf("OKX close %s awaits terminal venue evidence: %v", pending.ClientID, err), notifier, logger)
		return okxSettlePending
	}
	if out.OrderAbsent {
		if why := okxAbsentCloseHeld(pending.ExpiresAtMs, tradingNow()); why != "" {
			okxProtectionIncident(store, s, mu, p, fmt.Sprintf("OKX close %s %s", pending.ClientID, why), notifier, logger)
			return okxSettlePending
		}
		if !clearExpiredOKXClose(store, s, mu, p, saved, notifier, logger) {
			return okxSettlePending
		}
		return okxSettleReleased
	}
	if len(out.Fills) != 1 {
		okxProtectionIncident(store, s, mu, p, fmt.Sprintf("OKX close %s awaits terminal venue evidence: no unique order", pending.ClientID), notifier, logger)
		return okxSettlePending
	}
	fill := out.Fills[0]
	qty, err := strconv.ParseFloat(fill.Qty, 64)
	if err != nil || fill.Side != okxReduceSide(p.Side) || fill.OrdID == "" ||
		qty != 0 && (!okxUsablePrice(qty) || qty > pending.Quantity && !okxFillQuantityEqual(qty, pending.Quantity)) {
		okxProtectionIncident(store, s, mu, p, "OKX close evidence does not match the saved side or size", notifier, logger)
		return okxSettlePending
	}
	fills := out.Fills
	if qty == 0 {
		fills = nil
	}
	settleOKXProtectionFills(store, sc, s, mu, p, fills, p.Status, false, notifier, logger, &saved)
	var current okxStopRequest
	_ = json.Unmarshal([]byte(p.RequestJSON), &current)
	if current.Close != nil {
		return okxSettlePending
	}
	return okxSettleBooked
}

func clearExpiredOKXClose(store *StateStore, s *StrategyState, mu *sync.RWMutex, p *okxProtection, saved okxStopRequest, notifier *MultiNotifier, logger *StrategyLogger) bool {
	mu.Lock()
	previous := *p
	saved.Close = nil
	p.RequestJSON = okxStoredRequest(saved)
	p.bump(p.Status)
	err := saveOKXProtection(store, s)
	if err != nil {
		*p = previous
	}
	mu.Unlock()
	if err != nil {
		okxProtectionIncident(store, s, mu, p, "the expired OKX close could not be cleared: "+err.Error(), notifier, logger)
		return false
	}
	return true
}

func okxReduceSide(positionSide string) string {
	if positionSide == "long" {
		return "sell"
	}
	return "buy"
}

func okxBookStopFills(store *StateStore, sc StrategyConfig, s *StrategyState, mu *sync.RWMutex, p *okxProtection, found okxStopCommandResult, notifier *MultiNotifier, logger *StrategyLogger) {
	if len(found.OrderIDs) == 0 {
		okxProtectionIncident(store, s, mu, p, "the triggered stop has no child order IDs", notifier, logger)
		return
	}
	var fills []okxStopFill
	terminal := true
	for _, id := range found.OrderIDs {
		out, err := runOKXStop("fills", okxStopFields(p.Symbol, map[string]string{"order-id": id}))
		if err != nil {
			okxProtectionIncident(store, s, mu, p, "stop fills could not be read: "+err.Error(), notifier, logger)
			return
		}
		terminal = terminal && out.ChildTerminal
		for _, fill := range out.Fills {
			if fill.OrdID == id && fill.Side == okxReduceSide(p.Side) {
				fills = append(fills, fill)
			}
		}
	}
	remaining, settled := settleOKXProtectionFills(store, sc, s, mu, p, fills, okxProtectTriggered, terminal, notifier, logger, nil)
	if terminal && settled && remaining > 0 {
		okxRestoreRemainderStop(store, sc, s, mu, p, remaining, notifier, logger)
	}
}

func okxRestoreRemainderStop(store *StateStore, sc StrategyConfig, s *StrategyState, mu *sync.RWMutex, old *okxProtection, remaining float64, notifier *MultiNotifier, logger *StrategyLogger) {
	ref, err := runOKXStop("arm-reference", okxStopFields(old.Symbol, nil))
	if err != nil || ref.PosSide != old.Side || !okxFillMatchesContracts(remaining, ref.PosContracts) {
		okxProtectionIncident(store, s, mu, old, "the remaining exposure could not be verified for a replacement stop", notifier, logger)
		return
	}
	geom, err := okxGeometryFor(old, ref, ref, nil)
	if err != nil {
		okxProtectionIncident(store, s, mu, old, err.Error(), notifier, logger)
		return
	}
	final, err := okxFinalizeStop(geom)
	if err != nil {
		okxReduceUnprotected(store, sc, s, mu, old, notifier, logger)
		return
	}
	id := fmt.Sprintf("okp%x", sha256.Sum256([]byte(old.RecordID)))[:32]
	var saved okxStopRequest
	_ = json.Unmarshal([]byte(old.RequestJSON), &saved)
	saved.Mark, saved.HighWater, saved.Tick, saved.Lot = geom.Mark, final.HighWater, geom.Tick, ref.Lot
	saved.CtVal, saved.MMR, saved.Fee, saved.Equity = geom.CtVal, geom.MMR, geom.Fee, geom.Equity
	saved.VenueLiq, saved.VenueKnown = geom.VenueLiq, geom.VenueLiqKnown
	saved.Trigger, saved.Contracts = final.Trigger, ref.PosContracts
	raw, _ := json.Marshal(saved)
	now := tradingNow().UnixMilli()
	next := &okxProtection{
		RecordID: id, AlgoClientID: id, AccountKey: old.AccountKey, InstID: old.InstID, StrategyID: old.StrategyID,
		Symbol: old.Symbol, Side: old.Side, EntryClientID: old.EntryClientID, PositionID: old.PositionID,
		Status: okxProtectArming, Owner: old.Owner, PolicyJSON: old.PolicyJSON, TriggerPx: saved.Trigger,
		SizeContracts: saved.Contracts, RequestJSON: string(raw), Version: 1, CreatedAtMs: now, UpdatedAtMs: now,
		Replaces: old,
	}
	mu.Lock()
	old.bump(okxProtectRetired)
	s.OKXProtection = next
	if err := saveOKXProtection(store, s); err != nil {
		s.OKXProtection = old
		old.bump(okxProtectTriggered)
		mu.Unlock()
		okxProtectionIncident(store, s, mu, old, "the replacement stop could not be saved before sending: "+err.Error(), notifier, logger)
		return
	}
	mu.Unlock()
	placeSavedOKXStop(store, s, mu, next, notifier, logger)
}

func settleOKXProtectionFills(store *StateStore, sc StrategyConfig, s *StrategyState, mu *sync.RWMutex, p *okxProtection, fills []okxStopFill, status string, mayRetire bool, notifier *MultiNotifier, logger *StrategyLogger, requested *okxStopRequest) (float64, bool) {
	pos, posErr := runOKXStop("position", okxStopFields(p.Symbol, nil))
	incident := func(why string) { okxProtectionIncident(store, s, mu, p, why, notifier, logger) }
	mu.Lock()
	book := s.Positions[p.Symbol]
	remaining := 0.0
	if book != nil {
		remaining = book.Quantity
		if book.Side != p.Side || (p.PositionID != "" && p.PositionID != book.TradePositionID) {
			mu.Unlock()
			incident("the fill could not be attributed to the booked position side")
			return 0, false
		}
	}
	var legacyMark *Trade
	if book == nil && posErr == nil && pos.Flat && p.PositionID != "" {
		for i := len(s.TradeHistory) - 1; i >= 0; i-- {
			trade := &s.TradeHistory[i]
			if trade.PositionID == p.PositionID && trade.Symbol == p.Symbol && trade.IsClose &&
				trade.FeeSource == FeeSourceReconcileAdjustment && strings.HasPrefix(trade.Details, "Circuit breaker close ") {
				legacyMark = trade
				break
			}
		}
	}
	if legacyMark != nil {
		remaining = legacyMark.Quantity
	}
	seen := make(map[string]bool, len(fills)+len(p.PendingFills))
	for _, booked := range p.PendingFills {
		seen[booked.FillID] = true
	}
	var verified []okxProtectionFill
	for _, fill := range fills {
		if p.AppliedFills[fill.FillID] || seen[fill.FillID] {
			continue
		}
		qty, qErr := strconv.ParseFloat(fill.Qty, 64)
		px, pxErr := strconv.ParseFloat(fill.Px, 64)
		fee, feeErr := strconv.ParseFloat(fill.Fee, 64)
		validSize := qty <= remaining || okxFillQuantityEqual(qty, remaining)
		if fill.FillID == "" || fill.OrdID == "" || fill.Side != okxReduceSide(p.Side) ||
			qErr != nil || pxErr != nil || feeErr != nil || !okxUsablePrice(qty) || !okxUsablePrice(px) ||
			math.IsNaN(fee) || math.IsInf(fee, 0) || remaining <= 0 || !validSize {
			mu.Unlock()
			incident("a close fill lacked valid identity, side, price, fee or owned size")
			return 0, false
		}
		if okxFillQuantityEqual(qty, remaining) {
			qty = remaining
		}
		remaining = math.Max(0, remaining-qty)
		seen[fill.FillID] = true
		verified = append(verified, okxProtectionFill{FillID: fill.FillID, Side: fill.Side, Qty: qty, Px: px, Fee: math.Max(0, -fee), OrdID: fill.OrdID})
	}
	before, protectionBefore := *s, *p
	var bookBefore Position
	if book != nil {
		bookBefore = *book
	}
	restore := func() {
		*s, *p = before, protectionBefore
		if book != nil {
			*book = bookBefore
			s.Positions[p.Symbol] = book
		}
	}
	s.deferTradePersist = true
	if legacyMark != nil {
		p.PendingFills = append(p.PendingFills, verified...)
		verified = nil
		remaining = 0
	}
	for _, fill := range verified {
		book := s.Positions[p.Symbol]
		signal := -1
		if p.Side == "short" {
			signal = 1
		}
		book.StopLossTriggerPx, _ = strconv.ParseFloat(p.TriggerPx, 64)
		result, err := ExecutePerpsSignalWithLeverageDeferredOpen(s, signal, p.Symbol, fill.Px, PerpsSizingFor(sc, fill.Px, book.EntryATR), fill.Qty, fill.OrdID, fill.Fee, DirectionBoth, fill.Qty/book.Quantity, logger)
		if err != nil || result.TradesExecuted != 1 {
			restore()
			mu.Unlock()
			incident(fmt.Sprintf("the close fill could not be booked: %v", err))
			return 0, false
		}
		p.PendingFills = append(p.PendingFills, fill)
	}
	if requested != nil {
		saved := *requested
		if saved.Close.Circuit {
			if old := s.RiskState.getPendingCircuitClose(PlatformPendingCloseOKX); old != nil {
				left := saved.Close.Quantity
				for _, fill := range verified {
					left -= fill.Qty
				}
				s.RiskState.PendingCircuitCloses = maps.Clone(s.RiskState.PendingCircuitCloses)
				if (posErr == nil && pos.Flat) || left <= 0 || okxFillQuantityEqual(saved.Close.Quantity-left, saved.Close.Quantity) {
					s.RiskState.clearPendingCircuitClose(PlatformPendingCloseOKX)
				} else {
					next := *old
					next.Symbols = []PendingCircuitCloseSymbol{{Symbol: p.Symbol, Size: left}}
					s.RiskState.setPendingCircuitClose(PlatformPendingCloseOKX, &next)
				}
			}
		}
		saved.Close = nil
		raw, _ := json.Marshal(saved)
		p.RequestJSON = string(raw)
	}
	canRetire := mayRetire && posErr == nil && pos.Flat && remaining == 0 && (len(verified) > 0 || len(seen) > 0 || len(p.AppliedFills) > 0)
	if canRetire {
		p.bump(okxProtectRetired)
	} else {
		p.bump(status)
	}
	s.deferTradePersist = before.deferTradePersist
	err := saveOKXProtection(store, s)
	if err != nil {
		restore()
	}
	mu.Unlock()
	if err != nil {
		incident("proven close fills could not be saved: " + err.Error())
		return 0, false
	}
	matchedRemainder := posErr == nil && !pos.Flat && pos.PosSide == p.Side && okxFillMatchesContracts(remaining, pos.PosContracts)
	if canRetire && legacyMark != nil && len(fills) > 0 {
		msg := fmt.Sprintf("OKX stop %s: venue fill %s at %s followed an earlier model close at %g for %s. Venue evidence was saved once.", p.AlgoClientID, fills[0].FillID, fills[0].Px, legacyMark.Price, p.Symbol)
		if logger != nil {
			logger.Warn("%s", msg)
		}
		if notifier != nil {
			notifier.SendOwnerDM(msg)
		}
	}
	if requested == nil && !canRetire && !(mayRetire && status == okxProtectTriggered && matchedRemainder) {
		incident("the close has not proved both the exchange and strategy book flat; verify the remaining exposure")
	}
	return remaining, matchedRemainder
}

func okxFillQuantityEqual(a, b float64) bool {
	return okxUsablePrice(a) && okxUsablePrice(b) && math.Abs(a-b) <= 8*(math.Nextafter(b, math.Inf(1))-b)
}

func validateOKXStopStartup(state *AppState, cfgs []StrategyConfig) error {
	for _, sc := range cfgs {
		if !okxPerpsStrategy(sc) || !okxIsLive(sc.Args) || !okxStopWanted(sc) {
			continue
		}
		s := state.Strategies[sc.ID]
		if s == nil {
			return fmt.Errorf("strategy %s has no state", sc.ID)
		}
		p := s.OKXProtection
		if !p.unresolved() {
			continue
		}
		if p.AccountKey != okxEntryAccountKey(okxInstType(sc.Args)) || p.InstID != okxSwapRouteInstID(strings.ToUpper(okxSymbol(sc.Args))) {
			return fmt.Errorf("strategy %s has a saved OKX stop for a different account or instrument", sc.ID)
		}
		pol, _, err := okxStopPolicyOf(sc)
		if err != nil {
			return err
		}
		raw, _ := json.Marshal(pol)
		if p.PolicyJSON != string(raw) || p.Owner != pol.Owner {
			return fmt.Errorf("strategy %s changes an OKX stop while %s is unresolved", sc.ID, p.RecordID)
		}
	}
	return nil
}

func okxPaperReference(symbol, contracts string, opening bool) (okxStopCommandResult, error) {
	extra := map[string]string{}
	if contracts != "" {
		extra["contracts"] = contracts
		if opening {
			extra["round-entry"] = "true"
		}
	}
	res, err := runOKXStop("public-instrument", okxStopFields(symbol, extra))
	if err != nil {
		return res, err
	}
	res.Fee = okxPaperTakerFee
	return res, nil
}

func okxFillMatchesContracts(filled float64, contracts string) bool {
	got, err := okxPositive(strconv.FormatFloat(filled, 'f', -1, 64), "fill")
	want, wantErr := okxPositive(contracts, "contracts")
	if err != nil || wantErr != nil {
		return false
	}
	if got.Cmp(want) == 0 {
		return true
	}
	asFloat, _ := want.Float64()
	return okxUsablePrice(asFloat) && okxFillQuantityEqual(filled, asFloat)
}

func okxIntentATR(in *entryIntent) string {
	if in == nil || in.RequestJSON == "" {
		return ""
	}
	var req entryIntentRequest
	if json.Unmarshal([]byte(in.RequestJSON), &req) != nil || !(req.EntryATR > 0) {
		return ""
	}
	return strconv.FormatFloat(req.EntryATR, 'f', -1, 64)
}

func okxTrailAndAmend(store *StateStore, sc StrategyConfig, s *StrategyState, mu *sync.RWMutex, p *okxProtection, notifier *MultiNotifier, logger *StrategyLogger) {
	if p == nil || p.Status != okxProtectArmed {
		return
	}
	ref, err := runOKXStop("arm-reference", okxStopFields(p.Symbol, nil))
	if err != nil {
		okxProtectionIncident(store, s, mu, p, "the armed stop could not be checked against the position: "+err.Error(), notifier, logger)
		return
	}
	if ref.Flat || ref.PosSide != p.Side {
		okxProtectionIncident(store, s, mu, p, "the resting stop could not be matched to the open position", notifier, logger)
		return
	}
	geom, err := okxGeometryFor(p, ref, ref, nil)
	if err != nil {
		okxProtectionIncident(store, s, mu, p, err.Error(), notifier, logger)
		return
	}
	final, err := okxFinalizeStop(geom)
	if err != nil {
		okxProtectionIncident(store, s, mu, p, err.Error(), notifier, logger)
		return
	}
	var saved okxStopRequest
	_ = json.Unmarshal([]byte(p.RequestJSON), &saved)
	triggerChanged := final.Trigger != "" && final.Trigger != p.TriggerPx && final.Moved
	sizeChanged := ref.PosContracts != "" && !okxSameDecimal(ref.PosContracts, p.SizeContracts)
	if final.HighWater != "" && final.HighWater != saved.HighWater {
		saved.HighWater = final.HighWater
		raw, _ := json.Marshal(saved)
		mu.Lock()
		p.RequestJSON = string(raw)
		p.bump(p.Status)
		_ = saveOKXProtection(store, s)
		mu.Unlock()
	}
	if !triggerChanged && !sizeChanged {
		if p.IncidentID != "" {
			mu.Lock()
			p.IncidentID = ""
			p.Alerted = false
			p.bump(okxProtectArmed)
			_ = saveOKXProtection(store, s)
			mu.Unlock()
		}
		return
	}
	trigger := p.TriggerPx
	if triggerChanged {
		trigger = final.Trigger
	}
	size := p.SizeContracts
	if sizeChanged {
		size = ref.PosContracts
	}
	mu.Lock()
	p.bump(okxProtectAmending)
	saveErr := saveOKXProtection(store, s)
	mu.Unlock()
	if saveErr != nil {
		okxProtectionIncident(store, s, mu, p, "the stop amend was not saved, so it was not sent: "+saveErr.Error(), notifier, logger)
		return
	}
	if _, amendErr := runOKXStop("amend", okxStopFields(p.Symbol, map[string]string{
		"algo-id": p.AlgoID, "algo-client-id": p.AlgoClientID, "trigger-px": trigger, "contracts": size,
	})); amendErr != nil {
		okxProtectionIncident(store, s, mu, p, "the stop amend failed and the resting stop was left in place: "+amendErr.Error(), notifier, logger)
		return
	}
	mu.Lock()
	p.TriggerPx = trigger
	p.SizeContracts = size
	saved.Trigger = trigger
	saved.Contracts = size
	raw, _ := json.Marshal(saved)
	p.RequestJSON = string(raw)
	p.bump(okxProtectArmed)
	_ = saveOKXProtection(store, s)
	mu.Unlock()
}

func okxPaperRestingStop(sc StrategyConfig, s *StrategyState, mu *sync.RWMutex, symbol string, cash float64, logger *StrategyLogger) (bool, float64, error) {
	mu.RLock()
	pos := s.Positions[symbol]
	side, qty, trigger, avg, atr := "", 0.0, 0.0, 0.0, 0.0
	high := ""
	if pos != nil {
		side, qty, trigger, avg, atr = pos.Side, pos.Quantity, pos.StopLossTriggerPx, pos.AvgCost, pos.EntryATR
		if pos.StopLossHighWaterPx > 0 {
			high = strconv.FormatFloat(pos.StopLossHighWaterPx, 'f', -1, 64)
		}
	}
	mu.RUnlock()
	if qty <= 0 || trigger <= 0 {
		return false, 0, nil
	}
	ref, err := okxPaperReference(symbol, strconv.FormatFloat(qty, 'f', -1, 64), false)
	if err != nil {
		return false, 0, err
	}
	mark, err := okxPositive(ref.Mark, "mark")
	trig, trigErr := okxPositive(strconv.FormatFloat(trigger, 'f', -1, 64), "trigger")
	if err != nil || trigErr != nil {
		return false, 0, fmt.Errorf("paper stop mark is missing")
	}
	if (side == "long" && mark.Cmp(trig) <= 0) || (side == "short" && mark.Cmp(trig) >= 0) {
		fill := trigger
		if (side == "long" && mark.Cmp(trig) < 0) || (side == "short" && mark.Cmp(trig) > 0) {
			fill, _ = mark.Float64()
		}
		return true, paperStopBookPx(side, fill), nil
	}
	pol, _, err := okxStopPolicyOf(sc)
	if err != nil {
		return false, 0, err
	}
	geom := okxStopGeometryInput{
		Side: side, Owner: pol.Owner, Entry: strconv.FormatFloat(avg, 'f', -1, 64), Mark: ref.Mark,
		EntryATR: strconv.FormatFloat(atr, 'f', -1, 64), HighWater: high,
		PreviousTrigger: strconv.FormatFloat(trigger, 'f', -1, 64), Pct: pol.Pct, ATRMult: pol.ATRMult,
		MinMovePct: pol.MinMovePct, Tick: ref.Tick, Equity: strconv.FormatFloat(cash, 'f', -1, 64),
		Contracts: ref.Contracts, CtVal: ref.CtVal, MMR: ref.MMR, Fee: ref.Fee,
	}
	final, err := okxFinalizeStop(geom)
	if err != nil {
		return false, 0, err
	}
	mu.Lock()
	if cur := s.Positions[symbol]; cur != nil {
		if final.HighWater != "" {
			cur.StopLossHighWaterPx, _ = strconv.ParseFloat(final.HighWater, 64)
		}
		if final.Moved && final.Trigger != "" {
			cur.StopLossTriggerPx, _ = strconv.ParseFloat(final.Trigger, 64)
			if logger != nil {
				logger.Info("OKX paper stop moved to %s", final.Trigger)
			}
		}
	}
	mu.Unlock()
	return false, 0, nil
}

func okxPlanPaperOpen(sc StrategyConfig, symbol, side string, qty, price, atr, cash float64) (float64, okxStopGeometryResult, error) {
	ref, err := okxPaperReference(symbol, strconv.FormatFloat(qty, 'f', -1, 64), true)
	if err != nil {
		return 0, okxStopGeometryResult{}, err
	}
	pol, _, err := okxStopPolicyOf(sc)
	if err != nil {
		return 0, okxStopGeometryResult{}, err
	}
	qty, err = strconv.ParseFloat(ref.Contracts, 64)
	if err != nil || !okxUsablePrice(qty) {
		return 0, okxStopGeometryResult{}, fmt.Errorf("paper stop has no executable contract size")
	}
	final, err := okxFinalizeStop(okxStopGeometryInput{
		Side: side, Owner: pol.Owner, Entry: okxDecimalField(price), Mark: ref.Mark,
		EntryATR: okxDecimalField(atr), Pct: pol.Pct, ATRMult: pol.ATRMult, MinMovePct: pol.MinMovePct,
		Tick: ref.Tick, Equity: okxDecimalField(cash), Contracts: ref.Contracts, CtVal: ref.CtVal, MMR: ref.MMR, Fee: ref.Fee,
	})
	return qty, final, err
}

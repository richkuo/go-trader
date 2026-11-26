package main

import (
	"encoding/json"
	"fmt"
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

type okxStopRequest struct {
	Entry      string `json:"entry,omitempty"`
	Mark       string `json:"mark,omitempty"`
	EntryATR   string `json:"entry_atr,omitempty"`
	HighWater  string `json:"high_water,omitempty"`
	Tick       string `json:"tick,omitempty"`
	CtVal      string `json:"ct_val,omitempty"`
	MMR        string `json:"mmr,omitempty"`
	Fee        string `json:"fee,omitempty"`
	Equity     string `json:"equity,omitempty"`
	VenueLiq   string `json:"venue_liq,omitempty"`
	VenueKnown bool   `json:"venue_liq_known,omitempty"`
	Trigger    string `json:"trigger,omitempty"`
	Contracts  string `json:"contracts,omitempty"`
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
	return p.unresolved() && p.Status != okxProtectArmed
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
	sideEffect := op == "place" || op == "amend" || op == "cancel"
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
	if res.Contract != okxStopContract {
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

func okxPreflightAllowsEntry(symbol string) (okxStopCommandResult, error) {
	res, err := runOKXStop("preflight", okxStopFields(symbol, nil))
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
	if in == nil || fill == nil || s == nil || s.OKXProtection == nil || !okxStopWanted(sc) {
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
	if !okxFillMatchesContracts(fill.TotalSz, ref.PosContracts) {
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
	req := okxStopRequest{Entry: geom.Entry, Mark: geom.Mark, EntryATR: geom.EntryATR, HighWater: geom.HighWater, Tick: geom.Tick, CtVal: ref.CtVal, MMR: ref.MMR, Fee: ref.Fee, Equity: ref.Equity, VenueLiq: pos.LiqPx, VenueKnown: pos.LiqKnown, Trigger: "", Contracts: pos.PosContracts}
	final, err := okxFinalizeStop(geom)
	if err != nil {
		okxProtectionIncident(store, s, mu, p, err.Error(), notifier, logger)
		return
	}
	req.Trigger = final.Trigger
	raw, _ := json.Marshal(req)
	mu.Lock()
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
	mu.Lock()
	p.PlaceAttempts++
	err = saveOKXProtection(store, s)
	mu.Unlock()
	if err != nil {
		if logger != nil {
			logger.Error("CRITICAL: OKX stop attempt for %s was not saved, so it was not sent: %v", p.Symbol, err)
		}
		return
	}
	placed, placeErr := runOKXStop("place", okxStopFields(p.Symbol, map[string]string{
		"position-side":  p.Side,
		"contracts":      p.SizeContracts,
		"trigger-px":     p.TriggerPx,
		"algo-client-id": p.AlgoClientID,
	}))
	mu.Lock()
	defer mu.Unlock()
	if placeErr != nil {
		p.bump(okxProtectArming)
		_ = saveOKXProtection(store, s)
		okxProtectionIncident(store, s, nil, p, placeErr.Error(), notifier, logger)
		return
	}
	p.AlgoID = placed.AlgoID
	p.bump(okxProtectArmed)
	p.IncidentID = ""
	if err := saveOKXProtection(store, s); err != nil && logger != nil {
		logger.Error("CRITICAL: OKX stop %s was placed but the armed record was not saved: %v", p.AlgoClientID, err)
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
	if p.IncidentID == "" {
		p.IncidentID = p.RecordID
	}
	p.EvidenceJSON = reason
	if mu != nil {
		mu.Lock()
		defer mu.Unlock()
	}
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

func reconcileOKXProtection(store *StateStore, sc StrategyConfig, s *StrategyState, mu *sync.RWMutex, notifier *MultiNotifier, logger *StrategyLogger) {
	if s == nil || s.OKXProtection == nil || !s.OKXProtection.unresolved() || !okxIsLive(sc.Args) {
		return
	}
	p := s.OKXProtection
	lock := okxInstrumentLock(p.AccountKey, p.InstID)
	lock.Lock()
	defer lock.Unlock()
	found, err := runOKXStop("read", okxStopFields(p.Symbol, map[string]string{"algo-client-id": p.AlgoClientID}))
	if err != nil {
		okxProtectionIncident(store, s, mu, p, "the saved stop could not be read: "+err.Error(), notifier, logger)
		return
	}
	switch found.AlgoState {
	case "live":
		mu.Lock()
		p.AlgoID = found.AlgoID
		if p.Status == okxProtectArming || p.Status == okxProtectAmending || p.Status == okxProtectReserved {
			p.bump(okxProtectArmed)
			p.IncidentID = ""
			_ = saveOKXProtection(store, s)
		}
		mu.Unlock()
		if p.Status == okxProtectArmed {
			okxTrailAndAmend(store, sc, s, mu, p, notifier, logger)
		}
	case "absent":
		if p.Status == okxProtectCanceling {
			mu.Lock()
			p.bump(okxProtectRetired)
			_ = saveOKXProtection(store, s)
			mu.Unlock()
			return
		}
		if p.PlaceAttempts >= 2 || p.RequestJSON == "" || p.Status == okxProtectArmed {
			okxReduceUnprotected(store, sc, s, mu, p, notifier, logger)
			return
		}
		if p.Status == okxProtectArming || p.Status == okxProtectReserved {
			mu.Lock()
			p.PlaceAttempts++
			saveErr := saveOKXProtection(store, s)
			mu.Unlock()
			if saveErr != nil {
				okxProtectionIncident(store, s, mu, p, "the stop retry was not saved, so it was not sent: "+saveErr.Error(), notifier, logger)
				return
			}
			placed, placeErr := runOKXStop("place", okxStopFields(p.Symbol, map[string]string{
				"position-side": p.Side, "contracts": p.SizeContracts, "trigger-px": p.TriggerPx, "algo-client-id": p.AlgoClientID,
			}))
			if placeErr != nil {
				okxReduceUnprotected(store, sc, s, mu, p, notifier, logger)
				return
			}
			mu.Lock()
			p.AlgoID = placed.AlgoID
			p.bump(okxProtectArmed)
			p.IncidentID = ""
			_ = saveOKXProtection(store, s)
			mu.Unlock()
		}
	case "effective", "partially_effective":
		mu.Lock()
		p.bump(okxProtectTriggered)
		_ = saveOKXProtection(store, s)
		mu.Unlock()
		okxBookStopFills(store, sc, s, mu, p, notifier, logger)
	case "canceled":
		if p.Status == okxProtectCanceling {
			mu.Lock()
			p.bump(okxProtectRetired)
			_ = saveOKXProtection(store, s)
			mu.Unlock()
			return
		}
		okxProtectionIncident(store, s, mu, p, "the saved stop is canceled without a requested cancel", notifier, logger)
		okxReduceUnprotected(store, sc, s, mu, p, notifier, logger)
	case "order_failed":
		okxProtectionIncident(store, s, mu, p, "the saved stop is order_failed", notifier, logger)
		okxReduceUnprotected(store, sc, s, mu, p, notifier, logger)
	default:
		okxProtectionIncident(store, s, mu, p, "the saved stop state "+found.AlgoState+" is not conclusive", notifier, logger)
	}
}

func okxReduceUnprotected(store *StateStore, sc StrategyConfig, s *StrategyState, mu *sync.RWMutex, p *okxProtection, notifier *MultiNotifier, logger *StrategyLogger) {
	pos, err := runOKXStop("position", okxStopFields(p.Symbol, nil))
	if err != nil || (!pos.Flat && !pos.LiqKnown) {
		okxProtectionIncident(store, s, mu, p, "owned exposure could not be verified before a reduce", notifier, logger)
		return
	}
	if pos.Flat {
		mu.Lock()
		p.bump(okxProtectRetired)
		_ = saveOKXProtection(store, s)
		mu.Unlock()
		return
	}
	mu.Lock()
	p.bump(okxProtectReducing)
	_ = saveOKXProtection(store, s)
	mu.Unlock()
	_, _, _ = runPythonSideEffect(sc.Script, []string{"--execute", "--symbol=" + p.Symbol, "--side=" + okxReduceSide(pos.PosSide), "--size=" + pos.PosContracts, "--mode=live", "--inst-type=swap", "--reduce-only"})
	after, err := runOKXStop("position", okxStopFields(p.Symbol, nil))
	if err != nil || !after.Flat {
		okxProtectionIncident(store, s, mu, p, "the reduce-only close did not prove the position flat", notifier, logger)
		return
	}
	mu.Lock()
	p.bump(okxProtectRetired)
	_ = saveOKXProtection(store, s)
	mu.Unlock()
}

func okxReduceSide(positionSide string) string {
	if positionSide == "long" {
		return "sell"
	}
	return "buy"
}

func okxBookStopFills(store *StateStore, sc StrategyConfig, s *StrategyState, mu *sync.RWMutex, p *okxProtection, notifier *MultiNotifier, logger *StrategyLogger) {
	fills, err := runOKXStop("fills", okxStopFields(p.Symbol, nil))
	if err != nil {
		okxProtectionIncident(store, s, mu, p, "stop fills could not be read: "+err.Error(), notifier, logger)
		return
	}
	for _, fill := range fills.Fills {
		if fill.FillID == "" || p.AppliedFills[fill.FillID] {
			continue
		}
		px, err := okxPositive(fill.Px, "fill")
		if err != nil {
			okxProtectionIncident(store, s, mu, p, "stop fill "+fill.FillID+" has no price", notifier, logger)
			return
		}
		qty, err := okxPositive(fill.Qty, "fill qty")
		if err != nil {
			okxProtectionIncident(store, s, mu, p, "stop fill "+fill.FillID+" has no size", notifier, logger)
			return
		}
		pxText, err := okxFormatRat(px, okxFillTick(p))
		if err != nil {
			pxText = fill.Px
		}
		qtyText := okxBoundString(qty)
		booked := okxProtectionFill{FillID: fill.FillID, Side: fill.Side, OrdID: fill.OrdID}
		if pxf, perr := strconv.ParseFloat(pxText, 64); perr == nil {
			booked.Px = pxf
		}
		if qtyf, qerr := strconv.ParseFloat(qtyText, 64); qerr == nil {
			booked.Qty = qtyf
		}
		if fee, ferr := okxRat(orOKX(fill.Fee, "0"), "fill fee"); ferr == nil && fee.Sign() < 0 {
			booked.Fee, _ = new(big.Rat).Neg(fee).Float64()
		}
		mu.Lock()
		pos := s.Positions[p.Symbol]
		if pos != nil && pos.Quantity > 0 && booked.Qty > 0 && booked.Px > 0 {
			signal := -1
			if pos.Side == "short" {
				signal = 1
			}
			frac := 1.0
			if booked.Qty < pos.Quantity {
				frac = booked.Qty / pos.Quantity
			}
			pos.StopLossTriggerPx, _ = strconv.ParseFloat(p.TriggerPx, 64)
			if _, execErr := ExecutePerpsSignalWithLeverageDeferredOpen(s, signal, p.Symbol, booked.Px, PerpsSizingFor(sc, booked.Px, pos.EntryATR), booked.Qty, fill.OrdID, booked.Fee, EffectiveDirection(sc), frac, logger); execErr != nil {
				mu.Unlock()
				okxProtectionIncident(store, s, mu, p, "stop fill "+fill.FillID+" was not booked: "+execErr.Error(), notifier, logger)
				return
			}
		}
		p.PendingFills = append(p.PendingFills, booked)
		p.bump(p.Status)
		saveErr := saveOKXProtection(store, s)
		mu.Unlock()
		if saveErr != nil {
			okxProtectionIncident(store, s, mu, p, "proven stop fills were not saved: "+saveErr.Error(), notifier, logger)
			return
		}
	}
}

func cancelOKXProtection(store *StateStore, sc StrategyConfig, s *StrategyState, mu *sync.RWMutex, logger *StrategyLogger) error {
	if s == nil || s.OKXProtection == nil || !s.OKXProtection.unresolved() {
		return nil
	}
	p := s.OKXProtection
	lock := okxInstrumentLock(p.AccountKey, p.InstID)
	lock.Lock()
	defer lock.Unlock()
	mu.Lock()
	p.bump(okxProtectCanceling)
	err := saveOKXProtection(store, s)
	mu.Unlock()
	if err != nil {
		return err
	}
	if _, err := runOKXStop("cancel", okxStopFields(p.Symbol, map[string]string{"algo-id": p.AlgoID, "algo-client-id": p.AlgoClientID})); err != nil {
		return err
	}
	found, err := runOKXStop("read", okxStopFields(p.Symbol, map[string]string{"algo-client-id": p.AlgoClientID}))
	if err != nil {
		return err
	}
	if found.AlgoState == "absent" || found.AlgoState == "canceled" {
		mu.Lock()
		p.bump(okxProtectRetired)
		_ = saveOKXProtection(store, s)
		mu.Unlock()
		return nil
	}
	return fmt.Errorf("OKX stop %s is still %s after cancel", p.AlgoClientID, found.AlgoState)
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
		if p == nil {
			res, err := okxPreflightAllowsEntry(okxSymbol(sc.Args))
			if err != nil {
				return fmt.Errorf("strategy %s cannot activate an OKX stop until the account is flat, net-mode and cross with no conflicting orders: %v", sc.ID, err)
			}
			_ = res
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

func okxPaperReference(symbol, coinQty string) (okxStopCommandResult, error) {
	extra := map[string]string{}
	if coinQty != "" {
		extra["coin-qty"] = coinQty
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
	return got.Cmp(want) == 0
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

func okxFillTick(p *okxProtection) string {
	var saved okxStopRequest
	if p != nil {
		_ = json.Unmarshal([]byte(p.RequestJSON), &saved)
	}
	if saved.Tick != "" {
		return saved.Tick
	}
	return "0.00000001"
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
	if ref.Flat {
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
	sizeChanged := ref.PosContracts != "" && ref.PosContracts != p.SizeContracts
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
	ref, err := okxPaperReference(symbol, strconv.FormatFloat(qty, 'f', -1, 64))
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

func okxArmPaperPosition(sc StrategyConfig, s *StrategyState, mu *sync.RWMutex, symbol string, cash float64, logger *StrategyLogger) {
	mu.RLock()
	pos := s.Positions[symbol]
	side, qty, avg, atr, already := "", 0.0, 0.0, 0.0, 0.0
	if pos != nil {
		side, qty, avg, atr, already = pos.Side, pos.Quantity, pos.AvgCost, pos.EntryATR, pos.StopLossTriggerPx
	}
	mu.RUnlock()
	if qty <= 0 || already > 0 {
		return
	}
	ref, err := okxPaperReference(symbol, strconv.FormatFloat(qty, 'f', -1, 64))
	if err != nil {
		if logger != nil {
			logger.Error("CRITICAL: OKX paper stop was not armed: %v", err)
		}
		return
	}
	pol, _, err := okxStopPolicyOf(sc)
	if err != nil {
		return
	}
	geom := okxStopGeometryInput{
		Side: side, Owner: pol.Owner, Entry: strconv.FormatFloat(avg, 'f', -1, 64), Mark: ref.Mark,
		EntryATR: strconv.FormatFloat(atr, 'f', -1, 64), Pct: pol.Pct, ATRMult: pol.ATRMult,
		MinMovePct: pol.MinMovePct, Tick: ref.Tick, Equity: strconv.FormatFloat(cash, 'f', -1, 64),
		Contracts: ref.Contracts, CtVal: ref.CtVal, MMR: ref.MMR, Fee: ref.Fee,
	}
	final, err := okxFinalizeStop(geom)
	if err != nil {
		if logger != nil {
			logger.Error("CRITICAL: OKX paper stop was not armed: %v", err)
		}
		return
	}
	mu.Lock()
	if cur := s.Positions[symbol]; cur != nil && cur.StopLossTriggerPx == 0 {
		cur.StopLossTriggerPx, _ = strconv.ParseFloat(final.Trigger, 64)
		if final.HighWater != "" {
			cur.StopLossHighWaterPx, _ = strconv.ParseFloat(final.HighWater, 64)
		}
	}
	mu.Unlock()
	if logger != nil {
		logger.Info("OKX paper stop armed at %s", final.Trigger)
	}
}

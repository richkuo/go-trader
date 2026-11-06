package main

import (
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"reflect"
	"sort"
	"strings"
	"time"
)

const (
	operatorPaperCloseReason = "operator_paper_close"
	paperCloseCommandName    = "paper-close"
)

type paperCloseLeg struct {
	Kind       string  `json:"kind"`
	Key        string  `json:"key"`
	Side       string  `json:"side"`
	Quantity   float64 `json:"quantity"`
	AvgCost    float64 `json:"avg_cost"`
	PositionID string  `json:"position_id"`
	Hedge      bool    `json:"hedge,omitempty"`
}

type paperCloseKept struct {
	Key    string `json:"key"`
	Reason string `json:"reason"`
}

type paperClosePlan struct {
	StrategyID string           `json:"strategy_id"`
	Symbol     string           `json:"symbol,omitempty"`
	Partition  string           `json:"partition"`
	Legs       []paperCloseLeg  `json:"legs"`
	Kept       []paperCloseKept `json:"kept,omitempty"`
}

type paperCloseMark struct {
	Price  float64
	Source string
}

const (
	paperCloseLegPosition = "position"
	paperCloseLegOption   = "option"
)

func paperCloseRefusef(format string, args ...interface{}) error {
	return fmt.Errorf("paper-close refused, nothing changed: "+format, args...)
}

func (ss *StatusServer) paperCloseGlobalInterval() int {
	ss.strategiesMu.RLock()
	defer ss.strategiesMu.RUnlock()
	return ss.intervalSeconds
}

func (ss *StatusServer) paperCloseTarget(id string) (StrategyConfig, RiskPartition, error) {
	if ss == nil || ss.state == nil || ss.mu == nil {
		return StrategyConfig{}, RiskPartition{}, paperCloseRefusef("status server not ready")
	}
	sc, ok := ss.strategyConfig(id)
	if !ok {
		return StrategyConfig{}, RiskPartition{}, paperCloseRefusef("strategy %q not found in config", id)
	}
	if quoteOwnedStrategy(sc) {
		return StrategyConfig{}, RiskPartition{}, paperCloseRefusef("strategy %q is quote-owned; its inventory exits only through the %s lane of the admitted quote coordinator", id, inventoryQuoteExitCloseName)
	}
	if portfolioScopeFor(sc) != ScopePaper {
		return StrategyConfig{}, RiskPartition{}, paperCloseRefusef("strategy %q is live; paper-close books only paper positions. %s", id, removeStrategyCloseAdvice(sc, nil))
	}
	if ss.stateDB == nil {
		return StrategyConfig{}, RiskPartition{}, paperCloseRefusef("state store unavailable")
	}
	part, ok := ss.stateDB.partitionForStrategy(id)
	if !ok || part.IsLive() || part.Scope != ScopePaper {
		return StrategyConfig{}, RiskPartition{}, paperCloseRefusef("strategy %q has no paper storage partition (resolved %s)", id, part)
	}
	return sc, part, nil
}

func sortedPositionKeys(positions map[string]*Position) []string {
	keys := make([]string, 0, len(positions))
	for k := range positions {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func buildPaperClosePlanLocked(sc StrategyConfig, part RiskPartition, s *StrategyState, symbol string, clock optionHarvestClock) (*paperClosePlan, error) {
	if s == nil {
		return nil, paperCloseRefusef("strategy %q has no state in the daemon", sc.ID)
	}
	if quoteFundingOwned(sc, s) {
		return nil, paperCloseRefusef("strategy %q is quote-owned; its inventory exits only through the %s lane of the admitted quote coordinator", sc.ID, inventoryQuoteExitCloseName)
	}
	if s.robinhoodPartitionSavePending {
		return nil, paperCloseRefusef("strategy %q holds a shared Robinhood settlement that commits only with a full partition save; retry after the next cycle", sc.ID)
	}
	plan := &paperClosePlan{StrategyID: sc.ID, Symbol: symbol, Partition: part.String()}
	posKeys := sortedPositionKeys(s.Positions)
	found := false
	for _, key := range posKeys {
		pos := s.Positions[key]
		if pos == nil || (symbol != "" && key != symbol) {
			continue
		}
		found = true
		if !manualPositionOwnedByStrategy(pos, sc.ID) {
			return nil, paperCloseRefusef("position %s/%s is owned by %q, not %q", sc.ID, key, pos.OwnerStrategyID, sc.ID)
		}
		if symbol != "" {
			if pos.isHedgeLeg() {
				return nil, paperCloseRefusef("%s is a hedge leg coupled to %s; omit symbol to close the whole strategy", key, pos.HedgeFor)
			}
			for _, other := range posKeys {
				if op := s.Positions[other]; op != nil && other != key && op.HedgeFor == key {
					return nil, paperCloseRefusef("%s has a coupled hedge leg %s; omit symbol to close the whole strategy", key, other)
				}
			}
		}
		plan.Legs = append(plan.Legs, paperCloseLeg{
			Kind:       paperCloseLegPosition,
			Key:        key,
			Side:       pos.Side,
			Quantity:   pos.Quantity,
			AvgCost:    pos.AvgCost,
			PositionID: pos.TradePositionID,
			Hedge:      pos.isHedgeLeg(),
		})
	}
	var gated []string
	for _, key := range sortedOptionLegKeys(s.OptionPositions) {
		leg := s.OptionPositions[key]
		if leg == nil || (symbol != "" && key != symbol) {
			continue
		}
		found = true
		if optionLegAwaitingSettlement(s.Platform, leg, clock.Now) {
			plan.Kept = append(plan.Kept, paperCloseKept{Key: key, Reason: "at or past its expiry cutoff; settlement books it"})
			continue
		}
		if _, reason := optionHarvestEligibility(leg, clock); reason != "" {
			gated = append(gated, fmt.Sprintf("%s: %s", key, reason))
			continue
		}
		if math.IsNaN(leg.CurrentValueUSD) || math.IsInf(leg.CurrentValueUSD, 0) {
			gated = append(gated, fmt.Sprintf("%s: the stored mark is not a finite number", key))
			continue
		}
		plan.Legs = append(plan.Legs, paperCloseLeg{
			Kind:       paperCloseLegOption,
			Key:        key,
			Side:       leg.Action,
			Quantity:   leg.Quantity,
			AvgCost:    leg.EntryPremiumUSD,
			PositionID: leg.TradePositionID,
		})
	}
	if len(gated) > 0 {
		return nil, paperCloseRefusef("option leg mark fails the fresh-mark gate: %s", strings.Join(gated, "; "))
	}
	if len(plan.Legs) == 0 {
		switch {
		case len(plan.Kept) > 0:
			keys := make([]string, 0, len(plan.Kept))
			for _, k := range plan.Kept {
				keys = append(keys, k.Key)
			}
			return nil, paperCloseRefusef("nothing to close: every matching option leg is at or past its expiry cutoff and waits for settlement (%s)", strings.Join(keys, ", "))
		case symbol != "" && !found:
			return nil, paperCloseRefusef("strategy %q holds no open position or option leg %q", sc.ID, symbol)
		default:
			return nil, paperCloseRefusef("strategy %q holds no open position or option leg", sc.ID)
		}
	}
	return plan, nil
}

func paperCloseMarkFailure(key string, m paperCloseMark, ok bool) string {
	if !ok {
		return fmt.Sprintf("%s: no mark", key)
	}
	if math.IsNaN(m.Price) || math.IsInf(m.Price, 0) || m.Price <= 0 {
		return fmt.Sprintf("%s: mark %v from %s is not a finite price above zero", key, m.Price, m.Source)
	}
	return ""
}

func resolvePaperCloseMarks(sc StrategyConfig, plan *paperClosePlan) (map[string]paperCloseMark, error) {
	var hl, okxPerps, okxSpot, robinhood, spot, futures, unsupported []string
	for _, leg := range plan.Legs {
		if leg.Kind != paperCloseLegPosition {
			continue
		}
		switch {
		case leg.Hedge, sc.Platform == "hyperliquid" && (sc.Type == "perps" || sc.Type == "manual"):
			hl = append(hl, leg.Key)
		case sc.Platform == "okx" && sc.Type == "perps":
			okxPerps = append(okxPerps, leg.Key)
		case sc.Platform == "okx" && sc.Type == "spot":
			okxSpot = append(okxSpot, leg.Key)
		case sc.Platform == "robinhood" && sc.Type == "spot":
			robinhood = append(robinhood, leg.Key)
		case sc.Type == "spot" && strings.Contains(leg.Key, "/"):
			spot = append(spot, leg.Key)
		case sc.Type == "futures":
			futures = append(futures, leg.Key)
		default:
			unsupported = append(unsupported, leg.Key)
		}
	}
	if len(unsupported) > 0 {
		sort.Strings(unsupported)
		return nil, paperCloseRefusef("no daemon mark source for platform=%s type=%s (%s)", sc.Platform, sc.Type, strings.Join(unsupported, ", "))
	}
	marks := make(map[string]paperCloseMark)
	var problems []string
	failed := make(map[string]bool)
	put := func(keys []string, fetched map[string]float64, err error, source string) {
		if err != nil {
			problems = append(problems, fmt.Sprintf("%s: %s fetch failed: %v", strings.Join(keys, ", "), source, err))
			for _, k := range keys {
				failed[k] = true
			}
			return
		}
		for _, k := range keys {
			if px, ok := fetched[k]; ok {
				marks[k] = paperCloseMark{Price: px, Source: source}
			}
		}
	}
	if len(hl) > 0 {
		m, err := fetchHyperliquidMids(hl)
		put(hl, m, err, "hyperliquid allMids")
	}
	if len(okxPerps) > 0 {
		m, err := fetchOKXPerpsMids(okxPerps)
		put(okxPerps, m, err, "okx swap ticker")
	}
	if len(okxSpot) > 0 {
		m, insts, err := fetchOKXSpotMids(okxSpot)
		if err != nil {
			put(okxSpot, nil, err, "okx spot ticker")
		} else {
			for _, k := range okxSpot {
				if px, ok := m[k]; ok {
					marks[k] = paperCloseMark{Price: px, Source: "okx spot ticker " + insts[k]}
				}
			}
		}
	}
	for _, coin := range robinhood {
		source := "binanceus " + coin + "/USD ticker"
		px, err := defaultRobinhoodReleaseMark(coin)
		if err != nil {
			problems = append(problems, fmt.Sprintf("%s: %s fetch failed: %v", coin, source, err))
			failed[coin] = true
			continue
		}
		marks[coin] = paperCloseMark{Price: px, Source: source}
	}
	if len(spot) > 0 {
		m, err := FetchPrices(spot)
		put(spot, m, err, "spot ticker")
	}
	if len(futures) > 0 {
		m, mode, err := FetchFuturesMarks(futures)
		if mode == "" {
			mode = "default"
		}
		put(futures, m, err, "futures marks mode="+mode)
	}
	for _, leg := range plan.Legs {
		if leg.Kind != paperCloseLegPosition {
			continue
		}
		if failed[leg.Key] {
			continue
		}
		m, ok := marks[leg.Key]
		if reason := paperCloseMarkFailure(leg.Key, m, ok); reason != "" {
			problems = append(problems, reason)
		}
	}
	if len(problems) > 0 {
		sort.Strings(problems)
		return nil, paperCloseRefusef("no usable mark: %s", strings.Join(problems, "; "))
	}
	return marks, nil
}

func (ss *StatusServer) fetchPaperCloseMarks(sc StrategyConfig, plan *paperClosePlan) (map[string]paperCloseMark, error) {
	fn := ss.paperCloseMarks
	if fn == nil {
		fn = resolvePaperCloseMarks
	}
	marks, err := fn(sc, plan)
	if err != nil {
		return nil, err
	}
	for _, leg := range plan.Legs {
		if leg.Kind != paperCloseLegPosition {
			continue
		}
		m, ok := marks[leg.Key]
		if reason := paperCloseMarkFailure(leg.Key, m, ok); reason != "" {
			return nil, paperCloseRefusef("no usable mark: %s", reason)
		}
	}
	return marks, nil
}

func (ss *StatusServer) snapshotPaperClosePlan(sc StrategyConfig, part RiskPartition, symbol string) (*paperClosePlan, error) {
	clock := newOptionHarvestClock(sc, ss.paperCloseGlobalInterval(), tradingNow())
	ss.mu.RLock()
	defer ss.mu.RUnlock()
	return buildPaperClosePlanLocked(sc, part, ss.state.Strategies[sc.ID], symbol, clock)
}

func (ss *StatusServer) preparePaperClose(id, symbol string) (*paperClosePlan, map[string]paperCloseMark, error) {
	id = strings.TrimSpace(id)
	symbol = strings.TrimSpace(symbol)
	if id == "" {
		return nil, nil, paperCloseRefusef("strategy id is required")
	}
	if isDraining() {
		return nil, nil, paperCloseRefusef("the daemon is shutting down")
	}
	sc, part, err := ss.paperCloseTarget(id)
	if err != nil {
		return nil, nil, err
	}
	plan, err := ss.snapshotPaperClosePlan(sc, part, symbol)
	if err != nil {
		return nil, nil, err
	}
	marks, err := ss.fetchPaperCloseMarks(sc, plan)
	if err != nil {
		return nil, nil, err
	}
	return plan, marks, nil
}

func paperClosePlansMatch(a, b *paperClosePlan) bool {
	if a == nil || b == nil {
		return false
	}
	return a.StrategyID == b.StrategyID && a.Symbol == b.Symbol && a.Partition == b.Partition &&
		reflect.DeepEqual(a.Legs, b.Legs) && reflect.DeepEqual(a.Kept, b.Kept)
}

const paperCloseChangedRefusal = "the book or its option-leg state changed while you were confirming; run paper-close again"

type paperCloseBooked struct {
	Leg    paperCloseLeg
	Price  float64
	Source string
	PnL    float64
}

func (ss *StatusServer) applyPaperClose(operator string, pinned *paperClosePlan) (string, error) {
	if pinned == nil {
		return "", paperCloseRefusef("no confirmed plan")
	}
	if isDraining() {
		return "", paperCloseRefusef("the daemon is shutting down")
	}
	id := pinned.StrategyID
	sc, part, err := ss.paperCloseTarget(id)
	if err != nil {
		return "", err
	}
	if part.String() != pinned.Partition {
		return "", paperCloseRefusef("%s", paperCloseChangedRefusal)
	}
	release, err := ss.stateDB.manualActionLock(id)
	if err != nil {
		return "", paperCloseRefusef("could not take the manual-action lock for %s: %v", id, err)
	}
	defer release()

	current, err := ss.snapshotPaperClosePlan(sc, part, pinned.Symbol)
	if err != nil {
		return "", err
	}
	if !paperClosePlansMatch(current, pinned) {
		return "", paperCloseRefusef("%s", paperCloseChangedRefusal)
	}
	for _, leg := range pinned.Legs {
		if leg.Kind != paperCloseLegPosition {
			continue
		}
		if err := refuseIfPendingManualPositionAction(ss.stateDB, paperCloseCommandName, id, leg.Key); err != nil {
			return "", paperCloseRefusef("%v", err)
		}
		if err := refuseIfRestingLimitOrderQueued(ss.stateDB, paperCloseCommandName, id, leg.Key); err != nil {
			return "", paperCloseRefusef("%v", err)
		}
	}
	marks, err := ss.fetchPaperCloseMarks(sc, pinned)
	if err != nil {
		return "", err
	}

	clock := newOptionHarvestClock(sc, ss.paperCloseGlobalInterval(), tradingNow())
	ss.mu.Lock()
	s := ss.state.Strategies[id]
	locked, err := buildPaperClosePlanLocked(sc, part, s, pinned.Symbol, clock)
	if err != nil {
		ss.mu.Unlock()
		return "", err
	}
	if !paperClosePlansMatch(locked, pinned) {
		ss.mu.Unlock()
		return "", paperCloseRefusef("%s", paperCloseChangedRefusal)
	}
	cashBefore := s.Cash
	lossesBefore := s.RiskState.ConsecutiveLosses
	prevDefer := s.deferTradePersist
	s.deferTradePersist = true
	var booked []paperCloseBooked
	for _, leg := range pinned.Legs {
		if leg.Kind != paperCloseLegPosition {
			continue
		}
		m := marks[leg.Key]
		prefix := fmt.Sprintf("Operator paper close (%s) by %s, mark %s", operatorPaperCloseReason, operator, m.Source)
		closeVirtualPositionAtPrice(s, &sc, leg.Key, s.Positions[leg.Key], m.Price, operatorPaperCloseReason, prefix, nil)
		booked = append(booked, paperCloseBooked{Leg: leg, Price: m.Price, Source: m.Source, PnL: lastTradePnL(s, leg.Key)})
	}
	now := tradingNow().UTC()
	for _, leg := range pinned.Legs {
		if leg.Kind != paperCloseLegOption {
			continue
		}
		pos := s.OptionPositions[leg.Key]
		source := pos.MarkSource
		if source == "" {
			source = "stored option mark"
		}
		prefix := fmt.Sprintf("Operator paper close (%s) by %s, mark %s", operatorPaperCloseReason, operator, source)
		closeOptionLegAtValue(s, leg.Key, pos, operatorPaperCloseReason, prefix, now, nil)
		price := 0.0
		if n := len(s.TradeHistory); n > 0 {
			price = s.TradeHistory[n-1].Price
		}
		booked = append(booked, paperCloseBooked{Leg: leg, Price: price, Source: source, PnL: lastTradePnL(s, leg.Key)})
	}
	s.deferTradePersist = prevDefer
	saveErr := ss.stateDB.SaveStrategyBook(s)
	cashAfter := s.Cash
	dailyPnL := s.RiskState.DailyPnL
	lossesAfter := s.RiskState.ConsecutiveLosses
	ss.mu.Unlock()

	for _, b := range booked {
		fmt.Printf("[paper-close] strategy=%s partition=%s %s=%s side=%s qty=%.10g mark=%.10g source=%q realized_pnl=%.4f operator=%s\n",
			id, pinned.Partition, b.Leg.Kind, b.Leg.Key, b.Leg.Side, b.Leg.Quantity, b.Price, b.Source, b.PnL, operator)
	}
	return paperCloseReply(sc, pinned, booked, cashBefore, cashAfter, dailyPnL, lossesBefore, lossesAfter, saveErr), nil
}

func lastTradePnL(s *StrategyState, symbol string) float64 {
	if n := len(s.TradeHistory); n > 0 && s.TradeHistory[n-1].Symbol == symbol {
		return s.TradeHistory[n-1].RealizedPnL
	}
	return 0
}

func paperCloseKeptLines(plan *paperClosePlan) []string {
	var lines []string
	for _, k := range plan.Kept {
		lines = append(lines, fmt.Sprintf("- kept %s: %s", k.Key, k.Reason))
	}
	return lines
}

func paperClosePrompt(plan *paperClosePlan, marks map[string]paperCloseMark) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Close %d paper leg(s) of `%s` (partition %s) at the current mark?", len(plan.Legs), plan.StrategyID, plan.Partition)
	for _, leg := range plan.Legs {
		if leg.Kind == paperCloseLegPosition {
			m := marks[leg.Key]
			fmt.Fprintf(&b, "\n- %s %s %.8g @ avg %.8g, indicative mark %.8g (%s)", leg.Key, leg.Side, leg.Quantity, leg.AvgCost, m.Price, m.Source)
			continue
		}
		fmt.Fprintf(&b, "\n- option %s %s %.8g, booked at its fresh stored mark", leg.Key, leg.Side, leg.Quantity)
	}
	for _, line := range paperCloseKeptLines(plan) {
		b.WriteString("\n" + line)
	}
	fmt.Fprintf(&b, "\nIt books one close trade per leg with reason `%s`, sends no order, and needs no restart. Marks are fetched again after you confirm.", operatorPaperCloseReason)
	return b.String()
}

func paperCloseReply(sc StrategyConfig, plan *paperClosePlan, booked []paperCloseBooked, cashBefore, cashAfter, dailyPnL float64, lossesBefore, lossesAfter int, saveErr error) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Paper-closed %d leg(s) of `%s` (partition %s):", len(booked), plan.StrategyID, plan.Partition)
	for _, bk := range booked {
		fmt.Fprintf(&b, "\n- %s %s %.8g @ %.8g (%s), realized PnL $%.2f", bk.Leg.Key, bk.Leg.Side, bk.Leg.Quantity, bk.Price, bk.Source, bk.PnL)
	}
	for _, line := range paperCloseKeptLines(plan) {
		b.WriteString("\n" + line)
	}
	fmt.Fprintf(&b, "\nCash $%.2f -> $%.2f. DailyPnL $%.2f. ConsecutiveLosses %d -> %d.", cashBefore, cashAfter, dailyPnL, lossesBefore, lossesAfter)
	if lossesAfter > lossesBefore {
		b.WriteString(" The next risk check can fire this strategy's circuit breaker.")
	}
	if !sc.Paused {
		fmt.Fprintf(&b, "\nThe strategy is not paused, so its next signal can open a new position. Pause it first (dashboard pause toggle, or `config set strategies.%s.paused true`) if you intend to remove it.", sc.ID)
	}
	if strings.TrimSpace(sc.ReplaySourceID) != "" {
		fmt.Fprintf(&b, "\nThis paper mirror now differs from its live source %s until that source completes its next round trip.", sc.ReplaySourceID)
	}
	if saveErr != nil {
		fmt.Fprintf(&b, "\nBooked in memory; the state file save failed (%v); the next cycle save retries.", saveErr)
	}
	return b.String()
}

func paperCloseSymbolParam(params json.RawMessage) (string, error) {
	if len(params) == 0 || string(params) == "null" {
		return "", nil
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(params, &obj); err != nil {
		return "", fmt.Errorf("params must be a JSON object")
	}
	for k := range obj {
		if k != "symbol" {
			return "", fmt.Errorf("unknown paper-close param %q (want symbol)", k)
		}
	}
	raw, ok := obj["symbol"]
	if !ok {
		return "", nil
	}
	var sym string
	if err := json.Unmarshal(raw, &sym); err != nil {
		return "", fmt.Errorf("symbol must be a string")
	}
	return strings.TrimSpace(sym), nil
}

func (ss *StatusServer) confirmPaperClose(w http.ResponseWriter, req uiConfirmRequest) {
	if strings.TrimSpace(req.StrategyID) == "" {
		writeJSONError(w, http.StatusBadRequest, "strategy_id is required")
		return
	}
	symbol, err := paperCloseSymbolParam(req.Params)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	plan, marks, err := ss.preparePaperClose(req.StrategyID, symbol)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	payload, err := json.Marshal(plan)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "could not pin the close plan")
		return
	}
	binding, err := canonicalConfirmBinding(paperCloseCommandName, req.StrategyID, req.Params)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	nonce, err := ss.issueConfirmNonce(binding, string(payload), time.Now())
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "could not issue confirmation")
		return
	}
	writeJSON(w, uiConfirmResponse{
		Nonce:            nonce,
		ExpiresInSeconds: int(confirmNonceTTL / time.Second),
		ConfirmPhrase:    plan.StrategyID,
		Description:      paperClosePrompt(plan, marks),
	})
}

func (ss *StatusServer) handleAPIStrategyPaperClose(w http.ResponseWriter, r *http.Request, id string) {
	if !ss.uiTradeActionGuards(w, r) {
		return
	}
	obj, ok := readUIMutationBody(w, r)
	if !ok {
		return
	}
	var nonce string
	if raw, present := obj["nonce"]; present {
		_ = json.Unmarshal(raw, &nonce)
	}
	params := obj["params"]
	if nonce == "" {
		writeJSONError(w, http.StatusBadRequest, "nonce is required — call POST /api/confirm first")
		return
	}
	if _, err := paperCloseSymbolParam(params); err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	binding, err := canonicalConfirmBinding(paperCloseCommandName, id, params)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	payload, err := ss.consumeConfirmNonce(nonce, binding, time.Now())
	if err != nil {
		writeJSONError(w, http.StatusForbidden, err.Error())
		return
	}
	var plan paperClosePlan
	if err := json.Unmarshal([]byte(payload), &plan); err != nil || plan.StrategyID != id {
		writeJSONError(w, http.StatusForbidden, "confirmation does not carry a close plan for this strategy — request a new confirmation")
		return
	}
	ss.tradeActionMu.Lock()
	defer ss.tradeActionMu.Unlock()
	msg, err := ss.applyPaperClose("dashboard", &plan)
	if err != nil {
		writeJSONError(w, http.StatusConflict, err.Error())
		return
	}
	writeJSON(w, uiMutationResponse{OK: true, Message: msg})
}

package main

import (
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// hedgeMinOrderNotionalUSD is deliberately below the exchange's documented
// minimum. It is a local fail-closed guard: an undersized hedge open/add is
// rejected and its corresponding primary exposure is unwound rather than left
// naked.
const hedgeMinOrderNotionalUSD = 10.0

const hedgeQtyEpsilon = 1e-9

type hedgeSnapshot struct {
	PrimaryQty  float64
	PrimarySide string
	PrimaryCost float64

	HedgeQty   float64
	HedgeSide  string
	HedgeCost  float64
	HedgeBasis float64
}

type hedgeActionKind string

const (
	hedgeActionNone      hedgeActionKind = "none"
	hedgeActionOpen      hedgeActionKind = "open"
	hedgeActionAdd       hedgeActionKind = "add"
	hedgeActionReduce    hedgeActionKind = "reduce"
	hedgeActionCloseFull hedgeActionKind = "close_full"
)

type hedgeAction struct {
	Kind   hedgeActionKind
	Qty    float64
	Side   string // hedge position side, never the order side
	Reason string
}

// independentAlphaTradeCount deliberately excludes hedge execution legs from
// operator-facing alpha trade counts. Hedge trades remain in TradeHistory and
// the ledger so portfolio cash, realized PnL, fees, and daily return remain
// complete; they simply cannot look like their own strategy decision.
func independentAlphaTradeCount(trades []Trade) int {
	count := 0
	for _, trade := range trades {
		if trade.TradeType != "hedge" {
			count++
		}
	}
	return count
}

// persistedHedgeSymbolsForPrimary returns the sole source of truth for the
// hedge legs coupled to primary. Config can change across a restart, but a
// persisted HedgeFor record must continue to drive protective lifecycle work
// until the pair is flat. The output is sorted because callers use it to
// submit/log multi-leg close work.
func persistedHedgeSymbolsForPrimary(s *StrategyState, primary string) []string {
	if s == nil || primary == "" {
		return nil
	}
	var symbols []string
	for symbol, pos := range s.Positions {
		if pos != nil && pos.HedgeFor == primary && pos.Quantity > hedgeQtyEpsilon {
			symbols = append(symbols, symbol)
		}
	}
	sort.Strings(symbols)
	return symbols
}

func inverseHedgeSide(primarySide string) string {
	switch primarySide {
	case "long":
		return "short"
	case "short":
		return "long"
	default:
		return ""
	}
}

// hedgeTargetDecision is the pure hedge state machine. It keys all new hedge
// exposure off a change in primary quantity against the persisted watermark,
// not a change in either mark. That makes the result stable across ordinary
// mark drift and deterministic after restart/reconcile.
func hedgeTargetDecision(sc StrategyConfig, snap hedgeSnapshot, primaryPx, hedgePx float64) hedgeAction {
	if !HedgeEnabled(sc) {
		return hedgeAction{Kind: hedgeActionNone}
	}
	if snap.PrimaryQty <= hedgeQtyEpsilon {
		if snap.HedgeQty > hedgeQtyEpsilon {
			return hedgeAction{Kind: hedgeActionCloseFull, Qty: snap.HedgeQty, Side: snap.HedgeSide, Reason: "primary is flat"}
		}
		return hedgeAction{Kind: hedgeActionNone}
	}

	wantSide := inverseHedgeSide(snap.PrimarySide)
	if wantSide == "" {
		return hedgeAction{Kind: hedgeActionNone, Reason: fmt.Sprintf("invalid primary side %q", snap.PrimarySide)}
	}
	if snap.HedgeQty > hedgeQtyEpsilon && snap.HedgeSide != wantSide {
		// Never reverse a hedge in one order. Phase 1 rejects bidirectional
		// primary strategies; this guard contains corrupt/reconciled state.
		return hedgeAction{Kind: hedgeActionCloseFull, Qty: snap.HedgeQty, Side: snap.HedgeSide, Reason: "hedge side conflicts with primary"}
	}
	if snap.HedgeQty <= hedgeQtyEpsilon {
		qty, reason := hedgeQtyForPrimaryDelta(snap.PrimaryQty, primaryPx, hedgePx, HedgeRatio(sc))
		if reason != "" {
			return hedgeAction{Kind: hedgeActionNone, Reason: reason}
		}
		return hedgeAction{Kind: hedgeActionOpen, Qty: qty, Side: wantSide, Reason: "primary opened"}
	}
	if snap.HedgeBasis <= hedgeQtyEpsilon {
		return hedgeAction{Kind: hedgeActionNone, Reason: "hedge is missing primary quantity watermark"}
	}
	if snap.PrimaryQty > snap.HedgeBasis+hedgeQtyEpsilon {
		qty, reason := hedgeQtyForPrimaryDelta(snap.PrimaryQty-snap.HedgeBasis, primaryPx, hedgePx, HedgeRatio(sc))
		if reason != "" {
			return hedgeAction{Kind: hedgeActionNone, Reason: reason}
		}
		return hedgeAction{Kind: hedgeActionAdd, Qty: qty, Side: wantSide, Reason: "primary increased"}
	}
	if snap.PrimaryQty < snap.HedgeBasis-hedgeQtyEpsilon {
		// Reduce by the same fraction of the hedge, not by a fresh notional
		// calculation. That preserves the original hedge ratio despite later
		// mark drift and cannot increase exposure.
		qty := snap.HedgeQty * (snap.HedgeBasis - snap.PrimaryQty) / snap.HedgeBasis
		if qty > snap.HedgeQty {
			qty = snap.HedgeQty
		}
		if qty <= hedgeQtyEpsilon {
			return hedgeAction{Kind: hedgeActionNone}
		}
		return hedgeAction{Kind: hedgeActionReduce, Qty: qty, Side: snap.HedgeSide, Reason: "primary reduced"}
	}
	return hedgeAction{Kind: hedgeActionNone}
}

func hedgeQtyForPrimaryDelta(primaryQty, primaryPx, hedgePx, ratio float64) (float64, string) {
	if primaryQty <= hedgeQtyEpsilon {
		return 0, "primary quantity is not positive"
	}
	if primaryPx <= 0 || hedgePx <= 0 {
		return 0, "primary or hedge mark is unavailable"
	}
	if ratio <= 0 {
		return 0, "hedge ratio is not positive"
	}
	qty := primaryQty * primaryPx * ratio / hedgePx
	if math.IsNaN(qty) || math.IsInf(qty, 0) || qty <= hedgeQtyEpsilon {
		return 0, "hedge quantity is invalid"
	}
	return qty, ""
}

// hedgeOrderSkipReason repeats the state-machine preconditions immediately
// before a live subprocess is spawned. The caller compares its snapshot to
// current state as well; this helper keeps a malformed action from emitting an
// on-chain fill that cannot be recorded.
func hedgeOrderSkipReason(action hedgeAction, snap hedgeSnapshot) string {
	switch action.Kind {
	case hedgeActionOpen:
		if snap.PrimaryQty <= hedgeQtyEpsilon || snap.HedgeQty > hedgeQtyEpsilon {
			return "primary/hedge state changed before hedge open"
		}
	case hedgeActionAdd:
		if snap.PrimaryQty <= snap.HedgeBasis+hedgeQtyEpsilon || snap.HedgeQty <= hedgeQtyEpsilon {
			return "primary/hedge state changed before hedge add"
		}
	case hedgeActionReduce:
		if snap.PrimaryQty >= snap.HedgeBasis-hedgeQtyEpsilon || snap.HedgeQty <= hedgeQtyEpsilon {
			return "primary/hedge state changed before hedge reduction"
		}
	case hedgeActionCloseFull:
		if snap.HedgeQty <= hedgeQtyEpsilon {
			return "hedge is already flat"
		}
	default:
		return "no hedge action"
	}
	if action.Qty <= hedgeQtyEpsilon || action.Side == "" {
		return "hedge action has invalid quantity or side"
	}
	return ""
}

// validateHedgeStateConsistency catches config edits followed by a process
// restart, which bypass the SIGHUP state-compatibility gate. It is deliberately
// non-destructive: the persisted leg remains visible for reconciliation, but
// hedge sync will not place a new order until the operator restores a matching
// configuration or flattens the pair.
func validateHedgeStateConsistency(state *AppState, cfg *Config) []string {
	if state == nil || cfg == nil {
		return nil
	}
	byID := strategyConfigByID(cfg.Strategies)
	var warnings []string
	ids := make([]string, 0, len(state.Strategies))
	for id := range state.Strategies {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		ss := state.Strategies[id]
		if ss == nil {
			continue
		}
		sc, configured := byID[id]
		coins := make([]string, 0)
		for coin, pos := range ss.Positions {
			if pos != nil && pos.HedgeFor != "" && pos.Quantity > 0 {
				coins = append(coins, coin)
			}
		}
		sort.Strings(coins)
		for _, coin := range coins {
			pos := ss.Positions[coin]
			switch {
			case !configured || !HedgeEnabled(sc):
				warnings = append(warnings, fmt.Sprintf("hedge state mismatch: strategy %s holds hedge %s for %s but no enabled matching hedge config exists; leaving the leg frozen", id, coin, pos.HedgeFor))
			case pos.HedgeFor != hyperliquidConfiguredCoin(sc):
				warnings = append(warnings, fmt.Sprintf("hedge state mismatch: strategy %s hedge %s belongs to primary %s, config primary is %s; leaving the leg frozen", id, coin, pos.HedgeFor, hyperliquidConfiguredCoin(sc)))
			case coin != hedgeCoin(sc):
				warnings = append(warnings, fmt.Sprintf("hedge state mismatch: strategy %s holds hedge %s but config declares %s; leaving the leg frozen", id, coin, hedgeCoin(sc)))
			}
		}
	}
	return warnings
}

type hedgeSyncResult struct {
	Trades  int
	Detail  string
	Changed bool
}

// runHedgeCoherenceSweep is the post-reconcile safety pass. It runs even for
// strategies that were not due for an entry check, so an externally closed
// primary, a restart, a kill-switch residual, or a reconcile-driven quantity
// change cannot leave a persisted hedge leg orphaned until the next signal.
// It is intentionally state-derived: no check-script output can create a
// hedge order here.
func runHedgeCoherenceSweep(state *AppState, strategies []StrategyConfig, prices map[string]float64, hlPositions []HLPosition, mu *sync.RWMutex, notifier *MultiNotifier, logMgr *LogManager) {
	if state == nil || mu == nil {
		return
	}
	ordered := make([]StrategyConfig, 0)
	for _, sc := range strategies {
		if sc.Type == "perps" && sc.Platform == "hyperliquid" && HedgeEnabled(sc) {
			ordered = append(ordered, sc)
		}
	}
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].ID < ordered[j].ID })
	for _, sc := range ordered {
		primary, hedge := hyperliquidConfiguredCoin(sc), hedgeCoin(sc)
		if primary == "" || hedge == "" {
			continue
		}
		mu.RLock()
		ss := state.Strategies[sc.ID]
		hasLeg := ss != nil && (ss.Positions[primary] != nil || ss.Positions[hedge] != nil)
		mu.RUnlock()
		if !hasLeg {
			continue
		}
		var logger *StrategyLogger
		if logMgr != nil {
			logger, _ = logMgr.GetStrategyLogger(sc.ID)
		}
		result := runHedgeSync(sc, ss, primary, prices[primary], prices[hedge], hyperliquidIsLive(sc.Args), hlExecuteSnapshotForCoin(hlPositions, hedge), mu, notifier, logger, false)
		if result.Detail != "" {
			fmt.Println(result.Detail)
		}
	}
}

// runHedgeSync converges one persisted primary/hedge pair. It follows the
// scheduler's lock discipline: snapshot under RLock, submit outside the lock,
// then mutate virtual state only from a confirmed fill under Lock. The same
// state machine serves paper and live modes.
func runHedgeSync(
	sc StrategyConfig,
	s *StrategyState,
	primarySymbol string,
	primaryPx, hedgePx float64,
	live bool,
	walletSnapshot hlExecuteSnapshot,
	mu *sync.RWMutex,
	notifier *MultiNotifier,
	logger *StrategyLogger,
	freshPrimaryOpen bool,
) hedgeSyncResult {
	if !HedgeEnabled(sc) || s == nil || mu == nil {
		return hedgeSyncResult{}
	}
	coin := hedgeCoin(sc)
	if coin == "" || primarySymbol == "" {
		return hedgeSyncResult{}
	}

	mu.RLock()
	primary := s.Positions[primarySymbol]
	hedge := s.Positions[coin]
	snap := hedgeSnapshot{}
	hedgeFor := ""
	var conflictingHedgeCoins []string
	foreignExpectedHedge := false
	if primary != nil && primary.HedgeFor == "" {
		snap.PrimaryQty = primary.Quantity
		snap.PrimarySide = primary.Side
		snap.PrimaryCost = primary.AvgCost
	}
	if hedge != nil {
		snap.HedgeQty = hedge.Quantity
		snap.HedgeSide = hedge.Side
		snap.HedgeCost = hedge.AvgCost
		snap.HedgeBasis = hedge.HedgePrimaryQtyBasis
		hedgeFor = hedge.HedgeFor
	}
	for symbol, pos := range s.Positions {
		if pos == nil || pos.Quantity <= hedgeQtyEpsilon {
			continue
		}
		if pos.HedgeFor == primarySymbol && symbol != coin {
			conflictingHedgeCoins = append(conflictingHedgeCoins, symbol)
		}
		if symbol == coin && pos.HedgeFor != primarySymbol {
			foreignExpectedHedge = true
		}
	}
	mu.RUnlock()

	// A restart can bypass the SIGHUP flat-only guard. Never replace or
	// duplicate a persisted hedge after a config edit: its explicit ownership
	// is more authoritative than the new config until an operator flattens the
	// pair or restores the matching block.
	if len(conflictingHedgeCoins) > 0 {
		sort.Strings(conflictingHedgeCoins)
		hedgeSyncCritical(notifier, logger, sc.ID, fmt.Sprintf("refusing hedge sync for %s: persisted hedge(s) %s belong to this primary but config declares %s; leaving the pair frozen", primarySymbol, strings.Join(conflictingHedgeCoins, ", "), coin))
		return hedgeSyncResult{}
	}
	if foreignExpectedHedge {
		hedgeSyncCritical(notifier, logger, sc.ID, fmt.Sprintf("refusing hedge sync for %s: virtual %s position is not a persisted hedge for this primary", primarySymbol, coin))
		return hedgeSyncResult{}
	}
	if hedgeFor != "" && hedgeFor != primarySymbol {
		hedgeSyncCritical(notifier, logger, sc.ID, fmt.Sprintf("refusing hedge sync on %s: persisted hedge belongs to %s, not %s", coin, hedgeFor, primarySymbol))
		return hedgeSyncResult{}
	}
	action := hedgeTargetDecision(sc, snap, primaryPx, hedgePx)
	if action.Kind == hedgeActionNone {
		if action.Reason != "" && snap.PrimaryQty > hedgeQtyEpsilon && snap.HedgeQty <= hedgeQtyEpsilon {
			msg := fmt.Sprintf("cannot open hedge %s: %s", coin, action.Reason)
			hedgeSyncCritical(notifier, logger, sc.ID, msg)
			return unwindPrimaryAfterHedgeFailure(sc, s, primarySymbol, snap.PrimaryQty, primaryPx, live, mu, notifier, logger, freshPrimaryOpen, msg)
		}
		return hedgeSyncResult{}
	}
	if reason := hedgeOrderSkipReason(action, snap); reason != "" {
		hedgeSyncCritical(notifier, logger, sc.ID, fmt.Sprintf("hedge %s %s skipped: %s", action.Kind, coin, reason))
		return hedgeSyncResult{}
	}
	if (action.Kind == hedgeActionOpen || action.Kind == hedgeActionAdd) && action.Qty*hedgePx < hedgeMinOrderNotionalUSD {
		msg := fmt.Sprintf("hedge %s %s notional $%.2f is below the $%.2f safety floor", action.Kind, coin, action.Qty*hedgePx, hedgeMinOrderNotionalUSD)
		hedgeSyncCritical(notifier, logger, sc.ID, msg)
		if action.Kind == hedgeActionOpen || action.Kind == hedgeActionAdd {
			return unwindPrimaryAfterHedgeFailure(sc, s, primarySymbol, hedgePrimaryUnwindQty(action, snap), primaryPx, live, mu, notifier, logger, freshPrimaryOpen, msg)
		}
		return hedgeSyncResult{}
	}

	if live {
		return runLiveHedgeAction(sc, s, primarySymbol, coin, action, snap, walletSnapshot, mu, notifier, logger, freshPrimaryOpen)
	}
	return applyPaperHedgeAction(sc, s, primarySymbol, coin, action, snap, hedgePx, mu, notifier, logger, freshPrimaryOpen)
}

// hedgePrimaryUnwindQty returns only the unhedged primary increment for an
// add failure. An opening hedge has no prior protected basis, so it closes the
// whole primary. This is what makes a failed scale-in hedge fail closed
// without liquidating the pre-existing paired exposure.
func hedgePrimaryUnwindQty(action hedgeAction, snap hedgeSnapshot) float64 {
	if action.Kind == hedgeActionAdd {
		return math.Max(0, snap.PrimaryQty-snap.HedgeBasis)
	}
	return snap.PrimaryQty
}

func hedgeSyncCritical(notifier *MultiNotifier, logger *StrategyLogger, strategyID, msg string) {
	if logger != nil {
		logger.Error("CRITICAL hedge: %s", msg)
	} else {
		fmt.Printf("[CRITICAL] hedge[%s]: %s\n", strategyID, msg)
	}
	if notifier != nil && notifier.HasBackends() {
		alert := fmt.Sprintf("**HEDGE CRITICAL** [%s] %s", strategyID, msg)
		notifier.SendOwnerDM(alert)
		notifier.SendToAllChannels(alert)
	}
}

func runLiveHedgeAction(sc StrategyConfig, s *StrategyState, primarySymbol, coin string, action hedgeAction, snap hedgeSnapshot, walletSnapshot hlExecuteSnapshot, mu *sync.RWMutex, notifier *MultiNotifier, logger *StrategyLogger, freshPrimaryOpen bool) hedgeSyncResult {
	// Re-check the exact state used to size the order immediately before the
	// subprocess. A signal, reconciliation, or manual action can update the
	// pair between runHedgeSync's initial snapshot and this point.
	mu.RLock()
	current := hedgeSnapshot{}
	if primary := s.Positions[primarySymbol]; primary != nil && primary.HedgeFor == "" {
		current.PrimaryQty, current.PrimarySide, current.PrimaryCost = primary.Quantity, primary.Side, primary.AvgCost
	}
	if hedge := s.Positions[coin]; hedge != nil {
		current.HedgeQty, current.HedgeSide, current.HedgeCost, current.HedgeBasis = hedge.Quantity, hedge.Side, hedge.AvgCost, hedge.HedgePrimaryQtyBasis
	}
	mu.RUnlock()
	if current != snap || hedgeOrderSkipReason(action, current) != "" {
		reason := hedgeOrderSkipReason(action, current)
		if reason == "" {
			reason = "primary or hedge state changed after sizing"
		}
		hedgeSyncCritical(notifier, logger, sc.ID, fmt.Sprintf("hedge %s %s skipped: %s", action.Kind, coin, reason))
		return hedgeSyncResult{}
	}

	var fillPx, fillQty, fee float64
	var oid int64
	var err error
	switch action.Kind {
	case hedgeActionOpen, hedgeActionAdd:
		orderSide := "buy"
		if action.Side == "short" {
			orderSide = "sell"
		}
		marginMode, leverage := "", 0.0
		if action.Kind == hedgeActionOpen {
			marginMode, leverage = hedgeMarginMode(sc), hedgeLeverage(sc)
		}
		result, stderr, runErr := RunHyperliquidExecute(sc.Script, coin, orderSide, action.Qty, 0, 0, 0, marginMode, leverage, false, walletSnapshot)
		if stderr != "" && logger != nil {
			logger.Info("hedge execute stderr: %s", stderr)
		}
		if runErr != nil {
			err = runErr
		} else if result == nil || result.Error != "" {
			if result != nil {
				err = fmt.Errorf("%s", result.Error)
			} else {
				err = fmt.Errorf("empty hedge execute result")
			}
		} else if result.Execution == nil || result.Execution.Fill == nil || result.Execution.Fill.AvgPx <= 0 || result.Execution.Fill.TotalSz <= hedgeQtyEpsilon {
			err = fmt.Errorf("hedge execute lacked a confirmed fill")
		} else {
			fill := result.Execution.Fill
			fillPx, fillQty, fee, oid = fill.AvgPx, fill.TotalSz, fill.Fee, fill.OID
		}
	case hedgeActionReduce, hedgeActionCloseFull:
		qty := action.Qty
		result, stderr, runErr := RunHyperliquidClose(sc.Script, coin, &qty, nil)
		if stderr != "" && logger != nil {
			logger.Info("hedge close stderr: %s", stderr)
		}
		if runErr != nil {
			err = runErr
		} else if result == nil || result.Error != "" {
			if result != nil {
				err = fmt.Errorf("%s", result.Error)
			} else {
				err = fmt.Errorf("empty hedge close result")
			}
		} else if result.Close == nil || result.Close.Fill == nil || result.Close.Fill.AvgPx <= 0 || result.Close.Fill.TotalSz <= hedgeQtyEpsilon {
			err = fmt.Errorf("hedge close lacked a confirmed fill")
		} else {
			fill := result.Close.Fill
			fillPx, fillQty, fee, oid = fill.AvgPx, fill.TotalSz, fill.Fee, fill.OID
		}
	}
	if err != nil {
		msg := fmt.Sprintf("%s %s failed: %v", action.Kind, coin, err)
		hedgeSyncCritical(notifier, logger, sc.ID, msg)
		if action.Kind == hedgeActionOpen || action.Kind == hedgeActionAdd {
			return unwindPrimaryAfterHedgeFailure(sc, s, primarySymbol, hedgePrimaryUnwindQty(action, snap), 0, true, mu, notifier, logger, freshPrimaryOpen, msg)
		}
		return hedgeSyncResult{}
	}
	clearLiveExecThrottle(sc, directionOpen, coin)
	applied := applyHedgeFill(sc, s, primarySymbol, coin, action, snap, fillPx, fillQty, fee, oid, true, mu, logger)
	// A partial opening fill still creates real hedge exposure, but it leaves a
	// portion of the just-opened/add primary uncovered. Book the confirmed hedge
	// slice first, then immediately unwind only that uncovered primary slice.
	// Treating a partial as ordinary retry would knowingly carry naked alpha.
	if (action.Kind == hedgeActionOpen || action.Kind == hedgeActionAdd) && fillQty < action.Qty*0.99 {
		uncovered := hedgePrimaryUnwindQty(action, snap) * math.Max(0, 1-fillQty/action.Qty)
		msg := fmt.Sprintf("%s %s partially filled %.6f/%.6f; unwinding uncovered primary %.6f", action.Kind, coin, fillQty, action.Qty, uncovered)
		hedgeSyncCritical(notifier, logger, sc.ID, msg)
		unwind := unwindPrimaryAfterHedgeFailure(sc, s, primarySymbol, uncovered, 0, true, mu, notifier, logger, freshPrimaryOpen, msg)
		applied.Trades += unwind.Trades
		applied.Changed = applied.Changed || unwind.Changed
		if unwind.Detail != "" {
			applied.Detail = unwind.Detail
		}
	}
	return applied
}

func applyPaperHedgeAction(sc StrategyConfig, s *StrategyState, primarySymbol, coin string, action hedgeAction, snap hedgeSnapshot, hedgePx float64, mu *sync.RWMutex, notifier *MultiNotifier, logger *StrategyLogger, freshPrimaryOpen bool) hedgeSyncResult {
	if hedgePx <= 0 {
		if (action.Kind == hedgeActionReduce || action.Kind == hedgeActionCloseFull) && snap.HedgeCost > 0 {
			hedgePx = snap.HedgeCost
		} else {
			msg := fmt.Sprintf("%s %s failed: hedge mark is unavailable", action.Kind, coin)
			hedgeSyncCritical(notifier, logger, sc.ID, msg)
			if action.Kind == hedgeActionOpen || action.Kind == hedgeActionAdd {
				return unwindPrimaryAfterHedgeFailure(sc, s, primarySymbol, hedgePrimaryUnwindQty(action, snap), 0, false, mu, notifier, logger, freshPrimaryOpen, msg)
			}
			return hedgeSyncResult{}
		}
	}
	return applyHedgeFill(sc, s, primarySymbol, coin, action, snap, hedgePx, action.Qty, 0, 0, false, mu, logger)
}

// applyHedgeFill owns all virtual hedge mutations. Its callers have a real
// exchange fill (or a deterministic paper fill); failed subprocesses never
// enter here.
func applyHedgeFill(sc StrategyConfig, s *StrategyState, primarySymbol, coin string, action hedgeAction, snap hedgeSnapshot, fillPx, fillQty, fillFee float64, oid int64, useFillFee bool, mu *sync.RWMutex, logger *StrategyLogger) hedgeSyncResult {
	if fillPx <= 0 || fillQty <= hedgeQtyEpsilon {
		return hedgeSyncResult{}
	}
	mu.Lock()
	defer mu.Unlock()
	primary := s.Positions[primarySymbol]
	var oidStr string
	if oid > 0 {
		oidStr = strconv.FormatInt(oid, 10)
	}
	switch action.Kind {
	case hedgeActionOpen, hedgeActionAdd:
		pos := s.Positions[coin]
		if pos != nil && pos.HedgeFor != "" && pos.HedgeFor != primarySymbol {
			// We must never merge an on-chain fill into an ambiguously-owned
			// virtual leg. Leave the fill visible through the emergency alert
			// path; a later reconcile will not adopt foreign state either.
			if logger != nil {
				logger.Error("hedge fill for %s cannot be applied: persisted leg belongs to %s", coin, pos.HedgeFor)
			}
			return hedgeSyncResult{}
		}
		fee := fillFee
		feeSource := FeeSourceUserFills
		if !useFillFee {
			fee = CalculatePlatformSpotFee(s.Platform, fillQty*fillPx)
			feeSource = FeeSourceModeled
		}
		if pos == nil {
			positionID := ""
			if primary != nil {
				positionID = ensurePositionTradeID(s.ID, primarySymbol, primary)
			}
			if positionID == "" {
				positionID = newTradePositionID(s.ID, primarySymbol, time.Now().UTC())
			}
			pos = &Position{
				Symbol:               coin,
				TradePositionID:      positionID,
				Quantity:             fillQty,
				InitialQuantity:      fillQty,
				AvgCost:              fillPx,
				Side:                 action.Side,
				Multiplier:           1,
				Leverage:             hedgeLeverage(sc),
				OwnerStrategyID:      s.ID,
				HedgeFor:             primarySymbol,
				HedgePrimaryQtyBasis: hedgeBasisAfterFill(action, snap, fillQty),
				OpenedAt:             time.Now().UTC(),
			}
			s.Positions[coin] = pos
		} else {
			if pos.Side != action.Side {
				if logger != nil {
					logger.Error("hedge fill for %s has side %s but virtual leg is %s; refusing blend", coin, action.Side, pos.Side)
				}
				return hedgeSyncResult{}
			}
			oldQty := pos.Quantity
			pos.Quantity += fillQty
			pos.InitialQuantity += fillQty
			pos.AvgCost = (pos.AvgCost*oldQty + fillPx*fillQty) / pos.Quantity
			pos.HedgePrimaryQtyBasis = hedgeBasisAfterFill(action, snap, fillQty)
		}
		s.Cash -= fee
		orderSide := "buy"
		if action.Side == "short" {
			orderSide = "sell"
		}
		RecordTrade(s, Trade{
			Timestamp:       time.Now().UTC(),
			StrategyID:      s.ID,
			Symbol:          coin,
			PositionID:      ensurePositionTradeID(s.ID, coin, pos),
			Side:            orderSide,
			Quantity:        fillQty,
			Price:           fillPx,
			Value:           fillQty * fillPx,
			TradeType:       "hedge",
			Details:         fmt.Sprintf("HEDGE %s for %s @ $%.4f (fee $%.4f)", action.Kind, primarySymbol, fillPx, fee),
			ExchangeOrderID: oidStr,
			ExchangeFee:     fee,
			FeeSource:       feeSource,
			PnLGross:        true,
		})
		if logger != nil {
			logger.Info("HEDGE %s %s %s %.6f @ $%.4f", action.Kind, action.Side, coin, fillQty, fillPx)
		}
		return hedgeSyncResult{Trades: 1, Detail: fmt.Sprintf("[%s] HEDGE %s %s %.6f @ $%.2f", sc.ID, action.Kind, coin, fillQty, fillPx), Changed: true}
	case hedgeActionReduce, hedgeActionCloseFull:
		pos := s.Positions[coin]
		if pos == nil || pos.HedgeFor != primarySymbol {
			return hedgeSyncResult{}
		}
		if fillQty >= pos.Quantity-hedgeQtyEpsilon {
			if !bookPerpsCloseWithFillFee(s, coin, fillPx, fillFee, useFillFee, oidStr, "hedge_close", "HEDGE close", "hedge close", logger) {
				return hedgeSyncResult{}
			}
		} else {
			if !bookPerpsPartialCloseWithFillFee(s, coin, fillQty, fillPx, fillFee, useFillFee, oidStr, "hedge_reduce", "HEDGE reduce", "hedge reduce", logger) {
				return hedgeSyncResult{}
			}
			if remaining := s.Positions[coin]; remaining != nil {
				remaining.HedgePrimaryQtyBasis = hedgeBasisAfterFill(action, snap, fillQty)
			}
		}
		return hedgeSyncResult{Trades: 1, Detail: fmt.Sprintf("[%s] HEDGE %s %s %.6f @ $%.2f", sc.ID, action.Kind, coin, fillQty, fillPx), Changed: true}
	}
	return hedgeSyncResult{}
}

func hedgeBasisAfterFill(action hedgeAction, snap hedgeSnapshot, fillQty float64) float64 {
	if action.Qty <= hedgeQtyEpsilon {
		return snap.HedgeBasis
	}
	fraction := math.Min(1, math.Max(0, fillQty/action.Qty))
	switch action.Kind {
	case hedgeActionOpen:
		return snap.PrimaryQty * fraction
	case hedgeActionAdd:
		return snap.HedgeBasis + (snap.PrimaryQty-snap.HedgeBasis)*fraction
	case hedgeActionReduce:
		return snap.HedgeBasis - (snap.HedgeBasis-snap.PrimaryQty)*fraction
	default:
		return snap.HedgeBasis
	}
}

// unwindPrimaryAfterHedgeFailure is the fail-closed arm. A primary that has
// confirmed without a matching hedge must be reduced immediately; if that
// secondary close fails, virtual state remains intact and the next hedge sync
// retries rather than silently declaring the account flat.
func unwindPrimaryAfterHedgeFailure(sc StrategyConfig, s *StrategyState, primarySymbol string, requestedQty, fallbackPx float64, live bool, mu *sync.RWMutex, notifier *MultiNotifier, logger *StrategyLogger, freshPrimaryOpen bool, failure string) hedgeSyncResult {
	mu.RLock()
	pos := s.Positions[primarySymbol]
	if pos == nil || pos.HedgeFor != "" || pos.Quantity <= hedgeQtyEpsilon {
		mu.RUnlock()
		return hedgeSyncResult{}
	}
	qty := pos.Quantity
	if requestedQty > hedgeQtyEpsilon && requestedQty < qty {
		qty = requestedQty
	}
	avgCost := pos.AvgCost
	cancelOIDs := hyperliquidProtectionCancelOIDs(pos)
	mu.RUnlock()

	context := "existing primary"
	if freshPrimaryOpen {
		context = "fresh primary"
	}
	if !live {
		if fallbackPx <= 0 {
			fallbackPx = avgCost
		}
		mu.Lock()
		current := s.Positions[primarySymbol]
		if current == nil || current.HedgeFor != "" || current.Quantity <= hedgeQtyEpsilon {
			mu.Unlock()
			return hedgeSyncResult{}
		}
		if qty > current.Quantity {
			qty = current.Quantity
		}
		var ok bool
		if qty >= current.Quantity-hedgeQtyEpsilon {
			ok = bookPerpsClose(s, primarySymbol, fallbackPx, "hedge_open_failed_unwind", "HEDGE FAIL-CLOSED unwind", "hedge fail-closed unwind", logger)
		} else {
			ok = bookPerpsPartialCloseWithFillFee(s, primarySymbol, qty, fallbackPx, 0, false, "", "hedge_add_failed_unwind", "HEDGE FAIL-CLOSED add unwind", "hedge fail-closed add unwind", logger)
		}
		mu.Unlock()
		if ok {
			hedgeSyncCritical(notifier, logger, sc.ID, fmt.Sprintf("%s closed in paper mode after hedge failure: %s", context, failure))
			return hedgeSyncResult{Trades: 1, Detail: fmt.Sprintf("[%s] HEDGE FAIL-CLOSED %s", sc.ID, primarySymbol), Changed: true}
		}
		return hedgeSyncResult{}
	}
	result, stderr, err := RunHyperliquidClose(sc.Script, primarySymbol, &qty, cancelOIDs)
	if stderr != "" && logger != nil {
		logger.Info("hedge fail-closed unwind stderr: %s", stderr)
	}
	if err != nil || result == nil || result.Error != "" || result.Close == nil || result.Close.Fill == nil || result.Close.Fill.AvgPx <= 0 || result.Close.Fill.TotalSz <= hedgeQtyEpsilon {
		why := "unconfirmed primary unwind"
		if err != nil {
			why = err.Error()
		} else if result != nil && result.Error != "" {
			why = result.Error
		}
		hedgeSyncCritical(notifier, logger, sc.ID, fmt.Sprintf("%s hedge failure (%s); primary unwind FAILED: %s", context, failure, why))
		return hedgeSyncResult{}
	}
	fill := result.Close.Fill
	mu.Lock()
	ok := bookPerpsPartialCloseWithFillFee(s, primarySymbol, fill.TotalSz, fill.AvgPx, fill.Fee, true, strconv.FormatInt(fill.OID, 10), "hedge_open_failed_unwind", "HEDGE FAIL-CLOSED unwind", "hedge fail-closed unwind", logger)
	mu.Unlock()
	if !ok {
		return hedgeSyncResult{}
	}
	hedgeSyncCritical(notifier, logger, sc.ID, fmt.Sprintf("%s closed after hedge failure: %s", context, failure))
	return hedgeSyncResult{Trades: 1, Detail: fmt.Sprintf("[%s] HEDGE FAIL-CLOSED %s", sc.ID, primarySymbol), Changed: true}
}

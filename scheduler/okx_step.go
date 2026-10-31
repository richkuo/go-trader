package main

import (
	"math"
	"strings"
	"sync"
)

type okxCheckFunc func(sc StrategyConfig, posCtx PositionCtx, logger *StrategyLogger) (res *OKXResult, signalStr string, price float64, ok bool)

type okxStepPosition struct {
	Ctx      PositionCtx
	Qty      float64
	Side     string
	AvgCost  float64
	Leverage float64
}

type okxStepContext struct {
	Cfg              *Config
	State            *AppState
	StratState       *StrategyState
	Store            *StateStore
	StratDB          *StateDB
	Mu               *sync.RWMutex
	Notifier         *MultiNotifier
	Logger           *StrategyLogger
	Prices           map[string]float64
	Risk             *scopeCycleRisk
	Scope            PortfolioScope
	PersistenceHold  bool
	Coord            *basketCoordinator
	Pos              okxStepPosition
	Cash             float64
	PoolBalanceKnown bool
	CashReconcile    bool
}

type okxStepResult struct {
	Trades int
	Detail string
}

func okxStepDirection(sc StrategyConfig) (allowsLong, allowsShort, ok bool) {
	switch sc.Type {
	case "spot":
		return true, false, true
	case "perps":
		return PerpsAllowsLong(sc), PerpsAllowsShort(sc), true
	}
	return false, false, false
}

func okxStepRefusal(c okxStepContext, sc StrategyConfig, check okxCheckFunc) string {
	if sc.Platform != "okx" {
		return "platform " + sc.Platform + " is not okx"
	}
	if _, _, ok := okxStepDirection(sc); !ok {
		return "strategy type " + sc.Type + " has no OKX direction rule (want spot or perps)"
	}
	switch {
	case check == nil:
		return "no check was supplied"
	case c.Cfg == nil, c.State == nil, c.StratState == nil, c.Store == nil, c.StratDB == nil, c.Mu == nil, c.Prices == nil, c.Risk == nil:
		return "the step context is missing its configuration, state, storage, lock, prices or partition risk"
	case isPairMember(sc) && c.Coord == nil:
		return "a pair member needs the basket coordinator to record its observation"
	}
	return ""
}

func okxUsablePrice(p float64) bool {
	return p > 0 && !math.IsInf(p, 0)
}

func okxOrdinaryCheck(prices map[string]float64, cfg *Config, notifier *MultiNotifier) okxCheckFunc {
	return func(sc StrategyConfig, posCtx PositionCtx, logger *StrategyLogger) (*OKXResult, string, float64, bool) {
		return runOKXCheck(sc, prices, posCtx, cfg.Regime, resolveATRMethod(sc, cfg), notifier, logger)
	}
}

func okxPairManagementCheck(input pairManagementInput, notifier *MultiNotifier) okxCheckFunc {
	return okxPairManagementCheckWith(input, notifier, runPythonReadOnlyWithStdin)
}

func okxPairManagementCheckWith(input pairManagementInput, notifier *MultiNotifier, run func(script string, args []string, stdin []byte) ([]byte, []byte, error)) okxCheckFunc {
	return func(sc StrategyConfig, posCtx PositionCtx, logger *StrategyLogger) (*OKXResult, string, float64, bool) {
		args, result, stderr, err := runOKXPairManagementCheckRawWith(sc, posCtx, input, logger, run)
		frozen := map[string]float64{}
		if sym := okxSymbol(sc.Args); sym != "" && okxUsablePrice(input.MarkPrice) {
			frozen[sym] = input.MarkPrice
		}
		return completeOKXCheck(sc, args, result, stderr, err, frozen, notifier, logger)
	}
}

func runOKXStep(c okxStepContext, sc StrategyConfig, check okxCheckFunc) (out okxStepResult) {
	if why := okxStepRefusal(c, sc, check); why != "" {
		if c.Logger != nil {
			c.Logger.Error("OKX step refused for %s: %s", sc.ID, why)
		}
		return out
	}
	allowsLong, allowsShort, _ := okxStepDirection(sc)
	basketCoord, prices, cfg, notifier, logger := c.Coord, c.Prices, c.Cfg, c.Notifier, c.Logger
	okxPosCtx, okxPosQty, okxPosSide, okxAvgCost, okxPosLeverage := c.Pos.Ctx, c.Pos.Qty, c.Pos.Side, c.Pos.AvgCost, c.Pos.Leverage
	stratState, mu, sr, persistenceHold, stratScope := c.StratState, c.Mu, c.Risk, c.PersistenceHold, c.Scope
	store, stratDB, state := c.Store, c.StratDB, c.State
	okxCash, okxPoolBalanceKnown, okxCashReconcile := c.Cash, c.PoolBalanceKnown, c.CashReconcile
	var trades int
	var detail string
	if isPairMember(sc) {
		basketCoord.observePair(sc, StrategyDecisionFields{}, nil, 0, "the management check has not returned a validated pair echo this cycle")
	}
	if result, signalStr, price, ok := check(sc, okxPosCtx, logger); ok {
		validatedRegime := globalRegimeStore.PayloadForStrategy(sc, cfg.Regime)
		result.Regime = &validatedRegime
		prices[result.Symbol] = price
		suppressPairMemberIncrease(sc, &result.Signal, result.CloseFraction, okxPosQty, okxPosSide, signalStr, logger)
		if isPairMember(sc) {
			basketCoord.observePair(sc, result.StrategyDecisionFields, result.Indicators, price, "")
		}
		storeRegime := globalRegimeStore.PayloadForStrategy(sc, cfg.Regime)
		result.Regime = &storeRegime
		decisionGate := closedBarGateViewFor(sc, result.StrategyDecisionFields, storeRegime, cfg.Regime)
		result.RegimeReadinessHold = regimeDependencyHold(sc, storeRegime, decisionGate.Regime, cfg.Regime, false)
		applyRegimeReadinessHold(result.RegimeReadinessHold, &result.Signal, &result.CloseFraction, okxPosQty, okxPosSide, allowsLong, allowsShort, logger)
		logClosedBarDecision(sc, result.StrategyDecisionFields, logger)
		if !decisionGate.Held {
			if gateRegime, regimeBlocked := applyRegimeGate(sc, decisionGate.Regime, cfg.Regime, okxPosQty); regimeBlocked {
				logger.Info("Regime gate: open signal blocked (%s)", regimeGateBlockDetail(gateRegime))
				result.Signal = 0
			}
		}
		hurstDecision := closedBarHurstGate(sc, decisionGate, cfg.Regime, stratState, mu, okxPosQty)
		if decisionGate.HoldReason != "" && pausedBlocksSignal(result.Signal, result.CloseFraction, okxPosQty, okxPosSide, allowsLong, allowsShort) {
			logger.Warn("Closed-bar decision: %s signal suppressed — %s (#1712)", signalStr, decisionGate.HoldReason)
			result.Signal = 0
		}
		if htfHeld, htfWhy := htfFilterHoldsSignal(sc, result.StrategyDecisionFields, result.Signal, result.CloseFraction, okxPosQty, okxPosSide, allowsLong, allowsShort); htfHeld {
			logHTFFilterHold(signalStr, htfWhy, logger)
			result.Signal = 0
		}
		if hurstDecision.Holds && pausedBlocksSignal(result.Signal, result.CloseFraction, okxPosQty, okxPosSide, allowsLong, allowsShort) {
			logger.Info("Hurst gate: %s signal suppressed — %s (#1411)", signalStr, hurstDecision.Detail)
			result.Signal = 0
		}
		if sc.Paused && pausedBlocksSignal(result.Signal, result.CloseFraction, okxPosQty, okxPosSide, allowsLong, allowsShort) {
			logger.Info("Paused: %s signal suppressed — position-increasing actions held while paused (#1150)", signalStr)
			result.Signal = 0
		}
		if sr.DailyLossEntriesHeld && pausedBlocksSignal(result.Signal, result.CloseFraction, okxPosQty, okxPosSide, allowsLong, allowsShort) {
			logger.Info("Daily loss limit: %s signal suppressed — entries held until UTC rollover (#1269)", signalStr)
			result.Signal = 0
		}
		if sr.NotionalBlocked && pausedBlocksSignal(result.Signal, result.CloseFraction, okxPosQty, okxPosSide, allowsLong, allowsShort) {
			logger.Warn("Notional cap: %s signal suppressed — new opens blocked, exits continue (#1344)", signalStr)
			result.Signal = 0
		}
		if persistenceHold && pausedBlocksSignal(result.Signal, result.CloseFraction, okxPosQty, okxPosSide, allowsLong, allowsShort) {
			logger.Warn("Persistence hold: %s signal suppressed — the %s scope has an unacknowledged state save failure, so no new exposure is taken; exits, stops, ratchet and protection continue (#1523)", signalStr, scopeLabel(stratScope))
			result.Signal = 0
		}
		if capBlocked, capWhy := exposureCapBlocksSignal(sr.ExposureCapStatus, extractAsset(sc), result.Signal, result.CloseFraction, okxPosQty, okxPosSide, allowsLong, allowsShort); capBlocked {
			logger.Warn("Exposure cap: %s signal suppressed — %s (#1270)", signalStr, capWhy)
			result.Signal = 0
		}
		entryAdm := admitClosedBarEntry(sc, stratState, mu, result.Symbol, result.StrategyDecisionFields, result.Signal, result.CloseFraction, okxPosQty, okxPosSide, tradingNow())
		logEntryAdmission(entryAdm, signalStr, logger)
		applyEntryAdmission(entryAdm, &result.Signal, &result.CloseFraction)
		result.CloseReduceOnly = entryAdm.CloseOnly
		suppressPairMemberIncrease(sc, &result.Signal, result.CloseFraction, okxPosQty, okxPosSide, signalStr, logger)
		mu.Lock()
		syncStrategyRegimeState(stratState, storeRegime, cfg.Regime)
		mu.Unlock()
		var execResult *OKXExecuteResult
		var okxEntryIntent *entryIntent
		liveExecFailed := false
		if okxIsLive(sc.Args) && result.Signal != 0 && entryAdm.admitted() {
			in, reserveErr := reserveLiveEntryIntent(store, sc, stratState, mu, entryAdm, entryProviderOKX, entryIntentRequest{Kind: entryAdm.Kind.String(), Signal: result.Signal, PrevPosQty: okxPosQty, PrevPosSide: okxPosSide, PrevAvgCost: okxAvgCost, Price: price, EntryATR: indicatorsATRValue(result.Indicators)}, tradingNow())
			if reserveErr != nil {
				logger.Warn("Closed-bar entry: %s held — the live attempt could not be saved before sending: %v (#1769)", signalStr, reserveErr)
				liveExecFailed = true
			} else {
				okxEntryIntent = in
				result.EntrySubmitGate = okxEntrySubmitGateFor(store, stratState, mu, in)
			}
		}
		if okxIsLive(sc.Args) && result.Signal != 0 && !liveExecFailed {
			er, ok2 := runOKXExecuteOrder(sc, result, price, okxCash, okxPoolBalanceKnown, okxCashReconcile, okxPosQty, okxPosSide, okxAvgCost, okxPosLeverage, hurstDecision, notifier, logger)
			if ok2 {
				execResult = er
			} else {
				liveExecFailed = true
				outcome, outcomeDetail := okxExecuteOutcome(er)
				finishFailedLiveEntry(store, sc, stratState, mu, okxEntryIntent, result.OrderSubmitted, outcome, outcomeDetail, notifier, logger)
			}
		}
		if !liveExecFailed {
			var cashAlert string
			mu.Lock()
			entryAt := len(stratState.TradeHistory)
			endEntryBooking := beginEntryBookingLocked(stratState, entryAdm, okxEntryIntent)
			trades, detail, cashAlert = executeOKXResult(sc, stratState, stratDB, result, execResult, signalStr, price, cfg.Regime, cfg, hurstDecision, logger)
			finishEntryBookingLocked(store, stratState, entryAdm, okxEntryIntent, entryAt, endEntryBooking(), okxEntryEvidence(execResult), logger)
			mu.Unlock()
			if cashAlert != "" {
				notifySpotLiveCashOverBudget(notifier, cashAlert)
				mu.RLock()
				ids, _ := collectCashReconcileRequiredSnapshots(state)
				mu.RUnlock()
				globalSpotCashReconcileReminder.MarkNotified(strings.Join(ids, ","), tradingNow().UTC())
			}
		}
	}
	out.Trades, out.Detail = trades, detail
	return out
}

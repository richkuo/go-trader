package main

import (
	"strings"
	"sync"
	"time"
)

func okxSpotDispatch(sc StrategyConfig, basketCoord *basketCoordinator, prices map[string]float64, okxPosCtx PositionCtx, cfg *Config, notifier *MultiNotifier, logger *StrategyLogger, okxPosQty float64, okxPosSide string, stratState *StrategyState, mu *sync.RWMutex, sr *scopeCycleRisk, persistenceHold bool, stratScope PortfolioScope, store *StateStore, okxAvgCost, okxCash float64, okxPoolBalanceKnown, okxCashReconcile bool, okxPosLeverage float64, stratDB *StateDB, state *AppState) (trades int, detail string) {
	if isPairMember(sc) {
		basketCoord.observePair(sc, StrategyDecisionFields{}, nil, 0, "the management check has not returned a validated pair echo this cycle")
	}
	if result, signalStr, price, ok := runOKXCheck(sc, prices, okxPosCtx, cfg.Regime, resolveATRMethod(sc, cfg), notifier, logger); ok {
		prices[result.Symbol] = price
		suppressPairMemberIncrease(sc, &result.Signal, result.CloseFraction, okxPosQty, okxPosSide, signalStr, logger)
		if isPairMember(sc) {
			basketCoord.observePair(sc, result.StrategyDecisionFields, result.Indicators, price, "")
		}
		storeRegime := globalRegimeStore.PayloadForStrategy(sc, cfg.Regime)
		result.Regime = &storeRegime
		decisionGate := closedBarGateViewFor(sc, result.StrategyDecisionFields, storeRegime, cfg.Regime)
		logClosedBarDecision(sc, result.StrategyDecisionFields, logger)
		if !decisionGate.Held {
			if gateRegime, regimeBlocked := applyRegimeGate(sc, decisionGate.Regime, cfg.Regime, okxPosQty); regimeBlocked {
				logger.Info("Regime gate: open signal blocked (%s)", regimeGateBlockDetail(gateRegime))
				result.Signal = 0
			}
		}
		hurstDecision := closedBarHurstGate(sc, decisionGate, cfg.Regime, stratState, mu, okxPosQty)
		if decisionGate.HoldReason != "" && pausedBlocksSignal(result.Signal, result.CloseFraction, okxPosQty, okxPosSide, true, false) {
			logger.Warn("Closed-bar decision: %s signal suppressed — %s (#1712)", signalStr, decisionGate.HoldReason)
			result.Signal = 0
		}
		if htfHeld, htfWhy := htfFilterHoldsSignal(sc, result.StrategyDecisionFields, result.Signal, result.CloseFraction, okxPosQty, okxPosSide, true, false); htfHeld {
			logHTFFilterHold(signalStr, htfWhy, logger)
			result.Signal = 0
		}
		if hurstDecision.Holds && pausedBlocksSignal(result.Signal, result.CloseFraction, okxPosQty, okxPosSide, true, false) {
			logger.Info("Hurst gate: %s signal suppressed — %s (#1411)", signalStr, hurstDecision.Detail)
			result.Signal = 0
		}
		if sc.Paused && pausedBlocksSignal(result.Signal, result.CloseFraction, okxPosQty, okxPosSide, true, false) {
			logger.Info("Paused: %s signal suppressed — position-increasing actions held while paused (#1150)", signalStr)
			result.Signal = 0
		}
		if sr.DailyLossEntriesHeld && pausedBlocksSignal(result.Signal, result.CloseFraction, okxPosQty, okxPosSide, true, false) {
			logger.Info("Daily loss limit: %s signal suppressed — entries held until UTC rollover (#1269)", signalStr)
			result.Signal = 0
		}
		if sr.NotionalBlocked && pausedBlocksSignal(result.Signal, result.CloseFraction, okxPosQty, okxPosSide, true, false) {
			logger.Warn("Notional cap: %s signal suppressed — new opens blocked, exits continue (#1344)", signalStr)
			result.Signal = 0
		}
		if persistenceHold && pausedBlocksSignal(result.Signal, result.CloseFraction, okxPosQty, okxPosSide, true, false) {
			logger.Warn("Persistence hold: %s signal suppressed — the %s scope has an unacknowledged state save failure, so no new exposure is taken; exits, stops, ratchet and protection continue (#1523)", signalStr, scopeLabel(stratScope))
			result.Signal = 0
		}
		if capBlocked, capWhy := exposureCapBlocksSignal(sr.ExposureCapStatus, extractAsset(sc), result.Signal, result.CloseFraction, okxPosQty, okxPosSide, true, false); capBlocked {
			logger.Warn("Exposure cap: %s signal suppressed — %s (#1270)", signalStr, capWhy)
			result.Signal = 0
		}
		entryAdm := admitClosedBarEntry(sc, stratState, mu, result.Symbol, result.StrategyDecisionFields, result.Signal, result.CloseFraction, okxPosQty, okxPosSide, time.Now())
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
			in, reserveErr := reserveLiveEntryIntent(store, sc, stratState, mu, entryAdm, entryProviderOKX, entryIntentRequest{Kind: entryAdm.Kind.String(), Signal: result.Signal, PrevPosQty: okxPosQty, PrevPosSide: okxPosSide, PrevAvgCost: okxAvgCost, Price: price, EntryATR: indicatorsATRValue(result.Indicators)}, time.Now())
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
				globalSpotCashReconcileReminder.MarkNotified(strings.Join(ids, ","), time.Now().UTC())
			}
		}
	}
	return trades, detail
}

func okxPerpsDispatch(sc StrategyConfig, basketCoord *basketCoordinator, prices map[string]float64, okxPosCtx PositionCtx, cfg *Config, notifier *MultiNotifier, logger *StrategyLogger, okxPosQty float64, okxPosSide string, stratState *StrategyState, mu *sync.RWMutex, sr *scopeCycleRisk, persistenceHold bool, stratScope PortfolioScope, store *StateStore, okxAvgCost, okxCash float64, okxPoolBalanceKnown, okxCashReconcile bool, okxPosLeverage float64, stratDB *StateDB, state *AppState) (trades int, detail string) {
	if isPairMember(sc) {
		basketCoord.observePair(sc, StrategyDecisionFields{}, nil, 0, "the management check has not returned a validated pair echo this cycle")
	}
	if result, signalStr, price, ok := runOKXCheck(sc, prices, okxPosCtx, cfg.Regime, resolveATRMethod(sc, cfg), notifier, logger); ok {
		prices[result.Symbol] = price
		suppressPairMemberIncrease(sc, &result.Signal, result.CloseFraction, okxPosQty, okxPosSide, signalStr, logger)
		if isPairMember(sc) {
			basketCoord.observePair(sc, result.StrategyDecisionFields, result.Indicators, price, "")
		}
		storeRegime := globalRegimeStore.PayloadForStrategy(sc, cfg.Regime)
		result.Regime = &storeRegime
		decisionGate := closedBarGateViewFor(sc, result.StrategyDecisionFields, storeRegime, cfg.Regime)
		logClosedBarDecision(sc, result.StrategyDecisionFields, logger)
		if !decisionGate.Held {
			if gateRegime, regimeBlocked := applyRegimeGate(sc, decisionGate.Regime, cfg.Regime, okxPosQty); regimeBlocked {
				logger.Info("Regime gate: open signal blocked (%s)", regimeGateBlockDetail(gateRegime))
				result.Signal = 0
			}
		}
		hurstDecision := closedBarHurstGate(sc, decisionGate, cfg.Regime, stratState, mu, okxPosQty)
		if decisionGate.HoldReason != "" && pausedBlocksSignal(result.Signal, result.CloseFraction, okxPosQty, okxPosSide, PerpsAllowsLong(sc), PerpsAllowsShort(sc)) {
			logger.Warn("Closed-bar decision: %s signal suppressed — %s (#1712)", signalStr, decisionGate.HoldReason)
			result.Signal = 0
		}
		if htfHeld, htfWhy := htfFilterHoldsSignal(sc, result.StrategyDecisionFields, result.Signal, result.CloseFraction, okxPosQty, okxPosSide, PerpsAllowsLong(sc), PerpsAllowsShort(sc)); htfHeld {
			logHTFFilterHold(signalStr, htfWhy, logger)
			result.Signal = 0
		}
		if hurstDecision.Holds && pausedBlocksSignal(result.Signal, result.CloseFraction, okxPosQty, okxPosSide, PerpsAllowsLong(sc), PerpsAllowsShort(sc)) {
			logger.Info("Hurst gate: %s signal suppressed — %s (#1411)", signalStr, hurstDecision.Detail)
			result.Signal = 0
		}
		if sc.Paused && pausedBlocksSignal(result.Signal, result.CloseFraction, okxPosQty, okxPosSide, PerpsAllowsLong(sc), PerpsAllowsShort(sc)) {
			logger.Info("Paused: %s signal suppressed — position-increasing actions held while paused (#1150)", signalStr)
			result.Signal = 0
		}
		if sr.DailyLossEntriesHeld && pausedBlocksSignal(result.Signal, result.CloseFraction, okxPosQty, okxPosSide, PerpsAllowsLong(sc), PerpsAllowsShort(sc)) {
			logger.Info("Daily loss limit: %s signal suppressed — entries held until UTC rollover (#1269)", signalStr)
			result.Signal = 0
		}
		if sr.NotionalBlocked && pausedBlocksSignal(result.Signal, result.CloseFraction, okxPosQty, okxPosSide, PerpsAllowsLong(sc), PerpsAllowsShort(sc)) {
			logger.Warn("Notional cap: %s signal suppressed — new opens blocked, exits continue (#1344)", signalStr)
			result.Signal = 0
		}
		if persistenceHold && pausedBlocksSignal(result.Signal, result.CloseFraction, okxPosQty, okxPosSide, PerpsAllowsLong(sc), PerpsAllowsShort(sc)) {
			logger.Warn("Persistence hold: %s signal suppressed — the %s scope has an unacknowledged state save failure, so no new exposure is taken; exits, stops, ratchet and protection continue (#1523)", signalStr, scopeLabel(stratScope))
			result.Signal = 0
		}
		if capBlocked, capWhy := exposureCapBlocksSignal(sr.ExposureCapStatus, extractAsset(sc), result.Signal, result.CloseFraction, okxPosQty, okxPosSide, PerpsAllowsLong(sc), PerpsAllowsShort(sc)); capBlocked {
			logger.Warn("Exposure cap: %s signal suppressed — %s (#1270)", signalStr, capWhy)
			result.Signal = 0
		}
		entryAdm := admitClosedBarEntry(sc, stratState, mu, result.Symbol, result.StrategyDecisionFields, result.Signal, result.CloseFraction, okxPosQty, okxPosSide, time.Now())
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
			in, reserveErr := reserveLiveEntryIntent(store, sc, stratState, mu, entryAdm, entryProviderOKX, entryIntentRequest{Kind: entryAdm.Kind.String(), Signal: result.Signal, PrevPosQty: okxPosQty, PrevPosSide: okxPosSide, PrevAvgCost: okxAvgCost, Price: price, EntryATR: indicatorsATRValue(result.Indicators)}, time.Now())
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
				globalSpotCashReconcileReminder.MarkNotified(strings.Join(ids, ","), time.Now().UTC())
			}
		}
	}
	return trades, detail
}

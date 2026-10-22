package main

import (
	"fmt"
	"sync"
)

type hlPerpsStep struct {
	sc                       StrategyConfig
	mu                       *sync.RWMutex
	logger                   *StrategyLogger
	stratState               *StrategyState
	notifier                 *MultiNotifier
	cfg                      *Config
	stratDB                  *StateDB
	store                    *StateStore
	state                    *AppState
	sr                       *scopeCycleRisk
	prices                   map[string]float64
	feedCtx                  *marketFeedContext
	hlBatchResults           *hlBatchCycleResults
	basketCoord              *basketCoordinator
	intervals                map[string]int
	stratScope               PortfolioScope
	persistenceHold          bool
	cbManageOnly             bool
	hlProfileResolved        bool
	hlProfileNext            RegimeProfileState
	hlProfileActive          string
	hlPosCtx                 PositionCtx
	hlPosQty                 float64
	hlPosSide                string
	hlAvgCost                float64
	hlPosLeverage            float64
	hlEntryATR               float64
	hlStopLossOID            int64
	hlTPOIDs                 []int64
	hlStopLossTriggerPx      float64
	hlStopLossHighWaterPx    float64
	hlPosSnapshot            *Position
	hlScaleInCount           int
	hlLastAddPrice           float64
	hlAddedNotionalUSD       float64
	hlScaleInCash            float64
	hlScaleInResizePending   bool
	hlSharedCloseHoldUSD     float64
	hlSharedCloseHoldReason  string
	hlPeerVirtualQty         float64
	hlPeerSameQty            float64
	hlPeerOppQty             float64
	hlPeerSame               []hlShareBook
	hlStopArmed              bool
	hlPoolBalanceKnown       bool
	hlCash                   float64
	hlNetSideByCoin          map[string]string
	hlCycle                  *hlCycleShare
	hlLiquidationPx          map[string]float64
	hlReconcileFillHintsJSON []byte
	hlReconcileAll           []StrategyConfig
	hlStateFetched           bool
	hlOnChainAbsQty          map[string]float64
	hlAddr                   string
	hlPositions              []HLPosition
}

type hlPerpsPosition struct {
	Ctx                   PositionCtx
	Leverage              float64
	EntryATR              float64
	StopLossOID           int64
	TPOIDs                []int64
	StopLossTriggerPx     float64
	StopLossHighWaterPx   float64
	Snapshot              *Position
	ScaleInCount          int
	LastAddPrice          float64
	AddedNotionalUSD      float64
	ScaleInResizePending  bool
	SharedCloseHoldUSD    float64
	SharedCloseHoldReason string
}

func readHLPerpsPosition(sc StrategyConfig, s *StrategyState, sym string, regime *RegimeConfig) (hlPerpsPosition, bool) {
	if s == nil {
		return hlPerpsPosition{}, false
	}
	pos, ok := s.Positions[sym]
	if !ok {
		return hlPerpsPosition{}, false
	}
	return hlPerpsPosition{
		Ctx:                   positionCtxForCheck(sc, pos, regime),
		Leverage:              pos.Leverage,
		EntryATR:              pos.EntryATR,
		StopLossOID:           pos.StopLossOID,
		TPOIDs:                cloneInt64s(pos.TPOIDs),
		StopLossTriggerPx:     pos.StopLossTriggerPx,
		StopLossHighWaterPx:   pos.StopLossHighWaterPx,
		Snapshot:              hyperliquidProtectionPositionSnapshot(pos),
		ScaleInCount:          pos.ScaleInCount,
		LastAddPrice:          pos.LastAddPrice,
		AddedNotionalUSD:      pos.AddedNotionalUSD,
		ScaleInResizePending:  pos.ScaleInResizePending,
		SharedCloseHoldUSD:    pos.SharedCloseHoldUSD,
		SharedCloseHoldReason: pos.SharedCloseHoldReason,
	}, true
}

type hlPerpsStepOutcome struct {
	Ran    bool
	Trades int
	Detail string
	Step   *hlStepTradeAlerts
}

func runHyperliquidPerpsStep(x *hlPerpsStep) hlPerpsStepOutcome {
	sc := x.sc
	mu := x.mu
	logger := x.logger
	stratState := x.stratState
	notifier := x.notifier
	cfg := x.cfg
	stratDB := x.stratDB
	store := x.store
	state := x.state
	sr := x.sr
	prices := x.prices
	feedCtx := x.feedCtx
	hlBatchResults := x.hlBatchResults
	basketCoord := x.basketCoord
	intervals := x.intervals
	stratScope := x.stratScope
	persistenceHold := x.persistenceHold
	cbManageOnly := x.cbManageOnly
	hlProfileResolved := x.hlProfileResolved
	hlProfileNext := x.hlProfileNext
	hlProfileActive := x.hlProfileActive
	hlPosCtx := x.hlPosCtx
	hlPosQty := x.hlPosQty
	hlPosSide := x.hlPosSide
	hlAvgCost := x.hlAvgCost
	hlPosLeverage := x.hlPosLeverage
	hlEntryATR := x.hlEntryATR
	hlStopLossOID := x.hlStopLossOID
	hlTPOIDs := x.hlTPOIDs
	hlStopLossTriggerPx := x.hlStopLossTriggerPx
	hlStopLossHighWaterPx := x.hlStopLossHighWaterPx
	hlPosSnapshot := x.hlPosSnapshot
	hlScaleInCount := x.hlScaleInCount
	hlLastAddPrice := x.hlLastAddPrice
	hlAddedNotionalUSD := x.hlAddedNotionalUSD
	hlScaleInCash := x.hlScaleInCash
	hlScaleInResizePending := x.hlScaleInResizePending
	hlSharedCloseHoldUSD := x.hlSharedCloseHoldUSD
	hlSharedCloseHoldReason := x.hlSharedCloseHoldReason
	hlPeerVirtualQty := x.hlPeerVirtualQty
	hlPeerSameQty := x.hlPeerSameQty
	hlPeerOppQty := x.hlPeerOppQty
	hlPeerSame := x.hlPeerSame
	hlStopArmed := x.hlStopArmed
	hlPoolBalanceKnown := x.hlPoolBalanceKnown
	hlCash := x.hlCash
	hlNetSideByCoin := x.hlNetSideByCoin
	hlCycle := x.hlCycle
	hlLiquidationPx := x.hlLiquidationPx
	hlReconcileFillHintsJSON := x.hlReconcileFillHintsJSON
	hlReconcileAll := x.hlReconcileAll
	hlStateFetched := x.hlStateFetched
	hlOnChainAbsQty := x.hlOnChainAbsQty
	hlAddr := x.hlAddr
	hlPositions := x.hlPositions
	var trades int
	var detail string
	var hlStep *hlStepTradeAlerts
	if result, signalStr, price, ok := runHyperliquidCheck(&sc, prices, hlPosCtx, cfg.Regime, resolveATRMethod(sc, cfg), notifier, logger, hlBatchResults, feedCtx); ok {
		prices[result.Symbol] = price
		if isBasketMember(sc) {
			if pausedBlocksSignal(result.Signal, result.CloseFraction, hlPosQty, hlPosSide, PerpsAllowsLong(sc), PerpsAllowsShort(sc)) {
				logger.Error("Basket member check returned a position-increasing %s signal; only the basket coordinator places member increases, so it is dropped (#1781)", signalStr)
				result.Signal = 0
			}
			basketCoord.observe(sc, result, price)
		}
		if hlPosCtx.RestingTP != nil {
			mu.Lock()
			recordRestingTPScan(stratState, result.Symbol, hlPosCtx.RestingTP, result.StrategyDecisionFields)
			mu.Unlock()
		}
		hlStep = beginHyperliquidStepTradeAlerts(sc, stratState, mu)
		guardSameSideClose(sc, result, hlPosSide, hlPosQty, notifier, logger)
		if cbManageOnly {
			result.Signal = 0
		}
		if !hyperliquidIsLive(sc.Args) && hlPosQty > 0 {
			breachAt := hlStep.historyLen(mu)
			if paperStopTrades, paperStopDetail := applyPaperStopLossBreach(sc, stratState, result.Symbol, hlPosSide, price, mu, logger); paperStopTrades > 0 {
				hlStep.bindWindow(mu, breachAt, paperStopDetail)
				hlPosQty, hlPosSide, hlAvgCost, hlEntryATR = 0, "", 0, 0
				hlStopLossTriggerPx, hlStopLossHighWaterPx, hlPosSnapshot = 0, 0, nil
				hlScaleInCount, hlLastAddPrice, hlAddedNotionalUSD = 0, 0, 0
			}
		}
		storeRegime := globalRegimeStore.PayloadForStrategy(sc, cfg.Regime)
		result.Regime = &storeRegime
		decisionGate := closedBarGateViewFor(sc, result.StrategyDecisionFields, storeRegime, cfg.Regime)
		logClosedBarDecision(sc, result.StrategyDecisionFields, logger)
		if !decisionGate.Held {
			if gateRegime, regimeBlocked := applyRegimeGate(sc, decisionGate.Regime, cfg.Regime, hlPosQty); regimeBlocked {
				logger.Info("Regime gate: open signal blocked (%s)", regimeGateBlockDetail(gateRegime))
				result.Signal = 0
			}
		}
		hurstDecision := closedBarHurstGate(sc, decisionGate, cfg.Regime, stratState, mu, hlPosQty)
		if decisionGate.HoldReason != "" && pausedBlocksSignal(result.Signal, result.CloseFraction, hlPosQty, hlPosSide, PerpsAllowsLong(sc), PerpsAllowsShort(sc)) {
			logger.Warn("Closed-bar decision: %s signal suppressed — %s (#1712)", signalStr, decisionGate.HoldReason)
			result.Signal = 0
		}
		if htfHeld, htfWhy := htfFilterHoldsSignal(sc, result.StrategyDecisionFields, result.Signal, result.CloseFraction, hlPosQty, hlPosSide, PerpsAllowsLong(sc), PerpsAllowsShort(sc)); htfHeld {
			logHTFFilterHold(signalStr, htfWhy, logger)
			result.Signal = 0
		}
		if hurstDecision.Holds && pausedBlocksSignal(result.Signal, result.CloseFraction, hlPosQty, hlPosSide, PerpsAllowsLong(sc), PerpsAllowsShort(sc)) {
			logger.Info("Hurst gate: %s signal suppressed — %s (#1411)", signalStr, hurstDecision.Detail)
			result.Signal = 0
		}
		if sc.Paused && pausedBlocksSignal(result.Signal, result.CloseFraction, hlPosQty, hlPosSide, PerpsAllowsLong(sc), PerpsAllowsShort(sc)) {
			logger.Info("Paused: %s signal suppressed — position-increasing actions held while paused (#1150)", signalStr)
			result.Signal = 0
		}
		if sr.DailyLossEntriesHeld && pausedBlocksSignal(result.Signal, result.CloseFraction, hlPosQty, hlPosSide, PerpsAllowsLong(sc), PerpsAllowsShort(sc)) {
			logger.Info("Daily loss limit: %s signal suppressed — entries held until UTC rollover (#1269)", signalStr)
			result.Signal = 0
		}
		if sr.NotionalBlocked && pausedBlocksSignal(result.Signal, result.CloseFraction, hlPosQty, hlPosSide, PerpsAllowsLong(sc), PerpsAllowsShort(sc)) {
			logger.Warn("Notional cap: %s signal suppressed — new opens blocked, exits continue (#1344)", signalStr)
			result.Signal = 0
		}
		if persistenceHold && pausedBlocksSignal(result.Signal, result.CloseFraction, hlPosQty, hlPosSide, PerpsAllowsLong(sc), PerpsAllowsShort(sc)) {
			logger.Warn("Persistence hold: %s signal suppressed — the %s scope has an unacknowledged state save failure, so no new exposure is taken; exits, stops, ratchet and protection continue (#1523)", signalStr, scopeLabel(stratScope))
			result.Signal = 0
		}
		if capBlocked, capWhy := exposureCapBlocksSignal(sr.ExposureCapStatus, extractAsset(sc), result.Signal, result.CloseFraction, hlPosQty, hlPosSide, PerpsAllowsLong(sc), PerpsAllowsShort(sc)); capBlocked {
			logger.Warn("Exposure cap: %s signal suppressed — %s (#1270)", signalStr, capWhy)
			result.Signal = 0
		}
		if replayMirrorPaperActive(sc) && pausedBlocksSignal(result.Signal, result.CloseFraction, hlPosQty, hlPosSide, PerpsAllowsLong(sc), PerpsAllowsShort(sc)) {
			logger.Info("Replay mirror: %s signal suppressed — entries mirror the live decision log (#1431)", signalStr)
			result.Signal = 0
		}
		if feedHeld, feedWhy := feedCtx.feedHoldsSignal(sc); feedHeld && pausedBlocksSignal(result.Signal, result.CloseFraction, hlPosQty, hlPosSide, PerpsAllowsLong(sc), PerpsAllowsShort(sc)) {
			logger.Warn("Market feed: %s signal suppressed (%s) — closes, stops, ratchet and protection continue (#1524)", signalStr, feedWhy)
			result.Signal = 0
		}
		if tooOld, ageWhy := feedCtx.decisionTooOld(tradingNow(), intervals[sc.ID]); tooOld && pausedBlocksSignal(result.Signal, result.CloseFraction, hlPosQty, hlPosSide, PerpsAllowsLong(sc), PerpsAllowsShort(sc)) {
			logger.Warn("Market feed: %s signal suppressed (%s) — closes, stops, ratchet and protection continue (#1524)", signalStr, ageWhy)
			result.Signal = 0
		}
		entryAdm := admitClosedBarEntry(sc, stratState, mu, result.Symbol, result.StrategyDecisionFields, result.Signal, result.CloseFraction, hlPosQty, hlPosSide, tradingNow())
		logEntryAdmission(entryAdm, signalStr, logger)
		applyEntryAdmission(entryAdm, &result.Signal, &result.CloseFraction)
		mu.Lock()
		syncStrategyRegimeState(stratState, storeRegime, cfg.Regime)
		updateStrategyDivergenceState(stratState, result.Divergence)
		mu.Unlock()
		floorOutcome, floorRemainderUSD := hlSharedCloseFloorNone, 0.0
		if hyperliquidIsLive(sc.Args) && result.Signal != 0 {
			floorRefetch := func() (hlOnChainCoinView, error) {
				_, fresh, err := fetchHyperliquidStateFn(hlAddr)
				if err != nil {
					return hlOnChainCoinView{}, err
				}
				return hlOnChainCoinViewFromPositions(fresh), nil
			}
			floorOutcome, floorRemainderUSD = applySharedCoinFullCloseFloor(sc, result, hlPosQty, hlPosSide, price, hlReconcileAll, hlOnChainCoinView{Known: hlStateFetched, AbsQty: hlOnChainAbsQty, NetSide: hlNetSideByCoin}, hlPeerVirtualQty, hlSharedCloseHoldReason, floorRefetch, notifier, logger)
		}
		var execResult *HyperliquidExecuteResult
		liveExecFailed := false
		hedgeFreshExposureQty := 0.0
		var manageRatchetAlert *RatchetTriggerAlert
		var manageStopEvidence ratchetStopEvidence
		manageRatchetTightened := false
		if result.Signal == 0 && hlPosQty > 0 && strategyUsesTrailingTPRatchetClose(sc) {
			manageRatchetAlert = applyTrailingTPRatchet(sc, stratState, result.Symbol, price, mu, logger)
			manageRatchetTightened = manageRatchetAlert != nil
			mu.RLock()
			if pos, ok3 := stratState.Positions[result.Symbol]; ok3 && pos != nil {
				hlPosSnapshot = hyperliquidProtectionPositionSnapshot(pos)
			}
			mu.RUnlock()
		}
		if result.Signal == 0 && hlPosQty > 0 && atrMultMissingEntryATR(sc, hlPosSnapshot) {
			notifyATRMultMissingEntryATROnce(sc, result.Symbol, notifier, logger)
		}
		if result.Signal == 0 && hlPosQty > 0 && tieredTPATRMissingEntryATR(sc, hlPosSnapshot) {
			notifyTieredTPATRMissingEntryATROnce(sc, result.Symbol, notifier, logger)
		}
		if !hyperliquidIsLive(sc.Args) && result.Signal == 0 && hlPosQty > 0 && effectiveTrailingStopPct(sc, hlPosSnapshot) > 0 {
			newHighWater, newTrigger, breach, breachPx := runHyperliquidTrailingStopPaper(sc, hlPosSide, hlPosSnapshot, price, hlStopLossHighWaterPx, hlStopLossTriggerPx, trailingReplacePolicy{ratchetTightened: manageRatchetTightened})
			manageStopEvidence = ratchetStopEvidence{Ran: true, Live: false}
			mu.Lock()
			if pos, ok3 := stratState.Positions[result.Symbol]; ok3 && pos.Quantity > 0 && pos.Side == hlPosSide {
				if breach {
					if newTrigger > 0 {
						pos.StopLossTriggerPx = newTrigger
					}
					stopAt := hlStep.historyLenLocked()
					breachPx = paperStopBookPx(hlPosSide, breachPx)
					if recordPerpsStopLossClose(stratState, result.Symbol, breachPx, paperStopReasonTrailing, logger) {
						hlStep.bindWindowLocked(stopAt, fmt.Sprintf("[%s] PAPER TRAILING SL %s @ $%.2f", sc.ID, result.Symbol, breachPx))
					}
				} else {
					if newHighWater > 0 {
						pos.StopLossHighWaterPx = newHighWater
					}
					if newTrigger > 0 {
						pos.StopLossTriggerPx = newTrigger
						pos.RatchetFallbackNormalizePending = false
						logger.Info("Paper trailing SL trigger updated @ $%.4f (high_water=$%.4f)", newTrigger, newHighWater)
					}
				}
			}
			mu.Unlock()
		}
		if hyperliquidIsLive(sc.Args) && result.Signal == 0 && hlPosQty > 0 && effectiveTrailingStopPct(sc, hlPosSnapshot) > 0 {
			q := hlStopQty{Qty: hlPosQty, Fresh: true}
			if hlCycle != nil {
				q = hlCycle.StopQty(sc, result.Symbol, hlPosSide, hlPosQty, hlStopArmed, hlPeerSame, hlPeerOppQty)
			}
			slEffectiveQty, capped, place := hlReplaceQty(q, hlPosQty)
			if place {
				if capped {
					logger.Warn("trailing SL arm: virtual qty %.6f > chain share %.6f for %s; capping", hlPosQty, slEffectiveQty, result.Symbol)
				}
				forceResize := hlScaleInResizePending && !capped
				newHighWater, slUpdate, updateConfirmed := runHyperliquidTrailingStopUpdate(sc, result.Symbol, hlPosSide, slEffectiveQty, hlPosSnapshot, price, hlStopLossHighWaterPx, hlStopLossTriggerPx, hlStopLossOID, trailingReplacePolicy{forceResize: forceResize, ratchetTightened: manageRatchetTightened, liquidationPx: hlLiquidationPxForSide(hlLiquidationPx, hlNetSideByCoin, result.Symbol, hlPosSide)}, notifier, logger)
				manageStopEvidence = ratchetStopEvidence{Ran: true, Live: true, Confirmed: updateConfirmed, Result: slUpdate}
				mu.Lock()
				stopAt := hlStep.historyLenLocked()
				if immediateFill, fillPx := applyTrailingStopUpdateResult(stratState, result.Symbol, hlPosSide, hlStopLossOID, newHighWater, updateConfirmed, slUpdate, "trailing_stop_loss_immediate", logger, slEffectiveQty); immediateFill {
					hlStep.bindWindowLocked(stopAt, fmt.Sprintf("[%s] LIVE TRAILING SL %s @ $%.2f", sc.ID, result.Symbol, fillPx))
				}
				if forceResize && updateConfirmed {
					if p, ok := stratState.Positions[result.Symbol]; ok && p != nil {
						p.ScaleInResizePending = false
					}
				}
				mu.Unlock()
			}
		}
		completeAndNotifyRatchetTrigger(notifier, sc.NotifyRatchetTriggersEnabled(cfg), manageRatchetAlert, sc, stratState, result.Symbol, mu, manageStopEvidence)
		if !hyperliquidIsLive(sc.Args) && result.Signal == 0 && hlPosQty > 0 && effectiveTrailingStopPct(sc, hlPosSnapshot) <= 0 {
			newTrigger, breach, breachPx, stopReason := runHyperliquidFixedStopLossPaper(sc, hlPosSide, hlPosSnapshot, price, hlStopLossTriggerPx)
			mu.Lock()
			if pos, ok3 := stratState.Positions[result.Symbol]; ok3 && pos.Quantity > 0 && pos.Side == hlPosSide {
				if newTrigger > 0 && pos.StopLossTriggerPx == 0 {
					pos.StopLossTriggerPx = newTrigger
					stampOpenTradeWithProtectionSnapshot(stratState, stratDB, sc, result.Symbol, pos)
					logger.Info("Paper SL armed @ $%.4f (%s, anchor $%.4f)", newTrigger, stopReason, pos.riskAnchorPrice())
				}
				if breach {
					stopAt := hlStep.historyLenLocked()
					breachPx = paperStopBookPx(hlPosSide, breachPx)
					if recordPerpsStopLossClose(stratState, result.Symbol, breachPx, stopReason, logger) {
						hlStep.bindWindowLocked(stopAt, fmt.Sprintf("[%s] %s %s @ $%.2f", sc.ID, paperStopLossDetailLabel(stopReason), result.Symbol, breachPx))
					}
				}
			}
			mu.Unlock()
		}
		if hyperliquidIsLive(sc.Args) && result.Signal == 0 && hlPosQty > 0 && sc.StopLossATRMult != nil && *sc.StopLossATRMult > 0 && hlStopLossOID == 0 && hlStopLossTriggerPx == 0 && (hlPosSnapshot == nil || !hlPosSnapshot.SLAfterMoved) {
			triggerPx := fixedStopLossATRTriggerPx(sc, hlPosSide, hlPosSnapshot)
			clampOffendingPx := 0.0
			clampedTriggerPx := 0.0
			clampArmAction := hlLiquidationActionRearmFailed
			armLiqPx := hlLiquidationPxForSide(hlLiquidationPx, hlNetSideByCoin, result.Symbol, hlPosSide)
			if clamped, wasClamped := clampStopInsideLiquidation(hlPosSide, triggerPx, armLiqPx); wasClamped {
				logger.Warn("#1450 fixed ATR SL for %s would rest past liquidation $%.4f; tightening $%.4f -> $%.4f",
					result.Symbol, armLiqPx, triggerPx, clamped)
				clampOffendingPx = triggerPx
				clampedTriggerPx = clamped
				triggerPx = clamped
			}
			if triggerPx > 0 {
				q := hlStopQty{Qty: hlPosQty, Fresh: true}
				if hlCycle != nil {
					q = hlCycle.StopQty(sc, result.Symbol, hlPosSide, hlPosQty, hlStopArmed, hlPeerSame, hlPeerOppQty)
				}
				slEffectiveQty, capped, place := hlReplaceQty(q, hlPosQty)
				if !place {
					slEffectiveQty = 0
				}
				if capped && place {
					logger.Warn("fixed ATR SL arm: virtual qty %.6f > chain share %.6f for %s; capping", hlPosQty, slEffectiveQty, result.Symbol)
				}
				if place {
					slResult, ok2 := hyperliquidArmFixedATRStopLossLive(sc, result.Symbol, hlPosSide, slEffectiveQty, triggerPx, notifier, logger)
					clampArmAction = hlLiquidationArmClampAction(slResult, ok2)
					if ok2 && slResult != nil {
						mu.Lock()
						if pos, ok3 := stratState.Positions[result.Symbol]; ok3 && pos.Quantity > 0 && pos.Side == hlPosSide && pos.StopLossOID == 0 {
							if slResult.StopLossFilledImmediately && slResult.StopLossTriggerPx > 0 {
								stopAt := hlStep.historyLenLocked()
								if recordPerpsStopLossCloseQty(stratState, result.Symbol, hlPlacedStopQty(slEffectiveQty, slResult.StopLossSize), slResult.StopLossTriggerPx, "stop_loss_atr_immediate", logger) {
									hlStep.bindWindowLocked(stopAt, fmt.Sprintf("[%s] LIVE FIXED ATR SL %s @ $%.2f", sc.ID, result.Symbol, slResult.StopLossTriggerPx))
								}
							} else if slResult.StopLossOID > 0 {
								pos.StopLossOID = slResult.StopLossOID
								pos.StopLossTriggerPx = slResult.StopLossTriggerPx
								stampOpenTradeWithProtectionSnapshot(stratState, stratDB, sc, result.Symbol, pos)
								logger.Info("Fixed ATR SL armed oid=%d @ $%.4f", slResult.StopLossOID, slResult.StopLossTriggerPx)
							} else if slResult.StopLossOutcomeUnknown && slResult.StopLossTriggerPx > 0 {
								pos.StopLossTriggerPx = slResult.StopLossTriggerPx
								stampOpenTradeWithProtectionSnapshot(stratState, stratDB, sc, result.Symbol, pos)
								logger.Warn("Fixed ATR SL outcome unreadable for %s: requested trigger $%.4f recorded (oid unknown)", result.Symbol, slResult.StopLossTriggerPx)
							}
						}
						mu.Unlock()
					}
				}
			}
			if clampedTriggerPx > 0 {
				notifyHLStopPastLiquidation(sc, result.Symbol, hlPosSide, clampOffendingPx, clampedTriggerPx, armLiqPx, clampArmAction, notifier, logger, tradingNow().UTC())
			}
		}
		if floorOutcome == hlSharedCloseFloorHold {
			mu.Lock()
			stampSharedCloseHold(stratState, result.Symbol, floorRemainderUSD, hlSharedCloseHoldPeerBusy)
			mu.Unlock()
		}
		if hyperliquidIsLive(sc.Args) && result.Signal == 0 && hlPosQty > 0 {
			syncAt := hlStep.historyLen(mu)
			if _, fillPx := runHyperliquidProtectionSync(sc, stratState, stratDB, result.Symbol, mu, notifier, logger, "HL protection synced", hlReconcileFillHintsJSON, hlLiquidationPx, hlNetSideByCoin, hlProtectionGuardFull, hlCycle); fillPx > 0 {
				hlStep.bindWindow(mu, syncAt, fmt.Sprintf("[%s] LIVE PROTECTION SYNC SL %s @ $%.2f", sc.ID, result.Symbol, fillPx))
			}
			postTPAt := hlStep.historyLen(mu)
			if _, slFills, slDetail := runPostTPStopLossAdjustment(sc, stratState, result.Symbol, price, cfg, mu, notifier, logger, hlCycle, hlLiquidationPx, hlNetSideByCoin); slFills > 0 {
				hlStep.bindWindow(mu, postTPAt, slDetail)
			}
		}
		if !hyperliquidIsLive(sc.Args) && result.Signal == 0 && hlPosQty > 0 {
			runPaperHLQuietCycleMaintenance(sc, stratState, stratDB, result.Symbol, price, cfg, mu, notifier, logger, hlStep)
		}
		scaleInAddQty := 0.0
		if result.Signal != 0 && sc.Type == "perps" && sc.AllowScaleIn {
			defOpenNotional := PerpsOpenNotional(hlScaleInCash, EffectiveSizingLeverage(sc), EffectiveExchangeLeverage(sc), EffectiveMarginPerTradeUSD(sc))
			snap := scaleInSnapshot{Side: hlPosSide, Quantity: hlPosQty, AvgCost: hlAvgCost, EntryATR: hlEntryATR, ScaleInCount: hlScaleInCount, AddedNotionalUSD: hlAddedNotionalUSD, LastAddPrice: hlLastAddPrice}
			if q, okAdd, reason := perpsScaleInDecision(sc, snap, result.Signal, price, defOpenNotional); okAdd {
				scaleInAddQty = q * hurstDecision.OpenSizeMult()
			} else if reason != "" && reason != "not a same-direction add" {
				logger.Info("Scale-in not taken for %s: %s", result.Symbol, reason)
			}
		}
		var execRearm hlCloseRearmContext
		var hlEntryIntent *entryIntent
		if hyperliquidIsLive(sc.Args) && result.Signal != 0 && entryAdm.admitted() && ((entryAdm.Kind == entryIncreaseAdd && scaleInAddQty > 0) || (entryAdm.Kind == entryIncreaseOpen && hlPosQty <= 0)) {
			in, reserveErr := reserveLiveEntryIntent(store, sc, stratState, mu, entryAdm, entryProviderHyperliquid, entryIntentRequest{Kind: entryAdm.Kind.String(), Signal: result.Signal, PrevPosQty: hlPosQty, PrevPosSide: hlPosSide, PrevAvgCost: hlAvgCost, Price: price, EntryATR: indicatorsATRValue(result.Indicators)}, tradingNow())
			if reserveErr != nil {
				logger.Warn("Closed-bar entry: %s held — the live attempt could not be saved before sending: %v (#1769)", signalStr, reserveErr)
				liveExecFailed = true
			} else {
				hlEntryIntent = in
				result.EntrySubmitGate = hyperliquidEntrySubmitGate(store, stratState, mu, in)
			}
		}
		if hyperliquidIsLive(sc.Args) && result.Signal != 0 && !liveExecFailed {
			walletSnapshot := hlExecuteSnapshotForCoin(hlPositions, result.Symbol)
			if scaleInAddQty > 0 {
				if er, ok2 := runHyperliquidScaleInOrder(sc, result, scaleInAddQty, walletSnapshot, notifier, logger); ok2 {
					execResult = er
				} else {
					liveExecFailed = true
					outcome, outcomeDetail := hyperliquidExecuteOutcome(er)
					finishFailedLiveEntry(store, sc, stratState, mu, hlEntryIntent, result.LiveOrderSubmitted, outcome, outcomeDetail, notifier, logger)
				}
			} else {
				closeCtx := hlCloseContext{PeerSameQty: hlPeerSameQty, PeerOppQty: hlPeerOppQty, OnChain: hlOnChainCoinView{Known: hlStateFetched, AbsQty: hlOnChainAbsQty, NetSide: hlNetSideByCoin}, Refetch: hlOnChainRefetcher(hlAddr)}
				execRearm = hlCloseRearmContext{Price: price, PrevStopOID: hlStopLossOID, PrevTPOIDs: cloneInt64s(hlTPOIDs), PrevTriggerPx: hlStopLossTriggerPx, PrevHighWater: hlStopLossHighWaterPx, FillHintsJSON: hlReconcileFillHintsJSON, LiqPxByCoin: hlLiquidationPx, NetSideByCoin: hlNetSideByCoin, Backing: hlCloseBacking{PeerSameQty: hlPeerSameQty, PeerOppQty: hlPeerOppQty, PeerSame: hlPeerSame, Refetch: closeCtx.Refetch}}
				er, ok2 := runHyperliquidExecuteOrder(sc, result, price, hlCash, hlPoolBalanceKnown, hlPosQty, hlPosSide, hlAvgCost, hlPosLeverage, hlStopLossOID, hlTPOIDs, hlReconcileAll, walletSnapshot, closeCtx, hurstDecision, notifier, logger)
				switch {
				case result.SharedCloseStrandedUSD > 0:
					mu.Lock()
					stampSharedCloseHold(stratState, result.Symbol, result.SharedCloseStrandedUSD, hlSharedCloseHoldVenueReject)
					mu.Unlock()
				case result.SharedCloseEscalateFailedUSD > 0:
					if hlSharedCloseHoldReason == hlSharedCloseHoldEscalateFail {
						mu.Lock()
						stampSharedCloseHold(stratState, result.Symbol, result.SharedCloseEscalateFailedUSD, hlSharedCloseHoldVenueReject)
						mu.Unlock()
						logger.Error("Escalated whole-position close %s failed again ($%.2f) — holding the close whatever the rejection wording and alerting once", result.Symbol, result.SharedCloseEscalateFailedUSD)
						notifySharedCloseStranded(notifier, sc, result.Symbol, result.SharedCloseEscalateFailedUSD, fmt.Sprintf("Every peer is flat, but the venue refused the whole-position reduce-only close on consecutive cycles (%s).", result.SharedCloseEscalateFailureError), hlSharedCloseHoldVenueReject)
					} else {
						mu.Lock()
						stampSharedCloseHold(stratState, result.Symbol, result.SharedCloseEscalateFailedUSD, hlSharedCloseHoldEscalateFail)
						mu.Unlock()
						logger.Warn("Escalated whole-position close %s failed ($%.2f) — retrying once next cycle before holding on an unrecognised rejection", result.Symbol, result.SharedCloseEscalateFailedUSD)
					}
				case floorOutcome == hlSharedCloseFloorNone && (hlSharedCloseHoldUSD != 0 || hlSharedCloseHoldReason != "") && result.CloseFraction == 1.0:
					mu.Lock()
					if clearSharedCloseHold(stratState, result.Symbol) {
						logger.Info("Stranded-remainder hold cleared for %s: the full close is again above the venue minimum", result.Symbol)
					}
					mu.Unlock()
				case floorOutcome == hlSharedCloseFloorEscalate && ok2:
					mu.Lock()
					clearSharedCloseHold(stratState, result.Symbol)
					mu.Unlock()
				}
				if ok2 {
					execResult = er
				} else {
					liveExecFailed = true
					outcome, outcomeDetail := hyperliquidExecuteOutcome(er)
					finishFailedLiveEntry(store, sc, stratState, mu, hlEntryIntent, result.LiveOrderSubmitted, outcome, outcomeDetail, notifier, logger)
					canceledOIDs := append([]int64{hlStopLossOID}, hlTPOIDs...)
					canceledOIDs = hyperliquidExecuteSucceededCancelOIDs(er, canceledOIDs)
					if len(canceledOIDs) > 0 {
						sym := hyperliquidSymbol(sc.Args)
						if sym != "" {
							mu.Lock()
							if pos, ok3 := stratState.Positions[sym]; ok3 {
								clearHyperliquidProtectionOIDsMatching(pos, canceledOIDs)
								logger.Info("cleared canceled protection OIDs=%v after live execute failed", canceledOIDs)
							}
							mu.Unlock()
						}
					}
					unfilled := hlExecuteFillOutcome(er, nil, hlPosQty)
					if hlPosQty > 0 && result.LiveOrderSubmitted && hlUnfilledCloseNeedsRearm(result.LiveOrderCancelRequested, unfilled, canceledOIDs) {
						execRearm.Backing.PreSend = result.SizedClosePreSend
						rearmAt := hlStep.historyLen(mu)
						if extraTrades, slDetail := rearmAfterSizedClose(sc, stratState, stratDB, result.Symbol, hlPosSide, hlPosQty, unfilled, execRearm, mu, notifier, logger); extraTrades > 0 {
							hlStep.bindWindow(mu, rearmAt, slDetail)
						}
					}
				}
			}
		}
		if result.Signal != 0 && hlPaperSyntheticFill(sc, execResult != nil) {
			hlLotMetadata.Ensure(result.Symbol)
		}
		if !liveExecFailed {
			mu.Lock()
			var openTrade *Trade
			var ratchetAlert *RatchetTriggerAlert
			var execTrades int
			var execDetail string
			execAt := hlStep.historyLenLocked()
			endEntryBooking := beginEntryBookingLocked(stratState, entryAdm, hlEntryIntent)
			if scaleInAddQty > 0 {
				execTrades, execDetail, openTrade, ratchetAlert = executeHyperliquidScaleInDeferredOpen(sc, stratState, result, execResult, signalStr, price, scaleInAddQty, logger)
			} else {
				execTrades, execDetail, openTrade, ratchetAlert = executeHyperliquidResultDeferredOpen(sc, stratState, result, execResult, signalStr, price, cfg.Regime, cfg, hurstDecision, logger)
			}
			if openTrade != nil {
				var pos *Position
				if p, ok := stratState.Positions[result.Symbol]; ok {
					pos = p
				}
				recordPositionOpen(stratState, sc, openTrade, pos)
			}
			finishEntryBookingLocked(store, stratState, entryAdm, hlEntryIntent, execAt, endEntryBooking(), hyperliquidEntryEvidence(execResult), logger)
			hlStep.bindExecuteLocked(execAt, execDetail)
			mu.Unlock()
			var postTradeStopEvidence ratchetStopEvidence
			ratchetWalkerOwnedByScaleIn := scaleInAddQty > 0 && execResult != nil && execTrades > 0
			if ratchetAlert != nil && !ratchetWalkerOwnedByScaleIn {
				walkerAt := hlStep.historyLen(mu)
				extraTrades, slDetail, ev := runTrailingStopUpdateAfterRatchetTighten(sc, stratState, result.Symbol, price, hlCycle, hlLiquidationPx, hlNetSideByCoin, mu, notifier, logger)
				postTradeStopEvidence = ev
				if extraTrades > 0 {
					hlStep.bindWindow(mu, walkerAt, slDetail)
				}
			}
			sizedCloseShortFill := result.SizedCloseBookedQty > 0
			if execResult != nil && execTrades > 0 {
				syncAt := hlStep.historyLen(mu)
				if sizedCloseShortFill {
					logger.Info("Sized close %s filled short; the remainder's protection is re-armed at the backed size instead of the post-trade sync", result.Symbol)
				} else if _, fillPx := runHyperliquidProtectionSync(sc, stratState, stratDB, result.Symbol, mu, notifier, logger, "HL protection synced after trade", hlReconcileFillHintsJSON, hlLiquidationPx, hlNetSideByCoin, hlProtectionGuardFull, hlCycle); fillPx > 0 {
					hlStep.bindWindow(mu, syncAt, fmt.Sprintf("[%s] LIVE PROTECTION SYNC SL %s @ $%.2f", sc.ID, result.Symbol, fillPx))
				}
				postTPAt := hlStep.historyLen(mu)
				if _, slFills, slDetail := runPostTPStopLossAdjustment(sc, stratState, result.Symbol, price, cfg, mu, notifier, logger, hlCycle, hlLiquidationPx, hlNetSideByCoin); slFills > 0 {
					hlStep.bindWindow(mu, postTPAt, slDetail)
				}
				if scaleInAddQty > 0 {
					filledAddQty := scaleInAddQty
					if execResult.Execution != nil && execResult.Execution.Fill != nil && execResult.Execution.Fill.TotalSz > 0 {
						filledAddQty = execResult.Execution.Fill.TotalSz
					}
					hedgeFreshExposureQty = filledAddQty
					resizeAt := hlStep.historyLen(mu)
					extraTrades, slDetail, ev := scaleInResizeTrailingSLNow(sc, stratState, result.Symbol, price, hlCycle, hlLiquidationPx, hlNetSideByCoin, ratchetAlert != nil, mu, notifier, logger)
					if ratchetWalkerOwnedByScaleIn {
						postTradeStopEvidence = ev
					}
					if extraTrades > 0 {
						hlStep.bindWindow(mu, resizeAt, slDetail)
					}
				} else {
					if !sizedCloseShortFill {
						armAt := hlStep.historyLen(mu)
						if extraTrades, slDetail := armTrailingStopAtOpenNow(sc, stratState, result.Symbol, price, hlCycle, mu, notifier, logger); extraTrades > 0 {
							hlStep.bindWindow(mu, armAt, slDetail)
						}
					}
				}
				if openTrade != nil {
					mu.Lock()
					if pos, okPos := stratState.Positions[result.Symbol]; okPos && pos != nil && pos.Quantity > 0 {
						stampOpenTradeWithProtectionSnapshot(stratState, stratDB, sc, result.Symbol, pos)
					}
					mu.Unlock()
				}
				if scaleInAddQty <= 0 && openTrade != nil && openTrade.Quantity > 0 {
					hedgeFreshExposureQty = openTrade.Quantity
				}
			}
			if sizedCloseShortFill {
				execRearm.Backing.PreSend = result.SizedClosePreSend
				rearmAt := hlStep.historyLen(mu)
				if extraTrades, slDetail := rearmAfterSizedClose(sc, stratState, stratDB, result.Symbol, hlPosSide, hlPosQty, hlCloseFillOutcome{Filled: result.SizedCloseBookedQty, Known: true}, execRearm, mu, notifier, logger); extraTrades > 0 {
					hlStep.bindWindow(mu, rearmAt, slDetail)
				}
			}
			if execResult == nil && !hyperliquidIsLive(sc.Args) && result.Signal != 0 && execTrades > 0 {
				runPaperPostTPStopLossAdjustment(sc, stratState, result.Symbol, price, cfg, mu, notifier, logger)
			}
			if paperHLHeldPartialCloseNeedsQuietMaintenance(sc, result, execTrades, hlPosQty) {
				runPaperHLQuietCycleMaintenance(sc, stratState, stratDB, result.Symbol, price, cfg, mu, notifier, logger, hlStep)
			}
			completeAndNotifyRatchetTrigger(notifier, sc.NotifyRatchetTriggersEnabled(cfg), ratchetAlert, sc, stratState, result.Symbol, mu, postTradeStopEvidence)
		}
		if hlProfileResolved {
			mu.Lock()
			stampPositionProfileIfOpened(stratState, result.Symbol, hlProfileActive)
			updateStrategyProfileState(stratState, hlProfileNext)
			mu.Unlock()
		}
		if replaySourceID := replayMirrorSourceID(sc); replaySourceID != "" && decisionLog != nil {
			mu.Lock()
			sourceReset := syncReplayMirrorWatermarkSource(sc, stratState, replaySourceID, logger)
			var sourceResetSaveErr error
			if sourceReset {
				sourceResetSaveErr = store.SaveStrategyBook(stratState)
			}
			mu.Unlock()
			if sourceResetSaveErr != nil {
				logger.Error("Replay mirror: state save failed after resetting the watermark for source %q: %v (#1510)", replaySourceID, sourceResetSaveErr)
			}
			pending, perr := decisionLog.PendingDecisions(replaySourceID)
			switch {
			case perr != nil:
				logger.Error("Replay mirror: failed to read decision log: %v — holding last replayed state (#1431)", perr)
			case len(pending) > 0:
				mu.Lock()
				replayAt := hlStep.historyLenLocked()
				appliedIDs, replayTrades, replayDetails, driftDMs := applyReplayedLiveDecisions(sc, stratState, pending, price, result, cfg, logger)
				hlStep.bindWindowLocked(replayAt, replayDetails...)
				var saveErr error
				if len(appliedIDs) > 0 {
					saveErr = store.SaveStrategyBook(stratState)
				}
				mu.Unlock()
				sendReplayDriftWarns(driftDMs)
				if replayTrades > 0 {
					runPaperPostTPStopLossAdjustment(sc, stratState, result.Symbol, price, cfg, mu, notifier, logger)
				}
				if len(appliedIDs) > 0 {
					switch {
					case saveErr != nil:
						logger.Error("Replay mirror: state save failed after applying %d decision(s): %v — rows stay pending for retry (#1431)", len(appliedIDs), saveErr)
					default:
						if err := decisionLog.MarkDecisionsApplied(appliedIDs); err != nil {
							logger.Error("Replay mirror: failed to mark %d decision(s) applied: %v (#1431)", len(appliedIDs), err)
						}
					}
				}
			}
		}
		if HedgeEnabled(sc) {
			runHedgeSync(sc, stratState, mu, defaultHedgeExecutor(), hedgeSyncInputs{
				PrimaryPx:         price,
				HedgePx:           prices[hedgeCoin(sc)],
				FreshExposureQty:  hedgeFreshExposureQty,
				PrimaryCancelOIDs: hedgeUnwindCancelOIDs(stratState, mu, result.Symbol),
				PrimaryPeers:      hedgePrimaryPeersFor(state.Strategies, hlReconcileAll, stratState, mu, sc.ID, result.Symbol),
				Live:              hyperliquidIsLive(sc.Args),
			}, notifier, logger)
		}
		trades, detail = hlStep.finish(mu, notifier, cfg.Regime, logger)
	}
	return hlPerpsStepOutcome{Ran: hlStep != nil, Trades: trades, Detail: detail, Step: hlStep}
}

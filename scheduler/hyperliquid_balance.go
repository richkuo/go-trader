package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

type HLPosition struct {
	Coin          string
	Size          float64
	EntryPrice    float64
	Leverage      float64
	MarginMode    string
	UnrealizedPnL float64
	LiquidationPx float64
}

type hyperliquidTradeAlertState struct {
	sc       StrategyConfig
	ss       *StrategyState
	baseline int
	count    int
}

func hlExecuteSnapshotForCoin(positions []HLPosition, coin string) hlExecuteSnapshot {
	if coin == "" {
		return hlExecuteSnapshot{}
	}
	for _, p := range positions {
		if p.Coin != coin {
			continue
		}
		if p.Leverage < 1 || (p.MarginMode != "isolated" && p.MarginMode != "cross") {
			return hlExecuteSnapshot{}
		}
		return hlExecuteSnapshot{
			AccountLeverage:   int(p.Leverage),
			AccountMarginMode: p.MarginMode,
		}
	}
	return hlExecuteSnapshot{}
}

func hlReconcileSLFillConfirmed(lookup HLFillLookup, useFillFee bool, stopLossOID int64) bool {
	return useFillFee && lookup.OID == stopLossOID && lookup.FilledQty > 1e-9
}

func hlReconcileExternalClosePx(mark float64, lookup HLFillLookup, useFillFee bool) float64 {
	if useFillFee && lookup.Px > 0 {
		return lookup.Px
	}
	return mark
}

var hlMainnetURL = "https://api.hyperliquid.xyz"

var hyperliquidLiveCloseScript = "shared_scripts/close_hyperliquid_position.py"

type HyperliquidLiveCloser func(symbol string, partialSz *float64, cancelStopLossOIDs []int64) (*HyperliquidCloseResult, error)

func defaultHyperliquidLiveCloser(symbol string, partialSz *float64, cancelStopLossOIDs []int64) (*HyperliquidCloseResult, error) {
	result, stderr, err := RunHyperliquidClose(hyperliquidLiveCloseScript, symbol, partialSz, cancelStopLossOIDs)
	if stderr != "" {
		fmt.Fprintf(os.Stderr, "[hl-close] %s stderr: %s\n", symbol, stderr)
	}
	return result, err
}

func defaultHyperliquidForceCloseCloser(symbol string, partialSz *float64, cancelStopLossOIDs []int64) (*HyperliquidCloseResult, error) {
	result, stderr, err := RunHyperliquidCloseCancelAfterFill(hyperliquidLiveCloseScript, symbol, partialSz, cancelStopLossOIDs)
	if stderr != "" {
		fmt.Fprintf(os.Stderr, "[hl-force-close] %s stderr: %s\n", symbol, stderr)
	}
	return result, err
}

func fetchHyperliquidBalance(accountAddress string) (float64, error) {
	payload := map[string]string{
		"type": "clearinghouseState",
		"user": accountAddress,
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return 0, fmt.Errorf("marshal request: %w", err)
	}

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Post(hlMainnetURL+"/info", "application/json", bytes.NewReader(body))
	if err != nil {
		return 0, fmt.Errorf("http request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("http %d from %s", resp.StatusCode, hlMainnetURL)
	}

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return 0, fmt.Errorf("read response: %w", err)
	}

	var result struct {
		MarginSummary struct {
			AccountValue string `json:"accountValue"`
		} `json:"marginSummary"`
	}
	if err := json.Unmarshal(data, &result); err != nil {
		return 0, fmt.Errorf("parse response: %w", err)
	}

	val, err := strconv.ParseFloat(result.MarginSummary.AccountValue, 64)
	if err != nil {
		return 0, fmt.Errorf("parse accountValue %q: %w", result.MarginSummary.AccountValue, err)
	}
	return val, nil
}

var okxBalanceScript = "shared_scripts/fetch_okx_balance.py"

func defaultSharedWalletBalance(platform string) (float64, error) {
	switch platform {
	case "hyperliquid":
		addr := os.Getenv("HYPERLIQUID_ACCOUNT_ADDRESS")
		if addr == "" {
			return 0, fmt.Errorf("HYPERLIQUID_ACCOUNT_ADDRESS not set")
		}
		return fetchHyperliquidBalance(addr)
	case "okx":
		if os.Getenv("OKX_API_KEY") == "" {
			return 0, fmt.Errorf("OKX_API_KEY not set")
		}
		result, stderr, err := RunOKXFetchBalance(okxBalanceScript)
		if stderr != "" {
			fmt.Fprintf(os.Stderr, "[okx-balance] stderr: %s\n", stderr)
		}
		if err != nil {
			return 0, err
		}
		return result.Balance, nil
	}
	return 0, fmt.Errorf("no shared-wallet balance fetcher for platform %q", platform)
}

func defaultOKXEquitySnapshot() (eq, upnl float64, err error) {
	if os.Getenv("OKX_API_KEY") == "" {
		return 0, 0, fmt.Errorf("OKX_API_KEY not set")
	}
	result, stderr, err := RunOKXFetchBalance(okxBalanceScript)
	if stderr != "" {
		fmt.Fprintf(os.Stderr, "[okx-balance] stderr: %s\n", stderr)
	}
	if err != nil {
		return 0, 0, err
	}
	return result.Balance, result.UnrealizedPnL, nil
}

func syncHyperliquidLiveCapital(sc *StrategyConfig) {
}

func fetchHyperliquidState(accountAddress string) (float64, []HLPosition, error) {
	payload := map[string]string{
		"type": "clearinghouseState",
		"user": accountAddress,
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return 0, nil, fmt.Errorf("marshal request: %w", err)
	}

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Post(hlMainnetURL+"/info", "application/json", bytes.NewReader(body))
	if err != nil {
		return 0, nil, fmt.Errorf("http request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return 0, nil, fmt.Errorf("http %d from %s", resp.StatusCode, hlMainnetURL)
	}

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return 0, nil, fmt.Errorf("read response: %w", err)
	}

	var result struct {
		MarginSummary struct {
			AccountValue string `json:"accountValue"`
		} `json:"marginSummary"`
		AssetPositions []struct {
			Position struct {
				Coin     string `json:"coin"`
				Szi      string `json:"szi"`
				EntryPx  string `json:"entryPx"`
				Leverage struct {
					Type  string      `json:"type"`
					Value json.Number `json:"value"`
				} `json:"leverage"`
				UnrealizedPnl string          `json:"unrealizedPnl"`
				LiquidationPx json.RawMessage `json:"liquidationPx"`
			} `json:"position"`
		} `json:"assetPositions"`
	}
	if err := json.Unmarshal(data, &result); err != nil {
		return 0, nil, fmt.Errorf("parse response: %w", err)
	}

	balance, err := strconv.ParseFloat(result.MarginSummary.AccountValue, 64)
	if err != nil {
		return 0, nil, fmt.Errorf("parse accountValue %q: %w", result.MarginSummary.AccountValue, err)
	}

	var positions []HLPosition
	for _, ap := range result.AssetPositions {
		szi, err := strconv.ParseFloat(ap.Position.Szi, 64)
		if err != nil || szi == 0 {
			continue
		}
		entryPx, err := strconv.ParseFloat(ap.Position.EntryPx, 64)
		if err != nil {
			fmt.Printf("[WARN] hl-sync: failed to parse entryPx %q for %s: %v\n", ap.Position.EntryPx, ap.Position.Coin, err)
		}
		lev := 1.0
		if lvStr := ap.Position.Leverage.Value.String(); lvStr != "" {
			if parsed, lerr := strconv.ParseFloat(lvStr, 64); lerr == nil && parsed > 0 {
				lev = parsed
			}
		}
		mode := ""
		switch ap.Position.Leverage.Type {
		case "isolated", "cross":
			mode = ap.Position.Leverage.Type
		}
		var uPnL float64
		if ap.Position.UnrealizedPnl != "" {
			if parsed, perr := strconv.ParseFloat(ap.Position.UnrealizedPnl, 64); perr == nil {
				uPnL = parsed
			}
		}
		liqPx := parseHLLiquidationPx(ap.Position.LiquidationPx)
		positions = append(positions, HLPosition{
			Coin:          ap.Position.Coin,
			Size:          szi,
			EntryPrice:    entryPx,
			Leverage:      lev,
			MarginMode:    mode,
			UnrealizedPnL: uPnL,
			LiquidationPx: liqPx,
		})
	}

	return balance, positions, nil
}

func parseHLLiquidationPx(raw json.RawMessage) float64 {
	s := strings.Trim(strings.TrimSpace(string(raw)), `"`)
	if s == "" || s == "null" {
		return 0
	}
	parsed, err := strconv.ParseFloat(s, 64)
	if err != nil || parsed <= 0 {
		return 0
	}
	return parsed
}

func reconcileHyperliquidPositionsForStrategy(
	sc StrategyConfig,
	stratState *StrategyState,
	sym string,
	positions []HLPosition,
	resolveFee hlReconcileFillResolver,
	logger *StrategyLogger,
	pendingAlerts *[]ProtectionFillAlert,
	pendingOrphanCloses *[]RegimeDirectionOrphanCloseJob,
) bool {
	if stratState == nil || sym == "" {
		return false
	}

	if booked := tryBookSoleOwnerTPFill(sc, stratState, sym, positions, resolveFee, logger, pendingAlerts); booked {
		reconcileHyperliquidPositionsWithResolver(stratState, sym, positions, resolveFee, logger, pendingAlerts, pendingOrphanCloses, sc)
		return true
	}

	return reconcileHyperliquidPositionsWithResolver(stratState, sym, positions, resolveFee, logger, pendingAlerts, pendingOrphanCloses, sc)
}

func stampSoleOwnerRecoveryTierConsumed(pos *Position, tierIdx int) {
	if pos == nil || tierIdx < 0 {
		return
	}
	n := len(pos.TPOIDs)
	if tierIdx >= n {
		return
	}
	if len(pos.TPArmedTiers) < n {
		ext := make([]bool, n)
		copy(ext, pos.TPArmedTiers)
		pos.TPArmedTiers = ext
	}
	pos.TPOIDs[tierIdx] = 0
	pos.TPArmedTiers[tierIdx] = true
}

func tryBookSoleOwnerTPFill(
	sc StrategyConfig,
	stratState *StrategyState,
	sym string,
	positions []HLPosition,
	resolveFee hlReconcileFillResolver,
	logger *StrategyLogger,
	pendingAlerts *[]ProtectionFillAlert,
) bool {
	statePos := stratState.Positions[sym]
	if statePos == nil || statePos.Quantity <= 0 {
		return false
	}
	if statePos.AvgCost <= 0 || statePos.EntryATR <= 0 {
		return false
	}

	var onChainPos *HLPosition
	for i := range positions {
		if positions[i].Coin == sym {
			onChainPos = &positions[i]
			break
		}
	}

	var closeQty float64
	if onChainPos == nil {
		tiers := strategyTPTiersForRegime(sc, statePos.Regime)
		tpOIDs := tpOIDsForTierCount(statePos.TPOIDs, len(tiers))
		for _, oid := range tpOIDs {
			if oid > 0 {
				return false
			}
		}
		closeQty = statePos.Quantity
	} else {
		onChainAbs := math.Abs(onChainPos.Size)
		sameDirection := (onChainPos.Size > 0 && statePos.Side == "long") ||
			(onChainPos.Size < 0 && statePos.Side == "short")
		if !sameDirection {
			return false
		}
		if onChainAbs+1e-9 >= statePos.Quantity {
			return false
		}
		closeQty = statePos.Quantity - onChainAbs
	}
	if closeQty <= 1e-9 {
		return false
	}

	if onChainPos != nil && hyperliquidAllTiersArmedAndCleared(sc, statePos) {
		onChainAbs := math.Abs(onChainPos.Size)
		if hlAttemptCloseFromArmedTPClears(stratState, sc, sym, statePos, onChainAbs, resolveFee, logger, pendingAlerts) {
			return true
		}
	}

	lookup, useFillFee := resolveFee(sym, 0, closeQty)
	logHyperliquidReconcileFillLookup(logger, sym, 0, closeQty, lookup, useFillFee)

	var soleOwnerRecoveryBook bool
	tierIdx, hasCleared := hyperliquidClearedTPTier(sc, statePos, closeQty)
	if !hasCleared {
		if onChainPos == nil || !useFillFee || lookup.OID <= 0 {
			return false
		}
		tiers := strategyTPTiersForRegime(sc, statePos.Regime)
		if len(tiers) == 0 {
			return false
		}
		tpOIDs := tpOIDsForTierCount(statePos.TPOIDs, len(tiers))
		matched := -1
		for i, oid := range tpOIDs {
			if oid > 0 && oid == lookup.OID {
				matched = i
				break
			}
		}
		if matched < 0 {
			return false
		}
		tierIdx = matched
		soleOwnerRecoveryBook = true
	}

	tpPrices := tieredTPATRPricesForRegime(sc, statePos.Side, statePos.riskAnchorPrice(), statePos.EntryATR, statePos.Regime)
	tpPrice := 0.0
	if tierIdx >= 0 && tierIdx < len(tpPrices) {
		tpPrice = tpPrices[tierIdx]
	}

	closePx := hlReconcileExternalClosePx(tpPrice, lookup, useFillFee)
	if closePx <= 0 {
		return false
	}

	exchangeOrderID := ""
	if useFillFee && lookup.OID > 0 {
		exchangeOrderID = strconv.FormatInt(lookup.OID, 10)
	}

	alertSide := statePos.Side
	if !recordPerpsExternalPartialCloseWithFillFee(
		stratState, sym, closeQty, closePx, lookup.Fee, useFillFee,
		exchangeOrderID, "hl_sync_external_partial", logger,
	) {
		return false
	}
	posAfter := stratState.Positions[sym]
	if soleOwnerRecoveryBook && posAfter != nil {
		stampSoleOwnerRecoveryTierConsumed(posAfter, tierIdx)
	}

	remaining := 0.0
	if posAfter != nil {
		remaining = posAfter.Quantity
	}
	if pendingAlerts != nil {
		*pendingAlerts = append(*pendingAlerts, ProtectionFillAlert{
			StrategyID:      sc.ID,
			Symbol:          sym,
			Side:            alertSide,
			FillType:        tpTierLabel(tierIdx),
			IsPartial:       remaining > 1e-9,
			FillPrice:       closePx,
			CloseQty:        closeQty,
			RemainingQty:    remaining,
			RealizedPnL:     lastBookedTradePnL(stratState),
			HasPnL:          true,
			ExchangeOrderID: exchangeOrderID,
		})
	}
	return true
}

func reconcileHyperliquidPositionsWithResolver(stratState *StrategyState, sym string, positions []HLPosition, resolveFee hlReconcileFillResolver, logger *StrategyLogger, pendingAlerts *[]ProtectionFillAlert, pendingOrphanCloses *[]RegimeDirectionOrphanCloseJob, sc StrategyConfig) bool {
	changed := false

	var onChainPos *HLPosition
	for i := range positions {
		if positions[i].Coin == sym {
			onChainPos = &positions[i]
			break
		}
	}

	statePos := stratState.Positions[sym]

	if onChainPos != nil && statePos != nil {
		qty := math.Abs(onChainPos.Size)
		side := "long"
		if onChainPos.Size < 0 {
			side = "short"
		}
		if statePos.Quantity != qty || statePos.Side != side {
			skipQtyResync := sc.ID != "" &&
				math.Abs(statePos.Quantity-qty) > 1e-6 &&
				hyperliquidAllTiersArmedAndCleared(sc, statePos)
			if skipQtyResync {
				logger.Info("hl-sync: %s qty drift state=%.6f %s on-chain=%.6f %s (not auto-resyncing — all TP tiers armed/cleared, #777)",
					sym, statePos.Quantity, statePos.Side, qty, side)
			} else {
				logger.Info("hl-sync: reconciled %s: state=%.6f %s → on-chain=%.6f %s @ $%.2f",
					sym, statePos.Quantity, statePos.Side, qty, side, onChainPos.EntryPrice)
				statePos.Quantity = qty
				statePos.Side = side
				statePos.AvgCost = onChainPos.EntryPrice
				changed = true
			}
		}
		if statePos.Multiplier != 1 {
			logger.Info("hl-sync: %s migrate multiplier %v → 1 (perps PnL valuation) (#254)", sym, statePos.Multiplier)
			statePos.Multiplier = 1
			changed = true
		}
		if onChainPos.Leverage > 0 && statePos.Leverage == 0 {
			logger.Info("hl-sync: %s leverage init → %v (from on-chain, legacy/zero-value position)", sym, onChainPos.Leverage)
			statePos.Leverage = onChainPos.Leverage
			changed = true
		}
		if pendingOrphanCloses != nil && sc.ID != "" {
			if conflict, currentRegime, effectiveDir := perpsRegimeDirectionOrphanConflict(stratState, sc, statePos); conflict {
				logger.Warn("hl-sync: %s regime/direction orphan — %s qty=%.6f conflicts with current regime %q (effective_direction=%q); queuing auto-close (#822)",
					sym, statePos.Side, statePos.Quantity, currentRegime, effectiveDir)
				*pendingOrphanCloses = append(*pendingOrphanCloses, RegimeDirectionOrphanCloseJob{
					StrategyID:    sc.ID,
					Symbol:        sym,
					CloseQty:      statePos.Quantity,
					CancelOIDs:    hyperliquidProtectionCancelOIDs(statePos),
					PosSide:       statePos.Side,
					CurrentRegime: currentRegime,
					EffectiveDir:  effectiveDir,
				})
			}
		}
	} else if onChainPos == nil && statePos != nil {
		logger.Info("hl-sync: %s position (%.6f %s) no longer on-chain, removing",
			sym, statePos.Quantity, statePos.Side)
		if hlAttemptCloseFromTPFills(stratState, sym, statePos, resolveFee, logger, pendingAlerts) {
			return true
		}
		if statePos.StopLossOID > 0 && statePos.StopLossTriggerPx > 0 {
			lookup, useFillFee := resolveFee(sym, statePos.StopLossOID, statePos.Quantity)
			oidStr := strconv.FormatInt(statePos.StopLossOID, 10)
			logHyperliquidReconcileFillLookup(logger, sym, statePos.StopLossOID, statePos.Quantity, lookup, useFillFee)
			slConfirmed := hlReconcileSLFillConfirmed(lookup, useFillFee, statePos.StopLossOID)
			if slConfirmed {
				if lookup.FilledQty < statePos.Quantity-1e-9 {
					logger.Info("hl-sync: %s SL close qty adjusted %.6f → %.6f (actual fill from userFills)", sym, statePos.Quantity, lookup.FilledQty)
					statePos.Quantity = lookup.FilledQty
				}
				if recordPerpsStopLossCloseWithFillFee(stratState, sym, statePos.StopLossTriggerPx, lookup.Fee, useFillFee, oidStr, "stop_loss", logger) {
					if pendingAlerts != nil {
						*pendingAlerts = append(*pendingAlerts, ProtectionFillAlert{
							StrategyID:      sc.ID,
							Symbol:          sym,
							Side:            statePos.Side,
							FillType:        "SL",
							IsPartial:       false,
							FillPrice:       statePos.StopLossTriggerPx,
							CloseQty:        statePos.Quantity,
							RemainingQty:    0,
							RealizedPnL:     lastBookedTradePnL(stratState),
							HasPnL:          true,
							ExchangeOrderID: oidStr,
						})
					}
					return true
				}
			} else if useFillFee {
				logger.Info("hl-sync: %s SL OID %s unfilled — routing to hl_sync_external (matched oid=%d qty=%.6f)", sym, oidStr, lookup.OID, lookup.FilledQty)
			}
		}
		lookupExt, useFillFeeExt := resolveFee(sym, 0, statePos.Quantity)
		logHyperliquidReconcileFillLookup(logger, sym, 0, statePos.Quantity, lookupExt, useFillFeeExt)
		closePx := hlReconcileExternalClosePx(0, lookupExt, useFillFeeExt)
		if closePx <= 0 {
			closePx = statePos.AvgCost
			logger.Info("hl-sync: %s external close has no price source — booking at avg cost $%.4f (zero PnL)", sym, closePx)
		}
		if !recordPerpsExternalCloseWithFillFee(stratState, sym, closePx, lookupExt.Fee, useFillFeeExt, "", "hl_sync_external", logger) {
			recordClosedPosition(stratState, statePos, 0, 0, "hl_sync_external", time.Now().UTC())
			delete(stratState.Positions, sym)
			clearHLPerpsPositionAlertThrottles(stratState, sym)
		}
		changed = true
	}

	return changed
}

func syncHyperliquidAccountPositions(hlStrategies []StrategyConfig, state *AppState, mu *sync.RWMutex, logMgr *LogManager) bool {
	accountAddr := os.Getenv("HYPERLIQUID_ACCOUNT_ADDRESS")
	if accountAddr == "" {
		return false
	}

	_, positions, err := fetchHyperliquidState(accountAddr)
	if err != nil {
		fmt.Printf("[WARN] hl-sync: failed to fetch on-chain state: %v\n", err)
		return false
	}

	changed, _, _ := reconcileHyperliquidAccountPositions(hlStrategies, hlStrategies, state, mu, logMgr, positions, nil, accountAddr, nil, false)
	return changed
}

func reconcileHyperliquidAccountPositions(dueStrategies, allStrategies []StrategyConfig, state *AppState, mu *sync.RWMutex, logMgr *LogManager, positions []HLPosition, prices map[string]float64, accountAddress string, notifier ownerDMSender, notifyTPSLFills bool) (bool, []HyperliquidProtectionFillHint, []RegimeDirectionOrphanCloseJob) {
	resolveFee, fillHints := buildCachedHyperliquidReconcileFillResolver(accountAddress, allStrategies, state, mu, positions)

	var pendingAlerts []ProtectionFillAlert
	var pendingOrphanCloses []RegimeDirectionOrphanCloseJob
	var pendingHedgeAlerts []string
	tradeAlertStates := make(map[string]hyperliquidTradeAlertState, len(allStrategies))
	defer func() {
		for _, a := range pendingAlerts {
			notifyProtectionFill(notifier, notifyTPSLFills, a)
		}
		tradeNotifier, ok := notifier.(tradeAlertRouter)
		if ok && !isNilSender(notifier) {
			ids := make([]string, 0, len(tradeAlertStates))
			for id, alert := range tradeAlertStates {
				if alert.count > 0 {
					ids = append(ids, id)
				}
			}
			sort.Strings(ids)
			for _, id := range ids {
				alert := tradeAlertStates[id]
				sendTradeAlerts(alert.sc, alert.ss, alert.count, mu, tradeNotifier)
			}
		}
		if notifier != nil && !isNilSender(notifier) {
			for _, msg := range pendingHedgeAlerts {
				notifier.SendOwnerDM(msg)
			}
		}
	}()

	mu.Lock()
	defer mu.Unlock()

	changed := false
	for _, sc := range allStrategies {
		ss := state.Strategies[sc.ID]
		if ss == nil {
			continue
		}
		tradeAlertStates[sc.ID] = hyperliquidTradeAlertState{
			sc:       sc,
			ss:       ss,
			baseline: len(ss.TradeHistory),
		}
	}

	coinStrategies := make(map[string][]string)
	for _, sc := range allStrategies {
		sym := hyperliquidSymbol(sc.Args)
		if sym == "" {
			continue
		}
		coinStrategies[sym] = append(coinStrategies[sym], sc.ID)
	}
	strategyByID := make(map[string]StrategyConfig, len(allStrategies))
	for _, sc := range allStrategies {
		strategyByID[sc.ID] = sc
	}
	sharedCoins := make(map[string]bool)
	for coin, ids := range coinStrategies {
		sort.Strings(ids)
		coinStrategies[coin] = ids
		if len(ids) > 1 {
			sharedCoins[coin] = true
		}
	}

	for _, sc := range dueStrategies {
		ss := state.Strategies[sc.ID]
		if ss == nil {
			continue
		}
		sym := hyperliquidSymbol(sc.Args)
		if sym == "" {
			continue
		}
		if sharedCoins[sym] {
			continue
		}
		logger, err := logMgr.GetStrategyLogger(sc.ID)
		if err != nil {
			fmt.Printf("[ERROR] hl-sync: logger for %s: %v\n", sc.ID, err)
			continue
		}
		if reconcileHyperliquidPositionsForStrategy(sc, ss, sym, positions, resolveFee, logger, &pendingAlerts, &pendingOrphanCloses) {
			changed = true
		}
	}

	for _, sc := range dueStrategies {
		if !HedgeEnabled(sc) {
			continue
		}
		ss := state.Strategies[sc.ID]
		if ss == nil {
			continue
		}
		logger, err := logMgr.GetStrategyLogger(sc.ID)
		if err != nil {
			fmt.Printf("[ERROR] hl-sync: logger for %s: %v\n", sc.ID, err)
			continue
		}
		if reconcileHyperliquidHedgeLeg(sc, ss, positions, resolveFee, logger, &pendingAlerts, &pendingHedgeAlerts) {
			changed = true
		}
	}

	if len(pendingOrphanCloses) > 0 {
		sort.Slice(pendingOrphanCloses, func(i, j int) bool {
			if pendingOrphanCloses[i].StrategyID != pendingOrphanCloses[j].StrategyID {
				return pendingOrphanCloses[i].StrategyID < pendingOrphanCloses[j].StrategyID
			}
			return pendingOrphanCloses[i].Symbol < pendingOrphanCloses[j].Symbol
		})
	}

	now := time.Now().UTC()
	if state.ReconciliationGaps == nil {
		state.ReconciliationGaps = make(map[string]*ReconciliationGap)
	}
	for coin, stratIDs := range coinStrategies {
		if !sharedCoins[coin] {
			continue
		}

		var onChainPos *HLPosition
		for i := range positions {
			if positions[i].Coin == coin {
				onChainPos = &positions[i]
				break
			}
		}

		virtualQty := 0.0
		for _, id := range stratIDs {
			ss := state.Strategies[id]
			if ss == nil {
				continue
			}
			pos := ss.Positions[coin]
			if pos == nil {
				continue
			}
			if pos.Side == "long" {
				virtualQty += pos.Quantity
			} else if pos.Side == "short" {
				virtualQty -= pos.Quantity
			} else {
				fmt.Printf("[WARN] hl-sync: strategy %s coin %s has unexpected side=%q, skipping in virtual qty\n", id, coin, pos.Side)
			}
			if pos.Multiplier != 1 {
				logger, err := logMgr.GetStrategyLogger(id)
				if err != nil {
					fmt.Printf("[ERROR] hl-sync: logger for %s: %v\n", id, err)
				} else {
					logger.Info("hl-sync: %s migrate multiplier %v → 1 (shared coin) (#254)", coin, pos.Multiplier)
				}
				pos.Multiplier = 1
				changed = true
			}
			if onChainPos != nil && onChainPos.Leverage > 0 && pos.Leverage == 0 {
				logger, err := logMgr.GetStrategyLogger(id)
				if err != nil {
					fmt.Printf("[ERROR] hl-sync: logger for %s: %v\n", id, err)
				} else {
					logger.Info("hl-sync: %s leverage init → %v (shared coin, from on-chain)", coin, onChainPos.Leverage)
				}
				pos.Leverage = onChainPos.Leverage
				changed = true
			}
		}

		onChainQty := 0.0
		if onChainPos != nil {
			onChainQty = onChainPos.Size
		}
		delta := virtualQty - onChainQty

		if math.Abs(onChainQty) < 1e-6 && math.Abs(virtualQty) > 1e-6 {
			detector1ShareDenom := 0.0
			for _, id := range stratIDs {
				ss := state.Strategies[id]
				if ss == nil {
					continue
				}
				pos := ss.Positions[coin]
				if pos != nil && pos.Quantity > 0 {
					detector1ShareDenom += pos.Quantity
				}
			}
			detector1AggregateLookup, detector1AggregateUseFill := resolveFee(coin, 0, math.Abs(virtualQty))
			detector1AggregateOID := ""
			if detector1AggregateUseFill && detector1AggregateLookup.OID > 0 {
				detector1AggregateOID = strconv.FormatInt(detector1AggregateLookup.OID, 10)
			}
			detector1AggregateShare := func(qty float64) (HLFillLookup, bool, string) {
				if !detector1AggregateUseFill || detector1ShareDenom <= 0 {
					return HLFillLookup{}, false, ""
				}
				lookup, ok := splitHyperliquidFillLookupByQty(detector1AggregateLookup, qty, detector1ShareDenom)
				if !ok {
					return HLFillLookup{}, false, ""
				}
				return lookup, true, detector1AggregateOID
			}
			for _, id := range stratIDs {
				ss := state.Strategies[id]
				if ss == nil {
					continue
				}
				pos := ss.Positions[coin]
				if pos == nil {
					continue
				}
				logger, logErr := logMgr.GetStrategyLogger(id)
				if logErr != nil {
					fmt.Printf("[ERROR] hl-sync: logger for %s: %v\n", id, logErr)
				}
				if pos.StopLossOID > 0 && pos.StopLossTriggerPx > 0 {
					lookup, useFillFee := resolveFee(coin, pos.StopLossOID, pos.Quantity)
					oidStr := strconv.FormatInt(pos.StopLossOID, 10)
					logHyperliquidReconcileFillLookup(logger, coin, pos.StopLossOID, pos.Quantity, lookup, useFillFee)
					slConfirmed := hlReconcileSLFillConfirmed(lookup, useFillFee, pos.StopLossOID)
					if slConfirmed {
						if lookup.FilledQty < pos.Quantity-1e-9 {
							if logger != nil {
								logger.Info("hl-sync: %s SL close qty adjusted %.6f → %.6f (actual fill from userFills)", coin, pos.Quantity, lookup.FilledQty)
							}
							pos.Quantity = lookup.FilledQty
						}
						alertSide := pos.Side
						alertQty := pos.Quantity
						alertTriggerPx := pos.StopLossTriggerPx
						if recordPerpsStopLossCloseWithFillFee(ss, coin, pos.StopLossTriggerPx, lookup.Fee, useFillFee, oidStr, "hl_sync_stop_loss", logger) {
							changed = true
							pendingAlerts = append(pendingAlerts, ProtectionFillAlert{
								StrategyID:      id,
								Symbol:          coin,
								Side:            alertSide,
								FillType:        "SL",
								IsPartial:       false,
								FillPrice:       alertTriggerPx,
								CloseQty:        alertQty,
								RemainingQty:    0,
								RealizedPnL:     lastBookedTradePnL(ss),
								HasPnL:          true,
								ExchangeOrderID: oidStr,
							})
						}
					} else {
						if useFillFee && logger != nil {
							logger.Info("hl-sync: %s Detector 1 SL OID %s unfilled — routing external (matched oid=%d qty=%.6f)", coin, oidStr, lookup.OID, lookup.FilledQty)
						} else if logger != nil {
							logger.Info("hl-sync: %s Detector 1 SL OID %s unfilled — routing external (userFills miss)", coin, oidStr)
						}
						lookupExt, useFillFeeExt, oidExt := detector1AggregateShare(pos.Quantity)
						if !useFillFeeExt {
							lookupExt, useFillFeeExt = resolveFee(coin, 0, pos.Quantity)
							if useFillFeeExt && lookupExt.OID > 0 {
								oidExt = strconv.FormatInt(lookupExt.OID, 10)
							}
						}
						logHyperliquidReconcileFillLookup(logger, coin, 0, pos.Quantity, lookupExt, useFillFeeExt)
						closePx := hlReconcileExternalClosePx(prices[coin], lookupExt, useFillFeeExt)
						if closePx <= 0 {
							closePx = pos.AvgCost
							if logger != nil {
								logger.Info("hl-sync: %s Detector 1 external close has no price source — booking at avg cost $%.4f (zero PnL)", coin, closePx)
							}
						}
						if recordPerpsExternalCloseWithFillFee(ss, coin, closePx, lookupExt.Fee, useFillFeeExt, oidExt, "hl_sync_external", logger) {
							changed = true
						}
					}
				} else {
					lookup, useFillFee, oidStr := detector1AggregateShare(pos.Quantity)
					if !useFillFee {
						lookup, useFillFee = resolveFee(coin, 0, pos.Quantity)
						if useFillFee && lookup.OID > 0 {
							oidStr = strconv.FormatInt(lookup.OID, 10)
						}
					}
					logHyperliquidReconcileFillLookup(logger, coin, 0, pos.Quantity, lookup, useFillFee)
					closePx := hlReconcileExternalClosePx(prices[coin], lookup, useFillFee)
					if closePx <= 0 {
						closePx = pos.AvgCost
						if logger != nil {
							logger.Info("hl-sync: %s external close has no price source — booking at avg cost $%.4f (zero PnL)", coin, closePx)
						}
					}
					if recordPerpsExternalCloseWithFillFee(ss, coin, closePx, lookup.Fee, useFillFee, oidStr, "hl_sync_external", logger) {
						changed = true
					}
				}
			}
			virtualQty = 0.0
			delta = 0.0
		} else if math.Abs(delta) > 1e-6 {
			type confirmedSLFill struct {
				id        string
				ss        *StrategyState
				pos       *Position
				logger    *StrategyLogger
				lookup    HLFillLookup
				useFill   bool
				oidStr    string
				closeQty  float64
				triggerPx float64
				side      string
			}
			var confirmedSLFills []confirmedSLFill
			slOwnerCount := 0
			expectedResidual := virtualQty
			allowPartialAttribution := true
			for _, id := range stratIDs {
				ss := state.Strategies[id]
				if ss == nil {
					continue
				}
				pos := ss.Positions[coin]
				if pos == nil || pos.StopLossOID <= 0 || pos.StopLossTriggerPx <= 0 {
					continue
				}
				slOwnerCount++
				logger, logErr := logMgr.GetStrategyLogger(id)
				if logErr != nil {
					fmt.Printf("[ERROR] hl-sync: logger for %s: %v\n", id, logErr)
				}
				lookup, useFillFee := resolveFee(coin, pos.StopLossOID, pos.Quantity)
				logHyperliquidReconcileFillLookup(logger, coin, pos.StopLossOID, pos.Quantity, lookup, useFillFee)
				if !hlReconcileSLFillConfirmed(lookup, useFillFee, pos.StopLossOID) {
					continue
				}
				closeQty := pos.Quantity
				if lookup.FilledQty < closeQty-1e-9 {
					closeQty = lookup.FilledQty
				}
				if pos.Side == "long" {
					expectedResidual -= closeQty
				} else {
					expectedResidual += closeQty
				}
				confirmedSLFills = append(confirmedSLFills, confirmedSLFill{
					id:        id,
					ss:        ss,
					pos:       pos,
					logger:    logger,
					lookup:    lookup,
					useFill:   useFillFee,
					oidStr:    strconv.FormatInt(pos.StopLossOID, 10),
					closeQty:  closeQty,
					triggerPx: pos.StopLossTriggerPx,
					side:      pos.Side,
				})
			}
			if len(confirmedSLFills) > 0 && math.Abs(onChainQty-expectedResidual) < 1e-6 {
				for _, fill := range confirmedSLFills {
					if fill.closeQty < fill.pos.Quantity-1e-9 {
						if fill.logger != nil {
							fill.logger.Info("hl-sync: %s SL close qty adjusted %.6f → %.6f (actual fill from userFills)", coin, fill.pos.Quantity, fill.closeQty)
						}
						fill.pos.Quantity = fill.closeQty
					}
					if recordPerpsStopLossCloseWithFillFee(fill.ss, coin, fill.triggerPx, fill.lookup.Fee, fill.useFill, fill.oidStr, "hl_sync_stop_loss", fill.logger) {
						changed = true
						pendingAlerts = append(pendingAlerts, ProtectionFillAlert{
							StrategyID:      fill.id,
							Symbol:          coin,
							Side:            fill.side,
							FillType:        "SL",
							IsPartial:       false,
							FillPrice:       fill.triggerPx,
							CloseQty:        fill.closeQty,
							RemainingQty:    0,
							RealizedPnL:     lastBookedTradePnL(fill.ss),
							HasPnL:          true,
							ExchangeOrderID: fill.oidStr,
						})
					}
				}
				virtualQty = expectedResidual
				delta = virtualQty - onChainQty
			} else if slOwnerCount > 1 && len(confirmedSLFills) == 0 {
				fmt.Printf("[WARN] hl-sync: %s shared-coin partial drop with %d SL owners but no confirmed SL fill; leaving reconciliation gap for operator review\n", coin, slOwnerCount)
				allowPartialAttribution = false
			} else if len(confirmedSLFills) > 0 {
				fmt.Printf("[WARN] hl-sync: %s confirmed SL fills do not explain residual (expected %.8f, on-chain %.8f); leaving reconciliation gap for operator review\n", coin, expectedResidual, onChainQty)
				allowPartialAttribution = false
			}
			if math.Abs(delta) > 1e-6 && allowPartialAttribution {
				if closeSide, closeQty, ok := hyperliquidSharedPartialCloseDrift(virtualQty, onChainQty); ok {
					var candidateID string
					var candidateSS *StrategyState
					var candidatePos *Position
					var candidateTierIdx int
					for _, id := range stratIDs {
						ss := state.Strategies[id]
						if ss == nil {
							continue
						}
						pos := ss.Positions[coin]
						if pos == nil || pos.Side != closeSide {
							continue
						}
						sc, ok := strategyByID[id]
						if !ok {
							continue
						}
						tierIdx, hasCleared := hyperliquidClearedTPTier(sc, pos, closeQty)
						dustAllArmed := !hasCleared &&
							hyperliquidAllTiersArmedAndCleared(sc, pos) &&
							closeQty > 1e-9 && closeQty < pos.Quantity-1e-6
						if !hasCleared && !dustAllArmed {
							continue
						}
						if dustAllArmed {
							tiers := strategyTPTiersForRegime(sc, positionATRRegimeLabel(pos, sc))
							if len(tiers) > 0 {
								tierIdx = len(tiers) - 1
							}
						}
						if candidateID != "" {
							candidateID, candidateSS, candidatePos = "", nil, nil
							break
						}
						candidateID, candidateSS, candidatePos, candidateTierIdx = id, ss, pos, tierIdx
					}
					if candidateID != "" && candidateSS != nil && candidatePos != nil && closeQty <= candidatePos.Quantity+1e-6 {
						sc, scOK := strategyByID[candidateID]
						logger, logErr := logMgr.GetStrategyLogger(candidateID)
						if logErr != nil {
							fmt.Printf("[ERROR] hl-sync: logger for %s: %v\n", candidateID, logErr)
						}
						targetResidual := candidatePos.Quantity - closeQty
						if targetResidual < 0 {
							targetResidual = 0
						}
						if scOK && hyperliquidAllTiersArmedAndCleared(sc, candidatePos) {
							beforeQty := candidatePos.Quantity
							if hlAttemptCloseFromArmedTPClears(candidateSS, sc, coin, candidatePos, targetResidual, resolveFee, logger, &pendingAlerts) {
								changed = true
								bookedQty := beforeQty
								if posAfter := candidateSS.Positions[coin]; posAfter != nil {
									bookedQty -= posAfter.Quantity
								}
								if closeSide == "long" {
									virtualQty -= bookedQty
								} else {
									virtualQty += bookedQty
								}
								delta = virtualQty - onChainQty
							}
						} else if mark, ok := prices[coin]; ok && mark > 0 {
							lookup, useFillFee := resolveFee(coin, 0, closeQty)
							logHyperliquidReconcileFillLookup(logger, coin, 0, closeQty, lookup, useFillFee)
							detector3OID := ""
							if useFillFee && lookup.OID > 0 {
								detector3OID = strconv.FormatInt(lookup.OID, 10)
							}
							closePx := hlReconcileExternalClosePx(mark, lookup, useFillFee)
							if recordPerpsExternalPartialCloseWithFillFee(candidateSS, coin, closeQty, closePx, lookup.Fee, useFillFee, detector3OID, "hl_sync_external_partial", logger) {
								changed = true
								if closeSide == "long" {
									virtualQty -= closeQty
								} else {
									virtualQty += closeQty
								}
								delta = virtualQty - onChainQty
								remaining := 0.0
								if posAfter := candidateSS.Positions[coin]; posAfter != nil {
									remaining = posAfter.Quantity
								}
								pendingAlerts = append(pendingAlerts, ProtectionFillAlert{
									StrategyID:      candidateID,
									Symbol:          coin,
									Side:            closeSide,
									FillType:        tpTierLabel(candidateTierIdx),
									IsPartial:       true,
									FillPrice:       closePx,
									CloseQty:        closeQty,
									RemainingQty:    remaining,
									RealizedPnL:     lastBookedTradePnL(candidateSS),
									HasPnL:          true,
									ExchangeOrderID: detector3OID,
								})
							}
						} else {
							fmt.Printf("[WARN] hl-sync: shared coin %s TP partial drift detected for %s but no mark price is available; leaving virtual qty unchanged\n", coin, candidateID)
						}
					}
				}
			}
		}

		state.ReconciliationGaps[coin] = &ReconciliationGap{
			Coin:       coin,
			OnChainQty: onChainQty,
			VirtualQty: virtualQty,
			DeltaQty:   delta,
			Strategies: stratIDs,
			UpdatedAt:  now,
		}

		if math.Abs(delta) > 0.000001 {
			fmt.Printf("[WARN] hl-sync: shared coin %s reconciliation gap: virtual=%.6f on-chain=%.6f delta=%.6f (strategies: %v)\n",
				coin, virtualQty, onChainQty, delta, stratIDs)
		}
	}

	for coin := range state.ReconciliationGaps {
		if !sharedCoins[coin] {
			delete(state.ReconciliationGaps, coin)
		}
	}

	tradedCoins := make(map[string]bool)
	for coin := range coinStrategies {
		tradedCoins[coin] = true
	}
	for _, p := range positions {
		if !tradedCoins[p.Coin] {
			qty := math.Abs(p.Size)
			side := "long"
			if p.Size < 0 {
				side = "short"
			}
			fmt.Printf("[WARN] hl-sync: unowned on-chain position: %s %.6f %s @ $%.2f (no strategy claims it)\n",
				side, qty, p.Coin, p.EntryPrice)
		}
	}

	for id, alert := range tradeAlertStates {
		count := len(alert.ss.TradeHistory) - alert.baseline
		if count > 0 {
			alert.count = count
			tradeAlertStates[id] = alert
		}
	}

	return changed, fillHints, pendingOrphanCloses
}

func hyperliquidProtectionCancelOIDs(pos *Position) []int64 {
	if pos == nil {
		return nil
	}
	var oids []int64
	oids = appendUniquePositiveStopLossOID(oids, pos.StopLossOID)
	for _, tpOID := range pos.TPOIDs {
		oids = appendUniquePositiveStopLossOID(oids, tpOID)
	}
	return oids
}

func clearHyperliquidProtectionOIDsMatching(pos *Position, cancelOIDs []int64) {
	if pos == nil {
		return
	}
	for _, cancelOID := range cancelOIDs {
		if cancelOID > 0 && pos.StopLossOID == cancelOID {
			pos.StopLossOID = 0
			pos.StopLossTriggerPx = 0
		}
		for idx, tpOID := range pos.TPOIDs {
			if cancelOID > 0 && tpOID == cancelOID {
				pos.TPOIDs[idx] = 0
				if idx < len(pos.TPArmedTiers) {
					pos.TPArmedTiers[idx] = false
				}
			}
		}
	}
}

func runRegimeDirectionOrphanCloses(
	ctx context.Context,
	state *AppState,
	strategies []StrategyConfig,
	jobs []RegimeDirectionOrphanCloseJob,
	positions []HLPosition,
	closer HyperliquidLiveCloser,
	mu *sync.RWMutex,
	ownerDM func(string),
) {
	if ctx == nil || state == nil || closer == nil || len(jobs) == 0 {
		return
	}
	ctxOverall, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()

	strategyByID := make(map[string]StrategyConfig, len(strategies))
	for _, sc := range strategies {
		strategyByID[sc.ID] = sc
	}

	for _, job := range jobs {
		if err := ctxOverall.Err(); err != nil {
			fmt.Printf("[CRITICAL] hl-regime-orphan-close: budget exhausted: %v\n", err)
			return
		}
		sc, ok := strategyByID[job.StrategyID]
		if !ok || sc.Type != "perps" || !hyperliquidIsLive(sc.Args) {
			continue
		}
		sym := job.Symbol
		if sym == "" {
			continue
		}
		var hlPeerScope []StrategyConfig
		for _, s := range strategies {
			if isHLLiveReconcilable(s) && hyperliquidSymbol(s.Args) == sym {
				hlPeerScope = append(hlPeerScope, s)
			}
		}
		if len(hlLiveStrategiesForCoin(sym, hlPeerScope)) > 1 {
			msg := fmt.Sprintf("[CRITICAL] hl-regime-orphan-close: strategy %s %s %s qty=%.6f conflicts with effective_direction=%q (regime=%q) but coin %s is SHARED with live peers — auto-close skipped to avoid touching peer exposure. MANUAL CLOSE REQUIRED (#822/#1085).",
				job.StrategyID, job.PosSide, sym, job.CloseQty, job.EffectiveDir, job.CurrentRegime, sym)
			fmt.Println(msg)
			if ownerDM != nil {
				ownerDM(msg)
			}
			continue
		}

		sz := job.CloseQty
		var onChainSigned float64
		for _, p := range positions {
			if p.Coin != sym {
				continue
			}
			onChainSigned = p.Size
			absOC := math.Abs(p.Size)
			if absOC <= 1e-15 {
				sz = 0
				break
			}
			if sz > absOC {
				sz = absOC
			}
			break
		}
		if sz <= 1e-15 {
			continue
		}
		partial := sz
		result, err := closer(sym, &partial, job.CancelOIDs)
		if err != nil {
			msg := fmt.Sprintf("[CRITICAL] hl-regime-orphan-close: strategy %s %s %s qty=%.6f failed: %v (regime=%q effective_direction=%q)",
				job.StrategyID, job.PosSide, sym, sz, err, job.CurrentRegime, job.EffectiveDir)
			fmt.Println(msg)
			if ownerDM != nil {
				ownerDM(msg)
			}
			continue
		}

		var fillSz, fillPx, fillFee float64
		var fillOID int64
		alreadyFlat := false
		if result != nil && result.Close != nil {
			alreadyFlat = result.Close.AlreadyFlat
			if result.Close.Fill != nil {
				fillSz = result.Close.Fill.TotalSz
				fillPx = result.Close.Fill.AvgPx
				fillFee = result.Close.Fill.Fee
				fillOID = result.Close.Fill.OID
			}
		}

		mu.Lock()
		if ss := state.Strategies[job.StrategyID]; ss != nil {
			if pos, ok := ss.Positions[sym]; ok && pos != nil {
				if len(job.CancelOIDs) > 0 && result != nil && result.CancelStopLossSucceeded {
					clearHyperliquidProtectionOIDsMatching(pos, job.CancelOIDs)
				}
				if !alreadyFlat && fillSz > 1e-15 && fillPx > 0 {
					applyHyperliquidCircuitCloseFill(ss, sym, fillSz, fillPx, fillFee, onChainSigned, fillOID, "regime_direction_flip")
				}
			}
		}
		mu.Unlock()

		info := fmt.Sprintf("hl-regime-orphan-close: strategy %s closed %s %s sz=%.6f (filled %.6f) — regime %q now expects %q (#822)",
			job.StrategyID, job.PosSide, sym, sz, fillSz, job.CurrentRegime, job.EffectiveDir)
		fmt.Println("[INFO] " + info)
		if ownerDM != nil {
			ownerDM(info)
		}
	}
}

func hyperliquidSharedPartialCloseDrift(virtualQty, onChainQty float64) (string, float64, bool) {
	const tol = 1e-6
	if virtualQty > tol && onChainQty > tol && onChainQty < virtualQty-tol {
		return "long", virtualQty - onChainQty, true
	}
	if virtualQty < -tol && onChainQty < -tol && onChainQty > virtualQty+tol {
		return "short", onChainQty - virtualQty, true
	}
	return "", 0, false
}

func hyperliquidAllTiersArmedAndCleared(sc StrategyConfig, pos *Position) bool {
	if pos == nil {
		return false
	}
	tiers := strategyTPTiersForRegime(sc, positionATRRegimeLabel(pos, sc))
	if len(tiers) == 0 {
		return false
	}
	tpOIDs := tpOIDsForTierCount(pos.TPOIDs, len(tiers))
	armed := tpArmedTiersForTierCount(pos.TPArmedTiers, len(tiers))
	for i := range tiers {
		if tpOIDs[i] != 0 {
			return false
		}
		if !armed[i] {
			return false
		}
	}
	return true
}

func hyperliquidTPTierIncrementalCloseQty(initialQty float64, tiers []hlProtectionTier, tierIdx int) float64 {
	if initialQty <= 0 || tierIdx < 0 || tierIdx >= len(tiers) {
		return 0
	}
	prevFrac := 0.0
	if tierIdx > 0 {
		prevFrac = tiers[tierIdx-1].Fraction
	}
	currFrac := tiers[tierIdx].Fraction
	if tierIdx == len(tiers)-1 {
		currFrac = 1.0
	}
	delta := currFrac - prevFrac
	if delta <= 0 {
		return 0
	}
	return initialQty * delta
}

func tpOIDsFromOpenTrade(s *StrategyState, sym string, tierCount int) []int64 {
	if s == nil || sym == "" || tierCount <= 0 {
		return nil
	}
	for i := len(s.TradeHistory) - 1; i >= 0; i-- {
		t := &s.TradeHistory[i]
		if t.Symbol != sym {
			continue
		}
		if t.IsClose {
			break
		}
		oids := tpOIDsForTierCount(t.TPOIDs, tierCount)
		for _, oid := range oids {
			if oid > 0 {
				return oids
			}
		}
		return oids
	}
	return nil
}

func tpOIDsForReconcileLookup(s *StrategyState, pos *Position, sym string, tierCount int) []int64 {
	live := tpOIDsForTierCount(pos.TPOIDs, tierCount)
	for _, oid := range live {
		if oid > 0 {
			return live
		}
	}
	return tpOIDsFromOpenTrade(s, sym, tierCount)
}

func hlAttemptCloseFromArmedTPClears(
	s *StrategyState,
	sc StrategyConfig,
	sym string,
	pos *Position,
	targetResidualQty float64,
	resolveFee hlReconcileFillResolver,
	logger *StrategyLogger,
	pendingAlerts *[]ProtectionFillAlert,
) bool {
	if s == nil || pos == nil || resolveFee == nil || !hyperliquidAllTiersArmedAndCleared(sc, pos) {
		return false
	}
	if pos.Quantity <= targetResidualQty+1e-9 {
		return false
	}
	if pos.StopLossOID > 0 {
		if lookup, slFilled := resolveFee(sym, pos.StopLossOID, pos.Quantity); hlReconcileSLFillConfirmed(lookup, slFilled, pos.StopLossOID) {
			return false
		}
	}
	tiers := strategyTPTiersForRegime(sc, positionATRRegimeLabel(pos, sc))
	if len(tiers) == 0 {
		return false
	}
	initQty := pos.InitialQuantity
	if initQty <= 0 {
		initQty = pos.Quantity
	}
	lookupOIDs := tpOIDsForReconcileLookup(s, pos, sym, len(tiers))
	booked := false
	for i := range tiers {
		cur := s.Positions[sym]
		if cur == nil || cur.Quantity <= targetResidualQty+1e-9 {
			break
		}
		tierQty := hyperliquidTPTierIncrementalCloseQty(initQty, tiers, i)
		if tierQty <= 1e-9 {
			continue
		}
		var lookup HLFillLookup
		var useFillFee bool
		if i < len(lookupOIDs) && lookupOIDs[i] > 0 {
			lookup, useFillFee = resolveFee(sym, lookupOIDs[i], tierQty)
		}
		if !useFillFee || lookup.Px <= 0 || lookup.FilledQty <= 0 {
			lookup, useFillFee = resolveFee(sym, 0, tierQty)
		}
		if !useFillFee || lookup.Px <= 0 || lookup.FilledQty <= 0 {
			continue
		}
		closeQty := lookup.FilledQty
		if closeQty > cur.Quantity-targetResidualQty+1e-9 {
			closeQty = cur.Quantity - targetResidualQty
		}
		if closeQty <= 1e-9 {
			continue
		}
		alertSide := cur.Side
		oidStr := ""
		if lookup.OID > 0 {
			oidStr = strconv.FormatInt(lookup.OID, 10)
		}
		logHyperliquidReconcileFillLookup(logger, sym, lookup.OID, closeQty, lookup, useFillFee)
		reason := fmt.Sprintf("hl_sync_tp%d_fill", i+1)
		detailsPrefix := fmt.Sprintf("TP%d fill close", i+1)
		logPrefix := fmt.Sprintf("TP%d fill reconciled", i+1)
		if !bookPerpsPartialCloseWithFillFee(s, sym, closeQty, lookup.Px, lookup.Fee, true, oidStr, reason, detailsPrefix, logPrefix, logger) {
			break
		}
		booked = true
		if pendingAlerts != nil {
			remaining := 0.0
			if posAfter := s.Positions[sym]; posAfter != nil {
				remaining = posAfter.Quantity
			}
			*pendingAlerts = append(*pendingAlerts, ProtectionFillAlert{
				StrategyID:      s.ID,
				Symbol:          sym,
				Side:            alertSide,
				FillType:        tpTierLabel(i),
				IsPartial:       remaining > targetResidualQty+1e-9,
				FillPrice:       lookup.Px,
				CloseQty:        closeQty,
				RemainingQty:    remaining,
				RealizedPnL:     lastBookedTradePnL(s),
				HasPnL:          true,
				ExchangeOrderID: oidStr,
			})
		}
	}
	return booked
}

func hyperliquidClearedTPTier(sc StrategyConfig, pos *Position, closeQty float64) (int, bool) {
	if pos == nil || len(pos.TPOIDs) == 0 {
		return 0, false
	}
	tiers := strategyTPTiersForRegime(sc, positionATRRegimeLabel(pos, sc))
	if len(tiers) == 0 {
		return 0, false
	}
	if len(pos.TPOIDs) < len(tiers) {
		return 0, false
	}
	tpOIDs := tpOIDsForTierCount(pos.TPOIDs, len(tiers))
	clearedIdx := -1
	hasActive := false
	for i, oid := range tpOIDs {
		if oid > 0 {
			hasActive = true
		} else if clearedIdx < 0 {
			clearedIdx = i
		}
	}
	if clearedIdx < 0 {
		return 0, false
	}
	if hasActive {
		return clearedIdx, true
	}
	if math.Abs(pos.Quantity-closeQty) <= 1e-6 {
		return len(tpOIDs) - 1, true
	}
	return 0, false
}

func hyperliquidHasClearedTPTier(sc StrategyConfig, pos *Position, closeQty float64) bool {
	_, ok := hyperliquidClearedTPTier(sc, pos, closeQty)
	return ok
}

func hlAttemptCloseFromTPFills(s *StrategyState, sym string, pos *Position, resolveFee hlReconcileFillResolver, logger *StrategyLogger, pendingAlerts *[]ProtectionFillAlert) bool {
	if s == nil || pos == nil || len(pos.TPOIDs) == 0 || resolveFee == nil {
		return false
	}
	if pos.StopLossOID > 0 {
		if lookup, slFilled := resolveFee(sym, pos.StopLossOID, pos.Quantity); hlReconcileSLFillConfirmed(lookup, slFilled, pos.StopLossOID) {
			return false
		}
	}
	type tpFill struct {
		oid     int64
		tierIdx int
		lookup  HLFillLookup
	}
	var fills []tpFill
	for i, oid := range pos.TPOIDs {
		if oid <= 0 {
			continue
		}
		lookup, ok := resolveFee(sym, oid, pos.Quantity)
		if !ok || lookup.FilledQty <= 0 || lookup.Px <= 0 {
			continue
		}
		fills = append(fills, tpFill{oid: oid, tierIdx: i, lookup: lookup})
	}
	if len(fills) == 0 {
		return false
	}
	for _, f := range fills {
		curBefore := s.Positions[sym]
		if curBefore == nil {
			break
		}
		alertSide := curBefore.Side
		oidStr := strconv.FormatInt(f.oid, 10)
		logHyperliquidReconcileFillLookup(logger, sym, f.oid, f.lookup.FilledQty, f.lookup, true)
		reason := fmt.Sprintf("hl_sync_tp%d_fill", f.tierIdx+1)
		detailsPrefix := fmt.Sprintf("TP%d fill close", f.tierIdx+1)
		logPrefix := fmt.Sprintf("TP%d fill reconciled", f.tierIdx+1)
		if !bookPerpsPartialCloseWithFillFee(s, sym, f.lookup.FilledQty, f.lookup.Px, f.lookup.Fee, true, oidStr, reason, detailsPrefix, logPrefix, logger) {
			break
		}
		if pendingAlerts != nil {
			remaining := 0.0
			if posAfter := s.Positions[sym]; posAfter != nil {
				remaining = posAfter.Quantity
			}
			*pendingAlerts = append(*pendingAlerts, ProtectionFillAlert{
				StrategyID:      s.ID,
				Symbol:          sym,
				Side:            alertSide,
				FillType:        tpTierLabel(f.tierIdx),
				IsPartial:       remaining > 1e-9,
				FillPrice:       f.lookup.Px,
				CloseQty:        f.lookup.FilledQty,
				RemainingQty:    remaining,
				RealizedPnL:     lastBookedTradePnL(s),
				HasPnL:          true,
				ExchangeOrderID: oidStr,
			})
		}
	}
	if residual := s.Positions[sym]; residual != nil {
		if logger != nil {
			logger.Warn("hl-sync: %s residual %.6f after TP fill attribution; finalizing at zero PnL", sym, residual.Quantity)
		}
		recordClosedPosition(s, residual, 0, 0, "hl_sync_external", time.Now().UTC())
		delete(s.Positions, sym)
	}
	clearHLPerpsPositionAlertThrottles(s, sym)
	return true
}

type HyperliquidLiveCloseReport struct {
	ClosedCoins  []string
	Fills        map[string]HyperliquidCloseFill
	AlreadyFlat  []string
	Errors       map[string]error
	Unconfigured []HLPosition
}

func (r HyperliquidLiveCloseReport) ConfirmedFlat() bool {
	return len(r.Errors) == 0
}

func (r HyperliquidLiveCloseReport) SortedErrorCoins() []string {
	coins := make([]string, 0, len(r.Errors))
	for c := range r.Errors {
		coins = append(coins, c)
	}
	sort.Strings(coins)
	return coins
}

func forceCloseHyperliquidLive(ctx context.Context, positions []HLPosition, hlLiveAll []StrategyConfig, hedgeCoins map[string]bool, closer HyperliquidLiveCloser, stopLossOIDsByCoin map[string][]int64) HyperliquidLiveCloseReport {
	report := HyperliquidLiveCloseReport{
		Fills:  make(map[string]HyperliquidCloseFill),
		Errors: make(map[string]error),
	}

	tradedCoins := make(map[string]bool)
	for _, sc := range hlLiveAll {
		if sym := hyperliquidRawCoin(sc); sym != "" {
			tradedCoins[strings.ToUpper(sym)] = true
		}
	}
	for coin, held := range hedgeCoins {
		if held && coin != "" {
			tradedCoins[coin] = true
		}
	}

	for _, p := range positions {
		if !tradedCoins[strings.ToUpper(p.Coin)] {
			if p.Size != 0 {
				report.Unconfigured = append(report.Unconfigured, p)
			}
			continue
		}
		if p.Size == 0 {
			report.AlreadyFlat = append(report.AlreadyFlat, p.Coin)
			continue
		}
		if err := ctx.Err(); err != nil {
			report.Errors[p.Coin] = fmt.Errorf("close budget exhausted before submit: %w", err)
			continue
		}
		var slOIDs []int64
		if stopLossOIDsByCoin != nil {
			slOIDs = stopLossOIDsByCoin[p.Coin]
		}
		result, err := closer(p.Coin, nil, slOIDs)
		if err != nil {
			report.Errors[p.Coin] = err
			continue
		}
		if result != nil && result.Close != nil && result.Close.AlreadyFlat {
			report.AlreadyFlat = append(report.AlreadyFlat, p.Coin)
			continue
		}
		if result != nil && result.Close != nil && result.Close.Fill != nil {
			report.Fills[p.Coin] = *result.Close.Fill
		}
		report.ClosedCoins = append(report.ClosedCoins, p.Coin)
	}

	return report
}

func hlLiveStrategiesForCoin(coin string, hlLiveAll []StrategyConfig) []StrategyConfig {
	target := strings.ToUpper(strings.TrimSpace(coin))
	var out []StrategyConfig
	for _, sc := range hlLiveAll {
		if hyperliquidConfiguredCoin(sc) == target {
			out = append(out, sc)
		}
	}
	return out
}

func hyperliquidConfiguredCoin(sc StrategyConfig) string {
	return strings.ToUpper(strings.TrimSpace(hyperliquidRawCoin(sc)))
}

func hyperliquidRawCoin(sc StrategyConfig) string {
	if sc.Platform != "hyperliquid" {
		return ""
	}
	if sc.Type == "manual" {
		return sc.Symbol
	}
	return hyperliquidSymbol(sc.Args)
}

type hlVirtualQuantitySnapshot map[string]map[string]float64

func snapshotHyperliquidVirtualQuantities(strategies map[string]*StrategyState, hlLiveAll []StrategyConfig) hlVirtualQuantitySnapshot {
	if len(strategies) == 0 || len(hlLiveAll) == 0 {
		return nil
	}
	out := make(hlVirtualQuantitySnapshot)
	record := func(coin, id string, qty float64) {
		if out[coin] == nil {
			out[coin] = make(map[string]float64)
		}
		out[coin][id] = qty
	}
	for _, sc := range hlLiveAll {
		ss := strategies[sc.ID]
		if ss == nil {
			continue
		}
		coin := hyperliquidRawCoin(sc)
		if coin != "" {
			pos := hlVirtualPositionFor(ss, sc, coin)
			if pos != nil && pos.Quantity > 0 {
				record(coin, sc.ID, pos.Quantity)
			}
		}
		if hCoin := hedgeCoin(sc); hCoin != "" {
			if hPos := ss.Positions[hCoin]; hPos != nil && hPos.isHedgeLeg() && hPos.Quantity > 0 {
				record(hCoin, sc.ID, hPos.Quantity)
			}
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func hlVirtualPositionFor(ss *StrategyState, sc StrategyConfig, coin string) *Position {
	if ss == nil {
		return nil
	}
	return ss.Positions[coin]
}

func computeHyperliquidCircuitCloseQty(coin, strategyID string, hlPositions []HLPosition, hlLiveAll []StrategyConfig) (qty float64, ok bool) {
	var onChain float64
	found := false
	for i := range hlPositions {
		if hlPositions[i].Coin == coin {
			onChain = hlPositions[i].Size
			found = true
			break
		}
	}
	if !found || onChain == 0 {
		return 0, false
	}
	absSzi := math.Abs(onChain)
	peers := hlLiveStrategiesForCoin(coin, hlLiveAll)
	if len(peers) > 1 {
		return 0, false
	}
	return absSzi, true
}

func hyperliquidKillSwitchFillShare(sc StrategyConfig, coin string, fillSz, fillFee float64, hlLiveAll []StrategyConfig, virtualQty hlVirtualQuantitySnapshot) (float64, float64) {
	peers := hlLiveStrategiesForCoin(coin, hlLiveAll)
	if len(peers) <= 1 {
		return fillSz, fillFee
	}
	qtyByStrategy := virtualQty[coin]
	sumQty := 0.0
	var selfQty float64
	foundSelf := false
	for _, p := range peers {
		if p.ID == sc.ID {
			foundSelf = true
		}
		qty := qtyByStrategy[p.ID]
		if qty <= 0 {
			continue
		}
		sumQty += qty
		if p.ID == sc.ID {
			selfQty = qty
		}
	}
	if !foundSelf || sumQty <= 0 || selfQty <= 0 {
		return 0, 0
	}
	ratio := selfQty / sumQty
	if ratio < 0 {
		ratio = 0
	} else if ratio > 1 {
		ratio = 1
	}
	return fillSz * ratio, fillFee * ratio
}

func applyHyperliquidKillSwitchCloseFill(s *StrategyState, sc StrategyConfig, fills map[string]HyperliquidCloseFill, hlLiveAll []StrategyConfig, virtualQty hlVirtualQuantitySnapshot) bool {
	if s == nil || sc.Platform != "hyperliquid" || (sc.Type != "perps" && sc.Type != "manual") || !hyperliquidIsLive(sc.Args) {
		return false
	}
	coin := hyperliquidRawCoin(sc)
	if coin == "" {
		return false
	}
	fill, ok := fills[coin]
	if !ok || fill.TotalSz <= 1e-15 || fill.AvgPx <= 0 {
		return false
	}
	fillSz, fillFee := hyperliquidKillSwitchFillShare(sc, coin, fill.TotalSz, fill.Fee, hlLiveAll, virtualQty)
	if fillSz <= 1e-15 {
		return false
	}
	applyHyperliquidCircuitCloseFill(s, coin, fillSz, fill.AvgPx, fillFee, 0, fill.OID, "")
	return true
}

func applyHyperliquidKillSwitchHedgeFill(s *StrategyState, sc StrategyConfig, fills map[string]HyperliquidCloseFill) bool {
	if s == nil || !HedgeEnabled(sc) || sc.Platform != "hyperliquid" || sc.Type != "perps" || !hyperliquidIsLive(sc.Args) {
		return false
	}
	coin := hedgeCoin(sc)
	if coin == "" {
		return false
	}
	pos, ok := s.Positions[coin]
	if !ok || pos == nil || !pos.isHedgeLeg() {
		return false
	}
	fill, ok := fills[coin]
	if !ok || fill.TotalSz <= 1e-15 || fill.AvgPx <= 0 {
		return false
	}
	applyHyperliquidCircuitCloseFill(s, coin, fill.TotalSz, fill.AvgPx, fill.Fee, 0, fill.OID, "kill_switch")
	return true
}

func lookupStrategyConfig(strategies []StrategyConfig, id string) *StrategyConfig {
	for i := range strategies {
		if strategies[i].ID == id {
			return &strategies[i]
		}
	}
	return nil
}

func checkAbandonedPartialModelClose(state *AppState, stratID, symbol string, mu *sync.RWMutex, ownerDM func(string)) {
	if symbol == "" {
		return
	}
	now := time.Now().UTC()
	mu.Lock()
	var msg string
	if ss := state.Strategies[stratID]; ss != nil {
		msg = warnAbandonedPartialModelClose(ss, symbol, now)
	}
	mu.Unlock()
	if msg != "" && ownerDM != nil {
		fmt.Printf("[CRITICAL] hl-circuit-close: %s\n", msg)
		ownerDM(msg)
	}
}

func firstPendingSymbol(p PendingCircuitClose) string {
	if len(p.Symbols) == 0 {
		return ""
	}
	return p.Symbols[0].Symbol
}

func runPendingHyperliquidCircuitCloses(
	ctx context.Context,
	state *AppState,
	strategies []StrategyConfig,
	hlAddr string,
	hlPositions []HLPosition,
	hlStateFetched bool,
	hlFetcher HLStateFetcher,
	closer HyperliquidLiveCloser,
	totalBudget time.Duration,
	mu *sync.RWMutex,
	ownerDM func(string),
) {
	if hlAddr == "" || closer == nil || state == nil {
		return
	}

	var hlLiveAll []StrategyConfig
	var hlCircuitPeerAll []StrategyConfig
	for _, sc := range strategies {
		if sc.Platform == "hyperliquid" && sc.Type == "perps" && hyperliquidIsLive(sc.Args) {
			hlLiveAll = append(hlLiveAll, sc)
		}
		if isHLLiveReconcilable(sc) {
			hlCircuitPeerAll = append(hlCircuitPeerAll, sc)
		}
	}

	mu.RLock()
	hasPending := false
	hasStuckCB := false
	for _, ss := range state.Strategies {
		if ss == nil {
			continue
		}
		if ss.RiskState.getPendingCircuitClose(PlatformPendingCloseHyperliquid) != nil {
			hasPending = true
		}
	}
	for _, sc := range hlLiveAll {
		ss := state.Strategies[sc.ID]
		if ss == nil {
			continue
		}
		sym := hyperliquidConfiguredCoin(sc)
		if sym == "" || len(hlLiveStrategiesForCoin(sym, hlCircuitPeerAll)) > 1 {
			continue
		}
		if ss.RiskState.getPendingCircuitClose(PlatformPendingCloseHyperliquid) == nil && ss.RiskState.CircuitBreaker {
			hasStuckCB = true
			break
		}
	}
	mu.RUnlock()

	if !hasPending && !hasStuckCB {
		return
	}

	ctxOverall, cancelOverall := context.WithTimeout(ctx, totalBudget)
	defer cancelOverall()

	positions := hlPositions
	if !hlStateFetched && hlFetcher != nil {
		pos, err := hlFetcher(hlAddr)
		if err != nil {
			fmt.Printf("[CRITICAL] hl-circuit-close: cannot fetch HL positions: %v — will retry next cycle\n", err)
			return
		}
		positions = pos
	}

	if hasStuckCB {
		recoverOrder := make([]StrategyConfig, len(hlLiveAll))
		copy(recoverOrder, hlLiveAll)
		sort.Slice(recoverOrder, func(i, j int) bool { return recoverOrder[i].ID < recoverOrder[j].ID })
		var pendingCBAlerts []string
		mu.Lock()
		for _, sc := range recoverOrder {
			ss := state.Strategies[sc.ID]
			if ss == nil {
				continue
			}
			if ss.RiskState.getPendingCircuitClose(PlatformPendingCloseHyperliquid) != nil {
				continue
			}
			if !ss.RiskState.CircuitBreaker {
				continue
			}
			sym := hyperliquidConfiguredCoin(sc)
			if sym == "" {
				continue
			}
			if len(hlLiveStrategiesForCoin(sym, hlCircuitPeerAll)) > 1 {
				continue
			}
			var symbols []PendingCircuitCloseSymbol
			qty, ok := computeHyperliquidCircuitCloseQty(sym, sc.ID, positions, hlCircuitPeerAll)
			primaryLive := ok && qty > 0
			if primaryLive {
				symbols = append(symbols, PendingCircuitCloseSymbol{Symbol: sym, Size: qty})
			}
			if hCoin := hedgeCoin(sc); hCoin != "" {
				hQty, hok := computeHyperliquidCircuitCloseQty(hCoin, sc.ID, positions, hlCircuitPeerAll)
				switch {
				case !hok || hQty <= 0:
				case primaryLive && !hedgeIsInverseOfPrimaryOnChain(sym, hCoin, positions):
					fmt.Printf("[CRITICAL] hl-circuit-close: %s declares hedge coin %s and the circuit breaker is latched, but the on-chain %s position is NOT inverse to the primary %s — refusing to close it as a hedge (it is not one this scheduler could have opened). Reconcile it manually.\n",
						sc.ID, hCoin, hCoin, sym)
					pendingCBAlerts = append(pendingCBAlerts, fmt.Sprintf(
						"🚨 **CRITICAL — hedge coin conflict under a latched circuit breaker**\nStrategy `%s`: the circuit breaker is latched and %s carries an on-chain position, but it is NOT inverse to the live %s primary, so it cannot be this strategy's hedge. It was left untouched. Reconcile it manually.",
						sc.ID, hCoin, sym))
				case !primaryLive:
					symbols = append(symbols, PendingCircuitCloseSymbol{Symbol: hCoin, Size: hQty})
					fmt.Printf("[CRITICAL] hl-circuit-close: %s primary %s is already flat on-chain but hedge coin %s still carries %.6f — closing the orphaned hedge leg on sole-ownership trust (CB latched, virtual legs cleared by the fire cycle)\n",
						sc.ID, sym, hCoin, hQty)
					pendingCBAlerts = append(pendingCBAlerts, fmt.Sprintf(
						"🚨 **CRITICAL — orphaned hedge leg closed under a latched circuit breaker**\nStrategy `%s`: the circuit breaker is latched and its %s primary had already gone flat on-chain (stop-loss or liquidation during the fetch outage), leaving %.6f on the hedge coin %s with no primary to pair against.\n\nThat leg has been queued for a reduce-only close. With the primary flat there is no way to prove the position was ours beyond sole ownership of the declared hedge coin — the same trust the primary close already uses. **If you opened a manual position on %s yourself, it is being closed; re-open it and remove %s from this strategy's hedge block.**",
						sc.ID, sym, hQty, hCoin, hCoin, hCoin))
				default:
					symbols = append(symbols, PendingCircuitCloseSymbol{Symbol: hCoin, Size: hQty})
					fmt.Printf("[CRITICAL] hl-circuit-close: recovered pending HEDGE close for strategy %s coin %s sz=%.6f (virtual leg was cleared by the force-close sweep on the fire cycle)\n",
						sc.ID, hCoin, hQty)
				}
			}
			if len(symbols) == 0 {
				continue
			}
			ss.RiskState.setPendingCircuitClose(PlatformPendingCloseHyperliquid, &PendingCircuitClose{
				Symbols: symbols,
			})
			fmt.Printf("[CRITICAL] hl-circuit-close: recovered pending for strategy %s (CB latched, HL fetch had failed at fire time): %s\n",
				sc.ID, formatPendingCircuitCloseSymbols(symbols))
		}
		mu.Unlock()
		for _, msg := range pendingCBAlerts {
			if ownerDM != nil {
				ownerDM(msg)
			}
		}
	}

	type job struct {
		stratID string
		pending PendingCircuitClose
		slOIDs  map[string][]int64
	}
	var jobs []job
	mu.RLock()
	for id, ss := range state.Strategies {
		if ss == nil {
			continue
		}
		p := ss.RiskState.getPendingCircuitClose(PlatformPendingCloseHyperliquid)
		if p == nil || len(p.Symbols) == 0 {
			continue
		}
		slOIDs := make(map[string][]int64, len(p.Symbols))
		for _, c := range p.Symbols {
			if pos, ok := ss.Positions[c.Symbol]; ok && pos != nil {
				slOIDs[c.Symbol] = appendUniquePositiveStopLossOID(slOIDs[c.Symbol], pos.StopLossOID)
				for _, tpOID := range pos.TPOIDs {
					slOIDs[c.Symbol] = appendUniquePositiveStopLossOID(slOIDs[c.Symbol], tpOID)
				}
			}
		}
		jobs = append(jobs, job{id, *p, slOIDs})
	}
	mu.RUnlock()

	if len(jobs) == 0 {
		return
	}

	sort.Slice(jobs, func(i, j int) bool { return jobs[i].stratID < jobs[j].stratID })

	for _, j := range jobs {
		if err := ctxOverall.Err(); err != nil {
			fmt.Printf("[CRITICAL] hl-circuit-close: budget exhausted: %v\n", err)
			return
		}
		sc := lookupStrategyConfig(strategies, j.stratID)
		if sc == nil || sc.Platform != "hyperliquid" || sc.Type != "perps" || !hyperliquidIsLive(sc.Args) {
			checkAbandonedPartialModelClose(state, j.stratID, firstPendingSymbol(j.pending), mu, ownerDM)
			mu.Lock()
			if ss := state.Strategies[j.stratID]; ss != nil {
				ss.RiskState.clearPendingCircuitClose(PlatformPendingCloseHyperliquid)
			}
			mu.Unlock()
			continue
		}
		if sym := hyperliquidConfiguredCoin(*sc); sym != "" && len(hlLiveStrategiesForCoin(sym, hlCircuitPeerAll)) > 1 {
			fmt.Printf("[INFO] hl-circuit-close: strategy %s coin %s shares the wallet position with peers — clearing pending close and leaving exchange position untouched\n",
				j.stratID, sym)
			checkAbandonedPartialModelClose(state, j.stratID, sym, mu, ownerDM)
			mu.Lock()
			if ss := state.Strategies[j.stratID]; ss != nil {
				ss.RiskState.clearPendingCircuitClose(PlatformPendingCloseHyperliquid)
			}
			mu.Unlock()
			continue
		}

		allOK := true
		drainError := false
		var drainErrSym string
		var drainErrSz float64
		var drainErrMsg string
		for _, c := range j.pending.Symbols {
			if err := ctxOverall.Err(); err != nil {
				allOK = false
				break
			}
			sz := c.Size
			var onChainSigned float64
			for _, p := range positions {
				if p.Coin != c.Symbol {
					continue
				}
				onChainSigned = p.Size
				absOC := math.Abs(p.Size)
				if absOC <= 1e-15 {
					sz = 0
					break
				}
				if sz > absOC {
					sz = absOC
				}
				break
			}
			if sz <= 1e-15 {
				checkAbandonedPartialModelClose(state, j.stratID, c.Symbol, mu, ownerDM)
				continue
			}
			partial := sz
			cancelOIDs := j.slOIDs[c.Symbol]
			result, err := closer(c.Symbol, &partial, cancelOIDs)
			if err != nil {
				fmt.Printf("[CRITICAL] hl-circuit-close: strategy %s coin %s sz=%.6f failed: %v\n", j.stratID, c.Symbol, sz, err)
				allOK = false
				drainError = true
				drainErrSym = c.Symbol
				drainErrSz = sz
				drainErrMsg = err.Error()
				break
			}

			var (
				fillSz, fillPx, fillFee float64
				fillOID                 int64
				alreadyFlat             bool
			)
			if result != nil && result.Close != nil {
				alreadyFlat = result.Close.AlreadyFlat
				if result.Close.Fill != nil {
					fillSz = result.Close.Fill.TotalSz
					fillPx = result.Close.Fill.AvgPx
					fillFee = result.Close.Fill.Fee
					fillOID = result.Close.Fill.OID
				}
			}
			if alreadyFlat {
				checkAbandonedPartialModelClose(state, j.stratID, c.Symbol, mu, ownerDM)
			}

			if !alreadyFlat && fillSz > 1e-15 {
				mu.Lock()
				if ss := state.Strategies[j.stratID]; ss != nil {
					applyHyperliquidCircuitCloseFill(ss, c.Symbol, fillSz, fillPx, fillFee, onChainSigned, fillOID, "")
				}
				mu.Unlock()
			}

			underFill := !alreadyFlat && fillSz < sz*0.99
			if underFill {
				slCancelled := firstPositiveStopLossOID(cancelOIDs) > 0 && result != nil && result.CancelStopLossSucceeded
				slNote := ""
				if slCancelled {
					slNote = " — stop-loss was cancelled, residual is unprotected until retry"
				}
				fmt.Printf("[CRITICAL] hl-circuit-close: strategy %s coin %s PARTIAL fill %.6f/%.6f — leaving pending for retry%s\n",
					j.stratID, c.Symbol, fillSz, sz, slNote)
				allOK = false
			} else {
				fmt.Printf("[INFO] hl-circuit-close: strategy %s coin %s closed sz=%.6f (filled %.6f)\n", j.stratID, c.Symbol, sz, fillSz)
			}

			if len(cancelOIDs) > 0 && result != nil && result.CancelStopLossSucceeded {
				mu.Lock()
				if ss := state.Strategies[j.stratID]; ss != nil {
					if pos, ok := ss.Positions[c.Symbol]; ok && pos != nil {
						for _, cancelOID := range cancelOIDs {
							if cancelOID > 0 && pos.StopLossOID == cancelOID {
								pos.StopLossOID = 0
							}
							for idx, tpOID := range pos.TPOIDs {
								if cancelOID > 0 && tpOID == cancelOID {
									pos.TPOIDs[idx] = 0
								}
							}
						}
					}
				}
				mu.Unlock()
			}

			if underFill {
				continue
			}
		}

		var failCount int
		var shouldAlert bool
		now := time.Now().UTC()
		mu.Lock()
		if ss := state.Strategies[j.stratID]; ss != nil {
			if allOK {
				ss.RiskState.clearPendingCircuitClose(PlatformPendingCloseHyperliquid)
			} else if p := ss.RiskState.getPendingCircuitClose(PlatformPendingCloseHyperliquid); p != nil {
				if drainError {
					p.ConsecutiveFailures++
					failCount = p.ConsecutiveFailures
					if shouldNotifyDrainFailure(p.ConsecutiveFailures, p.LastNotifiedAt, now) {
						p.LastNotifiedAt = now
						shouldAlert = true
					}
				} else {
					p.ConsecutiveFailures = 0
				}
			}
		}
		mu.Unlock()

		if shouldAlert && ownerDM != nil {
			ownerDM(formatDrainFailureAlert("hyperliquid", j.stratID, drainErrSym, drainErrSz, drainErrMsg, failCount))
		}
	}
}

func hyperliquidOnChainCloseTradeLabel(closeReason string) string {
	switch closeReason {
	case "circuit_breaker":
		return "Circuit breaker on-chain close"
	case "regime_direction_flip":
		return "Regime/direction flip auto-close"
	case "":
		return "On-chain close"
	default:
		return closeReason + " on-chain close"
	}
}

func applyHyperliquidCircuitCloseFill(s *StrategyState, symbol string, fillSz, fillPx, fillFee, onChainSigned float64, fillOID int64, closeReason string) {
	if closeReason == "" {
		closeReason = "circuit_breaker"
	}
	closeLabel := hyperliquidOnChainCloseTradeLabel(closeReason)
	if s == nil || fillSz <= 0 || fillPx <= 0 {
		return
	}
	var oidStr string
	if fillOID > 0 {
		oidStr = strconv.FormatInt(fillOID, 10)
	}
	if oidStr != "" && strategyHasCloseTradeForOID(s, oidStr) {
		fmt.Printf("[hl-sync] %s/%s: close fill OID %s already booked — skipping duplicate (#954)\n", s.ID, symbol, oidStr)
		return
	}
	now := time.Now().UTC()
	pos, ok := s.Positions[symbol]
	if !ok || pos == nil || pos.Quantity <= 0 {
		if reconcileModelOnlyCloseWithFill(s, symbol, fillSz, fillPx, fillFee, fillOID, closeReason) == modelOnlyReconcileApplied {
			return
		}
		closeSide := "sell"
		if onChainSigned < 0 {
			closeSide = "buy"
		}
		RecordTrade(s, Trade{
			Timestamp:       now,
			StrategyID:      s.ID,
			Symbol:          symbol,
			Side:            closeSide,
			Quantity:        fillSz,
			Price:           fillPx,
			Value:           fillSz * fillPx,
			TradeType:       perpsPositionTradeType(pos),
			Details:         fmt.Sprintf("%s (no virtual position), fill=%.6f fee=$%.4f", closeLabel, fillSz, fillFee),
			ExchangeOrderID: oidStr,
			ExchangeFee:     fillFee,
			FeeSource:       FeeSourceUserFills,
			PnLGross:        true,
			IsClose:         true,
			Regime:          s.Regime,
		})
		return
	}

	qtyClosed := fillSz
	if qtyClosed > pos.Quantity {
		qtyClosed = pos.Quantity
	}
	side := pos.Side
	avgCost := pos.AvgCost
	var pnl float64
	if side == "long" {
		pnl = qtyClosed * (fillPx - avgCost)
	} else {
		pnl = qtyClosed * (avgCost - fillPx)
	}
	grossPnL := pnl
	pnl -= fillFee
	s.Cash += pnl
	positionID := ensurePositionTradeID(s.ID, symbol, pos)

	RecordTrade(s, Trade{
		Timestamp:         now,
		StrategyID:        s.ID,
		Symbol:            symbol,
		PositionID:        positionID,
		Side:              closeTradeSide(side),
		Quantity:          qtyClosed,
		Price:             fillPx,
		Value:             qtyClosed * fillPx,
		TradeType:         perpsPositionTradeType(pos),
		Details:           fmt.Sprintf("%s, PnL: $%.2f (fee $%.4f)", closeLabel, pnl, fillFee),
		ExchangeOrderID:   oidStr,
		ExchangeFee:       fillFee,
		FeeSource:         FeeSourceUserFills,
		IsClose:           true,
		RealizedPnL:       grossPnL,
		PnLGross:          true,
		Regime:            s.Regime,
		EntryATR:          pos.EntryATR,
		StopLossTriggerPx: pos.StopLossTriggerPx,
		StopLossATRMult:   pos.StopLossATRMult,
		TPTiersJSON:       pos.TPTiersJSON,
	})
	recordPositionTradeResult(s, pos, pnl)

	remaining := pos.Quantity - qtyClosed
	if remaining <= 1e-9 {
		recordClosedPosition(s, pos, fillPx, pnl, closeReason, now)
		delete(s.Positions, symbol)
		clearHLPerpsPositionAlertThrottles(s, symbol)
	} else {
		pos.Quantity = remaining
	}
}

func firstPositiveStopLossOID(oids []int64) int64 {
	for _, oid := range oids {
		if oid > 0 {
			return oid
		}
	}
	return 0
}

func appendUniquePositiveStopLossOID(oids []int64, oid int64) []int64 {
	if oid <= 0 {
		return oids
	}
	for _, existing := range oids {
		if existing == oid {
			return oids
		}
	}
	return append(oids, oid)
}

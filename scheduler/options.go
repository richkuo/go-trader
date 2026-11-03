package main

import (
	"encoding/json"
	"fmt"
	"sort"
	"sync"
	"time"
)

type OptionPosition struct {
	ID                 string    `json:"id"`
	TradePositionID    string    `json:"position_id,omitempty"`
	Underlying         string    `json:"underlying"`
	OptionType         string    `json:"option_type"`
	Strike             float64   `json:"strike"`
	Expiry             string    `json:"expiry"`
	DTE                float64   `json:"dte"`
	Action             string    `json:"action"`
	Quantity           float64   `json:"quantity"`
	ContractMultiplier float64   `json:"contract_multiplier,omitempty"`
	EntryPremium       float64   `json:"entry_premium"`
	EntryPremiumUSD    float64   `json:"entry_premium_usd"`
	CurrentValueUSD    float64   `json:"current_value_usd"`
	Greeks             OptGreeks `json:"greeks"`
	OpenedAt           time.Time `json:"opened_at"`
	MarkUpdatedAt      time.Time `json:"mark_updated_at,omitzero"`
	MarkError          string    `json:"mark_error,omitempty"`
	MarkSource         string    `json:"mark_source,omitempty"`
	ExpiryCutoffAt     time.Time `json:"expiry_cutoff_at,omitzero"`
	SettlementReadyAt  time.Time `json:"settlement_ready_at,omitzero"`
	markVerifiedAt     time.Time
}

type OptGreeks struct {
	Delta float64 `json:"delta"`
	Gamma float64 `json:"gamma"`
	Theta float64 `json:"theta"`
	Vega  float64 `json:"vega"`
}

type OptionsAction struct {
	Action     string    `json:"action"`
	OptionType string    `json:"option_type"`
	Strike     float64   `json:"strike"`
	Expiry     string    `json:"expiry"`
	DTE        float64   `json:"dte"`
	Premium    float64   `json:"premium"`
	PremiumUSD float64   `json:"premium_usd"`
	Quantity   float64   `json:"quantity,omitempty"`
	Greeks     OptGreeks `json:"greeks"`
}

type OptionsResult struct {
	Strategy        string          `json:"strategy"`
	Underlying      string          `json:"underlying"`
	Platform        string          `json:"platform"`
	PremiumUnit     string          `json:"premium_unit"`
	Signal          int             `json:"signal"`
	SpotPrice       float64         `json:"spot_price"`
	Actions         []OptionsAction `json:"actions"`
	IVRank          *float64        `json:"iv_rank"`
	VolMetricsError string          `json:"vol_metrics_error,omitempty"`
	Regime          string          `json:"regime,omitempty"`
	Timestamp       string          `json:"timestamp"`
	Error           string          `json:"error,omitempty"`
}

func formatOptionsIVRank(result *OptionsResult) string {
	if result == nil || result.VolMetricsError != "" || result.IVRank == nil {
		return "unavailable"
	}
	return fmt.Sprintf("%.1f", *result.IVRank)
}

func ExecuteOptionsSignal(s *StrategyState, result *OptionsResult, logger *StrategyLogger) (int, error) {
	if result.Signal == 0 || len(result.Actions) == 0 {
		return 0, nil
	}
	if err := optionsResultContractError(s.Platform, result); err != nil {
		return 0, fmt.Errorf("%w; refusing %d option action(s)", err, len(result.Actions))
	}

	refuseEntries := false
	spec, err := lookupOptionContractSpec(s.Platform, result.Underlying)
	if err != nil {
		return 0, err
	}
	if spec.RequireCoveredCalls {
		needed := 0.0
		var firstShortCall *OptionsAction
		for i := range result.Actions {
			action := &result.Actions[i]
			if action.Action != "sell" || action.OptionType != "call" {
				continue
			}
			qty := action.Quantity
			if qty <= 0 {
				qty = 1
			}
			shares, err := optionContractValueUSD(1, spec.Multiplier, qty)
			if err != nil {
				logger.Error("Option signal cover check failed: %v", err)
				refuseEntries = true
				break
			}
			needed += shares
			if firstShortCall == nil {
				firstShortCall = action
			}
		}
		if firstShortCall != nil {
			held := heldLongUnderlyingShares(s, result.Underlying)
			reservedShares, err := reservedShortCallShares(s, result.Underlying)
			if err != nil {
				logger.Error("Option signal cover check failed: %v", err)
				refuseEntries = true
			} else if needed > held-reservedShares {
				logger.Info("Uncovered short call refused %s strike=%.0f qty=%g: required %g free shares, available %g (held %g - reserved by open short calls %g); refusing every entry action in this signal",
					result.Underlying, firstShortCall.Strike, max(firstShortCall.Quantity, 1), needed, held-reservedShares, held, reservedShares)
				refuseEntries = true
			}
		}
	}

	tradesExecuted := 0

	for _, action := range result.Actions {
		if refuseEntries && (action.Action == "buy" || action.Action == "sell") {
			continue
		}
		switch action.Action {
		case "buy":
			trades, err := executeOptionBuy(s, result, &action, logger)
			if err != nil {
				logger.Error("Option buy failed: %v", err)
				continue
			}
			tradesExecuted += trades

		case "sell":
			trades, err := executeOptionSell(s, result, &action, logger)
			if err != nil {
				logger.Error("Option sell failed: %v", err)
				continue
			}
			tradesExecuted += trades

		case "close":
			trades, err := executeOptionClose(s, result, &action, logger)
			if err != nil {
				logger.Error("Option close failed: %v", err)
				continue
			}
			tradesExecuted += trades

		default:
			logger.Info("Unhandled options action: %s", action.Action)
		}
	}

	return tradesExecuted, nil
}

func optionActionUnitPremiumUSD(result *OptionsResult, action *OptionsAction) float64 {
	unit := action.PremiumUSD
	if unit <= 0 {
		unit = action.Premium * result.SpotPrice
	}
	return unit
}

func executeOptionBuy(s *StrategyState, result *OptionsResult, action *OptionsAction, logger *StrategyLogger) (int, error) {
	qty := action.Quantity
	if qty <= 0 {
		qty = 1.0
	}
	spec, err := lookupOptionContractSpec(s.Platform, result.Underlying)
	if err != nil {
		return 0, err
	}
	unit := optionActionUnitPremiumUSD(result, action)
	if unit <= 0 {
		logger.Info("Zero premium, skipping option buy")
		return 0, nil
	}
	cost, err := optionContractValueUSD(unit, spec.Multiplier, qty)
	if err != nil {
		return 0, fmt.Errorf("buy %s %s strike=%.0f: %w", result.Underlying, action.OptionType, action.Strike, err)
	}
	if cost <= 0 {
		logger.Info("Zero premium, skipping option buy")
		return 0, nil
	}

	fee := CalculateOptionFee(s.Platform, cost, qty)
	reserve, err := reservedShortPutCollateralUSD(s)
	if err != nil {
		return 0, err
	}

	totalCost := cost + fee
	available := s.Cash - reserve
	if totalCost > available {
		logger.Info("Insufficient free cash for option buy %s %s strike=%.0f: required $%.2f (premium $%.2f + fee $%.2f), available $%.2f (cash $%.2f - reserved put collateral $%.2f)",
			result.Underlying, action.OptionType, action.Strike, totalCost, cost, fee, available, s.Cash, reserve)
		return 0, nil
	}

	posID := fmt.Sprintf("%s-%s-%s-%.0f-%s",
		result.Underlying, action.OptionType, action.Action, action.Strike, action.Expiry)

	now := time.Now().UTC()
	positionID := newTradePositionID(s.ID, posID, now)
	s.Cash -= totalCost
	s.OptionPositions[posID] = &OptionPosition{
		ID:                 posID,
		TradePositionID:    positionID,
		Underlying:         result.Underlying,
		OptionType:         action.OptionType,
		Strike:             action.Strike,
		Expiry:             action.Expiry,
		DTE:                action.DTE,
		Action:             "buy",
		Quantity:           qty,
		ContractMultiplier: spec.Multiplier,
		EntryPremium:       action.Premium,
		EntryPremiumUSD:    cost,
		CurrentValueUSD:    cost,
		Greeks:             action.Greeks,
		OpenedAt:           now,
	}

	trade := Trade{
		Timestamp:  now,
		StrategyID: s.ID,
		Symbol:     fmt.Sprintf("%s-%s-%.0f-%s", result.Underlying, action.OptionType, action.Strike, action.Expiry),
		PositionID: positionID,
		Side:       "buy",
		Quantity:   qty,
		Price:      cost,
		Value:      totalCost,
		TradeType:  "options",
		Details:    fmt.Sprintf("Buy %s %s strike=%.0f exp=%s qty=%g premium=$%.2f fee=$%.2f", result.Underlying, action.OptionType, action.Strike, action.Expiry, qty, cost, fee),
	}
	trade.Regime = s.Regime
	RecordTrade(s, trade)
	logger.Info("BUY OPTION %s %s strike=%.0f exp=%s qty=%g | $%.2f (fee $%.2f)", result.Underlying, action.OptionType, action.Strike, action.Expiry, qty, cost, fee)

	return 1, nil
}

func executeOptionSell(s *StrategyState, result *OptionsResult, action *OptionsAction, logger *StrategyLogger) (int, error) {
	qty := action.Quantity
	if qty <= 0 {
		qty = 1.0
	}
	spec, err := lookupOptionContractSpec(s.Platform, result.Underlying)
	if err != nil {
		return 0, err
	}
	unit := optionActionUnitPremiumUSD(result, action)
	if unit <= 0 {
		logger.Info("Zero premium, skipping option sell")
		return 0, nil
	}
	premium, err := optionContractValueUSD(unit, spec.Multiplier, qty)
	if err != nil {
		return 0, fmt.Errorf("sell %s %s strike=%.0f: %w", result.Underlying, action.OptionType, action.Strike, err)
	}
	if premium <= 0 {
		logger.Info("Zero premium, skipping option sell")
		return 0, nil
	}

	reserve, err := reservedShortPutCollateralUSD(s)
	if err != nil {
		return 0, err
	}
	newCollateral := 0.0
	if action.OptionType == "put" {
		newCollateral, err = optionContractValueUSD(action.Strike, spec.Multiplier, qty)
		if err != nil {
			return 0, fmt.Errorf("sell %s put strike=%.0f collateral: %w", result.Underlying, action.Strike, err)
		}
		if reserve+newCollateral > s.Cash {
			logger.Info("Insufficient collateral for short put %s strike=%.0f qty=%g: required $%.2f (new $%.2f + reserved $%.2f), available cash $%.2f",
				result.Underlying, action.Strike, qty, reserve+newCollateral, newCollateral, reserve, s.Cash)
			return 0, nil
		}
	}
	if action.OptionType == "call" && spec.RequireCoveredCalls {
		need, err := optionContractValueUSD(1, spec.Multiplier, qty)
		if err != nil {
			return 0, fmt.Errorf("sell %s call strike=%.0f cover: %w", result.Underlying, action.Strike, err)
		}
		held := heldLongUnderlyingShares(s, result.Underlying)
		reservedShares, err := reservedShortCallShares(s, result.Underlying)
		if err != nil {
			return 0, err
		}
		free := held - reservedShares
		if need > free {
			logger.Info("Uncovered short call refused %s strike=%.0f qty=%g: required %g free shares, available %g (held %g - reserved by open short calls %g)",
				result.Underlying, action.Strike, qty, need, free, held, reservedShares)
			return 0, nil
		}
	}

	fee := CalculateOptionFee(s.Platform, premium, qty)

	netPremium := premium - fee
	resultingCash := s.Cash + netPremium
	resultingReserve := reserve + newCollateral
	if resultingCash < resultingReserve {
		logger.Info("Option sell %s %s strike=%.0f would leave cash below reserved put collateral: required $%.2f, available $%.2f after premium $%.2f and fee $%.2f",
			result.Underlying, action.OptionType, action.Strike, resultingReserve, resultingCash, premium, fee)
		return 0, nil
	}

	posID := fmt.Sprintf("%s-%s-%s-%.0f-%s",
		result.Underlying, action.OptionType, action.Action, action.Strike, action.Expiry)

	now := time.Now().UTC()
	positionID := newTradePositionID(s.ID, posID, now)
	s.Cash += netPremium
	s.OptionPositions[posID] = &OptionPosition{
		ID:                 posID,
		TradePositionID:    positionID,
		Underlying:         result.Underlying,
		OptionType:         action.OptionType,
		Strike:             action.Strike,
		Expiry:             action.Expiry,
		DTE:                action.DTE,
		Action:             "sell",
		Quantity:           qty,
		ContractMultiplier: spec.Multiplier,
		EntryPremium:       action.Premium,
		EntryPremiumUSD:    netPremium,
		CurrentValueUSD:    -netPremium,
		Greeks:             action.Greeks,
		OpenedAt:           now,
	}

	trade := Trade{
		Timestamp:  now,
		StrategyID: s.ID,
		Symbol:     fmt.Sprintf("%s-%s-%.0f-%s", result.Underlying, action.OptionType, action.Strike, action.Expiry),
		PositionID: positionID,
		Side:       "sell",
		Quantity:   qty,
		Price:      premium,
		Value:      netPremium,
		TradeType:  "options",
		Details:    fmt.Sprintf("Sell %s %s strike=%.0f exp=%s qty=%g premium=$%.2f fee=$%.2f", result.Underlying, action.OptionType, action.Strike, action.Expiry, qty, premium, fee),
	}
	trade.Regime = s.Regime
	RecordTrade(s, trade)
	logger.Info("SELL OPTION %s %s strike=%.0f exp=%s qty=%g | +$%.2f (fee $%.2f)", result.Underlying, action.OptionType, action.Strike, action.Expiry, qty, premium, fee)

	return 1, nil
}

func executeOptionClose(s *StrategyState, result *OptionsResult, action *OptionsAction, logger *StrategyLogger) (int, error) {
	closed := 0
	for _, id := range sortedOptionLegKeys(s.OptionPositions) {
		pos := s.OptionPositions[id]
		if pos.Underlying == result.Underlying && pos.Strike == action.Strike && pos.OptionType == action.OptionType {
			pnl := 0.0
			var closePriceUSD float64
			if pos.Action == "buy" {
				pnl = pos.CurrentValueUSD - pos.EntryPremiumUSD
				s.Cash += pos.CurrentValueUSD
				closePriceUSD = pos.CurrentValueUSD
			} else {
				m, err := strategyOptionMultiplier(s, id, pos)
				if err != nil {
					logger.Error("Option close skipped: %v", err)
					continue
				}
				unit := optionActionUnitPremiumUSD(result, action)
				if unit < 0 {
					unit = 0
				}
				debit, err := optionContractValueUSD(unit, m, pos.Quantity)
				if err != nil {
					logger.Error("Option close skipped for %s: %v", id, err)
					continue
				}
				pnl = pos.EntryPremiumUSD - debit
				s.Cash -= debit
				closePriceUSD = debit
			}
			now := time.Now().UTC()
			positionID := ensureOptionTradeID(s.ID, pos)
			trade := Trade{
				Timestamp:   now,
				StrategyID:  s.ID,
				Symbol:      pos.ID,
				PositionID:  positionID,
				Side:        optionCloseTradeSide(pos.Action),
				Quantity:    pos.Quantity,
				Price:       closePriceUSD,
				Value:       closePriceUSD,
				TradeType:   "options",
				Details:     fmt.Sprintf("Close %s PnL=$%.2f", pos.ID, pnl),
				IsClose:     true,
				RealizedPnL: pnl,
				PnLGross:    true,
			}
			trade.Regime = s.Regime
			RecordTrade(s, trade)
			RecordTradeResult(&s.RiskState, pnl)
			recordClosedOptionPosition(s, pos, closePriceUSD, pnl, "signal", now)
			logger.Info("CLOSE OPTION %s | PnL: $%.2f", pos.ID, pnl)
			delete(s.OptionPositions, id)
			closed++
		}
	}
	return closed, nil
}

func optionUnitEntryPremiumUSD(platform, key string, p *OptionPosition) (float64, error) {
	m, err := optionPositionMultiplier(platform, p)
	if err != nil {
		return 0, fmt.Errorf("option leg %s: %w", key, err)
	}
	if !finitePositive(p.Quantity) {
		return 0, fmt.Errorf("option leg %s: quantity %v is not positive", key, p.Quantity)
	}
	return p.EntryPremiumUSD / (p.Quantity * m), nil
}

type encodedOptionPosition struct {
	OptionType string  `json:"option_type"`
	Strike     float64 `json:"strike"`
	Expiry     string  `json:"expiry"`
	DTE        float64 `json:"dte"`
	Action     string  `json:"action"`
	Premium    float64 `json:"entry_premium_usd"`
	Delta      float64 `json:"delta"`
	Gamma      float64 `json:"gamma"`
	Theta      float64 `json:"theta"`
	Vega       float64 `json:"vega"`
}

func encodeOptionPosition(platform, key string, p *OptionPosition) (encodedOptionPosition, error) {
	unit, err := optionUnitEntryPremiumUSD(platform, key, p)
	if err != nil {
		return encodedOptionPosition{}, err
	}
	return encodedOptionPosition{
		OptionType: p.OptionType,
		Strike:     p.Strike,
		Expiry:     p.Expiry,
		DTE:        p.DTE,
		Action:     p.Action,
		Premium:    unit,
		Delta:      p.Greeks.Delta,
		Gamma:      p.Greeks.Gamma,
		Theta:      p.Greeks.Theta,
		Vega:       p.Greeks.Vega,
	}, nil
}

func EncodePositionsJSON(platform string, positions map[string]*OptionPosition) (string, error) {
	if len(positions) == 0 {
		return "[]", nil
	}
	var out []encodedOptionPosition
	for _, key := range sortedOptionLegKeys(positions) {
		e, err := encodeOptionPosition(platform, key, positions[key])
		if err != nil {
			return "", fmt.Errorf("options positions encoding failed: %w", err)
		}
		out = append(out, e)
	}
	b, err := json.Marshal(out)
	if err != nil {
		return "", fmt.Errorf("options positions encoding failed: %w", err)
	}
	return string(b), nil
}

func EncodeAllPositionsJSON(platform string, optPos map[string]*OptionPosition, spotPos map[string]*Position) (string, error) {
	type spotEntry struct {
		PositionType string  `json:"position_type"`
		Symbol       string  `json:"symbol"`
		Quantity     float64 `json:"quantity"`
		AvgCost      float64 `json:"avg_cost"`
		Side         string  `json:"side"`
	}

	var out []interface{}
	for _, key := range sortedOptionLegKeys(optPos) {
		e, err := encodeOptionPosition(platform, key, optPos[key])
		if err != nil {
			return "", fmt.Errorf("options positions encoding failed: %w", err)
		}
		out = append(out, e)
	}
	spotKeys := make([]string, 0, len(spotPos))
	for k := range spotPos {
		spotKeys = append(spotKeys, k)
	}
	sort.Strings(spotKeys)
	for _, k := range spotKeys {
		p := spotPos[k]
		out = append(out, spotEntry{
			PositionType: "spot",
			Symbol:       p.Symbol,
			Quantity:     p.Quantity,
			AvgCost:      p.AvgCost,
			Side:         p.Side,
		})
	}
	if len(out) == 0 {
		return "[]", nil
	}
	b, err := json.Marshal(out)
	if err != nil {
		return "", fmt.Errorf("options positions encoding failed: %w", err)
	}
	return string(b), nil
}

type optionsDispatch struct {
	SC                    StrategyConfig
	GlobalIntervalSeconds int
	Now                   time.Time
	State                 *StrategyState
	PositionsJSON         string
	PositionsErr          error
	Risk                  *scopeCycleRisk
	PersistenceHold       bool
	Scope                 PortfolioScope
	Regime                *RegimeConfig
	Mu                    *sync.RWMutex
	Notifier              *MultiNotifier
	Logger                *StrategyLogger
}

func runOptionsDispatch(d optionsDispatch) (int, []string) {
	sc, logger := d.SC, d.Logger
	var result *OptionsResult
	var signalStr string
	if d.PositionsErr != nil {
		logger.Error("Options check skipped: %v", d.PositionsErr)
		notifyScriptFailure(d.Notifier, sc, scriptFailureError, d.PositionsErr.Error())
	} else if r, sig, ok := runOptionsCheck(sc, d.PositionsJSON, d.Notifier, logger); ok {
		result, signalStr = r, sig
		filterOptionsEntryActions(d, result)
	}
	optionsRegime := globalRegimeStore.PayloadForStrategy(sc, d.Regime)
	d.Mu.Lock()
	defer d.Mu.Unlock()
	d.State.Regime = optionsRegime.PrimaryLabel(nil)
	now := d.Now
	if now.IsZero() {
		now = time.Now().UTC()
	}
	return executeOptionsCycleLocked(sc, d.State, result, signalStr, newOptionHarvestClock(sc, d.GlobalIntervalSeconds, now), logger)
}

func filterOptionsEntryActions(d optionsDispatch, result *OptionsResult) {
	sc, logger, sr := d.SC, d.Logger, d.Risk
	if sc.Paused {
		kept, dropped := pausedOptionsActions(result.Actions)
		if dropped > 0 {
			logger.Info("Paused: %d option open action(s) dropped — close actions still execute (#1150)", dropped)
		}
		result.Actions = kept
	}
	if sr.DailyLossEntriesHeld {
		kept, dropped := pausedOptionsActions(result.Actions)
		if dropped > 0 {
			logger.Info("Daily loss limit: %d option open action(s) dropped — entries held until UTC rollover (#1269)", dropped)
		}
		result.Actions = kept
	}
	if sr.NotionalBlocked {
		kept, dropped := pausedOptionsActions(result.Actions)
		if dropped > 0 {
			logger.Warn("Notional cap: %d option open action(s) dropped — new opens blocked, exits continue (#1344)", dropped)
		}
		result.Actions = kept
	}
	if d.PersistenceHold {
		kept, dropped := pausedOptionsActions(result.Actions)
		if dropped > 0 {
			logger.Warn("Persistence hold: %d option open action(s) dropped — the %s scope has an unacknowledged state save failure, so no new exposure is taken; closes continue (#1523)", dropped, scopeLabel(d.Scope))
		}
		result.Actions = kept
	}
	if kept, dropped, capWhy := exposureCapOptionsActions(sr.ExposureCapStatus, extractAsset(sc), result.Actions); dropped > 0 {
		logger.Warn("Exposure cap: %d option open action(s) dropped — %s (#1270)", dropped, capWhy)
		result.Actions = kept
	}
}

func executeOptionsCycleLocked(sc StrategyConfig, s *StrategyState, result *OptionsResult, signalStr string, clock optionHarvestClock, logger *StrategyLogger) (int, []string) {
	trades := 0
	var lines []string
	if result != nil {
		entryTrades, err := ExecuteOptionsSignal(s, result, logger)
		if err != nil {
			logger.Error("Options execution failed: %v", err)
		} else if entryTrades > 0 {
			trades += entryTrades
			lines = append(lines, fmt.Sprintf("[%s] %s %s spot=$%s IV=%s", sc.ID, signalStr, result.Underlying, formatSignalPrice(result.SpotPrice), formatOptionsIVRank(result)))
		}
	}
	if sc.ThetaHarvest != nil {
		harvestTrades, harvestDetails := CheckThetaHarvest(s, sc.ThetaHarvest, clock, logger)
		trades += harvestTrades
		lines = append(lines, harvestDetails...)
	}
	return trades, lines
}

func CheckThetaHarvest(s *StrategyState, cfg *ThetaHarvestConfig, clock optionHarvestClock, logger *StrategyLogger) (int, []string) {
	if cfg == nil || !cfg.Enabled {
		return 0, nil
	}

	trades := 0
	var details []string

	type closeAction struct {
		id     string
		reason string
	}
	var toClose []closeAction

	for _, id := range sortedOptionLegKeys(s.OptionPositions) {
		pos := s.OptionPositions[id]
		if pos == nil || pos.Action != "sell" {
			continue
		}
		dte, why := optionHarvestEligibility(pos, clock)
		if why != "" {
			logger.InfoOnChange("harvest-eligibility:"+id, why, "Theta harvest holds %s: %s", id, why)
			continue
		}
		logger.Changed("harvest-eligibility:"+id, "")

		entryPremium := pos.EntryPremiumUSD
		if entryPremium <= 0 {
			continue
		}

		currentCost := -pos.CurrentValueUSD
		if currentCost < 0 {
			currentCost = 0
		}

		profitUSD := entryPremium - currentCost
		profitPct := (profitUSD / entryPremium) * 100

		if cfg.ProfitTargetPct > 0 && profitPct >= cfg.ProfitTargetPct {
			toClose = append(toClose, closeAction{
				id:     id,
				reason: fmt.Sprintf("🎯 Theta harvest: %.0f%% profit captured ($%.2f of $%.2f premium)", profitPct, profitUSD, entryPremium),
			})
			continue
		}

		if cfg.StopLossPct > 0 && profitPct < 0 {
			lossPct := -profitPct
			if lossPct >= cfg.StopLossPct {
				toClose = append(toClose, closeAction{
					id:     id,
					reason: fmt.Sprintf("🛑 Stop loss: %.0f%% loss on sold option ($%.2f)", lossPct, -profitUSD),
				})
				continue
			}
		}

		if cfg.MinDTEClose > 0 && dte > 0 && dte <= cfg.MinDTEClose {
			toClose = append(toClose, closeAction{
				id:     id,
				reason: fmt.Sprintf("⏰ DTE exit: %.1f days to expiry (min: %.0f)", dte, cfg.MinDTEClose),
			})
			continue
		}
	}

	for _, c := range toClose {
		pos := s.OptionPositions[c.id]
		if pos == nil {
			continue
		}

		buybackCost := -pos.CurrentValueUSD
		if buybackCost < 0 {
			buybackCost = 0
		}
		pnl := pos.EntryPremiumUSD - buybackCost

		s.Cash -= buybackCost

		now := time.Now().UTC()
		positionID := ensureOptionTradeID(s.ID, pos)
		trade := Trade{
			Timestamp:   now,
			StrategyID:  s.ID,
			Symbol:      pos.ID,
			PositionID:  positionID,
			Side:        optionCloseTradeSide(pos.Action),
			Quantity:    pos.Quantity,
			Price:       buybackCost,
			Value:       buybackCost,
			TradeType:   "options",
			Details:     fmt.Sprintf("Theta harvest close %s PnL=$%.2f", pos.ID, pnl),
			IsClose:     true,
			RealizedPnL: pnl,
			PnLGross:    true,
		}
		trade.Regime = s.Regime
		RecordTrade(s, trade)
		RecordTradeResult(&s.RiskState, pnl)
		recordClosedOptionPosition(s, pos, buybackCost, pnl, "theta_harvest", now)

		logger.Info("%s | %s | PnL: $%.2f", c.reason, pos.ID, pnl)
		detail := fmt.Sprintf("[%s] CLOSE %s — %s (PnL: $%.2f)", s.ID, pos.ID, c.reason, pnl)
		details = append(details, detail)

		delete(s.OptionPositions, c.id)
		trades++
	}

	return trades, details
}

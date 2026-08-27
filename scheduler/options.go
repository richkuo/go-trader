package main

import (
	"encoding/json"
	"fmt"
	"time"
)

type OptionPosition struct {
	ID              string    `json:"id"`
	TradePositionID string    `json:"position_id,omitempty"`
	Underlying      string    `json:"underlying"`
	OptionType      string    `json:"option_type"`
	Strike          float64   `json:"strike"`
	Expiry          string    `json:"expiry"`
	DTE             float64   `json:"dte"`
	Action          string    `json:"action"`
	Quantity        float64   `json:"quantity"`
	EntryPremium    float64   `json:"entry_premium"`
	EntryPremiumUSD float64   `json:"entry_premium_usd"`
	CurrentValueUSD float64   `json:"current_value_usd"`
	Greeks          OptGreeks `json:"greeks"`
	OpenedAt        time.Time `json:"opened_at"`
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
	Strategy   string          `json:"strategy"`
	Underlying string          `json:"underlying"`
	Signal     int             `json:"signal"`
	SpotPrice  float64         `json:"spot_price"`
	Actions    []OptionsAction `json:"actions"`
	IVRank     float64         `json:"iv_rank"`
	Regime     string          `json:"regime,omitempty"`
	Timestamp  string          `json:"timestamp"`
	Error      string          `json:"error,omitempty"`
}

func ExecuteOptionsSignal(s *StrategyState, result *OptionsResult, logger *StrategyLogger) (int, error) {
	if result.Signal == 0 || len(result.Actions) == 0 {
		return 0, nil
	}

	tradesExecuted := 0

	for _, action := range result.Actions {
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

func executeOptionBuy(s *StrategyState, result *OptionsResult, action *OptionsAction, logger *StrategyLogger) (int, error) {
	qty := action.Quantity
	if qty <= 0 {
		qty = 1.0
	}
	cost := action.PremiumUSD
	if cost <= 0 {
		cost = action.Premium * result.SpotPrice
	}
	cost *= qty
	if cost <= 0 {
		logger.Info("Zero premium, skipping option buy")
		return 0, nil
	}

	fee := CalculateOptionFee(s.Platform, cost, 1.0)

	totalCost := cost + fee
	if totalCost > s.Cash {
		logger.Info("Insufficient cash ($%.2f) for option buy ($%.2f + $%.2f fee)", s.Cash, cost, fee)
		return 0, nil
	}

	posID := fmt.Sprintf("%s-%s-%s-%.0f-%s",
		result.Underlying, action.OptionType, action.Action, action.Strike, action.Expiry)

	now := time.Now().UTC()
	positionID := newTradePositionID(s.ID, posID, now)
	s.Cash -= totalCost
	s.OptionPositions[posID] = &OptionPosition{
		ID:              posID,
		TradePositionID: positionID,
		Underlying:      result.Underlying,
		OptionType:      action.OptionType,
		Strike:          action.Strike,
		Expiry:          action.Expiry,
		DTE:             action.DTE,
		Action:          "buy",
		Quantity:        qty,
		EntryPremium:    action.Premium,
		EntryPremiumUSD: cost,
		CurrentValueUSD: cost,
		Greeks:          action.Greeks,
		OpenedAt:        now,
	}

	trade := Trade{
		Timestamp:  now,
		StrategyID: s.ID,
		Symbol:     fmt.Sprintf("%s-%s-%.0f-%s", result.Underlying, action.OptionType, action.Strike, action.Expiry),
		PositionID: positionID,
		Side:       "buy",
		Quantity:   1.0,
		Price:      cost,
		Value:      totalCost,
		TradeType:  "options",
		Details:    fmt.Sprintf("Buy %s %s strike=%.0f exp=%s premium=$%.2f fee=$%.2f", result.Underlying, action.OptionType, action.Strike, action.Expiry, cost, fee),
	}
	trade.Regime = s.Regime
	RecordTrade(s, trade)
	logger.Info("BUY OPTION %s %s strike=%.0f exp=%s | $%.2f (fee $%.2f)", result.Underlying, action.OptionType, action.Strike, action.Expiry, cost, fee)

	return 1, nil
}

func executeOptionSell(s *StrategyState, result *OptionsResult, action *OptionsAction, logger *StrategyLogger) (int, error) {
	qty := action.Quantity
	if qty <= 0 {
		qty = 1.0
	}
	premium := action.PremiumUSD
	if premium <= 0 {
		premium = action.Premium * result.SpotPrice
	}
	premium *= qty
	if premium <= 0 {
		logger.Info("Zero premium, skipping option sell")
		return 0, nil
	}

	if action.OptionType == "put" && action.Strike*qty > s.Cash {
		logger.Info("Insufficient collateral for naked put: strike*qty=$%.2f > cash=$%.2f", action.Strike*qty, s.Cash)
		return 0, nil
	}

	fee := CalculateOptionFee(s.Platform, premium, 1.0)

	netPremium := premium - fee

	posID := fmt.Sprintf("%s-%s-%s-%.0f-%s",
		result.Underlying, action.OptionType, action.Action, action.Strike, action.Expiry)

	now := time.Now().UTC()
	positionID := newTradePositionID(s.ID, posID, now)
	s.Cash += netPremium
	s.OptionPositions[posID] = &OptionPosition{
		ID:              posID,
		TradePositionID: positionID,
		Underlying:      result.Underlying,
		OptionType:      action.OptionType,
		Strike:          action.Strike,
		Expiry:          action.Expiry,
		DTE:             action.DTE,
		Action:          "sell",
		Quantity:        qty,
		EntryPremium:    action.Premium,
		EntryPremiumUSD: netPremium,
		CurrentValueUSD: -netPremium,
		Greeks:          action.Greeks,
		OpenedAt:        now,
	}

	trade := Trade{
		Timestamp:  now,
		StrategyID: s.ID,
		Symbol:     fmt.Sprintf("%s-%s-%.0f-%s", result.Underlying, action.OptionType, action.Strike, action.Expiry),
		PositionID: positionID,
		Side:       "sell",
		Quantity:   1.0,
		Price:      premium,
		Value:      netPremium,
		TradeType:  "options",
		Details:    fmt.Sprintf("Sell %s %s strike=%.0f exp=%s premium=$%.2f fee=$%.2f", result.Underlying, action.OptionType, action.Strike, action.Expiry, premium, fee),
	}
	trade.Regime = s.Regime
	RecordTrade(s, trade)
	logger.Info("SELL OPTION %s %s strike=%.0f exp=%s | +$%.2f (fee $%.2f)", result.Underlying, action.OptionType, action.Strike, action.Expiry, premium, fee)

	return 1, nil
}

func executeOptionClose(s *StrategyState, result *OptionsResult, action *OptionsAction, logger *StrategyLogger) (int, error) {
	closed := 0
	for id, pos := range s.OptionPositions {
		if pos.Underlying == result.Underlying && pos.Strike == action.Strike && pos.OptionType == action.OptionType {
			pnl := 0.0
			var closePriceUSD float64
			if pos.Action == "buy" {
				pnl = pos.CurrentValueUSD - pos.EntryPremiumUSD
				s.Cash += pos.CurrentValueUSD
				closePriceUSD = pos.CurrentValueUSD
			} else {
				pnl = pos.EntryPremiumUSD - action.PremiumUSD
				s.Cash -= action.PremiumUSD
				closePriceUSD = action.PremiumUSD
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
				Price:       action.PremiumUSD,
				Value:       action.PremiumUSD,
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

func EncodePositionsJSON(positions map[string]*OptionPosition) string {
	if len(positions) == 0 {
		return "[]"
	}
	type posInfo struct {
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
	var out []posInfo
	for _, p := range positions {
		out = append(out, posInfo{
			OptionType: p.OptionType,
			Strike:     p.Strike,
			Expiry:     p.Expiry,
			DTE:        p.DTE,
			Action:     p.Action,
			Premium:    p.EntryPremiumUSD,
			Delta:      p.Greeks.Delta,
			Gamma:      p.Greeks.Gamma,
			Theta:      p.Greeks.Theta,
			Vega:       p.Greeks.Vega,
		})
	}
	b, _ := json.Marshal(out)
	return string(b)
}

func EncodeAllPositionsJSON(optPos map[string]*OptionPosition, spotPos map[string]*Position) string {
	type optEntry struct {
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
	type spotEntry struct {
		PositionType string  `json:"position_type"`
		Symbol       string  `json:"symbol"`
		Quantity     float64 `json:"quantity"`
		AvgCost      float64 `json:"avg_cost"`
		Side         string  `json:"side"`
	}

	var out []interface{}
	for _, p := range optPos {
		out = append(out, optEntry{
			OptionType: p.OptionType,
			Strike:     p.Strike,
			Expiry:     p.Expiry,
			DTE:        p.DTE,
			Action:     p.Action,
			Premium:    p.EntryPremiumUSD,
			Delta:      p.Greeks.Delta,
			Gamma:      p.Greeks.Gamma,
			Theta:      p.Greeks.Theta,
			Vega:       p.Greeks.Vega,
		})
	}
	for _, p := range spotPos {
		out = append(out, spotEntry{
			PositionType: "spot",
			Symbol:       p.Symbol,
			Quantity:     p.Quantity,
			AvgCost:      p.AvgCost,
			Side:         p.Side,
		})
	}
	if len(out) == 0 {
		return "[]"
	}
	b, _ := json.Marshal(out)
	return string(b)
}

func CheckThetaHarvest(s *StrategyState, cfg *ThetaHarvestConfig, logger *StrategyLogger) (int, []string) {
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

	for id, pos := range s.OptionPositions {
		if pos.Action != "sell" {
			continue
		}

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

		if cfg.MinDTEClose > 0 && pos.DTE > 0 && pos.DTE <= cfg.MinDTEClose {
			toClose = append(toClose, closeAction{
				id:     id,
				reason: fmt.Sprintf("⏰ DTE exit: %.1f days to expiry (min: %.0f)", pos.DTE, cfg.MinDTEClose),
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

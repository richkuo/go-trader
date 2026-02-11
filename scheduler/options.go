package main

import (
	"encoding/json"
	"fmt"
	"time"
)

// OptionPosition represents a tracked options position.
type OptionPosition struct {
	ID              string    `json:"id"`
	Underlying      string    `json:"underlying"`
	OptionType      string    `json:"option_type"` // "call" or "put"
	Strike          float64   `json:"strike"`
	Expiry          string    `json:"expiry"`
	DTE             float64   `json:"dte"`
	Action          string    `json:"action"` // "buy" or "sell"
	Quantity        float64   `json:"quantity"`
	EntryPremium    float64   `json:"entry_premium"`     // in underlying terms
	EntryPremiumUSD float64   `json:"entry_premium_usd"`
	CurrentValueUSD float64   `json:"current_value_usd"`
	Greeks          OptGreeks `json:"greeks"`
	OpenedAt        time.Time `json:"opened_at"`
}

// OptGreeks holds option Greeks.
type OptGreeks struct {
	Delta float64 `json:"delta"`
	Gamma float64 `json:"gamma"`
	Theta float64 `json:"theta"`
	Vega  float64 `json:"vega"`
}

// OptionsAction from the Python check_options.py output.
type OptionsAction struct {
	Action     string    `json:"action"`
	OptionType string    `json:"option_type"`
	Strike     float64   `json:"strike"`
	Expiry     string    `json:"expiry"`
	DTE        float64   `json:"dte"`
	Premium    float64   `json:"premium"`
	PremiumUSD float64   `json:"premium_usd"`
	Greeks     OptGreeks `json:"greeks"`
}

// OptionsResult is the JSON output from check_options.py.
type OptionsResult struct {
	Strategy  string          `json:"strategy"`
	Underlying string         `json:"underlying"`
	Signal    int             `json:"signal"`
	SpotPrice float64         `json:"spot_price"`
	Actions   []OptionsAction `json:"actions"`
	IVRank    float64         `json:"iv_rank"`
	Timestamp string          `json:"timestamp"`
	Error     string          `json:"error,omitempty"`
}

// ExecuteOptionsSignal processes options signals and manages positions.
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
	cost := action.PremiumUSD
	if cost <= 0 {
		cost = action.Premium * result.SpotPrice
	}
	if cost > s.Cash*0.95 {
		logger.Info("Insufficient cash ($%.2f) for option buy ($%.2f)", s.Cash, cost)
		return 0, nil
	}

	posID := fmt.Sprintf("%s-%s-%s-%.0f-%s",
		result.Underlying, action.OptionType, action.Action, action.Strike, action.Expiry)

	s.Cash -= cost
	s.OptionPositions[posID] = &OptionPosition{
		ID:              posID,
		Underlying:      result.Underlying,
		OptionType:      action.OptionType,
		Strike:          action.Strike,
		Expiry:          action.Expiry,
		DTE:             action.DTE,
		Action:          "buy",
		Quantity:        1.0,
		EntryPremium:    action.Premium,
		EntryPremiumUSD: cost,
		CurrentValueUSD: cost, // initial value = cost
		Greeks:          action.Greeks,
		OpenedAt:        time.Now().UTC(),
	}

	trade := Trade{
		Timestamp:  time.Now().UTC(),
		StrategyID: s.ID,
		Symbol:     fmt.Sprintf("%s-%s-%.0f-%s", result.Underlying, action.OptionType, action.Strike, action.Expiry),
		Side:       "buy",
		Quantity:   1.0,
		Price:      cost,
		Value:      cost,
		TradeType:  "options",
		Details:    fmt.Sprintf("Buy %s %s strike=%.0f exp=%s premium=$%.2f", result.Underlying, action.OptionType, action.Strike, action.Expiry, cost),
	}
	s.TradeHistory = append(s.TradeHistory, trade)
	logger.Info("BUY OPTION %s %s strike=%.0f exp=%s | $%.2f", result.Underlying, action.OptionType, action.Strike, action.Expiry, cost)

	return 1, nil
}

func executeOptionSell(s *StrategyState, result *OptionsResult, action *OptionsAction, logger *StrategyLogger) (int, error) {
	premium := action.PremiumUSD
	if premium <= 0 {
		premium = action.Premium * result.SpotPrice
	}

	posID := fmt.Sprintf("%s-%s-%s-%.0f-%s",
		result.Underlying, action.OptionType, action.Action, action.Strike, action.Expiry)

	s.Cash += premium
	s.OptionPositions[posID] = &OptionPosition{
		ID:              posID,
		Underlying:      result.Underlying,
		OptionType:      action.OptionType,
		Strike:          action.Strike,
		Expiry:          action.Expiry,
		DTE:             action.DTE,
		Action:          "sell",
		Quantity:        1.0,
		EntryPremium:    action.Premium,
		EntryPremiumUSD: premium,
		CurrentValueUSD: -premium, // liability
		Greeks:          action.Greeks,
		OpenedAt:        time.Now().UTC(),
	}

	trade := Trade{
		Timestamp:  time.Now().UTC(),
		StrategyID: s.ID,
		Symbol:     fmt.Sprintf("%s-%s-%.0f-%s", result.Underlying, action.OptionType, action.Strike, action.Expiry),
		Side:       "sell",
		Quantity:   1.0,
		Price:      premium,
		Value:      premium,
		TradeType:  "options",
		Details:    fmt.Sprintf("Sell %s %s strike=%.0f exp=%s premium=$%.2f", result.Underlying, action.OptionType, action.Strike, action.Expiry, premium),
	}
	s.TradeHistory = append(s.TradeHistory, trade)
	logger.Info("SELL OPTION %s %s strike=%.0f exp=%s | +$%.2f", result.Underlying, action.OptionType, action.Strike, action.Expiry, premium)

	return 1, nil
}

func executeOptionClose(s *StrategyState, result *OptionsResult, action *OptionsAction, logger *StrategyLogger) (int, error) {
	closed := 0
	for id, pos := range s.OptionPositions {
		if pos.Underlying == result.Underlying && pos.Strike == action.Strike && pos.OptionType == action.OptionType {
			pnl := 0.0
			if pos.Action == "buy" {
				pnl = pos.CurrentValueUSD - pos.EntryPremiumUSD
				s.Cash += pos.CurrentValueUSD
			} else {
				pnl = pos.EntryPremiumUSD - action.PremiumUSD
				s.Cash -= action.PremiumUSD
			}
			trade := Trade{
				Timestamp:  time.Now().UTC(),
				StrategyID: s.ID,
				Symbol:     pos.ID,
				Side:       "close",
				Quantity:   pos.Quantity,
				Price:      action.PremiumUSD,
				Value:      action.PremiumUSD,
				TradeType:  "options",
				Details:    fmt.Sprintf("Close %s PnL=$%.2f", pos.ID, pnl),
			}
			s.TradeHistory = append(s.TradeHistory, trade)
			logger.Info("CLOSE OPTION %s | PnL: $%.2f", pos.ID, pnl)
			delete(s.OptionPositions, id)
			closed++
		}
	}
	return closed, nil
}

// EncodePositionsJSON serializes current option positions for passing to Python scripts.
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

// UpdateOptionPositions refreshes DTE and current values for tracked options.
func UpdateOptionPositions(s *StrategyState) {
	now := time.Now().UTC()
	for id, pos := range s.OptionPositions {
		expiry, err := time.Parse("2006-01-02", pos.Expiry)
		if err != nil {
			continue
		}
		dte := expiry.Sub(now).Hours() / 24
		pos.DTE = dte
		if dte <= 0 {
			// Expired — assume worthless for bought, full profit for sold
			if pos.Action == "buy" {
				pos.CurrentValueUSD = 0
			} else {
				pos.CurrentValueUSD = 0 // liability gone
			}
			// Could auto-close here but let the strategy handle it
			_ = id
		}
	}
}

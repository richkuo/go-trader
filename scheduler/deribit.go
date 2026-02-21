package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

const deribitAPIBase = "https://www.deribit.com/api/v2"

// DeribitTickerResponse from /public/ticker endpoint
type DeribitTickerResponse struct {
	Result struct {
		InstrumentName  string  `json:"instrument_name"`
		MarkPrice       float64 `json:"mark_price"`
		UnderlyingPrice float64 `json:"underlying_price"`
		Bid             float64 `json:"best_bid_price"`
		Ask             float64 `json:"best_ask_price"`
		Greeks          struct {
			Delta float64 `json:"delta"`
			Gamma float64 `json:"gamma"`
			Theta float64 `json:"theta"`
			Vega  float64 `json:"vega"`
		} `json:"greeks"`
	} `json:"result"`
}

// DeribitPricer fetches live option prices from Deribit
type DeribitPricer struct {
	client *http.Client
}

func NewDeribitPricer() *DeribitPricer {
	return &DeribitPricer{
		client: &http.Client{Timeout: 10 * time.Second},
	}
}

// GetOptionPrice fetches live mark price for an option
// Falls back to nearest expiry if exact doesn't exist
func (d *DeribitPricer) GetOptionPrice(underlying, optionType string, strike float64, expiry string) (float64, float64, error) {
	instrument := d.formatInstrument(underlying, optionType, strike, expiry)
	if instrument == "" {
		return 0, 0, fmt.Errorf("invalid instrument format")
	}

	// Try exact match first
	markPrice, spotPrice, err := d.fetchTicker(instrument)
	if err == nil {
		return markPrice, spotPrice, nil
	}

	// If exact doesn't exist, try to find nearest expiry with same strike
	nearestInstrument, findErr := d.findNearestExpiry(underlying, optionType, strike, expiry)
	if findErr != nil {
		return 0, 0, fmt.Errorf("exact match failed: %w, nearest search failed: %w", err, findErr)
	}

	markPrice, spotPrice, err = d.fetchTicker(nearestInstrument)
	if err != nil {
		return 0, 0, fmt.Errorf("nearest expiry %s failed: %w", nearestInstrument, err)
	}

	return markPrice, spotPrice, nil
}

// fetchTickerFull retrieves full ticker data including Greeks.
func (d *DeribitPricer) fetchTickerFull(instrument string) (*DeribitTickerResponse, error) {
	url := fmt.Sprintf("%s/public/ticker?instrument_name=%s", deribitAPIBase, instrument)
	resp, err := d.client.Get(url)
	if err != nil {
		return nil, fmt.Errorf("deribit API error: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("deribit API status %d: %s", resp.StatusCode, string(body))
	}

	var ticker DeribitTickerResponse
	if err := json.NewDecoder(resp.Body).Decode(&ticker); err != nil {
		return nil, fmt.Errorf("decode error: %w", err)
	}

	return &ticker, nil
}

// fetchSpotPrice fetches the current spot price for an underlying via its perpetual instrument.
func (d *DeribitPricer) fetchSpotPrice(underlying string) (float64, error) {
	ticker, err := d.fetchTickerFull(strings.ToUpper(underlying) + "-PERPETUAL")
	if err != nil {
		return 0, err
	}
	return ticker.Result.UnderlyingPrice, nil
}

// GetOptionPriceFull fetches live mark price, spot price, and Greeks for an option.
func (d *DeribitPricer) GetOptionPriceFull(underlying, optionType string, strike float64, expiry string) (float64, float64, OptGreeks, error) {
	instrument := d.formatInstrument(underlying, optionType, strike, expiry)
	if instrument == "" {
		return 0, 0, OptGreeks{}, fmt.Errorf("invalid instrument format")
	}

	ticker, err := d.fetchTickerFull(instrument)
	if err == nil {
		g := OptGreeks{
			Delta: ticker.Result.Greeks.Delta,
			Gamma: ticker.Result.Greeks.Gamma,
			Theta: ticker.Result.Greeks.Theta,
			Vega:  ticker.Result.Greeks.Vega,
		}
		return ticker.Result.MarkPrice, ticker.Result.UnderlyingPrice, g, nil
	}

	nearestInstrument, findErr := d.findNearestExpiry(underlying, optionType, strike, expiry)
	if findErr != nil {
		return 0, 0, OptGreeks{}, fmt.Errorf("exact match failed: %w, nearest search failed: %w", err, findErr)
	}

	ticker, err = d.fetchTickerFull(nearestInstrument)
	if err != nil {
		return 0, 0, OptGreeks{}, fmt.Errorf("nearest expiry %s failed: %w", nearestInstrument, err)
	}

	g := OptGreeks{
		Delta: ticker.Result.Greeks.Delta,
		Gamma: ticker.Result.Greeks.Gamma,
		Theta: ticker.Result.Greeks.Theta,
		Vega:  ticker.Result.Greeks.Vega,
	}
	return ticker.Result.MarkPrice, ticker.Result.UnderlyingPrice, g, nil
}

// fetchTicker retrieves ticker data for a specific instrument
func (d *DeribitPricer) fetchTicker(instrument string) (float64, float64, error) {
	url := fmt.Sprintf("%s/public/ticker?instrument_name=%s", deribitAPIBase, instrument)
	resp, err := d.client.Get(url)
	if err != nil {
		return 0, 0, fmt.Errorf("deribit API error: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(resp.Body)
		return 0, 0, fmt.Errorf("deribit API status %d: %s", resp.StatusCode, string(body))
	}

	var ticker DeribitTickerResponse
	if err := json.NewDecoder(resp.Body).Decode(&ticker); err != nil {
		return 0, 0, fmt.Errorf("decode error: %w", err)
	}

	return ticker.Result.MarkPrice, ticker.Result.UnderlyingPrice, nil
}

// findNearestExpiry searches for the nearest available expiry with the same strike
func (d *DeribitPricer) findNearestExpiry(underlying, optionType string, strike float64, targetExpiry string) (string, error) {
	targetTime, err := time.Parse("2006-01-02", targetExpiry)
	if err != nil {
		return "", fmt.Errorf("invalid target expiry: %w", err)
	}

	// Fetch available instruments
	url := fmt.Sprintf("%s/public/get_instruments?currency=%s&kind=option&expired=false", deribitAPIBase, underlying)
	resp, err := d.client.Get(url)
	if err != nil {
		return "", fmt.Errorf("instruments API error: %w", err)
	}
	defer resp.Body.Close()

	var result struct {
		Result []struct {
			InstrumentName string  `json:"instrument_name"`
			Strike         float64 `json:"strike"`
			ExpirationTS   int64   `json:"expiration_timestamp"`
		} `json:"result"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", fmt.Errorf("decode instruments error: %w", err)
	}

	// Find closest expiry with matching strike and option type
	optType := "C"
	if strings.ToLower(optionType) == "put" {
		optType = "P"
	}

	var bestInstrument string
	var minDiff int64 = 1<<63 - 1

	for _, inst := range result.Result {
		// Check if strike and type match
		if inst.Strike != strike {
			continue
		}
		if !strings.HasSuffix(inst.InstrumentName, "-"+optType) {
			continue
		}

		// Calculate time difference
		expTime := time.Unix(inst.ExpirationTS/1000, 0)
		diff := expTime.Sub(targetTime)
		if diff < 0 {
			diff = -diff
		}

		diffSeconds := int64(diff.Seconds())
		if diffSeconds < minDiff {
			minDiff = diffSeconds
			bestInstrument = inst.InstrumentName
		}
	}

	if bestInstrument == "" {
		return "", fmt.Errorf("no matching strike %.0f found", strike)
	}

	const maxToleranceSeconds = 7 * 24 * 3600
	if minDiff > maxToleranceSeconds {
		return "", fmt.Errorf("nearest expiry %s is %.1f days away, too far from target %s",
			bestInstrument, float64(minDiff)/86400, targetExpiry)
	}

	return bestInstrument, nil
}

// formatInstrument converts position data to Deribit instrument name
// Example: BTC, call, 75000, 2026-03-13 -> BTC-13MAR26-75000-C
func (d *DeribitPricer) formatInstrument(underlying, optionType string, strike float64, expiry string) string {
	// Parse expiry: 2026-03-13 -> 13MAR26
	t, err := time.Parse("2006-01-02", expiry)
	if err != nil {
		return ""
	}

	day := t.Format("02")
	month := strings.ToUpper(t.Format("Jan"))
	year := t.Format("06")

	optType := "C"
	if strings.ToLower(optionType) == "put" {
		optType = "P"
	}

	// Deribit format: BTC-13MAR26-75000-C
	instrument := fmt.Sprintf("%s-%s%s%s-%.0f-%s",
		strings.ToUpper(underlying),
		day,
		month,
		year,
		strike,
		optType,
	)

	return instrument
}

// markRequest holds the data needed to fetch a live mark price for one position.
// Populated under RLock; no mutation.
type markRequest struct {
	ID         string
	Underlying string
	OptionType string
	Expiry     string
	Action     string
	Strike     float64
	DTE        float64
	Quantity   float64
	Expired    bool
}

// markResult holds the fetched data to be applied back to a position.
// Produced without any lock; applied under Lock.
type markResult struct {
	ID              string
	DTE             float64
	CurrentValueUSD float64
	Greeks          OptGreeks
	Expired         bool // position should be deleted after applying
	Fetched         bool // price was successfully retrieved
	// Assignment fields — set when a sold option expires ITM.
	Assigned         bool
	AssignUnderlying string
	AssignOptionType string // "put" or "call"
	AssignStrike     float64
	AssignSpotPrice  float64
	AssignQuantity   float64
}

// collectMarkRequests reads position data and computes DTE. Call under RLock.
func collectMarkRequests(s *StrategyState) []markRequest {
	var reqs []markRequest
	for id, pos := range s.OptionPositions {
		expiry, err := time.Parse("2006-01-02", pos.Expiry)
		if err != nil {
			continue
		}
		dte := expiry.UTC().Sub(time.Now().UTC()).Hours() / 24
		reqs = append(reqs, markRequest{
			ID:         id,
			Underlying: pos.Underlying,
			OptionType: pos.OptionType,
			Expiry:     pos.Expiry,
			Action:     pos.Action,
			Strike:     pos.Strike,
			DTE:        dte,
			Quantity:   pos.Quantity,
			Expired:    dte <= 0,
		})
	}
	return reqs
}

// fetchMarkPrices makes Deribit HTTP calls for each request. No lock held.
func fetchMarkPrices(requests []markRequest, pricer *DeribitPricer, logger *StrategyLogger) []markResult {
	var results []markResult
	for _, req := range requests {
		if req.Expired {
			spotPrice, spotErr := pricer.fetchSpotPrice(req.Underlying)
			intrinsic := 0.0
			itm := false
			if spotErr == nil && spotPrice > 0 {
				if req.OptionType == "put" && spotPrice < req.Strike {
					intrinsic = (req.Strike - spotPrice) * req.Quantity
					itm = true
				} else if req.OptionType == "call" && spotPrice > req.Strike {
					intrinsic = (spotPrice - req.Strike) * req.Quantity
					itm = true
				}
			}
			currentValue := intrinsic
			if req.Action == "sell" {
				currentValue = -intrinsic
			}
			res := markResult{
				ID:              req.ID,
				DTE:             req.DTE,
				CurrentValueUSD: currentValue,
				Expired:         true,
			}
			// Sold options that expire ITM trigger assignment / call-away.
			if req.Action == "sell" && itm {
				res.Assigned = true
				res.AssignUnderlying = req.Underlying
				res.AssignOptionType = req.OptionType
				res.AssignStrike = req.Strike
				res.AssignSpotPrice = spotPrice
				res.AssignQuantity = req.Quantity
			}
			assignedStr := ""
			if res.Assigned {
				assignedStr = " [ASSIGNED]"
			}
			logger.Info("Position %s expired (DTE=%.1f), intrinsic=$%.2f%s, scheduling removal", req.ID, req.DTE, intrinsic, assignedStr)
			results = append(results, res)
			continue
		}

		markPrice, spotPrice, greeks, err := pricer.GetOptionPriceFull(req.Underlying, req.OptionType, req.Strike, req.Expiry)
		if err != nil {
			logger.Warn("Failed to fetch price for %s: %v", req.ID, err)
			continue
		}

		priceUSD := markPrice * spotPrice
		currentValue := priceUSD
		if req.Action == "sell" {
			currentValue = -priceUSD
		}
		results = append(results, markResult{
			ID:              req.ID,
			DTE:             req.DTE,
			CurrentValueUSD: currentValue,
			Greeks:          greeks,
			Fetched:         true,
		})
	}
	return results
}

// applyMarkResults writes prices/Greeks back and deletes expired positions. Call under Lock.
func applyMarkResults(s *StrategyState, results []markResult, logger *StrategyLogger) {
	for _, r := range results {
		pos, ok := s.OptionPositions[r.ID]
		if !ok {
			continue
		}
		pos.DTE = r.DTE
		if r.Expired {
			pos.CurrentValueUSD = r.CurrentValueUSD
			delete(s.OptionPositions, r.ID)
			if r.Assigned {
				applyAssignment(s, r, logger)
			} else {
				logger.Info("Removed expired position %s (OTM, worthless)", r.ID)
			}
			continue
		}
		if r.Fetched {
			pos.CurrentValueUSD = r.CurrentValueUSD
			pos.Greeks = r.Greeks
		}
	}
}

// applyAssignment models option assignment for expired sold ITM options. Call under Lock.
func applyAssignment(s *StrategyState, r markResult, logger *StrategyLogger) {
	symbol := strings.ToUpper(r.AssignUnderlying)
	switch r.AssignOptionType {
	case "put":
		// Sold put ITM: we are obligated to buy the underlying at strike.
		cost := r.AssignStrike * r.AssignQuantity
		s.Cash -= cost
		if existing, ok := s.Positions[symbol]; ok && existing.Side == "long" {
			// Weighted average cost with existing long position.
			totalQty := existing.Quantity + r.AssignQuantity
			existing.AvgCost = (existing.AvgCost*existing.Quantity + r.AssignStrike*r.AssignQuantity) / totalQty
			existing.Quantity = totalQty
		} else {
			s.Positions[symbol] = &Position{
				Symbol:   symbol,
				Quantity: r.AssignQuantity,
				AvgCost:  r.AssignStrike,
				Side:     "long",
			}
		}
		s.TradeHistory = append(s.TradeHistory, Trade{
			Timestamp:  time.Now().UTC(),
			StrategyID: s.ID,
			Symbol:     symbol,
			Side:       "buy",
			Quantity:   r.AssignQuantity,
			Price:      r.AssignStrike,
			Value:      cost,
			TradeType:  "assignment",
			Details: fmt.Sprintf("Wheel assignment: sold put expired ITM (spot=$%.2f), bought %.4f %s @ $%.0f",
				r.AssignSpotPrice, r.AssignQuantity, symbol, r.AssignStrike),
		})
		logger.Info("ASSIGNMENT: sold put %s-%.0f expired ITM (spot=$%.2f), bought %.4f %s @ $%.0f (cash debit=$%.2f)",
			r.AssignUnderlying, r.AssignStrike, r.AssignSpotPrice, r.AssignQuantity, symbol, r.AssignStrike, cost)

	case "call":
		// Sold call ITM (call-away): we must sell the underlying at strike.
		proceeds := r.AssignStrike * r.AssignQuantity
		s.Cash += proceeds
		pnl := 0.0
		if existing, ok := s.Positions[symbol]; ok && existing.Side == "long" {
			pnl = (r.AssignStrike - existing.AvgCost) * r.AssignQuantity
			newQty := existing.Quantity - r.AssignQuantity
			if newQty <= 0 {
				delete(s.Positions, symbol)
			} else {
				existing.Quantity = newQty
			}
			RecordTradeResult(&s.RiskState, pnl)
		}
		s.TradeHistory = append(s.TradeHistory, Trade{
			Timestamp:  time.Now().UTC(),
			StrategyID: s.ID,
			Symbol:     symbol,
			Side:       "sell",
			Quantity:   r.AssignQuantity,
			Price:      r.AssignStrike,
			Value:      proceeds,
			TradeType:  "assignment",
			Details: fmt.Sprintf("Wheel call-away: sold call expired ITM (spot=$%.2f), sold %.4f %s @ $%.0f PnL=$%.2f",
				r.AssignSpotPrice, r.AssignQuantity, symbol, r.AssignStrike, pnl),
		})
		logger.Info("CALL-AWAY: sold call %s-%.0f expired ITM (spot=$%.2f), sold %.4f %s @ $%.0f (proceeds=$%.2f, PnL=$%.2f)",
			r.AssignUnderlying, r.AssignStrike, r.AssignSpotPrice, r.AssignQuantity, symbol, r.AssignStrike, proceeds, pnl)
	}
}

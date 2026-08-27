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

type DeribitPricer struct {
	client  *http.Client
	baseURL string
}

func NewDeribitPricer() *DeribitPricer {
	return &DeribitPricer{
		client: &http.Client{Timeout: 10 * time.Second},
	}
}

func (d *DeribitPricer) apiBase() string {
	if d.baseURL != "" {
		return d.baseURL
	}
	return deribitAPIBase
}

func (d *DeribitPricer) GetOptionPrice(underlying, optionType string, strike float64, expiry string) (float64, float64, error) {
	instrument := d.formatInstrument(underlying, optionType, strike, expiry)
	if instrument == "" {
		return 0, 0, fmt.Errorf("invalid instrument format")
	}

	markPrice, spotPrice, err := d.fetchTicker(instrument)
	if err == nil {
		return markPrice, spotPrice, nil
	}

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

func (d *DeribitPricer) fetchTickerFull(instrument string) (*DeribitTickerResponse, error) {
	url := fmt.Sprintf("%s/public/ticker?instrument_name=%s", d.apiBase(), instrument)
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

func (d *DeribitPricer) Name() string { return "deribit" }

func (d *DeribitPricer) FetchSpotPrice(underlying string) (float64, error) {
	ticker, err := d.fetchTickerFull(strings.ToUpper(underlying) + "-PERPETUAL")
	if err != nil {
		return 0, err
	}
	return ticker.Result.UnderlyingPrice, nil
}

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

func (d *DeribitPricer) fetchTicker(instrument string) (float64, float64, error) {
	url := fmt.Sprintf("%s/public/ticker?instrument_name=%s", d.apiBase(), instrument)
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

func (d *DeribitPricer) findNearestExpiry(underlying, optionType string, strike float64, targetExpiry string) (string, error) {
	targetTime, err := time.Parse("2006-01-02", targetExpiry)
	if err != nil {
		return "", fmt.Errorf("invalid target expiry: %w", err)
	}

	url := fmt.Sprintf("%s/public/get_instruments?currency=%s&kind=option&expired=false", d.apiBase(), underlying)
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

	optType := "C"
	if strings.ToLower(optionType) == "put" {
		optType = "P"
	}

	var bestInstrument string
	var minDiff int64 = 1<<63 - 1

	for _, inst := range result.Result {
		if inst.Strike != strike {
			continue
		}
		if !strings.HasSuffix(inst.InstrumentName, "-"+optType) {
			continue
		}

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

func (d *DeribitPricer) formatInstrument(underlying, optionType string, strike float64, expiry string) string {
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

type markResult struct {
	ID              string
	DTE             float64
	CurrentValueUSD float64
	Greeks          OptGreeks
	Expired         bool
	Fetched         bool
	Assigned         bool
	AssignUnderlying string
	AssignOptionType string
	AssignStrike     float64
	AssignSpotPrice  float64
	AssignQuantity   float64
}

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

func fetchMarkPrices(requests []markRequest, pricer OptionPricer, logger *StrategyLogger) []markResult {
	var results []markResult
	for _, req := range requests {
		if req.Expired {
			spotPrice, spotErr := pricer.FetchSpotPrice(req.Underlying)
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

func applyMarkResults(s *StrategyState, results []markResult, logger *StrategyLogger) {
	now := time.Now().UTC()
	for _, r := range results {
		pos, ok := s.OptionPositions[r.ID]
		if !ok {
			continue
		}
		pos.DTE = r.DTE
		if r.Expired {
			pos.CurrentValueUSD = r.CurrentValueUSD
			var pnl, closePriceUSD float64
			intrinsic := r.CurrentValueUSD
			if pos.Action == "sell" {
				intrinsic = -intrinsic
			}
			if pos.Action == "buy" {
				pnl = intrinsic - pos.EntryPremiumUSD
			} else {
				pnl = pos.EntryPremiumUSD - intrinsic
			}
			closePriceUSD = intrinsic
			reason := "expired_worthless"
			if r.Assigned {
				reason = "expired_itm"
			}
			recordClosedOptionPosition(s, pos, closePriceUSD, pnl, reason, now)
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

func applyAssignment(s *StrategyState, r markResult, logger *StrategyLogger) {
	symbol := strings.ToUpper(r.AssignUnderlying)
	switch r.AssignOptionType {
	case "put":
		cost := r.AssignStrike * r.AssignQuantity
		s.Cash -= cost
		now := time.Now().UTC()
		var positionID string
		if existing, ok := s.Positions[symbol]; ok && existing.Side == "long" {
			totalQty := existing.Quantity + r.AssignQuantity
			existing.AvgCost = (existing.AvgCost*existing.Quantity + r.AssignStrike*r.AssignQuantity) / totalQty
			existing.Quantity = totalQty
			positionID = ensurePositionTradeID(s.ID, symbol, existing)
		} else {
			positionID = newTradePositionID(s.ID, symbol, now)
			s.Positions[symbol] = &Position{
				Symbol:          symbol,
				TradePositionID: positionID,
				Quantity:        r.AssignQuantity,
				InitialQuantity: r.AssignQuantity,
				AvgCost:         r.AssignStrike,
				Side:            "long",
				OpenedAt:        now,
			}
		}
		RecordTrade(s, Trade{
			Timestamp:  now,
			StrategyID: s.ID,
			Symbol:     symbol,
			PositionID: positionID,
			Side:       "buy",
			Quantity:   r.AssignQuantity,
			Price:      r.AssignStrike,
			Value:      cost,
			TradeType:  "assignment",
			Details: fmt.Sprintf("Wheel assignment: sold put expired ITM (spot=$%.2f), bought %.4f %s @ $%.0f",
				r.AssignSpotPrice, r.AssignQuantity, symbol, r.AssignStrike),
			Regime: s.Regime,
		})
		logger.Info("ASSIGNMENT: sold put %s-%.0f expired ITM (spot=$%.2f), bought %.4f %s @ $%.0f (cash debit=$%.2f)",
			r.AssignUnderlying, r.AssignStrike, r.AssignSpotPrice, r.AssignQuantity, symbol, r.AssignStrike, cost)

	case "call":
		now := time.Now().UTC()
		proceeds := r.AssignStrike * r.AssignQuantity
		s.Cash += proceeds
		pnl := 0.0
		var positionID string
		var posEntryATR, posStopLossTriggerPx float64
		if existing, ok := s.Positions[symbol]; ok && existing.Side == "long" {
			positionID = ensurePositionTradeID(s.ID, symbol, existing)
			pnl = (r.AssignStrike - existing.AvgCost) * r.AssignQuantity
			posEntryATR = existing.EntryATR
			posStopLossTriggerPx = existing.StopLossTriggerPx
			newQty := existing.Quantity - r.AssignQuantity
			if newQty <= 0 {
				recordClosedPosition(s, existing, r.AssignStrike, pnl, "assignment", now)
				delete(s.Positions, symbol)
			} else {
				existing.Quantity = newQty
			}
			RecordTradeResult(&s.RiskState, pnl)
		}
		RecordTrade(s, Trade{
			Timestamp:  now,
			StrategyID: s.ID,
			Symbol:     symbol,
			PositionID: positionID,
			Side:       "sell",
			Quantity:   r.AssignQuantity,
			Price:      r.AssignStrike,
			Value:      proceeds,
			TradeType:  "assignment",
			Details: fmt.Sprintf("Wheel call-away: sold call expired ITM (spot=$%.2f), sold %.4f %s @ $%.0f PnL=$%.2f",
				r.AssignSpotPrice, r.AssignQuantity, symbol, r.AssignStrike, pnl),
			IsClose:           true,
			RealizedPnL:       pnl,
			PnLGross:          true,
			Regime:            s.Regime,
			EntryATR:          posEntryATR,
			StopLossTriggerPx: posStopLossTriggerPx,
		})
		logger.Info("CALL-AWAY: sold call %s-%.0f expired ITM (spot=$%.2f), sold %.4f %s @ $%.0f (proceeds=$%.2f, PnL=$%.2f)",
			r.AssignUnderlying, r.AssignStrike, r.AssignSpotPrice, r.AssignQuantity, symbol, r.AssignStrike, proceeds, pnl)
	}
}

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
		InstrumentName string  `json:"instrument_name"`
		MarkPrice      float64 `json:"mark_price"`
		UnderlyingPrice float64 `json:"underlying_price"`
		Bid            float64 `json:"best_bid_price"`
		Ask            float64 `json:"best_ask_price"`
		Greeks         struct {
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

// MarkOptionPositions updates all option positions with live Deribit prices
func MarkOptionPositions(s *StrategyState, pricer *DeribitPricer, logger *StrategyLogger) error {
	if len(s.OptionPositions) == 0 {
		return nil
	}

	var toDelete []string
	for id, pos := range s.OptionPositions {
		// Update DTE first
		expiry, err := time.Parse("2006-01-02", pos.Expiry)
		if err != nil {
			logger.Warn("Invalid expiry for %s: %v", id, err)
			continue
		}
		// Use UTC for both sides to avoid 1-day errors on non-UTC servers
		dte := expiry.UTC().Sub(time.Now().UTC()).Hours() / 24
		pos.DTE = dte

		// Mark expired positions — model assignment cost for sold ITM options
		if dte <= 0 {
			spotPrice, spotErr := pricer.fetchSpotPrice(pos.Underlying)
			intrinsic := 0.0
			if spotErr == nil && spotPrice > 0 {
				if pos.OptionType == "put" && spotPrice < pos.Strike {
					intrinsic = (pos.Strike - spotPrice) * pos.Quantity
				} else if pos.OptionType == "call" && spotPrice > pos.Strike {
					intrinsic = (spotPrice - pos.Strike) * pos.Quantity
				}
			}
			if pos.Action == "buy" {
				pos.CurrentValueUSD = intrinsic
			} else {
				pos.CurrentValueUSD = -intrinsic
			}
			logger.Info("Position %s expired (DTE=%.1f), intrinsic=$%.2f, scheduling removal", id, dte, intrinsic)
			toDelete = append(toDelete, id)
			continue
		}

		// Fetch live price and Greeks from Deribit
		markPrice, spotPrice, greeks, err := pricer.GetOptionPriceFull(pos.Underlying, pos.OptionType, pos.Strike, pos.Expiry)
		if err != nil {
			logger.Warn("Failed to fetch price for %s: %v", id, err)
			continue
		}

		// Convert mark price (in BTC/ETH terms) to USD
		priceUSD := markPrice * spotPrice

		// Update current value based on action
		if pos.Action == "buy" {
			pos.CurrentValueUSD = priceUSD
		} else { // sell
			pos.CurrentValueUSD = -priceUSD
		}

		// Update Greeks with live values from Deribit
		pos.Greeks = greeks

		_ = id // mark used
	}

	// Remove expired positions accumulated above
	for _, id := range toDelete {
		delete(s.OptionPositions, id)
		logger.Info("Removed expired position %s", id)
	}

	return nil
}

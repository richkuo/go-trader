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

	for id, pos := range s.OptionPositions {
		// Update DTE first
		expiry, err := time.Parse("2006-01-02", pos.Expiry)
		if err != nil {
			logger.Warn("Invalid expiry for %s: %v", id, err)
			continue
		}
		dte := time.Until(expiry).Hours() / 24
		pos.DTE = dte

		// Skip expired positions
		if dte <= 0 {
			if pos.Action == "buy" {
				pos.CurrentValueUSD = 0
			} else {
				pos.CurrentValueUSD = 0 // liability expired worthless
			}
			logger.Info("Position %s expired (DTE=%.1f)", id, dte)
			continue
		}

		// Fetch live price from Deribit
		markPrice, spotPrice, err := pricer.GetOptionPrice(pos.Underlying, pos.OptionType, pos.Strike, pos.Expiry)
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

		// Update Greeks if available (optional, could fetch from ticker response)
		// pos.Greeks = ...

		_ = id // mark used
	}

	return nil
}

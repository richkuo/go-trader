package main

import (
	"fmt"
	"math"
	"strings"
	"time"
)

// IBKRPricer implements OptionPricer using Black-Scholes for IBKR/CME crypto options.
// Uses spot prices from the cycle's price cache rather than live API calls.
type IBKRPricer struct {
	spotPrices map[string]float64
}

func NewIBKRPricer(spotPrices map[string]float64) *IBKRPricer {
	return &IBKRPricer{spotPrices: spotPrices}
}

func (p *IBKRPricer) Name() string { return "ibkr" }

// FetchSpotPrice looks up the spot price from the cached prices map.
func (p *IBKRPricer) FetchSpotPrice(underlying string) (float64, error) {
	upper := strings.ToUpper(underlying)
	for _, suffix := range []string{"/USD", "/USDT", "/USDC"} {
		if price, ok := p.spotPrices[upper+suffix]; ok && price > 0 {
			return price, nil
		}
	}
	return 0, fmt.Errorf("no spot price cached for %s", underlying)
}

// GetOptionPriceFull prices an option using Black-Scholes with default vol (30%).
// Returns (markPrice in underlying terms, spotPrice in USD, Greeks, error).
func (p *IBKRPricer) GetOptionPriceFull(underlying, optionType string, strike float64, expiry string) (float64, float64, OptGreeks, error) {
	spot, err := p.FetchSpotPrice(underlying)
	if err != nil {
		return 0, 0, OptGreeks{}, err
	}

	t, err := time.Parse("2006-01-02", expiry)
	if err != nil {
		return 0, 0, OptGreeks{}, fmt.Errorf("invalid expiry %q: %w", expiry, err)
	}
	dte := t.UTC().Sub(time.Now().UTC()).Hours() / 24
	if dte <= 0 {
		return 0, spot, OptGreeks{}, nil
	}
	T := dte / 365.0

	const vol = 0.80 // crypto default implied vol (80%)
	const r = 0.05   // risk-free rate

	price, delta, gamma, vega, theta := bsPrice(spot, strike, T, r, vol, strings.ToLower(optionType))

	greeks := OptGreeks{
		Delta: delta,
		Gamma: gamma,
		Theta: theta,
		Vega:  vega,
	}

	// Deribit convention: mark price expressed as fraction of underlying spot.
	markPrice := 0.0
	if spot > 0 {
		markPrice = price / spot
	}

	return markPrice, spot, greeks, nil
}

// bsPrice computes Black-Scholes option price and Greeks.
func bsPrice(S, K, T, r, sigma float64, optionType string) (price, delta, gamma, vega, theta float64) {
	if T <= 0 || sigma <= 0 || S <= 0 || K <= 0 {
		return 0, 0, 0, 0, 0
	}

	d1 := (math.Log(S/K) + (r+0.5*sigma*sigma)*T) / (sigma * math.Sqrt(T))
	d2 := d1 - sigma*math.Sqrt(T)

	nd1 := stdNormCDF(d1)
	nd2 := stdNormCDF(d2)
	pdfD1 := stdNormPDF(d1)

	if optionType == "call" {
		price = S*nd1 - K*math.Exp(-r*T)*nd2
		delta = nd1
		theta = (-S*pdfD1*sigma/(2*math.Sqrt(T)) - r*K*math.Exp(-r*T)*nd2) / 365
	} else {
		price = K*math.Exp(-r*T)*stdNormCDF(-d2) - S*stdNormCDF(-d1)
		delta = nd1 - 1
		theta = (-S*pdfD1*sigma/(2*math.Sqrt(T)) + r*K*math.Exp(-r*T)*stdNormCDF(-d2)) / 365
	}

	gamma = pdfD1 / (S * sigma * math.Sqrt(T))
	vega = S * pdfD1 * math.Sqrt(T)

	return
}

func stdNormCDF(x float64) float64 {
	return 0.5 * math.Erfc(-x/math.Sqrt2)
}

func stdNormPDF(x float64) float64 {
	return math.Exp(-0.5*x*x) / math.Sqrt(2*math.Pi)
}

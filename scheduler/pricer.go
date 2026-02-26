package main

// OptionPricer is the interface for fetching live option prices and Greeks.
// Implementations: DeribitPricer (live API), IBKRPricer (Black-Scholes).
type OptionPricer interface {
	// GetOptionPriceFull returns (markPrice, spotPrice, Greeks, error).
	// markPrice is in underlying terms (e.g. BTC), spotPrice is in USD.
	GetOptionPriceFull(underlying, optionType string, strike float64, expiry string) (float64, float64, OptGreeks, error)

	// FetchSpotPrice returns the current USD spot price for an underlying.
	FetchSpotPrice(underlying string) (float64, error)

	// Name returns the platform name (e.g. "deribit", "ibkr").
	Name() string
}

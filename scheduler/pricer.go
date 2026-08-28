package main

type OptionPricer interface {
	GetOptionPriceFull(underlying, optionType string, strike float64, expiry string) (float64, float64, OptGreeks, error)

	FetchSpotPrice(underlying string) (float64, error)

	Name() string
}

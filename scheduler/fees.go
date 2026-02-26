package main

import "math/rand"

// Fee rates for different exchange types
const (
	// Binance US spot trading fees (taker fee)
	BinanceSpotFeePct = 0.001 // 0.1% taker fee

	// Deribit options fees (maker/taker)
	DeribitOptionFeePct = 0.0003 // 0.03% of contract value

	// IBKR options fees (per contract, CME Micro)
	IBKROptionFeeFixed = 0.25 // $0.25 per contract (CME Micro fee)

	// Slippage simulation (random +/- this pct)
	SlippagePct = 0.0005 // 0.05% (5 basis points)
)

// ApplySlippage simulates price slippage between signal and execution
func ApplySlippage(price float64) float64 {
	// Random slippage between -0.05% and +0.05%
	slippage := (rand.Float64()*2 - 1) * SlippagePct
	return price * (1 + slippage)
}

// CalculateSpotFee calculates trading fee for spot trade
func CalculateSpotFee(value float64) float64 {
	return value * BinanceSpotFeePct
}

// CalculateDeribitOptionFee calculates trading fee for Deribit options
func CalculateDeribitOptionFee(premiumUSD float64) float64 {
	return premiumUSD * DeribitOptionFeePct
}

// CalculateIBKROptionFee calculates trading fee for IBKR/CME options
func CalculateIBKROptionFee(quantity float64) float64 {
	return quantity * IBKROptionFeeFixed
}

// CalculateOptionFee dispatches to the appropriate fee calculator based on platform.
func CalculateOptionFee(platform string, premiumUSD, quantity float64) float64 {
	if platform == "ibkr" {
		return CalculateIBKROptionFee(quantity)
	}
	return CalculateDeribitOptionFee(premiumUSD)
}

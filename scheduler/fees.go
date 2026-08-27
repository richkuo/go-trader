package main

import "math/rand"

const (
	BinanceSpotFeePct = 0.001

	DeribitOptionFeePct = 0.0003

	IBKROptionFeeFixed = 0.25

	HyperliquidTakerFeePct = 0.00045
	HyperliquidMakerFeePct = 0.00015

	LunoTakerFeePct = 0.01

	RobinhoodOptionFeeFixed = 0.03

	OKXSpotTakerFeePct  = 0.001
	OKXPerpsTakerFeePct = 0.0005
	OKXOptionFeePct     = 0.0003

	SlippagePct = 0.0005
)

func ApplySlippage(price float64) float64 {

	slippage := (rand.Float64()*2 - 1) * SlippagePct
	return price * (1 + slippage)
}

func CalculateSpotFee(value float64) float64 {
	return value * BinanceSpotFeePct
}

func CalculateHyperliquidFee(notionalUSD float64) float64 {
	return notionalUSD * HyperliquidTakerFeePct
}

func CalculatePlatformSpotFee(platform string, value float64) float64 {
	switch platform {
	case "hyperliquid":
		return CalculateHyperliquidFee(value)
	case "luno":
		return value * LunoTakerFeePct
	case "robinhood":
		return 0
	case "okx":
		return value * OKXSpotTakerFeePct
	case "okx-perps":
		return value * OKXPerpsTakerFeePct
	default:
		return CalculateSpotFee(value)
	}
}

func CalculateDeribitOptionFee(premiumUSD float64) float64 {
	return premiumUSD * DeribitOptionFeePct
}

func CalculateIBKROptionFee(quantity float64) float64 {
	return quantity * IBKROptionFeeFixed
}

func CalculateRobinhoodOptionFee(quantity float64) float64 {
	return quantity * RobinhoodOptionFeeFixed
}

func CalculateOptionFee(platform string, premiumUSD, quantity float64) float64 {
	switch platform {
	case "ibkr":
		return CalculateIBKROptionFee(quantity)
	case "robinhood":
		return CalculateRobinhoodOptionFee(quantity)
	case "okx":
		return premiumUSD * OKXOptionFeePct
	default:
		return CalculateDeribitOptionFee(premiumUSD)
	}
}

func CalculateFuturesFee(contracts int, feePerContract float64) float64 {
	return float64(contracts) * feePerContract
}

func CalculatePlatformFuturesFee(sc StrategyConfig, contracts int) float64 {
	if sc.FuturesConfig != nil && sc.FuturesConfig.FeePerContract > 0 {
		return CalculateFuturesFee(contracts, sc.FuturesConfig.FeePerContract)
	}
	return 0
}

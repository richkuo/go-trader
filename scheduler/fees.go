package main

import "fmt"

const (
	BinanceSpotFeePct = 0.001

	IBKROptionFeeFixed = 0.25

	HyperliquidTakerFeePct = 0.00045
	HyperliquidMakerFeePct = 0.00015

	LunoTakerFeePct = 0.01

	RobinhoodOptionFeeFixed = 0.03

	OKXSpotTakerFeePct  = 0.001
	OKXPerpsTakerFeePct = 0.0005

	SlippagePct = 0.0005

	FillCostModelVersion = 1
)

func stampTradeCostModel(trade *Trade) {
	if trade.CostModelVersion == 0 {
		trade.CostModelVersion = FillCostModelVersion
	}
}

func ApplyAdverseSlippage(price float64, isBuy bool) float64 {
	return applySlippageRate(price, isBuy, SlippagePct)
}

func applySlippageRate(price float64, isBuy bool, slip float64) float64 {
	if isBuy {
		return price * (1 + slip)
	}
	return price * (1 - slip)
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

func CalculateIBKROptionFee(quantity float64) float64 {
	return quantity * IBKROptionFeeFixed
}

func CalculateRobinhoodOptionFee(quantity float64) float64 {
	return quantity * RobinhoodOptionFeeFixed
}

func CalculateOptionFee(platform string, quantity float64) (float64, error) {
	switch platform {
	case "ibkr":
		return CalculateIBKROptionFee(quantity), nil
	case "robinhood":
		return CalculateRobinhoodOptionFee(quantity), nil
	default:
		return 0, fmt.Errorf("no option fee schedule for platform %q (supported: ibkr, robinhood)", platform)
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

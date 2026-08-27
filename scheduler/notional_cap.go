package main


import "fmt"

func notionalCapSkipsStrategyCycle(notionalBlocked bool) bool {
	_ = notionalBlocked
	return false
}

func notionalCapHoldDetail(totalNotional, capUSD float64) string {
	return fmt.Sprintf("portfolio notional $%.2f exceeds cap $%.2f — new opens blocked, exits continue",
		totalNotional, capUSD)
}

func evaluateNotionalCapHold(pr *PortfolioRiskConfig, strategies map[string]*StrategyState, prices map[string]float64) (held bool, detail string) {
	if pr == nil || pr.MaxNotionalUSD <= 0 {
		return false, ""
	}
	total := PortfolioNotional(strategies, prices)
	if total > pr.MaxNotionalUSD {
		return true, notionalCapHoldDetail(total, pr.MaxNotionalUSD)
	}
	return false, ""
}

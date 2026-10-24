package main

const (
	inventoryMarketMakingBookCapacity = 256
	inventoryMarketMakingBookDepth    = 20
	inventoryMarketMakingPredecessor  = int64(1_000)
)

func init() {
	feedBookStrategyNeeds[inventoryMarketMakingStrategyName] = inventoryMarketMakingBookNeed
}

func inventoryMarketMakingBookWindowMs(p quotePolicyParams) int64 {
	return int64(p.VolatilityWindowSeconds)*1000 + inventoryMarketMakingPredecessor + int64(p.MaxInputAgeMs)
}

func inventoryMarketMakingBookNeed(sc StrategyConfig, intervalMs int64) (feedBookNeed, error) {
	p, err := parseQuotePolicyParams(sc.OpenStrategy.Params)
	if err != nil {
		return feedBookNeed{}, err
	}
	return contractBookNeed(inventoryMarketMakingBookWindowMs(p), inventoryMarketMakingBookCapacity, inventoryMarketMakingBookDepth), nil
}

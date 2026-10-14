package main

import (
	"fmt"
	"strings"
)

const (
	bitcoinLeadLagStrategyName = "bitcoin_lead_lag"
	bitcoinLeadLagLeaderCoin   = "BTC"

	bitcoinLeadLagDefaultLagBars      = 1
	bitcoinLeadLagDefaultHorizonBars  = 4
	bitcoinLeadLagDefaultTrainingBars = 240
)

type bitcoinLeadLagBound struct {
	name     string
	def      int64
	min, max int64
}

var bitcoinLeadLagSupportParams = []bitcoinLeadLagBound{
	{"lag_bars", bitcoinLeadLagDefaultLagBars, 1, 6},
	{"horizon_bars", bitcoinLeadLagDefaultHorizonBars, 1, 12},
	{"training_bars", bitcoinLeadLagDefaultTrainingBars, 48, 720},
}

func feedStrategyUsesLeaderFrame(sc StrategyConfig) bool {
	return strings.TrimSpace(effectiveOpenStrategy(sc)) == bitcoinLeadLagStrategyName
}

func bitcoinLeadLagSupportBars(sc StrategyConfig) (int, error) {
	values := make(map[string]int64, len(bitcoinLeadLagSupportParams))
	for _, b := range bitcoinLeadLagSupportParams {
		v, err := openInterestParamInt(sc.OpenStrategy.Params, b.name, b.def)
		if err != nil {
			return 0, err
		}
		if v < b.min || v > b.max {
			return 0, fmt.Errorf("%s must be in [%d, %d], got %d", b.name, b.min, b.max, v)
		}
		values[b.name] = v
	}
	if values["training_bars"]%2 != 0 {
		return 0, fmt.Errorf("training_bars must be even, got %d", values["training_bars"])
	}
	return int(values["training_bars"] + 2*values["lag_bars"] + 2*values["horizon_bars"] - 2), nil
}

func bitcoinLeadLagFrameRows(sc StrategyConfig) (int, error) {
	support, err := bitcoinLeadLagSupportBars(sc)
	if err != nil {
		return 0, err
	}
	return support + 1, nil
}

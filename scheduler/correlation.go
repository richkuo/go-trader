package main

import (
	"fmt"
	"math"
	"sort"
	"strings"
	"time"
)

type StrategyExposure struct {
	StrategyID string  `json:"strategy_id"`
	DeltaUSD   float64 `json:"delta_usd"`
	Type       string  `json:"type"`
}

type AssetExposure struct {
	Asset            string             `json:"asset"`
	NetDeltaUSD      float64            `json:"net_delta_usd"`
	GrossDeltaUSD    float64            `json:"gross_delta_usd"`
	Strategies       []StrategyExposure `json:"strategies"`
	ConcentrationPct float64            `json:"concentration_pct"`
}

type CorrelationSnapshot struct {
	Timestamp         time.Time                 `json:"timestamp"`
	Assets            map[string]*AssetExposure `json:"assets"`
	PortfolioGrossUSD float64                   `json:"portfolio_gross_usd"`
	Warnings          []string                  `json:"warnings,omitempty"`
}

func computeAssetDeltas(strategies map[string]*StrategyState, cfgStrategies []StrategyConfig, prices map[string]float64) (map[string]*AssetExposure, []string) {
	cfgMap := make(map[string]StrategyConfig)
	for _, sc := range cfgStrategies {
		cfgMap[sc.ID] = sc
	}
	ids := make([]string, 0, len(strategies))
	for id := range strategies {
		ids = append(ids, id)
	}
	sort.Strings(ids)

	assets := make(map[string]*AssetExposure)
	var skipped []string

	for _, id := range ids {
		ss := strategies[id]
		if ss == nil {
			continue
		}
		sc, ok := cfgMap[id]
		if !ok {
			continue
		}
		asset := extractAsset(sc)
		if asset == "" {
			continue
		}

		spotPrice := findSpotPrice(asset, prices)

		var deltaUSD float64

		switch sc.Type {
		case "spot", "perps", "manual":

			syms := make([]string, 0, len(ss.Positions))
			for sym := range ss.Positions {
				syms = append(syms, sym)
			}
			sort.Strings(syms)
			for _, sym := range syms {
				pos := ss.Positions[sym]
				if pos == nil {
					continue
				}
				posAsset := strings.TrimSuffix(strings.ToUpper(pos.Symbol), "/USDT")
				if posAsset != asset {
					continue
				}
				if pos.Quantity <= 0 {
					skipped = append(skipped, fmt.Sprintf("%s/%s: non-positive quantity", id, pos.Symbol))
					continue
				}
				px := spotPrice
				if px <= 0 {
					px = pos.AvgCost
				}
				if px <= 0 {
					skipped = append(skipped, fmt.Sprintf("%s/%s: no usable price", id, pos.Symbol))
					continue
				}
				legUSD := pos.Quantity * positionMultiplier(pos) * px
				if pos.Side == "short" {
					deltaUSD -= legUSD
				} else {
					deltaUSD += legUSD
				}
			}
		case "options":
			if spotPrice <= 0 {
				if len(ss.OptionPositions) > 0 {
					skipped = append(skipped, fmt.Sprintf("%s: no usable spot price for options delta", id))
				}
				continue
			}
			optIDs := make([]string, 0, len(ss.OptionPositions))
			for oid := range ss.OptionPositions {
				optIDs = append(optIDs, oid)
			}
			sort.Strings(optIDs)
			for _, oid := range optIDs {
				opt := ss.OptionPositions[oid]
				if opt == nil {
					continue
				}
				optAsset := strings.ToUpper(opt.Underlying)
				if optAsset != asset {
					continue
				}
				sign := 1.0
				if opt.Action == "sell" {
					sign = -1.0
				}
				if opt.Greeks.Delta != 0 {
					deltaUSD += sign * opt.Greeks.Delta * opt.Quantity * spotPrice
				} else {

					coarseDelta := 1.0
					if opt.OptionType == "put" {
						coarseDelta = -1.0
					}
					deltaUSD += sign * coarseDelta * opt.Quantity * spotPrice
				}
			}
		}

		if deltaUSD == 0 {
			continue
		}

		ae, exists := assets[asset]
		if !exists {
			ae = &AssetExposure{Asset: asset}
			assets[asset] = ae
		}
		ae.Strategies = append(ae.Strategies, StrategyExposure{
			StrategyID: id,
			DeltaUSD:   deltaUSD,
			Type:       sc.Type,
		})
		ae.NetDeltaUSD += deltaUSD
		ae.GrossDeltaUSD += math.Abs(deltaUSD)
	}

	sort.Strings(skipped)
	return assets, skipped
}

func ComputeCorrelation(strategies map[string]*StrategyState, cfgStrategies []StrategyConfig, prices map[string]float64, corrCfg *CorrelationConfig) *CorrelationSnapshot {
	snap := &CorrelationSnapshot{
		Timestamp: time.Now().UTC(),
	}
	snap.Assets, _ = computeAssetDeltas(strategies, cfgStrategies, prices)

	for _, ae := range snap.Assets {
		snap.PortfolioGrossUSD += ae.GrossDeltaUSD
	}

	if snap.PortfolioGrossUSD > 0 {
		for _, ae := range snap.Assets {
			ae.ConcentrationPct = math.Abs(ae.NetDeltaUSD) / snap.PortfolioGrossUSD * 100

			if corrCfg != nil && ae.ConcentrationPct > corrCfg.MaxConcentrationPct {
				direction := "long"
				if ae.NetDeltaUSD < 0 {
					direction = "short"
				}
				snap.Warnings = append(snap.Warnings,
					fmt.Sprintf("%s concentration %.0f%% (net %s $%.0f) exceeds %.0f%% threshold",
						ae.Asset, ae.ConcentrationPct, direction, math.Abs(ae.NetDeltaUSD), corrCfg.MaxConcentrationPct))
			}
		}
	}

	if corrCfg != nil {
		for _, ae := range snap.Assets {
			if len(ae.Strategies) < 2 {
				continue
			}
			longCount, shortCount := 0, 0
			for _, se := range ae.Strategies {
				if se.DeltaUSD > 0 {
					longCount++
				} else if se.DeltaUSD < 0 {
					shortCount++
				}
			}
			maxSame := longCount
			direction := "long"
			if shortCount > longCount {
				maxSame = shortCount
				direction = "short"
			}
			sameDirectionPct := float64(maxSame) / float64(len(ae.Strategies)) * 100
			if sameDirectionPct > corrCfg.MaxSameDirectionPct {
				snap.Warnings = append(snap.Warnings,
					fmt.Sprintf("%s: %d/%d strategies %s (%.0f%%) exceeds %.0f%% same-direction threshold",
						ae.Asset, maxSame, len(ae.Strategies), direction, sameDirectionPct, corrCfg.MaxSameDirectionPct))
			}
		}
	}

	return snap
}

func findSpotPrice(asset string, prices map[string]float64) float64 {

	if p, ok := prices[asset+"/USDT"]; ok {
		return p
	}
	if p, ok := prices[asset]; ok {
		return p
	}

	for sym, p := range prices {
		base := strings.ToUpper(strings.SplitN(sym, "/", 2)[0])
		if base == asset {
			return p
		}
	}
	return 0
}

package main

import (
	"fmt"
	"math"
	"strings"
	"time"
)

// StrategyExposure represents a single strategy's directional exposure to an asset.
type StrategyExposure struct {
	StrategyID string  `json:"strategy_id"`
	DeltaUSD   float64 `json:"delta_usd"` // signed: +long, -short
	Type       string  `json:"type"`      // spot/options/perps
}

// AssetExposure aggregates all strategies' exposure to a single asset.
type AssetExposure struct {
	Asset            string             `json:"asset"`
	NetDeltaUSD      float64            `json:"net_delta_usd"`
	GrossDeltaUSD    float64            `json:"gross_delta_usd"`
	Strategies       []StrategyExposure `json:"strategies"`
	ConcentrationPct float64            `json:"concentration_pct"` // |net|/portfolio_gross * 100
}

// CorrelationSnapshot captures portfolio-level directional exposure at a point in time.
type CorrelationSnapshot struct {
	Timestamp         time.Time                 `json:"timestamp"`
	Assets            map[string]*AssetExposure `json:"assets"`
	PortfolioGrossUSD float64                   `json:"portfolio_gross_usd"`
	Warnings          []string                  `json:"warnings,omitempty"`
}

// ComputeCorrelation computes per-asset directional exposure across all strategies.
func ComputeCorrelation(strategies map[string]*StrategyState, cfgStrategies []StrategyConfig, prices map[string]float64, corrCfg *CorrelationConfig) *CorrelationSnapshot {
	snap := &CorrelationSnapshot{
		Timestamp: time.Now().UTC(),
		Assets:    make(map[string]*AssetExposure),
	}

	// Build config lookup for asset extraction and type info.
	cfgMap := make(map[string]StrategyConfig)
	for _, sc := range cfgStrategies {
		cfgMap[sc.ID] = sc
	}

	// Compute per-strategy delta-USD and group by asset.
	for id, ss := range strategies {
		sc, ok := cfgMap[id]
		if !ok {
			continue
		}
		asset := extractAsset(sc)
		if asset == "" {
			continue
		}

		// Find the spot price for this asset.
		spotPrice := findSpotPrice(asset, prices)
		if spotPrice <= 0 {
			continue
		}

		var deltaUSD float64

		switch sc.Type {
		case "spot", "perps":
			for _, pos := range ss.Positions {
				posAsset := strings.TrimSuffix(strings.ToUpper(pos.Symbol), "/USDT")
				if posAsset != asset {
					continue
				}
				if pos.Side == "long" {
					deltaUSD += pos.Quantity * spotPrice
				} else {
					deltaUSD -= pos.Quantity * spotPrice
				}
			}
		case "options":
			for _, opt := range ss.OptionPositions {
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
					// Coarse estimate when greeks not yet marked.
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

		ae, exists := snap.Assets[asset]
		if !exists {
			ae = &AssetExposure{Asset: asset}
			snap.Assets[asset] = ae
		}
		ae.Strategies = append(ae.Strategies, StrategyExposure{
			StrategyID: id,
			DeltaUSD:   deltaUSD,
			Type:       sc.Type,
		})
		ae.NetDeltaUSD += deltaUSD
		ae.GrossDeltaUSD += math.Abs(deltaUSD)
	}

	// Compute portfolio gross.
	for _, ae := range snap.Assets {
		snap.PortfolioGrossUSD += ae.GrossDeltaUSD
	}

	// Compute concentration percentages and generate warnings.
	if snap.PortfolioGrossUSD > 0 {
		for _, ae := range snap.Assets {
			ae.ConcentrationPct = math.Abs(ae.NetDeltaUSD) / snap.PortfolioGrossUSD * 100

			// Concentration warning.
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

	// Same-direction warning: check if too many strategies share a direction per asset.
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

// findSpotPrice finds a price for the given asset (e.g. "BTC") from the prices map.
func findSpotPrice(asset string, prices map[string]float64) float64 {
	// Try common symbol formats.
	if p, ok := prices[asset+"/USDT"]; ok {
		return p
	}
	if p, ok := prices[asset]; ok {
		return p
	}
	// Fallback: scan for any symbol starting with asset.
	for sym, p := range prices {
		base := strings.ToUpper(strings.SplitN(sym, "/", 2)[0])
		if base == asset {
			return p
		}
	}
	return 0
}

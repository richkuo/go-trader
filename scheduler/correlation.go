package main

import (
	"fmt"
	"math"
	"sort"
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

// computeAssetDeltas computes signed per-asset delta-USD exposure across all
// strategies. Shared by ComputeCorrelation (advisory snapshot / warnings) and
// evaluateExposureCap (#1270 blocking gate) so there is exactly one exposure
// model. Semantics:
//
//   - spot, perps, AND manual positions contribute quantity x multiplier x
//     price, signed by Side ("short" is negative; anything else — including
//     long-only spot legs with an empty Side — counts long).
//   - options contribute delta-weighted underlying exposure (emitted greeks
//     when marked, coarse +-1 call/put fallback otherwise), signed by action.
//   - positions with no live price fall back to AvgCost (mirrors
//     PortfolioNotional); a position with neither a usable price nor a
//     positive AvgCost — and any non-positive quantity — is EXCLUDED from the
//     sums and recorded in skipped (fail-safe: a corrupt or unpriceable leg
//     must never zero or inflate a blocking gate, #1270).
//
// Strategy IDs are iterated in sorted order so float summation and the
// per-asset Strategies slices are deterministic. Pure read; safe under
// mu.RLock. prices may be nil (AvgCost-only valuation).
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
		extraDeltas := make(map[string]float64)

		switch sc.Type {
		case "spot", "perps", "manual":
			// Deterministic position iteration (map order is randomized).
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
				if posAsset != asset && !pos.IsHedge {
					continue
				}
				if pos.Quantity <= 0 {
					skipped = append(skipped, fmt.Sprintf("%s/%s: non-positive quantity", id, pos.Symbol))
					continue
				}
				px := spotPrice
				if posAsset != asset {
					px = findSpotPrice(posAsset, prices)
				}
				if px <= 0 {
					px = pos.AvgCost
				}
				if px <= 0 {
					skipped = append(skipped, fmt.Sprintf("%s/%s: no usable price", id, pos.Symbol))
					continue
				}
				legUSD := pos.Quantity * positionMultiplier(pos) * px
				if posAsset != asset {
					if pos.Side == "short" {
						extraDeltas[posAsset] -= legUSD
					} else {
						extraDeltas[posAsset] += legUSD
					}
				} else if pos.Side == "short" {
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
					// Coarse estimate when greeks not yet marked.
					coarseDelta := 1.0
					if opt.OptionType == "put" {
						coarseDelta = -1.0
					}
					deltaUSD += sign * coarseDelta * opt.Quantity * spotPrice
				}
			}
		}

		addDelta := func(deltaAsset string, delta float64) {
			if delta == 0 {
				return
			}
			ae, exists := assets[deltaAsset]
			if !exists {
				ae = &AssetExposure{Asset: deltaAsset}
				assets[deltaAsset] = ae
			}
			ae.Strategies = append(ae.Strategies, StrategyExposure{StrategyID: id, DeltaUSD: delta, Type: sc.Type})
			ae.NetDeltaUSD += delta
			ae.GrossDeltaUSD += math.Abs(delta)
		}
		addDelta(asset, deltaUSD)
		extraAssets := make([]string, 0, len(extraDeltas))
		for extraAsset := range extraDeltas {
			extraAssets = append(extraAssets, extraAsset)
		}
		sort.Strings(extraAssets)
		for _, extraAsset := range extraAssets {
			addDelta(extraAsset, extraDeltas[extraAsset])
		}
	}

	sort.Strings(skipped)
	return assets, skipped
}

// ComputeCorrelation computes per-asset directional exposure across all strategies.
func ComputeCorrelation(strategies map[string]*StrategyState, cfgStrategies []StrategyConfig, prices map[string]float64, corrCfg *CorrelationConfig) *CorrelationSnapshot {
	snap := &CorrelationSnapshot{
		Timestamp: time.Now().UTC(),
	}
	snap.Assets, _ = computeAssetDeltas(strategies, cfgStrategies, prices)

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

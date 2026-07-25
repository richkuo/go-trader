package main

import (
	"fmt"
	"strconv"
	"strings"
)

// hedgeMaxRatio bounds the hedge notional multiplier (#1159 phase 1). A ratio
// above this is treated as an operator typo — a correlated hedge oversized by
// an order of magnitude would dominate the primary thesis it protects.
const hedgeMaxRatio = 10.0

// hedgeMaxLeverage mirrors the primary perps leverage validation bound
// (config.go "leverage must be in [1, 100]"); the hedge leg's own exchange
// leverage stays inside the same sanity envelope.
const hedgeMaxLeverage = 100.0

// HedgeConfig declares a per-strategy correlated hedge leg (#1159, phase 1).
// The hedge is an inverse-side HL perps leg on a second coin, auto-managed by
// the hedge-sync reconciler: it is opened/sized/closed off the PRIMARY
// position's quantity events and carries no SL/TP, no close evaluator, and no
// check script of its own. All fields are optional once the block exists;
// omitted fields resolve via the accessors below (read those, never the raw
// fields).
type HedgeConfig struct {
	// Enabled gates the whole feature for the strategy. A present-but-disabled
	// block is still shape-validated (side/ratio/leverage/margin typos reject)
	// but skips symbol requirement and collision checks — it changes nothing
	// live.
	Enabled bool `json:"enabled"`
	// Symbol is the hedge coin: bare ticker "BTC" or ccxt form
	// "BTC/USDC:USDC" (everything from the first "/" is stripped), normalized
	// to the upper/trimmed coin ticker exactly like hyperliquidConfiguredCoin.
	Symbol string `json:"symbol"`
	// Side is the hedge orientation. Only "inverse" (opposite the live primary
	// side) exists in phase 1; empty defaults to it. The vocabulary is
	// extensible later — unknown values reject loudly rather than silently
	// defaulting.
	Side string `json:"side,omitempty"`
	// Ratio is the hedge-to-primary NOTIONAL multiplier: hedge notional delta
	// = primary qty delta x primary price x ratio. 0 defaults to 1.0 (full
	// notional hedge); bounds (0, hedgeMaxRatio].
	Ratio float64 `json:"ratio,omitempty"`
	// Platform must be "" or "hyperliquid" (phase 1).
	Platform string `json:"platform,omitempty"`
	// Type must be "" or "perps" (phase 1).
	Type string `json:"type,omitempty"`
	// MarginMode is the hedge leg's OWN margin mode ("isolated" default, or
	// "cross") — explicit, never inherited from the primary leg. Enforced on
	// fresh hedge opens only (HL rejects update_leverage on an open position).
	MarginMode string `json:"margin_mode,omitempty"`
	// Leverage is the hedge leg's OWN exchange leverage; 0 defaults to 1.
	// Independent of the primary's leverage because collision validation
	// guarantees the hedge coin has exactly one owner on the wallet.
	Leverage float64 `json:"leverage,omitempty"`
}

// HedgeEnabled reports whether the strategy opts into a correlated hedge leg
// (#1159). Read via this accessor, never the raw pointer/bool.
func HedgeEnabled(sc StrategyConfig) bool {
	return sc.Hedge != nil && sc.Hedge.Enabled
}

// hedgeCoin returns the normalized hedge coin ticker ("" when no hedge block
// or blank symbol): strips the ccxt quote suffix, upper-cases and trims —
// the same normalization hyperliquidConfiguredCoin applies, so collision
// detection survives operator typos like `symbol: "btc"` or the ccxt form
// "BTC/USDC:USDC".
func hedgeCoin(sc StrategyConfig) string {
	if sc.Hedge == nil {
		return ""
	}
	raw := sc.Hedge.Symbol
	if i := strings.Index(raw, "/"); i >= 0 {
		raw = raw[:i]
	}
	return strings.ToUpper(strings.TrimSpace(raw))
}

// HedgeRatio resolves the hedge notional multiplier; 0 defaults to 1.0 (full
// notional hedge). Read via this accessor, never the raw field.
func HedgeRatio(sc StrategyConfig) float64 {
	if sc.Hedge == nil || sc.Hedge.Ratio == 0 {
		return 1.0
	}
	return sc.Hedge.Ratio
}

// hedgeLeverage resolves the hedge leg's own exchange leverage; 0 defaults to
// 1 (no leverage).
func hedgeLeverage(sc StrategyConfig) float64 {
	if sc.Hedge == nil || sc.Hedge.Leverage == 0 {
		return 1.0
	}
	return sc.Hedge.Leverage
}

// hedgeMarginMode resolves the hedge leg's own margin mode; empty defaults to
// "isolated" (mirrors the primary MarginMode convention).
func hedgeMarginMode(sc StrategyConfig) string {
	if sc.Hedge == nil || sc.Hedge.MarginMode == "" {
		return "isolated"
	}
	return sc.Hedge.MarginMode
}

// hedgeSide resolves the hedge orientation; empty defaults to "inverse" (the
// only phase-1 value — validation rejects anything else).
func hedgeSide(sc StrategyConfig) string {
	if sc.Hedge == nil || sc.Hedge.Side == "" {
		return "inverse"
	}
	return sc.Hedge.Side
}

// formatHedgeRatio renders a hedge ratio/leverage for operator surfaces with
// at least one decimal (1 → "1.0", 0.75 → "0.75") so the summary line reads
// hedge=BTC×1.0(inverse,cross,3x).
func formatHedgeRatio(v float64) string {
	s := strconv.FormatFloat(v, 'f', -1, 64)
	if !strings.ContainsAny(s, ".e") {
		s += ".0"
	}
	return s
}

// validateHedgeConfigs validates every strategy's hedge block (#1159 phase
// 1) and the cross-strategy collision matrix. Called from validateConfig
// alongside hyperliquidPeerStrategyErrors.
//
// Shape checks run whenever a hedge block is present (even disabled — a typo
// must reject, not silently default). The symbol requirement, scope
// (HL-perps-only), direction restriction, and collision matrix apply only to
// ENABLED blocks, since a disabled block changes nothing live.
//
// The collision matrix is the phase-1 sole-ownership invariant: a hedge coin
// must never be the strategy's own configured coin, never any configured HL
// strategy coin (perps AND manual, live and paper — paper peers still share
// the wallet), and never shared between two hedge-enabled strategies. With
// sole ownership guaranteed at load, every shared-coin mechanism keyed on
// hyperliquidConfiguredCoin can treat hedge coins as single-owner.
func validateHedgeConfigs(cfg *Config) []string {
	if cfg == nil {
		return nil
	}
	var errs []string
	for i := range cfg.Strategies {
		sc := &cfg.Strategies[i]
		if sc.Hedge == nil {
			continue
		}
		prefix := fmt.Sprintf("strategy[%s]", sc.ID)
		h := sc.Hedge

		// Shape vocabulary — validated even for disabled blocks.
		if h.Side != "" && h.Side != "inverse" {
			errs = append(errs, fmt.Sprintf("%s: hedge.side must be \"inverse\" (only value in phase 1) or empty, got %q", prefix, h.Side))
		}
		if h.Ratio < 0 || h.Ratio > hedgeMaxRatio {
			errs = append(errs, fmt.Sprintf("%s: hedge.ratio must be in (0, %g] (0 = default 1.0), got %g", prefix, hedgeMaxRatio, h.Ratio))
		}
		if h.Leverage < 0 || h.Leverage > hedgeMaxLeverage {
			errs = append(errs, fmt.Sprintf("%s: hedge.leverage must be in [0, %g] (0 = default 1), got %g", prefix, hedgeMaxLeverage, h.Leverage))
		}
		if h.MarginMode != "" && h.MarginMode != "isolated" && h.MarginMode != "cross" {
			errs = append(errs, fmt.Sprintf("%s: hedge.margin_mode must be \"isolated\" or \"cross\", got %q", prefix, h.MarginMode))
		}
		if h.Platform != "" && h.Platform != "hyperliquid" {
			errs = append(errs, fmt.Sprintf("%s: hedge.platform must be \"hyperliquid\" or empty (phase 1), got %q", prefix, h.Platform))
		}
		if h.Type != "" && h.Type != "perps" {
			errs = append(errs, fmt.Sprintf("%s: hedge.type must be \"perps\" or empty (phase 1), got %q", prefix, h.Type))
		}

		if !HedgeEnabled(*sc) {
			continue
		}

		// Enabled-only checks.
		if sc.Type != "perps" || sc.Platform != "hyperliquid" {
			errs = append(errs, fmt.Sprintf("%s: hedge is only supported on platform=hyperliquid type=perps strategies (phase 1), got platform=%q type=%q", prefix, sc.Platform, sc.Type))
		}
		coin := hedgeCoin(*sc)
		if coin == "" {
			errs = append(errs, fmt.Sprintf("%s: hedge.symbol is required (and must normalize to a coin ticker) when hedge is enabled", prefix))
		} else if coin == hyperliquidConfiguredCoin(*sc) {
			errs = append(errs, fmt.Sprintf("%s: hedge.symbol %s equals the strategy's own coin — a same-coin \"hedge\" just nets on-chain, not a correlated hedge", prefix, coin))
		}
		if EffectiveDirection(*sc) == DirectionBoth {
			errs = append(errs, fmt.Sprintf("%s: hedge + direction=%q is rejected in phase 1 — a bidirectional flip changes the hedge SIDE mid-flight; deterministic flip mirroring is a phase-2 problem", prefix, DirectionBoth))
		}
	}

	// Cross-strategy collision matrix (enabled hedges only). A hedge coin must
	// collide with NOTHING: not any configured HL strategy coin (perps and
	// manual, live and paper — paper peers still share the wallet coin) and
	// not another hedge-enabled strategy's hedge coin.
	hedgeCoinSeen := make(map[string]string) // normalized hedge coin -> first strategy ID
	for i := range cfg.Strategies {
		sc := &cfg.Strategies[i]
		if !HedgeEnabled(*sc) {
			continue
		}
		coin := hedgeCoin(*sc)
		if coin == "" {
			continue // already reported above
		}
		for j := range cfg.Strategies {
			other := &cfg.Strategies[j]
			if other.ID == sc.ID {
				continue
			}
			if otherCoin := hyperliquidConfiguredCoin(*other); otherCoin != "" && otherCoin == coin {
				errs = append(errs, fmt.Sprintf("strategy[%s]: hedge.symbol %s collides with strategy[%s]'s configured coin — hedge coins must be sole-owned (no configured strategy may trade the hedge coin)", sc.ID, coin, other.ID))
			}
		}
		if first, dup := hedgeCoinSeen[coin]; dup {
			errs = append(errs, fmt.Sprintf("strategy[%s]: hedge.symbol %s is also strategy[%s]'s hedge coin — two hedge-enabled strategies may not share a hedge coin (sole-ownership invariant)", sc.ID, coin, first))
		} else {
			hedgeCoinSeen[coin] = sc.ID
		}
	}
	return errs
}

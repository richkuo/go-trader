package main

import (
	"fmt"
	"sort"
	"strings"
)

// Phase-1 hedge constants (#1159).
const (
	// HedgeSideInverse is the only side policy accepted in phase 1: the hedge
	// always takes the opposite side of the LIVE primary position.
	HedgeSideInverse = "inverse"
	// DefaultHedgeRatio is the hedge notional multiple applied when the block
	// omits `ratio`.
	DefaultHedgeRatio = 1.0
	// MaxHedgeRatio bounds `ratio` — a hedge ten times the primary notional is
	// a typo, not a hedge.
	MaxHedgeRatio = 10.0
	// DefaultHedgeLeverage is the hedge leg's exchange leverage when omitted.
	DefaultHedgeLeverage = 1.0
	// MaxHedgeLeverage bounds `leverage` at Hyperliquid's practical ceiling.
	MaxHedgeLeverage = 50.0
)

// HedgeEnabled reports whether sc carries an active hedge block. Every hedge
// code path gates on this accessor — never on sc.Hedge != nil, which is true
// for an explicitly disabled block too.
func HedgeEnabled(sc StrategyConfig) bool {
	return sc.Hedge != nil && sc.Hedge.Enabled
}

// normalizeHedgeCoin turns a configured hedge symbol into the HL coin ticker
// used as the Position map key and the on-chain coin: "BTC/USDC:USDC" → "BTC",
// " eth " → "ETH". Mirrors hyperliquidConfiguredCoin's upper/trim contract so
// collision detection compares like with like.
func normalizeHedgeCoin(symbol string) string {
	s := strings.TrimSpace(symbol)
	if s == "" {
		return ""
	}
	// ccxt perp symbols are BASE/QUOTE:SETTLE — the base is the HL coin.
	if i := strings.IndexAny(s, "/:"); i >= 0 {
		s = s[:i]
	}
	return strings.ToUpper(strings.TrimSpace(s))
}

// hedgeCoin returns the normalized hedge coin ticker for sc, or "" when the
// strategy has no enabled hedge. This is the single source of truth for "which
// coin does this strategy's hedge live on".
func hedgeCoin(sc StrategyConfig) string {
	if !HedgeEnabled(sc) {
		return ""
	}
	return normalizeHedgeCoin(sc.Hedge.Symbol)
}

// HedgeRatio returns the effective hedge notional multiplier (default 1.0).
func HedgeRatio(sc StrategyConfig) float64 {
	if !HedgeEnabled(sc) || sc.Hedge.Ratio <= 0 {
		return DefaultHedgeRatio
	}
	return sc.Hedge.Ratio
}

// hedgeLeverage returns the hedge leg's own exchange leverage (default 1x).
func hedgeLeverage(sc StrategyConfig) float64 {
	if !HedgeEnabled(sc) || sc.Hedge.Leverage <= 0 {
		return DefaultHedgeLeverage
	}
	return sc.Hedge.Leverage
}

// hedgeMarginMode returns the hedge leg's own margin mode (default isolated).
func hedgeMarginMode(sc StrategyConfig) string {
	if !HedgeEnabled(sc) || strings.TrimSpace(sc.Hedge.MarginMode) == "" {
		return "isolated"
	}
	return strings.ToLower(strings.TrimSpace(sc.Hedge.MarginMode))
}

// hedgeSide returns the effective side policy (default "inverse").
func hedgeSide(sc StrategyConfig) string {
	if !HedgeEnabled(sc) || strings.TrimSpace(sc.Hedge.Side) == "" {
		return HedgeSideInverse
	}
	return strings.ToLower(strings.TrimSpace(sc.Hedge.Side))
}

// inverseSide maps a position side to its opposite. "" for an unknown input so
// callers fail closed rather than guessing a direction to trade.
func inverseSide(side string) string {
	switch side {
	case "long":
		return "short"
	case "short":
		return "long"
	}
	return ""
}

// hedgeOrderSide maps a hedge position side to the order side used for opens
// and adds ("long" → buy, "short" → sell).
func hedgeOrderSide(side string) string {
	if side == "short" {
		return "sell"
	}
	return "buy"
}

// validateHedgeConfigs enforces the full phase-1 hedge constraint matrix
// (#1159 constraints 1/2/3/5 and acceptance criterion 5). Returns one message
// per violation, sorted by the caller alongside the other validateConfig
// errors.
//
// The collision matrix is the load-bearing check: every shared-coin mechanism
// in the scheduler (peer detection, margin compatibility, circuit-breaker
// drain, kill-switch fill share, reconcile owner mapping) derives coin
// membership from hyperliquidConfiguredCoin and is therefore blind to hedge
// coins. Rejecting overlap at load is what makes that blindness safe.
func validateHedgeConfigs(cfg *Config) []string {
	var errs []string
	if cfg == nil {
		return nil
	}

	// Coin universe of every configured strategy (perps AND manual, live AND
	// paper). Paper peers still collide with a live hedge: they share the
	// wallet's on-chain coin slot the moment the operator flips one to live,
	// and a paper strategy's virtual position on the hedge coin would be
	// silently reconciled against the hedge's on-chain size. Err strict.
	configuredCoins := make(map[string][]string)
	for _, sc := range cfg.Strategies {
		if coin := hyperliquidConfiguredCoin(sc); coin != "" {
			configuredCoins[coin] = append(configuredCoins[coin], sc.ID)
		}
	}

	hedgeCoinOwners := make(map[string][]string)

	for _, sc := range cfg.Strategies {
		if sc.Hedge == nil {
			continue
		}
		prefix := fmt.Sprintf("strategy[%s]", sc.ID)
		h := sc.Hedge

		// A disabled block is inert everywhere else in the codebase, but its
		// SHAPE is still validated so an operator who flips `enabled: true`
		// doesn't discover the typo at first live trade.
		if sc.Type != "perps" {
			errs = append(errs, fmt.Sprintf("%s: hedge is only supported for perps strategies (got type %q) (#1159)", prefix, sc.Type))
		}
		if sc.Platform != "hyperliquid" {
			errs = append(errs, fmt.Sprintf("%s: hedge is only supported on hyperliquid (got platform %q) (#1159)", prefix, sc.Platform))
		}
		if p := strings.TrimSpace(h.Platform); p != "" && p != "hyperliquid" {
			errs = append(errs, fmt.Sprintf("%s: hedge.platform must be \"hyperliquid\" or omitted, got %q", prefix, h.Platform))
		}
		if t := strings.TrimSpace(h.Type); t != "" && t != "perps" {
			errs = append(errs, fmt.Sprintf("%s: hedge.type must be \"perps\" or omitted, got %q", prefix, h.Type))
		}
		if s := strings.ToLower(strings.TrimSpace(h.Side)); s != "" && s != HedgeSideInverse {
			errs = append(errs, fmt.Sprintf("%s: hedge.side must be %q or omitted in phase 1, got %q (#1159)", prefix, HedgeSideInverse, h.Side))
		}
		if h.Ratio < 0 || h.Ratio > MaxHedgeRatio {
			errs = append(errs, fmt.Sprintf("%s: hedge.ratio must be in (0, %g] (0 = default %g), got %g", prefix, MaxHedgeRatio, DefaultHedgeRatio, h.Ratio))
		}
		if h.Leverage < 0 || h.Leverage > MaxHedgeLeverage {
			errs = append(errs, fmt.Sprintf("%s: hedge.leverage must be in (0, %g] (0 = default %gx), got %g", prefix, MaxHedgeLeverage, DefaultHedgeLeverage, h.Leverage))
		}
		if m := strings.ToLower(strings.TrimSpace(h.MarginMode)); m != "" && m != "isolated" && m != "cross" {
			errs = append(errs, fmt.Sprintf("%s: hedge.margin_mode must be \"isolated\" or \"cross\", got %q", prefix, h.MarginMode))
		}

		coin := normalizeHedgeCoin(h.Symbol)
		if coin == "" {
			errs = append(errs, fmt.Sprintf("%s: hedge.symbol is required and must name a coin (e.g. \"BTC\" or \"BTC/USDC:USDC\"), got %q", prefix, h.Symbol))
		}

		// #1159 constraint: a flip is the one primary event that changes hedge
		// SIDE mid-flight, and perpsLiveOrderSize degrades a catastrophic flip
		// to close-only — deterministic hedge mirroring of flips is phase 2.
		if EffectiveDirection(sc) == DirectionBoth {
			errs = append(errs, fmt.Sprintf("%s: hedge is not supported with direction %q in phase 1 — a mid-position flip would change the hedge side (use \"long\" or \"short\") (#1159)", prefix, DirectionBoth))
		}

		if coin == "" {
			continue
		}

		if own := hyperliquidConfiguredCoin(sc); own != "" && own == coin {
			errs = append(errs, fmt.Sprintf("%s: hedge.symbol %q is the strategy's own coin — a same-coin hedge just nets the position on-chain (#1159)", prefix, coin))
		}
		if owners := configuredCoins[coin]; len(owners) > 0 {
			foreign := make([]string, 0, len(owners))
			for _, id := range owners {
				if id != sc.ID {
					foreign = append(foreign, id)
				}
			}
			if len(foreign) > 0 {
				sort.Strings(foreign)
				errs = append(errs, fmt.Sprintf("%s: hedge.symbol %q is the configured coin of strategy/strategies %v — hedge coins must be exclusive (HL nets one on-chain position per coin per wallet) (#1159)", prefix, coin, foreign))
			}
		}
		hedgeCoinOwners[coin] = append(hedgeCoinOwners[coin], sc.ID)
	}

	coins := make([]string, 0, len(hedgeCoinOwners))
	for coin := range hedgeCoinOwners {
		coins = append(coins, coin)
	}
	sort.Strings(coins)
	for _, coin := range coins {
		owners := hedgeCoinOwners[coin]
		if len(owners) > 1 {
			sort.Strings(owners)
			errs = append(errs, fmt.Sprintf("hedge coin %q is claimed by multiple strategies %v — two hedge legs on one coin share an on-chain position, margin assignment, and reduce-only order slots (#1159)", coin, owners))
		}
	}

	return errs
}

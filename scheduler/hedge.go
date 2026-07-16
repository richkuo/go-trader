package main

import (
	"fmt"
	"math"
	"sort"
	"strings"
)

// hedgeSnapshot is captured with the primary and hedge positions under the
// scheduler's phase-1 read lock. It deliberately carries a quantity watermark:
// mark drift must not cause an order; only a primary quantity event may.
type hedgeSnapshot struct {
	PrimaryQty, PrimaryAvgCost float64
	PrimarySide                string
	HedgeQty, HedgeAvgCost     float64
	HedgeBasis                 float64
	HedgeSide                  string
}

type hedgeActionKind string

const (
	hedgeActionNone      hedgeActionKind = "none"
	hedgeActionOpen      hedgeActionKind = "open"
	hedgeActionAdd       hedgeActionKind = "add"
	hedgeActionReduce    hedgeActionKind = "reduce"
	hedgeActionCloseFull hedgeActionKind = "close_full"
)

type hedgeAction struct {
	Kind   hedgeActionKind
	Qty    float64
	Side   string
	Reason string
}

const hedgeQuantityEpsilon = 1e-9

func inverseHedgeSide(side string) string {
	if strings.EqualFold(strings.TrimSpace(side), "short") {
		return "long"
	}
	return "short"
}

// hedgeTargetDecision is the side-effect-free hedge reconciler. Callers must
// treat a non-empty Reason as fail-closed: an unusable price must never turn
// into an unhedged primary open by silently substituting another oracle.
func hedgeTargetDecision(sc StrategyConfig, snap hedgeSnapshot, primaryPx, hedgePx float64) hedgeAction {
	if !sc.HedgeEnabled() {
		return hedgeAction{Kind: hedgeActionNone}
	}
	if snap.PrimaryQty <= hedgeQuantityEpsilon {
		if snap.HedgeQty > hedgeQuantityEpsilon {
			return hedgeAction{Kind: hedgeActionCloseFull, Qty: snap.HedgeQty, Side: snap.HedgeSide, Reason: "primary is flat"}
		}
		return hedgeAction{Kind: hedgeActionNone}
	}
	if primaryPx <= 0 || hedgePx <= 0 {
		return hedgeAction{Kind: hedgeActionNone, Reason: "primary or hedge mark is unavailable"}
	}
	wantSide := inverseHedgeSide(snap.PrimarySide)
	if snap.HedgeQty <= hedgeQuantityEpsilon {
		return hedgeAction{Kind: hedgeActionOpen, Qty: snap.PrimaryQty * primaryPx * sc.HedgeRatio() / hedgePx, Side: wantSide}
	}
	if !strings.EqualFold(snap.HedgeSide, wantSide) {
		return hedgeAction{Kind: hedgeActionCloseFull, Qty: snap.HedgeQty, Side: snap.HedgeSide, Reason: "hedge side disagrees with primary"}
	}
	if snap.HedgeBasis <= hedgeQuantityEpsilon {
		// A pre-watermark persisted hedge must never be auto-up-sized. Closing
		// it is safer than guessing whether it represents all of the primary.
		return hedgeAction{Kind: hedgeActionCloseFull, Qty: snap.HedgeQty, Side: snap.HedgeSide, Reason: "hedge quantity watermark is missing"}
	}
	if snap.PrimaryQty > snap.HedgeBasis+hedgeQuantityEpsilon {
		delta := snap.PrimaryQty - snap.HedgeBasis
		return hedgeAction{Kind: hedgeActionAdd, Qty: delta * primaryPx * sc.HedgeRatio() / hedgePx, Side: wantSide}
	}
	if snap.PrimaryQty < snap.HedgeBasis-hedgeQuantityEpsilon {
		qty := snap.HedgeQty * (snap.HedgeBasis - snap.PrimaryQty) / snap.HedgeBasis
		return hedgeAction{Kind: hedgeActionReduce, Qty: math.Min(qty, snap.HedgeQty), Side: snap.HedgeSide}
	}
	return hedgeAction{Kind: hedgeActionNone}
}

// validateHedgeConfigs enforces sole ownership before any order path sees a
// hedge. It intentionally checks every configured HL coin (including manual
// and paper strategies): an overlap is ambiguous after a mode change/restart.
func validateHedgeConfigs(strategies []StrategyConfig) []string {
	configured := make(map[string][]string)
	for _, sc := range strategies {
		if (sc.Type == "perps" || sc.Type == "manual") && sc.Platform == "hyperliquid" {
			if coin := hyperliquidConfiguredCoin(sc); coin != "" {
				configured[coin] = append(configured[coin], sc.ID)
			}
		}
	}
	var errs []string
	seenHedges := make(map[string]string)
	for _, sc := range strategies {
		if !sc.HedgeEnabled() {
			continue
		}
		prefix := fmt.Sprintf("strategy[%s].hedge", sc.ID)
		if sc.Type != "perps" || sc.Platform != "hyperliquid" {
			errs = append(errs, fmt.Sprintf("%s is only supported for hyperliquid perps strategies", prefix))
		}
		if sc.Hedge.Platform != "" && sc.Hedge.Platform != "hyperliquid" {
			errs = append(errs, fmt.Sprintf("%s.platform must be hyperliquid, got %q", prefix, sc.Hedge.Platform))
		}
		if sc.Hedge.Type != "" && sc.Hedge.Type != "perps" {
			errs = append(errs, fmt.Sprintf("%s.type must be perps, got %q", prefix, sc.Hedge.Type))
		}
		if sc.Hedge.Side != "" && sc.Hedge.Side != HedgeSideInverse {
			errs = append(errs, fmt.Sprintf("%s.side must be %q, got %q", prefix, HedgeSideInverse, sc.Hedge.Side))
		}
		if sc.Hedge.Ratio < 0 || sc.Hedge.Ratio > 10 {
			errs = append(errs, fmt.Sprintf("%s.ratio must be in (0, 10], got %g", prefix, sc.Hedge.Ratio))
		}
		if sc.Hedge.Leverage < 0 || sc.Hedge.Leverage > 100 {
			errs = append(errs, fmt.Sprintf("%s.leverage must be in [1, 100], got %g", prefix, sc.Hedge.Leverage))
		}
		if mode := sc.HedgeMarginMode(); mode != "isolated" && mode != "cross" {
			errs = append(errs, fmt.Sprintf("%s.margin_mode must be isolated or cross, got %q", prefix, mode))
		}
		coin := hedgeCoin(sc)
		if coin == "" {
			errs = append(errs, fmt.Sprintf("%s.symbol is required", prefix))
			continue
		}
		if EffectiveDirection(sc) == DirectionBoth {
			errs = append(errs, fmt.Sprintf("%s is not supported with direction=%q", prefix, DirectionBoth))
		}
		if ids := configured[coin]; len(ids) > 0 {
			ids = append([]string(nil), ids...)
			sort.Strings(ids)
			errs = append(errs, fmt.Sprintf("%s.symbol %q collides with configured Hyperliquid coin owned by strategies %s", prefix, coin, strings.Join(ids, ", ")))
		}
		if prior, exists := seenHedges[coin]; exists {
			errs = append(errs, fmt.Sprintf("%s.symbol %q collides with hedge on strategy %s", prefix, coin, prior))
		} else {
			seenHedges[coin] = sc.ID
		}
	}
	sort.Strings(errs)
	return errs
}

// validateHedgeStateConsistency catches the restart-only configuration drift
// that the SIGHUP flat-only guard cannot observe. It is intentionally
// non-destructive: a live leg with lost config must be surfaced to an operator,
// never guessed at or silently closed from startup code.
func validateHedgeStateConsistency(state *AppState, cfg *Config) []string {
	if state == nil || cfg == nil {
		return nil
	}
	byID := strategyConfigByID(cfg.Strategies)
	var warnings []string
	ids := make([]string, 0, len(state.Strategies))
	for id := range state.Strategies {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		ss := state.Strategies[id]
		sc, configured := byID[id]
		if ss == nil {
			continue
		}
		syms := make([]string, 0, len(ss.Positions))
		for sym := range ss.Positions {
			syms = append(syms, sym)
		}
		sort.Strings(syms)
		for _, sym := range syms {
			pos := ss.Positions[sym]
			if pos == nil || pos.HedgeFor == "" || pos.Quantity <= 0 {
				continue
			}
			if !configured || !sc.HedgeEnabled() {
				warnings = append(warnings, fmt.Sprintf("hedge state drift: strategy %s holds hedge %s for %s but its enabled hedge config is missing; leaving position untouched", id, sym, pos.HedgeFor))
				continue
			}
			if coin := hedgeCoin(sc); coin != sym {
				warnings = append(warnings, fmt.Sprintf("hedge state drift: strategy %s holds hedge %s for %s but config declares hedge %s; leaving position untouched", id, sym, pos.HedgeFor, coin))
			}
		}
	}
	return warnings
}

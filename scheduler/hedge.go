package main

// #1159 phase 1: per-strategy correlated hedge legs, live Hyperliquid perps
// only. A hedge-enabled strategy carries exactly one extra Position on a
// DIFFERENT coin (Position.IsHedge=true, keyed by the hedge coin) whose
// lifecycle is strictly coupled to the primary position: it opens with the
// primary open, scales with scale-in, reduces with partial close, and closes
// with full close / force-close / kill-switch / circuit-breaker. The hedge has
// no check script, no close evaluator, and no on-chain SL/TP of its own — the
// per-cycle convergence engine in hedge_sync.go is its only manager.
//
// Sizing model: the OPEN and each ADD are sized in USD-notional terms
// (primary_qty_delta × primary_mark × ratio ÷ hedge_mark), but the ongoing
// target is tracked in QUANTITY terms via Position.HedgeCoveredPrimaryQty (the
// primary quantity the current hedge leg covers). Reductions are proportional
// to the covered quantity, never re-priced from live marks — re-deriving a
// notional target every cycle would churn orders as prices move.

import (
	"fmt"
	"math"
	"sort"
	"strings"
	"time"
)

const (
	// hedgeQtyEpsilon is the absolute quantity below which a position/order is
	// treated as flat/empty.
	hedgeQtyEpsilon = 1e-9
	// hedgeCoveredRelEpsilon is the relative tolerance between the primary
	// quantity and the hedge's covered quantity before the convergence engine
	// issues an order. Both values come from virtual state (not marks), so in
	// practice they match exactly; the tolerance only absorbs float drift.
	hedgeCoveredRelEpsilon = 1e-6
	// hedgeFillShortfallTolerance is the relative shortfall between a hedge
	// order's requested and filled size before the engine treats it as a
	// genuine partial fill. It must absorb exchange lot-size rounding: the HL
	// adapter rounds order sizes to the asset's sz_decimals (round-to-nearest,
	// platforms/hyperliquid/adapter.py market_open), so a FULLY-filled order
	// legitimately reports up to half a lot less than the planner's unrounded
	// quantity — a tight epsilon here would misread every such fill as
	// partial and churn dust orders (review on #1333, round 3).
	hedgeFillShortfallTolerance = 0.01
)

func hedgeEnabled(sc StrategyConfig) bool { return sc.Hedge != nil && sc.Hedge.Enabled }

// hedgeCoin returns the normalized hedge coin ticker for a hedge-enabled
// strategy, "" otherwise.
func hedgeCoin(sc StrategyConfig) string {
	if !hedgeEnabled(sc) {
		return ""
	}
	return hyperliquidCoinFromSymbol(sc.Hedge.Symbol)
}

// hyperliquidCoinFromSymbol normalizes a symbol like "BTC/USDC:USDC" or
// "btc" to the HL coin ticker ("BTC").
func hyperliquidCoinFromSymbol(symbol string) string {
	s := strings.TrimSpace(symbol)
	if i := strings.IndexByte(s, '/'); i >= 0 {
		s = s[:i]
	}
	return strings.ToUpper(strings.TrimSpace(s))
}

// hedgeConfigEqual reports whether two hedge blocks are identical for
// hot-reload purposes. Mirrors scaleInConfigEqual: nil vs zero-value blocks
// are distinct by pointer presence.
func hedgeConfigEqual(a, b *HedgeConfig) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

// strategyHasOpenHedgeLeg reports whether the strategy state holds a live
// hedge leg (any IsHedge position with positive quantity).
func strategyHasOpenHedgeLeg(s *StrategyState) bool {
	if s == nil {
		return false
	}
	for _, pos := range s.Positions {
		if pos != nil && pos.IsHedge && pos.Quantity > hedgeQtyEpsilon {
			return true
		}
	}
	return false
}

// findHedgePosition returns the strategy's open hedge leg. The configured
// hedge coin is preferred; a state-only hedge under a different coin (config
// edited between restarts) still matches so kill-switch/CB/status paths never
// lose sight of a live leg.
func findHedgePosition(s *StrategyState, sc StrategyConfig) *Position {
	if s == nil {
		return nil
	}
	coin := hedgeCoin(sc)
	var fallback *Position
	for sym, pos := range s.Positions {
		if pos == nil || !pos.IsHedge || pos.Quantity <= hedgeQtyEpsilon {
			continue
		}
		if coin != "" && strings.EqualFold(sym, coin) {
			return pos
		}
		if fallback == nil || sym < fallback.Symbol {
			fallback = pos
		}
	}
	return fallback
}

// validateHedgeConfigs enforces the #1159 phase-1 constraints up front so no
// live order is ever placed against an ambiguous-ownership hedge coin:
//
//   - live Hyperliquid perps only, on both the primary and the hedge leg;
//   - side must be "inverse" (the only phase-1 policy);
//   - ratio positive and finite; margin fields validated like the primary's;
//   - the hedge coin must not equal the strategy's own primary coin (a
//     same-coin "hedge" just nets the position on-chain), any configured
//     strategy's coin (HL aggregates per coin per wallet — the hedge would
//     share an on-chain position, margin assignment, and reduce-only slots
//     with that strategy), or another hedge-enabled strategy's hedge coin.
func validateHedgeConfigs(strategies []StrategyConfig) []string {
	var errs []string
	primaryOwners := make(map[string]string)
	for _, sc := range strategies {
		if coin := hyperliquidConfiguredCoin(sc); coin != "" {
			if _, ok := primaryOwners[coin]; !ok {
				primaryOwners[coin] = sc.ID
			}
		}
	}
	hedgeOwners := make(map[string]string)
	for _, sc := range strategies {
		if !hedgeEnabled(sc) {
			continue
		}
		prefix := fmt.Sprintf("strategy[%s].hedge", sc.ID)
		h := sc.Hedge
		coin := hedgeCoin(sc)
		if sc.Platform != "hyperliquid" || sc.Type != "perps" || !hyperliquidIsLive(sc.Args) {
			errs = append(errs, prefix+": phase 1 requires a live Hyperliquid perps primary strategy (#1159)")
		}
		if h.Platform != "hyperliquid" || h.Type != "perps" {
			errs = append(errs, prefix+`: phase 1 requires platform="hyperliquid" and type="perps" on the hedge leg (#1159)`)
		}
		if coin == "" {
			errs = append(errs, prefix+".symbol is required")
		}
		if h.Side != HedgeSideInverse {
			errs = append(errs, fmt.Sprintf("%s.side must be %q (the only phase-1 policy), got %q", prefix, HedgeSideInverse, h.Side))
		}
		if h.Ratio <= 0 || math.IsNaN(h.Ratio) || math.IsInf(h.Ratio, 0) {
			errs = append(errs, fmt.Sprintf("%s.ratio must be > 0, got %g", prefix, h.Ratio))
		}
		if h.MarginMode != "isolated" && h.MarginMode != "cross" {
			errs = append(errs, fmt.Sprintf("%s.margin_mode must be \"isolated\" or \"cross\", got %q", prefix, h.MarginMode))
		}
		if h.Leverage < 1 || h.Leverage > 50 {
			errs = append(errs, fmt.Sprintf("%s.leverage must be in [1, 50], got %g", prefix, h.Leverage))
		}
		if coin == "" {
			continue
		}
		if coin == hyperliquidConfiguredCoin(sc) {
			errs = append(errs, fmt.Sprintf("%s.symbol %q matches its own primary coin — a same-coin hedge nets the on-chain position instead of hedging it", prefix, coin))
		} else if owner, ok := primaryOwners[coin]; ok {
			errs = append(errs, fmt.Sprintf("%s.symbol %q matches configured strategy %q's coin — hedge legs must not share an on-chain position with any strategy (phase 1, #1159)", prefix, coin, owner))
		}
		if owner, ok := hedgeOwners[coin]; ok && owner != sc.ID {
			errs = append(errs, fmt.Sprintf("%s.symbol %q is already the hedge coin of strategy %q — two hedge legs on one coin would share an on-chain position (phase 1, #1159)", prefix, coin, owner))
		} else if _, ok := hedgeOwners[coin]; !ok {
			hedgeOwners[coin] = sc.ID
		}
	}
	return errs
}

// validateHedgeStateConsistency flags persisted hedge legs whose strategy no
// longer manages them — the config hedge block was removed/disabled, or the
// hedge coin changed, across a restart (the SIGHUP guard can't see restarts).
// Warning-only, mirroring ValidatePerpsDirectionConfig: the leg stays visible
// and the kill switch still covers it (state-claimed coins), but nothing will
// converge it — the operator must flatten it or restore the hedge block.
func validateHedgeStateConsistency(state *AppState, cfg *Config) []string {
	var warnings []string
	byID := make(map[string]StrategyConfig, len(cfg.Strategies))
	for _, sc := range cfg.Strategies {
		byID[sc.ID] = sc
	}
	ids := make([]string, 0, len(state.Strategies))
	for id := range state.Strategies {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		s := state.Strategies[id]
		if s == nil {
			continue
		}
		sc, configured := byID[id]
		syms := make([]string, 0, len(s.Positions))
		for sym := range s.Positions {
			syms = append(syms, sym)
		}
		sort.Strings(syms)
		for _, sym := range syms {
			pos := s.Positions[sym]
			if pos == nil || !pos.IsHedge || pos.Quantity <= hedgeQtyEpsilon {
				continue
			}
			switch {
			case !configured:
				warnings = append(warnings, fmt.Sprintf("orphaned hedge leg: strategy %s no longer configured but holds hedge %s %s qty=%g (for %s) — nothing manages it; flatten manually or restore the strategy (#1159)",
					id, pos.Side, sym, pos.Quantity, pos.HedgePrimarySymbol))
			case !hedgeEnabled(sc):
				warnings = append(warnings, fmt.Sprintf("orphaned hedge leg: strategy %s holds hedge %s %s qty=%g (for %s) but its hedge block is disabled/removed — nothing converges it; flatten manually or restore the hedge block (#1159)",
					id, pos.Side, sym, pos.Quantity, pos.HedgePrimarySymbol))
			case !strings.EqualFold(hedgeCoin(sc), sym):
				warnings = append(warnings, fmt.Sprintf("hedge leg coin mismatch: strategy %s holds hedge %s %s qty=%g but config now hedges with %s — the old leg is converged as-is; flatten it to migrate coins (#1159)",
					id, pos.Side, sym, pos.Quantity, hedgeCoin(sc)))
			}
		}
	}
	return warnings
}

// inverseSide maps a primary position side to the phase-1 hedge side.
func inverseSide(side string) string {
	if side == "long" {
		return "short"
	}
	return "long"
}

// hedgePrimarySnapshot / hedgeLegSnapshot are lock-free copies of the fields
// the convergence planner needs, captured under the RLock in hedge_sync.go.
type hedgePrimarySnapshot struct {
	Symbol   string
	Side     string
	Quantity float64
}

type hedgeLegSnapshot struct {
	Symbol   string
	Side     string
	Quantity float64
	Covered  float64 // Position.HedgeCoveredPrimaryQty
}

// hedgeOrder is one on-chain action the convergence engine must take.
// Side != "" → market open/add of Quantity on the hedge coin.
// Close → reduce-only close (FullClose closes the whole leg).
// CoveredAfter is the HedgeCoveredPrimaryQty to stamp once the order books.
type hedgeOrder struct {
	Side         string
	Quantity     float64
	Close        bool
	FullClose    bool
	CoveredAfter float64
}

// hedgePlan is the convergence planner output. StampCovered (no orders) means
// the covered watermark should be adopted/re-synced without touching the
// exchange — e.g. a legacy hedge leg persisted before the watermark existed.
type hedgePlan struct {
	Orders       []hedgeOrder
	StampCovered *float64
}

// hedgeOpenQty sizes an opening/adding hedge order: the primary quantity
// delta's USD notional at the primary mark, scaled by ratio, converted to
// hedge-coin units at the hedge mark.
func hedgeOpenQty(ratio, primaryQtyDelta, primaryMark, hedgeMark float64) (float64, error) {
	if primaryMark <= 0 || hedgeMark <= 0 {
		return 0, fmt.Errorf("hedge sizing requires positive primary and hedge marks (primary=%g hedge=%g)", primaryMark, hedgeMark)
	}
	qty := primaryQtyDelta * primaryMark * ratio / hedgeMark
	if math.IsNaN(qty) || math.IsInf(qty, 0) || qty <= hedgeQtyEpsilon {
		return 0, fmt.Errorf("resolved hedge quantity %g is not positive", qty)
	}
	return qty, nil
}

// planHedgeConvergence derives the orders that bring the hedge leg in line
// with the primary position. Pure — no locks, no I/O — so every lifecycle
// case is unit-testable. Marks are only consulted when an opening/adding
// order must be sized; reductions and closes are mark-free.
func planHedgeConvergence(ratio float64, primary *hedgePrimarySnapshot, hedge *hedgeLegSnapshot, primaryMark, hedgeMark float64) (hedgePlan, error) {
	if hedge != nil && (hedge.Quantity <= hedgeQtyEpsilon || (hedge.Side != "long" && hedge.Side != "short")) {
		return hedgePlan{}, fmt.Errorf("corrupt hedge leg state (side=%q qty=%g)", hedge.Side, hedge.Quantity)
	}
	if primary == nil || primary.Quantity <= hedgeQtyEpsilon {
		if hedge == nil {
			return hedgePlan{}, nil
		}
		return hedgePlan{Orders: []hedgeOrder{{Close: true, FullClose: true, Quantity: hedge.Quantity}}}, nil
	}
	if primary.Side != "long" && primary.Side != "short" {
		return hedgePlan{}, fmt.Errorf("invalid primary side %q", primary.Side)
	}
	want := inverseSide(primary.Side)

	openOrder := func() (hedgeOrder, error) {
		qty, err := hedgeOpenQty(ratio, primary.Quantity, primaryMark, hedgeMark)
		if err != nil {
			return hedgeOrder{}, err
		}
		return hedgeOrder{Side: openTradeSide(want), Quantity: qty, CoveredAfter: primary.Quantity}, nil
	}

	if hedge == nil {
		open, err := openOrder()
		if err != nil {
			return hedgePlan{}, err
		}
		return hedgePlan{Orders: []hedgeOrder{open}}, nil
	}
	if hedge.Side != want {
		// Primary flipped: flatten the stale hedge, then open the inverse leg.
		open, err := openOrder()
		if err != nil {
			return hedgePlan{}, err
		}
		return hedgePlan{Orders: []hedgeOrder{
			{Close: true, FullClose: true, Quantity: hedge.Quantity},
			open,
		}}, nil
	}
	covered := hedge.Covered
	if covered <= hedgeQtyEpsilon {
		// Legacy leg persisted before the covered watermark existed (or a
		// zeroed stamp): adopt the current primary quantity — issuing orders
		// against an unknown baseline would guess.
		stamp := primary.Quantity
		return hedgePlan{StampCovered: &stamp}, nil
	}
	switch {
	case primary.Quantity < covered*(1-hedgeCoveredRelEpsilon):
		// Primary reduced: shed the same fraction of the hedge leg.
		frac := (covered - primary.Quantity) / covered
		reduce := hedge.Quantity * frac
		if reduce <= hedgeQtyEpsilon {
			stamp := primary.Quantity
			return hedgePlan{StampCovered: &stamp}, nil
		}
		if reduce >= hedge.Quantity*(1-hedgeCoveredRelEpsilon) {
			return hedgePlan{Orders: []hedgeOrder{{Close: true, FullClose: true, Quantity: hedge.Quantity}}}, nil
		}
		return hedgePlan{Orders: []hedgeOrder{{Close: true, Quantity: reduce, CoveredAfter: primary.Quantity}}}, nil
	case primary.Quantity > covered*(1+hedgeCoveredRelEpsilon):
		// Primary grew (scale-in): add the uncovered delta at current marks.
		qty, err := hedgeOpenQty(ratio, primary.Quantity-covered, primaryMark, hedgeMark)
		if err != nil {
			return hedgePlan{}, err
		}
		return hedgePlan{Orders: []hedgeOrder{{Side: openTradeSide(want), Quantity: qty, CoveredAfter: primary.Quantity}}}, nil
	case covered != primary.Quantity:
		// Sub-epsilon float drift: re-sync the watermark without an order.
		stamp := primary.Quantity
		return hedgePlan{StampCovered: &stamp}, nil
	}
	return hedgePlan{}, nil
}

// recordHedgeTradeResult books a hedge leg's realized PnL into the daily PnL
// (real money — the #1269 daily loss limit must see it) WITHOUT touching the
// consecutive-loss streak: a hedge leg is mechanical coupling, not an
// independent alpha outcome, and it typically loses exactly when the primary
// wins — feeding it to the streak would double-count every round trip and
// mis-fire the loss-streak circuit breaker (#1048/#1273 semantics).
func recordHedgeTradeResult(r *RiskState, pnl float64) {
	rolloverDailyPnL(r)
	r.DailyPnL += pnl
}

// recordCloseTradeResult routes a close leg's realized PnL to the right risk
// recorder: hedge legs skip the consecutive-loss streak (see
// recordHedgeTradeResult), everything else keeps the legacy behavior. Every
// shared close-booking path that can see an IsHedge position must call this
// instead of RecordTradeResult directly.
func recordCloseTradeResult(s *StrategyState, pos *Position, pnl float64) {
	if pos != nil && pos.IsHedge {
		recordHedgeTradeResult(&s.RiskState, pnl)
		return
	}
	RecordTradeResult(&s.RiskState, pnl)
}

// applyHedgeOpenFill books a confirmed hedge OPEN fill: creates the IsHedge
// position keyed by the hedge coin and records the open Trade. Mirrors the
// perps open-leg construction in executePerpsSignalWithLeverage (margin-based:
// only the fee leaves cash; notional stays virtual). Caller holds mu.Lock().
func applyHedgeOpenFill(s *StrategyState, sc StrategyConfig, primarySymbol, side string, fillQty, fillPx, fillFee float64, fillOID int64, covered float64, logger *StrategyLogger) *Position {
	if s == nil || fillQty <= hedgeQtyEpsilon || fillPx <= 0 || (side != "long" && side != "short") {
		return nil
	}
	coin := hedgeCoin(sc)
	if coin == "" {
		return nil
	}
	now := time.Now().UTC()
	fee := executionFee(CalculatePlatformSpotFee(s.Platform, fillQty*fillPx), fillFee, true)
	s.Cash -= fee
	positionID := newTradePositionID(s.ID, coin, now)
	pos := &Position{
		Symbol:                 coin,
		TradePositionID:        positionID,
		Quantity:               fillQty,
		InitialQuantity:        fillQty,
		AvgCost:                fillPx,
		Side:                   side,
		Multiplier:             1, // perps PnL-branch convention
		Leverage:               sc.Hedge.Leverage,
		OwnerStrategyID:        s.ID,
		OpenedAt:               now,
		IsHedge:                true,
		HedgePrimarySymbol:     primarySymbol,
		HedgeCoveredPrimaryQty: covered,
	}
	s.Positions[coin] = pos
	var oidStr string
	if fillOID > 0 {
		oidStr = fmt.Sprintf("%d", fillOID)
	}
	trade := Trade{
		Timestamp:       now,
		StrategyID:      s.ID,
		Symbol:          coin,
		PositionID:      positionID,
		Side:            openTradeSide(side),
		Quantity:        fillQty,
		Price:           fillPx,
		Value:           fillQty * fillPx,
		TradeType:       "perps",
		Details:         fmt.Sprintf("[hedge %s] Open %s %.6f @ $%.4f (ratio %gx, fee $%.4f)", primarySymbol, side, fillQty, fillPx, sc.Hedge.Ratio, fee),
		ExchangeOrderID: oidStr,
		ExchangeFee:     fee,
		FeeSource:       executionFeeSource(fillFee, true),
		PnLGross:        true,
	}
	trade.Regime = s.Regime
	RecordTrade(s, trade)
	if logger != nil {
		logger.Info("[hedge] opened %s %.6f %s @ $%.4f covering %.6f %s (fee $%.4f)", side, fillQty, coin, fillPx, covered, primarySymbol, fee)
	}
	return pos
}

// applyHedgeAddFill books a confirmed hedge ADD fill onto the existing leg:
// blends AvgCost, grows Quantity/InitialQuantity, and advances the covered
// watermark. Caller holds mu.Lock().
func applyHedgeAddFill(s *StrategyState, pos *Position, fillQty, fillPx, fillFee float64, fillOID int64, covered float64, logger *StrategyLogger) bool {
	if s == nil || pos == nil || !pos.IsHedge || fillQty <= hedgeQtyEpsilon || fillPx <= 0 {
		return false
	}
	now := time.Now().UTC()
	fee := executionFee(CalculatePlatformSpotFee(s.Platform, fillQty*fillPx), fillFee, true)
	s.Cash -= fee
	newQty := pos.Quantity + fillQty
	pos.AvgCost = (pos.Quantity*pos.AvgCost + fillQty*fillPx) / newQty
	pos.Quantity = newQty
	pos.InitialQuantity += fillQty
	pos.HedgeCoveredPrimaryQty = covered
	var oidStr string
	if fillOID > 0 {
		oidStr = fmt.Sprintf("%d", fillOID)
	}
	trade := Trade{
		Timestamp:       now,
		StrategyID:      s.ID,
		Symbol:          pos.Symbol,
		PositionID:      ensurePositionTradeID(s.ID, pos.Symbol, pos),
		Side:            openTradeSide(pos.Side),
		Quantity:        fillQty,
		Price:           fillPx,
		Value:           fillQty * fillPx,
		TradeType:       "perps",
		Details:         fmt.Sprintf("[hedge %s] Add %s %.6f @ $%.4f (fee $%.4f)", pos.HedgePrimarySymbol, pos.Side, fillQty, fillPx, fee),
		ExchangeOrderID: oidStr,
		ExchangeFee:     fee,
		FeeSource:       executionFeeSource(fillFee, true),
		PnLGross:        true,
	}
	trade.Regime = s.Regime
	RecordTrade(s, trade)
	if logger != nil {
		logger.Info("[hedge] added %.6f %s @ $%.4f (total %.6f, covering %.6f %s, fee $%.4f)", fillQty, pos.Symbol, fillPx, newQty, covered, pos.HedgePrimarySymbol, fee)
	}
	return true
}

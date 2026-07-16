package main

import (
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// HedgeConfig declares an opt-in, automatically managed correlated hedge leg
// (#1159 phase 1). Only consulted when Enabled is true. Phase 1 is Hyperliquid
// perps only: the hedge coin is a second Position under the same owning
// strategy, sized in notional terms at open/add and reduced proportionally as
// the primary shrinks. The scheduler manages the hedge exclusively by
// mirroring the primary's realized fills (open/scale-in/partial-close/
// full-close/flip) plus a per-cycle coherence-sync backstop that self-heals
// any drift from circuit-breaker drains, kill-switch closes, manual
// force-close, or an externally-caused reduction — no check script or close
// evaluator ever runs against the hedge symbol.
type HedgeConfig struct {
	// Enabled opts the strategy into the hedge leg. Default false (legacy
	// behavior unchanged).
	Enabled bool `json:"enabled"`
	// Symbol is the hedge instrument's HL coin ticker, e.g. "BTC". A ccxt-style
	// "BTC/USDC:USDC" form is also accepted and normalized. Required when
	// Enabled.
	Symbol string `json:"symbol"`
	// Side is the hedge side policy relative to the primary. Phase 1 supports
	// only "inverse" (primary long -> hedge short, primary short -> hedge
	// long). Empty defaults to "inverse".
	Side string `json:"side,omitempty"`
	// Ratio is the hedge notional as a multiple of the primary's fill notional
	// (e.g. 1.0 = fully notional-hedged). 0 defaults to 1.0. Must be in (0, 5].
	Ratio float64 `json:"ratio,omitempty"`
	// Platform must be "hyperliquid" in phase 1 (empty defaults to it).
	Platform string `json:"platform,omitempty"`
	// Type must be "perps" in phase 1 (empty defaults to it).
	Type string `json:"type,omitempty"`
	// MarginMode is the hedge leg's own HL margin mode ("isolated"|"cross");
	// empty defaults to "isolated". Never inherited from the primary
	// strategy's MarginMode — the hedge coin needs its own explicit on-chain
	// margin assignment (#1159 constraint 3).
	MarginMode string `json:"margin_mode,omitempty"`
	// Leverage is the hedge leg's own HL exchange leverage. 0 defaults to 1
	// (no leverage). Never inherited from the primary strategy's Leverage.
	Leverage float64 `json:"leverage,omitempty"`
}

// hedgeEnabled reports whether sc has an opted-in hedge leg (#1159 phase 1).
func hedgeEnabled(sc StrategyConfig) bool {
	return sc.Hedge != nil && sc.Hedge.Enabled
}

// hyperliquidHedgeCoin returns the hedge leg's coin ticker, normalized the
// same way hyperliquidConfiguredCoin normalizes the primary coin (upper-case,
// trimmed), additionally stripping a ccxt-style "BASE/QUOTE:SETTLE" suffix
// (e.g. "BTC/USDC:USDC" -> "BTC") since the issue's representative config
// shape uses the ccxt form while every other HL coin reference in this repo
// (Position.Symbol, hyperliquidSymbol(args)) uses the bare coin. Returns ""
// when hedge is not enabled or the symbol is empty.
func hyperliquidHedgeCoin(sc StrategyConfig) string {
	if !hedgeEnabled(sc) {
		return ""
	}
	raw := strings.ToUpper(strings.TrimSpace(sc.Hedge.Symbol))
	if idx := strings.IndexAny(raw, "/:"); idx >= 0 {
		raw = raw[:idx]
	}
	return raw
}

// effectiveHedgeRatio returns sc.Hedge.Ratio, defaulting 0 to 1.0.
func effectiveHedgeRatio(sc StrategyConfig) float64 {
	if !hedgeEnabled(sc) || sc.Hedge.Ratio <= 0 {
		return 1.0
	}
	return sc.Hedge.Ratio
}

// effectiveHedgeMarginMode returns sc.Hedge.MarginMode, defaulting "" to
// "isolated". Deliberately never falls back to the primary strategy's
// MarginMode (#1159 constraint 3): the hedge coin needs its own explicit
// on-chain margin assignment.
func effectiveHedgeMarginMode(sc StrategyConfig) string {
	if !hedgeEnabled(sc) || sc.Hedge.MarginMode == "" {
		return "isolated"
	}
	return sc.Hedge.MarginMode
}

// effectiveHedgeLeverage returns sc.Hedge.Leverage, defaulting 0 to 1.
func effectiveHedgeLeverage(sc StrategyConfig) float64 {
	if !hedgeEnabled(sc) || sc.Hedge.Leverage <= 0 {
		return 1
	}
	return sc.Hedge.Leverage
}

// validateHedgeConfig validates the phase-1 hedge block on a single strategy
// (#1159). Peer/self-collision checks run separately via
// hyperliquidHedgeStrategyErrors over the whole strategy list.
func validateHedgeConfig(prefix string, sc StrategyConfig, skipLiveCredentialChecks bool) []string {
	if sc.Hedge == nil || !sc.Hedge.Enabled {
		return nil
	}
	var errs []string
	if sc.Type != "perps" {
		errs = append(errs, fmt.Sprintf("%s: hedge is only supported for perps strategies (got type %q)", prefix, sc.Type))
	}
	if sc.Platform != "hyperliquid" {
		errs = append(errs, fmt.Sprintf("%s: hedge is only supported on hyperliquid (got platform %q)", prefix, sc.Platform))
	}
	if sc.Hedge.Platform != "" && sc.Hedge.Platform != "hyperliquid" {
		errs = append(errs, fmt.Sprintf("%s: hedge.platform must be \"hyperliquid\" (phase 1), got %q", prefix, sc.Hedge.Platform))
	}
	if sc.Hedge.Type != "" && sc.Hedge.Type != "perps" {
		errs = append(errs, fmt.Sprintf("%s: hedge.type must be \"perps\" (phase 1), got %q", prefix, sc.Hedge.Type))
	}
	if sc.Hedge.Side != "" && sc.Hedge.Side != "inverse" {
		errs = append(errs, fmt.Sprintf("%s: hedge.side must be \"inverse\" (phase 1), got %q", prefix, sc.Hedge.Side))
	}
	if sc.Hedge.Ratio != 0 && (sc.Hedge.Ratio <= 0 || sc.Hedge.Ratio > 5) {
		errs = append(errs, fmt.Sprintf("%s: hedge.ratio must be in (0, 5], got %g", prefix, sc.Hedge.Ratio))
	}
	switch sc.Hedge.MarginMode {
	case "", "isolated", "cross":
	default:
		errs = append(errs, fmt.Sprintf("%s: hedge.margin_mode must be \"isolated\" or \"cross\", got %q", prefix, sc.Hedge.MarginMode))
	}
	if sc.Hedge.Leverage != 0 && (sc.Hedge.Leverage < 1 || sc.Hedge.Leverage > 100) {
		errs = append(errs, fmt.Sprintf("%s: hedge.leverage must be in [1, 100], got %g", prefix, sc.Hedge.Leverage))
	}
	if strings.TrimSpace(sc.Hedge.Symbol) == "" {
		errs = append(errs, fmt.Sprintf("%s: hedge.symbol is required when hedge.enabled is true", prefix))
	}
	// #1159 review: syncHedgeAfterPrimaryFill and runHedgeCoherenceSync both
	// early-return on !hyperliquidIsLive, so a paper strategy with
	// hedge.enabled=true would otherwise book primary paper fills and never
	// open/manage a hedge — silently presenting as hedged while actually
	// running naked. Reject loudly at load rather than let that go
	// undetected; this also blocks a SIGHUP reload from landing a
	// live-hedge config back onto paper args (LoadConfig re-validates on
	// every reload) instead of leaving it stuck inert.
	if sc.Type == "perps" && sc.Platform == "hyperliquid" && !hyperliquidIsLive(sc.Args) {
		errs = append(errs, fmt.Sprintf("%s: hedge is live-only in phase 1 (got paper args) — the hedge leg would never open or be managed, silently leaving the primary unhedged", prefix))
	}
	if !skipLiveCredentialChecks && sc.Type == "perps" && sc.Platform == "hyperliquid" && hyperliquidIsLive(sc.Args) {
		// Coherence sync and kill-switch hedge closes both need on-chain reads
		// keyed by account address, same as the primary live-credential gate.
		if strings.TrimSpace(os.Getenv("HYPERLIQUID_ACCOUNT_ADDRESS")) == "" {
			errs = append(errs, fmt.Sprintf("%s: hedge requires HYPERLIQUID_ACCOUNT_ADDRESS to be set (live mode)", prefix))
		}
	}
	return errs
}

// hyperliquidHedgeStrategyErrors validates the phase-1 hedge collision rules
// (#1159 constraint 2): a hedge coin must not equal the owning strategy's own
// primary coin (that's not a hedge, it's a net-to-flat no-op), must not equal
// any configured HL strategy's primary coin (perps or manual, live or paper —
// HL aggregates positions per coin per account, and every shared-coin
// mechanism in this repo derives coin membership from the configured symbol
// only, so a hedge coin colliding with a peer's primary coin would be
// invisible to peer detection, margin compatibility, CB drain, kill-switch
// fill share, and reconcile owner mapping), and must not equal another
// hedge-enabled strategy's hedge coin (hedge-vs-hedge collision — two hedge
// legs on one coin would share an on-chain position, margin assignment, and
// reduce-only order slots). Overlap support is an explicit phase-1 follow-up.
func hyperliquidHedgeStrategyErrors(strategies []StrategyConfig) []string {
	var errs []string
	primaryCoins := make(map[string][]string) // coin -> strategy IDs configuring it as primary
	for _, sc := range strategies {
		if sc.Platform != "hyperliquid" {
			continue
		}
		coin := hyperliquidConfiguredCoin(sc)
		if coin == "" {
			continue
		}
		primaryCoins[coin] = append(primaryCoins[coin], sc.ID)
	}
	hedgeCoins := make(map[string][]string) // coin -> strategy IDs configuring it as a hedge
	var hedgeStrategies []StrategyConfig
	for _, sc := range strategies {
		if !hedgeEnabled(sc) {
			continue
		}
		hedgeStrategies = append(hedgeStrategies, sc)
		hc := hyperliquidHedgeCoin(sc)
		if hc == "" {
			continue
		}
		hedgeCoins[hc] = append(hedgeCoins[hc], sc.ID)
	}
	for _, sc := range hedgeStrategies {
		hc := hyperliquidHedgeCoin(sc)
		if hc == "" {
			continue
		}
		primaryCoin := hyperliquidConfiguredCoin(sc)
		if primaryCoin != "" && hc == primaryCoin {
			errs = append(errs, fmt.Sprintf(
				"strategy %s: hedge.symbol %q resolves to the strategy's own primary coin %s — a same-coin \"hedge\" just nets the position on-chain",
				sc.ID, sc.Hedge.Symbol, hc))
		}
		if ids, ok := primaryCoins[hc]; ok {
			for _, id := range ids {
				if id == sc.ID {
					continue
				}
				errs = append(errs, fmt.Sprintf(
					"strategy %s: hedge coin %s collides with strategy %s's configured primary coin — HL aggregates positions per coin per account; shared-coin machinery does not see hedge legs in phase 1",
					sc.ID, hc, id))
			}
		}
	}
	hcCoins := make([]string, 0, len(hedgeCoins))
	for coin := range hedgeCoins {
		hcCoins = append(hcCoins, coin)
	}
	sort.Strings(hcCoins)
	for _, coin := range hcCoins {
		ids := hedgeCoins[coin]
		if len(ids) < 2 {
			continue
		}
		sort.Strings(ids)
		errs = append(errs, fmt.Sprintf(
			"hyperliquid hedge coin %s is configured by multiple hedge-enabled strategies (%s): two hedge legs on one coin would share an on-chain position, margin assignment, and reduce-only order slots",
			coin, strings.Join(ids, ", ")))
	}
	return errs
}

// hedgeConfigEqual compares two hedge blocks for hot-reload purposes (#1159
// constraint 7): any change to enable/disable, symbol, side, ratio, or margin
// fields is state-shifting and must be blocked while a position is open, same
// shape as scaleInConfigEqual.
func hedgeConfigEqual(a, b *HedgeConfig) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

// ---------------------------------------------------------------------------
// Pure sizing/side engine (#1159). No subprocess/lock dependencies — unit
// testable in isolation.
// ---------------------------------------------------------------------------

// hedgeSideForPrimary maps the primary position's side to the hedge side
// under the phase-1 "inverse" policy: long -> short, short -> long. Any other
// input (should never occur — validated at config load) returns "".
func hedgeSideForPrimary(primarySide string) string {
	switch primarySide {
	case "long":
		return "short"
	case "short":
		return "long"
	default:
		return ""
	}
}

// hedgeOpenQty computes the hedge quantity for a primary fill of qty
// primaryFillQty at price primaryFillPx, hedged at the given notional ratio
// against the current hedge-coin mid. Returns 0 (fail closed) on any
// non-positive input.
func hedgeOpenQty(primaryFillPx, primaryFillQty, ratio, hedgeMid float64) float64 {
	if primaryFillPx <= 0 || primaryFillQty <= 0 || ratio <= 0 || hedgeMid <= 0 {
		return 0
	}
	notional := primaryFillPx * primaryFillQty * ratio
	return notional / hedgeMid
}

// hedgeTargetQty is the coherence-sync target: the hedge quantity implied by
// the primary's current quantity and the frozen per-primary-unit hedge ratio
// stamped at the hedge's most recent open/add.
func hedgeTargetQty(primaryQty, hedgeRatioQty float64) float64 {
	if primaryQty <= 0 || hedgeRatioQty <= 0 {
		return 0
	}
	return primaryQty * hedgeRatioQty
}

// hedgeAdjustDelta compares the current hedge quantity to the target and
// returns the reduce-only quantity to bring it back in line, plus whether the
// hedge is under target beyond tolerance. A tolerance band (max of 0.5% of
// target and a small absolute dust epsilon) absorbs sz-decimal rounding so
// coherence sync never chases its own tail. The hedge is NEVER increased here
// — only reduced or left alone; under-hedge is alert-only (#1159 design:
// never guess a fill, never auto-grow a mirror from a backstop).
func hedgeAdjustDelta(currentHedgeQty, target float64) (reduceBy float64, underHedged bool) {
	if currentHedgeQty <= 0 {
		return 0, target > 1e-9
	}
	tolerance := target * 0.005
	if tolerance < 1e-6 {
		tolerance = 1e-6
	}
	if currentHedgeQty > target+tolerance {
		return currentHedgeQty - target, false
	}
	if currentHedgeQty < target-tolerance {
		return 0, true
	}
	return 0, false
}

// hedgeLegSideMismatched reports whether a hedge position's side no longer
// inverts its primary's current side (#1159 review) — e.g. a flip whose
// old-hedge close under-filled or failed, leaving a stale same-direction
// residual that would amplify the primary instead of hedging it.
// expectedSide=="" means the primary side couldn't be resolved (e.g. no
// primary position) — never counts as a mismatch, so a data gap never
// flattens a legitimate hedge.
func hedgeLegSideMismatched(hedgeSide, expectedSide string) bool {
	return expectedSide != "" && hedgeSide != expectedSide
}

// ---------------------------------------------------------------------------
// State booking helpers. Callers must hold mu.Lock().
// ---------------------------------------------------------------------------

// applyHedgeOpenToState books a confirmed hedge open or add fill: creates the
// hedge Position on a fresh open, or blends into the existing one on an add,
// stamps IsHedge/HedgeForPositionID/HedgeRatioQty from the ACTUAL resulting
// quantities (never the requested size), and records an IsHedge Trade against
// the owning strategy's ledger (#1159 requirement 6: hedge PnL/fees book to
// the owner, never a peer).
//
// #1159 review: a same-symbol fill can arrive with the OPPOSITE side of an
// existing residual hedge position — a flip's close-old leg only partially
// filled, or the primary went flat and re-opened the other direction while a
// prior-direction hedge residual survived a failed/partial close. Blindly
// adding fillQty and re-blending AvgCost across opposite sides would corrupt
// Quantity/AvgCost/Side and invert every downstream PnL sign. Net it the way
// an exchange would instead: close up to the residual's quantity first
// (booking real realized PnL via bookHedgeCloseFill, fee pro-rated), then
// open any excess fill on the new side. This makes the invariant hold
// regardless of caller control flow — every caller of this function is safe
// from side corruption, not just the flip path.
func applyHedgeOpenToState(s *StrategyState, primarySym, hedgeSym, side string, fillQty, fillPx, fillFee float64, oid int64, hedgeLeverage float64, hedgePrimaryQtyAfter float64) {
	if s == nil || fillQty <= 0 || fillPx <= 0 || side == "" {
		return
	}
	if existing, ok := s.Positions[hedgeSym]; ok && existing != nil && existing.Quantity > 1e-9 && existing.Side != "" && existing.Side != side {
		netQty := fillQty
		if netQty > existing.Quantity {
			netQty = existing.Quantity
		}
		netFee := fillFee * (netQty / fillQty)
		bookHedgeCloseFill(s, hedgeSym, netQty, fillPx, netFee, oid, "hedge_mirror_flip_net")
		fillQty -= netQty
		fillFee -= netFee
		if fillQty <= 1e-9 {
			return
		}
		// Excess beyond the residual opens fresh on the new side — fall
		// through with the remaining fillQty/fillFee and a guaranteed-clean
		// (deleted-by-close, or never existed) map slot.
	}
	now := time.Now().UTC()
	pos, existed := s.Positions[hedgeSym]
	if !existed || pos == nil {
		pos = &Position{
			Symbol:     hedgeSym,
			Side:       side,
			Multiplier: 1,
			Leverage:   hedgeLeverage,
			OpenedAt:   now,
		}
		s.Positions[hedgeSym] = pos
	}
	// #1159 review: the on-chain wallet really paid this taker fee, same as a
	// primary perps open (portfolio.go: `s.Cash -= fee`). Omitting it here
	// overstates Go-side cash by every hedge open/add fee, drifting
	// PortfolioValue/drawdown/leaderboard/daily-loss basis upward and
	// eventually tripping the live HL total-drift journal / shared-wallet
	// drift tracker's $0.01 tolerance. Covers the netting-excess open too:
	// fillFee has already been reduced by netFee above, so this deducts only
	// the fee actually attributable to the fresh-side notional.
	s.Cash -= fillFee
	totalCost := pos.AvgCost*pos.Quantity + fillPx*fillQty
	pos.Quantity += fillQty
	if pos.Quantity > 0 {
		pos.AvgCost = totalCost / pos.Quantity
	}
	if pos.InitialQuantity == 0 {
		pos.InitialQuantity = pos.Quantity
	}
	pos.OwnerStrategyID = s.ID
	pos.IsHedge = true
	if primaryPos, ok := s.Positions[primarySym]; ok && primaryPos != nil {
		pos.HedgeForPositionID = ensurePositionTradeID(s.ID, primarySym, primaryPos)
	}
	if hedgePrimaryQtyAfter > 0 {
		pos.HedgeRatioQty = pos.Quantity / hedgePrimaryQtyAfter
	}

	var oidStr string
	if oid > 0 {
		oidStr = strconv.FormatInt(oid, 10)
	}
	side2 := "buy"
	if side == "short" {
		side2 = "sell"
	}
	RecordTrade(s, Trade{
		Timestamp:       now,
		StrategyID:      s.ID,
		Symbol:          hedgeSym,
		PositionID:      ensurePositionTradeID(s.ID, hedgeSym, pos),
		Side:            side2,
		Quantity:        fillQty,
		Price:           fillPx,
		Value:           fillQty * fillPx,
		TradeType:       "perps",
		Details:         fmt.Sprintf("Correlated hedge open/add %.6f @ $%.4f (fee $%.4f)", fillQty, fillPx, fillFee),
		ExchangeOrderID: oidStr,
		ExchangeFee:     fillFee,
		FeeSource:       FeeSourceUserFills,
		PnLGross:        true,
		IsHedge:         true,
		Regime:          s.Regime,
	})
}

// bookHedgeCloseFill books a confirmed hedge reduce/close fill (full or
// partial) with PnL accounting mirroring bookPerpsPartialCloseWithFillFee /
// bookPerpsCloseWithFillFee, but stamps IsHedge:true on the resulting Trade
// so lifetime trade-count stats and operator surfaces can distinguish hedge
// round-trips from primary alpha trades. closeQty<=0 or >= pos.Quantity
// closes the entire hedge leg.
func bookHedgeCloseFill(s *StrategyState, hedgeSym string, closeQty, closePx, fillFee float64, oid int64, reason string) bool {
	if s == nil || closePx <= 0 {
		return false
	}
	pos, ok := s.Positions[hedgeSym]
	if !ok || pos == nil || pos.Quantity <= 0 {
		return false
	}
	now := time.Now().UTC()
	qty := closeQty
	if qty <= 0 || qty > pos.Quantity {
		qty = pos.Quantity
	}
	side := pos.Side
	avgCost := pos.AvgCost
	var pnl float64
	if side == "long" {
		pnl = qty * (closePx - avgCost)
	} else {
		pnl = qty * (avgCost - closePx)
	}
	grossPnL := pnl
	pnl -= fillFee
	s.Cash += pnl
	positionID := ensurePositionTradeID(s.ID, hedgeSym, pos)

	var oidStr string
	if oid > 0 {
		oidStr = strconv.FormatInt(oid, 10)
	}
	RecordTrade(s, Trade{
		Timestamp:       now,
		StrategyID:      s.ID,
		Symbol:          hedgeSym,
		PositionID:      positionID,
		Side:            closeTradeSide(side),
		Quantity:        qty,
		Price:           closePx,
		Value:           qty * closePx,
		TradeType:       "perps",
		Details:         fmt.Sprintf("Correlated hedge %s %.6f @ $%.4f, PnL: $%.2f (fee $%.4f)", reason, qty, closePx, pnl, fillFee),
		IsClose:         true,
		RealizedPnL:     grossPnL,
		PnLGross:        true,
		ExchangeOrderID: oidStr,
		ExchangeFee:     fillFee,
		FeeSource:       FeeSourceUserFills,
		IsHedge:         true,
		Regime:          s.Regime,
	})
	recordHedgeDailyPnLOnly(&s.RiskState, pnl)

	remaining := pos.Quantity - qty
	if remaining <= 1e-9 {
		recordClosedPosition(s, pos, closePx, pnl, reason, now)
		delete(s.Positions, hedgeSym)
	} else {
		pos.Quantity = remaining
	}
	return true
}

// recordHedgeDailyPnLOnly applies a hedge close's realized PnL to the
// strategy's daily-PnL accounting (portfolio_risk daily-loss limits still see
// the real cash impact) WITHOUT touching RiskState.ConsecutiveLosses (#1159
// review). A hedge leg's PnL is inverse-correlated to the primary's and books
// AFTER it in the same cycle (main.go), so routing it through
// RecordTradeResult would reset the loss-streak counter on every winning
// hedge (i.e. every losing primary) — effectively disabling the loss-streak
// circuit breaker on any hedge-enabled strategy. Same "hedge is not alpha"
// reasoning that already excludes IsHedge legs from lifetime W/L stats
// (db.go LifetimeTradeStats*).
func recordHedgeDailyPnLOnly(r *RiskState, pnl float64) {
	rolloverDailyPnL(r)
	r.DailyPnL += pnl
}

// ---------------------------------------------------------------------------
// Live order wrappers. No lock held; subprocess side effects only.
// ---------------------------------------------------------------------------

// runHedgeOpenOrder submits a fresh hedge open or add via the same
// check_hyperliquid.py --execute path the primary leg uses (no dedicated
// script, no check-script surface for the hedge symbol — #1159 constraint 5).
func runHedgeOpenOrder(sc StrategyConfig, hedgeSym, side string, qty float64, hlPositions []HLPosition, notifier *MultiNotifier) (*HyperliquidExecuteResult, bool) {
	if qty <= 0 || side == "" {
		return nil, false
	}
	snapshot := hlExecuteSnapshotForCoin(hlPositions, hedgeSym)
	execResult, stderr, err := RunHyperliquidExecute(sc.Script, hedgeSym, side, qty, 0, 0, 0,
		effectiveHedgeMarginMode(sc), effectiveHedgeLeverage(sc), false, snapshot)
	if stderr != "" {
		fmt.Printf("[hedge] %s: execute stderr: %s\n", sc.ID, stderr)
	}
	if err != nil {
		notifyLiveExecFailure(notifier, sc, "hedge_open", hedgeSym, err.Error())
		return execResult, false
	}
	if execResult == nil || execResult.Error != "" {
		msg := "empty result"
		if execResult != nil {
			msg = execResult.Error
		}
		notifyLiveExecFailure(notifier, sc, "hedge_open", hedgeSym, msg)
		return execResult, false
	}
	return execResult, true
}

// runHedgeReduceOrder submits a reduce-only hedge close (partialSz nil = full
// market_close).
func runHedgeReduceOrder(sc StrategyConfig, hedgeSym string, partialSz *float64, notifier *MultiNotifier) (*HyperliquidCloseResult, bool) {
	result, stderr, err := RunHyperliquidClose(sc.Script, hedgeSym, partialSz, nil)
	if stderr != "" {
		fmt.Printf("[hedge] %s: close stderr: %s\n", sc.ID, stderr)
	}
	if err != nil {
		notifyLiveExecFailure(notifier, sc, "hedge_reduce", hedgeSym, err.Error())
		return result, false
	}
	if result == nil || result.Error != "" {
		msg := "empty result"
		if result != nil {
			msg = result.Error
		}
		notifyLiveExecFailure(notifier, sc, "hedge_reduce", hedgeSym, msg)
		return result, false
	}
	return result, true
}

// ---------------------------------------------------------------------------
// Dispatch integration: mirror a confirmed primary fill onto the hedge leg.
// ---------------------------------------------------------------------------

// syncHedgeAfterPrimaryFill mirrors a just-booked primary fill onto the
// strategy's hedge leg (#1159). Must be called AFTER the primary fill has
// already been committed to state (mu released) with prevQty/prevSide
// capturing the PRE-cycle snapshot. The mirror is driven entirely by the
// realized (prev -> now) position diff rather than by replaying the
// primary's own dispatch predicates, so it can never disagree with whatever
// the primary sizer actually did — including on partial fills.
//
// Constraint 4 (fail closed): when this is a fresh open (prevQty==0) and the
// hedge order fails, the primary leg is immediately unwound (reduce-only,
// sized to the exact fill) and the operator is alerted — the strategy must
// never run unhedged silently. A scale-in add failure unwinds only the added
// delta. Signal-close/partial-close hedge-reduce failures are NOT unwound
// (the primary already de-risked); they alert and rely on the per-cycle
// runHedgeCoherenceSync backstop to retry.
//
// primaryFillPx is the ACTUAL fill price of the order that just ran
// (execResult.Execution.Fill.AvgPx), never Position.AvgCost: on a scale-in
// add, AvgCost is the BLENDED cost across the whole position (old + new
// legs), not the incremental add's own fill price, so sizing the hedge
// notional off it would over/under-hedge after any price move since entry.
// Fresh-open and flip fills happen to have AvgCost == fill price (nothing to
// blend with), but using the real fill price uniformly removes that
// coincidental coupling.
func syncHedgeAfterPrimaryFill(sc StrategyConfig, s *StrategyState, mu *sync.RWMutex, hlPositions []HLPosition, prevQty float64, prevSide string, primaryFillPx float64, notifier *MultiNotifier, logger *StrategyLogger) {
	if !hedgeEnabled(sc) || !hyperliquidIsLive(sc.Args) {
		return
	}
	primarySym := hyperliquidSymbol(sc.Args)
	hedgeSym := hyperliquidHedgeCoin(sc)
	if primarySym == "" || hedgeSym == "" {
		return
	}

	mu.RLock()
	var newQty float64
	var newSide string
	if p, ok := s.Positions[primarySym]; ok && p != nil {
		newQty = p.Quantity
		newSide = p.Side
	}
	var hedgeQty float64
	if hp, ok := s.Positions[hedgeSym]; ok && hp != nil {
		hedgeQty = hp.Quantity
	}
	mu.RUnlock()

	const eps = 1e-12
	flipped := prevQty > eps && newQty > eps && prevSide != "" && newSide != "" && prevSide != newSide

	if flipped {
		if hedgeQty > eps {
			reduceHedgeAndBook(sc, s, mu, hedgeSym, nil, "hedge_mirror_flip", notifier, logger)
		}
		openHedgeForPrimaryDelta(sc, s, mu, hlPositions, primarySym, hedgeSym, newSide, newQty, primaryFillPx, newQty, notifier, logger, true)
		return
	}
	if prevQty <= eps && newQty > eps {
		// Fresh open.
		openHedgeForPrimaryDelta(sc, s, mu, hlPositions, primarySym, hedgeSym, newSide, newQty, primaryFillPx, newQty, notifier, logger, true)
		return
	}
	if newQty > prevQty+eps {
		// Scale-in add.
		delta := newQty - prevQty
		openHedgeForPrimaryDelta(sc, s, mu, hlPositions, primarySym, hedgeSym, newSide, delta, primaryFillPx, newQty, notifier, logger, false)
		return
	}
	if newQty < prevQty-eps {
		closedDelta := prevQty - newQty
		if newQty <= eps {
			if hedgeQty > eps {
				reduceHedgeAndBook(sc, s, mu, hedgeSym, nil, "hedge_mirror_close", notifier, logger)
			}
			return
		}
		frac := closedDelta / prevQty
		if hedgeQty > eps && frac > 0 {
			reduceQty := hedgeQty * frac
			reduceHedgeAndBook(sc, s, mu, hedgeSym, &reduceQty, "hedge_mirror_close", notifier, logger)
		}
	}
}

// openHedgeForPrimaryDelta sizes and submits a hedge open/add for a
// deltaPrimaryQty fill priced at primaryPx, then books the fill (or, on
// failure, fails closed per constraint 4 semantics driven by isFreshOpen).
func openHedgeForPrimaryDelta(sc StrategyConfig, s *StrategyState, mu *sync.RWMutex, hlPositions []HLPosition, primarySym, hedgeSym, primarySide string, deltaPrimaryQty, primaryPx, primaryQtyAfter float64, notifier *MultiNotifier, logger *StrategyLogger, isFreshOpen bool) {
	hedgeSide := hedgeSideForPrimary(primarySide)
	if hedgeSide == "" || deltaPrimaryQty <= 0 || primaryPx <= 0 {
		return
	}
	mids, err := fetchHyperliquidMids([]string{hedgeSym})
	if err != nil || mids[hedgeSym] <= 0 {
		msg := "hedge mid unavailable"
		if err != nil {
			msg = err.Error()
		}
		notifyLiveExecFailure(notifier, sc, "hedge_open", hedgeSym, msg)
		failHedgeOpenClosed(sc, s, mu, primarySym, deltaPrimaryQty, notifier, logger, isFreshOpen)
		return
	}
	qty := hedgeOpenQty(primaryPx, deltaPrimaryQty, effectiveHedgeRatio(sc), mids[hedgeSym])
	if qty <= 0 {
		notifyLiveExecFailure(notifier, sc, "hedge_open", hedgeSym, "computed hedge qty <= 0")
		failHedgeOpenClosed(sc, s, mu, primarySym, deltaPrimaryQty, notifier, logger, isFreshOpen)
		return
	}
	execResult, ok := runHedgeOpenOrder(sc, hedgeSym, hedgeSide, qty, hlPositions, notifier)
	if !ok || execResult == nil || execResult.Execution == nil || execResult.Execution.Fill == nil {
		failHedgeOpenClosed(sc, s, mu, primarySym, deltaPrimaryQty, notifier, logger, isFreshOpen)
		return
	}
	fill := execResult.Execution.Fill
	mu.Lock()
	applyHedgeOpenToState(s, primarySym, hedgeSym, hedgeSide, fill.TotalSz, fill.AvgPx, fill.Fee, fill.OID, effectiveHedgeLeverage(sc), primaryQtyAfter)
	mu.Unlock()
	if logger != nil {
		logger.Info("Hedge %s opened/added %.6f %s @ $%.4f (ratio target primary_qty=%.6f)", hedgeSym, fill.TotalSz, hedgeSide, fill.AvgPx, primaryQtyAfter)
	}
}

// failHedgeOpenClosed implements #1159 constraint 4: when the hedge open
// fails, the primary is unwound reduce-only, sized to EXACTLY the just-filled
// delta (deltaQty — the full open qty on a fresh open, or just the added
// delta on a scale-in add) so the strategy never runs unhedged silently.
// This must NEVER be a nil-sized market_close: #491 permits two HL perps
// strategies to share a primary coin (the hedge collision rules only
// restrict the hedge coin, never the primary), and a nil-sized close flattens
// the ENTIRE on-chain net for the coin — including a peer's share. Sizing to
// deltaQty is peer-safe on a shared coin and equivalent to a full close on a
// sole-owned one.
func failHedgeOpenClosed(sc StrategyConfig, s *StrategyState, mu *sync.RWMutex, primarySym string, deltaQty float64, notifier *MultiNotifier, logger *StrategyLogger, isFreshOpen bool) {
	closeResult, stderr, err := RunHyperliquidClose(sc.Script, primarySym, &deltaQty, nil)
	if stderr != "" {
		fmt.Printf("[hedge] %s: unwind stderr: %s\n", sc.ID, stderr)
	}
	if err != nil || closeResult == nil || closeResult.Error != "" || closeResult.Close == nil || closeResult.Close.Fill == nil {
		msg := "hedge open failed and primary unwind ALSO failed — strategy is running UNHEDGED"
		if err != nil {
			msg += ": " + err.Error()
		} else if closeResult != nil && closeResult.Error != "" {
			msg += ": " + closeResult.Error
		}
		if logger != nil {
			logger.Error("CRITICAL: %s", msg)
		}
		if notifier != nil && notifier.HasBackends() {
			alert := fmt.Sprintf("**HEDGE OPEN FAILED — UNWIND ALSO FAILED** [%s] %s: %s", sc.ID, primarySym, msg)
			notifier.SendToAllChannels(alert)
			notifier.SendOwnerDM(alert)
		}
		return
	}
	fill := closeResult.Close.Fill
	reason, detailsPrefix, logPrefix := "hedge_open_failed_unwind", "Hedge open failed — unwinding primary", "Hedge-unwind close"
	if !isFreshOpen {
		reason, detailsPrefix, logPrefix = "hedge_add_failed_unwind", "Hedge add failed — unwinding added delta", "Hedge-unwind partial close"
	}
	mu.Lock()
	// Always the partial/sized booking path — deltaQty equals the position's
	// full quantity on a fresh open (sole owner), so this still deletes the
	// position when remaining <= 1e-9; on a shared coin it correctly leaves
	// the peer's share untouched.
	booked := bookPerpsPartialCloseWithFillFee(s, primarySym, fill.TotalSz, fill.AvgPx, fill.Fee, true, strconv.FormatInt(fill.OID, 10), reason, detailsPrefix, logPrefix, logger)
	mu.Unlock()
	if !booked && logger != nil {
		logger.Error("CRITICAL: hedge-open-failed unwind fill for %s could not be booked (no matching virtual position)", primarySym)
	}
	if notifier != nil && notifier.HasBackends() {
		alert := fmt.Sprintf("**HEDGE OPEN FAILED — PRIMARY UNWOUND** [%s] %s: could not open the configured hedge leg; the primary fill was closed reduce-only rather than run unhedged. Investigate hedge-coin liquidity/margin before re-enabling.", sc.ID, primarySym)
		notifier.SendToAllChannels(alert)
		notifier.SendOwnerDM(alert)
	}
}

// reduceHedgeAndBook submits a reduce-only hedge order (qty nil = full close)
// and books the resulting fill. Failures are alert-only — the primary side
// has already de-risked, so there is nothing to unwind; runHedgeCoherenceSync
// retries next cycle.
func reduceHedgeAndBook(sc StrategyConfig, s *StrategyState, mu *sync.RWMutex, hedgeSym string, qty *float64, reason string, notifier *MultiNotifier, logger *StrategyLogger) {
	if qty != nil && *qty <= 0 {
		return
	}
	result, ok := runHedgeReduceOrder(sc, hedgeSym, qty, notifier)
	if !ok || result == nil || result.Close == nil {
		return
	}
	if result.Close.AlreadyFlat {
		return
	}
	fill := result.Close.Fill
	if fill == nil || fill.TotalSz <= 0 || fill.AvgPx <= 0 {
		return
	}
	mu.Lock()
	bookHedgeCloseFill(s, hedgeSym, fill.TotalSz, fill.AvgPx, fill.Fee, fill.OID, reason)
	mu.Unlock()
	if logger != nil {
		logger.Info("Hedge %s reduced %.6f @ $%.4f (%s)", hedgeSym, fill.TotalSz, fill.AvgPx, reason)
	}
}

// ---------------------------------------------------------------------------
// Coherence-sync backstop.
// ---------------------------------------------------------------------------

// runHedgeCoherenceSync is the per-cycle backstop that keeps every
// hedge-enabled strategy's hedge leg coherent with its primary, WITHOUT
// requiring a dedicated inline hook at every primary-reducing code path (CB
// drain, kill-switch close, manual force-close, on-chain TP/SL fill,
// hl_sync_external, or a restart mid-sequence). It:
//
//  1. Reduces an over-hedged leg (persisted hedge qty > target =
//     primaryQty * HedgeRatioQty) via a reduce-only order — the target is
//     re-derived from the CURRENT primary qty every cycle, so any primary
//     reduction from any source is picked up here within one cycle.
//  2. Fully closes the hedge when the primary has gone flat.
//  3. NEVER increases a hedge or guesses a fill: an under-hedge beyond
//     tolerance, or a hedge leg with no matching primary, is alert-only.
//
// This also serves as the hedge's startup reconcile: hedge ownership is read
// exclusively from persisted Position.IsHedge/HedgeForPositionID metadata —
// never inferred from hyperliquidHedgeCoin — so the first post-restart cycle
// self-heals any drift that happened while the process was down (#1159
// constraint 5 / acceptance 3).
func runHedgeCoherenceSync(state *AppState, strategies []StrategyConfig, hlPositions []HLPosition, hlStateFetched bool, mu *sync.RWMutex, notifier *MultiNotifier, logMgr *LogManager) {
	if state == nil || !hlStateFetched {
		return
	}
	type hedgeJob struct {
		sc           StrategyConfig
		hedgeSym     string
		reduceBy     float64
		underHedge   bool
		fullClose    bool
		sideMismatch bool
	}
	var jobs []hedgeJob
	// checkedKeys covers every live hedge-enabled strategy evaluated this
	// cycle (regardless of whether it currently holds a hedge position) so a
	// throttled under-hedge alert can be recovered even if the hedge leg
	// later closes entirely rather than catching back up to the ratio.
	var checkedKeys []string

	mu.RLock()
	ids := make([]string, 0, len(strategies))
	byID := make(map[string]StrategyConfig, len(strategies))
	for _, sc := range strategies {
		if !hedgeEnabled(sc) || !hyperliquidIsLive(sc.Args) {
			continue
		}
		ids = append(ids, sc.ID)
		byID[sc.ID] = sc
	}
	sort.Strings(ids)
	for _, id := range ids {
		sc := byID[id]
		s, ok := state.Strategies[id]
		if !ok || s == nil {
			continue
		}
		primarySym := hyperliquidSymbol(sc.Args)
		hedgeSym := hyperliquidHedgeCoin(sc)
		if primarySym == "" || hedgeSym == "" {
			continue
		}
		checkedKeys = append(checkedKeys, hedgeUnderHedgeKey(id, hedgeSym))
		var primaryQty float64
		var primarySide string
		if p, ok := s.Positions[primarySym]; ok && p != nil {
			primaryQty = p.Quantity
			primarySide = p.Side
		}
		hp, hasHedge := s.Positions[hedgeSym]
		if !hasHedge || hp == nil || !hp.IsHedge {
			continue
		}
		if primaryQty <= 1e-12 {
			jobs = append(jobs, hedgeJob{sc: sc, hedgeSym: hedgeSym, fullClose: true})
			continue
		}
		// #1159 review: a leg flagged IsHedge must always sit on the inverse
		// side of its primary. A flip whose old-hedge close order failed or
		// underfilled while the new-side open only partially netted the
		// residual (applyHedgeOpenToState's fillQty<=residual early return)
		// can leave a SAME-direction leg still stamped IsHedge with a stale
		// HedgeRatioQty — it now amplifies the primary instead of hedging it.
		// hedgeAdjustDelta below is side-blind (quantities only) and would
		// "correct" that leg toward a bogus ratio target rather than flatten
		// it, so detect the direction mismatch first and flatten
		// unconditionally — same reduce-only, collision-free path as a
		// flat-primary full close.
		if hedgeLegSideMismatched(hp.Side, hedgeSideForPrimary(primarySide)) {
			jobs = append(jobs, hedgeJob{sc: sc, hedgeSym: hedgeSym, fullClose: true, sideMismatch: true})
			continue
		}
		target := hedgeTargetQty(primaryQty, hp.HedgeRatioQty)
		reduceBy, under := hedgeAdjustDelta(hp.Quantity, target)
		if reduceBy > 1e-9 || under {
			jobs = append(jobs, hedgeJob{sc: sc, hedgeSym: hedgeSym, reduceBy: reduceBy, underHedge: under})
		}
	}
	mu.RUnlock()

	now := time.Now().UTC()
	underHedgeThisCycle := make(map[string]bool, len(jobs))
	for _, j := range jobs {
		var logger *StrategyLogger
		if logMgr != nil {
			if l, err := logMgr.GetStrategyLogger(j.sc.ID); err == nil {
				logger = l
			}
		}
		if j.underHedge {
			key := hedgeUnderHedgeKey(j.sc.ID, j.hedgeSym)
			underHedgeThisCycle[key] = true
			// #1159 review: an under-hedge is never auto-grown, so it can
			// persist indefinitely — throttle to the standard 1st/10th/hourly
			// cadence (same as live-exec failure alerts) instead of a DM
			// every cycle.
			if shouldNotify, count := hedgeUnderHedgeThrottle.Record(key, "under_hedge", now); shouldNotify && notifier != nil && notifier.HasBackends() {
				countNote := ""
				if count > 1 {
					countNote = fmt.Sprintf(" (persisting, cycle #%d)", count)
				}
				msg := fmt.Sprintf("**HEDGE UNDER TARGET**%s [%s] %s hedge leg is under the ratio target — never auto-grown by the coherence sync. Investigate (a scale-in-add hedge mirror may have failed).", countNote, j.sc.ID, j.hedgeSym)
				notifier.SendToAllChannels(msg)
				notifier.SendOwnerDM(msg)
			}
			continue
		}
		if j.fullClose {
			reason := "hedge_coherence"
			if j.sideMismatch {
				reason = "hedge_side_mismatch_flatten"
				if logger != nil {
					logger.Error("CRITICAL: %s hedge leg %s was on the SAME side as its primary (not inverse) — flattening immediately", j.sc.ID, j.hedgeSym)
				}
				if notifier != nil && notifier.HasBackends() {
					alert := fmt.Sprintf("**HEDGE SIDE MISMATCH — FLATTENED** [%s] %s: hedge leg was on the same side as its primary (amplifying, not hedging) — likely a flip whose old-hedge close under-filled. Flattened reduce-only; investigate hedge-coin liquidity around the flip.", j.sc.ID, j.hedgeSym)
					notifier.SendToAllChannels(alert)
					notifier.SendOwnerDM(alert)
				}
			}
			reduceHedgeAndBook(j.sc, state.Strategies[j.sc.ID], mu, j.hedgeSym, nil, reason, notifier, logger)
			continue
		}
		qty := j.reduceBy
		reduceHedgeAndBook(j.sc, state.Strategies[j.sc.ID], mu, j.hedgeSym, &qty, "hedge_coherence", notifier, logger)
	}

	// Recovery: any previously-alerted key that was checked this cycle but is
	// no longer under-hedged (ratio caught up, or the hedge leg closed
	// entirely) gets an explicit recovered notice instead of just going quiet.
	for _, key := range checkedKeys {
		if underHedgeThisCycle[key] || !hedgeUnderHedgeThrottle.Had(key) {
			continue
		}
		hedgeUnderHedgeThrottle.Clear(key)
		if notifier != nil && notifier.HasBackends() {
			msg := fmt.Sprintf("**HEDGE UNDER TARGET RECOVERED** %s", key)
			notifier.SendToAllChannels(msg)
			notifier.SendOwnerDM(msg)
		}
	}
}

// hedgeUnderHedgeKey builds the throttle key for a strategy's under-hedge
// condition, keyed by (strategy, hedge coin) so distinct strategies/hedges
// never share a throttle slot.
func hedgeUnderHedgeKey(strategyID, hedgeSym string) string {
	return strategyID + "|" + hedgeSym
}

// hedgeUnderHedgeThrottle is the package-level singleton tracking the
// coherence sync's under-hedge alert cadence (#1159 review). In-memory only;
// resets on restart, so the first under-hedge cycle after a restart always
// notifies — consistent with every other throttle in this package.
var hedgeUnderHedgeThrottle = &LiveExecFailureThrottle{}

// ---------------------------------------------------------------------------
// Kill switch integration.
// ---------------------------------------------------------------------------

// hedgeCoinsForKillSwitch returns the set of hedge coins the kill switch must
// also treat as "owned" so an emergency flatten-everything pass closes hedge
// legs in the SAME pass as their primaries, rather than waiting a cycle for
// runHedgeCoherenceSync. Only live hedge-enabled strategies contribute.
func hedgeCoinsForKillSwitch(hlLiveAll []StrategyConfig) map[string]bool {
	coins := make(map[string]bool)
	for _, sc := range hlLiveAll {
		if !hedgeEnabled(sc) {
			continue
		}
		if hc := hyperliquidHedgeCoin(sc); hc != "" {
			coins[hc] = true
		}
	}
	return coins
}

// applyHedgeKillSwitchCloseFill books a strategy's hedge-coin fill from the
// portfolio kill-switch close pass (#1159). Hedge coins are collision-free by
// validation (hyperliquidHedgeStrategyErrors), so — unlike the primary-coin
// path — there is never a peer to split the fill with; the owning strategy
// claims it in full.
func applyHedgeKillSwitchCloseFill(s *StrategyState, sc StrategyConfig, fills map[string]HyperliquidCloseFill) bool {
	if s == nil || !hedgeEnabled(sc) || !hyperliquidIsLive(sc.Args) {
		return false
	}
	hedgeSym := hyperliquidHedgeCoin(sc)
	if hedgeSym == "" {
		return false
	}
	fill, ok := fills[hedgeSym]
	if !ok || fill.TotalSz <= 1e-15 || fill.AvgPx <= 0 {
		return false
	}
	if oidStr := strconv.FormatInt(fill.OID, 10); fill.OID > 0 && strategyHasCloseTradeForOID(s, oidStr) {
		return false
	}
	return bookHedgeCloseFill(s, hedgeSym, fill.TotalSz, fill.AvgPx, fill.Fee, fill.OID, "hedge_kill_switch")
}

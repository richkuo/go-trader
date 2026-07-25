package main

import (
	"fmt"
	"math"
	"sort"
	"strings"
	"sync"
	"time"
)

// Per-strategy correlated hedge legs (#1159, phase 1 — Hyperliquid perps).
//
// DESIGN: hedge management is a per-cycle, STATE-DERIVED reconciler, not a set
// of per-event mirror hooks. hedgeTargetDecision is a pure function of
// (current primary position, current hedge position, persisted qty watermark);
// runHedgeSync converges the on-chain hedge to whatever that function returns.
// Every primary lifecycle event therefore mirrors automatically — fresh open,
// scale-in add, evaluator partial/full close, on-chain SL/TP fill booked by
// reconcile, ratchet close, kill switch, circuit breaker, manual force-close,
// external close, and a crash between the two legs. There is exactly one place
// where a hedge order is decided and exactly one place where a hedge fill is
// booked.
//
// SAFETY INVARIANTS (all load-bearing; see docs/ARCHITECTURE.md § Hedge legs):
//
//  1. Qty-event mirroring, never price mirroring. The target keys on
//     Position.HedgePrimaryQtyBasis, so mark drift never re-trades the hedge.
//  2. Fill-confirmed state mutation only. A hedge order that fails mutates no
//     virtual state, exactly like runHyperliquidExecuteOrder's ok2=false
//     contract. The next cycle's decision re-derives from unchanged state, so
//     "state disagrees with target" IS the retry mechanism — no pending table.
//  3. Fail closed on an unhedged primary. If the hedge cannot be opened or
//     grown, the UNHEDGED SLICE of the primary is immediately closed
//     reduce-only (the whole position when no hedge exists, only the add delta
//     when one does) and the owner is DM'd CRITICAL. Never run unhedged.
//  4. Sole ownership by construction. validateHedgeConfigs rejects any hedge
//     coin that collides with a configured strategy coin or another hedge coin,
//     so every hedge on-chain operation is a sole-owner operation. Closes are
//     still SIZED (never market_close(sz=None)) so a foreign position that
//     appeared on the hedge coin outside the config is never liquidated.
//  5. Hedge legs are not alpha. They carry no SL/TP, never enter protection
//     sync / the trailing walker / the ratchet / any close evaluator / the
//     regime store / trade diagnostics, and are excluded from #T and W-L stats.
//     Their PnL and fees DO book to the owning strategy's ledger.

const (
	// HedgeSideInverse is the only hedge side vocabulary accepted in phase 1:
	// a long primary opens a short hedge and vice versa.
	HedgeSideInverse = "inverse"
	// HedgeDefaultRatio is the hedge/primary notional multiplier used when
	// hedge.ratio is omitted.
	HedgeDefaultRatio = 1.0
	// HedgeMaxRatio bounds hedge.ratio. A hedge larger than 10x the primary
	// notional is a configuration error, not a hedge.
	HedgeMaxRatio = 10.0
	// HedgeMaxLeverage bounds hedge.leverage with the same sanity ceiling the
	// primary leverage validation uses.
	HedgeMaxLeverage = 50.0
	// hedgeMinOrderNotionalUSD is Hyperliquid's minimum order notional. A
	// REDUCE smaller than this is deferred (the basis is deliberately NOT
	// advanced) so the shortfall accumulates into a fillable order instead of
	// spamming rejects. A full close is never deferred.
	hedgeMinOrderNotionalUSD = 10.0
	// hedgeQtyRelTolerance is the relative band around the qty watermark
	// inside which the hedge is considered converged. Absorbs HL szDecimals
	// rounding without letting a real partial close hide.
	hedgeQtyRelTolerance = 1e-6
	// hedgeQtyAbsTolerance floors the relative band for tiny quantities.
	hedgeQtyAbsTolerance = 1e-9
	// hedgeOpenFailureHoldThreshold is how many consecutive hedge failures that
	// forced a primary unwind a strategy may accumulate before NEW primary
	// entries are held. Without it, a persistently failing hedge venue turns
	// every signal into an open→unwind round trip that burns fees on both legs.
	hedgeOpenFailureHoldThreshold = 3
	// hedgeEntryHoldCooldown is how long that hold lasts before it lifts on its
	// own and grants a fresh retry budget.
	//
	// A hold with no reachable clear condition would be a bug, not a safety
	// feature: while entries are held no open is attempted, so a
	// clear-only-on-success rule can never fire when the strategy is flat —
	// recovery would need a process restart, which is not something an
	// auto-protective mechanism may silently require. Expiring on a timer keeps
	// the brake honest: worst case is one bounded retry episode
	// (≤ hedgeOpenFailureHoldThreshold open→unwind round trips, each alerted)
	// per cooldown window, and a venue that recovers resumes trading unattended.
	hedgeEntryHoldCooldown = time.Hour
)

// HedgeEnabled reports whether the strategy runs an auto-managed hedge leg.
// Read through this accessor, never sc.Hedge.Enabled directly.
func HedgeEnabled(sc StrategyConfig) bool {
	return sc.Hedge != nil && sc.Hedge.Enabled && hedgeCoin(sc) != ""
}

// hedgeConfigured reports whether a hedge block is present at all (enabled or
// not). Used by validation, which must check the shape of a parked block.
func hedgeConfigured(sc StrategyConfig) bool { return sc.Hedge != nil }

// hedgeCoin normalizes hedge.symbol to an HL coin ticker. Accepts "BTC" and
// the ccxt "BTC/USDC:USDC" form; mirrors hyperliquidConfiguredCoin's
// upper/trim so collision detection survives operator casing typos.
func hedgeCoin(sc StrategyConfig) string {
	if sc.Hedge == nil {
		return ""
	}
	return normalizeHedgeCoin(sc.Hedge.Symbol)
}

func normalizeHedgeCoin(raw string) string {
	s := strings.TrimSpace(raw)
	if i := strings.Index(s, "/"); i >= 0 {
		s = s[:i]
	}
	if i := strings.Index(s, ":"); i >= 0 {
		s = s[:i]
	}
	return strings.ToUpper(strings.TrimSpace(s))
}

// hedgeRatio returns the effective hedge/primary notional multiplier.
func hedgeRatio(sc StrategyConfig) float64 {
	if sc.Hedge == nil || sc.Hedge.Ratio <= 0 {
		return HedgeDefaultRatio
	}
	return sc.Hedge.Ratio
}

// hedgeExchangeLeverage returns the hedge coin's own exchange leverage.
func hedgeExchangeLeverage(sc StrategyConfig) float64 {
	if sc.Hedge == nil || sc.Hedge.Leverage <= 0 {
		return 1
	}
	return sc.Hedge.Leverage
}

// hedgeMarginMode returns the hedge coin's own margin mode, defaulting to
// isolated so a hedge never silently shares cross-margin liquidation risk with
// the rest of the wallet.
func hedgeMarginMode(sc StrategyConfig) string {
	if sc.Hedge == nil || strings.TrimSpace(sc.Hedge.MarginMode) == "" {
		return "isolated"
	}
	return strings.ToLower(strings.TrimSpace(sc.Hedge.MarginMode))
}

// hedgeInverseSide maps a primary position side to the hedge position side.
func hedgeInverseSide(primarySide string) string {
	switch primarySide {
	case "long":
		return "short"
	case "short":
		return "long"
	}
	return ""
}

// hedgeOrderSideForPositionSide maps a desired hedge POSITION side to the
// order side used to open it.
func hedgeOrderSideForPositionSide(posSide string) string {
	if posSide == "short" {
		return "sell"
	}
	return "buy"
}

// hedgePositionOf returns the strategy's hedge leg, identified by the
// persisted HedgeFor stamp rather than by config lookup, plus its coin.
// Returns (nil, "") when the strategy holds no hedge leg. Deterministic under
// Go's randomized map iteration: coins are sorted before the scan.
func hedgePositionOf(s *StrategyState) (*Position, string) {
	if s == nil {
		return nil, ""
	}
	coins := make([]string, 0, len(s.Positions))
	for sym := range s.Positions {
		coins = append(coins, sym)
	}
	sort.Strings(coins)
	for _, sym := range coins {
		if pos := s.Positions[sym]; pos != nil && pos.HedgeFor != "" {
			return pos, sym
		}
	}
	return nil, ""
}

// ---------------------------------------------------------------------------
// Validation
// ---------------------------------------------------------------------------

// validateHedgeConfigs returns every hedge-related load error for a config,
// sorted for deterministic operator output. Wired into validateConfig and
// re-run on SIGHUP so a reload cannot introduce a collision startup would have
// rejected.
func validateHedgeConfigs(strategies []StrategyConfig) []string {
	var errs []string

	// Coin membership of every configured HL strategy — perps AND manual, live
	// AND paper. Paper peers still collide with a live hedge because the wallet
	// coin is shared on-chain the moment either goes live, and an operator
	// flipping --mode is a one-word edit. Erring strict here is what keeps every
	// shared-coin mechanism (peer detection, margin compatibility, CB drain,
	// kill-switch fill share, reconcile attribution) correct without change.
	primaryCoinOwners := make(map[string][]string)
	for _, sc := range strategies {
		if coin := hyperliquidConfiguredCoin(sc); coin != "" {
			primaryCoinOwners[coin] = append(primaryCoinOwners[coin], sc.ID)
		}
	}
	hedgeCoinOwners := make(map[string][]string)

	for _, sc := range strategies {
		if !hedgeConfigured(sc) {
			continue
		}
		prefix := fmt.Sprintf("strategy[%s].hedge", sc.ID)
		h := sc.Hedge

		if sc.Platform != "hyperliquid" || sc.Type != "perps" {
			errs = append(errs, fmt.Sprintf(
				"%s: correlated hedge legs are Hyperliquid perps only in phase 1 (#1159), got platform=%q type=%q",
				prefix, sc.Platform, sc.Type))
			// Everything below assumes the HL perps surface; keep reporting
			// shape errors anyway so one reload surfaces every problem.
		}
		if p := strings.ToLower(strings.TrimSpace(h.Platform)); p != "" && p != "hyperliquid" {
			errs = append(errs, fmt.Sprintf("%s.platform must be \"hyperliquid\" or omitted, got %q", prefix, h.Platform))
		}
		if t := strings.ToLower(strings.TrimSpace(h.Type)); t != "" && t != "perps" {
			errs = append(errs, fmt.Sprintf("%s.type must be \"perps\" or omitted, got %q", prefix, h.Type))
		}
		if s := strings.ToLower(strings.TrimSpace(h.Side)); s != "" && s != HedgeSideInverse {
			errs = append(errs, fmt.Sprintf("%s.side must be %q or omitted (phase 1 vocabulary), got %q", prefix, HedgeSideInverse, h.Side))
		}
		if h.Ratio < 0 || h.Ratio > HedgeMaxRatio {
			errs = append(errs, fmt.Sprintf("%s.ratio must be in (0, %g] (0/omitted defaults to %g), got %g", prefix, HedgeMaxRatio, HedgeDefaultRatio, h.Ratio))
		}
		if m := strings.ToLower(strings.TrimSpace(h.MarginMode)); m != "" && m != "isolated" && m != "cross" {
			errs = append(errs, fmt.Sprintf("%s.margin_mode must be \"isolated\" or \"cross\", got %q", prefix, h.MarginMode))
		}
		if h.Leverage < 0 || h.Leverage > HedgeMaxLeverage {
			errs = append(errs, fmt.Sprintf("%s.leverage must be in (0, %g] (0/omitted defaults to 1), got %g", prefix, HedgeMaxLeverage, h.Leverage))
		}

		coin := hedgeCoin(sc)
		if coin == "" {
			errs = append(errs, fmt.Sprintf("%s.symbol is empty or unparseable (expected an HL coin ticker like \"BTC\" or \"BTC/USDC:USDC\"), got %q", prefix, h.Symbol))
			continue
		}

		// #1159 constraint 2 — collision matrix. Each arm is reported
		// separately so the operator sees every conflict in one pass.
		if own := hyperliquidConfiguredCoin(sc); own != "" && own == coin {
			errs = append(errs, fmt.Sprintf(
				"%s.symbol %q equals the strategy's own primary coin — a same-coin hedge just nets the position on-chain (HL aggregates per coin per account) and hedges nothing",
				prefix, coin))
		}
		if owners := primaryCoinOwners[coin]; len(owners) > 0 {
			ids := append([]string(nil), owners...)
			sort.Strings(ids)
			errs = append(errs, fmt.Sprintf(
				"%s.symbol %q is the configured trading coin of strategy/strategies %s — HL aggregates one position per coin per account, so the hedge would share an on-chain position, margin assignment, and reduce-only order slots with them (phase 1 rejects overlap; #1159)",
				prefix, coin, strings.Join(ids, ", ")))
		}
		hedgeCoinOwners[coin] = append(hedgeCoinOwners[coin], sc.ID)
	}

	// Hedge-vs-hedge collisions, reported once per coin.
	coins := make([]string, 0, len(hedgeCoinOwners))
	for coin := range hedgeCoinOwners {
		coins = append(coins, coin)
	}
	sort.Strings(coins)
	for _, coin := range coins {
		owners := hedgeCoinOwners[coin]
		if len(owners) < 2 {
			continue
		}
		ids := append([]string(nil), owners...)
		sort.Strings(ids)
		errs = append(errs, fmt.Sprintf(
			"hedge symbol %q is declared by more than one strategy (%s) — two hedge legs on one coin share an on-chain position, margin assignment, and reduce-only order slots (phase 1 rejects overlap; #1159)",
			coin, strings.Join(ids, ", ")))
	}

	sort.Strings(errs)
	return errs
}

// hedgeConfigEqual reports whether two hedge blocks are identical for
// hot-reload purposes. nil vs non-nil is a change (mirrors scaleInConfigEqual).
func hedgeConfigEqual(a, b *HedgeConfig) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

func cloneHedgeConfig(h *HedgeConfig) *HedgeConfig {
	if h == nil {
		return nil
	}
	clone := *h
	return &clone
}

// ---------------------------------------------------------------------------
// Pure decision core
// ---------------------------------------------------------------------------

type hedgeActionKind int

const (
	hedgeActionNone hedgeActionKind = iota
	hedgeActionOpen
	hedgeActionAdd
	hedgeActionReduce
	hedgeActionCloseFull
)

func (k hedgeActionKind) String() string {
	switch k {
	case hedgeActionOpen:
		return "open"
	case hedgeActionAdd:
		return "add"
	case hedgeActionReduce:
		return "reduce"
	case hedgeActionCloseFull:
		return "close"
	}
	return "none"
}

// hedgeIncreasesExposure reports whether the action grows the hedge leg. Used
// only for order-side labelling and for deciding whether a SUCCESS clears the
// consecutive-failure hold.
//
// It is deliberately NOT the escalation predicate: whether a FAILURE leaves the
// primary exposed is a property of the specific decision, not of its kind — a
// side-mismatch closeFull leaves a same-direction residual leg that AMPLIFIES
// exposure even though it does not grow the hedge. See
// hedgeAction.FailureLeavesPrimaryExposed.
func (k hedgeActionKind) hedgeIncreasesExposure() bool {
	return k == hedgeActionOpen || k == hedgeActionAdd
}

// hedgeSnapshot is the primary+hedge state captured under one RLock. Every
// decision is a pure function of this value plus the strategy's hedge config.
type hedgeSnapshot struct {
	PrimarySymbol string
	PrimaryQty    float64
	PrimarySide   string
	PrimaryPx     float64 // current mark; 0 = unavailable
	HedgeSymbol   string
	HedgeQty      float64
	HedgeSide     string
	HedgeBasis    float64 // Position.HedgePrimaryQtyBasis on the hedge leg
	HedgePx       float64 // current mark; 0 = unavailable
	// HedgeStaleReason is non-empty when a hedge leg is held whose config no
	// longer authorizes it (hedge removed/disabled, or the configured coin
	// changed) — a config edit plus process restart bypasses the SIGHUP guard.
	// The decision unwinds it deterministically instead of stranding it.
	HedgeStaleReason string
}

// hedgeAction is what the reconciler will do this cycle.
type hedgeAction struct {
	Kind hedgeActionKind
	// Qty is the hedge-coin quantity to trade (always positive).
	Qty float64
	// PositionSide is the hedge position side an open/add establishes or
	// extends ("long"/"short"); empty for reduce/close.
	PositionSide string
	// NewBasis is the primary quantity the hedge will be sized against once
	// this action fills completely. Partial fills advance the basis
	// proportionally (see applyHedgeFill).
	NewBasis float64
	// Reason is an operator-facing explanation, always populated.
	Reason string
	// Blocked marks a decision that WANTED to act but could not (no usable
	// mark, dust-sized reduce). Callers alert rather than silently no-op; a
	// blocked increase on an unhedged primary escalates to the unwind.
	Blocked bool
	// FailureLeavesPrimaryExposed is THE escalation predicate: true when this
	// action failing (or being blocked) leaves primary exposure that no
	// correctly-sided hedge covers, so the fail-closed unwind must run.
	//
	// It is a property of the decision, not of the action kind, because the two
	// do not coincide:
	//
	//   - open / add fail → the uncovered slice is real exposure → true.
	//   - side-mismatch closeFull fails → the surviving hedge leg is now the
	//     SAME direction as the flipped primary, so net correlated exposure is
	//     roughly DOUBLED, not reduced → true. (Treating this as a benign
	//     over-hedge would leave a strategy running at double the intended risk
	//     with no unwind — the exact hole this field closes.)
	//   - proportional reduce fails → the hedge is over-sized but still
	//     INVERSE, so net exposure is smaller than intended → false, retry.
	//   - closeFull with the primary already flat fails → there is no primary
	//     left to unwind, so the reduce-only retry is the only action → false.
	FailureLeavesPrimaryExposed bool
	// ReopenAfterClose marks a close that is only HALF of a convergence: the
	// decision emits at most one action, so replacing a hedge (a primary side
	// flip, or a changed hedge.symbol) has to unwind the stale leg before the
	// correct one can be opened. Set here, runHedgeSync immediately re-derives
	// and performs that open in the SAME cycle — otherwise the primary would
	// run with no correctly-sided hedge for a full strategy interval, which is
	// precisely the state this feature exists to prevent.
	//
	// Deliberately narrow: it does NOT fire after an ordinary open/add/reduce.
	// Looping on those would re-issue an order every time a venue partially
	// filled, tripling order count on a thin book to chase a gap the next cycle
	// handles for free.
	ReopenAfterClose bool
	// HedgedPrimaryQtyOnFailure is how much of the primary a surviving hedge
	// leg still covers if this action fails — the watermark the unwind
	// subtracts, so only the UNCOVERED slice is closed. 0 means "nothing is
	// covered", which unwinds the whole primary.
	//
	// Passed explicitly rather than re-derived from the snapshot because the
	// snapshot's basis is misleading exactly where it matters most: after a
	// flip the stale basis still reads as the pre-flip primary quantity even
	// though the residual leg hedges none of the new side.
	HedgedPrimaryQtyOnFailure float64
}

// hedgeConverged reports whether primary qty is within tolerance of the basis.
func hedgeConverged(primaryQty, basis float64) bool {
	tol := math.Abs(basis) * hedgeQtyRelTolerance
	if tol < hedgeQtyAbsTolerance {
		tol = hedgeQtyAbsTolerance
	}
	return math.Abs(primaryQty-basis) <= tol
}

// hedgeTargetDecision is the single source of truth for what the hedge leg
// should do. Pure: no locks, no I/O, no clock. `enabled` and `ratio` come from
// the strategy config; everything else from the snapshot.
//
// Ordering matters — each branch below is reachable only because the ones
// above it are not:
//
//	stale config  → close the hedge (config no longer authorizes it)
//	disabled      → nothing (a disabled hedge with no leg is the common case)
//	primary flat  → close the hedge
//	no hedge leg  → open, sized to the FULL primary notional × ratio
//	side mismatch → close (the next cycle re-opens on the correct side)
//	qty grew      → add, sized to the DELTA notional × ratio
//	qty shrank    → reduce proportionally, or close when the primary is ~gone
//	otherwise     → converged
func hedgeTargetDecision(enabled bool, ratio float64, snap hedgeSnapshot) hedgeAction {
	if ratio <= 0 {
		ratio = HedgeDefaultRatio
	}
	hasHedge := snap.HedgeQty > 0

	if snap.HedgeStaleReason != "" {
		if !hasHedge {
			return hedgeAction{Kind: hedgeActionNone, Reason: snap.HedgeStaleReason}
		}
		// A symbol change (hedge still enabled) is the same unwind-then-reopen
		// shape as a flip; a removed/disabled block has nothing to reopen.
		return hedgeAction{Kind: hedgeActionCloseFull, Qty: snap.HedgeQty,
			ReopenAfterClose: enabled, Reason: snap.HedgeStaleReason}
	}
	if !enabled {
		return hedgeAction{Kind: hedgeActionNone, Reason: "hedge disabled"}
	}
	if snap.PrimaryQty <= 0 {
		if !hasHedge {
			return hedgeAction{Kind: hedgeActionNone, Reason: "primary flat, hedge flat"}
		}
		return hedgeAction{Kind: hedgeActionCloseFull, Qty: snap.HedgeQty, Reason: "primary position is flat"}
	}

	wantSide := hedgeInverseSide(snap.PrimarySide)
	if wantSide == "" {
		// A primary with an unrecognized side is a corrupt row; refuse to trade
		// against it rather than guessing a hedge direction.
		// Alert-only, deliberately NOT escalated: a garbage side is a data
		// problem, and "which way is this position facing" is exactly what an
		// unwind decision would need to be correct about. Surface it for the
		// operator rather than trading off an unreadable row.
		return hedgeAction{Kind: hedgeActionNone, Blocked: true,
			Reason: fmt.Sprintf("primary %s has unrecognized side %q — refusing to derive a hedge side", snap.PrimarySymbol, snap.PrimarySide)}
	}

	if !hasHedge {
		qty, ok := hedgeQtyForNotional(snap.PrimaryQty, snap.PrimaryPx, ratio, snap.HedgePx)
		if !ok {
			return hedgeAction{Kind: hedgeActionNone, Blocked: true,
				FailureLeavesPrimaryExposed: true, HedgedPrimaryQtyOnFailure: 0,
				Reason: fmt.Sprintf("cannot size hedge open: primary_px=%.6f hedge_px=%.6f (need positive marks for %s and %s)",
					snap.PrimaryPx, snap.HedgePx, snap.PrimarySymbol, snap.HedgeSymbol)}
		}
		return hedgeAction{Kind: hedgeActionOpen, Qty: qty, PositionSide: wantSide, NewBasis: snap.PrimaryQty,
			FailureLeavesPrimaryExposed: true, HedgedPrimaryQtyOnFailure: 0,
			Reason: fmt.Sprintf("open %s hedge for %.6f %s primary", wantSide, snap.PrimaryQty, snap.PrimarySymbol)}
	}

	if snap.HedgeSide != wantSide {
		// The primary flipped side (direction="both") without a hedge close in
		// between. Unwinding the stale leg first is the only safe convergence —
		// a same-coin opposite order would net on-chain, not flip cleanly.
		//
		// FailureLeavesPrimaryExposed is TRUE here even though this shrinks the
		// hedge: until the stale leg is gone it sits on the SAME side as the
		// flipped primary, so the pair is not hedged at all — it is roughly
		// double exposure on two correlated coins. HedgedPrimaryQtyOnFailure is
		// 0 because the residual leg covers none of the new primary side,
		// regardless of what the stale basis says.
		return hedgeAction{Kind: hedgeActionCloseFull, Qty: snap.HedgeQty,
			FailureLeavesPrimaryExposed: true, HedgedPrimaryQtyOnFailure: 0,
			ReopenAfterClose: true,
			Reason:           fmt.Sprintf("hedge side %q is no longer inverse of primary side %q — the pair is AMPLIFYING exposure until this leg is unwound", snap.HedgeSide, snap.PrimarySide)}
	}

	basis := snap.HedgeBasis
	if basis <= 0 {
		// Legacy/repaired hedge row with no watermark: adopt the current
		// primary qty as the basis rather than treating the whole position as
		// an un-hedged delta and doubling the hedge.
		basis = snap.PrimaryQty
	}
	if hedgeConverged(snap.PrimaryQty, basis) {
		return hedgeAction{Kind: hedgeActionNone, Reason: "hedge converged"}
	}

	if snap.PrimaryQty > basis {
		delta := snap.PrimaryQty - basis
		qty, ok := hedgeQtyForNotional(delta, snap.PrimaryPx, ratio, snap.HedgePx)
		if !ok {
			return hedgeAction{Kind: hedgeActionNone, Blocked: true,
				FailureLeavesPrimaryExposed: true, HedgedPrimaryQtyOnFailure: basis,
				Reason: fmt.Sprintf("cannot size hedge add: primary_px=%.6f hedge_px=%.6f", snap.PrimaryPx, snap.HedgePx)}
		}
		// A failed add leaves only the DELTA uncovered — the existing inverse
		// leg still hedges `basis`, so the unwind must scope to the add.
		return hedgeAction{Kind: hedgeActionAdd, Qty: qty, PositionSide: wantSide, NewBasis: snap.PrimaryQty,
			FailureLeavesPrimaryExposed: true, HedgedPrimaryQtyOnFailure: basis,
			Reason: fmt.Sprintf("primary grew %.6f → %.6f %s", basis, snap.PrimaryQty, snap.PrimarySymbol)}
	}

	// Primary shrank. Reduce the hedge by the same FRACTION of the basis the
	// primary lost, so the hedge/primary notional ratio established at each
	// entry leg is preserved without re-pricing.
	frac := (basis - snap.PrimaryQty) / basis
	if frac >= 1 {
		return hedgeAction{Kind: hedgeActionCloseFull, Qty: snap.HedgeQty, NewBasis: 0,
			Reason: fmt.Sprintf("primary fully closed (%.6f → %.6f %s)", basis, snap.PrimaryQty, snap.PrimarySymbol)}
	}
	qty := snap.HedgeQty * frac
	if qty >= snap.HedgeQty {
		qty = snap.HedgeQty
	}
	if qty <= 0 {
		return hedgeAction{Kind: hedgeActionNone, Reason: "hedge reduce rounds to zero"}
	}
	if snap.HedgePx > 0 && qty*snap.HedgePx < hedgeMinOrderNotionalUSD {
		// Deliberately do NOT advance the basis: the shortfall accumulates so a
		// later reduction clears both at once, instead of spamming the exchange
		// with sub-minimum orders that always reject.
		return hedgeAction{Kind: hedgeActionNone, Blocked: true,
			Reason: fmt.Sprintf("hedge reduce of %.6f %s (~$%.2f) is below the $%.2f exchange minimum — deferring, basis not advanced",
				qty, snap.HedgeSymbol, qty*snap.HedgePx, hedgeMinOrderNotionalUSD)}
	}
	return hedgeAction{Kind: hedgeActionReduce, Qty: qty, NewBasis: snap.PrimaryQty,
		Reason: fmt.Sprintf("primary shrank %.6f → %.6f %s (reduce hedge %.2f%%)", basis, snap.PrimaryQty, snap.PrimarySymbol, frac*100)}
}

// hedgeQtyForNotional converts a primary quantity delta into a hedge quantity
// at the configured notional ratio. ok=false when either mark is unusable or
// the result is non-positive — callers must fail closed, never fall back to a
// guessed size.
func hedgeQtyForNotional(primaryQtyDelta, primaryPx, ratio, hedgePx float64) (float64, bool) {
	if primaryQtyDelta <= 0 || primaryPx <= 0 || hedgePx <= 0 || ratio <= 0 {
		return 0, false
	}
	qty := (primaryQtyDelta * primaryPx * ratio) / hedgePx
	if qty <= 0 || math.IsNaN(qty) || math.IsInf(qty, 0) {
		return 0, false
	}
	return qty, true
}

// hedgeOrderSkipReason re-checks a decision against a FRESH snapshot taken
// immediately before the order is spawned (CLAUDE.md skip-reason mirror rule).
// Without it, state that moved between the Phase-1 snapshot and the Phase-3
// spawn could produce an on-chain fill with no matching Trade record — the
// #298 class of bug. Returns "" when the order may proceed.
func hedgeOrderSkipReason(action hedgeAction, fresh hedgeSnapshot) string {
	switch action.Kind {
	case hedgeActionNone:
		return "no hedge action"
	case hedgeActionOpen:
		if fresh.HedgeQty > 0 {
			return fmt.Sprintf("hedge leg on %s already exists (%.6f %s) — open no longer applies", fresh.HedgeSymbol, fresh.HedgeQty, fresh.HedgeSide)
		}
		if fresh.PrimaryQty <= 0 {
			return fmt.Sprintf("primary %s is flat — hedge open no longer applies", fresh.PrimarySymbol)
		}
		if hedgeInverseSide(fresh.PrimarySide) != action.PositionSide {
			return fmt.Sprintf("primary side changed to %q — hedge open side %q is stale", fresh.PrimarySide, action.PositionSide)
		}
	case hedgeActionAdd:
		if fresh.HedgeQty <= 0 {
			return fmt.Sprintf("hedge leg on %s vanished — add no longer applies", fresh.HedgeSymbol)
		}
		if fresh.HedgeSide != action.PositionSide {
			return fmt.Sprintf("hedge side changed to %q — add on %q is stale", fresh.HedgeSide, action.PositionSide)
		}
		if fresh.PrimaryQty <= 0 {
			return fmt.Sprintf("primary %s is flat — hedge add no longer applies", fresh.PrimarySymbol)
		}
	case hedgeActionReduce, hedgeActionCloseFull:
		if fresh.HedgeQty <= 0 {
			return fmt.Sprintf("hedge leg on %s is already flat", fresh.HedgeSymbol)
		}
		if action.Qty > fresh.HedgeQty {
			// Not a skip: the applier clamps. Reported for the log only.
			return ""
		}
	}
	return ""
}

// ---------------------------------------------------------------------------
// Operator surfaces
// ---------------------------------------------------------------------------

// hedgeSummaryTag renders the hedge configuration as a compact audit tag, e.g.
// `hedge=BTC×1.00(inverse,cross,3x)`. Empty when no hedge is configured;
// `hedge=BTC(disabled)` for a parked block, so a config an operator believes is
// hedging never looks identical to one that is not.
func hedgeSummaryTag(sc StrategyConfig) string {
	if !hedgeConfigured(sc) {
		return ""
	}
	coin := hedgeCoin(sc)
	if coin == "" {
		coin = "?"
	}
	if !HedgeEnabled(sc) {
		return fmt.Sprintf("hedge=%s(disabled)", coin)
	}
	return fmt.Sprintf("hedge=%s×%.2f(%s,%s,%gx)", coin, hedgeRatio(sc), HedgeSideInverse, hedgeMarginMode(sc), hedgeExchangeLeverage(sc))
}

// hedgeStatusNote renders the live hedge state for /status-style surfaces. It
// deliberately reports the leg as "coupled to <primary>" rather than as a
// standalone position, so an operator scanning positions never mistakes an
// auto-managed hedge for unmanaged exposure (#1159 requirement 7). Returns ""
// when there is nothing hedge-related to say.
func hedgeStatusNote(sc StrategyConfig, s *StrategyState) string {
	pos, coin := hedgePositionOf(s)
	if pos == nil {
		if !HedgeEnabled(sc) {
			return ""
		}
		return fmt.Sprintf("hedge %s: flat (auto-managed, coupled to %s)", hedgeCoin(sc), hyperliquidConfiguredCoin(sc))
	}
	note := fmt.Sprintf("hedge %s: %s %.6f @ $%.4f (auto-managed, coupled to %s; basis %.6f)",
		coin, pos.Side, pos.Quantity, pos.AvgCost, pos.HedgeFor, pos.HedgePrimaryQtyBasis)
	if !HedgeEnabled(sc) {
		note += " — CONFIG NO LONGER DECLARES THIS HEDGE; it will be unwound"
	}
	return note
}

// HedgeStatus is the serialized view of a strategy's correlated hedge leg for
// /status, the dashboard, and `inspect --json` (#1159 requirement 7).
type HedgeStatus struct {
	Enabled    bool    `json:"enabled"`
	Symbol     string  `json:"symbol"`
	Side       string  `json:"side"`
	Ratio      float64 `json:"ratio"`
	MarginMode string  `json:"margin_mode"`
	Leverage   float64 `json:"leverage"`
	// PrimarySymbol is the coin this hedge is coupled to.
	PrimarySymbol string `json:"primary_symbol,omitempty"`
	// Held* describe the live leg; zero values mean the hedge is flat.
	HeldQty      float64 `json:"held_qty,omitempty"`
	HeldSide     string  `json:"held_side,omitempty"`
	HeldAvgCost  float64 `json:"held_avg_cost,omitempty"`
	PrimaryBasis float64 `json:"primary_qty_basis,omitempty"`
	// EntryHold is true when consecutive hedge failures are currently holding
	// new entries for this strategy.
	EntryHold bool `json:"entry_hold,omitempty"`
	// StaleConfig flags a held leg the current config no longer authorizes —
	// the reconciler is about to unwind it.
	StaleConfig bool `json:"stale_config,omitempty"`
}

// hedgeStatusFor builds the serialized hedge view, or nil when the strategy
// neither declares nor holds a hedge leg.
func hedgeStatusFor(sc StrategyConfig, s *StrategyState) *HedgeStatus {
	pos, coin := hedgePositionOf(s)
	if !hedgeConfigured(sc) && pos == nil {
		return nil
	}
	out := &HedgeStatus{
		Enabled:       HedgeEnabled(sc),
		Symbol:        hedgeCoin(sc),
		Side:          HedgeSideInverse,
		Ratio:         hedgeRatio(sc),
		MarginMode:    hedgeMarginMode(sc),
		Leverage:      hedgeExchangeLeverage(sc),
		PrimarySymbol: hyperliquidConfiguredCoin(sc),
		EntryHold:     hedgeEntryHoldActive(sc),
	}
	if pos != nil {
		out.HeldQty = pos.Quantity
		out.HeldSide = pos.Side
		out.HeldAvgCost = pos.AvgCost
		out.PrimaryBasis = pos.HedgePrimaryQtyBasis
		out.StaleConfig = !HedgeEnabled(sc) || hedgeCoin(sc) != coin
		if out.Symbol == "" {
			out.Symbol = coin
		}
	}
	return out
}

// hedgeStrategiesNote renders every hedge-relevant strategy as a labelled
// /status footer, sorted by strategy ID (Go map iteration is randomized and
// this is operator-facing output). Empty string when nothing is hedged.
func hedgeStrategiesNote(strategies []StrategyConfig, byID map[string]*StrategyState) string {
	var lines []string
	ordered := append([]StrategyConfig(nil), strategies...)
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].ID < ordered[j].ID })
	for _, sc := range ordered {
		if sc.Platform != "hyperliquid" || sc.Type != "perps" {
			continue
		}
		note := hedgeStatusNote(sc, byID[sc.ID])
		if note == "" {
			continue
		}
		lines = append(lines, fmt.Sprintf("[%s] %s", sc.ID, note))
	}
	if len(lines) == 0 {
		return ""
	}
	return "\n⇄ " + strings.Join(lines, "\n⇄ ")
}

// ---------------------------------------------------------------------------
// Consecutive-failure entry hold
// ---------------------------------------------------------------------------

// hedgeFailureTracker counts consecutive hedge failures that forced a primary
// unwind, per strategy, so a persistently failing hedge venue cannot turn every
// signal into an open→unwind round trip.
//
// Deliberately in-memory: this is a fee-churn brake, not a safety latch — the
// fail-closed unwind is the safety, and it runs on every failure regardless of
// this counter. The hold is TIME-BOUNDED (hedgeEntryHoldCooldown) rather than
// cleared only on success, because while entries are held no open is attempted,
// so a success-only clear is unreachable whenever the strategy is flat. A
// successful hedge increase still clears it immediately, so a venue that
// recovers mid-position resumes without waiting out the cooldown.
type hedgeFailureTracker struct {
	mu      sync.Mutex
	counts  map[string]int
	warned  map[string]bool
	holdEnd map[string]time.Time
	// now is injectable so tests can advance the clock without sleeping.
	now func() time.Time
}

func newHedgeFailureTracker() *hedgeFailureTracker {
	return &hedgeFailureTracker{
		counts:  map[string]int{},
		warned:  map[string]bool{},
		holdEnd: map[string]time.Time{},
	}
}

var globalHedgeFailures = newHedgeFailureTracker()

func (t *hedgeFailureTracker) clock() time.Time {
	if t.now != nil {
		return t.now()
	}
	return time.Now()
}

// expireLocked drops a strategy's state once its hold window has passed, so the
// next episode starts from a clean budget and re-DMs. Caller holds t.mu.
func (t *hedgeFailureTracker) expireLocked(id string) {
	end, held := t.holdEnd[id]
	if !held || t.clock().Before(end) {
		return
	}
	delete(t.counts, id)
	delete(t.warned, id)
	delete(t.holdEnd, id)
}

// recordFailure increments the counter and reports the new count plus whether
// this crossing is the first to reach the hold threshold (so the owner DM fires
// exactly once per episode, and again on the next episode after a cooldown).
func (t *hedgeFailureTracker) recordFailure(id string) (count int, firstHold bool) {
	if t == nil || id == "" {
		return 0, false
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.expireLocked(id)
	t.counts[id]++
	count = t.counts[id]
	if count >= hedgeOpenFailureHoldThreshold && !t.warned[id] {
		t.warned[id] = true
		t.holdEnd[id] = t.clock().Add(hedgeEntryHoldCooldown)
		firstHold = true
	}
	return count, firstHold
}

func (t *hedgeFailureTracker) clear(id string) {
	if t == nil || id == "" {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.counts, id)
	delete(t.warned, id)
	delete(t.holdEnd, id)
}

func (t *hedgeFailureTracker) count(id string) int {
	if t == nil || id == "" {
		return 0
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.expireLocked(id)
	return t.counts[id]
}

// holdActive reports whether the strategy is inside its cooldown window,
// expiring a lapsed hold on the way.
func (t *hedgeFailureTracker) holdActive(id string) bool {
	if t == nil || id == "" {
		return false
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.expireLocked(id)
	end, held := t.holdEnd[id]
	return held && t.clock().Before(end)
}

// hedgeEntryHoldActive reports whether new position-increasing signals must be
// held for this strategy because its hedge leg keeps failing. Shaped like
// pausedBlocksSignal's callers: reductions and management always pass.
func hedgeEntryHoldActive(sc StrategyConfig) bool {
	if !HedgeEnabled(sc) {
		return false
	}
	return globalHedgeFailures.holdActive(sc.ID)
}

// hedgeEntryHoldMessage is the operator DM for an engaged hold. It must state
// the ACTUAL recovery path — an auto-hold that describes a clear condition it
// blocks would leave the operator waiting for something that cannot happen.
func hedgeEntryHoldMessage(strategyID string, count int, hedgeSymbol string) string {
	return fmt.Sprintf(
		"**HEDGE ENTRY HOLD** [%s] %d consecutive hedge-leg failures on %s — new entries are held for %s, then the hold lifts automatically and a bounded retry is attempted. Existing positions keep managing and closing normally, and a hedge that succeeds in the meantime clears the hold immediately.",
		strategyID, count, hedgeSymbol, hedgeEntryHoldCooldown)
}

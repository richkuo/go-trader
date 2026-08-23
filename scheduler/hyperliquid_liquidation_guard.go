package main

import (
	"fmt"
	"math"
	"os"
	"sort"
	"strings"
	"sync"
	"time"
)

// #1450 — stop-loss triggers past the Hyperliquid liquidation price.
//
// The problem: nothing compared a configured or derived stop-loss trigger to
// the price at which Hyperliquid force-closes the position. A stop past that
// price is UNREACHABLE by construction — the exchange liquidates first, so the
// configured trigger can never fill, while the operator sees an armed
// positions.stop_loss_oid and believes the geometry is protecting them.
//
// Policy, in three parts:
//
//  1. CLAMP, never refuse, never alert-only. Refusing to arm would leave a live
//     position with no exchange-side stop, which contradicts the repo invariant
//     that every position keeps volatility-adjusted exchange-side protection
//     (config.go DefaultStopLossATRMult, the manual-ratchet fallback that
//     ignores 0). Alert-only preserves nothing: the operator still eats the
//     liquidation close, liquidation-engine pricing, and possible ADL. Clamping
//     the trigger to just INSIDE the exchange-reported liquidation price exits
//     slightly earlier at a better price through the reduce-only trigger
//     machinery that already exists.
//
//  2. ONE-WAY TIGHTEN. The clamp only ever moves a trigger in the favorable
//     direction (a long's stop up, a short's stop down). It can never widen a
//     stop, so a stale or drifting liquidation price cannot loosen protection.
//     A clamp is never reversed when liquidation later moves away.
//
//  3. CURRENT-CYCLE VALUE ONLY, never persisted. Consumers read
//     HLPosition.LiquidationPx from the Phase 1 clearinghouseState fetch.
//     Never derive a liquidation band from 1/leverage or 1/(2*maxLev): HL
//     maintenance margin is per-asset and retierable, so a derived band would
//     falsely reject valid geometry. LiquidationPx == 0 means "unknown" and
//     every check skips (fail-open for an advisory mechanism — existing stops
//     keep resting, protection is never removed).
//
// Per-owner healing matrix — every live stop owner reaches the clamp:
//
//   - trailing owners (trailing_stop_pct / _atr_mult / _atr_regime, ratchet):
//     the walker clamps its candidate trigger the same cycle.
//   - fixed / regime / unified ATR owners: buildHyperliquidProtectionPlan
//     clamps the derived slMult, and sets ForceSLReplace when the RESTING
//     trigger is past liquidation.
//   - static scalar owners (stop_loss_pct, stop_loss_margin_pct, the
//     max_drawdown_pct fallback): no re-place mechanism exists, so the
//     per-cycle audit below cancels and re-places them directly.
//
// runPostTPStopLossAdjustment needs no clamp: post-TP adjustments only move the
// SL toward profit, i.e. AWAY from liquidation. The audit backstops it anyway.
//
// Open-cycle blind spot, stated honestly: a stop armed on the SAME cycle as the
// open (inline scalar SL at order time, armTrailingStopAtOpenNow, manual open,
// post-fill protection sync) cannot be compared, because the Phase 1 snapshot
// predates the position and the exchange has not reported a liquidation price
// for it yet. The audit catches those on the next cycle.
//
// Scope: live Hyperliquid perps and manual only. Paper has no real account and
// no liquidation price. Hedge legs carry no SL by design, so they produce no
// candidates.

// hlLiquidationStopBufferPct is how far INSIDE the liquidation price a clamped
// trigger is placed, as a percentage of the liquidation price. It must be
// strictly positive: a trigger exactly AT liquidation is a coin-flip against
// the exchange engine.
const hlLiquidationStopBufferPct = 0.5

// stopPastLiquidation reports whether triggerPx sits at or beyond liqPx on the
// unreachable side for a position with the given side — i.e. the exchange would
// force-close before the stop could fill.
//
// Returns false when either price is non-positive: 0 means "unknown" (the
// exchange did not report a liquidation price, or nothing is armed) and an
// unknown must never drive a clamp.
func stopPastLiquidation(side string, triggerPx, liqPx float64) bool {
	if triggerPx <= 0 || liqPx <= 0 {
		return false
	}
	switch side {
	case "long":
		// A long's stop sits below entry; liquidation is below it or between.
		return triggerPx <= liqPx
	case "short":
		// A short's stop sits above entry.
		return triggerPx >= liqPx
	}
	return false
}

// clampStopInsideLiquidation moves a past-liquidation trigger to just inside
// the liquidation price, in the favorable (tightening) direction only.
//
// Returns (triggerPx, false) unchanged when the trigger is not past liquidation
// or either price is unknown. The returned trigger is NEVER 0 for a positive
// input trigger — a caller that placed 0 would cancel protection outright,
// which is exactly the failure mode this mechanism exists to prevent.
func clampStopInsideLiquidation(side string, triggerPx, liqPx float64) (float64, bool) {
	if !stopPastLiquidation(side, triggerPx, liqPx) {
		return triggerPx, false
	}
	var clamped float64
	switch side {
	case "long":
		clamped = liqPx * (1.0 + hlLiquidationStopBufferPct/100.0)
	case "short":
		clamped = liqPx * (1.0 - hlLiquidationStopBufferPct/100.0)
	default:
		return triggerPx, false
	}
	if clamped <= 0 || math.IsNaN(clamped) || math.IsInf(clamped, 0) {
		// Unreachable for liqPx > 0 and a 0.5% buffer, but a clamp that
		// produced a non-positive trigger would REMOVE protection. Refuse it
		// and leave the original trigger; the alert still fires.
		return triggerPx, false
	}
	return clamped, true
}

// hlLiquidationPxForSide returns this coin's exchange-reported liquidation
// price ONLY when the strategy's own recorded side matches the side of the
// on-chain NET position that price describes.
//
// Hyperliquid nets ONE position per coin, so the reported liquidation price
// prices the NET across every strategy holding the coin (#1456 review). When a
// strategy's own side is OPPOSITE to that net — a shared coin holding both a
// long and a short peer (bidirectional perps are legal), or a sole owner whose
// recorded side is stale — the net's liquidation price sits on the far side of
// the mark from that leg's geometry, so stopPastLiquidation would be
// unconditionally true and the clamp would CANCEL a healthy resting stop to
// place one the exchange rejects against the opposite net. A side that cannot
// be confirmed must behave exactly like liquidationPx == 0: unknown, skip,
// change nothing.
func hlLiquidationPxForSide(liqPxByCoin map[string]float64, netSideByCoin map[string]string, coin, side string) float64 {
	px := liqPxByCoin[coin]
	if px <= 0 {
		return 0
	}
	if netSideByCoin[coin] != side || (side != "long" && side != "short") {
		return 0
	}
	return px
}

// --- boot-time validation --------------------------------------------------

// hlBankruptcyStopBoundPct is the price-% adverse move at which an
// ISOLATED-margin position has lost its entire posted margin: 100 / leverage.
//
// This is the ONLY liquidation-related bound provable at boot without per-asset
// exchange data. Hyperliquid's maintenance margin is strictly positive for
// every asset, so liquidation always happens strictly INSIDE this distance —
// a configured stop at or beyond it can never fill.
//
// The per-asset maintenance-margin band (1/(2 x maxLeverage)) is deliberately
// NOT encoded: it needs a meta fetch, HL can retier it, and a hardcoded table
// would either go stale or falsely reject valid geometry. The runtime
// exchange-reported check is the authoritative band.
func hlBankruptcyStopBoundPct(leverage float64) float64 {
	if leverage < 1 {
		leverage = 1
	}
	return 100.0 / leverage
}

// validateHLStopWithinBankruptcyBound reports the percentage stop owners whose
// distance is provably impossible at boot. Returns unprefixed messages; the
// caller prepends its own strategy prefix.
//
// Scope, deliberately narrow:
//
//   - LIVE Hyperliquid perps only. Paper has no real account and no
//     liquidation price at all.
//   - ISOLATED margin only. A cross-margin liquidation can sit BEYOND
//     1/leverage because the whole account equity backs the position, so the
//     bound would falsely reject valid configurations. Empty margin_mode reads
//     as isolated, matching the runtime default.
//   - Percentage owners only (stop_loss_pct and the stop_loss_margin_pct /
//     leverage derivation). Every ATR-derived owner needs a per-position
//     EntryATR that does not exist until the position opens, so it can only be
//     checked at arm time by the runtime clamp.
//
// trailing_stop_pct is deliberately EXCLUDED (#1456 review): a trailing
// trigger is anchored on the high-water mark (`highWater * (1 - pct/100)`),
// which ratchets with the mark, so the geometry becomes reachable as soon as
// the position moves in favor — the entry-anchored bankruptcy distance is
// exact only for stops whose anchor is frozen for the life of the position,
// and the runtime clamp already handles the pre-move window. A boot-time
// rejection here would abort startup for configurations that work.
//
// The MaxDrawdownPct fallback IS covered: EffectiveStopLossPct resolves it as
// an entry-anchored stop distance whenever all seven explicit owners are
// absent, so it must satisfy the same bound as stop_loss_pct (#1456 review).
//
// With leverage 1 the bound is 100%, above the existing 50% caps, so every
// low-leverage configuration passes.
func validateHLStopWithinBankruptcyBound(sc StrategyConfig) []string {
	if sc.Platform != "hyperliquid" || sc.Type != "perps" {
		return nil
	}
	if !isLiveArgs(sc.Args) {
		return nil
	}
	if strings.EqualFold(strings.TrimSpace(sc.MarginMode), "cross") {
		return nil
	}
	lev := EffectiveExchangeLeverage(sc)
	bound := hlBankruptcyStopBoundPct(lev)
	if lev < 1 {
		lev = 1
	}
	var errs []string
	report := func(field string, pct float64) {
		if pct <= 0 || pct < bound {
			return
		}
		errs = append(errs, fmt.Sprintf("%s = %g%% is at or beyond the isolated-margin bankruptcy distance (100 / leverage = %g%% at leverage %g), so Hyperliquid would force-close the position before the stop could ever fill; lower the stop distance or lower the leverage", field, pct, bound, lev))
	}
	// The pct fields are scored ONLY when they actually resolve as the stop
	// distance (#1456 review round 6): EffectiveStopLossPct returns 0 under a
	// unified regime close (#841 — the close owns an ATR-based SL armed after
	// open), making both fields INERT there, and no mutual-exclusion rule
	// rejects the combination — so reporting them unconditionally turned a
	// do-nothing field into a fatal boot failure. The runtime clamp still owns
	// the unified close's derived trigger, which is the protection that
	// matters.
	if !strategyUsesUnifiedRegimeClose(sc) {
		if sc.StopLossPct != nil {
			report("stop_loss_pct", *sc.StopLossPct)
		}
		if sc.StopLossMarginPct != nil && *sc.StopLossMarginPct > 0 {
			report("derived stop-loss price %% (stop_loss_margin_pct / leverage)", *sc.StopLossMarginPct/lev)
		}
	}
	if hlStopLossResolvesFromMaxDrawdownFallback(sc) {
		fallback := sc.MaxDrawdownPct
		if fallback > MaxAutoStopLossPct {
			fallback = MaxAutoStopLossPct
		}
		report("max_drawdown_pct", fallback)
	}
	return errs
}

// hlStopLossResolvesFromMaxDrawdownFallback mirrors exactly the branch chain
// of EffectiveStopLossPct up to its MaxDrawdownPct fallback: true only when
// that fallback is the owner that would actually resolve — every earlier
// branch (unified close, positive ATR mults, regime dicts, any explicit pct
// field) defers or wins instead. Explicit-zero ATR fields fall through in
// EffectiveStopLossPct, so they fall through here too.
//
// #1456 review round 4: the regime blocks are read with the raw-aware
// IsConfigured(), NOT EffectiveStopLossPct's !IsZero(). EffectiveStopLossPct is
// a RUNTIME caller, so its blocks have been through ResolveSurface; this helper
// runs from validateConfig's per-strategy loop, which is BEFORE
// validateRegimeATRConfig resolves them, and IsZero() reports true on an
// unresolved-but-configured block (see its doc, and #735.1 / #1111 which hit
// the same phase ordering). Reading !IsZero() here made a strategy whose stop is
// owned by stop_loss_atr_regime / trailing_stop_atr_regime look like it had NO
// owner, so the max_drawdown_pct fallback was scored as the owner and this
// FATAL bound refused to boot a legal config. The two predicates agree on every
// config that loads: a block that resolves is always use_defaults or a non-empty
// trend_regime map, and one that does not resolve fails validation anyway.
func hlStopLossResolvesFromMaxDrawdownFallback(sc StrategyConfig) bool {
	if strategyUsesUnifiedRegimeClose(sc) {
		return false
	}
	if sc.TrailingStopATRMult != nil && *sc.TrailingStopATRMult > 0 {
		return false
	}
	if sc.StopLossATRMult != nil && *sc.StopLossATRMult > 0 {
		return false
	}
	if sc.StopLossATRRegime.IsConfigured() {
		return false
	}
	if sc.TrailingStopATRRegime.IsConfigured() {
		return false
	}
	return sc.TrailingStopPct == nil &&
		sc.StopLossPct == nil &&
		sc.StopLossMarginPct == nil &&
		sc.MaxDrawdownPct > 0
}

// --- throttled operator alert ---------------------------------------------

// hlLiquidationAlertState is the per-(strategy, symbol) throttle record.
type hlLiquidationAlertState struct {
	Notified       bool
	LastNotifiedAt time.Time
	// LastAction is the action reported by the LAST notification. ANY change
	// re-notifies immediately, in both directions: an operator told the stop was
	// clamped needs to know when that stops being true, and one told the
	// replacement was merely deferred (the old stop still resting) needs to know
	// when it escalates to protection lost (no stop resting at all). A boolean
	// "did it fail" cannot express the second transition, because both states
	// are failures.
	LastAction hlLiquidationAlertAction
}

// hlLiquidationAlerts is keyed by hlLiquidationAlertKey. A sync.Map mirrors
// atrMultMissingEntryATRWarned: the audit runs on the main loop goroutine, but
// the per-strategy clamp sites run inside the dispatch where concurrent
// strategies can observe the same coin.
var hlLiquidationAlerts sync.Map

func hlLiquidationAlertKey(strategyID, symbol string) string {
	return strategyID + ":" + symbol
}

// hlLiquidationShouldNotify is the pure throttle decision. Escape hatches
// mirror portfolioWarningShouldNotify: the first observation of a condition
// always notifies, a newly-deferred replace always notifies, and otherwise the
// alert_throttle_interval floor applies.
func hlLiquidationShouldNotify(prev hlLiquidationAlertState, action hlLiquidationAlertAction, now time.Time) (bool, hlLiquidationAlertState) {
	notify := false
	switch {
	case !prev.Notified:
		notify = true // first observation for this (strategy, symbol)
	case action != prev.LastAction:
		notify = true // what the scheduler is doing about it changed — say so
	case now.Sub(prev.LastNotifiedAt) >= effectiveAlertThrottleInterval():
		notify = true // periodic reminder while the geometry stays impossible
	}
	if !notify {
		return false, prev
	}
	return true, hlLiquidationAlertState{
		Notified:       true,
		LastNotifiedAt: now,
		LastAction:     action,
	}
}

// hlLiquidationAlertDue applies the throttle to the live sync.Map and reports
// whether this observation should be sent.
func hlLiquidationAlertDue(strategyID, symbol string, action hlLiquidationAlertAction, now time.Time) bool {
	key := hlLiquidationAlertKey(strategyID, symbol)
	var prev hlLiquidationAlertState
	if v, ok := hlLiquidationAlerts.Load(key); ok {
		if s, ok2 := v.(hlLiquidationAlertState); ok2 {
			prev = s
		}
	}
	send, next := hlLiquidationShouldNotify(prev, action, now)
	if send {
		hlLiquidationAlerts.Store(key, next)
	}
	return send
}

// clearHLLiquidationAlert drops the throttle key so the next observation
// notifies on its first cycle. Called when the condition clears (the stop is no
// longer past liquidation) and when the position closes — an operator who fixed
// the geometry, or re-opened later, must not have the re-alert suppressed.
func clearHLLiquidationAlert(strategyID, symbol string) {
	hlLiquidationAlerts.Delete(hlLiquidationAlertKey(strategyID, symbol))
}

// clearHLLiquidationAlertOnHLPerpsClose mirrors
// clearATRMultMissingEntryATRWarningOnHLPerpsClose: shared close paths run for
// spot and futures too, so the platform/type check lives here rather than at
// every call site.
func clearHLLiquidationAlertOnHLPerpsClose(s *StrategyState, symbol string) {
	if s == nil || s.Platform != "hyperliquid" {
		return
	}
	if s.Type != "perps" && s.Type != "manual" {
		return
	}
	clearHLLiquidationAlert(s.ID, symbol)
}

// clearHLPerpsPositionAlertThrottles is the single "an HL position closed, drop
// its per-position alert throttles" hook. Every position-close path calls it so
// a future re-open re-alerts on its first cycle instead of inheriting a stale
// suppression. Add new per-(strategy, symbol) throttles HERE rather than
// spraying a new call at each of the close sites.
func clearHLPerpsPositionAlertThrottles(s *StrategyState, symbol string) {
	clearATRMultMissingEntryATRWarningOnHLPerpsClose(s, symbol)
	clearHLLiquidationAlertOnHLPerpsClose(s, symbol)
}

// hlLiquidationAlertAction labels what the scheduler did about the impossible
// geometry, so the operator DM is specific instead of merely worrying.
type hlLiquidationAlertAction string

const (
	// hlLiquidationActionClamped — the trigger was tightened to just inside
	// liquidation and the placement landed.
	hlLiquidationActionClamped hlLiquidationAlertAction = "clamped"
	// hlLiquidationActionReplaceDeferred — the cancel+replace failed BEFORE the
	// cancel landed; the OLD (inert but resting) stop is still there and the
	// scheduler retries next cycle.
	hlLiquidationActionReplaceDeferred hlLiquidationAlertAction = "replace deferred"
	// hlLiquidationActionProtectionLost — the cancel landed but the replacement
	// did NOT rest. The position has no exchange-side stop at all right now.
	// This is strictly worse than "deferred" and must never be reported as a
	// successful clamp.
	hlLiquidationActionProtectionLost hlLiquidationAlertAction = "protection lost"
	// hlLiquidationActionRearmed — a static scalar owner that had lost its stop
	// was re-armed by the audit at its configured distance.
	hlLiquidationActionRearmed hlLiquidationAlertAction = "re-armed"
	// hlLiquidationActionRearmFailed — the re-arm placement did not rest; the
	// position is still without an exchange-side stop.
	hlLiquidationActionRearmFailed hlLiquidationAlertAction = "re-arm failed"
	// hlLiquidationActionUnreconciled — the audit REFUSED to touch the order
	// because this coin's book does not match the on-chain snapshot (a phantom
	// virtual position on a shared coin). Acting could move a reduce-only
	// trigger that would close a peer strategy's real size.
	hlLiquidationActionUnreconciled hlLiquidationAlertAction = "not reconciled"
	// hlLiquidationActionExited — the clamped trigger FILLED at submit: the
	// position is FLAT. Reporting "tightened to $X" here would name an order
	// that no position needs anymore (#1456 review).
	hlLiquidationActionExited hlLiquidationAlertAction = "exited"
	// hlLiquidationActionFilledOnChain — the ORIGINAL stop already fired
	// on-chain before the replace could cancel it. Nothing was cancelled and
	// there is nothing left to replace; the reconciler books the close.
	// Reporting "replace deferred — the original stop is still resting" would
	// describe an order that just filled (#1456 review).
	hlLiquidationActionFilledOnChain hlLiquidationAlertAction = "SL filled"
)

// hlLiquidationActionUnprotected reports whether the action leaves the position
// with NO exchange-side stop resting. Used only for message severity — the
// throttle keys on the action itself.
func hlLiquidationActionUnprotected(a hlLiquidationAlertAction) bool {
	return a == hlLiquidationActionProtectionLost || a == hlLiquidationActionRearmFailed
}

// hlLiquidationUnprotectedRecovery states the ACTUAL recovery path for an
// unprotected position (#1456 review). The audit re-arms only STATIC SCALAR
// owners — it deliberately skips `unprotected && !staticScalar` — so a
// trailing or fixed-ATR owner recovers on ITS OWN next due manage-only cycle,
// which sits inside the dueStrategies dispatch gated on Signal == 0 and can be
// a whole strategy interval away. Promising "every cycle" for those owners
// described a self-healing cadence the code does not perform.
func hlLiquidationUnprotectedRecovery(sc StrategyConfig) string {
	if !scaleInLiveProtectionResizable(sc) {
		// Static scalar owner: no self-re-arm exists; the audit owns recovery
		// pre-dispatch every scheduler cycle.
		return "The scheduler re-arms it on the next cycle"
	}
	if sc.IntervalSeconds > 0 {
		return fmt.Sprintf("This strategy's own stop management re-arms it on its next due manage-only cycle (interval %s)", (time.Duration(sc.IntervalSeconds) * time.Second).String())
	}
	return "This strategy's own stop management re-arms it on its next due manage-only cycle"
}

// notifyHLStopPastLiquidation emits the throttled owner alert. Callers MUST
// invoke it with no state lock held: it sends to the notifier.
func notifyHLStopPastLiquidation(sc StrategyConfig, symbol, side string, triggerPx, clampedPx, liqPx float64, action hlLiquidationAlertAction, notifier *MultiNotifier, logger *StrategyLogger, now time.Time) {
	recovery := hlLiquidationUnprotectedRecovery(sc)
	_, detail, unprotected := hlLiquidationAlertMessage(triggerPx, clampedPx, liqPx, action, recovery)
	// The DM is throttled; the CRITICAL log for a position with NO exchange-side
	// stop is NOT. A naked live position must stay visible on every cycle it
	// persists, and the throttle exists to bound DM volume, never to hide the
	// state itself from the operator surfaces.
	due := hlLiquidationAlertDue(sc.ID, symbol, action, now)
	if logger != nil && (due || unprotected) {
		if unprotected {
			logger.Error("CRITICAL: %s (%s) has no exchange-side stop: %s", symbol, side, detail)
		} else {
			logger.Warn("SL past liquidation for %s (%s): %s", symbol, side, detail)
		}
	}
	if !due {
		return
	}
	if notifier != nil && notifier.HasBackends() {
		msg := hlLiquidationAlertFullMessage(sc, symbol, side, triggerPx, clampedPx, liqPx, action)
		notifier.SendToAllChannels(msg)
		notifier.SendOwnerDM(msg)
	}
}

// hlLiquidationAlertFullMessage assembles the complete operator DM. The
// past-liquidation advisory asserts a CAUSE (unreachable geometry from
// leverage) that only exists when both prices are known and the position is
// still open. A re-arm fixed a lost stop, and a flat position needs no advice —
// appending it there lectures about a geometry that was never measured
// (#1456 review).
func hlLiquidationAlertFullMessage(sc StrategyConfig, symbol, side string, triggerPx, clampedPx, liqPx float64, action hlLiquidationAlertAction) string {
	headline, detail, _ := hlLiquidationAlertMessage(triggerPx, clampedPx, liqPx, action, hlLiquidationUnprotectedRecovery(sc))
	msg := fmt.Sprintf("%s [%s] %s %s — %s.", headline, sc.ID, symbol, side, detail)
	if triggerPx > 0 && liqPx > 0 &&
		action != hlLiquidationActionRearmed &&
		action != hlLiquidationActionExited &&
		action != hlLiquidationActionFilledOnChain {
		msg += " A stop past liquidation can never fill: Hyperliquid force-closes first, at liquidation-engine pricing. Lower the leverage or tighten the stop distance so the configured geometry is reachable."
	}
	return msg
}

// hlLiquidationAlertMessage is the PURE message composition: headline, detail,
// and whether the state it describes leaves the position with NO exchange-side
// stop (which drives log severity).
//
// recovery (#1456 review) is the sentence stating how the position ACTUALLY
// regains a stop — the audit's per-cycle re-arm for static scalar owners, the
// owner's own next due manage-only cycle for everything else. It replaces the
// hardcoded "the scheduler retries every cycle", which promised a cadence the
// audit deliberately does not perform for non-scalar owners.
//
// The past-liquidation preamble is emitted ONLY when both prices are known. A
// candidate that carries no stop at all has triggerPx == 0, and an audit that
// ran without a usable snapshot has liqPx == 0; printing "configured trigger
// $0.0000 is past the exchange liquidation price $0.0000" for either describes
// a geometry that does not exist.
func hlLiquidationAlertMessage(triggerPx, clampedPx, liqPx float64, action hlLiquidationAlertAction, recovery string) (headline, detail string, unprotected bool) {
	headline = "**HL STOP PAST LIQUIDATION**"
	geometryKnown := triggerPx > 0 && liqPx > 0
	preamble := "the position has NO exchange-side stop"
	if geometryKnown {
		preamble = fmt.Sprintf("configured trigger $%.4f is past the exchange liquidation price $%.4f", triggerPx, liqPx)
	}
	unprotected = hlLiquidationActionUnprotected(action)
	switch action {
	case hlLiquidationActionRearmed:
		headline = "**HL STOP RE-ARMED**"
		// A re-arm most often fixes a stop that failed at OPEN — no liquidation
		// price may be known at all. Printing "$0.0000" for a geometry nobody
		// measured describes nothing; name the price only when it exists.
		if liqPx > 0 {
			detail = fmt.Sprintf("the position had NO exchange-side stop; re-armed at $%.4f (liquidation $%.4f)", clampedPx, liqPx)
		} else {
			detail = fmt.Sprintf("the position had NO exchange-side stop; re-armed at $%.4f", clampedPx)
		}
	case hlLiquidationActionRearmFailed:
		headline = "**HL POSITION UNPROTECTED**"
		if geometryKnown {
			detail = fmt.Sprintf("%s — the clamped stop at $%.4f did NOT rest: the position has no exchange-side stop right now. %s.", preamble, clampedPx, recovery)
		} else {
			detail = fmt.Sprintf("the position has NO exchange-side stop and the re-arm at $%.4f did not rest; %s", clampedPx, strings.ToLower(recovery[:1])+recovery[1:])
		}
	case hlLiquidationActionClamped:
		detail = preamble + fmt.Sprintf(" — tightened to $%.4f (%.2f%% inside liquidation)", clampedPx, hlLiquidationStopBufferPct)
	case hlLiquidationActionReplaceDeferred:
		detail = preamble + fmt.Sprintf(" — could not re-place at $%.4f; the ORIGINAL stop is still resting and the scheduler will retry next cycle", clampedPx)
	case hlLiquidationActionProtectionLost:
		headline = "**HL POSITION UNPROTECTED**"
		detail = preamble + fmt.Sprintf(" — the old trigger was CANCELLED but the replacement at $%.4f did NOT rest: the position has no exchange-side stop right now. %s.", clampedPx, recovery)
	case hlLiquidationActionUnreconciled:
		// A refused candidate that carries no stop is UNPROTECTED and stays
		// that way until the coin reconciles — that is strictly worse than a
		// refused tighten, where the old (unreachable) order is still resting,
		// so it gets the louder headline and the CRITICAL log.
		if !geometryKnown {
			headline = "**HL POSITION UNPROTECTED**"
			unprotected = true
		}
		detail = preamble + " — the audit did NOT touch the order: this coin's recorded size does not match the on-chain snapshot, so moving a reduce-only trigger could close a peer strategy's real position. Reconcile the coin, then the audit heals it."
	case hlLiquidationActionExited:
		headline = "**HL STOP FILLED — POSITION FLAT**"
		detail = fmt.Sprintf("the replacement trigger at $%.4f FILLED at submit — the position exited immediately, at a price inside the old liquidation price. No stop is needed; nothing is unprotected.", clampedPx)
	case hlLiquidationActionFilledOnChain:
		headline = "**HL STOP ALREADY FILLED**"
		detail = "the ORIGINAL stop already fired on-chain before it could be replaced — nothing was cancelled and there is no order left to replace; the reconciler books the close."
	default:
		detail = preamble
	}
	return headline, detail, unprotected
}

// hlLiquidationArmClampAction reports what a ONE-SHOT arm that was clamped
// inside liquidation should tell the operator.
//
// A one-shot arm (the #562 fixed-ATR arm) cancels nothing, so it can never lose
// a resting stop — but it can fail to place one, and a position that was already
// without a stop then STAYS without one. "clamped … tightened to $X" is claimed
// only when an order is on the book; every other shape reports the position as
// unprotected.
func hlLiquidationArmClampAction(result *HyperliquidStopLossUpdateResult, armOK bool) hlLiquidationAlertAction {
	if armOK && result != nil {
		if result.StopLossOID > 0 {
			return hlLiquidationActionClamped
		}
		// A fill at submit means the position is FLAT — the operator must
		// hear that, never "tightened to $X" (#1456 review).
		if result.StopLossFilledImmediately && result.StopLossTriggerPx > 0 {
			return hlLiquidationActionExited
		}
	}
	return hlLiquidationActionRearmFailed
}

// hlTriggerRoundingTolerancePct absorbs exchange tick rounding when comparing a
// freshly derived trigger against the one actually resting. Hyperliquid rounds
// a perps trigger price on placement, so the value stored back in state is the
// ROUNDED one; re-deriving it next cycle produces a marginally different
// number. Without this slack that difference alone would read as "tighter" and
// force a cancel+replace every cycle. It is two orders of magnitude below
// hlLiquidationStopBufferPct, so a genuine tighten is never absorbed.
const hlTriggerRoundingTolerancePct = 0.05

// hlTriggerStrictlyTighter reports whether candidate is a MEANINGFULLY tighter
// stop than resting for a position on the given side — higher for a long, lower
// for a short — beyond tick-rounding noise.
//
// Returns false when nothing is resting: there is no order to improve on, and
// the placement path handles a fresh arm on its own.
func hlTriggerStrictlyTighter(side string, candidate, resting float64) bool {
	if candidate <= 0 || resting <= 0 {
		return false
	}
	slack := resting * (hlTriggerRoundingTolerancePct / 100.0)
	switch side {
	case "long":
		return candidate > resting+slack
	case "short":
		return candidate < resting-slack
	}
	return false
}

// hlProtectionSLTriggerPx reproduces, in Go, the trigger price Python derives
// from a protection plan's SL ATR multiple: anchor - mult*atr for a long,
// anchor + mult*atr for a short (check_hyperliquid.py, run_protection_sync).
//
// It is the SINGLE place that formula is mirrored. Every #1450 decision about a
// protection-plan SL — is the derived trigger past liquidation, would a rewrite
// reproduce the clamped price, is a force-replace worth issuing — reads it from
// here, so a change to the Python geometry has exactly one Go counterpart to
// follow. Returns 0 when the inputs cannot produce a trigger.
func hlProtectionSLTriggerPx(side string, anchor, entryATR, slMult float64) float64 {
	if slMult <= 0 || entryATR <= 0 || anchor <= 0 {
		return 0
	}
	switch side {
	case "long":
		return anchor - slMult*entryATR
	case "short":
		return anchor + slMult*entryATR
	}
	return 0
}

// hlClampProtectionSLMult clamps a protection plan's SL ATR multiple so that
// the trigger Python derives from it (anchor minus/plus mult times EntryATR)
// lands just inside the liquidation price instead of past it.
//
// Rewriting the MULTIPLE rather than adding a trigger-price argument is what
// keeps this to one placement implementation: the protection sync already
// passes StopLossATRMult to the Python protection-sync entry point, so the
// clamped geometry rides the unchanged CLI contract — no new flag, no probe
// argv change, no Python edit.
//
// Returns (slMult, false) unchanged when the liquidation price is unknown, the
// inputs are incomplete, or the trigger is already reachable. It NEVER returns
// a non-positive multiple: 0 would tell Python to place no stop-loss at all,
// which is the exact failure this mechanism exists to prevent.
func hlClampProtectionSLMult(side string, anchor, entryATR, slMult, liqPx float64) (float64, bool) {
	if liqPx <= 0 || slMult <= 0 || entryATR <= 0 || anchor <= 0 {
		return slMult, false
	}
	if side != "long" && side != "short" {
		return slMult, false
	}
	// hlProtectionSLTriggerPx is the single mirror of the Python formula; for a
	// long it can legitimately return <= 0 under an extreme multiple, which the
	// branch below handles explicitly.
	wouldBe := hlProtectionSLTriggerPx(side, anchor, entryATR, slMult)
	clamped, ok := clampStopInsideLiquidation(side, wouldBe, liqPx)
	if !ok {
		if side == "long" && wouldBe <= 0 {
			// A derived long trigger at or below zero can never fill and is
			// past liquidation by construction, but stopPastLiquidation
			// deliberately reads a non-positive price as "unknown". Handle it
			// explicitly rather than leaving the impossible geometry in place.
			clamped = liqPx * (1.0 + hlLiquidationStopBufferPct/100.0)
		} else {
			return slMult, false
		}
	}
	// The multiple must be DIRECTIONAL: Python reproduces the trigger as
	// anchor - mult*atr (long) / anchor + mult*atr (short), so an unsigned
	// distance would mirror a clamped price that landed on the FAR side of the
	// anchor back across it. That mirrored trigger is itself past liquidation,
	// which would report a successful clamp and then force a cancel+replace at
	// the same unreachable price every cycle. The far-side case is reachable in
	// CROSS margin, where account-wide losses can push liquidationPx above a
	// long's frozen entry (and below a short's).
	var delta float64
	switch side {
	case "long":
		delta = anchor - clamped
	case "short":
		delta = clamped - anchor
	}
	newMult := delta / entryATR
	if newMult <= 0 || math.IsNaN(newMult) || math.IsInf(newMult, 0) {
		// The clamped trigger cannot be expressed as a positive multiple on the
		// correct side of the anchor. Report NO clamp rather than a wrong one:
		// the caller then leaves the resolved multiple alone, sets no
		// force-replace, and the per-cycle audit tightens the RESTING trigger
		// directly instead.
		return slMult, false
	}
	if newMult > slMult {
		// One-way tighten, belt-and-braces. A clamp derived from a trigger that
		// was past liquidation is always strictly closer to the anchor than the
		// original, so this is unreachable; a widening rewrite would loosen
		// protection and must never ship.
		return slMult, false
	}
	return newMult, true
}

// hlProtectionSLTriggerReachable reports whether the trigger Python derives
// from slMult lands on the fillable side of the liquidation price.
//
// This is what keeps a force-replace honest: a replace is only worth issuing
// when the price it would rest at is actually reachable. Forcing a replace at a
// trigger that is still past liquidation re-places the SAME unfillable order
// every cycle, against the live account, forever.
//
// Returns false when the inputs are incomplete or slMult is non-positive (no
// SL is being placed at all), and true when the liquidation price is unknown —
// an unknown never blocks the pre-existing replace behavior.
func hlProtectionSLTriggerReachable(side string, anchor, entryATR, slMult, liqPx float64) bool {
	if slMult <= 0 || entryATR <= 0 || anchor <= 0 {
		return false
	}
	if liqPx <= 0 {
		return true
	}
	wouldBe := hlProtectionSLTriggerPx(side, anchor, entryATR, slMult)
	if wouldBe <= 0 {
		return false
	}
	return !stopPastLiquidation(side, wouldBe, liqPx)
}

// --- per-cycle audit -------------------------------------------------------

// hlLiquidationAuditCandidate is the read-only snapshot the audit decision
// needs. Captured under RLock so the decision itself can run lock-free.
type hlLiquidationAuditCandidate struct {
	StrategyID string
	Script     string
	Symbol     string
	Side       string
	// Qty is the SL size to place — already passed through hlSLEffectiveQty so
	// the #621 virtual-vs-on-chain cap applies.
	Qty               float64
	StopLossOID       int64
	StopLossTriggerPx float64
	LiquidationPx     float64
	// StaticScalarOwner is !scaleInLiveProtectionResizable(sc): the stop was
	// placed once at open by a scalar owner with no re-place mechanism. Only
	// these owners are RE-ARMED by the audit when nothing is resting; every
	// other owner re-arms on its own path (walker, fixed-ATR arm, protection
	// sync). The past-liquidation TIGHTEN below applies to every owner.
	StaticScalarOwner bool
	// RearmTriggerPx is the trigger a static scalar owner's configured distance
	// resolves to, already clamped inside liquidation. 0 for every other owner
	// and whenever the geometry cannot be resolved.
	RearmTriggerPx float64
	// Unprotected is true when the position carries NO exchange-side stop at
	// all (no OID and no trigger).
	Unprotected bool
	// BookConsistent is true when this coin's SIGNED recorded net across every
	// live HL strategy fits inside the on-chain size reported by this cycle's
	// snapshot (#1456 review round 8: the exchange nets one position per coin,
	// so a legal long peer + short peer book nets to the reported figure —
	// only the signed net is comparable). False means at least one strategy
	// holds a phantom position on the coin, so a reduce-only trigger placed
	// from here could close a PEER strategy's real size. The audit refuses to
	// act and alerts instead.
	BookConsistent bool
}

// hlLiquidationAuditActionKind distinguishes the jobs the audit performs.
type hlLiquidationAuditActionKind int

const (
	// hlAuditTighten — a stop is resting past liquidation; cancel it and
	// re-place just inside.
	hlAuditTighten hlLiquidationAuditActionKind = iota
	// hlAuditRearm — a static scalar owner has NO stop resting; place one at
	// its configured distance.
	hlAuditRearm
	// hlAuditRefuse — the condition is real but the coin's book does not match
	// the on-chain snapshot; report only.
	hlAuditRefuse
)

// hlLiquidationAuditAction is one decided unit of work.
type hlLiquidationAuditAction struct {
	Candidate hlLiquidationAuditCandidate
	Kind      hlLiquidationAuditActionKind
	// ClampedTriggerPx is the price to place: the clamped trigger for a
	// tighten, the re-derived scalar trigger for a re-arm.
	ClampedTriggerPx float64
}

// planHyperliquidLiquidationAudit is the PURE audit decision: no locks, no
// globals, no IO. Candidates whose geometry is fine, whose liquidation price is
// unknown, or whose clamp would not move the trigger produce no action.
//
// The audit tightens for EVERY owner, not only the static scalar ones. The
// trailing walker and the protection sync do heal their own owners, but both
// run only inside the dispatch over the strategies that are DUE this cycle and
// only when that cycle's signal check returned Signal == 0. A 4h strategy on a
// non-due cycle, a cycle whose signal check errored, and a signal cycle that
// executed no trade all leave the unreachable stop resting for up to a full
// strategy interval — which is exactly the state #1450 exists to prevent. The
// audit runs every cycle, pre-dispatch, holding lockHyperliquidTrailingUpdate,
// so it cannot race the walker; and because the clamp is one-way tightening,
// the owner's own path finds a reachable trigger afterwards and leaves it
// alone (the walker never widens, and the protection plan force-replaces only
// when its own trigger is strictly tighter than what rests).
//
// Output order is deterministic (strategy id, then symbol) so operator output
// and tests are stable.
func planHyperliquidLiquidationAudit(candidates []hlLiquidationAuditCandidate) []hlLiquidationAuditAction {
	var actions []hlLiquidationAuditAction
	for _, c := range candidates {
		if c.Qty <= 0 {
			continue
		}
		switch {
		case c.Unprotected:
			// Only a static scalar owner is re-armed here; anything else has a
			// re-place path of its own that runs when the strategy next does.
			if !c.StaticScalarOwner || c.RearmTriggerPx <= 0 {
				continue
			}
			kind := hlAuditRearm
			if !c.BookConsistent {
				kind = hlAuditRefuse
			}
			actions = append(actions, hlLiquidationAuditAction{
				Candidate: c, Kind: kind, ClampedTriggerPx: c.RearmTriggerPx,
			})
		default:
			if c.StopLossTriggerPx <= 0 || c.LiquidationPx <= 0 {
				continue
			}
			clamped, ok := clampStopInsideLiquidation(c.Side, c.StopLossTriggerPx, c.LiquidationPx)
			if !ok {
				continue
			}
			kind := hlAuditTighten
			if !c.BookConsistent {
				kind = hlAuditRefuse
			}
			actions = append(actions, hlLiquidationAuditAction{
				Candidate: c, Kind: kind, ClampedTriggerPx: clamped,
			})
		}
	}
	sort.SliceStable(actions, func(i, j int) bool {
		if actions[i].Candidate.StrategyID != actions[j].Candidate.StrategyID {
			return actions[i].Candidate.StrategyID < actions[j].Candidate.StrategyID
		}
		return actions[i].Candidate.Symbol < actions[j].Candidate.Symbol
	})
	return actions
}

// hlLiquidationScalarRearmTriggerPx resolves the trigger a STATIC SCALAR owner
// would have been armed at, anchored on the frozen entry (#873 riskAnchorPrice)
// exactly like the order-time placement, and clamped inside liquidation.
//
// Returns 0 for any owner that is not a static scalar one, or when the geometry
// cannot be resolved — the caller then places nothing.
func hlLiquidationScalarRearmTriggerPx(sc StrategyConfig, side string, anchor, liqPx float64) float64 {
	if anchor <= 0 {
		return 0
	}
	pct := EffectiveStopLossPct(sc)
	if pct <= 0 {
		return 0
	}
	var trigger float64
	switch side {
	case "long":
		trigger = anchor * (1.0 - pct/100.0)
	case "short":
		trigger = anchor * (1.0 + pct/100.0)
	default:
		return 0
	}
	if trigger <= 0 {
		return 0
	}
	if clamped, ok := clampStopInsideLiquidation(side, trigger, liqPx); ok {
		return clamped
	}
	return trigger
}

// hlLiquidationCoinBookConsistent reports, per coin, whether the audit may
// touch an order on that coin this cycle.
//
// The hazard it guards is PEER damage, not book drift: a reduce-only trigger
// placed off stale virtual state can, if it fires, reduce ANOTHER strategy's
// real position. Shared coins deliberately never get per-strategy reconciliation
// (#258: reconciling per strategy on a shared coin collapses a peer's position
// and trips its circuit breaker), so the coin-level book check stands in for it.
//
// Two ways a coin qualifies:
//
//   - The recorded NET size across every live HL perps/manual strategy fits
//     inside the on-chain absolute size. Nothing is phantom, so nothing can be
//     over-closed. #1456 review round 8: the map is the SIGNED sum of recorded
//     quantities — Hyperliquid nets ONE position per coin, and the codebase
//     documents long + short peers on the same coin as legal (config.go,
//     bidirectional perps), so only the signed net is directly comparable to
//     the exchange's single reported figure. Summing magnitudes there made a
//     healthy 1.0-long + 0.4-short book read as a phantom against its true
//     0.6 net and refused the heal FOREVER for the leg that needed it.
//   - Exactly ONE live strategy records a position on the coin. There is no peer
//     to harm, and hlSLEffectiveQty already caps the placed size to the confirmed
//     on-chain quantity (#621) — precisely the drift a manual partial TP leaves
//     behind. Refusing here would leave the sole owner's unreachable stop
//     unreachable indefinitely, alerted but never healed.
//
// A coin missing from the snapshot never qualifies, on either route: nothing
// on-chain backs the recorded size, so there is no confirmed quantity to size a
// replacement from. Comparison carries the same 1e-9 slack the #621 size cap
// uses.
//
// Known limit (#1456 review round 9): phantoms on OPPOSITE sides of one coin
// cancel in the signed sum and read as consistent — a single netted exchange
// figure cannot distinguish them from a healthy bidirectional book, and no
// per-strategy on-chain truth exists on a pooled wallet (the same limit #258
// documents for shared-coin reconciliation). A strict per-owner bound would
// re-break the legal long+short case above. Bounds that remain: placements
// are reduce-only (Hyperliquid clips them at the real netted position), sized
// through hlSLEffectiveQty's cap at the confirmed on-chain quantity, and aimed
// only at owners whose recorded side matches the net side.
func hlLiquidationCoinBookConsistent(netVirtualByCoin map[string]float64, ownersByCoin map[string]int, onChainAbsQty map[string]float64) map[string]bool {
	out := make(map[string]bool, len(netVirtualByCoin))
	for coin, netVirtual := range netVirtualByCoin {
		onChain, ok := onChainAbsQty[coin]
		if !ok || onChain <= 1e-9 {
			out[coin] = false
			continue
		}
		out[coin] = math.Abs(netVirtual) <= onChain+1e-9 || ownersByCoin[coin] <= 1
	}
	return out
}

// collectHLLiquidationAuditCandidates snapshots every live HL perps/manual
// position the audit could act on, under a single RLock.
//
// Gating on hyperliquidIsLive is load-bearing: a PAPER strategy sharing a coin
// with a live position must never match a map entry — paper has no account and
// no liquidation price of its own.
func collectHLLiquidationAuditCandidates(
	strategies []StrategyConfig,
	state *AppState,
	hlLiquidationPx map[string]float64,
	// hlNetSideByCoin (#1456 review) is coin -> the side of the on-chain NET
	// position from the same snapshot. A candidate's liquidation price is read
	// only when its own side matches; see hlLiquidationPxForSide.
	hlNetSideByCoin map[string]string,
	hlOnChainAbsQty map[string]float64,
	mu *sync.RWMutex,
) []hlLiquidationAuditCandidate {
	if state == nil {
		return nil
	}
	var out []hlLiquidationAuditCandidate
	virtualByCoin := make(map[string]float64)
	ownersByCoin := make(map[string]int)
	mu.RLock()
	for _, sc := range strategies {
		if sc.Platform != "hyperliquid" {
			continue
		}
		if sc.Type != "perps" && sc.Type != "manual" {
			continue
		}
		if !hyperliquidIsLive(sc.Args) {
			continue
		}
		ss, ok := state.Strategies[sc.ID]
		if !ok || ss == nil {
			continue
		}
		for symbol, pos := range ss.Positions {
			if pos == nil || pos.Quantity <= 0 {
				continue
			}
			if pos.Side != "long" && pos.Side != "short" {
				continue
			}
			// #1159: a hedge leg carries no SL of its own, so it never reaches
			// here. Belt-and-braces — never touch a leg this strategy does not
			// own outright.
			if pos.HedgeFor != "" {
				continue
			}
			// Every live recorded position on the coin counts toward the
			// consistency check, including the ones that produce no candidate.
			// SIGNED (#1456 review round 8): the exchange nets one position per
			// coin, so the comparable figure is the signed virtual net — a
			// documented legal long peer + short peer on the same coin sums to
			// |net|, which is what the snapshot reports. Summing magnitudes
			// made that configuration permanently "inconsistent" and refused
			// the heal forever.
			if pos.Side == "short" {
				virtualByCoin[symbol] -= pos.Quantity
			} else {
				virtualByCoin[symbol] += pos.Quantity
			}
			ownersByCoin[symbol]++

			staticScalar := !scaleInLiveProtectionResizable(sc)
			armed := pos.StopLossTriggerPx > 0
			unprotected := pos.StopLossOID == 0 && pos.StopLossTriggerPx <= 0
			if !armed && !unprotected {
				// An OID with no recorded trigger: something rests on-chain but
				// its geometry is unknown, so neither a tighten nor a re-arm can
				// be justified. Leave it to the owner's own path.
				continue
			}
			if unprotected && !staticScalar {
				// The walker / fixed-ATR arm / protection sync re-arms these.
				continue
			}
			liqPx := hlLiquidationPxForSide(hlLiquidationPx, hlNetSideByCoin, symbol, pos.Side)
			if armed && liqPx <= 0 {
				// Unknown liquidation price — never derive a band; skip.
				continue
			}
			// #1456 review round 6: the re-arm PLACES a reduce-only order on
			// this position's side, so it needs the snapshot to confirm the
			// on-chain net is ON that side. A stale recorded side (a non-due
			// strategy on a long interval) would submit against the opposite
			// net every cycle — rejected, with an unthrottled CRITICAL each
			// time. Unlike liqPx, side confirmation does NOT depend on the
			// coin reporting a liquidation price, so a matching-side owner
			// with unknown geometry still re-arms.
			sideConfirmed := hlNetSideByCoin[symbol] == pos.Side
			if unprotected && !sideConfirmed {
				continue
			}
			// #621: never place a stop larger than the on-chain size.
			slQty, _ := hlSLEffectiveQty(symbol, pos.Quantity, hlOnChainAbsQty)
			rearmPx := 0.0
			if unprotected {
				rearmPx = hlLiquidationScalarRearmTriggerPx(sc, pos.Side, pos.riskAnchorPrice(), liqPx)
			}
			out = append(out, hlLiquidationAuditCandidate{
				StrategyID:        sc.ID,
				Script:            sc.Script,
				Symbol:            symbol,
				Side:              pos.Side,
				Qty:               slQty,
				StopLossOID:       pos.StopLossOID,
				StopLossTriggerPx: pos.StopLossTriggerPx,
				LiquidationPx:     liqPx,
				StaticScalarOwner: staticScalar,
				RearmTriggerPx:    rearmPx,
				Unprotected:       unprotected,
			})
		}
	}
	mu.RUnlock()

	consistent := hlLiquidationCoinBookConsistent(virtualByCoin, ownersByCoin, hlOnChainAbsQty)
	for i := range out {
		out[i].BookConsistent = consistent[out[i].Symbol]
	}
	return out
}

// hlLiquidationReplaceOutcome is what actually happened on the exchange.
//
// The distinction the bool it replaced could not make: a cancel that LANDED
// followed by a placement that did NOT rest leaves the position with no
// exchange-side stop. Reporting that as a successful clamp told the operator
// the stop was tightened while it had in fact been deleted.
type hlLiquidationReplaceOutcome int

const (
	// hlReplaceDeferred — nothing was cancelled; the ORIGINAL stop is still
	// resting exactly where it was.
	hlReplaceDeferred hlLiquidationReplaceOutcome = iota
	// hlReplacePlaced — a replacement is resting at the requested trigger.
	hlReplacePlaced
	// hlReplaceFilled — the trigger filled at submit; the position is flat.
	hlReplaceFilled
	// hlReplaceFilledExternally — the OLD stop fired on-chain before the
	// replace reached it. Nothing was cancelled; there is nothing left to
	// replace. Reporting this as a deferral told the operator the original
	// stop was still resting while it had just filled (#1456 review).
	hlReplaceFilledExternally
	// hlReplaceProtectionLost — the cancel landed, the replacement did not
	// rest. NOTHING is protecting the position.
	hlReplaceProtectionLost
)

// hlLiquidationClampReplace runs one cancel+replace (or, with
// candidate.StopLossOID == 0, one fresh placement) and interprets the result.
//
// It deliberately reuses runHyperliquidUpdateStopLossFunc — the SAME placement
// primitive the trailing walker and the open-cycle arm use — so there is only
// one stop-placement implementation that can drift. The result interpretation
// is spelled out here rather than shared with the walker because the walker's
// operator messages are trailing-specific.
func hlLiquidationClampReplace(candidate hlLiquidationAuditCandidate, clampedTriggerPx float64, logger *StrategyLogger) (*HyperliquidStopLossUpdateResult, hlLiquidationReplaceOutcome) {
	if clampedTriggerPx <= 0 || candidate.Qty <= 0 {
		return nil, hlReplaceDeferred
	}
	// Serialize against the trailing walker on the same coin so an audit
	// cancel+replace can never interleave with a walker cancel+replace.
	unlock := lockHyperliquidTrailingUpdate(candidate.Symbol)
	defer unlock()

	result, stderr, err := runHyperliquidUpdateStopLossFunc(candidate.Script, candidate.Symbol, candidate.Side, candidate.Qty, clampedTriggerPx, candidate.StopLossOID)
	if stderr != "" && logger != nil {
		logger.Info("liquidation-clamp SL replace stderr: %s", stderr)
	}
	if err != nil {
		if logger != nil {
			logger.Error("Liquidation-clamp SL replace failed for %s: %v", candidate.Symbol, err)
		}
		return result, hlReplaceDeferred
	}
	if result == nil {
		if logger != nil {
			logger.Error("Liquidation-clamp SL replace returned no result for %s", candidate.Symbol)
		}
		return nil, hlReplaceDeferred
	}
	if result.Error != "" {
		if result.CancelStopLossSucceeded {
			// #1456 review round 10: the subprocess raised AFTER the cancel
			// landed — nothing rests on the book. This is protection lost,
			// never a deferral: the caller's hlReplaceProtectionLost case
			// clears the dead OID and retries in-cycle, where "replace
			// deferred" would leave pos.StopLossOID pointed at a cancelled
			// order and tell the operator the original stop still rests.
			if logger != nil {
				logger.Error("CRITICAL: liquidation-clamp cancelled SL OID=%d for %s and the subprocess failed before placing — the position has NO exchange-side stop: %s", candidate.StopLossOID, candidate.Symbol, result.Error)
			}
			return result, hlReplaceProtectionLost
		}
		if logger != nil {
			logger.Error("Liquidation-clamp SL replace returned error for %s: %s", candidate.Symbol, result.Error)
		}
		return result, hlReplaceDeferred
	}
	if result.OpenOrderCheckError != "" {
		if logger != nil {
			logger.Warn("Liquidation-clamp SL replace deferred for %s (open-order lookup failed): %s", candidate.Symbol, result.OpenOrderCheckError)
		}
		return result, hlReplaceDeferred
	}
	if result.StopLossFilledExternally {
		if logger != nil {
			logger.Warn("Liquidation-clamp: SL OID=%d for %s already filled on-chain — reconciler will book the close", candidate.StopLossOID, candidate.Symbol)
		}
		return result, hlReplaceFilledExternally
	}
	if result.CancelStopLossError != "" {
		if logger != nil {
			logger.Warn("Liquidation-clamp SL cancel failed for %s; original stop still resting: %s", candidate.Symbol, result.CancelStopLossError)
		}
		return result, hlReplaceDeferred
	}
	if result.StopLossError != "" {
		if isHLOpenOrderCapRejection(result.StopLossError) {
			if logger != nil {
				logger.Error("CRITICAL: HL open-order-cap rejected the liquidation-clamp SL replace for %s: %s", candidate.Symbol, result.StopLossError)
			}
		} else if logger != nil {
			logger.Warn("Liquidation-clamp SL placement failed (non-fatal) for %s: %s", candidate.Symbol, result.StopLossError)
		}
	}
	switch {
	case result.StopLossFilledImmediately && result.StopLossTriggerPx > 0:
		// This is protection working: the clamp landed inside the mark, so the
		// position exits NOW at a better price than liquidation would give.
		if logger != nil {
			logger.Warn("Liquidation-clamp SL filled at submit for %s — position exited inside the liquidation price", candidate.Symbol)
		}
		return result, hlReplaceFilled
	case result.StopLossOID > 0:
		return result, hlReplacePlaced
	case result.CancelStopLossSucceeded:
		// The ONLY branch that must never read as success. The old trigger is
		// gone from the book and nothing replaced it.
		if logger != nil {
			logger.Error("CRITICAL: liquidation-clamp cancelled SL OID=%d for %s but the replacement did not rest — the position has NO exchange-side stop", candidate.StopLossOID, candidate.Symbol)
		}
		return result, hlReplaceProtectionLost
	}
	return result, hlReplaceDeferred
}

// hlLiquidationRetryPlace retries a FAILED placement once with nothing to
// cancel (cancelOID = 0) — used only after a cancel LANDED and the replacement
// did NOT rest, where the audit itself just stripped the position's only
// exchange-side stop (#1456 review round 6). Deferring recovery to the owner's
// own path would leave a trailing/ATR owner naked for up to a whole strategy
// interval: both sit inside the dueStrategies dispatch gated on Signal == 0,
// while the cancel ran on the every-cycle audit. The clamped trigger and the
// on-chain-capped quantity are already in hand; nothing rests on the book, so
// a fresh placement cannot duplicate an order.
func hlLiquidationRetryPlace(candidate hlLiquidationAuditCandidate, clampedTriggerPx float64, logger *StrategyLogger) (*HyperliquidStopLossUpdateResult, hlLiquidationReplaceOutcome) {
	if clampedTriggerPx <= 0 || candidate.Qty <= 0 {
		return nil, hlReplaceDeferred
	}
	unlock := lockHyperliquidTrailingUpdate(candidate.Symbol)
	defer unlock()
	return hlLiquidationPlaceFresh(candidate.Script, candidate.Symbol, candidate.Side, candidate.Qty, clampedTriggerPx, logger)
}

// hlLiquidationPlaceFresh submits ONE fresh placement (cancelOID = 0) and
// classifies the outcome. Callers that already hold lockHyperliquidTrailingUpdate
// (the trailing walker's clamp branch) may call it directly — it does NOT take
// the coin lock itself.
func hlLiquidationPlaceFresh(script, symbol, side string, qty, triggerPx float64, logger *StrategyLogger) (*HyperliquidStopLossUpdateResult, hlLiquidationReplaceOutcome) {
	if triggerPx <= 0 || qty <= 0 {
		return nil, hlReplaceDeferred
	}
	result, stderr, err := runHyperliquidUpdateStopLossFunc(script, symbol, side, qty, triggerPx, 0)
	if stderr != "" && logger != nil {
		logger.Info("liquidation-clamp SL retry stderr: %s", stderr)
	}
	if err != nil {
		if logger != nil {
			logger.Error("Liquidation-clamp SL retry failed for %s: %v", symbol, err)
		}
		return result, hlReplaceDeferred
	}
	if result == nil {
		return nil, hlReplaceDeferred
	}
	switch {
	case result.Error != "", result.OpenOrderCheckError != "", result.StopLossError != "":
		if logger != nil && result.StopLossError != "" {
			logger.Error("CRITICAL: liquidation-clamp SL retry did not rest for %s: %s — the position has NO exchange-side stop", symbol, result.StopLossError)
		}
		return result, hlReplaceDeferred
	case result.StopLossFilledImmediately && result.StopLossTriggerPx > 0:
		return result, hlReplaceFilled
	case result.StopLossOID > 0:
		if logger != nil {
			logger.Warn("Liquidation-clamp SL retry rested for %s at $%.4f after the first placement was rejected", symbol, result.StopLossTriggerPx)
		}
		return result, hlReplacePlaced
	}
	return result, hlReplaceDeferred
}

// hlLiquidationPendingAlert defers one operator alert until every lock is
// released (the pending-slice drain pattern used by the HL balance reconciler).
type hlLiquidationPendingAlert struct {
	sc        StrategyConfig
	symbol    string
	side      string
	triggerPx float64
	clampedPx float64
	liqPx     float64
	action    hlLiquidationAlertAction
	logger    *StrategyLogger
}

// hlLiquidationAuditResult is what the driver reports back to the main loop.
type hlLiquidationAuditResult struct {
	// ImmediateFills counts positions that ended the cycle flat because a
	// clamped trigger filled at submit (booked as stop-loss closes).
	ImmediateFills int
	// CloseDetails carries one operator-facing line per booked close, so the
	// audit's exits reach the same trade notification every other close path
	// produces instead of only a stdout line.
	CloseDetails []hlLiquidationCloseDetail
}

// hlLiquidationCloseDetail is one booked close awaiting its trade alert.
type hlLiquidationCloseDetail struct {
	SC     StrategyConfig
	Symbol string
	FillPx float64
	Detail string
}

// runHyperliquidLiquidationAudit is the per-cycle driver. Sequence:
//
//	RLock snapshot → release → unlocked subprocesses → Lock re-validated apply
//	→ release → drain alerts.
//
// It runs on the main loop goroutine, before per-strategy dispatch, so it
// cannot interleave with a walker cancel+replace on the same OID.
func runHyperliquidLiquidationAudit(
	strategies []StrategyConfig,
	state *AppState,
	hlLiquidationPx map[string]float64,
	// hlNetSideByCoin (#1456 review) gates every read of hlLiquidationPx on
	// the strategy's own side matching the on-chain NET side.
	hlNetSideByCoin map[string]string,
	hlOnChainAbsQty map[string]float64,
	// snapshotFetched reports whether THIS cycle's clearinghouseState fetch
	// succeeded. It is not optional: both maps are built from that snapshot, so
	// a failed fetch hands the audit two EMPTY maps, which is indistinguishable
	// from "every coin vanished on-chain". Every position would then read as a
	// phantom and the audit would raise a CRITICAL recorded-vs-on-chain mismatch
	// for a mismatch that did not happen — turning a transient exchange-API
	// failure into a false alarm about the operator's book. "No snapshot this
	// cycle" and "the snapshot contradicts the book" are different facts and the
	// audit must never conflate them. Same guard
	// reconcileHyperliquidAccountPositions is gated on.
	snapshotFetched bool,
	mu *sync.RWMutex,
	notifier *MultiNotifier,
	now time.Time,
) hlLiquidationAuditResult {
	var res hlLiquidationAuditResult
	if !snapshotFetched {
		return res
	}
	candidates := collectHLLiquidationAuditCandidates(strategies, state, hlLiquidationPx, hlNetSideByCoin, hlOnChainAbsQty, mu)
	// Clear the throttle for every live candidate whose geometry is now fine,
	// so a recurrence re-alerts on its first cycle instead of being suppressed
	// by a stale key.
	for _, c := range candidates {
		if !c.Unprotected && !stopPastLiquidation(c.Side, c.StopLossTriggerPx, c.LiquidationPx) {
			clearHLLiquidationAlert(c.StrategyID, c.Symbol)
		}
	}
	actions := planHyperliquidLiquidationAudit(candidates)
	if len(actions) == 0 {
		return res
	}
	byID := make(map[string]StrategyConfig, len(strategies))
	for _, sc := range strategies {
		byID[sc.ID] = sc
	}

	var pending []hlLiquidationPendingAlert
	for _, act := range actions {
		c := act.Candidate
		sc, ok := byID[c.StrategyID]
		if !ok {
			continue
		}
		logger := &StrategyLogger{stratID: sc.ID, writer: os.Stderr}
		if act.Kind == hlAuditRefuse {
			// A phantom position on this coin: acting could move a reduce-only
			// trigger that closes a PEER strategy's real size. Report only.
			logger.Error("CRITICAL: #1450 audit refused to touch %s — recorded size across live strategies exceeds the on-chain snapshot", c.Symbol)
			pending = append(pending, hlLiquidationPendingAlert{
				sc: sc, symbol: c.Symbol, side: c.Side,
				triggerPx: c.StopLossTriggerPx, clampedPx: act.ClampedTriggerPx, liqPx: c.LiquidationPx,
				action: hlLiquidationActionUnreconciled, logger: logger,
			})
			continue
		}
		result, outcome := hlLiquidationClampReplace(c, act.ClampedTriggerPx, logger)
		action := hlLiquidationActionClamped
		if act.Kind == hlAuditRearm {
			action = hlLiquidationActionRearmed
		}
		switch outcome {
		case hlReplaceDeferred:
			// Nothing was cancelled — the ORIGINAL stop is still resting (or,
			// for a re-arm, there was nothing to lose). No state change.
			action = hlLiquidationActionReplaceDeferred
			if act.Kind == hlAuditRearm {
				action = hlLiquidationActionRearmFailed
			}
		case hlReplaceFilledExternally:
			// The old stop fired on-chain before the replace reached it. No
			// state change here — the reconciler books the close from the
			// exchange side, and the alert says the stop FILLED, never that
			// the original order is still resting.
			action = hlLiquidationActionFilledOnChain
		case hlReplaceProtectionLost:
			// The cancel landed and the replacement did not rest. State MUST be
			// updated so it stops pointing at an OID that no longer exists —
			// that is also what makes this position an Unprotected re-arm
			// candidate on the very next cycle.
			action = hlLiquidationActionProtectionLost
			mu.Lock()
			applyTrailingStopUpdateResult(state.Strategies[c.StrategyID], c.Symbol, c.Side, c.StopLossOID, 0, true, result, "trailing_stop_loss_immediate", logger)
			mu.Unlock()
			// #1456 review round 6: the audit CREATED this window — it ran the
			// cancel. Retry the placement once in the same cycle instead of
			// leaving a trailing/ATR owner naked until its next due Signal == 0
			// cycle. Nothing rests on the book (that is what this outcome
			// means), so the retry places fresh with nothing to cancel.
			retryResult, retryOutcome := hlLiquidationRetryPlace(c, act.ClampedTriggerPx, logger)
			switch retryOutcome {
			case hlReplacePlaced, hlReplaceFilled:
				if retryOutcome == hlReplaceFilled {
					action = hlLiquidationActionExited
				} else {
					action = hlLiquidationActionClamped
				}
				mu.Lock()
				immediateFill, fillPx := applyTrailingStopUpdateResult(state.Strategies[c.StrategyID], c.Symbol, c.Side, c.StopLossOID, 0, true, retryResult, "liquidation_clamp_sl_immediate", logger)
				mu.Unlock()
				if immediateFill {
					res.ImmediateFills++
					logger.Warn("Liquidation-clamp SL retry booked an immediate close for %s @ $%.4f", c.Symbol, fillPx)
					res.CloseDetails = append(res.CloseDetails, hlLiquidationCloseDetail{
						SC: sc, Symbol: c.Symbol, FillPx: fillPx,
						Detail: fmt.Sprintf("[%s] LIQUIDATION-CLAMP SL %s @ $%.2f", sc.ID, c.Symbol, fillPx),
					})
				}
			default:
				// The retry failed too: one protection-lost report for the
				// cycle, never a duplicate resting stop and never a second
				// alert class.
			}
		case hlReplacePlaced, hlReplaceFilled:
			// A FILL at submit is not a tighten — the position ended the
			// cycle flat, and the operator DM must say so (#1456 review).
			if outcome == hlReplaceFilled {
				action = hlLiquidationActionExited
			}
			mu.Lock()
			ss := state.Strategies[c.StrategyID]
			// applyTrailingStopUpdateResult re-validates side and quantity
			// under the lock and handles all three outcomes (immediate fill,
			// resting replacement, cancel-without-rest). newHighWater=0 leaves
			// StopLossHighWaterPx untouched — the audit never moves a trail.
			// The close reason names the AUDIT, not the trailing walker: the
			// persisted CloseReason must match the LIQUIDATION-CLAMP DM built
			// below (#1456 review).
			immediateFill, fillPx := applyTrailingStopUpdateResult(ss, c.Symbol, c.Side, c.StopLossOID, 0, true, result, "liquidation_clamp_sl_immediate", logger)
			mu.Unlock()
			if immediateFill {
				res.ImmediateFills++
				logger.Warn("Liquidation-clamp SL booked an immediate close for %s @ $%.4f", c.Symbol, fillPx)
				res.CloseDetails = append(res.CloseDetails, hlLiquidationCloseDetail{
					SC: sc, Symbol: c.Symbol, FillPx: fillPx,
					Detail: fmt.Sprintf("[%s] LIQUIDATION-CLAMP SL %s @ $%.2f", sc.ID, c.Symbol, fillPx),
				})
			}
		}
		pending = append(pending, hlLiquidationPendingAlert{
			sc: sc, symbol: c.Symbol, side: c.Side,
			triggerPx: c.StopLossTriggerPx, clampedPx: act.ClampedTriggerPx, liqPx: c.LiquidationPx,
			action: action, logger: logger,
		})
	}

	// Alerts drain after every lock is released.
	for _, p := range pending {
		notifyHLStopPastLiquidation(p.sc, p.symbol, p.side, p.triggerPx, p.clampedPx, p.liqPx, p.action, notifier, p.logger, now)
	}
	return res
}

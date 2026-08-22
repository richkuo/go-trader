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
//   - Percentage owners only (stop_loss_pct, trailing_stop_pct, and the
//     stop_loss_margin_pct / leverage derivation). Every ATR-derived owner
//     needs a per-position EntryATR that does not exist until the position
//     opens, so it can only be checked at arm time by the runtime clamp.
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
	if sc.StopLossPct != nil {
		report("stop_loss_pct", *sc.StopLossPct)
	}
	if sc.TrailingStopPct != nil {
		report("trailing_stop_pct", *sc.TrailingStopPct)
	}
	if sc.StopLossMarginPct != nil && *sc.StopLossMarginPct > 0 {
		report("derived stop-loss price %% (stop_loss_margin_pct / leverage)", *sc.StopLossMarginPct/lev)
	}
	return errs
}

// --- throttled operator alert ---------------------------------------------

// hlLiquidationAlertState is the per-(strategy, symbol) throttle record.
type hlLiquidationAlertState struct {
	Notified       bool
	LastNotifiedAt time.Time
	// ReplaceFailed records whether the LAST notification reported a deferred
	// replace. A transition from "handled" to "deferred" re-notifies
	// immediately: an operator who was told the stop was clamped needs to know
	// when that stops being true.
	ReplaceFailed bool
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
func hlLiquidationShouldNotify(prev hlLiquidationAlertState, replaceFailed bool, now time.Time) (bool, hlLiquidationAlertState) {
	notify := false
	switch {
	case !prev.Notified:
		notify = true // first observation for this (strategy, symbol)
	case replaceFailed && !prev.ReplaceFailed:
		notify = true // the clamp stopped landing — escalate immediately
	case now.Sub(prev.LastNotifiedAt) >= effectiveAlertThrottleInterval():
		notify = true // periodic reminder while the geometry stays impossible
	}
	if !notify {
		return false, prev
	}
	return true, hlLiquidationAlertState{
		Notified:       true,
		LastNotifiedAt: now,
		ReplaceFailed:  replaceFailed,
	}
}

// hlLiquidationAlertDue applies the throttle to the live sync.Map and reports
// whether this observation should be sent.
func hlLiquidationAlertDue(strategyID, symbol string, replaceFailed bool, now time.Time) bool {
	key := hlLiquidationAlertKey(strategyID, symbol)
	var prev hlLiquidationAlertState
	if v, ok := hlLiquidationAlerts.Load(key); ok {
		if s, ok2 := v.(hlLiquidationAlertState); ok2 {
			prev = s
		}
	}
	send, next := hlLiquidationShouldNotify(prev, replaceFailed, now)
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
	// hlLiquidationActionReplaceDeferred — the cancel+replace failed; the OLD
	// (inert but resting) stop is still there and the scheduler will retry.
	hlLiquidationActionReplaceDeferred hlLiquidationAlertAction = "replace deferred"
	// hlLiquidationActionObserved — the owner heals itself on its own path
	// (trailing walker / protection plan); this cycle only reports it.
	hlLiquidationActionObserved hlLiquidationAlertAction = "observed"
)

// notifyHLStopPastLiquidation emits the throttled owner alert. Callers MUST
// invoke it with no state lock held: it sends to the notifier.
func notifyHLStopPastLiquidation(sc StrategyConfig, symbol, side string, triggerPx, clampedPx, liqPx float64, action hlLiquidationAlertAction, notifier *MultiNotifier, logger *StrategyLogger, now time.Time) {
	if !hlLiquidationAlertDue(sc.ID, symbol, action == hlLiquidationActionReplaceDeferred, now) {
		return
	}
	detail := fmt.Sprintf("configured trigger $%.4f is past the exchange liquidation price $%.4f", triggerPx, liqPx)
	switch action {
	case hlLiquidationActionClamped:
		detail += fmt.Sprintf(" — tightened to $%.4f (%.2f%% inside liquidation)", clampedPx, hlLiquidationStopBufferPct)
	case hlLiquidationActionReplaceDeferred:
		detail += fmt.Sprintf(" — could not re-place at $%.4f; the ORIGINAL stop is still resting and the scheduler will retry next cycle", clampedPx)
	case hlLiquidationActionObserved:
		detail += " — the owning stop mechanism will tighten it on its own path this cycle"
	}
	if logger != nil {
		logger.Warn("SL past liquidation for %s (%s): %s", symbol, side, detail)
	}
	if notifier != nil && notifier.HasBackends() {
		msg := fmt.Sprintf("**HL STOP PAST LIQUIDATION** [%s] %s %s — %s. A stop past liquidation can never fill: Hyperliquid force-closes first, at liquidation-engine pricing. Lower the leverage or tighten the stop distance so the configured geometry is reachable.",
			sc.ID, symbol, side, detail)
		notifier.SendToAllChannels(msg)
		notifier.SendOwnerDM(msg)
	}
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
	var wouldBe float64
	switch side {
	case "long":
		wouldBe = anchor - slMult*entryATR
	case "short":
		wouldBe = anchor + slMult*entryATR
	default:
		return slMult, false
	}
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
	newMult := math.Abs(anchor-clamped) / entryATR
	if newMult <= 0 || math.IsNaN(newMult) || math.IsInf(newMult, 0) {
		return slMult, false
	}
	return newMult, true
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
	// placed once at open by a scalar owner with no re-place mechanism, so the
	// audit itself must cancel+replace it. Every other owner heals on its own
	// path this cycle and the audit only reports.
	StaticScalarOwner bool
}

// hlLiquidationAuditAction is one decided unit of work.
type hlLiquidationAuditAction struct {
	Candidate        hlLiquidationAuditCandidate
	ClampedTriggerPx float64
	// Replace is true only for static scalar owners — the audit issues the
	// cancel+replace itself. False means alert-only: the trailing walker or the
	// protection plan tightens this owner on its own path.
	Replace bool
}

// planHyperliquidLiquidationAudit is the PURE audit decision: no locks, no
// globals, no IO. Candidates whose geometry is fine, whose liquidation price is
// unknown, or whose clamp would not move the trigger produce no action.
//
// Output order is deterministic (strategy id, then symbol) so operator output
// and tests are stable.
func planHyperliquidLiquidationAudit(candidates []hlLiquidationAuditCandidate) []hlLiquidationAuditAction {
	var actions []hlLiquidationAuditAction
	for _, c := range candidates {
		if c.Qty <= 0 || c.StopLossTriggerPx <= 0 || c.LiquidationPx <= 0 {
			continue
		}
		clamped, ok := clampStopInsideLiquidation(c.Side, c.StopLossTriggerPx, c.LiquidationPx)
		if !ok {
			continue
		}
		actions = append(actions, hlLiquidationAuditAction{
			Candidate:        c,
			ClampedTriggerPx: clamped,
			Replace:          c.StaticScalarOwner,
		})
	}
	sort.SliceStable(actions, func(i, j int) bool {
		if actions[i].Candidate.StrategyID != actions[j].Candidate.StrategyID {
			return actions[i].Candidate.StrategyID < actions[j].Candidate.StrategyID
		}
		return actions[i].Candidate.Symbol < actions[j].Candidate.Symbol
	})
	return actions
}

// collectHLLiquidationAuditCandidates snapshots every live HL perps/manual
// position with a resting stop, under a single RLock.
//
// Gating on hyperliquidIsLive is load-bearing: a PAPER strategy sharing a coin
// with a live position must never match a map entry — paper has no account and
// no liquidation price of its own.
func collectHLLiquidationAuditCandidates(
	strategies []StrategyConfig,
	state *AppState,
	hlLiquidationPx map[string]float64,
	hlOnChainAbsQty map[string]float64,
	mu *sync.RWMutex,
) []hlLiquidationAuditCandidate {
	if state == nil || len(hlLiquidationPx) == 0 {
		return nil
	}
	var out []hlLiquidationAuditCandidate
	mu.RLock()
	defer mu.RUnlock()
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
			if pos == nil || pos.Quantity <= 0 || pos.StopLossTriggerPx <= 0 {
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
			liqPx := hlLiquidationPx[symbol]
			if liqPx <= 0 {
				continue
			}
			// #621: never place a stop larger than the on-chain size.
			slQty, _ := hlSLEffectiveQty(symbol, pos.Quantity, hlOnChainAbsQty)
			out = append(out, hlLiquidationAuditCandidate{
				StrategyID:        sc.ID,
				Script:            sc.Script,
				Symbol:            symbol,
				Side:              pos.Side,
				Qty:               slQty,
				StopLossOID:       pos.StopLossOID,
				StopLossTriggerPx: pos.StopLossTriggerPx,
				LiquidationPx:     liqPx,
				StaticScalarOwner: !scaleInLiveProtectionResizable(sc),
			})
		}
	}
	return out
}

// hlLiquidationClampReplace runs the cancel+replace for one static-scalar
// candidate and interprets the result.
//
// It deliberately reuses runHyperliquidUpdateStopLossFunc — the SAME placement
// primitive the trailing walker and the open-cycle arm use — so there is only
// one stop-placement implementation that can drift. The result interpretation
// is spelled out here rather than shared with the walker because the walker's
// operator messages are trailing-specific; the failure semantics are identical:
// on any failure the OLD order keeps resting and the caller retries next cycle.
func hlLiquidationClampReplace(candidate hlLiquidationAuditCandidate, clampedTriggerPx float64, logger *StrategyLogger) (*HyperliquidStopLossUpdateResult, bool) {
	if clampedTriggerPx <= 0 || candidate.Qty <= 0 {
		return nil, false
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
		return result, false
	}
	if result == nil {
		if logger != nil {
			logger.Error("Liquidation-clamp SL replace returned no result for %s", candidate.Symbol)
		}
		return nil, false
	}
	if result.Error != "" {
		if logger != nil {
			logger.Error("Liquidation-clamp SL replace returned error for %s: %s", candidate.Symbol, result.Error)
		}
		return result, false
	}
	if result.OpenOrderCheckError != "" {
		if logger != nil {
			logger.Warn("Liquidation-clamp SL replace deferred for %s (open-order lookup failed): %s", candidate.Symbol, result.OpenOrderCheckError)
		}
		return result, false
	}
	if result.StopLossFilledExternally {
		if logger != nil {
			logger.Warn("Liquidation-clamp: SL OID=%d for %s already filled on-chain — reconciler will book the close", candidate.StopLossOID, candidate.Symbol)
		}
		return result, false
	}
	if result.CancelStopLossError != "" {
		if logger != nil {
			logger.Warn("Liquidation-clamp SL cancel failed for %s; original stop still resting: %s", candidate.Symbol, result.CancelStopLossError)
		}
		return result, false
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
	if result.StopLossFilledImmediately && logger != nil {
		// This is protection working: the clamp landed inside the mark, so the
		// position exits NOW at a better price than liquidation would give.
		logger.Warn("Liquidation-clamp SL filled at submit for %s — position exited inside the liquidation price", candidate.Symbol)
	}
	confirmed := (result.StopLossOID > 0) ||
		(result.StopLossFilledImmediately && result.StopLossTriggerPx > 0) ||
		result.CancelStopLossSucceeded
	return result, confirmed
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

// runHyperliquidLiquidationAudit is the per-cycle driver. Sequence:
//
//	RLock snapshot → release → unlocked subprocesses → Lock re-validated apply
//	→ release → drain alerts.
//
// It runs on the main loop goroutine, before per-strategy dispatch, so it
// cannot interleave with a walker cancel+replace on the same OID. It touches
// only static scalar owners; the walker and the protection plan own the rest.
//
// Returns the number of positions that ended the cycle flat because a clamped
// trigger filled at submit (booked as stop-loss closes), for the caller's
// cycle counters.
func runHyperliquidLiquidationAudit(
	strategies []StrategyConfig,
	state *AppState,
	hlLiquidationPx map[string]float64,
	hlOnChainAbsQty map[string]float64,
	mu *sync.RWMutex,
	notifier *MultiNotifier,
	now time.Time,
) int {
	candidates := collectHLLiquidationAuditCandidates(strategies, state, hlLiquidationPx, hlOnChainAbsQty, mu)
	// Clear the throttle for every live candidate whose geometry is now fine,
	// so a recurrence re-alerts on its first cycle instead of being suppressed
	// by a stale key.
	for _, c := range candidates {
		if !stopPastLiquidation(c.Side, c.StopLossTriggerPx, c.LiquidationPx) {
			clearHLLiquidationAlert(c.StrategyID, c.Symbol)
		}
	}
	actions := planHyperliquidLiquidationAudit(candidates)
	if len(actions) == 0 {
		return 0
	}
	byID := make(map[string]StrategyConfig, len(strategies))
	for _, sc := range strategies {
		byID[sc.ID] = sc
	}

	var pending []hlLiquidationPendingAlert
	immediateFills := 0
	for _, act := range actions {
		c := act.Candidate
		sc, ok := byID[c.StrategyID]
		if !ok {
			continue
		}
		logger := &StrategyLogger{stratID: sc.ID, writer: os.Stderr}
		if !act.Replace {
			// The trailing walker / protection plan tightens this owner on its
			// own path this cycle. Report only — a second cancel+replace from
			// here would race that path's OID.
			pending = append(pending, hlLiquidationPendingAlert{
				sc: sc, symbol: c.Symbol, side: c.Side,
				triggerPx: c.StopLossTriggerPx, clampedPx: act.ClampedTriggerPx, liqPx: c.LiquidationPx,
				action: hlLiquidationActionObserved, logger: logger,
			})
			continue
		}
		result, confirmed := hlLiquidationClampReplace(c, act.ClampedTriggerPx, logger)
		action := hlLiquidationActionClamped
		if !confirmed {
			// On ANY failure the original (inert but resting) stop stays where
			// it was — the position never ends the cycle without an
			// exchange-side stop.
			action = hlLiquidationActionReplaceDeferred
		} else {
			mu.Lock()
			ss := state.Strategies[c.StrategyID]
			// applyTrailingStopUpdateResult re-validates side and quantity
			// under the lock and handles all three outcomes (immediate fill,
			// resting replacement, cancel-without-rest). newHighWater=0 leaves
			// StopLossHighWaterPx untouched — a scalar owner has none.
			if immediateFill, fillPx := applyTrailingStopUpdateResult(ss, c.Symbol, c.Side, c.StopLossOID, 0, true, result, logger); immediateFill {
				immediateFills++
				logger.Warn("Liquidation-clamp SL booked an immediate close for %s @ $%.4f", c.Symbol, fillPx)
			}
			mu.Unlock()
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
	return immediateFills
}

package main

import (
	"fmt"
	"time"
)

type manualCloseTPTierClass int

const (
	manualCloseTPTierUntouched manualCloseTPTierClass = iota
	manualCloseTPTierCompleted
	manualCloseTPTierCancelled
	manualCloseTPTierUnconfirmed
)

type manualCloseTPOutcomeKind int

const (
	manualCloseTPSkipped manualCloseTPOutcomeKind = iota
	manualCloseTPRestored
	manualCloseTPPreserved
	manualCloseTPFilledExternally
	manualCloseTPFilledImmediately
	manualCloseTPUnverified
	manualCloseTPMissing
)

type manualCloseTPTierOutcome struct {
	Class   manualCloseTPTierClass
	Kind    manualCloseTPOutcomeKind
	PrevOID int64
	NewOID  int64
	Px      float64
	Detail  string
}

const manualTPRestoreUnverifiedState = "The take-profit state on the exchange is UNVERIFIED, so tiers may be MISSING."

func manualCloseTPSnapshotTierCount(snap manualCloseProtectionSnapshot) int {
	n := len(snap.TPOIDs)
	if len(snap.TPArmedTiers) > n {
		n = len(snap.TPArmedTiers)
	}
	return n
}

func classifyManualCloseTPTiers(tierCount int, tpOIDs []int64, armedTiers []bool, requestedCancelOIDs []int64, execResult *HyperliquidExecuteResult) []manualCloseTPTierClass {
	if tierCount <= 0 {
		return nil
	}
	confirmed := hyperliquidExecuteSucceededCancelOIDs(execResult, requestedCancelOIDs)
	classes := make([]manualCloseTPTierClass, tierCount)
	for i := range classes {
		var oid int64
		if i < len(tpOIDs) {
			oid = tpOIDs[i]
		}
		armed := i < len(armedTiers) && armedTiers[i]
		switch {
		case oid <= 0 && armed:
			classes[i] = manualCloseTPTierCompleted
		case oid <= 0:
			classes[i] = manualCloseTPTierUntouched
		case !containsInt64(requestedCancelOIDs, oid):
			classes[i] = manualCloseTPTierUntouched
		case containsInt64(confirmed, oid):
			classes[i] = manualCloseTPTierCancelled
		default:
			classes[i] = manualCloseTPTierUnconfirmed
		}
	}
	return classes
}

func manualCloseTPTierNeedsRestore(class manualCloseTPTierClass) bool {
	return class == manualCloseTPTierCancelled || class == manualCloseTPTierUnconfirmed
}

func manualCloseTPAffectedOIDs(classes []manualCloseTPTierClass, tpOIDs []int64, from int) []int64 {
	var out []int64
	for i := from; i < len(classes); i++ {
		if !manualCloseTPTierNeedsRestore(classes[i]) || i >= len(tpOIDs) || tpOIDs[i] <= 0 {
			continue
		}
		out = append(out, tpOIDs[i])
	}
	return out
}

func manualCloseTPRestorePlanInputs(plan hlProtectionPlan, classes []manualCloseTPTierClass, tpOIDs []int64, size float64) hlProtectionPlan {
	plan.StopLossATRMult = 0
	plan.StopLossOID = 0
	plan.ForceSLReplace = false
	plan.ForceTPReplace = nil
	plan.CancelTPOIDs = nil
	plan.Size = size
	oids := make([]int64, len(classes))
	armed := make([]bool, len(classes))
	for i, class := range classes {
		switch class {
		case manualCloseTPTierUnconfirmed:
			if i < len(tpOIDs) {
				oids[i] = tpOIDs[i]
			}
			armed[i] = true
		case manualCloseTPTierCancelled:
			oids[i] = 0
			armed[i] = false
		default:
			oids[i] = 0
			armed[i] = true
		}
	}
	plan.TPOIDs = oids
	plan.TPArmedTiers = armed
	return plan
}

func interpretManualCloseTPRestore(classes []manualCloseTPTierClass, prevOIDs []int64, result *HyperliquidProtectionSyncResult, syncErr error) []manualCloseTPTierOutcome {
	outcomes := make([]manualCloseTPTierOutcome, len(classes))
	for i, class := range classes {
		o := manualCloseTPTierOutcome{Class: class, Kind: manualCloseTPSkipped}
		if i < len(prevOIDs) {
			o.PrevOID = prevOIDs[i]
		}
		o.NewOID = o.PrevOID
		if !manualCloseTPTierNeedsRestore(class) {
			outcomes[i] = o
			continue
		}
		switch {
		case syncErr != nil:
			o.Kind = manualCloseTPMissing
			o.NewOID = 0
			o.Detail = syncErr.Error()
		case result == nil:
			o.Kind = manualCloseTPMissing
			o.NewOID = 0
			o.Detail = "the protection sync returned no result"
		case result.Error != "":
			o.Kind = manualCloseTPMissing
			o.NewOID = 0
			o.Detail = result.Error
		default:
			applyManualCloseTPTierResult(&o, i, result)
		}
		outcomes[i] = o
	}
	return outcomes
}

func applyManualCloseTPTierResult(o *manualCloseTPTierOutcome, idx int, result *HyperliquidProtectionSyncResult) {
	if idx < len(result.TPPxs) {
		o.Px = result.TPPxs[idx]
	}
	var placed int64
	if idx < len(result.TPOIDs) {
		placed = result.TPOIDs[idx]
	}
	switch {
	case idx < len(result.TPErrors) && result.TPErrors[idx] != "":
		o.Kind = manualCloseTPMissing
		o.NewOID = 0
		o.Detail = result.TPErrors[idx]
	case o.Class == manualCloseTPTierUnconfirmed && result.OpenOrderCheckError != "":
		o.Kind = manualCloseTPUnverified
		o.NewOID = o.PrevOID
		o.Detail = result.OpenOrderCheckError
	case idx < len(result.TPFilledExternally) && result.TPFilledExternally[idx]:
		o.Kind = manualCloseTPFilledExternally
		o.NewOID = 0
	case idx < len(result.TPFilledImmediately) && result.TPFilledImmediately[idx]:
		o.Kind = manualCloseTPFilledImmediately
		o.NewOID = 0
	case placed > 0 && placed == o.PrevOID:
		o.Kind = manualCloseTPPreserved
		o.NewOID = placed
	case placed > 0:
		o.Kind = manualCloseTPRestored
		o.NewOID = placed
	default:
		o.Kind = manualCloseTPMissing
		o.NewOID = 0
		o.Detail = "the tier did not rest on-chain"
	}
}

func applyRestoredTakeProfitTiers(pos *Position, outcomes []manualCloseTPTierOutcome) {
	if pos == nil || len(outcomes) == 0 {
		return
	}
	if len(pos.TPOIDs) < len(outcomes) {
		pos.TPOIDs = tpOIDsForTierCount(pos.TPOIDs, len(outcomes))
	}
	if len(pos.TPArmedTiers) < len(outcomes) {
		pos.TPArmedTiers = tpArmedTiersForTierCount(pos.TPArmedTiers, len(outcomes))
	}
	for i, o := range outcomes {
		switch o.Kind {
		case manualCloseTPRestored, manualCloseTPPreserved:
			pos.TPOIDs[i] = o.NewOID
			pos.TPArmedTiers[i] = true
		case manualCloseTPFilledExternally, manualCloseTPFilledImmediately:
			pos.TPOIDs[i] = 0
			pos.TPArmedTiers[i] = true
		case manualCloseTPUnverified:
			pos.TPOIDs[i] = o.PrevOID
			pos.TPArmedTiers[i] = o.PrevOID > 0
		case manualCloseTPMissing:
			pos.TPOIDs[i] = 0
			pos.TPArmedTiers[i] = false
		}
	}
}

func restoredTakeProfitBookChanged(outcomes []manualCloseTPTierOutcome) bool {
	for _, o := range outcomes {
		if o.PrevOID != o.NewOID {
			return true
		}
	}
	return false
}

func restoredTakeProfitActionVectors(outcomes []manualCloseTPTierOutcome) ([]int64, []int64, []bool) {
	prev := make([]int64, len(outcomes))
	next := make([]int64, len(outcomes))
	armed := make([]bool, len(outcomes))
	for i, o := range outcomes {
		prev[i] = o.PrevOID
		next[i] = o.NewOID
		switch o.Kind {
		case manualCloseTPRestored, manualCloseTPPreserved,
			manualCloseTPFilledExternally, manualCloseTPFilledImmediately:
			armed[i] = true
		case manualCloseTPUnverified:
			armed[i] = o.PrevOID > 0
		case manualCloseTPMissing:
			armed[i] = false
		default:
			armed[i] = o.PrevOID <= 0
		}
	}
	return prev, next, armed
}

func restoreManualTakeProfitsAfterFailedClose(d manualCoreDeps, res *manualCoreResult, sc StrategyConfig, strategyID string, snap manualCloseProtectionSnapshot, execResult *HyperliquidExecuteResult, requestedCancelOIDs []int64, positionFlat bool) {
	if !hyperliquidPlacesOnChainTPs(sc) {
		return
	}
	snapClasses := classifyManualCloseTPTiers(manualCloseTPSnapshotTierCount(snap), snap.TPOIDs, snap.TPArmedTiers, requestedCancelOIDs, execResult)
	lostOIDs := manualCloseTPAffectedOIDs(snapClasses, snap.TPOIDs, 0)
	if len(lostOIDs) == 0 {
		return
	}

	alertf := func(reason string) {
		msg := fmt.Sprintf("CRITICAL: [%s] %s: the venue rejected the manual close after a cancel of the exchange-side take-profit orders was requested (order ids %v) and the take-profit restore did not complete: %s. %s Verify the open orders on Hyperliquid and restore the tiers or close the position.",
			strategyID, snap.Symbol, lostOIDs, reason, manualTPRestoreUnverifiedState)
		res.outf("%s", msg)
		notifyManualCloseRearmFailure(d.notifier, msg)
	}

	if positionFlat {
		res.outf("manual-close %s %s: the stop-loss leg left the position flat on-chain, so the cancelled take-profit tiers (order ids %v) were not restored — the reconciler books the close at the next scheduler cycle.",
			strategyID, snap.Symbol, lostOIDs)
		return
	}
	if d.syncProtection == nil {
		alertf("no reduce-only take-profit placement path is configured")
		return
	}
	if d.recordRestoredTakeProfits == nil {
		alertf("no take-profit bookkeeping path is configured")
		return
	}
	if d.loadState == nil {
		alertf("no book reader is configured")
		return
	}

	unlockSymbol := lockHyperliquidProtectionSync(snap.Symbol)
	defer unlockSymbol()

	onChainAbsQty, _, netSideByCoin, mapErr := d.hyperliquidAccountMaps()
	if mapErr != nil {
		alertf(fmt.Sprintf("the Hyperliquid account could not be read after the close (%v), so the remaining exposure is unknown and nothing was placed", mapErr))
		return
	}
	if gone, detail := hlPositionGoneForSide(onChainAbsQty, netSideByCoin, snap.Symbol, snap.Side); gone {
		res.outf("manual-close %s %s: %s, so the cancelled take-profit tiers (order ids %v) were not restored — the reconciler books the close at the next scheduler cycle.",
			strategyID, snap.Symbol, detail, lostOIDs)
		return
	}

	view, loadErr := d.loadState(strategyID, snap.Symbol)
	if loadErr != nil {
		alertf(fmt.Sprintf("the book could not be re-read after the close (%v), so nothing was placed", loadErr))
		return
	}
	pos := view.Pos
	if pos == nil {
		res.outf("manual-close %s %s: the position is no longer in the book, so the cancelled take-profit tiers (order ids %v) were not restored.",
			strategyID, snap.Symbol, lostOIDs)
		return
	}
	if !manualPositionOwnedByStrategy(pos, strategyID) || pos.Side != snap.Side || pos.Quantity <= 0 ||
		(snap.PositionID != "" && pos.TradePositionID != "" && pos.TradePositionID != snap.PositionID) {
		res.outf("manual-close %s %s: the book now holds a different position (id %q side %q qty %.6f), so the cancelled take-profit tiers (order ids %v) were not restored — the scheduler protects the replacement.",
			strategyID, snap.Symbol, pos.TradePositionID, pos.Side, pos.Quantity, lostOIDs)
		return
	}

	plan, planOK := buildHyperliquidProtectionPlan(sc, pos, 0)
	if !planOK || len(plan.Tiers) == 0 {
		alertf("the take-profit plan could not be resolved from the position (entry ATR or tier configuration missing), so nothing was placed")
		return
	}
	if dropped := manualCloseTPAffectedOIDs(snapClasses, snap.TPOIDs, len(plan.Tiers)); len(dropped) > 0 {
		alertf(fmt.Sprintf("the strategy now resolves %d take-profit tiers, so the cancelled order ids %v have no tier to restore", len(plan.Tiers), dropped))
	}

	size, capped := hlSLEffectiveQty(snap.Symbol, pos.Quantity, onChainAbsQty)
	if capped {
		res.errf("warning: take-profit restore size for %s capped from the book %.6f to the on-chain %.6f", snap.Symbol, pos.Quantity, size)
	}
	planClasses := classifyManualCloseTPTiers(len(plan.Tiers), snap.TPOIDs, snap.TPArmedTiers, requestedCancelOIDs, execResult)
	prevOIDs := tpOIDsForTierCount(snap.TPOIDs, len(plan.Tiers))
	plan = manualCloseTPRestorePlanInputs(plan, planClasses, snap.TPOIDs, size)

	result, stderr, syncErr := d.syncProtection(sc, plan)
	if stderr != "" {
		res.errf("TP restore stderr: %s", stderr)
	}
	outcomes := interpretManualCloseTPRestore(planClasses, prevOIDs, result, syncErr)
	reportManualCloseTPRestore(res, alertf, strategyID, snap.Symbol, outcomes)

	if recErr := d.recordRestoredTakeProfits(strategyID, snap.Symbol, snap.Side, snap.PositionID, outcomes); recErr != nil {
		msg := fmt.Sprintf("CRITICAL: [%s] %s: the take-profit restore outcome could not be recorded in the book (%v) — the restored order ids %v may be resting untracked. Verify the open orders on Hyperliquid and reconcile before the next close.",
			strategyID, snap.Symbol, recErr, restoredTakeProfitPlacedOIDs(outcomes))
		res.outf("%s", msg)
		notifyManualCloseRearmFailure(d.notifier, msg)
	}
}

func restoredTakeProfitPlacedOIDs(outcomes []manualCloseTPTierOutcome) []int64 {
	var out []int64
	for _, o := range outcomes {
		if o.Kind == manualCloseTPRestored && o.NewOID > 0 {
			out = append(out, o.NewOID)
		}
	}
	return out
}

func reportManualCloseTPRestore(res *manualCoreResult, alertf func(string), strategyID, symbol string, outcomes []manualCloseTPTierOutcome) {
	var failures []string
	for i, o := range outcomes {
		tier := i + 1
		switch o.Kind {
		case manualCloseTPRestored:
			res.outf("Take-profit tier %d restored after the rejected close: %s @ $%.4f (OID=%d, replacing the cancelled OID=%d).",
				tier, symbol, o.Px, o.NewOID, o.PrevOID)
		case manualCloseTPPreserved:
			res.outf("Take-profit tier %d for %s (OID=%d) was verified still resting on-chain — nothing was replaced.",
				tier, symbol, o.PrevOID)
		case manualCloseTPFilledExternally:
			res.outf("Take-profit tier %d for %s (OID=%d) had already filled on-chain, so nothing was placed — the reconciler will book the fill.",
				tier, symbol, o.PrevOID)
		case manualCloseTPFilledImmediately:
			res.outf("The restored take-profit for tier %d of %s filled immediately, so nothing rests — the reconciler will book the fill.",
				tier, symbol)
		case manualCloseTPUnverified:
			failures = append(failures, fmt.Sprintf("tier %d (OID=%d) could not be verified: %s", tier, o.PrevOID, o.Detail))
		case manualCloseTPMissing:
			failures = append(failures, fmt.Sprintf("tier %d (cancelled OID=%d) was not restored: %s", tier, o.PrevOID, o.Detail))
		}
	}
	for _, failure := range failures {
		alertf(failure)
	}
}

func recordRestoredTakeProfitsInDB(cfg *Config, store *StateStore, strategyID, symbol, side, positionID string, outcomes []manualCloseTPTierOutcome) error {
	if store == nil || len(outcomes) == 0 {
		return nil
	}
	state, _, err := LoadStateWithStore(cfg, store)
	if err != nil {
		return err
	}
	strategy := state.Strategies[strategyID]
	if strategy == nil {
		return fmt.Errorf("strategy %q is no longer in the book", strategyID)
	}
	position := strategy.Positions[symbol]
	if position == nil {
		return fmt.Errorf("position %s/%s is no longer in the book", strategyID, symbol)
	}
	if positionID != "" && position.TradePositionID != "" && position.TradePositionID != positionID {
		return fmt.Errorf("position %s/%s was replaced (trade position %q, not %q)", strategyID, symbol, position.TradePositionID, positionID)
	}
	if !restoredTakeProfitBookChanged(outcomes) {
		return nil
	}
	applyRestoredTakeProfitTiers(position, outcomes)
	if err := store.SaveStrategyBook(strategy); err != nil {
		return err
	}
	prev, next, armed := restoredTakeProfitActionVectors(outcomes)
	return store.InsertPendingManualAction(PendingManualAction{
		StrategyID:   strategyID,
		Action:       "restore-tp",
		Symbol:       symbol,
		Side:         side,
		PositionID:   positionID,
		TPOIDs:       next,
		PrevTPOIDs:   prev,
		TPArmedTiers: armed,
		CreatedAt:    time.Now().UTC(),
	})
}

type restoredTPAdoption int

const (
	restoredTPAdoptionNoChange restoredTPAdoption = iota
	restoredTPAdoptionAdopt
	restoredTPAdoptionAlreadyApplied
	restoredTPAdoptionConflict
)

func decideRestoredTPAdoption(prevOID, newOID, currentOID int64, currentArmed bool) restoredTPAdoption {
	if prevOID == newOID {
		return restoredTPAdoptionNoChange
	}
	if currentOID == newOID {
		return restoredTPAdoptionAlreadyApplied
	}
	if currentOID == prevOID {
		return restoredTPAdoptionAdopt
	}
	if currentOID == 0 && !currentArmed {
		return restoredTPAdoptionAdopt
	}
	return restoredTPAdoptionConflict
}

func adoptRestoredTakeProfits(pos *Position, a PendingManualAction) []string {
	if pos == nil {
		return nil
	}
	tierCount := len(a.TPOIDs)
	if len(a.PrevTPOIDs) > tierCount {
		tierCount = len(a.PrevTPOIDs)
	}
	if tierCount == 0 {
		return nil
	}
	if len(pos.TPOIDs) < tierCount {
		pos.TPOIDs = tpOIDsForTierCount(pos.TPOIDs, tierCount)
	}
	if len(pos.TPArmedTiers) < tierCount {
		pos.TPArmedTiers = tpArmedTiersForTierCount(pos.TPArmedTiers, tierCount)
	}
	var conflicts []string
	for i := 0; i < tierCount; i++ {
		var prevOID, newOID int64
		if i < len(a.PrevTPOIDs) {
			prevOID = a.PrevTPOIDs[i]
		}
		if i < len(a.TPOIDs) {
			newOID = a.TPOIDs[i]
		}
		armed := i < len(a.TPArmedTiers) && a.TPArmedTiers[i]
		switch decideRestoredTPAdoption(prevOID, newOID, pos.TPOIDs[i], pos.TPArmedTiers[i]) {
		case restoredTPAdoptionAdopt, restoredTPAdoptionAlreadyApplied:
			pos.TPOIDs[i] = newOID
			pos.TPArmedTiers[i] = armed
		case restoredTPAdoptionConflict:
			conflicts = append(conflicts, fmt.Sprintf("tier %d holds OID=%d, neither the pre-restore OID=%d nor the restored OID=%d", i+1, pos.TPOIDs[i], prevOID, newOID))
		}
	}
	return conflicts
}

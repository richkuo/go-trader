package main

import (
	"fmt"
	"math"
	"strings"
	"sync"
)

const (
	ratchetOutcomeMoved            = "moved"
	ratchetOutcomeUnchanged        = "unchanged"
	ratchetOutcomeNotConfirmed     = "replacement not confirmed; previous stop retained"
	ratchetOutcomeFilledExternally = "previous stop filled on venue; close pending reconcile"
	ratchetOutcomeSamePrice        = "replaced at the same price"
	ratchetOutcomeRequestedUnknown = "requested, outcome unknown"
	ratchetOutcomeUnknownRetained  = "outcome unknown; previous stop retained"
	ratchetOutcomeCancelled        = "cancelled; no replacement resting"
	ratchetOutcomeClosed           = "position closed in this cycle"
	ratchetOutcomePartial          = "remainder open"
	ratchetOutcomeNoStop           = "no stop recorded"
)

type RatchetTriggerAlert struct {
	StrategyID string
	Symbol     string
	Side       string

	TradePositionID string
	OwnerStrategyID string
	PrevQuantity    float64

	TierIdx         int
	TotalTiers      int
	TierATRMultiple float64
	TierTriggerPx   float64

	MarkPrice   float64
	AnchorPrice float64
	EntryATR    float64

	ProfitATR float64
	ProfitUSD float64

	OldTrailMult float64
	NewTrailMult float64

	PrevStopTriggerPx float64
	PrevStopOID       int64

	StopTriggerPx float64
	StopOID       int64
	HighWaterMark float64
	TrailPct      float64
	RemainderQty  float64
	Outcome       string

	HasNextTier         bool
	NextTierATRMultiple float64
	NextTierTrailAfter  float64
	NextTierTriggerPx   float64

	RegimeLabel          string
	PositionRegimeAtOpen string
}

// ratchetStopEvidence is the trailing update's attempt for this alert only.
// It is not stored. The price still comes from the original position's book.
type ratchetStopEvidence struct {
	Ran       bool
	Live      bool
	Confirmed bool
	Result    *HyperliquidStopLossUpdateResult
}

func formatRatchetTriggerAlert(a RatchetTriggerAlert) string {
	side := "long"
	if strings.ToLower(strings.TrimSpace(a.Side)) == "short" {
		side = "short"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "[%s] %s %s — Ratchet Tier %d/%d cleared\n",
		a.StrategyID, a.Symbol, side, a.TierIdx+1, a.TotalTiers)
	fmt.Fprintf(&b, "  Triggered at: %g×ATR ($%.4f) | Mark: $%.4f\n",
		a.TierATRMultiple, a.TierTriggerPx, a.MarkPrice)
	fmt.Fprintf(&b, "  Entry: $%.4f | ATR: $%.4f | Profit: %.2f×ATR (%s)\n",
		a.AnchorPrice, a.EntryATR, a.ProfitATR, formatSignedUSD(a.ProfitUSD))
	fmt.Fprintf(&b, "  Trail tightened: %g×ATR → %g×ATR\n", a.OldTrailMult, a.NewTrailMult)
	fmt.Fprintf(&b, "  %s\n", formatRatchetSLLine(a))
	if a.Outcome != ratchetOutcomeClosed {
		fmt.Fprintf(&b, "  Trail: %g×ATR (%.4f%% of entry anchor) | HWM $%.4f\n",
			a.NewTrailMult, a.TrailPct, a.HighWaterMark)
	}
	if a.HasNextTier {
		fmt.Fprintf(&b, "  Next tier: %g×ATR ($%.4f) → trail tightens to %g×ATR\n",
			a.NextTierATRMultiple, a.NextTierTriggerPx, a.NextTierTrailAfter)
	}
	if regime := strings.TrimSpace(a.RegimeLabel); regime != "" {
		line := fmt.Sprintf("  Regime: %s", regime)
		if posReg := strings.TrimSpace(a.PositionRegimeAtOpen); posReg != "" && posReg != regime {
			line += fmt.Sprintf(" (stamped at open: %s)", posReg)
		}
		b.WriteString(line + "\n")
	}
	return strings.TrimRight(b.String(), "\n")
}

func formatRatchetSLLine(a RatchetTriggerAlert) string {
	switch a.Outcome {
	case ratchetOutcomeClosed:
		return "SL trigger: position closed in this cycle"
	case ratchetOutcomePartial:
		if a.StopTriggerPx > 0 {
			return fmt.Sprintf("SL trigger: $%.4f (remainder open, qty %.6f)", a.StopTriggerPx, a.RemainderQty)
		}
		return fmt.Sprintf("SL trigger: none (remainder open, qty %.6f; recorded protection cleared)", a.RemainderQty)
	case ratchetOutcomeMoved:
		if a.PrevStopTriggerPx > 0 {
			return fmt.Sprintf("SL trigger: $%.4f (moved; was $%.4f)", a.StopTriggerPx, a.PrevStopTriggerPx)
		}
		return fmt.Sprintf("SL trigger: $%.4f (moved)", a.StopTriggerPx)
	case ratchetOutcomeSamePrice:
		return fmt.Sprintf("SL trigger: $%.4f (replaced at the same price)", a.StopTriggerPx)
	case ratchetOutcomeUnchanged:
		return fmt.Sprintf("SL trigger: $%.4f (unchanged)", a.StopTriggerPx)
	case ratchetOutcomeNotConfirmed:
		return fmt.Sprintf("SL trigger: $%.4f (replacement not confirmed; previous stop retained)", a.StopTriggerPx)
	case ratchetOutcomeFilledExternally:
		return fmt.Sprintf("SL trigger: $%.4f (previous stop filled on venue; close pending reconcile)", a.StopTriggerPx)
	case ratchetOutcomeRequestedUnknown:
		return fmt.Sprintf("SL trigger: $%.4f (requested, outcome unknown)", a.StopTriggerPx)
	case ratchetOutcomeUnknownRetained:
		return fmt.Sprintf("SL trigger: $%.4f (outcome unknown; previous stop retained)", a.StopTriggerPx)
	case ratchetOutcomeCancelled:
		return "SL trigger: none (cancelled; no replacement resting)"
	default:
		return "SL trigger: none (no stop recorded)"
	}
}

func notifyRatchetTrigger(sender ownerDMSender, enabled bool, alert *RatchetTriggerAlert) {
	if !enabled || alert == nil || sender == nil || isNilSender(sender) {
		return
	}
	sender.SendOwnerDM(formatRatchetTriggerAlert(*alert))
}

func completeAndNotifyRatchetTrigger(sender ownerDMSender, enabled bool, alert *RatchetTriggerAlert, sc StrategyConfig, state *StrategyState, symbol string, mu *sync.RWMutex, ev ratchetStopEvidence) {
	if !enabled || alert == nil {
		return
	}
	completeRatchetTriggerAlert(alert, sc, state, symbol, mu, ev)
	notifyRatchetTrigger(sender, true, alert)
}

type ratchetBookSnap struct {
	Quantity            float64
	StopLossTriggerPx   float64
	StopLossOID         int64
	StopLossHighWaterPx float64
	TrailPct            float64
}

func completeRatchetTriggerAlert(alert *RatchetTriggerAlert, sc StrategyConfig, state *StrategyState, symbol string, mu *sync.RWMutex, ev ratchetStopEvidence) {
	if alert == nil {
		return
	}
	var snap ratchetBookSnap
	matched := false
	if mu != nil {
		mu.RLock()
	}
	if state != nil {
		if pos := state.Positions[symbol]; ratchetPositionMatches(pos, alert) {
			matched = true
			snap = ratchetBookSnap{
				Quantity:            pos.Quantity,
				StopLossTriggerPx:   pos.StopLossTriggerPx,
				StopLossOID:         pos.StopLossOID,
				StopLossHighWaterPx: pos.StopLossHighWaterPx,
				TrailPct:            effectiveTrailingStopPct(sc, pos),
			}
		}
	}
	if mu != nil {
		mu.RUnlock()
	}
	applyRatchetBookOutcome(alert, snap, matched, ev)
}

func ratchetPositionMatches(pos *Position, alert *RatchetTriggerAlert) bool {
	if pos == nil || alert == nil || pos.Quantity <= 0 {
		return false
	}
	if !strings.EqualFold(strings.TrimSpace(pos.Side), strings.TrimSpace(alert.Side)) {
		return false
	}
	if alert.TradePositionID != "" && pos.TradePositionID != alert.TradePositionID {
		return false
	}
	if alert.OwnerStrategyID != "" && pos.OwnerStrategyID != "" && pos.OwnerStrategyID != alert.OwnerStrategyID {
		return false
	}
	return true
}

func applyRatchetBookOutcome(alert *RatchetTriggerAlert, snap ratchetBookSnap, matched bool, ev ratchetStopEvidence) {
	if !matched || snap.Quantity <= 0 {
		alert.Outcome = ratchetOutcomeClosed
		alert.StopTriggerPx = 0
		alert.StopOID = 0
		alert.RemainderQty = 0
		return
	}
	alert.StopTriggerPx = snap.StopLossTriggerPx
	alert.StopOID = snap.StopLossOID
	alert.HighWaterMark = snap.StopLossHighWaterPx
	alert.TrailPct = snap.TrailPct
	alert.RemainderQty = snap.Quantity
	if alert.PrevQuantity > 0 && snap.Quantity+1e-9 < alert.PrevQuantity {
		alert.Outcome = ratchetOutcomePartial
		return
	}
	if ratchetAttemptUnknownRetained(ev) && ratchetPxEqual(snap.StopLossTriggerPx, alert.PrevStopTriggerPx) && snap.StopLossOID == alert.PrevStopOID {
		alert.Outcome = ratchetOutcomeUnknownRetained
		return
	}
	if ratchetAttemptRequestedUnknown(ev) && snap.StopLossTriggerPx > 0 {
		alert.Outcome = ratchetOutcomeRequestedUnknown
		return
	}
	if ratchetAttemptCancelledBare(ev) && snap.StopLossTriggerPx <= 0 && snap.StopLossOID <= 0 {
		alert.Outcome = ratchetOutcomeCancelled
		return
	}
	if snap.StopLossOID > 0 && snap.StopLossOID != alert.PrevStopOID && snap.StopLossTriggerPx > 0 && ratchetPxEqual(snap.StopLossTriggerPx, alert.PrevStopTriggerPx) {
		alert.Outcome = ratchetOutcomeSamePrice
		return
	}
	if snap.StopLossTriggerPx > 0 && !ratchetPxEqual(snap.StopLossTriggerPx, alert.PrevStopTriggerPx) && (!ev.Live || snap.StopLossOID > 0) {
		alert.Outcome = ratchetOutcomeMoved
		return
	}
	if ratchetAttemptFilledExternally(ev) {
		alert.Outcome = ratchetOutcomeFilledExternally
		return
	}
	if ratchetAttemptUnconfirmedRetained(ev, snap, alert.PrevStopTriggerPx, alert.PrevStopOID) {
		alert.Outcome = ratchetOutcomeNotConfirmed
		return
	}
	if snap.StopLossTriggerPx > 0 && ratchetPxEqual(snap.StopLossTriggerPx, alert.PrevStopTriggerPx) {
		alert.Outcome = ratchetOutcomeUnchanged
		return
	}
	alert.Outcome = ratchetOutcomeNoStop
}

func ratchetAttemptFilledExternally(ev ratchetStopEvidence) bool {
	r := ev.Result
	return ev.Live && r != nil && r.StopLossFilledExternally
}

func ratchetAttemptUnconfirmedRetained(ev ratchetStopEvidence, snap ratchetBookSnap, prevTrigger float64, prevOID int64) bool {
	return ev.Live && ev.Ran && !ev.Confirmed &&
		snap.StopLossTriggerPx > 0 &&
		ratchetPxEqual(snap.StopLossTriggerPx, prevTrigger) &&
		snap.StopLossOID == prevOID
}

func ratchetAttemptUnknownRetained(ev ratchetStopEvidence) bool {
	r := ev.Result
	return ev.Live && r != nil && r.StopLossOutcomeUnknown && r.StopLossOldStillOpen && !r.CancelStopLossSucceeded
}

func ratchetAttemptRequestedUnknown(ev ratchetStopEvidence) bool {
	r := ev.Result
	return ev.Live && r != nil && r.StopLossOutcomeUnknown && !r.StopLossOldStillOpen
}

func ratchetAttemptCancelledBare(ev ratchetStopEvidence) bool {
	r := ev.Result
	return ev.Live && r != nil && r.CancelStopLossSucceeded && !r.StopLossOutcomeUnknown && r.StopLossOID <= 0 && !r.StopLossFilledImmediately
}

func ratchetPxEqual(a, b float64) bool {
	return math.Abs(a-b) <= 1e-9
}

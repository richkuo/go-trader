package main

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// #1455 — reconcile a fire-time model-only circuit-breaker close row against
// the real exchange fill.
//
// When a per-position circuit breaker fires on a live HL perps strategy,
// CheckRisk calls forceCloseAllPositions BEFORE the reduce-only close is
// submitted: the close row carries a mark-derived estimate with
// FeeSourceReconcileAdjustment and an empty exchange_order_id. When the real
// fill arrives on a LATER cycle (runPendingHyperliquidCircuitCloses drain),
// the virtual position is already gone, so applyHyperliquidCircuitCloseFill
// used to take its defensive branch and book a SECOND row with the real OID
// but zero PnL — leaving the estimate permanently in realized_pnl, cash, the
// closed_positions row, and the #1147 diagnostics capture.
//
// The fix corrects the original row instead of adding a second one. The basis
// (quantity / avg cost / side / multiplier) is derived from what is ALREADY
// persisted — the model-only trade row plus its matching closed_positions row
// (same-cycle `now` timestamp) — so the reconciliation survives a restart
// between fire and fill.
//
// Partial fills are first-class (#418): each fill reconciles only ITS slice
// against the estimate's exact linear share, and the SAME trades row carries
// cumulative filled state until the persisted basis quantity is fully covered
// (#1455 review: correcting to the first slice while subtracting the
// full-quantity estimate permanently dropped the residual's PnL). The row's
// Price keeps the mark-derived estimate price and ExchangeOrderID stays empty
// until completion, which is what lets the next drain retry find and extend
// the same row; on the final slice the row takes the cumulative VWAP, the last
// fill's OID, and FeeSourceUserFills. Per-fill dedup rides the existing #954
// strategyHasCloseTradeForOID guard at the top of
// applyHyperliquidCircuitCloseFill, which runs before this branch for every
// fill.
//
// Conventions preserved:
//   - #954 one-fill-one-row: the completed row carries the closing OID, so
//     any replay of that same fill is caught by the duplicate-OID guard.
//   - Gross PnL convention: trades.realized_pnl stays PRE-FEE; cash and
//     closed_positions/diagnostics carry the NET effect (fee subtracted).
//   - Corrupt positions (#1009) are excluded — their rows carry reason
//     circuit_breaker_corrupt, which never matches a caller's close reason.

// modelOnlyCloseCorrection carries everything the DB updater needs to correct
// the persisted rows in ONE transaction. During a partial-fill sequence it
// writes CUMULATIVE state (the values the in-memory row already shows).
type modelOnlyCloseCorrection struct {
	StrategyID  string
	Timestamp   time.Time // identity of the trades row (formatTime key)
	Symbol      string
	PositionID  string // identity of the trade_diagnostics row ("" for hedge legs — never captured)
	ClosedAt    time.Time
	CloseReason string

	FilledQty float64 // cumulative filled quantity (trades.quantity)
	RowPrice  float64 // value written to trades.price: the estimate price while partial, the cumulative VWAP once complete
	VwapPx    float64 // cumulative VWAP — written to closed_positions.close_price / diagnostics.exit_price
	Value     float64 // cumulative filled notional Σ(q_i · px_i)
	CumGross  float64 // cumulative pre-fee gross vs the persisted basis
	CumFee    float64 // cumulative exchange fees
	Complete  bool    // cumulative fills reached the basis quantity
	OID       string  // non-empty exchange order id — stamped only when Complete
	Details   string  // replacement operator-facing detail line
}

// modelOnlyClosedBasis is the accounting basis recovered from the persisted
// closed_positions row.
type modelOnlyClosedBasis struct {
	Quantity   float64
	AvgCost    float64
	Side       string // "long" / "short"
	Multiplier float64
}

// modelOnlyCloseUpdater is the package-level hook for the transactional DB
// correction, set in main() to (*StateDB).ReconcileModelOnlyClose. Nil in
// subcommands/tests without a DB: the in-memory surfaces are still corrected
// and the DB correction is skipped with a stderr warning (the next restart's
// offline backfills aside, this mirrors how tradeRecorder degrades).
var modelOnlyCloseUpdater func(u modelOnlyCloseCorrection) error

// modelOnlyCloseBasisLoader is the hook for recovering the closed_positions
// basis when it is no longer in the in-memory buffer (restart between fire and
// fill). Set in main() to (*StateDB).LoadModelOnlyCloseBasis.
var modelOnlyCloseBasisLoader func(strategyID, symbol string, closedAt time.Time) (*modelOnlyClosedBasis, error)

const modelOnlyDetailMarker = "model-only reconciliation adjustment"

// modelOnlyFillReconciledMarker tags a row that has taken at least one real
// fill slice; its presence switches the row from full-basis-estimate semantics
// to cumulative-filled semantics (see reconcileModelOnlyCloseWithFill).
const modelOnlyFillReconciledMarker = "[fill-reconciled"

// modelOnlyReconcileMaxAge bounds how old an uncorrected model-only close row
// may be for a fill to correct it (#1455 review). A fill is paired with the
// most recent uncorrected row for the symbol; without a bound a row whose CB
// close cleared as alreadyFlat and never filled would stay matchable forever
// and let an unrelated later fill move its cash. 48h covers any realistic
// PendingCircuitClose drain retry window; past it the fill takes the defensive
// zero-PnL branch and the stale estimate stands until manual review.
const modelOnlyReconcileMaxAge = 48 * time.Hour

// findModelOnlyCloseTrade returns the most recent UNCORRECTED model-only close
// trade for symbol in s.TradeHistory within the age bound. Uncorrected means:
// close leg, empty ExchangeOrderID, FeeSourceReconcileAdjustment, and the
// model-only detail marker — which also excludes #1009 corrupt rows (their
// detail says "zero PnL booked" and carries reason circuit_breaker_corrupt).
func findModelOnlyCloseTrade(s *StrategyState, symbol string) *Trade {
	if s == nil {
		return nil
	}
	for i := len(s.TradeHistory) - 1; i >= 0; i-- {
		t := &s.TradeHistory[i]
		if !t.IsClose || t.Symbol != symbol || t.ExchangeOrderID != "" {
			continue
		}
		if t.FeeSource != FeeSourceReconcileAdjustment || !t.PnLGross {
			continue
		}
		if !strings.Contains(t.Details, modelOnlyDetailMarker) {
			continue
		}
		if time.Since(t.Timestamp) > modelOnlyReconcileMaxAge {
			continue
		}
		return t
	}
	return nil
}

// modelOnlyCloseBasisFor recovers the accounting basis for a model-only close
// booked at ts with the given close reason: first the in-memory ClosedPositions
// buffer (same-cycle flush pending), then the persisted closed_positions row
// via the loader hook (covers a restart between the CB fire and the fill).
func modelOnlyCloseBasisFor(s *StrategyState, symbol string, ts time.Time, closeReason string) *modelOnlyClosedBasis {
	for i := len(s.ClosedPositions) - 1; i >= 0; i-- {
		cp := &s.ClosedPositions[i]
		if cp.StrategyID == s.ID && cp.Symbol == symbol && cp.CloseReason == closeReason && cp.ClosedAt.Equal(ts) {
			return &modelOnlyClosedBasis{
				Quantity: cp.Quantity, AvgCost: cp.AvgCost, Side: cp.Side, Multiplier: cp.Multiplier,
			}
		}
	}
	if modelOnlyCloseBasisLoader != nil {
		basis, err := modelOnlyCloseBasisLoader(s.ID, symbol, ts)
		if err == nil && basis != nil {
			return basis
		}
		if err != nil {
			fmt.Printf("[state] WARN: model-only close basis lookup failed for %s %s: %v\n", s.ID, symbol, err)
		}
	}
	return nil
}

// reconcileModelOnlyCloseWithFill corrects a prior fire-time model-only close
// row with the real exchange fill. Called from
// applyHyperliquidCircuitCloseFill's no-virtual-position branch BEFORE the
// defensive zero-PnL row; true means the caller must return (the fill is fully
// accounted for — including the rolled-back-on-persist-failure case, where the
// defensive branch must NOT stamp the fill's OID), false means "no recoverable
// basis — take the defensive branch".
//
// Caller must hold mu (it mutates Cash, DailyPnL, TradeHistory,
// ClosedPositions). The DB correction runs inside the same critical section:
// RecordTrade's eager persist already establishes SQLite writes under mu, and
// the trade/closed_positions/diagnostics corrections must not be observable
// half-applied by the cycle-end save.
func reconcileModelOnlyCloseWithFill(s *StrategyState, symbol string, fillSz, fillPx, fillFee float64, fillOID int64, closeReason string) bool {
	if s == nil || fillSz <= 0 || fillPx <= 0 || fillOID <= 0 {
		return false
	}
	if closeReason == "" {
		closeReason = "circuit_breaker"
	}
	t := findModelOnlyCloseTrade(s, symbol)
	if t == nil {
		return false
	}
	basis := modelOnlyCloseBasisFor(s, symbol, t.Timestamp, closeReason)
	if basis == nil || basis.Quantity <= 0 || basis.AvgCost <= 0 || basis.Multiplier <= 0 {
		// A non-positive multiplier has no qty*mult*price-delta meaning and a
		// corrupt basis would book meaningless PnL — refuse both (#1455 review
		// case c); the caller falls through to the zero-PnL defensive branch.
		return false
	}

	// Untouched rows carry FULL-basis estimate semantics: Quantity = basis
	// quantity, Price = mark-derived estimate price, RealizedPnL =
	// full-quantity gross, zero fee, empty OID. After the first partial fill
	// the same row carries CUMULATIVE state flagged by the fill-reconciled
	// marker in Details.
	touched := strings.Contains(t.Details, modelOnlyFillReconciledMarker)
	filledBefore, cumGrossBefore, cumFeeBefore, notionalBefore := 0.0, 0.0, 0.0, 0.0
	if touched {
		filledBefore = t.Quantity
		cumGrossBefore = t.RealizedPnL
		cumFeeBefore = t.ExchangeFee
		notionalBefore = t.Value
	}

	qty := fillSz
	if remaining := basis.Quantity - filledBefore; qty > remaining {
		qty = remaining
	}
	if qty <= 1e-9 {
		// Every unit of the persisted basis is already reconciled; exposure
		// beyond it is genuinely unexplained — defensive branch.
		return false
	}
	feeShare := fillFee
	if fillSz > qty+1e-9 {
		feeShare = fillFee * qty / fillSz // clamped over-fill: only the covered slice's fee
	}
	dirSign := 1.0
	if basis.Side == "short" {
		dirSign = -1.0
	}
	unit := basis.Multiplier * dirSign
	gross := qty * unit * (fillPx - basis.AvgCost)

	// The estimate price stays in trades.price for the whole partial sequence,
	// so each fill's exact linear share of the fire-time estimate is
	// recoverable without extra state (the estimate is linear in quantity):
	// subtracting only the filled slice's share keeps the residual estimate in
	// cash for the residual fills to consume.
	estPx := t.Price
	estSliceGross := qty * unit * (estPx - basis.AvgCost)
	deltaNet := gross - feeShare - estSliceGross

	filledAfter := filledBefore + qty
	cumGross := cumGrossBefore + gross
	cumFee := cumFeeBefore + feeShare
	notional := notionalBefore + qty*fillPx
	vwap := notional / filledAfter
	complete := filledAfter >= basis.Quantity-max(1e-9, basis.Quantity*1e-6)

	label := hyperliquidOnChainCloseTradeLabel(closeReason)
	var details, oidStr string
	if complete {
		oidStr = strconv.FormatInt(fillOID, 10)
		details = fmt.Sprintf("%s [fill-reconciled], PnL: $%.2f gross (fee $%.4f) (%s)", label, cumGross, cumFee, modelOnlyDetailMarker)
	} else {
		details = fmt.Sprintf("%s [fill-reconciled partial %.6f/%.6f], PnL so far: $%.2f gross (fee $%.4f) (%s)", label, filledAfter, basis.Quantity, cumGross, cumFee, modelOnlyDetailMarker)
	}

	// Snapshot everything the mutation touches so a persist failure can roll
	// the in-memory surfaces back and leave them consistent with the
	// still-uncorrected DB rows (#1455 review finding 2).
	snapTrade := *t
	snapCash := s.Cash
	snapDaily, snapDailyDate := s.RiskState.DailyPnL, s.RiskState.DailyPnLDate
	snapStreak := s.RiskState.ConsecutiveLosses
	cpIdx := -1
	for i := len(s.ClosedPositions) - 1; i >= 0; i-- {
		cp := &s.ClosedPositions[i]
		if cp.StrategyID == s.ID && cp.Symbol == symbol && cp.CloseReason == closeReason && cp.ClosedAt.Equal(t.Timestamp) {
			cpIdx = i
			break
		}
	}
	var snapCP ClosedPosition
	if cpIdx >= 0 {
		snapCP = s.ClosedPositions[cpIdx]
	}

	// In-memory surfaces first (all corrected even when no DB is wired).
	t.Quantity = filledAfter
	t.Value = notional
	t.ExchangeFee = cumFee
	t.RealizedPnL = cumGross
	t.Details = details
	if complete {
		t.Price = vwap
		t.ExchangeOrderID = oidStr
		t.FeeSource = FeeSourceUserFills
	}
	s.Cash += deltaNet
	// A correction adjusts the daily aggregate by the delta only, and ONLY
	// when the fire landed on today's UTC day (#1455 review): a next-day
	// correction must not alter a different day's meter — daily_loss.go sums
	// today's DailyPnL, so posting yesterday's true-up here could inflate
	// today's headroom against daily_max_loss_usd. Cash is adjusted either way.
	fireDay := t.Timestamp.UTC().Format("2006-01-02")
	if fireDay == time.Now().UTC().Format("2006-01-02") {
		rolloverDailyPnL(&s.RiskState)
		s.RiskState.DailyPnL += deltaNet
	}
	// The loss STREAK was decided at fire time off the estimate's sign;
	// re-litigate ONLY on a sign transition between the estimate and the
	// cumulative realized net (#1455 review optional 5) — decrementing when a
	// booked loss turns out a win, incrementing on the inverse — so later
	// fills of the same position never double-count the round trip. Hedge
	// legs were already streak-excluded at fire time (#1159).
	if t.TradeType != hedgeTradeType {
		estLoss := unit*(estPx-basis.AvgCost) < 0
		prevGross := cumGrossBefore
		if !touched {
			// An untouched row's streak-relevant prior state is the FULL
			// fire-time estimate (what RecordTradeResult booked at fire), not
			// zero — otherwise a first fill that flips the sign could never
			// undo the fire-time streak move.
			prevGross = basis.Quantity * unit * (estPx - basis.AvgCost)
		}
		prevNet := prevGross - cumFeeBefore
		newNet := cumGross - cumFee
		if estLoss && prevNet < 0 && newNet >= 0 && s.RiskState.ConsecutiveLosses > 0 {
			s.RiskState.ConsecutiveLosses--
		} else if !estLoss && prevNet >= 0 && newNet < 0 {
			s.RiskState.ConsecutiveLosses++
		}
	}
	if cpIdx >= 0 {
		cp := &s.ClosedPositions[cpIdx]
		cp.ClosePrice = vwap
		cp.RealizedPnL = cumGross - cumFee
	}

	if modelOnlyCloseUpdater != nil {
		u := modelOnlyCloseCorrection{
			StrategyID:  s.ID,
			Timestamp:   t.Timestamp,
			Symbol:      symbol,
			PositionID:  t.PositionID,
			ClosedAt:    t.Timestamp,
			CloseReason: closeReason,
			FilledQty:   filledAfter,
			RowPrice:    t.Price,
			VwapPx:      vwap,
			Value:       notional,
			CumGross:    cumGross,
			CumFee:      cumFee,
			Complete:    complete,
			OID:         oidStr,
			Details:     details,
		}
		if err := modelOnlyCloseUpdater(u); err != nil {
			*t = snapTrade
			s.Cash = snapCash
			s.RiskState.DailyPnL, s.RiskState.DailyPnLDate = snapDaily, snapDailyDate
			s.RiskState.ConsecutiveLosses = snapStreak
			if cpIdx >= 0 {
				s.ClosedPositions[cpIdx] = snapCP
			}
			msg := fmt.Sprintf("model-only close reconciliation persist failed for %s %s: %v — correction rolled back; retries on the next observation of this fill", s.ID, symbol, err)
			fmt.Printf("[state] WARN: %s\n", msg)
			if tradePersistWarn != nil {
				tradePersistWarn(msg)
			}
			// Handled: returning true keeps the caller OFF the defensive
			// branch, whose row would stamp this fill's OID and make the
			// uncorrected model row permanently unreconcilable.
			return true
		}
	}
	fmt.Printf("[hl-sync] %s/%s: reconciled model-only %s close with fill oid=%d qty=%.6f/%.6f px=%.6f fee=%.6f cum_gross=%.2f cash_delta=%.2f complete=%v (#1455)\n",
		s.ID, symbol, closeReason, fillOID, filledAfter, basis.Quantity, fillPx, fillFee, cumGross, deltaNet, complete)
	return true
}

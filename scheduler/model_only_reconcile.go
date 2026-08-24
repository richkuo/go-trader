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
// between fire and fill. No new carried store: the pending-close retry owner
// (RiskState.PendingCircuitClose + the kill-switch latch) is unchanged, and
// the fire path that sets NO pending entry at all (CB firing on a failed-fetch
// cycle) is covered because the lookup never reads pending state.
//
// Conventions preserved:
//   - #954 one-fill-one-row: the corrected trade row takes the fill's OID, so
//     the duplicate-OID guard catches any replay of the same fill.
//   - Gross PnL convention: trades.realized_pnl stays PRE-FEE; cash and
//     closed_positions/diagnostics carry the NET effect (fee subtracted).
//   - Corrupt positions (#1009) are excluded — their rows are deliberately
//     zero-PnL with no AvgCost basis, and inventing one would be worse than
//     the estimate.

// modelOnlyCloseCorrection carries everything the DB updater needs to correct
// the persisted rows in ONE transaction.
type modelOnlyCloseCorrection struct {
	StrategyID string
	Timestamp  time.Time // identity of the trades row (formatTime key)
	Symbol     string
	PositionID string // identity of the trade_diagnostics row ("" for hedge legs — never captured)
	ClosedAt   time.Time

	Price    float64 // fill price
	Value    float64 // Quantity * fill price
	GrossPnL float64 // pre-fee, from the persisted basis vs the fill price
	Fee      float64 // real exchange fee
	OID      string  // non-empty exchange order id
	Details  string  // replacement operator-facing detail line
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

// findModelOnlyCloseTrade returns the most recent UNCORRECTED model-only close
// trade for symbol in s.TradeHistory. Uncorrected means: close leg, empty
// ExchangeOrderID, FeeSourceReconcileAdjustment, and the model-only detail
// marker — which also excludes #1009 corrupt rows (their detail says "zero PnL
// booked" and carries reason circuit_breaker_corrupt).
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
		return t
	}
	return nil
}

// modelOnlyCloseBasisFor recovers the accounting basis for a model-only close
// booked at ts: first the in-memory ClosedPositions buffer (same-cycle flush
// pending), then the persisted closed_positions row via the loader hook
// (covers a restart between the CB fire and the fill).
func modelOnlyCloseBasisFor(s *StrategyState, symbol string, ts time.Time) *modelOnlyClosedBasis {
	for i := len(s.ClosedPositions) - 1; i >= 0; i-- {
		cp := &s.ClosedPositions[i]
		if cp.StrategyID == s.ID && cp.Symbol == symbol && cp.CloseReason == "circuit_breaker" && cp.ClosedAt.Equal(ts) {
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

// reconcileModelOnlyCloseWithFill corrects a prior fire-time model-only
// circuit-breaker close row with the real exchange fill. Called from
// applyHyperliquidCircuitCloseFill's no-virtual-position branch BEFORE the
// defensive zero-PnL row; true means the caller must return (the fill is fully
// accounted for), false means "no recoverable basis — take the defensive
// branch".
//
// Caller must hold mu (it mutates Cash, DailyPnL, TradeHistory,
// ClosedPositions). The DB correction runs inside the same critical section:
// RecordTrade's eager persist already establishes SQLite writes under mu, and
// the trade/closed_positions/diagnostics corrections must not be observable
// half-applied by the cycle-end save.
func reconcileModelOnlyCloseWithFill(s *StrategyState, symbol string, fillSz, fillPx, fillFee float64, fillOID int64) bool {
	if s == nil || fillSz <= 0 || fillPx <= 0 || fillOID <= 0 {
		return false
	}
	t := findModelOnlyCloseTrade(s, symbol)
	if t == nil {
		return false
	}
	basis := modelOnlyCloseBasisFor(s, symbol, t.Timestamp)
	if basis == nil || basis.Quantity <= 0 || basis.AvgCost <= 0 {
		return false
	}

	// A partial first fill reconciles only the quantity actually closed; the
	// residual keeps retrying under PendingCircuitClose and books its own rows.
	qtyEff := basis.Quantity
	if fillSz < qtyEff {
		qtyEff = fillSz
	}
	var gross float64
	if basis.Side == "short" {
		gross = qtyEff * basis.Multiplier * (basis.AvgCost - fillPx)
	} else {
		gross = qtyEff * basis.Multiplier * (fillPx - basis.AvgCost)
	}
	net := gross - fillFee
	oldNet := t.RealizedPnL // model row: PnLGross with zero fee, so cash saw exactly this
	delta := net - oldNet

	positionID := t.PositionID

	oidStr := strconv.FormatInt(fillOID, 10)
	details := fmt.Sprintf("%s [fill-reconciled], PnL: $%.2f gross (fee $%.4f)",
		hyperliquidOnChainCloseTradeLabel("circuit_breaker"), gross, fillFee)

	// In-memory surfaces first (all corrected even when no DB is wired).
	t.Price = fillPx
	t.Value = t.Quantity * fillPx
	t.ExchangeOrderID = oidStr
	t.ExchangeFee = fillFee
	t.FeeSource = FeeSourceUserFills
	t.RealizedPnL = gross
	t.Details = details
	s.Cash += delta
	// A correction adjusts the daily aggregate by the delta only — the loss
	// STREAK was decided at fire time and re-litigating it off the corrected
	// number would double-count the round trip either way. Hedge legs were
	// already streak-excluded at fire time (#1159); the daily aggregate
	// includes both.
	rolloverDailyPnL(&s.RiskState)
	s.RiskState.DailyPnL += delta
	for i := len(s.ClosedPositions) - 1; i >= 0; i-- {
		cp := &s.ClosedPositions[i]
		if cp.StrategyID == s.ID && cp.Symbol == symbol && cp.CloseReason == "circuit_breaker" && cp.ClosedAt.Equal(t.Timestamp) {
			cp.ClosePrice = fillPx
			cp.RealizedPnL = net
			break
		}
	}

	if modelOnlyCloseUpdater != nil {
		u := modelOnlyCloseCorrection{
			StrategyID: s.ID,
			Timestamp:  t.Timestamp,
			Symbol:     symbol,
			PositionID: positionID,
			ClosedAt:   t.Timestamp,
			Price:      fillPx,
			Value:      t.Quantity * fillPx,
			GrossPnL:   gross,
			Fee:        fillFee,
			OID:        oidStr,
			Details:    details,
		}
		if err := modelOnlyCloseUpdater(u); err != nil {
			fmt.Printf("[state] WARN: model-only close reconciliation persist failed for %s %s: %v — in-memory state is corrected; the DB rows keep the estimate until the next backfill\n",
				s.ID, symbol, err)
		}
	}
	fmt.Printf("[hl-sync] %s/%s: reconciled model-only circuit-breaker close with fill oid=%s qty=%.6f px=%.6f fee=%.6f gross_pnl=%.2f cash_delta=%.2f (#1455)\n",
		s.ID, symbol, oidStr, qtyEff, fillPx, fillFee, gross, delta)
	return true
}

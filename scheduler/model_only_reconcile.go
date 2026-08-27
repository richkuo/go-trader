package main

import (
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"
)

type modelOnlyCloseCorrection struct {
	StrategyID  string
	Timestamp   time.Time
	Symbol      string
	PositionID  string
	ClosedAt    time.Time
	CloseReason string

	FilledQty float64
	RowPrice  float64
	VwapPx    float64
	Value     float64
	CumGross  float64
	CumFee    float64
	Complete  bool
	OID       string
	Details   string
}

type modelOnlyClosedBasis struct {
	Quantity   float64
	AvgCost    float64
	Side       string
	Multiplier float64
}

var modelOnlyCloseUpdater func(u modelOnlyCloseCorrection) error

var modelOnlyCloseBasisLoader func(strategyID, symbol, closeReason string, closedAt time.Time) (*modelOnlyClosedBasis, error)

const modelOnlyDetailMarker = "model-only reconciliation adjustment"

const modelOnlyFillReconciledMarker = "[fill-reconciled"

const modelOnlyReconcileMaxAge = 48 * time.Hour

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
		basis, err := modelOnlyCloseBasisLoader(s.ID, symbol, closeReason, ts)
		if err == nil && basis != nil {
			return basis
		}
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			msg := fmt.Sprintf("model-only close basis lookup FAILED for %s %s (%s): %v — correction degraded to the defensive branch; verify the state database", s.ID, symbol, closeReason, err)
			fmt.Printf("[state] WARN: %s\n", msg)
			if tradePersistWarn != nil {
				tradePersistWarn(msg)
			}
		}
	}
	return nil
}

type modelOnlyReconcileOutcome int

const (
	modelOnlyReconcileNone modelOnlyReconcileOutcome = iota
	modelOnlyReconcileApplied
	modelOnlyReconcilePersistFailed
)

func reconcileModelOnlyCloseWithFill(s *StrategyState, symbol string, fillSz, fillPx, fillFee float64, fillOID int64, closeReason string) modelOnlyReconcileOutcome {
	if s == nil || fillSz <= 0 || fillPx <= 0 || fillOID <= 0 {
		return modelOnlyReconcileNone
	}
	if closeReason == "" {
		closeReason = "circuit_breaker"
	}
	t := findModelOnlyCloseTrade(s, symbol)
	if t == nil {
		return modelOnlyReconcileNone
	}
	basis := modelOnlyCloseBasisFor(s, symbol, t.Timestamp, closeReason)
	if basis == nil || basis.Quantity <= 0 || basis.AvgCost <= 0 || basis.Multiplier <= 0 {
		return modelOnlyReconcileNone
	}

	touched := strings.Contains(t.Details, modelOnlyFillReconciledMarker)
	preStreak := modelOnlyPreStreak(t.Details)
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
		return modelOnlyReconcileNone
	}
	feeShare := fillFee
	if fillSz > qty+1e-9 {
		feeShare = fillFee * qty / fillSz
	}
	dirSign := 1.0
	if basis.Side == "short" {
		dirSign = -1.0
	}
	unit := basis.Multiplier * dirSign
	gross := qty * unit * (fillPx - basis.AvgCost)

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
	var detailsSuffix string
	if preStreak >= 0 {
		detailsSuffix = fmt.Sprintf(", pre-streak=%d", preStreak)
	}
	curOID := strconv.FormatInt(fillOID, 10)
	sliceOIDs := curOID
	if touched {
		prev := modelOnlySliceOIDs(t.Details)
		if len(prev) > 0 {
			seen := false
			for _, o := range prev {
				if o == curOID {
					seen = true
					break
				}
			}
			sliceOIDs = strings.Join(prev, ",")
			if !seen {
				sliceOIDs += "," + curOID
			}
		}
	}
	var details, oidStr string
	detailsBody := modelOnlyDetailMarker + detailsSuffix
	if complete {
		oidStr = curOID
		details = fmt.Sprintf("%s [fill-reconciled oids=%s], PnL: $%.2f gross (fee $%.4f) (%s)", label, sliceOIDs, cumGross, cumFee, detailsBody)
	} else {
		details = fmt.Sprintf("%s [fill-reconciled partial %.6f/%.6f oids=%s], PnL so far: $%.2f gross (fee $%.4f) (%s)", label, filledAfter, basis.Quantity, sliceOIDs, cumGross, cumFee, detailsBody)
	}

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
	fireDay := t.Timestamp.UTC().Format("2006-01-02")
	if fireDay == time.Now().UTC().Format("2006-01-02") {
		rolloverDailyPnL(&s.RiskState)
		s.RiskState.DailyPnL += deltaNet
	}
	if complete && t.TradeType != hedgeTradeType {
		estGross := basis.Quantity * unit * (estPx - basis.AvgCost)
		cumNet := cumGross - cumFee
		switch {
		case estGross < 0 && cumNet >= 0:
			s.RiskState.ConsecutiveLosses = 0
		case estGross >= 0 && cumNet < 0:
			if pre := preStreak; pre >= 0 {
				s.RiskState.ConsecutiveLosses = pre + 1
			} else if s.RiskState.ConsecutiveLosses == 0 {
				s.RiskState.ConsecutiveLosses = 1
			}
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
			msg := fmt.Sprintf("model-only close reconciliation persist failed for %s %s: %v — correction rolled back; the fill is recorded below as a zero-PnL audit row and the persisted estimate stays matchable — run backfill trade-ledger to repair it", s.ID, symbol, err)
			fmt.Printf("[state] WARN: %s\n", msg)
			if tradePersistWarn != nil {
				tradePersistWarn(msg)
			}
			return modelOnlyReconcilePersistFailed
		}
	}
	fmt.Printf("[hl-sync] %s/%s: reconciled model-only %s close with fill oid=%d qty=%.6f/%.6f px=%.6f fee=%.6f cum_gross=%.2f cash_delta=%.2f complete=%v (#1455)\n",
		s.ID, symbol, closeReason, fillOID, filledAfter, basis.Quantity, fillPx, fillFee, cumGross, deltaNet, complete)
	return modelOnlyReconcileApplied
}

const modelOnlyOIDsToken = " oids="

func modelOnlySliceOIDs(details string) []string {
	i := strings.Index(details, modelOnlyOIDsToken)
	if i < 0 {
		return nil
	}
	rest := details[i+len(modelOnlyOIDsToken):]
	if j := strings.IndexByte(rest, ']'); j >= 0 {
		rest = rest[:j]
	}
	var out []string
	for _, p := range strings.Split(rest, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func tradeHasModelOnlySliceOID(t *Trade, oidStr string) bool {
	if t == nil || oidStr == "" || !t.IsClose {
		return false
	}
	for _, o := range modelOnlySliceOIDs(t.Details) {
		if o == oidStr {
			return true
		}
	}
	return false
}

var modelOnlyAbandonedAlerts sync.Map

const modelOnlyAbandonedAlertCooldown = 24 * time.Hour

const modelOnlyAbandonedMarker = "[reconcile-abandoned]"

var modelOnlyCloseAbandonMarker func(strategyID, symbol string, ts time.Time) error

const modelOnlyPreStreakToken = ", pre-streak="

func modelOnlyPreStreak(details string) int {
	i := strings.LastIndex(details, modelOnlyPreStreakToken)
	if i < 0 {
		return -1
	}
	rest := details[i+len(modelOnlyPreStreakToken):]
	if j := strings.IndexAny(rest, ", )"); j >= 0 {
		rest = rest[:j]
	}
	n, err := strconv.Atoi(strings.TrimSpace(rest))
	if err != nil || n < 0 {
		return -1
	}
	return n
}

func warnAbandonedPartialModelClose(s *StrategyState, symbol string, now time.Time) string {
	if s == nil {
		return ""
	}
	key := s.ID + "|" + symbol
	t := findModelOnlyCloseTrade(s, symbol)
	if t == nil || t.ExchangeOrderID != "" || !strings.Contains(t.Details, modelOnlyFillReconciledMarker) {
		modelOnlyAbandonedAlerts.Delete(key)
		return ""
	}
	if strings.Contains(t.Details, modelOnlyAbandonedMarker) {
		modelOnlyAbandonedAlerts.Delete(key)
		return ""
	}
	if v, ok := modelOnlyAbandonedAlerts.Load(key); ok {
		if last, ok2 := v.(time.Time); ok2 && now.Sub(last) < modelOnlyAbandonedAlertCooldown {
			return ""
		}
	}
	modelOnlyAbandonedAlerts.Store(key, now)
	t.Details += " " + modelOnlyAbandonedMarker
	if modelOnlyCloseAbandonMarker != nil {
		if err := modelOnlyCloseAbandonMarker(s.ID, symbol, t.Timestamp); err != nil {
			fmt.Printf("[state] WARN: model-only close abandonment mark failed for %s %s: %v\n", s.ID, symbol, err)
		}
	}
	return fmt.Sprintf("model-only close reconciliation ABANDONED for %s %s: partial row covers %.6f (fee $%.4f) but the coin is flat on-chain — the residual was finished by another mechanism (e.g. a resting stop). The trade row, closed_positions and cash are inconsistent; run backfill trade-ledger or reconcile manually.",
		s.ID, symbol, t.Quantity, t.ExchangeFee)
}

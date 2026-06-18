package main

import (
	"database/sql"
	"fmt"
	"strings"
)

// #954 gross PnL convention helpers.
//
// trades.realized_pnl carries two conventions, disambiguated by pnl_gross:
//
//   - pnl_gross=1 (rows written at/after #954, or migrated by
//     `backfill trade-ledger`): realized_pnl is the PRE-FEE close PnL (or the
//     funding amount for trade_type='funding' rows; 0 for opens) and
//     exchange_fee is ALWAYS the fee deducted from cash — real or modeled,
//     fee_source says which.
//   - pnl_gross=0 (legacy): realized_pnl is NET of the fee that was deducted,
//     and exchange_fee is stamped only when a real fill fee was captured.
//
// Every consumer must read through these helpers; summing realized_pnl raw
// double-counts fees on gross rows (the #698 gross-vs-net trap, generalized).

// TradeTypeFunding marks exchange funding payments booked into the trades
// ledger (#954). Funding rows are not round-trips: they are excluded from
// the lifetime open count alongside scale_in legs, and their is_close=0
// keeps them out of W/L grouping. They DO contribute to a strategy's ledger
// PnL via tradeLedgerDeltaSQL.
const TradeTypeFunding = "funding"

// FeeSourceUserFills / FeeSourceModeled / FeeSourceReconcileAdjustment are the
// Trade.FeeSource values.
const (
	FeeSourceUserFills           = "userfills"
	FeeSourceModeled             = "modeled"
	FeeSourceReconcileAdjustment = "reconcile_adjustment"
)

// tradeNetPnLSQL is the convention-aware NET realized PnL of one row,
// matching legacy semantics exactly: what the trade contributed to cash on a
// close leg. Only meaningful on is_close=1 (and funding) rows — opens yield
// -exchange_fee on gross rows, use tradeLedgerDeltaSQL for value sums.
const tradeNetPnLSQL = "(CASE WHEN COALESCE(pnl_gross, 0) = 1 THEN realized_pnl - exchange_fee ELSE realized_pnl END)"

// tradeLedgerDeltaSQL is one row's contribution to a strategy's cash-equivalent
// ledger sum: close legs contribute net PnL, open legs contribute the fee paid
// (negative), funding rows contribute the funding amount. Legacy open rows
// contribute -exchange_fee, which understates paper/modeled open fees (stamped
// 0 pre-#954) until `backfill trade-ledger` migrates them.
const tradeLedgerDeltaSQL = "(CASE WHEN COALESCE(pnl_gross, 0) = 1 THEN realized_pnl - exchange_fee WHEN is_close = 1 THEN realized_pnl ELSE -exchange_fee END)"

// tradeNetPnL is the Go-side mirror of tradeNetPnLSQL.
func tradeNetPnL(t Trade) float64 {
	if t.PnLGross {
		return t.RealizedPnL - t.ExchangeFee
	}
	return t.RealizedPnL
}

// tradeLedgerDelta is the Go-side mirror of tradeLedgerDeltaSQL.
func tradeLedgerDelta(t Trade) float64 {
	if t.PnLGross {
		return t.RealizedPnL - t.ExchangeFee
	}
	if t.IsClose {
		return t.RealizedPnL
	}
	return -t.ExchangeFee
}

// tradeBackfillRowNetPnL mirrors tradeNetPnL for backfill planner rows.
func tradeBackfillRowNetPnL(t TradeBackfillRow) float64 {
	if t.PnLGross {
		return t.RealizedPnL - t.ExchangeFee
	}
	return t.RealizedPnL
}

// LedgerNetByStrategy returns each strategy's total trades-ledger delta:
// Σ tradeLedgerDeltaSQL across all of its rows (close net PnL − open fees +
// funding). This is the trade-derived PnL component of the #954 shared-wallet
// display value: display_i = initial_capital_i + ledger_i + owned_uPnL_i.
// Strategies with no rows are absent from the map (treat as 0).
func (sdb *StateDB) LedgerNetByStrategy(strategyIDs []string) (map[string]float64, error) {
	out := make(map[string]float64, len(strategyIDs))
	if sdb == nil || sdb.db == nil {
		return nil, fmt.Errorf("state db unavailable")
	}
	if len(strategyIDs) == 0 {
		return out, nil
	}
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(strategyIDs)), ",")
	args := make([]interface{}, len(strategyIDs))
	for i, id := range strategyIDs {
		args[i] = id
	}
	rows, err := sdb.db.Query(
		`SELECT strategy_id, SUM`+tradeLedgerDeltaSQL+` FROM trades WHERE strategy_id IN (`+placeholders+`) GROUP BY strategy_id`,
		args...)
	if err != nil {
		return nil, fmt.Errorf("ledger sums: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		var sum sql.NullFloat64
		if err := rows.Scan(&id, &sum); err != nil {
			return nil, fmt.Errorf("scan ledger sum: %w", err)
		}
		out[id] = sum.Float64
	}
	return out, rows.Err()
}

// HasTradeWithExchangeOrderID reports whether a strategy already has a
// persisted trades row with the given exchange_order_id. Used to dedupe
// funding-event re-ingestion across watermark overlaps/restarts (#954).
func (sdb *StateDB) HasTradeWithExchangeOrderID(strategyID, exchangeOrderID string) (bool, error) {
	if sdb == nil || sdb.db == nil {
		return false, fmt.Errorf("state db unavailable")
	}
	if exchangeOrderID == "" {
		return false, nil
	}
	var n int
	err := sdb.db.QueryRow(
		`SELECT EXISTS(SELECT 1 FROM trades WHERE strategy_id = ? AND exchange_order_id = ?)`,
		strategyID, exchangeOrderID).Scan(&n)
	if err != nil {
		return false, fmt.Errorf("trade oid existence: %w", err)
	}
	return n != 0, nil
}

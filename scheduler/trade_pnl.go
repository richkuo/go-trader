package main

import (
	"database/sql"
	"fmt"
	"strings"
)


const TradeTypeFunding = "funding"

const (
	FeeSourceUserFills           = "userfills"
	FeeSourceModeled             = "modeled"
	FeeSourceReconcileAdjustment = "reconcile_adjustment"
)

const tradeNetPnLSQL = "(CASE WHEN COALESCE(pnl_gross, 0) = 1 THEN realized_pnl - exchange_fee ELSE realized_pnl END)"

const tradeLedgerDeltaSQL = "(CASE WHEN COALESCE(pnl_gross, 0) = 1 THEN realized_pnl - exchange_fee WHEN is_close = 1 THEN realized_pnl ELSE -exchange_fee END)"

func tradeNetPnL(t Trade) float64 {
	if t.PnLGross {
		return t.RealizedPnL - t.ExchangeFee
	}
	return t.RealizedPnL
}

func tradeLedgerDelta(t Trade) float64 {
	if t.PnLGross {
		return t.RealizedPnL - t.ExchangeFee
	}
	if t.IsClose {
		return t.RealizedPnL
	}
	return -t.ExchangeFee
}

func tradeBackfillRowNetPnL(t TradeBackfillRow) float64 {
	if t.PnLGross {
		return t.RealizedPnL - t.ExchangeFee
	}
	return t.RealizedPnL
}

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

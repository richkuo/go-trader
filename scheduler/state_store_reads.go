package main

import (
	"fmt"
	"sort"
	"time"
)

// A combined read queries every required file, translates identifiers, merges,
// then orders and pages globally. A file that cannot be read fails the whole
// read: no page and no map is ever returned with a scope missing.

func (st *StateStore) stampScope(role storageRole) PortfolioScope {
	return st.scopeForRole(role)
}

// tradeSortKey orders merged rows by (timestamp DESC, scope ASC, rowid DESC) so
// equal timestamps and equal row identifiers in two files still produce one
// deterministic page.
func tradeSortLess(a, b Trade) bool {
	if !a.Timestamp.Equal(b.Timestamp) {
		return a.Timestamp.After(b.Timestamp)
	}
	if a.SourceScope != b.SourceScope {
		return a.SourceScope < b.SourceScope
	}
	if a.sourceRole != b.sourceRole {
		return a.sourceRole < b.sourceRole
	}
	return a.sourceRowID > b.sourceRowID
}

func (st *StateStore) RecentTrades(since time.Time, limit int) ([]Trade, error) {
	if st == nil {
		return nil, fmt.Errorf("state store unavailable")
	}
	if !st.layout.Split {
		return st.primary().RecentTrades(since, limit)
	}
	var all []Trade
	for _, role := range st.order {
		rows, err := st.file(role).RecentTrades(since, limit)
		if err != nil {
			return nil, fmt.Errorf("recent trades (%s): %w", role, err)
		}
		scope := st.stampScope(role)
		for i := range rows {
			rows[i].SourceScope = scope
		}
		all = append(all, rows...)
	}
	sort.SliceStable(all, func(i, j int) bool { return tradeSortLess(all[i], all[j]) })
	if limit > 0 && len(all) > limit {
		all = all[:limit]
	}
	return all, nil
}

func (st *StateStore) RecentTradesForStrategy(strategyID string, limit int) ([]Trade, error) {
	db, err := st.dbForStrategy(strategyID)
	if err != nil {
		return nil, err
	}
	return db.RecentTradesForStrategy(strategyID, limit)
}

func (st *StateStore) LifetimeTradeStatsAll() (map[string]LifetimeTradeStats, error) {
	if st == nil {
		return nil, fmt.Errorf("state store unavailable")
	}
	if !st.layout.Split {
		return st.primary().LifetimeTradeStatsAll()
	}
	out := make(map[string]LifetimeTradeStats)
	for _, role := range st.order {
		rows, err := st.file(role).LifetimeTradeStatsAll()
		if err != nil {
			return nil, fmt.Errorf("lifetime trade stats (%s): %w", role, err)
		}
		for id, stats := range rows {
			if !st.rowBelongsToRole(id, role) {
				continue
			}
			out[id] = stats
		}
	}
	return out, nil
}

// rowBelongsToRole keeps a historical row that kept its stored identifier from
// overwriting a configured strategy of the same name in the other file.
func (st *StateStore) rowBelongsToRole(id string, role storageRole) bool {
	ident, ok := st.ident.storageFor(id)
	if !ok {
		return true
	}
	return ident.Role == role
}

func (st *StateStore) LifetimeTradeStatsForStrategy(strategyID string) (LifetimeTradeStats, error) {
	db, err := st.dbForStrategy(strategyID)
	if err != nil {
		return LifetimeTradeStats{}, err
	}
	return db.LifetimeTradeStatsForStrategy(strategyID)
}

func (st *StateStore) QueryTradeHistory(strategyID, symbol string, since, until time.Time, limit, offset int) ([]Trade, int, error) {
	if st == nil {
		return nil, 0, fmt.Errorf("state store unavailable")
	}
	if !st.layout.Split {
		return st.primary().QueryTradeHistory(strategyID, symbol, since, until, limit, offset)
	}
	if strategyID != "" {
		db, err := st.dbForStrategy(strategyID)
		if err != nil {
			return nil, 0, err
		}
		trades, total, err := db.QueryTradeHistory(strategyID, symbol, since, until, limit, offset)
		if err != nil {
			return nil, 0, err
		}
		scope := st.stampScope(db.storageRoleOf())
		for i := range trades {
			trades[i].SourceScope = scope
		}
		return trades, total, nil
	}
	if limit <= 0 {
		limit = 50
	}
	if limit > 500 {
		limit = 500
	}
	if offset < 0 {
		offset = 0
	}
	window := offset + limit
	var all []Trade
	total := 0
	for _, role := range st.order {
		rows, count, err := st.file(role).QueryTradeHistory("", symbol, since, until, window, 0)
		if err != nil {
			return nil, 0, fmt.Errorf("trade history (%s): %w", role, err)
		}
		total += count
		scope := st.stampScope(role)
		for i := range rows {
			rows[i].SourceScope = scope
		}
		all = append(all, rows...)
	}
	sort.SliceStable(all, func(i, j int) bool { return tradeSortLess(all[i], all[j]) })
	if offset >= len(all) {
		return []Trade{}, total, nil
	}
	end := offset + limit
	if end > len(all) {
		end = len(all)
	}
	page := all[offset:end]
	if page == nil {
		page = []Trade{}
	}
	return page, total, nil
}

func (st *StateStore) QueryTradingViewExportTrades(strategyIDs []string) ([]Trade, error) {
	if st == nil {
		return nil, fmt.Errorf("state store unavailable")
	}
	if !st.layout.Split {
		return st.primary().QueryTradingViewExportTrades(strategyIDs)
	}
	var all []Trade
	for _, role := range st.order {
		owned := st.ownedProcessIDs(strategyIDs, role)
		if len(owned) == 0 {
			continue
		}
		rows, err := st.file(role).QueryTradingViewExportTrades(owned)
		if err != nil {
			return nil, fmt.Errorf("tradingview export (%s): %w", role, err)
		}
		scope := st.stampScope(role)
		for i := range rows {
			rows[i].SourceScope = scope
		}
		all = append(all, rows...)
	}
	sort.SliceStable(all, func(i, j int) bool {
		if !all[i].Timestamp.Equal(all[j].Timestamp) {
			return all[i].Timestamp.Before(all[j].Timestamp)
		}
		if all[i].StrategyID != all[j].StrategyID {
			return all[i].StrategyID < all[j].StrategyID
		}
		if all[i].Symbol != all[j].Symbol {
			return all[i].Symbol < all[j].Symbol
		}
		if all[i].SourceScope != all[j].SourceScope {
			return all[i].SourceScope < all[j].SourceScope
		}
		return all[i].sourceRowID < all[j].sourceRowID
	})
	if all == nil {
		all = []Trade{}
	}
	return all, nil
}

func (st *StateStore) QueryClosedPositions(strategyID, symbol string, since, until time.Time, limit, offset int) ([]ClosedPosition, int, error) {
	if st == nil {
		return nil, 0, fmt.Errorf("state store unavailable")
	}
	if !st.layout.Split || strategyID != "" {
		db := st.primary()
		if strategyID != "" {
			var err error
			db, err = st.dbForStrategy(strategyID)
			if err != nil {
				return nil, 0, err
			}
		}
		return db.QueryClosedPositions(strategyID, symbol, since, until, limit, offset)
	}
	if limit <= 0 {
		limit = 50
	}
	if limit > 500 {
		limit = 500
	}
	if offset < 0 {
		offset = 0
	}
	window := offset + limit
	var all []ClosedPosition
	total := 0
	for _, role := range st.order {
		rows, count, err := st.file(role).QueryClosedPositions("", symbol, since, until, window, 0)
		if err != nil {
			return nil, 0, fmt.Errorf("closed positions (%s): %w", role, err)
		}
		total += count
		all = append(all, rows...)
	}
	sort.SliceStable(all, func(i, j int) bool {
		if !all[i].ClosedAt.Equal(all[j].ClosedAt) {
			return all[i].ClosedAt.After(all[j].ClosedAt)
		}
		if all[i].StrategyID != all[j].StrategyID {
			return all[i].StrategyID < all[j].StrategyID
		}
		return all[i].Symbol < all[j].Symbol
	})
	if offset >= len(all) {
		return []ClosedPosition{}, total, nil
	}
	end := offset + limit
	if end > len(all) {
		end = len(all)
	}
	return all[offset:end], total, nil
}

func (st *StateStore) QueryClosedOptionPositions(strategyID, underlying string, since, until time.Time, limit, offset int) ([]ClosedOptionPosition, int, error) {
	if st == nil {
		return nil, 0, fmt.Errorf("state store unavailable")
	}
	if strategyID != "" {
		db, err := st.dbForStrategy(strategyID)
		if err != nil {
			return nil, 0, err
		}
		return db.QueryClosedOptionPositions(strategyID, underlying, since, until, limit, offset)
	}
	if !st.layout.Split {
		return st.primary().QueryClosedOptionPositions("", underlying, since, until, limit, offset)
	}
	if limit <= 0 {
		limit = 50
	}
	if limit > 500 {
		limit = 500
	}
	if offset < 0 {
		offset = 0
	}
	window := offset + limit
	var all []ClosedOptionPosition
	total := 0
	for _, role := range st.order {
		rows, count, err := st.file(role).QueryClosedOptionPositions("", underlying, since, until, window, 0)
		if err != nil {
			return nil, 0, fmt.Errorf("closed option positions (%s): %w", role, err)
		}
		total += count
		all = append(all, rows...)
	}
	sort.SliceStable(all, func(i, j int) bool {
		if !all[i].ClosedAt.Equal(all[j].ClosedAt) {
			return all[i].ClosedAt.After(all[j].ClosedAt)
		}
		if all[i].StrategyID != all[j].StrategyID {
			return all[i].StrategyID < all[j].StrategyID
		}
		return all[i].PositionID < all[j].PositionID
	})
	if offset >= len(all) {
		return []ClosedOptionPosition{}, total, nil
	}
	end := offset + limit
	if end > len(all) {
		end = len(all)
	}
	return all[offset:end], total, nil
}

func (st *StateStore) TradeDiagnosticsRows(strategyID string) ([]TradeDiagnosticsRow, error) {
	if st == nil {
		return nil, fmt.Errorf("state store unavailable")
	}
	if strategyID != "" {
		db, err := st.dbForStrategy(strategyID)
		if err != nil {
			return nil, err
		}
		rows, err := db.TradeDiagnosticsRows(strategyID)
		if err != nil {
			return nil, err
		}
		return st.stampDiagnosticsScope(rows, db.storageRoleOf()), nil
	}
	if !st.layout.Split {
		return st.primary().TradeDiagnosticsRows("")
	}
	var all []TradeDiagnosticsRow
	for _, role := range st.order {
		rows, err := st.file(role).TradeDiagnosticsRows("")
		if err != nil {
			return nil, fmt.Errorf("trade diagnostics (%s): %w", role, err)
		}
		all = append(all, st.stampDiagnosticsScope(rows, role)...)
	}
	sort.SliceStable(all, func(i, j int) bool { return diagnosticsLessAsc(all[i], all[j]) })
	return all, nil
}

func (st *StateStore) stampDiagnosticsScope(rows []TradeDiagnosticsRow, role storageRole) []TradeDiagnosticsRow {
	scope := st.stampScope(role)
	for i := range rows {
		rows[i].Scope = scope
		rows[i].SourceRole = role
	}
	return rows
}

func diagnosticsLessAsc(a, b TradeDiagnosticsRow) bool {
	if !a.ClosedAt.Equal(b.ClosedAt) {
		return a.ClosedAt.Before(b.ClosedAt)
	}
	if a.Scope != b.Scope {
		return a.Scope < b.Scope
	}
	return a.RowID < b.RowID
}

func (st *StateStore) TradeDiagnosticsRowsPage(strategyID string, limit, offset int) ([]TradeDiagnosticsRow, int, error) {
	if st == nil {
		return nil, 0, fmt.Errorf("state store unavailable")
	}
	if strategyID != "" {
		db, err := st.dbForStrategy(strategyID)
		if err != nil {
			return nil, 0, err
		}
		rows, total, err := db.TradeDiagnosticsRowsPage(strategyID, limit, offset)
		if err != nil {
			return nil, 0, err
		}
		return st.stampDiagnosticsScope(rows, db.storageRoleOf()), total, nil
	}
	if !st.layout.Split {
		return st.primary().TradeDiagnosticsRowsPage("", limit, offset)
	}
	if offset < 0 {
		offset = 0
	}
	window := offset + limit
	var all []TradeDiagnosticsRow
	total := 0
	for _, role := range st.order {
		rows, count, err := st.file(role).TradeDiagnosticsRowsPage("", window, 0)
		if err != nil {
			return nil, 0, fmt.Errorf("trade diagnostics page (%s): %w", role, err)
		}
		total += count
		all = append(all, st.stampDiagnosticsScope(rows, role)...)
	}
	sort.SliceStable(all, func(i, j int) bool { return diagnosticsLessAsc(all[j], all[i]) })
	if offset >= len(all) {
		return []TradeDiagnosticsRow{}, total, nil
	}
	end := offset + limit
	if end > len(all) {
		end = len(all)
	}
	return all[offset:end], total, nil
}

func (st *StateStore) NetPnLForPositions(strategyID string, positionIDs []string) (map[string]map[string]float64, error) {
	if st == nil {
		return nil, fmt.Errorf("state store unavailable")
	}
	if strategyID != "" {
		db, err := st.dbForStrategy(strategyID)
		if err != nil {
			return nil, err
		}
		return db.NetPnLForPositions(strategyID, positionIDs)
	}
	if !st.layout.Split {
		return st.primary().NetPnLForPositions("", positionIDs)
	}
	out := make(map[string]map[string]float64)
	for _, role := range st.order {
		rows, err := st.file(role).NetPnLForPositions("", positionIDs)
		if err != nil {
			return nil, fmt.Errorf("net pnl for positions (%s): %w", role, err)
		}
		st.mergeNetPnL(out, rows, role)
	}
	return out, nil
}

func (st *StateStore) NetPnLByPosition(strategyID string) (map[string]map[string]float64, error) {
	if st == nil {
		return nil, fmt.Errorf("state store unavailable")
	}
	if strategyID != "" {
		db, err := st.dbForStrategy(strategyID)
		if err != nil {
			return nil, err
		}
		return db.NetPnLByPosition(strategyID)
	}
	if !st.layout.Split {
		return st.primary().NetPnLByPosition("")
	}
	out := make(map[string]map[string]float64)
	for _, role := range st.order {
		rows, err := st.file(role).NetPnLByPosition("")
		if err != nil {
			return nil, fmt.Errorf("net pnl by position (%s): %w", role, err)
		}
		st.mergeNetPnL(out, rows, role)
	}
	return out, nil
}

func (st *StateStore) mergeNetPnL(dst, src map[string]map[string]float64, role storageRole) {
	for id, byPos := range src {
		if !st.rowBelongsToRole(id, role) {
			continue
		}
		if dst[id] == nil {
			dst[id] = make(map[string]float64, len(byPos))
		}
		for pid, net := range byPos {
			dst[id][pid] = net
		}
	}
}

// LoadPendingManualActions returns every owned file's queue with its source
// file tagged, so an acknowledgement can only ever reach its own file.
func (st *StateStore) LoadPendingManualActions() ([]PendingManualAction, error) {
	if st == nil {
		return nil, nil
	}
	if !st.layout.Split {
		return st.primary().LoadPendingManualActions()
	}
	var all []PendingManualAction
	for _, role := range st.order {
		rows, err := st.file(role).LoadPendingManualActions()
		if err != nil {
			return nil, fmt.Errorf("pending manual actions (%s): %w", role, err)
		}
		all = append(all, rows...)
	}
	sort.SliceStable(all, func(i, j int) bool {
		if all[i].SourceRole != all[j].SourceRole {
			return all[i].SourceRole < all[j].SourceRole
		}
		return all[i].ID < all[j].ID
	})
	return all, nil
}

// ------------------------------------------------------ live-only tables

func (st *StateStore) LoadPendingLimitOrders() ([]PendingLimitOrder, error) {
	if st == nil {
		return nil, nil
	}
	db, err := st.liveFile()
	if err != nil {
		return nil, err
	}
	return db.LoadPendingLimitOrders()
}

func (st *StateStore) InsertPendingLimitOrder(o PendingLimitOrder) (int64, error) {
	db, err := st.liveOwnedFileForStrategy(o.StrategyID)
	if err != nil {
		return 0, err
	}
	return db.InsertPendingLimitOrder(o)
}

func (st *StateStore) DeletePendingLimitOrder(id int64) error {
	db, err := st.liveFile()
	if err != nil {
		return err
	}
	return db.DeletePendingLimitOrder(id)
}

func (st *StateStore) UpdatePendingLimitOrderFill(id int64, filledSize, avgFillPrice, fillFee float64) error {
	db, err := st.liveFile()
	if err != nil {
		return err
	}
	return db.UpdatePendingLimitOrderFill(id, filledSize, avgFillPrice, fillFee)
}

func (st *StateStore) MarkPendingLimitOrderCancelRequested(strategyID, symbol string) (int64, error) {
	db, err := st.liveOwnedFileForStrategy(strategyID)
	if err != nil {
		return 0, err
	}
	return db.MarkPendingLimitOrderCancelRequested(strategyID, symbol)
}

func (st *StateStore) MarkPendingLimitOrderOperatorRequired(id int64, at time.Time) error {
	db, err := st.liveFile()
	if err != nil {
		return err
	}
	return db.MarkPendingLimitOrderOperatorRequired(id, at)
}

func (st *StateStore) ClearPendingLimitOrderOperatorRequired(id int64) error {
	db, err := st.liveFile()
	if err != nil {
		return err
	}
	return db.ClearPendingLimitOrderOperatorRequired(id)
}

func (st *StateStore) CountPendingLimitOrders(strategyID, symbol string) (int, error) {
	if st == nil {
		return 0, nil
	}
	db, paper, err := st.liveOnlyReadFileForStrategy(strategyID)
	if err != nil {
		return 0, err
	}
	if paper {
		return 0, nil
	}
	return db.CountPendingLimitOrders(strategyID, symbol)
}

// liveOnlyReadFileForStrategy resolves a read over a live-only table. A
// paper-scope strategy can never own a row there, so the read reports "none"
// instead of failing; an unmapped strategy is still an error.
func (st *StateStore) liveOnlyReadFileForStrategy(strategyID string) (*StateDB, bool, error) {
	db, err := st.liveFile()
	if err != nil {
		return nil, false, err
	}
	if !st.layout.Split {
		return db, false, nil
	}
	ident, ok := st.ident.storageFor(strategyID)
	if !ok {
		return nil, false, fmt.Errorf("strategy %q has no storage owner", strategyID)
	}
	return db, ident.Scope != ScopeLive, nil
}

// liveOwnedFileForStrategy refuses a live-only write that names a paper-scope
// strategy: resting limit orders exist on an exchange, so they can only ever
// belong to the primary file.
func (st *StateStore) liveOwnedFileForStrategy(strategyID string) (*StateDB, error) {
	db, err := st.liveFile()
	if err != nil {
		return nil, err
	}
	if !st.layout.Split {
		return db, nil
	}
	ident, ok := st.ident.storageFor(strategyID)
	if !ok {
		return nil, fmt.Errorf("strategy %q has no storage owner", strategyID)
	}
	if ident.Scope != ScopeLive {
		return nil, fmt.Errorf("strategy %q is in the paper scope; resting limit orders are live-only", strategyID)
	}
	return db, nil
}

func (st *StateStore) ListCashflowJournalWallets() ([]CashflowJournalWalletStatus, error) {
	if st != nil && st.layout.Split && !st.hasLiveScope() {
		return nil, nil
	}
	db, err := st.liveFile()
	if err != nil {
		return nil, err
	}
	return db.ListCashflowJournalWallets()
}

// ------------------------------------------------ shared regime history

func (st *StateStore) RecentRegimeWindowTransitions(limit int) ([]RegimeWindowTransitionRow, error) {
	if st == nil {
		return nil, fmt.Errorf("state store unavailable")
	}
	if !st.layout.Split {
		return st.primary().RecentRegimeWindowTransitions(limit)
	}
	var all []RegimeWindowTransitionRow
	for _, role := range st.order {
		rows, err := st.file(role).RecentRegimeWindowTransitions(limit)
		if err != nil {
			return nil, fmt.Errorf("recent regime transitions (%s): %w", role, err)
		}
		for i := range rows {
			rows[i].SourceRole = role
		}
		all = append(all, rows...)
	}
	sort.SliceStable(all, func(i, j int) bool {
		if all[i].TS != all[j].TS {
			return all[i].TS > all[j].TS
		}
		if all[i].SourceRole != all[j].SourceRole {
			return all[i].SourceRole < all[j].SourceRole
		}
		return all[i].ID > all[j].ID
	})
	if limit > 0 && len(all) > limit {
		all = all[:limit]
	}
	return all, nil
}

// ---------------------------------------------------- backfill sources

func (st *StateStore) ListTradesForBackfill(strategyID string) ([]TradeBackfillRow, error) {
	db, err := st.dbForStrategy(strategyID)
	if err != nil {
		return nil, err
	}
	return db.ListTradesForBackfill(strategyID)
}

func (st *StateStore) LoadClosedPositionRows(strategyID string) ([]ClosedPositionRow, error) {
	db, err := st.dbForStrategy(strategyID)
	if err != nil {
		return nil, err
	}
	return db.LoadClosedPositionRows(strategyID)
}

// EarliestTradeTimestamp spans every owned file, so a backfill window covers
// the whole roster it was asked about.
func (st *StateStore) EarliestTradeTimestamp(strategyIDs []string) (time.Time, error) {
	if st == nil {
		return time.Time{}, fmt.Errorf("state store unavailable")
	}
	if !st.layout.Split {
		return st.primary().EarliestTradeTimestamp(strategyIDs)
	}
	var earliest time.Time
	for _, role := range st.order {
		owned := st.ownedProcessIDs(strategyIDs, role)
		if len(owned) == 0 {
			continue
		}
		ts, err := st.file(role).EarliestTradeTimestamp(owned)
		if err != nil {
			return time.Time{}, fmt.Errorf("earliest trade timestamp (%s): %w", role, err)
		}
		if ts.IsZero() {
			continue
		}
		if earliest.IsZero() || ts.Before(earliest) {
			earliest = ts
		}
	}
	return earliest, nil
}

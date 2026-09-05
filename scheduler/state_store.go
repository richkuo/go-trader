package main

import (
	"errors"
	"fmt"
	"os"
	"sort"
	"sync"
	"time"
)

// StateStore owns the physical state files and the immutable identity map. It
// is the only handle the daemon, the status server, the workers and the CLIs
// hold: every write is routed to the file that owns the row, and every read
// that spans scopes is combined here.
type StateStore struct {
	layout storageLayout
	ident  storageIdentityMap
	files  map[storageRole]*StateDB
	order  []storageRole

	mu sync.Mutex
	// pendingAcks is keyed by the OWNING strategy, so a transaction that
	// persists one book can only acknowledge that book's actions. Keying by
	// scope alone would let a single-strategy save delete a queue row whose
	// effect is still only in memory for another strategy.
	pendingAcks    map[string][]int64
	appliedActions map[storageKey]bool
	failures       map[PortfolioScope]int
}

func newStateStore(layout storageLayout, ident storageIdentityMap) *StateStore {
	return &StateStore{
		layout:         layout,
		ident:          ident,
		files:          make(map[storageRole]*StateDB, len(layout.Files)),
		pendingAcks:    make(map[string][]int64),
		appliedActions: make(map[storageKey]bool),
		failures:       make(map[PortfolioScope]int),
	}
}

func OpenStateStore(layout storageLayout, ident storageIdentityMap) (*StateStore, error) {
	return openStateStoreWith(layout, ident, OpenStateDB)
}

// OpenStateStoreReadOnly opens every file with no schema DDL and no migration,
// for the one-shot reporting modes and the inspection command.
func OpenStateStoreReadOnly(layout storageLayout, ident storageIdentityMap) (*StateStore, error) {
	return openStateStoreWith(layout, ident, openStateDBReadOnly)
}

func openStateStoreWith(layout storageLayout, ident storageIdentityMap, open func(string) (*StateDB, error)) (*StateStore, error) {
	st := newStateStore(layout, ident)
	for _, spec := range layout.Files {
		db, err := open(spec.Path)
		if err != nil {
			st.Close()
			return nil, fmt.Errorf("open %s state file %q: %w", spec.Role, spec.Path, err)
		}
		db.role = spec.Role
		db.ident = ident.fileIdentity(spec.Role)
		st.files[spec.Role] = db
		st.order = append(st.order, spec.Role)
	}
	return st, nil
}

// singleFileStore wraps one already-open handle so the legacy single-file
// helpers and the tests keep working through the same surface.
func singleFileStore(sdb *StateDB) *StateStore {
	st := newStateStore(storageLayout{Files: []storageFileSpec{{Role: storageRolePrimary, Path: sdb.path}}}, storageIdentityMap{})
	st.files[storageRolePrimary] = sdb
	st.order = []storageRole{storageRolePrimary}
	return st
}

func (st *StateStore) Close() error {
	if st == nil {
		return nil
	}
	var firstErr error
	for _, role := range st.order {
		if db := st.files[role]; db != nil {
			if err := db.Close(); err != nil && firstErr == nil {
				firstErr = err
			}
		}
	}
	return firstErr
}

func (st *StateStore) Split() bool {
	return st != nil && st.layout.Split
}

func (st *StateStore) Layout() storageLayout {
	if st == nil {
		return storageLayout{}
	}
	return st.layout
}

func (st *StateStore) file(role storageRole) *StateDB {
	if st == nil {
		return nil
	}
	return st.files[role]
}

func (st *StateStore) primary() *StateDB {
	return st.file(storageRolePrimary)
}

// liveFile resolves the handle that owns the live-only tables: wallet ledgers,
// transfers, the cashflow journal and resting limit orders.
func (st *StateStore) liveFile() (*StateDB, error) {
	if st == nil {
		return nil, fmt.Errorf("state store unavailable")
	}
	db := st.primary()
	if db == nil {
		return nil, fmt.Errorf("primary state file unavailable")
	}
	return db, nil
}

func (st *StateStore) hasLiveScope() bool {
	return st != nil && st.ident.hasScope(ScopeLive)
}

func (st *StateStore) scopeForRole(role storageRole) PortfolioScope {
	if !st.layout.Split {
		return scopeUnassigned
	}
	if role == storageRolePaper {
		return ScopePaper
	}
	return ScopeLive
}

func (st *StateStore) fileForScope(scope PortfolioScope) (*StateDB, error) {
	if st == nil {
		return nil, fmt.Errorf("state store unavailable")
	}
	role := st.layout.roleForScope(scope)
	db := st.file(role)
	if db == nil {
		return nil, fmt.Errorf("no %s state file for the %s scope", role, scopeLabel(scope))
	}
	return db, nil
}

// dbForStrategy resolves the file that owns one strategy's books. Unknown
// ownership is an error before any mutation, never a guess.
func (st *StateStore) dbForStrategy(processID string) (*StateDB, error) {
	if st == nil {
		return nil, fmt.Errorf("state store unavailable")
	}
	if !st.layout.Split {
		db := st.primary()
		if db == nil {
			return nil, fmt.Errorf("primary state file unavailable")
		}
		return db, nil
	}
	ident, ok := st.ident.storageFor(processID)
	if !ok {
		return nil, fmt.Errorf("strategy %q has no storage owner", processID)
	}
	db := st.file(ident.Role)
	if db == nil {
		return nil, fmt.Errorf("no %s state file for strategy %q", ident.Role, processID)
	}
	return db, nil
}

func (st *StateStore) scopeForStrategy(processID string) (PortfolioScope, bool) {
	if st == nil {
		return scopeUnassigned, false
	}
	return st.ident.scopeFor(processID)
}

func (st *StateStore) ownedProcessIDs(ids []string, role storageRole) []string {
	if !st.layout.Split {
		return ids
	}
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		if ident, ok := st.ident.storageFor(id); ok && ident.Role == role {
			out = append(out, id)
		}
	}
	return out
}

// ---------------------------------------------------------------- saving

func (st *StateStore) buildRequest(state *AppState, role storageRole, scopes []PortfolioScope, fullFile, writeMeta bool) scopeSaveRequest {
	owned := make(map[PortfolioScope]bool, len(scopes))
	for _, scope := range scopes {
		owned[scope] = true
	}
	scopeOf := make(map[string]PortfolioScope, len(state.Strategies))
	ids := make([]string, 0, len(state.Strategies))
	for id := range state.Strategies {
		scope, mapped := st.ident.scopeFor(id)
		if !mapped {
			// No identity entry: the legacy single-file store, where the
			// primary owns every row. A split layout never guesses.
			if st.layout.Split || !fullFile {
				continue
			}
			scopeOf[id] = scopeUnassigned
			ids = append(ids, id)
			continue
		}
		if !owned[scope] {
			continue
		}
		scopeOf[id] = scope
		ids = append(ids, id)
	}
	sort.Strings(ids)
	states := make([]*StrategyState, 0, len(ids))
	for _, id := range ids {
		if s := state.Strategies[id]; s != nil {
			states = append(states, s)
		}
	}
	return scopeSaveRequest{
		Strategies:      states,
		ScopeOf:         scopeOf,
		Scopes:          scopes,
		FullFile:        fullFile,
		IncludeUnscoped: fullFile && !st.layout.Split,
		WriteMeta:       writeMeta,
		AckActionID:     st.acksForStrategies(ids),
	}
}

// SaveAll persists every scope, one transaction per physical file. A failed
// file leaves its scope's in-memory state untouched for retry; the other
// file's committed effects are already durable and are never replayed.
func (st *StateStore) SaveAll(state *AppState) map[PortfolioScope]error {
	out := make(map[PortfolioScope]error, 2)
	if st == nil || state == nil {
		return out
	}
	for _, role := range st.order {
		db := st.file(role)
		scopes := st.layout.scopesForRole(role)
		req := st.buildRequest(state, role, scopes, true, role == storageRolePrimary)
		err := db.saveStateSubset(state, req)
		if err == nil {
			st.clearAcks(strategyIDs(req.Strategies))
		}
		for _, scope := range scopes {
			out[scope] = err
			st.recordSaveOutcome(scope, err)
		}
	}
	return out
}

// SaveScope persists one scope's books and risk row. In the single-file layout
// it replaces only that scope's rows, so the other scope's books survive.
func (st *StateStore) SaveScope(state *AppState, scope PortfolioScope) error {
	if st == nil || state == nil {
		return fmt.Errorf("state store unavailable")
	}
	role := st.layout.roleForScope(scope)
	db := st.file(role)
	if db == nil {
		return fmt.Errorf("no %s state file for the %s scope", role, scopeLabel(scope))
	}
	fullFile := len(st.layout.scopesForRole(role)) == 1
	req := st.buildRequest(state, role, []PortfolioScope{scope}, fullFile, fullFile && role == storageRolePrimary)
	err := db.saveStateSubset(state, req)
	if err == nil {
		st.clearAcks(strategyIDs(req.Strategies))
	}
	st.recordSaveOutcome(scope, err)
	return err
}

func (st *StateStore) SaveStrategyBook(s *StrategyState) error {
	if st == nil || s == nil {
		return fmt.Errorf("state store unavailable")
	}
	db, err := st.dbForStrategy(s.ID)
	if err != nil {
		return err
	}
	scope, _ := st.scopeForStrategy(s.ID)
	acks := st.acksForStrategies([]string{s.ID})
	if err := db.saveStrategyBookWithAcks(s, scope, acks); err != nil {
		return err
	}
	st.clearAcks([]string{s.ID})
	return nil
}

// ------------------------------------------------- manual action acknowledgement

// recordAppliedManualAction marks an action applied in memory and queues its
// acknowledgement for the transaction that persists the effect. A failed action
// never reaches this set, so it survives every later success.
func (st *StateStore) recordAppliedManualAction(strategyID string, role storageRole, id int64) {
	if st == nil {
		return
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	key := storageKey{Role: role, ID: fmt.Sprintf("%d", id)}
	if st.appliedActions[key] {
		return
	}
	st.appliedActions[key] = true
	st.pendingAcks[strategyID] = append(st.pendingAcks[strategyID], id)
}

func (st *StateStore) manualActionApplied(role storageRole, id int64) bool {
	if st == nil {
		return false
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	return st.appliedActions[storageKey{Role: role, ID: fmt.Sprintf("%d", id)}]
}

func (st *StateStore) acksForStrategies(ids []string) []int64 {
	if st == nil {
		return nil
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	var out []int64
	for _, id := range ids {
		out = append(out, st.pendingAcks[id]...)
	}
	return out
}

func (st *StateStore) clearAcks(ids []string) {
	if st == nil {
		return
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	for _, id := range ids {
		ident, ok := st.ident.storageFor(id)
		role := storageRolePrimary
		if ok {
			role = ident.Role
		}
		for _, actionID := range st.pendingAcks[id] {
			delete(st.appliedActions, storageKey{Role: role, ID: fmt.Sprintf("%d", actionID)})
		}
		delete(st.pendingAcks, id)
	}
}

// strategyIDs names the books one save request covers, for acknowledgement.
func strategyIDs(states []*StrategyState) []string {
	out := make([]string, 0, len(states))
	for _, s := range states {
		if s != nil {
			out = append(out, s.ID)
		}
	}
	return out
}

// ------------------------------------------------------- persistence hold

func (st *StateStore) recordSaveOutcome(scope PortfolioScope, err error) {
	if st == nil {
		return
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	if err != nil {
		st.failures[scope]++
		return
	}
	delete(st.failures, scope)
}

func (st *StateStore) saveFailures(scope PortfolioScope) int {
	if st == nil {
		return 0
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	return st.failures[scope]
}

// persistenceHoldsScope reports an unacknowledged save failure. While it holds,
// the scope takes no position-increasing action; closes, trailing stops,
// ratchet, protection sync and hedge management keep running.
func (st *StateStore) persistenceHoldsScope(scope PortfolioScope) bool {
	return st.saveFailures(scope) > 0
}

// ------------------------------------------------------------ routed writes

func (st *StateStore) InsertTrade(strategyID string, trade Trade) error {
	db, err := st.dbForStrategy(strategyID)
	if err != nil {
		return err
	}
	return db.InsertTrade(strategyID, trade)
}

func (st *StateStore) ReconcileModelOnlyClose(u modelOnlyCloseCorrection) error {
	db, err := st.dbForStrategy(u.StrategyID)
	if err != nil {
		return err
	}
	return db.ReconcileModelOnlyClose(u)
}

func (st *StateStore) LoadModelOnlyCloseBasis(strategyID, symbol, closeReason string, closedAt time.Time) (*modelOnlyClosedBasis, error) {
	db, err := st.dbForStrategy(strategyID)
	if err != nil {
		return nil, err
	}
	return db.LoadModelOnlyCloseBasis(strategyID, symbol, closeReason, closedAt)
}

func (st *StateStore) MarkModelOnlyCloseAbandoned(strategyID, symbol string, ts time.Time) error {
	db, err := st.dbForStrategy(strategyID)
	if err != nil {
		return err
	}
	return db.MarkModelOnlyCloseAbandoned(strategyID, symbol, ts)
}

func (st *StateStore) SetInitialCapital(strategyID string, value float64) error {
	db, err := st.dbForStrategy(strategyID)
	if err != nil {
		return err
	}
	return db.SetInitialCapital(strategyID, value)
}

func (st *StateStore) PersistSharedWalletPoolStateTransition(s *StrategyState) error {
	if s == nil {
		return fmt.Errorf("strategy state is nil")
	}
	db, err := st.dbForStrategy(s.ID)
	if err != nil {
		return err
	}
	return db.PersistSharedWalletPoolStateTransition(s)
}

func (st *StateStore) InsertPendingManualAction(a PendingManualAction) error {
	db, err := st.dbForStrategy(a.StrategyID)
	if err != nil {
		return err
	}
	return db.InsertPendingManualAction(a)
}

// InsertTradeDiagnostics stamps the owning scope on the row so the background
// worker can name the file it must update.
func (st *StateStore) InsertTradeDiagnostics(row *TradeDiagnosticsRow) error {
	if row == nil {
		return fmt.Errorf("nil diagnostics row")
	}
	db, err := st.dbForStrategy(row.StrategyID)
	if err != nil {
		return err
	}
	scope, _ := st.scopeForStrategy(row.StrategyID)
	row.Scope = scope
	row.SourceRole = db.storageRoleOf()
	return db.InsertTradeDiagnostics(row)
}

// UpdateTradeDiagnosticsMetrics needs the owning scope: a bare row identifier
// cannot choose a file.
func (st *StateStore) UpdateTradeDiagnosticsMetrics(scope PortfolioScope, rowID int64, timeframe string, m *tradeQualityMetrics, status string) error {
	if st == nil {
		return fmt.Errorf("state store unavailable")
	}
	if st.layout.Split && scope == scopeUnassigned {
		return fmt.Errorf("diagnostics row %d carries no scope; refusing to guess a state file", rowID)
	}
	db, err := st.fileForScope(scope)
	if err != nil {
		return err
	}
	return db.UpdateTradeDiagnosticsMetrics(rowID, timeframe, m, status)
}

func (st *StateStore) UpdateTradeStampedFields(strategyID string, ts time.Time, entryATR float64, stopLossOID int64, stopLossTriggerPx float64, tpOIDs []int64, stopLossATRMult *float64, tpTiersJSON string) error {
	db, err := st.dbForStrategy(strategyID)
	if err != nil {
		return err
	}
	return db.UpdateTradeStampedFields(strategyID, ts, entryATR, stopLossOID, stopLossTriggerPx, tpOIDs, stopLossATRMult, tpTiersJSON)
}

func (st *StateStore) HasTradeWithExchangeOrderID(strategyID, exchangeOrderID string) (bool, error) {
	db, err := st.dbForStrategy(strategyID)
	if err != nil {
		return false, err
	}
	return db.HasTradeWithExchangeOrderID(strategyID, exchangeOrderID)
}

// storeLiveDB resolves the primary handle for a live-only table write. A
// resolution failure is reported once and the caller skips that ingestion, so a
// misconfigured layout never writes exchange ledger rows into the wrong file.
func storeLiveDB(store *StateStore) *StateDB {
	db, err := store.liveFile()
	if err != nil {
		fmt.Fprintf(os.Stderr, "[storage] WARN: %v\n", err)
		return nil
	}
	return db
}

// allScopesSaveBlocked reports the existing three-strike rule, now per scope:
// every active scope must be blocked before a whole cycle is skipped.
func allScopesSaveBlocked(store *StateStore, cfg *Config) bool {
	if store == nil || cfg == nil {
		return false
	}
	scopes := activeScopes(cfg.Strategies)
	if len(scopes) == 0 {
		return false
	}
	for _, scope := range scopes {
		if store.saveFailures(scope) < 3 {
			return false
		}
	}
	return true
}

// scopeSaveBlocked reports the three-strike skip for one scope.
func scopeSaveBlocked(store *StateStore, scope PortfolioScope) bool {
	return store != nil && store.saveFailures(scope) >= 3
}

// dueStrategiesPersistable drops the strategies of any scope whose saves have
// failed three times running; the other scope keeps trading.
func dueStrategiesPersistable(store *StateStore, due []StrategyConfig) []StrategyConfig {
	if store == nil {
		return due
	}
	out := make([]StrategyConfig, 0, len(due))
	for _, sc := range due {
		if scopeSaveBlocked(store, portfolioScopeFor(sc)) {
			continue
		}
		out = append(out, sc)
	}
	return out
}

// reportStateOwnershipFailure prints the singleton guard message for a failed
// ownership acquisition, naming the file and the holder.
func reportStateOwnershipFailure(err error) {
	var locked *stateDBLockedError
	if errors.As(err, &locked) {
		fmt.Fprintf(os.Stderr, "[singleton] CRITICAL: %s — refusing to start so this process can't double-trade against the same state files. If no other go-trader is actually running, the lock auto-releases on exit; check `pgrep -af go-trader`.\n", locked.Error())
		return
	}
	fmt.Fprintf(os.Stderr, "[singleton] CRITICAL: could not acquire state file ownership: %v — refusing to start.\n", err)
}

// manualActionLock takes the manual-action lock of the file that owns one
// strategy. Manual commands coexist with a running daemon, so they take this
// lock only, never file ownership.
func (st *StateStore) manualActionLock(strategyID string) (func(), error) {
	db, err := st.dbForStrategy(strategyID)
	if err != nil {
		return nil, err
	}
	return acquireManualActionFileLock(db.path)
}

// openToolStateStore opens every state file for a CLI command. Migrations run
// exactly as they always did, and no ownership lock is taken: manual commands
// coexist with a running daemon through the per-file manual-action lock.
func openToolStateStore(cfg *Config) (*StateStore, error) {
	layout, err := resolveStorageLayout(cfg)
	if err != nil {
		return nil, err
	}
	ident, err := buildStorageIdentityMap(cfg, layout)
	if err != nil {
		return nil, err
	}
	return OpenStateStore(layout, ident)
}

// openToolStateStoreReadOnly opens every existing state file with no DDL and no
// migration, for reporting commands.
func openToolStateStoreReadOnly(cfg *Config) (*StateStore, error) {
	layout, err := resolveStorageLayout(cfg)
	if err != nil {
		return nil, err
	}
	ident, err := buildStorageIdentityMap(cfg, layout)
	if err != nil {
		return nil, err
	}
	present := storageLayout{Split: layout.Split}
	for _, spec := range layout.Files {
		if _, statErr := os.Stat(spec.Canonical); statErr != nil {
			continue
		}
		present.Files = append(present.Files, spec)
	}
	if len(present.Files) == 0 {
		return nil, fmt.Errorf("no state file exists yet")
	}
	return OpenStateStoreReadOnly(present, ident)
}

// acquireBackfillOwnership takes file ownership plus every file's manual-action
// lock for the apply window, so a backfill can never race a scheduler or a
// manual write. It replaces the old process-name probe.
func acquireBackfillOwnership(cfg *Config) (release func(), err error) {
	layout, err := resolveStorageLayout(cfg)
	if err != nil {
		return nil, err
	}
	owned, err := acquireStateOwnership(layout.Files)
	if err != nil {
		return nil, err
	}
	var manualLocks []func()
	releaseAll := func() {
		for i := len(manualLocks) - 1; i >= 0; i-- {
			manualLocks[i]()
		}
		owned.Release()
	}
	for _, spec := range layout.Files {
		unlock, lockErr := acquireManualActionFileLock(spec.Path)
		if lockErr != nil {
			releaseAll()
			return nil, lockErr
		}
		manualLocks = append(manualLocks, unlock)
	}
	return releaseAll, nil
}

// bindPlanToStore stamps a plan with the file and stored identifier it was
// built against, so applying it to another file is refused.
func bindPlanToStore(store *StateStore, procID string) (storageRole, string, error) {
	db, err := store.dbForStrategy(procID)
	if err != nil {
		return "", "", err
	}
	storageID, err := db.toStorageID(procID)
	if err != nil {
		return "", "", err
	}
	return db.storageRoleOf(), storageID, nil
}

// readOnly reports a store whose files were opened with no DDL and no write
// path: the one-shot reporting modes and every inspection command.
func (st *StateStore) readOnly() bool {
	if st == nil {
		return false
	}
	for _, role := range st.order {
		if db := st.file(role); db != nil && db.readOnly {
			return true
		}
	}
	return false
}

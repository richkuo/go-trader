package main

import (
	"database/sql"
	"fmt"
	"sort"
	"strings"
)

type robinhoodCoinRewrite struct {
	StrategyID string
	From       string
	To         string
}

const robinhoodCoinIdentityCapability = "go-trader-capability:robinhood-coin-identity-v1"

func canonicalRobinhoodCoin(raw string) string {
	return strings.ToUpper(strings.TrimSpace(raw))
}

func robinhoodSpotIdentityOwner(sc StrategyConfig) bool {
	return sc.Platform == "robinhood" && sc.Type == "spot" && len(sc.Args) >= 2
}

func normalizeRobinhoodSpotCoins(cfg *Config) []string {
	if cfg == nil {
		return nil
	}
	cfg.robinhoodCoinRewrites = nil
	var errs []string
	for i := range cfg.Strategies {
		sc := &cfg.Strategies[i]
		if !robinhoodSpotIdentityOwner(*sc) {
			continue
		}
		raw := sc.Args[1]
		canon := canonicalRobinhoodCoin(raw)
		if canon == "" {
			errs = append(errs, fmt.Sprintf("strategy[%s]: robinhood spot coin args[1] %q is empty after trimming", sc.ID, raw))
			continue
		}
		if canon == raw {
			continue
		}
		args := append([]string(nil), sc.Args...)
		args[1] = canon
		sc.Args = args
		cfg.robinhoodCoinRewrites = append(cfg.robinhoodCoinRewrites, robinhoodCoinRewrite{StrategyID: sc.ID, From: raw, To: canon})
	}
	return errs
}

func robinhoodCoinRewriteWarnings(cfg *Config) []string {
	if cfg == nil {
		return nil
	}
	out := make([]string, 0, len(cfg.robinhoodCoinRewrites))
	for _, rw := range cfg.robinhoodCoinRewrites {
		out = append(out, fmt.Sprintf("[config] WARN: strategy %s robinhood coin %q is read as %q (one trimmed uppercase coin identity, #1908). Write %q into args[1] in the config file: an older go-trader binary reads the book by the configured spelling, so after the startup identity migration it would see the held %s book as flat",
			rw.StrategyID, rw.From, rw.To, rw.To, rw.To))
	}
	return out
}

func robinhoodCoinAlias(symbol, canonical string) bool {
	return symbol != canonical && canonicalRobinhoodCoin(symbol) == canonical
}

type robinhoodIdentityPlan struct {
	StrategyID string
	Canonical  string
	Partition  RiskPartition
	Role       storageRole

	book            *StrategyState
	positionFrom    string
	positionSymbols []string
	pendingRenames  int
	noticeFrom      string
	attempts        []*robinhoodOrderAttempt
	decisions       []entryDecisionKey
}

func (p *robinhoodIdentityPlan) changesBook() bool {
	return p.positionFrom != "" || p.pendingRenames > 0 || p.noticeFrom != ""
}

func (p *robinhoodIdentityPlan) changed() bool {
	return p.changesBook() || len(p.attempts) > 0 || len(p.decisions) > 0
}

func (p *robinhoodIdentityPlan) logLine() string {
	positions := 0
	if p.positionFrom != "" {
		positions = 1
	}
	notice := 0
	if p.noticeFrom != "" {
		notice = 1
	}
	return fmt.Sprintf("[storage] Robinhood coin identity migrated for %s in the %s state file to %s: positions=%d pending_circuit_closes=%d shared_close_notice=%d unresolved_attempts=%d entry_decisions=%d (#1908)",
		p.StrategyID, p.Role, p.Canonical, positions, p.pendingRenames, notice, len(p.attempts), len(p.decisions))
}

type ownedRobinhoodAttempt struct {
	ProcID  string
	Attempt *robinhoodOrderAttempt
}

func (st *StateStore) ownedUnresolvedRobinhoodAttempts(cfgs []StrategyConfig) ([]ownedRobinhoodAttempt, error) {
	if st == nil {
		return nil, fmt.Errorf("state store unavailable")
	}
	live := make(map[string]bool)
	for _, sc := range cfgs {
		if isRobinhoodLiveSpot(sc) {
			live[sc.ID] = true
		}
	}
	primary := st.primary()
	var out []ownedRobinhoodAttempt
	for _, role := range st.order {
		db := st.file(role)
		if db == nil {
			continue
		}
		attempts, sids, err := db.loadUnresolvedRobinhoodAttempts()
		if err != nil {
			return nil, err
		}
		for i, a := range attempts {
			if db != primary || !robinhoodRoleOwnsLive(st.layout, role) {
				return nil, fmt.Errorf("unresolved Robinhood order attempt %s sits in the %s state file, which does not own the live partition; resolve the venue order and move it by hand", a.AttemptID, role)
			}
			procID, mapped := db.mapStoredStrategyID(sids[i])
			if !mapped || !live[procID] {
				return nil, fmt.Errorf("unresolved Robinhood order attempt %s (%s %s) belongs to stored strategy %q, which is not a configured live Robinhood spot strategy; restore the strategy or resolve the venue order by hand", a.AttemptID, a.Side, a.Symbol, sids[i])
			}
			out = append(out, ownedRobinhoodAttempt{ProcID: procID, Attempt: a})
		}
	}
	return out, nil
}

type robinhoodIdentityRefusal struct {
	problems []string
}

func (r *robinhoodIdentityRefusal) Error() string {
	sort.Strings(r.problems)
	return fmt.Sprintf("Robinhood coin identity preflight refused %d conflict(s); no book was changed: %s. Review each named strategy against the Robinhood account and a state-file backup before any manual correction; never delete a row to pass this check (#1908)",
		len(r.problems), strings.Join(r.problems, "; "))
}

func planRobinhoodIdentityMigration(cfg *Config, store *StateStore, state *AppState) ([]*robinhoodIdentityPlan, error) {
	if cfg == nil || store == nil || state == nil {
		return nil, fmt.Errorf("robinhood identity preflight needs the config, the state store and the loaded state")
	}
	var problems []string
	var plans []*robinhoodIdentityPlan
	byID := make(map[string]*robinhoodIdentityPlan)
	for _, sc := range cfg.Strategies {
		if !robinhoodSpotIdentityOwner(sc) {
			continue
		}
		canon := canonicalRobinhoodCoin(robinhoodSymbol(sc.Args))
		if canon == "" {
			problems = append(problems, fmt.Sprintf("strategy %s has an empty Robinhood coin", sc.ID))
			continue
		}
		db, err := store.dbForStrategy(sc.ID)
		if err != nil {
			return nil, err
		}
		p := &robinhoodIdentityPlan{StrategyID: sc.ID, Canonical: canon, Partition: partitionFor(sc), Role: db.storageRoleOf()}
		if msgs := planRobinhoodIdentityBook(p, state.Strategies[sc.ID]); len(msgs) > 0 {
			problems = append(problems, msgs...)
		}
		decisions, msgs, err := planRobinhoodIdentityEntryRows(db, p)
		if err != nil {
			return nil, err
		}
		p.decisions = decisions
		problems = append(problems, msgs...)
		plans = append(plans, p)
		byID[sc.ID] = p
	}

	owned, err := store.ownedUnresolvedRobinhoodAttempts(cfg.Strategies)
	if err != nil {
		return nil, err
	}
	type coinKey struct{ account, symbol string }
	final := make(map[coinKey][]string)
	renamed := make(map[coinKey]bool)
	for _, o := range owned {
		a := o.Attempt
		target := a.Symbol
		if p := byID[o.ProcID]; p != nil && robinhoodCoinAlias(a.Symbol, p.Canonical) {
			target = p.Canonical
			p.attempts = append(p.attempts, a)
			renamed[coinKey{a.AccountKey, target}] = true
		}
		k := coinKey{a.AccountKey, target}
		final[k] = append(final[k], fmt.Sprintf("%s (%s, %s)", a.AttemptID, o.ProcID, a.Symbol))
	}
	for k, ids := range final {
		if len(ids) > 1 && renamed[k] {
			sort.Strings(ids)
			problems = append(problems, fmt.Sprintf("unresolved Robinhood order attempts %s would share one unresolved slot for %s on one account after conversion", strings.Join(ids, ", "), k.symbol))
		}
	}

	if len(problems) > 0 {
		return nil, &robinhoodIdentityRefusal{problems: problems}
	}
	out := make([]*robinhoodIdentityPlan, 0, len(plans))
	for _, p := range plans {
		if p.changed() {
			sort.Slice(p.attempts, func(i, j int) bool { return p.attempts[i].AttemptID < p.attempts[j].AttemptID })
			out = append(out, p)
		}
	}
	roleRank := make(map[storageRole]int, len(store.order))
	for i, role := range store.order {
		roleRank[role] = i
	}
	sort.SliceStable(out, func(i, j int) bool {
		if roleRank[out[i].Role] != roleRank[out[j].Role] {
			return roleRank[out[i].Role] < roleRank[out[j].Role]
		}
		return out[i].StrategyID < out[j].StrategyID
	})
	return out, nil
}

func planRobinhoodIdentityBook(p *robinhoodIdentityPlan, s *StrategyState) []string {
	if s == nil {
		return nil
	}
	var problems []string
	keys := make([]string, 0, len(s.Positions))
	for k := range s.Positions {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var aliases []string
	for _, k := range keys {
		pos := s.Positions[k]
		if pos != nil && pos.Symbol != k && (canonicalRobinhoodCoin(k) == p.Canonical || canonicalRobinhoodCoin(pos.Symbol) == p.Canonical) {
			problems = append(problems, fmt.Sprintf("strategy %s holds position key %q whose symbol field is %q", p.StrategyID, k, pos.Symbol))
			continue
		}
		if robinhoodCoinAlias(k, p.Canonical) {
			aliases = append(aliases, k)
		}
	}
	switch {
	case len(aliases) > 1:
		problems = append(problems, fmt.Sprintf("strategy %s holds positions %q that all convert to %s", p.StrategyID, aliases, p.Canonical))
	case len(aliases) == 1 && s.Positions[p.Canonical] != nil:
		problems = append(problems, fmt.Sprintf("strategy %s holds positions %q and %q that both convert to %s", p.StrategyID, aliases[0], p.Canonical, p.Canonical))
	case len(aliases) == 1:
		p.positionFrom = aliases[0]
	}
	p.positionSymbols = keys

	if pending := s.RiskState.PendingCircuitCloses[PlatformPendingCloseRobinhood]; pending != nil {
		seen := make(map[string]bool)
		for _, sym := range pending.Symbols {
			target := sym.Symbol
			if robinhoodCoinAlias(sym.Symbol, p.Canonical) {
				target = p.Canonical
				p.pendingRenames++
			}
			if seen[target] && target == p.Canonical {
				problems = append(problems, fmt.Sprintf("strategy %s holds more than one pending Robinhood circuit close that converts to %s", p.StrategyID, p.Canonical))
			}
			seen[target] = true
		}
	}
	if robinhoodCoinAlias(s.RiskState.RobinhoodSharedCloseNotice.Coin, p.Canonical) {
		p.noticeFrom = s.RiskState.RobinhoodSharedCloseNotice.Coin
	}
	if len(problems) == 0 && p.changesBook() {
		p.book = s
	}
	return problems
}

func planRobinhoodIdentityEntryRows(db *StateDB, p *robinhoodIdentityPlan) ([]entryDecisionKey, []string, error) {
	if err := validEntryPartition(p.Partition); err != nil {
		return nil, nil, fmt.Errorf("strategy %q: %w", p.StrategyID, err)
	}
	sid, err := db.toStorageID(p.StrategyID)
	if err != nil {
		return nil, nil, err
	}
	var problems []string
	var renames []entryDecisionKey
	exists, err := tableExists(db, "entry_decision_state")
	if err != nil {
		return nil, nil, err
	}
	if exists {
		rows, err := db.db.Query(`SELECT symbol, interval_ms, target_side FROM entry_decision_state
			WHERE scope = ? AND source = ? AND strategy_id = ? ORDER BY symbol, interval_ms, target_side`,
			string(p.Partition.Scope), p.Partition.Source, sid)
		if err != nil {
			return nil, nil, fmt.Errorf("read entry decisions for %s in the %s state file: %w", p.StrategyID, db.storageRoleOf(), err)
		}
		finalKeys := make(map[entryDecisionKey][]string)
		for rows.Next() {
			var k entryDecisionKey
			if err := rows.Scan(&k.Symbol, &k.IntervalMs, &k.Side); err != nil {
				rows.Close()
				return nil, nil, fmt.Errorf("scan entry decision for %s: %w", p.StrategyID, err)
			}
			target := k
			if robinhoodCoinAlias(k.Symbol, p.Canonical) {
				target.Symbol = p.Canonical
				renames = append(renames, k)
			}
			finalKeys[target] = append(finalKeys[target], k.Symbol)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, nil, fmt.Errorf("iterate entry decisions for %s: %w", p.StrategyID, err)
		}
		for k, from := range finalKeys {
			if len(from) > 1 && k.Symbol == p.Canonical {
				sort.Strings(from)
				problems = append(problems, fmt.Sprintf("strategy %s holds entry decisions %q for interval %dms side %s that all convert to %s", p.StrategyID, from, k.IntervalMs, k.Side, p.Canonical))
			}
		}
	}
	exists, err = tableExists(db, "entry_order_intents")
	if err != nil {
		return nil, nil, err
	}
	if exists {
		rows, err := db.db.Query(`SELECT intent_id, symbol, status FROM entry_order_intents
			WHERE scope = ? AND source = ? AND strategy_id = ?
			  AND (status IN ('reserved','submitting','settling')
			       OR (status = 'committed' AND replay_payload_json != '' AND replay_delivered_seq < settlement_seq))
			ORDER BY intent_id`, string(p.Partition.Scope), p.Partition.Source, sid)
		if err != nil {
			return nil, nil, fmt.Errorf("read entry intents for %s in the %s state file: %w", p.StrategyID, db.storageRoleOf(), err)
		}
		for rows.Next() {
			var id, symbol, status string
			if err := rows.Scan(&id, &symbol, &status); err != nil {
				rows.Close()
				return nil, nil, fmt.Errorf("scan entry intent for %s: %w", p.StrategyID, err)
			}
			if robinhoodCoinAlias(symbol, p.Canonical) {
				problems = append(problems, fmt.Sprintf("strategy %s holds active entry intent %s (%s, status %s); no Robinhood entry provider can resolve or rewrite it", p.StrategyID, id, symbol, status))
			}
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, nil, fmt.Errorf("iterate entry intents for %s: %w", p.StrategyID, err)
		}
	}
	return renames, problems, nil
}

func stageRobinhoodIdentityBook(s *StrategyState, p *robinhoodIdentityPlan) *StrategyState {
	c := *s
	c.Positions = make(map[string]*Position, len(s.Positions))
	for k, pos := range s.Positions {
		if pos == nil {
			continue
		}
		pc := *pos
		pc.TPOIDs = append([]int64(nil), pos.TPOIDs...)
		pc.TPArmedTiers = append([]bool(nil), pos.TPArmedTiers...)
		pc.TPConsumptions = append([]TPConsumption(nil), pos.TPConsumptions...)
		if k == p.positionFrom {
			k = p.Canonical
			pc.Symbol = p.Canonical
		}
		c.Positions[k] = &pc
	}
	c.OptionPositions = make(map[string]*OptionPosition, len(s.OptionPositions))
	for k, op := range s.OptionPositions {
		if op == nil {
			continue
		}
		oc := *op
		c.OptionPositions[k] = &oc
	}
	if s.RiskState.PendingCircuitCloses != nil {
		c.RiskState.PendingCircuitCloses = make(map[string]*PendingCircuitClose, len(s.RiskState.PendingCircuitCloses))
		for k, v := range s.RiskState.PendingCircuitCloses {
			if v == nil {
				continue
			}
			vc := *v
			vc.Symbols = append([]PendingCircuitCloseSymbol(nil), v.Symbols...)
			if k == PlatformPendingCloseRobinhood {
				for i := range vc.Symbols {
					if robinhoodCoinAlias(vc.Symbols[i].Symbol, p.Canonical) {
						vc.Symbols[i].Symbol = p.Canonical
					}
				}
			}
			c.RiskState.PendingCircuitCloses[k] = &vc
		}
	}
	if p.noticeFrom != "" {
		c.RiskState.RobinhoodSharedCloseNotice.Coin = p.Canonical
	}
	c.TradeHistory = nil
	c.ClosedPositions = nil
	c.ClosedOptionPositions = nil
	c.pendingTradeDiagnostics = nil
	c.EntryGuard = nil
	c.RobinhoodOrders = nil
	return &c
}

func (st *StateStore) MigrateRobinhoodIdentity(p *robinhoodIdentityPlan) error {
	if p == nil {
		return fmt.Errorf("robinhood identity plan is nil")
	}
	db, err := st.dbForStrategy(p.StrategyID)
	if err != nil {
		return err
	}
	return db.migrateRobinhoodIdentity(p)
}

func (sdb *StateDB) migrateRobinhoodIdentity(p *robinhoodIdentityPlan) error {
	if sdb == nil || sdb.db == nil {
		return fmt.Errorf("state db unavailable")
	}
	if sdb.readOnly {
		return fmt.Errorf("%s state file is open read-only", sdb.storageRoleOf())
	}
	sid, err := sdb.toStorageID(p.StrategyID)
	if err != nil {
		return err
	}
	var staged *StrategyState
	if p.book != nil {
		staged = stageRobinhoodIdentityBook(p.book, p)
	}

	tx, err := sdb.db.Begin()
	if err != nil {
		return fmt.Errorf("begin robinhood identity migration for %s: %w", p.StrategyID, err)
	}
	defer tx.Rollback()

	if staged != nil {
		if err := checkRobinhoodIdentityPositionsTx(tx, sid, p); err != nil {
			return err
		}
		if err := sdb.guardQuoteOwnersTx(tx, []*StrategyState{staged}, []string{sid}, false); err != nil {
			return err
		}
		flushed, diags, err := sdb.writeOneStrategyBookTx(tx, staged, sid, p.Partition.Scope)
		if err != nil {
			return err
		}
		if len(flushed) > 0 || len(diags) > 0 {
			return fmt.Errorf("robinhood identity migration for %s would write trade history; refusing", p.StrategyID)
		}
	}
	if len(p.attempts) > 0 {
		attemptSID, err := sdb.robinhoodAttemptOwner(p.StrategyID)
		if err != nil {
			return err
		}
		for _, a := range p.attempts {
			res, err := tx.Exec(`UPDATE robinhood_order_attempts SET symbol = ?
				WHERE attempt_id = ? AND strategy_id = ? AND account_key = ? AND symbol = ? AND ref_id = ?
				  AND status IN `+robinhoodUnresolvedStatusSQL,
				p.Canonical, a.AttemptID, attemptSID, a.AccountKey, a.Symbol, a.RefID)
			if err != nil {
				return fmt.Errorf("rename robinhood order attempt %s: %w", a.AttemptID, err)
			}
			if err := requireOneRow(res, fmt.Sprintf("robinhood order attempt %s (%s)", a.AttemptID, a.Symbol)); err != nil {
				return err
			}
		}
	}
	if len(p.decisions) > 0 {
		entrySID, err := sdb.entryOwner(p.StrategyID, p.Partition)
		if err != nil {
			return err
		}
		for _, k := range p.decisions {
			res, err := tx.Exec(`UPDATE entry_decision_state SET symbol = ?
				WHERE scope = ? AND source = ? AND strategy_id = ? AND symbol = ? AND interval_ms = ? AND target_side = ?`,
				p.Canonical, string(p.Partition.Scope), p.Partition.Source, entrySID, k.Symbol, k.IntervalMs, k.Side)
			if err != nil {
				return fmt.Errorf("rename entry decision %s %d %s for %s: %w", k.Symbol, k.IntervalMs, k.Side, p.StrategyID, err)
			}
			if err := requireOneRow(res, fmt.Sprintf("entry decision %s %dms %s for %s", k.Symbol, k.IntervalMs, k.Side, p.StrategyID)); err != nil {
				return err
			}
		}
	}

	if storeCommitHook != nil {
		if err := storeCommitHook(sdb.storageRoleOf()); err != nil {
			return fmt.Errorf("commit hook (%s): %w", sdb.storageRoleOf(), err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit robinhood identity migration for %s: %w", p.StrategyID, err)
	}
	if staged != nil {
		p.book.Positions = staged.Positions
		p.book.OptionPositions = staged.OptionPositions
		p.book.RiskState.PendingCircuitCloses = staged.RiskState.PendingCircuitCloses
		p.book.RiskState.RobinhoodSharedCloseNotice = staged.RiskState.RobinhoodSharedCloseNotice
	}
	return nil
}

func checkRobinhoodIdentityPositionsTx(tx *sql.Tx, sid string, p *robinhoodIdentityPlan) error {
	rows, err := tx.Query(`SELECT symbol FROM positions WHERE strategy_id = ? ORDER BY symbol`, sid)
	if err != nil {
		return fmt.Errorf("read positions of %s before the identity migration: %w", p.StrategyID, err)
	}
	var stored []string
	for rows.Next() {
		var sym string
		if err := rows.Scan(&sym); err != nil {
			rows.Close()
			return err
		}
		stored = append(stored, sym)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	want := append([]string(nil), p.positionSymbols...)
	sort.Strings(want)
	if strings.Join(stored, "\x00") != strings.Join(want, "\x00") {
		return fmt.Errorf("positions of %s changed after the identity preflight (stored %q, planned %q); refusing", p.StrategyID, stored, want)
	}
	return nil
}

func requireOneRow(res sql.Result, what string) error {
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("%s: rows affected: %w", what, err)
	}
	if n != 1 {
		return fmt.Errorf("%s changed after the identity preflight (%d rows matched, want 1); refusing", what, n)
	}
	return nil
}

type robinhoodIdentityApplyError struct {
	failed    string
	committed []string
	err       error
}

func (e *robinhoodIdentityApplyError) Error() string {
	committed := "none"
	if len(e.committed) > 0 {
		committed = strings.Join(e.committed, ", ")
	}
	return fmt.Sprintf("Robinhood coin identity migration failed for %s: %v. That book and its order rows are unchanged; books already committed: %s. Start this build again to finish the remaining books. Until then never let an older go-trader binary trade these files: it reads each book by the configured spelling and would see a committed canonical book as flat, so keep the service stopped or write each canonical coin into args[1] in the config first (#1908)",
		e.failed, e.err, committed)
}

func (e *robinhoodIdentityApplyError) Unwrap() error { return e.err }

func applyRobinhoodIdentityMigration(store *StateStore, plans []*robinhoodIdentityPlan) ([]string, error) {
	var lines []string
	var committed []string
	for _, p := range plans {
		if err := store.MigrateRobinhoodIdentity(p); err != nil {
			return lines, &robinhoodIdentityApplyError{failed: fmt.Sprintf("%s (%s state file)", p.StrategyID, p.Role), committed: committed, err: err}
		}
		committed = append(committed, fmt.Sprintf("%s (%s state file)", p.StrategyID, p.Role))
		lines = append(lines, p.logLine())
	}
	return lines, nil
}

func migrateRobinhoodCoinIdentity(cfg *Config, store *StateStore, state *AppState) ([]string, error) {
	plans, err := planRobinhoodIdentityMigration(cfg, store, state)
	if err != nil {
		return nil, err
	}
	return applyRobinhoodIdentityMigration(store, plans)
}

type robinhoodIdentityStartupError struct {
	err error
}

func (e *robinhoodIdentityStartupError) Error() string { return e.err.Error() }

func (e *robinhoodIdentityStartupError) Unwrap() error { return e.err }

func prepareStartupState(cfg *Config, store *StateStore, writable bool) (*AppState, []storageOrphan, []string, error) {
	state, orphans, lines, _, err := prepareStartupStateReport(cfg, store, writable)
	return state, orphans, lines, err
}

func prepareStartupStateReport(cfg *Config, store *StateStore, writable bool) (*AppState, []storageOrphan, []string, []string, error) {
	state, orphans, legacyPending, err := loadStateWithStore(cfg, store, writable)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	if !writable {
		return state, orphans, nil, nil, nil
	}
	lines, err := migrateRobinhoodCoinIdentity(cfg, store, state)
	if err != nil {
		return nil, nil, lines, nil, &robinhoodIdentityStartupError{err: err}
	}
	bindingLines, err := backfillRobinhoodAccountBindings(cfg, store, state)
	if err != nil {
		return nil, nil, lines, bindingLines, &robinhoodBindingStartupError{err: err}
	}
	bindingLines = append(bindingLines, robinhoodAccountReportLines(state, cfg.Strategies)...)
	if legacyPending {
		if err := persistLegacyPortfolioPlacement(state, store); err != nil {
			return nil, nil, lines, bindingLines, err
		}
	}
	return state, orphans, lines, bindingLines, nil
}

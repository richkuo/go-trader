package main

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"math"
	"sort"
	"strings"
	"sync"
	"time"
)

const robinhoodAccountBindingCapability = "go-trader-capability:robinhood-account-binding-v1"

const (
	robinhoodBindingSourceSettlement = "settlement"
	robinhoodBindingSourceBackfill   = "backfill"
	robinhoodBindingSourceOperator   = "operator:"
)

type robinhoodAccountState string

const (
	robinhoodAccountCurrent robinhoodAccountState = "current"
	robinhoodAccountUnbound robinhoodAccountState = "unbound"
	robinhoodAccountForeign robinhoodAccountState = "foreign"
)

func robinhoodAccountStateOf(accountKey, venueAccountID, currentKey, readAccountID string) robinhoodAccountState {
	switch {
	case currentKey == "":
		return robinhoodAccountForeign
	case accountKey != "" && accountKey != currentKey:
		return robinhoodAccountForeign
	case venueAccountID != "" && readAccountID != "" && venueAccountID != readAccountID:
		return robinhoodAccountForeign
	case accountKey == "" || venueAccountID == "":
		return robinhoodAccountUnbound
	}
	return robinhoodAccountCurrent
}

func robinhoodBookAccountState(pos *Position, currentKey, readAccountID string) robinhoodAccountState {
	if pos == nil {
		return robinhoodAccountUnbound
	}
	return robinhoodAccountStateOf(pos.AccountKey, pos.VenueAccountID, currentKey, readAccountID)
}

func robinhoodAccountFingerprint(kind, value string) string {
	if strings.TrimSpace(value) == "" {
		return "none"
	}
	sum := sha256.Sum256([]byte("go-trader/robinhood-" + kind + "-fingerprint/v1:" + value))
	return hex.EncodeToString(sum[:4])
}

func robinhoodKeyFingerprint(key string) string { return robinhoodAccountFingerprint("key", key) }

func robinhoodVenueFingerprint(id string) string { return robinhoodAccountFingerprint("venue", id) }

func robinhoodPositionHeld(pos *Position) bool {
	return pos != nil && pos.Quantity > 0 && robinhoodFinite(pos.Quantity)
}

func robinhoodAccountHoldReason(state robinhoodAccountState, accountKey, venueAccountID, currentKey, readAccountID string) string {
	switch state {
	case robinhoodAccountForeign:
		switch {
		case currentKey == "":
			return "ROBINHOOD_USERNAME is not set, so no configured account can own the book"
		case accountKey != "" && accountKey != currentKey:
			return fmt.Sprintf("the book is bound to account %s, and the configured account is %s", robinhoodKeyFingerprint(accountKey), robinhoodKeyFingerprint(currentKey))
		default:
			return fmt.Sprintf("the book is bound to venue account %s, and the holdings read used venue account %s", robinhoodVenueFingerprint(venueAccountID), robinhoodVenueFingerprint(readAccountID))
		}
	case robinhoodAccountUnbound:
		if accountKey == "" {
			return "the book has no account binding"
		}
		return "the book has no venue account binding"
	}
	return ""
}

type robinhoodBookEntry struct {
	StrategyID     string
	PositionID     string
	Side           string
	Quantity       float64
	AccountKey     string
	VenueAccountID string
}

func robinhoodBookEntryOf(strategyID string, pos *Position) robinhoodBookEntry {
	return robinhoodBookEntry{
		StrategyID:     strategyID,
		PositionID:     pos.TradePositionID,
		Side:           pos.Side,
		Quantity:       pos.Quantity,
		AccountKey:     pos.AccountKey,
		VenueAccountID: pos.VenueAccountID,
	}
}

func (e robinhoodBookEntry) matches(pos *Position) bool {
	return pos != nil && e.PositionID == pos.TradePositionID && e.Side == pos.Side &&
		math.Float64bits(e.Quantity) == math.Float64bits(pos.Quantity) &&
		e.AccountKey == pos.AccountKey && e.VenueAccountID == pos.VenueAccountID
}

func (e robinhoodBookEntry) held() bool {
	return e.Quantity > 0 && robinhoodFinite(e.Quantity)
}

func (e robinhoodBookEntry) longQty() float64 {
	if e.Side == "long" && e.held() {
		return e.Quantity
	}
	return 0
}

func robinhoodEntriesLongQty(entries []robinhoodBookEntry) float64 {
	total := 0.0
	for _, e := range entries {
		total += e.longQty()
	}
	return total
}

func robinhoodEntriesEqual(a, b []robinhoodBookEntry) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].StrategyID != b[i].StrategyID || a[i].PositionID != b[i].PositionID || a[i].Side != b[i].Side ||
			math.Float64bits(a[i].Quantity) != math.Float64bits(b[i].Quantity) || a[i].AccountKey != b[i].AccountKey || a[i].VenueAccountID != b[i].VenueAccountID {
			return false
		}
	}
	return true
}

func (b robinhoodBookSnapshot) split(coin, readAccountID string) (eligible, held []robinhoodBookEntry) {
	for _, e := range b.Books[coin] {
		state := robinhoodAccountStateOf(e.AccountKey, e.VenueAccountID, b.CurrentKey, readAccountID)
		switch {
		case state == robinhoodAccountCurrent:
			eligible = append(eligible, e)
		case e.held():
			held = append(held, e)
		}
	}
	return eligible, held
}

func robinhoodAccountHoldError(coin string, held []robinhoodBookEntry, currentKey, readAccountID string) error {
	if len(held) == 0 {
		return nil
	}
	parts := make([]string, 0, len(held))
	total := 0.0
	for _, e := range held {
		state := robinhoodAccountStateOf(e.AccountKey, e.VenueAccountID, currentKey, readAccountID)
		parts = append(parts, fmt.Sprintf("%s %.10g (%s: %s)", e.StrategyID, e.Quantity, state, robinhoodAccountHoldReason(state, e.AccountKey, e.VenueAccountID, currentKey, readAccountID)))
		total += e.Quantity
	}
	sort.Strings(parts)
	return fmt.Errorf("account hold: %.10g %s in go-trader books is not proven to belong to the logged-in Robinhood account [%s]; no sale or flat reconciliation for those books until the owner runs /%s%s (#1921)",
		total, coin, strings.Join(parts, "; "), commandPrefix, robinhoodAccountCommandName)
}

type robinhoodFlatProof struct {
	Coin          string
	ReadAccountID string
	AccountKey    string
	Positions     []robinhoodBookEntry
}

type robinhoodFlatProofs map[string]robinhoodFlatProof

func (p robinhoodFlatProof) authorizes(strategyID string, pos *Position, currentKey string) error {
	if pos == nil {
		return fmt.Errorf("no position")
	}
	if strings.TrimSpace(p.ReadAccountID) == "" {
		return fmt.Errorf("the flat proof names no venue account")
	}
	if currentKey == "" || p.AccountKey != currentKey {
		return fmt.Errorf("the configured Robinhood account changed after the flat read")
	}
	var entry *robinhoodBookEntry
	for i := range p.Positions {
		if p.Positions[i].StrategyID == strategyID {
			entry = &p.Positions[i]
			break
		}
	}
	if entry == nil {
		return fmt.Errorf("the flat read did not cover this position")
	}
	if !entry.matches(pos) {
		return fmt.Errorf("the position, its quantity or its account binding changed after the flat read")
	}
	if state := robinhoodBookAccountState(pos, currentKey, p.ReadAccountID); state != robinhoodAccountCurrent {
		return fmt.Errorf("the book is %s: %s", state, robinhoodAccountHoldReason(state, pos.AccountKey, pos.VenueAccountID, currentKey, p.ReadAccountID))
	}
	return nil
}

func robinhoodProofFor(coin, readAccountID, accountKey string, entries []robinhoodBookEntry) robinhoodFlatProof {
	return robinhoodFlatProof{Coin: coin, ReadAccountID: readAccountID, AccountKey: accountKey, Positions: append([]robinhoodBookEntry(nil), entries...)}
}

func (p robinhoodFlatProofs) only(coins []string) robinhoodFlatProofs {
	out := robinhoodFlatProofs{}
	for _, c := range coins {
		if proof, ok := p[c]; ok {
			out[c] = proof
		}
	}
	return out
}

func (p robinhoodFlatProofs) sortedCoins() []string {
	out := make([]string, 0, len(p))
	for c := range p {
		out = append(out, c)
	}
	sort.Strings(out)
	return out
}

type robinhoodHeldBook struct {
	StrategyID     string
	Coin           string
	PositionID     string
	Quantity       float64
	AccountKey     string
	VenueAccountID string
	Source         string
	State          robinhoodAccountState
	Reason         string
}

func robinhoodLiveBooksLocked(state *AppState, cfgs []StrategyConfig, currentKey string) []robinhoodHeldBook {
	var out []robinhoodHeldBook
	if state == nil {
		return nil
	}
	for _, sc := range cfgs {
		if !isRobinhoodLiveSpot(sc) {
			continue
		}
		s := state.Strategies[sc.ID]
		if s == nil {
			continue
		}
		coin := robinhoodSymbol(sc.Args)
		pos := s.Positions[coin]
		if !robinhoodPositionHeld(pos) {
			continue
		}
		st := robinhoodBookAccountState(pos, currentKey, "")
		out = append(out, robinhoodHeldBook{
			StrategyID: sc.ID, Coin: coin, PositionID: pos.TradePositionID, Quantity: pos.Quantity,
			AccountKey: pos.AccountKey, VenueAccountID: pos.VenueAccountID, Source: pos.AccountBindingSource,
			State: st, Reason: robinhoodAccountHoldReason(st, pos.AccountKey, pos.VenueAccountID, currentKey, ""),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].StrategyID < out[j].StrategyID })
	return out
}

func robinhoodAccountReportLines(state *AppState, cfgs []StrategyConfig) []string {
	key := robinhoodAccountKey()
	var lines []string
	for _, b := range robinhoodLiveBooksLocked(state, cfgs, key) {
		source := b.Source
		if source == "" {
			source = "none"
		}
		lines = append(lines, fmt.Sprintf("[robinhood-account] %s %s qty=%.10g position=%s state=%s bound_key=%s bound_venue=%s source=%s current_key=%s (#1921)",
			b.StrategyID, b.Coin, b.Quantity, b.PositionID, b.State, robinhoodKeyFingerprint(b.AccountKey), robinhoodVenueFingerprint(b.VenueAccountID), source, robinhoodKeyFingerprint(key)))
	}
	sort.Strings(lines)
	return lines
}

const robinhoodAccountCommandName = "resolve-rh-account"

const (
	robinhoodAccountActionBind    = "bind-current"
	robinhoodAccountActionRelease = "release-book"
)

func formatRobinhoodHeldBookAlert(b robinhoodHeldBook, currentKey string) string {
	return fmt.Sprintf("**ROBINHOOD BOOK HELD** [%s] %.10g %s (position %s) is %s: %s. Bound account %s, bound venue account %s, configured account %s. go-trader sends no sale and makes no flat reconciliation for this book. If the coins are on the logged-in account, run /%s%s %s %s. If they are outside every account this daemon manages, run /%s%s %s %s. Flatten and confirm the old account before you change the Robinhood account.",
		b.StrategyID, b.Quantity, b.Coin, b.PositionID, b.State, b.Reason,
		robinhoodKeyFingerprint(b.AccountKey), robinhoodVenueFingerprint(b.VenueAccountID), robinhoodKeyFingerprint(currentKey),
		commandPrefix, robinhoodAccountCommandName, b.StrategyID, robinhoodAccountActionBind,
		commandPrefix, robinhoodAccountCommandName, b.StrategyID, robinhoodAccountActionRelease)
}

var robinhoodHeldBookAlerts sync.Map

func robinhoodHeldBookAlertKey(b robinhoodHeldBook) string {
	return b.StrategyID + "|" + b.Coin + "|" + b.PositionID + "|" + string(b.State)
}

func collectRobinhoodHeldBookAlerts(state *AppState, cfgs []StrategyConfig, mu *sync.RWMutex, now time.Time) []string {
	key := robinhoodAccountKey()
	mu.RLock()
	books := robinhoodLiveBooksLocked(state, cfgs, key)
	mu.RUnlock()
	interval := effectiveAlertThrottleInterval()
	var msgs []string
	for _, b := range books {
		if b.State == robinhoodAccountCurrent {
			continue
		}
		msg := formatRobinhoodHeldBookAlert(b, key)
		k := robinhoodHeldBookAlertKey(b)
		if v, ok := robinhoodHeldBookAlerts.Load(k); ok {
			if last, _ := v.(time.Time); now.Sub(last) < interval {
				continue
			}
		}
		robinhoodHeldBookAlerts.Store(k, now)
		fmt.Printf("[CRITICAL] %s\n", msg)
		msgs = append(msgs, msg)
	}
	return msgs
}

func notifyRobinhoodHeldBooks(state *AppState, cfgs []StrategyConfig, mu *sync.RWMutex, notifier *MultiNotifier, now time.Time) {
	for _, msg := range collectRobinhoodHeldBookAlerts(state, cfgs, mu, now) {
		if notifier == nil || !notifier.HasOwner() {
			continue
		}
		notifier.SendOwnerDM(msg)
	}
}

type robinhoodBindingPlan struct {
	StrategyID     string
	Coin           string
	PositionID     string
	AccountKey     string
	VenueAccountID string
	Role           storageRole

	book *StrategyState
}

func (p *robinhoodBindingPlan) label() string {
	return fmt.Sprintf("%s (%s state file)", p.StrategyID, p.Role)
}

var robinhoodBindingReadHook func(procID string) error

func (sdb *StateDB) robinhoodBindingEvidence(procID, coin, positionID string) (accountKey, venueAccountID, problem string, err error) {
	if robinhoodBindingReadHook != nil {
		if err := robinhoodBindingReadHook(procID); err != nil {
			return "", "", "", err
		}
	}
	sid, err := sdb.toStorageID(procID)
	if err != nil {
		return "", "", "", err
	}
	rows, err := sdb.db.Query(`SELECT symbol, exchange_order_id FROM trades WHERE strategy_id = ? AND position_id = ? AND is_close = 0 ORDER BY rowid`, sid, positionID)
	if err != nil {
		return "", "", "", fmt.Errorf("read the opening trades of %s in the %s state file: %w", procID, sdb.storageRoleOf(), err)
	}
	var orderIDs []string
	opens := 0
	for rows.Next() {
		var symbol, oid string
		if err := rows.Scan(&symbol, &oid); err != nil {
			rows.Close()
			return "", "", "", fmt.Errorf("scan an opening trade of %s: %w", procID, err)
		}
		if canonicalRobinhoodCoin(symbol) != coin {
			continue
		}
		opens++
		if strings.TrimSpace(oid) != "" {
			orderIDs = append(orderIDs, oid)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return "", "", "", fmt.Errorf("iterate the opening trades of %s: %w", procID, err)
	}
	switch {
	case opens == 0:
		return "", "", "no opening trade is stored for the position", nil
	case opens > 1:
		return "", "", fmt.Sprintf("%d opening trades are stored for the position", opens), nil
	case len(orderIDs) != 1:
		return "", "", "the opening trade has no venue order id", nil
	}
	exists, err := tableExists(sdb, "robinhood_order_attempts")
	if err != nil {
		return "", "", "", err
	}
	if !exists {
		return "", "", "no Robinhood order attempt table is stored", nil
	}
	attemptSID, err := sdb.robinhoodAttemptOwner(procID)
	if err != nil {
		return "", "", "", err
	}
	arows, err := sdb.db.Query(`SELECT symbol, account_key, venue_account_id FROM robinhood_order_attempts
		WHERE scope = 'live' AND source = '' AND strategy_id = ? AND origin = 'signal' AND side = 'buy' AND status = 'settled' AND venue_order_id = ?
		ORDER BY attempt_id`, attemptSID, orderIDs[0])
	if err != nil {
		return "", "", "", fmt.Errorf("read the settled buy attempts of %s in the %s state file: %w", procID, sdb.storageRoleOf(), err)
	}
	type match struct{ key, venue string }
	var matches []match
	for arows.Next() {
		var symbol, key, venue string
		if err := arows.Scan(&symbol, &key, &venue); err != nil {
			arows.Close()
			return "", "", "", fmt.Errorf("scan a settled buy attempt of %s: %w", procID, err)
		}
		if canonicalRobinhoodCoin(symbol) == coin {
			matches = append(matches, match{key: key, venue: venue})
		}
	}
	arows.Close()
	if err := arows.Err(); err != nil {
		return "", "", "", fmt.Errorf("iterate the settled buy attempts of %s: %w", procID, err)
	}
	switch {
	case len(matches) == 0:
		return "", "", "no settled live signal buy matches the opening order", nil
	case len(matches) > 1:
		return "", "", fmt.Sprintf("%d settled live signal buys match the opening order", len(matches)), nil
	case strings.TrimSpace(matches[0].key) == "":
		return "", "", "the matching settled buy has no account key", nil
	}
	return matches[0].key, matches[0].venue, "", nil
}

func (sdb *StateDB) applyRobinhoodBinding(p *robinhoodBindingPlan) error {
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
	tx, err := sdb.db.Begin()
	if err != nil {
		return fmt.Errorf("begin the Robinhood account binding of %s: %w", p.StrategyID, err)
	}
	defer tx.Rollback()
	res, err := tx.Exec(`UPDATE positions SET account_key = ?, venue_account_id = CASE WHEN venue_account_id = '' THEN ? ELSE venue_account_id END, account_binding_source = ?
		WHERE strategy_id = ? AND symbol = ? AND position_id = ? AND account_key = '' AND (venue_account_id = '' OR venue_account_id = ?)`,
		p.AccountKey, p.VenueAccountID, robinhoodBindingSourceBackfill, sid, p.Coin, p.PositionID, p.VenueAccountID)
	if err != nil {
		return fmt.Errorf("bind the Robinhood book of %s: %w", p.StrategyID, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("bind the Robinhood book of %s: rows affected: %w", p.StrategyID, err)
	}
	if n != 1 {
		return fmt.Errorf("the Robinhood book of %s changed after the binding preflight (%d rows matched, want 1); refusing", p.StrategyID, n)
	}
	if storeCommitHook != nil {
		if err := storeCommitHook(sdb.storageRoleOf()); err != nil {
			return fmt.Errorf("commit hook (%s): %w", sdb.storageRoleOf(), err)
		}
	}
	return tx.Commit()
}

func (st *StateStore) planRobinhoodAccountBackfill(cfgs []StrategyConfig, state *AppState) ([]*robinhoodBindingPlan, []string, error) {
	if st == nil || state == nil {
		return nil, nil, fmt.Errorf("robinhood account backfill needs the state store and the loaded state")
	}
	var plans []*robinhoodBindingPlan
	var notes []string
	for _, sc := range cfgs {
		if !isRobinhoodLiveSpot(sc) {
			continue
		}
		s := state.Strategies[sc.ID]
		if s == nil {
			continue
		}
		coin := robinhoodSymbol(sc.Args)
		pos := s.Positions[coin]
		if !robinhoodPositionHeld(pos) || pos.Side != "long" || pos.AccountKey != "" {
			continue
		}
		if strings.TrimSpace(pos.TradePositionID) == "" {
			notes = append(notes, fmt.Sprintf("[robinhood-account] %s %s stays unbound: the book has no position id (#1921)", sc.ID, coin))
			continue
		}
		db, err := st.dbForStrategy(sc.ID)
		if err != nil {
			return nil, nil, err
		}
		key, venue, problem, err := db.robinhoodBindingEvidence(sc.ID, coin, pos.TradePositionID)
		if err != nil {
			return nil, nil, err
		}
		if problem == "" && pos.VenueAccountID != "" && venue != "" && venue != pos.VenueAccountID {
			problem = "the stored venue account differs from the settled buy's venue account"
		}
		if problem != "" {
			notes = append(notes, fmt.Sprintf("[robinhood-account] %s %s stays unbound: %s (#1921)", sc.ID, coin, problem))
			continue
		}
		if venue == "" {
			venue = pos.VenueAccountID
		}
		plans = append(plans, &robinhoodBindingPlan{StrategyID: sc.ID, Coin: coin, PositionID: pos.TradePositionID, AccountKey: key, VenueAccountID: venue, Role: db.storageRoleOf(), book: s})
	}
	sort.Slice(plans, func(i, j int) bool { return plans[i].StrategyID < plans[j].StrategyID })
	sort.Strings(notes)
	return plans, notes, nil
}

type robinhoodBindingApplyError struct {
	failed    string
	committed []string
	err       error
}

func (e *robinhoodBindingApplyError) Error() string {
	committed := "none"
	if len(e.committed) > 0 {
		committed = strings.Join(e.committed, ", ")
	}
	return fmt.Sprintf("Robinhood account binding backfill failed for %s: %v. That book is unchanged; books already bound: %s. Start this build again to finish the remaining books; each bound book is skipped on the next start (#1921)", e.failed, e.err, committed)
}

func (e *robinhoodBindingApplyError) Unwrap() error { return e.err }

func backfillRobinhoodAccountBindings(cfg *Config, store *StateStore, state *AppState) ([]string, error) {
	if cfg == nil {
		return nil, fmt.Errorf("robinhood account backfill needs the config")
	}
	plans, lines, err := store.planRobinhoodAccountBackfill(cfg.Strategies, state)
	if err != nil {
		return nil, fmt.Errorf("Robinhood account binding preflight failed; no book was bound: %w (#1921)", err)
	}
	var committed []string
	for _, p := range plans {
		db, err := store.dbForStrategy(p.StrategyID)
		if err == nil {
			err = db.applyRobinhoodBinding(p)
		}
		if err != nil {
			return lines, &robinhoodBindingApplyError{failed: p.label(), committed: committed, err: err}
		}
		pos := p.book.Positions[p.Coin]
		pos.AccountKey = p.AccountKey
		if pos.VenueAccountID == "" {
			pos.VenueAccountID = p.VenueAccountID
		}
		pos.AccountBindingSource = robinhoodBindingSourceBackfill
		committed = append(committed, p.label())
		lines = append(lines, fmt.Sprintf("[robinhood-account] %s %s bound by backfill: key=%s venue=%s (#1921)", p.StrategyID, p.Coin, robinhoodKeyFingerprint(p.AccountKey), robinhoodVenueFingerprint(pos.VenueAccountID)))
	}
	return lines, nil
}

type robinhoodBindingStartupError struct {
	err error
}

func (e *robinhoodBindingStartupError) Error() string { return e.err.Error() }

func (e *robinhoodBindingStartupError) Unwrap() error { return e.err }

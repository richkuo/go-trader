package main

import (
	"database/sql"
	"fmt"
	"sort"
	"strings"
)

type retiredVenueQueryer interface {
	Query(query string, args ...any) (*sql.Rows, error)
	QueryRow(query string, args ...any) *sql.Row
}

type retiredOptionBook struct {
	StorageID       string
	Type            string
	StoredPlatform  string
	Venue           string
	PositionRows    int
	OptionLegRows   int
	UnresolvedOwner bool
}

func legacyStoredStrategyVenue(id, typ, platform string) string {
	if p := normalizedVenue(platform); p != "" {
		return p
	}
	switch {
	case strings.HasPrefix(id, "ibkr-"):
		return "ibkr"
	case strings.HasPrefix(id, "deribit-"):
		return "deribit"
	case strings.HasPrefix(id, "hl-"):
		return "hyperliquid"
	case strings.HasPrefix(id, "ts-"):
		return "topstep"
	case strings.HasPrefix(id, "rh-"):
		return "robinhood"
	case strings.HasPrefix(id, "luno-"):
		return "luno"
	case strings.HasPrefix(id, "okx-"):
		return "okx"
	case strings.TrimSpace(typ) == "options":
		return "deribit"
	}
	return "binanceus"
}

func retiredOptionVenueBook(venue, typ string, optionLegs int) bool {
	switch venue {
	case "deribit":
		return true
	case "okx":
		return strings.TrimSpace(typ) == "options" || optionLegs > 0
	}
	return false
}

func retiredVenueTableExists(q retiredVenueQueryer, table string) (bool, error) {
	var name string
	err := q.QueryRow("SELECT name FROM sqlite_master WHERE type = 'table' AND name = ?", table).Scan(&name)
	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("probe table %q: %w", table, err)
	}
	return true, nil
}

func retiredVenueColumnExists(q retiredVenueQueryer, table, column string) (bool, error) {
	var n int
	if err := q.QueryRow("SELECT COUNT(*) FROM pragma_table_info(?) WHERE name = ?", table, column).Scan(&n); err != nil {
		return false, fmt.Errorf("probe column %s.%s: %w", table, column, err)
	}
	return n > 0, nil
}

func countLegRowsByStrategy(q retiredVenueQueryer, table string, present bool) (map[string]int, error) {
	out := map[string]int{}
	if !present {
		return out, nil
	}
	rows, err := q.Query("SELECT strategy_id, COUNT(*) FROM " + table + " GROUP BY strategy_id")
	if err != nil {
		return nil, fmt.Errorf("count %s rows: %w", table, err)
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		var n int
		if err := rows.Scan(&id, &n); err != nil {
			return nil, fmt.Errorf("scan %s row count: %w", table, err)
		}
		out[id] = n
	}
	return out, rows.Err()
}

// readRetiredOptionVenueBooks lists every stored book in one file that a
// retired venue owned, flat or not, plus every leg row whose owning strategy
// row is missing. It reads only and tolerates older schemas.
func readRetiredOptionVenueBooks(q retiredVenueQueryer) ([]retiredOptionBook, error) {
	posPresent, err := retiredVenueTableExists(q, "positions")
	if err != nil {
		return nil, err
	}
	optPresent, err := retiredVenueTableExists(q, "option_positions")
	if err != nil {
		return nil, err
	}
	posCounts, err := countLegRowsByStrategy(q, "positions", posPresent)
	if err != nil {
		return nil, err
	}
	optCounts, err := countLegRowsByStrategy(q, "option_positions", optPresent)
	if err != nil {
		return nil, err
	}
	stratPresent, err := retiredVenueTableExists(q, "strategies")
	if err != nil {
		return nil, err
	}
	type stored struct{ typ, platform string }
	known := map[string]stored{}
	if stratPresent {
		hasPlatform, err := retiredVenueColumnExists(q, "strategies", "platform")
		if err != nil {
			return nil, err
		}
		query := "SELECT id, type, '' FROM strategies"
		if hasPlatform {
			query = "SELECT id, type, platform FROM strategies"
		}
		rows, err := q.Query(query)
		if err != nil {
			return nil, fmt.Errorf("read stored strategy venues: %w", err)
		}
		for rows.Next() {
			var id string
			var typ, platform sql.NullString
			if err := rows.Scan(&id, &typ, &platform); err != nil {
				rows.Close()
				return nil, fmt.Errorf("scan stored strategy venue: %w", err)
			}
			known[id] = stored{typ: typ.String, platform: platform.String}
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, fmt.Errorf("iterate stored strategy venues: %w", err)
		}
	}
	ids := map[string]bool{}
	for id := range posCounts {
		ids[id] = true
	}
	for id := range optCounts {
		ids[id] = true
	}
	for id := range known {
		ids[id] = true
	}
	sorted := make([]string, 0, len(ids))
	for id := range ids {
		sorted = append(sorted, id)
	}
	sort.Strings(sorted)
	var out []retiredOptionBook
	for _, id := range sorted {
		book := retiredOptionBook{StorageID: id, PositionRows: posCounts[id], OptionLegRows: optCounts[id]}
		row, ok := known[id]
		if !ok {
			book.UnresolvedOwner = true
			out = append(out, book)
			continue
		}
		book.Type = row.typ
		book.StoredPlatform = row.platform
		book.Venue = legacyStoredStrategyVenue(id, row.typ, row.platform)
		if retiredOptionVenueBook(book.Venue, book.Type, book.OptionLegRows) {
			out = append(out, book)
		}
	}
	return out, nil
}

func (b retiredOptionBook) flat() bool {
	return b.PositionRows == 0 && b.OptionLegRows == 0
}

func (b retiredOptionBook) describeFlatTakeover(file string) string {
	stored := b.StoredPlatform
	if strings.TrimSpace(stored) == "" {
		stored = "(blank, resolved to " + b.Venue + " by the previous release)"
	}
	return fmt.Sprintf("%s: stored strategy id %q (type %q, platform %s) is a flat book of a retired venue that still holds its cash, risk state and trade history; Deribit support and OKX options support were removed in %s; a strategy on a supported venue cannot adopt this book, so give the replacement strategy a new id or a new storage_strategy_id and keep the retired row for its history (docs/POST_UPDATE_HISTORY.md)", file, b.StorageID, b.Type, stored, retiredOptionVenueIssue)
}

func (b retiredOptionBook) describe(file string) string {
	if b.UnresolvedOwner {
		return fmt.Sprintf("%s: stored strategy id %q holds %d position row(s) and %d option leg row(s) but has no strategy row, so its venue cannot be resolved; restore the strategy row from a backup or resolve the rows by hand before this release starts (%s)", file, b.StorageID, b.PositionRows, b.OptionLegRows, retiredOptionVenueIssue)
	}
	stored := b.StoredPlatform
	if strings.TrimSpace(stored) == "" {
		stored = "(blank, resolved to " + b.Venue + " by the previous release)"
	}
	return fmt.Sprintf("%s: stored strategy id %q (type %q, platform %s) holds %d position row(s) and %d option leg row(s) on a retired venue; Deribit support and OKX options support were removed in %s; %s", file, b.StorageID, b.Type, stored, b.PositionRows, b.OptionLegRows, retiredOptionVenueIssue, retiredOptionVenueRecovery)
}

func inspectRetiredOptionVenueBooks(db *StateDB, fi *storageFileInspection, ident storageIdentityMap) ([]string, error) {
	books, err := readRetiredOptionVenueBooks(db.db)
	if err != nil {
		return nil, fmt.Errorf("retired venue check of the %s state file %q: %w", fi.Role, fi.Path, err)
	}
	var out []string
	for _, b := range books {
		file := fmt.Sprintf("%s state file %q", fi.Role, fi.Path)
		if b.flat() {
			if procID, ok := ident.processFor(fi.Role, b.StorageID); ok {
				out = append(out, b.describeFlatTakeover(file)+fmt.Sprintf("; the current config maps process strategy %q to this stored id", procID))
			}
			continue
		}
		msg := b.describe(file)
		if procID, ok := ident.processFor(fi.Role, b.StorageID); ok {
			msg += fmt.Sprintf("; the current config maps process strategy %q to this stored id, so it cannot take over the book", procID)
		}
		out = append(out, msg)
	}
	return out, nil
}

// guardRetiredOptionBooksTx refuses a save that would drop, replace or
// reassign a stored book on a retired venue, inside the save transaction and
// before any write.
func (sdb *StateDB) guardRetiredOptionBooksTx(tx *sql.Tx, strategies []*StrategyState, sids []string, fullFile bool) error {
	file := fmt.Sprintf("%s state file %q", sdb.storageRoleOf(), sdb.path)
	for i, s := range strategies {
		if s == nil {
			continue
		}
		venue := legacyStoredStrategyVenue(sids[i], s.Type, s.Platform)
		if (len(s.Positions) > 0 || len(s.OptionPositions) > 0) && retiredOptionVenueBook(venue, s.Type, len(s.OptionPositions)) {
			return fmt.Errorf("refusing the save: %s would store strategy %q with %d position(s) and %d option leg(s) on the retired venue %q (%s); %s", file, s.ID, len(s.Positions), len(s.OptionPositions), venue, retiredOptionVenueIssue, retiredOptionVenueRecovery)
		}
	}
	books, err := readRetiredOptionVenueBooks(tx)
	if err != nil {
		return fmt.Errorf("refusing the save: retired venue check of the %s: %w", file, err)
	}
	if len(books) == 0 {
		return nil
	}
	incoming := make(map[string]bool, len(sids))
	for _, sid := range sids {
		incoming[sid] = true
	}
	for _, b := range books {
		if b.flat() {
			if incoming[b.StorageID] {
				return fmt.Errorf("refusing the save: %s", b.describeFlatTakeover(file))
			}
			continue
		}
		if b.UnresolvedOwner || fullFile || incoming[b.StorageID] {
			return fmt.Errorf("refusing the save: %s", b.describe(file))
		}
	}
	return nil
}

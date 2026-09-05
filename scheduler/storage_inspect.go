package main

import (
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"sort"
	"strings"
)

// Read-only ownership inspection. It opens every file with no schema DDL and no
// migration, tolerates a pre-#1509 schema, and never moves or deletes a row.
// Startup runs the same check before the first migrating open.

type storageStrategyRow struct {
	StorageID     string `json:"storage_strategy_id"`
	ProcessID     string `json:"process_strategy_id,omitempty"`
	Mapped        bool   `json:"mapped"`
	PositionCount int    `json:"position_count"`
}

type storageRiskRow struct {
	Scope   PortfolioScope `json:"scope"`
	Latched bool           `json:"kill_switch_active"`
}

type storageFileInspection struct {
	Role           storageRole          `json:"role"`
	Path           string               `json:"path"`
	Canonical      string               `json:"canonical_path"`
	Present        bool                 `json:"present"`
	LockHeld       bool                 `json:"lock_held"`
	LockHolderPID  int                  `json:"lock_holder_pid,omitempty"`
	ScopesOwned    []PortfolioScope     `json:"scopes_owned"`
	Strategies     []storageStrategyRow `json:"strategies"`
	Orphans        []storageStrategyRow `json:"orphans"`
	PendingActions int                  `json:"pending_manual_actions"`
	RiskRows       []storageRiskRow     `json:"portfolio_risk_rows"`
	LegacyUnscoped bool                 `json:"legacy_unscoped_risk_row"`
}

type storageInspection struct {
	Split      bool                    `json:"split"`
	Layout     string                  `json:"layout"`
	Files      []storageFileInspection `json:"files"`
	Rejections []string                `json:"rejections"`
}

func (si storageInspection) OK() bool { return len(si.Rejections) == 0 }

func inspectStorageOwnership(layout storageLayout, ident storageIdentityMap, cfg *Config, requireIdle bool) (storageInspection, error) {
	out := storageInspection{Split: layout.Split, Layout: layout.describe()}
	hasLive := cfg != nil && HasLiveStrategy(cfg.Strategies)

	for _, spec := range layout.Files {
		fi := storageFileInspection{
			Role:        spec.Role,
			Path:        spec.Path,
			Canonical:   spec.Canonical,
			ScopesOwned: layout.scopesForRole(spec.Role),
		}
		if pid, held := probeStateDBLockHolder(spec.Path); held {
			fi.LockHeld = true
			fi.LockHolderPID = pid
			if requireIdle {
				out.Rejections = append(out.Rejections,
					fmt.Sprintf("%s state file %q is owned by pid %d; stop the scheduler before inspecting for idleness", spec.Role, spec.Path, pid))
			}
		}
		if _, err := os.Stat(spec.Canonical); err != nil {
			if !errors.Is(err, fs.ErrNotExist) {
				return storageInspection{}, fmt.Errorf("stat %s state file %q: %w", spec.Role, spec.Path, err)
			}
			out.Files = append(out.Files, fi)
			continue
		}
		fi.Present = true
		db, err := openStateDBReadOnly(spec.Canonical)
		if err != nil {
			return storageInspection{}, fmt.Errorf("read-only open of the %s state file %q: %w", spec.Role, spec.Path, err)
		}
		rejections, err := inspectOneStorageFile(db, &fi, layout, ident, hasLive)
		db.Close()
		if err != nil {
			return storageInspection{}, err
		}
		out.Rejections = append(out.Rejections, rejections...)
		out.Files = append(out.Files, fi)
	}
	sort.Strings(out.Rejections)
	return out, nil
}

func inspectOneStorageFile(db *StateDB, fi *storageFileInspection, layout storageLayout, ident storageIdentityMap, hasLive bool) ([]string, error) {
	var rejections []string
	owned := make(map[PortfolioScope]bool, len(fi.ScopesOwned))
	for _, scope := range fi.ScopesOwned {
		owned[scope] = true
	}

	positionCounts := make(map[string]int)
	if ok, err := tableExists(db, "positions"); err != nil {
		return nil, err
	} else if ok {
		rows, err := db.db.Query("SELECT strategy_id, COUNT(*) FROM positions GROUP BY strategy_id")
		if err != nil {
			return nil, fmt.Errorf("count positions in the %s state file: %w", fi.Role, err)
		}
		for rows.Next() {
			var id string
			var n int
			if err := rows.Scan(&id, &n); err != nil {
				rows.Close()
				return nil, fmt.Errorf("scan position count in the %s state file: %w", fi.Role, err)
			}
			positionCounts[id] = n
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, fmt.Errorf("iterate position counts in the %s state file: %w", fi.Role, err)
		}
	}

	if ok, err := tableExists(db, "strategies"); err != nil {
		return nil, err
	} else if ok {
		rows, err := db.db.Query("SELECT id FROM strategies ORDER BY id")
		if err != nil {
			return nil, fmt.Errorf("read strategies in the %s state file: %w", fi.Role, err)
		}
		seen := make(map[string]bool)
		for rows.Next() {
			var storageID string
			if err := rows.Scan(&storageID); err != nil {
				rows.Close()
				return nil, fmt.Errorf("scan strategy row in the %s state file: %w", fi.Role, err)
			}
			if seen[storageID] {
				rejections = append(rejections,
					fmt.Sprintf("%s state file: duplicate stored strategy id %q", fi.Role, storageID))
				continue
			}
			seen[storageID] = true
			row := storageStrategyRow{StorageID: storageID, PositionCount: positionCounts[storageID]}
			if procID, ok := ident.processFor(fi.Role, storageID); ok {
				row.Mapped = true
				row.ProcessID = procID
				fi.Strategies = append(fi.Strategies, row)
			} else {
				if other, ok := foreignRoleForStorageID(ident, layout, fi.Role, storageID); ok {
					rejections = append(rejections,
						fmt.Sprintf("%s state file: stored strategy %q is mapped to the %s state file; move or re-map it by hand", fi.Role, storageID, other))
				}
				fi.Orphans = append(fi.Orphans, row)
			}
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, fmt.Errorf("iterate strategies in the %s state file: %w", fi.Role, err)
		}
	}

	if ok, err := tableExists(db, "pending_manual_actions"); err != nil {
		return nil, err
	} else if ok {
		if err := db.db.QueryRow("SELECT COUNT(*) FROM pending_manual_actions").Scan(&fi.PendingActions); err != nil {
			return nil, fmt.Errorf("count pending manual actions in the %s state file: %w", fi.Role, err)
		}
	}

	scopedTables := []string{"portfolio_risk", "kill_switch_events", "correlation_snapshot"}
	for _, table := range scopedTables {
		exists, err := tableExists(db, table)
		if err != nil {
			return nil, err
		}
		if !exists {
			continue
		}
		hasScope, err := db.tableHasColumn(table, "scope")
		if err != nil {
			return nil, fmt.Errorf("read %s schema in the %s state file: %w", table, fi.Role, err)
		}
		if !hasScope {
			// Pre-#1509 schema: every row is legacy and unscoped.
			var n int
			if err := db.db.QueryRow("SELECT COUNT(*) FROM " + table).Scan(&n); err != nil {
				return nil, fmt.Errorf("count %s in the %s state file: %w", table, fi.Role, err)
			}
			if n > 0 && table == "portfolio_risk" {
				fi.LegacyUnscoped = true
			}
			continue
		}
		query := "SELECT COALESCE(scope, '') AS scope, COUNT(*) FROM " + table + " GROUP BY scope"
		if table == "portfolio_risk" {
			query = "SELECT COALESCE(scope, '') AS scope, MAX(kill_switch_active) FROM portfolio_risk GROUP BY scope"
		}
		rows, err := db.db.Query(query)
		if err != nil {
			return nil, fmt.Errorf("read %s in the %s state file: %w", table, fi.Role, err)
		}
		for rows.Next() {
			var scopeStr string
			var v sql.NullInt64
			if err := rows.Scan(&scopeStr, &v); err != nil {
				rows.Close()
				return nil, fmt.Errorf("scan %s in the %s state file: %w", table, fi.Role, err)
			}
			scope := PortfolioScope(scopeStr)
			if table == "portfolio_risk" {
				fi.RiskRows = append(fi.RiskRows, storageRiskRow{Scope: scope, Latched: v.Int64 != 0})
			}
			if scope == scopeUnassigned {
				if table == "portfolio_risk" {
					fi.LegacyUnscoped = true
				}
				continue
			}
			if !owned[scope] {
				rejections = append(rejections,
					fmt.Sprintf("%s state file: %s holds a %s-scope row but the file owns %s", fi.Role, table, scopeLabel(scope), scopeListLabel(fi.ScopesOwned)))
			}
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, fmt.Errorf("iterate %s in the %s state file: %w", table, fi.Role, err)
		}
	}

	if fi.LegacyUnscoped && layout.Split && fi.Role == storageRolePrimary && !hasLive {
		rejections = append(rejections,
			fmt.Sprintf("%s state file: an unscoped legacy portfolio_risk row cannot be placed — the split layout has no live strategy, so the row's historic mode is unknown; resolve it by hand", fi.Role))
	}
	return rejections, nil
}

func foreignRoleForStorageID(ident storageIdentityMap, layout storageLayout, role storageRole, storageID string) (storageRole, bool) {
	for _, other := range layout.roles() {
		if other == role {
			continue
		}
		if _, ok := ident.processFor(other, storageID); ok {
			return other, true
		}
	}
	return "", false
}

func tableExists(db *StateDB, table string) (bool, error) {
	var name string
	err := db.db.QueryRow("SELECT name FROM sqlite_master WHERE type = 'table' AND name = ?", table).Scan(&name)
	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("probe table %q: %w", table, err)
	}
	return true, nil
}

func scopeListLabel(scopes []PortfolioScope) string {
	if len(scopes) == 0 {
		return "no scope"
	}
	out := make([]string, 0, len(scopes))
	for _, scope := range scopes {
		out = append(out, scopeLabel(scope))
	}
	return strings.Join(out, "+")
}

func formatStorageInspection(si storageInspection) string {
	var b strings.Builder
	fmt.Fprintf(&b, "storage layout: %s\n", si.Layout)
	for _, fi := range si.Files {
		fmt.Fprintf(&b, "\n[%s] %s\n", fi.Role, fi.Path)
		fmt.Fprintf(&b, "  canonical: %s\n", fi.Canonical)
		fmt.Fprintf(&b, "  present: %v\n", fi.Present)
		fmt.Fprintf(&b, "  owns scopes: %s\n", scopeListLabel(fi.ScopesOwned))
		if fi.LockHeld {
			fmt.Fprintf(&b, "  lock: held by pid %d\n", fi.LockHolderPID)
		} else {
			fmt.Fprintf(&b, "  lock: free\n")
		}
		if !fi.Present {
			continue
		}
		fmt.Fprintf(&b, "  strategies: %d mapped, %d orphan\n", len(fi.Strategies), len(fi.Orphans))
		for _, row := range fi.Strategies {
			fmt.Fprintf(&b, "    %s -> %s (%d position(s))\n", row.StorageID, row.ProcessID, row.PositionCount)
		}
		for _, row := range fi.Orphans {
			marker := ""
			if row.PositionCount > 0 {
				marker = "  ** holds positions **"
			}
			fmt.Fprintf(&b, "    ORPHAN %s (%d position(s))%s\n", row.StorageID, row.PositionCount, marker)
		}
		fmt.Fprintf(&b, "  pending manual actions: %d\n", fi.PendingActions)
		for _, row := range fi.RiskRows {
			fmt.Fprintf(&b, "  portfolio_risk[%s]: latched=%v\n", scopeLabel(row.Scope), row.Latched)
		}
		if fi.LegacyUnscoped {
			fmt.Fprintf(&b, "  legacy unscoped portfolio_risk row present\n")
		}
	}
	if len(si.Rejections) == 0 {
		b.WriteString("\nownership: OK\n")
		return b.String()
	}
	b.WriteString("\nownership REJECTED:\n")
	for _, r := range si.Rejections {
		fmt.Fprintf(&b, "  - %s\n", r)
	}
	return b.String()
}

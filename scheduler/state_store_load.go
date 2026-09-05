package main

import (
	"fmt"
	"os"
	"time"
)

// legacyScopeTargetForRole decides where one file's unscoped legacy risk row
// belongs. Current config alone cannot prove the historic mode of an arbitrary
// row, so an ambiguous split layout is rejected instead of guessed.
func legacyScopeTargetForRole(cfg *Config, layout storageLayout, role storageRole) (PortfolioScope, error) {
	hasLive := cfg != nil && HasLiveStrategy(cfg.Strategies)
	if !layout.Split {
		if hasLive {
			return ScopeLive, nil
		}
		return ScopePaper, nil
	}
	if role == storageRolePaper {
		return ScopePaper, nil
	}
	if hasLive {
		return ScopeLive, nil
	}
	return scopeUnassigned, fmt.Errorf("the primary state file holds an unscoped legacy portfolio risk row but the roster has no live strategy; resolve the row by hand (it cannot be placed from config alone)")
}

// LoadStateWithStore assembles the process state from every owned file. Paper
// scope loading never depends on the primary file's process metadata row.
func LoadStateWithStore(cfg *Config, store *StateStore) (*AppState, []storageOrphan, error) {
	if store == nil {
		return nil, nil, fmt.Errorf("state store unavailable")
	}
	state := NewAppState()
	var orphans []storageOrphan
	found := false

	primary := store.primary()
	if primary == nil || primary.db == nil {
		return nil, nil, fmt.Errorf("primary state file %q does not exist yet", cfg.DBFile)
	}
	meta, metaFound, err := primary.loadProcessMeta()
	if err != nil {
		return nil, nil, fmt.Errorf("sqlite load: %w", err)
	}
	if metaFound {
		found = true
		state.CycleCount = meta.CycleCount
		state.LastCycle = meta.LastCycle
		state.LastLeaderboardPostDate = meta.LastLeaderboardPostDate
		state.LastLeaderboardSummaries = meta.LastLeaderboardSummaries
		state.LastSummaryPost = meta.LastSummaryPost
	}

	type legacyRow struct {
		role  storageRole
		risk  *PortfolioRiskState
		snap  *CorrelationSnapshot
		scope PortfolioScope
	}
	var legacy []legacyRow

	for _, role := range store.order {
		db := store.file(role)
		if role != storageRolePrimary {
			// The paper file's own process metadata is read once and unioned
			// with primary precedence; the primary owns it from then on.
			if paperMeta, ok, err := db.loadProcessMeta(); err != nil {
				return nil, nil, fmt.Errorf("sqlite load (%s): %w", role, err)
			} else if ok {
				found = true
				unionTimeMapInto(state.LastSummaryPost, paperMeta.LastSummaryPost)
				unionTimeMapInto(state.LastLeaderboardSummaries, paperMeta.LastLeaderboardSummaries)
			}
		}
		books, err := db.loadScopeBooks(store.layout.scopesForRole(role))
		if err != nil {
			return nil, nil, fmt.Errorf("sqlite load (%s): %w", role, err)
		}
		if len(books.Strategies) > 0 || len(books.PortfolioRisk) > 0 || len(books.Orphans) > 0 {
			found = true
		}
		for id, s := range books.Strategies {
			state.Strategies[id] = s
		}
		orphans = append(orphans, books.Orphans...)
		row := legacyRow{role: role}
		for scope, prs := range books.PortfolioRisk {
			if scope == scopeUnassigned {
				row.risk = prs
				continue
			}
			state.PortfolioRisk[scope] = prs
		}
		for scope, snap := range books.CorrelationSnapshot {
			if scope == scopeUnassigned {
				row.snap = snap
				continue
			}
			state.CorrelationSnapshot[scope] = snap
		}
		if row.risk != nil || row.snap != nil {
			target, err := legacyScopeTargetForRole(cfg, store.layout, role)
			if err != nil {
				return nil, nil, err
			}
			row.scope = target
			legacy = append(legacy, row)
		}
	}

	if !found {
		return NewAppState(), nil, nil
	}

	moved := false
	for _, row := range legacy {
		placeLegacyPortfolioRisk(state, cfg, row.risk, row.snap, row.scope)
		fmt.Printf("[state] Legacy unscoped portfolio risk row in the %s state file placed in the %s scope\n", row.role, scopeLabel(row.scope))
		moved = true
	}
	if moved {
		if store.readOnly() {
			fmt.Fprintln(os.Stderr, "[state] WARN: legacy portfolio risk placement was applied in memory only; the state files are open read-only")
		} else {
			for scope, err := range store.SaveAll(state) {
				if err != nil {
					return nil, nil, fmt.Errorf("persist legacy portfolio scope placement (%s): %w", scopeLabel(scope), err)
				}
			}
		}
	}
	if migrated := migrateLegacyPerpsPositionMultipliers(state, cfg); migrated > 0 {
		fmt.Printf("[state] Migrated %d legacy perps position multiplier(s) to 1\n", migrated)
	}
	fmt.Println("[state] Loaded from SQLite")
	return state, orphans, nil
}

func unionTimeMapInto(dst, src map[string]time.Time) {
	if dst == nil || src == nil {
		return
	}
	for k, v := range src {
		if _, ok := dst[k]; ok {
			continue
		}
		dst[k] = v
	}
}

func SaveStateWithStore(state *AppState, store *StateStore) error {
	if store == nil {
		return fmt.Errorf("state store unavailable")
	}
	var firstErr error
	for _, scope := range sortedScopeErrors(store.SaveAll(state)) {
		if scope.err != nil && firstErr == nil {
			firstErr = fmt.Errorf("%s scope: %w", scopeLabel(scope.scope), scope.err)
		}
	}
	return firstErr
}

type scopeErr struct {
	scope PortfolioScope
	err   error
}

func sortedScopeErrors(m map[PortfolioScope]error) []scopeErr {
	out := make([]scopeErr, 0, len(m))
	for _, scope := range []PortfolioScope{ScopeLive, ScopePaper} {
		if err, ok := m[scope]; ok {
			out = append(out, scopeErr{scope: scope, err: err})
		}
	}
	return out
}

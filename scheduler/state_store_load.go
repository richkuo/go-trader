package main

import (
	"fmt"
	"os"
	"sort"
	"time"
)

// legacyPartitionTargetForRole decides where one file's unscoped legacy risk
// row belongs. A file that owns exactly one partition answers itself, so a
// folded paper source keeps its own legacy row. Current config alone cannot
// prove the historic mode of a row in a file that owns both modes, so that case
// is rejected instead of guessed.
func legacyPartitionTargetForRole(cfg *Config, layout storageLayout, role storageRole) (RiskPartition, error) {
	// A non-primary file was a standalone deployment's own file, so its legacy
	// row belongs to the one partition that file owns. The primary is the only
	// file whose historic mode config cannot prove.
	if role != storageRolePrimary {
		if parts := layout.partitionsForRole(role); len(parts) == 1 {
			return parts[0], nil
		}
	}
	hasLive := cfg != nil && HasLiveStrategy(cfg.Strategies)
	if !layout.Split {
		if hasLive {
			return livePartition, nil
		}
		return defaultPaperPartition, nil
	}
	if hasLive {
		return livePartition, nil
	}
	return unassignedPartition, fmt.Errorf("the primary state file holds an unscoped legacy portfolio risk row but the roster has no live strategy; resolve the row by hand (it cannot be placed from config alone)")
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
		role storageRole
		risk *PortfolioRiskState
		snap *CorrelationSnapshot
		part RiskPartition
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
		// Each loaded row is mapped from (file, scope) onto the partition that
		// file owns for that mode, so two paper source files never collide.
		for scope, prs := range books.PortfolioRisk {
			if scope == scopeUnassigned {
				row.risk = prs
				continue
			}
			part, ok := store.layout.partitionForRoleScope(role, scope)
			if !ok {
				return nil, nil, fmt.Errorf("the %s state file holds a %s portfolio risk row it does not own", role, scopeLabel(scope))
			}
			for i := range prs.Events {
				prs.Events[i].Partition = part
			}
			state.PortfolioRisk[part] = prs
		}
		for scope, snap := range books.CorrelationSnapshot {
			if scope == scopeUnassigned {
				row.snap = snap
				continue
			}
			part, ok := store.layout.partitionForRoleScope(role, scope)
			if !ok {
				return nil, nil, fmt.Errorf("the %s state file holds a %s correlation snapshot it does not own", role, scopeLabel(scope))
			}
			state.setPartitionCorrelation(part, snap)
		}
		if row.risk != nil || row.snap != nil {
			target, err := legacyPartitionTargetForRole(cfg, store.layout, role)
			if err != nil {
				return nil, nil, err
			}
			row.part = target
			legacy = append(legacy, row)
		}
	}

	if !found {
		return NewAppState(), nil, nil
	}

	moved := false
	for _, row := range legacy {
		placeLegacyPortfolioRisk(state, cfg, row.risk, row.snap, row.part)
		fmt.Printf("[state] Legacy unscoped portfolio risk row in the %s state file placed in the %s partition\n", row.role, partitionLabel(row.part))
		moved = true
	}
	if moved {
		if store.readOnly() {
			fmt.Fprintln(os.Stderr, "[state] WARN: legacy portfolio risk placement was applied in memory only; the state files are open read-only")
		} else {
			for part, err := range store.SaveAll(state) {
				if err != nil {
					return nil, nil, fmt.Errorf("persist legacy portfolio partition placement (%s): %w", partitionLabel(part), err)
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
	for _, pe := range sortedPartitionErrors(store.SaveAll(state)) {
		if pe.err != nil && firstErr == nil {
			firstErr = fmt.Errorf("%s scope: %w", partitionLabel(pe.part), pe.err)
		}
	}
	return firstErr
}

type partitionErr struct {
	part RiskPartition
	err  error
}

// sortedPartitionErrors reports failures in the stable partition order, so the
// first error an operator sees is deterministic across runs.
func sortedPartitionErrors(m map[RiskPartition]error) []partitionErr {
	parts := make([]RiskPartition, 0, len(m))
	for p := range m {
		parts = append(parts, p)
	}
	sort.Slice(parts, func(i, j int) bool { return partitionSortKey(parts[i]) < partitionSortKey(parts[j]) })
	out := make([]partitionErr, 0, len(parts))
	for _, p := range parts {
		out = append(out, partitionErr{part: p, err: m[p]})
	}
	return out
}

// partitionSortKey puts live first, then the default paper partition, then the
// named sources by id.
func partitionSortKey(p RiskPartition) string {
	switch {
	case p.IsLive():
		return "0"
	case p.Source == "":
		return "1"
	default:
		return "2" + p.Source
	}
}

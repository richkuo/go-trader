package main

import (
	"fmt"
	"sort"
)

type storageKey struct {
	Role storageRole
	ID   string
}

type storageIdentity struct {
	Role      storageRole
	Scope     PortfolioScope
	StorageID string
}

type storageIdentityMap struct {
	procToStore map[string]storageIdentity
	storeToProc map[storageKey]string
	layout      storageLayout
}

func buildStorageIdentityMap(cfg *Config, layout storageLayout) (storageIdentityMap, error) {
	m := storageIdentityMap{
		procToStore: make(map[string]storageIdentity),
		storeToProc: make(map[storageKey]string),
		layout:      layout,
	}
	if cfg == nil {
		return m, nil
	}
	for _, sc := range cfg.Strategies {
		if sc.ID == "" {
			continue
		}
		scope := portfolioScopeFor(sc)
		role := layout.roleForScope(scope)
		storageID := effectiveStorageStrategyID(sc)
		if storageID == "" {
			return storageIdentityMap{}, fmt.Errorf("strategy %q has an empty storage identity", sc.ID)
		}
		key := storageKey{Role: role, ID: storageID}
		if prev, ok := m.storeToProc[key]; ok {
			return storageIdentityMap{}, fmt.Errorf("strategies %q and %q both map to storage id %q in the %s state file", prev, sc.ID, storageID, role)
		}
		m.procToStore[sc.ID] = storageIdentity{Role: role, Scope: scope, StorageID: storageID}
		m.storeToProc[key] = sc.ID
	}
	return m, nil
}

func (m storageIdentityMap) storageFor(processID string) (storageIdentity, bool) {
	id, ok := m.procToStore[processID]
	return id, ok
}

func (m storageIdentityMap) processFor(role storageRole, storageID string) (string, bool) {
	id, ok := m.storeToProc[storageKey{Role: role, ID: storageID}]
	return id, ok
}

func (m storageIdentityMap) scopeFor(processID string) (PortfolioScope, bool) {
	id, ok := m.procToStore[processID]
	if !ok {
		return scopeUnassigned, false
	}
	return id.Scope, true
}

func (m storageIdentityMap) processIDsForRole(role storageRole) []string {
	out := make([]string, 0, len(m.procToStore))
	for procID, ident := range m.procToStore {
		if ident.Role == role {
			out = append(out, procID)
		}
	}
	sort.Strings(out)
	return out
}

func (m storageIdentityMap) processIDsForScope(scope PortfolioScope) []string {
	out := make([]string, 0, len(m.procToStore))
	for procID, ident := range m.procToStore {
		if ident.Scope == scope {
			out = append(out, procID)
		}
	}
	sort.Strings(out)
	return out
}

func (m storageIdentityMap) hasScope(scope PortfolioScope) bool {
	for _, ident := range m.procToStore {
		if ident.Scope == scope {
			return true
		}
	}
	return false
}

// fileIdentity is the per-file half of the identity map. It is handed to a
// StateDB so every strategy identifier is translated at the SQL boundary, in
// both directions, with no caller able to skip the translation.
type fileIdentity struct {
	role        storageRole
	procToStore map[string]string
	storeToProc map[string]string
}

func (m storageIdentityMap) fileIdentity(role storageRole) *fileIdentity {
	fi := &fileIdentity{
		role:        role,
		procToStore: make(map[string]string),
		storeToProc: make(map[string]string),
	}
	for procID, ident := range m.procToStore {
		if ident.Role != role {
			continue
		}
		fi.procToStore[procID] = ident.StorageID
		fi.storeToProc[ident.StorageID] = procID
	}
	return fi
}

// aliased reports whether any mapped strategy stores under a different
// identifier than its process identifier. A file with no alias needs no
// translation work at all.
func (f *fileIdentity) aliased() bool {
	if f == nil {
		return false
	}
	for procID, storageID := range f.procToStore {
		if procID != storageID {
			return true
		}
	}
	return false
}

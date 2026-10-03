package main

import (
	"fmt"
	"os"
	"sync"
)

// The storage identity map is attached to the physical file, so every
// strategy identifier is translated here, at the SQL boundary, in both
// directions. Callers above this layer only ever see process identifiers.

func (sdb *StateDB) storageRoleOf() storageRole {
	if sdb == nil || sdb.role == "" {
		return storageRolePrimary
	}
	return sdb.role
}

func (sdb *StateDB) ownsProcessID(procID string) bool {
	if sdb == nil || sdb.ident == nil {
		return true
	}
	_, ok := sdb.ident.procToStore[procID]
	return ok
}

func (sdb *StateDB) toStorageID(procID string) (string, error) {
	if sdb == nil || sdb.ident == nil {
		return procID, nil
	}
	if procID == "" {
		return "", nil
	}
	storageID, ok := sdb.ident.procToStore[procID]
	if !ok {
		return "", fmt.Errorf("strategy %q is not owned by the %s state file", procID, sdb.storageRoleOf())
	}
	return storageID, nil
}

func (sdb *StateDB) toStorageIDs(procIDs []string) ([]string, error) {
	if sdb == nil || sdb.ident == nil {
		return procIDs, nil
	}
	out := make([]string, 0, len(procIDs))
	for _, id := range procIDs {
		storageID, err := sdb.toStorageID(id)
		if err != nil {
			return nil, err
		}
		out = append(out, storageID)
	}
	return out, nil
}

// fromStorageID maps a stored identifier back to its process identifier. A row
// with no current strategy keeps its stored identifier so history is never
// silently re-attributed.
func (sdb *StateDB) fromStorageID(storageID string) string {
	if sdb == nil || sdb.ident == nil {
		return storageID
	}
	if procID, ok := sdb.ident.storeToProc[storageID]; ok {
		return procID
	}
	return storageID
}

var ownerStrategyTranslationWarned sync.Map

// fromStorageOwnerID translates a position owner. An owner that maps to no
// configured strategy in this file is stored verbatim, with one warning per
// identifier so an operator can see the unresolved reference.
func (sdb *StateDB) fromStorageOwnerID(storageID string) string {
	if sdb == nil || sdb.ident == nil || storageID == "" {
		return storageID
	}
	if procID, ok := sdb.ident.storeToProc[storageID]; ok {
		return procID
	}
	if !sdb.ident.aliased() {
		return storageID
	}
	key := string(sdb.storageRoleOf()) + "\x00" + storageID
	if _, loaded := ownerStrategyTranslationWarned.LoadOrStore(key, true); !loaded {
		fmt.Fprintf(os.Stderr, "[storage] WARN: owner_strategy_id %q in the %s state file maps to no configured strategy; kept verbatim\n",
			storageID, sdb.storageRoleOf())
	}
	return storageID
}

// toStorageOwnerID is the write-side counterpart: a mapped owner is translated,
// an unmapped one is stored verbatim rather than failing a book save.
func (sdb *StateDB) toStorageOwnerID(procID string) string {
	if sdb == nil || sdb.ident == nil || procID == "" {
		return procID
	}
	if storageID, ok := sdb.ident.procToStore[procID]; ok {
		return storageID
	}
	return procID
}

// resolvePlanStorageID checks that a backfill plan was built against this
// physical file, then returns the stored identifier the plan must update. A
// plan carrying no binding is a legacy in-process plan and resolves through the
// identity map.
func (sdb *StateDB) resolvePlanStorageID(role storageRole, storageID, procID string) (string, error) {
	if role != "" && role != sdb.storageRoleOf() {
		return "", fmt.Errorf("plan for %s was built against the %s state file", procID, role)
	}
	if storageID != "" {
		return storageID, nil
	}
	return sdb.toStorageID(procID)
}

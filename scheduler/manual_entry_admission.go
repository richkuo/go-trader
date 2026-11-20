package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

var manualEntryAdmissionBarrier = func(stage string) {}

type manualEntryAdmission struct {
	layout     storageLayout
	strategyID string
	part       RiskPartition
	pin        *manualRegimePin
	owned      *stateOwnership
}

type manualEntryRefusal struct {
	reason string
}

func (e *manualEntryRefusal) Error() string {
	return "entry admission refused: " + e.reason
}

func manualEntryRefused(format string, args ...any) error {
	return &manualEntryRefusal{reason: fmt.Sprintf(format, args...)}
}

func manualEntryRefusedFrom(err error) error {
	var te *manualRegimeTransportError
	if errors.As(err, &te) {
		return &manualEntryRefusal{reason: te.reason}
	}
	return &manualEntryRefusal{reason: err.Error()}
}

func admitManualEntry(cfg *Config, strategyID string) (*manualEntryAdmission, error) {
	layout, err := resolveStorageLayout(cfg)
	if err != nil {
		return nil, manualEntryRefused("storage layout: %v", err)
	}
	ident, err := buildStorageIdentityMap(cfg, layout)
	if err != nil {
		return nil, manualEntryRefused("storage identity: %v", err)
	}
	id, ok := ident.storageFor(strategyID)
	if !ok {
		return nil, manualEntryRefused("strategy %q has no storage identity", strategyID)
	}
	target, ok := layout.spec(id.Role)
	if !ok || target.InMemory {
		return nil, manualEntryRefused("the %s partition of strategy %q has no on-disk state file, so this command cannot verify who owns it", partitionLabel(id.Partition), strategyID)
	}
	var probes []stateDBLockProbe
	held, free := 0, 0
	for _, spec := range layout.Files {
		if spec.InMemory {
			continue
		}
		if err := os.MkdirAll(filepath.Dir(spec.Path), 0755); err != nil {
			return nil, manualEntryRefused("create the %s state directory: %v", spec.Role, err)
		}
		probe, err := probeStateDBLockStrict(spec.Path)
		if err != nil {
			return nil, manualEntryRefused("%v", err)
		}
		probes = append(probes, probe)
		if probe.held {
			held++
		} else {
			free++
		}
	}
	a := &manualEntryAdmission{layout: layout, strategyID: strategyID, part: id.Partition}
	switch {
	case held == 0 && free == 0:
		return nil, manualEntryRefused("the configured storage layout has no on-disk state file")
	case held > 0 && free > 0:
		return nil, manualEntryRefused("only part of the configured state-file set is owned by a running process")
	case held > 0:
		pin, terr := verifyManualRegimeOwnerPin(layout)
		if terr != nil {
			return nil, manualEntryRefusedFrom(terr)
		}
		a.pin = &pin
	default:
		owned, err := acquireStateOwnershipAs(layout.Files, stateDBLockKindManualEntry)
		if err != nil {
			return nil, manualEntryRefused("could not take the state files for a standalone entry: %v", err)
		}
		if len(owned.locks) != len(probes) {
			owned.Release()
			return nil, manualEntryRefused("the standalone ownership set differs from the probed state files")
		}
		for i, lock := range owned.locks {
			if lock.path != probes[i].lockPath {
				owned.Release()
				return nil, manualEntryRefused("ownership lock %s resolved to %s between the probe and the lock", probes[i].lockPath, lock.path)
			}
			if probes[i].info != nil {
				info, err := lock.f.Stat()
				if err != nil || !os.SameFile(info, probes[i].info) {
					owned.Release()
					return nil, manualEntryRefused("ownership lock %s was replaced between the probe and the lock", lock.path)
				}
			}
		}
		a.owned = owned
	}
	if err := a.check("admission"); err != nil {
		a.Release()
		return nil, err
	}
	return a, nil
}

func (a *manualEntryAdmission) standalone() bool {
	return a != nil && a.owned != nil
}

func (a *manualEntryAdmission) check(stage string) error {
	if a == nil {
		return nil
	}
	manualEntryAdmissionBarrier(stage)
	if a.owned != nil {
		for _, lock := range a.owned.locks {
			if lock.f == nil {
				return manualEntryRefused("the standalone ownership lock %s was released", lock.path)
			}
			held, herr := lock.f.Stat()
			current, lerr := os.Lstat(lock.path)
			if herr != nil || lerr != nil || !os.SameFile(held, current) {
				return manualEntryRefused("the standalone ownership lock %s was replaced while this command held it", lock.path)
			}
		}
		return nil
	}
	if a.pin == nil {
		return manualEntryRefused("no verified owner")
	}
	if terr := a.pin.recheck(stage); terr != nil {
		return manualEntryRefusedFrom(terr)
	}
	if err := checkManualRegimeHoldGuardWitness(*a.pin, a.part); err != nil {
		return manualEntryRefusedFrom(err)
	}
	return nil
}

func (a *manualEntryAdmission) Release() {
	if a == nil {
		return
	}
	a.owned.Release()
	a.owned = nil
}

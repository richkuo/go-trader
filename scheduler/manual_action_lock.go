package main

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"syscall"
	"time"
)

// manualActionLockSuffix names the advisory lock file that serializes operator
// trade actions (manual open/add/close, force-close, SL edits) across every OS
// process and caller sharing the state DB. It sits next to <DBFile> — distinct
// from the daemon's singleton ".lock" (singleton_lock.go), which the daemon
// holds for its ENTIRE lifetime; reusing that file would make a CLI process
// unable to ever take it while the daemon runs. This lock is instead taken
// briefly, only around a single manual action's critical section.
const manualActionLockSuffix = ".manual-action.lock"

// manualActionLockMaxWait bounds how long a caller blocks for the lock before
// failing closed. It comfortably absorbs a normal on-chain submit (so a second
// action arriving mid-submit waits, then hits the precise pending-row guard
// rather than a generic "in progress") without hanging a dashboard request for
// the full subprocess timeout. On timeout we refuse (fail closed): the operator
// retries, and by then the first action's pending row exists, so the retry gets
// the precise guard message — never a double-fire.
const manualActionLockMaxWait = 8 * time.Second

// manualActionLockPollInterval is the retry cadence for the bounded acquire.
// LOCK_NB is retried in a loop rather than blocking indefinitely so the wait is
// bounded by manualActionLockMaxWait.
const manualActionLockPollInterval = 25 * time.Millisecond

// acquireManualActionFileLock takes a cross-process exclusive advisory lock so a
// manual-action core's guard-check → on-chain submit → pending-row insert is
// atomic against every other process AND caller sharing the state DB — a CLI
// `manual-close` racing a dashboard Close click, or two concurrent CLI
// invocations. The in-process tradeActionMu (ui_trade_actions.go) only
// serializes HTTP requests to the ONE running daemon; SetMaxOpenConns(1) only
// serializes a single process's own SQLite pool; neither extends to a separate
// CLI process, so without this both callers can observe "no pending action",
// both fire, and double/flip the position (#1260 review).
//
// It is a kernel flock() on an fd (modeled on singleton_lock.go, same package):
// the OS releases it when the process dies — crash and SIGKILL included — so a
// killed holder never strands the lock. That crash-safety is the decisive
// advantage over a DB-row "reservation", which a killed process would leave
// stuck until manually cleared, permanently blocking the strategy+symbol. It is
// also why the reviewer's suggested BEGIN IMMEDIATE transaction is unusable
// here: the on-chain submit sits BETWEEN the guard read and the insert, so a
// transaction spanning them would hold a SQLite write lock across a subprocess
// network round-trip; and a unique index on the final insert fires only AFTER
// both orders already hit the chain, orphaning the second fill.
//
// The returned release closure closes the fd (releasing the lock); callers
// defer it. An in-memory DB is per-process by construction — no cross-process
// race, and no writable path for a lock file — so it returns a no-op. Any
// failure to acquire (open error, unexpected errno, or the bounded wait
// elapsing) fails CLOSED so the caller refuses the action rather than risk a
// double-fire, matching the existing pending-row guards.
func acquireManualActionFileLock(dbPath string) (release func(), err error) {
	if isInMemoryDBPath(dbPath) {
		return func() {}, nil
	}
	lockPath := canonicalDBPath(dbPath) + manualActionLockSuffix
	f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0644)
	if err != nil {
		return nil, fmt.Errorf("open manual-action lock %s: %w", lockPath, err)
	}
	deadline := time.Now().Add(manualActionLockMaxWait)
	for {
		flockErr := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if flockErr == nil {
			return func() { f.Close() }, nil
		}
		// LOCK_NB returns EWOULDBLOCK (== EAGAIN on Linux) while another holder
		// has it. Any other errno is a genuine failure — fail closed.
		if !errors.Is(flockErr, syscall.EWOULDBLOCK) {
			f.Close()
			return nil, fmt.Errorf("flock manual-action lock %s: %w", lockPath, flockErr)
		}
		if time.Now().After(deadline) {
			f.Close()
			return nil, fmt.Errorf("another manual action is in progress (lock held on %s); retry in a moment", lockPath)
		}
		time.Sleep(manualActionLockPollInterval)
	}
}

// isInMemoryDBPath reports whether dbPath names a SQLite in-memory database.
// Such a DB is private to the opening process, so it needs no cross-process
// lock (and has no on-disk directory in which to place a lock file).
func isInMemoryDBPath(dbPath string) bool {
	p := strings.TrimSpace(dbPath)
	return p == "" || p == ":memory:" || strings.Contains(p, ":memory:") || strings.Contains(p, "mode=memory")
}

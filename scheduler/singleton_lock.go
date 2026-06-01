package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

// stateDBLock holds an exclusive advisory lock on <DBFile>.lock for the
// lifetime of the daemon process (#849). It exists to stop a second go-trader
// from silently running against the same state DB / exchange account, which
// would book duplicate trades and desync on-chain positions.
//
// The lock is a kernel flock() on an open file descriptor — never a
// "write PID to a file, refuse if the file exists" scheme. The distinction is
// load-bearing for deploy safety: the OS auto-releases an fd lock when the
// process dies (including SIGKILL/crash), so a hard-killed daemon leaves no
// stale lock and the next start always succeeds. A pidfile-content check would
// survive a crash and permanently block restart, causing update.sh to roll
// back every deploy. update.sh's `systemctl restart` is sequential (old daemon
// fully drains and exits → lock released → new daemon starts), so there is no
// steady-state overlap window and no false "already running" failure.
type stateDBLock struct {
	f    *os.File
	path string
}

// heldStateDBLock keeps the daemon's singleton lock reachable for the whole
// process lifetime so the underlying *os.File isn't garbage-collected and
// finalized (which would close the fd and silently release the flock) while
// the daemon is still trading. main() assigns it on the daemon path; it is
// never read back — the OS releases the lock on process exit.
var heldStateDBLock *stateDBLock

// stateDBLockedError is returned by acquireStateDBLock when the lock is already
// held by another live process. PID is the value the holder recorded in the
// lock file (best-effort, for the operator message only — the lock decision is
// always the kernel flock, never this file content); it is 0 when unreadable.
type stateDBLockedError struct {
	path string
	pid  int
}

func (e *stateDBLockedError) Error() string {
	if e.pid > 0 {
		return fmt.Sprintf("another go-trader is already running (pid %d); lock held on %s", e.pid, e.path)
	}
	return fmt.Sprintf("another go-trader is already running; lock held on %s", e.path)
}

// stateDBLockPath returns the lock-file path for a given state-DB path. The
// lock sits next to the DB so per-DB-file isolation is automatic: genuinely
// separate instances (sub-accounts / distinct DBFile) resolve to distinct lock
// files and never contend. The DB path is first canonicalized so two launches
// that name the same DB through different strings (relative vs absolute, or via
// a symlink) collapse to one lock file and actually contend.
func stateDBLockPath(dbPath string) string {
	return canonicalDBPath(dbPath) + ".lock"
}

// canonicalDBPath resolves dbPath to an absolute, symlink-free form so distinct
// strings naming the same file map to the same lock path. Resolution is
// best-effort and degrades to the most-resolved form available: if the DB file
// (or a path component) doesn't exist yet, EvalSymlinks fails and we keep the
// Abs form; if even Abs fails we keep the raw string. The guard must never
// refuse to start merely because the path couldn't be canonicalized — the
// fallback just reverts to the pre-hardening raw-string behavior.
func canonicalDBPath(dbPath string) string {
	resolved := dbPath
	if abs, err := filepath.Abs(resolved); err == nil {
		resolved = abs
	}
	if eval, err := filepath.EvalSymlinks(resolved); err == nil {
		resolved = eval
	}
	return resolved
}

// acquireStateDBLock takes an exclusive, non-blocking flock on <dbPath>.lock.
// On success it returns a *stateDBLock that must be kept alive for the process
// lifetime (closing the fd, or process exit, releases the lock). When the lock
// is already held it returns a *stateDBLockedError; other failures (cannot
// create the lock file, unexpected flock errno) return a wrapped error.
func acquireStateDBLock(dbPath string) (*stateDBLock, error) {
	lockPath := stateDBLockPath(dbPath)
	f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0644)
	if err != nil {
		return nil, fmt.Errorf("open lock file %s: %w", lockPath, err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		// LOCK_NB returns EWOULDBLOCK (== EAGAIN on Linux) when another process
		// holds the lock. Read the holder's recorded PID for the message, then
		// release our fd so we don't leak it on the exit path.
		pid := readLockPID(f)
		f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, &stateDBLockedError{path: lockPath, pid: pid}
		}
		return nil, fmt.Errorf("flock %s: %w", lockPath, err)
	}
	// We hold the lock. Record our PID for human inspection / external
	// monitoring; failure here is non-fatal because the kernel flock — not the
	// file content — is what enforces exclusivity.
	if err := writeLockPID(f, os.Getpid()); err != nil {
		fmt.Fprintf(os.Stderr, "[singleton] WARN: could not write pid to %s: %v\n", lockPath, err)
	}
	return &stateDBLock{f: f, path: lockPath}, nil
}

// Release drops the lock by closing the fd. Safe to call on a nil receiver.
// The OS also releases the lock on process exit, so this is only needed for the
// graceful-return path where the process keeps running afterwards (tests).
func (l *stateDBLock) Release() {
	if l == nil || l.f == nil {
		return
	}
	l.f.Close()
	l.f = nil
}

// writeLockPID overwrites the lock file with the given PID (decimal, one line)
// and fsyncs so a contending process can read it back immediately.
func writeLockPID(f *os.File, pid int) error {
	if err := f.Truncate(0); err != nil {
		return err
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(f, "%d\n", pid); err != nil {
		return err
	}
	return f.Sync()
}

// readLockPID best-effort reads the PID the lock holder recorded. Returns 0 on
// any error (empty file, malformed content, read failure) — callers treat 0 as
// "unknown" and degrade the message gracefully.
func readLockPID(f *os.File) int {
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return 0
	}
	buf := make([]byte, 32)
	n, err := f.Read(buf)
	if (err != nil && err != io.EOF) || n <= 0 {
		return 0
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(buf[:n])))
	if err != nil {
		return 0
	}
	return pid
}

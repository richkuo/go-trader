package main

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

func TestAcquireStateDBLock_Basic(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "state.db")

	lock, err := acquireStateDBLock(dbPath)
	if err != nil {
		t.Fatalf("first acquire failed: %v", err)
	}
	if lock == nil || lock.f == nil {
		t.Fatal("expected a held lock")
	}

	// The lock file sits next to the DB and records our PID.
	wantPath := stateDBLockPath(dbPath)
	if lock.path != wantPath {
		t.Errorf("lock.path = %q, want %q", lock.path, wantPath)
	}
	if got := readLockFileContent(t, wantPath); got != os.Getpid() {
		t.Errorf("recorded pid = %d, want %d", got, os.Getpid())
	}

	lock.Release()
	// Release nils the fd; a second Release must be a safe no-op.
	lock.Release()
}

func TestAcquireStateDBLock_Contended(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "state.db")

	first, err := acquireStateDBLock(dbPath)
	if err != nil {
		t.Fatalf("first acquire failed: %v", err)
	}
	defer first.Release()

	// A second acquire against the same path must fail (not silently succeed),
	// and the error must carry the holder's recorded PID for the operator
	// message.
	second, err := acquireStateDBLock(dbPath)
	if err == nil {
		second.Release()
		t.Fatal("second acquire unexpectedly succeeded — singleton guard is not exclusive")
	}
	var locked *stateDBLockedError
	if !errors.As(err, &locked) {
		t.Fatalf("error = %v (%T), want *stateDBLockedError", err, err)
	}
	if locked.pid != os.Getpid() {
		t.Errorf("locked.pid = %d, want %d", locked.pid, os.Getpid())
	}
}

func TestAcquireStateDBLock_ReleaseAllowsReacquire(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "state.db")

	first, err := acquireStateDBLock(dbPath)
	if err != nil {
		t.Fatalf("first acquire failed: %v", err)
	}
	first.Release()

	// After Release the lock is free, so the same path acquires cleanly again.
	// This mirrors the normal `systemctl restart` sequence (old daemon exits →
	// lock released → new daemon starts).
	second, err := acquireStateDBLock(dbPath)
	if err != nil {
		t.Fatalf("reacquire after release failed: %v", err)
	}
	second.Release()
}

func TestAcquireStateDBLock_DistinctPaths(t *testing.T) {
	dir := t.TempDir()
	pathA := filepath.Join(dir, "a.db")
	pathB := filepath.Join(dir, "b.db")

	// Genuinely separate instances (distinct DBFile, e.g. sub-accounts) resolve
	// to distinct lock files and must both hold their lock at once.
	a, err := acquireStateDBLock(pathA)
	if err != nil {
		t.Fatalf("acquire A failed: %v", err)
	}
	defer a.Release()
	b, err := acquireStateDBLock(pathB)
	if err != nil {
		t.Fatalf("acquire B failed (distinct paths must not contend): %v", err)
	}
	defer b.Release()
}

func TestAcquireStateDBLock_SymlinkedPathContends(t *testing.T) {
	dir := t.TempDir()
	realDB := filepath.Join(dir, "state.db")
	// The DB file must exist for EvalSymlinks to resolve the path (in production
	// the state DB is opened before the lock is taken).
	if err := os.WriteFile(realDB, nil, 0o644); err != nil {
		t.Fatalf("create db file: %v", err)
	}
	// A file-level symlink under a different name. The ".lock" suffix is appended
	// to the symlink's *own* path, so without canonicalization the alias resolves
	// to a different lock file (state-alias.db.lock vs state.db.lock) and a
	// duplicate could start trading. EvalSymlinks collapses both to one lock.
	linkedDB := filepath.Join(dir, "state-alias.db")
	if err := os.Symlink(realDB, linkedDB); err != nil {
		t.Skipf("symlinks unavailable on this platform: %v", err)
	}

	first, err := acquireStateDBLock(realDB)
	if err != nil {
		t.Fatalf("acquire via real path failed: %v", err)
	}
	defer first.Release()

	// The same DB reached through the symlink must contend, not get its own lock.
	second, err := acquireStateDBLock(linkedDB)
	if err == nil {
		second.Release()
		t.Fatal("acquire via symlinked path succeeded — canonicalization is not collapsing symlinks")
	}
	var locked *stateDBLockedError
	if !errors.As(err, &locked) {
		t.Fatalf("error = %v (%T), want *stateDBLockedError", err, err)
	}
}

func TestAcquireStateDBLock_StaleFdReleasesOnClose(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "state.db")
	lockPath := stateDBLockPath(dbPath)

	// Simulate a crashed holder: open the lock file, flock it, then close the
	// fd WITHOUT calling Release — closing the fd is exactly what the kernel
	// does when a process dies (incl. SIGKILL), so no stale lock should remain.
	f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0644)
	if err != nil {
		t.Fatalf("open lock file: %v", err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatalf("seed flock: %v", err)
	}
	f.Close() // kernel releases the flock here

	lock, err := acquireStateDBLock(dbPath)
	if err != nil {
		t.Fatalf("acquire after simulated crash failed — stale lock blocked restart: %v", err)
	}
	lock.Release()
}

// readLockFileContent reads the lock file directly (a separate fd, not holding
// the flock) and parses the recorded PID.
func readLockFileContent(t *testing.T, path string) int {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open lock file for read: %v", err)
	}
	defer f.Close()
	return readLockPID(f)
}

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

	wantPath := stateDBLockPath(dbPath)
	if lock.path != wantPath {
		t.Errorf("lock.path = %q, want %q", lock.path, wantPath)
	}
	if got := readLockFileContent(t, wantPath); got != os.Getpid() {
		t.Errorf("recorded pid = %d, want %d", got, os.Getpid())
	}

	lock.Release()
	lock.Release()
}

func TestAcquireStateDBLock_Contended(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "state.db")

	first, err := acquireStateDBLock(dbPath)
	if err != nil {
		t.Fatalf("first acquire failed: %v", err)
	}
	defer first.Release()

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
	if err := os.WriteFile(realDB, nil, 0o644); err != nil {
		t.Fatalf("create db file: %v", err)
	}
	linkedDB := filepath.Join(dir, "state-alias.db")
	if err := os.Symlink(realDB, linkedDB); err != nil {
		t.Skipf("symlinks unavailable on this platform: %v", err)
	}

	first, err := acquireStateDBLock(realDB)
	if err != nil {
		t.Fatalf("acquire via real path failed: %v", err)
	}
	defer first.Release()

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

	f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0644)
	if err != nil {
		t.Fatalf("open lock file: %v", err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatalf("seed flock: %v", err)
	}
	f.Close()

	lock, err := acquireStateDBLock(dbPath)
	if err != nil {
		t.Fatalf("acquire after simulated crash failed — stale lock blocked restart: %v", err)
	}
	lock.Release()
}

func readLockFileContent(t *testing.T, path string) int {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open lock file for read: %v", err)
	}
	defer f.Close()
	return readLockPID(f)
}

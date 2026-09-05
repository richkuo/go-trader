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

type stateDBLock struct {
	f    *os.File
	path string
}

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

func stateDBLockPath(dbPath string) string {
	return canonicalDBPath(dbPath) + ".lock"
}

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

func acquireStateDBLock(dbPath string) (*stateDBLock, error) {
	lockPath := stateDBLockPath(dbPath)
	f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0644)
	if err != nil {
		return nil, fmt.Errorf("open lock file %s: %w", lockPath, err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		pid := readLockPID(f)
		f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, &stateDBLockedError{path: lockPath, pid: pid}
		}
		return nil, fmt.Errorf("flock %s: %w", lockPath, err)
	}
	if err := writeLockPID(f, os.Getpid()); err != nil {
		fmt.Fprintf(os.Stderr, "[singleton] WARN: could not write pid to %s: %v\n", lockPath, err)
	}
	return &stateDBLock{f: f, path: lockPath}, nil
}

func (l *stateDBLock) Release() {
	if l == nil || l.f == nil {
		return
	}
	l.f.Close()
	l.f = nil
}

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

// stateOwnership is the process-wide claim on every state file. Both files are
// locked in role order before any migration, startup write or trading, and any
// failure releases everything already held.
type stateOwnership struct {
	locks []*stateDBLock
}

func (o *stateOwnership) Release() {
	if o == nil {
		return
	}
	for i := len(o.locks) - 1; i >= 0; i-- {
		o.locks[i].Release()
	}
	o.locks = nil
}

func acquireStateOwnership(specs []storageFileSpec) (*stateOwnership, error) {
	owned := &stateOwnership{}
	for _, role := range storageRoleOrder {
		for _, spec := range specs {
			if spec.Role != role {
				continue
			}
			if spec.InMemory {
				continue
			}
			lock, err := acquireStateDBLock(spec.Path)
			if err != nil {
				owned.Release()
				return nil, err
			}
			owned.locks = append(owned.locks, lock)
		}
	}
	return owned, nil
}

// schedulerNeedsOwnership reports whether this scheduler invocation must own
// the state files. A one-shot cycle writes books exactly like the daemon, so it
// takes ownership too.
func schedulerNeedsOwnership(once bool) bool {
	return true
}

// probeStateDBLockHolder reports the pid holding a file's ownership lock
// without taking it. It never creates the lock file.
func probeStateDBLockHolder(dbPath string) (int, bool) {
	lockPath := stateDBLockPath(dbPath)
	f, err := os.OpenFile(lockPath, os.O_RDWR, 0644)
	if err != nil {
		return 0, false
	}
	defer f.Close()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return readLockPID(f), true
		}
		return 0, false
	}
	syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	return 0, false
}

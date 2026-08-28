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

var heldStateDBLock *stateDBLock

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

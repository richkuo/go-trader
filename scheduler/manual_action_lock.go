package main

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"syscall"
	"time"
)

const manualActionLockSuffix = ".manual-action.lock"

const manualActionLockMaxWait = 8 * time.Second

const manualActionLockPollInterval = 25 * time.Millisecond

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

func isInMemoryDBPath(dbPath string) bool {
	p := strings.TrimSpace(dbPath)
	return p == "" || p == ":memory:" || strings.Contains(p, ":memory:") || strings.Contains(p, "mode=memory")
}

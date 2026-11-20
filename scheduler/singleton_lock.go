package main

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

type stateDBLock struct {
	f    *os.File
	path string
}

const stateDBLockKindManualEntry = "manual-entry"

var (
	schedulerManualEntryWait    = 45 * time.Second
	schedulerManualEntryPollGap = 250 * time.Millisecond
)

type stateDBLockedError struct {
	path   string
	pid    int
	kind   string
	waited time.Duration
}

func (e *stateDBLockedError) Error() string {
	if e.kind == stateDBLockKindManualEntry {
		holder := "a standalone manual entry command"
		if e.pid > 0 {
			holder = fmt.Sprintf("a standalone manual entry command (pid %d)", e.pid)
		}
		if e.waited > 0 {
			return fmt.Sprintf("%s still holds the lock on %s after a %s wait", holder, e.path, e.waited)
		}
		return fmt.Sprintf("%s holds the lock on %s", holder, e.path)
	}
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

const (
	stateDBLockAttempts = 10
	stateDBLockRetryGap = 10 * time.Millisecond
)

// acquireStateDBLock takes the exclusive ownership lock. A contended lock is
// retried briefly so a read-only probe (probeStateDBLockHolder) that holds
// the lock for microseconds never turns a daemon start into an exit 79.
func acquireStateDBLock(dbPath string) (*stateDBLock, error) {
	return acquireStateDBLockAs(dbPath, "")
}

func acquireStateDBLockAs(dbPath, kind string) (*stateDBLock, error) {
	lockPath := stateDBLockPath(dbPath)
	f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0644)
	if err != nil {
		return nil, fmt.Errorf("open lock file %s: %w", lockPath, err)
	}
	if err := flockWithRetry(f); err != nil {
		pid, holderKind := readLockRecord(f)
		f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, &stateDBLockedError{path: lockPath, pid: pid, kind: holderKind}
		}
		return nil, fmt.Errorf("flock %s: %w", lockPath, err)
	}
	if err := writeLockRecord(f, os.Getpid(), kind); err != nil {
		fmt.Fprintf(os.Stderr, "[singleton] WARN: could not write pid to %s: %v\n", lockPath, err)
	}
	return &stateDBLock{f: f, path: lockPath}, nil
}

func flockWithRetry(f *os.File) error {
	var err error
	for attempt := 0; attempt < stateDBLockAttempts; attempt++ {
		if attempt > 0 {
			time.Sleep(stateDBLockRetryGap)
		}
		err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if !errors.Is(err, syscall.EWOULDBLOCK) {
			return err
		}
	}
	return err
}

func (l *stateDBLock) Release() {
	if l == nil || l.f == nil {
		return
	}
	l.f.Close()
	l.f = nil
}

func writeLockRecord(f *os.File, pid int, kind string) error {
	if err := f.Truncate(0); err != nil {
		return err
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return err
	}
	record := strconv.Itoa(pid)
	if kind != "" {
		record += " " + kind
	}
	if _, err := fmt.Fprintf(f, "%s\n", record); err != nil {
		return err
	}
	return f.Sync()
}

func readLockPID(f *os.File) int {
	pid, _ := readLockRecord(f)
	return pid
}

func readLockRecord(f *os.File) (int, string) {
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return 0, ""
	}
	buf := make([]byte, 64)
	n, err := f.Read(buf)
	if (err != nil && err != io.EOF) || n <= 0 {
		return 0, ""
	}
	fields := strings.Fields(string(buf[:n]))
	if len(fields) == 0 || len(fields) > 2 {
		return 0, ""
	}
	pid, err := strconv.Atoi(fields[0])
	if err != nil || pid <= 0 {
		return 0, ""
	}
	if len(fields) == 2 {
		return pid, fields[1]
	}
	return pid, ""
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
	return acquireStateOwnershipAs(specs, "")
}

func acquireSchedulerStateOwnership(specs []storageFileSpec) (*stateOwnership, error) {
	wait := schedulerManualEntryWait
	deadline := time.Now().Add(wait)
	announced, confirmed := false, false
	for {
		owned, err := acquireStateOwnership(specs)
		if err == nil {
			return owned, nil
		}
		var locked *stateDBLockedError
		if !errors.As(err, &locked) {
			return nil, err
		}
		if locked.kind != stateDBLockKindManualEntry {
			if confirmed {
				return nil, err
			}
			confirmed = true
			time.Sleep(schedulerManualEntryPollGap)
			continue
		}
		confirmed = false
		if !time.Now().Before(deadline) {
			locked.waited = wait
			return nil, err
		}
		if !announced {
			announced = true
			fmt.Printf("[singleton] %s; waiting up to %s for it to finish before this scheduler starts\n", locked.Error(), wait)
		}
		time.Sleep(schedulerManualEntryPollGap)
	}
}

func acquireStateOwnershipAs(specs []storageFileSpec, kind string) (*stateOwnership, error) {
	// The layout's file order (primary, paper, then sources by id) is the one
	// stable lock order every process and every tool takes, so two owners can
	// never deadlock against each other half way through a set of files.
	owned := &stateOwnership{}
	for _, spec := range specs {
		if spec.InMemory {
			continue
		}
		lock, err := acquireStateDBLockAs(spec.Path, kind)
		if err != nil {
			owned.Release()
			return nil, err
		}
		owned.locks = append(owned.locks, lock)
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

type stateDBLockProbe struct {
	lockPath string
	held     bool
	pid      int
	info     os.FileInfo
}

func probeStateDBLockStrict(dbPath string) (stateDBLockProbe, error) {
	lockPath := stateDBLockPath(dbPath)
	out := stateDBLockProbe{lockPath: lockPath}
	before, err := os.Lstat(lockPath)
	if errors.Is(err, fs.ErrNotExist) {
		return out, nil
	}
	if err != nil {
		return out, fmt.Errorf("ownership lock %s unreadable: %w", lockPath, err)
	}
	if !before.Mode().IsRegular() {
		return out, fmt.Errorf("ownership lock %s is not a regular file (%s)", lockPath, before.Mode().Type())
	}
	f, err := os.OpenFile(lockPath, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return out, fmt.Errorf("ownership lock %s unreadable: %w", lockPath, err)
	}
	defer f.Close()
	opened, err := f.Stat()
	if err != nil || !opened.Mode().IsRegular() || !os.SameFile(before, opened) {
		return out, fmt.Errorf("ownership lock %s was replaced during the probe", lockPath)
	}
	switch flockErr := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); {
	case flockErr == nil:
		syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	case errors.Is(flockErr, syscall.EWOULDBLOCK):
		out.held = true
		out.pid = readLockPID(f)
	default:
		return out, fmt.Errorf("ownership lock %s unreadable: %w", lockPath, flockErr)
	}
	after, err := os.Lstat(lockPath)
	if err != nil || !os.SameFile(before, after) {
		return out, fmt.Errorf("ownership lock %s was replaced during the probe", lockPath)
	}
	out.info = opened
	return out, nil
}

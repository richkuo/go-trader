//go:build pyintegration

package main

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	pairPaperTestClockEnv  = "GO_TRADER_PAPER_TEST_CLOCK"
	pairPaperTestClockTick = 20 * time.Millisecond
)

type pairPaperTestWall struct {
	mu   sync.Mutex
	path string
	ms   int64
}

func init() {
	path := os.Getenv(pairPaperTestClockEnv)
	if path == "" {
		return
	}
	blob, err := os.ReadFile(path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "the paper test clock %s is unreadable: %v\n", path, err)
		os.Exit(basketReplayExitError)
	}
	ms, err := strconv.ParseInt(strings.TrimSpace(string(blob)), 10, 64)
	if err != nil || ms <= 0 {
		fmt.Fprintf(os.Stderr, "the paper test clock %s holds no positive millisecond time: %q\n", path, blob)
		os.Exit(basketReplayExitError)
	}
	pairPaperWall = &pairPaperTestWall{path: path, ms: ms}
}

func (w *pairPaperTestWall) reloadLocked() {
	blob, err := os.ReadFile(w.path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "the paper test clock could not reload: %v\n", err)
		os.Exit(basketReplayExitError)
	}
	ms, err := strconv.ParseInt(strings.TrimSpace(string(blob)), 10, 64)
	if err != nil || ms <= 0 {
		fmt.Fprintf(os.Stderr, "the paper test clock holds no positive millisecond time: %q\n", blob)
		os.Exit(basketReplayExitError)
	}
	w.ms = ms
}

func (w *pairPaperTestWall) Now() time.Time {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.reloadLocked()
	return time.UnixMilli(w.ms)
}

func (w *pairPaperTestWall) advance(d time.Duration) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.reloadLocked()
	step := int64((d + time.Millisecond - 1) / time.Millisecond)
	if step < 1 {
		step = 1
	}
	w.ms += step
	tmp := w.path + ".next"
	if err := os.WriteFile(tmp, []byte(strconv.FormatInt(w.ms, 10)), 0o600); err != nil {
		fmt.Fprintf(os.Stderr, "the paper test clock could not advance: %v\n", err)
		os.Exit(basketReplayExitError)
	}
	if err := os.Rename(tmp, w.path); err != nil {
		fmt.Fprintf(os.Stderr, "the paper test clock could not advance: %v\n", err)
		os.Exit(basketReplayExitError)
	}
}

func (w *pairPaperTestWall) Sleep(d time.Duration) { w.advance(d) }

func (w *pairPaperTestWall) Tick() { w.advance(pairPaperTestClockTick) }

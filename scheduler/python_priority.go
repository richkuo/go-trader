package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"

	"golang.org/x/sys/unix"
)

type pythonChildPriority int

const (
	pythonPriorityDefault pythonChildPriority = iota
	pythonPriorityPaper
)

const (
	paperChildNiceEnv              = "GO_TRADER_PAPER_NICE"
	defaultPaperChildNiceIncrement = 10
	maxNiceValue                   = 19
)

type pythonChildPriorityKey struct{}

var paperPriorityProcessEligible atomic.Bool

var paperChildNiceIncrement = sync.OnceValue(resolvePaperChildNiceIncrement)

var pythonChildPriorityWarned sync.Map

func parsePaperChildNiceIncrement(raw string) (int, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return defaultPaperChildNiceIncrement, true
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 0 || n > maxNiceValue {
		return defaultPaperChildNiceIncrement, false
	}
	return n, true
}

func resolvePaperChildNiceIncrement() int {
	raw := os.Getenv("GO_TRADER_PAPER_NICE")
	n, ok := parsePaperChildNiceIncrement(raw)
	if !ok {
		fmt.Printf("[WARN] %s=%q is not an integer from 0 to %d; paper check children use the default increment %d\n", paperChildNiceEnv, raw, maxNiceValue, defaultPaperChildNiceIncrement)
	}
	return n
}

func configurePaperChildPriority(strategies []StrategyConfig, paperOnly bool) string {
	eligible := !HasLiveStrategy(strategies) && (paperOnly || len(strategies) > 0)
	paperPriorityProcessEligible.Store(eligible)
	increment := paperChildNiceIncrement()
	switch {
	case !eligible && len(strategies) == 0:
		return "[priority] paper check children run at the default priority: the roster is empty"
	case !eligible:
		return "[priority] paper check children run at the default priority: this process has a live strategy"
	case increment == 0:
		return fmt.Sprintf("[priority] paper check children run at the default priority: %s=0 turns the feature off", paperChildNiceEnv)
	}
	return fmt.Sprintf("[priority] paper-only process: paper check children run at the scheduler nice +%d (at most %d)", increment, maxNiceValue)
}

func checkChildPriority(sc StrategyConfig) pythonChildPriority {
	if !paperPriorityProcessEligible.Load() || portfolioScopeFor(sc) != ScopePaper {
		return pythonPriorityDefault
	}
	return pythonPriorityPaper
}

func hlBatchGroupPriority(members []StrategyConfig) pythonChildPriority {
	if !paperPriorityProcessEligible.Load() || len(members) == 0 {
		return pythonPriorityDefault
	}
	for _, sc := range members {
		if portfolioScopeFor(sc) != ScopePaper {
			return pythonPriorityDefault
		}
	}
	return pythonPriorityPaper
}

func withPythonChildPriority(ctx context.Context, prio pythonChildPriority) context.Context {
	if prio != pythonPriorityPaper {
		return ctx
	}
	return context.WithValue(ctx, pythonChildPriorityKey{}, prio)
}

func applyPythonChildPriority(ctx context.Context, pid int) {
	prio, _ := ctx.Value(pythonChildPriorityKey{}).(pythonChildPriority)
	if prio != pythonPriorityPaper || !paperPriorityProcessEligible.Load() {
		return
	}
	increment := paperChildNiceIncrement()
	if increment <= 0 {
		return
	}
	base, err := schedulerBaseNice()
	if err != nil {
		warnPythonChildPriority("read the scheduler nice value", err)
		return
	}
	target := min(maxNiceValue, base+increment)
	if target <= base {
		return
	}
	if err := unix.Setpriority(unix.PRIO_PGRP, pid, target); err != nil {
		if errors.Is(err, unix.ESRCH) {
			return
		}
		warnPythonChildPriority(fmt.Sprintf("set nice %d on a check child process group", target), err)
	}
}

func warnPythonChildPriority(action string, err error) {
	key := action + ": " + err.Error()
	if _, seen := pythonChildPriorityWarned.LoadOrStore(key, struct{}{}); seen {
		return
	}
	fmt.Printf("[WARN] paper check child priority: %s; the check continues at the default priority\n", key)
}

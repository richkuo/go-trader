package main

import (
	"fmt"
	"sync"
	"time"
)

var (
	tradingClockMu    sync.RWMutex
	tradingClockFn    func() time.Time
	tradingClockOwner string
)

func tradingNow() time.Time {
	tradingClockMu.RLock()
	fn := tradingClockFn
	tradingClockMu.RUnlock()
	if fn != nil {
		return fn()
	}
	return time.Now()
}

func bindTradingClock(owner string, fn func() time.Time) error {
	if fn == nil || owner == "" {
		return fmt.Errorf("a trading clock needs an owner and a clock function")
	}
	tradingClockMu.Lock()
	defer tradingClockMu.Unlock()
	if tradingClockFn != nil {
		return fmt.Errorf("the trading clock is already bound to %s", tradingClockOwner)
	}
	tradingClockFn, tradingClockOwner = fn, owner
	return nil
}

func unbindTradingClock(owner string) {
	tradingClockMu.Lock()
	defer tradingClockMu.Unlock()
	if tradingClockOwner == owner {
		tradingClockFn, tradingClockOwner = nil, ""
	}
}

func tradingClockBound() bool {
	tradingClockMu.RLock()
	defer tradingClockMu.RUnlock()
	return tradingClockFn != nil
}

type simulatedClock struct {
	mu  sync.RWMutex
	now time.Time
}

func (c *simulatedClock) Now() time.Time {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.now
}

func (c *simulatedClock) Set(t time.Time) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if t.Before(c.now) {
		return fmt.Errorf("the simulated clock cannot move back from %s to %s", c.now.UTC().Format(time.RFC3339Nano), t.UTC().Format(time.RFC3339Nano))
	}
	c.now = t.UTC()
	return nil
}

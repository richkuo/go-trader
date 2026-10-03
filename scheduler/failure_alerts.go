package main

import (
	"fmt"
	"sync"
	"time"
	"unicode/utf8"
)

const (
	directionOpen  = "open"
	directionClose = "close"
)

func shouldNotifyDrainFailure(failureCount int, lastNotifiedAt, now time.Time) bool {
	if failureCount <= 1 {
		return true
	}
	if failureCount%10 == 0 {
		return true
	}
	if !lastNotifiedAt.IsZero() && now.Sub(lastNotifiedAt) >= effectiveAlertThrottleInterval() {
		return true
	}
	return false
}

func formatDrainFailureAlert(platform, strategyID, symbol string, size float64, errMsg string, count int) string {
	countNote := ""
	if count > 1 {
		countNote = fmt.Sprintf(" (failure #%d, still retrying)", count)
	}
	return fmt.Sprintf("**CIRCUIT CLOSE FAILED** [%s] %s %s sz=%.6f: %s%s",
		strategyID, platform, symbol, size, errMsg, countNote)
}

type liveExecFailureEntry struct {
	count          int
	lastNotifiedAt time.Time
	lastErrSig     string
}

type LiveExecFailureThrottle struct {
	mu      sync.Mutex
	entries map[string]*liveExecFailureEntry
}

func liveExecKey(strategyID, platform, symbol, direction string) string {
	return strategyID + "|" + platform + "|" + symbol + "|" + direction
}

func truncErrSig(errMsg string) string {
	const limit = 120
	if len(errMsg) <= limit {
		return errMsg
	}
	cut := limit
	for cut > 0 && !utf8.RuneStart(errMsg[cut]) {
		cut--
	}
	return errMsg[:cut]
}

func (t *LiveExecFailureThrottle) Record(key, errSig string, now time.Time) (bool, int) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.entries == nil {
		t.entries = make(map[string]*liveExecFailureEntry)
	}
	e := t.entries[key]
	if e == nil {
		e = &liveExecFailureEntry{}
		t.entries[key] = e
	}
	sig := truncErrSig(errSig)
	if sig != e.lastErrSig {
		e.count = 1
		e.lastErrSig = sig
		e.lastNotifiedAt = now
		return true, 1
	}
	e.count++
	if shouldNotifyDrainFailure(e.count, e.lastNotifiedAt, now) {
		e.lastNotifiedAt = now
		return true, e.count
	}
	return false, e.count
}

func (t *LiveExecFailureThrottle) Clear(key string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.entries != nil {
		delete(t.entries, key)
	}
}

var liveExecThrottle = &LiveExecFailureThrottle{}

func formatLiveExecFailureAlert(strategyID, platform, direction, symbol, errMsg string, count int) string {
	countNote := ""
	if count > 1 {
		countNote = fmt.Sprintf(" (failure #%d)", count)
	}
	return fmt.Sprintf("**LIVE ORDER FAILED** [%s] %s %s %s: %s%s",
		strategyID, platform, direction, symbol, errMsg, countNote)
}

func notifyLiveExecFailure(notifier *MultiNotifier, sc StrategyConfig, direction, symbol, errMsg string) {
	if notifier == nil || !notifier.HasBackends() {
		return
	}
	key := liveExecKey(sc.ID, sc.Platform, symbol, direction)
	shouldNotify, count := liveExecThrottle.Record(key, errMsg, time.Now().UTC())
	if !shouldNotify {
		return
	}
	msg := formatLiveExecFailureAlert(sc.ID, sc.Platform, direction, symbol, errMsg, count)
	notifier.SendToAllChannels(msg)
	notifier.SendOwnerDM(msg)
}

func clearLiveExecThrottle(sc StrategyConfig, direction, symbol string) {
	key := liveExecKey(sc.ID, sc.Platform, symbol, direction)
	liveExecThrottle.Clear(key)
}

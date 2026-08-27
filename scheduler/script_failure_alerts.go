package main

import (
	"fmt"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"
)

const scriptFailureAlertThreshold = 3

const scriptFailureTransientAlertThreshold = 15

const scriptFailureTransientAlertMaxDuration = 75 * time.Minute

var scriptFailureTransientRE = regexp.MustCompile(
	`(?i)(\(429[,\)]|(?:http|status)[\s_]?429|status_code[=:]\s*429|rate.?limit|error from cloudfront` +
		`|\(5\d\d[,\)]|(?:http|status)[\s_]?5\d\d|status_code[=:]\s*5\d\d|bad.?gateway` +
		`|script timed out after` +
		`)`)

func scriptFailureErrorIsTransient(errMsg string) bool {
	return scriptFailureTransientRE.MatchString(errMsg)
}

type scriptFailureMode string

const (
	scriptFailureCrash scriptFailureMode = "crash"

	scriptFailureError scriptFailureMode = "error"
)

func scriptFailureModeLabel(mode scriptFailureMode) string {
	if mode == scriptFailureCrash {
		return "hard crash"
	}
	return "script error"
}

type scriptFailureEntry struct {
	count          int
	lastErrSig     string
	firstSeenAt    time.Time
	lastNotifiedAt time.Time
	alerted        bool
}

type ScriptFailureTracker struct {
	mu      sync.Mutex
	entries map[string]*scriptFailureEntry
}

func (t *ScriptFailureTracker) Record(strategyID, errSig string, now time.Time) (bool, int) {
	return recordScriptFailureAtThreshold(t, strategyID, errSig, now, scriptFailureAlertThreshold, 0)
}

func (t *ScriptFailureTracker) Clear(strategyID string) (bool, int) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.entries == nil {
		return false, 0
	}
	e := t.entries[strategyID]
	if e == nil {
		return false, 0
	}
	recovered := e.alerted
	priorCount := e.count
	delete(t.entries, strategyID)
	return recovered, priorCount
}

var scriptFailureTracker = &ScriptFailureTracker{}

var scriptFailureTransientTracker = &ScriptFailureTracker{}

func formatScriptFailureAlert(sc StrategyConfig, mode scriptFailureMode, errMsg string, count int) string {
	return fmt.Sprintf("**SIGNAL SCRIPT FAILING** [%s] %s %s (pid=%d, %s, %d consecutive failures): %s",
		sc.ID, sc.Platform, sc.Script, os.Getpid(), scriptFailureModeLabel(mode), count, errMsg)
}

func formatScriptRecoveredAlert(sc StrategyConfig, priorCount int) string {
	return fmt.Sprintf("**SIGNAL SCRIPT RECOVERED** [%s] %s %s (pid=%d): succeeded after %d consecutive failures",
		sc.ID, sc.Platform, sc.Script, os.Getpid(), priorCount)
}

func formatScriptFailureTransientAlert(sc StrategyConfig, mode scriptFailureMode, errMsg string, count int) string {
	return fmt.Sprintf("**SIGNAL SCRIPT FAILING (sustained upstream error)** [%s] %s %s (pid=%d, %s, %d consecutive transient failures): %s",
		sc.ID, sc.Platform, sc.Script, os.Getpid(), scriptFailureModeLabel(mode), count, errMsg)
}

func recordScriptFailureAtThreshold(t *ScriptFailureTracker, strategyID, errSig string, now time.Time, threshold int, maxDuration time.Duration) (bool, int) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.entries == nil {
		t.entries = make(map[string]*scriptFailureEntry)
	}
	e := t.entries[strategyID]
	if e == nil {
		e = &scriptFailureEntry{}
		t.entries[strategyID] = e
	}
	sig := truncErrSig(errSig)
	sigChanged := sig != e.lastErrSig
	e.count++
	if e.count == 1 {
		e.firstSeenAt = now
	}
	e.lastErrSig = sig

	crossed := e.count >= threshold
	if !crossed && maxDuration > 0 && !e.firstSeenAt.IsZero() {
		crossed = now.Sub(e.firstSeenAt) >= maxDuration
	}
	if !crossed {
		return false, e.count
	}

	shouldNotify := false
	switch {
	case !e.alerted:
		shouldNotify = true
	case sigChanged:
		shouldNotify = true
	case e.count%10 == 0:
		shouldNotify = true
	case !e.lastNotifiedAt.IsZero() && now.Sub(e.lastNotifiedAt) >= effectiveAlertThrottleInterval():
		shouldNotify = true
	}
	if shouldNotify {
		e.alerted = true
		e.lastNotifiedAt = now
	}
	return shouldNotify, e.count
}

func notifyScriptFailure(notifier *MultiNotifier, sc StrategyConfig, mode scriptFailureMode, errMsg string) {
	now := time.Now().UTC()
	if scriptFailureErrorIsTransient(errMsg) {
		fmt.Printf("[WARN] transient script failure [%s]: %s\n", sc.ID, errMsg)
		shouldNotify, count := recordScriptFailureAtThreshold(
			scriptFailureTransientTracker, sc.ID, errMsg, now,
			scriptFailureTransientAlertThreshold, scriptFailureTransientAlertMaxDuration)
		if !shouldNotify || notifier == nil || !notifier.HasBackends() {
			return
		}
		msg := formatScriptFailureTransientAlert(sc, mode, errMsg, count)
		notifier.SendToAllChannels(msg)
		notifier.SendOwnerDM(msg)
		return
	}
	shouldNotify, count := scriptFailureTracker.Record(sc.ID, errMsg, now)
	if !shouldNotify || notifier == nil || !notifier.HasBackends() {
		return
	}
	msg := formatScriptFailureAlert(sc, mode, errMsg, count)
	notifier.SendToAllChannels(msg)
	notifier.SendOwnerDM(msg)
}

func formatBatchSharedStateFailureAlert(sc StrategyConfig, errMsg string, memberIDs []string, count int) string {
	members := "none"
	if len(memberIDs) > 0 {
		members = strings.Join(memberIDs, ", ")
	}
	return fmt.Sprintf("**HL BATCH SHARED STATE FAILED** [%s] %s (pid=%d, %d consecutive failures, %d strategies affected: %s): %s",
		sc.ID, sc.Script, os.Getpid(), count, len(memberIDs), members, errMsg)
}

func formatBatchSharedStateRecoveredAlert(sc StrategyConfig, priorCount int) string {
	return fmt.Sprintf("**HL BATCH SHARED STATE RECOVERED** [%s] %s (pid=%d): succeeded after %d consecutive failures",
		sc.ID, sc.Script, os.Getpid(), priorCount)
}

func notifyBatchSharedStateFailure(notifier *MultiNotifier, sc StrategyConfig, errMsg string, memberIDs []string) {
	now := time.Now().UTC()
	if scriptFailureErrorIsTransient(errMsg) {
		fmt.Printf("[WARN] transient hl-batch shared-state failure [%s]: %s\n", sc.ID, errMsg)
		shouldNotify, count := recordScriptFailureAtThreshold(
			scriptFailureTransientTracker, sc.ID, errMsg, now,
			scriptFailureTransientAlertThreshold, scriptFailureTransientAlertMaxDuration)
		if !shouldNotify || notifier == nil || !notifier.HasBackends() {
			return
		}
		msg := formatBatchSharedStateFailureAlert(sc, errMsg, memberIDs, count)
		notifier.SendToAllChannels(msg)
		notifier.SendOwnerDM(msg)
		return
	}
	shouldNotify, count := scriptFailureTracker.Record(sc.ID, errMsg, now)
	if !shouldNotify || notifier == nil || !notifier.HasBackends() {
		return
	}
	msg := formatBatchSharedStateFailureAlert(sc, errMsg, memberIDs, count)
	notifier.SendToAllChannels(msg)
	notifier.SendOwnerDM(msg)
}

func clearBatchSharedStateFailure(notifier *MultiNotifier, sc StrategyConfig) {
	recovered, priorCount := scriptFailureTracker.Clear(sc.ID)
	transientRecovered, transientPrior := scriptFailureTransientTracker.Clear(sc.ID)
	if !recovered && !transientRecovered {
		return
	}
	if notifier == nil || !notifier.HasBackends() {
		return
	}
	prior := priorCount
	if transientPrior > prior {
		prior = transientPrior
	}
	msg := formatBatchSharedStateRecoveredAlert(sc, prior)
	notifier.SendToAllChannels(msg)
	notifier.SendOwnerDM(msg)
}

func clearScriptFailure(notifier *MultiNotifier, sc StrategyConfig) {
	recovered, priorCount := scriptFailureTracker.Clear(sc.ID)
	transientRecovered, transientPrior := scriptFailureTransientTracker.Clear(sc.ID)
	if !recovered && !transientRecovered {
		return
	}
	if notifier == nil || !notifier.HasBackends() {
		return
	}
	prior := priorCount
	if transientPrior > prior {
		prior = transientPrior
	}
	msg := formatScriptRecoveredAlert(sc, prior)
	notifier.SendToAllChannels(msg)
	notifier.SendOwnerDM(msg)
}

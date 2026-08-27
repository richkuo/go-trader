package main

import (
	"fmt"
	"math"
	"os"
	"sort"
	"strings"
	"sync"
	"time"
)

const hlReconcileGapTolerance = 1e-6

const hlReconcileGapAlertThreshold = 3

const hlReconcileGapRealertRatio = 0.10

func hlReconcileGapLogInterval() time.Duration {
	return effectiveAlertThrottleInterval()
}

type hlReconcileGapEntry struct {
	cycles int

	alerted bool

	lastNotifiedAt time.Time

	lastNotifiedDelta float64

	lastLoggedAt time.Time

	lastLoggedDelta float64
}

type HLReconcileGapTracker struct {
	mu      sync.Mutex
	entries map[string]*hlReconcileGapEntry
}

func (t *HLReconcileGapTracker) Record(coin string, delta float64, now time.Time) (shouldNotify bool, shouldLog bool, cycles int) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.entries == nil {
		t.entries = make(map[string]*hlReconcileGapEntry)
	}
	e := t.entries[coin]
	if e == nil {
		e = &hlReconcileGapEntry{}
		t.entries[coin] = e
	}
	e.cycles++

	logMove := math.Abs(delta - e.lastLoggedDelta)
	logSigChanged := logMove > hlReconcileGapTolerance &&
		logMove > hlReconcileGapRealertRatio*math.Abs(e.lastLoggedDelta)
	shouldLog = e.lastLoggedAt.IsZero() ||
		logSigChanged ||
		now.Sub(e.lastLoggedAt) >= hlReconcileGapLogInterval()

	if e.cycles < hlReconcileGapAlertThreshold {
		if shouldLog {
			e.lastLoggedAt = now
			e.lastLoggedDelta = delta
		}
		return false, shouldLog, e.cycles
	}

	deltaMove := math.Abs(delta - e.lastNotifiedDelta)
	sigChanged := deltaMove > hlReconcileGapTolerance &&
		deltaMove > hlReconcileGapRealertRatio*math.Abs(e.lastNotifiedDelta)

	switch {
	case !e.alerted:
		shouldNotify = true
	case sigChanged:
		shouldNotify = true
	case !e.lastNotifiedAt.IsZero() && now.Sub(e.lastNotifiedAt) >= effectiveAlertThrottleInterval():
		shouldNotify = true
	}
	if shouldNotify {
		shouldLog = true
		e.alerted = true
		e.lastNotifiedAt = now
		e.lastNotifiedDelta = delta
	}
	if shouldLog {
		e.lastLoggedAt = now
		e.lastLoggedDelta = delta
	}
	return shouldNotify, shouldLog, e.cycles
}

func (t *HLReconcileGapTracker) Clear(coin string) (bool, int) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.entries == nil {
		return false, 0
	}
	e := t.entries[coin]
	if e == nil {
		return false, 0
	}
	recovered := e.alerted
	priorCount := e.cycles
	delete(t.entries, coin)
	return recovered, priorCount
}

func (t *HLReconcileGapTracker) trackedCoins() []string {
	t.mu.Lock()
	defer t.mu.Unlock()
	coins := make([]string, 0, len(t.entries))
	for c := range t.entries {
		coins = append(coins, c)
	}
	sort.Strings(coins)
	return coins
}

var hlReconcileGapTracker = &HLReconcileGapTracker{}

type hlReconcileGapResult struct {
	Coin       string
	DeltaQty   float64
	VirtualQty float64
	OnChainQty float64
	Strategies []string
}

func formatHLReconcileGapAlert(r hlReconcileGapResult, count int) string {
	strats := "—"
	if len(r.Strategies) > 0 {
		strats = strings.Join(r.Strategies, ", ")
	}
	return fmt.Sprintf(
		"**HL RECONCILE GAP** %s (pid=%d, %d consecutive cycles): virtual=%.6f vs on-chain=%.6f, residual=%+.6f could not be explained by an exact-OID fill. A phantom virtual position is feeding drawdown/kill-switch math. Strategies: %s. Verify the on-chain fill in HL user-fills and reconcile manually if needed — fail-closed by design: no SL close is booked or owner guessed without exact-OID confirmation.",
		r.Coin, os.Getpid(), count, r.VirtualQty, r.OnChainQty, r.DeltaQty, strats)
}

func formatHLReconcileGapRecovered(coin string, priorCount int) string {
	return fmt.Sprintf(
		"**HL RECONCILE GAP RESOLVED** %s (pid=%d): the shared-coin reconciliation gap cleared after %d cycles of drift.",
		coin, os.Getpid(), priorCount)
}

func reportHLReconcileGaps(notifier ownerDMSender, results []hlReconcileGapResult) {
	now := time.Now().UTC()
	emit := func(msg string) {
		if notifier == nil || isNilSender(notifier) {
			return
		}
		notifier.SendOwnerDM(msg)
	}
	present := make(map[string]bool, len(results))

	sort.Slice(results, func(i, j int) bool { return results[i].Coin < results[j].Coin })
	for _, r := range results {
		present[r.Coin] = true
		if math.Abs(r.DeltaQty) > hlReconcileGapTolerance {
			shouldNotify, shouldLog, count := hlReconcileGapTracker.Record(r.Coin, r.DeltaQty, now)
			if shouldLog {
				fmt.Printf("[WARN] hl-sync: %s reconciliation gap residual=%+.6f persists (virtual=%.6f on-chain=%.6f, strategies: %v)\n",
					r.Coin, r.DeltaQty, r.VirtualQty, r.OnChainQty, r.Strategies)
			}
			if shouldNotify {
				emit(formatHLReconcileGapAlert(r, count))
			}
			continue
		}
		if recovered, prior := hlReconcileGapTracker.Clear(r.Coin); recovered {
			emit(formatHLReconcileGapRecovered(r.Coin, prior))
		}
	}

	for _, coin := range hlReconcileGapTracker.trackedCoins() {
		if present[coin] {
			continue
		}
		if recovered, prior := hlReconcileGapTracker.Clear(coin); recovered {
			emit(formatHLReconcileGapRecovered(coin, prior))
		}
	}
}

func collectHLReconcileGapResults(state *AppState, mu *sync.RWMutex) []hlReconcileGapResult {
	mu.RLock()
	defer mu.RUnlock()
	if len(state.ReconciliationGaps) == 0 {
		return nil
	}
	results := make([]hlReconcileGapResult, 0, len(state.ReconciliationGaps))
	for coin, g := range state.ReconciliationGaps {
		if g == nil {
			continue
		}
		strats := make([]string, len(g.Strategies))
		copy(strats, g.Strategies)
		results = append(results, hlReconcileGapResult{
			Coin:       coin,
			DeltaQty:   g.DeltaQty,
			VirtualQty: g.VirtualQty,
			OnChainQty: g.OnChainQty,
			Strategies: strats,
		})
	}
	return results
}

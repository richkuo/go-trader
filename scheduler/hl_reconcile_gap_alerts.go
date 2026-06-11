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

// #971: Active operator alerting for persistent Hyperliquid shared-coin
// reconciliation gaps.
//
// When a shared coin's on-chain position drops but the reconciler cannot
// confirm the residual via an exact stop-loss/TP OID (user-fills miss or a
// wrong OID), it fails closed (#964/#965): it does NOT guess an owner or book
// an SL close, leaving a `ReconciliationGap` and a phantom virtual position
// that keeps feeding drawdown/kill-switch math. The reconciler already
// auto-heals such a gap WITHIN a later cycle the moment a user-fills lookup
// confirms the exact OID (the confirmedSLFills / Detector paths re-run every
// reconcile with a freshly-built fill resolver). So a transient miss self-heals
// silently; this layer only surfaces a gap that stays stuck — the lookup never
// confirms — so an operator can reconcile it manually. It deliberately does NOT
// book or guess anything: the fail-closed invariant is preserved.

// hlReconcileGapTolerance is the absolute signed-qty drift below which a
// shared-coin reconciliation gap is treated as float noise rather than a real
// stuck gap. Mirrors the 1e-6 qty epsilon used throughout the reconciler.
const hlReconcileGapTolerance = 1e-6

// hlReconcileGapAlertThreshold is the number of CONSECUTIVE reconcile cycles a
// coin's gap must persist before the first operator alert fires. A recorded gap
// already means THIS cycle's user-fills lookup could not explain the residual
// (the reconciler books and clears the gap in-cycle the moment a lookup
// confirms the exact OID), so a gap surviving several consecutive lookups is
// genuinely stuck — a permanently missing or wrong OID — not the brief HL
// indexer lag that clears once the fill is indexed. Three absorbs a couple of
// lagged cycles while still surfacing a real stuck gap within a few cycles.
const hlReconcileGapAlertThreshold = 3

// hlReconcileGapRealertRatio gates re-alerts on an already-alerted coin to a
// material change in the residual qty (relative to the qty at the LAST
// notification), so a gap whose residual drifts slightly (a peer partial fill,
// a small re-open) does not spam; combined with the every-10th-cycle / hourly
// back-off below.
const hlReconcileGapRealertRatio = 0.10

// hlReconcileGapEntry is one slot in the per-coin gap tracker.
type hlReconcileGapEntry struct {
	// cycles counts CONSECUTIVE over-tolerance reconcile cycles for this coin.
	// It is the duration shown in operator messages and the base of the
	// every-10th-cycle cadence.
	cycles int
	// alerted marks that the confirmation window has been crossed and an alert
	// has fired, so subsequent cycles re-throttle instead of re-confirming.
	alerted bool
	// lastNotifiedAt anchors the hourly back-off.
	lastNotifiedAt time.Time
	// lastNotifiedDelta is the residual qty at the LAST NOTIFICATION (not the
	// previous cycle) — the anchor for the materially-changed re-alert gate.
	lastNotifiedDelta float64
}

// HLReconcileGapTracker throttles operator alerts for persistent Hyperliquid
// shared-coin reconciliation gaps (#971). State is per-coin and in-memory;
// it resets on restart. A gap must persist hlReconcileGapAlertThreshold
// consecutive cycles before the first alert; thereafter it re-alerts only on a
// materially changed residual, every 10th cycle, or once an hour, until the
// gap clears (Clear).
type HLReconcileGapTracker struct {
	mu      sync.Mutex
	entries map[string]*hlReconcileGapEntry
}

// Record registers an over-tolerance gap for coin and reports whether this
// cycle should fire an operator alert, along with the coin's post-increment
// consecutive over-tolerance cycle count (the duration shown in messages). No
// alert fires until the streak reaches hlReconcileGapAlertThreshold; the first
// alert fires on that crossing, then re-throttles while the gap persists.
func (t *HLReconcileGapTracker) Record(coin string, delta float64, now time.Time) (bool, int) {
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

	// Confirmation window: a transient one- or two-cycle gap (indexer lag that
	// resolves once the fill is indexed) never reaches the threshold — it
	// clears via Clear first — so it never alerts.
	if e.cycles < hlReconcileGapAlertThreshold {
		return false, e.cycles
	}

	// "Materially changed" = the residual moved, since the LAST NOTIFICATION,
	// by more than the qty epsilon AND more than hlReconcileGapRealertRatio of
	// the notified magnitude — so a growing/sign-flipping gap re-surfaces while
	// small per-cycle wiggle stays inside the backed-off cadence.
	deltaMove := math.Abs(delta - e.lastNotifiedDelta)
	sigChanged := deltaMove > hlReconcileGapTolerance &&
		deltaMove > hlReconcileGapRealertRatio*math.Abs(e.lastNotifiedDelta)

	shouldNotify := false
	switch {
	case !e.alerted:
		shouldNotify = true // first crossing of the confirmation window
	case sigChanged:
		shouldNotify = true
	case e.cycles%10 == 0:
		shouldNotify = true
	case !e.lastNotifiedAt.IsZero() && now.Sub(e.lastNotifiedAt) >= time.Hour:
		shouldNotify = true
	}
	if shouldNotify {
		e.alerted = true
		e.lastNotifiedAt = now
		e.lastNotifiedDelta = delta
	}
	return shouldNotify, e.cycles
}

// Clear resets the streak for coin after a within-tolerance (or vanished)
// cycle and reports whether the coin had alerted (a recovery notice is
// warranted) plus the consecutive over-tolerance cycle count that just ended.
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

// trackedCoins returns the sorted set of coins with a live streak.
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

// hlReconcileGapTracker is the package-level singleton; resets on restart.
var hlReconcileGapTracker = &HLReconcileGapTracker{}

// hlReconcileGapResult is a snapshot of one shared coin's reconciliation gap
// for a cycle, built from state.ReconciliationGaps under the read lock before
// the (blocking) DM emission.
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

// reportHLReconcileGaps evaluates each shared coin's post-reconcile gap against
// the qty tolerance and fires throttled owner alerts (first detection after the
// confirmation window, then backed off) or a one-shot recovery notice when a
// previously-alerting gap clears. Gaps are always recorded so counts and
// recovery state stay accurate even with no notifier. It must be called every
// cycle the reconciler ran (so cycle counts equal reconcile invocations), with
// results built from state.ReconciliationGaps; coins no longer present in the
// gap map (no longer shared, or reconciled away) are swept and recovery-cleared.
//
// It never books, guesses, or mutates positions — alerting only — so the
// fail-closed reconciliation invariant is preserved.
func reportHLReconcileGaps(notifier ownerDMSender, results []hlReconcileGapResult) {
	now := time.Now().UTC()
	emit := func(msg string) {
		if notifier == nil || isNilSender(notifier) {
			return
		}
		notifier.SendOwnerDM(msg)
	}
	present := make(map[string]bool, len(results))
	// Sort for deterministic alert ordering across the cycle.
	sort.Slice(results, func(i, j int) bool { return results[i].Coin < results[j].Coin })
	for _, r := range results {
		present[r.Coin] = true
		if math.Abs(r.DeltaQty) > hlReconcileGapTolerance {
			shouldNotify, count := hlReconcileGapTracker.Record(r.Coin, r.DeltaQty, now)
			fmt.Printf("[WARN] hl-sync: %s reconciliation gap residual=%+.6f persists (virtual=%.6f on-chain=%.6f, strategies: %v)\n",
				r.Coin, r.DeltaQty, r.VirtualQty, r.OnChainQty, r.Strategies)
			if shouldNotify {
				emit(formatHLReconcileGapAlert(r, count))
			}
			continue
		}
		if recovered, prior := hlReconcileGapTracker.Clear(r.Coin); recovered {
			emit(formatHLReconcileGapRecovered(r.Coin, prior))
		}
	}
	// Sweep coins tracked from a prior cycle that are absent this cycle (no
	// longer shared, or reconciled away): treat as resolved.
	for _, coin := range hlReconcileGapTracker.trackedCoins() {
		if present[coin] {
			continue
		}
		if recovered, prior := hlReconcileGapTracker.Clear(coin); recovered {
			emit(formatHLReconcileGapRecovered(coin, prior))
		}
	}
}

// collectHLReconcileGapResults snapshots state.ReconciliationGaps under the
// read lock into a slice safe to use after the lock is released.
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

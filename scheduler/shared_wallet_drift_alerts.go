package main

import (
	"fmt"
	"math"
	"os"
	"strings"
	"sync"
	"time"
)

// sharedWalletDriftTolerance is the cent-exact reconciliation tolerance (#918).
// Once per-strategy values are exchange-derived, Σ member value should equal
// the real account balance to the cent every cycle, so any excess is a genuine
// accounting/attribution bug (an on-chain position no member owns, a weight
// that summed to zero), NOT expected mark/fee noise. One cent absorbs benign
// float rounding only.
const sharedWalletDriftTolerance = 0.01

// sharedWalletDriftAlertThreshold is the number of CONSECUTIVE over-tolerance
// cycles before the first operator alert fires. It is 2 (not 1) to absorb a
// one-cycle booking lag: reconcileSharedWalletDisplayValues runs in the risk
// phase using freshly-fetched on-chain positions but the PRIOR cycle's virtual
// books — it executes before this cycle's reconcilePendingLimitOrders /
// drainPendingManualActions / reconcileHyperliquidAccountPositions create the
// matching virtual position. So a resting limit fill (#883) or an external
// manual open is legitimately unowned (an orphan → drift) for exactly one
// cycle, then the book catches up next cycle. Requiring two consecutive cycles
// means that transient self-heals silently (no alert, no recovery notice) while
// a genuine attribution bug — which persists across cycles — still alerts
// within two cycles.
const sharedWalletDriftAlertThreshold = 2

// sharedWalletDriftEntry is one slot in the per-wallet drift tracker.
type sharedWalletDriftEntry struct {
	// coinStreaks counts, per orphan coin, how many CONSECUTIVE over-tolerance
	// cycles that coin has stayed unowned. Continuity is per coin — not the
	// exact orphan set — so a persistent orphan keeps confirming even while
	// unrelated one-cycle transients churn the set around it (#920 review).
	// An over-tolerance cycle with no orphan coins (weighting bug) tracks under
	// the pseudo-coin "".
	coinStreaks map[string]int
	// alertedCoins marks coins whose confirmation alert already fired, so a NEW
	// persistent orphan appearing after a prior alert (no intervening clean
	// cycle) re-confirms and alerts deterministically when ITS streak crosses
	// the threshold, regardless of drift magnitude.
	alertedCoins   map[string]bool
	lastNotifiedAt time.Time
	alerted        bool
	lastDriftCents int64 // re-alert when the drift magnitude shifts
}

// SharedWalletDriftTracker throttles the cent-exact drift alarm per shared
// wallet so a persistent attribution bug does not spam the operator every
// cycle. It alerts after a short consecutive-detection confirmation window
// (sharedWalletDriftAlertThreshold) so a one-cycle booking lag for an
// externally-originated fill self-heals without alarming; a real bug persists
// and alerts within two cycles. All state is in-memory and resets on restart.
type SharedWalletDriftTracker struct {
	mu      sync.Mutex
	entries map[string]*sharedWalletDriftEntry
}

// Record registers an over-tolerance drift for walletKey and reports whether
// this cycle should fire an operator alert, along with the post-increment
// consecutive-detection count. No alert fires until the streak reaches
// sharedWalletDriftAlertThreshold; the first alert fires on that crossing, then
// re-throttles (a materially changed drift, every 10th cycle, or once an hour)
// while the drift persists.
//
// orphanCoins identifies WHICH positions are unattributed this cycle (sorted).
// Confirmation continuity is tracked PER COIN: a coin must stay unowned for
// sharedWalletDriftAlertThreshold consecutive cycles before it alerts. Two
// unrelated one-cycle transients on consecutive cycles (a resting-limit fill on
// one coin, an external manual open on another) therefore never confirm — but a
// genuinely persistent orphan keeps confirming even while transients on OTHER
// coins churn the set around it. The drift magnitude is deliberately NOT part
// of the continuity key — a real orphan's unrealized P&L moves with the mark
// every cycle. An over-tolerance cycle with no orphan coins (weighting bug)
// counts continuity under a pseudo-coin so it still confirms like a bare
// counter. The returned count is the longest current per-coin streak.
func (t *SharedWalletDriftTracker) Record(walletKey string, drift float64, orphanCoins []string, now time.Time) (bool, int) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.entries == nil {
		t.entries = make(map[string]*sharedWalletDriftEntry)
	}
	e := t.entries[walletKey]
	if e == nil {
		e = &sharedWalletDriftEntry{}
		t.entries[walletKey] = e
	}

	coins := orphanCoins
	if len(coins) == 0 {
		coins = []string{""} // weighting-bug drift: no orphan coin to key on
	}
	// Advance per-coin streaks; coins owned again this cycle drop out (their
	// continuity — and any per-coin alerted mark — resets).
	streaks := make(map[string]int, len(coins))
	for _, coin := range coins {
		streaks[coin] = e.coinStreaks[coin] + 1
	}
	e.coinStreaks = streaks
	maxStreak := 0
	confirmedNew := false
	for coin, n := range streaks {
		if n > maxStreak {
			maxStreak = n
		}
		if n >= sharedWalletDriftAlertThreshold && !e.alertedCoins[coin] {
			confirmedNew = true
		}
	}
	for coin := range e.alertedCoins {
		if _, still := streaks[coin]; !still {
			delete(e.alertedCoins, coin)
		}
	}

	driftCents := int64(math.Round(drift * 100))
	// "Materially changed" = the drift moved by more than a cent since the last
	// notification, so a slowly-worsening bug re-surfaces.
	sigChanged := absInt64(driftCents-e.lastDriftCents) > 1
	e.lastDriftCents = driftCents

	// Confirmation window: no coin has stayed unowned long enough yet — a
	// transient one-cycle orphan never reaches the threshold (it clears next
	// cycle via Clear or drops out of coinStreaks), so it never alerts.
	if maxStreak < sharedWalletDriftAlertThreshold {
		return false, maxStreak
	}

	shouldNotify := false
	switch {
	case confirmedNew:
		shouldNotify = true // a coin crossed its confirmation window
	case sigChanged:
		shouldNotify = true
	case maxStreak%10 == 0:
		shouldNotify = true
	case !e.lastNotifiedAt.IsZero() && now.Sub(e.lastNotifiedAt) >= time.Hour:
		shouldNotify = true
	}
	if shouldNotify {
		e.alerted = true
		e.lastNotifiedAt = now
		if e.alertedCoins == nil {
			e.alertedCoins = make(map[string]bool)
		}
		for coin, n := range streaks {
			if n >= sharedWalletDriftAlertThreshold {
				e.alertedCoins[coin] = true
			}
		}
	}
	return shouldNotify, maxStreak
}

// Clear resets the drift streak for walletKey after a within-tolerance cycle
// and reports whether the wallet had alerted (a recovery notice is warranted)
// plus the streak length that just ended.
func (t *SharedWalletDriftTracker) Clear(walletKey string) (bool, int) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.entries == nil {
		return false, 0
	}
	e := t.entries[walletKey]
	if e == nil {
		return false, 0
	}
	recovered := e.alerted
	priorCount := 0
	for _, n := range e.coinStreaks {
		if n > priorCount {
			priorCount = n
		}
	}
	delete(t.entries, walletKey)
	return recovered, priorCount
}

// sharedWalletDriftTracker is the package-level singleton; resets on restart.
var sharedWalletDriftTracker = &SharedWalletDriftTracker{}

func absInt64(v int64) int64 {
	if v < 0 {
		return -v
	}
	return v
}

// sharedWalletKeyLabel renders a wallet key as "{platform}/{account}" for
// operator messages. The account address is shown in full (it is a public
// on-chain address / API-key identifier already present in other operator logs).
func sharedWalletKeyLabel(key SharedWalletKey) string {
	return fmt.Sprintf("%s/%s", key.Platform, key.Account)
}

func formatSharedWalletDriftAlert(key SharedWalletKey, balance, memberSum, drift float64, count int, orphanCoins []string) string {
	orphanDetail := "no unattributed coins — check member weighting"
	if len(orphanCoins) > 0 {
		orphanDetail = "unattributed coins: " + strings.Join(orphanCoins, ", ")
	}
	return fmt.Sprintf(
		"**SHARED-WALLET DRIFT** %s (pid=%d, %d consecutive): Σ member value $%.2f vs real balance $%.2f — diff $%+.2f exceeds $%.2f tolerance (%s). Exchange-derived rows should reconcile exactly; this indicates an attribution/accounting bug (orphan position or weighting).",
		sharedWalletKeyLabel(key), os.Getpid(), count, memberSum, balance, drift, sharedWalletDriftTolerance, orphanDetail)
}

func formatSharedWalletDriftRecovered(key SharedWalletKey, priorCount int) string {
	return fmt.Sprintf(
		"**SHARED-WALLET DRIFT RESOLVED** %s (pid=%d): per-strategy values reconcile to the account balance again after %d cycles of drift.",
		sharedWalletKeyLabel(key), os.Getpid(), priorCount)
}

// reportSharedWalletDrift evaluates each reconciled wallet's drift against the
// cent tolerance and fires throttled operator alerts (first detection, then
// backed-off) or a one-shot recovery notice. Drift is always recorded so counts
// and recovery state stay accurate even with no notifier backends. Wallets not
// reconciled this cycle (balance fetch failed) are absent from results and so
// are neither alarmed nor recovery-cleared — their prior streak (if any) is
// preserved, matching the "skip on fetch failure, don't false-alarm" rule.
func reportSharedWalletDrift(notifier *MultiNotifier, results []sharedWalletDriftResult) {
	now := time.Now().UTC()
	for _, r := range results {
		label := sharedWalletKeyLabel(r.Key)
		if math.Abs(r.Drift) > sharedWalletDriftTolerance {
			shouldNotify, count := sharedWalletDriftTracker.Record(label, r.Drift, r.OrphanCoins, now)
			fmt.Printf("[WARN] shared-wallet %s drift $%+.2f (Σ members $%.2f vs balance $%.2f, orphans=[%s])\n",
				label, r.Drift, r.MemberSum, r.Balance, strings.Join(r.OrphanCoins, ","))
			if !shouldNotify || notifier == nil || !notifier.HasBackends() {
				continue
			}
			msg := formatSharedWalletDriftAlert(r.Key, r.Balance, r.MemberSum, r.Drift, count, r.OrphanCoins)
			notifier.SendToAllChannels(msg)
			notifier.SendOwnerDM(msg)
			continue
		}
		recovered, priorCount := sharedWalletDriftTracker.Clear(label)
		if !recovered || notifier == nil || !notifier.HasBackends() {
			continue
		}
		msg := formatSharedWalletDriftRecovered(r.Key, priorCount)
		notifier.SendToAllChannels(msg)
		notifier.SendOwnerDM(msg)
	}
}

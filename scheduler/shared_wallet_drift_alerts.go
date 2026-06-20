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

// sharedWalletDriftRealertRatio is the relative move, measured against the
// drift at the LAST NOTIFICATION, required to re-alert an already-alerted
// wallet ("materially changed"). The drift is an orphan position's
// exchange-reported unrealized P&L, which moves with the mark every cycle —
// an absolute cent threshold compared cycle-over-cycle would re-alert
// continuously and defeat the hourly back-off.
const sharedWalletDriftRealertRatio = 0.10

// sharedWalletDriftLogInterval throttles the stdout [WARN] drift line per
// wallet (#1088). The log is operator visibility, not an alert, so it fires at
// the onset of an over-tolerance episode and then at most once per this
// interval while the drift persists — NOT every reconcile cycle. Without it a
// stable drift logged once per ~35s reconcile cycle produced 620 lines in 6h.
// A cycle that ALSO fires an operator alert is always logged regardless, so the
// stdout trail and the Discord/DM alerts stay correlated.
const sharedWalletDriftLogInterval = time.Minute

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
	alertedCoins map[string]bool
	// cycles counts CONSECUTIVE over-tolerance cycles at the WALLET level,
	// independent of which orphan coin is responsible. It is the duration shown
	// in alert/recovery messages — per-coin streaks would undercount when the
	// orphan coin churns (#920 review round 4).
	cycles         int
	lastNotifiedAt time.Time
	// lastLoggedAt is the time of the last stdout [WARN] line for this wallet —
	// the anchor for the per-wallet log throttle (#1088), independent of the
	// notification throttle (lastNotifiedAt).
	lastLoggedAt time.Time
	alerted      bool
	// lastNotifiedDriftCents is the drift at the LAST NOTIFICATION (not the
	// previous cycle) — the anchor for the materially-changed re-alert gate.
	lastNotifiedDriftCents int64
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
// this cycle should fire an operator alert (shouldNotify), whether it should
// emit a stdout [WARN] line (shouldLog, #1088), and the wallet's post-increment
// consecutive over-tolerance cycle count (the duration shown in operator
// messages). No alert fires until some coin's streak reaches
// sharedWalletDriftAlertThreshold; the first alert fires on that crossing, then
// re-throttles (a materially changed drift, or once an hour) while the drift
// persists.
//
// shouldLog is throttled separately and more loosely than shouldNotify: it is
// true at the onset of an over-tolerance episode and then at most once per
// sharedWalletDriftLogInterval, plus on any cycle that fires an alert. This
// stops the per-cycle log spam (#1088: 620 lines in 6h) while preserving an
// onset record and periodic visibility of a persistent drift.
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
// counter.
//
// Re-alert gating: the drift is the orphan's exchange-reported unrealized P&L,
// which moves with the mark EVERY cycle, so "materially changed" is measured
// against the drift at the LAST NOTIFICATION (not the previous cycle) and must
// clear a relative threshold (sharedWalletDriftRealertRatio) as well as a
// cent. Mark wiggle therefore settles into the hourly cadence, while a
// genuinely growing (or sign-flipping) drift accumulates against the anchor and
// re-surfaces.
func (t *SharedWalletDriftTracker) Record(walletKey string, drift float64, orphanCoins []string, now time.Time) (shouldNotify bool, shouldLog bool, cycles int) {
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

	e.cycles++

	driftCents := int64(math.Round(drift * 100))
	// "Materially changed" = the drift moved, since the LAST NOTIFICATION, by
	// more than a cent AND more than sharedWalletDriftRealertRatio of the
	// notified magnitude — so a slowly-worsening bug accumulates against the
	// anchor and re-surfaces, while per-cycle mark wiggle on the orphan's uPnL
	// stays inside the backed-off cadence.
	deltaCents := absInt64(driftCents - e.lastNotifiedDriftCents)
	sigChanged := deltaCents > 1 &&
		float64(deltaCents) > sharedWalletDriftRealertRatio*float64(absInt64(e.lastNotifiedDriftCents))

	// Log throttle (#1088): emit the stdout [WARN] at the onset of an episode
	// and then at most once per sharedWalletDriftLogInterval. Computed before
	// the confirmation-window early return so onset is logged even during the
	// (un-alerted) confirmation window. A cycle that fires an alert overrides
	// this to always log (below).
	shouldLog = e.lastLoggedAt.IsZero() || now.Sub(e.lastLoggedAt) >= sharedWalletDriftLogInterval

	// Confirmation window: no coin has stayed unowned long enough yet — a
	// transient one-cycle orphan never reaches the threshold (it clears next
	// cycle via Clear or drops out of coinStreaks), so it never alerts.
	if maxStreak < sharedWalletDriftAlertThreshold {
		if shouldLog {
			e.lastLoggedAt = now
		}
		return false, shouldLog, e.cycles
	}

	switch {
	case confirmedNew:
		shouldNotify = true // a coin crossed its confirmation window
	case e.alerted && sigChanged:
		shouldNotify = true
	case !e.lastNotifiedAt.IsZero() && now.Sub(e.lastNotifiedAt) >= time.Hour:
		shouldNotify = true
	}
	if shouldNotify {
		shouldLog = true // always log a cycle that fires an operator alert
		e.alerted = true
		e.lastNotifiedAt = now
		e.lastNotifiedDriftCents = driftCents
		if e.alertedCoins == nil {
			e.alertedCoins = make(map[string]bool)
		}
		for coin, n := range streaks {
			if n >= sharedWalletDriftAlertThreshold {
				e.alertedCoins[coin] = true
			}
		}
	}
	if shouldLog {
		e.lastLoggedAt = now
	}
	return shouldNotify, shouldLog, e.cycles
}

// Clear resets the drift streak for walletKey after a within-tolerance cycle
// and reports whether the wallet had alerted (a recovery notice is warranted)
// plus the wallet-level consecutive over-tolerance cycle count that just ended
// (NOT the per-coin streak, which undercounts when the orphan coin churned
// during the episode).
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
	priorCount := e.cycles
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
	// The "diff" in the alert is the BASELINE-ANCHORED drift (post-baseline
	// change since the wallet was first reconciled). The displayed Σ member
	// value vs balance, however, is the RAW reconciliation — those are the
	// rows the operator sees. Showing raw diff (memberSum - balance) makes
	// the alert self-consistent with the numbers above it. A large raw diff
	// with a small baseline-anchored drift MAY be legacy data adopted into the
	// baseline at first contact (masked by design — see
	// wallet_ledger_state.baseline_offset_usd), but can equally be a live
	// orphan/weighting bug sitting near the offset; the alert flags it for
	// investigation rather than pre-explaining it.
	rawDiff := memberSum - balance
	return fmt.Sprintf(
		"**SHARED-WALLET DRIFT** %s (pid=%d, %d consecutive): Σ member value $%.2f vs real balance $%.2f — raw reconciliation diff $%+.2f, post-baseline drift $%+.2f (>$%.2f tolerance). %s. Exchange-derived rows should reconcile exactly; a large raw diff with a small post-baseline drift may indicate legacy data baked into the adopted baseline OR a live attribution bug (orphan/weighting) near the offset — investigate before assuming it is benign.",
		sharedWalletKeyLabel(key), os.Getpid(), count, memberSum, balance, rawDiff, drift, sharedWalletDriftTolerance, orphanDetail)
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
			shouldNotify, shouldLog, count := sharedWalletDriftTracker.Record(label, r.Drift, r.OrphanCoins, now)
			if shouldLog {
				rawDiff := r.MemberSum - r.Balance
				fmt.Printf("[WARN] shared-wallet %s drift $%+.2f (Σ members $%.2f vs balance $%.2f, rawDiff $%+.2f, orphans=[%s])\n",
					label, r.Drift, r.MemberSum, r.Balance, rawDiff, strings.Join(r.OrphanCoins, ","))
			}
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

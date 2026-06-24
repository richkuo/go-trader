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

// sharedWalletDriftLogInterval is the heartbeat ceiling for the stdout [WARN]
// drift line per wallet (#1088): once an over-tolerance episode is underway, a
// STABLE, unchanging drift re-logs at most once per this interval. It is aligned
// to the hourly notification back-off (time.Hour) so a persistent stable drift's
// stdout cadence matches its alert cadence instead of spamming the log — a flat
// per-cycle log produced 620 lines in 6h, and even a 1-minute interval still
// logged ~once/70s ≈ 310. A drift that materially CHANGES
// (sharedWalletDriftRealertRatio, anchored to the last logged line) logs
// immediately regardless of this interval, and so does any cycle that fires an
// operator alert, so onset, worsening, and alert visibility are all preserved
// while a stable drift collapses to onset + the hourly heartbeat + alert cycles.
const sharedWalletDriftLogInterval = time.Hour

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
	// the anchor for the per-wallet log heartbeat (#1088), independent of the
	// notification throttle (lastNotifiedAt).
	lastLoggedAt time.Time
	// lastLoggedDriftCents is the drift (in cents) at the last stdout [WARN]
	// line — the anchor for the materially-changed log gate (#1088), kept
	// separate from lastNotifiedDriftCents so the looser log cadence and the
	// notification cadence don't perturb each other.
	lastLoggedDriftCents int64
	alerted              bool
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
// true at the onset of an over-tolerance episode, whenever the drift materially
// changes since the last logged line, on any cycle that fires an alert, and
// otherwise at most once per sharedWalletDriftLogInterval as a "still-present"
// heartbeat. This stops the per-cycle log spam (#1088: 620 lines in 6h) while
// preserving onset, worsening-drift, and alert visibility — a stable drift
// collapses to onset + the hourly heartbeat + alert cycles.
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

	// Log throttle (#1088): emit the stdout [WARN] at the onset of an episode,
	// whenever the drift materially changes since the last logged line, and then
	// at most once per sharedWalletDriftLogInterval as a "still-present"
	// heartbeat. Computed before the confirmation-window early return so onset is
	// logged even during the (un-alerted) confirmation window. A cycle that fires
	// an alert overrides this to always log (below).
	//
	// The materially-changed gate is anchored to the LAST LOGGED drift (not the
	// last notified one) so it preserves per-move visibility of a worsening drift
	// — the explicit reason #1088 throttled rather than gated the log behind
	// shouldNotify — while a stable, unchanging drift collapses to onset + the
	// hourly heartbeat + alert cycles, so it can no longer read as spam (a flat
	// 1-minute interval still logged ~once/70s ≈ 310 lines/6h).
	logDeltaCents := absInt64(driftCents - e.lastLoggedDriftCents)
	logSigChanged := logDeltaCents > 1 &&
		float64(logDeltaCents) > sharedWalletDriftRealertRatio*float64(absInt64(e.lastLoggedDriftCents))
	shouldLog = e.lastLoggedAt.IsZero() ||
		logSigChanged ||
		now.Sub(e.lastLoggedAt) >= sharedWalletDriftLogInterval

	// Confirmation window: no coin has stayed unowned long enough yet — a
	// transient one-cycle orphan never reaches the threshold (it clears next
	// cycle via Clear or drops out of coinStreaks), so it never alerts.
	if maxStreak < sharedWalletDriftAlertThreshold {
		if shouldLog {
			e.lastLoggedAt = now
			e.lastLoggedDriftCents = driftCents
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
		e.lastLoggedDriftCents = driftCents
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

// formatSharedWalletJournalDriftAlert is the #1100 journal-basis drift alert:
// the wallet's total is reconstructed from the exchange's own cash-flow events
// (fills/funding/transfers) rather than the internal trade ledger, so a non-zero
// drift is a journal gap (a missing or lagging exchange event) or an
// unaccounted balance movement — not an attribution/weighting bug.
func formatSharedWalletJournalDriftAlert(key SharedWalletKey, balance, expectedEquity, drift float64, count int, orphanCoins []string) string {
	// An over-tolerance journal drift can co-occur with an unowned position: the
	// two are separate concerns (a missing/lagging event vs unmanaged exposure),
	// so when both are present this alert reports the drift truthfully and folds
	// the orphan in as additional context rather than ever claiming the total is
	// "within tolerance" (#1107).
	orphanNote := ""
	if len(orphanCoins) > 0 {
		orphanNote = fmt.Sprintf(" Additionally, %d on-chain position(s) are owned by NO strategy (%s) — investigate that unmanaged exposure as a possibly-separate issue alongside the journal gap.",
			len(orphanCoins), strings.Join(orphanCoins, ", "))
	}
	return fmt.Sprintf(
		"**SHARED-WALLET DRIFT (exchange journal)** %s (pid=%d, %d consecutive): exchange accountValue $%.2f vs cash-flow-journal expected-equity $%.2f — drift $%+.2f (>$%.2f tolerance). The total is reconstructed from on-chain fills/funding/transfers; a persistent drift means a journal gap (missing/lagging exchange event, or an unmapped balance movement), not a strategy-attribution bug — investigate the exchange event feed before assuming it is benign.%s",
		sharedWalletKeyLabel(key), os.Getpid(), count, balance, expectedEquity, drift, sharedWalletDriftTolerance, orphanNote)
}

// formatSharedWalletJournalOrphanAlert fires under the #1100 journal basis when
// the exchange total reconciles to the journal (no total-equity drift) BUT one
// or more on-chain positions are owned by NO strategy. The journal total is
// account-level and nets an orphan's fill + uPnL to ~0, so the orphan-exposure
// signal would otherwise be silenced by the basis switch (#1107) — this keeps
// the real-unmanaged-exposure alarm alive independent of the total drift.
func formatSharedWalletJournalOrphanAlert(key SharedWalletKey, balance, expectedEquity, drift float64, count int, orphanCoins []string) string {
	return fmt.Sprintf(
		"**SHARED-WALLET ORPHAN POSITION** %s (pid=%d, %d consecutive): exchange accountValue $%.2f reconciles to cash-flow-journal expected-equity $%.2f (total drift $%+.2f, within tolerance) BUT %d on-chain position(s) are owned by NO strategy: %s. The exchange total is correct, so this is unmanaged/un-attributed on-chain exposure — NOT a journal gap. Investigate the unowned position(s) before assuming it is benign.",
		sharedWalletKeyLabel(key), os.Getpid(), count, balance, expectedEquity, drift, len(orphanCoins), strings.Join(orphanCoins, ", "))
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
		// #1107: the journal is the governing basis but produced no reading this
		// cycle (a transient feed miss / a just-anchored baseline). Treat it as
		// "no info" and PRESERVE the journal streak rather than resetting it off
		// the within-tolerance trade-ledger fallback — matching the "absent from
		// results → streak preserved" rule above. A transient miss during a real
		// journal-gap episode must not delay or suppress the operator alarm.
		if r.JournalPending {
			continue
		}
		// The journal basis tracks a DISTINCT confirmation series from the
		// trade-ledger basis so neither resets the other across basis switches.
		trackerKey := label
		if r.Basis == driftBasisJournal {
			trackerKey += journalDriftStreakKeySuffix
		}
		// Under the journal basis the exchange total reconciles an unowned
		// position to ~0 (its fill AND uPnL both count), so orphan exposure is not
		// in the total drift — trip on an orphan coin regardless so real unmanaged
		// exposure still alarms (#1107). The trade-ledger basis already trips via
		// the orphan's uPnL drift, so this only widens the journal path.
		orphanExposure := r.Basis == driftBasisJournal && len(r.OrphanCoins) > 0
		if math.Abs(r.Drift) > sharedWalletDriftTolerance || orphanExposure {
			shouldNotify, shouldLog, count := sharedWalletDriftTracker.Record(trackerKey, r.Drift, r.OrphanCoins, now)
			if shouldLog {
				switch {
				case r.Basis == driftBasisJournal && math.Abs(r.Drift) > sharedWalletDriftTolerance:
					// #1100: journal basis — drift is accountValue vs the
					// exchange-sourced expected-equity, not a member-sum diff. An
					// over-tolerance drift takes precedence over the orphan wording so
					// the log never claims the total reconciles when it does not
					// (#1107); any co-occurring orphan is noted as context.
					orphanNote := ""
					if len(r.OrphanCoins) > 0 {
						orphanNote = fmt.Sprintf("; unowned coins=[%s]", strings.Join(r.OrphanCoins, ","))
					}
					fmt.Printf("[WARN] shared-wallet %s JOURNAL drift $%+.2f (accountValue $%.2f vs exchange-sourced expected-equity $%.2f)%s\n",
						label, r.Drift, r.Balance, r.ExpectedEquity, orphanNote)
				case r.Basis == driftBasisJournal:
					// Journal total reconciles (within tolerance) but a position is
					// unowned (#1107) — only reachable with an orphan present.
					fmt.Printf("[WARN] shared-wallet %s JOURNAL orphan exposure: total reconciles (drift $%+.2f, accountValue $%.2f vs expected-equity $%.2f) but unowned coins=[%s]\n",
						label, r.Drift, r.Balance, r.ExpectedEquity, strings.Join(r.OrphanCoins, ","))
				default:
					rawDiff := r.MemberSum - r.Balance
					fmt.Printf("[WARN] shared-wallet %s drift $%+.2f (Σ members $%.2f vs balance $%.2f, rawDiff $%+.2f, orphans=[%s])\n",
						label, r.Drift, r.MemberSum, r.Balance, rawDiff, strings.Join(r.OrphanCoins, ","))
				}
			}
			if !shouldNotify || notifier == nil || !notifier.HasBackends() {
				continue
			}
			var msg string
			switch {
			case r.Basis == driftBasisJournal && math.Abs(r.Drift) > sharedWalletDriftTolerance:
				// A real over-tolerance journal drift (a gap) — report it truthfully
				// and fold any co-occurring orphan in as context. NEVER select the
				// orphan alert (which asserts "within tolerance") when the drift
				// magnitude exceeds the tolerance (#1107).
				msg = formatSharedWalletJournalDriftAlert(r.Key, r.Balance, r.ExpectedEquity, r.Drift, count, r.OrphanCoins)
			case r.Basis == driftBasisJournal:
				// Total reconciles (within tolerance) but a position is unowned
				// (#1107) — only reachable with an orphan present.
				msg = formatSharedWalletJournalOrphanAlert(r.Key, r.Balance, r.ExpectedEquity, r.Drift, count, r.OrphanCoins)
			default:
				msg = formatSharedWalletDriftAlert(r.Key, r.Balance, r.MemberSum, r.Drift, count, r.OrphanCoins)
			}
			notifier.SendToAllChannels(msg)
			notifier.SendOwnerDM(msg)
			continue
		}
		recovered, priorCount := sharedWalletDriftTracker.Clear(trackerKey)
		if !recovered || notifier == nil || !notifier.HasBackends() {
			continue
		}
		msg := formatSharedWalletDriftRecovered(r.Key, priorCount)
		notifier.SendToAllChannels(msg)
		notifier.SendOwnerDM(msg)
	}
}

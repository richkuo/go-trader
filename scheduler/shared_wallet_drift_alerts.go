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

const sharedWalletDriftTolerance = 0.01

const sharedWalletDriftAlertThreshold = 2

const sharedWalletDriftRealertRatio = 0.10

func sharedWalletDriftLogInterval() time.Duration {
	return effectiveAlertThrottleInterval()
}

type sharedWalletDriftEntry struct {
	coinStreaks            map[string]int
	alertedCoins           map[string]bool
	cycles                 int
	lastNotifiedAt         time.Time
	lastLoggedAt           time.Time
	lastLoggedDriftCents   int64
	alerted                bool
	lastNotifiedDriftCents int64
}

type SharedWalletDriftTracker struct {
	mu      sync.Mutex
	entries map[string]*sharedWalletDriftEntry
}

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
		coins = []string{""}
	}
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
	deltaCents := absInt64(driftCents - e.lastNotifiedDriftCents)
	sigChanged := deltaCents > 1 &&
		float64(deltaCents) > sharedWalletDriftRealertRatio*float64(absInt64(e.lastNotifiedDriftCents))

	logDeltaCents := absInt64(driftCents - e.lastLoggedDriftCents)
	logSigChanged := logDeltaCents > 1 &&
		float64(logDeltaCents) > sharedWalletDriftRealertRatio*float64(absInt64(e.lastLoggedDriftCents))
	shouldLog = e.lastLoggedAt.IsZero() ||
		logSigChanged ||
		now.Sub(e.lastLoggedAt) >= sharedWalletDriftLogInterval()

	if maxStreak < sharedWalletDriftAlertThreshold {
		if shouldLog {
			e.lastLoggedAt = now
			e.lastLoggedDriftCents = driftCents
		}
		return false, shouldLog, e.cycles
	}

	switch {
	case confirmedNew:
		shouldNotify = true
	case e.alerted && sigChanged:
		shouldNotify = true
	case !e.lastNotifiedAt.IsZero() && now.Sub(e.lastNotifiedAt) >= effectiveAlertThrottleInterval():
		shouldNotify = true
	}
	if shouldNotify {
		shouldLog = true
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

type SharedWalletDriftView struct {
	Wallet         string    `json:"wallet"`
	Cycles         int       `json:"cycles"`
	Alerted        bool      `json:"alerted"`
	LastNotifiedAt time.Time `json:"last_notified_at,omitzero"`
	OrphanCoins    []string  `json:"orphan_coins,omitempty"`
}

func (t *SharedWalletDriftTracker) Snapshot() []SharedWalletDriftView {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make([]SharedWalletDriftView, 0, len(t.entries))
	for key, e := range t.entries {
		view := SharedWalletDriftView{
			Wallet:         key,
			Cycles:         e.cycles,
			Alerted:        e.alerted,
			LastNotifiedAt: e.lastNotifiedAt,
		}
		for coin := range e.coinStreaks {
			if coin != "" {
				view.OrphanCoins = append(view.OrphanCoins, coin)
			}
		}
		sort.Strings(view.OrphanCoins)
		out = append(out, view)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Wallet < out[j].Wallet })
	return out
}

var sharedWalletDriftTracker = &SharedWalletDriftTracker{}

func absInt64(v int64) int64 {
	if v < 0 {
		return -v
	}
	return v
}

func sharedWalletKeyLabel(key SharedWalletKey) string {
	return fmt.Sprintf("%s/%s", key.Platform, key.Account)
}

func formatSharedWalletDriftAlert(key SharedWalletKey, balance, memberSum, drift float64, count int, orphanCoins []string) string {
	orphanDetail := "no unattributed coins — check member weighting"
	if len(orphanCoins) > 0 {
		orphanDetail = "unattributed coins: " + strings.Join(orphanCoins, ", ")
	}
	rawDiff := memberSum - balance
	return fmt.Sprintf(
		"**SHARED-WALLET DRIFT** %s (pid=%d, %d consecutive): Σ member value $%.2f vs real balance $%.2f — raw reconciliation diff $%+.2f, post-baseline drift $%+.2f (>$%.2f tolerance). %s. Exchange-derived rows should reconcile exactly; a large raw diff with a small post-baseline drift may indicate legacy data baked into the adopted baseline OR a live attribution bug (orphan/weighting) near the offset — investigate before assuming it is benign.",
		sharedWalletKeyLabel(key), os.Getpid(), count, memberSum, balance, rawDiff, drift, sharedWalletDriftTolerance, orphanDetail)
}

func formatSharedWalletJournalDriftAlert(key SharedWalletKey, balance, expectedEquity, drift float64, count int, orphanCoins []string) string {
	orphanNote := ""
	if len(orphanCoins) > 0 {
		orphanNote = fmt.Sprintf(" Additionally, %d on-chain position(s) are owned by NO strategy (%s) — investigate that unmanaged exposure as a possibly-separate issue alongside the journal gap.",
			len(orphanCoins), strings.Join(orphanCoins, ", "))
	}
	return fmt.Sprintf(
		"**SHARED-WALLET DRIFT (exchange journal)** %s (pid=%d, %d consecutive): exchange accountValue $%.2f vs cash-flow-journal expected-equity $%.2f — drift $%+.2f (>$%.2f tolerance). The total is reconstructed from on-chain fills/funding/transfers; a persistent drift means a journal gap (missing/lagging exchange event, or an unmapped balance movement), not a strategy-attribution bug — investigate the exchange event feed before assuming it is benign.%s",
		sharedWalletKeyLabel(key), os.Getpid(), count, balance, expectedEquity, drift, sharedWalletDriftTolerance, orphanNote)
}

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

func reportSharedWalletDrift(notifier *MultiNotifier, results []sharedWalletDriftResult) {
	now := time.Now().UTC()
	for _, r := range results {
		label := sharedWalletKeyLabel(r.Key)
		if r.JournalPending {
			continue
		}
		trackerKey := label
		if r.Basis == driftBasisJournal {
			trackerKey += journalDriftStreakKeySuffix
			if recovered, priorCount := sharedWalletDriftTracker.Clear(label); recovered && notifier != nil && notifier.HasBackends() {
				msg := formatSharedWalletDriftRecovered(r.Key, priorCount)
				notifier.SendToAllChannels(msg)
				notifier.SendOwnerDM(msg)
			}
		}
		orphanExposure := r.Basis == driftBasisJournal && len(r.OrphanCoins) > 0
		if math.Abs(r.Drift) > sharedWalletDriftTolerance || orphanExposure {
			recordCoins := r.OrphanCoins
			if r.Basis == driftBasisJournal && math.Abs(r.Drift) > sharedWalletDriftTolerance {
				recordCoins = append(append([]string{}, r.OrphanCoins...), "")
			}
			shouldNotify, shouldLog, count := sharedWalletDriftTracker.Record(trackerKey, r.Drift, recordCoins, now)
			if shouldLog {
				switch {
				case r.Basis == driftBasisJournal && math.Abs(r.Drift) > sharedWalletDriftTolerance:
					orphanNote := ""
					if len(r.OrphanCoins) > 0 {
						orphanNote = fmt.Sprintf("; unowned coins=[%s]", strings.Join(r.OrphanCoins, ","))
					}
					fmt.Printf("[WARN] shared-wallet %s JOURNAL drift $%+.2f (accountValue $%.2f vs exchange-sourced expected-equity $%.2f)%s\n",
						label, r.Drift, r.Balance, r.ExpectedEquity, orphanNote)
				case r.Basis == driftBasisJournal:
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
				msg = formatSharedWalletJournalDriftAlert(r.Key, r.Balance, r.ExpectedEquity, r.Drift, count, r.OrphanCoins)
			case r.Basis == driftBasisJournal:
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

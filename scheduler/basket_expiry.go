package main

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"
)

const (
	basketExpiryRetryFloor    = time.Minute
	basketExpiryRetryCap      = 30 * time.Minute
	basketExpiryEscalateAfter = 3
)

func basketExpiryBackoff(failures int) time.Duration {
	d := basketExpiryRetryFloor
	for i := 1; i < failures && d < basketExpiryRetryCap; i++ {
		d *= 2
	}
	if d > basketExpiryRetryCap {
		d = basketExpiryRetryCap
	}
	return d
}

func (bc *basketCoordinator) memberPosition(m basketContractMember) *Position {
	s := bc.state.Strategies[m.StrategyID]
	if s == nil {
		return nil
	}
	pos := s.Positions[m.Coin]
	if pos == nil || pos.Quantity <= 0 {
		return nil
	}
	return pos
}

func (bc *basketCoordinator) expiryObligationOpen(m basketContractMember, l basketLifecycle, nowMs int64) bool {
	if !l.active() || l.ExpiryMs <= 0 || nowMs < l.ExpiryMs {
		return false
	}
	pos := bc.memberPosition(m)
	return pos != nil && pos.TradePositionID == l.PositionID
}

func (bc *basketCoordinator) expirySuppressionDeadline(rt *basketRuntime, now time.Time) int64 {
	cover := evaluationDeadline(now, rt.Contract.RebalanceIntervalSeconds).UnixMilli()
	if latest := rt.latest(); latest != nil && latest.DeadlineMs > cover {
		cover = latest.DeadlineMs
	}
	if rt.rank != nil && rt.rank.DeadlineMs > cover {
		cover = rt.rank.DeadlineMs
	}
	return cover
}

func (bc *basketCoordinator) deferExpiry(rt *basketRuntime, ids []string, until time.Time) {
	ms := until.UnixMilli()
	for _, id := range ids {
		l := rt.Lifecycle[id]
		if l.active() && l.RetryAtMs < ms {
			l.RetryAtMs = ms
			rt.Lifecycle[id] = l
		}
	}
}

func (bc *basketCoordinator) emit(rt *basketRuntime, msg string, critical bool) {
	label := "WARN"
	if critical {
		label = "CRITICAL"
	}
	text := fmt.Sprintf("[%s] basket %s (%s): %s", label, rt.Contract.BasketID, partitionLabel(rt.Partition), msg)
	fmt.Println(text)
	if bc.notifier == nil {
		return
	}
	notifier, partition := bc.notifier, rt.Partition
	bc.outbox = append(bc.outbox, func() {
		if notifier.HasOwner() {
			notifier.SendOwnerDM(text)
		}
		if critical && notifier.HasBackends() {
			notifier.SendToPartitionChannels(partition, text)
		}
	})
}

func (bc *basketCoordinator) lifecycleAlert(rt *basketRuntime, ids []string, positionIDs map[string]string, key, msg string, critical bool) {
	tracked := false
	for _, id := range ids {
		l := rt.Lifecycle[id]
		if !l.active() || (positionIDs != nil && positionIDs[id] != "" && positionIDs[id] != l.PositionID) {
			continue
		}
		tracked = true
		if l.alerted(key) {
			return
		}
	}
	if !tracked {
		bc.alert(rt, nil, key, msg, critical)
		return
	}
	for _, id := range ids {
		l := rt.Lifecycle[id]
		if !l.active() || (positionIDs != nil && positionIDs[id] != "" && positionIDs[id] != l.PositionID) {
			continue
		}
		l.markAlerted(key)
		rt.Lifecycle[id] = l
	}
	bc.emit(rt, msg, critical)
}

func (bc *basketCoordinator) actionAlert(rt *basketRuntime, rb *basketRebalance, a *basketAction, key, msg string, critical bool) {
	if a.expiry() {
		bc.lifecycleAlert(rt, []string{a.StrategyID}, map[string]string{a.StrategyID: a.LifecyclePositionID}, key, msg, critical)
		return
	}
	bc.alert(rt, rb, key, msg, critical)
}

func (bc *basketCoordinator) expiryFailed(rt *basketRuntime, a *basketAction, now time.Time, reason string) {
	l := rt.Lifecycle[a.StrategyID]
	if !l.active() || l.PositionID != a.LifecyclePositionID {
		bc.logf(rt, "expiry action %s for %s: %s; its lifecycle already settled", a.ActionID, a.StrategyID, reason)
		return
	}
	l.Failures++
	retry := now.Add(basketExpiryBackoff(l.Failures))
	if l.RetryAtMs < retry.UnixMilli() {
		l.RetryAtMs = retry.UnixMilli()
	}
	rt.Lifecycle[a.StrategyID] = l
	critical := l.Failures >= basketExpiryEscalateAfter
	key := "expiry-failed|" + l.PositionID
	if critical {
		key = "expiry-failed-critical|" + l.PositionID
	}
	bc.lifecycleAlert(rt, []string{a.StrategyID}, map[string]string{a.StrategyID: l.PositionID}, key,
		fmt.Sprintf("%s holding-horizon close attempt %d (action %s, client %s) did not fill: %s; the close obligation stays open, increases hold, protection stays active, and the next attempt waits until %s",
			a.StrategyID, a.LifecycleAttempt, a.ActionID, a.ClientOrderID, reason, retry.UTC().Format(time.RFC3339)), critical)
}

func (bc *basketCoordinator) advanceExpirySuppression(rt *basketRuntime, now time.Time) ([]basketContractMember, bool) {
	nowMs := now.UnixMilli()
	var expired []basketContractMember
	for _, m := range rt.Contract.Members {
		l := rt.Lifecycle[m.StrategyID]
		if l.active() && l.ExpiryMs > 0 && nowMs >= l.ExpiryMs {
			expired = append(expired, m)
		}
	}
	if len(expired) == 0 {
		return nil, false
	}
	cover := bc.expirySuppressionDeadline(rt, now)
	changed := false
	for _, m := range expired {
		l := rt.Lifecycle[m.StrategyID]
		if l.SuppressThroughMs < cover {
			l.SuppressThroughMs = cover
			changed = true
		}
		if l.State != basketExpiryDue {
			l.State = basketExpiryDue
			changed = true
			bc.logf(rt, "%s %s position %s reached its holding horizon at %s (entered %s); it closes before any new increase, and deadlines through %s cannot reopen it",
				m.StrategyID, m.Coin, l.PositionID, formatClosedBarMs(l.ExpiryMs), formatClosedBarMs(l.EntryMs), formatClosedBarMs(l.SuppressThroughMs))
		}
		rt.Lifecycle[m.StrategyID] = l
	}
	return expired, changed
}

func (bc *basketCoordinator) processExpiry(rt *basketRuntime, in basketCycleInputs) bool {
	rt.expiryHold = ""
	rt.expiryOpen = nil
	now := in.Now
	nowMs := now.UnixMilli()
	expired, changed := bc.advanceExpirySuppression(rt, now)
	if len(expired) == 0 {
		return true
	}
	var due []basketContractMember
	var dueIDs []string
	for _, m := range expired {
		if bc.expiryObligationOpen(m, rt.Lifecycle[m.StrategyID], nowMs) {
			due = append(due, m)
			dueIDs = append(dueIDs, m.StrategyID)
		}
	}
	if len(due) > 0 {
		rt.expiryOpen = make(map[string]bool, len(due))
		for _, id := range dueIDs {
			rt.expiryOpen[id] = true
		}
		rt.expiryHold = "a holding-horizon close is open for " + strings.Join(dueIDs, ", ")
	}
	if changed && !bc.persistProgress(rt) {
		rt.expiryHold = "the holding-horizon suppression could not be saved"
		return false
	}
	if len(due) == 0 {
		return true
	}
	if bc.state.partitionLatched(rt.Partition) {
		bc.logf(rt, "the %s kill switch is latched; it owns flattening, so the holding-horizon close for %s waits and increases hold", partitionLabel(rt.Partition), strings.Join(dueIDs, ", "))
		bc.deferExpiry(rt, dueIDs, now.Add(basketExpiryRetryFloor))
		bc.persistProgress(rt)
		return true
	}
	bounds := basketBoundsFor(rt.Contract)
	for n := 0; n < basketMaxActionsPerStep; n++ {
		if rt.pendingAction() != nil || rt.needsDurable {
			return true
		}
		nowMs = now.UnixMilli()
		var ready []basketContractMember
		var readyIDs []string
		for _, m := range rt.Contract.Members {
			l := rt.Lifecycle[m.StrategyID]
			if !bc.expiryObligationOpen(m, l, nowMs) || l.RetryAtMs > nowMs {
				continue
			}
			ready = append(ready, m)
			readyIDs = append(readyIDs, m.StrategyID)
		}
		if len(ready) == 0 {
			return true
		}
		marks, missing := bc.marks(rt, in)
		exposure := bc.exposureLocked(rt, marks)
		if unpriced := basketUnpricedHeld(exposure, missing); len(unpriced) > 0 {
			retry := now.Add(basketExpiryRetryFloor)
			if in.MarksRetryAt.After(now) {
				retry = in.MarksRetryAt
			}
			bc.deferExpiry(rt, readyIDs, retry)
			msg := fmt.Sprintf("the holding-horizon close for %s holds: no valid sealed mark for %s, so the basket bounds cannot be checked; increases hold, protection stays active, and the close retries at %s with no private price fallback",
				strings.Join(readyIDs, ", "), strings.Join(unpriced, ", "), retry.UTC().Format(time.RFC3339))
			bc.logf(rt, "%s", msg)
			bc.lifecycleAlert(rt, readyIDs, nil, "expiry-marks|"+strings.Join(unpriced, ","), msg, false)
			bc.persistProgress(rt)
			return true
		}
		floor := func(coin string, qty float64) float64 { return bc.lotFloor(coin, qty, marks[coin]) }
		acted := false
		var reasons, lotReasons, lotCoins []string
		for _, c := range basketOrderExpiryReductions(basketExpiryCandidates(ready, exposure), exposure) {
			tr := basketLegalTranche(exposure, c, bounds, false, floor)
			if !(tr.Qty > 0) {
				if lot := bc.lotPeek(c.Coin); !lot.Known {
					lotCoins = append(lotCoins, c.Coin)
					lotReasons = append(lotReasons, fmt.Sprintf("%s (%s lot size unavailable: %s)", tr.Detail, c.Coin, lot.Reason))
					continue
				}
				reasons = append(reasons, tr.Detail)
				continue
			}
			if !bc.runExpiryAction(rt, c, tr.Qty, marks[c.Coin], in) {
				return true
			}
			acted = true
			break
		}
		if acted {
			continue
		}
		retry := now.Add(basketExpiryRetryFloor)
		bc.deferExpiry(rt, readyIDs, retry)
		if len(reasons) == 0 && len(lotReasons) > 0 {
			sort.Strings(lotCoins)
			msg := fmt.Sprintf("the holding-horizon close for %s holds: the full close is outside the basket bounds and no partial tranche can be sized without the venue lot size (%s); the close obligation stays open, increases hold, protection stays active and it retries at %s",
				strings.Join(readyIDs, ", "), strings.Join(lotReasons, "; "), retry.UTC().Format(time.RFC3339))
			bc.logf(rt, "%s", msg)
			bc.lifecycleAlert(rt, readyIDs, nil, "expiry-lots|"+strings.Join(lotCoins, ","), msg, false)
			bc.persistProgress(rt)
			return true
		}
		reasons = append(reasons, lotReasons...)
		detail := strings.Join(reasons, "; ")
		msg := fmt.Sprintf("the holding-horizon close for %s is blocked by the basket bounds (%s); the close obligation stays open, increases hold, protection stays active and no bound is bypassed; it retries at %s",
			strings.Join(readyIDs, ", "), detail, retry.UTC().Format(time.RFC3339))
		bc.logf(rt, "%s", msg)
		bc.lifecycleAlert(rt, readyIDs, nil, "expiry-blocked", msg, true)
		bc.persistProgress(rt)
		return true
	}
	return true
}

func (bc *basketCoordinator) stepExpiry(in basketCycleInputs) {
	if !bc.active() {
		return
	}
	bc.ensureMemberLots()
	bc.mu.Lock()
	for _, id := range bc.sortedIDs() {
		bc.stepExpiryRuntime(bc.runtimes[id], in)
	}
	bc.mu.Unlock()
	bc.flushOutbox()
}

func (bc *basketCoordinator) stepExpiryRuntime(rt *basketRuntime, in basketCycleInputs) {
	rt.clock = in.Now
	_, changed := bc.advanceExpirySuppression(rt, in.Now)
	if in.SaveBlocked || rt.needsDurable || bc.memberLiveRefusal(rt) != "" {
		return
	}
	if changed && !bc.persistProgress(rt) {
		return
	}
	if a := rt.pendingAction(); a != nil {
		if !bc.reconcileAction(rt, a, in) {
			return
		}
	}
	bc.processExpiry(rt, in)
}

func (bc *basketCoordinator) runExpiryAction(rt *basketRuntime, c basketCandidate, qty, mark float64, in basketCycleInputs) bool {
	sc, ok := bc.members[c.StrategyID]
	if !ok {
		return false
	}
	l := rt.Lifecycle[c.StrategyID]
	ids := []string{c.StrategyID}
	if msg := basketLiveRefusal(sc); msg != "" || isLiveArgs(sc.Args) {
		bc.lifecycleAlert(rt, ids, nil, "live-refused", "live basket dispatch is refused; no holding-horizon close is sent", true)
		bc.deferExpiry(rt, ids, in.Now.Add(basketExpiryRetryFloor))
		return false
	}
	venue, err := bc.venueFor(sc)
	if err != nil {
		bc.lifecycleAlert(rt, ids, nil, "venue|"+err.Error(), err.Error(), true)
		bc.deferExpiry(rt, ids, in.Now.Add(basketExpiryRetryFloor))
		return false
	}
	cloid, err := venue.NewClientOrderID()
	if err != nil {
		bc.logf(rt, "could not generate a client order id for the %s holding-horizon close: %v", c.StrategyID, err)
		bc.deferExpiry(rt, ids, in.Now.Add(basketExpiryRetryFloor))
		return false
	}
	now := in.Now
	s := bc.state.Strategies[sc.ID]
	if s == nil {
		return false
	}
	pre := basketPositionSigned(s, c.Coin)
	var deadline int64
	if rb := rt.latest(); rb != nil {
		deadline = rb.DeadlineMs
	}
	attempt := l.Attempt + 1
	a := &basketAction{
		Partition:           rt.Partition,
		BasketID:            rt.Contract.BasketID,
		DeadlineMs:          deadline,
		StrategyID:          sc.ID,
		Symbol:              c.Coin,
		Direction:           basketDirectionReduce,
		Seq:                 rt.nextSeq(deadline, sc.ID, basketDirectionReduce),
		OrderSide:           c.OrderSide,
		PositionSide:        c.PositionSide,
		Status:              basketActionReserved,
		RequestedQty:        qty,
		PreQty:              pre,
		PreNotionalUSD:      pre * mark,
		MarkPx:              mark,
		ProtectionJSON:      basketProtectionSnapshot(s, c.Coin, basketMemberObservation{}),
		Provider:            venue.Provider(),
		AccountKey:          venue.AccountKey(rt.Partition),
		ClientOrderID:       cloid,
		CreatedAtMs:         now.UnixMilli(),
		UpdatedAtMs:         now.UnixMilli(),
		Purpose:             basketPurposeExpiry,
		LifecyclePositionID: l.PositionID,
		ExpiryMs:            l.ExpiryMs,
		LifecycleAttempt:    attempt,
	}
	a.ActionID = basketExpiryActionID(a.Partition, a.BasketID, a.StrategyID, a.LifecyclePositionID, a.ExpiryMs, a.LifecycleAttempt)
	if err := bc.store.ReserveBasketAction(rt, a); err != nil {
		bc.logf(rt, "%s holding-horizon close %.10g held: the action could not be reserved before sending: %v", sc.ID, qty, err)
		bc.deferExpiry(rt, ids, now.Add(basketExpiryRetryFloor))
		bc.persistProgress(rt)
		return false
	}
	rt.Actions[a.ActionID] = a
	attempt = a.LifecycleAttempt
	l.Attempt = attempt
	l.State = basketExpiryDue
	rt.Lifecycle[sc.ID] = l
	a.Status = basketActionSubmitting
	a.UpdatedAtMs = now.UnixMilli()
	if !bc.persistProgress(rt) {
		a.Status = basketActionNoFill
		a.HoldReason = "the submission could not be recorded, so nothing was sent"
		bc.persistProgress(rt)
		bc.deferExpiry(rt, ids, now.Add(basketExpiryRetryFloor))
		return false
	}
	bc.logf(rt, "%s holding-horizon close %s %s %.10g @ mark $%.4f (position %s, attempt %d, action %s, client %s)", sc.ID, c.OrderSide, c.Coin, qty, mark, l.PositionID, attempt, a.ActionID, a.ClientOrderID)
	var outcome basketVenueOutcome
	submitted := *a
	bc.unlocked(func() { outcome, err = venue.Submit(&submitted) })
	if err != nil {
		outcome = basketVenueOutcome{Outcome: basketVenueUnknown, Detail: "transport failure: " + err.Error()}
	}
	return bc.applyOutcome(rt, nil, a, outcome, basketMemberObservation{}, in, false)
}

func (bc *basketCoordinator) expiryWakeLocked(rt *basketRuntime, now time.Time) time.Duration {
	best := time.Duration(-1)
	nowMs := now.UnixMilli()
	for _, m := range rt.Contract.Members {
		l := rt.Lifecycle[m.StrategyID]
		if !l.active() || l.ExpiryMs <= 0 {
			continue
		}
		var d time.Duration
		if nowMs < l.ExpiryMs {
			d = time.Duration(l.ExpiryMs-nowMs) * time.Millisecond
		} else {
			if !bc.expiryObligationOpen(m, l, nowMs) {
				continue
			}
			at := time.UnixMilli(l.RetryAtMs)
			if rt.wakeRetryAt.After(at) {
				at = rt.wakeRetryAt
			}
			d = at.Sub(now)
			if d < 0 {
				d = 0
			}
		}
		if best < 0 || d < best {
			best = d
		}
	}
	return best
}

func (bc *basketCoordinator) expiryWork(now time.Time) bool {
	if !bc.active() {
		return false
	}
	bc.mu.RLock()
	defer bc.mu.RUnlock()
	for _, id := range bc.sortedIDs() {
		if bc.expiryWakeLocked(bc.runtimes[id], now) == 0 {
			return true
		}
	}
	return false
}

func (bc *basketCoordinator) expiryMarkCoins(now time.Time) []string {
	if !bc.active() {
		return nil
	}
	bc.mu.RLock()
	defer bc.mu.RUnlock()
	coins := make(map[string]bool)
	nowMs := now.UnixMilli()
	for _, id := range bc.sortedIDs() {
		rt := bc.runtimes[id]
		open := false
		for _, m := range rt.Contract.Members {
			if bc.expiryObligationOpen(m, rt.Lifecycle[m.StrategyID], nowMs) {
				open = true
			}
		}
		if !open {
			continue
		}
		for _, m := range rt.Contract.Members {
			if bc.memberPosition(m) != nil {
				coins[m.Coin] = true
			}
		}
	}
	out := make([]string, 0, len(coins))
	for c := range coins {
		out = append(out, c)
	}
	sort.Strings(out)
	return out
}

func (bc *basketCoordinator) effectiveTargets(rt *basketRuntime, rb *basketRebalance, targets *basketTargets) (*basketTargets, []string) {
	if targets == nil || rb == nil {
		return targets, nil
	}
	var eff *basketTargets
	var held []string
	for _, m := range rt.Contract.Members {
		l := rt.Lifecycle[m.StrategyID]
		if l.SuppressThroughMs <= 0 || rb.DeadlineMs > l.SuppressThroughMs {
			continue
		}
		mt, ok := targets.Members[m.StrategyID]
		if !ok || mt.TargetQty == 0 {
			continue
		}
		if eff == nil {
			cp := *targets
			cp.Members = make(map[string]basketMemberTarget, len(targets.Members))
			for k, v := range targets.Members {
				cp.Members[k] = v
			}
			eff = &cp
		}
		mt.TargetQty = 0
		eff.Members[m.StrategyID] = mt
		held = append(held, m.StrategyID)
	}
	if eff == nil {
		return targets, nil
	}
	return eff, held
}

type basketExpiryFeed struct {
	client  *sharedFeedClient
	sched   *sharedFeedSchedule
	lastKey int64
}

type basketExpiryMarks struct {
	Snapshot *marketSnapshot
	RetryAt  time.Time
	Report   *sharedFeedFetchReport
	Detail   string
}

func (f *basketExpiryFeed) nextRetry(now time.Time) time.Time {
	if next, ok := f.sched.nextKey(now); ok {
		return next
	}
	return now.Add(basketExpiryRetryFloor)
}

func (f *basketExpiryFeed) marks(ctx context.Context, now time.Time, coins []string, cycle *marketSnapshot, cycleKey int64) basketExpiryMarks {
	if len(coins) == 0 || snapshotHasFreshMids(cycle, coins) {
		return basketExpiryMarks{Snapshot: cycle}
	}
	if f == nil || f.client == nil || f.sched == nil {
		return basketExpiryMarks{RetryAt: now.Add(basketExpiryRetryFloor), Detail: "no shared feed client"}
	}
	if cycleKey > f.lastKey {
		f.lastKey = cycleKey
	}
	var after time.Time
	if f.lastKey > 0 {
		after = time.Unix(f.lastKey, 0).UTC()
	}
	key, ok := f.sched.latestFreshKey(now, after)
	if !ok {
		retry := f.nextRetry(now)
		return basketExpiryMarks{RetryAt: retry, Detail: fmt.Sprintf("no fresh supported feed key; the next key is %s", retry.UTC().Format(time.RFC3339))}
	}
	f.lastKey = key.Unix()
	snap, report := f.client.Fetch(ctx, key.Unix(), cycleMarketRequirements{Coins: append([]string(nil), coins...)})
	out := basketExpiryMarks{Report: &report}
	if report.Status != feedFetchSealed || !snapshotHasFreshMids(snap, coins) {
		out.RetryAt = f.nextRetry(now)
		out.Detail = fmt.Sprintf("key %d (%s) has no fresh sealed mark for every held member", key.Unix(), report.Status)
		return out
	}
	out.Snapshot = snap
	return out
}

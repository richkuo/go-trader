package main

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"reflect"
	"sort"
	"strings"
	"sync"
	"time"
)

const basketMaxActionsPerStep = 64

const basketHeldWakeRetry = time.Minute

type basketMemberObservation struct {
	Cycle      int
	AtMs       int64
	Fields     StrategyDecisionFields
	Indicators map[string]interface{}
	Price      float64
}

type basketCycleInputs struct {
	Now         time.Time
	Snapshot    *marketSnapshot
	SnapshotFor func(rt *basketRuntime, deadline time.Time) *marketSnapshot
	ScopeRisk   map[RiskPartition]*scopeCycleRisk

	PairSnapshotFor func(rt *basketRuntime, at time.Time) *marketSnapshot
	PairGeneration  func() uint64

	SaveBlocked bool
	Once        bool

	MarksRetryAt time.Time
}

type basketCoordinator struct {
	cfg          *Config
	state        *AppState
	store        *StateStore
	mu           *sync.RWMutex
	notifier     *MultiNotifier
	logMgr       *LogManager
	runtimes     map[string]*basketRuntime
	byMember     map[string]*basketRuntime
	members      map[string]StrategyConfig
	parent       context.Context
	rankRun      basketRankRunner
	rankClock    basketRankClock
	targetPolicy basketTargetPolicy
	venueFor     func(StrategyConfig) (basketVenue, error)
	lots         *hlLotMetadataCache
	cycle        int
	critical     map[string]bool
	outbox       []func()

	pairPolicyRun pairPolicyRunner
	pairClock     func() time.Time
}

func newBasketCoordinator(cfg *Config, state *AppState, store *StateStore, mu *sync.RWMutex, notifier *MultiNotifier, logMgr *LogManager, runtimes map[string]*basketRuntime) *basketCoordinator {
	bc := &basketCoordinator{
		cfg:      cfg,
		state:    state,
		store:    store,
		mu:       mu,
		notifier: notifier,
		logMgr:   logMgr,
		runtimes: runtimes,
		byMember: make(map[string]*basketRuntime),
		members:  make(map[string]StrategyConfig),
		parent:   shutdownReadOnlyCtx,
		rankRun:  basketRankRunFn,
		venueFor: basketVenueFor,
		lots:     hlLotMetadata,
		critical: make(map[string]bool),

		pairPolicyRun: pairPolicyRunFn,
		pairClock:     tradingNow,
	}
	if bc.parent == nil {
		bc.parent = context.Background()
	}
	if cfg != nil {
		for _, sc := range cfg.Strategies {
			if rt, ok := runtimes[sc.BasketID]; ok && isBasketMember(sc) && !rt.isPair() {
				bc.byMember[sc.ID] = rt
				bc.members[sc.ID] = sc
			}
			if rt, ok := runtimes[sc.PairID]; ok && isPairMember(sc) && rt.isPair() {
				bc.byMember[sc.ID] = rt
				bc.members[sc.ID] = sc
			}
		}
	}
	if state != nil && len(bc.sortedPairIDs()) > 0 {
		state.pairViews = bc
	}
	return bc
}

func (bc *basketCoordinator) active() bool {
	return bc != nil && len(bc.runtimes) > 0
}

func (bc *basketCoordinator) sortedIDs() []string {
	ids := make([]string, 0, len(bc.runtimes))
	for id, rt := range bc.runtimes {
		if !rt.isPair() {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	return ids
}

func (bc *basketCoordinator) sortedPairIDs() []string {
	ids := make([]string, 0, len(bc.runtimes))
	for id, rt := range bc.runtimes {
		if rt.isPair() {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	return ids
}

func (bc *basketCoordinator) logf(rt *basketRuntime, format string, a ...interface{}) {
	fmt.Printf("[basket %s] %s\n", rt.Contract.BasketID, fmt.Sprintf(format, a...))
}

func (bc *basketCoordinator) memberLogger(id string) *StrategyLogger {
	if bc.logMgr != nil {
		if l, err := bc.logMgr.GetStrategyLogger(id); err == nil && l != nil {
			return l
		}
	}
	return &StrategyLogger{stratID: id, writer: os.Stdout}
}

func (bc *basketCoordinator) alert(rt *basketRuntime, rb *basketRebalance, key, msg string, critical bool) {
	if rb != nil {
		if rb.Outcome.alerted(key) {
			return
		}
		rb.Outcome.markAlerted(key)
	} else {
		if bc.critical[rt.Contract.BasketID+"|"+key] {
			return
		}
		bc.critical[rt.Contract.BasketID+"|"+key] = true
	}
	bc.emit(rt, msg, critical)
}

func (bc *basketCoordinator) unlocked(fn func()) {
	bc.mu.Unlock()
	defer bc.mu.Lock()
	fn()
}

func (bc *basketCoordinator) flushOutbox() {
	out := bc.outbox
	bc.outbox = nil
	for _, send := range out {
		send()
	}
}

func basketContractDiff(saved, current basketContract) []string {
	var out []string
	sv, cv := reflect.ValueOf(saved), reflect.ValueOf(current)
	st := sv.Type()
	for i := 0; i < st.NumField(); i++ {
		if !reflect.DeepEqual(sv.Field(i).Interface(), cv.Field(i).Interface()) {
			name := strings.SplitN(st.Field(i).Tag.Get("json"), ",", 2)[0]
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}

func (bc *basketCoordinator) startupCheck() error {
	if !bc.active() {
		return nil
	}
	bc.mu.RLock()
	defer bc.mu.RUnlock()
	var errs []string
	for _, id := range bc.sortedIDs() {
		rt := bc.runtimes[id]
		coins := make(map[string]bool)
		for _, m := range rt.Contract.Members {
			coins[m.Coin] = true
		}
		open := false
		for _, m := range rt.Contract.Members {
			s := bc.state.Strategies[m.StrategyID]
			if s == nil {
				continue
			}
			for sym, pos := range s.Positions {
				if pos == nil || pos.Quantity <= 0 {
					continue
				}
				if sym != m.Coin {
					errs = append(errs, fmt.Sprintf("basket %s member %s holds a %s position outside its coin %s", id, m.StrategyID, sym, m.Coin))
					continue
				}
				open = true
			}
			if len(s.OptionPositions) > 0 {
				errs = append(errs, fmt.Sprintf("basket %s member %s holds option positions", id, m.StrategyID))
			}
		}
		expectedOpen := false
		for _, q := range rt.Expected {
			if q != 0 {
				expectedOpen = true
			}
		}
		pending := rt.unresolvedRecovery()
		if rt.SavedContract != nil && rt.SavedFP != rt.Contract.Fingerprint() && (pending || open || expectedOpen) {
			errs = append(errs, fmt.Sprintf("basket %s contract changed (%s) while a saved rebalance or a member position is open; restore the saved contract or flatten and resolve the basket first", id, strings.Join(basketContractDiff(*rt.SavedContract, rt.Contract), ", ")))
		}
		if rt.SavedContract == nil && open {
			errs = append(errs, fmt.Sprintf("basket %s has no saved contract, but a member already holds a position; flatten the members before they join a basket", id))
		}
		for _, sc := range bc.cfg.Strategies {
			if isBasketMember(sc) && sc.BasketID == id {
				continue
			}
			if partitionFor(sc) != rt.Partition {
				continue
			}
			s := bc.state.Strategies[sc.ID]
			if s == nil {
				continue
			}
			syms := make([]string, 0, len(s.Positions))
			for sym := range s.Positions {
				syms = append(syms, sym)
			}
			sort.Strings(syms)
			for _, sym := range syms {
				pos := s.Positions[sym]
				asset := strings.TrimSuffix(strings.ToUpper(sym), "/USDT")
				if pos != nil && pos.Quantity > 0 && coins[asset] {
					errs = append(errs, fmt.Sprintf("strategy %s holds %s in the %s partition, which basket %s owns exclusively", sc.ID, sym, partitionLabel(rt.Partition), id))
				}
			}
		}
	}
	for _, id := range bc.sortedPairIDs() {
		errs = append(errs, bc.pairStartupErrors(bc.runtimes[id])...)
	}
	if len(errs) > 0 {
		sort.Strings(errs)
		return fmt.Errorf("basket startup check refused:\n  %s", strings.Join(errs, "\n  "))
	}
	return nil
}

func (bc *basketCoordinator) recoverInterrupted(now time.Time) {
	if !bc.active() {
		return
	}
	bc.mu.Lock()
	defer bc.mu.Unlock()
	for _, id := range bc.sortedIDs() {
		rt := bc.runtimes[id]
		changed := false
		for _, d := range rt.sortedDeadlines() {
			rb := rt.Rebalances[d]
			if rb.Status == basketRebalanceReserved || rb.Status == basketRebalanceRanking {
				rb.Status = basketRebalanceHeld
				rb.HoldReason = "the scheduler restarted before targets were frozen; a deadline is never reranked"
				rb.Outcome.Result = "held"
				rb.UpdatedAtMs = now.UnixMilli()
				changed = true
				bc.logf(rt, "deadline %s: %s", formatClosedBarMs(d), rb.HoldReason)
			}
		}
		if changed {
			if err := bc.store.PersistBasketProgress(rt); err != nil {
				bc.logf(rt, "could not record the interrupted deadline: %v", err)
			}
		}
	}
	for _, id := range bc.sortedPairIDs() {
		bc.recoverInterruptedPair(bc.runtimes[id], now)
	}
}

func (bc *basketCoordinator) observe(sc StrategyConfig, result *HyperliquidResult, price float64) {
	if bc == nil || result == nil {
		return
	}
	rt := bc.byMember[sc.ID]
	if rt == nil || rt.isPair() {
		return
	}
	rt.observations[sc.ID] = basketMemberObservation{
		Cycle:      bc.cycle,
		Fields:     result.StrategyDecisionFields,
		Indicators: result.Indicators,
		Price:      price,
	}
}

func (bc *basketCoordinator) dueDeadlines(now time.Time) map[string]time.Time {
	if !bc.active() {
		return make(map[string]time.Time)
	}
	bc.mu.RLock()
	defer bc.mu.RUnlock()
	return bc.dueDeadlinesLocked(now)
}

func (bc *basketCoordinator) dueDeadlinesLocked(now time.Time) map[string]time.Time {
	out := make(map[string]time.Time)
	for _, id := range bc.sortedIDs() {
		rt := bc.runtimes[id]
		d := evaluationDeadline(now, rt.Contract.RebalanceIntervalSeconds)
		if _, ok := rt.Rebalances[d.UnixMilli()]; ok {
			continue
		}
		if latest := rt.latest(); latest != nil && latest.DeadlineMs >= d.UnixMilli() {
			continue
		}
		if now.Sub(d) <= time.Duration(rt.Contract.EvaluationWindowSeconds)*time.Second {
			out[id] = d
		}
	}
	return out
}

func (bc *basketCoordinator) wakeDelay(now time.Time) time.Duration {
	if !bc.active() {
		return -1
	}
	bc.mu.RLock()
	defer bc.mu.RUnlock()
	best := time.Duration(-1)
	consider := func(d time.Duration) {
		if d < 0 {
			d = 0
		}
		if best < 0 || d < best {
			best = d
		}
	}
	due := bc.dueDeadlinesLocked(now)
	for _, id := range bc.sortedIDs() {
		rt := bc.runtimes[id]
		if _, ok := due[id]; ok {
			if !rt.wakeRetryAt.After(now) {
				return 0
			}
			consider(rt.wakeRetryAt.Sub(now))
		}
		interval := time.Duration(rt.Contract.RebalanceIntervalSeconds) * time.Second
		next := evaluationDeadline(now, rt.Contract.RebalanceIntervalSeconds).Add(interval)
		consider(next.Sub(now))
		if rt.rank != nil {
			consider(time.Second)
		}
		if d := bc.expiryWakeLocked(rt, now); d >= 0 {
			consider(d)
		}
	}
	if d := bc.pairWakeDelayLocked(now); d >= 0 {
		consider(d)
	}
	return best
}

func (bc *basketCoordinator) cycleRequirements(due map[string]time.Time, req feedRequirements) cycleMarketRequirements {
	var out cycleMarketRequirements
	need := make(map[marketFeedKey]int)
	coins := make(map[string]bool)
	ids := make([]string, 0, len(due))
	for id := range due {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		rt := bc.runtimes[id]
		for _, m := range rt.Contract.Members {
			coins[m.Coin] = true
			if basketListingExclusion(rt.Contract, m, due[id].UnixMilli()) != "" {
				continue
			}
			k := feedKeyFor(m.Coin, m.Timeframe)
			if l := basketFrameLookback(rt.Contract.LookbackBars); l > need[k] {
				need[k] = l
			}
		}
	}
	for k, l := range need {
		out.Keys = append(out.Keys, cycleMarketRequirement{Key: k, Required: l})
	}
	sort.Slice(out.Keys, func(i, j int) bool { return marketFeedKeyLess(out.Keys[i].Key, out.Keys[j].Key) })
	for c := range coins {
		out.Coins = append(out.Coins, c)
	}
	sort.Strings(out.Coins)
	out.AccountingCoins = append(out.AccountingCoins, req.AccountingCoins...)
	return out
}

func mergeCycleRequirements(a, b cycleMarketRequirements) cycleMarketRequirements {
	need := make(map[marketFeedKey]int)
	for _, cr := range a.Keys {
		need[cr.Key] = cr.Required
	}
	for _, cr := range b.Keys {
		if cr.Required > need[cr.Key] {
			need[cr.Key] = cr.Required
		}
	}
	out := cycleMarketRequirements{Funding: make(map[string]feedFundingNeed), Observations: make(map[feedObservationKey]int64), Macro: make(map[feedMacroKey]int64),
		Books: make(map[feedObservationKey]feedBookNeed)}
	for k, l := range need {
		out.Keys = append(out.Keys, cycleMarketRequirement{Key: k, Required: l})
	}
	sort.Slice(out.Keys, func(i, j int) bool { return marketFeedKeyLess(out.Keys[i].Key, out.Keys[j].Key) })
	coins := make(map[string]bool)
	for _, c := range append(append([]string{}, a.Coins...), b.Coins...) {
		coins[c] = true
	}
	for c := range coins {
		out.Coins = append(out.Coins, c)
	}
	sort.Strings(out.Coins)
	for _, src := range []cycleMarketRequirements{a, b} {
		for coin, need := range src.Funding {
			f := out.Funding[coin]
			f.Scalar = f.Scalar || need.Scalar
			f.Records = f.Records || need.Records
			out.Funding[coin] = f
		}
		for k, w := range src.Observations {
			if w > out.Observations[k] {
				out.Observations[k] = w
			}
		}
		for k, w := range src.Macro {
			if existing, ok := out.Macro[k]; !ok || w > existing {
				out.Macro[k] = w
			}
		}
		for _, k := range sortedBookNeedKeys(src.Books) {
			addBookNeed(out.Books, k, src.Books[k], nil)
		}
	}
	acct := make(map[string]bool)
	for _, c := range append(append([]string{}, a.AccountingCoins...), b.AccountingCoins...) {
		acct[c] = true
	}
	for c := range acct {
		out.AccountingCoins = append(out.AccountingCoins, c)
	}
	sort.Strings(out.AccountingCoins)
	return out
}

func (bc *basketCoordinator) beginCycle() {
	if !bc.active() {
		return
	}
	bc.cycle++
}

func (bc *basketCoordinator) rankReady() bool {
	if !bc.active() {
		return false
	}
	for _, rt := range bc.runtimes {
		if rt.rank != nil && rt.rank.finished() {
			return true
		}
	}
	return false
}

func (bc *basketCoordinator) reload(cfg *Config) {
	if bc == nil || cfg == nil {
		return
	}
	bc.cfg = cfg
	bc.members = make(map[string]StrategyConfig)
	for _, sc := range cfg.Strategies {
		if _, ok := bc.byMember[sc.ID]; ok {
			bc.members[sc.ID] = sc
		}
	}
}

func (bc *basketCoordinator) step(in basketCycleInputs) {
	if !bc.active() {
		return
	}
	bc.ensureMemberLots()
	bc.mu.Lock()
	for _, id := range bc.sortedIDs() {
		bc.stepRuntime(bc.runtimes[id], in)
	}
	for _, id := range bc.sortedPairIDs() {
		bc.stepPair(bc.runtimes[id], in)
	}
	bc.mu.Unlock()
	bc.flushOutbox()
}

func (bc *basketCoordinator) ensureMemberLots() {
	if bc.lots == nil {
		return
	}
	for _, id := range bc.sortedIDs() {
		for _, m := range bc.runtimes[id].Contract.Members {
			bc.lots.Ensure(m.Coin)
		}
	}
}

func (bc *basketCoordinator) memberLiveRefusal(rt *basketRuntime) string {
	for _, id := range rt.memberIDs() {
		sc, ok := bc.members[id]
		if !ok {
			return fmt.Sprintf("member %s has no configuration", id)
		}
		if msg := basketLiveRefusal(sc); msg != "" {
			return msg
		}
		if isLiveArgs(sc.Args) {
			return fmt.Sprintf("strategy[%s]: live basket dispatch is refused", sc.ID)
		}
	}
	return ""
}

func (bc *basketCoordinator) stepRuntime(rt *basketRuntime, in basketCycleInputs) {
	rt.clock = in.Now
	rt.expiryHold, rt.expiryOpen = "", nil
	_, suppressionChanged := bc.advanceExpirySuppression(rt, in.Now)
	held := func() { rt.wakeRetryAt = in.Now.Add(basketHeldWakeRetry) }
	if msg := bc.memberLiveRefusal(rt); msg != "" {
		bc.alert(rt, nil, "live-refused", msg+"; no basket action is dispatched", true)
		held()
		return
	}
	if in.SaveBlocked {
		bc.logf(rt, "state saves are failing for the %s scope; no elective basket action runs this cycle, and stops, closes and protection continue", partitionLabel(rt.Partition))
		held()
		return
	}
	if rt.needsDurable {
		bc.logf(rt, "the last basket settlement is not durable yet; no new elective action runs until the %s state file saves", partitionLabel(rt.Partition))
		held()
		return
	}
	if suppressionChanged && !bc.persistProgress(rt) {
		held()
		return
	}
	if a := rt.pendingAction(); a != nil {
		if !bc.reconcileAction(rt, a, in) {
			held()
			return
		}
	}
	rt.wakeRetryAt = time.Time{}
	bc.detectExceptions(rt, in)
	if !bc.processExpiry(rt, in) {
		held()
		return
	}
	bc.collectRank(rt, in)
	bc.maybeStartDeadline(rt, in)
	if in.Once && rt.rank != nil {
		job := rt.rank
		bc.unlocked(func() {
			select {
			case <-job.done:
			case <-time.After(job.Timeout + 2*time.Second):
			}
		})
		bc.collectRank(rt, in)
	}
	bc.execute(rt, in)
}

func (bc *basketCoordinator) persistProgress(rt *basketRuntime) bool {
	if err := bc.store.PersistBasketProgress(rt); err != nil {
		bc.logf(rt, "could not save basket progress: %v; elective actions hold until it saves", err)
		return false
	}
	return true
}

func (bc *basketCoordinator) maybeStartDeadline(rt *basketRuntime, in basketCycleInputs) {
	now := in.Now
	d := evaluationDeadline(now, rt.Contract.RebalanceIntervalSeconds)
	dMs := d.UnixMilli()
	if _, ok := rt.Rebalances[dMs]; ok {
		return
	}
	prev := rt.latest()
	if prev != nil && prev.DeadlineMs >= dMs {
		return
	}
	windowEnd := d.Add(time.Duration(rt.Contract.EvaluationWindowSeconds) * time.Second)
	rb := &basketRebalance{
		Partition:   rt.Partition,
		BasketID:    rt.Contract.BasketID,
		DeadlineMs:  dMs,
		Status:      basketRebalanceReserved,
		ContractFP:  rt.Contract.Fingerprint(),
		CreatedAtMs: now.UnixMilli(),
		UpdatedAtMs: now.UnixMilli(),
	}
	hold := ""
	if now.After(windowEnd) {
		hold = fmt.Sprintf("the evaluation window closed at %s before the coordinator ran", windowEnd.UTC().Format(time.RFC3339))
	}
	var snap *marketSnapshot
	if hold == "" && in.SnapshotFor != nil {
		bc.unlocked(func() { snap = in.SnapshotFor(rt, d) })
		if snap != nil {
			if cutoff := snap.decisionCutoff(); cutoff.Before(d) {
				bc.logf(rt, "deadline %s: the cycle snapshot's decision cutoff %s is before the deadline; nothing is reserved, and a later cycle in the evaluation window ranks it", formatClosedBarMs(dMs), cutoff.Format(time.RFC3339))
				return
			}
		}
	}
	if prev != nil && !basketRebalanceTerminal(prev.Status) {
		if rt.pendingAction() != nil {
			hold = "the previous rebalance still has an unresolved member action"
		} else {
			prev.Status = basketRebalanceSuperseded
			prev.Outcome.Result = "superseded"
			prev.Outcome.Deviations = append(prev.Outcome.Deviations, basketDeviation{Kind: "superseded", Detail: fmt.Sprintf("the deadline %s replaced these targets", formatClosedBarMs(dMs)), AtMs: now.UnixMilli()})
			prev.UpdatedAtMs = now.UnixMilli()
		}
	}
	if hold != "" {
		rb.Status = basketRebalanceHeld
		rb.HoldReason = hold
		rb.Outcome.Result = "held"
	}
	if err := bc.store.ReserveBasketRebalance(rt, rb); err != nil {
		if err == errBasketDeadlineTaken {
			bc.logf(rt, "deadline %s is already recorded; it is never rebalanced twice", formatClosedBarMs(dMs))
			rb.Status = basketRebalanceHeld
			rb.HoldReason = "the deadline was already recorded"
			rb.durableStatus = rb.Status
			rt.Rebalances[dMs] = rb
			return
		}
		bc.logf(rt, "could not reserve deadline %s: %v; retrying within the evaluation window", formatClosedBarMs(dMs), err)
		rt.wakeRetryAt = now.Add(basketHeldWakeRetry)
		return
	}
	rt.Rebalances[dMs] = rb
	if hold != "" {
		bc.logf(rt, "deadline %s held: %s", formatClosedBarMs(dMs), hold)
		bc.persistProgress(rt)
		return
	}
	holdNow := func(reason string) {
		rb.Status = basketRebalanceHeld
		rb.HoldReason = reason
		rb.Outcome.Result = "held"
		rb.UpdatedAtMs = now.UnixMilli()
		bc.logf(rt, "deadline %s held: %s; existing positions, stops and recovery continue", formatClosedBarMs(dMs), reason)
		bc.persistProgress(rt)
	}
	if snap == nil {
		holdNow("no sealed market snapshot exists for the rebalance deadline")
		return
	}
	specs, coins := basketPayloadSpecs(rt, dMs)
	payload, err := marketPayloadFor(snap, specs, coins)
	if err != nil {
		holdNow("the sealed snapshot cannot supply every member frame: " + err.Error())
		return
	}
	job, err := startBasketRank(bc.parent, rt, dMs, payload, now, bc.rankRun, bc.rankClock)
	if err != nil {
		holdNow(err.Error())
		return
	}
	rb.Status = basketRebalanceRanking
	rb.SnapshotID = payload.SnapshotID
	rb.Generation = payload.Generation
	rb.CutoffMs = payload.DecisionCutoffMs
	rb.UpdatedAtMs = now.UnixMilli()
	rt.rank = job
	bc.logf(rt, "deadline %s: ranking %d members with %s on snapshot %s/%d", formatClosedBarMs(dMs), len(rt.Contract.Members), rt.Contract.Ranker, rb.SnapshotID, rb.Generation)
	bc.persistProgress(rt)
}

func (bc *basketCoordinator) collectRank(rt *basketRuntime, in basketCycleInputs) {
	job := rt.rank
	if job == nil || !job.finished() {
		return
	}
	rt.rank = nil
	rb := rt.Rebalances[job.DeadlineMs]
	if rb == nil || rb.Status != basketRebalanceRanking {
		bc.logf(rt, "a ranker result for deadline %s arrived after the deadline left the ranking state; it is discarded", formatClosedBarMs(job.DeadlineMs))
		return
	}
	now := in.Now
	hold := func(reason string) {
		rb.Status = basketRebalanceHeld
		rb.HoldReason = reason
		rb.Outcome.Result = "held"
		rb.UpdatedAtMs = now.UnixMilli()
		bc.logf(rt, "deadline %s held: %s; existing positions, stops and recovery continue", formatClosedBarMs(job.DeadlineMs), reason)
		bc.persistProgress(rt)
	}
	if job.stderr != "" {
		bc.logf(rt, "ranker stderr: %s", job.stderr)
	}
	if job.err != nil {
		hold(job.err.Error())
		return
	}
	if job.Clock == basketRankClockSimulated && !now.Before(job.WindowEnd) {
		hold(fmt.Sprintf("the ranker result was collected at simulated time %s, at or after the evaluation window end %s", now.UTC().Format(time.RFC3339), job.WindowEnd.UTC().Format(time.RFC3339)))
		return
	}
	res := job.result
	if res != nil {
		rb.InputFP = res.InputFingerprint
		rb.EvidenceJSON = basketRankEvidenceJSON(res)
	}
	reason, err := validateBasketTargetResult(rt, job, res, bc.targetPolicy)
	if err != nil {
		hold("invalid ranker output: " + err.Error())
		return
	}
	if reason != "" {
		hold(reason)
		return
	}
	targets, why := freezeBasketTargets(rt, res, job.Payload, bc.lots)
	if why != "" {
		hold(why)
		return
	}
	rb.TargetsJSON = targets.JSON()
	rb.targets = targets
	rb.Status = basketRebalanceFrozen
	rb.UpdatedAtMs = now.UnixMilli()
	bc.logf(rt, "deadline %s: targets frozen (input %s)", formatClosedBarMs(job.DeadlineMs), shortFingerprint(rb.InputFP))
	for _, id := range rt.memberIDs() {
		mt := targets.Members[id]
		if mt.Excluded != "" && mt.Weight == 0 {
			bc.logf(rt, "  %s %s excluded: %s", id, mt.Coin, mt.Excluded)
			continue
		}
		bc.logf(rt, "  %s %s weight %.6g target $%.2f qty %.10g (residual $%.2f)", id, mt.Coin, mt.Weight, mt.TargetNotionalUSD, mt.TargetQty, mt.ResidualUSD)
	}
	bc.persistProgress(rt)
}

func shortFingerprint(fp string) string {
	if len(fp) > 12 {
		return fp[:12]
	}
	return fp
}

func (bc *basketCoordinator) marks(rt *basketRuntime, in basketCycleInputs) (map[string]float64, []string) {
	out := make(map[string]float64, len(rt.Contract.Members))
	var missing []string
	for _, m := range rt.Contract.Members {
		if in.Snapshot == nil {
			missing = append(missing, m.Coin)
			continue
		}
		px, ok := in.Snapshot.midFor(m.Coin)
		if !ok || !finitePositive(px) {
			missing = append(missing, m.Coin)
			continue
		}
		out[m.Coin] = px
	}
	sort.Strings(missing)
	return out, missing
}

func basketUnpricedHeld(e basketExposure, missing []string) []string {
	var out []string
	for _, coin := range missing {
		if e.Qty[coin] != 0 {
			out = append(out, coin)
		}
	}
	return out
}

func (bc *basketCoordinator) unpricedMembers(rt *basketRuntime, rb *basketRebalance, targets *basketTargets, e basketExposure, missing []string, now time.Time) []string {
	absent := make(map[string]bool, len(missing))
	for _, coin := range missing {
		absent[coin] = true
	}
	var out []string
	for _, m := range rt.Contract.Members {
		if !absent[m.Coin] {
			continue
		}
		held := e.Qty[m.Coin] != 0
		wanted := false
		if targets != nil {
			wanted = targets.Members[m.StrategyID].TargetQty != 0
		}
		if !held && !wanted {
			continue
		}
		out = append(out, m.Coin)
		if held {
			if why := basketListingExclusion(rt.Contract, m, now.UnixMilli()); why != "" {
				bc.alert(rt, rb, "unpriced-held|"+m.StrategyID, fmt.Sprintf("%s holds %.10g %s, but the declared listing evidence excludes the coin (%s) and no sealed mark exists; the basket cannot value it, so elective and recovery actions hold until an operator closes it", m.StrategyID, e.Qty[m.Coin], m.Coin, why), true)
			}
		}
	}
	sort.Strings(out)
	return out
}

func (bc *basketCoordinator) exposureLocked(rt *basketRuntime, marks map[string]float64) basketExposure {
	e := basketExposure{Qty: make(map[string]float64, len(rt.Contract.Members)), Mark: marks}
	for _, m := range rt.Contract.Members {
		e.Qty[m.Coin] = basketPositionSigned(bc.state.Strategies[m.StrategyID], m.Coin)
	}
	return e
}

func (bc *basketCoordinator) detectExceptions(rt *basketRuntime, in basketCycleInputs) {
	type diff struct {
		m             basketContractMember
		exp, actual   float64
		cause, breach string
		growthOrFlip  bool
	}
	var diffs []diff
	marks, missing := bc.marks(rt, in)
	exposure := bc.exposureLocked(rt, marks)
	unpriced := basketUnpricedHeld(exposure, missing)
	for _, m := range rt.Contract.Members {
		actual := exposure.Qty[m.Coin]
		exp := rt.Expected[m.StrategyID]
		if basketQtyEqual(actual, exp) {
			continue
		}
		d := diff{m: m, exp: exp, actual: actual}
		switch {
		case actual == 0:
			d.cause = "the position closed outside the basket (stop, take-profit, kill switch, liquidation or operator close)"
		case (actual > 0) == (exp > 0) && math.Abs(actual) < math.Abs(exp):
			d.cause = "the position was reduced outside the basket (stop, take-profit, kill switch, liquidation or operator close)"
		default:
			d.cause = "the position grew or flipped outside the basket coordinator"
			d.growthOrFlip = true
		}
		if len(unpriced) == 0 {
			if v := exposure.reading().violations(basketBoundsFor(rt.Contract)); len(v) > 0 {
				d.breach = strings.Join(v, "; ")
			}
		} else {
			d.breach = "unknown: no valid sealed mark for " + strings.Join(unpriced, ", ")
		}
		diffs = append(diffs, d)
	}
	if len(diffs) == 0 {
		return
	}
	rb := rt.latest()
	now := in.Now
	for _, d := range diffs {
		rt.Expected[d.m.StrategyID] = d.actual
		msg := fmt.Sprintf("%s %s: expected %.10g, book holds %.10g — %s", d.m.StrategyID, d.m.Coin, d.exp, d.actual, d.cause)
		if d.breach != "" {
			msg += "; bound breach: " + d.breach
		}
		dup := false
		if rb != nil {
			for _, ex := range rb.Outcome.Exceptions {
				if ex.StrategyID == d.m.StrategyID && basketQtyEqual(ex.ExpectedQty, d.exp) && basketQtyEqual(ex.ActualQty, d.actual) {
					dup = true
				}
			}
		}
		if rb != nil && !dup {
			rb.Outcome.Exceptions = append(rb.Outcome.Exceptions, basketException{StrategyID: d.m.StrategyID, ExpectedQty: d.exp, ActualQty: d.actual, Cause: d.cause, Breach: d.breach, AtMs: now.UnixMilli()})
			rb.Outcome.Result = "protective_exception"
			rb.IncreasesFrozen = true
			if rb.Outcome.FreezeReason == "" {
				rb.Outcome.FreezeReason = "protective exception: " + msg
			}
			if d.breach != "" {
				rb.Outcome.RecoveryOpen = true
			}
			rb.UpdatedAtMs = now.UnixMilli()
		}
		key := fmt.Sprintf("exception|%s|%.10g|%.10g", d.m.StrategyID, d.exp, d.actual)
		bc.alert(rt, rb, key, msg+"; increases are frozen and reduce-only recovery continues", d.growthOrFlip || d.breach != "")
	}
	bc.persistProgress(rt)
}

func (bc *basketCoordinator) increaseHold(rt *basketRuntime, rb *basketRebalance, sc StrategyConfig, s *StrategyState, in basketCycleInputs) string {
	if rb.IncreasesFrozen {
		reason := rb.Outcome.FreezeReason
		if reason == "" {
			reason = "increases are frozen for this rebalance"
		}
		return reason
	}
	if bc.state.partitionLatched(rt.Partition) {
		return "the " + partitionLabel(rt.Partition) + " kill switch is latched"
	}
	if bc.store.persistenceHoldsPartition(rt.Partition) {
		return "the " + partitionLabel(rt.Partition) + " scope has an unacknowledged state save failure"
	}
	if sr := in.ScopeRisk[rt.Partition]; sr != nil {
		if sr.KillSwitchFired {
			return "the " + partitionLabel(rt.Partition) + " kill switch fired this cycle"
		}
		if sr.DailyLossEntriesHeld {
			return "the daily loss limit holds entries until UTC rollover"
		}
		if sr.NotionalBlocked {
			return "the partition notional cap holds new opens"
		}
	}
	if sc.Paused {
		return sc.ID + " is paused"
	}
	if s != nil && s.RiskState.CircuitBreaker {
		return sc.ID + " has an active circuit breaker"
	}
	return ""
}

func basketMemberHasStopOwner(sc StrategyConfig) bool {
	if EffectiveStopLossPct(sc) > 0 {
		return true
	}
	for _, v := range []*float64{sc.StopLossATRMult, sc.TrailingStopATRMult} {
		if v != nil && *v > 0 {
			return true
		}
	}
	return sc.StopLossATRMultRegime.IsConfigured() || sc.TrailingStopATRMultRegime.IsConfigured() || strategyUsesUnifiedRegimeClose(sc)
}

func (bc *basketCoordinator) unprotectedMember(rt *basketRuntime) string {
	for _, m := range rt.Contract.Members {
		sc := bc.members[m.StrategyID]
		s := bc.state.Strategies[m.StrategyID]
		if s == nil || !basketMemberHasStopOwner(sc) {
			continue
		}
		pos := s.Positions[m.Coin]
		if pos == nil || pos.Quantity <= 0 {
			continue
		}
		if pos.StopLossTriggerPx <= 0 && pos.StopLossOID <= 0 {
			return fmt.Sprintf("%s %s has no armed stop", m.StrategyID, m.Coin)
		}
	}
	return ""
}

func (bc *basketCoordinator) admitIncrease(rt *basketRuntime, sc StrategyConfig, s *StrategyState, c basketCandidate, now time.Time) (basketMemberObservation, string) {
	obs, ok := rt.observations[sc.ID]
	if !ok {
		return obs, "no management check has produced a closed-bar decision yet"
	}
	cb := obs.Fields.ClosedBar
	if why := closedBarDecisionIdentityError(sc, cb, now); why != "" {
		return obs, "invalid closed-bar decision identity: " + why
	}
	if indicatorsATRValue(obs.Indicators) <= 0 {
		return obs, "the closed-bar decision carries no ATR for the entry stop"
	}
	g := s.EntryGuard
	if g == nil {
		return obs, "the durable entry record for this member is unavailable"
	}
	if g.Partition != rt.Partition {
		return obs, "the durable entry record belongs to another partition"
	}
	key := entryDecisionKey{Symbol: c.Coin, IntervalMs: cb.IntervalMs, Side: c.PositionSide}
	rec := g.Decisions[key]
	if rec != nil {
		switch {
		case cb.BarOpenMs <= rec.BootstrapThroughBarMs:
			return obs, "upgrade bootstrap holds the first observed candle"
		case cb.BarOpenMs <= rec.ConsumedThroughBarMs:
			return obs, fmt.Sprintf("the %s decision for the candle opened %s is already consumed", c.PositionSide, formatClosedBarMs(rec.ConsumedThroughBarMs))
		case cb.BarOpenMs < rec.LatestObservedBarMs:
			return obs, "the decision candle is older than the newest observed candle"
		}
	}
	if in := g.unresolvedIntent(); in != nil {
		return obs, fmt.Sprintf("entry %s has no resolved venue outcome", in.IntentID)
	}
	return obs, ""
}

func (bc *basketCoordinator) partitionGate(rt *basketRuntime, sc StrategyConfig, s *StrategyState, c basketCandidate, qty, mark float64, marks map[string]float64) string {
	notional := qty * mark
	lev := EffectiveExchangeLeverage(sc)
	if lev <= 0 {
		lev = 1
	}
	execNotional := qty * ApplyAdverseSlippage(mark, c.OrderSide == "buy")
	if need := execNotional/lev + CalculatePlatformSpotFee("hyperliquid", execNotional); s.Cash < need {
		return fmt.Sprintf("%s paper margin $%.2f is below the $%.2f this tranche needs", sc.ID, s.Cash, need)
	}
	pr := partitionRiskConfig(bc.cfg, rt.Partition)
	states := filterStatesByPartition(bc.state.Strategies, bc.cfg.Strategies, rt.Partition)
	cfgs := strategiesInPartition(bc.cfg.Strategies, rt.Partition)
	prices := make(map[string]float64, len(marks))
	for k, v := range marks {
		prices[k] = v
	}
	if pr != nil && pr.MaxNotionalUSD > 0 {
		if total := PortfolioNotional(states, prices) + notional; total > pr.MaxNotionalUSD {
			return fmt.Sprintf("the partition notional would reach $%.2f, above the $%.2f cap", total, pr.MaxNotionalUSD)
		}
	}
	if exposureCapConfigured(pr) {
		signal := 1
		if c.PositionSide == entrySideShort {
			signal = -1
		}
		hyp := make(map[string]*StrategyState, len(states))
		var pv float64
		for id, st := range states {
			hyp[id] = st
			pv += displayStrategyValue(st, nil)
		}
		clone := *s
		clone.Positions = make(map[string]*Position, len(s.Positions))
		for k, p := range s.Positions {
			clone.Positions[k] = p
		}
		if p := s.Positions[c.Coin]; p != nil && p.Quantity > 0 {
			cp := *p
			cp.Quantity += qty
			clone.Positions[c.Coin] = &cp
		} else {
			clone.Positions[c.Coin] = &Position{Symbol: c.Coin, Quantity: qty, AvgCost: mark, Side: c.PositionSide, Multiplier: 1}
		}
		hyp[s.ID] = &clone
		before := evaluateExposureCap(pr, states, cfgs, prices, pv)
		if blocked, why := exposureCapBlocksSignal(before, c.Coin, signal, 0, math.Abs(c.CurrentQty), c.PositionSide, true, true); blocked {
			return why
		}
		after := evaluateExposureCap(pr, hyp, cfgs, prices, pv)
		dir := c.PositionSide
		if (dir == entrySideLong && after.LongBlocked) || (dir == entrySideShort && after.ShortBlocked) {
			return fmt.Sprintf("the tranche would breach the same-direction exposure cap ($%.2f long, $%.2f short, cap $%.2f)", after.LongUSD, after.ShortUSD, after.CapUSD)
		}
		if stat, ok := after.OverConcentrated[c.Coin]; ok && stat.Direction == dir {
			return fmt.Sprintf("the tranche would leave %s at %.1f%% of portfolio value (cap %.1f%%)", c.Coin, stat.Pct, after.ConcentrationPct)
		}
	}
	return ""
}

func (bc *basketCoordinator) lotPeek(coin string) hlLotLookup {
	return bc.lots.Peek(coin)
}

func (bc *basketCoordinator) lotFloor(coin string, qty float64, mark float64) float64 {
	lot := hlLotLookup{Coin: coin, Reason: "no venue metadata source"}
	if bc.lots != nil {
		lot = bc.lots.Lookup(coin)
	}
	if !lot.Known || !finitePositive(qty) {
		return 0
	}
	q := hlFloorLotSize(qty, lot.SzDecimals)
	if q*mark < hlVenueCloseGateThresholdUSD() {
		return 0
	}
	return q
}

func (bc *basketCoordinator) lotCeil(coin string, qty float64) float64 {
	lot := hlLotLookup{Coin: coin}
	if bc.lots != nil {
		lot = bc.lots.Lookup(coin)
	}
	if !lot.Known || !finitePositive(qty) {
		return qty
	}
	step := math.Pow(10, -float64(lot.SzDecimals))
	q := hlFloorLotSize(qty, lot.SzDecimals)
	if q < qty*(1-1e-12) {
		q = hlFloorLotSize(q+step*1.5, lot.SzDecimals)
	}
	return q
}

func (bc *basketCoordinator) dustQty(coin string, mark float64) float64 {
	if !finitePositive(mark) {
		return 0
	}
	return hlVenueCloseGateThresholdUSD() / mark
}

func (bc *basketCoordinator) execute(rt *basketRuntime, in basketCycleInputs) {
	rb := rt.latest()
	if rb == nil {
		return
	}
	recoveryOnly := false
	if rb.Status != basketRebalanceFrozen && rb.Status != basketRebalanceExecuting {
		if !rb.Outcome.RecoveryOpen {
			return
		}
		recoveryOnly = true
	}
	var targets *basketTargets
	var suppressed []string
	if !recoveryOnly {
		var err error
		targets, err = rb.Targets()
		if err != nil || targets == nil {
			bc.alert(rt, rb, "targets-unreadable", fmt.Sprintf("deadline %s targets are unreadable (%v); no elective action runs", formatClosedBarMs(rb.DeadlineMs), err), true)
			return
		}
		targets, suppressed = bc.effectiveTargets(rt, rb, targets)
		if len(suppressed) > 0 {
			detail := fmt.Sprintf("%s reached the holding horizon; the effective target is zero through this deadline, and the frozen targets stay unchanged", strings.Join(suppressed, ", "))
			bc.noteDeviation(rt, rb, "expiry_suppressed", detail, in.Now)
		}
	}
	bounds := basketBoundsFor(rt.Contract)
	for n := 0; n < basketMaxActionsPerStep; n++ {
		if rt.pendingAction() != nil || rt.needsDurable {
			return
		}
		marks, missing := bc.marks(rt, in)
		exposure := bc.exposureLocked(rt, marks)
		if unpriced := bc.unpricedMembers(rt, rb, targets, exposure, missing, in.Now); len(unpriced) > 0 {
			bc.logf(rt, "no valid sealed mark for %s; elective actions hold this cycle", strings.Join(unpriced, ", "))
			return
		}
		if bc.state.partitionLatched(rt.Partition) {
			bc.logf(rt, "the %s kill switch is latched; the kill switch owns flattening, so no elective basket action runs", partitionLabel(rt.Partition))
			return
		}
		dust := func(coin string) float64 { return bc.dustQty(coin, marks[coin]) }
		var reductions, increases []basketCandidate
		if !recoveryOnly {
			reductions, increases = basketCandidates(targets, exposure, rt.Contract.Members, dust)
		}
		if rb.IncreasesFrozen || recoveryOnly {
			ceil := func(coin string, qty float64) float64 { return bc.lotCeil(coin, qty) }
			reductions = append(reductions, basketNetRecoveryCandidates(exposure, bounds, rt.Contract.Members, reductions, ceil)...)
		}
		if len(rt.expiryOpen) > 0 {
			kept := reductions[:0]
			for _, c := range reductions {
				if !rt.expiryOpen[c.StrategyID] {
					kept = append(kept, c)
				}
			}
			reductions = kept
		}
		if recoveryOnly && len(reductions) == 0 {
			rb.Outcome.RecoveryOpen = false
			rb.UpdatedAtMs = in.Now.UnixMilli()
			bc.logf(rt, "deadline %s: reduce-only recovery restored the net bound (net $%.2f)", formatClosedBarMs(rb.DeadlineMs), exposure.net())
			bc.persistProgress(rt)
			return
		}
		floor := func(coin string, qty float64) float64 { return bc.lotFloor(coin, qty, marks[coin]) }
		if len(reductions) > 0 {
			acted := false
			var reasons []string
			for _, c := range reductions {
				tr := basketLegalTranche(exposure, c, bounds, false, floor)
				if !(tr.Qty > 0) {
					reasons = append(reasons, tr.Detail)
					continue
				}
				if !bc.runAction(rt, rb, c, tr.Qty, marks[c.Coin], basketMemberObservation{}, in) {
					return
				}
				acted = true
				break
			}
			if acted {
				continue
			}
			bc.logf(rt, "no legal reduction sequence this cycle: %s; protection stays active", strings.Join(reasons, "; "))
			bc.noteDeviation(rt, rb, "no_legal_reduction", strings.Join(reasons, "; "), in.Now)
			return
		}
		if recoveryOnly {
			return
		}
		if rt.expiryHold != "" {
			bc.logf(rt, "increases hold: %s; protection stays active", rt.expiryHold)
			return
		}
		if len(increases) == 0 {
			bc.complete(rt, rb, targets, exposure, in)
			return
		}
		if rb.IncreasesFrozen {
			bc.logf(rt, "increases are frozen (%s); reduce-only recovery is complete for now", rb.Outcome.FreezeReason)
			return
		}
		if why := bc.unprotectedMember(rt); why != "" {
			bc.logf(rt, "increases hold: %s; protect every residual position first", why)
			bc.alert(rt, rb, "unprotected|"+why, why+"; increases hold until protection is verified", true)
			return
		}
		acted := false
		var waits []string
		for _, c := range basketOrderIncreases(increases, exposure) {
			sc := bc.members[c.StrategyID]
			s := bc.state.Strategies[c.StrategyID]
			if s == nil {
				waits = append(waits, c.StrategyID+": no state")
				continue
			}
			if why := bc.increaseHold(rt, rb, sc, s, in); why != "" {
				waits = append(waits, c.StrategyID+": "+why)
				continue
			}
			obs, why := bc.admitIncrease(rt, sc, s, c, in.Now)
			if why != "" {
				waits = append(waits, c.StrategyID+": "+why)
				continue
			}
			tr := basketLegalTranche(exposure, c, bounds, true, floor)
			if !(tr.Qty > 0) {
				waits = append(waits, tr.Detail)
				continue
			}
			gate := bc.partitionGate(rt, sc, s, c, tr.Qty, marks[c.Coin], marks)
			if gate != "" {
				waits = append(waits, c.StrategyID+": "+gate)
				continue
			}
			if !bc.runAction(rt, rb, c, tr.Qty, marks[c.Coin], obs, in) {
				return
			}
			acted = true
			break
		}
		if !acted {
			bc.logf(rt, "increases wait: %s", strings.Join(waits, "; "))
			return
		}
	}
}

func basketNetRecoveryCandidates(e basketExposure, b basketBounds, members []basketContractMember, existing []basketCandidate, ceil func(coin string, qty float64) float64) []basketCandidate {
	net := e.net()
	excess := math.Abs(net) - b.NetUSD
	if excess <= 0 {
		return nil
	}
	taken := make(map[string]bool, len(existing))
	for _, c := range existing {
		taken[c.StrategyID] = true
	}
	var out []basketCandidate
	for _, m := range members {
		q := e.Qty[m.Coin]
		if taken[m.StrategyID] || q == 0 || (q > 0) != (net > 0) {
			continue
		}
		mark := e.Mark[m.Coin]
		if !finitePositive(mark) {
			continue
		}
		qty := math.Min(math.Abs(q), ceil(m.Coin, math.Max(excess, hlVenueCloseGateThresholdUSD())/mark))
		if !(qty > 0) {
			continue
		}
		c := basketCandidate{StrategyID: m.StrategyID, Coin: m.Coin, Direction: basketDirectionReduce, FullQty: qty, CurrentQty: q, TargetQty: 0}
		if q > 0 {
			c.PositionSide, c.OrderSide = entrySideLong, "sell"
		} else {
			c.PositionSide, c.OrderSide = entrySideShort, "buy"
		}
		out = append(out, c)
	}
	sort.SliceStable(out, func(i, j int) bool {
		return math.Abs(out[i].CurrentQty)*e.Mark[out[i].Coin] > math.Abs(out[j].CurrentQty)*e.Mark[out[j].Coin]
	})
	return out
}

func (bc *basketCoordinator) noteDeviation(rt *basketRuntime, rb *basketRebalance, kind, detail string, now time.Time) {
	for _, d := range rb.Outcome.Deviations {
		if d.Kind == kind && d.Detail == detail {
			return
		}
	}
	rb.Outcome.Deviations = append(rb.Outcome.Deviations, basketDeviation{Kind: kind, Detail: detail, AtMs: now.UnixMilli()})
	rb.UpdatedAtMs = now.UnixMilli()
	bc.persistProgress(rt)
}

func (bc *basketCoordinator) complete(rt *basketRuntime, rb *basketRebalance, targets *basketTargets, exposure basketExposure, in basketCycleInputs) {
	if rb.Status == basketRebalanceCompleted {
		return
	}
	now := in.Now
	for _, m := range rt.Contract.Members {
		mt := targets.Members[m.StrategyID]
		got := exposure.Qty[m.Coin]
		if mt.ResidualUSD != 0 {
			rb.Outcome.Deviations = append(rb.Outcome.Deviations, basketDeviation{StrategyID: m.StrategyID, Kind: "lot_residual", Detail: fmt.Sprintf("target $%.2f floors to %.10g lots (residual $%.2f)", mt.TargetNotionalUSD, mt.TargetQty, mt.ResidualUSD), AtMs: now.UnixMilli()})
		}
		if !basketQtyEqual(got, mt.TargetQty) {
			rb.Outcome.Deviations = append(rb.Outcome.Deviations, basketDeviation{StrategyID: m.StrategyID, Kind: "target_deviation", Detail: fmt.Sprintf("holds %.10g of a %.10g target (below the venue minimum step)", got, mt.TargetQty), AtMs: now.UnixMilli()})
		}
	}
	rb.Status = basketRebalanceCompleted
	switch {
	case len(rb.Outcome.Exceptions) > 0:
		rb.Outcome.Result = "protective_exception"
	case len(rb.Outcome.Deviations) > 0:
		rb.Outcome.Result = "completed_with_deviations"
	default:
		rb.Outcome.Result = "completed"
	}
	rb.Outcome.CompletedAtMs = now.UnixMilli()
	rb.UpdatedAtMs = now.UnixMilli()
	bc.logf(rt, "deadline %s: %s (gross $%.2f, net $%.2f)", formatClosedBarMs(rb.DeadlineMs), rb.Outcome.Result, exposure.gross(), exposure.net())
	err := bc.store.SavePartition(bc.state, rt.Partition)
	if err != nil {
		rt.needsDurable = true
		bc.logf(rt, "could not save the completed rebalance: %v; the next save retries", err)
	}
}

type basketActionProtection struct {
	StopLossOID        int64   `json:"stop_loss_oid,omitempty"`
	StopLossTriggerPx  float64 `json:"stop_loss_trigger_px,omitempty"`
	TPOIDs             []int64 `json:"tp_oids,omitempty"`
	EntryATR           float64 `json:"entry_atr,omitempty"`
	DecisionATR        float64 `json:"decision_atr,omitempty"`
	DecisionIntervalMs int64   `json:"decision_interval_ms,omitempty"`
	DecisionBoundaryMs int64   `json:"decision_boundary_ms,omitempty"`
	DecisionCutoffMs   int64   `json:"decision_cutoff_ms,omitempty"`
}

func basketProtectionSnapshot(s *StrategyState, coin string, obs basketMemberObservation) string {
	var p basketActionProtection
	if s != nil {
		if pos := s.Positions[coin]; pos != nil {
			p.StopLossOID, p.StopLossTriggerPx, p.TPOIDs, p.EntryATR = pos.StopLossOID, pos.StopLossTriggerPx, pos.TPOIDs, pos.EntryATR
		}
	}
	if cb := obs.Fields.ClosedBar; cb != nil {
		p.DecisionATR = indicatorsATRValue(obs.Indicators)
		p.DecisionIntervalMs, p.DecisionBoundaryMs, p.DecisionCutoffMs = cb.IntervalMs, cb.DecisionBoundaryMs, cb.CutoffMs
	}
	b, err := json.Marshal(p)
	if err != nil {
		return ""
	}
	return string(b)
}

func basketActionObservation(a *basketAction) basketMemberObservation {
	var p basketActionProtection
	_ = json.Unmarshal([]byte(a.ProtectionJSON), &p)
	obs := basketMemberObservation{
		Fields: StrategyDecisionFields{ClosedBar: &ClosedBarDecision{
			Enabled:            true,
			IntervalMs:         p.DecisionIntervalMs,
			BarOpenMs:          a.DecisionBarOpenMs,
			DecisionBoundaryMs: p.DecisionBoundaryMs,
			CutoffMs:           p.DecisionCutoffMs,
		}},
	}
	if p.DecisionATR > 0 {
		obs.Indicators = map[string]interface{}{"atr": p.DecisionATR}
	}
	return obs
}

func (bc *basketCoordinator) runAction(rt *basketRuntime, rb *basketRebalance, c basketCandidate, qty, mark float64, obs basketMemberObservation, in basketCycleInputs) bool {
	sc, ok := bc.members[c.StrategyID]
	if !ok {
		return false
	}
	if msg := basketLiveRefusal(sc); msg != "" || isLiveArgs(sc.Args) {
		bc.alert(rt, rb, "live-refused", "live basket dispatch is refused; no order is sent", true)
		return false
	}
	venue, err := bc.venueFor(sc)
	if err != nil {
		bc.alert(rt, rb, "venue|"+err.Error(), err.Error(), true)
		return false
	}
	cloid, err := venue.NewClientOrderID()
	if err != nil {
		bc.logf(rt, "could not generate a client order id: %v", err)
		return false
	}
	now := in.Now
	s := bc.state.Strategies[sc.ID]
	pre := basketPositionSigned(s, c.Coin)
	protection := basketProtectionSnapshot(s, c.Coin, obs)
	if s == nil {
		return false
	}
	seq := rt.nextSeq(rb.DeadlineMs, sc.ID, c.Direction)
	a := &basketAction{
		ActionID:       basketActionID(rt.Partition, rt.Contract.BasketID, rb.DeadlineMs, sc.ID, c.Direction, seq),
		Partition:      rt.Partition,
		BasketID:       rt.Contract.BasketID,
		DeadlineMs:     rb.DeadlineMs,
		StrategyID:     sc.ID,
		Symbol:         c.Coin,
		Direction:      c.Direction,
		Seq:            seq,
		OrderSide:      c.OrderSide,
		PositionSide:   c.PositionSide,
		Status:         basketActionReserved,
		RequestedQty:   qty,
		PreQty:         pre,
		PreNotionalUSD: pre * mark,
		MarkPx:         mark,
		ProtectionJSON: protection,
		Provider:       venue.Provider(),
		AccountKey:     venue.AccountKey(rt.Partition),
		ClientOrderID:  cloid,
		CreatedAtMs:    now.UnixMilli(),
		UpdatedAtMs:    now.UnixMilli(),
	}
	var intent *entryIntent
	if c.Direction == basketDirectionIncrease {
		cb := obs.Fields.ClosedBar
		a.DecisionBarOpenMs = cb.BarOpenMs
		if venue.LiveShaped() {
			intent, err = bc.reserveIncreaseIntent(rt, sc, s, a, venue, cb, now)
			if err != nil {
				bc.logf(rt, "%s increase held: the entry intent could not be reserved: %v", sc.ID, err)
				return false
			}
			a.EntryIntentID = intent.IntentID
		}
	}
	if rb.Status == basketRebalanceFrozen {
		rb.Status = basketRebalanceExecuting
	}
	if err := bc.store.ReserveBasketAction(rt, a); err != nil {
		bc.logf(rt, "%s %s %.10g held: the action could not be reserved before sending: %v", sc.ID, c.Direction, qty, err)
		if intent != nil {
			bc.releaseIntent(rt, sc, s, intent, entryIntentReserved, "the basket action reservation failed", now)
		}
		return false
	}
	rt.Actions[a.ActionID] = a
	a.Status = basketActionSubmitting
	a.UpdatedAtMs = now.UnixMilli()
	if !bc.persistProgress(rt) {
		a.Status = basketActionNoFill
		a.HoldReason = "the submission could not be recorded, so nothing was sent"
		bc.persistProgress(rt)
		if intent != nil {
			bc.releaseIntent(rt, sc, s, intent, entryIntentReserved, a.HoldReason, now)
		}
		return false
	}
	if intent != nil {
		if err := bc.markIntentSubmitting(sc, s, intent, a, now); err != nil {
			a.Status = basketActionNoFill
			a.HoldReason = "the entry intent submission could not be recorded, so nothing was sent: " + err.Error()
			bc.persistProgress(rt)
			bc.releaseIntent(rt, sc, s, intent, entryIntentReserved, a.HoldReason, now)
			return false
		}
	}
	bc.logf(rt, "%s %s %s %s %.10g @ mark $%.4f (action %s, client %s)", sc.ID, c.Direction, c.OrderSide, c.Coin, qty, mark, a.ActionID, a.ClientOrderID)
	var outcome basketVenueOutcome
	submitted := *a
	bc.unlocked(func() { outcome, err = venue.Submit(&submitted) })
	if err != nil {
		outcome = basketVenueOutcome{Outcome: basketVenueUnknown, Detail: "transport failure: " + err.Error()}
	}
	return bc.applyOutcome(rt, rb, a, outcome, obs, in, false)
}

func (bc *basketCoordinator) reserveIncreaseIntent(rt *basketRuntime, sc StrategyConfig, s *StrategyState, a *basketAction, venue basketVenue, cb *ClosedBarDecision, now time.Time) (*entryIntent, error) {
	id, err := newEntryIntentID()
	if err != nil {
		return nil, err
	}
	signal := 1
	if a.PositionSide == entrySideShort {
		signal = -1
	}
	in := &entryIntent{
		IntentID:           id,
		Partition:          rt.Partition,
		StrategyID:         sc.ID,
		Symbol:             a.Symbol,
		IntervalMs:         cb.IntervalMs,
		TargetSide:         a.PositionSide,
		BarOpenMs:          cb.BarOpenMs,
		DecisionBoundaryMs: cb.DecisionBoundaryMs,
		CutoffMs:           cb.CutoffMs,
		Provider:           venue.Provider(),
		AccountKey:         a.AccountKey,
		InstrumentType:     sc.Type,
		ClientOrderID:      a.ClientOrderID,
		Status:             entryIntentReserved,
		CreatedAtMs:        now.UnixMilli(),
		UpdatedAtMs:        now.UnixMilli(),
	}
	in.RequestJSON = marshalEntryJSON(entryIntentRequest{Version: entryIntentRequestVersion, Kind: "basket:" + a.ActionID, Signal: signal, PrevPosQty: math.Abs(a.PreQty), Price: a.MarkPx, Size: a.RequestedQty, Script: basketRankerScript})
	g := s.EntryGuard
	if g == nil {
		return nil, fmt.Errorf("durable entry record unavailable")
	}
	if prev := g.unresolvedIntent(); prev != nil {
		return nil, fmt.Errorf("entry %s is still unresolved", prev.IntentID)
	}
	if err := bc.store.ReserveEntryIntent(s, in); err != nil {
		return nil, err
	}
	g.Intents[in.IntentID] = in
	return in, nil
}

func (bc *basketCoordinator) markIntentSubmitting(sc StrategyConfig, s *StrategyState, in *entryIntent, a *basketAction, now time.Time) error {
	next := *in
	if err := bc.store.TransitionEntryIntent(sc.ID, &next, []string{entryIntentReserved}, entryIntentSubmitting); err != nil {
		return err
	}
	in.Status = entryIntentSubmitting
	in.durableStatus = entryIntentSubmitting
	in.UpdatedAtMs = now.UnixMilli()
	return nil
}

func (bc *basketCoordinator) releaseIntent(rt *basketRuntime, sc StrategyConfig, s *StrategyState, in *entryIntent, from, reason string, now time.Time) {
	next := *in
	next.EvidenceJSON = marshalEntryJSON(entryIntentEvidence{Outcome: "no_fill", Detail: reason, ObservedAtMs: now.UnixMilli()})
	if err := bc.store.TransitionEntryIntent(sc.ID, &next, []string{from, entryIntentSubmitting}, entryIntentNoFill); err != nil {
		bc.logf(rt, "entry %s could not be released (%v); it stays unresolved", in.IntentID, err)
		return
	}
	in.Status = entryIntentNoFill
	in.durableStatus = entryIntentNoFill
	in.EvidenceJSON = next.EvidenceJSON
}

func (bc *basketCoordinator) intentFor(s *StrategyState, a *basketAction) *entryIntent {
	if a.EntryIntentID == "" || s == nil || s.EntryGuard == nil {
		return nil
	}
	return s.EntryGuard.Intents[a.EntryIntentID]
}

func (bc *basketCoordinator) applyOutcome(rt *basketRuntime, rb *basketRebalance, a *basketAction, outcome basketVenueOutcome, obs basketMemberObservation, in basketCycleInputs, recovered bool) bool {
	sc := bc.members[a.StrategyID]
	s := bc.state.Strategies[a.StrategyID]
	intent := bc.intentFor(s, a)
	now := in.Now
	if outcome.ObservedAtMs == 0 {
		outcome.ObservedAtMs = now.UnixMilli()
	}
	evidence := marshalEntryJSON(outcome)
	if outcome.VenueOrderID != "" {
		a.VenueOrderID = outcome.VenueOrderID
	}
	switch outcome.Outcome {
	case basketVenueFilled, basketVenuePartial:
		if !finitePositive(outcome.FilledQty) || outcome.FilledQty > a.RequestedQty*(1+1e-9)+1e-12 {
			a.Status = basketActionUnresolved
			a.HoldReason = fmt.Sprintf("the venue reported %v filled of %v requested; the fill cannot be booked", outcome.FilledQty, a.RequestedQty)
			a.EvidenceJSON = evidence
			bc.persistProgress(rt)
			bc.actionAlert(rt, rb, a, "inconsistent|"+a.ActionID, fmt.Sprintf("%s action %s: %s", a.StrategyID, a.ActionID, a.HoldReason), true)
			return false
		}
		a.Status = basketActionSettling
		a.ConfirmedQty = outcome.FilledQty
		a.ConfirmedAvgPx = outcome.AvgPx
		a.ConfirmedFee = outcome.Fee
		a.EvidenceJSON = evidence
		a.HoldReason = ""
		a.UpdatedAtMs = now.UnixMilli()
		if venueLiveShaped(bc, sc) {
			if !bc.persistProgress(rt) {
				return false
			}
		}
		if !bc.settleAction(rt, rb, a, obs, in) {
			return false
		}
		if outcome.Outcome == basketVenuePartial || a.ConfirmedQty < a.RequestedQty*(1-1e-9) {
			detail := fmt.Sprintf("%s %s filled %.10g of %.10g", a.StrategyID, a.Direction, a.ConfirmedQty, a.RequestedQty)
			if a.Direction == basketDirectionIncrease {
				detail += "; the decision candle is consumed, and the underfill waits for a later eligible decision"
			}
			if a.expiry() {
				bc.logf(rt, "holding-horizon close %s: %s; the remainder takes a new attempt", a.ActionID, detail)
			} else if rb != nil {
				bc.noteDeviation(rt, rb, "partial_fill", detail, now)
			}
		}
		return true
	case basketVenueRejected:
		a.Status = basketActionNoFill
		a.HoldReason = outcome.Detail
		a.EvidenceJSON = evidence
		a.UpdatedAtMs = now.UnixMilli()
		if intent != nil {
			bc.releaseIntent(rt, sc, s, intent, entryIntentSubmitting, "venue rejected: "+outcome.Detail, now)
		}
		switch {
		case a.expiry():
			if recovered && a.Provider == "paper" {
				bc.logf(rt, "holding-horizon close %s: %s; a new attempt follows", a.ActionID, outcome.Detail)
			} else {
				bc.expiryFailed(rt, a, now, "rejected: "+outcome.Detail)
			}
		case recovered && a.Provider == "paper":
			bc.logf(rt, "action %s: %s", a.ActionID, outcome.Detail)
		case rb != nil:
			rb.IncreasesFrozen = true
			if rb.Outcome.FreezeReason == "" {
				rb.Outcome.FreezeReason = fmt.Sprintf("the %s %s leg for %s was rejected (%s)", a.Direction, a.OrderSide, a.StrategyID, outcome.Detail)
			}
			bc.alert(rt, rb, "rejected|"+a.ActionID, rb.Outcome.FreezeReason+"; further increases are blocked and reduce-only recovery starts", false)
		}
		if rb != nil {
			rb.UpdatedAtMs = now.UnixMilli()
		}
		return bc.persistProgress(rt)
	default:
		a.Status = basketActionUnresolved
		a.HoldReason = "the venue outcome is unknown: " + outcome.Detail
		a.EvidenceJSON = evidence
		a.UpdatedAtMs = now.UnixMilli()
		if intent != nil {
			intent.HoldReason = a.HoldReason
		}
		bc.persistProgress(rt)
		bc.actionAlert(rt, rb, a, "unknown|"+a.ActionID, fmt.Sprintf("%s action %s (client %s): %s; new risk holds until it reconciles", a.StrategyID, a.ActionID, a.ClientOrderID, a.HoldReason), false)
		return false
	}
}

func venueLiveShaped(bc *basketCoordinator, sc StrategyConfig) bool {
	v, err := bc.venueFor(sc)
	return err == nil && v.LiveShaped()
}

func (bc *basketCoordinator) settleAction(rt *basketRuntime, rb *basketRebalance, a *basketAction, obs basketMemberObservation, in basketCycleInputs) bool {
	sc := bc.members[a.StrategyID]
	logger := bc.memberLogger(a.StrategyID)
	defer logger.Close()
	now := in.Now
	s := bc.state.Strategies[a.StrategyID]
	if s == nil {
		return false
	}
	remaining := a.ConfirmedQty - a.AppliedQty
	if remaining <= 1e-12 {
		a.Status = basketActionCommitted
		return true
	}
	var booked basketBooking
	from := len(s.TradeHistory)
	prevDefer := s.deferTradePersist
	s.deferTradePersist = true
	withStrategyBookClock(s, now, func() {
		if a.Direction == basketDirectionIncrease {
			if obs.Fields.ClosedBar == nil || obs.Indicators == nil {
				obs = basketActionObservation(a)
			}
			booked = bookBasketPaperIncrease(sc, s, bc.cfg, a.Symbol, a.PositionSide, remaining, a.MarkPx, obs, logger)
		} else {
			booked = bookBasketPaperReduction(sc, s, a.Symbol, remaining, a.MarkPx, logger)
		}
	})
	s.deferTradePersist = prevDefer
	if !(booked.Qty > 0) {
		reason := booked.Hold
		if reason == "" {
			reason = "the booking owner recorded no fill"
		}
		if a.Provider == "paper" {
			a.Status = basketActionNoFill
			a.HoldReason = "paper booking held: " + reason
			bc.logf(rt, "action %s: %s", a.ActionID, a.HoldReason)
			if a.expiry() {
				bc.expiryFailed(rt, a, now, a.HoldReason)
			} else if rb != nil {
				rb.IncreasesFrozen = true
				if rb.Outcome.FreezeReason == "" {
					rb.Outcome.FreezeReason = fmt.Sprintf("the %s leg for %s could not be booked (%s)", a.Direction, a.StrategyID, reason)
				}
			}
			bc.persistProgress(rt)
			return false
		}
		a.Status = basketActionUnresolved
		a.HoldReason = "a confirmed venue fill could not be booked: " + reason
		bc.persistProgress(rt)
		bc.actionAlert(rt, rb, a, "unbooked|"+a.ActionID, fmt.Sprintf("%s action %s: %s", a.StrategyID, a.ActionID, a.HoldReason), true)
		return false
	}
	a.AppliedQty += booked.Qty
	a.AppliedFee += booked.Fee
	if a.ConfirmedAvgPx <= 0 {
		a.ConfirmedAvgPx = booked.Px
	}
	a.Status = basketActionCommitted
	a.UpdatedAtMs = now.UnixMilli()
	delta := booked.Qty
	if a.OrderSide == "sell" {
		delta = -delta
	}
	rt.Expected[a.StrategyID] += delta
	if math.Abs(rt.Expected[a.StrategyID]) < 1e-12 {
		rt.Expected[a.StrategyID] = 0
	}
	if a.Direction == basketDirectionIncrease && s.EntryGuard != nil && obs.Fields.ClosedBar != nil {
		key := entryDecisionKey{Symbol: a.Symbol, IntervalMs: obs.Fields.ClosedBar.IntervalMs, Side: a.PositionSide}
		markEntryConsumedLocked(s.EntryGuard, key, a.DecisionBarOpenMs, now)
	}
	if a.expiry() {
		if l := rt.Lifecycle[a.StrategyID]; l.active() && l.PositionID == a.LifecyclePositionID {
			l.Failures, l.RetryAtMs = 0, 0
			rt.Lifecycle[a.StrategyID] = l
		}
	}
	if in := bc.intentFor(s, a); in != nil {
		in.Status = entryIntentCommitted
		in.ConfirmedQty = a.ConfirmedQty
		in.ConfirmedNotional = a.ConfirmedQty * a.ConfirmedAvgPx
		in.ConfirmedFee = a.ConfirmedFee
		in.AppliedQty = a.AppliedQty
		in.AppliedNotional = booked.Qty * booked.Px
		in.AppliedFee = a.AppliedFee
		in.SettlementSeq++
		in.VenueOrderID = a.VenueOrderID
		in.HoldReason = ""
		in.EvidenceJSON = a.EvidenceJSON
		in.UpdatedAtMs = now.UnixMilli()
	}
	if rb != nil {
		rb.UpdatedAtMs = now.UnixMilli()
	}
	newTrades := append([]Trade(nil), s.TradeHistory[from:]...)
	err := bc.store.SavePartition(bc.state, rt.Partition)
	if err != nil {
		rt.needsDurable = true
		bc.logf(rt, "action %s booked %.10g in memory, but the %s state file did not save: %v; no further elective action runs until it does", a.ActionID, booked.Qty, partitionLabel(rt.Partition), err)
		return false
	}
	bc.logf(rt, "action %s committed: %s %.10g %s @ $%.4f (fee $%.4f)", a.ActionID, a.OrderSide, booked.Qty, a.Symbol, booked.Px, booked.Fee)
	if bc.notifier != nil && len(newTrades) > 0 {
		notifier, regime := bc.notifier, bc.cfg.Regime
		bc.outbox = append(bc.outbox, func() { sendTradeAlertRows(sc, newTrades, notifier, regime) })
	}
	return true
}

func (bc *basketCoordinator) reconcileAction(rt *basketRuntime, a *basketAction, in basketCycleInputs) bool {
	var rb *basketRebalance
	if !a.expiry() {
		rb = rt.Rebalances[a.DeadlineMs]
		if rb == nil {
			rb = &basketRebalance{Partition: rt.Partition, BasketID: rt.Contract.BasketID, DeadlineMs: a.DeadlineMs, Status: basketRebalanceExecuting}
			rt.Rebalances[a.DeadlineMs] = rb
		}
	}
	sc, ok := bc.members[a.StrategyID]
	if !ok {
		bc.actionAlert(rt, rb, a, "orphan|"+a.ActionID, fmt.Sprintf("action %s names %s, which has no configuration; it stays unresolved", a.ActionID, a.StrategyID), true)
		return false
	}
	switch a.Status {
	case basketActionReserved:
		a.Status = basketActionNoFill
		a.HoldReason = "the action was reserved but never marked submitting, so no order was sent"
		a.UpdatedAtMs = in.Now.UnixMilli()
		s := bc.state.Strategies[a.StrategyID]
		intent := bc.intentFor(s, a)
		if intent != nil {
			bc.releaseIntent(rt, sc, s, intent, entryIntentReserved, a.HoldReason, in.Now)
		}
		bc.logf(rt, "action %s: %s", a.ActionID, a.HoldReason)
		return bc.persistProgress(rt)
	case basketActionSettling:
		return bc.settleAction(rt, rb, a, basketActionObservation(a), in)
	}
	venue, err := bc.venueFor(sc)
	if err != nil {
		bc.actionAlert(rt, rb, a, "venue|"+err.Error(), err.Error(), true)
		return false
	}
	var outcome basketVenueOutcome
	queried := *a
	bc.unlocked(func() { outcome, err = venue.Query(&queried) })
	if err != nil {
		bc.logf(rt, "action %s (client %s) is still unresolved: the venue query failed: %v; new risk holds", a.ActionID, a.ClientOrderID, err)
		return false
	}
	if outcome.Outcome == basketVenueUnknown {
		bc.logf(rt, "action %s (client %s) is still unresolved: %s; new risk holds", a.ActionID, a.ClientOrderID, outcome.Detail)
		return false
	}
	ok = bc.applyOutcome(rt, rb, a, outcome, basketActionObservation(a), in, true)
	return ok && !basketActionPending(a.Status)
}

func basketStartupLines(runtimes map[string]*basketRuntime) []string {
	ids := make([]string, 0, len(runtimes))
	for id := range runtimes {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	var out []string
	for _, id := range ids {
		rt := runtimes[id]
		if rt.isPair() {
			out = append(out, pairStartupLine(rt))
			continue
		}
		line := fmt.Sprintf("[basket] %s (%s): %d members, ranker %s, rebalance every %ds, gross $%.2f, net limit $%.2f, per-coin cap $%.2f",
			id, partitionLabel(rt.Partition), len(rt.Contract.Members), rt.Contract.Ranker, rt.Contract.RebalanceIntervalSeconds,
			rt.Contract.GrossBudgetUSD, rt.Contract.MaxTransitionNetUSD, rt.Contract.MaxCoinNotionalUSD)
		if rb := rt.latest(); rb != nil {
			line += fmt.Sprintf("; last deadline %s is %s", formatClosedBarMs(rb.DeadlineMs), rb.Status)
		}
		if a := rt.pendingAction(); a != nil {
			line += fmt.Sprintf("; PENDING %s action %s (%s %s %s)", a.purpose(), a.ActionID, a.StrategyID, a.Direction, a.Status)
		}
		if rt.Contract.HoldingHorizonSeconds > 0 {
			open := 0
			for _, l := range rt.Lifecycle {
				if l.active() {
					open++
				}
			}
			line += fmt.Sprintf("; holding horizon %ds, %d open lifecycle(s)", rt.Contract.HoldingHorizonSeconds, open)
		}
		out = append(out, line)
	}
	return out
}

func (bc *basketCoordinator) adjustDelay(delay time.Duration, now time.Time) time.Duration {
	d := bc.wakeDelay(now)
	if d < 0 || d >= delay {
		return delay
	}
	if d < time.Second {
		d = time.Second
	}
	return d
}

func (bc *basketCoordinator) memberCoins() []string {
	var out []string
	if !bc.active() {
		return out
	}
	for _, rt := range bc.runtimes {
		if rt.isPair() {
			continue
		}
		for _, m := range rt.Contract.Members {
			out = append(out, m.Coin)
		}
	}
	sort.Strings(out)
	return out
}

func snapshotHasFreshMids(s *marketSnapshot, coins []string) bool {
	for _, c := range coins {
		if _, ok := s.midFor(c); !ok {
			return false
		}
	}
	return true
}

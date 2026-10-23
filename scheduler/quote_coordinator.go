package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"
)

const (
	quoteShutdownCleanupBudget = 10 * time.Second
	quoteCloseSlippagePct      = 1.0
)

type quoteDeadlineRequest struct {
	DeadlineMs int64
	Marks      []quoteDeadlineMark
	Funding    *paperFundingInputs
	Verdict    *quoteInputVerdict
	Decide     quoteDecisionFunc
}

type quoteDeadlineResult struct {
	SettlementKind string
	Revision       int64
	Generation     int64
	HoldReasons    []string
	SettlementHold []string
}

type quoteEnvelope struct {
	SettledQty      float64  `json:"settled_qty"`
	BaseQty         float64  `json:"base_qty"`
	MinQty          float64  `json:"min_qty"`
	MaxQty          float64  `json:"max_qty"`
	Mark            float64  `json:"mark"`
	ConservativePx  float64  `json:"conservative_px"`
	RequiredMargin  float64  `json:"required_margin"`
	AvailableMargin float64  `json:"available_margin"`
	FeeReserve      float64  `json:"fee_reserve"`
	Refusals        []string `json:"refusals,omitempty"`
}

type quoteSnapshotSide struct {
	RowID        int64             `json:"row_id"`
	Role         string            `json:"role"`
	Generation   int64             `json:"generation"`
	Request      quoteOrderRequest `json:"request"`
	Status       string            `json:"status"`
	CancelState  string            `json:"cancel_state,omitempty"`
	ClientID     string            `json:"client_order_id"`
	VenueOrderID string            `json:"venue_order_id,omitempty"`
	AcceptedAtMs int64             `json:"accepted_at_ms,omitempty"`
	ConfirmedQty string            `json:"confirmed_qty"`
	AppliedQty   string            `json:"applied_qty"`
}

type quoteSnapshotStop struct {
	RowID     int64  `json:"row_id"`
	ClientID  string `json:"client_order_id"`
	Side      string `json:"side"`
	TriggerPx string `json:"trigger_px"`
	Size      string `json:"size"`
	Status    string `json:"status"`
}

type quoteSnapshot struct {
	OwnerID                string              `json:"owner_id"`
	Partition              string              `json:"partition"`
	SourceRole             storageRole         `json:"source_role"`
	Coin                   string              `json:"coin"`
	LastDeadlineMs         int64               `json:"last_deadline_ms"`
	Revision               int64               `json:"revision"`
	PolicyFingerprint      string              `json:"policy_fingerprint"`
	ConfigFingerprint      string              `json:"config_fingerprint"`
	Inventory              string              `json:"inventory"`
	InventoryProjected     float64             `json:"inventory_projected"`
	AvgCost                float64             `json:"avg_cost"`
	Cash                   float64             `json:"cash"`
	FirstOpenMs            int64               `json:"first_open_ms,omitempty"`
	RiskAnchorPx           string              `json:"risk_anchor_px,omitempty"`
	Generation             int64               `json:"generation"`
	GenerationDeadlineMs   int64               `json:"generation_deadline_ms,omitempty"`
	Sides                  []quoteSnapshotSide `json:"sides"`
	Envelope               *quoteEnvelope      `json:"envelope,omitempty"`
	Holds                  []string            `json:"holds,omitempty"`
	SettlementHolds        []string            `json:"settlement_holds,omitempty"`
	Stop                   *quoteSnapshotStop  `json:"stop,omitempty"`
	AppliedBoundaryMs      int64               `json:"applied_boundary_ms"`
	FundingCoverageStartMs int64               `json:"funding_coverage_start_ms,omitempty"`
	FundingHeld            []paperFundingHeld  `json:"funding_held,omitempty"`
	FundingGaps            []paperFundingGap   `json:"funding_gaps,omitempty"`
	UnappliedFills         int                 `json:"unapplied_fills"`
	UnresolvedOrders       int                 `json:"unresolved_orders"`
	OperatorHold           bool                `json:"operator_hold"`
	OperatorHoldReason     string              `json:"operator_hold_reason,omitempty"`
	ContractCreated        bool                `json:"contract_created"`
}

func RunDeadline(ctx context.Context, adm *quoteAdmission, req quoteDeadlineRequest) (quoteDeadlineResult, error) {
	if adm == nil {
		return quoteDeadlineResult{}, fmt.Errorf("quote deadlines need a bound admission")
	}
	return adm.runDeadline(ctx, req)
}

func RecoverOnStart(ctx context.Context, adm *quoteAdmission) (quoteDeadlineResult, error) {
	if adm == nil {
		return quoteDeadlineResult{}, fmt.Errorf("quote startup recovery needs a bound admission")
	}
	return adm.recoverOnStart(ctx)
}

func (a *quoteAdmission) coin() string { return quoteCoinOf(a.sc) }

func (a *quoteAdmission) ownerKey() (quoteOwnerKey, error) {
	return a.db.quoteOwnerKey(a.sc.ID, a.partition)
}

func (a *quoteAdmission) loadOwnerState() (*quoteOwnerState, error) {
	k, err := a.ownerKey()
	if err != nil {
		return nil, err
	}
	d, err := a.db.loadQuoteDurableOwner(k)
	if err != nil {
		return nil, err
	}
	q := &quoteOwnerState{
		Partition:   a.partition,
		Coin:        a.coin(),
		Provider:    a.venue.Provider(),
		AccountKey:  a.venue.AccountKey(a.partition),
		Namespace:   a.venue.Namespace(),
		Contract:    d.Contract,
		Orders:      d.Orders,
		Generations: d.Generations,
		Unapplied:   d.Unapplied,
		Funding:     d.Funding,
		Settlements: d.Settlements,
	}
	if d.Contract != nil {
		q.Revision = d.Contract.Revision
		if d.Contract.Provider != q.Provider || d.Contract.AccountKey != q.AccountKey || d.Contract.Namespace != q.Namespace {
			return nil, fmt.Errorf("quote owner %s is bound to %s/%s/%s, the venue now answers for %s/%s/%s; refusing an account change", a.sc.ID,
				d.Contract.Provider, d.Contract.AccountKey, d.Contract.Namespace, q.Provider, q.AccountKey, q.Namespace)
		}
		if !strings.EqualFold(d.Contract.Coin, q.Coin) {
			return nil, fmt.Errorf("quote owner %s is bound to coin %s, the configuration names %s", a.sc.ID, d.Contract.Coin, q.Coin)
		}
	}
	return q, nil
}

func (a *quoteAdmission) attachRuntime() error {
	q, err := a.loadOwnerState()
	if err != nil {
		return err
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	s := a.state.Strategies[a.sc.ID]
	if s == nil {
		s = NewStrategyState(a.sc)
		a.state.Strategies[a.sc.ID] = s
	}
	s.Quote = q
	if q.Contract != nil {
		if q.Contract.PolicyFP != a.policyFP || q.Contract.ConfigFP != a.configFP {
			return fmt.Errorf("quote owner %s saved policy %s and configuration %s fingerprints; the bound admission carries %s and %s",
				a.sc.ID, shortFingerprint(q.Contract.PolicyFP), shortFingerprint(q.Contract.ConfigFP), shortFingerprint(a.policyFP), shortFingerprint(a.configFP))
		}
	}
	return nil
}

func (a *quoteAdmission) queueAlert(msg string, critical bool) {
	level := "WARN"
	if critical {
		level = "CRITICAL"
	}
	line := fmt.Sprintf("[%s] [quote] %s: %s", level, a.sc.ID, msg)
	fmt.Println(line)
	n := a.notifier
	part := a.partition
	if n == nil {
		return
	}
	body := partitionPrefixedDM(part, fmt.Sprintf("**QUOTE %s** [%s] %s", level, a.sc.ID, msg))
	a.opMu.Lock()
	a.outbox = append(a.outbox, func() {
		n.SendToPartitionChannels(part, body)
		if critical && n.HasOwner() {
			n.SendOwnerDM(body)
		}
	})
	a.opMu.Unlock()
}

func (a *quoteAdmission) flushOutbox() {
	a.opMu.Lock()
	out := a.outbox
	a.outbox = nil
	a.opMu.Unlock()
	for _, fn := range out {
		fn()
	}
}

func (a *quoteAdmission) ensureContract(ctx context.Context) error {
	a.mu.RLock()
	s := a.state.Strategies[a.sc.ID]
	exists := s != nil && s.Quote != nil && s.Quote.Contract != nil
	a.mu.RUnlock()
	if exists {
		return nil
	}
	pos, err := a.venue.Position(ctx, a.coin())
	if err != nil {
		return fmt.Errorf("the pre-existing exposure check could not read the venue: %w", err)
	}
	size, err := parseQuoteDecimal(pos.Size)
	if err != nil || size.Sign() != 0 {
		return fmt.Errorf("quote owner %s refuses unverifiable or pre-existing %s exposure %q at contract creation", a.sc.ID, a.coin(), pos.Size)
	}
	if pos.Account != "" && pos.Account != a.venue.AccountKey(a.partition) {
		return fmt.Errorf("the position read answered for account %q", pos.Account)
	}
	now := a.Now()
	nowMs := now.UnixMilli()
	k, err := a.ownerKey()
	if err != nil {
		return err
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	s = a.state.Strategies[a.sc.ID]
	if s == nil {
		s = NewStrategyState(a.sc)
		a.state.Strategies[a.sc.ID] = s
	}
	if len(s.Positions) > 0 {
		return fmt.Errorf("quote owner %s holds a pre-existing book position; quote ownership never adopts inventory", a.sc.ID)
	}
	cash, err := quoteRatFromFloat(s.Cash)
	if err != nil {
		return err
	}
	staged := cloneQuoteStrategy(s)
	if a.partition.Scope == ScopePaper {
		st := newPaperFundingState(nowMs)
		st.Coins[a.coin()] = &paperFundingCoin{Anchor: paperFundingAnchor{SinceMs: nowMs, VerifiedMs: nowMs}, SettledThroughMs: nowMs}
		staged.PaperFunding = st
		staged.paperFundingCorrupt = false
		staged.paperFundingRaw = ""
	}
	book := quoteExactBook{Qty: "0", Cost: "0", Cash: quoteRatString(cash), RealizedGross: "0", Fees: "0", Funding: "0"}
	c := &quoteContractRow{
		Partition: a.partition, StorageID: k.sid, Coin: a.coin(), Provider: a.venue.Provider(), AccountKey: a.venue.AccountKey(a.partition),
		Namespace: a.venue.Namespace(), PolicyFP: a.policyFP, ConfigFP: a.configFP, StartMs: nowMs, Revision: 1,
		AppliedBoundaryMs: nowMs, Book: book, UpdatedAtMs: nowMs,
	}
	flushed, diags, err := a.db.createQuoteContract(k, c, staged, a.partition.Scope)
	if err != nil {
		return err
	}
	for _, idx := range flushed {
		staged.TradeHistory[idx].persisted = true
	}
	staged.ClosedPositions = nil
	staged.pendingTradeDiagnostics = nil
	if staged.Quote == nil {
		staged.Quote = &quoteOwnerState{Partition: a.partition, Coin: a.coin(), Provider: c.Provider, AccountKey: c.AccountKey, Namespace: c.Namespace}
	}
	staged.Quote.Contract = c
	staged.Quote.Revision = 1
	*s = *staged
	enqueueFlushedDiagnostics(diags)
	return nil
}

func (a *quoteAdmission) runDeadline(ctx context.Context, req quoteDeadlineRequest) (quoteDeadlineResult, error) {
	a.opMu.Lock()
	closed, shuttingDown := a.closed, a.shutdown
	a.opMu.Unlock()
	if closed {
		return quoteDeadlineResult{}, fmt.Errorf("the quote admission for %s is closed", a.sc.ID)
	}
	defer a.flushOutbox()
	if req.DeadlineMs <= 0 {
		return quoteDeadlineResult{}, fmt.Errorf("quote deadline must be > 0")
	}
	a.mu.RLock()
	s := a.state.Strategies[a.sc.ID]
	var last int64
	if s != nil && s.Quote != nil && s.Quote.Contract != nil {
		last = s.Quote.Contract.LastDeadlineMs
	}
	a.mu.RUnlock()
	if req.DeadlineMs < last {
		return quoteDeadlineResult{}, fmt.Errorf("quote deadline %d is earlier than the last committed deadline %d", req.DeadlineMs, last)
	}
	if last > 0 && req.DeadlineMs > last {
		a.mu.RLock()
		interval := effectiveStrategyIntervalSeconds(a.sc, s, a.cfg.IntervalSeconds, configuredDrawdownWarnThresholdPct(a.cfg))
		a.mu.RUnlock()
		if interval > 0 && req.DeadlineMs-last < int64(interval)*1000 {
			return quoteDeadlineResult{}, fmt.Errorf("quote deadline %d is not due: the effective cadence is %ds after the last committed deadline %d", req.DeadlineMs, interval, last)
		}
	}
	if err := a.ensureContract(ctx); err != nil {
		return quoteDeadlineResult{}, err
	}
	staged, err := a.stageSettlement(ctx, req.DeadlineMs, req.Funding, req.Marks)
	if err != nil {
		return quoteDeadlineResult{}, err
	}
	for _, msg := range staged.Critical {
		a.alertOnce("settlement|"+msg, msg, true)
	}
	in := quoteDecisionInput{Snapshot: a.snapshotOf(staged.Staged, staged), SettlementHeld: len(staged.Holds) > 0, SettlementHolds: append([]string(nil), staged.Holds...)}
	if in.SettlementHeld {
		in.Snapshot = a.Snapshot()
		in.Snapshot.SettlementHolds = append([]string(nil), staged.Holds...)
	}
	var decision quoteDecision
	var evalErr error
	if req.Decide != nil {
		decision, evalErr = req.Decide(in)
	} else {
		evalErr = errors.New("no decision callback")
	}
	var progress quoteProgressRecord
	if evalErr == nil {
		progress = decision.Progress
	}
	res, err := a.commitStaged(staged, req.DeadlineMs, progress)
	if err != nil {
		if rerr := a.reloadOwner(); rerr != nil {
			return quoteDeadlineResult{}, &quoteReloadError{DeadlineMs: req.DeadlineMs, Cause: fmt.Errorf("%v; %v", err, rerr)}
		}
		return quoteDeadlineResult{}, &quoteReloadError{DeadlineMs: req.DeadlineMs, Cause: err}
	}
	out := quoteDeadlineResult{SettlementKind: res.Summary.SettlementKind, Revision: res.Revision, SettlementHold: staged.Holds}
	var holds []string
	if evalErr != nil {
		holds = append(holds, fmt.Sprintf("evaluation failed: %v", evalErr))
		decision = quoteDecision{}
	}
	if shuttingDown {
		holds = append(holds, "shutdown in progress")
	}
	gen, more := a.applyDeadline(ctx, req, decision, holds, staged)
	out.Generation = gen
	out.HoldReasons = more
	return out, nil
}

func (a *quoteAdmission) recoverOnStart(ctx context.Context) (quoteDeadlineResult, error) {
	defer a.flushOutbox()
	a.mu.RLock()
	s := a.state.Strategies[a.sc.ID]
	has := s != nil && s.Quote != nil && s.Quote.Contract != nil
	a.mu.RUnlock()
	if !has {
		return quoteDeadlineResult{SettlementKind: quoteSettleNone}, nil
	}
	a.resolveUncertain(ctx)
	nowMs := a.Now().UnixMilli()
	a.mu.RLock()
	last := s.Quote.Contract.LastDeadlineMs
	a.mu.RUnlock()
	deadline := last
	if deadline <= 0 {
		deadline = nowMs
	}
	staged, err := a.stageSettlement(ctx, deadline, nil, nil)
	if err != nil {
		return quoteDeadlineResult{}, err
	}
	for _, msg := range staged.Critical {
		a.alertOnce("settlement|"+msg, msg, true)
	}
	if staged.Kind == quoteSettleFunding && len(staged.FillIDs) == 0 && len(staged.FundingIDs) == 0 && a.partition.Scope == ScopePaper {
		staged.Kind = quoteSettleNone
	}
	res, err := a.commitStaged(staged, deadline, nil)
	if err != nil {
		if rerr := a.reloadOwner(); rerr != nil {
			return quoteDeadlineResult{}, &quoteReloadError{DeadlineMs: deadline, Cause: fmt.Errorf("%v; %v", err, rerr)}
		}
		return quoteDeadlineResult{}, &quoteReloadError{DeadlineMs: deadline, Cause: err}
	}
	a.resolveUncertain(ctx)
	a.maintainStop(ctx, a.freshPosition(ctx, staged))
	return quoteDeadlineResult{SettlementKind: res.Summary.SettlementKind, Revision: res.Revision, SettlementHold: staged.Holds}, nil
}

func (a *quoteAdmission) alertOnce(key, msg string, critical bool) {
	a.opMu.Lock()
	seen := a.holdAlerts[key]
	a.holdAlerts[key] = true
	a.opMu.Unlock()
	if !seen {
		a.queueAlert(msg, critical)
	}
}

func (a *quoteAdmission) ownerSnapshot() (*StrategyState, *quoteOwnerState) {
	a.mu.RLock()
	defer a.mu.RUnlock()
	s := a.state.Strategies[a.sc.ID]
	if s == nil || s.Quote == nil {
		return nil, nil
	}
	return cloneQuoteStrategy(s), s.Quote.clone()
}

func (a *quoteAdmission) markFor(marks []quoteDeadlineMark, deadlineMs int64) (float64, bool) {
	for _, m := range marks {
		if strings.EqualFold(m.Coin, a.coin()) && quoteMarkFresh(m, deadlineMs) {
			return m.Price, true
		}
	}
	return 0, false
}

func (a *quoteAdmission) increaseHolds(req quoteDeadlineRequest, extra []string, staged *quoteStaged, s *StrategyState, q *quoteOwnerState) []string {
	holds := append([]string(nil), extra...)
	holds = append(holds, quoteVerdictHoldReasons(req.Verdict, a.sc.ID, a.configFP, a.policyFP, req.DeadlineMs)...)
	if len(staged.Holds) > 0 {
		holds = append(holds, "settlement held: "+strings.Join(staged.Holds, "; "))
	}
	now := a.Now()
	if a.sc.Paused {
		holds = append(holds, "strategy paused")
	}
	if s.RiskState.CircuitBreaker && now.Before(s.RiskState.CircuitBreakerUntil) {
		holds = append(holds, "circuit breaker active")
	}
	a.mu.RLock()
	prs := a.state.PortfolioRisk[a.partition]
	killed := prs != nil && prs.KillSwitchActive
	pr := partitionRiskConfig(a.cfg, a.partition)
	states := filterStatesByPartition(a.state.Strategies, a.cfg.Strategies, a.partition)
	daily := evaluateDailyLossLimit(pr, states, strategiesInPartition(a.cfg.Strategies, a.partition), now)
	a.mu.RUnlock()
	if killed {
		holds = append(holds, "portfolio kill switch latched")
	}
	if daily.Tripped {
		holds = append(holds, "daily loss limit tripped: "+dailyLossHoldDetail(daily))
	}
	if a.store.persistenceHoldsPartition(a.partition) {
		holds = append(holds, "the partition state file has not saved")
	}
	if q.Contract != nil && q.Contract.OperatorHold {
		holds = append(holds, "operator hold: "+q.Contract.OperatorHoldReason)
	}
	if _, ok := a.markFor(req.Marks, req.DeadlineMs); !ok {
		holds = append(holds, "the deadline mark is missing or stale")
	}
	if q.Contract != nil && mustQuoteRat(q.Contract.Book.Qty).Sign() != 0 {
		stop := q.currentStop()
		if stop == nil || stop.SubmitState != quoteSubmitResting || stop.CancelState != quoteCancelNone {
			holds = append(holds, "nonzero inventory has no confirmed resting stop")
		}
	}
	for _, o := range q.Orders {
		if o.SubmitState == quoteSubmitUnknown || o.CancelState == quoteCancelUnknown {
			holds = append(holds, fmt.Sprintf("%s order %d has an unknown outcome", o.Role, o.RowID))
		}
	}
	sort.Strings(holds)
	return dedupeStrings(holds)
}

func dedupeStrings(in []string) []string {
	var out []string
	for i, s := range in {
		if i > 0 && s == in[i-1] {
			continue
		}
		out = append(out, s)
	}
	return out
}

func (a *quoteAdmission) freshPosition(ctx context.Context, staged *quoteStaged) *quotePositionRead {
	if staged != nil && staged.PositionKnown {
		p := staged.Position
		return &p
	}
	pos, err := a.venue.Position(ctx, a.coin())
	if err != nil {
		a.alertOnce("position|"+err.Error(), fmt.Sprintf("strict position read failed: %v", err), false)
		return nil
	}
	return &pos
}

func (a *quoteAdmission) applyDeadline(ctx context.Context, req quoteDeadlineRequest, decision quoteDecision, extra []string, staged *quoteStaged) (int64, []string) {
	a.resolveUncertain(ctx)
	pos := a.freshPosition(ctx, staged)
	a.maintainStop(ctx, pos)
	s, q := a.ownerSnapshot()
	if s == nil || q == nil || q.Contract == nil {
		return 0, []string{"quote owner has no durable contract"}
	}
	holds := a.increaseHolds(req, extra, staged, s, q)
	mark, markOK := a.markFor(req.Marks, req.DeadlineMs)
	inv := mustQuoteRat(q.Contract.Book.Qty)
	exit := decision.Exit
	if markOK && inv.Sign() != 0 && a.sc.Quote != nil {
		if notional := math.Abs(quoteRatFloat(inv)) * mark; notional > a.sc.Quote.MaxInventoryUSD {
			holds = append(holds, fmt.Sprintf("marked inventory $%.2f is above the $%.2f cap", notional, a.sc.Quote.MaxInventoryUSD))
			if exit.Kind != quoteExitKindExit {
				exit = quoteExitRequest{Kind: quoteExitKindExit, ExitReason: quoteExitReasonCap}
			}
		}
	}
	if a.killSwitchLatched() && inv.Sign() != 0 && exit.Kind != quoteExitKindExit {
		exit = quoteExitRequest{Kind: quoteExitKindExit, ExitReason: "kill_switch"}
	}
	if exit.Kind == quoteExitKindExit {
		holds = append(holds, "elective inventory exit requested ("+exit.ExitReason+")")
	}
	held := len(holds) > 0
	var bid, ask *quoteSideRequest
	if exit.Kind == quoteExitKindReducing {
		bid, ask = exit.ReducingBid, exit.ReducingAsk
		if bid != nil {
			b := *bid
			b.ReduceOnly = true
			bid = &b
		}
		if ask != nil {
			c := *ask
			c.ReduceOnly = true
			ask = &c
		}
		if bid != nil && inv.Sign() >= 0 {
			holds = append(holds, "a reducing bid needs short inventory")
			bid = nil
		}
		if ask != nil && inv.Sign() <= 0 {
			holds = append(holds, "a reducing ask needs long inventory")
			ask = nil
		}
	} else if exit.Kind != quoteExitKindExit && !held {
		bid, ask = decision.Desired.Bid, decision.Desired.Ask
		if (bid != nil || ask != nil) && decision.Desired.PolicyFingerprint != a.policyFP {
			holds = append(holds, "the desired pair names an incompatible policy fingerprint")
			bid, ask = nil, nil
		}
	}
	if held || exit.Kind == quoteExitKindExit {
		a.cancelRiskIncreasing(ctx, q)
	}
	gen, genHolds := a.coordinateGeneration(ctx, req, q, s, bid, ask, mark, markOK, pos)
	holds = append(holds, genHolds...)
	if exit.Kind == quoteExitKindExit {
		holds = append(holds, a.electiveExit(ctx, req, exit, mark, markOK, pos)...)
	}
	sort.Strings(holds)
	holds = dedupeStrings(holds)
	a.mu.Lock()
	if live := a.state.Strategies[a.sc.ID]; live != nil && live.Quote != nil {
		live.Quote.LastHolds = append([]string(nil), holds...)
	}
	a.mu.Unlock()
	return gen, holds
}

func (a *quoteAdmission) refreshOwner() {
	q, err := a.loadOwnerState()
	if err != nil {
		a.queueAlert(fmt.Sprintf("quote owner state reload failed: %v", err), true)
		return
	}
	a.mu.Lock()
	if live := a.state.Strategies[a.sc.ID]; live != nil && live.Quote != nil {
		q.LastHolds = live.Quote.LastHolds
		q.SettlementHolds = live.Quote.SettlementHolds
		q.Envelope = live.Quote.Envelope
		q.Revision = live.Quote.Revision
		if q.Contract != nil && live.Quote.Contract != nil && live.Quote.Contract.Revision > q.Contract.Revision {
			q.Contract = live.Quote.Contract
		}
		live.Quote = q
	}
	a.mu.Unlock()
}

func (a *quoteAdmission) saveOutcome(o *quoteOrderRow, from []string) error {
	k, err := a.ownerKey()
	if err != nil {
		return err
	}
	return a.db.updateQuoteOrder(k, o, from, a.Now().UnixMilli())
}

func quoteOutcomeJSON(out quoteOrderOutcome) string {
	b, _ := json.Marshal(out)
	return string(b)
}

func (a *quoteAdmission) applyOutcome(o *quoteOrderRow, out quoteOrderOutcome, cancel bool) {
	o.OutcomeJSON = quoteOutcomeJSON(out)
	if out.VenueOrderID != "" {
		o.VenueOrderID = out.VenueOrderID
	}
	if out.FilledQty != "" {
		if fq, err := parseQuoteDecimal(out.FilledQty); err == nil && fq.Cmp(mustQuoteRat(o.ConfirmedQty)) > 0 {
			o.ConfirmedQty = quoteRatString(fq)
		}
	}
	switch out.Status {
	case quoteOutcomeResting:
		o.SubmitState = quoteSubmitResting
		if out.AcceptedAtMs > 0 {
			o.AcceptedAtMs = out.AcceptedAtMs
		}
		if cancel {
			o.CancelState = quoteCancelRequested
		}
	case quoteOutcomeFilled:
		o.SubmitState = quoteSubmitFilled
		o.TerminalAtMs = out.VenueTimeMs
		if cancel {
			o.CancelState = quoteCancelConfirmed
		}
	case quoteOutcomeTriggered:
		o.SubmitState = quoteSubmitTriggered
	case quoteOutcomeRejected:
		o.SubmitState = quoteSubmitRejected
		o.TerminalAtMs = out.VenueTimeMs
	case quoteOutcomeCancelled:
		o.SubmitState = quoteSubmitCancelled
		o.TerminalAtMs = out.VenueTimeMs
		o.CancelState = quoteCancelConfirmed
	case quoteOutcomeAbsent:
		o.SubmitState = quoteSubmitUnknown
		if cancel {
			o.CancelState = quoteCancelUnknown
		}
	default:
		if cancel {
			o.CancelState = quoteCancelUnknown
		} else {
			o.SubmitState = quoteSubmitUnknown
		}
	}
}

func (a *quoteAdmission) resolveUncertain(ctx context.Context) {
	a.refreshOwner()
	_, q := a.ownerSnapshot()
	if q == nil {
		return
	}
	nowMs := a.Now().UnixMilli()
	for _, o := range q.Orders {
		switch {
		case o.SubmitState == quoteSubmitReserved:
			if nowMs >= o.RequestExpiryMs {
				o.SubmitState = quoteSubmitNeverSent
				o.TerminalAtMs = nowMs
				o.OutcomeJSON = quoteOutcomeJSON(quoteOrderOutcome{Status: quoteSubmitNeverSent, Detail: "the request expired before it was sent", VenueTimeMs: nowMs})
				if err := a.saveOutcome(o, []string{quoteSubmitReserved}); err != nil {
					a.queueAlert(fmt.Sprintf("order %d expiry could not be saved: %v", o.RowID, err), true)
				}
			}
		case o.SubmitState == quoteSubmitSubmitting || o.SubmitState == quoteSubmitUnknown || o.SubmitState == quoteSubmitResting || o.SubmitState == quoteSubmitTriggered || o.CancelState == quoteCancelUnknown || o.CancelState == quoteCancelRequested:
			from := o.SubmitState
			out, err := a.venue.Status(ctx, o.Request, o.VenueOrderID)
			if err != nil {
				out = quoteOrderOutcome{Status: quoteOutcomeUnknown, Detail: err.Error()}
			}
			if out.Status == quoteOutcomeAbsent && (from == quoteSubmitSubmitting || from == quoteSubmitUnknown) {
				boundary := int64(0)
				if q.Contract != nil {
					boundary = q.Contract.AppliedBoundaryMs
				}
				grace := entryRecoveryGrace.Milliseconds()
				if nowMs > o.RequestExpiryMs+grace && boundary >= o.RequestExpiryMs+grace && mustQuoteRat(o.ConfirmedQty).Sign() == 0 {
					o.SubmitState = quoteSubmitNeverSent
					o.TerminalAtMs = nowMs
					o.OutcomeJSON = quoteOutcomeJSON(quoteOrderOutcome{Status: quoteSubmitNeverSent, Detail: "absent after the request expiry plus grace with fill coverage through it", VenueTimeMs: nowMs})
				} else {
					o.SubmitState = quoteSubmitUnknown
					o.OutcomeJSON = quoteOutcomeJSON(out)
				}
			} else {
				cancel := o.CancelState == quoteCancelRequested || o.CancelState == quoteCancelUnknown || o.CancelState == quoteCancelSent
				a.applyOutcome(o, out, cancel)
			}
			if err := a.saveOutcome(o, []string{from}); err != nil {
				a.queueAlert(fmt.Sprintf("order %d outcome could not be saved: %v", o.RowID, err), true)
			}
			if o.SubmitState == quoteSubmitUnknown {
				a.alertOnce(fmt.Sprintf("unknown|%d", o.RowID), fmt.Sprintf("%s order %d (client %s) has an unknown venue outcome; replacement is blocked until recovery proves it", o.Role, o.RowID, o.ClientOrderID), true)
			}
		}
	}
	a.refreshOwner()
	_, q = a.ownerSnapshot()
	if q == nil {
		return
	}
	for _, o := range q.Orders {
		if o.CancelState == quoteCancelRequested && o.mayRest() {
			a.sendCancel(ctx, o)
		}
	}
	a.refreshOwner()
}

func (a *quoteAdmission) requestCancel(o *quoteOrderRow) bool {
	if o.CancelState != quoteCancelNone || !o.mayRest() {
		return o.CancelState == quoteCancelRequested
	}
	from := o.SubmitState
	o.CancelState = quoteCancelRequested
	if err := a.saveOutcome(o, []string{from}); err != nil {
		a.queueAlert(fmt.Sprintf("cancel intent for order %d could not be saved: %v", o.RowID, err), true)
		return false
	}
	return true
}

func (a *quoteAdmission) sendCancel(ctx context.Context, o *quoteOrderRow) {
	from := o.SubmitState
	out, err := a.venue.Cancel(ctx, o.Request, o.VenueOrderID)
	if err != nil {
		out = quoteOrderOutcome{Status: quoteOutcomeUnknown, Detail: err.Error()}
	}
	a.applyOutcome(o, out, true)
	if err := a.saveOutcome(o, []string{from}); err != nil {
		a.queueAlert(fmt.Sprintf("cancel outcome for order %d could not be saved: %v", o.RowID, err), true)
	}
	if o.CancelState == quoteCancelUnknown {
		a.alertOnce(fmt.Sprintf("cancel-unknown|%d", o.RowID), fmt.Sprintf("cancel of %s order %d (client %s) has an unknown outcome", o.Role, o.RowID, o.ClientOrderID), true)
	}
}

func (a *quoteAdmission) cancelRiskIncreasing(ctx context.Context, q *quoteOwnerState) {
	for _, o := range q.Orders {
		if o.increasesRisk() && o.mayRest() && o.CancelState == quoteCancelNone {
			if a.requestCancel(o) {
				a.sendCancel(ctx, o)
			}
		}
	}
	a.refreshOwner()
}

func (a *quoteAdmission) quoteRequestFor(role string, side *quoteSideRequest, nowMs int64) (quoteOrderRequest, error) {
	cid, err := a.venue.NewClientOrderID()
	if err != nil {
		return quoteOrderRequest{}, err
	}
	sideName := "buy"
	if role == quoteRoleAsk {
		sideName = "sell"
	}
	req := quoteOrderRequest{Coin: a.coin(), Role: role, Side: sideName, PostOnly: true, TIF: "Alo", ClientOrderID: cid, ExpiryMs: nowMs + a.sc.Quote.RequestExpiryMs}
	if side == nil {
		req.Px, req.Size = "0", "0"
		return req, nil
	}
	px, err := parseQuoteDecimal(side.Px)
	if err != nil || px.Sign() <= 0 {
		return req, fmt.Errorf("desired %s price %q is not a positive decimal", role, side.Px)
	}
	sz, err := parseQuoteDecimal(side.Size)
	if err != nil || sz.Sign() <= 0 {
		return req, fmt.Errorf("desired %s size %q is not a positive decimal", role, side.Size)
	}
	if maxQty, _ := quoteRatFromFloat(a.sc.Quote.MaxOrderQty); maxQty != nil && sz.Cmp(maxQty) > 0 {
		return req, fmt.Errorf("desired %s size %s is above max_order_qty %g", role, side.Size, a.sc.Quote.MaxOrderQty)
	}
	req.Px = quoteDecimalText(px)
	req.Size = quoteDecimalText(sz)
	req.ReduceOnly = side.ReduceOnly
	if err := a.prices.check(req); err != nil {
		return req, err
	}
	return req, nil
}

func (a *quoteAdmission) coordinateGeneration(ctx context.Context, req quoteDeadlineRequest, q *quoteOwnerState, s *StrategyState, bid, ask *quoteSideRequest, mark float64, markOK bool, pos *quotePositionRead) (int64, []string) {
	var holds []string
	nowMs := a.Now().UnixMilli()
	if g := q.generationForDeadline(req.DeadlineMs); g != nil {
		a.submitReserved(ctx, q, g.Generation, s, mark, pos)
		return g.Generation, nil
	}
	var latest *quoteGeneration
	for _, g := range q.Generations {
		if latest == nil || g.Generation > latest.Generation {
			latest = g
		}
	}
	if latest != nil {
		sides := q.sidesOf(latest.Generation)
		resolved := true
		for _, o := range sides {
			if !o.resolved() {
				resolved = false
			}
		}
		if !resolved {
			keepAll := true
			for _, o := range sides {
				want := bid
				if o.Role == quoteRoleAsk {
					want = ask
				}
				if o.terminal() {
					if want != nil {
						keepAll = false
					}
					continue
				}
				cur := &quoteSideRequest{Px: o.Request.Px, Size: o.Request.Size, ReduceOnly: o.Request.ReduceOnly}
				if want != nil && cur.same(want) && o.CancelState == quoteCancelNone {
					continue
				}
				keepAll = false
				if o.mayRest() && o.CancelState == quoteCancelNone {
					if a.requestCancel(o) {
						a.sendCancel(ctx, o)
					}
				}
			}
			a.refreshOwner()
			if !keepAll {
				holds = append(holds, fmt.Sprintf("generation %d is not resolved; the replacement waits for terminal proof and settlement", latest.Generation))
			}
			return latest.Generation, holds
		}
	}
	if bid == nil && ask == nil {
		if latest != nil {
			return latest.Generation, nil
		}
		return 0, nil
	}
	if !markOK {
		return 0, []string{"no fresh mark for the risk envelope"}
	}
	next := q.Contract.LastGeneration + 1
	if latest != nil && latest.Generation >= next {
		next = latest.Generation + 1
	}
	var rows []*quoteOrderRow
	var refusals []string
	for _, role := range []string{quoteRoleBid, quoteRoleAsk} {
		want := bid
		if role == quoteRoleAsk {
			want = ask
		}
		r, err := a.quoteRequestFor(role, want, nowMs)
		if err != nil {
			refusals = append(refusals, err.Error())
			continue
		}
		state := quoteSubmitReserved
		if want == nil {
			state = quoteSubmitNeverSent
		}
		rows = append(rows, &quoteOrderRow{Coin: a.coin(), Side: r.Side, Role: role, Generation: next, Provider: q.Provider, AccountKey: q.AccountKey,
			Namespace: q.Namespace, Request: r, RequestJSON: r.canonicalJSON(), RequestExpiryMs: r.ExpiryMs, ClientOrderID: r.ClientOrderID,
			SubmitState: state, ConfirmedQty: "0", AppliedQty: "0"})
	}
	if len(refusals) > 0 {
		return 0, refusals
	}
	env := a.envelope(q, s, rows, mark, pos)
	a.mu.Lock()
	if live := a.state.Strategies[a.sc.ID]; live != nil && live.Quote != nil {
		live.Quote.Envelope = env
	}
	a.mu.Unlock()
	if len(env.Refusals) > 0 {
		return 0, env.Refusals
	}
	desired, _ := json.Marshal(map[string]interface{}{"bid": bid, "ask": ask})
	reservation, _ := json.Marshal(env)
	gen := &quoteGeneration{Generation: next, DeadlineMs: req.DeadlineMs, Status: quoteGenReserved, DesiredJSON: string(desired), ReservationJSON: string(reservation)}
	k, err := a.ownerKey()
	if err != nil {
		return 0, []string{err.Error()}
	}
	if err := a.db.reserveQuoteOrders(k, &quoteReservation{ExpectRevision: q.Revision, Generation: gen, Orders: rows, DeadlineMs: req.DeadlineMs}, nowMs); err != nil {
		return 0, []string{fmt.Sprintf("the generation reservation failed: %v", err)}
	}
	a.refreshOwner()
	_, q2 := a.ownerSnapshot()
	if q2 == nil {
		return next, nil
	}
	a.submitReserved(ctx, q2, next, s, mark, pos)
	return next, nil
}

func (a *quoteAdmission) submitReserved(ctx context.Context, q *quoteOwnerState, generation int64, s *StrategyState, mark float64, pos *quotePositionRead) {
	sides := q.sidesOf(generation)
	sort.Slice(sides, func(i, j int) bool { return sides[i].Role < sides[j].Role })
	var accepted []*quoteOrderRow
	failed := false
	for _, o := range sides {
		if o.SubmitState != quoteSubmitReserved {
			if o.SubmitState == quoteSubmitResting || o.SubmitState == quoteSubmitFilled {
				accepted = append(accepted, o)
			}
			if o.SubmitState == quoteSubmitRejected || o.SubmitState == quoteSubmitUnknown {
				failed = true
			}
			continue
		}
		nowMs := a.Now().UnixMilli()
		a.opMu.Lock()
		stopping := a.shutdown
		a.opMu.Unlock()
		if failed || stopping || nowMs >= o.RequestExpiryMs {
			o.SubmitState = quoteSubmitNeverSent
			o.TerminalAtMs = nowMs
			detail := "the request expired before submission"
			if failed {
				detail = "the other side of the pair failed, so this side was not sent"
			}
			if stopping {
				detail = "shutdown stopped new quote intents"
			}
			o.OutcomeJSON = quoteOutcomeJSON(quoteOrderOutcome{Status: quoteSubmitNeverSent, Detail: detail, VenueTimeMs: nowMs})
			if err := a.saveOutcome(o, []string{quoteSubmitReserved}); err != nil {
				a.queueAlert(fmt.Sprintf("order %d could not be released: %v", o.RowID, err), true)
			}
			continue
		}
		recheck := ""
		if err := a.prices.check(o.Request); err != nil {
			recheck = err.Error()
		} else if o.increasesRisk() {
			if mark <= 0 {
				recheck = "no fresh mark to recheck the risk envelope before submission"
			} else if env := a.envelope(q, s, nil, mark, pos); len(env.Refusals) > 0 {
				recheck = "the risk envelope changed before submission: " + strings.Join(env.Refusals, "; ")
			}
		}
		if recheck != "" {
			o.SubmitState = quoteSubmitNeverSent
			o.TerminalAtMs = nowMs
			o.OutcomeJSON = quoteOutcomeJSON(quoteOrderOutcome{Status: quoteSubmitNeverSent, Detail: recheck, VenueTimeMs: nowMs})
			_ = a.saveOutcome(o, []string{quoteSubmitReserved})
			failed = true
			continue
		}
		o.SubmitState = quoteSubmitSubmitting
		if err := a.saveOutcome(o, []string{quoteSubmitReserved}); err != nil {
			a.queueAlert(fmt.Sprintf("order %d submitting state could not be saved; nothing was sent: %v", o.RowID, err), true)
			failed = true
			continue
		}
		out, err := a.venue.Place(ctx, o.Request)
		if err != nil {
			out = quoteOrderOutcome{Status: quoteOutcomeUnknown, Detail: err.Error()}
		}
		a.applyOutcome(o, out, false)
		if err := a.saveOutcome(o, []string{quoteSubmitSubmitting}); err != nil {
			a.queueAlert(fmt.Sprintf("order %d outcome %s could not be saved; recovery will query it: %v", o.RowID, out.Status, err), true)
		}
		switch o.SubmitState {
		case quoteSubmitResting, quoteSubmitFilled:
			accepted = append(accepted, o)
		case quoteSubmitUnknown:
			failed = true
			a.alertOnce(fmt.Sprintf("unknown|%d", o.RowID), fmt.Sprintf("%s order %d (client %s) has an unknown submission outcome; it is never resent", o.Role, o.RowID, o.ClientOrderID), true)
		default:
			failed = true
		}
	}
	if failed {
		for _, o := range accepted {
			if o.mayRest() && o.CancelState == quoteCancelNone && o.increasesRisk() {
				if a.requestCancel(o) {
					a.sendCancel(ctx, o)
				}
			}
		}
	}
	k, err := a.ownerKey()
	if err == nil {
		tx, terr := a.db.beginQuoteTx()
		if terr == nil {
			_, _ = tx.Exec(`UPDATE quote_generations SET status = 'active', updated_at_ms = ? WHERE scope = ? AND source = ? AND strategy_id = ? AND generation = ? AND status = 'reserved'`,
				a.Now().UnixMilli(), string(k.Partition.Scope), k.Partition.Source, k.sid, generation)
			if cerr := a.db.commitQuoteTx(tx); cerr != nil {
				tx.Rollback()
			}
		}
	}
	a.refreshOwner()
}

func (a *quoteAdmission) envelope(q *quoteOwnerState, s *StrategyState, proposed []*quoteOrderRow, mark float64, pos *quotePositionRead) *quoteEnvelope {
	env := &quoteEnvelope{Mark: mark}
	settled := quoteRatFloat(mustQuoteRat(q.Contract.Book.Qty))
	env.SettledQty = settled
	base := settled
	var buys, sells, orderNotional float64
	buffer := 0.0
	if a.sc.Quote != nil {
		buffer = a.sc.Quote.MarginPriceBufferPct
	}
	px := mark * (1 + buffer/100)
	env.ConservativePx = px
	consider := func(o *quoteOrderRow, proposedRow bool) {
		pending := quoteRatSub(mustQuoteRat(o.ConfirmedQty), mustQuoteRat(o.AppliedQty))
		if pending.Sign() > 0 {
			if o.Side == "buy" {
				base += quoteRatFloat(pending)
			} else {
				base -= quoteRatFloat(pending)
			}
		}
		live := o.mayRest() || o.SubmitState == quoteSubmitReserved || (proposedRow && o.SubmitState == quoteSubmitReserved)
		if !live || o.Request.ReduceOnly || o.Role == quoteRoleStop || o.Role == quoteRoleClose {
			return
		}
		rem := quoteRatFloat(o.remainingQty())
		if o.Side == "buy" {
			buys += rem
		} else {
			sells += rem
		}
		orderNotional += rem * px
	}
	for _, o := range q.Orders {
		consider(o, false)
	}
	for _, o := range proposed {
		consider(o, true)
	}
	env.BaseQty = base
	env.MinQty = base - sells
	env.MaxQty = base + buys
	feeRate := 0.0
	if a.sc.Quote != nil {
		feeRate = math.Max(a.sc.Quote.FeeReserveRate, 0)
	}
	env.FeeReserve = orderNotional * feeRate
	lev := EffectiveExchangeLeverage(a.sc)
	if lev <= 0 {
		lev = 1
	}
	env.AvailableMargin = s.Cash
	if pos != nil && pos.FreeMargin != "" {
		if fm, err := parseQuoteDecimal(pos.FreeMargin); err == nil {
			if f := quoteRatFloat(fm); f < env.AvailableMargin {
				env.AvailableMargin = f
			}
		}
	}
	pr := partitionRiskConfig(a.cfg, a.partition)
	a.mu.RLock()
	states := filterStatesByPartition(a.state.Strategies, a.cfg.Strategies, a.partition)
	cfgs := strategiesInPartition(a.cfg.Strategies, a.partition)
	var pv float64
	for _, st := range states {
		pv += displayStrategyValue(st, nil)
	}
	a.mu.RUnlock()
	prices := map[string]float64{a.coin(): px}
	for _, extreme := range []float64{env.MinQty, env.MaxQty} {
		notional := math.Abs(extreme) * px
		if a.sc.Quote != nil && notional > a.sc.Quote.MaxInventoryUSD {
			env.Refusals = append(env.Refusals, fmt.Sprintf("the inventory extreme %.10g would be $%.2f marked, above the $%.2f cap", extreme, notional, a.sc.Quote.MaxInventoryUSD))
		}
		margin := notional/lev + env.FeeReserve
		if margin > env.RequiredMargin {
			env.RequiredMargin = margin
		}
		hyp := make(map[string]*StrategyState, len(states))
		for id, st := range states {
			hyp[id] = st
		}
		clone := *s
		clone.Positions = make(map[string]*Position, 1)
		if extreme != 0 {
			side := "long"
			if extreme < 0 {
				side = "short"
			}
			clone.Positions[a.coin()] = &Position{Symbol: a.coin(), Quantity: math.Abs(extreme), AvgCost: px, Side: side, Multiplier: 1}
		}
		hyp[s.ID] = &clone
		if pr != nil && pr.MaxNotionalUSD > 0 {
			if held, detail := evaluateNotionalCapHold(pr, hyp, prices); held {
				env.Refusals = append(env.Refusals, "notional cap: "+detail)
			}
		}
		if exposureCapConfigured(pr) {
			st := evaluateExposureCap(pr, hyp, cfgs, prices, pv)
			if extreme > 0 && st.LongBlocked {
				env.Refusals = append(env.Refusals, fmt.Sprintf("the long extreme would breach the same-direction exposure cap ($%.2f, cap $%.2f)", st.LongUSD, st.CapUSD))
			}
			if extreme < 0 && st.ShortBlocked {
				env.Refusals = append(env.Refusals, fmt.Sprintf("the short extreme would breach the same-direction exposure cap ($%.2f, cap $%.2f)", st.ShortUSD, st.CapUSD))
			}
			dir := "long"
			if extreme < 0 {
				dir = "short"
			}
			if stat, ok := st.OverConcentrated[a.coin()]; ok && extreme != 0 && stat.Direction == dir {
				env.Refusals = append(env.Refusals, fmt.Sprintf("the %s extreme would leave %s at %.1f%% of portfolio value (cap %.1f%%)", dir, a.coin(), stat.Pct, st.ConcentrationPct))
			}
		}
	}
	if env.AvailableMargin < env.RequiredMargin {
		env.Refusals = append(env.Refusals, fmt.Sprintf("verified free margin $%.2f is below the $%.2f the worst inventory extreme needs", env.AvailableMargin, env.RequiredMargin))
	}
	sort.Strings(env.Refusals)
	env.Refusals = dedupeStrings(env.Refusals)
	return env
}

func (a *quoteAdmission) desiredStop(q *quoteOwnerState, pos *quotePositionRead) (*quoteOrderRequest, string) {
	inv := mustQuoteRat(q.Contract.Book.Qty)
	if inv.Sign() == 0 {
		return nil, ""
	}
	posSide, stopSide := "long", "sell"
	if inv.Sign() < 0 {
		posSide, stopSide = "short", "buy"
	}
	abs := quoteRatFloat(quoteRatAbs(inv))
	share := hlShareResult{Qty: abs}
	liq := 0.0
	if pos != nil {
		if signed, err := parseQuoteDecimal(pos.Size); err == nil {
			share = hlOwnStopShare(hlShareInput{Side: posSide, Self: hlShareBook{Qty: abs}, Signed: quoteRatFloat(signed), Known: true})
		}
		liq = pos.LiquidationPx
	}
	if share.Qty <= 0 {
		return nil, "the fresh venue position does not back the settled inventory, so no stop can be sized"
	}
	szRat, _ := quoteRatFromFloat(share.Qty)
	size, err := a.prices.floorQuoteSize(szRat)
	if err != nil || size.Sign() <= 0 {
		return nil, fmt.Sprintf("the stop size %.10g floors to zero on the lot grid", share.Qty)
	}
	anchor := quoteRatFloat(mustQuoteRat(q.Contract.Book.RiskAnchorPx))
	trigger := hlLiquidationScalarRearmTriggerPx(a.sc, posSide, anchor, liq)
	if trigger <= 0 {
		return nil, "the percent stop does not resolve a trigger"
	}
	if stopPastLiquidation(posSide, trigger, liq) {
		return nil, fmt.Sprintf("the stop trigger %.8g cannot be clamped inside the liquidation price %.8g", trigger, liq)
	}
	tr, _ := quoteRatFromFloat(trigger)
	rounded, err := a.prices.roundTriggerPx(stopSide, tr)
	if err != nil {
		return nil, err.Error()
	}
	if cur := q.currentStop(); cur != nil && !cur.terminal() && cur.Request.Side == stopSide {
		old := mustQuoteRatOrZero(cur.Request.TriggerPx)
		if old.Sign() > 0 && ((posSide == "long" && rounded.Cmp(old) < 0) || (posSide == "short" && rounded.Cmp(old) > 0)) {
			rounded = old
		}
	}
	return &quoteOrderRequest{Coin: a.coin(), Role: quoteRoleStop, Side: stopSide, Size: quoteDecimalText(size), TriggerPx: quoteDecimalText(rounded), ReduceOnly: true}, ""
}

func (a *quoteAdmission) maintainStop(ctx context.Context, pos *quotePositionRead) {
	_, q := a.ownerSnapshot()
	if q == nil || q.Contract == nil {
		return
	}
	want, why := a.desiredStop(q, pos)
	if why != "" {
		a.alertOnce("stop|"+why, why, true)
		return
	}
	open := q.openStops()
	matches := func(o *quoteOrderRow) bool {
		return want != nil && o.Request.Side == want.Side && o.Request.TriggerPx == want.TriggerPx && o.Request.Size == want.Size && o.CancelState == quoteCancelNone
	}
	var keep *quoteOrderRow
	for _, o := range open {
		if keep == nil && matches(o) && (o.SubmitState == quoteSubmitResting || o.SubmitState == quoteSubmitSubmitting || o.SubmitState == quoteSubmitReserved) {
			keep = o
			continue
		}
		if o.SubmitState == quoteSubmitUnknown || o.CancelState == quoteCancelUnknown {
			a.alertOnce(fmt.Sprintf("stop-unknown|%d", o.RowID), fmt.Sprintf("stop order %d (client %s) has an unknown outcome; both stop identities stay unresolved", o.RowID, o.ClientOrderID), true)
			return
		}
		if o.SubmitState == quoteSubmitReserved {
			continue
		}
		verify, err := a.venue.Status(ctx, o.Request, o.VenueOrderID)
		if err != nil || verify.Status == quoteOutcomeUnknown {
			a.alertOnce(fmt.Sprintf("stop-verify|%d", o.RowID), fmt.Sprintf("stop order %d could not be verified on the venue before replacement; it is kept", o.RowID), true)
			return
		}
		from := o.SubmitState
		a.applyOutcome(o, verify, false)
		_ = a.saveOutcome(o, []string{from})
		if o.terminal() {
			continue
		}
		if a.requestCancel(o) {
			a.sendCancel(ctx, o)
		}
		if !o.terminal() {
			return
		}
	}
	a.refreshOwner()
	if keep != nil {
		if keep.SubmitState == quoteSubmitReserved {
			a.placeStop(ctx, keep)
		}
		return
	}
	if want == nil {
		return
	}
	_, q = a.ownerSnapshot()
	if q == nil {
		return
	}
	for _, o := range q.openStops() {
		if !o.terminal() {
			return
		}
	}
	nowMs := a.Now().UnixMilli()
	cid, err := a.venue.NewClientOrderID()
	if err != nil {
		a.queueAlert(fmt.Sprintf("stop client id: %v", err), true)
		return
	}
	req := *want
	req.ClientOrderID = cid
	req.ExpiryMs = nowMs + a.sc.Quote.RequestExpiryMs
	if err := a.prices.check(req); err != nil {
		a.alertOnce("stop-round|"+err.Error(), "stop request refused: "+err.Error(), true)
		return
	}
	seq := q.Contract.LastStopSeq + 1
	row := &quoteOrderRow{Coin: a.coin(), Side: req.Side, Role: quoteRoleStop, Generation: seq, Provider: q.Provider, AccountKey: q.AccountKey, Namespace: q.Namespace,
		Request: req, RequestJSON: req.canonicalJSON(), RequestExpiryMs: req.ExpiryMs, ClientOrderID: cid, SubmitState: quoteSubmitReserved, ConfirmedQty: "0", AppliedQty: "0"}
	k, err := a.ownerKey()
	if err != nil {
		return
	}
	if err := a.db.reserveQuoteOrders(k, &quoteReservation{ExpectRevision: q.Revision, Orders: []*quoteOrderRow{row}, ContractField: "stop", ContractSeq: seq}, nowMs); err != nil {
		a.queueAlert(fmt.Sprintf("the stop identity could not be saved; no stop was sent: %v", err), true)
		return
	}
	a.placeStop(ctx, row)
	a.refreshOwner()
}

func (a *quoteAdmission) placeStop(ctx context.Context, row *quoteOrderRow) {
	row.SubmitState = quoteSubmitSubmitting
	if err := a.saveOutcome(row, []string{quoteSubmitReserved}); err != nil {
		a.queueAlert(fmt.Sprintf("stop %d submitting state could not be saved; nothing was sent: %v", row.RowID, err), true)
		return
	}
	out, err := a.venue.Place(ctx, row.Request)
	if err != nil {
		out = quoteOrderOutcome{Status: quoteOutcomeUnknown, Detail: err.Error()}
	}
	a.applyOutcome(row, out, false)
	if err := a.saveOutcome(row, []string{quoteSubmitSubmitting}); err != nil {
		a.queueAlert(fmt.Sprintf("stop %d outcome could not be saved: %v", row.RowID, err), true)
	}
	if row.SubmitState == quoteSubmitUnknown {
		a.alertOnce(fmt.Sprintf("stop-unknown|%d", row.RowID), fmt.Sprintf("stop order %d (client %s) placement has an unknown outcome", row.RowID, row.ClientOrderID), true)
	}
	if row.SubmitState == quoteSubmitRejected {
		a.alertOnce(fmt.Sprintf("stop-rejected|%d", row.RowID), fmt.Sprintf("stop order %d was rejected by the venue: %s", row.RowID, out.Detail), true)
	}
}

func (a *quoteAdmission) electiveExit(ctx context.Context, req quoteDeadlineRequest, exit quoteExitRequest, mark float64, markOK bool, pos *quotePositionRead) []string {
	_, q := a.ownerSnapshot()
	if q == nil || q.Contract == nil {
		return nil
	}
	inv := mustQuoteRat(q.Contract.Book.Qty)
	if inv.Sign() == 0 {
		return nil
	}
	for _, o := range q.Orders {
		if (o.Role == quoteRoleBid || o.Role == quoteRoleAsk || o.Role == quoteRoleClose) && !o.resolved() {
			return []string{"the elective exit waits until every quote and close order is terminal and settled"}
		}
	}
	if len(q.Unapplied) > 0 {
		return []string{"the elective exit waits for unapplied fills to settle"}
	}
	if !markOK {
		return []string{"the elective exit needs a fresh mark"}
	}
	posSide := "long"
	side := "sell"
	if inv.Sign() < 0 {
		posSide, side = "short", "buy"
	}
	abs := quoteRatFloat(quoteRatAbs(inv))
	view := hlOnChainCoinView{Known: false}
	if pos != nil {
		if signed, err := parseQuoteDecimal(pos.Size); err == nil {
			f := quoteRatFloat(signed)
			view = hlOnChainCoinView{Known: true, AbsQty: map[string]float64{a.coin(): math.Abs(f)}, NetSide: map[string]string{}}
			if f > 0 {
				view.NetSide[a.coin()] = "long"
			} else if f < 0 {
				view.NetSide[a.coin()] = "short"
			}
		}
	}
	plan := planHLCloseOrder(a.coin(), posSide, abs, abs, 0, 0, view)
	if plan.Action != hlCloseSend {
		return []string{"the close planner holds the exit: " + plan.Reason}
	}
	szRat, _ := quoteRatFromFloat(plan.Size)
	size, err := a.prices.floorQuoteSize(szRat)
	if err != nil || size.Sign() <= 0 {
		return []string{"the exit size floors to zero"}
	}
	limit := mark * (1 - quoteCloseSlippagePct/100)
	if side == "buy" {
		limit = mark * (1 + quoteCloseSlippagePct/100)
	}
	lr, _ := quoteRatFromFloat(limit)
	px, err := a.prices.roundQuotePx(side, lr)
	if err != nil {
		return []string{err.Error()}
	}
	nowMs := a.Now().UnixMilli()
	cid, err := a.venue.NewClientOrderID()
	if err != nil {
		return []string{err.Error()}
	}
	creq := quoteOrderRequest{Coin: a.coin(), Role: quoteRoleClose, Side: side, Px: quoteDecimalText(px), Size: quoteDecimalText(size), ReduceOnly: true, TIF: "Ioc", ClientOrderID: cid, ExpiryMs: nowMs + a.sc.Quote.RequestExpiryMs}
	seq := q.Contract.LastCloseSeq + 1
	row := &quoteOrderRow{Coin: a.coin(), Side: side, Role: quoteRoleClose, Generation: seq, Provider: q.Provider, AccountKey: q.AccountKey, Namespace: q.Namespace,
		Request: creq, RequestJSON: creq.canonicalJSON(), RequestExpiryMs: creq.ExpiryMs, ClientOrderID: cid, SubmitState: quoteSubmitReserved, ConfirmedQty: "0", AppliedQty: "0"}
	k, err := a.ownerKey()
	if err != nil {
		return []string{err.Error()}
	}
	if err := a.db.reserveQuoteOrders(k, &quoteReservation{ExpectRevision: q.Revision, Orders: []*quoteOrderRow{row}, ContractField: "close", ContractSeq: seq}, nowMs); err != nil {
		return []string{fmt.Sprintf("the exit identity could not be saved: %v", err)}
	}
	row.SubmitState = quoteSubmitSubmitting
	if err := a.saveOutcome(row, []string{quoteSubmitReserved}); err != nil {
		return []string{fmt.Sprintf("the exit submitting state could not be saved: %v", err)}
	}
	out, err := a.venue.Place(ctx, creq)
	if err != nil {
		out = quoteOrderOutcome{Status: quoteOutcomeUnknown, Detail: err.Error()}
	}
	a.applyOutcome(row, out, false)
	if row.SubmitState == quoteSubmitResting {
		row.SubmitState = quoteSubmitCancelled
		row.TerminalAtMs = a.Now().UnixMilli()
	}
	_ = a.saveOutcome(row, []string{quoteSubmitSubmitting})
	var holds []string
	switch row.SubmitState {
	case quoteSubmitUnknown:
		a.alertOnce(fmt.Sprintf("exit-unknown|%d", row.RowID), fmt.Sprintf("the inventory exit order %d has an unknown outcome", row.RowID), true)
		holds = append(holds, "the inventory exit outcome is unknown")
	case quoteSubmitFilled, quoteSubmitCancelled:
		filled := quoteRatFloat(mustQuoteRat(row.ConfirmedQty))
		if filled > 0 && filled < abs {
			rem := resolveHLCloseRemainderStop(a.coin(), posSide, abs, hlCloseFillOutcome{Known: true, Filled: filled}, hlCloseBacking{
				Refetch: func() (hlOnChainCoinView, error) {
					p, err := a.venue.Position(ctx, a.coin())
					if err != nil {
						return hlOnChainCoinView{}, err
					}
					signed, err := parseQuoteDecimal(p.Size)
					if err != nil {
						return hlOnChainCoinView{}, err
					}
					f := quoteRatFloat(signed)
					v := hlOnChainCoinView{Known: true, AbsQty: map[string]float64{a.coin(): math.Abs(f)}, NetSide: map[string]string{}}
					if f > 0 {
						v.NetSide[a.coin()] = "long"
					} else if f < 0 {
						v.NetSide[a.coin()] = "short"
					}
					return v, nil
				},
			})
			if rem.Qty > 0 {
				a.resizeStopAfterPartialClose(ctx, rem.Qty)
			} else if rem.Unbacked {
				holds = append(holds, "the partial exit left an unbacked remainder: "+rem.Detail)
			}
		}
	}
	a.refreshOwner()
	return holds
}

func (a *quoteAdmission) resizeStopAfterPartialClose(ctx context.Context, qty float64) {
	_, q := a.ownerSnapshot()
	if q == nil {
		return
	}
	cur := q.currentStop()
	if cur == nil || cur.terminal() {
		return
	}
	szRat, _ := quoteRatFromFloat(qty)
	size, err := a.prices.floorQuoteSize(szRat)
	if err != nil || size.Sign() <= 0 {
		return
	}
	if mustQuoteRat(cur.Request.Size).Cmp(size) == 0 {
		return
	}
	verify, err := a.venue.Status(ctx, cur.Request, cur.VenueOrderID)
	if err != nil || verify.Status != quoteOutcomeResting {
		return
	}
	if !a.requestCancel(cur) {
		return
	}
	a.sendCancel(ctx, cur)
	if !cur.terminal() {
		return
	}
	nowMs := a.Now().UnixMilli()
	cid, err := a.venue.NewClientOrderID()
	if err != nil {
		return
	}
	req := cur.Request
	req.Size = quoteDecimalText(size)
	req.ClientOrderID = cid
	req.ExpiryMs = nowMs + a.sc.Quote.RequestExpiryMs
	seq := q.Contract.LastStopSeq + 1
	row := &quoteOrderRow{Coin: a.coin(), Side: req.Side, Role: quoteRoleStop, Generation: seq, Provider: q.Provider, AccountKey: q.AccountKey, Namespace: q.Namespace,
		Request: req, RequestJSON: req.canonicalJSON(), RequestExpiryMs: req.ExpiryMs, ClientOrderID: cid, SubmitState: quoteSubmitReserved, ConfirmedQty: "0", AppliedQty: "0"}
	k, err := a.ownerKey()
	if err != nil {
		return
	}
	if err := a.db.reserveQuoteOrders(k, &quoteReservation{ExpectRevision: q.Revision, Orders: []*quoteOrderRow{row}, ContractField: "stop", ContractSeq: seq}, nowMs); err != nil {
		a.queueAlert(fmt.Sprintf("the remainder stop identity could not be saved: %v", err), true)
		return
	}
	a.placeStop(ctx, row)
}

func (a *quoteAdmission) Snapshot() quoteSnapshot {
	a.mu.RLock()
	defer a.mu.RUnlock()
	s := a.state.Strategies[a.sc.ID]
	if s == nil {
		return quoteSnapshot{OwnerID: a.sc.ID, Partition: a.partition.String()}
	}
	return a.snapshotOf(s, nil)
}

func (a *quoteAdmission) snapshotOf(s *StrategyState, staged *quoteStaged) quoteSnapshot {
	snap := quoteSnapshot{OwnerID: a.sc.ID, Partition: a.partition.String(), SourceRole: a.db.storageRoleOf(), Coin: a.coin(),
		PolicyFingerprint: a.policyFP, ConfigFingerprint: a.configFP, Cash: s.Cash}
	q := s.Quote
	if q == nil || q.Contract == nil {
		return snap
	}
	c := q.Contract
	book := c.Book
	snap.ContractCreated = true
	snap.Revision = q.Revision
	snap.LastDeadlineMs = c.LastDeadlineMs
	snap.AppliedBoundaryMs = c.AppliedBoundaryMs
	snap.OperatorHold = c.OperatorHold
	snap.OperatorHoldReason = c.OperatorHoldReason
	if staged != nil {
		book = staged.Book
		snap.Revision = staged.BaseRevision
		if staged.Kind != quoteSettleNone {
			snap.Revision = staged.BaseRevision + 1
		}
		snap.AppliedBoundaryMs = staged.BoundaryMs
	}
	snap.Inventory = book.Qty
	snap.InventoryProjected = quoteRatFloat(mustQuoteRat(book.Qty))
	if abs := quoteRatAbs(mustQuoteRat(book.Qty)); abs.Sign() > 0 {
		snap.AvgCost = quoteRatFloat(quoteRatQuo(mustQuoteRat(book.Cost), abs))
	}
	snap.FirstOpenMs = book.FirstOpenMs
	snap.RiskAnchorPx = book.RiskAnchorPx
	var latest *quoteGeneration
	for _, g := range q.Generations {
		if latest == nil || g.Generation > latest.Generation {
			latest = g
		}
	}
	if latest != nil {
		snap.Generation = latest.Generation
		snap.GenerationDeadlineMs = latest.DeadlineMs
		for _, o := range q.sidesOf(latest.Generation) {
			snap.Sides = append(snap.Sides, quoteSnapshotSide{RowID: o.RowID, Role: o.Role, Generation: o.Generation, Request: o.Request, Status: o.SubmitState,
				CancelState: o.CancelState, ClientID: o.ClientOrderID, VenueOrderID: o.VenueOrderID, AcceptedAtMs: o.AcceptedAtMs, ConfirmedQty: o.ConfirmedQty, AppliedQty: o.AppliedQty})
		}
	}
	if stop := q.currentStop(); stop != nil {
		snap.Stop = &quoteSnapshotStop{RowID: stop.RowID, ClientID: stop.ClientOrderID, Side: stop.Request.Side, TriggerPx: stop.Request.TriggerPx, Size: stop.Request.Size, Status: stop.SubmitState}
	}
	for _, o := range q.Orders {
		if !o.resolved() {
			snap.UnresolvedOrders++
		}
	}
	snap.UnappliedFills = len(q.Unapplied)
	snap.Envelope = q.Envelope
	snap.Holds = append([]string(nil), q.LastHolds...)
	snap.SettlementHolds = append([]string(nil), q.SettlementHolds...)
	if staged != nil {
		snap.SettlementHolds = append([]string(nil), staged.Holds...)
	}
	if s.PaperFunding != nil {
		if fc := s.PaperFunding.Coins[a.coin()]; fc != nil {
			snap.FundingCoverageStartMs = fc.coverageSince()
			snap.FundingHeld = append([]paperFundingHeld(nil), fc.Held...)
			snap.FundingGaps = append([]paperFundingGap(nil), fc.openGaps()...)
		}
	}
	return snap
}

func (a *quoteAdmission) OperatorCancel(generation int64) (int, error) {
	k, err := a.ownerKey()
	if err != nil {
		return 0, err
	}
	n, err := a.db.requestQuoteGenerationCancel(k, generation, a.Now().UnixMilli())
	if err != nil {
		return 0, err
	}
	a.refreshOwner()
	return n, nil
}

func (a *quoteAdmission) OperatorHold(hold bool, reason string) error {
	k, err := a.ownerKey()
	if err != nil {
		return err
	}
	if err := a.db.setQuoteOperatorHold(k, hold, reason, a.Now().UnixMilli()); err != nil {
		return err
	}
	a.refreshOwner()
	return nil
}

func (a *quoteAdmission) ShutdownCleanup(budget time.Duration) []string {
	if a == nil {
		return nil
	}
	if budget <= 0 {
		budget = quoteShutdownCleanupBudget
	}
	a.opMu.Lock()
	a.shutdown = true
	a.opMu.Unlock()
	defer a.flushOutbox()
	ctx, cancel := context.WithTimeout(context.Background(), budget)
	defer cancel()
	var lines []string
	_, q := a.ownerSnapshot()
	if q == nil {
		return nil
	}
	var targets []*quoteOrderRow
	for _, o := range q.Orders {
		if o.Role == quoteRoleStop {
			continue
		}
		if o.mayRest() && o.CancelState == quoteCancelNone {
			if a.requestCancel(o) {
				targets = append(targets, o)
			}
		} else if o.CancelState == quoteCancelRequested && o.mayRest() {
			targets = append(targets, o)
		}
	}
	for _, o := range targets {
		if ctx.Err() != nil {
			lines = append(lines, fmt.Sprintf("order %d cancel left for restart (cleanup budget spent)", o.RowID))
			continue
		}
		a.sendCancel(ctx, o)
		lines = append(lines, fmt.Sprintf("order %d cancel outcome: %s/%s", o.RowID, o.SubmitState, o.CancelState))
	}
	if inv := mustQuoteRat(q.Contract.Book.Qty); inv.Sign() != 0 {
		if stop := q.currentStop(); stop != nil && stop.SubmitState == quoteSubmitResting {
			lines = append(lines, fmt.Sprintf("stop %d stays resting on %s inventory", stop.RowID, q.Contract.Book.Qty))
		}
	}
	a.refreshOwner()
	return lines
}

func quoteShutdownCleanupFor(store *StateStore) []string {
	adm := quoteAdmissionForStore(store)
	if adm == nil {
		return nil
	}
	return adm.ShutdownCleanup(quoteShutdownCleanupBudget)
}

func quoteStartupRecoveryFor(ctx context.Context, store *StateStore) error {
	adm := quoteAdmissionForStore(store)
	if adm == nil {
		return nil
	}
	_, err := RecoverOnStart(ctx, adm)
	return err
}

type killSwitchQuoteReport struct {
	Clear    bool
	LogLines []string
}

func killSwitchQuoteLane(store *StateStore, budget time.Duration) killSwitchQuoteReport {
	rep := killSwitchQuoteReport{Clear: true}
	if store == nil {
		return rep
	}
	adm := quoteAdmissionForStore(store)
	if adm == nil {
		pending, err := store.LoadPendingOrdersCombined()
		if err != nil {
			rep.Clear = false
			rep.LogLines = append(rep.LogLines, fmt.Sprintf("[kill-switch] quote order lane could not read quote rows: %v", err))
			return rep
		}
		for _, o := range pending.Quote {
			if !o.resolved() {
				rep.Clear = false
				rep.LogLines = append(rep.LogLines, fmt.Sprintf("[kill-switch] quote order row %d (%s, %s) is unresolved and no admitted quote coordinator owns it; the account is not confirmed flat", o.RowID, o.Role, partitionLabel(o.Partition)))
			}
		}
		return rep
	}
	if budget <= 0 {
		budget = quoteShutdownCleanupBudget
	}
	ctx, cancel := context.WithTimeout(context.Background(), budget)
	defer cancel()
	defer adm.flushOutbox()
	_, q := adm.ownerSnapshot()
	if q == nil {
		return rep
	}
	for _, o := range q.Orders {
		if !(o.Role == quoteRoleBid || o.Role == quoteRoleAsk) || !o.mayRest() {
			continue
		}
		if o.CancelState == quoteCancelNone && !adm.requestCancel(o) {
			continue
		}
		if ctx.Err() != nil {
			break
		}
		adm.sendCancel(ctx, o)
		rep.LogLines = append(rep.LogLines, fmt.Sprintf("[kill-switch] quote order %d (%s) cancel outcome %s/%s", o.RowID, o.Role, o.SubmitState, o.CancelState))
	}
	adm.refreshOwner()
	_, q = adm.ownerSnapshot()
	if q != nil && q.busy() {
		rep.Clear = false
		rep.LogLines = append(rep.LogLines, fmt.Sprintf("[kill-switch] quote owner %s still holds inventory or unresolved quote evidence; its exit runs through the %s lane and the account is not confirmed flat", adm.sc.ID, inventoryQuoteExitCloseName))
	}
	return rep
}

func (a *quoteAdmission) killSwitchLatched() bool {
	a.mu.RLock()
	defer a.mu.RUnlock()
	prs := a.state.PortfolioRisk[a.partition]
	return prs != nil && prs.KillSwitchActive
}

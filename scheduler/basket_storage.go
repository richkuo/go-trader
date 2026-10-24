package main

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"
)

const (
	basketRebalanceReserved   = "reserved"
	basketRebalanceRanking    = "ranking"
	basketRebalanceFrozen     = "targets_frozen"
	basketRebalanceExecuting  = "executing"
	basketRebalanceCompleted  = "completed"
	basketRebalanceHeld       = "held"
	basketRebalanceSuperseded = "superseded"
)

const (
	basketActionReserved   = "reserved"
	basketActionSubmitting = "submitting"
	basketActionUnresolved = "unresolved"
	basketActionSettling   = "settling"
	basketActionCommitted  = "committed"
	basketActionNoFill     = "no_fill"
)

const (
	basketDirectionReduce   = "reduce"
	basketDirectionIncrease = "increase"
)

const basketTargetsVersion = 1

var errBasketDeadlineTaken = errors.New("the basket deadline is already reserved")

func basketRebalanceRank(status string) int {
	switch status {
	case basketRebalanceReserved:
		return 0
	case basketRebalanceRanking:
		return 1
	case basketRebalanceFrozen:
		return 2
	case basketRebalanceExecuting:
		return 3
	case basketRebalanceCompleted, basketRebalanceHeld, basketRebalanceSuperseded:
		return 4
	}
	return -1
}

func basketRebalanceTerminal(status string) bool {
	return basketRebalanceRank(status) == 4
}

func basketActionRank(status string) int {
	switch status {
	case basketActionReserved:
		return 0
	case basketActionSubmitting:
		return 1
	case basketActionUnresolved:
		return 2
	case basketActionSettling:
		return 3
	case basketActionCommitted, basketActionNoFill:
		return 4
	}
	return -1
}

func basketActionPending(status string) bool {
	r := basketActionRank(status)
	return r >= 0 && r < 4
}

func basketActionBookIndependent(status string) bool {
	return status != basketActionCommitted
}

type basketMemberTarget struct {
	StrategyID        string  `json:"strategy_id"`
	Coin              string  `json:"coin"`
	Weight            float64 `json:"weight"`
	Score             float64 `json:"score"`
	TargetNotionalUSD float64 `json:"target_notional_usd"`
	MarkPx            float64 `json:"mark_px"`
	TargetQty         float64 `json:"target_qty"`
	ResidualUSD       float64 `json:"residual_usd"`
	Excluded          string  `json:"excluded,omitempty"`
}

type basketTargets struct {
	Version        int                           `json:"v"`
	GrossBudgetUSD float64                       `json:"gross_budget_usd"`
	Members        map[string]basketMemberTarget `json:"members"`
}

type basketDeviation struct {
	StrategyID string `json:"strategy_id,omitempty"`
	Kind       string `json:"kind"`
	Detail     string `json:"detail"`
	AtMs       int64  `json:"at_ms"`
}

type basketException struct {
	StrategyID  string  `json:"strategy_id"`
	ExpectedQty float64 `json:"expected_qty"`
	ActualQty   float64 `json:"actual_qty"`
	Cause       string  `json:"cause"`
	Breach      string  `json:"breach,omitempty"`
	AtMs        int64   `json:"at_ms"`
}

type basketOutcome struct {
	Deviations    []basketDeviation `json:"deviations,omitempty"`
	Exceptions    []basketException `json:"exceptions,omitempty"`
	Alerts        []string          `json:"alerts,omitempty"`
	FreezeReason  string            `json:"freeze_reason,omitempty"`
	RecoveryOpen  bool              `json:"recovery_open,omitempty"`
	Result        string            `json:"result,omitempty"`
	CompletedAtMs int64             `json:"completed_at_ms,omitempty"`
}

func (o *basketOutcome) alerted(key string) bool {
	for _, k := range o.Alerts {
		if k == key {
			return true
		}
	}
	return false
}

func (o *basketOutcome) markAlerted(key string) {
	if !o.alerted(key) {
		o.Alerts = append(o.Alerts, key)
		sort.Strings(o.Alerts)
	}
}

type basketRebalance struct {
	Partition       RiskPartition
	BasketID        string
	DeadlineMs      int64
	Status          string
	ContractFP      string
	SnapshotID      string
	Generation      uint64
	CutoffMs        int64
	InputFP         string
	EvidenceJSON    string
	TargetsJSON     string
	HoldReason      string
	Outcome         basketOutcome
	IncreasesFrozen bool
	CreatedAtMs     int64
	UpdatedAtMs     int64

	targets       *basketTargets
	durableStatus string
}

func (rb *basketRebalance) Targets() (*basketTargets, error) {
	if rb == nil || rb.TargetsJSON == "" {
		return nil, nil
	}
	if rb.targets != nil {
		return rb.targets, nil
	}
	var t basketTargets
	if err := json.Unmarshal([]byte(rb.TargetsJSON), &t); err != nil {
		return nil, fmt.Errorf("basket %s deadline %d targets are unreadable: %w", rb.BasketID, rb.DeadlineMs, err)
	}
	if t.Version != basketTargetsVersion {
		return nil, fmt.Errorf("basket %s deadline %d targets version %d is not supported", rb.BasketID, rb.DeadlineMs, t.Version)
	}
	rb.targets = &t
	return rb.targets, nil
}

func (rb *basketRebalance) outcomeJSON() string {
	b, err := json.Marshal(rb.Outcome)
	if err != nil || string(b) == "{}" {
		return ""
	}
	return string(b)
}

type basketAction struct {
	ActionID          string
	Partition         RiskPartition
	BasketID          string
	DeadlineMs        int64
	StrategyID        string
	Symbol            string
	Direction         string
	Seq               int
	OrderSide         string
	PositionSide      string
	Status            string
	RequestedQty      float64
	PreQty            float64
	PreNotionalUSD    float64
	MarkPx            float64
	ProtectionJSON    string
	Provider          string
	AccountKey        string
	ClientOrderID     string
	VenueOrderID      string
	EntryIntentID     string
	DecisionBarOpenMs int64
	ConfirmedQty      float64
	ConfirmedAvgPx    float64
	ConfirmedFee      float64
	AppliedQty        float64
	AppliedFee        float64
	EvidenceJSON      string
	HoldReason        string
	CreatedAtMs       int64
	UpdatedAtMs       int64

	Purpose             string
	LifecyclePositionID string
	ExpiryMs            int64
	LifecycleAttempt    int

	GroupMode          string
	Instrument         string
	NativeQty          float64
	ContractSize       float64
	RequestJSON        string
	QuoteEventID       string
	PriceBound         float64
	AcceptDeadlineMs   int64
	SubmittedAtMs      int64
	ConfirmedNotional  float64
	FeePresent         bool
	FeeCurrency        string
	FeeAmount          float64
	FeeEvidenceID      string
	FeeEvidenceVersion int64
	AppliedNotional    float64
	AppliedFeeVersion  int64
	Terminal           bool
	CoverageThroughMs  int64
	RetryCount         int
	RetryAtMs          int64
	RecoveryJSON       string
	BookedJSON         string

	durableStatus  string
	durableApplied float64
	pendingFee     *pairFeeFix
	feeFinal       bool
}

func (a *basketAction) pair() bool {
	return a != nil && a.GroupMode == pairGroupMode
}

func (a *basketAction) expiry() bool {
	return a != nil && a.Purpose == basketPurposeExpiry
}

func (a *basketAction) purpose() string {
	if a == nil || a.Purpose == "" {
		return basketPurposeRebalance
	}
	return a.Purpose
}

func (a *basketAction) signedDelta() float64 {
	if a == nil {
		return 0
	}
	if a.OrderSide == "buy" {
		return 1
	}
	return -1
}

func basketActionID(p RiskPartition, basketID string, deadlineMs int64, strategyID, direction string, seq int) string {
	sum := sha256.Sum256([]byte(fmt.Sprintf("%s|%s|%d|%s|%s|%d", p.String(), basketID, deadlineMs, strategyID, direction, seq)))
	return "ba-" + hex.EncodeToString(sum[:12])
}

type basketRuntime struct {
	Config        BasketConfig
	Contract      basketContract
	Partition     RiskPartition
	SavedContract *basketContract
	SavedFP       string
	Rebalances    map[int64]*basketRebalance
	Actions       map[string]*basketAction
	Expected      map[string]float64
	Lifecycle     map[string]basketLifecycle

	Pair *pairRuntimeState

	pairEchoFailures map[string]string
	pairLastInputs   *pairStepInputs
	pairQuoteNote    string
	pairLastStepMs   int64

	expectedDurable map[string]float64
	needsDurable    bool
	rank            *basketRankJob
	observations    map[string]basketMemberObservation
	wakeRetryAt     time.Time
	clock           time.Time
	expiryHold      string
	expiryOpen      map[string]bool
}

func (rt *basketRuntime) auditNowMs() int64 {
	if rt != nil && !rt.clock.IsZero() {
		return rt.clock.UnixMilli()
	}
	return tradingNow().UnixMilli()
}

func newBasketRuntime(cfg BasketConfig, contract basketContract, p RiskPartition) *basketRuntime {
	return &basketRuntime{
		Config:          cfg,
		Contract:        contract,
		Partition:       p,
		Rebalances:      make(map[int64]*basketRebalance),
		Actions:         make(map[string]*basketAction),
		Expected:        make(map[string]float64),
		Lifecycle:       make(map[string]basketLifecycle),
		expectedDurable: make(map[string]float64),
		observations:    make(map[string]basketMemberObservation),
	}
}

func (rt *basketRuntime) sortedDeadlines() []int64 {
	out := make([]int64, 0, len(rt.Rebalances))
	for d := range rt.Rebalances {
		out = append(out, d)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

func (rt *basketRuntime) latest() *basketRebalance {
	ds := rt.sortedDeadlines()
	if len(ds) == 0 {
		return nil
	}
	return rt.Rebalances[ds[len(ds)-1]]
}

func (rt *basketRuntime) sortedActionIDs() []string {
	ids := make([]string, 0, len(rt.Actions))
	for id := range rt.Actions {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool {
		a, b := rt.Actions[ids[i]], rt.Actions[ids[j]]
		if a.DeadlineMs != b.DeadlineMs {
			return a.DeadlineMs < b.DeadlineMs
		}
		if a.CreatedAtMs != b.CreatedAtMs {
			return a.CreatedAtMs < b.CreatedAtMs
		}
		return ids[i] < ids[j]
	})
	return ids
}

func (rt *basketRuntime) pendingAction() *basketAction {
	for _, id := range rt.sortedActionIDs() {
		if a := rt.Actions[id]; basketActionPending(a.Status) {
			return a
		}
	}
	return nil
}

func (rt *basketRuntime) actionsFor(deadlineMs int64) []*basketAction {
	var out []*basketAction
	for _, id := range rt.sortedActionIDs() {
		if a := rt.Actions[id]; a.DeadlineMs == deadlineMs {
			out = append(out, a)
		}
	}
	return out
}

func (rt *basketRuntime) nextSeq(deadlineMs int64, strategyID, direction string) int {
	seq := 0
	for _, a := range rt.Actions {
		if a.DeadlineMs == deadlineMs && a.StrategyID == strategyID && a.Direction == direction && a.Seq > seq {
			seq = a.Seq
		}
	}
	return seq + 1
}

func (rt *basketRuntime) memberIDs() []string {
	out := make([]string, 0, len(rt.Contract.Members))
	for _, m := range rt.Contract.Members {
		out = append(out, m.StrategyID)
	}
	sort.Strings(out)
	return out
}

func (rt *basketRuntime) unresolvedRecovery() bool {
	if rt.pendingAction() != nil {
		return true
	}
	for _, l := range rt.Lifecycle {
		if l.active() {
			return true
		}
	}
	for _, rb := range rt.Rebalances {
		if !basketRebalanceTerminal(rb.Status) {
			return true
		}
	}
	return false
}

func (rt *basketRuntime) prune() {
	latest := rt.latest()
	for d, rb := range rt.Rebalances {
		if rb == latest || !basketRebalanceTerminal(rb.Status) || rb.durableStatus != rb.Status {
			continue
		}
		keep := false
		for _, a := range rt.Actions {
			if a.DeadlineMs == d && (basketActionPending(a.Status) || a.durableStatus != a.Status) {
				keep = true
			}
		}
		if keep {
			continue
		}
		delete(rt.Rebalances, d)
		for id, a := range rt.Actions {
			if a.DeadlineMs == d {
				delete(rt.Actions, id)
			}
		}
	}
}

const basketSchemaDDL = `
CREATE TABLE IF NOT EXISTS basket_contracts (
    scope TEXT NOT NULL CHECK (scope IN ('live','paper')),
    source TEXT NOT NULL DEFAULT '',
    basket_id TEXT NOT NULL,
    contract_json TEXT NOT NULL,
    contract_fingerprint TEXT NOT NULL,
    updated_at_ms INTEGER NOT NULL,
    PRIMARY KEY (scope, source, basket_id)
);

CREATE TABLE IF NOT EXISTS basket_rebalances (
    scope TEXT NOT NULL CHECK (scope IN ('live','paper')),
    source TEXT NOT NULL DEFAULT '',
    basket_id TEXT NOT NULL,
    deadline_ms INTEGER NOT NULL CHECK (deadline_ms > 0),
    status TEXT NOT NULL CHECK (status IN ('reserved','ranking','targets_frozen','executing','completed','held','superseded')),
    contract_fingerprint TEXT NOT NULL,
    snapshot_id TEXT NOT NULL DEFAULT '',
    generation INTEGER NOT NULL DEFAULT 0,
    cutoff_ms INTEGER NOT NULL DEFAULT 0,
    input_fingerprint TEXT NOT NULL DEFAULT '',
    evidence_json TEXT NOT NULL DEFAULT '',
    targets_json TEXT NOT NULL DEFAULT '',
    hold_reason TEXT NOT NULL DEFAULT '',
    outcome_json TEXT NOT NULL DEFAULT '',
    increases_frozen INTEGER NOT NULL DEFAULT 0,
    created_at_ms INTEGER NOT NULL,
    updated_at_ms INTEGER NOT NULL,
    PRIMARY KEY (scope, source, basket_id, deadline_ms)
);

CREATE TABLE IF NOT EXISTS basket_member_actions (
    action_id TEXT PRIMARY KEY,
    scope TEXT NOT NULL CHECK (scope IN ('live','paper')),
    source TEXT NOT NULL DEFAULT '',
    basket_id TEXT NOT NULL,
    deadline_ms INTEGER NOT NULL,
    strategy_id TEXT NOT NULL,
    symbol TEXT NOT NULL,
    direction TEXT NOT NULL CHECK (direction IN ('reduce','increase')),
    seq INTEGER NOT NULL CHECK (seq >= 1),
    order_side TEXT NOT NULL CHECK (order_side IN ('buy','sell')),
    position_side TEXT NOT NULL CHECK (position_side IN ('long','short')),
    status TEXT NOT NULL CHECK (status IN ('reserved','submitting','unresolved','settling','committed','no_fill')),
    requested_qty REAL NOT NULL CHECK (requested_qty > 0),
    pre_qty REAL NOT NULL DEFAULT 0,
    pre_notional_usd REAL NOT NULL DEFAULT 0,
    mark_px REAL NOT NULL DEFAULT 0,
    protection_json TEXT NOT NULL DEFAULT '',
    provider TEXT NOT NULL,
    account_key TEXT NOT NULL DEFAULT '',
    client_order_id TEXT NOT NULL,
    venue_order_id TEXT NOT NULL DEFAULT '',
    entry_intent_id TEXT NOT NULL DEFAULT '',
    decision_bar_open_ms INTEGER NOT NULL DEFAULT 0,
    confirmed_qty REAL NOT NULL DEFAULT 0,
    confirmed_avg_px REAL NOT NULL DEFAULT 0,
    confirmed_fee REAL NOT NULL DEFAULT 0,
    applied_qty REAL NOT NULL DEFAULT 0,
    applied_fee REAL NOT NULL DEFAULT 0,
    evidence_json TEXT NOT NULL DEFAULT '',
    hold_reason TEXT NOT NULL DEFAULT '',
    created_at_ms INTEGER NOT NULL,
    updated_at_ms INTEGER NOT NULL,
    UNIQUE (scope, source, basket_id, deadline_ms, strategy_id, direction, seq)
);

CREATE UNIQUE INDEX IF NOT EXISTS idx_basket_actions_one_pending
    ON basket_member_actions(scope, source, basket_id)
    WHERE status IN ('reserved','submitting','unresolved','settling');
CREATE UNIQUE INDEX IF NOT EXISTS idx_basket_actions_client_order
    ON basket_member_actions(provider, account_key, client_order_id);

CREATE TABLE IF NOT EXISTS basket_member_state (
    scope TEXT NOT NULL CHECK (scope IN ('live','paper')),
    source TEXT NOT NULL DEFAULT '',
    basket_id TEXT NOT NULL,
    strategy_id TEXT NOT NULL,
    expected_qty REAL NOT NULL DEFAULT 0,
    updated_at_ms INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (scope, source, basket_id, strategy_id)
);
`

const basketRebalanceColumns = `scope, source, basket_id, deadline_ms, status, contract_fingerprint, snapshot_id, generation,
    cutoff_ms, input_fingerprint, evidence_json, targets_json, hold_reason, outcome_json, increases_frozen,
    created_at_ms, updated_at_ms`

const basketActionColumns = `action_id, scope, source, basket_id, deadline_ms, strategy_id, symbol, direction, seq,
    order_side, position_side, status, requested_qty, pre_qty, pre_notional_usd, mark_px, protection_json,
    provider, account_key, client_order_id, venue_order_id, entry_intent_id, decision_bar_open_ms,
    confirmed_qty, confirmed_avg_px, confirmed_fee, applied_qty, applied_fee, evidence_json, hold_reason,
    created_at_ms, updated_at_ms, purpose, lifecycle_position_id, expiry_ms, lifecycle_attempt`

const basketActionBaseSelect = `action_id, scope, source, basket_id, deadline_ms, strategy_id, symbol, direction, seq,
    order_side, position_side, status, requested_qty, pre_qty, pre_notional_usd, mark_px, protection_json,
    provider, account_key, client_order_id, venue_order_id, entry_intent_id, decision_bar_open_ms,
    confirmed_qty, confirmed_avg_px, confirmed_fee, applied_qty, applied_fee, evidence_json, hold_reason,
    created_at_ms, updated_at_ms`

const basketStatusRankSQL = `CASE %s WHEN 'reserved' THEN 0 WHEN 'ranking' THEN 1 WHEN 'targets_frozen' THEN 2 WHEN 'executing' THEN 3 ELSE 4 END`

const basketActionRankSQL = `CASE %s WHEN 'reserved' THEN 0 WHEN 'submitting' THEN 1 WHEN 'unresolved' THEN 2 WHEN 'settling' THEN 3 ELSE 4 END`

func (sdb *StateDB) migrateBasketSchema() error {
	if _, err := sdb.db.Exec(basketSchemaDDL); err != nil {
		return fmt.Errorf("create basket tables: %w", err)
	}
	if err := sdb.migrateBasketLifecycleColumns(); err != nil {
		return err
	}
	return sdb.migratePairSchema()
}

func validBasketPartition(p RiskPartition) error {
	return validEntryPartition(p)
}

func (sdb *StateDB) basketMemberOwner(procID string, p RiskPartition) (string, error) {
	return sdb.entryOwner(procID, p)
}

func upsertBasketContractRow(exec sqlExecer, rt *basketRuntime, nowMs int64) error {
	_, err := exec.Exec(`INSERT INTO basket_contracts (scope, source, basket_id, contract_json, contract_fingerprint, updated_at_ms)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT(scope, source, basket_id) DO UPDATE SET
		    contract_json = excluded.contract_json,
		    contract_fingerprint = excluded.contract_fingerprint,
		    updated_at_ms = MAX(updated_at_ms, excluded.updated_at_ms)`,
		string(rt.Partition.Scope), rt.Partition.Source, rt.Contract.BasketID, rt.Contract.JSON(), rt.Contract.Fingerprint(), nowMs)
	if err != nil {
		return fmt.Errorf("upsert basket contract %s: %w", rt.Contract.BasketID, err)
	}
	return nil
}

func upsertBasketRebalanceRow(exec sqlExecer, rb *basketRebalance, status string) error {
	if basketRebalanceRank(status) < 0 {
		return fmt.Errorf("basket %s deadline %d has unknown status %q", rb.BasketID, rb.DeadlineMs, status)
	}
	frozen := 0
	if rb.IncreasesFrozen {
		frozen = 1
	}
	rankNew := fmt.Sprintf(basketStatusRankSQL, "excluded.status")
	rankOld := fmt.Sprintf(basketStatusRankSQL, "status")
	_, err := exec.Exec(`INSERT INTO basket_rebalances (`+basketRebalanceColumns+`)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(scope, source, basket_id, deadline_ms) DO UPDATE SET
		    status = excluded.status,
		    snapshot_id = CASE WHEN snapshot_id = '' THEN excluded.snapshot_id ELSE snapshot_id END,
		    generation = CASE WHEN generation = 0 THEN excluded.generation ELSE generation END,
		    cutoff_ms = CASE WHEN cutoff_ms = 0 THEN excluded.cutoff_ms ELSE cutoff_ms END,
		    input_fingerprint = CASE WHEN input_fingerprint = '' THEN excluded.input_fingerprint ELSE input_fingerprint END,
		    evidence_json = CASE WHEN evidence_json = '' THEN excluded.evidence_json ELSE evidence_json END,
		    targets_json = CASE WHEN targets_json = '' THEN excluded.targets_json ELSE targets_json END,
		    hold_reason = CASE WHEN excluded.hold_reason != '' THEN excluded.hold_reason ELSE hold_reason END,
		    outcome_json = CASE WHEN excluded.outcome_json != '' THEN excluded.outcome_json ELSE outcome_json END,
		    increases_frozen = MAX(increases_frozen, excluded.increases_frozen),
		    updated_at_ms = MAX(updated_at_ms, excluded.updated_at_ms)
		WHERE `+rankNew+` >= `+rankOld+`
		  AND (status NOT IN ('completed','held','superseded') OR status = excluded.status)`,
		string(rb.Partition.Scope), rb.Partition.Source, rb.BasketID, rb.DeadlineMs, status, rb.ContractFP,
		rb.SnapshotID, int64(rb.Generation), rb.CutoffMs, rb.InputFP, rb.EvidenceJSON, rb.TargetsJSON, rb.HoldReason,
		rb.outcomeJSON(), frozen, rb.CreatedAtMs, rb.UpdatedAtMs)
	if err != nil {
		return fmt.Errorf("upsert basket %s deadline %d: %w", rb.BasketID, rb.DeadlineMs, err)
	}
	return nil
}

func upsertBasketActionRow(exec sqlExecer, sid string, a *basketAction) error {
	if a.pair() {
		return fmt.Errorf("basket action %s is a pair action; the pair writer owns it", a.ActionID)
	}
	if basketActionRank(a.Status) < 0 {
		return fmt.Errorf("basket action %s has unknown status %q", a.ActionID, a.Status)
	}
	rankNew := fmt.Sprintf(basketActionRankSQL, "excluded.status")
	rankOld := fmt.Sprintf(basketActionRankSQL, "status")
	_, err := exec.Exec(`INSERT INTO basket_member_actions (`+basketActionColumns+`)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(action_id) DO UPDATE SET
		    status = excluded.status,
		    venue_order_id = CASE WHEN excluded.venue_order_id != '' THEN excluded.venue_order_id ELSE venue_order_id END,
		    entry_intent_id = CASE WHEN entry_intent_id = '' THEN excluded.entry_intent_id ELSE entry_intent_id END,
		    confirmed_qty = MAX(confirmed_qty, excluded.confirmed_qty),
		    confirmed_avg_px = CASE WHEN excluded.confirmed_qty >= confirmed_qty THEN excluded.confirmed_avg_px ELSE confirmed_avg_px END,
		    confirmed_fee = MAX(confirmed_fee, excluded.confirmed_fee),
		    applied_qty = MAX(applied_qty, excluded.applied_qty),
		    applied_fee = MAX(applied_fee, excluded.applied_fee),
		    evidence_json = CASE WHEN excluded.evidence_json != '' THEN excluded.evidence_json ELSE evidence_json END,
		    hold_reason = excluded.hold_reason,
		    updated_at_ms = MAX(updated_at_ms, excluded.updated_at_ms)
		WHERE `+rankNew+` >= `+rankOld+`
		  AND group_mode = 'basket'
		  AND (status NOT IN ('committed','no_fill') OR status = excluded.status)`,
		a.ActionID, string(a.Partition.Scope), a.Partition.Source, a.BasketID, a.DeadlineMs, sid, a.Symbol, a.Direction, a.Seq,
		a.OrderSide, a.PositionSide, a.Status, a.RequestedQty, a.PreQty, a.PreNotionalUSD, a.MarkPx, a.ProtectionJSON,
		a.Provider, a.AccountKey, a.ClientOrderID, a.VenueOrderID, a.EntryIntentID, a.DecisionBarOpenMs,
		a.ConfirmedQty, a.ConfirmedAvgPx, a.ConfirmedFee, a.AppliedQty, a.AppliedFee, a.EvidenceJSON, a.HoldReason,
		a.CreatedAtMs, a.UpdatedAtMs, a.purpose(), a.LifecyclePositionID, a.ExpiryMs, a.LifecycleAttempt)
	if err != nil {
		return fmt.Errorf("upsert basket action %s: %w", a.ActionID, err)
	}
	return nil
}

func (sdb *StateDB) checkBasketOwnership(rt *basketRuntime) (map[string]string, error) {
	if err := validBasketPartition(rt.Partition); err != nil {
		return nil, fmt.Errorf("basket %s: %w", rt.Contract.BasketID, err)
	}
	sids := make(map[string]string, len(rt.Contract.Members))
	for _, m := range rt.Contract.Members {
		sid, err := sdb.basketMemberOwner(m.StrategyID, rt.Partition)
		if err != nil {
			return nil, fmt.Errorf("basket %s member %s: %w", rt.Contract.BasketID, m.StrategyID, err)
		}
		sids[m.StrategyID] = sid
	}
	for _, rb := range rt.Rebalances {
		if rb.Partition != rt.Partition || rb.BasketID != rt.Contract.BasketID {
			return nil, fmt.Errorf("basket %s deadline %d record belongs to %s/%s", rt.Contract.BasketID, rb.DeadlineMs, partitionLabel(rb.Partition), rb.BasketID)
		}
	}
	for _, a := range rt.Actions {
		if a.Partition != rt.Partition || a.BasketID != rt.Contract.BasketID {
			return nil, fmt.Errorf("basket action %s belongs to %s/%s", a.ActionID, partitionLabel(a.Partition), a.BasketID)
		}
		if _, ok := sids[a.StrategyID]; !ok {
			return nil, fmt.Errorf("basket action %s names %s, which is not a member of basket %s", a.ActionID, a.StrategyID, rt.Contract.BasketID)
		}
	}
	return sids, nil
}

func (rt *basketRuntime) savesAllMembers(books map[string]*StrategyState) bool {
	for _, m := range rt.Contract.Members {
		if books[m.StrategyID] == nil {
			return false
		}
	}
	return true
}

func (sdb *StateDB) writeBasketTx(tx *sql.Tx, rt *basketRuntime, books map[string]*StrategyState, nowMs int64) (map[string]basketLifecycle, error) {
	if rt == nil {
		return nil, nil
	}
	if rt.isPair() {
		return sdb.writePairTx(tx, rt, books, nowMs)
	}
	sids, err := sdb.checkBasketOwnership(rt)
	if err != nil {
		return nil, err
	}
	full := rt.savesAllMembers(books)
	if err := upsertBasketContractRow(tx, rt, nowMs); err != nil {
		return nil, err
	}
	for _, d := range rt.sortedDeadlines() {
		rb := rt.Rebalances[d]
		status := rb.Status
		if !full && status == basketRebalanceCompleted {
			status = basketRebalanceExecuting
		}
		if err := upsertBasketRebalanceRow(tx, rb, status); err != nil {
			return nil, err
		}
	}
	for _, id := range rt.sortedActionIDs() {
		a := rt.Actions[id]
		if books[a.StrategyID] == nil && !basketActionBookIndependent(a.Status) && a.durableStatus != a.Status {
			continue
		}
		if err := upsertBasketActionRow(tx, sids[a.StrategyID], a); err != nil {
			return nil, err
		}
	}
	k := basketKey{Partition: rt.Partition, BasketID: rt.Contract.BasketID}
	next := make(map[string]basketLifecycle, len(books))
	for _, m := range rt.Contract.Members {
		id := m.StrategyID
		book := books[id]
		if book == nil {
			if err := writeBasketMemberProgressTx(tx, k, sids[id], rt.Lifecycle[id], nowMs); err != nil {
				return nil, err
			}
			continue
		}
		stored, _, err := readBasketMemberStateTx(tx, k, sids[id])
		if err != nil {
			return nil, err
		}
		l := basketLifecycleFromBook(mergeBasketLifecycle(stored, rt.Lifecycle[id]), book, m.Coin, rt.Contract.horizonMs(), 0)
		if err := writeBasketMemberBookTx(tx, k, sids[id], rt.Expected[id], l, nowMs); err != nil {
			return nil, err
		}
		next[id] = l
	}
	return next, nil
}

func markBasketDurable(rt *basketRuntime, books map[string]*StrategyState, next map[string]basketLifecycle) {
	if rt == nil {
		return
	}
	full := rt.savesAllMembers(books)
	if rt.isPair() {
		markPairDurable(rt, books)
		for _, rb := range rt.Rebalances {
			if full || rb.Status != basketRebalanceCompleted || rb.Outcome.Result == "evidence_only" {
				rb.durableStatus = rb.Status
			}
		}
		for id := range books {
			if q, ok := rt.Expected[id]; ok {
				rt.expectedDurable[id] = q
			} else {
				delete(rt.expectedDurable, id)
			}
		}
		if full {
			rt.needsDurable = false
		}
		rt.prunePair()
		return
	}
	for _, rb := range rt.Rebalances {
		if full || rb.Status != basketRebalanceCompleted {
			rb.durableStatus = rb.Status
		}
	}
	for _, a := range rt.Actions {
		if books[a.StrategyID] != nil || basketActionBookIndependent(a.Status) {
			a.durableStatus = a.Status
		}
	}
	for id := range books {
		if q, ok := rt.Expected[id]; ok {
			rt.expectedDurable[id] = q
		} else {
			delete(rt.expectedDurable, id)
		}
	}
	for id, l := range next {
		rt.Lifecycle[id] = l
	}
	if full {
		rt.needsDurable = false
	}
	rt.prune()
}

func (sdb *StateDB) persistBasketProgress(rt *basketRuntime) error {
	if sdb == nil || sdb.db == nil {
		return fmt.Errorf("state db unavailable")
	}
	if sdb.readOnly {
		return fmt.Errorf("%s state file is open read-only", sdb.storageRoleOf())
	}
	tx, err := sdb.db.Begin()
	if err != nil {
		return fmt.Errorf("begin basket progress: %w", err)
	}
	defer tx.Rollback()
	if _, err := sdb.writeBasketTx(tx, rt, nil, rt.auditNowMs()); err != nil {
		return err
	}
	if storeCommitHook != nil {
		if err := storeCommitHook(sdb.storageRoleOf()); err != nil {
			return fmt.Errorf("commit hook (%s): %w", sdb.storageRoleOf(), err)
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	markBasketDurable(rt, nil, nil)
	return nil
}

func (sdb *StateDB) reserveBasketRebalanceRow(rt *basketRuntime, rb *basketRebalance) error {
	if sdb == nil || sdb.db == nil {
		return fmt.Errorf("state db unavailable")
	}
	if sdb.readOnly {
		return fmt.Errorf("%s state file is open read-only", sdb.storageRoleOf())
	}
	if _, err := sdb.checkBasketOwnership(rt); err != nil {
		return err
	}
	tx, err := sdb.db.Begin()
	if err != nil {
		return fmt.Errorf("begin basket reservation: %w", err)
	}
	defer tx.Rollback()
	var existing int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM basket_rebalances WHERE scope = ? AND source = ? AND basket_id = ? AND deadline_ms = ?`,
		string(rb.Partition.Scope), rb.Partition.Source, rb.BasketID, rb.DeadlineMs).Scan(&existing); err != nil {
		return fmt.Errorf("read basket reservation: %w", err)
	}
	if existing > 0 {
		return errBasketDeadlineTaken
	}
	if err := upsertBasketContractRow(tx, rt, rt.auditNowMs()); err != nil {
		return err
	}
	if err := upsertBasketRebalanceRow(tx, rb, rb.Status); err != nil {
		return err
	}
	if storeCommitHook != nil {
		if err := storeCommitHook(sdb.storageRoleOf()); err != nil {
			return fmt.Errorf("commit hook (%s): %w", sdb.storageRoleOf(), err)
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	rb.durableStatus = rb.Status
	return nil
}

func (sdb *StateDB) reserveBasketActionRow(rt *basketRuntime, a *basketAction) error {
	if sdb == nil || sdb.db == nil {
		return fmt.Errorf("state db unavailable")
	}
	if sdb.readOnly {
		return fmt.Errorf("%s state file is open read-only", sdb.storageRoleOf())
	}
	if a.Status != basketActionReserved {
		return fmt.Errorf("basket action %s must be reserved before submission, got %q", a.ActionID, a.Status)
	}
	sids, err := sdb.checkBasketOwnership(rt)
	if err != nil {
		return err
	}
	sid, ok := sids[a.StrategyID]
	if !ok {
		return fmt.Errorf("basket action %s names %s, which is not a member of basket %s", a.ActionID, a.StrategyID, rt.Contract.BasketID)
	}
	tx, err := sdb.db.Begin()
	if err != nil {
		return fmt.Errorf("begin basket action reservation: %w", err)
	}
	defer tx.Rollback()
	if err := checkPendingGroupActionTx(tx, rt, a); err != nil {
		return err
	}
	seq, err := nextBasketActionSeqTx(tx, a, sid)
	if err != nil {
		return err
	}
	a.Seq = seq
	if a.expiry() {
		if a.Direction != basketDirectionReduce || a.LifecyclePositionID == "" || a.ExpiryMs <= 0 || a.LifecycleAttempt < 1 {
			return fmt.Errorf("basket expiry action %s has no lifecycle identity", a.ActionID)
		}
		if err := sdb.reserveBasketExpiryAttemptTx(tx, a, sid, rt.auditNowMs()); err != nil {
			return err
		}
	} else {
		a.ActionID = basketActionID(a.Partition, a.BasketID, a.DeadlineMs, a.StrategyID, a.Direction, a.Seq)
	}
	var dup int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM basket_member_actions WHERE action_id = ? OR (provider = ? AND account_key = ? AND client_order_id = ?)`,
		a.ActionID, a.Provider, a.AccountKey, a.ClientOrderID).Scan(&dup); err != nil {
		return fmt.Errorf("read basket action identity: %w", err)
	}
	if dup > 0 {
		return fmt.Errorf("basket action %s or client order %s is already reserved", a.ActionID, a.ClientOrderID)
	}
	if err := upsertBasketActionRow(tx, sid, a); err != nil {
		return err
	}
	for _, d := range rt.sortedDeadlines() {
		rb := rt.Rebalances[d]
		if rb.DeadlineMs != a.DeadlineMs || a.expiry() {
			continue
		}
		status := rb.Status
		if status == basketRebalanceCompleted {
			status = basketRebalanceExecuting
		}
		if err := upsertBasketRebalanceRow(tx, rb, status); err != nil {
			return err
		}
	}
	if storeCommitHook != nil {
		if err := storeCommitHook(sdb.storageRoleOf()); err != nil {
			return fmt.Errorf("commit hook (%s): %w", sdb.storageRoleOf(), err)
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	a.durableStatus = a.Status
	return nil
}

type basketKey struct {
	Partition RiskPartition
	BasketID  string
}

type basketFileRecords struct {
	Contracts  map[basketKey]string
	Rebalances map[basketKey][]*basketRebalance
	Actions    map[basketKey][]*basketAction
	Expected   map[basketKey]map[string]float64
	Lifecycle  map[basketKey]map[string]basketLifecycle
	Uses       map[basketKey][]*pairQuoteEventUse
	Progress   map[basketKey]string
	Unmapped   []string
}

func scanBasketRebalance(scan func(dest ...interface{}) error) (*basketRebalance, error) {
	var rb basketRebalance
	var scope, source, outcome string
	var gen int64
	var frozen int
	if err := scan(&scope, &source, &rb.BasketID, &rb.DeadlineMs, &rb.Status, &rb.ContractFP, &rb.SnapshotID, &gen,
		&rb.CutoffMs, &rb.InputFP, &rb.EvidenceJSON, &rb.TargetsJSON, &rb.HoldReason, &outcome, &frozen,
		&rb.CreatedAtMs, &rb.UpdatedAtMs); err != nil {
		return nil, err
	}
	rb.Partition = RiskPartition{Scope: PortfolioScope(scope), Source: source}
	if gen < 0 {
		return nil, fmt.Errorf("basket %s deadline %d has a negative generation", rb.BasketID, rb.DeadlineMs)
	}
	rb.Generation = uint64(gen)
	rb.IncreasesFrozen = frozen != 0
	if strings.TrimSpace(outcome) != "" {
		if err := json.Unmarshal([]byte(outcome), &rb.Outcome); err != nil {
			return nil, fmt.Errorf("basket %s deadline %d outcome is unreadable: %w", rb.BasketID, rb.DeadlineMs, err)
		}
	}
	rb.durableStatus = rb.Status
	return &rb, nil
}

func scanBasketAction(scan func(dest ...interface{}) error) (*basketAction, string, error) {
	var a basketAction
	var scope, source, sid string
	var feePresent, terminal int
	if err := scan(&a.ActionID, &scope, &source, &a.BasketID, &a.DeadlineMs, &sid, &a.Symbol, &a.Direction, &a.Seq,
		&a.OrderSide, &a.PositionSide, &a.Status, &a.RequestedQty, &a.PreQty, &a.PreNotionalUSD, &a.MarkPx, &a.ProtectionJSON,
		&a.Provider, &a.AccountKey, &a.ClientOrderID, &a.VenueOrderID, &a.EntryIntentID, &a.DecisionBarOpenMs,
		&a.ConfirmedQty, &a.ConfirmedAvgPx, &a.ConfirmedFee, &a.AppliedQty, &a.AppliedFee, &a.EvidenceJSON, &a.HoldReason,
		&a.CreatedAtMs, &a.UpdatedAtMs, &a.Purpose, &a.LifecyclePositionID, &a.ExpiryMs, &a.LifecycleAttempt,
		&a.GroupMode, &a.Instrument, &a.NativeQty, &a.ContractSize, &a.RequestJSON, &a.QuoteEventID, &a.PriceBound,
		&a.AcceptDeadlineMs, &a.SubmittedAtMs, &a.ConfirmedNotional, &feePresent, &a.FeeCurrency, &a.FeeAmount,
		&a.FeeEvidenceID, &a.FeeEvidenceVersion, &a.AppliedNotional, &a.AppliedFeeVersion, &terminal,
		&a.CoverageThroughMs, &a.RetryCount, &a.RetryAtMs, &a.RecoveryJSON, &a.BookedJSON); err != nil {
		return nil, "", err
	}
	a.Partition = RiskPartition{Scope: PortfolioScope(scope), Source: source}
	a.FeePresent, a.Terminal = feePresent != 0, terminal != 0
	a.durableStatus = a.Status
	a.durableApplied = a.AppliedQty
	return &a, sid, nil
}

func (sdb *StateDB) loadBasketRecords() (*basketFileRecords, error) {
	out := &basketFileRecords{
		Contracts:  make(map[basketKey]string),
		Rebalances: make(map[basketKey][]*basketRebalance),
		Actions:    make(map[basketKey][]*basketAction),
		Expected:   make(map[basketKey]map[string]float64),
		Lifecycle:  make(map[basketKey]map[string]basketLifecycle),
	}
	exists, err := tableExists(sdb, "basket_contracts")
	if err != nil || !exists {
		return out, err
	}
	actionProj, err := sdb.basketProjection("basket_member_actions", basketActionProjectedColumns())
	if err != nil {
		return nil, err
	}
	stateProj, err := sdb.basketProjection("basket_member_state", basketMemberStateLifecycleColumns)
	if err != nil {
		return nil, err
	}
	rows, err := sdb.db.Query(`SELECT scope, source, basket_id, contract_json FROM basket_contracts ORDER BY scope, source, basket_id`)
	if err != nil {
		return nil, fmt.Errorf("read basket contracts in the %s state file: %w", sdb.storageRoleOf(), err)
	}
	for rows.Next() {
		var scope, source, id, raw string
		if err := rows.Scan(&scope, &source, &id, &raw); err != nil {
			rows.Close()
			return nil, fmt.Errorf("scan basket contract in the %s state file: %w", sdb.storageRoleOf(), err)
		}
		out.Contracts[basketKey{Partition: RiskPartition{Scope: PortfolioScope(scope), Source: source}, BasketID: id}] = raw
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	latest := make(map[basketKey]int64)
	rows, err = sdb.db.Query(`SELECT scope, source, basket_id, MAX(deadline_ms) FROM basket_rebalances GROUP BY scope, source, basket_id`)
	if err != nil {
		return nil, fmt.Errorf("read latest basket deadlines in the %s state file: %w", sdb.storageRoleOf(), err)
	}
	for rows.Next() {
		var scope, source, id string
		var d int64
		if err := rows.Scan(&scope, &source, &id, &d); err != nil {
			rows.Close()
			return nil, err
		}
		latest[basketKey{Partition: RiskPartition{Scope: PortfolioScope(scope), Source: source}, BasketID: id}] = d
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	rows, err = sdb.db.Query(`SELECT ` + basketRebalanceColumns + ` FROM basket_rebalances ORDER BY scope, source, basket_id, deadline_ms`)
	if err != nil {
		return nil, fmt.Errorf("read basket rebalances in the %s state file: %w", sdb.storageRoleOf(), err)
	}
	loaded := make(map[basketKey]map[int64]bool)
	for rows.Next() {
		rb, err := scanBasketRebalance(rows.Scan)
		if err != nil {
			rows.Close()
			return nil, fmt.Errorf("scan basket rebalance in the %s state file: %w", sdb.storageRoleOf(), err)
		}
		k := basketKey{Partition: rb.Partition, BasketID: rb.BasketID}
		if basketRebalanceTerminal(rb.Status) && rb.DeadlineMs != latest[k] {
			continue
		}
		out.Rebalances[k] = append(out.Rebalances[k], rb)
		if loaded[k] == nil {
			loaded[k] = make(map[int64]bool)
		}
		loaded[k][rb.DeadlineMs] = true
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	rows, err = sdb.db.Query(`SELECT ` + basketActionBaseSelect + `, ` + actionProj + ` FROM basket_member_actions ORDER BY created_at_ms, action_id`)
	if err != nil {
		return nil, fmt.Errorf("read basket actions in the %s state file: %w", sdb.storageRoleOf(), err)
	}
	for rows.Next() {
		a, sid, err := scanBasketAction(rows.Scan)
		if err != nil {
			rows.Close()
			return nil, fmt.Errorf("scan basket action in the %s state file: %w", sdb.storageRoleOf(), err)
		}
		k := basketKey{Partition: a.Partition, BasketID: a.BasketID}
		if !loaded[k][a.DeadlineMs] && !basketActionPending(a.Status) {
			continue
		}
		procID, mapped := sdb.mapStoredStrategyID(sid)
		if !mapped {
			if basketActionPending(a.Status) {
				out.Unmapped = append(out.Unmapped, fmt.Sprintf("basket %s action %s for stored strategy %q", a.BasketID, a.ActionID, sid))
			}
			continue
		}
		a.StrategyID = procID
		out.Actions[k] = append(out.Actions[k], a)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	rows, err = sdb.db.Query(`SELECT ` + basketMemberStateBaseColumns + `, ` + stateProj + ` FROM basket_member_state ORDER BY scope, source, basket_id, strategy_id`)
	if err != nil {
		return nil, fmt.Errorf("read basket member state in the %s state file: %w", sdb.storageRoleOf(), err)
	}
	for rows.Next() {
		k, sid, q, l, err := scanBasketMemberState(rows.Scan)
		if err != nil {
			rows.Close()
			return nil, err
		}
		procID, mapped := sdb.mapStoredStrategyID(sid)
		if !mapped {
			if q != 0 {
				out.Unmapped = append(out.Unmapped, fmt.Sprintf("basket %s expected position %.10g for stored strategy %q", k.BasketID, q, sid))
			}
			if l.active() {
				out.Unmapped = append(out.Unmapped, fmt.Sprintf("basket %s holding-horizon lifecycle for position %s of stored strategy %q", k.BasketID, l.PositionID, sid))
			}
			continue
		}
		if out.Expected[k] == nil {
			out.Expected[k] = make(map[string]float64)
			out.Lifecycle[k] = make(map[string]basketLifecycle)
		}
		out.Expected[k][procID] = q
		out.Lifecycle[k][procID] = l
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if err := sdb.loadPairRecordsInto(out); err != nil {
		return nil, err
	}
	return out, nil
}

func (r *basketFileRecords) pendingFor(k basketKey) []string {
	var out []string
	for _, rb := range r.Rebalances[k] {
		if !basketRebalanceTerminal(rb.Status) {
			out = append(out, fmt.Sprintf("deadline %s is %s", formatClosedBarMs(rb.DeadlineMs), rb.Status))
		}
	}
	for _, a := range r.Actions[k] {
		if basketActionPending(a.Status) {
			out = append(out, fmt.Sprintf("action %s (%s %s) is %s", a.ActionID, a.StrategyID, a.Direction, a.Status))
		}
	}
	ids := make([]string, 0, len(r.Expected[k]))
	for id := range r.Expected[k] {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		if q := r.Expected[k][id]; q != 0 {
			out = append(out, fmt.Sprintf("member %s holds an expected %.10g", id, q))
		}
		if l := r.Lifecycle[k][id]; l.active() {
			out = append(out, fmt.Sprintf("member %s holds an unresolved holding-horizon expiry for position %s (expires %s)", id, l.PositionID, formatClosedBarMs(l.ExpiryMs)))
		}
	}
	return out
}

func (st *StateStore) basketFile(rt *basketRuntime) (*StateDB, error) {
	var db *StateDB
	for _, m := range rt.Contract.Members {
		mdb, err := st.dbForStrategy(m.StrategyID)
		if err != nil {
			return nil, fmt.Errorf("basket %s member %s: %w", rt.Contract.BasketID, m.StrategyID, err)
		}
		if mapped, ok := st.partitionForStrategy(m.StrategyID); ok && mapped != rt.Partition {
			return nil, fmt.Errorf("basket %s member %s is mapped to the %s partition, the basket to %s", rt.Contract.BasketID, m.StrategyID, partitionLabel(mapped), partitionLabel(rt.Partition))
		}
		if db == nil {
			db = mdb
		} else if db != mdb {
			return nil, fmt.Errorf("basket %s members resolve to the %s and %s state files; one basket needs one owning file", rt.Contract.BasketID, db.storageRoleOf(), mdb.storageRoleOf())
		}
	}
	if db == nil {
		return nil, fmt.Errorf("basket %s has no members", rt.Contract.BasketID)
	}
	return db, nil
}

func (st *StateStore) LoadBaskets(state *AppState, cfg *Config) (map[string]*basketRuntime, error) {
	out := make(map[string]*basketRuntime)
	if st == nil || state == nil || cfg == nil {
		return out, nil
	}
	records := make(map[storageRole]*basketFileRecords)
	for _, role := range st.order {
		db := st.file(role)
		if db == nil {
			continue
		}
		r, err := db.loadBasketRecords()
		if err != nil {
			return nil, err
		}
		if len(r.Unmapped) > 0 {
			sort.Strings(r.Unmapped)
			return nil, fmt.Errorf("the %s state file holds basket recovery evidence for unmapped strategies: %s", role, strings.Join(r.Unmapped, "; "))
		}
		records[role] = r
	}
	pairAdmitted := cfg.pairAdmission.valid()
	if !pairAdmitted && len(cfg.Pairs) > 0 {
		return nil, fmt.Errorf("pair activation needs the internal research admission")
	}
	for _, role := range st.order {
		r := records[role]
		if r == nil || pairAdmitted {
			continue
		}
		keys := make([]basketKey, 0, len(r.Contracts))
		for k := range r.Contracts {
			if r.pairContract(k) {
				keys = append(keys, k)
			}
		}
		sort.Slice(keys, func(i, j int) bool { return keys[i].BasketID < keys[j].BasketID })
		for _, k := range keys {
			return nil, fmt.Errorf("the %s state file holds research pair records for %s (%s); an ordinary load never activates or adopts a pair: %s", role, k.BasketID, partitionLabel(k.Partition), strings.Join(append([]string{"contract saved"}, r.pairPendingFor(k)...), "; "))
		}
	}
	configured := make(map[basketKey]storageRole)
	if pairAdmitted {
		if err := st.loadPairRuntimes(state, cfg, records, configured, out); err != nil {
			return nil, err
		}
	}
	for _, b := range cfg.Baskets {
		contract, err := buildBasketContract(cfg, b)
		if err != nil {
			return nil, err
		}
		members := basketMemberConfigs(cfg, b)
		if len(members) == 0 {
			return nil, fmt.Errorf("basket %s has no members", b.ID)
		}
		part := partitionFor(members[0])
		rt := newBasketRuntime(b, contract, part)
		db, err := st.basketFile(rt)
		if err != nil {
			return nil, err
		}
		role := db.storageRoleOf()
		k := basketKey{Partition: part, BasketID: b.ID}
		configured[k] = role
		for otherRole, r := range records {
			if otherRole == role {
				continue
			}
			if _, ok := r.Contracts[k]; ok {
				if pending := r.pendingFor(k); len(pending) > 0 {
					return nil, fmt.Errorf("basket %s recovery evidence sits in the %s state file, but its members are owned by the %s file: %s", b.ID, otherRole, role, strings.Join(pending, "; "))
				}
			}
		}
		if r := records[role]; r != nil {
			if raw, ok := r.Contracts[k]; ok {
				saved, perr := parseBasketContract(raw)
				if perr != nil {
					return nil, fmt.Errorf("basket %s: %w", b.ID, perr)
				}
				rt.SavedContract = &saved
				rt.SavedFP = saved.Fingerprint()
			}
			for _, rb := range r.Rebalances[k] {
				rt.Rebalances[rb.DeadlineMs] = rb
			}
			for _, a := range r.Actions[k] {
				rt.Actions[a.ActionID] = a
			}
			for id, q := range r.Expected[k] {
				rt.Expected[id] = q
				rt.expectedDurable[id] = q
			}
			for id, l := range r.Lifecycle[k] {
				rt.Lifecycle[id] = l
			}
		}
		if _, err := db.checkBasketOwnership(rt); err != nil {
			return nil, err
		}
		for _, m := range contract.Members {
			if s := state.Strategies[m.StrategyID]; s != nil {
				s.Basket = rt
			}
		}
		out[b.ID] = rt
	}
	for role, r := range records {
		keys := make([]basketKey, 0, len(r.Contracts))
		for k := range r.Contracts {
			keys = append(keys, k)
		}
		sort.Slice(keys, func(i, j int) bool {
			if keys[i].BasketID != keys[j].BasketID {
				return keys[i].BasketID < keys[j].BasketID
			}
			return keys[i].Partition.String() < keys[j].Partition.String()
		})
		for _, k := range keys {
			if _, ok := configured[k]; ok {
				continue
			}
			if pending := r.pendingFor(k); len(pending) > 0 {
				return nil, fmt.Errorf("basket %s (%s, %s state file) was removed from the config while it still holds recovery evidence: %s; restore its configuration until it is flat and resolved", k.BasketID, partitionLabel(k.Partition), role, strings.Join(pending, "; "))
			}
		}
	}
	return out, nil
}

func (st *StateStore) ReserveBasketRebalance(rt *basketRuntime, rb *basketRebalance) error {
	db, err := st.basketFile(rt)
	if err != nil {
		return err
	}
	return db.reserveBasketRebalanceRow(rt, rb)
}

func (st *StateStore) ReserveBasketAction(rt *basketRuntime, a *basketAction) error {
	db, err := st.basketFile(rt)
	if err != nil {
		return err
	}
	return db.reserveBasketActionRow(rt, a)
}

func (st *StateStore) PersistBasketProgress(rt *basketRuntime) error {
	db, err := st.basketFile(rt)
	if err != nil {
		return err
	}
	return db.persistBasketProgress(rt)
}

type basketSave struct {
	rt    *basketRuntime
	books map[string]*StrategyState
	next  map[string]basketLifecycle
}

func basketSavesFor(strategies []*StrategyState) []*basketSave {
	byRT := make(map[*basketRuntime]*basketSave)
	var out []*basketSave
	for _, s := range strategies {
		if s == nil || s.Basket == nil {
			continue
		}
		bs := byRT[s.Basket]
		if bs == nil {
			bs = &basketSave{rt: s.Basket, books: make(map[string]*StrategyState)}
			byRT[s.Basket] = bs
			out = append(out, bs)
		}
		if _, member := s.Basket.Contract.member(s.ID); member {
			bs.books[s.ID] = s
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].rt.Contract.BasketID < out[j].rt.Contract.BasketID })
	return out
}

func basketQtyEqual(a, b float64) bool {
	return math.Abs(a-b) <= 1e-9*math.Max(1, math.Max(math.Abs(a), math.Abs(b)))
}

func inspectBasketRows(db *StateDB, fi *storageFileInspection, ident storageIdentityMap) ([]string, error) {
	var rejections []string
	ownedParts := make(map[string]bool, len(fi.PartitionsOwned))
	for _, p := range fi.PartitionsOwned {
		ownedParts[p] = true
	}
	ownedScopes := make(map[PortfolioScope]bool, len(fi.ScopesOwned))
	for _, s := range fi.ScopesOwned {
		ownedScopes[s] = true
	}
	exists, err := tableExists(db, "basket_rebalances")
	if err != nil || !exists {
		return nil, err
	}
	rows, err := db.db.Query(`SELECT ` + basketRebalanceColumns + ` FROM basket_rebalances ORDER BY scope, source, basket_id, deadline_ms`)
	if err != nil {
		return nil, fmt.Errorf("read basket rebalances in the %s state file: %w", fi.Role, err)
	}
	for rows.Next() {
		rb, err := scanBasketRebalance(rows.Scan)
		if err != nil {
			rows.Close()
			return nil, fmt.Errorf("scan basket rebalance in the %s state file: %w", fi.Role, err)
		}
		fi.BasketRebalances++
		if !ownedScopes[rb.Partition.Scope] || (len(ownedParts) > 0 && !ownedParts[rb.Partition.String()]) {
			rejections = append(rejections, fmt.Sprintf("%s state file: basket_rebalances holds a %s row for basket %s but the file owns %s", fi.Role, partitionLabel(rb.Partition), rb.BasketID, strings.Join(fi.PartitionsOwned, ", ")))
		}
		if !basketRebalanceTerminal(rb.Status) {
			fi.BasketPending = append(fi.BasketPending, fmt.Sprintf("%s %s deadline %s: %s", partitionLabel(rb.Partition), rb.BasketID, formatClosedBarMs(rb.DeadlineMs), rb.Status))
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	actionProj, err := db.basketProjection("basket_member_actions", basketActionProjectedColumns())
	if err != nil {
		return nil, err
	}
	rows, err = db.db.Query(`SELECT ` + basketActionBaseSelect + `, ` + actionProj + ` FROM basket_member_actions WHERE status IN ('reserved','submitting','unresolved','settling') ORDER BY created_at_ms, action_id`)
	if err != nil {
		return nil, fmt.Errorf("read basket actions in the %s state file: %w", fi.Role, err)
	}
	for rows.Next() {
		a, sid, err := scanBasketAction(rows.Scan)
		if err != nil {
			rows.Close()
			return nil, fmt.Errorf("scan basket action in the %s state file: %w", fi.Role, err)
		}
		owner := sid
		if procID, ok := ident.processFor(fi.Role, sid); ok {
			owner = procID
		} else {
			rejections = append(rejections, fmt.Sprintf("%s state file: unresolved basket action %s names stored strategy %q, which this file does not own; resolve the venue order by hand", fi.Role, a.ActionID, sid))
		}
		if !ownedScopes[a.Partition.Scope] || (len(ownedParts) > 0 && !ownedParts[a.Partition.String()]) {
			rejections = append(rejections, fmt.Sprintf("%s state file: basket action %s belongs to the %s partition but the file owns %s", fi.Role, a.ActionID, partitionLabel(a.Partition), strings.Join(fi.PartitionsOwned, ", ")))
		}
		fi.BasketPending = append(fi.BasketPending, fmt.Sprintf("%s action %s: %s %s %s %s %.10g (%s, client %s)", a.BasketID, a.ActionID, a.purpose(), owner, a.Direction, a.OrderSide, a.RequestedQty, a.Status, a.ClientOrderID))
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	lifecycleRejections, err := inspectBasketLifecycleRows(db, fi, ident, ownedScopes, ownedParts)
	if err != nil {
		return nil, err
	}
	return append(rejections, lifecycleRejections...), nil
}

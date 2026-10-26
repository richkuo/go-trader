package main

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"
)

type pairReplayRiskRecord struct {
	Partition            string  `json:"partition"`
	TotalPV              float64 `json:"total_pv"`
	TotalNotional        float64 `json:"total_notional"`
	PairValuation        string  `json:"pair_valuation,omitempty"`
	EquityTrusted        bool    `json:"equity_trusted"`
	KillSwitchFired      bool    `json:"kill_switch_fired"`
	DailyLossEntriesHeld bool    `json:"daily_loss_entries_held"`
	NotionalBlocked      bool    `json:"notional_blocked"`
	ExposureValuation    string  `json:"exposure_valuation,omitempty"`
	Reason               string  `json:"reason,omitempty"`
}

type pairReplayViewRecord struct {
	MarkValid      bool    `json:"mark_valid"`
	ReferenceValid bool    `json:"reference_valid"`
	MarkUSD        float64 `json:"mark_usd"`
	LowerUSD       float64 `json:"lower_usd"`
	UpperUSD       float64 `json:"upper_usd"`
	ContractSize   float64 `json:"contract_size"`
	PriceCurrency  string  `json:"price_currency"`
	EvidenceMs     int64   `json:"evidence_ms"`
	Generation     uint64  `json:"generation"`
	Substituted    bool    `json:"substituted,omitempty"`
	MissingReason  string  `json:"missing_reason,omitempty"`
}

type pairReplayMemberRecord struct {
	StrategyID     string                `json:"strategy_id"`
	Cash           float64               `json:"cash"`
	InitialCapital float64               `json:"initial_capital"`
	SignedQty      float64               `json:"signed_qty"`
	AvgCost        float64               `json:"avg_cost,omitempty"`
	Value          float64               `json:"value"`
	ValueStatus    string                `json:"value_status,omitempty"`
	PeakValue      float64               `json:"peak_value"`
	DrawdownPct    float64               `json:"drawdown_pct"`
	CircuitBreaker bool                  `json:"circuit_breaker"`
	DailyPnL       float64               `json:"daily_pnl"`
	Trades         int                   `json:"trade_rows"`
	View           *pairReplayViewRecord `json:"view,omitempty"`
	ObservedAtMs   int64                 `json:"management_observed_at_ms,omitempty"`
	EchoFailure    string                `json:"echo_failure,omitempty"`
}

type pairReplayOwnerProof struct {
	PortfolioValue        map[string]float64 `json:"portfolio_value"`
	PortfolioValueStatus  string             `json:"portfolio_value_status,omitempty"`
	PortfolioNotional     float64            `json:"portfolio_notional"`
	NotionalStatus        string             `json:"portfolio_notional_status,omitempty"`
	PerpsUnrealizedLoss   float64            `json:"perps_unrealized_loss"`
	PerpsMargin           float64            `json:"perps_margin"`
	MarginStatus          string             `json:"perps_margin_status,omitempty"`
	AssetDeltas           map[string]float64 `json:"asset_net_delta_usd"`
	AssetGross            map[string]float64 `json:"asset_gross_delta_usd"`
	AssetDeltaStatus      string             `json:"asset_delta_status,omitempty"`
	ExposureCapValuation  string             `json:"exposure_cap_valuation,omitempty"`
	ExposureCapLongBlock  bool               `json:"exposure_cap_long_blocked"`
	ExposureCapShortBlock bool               `json:"exposure_cap_short_blocked"`
	NotionalCapHeld       bool               `json:"notional_cap_held"`
	NotionalCapDetail     string             `json:"notional_cap_detail,omitempty"`
	MemberRisk            map[string]string  `json:"member_risk_valuation,omitempty"`
}

type pairReplayPairRecord struct {
	Lifecycle          string   `json:"lifecycle"`
	Obligations        []string `json:"obligations,omitempty"`
	UnresolvedActions  []string `json:"unresolved_actions,omitempty"`
	IncompleteEvidence string   `json:"incomplete_evidence,omitempty"`
	NeedsDurable       bool     `json:"settlement_not_durable,omitempty"`
	Conservation       string   `json:"conservation,omitempty"`
	EmergencyPending   []string `json:"emergency_pending,omitempty"`
	NativeDecisions    []string `json:"native_decisions,omitempty"`
	Slots              int      `json:"slots"`
	Actions            int      `json:"actions"`
	RecordingBlocked   string   `json:"recording_blocked,omitempty"`
}

type pairReplayStepRecord struct {
	Ordinal     int                      `json:"ordinal"`
	At          string                   `json:"at"`
	AtMs        int64                    `json:"at_ms"`
	Managed     bool                     `json:"managed"`
	SaveBlocked bool                     `json:"save_blocked,omitempty"`
	Trace       []string                 `json:"trace"`
	NextWakeMs  int64                    `json:"next_wake_ms"`
	SameTime    int                      `json:"same_time_runs,omitempty"`
	Host        pairReplayHostState      `json:"host"`
	Risk        []pairReplayRiskRecord   `json:"risk"`
	Members     []pairReplayMemberRecord `json:"members"`
	Owners      pairReplayOwnerProof     `json:"owners"`
	Pair        pairReplayPairRecord     `json:"pair"`
}

func pairReplayRiskRecords(risk map[RiskPartition]*scopeCycleRisk) []pairReplayRiskRecord {
	out := []pairReplayRiskRecord{}
	for _, p := range pairReplayRiskPartitions(risk) {
		sr := risk[p]
		out = append(out, pairReplayRiskRecord{Partition: p.String(), TotalPV: sr.TotalPV, TotalNotional: sr.TotalNotional, PairValuation: sr.PairValuation.label(),
			EquityTrusted: sr.EquityTrusted, KillSwitchFired: sr.KillSwitchFired, DailyLossEntriesHeld: sr.DailyLossEntriesHeld, NotionalBlocked: sr.NotionalBlocked,
			ExposureValuation: sr.ExposureCapStatus.ValuationIncomplete, Reason: sr.Reason})
	}
	return out
}

func (r *pairReplayRuntime) stepRecord(ordinal int, stepID string, at time.Time, managed, saveBlocked bool, trace []string, next int64, sameTime int, risk []pairReplayRiskRecord) pairReplayStepRecord {
	rec := pairReplayStepRecord{Ordinal: ordinal, At: at.Format(time.RFC3339Nano), AtMs: at.UnixMilli(), Managed: managed, SaveBlocked: saveBlocked, Trace: trace,
		NextWakeMs: next, SameTime: sameTime, Host: r.hostState(), Risk: risk}
	if rec.Risk == nil {
		rec.Risk = []pairReplayRiskRecord{}
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	rec.Members, rec.Owners, rec.Pair = r.stateReadingsLocked(at)
	return rec
}

func (r *pairReplayRuntime) stateReadingsLocked(at time.Time) ([]pairReplayMemberRecord, pairReplayOwnerProof, pairReplayPairRecord) {
	rt, state, cfg := r.rt, r.state, r.cfg
	pm := r.bc.pairMarksLocked()
	views := r.bc.pairViewsLocked(at)
	prices := map[string]float64{}
	var members []pairReplayMemberRecord
	proof := pairReplayOwnerProof{PortfolioValue: map[string]float64{}, AssetDeltas: map[string]float64{}, AssetGross: map[string]float64{}, MemberRisk: map[string]string{}}
	var totalStatus pairValueStatus
	states := map[string]*StrategyState{}
	total := 0.0
	for _, sc := range r.members {
		s := state.Strategies[sc.ID]
		if s == nil {
			continue
		}
		states[sc.ID] = s
		sym := ""
		if cm, ok := rt.Contract.pairMember(sc.ID); ok {
			sym = cm.Symbol
		}
		v, st := portfolioValueMarked(s, prices, pm)
		total += v
		totalStatus.merge(st)
		proof.PortfolioValue[sc.ID] = v
		mr := pairReplayMemberRecord{StrategyID: sc.ID, Cash: s.Cash, InitialCapital: s.InitialCapital, SignedQty: basketPositionSigned(s, sym), Value: v, ValueStatus: st.label(),
			PeakValue: s.RiskState.PeakValue, DrawdownPct: s.RiskState.CurrentDrawdownPct, CircuitBreaker: s.RiskState.CircuitBreaker, DailyPnL: s.RiskState.DailyPnL, Trades: len(s.TradeHistory)}
		if pos := s.Positions[sym]; pos != nil {
			mr.AvgCost = pos.AvgCost
		}
		if vw, ok := views[sc.ID]; ok {
			mr.View = &pairReplayViewRecord{MarkValid: vw.MarkValid, ReferenceValid: vw.ReferenceValid, MarkUSD: vw.MarkUSD, LowerUSD: vw.LowerUSD, UpperUSD: vw.UpperUSD,
				ContractSize: vw.ContractSize, PriceCurrency: vw.PriceCurrency, EvidenceMs: vw.EvidenceMs, Generation: vw.Generation, Substituted: vw.Substituted, MissingReason: vw.MissingReason}
		}
		if o, ok := rt.observations[sc.ID]; ok {
			mr.ObservedAtMs = o.AtMs
		}
		mr.EchoFailure = rt.pairEchoFailures[sc.ID]
		_, _, rs := pairMemberRiskInputs(s, pm)
		proof.MemberRisk[sc.ID] = rs.label()
		members = append(members, mr)
	}
	proof.PortfolioValueStatus = totalStatus.label()
	var nst, mst pairValueStatus
	for _, sc := range r.members {
		one := map[string]*StrategyState{sc.ID: states[sc.ID]}
		n, st := portfolioNotionalMarked(one, prices, pm)
		loss, margin, ms := aggregatePerpsMarginInputsMarked(one, cfg.Strategies, prices, pm)
		proof.PortfolioNotional += n
		proof.PerpsUnrealizedLoss += loss
		proof.PerpsMargin += margin
		nst.merge(st)
		mst.merge(ms)
	}
	proof.NotionalStatus, proof.MarginStatus = nst.label(), mst.label()
	deltas, _, dst := computeAssetDeltasMarked(states, cfg.Strategies, prices, pm)
	for asset, e := range deltas {
		if e != nil {
			proof.AssetDeltas[asset] = e.NetDeltaUSD
			proof.AssetGross[asset] = e.GrossDeltaUSD
		}
	}
	proof.AssetDeltaStatus = dst.label()
	prc := partitionRiskConfig(cfg, r.partition)
	ec := evaluateExposureCapMarked(prc, states, cfg.Strategies, prices, total, pm)
	proof.ExposureCapValuation, proof.ExposureCapLongBlock, proof.ExposureCapShortBlock = ec.ValuationIncomplete, ec.LongBlocked, ec.ShortBlocked
	proof.NotionalCapHeld, proof.NotionalCapDetail = evaluateNotionalCapHoldMarked(prc, states, prices, pm)
	pr := pairReplayPairRecord{Lifecycle: rt.Pair.Progress.Lifecycle, NeedsDurable: rt.needsDurable, Conservation: r.bc.pairVirtualConservation(rt), Slots: len(rt.Rebalances), Actions: len(rt.Actions)}
	if r.paper != nil {
		pr.Slots, pr.Actions = r.durablePairCounts()
	}
	for _, o := range rt.Pair.Progress.Obligations {
		pr.Obligations = append(pr.Obligations, strings.TrimRight(o.Kind+"|"+o.MemberID+"|"+o.ActionID, "|"))
	}
	sort.Strings(pr.Obligations)
	for _, a := range rt.pairUnresolvedActions() {
		pr.UnresolvedActions = append(pr.UnresolvedActions, a.ActionID+" "+a.Status)
	}
	if o, ok := pairIncompleteEvidence(rt); ok {
		pr.IncompleteEvidence = o.Kind + ": " + o.Detail
	}
	pr.EmergencyPending = pairEmergencyPendingFor(state, r.partition)
	if b := rt.Pair.Progress.RecordingBlocked; b != nil {
		pr.RecordingBlocked = fmt.Sprintf("input %d: %s", b.FailedSeq, b.Reason)
	}
	for _, d := range rt.Pair.resolutions {
		pr.NativeDecisions = append(pr.NativeDecisions, fmt.Sprintf("%s|%s|%s %s by %s %s", d.Kind, d.MemberID, d.ObligationID, d.Mode, d.DecidedBy, d.ID))
	}
	sort.Strings(pr.NativeDecisions)
	return members, proof, pr
}

type pairReplayRunInfo struct {
	RunID        string                   `json:"run_id"`
	Mode         string                   `json:"mode"`
	StateDir     string                   `json:"state_dir"`
	Fingerprints basketReplayFingerprints `json:"fingerprints"`
	Lifetime     pairReplayLifetime       `json:"lifetime"`
	StepsApplied int                      `json:"steps_applied"`
	ResumedAt    int                      `json:"resumed_at_step"`
	Resumed      bool                     `json:"resumed"`
	CursorMs     int64                    `json:"cursor_ms"`
	CursorStepID string                   `json:"cursor_step_id,omitempty"`
	NextWakeMs   int64                    `json:"next_wake_ms"`
	Finished     bool                     `json:"finished"`
	StopReason   string                   `json:"stop_reason,omitempty"`
}

type pairReplayComparison struct {
	Hash     string   `json:"hash"`
	Eligible bool     `json:"eligible"`
	Reasons  []string `json:"reasons,omitempty"`
	RawCheck []string `json:"raw_checks"`
}

type pairReplayOutput struct {
	Protocol             string                     `json:"protocol"`
	Version              int                        `json:"version"`
	Status               string                     `json:"status"`
	Refusal              *basketReplayRefusal       `json:"refusal,omitempty"`
	Error                string                     `json:"error,omitempty"`
	Run                  *pairReplayRunInfo         `json:"run,omitempty"`
	ClockModel           string                     `json:"clock_model,omitempty"`
	EventOrder           string                     `json:"event_order,omitempty"`
	AdministrativeFields []string                   `json:"administrative_fields,omitempty"`
	MemberCosts          map[string]fillCostPolicy  `json:"member_costs,omitempty"`
	MechanicallyComplete bool                       `json:"mechanically_complete"`
	IncompleteReasons    []string                   `json:"incomplete_reasons,omitempty"`
	Scored               bool                       `json:"scored"`
	PromotionEligible    bool                       `json:"promotion_eligible"`
	Steps                []json.RawMessage          `json:"steps,omitempty"`
	Failures             []pairReplayFailure        `json:"failures,omitempty"`
	ServerStatus         map[string]json.RawMessage `json:"server_status,omitempty"`
	Export               map[string]interface{}     `json:"export,omitempty"`
	Comparison           *pairReplayComparison      `json:"comparison,omitempty"`
	Recording            *pairPaperOutputInfo       `json:"recording,omitempty"`
}

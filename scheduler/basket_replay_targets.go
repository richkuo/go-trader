package main

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"time"
)

func basketOfflineBaselineAllowed() bool {
	tradingClockMu.RLock()
	defer tradingClockMu.RUnlock()
	return tradingClockOwner == basketReplayClockOwner && tradingClockFn != nil
}

func basketBaselineRankRunner(weights map[int64]map[string]float64) basketRankRunner {
	return func(ctx context.Context, stdin []byte, timeout time.Duration) ([]byte, []byte, error) {
		if !basketOfflineBaselineAllowed() {
			return nil, nil, fmt.Errorf("the baseline target-input path runs only inside the offline basket replay adapter")
		}
		var req basketRankRequest
		if err := json.Unmarshal(stdin, &req); err != nil {
			return nil, nil, fmt.Errorf("decode baseline request: %w", err)
		}
		if req.V != basketRankRequestVersion {
			return nil, nil, fmt.Errorf("the baseline target-input path reads ranker request version %d, got %d", basketRankRequestVersion, req.V)
		}
		res := basketRankResult{
			V:                basketRankRequestVersion,
			BasketID:         req.BasketID,
			DeadlineMs:       req.DeadlineMs,
			SnapshotID:       req.Expected.SnapshotID,
			Generation:       req.Expected.Generation,
			CutoffMs:         req.Expected.CutoffMs,
			Status:           basketRankStatusTargets,
			Eligibility:      map[string]basketRankEligibility{},
			Scores:           map[string]float64{},
			Weights:          map[string]float64{},
			Exclusions:       map[string]string{},
			InputFingerprint: sha256Hex(stdin),
		}
		w, ok := weights[req.DeadlineMs]
		if !ok {
			res.Status, res.HoldReason = basketRankStatusHold, fmt.Sprintf("no frozen baseline weights exist for the deadline %s", formatClosedBarMs(req.DeadlineMs))
			res.Weights, res.Scores = nil, nil
			out, err := json.Marshal(res)
			return out, nil, err
		}
		contract := basketContract{Listings: req.Listings, MinHistoryBars: req.AllocationContext.MinHistoryBars}
		members := make(map[string]bool, len(req.Members))
		var invalid []string
		for _, m := range req.Members {
			members[m.Symbol] = true
			member := basketContractMember{Coin: m.Symbol, Timeframe: m.Timeframe, IntervalMs: closedBarDecisionIntervalsMs[m.Timeframe]}
			if why := basketListingExclusion(contract, member, req.DeadlineMs); why != "" {
				res.Eligibility[m.Symbol] = basketRankEligibility{Symbol: m.Symbol, Status: "excluded", Reason: why}
				if w[m.Symbol] != 0 {
					invalid = append(invalid, fmt.Sprintf("%s carries weight %v but is not eligible at the deadline (%s)", m.Symbol, w[m.Symbol], why))
				}
				continue
			}
			px, why := basketDeadlineClose(req.Market, member, req.DeadlineMs)
			if why != "" {
				res.Status, res.HoldReason = basketRankStatusHold, why
				res.Weights, res.Scores = nil, nil
				out, err := json.Marshal(res)
				return out, nil, err
			}
			res.Eligibility[m.Symbol] = basketRankEligibility{Symbol: m.Symbol, Status: "eligible", ClosedBars: req.AllocationContext.LookbackBars, Mark: px, BoundaryMs: req.DeadlineMs}
			res.Weights[m.Symbol] = w[m.Symbol]
			res.Scores[m.Symbol] = w[m.Symbol]
		}
		coins := make([]string, 0, len(w))
		for coin := range w {
			coins = append(coins, coin)
		}
		sort.Strings(coins)
		for _, coin := range coins {
			if !members[coin] {
				invalid = append(invalid, fmt.Sprintf("%s is not a basket member", coin))
			}
		}
		if len(invalid) > 0 {
			sort.Strings(invalid)
			res = basketRankResult{V: res.V, BasketID: res.BasketID, DeadlineMs: res.DeadlineMs, SnapshotID: res.SnapshotID, Generation: res.Generation, CutoffMs: res.CutoffMs,
				Status: basketRankStatusInvalid, Error: fmt.Sprintf("the baseline weights are refused: %v", invalid), InputFingerprint: res.InputFingerprint}
		}
		out, err := json.Marshal(res)
		return out, nil, err
	}
}

func (r *pairReplayRuntime) pairBaselineSelected() bool {
	return r != nil && r.spec != nil && r.spec.TargetInput != nil && (r.spec.TargetInput.Mode == pairTargetModeNoTrade || r.spec.TargetInput.Mode == pairTargetModeFrozen)
}

func pairBaselineAdmitted(r *pairReplayRuntime) bool {
	if !r.pairBaselineSelected() || r.paper != nil || r.spec.Recording != nil || r.clock == nil {
		return false
	}
	tradingClockMu.RLock()
	defer tradingClockMu.RUnlock()
	return tradingClockOwner == basketReplayClockOwner && tradingClockFn != nil
}

func pairBaselinePolicyRun(r *pairReplayRuntime, ctx context.Context, stdin []byte, timeout time.Duration) ([]byte, []byte, error) {
	if !pairBaselineAdmitted(r) {
		return nil, nil, fmt.Errorf("the pair baseline target-input path runs only inside an admitted offline pair replay")
	}
	if timeout <= 0 {
		return nil, nil, fmt.Errorf("the pair baseline target-input path has no time left")
	}
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	var req pairPolicyRequest
	if err := json.Unmarshal(stdin, &req); err != nil {
		return nil, nil, fmt.Errorf("decode pair baseline request: %w", err)
	}
	if req.V != pairPolicyRequestVersion {
		return nil, nil, fmt.Errorf("the pair baseline target-input path reads policy request version %d, got %d", pairPolicyRequestVersion, req.V)
	}
	mode := r.spec.TargetInput.Mode
	if mode == pairTargetModeNoTrade {
		return pairTargetHold(req, mode, pairTargetHoldNoTrade)
	}
	targets, ok := r.spec.TargetInput.slots[req.SlotMs]
	if !ok {
		return pairTargetHold(req, mode, fmt.Sprintf("%s (slot %d)", pairTargetHoldMissing, req.SlotMs))
	}
	if pairBookedMatchesTargets(req, targets) {
		return pairTargetHold(req, mode, pairTargetHoldSatisfied)
	}
	if pairTargetsAllZero(targets) {
		if req.Lifecycle == pairLifecycleMatched {
			return pairTargetExit(req, mode, targets)
		}
		return pairTargetHold(req, mode, fmt.Sprintf("%s (lifecycle %s)", pairTargetHoldConflict, req.Lifecycle))
	}
	if req.Lifecycle != pairLifecycleFlat {
		return pairTargetHold(req, mode, fmt.Sprintf("%s (lifecycle %s)", pairTargetHoldConflict, req.Lifecycle))
	}
	orientation, legal := pairBaselineOrientation(req, targets)
	if !legal {
		return pairTargetHold(req, mode, pairTargetHoldGeometry)
	}
	return pairTargetEnter(req, mode, orientation, targets)
}

func pairBookedMatchesTargets(req pairPolicyRequest, targets map[string]pairPolicyMemberTarget) bool {
	if len(req.Members) == 0 || len(targets) != len(req.Members) {
		return false
	}
	for _, m := range req.Members {
		t, ok := targets[m.MemberID]
		if !ok || !basketQtyEqual(m.BookedBaseQty, t.TargetBaseQty) {
			return false
		}
	}
	return true
}

func pairTargetsAllZero(targets map[string]pairPolicyMemberTarget) bool {
	if len(targets) == 0 {
		return false
	}
	for _, t := range targets {
		if t.TargetBaseQty != 0 {
			return false
		}
	}
	return true
}

func pairBaselineOrientation(req pairPolicyRequest, targets map[string]pairPolicyMemberTarget) (string, bool) {
	var hl, okx float64
	hlSet, okxSet := false, false
	for _, m := range req.Members {
		t, ok := targets[m.MemberID]
		if !ok {
			return "", false
		}
		switch m.Provider {
		case entryProviderHyperliquid:
			hl, hlSet = t.TargetBaseQty, true
		case entryProviderOKX:
			okx, okxSet = t.TargetBaseQty, true
		}
	}
	if !hlSet || !okxSet || hl == 0 || okx == 0 || (hl > 0) == (okx > 0) || !basketQtyEqual(math.Abs(hl), math.Abs(okx)) {
		return "", false
	}
	if okx > 0 {
		return pairOrientationLongOKXShortHL, true
	}
	return pairOrientationLongHLShortOKX, true
}

func pairTargetDecision(mode string, slot int64) json.RawMessage {
	b, err := json.Marshal(map[string]interface{}{"target_input": mode, "slot_ms": slot})
	if err != nil {
		return json.RawMessage(`{}`)
	}
	return b
}

func pairTargetResult(req pairPolicyRequest, res pairPolicyResult) ([]byte, []byte, error) {
	res.V = pairPolicyRequestVersion
	res.PairID = req.PairID
	res.SlotMs = req.SlotMs
	res.ContractFingerprint = req.ContractFingerprint
	res.InputFingerprint = req.InputFingerprint
	if len(res.Decision) == 0 {
		res.Decision = pairTargetDecision(req.Policy, req.SlotMs)
	}
	out, err := json.Marshal(res)
	return out, nil, err
}

func pairTargetHold(req pairPolicyRequest, mode, reason string) ([]byte, []byte, error) {
	return pairTargetResult(req, pairPolicyResult{Status: pairPolicyStatusHold, HoldReason: reason, Decision: pairTargetDecision(mode, req.SlotMs)})
}

func pairTargetEnter(req pairPolicyRequest, mode, orientation string, targets map[string]pairPolicyMemberTarget) ([]byte, []byte, error) {
	return pairTargetResult(req, pairPolicyResult{Status: pairPolicyStatusEnter, Orientation: orientation, Targets: targets, Decision: pairTargetDecision(mode, req.SlotMs)})
}

func pairTargetExit(req pairPolicyRequest, mode string, targets map[string]pairPolicyMemberTarget) ([]byte, []byte, error) {
	return pairTargetResult(req, pairPolicyResult{Status: pairPolicyStatusExit, Targets: targets, Decision: pairTargetDecision(mode, req.SlotMs)})
}

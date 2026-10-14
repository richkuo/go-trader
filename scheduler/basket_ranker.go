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

const basketRankRequestVersion = 2

const (
	basketRankStatusTargets = "targets"
	basketRankStatusHold    = "hold"
	basketRankStatusInvalid = "invalid"
)

type basketRankExpected struct {
	SnapshotID string `json:"snapshot_id"`
	Generation uint64 `json:"generation"`
	CutoffMs   int64  `json:"cutoff_ms"`
}

type basketRankMember struct {
	Symbol    string `json:"symbol"`
	Timeframe string `json:"timeframe"`
}

type basketAllocationContext struct {
	GrossBudgetUSD      float64 `json:"gross_budget_usd"`
	MaxCoinNotionalUSD  float64 `json:"max_coin_notional_usd"`
	MaxTransitionNetUSD float64 `json:"max_transition_net_usd"`
	LookbackBars        int     `json:"lookback_bars"`
	MinHistoryBars      int     `json:"min_history_bars"`
}

type basketRankRequest struct {
	V                 int                        `json:"v"`
	BasketID          string                     `json:"basket_id"`
	Ranker            string                     `json:"ranker"`
	Params            map[string]interface{}     `json:"params"`
	DeadlineMs        int64                      `json:"deadline_ms"`
	Expected          basketRankExpected         `json:"expected"`
	Members           []basketRankMember         `json:"members"`
	AllocationContext basketAllocationContext    `json:"allocation_context"`
	MinEligible       int                        `json:"min_eligible"`
	Listings          map[string]basketListingMs `json:"listings"`
	Mode              string                     `json:"mode"`
	Market            *marketPayload             `json:"market"`
}

type basketRankEligibility struct {
	Symbol      string  `json:"symbol"`
	Status      string  `json:"status"`
	Reason      string  `json:"reason"`
	ClosedBars  int     `json:"closed_bars"`
	BoundaryMs  int64   `json:"boundary_ms"`
	BarOpenMs   int64   `json:"bar_open_ms"`
	InputSHA256 string  `json:"input_sha256"`
	Mark        float64 `json:"mark"`
	ListedAtMs  int64   `json:"listed_at_ms"`
	KnownAtMs   int64   `json:"known_at_ms"`
}

type basketRankResult struct {
	V                int                              `json:"v"`
	BasketID         string                           `json:"basket_id"`
	DeadlineMs       int64                            `json:"deadline_ms"`
	SnapshotID       string                           `json:"snapshot_id"`
	Generation       uint64                           `json:"generation"`
	CutoffMs         int64                            `json:"cutoff_ms"`
	Status           string                           `json:"status"`
	HoldReason       string                           `json:"hold_reason"`
	Error            string                           `json:"error"`
	Eligibility      map[string]basketRankEligibility `json:"eligibility"`
	Scores           map[string]float64               `json:"scores"`
	Weights          map[string]float64               `json:"weights"`
	Exclusions       map[string]string                `json:"exclusions"`
	InputFingerprint string                           `json:"input_fingerprint"`
}

type basketRankClock int

const (
	basketRankClockRuntime basketRankClock = iota
	basketRankClockSimulated
)

type basketRankJob struct {
	DeadlineMs int64
	Expected   basketRankExpected
	WindowEnd  time.Time
	Timeout    time.Duration
	Clock      basketRankClock
	Payload    *marketPayload
	done       chan struct{}
	result     *basketRankResult
	stderr     string
	err        error
}

func (j *basketRankJob) finished() bool {
	if j == nil {
		return false
	}
	select {
	case <-j.done:
		return true
	default:
		return false
	}
}

type basketRankRunner func(ctx context.Context, stdin []byte, timeout time.Duration) ([]byte, []byte, error)

var basketRankerSemaphore = make(chan struct{}, 1)

func runBasketRankerProcess(ctx context.Context, stdin []byte, timeout time.Duration) ([]byte, []byte, error) {
	select {
	case basketRankerSemaphore <- struct{}{}:
	case <-ctx.Done():
		return nil, nil, fmt.Errorf("the ranker could not start before the evaluation window closed: %w", ctx.Err())
	}
	defer func() { <-basketRankerSemaphore }()
	if dl, ok := ctx.Deadline(); ok {
		if remaining := time.Until(dl); remaining < timeout {
			timeout = remaining
		}
	}
	if timeout <= 0 {
		return nil, nil, fmt.Errorf("the evaluation window closed before the ranker started")
	}
	return spawnPythonProcess(ctx, basketRankerScript, nil, stdin, timeout)
}

var basketRankRunFn basketRankRunner = runBasketRankerProcess

func basketRankRequestFor(rt *basketRuntime, deadlineMs int64, payload *marketPayload) basketRankRequest {
	c := rt.Contract
	req := basketRankRequest{
		V:          basketRankRequestVersion,
		BasketID:   c.BasketID,
		Ranker:     c.Ranker,
		Params:     c.Params,
		DeadlineMs: deadlineMs,
		AllocationContext: basketAllocationContext{
			GrossBudgetUSD:      c.GrossBudgetUSD,
			MaxCoinNotionalUSD:  c.MaxCoinNotionalUSD,
			MaxTransitionNetUSD: c.MaxTransitionNetUSD,
			LookbackBars:        c.LookbackBars,
			MinHistoryBars:      c.MinHistoryBars,
		},
		MinEligible: c.MinEligibleMembers,
		Listings:    c.Listings,
		Mode:        edgeGateModePaper,
		Market:      payload,
	}
	if req.Params == nil {
		req.Params = map[string]interface{}{}
	}
	if payload != nil {
		req.Expected = basketRankExpected{SnapshotID: payload.SnapshotID, Generation: payload.Generation, CutoffMs: payload.DecisionCutoffMs}
	}
	for _, m := range c.Members {
		req.Members = append(req.Members, basketRankMember{Symbol: m.Coin, Timeframe: m.Timeframe})
	}
	return req
}

func startBasketRank(parent context.Context, rt *basketRuntime, deadlineMs int64, payload *marketPayload, now time.Time, run basketRankRunner, clock basketRankClock) (*basketRankJob, error) {
	if payload == nil {
		return nil, fmt.Errorf("no sealed basket payload")
	}
	windowEnd := time.UnixMilli(deadlineMs).Add(time.Duration(rt.Contract.EvaluationWindowSeconds) * time.Second)
	if !now.Before(windowEnd) {
		return nil, fmt.Errorf("the evaluation window closed at %s", windowEnd.UTC().Format(time.RFC3339))
	}
	req := basketRankRequestFor(rt, deadlineMs, payload)
	stdin, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("encode ranker request: %w", err)
	}
	if len(stdin) > marketPayloadMaxBytes {
		return nil, fmt.Errorf("ranker request is %d bytes, over the %d-byte cap", len(stdin), marketPayloadMaxBytes)
	}
	remaining := windowEnd.Sub(now)
	timeout := time.Duration(rt.Contract.RankerTimeoutSeconds) * time.Second
	if remaining < timeout {
		timeout = remaining
	}
	job := &basketRankJob{DeadlineMs: deadlineMs, Expected: req.Expected, WindowEnd: windowEnd, Timeout: timeout, Clock: clock, Payload: payload, done: make(chan struct{})}
	var ctx context.Context
	var cancel context.CancelFunc
	if clock == basketRankClockSimulated {
		ctx, cancel = context.WithTimeout(parent, remaining)
	} else {
		ctx, cancel = context.WithDeadline(parent, windowEnd)
	}
	go func() {
		defer close(job.done)
		defer cancel()
		stdout, stderr, runErr := run(ctx, stdin, timeout)
		if clock == basketRankClockSimulated {
			if ctxErr := ctx.Err(); ctxErr != nil {
				job.stderr = strings.TrimSpace(string(stderr))
				job.err = fmt.Errorf("the ranker context ended before its result was accepted (%v); output from a cancelled or timed-out run never freezes targets", ctxErr)
				return
			}
		}
		job.stderr = strings.TrimSpace(string(stderr))
		var res basketRankResult
		parseErr := json.Unmarshal(stdout, &res)
		switch {
		case parseErr == nil && res.Status == basketRankStatusInvalid:
			job.err = fmt.Errorf("the ranker refused its output: %s", res.Error)
		case runErr != nil:
			var timeoutErr *pythonScriptTimeoutError
			if errors.As(runErr, &timeoutErr) {
				job.err = fmt.Errorf("the ranker timed out: %v", runErr)
			} else {
				job.err = fmt.Errorf("the ranker failed: %v", runErr)
			}
		case parseErr != nil:
			job.err = fmt.Errorf("the ranker output is unreadable JSON: %v", parseErr)
		default:
			job.result = &res
		}
	}()
	return job, nil
}

func basketFiniteNumber(v float64) bool {
	return !math.IsNaN(v) && !math.IsInf(v, 0)
}

type basketTargetPolicy int

const (
	basketTargetsRanked basketTargetPolicy = iota
	basketTargetsOfflineBaseline
)

func validateBasketRankResult(rt *basketRuntime, job *basketRankJob, res *basketRankResult) (string, error) {
	return validateBasketTargetResult(rt, job, res, basketTargetsRanked)
}

func validateBasketTargetResult(rt *basketRuntime, job *basketRankJob, res *basketRankResult, policy basketTargetPolicy) (string, error) {
	if res == nil {
		return "", fmt.Errorf("the ranker returned no result")
	}
	if res.V != basketRankRequestVersion || res.BasketID != rt.Contract.BasketID || res.DeadlineMs != job.DeadlineMs {
		return "", fmt.Errorf("the ranker result names basket %q deadline %d, not the pending %q deadline %d", res.BasketID, res.DeadlineMs, rt.Contract.BasketID, job.DeadlineMs)
	}
	if res.SnapshotID != job.Expected.SnapshotID || res.Generation != job.Expected.Generation || res.CutoffMs != job.Expected.CutoffMs {
		return "", fmt.Errorf("the ranker result belongs to snapshot %s/%d cutoff %d, not the pending %s/%d cutoff %d", res.SnapshotID, res.Generation, res.CutoffMs, job.Expected.SnapshotID, job.Expected.Generation, job.Expected.CutoffMs)
	}
	switch res.Status {
	case basketRankStatusHold:
		reason := strings.TrimSpace(res.HoldReason)
		if reason == "" {
			reason = "the ranker held without a reason"
		}
		return reason, nil
	case basketRankStatusTargets:
	default:
		return "", fmt.Errorf("the ranker status %q is not targets or hold", res.Status)
	}
	coins := make(map[string]bool, len(rt.Contract.Members))
	for _, m := range rt.Contract.Members {
		coins[m.Coin] = true
	}
	eligible := make(map[string]bool)
	for sym, ev := range res.Eligibility {
		if !coins[sym] {
			return "", fmt.Errorf("the ranker eligibility record names %s, which is not a member coin", sym)
		}
		switch ev.Status {
		case "eligible":
			eligible[sym] = true
		case "excluded":
			if strings.TrimSpace(ev.Reason) == "" {
				return "", fmt.Errorf("the ranker excluded %s without a reason", sym)
			}
		default:
			return "", fmt.Errorf("the ranker eligibility status for %s is %q", sym, ev.Status)
		}
	}
	if len(res.Eligibility) != len(coins) {
		return "", fmt.Errorf("the ranker eligibility record covers %d of %d members", len(res.Eligibility), len(coins))
	}
	for _, m := range rt.Contract.Members {
		declared := basketListingExclusion(rt.Contract, m, job.DeadlineMs)
		if ev := res.Eligibility[m.Coin]; (declared != "") != (ev.Status == "excluded") {
			return "", fmt.Errorf("the ranker reported %s as %s, but the declared listing evidence at the deadline says %q", m.Coin, ev.Status, declared)
		}
	}
	if len(eligible) < rt.Contract.MinEligibleMembers {
		return fmt.Sprintf("the eligible population %d is below the minimum %d", len(eligible), rt.Contract.MinEligibleMembers), nil
	}
	if len(res.Weights) != len(eligible) || len(res.Scores) != len(eligible) {
		return "", fmt.Errorf("the ranker returned %d weights and %d scores for %d eligible members", len(res.Weights), len(res.Scores), len(eligible))
	}
	var abs, net float64
	long, short := false, false
	for sym, w := range res.Weights {
		if !eligible[sym] {
			return "", fmt.Errorf("the ranker weighted %s, which is not eligible", sym)
		}
		if _, ok := res.Scores[sym]; !ok || !basketFiniteNumber(res.Scores[sym]) {
			return "", fmt.Errorf("the ranker score for %s is missing or not finite", sym)
		}
		if !basketFiniteNumber(w) {
			return "", fmt.Errorf("the ranker weight for %s is not finite", sym)
		}
		abs += math.Abs(w)
		net += w
		if w > 0 {
			long = true
		}
		if w < 0 {
			short = true
		}
		if math.Abs(w)*rt.Contract.GrossBudgetUSD > rt.Contract.MaxCoinNotionalUSD*(1+1e-9) {
			return fmt.Sprintf("the %s weight %.6g allocates $%.2f, above the $%.2f per-coin cap; the capped allocation is infeasible and nothing is redistributed", sym, w, math.Abs(w)*rt.Contract.GrossBudgetUSD, rt.Contract.MaxCoinNotionalUSD), nil
		}
	}
	for sym := range res.Exclusions {
		if !eligible[sym] || res.Weights[sym] != 0 {
			return "", fmt.Errorf("the ranker exclusion of %s is inconsistent with its weight", sym)
		}
	}
	if abs > 1+1e-9 {
		return "", fmt.Errorf("absolute target weights sum to %.9g, above 1", abs)
	}
	if policy == basketTargetsRanked {
		if !long {
			return "the long side is empty", nil
		}
		if !short {
			return "the short side is empty", nil
		}
	}
	if math.Abs(net)*rt.Contract.GrossBudgetUSD > rt.Contract.MaxTransitionNetUSD*(1+1e-9) {
		return fmt.Sprintf("the target net exposure $%.2f exceeds max_transition_net_usd $%.2f, so no legal transition reaches it", math.Abs(net)*rt.Contract.GrossBudgetUSD, rt.Contract.MaxTransitionNetUSD), nil
	}
	return "", nil
}

func freezeBasketTargets(rt *basketRuntime, res *basketRankResult, payload *marketPayload, lots *hlLotMetadataCache) (*basketTargets, string) {
	t := &basketTargets{Version: basketTargetsVersion, GrossBudgetUSD: rt.Contract.GrossBudgetUSD, Members: make(map[string]basketMemberTarget, len(rt.Contract.Members))}
	threshold := hlVenueCloseGateThresholdUSD()
	for _, m := range rt.Contract.Members {
		mt := basketMemberTarget{StrategyID: m.StrategyID, Coin: m.Coin}
		ev := res.Eligibility[m.Coin]
		if ev.Status != "eligible" {
			mt.Excluded = ev.Reason
			t.Members[m.StrategyID] = mt
			continue
		}
		if reason, ok := res.Exclusions[m.Coin]; ok {
			mt.Excluded = reason
		}
		mt.Weight = res.Weights[m.Coin]
		mt.Score = res.Scores[m.Coin]
		mt.TargetNotionalUSD = mt.Weight * rt.Contract.GrossBudgetUSD
		px, why := basketDeadlineClose(payload, m, res.DeadlineMs)
		if why != "" {
			return nil, why
		}
		if !basketQtyEqual(px, ev.Mark) {
			return nil, fmt.Sprintf("the ranker reported a %s deadline close of %v, the seal holds %v", m.Coin, ev.Mark, px)
		}
		mt.MarkPx = px
		if mt.Weight != 0 {
			lot := hlLotLookup{Coin: m.Coin, Reason: "no venue metadata source"}
			if lots != nil {
				lot = lots.Lookup(m.Coin)
			}
			if !lot.Known {
				return nil, fmt.Sprintf("the lot size for %s is unknown (%s)", m.Coin, lot.Reason)
			}
			qty := hlFloorLotSize(math.Abs(mt.TargetNotionalUSD)/mt.MarkPx, lot.SzDecimals)
			if qty*mt.MarkPx < threshold {
				qty = 0
			}
			if mt.Weight < 0 {
				qty = -qty
			}
			mt.TargetQty = qty
			mt.ResidualUSD = mt.TargetNotionalUSD - qty*mt.MarkPx
		}
		t.Members[m.StrategyID] = mt
	}
	return t, ""
}

func basketDeadlineClose(payload *marketPayload, m basketContractMember, deadlineMs int64) (float64, string) {
	if payload == nil {
		return 0, "no ranking seal"
	}
	frame, ok := payload.Frames[feedKeyFor(m.Coin, m.Timeframe).PayloadID()]
	if !ok || frame.Timing == nil || len(frame.Timing.Bars) != len(frame.Rows) {
		return 0, fmt.Sprintf("the ranking seal has no timed %s frame", m.Coin)
	}
	for i, bar := range frame.Timing.Bars {
		open, okOpen := basketTimingInt(bar[0])
		closed, okClosed := bar[2].(bool)
		if !okOpen || !okClosed || !closed || open+m.IntervalMs != deadlineMs {
			continue
		}
		px := frame.Rows[i].Close
		if !finitePositive(px) {
			return 0, fmt.Sprintf("the %s deadline candle close %v is not a finite positive price", m.Coin, px)
		}
		return px, ""
	}
	return 0, fmt.Sprintf("the ranking seal has no %s candle closing at the deadline", m.Coin)
}

func basketTimingInt(v any) (int64, bool) {
	switch n := v.(type) {
	case int64:
		return n, true
	case int:
		return int64(n), true
	case float64:
		return int64(n), n == math.Trunc(n)
	}
	return 0, false
}

func (t *basketTargets) JSON() string {
	b, err := json.Marshal(t)
	if err != nil {
		return ""
	}
	return string(b)
}

func basketRankEvidenceJSON(res *basketRankResult) string {
	if res == nil {
		return ""
	}
	type evidence struct {
		Eligibility map[string]basketRankEligibility `json:"eligibility"`
		Scores      map[string]float64               `json:"scores,omitempty"`
		Exclusions  map[string]string                `json:"exclusions,omitempty"`
	}
	b, err := json.Marshal(evidence{Eligibility: res.Eligibility, Scores: res.Scores, Exclusions: res.Exclusions})
	if err != nil {
		return ""
	}
	return string(b)
}

func basketListingExclusion(c basketContract, m basketContractMember, deadlineMs int64) string {
	fact, ok := c.Listings[m.Coin]
	if !ok || fact.KnownAtMs > deadlineMs || m.IntervalMs <= 0 {
		return ""
	}
	if fact.ListedAtMs > deadlineMs {
		return "not listed at the deadline"
	}
	if fact.DelistedAtMs != 0 && fact.DelistedAtMs <= deadlineMs {
		return "delisted at or before the deadline"
	}
	if bars := (deadlineMs - fact.ListedAtMs) / m.IntervalMs; bars < int64(c.MinHistoryBars) {
		return fmt.Sprintf("listed %d bars before the deadline, needs %d", bars, c.MinHistoryBars)
	}
	return ""
}

func basketPayloadSpecs(rt *basketRuntime, deadlineMs int64) ([]marketPayloadFrameSpec, []string) {
	specs := make([]marketPayloadFrameSpec, 0, len(rt.Contract.Members))
	coins := make([]string, 0, len(rt.Contract.Members))
	for _, m := range rt.Contract.Members {
		coins = append(coins, m.Coin)
		if basketListingExclusion(rt.Contract, m, deadlineMs) != "" {
			continue
		}
		specs = append(specs, marketPayloadFrameSpec{Key: feedKeyFor(m.Coin, m.Timeframe), Required: basketFrameLookback(rt.Contract.LookbackBars), Timing: true})
	}
	sort.Strings(coins)
	return specs, coins
}

func basketFrameLookback(lookbackBars int) int {
	return lookbackBars + 1
}

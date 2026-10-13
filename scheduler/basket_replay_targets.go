package main

import (
	"context"
	"encoding/json"
	"fmt"
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
		contract := basketContract{Listings: req.Listings, MinHistoryBars: req.MinHistoryBars}
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
			res.Eligibility[m.Symbol] = basketRankEligibility{Symbol: m.Symbol, Status: "eligible", ClosedBars: req.LookbackBars, Mark: px, BoundaryMs: req.DeadlineMs}
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

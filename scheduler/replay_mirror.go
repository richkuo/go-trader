package main

import (
	"fmt"
	"math"
	"strings"
	"sync"
	"time"
)

var replayMirrorProgress = struct {
	sync.Mutex
	last map[string]int64
}{last: make(map[string]int64)}

func replayMirrorLastApplied(strategyID string) int64 {
	replayMirrorProgress.Lock()
	defer replayMirrorProgress.Unlock()
	return replayMirrorProgress.last[strategyID]
}

func replayMirrorSetLastApplied(strategyID string, id int64) {
	replayMirrorProgress.Lock()
	defer replayMirrorProgress.Unlock()
	if id > replayMirrorProgress.last[strategyID] {
		replayMirrorProgress.last[strategyID] = id
	}
}

func replayMirrorResetProgress(strategyID string) {
	replayMirrorProgress.Lock()
	defer replayMirrorProgress.Unlock()
	delete(replayMirrorProgress.last, strategyID)
}

func syncReplayMirrorWatermarkSource(sc StrategyConfig, s *StrategyState, sourceID string, logger *StrategyLogger) bool {
	if s == nil || sourceID == "" {
		return false
	}
	stored := strings.TrimSpace(s.ReplayMirrorWatermarkSource)
	if stored == "" {
		stored = sc.ID
	}
	if stored == sourceID {
		s.ReplayMirrorWatermarkSource = sourceID
		return false
	}
	prev := s.ReplayMirrorWatermark
	s.ReplayMirrorWatermark = 0
	s.ReplayMirrorWatermarkSource = sourceID
	replayMirrorResetProgress(sc.ID)
	if logger != nil {
		logger.Warn("Replay mirror: source changed %q -> %q — watermark reset (was %d); every pending row of the new source replays from the start (#1510)",
			stored, sourceID, prev)
	}
	return true
}

const (
	replayDriftKindOpenWhileHolding       = "open-while-holding"
	replayDriftKindScaleInWhileMismatched = "scale-in-while-mismatched"
	replayDriftKindPartialCloseWhileFlat  = "partial-close-while-flat"
	replayDriftKindOpenWithoutEntryATR    = "open-without-entry-atr"
)

type replayDriftKey struct {
	strategyID string
	kind       string
}

type replayDriftSlot struct {
	lastNotifiedAt time.Time
}

type replayDriftTracker struct {
	mu      sync.Mutex
	entries map[replayDriftKey]*replayDriftSlot
}

var replayDriftAlerts = &replayDriftTracker{}

var replayDriftWarn func(msg string)

func (t *replayDriftTracker) Record(strategyID, kind string, now time.Time) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.entries == nil {
		t.entries = make(map[replayDriftKey]*replayDriftSlot)
	}
	k := replayDriftKey{strategyID: strategyID, kind: kind}
	e := t.entries[k]
	if e == nil {
		e = &replayDriftSlot{}
		t.entries[k] = e
	}
	if e.lastNotifiedAt.IsZero() || now.Sub(e.lastNotifiedAt) >= effectiveAlertThrottleInterval() {
		e.lastNotifiedAt = now
		return true
	}
	return false
}

func (t *replayDriftTracker) reset() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.entries = make(map[replayDriftKey]*replayDriftSlot)
}

func sendReplayDriftWarns(msgs []string) {
	if replayDriftWarn == nil {
		return
	}
	for _, msg := range msgs {
		if msg != "" {
			replayDriftWarn(msg)
		}
	}
}

func replayDriftMaybeNotify(strategyID, kind, msg string, now time.Time) string {
	if !replayDriftAlerts.Record(strategyID, kind, now) {
		return ""
	}
	return msg
}

func formatReplayDriftDM(strategyID, kind, detail string) string {
	return fmt.Sprintf("paper replay drift [%s] %s: %s — skipping (audit the live/paper pair) (#1436)", strategyID, kind, detail)
}

func appendReplayDriftDM(dst *[]string, strategyID, kind, detail string, now time.Time) {
	if msg := replayDriftMaybeNotify(strategyID, kind, formatReplayDriftDM(strategyID, kind, detail), now); msg != "" {
		*dst = append(*dst, msg)
	}
}

func applyReplayedLiveDecisions(sc StrategyConfig, s *StrategyState, pending []ReplayDecision, price float64, result *HyperliquidResult, cfg *Config, logger *StrategyLogger) (appliedIDs []int64, trades int, details []string, driftDMs []string) {
	defer suspendEagerTradePersist()()
	defer suspendEagerDiagnosticsPersist()()

	lastApplied := replayMirrorLastApplied(sc.ID)
	if s.ReplayMirrorWatermark > lastApplied {
		lastApplied = s.ReplayMirrorWatermark
	}
	markApplied := func(id int64) {
		appliedIDs = append(appliedIDs, id)
		if id > lastApplied {
			lastApplied = id
		}
	}
	now := time.Now()
	for _, row := range pending {
		if row.DecisionID <= lastApplied {
			markApplied(row.DecisionID)
			continue
		}
		switch row.DecisionType {
		case ReplayDecisionOpen:
			if pos := s.Positions[row.Symbol]; pos != nil && pos.Quantity > 0 {
				logger.Warn("Replay mirror: live opened %s %s %.6f @ $%.4f but paper already holds qty=%.6f — skipping open (drift; audit the live/paper pair) (#1431)",
					row.Side, row.Symbol, row.Quantity, row.ReferencePrice, pos.Quantity)
				appendReplayDriftDM(&driftDMs, sc.ID, replayDriftKindOpenWhileHolding,
					fmt.Sprintf("live opened %s %s %.6f @ $%.4f but paper already holds qty=%.6f", row.Side, row.Symbol, row.Quantity, row.ReferencePrice, pos.Quantity), now)
				markApplied(row.DecisionID)
				continue
			}
			if sc.ClosedBarDecisions && !replayRowEntryATRValid(row) {
				logger.Warn("Replay mirror: live opened %s %s %.6f @ $%.4f but the row carries no valid entry ATR (%g) — skipping open, paper stays flat (drift; audit the live/paper pair) (closed_bar_decisions, #1712)",
					row.Side, row.Symbol, row.Quantity, row.ReferencePrice, row.EntryATR)
				appendReplayDriftDM(&driftDMs, sc.ID, replayDriftKindOpenWithoutEntryATR,
					fmt.Sprintf("live opened %s %s %.6f @ $%.4f with no valid entry ATR (%g); paper did not book it", row.Side, row.Symbol, row.Quantity, row.ReferencePrice, row.EntryATR), now)
				markApplied(row.DecisionID)
				continue
			}
			if t, detail := replayBookOpen(sc, s, row, result, cfg, logger); t > 0 {
				trades += t
				details = append(details, detail)
			} else {
				logger.Warn("Replay mirror: open %s %s %.6f @ $%.4f booked no trade — marking applied to avoid wedging the mirror (#1431)",
					row.Side, row.Symbol, row.Quantity, row.ReferencePrice)
			}
			markApplied(row.DecisionID)
		case ReplayDecisionScaleIn:
			pos := s.Positions[row.Symbol]
			if pos == nil || pos.Quantity <= 0 || pos.Side != row.Side {
				heldSide, heldQty := "", 0.0
				if pos != nil {
					heldSide, heldQty = pos.Side, pos.Quantity
				}
				logger.Warn("Replay mirror: live scaled in %s %s +%.6f @ $%.4f but paper holds %s qty=%.6f — skipping add (drift; audit the live/paper pair) (#1431)",
					row.Side, row.Symbol, row.Quantity, row.ReferencePrice, heldSide, heldQty)
				appendReplayDriftDM(&driftDMs, sc.ID, replayDriftKindScaleInWhileMismatched,
					fmt.Sprintf("live scaled in %s %s +%.6f @ $%.4f but paper holds %s qty=%.6f", row.Side, row.Symbol, row.Quantity, row.ReferencePrice, heldSide, heldQty), now)
				markApplied(row.DecisionID)
				continue
			}
			addPx := ApplyAdverseSlippage(row.ReferencePrice, pos.Side == "long")
			n, trade := applyPerpsScaleIn(s, sc, row.Symbol, addPx, row.Quantity, 0, "", false, logger)
			if n > 0 && trade != nil {
				trade.Timestamp = row.DecidedAt
				trade.Details = fmt.Sprintf("%s [replay_live_mirror]", trade.Details)
				recordPositionOpen(s, sc, trade, pos)
				trades += n
				details = append(details, fmt.Sprintf("[%s] REPLAY SCALE-IN %s +%.6f @ $%.2f", sc.ID, row.Symbol, row.Quantity, addPx))
			} else {
				logger.Warn("Replay mirror: scale-in %s %s +%.6f @ $%.4f booked no trade — marking applied to avoid wedging the mirror (#1431)",
					row.Side, row.Symbol, row.Quantity, row.ReferencePrice)
			}
			markApplied(row.DecisionID)
		case ReplayDecisionPartialClose:
			pos := s.Positions[row.Symbol]
			if pos == nil || pos.Quantity <= 0 {
				logger.Warn("Replay mirror: live partially closed %s %.6f but paper is flat — skipping (drift; audit the live/paper pair) (#1431)",
					row.Symbol, row.Quantity)
				appendReplayDriftDM(&driftDMs, sc.ID, replayDriftKindPartialCloseWhileFlat,
					fmt.Sprintf("live partially closed %s %.6f but paper is flat", row.Symbol, row.Quantity), now)
				markApplied(row.DecisionID)
				continue
			}
			preQty := pos.Quantity
			preInit := pos.InitialQuantity
			closePx, closeLabel := replayClosePrice(row, pos.Side, price, logger)
			if bookPerpsPartialCloseWithFillFee(s, row.Symbol, row.Quantity, closePx, 0, false, "", "replay_live_mirror", "Live mirror partial close"+closeLabel, "Live mirror partial close", logger) {
				if replayCloseReasonIsTakeProfit(row.CloseReason) {
					if cur := s.Positions[row.Symbol]; cur != nil && cur.Quantity > 0 {
						recordPaperUnifiedTPConsumption(sc, cur, preQty, preInit)
					}
				}
				trades++
				details = append(details, fmt.Sprintf("[%s] REPLAY PARTIAL CLOSE %s %.6f @ $%.2f", sc.ID, row.Symbol, row.Quantity, closePx))
			} else {
				logger.Warn("Replay mirror: partial close %s %.6f booked no trade — marking applied to avoid wedging the mirror (#1431)", row.Symbol, row.Quantity)
			}
			markApplied(row.DecisionID)
		case ReplayDecisionFullClose:
			pos := s.Positions[row.Symbol]
			if pos == nil || pos.Quantity <= 0 {
				logger.Info("Replay mirror: live closed %s (%s) but paper is already flat — nothing to replay (#1431)", row.Symbol, row.CloseReason)
				markApplied(row.DecisionID)
				continue
			}
			closePx, closeLabel := replayClosePrice(row, pos.Side, price, logger)
			if bookPerpsClose(s, row.Symbol, closePx, "replay_live_mirror", "Live mirror close"+closeLabel, "Live mirror close", logger) {
				trades++
				details = append(details, fmt.Sprintf("[%s] REPLAY CLOSE %s @ $%.2f (live reason: %s)", sc.ID, row.Symbol, closePx, row.CloseReason))
			} else {
				logger.Warn("Replay mirror: full close %s booked no trade — marking applied to avoid wedging the mirror (#1431)", row.Symbol)
			}
			markApplied(row.DecisionID)
		default:
			logger.Warn("Replay mirror: unknown decision_type %q for %s (id=%d) — skipping (#1431)", row.DecisionType, row.Symbol, row.DecisionID)
			markApplied(row.DecisionID)
		}
	}
	replayMirrorSetLastApplied(sc.ID, lastApplied)
	if lastApplied > s.ReplayMirrorWatermark {
		s.ReplayMirrorWatermark = lastApplied
	}
	return appliedIDs, trades, details, driftDMs
}

func replayClosePrice(row ReplayDecision, posSide string, cyclePrice float64, logger *StrategyLogger) (float64, string) {
	if !replayCloseReasonIsTakeProfit(row.CloseReason) {
		return ApplyAdverseSlippage(cyclePrice, posSide == "short"), ""
	}
	if px := row.ReferencePrice; px > 0 && !math.IsInf(px, 0) && !math.IsNaN(px) {
		return px, ""
	}
	logger.Warn("Replay mirror: take-profit close %s (%s) carries no usable live tier price (%g) — booking at the cycle price $%.4f with no slippage (tier_price_source=cycle_price)", row.Symbol, row.CloseReason, row.ReferencePrice, cyclePrice)
	return cyclePrice, " tier_price_source=cycle_price"
}

func replayBookOpen(sc StrategyConfig, s *StrategyState, row ReplayDecision, result *HyperliquidResult, cfg *Config, logger *StrategyLogger) (int, string) {
	sig := 1
	if row.Side == "short" {
		sig = -1
	}
	var indicators map[string]interface{}
	var regime *RegimeConfig
	if result != nil {
		indicators = result.Indicators
	}
	if sc.ClosedBarDecisions {
		indicators = map[string]interface{}{"atr": row.EntryATR}
	}
	if cfg != nil {
		regime = cfg.Regime
	}
	sizing := PerpsSizingFor(sc, row.ReferencePrice, indicatorsATRValue(indicators))
	exec, err := ExecutePerpsSignalWithLeverageDeferredOpen(s, sig, row.Symbol, ApplyAdverseSlippage(row.ReferencePrice, sig == 1), sizing, row.Quantity, "", 0, DirectionBoth, 0, logger)
	if err != nil || exec.TradesExecuted == 0 || exec.OpenTrade == nil {
		if err != nil {
			logger.Error("Replay mirror: open booking failed for %s: %v (#1431)", row.Symbol, err)
		}
		return 0, ""
	}
	exec.OpenTrade.Timestamp = row.DecidedAt
	exec.OpenTrade.Details = fmt.Sprintf("%s [replay_live_mirror]", exec.OpenTrade.Details)
	if pos, ok := s.Positions[row.Symbol]; ok && pos != nil {
		pos.OpenedAt = row.DecidedAt
	}
	if sc.ClosedBarDecisions {
		if pos := s.Positions[row.Symbol]; pos != nil {
			pos.EntryATR = row.EntryATR
		}
	} else {
		stampEntryATRIfOpened(s, row.Symbol, indicators)
	}
	if result != nil {
		stampPositionRegimeIfOpened(s, row.Symbol, regimePayloadValue(result.Regime), sc, regime)
	}
	if pos := s.Positions[row.Symbol]; pos != nil {
		if row.EntryATR > 0 {
			pos.EntryATR = row.EntryATR
		}
		if row.Regime != "" {
			pos.Regime = row.Regime
		}
	}
	var pos *Position
	if p, ok := s.Positions[row.Symbol]; ok {
		pos = p
	}
	recordPositionOpen(s, sc, exec.OpenTrade, pos)
	return exec.TradesExecuted, fmt.Sprintf("[%s] REPLAY OPEN %s %s %.6f @ $%.2f", sc.ID, row.Side, row.Symbol, row.Quantity, row.ReferencePrice)
}

func mergeTradeDetails(existing string, parts ...string) string {
	out := make([]string, 0, 1+len(parts))
	if existing != "" {
		out = append(out, existing)
	}
	for _, p := range parts {
		if p != "" {
			out = append(out, p)
		}
	}
	return strings.Join(out, "; ")
}

func replayRowEntryATRValid(row ReplayDecision) bool {
	atr := row.EntryATR
	return atr > 0 && !math.IsInf(atr, 0) && !math.IsNaN(atr)
}

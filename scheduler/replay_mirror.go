package main

import (
	"fmt"
	"strings"
	"sync"
)

// replay_mirror.go — #1431 paper side of the live decision replay log.
//
// A PAPER HL perps strategy with replay_sharing="live_mirror" does not open
// from its own check-script signal: each cycle it consumes the pending rows
// the live deployment wrote (replay_log.go) and books the same actions
// against its virtual book — opens/scale-ins at live's ACTUAL filled
// quantity and VWAP, closes at paper's current mark (the only sanctioned
// drift; live's exit fill is asynchronous — see the issue's replay-slippage
// risk). Paper's own close re-evaluation and trailing/fixed-ATR SL loops keep
// running as a backstop, so a close CAN beat the mirror; the later full_close
// row then finds a flat book and is a no-op.
//
// Hedge legs need no replay rows: the state-derived hedge reconciler (#1159)
// runs after the mirror applies and converges the hedge from the replayed
// primary, exactly as it does for a natively-opened one.

// replayMirrorProgress tracks the highest decision ID each mirrored strategy
// has applied this process lifetime. The durable record is TWO-layered: the
// shared log's replay_status (what the live writer sees) and the paper state
// DB's strategies.replay_mirror_watermark (persisted in the SAME
// SaveStrategyBook transaction as the book mutation). This in-memory
// high-water covers the window where a row was applied to state but neither
// durable record has it yet — without all three, a restart in the
// apply→save→mark gap would either drop a mirrored trade (marked but never
// saved) or double-book a scale_in (saved but never marked; opens/closes
// self-heal via the flat/open guards, an add does not). Eager InsertTrade
// and diagnostics inserts are suspended during apply so those rows join the
// SaveStrategyBook transaction; otherwise a kill mid-save would leave a
// committed trade or orphaned diagnostics row while rolling back the watermark.
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

// applyReplayedLiveDecisions books every pending live decision for a mirrored
// paper strategy against its virtual state. MUST be called under mu.Lock (it
// mutates positions/cash/trades). price is paper's current mark from this
// cycle's check; result carries the check's indicators/regime so replayed
// opens get the same EntryATR/regime stamping a native paper open would.
//
// Rows are applied strictly in decision_id order. Every visited row is
// returned in appliedIDs (drift skips included — a row the mirror cannot
// apply, e.g. an open while paper is already holding, is an operator-visible
// WARN, not a retry loop: leaving it pending would wedge every later row
// behind it). Live downtime needs no special-casing: a gap simply produces
// no rows and paper holds its last replayed state until the next decision
// appears (resume policy (c) from the issue).
func applyReplayedLiveDecisions(sc StrategyConfig, s *StrategyState, pending []ReplayDecision, price float64, result *HyperliquidResult, cfg *Config, logger *StrategyLogger) (appliedIDs []int64, trades int, details []string) {
	// Replay bookings must not eager-InsertTrade. recordPositionOpen /
	// bookPerpsClose / bookPerpsPartialCloseWithFillFee all call RecordTrade,
	// which otherwise commits a trades row immediately. A kill during the
	// caller's SaveStrategyBook would then leave that row on disk while
	// rolling back positions + replay_mirror_watermark, and the next start
	// would re-apply and insert a second copy (#1435). RecordTrade still
	// appends in-memory (persisted=false); SaveStrategyBook inserts those
	// rows in the same transaction as the book and watermark.
	defer suspendEagerTradePersist()()
	// Same for #1147 diagnostics: recordClosedPosition otherwise eager-inserts
	// a trade_diagnostics row outside the save tx. A kill between that insert
	// and the save commit would leave an orphan that the retry duplicates.
	defer suspendEagerDiagnosticsPersist()()

	// The skip threshold is the max of the process-lifetime high-water and the
	// PERSISTED watermark: after a restart the in-memory map is empty and the
	// watermark is the only record that a row's book mutation already hit disk
	// (a crash between SaveState and MarkDecisionsApplied leaves such rows
	// pending in the shared log — they are re-marked below, never re-applied).
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
	for _, row := range pending {
		if row.DecisionID <= lastApplied {
			// Already applied to the persisted book (watermark) or earlier this
			// process (high-water) but the shared-log mark failed — skip the
			// state mutation, re-mark only.
			markApplied(row.DecisionID)
			continue
		}
		switch row.DecisionType {
		case ReplayDecisionOpen:
			if pos := s.Positions[row.Symbol]; pos != nil && pos.Quantity > 0 {
				logger.Warn("Replay mirror: live opened %s %s %.6f @ $%.4f but paper already holds qty=%.6f — skipping open (drift; audit the live/paper pair) (#1431)",
					row.Side, row.Symbol, row.Quantity, row.ReferencePrice, pos.Quantity)
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
				markApplied(row.DecisionID)
				continue
			}
			n, trade := applyPerpsScaleIn(s, sc, row.Symbol, row.ReferencePrice, row.Quantity, 0, "", false, logger)
			if n > 0 && trade != nil {
				// Mirror live's decision timestamp so hold-duration analytics
				// line up with the live book.
				trade.Timestamp = row.DecidedAt
				trade.Details = fmt.Sprintf("%s [replay_live_mirror]", trade.Details)
				recordPositionOpen(s, sc, trade, pos)
				trades += n
				details = append(details, fmt.Sprintf("[%s] REPLAY SCALE-IN %s +%.6f @ $%.2f", sc.ID, row.Symbol, row.Quantity, row.ReferencePrice))
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
				markApplied(row.DecisionID)
				continue
			}
			if bookPerpsPartialCloseWithFillFee(s, row.Symbol, row.Quantity, price, 0, false, "", "replay_live_mirror", "Live mirror partial close", "Live mirror partial close", logger) {
				trades++
				details = append(details, fmt.Sprintf("[%s] REPLAY PARTIAL CLOSE %s %.6f @ $%.2f", sc.ID, row.Symbol, row.Quantity, price))
			} else {
				logger.Warn("Replay mirror: partial close %s %.6f booked no trade — marking applied to avoid wedging the mirror (#1431)", row.Symbol, row.Quantity)
			}
			markApplied(row.DecisionID)
		case ReplayDecisionFullClose:
			pos := s.Positions[row.Symbol]
			if pos == nil || pos.Quantity <= 0 {
				// Expected whenever paper's own close re-evaluation or
				// trailing/fixed-ATR SL beat the mirror to the exit — the
				// acceptance criterion's trailing_stop_loss_paper carve-out.
				logger.Info("Replay mirror: live closed %s (%s) but paper is already flat — nothing to replay (#1431)", row.Symbol, row.CloseReason)
				markApplied(row.DecisionID)
				continue
			}
			if bookPerpsClose(s, row.Symbol, price, "replay_live_mirror", "Live mirror close", "Live mirror close", logger) {
				trades++
				details = append(details, fmt.Sprintf("[%s] REPLAY CLOSE %s @ $%.2f (live reason: %s)", sc.ID, row.Symbol, price, row.CloseReason))
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
		// Advance the durable cursor IN MEMORY; the caller's SaveStrategyBook
		// persists it in the same transaction as the book mutations above,
		// BEFORE the shared log's MarkDecisionsApplied runs.
		s.ReplayMirrorWatermark = lastApplied
	}
	return appliedIDs, trades, details
}

// replayBookOpen books a replayed live open against the flat paper book:
// exact live filled quantity at live's VWAP (fillQty semantics — no sizing,
// no slippage), modeled paper fee, and the same EntryATR/regime stamping a
// native paper open receives. Direction is forced to "both": the row's side
// IS the decision — a per-cycle regime_directional_policy resolution must not
// re-interpret (or reject) live's already-executed open.
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
	if cfg != nil {
		regime = cfg.Regime
	}
	sizing := PerpsSizingFor(sc, row.ReferencePrice, indicatorsATRValue(indicators))
	exec, err := ExecutePerpsSignalWithLeverageDeferredOpen(s, sig, row.Symbol, row.ReferencePrice, sizing, row.Quantity, "", 0, DirectionBoth, 0, logger)
	if err != nil || exec.TradesExecuted == 0 || exec.OpenTrade == nil {
		if err != nil {
			logger.Error("Replay mirror: open booking failed for %s: %v (#1431)", row.Symbol, err)
		}
		return 0, ""
	}
	// Mirror live's decision timestamp so hold-duration analytics line up.
	exec.OpenTrade.Timestamp = row.DecidedAt
	exec.OpenTrade.Details = fmt.Sprintf("%s [replay_live_mirror]", exec.OpenTrade.Details)
	if pos, ok := s.Positions[row.Symbol]; ok && pos != nil {
		pos.OpenedAt = row.DecidedAt
	}
	stampEntryATRIfOpened(s, row.Symbol, indicators)
	if result != nil {
		stampPositionRegimeIfOpened(s, row.Symbol, regimePayloadValue(result.Regime), sc, regime)
	}
	// Live's open-time stamps win over paper's own payload when the row
	// carries them: the mirror reproduces live's stop geometry (EntryATR is
	// the frozen basis every ATR stop/tier derives from) and regime label
	// even when the two deployments' payloads disagree on the same bar.
	// Rows written before the columns existed (0/"") keep the paper stamps.
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

// mergeTradeDetails concatenates operator-facing per-cycle digest fragments
// so a later booked action cannot silently drop an earlier one. Empty
// fragments are skipped. Used by the paper replay arm, which runs AFTER
// paper's own close/SL backstop in the same iteration and must not overwrite
// that earlier detail (#1435).
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

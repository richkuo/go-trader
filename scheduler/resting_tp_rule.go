package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"path/filepath"
	"reflect"
	"strings"
	"time"
)

const (
	restingTPTradeThroughKey     = "resting_tp_trade_through"
	restingTPRuleFlag            = "--resting-tp-rule-json"
	restingTPRuleVersion         = 1
	restingTPTradeThroughTicks   = 1
	restingTPHoldEntryTime       = "entry_time_unknown"
	restingTPHoldStopMayWiden    = "stop_may_widen"
	restingTPHoldStopUnarmed     = "stop_unarmed"
	restingTPCoverageTruncated   = "frame_truncated"
	restingTPUnsupportedDynamic  = dynamicCloseStrategyName
	restingTPProbeRuleJSONSample = `{"v":1,"k_ticks":1,"sz_decimals":5,"entry_time_ms":1,"stop_trigger_px":0,"hold_reason":"","scanned_through_ms":0,"prior_reach_px":null}`
)

var restingTPTierCloseNames = map[string]bool{
	"tiered_tp_atr":             true,
	"tiered_tp_atr_live":        true,
	"tiered_tp_atr_regime":      true,
	"tiered_tp_atr_live_regime": true,
}

type restingTPRuleRequest struct {
	V                int      `json:"v"`
	KTicks           int      `json:"k_ticks"`
	SzDecimals       *int     `json:"sz_decimals"`
	EntryTimeMs      int64    `json:"entry_time_ms"`
	StopTriggerPx    float64  `json:"stop_trigger_px"`
	HoldReason       string   `json:"hold_reason"`
	ScannedThroughMs int64    `json:"scanned_through_ms"`
	PriorReachPx     *float64 `json:"prior_reach_px"`
	PhaseOneStopPx   float64  `json:"-"`
}

type RestingTPRuleEcho struct {
	V                    int      `json:"v"`
	Enabled              bool     `json:"enabled"`
	Held                 bool     `json:"held"`
	HoldReason           string   `json:"hold_reason,omitempty"`
	KTicks               int      `json:"k_ticks"`
	SzDecimals           *int     `json:"sz_decimals"`
	CutoffMs             int64    `json:"cutoff_ms"`
	EntryTimeMs          int64    `json:"entry_time_ms"`
	EntryBarOpenMs       int64    `json:"entry_bar_open_ms"`
	StopTriggerPx        float64  `json:"stop_trigger_px"`
	Coverage             string   `json:"coverage"`
	ObservedBars         int      `json:"observed_bars"`
	FirstObservedOpenMs  int64    `json:"first_observed_open_ms"`
	LastObservedOpenMs   int64    `json:"last_observed_open_ms"`
	ObservedRowsSHA256   string   `json:"observed_rows_sha256"`
	ScannedThroughMs     int64    `json:"scanned_through_ms"`
	PriorReachPx         *float64 `json:"prior_reach_px"`
	NextScannedThroughMs int64    `json:"next_scanned_through_ms"`
	NextReachPx          *float64 `json:"next_reach_px"`
	ReachBarOpenMs       int64    `json:"reach_bar_open_ms"`
	StopReachedBarOpenMs int64    `json:"stop_reached_bar_open_ms"`
	Verdict              string   `json:"verdict,omitempty"`
}

func restingTPTradeThroughRawErrors(prefix string, entry map[string]json.RawMessage) []string {
	raw, ok := entry[restingTPTradeThroughKey]
	if !ok {
		return nil
	}
	switch string(bytes.TrimSpace(raw)) {
	case "true", "false":
		return nil
	}
	return []string{fmt.Sprintf("%s: %s must be the JSON literal true or false, got %s", prefix, restingTPTradeThroughKey, strings.TrimSpace(string(raw)))}
}

func restingTPCloseNames(sc StrategyConfig) []string {
	refs := sc.closeRefs()
	out := make([]string, 0, len(refs))
	for _, ref := range refs {
		out = append(out, strings.TrimSpace(ref.Name))
	}
	return out
}

func restingTPTradeThroughStrategyErrors(sc StrategyConfig, cfg *Config) []string {
	if !sc.RestingTPTradeThrough {
		return nil
	}
	prefix := fmt.Sprintf("strategy[%s]: %s", sc.ID, restingTPTradeThroughKey)
	if sc.Type != "perps" || sc.Platform != "hyperliquid" {
		return []string{fmt.Sprintf("%s supports only Hyperliquid perps paper strategies, got type=%s platform=%s", prefix, sc.Type, sc.Platform)}
	}
	if hyperliquidIsLive(sc.Args) {
		return []string{fmt.Sprintf("%s is paper-only: the venue decides live take-profit fills, so a live strategy cannot carry it", prefix)}
	}
	var errs []string
	if filepath.Clean(strings.TrimSpace(sc.Script)) != hyperliquidCheckScript {
		errs = append(errs, fmt.Sprintf("%s supports only the built-in Hyperliquid check %s, got script %q", prefix, hyperliquidCheckScript, sc.Script))
	}
	names := restingTPCloseNames(sc)
	tier := false
	for _, name := range names {
		if name == restingTPUnsupportedDynamic {
			errs = append(errs, fmt.Sprintf("%s does not support close strategy %s (Hyperliquid live-only)", prefix, name))
		}
		if restingTPTierCloseNames[name] {
			tier = true
		}
	}
	if !tier {
		errs = append(errs, fmt.Sprintf("%s needs a tier close strategy (tiered_tp_atr, tiered_tp_atr_live, tiered_tp_atr_regime or tiered_tp_atr_live_regime), got %v", prefix, names))
	}
	if replayMirrorPaperActive(sc) {
		errs = append(errs, fmt.Sprintf("%s does not support a replay-mirror paper strategy: the mirror books live take-profit closes at the live tier price", prefix))
	}
	if sc.AllowScaleIn {
		errs = append(errs, fmt.Sprintf("%s does not support allow_scale_in: the tier geometry must stay frozen", prefix))
	}
	_, timeframe := strategyArgSymbolTimeframe(sc.Args)
	if timeframe == "" {
		errs = append(errs, fmt.Sprintf("%s needs args <strategy> <symbol> <timeframe>", prefix))
		return errs
	}
	if !closedBarIntervalSupported(sc, timeframe) {
		errs = append(errs, fmt.Sprintf("%s does not support timeframe %q (fixed-duration Hyperliquid intervals only: %s)", prefix, timeframe, closedBarSupportedIntervals()))
		return errs
	}
	intervalMs, _ := hlCandleIntervalMs(timeframe)
	global := 0
	if cfg != nil {
		global = cfg.IntervalSeconds
	}
	if every := configuredStrategyIntervalSeconds(sc, global); int64(every)*1000 > intervalMs {
		errs = append(errs, fmt.Sprintf("%s needs a check interval at or below the %s bar (got %ds); the limit does not guarantee one check per bar, and each completed bar is stop-tested once, on the first check after it completes", prefix, timeframe, every))
	}
	return errs
}

func restingTPTradeThroughConfigErrors(cfg *Config) []string {
	if cfg == nil {
		return nil
	}
	var errs []string
	for _, sc := range cfg.Strategies {
		errs = append(errs, restingTPTradeThroughStrategyErrors(sc, cfg)...)
	}
	return errs
}

func restingTPTradeThroughReloadErrors(sc, ns StrategyConfig) []string {
	if sc.RestingTPTradeThrough != ns.RestingTPTradeThrough {
		return []string{fmt.Sprintf("strategy[%s] %s changed (%t -> %t; restart required)", sc.ID, restingTPTradeThroughKey, sc.RestingTPTradeThrough, ns.RestingTPTradeThrough)}
	}
	return nil
}

func restingTPTradeThroughStateReloadErrors(sc, ns StrategyConfig, open bool) []string {
	if !open || (!sc.RestingTPTradeThrough && !ns.RestingTPTradeThrough) {
		return nil
	}
	if reflect.DeepEqual(sc.closeRefs(), ns.closeRefs()) {
		return nil
	}
	return []string{fmt.Sprintf("strategy[%s] close_strategy changed with open positions while %s is on (the tier geometry must stay frozen; flatten first or restart after close)", sc.ID, restingTPTradeThroughKey)}
}

func needsTimedSignalFrame(sc StrategyConfig) bool {
	return sc.ClosedBarDecisions || sc.RestingTPTradeThrough
}

func restingTPPaperStopOwnerActive(sc StrategyConfig, pos *Position) bool {
	snap := hyperliquidProtectionPositionSnapshot(pos)
	if snap == nil {
		return false
	}
	return effectiveTrailingStopPct(sc, snap) > 0 || effectiveFixedStopLossATRPct(sc, snap) > 0 ||
		(EffectiveStopLossPct(sc) > 0 && snap.AvgCost > 0)
}

func restingTPRuleRequestFor(sc StrategyConfig, pos *Position, ctx PositionCtx) *restingTPRuleRequest {
	if !sc.RestingTPTradeThrough || sc.Type != "perps" || sc.Platform != "hyperliquid" || hyperliquidIsLive(sc.Args) {
		return nil
	}
	if pos == nil || ctx.Quantity <= 0 {
		return nil
	}
	req := &restingTPRuleRequest{
		V:              restingTPRuleVersion,
		KTicks:         restingTPTradeThroughTicks,
		StopTriggerPx:  restingTPScanStopTriggerPx(ctx),
		PhaseOneStopPx: ctx.StopLossTriggerPx,
	}
	if !ctx.OpenedAt.IsZero() && ctx.OpenedAt.UnixMilli() > 0 {
		req.EntryTimeMs = ctx.OpenedAt.UnixMilli()
	}
	if ctx.RestingTPScannedOpenMs > 0 {
		req.ScannedThroughMs = ctx.RestingTPScannedOpenMs
		if ctx.RestingTPReachPx > 0 && !math.IsInf(ctx.RestingTPReachPx, 0) {
			reach := ctx.RestingTPReachPx
			req.PriorReachPx = &reach
		}
	}
	if lot := hlLotMetadata.Peek(hyperliquidSymbol(sc.Args)); lot.Known {
		d := lot.SzDecimals
		req.SzDecimals = &d
	}
	switch {
	case req.EntryTimeMs <= 0:
		req.HoldReason = restingTPHoldEntryTime
	case ctx.RatchetFallbackNormalizePending:
		req.HoldReason = restingTPHoldStopMayWiden
	case ctx.StopLossTriggerPx <= 0 && restingTPPaperStopOwnerActive(sc, pos):
		req.HoldReason = restingTPHoldStopUnarmed
	}
	return req
}

func restingTPScanStopTriggerPx(ctx PositionCtx) float64 {
	current := ctx.StopLossTriggerPx
	if ctx.RestingTPScannedOpenMs <= 0 {
		return current
	}
	stored := ctx.RestingTPStopTriggerPx
	if current <= 0 || stored <= 0 {
		return 0
	}
	if ctx.Side == "short" {
		return math.Max(current, stored)
	}
	return math.Min(current, stored)
}

func restingTPRuleJSON(req *restingTPRuleRequest) (json.RawMessage, error) {
	if req == nil {
		return nil, nil
	}
	blob, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	return json.RawMessage(blob), nil
}

func appendRestingTPRuleArg(args []string, ctx PositionCtx) ([]string, error) {
	blob, err := restingTPRuleJSON(ctx.RestingTP)
	if err != nil || blob == nil {
		return args, err
	}
	return append(args, restingTPRuleFlag+"="+string(blob)), nil
}

func restingTPNextScanError(sent *restingTPRuleRequest, echo *RestingTPRuleEcho, side string) string {
	if echo.NextScannedThroughMs < sent.ScannedThroughMs {
		return "the next scan watermark moved backwards"
	}
	next := echo.NextReachPx
	if echo.Held && (echo.NextScannedThroughMs != sent.ScannedThroughMs || !floatPtrEqual(next, sent.PriorReachPx)) {
		return "a held rule advanced the scan"
	}
	if next == nil {
		if sent.PriorReachPx != nil {
			return "the next reach is missing"
		}
		return ""
	}
	if *next <= 0 || math.IsInf(*next, 0) || math.IsNaN(*next) || echo.NextScannedThroughMs <= 0 {
		return "the next reach or watermark is not usable"
	}
	if sent.PriorReachPx != nil {
		prior := *sent.PriorReachPx
		if (side == "short" && *next > prior) || (side != "short" && *next < prior) {
			return "the next reach falls behind the prior reach"
		}
	}
	return ""
}

func restingTPRuleContractError(sent *restingTPRuleRequest, side string, fields StrategyDecisionFields) string {
	echo := fields.RestingTPRule
	const redeploy = " (redeploy with scripts/update.sh so Go and Python match)"
	switch {
	case sent == nil && echo != nil:
		return "resting take-profit rule contract mismatch: no rule was sent but the check returned a resting_tp_rule echo" + redeploy
	case sent == nil:
		return ""
	case echo == nil || !echo.Enabled:
		return "resting take-profit rule contract mismatch: sent a resting_tp_rule but the check returned no echo; holding this signal because the check may have used the mark rule" + redeploy
	case echo.V != sent.V || echo.KTicks != sent.KTicks || !intPtrEqual(echo.SzDecimals, sent.SzDecimals) || echo.EntryTimeMs != sent.EntryTimeMs || echo.StopTriggerPx != sent.StopTriggerPx ||
		echo.ScannedThroughMs != sent.ScannedThroughMs || !floatPtrEqual(echo.PriorReachPx, sent.PriorReachPx):
		return "resting take-profit rule contract mismatch: the echo disagrees with the sent k_ticks, sz_decimals, entry_time_ms, stop_trigger_px, scanned_through_ms or prior_reach_px; holding this signal" + redeploy
	case restingTPNextScanError(sent, echo, side) != "":
		return "resting take-profit rule contract mismatch: " + restingTPNextScanError(sent, echo, side) + "; holding this signal" + redeploy
	case echo.Held && fields.CloseTierFillPrice > 0:
		return "resting take-profit rule contract mismatch: the rule was held but the check returned a tier fill price; holding this signal" + redeploy
	case fields.CloseFraction > 0 && restingTPTierCloseNames[strings.TrimSpace(fields.CloseStrategy)] &&
		!(fields.CloseTierFillPrice > 0 && !math.IsInf(fields.CloseTierFillPrice, 0) && !math.IsNaN(fields.CloseTierFillPrice)):
		return "resting take-profit rule contract mismatch: a tier close returned no close_tier_fill_price, so it would book at the cycle price; holding this signal" + redeploy
	}
	return ""
}

func logRestingTPRule(sc StrategyConfig, posCtx PositionCtx, fields StrategyDecisionFields, logger *StrategyLogger) {
	echo := fields.RestingTPRule
	if logger == nil || posCtx.RestingTP == nil || echo == nil {
		return
	}
	position := fmt.Sprintf("%d", posCtx.RestingTP.EntryTimeMs)
	if echo.Held {
		if logger.Changed("resting-tp-hold", position+"|"+echo.HoldReason) {
			logger.Warn("Resting take-profit rule held: %s; tier take-profits wait, stops and other closes continue (#1727)", echo.HoldReason)
		}
	} else {
		logger.Changed("resting-tp-hold", position+"|")
	}
	if echo.Coverage == restingTPCoverageTruncated && logger.Changed("resting-tp-truncated", position) {
		logger.Warn("Resting take-profit rule: the candle frame starts after the entry bar (frame_truncated); a tier crossed in a bar outside the frame cannot fill, so fills can only be missed (#1727)")
	}
	if echo.StopReachedBarOpenMs > 0 && logger.Changed("resting-tp-stop-bar", position+"|"+fmt.Sprintf("%d", echo.StopReachedBarOpenMs)) {
		logger.Warn("Resting take-profit rule: the bar opened %s reached the stop trigger $%.4f; tiers crossed in that bar do not fill (stop-first); later bars are scanned from the next check (#1727)", formatClosedBarMs(echo.StopReachedBarOpenMs), echo.StopTriggerPx)
	}
	logger.Debug("Resting take-profit rule: %d observed bar(s) %s..%s (cutoff %s, entry bar %s, coverage %s, rows sha256 %s, verdict %s)",
		echo.ObservedBars, formatClosedBarMs(echo.FirstObservedOpenMs), formatClosedBarMs(echo.LastObservedOpenMs),
		formatClosedBarMs(echo.CutoffMs), formatClosedBarMs(echo.EntryBarOpenMs), echo.Coverage, echo.ObservedRowsSHA256, echo.Verdict)
}

func restingTPFillLogSuffix(result *HyperliquidResult) string {
	if result == nil || result.RestingTPRule == nil || result.RestingTPRule.ReachBarOpenMs <= 0 {
		return ""
	}
	return fmt.Sprintf(" (resting rule: traded through in the bar opened %s)", time.UnixMilli(result.RestingTPRule.ReachBarOpenMs).UTC().Format(time.RFC3339))
}

func ensureRestingTPLotMetadata(due []StrategyConfig) {
	seen := map[string]bool{}
	for _, sc := range due {
		if !sc.RestingTPTradeThrough || sc.Platform != "hyperliquid" || sc.Type != "perps" || hyperliquidIsLive(sc.Args) {
			continue
		}
		sym := hyperliquidSymbol(sc.Args)
		if sym == "" || seen[sym] {
			continue
		}
		seen[sym] = true
		hlLotMetadata.Ensure(sym)
	}
}

func recordRestingTPScan(s *StrategyState, symbol string, sent *restingTPRuleRequest, fields StrategyDecisionFields) bool {
	echo := fields.RestingTPRule
	if s == nil || sent == nil || echo == nil || echo.Held {
		return false
	}
	pos := s.Positions[symbol]
	if pos == nil || pos.Quantity <= 0 || pos.OpenedAt.IsZero() || pos.OpenedAt.UnixMilli() != sent.EntryTimeMs {
		return false
	}
	if pos.RestingTPScannedOpenMs != sent.ScannedThroughMs {
		return false
	}
	if echo.NextScannedThroughMs <= sent.ScannedThroughMs {
		return false
	}
	pos.RestingTPScannedOpenMs = echo.NextScannedThroughMs
	if echo.NextReachPx != nil {
		pos.RestingTPReachPx = *echo.NextReachPx
	}
	pos.RestingTPStopTriggerPx = sent.PhaseOneStopPx
	return true
}

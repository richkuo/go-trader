package main

import (
	"encoding/json"
	"fmt"
	"math"
	"strings"
)

const (
	sweepAbsorptionStrategyName = "sweep_absorption_ifvg"

	sweepAbsorptionATRPeriod       = 14
	sweepAbsorptionHTFTimeframe    = "1h"
	sweepAbsorptionHTFIntervalMs   = int64(3_600_000)
	sweepAbsorptionHTFLeft         = 2
	sweepAbsorptionHTFSearchMs     = int64(24 * 3_600_000)
	sweepAbsorptionHTFSlackRows    = 2
	sweepAbsorptionMaxDecisionBars = 200
	sweepAbsorptionAlignmentRows   = 1
	sweepAbsorptionDayMs           = int64(24 * 3_600_000)
)

type sweepAbsorptionIntBound struct {
	Name    string
	Default int64
	Min     int64
	Max     int64
}

var sweepAbsorptionIntParams = []sweepAbsorptionIntBound{
	{"session_start_hour_utc", 0, 0, 23},
	{"session_end_hour_utc", 8, 0, 23},
	{"sweep_window", 24, 1, 96},
	{"max_sweep_bars", 6, 1, 24},
	{"gap_window", 12, 1, 48},
	{"confirm_window", 6, 1, 48},
	{"setup_expiry", 12, 1, 96},
	{"expected_interval_ms", 300_000, 60_000, 1_800_000},
}

type sweepAbsorptionSupport struct {
	SessionMs     int64
	SessionBars   int64
	PreludeBars   int64
	PostBars      int64
	DecisionBars  int64
	FlowWindowMs  int64
	HTFRows       int64
	ExpectedMs    int64
	SessionStartH int64
	SessionEndH   int64
}

func sweepAbsorptionIntParam(params map[string]interface{}, b sweepAbsorptionIntBound) (int64, error) {
	raw, ok := params[b.Name]
	if !ok || raw == nil {
		return b.Default, nil
	}
	var v float64
	switch t := raw.(type) {
	case float64:
		v = t
	case int:
		v = float64(t)
	case int64:
		v = float64(t)
	case json.Number:
		f, err := t.Float64()
		if err != nil {
			return 0, fmt.Errorf("%s %q is not a number", b.Name, t.String())
		}
		v = f
	default:
		return 0, fmt.Errorf("%s must be an integer, got %v", b.Name, raw)
	}
	if math.IsNaN(v) || math.IsInf(v, 0) || v != math.Trunc(v) {
		return 0, fmt.Errorf("%s must be an integer, got %v", b.Name, raw)
	}
	n := int64(v)
	if n < b.Min || n > b.Max {
		return 0, fmt.Errorf("%s must be in [%d, %d], got %v", b.Name, b.Min, b.Max, raw)
	}
	return n, nil
}

func sweepAbsorptionSupportFor(params map[string]interface{}) (sweepAbsorptionSupport, error) {
	vals := make(map[string]int64, len(sweepAbsorptionIntParams))
	for _, b := range sweepAbsorptionIntParams {
		v, err := sweepAbsorptionIntParam(params, b)
		if err != nil {
			return sweepAbsorptionSupport{}, err
		}
		vals[b.Name] = v
	}
	start, end := vals["session_start_hour_utc"], vals["session_end_hour_utc"]
	if start == end {
		return sweepAbsorptionSupport{}, fmt.Errorf("session_start_hour_utc equals session_end_hour_utc; the session would be empty")
	}
	interval := vals["expected_interval_ms"]
	if interval%feedTradeIntervalMs != 0 {
		return sweepAbsorptionSupport{}, fmt.Errorf("expected_interval_ms %d is not a multiple of the %d ms trade-flow interval", interval, feedTradeIntervalMs)
	}
	if sweepAbsorptionHTFIntervalMs%interval != 0 {
		return sweepAbsorptionSupport{}, fmt.Errorf("expected_interval_ms %d does not divide one hour, so UTC session boundaries would not align with bar opens", interval)
	}
	if feedTradeReorderGraceMs >= interval {
		return sweepAbsorptionSupport{}, fmt.Errorf("the trade-flow reorder grace must be shorter than one bar")
	}
	sessionMs := (((end-start)%24 + 24) % 24) * 3_600_000
	sessionBars := sessionMs / interval
	prelude := sessionBars
	if g := vals["gap_window"] + sweepAbsorptionATRPeriod; g > prelude {
		prelude = g
	}
	post := vals["sweep_window"] + vals["max_sweep_bars"] + vals["confirm_window"] + vals["setup_expiry"] - 1
	s := sweepAbsorptionSupport{
		SessionMs:     sessionMs,
		SessionBars:   sessionBars,
		PreludeBars:   prelude,
		PostBars:      post,
		DecisionBars:  prelude + post + sweepAbsorptionAlignmentRows,
		FlowWindowMs:  sessionMs + post*interval + interval + feedTradeIntervalMs,
		HTFRows:       sweepAbsorptionHTFSearchMs/sweepAbsorptionHTFIntervalMs + sweepAbsorptionHTFLeft + (post*interval+sweepAbsorptionHTFIntervalMs-1)/sweepAbsorptionHTFIntervalMs + sweepAbsorptionHTFSlackRows,
		ExpectedMs:    interval,
		SessionStartH: start,
		SessionEndH:   end,
	}
	if sessionBars < 2 {
		return sweepAbsorptionSupport{}, fmt.Errorf("the session holds %d bars; it needs at least 2", sessionBars)
	}
	if s.DecisionBars > sweepAbsorptionMaxDecisionBars {
		return sweepAbsorptionSupport{}, fmt.Errorf("the support horizon needs %d decision bars (prelude %d + post-session %d + alignment %d), over the %d-bar decision window", s.DecisionBars, prelude, post, sweepAbsorptionAlignmentRows, sweepAbsorptionMaxDecisionBars)
	}
	if s.FlowWindowMs > feedTradeMaxWindowMs {
		return sweepAbsorptionSupport{}, fmt.Errorf("the trade-flow window %d ms is over the %d ms retention cap", s.FlowWindowMs, feedTradeMaxWindowMs)
	}
	if sessionMs+post*interval > sweepAbsorptionDayMs {
		return sweepAbsorptionSupport{}, fmt.Errorf("the session plus its post-session horizon spans more than 24 hours, so a setup would outlive the next session start")
	}
	return s, nil
}

func sweepAbsorptionStrategySupport(sc StrategyConfig, intervalMs int64) (sweepAbsorptionSupport, error) {
	s, err := sweepAbsorptionSupportFor(sc.OpenStrategy.Params)
	if err != nil {
		return s, err
	}
	if s.ExpectedMs != intervalMs {
		return sweepAbsorptionSupport{}, fmt.Errorf("expected_interval_ms %d does not match the %d ms timeframe", s.ExpectedMs, intervalMs)
	}
	return s, nil
}

func feedStrategyUsesResearchHTF(sc StrategyConfig) bool {
	return strings.TrimSpace(effectiveOpenStrategy(sc)) == sweepAbsorptionStrategyName
}

package main

import (
	"context"
	"log"
	"strconv"
	"strings"
	"sync"
	"time"
)

var tradeDiagnosticsRecorder func(row *TradeDiagnosticsRow) error

var tradeDiagnosticsEnqueue func(row TradeDiagnosticsRow)

var tradeDiagnosticsPersistDeferred bool

func suspendEagerDiagnosticsPersist() func() {
	prev := tradeDiagnosticsPersistDeferred
	tradeDiagnosticsPersistDeferred = true
	return func() { tradeDiagnosticsPersistDeferred = prev }
}

const (
	diagMetricsPending         = "pending"
	diagMetricsOK              = "ok"
	diagMetricsFetchFailed     = "fetch_failed"
	diagMetricsNoCandles       = "no_candles"
	diagMetricsWindowUncovered = "window_uncovered"
	diagMetricsNoStrategyMeta  = "no_strategy_meta"
	diagMetricsBadInputs       = "bad_inputs"
)

type TradeDiagnosticsRow struct {
	RowID           int64
	StrategyID      string
	PositionID      string
	Symbol          string
	Side            string
	Timeframe       string
	RegimeAtOpen    string
	CloseReason     string
	EntryPrice      float64
	ExitPrice       float64
	Quantity        float64
	RealizedPnL     float64
	EntryATR        float64
	StopLossATRMult *float64
	OpenedAt        time.Time
	ClosedAt        time.Time

	MFEPrice     *float64
	MAEPrice     *float64
	FavorablePct *float64
	AdversePct   *float64
	CaptureRatio *float64

	MetricsStatus string
	LLMVerdict    *string

	HurstAtOpen   *float64
	HurstSizeMult *float64

	Scope      PortfolioScope `json:"-"`
	SourceRole storageRole    `json:"-"`
}

func captureTradeDiagnostics(s *StrategyState, pos *Position, closePrice, realizedPnL float64, reason string, closedAt time.Time) {
	if s == nil || pos == nil {
		return
	}
	if pos.isHedgeLeg() {
		return
	}
	if !tradeDiagnosticsPersistDeferred && tradeDiagnosticsRecorder == nil {
		return
	}
	row := TradeDiagnosticsRow{
		StrategyID:      s.ID,
		PositionID:      pos.TradePositionID,
		Symbol:          pos.Symbol,
		Side:            pos.Side,
		RegimeAtOpen:    pos.Regime,
		CloseReason:     reason,
		EntryPrice:      pos.AvgCost,
		ExitPrice:       closePrice,
		Quantity:        pos.Quantity,
		RealizedPnL:     realizedPnL,
		EntryATR:        pos.EntryATR,
		StopLossATRMult: pos.StopLossATRMult,
		OpenedAt:        pos.OpenedAt,
		ClosedAt:        closedAt,
		MetricsStatus:   diagMetricsPending,
	}
	if pos.LLMVerdict != "" {
		v := pos.LLMVerdict
		row.LLMVerdict = &v
	}
	if pos.HurstAtOpen > 0 {
		v := pos.HurstAtOpen
		row.HurstAtOpen = &v
	}
	if pos.HurstSizeMult > 0 {
		v := pos.HurstSizeMult
		row.HurstSizeMult = &v
	}
	if tradeDiagnosticsPersistDeferred {
		s.pendingTradeDiagnostics = append(s.pendingTradeDiagnostics, row)
		return
	}
	if err := tradeDiagnosticsRecorder(&row); err != nil {
		log.Printf("[diagnostics] insert row for %s %s: %v", s.ID, pos.Symbol, err)
		return
	}
	if tradeDiagnosticsEnqueue != nil {
		tradeDiagnosticsEnqueue(row)
	}
}

type tradeQualityMetrics struct {
	MFEPrice     float64
	MAEPrice     float64
	FavorablePct float64
	AdversePct   float64
	CaptureRatio *float64
}

func computeTradeQuality(candles []UICandle, side string, entry, exit float64) (tradeQualityMetrics, bool) {
	if entry <= 0 || len(candles) == 0 {
		return tradeQualityMetrics{}, false
	}
	short := side == "short"
	best, worst := entry, entry
	for _, c := range candles {
		hi, lo := c.High, c.Low
		if hi <= 0 || lo <= 0 {
			continue
		}
		if short {
			if lo < best {
				best = lo
			}
			if hi > worst {
				worst = hi
			}
		} else {
			if hi > best {
				best = hi
			}
			if lo < worst {
				worst = lo
			}
		}
	}
	m := tradeQualityMetrics{MFEPrice: best, MAEPrice: worst}
	if short {
		m.FavorablePct = (entry - best) / entry * 100
		m.AdversePct = (worst - entry) / entry * 100
	} else {
		m.FavorablePct = (best - entry) / entry * 100
		m.AdversePct = (entry - worst) / entry * 100
	}
	realizedPct := realizedPriceReturnPct(side, entry, exit)
	if realizedPct > 0 && m.FavorablePct > 0 {
		ratio := realizedPct / m.FavorablePct
		if ratio > 1 {
			ratio = 1
		}
		m.CaptureRatio = &ratio
	}
	return m, true
}

func realizedPriceReturnPct(side string, entry, exit float64) float64 {
	if entry <= 0 {
		return 0
	}
	pct := (exit - entry) / entry * 100
	if side == "short" {
		pct = -pct
	}
	return pct
}

func diagTimeframeDuration(tf string) (time.Duration, bool) {
	tf = strings.TrimSpace(strings.ToLower(tf))
	if len(tf) < 2 {
		return 0, false
	}
	n, err := strconv.Atoi(tf[:len(tf)-1])
	if err != nil || n <= 0 {
		return 0, false
	}
	switch tf[len(tf)-1] {
	case 'm':
		return time.Duration(n) * time.Minute, true
	case 'h':
		return time.Duration(n) * time.Hour, true
	case 'd':
		return time.Duration(n) * 24 * time.Hour, true
	case 'w':
		return time.Duration(n) * 7 * 24 * time.Hour, true
	}
	return 0, false
}

const (
	diagQueueCap         = 256
	diagMaxFetchBars     = 1500
	diagDefaultTimeframe = "1h"
)

type tradeDiagnosticsWorker struct {
	ch chan TradeDiagnosticsRow

	metaMu sync.RWMutex
	meta   map[string]StrategyConfig

	fetchCandles func(UICandleRequest) ([]UICandle, string, error)
	// updateMetrics carries the owning scope beside the row identifier: a bare
	// row identifier cannot choose a file.
	updateMetrics func(scope PortfolioScope, rowID int64, timeframe string, m *tradeQualityMetrics, status string) error

	// splitStorage marks a layout where an unscoped row cannot be resolved.
	splitStorage bool
}

func newTradeDiagnosticsWorker(fetch func(UICandleRequest) ([]UICandle, string, error), update func(PortfolioScope, int64, string, *tradeQualityMetrics, string) error) *tradeDiagnosticsWorker {
	return &tradeDiagnosticsWorker{
		ch:            make(chan TradeDiagnosticsRow, diagQueueCap),
		meta:          make(map[string]StrategyConfig),
		fetchCandles:  fetch,
		updateMetrics: update,
	}
}

func (w *tradeDiagnosticsWorker) UpdateStrategies(strategies []StrategyConfig) {
	next := make(map[string]StrategyConfig, len(strategies))
	for _, sc := range strategies {
		next[sc.ID] = sc
	}
	w.metaMu.Lock()
	w.meta = next
	w.metaMu.Unlock()
}

func (w *tradeDiagnosticsWorker) strategyConfig(id string) (StrategyConfig, bool) {
	w.metaMu.RLock()
	defer w.metaMu.RUnlock()
	sc, ok := w.meta[id]
	return sc, ok
}

func (w *tradeDiagnosticsWorker) Enqueue(row TradeDiagnosticsRow) {
	select {
	case w.ch <- row:
	default:
		log.Printf("[diagnostics] metrics queue full; row %d (%s %s) stays pending", row.RowID, row.StrategyID, row.Symbol)
	}
}

func (w *tradeDiagnosticsWorker) run(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case row := <-w.ch:
			w.process(row)
		}
	}
}

func (w *tradeDiagnosticsWorker) process(row TradeDiagnosticsRow) {
	if w.splitStorage && row.Scope == scopeUnassigned {
		log.Printf("[diagnostics] row %d (%s %s) carries no scope; refusing to update before choosing a state file", row.RowID, row.StrategyID, row.Symbol)
		return
	}
	status, tf, metrics := w.computeRowMetrics(row)
	if err := w.updateMetrics(row.Scope, row.RowID, tf, metrics, status); err != nil {
		log.Printf("[diagnostics] update metrics for row %d (%s %s): %v", row.RowID, row.StrategyID, row.Symbol, err)
	}
}

func (w *tradeDiagnosticsWorker) computeRowMetrics(row TradeDiagnosticsRow) (string, string, *tradeQualityMetrics) {
	if row.EntryPrice <= 0 || row.OpenedAt.IsZero() {
		return diagMetricsBadInputs, "", nil
	}
	sc, ok := w.strategyConfig(row.StrategyID)
	if !ok {
		return diagMetricsNoStrategyMeta, "", nil
	}
	tf := strategyDisplayTimeframe(sc)
	if tf == "" {
		tf = diagDefaultTimeframe
	}
	tfDur, ok := diagTimeframeDuration(tf)
	if !ok {
		tfDur, tf = time.Hour, diagDefaultTimeframe
	}
	sc.Timeframe = tf
	from := row.OpenedAt.UTC().Truncate(tfDur)
	to := row.ClosedAt.UTC()
	if to.Before(from) {
		return diagMetricsBadInputs, tf, nil
	}
	bars := int(to.Sub(from)/tfDur) + 3
	if bars < 10 {
		bars = 10
	}
	if bars > diagMaxFetchBars {
		return diagMetricsWindowUncovered, tf, nil
	}
	candles, _, err := w.fetchCandles(UICandleRequest{
		Strategy: sc,
		From:     from,
		To:       to,
		Limit:    bars,
	})
	if err != nil {
		log.Printf("[diagnostics] candle fetch for %s %s: %v", row.StrategyID, row.Symbol, err)
		return diagMetricsFetchFailed, tf, nil
	}
	if len(candles) == 0 {
		return diagMetricsNoCandles, tf, nil
	}
	first := time.Unix(candles[0].Time, 0).UTC()
	if first.After(from.Add(tfDur)) {
		return diagMetricsWindowUncovered, tf, nil
	}
	m, ok := computeTradeQuality(candles, row.Side, row.EntryPrice, row.ExitPrice)
	if !ok {
		return diagMetricsBadInputs, tf, nil
	}
	return diagMetricsOK, tf, &m
}

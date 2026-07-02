package main

import (
	"context"
	"log"
	"strconv"
	"strings"
	"sync"
	"time"
)

// #1147 per-trade trade-quality diagnostics.
//
// Every full position close (all recordClosedPosition paths: signal closes,
// kill switch, circuit breaker, corrupt legs, HL reconcile/external sync,
// manual, deribit assignment) writes one diagnostics row to the
// trade_diagnostics table. The identity/outcome part of the row is inserted
// EAGERLY under the caller's lock — mirroring the tradeRecorder hook (#289) so
// a crash right after the close never loses the row — and the derived quality
// metrics (MFE/MAE/capture ratio, which need a hold-window OHLCV fetch) are
// filled in by a background worker OUTSIDE mu, via an UPDATE keyed on rowid.
//
// Diagnostics-only by construction: nothing here mutates positions, orders,
// or config; a fetch/compute failure downgrades the row's metrics_status and
// leaves the quality columns NULL — it can never block or alter a close.
//
// Options positions are out of scope (they close via
// recordClosedOptionPosition and premium P&L has no meaningful underlying
// MFE/MAE); the on-demand report lives in diagnostics_cmd.go.

// tradeDiagnosticsRecorder is the package-level hook for eager row inserts,
// set in main() to stateDB.InsertTradeDiagnostics (nil in subcommands and
// tests that don't wire it — capture then no-ops).
var tradeDiagnosticsRecorder func(row *TradeDiagnosticsRow) error

// tradeDiagnosticsEnqueue hands the inserted row to the async metrics worker.
// nil (e.g. --once teardown races, tests) leaves the row at metrics_status
// 'pending' — still fully usable by the report, just without MFE/MAE.
var tradeDiagnosticsEnqueue func(row TradeDiagnosticsRow)

// Metrics status values for trade_diagnostics.metrics_status.
const (
	diagMetricsPending         = "pending"          // inserted; worker hasn't resolved it yet
	diagMetricsOK              = "ok"               // MFE/MAE/capture computed from a covered window
	diagMetricsFetchFailed     = "fetch_failed"     // candle fetch errored
	diagMetricsNoCandles       = "no_candles"       // fetch returned an empty window
	diagMetricsWindowUncovered = "window_uncovered" // fetched candles don't reach back to the open (metrics would be biased)
	diagMetricsNoStrategyMeta  = "no_strategy_meta" // strategy no longer in config; no platform/timeframe to fetch with
	diagMetricsBadInputs       = "bad_inputs"       // entry price/side unusable
)

// TradeDiagnosticsRow is one closed position's diagnostics record.
// Quality-metric pointers are nil until the worker computes them (NULL in
// SQLite). LLMVerdict is reserved for #1137 and never written here.
type TradeDiagnosticsRow struct {
	RowID           int64
	StrategyID      string
	PositionID      string
	Symbol          string
	Side            string // "long" / "short"
	Timeframe       string // stamped by the worker when it resolves the fetch timeframe
	RegimeAtOpen    string
	CloseReason     string
	EntryPrice      float64 // blended AvgCost at close (scale-ins blend; RiskAnchorPrice is not used for excursions)
	ExitPrice       float64
	Quantity        float64
	RealizedPnL     float64 // PRE-FEE final-close-leg PnL as passed to recordClosedPosition; report reads NET over all legs via the trades join
	EntryATR        float64
	StopLossATRMult *float64
	OpenedAt        time.Time
	ClosedAt        time.Time

	MFEPrice     *float64
	MAEPrice     *float64
	FavorablePct *float64 // max favorable excursion, % of entry
	AdversePct   *float64 // max adverse excursion, % of entry
	CaptureRatio *float64 // realized price return / favorable excursion, winners only

	MetricsStatus string
	LLMVerdict    *string
}

// captureTradeDiagnostics builds and eagerly persists the diagnostics row for
// a just-closed position, then queues it for async metric enrichment. Called
// from recordClosedPosition under the caller's state lock — the insert is a
// local SQLite write (same cost class as the InsertTrade that already runs on
// every close path); the OHLCV fetch never happens here.
func captureTradeDiagnostics(s *StrategyState, pos *Position, closePrice, realizedPnL float64, reason string, closedAt time.Time) {
	if tradeDiagnosticsRecorder == nil || s == nil || pos == nil {
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
	if err := tradeDiagnosticsRecorder(&row); err != nil {
		log.Printf("[diagnostics] insert row for %s %s: %v", s.ID, pos.Symbol, err)
		return
	}
	if tradeDiagnosticsEnqueue != nil {
		tradeDiagnosticsEnqueue(row)
	}
}

// tradeQualityMetrics is the derived quality block computed from hold-window
// candles.
type tradeQualityMetrics struct {
	MFEPrice     float64
	MAEPrice     float64
	FavorablePct float64
	AdversePct   float64
	CaptureRatio *float64
}

// computeTradeQuality derives MFE/MAE/capture ratio from hold-window OHLCV.
//
// Long:  MFE = best high seen (floored at entry), MAE = worst low (capped at
// entry). Short: mirrored. Excursions are % of entry price. Capture ratio is
// the realized price return divided by the favorable excursion, defined only
// for winning trades with a positive favorable move, clamped to [0, 1] (a
// fill can beat the candle range on a gap; >1 carries no signal).
//
// Single-bar holds work (one candle spanning both open and close) with a
// known bounded imprecision: intra-bar movement before the actual open fill
// is included in the excursion.
func computeTradeQuality(candles []UICandle, side string, entry, exit float64) (tradeQualityMetrics, bool) {
	if entry <= 0 || len(candles) == 0 {
		return tradeQualityMetrics{}, false
	}
	short := side == "short"    // anything else (incl. legacy empty side) = long/spot
	best, worst := entry, entry // best = favorable extreme, worst = adverse extreme
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

// realizedPriceReturnPct is the price-based (pre-fee) return of the round
// trip in % of entry, sign-adjusted for shorts.
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

// diagTimeframeDuration parses a candle timeframe token ("1m", "15m", "1h",
// "4h", "1d", "1w") into a duration. Unknown tokens return false.
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
	// diagQueueCap bounds the pending-metrics queue; the enqueue is
	// non-blocking (it runs under mu) so overflow drops the metric update,
	// never the row.
	diagQueueCap = 256
	// diagMaxFetchBars caps the hold-window candle fetch. Holds needing more
	// bars than this at the strategy timeframe get metrics_status
	// window_uncovered rather than silently-truncated (biased) excursions.
	diagMaxFetchBars = 1500
	// diagDefaultTimeframe is the fetch timeframe when the strategy config
	// doesn't resolve one (manual strategies without an explicit timeframe),
	// matching resolveManualATRTimeframe's 1h default (#1131).
	diagDefaultTimeframe = "1h"
)

// tradeDiagnosticsWorker fills in quality metrics for captured rows outside
// mu: resolve the strategy's fetch metadata, fetch hold-window candles via
// fetch_candles.py (read-only subprocess), compute, and UPDATE the row.
type tradeDiagnosticsWorker struct {
	ch chan TradeDiagnosticsRow

	metaMu sync.RWMutex
	meta   map[string]StrategyConfig

	fetchCandles  func(UICandleRequest) ([]UICandle, string, error)
	updateMetrics func(rowID int64, timeframe string, m *tradeQualityMetrics, status string) error
}

func newTradeDiagnosticsWorker(fetch func(UICandleRequest) ([]UICandle, string, error), update func(int64, string, *tradeQualityMetrics, string) error) *tradeDiagnosticsWorker {
	return &tradeDiagnosticsWorker{
		ch:            make(chan TradeDiagnosticsRow, diagQueueCap),
		meta:          make(map[string]StrategyConfig),
		fetchCandles:  fetch,
		updateMetrics: update,
	}
}

// UpdateStrategies refreshes the strategy-ID → config snapshot the worker
// resolves fetch metadata from. Called at startup and after each successful
// SIGHUP reload; independent of the main state mutex.
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

// Enqueue hands a freshly inserted row to the worker. Non-blocking: it is
// called under mu, so a full queue drops the metric update (the row stays
// 'pending') instead of ever stalling a close.
func (w *tradeDiagnosticsWorker) Enqueue(row TradeDiagnosticsRow) {
	select {
	case w.ch <- row:
	default:
		log.Printf("[diagnostics] metrics queue full; row %d (%s %s) stays pending", row.RowID, row.StrategyID, row.Symbol)
	}
}

// run drains the queue until ctx is cancelled (daemon shutdown). In-flight
// fetches are cancelled by runPythonReadOnly's own shutdown context.
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

// process resolves metadata, fetches the hold window, computes metrics, and
// persists the update. Every failure path downgrades metrics_status and
// returns — diagnostics never escalate.
func (w *tradeDiagnosticsWorker) process(row TradeDiagnosticsRow) {
	status, tf, metrics := w.computeRowMetrics(row)
	if err := w.updateMetrics(row.RowID, tf, metrics, status); err != nil {
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
		// fetch_candles.py fetches the most recent `limit` bars then filters;
		// a hold longer than the cap can't be covered back to the open, and a
		// truncated window would bias MFE/MAE.
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
	// Coverage check: the earliest candle must reach back to the open's
	// bar (within one timeframe of slack for exchange bucketing).
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

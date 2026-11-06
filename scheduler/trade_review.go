package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"sort"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	tradeReviewCoverageFull    = "full"
	tradeReviewCoveragePartial = "partial"
	tradeReviewCoverageNone    = "none"
	tradeReviewCoverageTooLong = "too_long"

	tradeReviewPadMinBars        = 20
	tradeReviewMaxHoldBars       = int64(1) << 32
	tradeReviewTimeSlack         = 2 * time.Second
	tradeFeedbackBodyLimit       = 1 << 20
	tradeFeedbackNoteMaxRunes    = 2000
	tradeFeedbackEvidenceVersion = 1

	tradeReviewRealizedBasis = "sum_of_net_close_results"
)

var tradeReviewIntervals = map[string]int64{
	"1m": 60, "3m": 180, "5m": 300, "15m": 900, "30m": 1800,
	"1h": 3600, "2h": 7200, "4h": 14400, "6h": 21600, "8h": 28800, "12h": 43200,
	"1d": 86400,
}

var tradeReviewSemantics = map[string]string{
	"actual_avg_entry_price":    "quantity-weighted mean price of every opening fill of the position",
	"actual_avg_exit_price":     "quantity-weighted mean price of every closing fill of the position",
	"actual_realized_pnl":       "sum of the net close result of every closing fill: realized_pnl minus exchange_fee when pnl_gross is set, else realized_pnl; it does not include opening fees or funding",
	"opening_fees":              "sum of exchange_fee over the opening fills, reported separately",
	"funding":                   "sum of the funding rows of the position, reported separately and excluded from price averages",
	"bar_membership":            "a bar is named by its opening time t and holds instants in [t, t + bar_seconds)",
	"preferred_times":           "Unix seconds of completed bar opening times inside the effective window",
	"window_padding":            "max(20 bars, 25 percent of the holding bars rounded up) on each side of the holding bars",
	"coverage_full_requirement": "one valid completed bar at every expected opening time from effective_from to effective_to, from a verified range read",
}

type tradeReviewFill struct {
	RowID            int64   `json:"row_id"`
	Time             int64   `json:"time"`
	Timestamp        string  `json:"timestamp"`
	Symbol           string  `json:"symbol"`
	Side             string  `json:"side"`
	TradeType        string  `json:"trade_type"`
	Role             string  `json:"role"`
	IsClose          bool    `json:"is_close"`
	Quantity         float64 `json:"quantity"`
	Price            float64 `json:"price"`
	Value            float64 `json:"value"`
	ExchangeFee      float64 `json:"exchange_fee"`
	RealizedPnL      float64 `json:"realized_pnl"`
	PnLGross         bool    `json:"pnl_gross"`
	NetPnL           float64 `json:"net_pnl"`
	FeeSource        string  `json:"fee_source,omitempty"`
	ExchangeOrderID  string  `json:"exchange_order_id,omitempty"`
	Details          string  `json:"details,omitempty"`
	Regime           string  `json:"regime,omitempty"`
	EntryATR         float64 `json:"entry_atr,omitempty"`
	CostModelVersion int     `json:"cost_model_version,omitempty"`
	CostPolicyID     string  `json:"cost_policy_id,omitempty"`
}

type tradeEvidenceRow struct {
	RowID int64  `json:"row_id"`
	Role  string `json:"role"`
	Trade
}

type tradeReviewActual struct {
	OpenedAt          int64   `json:"opened_at"`
	ClosedAt          int64   `json:"closed_at,omitempty"`
	AvgEntryPrice     float64 `json:"avg_entry_price"`
	AvgExitPrice      float64 `json:"avg_exit_price,omitempty"`
	RealizedPnL       float64 `json:"realized_pnl"`
	RealizedPnLBasis  string  `json:"realized_pnl_basis"`
	OpenedQuantity    float64 `json:"opened_quantity"`
	ClosedQuantity    float64 `json:"closed_quantity"`
	RemainingQuantity float64 `json:"remaining_quantity"`
	OpeningFees       float64 `json:"opening_fees"`
	Funding           float64 `json:"funding"`
	CloseReason       string  `json:"close_reason,omitempty"`
	EntryATR          float64 `json:"entry_atr,omitempty"`
}

type tradeReviewAnalysis struct {
	Symbol     string
	Side       string
	Fills      []tradeReviewFill
	Evidence   []tradeEvidenceRow
	FirstFill  time.Time
	LastClose  time.Time
	Actual     tradeReviewActual
	Closed     bool
	Eligible   bool
	Reason     string
	Diagnostic *TradeDiagnosticsRow
}

type tradeReviewWindow struct {
	Timeframe              string `json:"timeframe"`
	BarSeconds             int64  `json:"bar_seconds"`
	PadBars                int64  `json:"pad_bars"`
	HoldStart              int64  `json:"hold_start"`
	HoldEnd                int64  `json:"hold_end"`
	RequestedFrom          int64  `json:"requested_from"`
	RequestedTo            int64  `json:"requested_to"`
	EffectiveFrom          int64  `json:"effective_from"`
	EffectiveTo            int64  `json:"effective_to"`
	LatestCompleted        int64  `json:"latest_completed"`
	ExpectedBars           int64  `json:"expected_bars"`
	FuturePaddingShortened bool   `json:"future_padding_shortened"`
	CloseBarComplete       bool   `json:"close_bar_complete"`
}

type tradeReviewCoverage struct {
	Status       string `json:"status"`
	Expected     int64  `json:"expected"`
	Present      int64  `json:"present"`
	Missing      int64  `json:"missing"`
	Duplicates   int    `json:"duplicates"`
	Invalid      int    `json:"invalid"`
	OffGrid      int    `json:"off_grid"`
	FirstMissing int64  `json:"first_missing,omitempty"`
	Verified     bool   `json:"range_verified"`
	Reason       string `json:"reason,omitempty"`
}

type tradeReviewMetrics struct {
	MFEPrice float64 `json:"mfe_price"`
	MAEPrice float64 `json:"mae_price"`
	Status   string  `json:"status"`
}

type tradeReviewDiagnosticsView struct {
	RowID         int64    `json:"row_id"`
	Symbol        string   `json:"symbol"`
	Side          string   `json:"side"`
	Timeframe     string   `json:"timeframe"`
	CloseReason   string   `json:"close_reason"`
	EntryPrice    float64  `json:"entry_price"`
	ExitPrice     float64  `json:"exit_price"`
	Quantity      float64  `json:"quantity"`
	RealizedPnL   float64  `json:"realized_pnl"`
	EntryATR      float64  `json:"entry_atr"`
	OpenedAt      int64    `json:"opened_at"`
	ClosedAt      int64    `json:"closed_at"`
	MetricsStatus string   `json:"metrics_status"`
	MFEPrice      *float64 `json:"mfe_price,omitempty"`
	MAEPrice      *float64 `json:"mae_price,omitempty"`
}

type tradeReviewResponse struct {
	StrategyID          string                       `json:"strategy_id"`
	PositionID          string                       `json:"position_id"`
	Partition           string                       `json:"partition"`
	Platform            string                       `json:"platform"`
	InstrumentType      string                       `json:"instrument_type"`
	Symbol              string                       `json:"symbol"`
	Side                string                       `json:"side"`
	Timeframe           string                       `json:"timeframe"`
	Closed              bool                         `json:"closed"`
	AnnotationEligible  bool                         `json:"annotation_eligible"`
	SaveAvailable       bool                         `json:"save_available"`
	Reason              string                       `json:"reason,omitempty"`
	Fills               []tradeReviewFill            `json:"fills"`
	Diagnostics         []tradeReviewDiagnosticsView `json:"diagnostics"`
	Actual              tradeReviewActual            `json:"actual"`
	Semantics           map[string]string            `json:"semantics"`
	Window              *tradeReviewWindow           `json:"window,omitempty"`
	Coverage            tradeReviewCoverage          `json:"coverage"`
	CandleSource        string                       `json:"candle_source,omitempty"`
	CandleError         string                       `json:"candle_error,omitempty"`
	Candles             []UICandle                   `json:"candles"`
	Metrics             *tradeReviewMetrics          `json:"metrics,omitempty"`
	Annotations         []tradeFeedbackRecord        `json:"annotations"`
	CurrentAnnotationID int64                        `json:"current_annotation_id,omitempty"`
}

type tradeFeedbackContext struct {
	StrategyID        string `json:"strategy_id"`
	StorageStrategyID string `json:"storage_strategy_id"`
	Platform          string `json:"platform"`
	Type              string `json:"type"`
	Mode              string `json:"mode"`
	Partition         string `json:"partition"`
}

type tradeFeedbackRequest struct {
	PositionID string  `json:"position_id"`
	EntryTime  int64   `json:"entry_time"`
	ExitTime   int64   `json:"exit_time"`
	EntryPrice float64 `json:"entry_price"`
	ExitPrice  float64 `json:"exit_price"`
	Note       string  `json:"note"`
}

type tradeFeedbackSelection struct {
	EntryTime  int64    `json:"entry_time"`
	EntryPrice float64  `json:"entry_price"`
	EntryBar   UICandle `json:"entry_bar"`
	ExitTime   int64    `json:"exit_time"`
	ExitPrice  float64  `json:"exit_price"`
	ExitBar    UICandle `json:"exit_bar"`
}

type tradeFeedbackCandleEvidence struct {
	Source        string              `json:"source"`
	Timeframe     string              `json:"timeframe"`
	TimestampKind string              `json:"timestamp_kind"`
	Coverage      tradeReviewCoverage `json:"coverage"`
	Bars          []UICandle          `json:"bars"`
}

type tradeFeedbackEvidence struct {
	Version           int                         `json:"version"`
	Strategy          tradeFeedbackContext        `json:"strategy"`
	SourceRole        string                      `json:"source_role"`
	PositionID        string                      `json:"position_id"`
	Symbol            string                      `json:"symbol"`
	Side              string                      `json:"side"`
	SourceFingerprint string                      `json:"source_fingerprint"`
	Fills             []tradeEvidenceRow          `json:"fills"`
	Diagnostics       TradeDiagnosticsRow         `json:"diagnostics"`
	Actual            tradeReviewActual           `json:"actual"`
	Semantics         map[string]string           `json:"semantics"`
	Window            tradeReviewWindow           `json:"window"`
	Candles           tradeFeedbackCandleEvidence `json:"candles"`
	Selection         tradeFeedbackSelection      `json:"selection"`
	ValidatedAt       time.Time                   `json:"validated_at"`
}

type tradeReviewError struct {
	Status int
	Msg    string
}

func (e *tradeReviewError) Error() string { return e.Msg }

func reviewErr(status int, format string, args ...interface{}) *tradeReviewError {
	return &tradeReviewError{Status: status, Msg: fmt.Sprintf(format, args...)}
}

func tradeReviewFinite(v float64) bool {
	return !math.IsNaN(v) && !math.IsInf(v, 0)
}

func tradeReviewRole(t Trade) string {
	switch {
	case t.TradeType == TradeTypeFunding:
		return "funding"
	case t.TradeType == quoteFeeAdjustmentType:
		return "fee_adjustment"
	case t.TradeType == "options" || t.TradeType == "assignment" || t.TradeType == hedgeTradeType:
		return "unsupported"
	case t.IsClose:
		return "close"
	default:
		return "open"
	}
}

func tradeReviewFillView(t Trade, role string) tradeReviewFill {
	return tradeReviewFill{
		RowID:            t.sourceRowID,
		Time:             t.Timestamp.UTC().Unix(),
		Timestamp:        formatTime(t.Timestamp),
		Symbol:           t.Symbol,
		Side:             t.Side,
		TradeType:        t.TradeType,
		Role:             role,
		IsClose:          t.IsClose,
		Quantity:         t.Quantity,
		Price:            t.Price,
		Value:            t.Value,
		ExchangeFee:      t.ExchangeFee,
		RealizedPnL:      t.RealizedPnL,
		PnLGross:         t.PnLGross,
		NetPnL:           tradeNetPnL(t),
		FeeSource:        t.FeeSource,
		ExchangeOrderID:  t.ExchangeOrderID,
		Details:          t.Details,
		Regime:           t.Regime,
		EntryATR:         t.EntryATR,
		CostModelVersion: t.CostModelVersion,
		CostPolicyID:     t.CostPolicyID,
	}
}

func tradeReviewNearlyEqual(a, b float64) bool {
	scale := math.Max(math.Abs(a), math.Abs(b))
	return math.Abs(a-b) <= math.Max(1e-12, 1e-9*scale)
}

func analyzeTradeReview(sc StrategyConfig, ev *positionEvidence, memOpen bool) tradeReviewAnalysis {
	a := tradeReviewAnalysis{Fills: []tradeReviewFill{}}
	refuse := func(format string, args ...interface{}) {
		if a.Reason == "" {
			a.Reason = fmt.Sprintf(format, args...)
		}
	}
	if sc.Type == "options" {
		refuse("option positions are not supported for annotation")
	}
	var opens, closes []Trade
	var openSide string
	for _, t := range ev.Trades {
		role := tradeReviewRole(t)
		a.Fills = append(a.Fills, tradeReviewFillView(t, role))
		a.Evidence = append(a.Evidence, tradeEvidenceRow{RowID: t.sourceRowID, Role: role, Trade: t})
		switch role {
		case "funding":
			a.Actual.Funding += tradeNetPnL(t)
			continue
		case "fee_adjustment":
			continue
		case "unsupported":
			refuse("trade row %d is a %s record; option and hedge positions are not supported for annotation", t.sourceRowID, t.TradeType)
			continue
		}
		if t.Timestamp.IsZero() {
			refuse("fill row %d has no timestamp", t.sourceRowID)
		}
		if !tradeReviewFinite(t.Quantity) || !tradeReviewFinite(t.Price) || t.Quantity <= 0 || t.Price <= 0 {
			refuse("fill row %d has a non-positive or non-finite quantity or price", t.sourceRowID)
			continue
		}
		if t.FeeSource == FeeSourceReconcileAdjustment {
			refuse("fill row %d is a model-only reconciliation close", t.sourceRowID)
		}
		if t.FeeSource == FeeSourceUserFillsPending {
			refuse("fill row %d still has a pending venue fee", t.sourceRowID)
		}
		if a.Symbol == "" {
			a.Symbol = t.Symbol
		} else if t.Symbol != a.Symbol {
			refuse("fills name two symbols (%s and %s)", a.Symbol, t.Symbol)
		}
		side := strings.ToLower(strings.TrimSpace(t.Side))
		if role == "open" {
			if side != "buy" && side != "sell" {
				refuse("opening fill row %d has side %q", t.sourceRowID, t.Side)
			} else if openSide == "" {
				openSide = side
			} else if side != openSide {
				refuse("opening fills have both buy and sell sides")
			}
			opens = append(opens, t)
		} else {
			if len(opens) == 0 {
				refuse("closing fill row %d comes before any opening fill", t.sourceRowID)
			}
			closes = append(closes, t)
		}
	}
	if openSide == "buy" {
		a.Side = "long"
	} else if openSide == "sell" {
		a.Side = "short"
	}
	for _, t := range closes {
		if a.Side != "" && !strings.EqualFold(strings.TrimSpace(t.Side), closeTradeSide(a.Side)) {
			refuse("closing fill row %d has side %q, not %q", t.sourceRowID, t.Side, closeTradeSide(a.Side))
		}
	}
	if len(opens) == 0 {
		refuse("the position has no opening fill")
	}

	var openQty, openNotional, closeQty, closeNotional float64
	for _, t := range opens {
		openQty += t.Quantity
		openNotional += t.Quantity * t.Price
		a.Actual.OpeningFees += t.ExchangeFee
	}
	for _, t := range closes {
		closeQty += t.Quantity
		closeNotional += t.Quantity * t.Price
		a.Actual.RealizedPnL += tradeNetPnL(t)
	}
	tol := math.Max(1e-12, 1e-9*openQty)
	running := 0.0
	for _, t := range ev.Trades {
		switch tradeReviewRole(t) {
		case "open":
			running += t.Quantity
		case "close":
			running -= t.Quantity
			if running < -tol {
				refuse("closing fills exceed the opened quantity at row %d", t.sourceRowID)
			}
		}
	}
	remaining := openQty - closeQty
	if math.Abs(remaining) <= tol {
		remaining = 0
	}
	a.Actual.OpenedQuantity = openQty
	a.Actual.ClosedQuantity = closeQty
	a.Actual.RemainingQuantity = remaining
	a.Actual.RealizedPnLBasis = tradeReviewRealizedBasis
	if openQty > 0 {
		a.Actual.AvgEntryPrice = openNotional / openQty
	}
	if closeQty > 0 {
		a.Actual.AvgExitPrice = closeNotional / closeQty
	}
	if len(opens) > 0 {
		a.FirstFill = opens[0].Timestamp.UTC()
		a.Actual.OpenedAt = a.FirstFill.Unix()
		a.Actual.EntryATR = opens[0].EntryATR
	}
	if len(closes) > 0 {
		a.LastClose = closes[len(closes)-1].Timestamp.UTC()
	}

	exposure := memOpen || len(ev.OpenBookRows) > 0
	a.Closed = len(closes) > 0 && remaining == 0 && !exposure
	if a.Closed {
		a.Actual.ClosedAt = a.LastClose.Unix()
	}
	switch {
	case len(closes) == 0 && len(opens) > 0:
		refuse("the position is still open")
	case remaining > 0:
		refuse("the position is only partly closed (%.10g of %.10g remains)", remaining, openQty)
	case exposure:
		refuse("the book still holds this position")
	}

	switch len(ev.Diagnostics) {
	case 0:
		refuse("no close diagnostics record exists for this position")
	case 1:
		d := ev.Diagnostics[0]
		a.Diagnostic = &d
		a.Actual.CloseReason = d.CloseReason
		if d.EntryATR > 0 {
			a.Actual.EntryATR = d.EntryATR
		}
		if a.Closed {
			last := closes[len(closes)-1]
			first := opens[0]
			switch {
			case diagExcludedReason(d.CloseReason):
				refuse("the close is synthetic (%s)", d.CloseReason)
			case d.Symbol != a.Symbol:
				refuse("the diagnostics symbol %s differs from the fills (%s)", d.Symbol, a.Symbol)
			case d.Side != a.Side:
				refuse("the diagnostics side %s differs from the fills (%s)", d.Side, a.Side)
			case d.OpenedAt.IsZero() || d.ClosedAt.IsZero() || d.ClosedAt.Before(d.OpenedAt):
				refuse("the diagnostics open and close times are not a valid interval")
			case absDuration(d.ClosedAt.Sub(last.Timestamp)) > tradeReviewTimeSlack:
				refuse("the diagnostics close time does not match the final closing fill")
			case absDuration(d.OpenedAt.Sub(first.Timestamp)) > tradeReviewTimeSlack:
				refuse("the diagnostics open time does not match the first opening fill")
			case !tradeReviewNearlyEqual(d.Quantity, last.Quantity):
				refuse("the diagnostics quantity %.10g differs from the final closing fill (%.10g)", d.Quantity, last.Quantity)
			case !tradeReviewNearlyEqual(d.ExitPrice, last.Price):
				refuse("the diagnostics exit price %.10g differs from the final closing fill (%.10g)", d.ExitPrice, last.Price)
			}
		}
	default:
		refuse("%d close diagnostics records match this position; the close record is ambiguous", len(ev.Diagnostics))
	}
	a.Eligible = a.Closed && a.Reason == ""
	return a
}

func absDuration(d time.Duration) time.Duration {
	if d < 0 {
		return -d
	}
	return d
}

func tradeReviewTimeframe(sc StrategyConfig, d *TradeDiagnosticsRow) (string, int64, bool) {
	if d != nil {
		if sec, ok := tradeReviewIntervals[d.Timeframe]; ok {
			return d.Timeframe, sec, true
		}
	}
	tf := strategyDisplayTimeframe(sc)
	if sec, ok := tradeReviewIntervals[tf]; ok {
		return tf, sec, true
	}
	if dur, ok := diagTimeframeDuration(tf); ok {
		return tf, int64(dur / time.Second), false
	}
	return tf, 0, false
}

func tradeReviewFloor(t, bar int64) int64 {
	q := t / bar
	if t%bar < 0 {
		q--
	}
	return q * bar
}

func tradeReviewWindowFor(tf string, bar int64, first, end time.Time, open bool, now time.Time) (tradeReviewWindow, error) {
	if bar <= 0 {
		return tradeReviewWindow{}, fmt.Errorf("timeframe %q has no bar length", tf)
	}
	if first.IsZero() {
		return tradeReviewWindow{}, fmt.Errorf("the position has no first fill time")
	}
	if open || end.IsZero() {
		end = now
	}
	start := tradeReviewFloor(first.UTC().Unix(), bar)
	holdEnd := tradeReviewFloor(end.UTC().Unix(), bar)
	if holdEnd < start {
		return tradeReviewWindow{}, fmt.Errorf("the holding end is before its start")
	}
	holdBars := (holdEnd - start) / bar
	if holdBars > tradeReviewMaxHoldBars {
		return tradeReviewWindow{}, fmt.Errorf("the holding period spans %d bars", holdBars)
	}
	pad := (holdBars + 3) / 4
	if pad < tradeReviewPadMinBars {
		pad = tradeReviewPadMinBars
	}
	latest := tradeReviewFloor(now.UTC().Unix(), bar) - bar
	w := tradeReviewWindow{
		Timeframe:       tf,
		BarSeconds:      bar,
		PadBars:         pad,
		HoldStart:       start,
		HoldEnd:         holdEnd,
		RequestedFrom:   start - pad*bar,
		RequestedTo:     holdEnd + pad*bar,
		LatestCompleted: latest,
	}
	w.EffectiveFrom = w.RequestedFrom
	w.EffectiveTo = w.RequestedTo
	if w.EffectiveTo > latest {
		w.EffectiveTo = latest
		w.FuturePaddingShortened = true
	}
	w.CloseBarComplete = !open && holdEnd <= latest
	if w.EffectiveTo >= w.EffectiveFrom {
		w.ExpectedBars = (w.EffectiveTo-w.EffectiveFrom)/bar + 1
	}
	return w, nil
}

func tradeReviewCandleValid(c UICandle) bool {
	for _, v := range []float64{c.Open, c.High, c.Low, c.Close} {
		if !tradeReviewFinite(v) || v <= 0 {
			return false
		}
	}
	if !tradeReviewFinite(c.Volume) || c.Volume < 0 {
		return false
	}
	return c.High >= math.Max(math.Max(c.Open, c.Close), c.Low) && c.Low <= math.Min(c.Open, c.Close)
}

func tradeReviewRangeVerified(source string) bool {
	return strings.HasSuffix(source, ":range")
}

func assessTradeReviewCoverage(candles []UICandle, w tradeReviewWindow, source string) (tradeReviewCoverage, []UICandle) {
	cov := tradeReviewCoverage{Expected: w.ExpectedBars, Verified: tradeReviewRangeVerified(source)}
	if w.ExpectedBars <= 0 {
		cov.Status = tradeReviewCoverageNone
		cov.Reason = "the effective window holds no completed bar"
		return cov, []UICandle{}
	}
	if w.ExpectedBars > diagMaxFetchBars {
		cov.Status = tradeReviewCoverageTooLong
		cov.Reason = fmt.Sprintf("the window needs %d bars, above the %d-bar limit", w.ExpectedBars, diagMaxFetchBars)
		return cov, []UICandle{}
	}
	counts := make(map[int64]int)
	bars := make(map[int64]UICandle)
	for _, c := range candles {
		if c.Time < w.EffectiveFrom || c.Time > w.EffectiveTo {
			continue
		}
		if (c.Time-w.EffectiveFrom)%w.BarSeconds != 0 {
			cov.OffGrid++
			continue
		}
		if !tradeReviewCandleValid(c) {
			cov.Invalid++
			continue
		}
		counts[c.Time]++
		bars[c.Time] = c
	}
	out := make([]UICandle, 0, len(bars))
	for slot := w.EffectiveFrom; slot <= w.EffectiveTo; slot += w.BarSeconds {
		switch counts[slot] {
		case 0:
			cov.Missing++
			if cov.FirstMissing == 0 {
				cov.FirstMissing = slot
			}
		case 1:
			cov.Present++
			out = append(out, bars[slot])
		default:
			cov.Duplicates++
		}
	}
	switch {
	case cov.Present == 0:
		cov.Status = tradeReviewCoverageNone
		cov.Reason = "no valid completed bar was returned for the window"
	case cov.Present == cov.Expected && cov.Duplicates == 0 && cov.Invalid == 0 && cov.OffGrid == 0 && cov.Verified:
		cov.Status = tradeReviewCoverageFull
	default:
		cov.Status = tradeReviewCoveragePartial
		if !cov.Verified {
			cov.Reason = fmt.Sprintf("candle source %q is not a verified historical range read", source)
		} else {
			cov.Reason = fmt.Sprintf("%d missing, %d duplicate, %d invalid and %d off-grid bars in the window", cov.Missing, cov.Duplicates, cov.Invalid, cov.OffGrid)
		}
	}
	return cov, out
}

func tradeReviewCandleRequest(sc StrategyConfig, symbol string, w tradeReviewWindow) UICandleRequest {
	cp := sc
	cp.Symbol = symbol
	cp.Timeframe = w.Timeframe
	return UICandleRequest{
		Strategy: cp,
		From:     time.Unix(w.EffectiveFrom, 0).UTC(),
		To:       time.Unix(w.EffectiveTo, 0).UTC(),
		Limit:    int(w.ExpectedBars),
	}
}

func tradeReviewDiagnosticsViews(rows []TradeDiagnosticsRow) []tradeReviewDiagnosticsView {
	out := make([]tradeReviewDiagnosticsView, 0, len(rows))
	for _, d := range rows {
		out = append(out, tradeReviewDiagnosticsView{
			RowID: d.RowID, Symbol: d.Symbol, Side: d.Side, Timeframe: d.Timeframe, CloseReason: d.CloseReason,
			EntryPrice: d.EntryPrice, ExitPrice: d.ExitPrice, Quantity: d.Quantity, RealizedPnL: d.RealizedPnL,
			EntryATR: d.EntryATR, OpenedAt: d.OpenedAt.Unix(), ClosedAt: d.ClosedAt.Unix(),
			MetricsStatus: d.MetricsStatus, MFEPrice: d.MFEPrice, MAEPrice: d.MAEPrice,
		})
	}
	return out
}

func tradeReviewMetricsFor(d *TradeDiagnosticsRow) *tradeReviewMetrics {
	if d == nil || d.MetricsStatus != diagMetricsOK || d.MFEPrice == nil || d.MAEPrice == nil {
		return nil
	}
	if !tradeReviewFinite(*d.MFEPrice) || !tradeReviewFinite(*d.MAEPrice) || *d.MFEPrice <= 0 || *d.MAEPrice <= 0 {
		return nil
	}
	return &tradeReviewMetrics{MFEPrice: *d.MFEPrice, MAEPrice: *d.MAEPrice, Status: d.MetricsStatus}
}

func tradeReviewSourceFingerprint(ev *positionEvidence) (string, error) {
	rows := make([]tradeEvidenceRow, 0, len(ev.Trades))
	for _, t := range ev.Trades {
		rows = append(rows, tradeEvidenceRow{RowID: t.sourceRowID, Role: tradeReviewRole(t), Trade: t})
	}
	book := ev.OpenBookRows
	if book == nil {
		book = []positionBookRow{}
	}
	diags := ev.Diagnostics
	if diags == nil {
		diags = []TradeDiagnosticsRow{}
	}
	raw, err := json.Marshal(struct {
		Trades      []tradeEvidenceRow    `json:"trades"`
		Diagnostics []TradeDiagnosticsRow `json:"diagnostics"`
		Book        []positionBookRow     `json:"book"`
	}{rows, diags, book})
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:]), nil
}

func tradeFeedbackContextFor(sc StrategyConfig) tradeFeedbackContext {
	return tradeFeedbackContext{
		StrategyID:        sc.ID,
		StorageStrategyID: effectiveStorageStrategyID(sc),
		Platform:          sc.Platform,
		Type:              sc.Type,
		Mode:              strategyDisplayMode(sc),
		Partition:         partitionFor(sc).String(),
	}
}

func (ss *StatusServer) positionOpenInMemory(strategyID, positionID string) bool {
	ss.mu.RLock()
	defer ss.mu.RUnlock()
	if ss.state == nil {
		return false
	}
	s := ss.state.Strategies[strategyID]
	if s == nil {
		return false
	}
	for _, pos := range s.Positions {
		if pos != nil && pos.TradePositionID == positionID {
			return true
		}
	}
	return false
}

type tradeReviewBuild struct {
	sc       StrategyConfig
	ev       *positionEvidence
	analysis tradeReviewAnalysis
	tf       string
	tfOK     bool
	window   *tradeReviewWindow
	coverage tradeReviewCoverage
	candles  []UICandle
	source   string
	fetchErr error
	fp       string
}

func (ss *StatusServer) buildTradeReview(sc StrategyConfig, positionID string, now time.Time) (*tradeReviewBuild, *tradeReviewError) {
	ev, err := ss.stateDB.ReadPositionEvidence(sc.ID, positionID)
	if err != nil {
		return nil, reviewErr(http.StatusInternalServerError, "read position evidence: %v", err)
	}
	if len(ev.Trades) == 0 && len(ev.Diagnostics) == 0 {
		return nil, reviewErr(http.StatusNotFound, "position %s not found for strategy %s", positionID, sc.ID)
	}
	fp, err := tradeReviewSourceFingerprint(ev)
	if err != nil {
		return nil, reviewErr(http.StatusInternalServerError, "fingerprint position evidence: %v", err)
	}
	b := &tradeReviewBuild{sc: sc, ev: ev, fp: fp}
	b.analysis = analyzeTradeReview(sc, ev, ss.positionOpenInMemory(sc.ID, positionID))
	tf, bar, tfOK := tradeReviewTimeframe(sc, b.analysis.Diagnostic)
	b.tf, b.tfOK = tf, tfOK
	if !tfOK && b.analysis.Reason == "" {
		b.analysis.Reason = fmt.Sprintf("timeframe %q is not supported for annotation", tf)
	}
	b.coverage = tradeReviewCoverage{Status: tradeReviewCoverageNone}
	b.candles = []UICandle{}
	if bar <= 0 || b.analysis.Symbol == "" {
		b.coverage.Reason = "no bar length or symbol is known for this position"
		return b, nil
	}
	w, err := tradeReviewWindowFor(tf, bar, b.analysis.FirstFill, b.analysis.LastClose, !b.analysis.Closed, now)
	if err != nil {
		b.coverage.Reason = err.Error()
		return b, nil
	}
	b.window = &w
	if w.ExpectedBars <= 0 || w.ExpectedBars > diagMaxFetchBars {
		b.coverage, b.candles = assessTradeReviewCoverage(nil, w, "")
		return b, nil
	}
	if ss.candleFetcher == nil {
		b.fetchErr = fmt.Errorf("no candle fetcher is configured")
		b.coverage.Reason = b.fetchErr.Error()
		return b, nil
	}
	candles, source, ferr := ss.candleFetcher(tradeReviewCandleRequest(sc, b.analysis.Symbol, w))
	if ferr != nil {
		b.fetchErr = ferr
		b.coverage.Expected = w.ExpectedBars
		b.coverage.Reason = "candle fetch failed"
		return b, nil
	}
	b.source = source
	b.coverage, b.candles = assessTradeReviewCoverage(candles, w, source)
	return b, nil
}

func (b *tradeReviewBuild) saveRefusal() string {
	switch {
	case b.analysis.Reason != "":
		return b.analysis.Reason
	case !b.analysis.Eligible:
		return "the position is not eligible for annotation"
	case b.window == nil:
		return b.coverage.Reason
	case !b.window.CloseBarComplete:
		return "the closing bar is still forming; saving opens once it completes"
	case b.fetchErr != nil:
		return "candle fetch failed: " + b.fetchErr.Error()
	case b.coverage.Status != tradeReviewCoverageFull:
		return fmt.Sprintf("candle coverage is %s: %s", b.coverage.Status, b.coverage.Reason)
	}
	return ""
}

func (b *tradeReviewBuild) response() tradeReviewResponse {
	sc := b.sc
	annotations := b.ev.Feedback
	if annotations == nil {
		annotations = []tradeFeedbackRecord{}
	}
	resp := tradeReviewResponse{
		StrategyID:         sc.ID,
		PositionID:         "",
		Partition:          partitionFor(sc).String(),
		Platform:           sc.Platform,
		InstrumentType:     sc.Type,
		Symbol:             b.analysis.Symbol,
		Side:               b.analysis.Side,
		Timeframe:          b.tf,
		Closed:             b.analysis.Closed,
		AnnotationEligible: b.analysis.Eligible && b.tfOK,
		Fills:              b.analysis.Fills,
		Diagnostics:        tradeReviewDiagnosticsViews(b.ev.Diagnostics),
		Actual:             b.analysis.Actual,
		Semantics:          tradeReviewSemantics,
		Window:             b.window,
		Coverage:           b.coverage,
		CandleSource:       b.source,
		Candles:            b.candles,
		Metrics:            tradeReviewMetricsFor(b.analysis.Diagnostic),
		Annotations:        annotations,
	}
	if b.fetchErr != nil {
		resp.CandleError = b.fetchErr.Error()
	}
	resp.Reason = b.saveRefusal()
	resp.SaveAvailable = resp.Reason == ""
	if len(annotations) > 0 {
		resp.CurrentAnnotationID = annotations[0].RowID
	}
	return resp
}

func (ss *StatusServer) handleAPIStrategyTradeReview(w http.ResponseWriter, r *http.Request, id string) {
	sc, ok := ss.strategyConfig(id)
	if !ok {
		writeJSONError(w, http.StatusNotFound, "strategy not found")
		return
	}
	positionID := strings.TrimSpace(r.URL.Query().Get("position_id"))
	if positionID == "" {
		writeJSONError(w, http.StatusBadRequest, "position_id is required")
		return
	}
	if ss.stateDB == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "database not available")
		return
	}
	b, rerr := ss.buildTradeReview(sc, positionID, time.Now().UTC())
	if rerr != nil {
		writeJSONError(w, rerr.Status, rerr.Msg)
		return
	}
	resp := b.response()
	resp.PositionID = positionID
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	if b.fetchErr != nil {
		w.WriteHeader(http.StatusBadGateway)
	}
	json.NewEncoder(w).Encode(resp)
}

func decodeTradeFeedbackRequest(w http.ResponseWriter, r *http.Request) (tradeFeedbackRequest, *tradeReviewError) {
	var req tradeFeedbackRequest
	body := http.MaxBytesReader(w, r.Body, tradeFeedbackBodyLimit)
	dec := json.NewDecoder(body)
	dec.DisallowUnknownFields()
	classify := func(err error, what string) *tradeReviewError {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			return reviewErr(http.StatusRequestEntityTooLarge, "request body exceeds %d bytes", tradeFeedbackBodyLimit)
		}
		return reviewErr(http.StatusBadRequest, "invalid json body: %s", what)
	}
	if err := dec.Decode(&req); err != nil {
		return req, classify(err, err.Error())
	}
	if err := dec.Decode(&struct{}{}); err != io.EOF {
		return req, classify(err, "trailing data")
	}
	req.PositionID = strings.TrimSpace(req.PositionID)
	if req.PositionID == "" {
		return req, reviewErr(http.StatusBadRequest, "position_id is required")
	}
	if !utf8.ValidString(req.Note) {
		return req, reviewErr(http.StatusBadRequest, "note is not valid UTF-8")
	}
	if n := utf8.RuneCountInString(req.Note); n > tradeFeedbackNoteMaxRunes {
		return req, reviewErr(http.StatusBadRequest, "note has %d characters, above the %d-character limit", n, tradeFeedbackNoteMaxRunes)
	}
	for _, p := range []struct {
		name string
		v    float64
	}{{"entry_price", req.EntryPrice}, {"exit_price", req.ExitPrice}} {
		if !tradeReviewFinite(p.v) || p.v <= 0 {
			return req, reviewErr(http.StatusBadRequest, "%s must be a finite positive price", p.name)
		}
	}
	if req.EntryTime >= req.ExitTime {
		return req, reviewErr(http.StatusBadRequest, "entry_time must be earlier than exit_time")
	}
	return req, nil
}

func tradeReviewBarAt(candles []UICandle, t int64) (UICandle, bool) {
	i := sort.Search(len(candles), func(i int) bool { return candles[i].Time >= t })
	if i < len(candles) && candles[i].Time == t {
		return candles[i], true
	}
	return UICandle{}, false
}

func (ss *StatusServer) tradeFeedbackContextRefusal(want tradeFeedbackContext) string {
	if reason := ss.removedStrategyRefusal(want.StrategyID); reason != "" {
		return reason
	}
	cur, ok := ss.strategyConfig(want.StrategyID)
	if !ok {
		return fmt.Sprintf("strategy %q left the running roster", want.StrategyID)
	}
	if tradeFeedbackContextFor(cur) != want {
		return fmt.Sprintf("strategy %q changed identity, mode or partition in the running roster", want.StrategyID)
	}
	if strings.TrimSpace(ss.configPath) == "" {
		return ""
	}
	raw, err := os.ReadFile(ss.configPath)
	if err != nil {
		return fmt.Sprintf("cannot read the config to confirm strategy %q: %v", want.StrategyID, err)
	}
	var root map[string]json.RawMessage
	if err := json.Unmarshal(raw, &root); err != nil {
		return fmt.Sprintf("cannot parse the config to confirm strategy %q: %v", want.StrategyID, err)
	}
	list, err := configStrategies(root)
	if err != nil {
		return fmt.Sprintf("cannot read the config strategies to confirm %q: %v", want.StrategyID, err)
	}
	for _, item := range list {
		var disk struct {
			ID                string   `json:"id"`
			Platform          string   `json:"platform"`
			Type              string   `json:"type"`
			Args              []string `json:"args"`
			PaperSource       string   `json:"paper_source"`
			StorageStrategyID string   `json:"storage_strategy_id"`
		}
		if json.Unmarshal(item, &disk) != nil || disk.ID != want.StrategyID {
			continue
		}
		storage := strings.TrimSpace(disk.StorageStrategyID)
		if storage == "" {
			storage = disk.ID
		}
		diskMode := "paper"
		if isLiveArgs(disk.Args) {
			diskMode = "live"
		}
		switch {
		case diskMode != want.Mode:
			return fmt.Sprintf("strategy %q has a pending %s-to-%s change in the config", want.StrategyID, want.Mode, diskMode)
		case want.Mode == "paper" && paperSourcePartition(strings.TrimSpace(disk.PaperSource)).String() != want.Partition:
			return fmt.Sprintf("strategy %q has a pending paper_source change in the config", want.StrategyID)
		case storage != want.StorageStrategyID:
			return fmt.Sprintf("strategy %q has a pending storage_strategy_id change in the config", want.StrategyID)
		case disk.Platform != "" && disk.Platform != want.Platform:
			return fmt.Sprintf("strategy %q has a pending platform change in the config", want.StrategyID)
		case disk.Type != "" && disk.Type != want.Type:
			return fmt.Sprintf("strategy %q has a pending type change in the config", want.StrategyID)
		}
		return ""
	}
	return fmt.Sprintf("strategy %q is no longer in the config; it was removed pending a restart", want.StrategyID)
}

func (ss *StatusServer) handleAPIStrategyTradeFeedback(w http.ResponseWriter, r *http.Request, id string) {
	if !ss.requireMutatingAPIAuth(w, r) || !requireSameOrigin(w, r) || !requireJSONContentType(w, r) {
		return
	}
	sc, ok := ss.strategyConfig(id)
	if !ok {
		writeJSONError(w, http.StatusNotFound, "strategy not found")
		return
	}
	req, rerr := decodeTradeFeedbackRequest(w, r)
	if rerr != nil {
		writeJSONError(w, rerr.Status, rerr.Msg)
		return
	}
	if ss.stateDB == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "database not available")
		return
	}
	ctx := tradeFeedbackContextFor(sc)
	now := time.Now().UTC()
	b, rerr := ss.buildTradeReview(sc, req.PositionID, now)
	if rerr != nil {
		writeJSONError(w, rerr.Status, rerr.Msg)
		return
	}
	a := b.analysis
	if !a.Eligible || !b.tfOK {
		reason := a.Reason
		if reason == "" {
			reason = "the position is not eligible for annotation"
		}
		writeJSONError(w, http.StatusConflict, reason)
		return
	}
	if b.window == nil {
		writeJSONError(w, http.StatusUnprocessableEntity, "no candle window: "+b.coverage.Reason)
		return
	}
	win := *b.window
	for _, sel := range []struct {
		name string
		t    int64
	}{{"entry_time", req.EntryTime}, {"exit_time", req.ExitTime}} {
		name, t := sel.name, sel.t
		if t < win.EffectiveFrom || t > win.EffectiveTo {
			writeJSONError(w, http.StatusBadRequest, fmt.Sprintf("%s %d is outside the effective window [%d, %d]", name, t, win.EffectiveFrom, win.EffectiveTo))
			return
		}
		if (t-win.EffectiveFrom)%win.BarSeconds != 0 {
			writeJSONError(w, http.StatusBadRequest, fmt.Sprintf("%s %d is not a %s bar opening time", name, t, win.Timeframe))
			return
		}
	}
	if !win.CloseBarComplete {
		writeJSONError(w, http.StatusUnprocessableEntity, "the closing bar is still forming; saving opens once it completes")
		return
	}
	if b.fetchErr != nil {
		writeJSONError(w, http.StatusBadGateway, "candle fetch failed: "+b.fetchErr.Error())
		return
	}
	if b.coverage.Status != tradeReviewCoverageFull {
		writeJSONError(w, http.StatusUnprocessableEntity, fmt.Sprintf("candle coverage is %s: %s", b.coverage.Status, b.coverage.Reason))
		return
	}
	entryBar, okEntry := tradeReviewBarAt(b.candles, req.EntryTime)
	exitBar, okExit := tradeReviewBarAt(b.candles, req.ExitTime)
	if !okEntry || !okExit {
		writeJSONError(w, http.StatusUnprocessableEntity, "a selected bar is not in the validated candles")
		return
	}
	if req.EntryPrice < entryBar.Low || req.EntryPrice > entryBar.High {
		writeJSONError(w, http.StatusUnprocessableEntity, fmt.Sprintf("entry_price %.10g is outside its bar [%.10g, %.10g]", req.EntryPrice, entryBar.Low, entryBar.High))
		return
	}
	if req.ExitPrice < exitBar.Low || req.ExitPrice > exitBar.High {
		writeJSONError(w, http.StatusUnprocessableEntity, fmt.Sprintf("exit_price %.10g is outside its bar [%.10g, %.10g]", req.ExitPrice, exitBar.Low, exitBar.High))
		return
	}

	evidence := tradeFeedbackEvidence{
		Version:           tradeFeedbackEvidenceVersion,
		Strategy:          ctx,
		SourceRole:        string(b.ev.SourceRole),
		PositionID:        req.PositionID,
		Symbol:            a.Symbol,
		Side:              a.Side,
		SourceFingerprint: b.fp,
		Fills:             a.Evidence,
		Diagnostics:       *a.Diagnostic,
		Actual:            a.Actual,
		Semantics:         tradeReviewSemantics,
		Window:            win,
		Candles: tradeFeedbackCandleEvidence{
			Source: b.source, Timeframe: b.tf, TimestampKind: "open", Coverage: b.coverage, Bars: b.candles,
		},
		Selection: tradeFeedbackSelection{
			EntryTime: req.EntryTime, EntryPrice: req.EntryPrice, EntryBar: entryBar,
			ExitTime: req.ExitTime, ExitPrice: req.ExitPrice, ExitBar: exitBar,
		},
		ValidatedAt: now,
	}
	evidenceJSON, err := json.Marshal(evidence)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "encode evidence: "+err.Error())
		return
	}
	sum := sha256.Sum256(evidenceJSON)
	rec := &tradeFeedbackRecord{
		PositionID:          req.PositionID,
		Symbol:              a.Symbol,
		Side:                a.Side,
		Platform:            sc.Platform,
		InstrumentType:      sc.Type,
		Timeframe:           b.tf,
		PreferredEntryTime:  req.EntryTime,
		PreferredEntryPrice: req.EntryPrice,
		PreferredExitTime:   req.ExitTime,
		PreferredExitPrice:  req.ExitPrice,
		ActualOpenedAt:      a.FirstFill,
		ActualClosedAt:      a.LastClose,
		ActualAvgEntryPrice: a.Actual.AvgEntryPrice,
		ActualAvgExitPrice:  a.Actual.AvgExitPrice,
		ActualRealizedPnL:   a.Actual.RealizedPnL,
		ActualRealizedBasis: tradeReviewRealizedBasis,
		CloseReason:         a.Actual.CloseReason,
		EntryATR:            a.Actual.EntryATR,
		Note:                req.Note,
		EvidenceVersion:     tradeFeedbackEvidenceVersion,
		EvidenceJSON:        string(evidenceJSON),
		EvidenceSHA256:      hex.EncodeToString(sum[:]),
		CreatedAt:           time.Now().UTC(),
	}

	ss.configWriteMu.Lock()
	refusal := ss.tradeFeedbackContextRefusal(ctx)
	var insertErr error
	if refusal == "" {
		insertErr = ss.stateDB.InsertTradeFeedbackChecked(sc.ID, rec, func(cur *positionEvidence) error {
			fp, err := tradeReviewSourceFingerprint(cur)
			if err != nil {
				return err
			}
			if fp != b.fp {
				return errTradeFeedbackStale
			}
			return nil
		})
	}
	ss.configWriteMu.Unlock()
	if refusal != "" {
		writeJSONError(w, http.StatusConflict, refusal+"; nothing was saved")
		return
	}
	if errors.Is(insertErr, errTradeFeedbackStale) {
		writeJSONError(w, http.StatusConflict, "the position's trades or diagnostics changed during validation; nothing was saved, reload and try again")
		return
	}
	if insertErr != nil {
		writeJSONError(w, http.StatusInternalServerError, "save trade feedback: "+insertErr.Error())
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	json.NewEncoder(w).Encode(map[string]interface{}{"ok": true, "annotation": rec})
}

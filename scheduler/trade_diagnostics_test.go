package main

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func diagCandle(t int64, o, h, l, c float64) UICandle {
	return UICandle{Time: t, Open: o, High: h, Low: l, Close: c}
}

func TestComputeTradeQualityLongWinner(t *testing.T) {
	candles := []UICandle{
		diagCandle(0, 100, 104, 99, 103),
		diagCandle(3600, 103, 110, 102, 108),
		diagCandle(7200, 108, 109, 105, 106),
	}
	m, ok := computeTradeQuality(candles, "long", 100, 106)
	if !ok {
		t.Fatal("expected metrics")
	}
	if m.MFEPrice != 110 || m.MAEPrice != 99 {
		t.Fatalf("MFE/MAE = %v/%v, want 110/99", m.MFEPrice, m.MAEPrice)
	}
	if m.FavorablePct != 10 {
		t.Fatalf("favorable = %v, want 10", m.FavorablePct)
	}
	if m.AdversePct != 1 {
		t.Fatalf("adverse = %v, want 1", m.AdversePct)
	}
	if m.CaptureRatio == nil || *m.CaptureRatio != 0.6 {
		t.Fatalf("capture = %v, want 0.6", m.CaptureRatio)
	}
}

func TestComputeTradeQualityShortWinner(t *testing.T) {
	candles := []UICandle{
		diagCandle(0, 100, 102, 95, 96),
		diagCandle(3600, 96, 98, 90, 92),
	}
	m, ok := computeTradeQuality(candles, "short", 100, 95)
	if !ok {
		t.Fatal("expected metrics")
	}
	if m.MFEPrice != 90 || m.MAEPrice != 102 {
		t.Fatalf("MFE/MAE = %v/%v, want 90/102", m.MFEPrice, m.MAEPrice)
	}
	if m.FavorablePct != 10 || m.AdversePct != 2 {
		t.Fatalf("favorable/adverse = %v/%v, want 10/2", m.FavorablePct, m.AdversePct)
	}
	if m.CaptureRatio == nil || *m.CaptureRatio != 0.5 {
		t.Fatalf("capture = %v, want 0.5", m.CaptureRatio)
	}
}

func TestComputeTradeQualityLoserHasNoCaptureRatio(t *testing.T) {
	candles := []UICandle{diagCandle(0, 100, 101, 94, 95)}
	m, ok := computeTradeQuality(candles, "long", 100, 95)
	if !ok {
		t.Fatal("expected metrics")
	}
	if m.CaptureRatio != nil {
		t.Fatalf("losers must not get a capture ratio, got %v", *m.CaptureRatio)
	}
	if m.AdversePct != 6 {
		t.Fatalf("adverse = %v, want 6", m.AdversePct)
	}
}

func TestComputeTradeQualityImmediateReversal(t *testing.T) {
	// Price never went favorable: MFE floors at entry, no capture ratio even
	// though exit > entry is impossible here.
	candles := []UICandle{diagCandle(0, 100, 100, 92, 93)}
	m, ok := computeTradeQuality(candles, "long", 100, 93)
	if !ok {
		t.Fatal("expected metrics")
	}
	if m.MFEPrice != 100 || m.FavorablePct != 0 {
		t.Fatalf("MFE = %v favorable = %v, want entry/0", m.MFEPrice, m.FavorablePct)
	}
	if m.CaptureRatio != nil {
		t.Fatal("no favorable move → no capture ratio")
	}
}

func TestComputeTradeQualitySingleBarHold(t *testing.T) {
	candles := []UICandle{diagCandle(0, 100, 105, 98, 104)}
	m, ok := computeTradeQuality(candles, "long", 100, 104)
	if !ok {
		t.Fatal("expected metrics")
	}
	if m.MFEPrice != 105 || m.MAEPrice != 98 {
		t.Fatalf("MFE/MAE = %v/%v, want 105/98", m.MFEPrice, m.MAEPrice)
	}
	if m.CaptureRatio == nil || *m.CaptureRatio != 0.8 {
		t.Fatalf("capture = %v, want 0.8", m.CaptureRatio)
	}
}

func TestComputeTradeQualityCaptureClampsAtOne(t *testing.T) {
	// Exit fill better than any candle extreme (gap fill): ratio clamps to 1.
	candles := []UICandle{diagCandle(0, 100, 104, 99, 104)}
	m, ok := computeTradeQuality(candles, "long", 100, 106)
	if !ok {
		t.Fatal("expected metrics")
	}
	if m.CaptureRatio == nil || *m.CaptureRatio != 1 {
		t.Fatalf("capture = %v, want clamp to 1", m.CaptureRatio)
	}
}

func TestComputeTradeQualityBadInputs(t *testing.T) {
	if _, ok := computeTradeQuality(nil, "long", 100, 105); ok {
		t.Fatal("no candles must fail")
	}
	if _, ok := computeTradeQuality([]UICandle{diagCandle(0, 1, 1, 1, 1)}, "long", 0, 105); ok {
		t.Fatal("zero entry must fail")
	}
}

func TestDiagTimeframeDuration(t *testing.T) {
	cases := map[string]time.Duration{
		"1m": time.Minute, "15m": 15 * time.Minute, "1h": time.Hour,
		"4h": 4 * time.Hour, "1d": 24 * time.Hour, "1w": 7 * 24 * time.Hour,
	}
	for tf, want := range cases {
		got, ok := diagTimeframeDuration(tf)
		if !ok || got != want {
			t.Fatalf("diagTimeframeDuration(%q) = %v/%v, want %v", tf, got, ok, want)
		}
	}
	for _, bad := range []string{"", "h", "0m", "-5m", "1x", "abc"} {
		if _, ok := diagTimeframeDuration(bad); ok {
			t.Fatalf("diagTimeframeDuration(%q) should fail", bad)
		}
	}
}

func TestCaptureTradeDiagnosticsFromRecordClosedPosition(t *testing.T) {
	prevRec, prevEnq := tradeDiagnosticsRecorder, tradeDiagnosticsEnqueue
	defer func() { tradeDiagnosticsRecorder, tradeDiagnosticsEnqueue = prevRec, prevEnq }()

	var inserted []TradeDiagnosticsRow
	var enqueued []TradeDiagnosticsRow
	tradeDiagnosticsRecorder = func(row *TradeDiagnosticsRow) error {
		row.RowID = int64(len(inserted) + 1)
		inserted = append(inserted, *row)
		return nil
	}
	tradeDiagnosticsEnqueue = func(row TradeDiagnosticsRow) { enqueued = append(enqueued, row) }

	slMult := 1.5
	opened := time.Date(2026, 6, 1, 10, 0, 0, 0, time.UTC)
	closed := opened.Add(6 * time.Hour)
	s := &StrategyState{ID: "hl-test", Positions: map[string]*Position{}}
	pos := &Position{
		Symbol: "ETH", TradePositionID: "pos-1", Quantity: 2, AvgCost: 3000,
		Side: "long", EntryATR: 40, StopLossATRMult: &slMult,
		Regime: "trending_up", OpenedAt: opened,
	}
	recordClosedPosition(s, pos, 3100, 200, "signal", closed)

	if len(inserted) != 1 || len(enqueued) != 1 {
		t.Fatalf("inserted=%d enqueued=%d, want 1/1", len(inserted), len(enqueued))
	}
	row := enqueued[0]
	if row.RowID != 1 {
		t.Fatalf("enqueued row must carry the inserted rowid, got %d", row.RowID)
	}
	if row.StrategyID != "hl-test" || row.PositionID != "pos-1" || row.Symbol != "ETH" ||
		row.Side != "long" || row.RegimeAtOpen != "trending_up" || row.CloseReason != "signal" {
		t.Fatalf("identity fields wrong: %+v", row)
	}
	if row.EntryPrice != 3000 || row.ExitPrice != 3100 || row.RealizedPnL != 200 || row.EntryATR != 40 {
		t.Fatalf("outcome fields wrong: %+v", row)
	}
	if row.StopLossATRMult == nil || *row.StopLossATRMult != 1.5 {
		t.Fatalf("stop mult wrong: %+v", row.StopLossATRMult)
	}
	if !row.OpenedAt.Equal(opened) || !row.ClosedAt.Equal(closed) {
		t.Fatalf("timestamps wrong: %+v", row)
	}
	if row.MetricsStatus != diagMetricsPending {
		t.Fatalf("status = %q, want pending", row.MetricsStatus)
	}
}

func TestCaptureTradeDiagnosticsNilRecorderNoop(t *testing.T) {
	prevRec, prevEnq := tradeDiagnosticsRecorder, tradeDiagnosticsEnqueue
	defer func() { tradeDiagnosticsRecorder, tradeDiagnosticsEnqueue = prevRec, prevEnq }()
	tradeDiagnosticsRecorder = nil
	tradeDiagnosticsEnqueue = func(TradeDiagnosticsRow) { t.Fatal("must not enqueue without a recorder") }

	s := &StrategyState{ID: "x", Positions: map[string]*Position{}}
	recordClosedPosition(s, &Position{Symbol: "BTC", AvgCost: 1, Quantity: 1}, 1, 0, "signal", time.Now().UTC())
}

type diagWorkerFixture struct {
	worker  *tradeDiagnosticsWorker
	fetched []UICandleRequest
	updates []string
	metrics []*tradeQualityMetrics
	tfs     []string
}

func newDiagWorkerFixture(candles []UICandle, fetchErr error) *diagWorkerFixture {
	f := &diagWorkerFixture{}
	f.worker = newTradeDiagnosticsWorker(
		func(req UICandleRequest) ([]UICandle, string, error) {
			f.fetched = append(f.fetched, req)
			return candles, "test", fetchErr
		},
		func(rowID int64, tf string, m *tradeQualityMetrics, status string) error {
			f.updates = append(f.updates, status)
			f.metrics = append(f.metrics, m)
			f.tfs = append(f.tfs, tf)
			return nil
		},
	)
	f.worker.UpdateStrategies([]StrategyConfig{{ID: "hl-test", Platform: "hyperliquid", Type: "perps", Symbol: "ETH", Timeframe: "1h"}})
	return f
}

func diagTestRow(opened, closed time.Time) TradeDiagnosticsRow {
	return TradeDiagnosticsRow{
		RowID: 7, StrategyID: "hl-test", PositionID: "p1", Symbol: "ETH",
		Side: "long", EntryPrice: 3000, ExitPrice: 3100,
		OpenedAt: opened, ClosedAt: closed, MetricsStatus: diagMetricsPending,
	}
}

func TestDiagnosticsWorkerHappyPath(t *testing.T) {
	opened := time.Date(2026, 6, 1, 10, 0, 0, 0, time.UTC)
	closed := opened.Add(3 * time.Hour)
	candles := []UICandle{
		diagCandle(opened.Unix(), 3000, 3050, 2980, 3040),
		diagCandle(opened.Add(time.Hour).Unix(), 3040, 3200, 3030, 3150),
		diagCandle(opened.Add(2*time.Hour).Unix(), 3150, 3160, 3080, 3100),
	}
	f := newDiagWorkerFixture(candles, nil)
	f.worker.process(diagTestRow(opened, closed))

	if len(f.updates) != 1 || f.updates[0] != diagMetricsOK {
		t.Fatalf("updates = %v, want [ok]", f.updates)
	}
	if f.tfs[0] != "1h" {
		t.Fatalf("timeframe = %q, want 1h", f.tfs[0])
	}
	m := f.metrics[0]
	if m == nil || m.MFEPrice != 3200 || m.MAEPrice != 2980 {
		t.Fatalf("metrics = %+v, want MFE 3200 MAE 2980", m)
	}
	if len(f.fetched) != 1 {
		t.Fatalf("fetched %d times, want 1", len(f.fetched))
	}
	req := f.fetched[0]
	if !req.From.Equal(opened.Truncate(time.Hour)) || !req.To.Equal(closed) {
		t.Fatalf("fetch window = %v..%v, want %v..%v", req.From, req.To, opened, closed)
	}
}

func TestDiagnosticsWorkerFailurePaths(t *testing.T) {
	opened := time.Date(2026, 6, 1, 10, 0, 0, 0, time.UTC)
	closed := opened.Add(3 * time.Hour)

	t.Run("fetch error", func(t *testing.T) {
		f := newDiagWorkerFixture(nil, fmt.Errorf("boom"))
		f.worker.process(diagTestRow(opened, closed))
		if f.updates[0] != diagMetricsFetchFailed || f.metrics[0] != nil {
			t.Fatalf("got %v/%v, want fetch_failed/nil", f.updates[0], f.metrics[0])
		}
	})
	t.Run("no candles", func(t *testing.T) {
		f := newDiagWorkerFixture(nil, nil)
		f.worker.process(diagTestRow(opened, closed))
		if f.updates[0] != diagMetricsNoCandles {
			t.Fatalf("got %v, want no_candles", f.updates[0])
		}
	})
	t.Run("uncovered window", func(t *testing.T) {
		// Earliest candle starts two hours after the open: metrics would be
		// biased, so they must be refused.
		f := newDiagWorkerFixture([]UICandle{diagCandle(opened.Add(2*time.Hour).Unix(), 3150, 3160, 3080, 3100)}, nil)
		f.worker.process(diagTestRow(opened, closed))
		if f.updates[0] != diagMetricsWindowUncovered || f.metrics[0] != nil {
			t.Fatalf("got %v/%v, want window_uncovered/nil", f.updates[0], f.metrics[0])
		}
	})
	t.Run("unknown strategy", func(t *testing.T) {
		f := newDiagWorkerFixture(nil, nil)
		row := diagTestRow(opened, closed)
		row.StrategyID = "gone"
		f.worker.process(row)
		if f.updates[0] != diagMetricsNoStrategyMeta {
			t.Fatalf("got %v, want no_strategy_meta", f.updates[0])
		}
		if len(f.fetched) != 0 {
			t.Fatal("must not fetch without strategy meta")
		}
	})
	t.Run("bad inputs", func(t *testing.T) {
		f := newDiagWorkerFixture(nil, nil)
		row := diagTestRow(opened, closed)
		row.EntryPrice = 0
		f.worker.process(row)
		if f.updates[0] != diagMetricsBadInputs {
			t.Fatalf("got %v, want bad_inputs", f.updates[0])
		}
	})
	t.Run("hold longer than fetch cap", func(t *testing.T) {
		f := newDiagWorkerFixture(nil, nil)
		row := diagTestRow(opened.Add(-diagMaxFetchBars*2*time.Hour), closed)
		f.worker.process(row)
		if f.updates[0] != diagMetricsWindowUncovered {
			t.Fatalf("got %v, want window_uncovered", f.updates[0])
		}
		if len(f.fetched) != 0 {
			t.Fatal("must not fetch a window it cannot cover")
		}
	})
	t.Run("missing timeframe defaults to 1h and fetches at 1h", func(t *testing.T) {
		// Manual strategy with no sc.Timeframe and <3 args (the exact case the
		// default targets): window math AND the candle fetch must use 1h, so the
		// row reaches metrics_status=ok rather than fetch_failed.
		f := newDiagWorkerFixture([]UICandle{diagCandle(opened.Unix(), 3000, 3200, 2980, 3100)}, nil)
		f.worker.UpdateStrategies([]StrategyConfig{{ID: "hl-test", Platform: "hyperliquid", Type: "manual", Symbol: "ETH"}})
		f.worker.process(diagTestRow(opened, closed))
		if f.updates[0] != diagMetricsOK {
			t.Fatalf("status = %q, want %q", f.updates[0], diagMetricsOK)
		}
		if f.tfs[0] != diagDefaultTimeframe {
			t.Fatalf("timeframe = %q, want %q", f.tfs[0], diagDefaultTimeframe)
		}
		if f.fetched[0].Strategy.Timeframe != diagDefaultTimeframe {
			t.Fatalf("fetch timeframe = %q, want %q", f.fetched[0].Strategy.Timeframe, diagDefaultTimeframe)
		}
	})
	t.Run("unknown timeframe token uses 1h for both window math and fetch", func(t *testing.T) {
		// diagTimeframeDuration rejects the token: the window math falls back to
		// 1h, and the fetch must be re-pointed at 1h too (not the bad token).
		f := newDiagWorkerFixture([]UICandle{diagCandle(opened.Unix(), 3000, 3200, 2980, 3100)}, nil)
		f.worker.UpdateStrategies([]StrategyConfig{{ID: "hl-test", Platform: "hyperliquid", Type: "manual", Symbol: "ETH", Timeframe: "bogus"}})
		f.worker.process(diagTestRow(opened, closed))
		if f.updates[0] != diagMetricsOK {
			t.Fatalf("status = %q, want %q", f.updates[0], diagMetricsOK)
		}
		if f.tfs[0] != diagDefaultTimeframe {
			t.Fatalf("timeframe = %q, want %q", f.tfs[0], diagDefaultTimeframe)
		}
		if f.fetched[0].Strategy.Timeframe != diagDefaultTimeframe {
			t.Fatalf("fetch timeframe = %q, want %q", f.fetched[0].Strategy.Timeframe, diagDefaultTimeframe)
		}
	})
	t.Run("explicit timeframe fetches unchanged", func(t *testing.T) {
		// A valid explicit timeframe must reach the fetch verbatim — no regression
		// from the resolution→fetch wiring.
		f := newDiagWorkerFixture([]UICandle{diagCandle(opened.Truncate(15*time.Minute).Unix(), 3000, 3200, 2980, 3100)}, nil)
		f.worker.UpdateStrategies([]StrategyConfig{{ID: "hl-test", Platform: "hyperliquid", Type: "perps", Symbol: "ETH", Timeframe: "15m"}})
		f.worker.process(diagTestRow(opened, closed))
		if f.updates[0] != diagMetricsOK {
			t.Fatalf("status = %q, want %q", f.updates[0], diagMetricsOK)
		}
		if f.tfs[0] != "15m" {
			t.Fatalf("timeframe = %q, want 15m", f.tfs[0])
		}
		if f.fetched[0].Strategy.Timeframe != "15m" {
			t.Fatalf("fetch timeframe = %q, want 15m", f.fetched[0].Strategy.Timeframe)
		}
	})
}

func TestTradeDiagnosticsDBRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	sdb, err := OpenStateDB(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}

	slMult := 2.0
	opened := time.Date(2026, 6, 1, 10, 0, 0, 0, time.UTC)
	closed := opened.Add(4 * time.Hour)
	row := &TradeDiagnosticsRow{
		StrategyID: "hl-a", PositionID: "p1", Symbol: "BTC", Side: "long",
		RegimeAtOpen: "ranging_quiet", CloseReason: "signal",
		EntryPrice: 50000, ExitPrice: 51000, Quantity: 0.1, RealizedPnL: 100,
		EntryATR: 500, StopLossATRMult: &slMult,
		OpenedAt: opened, ClosedAt: closed, MetricsStatus: diagMetricsPending,
	}
	if err := sdb.InsertTradeDiagnostics(row); err != nil {
		t.Fatalf("insert: %v", err)
	}
	if row.RowID == 0 {
		t.Fatal("insert must stamp RowID")
	}

	capture := 0.4
	m := &tradeQualityMetrics{MFEPrice: 52500, MAEPrice: 49500, FavorablePct: 5, AdversePct: 1, CaptureRatio: &capture}
	if err := sdb.UpdateTradeDiagnosticsMetrics(row.RowID, "1h", m, diagMetricsOK); err != nil {
		t.Fatalf("update: %v", err)
	}

	rows, err := sdb.TradeDiagnosticsRows("hl-a")
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("rows = %d, want 1", len(rows))
	}
	got := rows[0]
	if got.MetricsStatus != diagMetricsOK || got.Timeframe != "1h" {
		t.Fatalf("status/timeframe = %q/%q", got.MetricsStatus, got.Timeframe)
	}
	if got.MFEPrice == nil || *got.MFEPrice != 52500 || got.CaptureRatio == nil || *got.CaptureRatio != 0.4 {
		t.Fatalf("metrics round-trip wrong: %+v", got)
	}
	if got.StopLossATRMult == nil || *got.StopLossATRMult != 2.0 {
		t.Fatalf("stop mult round-trip wrong: %+v", got.StopLossATRMult)
	}
	if got.LLMVerdict != nil {
		t.Fatal("llm_verdict must stay NULL")
	}
	if !got.OpenedAt.Equal(opened) || !got.ClosedAt.Equal(closed) {
		t.Fatalf("timestamps round-trip wrong: %+v", got)
	}

	// Status-only update (failure path) leaves quality columns NULL.
	row2 := &TradeDiagnosticsRow{StrategyID: "hl-a", Symbol: "BTC", MetricsStatus: diagMetricsPending, OpenedAt: opened, ClosedAt: closed}
	if err := sdb.InsertTradeDiagnostics(row2); err != nil {
		t.Fatalf("insert2: %v", err)
	}
	if err := sdb.UpdateTradeDiagnosticsMetrics(row2.RowID, "1h", nil, diagMetricsFetchFailed); err != nil {
		t.Fatalf("update2: %v", err)
	}
	rows, err = sdb.TradeDiagnosticsRows("hl-a")
	if err != nil {
		t.Fatalf("query2: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("rows = %d, want 2", len(rows))
	}
	if rows[1].MetricsStatus != diagMetricsFetchFailed || rows[1].MFEPrice != nil {
		t.Fatalf("failure row wrong: %+v", rows[1])
	}

	// Idempotent migration: reopening the same DB must not error.
	if err := sdb.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	sdb2, err := OpenStateDB(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer sdb2.Close()
	rows, err = sdb2.TradeDiagnosticsRows("")
	if err != nil {
		t.Fatalf("query after reopen: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("rows after reopen = %d, want 2", len(rows))
	}
}

func TestNetPnLByPositionAggregatesLegs(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	sdb, err := OpenStateDB(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer sdb.Close()

	now := time.Now().UTC()
	// Two tiered-TP close legs of the same position under the #954 gross
	// convention: net = (60-1) + (50-1) = 108.
	for i, pnl := range []float64{60, 50} {
		trade := Trade{
			Timestamp: now.Add(time.Duration(i) * time.Minute), Symbol: "ETH", Side: "sell",
			Quantity: 1, Price: 3100, Value: 3100, PositionID: "p1",
			IsClose: true, RealizedPnL: pnl, ExchangeFee: 1, PnLGross: true, FeeSource: FeeSourceModeled,
		}
		if err := sdb.InsertTrade("hl-a", trade); err != nil {
			t.Fatalf("insert trade: %v", err)
		}
	}
	// Legacy-convention close leg of a different position: net = RealizedPnL as-is.
	legacy := Trade{Timestamp: now, Symbol: "ETH", Side: "sell", Quantity: 1, Price: 3000, Value: 3000,
		PositionID: "p2", IsClose: true, RealizedPnL: -25}
	if err := sdb.InsertTrade("hl-a", legacy); err != nil {
		t.Fatalf("insert legacy: %v", err)
	}
	// Open leg must not contribute.
	open := Trade{Timestamp: now, Symbol: "ETH", Side: "buy", Quantity: 1, Price: 3000, Value: 3000,
		PositionID: "p1", PnLGross: true, ExchangeFee: 1, FeeSource: FeeSourceModeled}
	if err := sdb.InsertTrade("hl-a", open); err != nil {
		t.Fatalf("insert open: %v", err)
	}

	net, err := sdb.NetPnLByPosition("hl-a")
	if err != nil {
		t.Fatalf("net: %v", err)
	}
	if got := net["hl-a"]["p1"]; got != 108 {
		t.Fatalf("p1 net = %v, want 108", got)
	}
	if got := net["hl-a"]["p2"]; got != -25 {
		t.Fatalf("p2 net = %v, want -25", got)
	}
}

func fptr(v float64) *float64 { return &v }

func diagReportRow(i int, regime, side string, net float64, capture *float64) TradeDiagnosticsRow {
	opened := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC).Add(time.Duration(i) * time.Hour)
	r := TradeDiagnosticsRow{
		StrategyID: "hl-a", PositionID: fmt.Sprintf("p%d", i), Symbol: "ETH",
		Timeframe: "1h", Side: side, RegimeAtOpen: regime, CloseReason: "signal",
		EntryPrice: 3000, ExitPrice: 3000 + net, Quantity: 1, RealizedPnL: net,
		EntryATR: 30, OpenedAt: opened, ClosedAt: opened.Add(2 * time.Hour),
		MetricsStatus: diagMetricsOK,
		FavorablePct:  fptr(2), AdversePct: fptr(0.5),
		MFEPrice: fptr(3060), MAEPrice: fptr(2985),
	}
	r.CaptureRatio = capture
	return r
}

func TestDiagnosticsReportSampleGating(t *testing.T) {
	var rows []TradeDiagnosticsRow
	for i := 0; i < 5; i++ {
		rows = append(rows, diagReportRow(i, "trending_up", "long", 10, fptr(0.9)))
	}
	out := buildTradeDiagnosticsReport(rows, nil, "cfg.json", diagReportOptions{MinTrades: 30, MinBucket: 10})
	if !strings.Contains(out, "insufficient data, 5/30") {
		t.Fatalf("gating line missing:\n%s", out)
	}
	if strings.Contains(out, "- [") {
		t.Fatalf("hypotheses must be suppressed below the sample threshold:\n%s", out)
	}
}

func TestDiagnosticsReportCaptureAndRegimeHypotheses(t *testing.T) {
	var rows []TradeDiagnosticsRow
	// 20 winners in trending_up with low capture, 12 losers in ranging_choppy.
	for i := 0; i < 20; i++ {
		rows = append(rows, diagReportRow(i, "trending_up", "long", 10, fptr(0.2)))
	}
	for i := 20; i < 32; i++ {
		rows = append(rows, diagReportRow(i, "ranging_choppy", "long", -15, nil))
	}
	out := buildTradeDiagnosticsReport(rows, nil, "cfg.json", diagReportOptions{MinTrades: 30, MinBucket: 10})
	if !strings.Contains(out, "[capture]") {
		t.Fatalf("capture hypothesis missing:\n%s", out)
	}
	if !strings.Contains(out, `regime "ranging_choppy" is net-negative`) {
		t.Fatalf("regime hypothesis missing:\n%s", out)
	}
	if strings.Contains(out, `regime "trending_up" is net-negative`) {
		t.Fatalf("profitable regime must not be flagged:\n%s", out)
	}
	if !strings.Contains(out, "run_backtest.py --config cfg.json --strategy hl-a --mode single") {
		t.Fatalf("backtest command missing:\n%s", out)
	}
}

func TestDiagnosticsReportDirectionHypothesis(t *testing.T) {
	var rows []TradeDiagnosticsRow
	for i := 0; i < 20; i++ {
		rows = append(rows, diagReportRow(i, "trending_up", "long", 10, fptr(0.9)))
	}
	for i := 20; i < 32; i++ {
		rows = append(rows, diagReportRow(i, "trending_up", "short", -12, nil))
	}
	out := buildTradeDiagnosticsReport(rows, nil, "cfg.json", diagReportOptions{MinTrades: 30, MinBucket: 10})
	if !strings.Contains(out, "short side is net-negative") {
		t.Fatalf("direction hypothesis missing:\n%s", out)
	}
	if strings.Contains(out, "long side is net-negative") {
		t.Fatalf("profitable side must not be flagged:\n%s", out)
	}
}

func TestDiagnosticsReportPartialCloseAggregation(t *testing.T) {
	// The diagnostics row stores only the final leg's PnL (-5), but the trades
	// join says the position's legs sum to +40 net — the report must use +40.
	row := diagReportRow(0, "trending_up", "long", -5, nil)
	net := map[string]map[string]float64{"hl-a": {"p0": 40}}
	out := buildTradeDiagnosticsReport([]TradeDiagnosticsRow{row}, net, "cfg.json", diagReportOptions{MinTrades: 30, MinBucket: 10})
	if !strings.Contains(out, "wins: 1  losses: 0") {
		t.Fatalf("partial-close aggregation not applied:\n%s", out)
	}
	if !strings.Contains(out, "net PnL: $40.00") {
		t.Fatalf("net PnL must come from the trades join:\n%s", out)
	}
}

func TestDiagnosticsReportExcludesSyntheticCloses(t *testing.T) {
	rows := []TradeDiagnosticsRow{
		diagReportRow(0, "trending_up", "long", 10, fptr(0.9)),
	}
	ext := diagReportRow(1, "trending_up", "long", 0, nil)
	ext.CloseReason = "hl_sync_external"
	corrupt := diagReportRow(2, "trending_up", "long", 0, nil)
	corrupt.CloseReason = "circuit_breaker_corrupt"
	rows = append(rows, ext, corrupt)

	out := buildTradeDiagnosticsReport(rows, nil, "cfg.json", diagReportOptions{MinTrades: 30, MinBucket: 10})
	if !strings.Contains(out, "closed positions: 1 (+2 excluded") {
		t.Fatalf("synthetic closes must be excluded from aggregates:\n%s", out)
	}
}

func TestAgentInfoCommandsIncludeDiagnostics(t *testing.T) {
	for _, cmd := range agentInfoCommands {
		if cmd.Name == "diagnostics" {
			return
		}
	}
	t.Fatal("diagnostics subcommand missing from agentInfoCommands")
}

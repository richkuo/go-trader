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

func TestComputeTradeQuality(t *testing.T) {
	f := func(v float64) *float64 { return &v }
	cases := []struct {
		name          string
		candles       []UICandle
		side          string
		entry, exit   float64
		wantOK        bool
		wantMFE       *float64
		wantMAE       *float64
		wantFav       *float64
		wantAdv       *float64
		wantCapture   *float64
		wantNoCapture bool
	}{
		{
			name: "long winner",
			candles: []UICandle{
				diagCandle(0, 100, 104, 99, 103),
				diagCandle(3600, 103, 110, 102, 108),
				diagCandle(7200, 108, 109, 105, 106),
			},
			side: "long", entry: 100, exit: 106, wantOK: true,
			wantMFE: f(110), wantMAE: f(99), wantFav: f(10), wantAdv: f(1), wantCapture: f(0.6),
		},
		{
			name: "short winner",
			candles: []UICandle{
				diagCandle(0, 100, 102, 95, 96),
				diagCandle(3600, 96, 98, 90, 92),
			},
			side: "short", entry: 100, exit: 95, wantOK: true,
			wantMFE: f(90), wantMAE: f(102), wantFav: f(10), wantAdv: f(2), wantCapture: f(0.5),
		},
		{
			name:    "loser has no capture ratio",
			candles: []UICandle{diagCandle(0, 100, 101, 94, 95)},
			side:    "long", entry: 100, exit: 95, wantOK: true,
			wantAdv: f(6), wantNoCapture: true,
		},
		{
			name:    "immediate reversal",
			candles: []UICandle{diagCandle(0, 100, 100, 92, 93)},
			side:    "long", entry: 100, exit: 93, wantOK: true,
			wantMFE: f(100), wantFav: f(0), wantNoCapture: true,
		},
		{
			name:    "single bar hold",
			candles: []UICandle{diagCandle(0, 100, 105, 98, 104)},
			side:    "long", entry: 100, exit: 104, wantOK: true,
			wantMFE: f(105), wantMAE: f(98), wantCapture: f(0.8),
		},
		{
			name:    "capture clamps at one",
			candles: []UICandle{diagCandle(0, 100, 104, 99, 104)},
			side:    "long", entry: 100, exit: 106, wantOK: true,
			wantCapture: f(1),
		},
		{
			name: "no candles fails", candles: nil,
			side: "long", entry: 100, exit: 105, wantOK: false,
		},
		{
			name: "zero entry fails", candles: []UICandle{diagCandle(0, 1, 1, 1, 1)},
			side: "long", entry: 0, exit: 105, wantOK: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m, ok := computeTradeQuality(tc.candles, tc.side, tc.entry, tc.exit)
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tc.wantOK)
			}
			if !tc.wantOK {
				return
			}
			if tc.wantMFE != nil && m.MFEPrice != *tc.wantMFE {
				t.Fatalf("MFE = %v, want %v", m.MFEPrice, *tc.wantMFE)
			}
			if tc.wantMAE != nil && m.MAEPrice != *tc.wantMAE {
				t.Fatalf("MAE = %v, want %v", m.MAEPrice, *tc.wantMAE)
			}
			if tc.wantFav != nil && m.FavorablePct != *tc.wantFav {
				t.Fatalf("favorable = %v, want %v", m.FavorablePct, *tc.wantFav)
			}
			if tc.wantAdv != nil && m.AdversePct != *tc.wantAdv {
				t.Fatalf("adverse = %v, want %v", m.AdversePct, *tc.wantAdv)
			}
			if tc.wantNoCapture && m.CaptureRatio != nil {
				t.Fatalf("expected no capture ratio, got %v", *m.CaptureRatio)
			}
			if tc.wantCapture != nil {
				if m.CaptureRatio == nil || *m.CaptureRatio != *tc.wantCapture {
					t.Fatalf("capture = %v, want %v", m.CaptureRatio, *tc.wantCapture)
				}
			}
		})
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
	if len(s.pendingTradeDiagnostics) != 0 {
		t.Fatalf("nil recorder buffered %d pending rows; defer only happens under suspendEagerDiagnosticsPersist", len(s.pendingTradeDiagnostics))
	}
}

func TestCaptureTradeDiagnosticsDeferredBuffersUntilSave(t *testing.T) {
	prevRec, prevEnq, prevDef := tradeDiagnosticsRecorder, tradeDiagnosticsEnqueue, tradeDiagnosticsPersistDeferred
	defer func() {
		tradeDiagnosticsRecorder, tradeDiagnosticsEnqueue = prevRec, prevEnq
		tradeDiagnosticsPersistDeferred = prevDef
	}()
	var inserts int
	tradeDiagnosticsRecorder = func(*TradeDiagnosticsRow) error {
		inserts++
		return nil
	}
	tradeDiagnosticsEnqueue = func(TradeDiagnosticsRow) { t.Fatal("must not enqueue while deferred") }
	restore := suspendEagerDiagnosticsPersist()
	defer restore()

	s := &StrategyState{ID: "hl-test", Positions: map[string]*Position{}}
	recordClosedPosition(s, &Position{Symbol: "ETH", TradePositionID: "pos-1", AvgCost: 1, Quantity: 1}, 1, 0, "signal", time.Now().UTC())
	if inserts != 0 {
		t.Fatalf("eager insert ran %d time(s) while deferred, want 0", inserts)
	}
	if len(s.pendingTradeDiagnostics) != 1 || s.pendingTradeDiagnostics[0].PositionID != "pos-1" {
		t.Fatalf("pending = %+v, want one pos-1 row", s.pendingTradeDiagnostics)
	}
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
	legacy := Trade{Timestamp: now, Symbol: "ETH", Side: "sell", Quantity: 1, Price: 3000, Value: 3000,
		PositionID: "p2", IsClose: true, RealizedPnL: -25}
	if err := sdb.InsertTrade("hl-a", legacy); err != nil {
		t.Fatalf("insert legacy: %v", err)
	}
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

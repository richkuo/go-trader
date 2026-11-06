//go:build pyintegration

package main

import (
	"bufio"
	"bytes"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

var qpQuotes = []float64{0.00001234}

var qpZeroPriceText = regexp.MustCompile(`\$0\.00(?:[^0-9]|$)`)

const (
	qpMarketOpen = "2026-10-21T12:00:00-04:00"
	qpRoundedTo  = 0.12
)

var qpEnvKeys = []string{
	"QP_CLOSE", "QP_QUOTE", "QP_SHAPE", "QP_NOW", "QP_TICKERS", "QP_CLOSES", "QP_CANDLES",
	"QP_OKX_FILL", "QP_LOG", "QP_ALL_CLOSED",
	"QP_DAILY", "QP_OPTION_QUOTE", "QP_OPTION_GREEKS", "QP_OPTION_QUOTE_FAIL_AFTER", "QP_OKX_OPTIONS", "QP_STDOUT_NOISE",
	"OKX_API_KEY", "OKX_API_SECRET", "OKX_PASSPHRASE", "OKX_SANDBOX",
	"ROBINHOOD_USERNAME", "ROBINHOOD_PASSWORD", "ROBINHOOD_TOTP_SECRET",
	"TOPSTEP_API_KEY", "TOPSTEP_API_SECRET", "TOPSTEP_ACCOUNT_ID",
}

func qpRoot(t *testing.T) string {
	t.Helper()
	root := robinhoodStdoutPyRoot(t)
	t.Chdir(root)
	return root
}

func qpEnv(t *testing.T, root string, vars map[string]string) {
	t.Helper()
	t.Setenv("QP_STUB", "1")
	t.Setenv("PYTHONPATH", filepath.Join(root, "shared_scripts", "testdata", "quote_precision"))
	t.Setenv("HOME", t.TempDir())
	for _, k := range qpEnvKeys {
		t.Setenv(k, "")
	}
	keys := make([]string, 0, len(vars))
	for k := range vars {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		t.Setenv(k, vars[k])
	}
}

func qpChildEnv(t *testing.T, root string, vars map[string]string) []string {
	t.Helper()
	env := append(os.Environ(),
		"QP_STUB=1",
		"PYTHONPATH="+filepath.Join(root, "shared_scripts", "testdata", "quote_precision"),
		"HOME="+t.TempDir())
	for _, k := range qpEnvKeys {
		env = append(env, k+"=")
	}
	keys := make([]string, 0, len(vars))
	for k := range vars {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		env = append(env, k+"="+vars[k])
	}
	return env
}

func qpNum(v float64) string { return strconv.FormatFloat(v, 'g', -1, 64) }

func qpDebugLogs(t *testing.T) {
	t.Helper()
	prev := debugLogEnabled.Load()
	debugLogEnabled.Store(true)
	t.Cleanup(func() { debugLogEnabled.Store(prev) })
}

func qpLogger(id string) (*StrategyLogger, *bytes.Buffer) {
	buf := &bytes.Buffer{}
	return &StrategyLogger{stratID: id, writer: buf}, buf
}

func qpClose(a, b float64) bool {
	return math.Abs(a-b) <= 1e-12*math.Max(1, math.Max(math.Abs(a), math.Abs(b)))
}

type qpRunner struct {
	name     string
	sc       StrategyConfig
	symbol   string
	quoteVar bool
	extra    map[string]string
	run      func(sc StrategyConfig, prices map[string]float64, logger *StrategyLogger) (float64, float64, bool)
}

func qpRunSpot(sc StrategyConfig, prices map[string]float64, logger *StrategyLogger) (float64, float64, bool) {
	r, _, price, ok := runSpotCheck(sc, prices, PositionCtx{}, nil, "simple", nil, logger)
	if r == nil {
		return 0, price, ok
	}
	return r.Price, price, ok
}

func qpRunRobinhood(sc StrategyConfig, prices map[string]float64, logger *StrategyLogger) (float64, float64, bool) {
	r, _, price, ok := runRobinhoodCheck(sc, prices, PositionCtx{}, nil, "simple", nil, logger)
	if r == nil {
		return 0, price, ok
	}
	return r.Price, price, ok
}

func qpRunOKX(sc StrategyConfig, prices map[string]float64, logger *StrategyLogger) (float64, float64, bool) {
	r, _, price, ok := runOKXCheck(sc, prices, PositionCtx{}, nil, "simple", nil, logger)
	if r == nil {
		return 0, price, ok
	}
	return r.Price, price, ok
}

func qpRunTopStep(sc StrategyConfig, prices map[string]float64, logger *StrategyLogger) (float64, float64, bool) {
	r, _, price, ok := runTopStepCheck(sc, prices, PositionCtx{}, nil, "simple", nil, logger)
	if r == nil {
		return 0, price, ok
	}
	return r.Price, price, ok
}

func qpSpotSC(id, symbol string) StrategyConfig {
	return StrategyConfig{ID: id, Type: "spot", Platform: "binanceus", Script: "shared_scripts/check_strategy.py",
		Args: []string{"sma_crossover", symbol, "1h", "--mode=paper"}, Capital: 100, MaxDrawdownPct: 50}
}

func qpOKXSC(id, instType, mode string) StrategyConfig {
	typ := "spot"
	if instType == "swap" {
		typ = "perps"
	}
	return StrategyConfig{ID: id, Type: typ, Platform: "okx", Script: "shared_scripts/check_okx.py",
		Args: []string{"sma_crossover", "BTC", "1h", "--mode=" + mode, "--inst-type=" + instType}, Capital: 100,
		MaxDrawdownPct: 50, Direction: DirectionBoth, Leverage: 1, SizingLeverage: 1, AllowNoEdge: qpAllowNoEdge(mode)}
}

func qpAllowNoEdge(mode string) *bool {
	if mode != "live" {
		return nil
	}
	v := true
	return &v
}

func qpRunners() []qpRunner {
	return []qpRunner{
		{name: "spot", sc: qpSpotSC("qp-spot", "BTC/USDT"), symbol: "BTC/USDT", run: qpRunSpot},
		{name: "spot_closed_bar", sc: func() StrategyConfig {
			sc := qpSpotSC("qp-spot-cb", "BTC/USDT")
			sc.ClosedBarDecisions = true
			return sc
		}(),
			symbol: "BTC/USDT", extra: map[string]string{"QP_ALL_CLOSED": "1"}, run: qpRunSpot},
		{name: "robinhood", sc: StrategyConfig{ID: "qp-rh", Type: "spot", Platform: "robinhood", Script: "shared_scripts/check_robinhood.py",
			Args: []string{"sma_crossover", "DOGE", "1h", "--mode=paper"}, Capital: 100}, symbol: "DOGE", quoteVar: true, run: qpRunRobinhood},
		{name: "okx_spot", sc: qpOKXSC("qp-okx-spot", "spot", "paper"), symbol: "BTC", quoteVar: true, run: qpRunOKX},
		{name: "okx_swap", sc: qpOKXSC("qp-okx-swap", "swap", "paper"), symbol: "BTC", quoteVar: true, run: qpRunOKX},
		{name: "okx_swap_closed_bar", sc: func() StrategyConfig {
			sc := qpOKXSC("qp-okx-swap-cb", "swap", "paper")
			sc.ClosedBarDecisions = true
			return sc
		}(),
			symbol: "BTC", quoteVar: true, extra: map[string]string{"QP_ALL_CLOSED": "1"}, run: qpRunOKX},
		{name: "topstep", sc: StrategyConfig{ID: "qp-ts", Type: "futures", Platform: "topstep", Script: "shared_scripts/check_topstep.py",
			Args: []string{"sma_crossover", "MES", "1h", "--mode=paper"}, Capital: 5000}, symbol: "MES", quoteVar: true,
			extra: map[string]string{"QP_NOW": qpMarketOpen}, run: qpRunTopStep},
	}
}

func qpRunnerEnv(t *testing.T, root string, r qpRunner, vars map[string]string) {
	t.Helper()
	merged := map[string]string{}
	for k, v := range r.extra {
		merged[k] = v
	}
	for k, v := range vars {
		merged[k] = v
	}
	qpEnv(t, root, merged)
}

func qpCaseCheckQuotePrecisionRunnersReturnTheFullQuote(t *testing.T) {
	root := qpRoot(t)
	qpDebugLogs(t)
	for _, r := range qpRunners() {
		for _, q := range qpQuotes {
			t.Run(r.name+"/"+qpNum(q), func(t *testing.T) {
				vars := map[string]string{"QP_SHAPE": "cross_up", "QP_CLOSE": qpNum(q)}
				if r.quoteVar {
					vars["QP_CLOSE"] = "0.5"
					vars["QP_QUOTE"] = qpNum(q)
				}
				qpRunnerEnv(t, root, r, vars)
				logger, buf := qpLogger(r.sc.ID)
				resultPrice, price, ok := r.run(r.sc, map[string]float64{}, logger)
				if !ok || resultPrice != q || price != q {
					t.Fatalf("ok=%t result price=%v dispatch price=%v, want %v unchanged; log:\n%s", ok, resultPrice, price, q, buf.String())
				}
				want := "Signal: BUY | " + r.symbol + " @ $" + formatSignalPrice(q)
				if !strings.Contains(buf.String(), want) || qpZeroPriceText.MatchString(buf.String()) {
					t.Fatalf("signal text must show the positive quote %q; log:\n%s", want, buf.String())
				}
			})
		}
	}
}

func qpCaseCheckQuotePrecisionRunnerFallbacksAndFailures(t *testing.T) {
	root := qpRoot(t)
	q := 0.00001234
	runners := []qpRunner{}
	for _, r := range qpRunners() {
		if r.name == "spot" || r.name == "robinhood" {
			runners = append(runners, r)
		}
	}
	for _, r := range runners {
		t.Run(r.name+"/zero_quote_without_fallback_holds", func(t *testing.T) {
			qpRunnerEnv(t, root, r, map[string]string{"QP_CLOSE": "0"})
			logger, buf := qpLogger(r.sc.ID)
			if _, _, ok := r.run(r.sc, map[string]float64{"OTHER/USDT": 5}, logger); ok {
				t.Fatalf("a zero script price with no fallback must hold; log:\n%s", buf.String())
			}
			if !strings.Contains(buf.String(), "No price available for "+r.symbol) {
				t.Fatalf("log lacks the missing-price error:\n%s", buf.String())
			}
		})
		t.Run(r.name+"/zero_quote_uses_the_full_map_fallback", func(t *testing.T) {
			qpRunnerEnv(t, root, r, map[string]string{"QP_CLOSE": "0"})
			logger, buf := qpLogger(r.sc.ID)
			resultPrice, price, ok := r.run(r.sc, map[string]float64{r.symbol: q}, logger)
			if !ok || resultPrice != 0 || price != q {
				t.Fatalf("ok=%t result=%v price=%v, want the map fallback %v; log:\n%s", ok, resultPrice, price, q, buf.String())
			}
		})
		t.Run(r.name+"/script_error_holds_even_with_a_map_price", func(t *testing.T) {
			qpRunnerEnv(t, root, r, map[string]string{"QP_CLOSE": qpNum(q), "QP_QUOTE": qpNum(q)})
			sc := r.sc
			sc.Args = append([]string{"no_such_strategy"}, sc.Args[1:]...)
			logger, buf := qpLogger(sc.ID)
			if _, _, ok := r.run(sc, map[string]float64{r.symbol: q}, logger); ok {
				t.Fatalf("a script error must hold; log:\n%s", buf.String())
			}
		})
	}
	for _, r := range runners {
		if !r.quoteVar {
			continue
		}
		t.Run(r.name+"/failed_quote_falls_back_to_the_full_candle_close", func(t *testing.T) {
			qpRunnerEnv(t, root, r, map[string]string{"QP_CLOSE": qpNum(q), "QP_QUOTE": "error"})
			logger, buf := qpLogger(r.sc.ID)
			resultPrice, price, ok := r.run(r.sc, map[string]float64{}, logger)
			if !ok || resultPrice != q || price != q {
				t.Fatalf("ok=%t result=%v price=%v, want the candle close %v; log:\n%s", ok, resultPrice, price, q, buf.String())
			}
		})
	}
}

func qpCaseCheckQuotePrecisionFetchPricesKeepsTheTickerQuote(t *testing.T) {
	root := qpRoot(t)
	tickers := map[string]interface{}{
		"A/USDT": 0.1234, "B/USDT": 0.006, "C/USDT": 0.00001234, "BTC/USDT": 65000.5,
		"NULL/USDT": nil, "STRING/USDT": "0.12", "NOLAST/USDT": map[string]interface{}{"symbol": "NOLAST/USDT"},
	}
	raw, err := json.Marshal(tickers)
	if err != nil {
		t.Fatal(err)
	}
	qpEnv(t, root, map[string]string{"QP_TICKERS": string(raw)})
	got, err := FetchPrices([]string{"A/USDT", "B/USDT", "BTC/USDT", "C/USDT", "GONE/USDT", "NOLAST/USDT", "NULL/USDT", "STRING/USDT"})
	if err != nil {
		t.Fatalf("FetchPrices: %v", err)
	}
	want := map[string]float64{"A/USDT": 0.1234, "B/USDT": 0.006, "BTC/USDT": 65000.5, "C/USDT": 0.00001234}
	if len(got) != len(want) {
		t.Fatalf("prices = %v, want %v", got, want)
	}
	for k, v := range want {
		if got[k] != v {
			t.Fatalf("prices[%s] = %v, want %v (all: %v)", k, got[k], v, got)
		}
	}
}

type qpScheduler struct {
	t    *testing.T
	root string
	bin  string
	dir  string
	env  []string
}

func qpBuildScheduler(t *testing.T, root string) *qpScheduler {
	t.Helper()
	return &qpScheduler{t: t, root: root, bin: pyintegrationBinary(t)}
}

func (s *qpScheduler) config(strategies []StrategyConfig) string {
	s.t.Helper()
	s.dir = s.t.TempDir()
	cfg := map[string]interface{}{
		"interval_seconds": 3600,
		"db_file":          filepath.Join(s.dir, "state.db"),
		"status_port":      htfITFreePort(s.t),
		"log_dir":          filepath.Join(s.dir, "logs"),
		"log_level":        "debug",
		"strategies":       strategies,
	}
	raw, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		s.t.Fatal(err)
	}
	path := filepath.Join(s.dir, "config.json")
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		s.t.Fatal(err)
	}
	return path
}

var qpStatusLine = regexp.MustCompile(`\[server\] Status endpoint at (http://localhost:\d+/status)`)

func (s *qpScheduler) run(cfgPath, until string, whileRunning func(statusURL string)) string {
	s.t.Helper()
	out, err := qpRunSchedulerProcess(s.bin, s.root, s.env, cfgPath, until, nil, whileRunning)
	if err != nil {
		s.t.Fatal(err)
	}
	return out
}

func qpRunSchedulerProcess(bin, root string, env []string, cfgPath, until string, stop <-chan struct{}, whileRunning func(statusURL string)) (string, error) {
	cmd := exec.Command(bin, "--config", cfgPath)
	cmd.Dir = root
	cmd.Env = env
	pr, pw := io.Pipe()
	cmd.Stdout = pw
	cmd.Stderr = pw
	if err := cmd.Start(); err != nil {
		return "", fmt.Errorf("start scheduler: %v", err)
	}
	lines := make(chan string, 1<<16)
	go func() {
		sc := bufio.NewScanner(pr)
		sc.Buffer(make([]byte, 1<<20), 1<<20)
		for sc.Scan() {
			lines <- sc.Text()
		}
		close(lines)
	}()
	exited := make(chan error, 1)
	go func() {
		err := cmd.Wait()
		pw.Close()
		exited <- err
	}()
	var got []string
	statusURL := ""
	reached := false
	stopped := false
	deadline := time.After(180 * time.Second)
wait:
	for {
		select {
		case line, ok := <-lines:
			if !ok {
				break wait
			}
			got = append(got, line)
			if m := qpStatusLine.FindStringSubmatch(line); m != nil {
				statusURL = m[1]
			}
			if strings.Contains(line, until) {
				reached = true
				break wait
			}
		case <-deadline:
			break wait
		case <-stop:
			stopped = true
			break wait
		}
	}
	if reached && whileRunning != nil {
		whileRunning(statusURL)
	}
	_ = cmd.Process.Signal(syscall.SIGTERM)
	select {
	case <-exited:
	case <-time.After(90 * time.Second):
		_ = cmd.Process.Kill()
		<-exited
	}
	for line := range lines {
		got = append(got, line)
	}
	out := strings.Join(got, "\n")
	if stopped {
		return out, fmt.Errorf("scheduler stopped before it printed %q; output:\n%s", until, out)
	}
	if !reached {
		return out, fmt.Errorf("scheduler never printed %q; output:\n%s", until, out)
	}
	return out, nil
}

type qpTradeRow struct {
	StrategyID string
	Side       string
	Quantity   float64
	Price      float64
	Value      float64
	Details    string
	IsClose    bool
}

func qpTradesFromDB(t *testing.T, db *sql.DB, strategyID string) []qpTradeRow {
	t.Helper()
	query := `SELECT strategy_id, side, quantity, price, value, details, is_close FROM trades`
	var args []interface{}
	if strategyID != "" {
		query += ` WHERE strategy_id = ?`
		args = append(args, strategyID)
	}
	rows, err := db.Query(query+` ORDER BY rowid`, args...)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []qpTradeRow
	for rows.Next() {
		var r qpTradeRow
		var isClose int
		if err := rows.Scan(&r.StrategyID, &r.Side, &r.Quantity, &r.Price, &r.Value, &r.Details, &isClose); err != nil {
			t.Fatal(err)
		}
		r.IsClose = isClose != 0
		out = append(out, r)
	}
	return out
}

func (s *qpScheduler) trades() []qpTradeRow {
	s.t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(s.dir, "state.db"))
	if err != nil {
		s.t.Fatal(err)
	}
	defer db.Close()
	return qpTradesFromDB(s.t, db, "")
}

func qpStatusPrices(t *testing.T, statusURL string) map[string]float64 {
	t.Helper()
	if statusURL == "" {
		t.Fatal("the scheduler printed no status endpoint")
	}
	resp, err := http.Get(statusURL)
	if err != nil {
		t.Fatalf("GET %s: %v", statusURL, err)
	}
	defer resp.Body.Close()
	var body struct {
		Prices map[string]float64 `json:"prices"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode status: %v", err)
	}
	return body.Prices
}

func qpCaseCheckQuotePrecisionSchedulerCycleKeepsTinySpotQuotes(t *testing.T) {
	t.Parallel()
	root := robinhoodStdoutPyRoot(t)
	sched := qpBuildScheduler(t, root)
	q := 0.00001234
	tinyBuy := ApplyAdverseSlippage(q, true)

	t.Run("only_tiny_quotes", func(t *testing.T) {
		sched.env = qpChildEnv(t, root, map[string]string{"QP_TICKERS": `{"DOGE/USDT": 1.234e-05}`, "QP_CLOSES": `{"DOGE/USDT": 1.234e-05}`, "QP_SHAPE": "cross_up"})
		cfg := sched.config([]StrategyConfig{qpSpotSC("qp-doge", "DOGE/USDT")})
		var status map[string]float64
		out := sched.run(cfg, "Cycle 1 complete", func(u string) { status = qpStatusPrices(t, u) })
		if strings.Contains(out, "All prices are zero/missing") || strings.Contains(out, "Skipping zero price") {
			t.Fatalf("a valid tiny quote was filtered out of the cycle:\n%s", out)
		}
		if status["DOGE/USDT"] != q {
			t.Fatalf("status prices = %v, want DOGE/USDT %v", status, q)
		}
		for _, want := range []string{"Prices: DOGE/USDT=$1.234e-05", "Signal: BUY | DOGE/USDT @ $" + formatSignalPrice(q), "@ $" + formatSignalPrice(tinyBuy)} {
			if !strings.Contains(out, want) {
				t.Fatalf("output lacks %q:\n%s", want, out)
			}
		}
		trades := sched.trades()
		if len(trades) != 1 || trades[0].Side != "buy" || !qpClose(trades[0].Price, tinyBuy) || !qpClose(trades[0].Quantity, 100/tinyBuy) {
			t.Fatalf("trades = %+v, want one buy of %v at %v", trades, 100/tinyBuy, tinyBuy)
		}
	})
}

type qpBook struct {
	env *entryTestEnv
	sc  StrategyConfig
}

func qpOpenBook(t *testing.T, sc StrategyConfig) *qpBook {
	t.Helper()
	env := openEntryTestEnv(t, newEntryTestConfig(t, sc))
	useEntryTradeRecorder(t, env.store)
	return &qpBook{env: env, sc: sc}
}

func (b *qpBook) state() *StrategyState { return b.env.strat(b.sc.ID) }

func (b *qpBook) trades() []qpTradeRow {
	return qpTradesFromDB(b.env.t, b.env.fileFor(b.sc.ID).db, b.sc.ID)
}

func qpSpotCheck(t *testing.T, root string, sc StrategyConfig, shape string, q float64) (*SpotResult, string, float64) {
	t.Helper()
	qpEnv(t, root, map[string]string{"QP_SHAPE": shape, "QP_CLOSE": qpNum(q)})
	logger, buf := qpLogger(sc.ID)
	r, sig, price, ok := runSpotCheck(sc, map[string]float64{}, PositionCtx{}, nil, "simple", nil, logger)
	if !ok || price != q {
		t.Fatalf("spot check ok=%t price=%v, want %v; log:\n%s", ok, price, q, buf.String())
	}
	return r, sig, price
}

func qpOKXCheck(t *testing.T, root string, sc StrategyConfig, shape string, q float64, extra map[string]string) (*OKXResult, string, float64) {
	t.Helper()
	vars := map[string]string{"QP_SHAPE": shape, "QP_CLOSE": "0.5", "QP_QUOTE": qpNum(q)}
	for k, v := range extra {
		vars[k] = v
	}
	qpEnv(t, root, vars)
	logger, buf := qpLogger(sc.ID)
	r, sig, price, ok := runOKXCheck(sc, map[string]float64{}, PositionCtx{}, nil, "simple", nil, logger)
	if !ok || price != q {
		t.Fatalf("okx check ok=%t price=%v, want %v; log:\n%s", ok, price, q, buf.String())
	}
	return r, sig, price
}

func qpCaseCheckQuotePrecisionPaperBookingUsesTheFullQuote(t *testing.T) {
	root := qpRoot(t)

	t.Run("spot_open_partial_and_full_close", func(t *testing.T) {
		sc := qpSpotSC("qp-paper-spot", "DOGE/USDT")
		b := qpOpenBook(t, sc)
		s := b.state()
		logger, buf := qpLogger(sc.ID)

		r, sig, price := qpSpotCheck(t, root, sc, "cross_up", 0.1234)
		if n, detail := executeSpotResult(sc, s, b.env.fileFor(sc.ID), r, sig, price, nil, &Config{}, HurstGateDecision{}, logger); n != 1 || !strings.Contains(detail, "@ $0.1234") {
			t.Fatalf("open trades=%d detail=%q", n, detail)
		}
		openPx := ApplyAdverseSlippage(0.1234, true)
		openQty := 100 / openPx
		openFee := CalculatePlatformSpotFee(s.Platform, openQty*openPx)
		pos := s.Positions["DOGE/USDT"]
		if pos == nil || !qpClose(pos.Quantity, openQty) || !qpClose(pos.AvgCost, openPx) || !qpClose(s.Cash, 100-openQty*openPx-openFee) {
			t.Fatalf("open book pos=%+v cash=%v, want %v @ %v cash %v", pos, s.Cash, openQty, openPx, 100-openQty*openPx-openFee)
		}
		if qpClose(openQty, 100/ApplyAdverseSlippage(qpRoundedTo, true)) {
			t.Fatal("the open quantity matches the cent-rounded quote")
		}

		cashBefore := s.Cash
		r, sig, price = qpSpotCheck(t, root, sc, "cross_down", 0.1236)
		r.CloseFraction = 0.5
		if n, _ := executeSpotResult(sc, s, b.env.fileFor(sc.ID), r, sig, price, nil, &Config{}, HurstGateDecision{}, logger); n != 1 {
			t.Fatalf("partial close trades = %d", n)
		}
		partPx := ApplyAdverseSlippage(0.1236, false)
		partQty := openQty * 0.5
		partFee := CalculatePlatformSpotFee(s.Platform, partQty*partPx)
		if pos := s.Positions["DOGE/USDT"]; pos == nil || !qpClose(pos.Quantity, openQty-partQty) || !qpClose(s.Cash, cashBefore+partQty*partPx-partFee) {
			t.Fatalf("partial close pos=%+v cash=%v", pos, s.Cash)
		}

		cashBefore = s.Cash
		r, sig, price = qpSpotCheck(t, root, sc, "cross_down", 0.1238)
		if n, _ := executeSpotResult(sc, s, b.env.fileFor(sc.ID), r, sig, price, nil, &Config{}, HurstGateDecision{}, logger); n != 1 {
			t.Fatalf("full close trades = %d", n)
		}
		closePx := ApplyAdverseSlippage(0.1238, false)
		closeQty := openQty - partQty
		closeFee := CalculatePlatformSpotFee(s.Platform, closeQty*closePx)
		if len(s.Positions) != 0 || !qpClose(s.Cash, cashBefore+closeQty*closePx-closeFee) {
			t.Fatalf("full close positions=%v cash=%v", s.Positions, s.Cash)
		}

		rows := b.trades()
		if len(rows) != 3 || !qpClose(rows[0].Price, openPx) || !qpClose(rows[1].Price, partPx) || !qpClose(rows[2].Price, closePx) ||
			!qpClose(rows[1].Quantity, partQty) || !qpClose(rows[2].Quantity, closeQty) || !rows[2].IsClose {
			t.Fatalf("saved trades = %+v", rows)
		}
		if !strings.Contains(rows[0].Details, "@ $"+formatSignalPrice(openPx)) || strings.Contains(rows[0].Details, "@ $0.12 ") {
			t.Fatalf("open detail %q must carry the full execution price", rows[0].Details)
		}
		log := buf.String()
		for _, want := range []string{"@ $" + formatSignalPrice(openPx), "@ $" + formatSignalPrice(partPx), "@ $" + formatSignalPrice(closePx)} {
			if !strings.Contains(log, want) {
				t.Fatalf("execution log lacks %q:\n%s", want, log)
			}
		}
	})

	t.Run("spot_same_cent_quotes_book_different_sizes", func(t *testing.T) {
		qty := map[float64]float64{}
		for _, q := range []float64{0.1234, 0.1236} {
			sc := qpSpotSC("qp-paper-cent", "DOGE/USDT")
			b := qpOpenBook(t, sc)
			s := b.state()
			r, sig, price := qpSpotCheck(t, root, sc, "cross_up", q)
			executeSpotResult(sc, s, b.env.fileFor(sc.ID), r, sig, price, nil, &Config{}, HurstGateDecision{}, silentStrategyLogger(sc.ID))
			if pos := s.Positions["DOGE/USDT"]; pos != nil {
				qty[q] = pos.Quantity
			}
		}
		if qty[0.1234] == 0 || qty[0.1236] == 0 || qty[0.1234] == qty[0.1236] {
			t.Fatalf("quotes inside one cent booked quantities %v; the full quote must reach sizing", qty)
		}
	})

	t.Run("okx_perps_long_flip_and_close", func(t *testing.T) {
		sc := qpOKXSC("qp-paper-okx", "swap", "paper")
		b := qpOpenBook(t, sc)
		s := b.state()
		logger, buf := qpLogger(sc.ID)

		r, sig, price := qpOKXCheck(t, root, sc, "cross_up", 0.1234, nil)
		if n, detail, _ := executeOKXResult(sc, s, b.env.fileFor(sc.ID), r, nil, sig, price, nil, &Config{}, HurstGateDecision{}, logger); n != 1 || !strings.Contains(detail, "@ $0.1234") {
			t.Fatalf("open trades=%d detail=%q", n, detail)
		}
		longPx := ApplyAdverseSlippage(0.1234, true)
		sizing := PerpsSizingFor(sc, longPx, 0)
		longQty := PerpsOpenNotionalSized(100, longPx, sizing) / longPx
		longFee := CalculatePlatformSpotFee("okx-perps", longQty*longPx)
		if !qpClose(longQty, 100/longPx) {
			t.Fatalf("1x sizing notional drifted: %v vs %v", longQty, 100/longPx)
		}
		pos := s.Positions["BTC"]
		if pos == nil || pos.Side != "long" || !qpClose(pos.Quantity, longQty) || !qpClose(pos.AvgCost, longPx) || !qpClose(s.Cash, 100-longFee) {
			t.Fatalf("long book pos=%+v cash=%v", pos, s.Cash)
		}

		cashBefore := s.Cash
		r, sig, price = qpOKXCheck(t, root, sc, "cross_down", 0.1236, nil)
		if n, _, _ := executeOKXResult(sc, s, b.env.fileFor(sc.ID), r, nil, sig, price, nil, &Config{}, HurstGateDecision{}, logger); n != 2 {
			t.Fatalf("flip trades = %d, want close + open", n)
		}
		flipPx := ApplyAdverseSlippage(0.1236, false)
		closeFee := CalculatePlatformSpotFee("okx-perps", longQty*flipPx)
		cashAfterClose := cashBefore + longQty*(flipPx-longPx) - closeFee
		shortQty := PerpsOpenNotionalSized(cashAfterClose, flipPx, sizing) / flipPx
		shortFee := CalculatePlatformSpotFee("okx-perps", shortQty*flipPx)
		pos = s.Positions["BTC"]
		if pos == nil || pos.Side != "short" || !qpClose(pos.Quantity, shortQty) || !qpClose(pos.AvgCost, flipPx) || !qpClose(s.Cash, cashAfterClose-shortFee) {
			t.Fatalf("flipped book pos=%+v cash=%v, want short %v @ %v cash %v", pos, s.Cash, shortQty, flipPx, cashAfterClose-shortFee)
		}

		cashBefore = s.Cash
		r, sig, price = qpOKXCheck(t, root, sc, "cross_up", 0.1232, nil)
		if n, _, _ := executeOKXResult(sc, s, b.env.fileFor(sc.ID), r, nil, sig, price, nil, &Config{}, HurstGateDecision{}, logger); n < 1 {
			t.Fatalf("cover trades = %d", n)
		}
		coverPx := ApplyAdverseSlippage(0.1232, true)
		coverFee := CalculatePlatformSpotFee("okx-perps", shortQty*coverPx)
		rows := b.trades()
		if len(rows) < 4 || !qpClose(rows[0].Price, longPx) || !qpClose(rows[1].Price, flipPx) || !qpClose(rows[2].Price, flipPx) || !qpClose(rows[3].Price, coverPx) {
			t.Fatalf("saved trades = %+v", rows)
		}
		if !qpClose(rows[3].Quantity, shortQty) || !rows[3].IsClose {
			t.Fatalf("cover trade = %+v, want %v closed", rows[3], shortQty)
		}
		wantCash := cashBefore + shortQty*(flipPx-coverPx) - coverFee
		if len(rows) == 4 && !qpClose(s.Cash, wantCash) {
			t.Fatalf("cash after cover = %v, want %v", s.Cash, wantCash)
		}
		for _, want := range []string{"Open long", "@ $" + formatSignalPrice(longPx), "@ $" + formatSignalPrice(flipPx), "@ $" + formatSignalPrice(coverPx)} {
			if !strings.Contains(buf.String()+rows[0].Details+rows[2].Details, want) {
				t.Fatalf("execution text lacks %q:\n%s", want, buf.String())
			}
		}
	})
}

type qpOrder struct {
	Call   string                 `json:"call"`
	Symbol string                 `json:"symbol"`
	Side   string                 `json:"side"`
	Amount float64                `json:"amount"`
	Params map[string]interface{} `json:"params"`
}

func qpOrders(t *testing.T, path string) []qpOrder {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	var out []qpOrder
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		if line == "" {
			continue
		}
		var o qpOrder
		if err := json.Unmarshal([]byte(line), &o); err != nil {
			t.Fatalf("venue log line %q: %v", line, err)
		}
		if o.Call == "create_market_order" {
			out = append(out, o)
		}
	}
	return out
}

func qpOKXLiveEnv(t *testing.T, root, log string, fill map[string]interface{}, shape string, q float64) {
	t.Helper()
	raw, err := json.Marshal(fill)
	if err != nil {
		t.Fatal(err)
	}
	qpEnv(t, root, map[string]string{
		"QP_SHAPE": shape, "QP_CLOSE": "0.5", "QP_QUOTE": qpNum(q), "QP_LOG": log, "QP_OKX_FILL": string(raw),
		"OKX_API_KEY": "qp-key", "OKX_API_SECRET": "qp-secret", "OKX_PASSPHRASE": "qp-pass",
	})
}

func qpCaseCheckQuotePrecisionOKXLiveSizingAndFillPrecedence(t *testing.T) {
	root := qpRoot(t)

	t.Run("spot_buy_sizes_from_the_full_quote_and_books_the_fill", func(t *testing.T) {
		q := 0.1234
		log := filepath.Join(t.TempDir(), "venue.jsonl")
		qpOKXLiveEnv(t, root, log, map[string]interface{}{"average": 0.1231, "fee": 0.02}, "cross_up", q)
		sc := qpOKXSC("qp-live-okx-spot", "spot", "live")
		b := qpOpenBook(t, sc)
		s := b.state()
		logger, buf := qpLogger(sc.ID)
		r, sig, price, ok := runOKXCheck(sc, map[string]float64{}, PositionCtx{}, nil, "simple", nil, logger)
		if !ok || price != q || r.Signal != 1 {
			t.Fatalf("live check ok=%t price=%v signal=%v; log:\n%s", ok, price, r, buf.String())
		}
		er, ok := runOKXExecuteOrder(sc, r, price, 100, true, false, 0, "", 0, 0, HurstGateDecision{}, nil, logger)
		if !ok {
			t.Fatalf("execute failed; log:\n%s", buf.String())
		}
		orders := qpOrders(t, log)
		if len(orders) != 1 || orders[0].Side != "buy" || orders[0].Symbol != "BTC/USDT" || orders[0].Amount != 100/q {
			t.Fatalf("orders = %+v, want one spot buy of %v", orders, 100/q)
		}
		if orders[0].Amount == 100/qpRoundedTo {
			t.Fatal("the spot order was sized from the cent-rounded quote")
		}
		n, detail, _ := executeOKXResult(sc, s, b.env.fileFor(sc.ID), r, er, sig, price, nil, &Config{}, HurstGateDecision{}, logger)
		pos := s.Positions["BTC"]
		if n != 1 || pos == nil || pos.Quantity != 100/q || pos.AvgCost != 0.1231 || !strings.Contains(detail, "LIVE BUY BTC @ $0.1231") {
			t.Fatalf("booked n=%d detail=%q pos=%+v, want the venue fill", n, detail, pos)
		}
		if !qpClose(s.Cash, 100-(100/q)*0.1231-0.02) {
			t.Fatalf("cash = %v", s.Cash)
		}
		if !strings.Contains(buf.String(), "Live fill at $0.1231 qty=") || !strings.Contains(buf.String(), "(mid was $0.1234)") {
			t.Fatalf("fill line must show both full prices:\n%s", buf.String())
		}

		log2 := filepath.Join(t.TempDir(), "venue.jsonl")
		qpOKXLiveEnv(t, root, log2, map[string]interface{}{"average": 0.1239, "fee": 0.02}, "cross_down", 0.1240)
		r, _, price, ok = runOKXCheck(sc, map[string]float64{}, PositionCtx{}, nil, "simple", nil, logger)
		if !ok || r.Signal != -1 {
			t.Fatalf("sell check ok=%t result=%+v", ok, r)
		}
		if _, ok := runOKXExecuteOrder(sc, r, price, s.Cash, true, false, pos.Quantity, "long", pos.AvgCost, 0, HurstGateDecision{}, nil, logger); !ok {
			t.Fatalf("sell failed:\n%s", buf.String())
		}
		if sells := qpOrders(t, log2); len(sells) != 1 || sells[0].Side != "sell" || sells[0].Amount != pos.Quantity {
			t.Fatalf("sell orders = %+v, want the book quantity %v regardless of price", sells, pos.Quantity)
		}
	})

	t.Run("perps_open_and_flip_size_from_the_full_quote", func(t *testing.T) {
		q := 0.1234
		log := filepath.Join(t.TempDir(), "venue.jsonl")
		qpOKXLiveEnv(t, root, log, map[string]interface{}{"average": 0.1235, "fee": 0.03}, "cross_up", q)
		sc := qpOKXSC("qp-live-okx-swap", "swap", "live")
		b := qpOpenBook(t, sc)
		s := b.state()
		logger, buf := qpLogger(sc.ID)
		r, sig, price, ok := runOKXCheck(sc, map[string]float64{}, PositionCtx{}, nil, "simple", nil, logger)
		if !ok || price != q {
			t.Fatalf("live check ok=%t price=%v; log:\n%s", ok, price, buf.String())
		}
		er, ok := runOKXExecuteOrder(sc, r, price, 100, true, false, 0, "", 0, 0, HurstGateDecision{}, nil, logger)
		if !ok {
			t.Fatalf("execute failed; log:\n%s", buf.String())
		}
		wantOpen := PerpsOpenNotionalSized(100, q, PerpsSizingFor(sc, q, 0)) / q
		orders := qpOrders(t, log)
		if len(orders) != 1 || orders[0].Symbol != "BTC/USDT:USDT" || orders[0].Amount != wantOpen || !qpClose(wantOpen, 100/q) {
			t.Fatalf("orders = %+v, want one swap buy of %v", orders, wantOpen)
		}
		executeOKXResult(sc, s, b.env.fileFor(sc.ID), r, er, sig, price, nil, &Config{}, HurstGateDecision{}, logger)
		pos := s.Positions["BTC"]
		if pos == nil || pos.Quantity != wantOpen || pos.AvgCost != 0.1235 {
			t.Fatalf("booked pos=%+v, want %v @ 0.1235 from the venue fill", pos, wantOpen)
		}

		q2 := 0.1238
		log2 := filepath.Join(t.TempDir(), "venue.jsonl")
		qpOKXLiveEnv(t, root, log2, map[string]interface{}{"average": 0.1237, "fee": 0.03}, "cross_down", q2)
		r, _, price, ok = runOKXCheck(sc, map[string]float64{}, PositionCtx{}, nil, "simple", nil, logger)
		if !ok || price != q2 || r.Signal != -1 {
			t.Fatalf("flip check ok=%t price=%v result=%+v", ok, price, r)
		}
		if _, ok := runOKXExecuteOrder(sc, r, price, s.Cash, true, false, pos.Quantity, "long", pos.AvgCost, 1, HurstGateDecision{}, nil, logger); !ok {
			t.Fatalf("flip failed:\n%s", buf.String())
		}
		effective := s.Cash + pos.Quantity*(q2-pos.AvgCost)
		wantFlip := pos.Quantity + PerpsOpenNotionalSized(effective, q2, PerpsSizingFor(sc, q2, 0))/q2
		if flips := qpOrders(t, log2); len(flips) != 1 || flips[0].Side != "sell" || !qpClose(flips[0].Amount, wantFlip) {
			t.Fatalf("flip orders = %+v, want one sell of %v", flips, wantFlip)
		}
	})

	t.Run("guarded_entry_books_only_a_confirmed_fill", func(t *testing.T) {
		q := 0.00001234
		sc := qpOKXSC("qp-live-okx-guarded", "swap", "live")
		sc.ClosedBarDecisions = true
		sc.Capital = 1000

		clk := bindEntryTestClockFrom(t, time.Now().UTC().Truncate(time.Hour).Add(time.Hour), 0)
		log := filepath.Join(t.TempDir(), "venue.jsonl")
		qpOKXLiveEnv(t, root, log, map[string]interface{}{"average": 0.00001236, "fee": 0.01}, "cross_up", q)
		env := openEntryTestEnv(t, newEntryTestConfig(t, sc))
		useEntryTradeRecorder(t, env.store)
		at := clk.now()
		bar := entryTestNewestBar(at)
		env.dispatchOKX(sc, 1, q, entryTestDecision(bar, at), at)
		orders := qpOrders(t, log)
		if len(orders) != 1 || orders[0].Params["clOrdId"] == nil || !qpClose(orders[0].Amount, 1000/q) {
			t.Fatalf("guarded orders = %+v, want one tagged buy of %v", orders, 1000/q)
		}
		if qty, side := env.position(sc.ID, "BTC"); qty != orders[0].Amount || side != "long" {
			t.Fatalf("confirmed fill booked %v %q, want %v long", qty, side, orders[0].Amount)
		}
		if pos := env.strat(sc.ID).Positions["BTC"]; pos.AvgCost != 0.00001236 {
			t.Fatalf("booked avg cost %v, want the venue fill 0.00001236", pos.AvgCost)
		}
		if got := env.intentStatuses(sc.ID); len(got) != 1 || got[0] != entryIntentCommitted {
			t.Fatalf("intents %v", got)
		}

		sc2 := sc
		sc2.ID = "qp-live-okx-unresolved"
		log2 := filepath.Join(t.TempDir(), "venue.jsonl")
		qpOKXLiveEnv(t, root, log2, map[string]interface{}{"average": nil, "filled": 0, "status": "open", "fee": 0}, "cross_up", q)
		env2 := openEntryTestEnv(t, newEntryTestConfig(t, sc2))
		useEntryTradeRecorder(t, env2.store)
		env2.dispatchOKX(sc2, 1, q, entryTestDecision(bar, at), at)
		if len(qpOrders(t, log2)) != 1 {
			t.Fatalf("unresolved entry orders = %+v", qpOrders(t, log2))
		}
		if qty, _ := env2.position(sc2.ID, "BTC"); qty != 0 || len(env2.strat(sc2.ID).TradeHistory) != 0 {
			t.Fatalf("an unresolved entry booked %v", qty)
		}
		if got := env2.intentStatuses(sc2.ID); len(got) != 1 || got[0] != entryIntentSubmitting {
			t.Fatalf("unresolved intents %v, want one submitting", got)
		}
	})
}

func qpCaseCheckQuotePrecisionRobinhoodLiveKeepsTheVenueFill(t *testing.T) {
	q := 0.00001234

	t.Run("confirmed_buy_books_the_venue_price", func(t *testing.T) {
		e := newRHLiveEnv(t, rhLiveSpot("rh-btc", "BTC", 1000))
		e.setRoutes(rhWith(rhPrepRoutes(), rhRoutes{
			"POST " + rhTestOrders:                          {rhAccepted("buy")},
			"GET " + rhOrderURL(rhTestOrderID):              {rhOrder("queued", "buy", "0", nil), rhOrder("filled", "buy", "9.5", "100.25")},
			"POST " + rhOrderURL(rhTestOrderID) + "cancel/": {{"status": 200, "json": map[string]interface{}{}}},
		}))
		trades, detail := e.signal("rh-btc", 1, 0, q)
		if trades != 1 || !strings.Contains(detail, "@ $100.25") {
			t.Fatalf("trades=%d detail=%q, want one live buy at the venue price", trades, detail)
		}
		s := e.state.Strategies["rh-btc"]
		if pos := s.Positions["BTC"]; pos == nil || pos.Quantity != 9.5 || pos.AvgCost != 100.25 || math.Abs(s.Cash-(1000-9.5*100.25)) > 1e-6 {
			t.Fatalf("pos=%+v cash=%v, want 9.5 @ 100.25", pos, s.Cash)
		}
		if dbt := e.dbTrades("rh-btc"); len(dbt) != 1 || dbt[0].Price != 100.25 || dbt[0].Quantity != 9.5 {
			t.Fatalf("db trades = %+v", dbt)
		}
		if row := e.onlyAttempt(); row.Status != robinhoodAttemptSettled || row.SettlementSeq != 1 {
			t.Fatalf("attempt = %+v", row)
		}
	})
}

func qpCaseCheckQuotePrecisionOptionsSpotAndPremiumPrecedence(t *testing.T) {
	root := qpRoot(t)
	sc := StrategyConfig{ID: "qp-opt", Type: "options", Platform: "deribit", Script: "shared_scripts/check_options.py",
		Args: []string{"vol_mean_reversion", "BTC", "--platform=deribit"}, Capital: 1000}
	for _, q := range qpQuotes {
		t.Run(qpNum(q), func(t *testing.T) {
			qpDebugLogs(t)
			qpEnv(t, root, map[string]string{"QP_QUOTE": qpNum(q), "QP_CLOSE": qpNum(q)})
			logger, buf := qpLogger(sc.ID)
			result, _, ok := runOptionsCheck(sc, "[]", nil, logger)
			if !ok || result.SpotPrice != q {
				t.Fatalf("options check ok=%t spot=%v, want %v; log:\n%s", ok, result, q, buf.String())
			}
			if !strings.Contains(buf.String(), "BTC spot=$"+formatSignalPrice(q)) {
				t.Fatalf("signal line must show the positive spot price:\n%s", buf.String())
			}
			s := NewStrategyState(sc)
			result.Signal = 1
			if result.StructureContract != optionsStructureContractV1 {
				t.Fatalf("structure_contract = %q, want %q", result.StructureContract, optionsStructureContractV1)
			}
			result.Actions = []OptionsAction{
				{Action: "buy", OptionType: "call", Strike: 1, Expiry: "2026-12-25", Premium: 0.05, Quantity: 2, StructureID: "qp-1", StructureLegs: 3},
				{Action: "sell", OptionType: "call", Strike: 2, Expiry: "2026-12-25", Premium: 0.03, PremiumUSD: -1, Quantity: 1, StructureID: "qp-1", StructureLegs: 3},
				{Action: "buy", OptionType: "put", Strike: 3, Expiry: "2026-12-25", Premium: 0.05, PremiumUSD: 3.5, Quantity: 1, StructureID: "qp-1", StructureLegs: 3},
			}
			n, err := ExecuteOptionsSignal(s, result, logger)
			if err != nil || n != 3 {
				t.Fatalf("options trades=%d err=%v; log:\n%s", n, err, buf.String())
			}
			buyCost := 0.05 * q * 2
			sellPremium := 0.03 * q
			fixedCost := 3.5
			buyFee := CalculateOptionFee(s.Platform, buyCost, 1)
			sellFee := CalculateOptionFee(s.Platform, sellPremium, 1)
			fixedFee := CalculateOptionFee(s.Platform, fixedCost, 1)
			wantCash := 1000 - (buyCost + buyFee) + (sellPremium - sellFee) - (fixedCost + fixedFee)
			if !qpClose(s.Cash, wantCash) {
				t.Fatalf("cash = %v, want %v", s.Cash, wantCash)
			}
			if len(s.TradeHistory) != 3 || !qpClose(s.TradeHistory[0].Price, buyCost) || !qpClose(s.TradeHistory[1].Price, sellPremium) || s.TradeHistory[2].Price != fixedCost {
				t.Fatalf("trades = %+v", s.TradeHistory)
			}
			if len(s.OptionPositions) != 3 {
				t.Fatalf("option positions = %v", s.OptionPositions)
			}
		})
	}
}

func qpCaseCheckQuotePrecisionPriceTextKeepsLargePrices(t *testing.T) {
	root := qpRoot(t)
	qpDebugLogs(t)
	cases := []struct {
		r    qpRunner
		q    float64
		text string
	}{
		{qpRunners()[0], 65000.5, "65000.50"},
	}
	for _, tc := range cases {
		t.Run(tc.r.name, func(t *testing.T) {
			vars := map[string]string{"QP_SHAPE": "cross_up", "QP_CLOSE": qpNum(tc.q)}
			if tc.r.quoteVar {
				vars["QP_QUOTE"] = qpNum(tc.q)
			}
			qpRunnerEnv(t, root, tc.r, vars)
			logger, buf := qpLogger(tc.r.sc.ID)
			_, price, ok := tc.r.run(tc.r.sc, map[string]float64{}, logger)
			if !ok || price != tc.q {
				t.Fatalf("ok=%t price=%v; log:\n%s", ok, price, buf.String())
			}
			if want := fmt.Sprintf("Signal: BUY | %s @ $%s", tc.r.symbol, tc.text); !strings.Contains(buf.String(), want) {
				t.Fatalf("log lacks %q:\n%s", want, buf.String())
			}
		})
	}
	t.Run("spot_booking_text", func(t *testing.T) {
		sc := qpSpotSC("qp-text-spot", "BTC/USDT")
		b := qpOpenBook(t, sc)
		s := b.state()
		logger, buf := qpLogger(sc.ID)
		r, sig, price := qpSpotCheck(t, root, sc, "cross_up", 65000.5)
		executeSpotResult(sc, s, b.env.fileFor(sc.ID), r, sig, price, nil, &Config{}, HurstGateDecision{}, logger)
		px := ApplyAdverseSlippage(65000.5, true)
		qty := 100 / px
		fee := CalculatePlatformSpotFee(s.Platform, qty*px)
		wantDetail := fmt.Sprintf("Open long %.6f @ $%.2f (fee $%.2f)", qty, px, fee)
		wantLog := fmt.Sprintf("BUY BTC/USDT: %.6f @ $%.2f (fee $%.2f, total $%.2f)", qty, px, fee, qty*px+fee)
		rows := b.trades()
		if len(rows) != 1 || rows[0].Details != wantDetail || !strings.Contains(buf.String(), wantLog) {
			t.Fatalf("detail=%+v want %q; log lacks %q:\n%s", rows, wantDetail, wantLog, buf.String())
		}
	})
}

type qpRiskRow struct {
	Cash           float64
	Peak           float64
	CircuitBreaker bool
	Quantity       float64
}

func qpRiskRows(t *testing.T, db *sql.DB) map[string]qpRiskRow {
	t.Helper()
	rows, err := db.Query(`SELECT s.id, s.cash, s.risk_peak_value, s.risk_circuit_breaker, COALESCE(SUM(p.quantity), 0)
		FROM strategies s LEFT JOIN positions p ON p.strategy_id = s.id GROUP BY s.id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	out := map[string]qpRiskRow{}
	for rows.Next() {
		var id string
		var r qpRiskRow
		var cb int
		if err := rows.Scan(&id, &r.Cash, &r.Peak, &cb, &r.Quantity); err != nil {
			t.Fatal(err)
		}
		r.CircuitBreaker = cb != 0
		out[id] = r
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

func qpPaperRisk(t *testing.T, db *sql.DB) (peak float64, marker int, active int) {
	t.Helper()
	if err := db.QueryRow(`SELECT peak_value, quote_precision_rebaselined, kill_switch_active FROM portfolio_risk WHERE scope = 'paper'`).Scan(&peak, &marker, &active); err != nil {
		t.Fatal(err)
	}
	return peak, marker, active
}

func qpLogLines(t *testing.T, out, needle string) {
	t.Helper()
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, needle) {
			t.Log(strings.TrimSpace(line))
		}
	}
}

func qpExec(t *testing.T, db *sql.DB, query string, args ...interface{}) {
	t.Helper()
	if _, err := db.Exec(query, args...); err != nil {
		t.Fatalf("%s: %v", query, err)
	}
}

func TestCheckQuotePrecisionUpgradeRebaselinesRoundedPeaksOnce(t *testing.T) {
	t.Parallel()
	root := robinhoodStdoutPyRoot(t)
	sched := qpBuildScheduler(t, root)
	const q = 0.0055
	const legacyMark = 0.01
	tickers := `{"DOGE/USDT": 0.0055, "SHIB/USDT": 0.0055, "XRP/USDT": 0.0055}`
	sched.env = qpChildEnv(t, root, map[string]string{"QP_TICKERS": tickers, "QP_CLOSES": tickers, "QP_SHAPE": "cross_up"})
	tight := func(sc StrategyConfig) StrategyConfig {
		sc.MaxDrawdownPct = 20
		return sc
	}
	cfg := sched.config([]StrategyConfig{
		tight(qpSpotSC("qp-drawdown", "DOGE/USDT")),
		tight(qpSpotSC("qp-at-peak", "SHIB/USDT")),
		tight(qpSpotSC("qp-flat", "XRP/USDT")),
	})
	label := partitionLabel(defaultPaperPartition)
	marker := "#1820 quote-precision peak rebaseline"

	out := sched.run(cfg, "Cycle 1 complete", nil)
	if strings.Contains(out, marker+" ["+label+"] qp-") || strings.Contains(out, "] portfolio: peak") {
		t.Fatalf("a store with no open position must not rebaseline:\n%s", out)
	}
	db, err := sql.Open("sqlite", filepath.Join(sched.dir, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, m, _ := qpPaperRisk(t, db); m != 1 {
		t.Fatalf("the first cycle on a fresh store must set the one-time marker, got %d", m)
	}
	opened := qpRiskRows(t, db)
	for _, id := range []string{"qp-drawdown", "qp-at-peak", "qp-flat"} {
		if opened[id].Quantity <= 0 {
			t.Fatalf("cycle 1 opened no position for %s: %+v", id, opened)
		}
	}

	qpExec(t, db, `ALTER TABLE portfolio_risk DROP COLUMN quote_precision_rebaselined`)
	qpExec(t, db, `ALTER TABLE strategies DROP COLUMN quote_precision_rebaselined`)
	qpExec(t, db, `DELETE FROM positions WHERE strategy_id = 'qp-flat'`)
	qpExec(t, db, `UPDATE strategies SET cash = 100, risk_peak_value = 101 WHERE id = 'qp-flat'`)
	qpExec(t, db, `UPDATE strategies SET cash = 0 WHERE id IN ('qp-drawdown', 'qp-at-peak')`)
	dd, ap := opened["qp-drawdown"], opened["qp-at-peak"]
	dd.Cash, ap.Cash = 0, 0
	legacyDD := dd.Cash + dd.Quantity*legacyMark
	legacyAP := ap.Cash + ap.Quantity*legacyMark
	peakDD := legacyDD + 10
	peakAP := legacyAP
	peakPart := legacyDD + legacyAP + 100 + 10
	qpExec(t, db, `UPDATE strategies SET risk_peak_value = ? WHERE id = 'qp-drawdown'`, peakDD)
	qpExec(t, db, `UPDATE strategies SET risk_peak_value = ? WHERE id = 'qp-at-peak'`, peakAP)
	qpExec(t, db, `UPDATE portfolio_risk SET peak_value = ? WHERE scope = 'paper'`, peakPart)

	trueDD := dd.Cash + dd.Quantity*q
	trueAP := ap.Cash + ap.Quantity*q
	boundDD := dd.Quantity * quotePrecisionRoundingPerUnit
	boundAP := ap.Quantity * quotePrecisionRoundingPerUnit
	trueTotal := 0.0
	trueTotal += trueDD
	trueTotal += trueAP
	trueTotal += 100.0
	wantDD := math.Max(trueDD, peakDD-boundDD)
	wantAP := math.Max(trueAP, peakAP-boundAP)
	wantPart := math.Max(trueTotal, peakPart-(boundDD+boundAP))
	if wantDD != peakDD-boundDD || wantAP != trueAP || wantPart != trueTotal {
		t.Fatalf("fixture must exercise both branches: dd bound %v value %v, at-peak bound %v value %v, partition bound %v value %v",
			peakDD-boundDD, trueDD, peakAP-boundAP, trueAP, peakPart-(boundDD+boundAP), trueTotal)
	}
	if (peakDD-trueDD)/peakDD*100 <= 20 || (peakPart-trueTotal)/peakPart*100 <= 25 {
		t.Fatalf("fixture must trip both guards without the rebaseline: strategy %.2f%%, portfolio %.2f%%",
			(peakDD-trueDD)/peakDD*100, (peakPart-trueTotal)/peakPart*100)
	}

	t.Run("upgrade_cycle_lowers_only_the_rounding_artifact", func(t *testing.T) {
		out := sched.run(cfg, "Cycle 2 complete", nil)
		for _, want := range []string{
			fmt.Sprintf("[WARN] %s [%s] qp-drawdown: peak $%.4f -> $%.4f (value $%.4f, rounding bound $%.4f)", marker, label, peakDD, wantDD, trueDD, boundDD),
			fmt.Sprintf("[WARN] %s [%s] qp-at-peak: peak $%.4f -> $%.4f (value $%.4f, rounding bound $%.4f)", marker, label, peakAP, wantAP, trueAP, boundAP),
			fmt.Sprintf("[WARN] %s [%s] portfolio: peak $%.4f -> $%.4f (value $%.4f, rounding bound $%.4f)", marker, label, peakPart, wantPart, trueTotal, boundDD+boundAP),
		} {
			if !strings.Contains(out, want) {
				t.Fatalf("output lacks %q:\n%s", want, out)
			}
		}
		qpLogLines(t, out, marker)
		if strings.Contains(out, marker+" ["+label+"] qp-flat") || strings.Contains(out, "max drawdown") || strings.Contains(out, "Portfolio kill switch") {
			t.Fatalf("the upgrade cycle touched the flat strategy or tripped a guard:\n%s", out)
		}
		got := qpRiskRows(t, db)
		if !qpClose(got["qp-drawdown"].Peak, wantDD) || !qpClose(got["qp-at-peak"].Peak, wantAP) || got["qp-flat"].Peak != 101 {
			t.Fatalf("peaks = %+v, want drawdown %v at-peak %v flat 101", got, wantDD, wantAP)
		}
		for _, id := range []string{"qp-drawdown", "qp-at-peak"} {
			if got[id].CircuitBreaker || !qpClose(got[id].Quantity, opened[id].Quantity) {
				t.Fatalf("%s must keep its position with no breaker: %+v", id, got[id])
			}
		}
		peak, m, active := qpPaperRisk(t, db)
		if !qpClose(peak, wantPart) || m != 1 || active != 0 {
			t.Fatalf("portfolio peak %v marker %d kill switch %d, want %v, 1, 0", peak, m, active, wantPart)
		}
		var events int
		if err := db.QueryRow(`SELECT COUNT(*) FROM kill_switch_events WHERE scope = 'paper' AND type = 'basis_rebaseline' AND details LIKE '#1820 %'`).Scan(&events); err != nil || events != 1 {
			t.Fatalf("basis_rebaseline events = %d (err %v), want 1", events, err)
		}
	})

	t.Run("marker_blocks_a_second_rebaseline", func(t *testing.T) {
		qpExec(t, db, `UPDATE strategies SET risk_peak_value = ? WHERE id = 'qp-drawdown'`, peakDD)
		out := sched.run(cfg, "Cycle 3 complete", nil)
		if strings.Contains(out, marker) {
			t.Fatalf("the rebaseline ran a second time:\n%s", out)
		}
		if !strings.Contains(out, "max drawdown") {
			t.Fatalf("the restored rounded peak must trip the breaker once the one-time rebaseline is spent:\n%s", out)
		}
		qpLogLines(t, out, "max drawdown")
		got := qpRiskRows(t, db)
		if !got["qp-drawdown"].CircuitBreaker || !qpClose(got["qp-at-peak"].Peak, wantAP) {
			t.Fatalf("rows = %+v, want the drawdown breaker latched and the at-peak peak %v unchanged", got, wantAP)
		}
		var events int
		if err := db.QueryRow(`SELECT COUNT(*) FROM kill_switch_events WHERE scope = 'paper' AND type = 'basis_rebaseline' AND details LIKE '#1820 %'`).Scan(&events); err != nil || events != 1 {
			t.Fatalf("basis_rebaseline events = %d (err %v), want 1", events, err)
		}
	})
}

func TestCheckQuotePrecisionRebaselineSurvivesABookSaveBeforeSaveAll(t *testing.T) {
	const q = 0.0055
	const qty = 20000.0
	sc := StrategyConfig{ID: "qp-rh-live", Type: "spot", Platform: "robinhood", Script: "shared_scripts/check_robinhood.py",
		Args: []string{"sma_crossover", "DOGE", "1h", "--mode=live"}, Capital: 100, MaxDrawdownPct: 20}
	flat := StrategyConfig{ID: "qp-rh-flat", Type: "spot", Platform: "robinhood", Script: "shared_scripts/check_robinhood.py",
		Args: []string{"sma_crossover", "SHIB", "1h", "--mode=live"}, Capital: 100, MaxDrawdownPct: 20}
	part := partitionFor(sc)
	prices := map[string]float64{"DOGE": q, "SHIB": q}
	legacyPeak := qty*0.01 + 20
	legacyPart := legacyPeak + 101
	bound := qty * quotePrecisionRoundingPerUnit
	value := qty * q
	total := value + 100
	wantPeak := math.Max(value, legacyPeak-bound)
	wantPart := math.Max(total, legacyPart-bound)
	if wantPeak != legacyPeak-bound || wantPeak == value || wantPart != legacyPart-bound || wantPart == total {
		t.Fatalf("fixture must take the bound branch: peak %v part %v", wantPeak, wantPart)
	}

	legacyStore := func(t *testing.T) *entryTestEnv {
		env := openEntryTestEnv(t, newEntryTestConfig(t, sc, flat))
		s := env.strat(sc.ID)
		s.Cash = 0
		s.Positions["DOGE"] = &Position{Symbol: "DOGE", Quantity: qty, InitialQuantity: qty, AvgCost: q, Side: "long"}
		s.RiskState.PeakValue = legacyPeak
		env.strat(flat.ID).Cash = 100
		env.strat(flat.ID).RiskState.PeakValue = 101
		env.state.partitionRisk(part).PeakValue = legacyPart
		env.saveAll()
		return env
	}
	pass := func(env *entryTestEnv) []string {
		env.mu.Lock()
		defer env.mu.Unlock()
		lines := applyQuotePrecisionPeakRebaseline(part, env.state.partitionRisk(part), env.cfg.Strategies, env.state, prices, total)
		env.t.Log(strings.Join(lines, "\n"))
		return lines
	}
	singlePass := func(t *testing.T, env *entryTestEnv, save func(*StrategyState) error) *entryTestEnv {
		pass(env)
		if got := env.strat(sc.ID).RiskState.PeakValue; got != wantPeak {
			t.Fatalf("upgrade pass peak %v, want %v", got, wantPeak)
		}
		if err := save(env.strat(sc.ID)); err != nil {
			t.Fatalf("book save: %v", err)
		}
		env = env.restart()
		if got := env.strat(sc.ID); got.RiskState.PeakValue != wantPeak || !got.RiskState.QuotePrecisionRebaselined {
			t.Fatalf("after the book save and restart: peak %v marker %t, want %v true", got.RiskState.PeakValue, got.RiskState.QuotePrecisionRebaselined, wantPeak)
		}
		if prs := env.state.partitionRisk(part); prs.PeakValue != legacyPart || prs.QuotePrecisionRebaselined {
			t.Fatalf("the book save must not move the partition row: peak %v marker %t", prs.PeakValue, prs.QuotePrecisionRebaselined)
		}
		lines := pass(env)
		if got := env.strat(sc.ID).RiskState.PeakValue; got != wantPeak {
			t.Fatalf("pass after restart: strategy peak %v, want the single-pass %v", got, wantPeak)
		}
		if got := env.state.partitionRisk(part).PeakValue; got != wantPart {
			t.Fatalf("pass after restart: partition peak %v, want the single-pass %v", got, wantPart)
		}
		if got := env.strat(flat.ID).RiskState.PeakValue; got != 101 {
			t.Fatalf("flat strategy peak %v, want 101", got)
		}
		for _, line := range lines {
			if strings.Contains(line, sc.ID) {
				t.Fatalf("the strategy took a second pass: %s", line)
			}
		}
		return env
	}

	t.Run("settlement_book_save_then_kill_before_save_all", func(t *testing.T) {
		env := legacyStore(t)
		singlePass(t, env, env.store.SaveStrategyBook)
	})

	t.Run("dashboard_book_save_after_save_all_failed", func(t *testing.T) {
		env := legacyStore(t)
		singlePass(t, env, env.fileFor(sc.ID).SaveStrategyBook)
	})

	cycleDMs := func(env *entryTestEnv) []mockDM {
		mock := &mockNotifier{}
		notifier := NewMultiNotifier(notifierBackend{notifier: mock, ownerID: "owner"})
		scopeRisk := map[RiskPartition]*scopeCycleRisk{part: {Partition: part, TotalPV: total}}
		env.mu.Lock()
		dm := runQuotePrecisionPeakRebaseline(activePartitions(env.cfg.Strategies), env.cfg.Strategies, env.state, prices, scopeRisk)
		env.mu.Unlock()
		if dm != "" {
			notifier.SendOwnerDM(dm)
		}
		mock.mu.Lock()
		defer mock.mu.Unlock()
		return append([]mockDM(nil), mock.dms...)
	}

	t.Run("owner_dm_lists_each_changed_peak_once", func(t *testing.T) {
		env := legacyStore(t)
		dms := cycleDMs(env)
		if len(dms) != 1 {
			t.Fatalf("owner DMs = %d, want 1: %+v", len(dms), dms)
		}
		label := partitionLabel(part)
		for _, want := range []string{
			"#1820 quote precision",
			fmt.Sprintf("- [%s] %s: peak $%.4f -> $%.4f (value $%.4f, rounding bound $%.4f)", label, sc.ID, legacyPeak, wantPeak, value, bound),
			fmt.Sprintf("- [%s] portfolio: peak $%.4f -> $%.4f (value $%.4f, rounding bound $%.4f)", label, legacyPart, wantPart, total, bound),
		} {
			if !strings.Contains(dms[0].content, want) {
				t.Fatalf("owner DM lacks %q:\n%s", want, dms[0].content)
			}
		}
		if strings.Contains(dms[0].content, flat.ID) {
			t.Fatalf("owner DM names the flat strategy:\n%s", dms[0].content)
		}
		t.Log(dms[0].content)
	})

	t.Run("owner_dm_silent_when_no_peak_changes", func(t *testing.T) {
		env := legacyStore(t)
		env.strat(sc.ID).RiskState.PeakValue = value
		env.state.partitionRisk(part).PeakValue = total
		if dms := cycleDMs(env); len(dms) != 0 {
			t.Fatalf("owner DMs = %+v, want none when every peak is unchanged", dms)
		}
		if !env.strat(sc.ID).RiskState.QuotePrecisionRebaselined || !env.state.partitionRisk(part).QuotePrecisionRebaselined {
			t.Fatal("an unchanged pass must still spend the markers")
		}
	})

	t.Run("owner_dm_silent_after_restart", func(t *testing.T) {
		env := legacyStore(t)
		if dms := cycleDMs(env); len(dms) != 1 {
			t.Fatalf("upgrade cycle owner DMs = %d, want 1", len(dms))
		}
		env.saveAll()
		env = env.restart()
		if dms := cycleDMs(env); len(dms) != 0 {
			t.Fatalf("owner DMs after restart = %+v, want none", dms)
		}
	})

	t.Run("save_all_then_restart_runs_no_second_pass", func(t *testing.T) {
		env := legacyStore(t)
		pass(env)
		env.saveAll()
		env = env.restart()
		env.strat(sc.ID).RiskState.PeakValue = legacyPeak
		env.state.partitionRisk(part).PeakValue = legacyPart
		if lines := pass(env); len(lines) != 0 {
			t.Fatalf("a spent marker still ran the pass: %v", lines)
		}
		if env.strat(sc.ID).RiskState.PeakValue != legacyPeak || env.state.partitionRisk(part).PeakValue != legacyPart {
			t.Fatal("a spent marker still moved a peak")
		}
	})
}

func TestCheckQuotePrecision(t *testing.T) {
	optionsOutage := qpStartOptionsSchedulerOutage(t)
	for _, tc := range []sigAdmCase{
		{"RunnersReturnTheFullQuote", qpCaseCheckQuotePrecisionRunnersReturnTheFullQuote},
		{"RunnerFallbacksAndFailures", qpCaseCheckQuotePrecisionRunnerFallbacksAndFailures},
		{"FetchPricesKeepsTheTickerQuote", qpCaseCheckQuotePrecisionFetchPricesKeepsTheTickerQuote},
		{"SchedulerCycleKeepsTinySpotQuotes", qpCaseCheckQuotePrecisionSchedulerCycleKeepsTinySpotQuotes},
		{"PaperBookingUsesTheFullQuote", qpCaseCheckQuotePrecisionPaperBookingUsesTheFullQuote},
		{"OKXLiveSizingAndFillPrecedence", qpCaseCheckQuotePrecisionOKXLiveSizingAndFillPrecedence},
		{"RobinhoodLiveKeepsTheVenueFill", qpCaseCheckQuotePrecisionRobinhoodLiveKeepsTheVenueFill},
		{"OptionsSpotAndPremiumPrecedence", qpCaseCheckQuotePrecisionOptionsSpotAndPremiumPrecedence},
		{"PriceTextKeepsLargePrices", qpCaseCheckQuotePrecisionPriceTextKeepsLargePrices},
		{"OptionsOutageKeepsHarvestAndReporting", qpCaseCheckQuotePrecisionOptionsOutageKeepsHarvestAndReporting},
		{"OptionsFailureAlertThresholds", qpCaseCheckQuotePrecisionOptionsFailureAlertThresholds},
		{"OptionsSchedulerHarvestsDuringEntryOutage", optionsOutage.check},
		{"OptionsResultContractRefusesMismatchedUnitsAndPlatforms", qpCaseCheckQuotePrecisionOptionsResultContract},
		{"OptionsStructureContractBooksWholeStructures", qpCaseCheckQuotePrecisionOptionsStructureContract},
	} {
		t.Run(tc.name, tc.run)
	}
}

const qpOptionsBaselineDaily = "baseline"

func qpOptionsDaily(kind string) string {
	if kind != qpOptionsBaselineDaily {
		return kind
	}
	closes := make([]float64, 90)
	for i := range closes {
		x := 100 * math.Exp(0.03*math.Sin(float64(i)*1.7)+0.02*math.Cos(float64(i)*0.37)+0.001*float64(i))
		closes[i] = math.Round(x*1e6) / 1e6
	}
	raw, err := json.Marshal(closes)
	if err != nil {
		panic(err)
	}
	return string(raw)
}

func qpOptionsHarvestSC(id string) StrategyConfig {
	return StrategyConfig{ID: id, Type: "options", Platform: "ibkr", Script: "shared_scripts/check_options.py",
		Args: []string{"vol_mean_reversion", "BTC", "--platform=ibkr"}, Capital: 1000, MaxDrawdownPct: 50,
		ThetaHarvest: &ThetaHarvestConfig{Enabled: true, ProfitTargetPct: 60, StopLossPct: 200, MinDTEClose: 3}}
}

type qpOptionLeg struct {
	id, optionType, action string
	dte, entryUSD, value   float64
	closes                 bool
}

func qpOptionLegs() []qpOptionLeg {
	return []qpOptionLeg{
		{id: "BTC-call-sell-profit", optionType: "call", action: "sell", dte: 20, entryUSD: 100, value: -30, closes: true},
		{id: "BTC-put-sell-stop", optionType: "put", action: "sell", dte: 20, entryUSD: 100, value: -350, closes: true},
		{id: "BTC-call-sell-dte", optionType: "call", action: "sell", dte: 2, entryUSD: 100, value: -90, closes: true},
	}
}

const (
	qpOptionsSeedCash    = 5350.0
	qpOptionsHarvestCost = 30.0 + 350.0 + 90.0
)

func qpSeedOptionLegs(s *StrategyState, now time.Time) {
	qpSeedOptionLegsN(s, now, len(qpOptionLegs()))
}

func qpSeedOptionLegsN(s *StrategyState, now time.Time, n int) {
	s.Cash = qpOptionsSeedCash
	for i, leg := range qpOptionLegs()[:n] {
		expiry := now.AddDate(0, 0, int(leg.dte)).Format("2006-01-02")
		s.OptionPositions[leg.id] = &OptionPosition{ID: leg.id, Underlying: "BTC", OptionType: leg.optionType, Strike: float64(3000 + 100*i),
			Expiry: expiry, DTE: leg.dte, Action: leg.action, Quantity: 1, EntryPremium: 0.1, EntryPremiumUSD: leg.entryUSD,
			CurrentValueUSD: leg.value, OpenedAt: now.Add(-time.Hour)}
	}
}

func qpResetScriptFailures(t *testing.T, ids ...string) {
	t.Helper()
	clear := func() {
		for _, id := range ids {
			scriptFailureTracker.Clear(id)
			scriptFailureTransientTracker.Clear(id)
		}
	}
	clear()
	t.Cleanup(clear)
}

func qpOptionsNotifier() (*MultiNotifier, *mockNotifier) {
	backend := &mockNotifier{}
	return NewMultiNotifier(notifierBackend{notifier: backend, channels: map[string]string{"options": "opt-ch"}, ownerID: "owner"}), backend
}

func qpChannelMessages(m *mockNotifier, needle string) []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []string
	for _, msg := range m.messages {
		if strings.Contains(msg.content, needle) {
			out = append(out, msg.channelID+": "+msg.content)
		}
	}
	return out
}

func qpCountRows(t *testing.T, db *sql.DB, query string, args ...interface{}) int {
	t.Helper()
	var n int
	if err := db.QueryRow(query, args...).Scan(&n); err != nil {
		t.Fatalf("%s: %v", query, err)
	}
	return n
}

func qpOptionsDispatch(env *entryTestEnv, sc StrategyConfig, notifier *MultiNotifier, logger *StrategyLogger) (int, []string) {
	s := env.strat(sc.ID)
	env.mu.RLock()
	posJSON, posErr := EncodeAllPositionsJSON(s.Platform, s.OptionPositions, s.Positions)
	env.mu.RUnlock()
	return runOptionsDispatch(optionsDispatch{SC: sc, GlobalIntervalSeconds: env.cfg.IntervalSeconds, State: s, PositionsJSON: posJSON, PositionsErr: posErr,
		Risk: &scopeCycleRisk{Partition: partitionFor(sc)}, Scope: partitionFor(sc).Scope, Mu: &env.mu, Notifier: notifier, Logger: logger})
}

type qpSeededValuePricer struct{ values map[string]float64 }

func (p qpSeededValuePricer) Name() string { return "qp-seeded-values" }

func (p qpSeededValuePricer) Refresh(reqs []optionLegRequest, refreshAt time.Time) []optionLegReply {
	var out []optionLegReply
	for _, req := range reqs {
		cutoff, ready, _, _ := optionGoExpirySchedule("ibkr", req.Expiry)
		out = append(out, optionLegReply{Key: req.Key, CutoffAt: cutoff, ReadyAt: ready,
			Mark: &optionLegMark{PerUnitUSD: math.Abs(p.values[req.Key]) / (req.Multiplier * req.Quantity), Spot: 1000, Source: "qp-seeded-values"}})
	}
	return out
}

func qpRefreshSeededOptionMarks(t *testing.T, env *entryTestEnv, sc StrategyConfig) {
	t.Helper()
	values := map[string]float64{}
	for _, leg := range qpOptionLegs() {
		values[leg.id] = leg.value
	}
	res := runOptionMarkRefresh(optionMarkRefresh{SC: sc, State: env.strat(sc.ID), Pricer: qpSeededValuePricer{values: values},
		Mu: &env.mu, Logger: silentStrategyLogger(sc.ID), RefreshAt: time.Now().UTC()})
	if res.Marked != len(env.strat(sc.ID).OptionPositions) || len(res.Failures) != 0 {
		t.Fatalf("seeded mark refresh = %+v, want every leg marked", res)
	}
}

func qpCaseCheckQuotePrecisionOptionsOutageKeepsHarvestAndReporting(t *testing.T) {
	root := qpRoot(t)
	cases := []struct {
		name      string
		daily     string
		entries   int
		legs      int
		wantError bool
	}{
		{name: "entry_check_fails", daily: "error", legs: 3, wantError: true},
		{name: "entry_check_succeeds", daily: qpOptionsBaselineDaily, entries: 2, legs: 2},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			qpDebugLogs(t)
			sc := qpOptionsHarvestSC("qp-opt-harvest")
			qpResetScriptFailures(t, sc.ID)
			qpEnv(t, root, map[string]string{"QP_QUOTE": "1000", "QP_CLOSE": "1000", "QP_DAILY": qpOptionsDaily(tc.daily)})
			env := openEntryTestEnv(t, newEntryTestConfig(t, sc))
			useEntryTradeRecorder(t, env.store)
			qpSeedOptionLegsN(env.strat(sc.ID), time.Now().UTC(), tc.legs)
			env.saveAll()
			env = env.restart()
			useEntryTradeRecorder(t, env.store)
			if got := len(env.strat(sc.ID).OptionPositions); got != tc.legs {
				t.Fatalf("reloaded %d option legs, want %d", got, tc.legs)
			}
			qpRefreshSeededOptionMarks(t, env, sc)
			notifier, backend := qpOptionsNotifier()
			logger, buf := qpLogger(sc.ID)
			trades, lines := qpOptionsDispatch(env, sc, notifier, logger)

			if tc.wantError != strings.Contains(buf.String(), "Script returned error: vol_mean_reversion needs iv rank: volatility unavailable: daily history read failed") {
				t.Fatalf("entry check error presence = %t; log:\n%s", !tc.wantError, buf.String())
			}
			if trades != tc.entries+tc.legs || len(lines) != tc.legs+boolInt(tc.entries > 0) {
				t.Fatalf("trades=%d lines=%q, want %d trades; log:\n%s", trades, lines, tc.entries+tc.legs, buf.String())
			}
			harvestLines := lines
			if tc.entries > 0 {
				if !strings.HasPrefix(lines[0], "[qp-opt-harvest] BEARISH BTC spot=$1000.00 IV=78.8") {
					t.Fatalf("the entry summary must lead the detail lines: %q", lines)
				}
				harvestLines = lines[1:]
			}
			for _, leg := range qpOptionLegs()[:tc.legs] {
				n := 0
				for _, line := range harvestLines {
					if strings.Contains(line, "CLOSE "+leg.id+" ") {
						n++
					}
				}
				if (leg.closes && n != 1) || (!leg.closes && n != 0) {
					t.Fatalf("leg %s appears in %d harvest detail(s): %q", leg.id, n, harvestLines)
				}
			}
			s := env.strat(sc.ID)
			if tc.entries == 0 && !qpClose(s.Cash, qpOptionsSeedCash-qpOptionsHarvestCost) {
				t.Fatalf("cash = %v, want %v", s.Cash, qpOptionsSeedCash-qpOptionsHarvestCost)
			}
			if len(s.OptionPositions) != tc.entries {
				t.Fatalf("option positions = %v, want only the %d new entries", s.OptionPositions, tc.entries)
			}
			for _, leg := range qpOptionLegs()[:tc.legs] {
				if _, ok := s.OptionPositions[leg.id]; ok == leg.closes {
					t.Fatalf("leg %s open=%t after harvest", leg.id, ok)
				}
			}

			channelTrades := map[string]int{}
			channelTradeDetails := map[string][]string{}
			if !routeCycleTrades(sc, trades, "", lines, notifier, channelTrades, channelTradeDetails) {
				t.Fatalf("the cycle tail must report %d options trades", trades)
			}
			sendTradeAlerts(sc, s, trades, &env.mu, notifier, nil)
			if channelTrades["options"] != trades {
				t.Fatalf("channel totals = %v, want options=%d", channelTrades, trades)
			}
			if got := channelTradeDetails["options|"+extractAsset(sc)]; len(got) != len(lines) {
				t.Fatalf("channel details = %q, want %q", got, lines)
			}
			if alerts := qpChannelMessages(backend, ""); len(alerts) != trades {
				t.Fatalf("trade alerts = %d, want %d: %q", len(alerts), trades, alerts)
			}

			env.saveAll()
			db := env.fileFor(sc.ID).db
			if n := qpCountRows(t, db, `SELECT COUNT(*) FROM closed_option_positions WHERE strategy_id = ? AND close_reason = 'theta_harvest'`, sc.ID); n != tc.legs {
				t.Fatalf("closed theta-harvest rows = %d, want %d (harvest runs once)", n, tc.legs)
			}
			if n := qpCountRows(t, db, `SELECT COUNT(*) FROM trades WHERE strategy_id = ? AND is_close = 1`, sc.ID); n != tc.legs {
				t.Fatalf("close trade rows = %d, want %d", n, tc.legs)
			}
			if n := qpCountRows(t, db, `SELECT COUNT(*) FROM trades WHERE strategy_id = ? AND is_close = 0`, sc.ID); n != tc.entries {
				t.Fatalf("entry trade rows = %d, want %d", n, tc.entries)
			}
			if n := qpCountRows(t, db, `SELECT COUNT(*) FROM option_positions WHERE strategy_id = ?`, sc.ID); n != tc.entries {
				t.Fatalf("stored option legs = %d, want %d", n, tc.entries)
			}
		})
	}
}

func qpCaseCheckQuotePrecisionOptionsFailureAlertThresholds(t *testing.T) {
	root := qpRoot(t)
	run := func(sc StrategyConfig, notifier *MultiNotifier) bool {
		logger, _ := qpLogger(sc.ID)
		_, _, ok := runOptionsCheck(sc, "[]", notifier, logger)
		return ok
	}
	failing := func(m *mockNotifier) int { return len(qpChannelMessages(m, "SIGNAL SCRIPT FAILING")) }

	t.Run("ordinary_failures_alert_at_three", func(t *testing.T) {
		sc := qpOptionsHarvestSC("qp-opt-alert")
		qpResetScriptFailures(t, sc.ID)
		notifier, backend := qpOptionsNotifier()
		qpEnv(t, root, map[string]string{"QP_QUOTE": "1000", "QP_CLOSE": "1000", "QP_DAILY": "error"})
		for i, want := range []int{0, 0, 1, 1} {
			if run(sc, notifier) {
				t.Fatalf("failure %d: the check succeeded", i+1)
			}
			if got := failing(backend); got != want {
				t.Fatalf("after failure %d: %d failure alert(s), want %d", i+1, got, want)
			}
		}
		if msgs := qpChannelMessages(backend, "SIGNAL SCRIPT FAILING"); !strings.Contains(msgs[0], "vol_mean_reversion needs iv rank: volatility unavailable: daily history read failed") || strings.Contains(msgs[0], "sustained upstream") {
			t.Fatalf("ordinary alert = %q", msgs[0])
		}
		qpEnv(t, root, map[string]string{"QP_QUOTE": "1000", "QP_CLOSE": "1000", "QP_DAILY": qpOptionsDaily(qpOptionsBaselineDaily)})
		if !run(sc, notifier) {
			t.Fatal("the recovery check failed")
		}
		if len(qpChannelMessages(backend, "SIGNAL SCRIPT RECOVERED")) != 1 {
			t.Fatalf("a success must clear the tracker with one recovery alert: %+v", backend.messages)
		}
		scriptFailureTracker.mu.Lock()
		left := scriptFailureTracker.entries[sc.ID]
		scriptFailureTracker.mu.Unlock()
		if left != nil {
			t.Fatalf("tracker entry survived a success: %+v", left)
		}
	})
}

type qpSchedOptionLeg struct {
	id, optionType string
	strike         float64
	days           int
	entryUSD       float64
	reason         string
}

func qpSchedOptionLegs() []qpSchedOptionLeg {
	return []qpSchedOptionLeg{
		{id: "BTC-call-sell-profit", optionType: "call", strike: 3000, days: 20, entryUSD: 100, reason: "Theta harvest:"},
		{id: "BTC-put-sell-stop", optionType: "put", strike: 1350, days: 20, entryUSD: 100, reason: "Stop loss:"},
		{id: "BTC-call-sell-dte", optionType: "call", strike: 1000, days: 2, entryUSD: 30, reason: "DTE exit:"},
	}
}

func qpFlatRankDaily() string {
	closes := make([]float64, 40)
	for i := range closes {
		closes[i] = 100
		if i%2 == 1 {
			closes[i] = 100 * math.Exp(0.8/math.Sqrt(365))
		}
	}
	raw, err := json.Marshal(closes)
	if err != nil {
		panic(err)
	}
	return string(raw)
}

type qpOptionsOutageRun struct {
	sc   StrategyConfig
	db   string
	stop chan struct{}
	done chan struct{}
	out  string
	err  error
}

func qpStartOptionsSchedulerOutage(t *testing.T) *qpOptionsOutageRun {
	t.Helper()
	root := robinhoodStdoutPyRoot(t)
	sched := qpBuildScheduler(t, root)
	sc := qpOptionsHarvestSC("qp-opt-sched")
	sc.IntervalSeconds = 20
	spot := qpSpotSC("qp-opt-sched-spot", "BTC/USDT")
	spot.IntervalSeconds = 20
	sched.env = qpChildEnv(t, root, map[string]string{"QP_QUOTE": "1000", "QP_CLOSE": "1000", "QP_DAILY": qpFlatRankDaily()})
	cfg := sched.config([]StrategyConfig{sc, spot})
	r := &qpOptionsOutageRun{sc: sc, db: filepath.Join(sched.dir, "state.db"), stop: make(chan struct{}), done: make(chan struct{})}
	bin, env := sched.bin, sched.env
	go func() {
		defer close(r.done)
		r.out, r.err = qpOptionsOutageScenario(bin, root, env, cfg, r.db, sc, r.stop)
	}()
	t.Cleanup(func() {
		close(r.stop)
		<-r.done
	})
	return r
}

func qpOptionsOutageScenario(bin, root string, env []string, cfgPath, dbPath string, sc StrategyConfig, stop <-chan struct{}) (string, error) {
	if _, err := qpRunSchedulerProcess(bin, root, env, cfgPath, "Cycle 1 complete", stop, nil); err != nil {
		return "", err
	}
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return "", err
	}
	defer db.Close()
	now := time.Now().UTC()
	for _, leg := range qpSchedOptionLegs() {
		if _, err := db.Exec(`INSERT INTO option_positions (strategy_id, id, position_id, underlying, option_type, strike, expiry, dte, action, quantity, entry_premium, entry_premium_usd, current_value_usd, opened_at)
			VALUES (?, ?, '', 'BTC', ?, ?, ?, ?, 'sell', 1, 0.1, ?, ?, ?)`,
			sc.ID, leg.id, leg.optionType, leg.strike, now.AddDate(0, 0, leg.days).Format("2006-01-02"), float64(leg.days), leg.entryUSD, -leg.entryUSD, now.Add(-time.Hour).Format(time.RFC3339)); err != nil {
			return "", fmt.Errorf("seed option leg %s: %v", leg.id, err)
		}
	}
	if _, err := db.Exec(`UPDATE strategies SET cash = ? WHERE id = ?`, qpOptionsSeedCash, sc.ID); err != nil {
		return "", fmt.Errorf("seed cash: %v", err)
	}
	return qpRunSchedulerProcess(bin, root, env, cfgPath, "Cycle 3 complete", stop, nil)
}

func (r *qpOptionsOutageRun) check(t *testing.T) {
	t.Parallel()
	<-r.done
	if r.err != nil {
		t.Fatal(r.err)
	}
	sc, out := r.sc, r.out
	db, err := sql.Open("sqlite", r.db)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	firstCycle, _, ok := strings.Cut(out, "Cycle 2 complete")
	if !ok {
		t.Fatalf("the process never finished its first cycle:\n%s", out)
	}
	if n := strings.Count(out, "Script returned error: vol_mean_reversion needs iv rank: iv rank unavailable: flat volatility range"); n < 2 {
		t.Fatalf("the entry check must fail in both cycles (%d failures):\n%s", n, out)
	}
	for _, leg := range qpSchedOptionLegs() {
		if strings.Contains(firstCycle, "| "+leg.id+" | PnL:") {
			t.Fatalf("leg %s harvested in the first cycle after a restart, before any successful mark:\n%s", leg.id, firstCycle)
		}
		if !strings.Contains(firstCycle, "Theta harvest holds "+leg.id+": no successful mark refresh in this process yet") {
			t.Fatalf("leg %s first-cycle hold reason missing:\n%s", leg.id, firstCycle)
		}
		n := strings.Count(out, "| "+leg.id+" | PnL:")
		if n != 1 {
			t.Fatalf("leg %s harvested %d time(s), want once in the second cycle:\n%s", leg.id, n, out)
		}
		line := ""
		for _, l := range strings.Split(out, "\n") {
			if strings.Contains(l, "| "+leg.id+" | PnL:") {
				line = l
			}
		}
		if !strings.Contains(line, leg.reason) {
			t.Fatalf("leg %s closed by %q, want %q", leg.id, line, leg.reason)
		}
	}
	var cash, closeTotal float64
	if err := db.QueryRow(`SELECT cash FROM strategies WHERE id = ?`, sc.ID).Scan(&cash); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT COALESCE(SUM(close_price_usd), 0) FROM closed_option_positions WHERE strategy_id = ? AND close_reason = 'theta_harvest'`, sc.ID).Scan(&closeTotal); err != nil {
		t.Fatal(err)
	}
	if closeTotal <= 0 || !qpClose(cash, qpOptionsSeedCash-closeTotal) {
		t.Fatalf("cash = %v, want seed %v minus the booked buybacks %v", cash, qpOptionsSeedCash, closeTotal)
	}
	if n := qpCountRows(t, db, `SELECT COUNT(*) FROM closed_option_positions WHERE strategy_id = ? AND close_reason = 'theta_harvest'`, sc.ID); n != 3 {
		t.Fatalf("closed theta-harvest rows = %d, want 3", n)
	}
	if n := qpCountRows(t, db, `SELECT COUNT(*) FROM trades WHERE strategy_id = ? AND is_close = 1`, sc.ID); n != 3 {
		t.Fatalf("close trade rows = %d, want 3", n)
	}
	if n := qpCountRows(t, db, `SELECT COUNT(*) FROM trades WHERE strategy_id = ? AND is_close = 0`, sc.ID); n != 0 {
		t.Fatalf("entry trade rows = %d, want none", n)
	}
	if n := qpCountRows(t, db, `SELECT COUNT(*) FROM option_positions WHERE strategy_id = ?`, sc.ID); n != 0 {
		t.Fatalf("stored option legs = %d, want none", n)
	}
}

const qpOptionsUnitWrapper = `import json, os, sys
sys.path.insert(0, os.path.join(os.environ["QP_OC_ROOT"], "shared_scripts"))
import check_options
unit = os.environ.get("QP_OC_UNIT", "")
def emit(doc):
    if unit:
        doc["premium_unit"] = unit
    else:
        doc.pop("premium_unit", None)
    sys.stdout.write(json.dumps(doc) + "\n")
sys.exit(check_options._run(sys.argv[1:], emit))
`

func qpScriptFailureCount(id string) int {
	scriptFailureTracker.mu.Lock()
	defer scriptFailureTracker.mu.Unlock()
	if e := scriptFailureTracker.entries[id]; e != nil {
		return e.count
	}
	return 0
}

func qpCaseCheckQuotePrecisionOptionsResultContract(t *testing.T) {
	root := qpRoot(t)
	fourLegs := `[{"option_type":"call","strike":500,"expiry":"2026-12-18"},{"option_type":"call","strike":510,"expiry":"2026-12-18"},{"option_type":"put","strike":450,"expiry":"2026-12-18"},{"option_type":"put","strike":440,"expiry":"2026-12-18"}]`

	t.Run("check_platform_must_match_the_strategy_platform", func(t *testing.T) {
		qpEnv(t, root, map[string]string{"QP_QUOTE": "1000", "QP_CLOSE": "1000"})
		for _, tc := range []struct{ cfgPlatform, argPlatform string }{
			{"deribit", "robinhood"},
			{"robinhood", "robinhood"},
		} {
			t.Run(tc.cfgPlatform+"_config_"+tc.argPlatform+"_check", func(t *testing.T) {
				sc := StrategyConfig{ID: "qp-opt-contract-" + tc.cfgPlatform + "-" + tc.argPlatform, Type: "options", Platform: tc.cfgPlatform,
					Script: "shared_scripts/check_options.py", Args: []string{"covered_calls", "SPY", "--platform=" + tc.argPlatform}, Capital: 1000}
				qpResetScriptFailures(t, sc.ID)
				notifier, _ := qpOptionsNotifier()
				logger, buf := qpLogger(sc.ID)
				result, _, ok := runOptionsCheck(sc, fourLegs, notifier, logger)
				if tc.cfgPlatform == tc.argPlatform {
					if !ok || result.Platform != tc.argPlatform || result.PremiumUnit != optionPremiumUnitUnderlying || qpScriptFailureCount(sc.ID) != 0 {
						t.Fatalf("matching platforms must pass: ok=%t result=%+v; log:\n%s", ok, result, buf.String())
					}
					return
				}
				want := fmt.Sprintf("Script contract failure: options result contract: check priced on platform %q but the strategy platform is %q", tc.argPlatform, tc.cfgPlatform)
				if ok || !strings.Contains(buf.String(), want) || qpScriptFailureCount(sc.ID) != 1 {
					t.Fatalf("mismatch must fail the check: ok=%t failures=%d; log:\n%s", ok, qpScriptFailureCount(sc.ID), buf.String())
				}
			})
		}
	})

	wrapper := filepath.Join(t.TempDir(), "unit_wrapper.py")
	if err := os.WriteFile(wrapper, []byte(qpOptionsUnitWrapper), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, unit, script, argPlatform string
		wantFailure                     string
		entries, legs                   int
	}{
		{name: "other_unit", unit: "contract", script: wrapper, argPlatform: "ibkr", legs: 3, wantFailure: `premium_unit "contract", want "underlying_unit"`},
		{name: "valid_result", unit: optionPremiumUnitUnderlying, script: wrapper, argPlatform: "ibkr", entries: 2, legs: 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sc := qpOptionsHarvestSC("qp-opt-contract-harvest")
			sc.Script = tc.script
			sc.Args = []string{"vol_mean_reversion", "BTC", "--platform=" + tc.argPlatform}
			qpResetScriptFailures(t, sc.ID)
			qpEnv(t, root, map[string]string{"QP_QUOTE": "1000", "QP_CLOSE": "1000", "QP_DAILY": qpOptionsDaily(qpOptionsBaselineDaily),
				"QP_OC_ROOT": root, "QP_OC_UNIT": tc.unit})
			env := openEntryTestEnv(t, newEntryTestConfig(t, sc))
			useEntryTradeRecorder(t, env.store)
			qpSeedOptionLegsN(env.strat(sc.ID), time.Now().UTC(), tc.legs)
			qpRefreshSeededOptionMarks(t, env, sc)
			notifier, _ := qpOptionsNotifier()
			logger, buf := qpLogger(sc.ID)
			trades, lines := qpOptionsDispatch(env, sc, notifier, logger)
			if trades != tc.entries+tc.legs {
				t.Fatalf("trades=%d lines=%q, want %d entries plus %d harvest closes; log:\n%s", trades, lines, tc.entries, tc.legs, buf.String())
			}
			s := env.strat(sc.ID)
			if len(s.OptionPositions) != tc.entries {
				t.Fatalf("option legs = %d, want %d", len(s.OptionPositions), tc.entries)
			}
			db := env.fileFor(sc.ID).db
			env.saveAll()
			if n := qpCountRows(t, db, `SELECT COUNT(*) FROM trades WHERE strategy_id = ? AND is_close = 0`, sc.ID); n != tc.entries {
				t.Fatalf("entry trade rows = %d, want %d", n, tc.entries)
			}
			if n := qpCountRows(t, db, `SELECT COUNT(*) FROM trades WHERE strategy_id = ? AND is_close = 1`, sc.ID); n != tc.legs {
				t.Fatalf("harvest close rows = %d, want %d (harvest runs once)", n, tc.legs)
			}
			if tc.wantFailure == "" {
				if qpScriptFailureCount(sc.ID) != 0 {
					t.Fatalf("a valid result must clear the failure state; log:\n%s", buf.String())
				}
				for _, leg := range s.OptionPositions {
					if leg.ContractMultiplier != 0.1 {
						t.Fatalf("ibkr leg %s stamped %v, want 0.1", leg.ID, leg.ContractMultiplier)
					}
				}
				return
			}
			if !strings.Contains(buf.String(), "Script contract failure: options result contract: "+tc.wantFailure) || qpScriptFailureCount(sc.ID) != 1 {
				t.Fatalf("contract failure not reported once (failures=%d); log:\n%s", qpScriptFailureCount(sc.ID), buf.String())
			}
		})
	}
}

const qpOptionsStructureWrapper = `import json, os, sys
sys.path.insert(0, os.path.join(os.environ["QP_OC_ROOT"], "shared_scripts"))
import check_options
mode = os.environ.get("QP_OC_MUTATE", "")
extra = os.environ.get("QP_OC_CLOSE", "")
def emit(doc):
    entries = [a for a in doc.get("actions", []) if a.get("action") in ("buy", "sell")]
    if mode == "truncate":
        doc["actions"].remove(entries[-1])
    if extra:
        doc["actions"].append(json.loads(extra))
    sys.stdout.write(json.dumps(doc) + "\n")
sys.exit(check_options._run(sys.argv[1:], emit))
`

func qpMidRankDaily() string {
	closes := make([]float64, 80)
	for i := range closes {
		x := 100 * math.Exp(0.01*math.Sin(float64(i)*1.7)+0.01*math.Cos(float64(i)*0.37))
		closes[i] = math.Round(x*1e6) / 1e6
	}
	raw, err := json.Marshal(closes)
	if err != nil {
		panic(err)
	}
	return string(raw)
}

func qpCaseCheckQuotePrecisionOptionsStructureContract(t *testing.T) {
	root := qpRoot(t)

	t.Run("real_butterfly_books_whole_and_commits_with_the_save", func(t *testing.T) {
		sc := StrategyConfig{ID: "qp-opt-fly", Type: "options", Platform: "deribit", Script: "shared_scripts/check_options.py",
			Args: []string{"butterfly", "BTC", "--platform=deribit"}, Capital: 100000, MaxDrawdownPct: 50}
		qpResetScriptFailures(t, sc.ID)
		qpEnv(t, root, map[string]string{"QP_QUOTE": "100000", "QP_CLOSE": "100000", "QP_DAILY": qpMidRankDaily()})
		env := openEntryTestEnv(t, newEntryTestConfig(t, sc))
		useEntryTradeRecorder(t, env.store)
		env.saveAll()
		notifier, _ := qpOptionsNotifier()
		logger, buf := qpLogger(sc.ID)
		trades, _ := qpOptionsDispatch(env, sc, notifier, logger)
		s := env.strat(sc.ID)
		if trades != 3 || len(s.OptionPositions) != 3 || qpScriptFailureCount(sc.ID) != 0 {
			t.Fatalf("trades=%d legs=%v failures=%d; log:\n%s", trades, sortedOptionLegKeys(s.OptionPositions), qpScriptFailureCount(sc.ID), buf.String())
		}
		for _, leg := range s.OptionPositions {
			if leg.ID != optionContractKey("BTC", leg.OptionType, leg.Action, leg.Strike, leg.Expiry) {
				t.Fatalf("leg key %s does not match its contract", leg.ID)
			}
		}
		db := env.fileFor(sc.ID).db
		if n := qpCountRows(t, db, `SELECT COUNT(*) FROM trades WHERE strategy_id = ?`, sc.ID); n != 0 {
			t.Fatalf("entry trades persisted before the save: %d", n)
		}
		env.saveAll()
		if n := qpCountRows(t, db, `SELECT COUNT(*) FROM trades WHERE strategy_id = ?`, sc.ID); n != 3 {
			t.Fatalf("entry trade rows after save = %d, want 3", n)
		}
		if n := qpCountRows(t, db, `SELECT COUNT(*) FROM option_positions WHERE strategy_id = ?`, sc.ID); n != 3 {
			t.Fatalf("stored option rows after save = %d, want 3", n)
		}
	})

	wrapper := filepath.Join(t.TempDir(), "structure_wrapper.py")
	if err := os.WriteFile(wrapper, []byte(qpOptionsStructureWrapper), 0o644); err != nil {
		t.Fatal(err)
	}
	closeJSON := `{"action":"close","option_type":"put","strike":2500,"premium_usd":0.5}`
	for _, tc := range []struct {
		mode, want string
		entries    int
	}{
		{mode: "", entries: 2},
		{mode: "truncate", want: "structure_legs 2 does not match its 1 member(s)"},
	} {
		name := tc.mode
		if name == "" {
			name = "valid_control"
		}
		t.Run(name, func(t *testing.T) {
			sc := qpOptionsHarvestSC("qp-opt-structure")
			sc.Script = wrapper
			qpResetScriptFailures(t, sc.ID)
			qpEnv(t, root, map[string]string{"QP_QUOTE": "1000", "QP_CLOSE": "1000", "QP_DAILY": qpOptionsDaily(qpOptionsBaselineDaily),
				"QP_OC_ROOT": root, "QP_OC_MUTATE": tc.mode, "QP_OC_CLOSE": closeJSON})
			env := openEntryTestEnv(t, newEntryTestConfig(t, sc))
			useEntryTradeRecorder(t, env.store)
			s := env.strat(sc.ID)
			now := time.Now().UTC()
			qpSeedOptionLegsN(s, now, 1)
			qpRefreshSeededOptionMarks(t, env, sc)
			s.OptionPositions["BTC-put-sell-2500-target"] = &OptionPosition{ID: "BTC-put-sell-2500-target", Underlying: "BTC", OptionType: "put", Strike: 2500,
				Expiry: now.AddDate(0, 0, 20).Format("2006-01-02"), DTE: 20, Action: "sell", Quantity: 1, ContractMultiplier: 1, EntryPremiumUSD: 60, CurrentValueUSD: -50, OpenedAt: now.Add(-time.Hour)}
			notifier, _ := qpOptionsNotifier()
			logger, buf := qpLogger(sc.ID)
			trades, _ := qpOptionsDispatch(env, sc, notifier, logger)
			if s.OptionPositions["BTC-put-sell-2500-target"] != nil || s.OptionPositions[qpOptionLegs()[0].id] != nil {
				t.Fatalf("the explicit close and the harvest must run: legs=%v; log:\n%s", sortedOptionLegKeys(s.OptionPositions), buf.String())
			}
			if trades != tc.entries+2 || len(s.OptionPositions) != tc.entries {
				t.Fatalf("trades=%d legs=%v, want %d entries plus one close and one harvest; log:\n%s", trades, sortedOptionLegKeys(s.OptionPositions), tc.entries, buf.String())
			}
			env.saveAll()
			db := env.fileFor(sc.ID).db
			if n := qpCountRows(t, db, `SELECT COUNT(*) FROM trades WHERE strategy_id = ? AND is_close = 0`, sc.ID); n != tc.entries {
				t.Fatalf("entry trade rows = %d, want %d", n, tc.entries)
			}
			if n := qpCountRows(t, db, `SELECT COUNT(*) FROM closed_option_positions WHERE strategy_id = ? AND close_reason = 'theta_harvest'`, sc.ID); n != 1 {
				t.Fatalf("theta harvest closes = %d, want 1 (harvest runs once)", n)
			}
			if tc.want == "" {
				if qpScriptFailureCount(sc.ID) != 0 {
					t.Fatalf("a valid structure result must clear the failure state; log:\n%s", buf.String())
				}
				return
			}
			if qpScriptFailureCount(sc.ID) != 1 || !strings.Contains(buf.String(), "Script structure failure: options structure contract: ") || !strings.Contains(buf.String(), tc.want) {
				t.Fatalf("structure failure not reported once with %q (failures=%d); log:\n%s", tc.want, qpScriptFailureCount(sc.ID), buf.String())
			}
		})
	}
}

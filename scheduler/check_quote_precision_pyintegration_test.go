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
	"math/rand"
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

var qpQuotes = []float64{0.1234, 0.006, 0.00001234}

var qpZeroPriceText = regexp.MustCompile(`\$0\.00(?:[^0-9]|$)`)

const (
	qpMarketOpen = "2026-10-21T12:00:00-04:00"
	qpRoundedTo  = 0.12
)

var qpEnvKeys = []string{
	"QP_CLOSE", "QP_QUOTE", "QP_SHAPE", "QP_NOW", "QP_TICKERS", "QP_CLOSES", "QP_CANDLES",
	"QP_OKX_FILL", "QP_LOG", "QP_ALL_CLOSED",
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

func TestCheckQuotePrecisionRunnersReturnTheFullQuote(t *testing.T) {
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

func TestCheckQuotePrecisionRunnerFallbacksAndFailures(t *testing.T) {
	root := qpRoot(t)
	q := 0.00001234
	for _, r := range qpRunners() {
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
	for _, r := range qpRunners() {
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

func TestCheckQuotePrecisionFetchPricesKeepsTheTickerQuote(t *testing.T) {
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
	t     *testing.T
	root  string
	bin   string
	dir   string
	lines []string
}

func qpBuildScheduler(t *testing.T, root string) *qpScheduler {
	t.Helper()
	dir := t.TempDir()
	bin := filepath.Join(dir, "go-trader")
	build := exec.Command("go", "build", "-o", bin, ".")
	build.Dir = filepath.Join(root, "scheduler")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build scheduler: %v\n%s", err, out)
	}
	return &qpScheduler{t: t, root: root, bin: bin}
}

func (s *qpScheduler) config(strategies []StrategyConfig) string {
	s.t.Helper()
	s.dir = s.t.TempDir()
	cfg := map[string]interface{}{
		"interval_seconds": 3600,
		"db_file":          filepath.Join(s.dir, "state.db"),
		"status_port":      20000 + rand.Intn(20000),
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
	s.lines = nil
	cmd := exec.Command(s.bin, "--config", cfgPath)
	cmd.Dir = s.root
	cmd.Env = os.Environ()
	pr, pw := io.Pipe()
	cmd.Stdout = pw
	cmd.Stderr = pw
	if err := cmd.Start(); err != nil {
		s.t.Fatalf("start scheduler: %v", err)
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
	statusURL := ""
	reached := false
	deadline := time.After(180 * time.Second)
wait:
	for {
		select {
		case line, ok := <-lines:
			if !ok {
				break wait
			}
			s.lines = append(s.lines, line)
			if m := qpStatusLine.FindStringSubmatch(line); m != nil {
				statusURL = m[1]
			}
			if strings.Contains(line, until) {
				reached = true
				break wait
			}
		case <-deadline:
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
		s.lines = append(s.lines, line)
	}
	out := strings.Join(s.lines, "\n")
	if !reached {
		s.t.Fatalf("scheduler never printed %q; output:\n%s", until, out)
	}
	return out
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

func TestCheckQuotePrecisionSchedulerCycleKeepsTinySpotQuotes(t *testing.T) {
	root := qpRoot(t)
	sched := qpBuildScheduler(t, root)
	q := 0.00001234
	tinyBuy := ApplyAdverseSlippage(q, true)

	t.Run("only_tiny_quotes", func(t *testing.T) {
		qpEnv(t, root, map[string]string{"QP_TICKERS": `{"DOGE/USDT": 1.234e-05}`, "QP_CLOSES": `{"DOGE/USDT": 1.234e-05}`, "QP_SHAPE": "cross_up"})
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

	t.Run("tiny_quote_with_a_positive_peer", func(t *testing.T) {
		qpEnv(t, root, map[string]string{
			"QP_TICKERS": `{"DOGE/USDT": 1.234e-05, "BTC/USDT": 65000.5}`,
			"QP_CLOSES":  `{"DOGE/USDT": 1.234e-05, "BTC/USDT": 65000.5}`,
			"QP_SHAPE":   "cross_up",
		})
		cfg := sched.config([]StrategyConfig{qpSpotSC("qp-doge", "DOGE/USDT"), qpSpotSC("qp-btc", "BTC/USDT")})
		var status map[string]float64
		out := sched.run(cfg, "Cycle 1 complete", func(u string) { status = qpStatusPrices(t, u) })
		if status["DOGE/USDT"] != q || status["BTC/USDT"] != 65000.5 {
			t.Fatalf("status prices = %v", status)
		}
		for _, want := range []string{"Signal: BUY | DOGE/USDT @ $1.234e-05", "Signal: BUY | BTC/USDT @ $65000.50"} {
			if !strings.Contains(out, want) {
				t.Fatalf("output lacks %q:\n%s", want, out)
			}
		}
		byID := map[string]qpTradeRow{}
		for _, tr := range sched.trades() {
			byID[tr.StrategyID] = tr
		}
		if len(byID) != 2 || !qpClose(byID["qp-doge"].Price, tinyBuy) || !qpClose(byID["qp-btc"].Price, ApplyAdverseSlippage(65000.5, true)) {
			t.Fatalf("trades = %+v", byID)
		}
	})

	t.Run("unavailable_quote_skips_the_cycle", func(t *testing.T) {
		qpEnv(t, root, map[string]string{"QP_TICKERS": `{"DOGE/USDT": null}`, "QP_CLOSES": `{"DOGE/USDT": 1.234e-05}`, "QP_SHAPE": "cross_up"})
		cfg := sched.config([]StrategyConfig{qpSpotSC("qp-doge", "DOGE/USDT")})
		out := sched.run(cfg, "All prices are zero/missing", nil)
		if strings.Contains(out, "Cycle 1 complete") || strings.Contains(out, "Signal: BUY") {
			t.Fatalf("an unavailable quote must skip the cycle before any strategy runs:\n%s", out)
		}
		if trades := sched.trades(); len(trades) != 0 {
			t.Fatalf("trades = %+v, want none", trades)
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

func TestCheckQuotePrecisionPaperBookingUsesTheFullQuote(t *testing.T) {
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

func TestCheckQuotePrecisionOKXLiveSizingAndFillPrecedence(t *testing.T) {
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

		log := filepath.Join(t.TempDir(), "venue.jsonl")
		qpOKXLiveEnv(t, root, log, map[string]interface{}{"average": 0.00001236, "fee": 0.01}, "cross_up", q)
		env := openEntryTestEnv(t, newEntryTestConfig(t, sc))
		useEntryTradeRecorder(t, env.store)
		at := time.Now().Add(-5 * time.Minute)
		bar := at.Truncate(time.Hour).Add(-time.Hour)
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

func TestCheckQuotePrecisionRobinhoodLiveKeepsTheVenueFill(t *testing.T) {
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

	t.Run("missing_or_unresolved_fill_leaves_the_book", func(t *testing.T) {
		cases := map[string]struct {
			routes rhRoutes
			status string
		}{
			"zero_fill_cancel": {rhRoutes{"POST " + rhTestOrders: {rhAccepted("buy")}, "GET " + rhOrderURL(rhTestOrderID): {rhOrder("canceled", "buy", "0", nil)}}, robinhoodAttemptNoFill},
			"http_503":         {rhRoutes{"POST " + rhTestOrders: {{"status": 503, "json": map[string]interface{}{"detail": "unavailable"}}}}, robinhoodAttemptUnknown},
		}
		names := make([]string, 0, len(cases))
		for name := range cases {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			tc := cases[name]
			t.Run(name, func(t *testing.T) {
				e := newRHLiveEnv(t, rhLiveSpot("rh-btc", "BTC", 1000))
				e.setRoutes(rhWith(rhPrepRoutes(), tc.routes))
				if trades, _ := e.signal("rh-btc", 1, 0, q); trades != 0 {
					t.Fatalf("trades = %d, want 0", trades)
				}
				s := e.state.Strategies["rh-btc"]
				if s.Cash != 1000 || len(s.Positions) != 0 || len(e.dbTrades("rh-btc")) != 0 {
					t.Fatalf("book changed: cash=%v positions=%v", s.Cash, s.Positions)
				}
				if row := e.onlyAttempt(); row.Status != tc.status {
					t.Fatalf("attempt = %+v, want %s", row, tc.status)
				}
			})
		}
	})

	t.Run("partial_sell_keeps_the_remainder", func(t *testing.T) {
		e := newRHLiveEnv(t, rhLiveSpot("rh-btc", "BTC", 1000))
		e.setPosition("rh-btc", "BTC", 0.5, 90)
		e.setRoutes(rhWith(rhWith(rhPrepRoutes(), rhSellHolding("0.5", "0.5")), rhRoutes{
			"POST " + rhTestOrders:             {rhAccepted("sell")},
			"GET " + rhOrderURL(rhTestOrderID): {rhOrder("confirmed", "sell", "0", nil), rhOrder("canceled", "sell", "0.2", "99")},
		}))
		if trades, _ := e.signal("rh-btc", -1, 0, q); trades != 1 {
			t.Fatalf("trades = %d, want 1", trades)
		}
		if pos := e.state.Strategies["rh-btc"].Positions["BTC"]; pos == nil || math.Abs(pos.Quantity-0.3) > 1e-12 {
			t.Fatalf("position = %+v, want 0.3 left", pos)
		}
		if dbt := e.dbTrades("rh-btc"); len(dbt) != 1 || dbt[0].Quantity != 0.2 || dbt[0].Price != 99 {
			t.Fatalf("db trades = %+v", dbt)
		}
	})

	t.Run("sell_is_capped_by_quantity_available", func(t *testing.T) {
		e := newRHLiveEnv(t, rhLiveSpot("rh-btc", "BTC", 1000))
		e.setPosition("rh-btc", "BTC", 0.5, 90)
		e.setRoutes(rhSizedSellRoutes(rhSellHolding("0.5", "0.3"), "0.3"))
		if trades, _ := e.signal("rh-btc", -1, 0, q); trades != 1 {
			t.Fatalf("trades = %d, want 1", trades)
		}
		rhSamePosts(t, e.postQuantities(), 0.3)
		if pos := e.state.Strategies["rh-btc"].Positions["BTC"]; pos == nil || math.Abs(pos.Quantity-0.2) > 1e-12 {
			t.Fatalf("position = %+v, want 0.2 left", pos)
		}
	})
}

func TestCheckQuotePrecisionOptionsSpotAndPremiumPrecedence(t *testing.T) {
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
			result.Actions = []OptionsAction{
				{Action: "buy", OptionType: "call", Strike: 1, Expiry: "2026-12-25", Premium: 0.05, Quantity: 2},
				{Action: "sell", OptionType: "call", Strike: 2, Expiry: "2026-12-25", Premium: 0.03, PremiumUSD: -1, Quantity: 1},
				{Action: "buy", OptionType: "put", Strike: 3, Expiry: "2026-12-25", Premium: 0.05, PremiumUSD: 3.5, Quantity: 1},
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
	t.Run("unavailable_spot_holds", func(t *testing.T) {
		qpEnv(t, root, map[string]string{"QP_CLOSE": "0.1234"})
		logger, buf := qpLogger(sc.ID)
		if _, _, ok := runOptionsCheck(sc, "[]", nil, logger); ok {
			t.Fatalf("a missing spot quote must hold; log:\n%s", buf.String())
		}
	})
}

func TestCheckQuotePrecisionPriceTextKeepsLargePrices(t *testing.T) {
	root := qpRoot(t)
	qpDebugLogs(t)
	cases := []struct {
		r    qpRunner
		q    float64
		text string
	}{
		{qpRunners()[0], 65000.5, "65000.50"},
		{qpRunners()[3], 3000.25, "3000.25"},
		{qpRunners()[6], 5000.25, "5000.25"},
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
	root := qpRoot(t)
	sched := qpBuildScheduler(t, root)
	const q = 0.0055
	const legacyMark = 0.01
	tickers := `{"DOGE/USDT": 0.0055, "SHIB/USDT": 0.0055, "XRP/USDT": 0.0055}`
	qpEnv(t, root, map[string]string{"QP_TICKERS": tickers, "QP_CLOSES": tickers, "QP_SHAPE": "cross_up"})
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

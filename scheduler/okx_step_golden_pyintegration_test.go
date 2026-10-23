//go:build pyintegration

package main

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const okxGoldenProjectionVersion = 1

const okxGoldenDir = "testdata/okx_step/golden"

const okxGoldenManifest = "testdata/okx_step/golden/manifest.json"

const okxGoldenSourceFile = "okx_step.go"

const okxGoldenCaptureEnv = "OKX_STEP_CAPTURE_GOLDEN"

const okxGoldenCheckScript = "scheduler/testdata/okx_step/check_fixture.py"

type okxGoldenManifestFile struct {
	ProjectionVersion int      `json:"projection_version"`
	BaseCommit        string   `json:"base_commit"`
	SourceCommit      string   `json:"source_commit"`
	SourceFile        string   `json:"source_file"`
	SourceSHA256      string   `json:"source_sha256"`
	CaptureInvocation string   `json:"capture_invocation"`
	Scenarios         []string `json:"scenarios"`
}

type okxGoldenCheck struct {
	Signal        int
	Price         float64
	CloseFraction float64
	ClosedBar     string
	HTF           string
	Raw           string
	Exit          int
	Error         string
	Stderr        string
}

type okxGoldenSendCall struct {
	Guarded  bool
	Symbol   string
	Side     string
	Size     float64
	InstType string
	Opts     okxExecuteOptions
}

type okxGoldenVenue func(call okxGoldenSendCall) (*OKXExecuteResult, error)

type okxGoldenStep struct {
	Name     string
	Check    okxGoldenCheck
	Prices   map[string]float64
	Venue    []okxGoldenVenue
	Before   func(h *okxGoldenHarness)
	FailSave bool
}

type okxGoldenScenario struct {
	Name   string
	Types  []string
	Mode   string
	Config func(sc *StrategyConfig)
	Steps  []okxGoldenStep
	Expect map[string][]string
}

type okxGoldenRecNotifier struct {
	mu     sync.Mutex
	events []map[string]string
}

func (n *okxGoldenRecNotifier) SendMessage(channelID, content string) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.events = append(n.events, map[string]string{"kind": "channel", "target": channelID, "content": content})
	return nil
}

func (n *okxGoldenRecNotifier) SendDM(userID, content string) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.events = append(n.events, map[string]string{"kind": "dm", "target": userID, "content": content})
	return nil
}

func (n *okxGoldenRecNotifier) AskDM(userID, question string, timeout time.Duration) (string, error) {
	return "", errors.New("the okx step fixture never asks")
}

func (n *okxGoldenRecNotifier) Close() {}

func (n *okxGoldenRecNotifier) drain() []map[string]string {
	n.mu.Lock()
	defer n.mu.Unlock()
	out := n.events
	n.events = nil
	return out
}

type okxGoldenDenyTransport struct {
	attempts atomic.Int64
}

func (d *okxGoldenDenyTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	d.attempts.Add(1)
	return nil, fmt.Errorf("the okx step fixture denies Go HTTP to %s", req.URL.Host)
}

type okxGoldenHarness struct {
	t        *testing.T
	env      *entryTestEnv
	sc       StrategyConfig
	sr       *scopeCycleRisk
	rec      *okxGoldenRecNotifier
	notifier *MultiNotifier
	logBuf   *bytes.Buffer
	logger   *StrategyLogger
	dir      string
	spec     string
	argvLog  string
	venue    []okxGoldenVenue
	sends    []map[string]interface{}
	barMs    int64
	cutoffMs int64
	nowBase  time.Time
}

func okxGoldenTimes() (bar, cutoff time.Time) {
	cutoff = time.Now().UTC().Add(-5 * time.Minute).Truncate(time.Second)
	bar = cutoff.Truncate(time.Hour).Add(-time.Hour)
	return bar, cutoff
}

func (h *okxGoldenHarness) response(c okxGoldenCheck) map[string]interface{} {
	resp := map[string]interface{}{
		"strategy": "fixture", "symbol": "BTC", "timeframe": "1h", "signal": c.Signal, "price": c.Price,
		"close_fraction": c.CloseFraction, "indicators": map[string]interface{}{"atr": 2.0},
		"mode": okxGoldenMode(h.sc), "platform": "okx", "timestamp": "fixture",
	}
	if c.Error != "" {
		resp["error"] = c.Error
	}
	switch c.ClosedBar {
	case "":
		if h.sc.ClosedBarDecisions {
			resp["closed_bar_decision"] = h.closedBarBlock(false)
		}
	case "held":
		resp["closed_bar_decision"] = h.closedBarBlock(true)
	case "none":
	}
	switch c.HTF {
	case "ok":
		resp["htf_filter"] = map[string]interface{}{"status": "ok", "timeframe": "4h", "trend": 1, "reason": ""}
	case "unavailable":
		resp["htf_filter"] = map[string]interface{}{"status": "unavailable", "timeframe": "4h", "trend": 0, "reason": "fixture higher-timeframe candles are missing"}
	case "":
		if h.sc.HTFFilter {
			resp["htf_filter"] = map[string]interface{}{"status": "ok", "timeframe": "4h", "trend": 1, "reason": ""}
		}
	case "none":
	}
	return resp
}

func (h *okxGoldenHarness) closedBarBlock(held bool) map[string]interface{} {
	b := map[string]interface{}{
		"enabled": true, "cutoff_ms": h.cutoffMs, "decision_boundary_ms": h.barMs + entryTestHour,
		"bar_open_ms": h.barMs, "interval_ms": entryTestHour, "closure_rule": "fixture_closed_bar", "rows": 200,
	}
	if held {
		b["held"] = true
		b["hold_reason"] = "fixture history is incomplete"
	}
	return b
}

func okxGoldenMode(sc StrategyConfig) string {
	if okxIsLive(sc.Args) {
		return "live"
	}
	return "paper"
}

func (h *okxGoldenHarness) writeSpec(c okxGoldenCheck) {
	h.t.Helper()
	spec := map[string]interface{}{"argv_log": h.argvLog, "exit_code": c.Exit}
	if c.Stderr != "" {
		spec["stderr"] = c.Stderr
	}
	if c.Raw != "" {
		spec["raw_stdout"] = c.Raw
	} else {
		spec["response"] = h.response(c)
	}
	blob, err := json.Marshal(spec)
	if err != nil {
		h.t.Fatal(err)
	}
	if err := os.WriteFile(h.spec, blob, 0o600); err != nil {
		h.t.Fatal(err)
	}
	_ = os.Remove(h.argvLog)
}

func (h *okxGoldenHarness) installVenue() {
	prevPlain, prevGuarded := okxExecuteFn, okxExecuteGuardedFn
	h.t.Cleanup(func() { okxExecuteFn, okxExecuteGuardedFn = prevPlain, prevGuarded })
	respond := func(call okxGoldenSendCall) (*OKXExecuteResult, string, error) {
		entry := map[string]interface{}{
			"guarded": call.Guarded, "symbol": call.Symbol, "side": call.Side, "size": call.Size, "inst_type": call.InstType,
			"client_order_id": call.Opts.ClientOrderID, "reduce_only": call.Opts.ReduceOnly, "expires_after_ms": call.Opts.ExpiresAfterMs,
		}
		if call.Opts.ClientOrderID != "" {
			h.env.mu.RLock()
			saved := false
			if g := h.env.state.Strategies[h.sc.ID].EntryGuard; g != nil {
				for _, in := range g.Intents {
					if in.ClientOrderID != call.Opts.ClientOrderID {
						continue
					}
					var req map[string]interface{}
					if json.Unmarshal([]byte(in.RequestJSON), &req) == nil {
						sub, _ := req["submitted_at_ms"].(float64)
						exp, _ := req["expires_after_ms"].(float64)
						saved = true
						entry["saved_intent_status"] = in.Status
						entry["saved_expiry_matches_sent"] = int64(exp) == call.Opts.ExpiresAfterMs
						entry["saved_deadline_ms"] = int64(exp - sub)
						entry["saved_side_size_match"] = req["side"] == call.Side && req["size"] == call.Size
					}
				}
			}
			h.env.mu.RUnlock()
			entry["saved_intent_found"] = saved
		}
		h.sends = append(h.sends, entry)
		if len(h.venue) == 0 {
			entry["response"] = "unscripted"
			return nil, "", errors.New("the okx step fixture has no scripted venue response")
		}
		next := h.venue[0]
		h.venue = h.venue[1:]
		res, err := next(call)
		if err != nil {
			entry["response"] = "error: " + err.Error()
		} else if res != nil {
			entry["response"] = res.OrderOutcome + "|" + res.Error
		}
		return res, "", err
	}
	okxExecuteFn = func(script, symbol, side string, size float64, instType string) (*OKXExecuteResult, string, error) {
		return respond(okxGoldenSendCall{Symbol: symbol, Side: side, Size: size, InstType: instType})
	}
	okxExecuteGuardedFn = func(script, symbol, side string, size float64, instType string, opts okxExecuteOptions) (*OKXExecuteResult, string, error) {
		return respond(okxGoldenSendCall{Guarded: true, Symbol: symbol, Side: side, Size: size, InstType: instType, Opts: opts})
	}
}

func okxGoldenFill(fraction float64) okxGoldenVenue {
	return func(call okxGoldenSendCall) (*OKXExecuteResult, error) {
		qty := call.Size * fraction
		return &OKXExecuteResult{OrderOutcome: "filled", OrderStatus: "closed", ClientOrderID: call.Opts.ClientOrderID,
			Execution: &OKXExecution{Action: call.Side, Symbol: call.Symbol, Size: call.Size, Fill: &OKXFill{AvgPx: 100, TotalSz: qty, Fee: 0.01, OID: "venue-" + call.Side}}}, nil
	}
}

func okxGoldenFillAt(px, qty float64) okxGoldenVenue {
	return func(call okxGoldenSendCall) (*OKXExecuteResult, error) {
		return &OKXExecuteResult{OrderOutcome: "filled", OrderStatus: "closed", ClientOrderID: call.Opts.ClientOrderID,
			Execution: &OKXExecution{Action: call.Side, Symbol: call.Symbol, Size: call.Size, Fill: &OKXFill{AvgPx: px, TotalSz: qty, Fee: 0.02, OID: "venue-fixed"}}}, nil
	}
}

func okxGoldenOutcome(outcome, msg string) okxGoldenVenue {
	return func(call okxGoldenSendCall) (*OKXExecuteResult, error) {
		return &OKXExecuteResult{OrderOutcome: outcome, Error: msg, ClientOrderID: call.Opts.ClientOrderID}, nil
	}
}

func okxGoldenTransportError(msg string) okxGoldenVenue {
	return func(call okxGoldenSendCall) (*OKXExecuteResult, error) {
		return nil, errors.New(msg)
	}
}

type okxGoldenInputs struct {
	PosCtx           PositionCtx
	PosQty           float64
	PosSide          string
	AvgCost          float64
	Leverage         float64
	Cash             float64
	PoolBalanceKnown bool
	CashReconcile    bool
}

func (h *okxGoldenHarness) captureInputs(prices map[string]float64) okxGoldenInputs {
	sc, cfg, state := h.sc, h.env.cfg, h.env.state
	stratState := state.Strategies[sc.ID]
	var in okxGoldenInputs
	h.env.mu.RLock()
	defer h.env.mu.RUnlock()
	if okxIsLive(sc.Args) {
		if sc.Type == "perps" {
			if poolCash, pooled, balanceKnown := sharedWalletPoolAvailableMargin(sc, cfg.Strategies, state, prices, nil, nil); pooled {
				in.Cash = poolCash
				in.PoolBalanceKnown = balanceKnown
			} else {
				in.Cash = stratState.Cash
			}
		} else {
			in.Cash = stratState.Cash
		}
		in.CashReconcile = stratState.CashReconcileRequired
	}
	if sym := okxSymbol(sc.Args); sym != "" {
		if pos, ok := stratState.Positions[sym]; ok {
			in.PosCtx = positionCtxForCheck(sc, pos, cfg.Regime)
			in.PosSide = in.PosCtx.Side
			in.PosQty = in.PosCtx.Quantity
			in.AvgCost = in.PosCtx.AvgCost
			in.Leverage = pos.Leverage
		}
	}
	return in
}

func (h *okxGoldenHarness) save(fail bool) []string {
	if fail {
		prev := storeCommitHook
		storeCommitHook = func(role storageRole) error { return errors.New("injected cycle save failure") }
		defer func() { storeCommitHook = prev }()
	}
	h.env.mu.Lock()
	defer h.env.mu.Unlock()
	var out []string
	errs := h.env.store.SaveAll(h.env.state)
	parts := make([]string, 0, len(errs))
	byLabel := map[string]error{}
	for p, err := range errs {
		parts = append(parts, partitionLabel(p))
		byLabel[partitionLabel(p)] = err
	}
	sort.Strings(parts)
	for _, p := range parts {
		if byLabel[p] != nil {
			out = append(out, p+": "+byLabel[p].Error())
		} else {
			out = append(out, p+": ok")
		}
	}
	return out
}

func (h *okxGoldenHarness) execSQL(q string) {
	h.t.Helper()
	if _, err := h.env.fileFor(h.sc.ID).db.Exec(q); err != nil {
		h.t.Fatalf("%s: %v", q, err)
	}
}

func (h *okxGoldenHarness) dumpTables(names []string) map[string][]map[string]string {
	h.t.Helper()
	db := h.env.fileFor(h.sc.ID).db
	if names == nil {
		rows, err := db.Query(`SELECT name FROM sqlite_master WHERE type = 'table' AND name NOT LIKE 'sqlite_%' ORDER BY name`)
		if err != nil {
			h.t.Fatal(err)
		}
		for rows.Next() {
			var n string
			if err := rows.Scan(&n); err != nil {
				h.t.Fatal(err)
			}
			names = append(names, n)
		}
		rows.Close()
	}
	out := map[string][]map[string]string{}
	for _, name := range names {
		rows, err := db.Query(`SELECT * FROM "` + name + `"`)
		if err != nil {
			h.t.Fatalf("dump %s: %v", name, err)
		}
		cols, _ := rows.Columns()
		var table []map[string]string
		for rows.Next() {
			vals := make([]interface{}, len(cols))
			ptrs := make([]interface{}, len(cols))
			for i := range vals {
				ptrs[i] = &vals[i]
			}
			if err := rows.Scan(ptrs...); err != nil {
				h.t.Fatal(err)
			}
			row := map[string]string{}
			for i, c := range cols {
				switch v := vals[i].(type) {
				case nil:
					row[c] = "<null>"
				case []byte:
					row[c] = string(v)
				default:
					row[c] = fmt.Sprint(v)
				}
			}
			table = append(table, row)
		}
		rows.Close()
		if len(table) == 0 {
			continue
		}
		sort.SliceStable(table, func(i, j int) bool { return okxGoldenRowKey(table[i]) < okxGoldenRowKey(table[j]) })
		out[name] = table
	}
	return out
}

func okxGoldenRowKey(row map[string]string) string {
	keys := make([]string, 0, len(row))
	for k := range row {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	for _, k := range keys {
		b.WriteString(k + "=" + row[k] + ";")
	}
	return b.String()
}

func (h *okxGoldenHarness) book() interface{} {
	blob, err := json.Marshal(h.bookLive())
	if err != nil {
		h.t.Fatal(err)
	}
	var out interface{}
	if err := json.Unmarshal(blob, &out); err != nil {
		h.t.Fatal(err)
	}
	return out
}

func (h *okxGoldenHarness) bookLive() map[string]interface{} {
	h.env.mu.RLock()
	defer h.env.mu.RUnlock()
	s := h.env.state.Strategies[h.sc.ID]
	intents := []map[string]interface{}{}
	if s.EntryGuard != nil {
		ids := make([]string, 0, len(s.EntryGuard.Intents))
		for id := range s.EntryGuard.Intents {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		for _, id := range ids {
			in := s.EntryGuard.Intents[id]
			row := map[string]interface{}{"intent_id": in.IntentID, "client_order_id": in.ClientOrderID, "status": in.Status,
				"target_side": in.TargetSide, "bar_open_ms": in.BarOpenMs, "hold_reason": in.HoldReason, "request": in.RequestJSON}
			var req map[string]interface{}
			if json.Unmarshal([]byte(in.RequestJSON), &req) == nil {
				sub, _ := req["submitted_at_ms"].(float64)
				exp, _ := req["expires_after_ms"].(float64)
				if sub > 0 && exp > 0 {
					row["expires_minus_submitted_ms"] = int64(exp - sub)
				}
			}
			intents = append(intents, row)
		}
	}
	return map[string]interface{}{
		"cash": s.Cash, "positions": s.Positions, "trade_history": s.TradeHistory, "risk_state": s.RiskState,
		"cash_reconcile_required": s.CashReconcileRequired, "hurst_gate_state": s.HurstGate, "regime": s.Regime,
		"memory_intents": intents,
	}
}

func (h *okxGoldenHarness) argv() [][]string {
	blob, err := os.ReadFile(h.argvLog)
	if err != nil {
		return nil
	}
	var out [][]string
	sc := bufio.NewScanner(bytes.NewReader(blob))
	sc.Buffer(make([]byte, 1<<20), 1<<24)
	for sc.Scan() {
		var argv []string
		if json.Unmarshal(sc.Bytes(), &argv) == nil {
			out = append(out, argv)
		}
	}
	return out
}

func (h *okxGoldenHarness) logLines() []string {
	text := h.logBuf.String()
	h.logBuf.Reset()
	var out []string
	for _, line := range strings.Split(strings.TrimRight(text, "\n"), "\n") {
		if line != "" {
			out = append(out, line)
		}
	}
	return out
}

func (h *okxGoldenHarness) tradeTail(trades int, detail string) []map[string]string {
	if trades > 0 && detail != "" {
		sendTradeAlerts(h.sc, h.env.state.Strategies[h.sc.ID], trades, &h.env.mu, h.notifier, h.env.cfg.Regime)
	}
	return h.rec.drain()
}

func okxGoldenRun(t *testing.T, sc okxGoldenScenario, typ string, dispatch okxGoldenDispatchFunc) (map[string]interface{}, []string, int64, int64) {
	t.Helper()
	strat := StrategyConfig{ID: "okx-" + typ + "-" + sc.Name, Type: typ, Platform: "okx", Script: okxGoldenCheckScript,
		Args:      []string{"fixture", "BTC", "1h", "--mode=" + sc.Mode, "--inst-type=" + map[string]string{"spot": "spot", "perps": "swap"}[typ]},
		Direction: DirectionLong, Capital: 1000, Leverage: 1, SizingLeverage: 1}
	if sc.Config != nil {
		sc.Config(&strat)
	}
	env := openEntryTestEnv(t, newEntryTestConfig(t, strat))
	useEntryTradeRecorder(t, env.store)
	dir := t.TempDir()
	rec := &okxGoldenRecNotifier{}
	h := &okxGoldenHarness{t: t, env: env, sc: strat, rec: rec,
		notifier: NewMultiNotifier(notifierBackend{notifier: rec, channels: map[string]string{"okx": "chan-okx"}, ownerID: "owner-1"}),
		logBuf:   &bytes.Buffer{}, dir: dir, spec: filepath.Join(dir, "spec.json"), argvLog: filepath.Join(dir, "argv.jsonl")}
	h.logger = &StrategyLogger{stratID: strat.ID, writer: h.logBuf}
	bar, cutoff := okxGoldenTimes()
	h.barMs, h.cutoffMs = bar.UnixMilli(), cutoff.UnixMilli()
	part := partitionFor(strat)
	h.sr = &scopeCycleRisk{Partition: part, Config: partitionRiskConfig(env.cfg, part)}
	t.Setenv("OKX_STEP_CHECK_FIXTURE", h.spec)
	h.installVenue()
	var steps []interface{}
	var allLogs []string
	for _, st := range sc.Steps {
		h.sr = &scopeCycleRisk{Partition: part, Config: partitionRiskConfig(env.cfg, part)}
		if st.Before != nil {
			st.Before(h)
		}
		h.venue = append([]okxGoldenVenue(nil), st.Venue...)
		h.sends = nil
		h.writeSpec(st.Check)
		prices := map[string]float64{}
		for k, v := range st.Prices {
			prices[k] = v
		}
		persistenceHold := env.store.persistenceHoldsPartition(part)
		in := h.captureInputs(prices)
		trades, detail := dispatch(h, prices, in, persistenceHold)
		stepAlerts := h.rec.drain()
		tail := h.tradeTail(trades, detail)
		saves := h.save(st.FailSave)
		logs := h.logLines()
		allLogs = append(allLogs, logs...)
		globalSpotCashReconcileReminder.mu.Lock()
		reminder := globalSpotCashReconcileReminder.lastSig
		globalSpotCashReconcileReminder.mu.Unlock()
		steps = append(steps, map[string]interface{}{
			"step": st.Name, "persistence_hold_before": persistenceHold, "inputs": in,
			"trades": trades, "detail": detail, "check_argv": h.argv(), "log": logs,
			"sends": h.sends, "alerts": stepAlerts, "trade_alert_tail": tail, "prices_after": prices,
			"cycle_save": saves, "reminder_sig": reminder, "book": h.book(),
			"db": h.dumpTables([]string{"trades", "entry_order_intents", "entry_decision_state"}),
		})
		if len(h.venue) != 0 {
			t.Fatalf("%s/%s step %s left %d scripted venue responses unused", sc.Name, typ, st.Name, len(h.venue))
		}
	}
	return map[string]interface{}{"scenario": sc.Name, "type": typ, "mode": sc.Mode, "strategy": strat, "steps": steps, "final_db": h.dumpTables(nil)}, allLogs, h.barMs, h.cutoffMs
}

type okxGoldenNormalizer struct {
	ids      map[string]string
	counters map[string]int
	times    map[string]string
}

var (
	okxGoldenRFC3339  = regexp.MustCompile(`\d{4}-\d{2}-\d{2}[T ]\d{2}:\d{2}:\d{2}(\.\d+)?(Z|[+-]\d{2}:?\d{2})?( [+-]\d{4} UTC)?`)
	okxGoldenMs       = regexp.MustCompile(`\b1\d{12}\b`)
	okxGoldenIntentID = regexp.MustCompile(`\bei-[0-9a-f]{24}\b`)
	okxGoldenNs       = regexp.MustCompile(`\b1\d{18}\b`)
	okxGoldenPid      = regexp.MustCompile(`pid=\d+`)
	okxGoldenLogStamp = regexp.MustCompile(`^\[\d{4}-\d{2}-\d{2} \d{2}:\d{2}:\d{2}\] `)
	okxGoldenHexID    = regexp.MustCompile(`\b[0-9a-f]{32}\b`)
)

func (n *okxGoldenNormalizer) label(kind, raw string) string {
	if raw == "" {
		return raw
	}
	if v, ok := n.ids[raw]; ok {
		return v
	}
	n.counters[kind]++
	v := fmt.Sprintf("<%s-%d>", kind, n.counters[kind])
	n.ids[raw] = v
	return v
}

func (n *okxGoldenNormalizer) collectIDs(v interface{}) {
	switch x := v.(type) {
	case map[string]interface{}:
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			if s, ok := x[k].(string); ok && s != "" {
				switch k {
				case "intent_id", "IntentID":
					n.label("intent", s)
				case "client_order_id", "ClientOrderID":
					n.label("cid", s)
				case "position_id", "TradePositionID", "PositionID":
					n.label("pos", s)
				}
			}
			n.collectIDs(x[k])
		}
	case []interface{}:
		for _, e := range x {
			n.collectIDs(e)
		}
	case string:
		for _, m := range okxGoldenIntentID.FindAllString(x, -1) {
			n.label("intent", m)
		}
	}
}

func (n *okxGoldenNormalizer) str(s string) string {
	s = okxGoldenLogStamp.ReplaceAllString(s, "")
	raws := make([]string, 0, len(n.ids))
	for raw := range n.ids {
		raws = append(raws, raw)
	}
	sort.Slice(raws, func(i, j int) bool {
		if len(raws[i]) != len(raws[j]) {
			return len(raws[i]) > len(raws[j])
		}
		return raws[i] < raws[j]
	})
	for _, raw := range raws {
		s = strings.ReplaceAll(s, raw, n.ids[raw])
	}
	timeKeys := make([]string, 0, len(n.times))
	for k := range n.times {
		timeKeys = append(timeKeys, k)
	}
	sort.Strings(timeKeys)
	for _, k := range timeKeys {
		s = strings.ReplaceAll(s, k, n.times[k])
	}
	s = okxGoldenRFC3339.ReplaceAllString(s, "<time>")
	s = okxGoldenNs.ReplaceAllString(s, "<ns>")
	s = okxGoldenMs.ReplaceAllString(s, "<ms>")
	s = okxGoldenPid.ReplaceAllString(s, "pid=<pid>")
	s = okxGoldenHexID.ReplaceAllString(s, "<hex>")
	return s
}

func (n *okxGoldenNormalizer) walk(v interface{}) interface{} {
	switch x := v.(type) {
	case map[string]interface{}:
		out := make(map[string]interface{}, len(x))
		for k, e := range x {
			out[k] = n.walk(e)
		}
		return out
	case []interface{}:
		out := make([]interface{}, len(x))
		for i, e := range x {
			out[i] = n.walk(e)
		}
		return out
	case string:
		return n.str(x)
	case float64:
		if x == math.Trunc(x) && math.Abs(x) >= 1e12 && math.Abs(x) < 1e13 {
			return n.str(strconv.FormatInt(int64(x), 10))
		}
		return x
	}
	return v
}

func okxGoldenNormalize(t *testing.T, raw map[string]interface{}, barMs, cutoffMs int64) []byte {
	t.Helper()
	blob, err := json.Marshal(raw)
	if err != nil {
		t.Fatal(err)
	}
	var generic interface{}
	dec := json.NewDecoder(bytes.NewReader(blob))
	if err := dec.Decode(&generic); err != nil {
		t.Fatal(err)
	}
	n := &okxGoldenNormalizer{ids: map[string]string{}, counters: map[string]int{}, times: map[string]string{}}
	n.collectIDs(generic)
	for label, ms := range map[string]int64{"bar": barMs, "boundary": barMs + entryTestHour, "cutoff": cutoffMs} {
		n.times[strconv.FormatInt(ms, 10)] = "<" + label + ">"
		n.times[time.UnixMilli(ms).UTC().Format(time.RFC3339)] = "<" + label + "-rfc3339>"
	}
	out, err := json.MarshalIndent(n.walk(generic), "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	return append(out, '\n')
}

type okxGoldenDispatchFunc func(h *okxGoldenHarness, prices map[string]float64, in okxGoldenInputs, persistenceHold bool) (int, string)

func okxGoldenSourceDigest(t *testing.T) string {
	t.Helper()
	blob, err := os.ReadFile(okxGoldenSourceFile)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(blob)
	return hex.EncodeToString(sum[:])
}

func okxGoldenResetTrackers(t *testing.T) {
	t.Helper()
	prevSF, prevSFT := scriptFailureTracker, scriptFailureTransientTracker
	prevHTF, prevHTFT := htfFilterTracker, htfFilterTransientTracker
	prevLive, prevReminder := liveExecThrottle, globalSpotCashReconcileReminder
	prevChanges := logChanges
	logChanges = &logChangeGate{}
	scriptFailureTracker, scriptFailureTransientTracker = &ScriptFailureTracker{}, &ScriptFailureTracker{}
	htfFilterTracker, htfFilterTransientTracker = &ScriptFailureTracker{}, &ScriptFailureTracker{}
	liveExecThrottle, globalSpotCashReconcileReminder = &LiveExecFailureThrottle{}, &spotCashReconcileReminderTracker{}
	t.Cleanup(func() {
		scriptFailureTracker, scriptFailureTransientTracker = prevSF, prevSFT
		htfFilterTracker, htfFilterTransientTracker = prevHTF, prevHTFT
		liveExecThrottle, globalSpotCashReconcileReminder = prevLive, prevReminder
		logChanges = prevChanges
	})
}

func okxGoldenFileName(scenario, typ string) string {
	return filepath.Join(okxGoldenDir, scenario+"."+typ+".json")
}

func TestOKXSharedStepOrdinaryGolden(t *testing.T) {
	root := basketPyRoot(t)
	schedDir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	capture := os.Getenv(okxGoldenCaptureEnv) == "1"
	manifestPath := filepath.Join(schedDir, okxGoldenManifest)
	digest := okxGoldenSourceDigest(t)
	var manifest okxGoldenManifestFile
	blob, readErr := os.ReadFile(manifestPath)
	switch {
	case readErr == nil:
		if err := json.Unmarshal(blob, &manifest); err != nil {
			t.Fatalf("golden manifest: %v", err)
		}
		if manifest.ProjectionVersion != okxGoldenProjectionVersion {
			t.Fatalf("golden projection version %d, the fixture projects version %d", manifest.ProjectionVersion, okxGoldenProjectionVersion)
		}
		if capture && manifest.SourceSHA256 != digest {
			t.Fatalf("refusing to recapture: %s no longer matches the pre-extraction source %s (sha256 %s) recorded in the manifest; the golden traces are the baseline for the extracted owner", okxGoldenSourceFile, manifest.SourceCommit, manifest.SourceSHA256)
		}
	case errors.Is(readErr, os.ErrNotExist) && capture:
		src, err := os.ReadFile(okxGoldenSourceFile)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(src), "func runOKXStep(") {
			t.Fatalf("refusing to capture a first baseline from the extracted owner; capture runs only on the moved original branches")
		}
	default:
		t.Fatalf("golden manifest %s is missing or unreadable (%v); the comparison never skips", manifestPath, readErr)
	}
	t.Setenv("OKX_API_KEY", "synthetic-okx-step-fixture-key")
	t.Setenv("OKX_API_SECRET", "")
	t.Setenv("OKX_PASSPHRASE", "")
	t.Setenv("OKX_SANDBOX", "")
	deny := &okxGoldenDenyTransport{}
	prevTransport := http.DefaultTransport
	http.DefaultTransport = deny
	t.Cleanup(func() { http.DefaultTransport = prevTransport })
	t.Chdir(root)
	var names []string
	for _, sc := range okxGoldenScenarios() {
		for _, typ := range sc.Types {
			name := sc.Name + "." + typ
			names = append(names, name)
			t.Run(name, func(t *testing.T) {
				okxGoldenResetTrackers(t)
				raw, logs, barMs, cutoffMs := okxGoldenRun(t, sc, typ, okxGoldenDispatch)
				joined := strings.Join(logs, "\n")
				for _, want := range append(sc.Expect["*"], sc.Expect[typ]...) {
					if !strings.Contains(joined, want) {
						t.Errorf("%s: the scenario never reached %q; its logs:\n%s", name, want, joined)
					}
				}
				got := okxGoldenNormalize(t, raw, barMs, cutoffMs)
				path := filepath.Join(schedDir, okxGoldenFileName(sc.Name, typ))
				if capture {
					if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(path, got, 0o644); err != nil {
						t.Fatal(err)
					}
					return
				}
				want, err := os.ReadFile(path)
				if err != nil {
					t.Fatalf("golden trace %s is missing (%v); the comparison never skips", path, err)
				}
				if !bytes.Equal(got, want) {
					diffPath := filepath.Join(t.TempDir(), filepath.Base(path))
					_ = os.WriteFile(diffPath, got, 0o644)
					t.Fatalf("%s differs from the pre-extraction golden trace %s; the new projection is at %s", name, path, diffPath)
				}
			})
		}
	}
	if n := deny.attempts.Load(); n != 0 {
		t.Fatalf("the ordinary OKX path attempted %d Go HTTP request(s)", n)
	}
	sort.Strings(names)
	if capture {
		if readErr == nil || t.Failed() {
			return
		}
		m := okxGoldenManifestFile{ProjectionVersion: okxGoldenProjectionVersion, BaseCommit: os.Getenv("OKX_STEP_CAPTURE_BASE"), SourceCommit: os.Getenv("OKX_STEP_CAPTURE_SOURCE_COMMIT"),
			SourceFile: "scheduler/" + okxGoldenSourceFile, SourceSHA256: digest,
			CaptureInvocation: "OKX_STEP_CAPTURE_GOLDEN=1 OKX_STEP_CAPTURE_BASE=<base> OKX_STEP_CAPTURE_SOURCE_COMMIT=<commit> go -C scheduler test -tags pyintegration -run '^TestOKXSharedStepOrdinaryGolden$' -count=1 ./...",
			Scenarios:         names}
		if m.BaseCommit == "" || m.SourceCommit == "" {
			t.Fatal("a first capture must name OKX_STEP_CAPTURE_BASE and OKX_STEP_CAPTURE_SOURCE_COMMIT")
		}
		out, _ := json.MarshalIndent(m, "", "  ")
		if err := os.WriteFile(manifestPath, append(out, '\n'), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	if strings.Join(manifest.Scenarios, ",") != strings.Join(names, ",") {
		t.Fatalf("the golden manifest lists %v, the fixture runs %v", manifest.Scenarios, names)
	}
}

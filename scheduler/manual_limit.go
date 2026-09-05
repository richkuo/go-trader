package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const limitFillEpsilon = 1e-9

type HyperliquidLimitOpenResult struct {
	Platform   string  `json:"platform"`
	Timestamp  string  `json:"timestamp"`
	Error      string  `json:"error,omitempty"`
	Status     string  `json:"status,omitempty"`
	OrderOID   int64   `json:"order_oid,omitempty"`
	LimitPrice float64 `json:"limit_price,omitempty"`
	TIF        string  `json:"tif,omitempty"`
}

type HyperliquidLimitOrderStatus struct {
	OID        int64   `json:"oid"`
	Resting    *bool   `json:"resting"`
	FilledSize float64 `json:"filled_size"`
	AvgPx      float64 `json:"avg_px"`
	Fee        float64 `json:"fee"`
	Count      int     `json:"count"`
	FillsError string  `json:"fills_error,omitempty"`
}

type HyperliquidLimitStatusResult struct {
	Platform        string                        `json:"platform"`
	Timestamp       string                        `json:"timestamp"`
	Error           string                        `json:"error,omitempty"`
	Orders          []HyperliquidLimitOrderStatus `json:"orders"`
	OpenOrdersError string                        `json:"open_orders_error,omitempty"`
}

type HyperliquidCancelOrderResult struct {
	Platform    string `json:"platform"`
	Timestamp   string `json:"timestamp"`
	Error       string `json:"error,omitempty"`
	OID         int64  `json:"oid"`
	Cancelled   bool   `json:"cancelled"`
	CancelError string `json:"cancel_error,omitempty"`
}

func buildHyperliquidLimitOpenArgs(symbol, side string, size, limitPx float64, tif, marginMode string, leverage float64, snapshot hlExecuteSnapshot) []string {
	if tif == "" {
		tif = "Alo"
	}
	args := []string{
		"--limit-open",
		fmt.Sprintf("--symbol=%s", symbol),
		fmt.Sprintf("--side=%s", side),
		fmt.Sprintf("--size=%g", size),
		fmt.Sprintf("--limit-price=%g", limitPx),
		fmt.Sprintf("--tif=%s", tif),
		"--mode=live",
	}
	if marginMode != "" {
		args = append(args, fmt.Sprintf("--margin-mode=%s", marginMode))
		if leverage > 0 {
			args = append(args, fmt.Sprintf("--leverage=%g", leverage))
		}
		if snapshot.AccountLeverage > 0 && (snapshot.AccountMarginMode == "isolated" || snapshot.AccountMarginMode == "cross") {
			args = append(args, fmt.Sprintf("--account-leverage=%d", snapshot.AccountLeverage))
			args = append(args, fmt.Sprintf("--account-margin-mode=%s", snapshot.AccountMarginMode))
		}
	}
	return args
}

func parseHyperliquidLimitOpenOutput(stdout []byte, stderrStr string, runErr error) (*HyperliquidLimitOpenResult, string, error) {
	var result HyperliquidLimitOpenResult
	if jsonErr := json.Unmarshal(stdout, &result); jsonErr != nil {
		if runErr != nil {
			return nil, stderrStr, fmt.Errorf("limit-open error: %w (stderr: %s; stdout: %s)", runErr, stderrStr, string(stdout))
		}
		return nil, stderrStr, fmt.Errorf("parse output: %w (stdout: %s)", jsonErr, string(stdout))
	}
	return &result, stderrStr, nil
}

func RunHyperliquidLimitOpen(script, symbol, side string, size, limitPx float64, tif, marginMode string, leverage float64, snapshot hlExecuteSnapshot) (*HyperliquidLimitOpenResult, string, error) {
	args := buildHyperliquidLimitOpenArgs(symbol, side, size, limitPx, tif, marginMode, leverage, snapshot)
	stdout, stderr, err := runPythonSideEffect(script, args)
	return parseHyperliquidLimitOpenOutput(stdout, string(stderr), err)
}

func parseHyperliquidLimitStatusOutput(stdout []byte, stderrStr string, runErr error) (*HyperliquidLimitStatusResult, string, error) {
	var result HyperliquidLimitStatusResult
	if jsonErr := json.Unmarshal(stdout, &result); jsonErr != nil {
		if runErr != nil {
			return nil, stderrStr, fmt.Errorf("limit-status error: %w (stderr: %s; stdout: %s)", runErr, stderrStr, string(stdout))
		}
		return nil, stderrStr, fmt.Errorf("parse output: %w (stdout: %s)", jsonErr, string(stdout))
	}
	return &result, stderrStr, nil
}

func RunHyperliquidLimitStatus(script, symbol string, oids []int64, sinceMs int64) (*HyperliquidLimitStatusResult, string, error) {
	oidsJSON, err := json.Marshal(oids)
	if err != nil {
		return nil, "", fmt.Errorf("marshal oids: %w", err)
	}
	args := []string{
		"--limit-status",
		fmt.Sprintf("--symbol=%s", symbol),
		fmt.Sprintf("--oids-json=%s", string(oidsJSON)),
		"--mode=live",
	}
	if sinceMs > 0 {
		args = append(args, fmt.Sprintf("--since-ms=%d", sinceMs))
	}
	stdout, stderr, runErr := runPythonSideEffect(script, args)
	return parseHyperliquidLimitStatusOutput(stdout, string(stderr), runErr)
}

func limitStatusSinceMs(createdAt time.Time) int64 {
	if createdAt.IsZero() {
		return 0
	}
	ms := createdAt.Add(-60 * time.Second).UnixMilli()
	if ms < 0 {
		return 0
	}
	return ms
}

func parseHyperliquidCancelOrderOutput(stdout []byte, stderrStr string, runErr error) (*HyperliquidCancelOrderResult, string, error) {
	var result HyperliquidCancelOrderResult
	if jsonErr := json.Unmarshal(stdout, &result); jsonErr != nil {
		if runErr != nil {
			return nil, stderrStr, fmt.Errorf("cancel-order error: %w (stderr: %s; stdout: %s)", runErr, stderrStr, string(stdout))
		}
		return nil, stderrStr, fmt.Errorf("parse output: %w (stdout: %s)", jsonErr, string(stdout))
	}
	return &result, stderrStr, nil
}

func RunHyperliquidCancelOrder(script, symbol string, oid int64) (*HyperliquidCancelOrderResult, string, error) {
	args := []string{
		"--cancel-order",
		fmt.Sprintf("--symbol=%s", symbol),
		fmt.Sprintf("--oid=%d", oid),
		"--mode=live",
	}
	stdout, stderr, runErr := runPythonSideEffect(script, args)
	return parseHyperliquidCancelOrderOutput(stdout, string(stderr), runErr)
}

var (
	runHyperliquidLimitOpenFn   = RunHyperliquidLimitOpen
	runHyperliquidLimitStatusFn = RunHyperliquidLimitStatus
	runHyperliquidCancelOrderFn = RunHyperliquidCancelOrder
)

type manualLimitOpenInputs struct {
	strategyID  string
	side        string
	openSide    string
	tif         string
	size        float64
	notional    float64
	margin      float64
	limitPrice  float64
	atr         float64
	slATRMult   float64
	slPct       float64
	expireAfter time.Duration
	dryRun      bool
}

func runManualLimitOpen(cfg *Config, sc StrategyConfig, stateDB *StateStore, in manualLimitOpenInputs) int {
	size := resolveManualSize(in.size, in.notional, in.margin, in.limitPrice, sc.Leverage)
	if size <= 0 {
		fmt.Fprintf(os.Stderr, "error: resolved size is zero (size=%g notional=%g margin=%g limit=%g leverage=%g)\n",
			in.size, in.notional, in.margin, in.limitPrice, sc.Leverage)
		return 1
	}

	if in.dryRun {
		exp := "none"
		if in.expireAfter > 0 {
			exp = in.expireAfter.String()
		}
		fmt.Printf("[dry-run] manual-open %s: LIMIT %s %.6f %s @ $%.4f tif=%s expire=%s\n",
			in.strategyID, in.side, size, sc.Symbol, in.limitPrice, in.tif, exp)
		return 0
	}

	if in.slATRMult > 0 || in.slPct > 0 {
		fmt.Fprintln(os.Stderr, "warning: --stop-loss-atr-mult / --stop-loss-pct are ignored for --limit-price orders; the scheduler arms SL/TP from strategy config after the fill")
	}

	unlock, lockErr := stateDB.manualActionLock(in.strategyID)
	if lockErr != nil {
		fmt.Fprintf(os.Stderr, "error: %v — refusing to avoid double-firing an on-chain order\n", lockErr)
		return 1
	}
	defer unlock()

	state, _, loadErr := LoadStateWithStore(cfg, stateDB)
	if loadErr != nil {
		fmt.Fprintf(os.Stderr, "error: could not load state for placement guard: %v\n", loadErr)
		return 1
	}
	limitScope := portfolioScopeFor(sc)
	if state.scopeLatched(limitScope) {
		fmt.Fprintf(os.Stderr, "error: portfolio kill switch is active for the %s scope — manual-open blocked (use manual-close to flatten)\n", scopeLabel(limitScope))
		return 1
	}
	if ss := state.Strategies[in.strategyID]; ss != nil {
		if ss.RiskState.getPendingCircuitClose(PlatformPendingCloseHyperliquid) != nil {
			fmt.Fprintln(os.Stderr, "error: strategy has a pending circuit-breaker close — manual-open blocked")
			return 1
		}
		if pos := ss.Positions[sc.Symbol]; pos != nil {
			fmt.Fprintf(os.Stderr, "error: %s already has an open position for %s — close it before placing a limit order\n", in.strategyID, sc.Symbol)
			return 1
		}
	}
	if err := refuseIfPendingManualPositionAction(stateDB, "manual-open --limit-price", in.strategyID, sc.Symbol); err != nil {
		fmt.Fprintln(os.Stderr, err.Error())
		return 1
	}
	if err := refuseIfRestingLimitOrderQueued(stateDB, "manual-open --limit-price", in.strategyID, sc.Symbol); err != nil {
		fmt.Fprintln(os.Stderr, err.Error())
		return 1
	}

	res, stderr, err := runHyperliquidLimitOpenFn(sc.Script, sc.Symbol, in.openSide, size, in.limitPrice, in.tif, sc.MarginMode, sc.Leverage, hlExecuteSnapshot{})
	if stderr != "" {
		fmt.Fprintf(os.Stderr, "HL limit-open stderr: %s\n", stderr)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "error placing limit order: %v\n", err)
		return 1
	}
	if res == nil || res.Error != "" || res.Status == "error" {
		msg := "unknown error"
		if res != nil && res.Error != "" {
			msg = res.Error
		}
		fmt.Fprintf(os.Stderr, "error from HL: %s\n", msg)
		return 1
	}
	if res.OrderOID == 0 {
		fmt.Fprintf(os.Stderr, "error: HL returned no order OID (status=%s) — cannot track the resting order\n", res.Status)
		return 1
	}

	var expiresAt time.Time
	if in.expireAfter > 0 {
		expiresAt = time.Now().UTC().Add(in.expireAfter)
	}
	row := PendingLimitOrder{
		StrategyID: in.strategyID,
		Symbol:     sc.Symbol,
		Side:       in.side,
		OrderOID:   res.OrderOID,
		LimitPrice: in.limitPrice,
		OrderSize:  size,
		TIF:        in.tif,
		EntryATR:   in.atr,
		ExpiresAt:  expiresAt,
		CreatedAt:  time.Now().UTC(),
	}
	if _, err := stateDB.InsertPendingLimitOrder(row); err != nil {
		fmt.Fprintf(os.Stderr, "CRITICAL: limit order placed (oid=%d) but DB insert failed (%v); attempting cancel...\n", res.OrderOID, err)
		if _, cstderr, cerr := RunHyperliquidCancelOrder(sc.Script, sc.Symbol, res.OrderOID); cerr != nil {
			fmt.Fprintf(os.Stderr, "cancel ALSO failed (%v, stderr=%s) — MANUAL INTERVENTION REQUIRED: cancel oid=%d on the HL UI\n", cerr, cstderr, res.OrderOID)
		} else {
			fmt.Fprintln(os.Stderr, "resting order cancelled.")
		}
		return 1
	}

	statusNote := ""
	if res.Status == "filled" {
		statusNote = " (price was marketable — filled at submit; the scheduler will adopt the fill next cycle)"
	}
	fmt.Printf("Resting limit order placed: %s %.6f %s @ $%.4f tif=%s (oid=%d)%s\n",
		in.side, size, sc.Symbol, in.limitPrice, in.tif, res.OrderOID, statusNote)
	fmt.Printf("The scheduler will track fills and arm protection automatically. Cancel with: go-trader manual-cancel %s\n", in.strategyID)
	return 0
}

func runManualCancel(args []string) int {
	fs := flag.NewFlagSet("manual-cancel", flag.ContinueOnError)
	configPath := fs.String("config", "scheduler/config.json", "Path to config file")

	args = reorderArgsForPositional(args, collectBoolFlagNames(fs))
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 1 {
		fmt.Fprintln(os.Stderr, "Usage: go-trader manual-cancel <strategy-id>")
		return 2
	}
	strategyID := fs.Arg(0)

	cfg, err := LoadConfig(*configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to load config: %v\n", err)
		return 1
	}
	sc, ok := findManualStrategy(cfg, strategyID)
	if !ok {
		return 1
	}

	stateDB, err := openToolStateStore(cfg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to open state DB: %v\n", err)
		return 1
	}
	defer stateDB.Close()

	n, err := stateDB.MarkPendingLimitOrderCancelRequested(strategyID, sc.Symbol)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error queuing cancellation: %v\n", err)
		return 1
	}
	if n == 0 {
		fmt.Fprintf(os.Stderr, "no resting limit order found for %s/%s\n", strategyID, sc.Symbol)
		return 1
	}
	fmt.Printf("Cancellation queued for %s/%s (%d order(s)); the scheduler will cancel on-chain and finalize on its next cycle.\n",
		strategyID, sc.Symbol, n)
	return 0
}

func findPendingLimitOrderByOID(orders []PendingLimitOrder, oid int64) (PendingLimitOrder, bool) {
	for _, o := range orders {
		if o.OrderOID == oid {
			return o, true
		}
	}
	return PendingLimitOrder{}, false
}

func clearOperatorRequiredLimitRowRefusal(cfg *Config, o PendingLimitOrder, flattened bool) string {
	sc, known := scByIDLookup(cfg, o.StrategyID)
	block := killSwitchLimitOrderAdoptionBlock(sc, known)
	if o.OperatorRequiredSince.IsZero() {
		if block == "" {
			return fmt.Sprintf("refusing: %s is a Hyperliquid-live type=manual strategy and the reconciler has NOT marked oid=%d as needing an operator, so the scheduler adopts this fill itself — clearing the row would orphan it; let the reconciler converge instead",
				o.StrategyID, o.OrderOID)
		}
		return fmt.Sprintf("refusing: the reconciler has not classified oid=%d as needing an operator — it is still converging on its own, and clearing the row now would discard a live recovery record",
			o.OrderOID)
	}
	if !flattened {
		return fmt.Sprintf("refusing: pass --flattened to assert that you have confirmed on the Hyperliquid UI that the %s position from oid=%d is closed — the scheduler cannot verify a flatten for a strategy it does not own",
			o.Symbol, o.OrderOID)
	}
	return ""
}

func scByIDLookup(cfg *Config, id string) (StrategyConfig, bool) {
	for _, sc := range cfg.Strategies {
		if sc.ID == id {
			return sc, true
		}
	}
	return StrategyConfig{}, false
}

func runManualClearLimitRow(args []string) int {
	fs := flag.NewFlagSet("manual-clear-limit-row", flag.ContinueOnError)
	configPath := fs.String("config", "scheduler/config.json", "Path to config file")
	flattened := fs.Bool("flattened", false, "Assert you confirmed on the Hyperliquid UI that the position from this order is closed")
	dryRun := fs.Bool("dry-run", false, "Print the record that would be discarded without deleting it")

	args = reorderArgsForPositional(args, collectBoolFlagNames(fs))
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 1 {
		fmt.Fprintln(os.Stderr, "Usage: go-trader manual-clear-limit-row <order-oid> --flattened [--dry-run]")
		return 2
	}
	oid, convErr := strconv.ParseInt(fs.Arg(0), 10, 64)
	if convErr != nil || oid <= 0 {
		fmt.Fprintf(os.Stderr, "error: <order-oid> must be a positive integer, got %q\n", fs.Arg(0))
		return 2
	}

	cfg, err := LoadConfig(*configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to load config: %v\n", err)
		return 1
	}
	stateDB, err := openToolStateStore(cfg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to open state DB: %v\n", err)
		return 1
	}
	defer stateDB.Close()

	unlock, lockErr := acquireManualActionFileLock(cfg.DBFile)
	if lockErr != nil {
		fmt.Fprintf(os.Stderr, "error: %v — refusing to discard a recovery record under a concurrent manual action\n", lockErr)
		return 1
	}
	defer unlock()

	orders, loadErr := stateDB.LoadPendingLimitOrders()
	if loadErr != nil {
		fmt.Fprintf(os.Stderr, "error loading the resting limit order queue: %v\n", loadErr)
		return 1
	}
	o, found := findPendingLimitOrderByOID(orders, oid)
	if !found {
		fmt.Fprintf(os.Stderr, "no queued limit order with oid=%d — nothing to clear\n", oid)
		return 1
	}

	fmt.Printf("Queue row %d: %s/%s %s %.6f @ $%.4f (oid=%d), tracked fill %.6f, operator-required since %s\n",
		o.ID, o.StrategyID, o.Symbol, o.Side, o.OrderSize, o.LimitPrice, o.OrderOID, o.FilledSize,
		limitRowOperatorRequiredLabel(o))

	if refusal := clearOperatorRequiredLimitRowRefusal(cfg, o, *flattened); refusal != "" {
		fmt.Fprintln(os.Stderr, refusal)
		return 1
	}
	if *dryRun {
		fmt.Printf("[dry-run] would discard the recovery record for oid=%d; the unadopted fill is NOT booked and stays off the ledger\n", o.OrderOID)
		return 0
	}
	if err := stateDB.DeletePendingLimitOrder(o.ID); err != nil {
		fmt.Fprintf(os.Stderr, "error clearing queue row %d: %v\n", o.ID, err)
		return 1
	}
	fmt.Printf("Cleared queue row %d (oid=%d). Its unadopted fill was never booked, so it stays off the ledger — the scheduler stops alerting on it next cycle.\n",
		o.ID, o.OrderOID)
	return 0
}

func limitRowOperatorRequiredLabel(o PendingLimitOrder) string {
	if o.OperatorRequiredSince.IsZero() {
		return "never"
	}
	return o.OperatorRequiredSince.Format(time.RFC3339)
}

func limitOrderFullyFilled(cumFilled, orderSize float64) bool {
	tol := orderSize * 1e-6
	if tol < 1e-9 {
		tol = 1e-9
	}
	return cumFilled >= orderSize-tol
}

func resolveLimitFillEntryATR(sc StrategyConfig, cfg *Config, rowATR, avgPx float64, notifier *MultiNotifier) float64 {
	entryATR := rowATR
	if entryATR == 0 && (effectiveManualSLATRMult(sc) > 0 || strategyUsesTieredTPATRClose(sc)) {
		fetched, fetchErr, ok := fetchManualEntryATR(sc, cfg)
		if ok && !(avgPx > 0 && fetched > 0.5*avgPx) {
			entryATR = fetched
		} else {
			if ok {
				fetchErr = fmt.Sprintf("fetched ATR=%.6f exceeds 50%% of fill price %.4f", fetched, avgPx)
			}
			if fb, fbOK := computeFallbackATR(avgPx, sc.Leverage); fbOK {
				entryATR = fb
				warnNotifier(notifier, fmt.Sprintf(
					"[limit-fill] %s %s: ATR auto-fetch failed (%s); using fallback ATR=%.6f — pass --atr on manual-open for accuracy",
					sc.ID, sc.Symbol, fetchErr, fb))
			} else {
				warnNotifier(notifier, fmt.Sprintf(
					"[limit-fill] %s %s: ATR auto-fetch failed (%s) and leverage<=0 — position is NAKED (no ATR-based SL/TP)",
					sc.ID, sc.Symbol, fetchErr))
			}
		}
	}
	if entryATR > 0 && avgPx > 0 && entryATR > 0.5*avgPx {
		warnNotifier(notifier, fmt.Sprintf(
			"[limit-fill] %s %s: entry ATR %.6f exceeds 50%% of fill price %.4f — not stamping ATR",
			sc.ID, sc.Symbol, entryATR, avgPx))
		entryATR = 0
	}
	return entryATR
}

func effectiveManualSLATRMult(sc StrategyConfig) float64 {
	if sc.StopLossATRMult != nil {
		return *sc.StopLossATRMult
	}
	return 0
}

func applyLimitFillProgress(state *AppState, sc StrategyConfig, o PendingLimitOrder, cumFilled, avgPx, cumFee, entryATR float64, atrMethod string, now time.Time) (int, error) {
	ss := state.Strategies[o.StrategyID]
	if ss == nil {
		return 0, fmt.Errorf("strategy state for %q not found", o.StrategyID)
	}
	pos := ss.Positions[o.Symbol]

	if o.FilledSize == 0 {
		if pos != nil {
			return 0, fmt.Errorf("limit fill for %s/%s but a position already exists (owner=%q) — not adopting", o.StrategyID, o.Symbol, pos.OwnerStrategyID)
		}
		pos = &Position{
			Symbol:          o.Symbol,
			Quantity:        cumFilled,
			InitialQuantity: cumFilled,
			AvgCost:         avgPx,
			EntryATR:        entryATR,
			Side:            o.Side,
			Multiplier:      1,
			Leverage:        sc.Leverage,
			OwnerStrategyID: o.StrategyID,
			OpenedAt:        now,
			ATRMethodAtOpen: atrMethod,
		}
		pos.TradePositionID = newTradePositionID(o.StrategyID, o.Symbol, now)
		ss.Positions[o.Symbol] = pos

		trade := Trade{
			Timestamp:       now,
			StrategyID:      o.StrategyID,
			Symbol:          o.Symbol,
			Side:            openTradeSide(o.Side),
			Quantity:        cumFilled,
			Price:           avgPx,
			Value:           cumFilled * avgPx,
			TradeType:       "perps",
			Details:         fmt.Sprintf("manual limit open %s %s @ $%.4f (oid=%d)", o.Side, o.Symbol, avgPx, o.OrderOID),
			PositionID:      pos.TradePositionID,
			ExchangeOrderID: fmt.Sprintf("%d", o.OrderOID),
			ExchangeFee:     cumFee,
			FeeSource:       FeeSourceUserFills,
			PnLGross:        true,
			EntryATR:        entryATR,
			Manual:          true,
		}
		recordPositionOpen(ss, sc, &trade, pos)
		ss.Cash -= cumFee
		fmt.Printf("[limit] applied open: %s %s %.6f %s @ $%.4f (oid=%d)\n",
			o.StrategyID, o.Side, cumFilled, o.Symbol, avgPx, o.OrderOID)
		return 1, nil
	}

	if pos == nil {
		return 0, fmt.Errorf("limit partial fill for %s/%s but position is missing — not re-creating", o.StrategyID, o.Symbol)
	}
	if !manualPositionOwnedByStrategy(pos, o.StrategyID) {
		return 0, fmt.Errorf("limit partial fill for %s/%s but position owner=%q — not growing", o.StrategyID, o.Symbol, pos.OwnerStrategyID)
	}
	deltaQty := cumFilled - o.FilledSize
	deltaFee := cumFee - o.FillFee
	if deltaFee < 0 {
		deltaFee = 0
	}
	pos.Quantity = cumFilled
	pos.InitialQuantity = cumFilled
	pos.AvgCost = avgPx
	if entryATR > 0 && pos.EntryATR == 0 {
		pos.EntryATR = entryATR
	}

	trade := Trade{
		Timestamp:       now,
		StrategyID:      o.StrategyID,
		Symbol:          o.Symbol,
		Side:            openTradeSide(o.Side),
		Quantity:        deltaQty,
		Price:           avgPx,
		Value:           deltaQty * avgPx,
		TradeType:       scaleInTradeType,
		Details:         fmt.Sprintf("manual limit add %s %s %.6f @ $%.4f (oid=%d)", o.Side, o.Symbol, deltaQty, avgPx, o.OrderOID),
		PositionID:      ensurePositionTradeID(o.StrategyID, o.Symbol, pos),
		ExchangeOrderID: fmt.Sprintf("%d", o.OrderOID),
		ExchangeFee:     deltaFee,
		FeeSource:       FeeSourceUserFills,
		PnLGross:        true,
		EntryATR:        pos.EntryATR,
		Manual:          true,
	}
	RecordTrade(ss, trade)
	ss.Cash -= deltaFee
	fmt.Printf("[limit] applied partial add: %s %s +%.6f (cum=%.6f) %s @ $%.4f (oid=%d)\n",
		o.StrategyID, o.Side, deltaQty, cumFilled, o.Symbol, avgPx, o.OrderOID)
	return 1, nil
}

type orphanLimitCancelState int

const (
	orphanLimitStateUnknown orphanLimitCancelState = iota
	orphanLimitStateResting
	orphanLimitStateOffBookUnadoptedFill
	orphanLimitStateOffBookRowStuck
)

type orphanLimitCancelOutcome struct {
	Resolved     bool
	State        orphanLimitCancelState
	Reason       string
	CancelIssued bool
	AdoptedFill  float64
	ExchangeFill float64
}

func orphanLimitOrderStatus(script string, o PendingLimitOrder) (HyperliquidLimitOrderStatus, string) {
	res, stderr, err := runHyperliquidLimitStatusFn(script, o.Symbol, []int64{o.OrderOID}, limitStatusSinceMs(o.CreatedAt))
	if stderr != "" {
		fmt.Fprintf(os.Stderr, "[limit] %s orphan status stderr: %s\n", o.StrategyID, stderr)
	}
	if err != nil || res == nil || res.Error != "" {
		msg := ""
		if res != nil {
			msg = res.Error
		}
		return HyperliquidLimitOrderStatus{}, strings.TrimSpace(fmt.Sprintf("order state unreadable: %v %s", err, msg))
	}
	if res.OpenOrdersError != "" {
		return HyperliquidLimitOrderStatus{}, fmt.Sprintf("open-orders state unknown (%s)", res.OpenOrdersError)
	}
	st, ok := limitStatusForOID(res, o.OrderOID)
	if !ok {
		return HyperliquidLimitOrderStatus{}, "status response did not include the order"
	}
	if st.FillsError != "" {
		return HyperliquidLimitOrderStatus{}, fmt.Sprintf("fills unreadable (%s)", st.FillsError)
	}
	if st.Resting == nil {
		return HyperliquidLimitOrderStatus{}, "the exchange did not report whether the order is still resting"
	}
	return st, ""
}

func cancelOrphanedLimitOrder(state *AppState, cfg *Config, store *StateStore, mu *sync.RWMutex, o PendingLimitOrder) orphanLimitCancelOutcome {
	out := orphanLimitCancelOutcome{AdoptedFill: o.FilledSize}

	candidates, _ := collectKillSwitchLimitOrderCandidates(
		[]PendingLimitOrder{o}, killSwitchLimitOrderRoster(cfg.Strategies))
	if len(candidates) == 0 {
		out.State = orphanLimitStateUnknown
		out.Reason = "no Hyperliquid strategy with a script remains in this config to cancel it — its on-chain state was never read, so manual intervention is required"
		return out
	}
	script := candidates[0].Script

	st, reason := orphanLimitOrderStatus(script, o)
	if reason != "" {
		out.State = orphanLimitStateUnknown
		out.Reason = reason
		return out
	}

	if *st.Resting {
		cancelRes, cstderr, cerr := runHyperliquidCancelOrderFn(script, o.Symbol, o.OrderOID)
		if cstderr != "" {
			fmt.Fprintf(os.Stderr, "[limit] %s orphan cancel stderr: %s\n", o.StrategyID, cstderr)
		}
		if cerr != nil || cancelRes == nil || cancelRes.Error != "" {
			msg := ""
			if cancelRes != nil {
				msg = cancelRes.Error
			}
			out.State = orphanLimitStateResting
			out.Reason = strings.TrimSpace(fmt.Sprintf("cancel failed: %v %s", cerr, msg))
			return out
		}
		out.CancelIssued = true
		st, reason = orphanLimitOrderStatus(script, o)
		if reason != "" {
			out.State = orphanLimitStateUnknown
			out.Reason = "cancel issued but unverified: " + reason
			return out
		}
		if *st.Resting {
			out.State = orphanLimitStateResting
			out.Reason = "cancel issued but the order is still resting on the exchange"
			return out
		}
	}

	out.ExchangeFill = st.FilledSize
	if st.FilledSize > o.FilledSize+limitFillEpsilon {
		out.State = orphanLimitStateOffBookUnadoptedFill
		out.Reason = fmt.Sprintf(
			"the exchange reports a filled size of %.6f where the book holds %.6f, and NO automatic path can book the difference, so no fill was booked and the queue row is kept as the recovery record",
			st.FilledSize, o.FilledSize)
		return out
	}
	if o.FilledSize > 0 {
		mu.Lock()
		saveErr := SaveStateWithStore(state, store)
		mu.Unlock()
		if saveErr != nil {
			out.State = orphanLimitStateOffBookRowStuck
			out.Reason = fmt.Sprintf(
				"its already-booked fill of %.6f could not be flushed to the state DB (%v), so the queue row is kept as the recovery record",
				o.FilledSize, saveErr)
			return out
		}
	}
	if err := store.DeletePendingLimitOrder(o.ID); err != nil {
		out.State = orphanLimitStateOffBookRowStuck
		out.Reason = fmt.Sprintf("the queue row could not be cleared (%v)", err)
		return out
	}
	out.Resolved = true
	return out
}

func orphanLimitPollDeferred(o PendingLimitOrder, now time.Time) bool {
	if o.OperatorRequiredSince.IsZero() {
		return false
	}
	return now.Sub(o.OperatorRequiredSince) < effectiveAlertThrottleInterval()
}

func applyOrphanLimitOperatorRequired(store *StateStore, o PendingLimitOrder, outcome orphanLimitCancelOutcome, now time.Time) {
	if !outcome.Resolved && outcome.State == orphanLimitStateOffBookUnadoptedFill {
		if err := store.MarkPendingLimitOrderOperatorRequired(o.ID, now); err != nil {
			fmt.Printf("[limit] failed to persist the operator-required marker on row %d: %v\n", o.ID, err)
		}
		return
	}
	if !o.OperatorRequiredSince.IsZero() && !outcome.Resolved {
		if err := store.ClearPendingLimitOrderOperatorRequired(o.ID); err != nil {
			fmt.Printf("[limit] failed to clear the operator-required marker on row %d: %v\n", o.ID, err)
		}
	}
}

func reconcilePendingLimitOrders(state *AppState, cfg *Config, store *StateStore, mu *sync.RWMutex, notifier *MultiNotifier, logMgr *LogManager) []manualAlert {
	if store == nil {
		return nil
	}
	orders, err := store.LoadPendingLimitOrders()
	if err != nil {
		fmt.Printf("[limit] failed to load pending limit orders: %v\n", err)
		return nil
	}
	if len(orders) == 0 {
		return nil
	}

	scByID := make(map[string]StrategyConfig, len(cfg.Strategies))
	for _, sc := range cfg.Strategies {
		scByID[sc.ID] = sc
	}

	now := time.Now().UTC()
	orphanLimitCancelAlerts.Retain(orders)
	limitFillExposureAlerts.Retain(orders)
	exposureReader := &hlLiveExposureReader{}
	applied := make(map[string]*manualAlert)
	var order []string

	var candidates []*limitFillCandidate
	for _, o := range orders {
		sc, known := scByID[o.StrategyID]
		if block := killSwitchLimitOrderAdoptionBlock(sc, known); block != "" {
			expired := !o.ExpiresAt.IsZero() && now.After(o.ExpiresAt)
			if !o.CancelRequested && !expired {
				fmt.Printf("[limit] skipping row %d: %s (%q) and no cancellation is queued for it\n", o.ID, block, o.StrategyID)
				continue
			}
			if orphanLimitPollDeferred(o, now) {
				fmt.Printf("[limit] row %d: %s needs an operator since %s — deferring the next exchange poll until %s\n",
					o.ID, killSwitchLimitOrderLabel(o), o.OperatorRequiredSince.Format(time.RFC3339),
					o.OperatorRequiredSince.Add(effectiveAlertThrottleInterval()).Format(time.RFC3339))
				continue
			}
			outcome := cancelOrphanedLimitOrder(state, cfg, store, mu, o)
			applyOrphanLimitOperatorRequired(store, o, outcome, now)
			if outcome.Resolved {
				orphanLimitCancelAlerts.Clear(orphanLimitCancelKeyFor(o))
				warnNotifier(notifier, orphanLimitCancelResolvedMessage(o, block, outcome))
				continue
			}
			reportOrphanLimitCancel(notifier, o, block, outcome, now)
			continue
		}
		var logger *StrategyLogger
		if logMgr != nil {
			logger, _ = logMgr.GetStrategyLogger(o.StrategyID)
		}

		statusPolledAt := time.Now()
		statusRes, stderr, perr := runHyperliquidLimitStatusFn(sc.Script, o.Symbol, []int64{o.OrderOID}, limitStatusSinceMs(o.CreatedAt))
		if stderr != "" {
			fmt.Fprintf(os.Stderr, "[limit] %s status stderr: %s\n", o.StrategyID, stderr)
		}
		if perr != nil || statusRes == nil || statusRes.Error != "" {
			msg := ""
			if statusRes != nil {
				msg = statusRes.Error
			}
			fmt.Printf("[limit] status poll failed for %s oid=%d: %v %s\n", o.StrategyID, o.OrderOID, perr, msg)
			continue
		}
		if len(statusRes.Orders) == 0 {
			continue
		}
		st := statusRes.Orders[0]

		c := &limitFillCandidate{order: o, sc: sc, logger: logger, status: st, polledAt: statusPolledAt}
		if st.FilledSize > o.FilledSize+limitFillEpsilon {
			c.hasFill = true
			c.avgPx = st.AvgPx
			if c.avgPx <= 0 {
				c.avgPx = o.LimitPrice
			}
			c.signedDelta = hlSignedQty(o.Side, st.FilledSize-o.FilledSize)
			c.entryATR = o.EntryATR
		}
		candidates = append(candidates, c)
	}

	coinRows := make(map[string][]*limitFillCandidate)
	var coins []string
	for _, c := range candidates {
		if !c.hasFill {
			continue
		}
		if _, seen := coinRows[c.order.Symbol]; !seen {
			coins = append(coins, c.order.Symbol)
		}
		coinRows[c.order.Symbol] = append(coinRows[c.order.Symbol], c)
	}
	sort.Strings(coins)

	for _, coin := range coins {
		rows := coinRows[coin]
		latestPoll := rows[0].polledAt
		totalDelta := 0.0
		for _, r := range rows {
			if r.polledAt.After(latestPoll) {
				latestPoll = r.polledAt
			}
			totalDelta += r.signedDelta
		}
		positions, readErr := exposureReader.snapshotNewerThan(latestPoll)
		onChainNet := hyperliquidOnChainNetForCoin(positions, coin)
		owners := hyperliquidLiveOwnersForCoin(cfg.Strategies, coin)

		mu.RLock()
		preBooked := hyperliquidBookedSignedNetForCoin(cfg.Strategies, state, coin)
		for _, r := range rows {
			r.resolveATR = r.order.FilledSize == 0
			if !r.resolveATR {
				if ss := state.Strategies[r.order.StrategyID]; ss != nil {
					if p := ss.Positions[coin]; p != nil && p.EntryATR == 0 {
						r.resolveATR = true
					}
				}
			}
		}
		mu.RUnlock()

		if pre := classifyLimitFillLiveExposure(coin, preBooked, totalDelta, onChainNet, readErr, owners); pre.adopts() {
			for _, r := range rows {
				if r.resolveATR {
					r.entryATR = resolveLimitFillEntryATR(r.sc, cfg, r.order.EntryATR, r.avgPx, notifier)
				}
			}
		}

		decision, trades, errs := applyCoinLimitFills(state, cfg, mu, coin, rows, totalDelta, onChainNet, readErr, owners, now)
		if !decision.adopts() {
			for _, r := range rows {
				r.refused = true
				r.refusalVerdict = decision.Verdict
				reportLimitFillExposureRefusal(notifier, r.order, r.status.FilledSize, decision, now)
			}
			continue
		}
		for i, r := range rows {
			if errs[i] != nil {
				warnNotifier(notifier, fmt.Sprintf("[limit-fill] %s %s: %v", r.order.StrategyID, coin, errs[i]))
				r.applyFailed = true
				continue
			}
			limitFillExposureAlerts.Clear(limitFillExposureKeyFor(r.order))
			if err := store.UpdatePendingLimitOrderFill(r.order.ID, r.status.FilledSize, r.avgPx, r.status.Fee); err != nil {
				fmt.Printf("[limit] failed to persist fill watermark for row %d: %v\n", r.order.ID, err)
			}
			r.order.FilledSize = r.status.FilledSize
			r.order.AvgFillPrice = r.avgPx
			r.order.FillFee = r.status.Fee
			booked := trades[i]
			protectionDB, protectionErr := store.dbForStrategy(r.order.StrategyID)
			if protectionErr != nil {
				fmt.Printf("[limit] %v\n", protectionErr)
				continue
			}
			if _, fillPx := runHyperliquidProtectionSync(r.sc, state.Strategies[r.order.StrategyID], protectionDB, coin, mu, notifier, r.logger, "HL limit-fill protection synced", nil, nil, nil); fillPx > 0 {
				booked++
			}
			if ma := applied[r.order.StrategyID]; ma == nil {
				applied[r.order.StrategyID] = &manualAlert{sc: r.sc, ss: state.Strategies[r.order.StrategyID], trades: booked}
				order = append(order, r.order.StrategyID)
			} else {
				ma.trades += booked
			}
		}
	}

	for _, c := range candidates {
		applyLimitExposureOperatorRequired(store, c, now)
	}

	for _, c := range candidates {
		if c.applyFailed {
			continue
		}
		o := c.order
		st := c.status

		if st.Resting != nil && !*st.Resting {
			if c.refused {
				fmt.Printf("[limit] %s: order is off-book and its fill was NOT booked — keeping the queue row as the recovery record\n",
					killSwitchLimitOrderLabel(o))
				continue
			}
			if limitOrderFullyFilled(o.FilledSize, o.OrderSize) {
				fmt.Printf("[limit] %s oid=%d fully filled (%.6f %s)\n", o.StrategyID, o.OrderOID, o.FilledSize, o.Symbol)
			} else if o.FilledSize > 0 {
				warnNotifier(notifier, fmt.Sprintf(
					"[limit] %s %s: order no longer resting after partial fill %.6f of %.6f (remainder cancelled on-chain) — position tracked at filled size",
					o.StrategyID, o.Symbol, o.FilledSize, o.OrderSize))
			} else {
				warnNotifier(notifier, fmt.Sprintf(
					"[limit] %s %s: limit order cancelled with no fill (oid=%d)",
					o.StrategyID, o.Symbol, o.OrderOID))
			}
			if o.FilledSize > 0 {
				mu.Lock()
				saveErr := SaveStateWithStore(state, store)
				mu.Unlock()
				if saveErr != nil {
					warnNotifier(notifier, fmt.Sprintf(
						"[limit] %s %s: could not flush adopted fill before finalizing oid=%d (%v) — retrying next cycle",
						o.StrategyID, o.Symbol, o.OrderOID, saveErr))
					continue
				}
			}
			if err := store.DeletePendingLimitOrder(o.ID); err != nil {
				fmt.Printf("[limit] failed to delete terminal row %d: %v\n", o.ID, err)
			}
			continue
		}

		expired := !o.ExpiresAt.IsZero() && now.After(o.ExpiresAt)
		if o.CancelRequested || expired {
			reason := "operator cancel"
			if expired {
				reason = "TTL expiry"
			}
			cancelRes, cstderr, cerr := runHyperliquidCancelOrderFn(c.sc.Script, o.Symbol, o.OrderOID)
			if cstderr != "" {
				fmt.Fprintf(os.Stderr, "[limit] %s cancel stderr: %s\n", o.StrategyID, cstderr)
			}
			if cerr != nil || cancelRes == nil || cancelRes.Error != "" {
				fmt.Printf("[limit] cancel (%s) failed for %s oid=%d: %v — will retry\n", reason, o.StrategyID, o.OrderOID, cerr)
				continue
			}
			fmt.Printf("[limit] %s oid=%d cancel issued (%s); finalizing next cycle\n", o.StrategyID, o.OrderOID, reason)
		}
	}

	alerts := make([]manualAlert, 0, len(order))
	for _, id := range order {
		alerts = append(alerts, *applied[id])
	}
	return alerts
}

package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"math"
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
	OID          int64    `json:"oid"`
	Coin         string   `json:"coin,omitempty"`
	Resting      *bool    `json:"resting"`
	OrderStatus  string   `json:"order_status,omitempty"`
	OrderState   string   `json:"order_state,omitempty"`
	OrigSize     *float64 `json:"orig_size,omitempty"`
	ExecutedSize *float64 `json:"executed_size,omitempty"`
	StatusError  string   `json:"status_error,omitempty"`
	SzDecimals   *int     `json:"sz_decimals,omitempty"`
	PlacedMs     int64    `json:"placed_ms,omitempty"`
	StatusMs     int64    `json:"status_ms,omitempty"`
	FilledSize   float64  `json:"filled_size"`
	AvgPx        float64  `json:"avg_px"`
	Fee          float64  `json:"fee"`
	Count        int      `json:"count"`
	FillsProven  *bool    `json:"fills_proven,omitempty"`
	FillsError   string   `json:"fills_error,omitempty"`
	FillsReason  string   `json:"fills_reason,omitempty"`
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
	observeHLAgentScriptOutput(stdout, stderr)
	hlNoteCoinSubmission(symbol)
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
	observeHLAgentScriptOutput(stdout, stderr)
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
	admission   *manualEntryAdmission
}

func runManualLimitOpen(cfg *Config, sc StrategyConfig, stateDB *StateStore, in manualLimitOpenInputs) int {
	sealed := sealedManualCommandFeed(cfg)
	if sealed && manualLimitRequiresATR(sc) {
		if !validExplicitATR(in.atr) {
			fmt.Fprintf(os.Stderr, "error: market_feed %q forbids a private candle fetch; this strategy's protection needs a finite, positive --atr for a new limit open\n", cfg.marketFeedMode())
			return 2
		}
		if in.limitPrice > 0 && in.atr > 0.5*in.limitPrice {
			fmt.Fprintf(os.Stderr, "error: --atr %.4f exceeds 50%% of the limit price %.4f (plausibility guard)\n", in.atr, in.limitPrice)
			return 1
		}
	}
	resolve := func(sc StrategyConfig) (RegimePayload, error) { return resolveManualRegimePayload(sc, cfg) }
	confirm := func() error { return nil }
	var remote *manualRegimeRemote
	if sealed {
		remote = newManualRegimeRemote(cfg, stateDB)
		remote.admission = in.admission
		resolve, confirm = remote.resolve, remote.confirm
	}
	var entrySnapshots map[string]RegimeSnapshot
	regimeAdmitted := false
	if strategyRegimeStopDependent(sc) {
		payload, err := resolve(sc)
		if err != nil {
			if manualRegimeHardRefusal(err) {
				fmt.Fprintf(os.Stderr, "manual limit-open held: regime stop window refused: %v\n", err)
			} else {
				fmt.Fprintf(os.Stderr, "manual limit-open held: regime stop window unavailable: %v\n", err)
			}
			return 1
		}
		if why := regimeDependencyHold(sc, payload, payload, cfg.Regime, true); why != "" {
			fmt.Fprintf(os.Stderr, "manual limit-open held: %s\n", why)
			return 1
		}
		entrySnapshots = entryRegimeSnapshots(payload, cfg.Regime)
		regimeAdmitted = true
	}
	confirmAdmission := func() bool {
		if !regimeAdmitted {
			return true
		}
		if err := confirm(); err != nil {
			fmt.Fprintf(os.Stderr, "manual limit-open held: regime stop window refused: %v\n", err)
			return false
		}
		return true
	}
	size := resolveManualSize(in.size, in.notional, in.margin, in.limitPrice, sc.Leverage)
	if size <= 0 {
		fmt.Fprintf(os.Stderr, "error: resolved size is zero (size=%g notional=%g margin=%g limit=%g leverage=%g)\n",
			in.size, in.notional, in.margin, in.limitPrice, sc.Leverage)
		return 1
	}

	if in.dryRun {
		if !confirmAdmission() {
			return 1
		}
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
	limitPartition := partitionFor(sc)
	if state.partitionLatched(limitPartition) {
		fmt.Fprintf(os.Stderr, "error: portfolio kill switch is active for the %s scope — manual-open blocked (use manual-close to flatten)\n", partitionLabel(limitPartition))
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
	if !confirmAdmission() {
		return 1
	}
	if in.admission != nil {
		if err := in.admission.check("before placement"); err != nil {
			fmt.Fprintf(os.Stderr, "manual limit-open held: %v\n", err)
			return 1
		}
	} else if remote != nil && regimeAdmitted {
		if err := remote.checkPersistenceGuard(in.strategyID); err != nil {
			fmt.Fprintf(os.Stderr, "manual limit-open held: %v\n", err)
			return 1
		}
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
		RegimeSnapshots: entrySnapshots,
		StrategyID:      in.strategyID,
		Symbol:          sc.Symbol,
		Side:            in.side,
		OrderOID:        res.OrderOID,
		LimitPrice:      in.limitPrice,
		OrderSize:       size,
		TIF:             in.tif,
		EntryATR:        in.atr,
		ExpiresAt:       expiresAt,
		CreatedAt:       time.Now().UTC(),
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
	block := limitRowOwnerBlock(sc, known, o)
	if o.OperatorRequiredSince.IsZero() {
		if block == "" {
			return fmt.Sprintf("refusing: %s is a Hyperliquid-live type=manual strategy and the reconciler has NOT marked oid=%d as needing an operator, so the scheduler adopts this fill itself — clearing the row would orphan it; let the reconciler converge instead",
				o.StrategyID, o.OrderOID)
		}
		return fmt.Sprintf("refusing: the reconciler has not classified oid=%d as needing an operator — it is still converging on its own, and clearing the row now would discard a live recovery record",
			o.OrderOID)
	}
	if refusal := limitOperatorEvidenceRefusal(o); refusal != "" {
		return refusal
	}
	if !flattened {
		if ev, ok := parseLimitOperatorEvidence(o.OperatorEvidenceJSON); ok && ev.Unbookable {
			return fmt.Sprintf("refusing: pass --flattened to assert that you have confirmed on the Hyperliquid UI that no %s size from oid=%d remains open outside the scheduler's book — the flag does NOT ask you to close size the book holds, and the scheduler cannot verify size it does not track",
				o.Symbol, o.OrderOID)
		}
		return fmt.Sprintf("refusing: pass --flattened to assert that you have confirmed on the Hyperliquid UI that the %s position from oid=%d is closed — the scheduler cannot verify a flatten for a strategy it does not own",
			o.Symbol, o.OrderOID)
	}
	return ""
}

func clearedLimitRowLedgerNote(o PendingLimitOrder) string {
	ev, ok := parseLimitOperatorEvidence(o.OperatorEvidenceJSON)
	if ok && (ev.Unproven || !o.FillProofBound) && o.FilledSize > limitFillEpsilon {
		return fmt.Sprintf("the record is discarded and the ledger stays exactly as it is: nothing is booked or removed, and the scheduler does not assume that the recorded fill of %.6f reached the book", o.FilledSize)
	}
	if ok && ev.Unbookable && ev.ExecutedSize <= ev.BookedSize+limitFillEpsilon {
		return "the record is discarded, the book keeps the fill it already holds for this order, and nothing is booked or removed"
	}
	return "the record is discarded and the unadopted fill is NOT booked and stays off the ledger"
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
	flattened := fs.Bool("flattened", false, "Assert you confirmed on the Hyperliquid UI that no position size from this order remains open outside the scheduler's book")
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
		fmt.Printf("[dry-run] would discard the recovery record for oid=%d; %s\n", o.OrderOID, clearedLimitRowLedgerNote(o))
		return 0
	}
	if err := stateDB.DeletePendingLimitOrderIfUnchanged(o); err != nil {
		fmt.Fprintf(os.Stderr, "error clearing queue row %d: %v\n", o.ID, err)
		return 1
	}
	fmt.Printf("Cleared queue row %d (oid=%d): %s — the scheduler stops alerting on it next cycle.\n",
		o.ID, o.OrderOID, clearedLimitRowLedgerNote(o))
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
	if rowATR > 0 && avgPx > 0 && rowATR > 0.5*avgPx {
		if sealedManualCommandFeed(cfg) {
			return recoverSuppliedManualATR("limit-fill", sc.ID, sc.Symbol, rowATR, avgPx, sc.Leverage, notifier)
		}
		warnNotifier(notifier, fmt.Sprintf(
			"[limit-fill] %s %s: supplied ATR %.6f exceeds 50%% of fill price %.4f — discarding it",
			sc.ID, sc.Symbol, rowATR, avgPx))
		rowATR = 0
	}
	entryATR := rowATR
	if entryATR == 0 && (effectiveManualSLATRMult(sc) > 0 || strategyUsesTieredTPATRClose(sc) || strategyRegimeStopDependent(sc)) {
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
	var ss *StrategyState
	if state != nil {
		ss = state.Strategies[o.StrategyID]
	}
	return applyLimitFillProgressTo(ss, sc, o, cumFilled, avgPx, cumFee, entryATR, atrMethod, now)
}

func applyLimitFillProgressTo(ss *StrategyState, sc StrategyConfig, o PendingLimitOrder, cumFilled, avgPx, cumFee, entryATR float64, atrMethod string, now time.Time) (int, error) {
	var (
		n   int
		err error
	)
	mutatePaperPerpsBook(ss, o.Symbol, func() {
		n, err = applyLimitFillProgressInner(ss, sc, o, cumFilled, avgPx, cumFee, entryATR, atrMethod, now)
	})
	return n, err
}

func applyLimitFillProgressInner(ss *StrategyState, sc StrategyConfig, o PendingLimitOrder, cumFilled, avgPx, cumFee, entryATR float64, atrMethod string, now time.Time) (int, error) {
	if ss == nil {
		return 0, fmt.Errorf("strategy state for %q not found", o.StrategyID)
	}
	if cumFilled <= o.FilledSize+limitFillEpsilon {
		return 0, fmt.Errorf("limit fill for %s/%s does not exceed the booked %.8f", o.StrategyID, o.Symbol, o.FilledSize)
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
		pos.RegimeSnapshots = cloneRegimeSnapshots(o.RegimeSnapshots)
		if pos.RegimeSnapshots == nil {
			pos.RegimeSnapshots = map[string]RegimeSnapshot{}
		}
		payload := RegimePayload{MultiMode: true, Windows: pos.RegimeSnapshots}
		pos.RegimeWindows = payload.WindowLabels()
		gateKey := normalizeRegimeWindowKey(sc.RegimeGateWindow)
		if gateKey == "" || gateKey == regimeWindowDefaultKey {
			gateKey = regimeWindowReservedName
		}
		pos.Regime = pos.RegimeSnapshots[gateKey].Regime
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
		return 1, nil
	}

	if pos == nil {
		return 0, fmt.Errorf("limit partial fill for %s/%s but position is missing — not re-creating", o.StrategyID, o.Symbol)
	}
	if !manualPositionOwnedByStrategy(pos, o.StrategyID) {
		return 0, fmt.Errorf("limit partial fill for %s/%s but position owner=%q — not growing", o.StrategyID, o.Symbol, pos.OwnerStrategyID)
	}
	if pos.Side != o.Side || pos.Quantity <= 0 {
		return 0, fmt.Errorf("limit partial fill for %s/%s is %s but the booked position is %.6f %s — not growing", o.StrategyID, o.Symbol, o.Side, pos.Quantity, pos.Side)
	}
	deltaQty := cumFilled - o.FilledSize
	deltaFee := cumFee - o.FillFee
	deltaPx := avgPx
	if o.FilledSize > 0 && o.AvgFillPrice > 0 {
		if px := (cumFilled*avgPx - o.FilledSize*o.AvgFillPrice) / deltaQty; px > 0 && !math.IsInf(px, 0) && !math.IsNaN(px) {
			deltaPx = px
		}
	}
	prevQty := pos.Quantity
	pos.Quantity = prevQty + deltaQty
	pos.InitialQuantity += deltaQty
	if math.Abs(prevQty-o.FilledSize) <= limitFillEpsilon {
		pos.AvgCost = avgPx
	} else {
		pos.AvgCost = (prevQty*pos.AvgCost + deltaQty*deltaPx) / pos.Quantity
	}
	if entryATR > 0 && pos.EntryATR == 0 {
		pos.EntryATR = entryATR
	}

	trade := Trade{
		Timestamp:       now,
		StrategyID:      o.StrategyID,
		Symbol:          o.Symbol,
		Side:            openTradeSide(o.Side),
		Quantity:        deltaQty,
		Price:           deltaPx,
		Value:           deltaQty * deltaPx,
		TradeType:       scaleInTradeType,
		Details:         fmt.Sprintf("manual limit add %s %s %.6f @ $%.4f (oid=%d)", o.Side, o.Symbol, deltaQty, deltaPx, o.OrderOID),
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
	Proof        limitRowProof
}

func limitRowStatusProof(script string, o PendingLimitOrder, label string) limitRowProof {
	res, stderr, err := runHyperliquidLimitStatusFn(script, o.Symbol, []int64{o.OrderOID}, limitStatusSinceMs(o.CreatedAt))
	if stderr != "" {
		fmt.Fprintf(os.Stderr, "[limit] %s %s stderr: %s\n", o.StrategyID, label, stderr)
	}
	return decideLimitRowProof(o, res, err)
}

func limitOffBookUnverified(p limitRowProof) string {
	if p.Hold != "" {
		return p.Hold
	}
	if p.OpenOrdersError != "" {
		return fmt.Sprintf("open-orders state unknown (%s)", p.OpenOrdersError)
	}
	return "the exchange did not confirm that the order is off the book"
}

func cancelOrphanedLimitOrder(cfg *Config, store *StateStore, o PendingLimitOrder) orphanLimitCancelOutcome {
	out := orphanLimitCancelOutcome{AdoptedFill: o.FilledSize}

	candidates, _ := collectKillSwitchLimitOrderCandidates(
		[]PendingLimitOrder{o}, killSwitchLimitOrderRoster(cfg.Strategies))
	if len(candidates) == 0 {
		out.State = orphanLimitStateUnknown
		out.Reason = "no Hyperliquid strategy with a script remains in this config to cancel it — its on-chain state was never read, so manual intervention is required"
		return out
	}
	script := candidates[0].Script

	proof := limitRowStatusProof(script, o, "orphan status")
	out.Proof = proof
	if !proof.OffBook {
		cancelRes, cstderr, cerr := runHyperliquidCancelOrderFn(script, o.Symbol, o.OrderOID)
		if cstderr != "" {
			fmt.Fprintf(os.Stderr, "[limit] %s orphan cancel stderr: %s\n", o.StrategyID, cstderr)
		}
		if cerr != nil || cancelRes == nil || cancelRes.Error != "" {
			msg := ""
			if cancelRes != nil {
				msg = cancelRes.Error
			}
			out.Reason = strings.TrimSpace(fmt.Sprintf("cancel failed: %v %s", cerr, msg))
			if proof.Live {
				out.State = orphanLimitStateResting
			} else {
				out.State = orphanLimitStateUnknown
				out.Reason += "; " + limitOffBookUnverified(proof)
			}
			return out
		}
		out.CancelIssued = true
		proof = limitRowStatusProof(script, o, "orphan status")
		out.Proof = proof
		if proof.Live {
			out.State = orphanLimitStateResting
			out.Reason = "cancel issued but the order is still resting on the exchange"
			return out
		}
		if !proof.OffBook {
			out.State = orphanLimitStateUnknown
			out.Reason = "cancel issued but unverified: " + limitOffBookUnverified(proof)
			return out
		}
	}

	if proof.ExecKnown {
		out.ExchangeFill = proof.Executed
	}
	if proof.ProvenAboveBooked() {
		switch {
		case proof.CanBook():
			out.State = orphanLimitStateOffBookUnadoptedFill
			out.Reason = fmt.Sprintf(
				"the exchange proves an executed size of %.6f where the book holds %.6f, and NO automatic path can book the difference, so no fill was booked and the queue row is kept as the recovery record",
				proof.Executed, o.FilledSize)
		case proof.Permanent:
			out.State = orphanLimitStateOffBookUnadoptedFill
			out.Reason = fmt.Sprintf(
				"the exchange reports an executed size of %.6f where the book holds %.6f, and its fills can never be proven (%s), so no fill was booked and the queue row is kept as the recovery record",
				proof.Executed, o.FilledSize, proof.Hold)
		default:
			out.State = orphanLimitStateUnknown
			out.Reason = fmt.Sprintf("the exchange reports an executed size of %.6f where the book holds %.6f, but %s — the row is kept and the proof is retried",
				proof.Executed, o.FilledSize, proof.holdOr("its fills are not proven"))
		}
		return out
	}
	if !proof.CanFinalize() {
		out.State = orphanLimitStateUnknown
		out.Reason = proof.holdOr("the order's terminal state and fills are not yet proven")
		return out
	}
	sc, known := scByIDLookup(cfg, o.StrategyID)
	if holdUnprovenLimitWatermark(&out.Proof, o) || holdIneligibleLimitOwner(&out.Proof, o, limitRowOwnerBlock(sc, known, o)) {
		out.State = orphanLimitStateOffBookRowStuck
		out.Reason = out.Proof.Unbookable
		return out
	}
	unlock, lerr := store.tryLiveManualActionLock()
	if lerr != nil {
		out.State = orphanLimitStateOffBookRowStuck
		out.Reason = fmt.Sprintf("a manual command holds the manual-action lock (%v), so the off-book row is finalized on a later cycle", lerr)
		return out
	}
	defer unlock()
	if why := recheckLimitRow(store, o); why != "" {
		out.State = orphanLimitStateOffBookRowStuck
		out.Reason = why
		return out
	}
	if err := store.DeletePendingLimitOrderIfUnchanged(o); err != nil {
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
	if _, ok := parseLimitOperatorEvidence(o.OperatorEvidenceJSON); !ok {
		return false
	}
	return now.Sub(o.OperatorRequiredSince) < effectiveAlertThrottleInterval()
}

func recordLimitOperatorRequired(store *StateStore, o *PendingLimitOrder, p limitRowProof, reason string, now time.Time) bool {
	ev := limitOperatorEvidenceFor(*o, p, reason, now)
	blob := marshalLimitOperatorEvidence(ev)
	if err := store.MarkPendingLimitOrderOperatorRequired(*o, now, blob); err != nil {
		fmt.Printf("[limit] failed to persist the operator-required marker on row %d: %v\n", o.ID, err)
		return false
	}
	_, hadEvidence := parseLimitOperatorEvidence(o.OperatorEvidenceJSON)
	isNew := o.OperatorRequiredSince.IsZero() || !hadEvidence
	if o.OperatorRequiredSince.IsZero() {
		o.OperatorRequiredSince = now
	}
	o.OperatorEvidenceJSON = blob
	return isNew
}

func clearLimitOperatorRequired(store *StateStore, o *PendingLimitOrder) {
	if o.OperatorRequiredSince.IsZero() && o.OperatorEvidenceJSON == "" {
		return
	}
	if err := store.ClearPendingLimitOrderOperatorRequired(*o); err != nil {
		fmt.Printf("[limit] failed to clear the operator-required marker on row %d: %v\n", o.ID, err)
		return
	}
	o.OperatorRequiredSince = time.Time{}
	o.OperatorEvidenceJSON = ""
}

func applyOrphanLimitOperatorRequired(store *StateStore, o PendingLimitOrder, outcome orphanLimitCancelOutcome, now time.Time) bool {
	if outcome.Resolved {
		return false
	}
	if (outcome.State == orphanLimitStateOffBookUnadoptedFill || outcome.Proof.Unbookable != "") && limitOperatorRequiredWarranted(outcome.Proof) {
		return recordLimitOperatorRequired(store, &o, outcome.Proof, outcome.Reason, now)
	}
	if outcome.Proof.CertainForPeers() {
		clearLimitOperatorRequired(store, &o)
	}
	return false
}

type limitPeerRow struct {
	order   PendingLimitOrder
	certain bool
	reason  string
}

func limitPeerScript(cfg *Config, o PendingLimitOrder) string {
	candidates, _ := collectKillSwitchLimitOrderCandidates([]PendingLimitOrder{o}, killSwitchLimitOrderRoster(cfg.Strategies))
	if len(candidates) == 0 {
		return ""
	}
	return candidates[0].Script
}

var limitQueueReadFault = func() error { return nil }

func reconcilePendingLimitOrders(state *AppState, cfg *Config, store *StateStore, mu *sync.RWMutex, notifier *MultiNotifier, logMgr *LogManager) []manualAlert {
	if store == nil {
		return nil
	}
	var orders []PendingLimitOrder
	manualErr := limitQueueReadFault()
	if manualErr == nil {
		orders, manualErr = store.LoadPendingLimitOrders()
	}
	if pending, err := store.LoadPendingOrdersCombined(); err != nil {
		fmt.Printf("[limit] failed to load pending quote orders: %v\n", err)
	} else {
		for _, line := range dispatchQuotePendingRows(store, pending.Quote) {
			fmt.Println(line)
		}
	}
	if manualErr != nil {
		fmt.Printf("[limit] failed to load pending limit orders: %v — live entries stay held and no book increase or side change is resynced until the queue reads\n", manualErr)
		holdLimitEntryResyncUnreadable()
		store.finishLimitRecoveryPass(limitRecoveryPass{unreadable: manualErr.Error()})
		return nil
	}
	pass := limitRecoveryPass{read: true, present: make(map[int64]int64, len(orders))}
	for _, o := range orders {
		pass.present[o.ID] = o.OrderOID
	}
	defer func() { store.finishLimitRecoveryPass(pass) }()
	publishLimitEntryResyncHolds(orders)
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

	peers := make(map[string][]limitPeerRow)
	var skipped []PendingLimitOrder
	var candidates []*limitFillCandidate
	for _, o := range orders {
		sc, known := scByID[o.StrategyID]
		if block := limitRowOwnerBlock(sc, known, o); block != "" {
			expired := !o.ExpiresAt.IsZero() && now.After(o.ExpiresAt)
			if !o.CancelRequested && !expired {
				fmt.Printf("[limit] skipping row %d: %s (%q) and no cancellation is queued for it\n", o.ID, block, o.StrategyID)
				skipped = append(skipped, o)
				continue
			}
			if orphanLimitPollDeferred(o, now) {
				fmt.Printf("[limit] row %d: %s needs an operator since %s — deferring the next exchange poll until %s\n",
					o.ID, killSwitchLimitOrderLabel(o), o.OperatorRequiredSince.Format(time.RFC3339),
					o.OperatorRequiredSince.Add(effectiveAlertThrottleInterval()).Format(time.RFC3339))
				peers[o.Symbol] = append(peers[o.Symbol], limitPeerRow{order: o, reason: "it awaits an operator and its exchange poll is deferred"})
				continue
			}
			outcome := cancelOrphanedLimitOrder(cfg, store, o)
			markerNew := applyOrphanLimitOperatorRequired(store, o, outcome, now)
			if outcome.Resolved {
				orphanLimitCancelAlerts.Clear(orphanLimitCancelKeyFor(o))
				warnNotifier(notifier, orphanLimitCancelResolvedMessage(o, block, outcome))
				continue
			}
			peers[o.Symbol] = append(peers[o.Symbol], limitPeerRow{order: o, certain: outcome.Proof.CertainForPeers(), reason: outcome.Reason})
			reportOrphanLimitCancel(notifier, o, block, outcome, now, markerNew)
			continue
		}
		var logger *StrategyLogger
		if logMgr != nil {
			logger, _ = logMgr.GetStrategyLogger(o.StrategyID)
		}

		statusPolledAt := time.Now()
		proof := limitRowStatusProof(sc.Script, o, "status")
		holdUnprovenLimitWatermark(&proof, o)
		if proof.Hold == "" && proof.CanBook() {
			mu.RLock()
			why, final := verifyLimitIncrementLineage(store, state, o)
			mu.RUnlock()
			if why != "" {
				proof.Hold = why
				if final {
					proof.Unbookable = why
				}
			}
		}
		c := &limitFillCandidate{order: o, sc: sc, logger: logger, status: proof.Status, proof: proof, polledAt: statusPolledAt}
		if proof.CanBook() {
			c.hasFill = true
			c.avgPx = proof.Status.AvgPx
			c.signedDelta = hlSignedQty(o.Side, proof.Executed-o.FilledSize)
			c.entryATR = o.EntryATR
		} else if proof.Hold != "" {
			fmt.Printf("[limit] %s: nothing is booked or finalized this cycle: %s\n", killSwitchLimitOrderLabel(o), proof.Hold)
		}
		candidates = append(candidates, c)
	}

	needLock := false
	readyFill := false
	for _, c := range candidates {
		if c.hasFill || c.proof.CanFinalize() {
			needLock = true
		}
		if c.hasFill {
			readyFill = true
		}
	}
	var unlock func()
	release := func() {
		if unlock != nil {
			unlock()
			unlock = nil
		}
	}
	defer release()
	if needLock {
		u, lerr := store.tryLiveManualActionLock()
		blocked := ""
		if lerr != nil {
			blocked = fmt.Sprintf("a manual command holds the manual-action lock (%v), so booking and finalization wait for a later cycle", lerr)
			if readyFill {
				pass.deferred = true
			}
		} else {
			unlock = u
		}
		var fresh map[int64]PendingLimitOrder
		if blocked == "" {
			rows, rerr := store.LoadPendingLimitOrders()
			if rerr != nil {
				blocked = fmt.Sprintf("the queue could not be re-read under the manual-action lock (%v)", rerr)
				pass.unreadable = rerr.Error()
				holdLimitEntryResyncUnreadable()
			} else {
				fresh = make(map[int64]PendingLimitOrder, len(rows))
				for _, r := range rows {
					fresh[r.ID] = r
				}
			}
		}
		for _, c := range candidates {
			if !c.hasFill && !c.proof.CanFinalize() {
				continue
			}
			why := blocked
			if why == "" {
				if r, ok := fresh[c.order.ID]; !ok {
					why = "the row vanished under a concurrent manual action"
				} else if !limitRowUnchanged(r, c.order) {
					why = "the row changed under a concurrent manual action"
				}
			}
			if why != "" {
				c.hasFill = false
				c.blocked = why
				fmt.Printf("[limit] %s: %s\n", killSwitchLimitOrderLabel(c.order), why)
			}
		}
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

	for _, o := range skipped {
		positive := o.FilledSize > limitFillEpsilon
		if _, ok := coinRows[o.Symbol]; !ok && !positive {
			continue
		}
		sc, known := scByID[o.StrategyID]
		block := limitRowOwnerBlock(sc, known, o)
		if positive && orphanLimitPollDeferred(o, now) {
			peers[o.Symbol] = append(peers[o.Symbol], limitPeerRow{order: o, reason: "its owner cannot carry its booked fill, it awaits an operator, and its exchange poll is deferred"})
			continue
		}
		script := limitPeerScript(cfg, o)
		if script == "" {
			peers[o.Symbol] = append(peers[o.Symbol], limitPeerRow{order: o, reason: "no Hyperliquid strategy with a script remains to read its state"})
			continue
		}
		peerProof := limitRowStatusProof(script, o, "peer status")
		holdUnprovenLimitWatermark(&peerProof, o)
		holdIneligibleLimitOwner(&peerProof, o, block)
		peers[o.Symbol] = append(peers[o.Symbol], limitPeerRow{order: o, certain: peerProof.CertainForPeers(), reason: peerProof.holdOr("its execution is not settled in the book")})
		if positive {
			candidates = append(candidates, &limitFillCandidate{order: o, sc: sc, status: peerProof.Status, proof: peerProof, review: true})
		}
	}

	var adopted []*limitFillCandidate
	for _, coin := range coins {
		rows := coinRows[coin]
		peerWhy := ""
		for _, c := range candidates {
			if c.review || c.order.Symbol != coin || c.hasFill || c.proof.CertainForPeers() {
				continue
			}
			why := c.blocked
			if why == "" {
				why = c.proof.holdOr("its execution is not settled in the book")
			}
			peerWhy = fmt.Sprintf("%s (%s)", killSwitchLimitOrderLabel(c.order), why)
			break
		}
		if peerWhy == "" {
			for _, p := range peers[coin] {
				if !p.certain {
					peerWhy = fmt.Sprintf("%s (%s)", killSwitchLimitOrderLabel(p.order), p.reason)
					break
				}
			}
		}
		if peerWhy != "" {
			reason := fmt.Sprintf("pending limit row %s has uncertain execution on %s that could account for the account's exposure, so no fill on this coin is booked until it is resolved", peerWhy, coin)
			for _, r := range rows {
				r.hasFill = false
				r.blocked = reason
			}
			fmt.Printf("[limit] %s\n", reason)
			continue
		}

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

		decision, trades, errs, commitErr := applyCoinLimitFills(state, cfg, store, mu, coin, rows, totalDelta, onChainNet, readErr, owners, now)
		if !decision.adopts() {
			for _, r := range rows {
				r.refused = true
				r.refusalDecision = decision
			}
			continue
		}
		if commitErr != nil {
			failed := make(map[int64]int64)
			for _, r := range rows {
				if r.commitFailed {
					failed[r.order.ID] = r.order.OrderOID
				}
			}
			store.noteLimitFillCommitFailed(failed)
			warnNotifier(notifier, fmt.Sprintf("[limit-fill] %s: the proven fill could not be committed with its queue rows (%v) — nothing was booked, the rows stay retryable, and new live entries stay held until the fill commits", coin, commitErr))
			for _, r := range rows {
				r.applyFailed = true
				r.blocked = fmt.Sprintf("the booking could not be committed (%v), so the row stays retryable", commitErr)
			}
			continue
		}
		for i, r := range rows {
			if errs[i] != nil {
				warnNotifier(notifier, fmt.Sprintf("[limit-fill] %s %s: %v", r.order.StrategyID, coin, errs[i]))
				r.applyFailed = true
				r.blocked = errs[i].Error()
				r.proof.Unbookable = errs[i].Error()
				continue
			}
			store.clearLimitFillCommitFailed(r.order.ID)
			limitFillExposureAlerts.Clear(limitFillExposureKeyFor(r.order))
			r.order.FilledSize = r.proof.Executed
			r.order.AvgFillPrice = r.avgPx
			r.order.FillFee = r.status.Fee
			r.order.FillProofVersion = limitFillProofVersionAtomic
			r.order.FillProofBinding = r.binding
			r.order.FillProofBound = true
			r.proof = decideLimitRowProof(r.order, &HyperliquidLimitStatusResult{Orders: []HyperliquidLimitOrderStatus{r.status}}, nil)
			r.booked = trades[i]
			r.adopted = true
			r.positions = positions
			r.readErr = readErr
			adopted = append(adopted, r)
		}
	}

	for _, c := range candidates {
		o := c.order
		if c.refused {
			if c.proof.OffBook {
				fmt.Printf("[limit] %s: order is off-book and its fill was NOT booked — keeping the queue row as the recovery record\n",
					killSwitchLimitOrderLabel(o))
			}
			continue
		}
		if c.review || c.blocked != "" || c.applyFailed || !c.proof.CanFinalize() || limitWatermarkUnproven(o) != "" {
			continue
		}
		if err := store.DeletePendingLimitOrderIfUnchanged(o); err != nil {
			fmt.Printf("[limit] failed to finalize terminal row %d: %v\n", o.ID, err)
			continue
		}
		c.finalized = true
		limitFillExposureAlerts.Clear(limitFillExposureKeyFor(o))
		if limitOrderFullyFilled(o.FilledSize, o.OrderSize) {
			fmt.Printf("[limit] %s oid=%d fully filled (%.6f %s)\n", o.StrategyID, o.OrderOID, o.FilledSize, o.Symbol)
		} else if o.FilledSize > 0 {
			warnNotifier(notifier, fmt.Sprintf(
				"[limit] %s %s: order no longer resting after a recorded fill of %.6f of %.6f (remainder cancelled on-chain) — the book keeps its committed position, including any later reduction, and nothing more is booked for this order",
				o.StrategyID, o.Symbol, o.FilledSize, o.OrderSize))
		} else {
			warnNotifier(notifier, fmt.Sprintf(
				"[limit] %s %s: limit order cancelled with no fill (oid=%d)",
				o.StrategyID, o.Symbol, o.OrderOID))
		}
	}
	release()

	for _, r := range adopted {
		protectionDB, protectionErr := store.dbForStrategy(r.order.StrategyID)
		if protectionErr != nil {
			fmt.Printf("[limit] %v\n", protectionErr)
			continue
		}
		var fillShare *hlCycleShare
		if r.readErr == nil {
			fillShare = newHLCycleShare(hlOnChainCoinViewFromPositions(r.positions), hlCoinSubmitSnapshot(), nil, state.Strategies, hyperliquidCloseScopeStrategies(cfg.Strategies), notifier)
		}
		booked := r.booked
		if _, fillPx := runHyperliquidProtectionSync(r.sc, state.Strategies[r.order.StrategyID], protectionDB, r.order.Symbol, mu, notifier, r.logger, "HL limit-fill protection synced", nil, nil, nil, hlProtectionGuardFull, fillShare); fillPx > 0 {
			booked++
		}
		if ma := applied[r.order.StrategyID]; ma == nil {
			applied[r.order.StrategyID] = &manualAlert{sc: r.sc, ss: state.Strategies[r.order.StrategyID], trades: booked}
			order = append(order, r.order.StrategyID)
		} else {
			ma.trades += booked
		}
	}

	for _, c := range candidates {
		markerNew := applyLimitExposureOperatorRequired(store, c, now)
		switch {
		case c.refused:
			reportLimitFillExposureRefusal(notifier, c.order, c.proof.Executed, c.refusalDecision, now, markerNew)
		case c.commitFailed:
		case !c.adopted && !c.finalized && (c.proof.ProvenAboveBooked() || c.proof.Unbookable != ""):
			reason := c.blocked
			if reason == "" {
				reason = c.proof.holdOr("the fill could not be booked this cycle")
			}
			reportLimitFillHeld(notifier, c.order, c.proof, reason, now, markerNew)
		}
	}

	for _, c := range candidates {
		if c.review || c.finalized || c.proof.OffBook {
			continue
		}
		o := c.order
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

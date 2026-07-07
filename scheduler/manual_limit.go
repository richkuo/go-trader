package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"sync"
	"time"
)

// limitFillEpsilon is the minimum increase in cumulative filled size that
// counts as a new fill worth booking. Guards against float noise in the
// userFills size summation re-booking a zero-delta "fill" every cycle.
const limitFillEpsilon = 1e-9

// ---------------------------------------------------------------------------
// Subprocess result types + wrappers for the #883 resting-limit-order modes of
// check_hyperliquid.py. Parsers are extracted so Go CI (no .venv) can assert
// the JSON contract without spawning Python (same pattern as the execute /
// update-stop-loss parsers).
// ---------------------------------------------------------------------------

// HyperliquidLimitOpenResult is emitted by check_hyperliquid.py --limit-open.
type HyperliquidLimitOpenResult struct {
	Platform   string  `json:"platform"`
	Timestamp  string  `json:"timestamp"`
	Error      string  `json:"error,omitempty"`
	Status     string  `json:"status,omitempty"` // "resting" | "filled" | "error"
	OrderOID   int64   `json:"order_oid,omitempty"`
	LimitPrice float64 `json:"limit_price,omitempty"`
	TIF        string  `json:"tif,omitempty"`
}

// HyperliquidLimitOrderStatus is one order's poll result from --limit-status.
// Resting is a pointer so a null (open-orders fetch failed) is distinguishable
// from a confirmed false — the scheduler must defer the cancelled/expired
// verdict when the book state is unknown, never book a phantom cancellation.
type HyperliquidLimitOrderStatus struct {
	OID        int64   `json:"oid"`
	Resting    *bool   `json:"resting"`
	FilledSize float64 `json:"filled_size"`
	AvgPx      float64 `json:"avg_px"`
	Fee        float64 `json:"fee"`
	Count      int     `json:"count"`
	FillsError string  `json:"fills_error,omitempty"`
}

// HyperliquidLimitStatusResult is emitted by check_hyperliquid.py --limit-status.
type HyperliquidLimitStatusResult struct {
	Platform        string                        `json:"platform"`
	Timestamp       string                        `json:"timestamp"`
	Error           string                        `json:"error,omitempty"`
	Orders          []HyperliquidLimitOrderStatus `json:"orders"`
	OpenOrdersError string                        `json:"open_orders_error,omitempty"`
}

// HyperliquidCancelOrderResult is emitted by check_hyperliquid.py --cancel-order.
type HyperliquidCancelOrderResult struct {
	Platform    string `json:"platform"`
	Timestamp   string `json:"timestamp"`
	Error       string `json:"error,omitempty"`
	OID         int64  `json:"oid"`
	Cancelled   bool   `json:"cancelled"`
	CancelError string `json:"cancel_error,omitempty"`
}

// buildHyperliquidLimitOpenArgs builds the argv for --limit-open. Extracted so
// the argv contract can be asserted without spawning a subprocess.
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
	// A non-empty Error / status=error is a structured failure the caller
	// inspects; runErr (exit 1) accompanies it but the JSON is authoritative.
	return &result, stderrStr, nil
}

// RunHyperliquidLimitOpen places a resting maker limit order to open a position.
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

// RunHyperliquidLimitStatus polls fill/resting status for the given OIDs.
// sinceMs is the userFills lookback floor (epoch ms); pass the order's placement
// time so the cumulative-fill summary always reaches back to when the order was
// placed. 0 lets Python fall back to its rolling 7-day window — which would
// silently undercount cumulative fills on an order resting longer than 7 days
// (an earlier partial ages out of the window, so a later fill reads as ≤ the
// already-booked watermark and is skipped). (#886 review)
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

// limitStatusSinceMs returns the userFills lookback floor for an order's fill
// poll: its placement time minus a 60s buffer for local-vs-indexer clock skew.
// Zero when the row has no recorded createdAt (Python then defaults to 7 days).
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

// RunHyperliquidCancelOrder cancels a resting order by OID (idempotent).
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

// Package vars so reconcile tests can stub the subprocess calls without a .venv.
var (
	runHyperliquidLimitOpenFn   = RunHyperliquidLimitOpen
	runHyperliquidLimitStatusFn = RunHyperliquidLimitStatus
	runHyperliquidCancelOrderFn = RunHyperliquidCancelOrder
)

// ---------------------------------------------------------------------------
// CLI: manual-open --limit-price (fire-and-exit) + manual-cancel (#883).
// ---------------------------------------------------------------------------

// manualLimitOpenInputs carries the parsed manual-open flags relevant to the
// resting-limit-order path.
type manualLimitOpenInputs struct {
	strategyID  string
	side        string // "long" | "short"
	openSide    string // "buy" | "sell"
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

// runManualLimitOpen places a resting maker limit order and persists it for the
// scheduler to track. There is no synchronous fill, so SL/TP arming is deferred
// to the scheduler's per-cycle protection sync (#883). Returns a process exit
// code.
func runManualLimitOpen(cfg *Config, sc StrategyConfig, stateDB *StateDB, in manualLimitOpenInputs) int {
	// Size against the intended entry (limit) price, not the current mid — the
	// position opens at the limit price when it fills.
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

	// Hold the same cross-process lock as the synchronous manual-action cores from
	// before the guard reads through the pending_limit_orders insert. Without it,
	// two CLIs (or a CLI racing the dashboard market-open core) can both observe no
	// pending row and both fire on-chain before either row is visible (#1261).
	unlock, lockErr := acquireManualActionFileLock(cfg.DBFile)
	if lockErr != nil {
		fmt.Fprintf(os.Stderr, "error: %v — refusing to avoid double-firing an on-chain order\n", lockErr)
		return 1
	}
	defer unlock()

	// Placement guard: one un-drained position-changing action, resting order, or
	// open position per strategy+symbol. On fill the scheduler creates a fresh
	// position; a pre-existing position or resting/queued open would collide (the
	// fill path fails closed rather than adopt a foreign position).
	state, loadErr := LoadStateWithDB(cfg, stateDB)
	if loadErr != nil {
		fmt.Fprintf(os.Stderr, "error: could not load state for placement guard: %v\n", loadErr)
		return 1
	}
	if state.PortfolioRisk.KillSwitchActive {
		fmt.Fprintln(os.Stderr, "error: portfolio kill switch is active — manual-open blocked (use manual-close to flatten)")
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
		// The order is resting on-chain but we failed to persist it — cancel it
		// so it can't fill into an untracked position.
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

// runManualCancel implements `go-trader manual-cancel <strategy-id>`. It flags
// the strategy's resting limit order for cancellation; the scheduler cancels it
// on-chain and finalizes (booking any last-moment fill) on its next cycle. A
// single writer (the scheduler) owns all fill booking, so the CLI never races
// the reconcile by cancelling + deleting directly.
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

	stateDB, err := OpenStateDB(cfg.DBFile)
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

// ---------------------------------------------------------------------------
// Scheduler-side reconcile of resting limit orders (#883).
// ---------------------------------------------------------------------------

// limitOrderFullyFilled reports whether the cumulative filled size has reached
// the requested order size, within a lot-rounding tolerance. order_size was
// rounded to the asset's lot precision at placement, so the summed fills match
// it exactly in the happy path; the tolerance absorbs float summation drift.
func limitOrderFullyFilled(cumFilled, orderSize float64) bool {
	tol := orderSize * 1e-6
	if tol < 1e-9 {
		tol = 1e-9
	}
	return cumFilled >= orderSize-tol
}

// resolveLimitFillEntryATR resolves the entry ATR to stamp on a position opened
// by a limit fill. An operator-supplied value (rowATR) wins; otherwise fetch the
// live ATR at fill time (more correct than freezing it at placement, which can
// be many hours/days before the fill) with the leverage-aware fallback. Mirrors
// the market-open ATR resolution in runManualOpen. Spawns Python, so it must run
// outside the state lock.
func resolveLimitFillEntryATR(sc StrategyConfig, rowATR, avgPx float64, notifier *MultiNotifier) float64 {
	entryATR := rowATR
	if entryATR == 0 && (effectiveManualSLATRMult(sc) > 0 || strategyUsesTieredTPATRClose(sc)) {
		fetched, fetchErr, ok := fetchManualEntryATR(sc)
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
	// Plausibility guard mirrors stampEntryATRIfOpened's 50%-of-AvgCost check.
	if entryATR > 0 && avgPx > 0 && entryATR > 0.5*avgPx {
		warnNotifier(notifier, fmt.Sprintf(
			"[limit-fill] %s %s: entry ATR %.6f exceeds 50%% of fill price %.4f — not stamping ATR",
			sc.ID, sc.Symbol, entryATR, avgPx))
		entryATR = 0
	}
	return entryATR
}

// effectiveManualSLATRMult returns the strategy-level ATR SL multiplier that the
// limit-fill path uses to decide whether ATR protection is needed. Manual
// strategies default to defaultManualStopLossATRMult via config normalization, so
// sc.StopLossATRMult is populated; this just reads it safely.
func effectiveManualSLATRMult(sc StrategyConfig) float64 {
	if sc.StopLossATRMult != nil {
		return *sc.StopLossATRMult
	}
	return 0
}

// applyLimitFillProgress creates (first fill) or grows (subsequent partial) the
// tracked position for a resting limit order and records the open trade for the
// newly-filled delta. MUST be called with the state write lock held. Returns the
// number of trades booked (0 or 1). The watermark advance + protection sync are
// the caller's responsibility (they happen outside the lock).
func applyLimitFillProgress(state *AppState, sc StrategyConfig, o PendingLimitOrder, cumFilled, avgPx, cumFee, entryATR float64, now time.Time) (int, error) {
	ss := state.Strategies[o.StrategyID]
	if ss == nil {
		return 0, fmt.Errorf("strategy state for %q not found", o.StrategyID)
	}
	pos := ss.Positions[o.Symbol]

	if o.FilledSize == 0 {
		// First fill: the position must not already exist (the placement guard in
		// manual-open rejects a limit order when a position is already open). If
		// one exists we did NOT create it, so adopting would corrupt a foreign
		// position — fail closed and leave it for the operator.
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
			Multiplier:      1, // perps
			Leverage:        sc.Leverage,
			OwnerStrategyID: o.StrategyID,
			OpenedAt:        now,
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
		ss.Cash -= cumFee // perps open deducts only the fee; notional stays virtual
		fmt.Printf("[limit] applied open: %s %s %.6f %s @ $%.4f (oid=%d)\n",
			o.StrategyID, o.Side, cumFilled, o.Symbol, avgPx, o.OrderOID)
		return 1, nil
	}

	// Subsequent partial fill: grow the position to the new cumulative size and
	// book the incremental delta as another open leg (same PositionID).
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
	pos.AvgCost = avgPx // cumulative VWAP is authoritative
	if entryATR > 0 && pos.EntryATR == 0 {
		pos.EntryATR = entryATR
	}

	trade := Trade{
		Timestamp:  now,
		StrategyID: o.StrategyID,
		Symbol:     o.Symbol,
		Side:       openTradeSide(o.Side),
		Quantity:   deltaQty,
		Price:      avgPx,
		Value:      deltaQty * avgPx,
		// A growth leg is an additional fill of the SAME order, not a new
		// position open. Tag it scale_in (like #873 adds) so LifetimeTradeStats
		// open-count (COUNT WHERE is_close=0 AND trade_type<>'scale_in') treats a
		// multi-partial limit fill as ONE opened position, matching a market open.
		// W/L are grouped by position_id and so are unaffected.
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

// reconcilePendingLimitOrders polls every resting limit order, adopts new fills
// into the tracked position (arming protection immediately so no filled coin
// sits unprotected), and finalizes orders that have fully filled, been
// cancelled, or expired. Runs at the top of each cycle after the manual-action
// drain. Returns one manualAlert per strategy that booked >=1 fill so the caller
// can fire sendTradeAlerts outside the state lock (same #880 pattern as the
// drain). Network calls (status poll, cancel, protection sync) run outside mu;
// position mutation is done under mu.Lock per row.
func reconcilePendingLimitOrders(state *AppState, cfg *Config, stateDB *StateDB, mu *sync.RWMutex, notifier *MultiNotifier, logMgr *LogManager) []manualAlert {
	if stateDB == nil {
		return nil
	}
	orders, err := stateDB.LoadPendingLimitOrders()
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
	applied := make(map[string]*manualAlert)
	var order []string

	for _, o := range orders {
		sc, ok := scByID[o.StrategyID]
		if !ok || sc.Type != "manual" {
			// Orphan row (strategy removed/retyped). Leave it — deleting could
			// strand a resting on-chain order with no operator visibility.
			fmt.Printf("[limit] skipping row %d: strategy %q missing or not type=manual\n", o.ID, o.StrategyID)
			continue
		}
		if !hyperliquidIsLive(sc.Args) {
			fmt.Printf("[limit] skipping row %d: strategy %q is not HL-live\n", o.ID, o.StrategyID)
			continue
		}
		var logger *StrategyLogger
		if logMgr != nil {
			logger, _ = logMgr.GetStrategyLogger(o.StrategyID)
		}

		// 1. Poll fill/resting status (subprocess, no lock). Anchor the fill
		// lookback to the order's placement time so an order resting >7 days
		// never has an earlier partial age out of the window (#886 review).
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

		// 2. Adopt any incremental fill (create/grow position + arm protection).
		if st.FilledSize > o.FilledSize+limitFillEpsilon {
			avgPx := st.AvgPx
			if avgPx <= 0 {
				avgPx = o.LimitPrice
			}
			// Resolve entry ATR at the first fill, and re-resolve on a later
			// partial if the position is still missing ATR — e.g. the first-fill
			// fetch failed (and the leverage fallback couldn't apply), leaving the
			// position naked for ATR-based protection. Re-fetching on the next
			// partial heals it instead of staying naked for the order's life
			// (#886 review). Operator-supplied o.EntryATR short-circuits the fetch.
			entryATR := o.EntryATR
			resolveATR := o.FilledSize == 0
			if !resolveATR {
				mu.RLock()
				if ss := state.Strategies[o.StrategyID]; ss != nil {
					if p := ss.Positions[o.Symbol]; p != nil && p.EntryATR == 0 {
						resolveATR = true
					}
				}
				mu.RUnlock()
			}
			if resolveATR {
				entryATR = resolveLimitFillEntryATR(sc, o.EntryATR, avgPx, notifier)
			}
			mu.Lock()
			tradesBooked, applyErr := applyLimitFillProgress(state, sc, o, st.FilledSize, avgPx, st.Fee, entryATR, now)
			mu.Unlock()
			if applyErr != nil {
				warnNotifier(notifier, fmt.Sprintf("[limit-fill] %s %s: %v", o.StrategyID, o.Symbol, applyErr))
				// Do not advance the watermark — retry next cycle.
				continue
			}
			if err := stateDB.UpdatePendingLimitOrderFill(o.ID, st.FilledSize, avgPx, st.Fee); err != nil {
				fmt.Printf("[limit] failed to persist fill watermark for row %d: %v\n", o.ID, err)
			}
			// Advance the local copy so the terminal logic below sees the new fill.
			o.FilledSize = st.FilledSize
			o.AvgFillPrice = avgPx
			o.FillFee = st.Fee
			// Arm protection immediately so the filled coin is never unprotected,
			// regardless of whether this strategy is "due" this cycle.
			runHyperliquidProtectionSync(sc, state.Strategies[o.StrategyID], stateDB, o.Symbol, mu, notifier, logger, "HL limit-fill protection synced", nil)
			if ma := applied[o.StrategyID]; ma == nil {
				applied[o.StrategyID] = &manualAlert{sc: sc, ss: state.Strategies[o.StrategyID], trades: tradesBooked}
				order = append(order, o.StrategyID)
			} else {
				ma.trades += tradesBooked
			}
		}

		// 3. Terminal conditions.
		if st.Resting != nil && !*st.Resting {
			// Order is off the book: fully filled, or cancelled/expired externally.
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
			// Durably flush the position carrying this order's adopted fills
			// BEFORE the terminal row disappears, so a cross-process reader (a CLI
			// manual-close / manual-add reading state.db) never observes the row
			// gone while the DB position still understates the adopted fill
			// (#1263 review-3 race). This loop grows the position in-memory
			// (applyLimitFillProgress) but only persists it at the end-of-cycle
			// SaveState; deleting the row first opens a window where state.db is
			// stale and the CLI has no pending row left to reconcile against, so a
			// sized shared-coin close would leak an untracked residual. Persisting
			// here establishes the invariant "terminal row absent ⇒ state.db
			// reflects the adopted fill." Only a fill changes a position, so a
			// no-fill cancel skips the flush. On a save failure keep the row and
			// retry the flush+delete next cycle rather than deleting against a
			// stale DB.
			if o.FilledSize > 0 {
				mu.Lock()
				saveErr := SaveStateWithDB(state, cfg, stateDB)
				mu.Unlock()
				if saveErr != nil {
					warnNotifier(notifier, fmt.Sprintf(
						"[limit] %s %s: could not flush adopted fill before finalizing oid=%d (%v) — retrying next cycle",
						o.StrategyID, o.Symbol, o.OrderOID, saveErr))
					continue
				}
			}
			if err := stateDB.DeletePendingLimitOrder(o.ID); err != nil {
				fmt.Printf("[limit] failed to delete terminal row %d: %v\n", o.ID, err)
			}
			continue
		}

		// Still resting (or book state unknown). Honor operator cancel / TTL
		// expiry by cancelling on-chain; the NEXT cycle observes resting=false
		// and finalizes via the branch above (which also adopts any last-moment
		// fill through the same incremental path — no fill can be missed).
		expired := !o.ExpiresAt.IsZero() && now.After(o.ExpiresAt)
		if o.CancelRequested || expired {
			reason := "operator cancel"
			if expired {
				reason = "TTL expiry"
			}
			cancelRes, cstderr, cerr := runHyperliquidCancelOrderFn(sc.Script, o.Symbol, o.OrderOID)
			if cstderr != "" {
				fmt.Fprintf(os.Stderr, "[limit] %s cancel stderr: %s\n", o.StrategyID, cstderr)
			}
			if cerr != nil || cancelRes == nil || cancelRes.Error != "" {
				// Keep the row; retry the cancel next cycle.
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

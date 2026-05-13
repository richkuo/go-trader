package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"math"
	"os"
	"os/exec"
	"sort"
	"strings"
	"time"
)

// HLFillSummary is a per-OID aggregate from fetch_hl_user_fills.py.
// A single market order can fragment into multiple partial fills sharing the
// same OID; the script sums fee + closedPnl across legs before emitting.
//
// ClosedPnLGross is Hyperliquid's `closedPnl` summed across legs. It is
// gross of fees — do not use it for realized-PnL bookkeeping (#698). The
// backfill recomputes realized PnL locally from the stored pre-fee PnL
// and the real exchange fee (see planTradeRewrites in this file).
type HLFillSummary struct {
	Fee            float64 `json:"fee"`
	ClosedPnLGross float64 `json:"closed_pnl"`
	Count          int     `json:"count"`
}

// HLUserFillsResult is the stdout envelope from fetch_hl_user_fills.py.
type HLUserFillsResult struct {
	ByOID          map[string]HLFillSummary `json:"by_oid"`
	FillCount      int                      `json:"fill_count"`
	PageCount      int                      `json:"page_count"`
	AccountAddress string                   `json:"account_address"`
	Error          string                   `json:"error"`
}

// backfillHLUserFillsLookback widens the userFills query before the first DB
// trade timestamp. Trade rows are recorded after the Python order call returns,
// while HL's fill timestamp is earlier, so an exact lower bound can miss the
// first order in a targeted backfill (#597).
const backfillHLUserFillsLookback = 10 * time.Minute

func backfillUserFillsStartTime(earliestTrade time.Time) time.Time {
	if earliestTrade.IsZero() {
		return time.Time{}
	}
	queryStart := earliestTrade.Add(-backfillHLUserFillsLookback)
	minStart := time.UnixMilli(1).UTC()
	if queryStart.Before(minStart) {
		return minStart
	}
	return queryStart
}

// TradeBackfillRow is the subset of a `trades` row needed by planBackfillForStrategy.
// Pulled into its own type so the planner is pure (no DB dep).
type TradeBackfillRow struct {
	RowID           int64
	Timestamp       time.Time
	Symbol          string
	PositionID      string
	Value           float64
	IsClose         bool
	ExchangeOrderID string
	ExchangeFee     float64
	RealizedPnL     float64
}

// TradeChange describes one trade-row update produced by planBackfillForStrategy.
type TradeChange struct {
	RowID          int64
	Timestamp      time.Time
	Symbol         string
	OID            string
	OldFee         float64
	NewFee         float64
	OldRealizedPnL float64
	NewRealizedPnL float64
	IsClose        bool
}

// SkippedTrade explains why a trade row was not updated.
type SkippedTrade struct {
	RowID     int64
	Timestamp time.Time
	Symbol    string
	OID       string
	Reason    string // "missing_oid", "no_fill_match", "already_real_fee"
}

// ClosedPositionRecompute carries the new aggregate PnL for a single
// closed_positions row. Pinned by row id (closed_positions has no
// position_id column yet, so we identify the target row directly).
type ClosedPositionRecompute struct {
	RowID      int64
	Symbol     string
	PositionID string // resolved from the matched close-leg trade
	OldPnL     float64
	NewPnL     float64
}

// BackfillPlan is the full set of changes for one strategy.
type BackfillPlan struct {
	StrategyID            string
	TradeChanges          []TradeChange
	Skipped               []SkippedTrade
	ClosedPositions       []ClosedPositionRecompute
	OldCash               float64
	NewCash               float64
	TotalFeeDeltaUSD      float64 // sum of (oldFee - newFee) across rows; positive = strategy "got back" fees
	TotalPnLDeltaUSD      float64 // sum of (newPnL - oldPnL) across close legs
	MatchedTradeCount     int
	UnmatchedOIDCount     int
	MissingOIDCount       int
	AlreadyRealFeeCount   int     // post-#587 rows skipped because exchange_fee was already non-zero
	ReplayedCash          float64 // pre-correction cash replay (initial_capital + old fees + old pnl) — diverges from OldCash when SIGHUP capital top-ups landed
	CashBaselineDivergent bool    // ReplayedCash differs from OldCash by more than $1 → likely SIGHUP capital top-up; --apply must be gated
}

// planBackfillForStrategy is the pure planner used by the backfill command.
// It does no IO — given a trade history, an OID→fill map, and the strategy's
// initial capital + current cash, it returns the change list and the
// recomputed cash baseline.
//
// Cash recompute model (perps): cash starts at initial_capital; each open leg
// debits the *real* fee (or the modeled fee if no OID match was available);
// each close leg credits the *new* realized_pnl (already net of the real fee
// in the corrected value). This mirrors the live model
// (`ExecutePerpsSignalWithLeverage` / `applyManualAction`) where notional
// stays virtual and only fees + realized pnl move cash.
func planBackfillForStrategy(
	strategyID string,
	trades []TradeBackfillRow,
	fillMap map[string]HLFillSummary,
	initialCapital, oldCash float64,
) BackfillPlan {
	plan := BackfillPlan{
		StrategyID: strategyID,
		OldCash:    oldCash,
	}

	// Sort trades chronologically so the cash replay matches the on-chain
	// sequence — close-leg credits depend on the open leg landing first when
	// realized_pnl is recomputed via the corrected fee.
	sortedTrades := make([]TradeBackfillRow, len(trades))
	copy(sortedTrades, trades)
	sort.SliceStable(sortedTrades, func(i, j int) bool {
		return sortedTrades[i].Timestamp.Before(sortedTrades[j].Timestamp)
	})

	// Pre-correction replay: walk the trades using the values *currently*
	// stored on disk (modeled fee fallback when exchange_fee=0 — that's what
	// was actually deducted at execution time). If the result diverges from
	// `oldCash` by more than $1, the strategy almost certainly had its
	// `capital` raised mid-run via SIGHUP (config_reload.go applies
	// `Cash += new - old` directly with no trade row), so a forward replay
	// from initial_capital would silently roll cash back to a wrong baseline
	// on --apply. Surface it as a divergence flag so the report and
	// runBackfillHLFees can refuse --apply unless the operator opts in.
	preReplayCash := initialCapital
	for _, t := range sortedTrades {
		preFee := t.ExchangeFee
		if preFee == 0 {
			preFee = math.Abs(t.Value) * HyperliquidTakerFeePct
		}
		if t.IsClose {
			preReplayCash += t.RealizedPnL
		} else {
			preReplayCash -= preFee
		}
	}
	plan.ReplayedCash = preReplayCash
	if math.Abs(preReplayCash-oldCash) > 1.0 {
		plan.CashBaselineDivergent = true
	}

	cash := initialCapital
	for _, t := range sortedTrades {
		newFee := t.ExchangeFee
		newPnL := t.RealizedPnL
		modeledFee := math.Abs(t.Value) * HyperliquidTakerFeePct

		matchAttempted := false
		matched := false
		if t.ExchangeOrderID == "" {
			plan.MissingOIDCount++
			plan.Skipped = append(plan.Skipped, SkippedTrade{
				RowID:     t.RowID,
				Timestamp: t.Timestamp,
				Symbol:    t.Symbol,
				Reason:    "missing_oid",
			})
		} else {
			matchAttempted = true
			summary, ok := fillMap[t.ExchangeOrderID]
			if ok {
				matched = true
			} else {
				plan.UnmatchedOIDCount++
				plan.Skipped = append(plan.Skipped, SkippedTrade{
					RowID:     t.RowID,
					Timestamp: t.Timestamp,
					Symbol:    t.Symbol,
					OID:       t.ExchangeOrderID,
					Reason:    "no_fill_match",
				})
			}
			if matched {
				realFee := summary.Fee
				if t.ExchangeFee != 0 {
					// Already-real-fee guard: never overwrite a non-zero
					// stored fee (post-#587 rows). Keep the stored values.
					plan.AlreadyRealFeeCount++
					plan.Skipped = append(plan.Skipped, SkippedTrade{
						RowID:     t.RowID,
						Timestamp: t.Timestamp,
						Symbol:    t.Symbol,
						OID:       t.ExchangeOrderID,
						Reason:    "already_real_fee",
					})
				} else {
					newFee = realFee
					if t.IsClose {
						// realized_pnl was originally `pnl_pre_fee - modeledFee`.
						// We want `pnl_pre_fee - realFee` → adjust by (modeledFee - realFee).
						newPnL = t.RealizedPnL + (modeledFee - realFee)
					}
					if newFee != t.ExchangeFee || newPnL != t.RealizedPnL {
						plan.TradeChanges = append(plan.TradeChanges, TradeChange{
							RowID:          t.RowID,
							Timestamp:      t.Timestamp,
							Symbol:         t.Symbol,
							OID:            t.ExchangeOrderID,
							OldFee:         t.ExchangeFee,
							NewFee:         newFee,
							OldRealizedPnL: t.RealizedPnL,
							NewRealizedPnL: newPnL,
							IsClose:        t.IsClose,
						})
						plan.MatchedTradeCount++
						plan.TotalFeeDeltaUSD += t.ExchangeFee - newFee
						plan.TotalPnLDeltaUSD += newPnL - t.RealizedPnL
					}
				}
			}
		}

		// Cash replay: open leg debits effective fee, close leg credits newPnL.
		// For an unmatched open leg we fall back to the modeled fee since
		// that's what was originally deducted from cash at execution time
		// (and remains a closer estimate than zero).
		effectiveFee := newFee
		if !matched && matchAttempted {
			effectiveFee = modeledFee
		} else if !matched && !matchAttempted {
			// missing_oid: pre-OID-stamping legacy row — also uses modeled fee
			effectiveFee = modeledFee
		}
		if t.IsClose {
			cash += newPnL
		} else {
			cash -= effectiveFee
		}

	}

	plan.NewCash = cash
	return plan
}

// planClosedPositionRecomputes matches each closed_positions row to a
// corrected close-leg trade by (symbol, closed_at == trade.timestamp), then
// emits a recompute when the per-position aggregate (sum of close-leg
// realized_pnl sharing that position_id) differs from the stored value.
//
// closed_positions has no position_id column yet, so we recover the grouping
// via the matching close-leg trade. Rows whose match has empty position_id
// (legacy pre-#471 entries, or non-perps closes) are skipped — there's no
// reliable way to aggregate them.
func planClosedPositionRecomputes(
	corrected []TradeBackfillRow,
	closedRows []ClosedPositionRow,
) []ClosedPositionRecompute {
	// Sum corrected close-leg PnL by position_id.
	sumsByPID := make(map[string]float64)
	pidToSymbol := make(map[string]string)
	for _, t := range corrected {
		if !t.IsClose || t.PositionID == "" {
			continue
		}
		sumsByPID[t.PositionID] += t.RealizedPnL
		pidToSymbol[t.PositionID] = t.Symbol
	}

	// Build a lookup: (symbol, ms-truncated timestamp) → position_id from
	// the corrected close-leg trades. The closed_at column on
	// closed_positions is RFC 3339 with nanosecond precision matching the
	// trade row written in the same SaveState pass, so an exact-key lookup
	// works in practice; use a tolerance-based fallback for legacy rows.
	type tradeKey struct {
		Symbol string
		UnixNs int64
	}
	exact := make(map[tradeKey]string)
	type closeTrade struct {
		Symbol string
		Ts     time.Time
		PID    string
	}
	var closeLegs []closeTrade
	for _, t := range corrected {
		if !t.IsClose || t.PositionID == "" {
			continue
		}
		exact[tradeKey{Symbol: t.Symbol, UnixNs: t.Timestamp.UnixNano()}] = t.PositionID
		closeLegs = append(closeLegs, closeTrade{Symbol: t.Symbol, Ts: t.Timestamp, PID: t.PositionID})
	}
	sort.Slice(closeLegs, func(i, j int) bool { return closeLegs[i].Ts.Before(closeLegs[j].Ts) })

	out := make([]ClosedPositionRecompute, 0)
	for _, cp := range closedRows {
		pid := exact[tradeKey{Symbol: cp.Symbol, UnixNs: cp.ClosedAt.UnixNano()}]
		if pid == "" {
			// Tolerance match: nearest close-leg trade on same symbol within
			// 5s, but require BOTH (a) directional ordering — the trade leg
			// must land at or after the closed_positions row — AND (b)
			// uniqueness: no other candidate within the same window. This
			// rules out rapid back-to-back partial-then-final closes on the
			// same symbol silently mapping to the wrong position_id.
			//
			// Timestamp invariant (post-#471): both trades.timestamp and
			// closed_positions.closed_at are written from pos.ClosedAt in the
			// same SaveState pass, so they are the same value and the exact-ns
			// path above handles all modern rows without reaching here.
			// Legacy rows (pre-#471) may violate the ordering assumption
			// because trades.timestamp was the HL exchange fill time (a few
			// ms–s before the scheduler runs SaveState) while closed_at is the
			// scheduler processing time — meaning leg.Ts could be slightly
			// before cp.ClosedAt, causing this guard to produce no match
			// rather than a wrong match. That is the safe failure mode.
			var candidate string
			candidates := 0
			for _, leg := range closeLegs {
				if leg.Symbol != cp.Symbol {
					continue
				}
				if leg.Ts.Before(cp.ClosedAt) {
					continue
				}
				if leg.Ts.Sub(cp.ClosedAt) > 5*time.Second {
					continue
				}
				candidate = leg.PID
				candidates++
				if candidates > 1 {
					break
				}
			}
			if candidates == 1 {
				pid = candidate
			}
		}
		if pid == "" {
			continue
		}
		newPnL, ok := sumsByPID[pid]
		if !ok {
			continue
		}
		// Tolerance: skip when the delta is below 0.001 USD to avoid
		// floating-point noise rewriting the row for nothing.
		if math.Abs(newPnL-cp.RealizedPnL) < 1e-3 {
			continue
		}
		out = append(out, ClosedPositionRecompute{
			RowID:      cp.ID,
			Symbol:     cp.Symbol,
			PositionID: pid,
			OldPnL:     cp.RealizedPnL,
			NewPnL:     newPnL,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].RowID < out[j].RowID })
	return out
}

// runBackfill is the dispatcher for `go-trader backfill <subcommand>`.
func runBackfill(args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "Usage: go-trader backfill hl-fees [--config scheduler/config.json] (--all | --strategy <id>) [--apply]")
		return 2
	}
	switch args[0] {
	case "hl-fees":
		return runBackfillHLFees(args[1:])
	default:
		fmt.Fprintf(os.Stderr, "Unknown backfill target %q\n", args[0])
		return 2
	}
}

// runBackfillHLFees implements `go-trader backfill hl-fees`.
//
// Backfills `trades.exchange_fee` (and `realized_pnl` on close legs) for live
// HL strategies whose rows pre-date #587 — when modeled fees were written
// directly because HL's order-placement response did not surface the real fee.
//
// Default mode is dry-run. Pass --apply to commit. --apply refuses to run
// when another go-trader process is alive (a concurrent SaveState would
// overwrite the recomputed cash on its next cycle), and refuses per-strategy
// when the pre-correction cash replay diverges from the stored cash by more
// than $1 (likely a SIGHUP capital top-up via config_reload.go that a
// forward-from-initial-capital replay cannot reproduce). Pass --reset-cash to
// override the divergence guard.
func runBackfillHLFees(args []string) int {
	fs := flag.NewFlagSet("backfill hl-fees", flag.ContinueOnError)
	configPath := fs.String("config", "scheduler/config.json", "Path to config file")
	strategyID := fs.String("strategy", "", "Strategy ID to backfill (mutually exclusive with --all)")
	all := fs.Bool("all", false, "Backfill all live HL perps strategies")
	apply := fs.Bool("apply", false, "Commit changes (default: dry-run)")
	resetCash := fs.Bool("reset-cash", false, "Allow --apply to overwrite strategies.cash even when the pre-correction replay diverges from the stored value (e.g. after a SIGHUP capital top-up)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *apply {
		if err := refuseIfSchedulerRunning(); err != nil {
			fmt.Fprintf(os.Stderr, "error: %v\n", err)
			return 1
		}
	}
	if (*strategyID == "" && !*all) || (*strategyID != "" && *all) {
		fmt.Fprintln(os.Stderr, "error: exactly one of --strategy <id> or --all is required")
		return 2
	}

	cfg, err := LoadConfig(*configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to load config: %v\n", err)
		return 1
	}

	stateDB, err := OpenStateDB(cfg.DBFile)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to open state DB: %v\n", err)
		return 1
	}
	defer stateDB.Close()

	state, err := LoadStateWithDB(cfg, stateDB)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to load state: %v\n", err)
		return 1
	}

	var targets []StrategyConfig
	if *strategyID != "" {
		var found bool
		for _, sc := range cfg.Strategies {
			if sc.ID == *strategyID {
				if sc.Platform != "hyperliquid" {
					fmt.Fprintf(os.Stderr, "error: strategy %q platform=%q (expected hyperliquid)\n", *strategyID, sc.Platform)
					return 1
				}
				if sc.Type == "perps" && !hyperliquidIsLive(sc.Args) {
					fmt.Fprintf(os.Stderr, "error: strategy %q is paper-mode (no real OIDs to match against userFills)\n", *strategyID)
					return 1
				}
				targets = []StrategyConfig{sc}
				found = true
				break
			}
		}
		if !found {
			fmt.Fprintf(os.Stderr, "error: strategy %q not found in config\n", *strategyID)
			return 1
		}
	} else {
		for _, sc := range cfg.Strategies {
			if sc.Platform != "hyperliquid" {
				continue
			}
			if sc.Type != "perps" && sc.Type != "manual" {
				continue
			}
			// Paper-mode HL perps trades have synthetic OIDs that won't match
			// any userFills row; skip them with a one-line note rather than
			// burying the operator in noisy `missing_oid` skip lines.
			// `manual` strategies are always live (no paper mode).
			if sc.Type == "perps" && !hyperliquidIsLive(sc.Args) {
				fmt.Printf("[%s] skipped: paper-mode (no real OIDs)\n", sc.ID)
				continue
			}
			targets = append(targets, sc)
		}
		sort.Slice(targets, func(i, j int) bool { return targets[i].ID < targets[j].ID })
	}
	if len(targets) == 0 {
		fmt.Fprintln(os.Stderr, "error: no live HL strategies found in config")
		return 1
	}

	// Fetch userFills once across all targets (same wallet). Start slightly
	// before the earliest trade timestamp because DB rows are stamped after
	// the order returns, while HL userFills are stamped at exchange fill time.
	earliest, err := stateDB.EarliestTradeTimestamp(strategyIDsOf(targets))
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to read earliest trade timestamp: %v\n", err)
		return 1
	}
	if earliest.IsZero() {
		fmt.Fprintln(os.Stderr, "info: no trades found for the selected strategies — nothing to backfill")
		return 0
	}

	queryStart := backfillUserFillsStartTime(earliest)
	fmt.Printf("Fetching HL userFills since %s (earliest trade %s, lookback %s)...\n",
		queryStart.UTC().Format(time.RFC3339),
		earliest.UTC().Format(time.RFC3339),
		backfillHLUserFillsLookback)
	fillResult, err := runFetchHLUserFills(queryStart)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to fetch HL userFills: %v\n", err)
		return 1
	}
	if fillResult.Error != "" {
		fmt.Fprintf(os.Stderr, "HL userFills returned an error: %s\n", fillResult.Error)
		return 1
	}
	fmt.Printf("Fetched %d fills across %d pages (account=%s)\n",
		fillResult.FillCount, fillResult.PageCount, fillResult.AccountAddress)
	if len(fillResult.ByOID) == 0 {
		fmt.Println("warning: HL returned 0 fills — nothing to match against (verify HYPERLIQUID_ACCOUNT_ADDRESS)")
	}

	mode := "DRY-RUN"
	if *apply {
		mode = "APPLY"
	}
	fmt.Printf("\n=== %s mode ===\n", mode)

	exitCode := 0
	for _, sc := range targets {
		ss := state.Strategies[sc.ID]
		var oldCash float64
		var initialCapital float64
		if ss != nil {
			oldCash = ss.Cash
			initialCapital = ss.InitialCapital
		}
		if initialCapital == 0 {
			initialCapital = sc.Capital
		}

		trades, err := stateDB.ListTradesForBackfill(sc.ID)
		if err != nil {
			fmt.Fprintf(os.Stderr, "[%s] failed to list trades: %v\n", sc.ID, err)
			exitCode = 1
			continue
		}

		closedRows, err := stateDB.LoadClosedPositionRows(sc.ID)
		if err != nil {
			fmt.Fprintf(os.Stderr, "[%s] failed to load closed_positions: %v\n", sc.ID, err)
			exitCode = 1
			continue
		}

		plan := planBackfillForStrategy(sc.ID, trades, fillResult.ByOID, initialCapital, oldCash)

		changeByRowID := make(map[int64]TradeChange, len(plan.TradeChanges))
		for _, c := range plan.TradeChanges {
			changeByRowID[c.RowID] = c
		}
		correctedTrades := make([]TradeBackfillRow, 0, len(trades))
		for _, t := range trades {
			row := t
			if c, ok := changeByRowID[t.RowID]; ok {
				row.ExchangeFee = c.NewFee
				row.RealizedPnL = c.NewRealizedPnL
			}
			correctedTrades = append(correctedTrades, row)
		}
		plan.ClosedPositions = planClosedPositionRecomputes(correctedTrades, closedRows)

		printBackfillReport(plan)

		if *apply {
			if plan.CashBaselineDivergent && !*resetCash {
				fmt.Fprintf(os.Stderr, "[%s] APPLY refused: cash baseline diverges from pre-correction replay by $%+.4f. Re-run with --reset-cash to acknowledge that the recomputed cash will not preserve mid-run capital top-ups.\n",
					sc.ID, plan.OldCash-plan.ReplayedCash)
				exitCode = 1
				continue
			}
			if err := stateDB.ApplyBackfillPlan(plan); err != nil {
				fmt.Fprintf(os.Stderr, "[%s] APPLY failed: %v\n", sc.ID, err)
				exitCode = 1
				continue
			}
			fmt.Printf("[%s] APPLY committed: %d trade rows, %d closed_positions, cash %.4f → %.4f\n",
				sc.ID, len(plan.TradeChanges), len(plan.ClosedPositions), plan.OldCash, plan.NewCash)
		}
	}

	if !*apply {
		fmt.Println("\n(dry-run — re-run with --apply to commit)")
	}
	return exitCode
}

func strategyIDsOf(strategies []StrategyConfig) []string {
	ids := make([]string, 0, len(strategies))
	for _, sc := range strategies {
		ids = append(ids, sc.ID)
	}
	return ids
}

// printBackfillReport renders a per-strategy summary block to stdout.
func printBackfillReport(plan BackfillPlan) {
	fmt.Printf("\n--- %s ---\n", plan.StrategyID)
	fmt.Printf("  rows updated:        %d\n", len(plan.TradeChanges))
	fmt.Printf("  rows skipped:        %d (missing_oid=%d, unmatched=%d, already_real=%d)\n",
		len(plan.Skipped), plan.MissingOIDCount, plan.UnmatchedOIDCount, plan.AlreadyRealFeeCount)
	fmt.Printf("  fee delta (sum):     $%+.4f (positive = fees were over-modeled)\n", plan.TotalFeeDeltaUSD)
	fmt.Printf("  pnl delta (sum):     $%+.4f\n", plan.TotalPnLDeltaUSD)
	fmt.Printf("  cash:                $%.4f → $%.4f (Δ %+.4f)\n",
		plan.OldCash, plan.NewCash, plan.NewCash-plan.OldCash)
	fmt.Printf("  closed_positions:    %d aggregate rows to update\n", len(plan.ClosedPositions))
	if plan.CashBaselineDivergent {
		fmt.Printf("  WARNING: cash baseline diverges from pre-correction replay by $%+.4f\n",
			plan.OldCash-plan.ReplayedCash)
		fmt.Printf("           (replayed=$%.4f vs stored=$%.4f). Likely SIGHUP capital top-up\n",
			plan.ReplayedCash, plan.OldCash)
		fmt.Printf("           — SIGHUP applies Cash += new - old with no trade row, which a\n")
		fmt.Printf("           forward replay cannot reproduce. --apply requires --reset-cash.\n")
	}
}

// refuseIfSchedulerRunning aborts when another `go-trader` process is alive.
//
// The backfill rewrites `strategies.cash` directly. SQLite's WAL journal lets
// the running scheduler keep its own write connection open in parallel, so the
// next SaveState in that scheduler will overwrite the recomputed cash with
// whatever value its in-memory `AppState` holds — silently undoing the
// backfill. Detect a peer process via `pgrep` and refuse with an actionable
// error before the operator commits.
func refuseIfSchedulerRunning() error {
	out, err := exec.Command("pgrep", "-x", "go-trader").Output()
	if err != nil {
		// pgrep exits 1 when no match — that's the success path here. Any
		// other error means pgrep itself failed (missing binary, etc.); skip
		// the check rather than blocking apply on operator tooling.
		return nil
	}
	self := os.Getpid()
	var others []int
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var pid int
		if _, perr := fmt.Sscanf(line, "%d", &pid); perr != nil {
			continue
		}
		if pid == self {
			continue
		}
		others = append(others, pid)
	}
	if len(others) == 0 {
		return nil
	}
	return fmt.Errorf("another go-trader process is running (pid %v); stop it before running --apply (concurrent SaveState would overwrite the recomputed strategies.cash)", others)
}

// hlUserFillsFetchTimeout bounds fetch_hl_user_fills.py — paging through years
// of history can exceed the standard scriptTimeout used by per-cycle scripts.
const hlUserFillsFetchTimeout = 5 * time.Minute

// runFetchHLUserFills spawns shared_scripts/fetch_hl_user_fills.py with
// hlUserFillsFetchTimeout via the shared Python runner (semaphore + uv argv).
func runFetchHLUserFills(since time.Time) (*HLUserFillsResult, error) {
	script := "shared_scripts/fetch_hl_user_fills.py"
	args := []string{
		fmt.Sprintf("--since-ms=%d", since.UnixMilli()),
	}

	stdout, stderr, runErr := runPythonWithTimeout(context.Background(), script, args, nil, hlUserFillsFetchTimeout)
	if runErr != nil {
		var toe *pythonScriptTimeoutError
		if errors.As(runErr, &toe) {
			return nil, runErr
		}
		if stdout == nil {
			return nil, runErr
		}
	}

	stderrStr := strings.TrimSpace(string(stderr))
	if stderrStr != "" {
		fmt.Fprintln(os.Stderr, stderrStr)
	}
	var result HLUserFillsResult
	if err := json.Unmarshal(stdout, &result); err != nil {
		return nil, fmt.Errorf("parse output: %w (stdout: %s)", err, string(stdout))
	}
	if runErr != nil && result.Error == "" {
		return &result, fmt.Errorf("script error: %w", runErr)
	}
	return &result, nil
}

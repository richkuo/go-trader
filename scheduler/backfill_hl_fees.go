package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"math"
	"os"
	"os/exec"
	"sort"
	"strings"
	"syscall"
	"time"
)

// HLFillSummary is a per-OID aggregate from fetch_hl_user_fills.py.
// A single market order can fragment into multiple partial fills sharing the
// same OID; the script sums fee + closedPnl across legs before emitting.
type HLFillSummary struct {
	Fee       float64 `json:"fee"`
	ClosedPnL float64 `json:"closed_pnl"`
	Count     int     `json:"count"`
}

// HLUserFillsResult is the stdout envelope from fetch_hl_user_fills.py.
type HLUserFillsResult struct {
	ByOID          map[string]HLFillSummary `json:"by_oid"`
	FillCount      int                      `json:"fill_count"`
	PageCount      int                      `json:"page_count"`
	AccountAddress string                   `json:"account_address"`
	Error          string                   `json:"error"`
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
	StrategyID        string
	TradeChanges      []TradeChange
	Skipped           []SkippedTrade
	ClosedPositions   []ClosedPositionRecompute
	OldCash           float64
	NewCash           float64
	TotalFeeDeltaUSD  float64 // sum of (oldFee - newFee) across rows; positive = strategy "got back" fees
	TotalPnLDeltaUSD  float64 // sum of (newPnL - oldPnL) across close legs
	MatchedTradeCount int
	UnmatchedOIDCount int
	MissingOIDCount   int
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

	// Map rowid → resolved (newFee, newPnL) so the closed-position recompute
	// pass below can read the corrected values.
	corrected := make(map[int64]TradeBackfillRow, len(sortedTrades))

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

		// Stash corrected values for the closed-position aggregate pass.
		corrected[t.RowID] = TradeBackfillRow{
			RowID:           t.RowID,
			Timestamp:       t.Timestamp,
			Symbol:          t.Symbol,
			PositionID:      t.PositionID,
			Value:           t.Value,
			IsClose:         t.IsClose,
			ExchangeOrderID: t.ExchangeOrderID,
			ExchangeFee:     newFee,
			RealizedPnL:     newPnL,
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
			// Tolerance match: closest close-leg trade on same symbol within 5s.
			best := time.Duration(0)
			for _, leg := range closeLegs {
				if leg.Symbol != cp.Symbol {
					continue
				}
				d := leg.Ts.Sub(cp.ClosedAt)
				if d < 0 {
					d = -d
				}
				if d > 5*time.Second {
					continue
				}
				if pid == "" || d < best {
					pid = leg.PID
					best = d
				}
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
// Default mode is dry-run. Pass --apply to commit. The scheduler should be
// stopped before running with --apply (this command opens its own write
// transaction; SQLite serializes writers but a concurrent SaveState could
// overwrite the recomputed cash on its next cycle if left running).
func runBackfillHLFees(args []string) int {
	fs := flag.NewFlagSet("backfill hl-fees", flag.ContinueOnError)
	configPath := fs.String("config", "scheduler/config.json", "Path to config file")
	strategyID := fs.String("strategy", "", "Strategy ID to backfill (mutually exclusive with --all)")
	all := fs.Bool("all", false, "Backfill all live HL perps strategies")
	apply := fs.Bool("apply", false, "Commit changes (default: dry-run)")
	if err := fs.Parse(args); err != nil {
		return 2
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
			targets = append(targets, sc)
		}
		sort.Slice(targets, func(i, j int) bool { return targets[i].ID < targets[j].ID })
	}
	if len(targets) == 0 {
		fmt.Fprintln(os.Stderr, "error: no live HL strategies found in config")
		return 1
	}

	// Fetch userFills once across all targets (same wallet). Use the earliest
	// trade timestamp across all targets as the lower bound.
	earliest, err := stateDB.EarliestTradeTimestamp(strategyIDsOf(targets))
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to read earliest trade timestamp: %v\n", err)
		return 1
	}
	if earliest.IsZero() {
		fmt.Fprintln(os.Stderr, "info: no trades found for the selected strategies — nothing to backfill")
		return 0
	}

	fmt.Printf("Fetching HL userFills since %s...\n", earliest.UTC().Format(time.RFC3339))
	fillResult, err := runFetchHLUserFills(earliest)
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

		correctedTrades := make([]TradeBackfillRow, 0, len(trades))
		for _, t := range trades {
			row := t
			for _, c := range plan.TradeChanges {
				if c.RowID == t.RowID {
					row.ExchangeFee = c.NewFee
					row.RealizedPnL = c.NewRealizedPnL
					break
				}
			}
			correctedTrades = append(correctedTrades, row)
		}
		plan.ClosedPositions = planClosedPositionRecomputes(correctedTrades, closedRows)

		printBackfillReport(plan)

		if *apply {
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
	fmt.Printf("  rows skipped:        %d (missing_oid=%d, unmatched=%d)\n",
		len(plan.Skipped), plan.MissingOIDCount, plan.UnmatchedOIDCount)
	fmt.Printf("  fee delta (sum):     $%+.4f (positive = fees were over-modeled)\n", plan.TotalFeeDeltaUSD)
	fmt.Printf("  pnl delta (sum):     $%+.4f\n", plan.TotalPnLDeltaUSD)
	fmt.Printf("  cash:                $%.4f → $%.4f (Δ %+.4f)\n",
		plan.OldCash, plan.NewCash, plan.NewCash-plan.OldCash)
	fmt.Printf("  closed_positions:    %d aggregate rows to update\n", len(plan.ClosedPositions))
}

// runFetchHLUserFills spawns shared_scripts/fetch_hl_user_fills.py with a
// generous timeout (paging through years of history can exceed the standard
// 30s scriptTimeout used by the per-cycle check scripts).
func runFetchHLUserFills(since time.Time) (*HLUserFillsResult, error) {
	script := "shared_scripts/fetch_hl_user_fills.py"
	args := []string{
		fmt.Sprintf("--since-ms=%d", since.UnixMilli()),
	}

	pythonSemaphore <- struct{}{}
	defer func() { <-pythonSemaphore }()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	cmd := exec.CommandContext(ctx, ".venv/bin/python3", append([]string{script}, args...)...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	runErr := cmd.Run()
	if ctx.Err() == context.DeadlineExceeded {
		if cmd.Process != nil {
			syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		}
		return nil, fmt.Errorf("script timed out after 5m")
	}
	stderrStr := strings.TrimSpace(stderr.String())
	if stderrStr != "" {
		fmt.Fprintln(os.Stderr, stderrStr)
	}
	var result HLUserFillsResult
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
		return nil, fmt.Errorf("parse output: %w (stdout: %s)", err, stdout.String())
	}
	if runErr != nil && result.Error == "" {
		return &result, fmt.Errorf("script error: %w", runErr)
	}
	return &result, nil
}

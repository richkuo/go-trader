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

type HLFillSummary struct {
	Coin           string  `json:"coin"`
	FirstTimeMS    int64   `json:"first_time_ms"`
	LastTimeMS     int64   `json:"last_time_ms"`
	Fee            float64 `json:"fee"`
	ClosedPnLGross float64 `json:"closed_pnl"`
	Count          int     `json:"count"`
	Qty float64 `json:"qty"`
	Px  float64 `json:"px"`
}

type HLUserFillsResult struct {
	ByOID          map[string]HLFillSummary `json:"by_oid"`
	FillCount      int                      `json:"fill_count"`
	PageCount      int                      `json:"page_count"`
	AccountAddress string                   `json:"account_address"`
	Error          string                   `json:"error"`
}

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

type TradeBackfillRow struct {
	RowID           int64
	Timestamp       time.Time
	Symbol          string
	PositionID      string
	Side            string
	Quantity        float64
	Price           float64
	Value           float64
	TradeType       string
	Details         string
	IsClose         bool
	ExchangeOrderID string
	ExchangeFee     float64
	RealizedPnL     float64
	PnLGross        bool
	FeeSource       string
}

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

type SkippedTrade struct {
	RowID     int64
	Timestamp time.Time
	Symbol    string
	OID       string
	Reason    string
}

type ClosedPositionRecompute struct {
	RowID      int64
	Symbol     string
	PositionID string
	OldPnL     float64
	NewPnL     float64
}

type BackfillPlan struct {
	StrategyID            string
	TradeChanges          []TradeChange
	Skipped               []SkippedTrade
	ClosedPositions       []ClosedPositionRecompute
	OldCash               float64
	NewCash               float64
	TotalFeeDeltaUSD      float64
	TotalPnLDeltaUSD      float64
	MatchedTradeCount     int
	UnmatchedOIDCount     int
	MissingOIDCount       int
	AlreadyRealFeeCount   int
	ReplayedCash          float64
	CashBaselineDivergent bool
}

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

	sortedTrades := make([]TradeBackfillRow, len(trades))
	copy(sortedTrades, trades)
	sort.SliceStable(sortedTrades, func(i, j int) bool {
		return sortedTrades[i].Timestamp.Before(sortedTrades[j].Timestamp)
	})

	preReplayCash := initialCapital
	for _, t := range sortedTrades {
		if t.TradeType == TradeTypeFunding {
			continue
		}
		preFee := t.ExchangeFee
		if preFee == 0 && !t.PnLGross {
			preFee = math.Abs(t.Value) * HyperliquidTakerFeePct
		}
		if t.IsClose {
			preReplayCash += tradeBackfillRowNetPnL(t)
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
		if t.TradeType == TradeTypeFunding {
			continue
		}
		newFee := t.ExchangeFee
		newPnL := t.RealizedPnL
		modeledFee := math.Abs(t.Value) * HyperliquidTakerFeePct

		if t.PnLGross {
			plan.AlreadyRealFeeCount++
			plan.Skipped = append(plan.Skipped, SkippedTrade{
				RowID:     t.RowID,
				Timestamp: t.Timestamp,
				Symbol:    t.Symbol,
				OID:       t.ExchangeOrderID,
				Reason:    "gross_convention_row",
			})
			if t.IsClose {
				cash += tradeBackfillRowNetPnL(t)
			} else {
				cash -= t.ExchangeFee
			}
			continue
		}

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

		effectiveFee := newFee
		if !matched && matchAttempted {
			effectiveFee = modeledFee
		} else if !matched && !matchAttempted {
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

func planClosedPositionRecomputes(
	corrected []TradeBackfillRow,
	closedRows []ClosedPositionRow,
) []ClosedPositionRecompute {
	sumsByPID := make(map[string]float64)
	pidToSymbol := make(map[string]string)
	for _, t := range corrected {
		if !t.IsClose || t.PositionID == "" {
			continue
		}
		sumsByPID[t.PositionID] += t.RealizedPnL
		pidToSymbol[t.PositionID] = t.Symbol
	}

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

func runBackfill(args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "Usage: go-trader backfill <hl-fees|trade-ledger> [--config scheduler/config.json] (--all | --strategy <id>) [--apply]")
		return 2
	}
	switch args[0] {
	case "hl-fees":
		return runBackfillHLFees(args[1:])
	case "trade-ledger":
		return runBackfillTradeLedger(args[1:])
	default:
		fmt.Fprintf(os.Stderr, "Unknown backfill target %q\n", args[0])
		return 2
	}
}

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
		if usesSharedWalletPoolBudget(sc) {
			initialCapital = 0
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

func refuseIfSchedulerRunning() error {
	out, err := exec.Command("pgrep", "-x", "go-trader").Output()
	if err != nil {
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

const hlUserFillsFetchTimeout = 5 * time.Minute

func runFetchHLUserFills(since time.Time) (*HLUserFillsResult, error) {
	return runFetchHLUserFillsWithTimeout(since, hlUserFillsFetchTimeout)
}

func runFetchHLUserFillsWithTimeout(since time.Time, timeout time.Duration) (*HLUserFillsResult, error) {
	script := "shared_scripts/fetch_hl_user_fills.py"
	args := []string{
		fmt.Sprintf("--since-ms=%d", since.UnixMilli()),
	}

	stdout, stderr, runErr := runPythonWithTimeout(context.Background(), script, args, nil, timeout)
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

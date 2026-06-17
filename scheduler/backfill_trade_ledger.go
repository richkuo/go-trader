package main

import (
	"flag"
	"fmt"
	"math"
	"os"
	"sort"
	"time"
)

// `go-trader backfill trade-ledger` (#954): repairs the lossy columns of the
// trades table from userFills and migrates legacy net-convention rows to the
// gross convention, so the shared-wallet ledger display path
// (ledgerSharedWalletMemberValues) reads exchange-accurate values.
//
// Per HL-live strategy row, chronologically:
//
//  1. Legacy rows (pnl_gross=0) migrate: the fee that was deducted at booking
//     (stored exchange_fee when real, else the modeled taker fee) is stamped
//     into exchange_fee, close-leg realized_pnl gets that fee added back
//     (net → gross), pnl_gross=1, fee_source records provenance.
//  2. Rows whose OID matches a userFills aggregate are then trued up:
//     exchange_fee ← real fee, price/value ← fill VWAP, close-leg
//     realized_pnl ← exchange closedPnl (gross), fee_source='userfills'.
//     Rows sharing one OID (partial TP legs, resting-limit partial adds)
//     apportion the aggregate by quantity share.
//  3. strategies.cash is replayed under net semantics (close net PnL − open
//     fees; funding rows never touch cash), with the same SIGHUP-divergence
//     guard + --reset-cash override as `backfill hl-fees`.
//
// Idempotent: a second run against the same fills produces zero changes —
// migration keys off the pnl_gross marker and the userFills true-up converges.
// --apply also resets the ledger drift baseline of each wallet whose members
// were repaired (ResetWalletLedgerBaseline, scoped) so the repaired ledger
// re-anchors instead of reading the correction as drift; untouched wallets
// keep their baseline and any standing drift there keeps alarming.

// TradeLedgerChange is one trade-row rewrite produced by planTradeLedgerForStrategy.
type TradeLedgerChange struct {
	RowID        int64
	Timestamp    time.Time
	Symbol       string
	OID          string
	IsClose      bool
	OldPrice     float64
	NewPrice     float64
	OldValue     float64
	NewValue     float64
	OldFee       float64
	NewFee       float64
	OldPnL       float64
	NewPnL       float64
	WasGross     bool
	NewFeeSource string
}

// TradeLedgerPlan is the full change set for one strategy.
type TradeLedgerPlan struct {
	StrategyID            string
	Changes               []TradeLedgerChange
	Skipped               []SkippedTrade
	ClosedPositions       []ClosedPositionRecompute
	OldCash               float64
	NewCash               float64
	ReplayedCash          float64
	CashBaselineDivergent bool
	MigratedCount         int // legacy net→gross conversions
	MatchedCount          int // rows trued up from a userFills aggregate
	UnmatchedOIDCount     int
	MissingOIDCount       int
}

// tradeLedgerRowNewValues is the planner's per-row outcome.
type tradeLedgerRowNewValues struct {
	Price, Value, Fee, PnL float64
	FeeSource              string
}

type tradeLedgerOIDTotals struct {
	FeeQty   float64
	CloseQty float64
}

// planTradeLedgerForStrategy is the pure planner (no I/O).
func planTradeLedgerForStrategy(
	strategyID string,
	trades []TradeBackfillRow,
	fillMap map[string]HLFillSummary,
	initialCapital, oldCash float64,
) TradeLedgerPlan {
	return planTradeLedgerForStrategyWithOIDTotals(strategyID, trades, fillMap, initialCapital, oldCash, nil)
}

func planTradeLedgerForStrategyWithOIDTotals(
	strategyID string,
	trades []TradeBackfillRow,
	fillMap map[string]HLFillSummary,
	initialCapital, oldCash float64,
	oidTotals map[string]tradeLedgerOIDTotals,
) TradeLedgerPlan {
	plan := TradeLedgerPlan{StrategyID: strategyID, OldCash: oldCash}

	sorted := make([]TradeBackfillRow, len(trades))
	copy(sorted, trades)
	sort.SliceStable(sorted, func(i, j int) bool {
		return sorted[i].Timestamp.Before(sorted[j].Timestamp)
	})

	// Pre-correction replay with current on-disk values (net semantics) —
	// mirrors planBackfillForStrategy's SIGHUP-top-up divergence guard.
	preReplay := initialCapital
	for _, t := range sorted {
		if t.TradeType == TradeTypeFunding {
			continue
		}
		feePaid := t.ExchangeFee
		if feePaid == 0 && !t.PnLGross {
			feePaid = math.Abs(t.Value) * HyperliquidTakerFeePct
		}
		if t.IsClose {
			preReplay += tradeBackfillRowNetPnL(t)
		} else {
			preReplay -= feePaid
		}
	}
	plan.ReplayedCash = preReplay
	if math.Abs(preReplay-oldCash) > 1.0 {
		plan.CashBaselineDivergent = true
	}

	// Quantity totals per OID across this strategy's rows, optionally widened
	// to shared-wallet peers. A userFills aggregate spans every leg of the OID
	// — partial TP legs, resting-limit partial adds, bidirectional flip legs,
	// and shared-wallet external closes — so apportion by qty share instead
	// of letting each leg absorb the full aggregate. The fee was charged on
	// the whole order (all legs); closedPnl accrues only on close legs, so the
	// two use different denominators.
	feeQtyByOID := make(map[string]float64)
	closeQtyByOID := make(map[string]float64)
	for _, t := range sorted {
		if t.ExchangeOrderID == "" || t.TradeType == TradeTypeFunding {
			continue
		}
		feeQtyByOID[t.ExchangeOrderID] += t.Quantity
		if t.IsClose {
			closeQtyByOID[t.ExchangeOrderID] += t.Quantity
		}
	}
	feeQtyTotal := func(oid string) float64 {
		total := feeQtyByOID[oid]
		if oidTotals != nil && oidTotals[oid].FeeQty > total {
			total = oidTotals[oid].FeeQty
		}
		return total
	}
	closeQtyTotal := func(oid string) float64 {
		total := closeQtyByOID[oid]
		if oidTotals != nil && oidTotals[oid].CloseQty > total {
			total = oidTotals[oid].CloseQty
		}
		return total
	}

	cash := initialCapital
	for _, t := range sorted {
		if t.TradeType == TradeTypeFunding {
			continue // funding rows are written gross at ingestion; never cash
		}
		modeledFee := math.Abs(t.Value) * HyperliquidTakerFeePct

		nv := tradeLedgerRowNewValues{
			Price:     t.Price,
			Value:     t.Value,
			Fee:       t.ExchangeFee,
			PnL:       t.RealizedPnL,
			FeeSource: t.FeeSource,
		}

		// Step 1: legacy net → gross migration.
		if !t.PnLGross {
			feePaid := t.ExchangeFee
			source := FeeSourceUserFills
			if feePaid == 0 {
				feePaid = modeledFee
				source = FeeSourceModeled
			}
			nv.Fee = feePaid
			nv.FeeSource = source
			if t.IsClose {
				nv.PnL = t.RealizedPnL + feePaid
			}
		}

		// Step 2: userFills true-up when the OID matched.
		summary, matched := fillMap[t.ExchangeOrderID]
		switch {
		case t.ExchangeOrderID == "":
			plan.MissingOIDCount++
			plan.Skipped = append(plan.Skipped, SkippedTrade{
				RowID: t.RowID, Timestamp: t.Timestamp, Symbol: t.Symbol,
				Reason: "missing_oid",
			})
		case !matched:
			plan.UnmatchedOIDCount++
			plan.Skipped = append(plan.Skipped, SkippedTrade{
				RowID: t.RowID, Timestamp: t.Timestamp, Symbol: t.Symbol,
				OID: t.ExchangeOrderID, Reason: "no_fill_match",
			})
		default:
			feeShare := 1.0
			if total := feeQtyTotal(t.ExchangeOrderID); total > 0 && t.Quantity > 0 {
				feeShare = t.Quantity / total
			}
			nv.Fee = summary.Fee * feeShare
			nv.FeeSource = FeeSourceUserFills
			if summary.Px > 0 {
				nv.Price = summary.Px
				nv.Value = t.Quantity * summary.Px
			}
			if t.IsClose {
				pnlShare := 1.0
				if total := closeQtyTotal(t.ExchangeOrderID); total > 0 && t.Quantity > 0 {
					pnlShare = t.Quantity / total
				}
				// Exchange-reported gross closedPnl. For shared-coin peers HL
				// computes this against the ACCOUNT's average entry, so the
				// per-strategy attribution can shift slightly vs the locally
				// computed (px − AvgCost) value — the per-wallet SUM is exact,
				// which is what the drift alarm reconciles.
				nv.PnL = summary.ClosedPnLGross * pnlShare
			}
			plan.MatchedCount++
		}

		if !t.PnLGross {
			plan.MigratedCount++
		}

		changed := !t.PnLGross ||
			math.Abs(nv.Fee-t.ExchangeFee) > 1e-9 ||
			math.Abs(nv.PnL-t.RealizedPnL) > 1e-9 ||
			math.Abs(nv.Price-t.Price) > 1e-9 ||
			math.Abs(nv.Value-t.Value) > 1e-9 ||
			nv.FeeSource != t.FeeSource
		if changed {
			plan.Changes = append(plan.Changes, TradeLedgerChange{
				RowID: t.RowID, Timestamp: t.Timestamp, Symbol: t.Symbol,
				OID: t.ExchangeOrderID, IsClose: t.IsClose,
				OldPrice: t.Price, NewPrice: nv.Price,
				OldValue: t.Value, NewValue: nv.Value,
				OldFee: t.ExchangeFee, NewFee: nv.Fee,
				OldPnL: t.RealizedPnL, NewPnL: nv.PnL,
				WasGross: t.PnLGross, NewFeeSource: nv.FeeSource,
			})
		}

		// Cash replay with corrected values (net semantics).
		if t.IsClose {
			cash += nv.PnL - nv.Fee
		} else {
			cash -= nv.Fee
		}
	}
	plan.NewCash = cash
	return plan
}

// tradeLedgerCorrectedNetRows returns the strategy's rows with the planned
// values applied AND RealizedPnL converted to NET, for the closed_positions
// recompute (closed_positions stays net-of-fee).
func tradeLedgerCorrectedNetRows(trades []TradeBackfillRow, plan TradeLedgerPlan) []TradeBackfillRow {
	byRowID := make(map[int64]TradeLedgerChange, len(plan.Changes))
	for _, c := range plan.Changes {
		byRowID[c.RowID] = c
	}
	out := make([]TradeBackfillRow, 0, len(trades))
	for _, t := range trades {
		row := t
		if c, ok := byRowID[t.RowID]; ok {
			row.Price = c.NewPrice
			row.Value = c.NewValue
			row.ExchangeFee = c.NewFee
			row.RealizedPnL = c.NewPnL
			row.PnLGross = true
		}
		row.RealizedPnL = tradeBackfillRowNetPnL(row)
		row.PnLGross = false // values now net; prevent double subtraction downstream
		out = append(out, row)
	}
	return out
}

// ApplyTradeLedgerPlan commits one strategy's plan in a single transaction.
func (sdb *StateDB) ApplyTradeLedgerPlan(plan TradeLedgerPlan) error {
	if sdb == nil || sdb.db == nil {
		return fmt.Errorf("state db unavailable")
	}
	tx, err := sdb.db.Begin()
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback()

	stmt, err := tx.Prepare(
		"UPDATE trades SET price = ?, value = ?, exchange_fee = ?, realized_pnl = ?, pnl_gross = 1, fee_source = ? WHERE rowid = ?",
	)
	if err != nil {
		return fmt.Errorf("prepare trade update: %w", err)
	}
	defer stmt.Close()
	for _, c := range plan.Changes {
		if _, err := stmt.Exec(c.NewPrice, c.NewValue, c.NewFee, c.NewPnL, c.NewFeeSource, c.RowID); err != nil {
			return fmt.Errorf("update trade rowid=%d: %w", c.RowID, err)
		}
	}

	cpStmt, err := tx.Prepare(
		"UPDATE closed_positions SET realized_pnl = ? WHERE id = ? AND strategy_id = ?",
	)
	if err != nil {
		return fmt.Errorf("prepare closed_positions update: %w", err)
	}
	defer cpStmt.Close()
	for _, cp := range plan.ClosedPositions {
		if _, err := cpStmt.Exec(cp.NewPnL, cp.RowID, plan.StrategyID); err != nil {
			return fmt.Errorf("update closed_positions id=%d: %w", cp.RowID, err)
		}
	}

	if _, err := tx.Exec("UPDATE strategies SET cash = ? WHERE id = ?", plan.NewCash, plan.StrategyID); err != nil {
		return fmt.Errorf("update strategy cash: %w", err)
	}
	return tx.Commit()
}

func tradeLedgerDenominatorStrategies(strategies, targets []StrategyConfig) []StrategyConfig {
	byID := make(map[string]StrategyConfig, len(strategies))
	for _, sc := range strategies {
		byID[sc.ID] = sc
	}
	targetIDs := make(map[string]bool, len(targets))
	denomIDs := make(map[string]bool, len(targets))
	for _, sc := range targets {
		targetIDs[sc.ID] = true
		denomIDs[sc.ID] = true
	}
	for key, memberIDs := range detectSharedWallets(strategies) {
		members := sharedWalletMembersWithManual(key, memberIDs, strategies)
		selected := false
		for _, id := range members {
			if targetIDs[id] {
				selected = true
				break
			}
		}
		if !selected {
			continue
		}
		for _, id := range members {
			sc, ok := byID[id]
			if !ok || sc.Platform != "hyperliquid" || (sc.Type != "perps" && sc.Type != "manual") {
				continue
			}
			denomIDs[id] = true
		}
	}
	out := make([]StrategyConfig, 0, len(denomIDs))
	for id := range denomIDs {
		if sc, ok := byID[id]; ok {
			out = append(out, sc)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

func tradeLedgerSharedWalletOIDTotals(strategies []StrategyConfig, tradesByID map[string][]TradeBackfillRow) map[string]map[string]tradeLedgerOIDTotals {
	out := make(map[string]map[string]tradeLedgerOIDTotals)
	for key, memberIDs := range detectSharedWallets(strategies) {
		members := sharedWalletMembersWithManual(key, memberIDs, strategies)
		totals := make(map[string]tradeLedgerOIDTotals)
		for _, id := range members {
			for _, t := range tradesByID[id] {
				if t.ExchangeOrderID == "" || t.TradeType == TradeTypeFunding {
					continue
				}
				total := totals[t.ExchangeOrderID]
				total.FeeQty += t.Quantity
				if t.IsClose {
					total.CloseQty += t.Quantity
				}
				totals[t.ExchangeOrderID] = total
			}
		}
		if len(totals) == 0 {
			continue
		}
		for _, id := range members {
			if _, loaded := tradesByID[id]; loaded {
				out[id] = totals
			}
		}
	}
	return out
}

// runBackfillTradeLedger implements `go-trader backfill trade-ledger`.
// Dry-run by default; --apply commits and resets the per-wallet ledger drift
// baselines so the next reconcile re-anchors on the repaired ledger.
func runBackfillTradeLedger(args []string) int {
	fs := flag.NewFlagSet("backfill trade-ledger", flag.ContinueOnError)
	configPath := fs.String("config", "scheduler/config.json", "Path to config file")
	strategyID := fs.String("strategy", "", "Strategy ID to repair (mutually exclusive with --all)")
	all := fs.Bool("all", false, "Repair all live HL strategies")
	apply := fs.Bool("apply", false, "Commit changes (default: dry-run)")
	fs.Bool("dry-run", false, "Explicit dry-run (the default; provided for symmetry)")
	resetCash := fs.Bool("reset-cash", false, "Allow --apply when the pre-correction cash replay diverges from the stored value (e.g. after a SIGHUP capital top-up)")
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
	for _, sc := range cfg.Strategies {
		if sc.Platform != "hyperliquid" {
			continue
		}
		if sc.Type != "perps" && sc.Type != "manual" {
			continue
		}
		if sc.Type == "perps" && !hyperliquidIsLive(sc.Args) {
			if *strategyID == sc.ID {
				fmt.Fprintf(os.Stderr, "error: strategy %q is paper-mode (no real OIDs to match against userFills)\n", sc.ID)
				return 1
			}
			if *all {
				fmt.Printf("[%s] skipped: paper-mode (no real OIDs)\n", sc.ID)
			}
			continue
		}
		if *strategyID != "" && sc.ID != *strategyID {
			continue
		}
		targets = append(targets, sc)
	}
	if len(targets) == 0 {
		fmt.Fprintln(os.Stderr, "error: no matching live HL strategies found in config")
		return 1
	}
	sort.Slice(targets, func(i, j int) bool { return targets[i].ID < targets[j].ID })
	denomStrategies := tradeLedgerDenominatorStrategies(cfg.Strategies, targets)

	earliest, err := stateDB.EarliestTradeTimestamp(strategyIDsOf(denomStrategies))
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to read earliest trade timestamp: %v\n", err)
		return 1
	}
	if earliest.IsZero() {
		fmt.Fprintln(os.Stderr, "info: no trades found for the selected strategies — nothing to repair")
		return 0
	}
	queryStart := backfillUserFillsStartTime(earliest)
	fmt.Printf("Fetching HL userFills since %s (earliest trade %s, lookback %s)...\n",
		queryStart.UTC().Format(time.RFC3339), earliest.UTC().Format(time.RFC3339), backfillHLUserFillsLookback)
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

	tradesByID := make(map[string][]TradeBackfillRow, len(denomStrategies))
	for _, sc := range denomStrategies {
		trades, err := stateDB.ListTradesForBackfill(sc.ID)
		if err != nil {
			fmt.Fprintf(os.Stderr, "[%s] failed to list trades: %v\n", sc.ID, err)
			return 1
		}
		tradesByID[sc.ID] = trades
	}
	sharedOIDTotalsByID := tradeLedgerSharedWalletOIDTotals(cfg.Strategies, tradesByID)

	mode := "DRY-RUN"
	if *apply {
		mode = "APPLY"
	}
	fmt.Printf("\n=== %s mode ===\n", mode)

	exitCode := 0
	appliedIDs := make(map[string]bool)
	for _, sc := range targets {
		ss := state.Strategies[sc.ID]
		var oldCash, initialCapital float64
		if ss != nil {
			oldCash = ss.Cash
			initialCapital = ss.InitialCapital
		}
		if initialCapital == 0 {
			initialCapital = sc.Capital
		}

		trades := tradesByID[sc.ID]
		closedRows, err := stateDB.LoadClosedPositionRows(sc.ID)
		if err != nil {
			fmt.Fprintf(os.Stderr, "[%s] failed to load closed_positions: %v\n", sc.ID, err)
			exitCode = 1
			continue
		}

		plan := planTradeLedgerForStrategyWithOIDTotals(sc.ID, trades, fillResult.ByOID, initialCapital, oldCash, sharedOIDTotalsByID[sc.ID])
		plan.ClosedPositions = planClosedPositionRecomputes(tradeLedgerCorrectedNetRows(trades, plan), closedRows)
		printTradeLedgerReport(plan)

		if *apply {
			if plan.CashBaselineDivergent && !*resetCash {
				fmt.Fprintf(os.Stderr, "[%s] APPLY refused: cash baseline diverges from pre-correction replay by $%+.4f. Re-run with --reset-cash to acknowledge that the recomputed cash will not preserve mid-run capital top-ups.\n",
					sc.ID, plan.OldCash-plan.ReplayedCash)
				exitCode = 1
				continue
			}
			if len(plan.Changes) == 0 && len(plan.ClosedPositions) == 0 && math.Abs(plan.NewCash-plan.OldCash) <= 1e-9 {
				fmt.Printf("[%s] APPLY skipped: no changes (ledger already repaired)\n", sc.ID)
				continue
			}
			if err := stateDB.ApplyTradeLedgerPlan(plan); err != nil {
				fmt.Fprintf(os.Stderr, "[%s] APPLY failed: %v\n", sc.ID, err)
				exitCode = 1
				continue
			}
			appliedIDs[sc.ID] = true
			fmt.Printf("[%s] APPLY committed: %d trade rows, %d closed_positions, cash %.4f → %.4f\n",
				sc.ID, len(plan.Changes), len(plan.ClosedPositions), plan.OldCash, plan.NewCash)
		}
	}

	if len(appliedIDs) > 0 {
		// Repaired ledger sums shift Σ member values — recompute the drift
		// baseline on the next reconciled cycle instead of alarming on the
		// correction itself. Scoped to wallets whose members were actually
		// repaired: a targeted --strategy run must not re-anchor an unrelated
		// wallet's baseline (that would fold its genuine standing drift into
		// the new offset and silence a real alarm).
		resetWalletBaselinesForAppliedStrategies(stateDB, cfg.Strategies, appliedIDs)
	}
	if !*apply {
		fmt.Println("\n(dry-run — re-run with --apply to commit)")
	}
	return exitCode
}

// printTradeLedgerReport renders one strategy's summary block.
func printTradeLedgerReport(plan TradeLedgerPlan) {
	feeDelta, pnlDelta := 0.0, 0.0
	for _, c := range plan.Changes {
		feeDelta += c.OldFee - c.NewFee
		pnlDelta += c.NewPnL - c.OldPnL
	}
	fmt.Printf("\n--- %s ---\n", plan.StrategyID)
	fmt.Printf("  rows updated:        %d (net→gross migrated %d, userFills matched %d)\n",
		len(plan.Changes), plan.MigratedCount, plan.MatchedCount)
	fmt.Printf("  rows skipped:        %d (missing_oid=%d, unmatched=%d)\n",
		len(plan.Skipped), plan.MissingOIDCount, plan.UnmatchedOIDCount)
	fmt.Printf("  fee delta (sum):     $%+.4f\n", feeDelta)
	fmt.Printf("  pnl delta (sum):     $%+.4f (gross-convention values)\n", pnlDelta)
	fmt.Printf("  cash:                $%.4f → $%.4f (Δ %+.4f)\n",
		plan.OldCash, plan.NewCash, plan.NewCash-plan.OldCash)
	fmt.Printf("  closed_positions:    %d aggregate rows to update\n", len(plan.ClosedPositions))
	if plan.CashBaselineDivergent {
		fmt.Printf("  WARNING: cash baseline diverges from pre-correction replay by $%+.4f\n", plan.OldCash-plan.ReplayedCash)
		fmt.Printf("           (replayed=$%.4f vs stored=$%.4f) — --apply requires --reset-cash.\n", plan.ReplayedCash, plan.OldCash)
	}
}

// resetWalletBaselinesForAppliedStrategies clears the drift baseline of every
// shared wallet that has at least one repaired member (perps members from
// detectSharedWallets plus same-account live manual strategies — the same
// membership the reconcile uses). Wallets untouched by the apply keep their
// baseline so genuine standing drift there keeps alarming.
func resetWalletBaselinesForAppliedStrategies(sdb *StateDB, strategies []StrategyConfig, appliedIDs map[string]bool) {
	wallets := detectSharedWallets(strategies)
	keys := make([]SharedWalletKey, 0, len(wallets))
	for key := range wallets {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].Platform != keys[j].Platform {
			return keys[i].Platform < keys[j].Platform
		}
		return keys[i].Account < keys[j].Account
	})
	for _, key := range keys {
		members := sharedWalletMembersWithManual(key, wallets[key], strategies)
		touched := false
		for _, id := range members {
			if appliedIDs[id] {
				touched = true
				break
			}
		}
		if !touched {
			continue
		}
		if err := sdb.ResetWalletLedgerBaseline(key.Platform, key.Account); err != nil {
			fmt.Fprintf(os.Stderr, "warning: failed to reset ledger drift baseline for %s: %v\n", sharedWalletKeyLabel(key), err)
		} else {
			fmt.Printf("Reset ledger drift baseline for %s (recomputed next cycle).\n", sharedWalletKeyLabel(key))
		}
	}
}

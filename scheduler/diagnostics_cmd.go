package main

import (
	"database/sql"
	"encoding/json"
	"flag"
	"fmt"
	"math"
	"os"
	"sort"
	"strings"
)

// #1147 `go-trader diagnostics` — on-demand, read-only trade-quality report.
// Queries the trade_diagnostics table (capture side: trade_diagnostics.go),
// aggregates per strategy, and prints deterministic, backtestable tuning
// hypotheses. Diagnostics-only: opens the state DB mode=ro, never mutates
// positions, orders, or config, and nothing runs unless the operator invokes
// it.

// Hypothesis thresholds. Deliberately simple, documented heuristics — the
// report tells the operator what to INVESTIGATE (with the exact backtest
// command); it never claims statistical proof and never tunes anything.
const (
	diagDefaultMinTrades = 30  // closed positions before any hypothesis prints
	diagDefaultMinBucket = 10  // per regime/direction bucket before a split hypothesis prints
	diagCaptureLowMean   = 0.5 // mean winner capture ratio below this → exits leak gains
	diagMAEATRShare      = 0.5 // share of trades whose adverse excursion exceeded 1×entry-ATR → entries fire early
	diagLoserAtStopShare = 0.7 // share of losers whose MAE reached ~the stop distance → SL placement worth a sweep
	diagLoserAtStopSlack = 0.9 // "reached the stop" = adverse excursion ≥ this fraction of stop distance
)

// diagExcludedReason filters rows whose prices/PnL are synthetic or zeroed by
// construction, which would poison quality aggregates: hl_sync_external rows
// carry mark-based or zero close prices (see ClosedPosition doc), and
// *_corrupt / *_dup_oid legs book zero PnL (#1009).
func diagExcludedReason(reason string) bool {
	return reason == "hl_sync_external" ||
		strings.HasSuffix(reason, "_corrupt") ||
		strings.HasSuffix(reason, "_dup_oid")
}

func runDiagnostics(args []string) int {
	fs := flag.NewFlagSet("diagnostics", flag.ContinueOnError)
	configPath := fs.String("config", "scheduler/config.json", "Path to config file (used to locate the state DB)")
	dbPath := fs.String("db", "", "State DB path override (skips reading the config)")
	strategyID := fs.String("strategy", "", "Report a single strategy (default: fleet-wide)")
	minTrades := fs.Int("min-trades", diagDefaultMinTrades, "Closed positions required before hypotheses print")
	minBucket := fs.Int("min-bucket", diagDefaultMinBucket, "Bucket size required for regime/direction split hypotheses")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	path := *dbPath
	if path == "" {
		resolved, err := diagnosticsDBPathFromConfig(*configPath)
		if err != nil {
			fmt.Fprintf(os.Stderr, "diagnostics: %v\n", err)
			return 1
		}
		path = resolved
	}
	if _, err := os.Stat(path); err != nil {
		fmt.Fprintf(os.Stderr, "diagnostics: state DB %s not found: %v\n", path, err)
		return 1
	}

	// Read-only open (agent-info pattern): never migrates, never writes, safe
	// to run next to the live daemon.
	db, err := sql.Open("sqlite", "file:"+path+"?mode=ro&_pragma=busy_timeout(5000)")
	if err != nil {
		fmt.Fprintf(os.Stderr, "diagnostics: open %s: %v\n", path, err)
		return 1
	}
	defer db.Close()
	sdb := &StateDB{db: db}

	rows, err := sdb.TradeDiagnosticsRows(*strategyID)
	if err != nil {
		if strings.Contains(err.Error(), "no such table") {
			fmt.Println("No diagnostics recorded yet (the trade_diagnostics table is created on the daemon's next start; rows appear as positions close).")
			return 0
		}
		fmt.Fprintf(os.Stderr, "diagnostics: %v\n", err)
		return 1
	}
	netByPos, err := sdb.NetPnLByPosition(*strategyID)
	if err != nil {
		fmt.Fprintf(os.Stderr, "diagnostics: %v\n", err)
		return 1
	}

	fmt.Print(buildTradeDiagnosticsReport(rows, netByPos, *configPath, diagReportOptions{
		MinTrades: *minTrades,
		MinBucket: *minBucket,
	}))
	return 0
}

// diagnosticsDBPathFromConfig extracts db_file from the config JSON without
// loadConfig (which normalizes and can rewrite the file in place — a
// read-only report must not touch it).
func diagnosticsDBPathFromConfig(path string) (string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read config %s: %w", path, err)
	}
	var probe struct {
		DBFile string `json:"db_file"`
	}
	if err := json.Unmarshal(raw, &probe); err != nil {
		return "", fmt.Errorf("parse config %s: %w", path, err)
	}
	if probe.DBFile == "" {
		return "scheduler/state.db", nil
	}
	return probe.DBFile, nil
}

type diagReportOptions struct {
	MinTrades int
	MinBucket int
}

// diagStrategyStats is the per-strategy aggregate the report prints.
type diagStrategyStats struct {
	StrategyID string
	Symbol     string
	Timeframe  string

	Total    int // all rows incl. excluded-reason rows
	Excluded int // synthetic rows (hl_sync_external, *_corrupt, *_dup_oid) kept out of the aggregates below
	N        int // rows contributing to stats
	Wins     int
	Losses   int
	NetPnL   float64

	MetricsOK     int
	StatusCounts  map[string]int
	CaptureVals   []float64 // winners only
	FavorableVals []float64
	AdverseVals   []float64
	MAEOverATR    int // trades whose adverse excursion ≥ 1× entry ATR (as % of entry)
	MAEOverATRN   int // trades where that comparison was computable
	LosersAtStop  int // losers whose MAE reached ≥ diagLoserAtStopSlack × stop distance
	LosersWithSL  int // losers where stop distance was computable

	Regimes    map[string]*diagBucket
	Directions map[string]*diagBucket
}

type diagBucket struct {
	N      int
	Wins   int
	NetPnL float64
}

func (b *diagBucket) expectancy() float64 {
	if b.N == 0 {
		return 0
	}
	return b.NetPnL / float64(b.N)
}

// diagRowNetPnL resolves a row's net PnL: the summed convention-aware net
// over ALL close legs of the position (multi-leg exits aggregate) when the
// trades join can attribute it, else the final-leg pre-fee PnL stored on the
// row.
func diagRowNetPnL(r TradeDiagnosticsRow, netByPos map[string]map[string]float64) float64 {
	if r.PositionID != "" {
		if byPos, ok := netByPos[r.StrategyID]; ok {
			if net, ok := byPos[r.PositionID]; ok {
				return net
			}
		}
	}
	return r.RealizedPnL
}

func aggregateTradeDiagnostics(rows []TradeDiagnosticsRow, netByPos map[string]map[string]float64) map[string]*diagStrategyStats {
	stats := make(map[string]*diagStrategyStats)
	for _, r := range rows {
		st := stats[r.StrategyID]
		if st == nil {
			st = &diagStrategyStats{
				StrategyID:   r.StrategyID,
				StatusCounts: make(map[string]int),
				Regimes:      make(map[string]*diagBucket),
				Directions:   make(map[string]*diagBucket),
			}
			stats[r.StrategyID] = st
		}
		st.Total++
		if r.Symbol != "" {
			st.Symbol = r.Symbol
		}
		if r.Timeframe != "" {
			st.Timeframe = r.Timeframe
		}
		if diagExcludedReason(r.CloseReason) {
			st.Excluded++
			continue
		}
		st.N++
		st.StatusCounts[r.MetricsStatus]++
		net := diagRowNetPnL(r, netByPos)
		st.NetPnL += net
		win := net > 0
		if win {
			st.Wins++
		} else {
			st.Losses++
		}

		regime := r.RegimeAtOpen
		if regime == "" {
			regime = "(none)"
		}
		rb := st.Regimes[regime]
		if rb == nil {
			rb = &diagBucket{}
			st.Regimes[regime] = rb
		}
		rb.N++
		rb.NetPnL += net
		if win {
			rb.Wins++
		}

		dir := r.Side
		if dir == "" {
			dir = "long"
		}
		db := st.Directions[dir]
		if db == nil {
			db = &diagBucket{}
			st.Directions[dir] = db
		}
		db.N++
		db.NetPnL += net
		if win {
			db.Wins++
		}

		if r.MetricsStatus == diagMetricsOK {
			st.MetricsOK++
			if r.FavorablePct != nil {
				st.FavorableVals = append(st.FavorableVals, *r.FavorablePct)
			}
			if r.AdversePct != nil {
				st.AdverseVals = append(st.AdverseVals, *r.AdversePct)
				if r.EntryATR > 0 && r.EntryPrice > 0 {
					atrPct := r.EntryATR / r.EntryPrice * 100
					st.MAEOverATRN++
					if *r.AdversePct >= atrPct {
						st.MAEOverATR++
					}
				}
				if !win && r.StopLossATRMult != nil && *r.StopLossATRMult > 0 && r.EntryATR > 0 && r.EntryPrice > 0 {
					stopPct := *r.StopLossATRMult * r.EntryATR / r.EntryPrice * 100
					st.LosersWithSL++
					if *r.AdversePct >= diagLoserAtStopSlack*stopPct {
						st.LosersAtStop++
					}
				}
			}
			if win && r.CaptureRatio != nil {
				st.CaptureVals = append(st.CaptureVals, *r.CaptureRatio)
			}
		}
	}
	return stats
}

func diagMean(vals []float64) float64 {
	if len(vals) == 0 {
		return math.NaN()
	}
	sum := 0.0
	for _, v := range vals {
		sum += v
	}
	return sum / float64(len(vals))
}

// diagHypothesis is one printed finding: what the metric shows, what to try,
// and the exact backtest command that validates it.
type diagHypothesis struct {
	Tag      string
	Finding  string
	Try      string
	Validate string
}

func diagBaselineCommand(cfgPath, strategyID string) string {
	return fmt.Sprintf("uv run --no-sync python backtest/run_backtest.py --config %s --strategy %s --mode single", cfgPath, strategyID)
}

// diagHypotheses derives the deterministic hypothesis list for one strategy.
// Sample-size gating happens in the caller (buildTradeDiagnosticsReport) so
// the "insufficient data" shortfall can be printed instead.
func diagHypotheses(st *diagStrategyStats, cfgPath string, minBucket int) []diagHypothesis {
	var out []diagHypothesis
	baseline := diagBaselineCommand(cfgPath, st.StrategyID)
	variantHint := func(edit string) string {
		return fmt.Sprintf("cp %s /tmp/diag-%s.json  # then edit strategy %q: %s\nuv run --no-sync python backtest/run_backtest.py --config /tmp/diag-%s.json --strategy %s --mode single  # compare against baseline:\n%s",
			cfgPath, st.StrategyID, st.StrategyID, edit, st.StrategyID, st.StrategyID, baseline)
	}

	if mean := diagMean(st.CaptureVals); len(st.CaptureVals) >= minBucket && mean < diagCaptureLowMean {
		out = append(out, diagHypothesis{
			Tag:      "capture",
			Finding:  fmt.Sprintf("winners captured only %.0f%% of their max favorable move on average (n=%d winners with metrics)", mean*100, len(st.CaptureVals)),
			Try:      "exits leave gains on the table — test wider TP tiers (larger atr_multiple) or a looser trailing stop",
			Validate: variantHint("widen tp_tiers atr_multiple values / raise trailing_stop_atr_mult"),
		})
	}
	if st.MAEOverATRN >= minBucket {
		share := float64(st.MAEOverATR) / float64(st.MAEOverATRN)
		if share > diagMAEATRShare {
			out = append(out, diagHypothesis{
				Tag:      "mae",
				Finding:  fmt.Sprintf("%.0f%% of trades went more than 1× entry ATR against the position before resolving (n=%d)", share*100, st.MAEOverATRN),
				Try:      "entries fire early — tighten the regime gate (allowed_regimes) or add a confirmation filter to the open strategy",
				Validate: variantHint("restrict allowed_regimes to the profitable labels below"),
			})
		}
	}
	regimeKeys := make([]string, 0, len(st.Regimes))
	for k := range st.Regimes {
		regimeKeys = append(regimeKeys, k)
	}
	sort.Strings(regimeKeys)
	if len(regimeKeys) > 1 {
		for _, k := range regimeKeys {
			b := st.Regimes[k]
			if b.N >= minBucket && b.expectancy() < 0 {
				out = append(out, diagHypothesis{
					Tag:      "regime",
					Finding:  fmt.Sprintf("regime %q is net-negative: expectancy $%.2f/trade over %d trades", k, b.expectancy(), b.N),
					Try:      fmt.Sprintf("gate it out — drop %q from allowed_regimes", k),
					Validate: variantHint(fmt.Sprintf("remove %q from allowed_regimes", k)),
				})
			}
		}
	}
	dirKeys := make([]string, 0, len(st.Directions))
	for k := range st.Directions {
		dirKeys = append(dirKeys, k)
	}
	sort.Strings(dirKeys)
	if len(dirKeys) > 1 {
		for _, k := range dirKeys {
			b := st.Directions[k]
			if b.N >= minBucket && b.expectancy() < 0 {
				out = append(out, diagHypothesis{
					Tag:      "direction",
					Finding:  fmt.Sprintf("%s side is net-negative: expectancy $%.2f/trade over %d trades", k, b.expectancy(), b.N),
					Try:      fmt.Sprintf("restrict direction away from %s (direction / directional-policy certification)", k),
					Validate: variantHint(fmt.Sprintf("set the strategy direction to exclude %s entries", k)),
				})
			}
		}
	}
	if st.LosersWithSL >= minBucket {
		share := float64(st.LosersAtStop) / float64(st.LosersWithSL)
		if share >= diagLoserAtStopShare {
			out = append(out, diagHypothesis{
				Tag:      "stop",
				Finding:  fmt.Sprintf("%.0f%% of losers ran to ~the stop distance before closing (n=%d losers with ATR-stop data)", share*100, st.LosersWithSL),
				Try:      "stop placement is the binding exit — sweep stop_loss_atr_mult wider (stopped-then-reversed) AND tighter (oversized losers) to find the better trade-off",
				Validate: variantHint("sweep stop_loss_atr_mult (e.g. 0.75×, 1.25×, 1.5× the current value)"),
			})
		}
	}
	return out
}

func buildTradeDiagnosticsReport(rows []TradeDiagnosticsRow, netByPos map[string]map[string]float64, cfgPath string, opts diagReportOptions) string {
	if opts.MinTrades <= 0 {
		opts.MinTrades = diagDefaultMinTrades
	}
	if opts.MinBucket <= 0 {
		opts.MinBucket = diagDefaultMinBucket
	}
	var b strings.Builder
	if len(rows) == 0 {
		b.WriteString("No diagnostics rows recorded yet (rows appear as positions close).\n")
		return b.String()
	}
	stats := aggregateTradeDiagnostics(rows, netByPos)
	ids := make([]string, 0, len(stats))
	for id := range stats {
		ids = append(ids, id)
	}
	sort.Strings(ids)

	fmt.Fprintf(&b, "Trade diagnostics — %d closed positions across %d strategies\n", len(rows), len(ids))
	b.WriteString("(diagnostics-only: nothing here changes config or live trading)\n\n")

	for _, id := range ids {
		st := stats[id]
		fmt.Fprintf(&b, "=== %s", id)
		if st.Symbol != "" {
			fmt.Fprintf(&b, " (%s", st.Symbol)
			if st.Timeframe != "" {
				fmt.Fprintf(&b, " %s", st.Timeframe)
			}
			b.WriteString(")")
		}
		b.WriteString(" ===\n")
		fmt.Fprintf(&b, "closed positions: %d", st.N)
		if st.Excluded > 0 {
			fmt.Fprintf(&b, " (+%d excluded: external-sync/corrupt legs)", st.Excluded)
		}
		if st.N > 0 {
			fmt.Fprintf(&b, "   wins: %d  losses: %d  win rate: %.0f%%   net PnL: $%.2f",
				st.Wins, st.Losses, float64(st.Wins)/float64(st.N)*100, st.NetPnL)
		}
		b.WriteString("\n")
		fmt.Fprintf(&b, "quality metrics: %d/%d rows computed", st.MetricsOK, st.N)
		if st.MetricsOK < st.N {
			statusKeys := make([]string, 0, len(st.StatusCounts))
			for k := range st.StatusCounts {
				if k != diagMetricsOK {
					statusKeys = append(statusKeys, k)
				}
			}
			sort.Strings(statusKeys)
			parts := make([]string, 0, len(statusKeys))
			for _, k := range statusKeys {
				parts = append(parts, fmt.Sprintf("%s=%d", k, st.StatusCounts[k]))
			}
			fmt.Fprintf(&b, " (%s)", strings.Join(parts, ", "))
		}
		b.WriteString("\n")
		if fav := diagMean(st.FavorableVals); !math.IsNaN(fav) {
			fmt.Fprintf(&b, "avg favorable excursion: %.2f%%   avg adverse excursion: %.2f%%\n", fav, diagMean(st.AdverseVals))
		}
		if capture := diagMean(st.CaptureVals); !math.IsNaN(capture) {
			fmt.Fprintf(&b, "avg capture ratio (winners): %.2f (n=%d)\n", capture, len(st.CaptureVals))
		}

		writeBuckets := func(label string, buckets map[string]*diagBucket) {
			if len(buckets) == 0 {
				return
			}
			keys := make([]string, 0, len(buckets))
			for k := range buckets {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			parts := make([]string, 0, len(keys))
			for _, k := range keys {
				bk := buckets[k]
				parts = append(parts, fmt.Sprintf("%s: n=%d win%%=%.0f exp=$%.2f", k, bk.N, float64(bk.Wins)/float64(bk.N)*100, bk.expectancy()))
			}
			fmt.Fprintf(&b, "%s: %s\n", label, strings.Join(parts, "  |  "))
		}
		writeBuckets("regime split", st.Regimes)
		writeBuckets("direction split", st.Directions)

		if st.N < opts.MinTrades {
			fmt.Fprintf(&b, "hypotheses: insufficient data, %d/%d closed trades\n\n", st.N, opts.MinTrades)
			continue
		}
		hyps := diagHypotheses(st, cfgPath, opts.MinBucket)
		if len(hyps) == 0 {
			b.WriteString("hypotheses: none — no metric crossed its threshold\n\n")
			continue
		}
		b.WriteString("hypotheses:\n")
		for _, h := range hyps {
			fmt.Fprintf(&b, "- [%s] %s\n  try: %s\n  validate:\n", h.Tag, h.Finding, h.Try)
			for _, line := range strings.Split(h.Validate, "\n") {
				fmt.Fprintf(&b, "    %s\n", line)
			}
		}
		b.WriteString("\n")
	}
	return b.String()
}

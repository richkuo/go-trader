package main

import (
	"bytes"
	"fmt"
	"html/template"
	"net/http"
	"strings"
)

// ui_reports.go (#956): serves the strategy-audit report as an in-dashboard HTML
// page under a "Reports" section. The audit data lives here as Go structs — the
// single source of truth feeding the template — so the page never drifts from the
// numbers in the issue-956 audit comment. Loopback-only, same auth/draining posture
// as the rest of the UI; no external assets (styling reuses /dashboard/styles.css).

// auditRow is one strategy's aggregated backtest result across the 6 (symbol,
// timeframe) runs. Verdict is a CSS class (keep|watch|deprecate|bug|na); VerdictLabel
// is the human display text (may carry a qualifier, e.g. "watch (short leg unmeasured)").
type auditRow struct {
	Strategy     string
	Registry     string
	Sharpe       float64
	ReturnPct    float64
	VsBH         float64
	HasVsBH      bool
	WorstDD      float64
	Trades       int
	Verdict      string
	VerdictLabel string
}

func (r auditRow) SharpeText() string { return fmt.Sprintf("%.2f", r.Sharpe) }
func (r auditRow) ReturnText() string { return fmt.Sprintf("%.1f", r.ReturnPct) }
func (r auditRow) DDText() string     { return fmt.Sprintf("%.1f", r.WorstDD) }
func (r auditRow) VsBHText() string {
	if !r.HasVsBH {
		return "—"
	}
	return fmt.Sprintf("%+.1f", r.VsBH)
}

// VsBHSort returns a sort key that pushes the unmeasured ("—") rows to the bottom
// of an ascending sort instead of letting a 0 interleave with real edges.
func (r auditRow) VsBHSort() float64 {
	if !r.HasVsBH {
		return -1000
	}
	return r.VsBH
}

// oosRow is one strategy's out-of-sample check (BTC/USDT 1h, since 2026-01-01).
type oosRow struct {
	Strategy  string
	ReturnPct float64
	Sharpe    float64
	Trades    int
	EdgeVsBH  float64
	Holds     string
}

func (r oosRow) ReturnText() string { return fmt.Sprintf("%.1f", r.ReturnPct) }
func (r oosRow) SharpeText() string { return fmt.Sprintf("%.2f", r.Sharpe) }
func (r oosRow) EdgeText() string   { return fmt.Sprintf("%+.1f", r.EdgeVsBH) }

// deprecationItem and candidateVerdict are the narrative sections of the report.
type deprecationItem struct {
	Strategy  string
	Rationale string
}

type candidateVerdict struct {
	Name    string
	Verdict string // CONFIRM | CUT | BLOCKED
	Body    string
}

// reportMeta describes one available report for the /reports index.
type reportMeta struct {
	Slug        string
	Title       string
	Description string
	Generated   string
}

// strategyAuditReport is the full dataset for /reports/strategy-audit.
type strategyAuditReport struct {
	Meta          reportMeta
	Method        []string
	Caveats       []string
	Ranking       []auditRow
	Deprecations  []deprecationItem
	SupertrendBug string
	OOS           []oosRow
	OOSNote       string
	Candidates    []candidateVerdict
}

// availableReports is the index registry. Add new reports here.
var availableReports = []reportMeta{strategyAuditReportData.Meta}

// strategyAuditReportData is the single source of truth for the audit page, mirroring
// the audit comment on issue #956 (2026-06-10, backtest-only, 12-month bear window).
var strategyAuditReportData = strategyAuditReport{
	Meta: reportMeta{
		Slug:        "strategy-audit",
		Title:       "Strategy audit",
		Description: "Ranking of every registered open strategy by out-of-sample Sharpe and drawdown-adjusted return, with deprecation candidates.",
		Generated:   "2026-06-10",
	},
	Method: []string{
		"Runs: backtest/run_backtest.py --mode compare --strategy all, both registries (spot + futures), symbols BTC/USDT, ETH/USDT, SOL/USDT, timeframes 1h + 4h, --since 2025-06-10 (12 months). 12 compare runs, all completed.",
		"Fee model: default binanceus (0.1% taker per side). Close stack: default (open-signal-as-close, no SL/TP) — isolates entry quality, which is what this audit ranks.",
		"Aggregation: per strategy, mean Sharpe / mean total return / worst max DD / summed trades across the 6 (symbol, timeframe) runs. Spot and futures rows deduped to \"both\" when byte-identical; momentum differs and is listed per registry.",
		"Market context: a deep bear — buy-and-hold BTC -44.3%, ETH -40.2%, SOL -61.2%. The vs B&H column (mean return minus same-run buy-and-hold) separates real entry edge from market beta.",
	},
	Caveats: []string{
		"Backtest-only. state.db is not in this checkout (production DB lives on the server), so live leaderboard / lifetime stats were unavailable.",
		"Long/flat path only. Compare mode never opens shorts; bidirectional strategies are measured on their long leg only; short-only strategies produce 0 trades and are unmeasurable here.",
		"Bar-level granularity; no intra-bar trigger races; no funding or leverage modeling in this mode.",
	},
	Ranking: []auditRow{
		{"squeeze_momentum", "both", 0.03, -0.6, 47.9, true, -58.5, 131, "keep", "keep"},
		{"breakout", "futures", 0.01, -1.1, 47.5, true, -52.2, 260, "keep", "keep"},
		{"supertrend", "both", 0.00, 0.0, 0, false, 0.0, 0, "bug", "bug — never trades"},
		{"delta_neutral_funding", "futures", 0.00, 0.0, 0, false, 0.0, 0, "na", "n/a (needs funding data)"},
		{"bear_pullback_st", "futures", 0.00, 0.0, 0, false, 0.0, 0, "na", "n/a (short-only, unmeasured)"},
		{"vwap_rejection_st", "futures", 0.00, 0.0, 0, false, 0.0, 0, "na", "n/a (short-only, unmeasured)"},
		{"hold", "both", 0.00, 0.0, 0, false, 0.0, 0, "na", "n/a (placeholder)"},
		{"mean_reversion_pro", "both", -0.20, -12.8, 35.8, true, -59.8, 17, "watch", "watch"},
		{"chart_pattern", "both", -0.27, -11.5, 37.1, true, -60.5, 185, "watch", "watch"},
		{"momentum_pro", "both", -0.32, -12.0, 36.5, true, -55.9, 51, "watch", "watch"},
		{"range_scalper", "both", -0.38, -14.2, 34.4, true, -55.9, 9, "deprecate", "deprecate (M1 degenerate; M5 gross <= 0)"},
		{"donchian_breakout", "both", -0.42, -17.6, 31.0, true, -55.5, 355, "deprecate", "deprecate (#985: no gate/profile clears protocol OOS + held-outs)"},
		{"ichimoku_cloud", "both", -0.43, -8.5, 40.1, true, -65.5, 173, "watch", "watch"},
		{"order_blocks", "both", -0.52, -19.9, 28.7, true, -63.4, 295, "watch", "watch"},
		{"amd_ifvg", "both", -0.53, -19.6, 28.9, true, -63.4, 87, "deprecate", "deprecate (#1023: DST/session-timing corrected + 15m rebaseline; passes OOS but failed 2023/2024 held-outs on both 15m and 1h/4h)"},
		{"sweep_squeeze_combo", "both", -0.55, -29.4, 19.2, true, -73.5, 26, "watch", "watch"},
		{"momentum", "spot", -0.58, -22.1, 26.5, true, -58.1, 126, "watch", "watch"},
		{"sma_crossover", "both", -0.68, -24.0, 24.5, true, -70.7, 360, "watch", "watch"},
		{"volume_weighted", "both", -0.72, -26.6, 22.0, true, -64.6, 287, "watch", "watch"},
		{"session_breakout", "futures", -0.79, -28.4, 20.2, true, -67.3, 371, "deprecate", "deprecate (#1031: short leg failed bull-year held-outs)"},
		{"atr_breakout", "both", -0.89, -22.7, 25.9, true, -69.8, 389, "watch", "watch"},
		{"tema_cross", "both", -0.90, -17.9, 30.6, true, -49.5, 451, "watch", "watch"},
		{"momentum", "futures", -0.92, -30.2, 18.4, true, -62.3, 287, "watch", "watch"},
		{"bollinger_bands", "both", -1.08, -44.0, 4.6, true, -60.5, 344, "deprecate", "deprecate"},
		{"adx_trend", "both", -1.10, -38.5, 10.1, true, -76.1, 204, "deprecate", "deprecate"},
		{"liquidity_sweeps", "both", -1.10, -42.0, 6.6, true, -63.0, 127, "deprecate", "deprecate (short leg failed held-outs)"},
		{"rsi", "both", -1.11, -45.3, 3.3, true, -63.8, 118, "deprecate", "deprecate"},
		{"triple_ema", "both", -1.12, -32.6, 16.0, true, -58.1, 512, "deprecate", "deprecate"},
		{"rsi_macd_combo", "both", -1.13, -46.0, 2.6, true, -64.6, 180, "deprecate", "deprecate"},
		{"tema_cross_bd", "futures", -1.16, -41.8, 6.8, true, -65.9, 806, "watch", "watch (short leg unmeasured)"},
		{"ema_crossover", "both", -1.33, -40.8, 7.8, true, -66.8, 559, "deprecate", "deprecate"},
		{"stoch_rsi", "both", -1.42, -46.6, 1.9, true, -65.2, 664, "deprecate", "deprecate"},
		{"pairs_spread", "spot", -1.45, -47.9, 0.7, true, -72.0, 374, "deprecate", "deprecate"},
		{"mean_reversion", "both", -1.46, -49.5, -0.9, true, -70.5, 496, "deprecate", "deprecate"},
		{"triple_ema_bidir", "futures", -1.55, -46.6, 2.0, true, -69.8, 800, "watch", "watch (short leg unmeasured)"},
		{"heikin_ashi_ema", "both", -1.56, -48.7, -0.2, true, -74.9, 801, "deprecate", "deprecate"},
		{"parabolic_sar", "both", -1.84, -55.8, -7.3, true, -72.6, 1327, "deprecate", "deprecate"},
		{"macd", "both", -1.86, -55.6, -7.1, true, -80.8, 1264, "deprecate", "deprecate"},
		{"consolidation_range", "futures", -1.88, -58.5, -9.9, true, -74.5, 624, "watch", "watch (short leg unmeasured)"},
		{"vwap_reversion", "both", -1.88, -59.5, -10.9, true, -78.9, 1504, "deprecate", "deprecate"},
	},
	Deprecations: []deprecationItem{
		{"vwap_reversion", "Worst in registry: -59.5% mean, 1,504 trades, edge -10.9pts below buy-and-hold; pure fee churn."},
		{"macd", "-55.6% mean over 1,264 trades, -80.8% worst DD, below B&H."},
		{"parabolic_sar", "-55.8% over 1,327 trades, below B&H; whipsaws every regime."},
		{"heikin_ashi_ema", "-48.7% over 801 trades, zero edge vs B&H."},
		{"mean_reversion", "-49.5% over 496 trades, negative edge; superseded by mean_reversion_pro (regime-filtered variant)."},
		{"pairs_spread", "-47.9%, +0.7pt edge; spot-only and structurally homeless (no second-leg execution in live)."},
		{"stoch_rsi", "-46.6% over 664 trades, +1.9pt edge; pure churn."},
		{"rsi_macd_combo", "-46.0%, +2.6pt edge; the combo inherits both parents' weaknesses."},
		{"rsi", "-45.3%, +3.3pt edge over 118 trades."},
		{"liquidity_sweeps", "Long leg was -16.94% gross in M5; #1022 short-leg screen passes protocol OOS but fails 2023/2024 held-outs with SOL liquidations."},
		{"range_scalper", "#987 current-cache M1 is degenerate in every window (0/3 held-outs) and M5 remains gross-negative (-7.46% gross over 7 trades)."},
		{"session_breakout", "#1031 short leg: gross edge real on OOS (Sharpe up to 2.50) but bear gate + atr_stop + zscore all fail the 2023/2024 bull-year held-outs (best 1/3); the short edge can't survive an uptrend and exits amputate the long-hold winners."},
		{"amd_ifvg", "#1023: session timing was UTC-fixed/DST-unaware and the M5 deprecate baseline ran on 1h/4h, not the designed 15m. Corrected to NY-anchored ICT killzones (DST-aware civil sessions) and rebaselined at 15m + 1h/4h. The corrected baseline clears protocol OOS but fails the 2023 and 2024 held-out years on both timeframes (1/3 held-out) — competitive only in chop, lags incumbents in trends. Concept disproven on a sound implementation."},
		{"donchian_breakout", "#985 full M1: the long leg (shipped default) fails every window ungated, and no mechanism rescues it — ADX trending_up gate and composite trending_up_clean gate flip IS only, the entry_period plateau wanders per window (30 passes 2023, 40 passes 2024, nothing passes 2025H1), the best tuned config (period 40 + gate) fails the one protocol-OOS look on DDadj, and the #977 M4 dual profile (20/55 on the ADX switch) fails all five windows. The short leg has a real OOS edge (Sharpe 1.62 gated vs bar -0.75) but fails the 2023/2024 bull-year held-outs at every period and under both classifiers (best 1/3) — the #1031 session_breakout failure shape. Both sides measured per the M5 unscreened_short requirement."},
		{"bollinger_bands", "-44.0%, +4.6pt edge over 344 trades."},
		{"ema_crossover", "-40.8% over 559 trades; dominated by sma_crossover and tema_cross."},
		{"adx_trend", "-38.5%, -76.1% worst DD; trend filter that doesn't filter."},
		{"triple_ema", "-32.6% over 512 trades; dominated by tema_cross (-17.9% on the same idea)."},
	},
	SupertrendBug: "supertrend_strategy produces zero signals on any input: the recursive band update seeds from NaN (rolling-ATR warmup), so final_upper/final_lower stay NaN for the entire series — verified empirically (8,783 bars, 0 signals, direction stuck at -1). Any live config running supertrend has been silently flat. It is a band-seeding bug, not a performance verdict.",
	OOS: []oosRow{
		{"momentum_pro", -3.1, -0.09, 5, 27.3, "yes — best OOS"},
		{"chart_pattern", -6.4, -0.37, 23, 24.0, "yes"},
		{"breakout", -6.8, -0.39, 33, 23.6, "yes"},
		{"squeeze_momentum", -11.7, -0.78, 21, 18.7, "mostly"},
		{"mean_reversion_pro", -18.6, -1.84, 2, 11.8, "weak (2 trades)"},
		{"donchian_breakout", -20.5, -1.53, 43, 9.9, "weak"},
		{"ichimoku_cloud", -26.6, -2.73, 23, 3.8, "no"},
		{"range_scalper", -38.3, -2.69, 5, -7.9, "no — worse than B&H"},
	},
	OOSNote: "Top-4 ordering is stable in and out of sample (BTC/USDT 1h, since 2026-01-01; B&H -30.4%): squeeze_momentum, breakout, momentum_pro, chart_pattern keep a 19-27pt edge over buy-and-hold in the unseen window. ichimoku_cloud's in-sample edge evaporates OOS and stays watch; range_scalper underperforms holding and #987 moves it to deprecate after M1/M5 confirmed a degenerate, gross-negative sample.",
	Candidates: []candidateVerdict{
		{"Multi-timeframe confluence", "CONFIRM", "The single biggest failure mode is high-frequency long entries fighting a year-long downtrend: every strategy with 300+ trades churned to -30%..-60%, and 4h runs consistently beat their 1h twins. An HTF trend gate over an LTF pullback entry attacks exactly this; highest-conviction candidate."},
		{"Regime-adaptive entry", "CONFIRM", "Style alone did not decide outcomes: squeeze/breakout top the table while unfiltered mean reversion is the bottom — yet mean_reversion_pro, the ADX-gated variant, ranks in the top tier of strategies that traded. Regime selectivity, not entry style, separated winners from losers."},
		{"Volatility-targeted momentum", "CONFIRM", "Raw momentum is mid-table, but momentum_pro (confirmation-gated) posted the best out-of-sample edge of any strategy on very few, selective trades. ATR-normalized sizing plus efficiency confirmation addresses the churn that killed macd/parabolic_sar. Build it bidirectional."},
		{"Funding-rate aware perps entry", "BLOCKED", "The audit could not evaluate funding signals at all: delta_neutral_funding runs 0 trades because the backtest path has no funding data. Untested rather than disproven; add funding-data support to the backtester first, otherwise the strategy cannot clear its own gate."},
		{"Volume-profile / liquidity levels", "CUT", "The existing structure/liquidity family is mid-to-bottom tier: order_blocks -0.52, amd_ifvg -0.53, sweep_squeeze_combo -0.55, liquidity_sweeps -1.10 mean Sharpe — none survived fees and none shows OOS promise. Cut it (or fold untested-level confluence into candidate 1)."},
	},
}

// handleReports serves the reports index (/reports) and dispatches the audit page
// (/reports/strategy-audit). Loopback-only and drain-aware like the rest of the UI.
func (ss *StatusServer) handleReports(w http.ResponseWriter, r *http.Request) {
	if ss.rejectIfDraining(w) {
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	switch r.URL.Path {
	case "/reports", "/reports/":
		ss.renderReportsIndex(w)
	case "/reports/strategy-audit":
		ss.renderHTML(w, strategyAuditTemplate, strategyAuditReportData)
	default:
		http.NotFound(w, r)
	}
}

func (ss *StatusServer) renderReportsIndex(w http.ResponseWriter) {
	ss.renderHTML(w, reportsIndexTemplate, availableReports)
}

func (ss *StatusServer) renderHTML(w http.ResponseWriter, tmpl *template.Template, data interface{}) {
	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, data); err != nil {
		http.Error(w, "report render failed", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write(buf.Bytes())
}

// reportPageFuncs are shared template helpers.
var reportPageFuncs = template.FuncMap{
	"lower": strings.ToLower,
}

const reportHeadHTML = `<!doctype html>
<html lang="en">
  <head>
    <meta charset="utf-8">
    <meta name="viewport" content="width=device-width, initial-scale=1">
    <title>{{.Title}} · go-trader reports</title>
    <script>
      (function () {
        try {
          if (window.localStorage.getItem("goTraderDarkMode") === "1") {
            document.documentElement.classList.add("dark");
          }
        } catch (e) {}
      })();
    </script>
    <link rel="stylesheet" href="/dashboard/styles.css">
    <style>
      .report-shell { max-width: 1100px; margin: 0 auto; padding: 24px 20px 64px; }
      .report-shell h1 { font-size: 1.6rem; margin: 0 0 4px; }
      .report-shell h2 { font-size: 1.15rem; margin: 32px 0 10px; border-bottom: 1px solid var(--line); padding-bottom: 6px; }
      .report-nav { display: flex; gap: 12px; align-items: center; margin-bottom: 18px; font-size: 0.9rem; }
      .report-nav a { color: var(--blue); text-decoration: none; }
      .report-nav a:hover { text-decoration: underline; }
      .report-meta { color: var(--muted); font-size: 0.88rem; margin: 0 0 8px; }
      .report-list { list-style: none; padding: 0; margin: 0; }
      .report-list li { border: 1px solid var(--line); border-radius: 10px; padding: 14px 16px; margin-bottom: 12px; background: var(--panel); }
      .report-list a { font-size: 1.05rem; font-weight: 600; color: var(--text); text-decoration: none; }
      .report-list a:hover { color: var(--blue); }
      .report-list p { margin: 6px 0 0; color: var(--muted); font-size: 0.9rem; }
      .report-notes { margin: 0 0 12px; padding-left: 20px; color: var(--text); font-size: 0.92rem; }
      .report-notes li { margin-bottom: 6px; }
      table.report-table { width: 100%; border-collapse: collapse; font-size: 0.86rem; background: var(--panel); }
      table.report-table th, table.report-table td { padding: 6px 10px; border-bottom: 1px solid var(--line); text-align: right; white-space: nowrap; }
      table.report-table th:first-child, table.report-table td:first-child,
      table.report-table th.txt, table.report-table td.txt { text-align: left; }
      table.report-table thead th { position: sticky; top: 0; background: var(--panel); border-bottom: 2px solid var(--line-strong); }
      table.report-table th.sortable { cursor: pointer; user-select: none; }
      table.report-table th.sortable:hover { color: var(--blue); }
      table.report-table tbody tr:hover { background: var(--strategy-hover); }
      .verdict { display: inline-block; padding: 1px 8px; border-radius: 999px; font-size: 0.78rem; font-weight: 600; }
      .verdict-keep { background: rgba(15,138,95,0.14); color: var(--green); }
      .verdict-watch { background: rgba(164,95,8,0.14); color: var(--amber); }
      .verdict-deprecate { background: rgba(194,59,59,0.14); color: var(--red); }
      .verdict-bug { background: rgba(194,59,59,0.22); color: var(--red); }
      .verdict-na { background: var(--position-line); color: var(--muted); }
      .neg { color: var(--red); }
      .pos { color: var(--green); }
      .cand { border: 1px solid var(--line); border-radius: 10px; padding: 12px 14px; margin-bottom: 10px; background: var(--panel); }
      .cand h3 { margin: 0 0 6px; font-size: 1rem; display: flex; gap: 10px; align-items: center; }
      .cand p { margin: 0; color: var(--text); font-size: 0.9rem; }
      .tag { font-size: 0.72rem; font-weight: 700; padding: 2px 8px; border-radius: 999px; }
      .tag-confirm { background: rgba(15,138,95,0.16); color: var(--green); }
      .tag-cut { background: rgba(194,59,59,0.16); color: var(--red); }
      .tag-blocked { background: rgba(164,95,8,0.16); color: var(--amber); }
      .callout { border-left: 3px solid var(--amber); background: var(--auth-bg); padding: 10px 14px; border-radius: 0 8px 8px 0; font-size: 0.9rem; margin: 10px 0; }
    </style>
  </head>
  <body>
    <div class="report-shell">
      <div class="report-nav">
        <a href="/dashboard">← Dashboard</a>
        <span>·</span>
        <a href="/reports">Reports</a>
      </div>`

const reportFootHTML = `
    </div>
  </body>
</html>`

var reportsIndexTemplate = template.Must(template.New("reports-index").Funcs(reportPageFuncs).Parse(
	strings.ReplaceAll(reportHeadHTML, "{{.Title}}", "Reports") + `
      <h1>Reports</h1>
      <p class="report-meta">Generated analyses surfaced from the trading engine. Loopback-only.</p>
      <ul class="report-list">
        {{range .}}
        <li>
          <a href="/reports/{{.Slug}}">{{.Title}}</a>
          <p>{{.Description}}</p>
          <p class="report-meta">Generated {{.Generated}}</p>
        </li>
        {{end}}
      </ul>` + reportFootHTML))

var strategyAuditTemplate = template.Must(template.New("strategy-audit").Funcs(reportPageFuncs).Parse(
	strings.ReplaceAll(reportHeadHTML, "{{.Title}}", "{{.Meta.Title}}") + `
      <h1>{{.Meta.Title}}</h1>
      <p class="report-meta">{{.Meta.Description}} · Generated {{.Meta.Generated}} · backtest-only</p>

      <h2>Method</h2>
      <ul class="report-notes">{{range .Method}}<li>{{.}}</li>{{end}}</ul>

      <h2>Caveats</h2>
      <ul class="report-notes">{{range .Caveats}}<li>{{.}}</li>{{end}}</ul>

      <h2>Ranking</h2>
      <p class="report-meta">Sorted by mean Sharpe across the 6 runs. Click a column header to sort.</p>
      <table class="report-table" id="ranking">
        <thead>
          <tr>
            <th class="txt sortable" data-type="text">Strategy</th>
            <th class="txt sortable" data-type="text">Registry</th>
            <th class="sortable" data-type="num">Mean Sharpe</th>
            <th class="sortable" data-type="num">Mean return %</th>
            <th class="sortable" data-type="num">vs B&amp;H %</th>
            <th class="sortable" data-type="num">Worst max DD %</th>
            <th class="sortable" data-type="num">Trades</th>
            <th class="txt sortable" data-type="text">Verdict</th>
          </tr>
        </thead>
        <tbody>
          {{range .Ranking}}
          <tr>
            <td class="txt">{{.Strategy}}</td>
            <td class="txt">{{.Registry}}</td>
            <td data-sort="{{.Sharpe}}">{{.SharpeText}}</td>
            <td data-sort="{{.ReturnPct}}" class="{{if lt .ReturnPct 0.0}}neg{{else if gt .ReturnPct 0.0}}pos{{end}}">{{.ReturnText}}</td>
            <td data-sort="{{.VsBHSort}}">{{.VsBHText}}</td>
            <td data-sort="{{.WorstDD}}">{{.DDText}}</td>
            <td data-sort="{{.Trades}}">{{.Trades}}</td>
            <td class="txt"><span class="verdict verdict-{{.Verdict}}">{{.VerdictLabel}}</span></td>
          </tr>
          {{end}}
        </tbody>
      </table>

      <h2>Deprecation candidates ({{len .Deprecations}})</h2>
      <p class="report-meta">Bar: mean Sharpe ≤ -1.0 across all 6 runs, ≥100 trades, edge vs B&amp;H ≤ ~+16pts. Removal is a separate PR — the registry --list-json output must stay byte-identical until then.</p>
      <ol class="report-notes">{{range .Deprecations}}<li><strong>{{.Strategy}}</strong> — {{.Rationale}}</li>{{end}}</ol>

      <div class="callout"><strong>Bug found:</strong> {{.SupertrendBug}}</div>

      <h2>Out-of-sample check</h2>
      <table class="report-table">
        <thead>
          <tr>
            <th class="txt">Strategy</th>
            <th>OOS return %</th>
            <th>OOS Sharpe</th>
            <th>Trades</th>
            <th>Edge vs B&amp;H</th>
            <th class="txt">Holds?</th>
          </tr>
        </thead>
        <tbody>
          {{range .OOS}}
          <tr>
            <td class="txt">{{.Strategy}}</td>
            <td class="{{if lt .ReturnPct 0.0}}neg{{else}}pos{{end}}">{{.ReturnText}}</td>
            <td>{{.SharpeText}}</td>
            <td>{{.Trades}}</td>
            <td class="{{if lt .EdgeVsBH 0.0}}neg{{else}}pos{{end}}">{{.EdgeText}}</td>
            <td class="txt">{{.Holds}}</td>
          </tr>
          {{end}}
        </tbody>
      </table>
      <p class="report-meta">{{.OOSNote}}</p>

      <h2>Verdicts on the five candidate strategies</h2>
      {{range .Candidates}}
      <div class="cand">
        <h3>{{.Name}} <span class="tag tag-{{lower .Verdict}}">{{.Verdict}}</span></h3>
        <p>{{.Body}}</p>
      </div>
      {{end}}

      <script>
        (function () {
          var table = document.getElementById("ranking");
          if (!table) return;
          var headers = table.querySelectorAll("th.sortable");
          headers.forEach(function (th, idx) {
            var asc = true;
            th.addEventListener("click", function () {
              var type = th.getAttribute("data-type");
              var rows = Array.prototype.slice.call(table.tBodies[0].rows);
              rows.sort(function (a, b) {
                var ca = a.cells[idx], cb = b.cells[idx];
                if (type === "num") {
                  var va = parseFloat(ca.getAttribute("data-sort"));
                  var vb = parseFloat(cb.getAttribute("data-sort"));
                  return asc ? va - vb : vb - va;
                }
                var ta = ca.textContent.trim().toLowerCase();
                var tb = cb.textContent.trim().toLowerCase();
                return asc ? ta.localeCompare(tb) : tb.localeCompare(ta);
              });
              asc = !asc;
              rows.forEach(function (r) { table.tBodies[0].appendChild(r); });
            });
          });
        })();
      </script>` + reportFootHTML))

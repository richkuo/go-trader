import json
import os
import statistics

HERE = os.path.dirname(os.path.abspath(__file__))


def _f(v, prec=2):
    if v is None:
        return "-"
    if isinstance(v, bool):
        return "yes" if v else "no"
    if isinstance(v, (int, float)):
        return f"{v:.{prec}f}"
    return str(v)


def _params(p: dict) -> str:
    return ", ".join(f"{k}={v}" for k, v in p.items() if k != "expected_interval_ms")


def _legs(row: dict) -> list:
    return [l for l in row["legs"].values() if l is not None]


def _sum(row: dict, key: str) -> float:
    return sum(l[key] or 0.0 for l in _legs(row))


def _funding_lines(results: dict) -> list:
    lines = ["| group | window | leg | dataset | source | frame hours | priced | venue-absent | missing | "
             "unpriced held hours | rows sha256 |",
             "|---|---|---|---|---|---|---|---|---|---|---|"]
    for g, gd in results["groups"].items():
        for w, row in gd["confirmation"].items():
            entries = [("candidate", row)] + [(c, rows[w]) for c, rows in gd["comparators"].items()]
            for label, r in entries:
                for ds, l in r["legs"].items():
                    if l is None:
                        lines.append(f"| {g} | {w} | {label} | {ds} | no data | - | - | - | - | - | - |")
                        continue
                    f = l["funding"]
                    lines.append(
                        f"| {g} | {w} | {label} | {ds} | {f['source']} | {f['expected_hours']} | "
                        f"{f['priced_hours']} | {f['venue_absent_hours']} | {f['missing_hours']} | "
                        f"{f['unpriced_held_hours']} | `{(f['sha256'] or '-')[:16]}` |")
    return lines


def render(proto: dict, results_path: str, manifest_path: str, report_path: str) -> None:
    with open(results_path) as fh:
        res = json.load(fh)
    with open(manifest_path) as fh:
        manifest = json.load(fh)
    parity = None
    ppath = os.path.join(HERE, "parity_results.json")
    if os.path.exists(ppath):
        with open(ppath) as fh:
            parity = json.load(fh)
    verdict = res["verdict"]
    source = {"pass": "unvalidated", "fail": "study_fail", "inconclusive": "study_inconclusive"}[verdict]
    cm = res["cost_model"]
    out = []
    w = out.append
    w("# Chande Momentum Oscillator study (issue 1661)")
    w("")
    w(f"**Verdict: {verdict.upper()}.** Registry metadata after this study: `edge_status=\"no_edge\"`, "
      f"`edge_source=\"{source}\"`, `edge_ref` = this file. The candidate stays hidden from discovery. "
      "Explicit paper mode is admitted; live use still needs `allow_no_edge: true`, and no result here adds one. "
      "Every command in this study is suggest-only and sends no order.")
    w("")
    w("## Signal")
    w("")
    w("`chande_momentum` computes `100 * (G - L) / (G + L)` over `period` unsmoothed close changes, where G sums the "
      "up changes and L sums the absolute down changes. It enters long when the value rises strictly above "
      "`threshold` from at or below it, and short when it falls strictly below `-threshold` from at or above it. "
      "A flat window (G + L = 0) reads 0 and fires nothing on that bar. Each decision reads exactly `period + 2` "
      "closes; every close must be finite and positive and every step inside both windows must equal the fixed "
      "`expected_interval_ms`, else the entry holds with a reason and close/stop management continues.")
    w("")
    w("For a nonzero denominator the value equals `2 * RSI_sma - 100`, where `RSI_sma` uses simple averages of "
      "the gains and losses over the window. The registry `rsi` strategy is different: it uses recursive Wilder "
      "averages (`indicators_core.wilder_rsi`) that carry state from all earlier bars, and it trades reversion "
      "crossings of the oversold and overbought levels. `chande_momentum` trades momentum crossings and its value "
      "depends only on its bounded window.")
    w("")
    w("## Protocol")
    w("")
    w(f"`protocol.json` (sha256 `{res['protocol_sha256']}`) was written and frozen before any held-out window "
      "was scored. It fixes the grids, the selection rule on the `is` window, the confirmation windows, the "
      "sample floor, the funding rule and the pass/fail/inconclusive rules. Selection read only `is`; "
      "the stability sweep ran after confirmation and does not change the verdict.")
    w("")
    if proto.get("amendment"):
        w(f"Amendment made before any scoring: {proto['amendment']}")
        w("")
    w("| window | start | end (exclusive) | role |")
    w("|---|---|---|---|")
    for name, (s, e) in proto["windows"].items():
        role = "training/selection" if name == proto["training_window"] else (
            "held-out" if name in proto["held_out_windows"] else "protocol out-of-sample")
        w(f"| {name} | {s} | {e} | {role} |")
    w("")
    w("These are the M1 window definitions, with the open-ended `oos` end pinned to "
      f"{proto['pinned_end']}. The M1 `is` window (from 2025-06-10) overlaps the last three weeks of `2025H1`; "
      "the `2025H1` result is therefore not fully independent of selection.")
    w("")
    w("Exits for the candidate and both matched-exit comparators: one `tiered_tp_pct` tier at +3% closing the "
      "whole position, plus the single stop owner `stop_loss_atr_mult: 1.0`, `direction: both`, "
      f"`intrabar_resolution: {proto['intrabar_resolution']}`, funding `{proto['funding_mode']}`, "
      "strict close capability checks. The M1 incumbent median uses the eight fixed incumbents with registry "
      "defaults and default exits, unchanged.")
    w("")
    w("## Inputs, costs and their limits")
    w("")
    w(f"- Candles: Binance US spot USDT pairs through the M1 non-manifest path. They proxy Hyperliquid perpetual "
      "prices; basis and venue-specific wicks are not modeled.")
    w(f"- Funding: Hyperliquid hourly funding through the shared funding helper, charged on every held hour "
      "(`--funding charge`).")
    w(f"- Fees: the engine's Hyperliquid fee model (commission_pct `{cm['commission_pct']}` per side as reported by "
      f"the Backtester). Slippage: engine default `{cm['slippage_pct']}` per fill, which stands in for the "
      "spread; no order book was replayed.")
    w(f"- Capital {cm['capital_usd']} USD per leg; notionals below are the engine's actual entry notionals. "
      "Lot size, size decimals and the venue minimum notional are not modeled.")
    w(f"- Frozen inputs: `study_manifest.json` (acquired {manifest['acquired_at_utc']} from commit "
      f"`{manifest['source_commit']}`); `run_study.py run` rebuilds a fresh cache from `data/`, checks every "
      "sha256 and disables network acquisition.")
    w("")
    w("| file | rows | sha256 |")
    w("|---|---|---|")
    for name, meta in manifest["files"].items():
        w(f"| `data/{name}` | {meta['rows']} | `{meta['sha256']}` |")
    w("")
    gaps = []
    for name, meta in manifest["files"].items():
        if meta["kind"] == "candles":
            gaps.append(f"{meta['symbol']} {meta['timeframe']}: {meta.get('gap_count', '?')} gaps, "
                        f"{meta.get('missing_bars', '?')} missing bars")
        else:
            gaps.append(f"{meta['coin']} funding: {meta.get('pre_venue_hours', 0)} hours before the first venue rate, "
                        f"{len(meta['venue_absent_hours'])} venue-absent hours")
    w("Input gaps: " + "; ".join(gaps) + ". The candidate holds its entry for the bars whose windows contain a "
      "candle gap; the engine still manages open positions.")
    w("")
    for g, gd in res["groups"].items():
        sel = gd["selections"]
        w(f"## {g} group (expected_interval_ms = {gd['expected_interval_ms']})")
        w("")
        w("### Training selection (`is`)")
        w("")
        w("| strategy | selected params | how | eligible combos | is mean DDadj | is mean Sharpe |")
        w("|---|---|---|---|---|---|")
        for name, s in sel.items():
            w(f"| {name} | {_params(s['params'])} | {s['how']} | {s['eligible']} | {_f(s.get('mean_ddadj'))} | "
              f"{_f(s.get('mean_sharpe'))} |")
        w("")
        w("Every training combination (unsuccessful variants included):")
        w("")
        w("| strategy | params | M1 verdict | mean Sharpe | bar Sharpe | mean DDadj | bar DDadj | min trades |")
        w("|---|---|---|---|---|---|---|---|")
        for name, rows in gd["training"].items():
            for r in rows:
                trades = min((l["trades"] for l in _legs(r)), default=0)
                w(f"| {name} | {_params(r['params'])} | {r['verdict']} | {_f(r['mean_sharpe'])} | "
                  f"{_f(r['mean_bar_sharpe'])} | {_f(r['mean_ddadj'])} | {_f(r['mean_bar_ddadj'])} | {trades} |")
        w("")
        w("### Confirmation (locked selections)")
        w("")
        w("| window | leg | M1 verdict | mean Sharpe | bar Sharpe | mean DDadj | bar DDadj | mean return % | "
          "worst max DD % | positions (L/S) | net PnL USD | fees USD | funding USD | turnover | mean entry notional |")
        w("|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|")
        for win, row in gd["confirmation"].items():
            entries = [("candidate", row)] + [(c, rows[win]) for c, rows in gd["comparators"].items()]
            for label, r in entries:
                legs = _legs(r)
                if not legs:
                    w(f"| {win} | {label} | no data | | | | | | | | | | | | |")
                    continue
                notional = [l["mean_entry_notional_usd"] for l in legs if l["mean_entry_notional_usd"]]
                w(f"| {win} | {label} | {r['verdict']} | {_f(r['mean_sharpe'])} | {_f(r['mean_bar_sharpe'])} | "
                  f"{_f(r['mean_ddadj'])} | {_f(r['mean_bar_ddadj'])} | "
                  f"{_f(statistics.mean(l['return_pct'] for l in legs))} | "
                  f"{_f(min(l['max_dd_pct'] for l in legs))} | "
                  f"{sum(l['positions'] for l in legs)} ({sum(l['long_positions'] for l in legs)}/"
                  f"{sum(l['short_positions'] for l in legs)}) | {_f(_sum(r, 'net_pnl_usd'))} | "
                  f"{_f(_sum(r, 'fees_usd'))} | {_f(_sum(r, 'funding_pnl_usd'))} | "
                  f"{_f(statistics.mean(l['turnover'] or 0.0 for l in legs))} | "
                  f"{_f(statistics.mean(notional) if notional else None)} |")
        w("")
        w("Per-dataset candidate legs:")
        w("")
        w("| window | dataset | return % | max DD % | Sharpe | DDadj | positions | net PnL USD | B&H % | exits |")
        w("|---|---|---|---|---|---|---|---|---|---|")
        for win, row in gd["confirmation"].items():
            for ds, l in row["legs"].items():
                if l is None:
                    w(f"| {win} | {ds} | no data | | | | | | | |")
                    continue
                w(f"| {win} | {ds} | {_f(l['return_pct'])} | {_f(l['max_dd_pct'])} | {_f(l['sharpe'])} | "
                  f"{_f(l['ddadj'])} | {l['positions']} | {_f(l['net_pnl_usd'])} | {_f(l['bh_return_pct'])} | "
                  f"{', '.join(l['exit_reasons'])} |")
        w("")
        v = gd["verdict"]
        c = v["checks"]
        w(f"**{g} verdict: {v['verdict'].upper()}** ({v['reason']}).")
        w("")
        cond = c["conditions"]
        tri = lambda x: "unknown" if x is None else ("true" if x else "false")
        w("| condition | value |")
        w("|---|---|")
        for k in ("oos_pass", "held_out_passes", "net_pnl_positive", "beats_comparators", "not_concentrated"):
            w(f"| {k} | {tri(cond[k])} |")
        w("")
        w(f"- scorable confirmation windows: {', '.join(c['scorable_windows']) or 'none'}")
        for uw, why in c["unscorable_windows"].items():
            w(f"- unscorable `{uw}` (funding incomplete or missing): {', '.join(why)}")
        w(f"- held-out M1 passes among scorable windows: {c['held_out_passes']}")
        w(f"- summed candidate net PnL on scorable windows: {_f(c['net_pnl_usd'])} USD over {c['positions']} "
          "independent positions")
        w(f"- mean return % on scorable windows: candidate {_f(c['mean_return_pct'])}; "
          + "; ".join(f"{k} {_f(x)}" for k, x in c["comparator_mean_return_pct"].items()))
        w(f"- largest single position share of summed winning PnL: {_f(c['top_position_share'], 4)}")
        w("")
        stab = gd.get("stability")
        if stab:
            w("### Stability sweep (exploratory, after confirmation)")
            w("")
            w("| window | combos | M1 pass | positive mean return | median mean return % |")
            w("|---|---|---|---|---|")
            for win, rows in stab.items():
                rets = [r["mean_return_pct"] for r in rows]
                w(f"| {win} | {len(rows)} | {sum(1 for r in rows if r['verdict'] == 'pass')} | "
                  f"{sum(1 for x in rets if x > 0)} | {_f(statistics.median(rets))} |")
            w("")
    w("## Funding coverage")
    w("")
    w("`frame hours` counts every hour of the leg's candle frame the helper tried to price (all fetched hours); "
      "`unpriced held hours` counts only hours a position was actually held without a funding rate. A charge "
      "leg with any unpriced held hour is funding-incomplete and cannot support a pass. Source `cache` means the "
      "rows came from the frozen inputs.")
    w("")
    out.extend(_funding_lines(res))
    w("")
    w("## Candle frames scored")
    w("")
    w("| dataset window | rows | first | last | sha256 |")
    w("|---|---|---|---|---|")
    for k, v in res["frames"].items():
        w(f"| {k} | {v['rows']} | {v['first']} | {v['last']} | `{v['sha256'][:16]}` |")
    w("")
    if parity is not None:
        w("## Decision parity (paper configurations)")
        w("")
        w("`run_study.py parity` reads `paper_parity_1h.json` / `paper_parity_4h.json` through "
          "`parity_diff.config_from_live_config` (the `--config` parser), feeds the frozen BTC/USDT candles, and runs "
          "`compute_parity_frame` non-batched and `--batched` at window 200 and window 0 (every prefix). Strict "
          "mode reports close parity `unverified` with `clean = true`; that is the strict-mode label for an "
          "eligible close set with no approximation.")
        w("")
        w("| config | params | batched | window | candles | bars compared | mismatches | batch mismatches | "
          "long/short live signals | clean |")
        w("|---|---|---|---|---|---|---|---|---|---|")
        for r in parity["runs"]:
            w(f"| {r['config']} | {r['params_label']} ({_params(r['params'])}) | {_f(r['batched'])} | {r['window']} | "
              f"{r['candles']} | {r['bars_compared']} | {r['mismatches']} | {_f(r['batch_mismatches'], 0)} | "
              f"{r['live_long_signals']}/{r['live_short_signals']} | {_f(r['clean'])} |")
        w("")
        w(f"All parity runs clean: {_f(parity['clean'])}.")
        w("")
    w("## Reproduce")
    w("")
    w("```bash")
    w("uv run --no-sync python backtest/candidates/chande_momentum_1661/run_study.py acquire --cache-db /tmp/cmo_acquire.db  # network; rewrites data/")
    w("uv run --no-sync python backtest/candidates/chande_momentum_1661/run_study.py run --cache-db /tmp/cmo_run.db")
    w("uv run --no-sync python backtest/candidates/chande_momentum_1661/run_study.py parity --cache-db /tmp/cmo_parity.db")
    w("uv run --no-sync python backtest/candidates/chande_momentum_1661/run_study.py render")
    w("```")
    w("")
    w("`run` and `parity` refuse an existing cache path and rebuild it from `data/`. The interval specs "
      "`spec_1h.json` / `spec_4h.json` (seeds) and `selected_1h.json` / `selected_4h.json` (locked selections) "
      "also run through the stock M1 CLI, for example:")
    w("")
    w("```bash")
    w("GO_TRADER_OHLCV_CACHE_DB=/tmp/cmo_run.db uv run --no-sync python backtest/eval_windows.py \\")
    w("  --candidate-json backtest/candidates/chande_momentum_1661/selected_1h.json --registry futures \\")
    w("  --datasets BTC/USDT:1h,ETH/USDT:1h,SOL/USDT:1h --windows 2023,2024,2025H1 --funding charge")
    w("```")
    w("")
    w("The stock CLI keeps `oos` open-ended, so `run_study.py` pins its end to "
      f"{proto['pinned_end']} instead.")
    w("")
    w(f"Study runtime: {res.get('runtime_seconds')} s on commit `{res['source_commit']}`.")
    with open(report_path, "w") as fh:
        fh.write("\n".join(out) + "\n")

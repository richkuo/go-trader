import json
import os


def _f(v, prec=2):
    if v is None:
        return "-"
    if isinstance(v, bool):
        return "yes" if v else "no"
    if isinstance(v, (int, float)):
        return f"{v:.{prec}f}"
    return str(v)


def _p(params: dict) -> str:
    return ", ".join(f"{k}={v}" for k, v in params.items())


def _legs(row: dict) -> list:
    return [l for l in row["legs"].values() if l is not None]


def _sum(row: dict, key: str) -> float:
    return sum(l[key] or 0.0 for l in _legs(row))


def _arm_line(group, window, label, row) -> str:
    legs = _legs(row)
    return (f"| {group} | {window} | {label} | {row['verdict']} | {_f(row['mean_sharpe'])} | {_f(row['mean_ddadj'])} | "
            f"{_f(row['mean_bar_sharpe'])} | {_f(row['mean_bar_ddadj'])} | {_f(_sum(row, 'net_pnl_usd'))} | "
            f"{sum(l['positions'] for l in legs)} | {sum(l['long_positions'] for l in legs)}/"
            f"{sum(l['short_positions'] for l in legs)} | {_f(_sum(row, 'fees_usd'))} | "
            f"{_f(_sum(row, 'funding_pnl_usd'))} | {_f(_sum(row, 'turnover'))} |")


def render(proto: dict, results_path: str, manifest_path: str, parity_path: str, report_path: str) -> None:
    with open(results_path) as fh:
        res = json.load(fh)
    with open(manifest_path) as fh:
        manifest = json.load(fh)
    parity = None
    if os.path.exists(parity_path):
        with open(parity_path) as fh:
            parity = json.load(fh)
    verdict = res["verdict"]
    source = {"pass": "unvalidated", "fail": "study_fail", "inconclusive": "study_inconclusive"}[verdict]
    L = []
    L.append("# adaptive_moving_average study (issue 1641)")
    L.append("")
    L.append(f"**Verdict: {verdict.upper()}.** " + "; ".join(
        f"{g}: {gd['verdict']['verdict']} ({gd['verdict']['reason']})" for g, gd in res["groups"].items()) + ".")
    L.append("")
    L.append(f"The registry and the Go mirror keep `edge_status=\"no_edge\"` with `edge_source=\"{source}\"` and this "
             "file as `edge_ref`. A study result never removes the no-edge label: paper evaluation needs one explicit "
             "`--mode=paper`, and live use still needs `allow_no_edge: true` plus a separate reviewed edge decision. "
             "Nothing here edits deployed configuration, live defaults or acknowledgements; the study is suggest-only.")
    L.append("")
    L.append("## Candidate")
    L.append("")
    L.append("Entry-only Kaufman adaptive moving average (`shared_strategies/open/adaptive_moving_average.py`). For "
             "efficiency period E, efficiency at bar k is |close k - close k-E| divided by the sum of the E absolute "
             "one-bar close changes ending at k (0 when that sum is 0). The smoothing constant is "
             "(efficiency x (2/(fast+1) - 2/(slow+1)) + 2/(slow+1)) squared. Every average at bar t restarts from "
             "close t-L and applies exactly L updates (t-L+1 through t), so one decision reads L + E + max(S, 1) rows "
             "and full-series, prefix and bounded-frame decisions are identical. Long when close t > average t, "
             "close t-1 <= average t-1 and average t - average t-S > 0; short is the exact mirror. Equality at the "
             "current bar and a zero slope hold. Bad prices, bad volume, timestamp defects, missing cadence and "
             "cadence gaps inside the window hold the entry with a reason; the close owner and the stop keep running.")
    L.append("")
    L.append(f"Close owner: `{proto['close_strategies'][0]['name']}` "
             f"(`{json.dumps(proto['close_strategies'][0]['params'])}`); stop owner: "
             f"`stop_loss_atr_mult={proto['stop_loss_atr_mult']}`; candidate direction "
             f"`{proto['directions'][proto['candidate']]}`. Seed: {_p(proto['seed_params'])} "
             "(133 rows of history). The registered limit is 199 rows.")
    L.append("")
    L.append("## Protocol and provenance")
    L.append("")
    L.append(f"- Protocol: `protocol.json`, sha256 `{res['protocol_sha256']}` (frozen before scoring; "
             f"`study_manifest.json` records the same hash at freeze time, sha256 `{res['manifest_sha256']}`).")
    L.append(f"- Source commit of the run: `{res['source_commit']}`; runtime {res['runtime_seconds']} s.")
    L.append(f"- Inputs: the frozen files of study `{proto['data']['source_study']}`, read in place and verified "
             f"against the pins in `protocol.json` (source manifest sha256 `{proto['data']['source_manifest_sha256']}`, "
             f"acquired {manifest['source_manifest']['acquired_at_utc']} at commit "
             f"`{manifest['source_manifest']['source_commit']}`). The run rebuilds a fresh cache from them and "
             "disables every network fetch.")
    L.append(f"- Windows (start inclusive, end exclusive): " + "; ".join(
        f"`{n}` {s} to {e}" for n, (s, e) in proto["windows"].items()) + f". Training window `{proto['training_window']}`; "
             f"held-out `{', '.join(proto['held_out_windows'])}`. The open-ended M1 oos window is pinned to "
             f"{proto['pinned_end']}.")
    L.append(f"- Warmup: {proto['warmup_rule']}.")
    L.append(f"- Costs: commission {proto['cost_model']['commission_pct'] * 100:.3f}% per side, slippage "
             f"{proto['cost_model']['slippage_pct'] * 100:.3f}% per fill, stress x"
             f"{proto['cost_model']['stress']['slippage_multiplier']} slippage; funding `--funding "
             f"{proto['funding_mode']}`.")
    L.append(f"- Selection: {proto['selection_rule']['rank']}; eligible: {proto['selection_rule']['eligible']} "
             f"(min {proto['selection_rule']['min_training_positions']}).")
    L.append(f"- Verdict rules: {proto['verdict_rules']['scope']}. Floor: "
             f"{proto['verdict_rules']['min_held_out_entry_clusters']} entry clusters "
             f"({proto['verdict_rules']['entry_cluster']}). Unscorable window: "
             f"{proto['verdict_rules']['unscorable_window']}.")
    L.append("")
    L.append("| frozen file | sha256 |")
    L.append("|---|---|")
    for name, sha in sorted(proto["data"]["files"].items()):
        L.append(f"| `{proto['data']['source_dir']}/{name}` | `{sha}` |")
    L.append("")
    L.append("## Reproduce")
    L.append("")
    L.append("```bash")
    L.append("uv run --no-sync python backtest/candidates/adaptive_moving_average_1641/run_study.py freeze")
    L.append("uv run --no-sync python backtest/candidates/adaptive_moving_average_1641/run_study.py run "
             "--cache-db /tmp/ama_1641_run.db")
    L.append("uv run --no-sync python backtest/candidates/adaptive_moving_average_1641/run_study.py parity "
             "--cache-db /tmp/ama_1641_parity.db")
    L.append("uv run --no-sync python backtest/candidates/adaptive_moving_average_1641/run_study.py render")
    L.append("```")
    L.append("")
    L.append("Each run and parity call needs a cache path that does not exist yet. The ordinary M1 command for the "
             "seed on the cached path is `uv run --no-sync python backtest/eval_windows.py --registry futures "
             "--candidate-json backtest/candidates/adaptive_moving_average_1641/spec_seed.json --funding charge "
             "--warmup-bars 300` (it uses the M1 default windows and the live cache).")
    L.append("")
    for group, gd in res["groups"].items():
        v = gd["verdict"]
        L.append(f"## {group}: {v['verdict'].upper()}")
        L.append("")
        L.append(f"Reason: {v['reason']}.")
        L.append("")
        L.append("| selection | params | how | eligible variants |")
        L.append("|---|---|---|---|")
        for name, sel in gd["selections"].items():
            L.append(f"| {name} | {_p(sel['params'])} | {sel['how']} | {sel['eligible']} of "
                     f"{len(gd['training'][name])} |")
        L.append("")
        L.append("Held-out results (sums over BTC, ETH and SOL legs; M1 bar = median of the eight incumbents with the "
                 "same warmup):")
        L.append("")
        L.append("| group | window | arm | M1 verdict | Sharpe | DD-adj | bar Sharpe | bar DD-adj | net PnL USD | "
                 "positions | long/short | fees USD | funding USD | turnover |")
        L.append("|---|---|---|---|---|---|---|---|---|---|---|---|---|---|")
        for w in proto["held_out_windows"]:
            L.append(_arm_line(group, w, "candidate", gd["held_out"][w]))
            L.append(_arm_line(group, w, "candidate x2 slippage", gd["held_out_x2_slippage"][w]))
            for c, rows in gd["comparators"].items():
                L.append(_arm_line(group, w, f"{c} ({proto['directions'][c]})", rows[w]))
        L.append("")
        ck = v["checks"]
        L.append("| condition | value |")
        L.append("|---|---|")
        for k, val in ck["conditions"].items():
            L.append(f"| {k} | {_f(val)} |")
        L.append(f"| scorable windows | {', '.join(ck['scorable_windows']) or 'none'} |")
        for w, why in ck["unscorable_windows"].items():
            L.append(f"| unscorable {w} | {'; '.join(why)} |")
        L.append(f"| entry clusters / positions (long/short) | {ck['entry_clusters']} / {ck['positions']} "
                 f"({ck['long_positions']}/{ck['short_positions']}) |")
        L.append(f"| net PnL USD base / x2 slippage | {_f(ck['net_pnl_usd'])} / {_f(ck['net_pnl_usd_x2_slippage'])} |")
        L.append(f"| fees USD / funding USD | {_f(ck['fees_usd'])} / {_f(ck['funding_pnl_usd'])} |")
        L.append(f"| mean return % candidate / comparators | {_f(ck['mean_return_pct'])} / " + ", ".join(
            f"{c} {_f(r)}" for c, r in ck["comparator_mean_return_pct"].items()) + " |")
        L.append(f"| largest position share of positive PnL | {_f(ck['top_position_share'], 4)} |")
        L.append(f"| M1 passes | {ck['m1_passes']} of {len(proto['held_out_windows'])} |")
        L.append("")
        fills = [l["next_bar_fills"] for w in proto["held_out_windows"] for l in _legs(gd["held_out"][w])]
        warm = [l["warmup"] for w in proto["held_out_windows"] for l in _legs(gd["held_out"][w])]
        L.append(f"Next-bar fills: {sum(f['positions_checked'] for f in fills)} held-out candidate positions checked, "
                 f"{sum(f['mismatches'] for f in fills)} mismatches (each entry fills at the open of the bar after "
                 "its signal, plus the modeled slippage). Every scored frame starts at its window start after "
                 f"{warm[0]['bars'] if warm and warm[0] else '-'} warmup bars: "
                 + ", ".join(sorted({f['first_scored_bar'] for f in fills})) + ".")
        ev = gd["entry_evidence"]
        L.append("")
        L.append(f"Per-entry evidence: `{ev['path']}` ({ev['rows']} rows, sha256 `{ev['sha256']}`) lists every "
                 "held-out entry signal with its input window hash and indicator values; replaying each entry on "
                 f"exactly its required rows reproduced it with {ev['exact_span_replay_mismatches']} mismatches.")
        L.append("")
        st = gd["stability"]
        L.append("Parameter stability (one grid step from the selection; training rows, then held-out, exploratory "
                 "after selection):")
        L.append("")
        L.append("| params | train DD-adj | train Sharpe | train positions |")
        L.append("|---|---|---|---|")
        for r in st["train"]:
            L.append(f"| {_p(r['params'])} | {_f(r['mean_ddadj'])} | {_f(r['mean_sharpe'])} | {r['positions']} |")
        L.append("")
        L.append("| params | window | M1 verdict | Sharpe | DD-adj | net PnL USD | positions |")
        L.append("|---|---|---|---|---|---|---|")
        for key, rows in st["held_out"].items():
            for w, r in rows.items():
                L.append(f"| {_p(json.loads(key))} | {w} | {r['verdict']} | {_f(r['mean_sharpe'])} | "
                         f"{_f(r['mean_ddadj'])} | {_f(r['net_pnl_usd'])} | {r['positions']} |")
        L.append("")
        top = sorted(gd["training"][proto["candidate"]], key=lambda r: -(r["mean_ddadj"] or -1e9))[:5]
        L.append("Best five candidate training variants by DD-adj (every variant, eligible or not, is in "
                 "`results.json`):")
        L.append("")
        L.append("| params | DD-adj | Sharpe | positions |")
        L.append("|---|---|---|---|")
        for r in top:
            L.append(f"| {_p(r['params'])} | {_f(r['mean_ddadj'])} | {_f(r['mean_sharpe'])} | "
                     f"{sum(l['positions'] for l in _legs(r))} |")
        L.append("")
    L.append("## Funding coverage")
    L.append("")
    L.append("| group | window | arm | dataset | frame hours | priced | venue-absent | missing | unpriced held | "
             "rows sha256 |")
    L.append("|---|---|---|---|---|---|---|---|---|---|")
    for group, gd in res["groups"].items():
        for w in proto["held_out_windows"]:
            arms = [("candidate", gd["held_out"][w]), ("candidate x2", gd["held_out_x2_slippage"][w])] + [
                (c, rows[w]) for c, rows in gd["comparators"].items()]
            for label, row in arms:
                for ds, l in row["legs"].items():
                    if l is None:
                        L.append(f"| {group} | {w} | {label} | {ds} | no data | - | - | - | - | - |")
                        continue
                    f = l["funding"]
                    L.append(f"| {group} | {w} | {label} | {ds} | {f['expected_hours']} | {f['priced_hours']} | "
                             f"{f['venue_absent_hours']} | {f['missing_hours']} | {f['unpriced_held_hours']} | "
                             f"`{(f['sha256'] or '-')[:16]}` |")
    L.append("")
    L.append("## Decision parity")
    L.append("")
    if parity is None:
        L.append("Not run.")
    else:
        L.append(f"`parity_results.json` (source commit `{parity['source_commit']}`): clean = "
                 f"{_f(parity['clean'])}. Each run feeds the frozen BTC candles through `parity_diff.compute_parity_frame` "
                 "with the paper configuration (explicit `--mode=paper`, `closed_bar_decisions: true`, "
                 "`tiered_tp_pct`, one ATR stop, `direction: both`), comparing the full-series backtest decision with "
                 "the live check decision on a trailing 200-row frame (window 200) or the whole prefix (window 0), "
                 "solo and batched, in the flat, long and short position contexts the simulation produces. These "
                 "runs prove supplied-frame parity; the closed-bar cutoff is proved separately by the subprocess "
                 "checks in `shared_scripts/test_adaptive_moving_average_checks.py`.")
        L.append("")
        L.append("| config | params | batched | window | bars | mismatches | batch mismatches | long entries | "
                 "short entries | long closes | short closes | clean |")
        L.append("|---|---|---|---|---|---|---|---|---|---|---|---|")
        for r in parity["runs"]:
            L.append(f"| {r['config']} | {r['params_label']} | {_f(r['batched'])} | {r['window']} | "
                     f"{r['bars_compared']} | {r['mismatches']} | {r['batch_mismatches']} | {r['live_long_entries']} | "
                     f"{r['live_short_entries']} | {r['long_position_closes']} | {r['short_position_closes']} | "
                     f"{_f(r['clean'])} |")
    L.append("")
    L.append("## Differences and limits")
    L.append("")
    L.append(f"- Side populations: {proto['side_population_note']}.")
    L.append("- Every arm uses the same close owner, ATR stop, fees, slippage, funding mode, capital, windows and "
             "warmup. The incumbents keep the M1 default contract (long-only, signal exits), so the M1 bar compares a "
             "different exit policy.")
    for item in proto["limitations"]:
        L.append(f"- {item}")
    L.append(f"- {proto['window_notes']}")
    L.append("")
    with open(report_path, "w") as fh:
        fh.write("\n".join(L) + "\n")

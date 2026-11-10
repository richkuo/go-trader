#!/usr/bin/env python3

import json
import os

HERE = os.path.dirname(os.path.abspath(__file__))


def load(name: str):
    path = os.path.join(HERE, name)
    if not os.path.exists(path):
        return None
    with open(path) as fh:
        return json.load(fh)


def fmt(v, nd=2):
    if v is None:
        return "-"
    if isinstance(v, bool):
        return "yes" if v else "no"
    if isinstance(v, float):
        return f"{v:.{nd}f}"
    return str(v)


def proxy_table(proto: dict, res: dict, set_name: str) -> list:
    windows = [proto["proxy"]["selection_window"]] + proto["proxy"]["judged_windows"]
    held = [w for w in proto["proxy"]["judged_windows"] if w not in ("oos", "stitched")]
    arms = sorted(proto["arms"]) + sorted(proto["reference_arms"])
    lines = ["| arm | is M1 | oos M1 | " + " | ".join(f"{w} M1" for w in held)
             + " | stitched mean return % | stitched mean Sharpe | stitched worst DD % | stitched trades "
               "| stitched fee drag % | stitched funding drag % | stitched clusters |",
             "|---" * (11 + len(held) - 1) + "|"]
    for arm in arms:
        row = [arm]
        for w in windows:
            if w == "stitched":
                continue
            s = res["proxy"][set_name].get(w, {}).get(arm)
            if w in held or w in ("is", "oos"):
                row.append(s["m1_verdict"] if s else "not scored")
        st = res["proxy"][set_name]["stitched"].get(arm)
        if st:
            row += [fmt(st["mean_return_pct"]), fmt(st["mean_sharpe"]), fmt(st["worst_max_dd_pct"]),
                    fmt(st["trades"]), fmt(st["fee_drag_pct"]), fmt(st["funding_drag_pct"]), fmt(st["clusters"])]
        else:
            row += ["-"] * 7
        lines.append("| " + " | ".join(row) + " |")
    return lines


def window_table(summaries: dict, arms: list) -> list:
    lines = ["| arm | datasets | M1 | mean return % | mean Sharpe | mean ddadj | worst DD % | trades | positions (L/S) "
             "| clusters | fee drag % | funding drag % | funding incomplete legs |",
             "|---|---|---|---|---|---|---|---|---|---|---|---|---|"]
    for arm in arms:
        s = summaries.get(arm)
        if not s:
            continue
        lines.append(f"| {arm} | {s['datasets']} | {s['m1_verdict']} | {fmt(s['mean_return_pct'])} "
                     f"| {fmt(s['mean_sharpe'])} | {fmt(s['mean_ddadj'])} | {fmt(s['worst_max_dd_pct'])} "
                     f"| {s['trades']} | {s['positions']} ({s.get('long_positions', 0)}/{s.get('short_positions', 0)}) "
                     f"| {s['clusters']} | {fmt(s['fee_drag_pct'])} | {fmt(s['funding_drag_pct'])} "
                     f"| {s['funding_incomplete_legs']} |")
    return lines


def render() -> None:
    proto = load("protocol.json")
    cov = load("coverage_report.json")
    sel = load("selection.json")
    res = load("results.json")
    fid = load("fidelity.json")
    ver = load("verdict.json")
    par = load("cache_parity.json")
    timing = load("timing.json")
    if not (proto and cov and sel and res and fid and ver):
        raise SystemExit("render needs coverage_report.json, selection.json, results.json, fidelity.json and "
                         "verdict.json")
    arms = sorted(proto["arms"])
    L = ["# breakout_2014: trend-quality entry gate on the deployed Hyperliquid perps shape", ""]
    L.append(f"Issue {proto['issue']}. Suggest-only: {proto['suggest_only']}.")
    L.append("")
    L.append("## Verdict")
    L.append("")
    L.append(f"**{ver['outcome']}.**")
    L.append("")
    for s, a in sorted(ver["sets"].items()):
        L.append(f"- set `{s}`: primary arm `{a['arm']}`; adopted {fmt(a['adopted'])}; {a['reason']}.")
        if a.get("checks"):
            L.append("  Checks: " + ", ".join(f"{k} {fmt(v)}" for k, v in sorted(a["checks"].items())) + ".")
        if a.get("venue"):
            L.append(f"  Venue verdict: {a['venue']['outcome']} ({a['venue']['reason']}).")
        if a.get("excluded"):
            L.append("  Excluded datasets: " + "; ".join(f"{k}: {', '.join(v)}" for k, v in sorted(a["excluded"].items()))
                     + ".")
    L.append("")
    if ver.get("suggested_config"):
        L.append(f"Suggested config: `{ver['suggested_config']}` adds {', '.join('`' + g + '`' for g in ver['gated_ids'])}. "
                 "Apply it while flat; it is a suggestion only.")
    else:
        L.append(f"No config is suggested: {ver.get('reason')}.")
    L.append("")
    L.append("## Arms")
    L.append("")
    L.append("| arm | config | directional policy | entries | gate and short entries | gate window |")
    L.append("|---|---|---|---|---|---|")
    for a in arms:
        spec = proto["arms"][a]
        L.append(f"| {a} | `{spec['config']}` | {'on (research replay)' if spec['policy'] else 'off, as live'} "
                 f"| {spec['entries']} | {spec['short_entries']} | `{spec['gate_window']}` |")
    for a, path in sorted(proto["reference_arms"].items()):
        L.append(f"| {a} | `{path}` (read in place) | none | long, open signal as close | reference only | first-study "
                 "spec |")
    L.append("")
    L.append("Every config arm keeps `regime_atr_window: composite_medium`, so the `trailing_tp_ratchet_regime` ladder and "
             "the 2.5 ATR trail read that window; the period 21 window feeds the gate only.")
    L.append("")
    L.append("## Coverage")
    L.append("")
    L.append("| group | dataset | status | reason |")
    L.append("|---|---|---|---|")
    for r in cov["rows"]:
        L.append(f"| {r['group']} | {r['dataset']} | {r['status']} | {r['reason']} |")
    L.append("")
    held_ex = []
    for r in cov["rows"]:
        for w, item in sorted((r.get("windows") or {}).items()):
            if item.get("status") == "excluded" and r["status"] == "included":
                held_ex.append(f"{r['group']} {r['dataset']} {w}: {item['reason']}")
    if held_ex:
        L.append("Window-level exclusions on included datasets: " + "; ".join(held_ex) + ".")
        L.append("")
    L.append("## Selection on is")
    L.append("")
    for s, block in sorted(sel["sets"].items()):
        L.append(f"Set `{s}` ({len(block['datasets'])} datasets): shortlist {block['shortlist'] or 'empty'}, primary "
                 f"arm `{block['primary_arm']}`, policy-family arm `{block['policy_family_arm']}`.")
        L.append("")
        L.extend(window_table(block["arms"], arms))
        L.append("")
        L.append("| gate arm | " + " | ".join(sorted(next(iter(block["checks"].values())).keys())) + " |")
        L.append("|---" * (1 + len(next(iter(block["checks"].values())))) + "|")
        for a, c in sorted(block["checks"].items()):
            L.append(f"| {a} | " + " | ".join(fmt(v) for _, v in sorted(c.items())) + " |")
        L.append("")
    L.append("## Proxy group")
    L.append("")
    L.append(f"Candle source: {proto['proxy']['candle_source']}. Funding charged on every leg; warm-up "
             f"{proto['warmup_bars']} bars before every window.")
    L.append("")
    for s in sorted(proto["dataset_sets"]):
        L.append(f"### Set `{s}`")
        L.append("")
        L.extend(proxy_table(proto, res, s))
        L.append("")
        for w in ["oos"]:
            L.append(f"`{w}` window:")
            L.append("")
            L.extend(window_table(res["proxy"][s][w], arms + sorted(proto["reference_arms"])))
            L.append("")
    L.append("## Venue group")
    L.append("")
    L.append(f"Candle source: {proto['venue']['candle_source']}. Window `{proto['venue']['verdict_window']}`; "
             f"warm-up {proto['venue']['warmup_bars']} bars; manifest execution specification (spread, slippage, lot "
             "rounding, venue minimum).")
    L.append("")
    w = proto["venue"]["verdict_window"]
    for s in sorted(proto["dataset_sets"]):
        for label in sorted(proto["venue"]["cost_multipliers"]):
            L.append(f"### Set `{s}`, cost {label} (x{proto['venue']['cost_multipliers'][label]:g})")
            L.append("")
            L.extend(window_table(res["venue"][s][w][label], arms))
            L.append("")
    L.append("### Bridge (Binance US candles on the venue windows)")
    L.append("")
    L.append("Same windows, warm-up and execution specification as the venue group; only the candle source differs.")
    L.append("")
    for s in sorted(proto["dataset_sets"]):
        L.append(f"Set `{s}`, cost base:")
        L.append("")
        L.extend(window_table(res["bridge"][s][w]["base"], arms))
        L.append("")
    L.append("### Common dates (30m tail dates scored at 30m and 1h)")
    L.append("")
    for group in sorted(res["common_window"]):
        for key, by in sorted(res["common_window"][group].items()):
            L.append(f"{group} `{key}`, cost base:")
            L.append("")
            L.extend(window_table(by["base"], arms))
            L.append("")
    L.append("## Live-label fidelity")
    L.append("")
    L.append(f"`validate_frames` on frames built from the frozen files, lookback {proto['fidelity']['lookback']} bars, "
             f"pass at agreement >= {proto['fidelity']['agreement_threshold']} on at least "
             f"{proto['fidelity']['min_agreement_bars']} bars. Engine check: {fid['engine_check']}.")
    L.append("")
    L.append("| group | dataset | window | regime window | n | agreement | passes | shortest bounded slice | engine labels "
             "sampled / equal |")
    L.append("|---|---|---|---|---|---|---|---|---|")
    for r in fid["rows"]:
        u = r["unit"]
        L.append(f"| {u['group']} | {u['coin']} {u['interval']} | {u['window']} | `{u['regime_window']}` | {r['n']} "
                 f"| {r['agreement']:.4f} | {fmt(r['passes'])} | {r['shortest_bounded_slice']} "
                 f"| {r['engine_check']['sampled']} / {r['engine_check']['equal']} |")
    L.append("")
    L.append("## Reference arms and the first-study rerun")
    L.append("")
    L.append("The first study's committed legs were scored without funding. Rerun with funding off on the same start and "
             "the committed last bar:")
    L.append("")
    L.append("| arm and dataset | committed Sharpe / return % / DD % / trades | rerun | difference |")
    L.append("|---|---|---|---|")
    for k, r in sorted(res["audit_rerun"].items()):
        c, n, d = r["committed"], r["rerun"] or {}, r["difference"] or {}
        L.append(f"| {k} | {fmt(c['sharpe'])} / {fmt(c['return_pct'])} / {fmt(c['max_dd_pct'])} / {c['trades']} "
                 f"| {fmt(n.get('sharpe'))} / {fmt(n.get('return_pct'))} / {fmt(n.get('max_dd_pct'))} / "
                 f"{n.get('trades')} | {fmt(d.get('sharpe'))} / {fmt(d.get('return_pct'))} / "
                 f"{fmt(d.get('max_dd_pct'))} / {d.get('trades')} |")
    L.append("")
    eq = res["policy_gate_p350_equality"]
    L.append(f"`policy_gate_p350` against `gate_p350`: {eq['legs_equal']} of {eq['legs_compared']} legs equal"
             + (f"; differences: {', '.join(eq['differences'])}" if eq["differences"] else "") + ".")
    L.append("")
    if par:
        L.append(f"Label cache parity on {par['dataset']} `{par['window']}`: identical {fmt(par['identical'])} "
                 f"({', '.join(f'{a}: {fmt(r['identical'])}' for a, r in sorted(par['rows'].items()))}).")
        L.append("")
    L.append("## Disclosures")
    L.append("")
    for line in proto_disclosures(proto, res, timing):
        L.append(f"- {line}")
    L.append("")
    L.append("## Reproduction")
    L.append("")
    L.append("Every command is in `README.md`. Results: `results.json` (source commit "
             f"`{res['source_commit']}`, protocol sha256 `{res['protocol_sha256']}`), `selection.json`, "
             "`fidelity.json`, `verdict.json`, `coverage_report.json`.")
    L.append("")
    with open(os.path.join(HERE, "REPORT.md"), "w") as fh:
        fh.write("\n".join(L))
    print("wrote REPORT.md")


def proto_disclosures(proto: dict, res: dict, timing) -> list:
    out = [
        "The deployed `regime_directional_policy` is off live: the scheduler honours a policy entry only for a certified "
        "(asset, timeframe, classifier) cell and none is certified, so all ten strategies trade the base `direction: "
        "long`. The primary arms are long-only; the configured-policy arms are a research replay.",
        "`use_defaults: true` is not read by the Python evaluator; with no `tp_tiers`, the system ladders of "
        "`trailing_tp_ratchet_regime` apply, chosen by the entry label of `composite_medium`.",
        "`atr_method: wilder` does not change these legs: `breakout` writes its own `atr` column, which sets the entry "
        "ATR in live and in the backtest.",
        "Binance US candles are a proxy for Hyperliquid prices; only fees and funding are Hyperliquid on the proxy group.",
        "The venue window lies inside the proxy `oos` period, so the two groups are not independent samples.",
        "The first study already read `oos` through June 2026 for the period 21 gate on BTC, ETH and SOL 1h.",
        "Closed-bar decisions are opt-in live (`closed_bar_decisions`), and the deployed strategies do not set them, "
        "while the backtest decides on closed bars.",
        "No `apply-regime-gate` preset exists for period 350; the `comp_up_clean_p21` preset edits a strategy in place "
        "and makes no copy.",
        "Venue labels at the start of the venue window come from slices shorter than the live depth; only the ADX seed "
        "differs, and the fidelity rows measure it.",
        f"Every leg charged funding; legs that fetched funding while scoring: {res['funding_fetched_legs']}.",
    ]
    if timing:
        out.append(f"Run-time projection measured before the full run: {timing['seconds_per_window_bar']} s per labelled "
                   f"window-bar on {timing['dataset']} `{timing['window']}`.")
    return out


if __name__ == "__main__":
    render()

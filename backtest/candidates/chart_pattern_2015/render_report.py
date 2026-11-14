#!/usr/bin/env python3

import json
import os
import sys

WINDOW_ORDER = ["is", "oos", "oos_982", "2023", "2024", "2025H1", "2025H1t", "stitched", "tail"]


def _load(root: str, name: str):
    path = os.path.join(root, name)
    if not os.path.exists(path):
        return None
    with open(path) as fh:
        return json.load(fh)


def _f(v, prec=2, sign=False):
    if v is None:
        return "-"
    if isinstance(v, bool):
        return "yes" if v else "no"
    if isinstance(v, int):
        return str(v)
    fmt = f"{{:{'+' if sign else ''}.{prec}f}}"
    return fmt.format(v)


def _wkey(w):
    return (WINDOW_ORDER.index(w) if w in WINDOW_ORDER else 99, w)


def _table(head, rows):
    out = ["| " + " | ".join(head) + " |", "|" + "|".join("---" for _ in head) + "|"]
    for r in rows:
        out.append("| " + " | ".join(str(c) for c in r) + " |")
    return out


def _verdict_section(verdict):
    lines = ["## Verdict", ""]
    if verdict is None:
        return lines + ["verdict.json is not written yet.", ""]
    rows = []
    for key in sorted(verdict["members"]):
        m = verdict["members"][key]
        rows.append([m["contract"], m["group"], f"f{m['factor']} ({m['slot']})", m["outcome"],
                     _f(m["paired"]["mean"] * 100 if m["paired"]["mean"] is not None else None, 4, True),
                     _f(m["paired"]["p"], 4), _f(m["bh"]["q"], 4), m["positions"], m["paired"]["blocks"],
                     f"{len(m['usable_members'])}/{len(m['frozen_members'])}", _f(m["venue_supports"]),
                     "; ".join(m["reasons"]) or "-"])
    lines += _table(["contract", "group", "factor", "outcome", "mean daily paired diff (pct)", "p", "BH q",
                     "positions", "blocks", "usable members", "venue supports", "reasons"], rows)
    lines += ["", f"Family size {verdict['family_size']} (Benjamini-Hochberg, alpha 0.05). "
              f"Finite-frame replay passed: {_f(verdict['replay']['finite_frame'])}. "
              f"Full-frame causality passed: {_f(verdict['replay']['full_frame'])}.", "",
              f"Deployment verdict: {verdict['deployment']['outcome']}. {verdict['deployment']['reason']}", "",
              f"Audit reference: {verdict['audit']['outcome']}. {verdict['audit']['reason']}", ""]
    lines.append("Suggested parameter lines: " + (
        ", ".join(f"`{k}`: `{json.dumps(v['open_strategy'], sort_keys=True)}`"
                  for k, v in sorted(verdict["suggested_lines"].items())) if verdict["suggested_lines"]
        else "none (no opt-in result)."))
    lines.append("")
    return lines


def _audit_section(proto, verdict):
    lines = ["## Audit reference (spot registry, long, open-as-close, full frame)", ""]
    if verdict is None:
        return lines + ["Not scored yet.", ""]
    a = verdict["audit"]
    rows = []
    for f in sorted(a["comparison"], key=lambda x: int(x[1:])):
        for w in ("is", "oos", "2023", "2024", "2025H1"):
            c = a["comparison"][f][w]
            cur = a["memos"]["current"]["windows"].get(w, {}).get(f, {})
            rows.append([f, w, c["982"] or "-", c["1282"] or "not documented", c["current"] or "-",
                         _f(cur.get("mean_sharpe")), _f(cur.get("mean_bar_sharpe")),
                         c["current_funding_off"] or "-", c["legacy_cost"] or "-"])
    lines += _table(["factor", "window", "982", "1282", "current", "current mean Sharpe", "current bar Sharpe",
                     "diag: funding off", "diag: 982 costs"], rows)
    lines += ["", "Columns: `982` and `1282` are the documented verdicts. `current` is the current-default rerun "
              "(Hyperliquid taker fee 0.045 percent, 0.05 percent slippage, perps accounting, charged funding, oos "
              "to 2026-11-10). `funding_incomplete` means the window has hours before the first Hyperliquid "
              "funding rate (2023-05-12), so the current harness refuses a verdict. The two diagnostic memos are "
              "labeled references: `funding off` keeps current fees, `982 costs` uses the issue 982 cost model "
              "(0.1 percent commission, 0.05 percent slippage, no funding).", ""]
    lines += ["### What changed against issue 982", ""]
    lines += _explain_changes(proto, a)
    lines += ["### Current-default rerun per window", ""]
    rows = []
    for w in sorted(a["memos"]["current"]["windows"], key=_wkey):
        for f, r in sorted(a["memos"]["current"]["windows"][w].items(), key=lambda kv: int(kv[0][1:])):
            rows.append([w, f, r.get("verdict"), _f(r.get("mean_sharpe")), _f(r.get("mean_bar_sharpe")),
                         _f(r.get("mean_ddadj")), _f(r.get("mean_bar_ddadj")), _f(r.get("mean_return_pct")),
                         _f(r.get("worst_max_dd_pct")), r.get("positions"), r.get("close_records")])
    lines += _table(["window", "factor", "verdict", "mean Sharpe", "bar Sharpe", "mean DDadj", "bar DDadj",
                     "mean return %", "worst DD %", "positions", "close records"], rows)
    p = a["paired_f4_vs_f0"]
    lines += ["", f"Paired factor 4 minus factor 0, daily equity, oos + 2024 + 2025H1t: mean daily difference "
              f"{_f((p['mean'] or 0) * 100, 4, True)} percent over {p['blocks']} blocks, one-sided bootstrap p "
              f"{_f(p['p'], 4)}. This is a historical reference and enters no correction family.", ""]
    lines += ["Incumbent memo instances: " + ", ".join(
        f"`{k}` ({v['memo_identity']})" for k, v in sorted(a["memos"].items())) + ".", ""]
    return lines


def _explain_changes(proto, a):
    hist = proto["historical"]["982"]
    out = []
    cur = a["memos"]["current"]["windows"]
    leg = a["memos"]["legacy_cost"]["windows"]
    off = a["memos"]["current_funding_off"]["windows"]
    for f in sorted(a["comparison"], key=lambda x: int(x[1:])):
        for w in ("is", "oos", "2023", "2024", "2025H1"):
            c = a["comparison"][f][w]
            if c["982"] is None or c["current"] is None or c["982"].lower() == c["current"].lower():
                continue
            parts = []
            h_sh = hist["mean_sharpe"].get(f[1:], {}).get(w)
            l = leg.get(w, {}).get(f, {})
            o = off.get(w, {}).get(f, {})
            n = cur.get(w, {}).get(f, {})
            parts.append(f"982 {c['982']} (Sharpe {_f(h_sh)} vs bar {_f(hist['bar_sharpe'].get(w))})")
            if w == "oos":
                l982 = leg.get("oos_982", {}).get(f, {})
                parts.append(f"982 costs on the 982 dates (oos_982) {l982.get('verdict', '-')} "
                             f"(Sharpe {_f(l982.get('mean_sharpe'))} vs bar {_f(l982.get('mean_bar_sharpe'))}): "
                             "the engine change on the same dates")
                parts.append(f"982 costs on the full oos {l.get('verdict', '-')} (Sharpe {_f(l.get('mean_sharpe'))} "
                             f"vs bar {_f(l.get('mean_bar_sharpe'))}): adds the data-range change")
            else:
                parts.append(f"982 costs {l.get('verdict', '-')} (Sharpe {_f(l.get('mean_sharpe'))} vs bar "
                             f"{_f(l.get('mean_bar_sharpe'))}): the engine and data change at the 982 cost model")
            parts.append(f"current fees without funding {o.get('verdict', '-')} (Sharpe {_f(o.get('mean_sharpe'))} vs "
                         f"bar {_f(o.get('mean_bar_sharpe'))}): adds the fee change")
            parts.append(f"current {n.get('verdict', '-')} (Sharpe {_f(n.get('mean_sharpe'))} vs bar "
                         f"{_f(n.get('mean_bar_sharpe'))}): adds charged funding")
            out.append(f"- {f} {w}: " + "; ".join(parts) + ".")
    if not out:
        out.append("- No current verdict differs from issue 982.")
    out.append("")
    out.append("A step that keeps the verdict and moves the Sharpe means attributes that movement to the named "
               "change; a step that flips the verdict names the change that flips it. The engine change is the "
               "residual between the issue 982 numbers and the 982-cost rerun on the same dates, so it also holds "
               "any cache difference between the 2026-07-01 shared cache and the frozen candles.")
    out.append("")
    return out


def _replay_section(val):
    lines = ["## Replay and causality checks", ""]
    if val is None:
        return lines + ["validation.json is not written yet.", ""]
    rp = val["replay"]
    rows = []
    for name in ("decomposition", "signal_check_path", "future_invariance", "htf_visibility", "next_bar_fill",
                 "full_frame_causality"):
        r = rp.get(name) or {}
        mm = r.get("mismatch_count", r.get("mismatches"))
        if isinstance(mm, list):
            mm = len(mm)
        rows.append([name, r.get("compared"), mm, _f(r.get("passed"))])
    lines += _table(["check", "compared", "mismatches", "passed"], rows)
    lines += ["", f"Signal-check path: {rp['signal_check_path'].get('path')}.", ""]
    ff = rp.get("full_frame_causality") or {}
    lines += ["### Full-frame path causality (historical audit path)", ""]
    lines += _table(["dataset", "window", "full-frame signals", "sampled", "reproduced on cut frame", "mismatches"],
                    [[r["dataset"], r["window"], r["full_frame_signals"], r["sampled"], r["reproduced_on_cut_frame"],
                      r["mismatches"]] for r in ff.get("rows", [])])
    lines += ["", ff.get("rule", ""), ""]
    fv = rp.get("finite_vs_full_frame") or {}
    lines += ["### Full frame against the live-shaped finite frame (diagnostic)", ""]
    lines += _table(["dataset", "window", "factor", "full-frame signals", "finite-frame signals",
                     "same bar and side", "full-frame longs", "finite-frame longs"],
                    [[r["dataset"], r["window"], f"f{r['factor']}", r["full_frame_signals"],
                      r["finite_frame_signals"], r["same_bar_same_side"], r["full_long"], r["finite_long"]]
                     for r in fv.get("rows", [])])
    lines += ["", f"Network during checks: {json.dumps(val.get('network_probe'), sort_keys=True)}; refused attempts "
              f"{val.get('network_calls')} (the probe).", ""]
    return lines


def _coverage_section(cov):
    lines = ["## Coverage and exclusions", ""]
    if cov is None:
        return lines + ["coverage_report.json is not written yet.", ""]
    rows = []
    for r in cov["rows"]:
        for w, v in sorted(r["windows"].items(), key=lambda kv: _wkey(kv[0])):
            if v["status"] == "excluded" or r["section"] == "perps":
                rows.append([r["section"], r["group"], f"{r['coin']} {r['interval']}", w, v["status"],
                             v.get("reason", "")])
    lines += _table(["section", "group", "dataset", "window", "status", "reason"], rows)
    lines += ["", "Audit rows are all included (the audit path loads no warm-up); their funding-incomplete windows "
              "are reported in the audit table.", ""]
    return lines


def _selection_section(sel):
    lines = ["## In-sample selection (is only)", ""]
    if sel is None:
        return lines + ["selection.json is not written yet.", ""]
    rows = []
    for k in sorted(sel["selection"]):
        s = sel["selection"][k]
        for f in sorted(s["rows"], key=lambda x: int(x[1:])):
            r = s["rows"][f]
            rows.append([s["contract"], s["group"], f, _f(r.get("mean_return_pct")), _f(r.get("mean_ddadj")),
                         _f(r.get("mean_sharpe")), r.get("positions"),
                         _f(s["improves_on_f0"].get(f)) if f != "f0" else "-",
                         "selected" if s["selected_additional"] == f else ("shortlist" if f in s["shortlist"] else "")])
    lines += _table(["contract", "group", "factor", "mean return %", "mean DDadj", "mean Sharpe", "positions",
                     "beats f0 on both", "pick"], rows)
    lines += ["", f"Rule: {sel['rule']}", ""]
    return lines


def _results_section(verdict):
    lines = ["## Paired results per contract and group", ""]
    if verdict is None:
        return lines + ["Not scored yet.", ""]
    for key in sorted(verdict["members"]):
        m = verdict["members"][key]
        lines += [f"### {m['contract']} / {m['group']} / f{m['factor']} against f0", ""]
        rows = []
        for w in sorted(m["windows"], key=_wkey):
            pv = m["windows"][w]
            if not pv.get("datasets"):
                rows.append([w, pv.get("verdict"), 0] + ["-"] * 12)
                continue
            g, b = pv["gated"], pv["baseline"]
            rows.append([w, pv["verdict"], pv["datasets"], _f(g["mean_return_pct"]), _f(b["mean_return_pct"]),
                         _f(g["mean_sharpe"]), _f(b["mean_sharpe"]), _f(g["worst_max_dd_pct"]),
                         _f(b["worst_max_dd_pct"]), g["positions"], b["positions"], g["close_records"],
                         b["close_records"], _f(g["net_pnl_usd"]), _f(b["net_pnl_usd"])])
        for vg, pv in sorted(m["venue"].items()):
            if not pv.get("datasets"):
                rows.append([vg, pv.get("verdict"), 0] + ["-"] * 12)
                continue
            g, b = pv["gated"], pv["baseline"]
            rows.append([vg, pv["verdict"], pv["datasets"], _f(g["mean_return_pct"]), _f(b["mean_return_pct"]),
                         _f(g["mean_sharpe"]), _f(b["mean_sharpe"]), _f(g["worst_max_dd_pct"]),
                         _f(b["worst_max_dd_pct"]), g["positions"], b["positions"], g["close_records"],
                         b["close_records"], _f(g["net_pnl_usd"]), _f(b["net_pnl_usd"])])
        lines += _table(["window", "gate vs f0", "datasets", "return % gated", "return % f0", "Sharpe gated",
                         "Sharpe f0", "worst DD % gated", "worst DD % f0", "positions gated", "positions f0",
                         "close records gated", "close records f0", "net USD gated", "net USD f0"], rows)
        lines += ["", f"Statistic over oos, 2024 and 2025H1t: mean daily paired difference "
                  f"{_f((m['paired']['mean'] or 0) * 100, 4, True)} percent, {m['paired']['blocks']} blocks, "
                  f"p {_f(m['paired']['p'], 4)}, BH q {_f(m['bh']['q'], 4)}; gated net PnL {_f(m['net_pnl_usd'])} "
                  f"USD over {m['positions']} positions. Outcome: **{m['outcome']}**.", ""]
    lines += ["`gate vs f0` is PASS when the gated arm's mean Sharpe and mean drawdown-adjusted return both exceed "
              "factor 0 over the datasets both arms scored. `is` is the selection window; `stitched` is one "
              "continuous run over is and oos. venue rows use Hyperliquid candles; bridge rows use proxy candles on "
              "the venue dates.", ""]
    return lines


def _resolution_section(res):
    lines = ["## Resolved research configuration", ""]
    if res is None:
        return lines + ["arms/resolution.json is not written yet.", ""]
    rows = []
    for c, r in sorted(res["contracts"].items()):
        x = r["resolved"]
        rows.append([c, r["kind"], x["direction"], ", ".join(z["name"] for z in x.get("close") or []) or "open-as-close",
                     x.get("atr_method"), _f(x.get("htf_filter")),
                     json.dumps(x.get("trailing_stop_atr_mult_regime"), sort_keys=True)
                     if x.get("trailing_stop_atr_mult_regime") else "none",
                     (x.get("regime") or {}).get("classifier", {}).get("atr", "-") if x.get("regime") else "-"])
    lines += _table(["contract", "kind", "direction", "close", "ATR method", "htf_filter", "trailing stop",
                     "regime classifier"], rows)
    rb = res["contracts"].get("ratchet_long", {}).get("resolved", {})
    if rb.get("resolved_per_label"):
        lines += ["", "Ratchet tiers and trailing multiple per regime label (use_defaults):", ""]
        trows = []
        for label, v in sorted(rb["resolved_per_label"].items()):
            trows.append([label, "; ".join(f"{t['atr_multiple']} ATR close {t['close_fraction']} trail "
                                           f"{t['trailing_mult_after']}" for t in v["tiers"]),
                          v["trailing_stop_atr_multiple"]])
        lines += _table(["label", "tiers", "trailing stop ATR multiple"], trows)
        lines += ["", "Carried by the study: " + "; ".join(f"{k}: {v}" for k, v in sorted(rb["carried_by_study"].items()))
                  + ".", "", "Not modeled: " + ", ".join(rb["unsupported_or_unmodeled"]) + ".", ""]
    for c, r in sorted(res["refused"].items()):
        lines += [f"Refused contract `{c}`: {r['protocol_reason']} (harness: {r['error'][:160]}...).", ""]
    return lines


def render(root: str) -> str:
    proto = _load(root, "protocol.json")
    verdict = _load(root, "verdict.json")
    lines = ["# chart_pattern_2015 report", "",
             "Offline, suggest-only study for issue 2015. Every number below regenerates from "
             "`run_study.py` on the hash-checked inputs in `study_manifest.json`; this file is written by "
             "`run_study.py render` and checked by `run_study.py render --check`.", ""]
    lines += [f"Question: {proto['question']}", ""]
    lines += _verdict_section(verdict)
    lines += _audit_section(proto, verdict)
    lines += _replay_section(_load(root, "validation.json"))
    lines += _coverage_section(_load(root, "coverage_report.json"))
    lines += _selection_section(_load(root, "selection.json"))
    lines += _results_section(verdict)
    lines += _resolution_section(_load(os.path.join(root, "arms"), "resolution.json"))
    lines += ["## Disclosures", ""] + [f"- {d}" for d in proto["disclosures"]] + [
        f"- Prior exposure: {proto['prior_exposure']}", ""]
    lock = _load(root, "protocol_lock.json")
    if lock:
        lines += [f"Protocol lock: `{lock['protocol_sha256']}` at {lock['locked_at_utc']} "
                  f"(source commit `{lock['source_commit']}`).", ""]
    results = _load(root, "results.json")
    if results:
        lines += [f"Results: {len(results['legs'])} legs, source commit `{results['source_commit']}`. Refused network "
                  f"attempts in the main process: {results['network_calls']} (the deliberate probe of a socket, a name "
                  "lookup and an unfrozen symbol). Every scoring worker runs with the same block on its own copy of the "
                  "throwaway cache.", ""]
    return "\n".join(lines).rstrip() + "\n"


if __name__ == "__main__":
    sys.stdout.write(render(os.path.dirname(os.path.abspath(__file__))))

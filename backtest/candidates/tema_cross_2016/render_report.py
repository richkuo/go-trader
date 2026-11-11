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


def pct(v, nd=3):
    return "-" if v is None else f"{v:+.{nd}f}"


def arm_row(arm: str, fam: str, s: dict) -> str:
    return (f"| {arm} | {fam} | {s['datasets']} | {s['trades']} | {fmt(s.get('trades_per_year'), 1)} "
            f"| {pct(s.get('gross_per_trade_pct'))} | {pct(s.get('net_per_trade_pct'))} "
            f"| {pct(s.get('stress_per_trade_pct'))} | {pct(s.get('cost_drag_per_trade_pct'))} "
            f"| {fmt(s.get('fee_drag_pct'))} | {fmt(s.get('funding_drag_pct'))} | {fmt(s.get('worst_max_dd_pct'))} "
            f"| {fmt(s.get('mean_ddadj'), 3)} | {fmt(s.get('mean_return_pct'))} "
            f"| {s.get('long_positions', 0)}/{s.get('short_positions', 0)} | {s.get('clusters', 0)} |")


ARM_HEADER = ["| arm | family | datasets | trades | trades/yr | gross %/trade | net %/trade | stress %/trade "
              "| cost drag %/trade | fee drag %/leg | funding drag %/leg | worst DD % | mean ddadj | mean return % "
              "| positions L/S | clusters |",
              "|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|"]


def render() -> None:
    proto = load("protocol.json")
    cov = load("coverage_report.json")
    sel = load("selection.json")
    noise = load("noise_gate.json")
    res = load("results.json")
    fid = load("fidelity.json")
    ver = load("verdict.json")
    smoke = load("smoke.json")
    parity = load("htf_parity.json")
    cpar = load("cache_parity.json")
    timing = load("timing.json")
    runlog = load("run_log.json")
    if not (proto and cov and sel and noise and res and fid and ver):
        raise SystemExit("render needs coverage_report.json, selection.json, noise_gate.json, results.json, "
                         "fidelity.json and verdict.json")
    L = []
    L += ["# tema_cross_2016 report", "",
          "Issue 2016: does an entry filter keep the `tema_cross` edge after fees, on the spot long frame and on the "
          "two Hyperliquid perps shapes that run today? Suggest-only: no command writes a live or paper config, a "
          "registry default or `edge_status`, and none sends an order. Every number below renders from the committed "
          "JSON files in this folder.", ""]
    L += ["## Verdict per frame", "",
          "| frame | verdict | arm | net %/trade (base) | net %/trade (stress) | trades | clusters | p_decision | BH | "
          "venue | reason |", "|---|---|---|---|---|---|---|---|---|---|---|"]
    for frame in ("spot", "paper", "live"):
        fo = ver["frames"][frame]
        arm = fo.get("arm")
        item = fo["arms"].get(arm) if arm else None
        if item is None and fo["arms"]:
            arm_best = min(fo["arms"], key=lambda a: (fo["arms"][a]["p_decision"], a))
            item = fo["arms"][arm_best]
            arm_label = f"best judged: {arm_best}"
        else:
            arm_label = arm or "-"
        if item:
            L.append(f"| {frame} | **{fo['verdict']}** | {arm_label} | {pct(item['net_per_trade_pct'])} "
                     f"| {pct(item['stress_per_trade_pct'])} | {item['trades']} | {item['clusters']} "
                     f"| {fmt(item['p_decision'], 4)} | {fmt(item.get('bh_pass'))} "
                     f"| {(item.get('venue') or {}).get('outcome', '-')} | {fo.get('reason', '')} |")
        else:
            L.append(f"| {frame} | **{fo['verdict']}** | - | - | - | - | - | - | - | - | {fo.get('reason', '')} |")
    L.append("")
    L.append(f"Frames agree: {fmt(ver['frames_agree'])}. {ver['suggestion_basis']}.")
    if ver["suggested_configs"]:
        L.append(f"Suggested configs: {', '.join('`' + s + '`' for s in ver['suggested_configs'])}.")
    else:
        L.append("No suggested config is written.")
    L.append("")
    L += ["## What `direction: both` does for `tema_cross`", "",
          "`tema_cross` is registered with `short_entries=False`, but that flag is catalog metadata only: no scheduler "
          "file, no `load_strategy_config` path and no backtester path reads it. The strategy emits `+1` on a bullish "
          "fast/mid cross while the mid EMA is above the long EMA, and `-1` when its own position state leaves long "
          "(a bearish cross). Live maps `-1` to a short open when the book is flat and `direction` allows shorts "
          "(`both`); the ratchet close owns every exit, so there is no flip. The higher-timeframe filter passes a `-1` "
          "only when the 4h trend is `-1` or `0`. The backtester does the same: `-1` is zeroed only under "
          "`direction: long`. So the live entry opens real shorts on the bearish cross that follows a bullish cross "
          "whenever the book is flat at that bar. The tables report long and short positions separately (L/S).", ""]
    L += ["## Noise gate (gross edge of each baseline, before any filter verdict)", "",
          f"Windows: {', '.join(noise['windows'])}; field: {noise['field']}.", "",
          "| baseline | n | mean gross %/trade | p (sign flip) | p (cluster sign flip) | clusters | verdict |",
          "|---|---|---|---|---|---|---|"]
    for key, st in sorted(noise["baselines"].items()):
        L.append(f"| {key} | {st['n']} | {pct(st['summary']['mean'], 4)} | {fmt(st['permutation']['p_value'], 4)} "
                 f"| {fmt(st['cluster_permutation']['p_value'], 4)} | {st['cluster_permutation'].get('clusters')} "
                 f"| {st['verdict']} |")
    L.append("")
    L += ["## Selection window (`is`, written once to `selection.json` before any judged window)", ""]
    for frame in ("spot", "paper", "live"):
        s = sel["frames"][frame]
        fr = proto["frames"][frame]
        L += [f"### {frame}", "", fr["description"] + ".", ""] + ARM_HEADER
        for arm in sorted(s["arms"], key=lambda a: (fr["arms"][a]["family"] != "control", fr["arms"][a]["family"], a)):
            L.append(arm_row(arm, fr["arms"][arm]["family"], s["arms"][arm]))
        L.append("")
        L += ["| arm | control | control mean ddadj | failed checks | shortlisted |", "|---|---|---|---|---|"]
        for arm, c in sorted(s["checks"].items()):
            failed = [k for k, v in c["checks"].items() if not v]
            L.append(f"| {arm} | {c['control']} | {fmt(c['control_mean_ddadj'], 3)} | {', '.join(failed) or 'none'} "
                     f"| {fmt(arm in s['shortlist'])} |")
        L.append("")
        L.append(f"Picks: {', '.join(f'{k}: {v}' for k, v in sorted(s['picks'].items()))}. Combination: "
                 f"{(s['combo'] or {}).get('members') or 'not built (fewer than two picks)'}. "
                 f"Judged arms: {', '.join(s['judged_arms']) or 'none'}.")
        L.append("")
    L += ["## Judged windows", "",
          "Pooled windows for the statistics: " + ", ".join(proto["proxy"]["pooled_windows"])
          + ". `stitched` is one continuous run from a flat book and is reported only.", ""]
    for frame in ("spot", "paper", "live"):
        fr = proto["frames"][frame]
        L += [f"### {frame}", ""]
        for w in proto["proxy"]["judged_windows"]:
            L += [f"`{w}`", ""] + ARM_HEADER
            for arm, s in sorted(res["proxy"][frame][w].items()):
                L.append(arm_row(arm, fr["arms"][arm]["family"], s))
            L.append("")
    L += ["## Statistics and verdict checks per judged arm", "",
          f"BH family size {ver['family_size']} (12 in-family arms per frame, three frames); p_decision is the larger "
          "of the plain and the cluster sign-flip p-values.", "",
          "| frame | arm | noise baseline verdict | trades | clusters | gross %/trade | net %/trade | stress %/trade "
          "| p plain | p cluster | BH pass | venue | failed checks |",
          "|---|---|---|---|---|---|---|---|---|---|---|---|---|"]
    for frame in ("spot", "paper", "live"):
        for arm, it in sorted(ver["frames"][frame]["arms"].items()):
            failed = [k for k, v in it["checks"].items() if not v]
            L.append(f"| {frame} | {arm} | {it['noise_verdict']} | {it['trades']} | {it['clusters']} "
                     f"| {pct(it['gross_per_trade_pct'])} | {pct(it['net_per_trade_pct'])} "
                     f"| {pct(it['stress_per_trade_pct'])} | {fmt(it['p_plain'], 4)} | {fmt(it['p_cluster'], 4)} "
                     f"| {fmt(it.get('bh_pass'))} | {(it.get('venue') or {}).get('outcome', '-')} "
                     f"| {', '.join(failed) or 'none'} |")
    L.append("")
    L += ["## Venue and bridge windows (perps frames)", ""]
    for frame in ("paper", "live"):
        L += [f"### {frame}", ""]
        for group in ("venue", "bridge"):
            for key, by_arm in sorted(res["manifest"][frame][group].items()):
                if not by_arm:
                    continue
                L += [f"{group} `{key}`", "",
                      "| arm | datasets | trades | net %/trade | mean Sharpe | mean ddadj | worst DD % | clusters |",
                      "|---|---|---|---|---|---|---|---|"]
                for arm, s in sorted(by_arm.items()):
                    L.append(f"| {arm} | {s['datasets']} | {s['trades']} | {pct(s.get('net_per_trade_pct'))} "
                             f"| {fmt(s.get('mean_sharpe'))} | {fmt(s.get('mean_ddadj'), 3)} "
                             f"| {fmt(s.get('worst_max_dd_pct'))} | {s.get('clusters', 0)} |")
                L.append("")
        for arm, it in sorted(ver["frames"][frame]["arms"].items()):
            v = it.get("venue") or {}
            L.append(f"- venue verdict for `{arm}` against `{v.get('control')}`: **{v.get('outcome')}** "
                     f"({v.get('reason')})")
        L.append("")
    L += ["## Exclusions", ""]
    for frame in ("spot", "paper", "live"):
        ex = ver["frames"][frame]["excluded"]
        if not ex:
            L.append(f"- {frame}: none")
        for ds, why in sorted(ex.items()):
            L.append(f"- {frame} {ds}: {'; '.join(why)}")
    L.append("")
    L += ["## Parity, smoke and fidelity", ""]
    if parity:
        s = parity.get("signals") or {}
        L.append(f"- Frozen higher-timeframe input against the existing `--htf-filter` path ({s.get('dataset')} "
                 f"`{s.get('window')}` with its {s.get('warmup_bars')}-bar warm-up, {s.get('frame_bars')} bars): trend "
                 f"identical {fmt(s.get('trend_identical'))}, filtered signal identical "
                 f"{fmt(s.get('filtered_signal_identical'))}, bucket closes identical "
                 f"{fmt(s.get('bucket_closes_identical'))}, fetch calls {s.get('fetch_calls')}.")
        lg = parity.get("leg")
        if lg:
            L.append(f"- The same `htf` leg scored with the derived trend and with the existing-path trend injected: "
                     f"identical {fmt(lg.get('identical'))} ({lg.get('trades')} trades), fetch calls "
                     f"{lg.get('fetch_calls')}.")
    if cpar:
        L.append(f"- Label cache parity ({cpar['dataset']} `{cpar['window']}`): identical {fmt(cpar['identical'])} "
                 f"for {', '.join(sorted(cpar['rows']))}.")
    if smoke:
        c = smoke["checks"]
        L.append(f"- Smoke on synthetic candles: every arm ran {fmt(c['every_arm_ran'])}, `live_both` opened a short "
                 f"{fmt(c['live_both_opened_a_short'])}, entry ATR is Wilder {fmt(c['entry_atr_is_wilder'])} "
                 f"({c['entry_atr_rows']} entries), network calls {c['network_calls']}; live sizing "
                 f"`{json.dumps(smoke.get('live_sizing'), sort_keys=True)}`.")
    rows = fid["rows"]
    L.append(f"- Fidelity: {len(rows)} rows, {sum(1 for r in rows if r['passes'])} pass; engine label check "
             f"{fid['engine_check']['rows_checked']} rows, {fid['engine_check']['sampled_bars']} sampled bars, "
             f"{fid['engine_check']['rows_with_mismatch']} rows with a mismatch.")
    L.append("")
    L += ["| frame | group | dataset | window | kind | regime window | n | agreement | passes |",
          "|---|---|---|---|---|---|---|---|---|"]
    for r in rows:
        u = r["unit"]
        L.append(f"| {u['frame']} | {u['group']} | {u['coin']} {u['interval']} | {u['window']} | {u['kind']} "
                 f"| {u['regime_window']} | {r.get('n')} | {fmt(r.get('agreement'), 4)} | {fmt(r['passes'])} |")
    L.append("")
    L += ["## Coverage", ""]
    for r in cov["rows"]:
        L.append(f"- {r['frame']} {r['group']} {r['dataset']}: {r['status']} ({r['reason']})")
    L.append("")
    if timing:
        L += ["## Run time", "",
              f"`time-leg` measured {timing['seconds_per_window_bar']} s per labelled window-bar on "
              f"{timing['dataset']} `{timing['window']}` (load average {', '.join(fmt(x, 1) for x in timing['machine']['load_avg'])} "
              f"on {timing['machine']['cpus']} CPUs) and projected {timing['projected_process_hours_total']} "
              "process-hours for the label stages.", ""]
    if runlog:
        L.append(f"`judge` starts: {len(runlog.get('starts', []))}; finished runs: {len(runlog.get('runs', []))}.")
        L.append("")
    L += ["## Prior reads", ""] + [f"- {x}" for x in proto["prior_reads"]] + [""]
    L += ["## Disclosures", ""] + [f"- {x}" for x in proto["disclosures"]] + [""]
    text = "\n".join(L).rstrip() + "\n"
    with open(os.path.join(HERE, "REPORT.md"), "w") as fh:
        fh.write(text)
    print(f"wrote {os.path.join(HERE, 'REPORT.md')}")


if __name__ == "__main__":
    render()

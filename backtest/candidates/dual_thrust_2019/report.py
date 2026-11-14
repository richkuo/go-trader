import ast
import json
import os

import run_study as rs

PR = 2026
CMD = "uv run --no-sync python backtest/candidates/dual_thrust_2019/run_study.py"


def path_join_target(node) -> str:
    if not (isinstance(node, ast.Call) and isinstance(node.func, ast.Attribute) and node.func.attr == "join"):
        return ""
    parts = [a.value for a in node.args if isinstance(a, ast.Constant) and isinstance(a.value, str) and a.value != ".."]
    if not parts or not parts[-1].endswith(".py"):
        return ""
    return os.path.normpath(os.path.join(*parts))


def unhashed_loadable_files() -> list:
    hashed = set(rs.source_files())
    queue = sorted(hashed)
    seen = set(queue)
    while queue:
        rel = queue.pop()
        with open(os.path.join(rs.REPO, rel)) as fh:
            tree = ast.parse(fh.read())
        found = []
        for node in ast.walk(tree):
            if isinstance(node, ast.Import):
                names = [a.name for a in node.names]
            elif isinstance(node, ast.ImportFrom) and node.level == 0 and node.module:
                names = [node.module]
            else:
                target = path_join_target(node)
                if target:
                    found.append(target)
                continue
            for name in names:
                parts = name.split(".")
                for d in ("", "backtest", "shared_tools", os.path.dirname(rel)):
                    for k in range(1, len(parts) + 1):
                        base = os.path.join(d, *parts[:k])
                        found.extend((base + ".py", os.path.join(base, "__init__.py")))
        for dep in found:
            if dep not in seen and os.path.isfile(os.path.join(rs.REPO, dep)):
                seen.add(dep)
                queue.append(dep)
    return sorted(seen - hashed)


def sentence(text: str) -> str:
    return text[:1].upper() + text[1:]


def fmt(v, nd=2):
    if v is None:
        return "-"
    if isinstance(v, bool):
        return "yes" if v else "no"
    if isinstance(v, float):
        return f"{v:,.{nd}f}"
    if isinstance(v, int):
        return f"{v:,}"
    return str(v)


def tri(v):
    return "-" if v is None else ("true" if v else "false")


def arm_table(arms: dict) -> list:
    lines = ["| arm | datasets | mean Sharpe | mean return % | net PnL $ | worst DD % | positions (L/S) | pooled clusters "
             "| summed per-coin clusters | fees $ | funding $ | liquidated legs |",
             "|---|---|---|---|---|---|---|---|---|---|---|---|"]
    for name, s in arms.items():
        lines.append(f"| {name} | {s.get('datasets')} | {fmt(s.get('mean_sharpe'), 3)} | {fmt(s.get('mean_return_pct'))} "
                     f"| {fmt(s.get('net_pnl_usd'))} | {fmt(s.get('worst_max_dd_pct'))} | {s.get('positions')} "
                     f"({s.get('long_positions')}/{s.get('short_positions')}) | {s.get('pooled_clusters')} "
                     f"| {s.get('summed_per_coin_clusters')} | {fmt(s.get('fees_usd'))} | {fmt(s.get('funding_pnl_usd'))} "
                     f"| {s.get('liquidated_legs')} |")
    return lines


def per_dataset_table(arm: dict) -> list:
    lines = ["| dataset | Sharpe | return % | net PnL $ | max DD % | positions (L/S) | clusters | fees $ | funding $ "
             "| rejected entries |", "|---|---|---|---|---|---|---|---|---|---|"]
    for ds, v in (arm.get("per_dataset") or {}).items():
        lines.append(f"| {ds} | {fmt(v['sharpe'], 3)} | {fmt(v['return_pct'])} | {fmt(v['net_pnl_usd'])} "
                     f"| {fmt(v['max_dd_pct'])} | {v['positions']} ({v['long_positions']}/{v['short_positions']}) "
                     f"| {v['clusters']} | {fmt(v['fees_usd'])} | {fmt(v['funding_pnl_usd'])} "
                     f"| {v.get('rejected_entries')} |")
    return lines


def m1_lines(m1: dict) -> list:
    if not m1:
        return ["M1 comparison not computed: the evidence is incomplete."]
    lines = [f"M1 verdict `{m1['verdict']}`: candidate mean Sharpe {fmt(m1.get('mean_sharpe'), 3)} vs incumbent "
             f"median {fmt(m1.get('mean_bar_sharpe'), 3)}; candidate mean drawdown-adjusted return "
             f"{fmt(m1.get('mean_ddadj'), 3)} vs {fmt(m1.get('mean_bar_ddadj'), 3)}; traded datasets "
             f"{m1.get('traded_datasets')} of {m1.get('scored_datasets')}.", "",
             "| dataset | incumbent median Sharpe | incumbent median DDadj | incumbents |", "|---|---|---|---|"]
    for ds, b in sorted((m1.get("bars") or {}).items()):
        lines.append(f"| {ds} | {fmt(b['sharpe'], 3)} | {fmt(b['ddadj'], 3)} | {b['n']} |" if b else f"| {ds} | - | - | 0 |")
    return lines


def render(paths: rs.Paths) -> None:
    proto = rs.protocol(paths)
    res = rs.load_json(paths.results)
    ver = rs.load_json(paths.verification) if os.path.exists(paths.verification) else None
    scr = rs.load_json(paths.screen)
    inv = rs.load_json(paths.inventory)
    held = rs.load_json(paths.lock)
    orig = rs.load_json(os.path.join(rs.REPO, proto["references"]["original_results"]["path"]))
    ov = orig["groups"]["venue_4h"]
    v = res["verdict"]
    red = res["primary_reduced"]
    L = []
    L.append("# Dual Thrust Hyperliquid 4h confirmation (issue 2019)")
    L.append("")
    L.append(f"**Primary verdict: {v['outcome'].upper()}** ({v['reason']}). Only the additional-coin 4h population "
             "decides this verdict. Registry metadata, strategy defaults, live and paper configuration, "
             "acknowledgements and deployment decisions are unchanged: `dual_thrust` keeps `edge_status=\"no_edge\"` "
             "and `edge_source=\"study_fail\"`. Every command in this study is suggest-only and sends no order.")
    L.append("")
    L.append("## Question")
    L.append("")
    ob, os_ = ov["held_out"]["candidate"]["base"], ov["held_out"]["candidate"]["stress"]
    L.append(f"The `dual_thrust_1642` venue 4h group (BTC, ETH, SOL; test window "
             f"{orig['venue_manifest']['windows']['test']['start']} to {orig['venue_manifest']['windows']['test']['end']}) "
             f"was `{ov['verdict']['outcome']}` ({ov['verdict']['reason']}) while every performance check passed: base "
             f"mean return {fmt(ob['mean_return_pct'])}%, mean Sharpe {fmt(ob['mean_sharpe'])}, net ${fmt(ob['net_pnl_usd'])}; "
             f"stress mean return {fmt(os_['mean_return_pct'])}%, net ${fmt(os_['net_pnl_usd'])}. This study scores the "
             "same frozen settings, once, on additional Hyperliquid coins chosen by liquidity and coverage only, to "
             f"see whether the pooled sample reaches the {proto['sample_rules']['min_primary_clusters']}-cluster floor "
             "and what the frozen checks say there. An inconclusive result is a valid outcome.")
    L.append("")
    L.append("## Protocol and lock")
    L.append("")
    lg = res.get("lock_git") or {}
    L.append(f"- `protocol.json` sha256 `{held['protocol_sha256']}`; input inventory `study_manifest.json` sha256 "
             f"`{held['inventory_sha256']}`; `screen.json` sha256 `{held['screen_sha256']}`.")
    L.append(f"- `run_study.py lock` wrote `protocol_lock.json` at {held['locked_at_utc']} (working tree "
             f"`{held['source_commit']}`); the lock commit is `{lg.get('lock_commit')}` and it precedes every scored "
             "artifact in branch history (the runner refuses to score otherwise). The lock also records the sha256 of "
             f"{len(held['inputs_sha256'])} input files, {len(held['manifests_sha256'])} manifests and "
             f"{len(held['source_sha256'])} scoring source files.")
    L.append(f"- Scored at source commit `{res['source_commit']}` with network access blocked and a fresh research "
             f"cache; runtime {res['runtime_seconds']} s.")
    L.append("- Frozen candidate: `dual_thrust` " + json.dumps(proto["candidate_params"], sort_keys=True)
             + " with `expected_interval_ms` bound to each interval (the original venue selection). Comparators: "
             + "; ".join(f"`{k}` {json.dumps(p, sort_keys=True)}" for k, p in proto["comparator_params"].items())
             + ". Incumbents (M1 median): " + ", ".join(f"`{n}`" for n in proto["incumbents"]) + ", each with the "
             f"effective parameters read from commit `{proto['references']['incumbent_params_source']['commit']}` "
             f"(source check: {res['references']['incumbent_source_commit_check']}). No grid, re-selection, "
             "alternative-setting diagnostic or seed fallback runs.")
    c = proto["contract"]
    L.append(f"- Matched contract for every arm and incumbent: direction `{c['direction']}`, one `tiered_tp_pct` close "
             f"(+3%, full position), one fixed entry-ATR stop (`stop_loss_atr_mult: {c['stop_loss_atr_mult']}`), "
             f"`comparison_mode: {c['comparison_mode']}`, `intrabar_resolution: {proto['intrabar_resolution']}`, "
             f"${proto['capital_usd']:.0f} per leg.")
    k = proto["costs"]
    L.append(f"- Costs (manifest execution specification): taker fee {k['taker_fee_pct']}, maker fee "
             f"{k['maker_fee_pct']}, slippage {k['slippage_bps']} bps plus the per-coin half spread, szDecimals lot "
             f"rounding, minimum notional ${k['min_notional_usd']:.0f} with a {k['min_notional_margin']} margin. Stress "
             "(cost x2) doubles spread and slippage; fees stay fixed. Hourly funding is charged from the frozen series.")
    drift = res["references"].get("evaluator_sources_changed_since_original") or []
    unhashed = unhashed_loadable_files()
    L.append(f"- Evaluator source files that differ from the original study's recorded versions: "
             + (", ".join(f"`{p}`" for p in drift) if drift else "none")
             + f". The lock hashes {len(held['source_sha256'])} scoring source files. Repository files that the hashed "
             "files can load, by import or by file path, directly or through each other, and that the lock does not "
             "hash (found by a scan of the import statements and the file paths in the code, so the list can name files "
             "that a scoring run does not load): " + ", ".join(f"`{p}`" for p in unhashed)
             + "; `run` does not refuse a change to these files. Registry default parameters that differ from "
             "the frozen values: " + (json.dumps(res["references"]["current_default_params_differ"], sort_keys=True)
                                      if res["references"]["current_default_params_differ"] else "none") + ".")
    L.append("")
    L.append("Ordered verdict rule (primary population only):")
    L.append("")
    for step in proto["verdict_rule"]["order"]:
        L.append(f"- {step}")
    L.append("")
    L.append("## Population screen")
    L.append("")
    L.append(f"Liquidity source: {scr['liquidity_source']}. Observation time {scr['observed_at_utc']} (raw response "
             f"`{scr['liquidity_response']['path']}`, sha256 `{scr['liquidity_response']['sha256']}`). Rule: "
             f"{scr['rule']}. The screen read only liquidity, listing and coverage information.")
    L.append("")
    L.append("| rank | coin | 24h notional volume $ | 4h window bars | warm-up bars | invalid bars | funding hours "
             "| selected | reasons |")
    L.append("|---|---|---|---|---|---|---|---|---|")
    for s in scr["screened"]:
        cc, fc = s["candles"], s["funding"]
        L.append(f"| {s['rank']} | {s['coin']} | {s['day_ntl_vlm']:,.0f} | {cc['present_window_bars']}/"
                 f"{cc['expected_window_bars']} | {cc['present_warmup_bars']}/{cc['warmup_bars']} | {cc['invalid_bars']} "
                 f"| {fc['present_hours']}/{fc['expected_hours']} | {fmt(s['selected'])} "
                 f"| {'; '.join(cc['reasons'] + fc['reasons']) or '-'} |")
    L.append("")
    L.append(f"Not screened (ranked below the last selection): {len(scr['not_screened'])} coins. Excluded before "
             f"ranking: {len(scr['excluded'])} ("
             + ", ".join(f"{r['coin']}: {r['reason']}" for r in scr["excluded"] if r["reason"] != "isDelisted")
             + f"; {sum(1 for r in scr['excluded'] if r['reason'] == 'isDelisted')} delisted). Named candidates: "
             + "; ".join(f"{c} rank {d['rank']}, selected {fmt(d['selected'])}"
                         + (f" ({'; '.join(d['reasons'])})" if d["reasons"] else "")
                         for c, d in scr["named_candidates"].items()) + ".")
    L.append("")
    L.append("## Populations")
    L.append("")
    L.append("| population | role | interval | window (end exclusive) | warm-up | roster |")
    L.append("|---|---|---|---|---|---|")
    for pop in rs.POPULATIONS:
        cfg = proto["populations"][pop]
        L.append(f"| {pop} | {cfg['role']} | {cfg['interval']} | {cfg['window'][0]} to {cfg['window'][1]} "
                 f"| {proto['warmup_bars']} bars | {', '.join(res['rosters'][pop]) or '-'} |")
    L.append("")
    L.append(sentence(proto["calendar_overlap"]) + ".")
    L.append("")
    L.append("## Inputs")
    L.append("")
    L.append("| population | dataset | size decimals | half spread bps | candles complete | warm-up range | funding hours "
             "| candles sha256 | funding sha256 |")
    L.append("|---|---|---|---|---|---|---|---|---|")
    for pop in rs.POPULATIONS:
        pr = res["populations"].get(pop) or {}
        for ds, d in sorted(((pr.get("inputs") or {}).get("datasets") or {}).items()):
            L.append(f"| {pop} | {ds} | {d['size_decimals']} | {d['half_spread_bps']} | {fmt(d['candles']['complete'])} "
                     f"| {' to '.join(d['candles']['warmup_range'] or ['-'])} | {d['funding'].get('present_hours', '-')}/"
                     f"{d['funding'].get('expected_hours', '-')} | `{d['candles_sha256'][:16]}` "
                     f"| `{(d['funding_sha256'] or '-')[:16]}` |")
    L.append("")
    L.append(f"Every file and its full sha256, row count, scored range and warm-up range is in `study_manifest.json` "
             f"({len(inv['files'])} files, acquired {inv['acquired_at_utc']}).")
    L.append("")
    L.append("## Primary population (verdict)")
    L.append("")
    L.append(f"Evidence complete: {fmt(red['evidence_complete'])}."
             + ("" if red["evidence_complete"] else " Reasons: " + "; ".join(red["evidence_reasons"]) + "."))
    L.append("")
    cand = red["arms"]["base"]["candidate"]
    L.append(f"Pooled candidate base-cost clusters: **{cand.get('pooled_clusters')}** (floor {v['floor']}), merged from "
             f"position spans across all primary coins. Per-coin clusters: "
             + ", ".join(f"{ds} {n}" for ds, n in (cand.get("per_coin_clusters") or {}).items())
             + f" (sum {cand.get('summed_per_coin_clusters')}, which is never used as the pooled total).")
    L.append("")
    L.append("| check | value |")
    L.append("|---|---|")
    checks = v["checks"] or v.get("checks_not_used_below_floor") or {}
    for name in proto["verdict_rule"]["checks"]:
        L.append(f"| {name} | {tri(checks.get(name))} |")
    L.append("")
    if v["checks"] is None and v.get("checks_not_used_below_floor"):
        L.append("The sample is below the floor, so these performance checks are reported for the record and do not "
                 "decide the verdict.")
        L.append("")
    for cost in rs.COSTS:
        L.append(f"### Cost {cost} (x{proto['cost_multipliers'][cost]:g})")
        L.append("")
        L.extend(arm_table(red["arms"][cost]))
        L.append("")
        L.extend(m1_lines(red["m1"].get(cost)))
        L.append("")
        L.append(f"Candidate per dataset, cost {cost}:")
        L.append("")
        L.extend(per_dataset_table(red["arms"][cost]["candidate"]))
        L.append("")
    for pop, dred in res["descriptive_reduced"].items():
        cfg = proto["populations"][pop]
        L.append(f"## Descriptive population `{pop}` ({cfg['interval']}, {cfg['window'][0]} to {cfg['window'][1]})")
        L.append("")
        L.append(f"Role: {cfg['role']}. Evidence complete: {fmt(dred['evidence_complete'])}."
                 + ("" if dred["evidence_complete"] else " Reasons: " + "; ".join(dred["evidence_reasons"][:20])
                    + (f" (+{len(dred['evidence_reasons']) - 20} more)" if len(dred["evidence_reasons"]) > 20 else "")
                    + "."))
        L.append("")
        for cost in rs.COSTS:
            L.append(f"Cost {cost}:")
            L.append("")
            L.extend(arm_table(dred["arms"][cost]))
            L.append("")
            if dred["m1"].get(cost):
                L.extend(m1_lines(dred["m1"][cost])[:1])
                L.append("")
        L.append("Candidate per dataset, cost base:")
        L.append("")
        L.extend(per_dataset_table(dred["arms"]["base"]["candidate"]))
        L.append("")
    audit = res["populations"][rs.PRIMARY].get("entry_audit") or {}
    L.append("## Entry audit")
    L.append("")
    L.append(f"Primary candidate base-cost entries audited: {audit.get('entries')}; failures: "
             f"{len(audit.get('failures') or [])}. Each fill must follow a valid raw signal on the prior closed bar, "
             "enter inside the window (never in the warm-up) and exit before the window end. First entries:")
    L.append("")
    L.append("| dataset | side | signal bar | fill bar | exit bar | anchor | upper | lower | close |")
    L.append("|---|---|---|---|---|---|---|---|---|")
    for a in (audit.get("rows") or [])[:12]:
        L.append(f"| {a['dataset']} | {a['side']} | {a.get('signal_bar')} | {a['fill_bar']} | {a['exit_bar']} "
                 f"| {a.get('anchor', 0):.6g} | {a.get('upper', 0):.6g} | {a.get('lower', 0):.6g} | {a.get('close', 0):.6g} |")
    L.append("")
    L.append("## Verification")
    L.append("")
    if ver:
        L.append(f"`run_study.py verify` ran the real loader, evaluator and verdict paths on disposable copies and on "
                 f"saved leg evidence (all checks passed: {fmt(ver['all_ok'])}; results sha256 "
                 f"`{ver['results_sha256'][:16]}`). Fixture rows test verdict branches and are not market evidence.")
        L.append("")
        L.append("| case | kind | expected | observed | ok |")
        L.append("|---|---|---|---|---|")
        for cse in ver["cases"]:
            obs = cse["observed"].replace("|", "/").replace("\n", " ")
            if len(obs) > 220:
                obs = obs[:220] + "..."
            L.append(f"| {cse['name']} | {cse['kind']} | {cse['expected']} | {obs} | {fmt(cse['ok'])} |")
    else:
        L.append("Verification has not been run for this results file.")
    L.append("")
    L.append("## Limits")
    L.append("")
    L.append("- The roster comes from one 24h notional volume snapshot taken after the scored window, so it favours "
             "coins that are liquid and listed today (survivorship). The screen never read strategy outcomes.")
    L.append("- " + sentence(proto["calendar_overlap"]) + ".")
    L.append("- Pooling more coins can merge existing holding spans into one cluster, so more coins do not guarantee "
             "more clusters; the pooled count above is merged from spans and never summed.")
    L.append("- Spread is one frozen L2 snapshot per coin plus flat slippage; queue position, partial fills and "
             "market impact are not modelled. The venue serves only the most recent 5,000 candles per interval, "
             "so a later acquisition cannot reproduce the earliest bars; reproduction uses the committed files.")
    L.append("- The descriptive populations are short or use a different interval; they never enter the verdict.")
    L.append("- A research result alone cannot approve deployment.")
    L.append("")
    L.append("## Reproduction")
    L.append("")
    L.append("```")
    L.append(f"{CMD} acquire            # network; already done, refuses to overwrite frozen inputs")
    L.append(f"{CMD} lock               # already done; confirms the existing lock")
    L.append(f"{CMD} run --cache-db <fresh-research-path>/cache.db --out <path>/rerun.json")
    L.append(f"{CMD} verify --work-dir <fresh-empty-directory>")
    L.append(f"{CMD} render")
    for pop in rs.POPULATIONS:
        L.append(f"uv run --no-sync python backtest/offline_manifest.py verify --manifest "
                 f"backtest/candidates/dual_thrust_2019/{proto['populations'][pop]['manifest']}")
    L.append("```")
    L.append("")
    with open(os.path.join(paths.root, "REPORT.md"), "w") as fh:
        fh.write("\n".join(L))
    readme(paths, proto, res)
    print(f"wrote {os.path.join(paths.root, 'REPORT.md')} and README.md")


def readme(paths: rs.Paths, proto: dict, res: dict) -> None:
    v = res["verdict"]
    lock_commit = (res.get("lock_git") or {}).get("lock_commit")
    L = ["# dual_thrust_2019", "",
         "Frozen confirmation of the `dual_thrust_1642` Hyperliquid 4h settings on additional coins (issue 2019). "
         f"Primary verdict: **{v['outcome']}** ({v['reason']}). Every number is in `REPORT.md`; this file lists what "
         "each artifact is for. Everything here is suggest-only: no command writes configuration, registry metadata, "
         "defaults or acknowledgements, and none sends an order.", "",
         "| file | role |", "|---|---|",
         "| `protocol.json` | Frozen rules: settings, contract, costs, populations, screen, sample floor and the ordered verdict rule. Committed before acquisition. |",
         "| `screen.json` | The liquidity-and-coverage screen: observation time, ranking, every screened coin with its checks, and the frozen rosters. |",
         "| `study_manifest.json` | Input inventory: every file under `data/` with sha256, rows, coin, interval, acquired, scored and warm-up ranges, and the manifest hashes. |",
         "| `venue_primary_4h.json`, `venue_forward_4h.json`, `venue_descriptive_1h.json` | One `offline_candle_manifest/v1` manifest per population. |",
         "| `data/venue/` | Frozen Hyperliquid candles, hourly funding, venue meta, the liquidity response and the L2 snapshot. |",
         "| `protocol_lock.json` | Written by `lock` before any score: protocol, inventory, screen, manifest, input and scoring-source hashes. |",
         "| `run_study.py` | `acquire` (network), `lock`, `run` (offline, network blocked, fresh research cache), `verify`, `render`. |",
         "| `verify_study.py` | Integration verification on disposable copies and saved evidence (`run_study.py verify`). |",
         "| `report.py` | Writes `REPORT.md` and this file from the committed results (`run_study.py render`). |",
         "| `results.json` | Every leg at both costs for every population, input checks, the entry audit, reductions and the verdict. |",
         "| `verification.json` | Outcome of every verification case. |", "",
         "## Reproduction", "", "```",
         f"{CMD} run --cache-db <fresh-research-path>/cache.db --out <path>/rerun.json",
         f"{CMD} verify --work-dir <fresh-empty-directory>",
         f"{CMD} render", "```", "",
         "`run` refuses an existing cache, a missing or changed lock, an unregistered input, uncommitted locked files "
         "and changed scoring sources. Without `--out` it refuses to replace `results.json`.", "",
         f"Run `run` at the lock commit `{lock_commit}`, and `verify` at the pull request head. Do not use the "
         "working-tree `source_commit` that `protocol_lock.json` records: that commit has no inputs and no lock. "
         "Where the runner's refusal text says \"the lock's source commit\", use the lock commit.", "",
         "`run` and `verify` also check that the lock commit holds no scored file. This repository squash-merges pull "
         "requests, so on `main` one commit holds both `protocol_lock.json` and `results.json`, and both commands refuse "
         "there with \"results.json already exists in the lock commit\". The branch commits stay reachable through the "
         f"pull request ref: `git fetch origin pull/{PR}/head`, then `git checkout {lock_commit}` for `run`, or "
         "`git checkout FETCH_HEAD` for `verify`.", ""]
    with open(os.path.join(paths.root, "README.md"), "w") as fh:
        fh.write("\n".join(L))

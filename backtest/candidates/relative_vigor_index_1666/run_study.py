#!/usr/bin/env python3

import argparse
import json
import os
import statistics
import sys

import pandas as pd

_HERE = os.path.dirname(os.path.abspath(__file__))
_BACKTEST = os.path.abspath(os.path.join(_HERE, "..", ".."))
_REPO = os.path.abspath(os.path.join(_BACKTEST, ".."))
sys.path.insert(0, _BACKTEST)
sys.path.insert(0, os.path.join(_REPO, "shared_tools"))

import offline_manifest as om
from eval_windows import (INCUMBENTS, evaluate_window, expand_sweep,
                          manifest_datasets)
from optimizer import DEFAULT_PARAM_RANGES
from parity_diff import ParityConfig, compute_parity_frame

MANIFEST = os.path.join(_HERE, "study_manifest.json")
CANDIDATE = "relative_vigor_index"
COMPARATORS = ("momentum_pro", "sma_crossover", "ema_crossover")
ABLATION_OVERRIDE = {"zero_line_filter": False}
PARITY_WINDOWS = (None, 200)
CLOSE_STRATEGIES = [{"name": "time_stop", "params": {"max_bars": 20}}]
STOP_LOSS_ATR_MULT = 1.0
DIRECTION = "both"
CAPITAL = 1000.0
SELECTION_WINDOW = "train"
HELD_OUT_WINDOW = "test"
BASE_COST = 1.0
STRESS_COST = 2.0
MIN_TRAIN_POSITIONS = 10
MIN_TEST_POSITIONS = 30
SOURCE_FILES = (
    "shared_strategies/open/relative_vigor_index.py",
    "shared_strategies/open/registry.py",
    "shared_tools/strategy_composition.py",
    "shared_strategies/close/time_stop.py",
    "backtest/backtester.py",
    "backtest/eval_windows.py",
    "backtest/offline_manifest.py",
    "backtest/optimizer.py",
    "backtest/parity_diff.py",
    "backtest/candidates/relative_vigor_index_1666/run_study.py",
)


def _sha(path: str) -> str:
    return om.sha256_file(path)


def arm_candidate(name: str, params: dict) -> dict:
    return {
        "name": name,
        "params": dict(params),
        "direction": DIRECTION,
        "close_strategies": [dict(c, params=dict(c["params"])) for c in CLOSE_STRATEGIES],
        "stop_loss_atr_mult": STOP_LOSS_ATR_MULT,
    }


def _grid(name: str, defaults: dict) -> list:
    ranges = DEFAULT_PARAM_RANGES[name]
    specs = [(k, list(v)) for k, v in ranges.items()]
    return expand_sweep(dict(defaults), specs)


def _leg_rows(score: dict) -> list:
    return [r for r in score["rows"] if r["leg"] is not None]


def summarize(score: dict, window_bars: dict) -> dict:
    rows = _leg_rows(score)
    if not rows:
        return {"datasets": 0}
    legs = [r["leg"] for r in rows]
    positions = []
    per_dataset = {}
    for r in rows:
        ex = r["leg"]["execution"]
        per_dataset[r["dataset"]] = {
            "return_pct": r["leg"]["return_pct"],
            "sharpe": r["leg"]["sharpe"],
            "max_dd_pct": r["leg"]["max_dd_pct"],
            "ddadj": r["leg"]["ddadj"],
            "net_pnl_usd": ex["net_pnl_usd"],
            "fees_usd": ex["fees_usd"],
            "funding_pnl_usd": ex["funding_pnl_usd"],
            "turnover": ex["turnover"],
            "positions": ex["positions"],
            "long_positions": ex["long_positions"],
            "short_positions": ex["short_positions"],
            "time_in_market": round(ex["bars_in_market"] / window_bars[r["dataset"]], 4),
            "rejected_entries": ex.get("rejected_entries", 0),
            "skipped_partial_closes": ex.get("skipped_partial_closes", 0),
            "candle_coverage_complete": r["leg"]["manifest"]["candle_coverage"]["complete"],
            "funding_coverage_complete": r["leg"]["manifest"]["funding_coverage"]["complete"],
            "beats_incumbent_sharpe": r["beats_sharpe"],
            "beats_incumbent_ddadj": r["beats_ddadj"],
        }
        for p in ex["position_list"]:
            positions.append(dict(p, dataset=r["dataset"]))
    abs_ds = [abs(v["net_pnl_usd"]) for v in per_dataset.values()]
    abs_pos = sorted((abs(p["net_pnl"]) for p in positions), reverse=True)
    overlap = 0
    spans = [(pd.Timestamp(p["entry_date"]), pd.Timestamp(p["exit_date"]), p["side"], p["dataset"])
             for p in positions]
    for i, (s, e, side, ds) in enumerate(spans):
        if any(ds2 != ds and side2 == side and s2 <= e and s <= e2
               for j, (s2, e2, side2, ds2) in enumerate(spans) if j != i):
            overlap += 1
    return {
        "datasets": len(rows),
        "mean_return_pct": round(statistics.mean(l["return_pct"] for l in legs), 4),
        "mean_sharpe": round(statistics.mean(l["sharpe"] for l in legs), 4),
        "mean_ddadj": round(statistics.mean(l["ddadj"] for l in legs), 4),
        "worst_max_dd_pct": round(min(l["max_dd_pct"] for l in legs), 4),
        "net_pnl_usd": round(sum(v["net_pnl_usd"] for v in per_dataset.values()), 4),
        "fees_usd": round(sum(v["fees_usd"] for v in per_dataset.values()), 4),
        "funding_pnl_usd": round(sum(v["funding_pnl_usd"] for v in per_dataset.values()), 4),
        "mean_turnover": round(statistics.mean(v["turnover"] for v in per_dataset.values()), 4),
        "positions": len(positions),
        "long_positions": sum(1 for p in positions if p["side"] == "long"),
        "short_positions": sum(1 for p in positions if p["side"] == "short"),
        "liquidated_legs": sum(1 for l in legs if l.get("liquidated")),
        "top_dataset_abs_pnl_share": (round(max(abs_ds) / sum(abs_ds), 4) if sum(abs_ds) > 0 else None),
        "top5_position_abs_pnl_share": (round(sum(abs_pos[:5]) / sum(abs_pos), 4) if sum(abs_pos) > 0 else None),
        "same_side_cross_asset_overlap_share": (round(overlap / len(positions), 4) if positions else None),
        "coverage_complete": all(v["candle_coverage_complete"] and v["funding_coverage_complete"]
                                 for v in per_dataset.values()),
        "incumbent_m1_verdict": score.get("verdict"),
        "incumbent_bar_mean_sharpe": score.get("mean_bar_sharpe"),
        "incumbent_bar_mean_ddadj": score.get("mean_bar_ddadj"),
        "per_dataset": per_dataset,
    }


def select(rows: list) -> dict:
    eligible = [r for r in rows
                if r["summary"]["datasets"] == r["summary"].get("expected_datasets")
                and r["summary"]["liquidated_legs"] == 0
                and r["summary"]["positions"] >= MIN_TRAIN_POSITIONS]
    if not eligible:
        return {"rule_outcome": "no_eligible_combo", "selected": None}
    best = max(enumerate(eligible),
               key=lambda kv: (kv[1]["summary"]["mean_sharpe"],
                               kv[1]["summary"]["mean_ddadj"], -kv[0]))[1]
    return {"rule_outcome": "selected", "selected": best["label"], "params": best["params"]}


def neighbors(label_params: dict, rows: list, name: str) -> dict:
    ranges = DEFAULT_PARAM_RANGES[name]
    near = []
    for r in rows:
        diffs = []
        for k, grid in ranges.items():
            a, b = grid.index(label_params[k]), grid.index(r["params"][k])
            diffs.append(abs(a - b))
        if sum(diffs) == 1 and max(diffs) == 1:
            near.append(r["summary"]["mean_sharpe"])
    return {"neighbor_count": len(near),
            "neighbor_median_sharpe": round(statistics.median(near), 4) if near else None,
            "neighbor_min_sharpe": round(min(near), 4) if near else None}


def _rank(values: list) -> list:
    order = sorted(range(len(values)), key=lambda i: values[i])
    ranks = [0.0] * len(values)
    i = 0
    while i < len(order):
        j = i
        while j + 1 < len(order) and values[order[j + 1]] == values[order[i]]:
            j += 1
        for k in range(i, j + 1):
            ranks[order[k]] = (i + j) / 2.0
        i = j + 1
    return ranks


def spearman(a: list, b: list):
    if len(a) < 3:
        return None
    ra, rb = _rank(a), _rank(b)
    ma, mb = statistics.mean(ra), statistics.mean(rb)
    num = sum((x - ma) * (y - mb) for x, y in zip(ra, rb))
    den = (sum((x - ma) ** 2 for x in ra) * sum((y - mb) ** 2 for y in rb)) ** 0.5
    return round(num / den, 4) if den else None


def verdict(test: dict) -> dict:
    cand = test[CANDIDATE]["selected"]["base"]
    stress = test[CANDIDATE]["selected"]["stress"]
    comps = {c: test[c]["selected"]["base"] for c in COMPARATORS}
    checks = {
        "coverage_complete": cand["coverage_complete"] and all(c["coverage_complete"] for c in comps.values()),
        "sample_sufficient": cand["positions"] >= MIN_TEST_POSITIONS,
        "net_positive_base": cand["mean_return_pct"] > 0,
        "net_positive_stress": stress["mean_return_pct"] > 0,
        "beats_comparators_sharpe": all(cand["mean_sharpe"] > c["mean_sharpe"] for c in comps.values()),
        "beats_comparators_ddadj": all(cand["mean_ddadj"] > c["mean_ddadj"] for c in comps.values()),
    }
    if not checks["coverage_complete"]:
        outcome, reason = "inconclusive", "candle or funding coverage incomplete on the held-out window"
    elif not checks["sample_sufficient"]:
        outcome, reason = "inconclusive", (
            f"{cand['positions']} independent held-out positions < {MIN_TEST_POSITIONS}")
    elif all(checks.values()):
        outcome, reason = "pass", "every frozen held-out check passed"
    else:
        failed = sorted(k for k, v in checks.items() if not v)
        outcome, reason = "fail", "failed frozen checks: " + ", ".join(failed)
    return {"outcome": outcome, "reason": reason, "checks": checks,
            "live_status": "research-only; backtest_only=True regardless of outcome"}


def frozen_parity(manifest: dict, params: dict) -> list:
    cfg = ParityConfig(strategy_name=CANDIDATE, params=dict(params), registry="futures",
                       close_refs=[dict(c, params=dict(c["params"])) for c in CLOSE_STRATEGIES],
                       direction=DIRECTION)
    rows = []
    for ds in manifest["datasets"]:
        frame, _, _ = om.window_frame(manifest, ds, HELD_OUT_WINDOW)
        for window in PARITY_WINDOWS:
            parity = compute_parity_frame(frame, cfg=cfg, window=window)
            rows.append({
                "dataset": ds["key"],
                "window": "every prefix" if window is None else window,
                "frame_bars": len(frame),
                "compared_bars": int(len(parity)),
                "mismatches": int((~parity["match"]).sum()),
                "entry_decisions": int((parity["live_signal"] != 0).sum()),
            })
    return rows


def run(manifest_path: str) -> dict:
    from registry_loader import load_registry
    manifest = om.load_manifest(manifest_path)
    reg = load_registry("futures")
    datasets = manifest_datasets(manifest)
    window_bars = {}
    for ds in manifest["datasets"]:
        for w in (SELECTION_WINDOW, HELD_OUT_WINDOW):
            _, _, cov = om.window_frame(manifest, ds, w)
            window_bars.setdefault(w, {})[ds["key"]] = cov["present_bars"]
    memo: dict = {}

    def score(name, params, window, cost):
        s = evaluate_window(reg, arm_candidate(name, params), datasets, window, CAPITAL, memo,
                            manifest=manifest, cost_multiplier=cost)
        out = summarize(s, window_bars[window])
        out["expected_datasets"] = len(datasets)
        return out

    arms = {}
    for name in (CANDIDATE,) + COMPARATORS:
        defaults = dict(reg.STRATEGY_REGISTRY[name]["default_params"])
        train_rows = []
        for label, params in _grid(name, defaults):
            summary = score(name, params, SELECTION_WINDOW, BASE_COST)
            summary.pop("per_dataset", None)
            train_rows.append({"label": label, "params": params, "summary": summary})
        choice = select(train_rows)
        selected_params = choice["params"] if choice["selected"] else defaults
        arms[name] = {
            "defaults": defaults,
            "grid": {k: list(v) for k, v in DEFAULT_PARAM_RANGES[name].items()},
            "train_sweep": train_rows,
            "selection": choice,
            "selected_params": selected_params,
            "selected_plateau": (neighbors(selected_params, train_rows, name)
                                 if choice["selected"] else None),
        }

    test = {}
    for name, arm in arms.items():
        test[name] = {
            "selected": {
                "params": arm["selected_params"],
                "base": score(name, arm["selected_params"], HELD_OUT_WINDOW, BASE_COST),
                "stress": score(name, arm["selected_params"], HELD_OUT_WINDOW, STRESS_COST),
            },
            "defaults": {
                "params": arm["defaults"],
                "base": score(name, arm["defaults"], HELD_OUT_WINDOW, BASE_COST),
            },
        }

    ablation_params = dict(arms[CANDIDATE]["selected_params"], **ABLATION_OVERRIDE)
    ablation = {
        "params": ablation_params,
        "train": score(CANDIDATE, ablation_params, SELECTION_WINDOW, BASE_COST),
        "held_out": score(CANDIDATE, ablation_params, HELD_OUT_WINDOW, BASE_COST),
    }
    ablation["train"].pop("per_dataset", None)

    variants = []
    for row in arms[CANDIDATE]["train_sweep"]:
        held = score(CANDIDATE, row["params"], HELD_OUT_WINDOW, BASE_COST)
        variants.append({"label": row["label"], "params": row["params"],
                         "train_mean_sharpe": row["summary"]["mean_sharpe"],
                         "test_mean_sharpe": held["mean_sharpe"],
                         "test_mean_return_pct": held["mean_return_pct"],
                         "test_positions": held["positions"]})
    stability = {
        "spearman_train_vs_test_sharpe": spearman(
            [v["train_mean_sharpe"] for v in variants],
            [v["test_mean_sharpe"] for v in variants]),
        "test_net_positive_variants": sum(1 for v in variants if v["test_mean_return_pct"] > 0),
        "variants": len(variants),
    }

    return {
        "study": manifest["study"],
        "issue": 1666,
        "manifest": {
            "path": os.path.relpath(manifest["path"], _REPO),
            "sha256": _sha(manifest["path"]),
            "provenance": manifest["provenance"],
            "interval": manifest["interval"],
            "warmup_bars": manifest["warmup_bars"],
            "windows": {k: {"start": v["start"], "end": v["end"], "role": v["role"]}
                        for k, v in manifest["windows"].items()},
            "costs": manifest["costs"],
            "datasets": [{"key": d["key"], "size_decimals": d["size_decimals"],
                          "half_spread_bps": d["half_spread_bps"],
                          "candles_sha256": d["candles"]["sha256"],
                          "funding_sha256": d["funding"]["sha256"] if d["funding"] else None}
                         for d in manifest["datasets"]],
            "venue_reference": {
                "meta_sha256": manifest["venue_reference"]["meta"]["sha256"],
                "l2_snapshot_sha256": manifest["venue_reference"].get("l2_snapshot", {}).get("sha256"),
                "documents": manifest["venue_reference"]["documents"],
            },
        },
        "source_sha256": {p: _sha(os.path.join(_REPO, p)) for p in SOURCE_FILES},
        "protocol": {
            "close_strategies": CLOSE_STRATEGIES,
            "stop_loss_atr_mult": STOP_LOSS_ATR_MULT,
            "direction": DIRECTION,
            "capital": CAPITAL,
            "selection_window": SELECTION_WINDOW,
            "held_out_window": HELD_OUT_WINDOW,
            "selection_rule": (
                "eligible = all datasets scored, no liquidated leg, >= "
                f"{MIN_TRAIN_POSITIONS} positions; pick max mean Sharpe, then max mean "
                "DD-adjusted return, then earliest grid order"),
            "cost_multipliers": {"base": BASE_COST, "stress": STRESS_COST},
            "min_test_positions": MIN_TEST_POSITIONS,
            "incumbents_reference": list(INCUMBENTS),
        },
        "arms": arms,
        "held_out": test,
        "zero_line_ablation": ablation,
        "candidate_variants_held_out": variants,
        "parameter_stability": stability,
        "frozen_parity": frozen_parity(manifest, arms[CANDIDATE]["selected_params"]),
        "verdict": verdict(test),
    }


def _fmt(v, nd=2):
    if v is None:
        return "-"
    if isinstance(v, bool):
        return "yes" if v else "no"
    if isinstance(v, float):
        return f"{v:.{nd}f}"
    return str(v)


def render(result: dict) -> str:
    v = result["verdict"]
    lines = [
        "# Relative Vigor Index crossover: held-out result (#1666)",
        "",
        f"Verdict: **{v['outcome'].upper()}**. {v['reason']}. {v['live_status']}.",
        "",
        "Generated by `run_study.py` from `results.json`; do not edit by hand.",
        "",
        "## Inputs",
        "",
        f"- Manifest `{result['manifest']['path']}` sha256 `{result['manifest']['sha256']}`",
        f"- Provenance: {result['manifest']['provenance']['kind']}: {result['manifest']['provenance']['label']}",
        f"- Interval {result['manifest']['interval']}, warm-up {result['manifest']['warmup_bars']} bars (not scored)",
    ]
    for name, w in sorted(result["manifest"]["windows"].items(), key=lambda kv: kv[1]["start"]):
        lines.append(f"- Window `{name}` [{w['start']}, {w['end']}): {w['role']}")
    c = result["manifest"]["costs"]
    lines.append(
        f"- Costs: taker {c['taker_fee_pct']*100:.3f}%, maker {c['maker_fee_pct']*100:.3f}%, "
        f"slippage {c['slippage_bps']} bps per side plus per-coin half spread, minimum order "
        f"${c['min_notional_usd']:.0f} with a {c['min_notional_margin']*100:.0f}% margin, funding booked hourly")
    for d in result["manifest"]["datasets"]:
        lines.append(f"- `{d['key']}`: szDecimals {d['size_decimals']}, half spread {d['half_spread_bps']} bps, "
                     f"candles `{d['candles_sha256'][:16]}`, funding `{(d['funding_sha256'] or '')[:16]}`")
    p = result["protocol"]
    lines += [
        "",
        "## Protocol",
        "",
        f"- Every arm: direction `{p['direction']}`, close owner `time_stop` (max_bars "
        f"{p['close_strategies'][0]['params']['max_bars']}), stop owner fixed entry ATR x{p['stop_loss_atr_mult']}, "
        f"capital ${p['capital']:.0f}, fills at the next bar open.",
        f"- Selection: {p['selection_rule']}; scored on `{p['selection_window']}` only.",
        f"- Held-out `{p['held_out_window']}` scored once per arm at cost x{p['cost_multipliers']['base']} "
        f"and x{p['cost_multipliers']['stress']}.",
        "",
        "## Selections (train)",
        "",
        "| Arm | Selected params | Train mean Sharpe | Neighbour median Sharpe |",
        "|---|---|---|---|",
    ]
    for name in (CANDIDATE,) + COMPARATORS:
        arm = result["arms"][name]
        sel = next((r for r in arm["train_sweep"] if r["label"] == arm["selection"].get("selected")), None)
        plateau = arm["selected_plateau"] or {}
        lines.append(f"| `{name}` | `{json.dumps(arm['selected_params'], sort_keys=True)}` | "
                     f"{_fmt(sel['summary']['mean_sharpe'] if sel else None)} | "
                     f"{_fmt(plateau.get('neighbor_median_sharpe'))} |")
    lines += [
        "",
        "## Held-out comparison",
        "",
        "| Arm | Cost | Mean ret % | Mean Sharpe | Mean DDadj | Worst DD % | Net $ | Fees $ | Funding $ | Turnover | Positions (L/S) | Top dataset share | Top-5 share | Cross-asset overlap | M1 incumbent verdict |",
        "|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|",
    ]
    for name in (CANDIDATE,) + COMPARATORS:
        t = result["held_out"][name]
        for label, s in (("selected x1", t["selected"]["base"]), ("selected x2", t["selected"]["stress"]),
                         ("defaults x1", t["defaults"]["base"])):
            lines.append(
                f"| `{name}` | {label} | {_fmt(s['mean_return_pct'])} | {_fmt(s['mean_sharpe'], 3)} | "
                f"{_fmt(s['mean_ddadj'], 3)} | {_fmt(s['worst_max_dd_pct'])} | {_fmt(s['net_pnl_usd'])} | "
                f"{_fmt(s['fees_usd'])} | {_fmt(s['funding_pnl_usd'])} | {_fmt(s['mean_turnover'])} | "
                f"{s['positions']} ({s['long_positions']}/{s['short_positions']}) | "
                f"{_fmt(s['top_dataset_abs_pnl_share'])} | {_fmt(s['top5_position_abs_pnl_share'])} | "
                f"{_fmt(s['same_side_cross_asset_overlap_share'])} | {s['incumbent_m1_verdict']} |")
    lines += ["", "## Candidate per dataset (held-out, selected, cost x1)", "",
              "| Dataset | Ret % | Sharpe | Max DD % | Net $ | Fees $ | Funding $ | Positions (L/S) | Time in market | Rejected entries | Skipped partial closes |",
              "|---|---|---|---|---|---|---|---|---|---|---|"]
    for ds, s in sorted(result["held_out"][CANDIDATE]["selected"]["base"]["per_dataset"].items()):
        lines.append(f"| {ds} | {_fmt(s['return_pct'])} | {_fmt(s['sharpe'], 3)} | {_fmt(s['max_dd_pct'])} | "
                     f"{_fmt(s['net_pnl_usd'])} | {_fmt(s['fees_usd'])} | {_fmt(s['funding_pnl_usd'])} | "
                     f"{s['positions']} ({s['long_positions']}/{s['short_positions']}) | "
                     f"{_fmt(s['time_in_market'])} | {s['rejected_entries']} | {s['skipped_partial_closes']} |")
    ab = result["zero_line_ablation"]
    lines += ["", "## Zero-line ablation (selected params, filter off, cost x1)", "",
              f"- Params `{json.dumps(ab['params'], sort_keys=True)}`",
              "",
              "| Window | Mean ret % | Mean Sharpe | Mean DDadj | Worst DD % | Net $ | Positions (L/S) |",
              "|---|---|---|---|---|---|---|"]
    for label, s in (("train", ab["train"]), ("held-out", ab["held_out"])):
        lines.append(f"| {label} | {_fmt(s['mean_return_pct'])} | {_fmt(s['mean_sharpe'], 3)} | "
                     f"{_fmt(s['mean_ddadj'], 3)} | {_fmt(s['worst_max_dd_pct'])} | {_fmt(s['net_pnl_usd'])} | "
                     f"{s['positions']} ({s['long_positions']}/{s['short_positions']}) |")
    lines += ["", "## Frozen-input parity (non-batched, held-out frame with warm-up)", "",
              "| Dataset | Window | Frame bars | Compared bars | Entry decisions | Mismatches |",
              "|---|---|---|---|---|---|"]
    for row in result["frozen_parity"]:
        lines.append(f"| {row['dataset']} | {row['window']} | {row['frame_bars']} | {row['compared_bars']} | "
                     f"{row['entry_decisions']} | {row['mismatches']} |")
    st = result["parameter_stability"]
    lines += ["", "## Verdict checks", ""]
    for k, ok in sorted(v["checks"].items()):
        lines.append(f"- {k}: {_fmt(ok)}")
    lines += [
        "",
        "## Parameter stability and unsuccessful variants",
        "",
        f"- Spearman rank correlation of train vs held-out mean Sharpe over all "
        f"{st['variants']} candidate variants: {_fmt(st['spearman_train_vs_test_sharpe'], 3)}",
        f"- Held-out net-positive variants: {st['test_net_positive_variants']}/{st['variants']}",
        "",
        "| Variant | Train Sharpe | Held-out Sharpe | Held-out ret % | Held-out positions |",
        "|---|---|---|---|---|",
    ]
    for row in result["candidate_variants_held_out"]:
        lines.append(f"| {row['label']} | {_fmt(row['train_mean_sharpe'], 3)} | {_fmt(row['test_mean_sharpe'], 3)} | "
                     f"{_fmt(row['test_mean_return_pct'])} | {row['test_positions']} |")
    lines += ["", "## Source hashes", ""]
    for path, digest in sorted(result["source_sha256"].items()):
        lines.append(f"- `{path}` `{digest}`")
    lines.append("")
    return "\n".join(lines)


def main(argv=None) -> int:
    p = argparse.ArgumentParser(description="#1666 frozen held-out study driver (research-only)")
    p.add_argument("--manifest", default=MANIFEST)
    p.add_argument("--json", default=os.path.join(_HERE, "results.json"), dest="json_out")
    p.add_argument("--report", default=os.path.join(_HERE, "REPORT.md"))
    p.add_argument("--render-only", action="store_true",
                   help="Re-render the report from the committed results JSON")
    args = p.parse_args(argv)
    if args.render_only:
        with open(args.json_out) as fh:
            result = json.load(fh)
    else:
        try:
            result = run(args.manifest)
        except om.ManifestError as exc:
            print(f"manifest error: {exc}", file=sys.stderr)
            return 1
        with open(args.json_out, "w") as fh:
            json.dump(result, fh, indent=1, sort_keys=True, default=str)
            fh.write("\n")
    with open(args.report, "w") as fh:
        fh.write(render(result))
    print(f"verdict: {result['verdict']['outcome']} ({result['verdict']['reason']})")
    print(f"wrote {args.json_out} and {args.report}")
    return 0


if __name__ == "__main__":
    sys.exit(main())
